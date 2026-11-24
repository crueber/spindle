package threads

import (
	"encoding/json"
	"testing"
)

// `needs:` ordering (15 §4.1, round 2): a dependent job is not claimable until
// every dependency is terminal success; a failed/cancelled dependency skips the
// dependent transitively; unrelated jobs start in parallel. The gate lives in
// the queue (entry eligibility), never in the runner.

const needsChainYAML = `name: Chain
on:
  push:
    branches: [main]
jobs:
  lint:
    runs-on: linux
    steps:
      - run: echo lint
  build:
    runs-on: linux
    needs: [lint]
    steps:
      - run: echo build
  test:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo test
`

const needsDiamondYAML = `name: Diamond
on:
  push:
    branches: [main]
jobs:
  root:
    runs-on: linux
    steps:
      - run: echo root
  left:
    runs-on: linux
    needs: [root]
    steps:
      - run: echo left
  right:
    runs-on: linux
    needs: [root]
    steps:
      - run: echo right
  join:
    runs-on: linux
    needs: [left, right]
    steps:
      - run: echo join
`

// needsLateRootYAML names the dependent "aaa" so it is ENQUEUED FIRST (job keys
// are sorted): a waiting entry must never starve its dependency in the probe.
const needsLateRootYAML = `name: Late
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
    steps:
      - run: echo root
`

// needsEnv swaps the fixture workflow and creates the push run.
func needsEnv(t *testing.T, workflow string) (*testEnv, string) {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflow)
	runs := e.mustCreate(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	return e, runs[0].ID
}

// needsRunner is the label set the fixture jobs ask for.
// needsRunner is the shared unit-test runner. It opts in to fork runs so
// fork-flow tests (permissions, payloads, approval) need no special casing; the
// fork-gating tests construct a DEFAULT runner explicitly to pin the refusal.
func needsRunner() *RunnerDoc {
	return &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive, AllowForkRuns: true}
}

// claimOne runs one claim probe and returns the claimed job id ("" = nothing).
func (e *testEnv) claimOne(t *testing.T, runID string) string {
	t.Helper()
	r := needsRunner()
	claim, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels)
	if err != nil {
		t.Fatalf("claimLoop: %v", err)
	}
	if claim == nil {
		return ""
	}
	if claim.Entry.RunID != runID {
		t.Fatalf("claimed a job of another run: %+v", claim.Entry)
	}
	return claim.Entry.JobID
}

// finish drives one job to a terminal (or running) state, as a runner does.
func (e *testEnv) finish(t *testing.T, runID, jobID, state string) {
	t.Helper()
	if _, err := e.svc.UpdateJob(ctx(), "o/r", runID, jobID, JobUpdate{State: state}); err != nil {
		t.Fatalf("UpdateJob(%s, %s): %v", jobID, state, err)
	}
}

// jobState reads one job record's state.
func (e *testEnv) jobState(t *testing.T, runID, jobID string) string {
	t.Helper()
	raw, _, err := e.svc.getJSON(ctx(), JobKey("o", "r", runID, jobID))
	if err != nil || raw == nil {
		t.Fatalf("job %s: %v", jobID, err)
	}
	var job Job
	if jerr := json.Unmarshal(raw, &job); jerr != nil {
		t.Fatalf("job %s: %v", jobID, jerr)
	}
	return job.State
}

// TestNeedsChainOrderIsStrict walks a linear chain: only the head is claimable,
// each step stays blocked while its dependency is live, and success releases
// exactly the next one.
func TestNeedsChainOrderIsStrict(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)

	if got := e.claimOne(t, runID); got != "j-lint" {
		t.Fatalf("first claim = %q, want j-lint (nothing else is eligible)", got)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("claim while lint runs = %q, want none", got)
	}
	e.finish(t, runID, "j-lint", JobRunning)
	e.finish(t, runID, "j-lint", JobSuccess)
	if got := e.claimOne(t, runID); got != "j-build" {
		t.Fatalf("claim after lint success = %q, want j-build", got)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("claim while build runs = %q, want none", got)
	}
	e.finish(t, runID, "j-build", JobSuccess)
	if got := e.claimOne(t, runID); got != "j-test" {
		t.Fatalf("claim after build success = %q, want j-test", got)
	}
	e.finish(t, runID, "j-test", JobSuccess)
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("claim after the chain finished = %q, want none", got)
	}
}

// TestNeedsDiamondJoinWaitsForBoth pins a diamond: the two middles run in
// parallel once the root is green, and the join waits for both.
func TestNeedsDiamondJoinWaitsForBoth(t *testing.T) {
	e, runID := needsEnv(t, needsDiamondYAML)

	if got := e.claimOne(t, runID); got != "j-root" {
		t.Fatalf("first claim = %q, want j-root", got)
	}
	e.finish(t, runID, "j-root", JobSuccess)
	first := e.claimOne(t, runID)
	second := e.claimOne(t, runID)
	if first == second || first == "" || second == "" {
		t.Fatalf("diamond parallel claims = %q, %q (want left and right)", first, second)
	}
	// Both middles are live: the join must stay blocked.
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("join claimed before its middles finished: %q", got)
	}
	e.finish(t, runID, first, JobSuccess)
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("join claimed with one middle still live: %q", got)
	}
	e.finish(t, runID, second, JobSuccess)
	if got := e.claimOne(t, runID); got != "j-join" {
		t.Fatalf("join claim = %q, want j-join", got)
	}
}

