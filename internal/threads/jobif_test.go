package threads

// Job-level `if:` and `continue-on-error` (docs/features/15_threads.md §4.1,
// round 4 2026-11-21). Both keys were parsed but inert; these tests pin the
// evaluated behaviour: a false condition terminalises the job as `skipped` with
// the reason on the record and no queue entry, a dependent job is decided once
// its dependencies are terminal (with needs.<job>.result available), and a
// tolerated failure leaves the job `failure` but the run green.

import (
	"strings"
	"testing"
)

const jobIfCreateYAML = `name: IfAtCreation
on:
  push:
    branches: [main]
jobs:
  skip-me:
    runs-on: linux
    if: false
    steps:
      - run: echo never
  run-me:
    runs-on: linux
    if: github.ref_name == 'main'
    steps:
      - run: echo hi
`

// jobRecord reads one job record.
func (e *testEnv) jobRecord(t *testing.T, runID, jobID string) *Job {
	t.Helper()
	job, err := e.svc.loadJobCached(ctx(), "o", "r", runID, jobID, nil)
	if err != nil || job == nil {
		t.Fatalf("job %s: %+v, %v", jobID, job, err)
	}
	return job
}

// pendingJobIDs lists the job ids the queue's head offers.
func (e *testEnv) pendingJobIDs(t *testing.T) map[string]bool {
	t.Helper()
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	out := map[string]bool{}
	for _, seq := range head.Pending {
		entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if eerr != nil || entry == nil {
			t.Fatalf("entry %s: %v", seq, eerr)
		}
		out[entry.JobID] = true
	}
	return out
}

// TestJobIfFalseAtCreationIsSkippedNeverQueued pins the earliest decision
// point: a job with no `needs:` whose `if:` is false is written `skipped` with
// the condition on the record and never reaches the queue, so no runner (or
// probe) ever sees it.
func TestJobIfFalseAtCreationIsSkippedNeverQueued(t *testing.T) {
	e, runID := needsEnv(t, jobIfCreateYAML)
	skipped := e.jobRecord(t, runID, "j-skip-me")
	if skipped.State != JobSkipped {
		t.Fatalf("if: false job state = %q, want skipped", skipped.State)
	}
	if skipped.Reason != "if: false" {
		t.Fatalf("skip reason = %q", skipped.Reason)
	}
	if skipped.CompletedAt == nil {
		t.Fatal("a skipped-at-creation job has no completion time")
	}
	if pending := e.pendingJobIDs(t); pending["j-skip-me"] {
		t.Fatalf("a skipped job was enqueued: %v", pending)
	}
	// The true condition ran, and it is the only claimable job.
	if got := e.claimOne(t, runID); got != "j-run-me" {
		t.Fatalf("first claim = %q, want j-run-me", got)
	}
	// A skipped job is a non-green, neutral check; nothing claims the run failed.
	last := e.checks.reports[len(e.checks.reports)-1]
	if last.State == "success" && last.Context == skipped.Context {
		t.Fatalf("skipped job mirrored as %q", last.State)
	}
	e.finish(t, runID, "j-run-me", JobSuccess)
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunSuccess {
		t.Fatalf("run state = %q, want success (skipped is neutral)", run.State)
	}
}

const jobIfNeedsYAML = `name: NeedsIf
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  on-success:
    runs-on: linux
    needs: [build]
    if: needs.build.result == 'success'
    steps:
      - run: echo ok
  on-failure:
    runs-on: linux
    needs: [build]
    if: needs.build.result == 'failure'
    steps:
      - run: echo recovered
  plain:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo default
`

