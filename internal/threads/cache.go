package threads

// The cache service (docs/threads.md §3.4c; PAR-095/PAR-096, decisions D-10):
// the server half of the built-in `actions/cache` steps. The engine
// (internal/actionscache) stores content-addressed payloads and one repo index;
// this file is the POLICY half — which scopes a run may read and which single
// scope it may write.
//
// ## Scopes (D-10)
//
//	pr:<n>              one pull request's cache, trusted PR runs
//	pr:<n>:untrusted    the same PR's fork half (never read by a trusted run)
//	branch:<name>       one non-default branch
//	default             the default branch, readable by every run
//
// A run's writable scope is exactly one, derived from its own header — never
// from a client parameter:
//
//   - a pull_request run writes its own PR scope (the untrusted half for a fork
//     run), so PR work can never write a base-readable entry;
//   - a trusted push/dispatch run on the default branch writes `default`;
//   - a trusted run on any other branch writes `branch:<name>`;
//   - a tag push (and any run with no branch) writes NOTHING: there is no
//     branch to scope it to, and a tag is not a place to publish bytes other
//     branches read.
//
// Readable scopes are ordered (own first, then the base/default branch):
//
//   - a PR run reads its own scope, then its BASE branch's scope (`default`
//     when the base IS the default branch, else `branch:<base>`);
//   - a branch run reads its own scope, then `default`;
//   - a tag run reads `default` only.
//
// Two invariants follow, and both are pinned by tests: a PR run never reads
// another PR's scope, and a trusted run never reads a fork's `:untrusted` half.
// Cache keys may collide across scopes; the payload is content-addressed and
// integrity-checked, so a collision can only ever select bytes the writer
// actually produced, and the writer is a run of that same scope.
//
// ## Discard at merge
//
// `DiscardPRCaches` removes a PR's two scopes; composition calls it when a pull
// request is merged (pulls' `closed` event with Merged=true). Anything a merge
// misses ages out through the cache retention window.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/actionscache"
	"github.com/crueber/spindle/internal/server/auth"
)

// Cache defaults (threads.cache_* — docs/threads.md §9).
const (
	// DefaultCacheRetention is how long an unused cache entry lives.
	DefaultCacheRetention = actionscache.DefaultRetention
	// MaxCacheKeyLen bounds a cache key (the engine's rule, re-exported for the
	// runner's pre-check and the docs).
	MaxCacheKeyLen = actionscache.MaxKeyLen
	// MaxCacheRestoreKeys bounds one restore.
	MaxCacheRestoreKeys = actionscache.MaxRestoreKeys
)

// CacheCaps is the runner-facing cache advertisement (the start answer carries
// it so the runner refuses an oversized entry before streaming it).
type CacheCaps struct {
	MaxBytesPerEntry int64 `json:"max_bytes_per_entry"`
	MaxBytesPerRepo  int64 `json:"max_bytes_per_repo"`
	MaxEntries       int   `json:"max_entries"`
	RetentionDays    int   `json:"retention_days"`
}

// CacheCaps resolves the effective cache limits.
func (s *Service) CacheCaps() CacheCaps {
	caps := CacheCaps{
		MaxBytesPerEntry: DefaultMaxCacheEntryBytes,
		MaxBytesPerRepo:  actionscache.DefaultMaxBytes,
		MaxEntries:       actionscache.DefaultMaxEntries,
		RetentionDays:    int(DefaultCacheRetention / (24 * time.Hour)),
	}
	if s.MaxCacheBytes > 0 {
		caps.MaxBytesPerRepo = s.MaxCacheBytes
	}
	if s.MaxCacheEntryBytes > 0 {
		caps.MaxBytesPerEntry = s.MaxCacheEntryBytes
	}
	if caps.MaxBytesPerEntry > caps.MaxBytesPerRepo {
		caps.MaxBytesPerEntry = caps.MaxBytesPerRepo
	}
	if s.CacheRetention > 0 {
		caps.RetentionDays = int(s.CacheRetention / (24 * time.Hour))
	}
	return caps
}

// DefaultMaxCacheEntryBytes is the one-entry ceiling when config says nothing.
const DefaultMaxCacheEntryBytes int64 = actionscache.DefaultMaxEntryBytes

