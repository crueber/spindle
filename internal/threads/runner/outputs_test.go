package runner

// Step and job outputs (docs/threads.md §3.9): the env-file format, the
// collection argv, the substitution contexts, fail-closed references, the caps
// and the job-level `outputs:` evaluation.

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestParseOutputFile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"plain", "digest=abc\n", map[string]string{"digest": "abc"}},
		{"equals in value", "url=https://x/?a=b=c\n", map[string]string{"url": "https://x/?a=b=c"}},
		{"empty value", "flag=\n", map[string]string{"flag": ""}},
		{"blank lines and CRLF", "\r\n digest=abc \r\n\r\n", map[string]string{"digest": "abc "}},
		{"heredoc", "body<<EOF\nline one\nline two\nEOF\n", map[string]string{"body": "line one\nline two"}},
		{"heredoc with blank lines", "body<<END\na\n\nb\nEND\n", map[string]string{"body": "a\n\nb"}},
		{"heredoc value containing =", "body<<EOF\na=b\nEOF", map[string]string{"body": "a=b"}},
		{"duplicate keeps last", "x=first\nx=second\n", map[string]string{"x": "second"}},
		{"heredoc duplicate keeps last", "x<<EOF\na\nEOF\nx=after\n", map[string]string{"x": "after"}},
		{"no trailing newline", "x=y", map[string]string{"x": "y"}},
		{"empty file", "", map[string]string{}},
	}
	for _, tc := range cases {
		got, err := parseOutputFile([]byte(tc.in))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.name, k, got[k], v)
			}
		}
	}
}

func TestParseOutputFileErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bad line", "this is not an assignment\n", "neither name=value nor a name<<DELIMITER heredoc"},
		{"bad name", "bad name=value\n", "invalid output name"},
		{"empty heredoc delimiter", "x<<\n", "no delimiter"},
		{"unterminated heredoc", "x<<EOF\na\n", "never closed"},
	}
	for _, tc := range cases {
		_, err := parseOutputFile([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestParseOutputFileCaps(t *testing.T) {
	var many strings.Builder
	for i := 0; i < workflow.MaxOutputsPerStep+1; i++ {
		many.WriteString("k" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + "=v\n")
	}
	if _, err := parseOutputFile([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "over the cap of") {
		t.Fatalf("count cap: %v", err)
	}
	big := "blob=" + strings.Repeat("x", workflow.MaxOutputValueBytes+1) + "\n"
	if _, err := parseOutputFile([]byte(big)); err == nil || !strings.Contains(err.Error(), "per value") {
		t.Fatalf("value cap: %v", err)
	}
}

func TestOutputEnvReachesEveryStep(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "one", Run: "echo hi"}}
	plan, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(plan.Steps[0].Argv, " ")
	for _, want := range []string{
		"SPINDLE_OUTPUT=" + outputFilePath(0),
		"GITHUB_OUTPUT=" + outputFilePath(0),
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("step argv misses %q: %s", want, argv)
		}
	}
	if plan.Steps[0].OutputPath != "/tmp/spindle-output-1" {
		t.Fatalf("output path = %q", plan.Steps[0].OutputPath)
	}
}

// TestExecuteStepOutputsFlow pins the whole in-job chain: step A writes an
// output file, step B's `if:` and `run` read it through `steps.a...`, and the
// job's declared outputs are resolved for the complete payload.
func TestExecuteStepOutputsFlow(t *testing.T) {
	docker := &fakeDocker{runOutputs: []runOutput{
		{match: "spindle-output-1", out: "digest=abc123\nbody<<EOF\nline one\nline two\nEOF\n"},
	}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Matrix = nil
	job.Spec.Outputs = map[string]string{
		"digest": "${{ steps.build.outputs.digest }}",
		"body":   "${{ steps.build.outputs.body }}",
	}
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make"},
		{Name: "gate", Run: "echo gated", If: "steps.build.outputs.digest == 'abc123'"},
		{Name: "use", Run: "echo digest=${{ steps.build.outputs.digest }}"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s); steps=%+v", res.Conclusion, res.Error, res.Steps)
	}
	for _, step := range res.Steps {
		if step.Conclusion != threads.JobSuccess {
			t.Fatalf("step %q = %+v", step.Name, step)
		}
	}
	if !docker.called("echo digest=abc123") {
		t.Fatalf("step-output substitution missing from argv: %v", docker.snapshot())
	}
	if !docker.called("head -c 1048577") {
		t.Fatalf("collection argv missing: %v", docker.snapshot())
	}
	if res.Outputs["digest"] != "abc123" || res.Outputs["body"] != "line one\nline two" {
		t.Fatalf("job outputs = %#v", res.Outputs)
	}
}

// TestExecuteUnresolvedOutputFailsTheStep pins fail-closed substitution: an
// output the step never wrote is a named error, not an empty string.
func TestExecuteUnresolvedOutputFailsTheStep(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make"},
		{Name: "use", Run: "echo ${{ steps.build.outputs.missing }}"},
	}
	res := executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "did not publish the output") {
		t.Fatalf("res = %+v", res)
	}
	if res.Steps[1].Conclusion != threads.JobFailure || !strings.Contains(res.Steps[1].Error, `steps.build.outputs.missing`) {
		t.Fatalf("step error = %+v", res.Steps[1])
	}
}

