package notify

// moderation_test.go — PLAN-V2 §M.5's mention filter: a blocked principal's
// interaction never produces a notification for the blocker, the filter runs
// before the activity append (so the overflow path cannot reintroduce the
// name), and it creates no duplicates.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeBlockGate struct {
	pairs map[string]bool
	err   error
}

func newFakeBlockGate() *fakeBlockGate { return &fakeBlockGate{pairs: map[string]bool{}} }

func (f *fakeBlockGate) block(blocker, subject string) { f.pairs[blocker+"\x00"+subject] = true }

func (f *fakeBlockGate) BlockedBy(_ context.Context, blocker, subject string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.pairs[blocker+"\x00"+subject], nil
}

func TestBlockedActorNeverNotifiesBlocker(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")

	gate := newFakeBlockGate()
	gate.block("amy@example.com", "bob@example.com") // amy blocked bob
	x.svc.Blocks = gate

	// bob (blocked by amy) mentions amy and carol.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "commented",
		[]string{"amy@example.com", "carol@example.com"})

	if got := countNotifs(t, x, "amy@example.com"); got != 0 {
		t.Fatalf("blocker notified %d times", got)
	}
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("third party notifications = %d, want 1", got)
	}
	// The activity event's recipient list is truthful: the suppressed name
	// never reaches the durable overflow queue either.
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev == nil {
		t.Fatal("no activity event")
	}
	var payload activityPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, r := range payload.Recipients {
		if r.Principal == "amy@example.com" {
			t.Fatalf("suppressed recipient persisted in the activity payload: %v", payload.Recipients)
		}
	}
	if len(payload.Recipients) != 1 || payload.Recipients[0].Principal != "carol@example.com" {
		t.Fatalf("payload recipients = %v", payload.Recipients)
	}
}

func TestBlockFilterDoesNotDuplicateOrRestrictReverse(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	gate := newFakeBlockGate()
	gate.block("amy@example.com", "bob@example.com")
	x.svc.Blocks = gate

	// carol is both a primary recipient and the thread author; the filter
	// must preserve the existing (principal, reason) dedupe exactly.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented",
		[]string{"carol@example.com"})
	// carol is not blocked and is one recipient per reason; she is not
	// notified twice for the same (thread, reason).
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("carol notifications = %d, want 1 (no duplicates)", got)
	}

	// Reverse: bob blocked carol (not the actor's blocker), so amy's
	// notification to bob is untouched.
	gate2 := newFakeBlockGate()
	gate2.block("bob@example.com", "carol@example.com")
	x.svc.Blocks = gate2
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "amy@example.com", "", "commented",
		[]string{"bob@example.com"})
	if got := countNotifs(t, x, "bob@example.com"); got != 1 {
		t.Fatalf("reverse-direction notifications = %d, want 1", got)
	}
}

func TestBlockFilterProbeErrorDropsRecipient(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	gate := newFakeBlockGate()
	gate.err = errors.New("store down")
	x.svc.Blocks = gate

	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "commented",
		[]string{"amy@example.com", "carol@example.com"})
	// Fail closed: both recipients dropped (silence beats notifying a person
	// who may have blocked the actor), and the emission still commits its
	// activity record so the webhook/backfill truth exists.
	if got := countNotifs(t, x, "amy@example.com"); got != 0 {
		t.Fatalf("probe failure notified the blocker %d times", got)
	}
	if got := countNotifs(t, x, "carol@example.com"); got != 0 {
		t.Fatalf("probe failure notified %d times, want 0 (fail closed)", got)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev == nil {
		t.Fatal("activity event must still land")
	}
}

func TestNilBlockGateAllows(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.Blocks = nil
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "commented",
		[]string{"amy@example.com"})
	if got := countNotifs(t, x, "amy@example.com"); got != 1 {
		t.Fatalf("nil gate notifications = %d, want 1", got)
	}
}
