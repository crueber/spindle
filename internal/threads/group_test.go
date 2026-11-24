package threads

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Concurrency-group contexts and rendered matrix names (Threads round-2 audit).
//
// A group is a bucket key and the lock that decides which run executes, so a
// `${{ … }}` that does not resolve must never become literal text: a
// repository-wide key with `cancel-in-progress: true` lets a feature-branch
// push cancel main's run. The parser fails such a file; run creation fails a
// reference that has no value for THIS run. A job name is display-only and is
// filled leniently, but it must render the matrix cell it belongs to.

const branchGroupWorkflow = `name: Branch CI
on: [push]
concurrency:
  group: "ci-${{ github.ref }}"
  cancel-in-progress: true
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// createOnBranch creates one run of a one-file workflow on a branch.
func (e *testEnv) createOnBranch(t *testing.T, path, yaml, branch string) *Run {
	t.Helper()
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, path, yaml)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		Branch: branch, Ref: "refs/heads/" + branch, Actor: "writer",
	})
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", branch, err)
	}
	if len(runs) != 1 {
		t.Fatalf("CreateRun(%s) = %d runs, want 1", branch, len(runs))
	}
	return runs[0]
}

// TestConcurrencyGroupPerBranchDoesNotCancelMain is the audit's sharp edge: a
// feature-branch push and a main push hold DIFFERENT groups, so the newer run
// never cancels main's.
func TestConcurrencyGroupPerBranchDoesNotCancelMain(t *testing.T) {
	e := newTestEnv(t)
	main := e.createOnBranch(t, ".spindle/workflows/ci.yml", branchGroupWorkflow, "main")
	feature := e.createOnBranch(t, ".spindle/workflows/ci.yml", branchGroupWorkflow, "fix/x")
	if len(main.Concurrency) != 1 || main.Concurrency[0] != "ci-refs/heads/main" {
		t.Fatalf("main group = %v", main.Concurrency)
	}
	if len(feature.Concurrency) != 1 || feature.Concurrency[0] != "ci-refs/heads/fix/x" {
		t.Fatalf("feature group = %v", feature.Concurrency)
	}
	a := runnerFor(e)
	if job, err := e.svc.FetchJob(ctx(), a, time.Millisecond); err != nil || job == nil || job.RunID != main.ID {
		t.Fatalf("main fetch = %+v, %v", job, err)
	}
	// The feature run takes its own group: it is admitted, and cancel-in-progress
	// finds no foreign holder to cancel.
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbranch2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), b, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("feature fetch = %+v, %v (a per-branch group must not serialise)", job, err)
	}
	if job.RunID != feature.ID {
		t.Fatalf("feature fetch claimed %s, want %s", job.RunID, feature.ID)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", main.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.CancelRequested || j.State == JobCancelled {
			t.Fatalf("a feature-branch push cancelled main's job: %+v", j)
		}
	}
	mainRun, err := e.svc.loadRun(ctx(), "o", "r", main.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal(mainRun.State) {
		t.Fatalf("main run folded %s", mainRun.State)
	}
}

// TestConcurrencyGroupSameBranchSerialises pins the other half: the expression
// still serialises two runs of the SAME branch.
func TestConcurrencyGroupSameBranchSerialises(t *testing.T) {
	e := newTestEnv(t)
	first := e.createOnBranch(t, ".spindle/workflows/ci.yml", branchGroupWorkflow, "main")
	second := e.createOnBranch(t, ".spindle/workflows/ci.yml", branchGroupWorkflow, "main")
	if first.Concurrency[0] != second.Concurrency[0] {
		t.Fatalf("same branch, different groups: %v / %v", first.Concurrency, second.Concurrency)
	}
	a := runnerFor(e)
	if job, err := e.svc.FetchJob(ctx(), a, time.Millisecond); err != nil || job == nil || job.RunID != first.ID {
		t.Fatalf("first fetch = %+v, %v", job, err)
	}
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbranch3", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	if got, ferr := e.svc.FetchJob(ctx(), b, time.Millisecond); ferr != nil || got != nil {
		t.Fatalf("same-branch run admitted alongside the holder: %+v %v", got, ferr)
	}
}

const matrixGroupWorkflow = `name: Matrix Group
on: [push]
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    concurrency:
      group: "ci-${{ github.ref }}-${{ matrix.go }}"
      cancel-in-progress: false
    steps:
      - run: echo hi