// cacheEngine builds the engine over the service's store and settings.
func (s *Service) cacheEngine() *actionscache.Engine {
	eng := &actionscache.Engine{Store: s.Store, Now: s.Now}
	if s != nil {
		eng.MaxBytes = s.MaxCacheBytes
		eng.MaxEntryBytes = s.MaxCacheEntryBytes
		eng.Retention = s.CacheRetention
		eng.MaxEntries = s.MaxCacheEntries
	}
	return eng
}

// cacheErr maps an engine error onto the package's error classes (the HTTP
// status mapping is statusFor).
func cacheErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, actionscache.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, actionscache.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, actionscache.ErrTooLarge):
		return fmt.Errorf("%w: %v", ErrTooLarge, err)
	case errors.Is(err, actionscache.ErrForbidden):
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	case errors.Is(err, actionscache.ErrCorrupt):
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return err
}

// cacheDefaultBranch resolves the repository's default branch for the
// read-fallback scope. A reader without the capability (or a broken repository)
// yields "": the run then gets no cross-branch fallback, which is safe (it can
// still use its own scope).
func (s *Service) cacheDefaultBranch(ctx context.Context, owner, repo string) string {
	if s.Git == nil {
		return ""
	}
	br, ok := s.Git.(DefaultBranchReader)
	if !ok {
		return ""
	}
	name, err := br.DefaultBranch(ctx, repo)
	if err != nil {
		return ""
	}
	return shortRef(name)
}

// cacheScopeSet is one run's writable scope and ordered readable scopes.
type cacheScopeSet struct {
	Write *actionscache.Scope
	Read  []actionscache.Scope
}

// baseScopeOf is the branch a PR targets, or the branch a run is on: the
// default-branch scope when it IS the default, a read-only branch scope
// otherwise.
func baseScopeOf(branch, defaultBranch string) *actionscache.Scope {
	branch = shortRef(branch)
	if branch == "" {
		return nil
	}
	if defaultBranch != "" && branch == defaultBranch {
		return &actionscache.Scope{Kind: actionscache.KindDefault, ReadOnly: true}
	}
	return &actionscache.Scope{Kind: actionscache.KindBranch, Branch: branch, ReadOnly: true}
}

