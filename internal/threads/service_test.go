package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// Service tests: run/job round-trip, no-workflow no-op, trust-class selection,
// the job→check mirror + merge-gate integration, and CAS contention.

func TestCreateRunRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	runs := e.mustCreate(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Repo != "o/r" || run.Commit != testSHA || run.Event != EventPush || run.State != RunQueued {
		t.Fatalf("run = %+v", run)
	}
	if run.Workflow != "CI" || run.WorkflowPath != ".spindle/workflows/ci.yml" || run.WorkflowCommit != testSHA {
		t.Fatalf("workflow = %+v", run)
	}
	if !run.Trusted {
		t.Fatal("push run must be trusted")
	}
	if run.Branch != "main" || run.Ref != "refs/heads/main" || run.Actor != "writer" || run.Attempt != 1 {
		t.Fatalf("trigger = %+v", run)
	}
	if !runIDRe.MatchString(run.ID) || run.Version != 1 {
		t.Fatalf("id/version = %q/%d", run.ID, run.Version)
	}

	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if view.Run.ID != run.ID || len(view.Jobs) != 2 {
		t.Fatalf("view = %+v", view)
	}
	jobs := view.Jobs
	if jobs[0].ID != "j-build" || jobs[0].Name != "build" || jobs[0].RunsOn != "linux" {
		t.Fatalf("job[0] = %+v", jobs[0])
	}
	if jobs[0].Context != "threads/ci/build" || jobs[1].Context != "threads/ci/test" {
		t.Fatalf("contexts = %q, %q", jobs[0].Context, jobs[1].Context)
	}
	if jobs[0].Steps[0].Run != "echo build" || len(jobs[1].Steps) != 1 {
		t.Fatalf("steps = %+v", jobs)
	}
	if len(jobs[1].Needs) != 1 || jobs[1].Needs[0] != "build" {
		t.Fatalf("needs = %v", jobs[1].Needs)
	}
	if !jobs[0].Trusted || jobs[0].State != JobQueued {
		t.Fatalf("job trust/state = %+v", jobs[0])
	}
}

func TestCreateRunNoWorkflowsIsSilentNoop(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{} // no workflow files
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want 0", len(runs))
	}
	ids, err := e.svc.runIDs(ctx(), "o", "r")
	if err != nil || len(ids) != 0 {
		t.Fatalf("no run may be written (ids=%v err=%v)", ids, err)
	}
}

func TestCreateRunNonMatchingTriggerIsNoop(t *testing.T) {
	e := newTestEnv(t)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "dev"})
	if err != nil || len(runs) != 0 {
		t.Fatalf("dev push = %d runs, err %v (want 0)", len(runs), err)
	}
}

