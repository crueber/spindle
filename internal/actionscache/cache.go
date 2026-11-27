// Package actionscache is the store-backed cache engine behind the built-in
// `actions/cache` steps (docs/threads.md §3.4c, PAR-095/PAR-096).
//
// ## Storage
//
//	repos/<o>/<r>/threads/caches/index.json                  the repo index (CAS)
//	repos/<o>/<r>/threads/caches/blobs/<sha256>              content-addressed payload
//
// One cache entry is one payload: the runner tars the requested paths in the
// job container and streams the archive here; the archive is stored under the
// sha256 the runner computed while streaming it, so identical content is stored
// exactly once and every restore is integrity-checked against that digest. The
// index is the commit point: a payload nobody points at is invisible and is
// reclaimed by the sweeper.
//
// ## Why one index and no LIST on the hot path
//
// Lookup and save are exact-key operations: one conditional GET of the index,
// then (for a save) one Create of the payload and one CAS of the index. The
// index is small — an entry costs a key, a digest and a size, never a file
// list — and its size is bounded by MaxEntries, so no prefix enumeration ever
// happens on a job's step path. The single LIST in this package is Reclaim's
// orphan sweep, which the maintainer runs.
//
// ## Scope, not policy
//
// The engine stores the scope STRING it is handed and never decides trust: the
// caller (internal/threads) computes the run's writable scope and its readable
// scopes from the run's event and trust class. Lookup takes the readable scopes
// in order, so a miss in one scope falls through to the next; Save takes exactly
// one writable scope, and a scope marked non-writable is refused here too
// (defence in depth: a caller bug cannot turn a read-only run into a writer).
//
// ## Eviction
//
// Eviction is oldest-saved-first, inside the CAS that commits a save: the index
// is a repo-wide size budget (MaxBytes) and an entry-count budget (MaxEntries).
// An entry is never evicted by the save that created it. Payload deletion
// happens after the index commit, so a concurrent lookup either sees the entry
// with its bytes present or does not see it at all.
//
// ### Concurrency
//
// Every mutation is a compare-and-swap on the single index document
// (store.PutUpdate with IfVersion); a loser re-reads and re-applies, which is
// the canonical retry loop in docs/go/13_concurrency.md §3. No lock is taken,
// so no store call is ever made under one. Two instances saving concurrently
// converge: each sees the other's entry and one of the two wins the CAS, then
// re-applies its own entry on the next attempt. A payload orphaned by a lost
// CAS is invisible (nothing points at it) and Reclaim deletes it.
package actionscache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// Package errors (the threads layer maps these onto its own error classes).
var (
	// ErrNotFound is a cache miss. It is never a step failure.
	ErrNotFound = errors.New("cache entry not found")
	// ErrInvalid is a malformed key, version, scope or digest.
	ErrInvalid = errors.New("invalid cache request")
	// ErrTooLarge is an entry over the per-entry or repo budget.
	ErrTooLarge = errors.New("cache entry is too large")
	// ErrForbidden is a write into a scope that is not writable by this run.
	ErrForbidden = errors.New("cache scope is not writable")
	// ErrCorrupt is a bucket anomaly (a payload that disagrees with its entry).
	ErrCorrupt = errors.New("cache entry is corrupt")
)

// Defaults and bounds (threads.cache_* — docs/threads.md §3.4c).
const (
	// DefaultMaxBytes is the per-repository cache budget when config says
	// nothing (5 GiB).
	DefaultMaxBytes int64 = 5 << 30
	// DefaultMaxEntryBytes is the one-entry ceiling (1 GiB).
	DefaultMaxEntryBytes int64 = 1 << 30
	// DefaultMaxEntries bounds the index's entry count (256).
	DefaultMaxEntries = 256
	// DefaultRetention is how long an unused entry lives (7 days, GitHub's
	// documented cache idle window).
	DefaultRetention = 7 * 24 * time.Hour

	// MaxKeyLen bounds a cache key (it is indexed and compared in full).
	MaxKeyLen = 512
	// MaxVersionLen bounds the `version` input (a digest-like token).
	MaxVersionLen = 128
	// MaxRestoreKeys bounds one restore.
	MaxRestoreKeys = 10

	// indexSchema is the index document's schema version.
	indexSchema = 1
	// casAttempts bounds the index retry loop.
	casAttempts = 8
	// idLen is the hex length of an entry id (20 bytes).
	idLen = 40
)

