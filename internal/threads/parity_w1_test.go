package threads

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Parity wave 1a server-side halves: matrix include/exclude expansion and the
// `if:` function/context surface (the parser's half lives in
// internal/threads/workflow).

func TestExpandMatrixIncludeExclude(t *testing.T) {
	cases := []struct {
		name  string
		strat *StrategySpec
		want  []string // sorted "k=v,k=v" cells in expansion order
	}{
		{
			name:  "plain product",
			strat: &StrategySpec{Matrix: map[string][]string{"go": {"1.26", "1.27"}}},
			want:  []string{"go=1.26", "go=1.27"},
		},
		{
			name: "include-only (scriobh)",
			strat: &StrategySpec{Include: []map[string]string{
				{"goos": "linux", "goarch": "amd64"},
				{"goos": "linux", "goarch": "arm64"},
			}},
			want: []string{"goarch=amd64,goos=linux", "goarch=arm64,goos=linux"},
		},
		{
			name: "exclude and include",
			strat: &StrategySpec{
				Matrix:  map[string][]string{"go": {"1.26", "1.27"}, "os": {"linux"}},
				Exclude: []map[string]string{{"go": "1.26"}},
				Include: []map[string]string{{"experimental": "true"}},
			},
			want: []string{"experimental=true,go=1.27,os=linux"},
		},
		{
			name:  "empty axis values is not a matrix",
			strat: &StrategySpec{Matrix: map[string][]string{"go": {}}},
			want:  []string{""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cells := expandMatrix(tc.strat)
			if len(cells) != len(tc.want) {
				t.Fatalf("cells = %d (%v), want %d", len(cells), cells, len(tc.want))
			}
			for i, cell := range cells {
				parts := make([]string, 0, len(cell.Values))
				for k, v := range cell.Values {
					parts = append(parts, k+"="+v)
				}
				// The order of display keys is pinned by the workflow package;
				// here only the set matters.
				got := strings.Join(sortStrings(parts), ",")
				want := strings.Join(sortStrings(strings.Split(tc.want[i], ",")), ",")
				if got != want {
					t.Fatalf("cell %d = %q, want %q", i, got, want)
				}
			}
			if tc.name == "include-only (scriobh)" {
				if got := cells[0].Suffix; got != " (linux, amd64)" && got != " (amd64, linux)" {
					t.Fatalf("include-only suffix = %q", got)
				}
			}
		})
	}
	// An over-cap declaration is refused by the server safety net too.
	big := &StrategySpec{Matrix: map[string][]string{}}
	for i := 0; i < 10; i++ {
		big.Matrix[string(rune('a'+i))] = []string{"1", "2", "3"}
	}
	if _, err := matrixProductSize(big); err == nil {
		t.Fatal("over-cap spec accepted")
	}
}

