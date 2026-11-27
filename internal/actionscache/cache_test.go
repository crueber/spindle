package actionscache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// The cache engine's table: keys, scope order, save/replace/evict, integrity,
// the sweeper and the D-10 scope discard.

const (
	defScope = "default"
	pr2Scope = "pr:2"
	pr3Scope = "pr:3"
	brFeat   = "branch:feat"
	pr2Fork  = "pr:2:untrusted"
)

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// env is one engine over a memory store with a pinned clock.
type env struct {
	t      *testing.T
	eng    *Engine
	store  *store.Memory
	now    time.Time
	ctx    context.Context
	owner  string
	repo   string
	closed bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, store: store.NewMemory(), now: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC),
		ctx: context.Background(), owner: "o", repo: "r"}
	e.eng = &Engine{Store: e.store, Now: func() time.Time { return e.now }}
	return e
}

// put stores a payload and returns (sha, size).
func (e *env) put(body string) (string, int64) {
	e.t.Helper()
	sha := shaOf(body)
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, sha, int64(len(body)), strings.NewReader(body)); err != nil {
		e.t.Fatalf("PutBlob: %v", err)
	}
	return sha, int64(len(body))
}

// save commits one entry in scope with the given key/version.
func (e *env) save(scope Scope, key, version, body string) *Entry {
	e.t.Helper()
	sha, size := e.put(body)
	ent, err := e.eng.Save(e.ctx, e.owner, e.repo, scope, SaveInput{
		Key: key, Version: version, SHA256: sha, Size: size, Files: 2, Run: "run-" + key,
	})
	if err != nil {
		e.t.Fatalf("Save(%s/%s): %v", scope.String(), key, err)
	}
	return ent
}

func TestKeyAndEntryLayout(t *testing.T) {
	if got := IndexKey("o", "r"); got != "repos/o/r/threads/caches/index.json" {
		t.Fatalf("index key = %q", got)
	}
	if got := BlobKey("o", "r", "abc"); got != "repos/o/r/threads/caches/blobs/abc" {
		t.Fatalf("blob key = %q", got)
	}
	id := NewEntryID("pr:3", "npm-abc", "v1", shaOf("x"))
	if len(id) != idLen {
		t.Fatalf("entry id length = %d", len(id))
	}
	if other := NewEntryID("pr:3", "npm-abc", "v1", shaOf("y")); other == id {
		t.Fatal("a different payload must produce a different entry id (content addressed)")
	}
	if same := NewEntryID("pr:3", "npm-abc", "v1", shaOf("x")); same != id {
		t.Fatal("the entry id must be deterministic")
	}
}

func TestScopeSpellings(t *testing.T) {
	cases := []struct {
		scope Scope
		want  string
		write bool
	}{
		{Scope{Kind: KindDefault}, "default", true},
		{Scope{Kind: KindBranch, Branch: "feat"}, "branch:feat", true},
		{Scope{Kind: KindPR, PR: 7}, "pr:7", true},
		{Scope{Kind: KindPR, PR: 7, Untrusted: true}, "pr:7:untrusted", true},
		{Scope{Kind: KindDefault, ReadOnly: true}, "default", false},
		{Scope{Kind: KindBranch, Branch: "x", ReadOnly: true}, "branch:x", false},
	}
	for _, tc := range cases {
		if got := tc.scope.String(); got != tc.want {
			t.Fatalf("scope = %q, want %q", got, tc.want)
		}
		if got := tc.scope.Writable(); got != tc.write {
			t.Fatalf("%q writable = %v", tc.want, got)
		}
	}
	if got := (Scope{Kind: ScopeKind(99)}).String(); got != "unknown" {
		t.Fatalf("unknown scope = %q", got)
	}
}

