package runner

// Step `id:` in the executor (15 §3.4/§4.1, 2026-11-21): the plan carries the
// id, the executor records `steps.<id>.outcome` / `.conclusion`, and a later
// step's `if:` can branch on an earlier result.

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func TestPlanCarriesStepIDs(t *testing.T) {
	job := fixtureJob()
	job.Spec.Steps = []threads.StepSpec{
		{ID: "Checkout", Uses: "actions/checkout"},
		{Run: "echo hi"}, // anonymous
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Steps[0].ID != "checkout" {
		t.Fatalf("step id = %q, want checkout (lowercased)", p.Steps[0].ID)
	}
	if p.Steps[1].ID != "" {
		t.Fatalf("anonymous step id = %q", p.Steps[1].ID)
	}
}

// TestExecuteStepOutcomesDriveLaterIf pins both branches of
// `if: steps.<id>.outcome == ...` and the continue-on-error conclusion.
func TestExecuteStepOutcomesDriveLaterIf(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "run-lint", code: 1}}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "lint", Name: "lint", Run: "run-lint", ContinueOnError: true},
		{ID: "on-failure", Name: "on-failure", Run: "echo recovered", If: "steps.lint.outcome == 'failure'"},
		{ID: "on-success", Name: "on-success", Run: "echo green", If: "steps.lint.outcome == 'success'"},
		{ID: "concluded", Name: "concluded", Run: "echo concluded", If: "steps.lint.conclusion == 'success'"},
		{ID: "outputs", Name: "outputs", Run: "echo outputs", If: "steps.lint.outputs.digest == ''"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	want := map[string]string{
		"lint":       threads.JobFailure,
		"on-failure": threads.JobSuccess,
		"on-success": "skipped",
		"concluded":  threads.JobSuccess, // conclusion is green: tolerated failure
		"outputs":    threads.JobSuccess, // a known step's unset output reads empty
	}
	if len(res.Steps) != len(want) {
		t.Fatalf("steps = %+v", res.Steps)
	}
	for _, step := range res.Steps {
		if got := want[step.ID]; step.Conclusion != got {
			t.Errorf("step %s conclusion = %q, want %q (all: %+v)", step.ID, step.Conclusion, got, res.Steps)
		}
	}
	// The tolerated failure keeps the job green.
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("job conclusion = %s (%s)", res.Conclusion, res.Error)
	}
}

// TestStepOutcomeAcrossAHardFailure pins the skipped remainder's outcome: a
// later step is recorded `skipped` under its id even though it never ran.
func TestStepOutcomeAcrossAHardFailure(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "run-boom", code: 2}}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "boom", Name: "boom", Run: "run-boom"},
		{ID: "tail", Name: "tail", Run: "echo tail"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || len(res.Steps) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if res.Steps[0].ID != "boom" || res.Steps[0].Conclusion != threads.JobFailure {
		t.Fatalf("boom = %+v", res.Steps[0])
	}
	if res.Steps[1].ID != "tail" || res.Steps[1].Conclusion != "skipped" {
		t.Fatalf("tail = %+v", res.Steps[1])
	}
}

// TestStepOutcomesRecordAnonymousStepsAreUnaddressable pins the map guard.
func TestStepOutcomesRecordAnonymousStepsAreUnaddressable(t *testing.T) {
	o := newStepOutcomes()
	o.record(StepPlan{}, threads.StepResult{Conclusion: threads.JobSuccess})
	if len(o.Outcome) != 0 || len(o.Conclusion) != 0 {
		t.Fatalf("anonymous step recorded: %+v", o)
	}
	o.record(StepPlan{ID: "x", ContinueOnError: true}, threads.StepResult{Conclusion: threads.JobFailure})
	if o.Outcome["x"] != threads.JobFailure || o.Conclusion["x"] != threads.JobSuccess {
		t.Fatalf("tolerated step = %+v", o)
	}
}