// TestExecuteFailedStepOutputsAreNotSet pins the failure rule: a step that
// failed published nothing, so a later read fails naming the step.
func TestExecuteFailedStepOutputsAreNotSet(t *testing.T) {
	docker := &fakeDocker{
		streamByMatch: []runOutput{{match: "make", code: 2}},
		runOutputs:    []runOutput{{match: "spindle-output-1", out: "digest=leaked\n"}},
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make", ContinueOnError: true},
		{Name: "after", Run: "echo ${{ steps.build.outputs.digest }}", ContinueOnError: true},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("tolerated failure must keep the job green: %+v", res)
	}
	if docker.called("head -c") && docker.called("/tmp/spindle-output-1'; then") {
		t.Fatalf("a failed step's output file was read: %v", docker.snapshot())
	}
	if res.Steps[1].Conclusion != threads.JobFailure || !strings.Contains(res.Steps[1].Error, "did not publish") {
		t.Fatalf("after step = %+v", res.Steps[1])
	}
}

// TestExecuteUnknownStepOutputIsNamed pins the typo case (an id that does not
// exist) in the `if:` evaluator.
func TestExecuteUnknownStepOutputIsNamed(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make"},
		{Name: "gate", Run: "echo hi", If: "steps.ghost.outputs.x == 'y'"},
	}
	res := executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "no step id") {
		t.Fatalf("res = %+v", res)
	}
}

// TestExecuteJobOutputCaps pins the per-step value/count caps and the per-job
// byte cap as named step failures, so a runaway job cannot fill the record.
func TestExecuteJobOutputCaps(t *testing.T) {
	// Nine 64 KiB values per step are under the per-value and per-step caps but
	// two steps exceed the 1 MiB per-job total.
	huge := strings.Repeat("x", workflow.MaxOutputValueBytes)
	var file strings.Builder
	for i := 0; i < 9; i++ {
		file.WriteString("k" + string(rune('a'+i)) + "=" + huge + "\n")
	}
	docker := &fakeDocker{runOutputs: []runOutput{
		{match: "spindle-output-1", out: file.String()},
		{match: "spindle-output-2", out: file.String()},
	}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "a", Name: "a", Run: "one"},
		{ID: "b", Name: "b", Run: "two"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "bytes per job") {
		t.Fatalf("res = %+v", res)
	}
}

