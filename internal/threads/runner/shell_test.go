package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Shell parity (PAR-079): a custom command with {0}, the bash→sh fallback with
// its log line, and the script file the custom form needs.

func shellJob(shell, script string) *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "s", Run: script, Shell: shell}}
	return job
}

// TestCustomShellWritesTheScriptFile pins the `shell: dash -e {0}` form: the
// script is written to a per-step file inside the container and the argv carries
// the substituted path, never the script as one huge argument.
func TestCustomShellWritesTheScriptFile(t *testing.T) {
	d := &fakeDocker{}
	stream, _ := recordingStream(t)
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: shellJob("dash -e {0}", "echo hi"), AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !d.called("exec -i sp-j-") || !d.called("cat > '/tmp/spindle-step-1'") {
		t.Fatalf("the script was not written: %v", d.snapshot())
	}
	if !d.called("dash -e /tmp/spindle-step-1") {
		t.Fatalf("argv = %v", d.snapshot())
	}
	wrote := false
	for _, sink := range d.streamInSinks {
		if strings.Contains(sink.String(), "echo hi") {
			wrote = true
		}
	}
	if !wrote {
		t.Fatalf("the script body never reached the container: %v", d.streamInSinks)
	}
}

// TestBashFallsBackToSh pins GitHub's Linux fallback: an image without bash runs
// the step with `sh -e` and says so once, instead of failing every bash step
// with exit 127.
func TestBashFallsBackToSh(t *testing.T) {
	d := &fakeDocker{runOutputs: []runOutput{{match: "command -v 'bash'", code: 1}}}
	stream, frames := recordingStream(t)
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: shellJob("bash", "echo hi"), AttemptToken: "wjt_token_value"}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !d.called(" sh -e -c echo hi") {
		t.Fatalf("fallback argv = %v", d.snapshot())
	}
	if !strings.Contains(frames(), "bash is not installed in this image; step \"s\" falls back to sh -e") {
		t.Fatalf("log = %q", frames())
	}
	// bash present keeps the documented pipefail argv.
	d2 := &fakeDocker{}
	stream2 := quietStream(t)
	if res := executorWith(d2).Execute(context.Background(), ExecInput{Job: shellJob("bash", "echo hi"), AttemptToken: "wjt_token_value"}, stream2); res.Conclusion != threads.JobSuccess {
		t.Fatalf("bash conclusion = %s", res.Conclusion)
	}
	if !d2.called("bash --noprofile --norc -eo pipefail -c echo hi") {
		t.Fatalf("bash argv = %v", d2.snapshot())
	}
	if !d2.called("command -v 'bash'") {
		t.Fatalf("no probe: %v", d2.snapshot())
	}
}

// TestBashFallbackKeepsThePathWrapper pins the interaction order: the fallback
// rebuilds the command tail BEFORE $GITHUB_PATH wraps it, so a bash step after
// a PATH write keeps both the fallback and the prepended directories.
func TestBashFallbackKeepsThePathWrapper(t *testing.T) {
	d := &fakeDocker{runOutputs: []runOutput{
		{match: "command -v 'bash'", code: 1},
		{match: "spindle-command-1-path", out: "/opt/tools/bin\n"},
	}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "write path", Run: "echo path", Shell: "sh"},
		{Name: "bash step", Run: "echo hi", Shell: "bash"},
	}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !d.called(`PATH="$0:$PATH"`) || !d.called("sh -e -c echo hi") {
		t.Fatalf("the PATH wrapper or the fallback was lost: %v", d.snapshot())
	}
}

// TestCustomShellWithoutPlaceholderIsRefused: a template the runner cannot
// honour is a named step failure, never a guess.
func TestCustomShellWithoutPlaceholderIsRefused(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: shellJob("dash -e", "echo hi")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "unsupported shell") {
		t.Fatalf("step = %+v", p.Steps[0])
	}
}

// TestDefaultShellIsBashWithTheShFallback pins the default-shell parity rule:
// GitHub and Forgejo run a step with no authored `shell:` under bash, and a
// real corpus workflow relies on that (the publish entry's `<<<` here-string).
// An image without bash still runs, with the documented `sh -e` fallback and a
// log line — never a silent interpreter switch.
func TestDefaultShellIsBashWithTheShFallback(t *testing.T) {
	d := &fakeDocker{}
	stream := quietStream(t)
	if res := executorWith(d).Execute(context.Background(), ExecInput{Job: shellJob("", "echo hi"), AttemptToken: "wjt_token_value"}, stream); res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if !d.called("bash --noprofile --norc -eo pipefail -c echo hi") {
		t.Fatalf("the default step did not run under bash: %v", d.snapshot())
	}

	d2 := &fakeDocker{runOutputs: []runOutput{{match: "command -v 'bash'", code: 1}}}
	stream2, frames := recordingStream(t)
	if res := executorWith(d2).Execute(context.Background(), ExecInput{Job: shellJob("", "echo hi"), AttemptToken: "wjt_token_value"}, stream2); res.Conclusion != threads.JobSuccess {
		t.Fatalf("fallback conclusion = %s", res.Conclusion)
	}
	stream2.Close()
	if !d2.called(" sh -e -c echo hi") {
		t.Fatalf("fallback argv = %v", d2.snapshot())
	}
	if !strings.Contains(frames(), "bash is not installed in this image") {
		t.Fatalf("the fallback was silent: %q", frames())
	}
}
