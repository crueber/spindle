package issues

// lockmode_test.go — the §10.1 lock-mode half of the M2 gate as this package
// enforces it: `all` refuses everyone (host admins included, unchanged), and
// `collaborators` admits role ≥ write — which is where a host admin resolves.

import (
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestCommentLockModes(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mod := newFakeModeration()
	s.Moderation = mod
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	f := newFakeRoles()
	f.grant("acme", "repo", "bob@example.com", "write") // writer
	f.grant("acme", "repo", "carol@example.com", "read")
	s.Roles = f
	_ = roles

	// `all`: nobody may comment — a writer, a read collaborator and a host
	// admin all get ErrLocked.
	mod.lockMode(th.Num, "all", "flame war")
	for _, actor := range []auth.Principal{
		{Name: "bob@example.com"},
		{Name: "carol@example.com"},
		adminP,
	} {
		if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, actor, "hi"); !errors.Is(err, ErrLocked) {
			t.Fatalf("all-mode comment by %s err = %v", actor.Name, err)
		}
	}

	// `collaborators`: a writer and a host admin may comment; a read
	// collaborator is still refused.
	mod.lockMode(th.Num, "collaborators", "keep it focused")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, auth.Principal{Name: "bob@example.com"}, "still here"); err != nil {
		t.Fatalf("writer comment under collaborators = %v", err)
	}
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, adminP, "operator note"); err != nil {
		t.Fatalf("admin comment under collaborators = %v", err)
	}
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, auth.Principal{Name: "carol@example.com"}, "read only"); !errors.Is(err, ErrLocked) {
		t.Fatalf("read comment under collaborators err = %v", err)
	}
	// Reactions follow the same gate.
	if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, auth.Principal{Name: "carol@example.com"}, "heart"); !errors.Is(err, ErrLocked) {
		t.Fatalf("read reaction under collaborators err = %v", err)
	}
	if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, auth.Principal{Name: "bob@example.com"}, "heart"); err != nil {
		t.Fatalf("writer reaction under collaborators = %v", err)
	}

	// Unlock restores everyone.
	delete(mod.locks, th.Num)
	delete(mod.modes, th.Num)
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, auth.Principal{Name: "carol@example.com"}, "back"); err != nil {
		t.Fatalf("comment after unlock = %v", err)
	}

	// The thread view carries the mode for the page and the JSON twin.
	mod.lockMode(th.Num, "collaborators", "narrowed")
	view, err := s.GetThread(reqCtx(), "acme", "repo", th.Num, janeP, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Locked || view.LockMode != "collaborators" || view.LockReason != "narrowed" {
		t.Fatalf("view = locked:%v mode:%q reason:%q", view.Locked, view.LockMode, view.LockReason)
	}
}

func TestCommentLockModeGateErrorFailsClosed(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	mod := newFakeModeration()
	mod.err = errors.New("moderation down")
	s.Moderation = mod
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, auth.Principal{Name: "bob", Write: true}, "hi"); err == nil {
		t.Fatal("a gate error must refuse the write, not allow it")
	}
}
