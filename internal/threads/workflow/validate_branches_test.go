package workflow

// Targeted branches of validateRef/validateReference and splitRef (the parser's
// per-context and indexed-reference edges), so the language package keeps its
// per-package coverage floor.

import (
	"strings"
	"testing"
)

func refOf(t *testing.T, expr string) TemplateRef {
	t.Helper()
	ref, ok := splitRef(expr)
	if !ok {
		t.Fatalf("splitRef(%q) failed", expr)
	}
	return *ref
}

func TestValidateRefBranches(t *testing.T) {
	base := TemplateScope{Axes: []string{"go"}, Inputs: []string{"mode"}, Steps: []string{"build"}, Needs: []string{"prep"}}
	cases := []struct {
		ref   string
		scope TemplateScope
		ok    bool
	}{
		{"github.ref", base, true},
		{"github.action_path", base, false},
		{"github.action_path", TemplateScope{ActionPath: true}, true},
		{"github.event.inputs.mode", base, true},
		{"github.event.inputs.other", base, false},
		{"github.event.release.tag_name", base, true},
		{"github.event.release.nope", base, false},
		{"runner.os", base, true},
		{"runner.os", TemplateScope{JobIf: true}, false},
		{"vars.REGION", base, true},
		{"matrix.go", base, true},
		{"matrix.go", TemplateScope{}, false},
		{"inputs.mode", base, true},
		{"inputs.mode", TemplateScope{}, false},
		{"env.FOO", base, true},
		{"secrets.TOKEN", base, true},
		{"secrets.TOKEN", TemplateScope{SecretsClosed: true}, false},
		{"secrets.TOKEN", TemplateScope{SecretsClosed: true, SecretNames: []string{"TOKEN"}}, true},
		{"steps.build.outputs.x", base, true},
		{"steps.ghost.outputs.x", base, false},
		{"needs.prep.outputs.x", base, true},
		{"needs.ghost.outputs.x", base, false},
		{"jobs.a.outputs.x", base, false},
		{"jobs.a.outputs.x", TemplateScope{CallOutput: true, Jobs: map[string][]string{"a": {"x"}}}, true},
		{"jobs.a.outputs.y", TemplateScope{CallOutput: true, Jobs: map[string][]string{"a": {"x"}}}, false},
		{"jobs.b.outputs.x", TemplateScope{CallOutput: true, Jobs: map[string][]string{"a": {"x"}}}, false},
		{"matrix.go", TemplateScope{CallWith: true}, false},
		{"github.sha", TemplateScope{CallWith: true}, true},
		{"nope.x", base, false},
	}
	for _, tc := range cases {
		err := validateRef(refOf(t, tc.ref), tc.scope)
		if tc.ok && err != nil {
			t.Errorf("validateRef(%q) refused: %v", tc.ref, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateRef(%q) accepted", tc.ref)
		}
	}
}

func TestValidateReferenceBranches(t *testing.T) {
	scope := TemplateScope{Axes: []string{"go"}, Inputs: []string{"mode"}, Steps: []string{"build"}, Needs: []string{"prep"}, Site: SiteStepIf}
	ok := []string{
		"github.event.inputs.mode == 'x'",
		"github.event.release.tag_name == 'x'",
		"github['sha'] == 'x'",
		"runner.os == 'Linux'",
		"vars.REGION == 'eu'",
		"matrix.go == '1.27'",
		"inputs.mode == 'fast'",
		"env.FOO == 'x'",
		"secrets.TOKEN == 'x'",
		"steps.build.outcome == 'failure'",
		"steps.build.conclusion == 'success'",
		"steps.build.outputs.x == 'y'",
		"steps.*.outcome == 'x'",
		"steps.*.conclusion == 'x'",
		"steps.*.outputs.x == 'y'",
		"needs.prep.result == 'success'",
		"needs.prep.outputs.x == 'y'",
		"needs.*.result == 'x'",
		"needs.*.outputs.x == 'y'",
		"true && (false || true)",
		"1.5 == 1.5",
	}
	for _, expr := range ok {
		if err := validateExpr(expr, scope); err != nil {
			t.Errorf("validateExpr(%q): %v", expr, err)
		}
	}
	bad := []string{
		"github.event.inputs.other == 'x'",
		"github.event.release.nope == 'x'",
		"github.event.inputs == 'x'",
		"runner.nope == 'x'",
		"vars.1BAD == 'x'",
		"matrix.nope == 'x'",
		"inputs.nope == 'x'",
		"env.a.b == 'x'",
		"secrets.1BAD == 'x'",
		"steps.ghost.outcome == 'x'",
		"steps.*.wat == 'x'",
		"needs.ghost.result == 'x'",
		"needs.*.wat == 'x'",
		"jobs.a.outputs.x == 'y'",
		"true true",
		"'a' 'b'",
		"(true",
		"true)",
		"github['sha'",
		"github[sha]",
	}
	for _, expr := range bad {
		if err := validateExpr(expr, scope); err == nil {
			t.Errorf("validateExpr(%q) accepted", expr)
		}
	}
	// A declared multi-segment output name is accepted.
	scope.Steps = []string{"build"}
	if err := validateExpr("steps.build.outputs.a.b == 'c'", scope); err != nil {
		t.Errorf("multi-segment output: %v", err)
	}
	// A job scope accepts secrets/vars, refuses steps/runner.
	scope.Site = SiteJobIf
	for _, expr := range []string{"secrets.X == 'y'", "vars.X == 'y'"} {
		if err := validateExpr(expr, scope); err != nil {
			t.Errorf("job scope %q: %v", expr, err)
		}
	}
	for _, expr := range []string{"steps.build.outcome == 'x'", "runner.os == 'x'"} {
		if err := validateExpr(expr, scope); err == nil || !strings.Contains(err.Error(), "not available") {
			t.Errorf("job scope %q = %v", expr, err)
		}
	}
	for _, expr := range []string{"needs.prep.result == 'success'", "needs.*.result == 'x'"} {
		if err := validateExpr(expr, scope); err != nil {
			t.Errorf("job needs result %q: %v", expr, err)
		}
	}
	if err := validateExpr("needs.ghost.result == 'x'", scope); err == nil {
		t.Error("job scope accepted a ghost need")
	}
}

func TestSplitRefIndexedAndMalformed(t *testing.T) {
	for _, expr := range []string{"github['sha']", "github[\"sha\"]", "steps['build'].outputs['x']"} {
		ref, ok := splitRef(expr)
		if !ok || ref.Context == "" {
			t.Errorf("splitRef(%q) failed", expr)
		}
	}
	for _, expr := range []string{"github[", "github['sha'", "github[sha]", "github['1BAD']"} {
		if _, ok := splitRef(expr); ok {
			t.Errorf("splitRef(%q) accepted a malformed index", expr)
		}
	}
	// A filter path is not a plain reference (it becomes an expression).
	if _, ok := splitRef("needs.*.result"); ok {
		t.Error("a filter path was accepted as a plain reference")
	}
}