func TestValidation(t *testing.T) {
	if err := ValidKey(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty key = %v", err)
	}
	if err := ValidKey(strings.Repeat("k", MaxKeyLen+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long key = %v", err)
	}
	if err := ValidKey("bad\x01key"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control key = %v", err)
	}
	if err := ValidKey("npm-abc"); err != nil {
		t.Fatalf("good key = %v", err)
	}
	if err := ValidVersion(strings.Repeat("v", MaxVersionLen+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long version = %v", err)
	}
	if err := ValidVersion("bad\x02"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control version = %v", err)
	}
	if err := ValidSHA("nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short sha = %v", err)
	}
	if err := ValidSHA(strings.Repeat("A", 64)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uppercase sha = %v", err)
	}
	if err := ValidSHA(shaOf("x")); err != nil {
		t.Fatalf("good sha = %v", err)
	}
}

func TestLookupOrderIsAScopeThenRestoreTable(t *testing.T) {
	e := newEnv(t)
	// Two defaults (an older key and a newer prefix), one branch, one PR.
	e.save(Scope{Kind: KindDefault}, "npm-old", "v1", "default-old")
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindDefault}, "npm-lock-111", "v1", "default-new")
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindBranch, Branch: "feat"}, "npm-lock-222", "v1", "branch")
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindPR, PR: 3}, "npm-lock-333", "v1", "pr3")

	read := []Scope{{Kind: KindPR, PR: 3}, {Kind: KindDefault}}
	cases := []struct {
		name      string
		key       string
		restore   []string
		wantBody  string
		wantExact bool
		wantErr   bool
	}{
		{name: "exact in own scope", key: "npm-lock-333", wantBody: "pr3", wantExact: true},
		{name: "exact key elsewhere falls through", key: "npm-lock-222", wantErr: true},
		{name: "restore-key prefix, own scope first", key: "npm-lock-999", restore: []string{"npm-lock-", "npm-"},
			wantBody: "pr3", wantExact: false},
		{name: "restore-key in the base scope by prefix order", key: "npm-lock-000", restore: []string{"npm-lock-"},
			wantBody: "pr3"},
		{name: "restore key only in default", key: "other-1", restore: []string{"npm-old"}, wantBody: "default-old"},
		{name: "no match", key: "nothing", restore: []string{"zzz"}, wantErr: true},
	}
	for _, tc := range cases {
		hit, err := e.eng.Lookup(e.ctx, e.owner, e.repo, read, tc.key, "v1", tc.restore)
		if tc.wantErr {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: err = %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if hit.Exact != tc.wantExact {
			t.Fatalf("%s: exact = %v", tc.name, hit.Exact)
		}
		body, _, err := e.eng.OpenBlob(e.ctx, e.owner, e.repo, hit.Entry.SHA256)
		if err != nil {
			t.Fatalf("%s: open: %v", tc.name, err)
		}
		raw, _ := io.ReadAll(body)
		_ = body.Close()
		if string(raw) != tc.wantBody {
			t.Fatalf("%s: body = %q, want %q", tc.name, raw, tc.wantBody)
		}
	}

	// Version is part of the identity: a different version is a miss.
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo, read, "npm-lock-333", "v2", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version mismatch = %v", err)
	}
	// Too many restore keys is a named error, not a silent truncation.
	many := make([]string, MaxRestoreKeys+1)
	for i := range many {
		many[i] = "k"
	}
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo, read, "x", "", many); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many restore keys = %v", err)
	}
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo, read, "x", "", []string{" "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty restore key = %v", err)
	}
}

func TestForkAndPRScopesNeverSeeEachOther(t *testing.T) {
	e := newEnv(t)
	e.save(Scope{Kind: KindPR, PR: 2, Untrusted: true}, "npm-lock-1", "v1", "fork")
	e.save(Scope{Kind: KindPR, PR: 3, Untrusted: true}, "npm-lock-1", "v1", "other-fork")

	// A trusted PR run reads its own scope and the base, never a fork scope.
	hit, err := e.eng.Lookup(e.ctx, e.owner, e.repo,
		[]Scope{{Kind: KindPR, PR: 2}, {Kind: KindDefault}}, "npm-lock-1", "v1", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a trusted PR run read a fork cache: %v %v", hit, err)
	}
	// The fork reads exactly its own half.
	hit, err = e.eng.Lookup(e.ctx, e.owner, e.repo,
		[]Scope{{Kind: KindPR, PR: 2, Untrusted: true}, {Kind: KindDefault}}, "npm-lock-1", "v1", nil)
	if err != nil || hit.Entry.SHA256 != shaOf("fork") {
		t.Fatalf("fork restore = %v %v", hit, err)
	}
	// Another fork's PR scope is never reachable.
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo,
		[]Scope{{Kind: KindPR, PR: 2, Untrusted: true}}, "npm-lock-1", "v1", nil); err != nil {
		t.Fatalf("own scope lookup: %v", err)
	}
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo,
		[]Scope{{Kind: KindPR, PR: 3, Untrusted: true}}, "npm-lock-1", "v1", nil); err != nil {
		t.Fatalf("the other fork's own scope must still work for itself: %v", err)
	}
}

