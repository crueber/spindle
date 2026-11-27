package runner

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Workflow commands (PAR-093, docs/WORKFLOWS.md §6.7). A step writes
//
//	::name key=value,key=value::message
//
// on its output; the runner consumes the line and turns it into an annotation,
// a log group marker or a mask. The command stream is UNTRUSTED input: a fork
// pull request's code writes it, so every field is bounded, escaped and used as
// data. There is no shell expansion of any payload (the line is never handed to
// a shell), a mask can never be removed, and a command the runner does not
// implement is answered with a visible warning instead of being swallowed.
//
// Escaping follows the Actions runner: `%25`→`%`, `%0D`→CR and `%0A`→LF in the
// message; a property additionally unescapes `%3A`→`:` and `%2C`→`,`. The
// runner escapes nothing when it prints a command's payload: the text goes
// through the same mask as the log stream and is rendered escaped by the page.
const (
	// MaxCommandLineBytes bounds one output line the parser will buffer. A
	// longer line is forwarded as plain text (never parsed mid-line), so a
	// program that prints megabytes without a newline cannot grow the runner's
	// memory.
	MaxCommandLineBytes = 64 << 10
	// MaxMaskValues bounds how many `::add-mask::` values one job may add: the
	// mask is applied to every log frame, so an unbounded list is a CPU knife.
	MaxMaskValues = 128
	// MaxUnknownCommands bounds the warnings for unrecognized `::name::` lines
	// (a program that prints such lines in a loop must not flood the job).
	MaxUnknownCommands = 10
)

// workflowCommand is one parsed `::name props::message` line.
type workflowCommand struct {
	Name    string
	Props   map[string]string
	Message string
}

// commandLineRe is the command-name vocabulary: a name starts with a letter and
// continues with letters, digits, `-` or `_`. Anything else (an IPv6 address
// such as `::1::`, a timestamp) is not a command and stays in the log.
func validCommandName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9', r == '-', r == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// unescapeCommandData decodes GitHub's data escapes.
func unescapeCommandData(s string) string {
	s = strings.ReplaceAll(s, "%0D", "\r")
	s = strings.ReplaceAll(s, "%0A", "\n")
	return strings.ReplaceAll(s, "%25", "%")
}

// unescapeCommandProperty decodes GitHub's property escapes.
func unescapeCommandProperty(s string) string {
	s = strings.ReplaceAll(s, "%3A", ":")
	s = strings.ReplaceAll(s, "%2C", ",")
	return unescapeCommandData(s)
}

// parseWorkflowCommand parses one output line. ok is false for a line that is
// not a workflow command (plain output, an IPv6 address, a malformed line).
func parseWorkflowCommand(line string) (workflowCommand, bool) {
	if !strings.HasPrefix(line, "::") {
		return workflowCommand{}, false
	}
	rest := line[2:]
	end := strings.Index(rest, "::")
	if end < 0 {
		return workflowCommand{}, false
	}
	header, message := rest[:end], unescapeCommandData(rest[end+2:])
	name := header
	propText := ""
	if i := strings.IndexByte(header, ' '); i >= 0 {
		name, propText = header[:i], header[i+1:]
	}
	if !validCommandName(name) {
		return workflowCommand{}, false
	}
	cmd := workflowCommand{Name: name, Message: message}
	if propText != "" {
		cmd.Props = map[string]string{}
		for _, pair := range strings.Split(propText, ",") {
			eq := strings.IndexByte(pair, '=')
			if eq <= 0 {
				continue // a property without a name is skipped, never guessed
			}
			cmd.Props[unescapeCommandProperty(pair[:eq])] = unescapeCommandProperty(pair[eq+1:])
		}
	}
	return cmd, true
}

// jobCommands is one job attempt's workflow-command state: the annotations it
// collected, the masks it added and the deprecated `::set-output::` values of
// the step in flight. It is owned by the executor's single goroutine, so no
// lock is needed (like stepOutcomes).
type jobCommands struct {
	stream *Streamer
	// mu guards every field: a parallel group's members each parse their own
	// output and record annotations from their own goroutine (PAR-080).
	mu          sync.Mutex
	annotations []threads.Annotation
	masks       int
	unknown     int
}

// newJobCommands builds an empty sink for one attempt.
func newJobCommands(stream *Streamer) *jobCommands {
	return &jobCommands{stream: stream}
}