// TestNeedsFailedDependencySkipsDownstream pins the failure rule: a failed
// dependency skips its dependent AND that dependent's dependents (transitive),
// without ever offering them to a runner.
func TestNeedsFailedDependencySkipsDownstream(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	if got := e.claimOne(t, runID); got != "j-lint" {
		t.Fatalf("first claim = %q", got)
	}
	e.finish(t, runID, "j-lint", JobFailure)

	if got := e.jobState(t, runID, "j-build"); got != JobSkipped {
		t.Fatalf("dependent of a failed job = %q, want skipped", got)
	}
	if got := e.jobState(t, runID, "j-test"); got != JobSkipped {
		t.Fatalf("transitive dependent = %q, want skipped", got)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("a skipped job was offered: %q", got)
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunFailure {
		t.Fatalf("run state = %q, want failure", run.State)
	}
	// The skip is a check too: it reads `skipped` (distinct from a cancel) and
	// must never read green.
	last := e.checks.reports[len(e.checks.reports)-1]
	if last.State != "skipped" {
		t.Fatalf("skipped check state = %q, want skipped", last.State)
	}
}

// TestNeedsCancelledDependencySkipsDownstream is the cancel side of the same
// rule (an operator/concurrency cancel, not a failure).
func TestNeedsCancelledDependencySkipsDownstream(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	e.finish(t, runID, "j-lint", JobCancelled)
	for _, id := range []string{"j-build", "j-test"} {
		if got := e.jobState(t, runID, id); got != JobSkipped {
			t.Fatalf("%s = %q, want skipped", id, got)
		}
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunCancelled {
		t.Fatalf("run state = %q, want cancelled (no failure in the run)", run.State)
	}
}

// TestNeedsUnrelatedJobsStartInParallel pins that gating is per dependency set,
// not a run-wide serialisation.
func TestNeedsUnrelatedJobsStartInParallel(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML+`
  docs:
    runs-on: linux
    steps:
      - run: echo docs
`)
	// Two independent jobs are claimable before anything has run (the pair is
	// what matters, not the order: entries are enqueued in job-key order).
	first := e.claimOne(t, runID)
	second := e.claimOne(t, runID)
	got := map[string]bool{first: true, second: true}
	if !got["j-lint"] || !got["j-docs"] || first == "" || second == "" || first == second {
		t.Fatalf("parallel claims = %q, %q (want j-lint and j-docs)", first, second)
	}
}

// TestNeedsWaitingEntryDoesNotStarveDependency pins the probe's scan budget:
// the dependent is enqueued first (its job key sorts earlier), and the probe
// must still reach the dependency it waits on.
func TestNeedsWaitingEntryDoesNotStarveDependency(t *testing.T) {
	e, runID := needsEnv(t, needsLateRootYAML)
	if got := e.claimOne(t, runID); got != "j-zzz-root" {
		t.Fatalf("claim = %q, want j-zzz-root (the waiting entry must not starve it)", got)
	}
	e.finish(t, runID, "j-zzz-root", JobSuccess)
	if got := e.claimOne(t, runID); got != "j-aaa-dependent" {
		t.Fatalf("claim after root = %q, want j-aaa-dependent", got)
	}
}

// TestNeedsMatrixDependencyWaitsForEveryCell pins that a `needs` on a matrix
// job means ALL its cells (a matrix fan-in).
func TestNeedsMatrixDependencyWaitsForEveryCell(t *testing.T) {
	workflow := `name: Matrix needs
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - run: echo ${{ matrix.go }}
  deploy:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo deploy
`
	e, runID := needsEnv(t, workflow)
	first := e.claimOne(t, runID)
	second := e.claimOne(t, runID)
	if first == "" || second == "" || first == second {
		t.Fatalf("matrix cells = %q, %q (want both claimable)", first, second)
	}
	e.finish(t, runID, first, JobSuccess)
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("deploy claimed with one cell live: %q", got)
	}
	e.finish(t, runID, second, JobSuccess)
	if got := e.claimOne(t, runID); got != "j-deploy" {
		t.Fatalf("deploy claim = %q", got)
	}
}

// TestNeedsQueueEntryCarriesDependencies pins the entry projection the claim
// path reads (one bounded GET per dependency; no LIST).
func TestNeedsQueueEntryCarriesDependencies(t *testing.T) {
	e, _ := needsEnv(t, needsChainYAML)
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	needs := map[string][]string{}
	for _, seq := range head.Pending {
		entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if eerr != nil || entry == nil {
			t.Fatalf("entry %s: %v", seq, eerr)
		}
		needs[entry.JobID] = entry.Needs
	}
	if len(needs["j-lint"]) != 0 {
		t.Fatalf("root needs = %v, want none", needs["j-lint"])
	}
	if got := needs["j-build"]; len(got) != 1 || got[0] != "j-lint" {
		t.Fatalf("build needs = %v, want [j-lint]", got)
	}
	if got := needs["j-test"]; len(got) != 1 || got[0] != "j-build" {
		t.Fatalf("test needs = %v, want [j-build]", got)
	}
}
