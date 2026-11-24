package runner

import "github.com/crueber/spindle/internal/threads"

// The `if:` evaluator lives in the threads package (internal/threads/expr.go):
// the server evaluates a job-level `if:` with the same subset the runner
// evaluates a step `if:` with, and two parsers would drift. These aliases keep
// the runner's own call sites and tests reading naturally.

// ExprContext is what a step `if:` can read (threads.ExprContext).
type ExprContext = threads.ExprContext

// EvalIf evaluates one step condition (threads.EvalIf).
func EvalIf(expr string, c ExprContext) (bool, error) { return threads.EvalIf(expr, c) }

// ExpandGroup resolves a concurrency group template with the server's rule and
// strictness (threads.ExpandGroup): a reference that resolves to nothing is an
// error, never a literal. The runner reads the group already expanded in the
// job spec, but keeping the rule here means both ends agree by construction.
func ExpandGroup(tmpl string, github, matrix, inputs map[string]string) (string, error) {
	return threads.ExpandGroup(tmpl, github, matrix, inputs)
}
