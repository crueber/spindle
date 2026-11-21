package threads

import (
	"context"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// approvalEnv wires the gate: a fake evidence source, the gate on, and a
// maintainer principal the fake roles know.
type fakeApprovals struct {
	merged     map[string]bool // actor → has a merged PR
	maintainer map[string]bool // actor → holds maintain+
	err        error
	calls      int
}

func (f *fakeApprovals) MergedAuthor(_ context.Context, _, _, author string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.merged[author], nil
}

// Maintainer answers from the fixture set (a repo maintainer or instance admin).
func (f *fakeApprovals) Maintainer(_ context.Context, _, _, actor string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.maintainer[actor], nil
}

func approvalEnv(t *testing.T) (*testEnv, *fakeApprovals) {
	t.Helper()
	e := newTestEnv(t)
	ev := &fakeApprovals{merged: map[string]bool{}, maintainer: map[string]bool{"maintainer": true}}
	e.svc.Approvals = ev
	e.svc.ApproveFirstTimeForks = true
	e.roles.roles["maintainer"] = "maintain"
	return e, ev
}

// forkPR creates a fork pull_request run for the actor.
func forkPR(t *testing.T, e *testEnv, actor string) []*Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Fork: true, Actor: actor,
	})
	if err != nil {
		t.Fatalf("fork PR run: %v", err)
	}
	return runs
}

func maintainer() auth.Principal { return auth.Principal{Name: "maintainer"} }

// TestApprovalHoldsFirstTimeForkRun pins the gate: the run exists, its jobs and
// checks exist, but NOTHING is enqueued — a runner sees no claimable job.
func TestApprovalHoldsFirstTimeForkRun(t *testing.T) {
	e, _ := approvalEnv(t)
	runs := forkPR(t, e, "newcomer")
	if len(runs) != 1 || runs[0].State != RunPendingApproval {
		t.Fatalf("runs = %+v, want one pending_approval run", runs)
	}
	if got := countPrefix(t, e, QueuePrefix("o", "r")); got != 0 {
		t.Fatalf("queue objects = %d, want 0 (the gate IS the queue)", got)
	}
	runner := &RunnerDoc{ID: "rnr", Name: "r1", Labels: []string{"ubuntu-latest"}, State: RunnerActive}
	claim, err := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil {
		t.Fatalf("claimLoop: %v", err)
	}
	if claim != nil {
		t.Fatalf("a held run was claimable: %+v", claim)
	}
	// Its jobs and checks still exist (the page and the PR panel stay coherent).
	if jobs := mustJobs(t, e, runs[0].ID); len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	if len(e.checks.reports) == 0 || e.checks.reports[0].State != "pending" {
		t.Fatalf("checks = %+v, want pending mirrors", e.checks.reports)
	}
	// A fold (any unrelated transition) must not release it.
	if _, ferr := e.svc.foldRun(ctx(), "o", "r", runs[0].ID); ferr != nil {
		t.Fatal(ferr)
	}
	if r, _ := e.svc.loadRun(ctx(), "o", "r", runs[0].ID); r.State != RunPendingApproval {
		t.Fatalf("fold released the gate: %s", r.State)
	}
}

// TestApprovalSkipsTrustedAndKnownContributors pins the exemptions: a
// maintainer's run, a merged contributor's run and a same-repository PR run all
// queue immediately.
func TestApprovalSkipsTrustedAndKnownContributors(t *testing.T) {
	e, ev := approvalEnv(t)
	ev.merged["known"] = true
	cases := []struct {
		name  string
		actor string
		fork  bool
	}{
		{"maintainer", "maintainer", true},
		{"merged contributor", "known", true},
		{"same-repository PR", "newcomer", false},
	}
	for _, tc := range cases {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
			Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Fork: tc.fork, Actor: tc.actor,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(runs) != 1 || runs[0].State != RunQueued {
			t.Fatalf("%s: state = %+v, want queued", tc.name, runs)
		}
	}
	// Push runs are never gated.
	runs := e.mustCreate(t)
	if runs[0].State != RunQueued {
		t.Fatalf("push run state = %s", runs[0].State)
	}
}

