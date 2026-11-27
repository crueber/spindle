package workflow

// Evaluator coverage (PAR-038): the same table the threads package drives
// through `EvalIf`, exercised here so the package that owns the language carries
// its tests (the per-package coverage gate).

import (
	"strings"
	"testing"
)

func evalTestScope() EvalScope {
	return EvalScope{
		Success: true,
		GitHub: map[string]string{
			"sha": "abc123", "ref": "refs/heads/main", "event_name": "push",
			"event.inputs.mode": "fast", "event.release.tag_name": "v1.2.3",
			"action_path": "/actions/x",
		},
		Runner:          map[string]string{"os": "Linux"},
		Vars:            map[string]string{"REGION": "eu"},
		Matrix:          map[string]string{"go": "1.27"},
		Inputs:          map[string]string{"mode": "fast"},
		Env:             map[string]string{"FOO": "bar"},
		Secrets:         map[string]string{"TOKEN": "s3cret"},
		Steps:           map[string]string{"build": "failure", "test": "success"},
		StepConclusions: map[string]string{"build": "success", "test": "success"},
		Outputs: map[string]map[string]string{
			"build": {"digest": "deadbeef", "empty": ""},
		},
		Needs:        map[string]string{"prep": "success", "lint": "failure"},
		NeedsOutputs: map[string]map[string]string{"prep": {"artifact": "dist"}},
		Site:         SiteStepIf,
	}
}

