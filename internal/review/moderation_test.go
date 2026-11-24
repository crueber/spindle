package review

// moderation_test.go — the review half of PLAN-V2 §M.6.1 (blocked principal
// cannot review, comment on a review thread, resolve it, or touch review
// requests), plus the direction and admin-exemption arms.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

type fakeBlockGate struct {
	pairs  map[string]bool
	hidden map[string]HiddenInfo
	locks  map[int]string
	modes  map[int]string
	rev    int
	mu     sync.Mutex
	err    error
}

func newFakeBlockGate() *fakeBlockGate {
	return &fakeBlockGate{pairs: map[string]bool{}, hidden: map[string]HiddenInfo{}, locks: map[int]string{}, modes: map[int]string{}}
}

func (f *fakeBlockGate) block(owner, actor string) { f.pairs[owner+"\x00"+actor] = true }

func (f *fakeBlockGate) BlockedFromRepo(_ context.Context, owner, _, actor string) (bool, string, error) {
	if f.err != nil {
		return false, "", f.err
	}
	if f.pairs[owner+"\x00"+actor] {
		return true, "interaction not permitted", nil
	}
	return false, "", nil
}

// Snapshot is the M2 gate arm: hidden targets + this conversation's lock.
func (f *fakeBlockGate) Snapshot(_ context.Context, _, _ string, num int, fn func(string, string, string, string)) (bool, string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, "", "", 0, f.err
	}
	for target, h := range f.hidden {
		if fn != nil {
			fn(target, h.By, h.At, h.Reason)
		}
	}
	if reason, ok := f.locks[num]; ok {
		return true, f.lockMode(num), reason, f.rev, nil
	}
	return false, "", "", f.rev, nil
}

// lockMode reports the stored lock's mode (default "all").
func (f *fakeBlockGate) lockMode(num int) string {
	if f.modes != nil {
		if m, ok := f.modes[num]; ok {
			return m
		}
	}
	return "all"
}

func TestBlockedPrincipalCannotReview(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	gate := newFakeBlockGate()
	gate.block(testOwner, "alice")
	svc.Moderation = gate
	seedPR(t, svc)
	alice := testPrincipal("alice")

	refused := func(err error) bool {
		return errors.Is(err, ErrForbidden) && strings.Contains(err.Error(), identity.InteractionRefused) &&
			!strings.Contains(strings.ToLower(err.Error()), "block")
	}

	// SubmitReview, OpenThread, AddRequests: seeded PR, no extra state.
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, alice,
		SubmitInput{State: StateApproved, CommitSHA: testHead}); !refused(err) {
		t.Fatalf("SubmitReview err = %v", err)
	}
	if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, alice,
		testAnchor(), "x"); !refused(err) {
		t.Fatalf("OpenThread err = %v", err)
	}
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, alice, []string{"carol"}); !refused(err) {
		t.Fatalf("AddRequests err = %v", err)
	}
	if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, alice, []string{"carol"}); !refused(err) {
		t.Fatalf("RemoveRequests err = %v", err)
	}

	// A thread seeded by an unblocked actor: comment + resolve attempts are
	// refused for the blocked principal.
	th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"),
		testAnchor(), "seed")
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, alice, "x"); !refused(err) {
		t.Fatalf("AddThreadComment err = %v", err)
	}
	if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, th.TID, alice); !refused(err) {
		t.Fatalf("ResolveThread err = %v", err)
	}
	if _, err := svc.UnresolveThread(ctx, testOwner, testRepo, testPR, th.TID, alice); !refused(err) {
		t.Fatalf("UnresolveThread err = %v", err)
	}

	// DismissReview needs maintain; bob is maintain and blocked too.
	gate.block(testOwner, "bob")
	bob := testPrincipal("bob")
	// Seed a review as the PR author via a non-blocked maintainer-less path
	// (alice is the author but blocked, so erin, a host admin, submits).
	erin := auth.Principal{Name: "erin", Admin: true}
	ev, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, erin,
		SubmitInput{State: StateApproved, CommitSHA: testHead})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, ev.Seq, bob, "stale"); !refused(err) {
		t.Fatalf("DismissReview err = %v", err)
	}
}

func TestReviewBlockDirectionAndAdmin(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	gate := newFakeBlockGate()
	svc.Moderation = gate
	seedPR(t, svc)
	// o blocked alice; alice acting in carol's namespace is unaffected —
	// modelled by blocking a different owner pair.
	gate.block("carol", "alice")
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("alice"),
		SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
		t.Fatalf("reverse direction refused: %v", err)
	}
	// Host admin is never blocked out.
	gate.block(testOwner, "erin")
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR,
		auth.Principal{Name: "erin", Admin: true}, SubmitInput{State: StateCommented, CommitSHA: testHead}); err != nil {
		t.Fatalf("host admin refused: %v", err)
	}
	// Nil gate allows; gate errors fail closed.
	svc.Moderation = nil
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("alice"),
		SubmitInput{State: StateCommented, CommitSHA: testHead}); err != nil {
		t.Fatalf("nil gate refused: %v", err)
	}
	bad := newFakeBlockGate()
	bad.err = errors.New("store down")
	svc.Moderation = bad
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("alice"),
		SubmitInput{State: StateCommented, CommitSHA: testHead}); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("gate error = %v (want a 503-class error)", err)
	}
}

// --- M2 projection + lock ------------------------------------------------------

func (f *fakeBlockGate) hide(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hidden[target] = HiddenInfo{By: "mod", At: "2026-11-21T12:00:00Z", Reason: "abusive"}
	f.rev++
}

