package workflow

// Flow-control keys shared by the parser and the server (PAR-053…PAR-058):
// `runs-on` substitution, `defaults.run`, `timeout-minutes`, `run-name` and
// `strategy.max-parallel`. The parse-time validators are here; the runtime
// helpers the server calls at run creation live beside them so the two halves
// cannot drift.

import (
	"fmt"
	"strconv"
	"strings"
)

// validateDefaults checks one `defaults:` block. Only `run.shell` and
// `run.working-directory` are accepted, and both are literals: GitHub does not
// resolve contexts there, and a substituted default would make a step's shell
// depend on a value the author cannot see at the step.
func validateDefaults(where string, d *Defaults) error {
	if d == nil {
		return nil
	}
	if d.Run == nil {
		return nil
	}
	for _, field := range []struct{ name, value string }{
		{"shell", d.Run.Shell},
		{"working-directory", d.Run.WorkingDirectory},
	} {
		if strings.Contains(field.value, "${{") {
			return fmt.Errorf("%w: %s.run.%s is %q; defaults.run takes a literal (write the value on the job, or on the step, which does substitute)",
				ErrStructuralExpression, where, field.name, field.value)
		}
	}
	return nil
}

// DefaultsFor returns the effective `defaults.run` pair for one job: the job's
// block overrides the workflow's per key.
func DefaultsFor(wf *Workflow, job *Job) (shell, workingDir string) {
	if wf != nil && wf.Defaults != nil && wf.Defaults.Run != nil {
		shell, workingDir = wf.Defaults.Run.Shell, wf.Defaults.Run.WorkingDirectory
	}
	if job != nil && job.Defaults != nil && job.Defaults.Run != nil {
		if job.Defaults.Run.Shell != "" {
			shell = job.Defaults.Run.Shell
		}
		if job.Defaults.Run.WorkingDirectory != "" {
			workingDir = job.Defaults.Run.WorkingDirectory
		}
	}
	return strings.TrimSpace(shell), strings.TrimSpace(workingDir)
}

