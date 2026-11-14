// cover_gaps_test.go — the low-volume branches the coverage gate needs and the
// package's behavioral tests do not otherwise reach: the evictor tick, the
// repo-list refresh failure path, the serve-health default that marks a
// status-less marker degraded, the heal-attempt reason fallbacks, and replay
// with nothing to replay.
package wal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/store"
)

// listFailStore fails every ListPrefixes call (a store outage during a
// listing refresh).
type listFailStore struct {
	store.ObjectStore
}

func (s *listFailStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return store.NewOther(prefix, errors.New("list unavailable"))
}

func TestRefreshListStoreErrorIsEmpty(t *testing.T) {
	r := NewRegistry(context.Background(), &listFailStore{store2()}, testConfig(t))
	defer r.Close()
	// A failed refresh is "no listing", never a partial or stale one.
	if got := r.refreshList(context.Background()); got != nil {
		t.Fatalf("refreshList on a failing store = %v, want nil", got)
	}
}

func TestEvictorLoopTicksAndStops(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r.wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.evictorLoop(ctx, 5*time.Millisecond)
	}()
	time.Sleep(40 * time.Millisecond) // let at least one tick run
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("evictorLoop did not exit on context cancellation")
	}
}

func TestServeHealthStatusDefaultIsDegraded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// A marker written without a status is still a degradation claim
	// (fail closed): readers must not treat it as healthy.
	if _, err := store.PutBytes(ctx, st, store.ServeHealthKey("acme", "api"),
		[]byte(`{"version":1,"reason":"serve sync timed out"}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	doc, ok := LoadServeHealth(ctx, st, "acme", "api")
	if !ok || doc == nil || doc.Status != ServeHealthDegraded {
		t.Fatalf("status-less marker = %+v ok=%v, want degraded", doc, ok)
	}
}

func TestWriteHealAttemptReasonFallbacks(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	// No previous marker and no reason: the synthetic reason is recorded.
	if err := WriteHealAttempt(ctx, st, "acme", "api", "", nil); err != nil {
		t.Fatal(err)
	}
	doc, ok := LoadServeHealth(ctx, st, "acme", "api")
	if !ok || doc.Attempts != 1 || doc.Reason != "heal attempt failed" {
		t.Fatalf("first heal attempt = %+v ok=%v", doc, ok)
	}

	// A later attempt with no new reason keeps the previous one and increments.
	if err := WriteHealAttempt(ctx, st, "acme", "api", "", doc); err != nil {
		t.Fatal(err)
	}
	doc2, _ := LoadServeHealth(ctx, st, "acme", "api")
	if doc2.Attempts != 2 || doc2.Reason != doc.Reason {
		t.Fatalf("second heal attempt = %+v, want attempts=2 reason=%q", doc2, doc.Reason)
	}
}

func TestReplayEntriesEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(ctx, store2(), testConfig(t))
	defer r.Close()
	h, err := r.Create(ctx, "acme/empty", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to replay: no snapshot, no rewrite, no error.
	if err := h.replayEntries(ctx, nil); err != nil {
		t.Fatalf("replayEntries(nil) = %v", err)
	}
}
