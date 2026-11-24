// restart_test.go — the bridge cursor survives a restart: a new Bridge over
// the same store (the process-restart shape) neither re-delivers the batch the
// previous instance published nor skips a later one. This is the at-least-once
// boundary the repo-hook push sink inherits.
package events

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/store/proto"
)

func TestCatchUp_RestartDoesNotRedeliver(t *testing.T) {
	a, b := "aaaa", "bbbb"
	repo := &fakeRepo{head: 2, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, a)),
		mkEntry(2, proto.EntryKindPush, nil, upd("refs/heads/main", a, b)),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("repo-hooks")
	br, _, st := newTestBridge(t, src, sink)

	if n, err := br.catchUp(context.Background(), "owner/r0"); err != nil || n != 2 {
		t.Fatalf("first catchUp = (%d, %v), want (2, nil)", n, err)
	}
	if sink.deliveries() != 1 || len(sink.lastBatch()) != 2 {
		t.Fatalf("first pass: deliveries=%d batch=%d", sink.deliveries(), len(sink.lastBatch()))
	}

	// Restart: a fresh Bridge (fresh in-memory state) over the same store.
	restarted := New(Deps{Source: src, Store: st, Sinks: []Sink{sink}})
	if n, err := restarted.catchUp(context.Background(), "owner/r0"); err != nil || n != 0 {
		t.Fatalf("restarted catchUp = (%d, %v), want (0, nil)", n, err)
	}
	if sink.deliveries() != 1 {
		t.Fatalf("restart re-delivered: deliveries=%d", sink.deliveries())
	}

	// A new entry after the restart is delivered exactly once.
	repo.mu.Lock()
	repo.head = 3
	repo.entries = append(repo.entries, mkEntry(3, proto.EntryKindPush, nil, upd("refs/heads/dev", testZero40, b)))
	repo.mu.Unlock()
	if n, err := restarted.catchUp(context.Background(), "owner/r0"); err != nil || n != 1 {
		t.Fatalf("post-restart catchUp = (%d, %v), want (1, nil)", n, err)
	}
	if sink.deliveries() != 2 {
		t.Fatalf("post-restart deliveries = %d, want 2", sink.deliveries())
	}
}
