package issues

// moderation_test.go — the consumer half of PLAN-V2 §M.6.1: every interaction
// verb refuses a principal the blocker's user namespace blocked, allows the
// reverse direction, exempts host admins, and never leaks the block.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// fakeModeration is the consumer-owned gate double: "owner\x00actor" pairs.
type fakeModeration struct {
	pairs  map[string]bool
	err    error
	hidden map[string]HiddenInfo
	locks  map[int]string
	modes  map[int]string
}

func newFakeModeration() *fakeModeration {
	return &fakeModeration{pairs: map[string]bool{}, hidden: map[string]HiddenInfo{}, locks: map[int]string{}, modes: map[int]string{}}
}

func (f *fakeModeration) Block(dave, actor string) { f.pairs[dave+"\x00"+actor] = true }

func (f *fakeModeration) BlockedFromRepo(_ context.Context, owner, _, actor string) (bool, string, error) {
	if f.err != nil {
		return false, "", f.err
	}
	if f.pairs[owner+"\x00"+actor] {
		return true, "interaction not permitted", nil
	}
	return false, "", nil
}

// Snapshot is the M2 gate arm: the repo's hidden targets plus this
// conversation's lock, in one call.
func (f *fakeModeration) Snapshot(_ context.Context, _, _ string, num int, fn func(string, string, string, string)) (bool, string, string, int, error) {
	if f.err != nil {
		return false, "", "", 0, f.err
	}
	for target, h := range f.hidden {
		if fn != nil {
			fn(target, h.By, h.At, h.Reason)
		}
	}
	rev := len(f.hidden) + len(f.locks)
	mode := f.modes[num]
	if mode == "" {
		mode = "all"
	}
	if reason, ok := f.locks[num]; ok {
		return true, mode, reason, rev, nil
	}
	return false, "", "", rev, nil
}

func (f *fakeModeration) hide(target string) {
	f.hidden[target] = HiddenInfo{By: "mod", At: "2026-11-21T12:00:00Z", Reason: "spam"}
}

func (f *fakeModeration) lock(num int, reason string) { f.locks[num] = reason }

// lockMode records a collaborators-only lock for one conversation.
func (f *fakeModeration) lockMode(num int, mode, reason string) {
	f.locks[num] = reason
	f.modes[num] = mode
}

func TestBlockedPrincipalCannotInteract(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	// dave (the repo owner) blocked bob.
	mod.Block("dave", "bob")
	// bob has write access, so a role refusal cannot explain a 403 — the
	// block must be the reason.
	roles.grant("dave", "repo", "bob", "write")

	seed := func(t *testing.T) *Thread {
		t.Helper()
		th, _, err := s.CreateIssue(reqCtx(), "dave", "repo", auth.Principal{Name: "dave"}, "seed", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return th
	}

	bob := auth.Principal{Name: "bob"}

	t.Run("open issue", func(t *testing.T) {
		_, _, err := s.CreateIssue(reqCtx(), "dave", "repo", bob, "hi", "", nil)
		assertRefused(t, err)
	})
	t.Run("comment", func(t *testing.T) {
		th := seed(t)
		if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "hi"); !isRefused(err) {
			t.Fatalf("comment err = %v", err)
		}
	})
	t.Run("reaction", func(t *testing.T) {
		th := seed(t)
		if _, _, _, err := s.AddReaction(reqCtx(), "dave", "repo", th.Num, 0, bob, "+1"); !isRefused(err) {
			t.Fatalf("reaction err = %v", err)
		}
	})
	t.Run("assign", func(t *testing.T) {
		th := seed(t)
		if _, err := s.PatchThreadMetadata(reqCtx(), "dave", "repo", th.Num, bob, IssuePatch{Assignees: &[]string{"bob"}}); !isRefused(err) {
			t.Fatalf("assign err = %v", err)
		}
	})
	t.Run("patch issue", func(t *testing.T) {
		th := seed(t)
		if _, err := s.PatchIssue(reqCtx(), "dave", "repo", th.Num, bob, IssuePatch{}); !isRefused(err) {
			t.Fatalf("patch err = %v", err)
		}
	})
	t.Run("attachment", func(t *testing.T) {
		if _, err := s.UploadAttachment(reqCtx(), "dave", "repo", bob, "x.png", strings.NewReader("x"), ""); !isRefused(err) {
			t.Fatalf("attachment err = %v", err)
		}
	})
}

