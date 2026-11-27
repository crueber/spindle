package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Edge coverage for local actions: the runner's refusals, the caller's env /
// working directory / timeout inheritance, nested-action output chaining, and
// a docker action's output edge cases.

func TestBuildPlanActionPlanEdges(t *testing.T) {
	// An undeclared input on a local docker action.
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./a", With: map[string]string{"nope": "x"}}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		"a": {Path: "a", Using: "docker", Image: "alpine:3.20", Inputs: map[string]threads.ActionInputSpec{"real": {}}},
	}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), `"nope" is not declared`) {
		t.Fatalf("docker with key = %v", err)
	}
	// An action the runner cannot execute.
	job.Spec.Steps = []threads.StepSpec{{Uses: "./a"}}
	job.Spec.Actions["a"] = threads.ActionSpec{Path: "a", Using: "node20"}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "unsupported using") {
		t.Fatalf("using = %v", err)
	}
	// A docker action with no image.
	job.Spec.Actions["a"] = threads.ActionSpec{Path: "a", Using: "docker"}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "has no image") {
		t.Fatalf("empty image = %v", err)
	}
}

// TestBuildPlanCompositeInheritance pins what a composite's inner steps
// inherit: the caller's env and working directory, its timeout, and the
// caller's INPUT_* exclusion (a nested action gets its own inputs only).
func TestBuildPlanCompositeInheritance(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{
		Name: "outer", Uses: "./outer", TimeoutMinutes: 4,
		Env:              map[string]string{"FROM_CALLER": "yes", "INPUT_OUTER": "kept-for-runs"},
		WorkingDirectory: "./sub",
	}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		"outer": {Path: "outer", Using: "composite",
			Inputs: map[string]threads.ActionInputSpec{"level": {Default: "deep", HasDefault: true}},
			Steps: []threads.StepSpec{
				{ID: "run", Run: "echo x", Shell: "sh"},
				{ID: "inner", Uses: "./inner"},
			}},
		"inner": {Path: "inner", Using: "composite",
			Inputs: map[string]threads.ActionInputSpec{"own": {Default: "1", HasDefault: true}},
			Steps:  []threads.StepSpec{{ID: "child", Run: "echo y", Shell: "sh"}}},
	}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	caller := p.Steps[0]
	if caller.Timeout != 4*time.Minute {
		t.Fatalf("caller timeout = %v", caller.Timeout)
	}
	run := caller.Composite.Steps[0]
	if run.WorkingDirectory != DefaultWorkspace+"/sub" {
		t.Fatalf("child workdir = %q", run.WorkingDirectory)
	}
	joined := strings.Join(run.Argv, " ")
	for _, want := range []string{"--env FROM_CALLER=yes", "--env INPUT_OUTER=kept-for-runs", "--env INPUT_LEVEL=deep"} {
		if !strings.Contains(joined, want) {
			t.Errorf("child argv missing %q\n%s", want, joined)
		}
	}
	// The nested action's own inputs only: the outer action's INPUT_* names are
	// not its interface.
	nested := caller.Composite.Steps[1]
	nestedRun := nested.Composite.Steps[0]
	nestedArgv := strings.Join(nestedRun.Argv, " ")
	for _, want := range []string{"--env FROM_CALLER=yes", "--env INPUT_OWN=1"} {
		if !strings.Contains(nestedArgv, want) {
			t.Errorf("nested argv missing %q\n%s", want, nestedArgv)
		}
	}
	if strings.Contains(nestedArgv, "INPUT_LEVEL") || strings.Contains(nestedArgv, "INPUT_OUTER") {
		t.Fatalf("a nested action inherited an enclosing INPUT_*: %s", nestedArgv)
	}
	// An inner working-directory is relative to the caller's.
	job.Spec.Actions["outer"] = threads.ActionSpec{Path: "outer", Using: "composite",
		Steps: []threads.StepSpec{{Run: "echo x", Shell: "sh", WorkingDirectory: "build"}}}
	p, err = BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Steps[0].Composite.Steps[0].WorkingDirectory; got != DefaultWorkspace+"/sub/build" {
		t.Fatalf("relative inner workdir = %q", got)
	}
}

