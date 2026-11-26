package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Step outputs (docs/threads.md §3.9). The runner gives every step a file to
// write outputs into — the same mechanism GitHub calls `$GITHUB_OUTPUT` — and
// collects what a step with an `id:` wrote after it succeeded.
//
// The format is GitHub's:
//
//	name=value                     one line, value may contain '='
//	name<<DELIMITER                a heredoc: the value is every following line
//	line one                       until a line that is exactly DELIMITER
//	DELIMITER
//
// Blank lines outside a heredoc are ignored, a trailing CR is stripped (CRLF
// files work), and a duplicate name keeps the LAST value. Anything else is a
// named parse error, never a silently dropped output.
const (
	// OutputEnvVar is the file a step writes outputs to. The runner creates no
	// file: a step that publishes nothing simply never opens it.
	OutputEnvVar = "SPINDLE_OUTPUT"
	// OutputEnvAlias is the same path under the GitHub-compatible spelling, so
	// a script written for GitHub keeps working.
	OutputEnvAlias = "GITHUB_OUTPUT"
	// outputFilePrefix is the per-step path inside the job container. The
	// container (not the workspace volume) owns it, so a `git clean` in the
	// workspace cannot delete another step's outputs and nothing in the checkout
	// shows up as untracked.
	outputFilePrefix = "/tmp/spindle-output-"
)

// outputFilePath is the in-container output file of one step (index is the
// step's position in the job; a container serves exactly one attempt).
func outputFilePath(index int) string {
	return outputFilePrefix + strconv.Itoa(index+1)
}

// parseOutputFile parses one step's output file. The returned map is the step's
// outputs; the error names the line and the problem (fail closed).
func parseOutputFile(data []byte) (map[string]string, error) {
	out := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq >= 0 {
			// `name=value` wins only when it is not a heredoc marker. A name
			// cannot contain '<', so the heredoc branch below is unambiguous.
			name, val := strings.TrimSpace(line[:eq]), line[eq+1:]
			if err := checkOutputName(name, i); err != nil {
				return nil, err
			}
			if err := storeOutput(out, name, val, i); err != nil {
				return nil, err
			}
			continue
		}
		if hd := strings.Index(line, "<<"); hd >= 0 {
			name := strings.TrimSpace(line[:hd])
			delim := strings.TrimSpace(line[hd+2:])
			if err := checkOutputName(name, i); err != nil {
				return nil, err
			}
			if delim == "" {
				return nil, fmt.Errorf("line %d: %q uses a heredoc with no delimiter (write name<<DELIMITER)", i+1, name)
			}
			var value []string
			closed := false
			for j := i + 1; j < len(lines); j++ {
				cur := strings.TrimSuffix(lines[j], "\r")
				if cur == delim {
					i = j
					closed = true
					break
				}
				value = append(value, cur)
			}
			if !closed {
				return nil, fmt.Errorf("line %d: heredoc %s<<%s is never closed (a line containing only the delimiter must end it)", i+1, name, delim)
			}
			if err := storeOutput(out, name, strings.Join(value, "\n"), i); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("line %d: %q is neither name=value nor a name<<DELIMITER heredoc", i+1, truncateLine(line))
	}
	if len(out) > workflow.MaxOutputsPerStep {
		return nil, fmt.Errorf("the step wrote %d outputs, over the cap of %d per step", len(out), workflow.MaxOutputsPerStep)
	}
	return out, nil
}

// checkOutputName validates one written output name.
func checkOutputName(name string, line int) error {
	if !workflow.ValidOutputName(name) {
		return fmt.Errorf("line %d: invalid output name %q (letters, digits, _ and - only, at most %d bytes)",
			line+1, name, workflow.MaxOutputNameLen)
	}
	return nil
}

// storeOutput records one value (last write wins, as GitHub documents) and
// enforces the per-value cap.
func storeOutput(out map[string]string, name, value string, line int) error {
	if len(value) > workflow.MaxOutputValueBytes {
		return fmt.Errorf("line %d: output %q is %d bytes, over the cap of %d bytes per value",
			line+1, name, len(value), workflow.MaxOutputValueBytes)
	}
	out[name] = value
	return nil
}