func TestEvalIfAndEvalStringTables(t *testing.T) {
	c := evalTestScope()
	if ok, err := EvalIf("", c); err != nil || !ok {
		t.Fatalf("empty = %v, %v", ok, err)
	}
	if ok, err := EvalIf("${{ inputs.mode == 'fast' }}", c); err != nil || !ok {
		t.Fatalf("whole span = %v, %v", ok, err)
	}
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{"1 == '1'", true},
		{"true == 'true'", true},
		{"1 != 2", true},
		{"'a' < 'b'", true},
		{"'b' <= 'a'", false},
		{"'2' < '10'", true},
		{"2 > 1", true},
		{"2 >= 3", false},
		{"!failure()", true},
		{"!(success() && failure())", true},
		{"false || true", true},
		{"true && false", false},
		{"github.sha == 'abc123'", true},
		{"github['sha'] == 'abc123'", true},
		{"github.event.inputs.mode == 'fast'", true},
		{"github.event.release.tag_name == 'v1.2.3'", true},
		{"github.action_path == '/actions/x'", true},
		{"runner.os == 'Linux'", true},
		{"vars.region == 'eu'", true},
		{"matrix.go == '1.27'", true},
		{"inputs.mode == 'fast'", true},
		{"env.FOO == 'bar'", true},
		{"secrets.token == 's3cret'", true},
		{"steps.build.outcome == 'failure'", true},
		{"steps.build.conclusion == 'success'", true},
		{"steps['build'].outputs['digest'] == 'deadbeef'", true},
		{"steps.build.outputs.empty == ''", true},
		{"needs['prep'].outputs['artifact'] == 'dist'", true},
		{"contains(steps.*.outcome, 'failure')", true},
		{"contains(steps.*.conclusion, 'success')", true},
		{"contains(steps.*.outputs.digest, 'deadbeef')", true},
		{"contains(needs.*.outputs.artifact, 'dist')", true},
		{"contains('abc', 'b')", true},
		{"startsWith(github.ref, 'refs/heads/')", true},
		{"endsWith(github.ref, '/main')", true},
		{"format('{0}/{1}', github.event_name, inputs.mode) == 'push/fast'", true},
		{"contains('x', 'y') == false", true},
	} {
		got, err := EvalIf(tc.expr, c)
		if err != nil || got != tc.want {
			t.Errorf("EvalIf(%q) = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
	// A dependency RESULT is a job-if context only (PAR-040); the runner sees
	// the outputs, so the same names are a named error at a step site.
	jc := evalTestScope()
	jc.Site = SiteJobIf
	for _, tc := range []struct {
		expr string
		want any
	}{
		{"needs.prep.result == 'success'", true},
		{"contains(needs.*.result, 'failure')", true},
		{"join(needs.*.result, ',') == 'failure,success'", true},
	} {
		got, err := EvalIf(tc.expr, jc)
		if err != nil || got != tc.want {
			t.Errorf("job EvalIf(%q) = %v, %v", tc.expr, got, err)
		}
	}
	if got, err := EvalString("join(needs.*.result, '/')", jc); err != nil || got != "failure/success" {
		t.Fatalf("job join = %q, %v", got, err)
	}

	// Value-returning operators.
	for _, tc := range []struct{ expr, want string }{
		{"github.ref == 'refs/heads/main' && 'prod' || 'dev'", "prod"},
		{"github.ref == 'refs/heads/dev' && 'prod' || 'dev'", "dev"},
		{"'x' && 'y'", "y"},
		{"'' || 'fallback'", "fallback"},
		{"format('{{{0}}}', 'v')", "{v}"},
		{"format('{9}', 'v')", "{9}"},
		{"format('{', 'v')", "{"},
	} {
		got, err := EvalString(tc.expr, c)
		if err != nil || got != tc.want {
			t.Errorf("EvalString(%q) = %q, %v; want %q", tc.expr, got, err, tc.want)
		}
	}
}

func TestEvalErrorsAreNamed(t *testing.T) {
	c := evalTestScope()
	for _, expr := range []string{
		"@",
		"1 <",
		"(success()",
		"'unterminated",
		"toJSON(github)",
		"startsWith(github.ref)",
		"format()",
		"hashFiles()",
		"nope.value",
		"github.nope",
		"github.*.sha",
		"runner.nope",
		"vars.a.b",
		"matrix.a.b",
		"inputs.a.b",
		"env.a.b",
		"secrets.1BAD",
		"steps.build",
		"steps.build.verdict",
		"steps.ghost.outputs.x",
		"steps.*.wat",
		"needs.prep",
		"needs.ghost.outputs.x",
		"needs.*.wat",
		"github['sha'",
		"github[sha]",
		"github['sha'].",
	} {
		if _, err := EvalIf(expr, c); err == nil {
			t.Errorf("EvalIf(%q) accepted", expr)
		}
	}
	if _, err := EvalString("", c); err == nil {
		t.Fatal("an empty expression string was accepted")
	}
	if _, err := EvalString("true false", c); err == nil {
		t.Fatal("trailing input accepted")
	}
	// A non-boolean final condition stays an error (the documented strictness).
	if _, err := EvalIf("'maybe'", c); err == nil {
		t.Fatal("a non-boolean condition was accepted")
	}
	if _, err := EvalIf("!env.FOO", c); err == nil {
		t.Fatal("! on a non-boolean was accepted")
	}
	// Context availability is enforced at evaluation time too.
	job := c
	job.Site = SiteJobIf
	if _, err := EvalIf("runner.os == 'Linux'", job); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("job runner = %v", err)
	}
}

func TestEvalListEqualityAndNumbers(t *testing.T) {
	c := evalTestScope()
	c.Site = SiteJobIf
	c.GitHub["event.inputs.mode"] = "1"
	if ok, err := EvalIf("needs.*.result == needs.*.result", c); err != nil || !ok {
		t.Fatalf("list equality = %v, %v", ok, err)
	}
	if ok, err := EvalIf("needs.*.result == needs.*.outputs.artifact", c); err != nil || ok {
		t.Fatalf("different lists = %v, %v", ok, err)
	}
	if ok, err := EvalIf("'1' == 1", c); err != nil || !ok {
		t.Fatalf("numeric equality = %v, %v", ok, err)
	}
}

func TestEvalHashFilesProvider(t *testing.T) {
	c := evalTestScope()
	c.HashFiles = func(patterns []string) (string, error) {
		if strings.Join(patterns, ",") != "**/go.sum,**/go.mod" {
			t.Fatalf("patterns = %v", patterns)
		}
		return "sum", nil
	}
	if ok, err := EvalIf("hashFiles('**/go.sum', '**/go.mod') == 'sum'", c); err != nil || !ok {
		t.Fatalf("hashFiles = %v, %v", ok, err)
	}
	c.HashFiles = func([]string) (string, error) { return "", nil }
	if got, err := EvalString("hashFiles('none/*')", c); err != nil || got != "" {
		t.Fatalf("no match = %q, %v", got, err)
	}
	job := c
	job.Site = SiteJobIf
	job.HashFiles = func([]string) (string, error) { return "x", nil }
	if _, err := EvalIf("hashFiles('x') != ''", job); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("job hashFiles = %v", err)
	}
}