// TestBlockDirectionIsOneWay proves A blocking B does not restrict A acting
// on B's content (PLAN-V2 §M.6.1 "each direction").
func TestBlockDirectionIsOneWay(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	mod.Block("dave", "bob") // dave blocked bob

	// bob owns bob/app; dave may still open/comment there.
	if _, _, err := s.CreateIssue(reqCtx(), "bob", "app", auth.Principal{Name: "dave"}, "hi", "", nil); err != nil {
		t.Fatalf("dave in bob's repo: %v", err)
	}
	// And bob's own repo reacts normally for bob.
	if _, _, err := s.CreateIssue(reqCtx(), "bob", "app", auth.Principal{Name: "bob"}, "x", "", nil); err != nil {
		t.Fatalf("bob in own repo: %v", err)
	}
}

func TestBlockExemptsHostAdminAndNilGate(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	mod.Block("dave", "admin@example.com")

	if _, _, err := s.CreateIssue(reqCtx(), "dave", "repo", adminP, "op", "", nil); err != nil {
		t.Fatalf("host admin refused: %v", err)
	}
	s.Moderation = nil
	if _, _, err := s.CreateIssue(reqCtx(), "dave", "repo", bobP, "x", "", nil); err != nil {
		t.Fatalf("nil gate must allow: %v", err)
	}
}

func TestBlockGateErrorFailsClosed(t *testing.T) {
	s := testService(newFakeRoles())
	mod := newFakeModeration()
	mod.err = errors.New("store down")
	s.Moderation = mod
	_, _, err := s.CreateIssue(reqCtx(), "dave", "repo", bobP, "x", "", nil)
	if err == nil {
		t.Fatal("gate error must refuse the write")
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("gate outage must be 503-class, not a 403: %v", err)
	}
}

// isRefused asserts ErrForbidden AND the neutral text: no "block", no
// blocker name, no difference from a permissions refusal.
func isRefused(err error) bool {
	return errors.Is(err, ErrForbidden) && strings.Contains(err.Error(), identity.InteractionRefused)
}

