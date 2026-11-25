// warmth_test.go — the PacksReady bookkeeping under the two writers that can
// touch it: a detached (timeout-continued) materialize body and the publish
// path's keep-ready rule (doc 05 §5.2 step 3, §5.1.4; 20_search.md §3.1).
//
// Both counters are forward-only and the publish commit's keep-ready snapshot
// is taken under stateMu: without that, a stale body for revision 5 could
// overwrite PacksRevision=6 set by a concurrent publish (Revision=6 /
// PacksRevision=5 → a fully warm repository reads cold forever), and the
// keep-ready read itself was a data race against the materialize write.
package wal

import (
	"context"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/git"
)

// stateOf reads the two warmth counters under stateMu (the only safe way).
func stateOf(h *RepoHandle) (revision, packsRevision uint64) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.state.Revision, h.state.PacksRevision
}

// snapshotManifest builds a manifest snapshot a materialize body might apply.
func snapshotManifest(repo string, revision uint64) *pbManifest {
	return &pbManifest{FormatVersion: 1, Repo: repo, ObjectFormat: "sha1", Revision: revision}
}

// TestMaterializePacksRevisionForwardOnly drives the reviewer's stale-body
// scenario deterministically: a publish carried the state to revision 6
// (packs ready at 6), then a detached materialize body for revision 5 runs.
// The expected state is (6, 6, ready) — never (6, 5, cold).
func TestMaterializePacksRevisionForwardOnly(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/app", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The concurrent publish's keep-ready rule advanced both counters.
	if err := h.updateState(func(st *RepoState) {
		st.Revision = 6
		st.PacksRevision = 6
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.materialize(ctx, nil, snapshotManifest("acme/app", 5), LevelServe); err != nil {
		t.Fatalf("stale materialize: %v", err)
	}
	rev, packs := stateOf(h)
	if rev != 6 || packs != 6 || !h.PacksReady() {
		t.Fatalf("after a stale materialize: (revision %d, packs_revision %d, ready %v), want (6, 6, true)", rev, packs, h.PacksReady())
	}

	// A newer snapshot still advances both (forward-only, not frozen).
	if err := h.materialize(ctx, nil, snapshotManifest("acme/app", 9), LevelServe); err != nil {
		t.Fatalf("forward materialize: %v", err)
	}
	rev, packs = stateOf(h)
	if rev != 9 || packs != 9 || !h.PacksReady() {
		t.Fatalf("after a forward materialize: (revision %d, packs_revision %d, ready %v), want (9, 9, true)", rev, packs, h.PacksReady())
	}
}

// TestPublishKeepReadySemantics pins the keep-ready rule itself (unchanged by
// moving the snapshot under stateMu): a warm repository stays warm across a
// publish, a cold one stays cold, and a pending removal blocks the carry.
func TestPublishKeepReadySemantics(t *testing.T) {
	ctx := context.Background()
	next := snapshotManifest("acme/app", 4)
	next.HeadSeq = 2
	tests := []struct {
		name          string
		revision      uint64
		packsRevision uint64
		pending       []string
		wantRevision  uint64
		wantPacks     uint64
		wantReady     bool
	}{
		{name: "warm carries readiness", revision: 3, packsRevision: 3, wantRevision: 4, wantPacks: 4, wantReady: true},
		{name: "cold stays cold", revision: 3, packsRevision: 2, wantRevision: 4, wantPacks: 2, wantReady: false},
		{name: "pending removal blocks the carry", revision: 3, packsRevision: 3, pending: []string{"ghost"}, wantRevision: 4, wantPacks: 3, wantReady: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRegistry(t)
			h, err := r.Create(ctx, "acme/app", git.Sha1)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.updateState(func(st *RepoState) {
				st.Revision = tc.revision
				st.PacksRevision = tc.packsRevision
				st.PendingPackRemovals = tc.pending
			}); err != nil {
				t.Fatal(err)
			}
			pub := &Publisher{h: h}
			pub.commitLocal(ctx, next, "version-4", nil, nil)
			rev, packs := stateOf(h)
			if rev != tc.wantRevision || packs != tc.wantPacks || h.PacksReady() != tc.wantReady {
				t.Fatalf("state = (revision %d, packs_revision %d, ready %v), want (%d, %d, %v)",
					rev, packs, h.PacksReady(), tc.wantRevision, tc.wantPacks, tc.wantReady)
			}
		})
	}
}

// TestPublishKeepReadySnapshotUnderStateLock exercises the real publish commit
// concurrently with the real materialize state write, under -race.
//
// Caveat, stated rather than hidden: the Go detector cannot DETERMINISTICALLY
// flag this pattern, because the publish read sits between the same goroutine's
// locked writes and the detector's per-address shadow is dominated by those
// ordered accesses. The probe still exercises the shipped path concurrently
// (it catches gross races and deadlocks) and asserts the coherence property
// that matters — neither counter ever moves backwards and a warm repository
// ends warm. The stale-body defect itself is pinned deterministically by
// TestMaterializePacksRevisionForwardOnly.
func TestPublishKeepReadySnapshotUnderStateLock(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/app", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	pub := &Publisher{h: h}
	next := snapshotManifest("acme/app", 7)
	next.HeadSeq = 3
	stale := snapshotManifest("acme/app", 2)

	const rounds = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	var born sync.WaitGroup
	wg.Add(2)
	born.Add(2)
	go func() {
		defer wg.Done()
		born.Done()
		<-start
		for i := 0; i < rounds; i++ {
			pub.commitLocal(ctx, next, "version-7", nil, nil)
		}
	}()
	go func() {
		defer wg.Done()
		born.Done()
		<-start
		var lastRev, lastPacks uint64
		for i := 0; i < rounds; i++ {
			if err := h.materialize(ctx, nil, stale, LevelServe); err != nil {
				t.Errorf("materialize: %v", err)
				return
			}
			rev, packs := stateOf(h)
			if rev < lastRev {
				t.Errorf("revision went backwards: %d → %d", lastRev, rev)
			}
			if packs < lastPacks {
				t.Errorf("packs_revision went backwards: %d → %d", lastPacks, packs)
			}
			lastRev, lastPacks = rev, packs
		}
	}()
	born.Wait()
	close(start)
	wg.Wait()

	// The publish revision is the highest either writer saw, and the keep-ready
	// rule carried packs with it: a warm repository stays warm.
	rev, packs := stateOf(h)
	if rev != 7 || packs != 7 || !h.PacksReady() {
		t.Fatalf("final state: (revision %d, packs_revision %d, ready %v), want (7, 7, true)", rev, packs, h.PacksReady())
	}
}
