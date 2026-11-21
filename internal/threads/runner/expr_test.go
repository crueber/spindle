package runner

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
	} {
		if _, err := EvalIf(expr, ctx); err == nil {
			t.Errorf("EvalIf(%q) accepted an unsupported expression", expr)
		} else if !strings.Contains(err.Error(), "if:") {
			t.Errorf("EvalIf(%q) error = %v, want an if: prefix", expr, err)
		}
	}
}
