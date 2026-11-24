package threads

import (
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Step and job outputs (docs/threads.md §3.9). A step publishes outputs through
// the runner-provided file named by `$SPINDLE_OUTPUT` (aliased as
// `$GITHUB_OUTPUT`); the runner collects what a step with an `id:` wrote, and
// the server stores the job outputs the runner reports on complete.
//
// The caps live in the workflow package (the leaf both ends import), so the
// parser, the server and the runner share one bound; these wrappers keep the
// threads package's call sites reading naturally.

// SubstituteStepOutputs replaces every `${{ steps.<id>.outputs.<name> }}`
// reference in s and leaves every other `${{ … }}` span as written. It is
// strict: an unknown step id, or an output the step did not publish, is an error
// naming the reference (fail closed — an empty string would hide the bug).
func SubstituteStepOutputs(s string, outputs map[string]map[string]string) (string, error) {
	return workflow.FillStepOutputs(s, outputs)
}

// SubstituteNeedsOutputs is SubstituteStepOutputs for
// `${{ needs.<job>.outputs.<name> }}`.
func SubstituteNeedsOutputs(s string, needs map[string]map[string]string) (string, error) {
	return workflow.FillNeedsOutputs(s, needs)
}

// SubstituteOutputs replaces both kinds of output reference, in one pass over
// the string (a field may read an earlier step and a needed job).
func SubstituteOutputs(s string, steps, needs map[string]map[string]string) (string, error) {
	return workflow.FillOutputs(s, steps, needs)
}

// ValidateOutputs checks a collected output map against the documented caps and
// name charset. Both ends call it (the runner before reporting, the server
// before storing).
func ValidateOutputs(outs map[string]string) error {
	return workflow.ValidateOutputs(outs)
}