`

const matrixSharedGroupWorkflow = `name: Matrix Shared
on: [push]
concurrency: {group: "ci-${{ github.ref }}", cancel-in-progress: false}
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - run: echo hi
`

func TestConcurrencyGroupMatrixCells(t *testing.T) {
	e := newTestEnv(t)
	run := e.createOnBranch(t, ".spindle/workflows/m.yml", matrixGroupWorkflow, "main")
	if len(run.Concurrency) != 2 ||
		run.Concurrency[0] != "ci-refs/heads/main-1.26" || run.Concurrency[1] != "ci-refs/heads/main-1.27" {
		t.Fatalf("per-cell groups = %v", run.Concurrency)
	}

	// Without a matrix reference the cells belong to the run's ONE group, and a
	// holding run's siblings pass (a workflow-level group belongs to the run).
	shared := newTestEnv(t)
	run = shared.createOnBranch(t, ".spindle/workflows/m.yml", matrixSharedGroupWorkflow, "main")
	if len(run.Concurrency) != 1 || run.Concurrency[0] != "ci-refs/heads/main" {
		t.Fatalf("shared group = %v", run.Concurrency)
	}
	a := runnerFor(shared)
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrmatrix2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	first, err := shared.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first cell = %+v, %v", first, err)
	}
	second, err := shared.svc.FetchJob(ctx(), b, time.Millisecond)
	if err != nil || second == nil {
		t.Fatalf("sibling cell blocked by its own run's group: %+v, %v", second, err)
	}
	if first.RunID != run.ID || second.RunID != run.ID || first.JobID == second.JobID {
		t.Fatalf("cells = %s/%s of %s/%s", first.JobID, second.JobID, first.RunID, second.RunID)
	}
}

const namedCellWorkflow = `name: Named
on: [push]
jobs:
  cell:
    name: "cell-${{ matrix.v }}"
    runs-on: linux
    strategy:
      matrix:
        v: [a, b]
    steps:
      - run: echo hi
`

// TestMatrixJobNameRendersItsCell: the rendered name and the checks context
// agree, and a name that names a matrix axis does not also get the "(a)"
// suffix.
func TestMatrixJobNameRendersItsCell(t *testing.T) {
	e := newTestEnv(t)
	run := e.createOnBranch(t, ".spindle/workflows/named.yml", namedCellWorkflow, "main")
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 2 {
		t.Fatalf("jobs = %d", len(view.Jobs))
	}
	wantNames := []string{"cell-a", "cell-b"}
	wantContexts := []string{"threads/named/cell-a", "threads/named/cell-b"}
	for i, job := range view.Jobs {
		if job.Name != wantNames[i] {
			t.Fatalf("job %d name = %q, want %q", i, job.Name, wantNames[i])
		}
		if job.Context != wantContexts[i] {
			t.Fatalf("job %d context = %q, want %q", i, job.Context, wantContexts[i])
		}
		if job.Spec.Matrix["v"] != wantNames[i][len("cell-"):] {
			t.Fatalf("job %d cell = %v", i, job.Spec.Matrix)
		}
	}
	if view.Jobs[0].ID == view.Jobs[1].ID {
		t.Fatal("cells share a job id")
	}
	// The checks mirror reads the same rendered name (context agreement).
	mirrored := map[string]bool{}
	for _, rep := range e.checks.reports {
		mirrored[rep.Context] = true
	}
	for _, want := range wantContexts {
		if !mirrored[want] {
			t.Fatalf("no check mirror for %q (have %v)", want, mirrored)
		}
	}
}

const unresolvedInputGroupWorkflow = `name: Input Group
on:
  push:
  workflow_dispatch:
    inputs:
      mode:
        type: string
concurrency:
  group: "ci-${{ inputs.mode }}"
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

