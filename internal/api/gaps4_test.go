package api

// gaps4_test.go: final cheap arms — task-subsystem failures, policy PUT
// guards, dispatch short-circuits.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// failingTasks fails one Tasks method on demand.
type failingTasks struct {
	fakeTasks
	fail map[string]error
}

func (f *failingTasks) Begin(ctx context0, id gitRepoId2, op string, params map[string]string) (TaskStream, error) {
	if err := f.fail["Begin"]; err != nil {
		return TaskStream{}, err
	}
	return f.fakeTasks.Begin(ctx, id, op, params)
}

func (f *failingTasks) Attach(ctx context0, id gitRepoId2, taskID string) (TaskStream, bool, error) {
	if err := f.fail["Attach"]; err != nil {
		return TaskStream{}, false, err
	}
	return f.fakeTasks.Attach(ctx, id, taskID)
}

func (f *failingTasks) Get(ctx context0, id gitRepoId2, taskID string) (TaskRecord, bool, error) {
	if err := f.fail["Get"]; err != nil {
		return TaskRecord{}, false, err
	}
	return f.fakeTasks.Get(ctx, id, taskID)
}

// aliases keep override signatures compact.
type context0 = context.Context
type gitRepoId2 = git.RepoId

func TestTaskFailureArms(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	ft := &failingTasks{fail: map[string]error{}}
	ft.running = f.tasks.running
	ft.recent = f.tasks.recent
	ft.records = f.tasks.records
	ft.streams = f.tasks.streams
	f.env.Tasks = ft

	ft.fail["Begin"] = errBoom
	if w := f.req("POST", "/demo/walgit/api/ops/sync"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Begin fail = %d", w.Code)
	}
	ft.fail["Begin"] = nil

	ft.fail["Attach"] = errBoom
	hdr := map[string]string{"Accept": "text/event-stream"}
	p := &auth.Principal{Name: "jane"}
	if w := f.do("GET", "/demo/walgit/api/tasks/t1", nil, hdr, p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Attach fail = %d", w.Code)
	}
	ft.fail["Attach"] = nil

	ft.fail["Get"] = errBoom
	if w := f.req("GET", "/demo/walgit/api/tasks/t1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Get fail = %d", w.Code)
	}
}

func TestPolicyPutGuards(t *testing.T) {
	f := newFixture(t)
	// oversize body → 413
	big := strings.Repeat("x", 1<<20+2)
	if w := f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize policy = %d", w.Code)
	}
	// corrupt stored doc → PUT fails closed with 503
	putPolicyRaw(t, f, `{"version":1,"rules":[{"name":"x","effect":{"protect":{"restricts":["bogus"]}}}]}`)
	if w := f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(allowAllPolicy)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt stored PUT = %d", w.Code)
	}
}

func TestDispatchShortCircuits(t *testing.T) {
	f := newFixture(t)
	// single raw segment → not found
	if w := f.req("GET", "/justjunk"); w.Code != http.StatusNotFound {
		t.Fatalf("short path = %d", w.Code)
	}
	// HEAD on a GET route → 405 (method enforced by the dispatcher)
	if w := f.req("HEAD", "/demo/walgit/api"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD = %d", w.Code)
	}
}

// TestTaskAcrossLaneAfterMove pins the repo-transfer attach path (PLAN-V2
// §T.1): a task's record names its own repo, and after the move the lane the
// client started on no longer resolves — so the read gate follows the record's
// repo (or the move destination it carries), never the lane path.
func TestTaskAcrossLaneAfterMove(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	ok := true
	// The record is keyed by the SOURCE (gone after the move) and names the
	// destination in params["to"].
	f.tasks.records["m1"] = TaskRecord{ID: "m1", Kind: "repo-transfer", Repo: "demo/moved",
		OK: &ok, Finished: "2026-11-21T00:00:01Z", LogTail: []string{},
		Params: map[string]string{"from": "demo/moved", "to": "demo/walgit"}}
	// A gate that refuses the source but admits the destination: the pair is
	// readable through the destination (the access document moved with it).
	g := &laneGate{allow: "demo/walgit", deny: "demo/moved"}
	f.env.Access = g
	w := f.do("GET", "/demo/walgit/api/tasks/m1", nil, nil, readP())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"repo-transfer"`) {
		t.Fatalf("follow via destination = %d (%s)", w.Code, w.Body.String())
	}
	// A gate that refuses both is a real 403 (never a silent 200).
	f.env.Access = &laneGate{deny: "demo/moved", deny2: "demo/walgit"}
	if w := f.do("GET", "/demo/walgit/api/tasks/m1", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("refused both = %d (%s), want 403", w.Code, w.Body.String())
	}
	// A task with no destination is gated on its own repo alone.
	f.tasks.records["m2"] = TaskRecord{ID: "m2", Kind: "fsck", Repo: "demo/walgit", OK: &ok}
	f.env.Access = &laneGate{allow: "demo/walgit"}
	if w := f.do("GET", "/demo/walgit/api/tasks/m2", nil, nil, readP()); w.Code != 200 {
		t.Fatalf("own repo = %d (%s)", w.Code, w.Body.String())
	}
	// A record naming a malformed repo is refused, not panicked on.
	f.tasks.records["m3"] = TaskRecord{ID: "m3", Kind: "fsck", Repo: "no-slash"}
	if w := f.do("GET", "/demo/walgit/api/tasks/m3", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("malformed repo = %d (%s), want 403", w.Code, w.Body.String())
	}
}

// laneGate allows exactly the named repos (and denies everything else).
type laneGate struct {
	allow, deny, deny2 string
}

func (g *laneGate) CheckRead(_ context.Context, owner, repo string, _ auth.Principal) *auth.AuthError {
	id := owner + "/" + repo
	switch id {
	case g.allow:
		return nil
	case g.deny, g.deny2:
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}
