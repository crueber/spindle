package wal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/store"
	"github.com/crueber/walhub/internal/store/proto"
)

// TestRefsAtSeqFreshenFailure pins the error path that unlocks the sync mutex
// when the manifest refresh fails: a store outage during a read must release
// the lock and surface the store error, never wedge the handle.
func TestRefsAtSeqFreshenFailure(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	r := NewRegistry(ctx, st, testConfig(t))
	defer r.Close()

	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Fail exactly the manifest read, and force it (a fresh handle would answer
	// from the TTL cache and never reach the store).
	r.st = &errAtStore{inner: st, failKey: manifestKey(h.ID)}
	h.freshAt = time.Time{}

	if _, err := h.RefsAtSeq(ctx, 1); err == nil {
		t.Fatal("RefsAtSeq with a failing manifest read must return an error")
	} else if werr, ok := err.(*WalError); !ok || werr.Kind != WalErrStore {
		t.Fatalf("RefsAtSeq error kind: %v", err)
	}

	// The lock must be free again: a subsequent call with the store restored
	// succeeds instead of blocking on a leaked mutex.
	r.st = st
	if _, err := h.RefsAtSeq(ctx, 0); err != nil {
		t.Fatalf("second RefsAtSeq (store restored): %v", err)
	}
}

// TestReplayEntriesSymbolicHead covers the replay branch that adopts a symbolic
// HEAD update (a default-branch move replayed offline). Without it the branch
// only runs on a specific WAL history, so the gate would depend on luck.
func TestReplayEntriesSymbolicHead(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(ctx, store.NewMemory(), testConfig(t))
	defer r.Close()
	h, err := r.Create(ctx, "acme/sym", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	entry := &proto.LogEntry{
		Seq:  1,
		Kind: proto.EntryKindRefUpdate,
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{
			{Name: "refs/heads/main", NewOid: strings.Repeat("1", 40)},
			{Name: "HEAD", NewSymbolicTarget: "refs/heads/main"},
		}},
		Writer: "test",
	}
	if err := h.replayEntries(ctx, []*proto.LogEntry{entry}); err != nil {
		t.Fatalf("replayEntries(symbolic HEAD): %v", err)
	}
}
