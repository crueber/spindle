package runner

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Workflow commands (PAR-093): the parser and the sink are the untrusted-input
// boundary. These tables pin escaping, caps, masking and the "never inert"
// rule (an unimplemented command is named, not swallowed).

func TestParseWorkflowCommand(t *testing.T) {
	cases := []struct {
		line    string
		ok      bool
		name    string
		message string
		props   map[string]string
	}{
		{line: "::error file=a.go,line=3,col=2,title=Build::boom", ok: true, name: "error",
			message: "boom", props: map[string]string{"file": "a.go", "line": "3", "col": "2", "title": "Build"}},
		{line: `::warning::a%0Ab%25c%0Dd`, ok: true, name: "warning", message: "a\nb%c\rd"},
		{line: `::notice title=has%2Ccomma::x%3Ay`, ok: true, name: "notice", message: "x%3Ay",
			props: map[string]string{"title": "has,comma"}},
		{line: "::group::Build", ok: true, name: "group", message: "Build"},
		{line: "::stop-commands::tok", ok: true, name: "stop-commands", message: "tok"},
		{line: "plain output", ok: false},
		{line: "::1::", ok: false},        // an IPv6-shaped line is not a command
		{line: "::error", ok: false},      // no closing ::
		{line: "::9lives::x", ok: false},  // a name starts with a letter
		{line: "  ::error::x", ok: false}, // commands start at column 0
	}
	for _, c := range cases {
		cmd, ok := parseWorkflowCommand(c.line)
		if ok != c.ok {
			t.Fatalf("%q: ok=%v want %v", c.line, ok, c.ok)
		}
		if !ok {
			continue
		}
		if cmd.Name != c.name || cmd.Message != c.message {
			t.Errorf("%q = %+v", c.line, cmd)
		}
		for k, v := range c.props {
			if cmd.Props[k] != v {
				t.Errorf("%q prop %s = %q want %q", c.line, k, cmd.Props[k], v)
			}
		}
	}
}

// TestWorkflowCommandAnnotations pins the annotation fields, masking and the
// log line: the raw `::error::` syntax never reaches the log as if it were
// content.
func TestWorkflowCommandAnnotations(t *testing.T) {
	stream := quietStream(t)
	stream.addMask([]string{"hunter2"})
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Index: 0, ID: "build", Name: "build"})
	_, _ = sink.Write([]byte("normal line\n::error file=a.go,line=3,col=2,title=Oops::hunter2 failed\n::warning::careful\n"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	text := dst.String()
	if strings.Contains(text, "::error") {
		t.Fatalf("the raw command reached the log:\n%s", text)
	}
	for _, want := range []string{"normal line", "a.go:3:2", "Oops", "[WARNING]", "careful"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hunter2") {
		t.Fatalf("a masked value reached the log:\n%s", text)
	}
	if len(job.annotations) != 2 {
		t.Fatalf("annotations = %+v", job.annotations)
	}
	a := job.annotations[0]
	if a.Level != "error" || a.File != "a.go" || a.Line != 3 || a.Col != 2 || a.Title != "Oops" ||
		a.Step != "build" || a.Message != "*** failed" {
		t.Fatalf("annotation = %+v", a)
	}
	if job.annotations[1].Level != "warning" || job.annotations[1].File != "" {
		t.Fatalf("warning annotation = %+v", job.annotations[1])
	}
}

// TestWorkflowCommandGroupsAndStop covers the two stateful commands: a group
// must close (even when the step forgets), and stopped commands are plain text
// until the exact resume token.
func TestWorkflowCommandGroupsAndStop(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Name: "s"})
	_, _ = sink.Write([]byte("::group::inner\nlog\n::stop-commands::secret-token\n::error::not a command\n::wrong-token::\n::secret-token::\n::notice::resumed\n"))
	_ = sink.Close()
	text := dst.String()
	if strings.Count(text, "▸ group: inner") != 1 || strings.Count(text, "◂ group end") != 1 {
		t.Fatalf("group markers = %q", text)
	}
	if !strings.Contains(text, "::error::not a command") {
		t.Fatalf("a stopped command was consumed instead of printed:\n%s", text)
	}
	if strings.Contains(text, "[NOTICE]") && !strings.Contains(text, "resumed") {
		t.Fatalf("resume failed:\n%s", text)
	}
	if len(job.annotations) != 1 || job.annotations[0].Message != "resumed" {
		t.Fatalf("annotations after resume = %+v", job.annotations)
	}
	// A group left open is closed at the end of the step, with no warning noise
	// in the step's own output beyond the marker.
	if !strings.HasSuffix(strings.TrimSpace(text), "◂ group end") {
		t.Fatalf("unclosed group not closed:\n%s", text)
	}
}