// addAnnotation stores one annotation, enforcing the per-step and per-job caps.
// A message is data: it is masked and length-bounded, never interpreted.
func (j *jobCommands) addAnnotation(a threads.Annotation, stepName string, stepCount *int) error {
	a.Level = strings.ToLower(strings.TrimSpace(a.Level))
	if !threads.ValidAnnotationLevel(a.Level) {
		return fmt.Errorf("unknown annotation level %q", a.Level)
	}
	a.Message = j.stream.Mask(a.Message)
	a.Title = j.stream.Mask(a.Title)
	if len(a.Message) > threads.MaxAnnotationMessage {
		a.Message = a.Message[:threads.MaxAnnotationMessage]
	}
	if len(a.Title) > threads.MaxAnnotationTitle {
		a.Title = a.Title[:threads.MaxAnnotationTitle]
	}
	if len(a.File) > threads.MaxAnnotationFile {
		a.File = a.File[:threads.MaxAnnotationFile]
	}
	if len(stepName) > threads.MaxAnnotationStep {
		stepName = stepName[:threads.MaxAnnotationStep]
	}
	a.Step = stepName
	j.mu.Lock()
	defer j.mu.Unlock()
	if *stepCount >= threads.MaxAnnotationsPerStep {
		return fmt.Errorf("over the cap of %d annotations per step", threads.MaxAnnotationsPerStep)
	}
	if len(j.annotations) >= threads.MaxAnnotationsPerJob {
		return fmt.Errorf("over the cap of %d annotations per job", threads.MaxAnnotationsPerJob)
	}
	*stepCount++
	j.annotations = append(j.annotations, a)
	return nil
}

// snapshot copies the annotations for the completion report.
func (j *jobCommands) snapshot() []threads.Annotation {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]threads.Annotation(nil), j.annotations...)
}

// addMask registers one `::add-mask::` value. A mask is permanent (there is no
// unmask command, and nothing removes an entry), and the list is bounded. A
// value under 4 bytes is still recorded but the log masker ignores it (masking
// a one-character string would redact the whole log); the caller says so.
func (j *jobCommands) addMask(value string) error {
	if value == "" {
		return fmt.Errorf("::add-mask:: needs a value")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.masks >= MaxMaskValues {
		return fmt.Errorf("over the cap of %d masks per job", MaxMaskValues)
	}
	j.masks++
	j.stream.addMask([]string{value})
	return nil
}

// commandSink writes one step's output through the command parser. Text lines
// are forwarded to the log unchanged; a recognized command is consumed and
// replaced by the runner's own line, so the raw `::error::` syntax never
// reaches a rendered page as if it were content.
type commandSink struct {
	dst  markerSink
	job  *jobCommands
	step StepPlan
	// state is the plan's per-action `$GITHUB_STATE` map (PAR-092): nil outside
	// a JavaScript action, where `::save-state::` has no post hook to reach.
	state *actionState
	// setOutputs holds `::set-output name=x::value` values (the deprecated
	// alias), merged into the step's collected outputs when it finishes.
	setOutputs map[string]string
	// stepAnnotations counts this step's annotations (the per-step cap).
	stepAnnotations int
	stopped         string
	groups          []string
	pending         []byte
	// skipping is set after an over-long line was flushed as text: the rest of
	// that line is text too, never a command parsed from its middle.
	skipping bool
}

// markerSink is the write side a command sink needs: plain text plus the two
// structural markers `::group::`/`::endgroup::` produce (PAR-190 group folding).
type markerSink interface {
	Write(p []byte) (int, error)
	Group(name string)
	GroupEnd()
}

// newCommandSink builds one step's sink writing to dst.
func newCommandSink(dst markerSink, job *jobCommands, step StepPlan) *commandSink {
	return &commandSink{dst: dst, job: job, step: step, setOutputs: map[string]string{}}
}

// Write implements io.Writer.
func (w *commandSink) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		line := string(w.pending[:i])
		w.pending = w.pending[i+1:]
		if w.skipping || len(line) > MaxCommandLineBytes {
			// A line over the bound is plain text: the runner never parses a
			// command out of a truncated or oversized line.
			w.skipping = false
			w.text(line)
			continue
		}
		w.handle(line)
	}
	if len(w.pending) > MaxCommandLineBytes {
		w.text(string(w.pending))
		w.pending = w.pending[:0]
		w.skipping = true
	}
	return len(p), nil
}