func TestEvalMoreBranches(t *testing.T) {
	c := evalTestScope()
	jc := evalTestScope()
	jc.Site = SiteJobIf
	// A list renders comma-joined and is truthy by length.
	if got, err := EvalString("needs.*.result", jc); err != nil || got != "failure,success" {
		t.Fatalf("list text = %q, %v", got, err)
	}
	if got, err := EvalString("format('{0}', needs.*.result)", jc); err != nil || got != "failure,success" {
		t.Fatalf("format list = %q, %v", got, err)
	}
	if ok, err := EvalIf("needs.*.result", jc); err != nil || !ok {
		t.Fatalf("list truthiness = %v, %v", ok, err)
	}
	if ok, err := EvalIf("'' < 'a'", c); err != nil || !ok {
		t.Fatalf("empty lexical = %v, %v", ok, err)
	}
	if ok, err := EvalIf("${{ }}", c); err != nil || !ok {
		t.Fatalf("empty span = %v, %v", ok, err)
	}
	// Every zero-arity status function and both cancelled spellings.
	for _, expr := range []string{"success()", "always()"} {
		if ok, err := EvalIf(expr, c); err != nil || !ok {
			t.Errorf("%s = %v, %v", expr, ok, err)
		}
	}
	for _, expr := range []string{"failure()", "cancelled()", "canceled()"} {
		if ok, err := EvalIf(expr, c); err != nil || ok {
			t.Errorf("%s = %v, %v", expr, ok, err)
		}
	}
	// Argument-count errors for every function arm.
	for _, expr := range []string{
		"success(1)", "failure(1)", "always(1)", "cancelled(1)",
		"startsWith('a')", "endsWith('a')", "contains('a')", "join('a')",
		"join('a', 'b', 'c')", "hashFiles()",
	} {
		if _, err := EvalIf(expr, c); err == nil {
			t.Errorf("%s accepted a wrong arity", expr)
		}
	}
	// A filter list against a scalar is false, not an error.
	if ok, err := EvalIf("needs.*.result == 'success'", jc); err != nil || ok {
		t.Fatalf("list/scalar equality = %v, %v", ok, err)
	}
	// A bare non-boolean value is a named error.
	if _, err := EvalIf("needs.prep.result", jc); err == nil {
		t.Fatal("a bare non-boolean needs result was accepted")
	}
	// Indexed references evaluate the same as dotted ones.
	if got, err := EvalString("github['sha']", c); err != nil || got != "abc123" {
		t.Fatalf("indexed = %q, %v", got, err)
	}
	// A whole-span check with trailing text is not a whole span.
	if _, err := EvalIf("${{ inputs.mode }} extra", c); err == nil {
		t.Fatal("trailing text after a span was accepted")
	}
}

func TestSiteStringsAndContexts(t *testing.T) {
	want := map[Site]string{
		SiteStepField: "a step field", SiteStepIf: "a step if:", SiteJobIf: "a job if:",
		SiteJobName: "a job name", SiteJobGroup: "a job concurrency.group",
		SiteWorkflowGroup: "a workflow concurrency.group", SiteJobOutputs: "a job outputs value",
		SiteRunName: "run-name", SiteRunsOn: "runs-on", SiteDefaultsRun: "defaults.run",
		SiteCallWith: "a reusable-workflow with: value", SiteCallOutput: "a workflow_call output value",
		SiteJobTimeout: "a job timeout-minutes", SiteStepTimeout: "a step timeout-minutes",
	}
	for site, label := range want {
		if got := site.String(); got != label {
			t.Errorf("%d.String() = %q, want %q", site, got, label)
		}
	}
	if got := Site(SiteStepTimeout + 99).String(); got != "a step field" {
		t.Errorf("unknown site = %q", got)
	}
	for _, site := range []Site{SiteStepField, SiteStepIf, SiteJobOutputs} {
		if !siteAllowsHashFiles(site) {
			t.Errorf("%s should allow hashFiles", site)
		}
	}
	if siteAllowsHashFiles(SiteJobIf) {
		t.Error("job if: allows hashFiles")
	}
}

func TestEmptySubstitutionSpan(t *testing.T) {
	if err := ValidateTemplate("${{ }}", TemplateScope{}); err == nil {
		t.Fatal("an empty span was accepted")
	}
	if _, err := Substitute("${{ }}", TemplateValues{}); err == nil {
		t.Fatal("an empty span substituted")
	}
}

func TestFillTemplateEvaluatesExpressions(t *testing.T) {
	if got := FillTemplate("n-${{ matrix.a == 'x' && 'yes' || 'no' }}", nil, map[string]string{"a": "x"}, nil); got != "n-yes" {
		t.Fatalf("expression name = %q", got)
	}
	if got := FillTemplate("${{ github.ref }}", map[string]string{"ref": "refs/heads/main"}, nil, nil); got != "refs/heads/main" {
		t.Fatalf("reference name = %q", got)
	}
	// An expression the display site cannot evaluate stays visible.
	if got := FillTemplate("${{ secrets.X }}", nil, nil, nil); got != "${{ secrets.X }}" {
		t.Fatalf("refused expression = %q", got)
	}
	if got := FillTemplate("${{ bad", nil, nil, nil); got != "${{ bad" {
		t.Fatalf("malformed = %q", got)
	}
}