// TestExecuteCompositeEdgeFailures pins the runner's fail-closed edges inside a
// composite: an unresolvable reference, an undeclared output, an invalid output
// map, and the internal "no expansion" guard.
func TestExecuteCompositeEdgeFailures(t *testing.T) {
	base := func() *threads.FetchResult {
		job := fixtureJob()
		job.Spec.Services = nil
		job.Checkout = nil
		return job
	}
	// A child field naming a step output that does not exist.
	job := base()
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Steps: []threads.StepSpec{{Run: "echo ${{ steps.nope.outputs.x }}", Shell: "sh"}}}}
	job.Spec.Steps = []threads.StepSpec{{Name: "a", Uses: "./a"}}
	res := executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "no step with id") {
		t.Fatalf("unresolved child = %s / %q", res.Conclusion, res.Error)
	}

	// The action's output names a step that published nothing.
	job = base()
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Outputs: map[string]string{"x": "${{ steps.run.outputs.missing }}"},
		Steps:   []threads.StepSpec{{ID: "run", Run: "true", Shell: "sh"}}}}
	job.Spec.Steps = []threads.StepSpec{{ID: "a", Name: "a", Uses: "./a"}}
	res = executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "did not publish") {
		t.Fatalf("unpublished output = %s / %q", res.Conclusion, res.Error)
	}

	// An invalid output map is refused by ValidateOutputs.
	job = base()
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Outputs: map[string]string{"bad name": "x"},
		Steps:   []threads.StepSpec{{Run: "true", Shell: "sh"}}}}
	job.Spec.Steps = []threads.StepSpec{{ID: "a", Name: "a", Uses: "./a"}}
	res = executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "invalid output name") {
		t.Fatalf("bad output map = %s / %q", res.Conclusion, res.Error)
	}

	// A composite plan with no expansion is an internal failure, never a panic.
	exec := executorWith(&fakeDocker{})
	plan, err := BuildPlan(PlanInput{Job: base()})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := exec.runStep(context.Background(), plan, StepPlan{Index: 0, Kind: "composite", Name: "x"}, quietStream(t), nil, nil, nil, false, newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if out.Conclusion != threads.JobFailure || !strings.Contains(out.Error, "no expansion") {
		t.Fatalf("no expansion = %+v", out)
	}
}

// TestExecuteCompositeToleratedInnerStep pins the inner continue-on-error rule:
// the caller keeps going and the caller's record stays success when every
// failing inner step was tolerated.
func TestExecuteCompositeToleratedInnerStep(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Steps: []threads.StepSpec{
			{ID: "flaky", Run: "exit 4", Shell: "sh", ContinueOnError: true},
			{ID: "after", Run: "echo ok", Shell: "sh"},
		}}}
	job.Spec.Steps = []threads.StepSpec{{ID: "a", Name: "a", Uses: "./a"}}
	d := &fakeDocker{streamByMatch: []runOutput{{match: "exit 4", code: 4}}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobSuccess || res.Steps[1].Conclusion != threads.JobFailure {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if !strings.Contains(strings.Join(d.snapshot(), "\n"), "echo ok") {
		t.Fatal("the tolerated inner failure must not stop the action")
	}
}

// TestExecuteCompositeTimeout pins the composite's own timeout: the job
// container is killed (the only reliable process-tree kill) and the caller
// reports the timeout.
func TestExecuteCompositeTimeout(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Steps: []threads.StepSpec{{Run: "sleep 100", Shell: "sh"}}}}
	job.Spec.Steps = []threads.StepSpec{{ID: "a", Name: "a", Uses: "./a", TimeoutMinutes: 1}}
	d := &fakeDocker{blockOn: "exec --workdir"}
	plan, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps[0].Timeout = 60 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := quietStream(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		// Nothing: the composite's own deadline fires. The parent stays alive.
	}()
	res, _ := executorWith(d).runStep(ctx, plan, plan.Steps[0], stream, nil, newStepOutcomes(), nil, false, newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if res.Conclusion != threads.JobTimedOut || !strings.Contains(res.Error, "timed out after") {
		t.Fatalf("timeout = %+v", res)
	}
	if !d.called("kill " + plan.Names.Container) {
		t.Fatalf("a composite timeout must kill the job container: %v", d.snapshot())
	}
}

