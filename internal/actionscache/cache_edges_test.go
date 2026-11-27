package actionscache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// The remaining engine edges: the validation arms that only a caller can reach,
// a clock that runs backwards, a shared payload across a replace, a scope
// discard that keeps other scopes, and the sweeper's starvation/error paths.

func TestLookupAndSaveValidationArms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scope := Scope{Kind: KindDefault}
	if _, err := e.eng.Lookup(ctx, "o", "r", []Scope{scope}, "", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty lookup key = %v", err)
	}
	if _, err := e.eng.Lookup(ctx, "o", "r", []Scope{scope}, "k", strings.Repeat("v", MaxVersionLen+1), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long lookup version = %v", err)
	}
	if _, err := e.eng.HasBlob(ctx, "o", "r", "bad", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("HasBlob bad sha = %v", err)
	}
	if _, err := e.eng.Save(ctx, "o", "r", scope, SaveInput{
		Key: "k", Version: strings.Repeat("v", MaxVersionLen+1), SHA256: shaOf("x"), Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("save long version = %v", err)
	}
	if _, err := e.eng.Save(ctx, "o", "r", scope, SaveInput{
		Key: "k", Version: "v", SHA256: "bad", Size: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("save bad sha = %v", err)
	}
}

func TestIndexWithoutBlobsIsEmptyNotCorrupt(t *testing.T) {
	e := newEnv(t)
	if _, err := store.PutBytes(e.ctx, e.store, IndexKey("o", "r"), []byte(`{"schema":1,"entries":[]}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if entries, err := e.eng.List(e.ctx, e.owner, e.repo); err != nil || len(entries) != 0 {
		t.Fatalf("index without blobs = %v %v", entries, err)
	}
	// A save onto it works (the map is initialised).
	sha, size := e.put("x")
	if _, err := e.eng.Save(e.ctx, "o", "r", Scope{Kind: KindDefault}, SaveInput{Key: "k", Version: "v", SHA256: sha, Size: size}); err != nil {
		t.Fatalf("save onto a blob-less index: %v", err)
	}
}

func TestBackwardsClockNeverEvictsTheNewEntry(t *testing.T) {
	e := newEnv(t)
	e.eng.MaxBytes = 10
	e.eng.MaxEntries = 4
	e.now = e.now.Add(time.Hour)
	e.save(Scope{Kind: KindDefault}, "a", "v", strings.Repeat("a", 8))
	// The clock moves backwards: the new entry sorts oldest and the eviction
	// loop must still never drop the entry being saved.
	e.now = e.now.Add(-2 * time.Hour)
	b := e.save(Scope{Kind: KindDefault}, "b", "v", strings.Repeat("b", 8))
	entries, _ := e.eng.List(e.ctx, e.owner, e.repo)
	found := false
	for _, ent := range entries {
		if ent.ID == b.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the new entry was evicted by a backwards clock: %v", entries)
	}
}

func TestReplaceKeepsAPayloadAnotherEntryUses(t *testing.T) {
	e := newEnv(t)
	sha, size := e.put("shared")
	first, err := e.eng.Save(e.ctx, "o", "r", Scope{Kind: KindDefault}, SaveInput{Key: "k", Version: "v", SHA256: sha, Size: size})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A second entry (another key) points at the same payload.
	if _, err := e.eng.Save(e.ctx, "o", "r", Scope{Kind: KindBranch, Branch: "b"}, SaveInput{Key: "other", Version: "v", SHA256: sha, Size: size}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	// Replacing the first key supersedes its entry; the shared payload stays
	// because the branch entry still points at it.
	e.now = e.now.Add(time.Minute)
	other, otherSize := e.put("different")
	if _, err := e.eng.Save(e.ctx, "o", "r", Scope{Kind: KindDefault}, SaveInput{Key: "k", Version: "v", SHA256: other, Size: otherSize}); err != nil {
		t.Fatalf("replace save: %v", err)
	}
	if meta, _ := e.store.Head(e.ctx, BlobKey("o", "r", first.SHA256)); meta == nil {
		t.Fatal("a payload another entry still references was deleted")
	}
}

func TestDiscardScopeKeepsOtherEntries(t *testing.T) {
	e := newEnv(t)
	e.save(Scope{Kind: KindPR, PR: 2}, "a", "v", "aaa")
	e.save(Scope{Kind: KindDefault}, "b", "v", "bbb")
	freed, err := e.eng.DiscardScope(e.ctx, "o", "r", Scope{Kind: KindPR, PR: 2})
	if err != nil || freed != 3 {
		t.Fatalf("DiscardScope = %d %v", freed, err)
	}
	entries, _ := e.eng.List(e.ctx, e.owner, e.repo)
	if len(entries) != 1 || entries[0].Scope != "default" {
		t.Fatalf("discard removed the wrong scope: %v", entries)
	}
}

func TestReclaimStarvationAndOrphanDeleteFailure(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	e.eng.Retention = time.Second
	e.save(Scope{Kind: KindDefault}, "a", "v", "aaa")
	e.now = e.now.Add(time.Hour)
	f.alwaysStale = true
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("starved reclaim = %v", err)
	}
	f.alwaysStale = false

	// An orphan whose delete fails fails the pass (the index is untouched).
	orphan := shaOf("orphan")
	if err := e.eng.PutBlob(ctx, "o", "r", orphan, 6, strings.NewReader("orphan")); err != nil {
		t.Fatalf("orphan put: %v", err)
	}
	if err := e.store.Delete(ctx, IndexKey("o", "r"), ""); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	f.deleteErr = errBoom
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); !errors.Is(err, errBoom) {
		t.Fatalf("orphan delete failure = %v", err)
	}
	f.deleteErr = nil

	// Two entries with the same creation stamp sort by id (a deterministic
	// order across instances).
	e.eng.Retention = time.Hour
	e.now = e.now.Add(time.Hour)
	a := e.save(Scope{Kind: KindDefault}, "x", "v", "xxx")
	b := e.save(Scope{Kind: KindDefault}, "y", "v", "yyy")
	if a.CreatedAt != b.CreatedAt {
		t.Fatalf("fixture: stamps differ (%s, %s)", a.CreatedAt, b.CreatedAt)
	}
	e.eng.MaxEntries = 1
	res, err := e.eng.Reclaim(ctx, "o", "r", true)
	if err != nil || res.EntriesDeleted != 1 {
		t.Fatalf("same-stamp reclaim = %+v %v", res, err)
	}
	entries, _ := e.eng.List(ctx, "o", "r")
	want := a.ID
	if b.ID > a.ID {
		want = b.ID
	}
	if len(entries) != 1 || entries[0].ID != want {
		t.Fatalf("same-stamp eviction kept %v, want the higher id %s", entries, want)
	}
}

func TestReclaimHonoursCancellation(t *testing.T) {
	e := newEnv(t)
	e.save(Scope{Kind: KindDefault}, "a", "v", "aaa")
	orphan := shaOf("orphan")
	if err := e.eng.PutBlob(e.ctx, "o", "r", orphan, 6, strings.NewReader("orphan")); err != nil {
		t.Fatalf("orphan put: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reclaim = %v", err)
	}
}

func TestVisibleBlobScopeGate(t *testing.T) {
	e := newEnv(t)
	sha, size := e.put("visible")
	// Nothing points at the payload yet: no run can see it.
	if _, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindDefault}}, sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible payload = %v", err)
	}
	if _, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindDefault}}, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sha = %v", err)
	}
	if _, err := e.eng.Save(e.ctx, "o", "r", Scope{Kind: KindPR, PR: 2}, SaveInput{
		Key: "k", Version: "v", SHA256: sha, Size: size,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// The owner scope sees it; another PR does not, and a different payload
	// digest never matches even inside the right scope.
	if got, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindPR, PR: 2}}, sha); err != nil || got.SHA256 != sha {
		t.Fatalf("visible = %+v %v", got, err)
	}
	if _, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindPR, PR: 3}}, sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other PR = %v", err)
	}
	other, otherSize := e.put("other")
	_ = otherSize
	if _, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindPR, PR: 2}}, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unreferenced digest in a visible scope = %v", err)
	}
	// A store outage is surfaced.
	f := &faultStore{ObjectStore: e.store, getErr: errBoom}
	e.eng.Store = f
	if _, err := e.eng.VisibleBlob(e.ctx, "o", "r", []Scope{{Kind: KindPR, PR: 2}}, sha); !errors.Is(err, errBoom) {
		t.Fatalf("outage = %v", err)
	}
	e.eng.Store = e.store
}

func TestEvictionKeepsTheNewestOnEqualStamps(t *testing.T) {
	e := newEnv(t)
	e.eng.MaxEntries = 1
	e.eng.MaxBytes = 1 << 20
	a := e.save(Scope{Kind: KindDefault}, "a", "v", "aaa")
	b := e.save(Scope{Kind: KindDefault}, "b", "v", "bbb") // same clock tick
	entries, _ := e.eng.List(e.ctx, e.owner, e.repo)
	// Exactly one survives, and it is the higher (id-ordered) of the two.
	want := a.ID
	if b.ID > a.ID {
		want = b.ID
	}
	if len(entries) != 1 || entries[0].ID != want {
		t.Fatalf("entries = %v, want %s", entries, want)
	}
}