func TestCreateRunMultipleWorkflowsCreateMultipleRuns(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/docs.yml", secondWorkflow)
	runs := e.mustCreate(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	names := []string{runs[0].Workflow, runs[1].Workflow}
	if names[0] != "CI" || names[1] != "Docs" {
		t.Fatalf("workflows = %v", names)
	}
	// Two runs, one run header each; the second workflow's job uses
	// runs-on linux.
	view, err := e.svc.GetRun(ctx(), "o/r", runs[1].ID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(view.Jobs) != 1 || view.Jobs[0].RunsOn != "linux" {
		t.Fatalf("docs run jobs = %+v", view.Jobs)
	}
}

func TestCreateRunValidation(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name   string
		repo   string
		commit string
		event  string
		opts   CreateOpts
		want   error
	}{
		{"bad repo", "not-a-repo", testSHA, EventPush, CreateOpts{}, ErrInvalid},
		{"bad sha", "o/r", "nope", EventPush, CreateOpts{}, ErrInvalid},
		{"bad event", "o/r", testSHA, "bogus", CreateOpts{}, ErrInvalid},
		{"fork without base", "o/r", testSHA, EventPullReq, CreateOpts{Action: "opened", Fork: true}, ErrInvalid},
	}
	for _, tc := range cases {
		_, err := e.svc.CreateRun(ctx(), tc.repo, tc.commit, tc.event, tc.opts)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A nil git reader fails closed.
	svc := New(e.store, e.roles)
	if _, err := svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil git err = %v", err)
	}
}

// TestTrustClassSelection pins 15 §2/§5: same-repo push/PR read the head
// commit's workflow; a fork PR reads the BASE branch's file and is untrusted.
func TestTrustClassSelection(t *testing.T) {
	e := newTestEnv(t)
	// The base branch carries a DIFFERENT workflow (Docs), so the selected
	// revision is observable.
	e.git.add(baseSHA, ".spindle/workflows/base.yml", baseWorkflow)
	e.git.refs["refs/heads/main"] = baseSHA

	// Same-repository PR: head file, trusted.
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{Action: "opened", Branch: "main", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("same-repo PR: %v", err)
	}
	if len(runs) != 1 || runs[0].Workflow != "CI" || !runs[0].Trusted || runs[0].WorkflowCommit != testSHA {
		t.Fatalf("same-repo PR run = %+v", runs)
	}

	// Fork PR: base-branch file, untrusted, checks still report the head.
	runs, err = e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened",
		Branch: "main", BaseBranch: "main", Fork: true,
	})
	if err != nil {
		t.Fatalf("fork PR: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("fork PR runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Trusted {
		t.Fatal("fork PR run must be untrusted")
	}
	if run.Workflow != "Base CI" || run.WorkflowPath != ".spindle/workflows/base.yml" {
		t.Fatalf("fork PR read %q (%s), want the base branch's workflow", run.Workflow, run.WorkflowPath)
	}
	if run.WorkflowCommit != baseSHA {
		t.Fatalf("workflow commit = %q, want %q", run.WorkflowCommit, baseSHA)
	}
	if run.Commit != testSHA {
		t.Fatalf("run commit = %q (checks must target the head)", run.Commit)
	}
	if len(e.checks.reports) == 0 || e.checks.reports[len(e.checks.reports)-1].SHA != testSHA {
		t.Fatalf("check reports must target the head sha: %+v", e.checks.reports)
	}

	// An explicit BaseCommit wins over the branch resolution.
	e.git.refs["refs/heads/main"] = testSHA
	runs, err = e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened",
		Branch: "main", BaseBranch: "main", BaseCommit: baseSHA, Fork: true,
	})
	if err != nil || len(runs) != 1 || runs[0].WorkflowCommit != baseSHA {
		t.Fatalf("explicit base commit: %v %+v", err, runs)
	}

	// A fork PR whose base branch cannot resolve fails visibly.
	e.git.refs = map[string]string{}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{Action: "opened", BaseBranch: "gone", Fork: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown base err = %v", err)
	}
}

func TestCreateRunMalformedWorkflowWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/broken.yml", "on: [\n")
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("one broken file must not fail the call: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a malformed workflow must write no run: %+v", runs)
	}
	ids, _ := e.svc.runIDs(ctx(), "o", "r")
	if len(ids) != 0 {
		t.Fatalf("a malformed workflow must write no run (ids=%v)", ids)
	}
	if len(e.checks.reports) != 0 {
		t.Fatalf("a malformed workflow must write no check: %+v", e.checks.reports)
	}
}

// TestCreateRunOneBadFileDoesNotDisableOthers pins per-file isolation (15
// §2.1): the good workflow still creates its run, and the broken file's error
// is visible through ListWorkflows.
func TestCreateRunOneBadFileDoesNotDisableOthers(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.add(testSHA, ".spindle/workflows/broken.yml", "name: broken\non: [\n")
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 1 || runs[0].Workflow != "CI" {
		t.Fatalf("the good workflow must still run: %+v", runs)
	}
	list, err := e.svc.ListWorkflows(ctx(), "o/r", testSHA, e.public())
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(list.Workflows) != 2 {
		t.Fatalf("workflows = %+v", list.Workflows)
	}
	var broken *WorkflowState
	for _, wf := range list.Workflows {
		if wf.Path == ".spindle/workflows/broken.yml" {
			broken = wf
		}
	}
	if broken == nil || broken.Error == "" || broken.Name != "broken" {
		t.Fatalf("broken workflow state = %+v", broken)
	}
	if len(broken.Triggers) != 0 {
		t.Fatalf("broken workflow triggers = %v", broken.Triggers)
	}
}

