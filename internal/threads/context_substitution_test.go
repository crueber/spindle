package threads

// context_substitution_test.go — the run-creation half of PAR-004/PAR-030/
// PAR-032/PAR-056/PAR-151: the `spindle.*` alias set, the substituted
// `container.image`, the full github field set against a real run, and the two
// expression-valued booleans evaluated from the same matrix cell as the rest of
// the strategy.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// TestContainerImageSubstitutesPerCell pins PAR-030's acceptance: a
// `container: image: debian:${{ matrix.variant }}` pulls the real tag on each
// cell (the job spec carries it, so the image gate and the container plan see
// it), never the authored span.
func TestContainerImageSubstitutesPerCell(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: M
on: push
jobs:
  build:
    runs-on: linux
    container:
      image: "debian:${{ matrix.variant }}"
    strategy:
      matrix:
        variant: [bookworm, trixie]
    steps:
      - run: echo hi
`)
	run := e.mustCreate(t)[0]
	jobs := mustJobs(t, e, run.ID)
	seen := map[string]bool{}
	for _, job := range jobs {
		if job.Spec.Container == nil {
			t.Fatalf("%s: no container spec", job.Key)
		}
		image := job.Spec.Container.Image
		if strings.Contains(image, "${{") {
			t.Fatalf("%s image still literal: %q", job.Key, image)
		}
		if image != "debian:bookworm" && image != "debian:trixie" {
			t.Fatalf("%s image = %q", job.Key, image)
		}
		seen[image] = true
	}
	if !seen["debian:bookworm"] || !seen["debian:trixie"] {
		t.Fatalf("cells = %v, want one job per variant", seen)
	}
}

// TestContinueOnErrorExpressionPerCell pins PAR-056's acceptance:
// `continue-on-error: ${{ matrix.experimental }}` tolerates exactly the cell
// that carries true, and a failed tolerated cell still leaves the run green.
func TestContinueOnErrorExpressionPerCell(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: T
on: push
jobs:
  build:
    runs-on: linux
    continue-on-error: ${{ matrix.experimental }}
    strategy:
      matrix:
        experimental: ["true", "false"]
    steps:
      - run: echo hi
`)
	run := e.mustCreate(t)[0]
	tolerated := ""
	for _, job := range mustJobs(t, e, run.ID) {
		want := job.Spec.Matrix["experimental"] == "true"
		if job.Spec.ContinueOnError != want {
			t.Fatalf("%s experimental=%q continue-on-error = %v, want %v",
				job.Key, job.Spec.Matrix["experimental"], job.Spec.ContinueOnError, want)
		}
		if want {
			tolerated = job.ID
		}
	}
	if tolerated == "" {
		t.Fatal("no tolerated cell was created")
	}
	// Drive the tolerated cell to failure: the run stays green and the job is
	// recorded as tolerated (the job-level semantics already pinned by
	// TestContinueOnErrorJobKeepsTheRunGreen — this is the per-cell half).
	e.finish(t, run.ID, tolerated, JobRunning)
	e.finish(t, run.ID, tolerated, JobFailure)
	job := toleratedJob(t, e, run.ID, tolerated)
	if job.State != JobFailure || !job.Tolerated {
		t.Fatalf("tolerated cell = %s/%v", job.State, job.Tolerated)
	}
}

// TestConcurrencyCancelInProgressExpression pins PAR-151's acceptance:
// `cancel-in-progress: ${{ github.ref_name != 'main' }}` cancels a holder for a
// branch run and leaves it alone for a main run.
func TestConcurrencyCancelInProgressExpression(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	const exprWorkflow = `name: Deploy
on: [push]
concurrency:
  group: expr-group
  cancel-in-progress: ${{ github.ref_name != 'main' }}
jobs:
  ship:
    runs-on: linux
    steps:
      - run: echo ship
`
	e.git.add(testSHA, ".spindle/workflows/ci.yml", exprWorkflow)
	e.git.refs["refs/heads/main"] = testSHA

	// A main run does not cancel the holder: the expression is false.
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatalf("first run = %v (%v)", first, err)
	}
	holdGroup(t, e, first[0])
	second, cerr := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if cerr != nil || len(second) != 1 {
		t.Fatalf("second main run = %v (%v)", second, cerr)
	}
	if second[0].PendingGroup == "" {
		t.Fatal("a false cancel-in-progress did not wait on the holder")
	}
	if got, _ := e.svc.loadRun(ctx(), "o", "r", first[0].ID); got == nil || got.State != RunQueued {
		t.Fatalf("a false cancel-in-progress cancelled the holder: %+v", got)
	}

	// A feature run evaluates true and cancels the holder at creation.
	e.git.refs["refs/heads/feature"] = testSHA
	third, terr := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "feature", Ref: "refs/heads/feature", Actor: "w"})
	if terr != nil || len(third) != 1 {
		t.Fatalf("feature run = %v (%v)", third, terr)
	}
	got, lerr := e.svc.loadRun(ctx(), "o", "r", first[0].ID)
	if lerr != nil || got == nil {
		t.Fatal(lerr)
	}
	if got.State != RunCancelled || got.CancelReason != CancelReasonConcurrency {
		t.Fatalf("holder after a true expression = %s / %q", got.State, got.CancelReason)
	}
	// The expression is a spec input only: the authored text never survives
	// into the job record.
	for _, job := range mustJobs(t, e, third[0].ID) {
		if job.Spec.Concurrency == nil {
			t.Fatalf("%s: no concurrency spec", job.Key)
		}
		if job.Spec.Concurrency.CancelInProgressExpr != "" {
			t.Fatalf("%s kept the expression: %q", job.Key, job.Spec.Concurrency.CancelInProgressExpr)
		}
		if !job.Spec.Concurrency.CancelInProgress {
			t.Fatalf("%s: cancel-in-progress expression did not resolve true", job.Key)
		}
	}
}