// TestExecuteCompositeCancelled pins a cancel during an inner step.
func TestExecuteCompositeCancelled(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{"a": {Path: "a", Using: "composite",
		Steps: []threads.StepSpec{{Run: "sleep 100", Shell: "sh"}}}}
	job.Spec.Steps = []threads.StepSpec{{ID: "a", Name: "a", Uses: "./a"}}
	d := &fakeDocker{blockOn: "exec --workdir"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res := executorWith(d).Execute(ctx, ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobCancelled {
		t.Fatalf("conclusion = %s (%+v)", res.Conclusion, res.Steps)
	}
}

// TestDockerActionFailureAndTimeout pins a docker action's failure path (its
// container is removed) and its own step timeout.
func TestDockerActionFailureAndTimeout(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Steps = []threads.StepSpec{{ID: "img", Name: "img", Uses: "docker://alpine:3.20"}}
	d := &fakeDocker{streamCode: 2}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || res.Steps[0].ExitCode != 2 {
		t.Fatalf("res = %s / %+v", res.Conclusion, res.Steps)
	}
	if !d.called("rm -f sp-a-") {
		t.Fatalf("a failed docker action's container was not removed: %v", d.snapshot())
	}

	// Timeout: the action container is removed (not the job container).
	d = &fakeDocker{blockOn: "run --name sp-a-"}
	plan, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps[0].Timeout = 60 * time.Millisecond
	err = executorWith(d).runStepCommand(context.Background(), plan, plan.Steps[0], quietStream(t), quietStream(t))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout = %v", err)
	}
	if !d.called("rm -f sp-a-") {
		t.Fatalf("a timed-out docker action was not removed: %v", d.snapshot())
	}
}

// TestCollectDockerActionOutputsEdges pins the copy-out edge cases: a missing
// file is "no outputs" (normal), a copy failure is an error, and an oversized
// or malformed file is refused.
func TestCollectDockerActionOutputsEdges(t *testing.T) {
	plan := &Plan{Names: Names{Container: "sp-j-abc"}}
	step := StepPlan{Kind: "docker", OutputIndex: 0, OutputPath: outputFilePath(0)}

	// A copy failure is not "no outputs": it is a named error.
	d := &fakeDocker{runOutputs: []runOutput{{match: "cp ", code: 9, errOut: "daemon down"}}}
	if _, err := executorWith(d).collectDockerOutputs(context.Background(), plan, step); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("copy failure = %v", err)
	}

	// No file: the action published nothing.
	d = &fakeDocker{}
	if outs, err := executorWith(d).collectDockerOutputs(context.Background(), plan, step); err != nil || len(outs) != 0 {
		t.Fatalf("no file = %v %v", outs, err)
	}

	// A malformed file is refused.
	d = &fakeDocker{cpFiles: map[string]string{"spindle-output-1": "not an assignment\n"}}
	if _, err := executorWith(d).collectDockerOutputs(context.Background(), plan, step); err == nil || !strings.Contains(err.Error(), "neither name=value") {
		t.Fatalf("malformed = %v", err)
	}

	// An oversized file is refused before it is read.
	d = &fakeDocker{cpFiles: map[string]string{"spindle-output-1": strings.Repeat("x", workflow.MaxOutputBytes+1)}}
	if _, err := executorWith(d).collectDockerOutputs(context.Background(), plan, step); err == nil || !strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("oversized = %v", err)
	}

	// A docker action with no output id is never collected (the executor skips
	// it), and a composite caller has no output file of its own.
	if step.OutputPath == "" {
		t.Fatal("the fixture must carry an output path")
	}
}

// TestCompositeOutputCaps pins the per-action cap (the same constants as a job).
func TestCompositeOutputCaps(t *testing.T) {
	s := newLocalScope()
	outs := map[string]string{}
	for i := 0; i < workflow.MaxOutputsPerJob; i++ {
		outs["k"+strings.Repeat("x", i)] = "v"
	}
	if err := s.addOutputs("a", outs); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if err := s.addOutputs("a", map[string]string{"one-more": "v"}); err == nil || !strings.Contains(err.Error(), "per action") {
		t.Fatalf("over the cap = %v", err)
	}
	if err := s.addOutputs("", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("an anonymous step's outputs are discarded: %v", err)
	}
	// A huge value set hits the byte cap.
	s2 := newLocalScope()
	big := map[string]string{}
	for i := 0; i < 17; i++ {
		big["o"+strings.Repeat("y", i)] = strings.Repeat("z", workflow.MaxOutputValueBytes)
	}
	if err := s2.addOutputs("a", big); err == nil || !strings.Contains(err.Error(), "bytes per action") {
		t.Fatalf("byte cap = %v", err)
	}
}