// TestJobIfNeedsResultPicksTheBranch pins the dependency context: a job with a
// satisfied `needs` and a true condition runs, a true condition after a failed
// dependency ALSO runs (the point of `needs.<job>.result`), and the default
// (no condition) stays strict.
func TestJobIfNeedsResultPicksTheBranch(t *testing.T) {
	e, runID := needsEnv(t, jobIfNeedsYAML)
	e.finish(t, runID, "j-build", JobSuccess)

	if got := e.claimOne(t, runID); got != "j-on-success" {
		t.Fatalf("claim after a green build = %q, want j-on-success", got)
	}
	e.finish(t, runID, "j-on-success", JobSuccess)
	// `needs.build.result == 'failure'` is false: skipped, reason on the record.
	off := e.jobRecord(t, runID, "j-on-failure")
	if off.State != JobSkipped || !strings.Contains(off.Reason, "needs.build.result") {
		t.Fatalf("on-failure = %q (%q), want skipped with the condition", off.State, off.Reason)
	}
	if got := e.claimOne(t, runID); got != "j-plain" {
		t.Fatalf("claim = %q, want j-plain", got)
	}
}

// TestJobIfRunsAfterFailedDependency pins the opt-in: `if: needs.<job>.result
// == 'failure'` runs the recovery job even though the default would skip it.
func TestJobIfRunsAfterFailedDependency(t *testing.T) {
	e, runID := needsEnv(t, jobIfNeedsYAML)
	e.finish(t, runID, "j-build", JobFailure)

	if got := e.jobState(t, runID, "j-on-failure"); got != JobQueued {
		t.Fatalf("recovery job = %q, want queued (its condition holds)", got)
	}
	if got := e.claimOne(t, runID); got != "j-on-failure" {
		t.Fatalf("claim = %q, want j-on-failure", got)
	}
	// The sibling whose condition is false is skipped, and the plain dependent
	// keeps the strict default.
	if got := e.jobState(t, runID, "j-on-success"); got != JobSkipped {
		t.Fatalf("on-success = %q, want skipped", got)
	}
	plain := e.jobRecord(t, runID, "j-plain")
	if plain.State != JobSkipped || !strings.Contains(plain.Reason, "needs: build concluded failure") {
		t.Fatalf("plain = %q (%q), want skipped with the needs reason", plain.State, plain.Reason)
	}
}

const jobIfErrorYAML = `name: BadIf
on:
  push:
    branches: [main]
jobs:
  broken:
    runs-on: linux
    if: '!github.ref'
    steps:
      - run: echo hi
  fine:
    runs-on: linux
    steps:
      - run: echo hi
`

// TestJobIfUnsupportedExpressionFailsTheJob pins that a condition spindle cannot
// read is never a silent skip: the job fails with the parse error on its record
// (the same rule as a step `if:`).
func TestJobIfUnsupportedExpressionFailsTheJob(t *testing.T) {
	e, runID := needsEnv(t, jobIfErrorYAML)
	broken := e.jobRecord(t, runID, "j-broken")
	if broken.State != JobFailure {
		t.Fatalf("broken = %q, want failure", broken.State)
	}
	if !strings.Contains(broken.Reason, "if:") || !strings.Contains(broken.Reason, "not a boolean") {
		t.Fatalf("reason = %q, want the expression error", broken.Reason)
	}
	if e.pendingJobIDs(t)["j-broken"] {
		t.Fatal("a failed-at-creation job was enqueued")
	}
	e.finish(t, runID, "j-fine", JobSuccess)
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunFailure {
		t.Fatalf("run state = %q, want failure", run.State)
	}
}

const jobIfMatrixYAML = `name: MatrixIf
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    if: matrix.go == '1.27'
    steps:
      - run: echo ${{ matrix.go }}
`

// TestJobIfReadsTheMatrixCell pins that the job condition sees the expanded
// cell: exactly one cell of the product runs.
func TestJobIfReadsTheMatrixCell(t *testing.T) {
	e, runID := needsEnv(t, jobIfMatrixYAML)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want a 2-cell product", len(jobs))
	}
	states := map[string]string{}
	for _, j := range jobs {
		states[j.Spec.Matrix["go"]] = j.State
		if j.State == JobQueued && j.Spec.Matrix["go"] != "1.27" {
			t.Fatalf("cell %q is queued but its condition is false", j.Spec.Matrix["go"])
		}
	}
	if states["1.27"] != JobQueued || states["1.26"] != JobSkipped {
		t.Fatalf("cell states = %v", states)
	}
	if got := e.claimOne(t, runID); !strings.Contains(got, "1-27") && !strings.Contains(got, "1.27") {
		// The job id slugs the cell; assert by the spec instead of the id text.
		jobs, _ := e.svc.loadJobs(ctx(), "o", "r", runID)
		for _, j := range jobs {
			if j.ID == got && j.Spec.Matrix["go"] != "1.27" {
				t.Fatalf("claimed %q (cell %q)", got, j.Spec.Matrix["go"])
			}
		}
	}
}

