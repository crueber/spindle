package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Step summary (PAR-092): collection, caps, and the end-to-end path from a
// step's `$GITHUB_STEP_SUMMARY` file to the job result.

func TestJobSummaryCaps(t *testing.T) {
	var s jobSummary
	if err := s.add("one", "a\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.add("two", "b\n"); err != nil {
		t.Fatal(err)
	}
	if got := s.text(); got != "a\n\nb\n" {
		t.Fatalf("text = %q", got)
	}
	if err := s.add("big", strings.Repeat("x", MaxStepSummaryBytes+1)); err == nil {
		t.Fatal("an oversized step summary was accepted")
	}
	// A step that writes nothing adds nothing.
	if err := s.add("empty", " \n"); err != nil || s.entries != 2 {
		t.Fatalf("empty add = %v entries=%d", err, s.entries)
	}
	// The count cap.
	for s.entries < MaxStepSummaries {
		if err := s.add("x", "x\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.add("one-too-many", "x\n"); err == nil {
		t.Fatal("the step-summary count cap was not enforced")
	}
}

func TestCollectStepSummary(t *testing.T) {
	plan := &Plan{Names: Names{Container: "sp-j-x"}}
	step := StepPlan{Index: 0, Name: "build", SummaryPath: "/tmp/spindle-summary-1"}
	stream := quietStream(t)

	d := &fakeDocker{runOutputs: []runOutput{{match: "spindle-summary-1", out: "# Build\nall good\n"}}}
	var s jobSummary
	if err := executorWith(d).collectStepSummary(context.Background(), plan, step, &s, stream); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := s.text(); got != "# Build\nall good\n" {
		t.Fatalf("summary = %q", got)
	}

	// No file at all is normal (exit 3 from the probe).
	missing := &fakeDocker{runOutputs: []runOutput{{match: "spindle-summary-1", code: 3}}}
	var s2 jobSummary
	if err := executorWith(missing).collectStepSummary(context.Background(), plan, step, &s2, stream); err != nil {
		t.Fatalf("missing summary = %v", err)
	}
	if s2.text() != "" {
		t.Fatalf("missing summary = %q", s2.text())
	}

	// An oversized file is a named error.
	big := &fakeDocker{runOutputs: []runOutput{{match: "spindle-summary-1", out: strings.Repeat("x", MaxStepSummaryBytes+1)}}}
	if err := executorWith(big).collectStepSummary(context.Background(), plan, step, &s2, stream); err == nil ||
		!strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("oversized summary = %v", err)
	}
}

// TestExecuteCollectsSummaryAndAnnotations drives the whole job: a step's
// command output becomes a masked annotation, and its summary file becomes the
// job's summary — both on the Result the runner reports on complete.
func TestExecuteCollectsSummaryAndAnnotations(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "build", Run: "echo hi", Shell: "sh"}}
	d := &fakeDocker{
		streamOutput: "::error file=main.go,line=7,title=Build::s3cr3t-value broke\n",
		runOutputs:   []runOutput{{match: "spindle-summary-", out: "## Result\n\n```mermaid\ngraph TD; A-->B;\n```\n"}},
	}
	stream := quietStream(t)
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if len(res.Annotations) != 1 {
		t.Fatalf("annotations = %+v", res.Annotations)
	}
	a := res.Annotations[0]
	if a.Level != "error" || a.File != "main.go" || a.Line != 7 || a.Title != "Build" || a.Message != "*** broke" {
		t.Fatalf("annotation = %+v", a)
	}
	if !strings.Contains(res.Summary, "## Result") || !strings.Contains(res.Summary, "mermaid") {
		t.Fatalf("summary = %q", res.Summary)
	}
}

// TestExecuteSummaryOnAFailedStep pins that a failing step's summary is still
// collected (it is often the explanation) but a summary read error does not
// replace the step's own failure.
func TestExecuteSummaryOnAFailedStep(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "build", Run: "exit 1", Shell: "sh"}}
	d := &fakeDocker{
		streamCode: 1,
		runOutputs: []runOutput{{match: "spindle-summary-", out: "the test failed because X\n"}},
	}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if !strings.Contains(res.Summary, "test failed because X") {
		t.Fatalf("failed step's summary = %q", res.Summary)
	}
	if !strings.Contains(res.Error, "exit code 1") {
		t.Fatalf("error = %q", res.Error)
	}
}