// TestResolveStepHandlesEverySubstitutionSite pins that env, with,
// working-directory and name all resolve a step output (not just run).
func TestResolveStepHandlesEverySubstitutionSite(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make"},
		{
			Name:             "uses ${{ steps.build.outputs.digest }}",
			Uses:             "actions/upload-artifact",
			WorkingDirectory: "out/${{ steps.build.outputs.digest }}",
			Env:              map[string]string{"DIGEST": "${{ steps.build.outputs.digest }}"},
			With:             map[string]string{"name": "art-${{ steps.build.outputs.digest }}", "path": "dist"},
		},
	}
	plan, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]map[string]string{"build": {"digest": "abc"}}
	resolved, err := plan.ResolveStep(plan.Steps[1], outputs)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "uses abc" || resolved.WorkingDirectory != "/spindle/workspace/out/abc" {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.Artifact == nil || resolved.Artifact.Name != "art-abc" {
		t.Fatalf("with: %+v", resolved.Artifact)
	}
	if !strings.Contains(strings.Join(resolved.Argv, " "), "DIGEST=") {
		// Artifact steps have no argv; the env path is exercised by a run step.
		t.Log("artifact step carries no argv (expected)")
	}
	// A run step carries the substituted env and every output-file variable.
	job.Spec.Steps[1] = threads.StepSpec{
		ID: "use", Name: "use", Run: "printenv DIGEST",
		Env: map[string]string{"DIGEST": "${{ steps.build.outputs.digest }}"},
	}
	plan, err = BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = plan.ResolveStep(plan.Steps[1], outputs)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(resolved.Argv, " ")
	if !strings.Contains(argv, "DIGEST=abc") || !strings.Contains(argv, "SPINDLE_OUTPUT=") {
		t.Fatalf("run argv = %s", argv)
	}
}

// TestResolveStepNeedsOutputs pins a dependent job's step reading
// `${{ needs.<job>.outputs.<name> }}` from the claim payload.
func TestResolveStepNeedsOutputs(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.NeedsOutputs = map[string]map[string]string{"build": {"digest": "abc"}}
	job.Spec.Steps = []threads.StepSpec{
		{Name: "use", Run: "echo ${{ needs.build.outputs.digest }}"},
		{Name: "bad", Run: "echo ${{ needs.build.outputs.nope }}", ContinueOnError: true},
	}
	plan, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := plan.ResolveStep(plan.Steps[0], nil)
	if err != nil || !strings.Contains(strings.Join(resolved.Argv, " "), "echo abc") {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	if _, err := plan.ResolveStep(plan.Steps[1], nil); err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("unset needs output = %v", err)
	}
}

// TestCollectStepOutputsReadFailure pins a docker read failure as a named error.
func TestCollectStepOutputsReadFailure(t *testing.T) {
	docker := &fakeDocker{runOutputs: []runOutput{{match: "spindle-output-1", out: "denied", code: 126}}}
	exec := executorWith(docker)
	_, err := exec.collectStepOutputs(context.Background(), &Plan{Names: Names{Container: "c"}}, StepPlan{ID: "a", OutputPath: "/tmp/spindle-output-1"})
	if err == nil || !strings.Contains(err.Error(), "could not read the step's output file") {
		t.Fatalf("err = %v", err)
	}
	// A hand-built step with no path reads nothing.
	if outs, err := exec.collectStepOutputs(context.Background(), &Plan{}, StepPlan{ID: "a"}); err != nil || outs != nil {
		t.Fatalf("no path = %v, %v", outs, err)
	}
}

// TestEvaluateJobOutputsFailsClosed pins a job output referencing an output the
// step never wrote: the job fails naming the output rather than reporting "".
func TestEvaluateJobOutputsFailsClosed(t *testing.T) {
	plan := &Plan{OutputTemplates: map[string]string{"x": "${{ steps.a.outputs.nope }}"}}
	steps := newStepOutcomes()
	steps.record(StepPlan{ID: "a"}, threads.StepResult{Conclusion: threads.JobSuccess})
	if outs, err := evaluateJobOutputs(plan, steps); err == nil || !strings.Contains(err.Error(), `job output "x"`) {
		t.Fatalf("outs = %v, err = %v", outs, err)
	}
	if outs, err := evaluateJobOutputs(nil, steps); outs != nil || err != nil {
		t.Fatalf("no templates = %v, %v", outs, err)
	}
}