const defaultedInputGroupWorkflow = `name: Input Group
on:
  push:
  workflow_dispatch:
    inputs:
      mode:
        type: string
        default: fast
concurrency:
  group: "ci-${{ inputs.mode }}"
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// TestConcurrencyGroupUnresolvedInputFailsTheRun: a declared input with no
// value and no default cannot key a group, so the run is not created with a
// literal (fail closed). A declared default resolves outside a dispatch.
func TestConcurrencyGroupUnresolvedInputFailsTheRun(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", unresolvedInputGroupWorkflow)
	_, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"})
	if err == nil {
		t.Fatal("an unresolvable group created a run")
	}
	if !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("error = %v, want ErrUnprocessable", err)
	}
	if !strings.Contains(err.Error(), "inputs.mode") || !strings.Contains(err.Error(), "supported:") {
		t.Fatalf("error must name the key and the supported set: %v", err)
	}

	defaulted := newTestEnv(t)
	run := defaulted.createOnBranch(t, ".spindle/workflows/ci.yml", defaultedInputGroupWorkflow, "main")
	if len(run.Concurrency) != 1 || run.Concurrency[0] != "ci-fast" {
		t.Fatalf("defaulted input group = %v", run.Concurrency)
	}
}

const dispatchGroupWorkflow = `name: Dispatch Group
on:
  workflow_dispatch:
    inputs:
      mode:
        type: choice
        options: [staging, production]
        default: staging
concurrency: {group: "ci-${{ inputs.mode }}", cancel-in-progress: true}
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// TestConcurrencyGroupUsesDispatchInputs: a dispatch group keys on the values
// the dispatcher supplied (and the declared default when it omits one).
func TestConcurrencyGroupUsesDispatchInputs(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/grouped.yml", dispatchGroupWorkflow)
	staging, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", ".spindle/workflows/grouped.yml",
		nil, e.writer())
	if err != nil || len(staging) != 1 {
		t.Fatalf("default dispatch = %+v, %v", staging, err)
	}
	if got := staging[0].Concurrency; len(got) != 1 || got[0] != "ci-staging" {
		t.Fatalf("default group = %v", got)
	}
	prod, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", ".spindle/workflows/grouped.yml",
		map[string]string{"mode": "production"}, e.writer())
	if err != nil || len(prod) != 1 {
		t.Fatalf("dispatch = %+v, %v", prod, err)
	}
	if got := prod[0].Concurrency; len(got) != 1 || got[0] != "ci-production" {
		t.Fatalf("supplied group = %v", got)
	}
}

// TestExpandJobSpecEdges pins the shapes the run path hands it: a nil basis
// spec, a display name with a malformed template (the suffix still applies),
// and a strategy whose only axis is empty (no product, one instance).
func TestExpandJobSpecEdges(t *testing.T) {
	instances, err := expandJobSpec("a", "", nil, expandContext{})
	if err != nil || len(instances) != 1 || instances[0].Key != "a" || instances[0].Name != "" {
		t.Fatalf("nil basis = %+v, %v", instances, err)
	}
	base := &Spec{Strategy: &StrategySpec{Matrix: map[string][]string{"go": {"1.26", "1.27"}}}}
	instances, err = expandJobSpec("a", "${{ broken", base, expandContext{})
	if err != nil || len(instances) != 2 {
		t.Fatalf("malformed name = %+v, %v", instances, err)
	}
	for i, want := range []string{"${{ broken (1.26)", "${{ broken (1.27)"} {
		if instances[i].Name != want {
			t.Fatalf("name %d = %q, want %q", i, instances[i].Name, want)
		}
	}
	empty := &Spec{Strategy: &StrategySpec{Matrix: map[string][]string{"a": {}}}}
	if n, perr := matrixProductSize(empty.Strategy); perr != nil || n != 1 {
		t.Fatalf("empty axis product = %d, %v", n, perr)
	}
}

// TestUnsupportedGroupExpressionDisablesOnlyThatFile: the parser's per-file
// error means the workflow never runs (no literal-keyed run), and discovery
// reports it rather than failing the repository.
func TestUnsupportedGroupExpressionDisablesOnlyThatFile(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/bad.yml", `name: Bad
on: [push]
concurrency: "ci-${{ env.BRANCH }}"
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("a broken file must not fail the whole call: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a workflow with an unsupported group expression created %d runs", len(runs))
	}
	files, derr := Discover(ctx(), e.git, "o/r", testSHA)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(files) != 1 || files[0].Error == "" || !strings.Contains(files[0].Error, "env.BRANCH") {
		t.Fatalf("discovery did not report the group expression: %+v", files)
	}
}