func TestHiddenReviewBodiesNeverReachClient(t *testing.T) {
	svc, _ := testSvc()
	seedPR(t, svc)
	gate := newFakeBlockGate()
	svc.Moderation = gate
	alice := testPrincipal("alice")
	carol := testPrincipal("carol") // read
	dave := testPrincipal("dave")   // triage (moderator)

	ev, _, _, err := svc.SubmitReview(context.Background(), testOwner, testRepo, testPR, alice, SubmitInput{
		State: StateCommented, Body: "secret review", CommitSHA: testHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	th, err := svc.OpenThread(context.Background(), testOwner, testRepo, testPR, alice, Anchor{
		Path: "a.go", Side: SideNew, NewStart: 1, NewLines: 1, CommitSHA: testHead,
		ContextSHA: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}, "secret thread comment")
	if err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	gate.hidden[reviewTarget(testPR, ev.Seq)] = HiddenInfo{By: "mod", At: "t", Reason: "abusive"}
	gate.hidden[reviewThreadTarget(testPR, th.TID, 1)] = HiddenInfo{By: "mod", At: "t", Reason: "abusive"}
	gate.rev += 2
	gate.mu.Unlock()

	res, err := svc.ListReviews(context.Background(), testOwner, testRepo, testPR, carol, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reviews) != 1 || res.Reviews[0].Body != "" || res.Reviews[0].Hidden == nil {
		t.Fatalf("reader review = %+v", res.Reviews)
	}
	// The single-review read redacts the same way.
	one, err := svc.GetReview(context.Background(), testOwner, testRepo, testPR, ev.Seq, carol)
	if err != nil || one.Body != "" || one.Hidden == nil {
		t.Fatalf("reader get review = (%+v, %v)", one, err)
	}
	// JSON lane: no body in the payload.
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return carol, nil
	}}
	w := doReq(h, http.MethodGet, "/o/r/api/pulls/7/reviews", "", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret review") {
		t.Fatalf("JSON reviews = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"hidden"`) {
		t.Fatalf("review hidden marker missing: %s", w.Body.String())
	}
	// Thread comments redact in the JSON lane too.
	tv, err := svc.GetThread(context.Background(), testOwner, testRepo, testPR, th.TID, carol, 0, 50)
	if err != nil || len(tv.Comments) != 1 || tv.Comments[0].Body != "" || tv.Comments[0].Hidden == nil {
		t.Fatalf("reader thread = (%+v, %v)", tv, err)
	}
	w = doReq(h, http.MethodGet, "/o/r/api/pulls/7/threads/"+th.TID, "", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret thread comment") {
		t.Fatalf("JSON thread = %d: %s", w.Code, w.Body.String())
	}
	// The moderator sees both originals with the markers.
	mres, _ := svc.ListReviews(context.Background(), testOwner, testRepo, testPR, dave, 0, 50)
	if len(mres.Reviews) != 1 || mres.Reviews[0].Body != "secret review" || mres.Reviews[0].Hidden == nil {
		t.Fatalf("moderator review = %+v", mres.Reviews)
	}
	mtv, _ := svc.GetThread(context.Background(), testOwner, testRepo, testPR, th.TID, dave, 0, 50)
	if mtv.Comments[0].Body != "secret thread comment" || mtv.Comments[0].Hidden == nil {
		t.Fatalf("moderator thread = %+v", mtv.Comments)
	}
}

func TestModerationLockRefusesReviewWrites(t *testing.T) {
	svc, _ := testSvc()
	seedPR(t, svc)
	gate := newFakeBlockGate()
	svc.Moderation = gate
	gate.mu.Lock()
	gate.locks[testPR] = "cooling off"
	gate.mu.Unlock()
	alice := testPrincipal("alice")
	if _, _, _, err := svc.SubmitReview(context.Background(), testOwner, testRepo, testPR, alice, SubmitInput{
		State: StateCommented, Body: "x", CommitSHA: testHead,
	}); !errors.Is(err, ErrLocked) {
		t.Fatalf("submit on locked PR = %v", err)
	}
	if _, err := svc.OpenThread(context.Background(), testOwner, testRepo, testPR, alice, Anchor{
		Path: "a.go", Side: SideNew, NewStart: 1, NewLines: 1, CommitSHA: testHead,
		ContextSHA: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}, "x"); !errors.Is(err, ErrLocked) {
		t.Fatalf("open thread on locked PR = %v", err)
	}
	// A host admin is refused too (no override), reads still work.
	admin := auth.Principal{Name: "root", Admin: true}
	if _, _, _, err := svc.SubmitReview(context.Background(), testOwner, testRepo, testPR, admin, SubmitInput{
		State: StateCommented, Body: "x", CommitSHA: testHead,
	}); !errors.Is(err, ErrLocked) {
		t.Fatalf("admin submit on locked PR = %v", err)
	}
	if _, err := svc.ListReviews(context.Background(), testOwner, testRepo, testPR, alice, 0, 50); err != nil {
		t.Fatalf("read while locked = %v", err)
	}
	gate.mu.Lock()
	delete(gate.locks, testPR)
	gate.mu.Unlock()
	if _, _, _, err := svc.SubmitReview(context.Background(), testOwner, testRepo, testPR, alice, SubmitInput{
		State: StateCommented, Body: "x", CommitSHA: testHead,
	}); err != nil {
		t.Fatalf("submit after unlock = %v", err)
	}
}