func sortStrings(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestEvalIfFunctions(t *testing.T) {
	ctx := ExprContext{
		Success: true,
		GitHub:  map[string]string{"ref": "refs/tags/v1.2.3", "actor": "ada"},
		Matrix:  map[string]string{"go": "1.27"},
		Inputs:  map[string]string{"mode": "fast"},
		Secrets: map[string]string{"TOKEN": "s3cr3t"},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"startsWith(github.ref, 'refs/tags/v')", true},
		{"startsWith(github.ref, 'refs/heads/')", false},
		{"contains(github.ref, 'v1.2')", true},
		{"contains(format('{0}-{1}', matrix.go, inputs.mode), '1.27-fast')", true},
		{"format('{0}/{1}', 'a', 'b') == 'a/b'", true},
		{"format('{{literal}}') == '{literal}'", true},
		{"secrets.TOKEN == 's3cr3t'", true},
		{"secrets.token == 's3cr3t'", true}, // secret names are case-insensitive
		{"secrets.MISSING == ''", true},     // absent secret reads empty, never the literal
		{"forgejo.actor == github.actor", true},
		{"github.event.inputs.mode == 'fast'", true},
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
	// Every accepted github/forgejo field resolves in an if: (PAR-032), and the
	// aliases are value-identical.
	fields := map[string]string{
		"event_name": "push", "sha": "abc", "ref": "refs/heads/main",
		"ref_name": "main", "repository": "o/r", "workflow": "CI",
		"job": "build", "run_id": "r1", "actor": "ada",
	}
	fieldCtx := ExprContext{Success: true, GitHub: fields}
	for name, want := range fields {
		for _, context := range []string{"github", "forgejo", "forge"} {
			ok, err := EvalIf(context+"."+name+" == '"+want+"'", fieldCtx)
			if err != nil || !ok {
				t.Errorf("%s.%s = %v, %v; want the value", context, name, ok, err)
			}
		}
	}
	// A whole-span condition is accepted (the documented substitution form).
	if ok, err := EvalIf("${{ inputs.mode }}", ExprContext{Success: true, Inputs: map[string]string{"mode": "true"}}); err != nil || !ok {
		t.Fatalf("whole span = %v, %v", ok, err)
	}
	for _, expr := range []string{
		"endsWith(github.ref, 'v1')",
		"startsWith(github.ref)",
		"format()",
		"github.head_ref == 'x'",
		"secrets.1BAD == 'x'",
	} {
		if _, err := EvalIf(expr, ctx); err == nil {
			t.Errorf("EvalIf(%q) accepted an unsupported expression", expr)
		}
	}
}

// TestStrategySpecCarriesIncludeExclude pins the parser→spec seam: the server
// expands exactly what the file declared.
func TestStrategySpecCarriesIncludeExclude(t *testing.T) {
	in := &workflow.Strategy{Matrix: workflow.Matrix{
		Axes:    map[string][]string{"go": {"1.27"}},
		Include: []workflow.MatrixCombination{{"experimental": "true"}},
		Exclude: []workflow.MatrixCombination{{"go": "1.26"}},
	}}
	out := strategySpec(in)
	if out == nil || len(out.Matrix["go"]) != 1 {
		t.Fatalf("axes = %+v", out)
	}
	if len(out.Include) != 1 || out.Include[0]["experimental"] != "true" {
		t.Fatalf("include = %+v", out.Include)
	}
	if len(out.Exclude) != 1 || out.Exclude[0]["go"] != "1.26" {
		t.Fatalf("exclude = %+v", out.Exclude)
	}
	// The copies are independent of the parsed document.
	out.Include[0]["experimental"] = "mutated"
	if in.Matrix.Include[0]["experimental"] != "true" {
		t.Fatal("strategySpec aliased the parsed include map")
	}
}

// TestEvalIfFunctionEdges pins the function parser/format edge cases and the
// error surface the validator and the evaluator share.
func TestEvalIfFunctionEdges(t *testing.T) {
	ctx := ExprContext{Success: true, GitHub: map[string]string{"ref": "refs/heads/main"}}
	cases := []struct{ expr, want string }{
		{"format('plain')", "plain"},
		{"format('{0}/{1}', 'a', 'b')", "a/b"},
		{"format('{1}-{0}', 'a', 'b')", "b-a"},
		{"format('{9}', 'a')", "{9}"}, // out of range is left visible
		{"format('{x}', 'a')", "{x}"}, // non-numeric index is left visible
		{"format('{', 'a')", "{"},
		{"format('a}b')", "a}b"},
		{"format('{{x}}')", "{x}"},
		{"contains('abc', 'b')", "true"},
		{"startsWith('abc', 'ab')", "true"},
	}
	for _, tc := range cases {
		got, err := EvalIf(tc.expr+" == '"+tc.want+"'", ctx)
		if err != nil || !got {
			t.Errorf("EvalIf(%q) = %v, %v; want %q", tc.expr, got, err, tc.want)
		}
	}
	if _, err := EvalIf("canceled()", ctx); err != nil { // the synonym parses
		t.Fatalf("canceled(): %v", err)
	}
	for _, expr := range []string{
		"success(true)",
		"failure(true)",
		"always(true)",
		"cancelled(true)",
		"true false",
		"!@",
		"(@)",
		"format()",
		"format('{0}', 'a' 'b')",
		"github",
		"github.event",
		"github.event.inputs",
		"secrets",
		"secrets.A.B",
		"contains('a')",
		"always(1)",
		"failure(1)",
		"cancelled(1)",
		"success() ||",
		"success() &&",
		"${{ inputs.a }} ${{ inputs.b }}",
	} {
		if _, err := EvalIf(expr, ctx); err == nil {
			t.Errorf("EvalIf(%q) accepted", expr)
		}
	}
}