// TestContinueOnErrorJobFailureLeavesTheRunGreen pins the tolerated-failure
// representation: the job record still reads `failure` and carries Tolerated,
// while the run conclusion follows the other jobs.
func TestContinueOnErrorJobFailureLeavesTheRunGreen(t *testing.T) {
	workflow := `name: Tolerated
on:
  push:
    branches: [main]
jobs:
  flaky:
    runs-on: linux
    continue-on-error: true
    steps:
      - run: exit 1
  gate:
    runs-on: linux
    steps:
      - run: echo ok
`
	e, runID := needsEnv(t, workflow)
	e.finish(t, runID, "j-flaky", JobFailure)
	flaky := e.jobRecord(t, runID, "j-flaky")
	if flaky.State != JobFailure {
		t.Fatalf("tolerated job state = %q, want failure (the record is honest)", flaky.State)
	}
	if !flaky.Tolerated {
		t.Fatal("the job record does not say the failure was tolerated")
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunRunning {
		t.Fatalf("run with a live job = %q, want running", run.State)
	}
	e.finish(t, runID, "j-gate", JobSuccess)
	run, _ = e.svc.loadRun(ctx(), "o", "r", runID)
	if run.State != RunSuccess {
		t.Fatalf("run state = %q, want success (the failure is tolerated)", run.State)
	}
	// The flag is derived from the final state, so a later success clears it
	// (a requeue/report cycle never leaves a stale "tolerated" marker).
	if _, uerr := e.svc.UpdateJob(ctx(), "o/r", runID, "j-flaky", JobUpdate{State: JobSuccess}); uerr != nil {
		t.Fatalf("UpdateJob success: %v", uerr)
	}
	if flakyAgain := e.jobRecord(t, runID, "j-flaky"); flakyAgain.Tolerated {
		t.Fatal("a succeeded job still says it was tolerated")
	}
}

// TestContinueOnErrorDoesNotFailFastMatrixSiblings pins that a tolerated failure
// is not a fail-fast trigger.
func TestContinueOnErrorDoesNotFailFastMatrixSiblings(t *testing.T) {
	workflow := `name: MatrixTolerated
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    continue-on-error: true
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - run: echo ${{ matrix.go }}
`
	e, runID := needsEnv(t, workflow)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	first, second := jobs[0].ID, jobs[1].ID
	// Complete reports the conclusion, which is where fail-fast lives.
	if _, cerr := e.svc.Complete(ctx(), AttemptClaims{
		Repo: "o/r", Run: runID, Job: first, Attempt: 1, Scopes: []string{ScopeComplete},
	}, CompleteInput{Conclusion: JobFailure}); cerr != nil {
		t.Fatalf("Complete: %v", cerr)
	}
	// The sibling is untouched (fail-fast would have cancelled it).
	if got := e.jobState(t, runID, second); got != JobQueued {
		t.Fatalf("sibling = %q, want queued (a tolerated failure must not fail fast)", got)
	}
}

// TestJobIfSurvivesApprovalAndRerun pins the two later enqueue paths: approval
// enqueues only queued jobs, and a rerun re-evaluates the condition instead of
// reviving a job that must stay skipped.
func TestJobIfSurvivesApprovalAndRerun(t *testing.T) {
	e, runID := needsEnv(t, jobIfCreateYAML)
	// Rerun: the skipped job is re-decided (still false) and not re-enqueued.
	run, err := e.svc.RerunRun(ctx(), "o/r", runID, e.writer())
	if err != nil || run == nil {
		t.Fatalf("RerunRun: %v", err)
	}
	if got := e.jobState(t, runID, "j-skip-me"); got != JobSkipped {
		t.Fatalf("rerun skip-me = %q, want skipped", got)
	}
	if e.pendingJobIDs(t)["j-skip-me"] {
		t.Fatal("a rerun enqueued a job whose condition is false")
	}
	rerun, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || rerun == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if rerun.Attempt != 2 {
		t.Fatalf("rerun attempt = %d", rerun.Attempt)
	}
}

// TestApprovalOfAnAllSkippedRunFolds pins the second enqueue path (approval):
// a held run whose every job was already decided before approval has nothing to
// enqueue and must fold instead of sitting at pending_approval/queued forever.
func TestApprovalOfAnAllSkippedRunFolds(t *testing.T) {
	e, _ := approvalEnv(t)
	workflow := `name: HeldIf
on:
  pull_request:
    branches: [main]
jobs:
  skip-me:
    runs-on: linux
    if: false
    steps:
      - run: echo never
  also-skipped:
    runs-on: linux
    if: github.event_name == 'push'
    steps:
      - run: echo never
`
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflow)
	runs := forkPR(t, e, "newcomer")
	if len(runs) != 1 || runs[0].State != RunPendingApproval {
		t.Fatalf("runs = %+v", runs)
	}
	held := runs[0]
	if got := countPrefix(t, e, QueuePrefix("o", "r")); got != 0 {
		t.Fatalf("queue objects = %d before approval", got)
	}
	approved, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, maintainer())
	if err != nil || approved == nil {
		t.Fatalf("ApproveRun: %+v, %v", approved, err)
	}
	if got := countPrefix(t, e, QueuePrefix("o", "r")); got != 0 {
		t.Fatalf("a decided job was enqueued at approval: %d entries", got)
	}
	if approved.State != RunSuccess {
		t.Fatalf("approved run = %q, want success (nothing left to run)", approved.State)
	}
	for _, id := range []string{"j-skip-me", "j-also-skipped"} {
		if got := e.jobState(t, held.ID, id); got != JobSkipped {
			t.Fatalf("%s = %q, want skipped", id, got)
		}
	}
}