// ScopeKind is the sort of cache scope an entry lives in.
type ScopeKind int

// The scope kinds. The default branch is one repo-wide scope; every other
// branch has its own; every pull request has its own (trusted and untrusted
// halves), per D-10.
const (
	// KindDefault is the repository's default-branch scope.
	KindDefault ScopeKind = iota
	// KindBranch is one non-default branch's scope.
	KindBranch
	// KindPR is one pull request's scope.
	KindPR
)

// Scope is one cache scope. String() is the stored spelling, so the value that
// selects an entry is unambiguous in the bucket.
type Scope struct {
	Kind ScopeKind
	// Branch is the branch name for KindBranch.
	Branch string
	// PR is the pull request number for KindPR.
	PR int
	// Untrusted marks the fork half of a pull request's scope (D-10: a fork
	// run writes only its own untrusted scope, and no trusted scope ever reads
	// it).
	Untrusted bool
	// ReadOnly marks a scope this run may restore from but not write (a tag
	// run has no branch to write; the default-branch scope is read-only for
	// every other run).
	ReadOnly bool
}

// String is the stored scope spelling.
func (s Scope) String() string {
	switch s.Kind {
	case KindDefault:
		return "default"
	case KindBranch:
		return "branch:" + s.Branch
	case KindPR:
		if s.Untrusted {
			return fmt.Sprintf("pr:%d:untrusted", s.PR)
		}
		return fmt.Sprintf("pr:%d", s.PR)
	}
	return "unknown"
}

// Writable reports whether a run may save into this scope.
func (s Scope) Writable() bool { return !s.ReadOnly }