func TestCreateRunChecksMirror(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	if len(e.checks.reports) != 2 {
		t.Fatalf("check reports = %d, want 2", len(e.checks.reports))
	}
	first := e.checks.reports[0]
	if first.Context != "threads/ci/build" || first.State != "pending" || first.SHA != testSHA {
		t.Fatalf("check = %+v", first)
	}
	if !strings.Contains(first.Description, "CI / build") || !strings.Contains(first.Description, run.ID) {
		t.Fatalf("description = %q", first.Description)
	}
	// A failing sink surfaces as unavailable and the run record still
	// exists (the header is the commit point).
	e2 := newTestEnv(t)
	e2.checks.err = errors.New("checks down")
	if _, err := e2.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("sink failure err = %v", err)
	}
	if ids, _ := e2.svc.runIDs(ctx(), "o", "r"); len(ids) != 1 {
		t.Fatalf("the run header must exist after a mirror failure: %v", ids)
	}
}

func TestUpdateJobFoldsRunAndMirrorsChecks(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	build, test := view.Jobs[0], view.Jobs[1]

	job, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, build.ID, JobUpdate{State: JobRunning})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if job.State != JobRunning || job.StartedAt == nil || job.CompletedAt != nil || job.Version != 2 {
		t.Fatalf("running job = %+v", job)
	}
	if got := lastReport(t, e); got.State != "in_progress" || got.StartedAt == nil {
		t.Fatalf("running mirror = %+v", got)
	}
	if v, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public()); v.Run.State != RunRunning || v.Run.StartedAt == nil {
		t.Fatalf("run after start = %+v", v.Run)
	}

	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, build.ID, JobUpdate{State: JobSuccess}); err != nil {
		t.Fatalf("build success: %v", err)
	}
	if v, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public()); v.Run.State != RunRunning {
		t.Fatalf("run with a queued job = %q, want running", v.Run.State)
	}
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, test.ID, JobUpdate{State: JobFailure}); err != nil {
		t.Fatalf("test failure: %v", err)
	}
	v, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if v.Run.State != RunFailure || v.Run.CompletedAt == nil {
		t.Fatalf("folded run = %+v", v.Run)
	}
	if got := lastReport(t, e); got.State != "failure" || got.CompletedAt == nil {
		t.Fatalf("failure mirror = %+v", got)
	}

	// A requeue clears the completion stamp and folds back to running.
	job, err = e.svc.UpdateJob(ctx(), "o/r", run.ID, test.ID, JobUpdate{State: JobQueued})
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if job.CompletedAt != nil {
		t.Fatalf("requeued job kept completed_at: %+v", job)
	}
	if got := lastReport(t, e); got.State != "pending" {
		t.Fatalf("requeued mirror = %+v", got)
	}
}

