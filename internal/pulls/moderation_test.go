package pulls

// moderation_test.go — the pulls half of PLAN-V2 §M.6.1: a blocked principal
// cannot open, update, comment on, merge, update-branch or delete the head
// of a pull request in the blocker's repository; forks are exempt; the
// reverse direction and host admins are unaffected.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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

func (f *fakeBlockGate) hide(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hidden[target] = HiddenInfo{By: "mod", At: "2026-11-21T12:00:00Z", Reason: "spam"}
	f.rev++
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

func refused(err error) bool {
	return errors.Is(err, ErrForbidden) &&
		strings.Contains(err.Error(), identity.InteractionRefused) &&
		!strings.Contains(strings.ToLower(err.Error()), "block")
}

func TestBlockedPrincipalCannotTouchPR(t *testing.T) {
	e := newTestEnv()
	gate := newFakeBlockGate()
	gate.block("dave", "bob@example.com")
	e.svc.Moderation = gate
	// bob has the roles every verb needs, so a role refusal cannot be the
	// explanation for the 403.
	e.roles.Roles["bob@example.com"] = "admin"
	e.roles.Roles["dave@example.com"] = "write"
	e.seedRefs("dave/repo", map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
	})
	bob := auth.Principal{Name: "bob@example.com"}
	dave := auth.Principal{Name: "dave@example.com"}

	// OpenPR is refused before any ref resolution.
	if _, _, err := e.svc.OpenPR(ctx(), "dave", "repo", bob, OpenInput{
		Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr"); !refused(err) {
		t.Fatalf("OpenPR = %v", err)
	}
	// Seed a PR as the owner, then every per-PR verb refuses bob.
	th, _, err := e.svc.OpenPR(ctx(), "dave", "repo", dave, OpenInput{
		Title: "seed", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr-seed")
	if err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	num := th.Num
	if _, _, err := e.svc.UpdatePR(ctx(), "dave", "repo", num, bob, PRPatch{Title: strPtr("x")}); !refused(err) {
		t.Fatalf("UpdatePR = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", num, bob, "x"); !refused(err) {
		t.Fatalf("AddComment = %v", err)
	}
	if _, err := e.svc.StartMerge(ctx(), "dave", "repo", num, bob, MergeInput{Strategy: StrategyMerge}, "corr"); !refused(err) {
		t.Fatalf("StartMerge = %v", err)
	}
	if _, err := e.svc.UpdateBranch(ctx(), "dave", "repo", num, bob, ""); !refused(err) {
		t.Fatalf("UpdateBranch = %v", err)
	}
	if err := e.svc.DeleteHead(ctx(), "dave", "repo", num, bob); !refused(err) {
		t.Fatalf("DeleteHead = %v", err)
	}
	// Forks are explicitly unaffected (PLAN-V2 §M.5): the block must not be
	// the reason StartFork stops.
	if _, _, err := e.svc.StartFork(ctx(), "dave", "repo", bob, ForkInput{}); refused(err) {
		t.Fatalf("StartFork refused by the block: %v", err)
	}
}

func TestPullsBlockDirectionAndAdmin(t *testing.T) {
	e := newTestEnv()
	gate := newFakeBlockGate()
	e.svc.Moderation = gate
	e.roles.Roles["bob@example.com"] = "write"
	e.roles.Roles["dave@example.com"] = "write"
	e.seedRefs("bob/repo", map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
		"refs/heads/other": hexSHA(3),
	})
	// dave blocked bob, but bob's own namespace stays writable to bob and to
	// dave (one-way).
	gate.block("dave", "bob@example.com")
	if _, _, err := e.svc.OpenPR(ctx(), "bob", "repo", auth.Principal{Name: "bob@example.com"}, OpenInput{
		Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr"); err != nil {
		t.Fatalf("bob own repo: %v", err)
	}
	if _, _, err := e.svc.OpenPR(ctx(), "bob", "repo", auth.Principal{Name: "dave@example.com"}, OpenInput{
		Title: "y", BaseRef: "refs/heads/main", HeadRef: "refs/heads/other",
	}, "corr2"); err != nil {
		t.Fatalf("dave in bob's repo: %v", err)
	}
	// Host admin exemption on the blocker's namespace.
	gate.block("dave", "root@example.com")
	if _, _, err := e.svc.OpenPR(ctx(), "dave", "repo", auth.Principal{Name: "root@example.com", Admin: true}, OpenInput{
		Title: "z", BaseRef: "refs/heads/main", HeadRef: "refs/heads/other",
	}, "corr3"); err != nil && refused(err) {
		t.Fatalf("host admin refused: %v", err)
	}
	// Gate failure fails closed (not a 403).
	bad := newFakeBlockGate()
	bad.err = errors.New("store down")
	e.svc.Moderation = bad
	if _, _, err := e.svc.OpenPR(ctx(), "dave", "repo", auth.Principal{Name: "bob@example.com"}, OpenInput{
		Title: "w", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr4"); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("gate outage = %v (want 503-class)", err)
	}
}

// --- M2 projection + lock ------------------------------------------------------

func (f *fakeBlockGate) snapshot() (map[string]HiddenInfo, map[int]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := map[string]HiddenInfo{}
	for k, v := range f.hidden {
		h[k] = v
	}
	l := map[int]string{}
	for k, v := range f.locks {
		l[k] = v
	}
	return h, l, f.rev
}

func TestHiddenPRBodiesNeverReachClient(t *testing.T) {
	e := newTestEnv()
	gate := newFakeBlockGate()
	e.svc.Moderation = gate
	e.roles.Public = true
	e.roles.Roles["dave@example.com"] = "write"
	e.roles.Roles["mod@example.com"] = "triage"
	e.seedRefs("dave/repo", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	dave := auth.Principal{Name: "dave@example.com"}
	th, _, err := e.svc.OpenPR(ctx(), "dave", "repo", dave, OpenInput{
		Title: "seed", Body: "secret description", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := e.svc.AddComment(ctx(), "dave", "repo", th.Num, dave, "secret comment")
	if err != nil {
		t.Fatal(err)
	}
	gate.hide(HiddenTarget(th.Num, 0)) // the opened event IS the description
	gate.hide(HiddenTarget(th.Num, ev.Seq))

	reader := auth.Principal{Name: "reader@example.com"}
	view, err := e.svc.GetPR(ctx(), "dave", "repo", th.Num, reader, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if view.PR.Body != "" || view.DescriptionHidden == nil || view.DescriptionHidden.Reason != "spam" {
		t.Fatalf("description = %q hidden=%+v", view.PR.Body, view.DescriptionHidden)
	}
	var commented *Event
	for _, e2 := range view.Events {
		if e2.Seq == ev.Seq {
			commented = e2
		}
	}
	if commented == nil || commented.Body != nil || commented.Hidden == nil {
		t.Fatalf("comment event = %+v", commented)
	}
	if view.ModerationRevision == 0 {
		t.Fatal("moderation revision not exposed for the ETag")
	}
	// The JSON lane shares the verb: no body reaches the payload.
	w := doReq(t, &Handler{Svc: e.svc}, http.MethodGet, "api", "/dave/repo/api/pulls/"+strconv.Itoa(th.Num), "", reader)
	if w.Code != http.StatusOK {
		t.Fatalf("JSON = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret description") || strings.Contains(w.Body.String(), "secret comment") {
		t.Fatalf("hidden body reached JSON: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "description_hidden") {
		t.Fatalf("description marker missing: %s", w.Body.String())
	}
	// Moderators see the originals with the marker.
	mv, err := e.svc.GetPR(ctx(), "dave", "repo", th.Num, auth.Principal{Name: "mod@example.com"}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if mv.PR.Body != "secret description" || mv.DescriptionHidden == nil {
		t.Fatalf("moderator description = %q", mv.PR.Body)
	}
}

func TestModerationLockedPRRefusesComment(t *testing.T) {
	e := newTestEnv()
	gate := newFakeBlockGate()
	e.svc.Moderation = gate
	e.roles.Public = true
	e.roles.Roles["dave@example.com"] = "write"
	e.seedRefs("dave/repo", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	dave := auth.Principal{Name: "dave@example.com"}
	th, _, err := e.svc.OpenPR(ctx(), "dave", "repo", dave, OpenInput{
		Title: "seed", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr")
	if err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	gate.locks[th.Num] = "flame war"
	gate.rev++
	gate.mu.Unlock()
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", th.Num, dave, "x"); !errors.Is(err, ErrLocked) {
		t.Fatalf("comment on locked PR = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", th.Num, auth.Principal{Name: "root", Admin: true}, "x"); !errors.Is(err, ErrLocked) {
		t.Fatalf("admin comment on locked PR = %v", err)
	}
	if _, err := e.svc.GetPR(ctx(), "dave", "repo", th.Num, dave, 0, 5); err != nil {
		t.Fatalf("read while locked = %v", err)
	}
	gate.mu.Lock()
	delete(gate.locks, th.Num)
	gate.mu.Unlock()
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", th.Num, dave, "x"); err != nil {
		t.Fatalf("comment after unlock = %v", err)
	}
}
