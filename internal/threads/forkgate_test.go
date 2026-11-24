package threads

// Fork-run gating on instance runners (docs/threads.md §8.7): a fork pull
// request's code is only offered to a runner that opted in, the per-repository
// CAS stays the authority, and a run with no eligible runner stays queued.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

const forkGateWorkflow = `name: CI
on: [pull_request]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// forkGateEnv creates one run: a fork PR (integration branch commits, base-tip
// workflow) or a same-repository PR, per fork.
func forkGateEnv(t *testing.T, fork bool) (*testEnv, *Run, *Job) {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", forkGateWorkflow)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened", Branch: "main", Ref: "refs/pull/1/head",
		BaseBranch: "main", BaseCommit: testSHA, Fork: fork, Actor: "guest",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun = %d runs, %v", len(runs), err)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d (%v)", len(jobs), err)
	}
	return e, runs[0], jobs[0]
}

// declaredRunner registers a runner through the service and returns its
// authenticated protocol context (the registry record is the claim authority).
func declaredRunner(t *testing.T, e *testEnv, labels []string, allowForkRuns bool) *runnerAuth {
	t.Helper()
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("r1"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{
		Token: mint.Token, Labels: labels, Capacity: 1, AllowForkRuns: allowForkRuns,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	doc, _, err := e.svc.loadRunner(ctx(), mint.ID)
	if err != nil || doc == nil {
		t.Fatalf("loadRunner = %+v (%v)", doc, err)
	}
	return &runnerAuth{Doc: doc}
}

// TestForkRunIsNotOfferedToADefaultRunner pins the gate at both layers: the
// global hint skips the row and the per-repository claim refuses it, so the job
// stays queued (never failed, never executed).
func TestForkRunIsNotOfferedToADefaultRunner(t *testing.T) {
	e, run, _ := forkGateEnv(t, true)
	runner := declaredRunner(t, e, []string{"linux"}, false)

	// The hint carries the fork marker.
	idx, _, err := e.svc.loadQueueIndex(ctx())
	if err != nil || idx == nil || len(idx.Queued) != 1 || !idx.Queued[0].Fork {
		t.Fatalf("queue index = %+v (%v), want one fork row", idx, err)
	}
	// The global claim refuses it.
	if claim, _, _, cerr := e.svc.claimGlobal(ctx(), runner.Doc, runner.Doc.Labels, nil); cerr != nil || claim != nil {
		t.Fatalf("global claim = %+v (%v), want none", claim, cerr)
	}
	// The long-poll hands out nothing and the job is still queued.
	for i := 0; i < 3; i++ {
		got, ferr := e.svc.FetchJobFrom(ctx(), runner, time.Millisecond, "http://127.0.0.1:8080")
		if ferr != nil || got != nil {
			t.Fatalf("fetch = %+v (%v), want nothing", got, ferr)
		}
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d (%v)", len(jobs), err)
	}
	if jobs[0].State != JobQueued {
		t.Fatalf("job = %s, want queued (a fork job waits, it does not fail)", jobs[0].State)
	}
	head, _, herr := e.svc.loadHead(ctx(), "o", "r")
	if herr != nil || head == nil || len(head.Pending) != 1 {
		t.Fatalf("head = %+v (%v), want the fork job still pending", head, herr)
	}
	entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	if eerr != nil || entry == nil || entry.State != QueueQueued || entry.Runner != "" || !entry.Fork {
		t.Fatalf("entry = %+v (%v), want a queued, unclaimed fork entry", entry, eerr)
	}
}

// TestForkRunIsClaimedAfterTheOptIn pins the flag as the switch, through the
// real declare path (a restart re-declares).
func TestForkRunIsClaimedAfterTheOptIn(t *testing.T) {
	e, run, _ := forkGateEnv(t, true)
	runner := declaredRunner(t, e, []string{"linux"}, false)
	if got, err := e.svc.FetchJobFrom(ctx(), runner, time.Millisecond, "http://127.0.0.1:8080"); err != nil || got != nil {
		t.Fatalf("before opt-in: fetch = %+v (%v)", got, err)
	}
	view, err := e.svc.Declare(ctx(), runner, DeclareInput{Version: "test", Labels: []string{"linux"}, Capacity: 1, AllowForkRuns: true})
	if err != nil || !view.AllowForkRuns {
		t.Fatalf("declare = %+v (%v)", view, err)
	}
	got, err := e.svc.FetchJobFrom(ctx(), runner, time.Millisecond, "http://127.0.0.1:8080")
	if err != nil || got == nil || got.RunID != run.ID {
		t.Fatalf("after opt-in: fetch = %+v (%v)", got, err)
	}
	if got.Trusted {
		t.Fatal("the fork payload must stay untrusted")
	}
}

// TestSameRepoRunIsUnaffected pins that the gate is fork-only.
func TestSameRepoRunIsUnaffected(t *testing.T) {
	e, run, _ := forkGateEnv(t, false)
	runner := declaredRunner(t, e, []string{"linux"}, false)
	got, err := e.svc.FetchJobFrom(ctx(), runner, time.Millisecond, "http://127.0.0.1:8080")
	if err != nil || got == nil || got.RunID != run.ID || !got.Trusted {
		t.Fatalf("same-repo fetch = %+v (%v)", got, err)
	}
}

// TestForkOptInIsRecordedAndDeclared pins the wire field: register stores it,
// the fleet exposes it, and declare is idempotent (a restart without the flag
// turns it off — fail closed).
func TestForkOptInIsRecordedAndDeclared(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("r1"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"token": mint.Token, "name": "r1", "labels": []string{"linux"}, "capacity": 1, "allow_fork_runs": true,
	})
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", string(body), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	var reg RegisterResult
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || !reg.AllowForkRuns {
		t.Fatalf("register answer = %+v (%v)", reg, err)
	}
	rec = doAuth(h, http.MethodGet, "/api/v1/admin/runners", "", "")
	var fleet struct {
		Runners []RunnerView `json:"runners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fleet); err != nil || len(fleet.Runners) != 1 || !fleet.Runners[0].AllowForkRuns {
		t.Fatalf("fleet = %+v (%v)", fleet, err)
	}
	// Declare without the flag turns it off.
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{"version":"test","labels":["linux"]}`, reg.Token)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"allow_fork_runs":true`) {
		t.Fatalf("declare = %d: %s", rec.Code, rec.Body.String())
	}
	doc, _, err := e.svc.loadRunner(ctx(), reg.RunnerID)
	if err != nil || doc == nil || doc.AllowForkRuns {
		t.Fatalf("stored runner = %+v (%v), want the opt-in off", doc, err)
	}
}