// validateRunsOn checks every `runs-on` label (PAR-053). A label may be a
// literal or a whole `${{ … }}` reference to a matrix axis, a declared input or
// a github field; anything else is this file's parse error.
func validateRunsOn(jobName string, labels Labels, scope TemplateScope) error {
	for i, label := range labels {
		where := fmt.Sprintf("workflow: job %s runs-on", jobName)
		if len(labels) > 1 {
			where = fmt.Sprintf("workflow: job %s runs-on[%d]", jobName, i)
		}
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("%w: %s has an empty label", ErrNoRunsOn, where)
		}
		if err := ValidateTemplate(label, TemplateScope{
			Axes: scope.Axes, Inputs: scope.Inputs, Site: SiteRunsOn,
		}); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// ExpandRunsOn resolves every `runs-on` label for one matrix cell. A reference
// that resolves to nothing is an error naming the label (a job must never queue
// against the empty string); the caller fails run creation.
func ExpandRunsOn(labels []string, github, matrix, inputs map[string]string) ([]string, error) {
	return ExpandRunsOnWithStrategy(labels, github, matrix, inputs, nil)
}

// ExpandRunsOnWithStrategy is ExpandRunsOn plus the matrix cell's `strategy.*`
// context (PAR-039).
func ExpandRunsOnWithStrategy(labels []string, github, matrix, inputs, strategy map[string]string) ([]string, error) {
	out := make([]string, len(labels))
	for i, label := range labels {
		if !strings.Contains(label, "${{") {
			out[i] = label
			continue
		}
		val, err := Substitute(label, TemplateValues{GitHub: github, Matrix: matrix, Inputs: inputs, Strategy: strategy})
		if err != nil {
			return nil, fmt.Errorf("runs-on[%d]: %w", i, err)
		}
		if strings.TrimSpace(val) == "" {
			return nil, fmt.Errorf("runs-on[%d] %q resolved to an empty label for this matrix cell", i, label)
		}
		out[i] = val
	}
	return out, nil
}

// validateContainerImage checks a job `container.image` (PAR-030). It is a
// substituted structural field like `runs-on`: a literal, or a value with
// whole `${{ … }}` spans naming a matrix axis, a declared input or a github
// field. Every other structural field (a service image, `environment`, a
// workflow/job/container env value) is still a named parse error.
func validateContainerImage(jobName string, c *Container, scope TemplateScope) error {
	if c == nil {
		return nil
	}
	// An empty image is a carried shape (a `container:` mapping with no image
	// runs the runner's default image), so only a `${{ … }}` span is checked.
	if err := ValidateTemplate(c.Image, TemplateScope{
		Axes: scope.Axes, Inputs: scope.Inputs, Site: SiteContainerImage,
	}); err != nil {
		return fmt.Errorf("workflow: job %s container.image: %w", jobName, err)
	}
	return nil
}

// ExpandContainerImage resolves one `container.image` for a matrix cell
// (PAR-030). An image that resolves to nothing is an error naming the value: a
// job must never start against the empty image, and the image gate must see the
// real tag rather than the authored span.
func ExpandContainerImage(image string, github, matrix, inputs map[string]string) (string, error) {
	return ExpandContainerImageWithStrategy(image, github, matrix, inputs, nil)
}

// ExpandContainerImageWithStrategy is ExpandContainerImage plus the matrix
// cell's `strategy.*` context (PAR-039).
func ExpandContainerImageWithStrategy(image string, github, matrix, inputs, strategy map[string]string) (string, error) {
	if !strings.Contains(image, "${{") {
		return image, nil
	}
	// A container image is structural: an absent reference must fail the run,
	// never leave `debian:` (an empty tag) to the image gate. Every reference is
	// therefore resolved strictly, exactly like a concurrency group's key.
	parts, err := splitTemplate(image)
	if err != nil {
		return "", fmt.Errorf("container.image: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		if p.ref.Context == "spindle" || p.ref.Context == "github" || p.ref.Context == "forgejo" || p.ref.Context == "forge" {
			if reason, unavailable := GitHubFieldUnavailable(p.ref.Key); unavailable {
				return "", fmt.Errorf("container.image: %s: %s", p.ref.Raw, reason)
			}
		}
		val, ok := lookupRefStrategy(*p.ref, github, matrix, inputs, strategy)
		if !ok {
			return "", fmt.Errorf("container.image: ${{ %s.%s }} resolved to nothing for this matrix cell", p.ref.Context, p.ref.Key)
		}
		b.WriteString(val)
	}
	return b.String(), nil
}

// validateBoolExpr checks one expression-valued boolean field (PAR-056,
// PAR-151): a literal is unchanged, a whole `${{ … }}` expression is validated
// against the site's availability table (a reference the position cannot read
// is that file's parse error, never an empty value).
func validateBoolExpr(where string, b BoolExpr, scope TemplateScope) error {
	if b.Expr == "" {
		return nil
	}
	if err := validateExpr(b.Expr, scope); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	return nil
}

// EvalBoolExpr evaluates one expression-valued boolean for a matrix cell. A
// result that is not a boolean is an error (fail closed): a job must never
// become tolerated — or a run silently cancel its holder — on a mistaken value.
func EvalBoolExpr(expr string, github, matrix, inputs, strategy map[string]string) (bool, error) {
	return EvalBool(expr, EvalScope{GitHub: github, Matrix: matrix, Inputs: inputs, Strategy: strategy})
}

// validateTimeout checks one `timeout-minutes` value (PAR-055). An expression
// must be one whole span and is validated against the site's contexts; a
// literal must be 1…360.
func validateTimeout(where string, m Minutes, scope TemplateScope) error {
	if m.Expr == "" {
		if m.Present && (m.Value < 1 || m.Value > MaxTimeoutMinutes) {
			return fmt.Errorf("%w: %s timeout-minutes must be 1…%d, got %d", ErrBadTimeout, where, MaxTimeoutMinutes, m.Value)
		}
		return nil
	}
	if err := validateExpr(m.Expr, TemplateScope{Axes: scope.Axes, Inputs: scope.Inputs, Site: scope.Site}); err != nil {
		return fmt.Errorf("workflow: %s timeout-minutes: %w", where, err)
	}
	return nil
}

// EvalMinutes evaluates one `timeout-minutes` value for a matrix cell. A
// non-numeric or out-of-range result fails run creation (fail closed): the job
// must not be created with a timeout the author did not mean.
func EvalMinutes(expr string, github, matrix, inputs map[string]string) (int, error) {
	return EvalMinutesWithStrategy(expr, github, matrix, inputs, nil)
}

// EvalMinutesWithStrategy is EvalMinutes plus the matrix cell's `strategy.*`
// context (PAR-039).
func EvalMinutesWithStrategy(expr string, github, matrix, inputs, strategy map[string]string) (int, error) {
	out, err := EvalString(expr, EvalScope{GitHub: github, Matrix: matrix, Inputs: inputs, Strategy: strategy})
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", out)
	}
	if n < 1 || n > MaxTimeoutMinutes {
		return 0, fmt.Errorf("must be 1…%d, got %d", MaxTimeoutMinutes, n)
	}
	return n, nil
}

// RunNameMaxLen bounds the rendered run title (a list-page cell, not a payload).
const RunNameMaxLen = 256

// RenderRunName renders a workflow's `run-name:` for one run (PAR-057). The
// parse-time validator has already closed its vocabulary to github.* and
// inputs.*; a reference with no value renders empty (display-only, like a job
// name) and the result is trimmed to one bounded line.
func RenderRunName(tmpl string, github, inputs map[string]string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return "", nil
	}
	parts, err := splitTemplateExpr(tmpl)
	if err != nil {
		return "", err
	}
	scope := EvalScope{GitHub: github, Inputs: inputs, Site: SiteRunName}
	var b strings.Builder
	for _, p := range parts {
		if p.literal != "" {
			b.WriteString(p.literal)
			continue
		}
		expr := p.expr
		if p.ref != nil {
			expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(p.ref.Raw, "${{"), "}}"))
		}
		val, eerr := EvalString(expr, scope)
		if eerr != nil {
			return "", eerr
		}
		b.WriteString(val)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if runes := []rune(out); len(runes) > RunNameMaxLen {
		out = strings.TrimSpace(string(runes[:RunNameMaxLen]))
	}
	return out, nil
}
