package threads

import (
	"strings"
	"testing"
)

func TestEvalIf(t *testing.T) {
	ctx := ExprContext{
		Success: true, Failure: false, Cancelled: false,
		GitHub: map[string]string{"ref": "refs/heads/main", "event_name": "push"},
		Matrix: map[string]string{"go": "1.27"},
		Env:    map[string]string{"FOO": "bar"},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"", true},
		{"success()", true},
		{"failure()", false},
		{"always()", true},
		{"cancelled()", false},
		{"!failure()", true},
		{"github.ref == 'refs/heads/main'", true},
		{"github.ref == 'refs/heads/dev'", false},
		{"github.ref != 'refs/heads/dev'", true},
		{"matrix.go == '1.27'", true},
		{"matrix.go == '1.26'", false},
		{"env.FOO == 'bar'", true},
		{"success() && github.event_name == 'push'", true},
		{"failure() || matrix.go == '1.27'", true},
		{"(success() || failure()) && !cancelled()", true},
		{"true", true},
		{"false", false},
	}
	for _, tc := range cases {
		got, err := EvalIf(tc.expr, ctx)
		if err != nil {
			t.Errorf("EvalIf(%q): %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("EvalIf(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalIfErrorsAreExplicit(t *testing.T) {
	ctx := ExprContext{Success: true}
	for _, expr := range []string{
		"contains(github.ref, 'x')",
		"nope == 'x'",
		"github.ref ==",
		"'unterminated",
		"(success()",
		"steps.build",
		"steps.build.verdict == 'success'",
		"needs.build",
		"needs.build.outcome == 'success'",
	} {
		if _, err := EvalIf(expr, ctx); err == nil {
			t.Errorf("EvalIf(%q) accepted an unsupported expression", expr)
		} else if !strings.Contains(err.Error(), "if:") {
			t.Errorf("EvalIf(%q) error = %v, want an if: prefix", expr, err)
		}
	}
}

// TestEvalIfStepAndNeedsContexts pins the contexts a step id and a job
// dependency expose: outcome vs conclusion (continue-on-error), a known step's
// unset output (empty), and needs.<job>.result.
func TestEvalIfStepAndNeedsContexts(t *testing.T) {
	ctx := ExprContext{
		Success: true,
		Steps: map[string]string{
			"lint":  "failure",
			"build": "success",
		},
		StepConclusions: map[string]string{
			"lint":  "success", // failed with continue-on-error
			"build": "success",
		},
		Needs: map[string]string{
			"build": "failure",
			"lint":  "success",
		},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"steps.lint.outcome == 'failure'", true},
		{"steps.lint.conclusion == 'success'", true},
		{"steps.lint.outcome == steps.lint.conclusion", false},
		{"steps.build.outcome == 'success'", true},
		{"steps.BUILD.outcome == 'success'", true}, // ids are case-insensitive
		{"steps.nope.outcome == ''", true},         // an unknown id reads empty
		{"steps.build.outputs.digest == ''", true}, // known step, unset output
		{"needs.build.result == 'failure'", true},
		{"needs.build.result != 'success'", true},
		{"needs.lint.result == 'success'", true},
		{"needs.build.outputs.anything == ''", true},
		{"needs.ghost.result == 'success'", false},
		{"steps.lint.outcome == 'failure' && needs.build.result == 'failure'", true},
	}
	for _, tc := range cases {
		got, err := EvalIf(tc.expr, ctx)
		if err != nil {
			t.Errorf("EvalIf(%q): %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("EvalIf(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestValueTruthy pins the literal coercion (0/empty false, 1/true true).
func TestValueTruthy(t *testing.T) {
	if b, err := (value{s: "0"}).truthy(); err != nil || b {
		t.Fatal("truthy(0)")
	}
	if b, err := (value{s: "1"}).truthy(); err != nil || !b {
		t.Fatal("truthy(1)")
	}
	if _, err := (value{s: "maybe"}).truthy(); err == nil {
		t.Fatal("truthy(maybe) must error")
	}
}