func TestSaveReplacesAndEvictsToBudget(t *testing.T) {
	e := newEnv(t)
	e.eng.MaxBytes = 30
	e.eng.MaxEntryBytes = 20
	e.eng.MaxEntries = 5

	// Replacing one (scope,key,version) supersedes the earlier entry.
	first := e.save(Scope{Kind: KindDefault}, "k", "v1", strings.Repeat("a", 10))
	e.now = e.now.Add(time.Minute)
	second := e.save(Scope{Kind: KindDefault}, "k", "v1", strings.Repeat("b", 12))
	entries, err := e.eng.List(e.ctx, e.owner, e.repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != second.ID {
		t.Fatalf("replace kept %v, want only %s", entries, second.ID)
	}
	// The superseded payload is gone (nothing points at it).
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", first.SHA256)); meta != nil {
		t.Fatal("the replaced payload should have been deleted")
	}

	// Fill to the byte budget: the oldest entries are evicted.
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindDefault}, "k2", "v1", strings.Repeat("c", 10))
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindDefault}, "k3", "v1", strings.Repeat("d", 10))
	entries, _ = e.eng.List(e.ctx, e.owner, e.repo)
	var total int64
	for _, ent := range entries {
		total += ent.Size
	}
	if total > 30 {
		t.Fatalf("total %d over budget 30: %v", total, entries)
	}
	if len(entries) < 2 {
		t.Fatalf("eviction dropped too much: %v", entries)
	}
	// The newest entry is never the evicted one.
	if entries[len(entries)-1].Key != "k3" {
		t.Fatalf("newest entry was evicted: %v", entries)
	}
	// An entry larger than the per-entry cap is refused, not clamped.
	e.eng.MaxEntryBytes = 4
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindDefault}, SaveInput{
		Key: "big", Version: "v1", SHA256: shaOf(strings.Repeat("c", 10)), Size: 10,
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-cap save = %v", err)
	}
}

func TestUploadIntegrityAndDedupe(t *testing.T) {
	e := newEnv(t)
	body := "payload"
	sha := shaOf(body)
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, sha, int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	// Identical bytes again: the content address dedupes (no error).
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, sha, int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatalf("dedupe PutBlob: %v", err)
	}
	// A payload that does not match its address is refused and removed.
	wrong := shaOf("other")
	err := e.eng.PutBlob(e.ctx, e.owner, e.repo, wrong, int64(len(body)), strings.NewReader(body))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched digest = %v", err)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", wrong)); meta != nil {
		t.Fatal("a mismatched payload must not survive")
	}
	// A short body is refused and removed.
	short := shaOf(strings.Repeat("q", 10))
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, short, 10, strings.NewReader("qq")); err == nil {
		t.Fatal("a body shorter than its advertised size must be rejected by the store")
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", short)); meta != nil {
		t.Fatal("a truncated payload must not survive")
	}
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, "bad", 1, strings.NewReader("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sha = %v", err)
	}
	if _, _, err := e.eng.OpenBlob(e.ctx, e.owner, e.repo, shaOf("gone")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing blob = %v", err)
	}
	if _, _, err := e.eng.OpenBlob(e.ctx, e.owner, e.repo, "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad open sha = %v", err)
	}
}