func assertRefused(t *testing.T, err error) {
	t.Helper()
	if !isRefused(err) {
		t.Fatalf("err = %v, want ErrForbidden with the neutral text", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "block") {
		t.Fatalf("refusal leaks the block: %q", err.Error())
	}
}

// --- M2 projection + lock ------------------------------------------------------

// TestHiddenBodyNeverReachesClient is the §M.6.2 projection matrix for issues:
// the normal read has no body, the moderator read has it, and the JSON lane
// agrees with the page (both call this one verb).
func TestHiddenBodyNeverReachesClient(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	roles.grant("dave", "repo", "mod", "triage")
	roles.grant("dave", "repo", "reader", "read")

	th, ev, err := s.CreateIssue(reqCtx(), "dave", "repo", auth.Principal{Name: "dave"}, "title", "secret body", nil)
	if err != nil {
		t.Fatal(err)
	}
	mod.hide(hiddenTarget(th.Num, ev.Seq))

	reader := auth.Principal{Name: "reader"}
	view, err := s.GetThread(reqCtx(), "dave", "repo", th.Num, reader, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := view.Events[0]
	if got.Body != nil || got.Hidden == nil || got.Hidden.Reason != "spam" {
		t.Fatalf("reader event = %+v (body %v)", got, got.Body)
	}
	if view.ModerationRevision == 0 {
		t.Fatal("moderation revision not exposed for the ETag")
	}

	// The JSON lane reads the same verb: the body is absent from the payload.
	h := testHandler(s, reader)
	rec := doReq(h, http.MethodGet, "/dave/repo/api/issues/"+strconv.Itoa(th.Num), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON GET = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret body") {
		t.Fatalf("hidden body reached the JSON lane: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"hidden"`) {
		t.Fatalf("hidden marker missing: %s", rec.Body.String())
	}
	// ETag material includes the moderation revision.
	if et := rec.Header().Get("ETag"); !strings.Contains(et, "-m") {
		t.Fatalf("etag = %q", et)
	}

	// The moderator gets the original alongside the marker.
	mv, err := s.GetThread(reqCtx(), "dave", "repo", th.Num, auth.Principal{Name: "mod"}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if mv.Events[0].Body == nil || *mv.Events[0].Body != "secret body" || mv.Events[0].Hidden == nil {
		t.Fatalf("moderator event = %+v", mv.Events[0])
	}
	// A gate error fails the read closed rather than serving the body.
	mod.err = errors.New("moderation down")
	if _, err := s.GetThread(reqCtx(), "dave", "repo", th.Num, reader, 0, 50); err == nil {
		t.Fatal("gate error was served as visible content")
	}
}

func TestModerationLockRefusesEveryone(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	roles.grant("dave", "repo", "bob", "write")
	th, ev, err := s.CreateIssue(reqCtx(), "dave", "repo", auth.Principal{Name: "dave"}, "title", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	mod.lock(th.Num, "flame war")

	bob := auth.Principal{Name: "bob"}
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "hello"); !errors.Is(err, ErrLocked) {
		t.Fatalf("comment on locked thread = %v", err)
	}
	if _, _, _, err := s.AddReaction(reqCtx(), "dave", "repo", th.Num, ev.Seq, bob, "+1"); !errors.Is(err, ErrLocked) {
		t.Fatalf("reaction on locked thread = %v", err)
	}
	// Even a host admin cannot comment while the moderator lock is on (the
	// v1 no-override decision), but reading and metadata stay available.
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, auth.Principal{Name: "root", Admin: true}, "hello"); !errors.Is(err, ErrLocked) {
		t.Fatalf("admin comment = %v", err)
	}
	if _, err := s.GetThread(reqCtx(), "dave", "repo", th.Num, bob, 0, 50); err != nil {
		t.Fatalf("read while locked = %v", err)
	}
	title := "renamed while locked"
	if _, err := s.PatchIssue(reqCtx(), "dave", "repo", th.Num, auth.Principal{Name: "dave"}, IssuePatch{Title: &title}); err != nil {
		t.Fatalf("metadata while locked = %v", err)
	}
	// Unlock restores comments.
	delete(mod.locks, th.Num)
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "hello"); err != nil {
		t.Fatalf("comment after unlock = %v", err)
	}
}

func TestBlockAndLockCompose(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	roles.grant("dave", "repo", "bob", "write")
	th, _, err := s.CreateIssue(reqCtx(), "dave", "repo", auth.Principal{Name: "dave"}, "title", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	mod.Block("dave", "bob")
	mod.lock(th.Num, "cooldown")
	bob := auth.Principal{Name: "bob"}

	// Both arms set: the block is refused first (it is the outer interaction
	// gate), and the thread stays locked.
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("blocked+locked = %v", err)
	}
	// Lift the block: the lock still refuses.
	mod.pairs = map[string]bool{}
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "x"); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked after unblock = %v", err)
	}
	// Lift the lock: comments work again.
	delete(mod.locks, th.Num)
	if _, err := s.AddComment(reqCtx(), "dave", "repo", th.Num, bob, "x"); err != nil {
		t.Fatalf("unblocked+unlocked = %v", err)
	}
}