// Close flushes the trailing partial line and closes any group the step left
// open (a group is a display aid; an unbalanced one must not swallow the rest
// of the job's log).
func (w *commandSink) Close() error {
	if len(w.pending) > 0 {
		line := string(w.pending)
		w.pending = w.pending[:0]
		if w.skipping {
			w.text(line)
		} else {
			w.handle(line)
		}
	}
	for len(w.groups) > 0 {
		w.closeGroup()
	}
	if w.stopped != "" {
		w.system("workflow commands were stopped with a token that never resumed; the rest of the step is plain output")
		w.stopped = ""
	}
	return nil
}

// text writes one line to the log verbatim (with its newline).
func (w *commandSink) text(line string) {
	_, _ = io.WriteString(w.dst, line+"\n")
}

// system writes one runner-emitted line inside the step's output.
func (w *commandSink) system(format string, args ...any) {
	_, _ = io.WriteString(w.dst, fmt.Sprintf(format, args...)+"\n")
}

// handle consumes one line: a command, or text.
func (w *commandSink) handle(line string) {
	if w.stopped != "" {
		if line == "::"+w.stopped+"::" {
			w.stopped = ""
			w.system("spindle: workflow commands resumed")
			return
		}
		w.text(line)
		return
	}
	cmd, ok := parseWorkflowCommand(line)
	if !ok {
		w.text(line)
		return
	}
	if !w.run(cmd) {
		// An unimplemented command is never inert: the warning names it and the
		// original line still reaches the log.
		if w.job.unknown < MaxUnknownCommands {
			w.job.unknown++
			w.system("spindle: unsupported workflow command ::%s:: (ignored; supported: error, warning, notice, group, endgroup, add-mask, stop-commands, set-output, save-state, debug)", cmd.Name)
		}
		w.text(line)
	}
}

// run applies one command; false means the command is not implemented.
func (w *commandSink) run(cmd workflowCommand) bool {
	switch cmd.Name {
	case "error", "warning", "notice":
		w.annotation(cmd)
		return true
	case "group":
		w.groups = append(w.groups, cmd.Message)
		w.dst.Group(oneLine(cmd.Message))
		return true
	case "endgroup":
		if len(w.groups) == 0 {
			w.system("spindle: ::endgroup:: with no open group")
			return true
		}
		w.closeGroup()
		return true
	case "add-mask":
		if err := w.job.addMask(cmd.Message); err != nil {
			w.system("spindle: ::add-mask:: %v", err)
			return true
		}
		w.system("spindle: added a mask (the value is never printed)")
		if len(cmd.Message) > 0 && len(cmd.Message) < 4 {
			w.system("spindle: a mask shorter than 4 bytes is not applied to the log (it would redact everything)")
		}
		return true
	case "stop-commands":
		if strings.TrimSpace(cmd.Message) == "" {
			w.system("spindle: ::stop-commands:: needs a resume token")
			return true
		}
		w.stopped = cmd.Message
		w.system("spindle: workflow commands stopped (resume with ::%s::)", cmd.Message)
		return true
	case "set-output":
		w.setOutput(cmd)
		return true
	case "debug":
		w.system("· %s", cmd.Message)
		return true
	case "save-state":
		w.saveState(cmd)
		return true
	case "set-env", "add-path":
		w.system("spindle: ::%s:: was removed by GitHub and is not supported; use $GITHUB_ENV / $GITHUB_PATH", cmd.Name)
		return true
	}
	return false
}

// saveState implements the deprecated `::save-state name=X::value` spelling of
// a `$GITHUB_STATE` write (PAR-092). It feeds the SAME map the file feeds, so
// the post hook receives `STATE_X` either way; a step that is not a JavaScript
// action has no post hook, and says so by name instead of accepting the value
// into nowhere.
func (w *commandSink) saveState(cmd workflowCommand) {
	name := strings.TrimSpace(cmd.Props["name"])
	if name == "" {
		w.system("spindle: ::save-state:: needs a name property")
		return
	}
	if w.state == nil || w.step.Node == nil || w.step.Node.StatePath == "" {
		w.system("spindle: ::save-state:: is ignored here: state exists to reach an action's post hook, and this step is not a JavaScript action (write the value to $GITHUB_OUTPUT or $GITHUB_ENV instead)")
		return
	}
	if err := w.state.set(w.step.Node.StatePath, name, cmd.Message); err != nil {
		w.system("spindle: ::save-state:: refused: %v", err)
		return
	}
	// The name is a fact; the VALUE is action data and may be a token, so the
	// confirmation never prints it.
	w.system("spindle: saved state %q for the post hook", name)
}