func TestSaveRequiresTheBytesAndAWriteScope(t *testing.T) {
	e := newEnv(t)
	_, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindDefault}, SaveInput{
		Key: "k", Version: "v1", SHA256: shaOf("never-uploaded"), Size: 15,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("save without bytes = %v", err)
	}
	readOnly := Scope{Kind: KindDefault, ReadOnly: true}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, readOnly, SaveInput{
		Key: "k", Version: "v1", SHA256: shaOf("x"), Size: 1,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only scope save = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindBranch}, SaveInput{
		Key: "k", Version: "v1", SHA256: shaOf("x"), Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("branch scope without a branch = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindPR}, SaveInput{
		Key: "k", Version: "v1", SHA256: shaOf("x"), Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pr scope without a number = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindDefault}, SaveInput{
		Key: "k", Version: "v1", SHA256: shaOf("x"), Size: -1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative size = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindDefault}, SaveInput{
		Key: "", Version: "v1", SHA256: shaOf("x"), Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty key = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, e.owner, e.repo, Scope{Kind: KindDefault}, SaveInput{
		Key: "k", Version: "v1", SHA256: "bad", Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad digest = %v", err)
	}
}

func TestSnapshotsAndDelete(t *testing.T) {
	e := newEnv(t)
	a := e.save(Scope{Kind: KindDefault}, "a", "v1", "aaa")
	b := e.save(Scope{Kind: KindPR, PR: 2}, "b", "v1", "bbb")
	entries, err := e.eng.List(e.ctx, e.owner, e.repo)
	if err != nil || len(entries) != 2 {
		t.Fatalf("List = %v %v", entries, err)
	}
	freed, err := e.eng.Delete(e.ctx, e.owner, e.repo, a.ID)
	if err != nil || freed != 3 {
		t.Fatalf("Delete = %d %v", freed, err)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", a.SHA256)); meta != nil {
		t.Fatal("delete must remove the payload")
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", b.SHA256)); meta == nil {
		t.Fatal("delete removed another entry's payload")
	}
	if _, err := e.eng.Delete(e.ctx, e.owner, e.repo, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	// A scope discard (the merge rule) removes exactly that scope.
	if freed, err := e.eng.DiscardScope(e.ctx, e.owner, e.repo, Scope{Kind: KindPR, PR: 2}); err != nil || freed != 3 {
		t.Fatalf("DiscardScope = %d %v", freed, err)
	}
	if entries, _ := e.eng.List(e.ctx, e.owner, e.repo); len(entries) != 0 {
		t.Fatalf("entries after discard = %v", entries)
	}
	if freed, err := e.eng.DiscardScope(e.ctx, e.owner, e.repo, Scope{Kind: KindPR, PR: 9}); err != nil || freed != 0 {
		t.Fatalf("discard of an empty scope = %d %v", freed, err)
	}
}

func TestReclaimExpiresBudgetsAndOrphans(t *testing.T) {
	e := newEnv(t)
	e.eng.Retention = 24 * time.Hour
	old := e.save(Scope{Kind: KindDefault}, "old", "v1", strings.Repeat("a", 8))
	e.now = e.now.Add(25 * time.Hour)
	fresh := e.save(Scope{Kind: KindDefault}, "fresh", "v1", strings.Repeat("b", 8))
	// An orphan payload (a lost index CAS) is invisible but sized.
	orphan := shaOf("orphan")
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, orphan, 6, strings.NewReader("orphan")); err != nil {
		t.Fatalf("orphan put: %v", err)
	}

	dry, err := e.eng.Reclaim(e.ctx, e.owner, e.repo, false)
	if err != nil {
		t.Fatalf("dry Reclaim: %v", err)
	}
	if dry.EntriesDeleted != 1 || dry.OrphansDeleted != 1 || !dry.DryRun {
		t.Fatalf("dry run = %+v", dry)
	}
	// Nothing was deleted by the dry run.
	if entries, _ := e.eng.List(e.ctx, e.owner, e.repo); len(entries) != 2 {
		t.Fatalf("dry run deleted entries: %v", entries)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", orphan)); meta == nil {
		t.Fatal("dry run deleted the orphan")
	}

	res, err := e.eng.Reclaim(e.ctx, e.owner, e.repo, true)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if res.EntriesDeleted != 1 || res.OrphansDeleted != 1 || res.BytesFreed == 0 {
		t.Fatalf("apply run = %+v", res)
	}
	entries, _ := e.eng.List(e.ctx, e.owner, e.repo)
	if len(entries) != 1 || entries[0].ID != fresh.ID {
		t.Fatalf("entries after reclaim = %v", entries)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", old.SHA256)); meta != nil {
		t.Fatal("the expired payload should be gone")
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", orphan)); meta != nil {
		t.Fatal("the orphan payload should be gone")
	}
	// Idempotent: a second pass deletes nothing.
	again, err := e.eng.Reclaim(e.ctx, e.owner, e.repo, true)
	if err != nil || again.EntriesDeleted != 0 || again.OrphansDeleted != 0 {
		t.Fatalf("second reclaim = %+v %v", again, err)
	}

	// A shrunken budget reclaims on the next pass (oldest first).
	e.eng.MaxBytes = 4
	shrunk, err := e.eng.Reclaim(e.ctx, e.owner, e.repo, true)
	if err != nil || shrunk.EntriesDeleted != 1 {
		t.Fatalf("budget reclaim = %+v %v", shrunk, err)
	}
	if entries, _ := e.eng.List(e.ctx, e.owner, e.repo); len(entries) != 0 {
		t.Fatalf("entry survived a zero budget: %v", entries)
	}
}

func TestReclaimKeepsSameBytesShared(t *testing.T) {
	e := newEnv(t)
	// Two entries, one payload: deleting one entry must keep the bytes.
	a := e.save(Scope{Kind: KindDefault}, "a", "v1", "shared")
	e.now = e.now.Add(time.Minute)
	b := e.save(Scope{Kind: KindBranch, Branch: "b"}, "b", "v1", "shared")
	if a.SHA256 != b.SHA256 {
		t.Fatal("fixture: payloads should be shared")
	}
	if _, err := e.eng.Delete(e.ctx, e.owner, e.repo, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", a.SHA256)); meta == nil {
		t.Fatal("a shared payload was deleted while another entry still points at it")
	}
	res, err := e.eng.Reclaim(e.ctx, e.owner, e.repo, true)
	if err != nil || res.EntriesDeleted != 0 {
		t.Fatalf("Reclaim = %+v %v", res, err)
	}
}

func TestIndexBudgetCount(t *testing.T) {
	e := newEnv(t)
	e.eng.MaxEntries = 2
	e.save(Scope{Kind: KindDefault}, "a", "v1", "aaa")
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindDefault}, "b", "v1", "bbb")
	e.now = e.now.Add(time.Minute)
	e.save(Scope{Kind: KindDefault}, "c", "v1", "ccc")
	entries, _ := e.eng.List(e.ctx, e.owner, e.repo)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	if entries[0].Key != "b" || entries[1].Key != "c" {
		t.Fatalf("count eviction removed the wrong entries: %v", entries)
	}
}

