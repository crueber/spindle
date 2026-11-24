package threads

// Step and job outputs on the server side (docs/threads.md §3.9): the `if:`
// contexts, the matrix `needs:` fold, storing the reported outputs, the
// fail-closed caps and the rerun reset.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestEvalIfStepOutputs(t *testing.T) {
	ctx := ExprContext{
		Success: true,
		Steps:   map[string]string{"build": JobSuccess, "skipped": "skipped"},
		StepConclusions: map[string]string{
			"build": JobSuccess, "skipped": "skipped",
		},
		Outputs: map[string]map[string]string{
			"build": {"digest": "abc123", "flag": "true"},
			// A step that ran but wrote nothing keeps an empty entry.
			"skipped": {},
		},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{`steps.build.outputs.digest == 'abc123'`, true},
		{`steps.BUILD.outputs.digest == 'abc123'`, true}, // ids are case-insensitive
		{`steps.build.outputs.flag == 'true'`, true},
		{`steps.build.outputs.missing == ''`, true}, // known step, unset output reads empty
		{`steps.skipped.outputs.anything == ''`, true},
	}
	for _, tc := range cases {
		got, err := EvalIf(tc.expr, ctx)
		if err != nil || got != tc.want {
			t.Errorf("EvalIf(%q) = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
	// An unknown step id is a named error, never "not set".
	if _, err := EvalIf(`steps.ghost.outputs.x == ''`, ctx); err == nil || !strings.Contains(err.Error(), "no step id") {
		t.Fatalf("unknown step id = %v", err)
	}
}

func TestEvalIfNeedsOutputs(t *testing.T) {
	ctx := ExprContext{
		Success:      true,
		Needs:        map[string]string{"build": JobSuccess},
		NeedsOutputs: map[string]map[string]string{"build": {"digest": "abc"}},
	}
	if ok, err := EvalIf(`needs.build.outputs.digest == 'abc'`, ctx); err != nil || !ok {
		t.Fatalf("needs output = %v, %v", ok, err)
	}
	if ok, err := EvalIf(`needs.build.outputs.missing == ''`, ctx); err != nil || !ok {
		t.Fatalf("unset needs output must read empty: %v, %v", ok, err)
	}
	if _, err := EvalIf(`needs.ghost.outputs.x == ''`, ctx); err == nil || !strings.Contains(err.Error(), "no `needs:` dependency") {
		t.Fatalf("unknown needs key = %v", err)
	}
}

func TestFoldNeedsMatrixOutputs(t *testing.T) {
	byID := map[string]*Job{
		"j-c1": {ID: "j-c1", State: JobSuccess, Outputs: map[string]string{"digest": "one", "only1": "a"}},
		"j-c2": {ID: "j-c2", State: JobSuccess, Outputs: map[string]string{"digest": "two"}},
	}
	facts := foldNeeds(map[string][]string{"build": {"j-c1", "j-c2"}}, byID)
	if facts.Results["build"] != JobSuccess {
		t.Fatalf("result = %q", facts.Results["build"])
	}
	// The union of the cells, with the LAST cell in job-id order winning a
	// collision (deterministic; docs/threads.md §3.9).
	if facts.Outputs["build"]["digest"] != "two" || facts.Outputs["build"]["only1"] != "a" {
		t.Fatalf("merged outputs = %#v", facts.Outputs["build"])
	}
	// A failed cell exposes no outputs at all, even its own successful siblings'.
	byID["j-c2"].State = JobFailure
	facts = foldNeeds(map[string][]string{"build": {"j-c1", "j-c2"}}, byID)
	if facts.Results["build"] != JobFailure || len(facts.Outputs["build"]) != 0 {
		t.Fatalf("failed matrix dependency = %#v, %#v", facts.Results, facts.Outputs)
	}
}

func TestCompleteStoresJobOutputsAndFeedsNeeds(t *testing.T) {
	const yaml = `name: Out
on: [push]
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - id: build
        run: echo build
  gate:
    runs-on: linux
    needs: [build]
    if: needs.build.outputs.digest == 'abc'
    steps:
      - run: echo gate
`
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/out.yml", yaml)
	run := e.mustCreate(t)[0]
	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, 1)
	if err != nil || job == nil || job.JobID != "j-build" {
		t.Fatalf("fetch build = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	claims, err := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "success",
		Outputs:    map[string]string{"digest": "abc"},
	}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	stored, err := e.svc.loadJobCached(ctx(), "o", "r", run.ID, "j-build", nil)
	if err != nil || stored == nil || stored.Outputs["digest"] != "abc" {
		t.Fatalf("stored job = %+v, %v", stored, err)
	}
	// The gate's `if:` read the output and stayed runnable.
	next, ferr := e.svc.FetchJob(ctx(), a, 1)
	if ferr != nil || next == nil || next.JobID != "j-gate" {
		t.Fatalf("gate not claimable: %+v, %v", next, ferr)
	}
	// The payload materializes the dependency's outputs for step references.
	if next.NeedsOutputs["build"]["digest"] != "abc" {
		t.Fatalf("payload needs outputs = %#v", next.NeedsOutputs)
	}
}

func TestFailedDependencyExposesNoOutputs(t *testing.T) {
	const yaml = `name: Out
on: [push]
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - id: build
        run: echo build
  gate:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo gate
`
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/out.yml", yaml)
	run := e.mustCreate(t)[0]
	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, 1)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	// A failed job reporting outputs must have them dropped: a dependent can
	// never read the outputs of a dependency that did not succeed.
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "failure",
		Outputs:    map[string]string{"digest": "leaked"},
	}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	stored, _ := e.svc.loadJobCached(ctx(), "o", "r", run.ID, "j-build", nil)
	if stored == nil || len(stored.Outputs) != 0 {
		t.Fatalf("a failed job kept outputs: %+v", stored)
	}
	gate, _ := e.svc.loadJobCached(ctx(), "o", "r", run.ID, "j-gate", nil)
	if gate == nil || gate.State != JobSkipped || !strings.Contains(gate.Reason, "build concluded failure") {
		t.Fatalf("gate = %+v", gate)
	}
}