// TestParseOutputFileLongBadLine pins the truncation of a bad line in the
// error message (a binary or huge line must not blow up the log frame).
func TestParseOutputFileLongBadLine(t *testing.T) {
	line := strings.Repeat("z", 300)
	_, err := parseOutputFile([]byte(line + "\n"))
	if err == nil || !strings.Contains(err.Error(), "…") {
		t.Fatalf("long bad line = %v", err)
	}
}

// TestCollectOutputsWrapper pins collectStepOutputs' parse-error path and the
// nil context helper.
func TestCollectOutputsWrapper(t *testing.T) {
	if outputContext(nil) != nil {
		t.Fatal("nil steps must yield a nil output context")
	}
	docker := &fakeDocker{runOutputs: []runOutput{{match: "spindle-output-1", out: "not an assignment\n"}}}
	exec := executorWith(docker)
	_, err := exec.collectStepOutputs(context.Background(), &Plan{Names: Names{Container: "c"}}, StepPlan{ID: "a", OutputPath: "/tmp/spindle-output-1"})
	if err == nil || !strings.Contains(err.Error(), "neither name=value") {
		t.Fatalf("parse error = %v", err)
	}
	// An oversized read is refused at the byte cap.
	docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-output-1", out: strings.Repeat("x", workflow.MaxOutputBytes+1)}}}
	exec = executorWith(docker)
	if _, err := exec.collectStepOutputs(context.Background(), &Plan{Names: Names{Container: "c"}}, StepPlan{ID: "a", OutputPath: "/tmp/spindle-output-1"}); err == nil || !strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("oversized file = %v", err)
	}
}

// TestStepOutcomeOutputCaps pins the per-job count cap and the merge bookkeeping.
func TestStepOutcomeOutputCaps(t *testing.T) {
	steps := newStepOutcomes()
	steps.record(StepPlan{ID: "a"}, threads.StepResult{Conclusion: threads.JobSuccess})
	outs := map[string]string{}
	for i := 0; i < workflow.MaxOutputsPerJob; i++ {
		outs["k"+string(rune('a'+i%26))+strings.Repeat("z", i/26)] = "v"
	}
	if err := steps.addOutputs("a", outs); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if err := steps.addOutputs("a", map[string]string{"one-more": "v"}); err == nil || !strings.Contains(err.Error(), "per job") {
		t.Fatalf("over the count cap = %v", err)
	}
}

// TestResolveStepSubstitutionErrors pins the named error for an unresolvable
// reference in name/env/with (not just run).
func TestResolveStepSubstitutionErrors(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "build", Name: "build", Run: "make"},
		{Name: "n ${{ steps.build.outputs.nope }}", Run: "echo hi"},
		{Name: "e", Run: "echo hi", Env: map[string]string{"X": "${{ steps.build.outputs.nope }}"}},
		{Name: "w", Uses: "actions/upload-artifact", With: map[string]string{"name": "${{ steps.build.outputs.nope }}", "path": "dist"}},
	}
	plan, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]map[string]string{"build": {}}
	for _, idx := range []int{1, 2, 3} {
		if _, rerr := plan.ResolveStep(plan.Steps[idx], outputs); rerr == nil || !strings.Contains(rerr.Error(), "did not publish") {
			t.Errorf("step %d resolve error = %v", idx, rerr)
		}
	}
}

// TestEvaluateJobOutputsCompositeCap pins a template whose concatenation
// exceeds the per-value cap: the job output evaluation refuses it.
func TestEvaluateJobOutputsCompositeCap(t *testing.T) {
	half := strings.Repeat("x", workflow.MaxOutputValueBytes/2+1)
	plan := &Plan{OutputTemplates: map[string]string{"big": "${{ steps.a.outputs.one }}${{ steps.a.outputs.two }}"}}
	steps := newStepOutcomes()
	steps.record(StepPlan{ID: "a"}, threads.StepResult{Conclusion: threads.JobSuccess})
	steps.Outputs["a"] = map[string]string{"one": half, "two": half}
	if _, err := evaluateJobOutputs(plan, steps); err == nil || !strings.Contains(err.Error(), "per value") {
		t.Fatalf("composite cap = %v", err)
	}
}