// prNumberOf returns the pull request number a run was created for (0 when the
// run is not a pull_request run, or the payload is malformed — the cache then
// fails closed rather than guessing a scope).
func prNumberOf(run *Run) int {
	if run == nil || run.Event != EventPullReq || run.EventPayload == nil {
		return 0
	}
	pr, ok := run.EventPayload["pull_request"].(map[string]any)
	if !ok {
		return 0
	}
	switch n := pr["number"].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// cacheScopesForRun is the one place a run's cache scopes are decided.
func (s *Service) cacheScopesForRun(ctx context.Context, owner, repo string, run *Run) (cacheScopeSet, error) {
	var out cacheScopeSet
	if run == nil {
		return out, fmt.Errorf("%w: no run", ErrInvalid)
	}
	def := s.cacheDefaultBranch(ctx, owner, repo)
	if run.Event == EventPullReq {
		n := prNumberOf(run)
		if n <= 0 {
			return out, fmt.Errorf("%w: a pull_request run without a pull request number cannot use the cache", ErrInvalid)
		}
		own := actionscache.Scope{Kind: actionscache.KindPR, PR: n, Untrusted: !run.Trusted}
		out.Write = &own
		out.Read = append(out.Read, own)
		if base := baseScopeOf(run.Branch, def); base != nil {
			out.Read = append(out.Read, *base)
		}
		return out, nil
	}
	// A tag push has no branch: it may restore the default-branch cache but it
	// never writes (there is no branch to scope a write to).
	if strings.TrimSpace(run.Branch) == "" || !run.Trusted {
		if base := baseScopeOf(run.Branch, def); base != nil {
			out.Read = append(out.Read, *base)
		} else {
			out.Read = append(out.Read, actionscache.Scope{Kind: actionscache.KindDefault, ReadOnly: true})
		}
		return out, nil
	}
	if def != "" && run.Branch == def {
		own := actionscache.Scope{Kind: actionscache.KindDefault}
		out.Write = &own
		out.Read = append(out.Read, own)
		return out, nil
	}
	own := actionscache.Scope{Kind: actionscache.KindBranch, Branch: run.Branch}
	out.Write = &own
	out.Read = append(out.Read, own)
	if base := baseScopeOf(def, def); base != nil {
		out.Read = append(out.Read, *base)
	} else {
		out.Read = append(out.Read, actionscache.Scope{Kind: actionscache.KindDefault, ReadOnly: true})
	}
	return out, nil
}

// CacheLookupInput is one restore request from a job.
type CacheLookupInput struct {
	Key         string   `json:"key"`
	Version     string   `json:"version"`
	RestoreKeys []string `json:"restore_keys,omitempty"`
}

// CacheView is one restore's answer. Hit=false is a MISS, never an error: a
// cache-less job is a normal job.
type CacheView struct {
	Hit       bool   `json:"hit"`
	Exact     bool   `json:"exact"`
	Key       string `json:"key,omitempty"`
	Version   string `json:"version,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size"`
	Files     int    `json:"files,omitempty"`
	Scope     string `json:"scope,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// Reason names why a lookup produced no entry (a miss vs a refusal).
	Reason string `json:"reason,omitempty"`
}

// cacheViewOf projects an engine hit.
func cacheViewOf(hit *actionscache.Hit) *CacheView {
	if hit == nil {
		return &CacheView{Hit: false}
	}
	return &CacheView{
		Hit: true, Exact: hit.Exact, Key: hit.Entry.Key, Version: hit.Entry.Version,
		SHA256: hit.Entry.SHA256, Size: hit.Entry.Size, Files: hit.Entry.Files,
		Scope: hit.Entry.Scope, CreatedAt: hit.Entry.CreatedAt,
	}
}

// attemptRun loads the run an attempt token belongs to.
func (s *Service) attemptRun(ctx context.Context, claims AttemptClaims) (*Run, string, string, error) {
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, "", "", err
	}
	run, err := s.loadRun(ctx, owner, repo, claims.Run)
	if err != nil {
		return nil, "", "", err
	}
	if run == nil {
		return nil, "", "", fmt.Errorf("%w: unknown run %q", ErrNotFound, claims.Run)
	}
	return run, owner, repo, nil
}

// CacheLookupForJob answers one restore. The scope comes from the run, never
// from the request.
func (s *Service) CacheLookupForJob(ctx context.Context, claims AttemptClaims, in CacheLookupInput) (*CacheView, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeCache); err != nil {
		return nil, err
	}
	run, owner, repo, err := s.attemptRun(ctx, claims)
	if err != nil {
		return nil, err
	}
	scopes, err := s.cacheScopesForRun(ctx, owner, repo, run)
	if err != nil {
		return nil, err
	}
	hit, lerr := s.cacheEngine().Lookup(ctx, owner, repo, scopes.Read, in.Key, in.Version, in.RestoreKeys)
	if lerr != nil {
		if errors.Is(lerr, actionscache.ErrNotFound) {
			return &CacheView{Hit: false, Reason: "no cache entry matches the key or restore-keys in this run's scopes"}, nil
		}
		return nil, cacheErr(lerr)
	}
	return cacheViewOf(hit), nil
}

// CacheBlobForJob streams one payload back to the job container. The payload
// must belong to an entry the run can see, so a guessed digest reads nothing.
func (s *Service) CacheBlobForJob(ctx context.Context, claims AttemptClaims, sha string) (io.ReadCloser, int64, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeCache); err != nil {
		return nil, 0, err
	}
	run, owner, repo, err := s.attemptRun(ctx, claims)
	if err != nil {
		return nil, 0, err
	}
	scopes, err := s.cacheScopesForRun(ctx, owner, repo, run)
	if err != nil {
		return nil, 0, err
	}
	eng := s.cacheEngine()
	entry, verr := eng.VisibleBlob(ctx, owner, repo, scopes.Read, sha)
	if verr != nil {
		return nil, 0, cacheErr(verr)
	}
	body, size, oerr := eng.OpenBlob(ctx, owner, repo, sha)
	if oerr != nil {
		return nil, 0, cacheErr(oerr)
	}
	if size != entry.Size {
		_ = body.Close()
		return nil, 0, fmt.Errorf("%w: cache payload %s is %s, the index says %s",
			ErrCorrupt, sha, humanBytes(size), humanBytes(entry.Size))
	}
	return body, size, nil
}

// PutCacheBlobForJob receives one payload. Only a run with a writable scope may
// upload, and the bytes must hash to the address the request names.
func (s *Service) PutCacheBlobForJob(ctx context.Context, claims AttemptClaims, sha string, size int64, body io.Reader) error {
	if _, err := s.verifyAttempt(ctx, claims, ScopeCache); err != nil {
		return err
	}
	run, owner, repo, err := s.attemptRun(ctx, claims)
	if err != nil {
		return err
	}
	scopes, err := s.cacheScopesForRun(ctx, owner, repo, run)
	if err != nil {
		return err
	}
	if scopes.Write == nil {
		return fmt.Errorf("%w: this run has no writable cache scope (a tag push cannot save a cache)", ErrForbidden)
	}
	return cacheErr(s.cacheEngine().PutBlob(ctx, owner, repo, sha, size, body))
}

// SaveCacheInput is one commit from a job. Key/version/sha/size are the
// client's; the SCOPE is the run's.
type SaveCacheInput struct {
	Key     string `json:"key"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Files   int    `json:"files"`
}

// SaveCacheForJob commits one entry into the run's writable scope and returns
// the committed entry (the log line and the page read it).
func (s *Service) SaveCacheForJob(ctx context.Context, claims AttemptClaims, in SaveCacheInput) (*CacheView, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeCache); err != nil {
		return nil, err
	}
	run, owner, repo, err := s.attemptRun(ctx, claims)
	if err != nil {
		return nil, err
	}
	scopes, err := s.cacheScopesForRun(ctx, owner, repo, run)
	if err != nil {
		return nil, err
	}
	if scopes.Write == nil {
		return nil, fmt.Errorf("%w: this run has no writable cache scope (a tag push cannot save a cache)", ErrForbidden)
	}
	entry, serr := s.cacheEngine().Save(ctx, owner, repo, *scopes.Write, actionscache.SaveInput{
		Key: in.Key, Version: in.Version, SHA256: in.SHA256, Size: in.Size, Files: in.Files, Run: claims.Run,
	})
	if serr != nil {
		return nil, cacheErr(serr)
	}
	return cacheViewOf(&actionscache.Hit{Entry: *entry, Exact: true}), nil
}