func TestCompleteRejectsOversizeOutputs(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec9", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), a, 1)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	_, cerr := e.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "success",
		Outputs:    map[string]string{"blob": strings.Repeat("x", workflow.MaxOutputValueBytes+1)},
	})
	if cerr == nil || !strings.Contains(cerr.Error(), "per value") {
		t.Fatalf("oversize output = %v", cerr)
	}
	// The completion was refused, so the job is still running (the lease
	// requeues it rather than storing a bad record).
	stored, _ := e.svc.loadJobCached(ctx(), "o", "r", run.ID, job.JobID, nil)
	if stored == nil || stored.State != JobRunning {
		t.Fatalf("job = %+v", stored)
	}
}

func TestRerunClearsOutputs(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec8", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), a, 1)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "success", Outputs: map[string]string{"x": "y"},
	}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	admin := auth.Principal{Name: "admin", Admin: true}
	if _, rerr := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); rerr != nil {
		t.Fatalf("rerun: %v", rerr)
	}
	after, _ := e.svc.loadJobCached(ctx(), "o", "r", run.ID, job.JobID, nil)
	if after == nil || len(after.Outputs) != 0 {
		t.Fatalf("outputs survived a rerun: %+v", after)
	}
}

func TestSubstituteOutputWrappers(t *testing.T) {
	steps := map[string]map[string]string{"a": {"x": "one"}}
	needs := map[string]map[string]string{"build": {"y": "two"}}
	if got, err := SubstituteStepOutputs("${{ steps.a.outputs.x }}", steps); err != nil || got != "one" {
		t.Fatalf("step = %q, %v", got, err)
	}
	if got, err := SubstituteNeedsOutputs("${{ needs.build.outputs.y }}", needs); err != nil || got != "two" {
		t.Fatalf("needs = %q, %v", got, err)
	}
	if got, err := SubstituteOutputs("${{ steps.a.outputs.x }}-${{ needs.build.outputs.y }}", steps, needs); err != nil || got != "one-two" {
		t.Fatalf("both = %q, %v", got, err)
	}
	if _, err := SubstituteStepOutputs("${{ steps.ghost.outputs.x }}", steps); err == nil {
		t.Fatal("an unknown step must fail")
	}
	if err := ValidateOutputs(map[string]string{"x": "y"}); err != nil {
		t.Fatalf("ValidateOutputs: %v", err)
	}
}

func TestNeedsOutputConditionSkipsDependent(t *testing.T) {
	const yaml = `name: Out
on: [push]
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - id: build
        run: echo build
  gate:
    runs-on: linux
    needs: [build]
    if: needs.build.outputs.digest == 'other'
    steps:
      - run: echo gate
`
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/out.yml", yaml)
	run := e.mustCreate(t)[0]
	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, 1)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "success", Outputs: map[string]string{"digest": "abc"},
	}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	gate, _ := e.svc.loadJobCached(ctx(), "o", "r", run.ID, "j-gate", nil)
	if gate == nil || gate.State != JobSkipped || !strings.Contains(gate.Reason, "needs.build.outputs.digest") {
		t.Fatalf("gate = %+v", gate)
	}
	if next, _ := e.svc.FetchJob(ctx(), a, 1); next != nil {
		t.Fatalf("a skipped dependent was claimable: %+v", next)
	}
}
