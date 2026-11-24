package identity

// blocks_test.go — PLAN-V2 §M.5's identity consumer gate: an invitation
// created BY a principal who has blocked the subject is refused (403, the
// shared neutral text), the reverse direction is untouched, and an unwired
// gate never breaks invites.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
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

func TestBlockedInviteRefused(t *testing.T) {
	ctx := context.Background()
	s := testService()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	gate := newFakeBlockGate()
	gate.block("alice@example.com", "bob@example.com")
	s.Blocks = gate

	_, err := s.CreateOrgInvite(ctx, "acme", "bob@example.com", "member", "alice@example.com", time.Hour)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), InteractionRefused) {
		t.Fatalf("refusal %q is not the neutral text", err.Error())
	}
	if strings.Contains(strings.ToLower(err.Error()), "block") {
		t.Fatalf("refusal leaks the block: %q", err.Error())
	}
	// Nothing was written: bob's inbox is empty and the org has no invite.
	entries, merr := s.MyInvites(ctx, "bob@example.com")
	if merr != nil || len(entries) != 0 {
		t.Fatalf("inbox after refusal = (%v, %v)", entries, merr)
	}
	var n int
	if lerr := s.Store.List(ctx, "orgs/acme/invitations/", "", func(store.ObjectMeta) error {
		n++
		return nil
	}); lerr != nil {
		t.Fatal(lerr)
	}
	if n != 0 {
		t.Fatalf("refused invite left %d objects", n)
	}

	// Reverse: bob blocked alice does not stop alice's invite of bob.
	rev := newFakeBlockGate()
	rev.block("bob@example.com", "alice@example.com")
	s.Blocks = rev
	if _, err := s.CreateOrgInvite(ctx, "acme", "bob@example.com", "member", "alice@example.com", time.Hour); err != nil {
		t.Fatalf("reverse direction refused: %v", err)
	}
}

func TestInviteGateNilAndError(t *testing.T) {
	ctx := context.Background()
	s := testService()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	s.Blocks = nil
	if _, err := s.CreateOrgInvite(ctx, "acme", "bob@example.com", "member", "alice@example.com", time.Hour); err != nil {
		t.Fatalf("nil gate refused: %v", err)
	}
	bad := newFakeBlockGate()
	bad.err = errors.New("store down")
	s.Blocks = bad
	if _, err := s.CreateOrgInvite(ctx, "acme", "carol@example.com", "member", "alice@example.com", time.Hour); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("gate outage = %v (want 503-class)", err)
	}
	// A system invite (blank inviter) skips the gate entirely.
	if _, err := s.CreateOrgInvite(ctx, "acme", "dave@example.com", "member", "", time.Hour); err != nil {
		t.Fatalf("system invite refused: %v", err)
	}
}
