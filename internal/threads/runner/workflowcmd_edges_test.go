package runner

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Edge branches of the workflow-command sink: every one is a bounded-input or
// typo path that must be NAMED, never a silent drop.

func newSinkForTest(t *testing.T) (*commandSink, *jobCommands, *markerBuffer) {
	t.Helper()
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Index: 0, ID: "build", Name: "build"})
	return sink, job, &dst
}

func TestCommandSinkEdgeCommands(t *testing.T) {
	sink, job, dst := newSinkForTest(t)
	_, _ = sink.Write([]byte(strings.Join([]string{
		"::endgroup::",
		"::stop-commands::",
		"::debug::a debug line",
		"::set-env name=X::y",
		"::add-path::/nope",
		"::set-output::no-name",
		"::set-output name=bad name::v",
		"::add-mask::abc",
		"",
	}, "\n")))
	_ = sink.Close()
	text := dst.String()
	for _, want := range []string{
		"::endgroup:: with no open group",
		"needs a resume token",
		"a debug line",
		"::set-env:: was removed",
		"::add-path:: was removed",
		"needs a name property",
		"is not a valid output name",
		"shorter than 4 bytes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("log misses %q:\n%s", want, text)
		}
	}
	if len(job.annotations) != 0 {
		t.Fatalf("annotations = %+v", job.annotations)
	}
}

func TestCommandSinkSetOutputCaps(t *testing.T) {
	sink, _, dst := newSinkForTest(t)
	for i := 0; i <= workflow.MaxOutputsPerStep; i++ {
		_, _ = sink.Write([]byte("::set-output name=o" + strconv.Itoa(i) + "::v\n"))
	}
	_ = sink.Close()
	if !strings.Contains(dst.String(), "wrote more than") {
		t.Fatalf("the output count cap was not named:\n%s", dst.String())
	}
}

func TestCommandSinkMaskCap(t *testing.T) {
	sink, job, dst := newSinkForTest(t)
	for i := 0; i < MaxMaskValues+1; i++ {
		_, _ = sink.Write([]byte("::add-mask::value-" + strings.Repeat("x", i+1) + "\n"))
	}
	_ = sink.Close()
	if job.masks != MaxMaskValues {
		t.Fatalf("masks = %d, want %d", job.masks, MaxMaskValues)
	}
	if !strings.Contains(dst.String(), "over the cap of 128 masks") {
		t.Fatalf("the mask cap was not named:\n%s", dst.String())
	}
}

func TestCommandSinkAnnotationCaps(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Name: "s"})
	// Long message/title/file are truncated to the documented caps.
	_, _ = sink.Write([]byte("::error file=" + strings.Repeat("f", threads.MaxAnnotationFile+10) +
		",title=" + strings.Repeat("t", threads.MaxAnnotationTitle+10) +
		"::" + strings.Repeat("m", threads.MaxAnnotationMessage+10) + "\n"))
	// A line number beyond the ceiling is clamped.
	_, _ = sink.Write([]byte("::notice file=x.go,line=99999999::hi\n"))
	_ = sink.Close()
	if len(job.annotations) != 2 {
		t.Fatalf("annotations = %d", len(job.annotations))
	}
	a := job.annotations[0]
	if len(a.File) != threads.MaxAnnotationFile || len(a.Title) != threads.MaxAnnotationTitle || len(a.Message) != threads.MaxAnnotationMessage {
		t.Fatalf("caps: file=%d title=%d message=%d", len(a.File), len(a.Title), len(a.Message))
	}
	if job.annotations[1].Line != 1_000_000 {
		t.Fatalf("line clamp = %d", job.annotations[1].Line)
	}
	// The per-JOB cap holds across steps (each step has its own sink).
	for i := 0; i < threads.MaxAnnotationsPerJob; i++ {
		s := newCommandSink(&dst, job, StepPlan{Name: "s2"})
		_, _ = s.Write([]byte("::warning::w\n"))
		_ = s.Close()
	}
	if len(job.annotations) != threads.MaxAnnotationsPerJob {
		t.Fatalf("job annotations = %d, want %d", len(job.annotations), threads.MaxAnnotationsPerJob)
	}
}

