package review

// lockmode_test.go — the §10.1 lock-mode half of the M2 gate as review
// enforces it: `all` refuses everyone; `collaborators` admits write+ (a host
// admin included) and still refuses read.

import (
	"context"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestReviewLockModes(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	gate := newFakeBlockGate()
	gate.locks[testPR] = "flame war"
	gate.modes[testPR] = "all"
	svc.Moderation = gate
	seedPR(t, svc)

	in := SubmitInput{State: StateApproved, CommitSHA: testHead}
	// all: a writer, a read collaborator and a host admin are refused.
	for _, actor := range []auth.Principal{
		testPrincipal("alice"), testPrincipal("carol"), {Name: "root", Admin: true},
	} {
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, actor, in); !errors.Is(err, ErrLocked) {
			t.Fatalf("all-mode review by %s err = %v", actor.Name, err)
		}
	}

	// collaborators: alice (write) and the host admin may review; carol
	// (read) is refused.
	gate.modes[testPR] = "collaborators"
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), in); err != nil {
		t.Fatalf("write review under collaborators = %v", err)
	}
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, auth.Principal{Name: "root", Admin: true}, in); err != nil {
		t.Fatalf("admin review under collaborators = %v", err)
	}
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), in); !errors.Is(err, ErrLocked) {
		t.Fatalf("read review under collaborators err = %v", err)
	}
}
