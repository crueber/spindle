package pulls

// lockmode_test.go — the §10.1 lock-mode half of the M2 gate as pulls
// enforces it on conversation writes: `all` refuses everyone; `collaborators`
// admits write+ (a host admin included) and still refuses read.

import (
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestPullCommentLockModes(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["dave@example.com"] = "write"
	e.roles.Roles["bob@example.com"] = "write"
	e.roles.Roles["carol@example.com"] = "read"
	e.seedRefs("dave/repo", map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
	})
	th, _, err := e.svc.OpenPR(ctx(), "dave", "repo", auth.Principal{Name: "dave@example.com"}, OpenInput{
		Title: "seed", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic",
	}, "corr")
	if err != nil {
		t.Fatal(err)
	}
	num := th.Num
	gate := newFakeBlockGate()
	gate.locks[num] = "flame war"
	gate.modes[num] = "all"
	e.svc.Moderation = gate

	for _, actor := range []auth.Principal{
		{Name: "bob@example.com"}, {Name: "carol@example.com"}, {Name: "root", Admin: true},
	} {
		if _, err := e.svc.AddComment(ctx(), "dave", "repo", num, actor, "x"); !errors.Is(err, ErrLocked) {
			t.Fatalf("all-mode comment by %s err = %v", actor.Name, err)
		}
	}
	gate.modes[num] = "collaborators"
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", num, auth.Principal{Name: "bob@example.com"}, "still here"); err != nil {
		t.Fatalf("writer comment under collaborators = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", num, auth.Principal{Name: "root", Admin: true}, "operator"); err != nil {
		t.Fatalf("admin comment under collaborators = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "dave", "repo", num, auth.Principal{Name: "carol@example.com"}, "read"); !errors.Is(err, ErrLocked) {
		t.Fatalf("read comment under collaborators err = %v", err)
	}
}