func TestCommandSinkLongLineAndUnclosedStop(t *testing.T) {
	sink, job, dst := newSinkForTest(t)
	// A line over the buffer bound is forwarded as text and never parsed.
	_, _ = sink.Write([]byte("::error::" + strings.Repeat("x", MaxCommandLineBytes+10)))
	if !strings.Contains(dst.String(), "::error::xxx") {
		t.Fatal("the over-long line was not forwarded")
	}
	// The rest of that line belongs to the text too.
	_, _ = sink.Write([]byte("tail\n"))
	if !strings.Contains(dst.String(), "tail\n") {
		t.Fatal("the tail of the over-long line was not forwarded")
	}
	if len(job.annotations) != 0 {
		t.Fatalf("an over-long line produced an annotation: %+v", job.annotations)
	}
	// A trailing partial line and an un-resumed stop are both closed.
	_, _ = sink.Write([]byte("::group::open\n::stop-commands::tok\n::notice::last line without newline"))
	_ = sink.Close()
	text := dst.String()
	if !strings.Contains(text, "never resumed") {
		t.Fatalf("unclosed stop-commands was not named:\n%s", text)
	}
	if !strings.Contains(text, "notice::last line without newline") {
		t.Fatalf("the stopped trailing line was not printed:\n%s", text)
	}
	if !strings.Contains(text, "◂ group end") {
		t.Fatalf("the unclosed group was not closed:\n%s", text)
	}
}

func TestCommandSinkCloseGroupAndNameBounds(t *testing.T) {
	sink, _, dst := newSinkForTest(t)
	sink.closeGroup() // no open group: a no-op, not a panic
	_ = sink.Close()
	if dst.Len() != 0 {
		t.Fatalf("closeGroup with no group wrote %q", dst.String())
	}
	// A command name over the length bound, or with an invalid character, is
	// not a command.
	for _, name := range []string{strings.Repeat("a", 65), "err.or", "9error"} {
		if _, ok := parseWorkflowCommand("::" + name + "::x"); ok {
			t.Errorf("name %q was accepted", name)
		}
	}
	if _, ok := parseWorkflowCommand("::error title=a%3Ab%2Cc::x"); !ok {
		t.Fatal("property escapes were rejected")
	}
}

func TestJobSummaryTotalAndSeparator(t *testing.T) {
	var s jobSummary
	// A piece without a trailing newline gets a separator when it is not first.
	if err := s.add("a", "no newline"); err != nil {
		t.Fatal(err)
	}
	if err := s.add("b", "second"); err != nil {
		t.Fatal(err)
	}
	if got := s.text(); got != "no newline\nsecond\n" {
		t.Fatalf("text = %q", got)
	}
	// The total cap is enforced before anything is stored.
	var big jobSummary
	piece := strings.Repeat("x", MaxStepSummaryBytes-1) + "\n"
	for i := 0; i < 5; i++ {
		if err := big.add("p", piece); err != nil {
			break
		}
	}
	if err := big.add("p", piece); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("total cap = %v", err)
	}
}

func TestCollectStepSummaryErrors(t *testing.T) {
	plan := &Plan{Names: Names{Container: "sp-j-x"}}
	step := StepPlan{Index: 0, Name: "s", SummaryPath: "/tmp/spindle-summary-1"}
	stream := quietStream(t)
	// A read failure is named.
	d := &fakeDocker{runOutputs: []runOutput{{match: "spindle-summary-1", errOut: "no such file", code: 1}}}
	var s jobSummary
	if err := executorWith(d).collectStepSummary(context.Background(), plan, step, &s, stream); err == nil ||
		!strings.Contains(err.Error(), "no such file") {
		t.Fatalf("read error = %v", err)
	}
	// An empty file adds nothing.
	e := &fakeDocker{runOutputs: []runOutput{{match: "spindle-summary-1", out: " \n"}}}
	if err := executorWith(e).collectStepSummary(context.Background(), plan, step, &s, stream); err != nil || s.entries != 0 {
		t.Fatalf("empty summary = %v entries=%d", err, s.entries)
	}
}

// TestPrepareStepScriptWriteFailure pins the custom-shell write path failing
// closed: the step fails naming the script file instead of running a stale or
// missing script.
func TestPrepareStepScriptWriteFailure(t *testing.T) {
	d := &fakeDocker{streamInErr: context.DeadlineExceeded}
	stream := quietStream(t)
	job := shellJob("dash -e {0}", "echo hi")
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if !strings.Contains(res.Error, "could not write the step's script file") {
		t.Fatalf("error = %q", res.Error)
	}
}
