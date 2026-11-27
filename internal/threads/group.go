package threads

import (
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Template expansion at run creation (15 §4, round-2 audit).
//
// A concurrency group is the lock that decides which run may execute: the group
// text is hashed into the holder index (`concurrencySlug`), so a template that
// silently collapses to literal text makes every branch share one key — and
// with `cancel-in-progress: true` a push on any branch cancels main's run. The
// expansion is therefore STRICT (ExpandGroup): every `${{ … }}` must resolve to
// a non-empty value, or the run is not created. The vocabulary is closed and
// shared with the parser (internal/threads/workflow/template.go), which fails
// the file at parse time when a reference cannot exist at all.
//
// Supported contexts, for both the parser and the expander:
//
//	github.ref  github.ref_name  github.repository  github.workflow
//	github.event_name  github.sha  github.run_id
//	matrix.<axis>  inputs.<name>
//
// There is no `spindle.*` expression alias: the SPINDLE_* names are plain
// environment variables, not an expression context. `env.*`, `steps.*`,
// `needs.*` and `secrets.*` are not resolvable here and are rejected.

// ExpandGroup resolves a concurrency group template against one job's context.
// The returned error wraps ErrUnprocessable and names the unresolved key.
func ExpandGroup(tmpl string, github, matrix, inputs map[string]string) (string, error) {
	return ExpandGroupWithStrategy(tmpl, github, matrix, inputs, nil)
}

// ExpandGroupWithStrategy is ExpandGroup plus the job cell's `strategy.*`
// context (PAR-039).
func ExpandGroupWithStrategy(tmpl string, github, matrix, inputs, strategy map[string]string) (string, error) {
	group, err := workflow.ExpandGroupWithStrategy(tmpl, github, matrix, inputs, strategy)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnprocessable, err)
	}
	return group, nil
}

// ExpandJobName resolves the references a job display name can carry. A name is
// display-only (nothing locks on it), so it is filled leniently: an unresolved
// reference stays visible instead of failing an otherwise runnable job.
func ExpandJobName(name string, github, matrix, inputs map[string]string) string {
	return ExpandJobNameWithStrategy(name, github, matrix, inputs, nil)
}

// ExpandJobNameWithStrategy is ExpandJobName plus the cell's `strategy.*`
// context (PAR-039).
func ExpandJobNameWithStrategy(name string, github, matrix, inputs, strategy map[string]string) string {
	return workflow.FillTemplateWithStrategy(name, github, matrix, inputs, strategy)
}

// jobNameNamesMatrix reports whether a display template already names a matrix
// axis. GitHub then does not append the "(v1, v2)" suffix: the author made the
// cells distinct themselves.
func jobNameNamesMatrix(name string) bool {
	refs, err := workflow.TemplateRefs(name)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if ref.Context == "matrix" {
			return true
		}
	}
	return false
}

// groupInputs is the inputs map a group template expands against: the run's
// effective values (already default-filled for a dispatch) plus each declared
// input's default, so a workflow that also fires on push still has a
// deterministic value for a declared, defaulted input. An input with no value
// and no default stays absent — and a group that names it fails the run.
func groupInputs(wf *workflow.Workflow, supplied map[string]string) map[string]string {
	if wf == nil || wf.On.WorkflowDispatch == nil || len(wf.On.WorkflowDispatch.Inputs) == 0 {
		return supplied
	}
	out := make(map[string]string, len(supplied)+len(wf.On.WorkflowDispatch.Inputs))
	for k, v := range supplied {
		out[k] = v
	}
	for name, in := range wf.On.WorkflowDispatch.Inputs {
		if strings.TrimSpace(out[name]) != "" {
			continue
		}
		if in.Default != nil {
			out[name] = strings.TrimSpace(*in.Default)
		}
	}
	return out
}
