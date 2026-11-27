package threads

// Lease renewal and entry retirement edge paths (queue.go, 15 §3). The claim
// lease is renewed by accepted attempt traffic, coalesced to one write per
// half-lease; a corrupt entry is ignored rather than crashing the sweeper.

import (
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

func TestRenewLeaseWritesThenCoalesces(t *testing.T) {
	e, runID := needsEnv(t, independentWorkflowYAML)
	r := needsRunner()
	claim, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels)
	if err != nil || claim == nil || claim.Entry.RunID != runID {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	// A fresh claim's lease is a full window away: the caller's first renewal
	// is coalesced (no write).
	if until := e.svc.renewLease(ctx(), "o", "r", claim.Entry, claim.Job); until != nil {
		t.Fatalf("a fresh lease was rewritten: %v", until)
	}
	// Past half the window it writes, and a second call coalesces again. The
	// window is the 30-minute cap (the default 360-minute job timeout), so half
	// is 15 minutes.
	e.advance(20 * time.Minute)
	until := e.svc.renewLease(ctx(), "o", "r", claim.Entry, claim.Job)
	if until == nil {
		t.Fatal("a half-lapsed lease was not renewed")
	}
	if again := e.svc.renewLease(ctx(), "o", "r", claim.Entry, claim.Job); again != nil {
		t.Fatalf("a just-renewed lease was written again: %v", again)
	}
}

func TestFinishEntryIgnoresACorruptEntry(t *testing.T) {
	e, _ := needsEnv(t, independentWorkflowYAML)
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil || len(head.Pending) == 0 {
		t.Fatalf("head = %+v, %v", head, err)
	}
	seq := head.Pending[0]
	if _, perr := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", seq), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatalf("corrupt entry: %v", perr)
	}
	// The retirement pass must not panic on an unreadable entry (best effort:
	// threads-gc rebuilds the head).
	e.svc.finishEntry(ctx(), "o", "r", seq)
	e.svc.finishEntry(ctx(), "o", "r", "") // no seq is a no-op
}