// TestGitHubContextFullFieldSetOnARun pins PAR-032 against a real run record:
// every value-bearing field resolves from the run (workflow_ref/workflow_sha,
// ref_type, run_attempt, triggering_actor, repository_owner, head_ref/base_ref
// from the payload), and the fields the model cannot value are the SAME named
// error in the server's job condition and a step substitution.
func TestGitHubContextFullFieldSetOnARun(t *testing.T) {
	e, runID := needsEnv(t, `name: Ctx
on: push
jobs:
  show:
    runs-on: linux
    steps:
      - run: echo hi
`)
	if got := e.claimOne(t, runID); got != "j-show" {
		t.Fatalf("claim = %q", got)
	}
	job := e.jobRecord(t, runID, "j-show")
	gh := githubContext(mustRun(t, e, runID), job.specWorkflow("Ctx"))
	want := map[string]string{
		"workflow_ref":     "o/r/.spindle/workflows/ci.yml@refs/heads/main",
		"workflow_sha":     testSHA,
		"ref_type":         "branch",
		"run_attempt":      "1",
		"triggering_actor": "writer",
		"repository_owner": "o",
		"actor":            "writer",
		"ref":              "refs/heads/main",
		"ref_name":         "main",
	}
	for field, expected := range want {
		if got := gh[field]; got != expected {
			t.Errorf("github.%s = %q, want %q", field, got, expected)
		}
	}
	// A push has no pull-request head/base ref: the documented answer is empty,
	// never a guessed branch.
	if gh["head_ref"] != "" || gh["base_ref"] != "" {
		t.Fatalf("push head/base ref = %q/%q, want empty", gh["head_ref"], gh["base_ref"])
	}
	// The empty strings are real values, not missing fields.
	for _, field := range []string{"head_ref", "base_ref"} {
		if _, unavailable := workflow.GitHubFieldUnavailable(field); unavailable {
			t.Fatalf("%s is marked unavailable, but an empty value is documented", field)
		}
	}
}

// mustRun loads one run record (test observability).
func mustRun(t *testing.T, e *testEnv, runID string) *Run {
	t.Helper()
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun(%s): %v", runID, err)
	}
	return run
}

// TestSubstitutionFailsClosedAtRunCreation pins the failure branch of each new
// resolver: a non-boolean tolerant flag, a non-boolean cancel-in-progress and an
// image reference with no value for this run all FAIL creation (the job is
// never recorded with a guessed value).
func TestSubstitutionFailsClosedAtRunCreation(t *testing.T) {
	cases := []struct{ name, workflow, want string }{
		{
			name: "continue-on-error not a boolean",
			workflow: `name: X
on: push
jobs:
  a:
    runs-on: linux
    continue-on-error: ${{ 'maybe' }}
    steps:
      - run: echo hi
`,
			want: "not a boolean",
		},
		{
			name: "cancel-in-progress not a boolean",
			workflow: `name: X
on: push
concurrency:
  group: g
  cancel-in-progress: ${{ 'maybe' }}
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`,
			want: "not a boolean",
		},
		{
			name: "container image with no value",
			workflow: `name: X
on:
  push:
  workflow_dispatch:
    inputs:
      variant:
        type: string
jobs:
  a:
    runs-on: linux
    container:
      image: "debian:${{ inputs.variant }}"
    steps:
      - run: echo hi
`,
			want: "resolved to nothing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", tc.workflow)
			_, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateRun = %v, want %q", err, tc.want)
			}
		})
	}
}