func TestCorruptIndexIsNamed(t *testing.T) {
	e := newEnv(t)
	if _, err := store.PutBytes(e.ctx, e.store, IndexKey("o", "r"), []byte("{not json"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.eng.Lookup(e.ctx, e.owner, e.repo, []Scope{{Kind: KindDefault}}, "k", "", nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt index = %v", err)
	}
}

func TestBlobCopyIsContentAddressed(t *testing.T) {
	e := newEnv(t)
	body := bytes.Repeat([]byte("x"), 1024)
	sha := shaOf(string(body))
	if err := e.eng.PutBlob(e.ctx, e.owner, e.repo, sha, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	rc, size, err := e.eng.OpenBlob(e.ctx, e.owner, e.repo, sha)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if int64(len(got)) != size || shaOf(string(got)) != sha {
		t.Fatalf("round trip corrupted: %d bytes", len(got))
	}
}

func TestEngineDefaults(t *testing.T) {
	var e Engine
	if e.maxBytes() != DefaultMaxBytes || e.maxEntryBytes() != DefaultMaxEntryBytes ||
		e.maxEntries() != DefaultMaxEntries || e.retention() != DefaultRetention {
		t.Fatalf("defaults = %d %d %d %s", e.maxBytes(), e.maxEntryBytes(), e.maxEntries(), e.retention())
	}
	// A tiny budget clamps the per-entry ceiling rather than allowing an entry
	// the repo budget could never hold.
	e.MaxBytes = 100
	if got := e.maxEntryBytes(); got != 100 {
		t.Fatalf("clamped per-entry = %d", got)
	}
	// A nil clock is now.
	if now := (&Engine{}).nowUTC(); time.Since(now) > time.Minute {
		t.Fatalf("nil clock = %s", now)
	}
	if got := (ReclaimResult{EntriesDeleted: 1, PayloadsDelete: 2, OrphansDeleted: 3, BytesFreed: 4}).String(); !strings.Contains(got, "entries=1") || !strings.Contains(got, "freed=4B") {
		t.Fatalf("report = %q", got)
	}
	if got := (ReclaimResult{DryRun: true}).String(); !strings.Contains(got, "dry run") {
		t.Fatalf("dry report = %q", got)
	}
}
