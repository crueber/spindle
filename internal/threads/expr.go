package threads

import (
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// `if:` expressions (15 §1/§4; docs/threads.md §3.7). The evaluator lives in
// internal/threads/workflow/expr_eval.go, beside the parser that validates the
// same grammar: BOTH ends evaluate conditions (the runner a step `if:`, the
// server a job-level `if:` and every substitution site), and two parsers would
// drift. These aliases keep the server's and the runner's call sites reading
// naturally.
//
// The supported subset is documented in docs/threads.md §3.7 and
// docs/WORKFLOWS.md §5.3: success/failure/always/cancelled, startsWith,
// endsWith, contains, format, join, hashFiles; the github/forgejo/forge,
// runner, vars, matrix, inputs, env, secrets, steps and needs contexts; and
// value-returning `!`, `==`, `!=`, `<`, `<=`, `>`, `>=`, `&&`, `||` and
// parentheses. Anything else is an evaluation ERROR (the caller fails the step
// or job with a clear message, never silently skips) and a parse error in the
// workflow package.

// ExprContext is what an `if` can read (workflow.EvalScope).
type ExprContext = workflow.EvalScope

// EvalIf evaluates one condition (workflow.EvalIf).
func EvalIf(expr string, c ExprContext) (bool, error) { return workflow.EvalIf(expr, c) }

// EvalString evaluates one `${{ … }}` expression and renders its value.
func EvalString(expr string, c ExprContext) (string, error) { return workflow.EvalString(expr, c) }

// lookupVar resolves `vars.<name>` case-insensitively (variables are stored in
// their canonical spelling and referenced freely).
func lookupVar(values map[string]string, name string) string {
	if val, ok := values[name]; ok {
		return val
	}
	for key, val := range values {
		if strings.EqualFold(key, name) {
			return val
		}
	}
	return ""
}