// TestWorkflowCommandAddMaskCannotUnmask pins the mask rule: a mask is permanent,
// an empty mask is refused, and a later text line cannot remove it.
func TestWorkflowCommandAddMaskCannotUnmask(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Name: "s"})
	_, _ = sink.Write([]byte("::add-mask::topsecret-value\n::add-mask::\n"))
	_ = sink.Close()
	if !strings.Contains(stream.Mask("topsecret-value"), "***") {
		t.Fatalf("the mask was not registered: %q", stream.Mask("topsecret-value"))
	}
	if strings.Contains(dst.String(), "topsecret-value") {
		t.Fatalf("the masked value was printed:\n%s", dst.String())
	}
	if !strings.Contains(dst.String(), "needs a value") {
		t.Fatalf("an empty add-mask was not named:\n%s", dst.String())
	}
	// There is no command, and no API, that removes a mask: a "remove" spelling
	// is an unsupported command and the value still masks.
	_, _ = sink.Write([]byte("::remove-mask::topsecret-value\n"))
	_ = sink.Close()
	if !strings.Contains(stream.Mask("topsecret-value"), "***") {
		t.Fatal("a mask was removed")
	}
}

// TestWorkflowCommandSetOutputAndUnsupported covers the deprecated output alias
// and the never-inert rule for commands the runner does not implement.
func TestWorkflowCommandSetOutputAndUnsupported(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{ID: "build", Name: "build"})
	_, _ = sink.Write([]byte("::set-output name=tag::v1.2.3\n::save-state name=x::1\n::set-env name=X::y\n::frobnicate::hello\n"))
	_ = sink.Close()
	if got := sink.setOutputs["tag"]; got != "v1.2.3" {
		t.Fatalf("set-output = %q", got)
	}
	text := dst.String()
	for _, want := range []string{"is ignored here", "removed by GitHub", "unsupported workflow command ::frobnicate::"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log misses %q:\n%s", want, text)
		}
	}
	// The unsupported line is preserved as text, so nothing disappears.
	if !strings.Contains(text, "::frobnicate::hello") {
		t.Fatalf("the unsupported line was swallowed:\n%s", text)
	}
	// An output on an anonymous step is discarded by name.
	anon := newCommandSink(&dst, job, StepPlan{Name: "anon"})
	_, _ = anon.Write([]byte("::set-output name=x::y\n"))
	_ = anon.Close()
	if len(anon.setOutputs) != 0 || !strings.Contains(dst.String(), "no id") {
		t.Fatalf("anonymous set-output = %v\n%s", anon.setOutputs, dst.String())
	}
}

// TestWorkflowCommandCaps pins the per-step and per-job annotation caps and the
// bounded line buffer: a hostile step cannot grow the record or the runner.
func TestWorkflowCommandCaps(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Name: "s"})
	var b strings.Builder
	for i := 0; i < threads.MaxAnnotationsPerStep+5; i++ {
		b.WriteString("::error::e\n")
	}
	_, _ = sink.Write([]byte(b.String()))
	_ = sink.Close()
	if len(job.annotations) != threads.MaxAnnotationsPerStep {
		t.Fatalf("annotations = %d, want %d", len(job.annotations), threads.MaxAnnotationsPerStep)
	}
	if !strings.Contains(dst.String(), "annotation dropped") {
		t.Fatalf("the cap was not named:\n%s", dst.String())
	}

	// A line over the buffer bound is forwarded as text and never parsed from
	// its middle.
	var long strings.Builder
	long.WriteString("::error::")
	for i := 0; i < MaxCommandLineBytes+1024; i++ {
		long.WriteByte('x')
	}
	long.WriteString("\n")
	var dst2 markerBuffer
	sink2 := newCommandSink(&dst2, job, StepPlan{Name: "s"})
	_, _ = sink2.Write([]byte(long.String()))
	_ = sink2.Close()
	if len(job.annotations) != threads.MaxAnnotationsPerStep {
		t.Fatalf("an over-long line produced an annotation: %+v", job.annotations)
	}
	if dst2.Len() < MaxCommandLineBytes {
		t.Fatalf("the over-long line was not forwarded (%d bytes)", dst2.Len())
	}
}

// TestWorkflowCommandInvalidAnnotationIsNumber keeps a malformed numeric property
// an unanchored annotation (never a failed step).
func TestWorkflowCommandInvalidAnnotationIsNumber(t *testing.T) {
	stream := quietStream(t)
	job := newJobCommands(stream)
	var dst markerBuffer
	sink := newCommandSink(&dst, job, StepPlan{Name: "s"})
	_, _ = sink.Write([]byte("::error file=./a%0Ab.go,line=nope::-1\n"))
	_ = sink.Close()
	if len(job.annotations) != 1 {
		t.Fatalf("annotations = %+v", job.annotations)
	}
	a := job.annotations[0]
	if a.Line != 0 || a.File != "ab.go" {
		t.Fatalf("annotation = %+v", a)
	}
}