// TestFailingThreadsJobBlocksMerge is the 15 §2 checks integration proof: the
// job's context is visible to the existing require_checks merge gate.
func TestFailingThreadsJobBlocksMerge(t *testing.T) {
	sha := testSHA
	mem := store.NewMemory()
	ck := checksTestService(t, mem, sha)
	gitReader := newFakeGit()
	gitReader.add(sha, ".spindle/workflows/ci.yml", workflowYAML)
	svc := New(mem, &fakeRoles{public: true})
	svc.Git = gitReader
	svc.Checks = realCheckSink{svc: ck}
	svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	run, err := svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{Branch: "main"})
	if err != nil || len(run) != 1 {
		t.Fatalf("create: %v %d", err, len(run))
	}
	view, err := svc.GetRun(ctx(), "o/r", run[0].ID, authAnonymous())
	if err != nil {
		t.Fatal(err)
	}
	build, test := view.Jobs[0], view.Jobs[1]

	// Write a policy requiring both Threads contexts.
	if err := putPolicy(mem, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["threads/ci/build","threads/ci/test"]}}}]}`); err != nil {
		t.Fatal(err)
	}
	// Queued = pending → the gate must refuse.
	err = ck.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "threads/ci/build (pending)") {
		t.Fatalf("queued gate = %v", err)
	}
	// Both green → pass.
	for _, j := range []*Job{build, test} {
		if _, err := svc.UpdateJob(ctx(), "o/r", run[0].ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatalf("green %s: %v", j.ID, err)
		}
	}
	if err := ck.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("green gate: %v", err)
	}
	// One failing job → the gate refuses with the failure named.
	if _, err := svc.UpdateJob(ctx(), "o/r", run[0].ID, test.ID, JobUpdate{State: JobFailure}); err != nil {
		t.Fatal(err)
	}
	err = ck.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "threads/ci/test (failure)") {
		t.Fatalf("failing gate = %v", err)
	}
	if strings.Contains(err.Error(), "threads/ci/build") {
		t.Fatalf("green context reported as an offender: %v", err)
	}
}

func TestUpdateJobValidation(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	job := view.Jobs[0]
	cases := []struct {
		name    string
		runID   string
		jobID   string
		up      JobUpdate
		wantErr error
	}{
		{"bad state", run.ID, job.ID, JobUpdate{State: "exploded"}, ErrInvalid},
		{"bad run id", "nope", job.ID, JobUpdate{State: JobSuccess}, ErrNotFound},
		{"bad job id", run.ID, "nope", JobUpdate{State: JobSuccess}, ErrNotFound},
		{"unknown run", "0000000000000000001-abc123", job.ID, JobUpdate{State: JobSuccess}, ErrNotFound},
		{"unknown job", run.ID, "j-nope", JobUpdate{State: JobSuccess}, ErrNotFound},
	}
	for _, tc := range cases {
		_, err := e.svc.UpdateJob(ctx(), "o/r", tc.runID, tc.jobID, tc.up)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
	if _, err := e.svc.UpdateJob(ctx(), "bad-repo", run.ID, job.ID, JobUpdate{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo err = %v", err)
	}
}

// TestCASConflictIsBounded pins the CAS retry bound: a store that never lets
// the update through surfaces ErrConflict, not a hang or a silent success.
func TestCASConflictIsBounded(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	svc := *e.svc
	svc.Store = &alwaysConflict{Memory: e.store, key: JobKey("o", "r", run.ID, view.Jobs[0].ID)}
	_, err := svc.UpdateJob(ctx(), "o/r", run.ID, view.Jobs[0].ID, JobUpdate{State: JobSuccess})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v (want ErrConflict)", err)
	}
}

func TestFoldRunAlreadyFoldedIsNoop(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	for _, j := range view.Jobs {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := e.svc.loadRun(ctx(), "o", "r", run.ID)
	if _, err := e.svc.foldRun(ctx(), "o", "r", run.ID); err != nil {
		t.Fatalf("fold: %v", err)
	}
	after, _ := e.svc.loadRun(ctx(), "o", "r", run.ID)
	if after.Version != before.Version {
		t.Fatalf("a clean fold bumped the version %d→%d", before.Version, after.Version)
	}
}

func TestListRunsPaging(t *testing.T) {
	e := newTestEnv(t)
	var ids []string
	for i := 0; i < 5; i++ {
		runs := e.mustCreate(t)
		ids = append(ids, runs[0].ID)
	}
	page, err := e.svc.ListRuns(ctx(), "o/r", e.public(), "", 2)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 2 || !page.More {
		t.Fatalf("page = %+v", page)
	}
	// Newest first.
	if page.Runs[0].ID != ids[4] || page.Runs[1].ID != ids[3] {
		t.Fatalf("order = %s,%s want %s,%s", page.Runs[0].ID, page.Runs[1].ID, ids[4], ids[3])
	}
	next, err := e.svc.ListRuns(ctx(), "o/r", e.public(), page.Runs[1].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Runs) != 3 || next.More {
		t.Fatalf("next = %+v", next)
	}
	if next.Runs[0].ID != ids[2] {
		t.Fatalf("next first = %s, want %s", next.Runs[0].ID, ids[2])
	}
	// n bounds.
	if p, _ := e.svc.ListRuns(ctx(), "o/r", e.public(), "", 1000); len(p.Runs) != 5 {
		t.Fatalf("large n = %d", len(p.Runs))
	}
	if _, err := e.svc.ListRuns(ctx(), "o/r", e.public(), "bogus", 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor err = %v", err)
	}
	// Empty repo: [] not null.
	if p, _ := e.svc.ListRuns(ctx(), "o/r", e.public(), ids[0], 2); p.Runs == nil || len(p.Runs) != 0 {
		t.Fatalf("tail page = %+v", p)
	}
}

func TestGetRunPermissionAndShape(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	if _, err := e.svc.GetRun(ctx(), "o/r", "nope", e.public()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id err = %v", err)
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", "0000000000000000001-abc123", e.public()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run err = %v", err)
	}
	e.roles.public = false
	if _, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("private anonymous err = %v", err)
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.reader()); err != nil {
		t.Fatalf("private reader: %v", err)
	}
}

func TestDispatchRun(t *testing.T) {
	e := newTestEnv(t)
	runs, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, e.writer())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(runs) != 1 || runs[0].Event != EventDispatch || runs[0].Actor != "writer" || runs[0].Branch != "main" {
		t.Fatalf("dispatch runs = %+v", runs)
	}
	if !runs[0].Trusted {
		t.Fatal("a manual dispatch is a trusted run")
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "ci.yml", nil, e.writer()); err != nil {
		t.Fatalf("named dispatch: %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "ghost.yml", nil, e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown workflow err = %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "", "", nil, e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ref err = %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/gone", "", nil, e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read role dispatch err = %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, e.public()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous dispatch err = %v", err)
	}
}

func TestRunIDAndRecordHelpers(t *testing.T) {
	if !runIDRe.MatchString("0000000000000000001-abcdef") {
		t.Fatal("run id regexp rejects a minted id")
	}
	if jobIDRe.MatchString("j-") || !jobIDRe.MatchString("j-build") {
		t.Fatal("job id regexp")
	}
	if got := nonNilStr(nil); got == nil || len(got) != 0 {
		t.Fatalf("nonNilStr = %#v", got)
	}
	if got := nonNilSteps(nil); got == nil || len(got) != 0 {
		t.Fatalf("nonNilSteps = %#v", got)
	}
	if repoID("a", "b") != "a/b" {
		t.Fatal("repoID")
	}
	if got := statusFor(fmt.Errorf("%w: x", ErrUnprocessable)); got != 422 {
		t.Fatalf("statusFor unprocessable = %d", got)
	}
}

func TestTaskKindRegistration(t *testing.T) {
	ResetKindsForTest()
	t.Cleanup(ResetKindsForTest)
	RegisterKind(KindThreadsGC)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a duplicate kind must panic")
			}
		}()
		RegisterKind(KindThreadsGC)
	}()
	if n, err := New(nil, nil).GC(ctx(), "o", "r"); n != 0 || err != nil {
		t.Fatalf("GC stub = %d, %v", n, err)
	}
}

// TestRecordsAreBucketOnly pins the 15 §2 key layout and JSON shape.
func TestRecordsAreBucketOnly(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	raw, _, err := store.GetBytes(ctx(), e.store, RunKey("o", "r", run.ID), store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("run key: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("run json: %v", err)
	}
	for _, k := range []string{"id", "repo", "commit", "event", "workflow", "workflow_path", "workflow_commit", "trusted", "state", "created_at"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("run record misses %q", k)
		}
	}
	jraw, _, err := store.GetBytes(ctx(), e.store, JobKey("o", "r", run.ID, "j-build"), store.GetOptions{})
	if err != nil || jraw == nil {
		t.Fatalf("job key: %v", err)
	}
	var jrec map[string]any
	if err := json.Unmarshal(jraw, &jrec); err != nil {
		t.Fatalf("job json: %v", err)
	}
	for _, k := range []string{"id", "run_id", "name", "context", "runs_on", "steps", "needs", "attempt", "state"} {
		if _, ok := jrec[k]; !ok {
			t.Errorf("job record misses %q", k)
		}
	}
	if !strings.HasPrefix(JobKey("o", "r", run.ID, "j-build"), "repos/o/r/threads/runs/"+run.ID+"/jobs/") {
		t.Fatal("job key layout drifted")
	}
}

// --- helpers ---------------------------------------------------------------

func lastReport(t *testing.T, e *testEnv) checkReport {
	t.Helper()
	if len(e.checks.reports) == 0 {
		t.Fatal("no check reports")
	}
	return e.checks.reports[len(e.checks.reports)-1]
}

// alwaysConflict fails every Put for one key with a precondition error.
type alwaysConflict struct {
	*store.Memory
	key string
}

func (a *alwaysConflict) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == a.key {
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return a.Memory.Put(ctx, key, body, opts)
}