// truncateLine bounds a bad line in an error message.
func truncateLine(line string) string {
	if len(line) > 120 {
		return line[:120] + "…"
	}
	return line
}

// collectStepOutputs reads one step's output file out of its container. Only a
// step that succeeded reaches here, so a failed or skipped step's file is never
// read (docs/threads.md §3.9: unset, not empty).
func (e *Executor) collectStepOutputs(ctx context.Context, plan *Plan, step StepPlan) (map[string]string, error) {
	if step.OutputPath == "" {
		return nil, nil
	}
	if step.Kind == "docker" {
		return e.collectDockerOutputs(ctx, plan, step)
	}
	// head -c bounds the read at one byte over the per-job cap: enough to
	// detect an oversized file without buffering it whole.
	limit := workflow.MaxOutputBytes + 1
	script := "if [ -f " + shQuote(step.OutputPath) + " ]; then head -c " + strconv.Itoa(limit) + " " + shQuote(step.OutputPath) + "; fi"
	out, stderr, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c", script)
	if err != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return nil, fmt.Errorf("could not read the step's output file %s: %s", step.OutputPath, msg)
	}
	if len(out) > workflow.MaxOutputBytes {
		return nil, fmt.Errorf("the step's output file is over the cap of %d bytes per job", workflow.MaxOutputBytes)
	}
	outs, perr := parseOutputFile([]byte(out))
	if perr != nil {
		return nil, fmt.Errorf("step output file %s: %v", step.OutputPath, perr)
	}
	return outs, nil
}

// collectDockerOutputs reads a docker action's output file out of its own
// container (the workspace volume is shared, the container's /tmp is not). The
// container is still alive here: it is removed after this copy. An image with
// no /tmp, or a step that wrote nothing, simply has no file — that is normal,
// not an error (only a malformed or oversized file is).
func (e *Executor) collectDockerOutputs(ctx context.Context, plan *Plan, step StepPlan) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "spindle-action-out-")
	if err != nil {
		return nil, fmt.Errorf("could not create a temp directory for the action's outputs: %v", err)
	}
	defer os.RemoveAll(dir)
	dest := filepath.Join(dir, "out")
	name := dockerActionName(plan, step.OutputIndex)
	_, stderr, code, cerr := e.Docker.Run(ctx, nil, "cp", name+":/tmp", dest)
	if cerr != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && cerr != nil {
			msg = cerr.Error()
		}
		return nil, fmt.Errorf("could not read the docker action's output file %s from %s: %s", step.OutputPath, name, msg)
	}
	path := filepath.Join(dest, filepath.Base(step.OutputPath))
	info, serr := os.Stat(path)
	if errors.Is(serr, os.ErrNotExist) {
		return nil, nil // the action published nothing
	}
	if serr != nil {
		return nil, fmt.Errorf("could not read the docker action's output file %s: %v", step.OutputPath, serr)
	}
	if info.Size() > workflow.MaxOutputBytes {
		return nil, fmt.Errorf("the action's output file is over the cap of %d bytes per job", workflow.MaxOutputBytes)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return nil, fmt.Errorf("could not read the docker action's output file %s: %v", step.OutputPath, rerr)
	}
	outs, perr := parseOutputFile(data)
	if perr != nil {
		return nil, fmt.Errorf("action output file %s: %v", step.OutputPath, perr)
	}
	return outs, nil
}

// evaluateJobOutputs fills the job's declared `outputs:` templates from the
// collected step outputs. It runs only for a successful job; a template that
// names a step which never published its output is an error (the job fails
// naming it) rather than an empty value crossing a job boundary.
func evaluateJobOutputs(plan *Plan, steps *stepOutcomes) (map[string]string, error) {
	if plan == nil || len(plan.OutputTemplates) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(plan.OutputTemplates))
	for name := range plan.OutputTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		val, err := plan.expandValue(plan.OutputTemplates[name], plan.Env, steps.Outputs, true)
		if err != nil {
			return nil, fmt.Errorf("job output %q: %v", name, err)
		}
		out[name] = val
	}
	if err := threads.ValidateOutputs(out); err != nil {
		return nil, err
	}
	return out, nil
}