// closeGroup emits the end marker of the innermost open group.
func (w *commandSink) closeGroup() {
	if len(w.groups) == 0 {
		return
	}
	w.groups = w.groups[:len(w.groups)-1]
	w.dst.GroupEnd()
}

// annotation turns one error/warning/notice command into an annotation and a
// log line. The log line is the same text the job record carries (masked), so
// the raw log and the annotation panel agree.
func (w *commandSink) annotation(cmd workflowCommand) {
	// Mask BEFORE storing and before printing: one value, one rule (a log line
	// and the job record must never disagree about a secret).
	a := threads.Annotation{Level: cmd.Name, Message: w.job.stream.Mask(cmd.Message)}
	if v := strings.TrimSpace(cmd.Props["title"]); v != "" {
		a.Title = w.job.stream.Mask(v)
	}
	if v := strings.TrimSpace(cmd.Props["file"]); v != "" {
		a.File = sanitizeAnnotationPath(v)
	}
	a.Line = annotationInt(cmd.Props["line"])
	a.EndLine = annotationInt(cmd.Props["endLine"])
	a.Col = annotationInt(cmd.Props["col"])
	a.EndColumn = annotationInt(cmd.Props["endColumn"])
	if err := w.job.addAnnotation(a, w.step.Name, &w.stepAnnotations); err != nil {
		w.system("spindle: %s annotation dropped: %v", cmd.Name, err)
		return
	}
	label := strings.ToUpper(cmd.Name)
	where := ""
	if a.File != "" {
		where = " " + a.File
		if a.Line > 0 {
			where += ":" + strconv.Itoa(a.Line)
			if a.Col > 0 {
				where += ":" + strconv.Itoa(a.Col)
			}
		}
	}
	if a.Title != "" {
		label += " " + a.Title
	}
	w.system("[%s]%s %s", label, where, oneLine(a.Message))
}

// setOutput stores the deprecated `::set-output name=x::value` alias. GitHub
// disabled it; Threads honours it (documented) so an older workflow keeps
// working, with the same caps as the output file. A step without an id cannot
// publish anything, which is a warning, not a silent drop.
func (w *commandSink) setOutput(cmd workflowCommand) {
	name := strings.TrimSpace(cmd.Props["name"])
	if name == "" {
		w.system("spindle: ::set-output:: needs a name property")
		return
	}
	if w.step.ID == "" && w.step.LocalID == "" {
		w.system("spindle: ::set-output name=%s:: on a step with no id is discarded (outputs belong to `steps.<id>`)", name)
		return
	}
	if !workflow.ValidOutputName(name) {
		w.system("spindle: ::set-output name=%q:: is not a valid output name", name)
		return
	}
	value := w.job.stream.Mask(cmd.Message)
	if len(value) > workflow.MaxOutputValueBytes {
		w.system("spindle: ::set-output name=%s:: is over the per-value cap and was dropped", name)
		return
	}
	if len(w.setOutputs) >= workflow.MaxOutputsPerStep {
		if _, replaced := w.setOutputs[name]; !replaced {
			w.system("spindle: ::set-output:: wrote more than %d outputs in one step and was dropped", workflow.MaxOutputsPerStep)
			return
		}
	}
	w.setOutputs[name] = value
}

// oneLine flattens a command payload for a log line (the stored annotation
// keeps its newlines; a log line must stay one line).
func oneLine(s string) string {
	r := strings.NewReplacer("\r", " ", "\n", " ")
	return r.Replace(strings.TrimSpace(s))
}

// sanitizeAnnotationPath keeps a `file=` property a path: control characters
// are stripped (a newline in a file name is never a path) and a leading `./`
// is removed so the page can link the repository coordinate.
func sanitizeAnnotationPath(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimPrefix(out, "./")
	return out
}

// annotationInt parses one numeric annotation property; a malformed value is 0
// (the annotation still exists, unanchored) rather than a failed step.
func annotationInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	if n > 1_000_000 {
		return 1_000_000
	}
	return n
}