// Entry is one committed cache entry (a row of the index).
type Entry struct {
	// ID is content-addressed: sha256(scope \n key \n version \n payload)[:40].
	ID string `json:"id"`
	// Scope is the stored scope spelling.
	Scope string `json:"scope"`
	Key   string `json:"key"`
	// Version is the cache's version input (the caller's path/content
	// fingerprint). Restores match it exactly.
	Version string `json:"version"`
	// SHA256 addresses the payload and is the integrity check on restore.
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Files is the runner-reported file count (informational).
	Files int `json:"files"`
	// Run is the run that saved the entry (diagnostics and the audit trail).
	Run       string `json:"run,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Hit is one restore result.
type Hit struct {
	Entry Entry `json:"entry"`
	// Exact is true when the entry matched key+version exactly; false when a
	// restore-keys prefix selected it. This is `cache-hit`'s value.
	Exact bool `json:"exact"`
}

// SaveInput is one save.
type SaveInput struct {
	Key     string
	Version string
	SHA256  string
	Size    int64
	Files   int
	Run     string
}

// ReclaimResult is what one sweep did.
type ReclaimResult struct {
	EntriesDeleted int   `json:"entries_deleted"`
	PayloadsDelete int   `json:"payloads_deleted"`
	OrphansDeleted int   `json:"orphans_deleted"`
	BytesFreed     int64 `json:"bytes_freed"`
	DryRun         bool  `json:"dry_run,omitempty"`
}

// String renders the sweep's one-line report.
func (r ReclaimResult) String() string {
	mode := ""
	if r.DryRun {
		mode = " (dry run)"
	}
	return fmt.Sprintf("cache entries=%d payloads=%d orphans=%d freed=%dB%s",
		r.EntriesDeleted, r.PayloadsDelete, r.OrphansDeleted, r.BytesFreed, mode)
}

// blobRef counts the entries that point at one payload.
type blobRef struct {
	Size int64 `json:"size"`
	Refs int   `json:"refs"`
}

// Index is the repository's cache index (the CAS document).
type Index struct {
	Schema  int                `json:"schema"`
	Entries []Entry            `json:"entries"`
	Blobs   map[string]blobRef `json:"blobs"`
	Total   int64              `json:"total_bytes"`
}

// Engine is the cache engine over one object store.
type Engine struct {
	Store store.ObjectStore
	// Now is the clock (tests pin it; nil = time.Now).
	Now func() time.Time
	// MaxBytes is the per-repository budget (0 selects DefaultMaxBytes).
	MaxBytes int64
	// MaxEntryBytes is the one-entry ceiling (0 selects DefaultMaxEntryBytes).
	MaxEntryBytes int64
	// MaxEntries bounds the entry count (0 selects DefaultMaxEntries).
	MaxEntries int
	// Retention is the idle window (0 selects DefaultRetention).
	Retention time.Duration
}

// nowUTC is the engine clock.
func (e *Engine) nowUTC() time.Time {
	if e == nil || e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

func (e *Engine) maxBytes() int64 {
	if e.MaxBytes > 0 {
		return e.MaxBytes
	}
	return DefaultMaxBytes
}

func (e *Engine) maxEntryBytes() int64 {
	limit := e.MaxEntryBytes
	if limit <= 0 {
		limit = DefaultMaxEntryBytes
	}
	if budget := e.maxBytes(); limit > budget {
		limit = budget
	}
	return limit
}

func (e *Engine) maxEntries() int {
	if e.MaxEntries > 0 {
		return e.MaxEntries
	}
	return DefaultMaxEntries
}

func (e *Engine) retention() time.Duration {
	if e.Retention > 0 {
		return e.Retention
	}
	return DefaultRetention
}

// IndexKey returns repos/<o>/<r>/threads/caches/index.json.
func IndexKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/caches/index.json"
}

// BlobPrefix returns repos/<o>/<r>/threads/caches/blobs/.
func BlobPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/caches/blobs/"
}

// BlobKey returns the content-addressed payload key for one digest.
func BlobKey(owner, repo, sha string) string {
	return BlobPrefix(owner, repo) + sha
}

// NewEntryID derives the content-addressed entry id.
func NewEntryID(scope, key, version, sha string) string {
	sum := sha256.Sum256([]byte(scope + "\n" + key + "\n" + version + "\n" + sha))
	out := hex.EncodeToString(sum[:])
	return out[:idLen]
}

// ValidKey pins a cache key: non-empty, bounded, no control characters.
func ValidKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: a cache key is required", ErrInvalid)
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("%w: cache key is longer than %d bytes", ErrInvalid, MaxKeyLen)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: cache key contains a control character", ErrInvalid)
		}
	}
	return nil
}

// ValidVersion pins the version input ("" is allowed: the caller may use no
// version, which matches like any other literal).
func ValidVersion(version string) error {
	if len(version) > MaxVersionLen {
		return fmt.Errorf("%w: cache version is longer than %d bytes", ErrInvalid, MaxVersionLen)
	}
	for _, r := range version {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: cache version contains a control character", ErrInvalid)
		}
	}
	return nil
}

// ValidSHA pins a payload digest (lowercase hex sha256).
func ValidSHA(sha string) error {
	if len(sha) != 64 {
		return fmt.Errorf("%w: cache payload digest must be 64 hex characters", ErrInvalid)
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return fmt.Errorf("%w: cache payload digest must be lowercase hex", ErrInvalid)
		}
	}
	return nil
}

// loadIndex reads the index; an absent index is empty, never an error.
func (e *Engine) loadIndex(ctx context.Context, owner, repo string) (*Index, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, e.Store, IndexKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &Index{Schema: indexSchema, Blobs: map[string]blobRef{}}, "", nil
		}
		return nil, "", err
	}
	var idx Index
	if jerr := json.Unmarshal(raw, &idx); jerr != nil {
		return nil, "", fmt.Errorf("%w: cache index: %v", ErrCorrupt, jerr)
	}
	if idx.Blobs == nil {
		idx.Blobs = map[string]blobRef{}
	}
	return &idx, meta.Version, nil
}

// findEntry returns the newest entry matching the predicate.
func (idx *Index) findEntry(match func(Entry) bool) (Entry, bool) {
	best := Entry{}
	found := false
	for _, e := range idx.Entries {
		if !match(e) {
			continue
		}
		if !found || e.CreatedAt > best.CreatedAt || (e.CreatedAt == best.CreatedAt && e.ID > best.ID) {
			best, found = e, true
		}
	}
	return best, found
}

// Lookup resolves a restore: exact key+version in each readable scope in order,
// then restore-keys prefixes in the same order. The scope order is the caller's
// policy (own scope first, then the base/default scope).
func (e *Engine) Lookup(ctx context.Context, owner, repo string, scopes []Scope, key, version string, restoreKeys []string) (*Hit, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	if err := ValidVersion(version); err != nil {
		return nil, err
	}
	if len(restoreKeys) > MaxRestoreKeys {
		return nil, fmt.Errorf("%w: at most %d restore-keys are accepted", ErrInvalid, MaxRestoreKeys)
	}
	idx, _, err := e.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		names = append(names, sc.String())
	}
	for _, scope := range names {
		if entry, ok := idx.findEntry(func(e Entry) bool {
			return e.Scope == scope && e.Key == key && e.Version == version
		}); ok {
			return &Hit{Entry: entry, Exact: true}, nil
		}
	}
	for _, scope := range names {
		for _, prefix := range restoreKeys {
			if strings.TrimSpace(prefix) == "" {
				return nil, fmt.Errorf("%w: a restore-key must not be empty", ErrInvalid)
			}
			if entry, ok := idx.findEntry(func(e Entry) bool {
				return e.Scope == scope && strings.HasPrefix(e.Key, prefix)
			}); ok {
				return &Hit{Entry: entry, Exact: false}, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: no entry matches key %q", ErrNotFound, key)
}

// HasBlob reports whether a payload of exactly size bytes is already stored.
func (e *Engine) HasBlob(ctx context.Context, owner, repo, sha string, size int64) (bool, error) {
	if err := ValidSHA(sha); err != nil {
		return false, err
	}
	meta, err := e.Store.Head(ctx, BlobKey(owner, repo, sha))
	if err != nil {
		return false, err
	}
	return meta != nil && meta.Size == size, nil
}

// VisibleBlob resolves one payload by digest within the caller's readable
// scopes. A digest the run cannot see is a miss, so a job can never read an
// arbitrary payload by guessing its address (another PR's cache is not
// readable, even with the digest).
func (e *Engine) VisibleBlob(ctx context.Context, owner, repo string, scopes []Scope, sha string) (*Entry, error) {
	if err := ValidSHA(sha); err != nil {
		return nil, err
	}
	idx, _, err := e.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		names = append(names, sc.String())
	}
	if entry, ok := idx.findEntry(func(e Entry) bool {
		if e.SHA256 != sha {
			return false
		}
		for _, name := range names {
			if e.Scope == name {
				return true
			}
		}
		return false
	}); ok {
		return &entry, nil
	}
	return nil, fmt.Errorf("%w: payload %s is not visible to this run", ErrNotFound, sha)
}

// PutBlob receives one payload. The server recomputes the digest while
// receiving and refuses (and removes) bytes that do not match the claimed
// digest: a client can never poison a content address.
func (e *Engine) PutBlob(ctx context.Context, owner, repo, sha string, size int64, body io.Reader) error {
	if err := ValidSHA(sha); err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("%w: a cache payload needs a size", ErrInvalid)
	}
	if limit := e.maxEntryBytes(); size > limit {
		return fmt.Errorf("%w: the cache payload is %s, over the per-entry cap of %s (threads.cache_max_entry_bytes)",
			ErrTooLarge, humanBytes(size), humanBytes(limit))
	}
	key := BlobKey(owner, repo, sha)
	h := sha256.New()
	counted := &countingReader{r: io.LimitReader(body, size), h: h}
	_, err := e.Store.Put(ctx, key, store.PutBody{Stream: counted, StreamLen: size},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream", Immutable: true})
	if err != nil {
		if store.IsPreconditionFailed(err) {
			// Identical content is already stored (the address is the digest).
			meta, herr := e.Store.Head(ctx, key)
			if herr != nil {
				return herr
			}
			if meta == nil || meta.Size != size {
				return fmt.Errorf("%w: payload %s exists with a different size", ErrCorrupt, sha)
			}
			return nil
		}
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		_ = e.Store.Delete(ctx, key, "")
		return fmt.Errorf("%w: the cache payload hashed to %s, not the claimed %s", ErrInvalid, got, sha)
	}
	return nil
}

// countingReader hashes and counts what it passes through.
type countingReader struct {
	r io.Reader
	h io.Writer
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		_, _ = c.h.Write(p[:n])
	}
	return n, err
}

// OpenBlob streams one payload and its recorded size.
func (e *Engine) OpenBlob(ctx context.Context, owner, repo, sha string) (io.ReadCloser, int64, error) {
	if err := ValidSHA(sha); err != nil {
		return nil, 0, err
	}
	res, err := e.Store.Get(ctx, BlobKey(owner, repo, sha), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, 0, fmt.Errorf("%w: cache payload %s is missing from the bucket", ErrCorrupt, sha)
		}
		return nil, 0, err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return nil, 0, fmt.Errorf("%w: unexpected object result", ErrCorrupt)
	}
	return obj.Body, obj.Meta.Size, nil
}

// Save commits one entry. The payload must already be stored at in.SHA256 with
// in.Size bytes (PutBlob first), so a save is a small index write, never a
// second copy of the bytes.
func (e *Engine) Save(ctx context.Context, owner, repo string, scope Scope, in SaveInput) (*Entry, error) {
	if !scope.Writable() {
		return nil, fmt.Errorf("%w: scope %s is read-only for this run", ErrForbidden, scope.String())
	}
	if scope.Kind == KindBranch && strings.TrimSpace(scope.Branch) == "" {
		return nil, fmt.Errorf("%w: a branch scope needs a branch name", ErrInvalid)
	}
	if scope.Kind == KindPR && scope.PR <= 0 {
		return nil, fmt.Errorf("%w: a pull request scope needs a number", ErrInvalid)
	}
	if err := ValidKey(in.Key); err != nil {
		return nil, err
	}
	if err := ValidVersion(in.Version); err != nil {
		return nil, err
	}
	if err := ValidSHA(in.SHA256); err != nil {
		return nil, err
	}
	if in.Size < 0 {
		return nil, fmt.Errorf("%w: a cache entry needs a size", ErrInvalid)
	}
	if limit := e.maxEntryBytes(); in.Size > limit {
		return nil, fmt.Errorf("%w: the cache entry is %s, over the per-entry cap of %s (threads.cache_max_entry_bytes)",
			ErrTooLarge, humanBytes(in.Size), humanBytes(limit))
	}
	ok, err := e.HasBlob(ctx, owner, repo, in.SHA256, in.Size)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: the cache payload %s was never uploaded (put the bytes first)", ErrInvalid, in.SHA256)
	}
	now := e.nowUTC().Format(time.RFC3339)
	entry := Entry{
		ID: NewEntryID(scope.String(), in.Key, in.Version, in.SHA256), Scope: scope.String(),
		Key: in.Key, Version: in.Version, SHA256: in.SHA256, Size: in.Size, Files: in.Files,
		Run: in.Run, CreatedAt: now,
	}
	if err := e.commit(ctx, owner, repo, entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// commit is the CAS that adds entry and evicts to the budget.
func (e *Engine) commit(ctx context.Context, owner, repo string, entry Entry) error {
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < casAttempts; attempt++ {
		idx, ver, err := e.loadIndex(ctx, owner, repo)
		if err != nil {
			return err
		}
		// Replacing the same (scope,key,version) is GitHub's rule: a later save
		// of one key supersedes the earlier entry instead of growing the index.
		kept := make([]Entry, 0, len(idx.Entries)+1)
		replaced := map[string]bool{}
		for _, existing := range idx.Entries {
			if existing.Scope == entry.Scope && existing.Key == entry.Key && existing.Version == entry.Version {
				replaced[existing.SHA256] = true
				continue
			}
			kept = append(kept, existing)
		}
		kept = append(kept, entry)
		// Oldest-saved-first eviction to the count and byte budgets. The entry
		// being saved is never the one evicted.
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].CreatedAt != kept[j].CreatedAt {
				return kept[i].CreatedAt < kept[j].CreatedAt
			}
			return kept[i].ID < kept[j].ID
		})
		evicted := map[string]bool{}
		var total int64
		for _, ent := range kept {
			total += ent.Size
		}
		out := kept
		for len(out) > 0 && (len(out) > e.maxEntries() || total > e.maxBytes()) {
			drop := out[0]
			if drop.ID == entry.ID {
				// The new entry is the only candidate left; keep it (the size
				// was checked against both budgets before we got here).
				break
			}
			out = out[1:]
			total -= drop.Size
			evicted[drop.SHA256] = true
		}
		idx.Entries = out
		// Recompute the payload refcounts from the surviving entries. The index
		// is small by construction (MaxEntries), so this is a cheap in-memory
		// pass and it keeps refcounts exact after a replace or an eviction.
		refs := map[string]blobRef{}
		for _, ent := range out {
			ref := refs[ent.SHA256]
			ref.Size = ent.Size
			ref.Refs++
			refs[ent.SHA256] = ref
		}
		// A replaced entry's payload may still be referenced by another entry;
		// only the ones that fell to zero are deleted.
		for sha := range replaced {
			evicted[sha] = true
		}
		idx.Blobs = refs
		idx.Total = total
		idx.Schema = indexSchema
		raw, merr := json.Marshal(idx)
		if merr != nil {
			return merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, e.Store, key, raw, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return perr
		}
		e.deleteBlobs(ctx, owner, repo, evicted, refs)
		return nil
	}
	return fmt.Errorf("%w: cache index for %s/%s changed concurrently; retry", ErrInvalid, owner, repo)
}

// deleteBlobs removes payloads nothing points at any more (best effort: a
// failure leaves an orphan Reclaim deletes).
func (e *Engine) deleteBlobs(ctx context.Context, owner, repo string, candidates map[string]bool, refs map[string]blobRef) {
	for sha := range candidates {
		if refs[sha].Refs > 0 {
			continue
		}
		_ = e.Store.Delete(ctx, BlobKey(owner, repo, sha), "")
	}
}

// List returns the index's entries, oldest first (the admin/API read model).
func (e *Engine) List(ctx context.Context, owner, repo string) ([]Entry, error) {
	idx, _, err := e.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out := append([]Entry{}, idx.Entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete removes one entry by id and returns the bytes reclaimed (the payload
// is deleted when nothing else points at it).
func (e *Engine) Delete(ctx context.Context, owner, repo, id string) (int64, error) {
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < casAttempts; attempt++ {
		idx, ver, err := e.loadIndex(ctx, owner, repo)
		if err != nil {
			return 0, err
		}
		out := make([]Entry, 0, len(idx.Entries))
		found := false
		for _, ent := range idx.Entries {
			if ent.ID == id {
				found = true
				continue
			}
			out = append(out, ent)
		}
		if !found {
			return 0, fmt.Errorf("%w: unknown cache entry %q", ErrNotFound, id)
		}
		freed := e.recount(idx, out)
		raw, merr := json.Marshal(idx)
		if merr != nil {
			return 0, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, e.Store, key, raw, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return 0, perr
		}
		e.deleteBlobs(ctx, owner, repo, freed.shas, freed.refs)
		return freed.bytes, nil
	}
	return 0, fmt.Errorf("%w: cache index changed concurrently; retry", ErrInvalid)
}

// recount replaces idx.Entries with out and recomputes the refcounts and total.
func (e *Engine) recount(idx *Index, out []Entry) (freed recollection) {
	refs := map[string]blobRef{}
	var total int64
	for _, ent := range out {
		ref := refs[ent.SHA256]
		ref.Size = ent.Size
		ref.Refs++
		refs[ent.SHA256] = ref
		total += ent.Size
	}
	freed.shas = map[string]bool{}
	freed.bytes = idx.Total - total
	for sha := range idx.Blobs {
		if refs[sha].Refs == 0 {
			freed.shas[sha] = true
		}
	}
	idx.Entries = out
	idx.Blobs = refs
	idx.Total = total
	idx.Schema = indexSchema
	freed.refs = refs
	return freed
}

// recollection is the internal result of a recount.
type recollection struct {
	shas  map[string]bool
	refs  map[string]blobRef
	bytes int64
}

// DiscardScope removes every entry in one scope (the merge-time discard, D-10).
// It returns the bytes reclaimed.
func (e *Engine) DiscardScope(ctx context.Context, owner, repo string, scope Scope) (int64, error) {
	name := scope.String()
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < casAttempts; attempt++ {
		idx, ver, err := e.loadIndex(ctx, owner, repo)
		if err != nil {
			return 0, err
		}
		out := make([]Entry, 0, len(idx.Entries))
		removed := 0
		for _, ent := range idx.Entries {
			if ent.Scope == name {
				removed++
				continue
			}
			out = append(out, ent)
		}
		if removed == 0 {
			return 0, nil
		}
		freed := e.recount(idx, out)
		raw, merr := json.Marshal(idx)
		if merr != nil {
			return 0, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, e.Store, key, raw, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return 0, perr
		}
		e.deleteBlobs(ctx, owner, repo, freed.shas, freed.refs)
		return freed.bytes, nil
	}
	return 0, fmt.Errorf("%w: cache index changed concurrently; retry", ErrInvalid)
}

// Reclaim is the sweeper: it expires entries past the idle window, enforces the
// budgets (a config that shrank reclaims on the next pass), and deletes
// payloads nothing points at — including the orphans a lost index CAS left
// behind. One LIST, maintainer only. A dry run reports the same numbers and
// deletes nothing.
//
// Order matters: the orphan LIST runs BEFORE the index commit, so a listing
// failure leaves the index untouched and the work is simply retried next pass;
// the index commit is the point of no return, so the payload deletes that
// follow are best effort (an index row pointing at a missing payload is
// reported as ErrCorrupt on read, and the next pass's orphan/corruption sweep is
// the repair path).
func (e *Engine) Reclaim(ctx context.Context, owner, repo string, apply bool) (*ReclaimResult, error) {
	for attempt := 0; attempt < casAttempts; attempt++ {
		res, stale, err := e.reclaimOnce(ctx, owner, repo, apply)
		if err != nil {
			return res, err
		}
		if !stale {
			return res, nil
		}
	}
	return &ReclaimResult{DryRun: !apply}, fmt.Errorf("%w: cache index for %s/%s changed concurrently; retry", ErrInvalid, owner, repo)
}

// reclaimOnce is one sweep pass; stale reports a lost index CAS.
func (e *Engine) reclaimOnce(ctx context.Context, owner, repo string, apply bool) (res *ReclaimResult, stale bool, err error) {
	res = &ReclaimResult{DryRun: !apply}
	idx, ver, err := e.loadIndex(ctx, owner, repo)
	if err != nil {
		return res, false, err
	}
	now := e.nowUTC()
	kept := make([]Entry, 0, len(idx.Entries))
	for _, ent := range idx.Entries {
		at, perr := time.Parse(time.RFC3339, ent.CreatedAt)
		if perr != nil || !at.Add(e.retention()).After(now) {
			res.EntriesDeleted++
			continue
		}
		kept = append(kept, ent)
	}
	// Budget enforcement (oldest first), the same rule a save applies.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].CreatedAt != kept[j].CreatedAt {
			return kept[i].CreatedAt < kept[j].CreatedAt
		}
		return kept[i].ID < kept[j].ID
	})
	var total int64
	for _, ent := range kept {
		total += ent.Size
	}
	for len(kept) > 0 && (len(kept) > e.maxEntries() || total > e.maxBytes()) {
		total -= kept[0].Size
		kept = kept[1:]
		res.EntriesDeleted++
	}
	changed := res.EntriesDeleted > 0
	freed := recollection{shas: map[string]bool{}, refs: map[string]blobRef{}}
	for sha := range idx.Blobs {
		freed.refs[sha] = idx.Blobs[sha]
	}
	if changed {
		freed = e.recount(idx, kept)
		res.BytesFreed = freed.bytes
	}
	// Orphan sweep: a payload no surviving entry points at is invisible. This is
	// the one prefix LIST in the package and it runs on the maintainer only. The
	// payloads the entries above already account for are excluded, so the report
	// never counts one byte twice.
	var orphans []string
	var orphanBytes int64
	if lerr := e.Store.List(ctx, BlobPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		sha := m.Key[strings.LastIndex(m.Key, "/")+1:]
		if _, live := freed.refs[sha]; live || freed.shas[sha] {
			return nil
		}
		orphans = append(orphans, m.Key)
		orphanBytes += m.Size
		return nil
	}); lerr != nil {
		return res, false, lerr
	}
	res.OrphansDeleted = len(orphans)
	res.BytesFreed += orphanBytes
	res.PayloadsDelete = len(freed.shas)
	if !apply {
		return res, false, nil
	}
	if changed {
		raw, merr := json.Marshal(idx)
		if merr != nil {
			return res, false, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, e.Store, IndexKey(owner, repo), raw, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				return res, true, nil
			}
			return res, false, perr
		}
		e.deleteBlobs(ctx, owner, repo, freed.shas, freed.refs)
	}
	for _, key := range orphans {
		if ctx.Err() != nil {
			return res, false, ctx.Err()
		}
		if derr := e.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			return res, false, derr
		}
	}
	return res, false, nil
}

// humanBytes renders a size for an error message.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