// TestApprovalEventualEvidenceFailureHolds pins fail-closed: an unreadable
// evidence source holds the run instead of letting it execute.
func TestApprovalEventualEvidenceFailureHolds(t *testing.T) {
	e, ev := approvalEnv(t)
	ev.err = errors.New("store down")
	runs := forkPR(t, e, "newcomer")
	if runs[0].State != RunPendingApproval {
		t.Fatalf("state = %s, want pending_approval (fail closed)", runs[0].State)
	}
}

// TestApprovalApproveQueuesAndDenyCancels pins both decisions.
func TestApprovalApproveQueuesAndDenyCancels(t *testing.T) {
	e, _ := approvalEnv(t)
	held := forkPR(t, e, "newcomer")[0]
	denied := forkPR(t, e, "other-newcomer")[0]

	// A reader cannot decide; the service is the gate.
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader approve err = %v, want forbidden", err)
	}
	got, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, maintainer())
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.State != RunQueued || got.ApprovedBy != "maintainer" || got.ApprovedAt == nil {
		t.Fatalf("approved run = %+v", got)
	}
	// It is now claimable.
	runner := &RunnerDoc{ID: "rnr", Labels: []string{"ubuntu-latest"}, State: RunnerActive}
	claim, cerr := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if cerr != nil || claim == nil || claim.Run.ID != held.ID {
		t.Fatalf("claim after approve = %+v, %v", claim, cerr)
	}
	// A second approve is a conflict, never a second enqueue.
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, maintainer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("second approve err = %v, want conflict", err)
	}

	deniedRun, err := e.svc.DenyRun(ctx(), "o/r", denied.ID, maintainer())
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if deniedRun.State != RunCancelled || deniedRun.DeniedBy != "maintainer" {
		t.Fatalf("denied run = %+v", deniedRun)
	}
	for _, job := range mustJobs(t, e, denied.ID) {
		if job.State != JobCancelled {
			t.Fatalf("denied job %s = %s, want cancelled", job.ID, job.State)
		}
	}
	if _, err := e.svc.ApproveRun(ctx(), "o/r", denied.ID, maintainer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving a denied run = %v, want conflict", err)
	}
}

// TestApprovalRunViewReportsState pins the projection the API/page render.
func TestApprovalRunViewReportsState(t *testing.T) {
	e, _ := approvalEnv(t)
	held := forkPR(t, e, "newcomer")[0]
	view, err := e.svc.GetRun(ctx(), "o/r", held.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	av := Approval(view.Run)
	if !av.Pending {
		t.Fatalf("approval = %+v, want pending", av)
	}
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, maintainer()); err != nil {
		t.Fatal(err)
	}
	view, _ = e.svc.GetRun(ctx(), "o/r", held.ID, e.public())
	if av := Approval(view.Run); av.Pending || av.ApprovedBy != "maintainer" {
		t.Fatalf("approval = %+v after approve", av)
	}
}

// TestNeedsApprovalMaintainerArms pins the two maintainer branches the policy
// hook can take: a hook that says maintainer skips the gate, a hook error is
// fail-closed (the run is held), and a service with no hook at all falls back to
// the repo role ladder.
func TestNeedsApprovalMaintainerArms(t *testing.T) {
	e := newTestEnv(t)
	e.svc.ApproveFirstTimeForks = true
	opts := CreateOpts{Fork: true}
	// No hook: the role ladder decides (a maintainer is exempt, a newcomer is
	// gated).
	e.roles.roles["maint"] = "maintain"
	if e.svc.needsApproval(ctx(), "o", "r", "maint", opts, EventPullReq) {
		t.Fatal("a maintain-by-role actor was gated without a hook")
	}
	if !e.svc.needsApproval(ctx(), "o", "r", "newcomer", opts, EventPullReq) {
		t.Fatal("a newcomer was not gated without a hook")
	}
	// A hook that errors on the maintainer question holds the run.
	e.svc.Approvals = &fakeApprovals{err: errors.New("identity down")}
	if !e.svc.needsApproval(ctx(), "o", "r", "maint", opts, EventPullReq) {
		t.Fatal("a hook error did not hold the run")
	}
	// A hook that answers maintainer skips the gate.
	e.svc.Approvals = &fakeApprovals{merged: map[string]bool{}, maintainer: map[string]bool{"maint": true}}
	if e.svc.needsApproval(ctx(), "o", "r", "maint", opts, EventPullReq) {
		t.Fatal("the hook's maintainer answer was ignored")
	}
}