// ListCaches is the repository read model (cache page/API; read gate).
func (s *Service) ListCaches(ctx context.Context, repo string, p auth.Principal) ([]actionscache.Entry, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	entries, lerr := s.cacheEngine().List(ctx, owner, name)
	if lerr != nil {
		return nil, cacheErr(lerr)
	}
	if entries == nil {
		entries = []actionscache.Entry{}
	}
	return entries, nil
}

// DeleteCache removes one cache entry (maintain: a cache is shared work, and
// deleting one can slow every branch that restores it).
func (s *Service) DeleteCache(ctx context.Context, repo, id string, p auth.Principal) (int64, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
		return 0, err
	}
	freed, derr := s.cacheEngine().Delete(ctx, owner, name, strings.TrimSpace(id))
	if derr != nil {
		return 0, cacheErr(derr)
	}
	s.auditAdmin(ctx, p.Name, "threads.cache.delete", "cache:"+id, "repo="+repo)
	return freed, nil
}

// DiscardPRCaches drops both halves of one pull request's cache scope (D-10's
// "discard at merge"). Composition calls it from the pulls `closed` event when
// the PR merged; a hand call is idempotent.
func (s *Service) DiscardPRCaches(ctx context.Context, repo string, num int) (int64, error) {
	if num <= 0 {
		return 0, fmt.Errorf("%w: a pull request number is required", ErrInvalid)
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	eng := s.cacheEngine()
	var freed int64
	for _, scope := range []actionscache.Scope{
		{Kind: actionscache.KindPR, PR: num},
		{Kind: actionscache.KindPR, PR: num, Untrusted: true},
	} {
		n, derr := eng.DiscardScope(ctx, owner, name, scope)
		if derr != nil {
			return freed, cacheErr(derr)
		}
		freed += n
	}
	return freed, nil
}

// reclaimCaches is the threads-gc cache family: the engine's sweep, reported in
// the same pass's numbers.
func (s *Service) reclaimCaches(ctx context.Context, owner, repo string, apply bool, res *GCResult) error {
	sweep, err := s.cacheEngine().Reclaim(ctx, owner, repo, apply)
	if err != nil {
		return cacheErr(err)
	}
	res.CachesDeleted += sweep.EntriesDeleted
	res.CacheBytes += sweep.BytesFreed
	res.BytesFreed += sweep.BytesFreed
	return nil
}