// TestCreationSkipCascadesWithoutARunner pins convergence: a job skipped by its
// condition dooms its dependents immediately (no probe, no runner), and a run
// with nothing left to run folds at creation instead of sitting at queued.
func TestCreationSkipCascadesWithoutARunner(t *testing.T) {
	workflow := `name: SkipCascade
on:
  push:
    branches: [main]
jobs:
  lint:
    runs-on: linux
    if: false
    steps:
      - run: echo never
  build:
    runs-on: linux
    needs: [lint]
    steps:
      - run: echo build
`
	e, runID := needsEnv(t, workflow)
	for _, id := range []string{"j-lint", "j-build"} {
		if got := e.jobState(t, runID, id); got != JobSkipped {
			t.Fatalf("%s = %q, want skipped (the cascade must not wait for a runner)", id, got)
		}
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunSuccess {
		t.Fatalf("run = %q, want success (skipped is neutral)", run.State)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("a fully skipped run offered a job: %q", got)
	}
}

// TestRerunSettlesSkipsInAnyOrder pins the rerun tail: a condition that skips a
// dependency still dooms its dependent when the reset loop reached the
// dependent first (the cascade runs against the final job set).
func TestRerunSettlesSkipsInAnyOrder(t *testing.T) {
	workflow := `name: RerunSkip
on:
  push:
    branches: [main]
jobs:
  aaa-dependent:
    runs-on: linux
    needs: [zzz-root]
    steps:
      - run: echo dependent
  zzz-root:
    runs-on: linux
    if: false
    steps:
      - run: echo root
`
	e, runID := needsEnv(t, workflow)
	if _, err := e.svc.RerunRun(ctx(), "o/r", runID, e.writer()); err != nil {
		t.Fatalf("RerunRun: %v", err)
	}
	for _, id := range []string{"j-aaa-dependent", "j-zzz-root"} {
		if got := e.jobState(t, runID, id); got != JobSkipped {
			t.Fatalf("%s = %q, want skipped after the rerun", id, got)
		}
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunSuccess {
		t.Fatalf("rerun run = %q, want success", run.State)
	}
}
