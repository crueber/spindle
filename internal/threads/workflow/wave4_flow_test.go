package workflow

// Wave 4: expressions, context availability, runs-on substitution, defaults,
// timeout forms, max-parallel and run-name (PAR-038…PAR-058). These are the
// parser/validation halves; the evaluator and scheduler halves are pinned in
// internal/threads/wave4_*_test.go and internal/threads/runner/hashfiles_test.go.

import (
	"strings"
	"testing"
)

// TestExpressionOperatorsAccepted pins the PAR-038 grammar at parse time,
// including the indexed property spelling and the documented filters.
func TestExpressionOperatorsAccepted(t *testing.T) {
	scope := TemplateScope{
		Axes:   []string{"go", "os"},
		Inputs: []string{"mode"},
		Steps:  []string{"build"},
		Site:   SiteStepIf,
	}
	ok := []string{
		"1 == '1'",
		"true == 'true'",
		"'a' < 'b'",
		"'2' < '10'",
		"github['sha'] == github.sha",
		"github['event_name'] == 'push'",
		"!failure() && (success() || cancelled())",
		"matrix.go >= '1.26' && matrix.go <= '1.27'",
		"contains(steps.*.outcome, 'failure')",
		"needs.*.result == 'success'",
		"contains(needs.*.result, 'failure')",
		"steps['build'].outputs['sum'] != ''",
	}
	// steps.* needs a declared step id only for the direct form.
	for _, expr := range ok {
		t.Run(expr, func(t *testing.T) {
			if err := ValidateIf(expr, scope); err != nil {
				t.Fatalf("ValidateIf(%q): %v", expr, err)
			}
		})
	}
	bad := []struct{ expr, want string }{
		{"1 <", "expected a value"},
		{"github['sha'] [", "property access"},
		{"steps['ghost'].outputs.sum", "not declared before this one"},
		{"needs.*.wat == 'x'", "needs.* supports"},
		{"steps.*.wat == 'x'", "steps.* supports"},
		{"github.*.sha == 'x'", "object filters are not supported"},
	}
	for _, tc := range bad {
		err := ValidateIf(tc.expr, scope)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateIf(%q) = %v, want %q", tc.expr, err, tc.want)
		}
	}
}

// TestSubstitutionAcceptsExpressions pins that a `${{ … }}` span in a
// substituted site may be a whole expression (PAR-038), not only a reference.
func TestSubstitutionAcceptsExpressions(t *testing.T) {
	scope := TemplateScope{Axes: []string{"go"}, Inputs: []string{"flag"}, Site: SiteStepField}
	ok := []string{
		"${{ github.ref == 'refs/heads/main' && 'prod' || 'dev' }}",
		"prefix-${{ 1 + 1 }}", // `+` is not an operator: this must be refused
	}
	if err := ValidateTemplate(ok[0], scope); err != nil {
		t.Fatalf("expression span rejected: %v", err)
	}
	if err := ValidateTemplate("${{ github['sha'] }}", scope); err != nil {
		t.Fatalf("indexed reference rejected: %v", err)
	}
	// `+` has no place in the language; the error must name what is supported,
	// not silently pass the text to a shell.
	if err := ValidateTemplate(ok[1], scope); err == nil {
		t.Fatal("arithmetic accepted")
	}
}

// TestContextAvailabilityMatrix is the PAR-040 acceptance table: a context
// outside its site's documented set is a named error, and the documented set is
// accepted. It is a table so a new site or context cannot be added implicitly.
func TestContextAvailabilityMatrix(t *testing.T) {
	base := TemplateScope{Axes: []string{"go"}, Inputs: []string{"mode"}, Steps: []string{"build"}, Needs: []string{"prep"}}
	cases := []struct {
		site  Site
		ref   string
		allow bool
	}{
		{SiteStepField, "${{ secrets.TOKEN }}", true},
		{SiteStepField, "${{ vars.REGION }}", true},
		{SiteStepField, "${{ runner.os }}", true},
		{SiteStepField, "${{ steps.build.outputs.x }}", true},
		{SiteStepIf, "${{ runner.os }}", true},
		{SiteStepIf, "${{ steps.build.outcome }}", true},
		{SiteJobIf, "${{ secrets.TOKEN }}", true},
		{SiteJobIf, "${{ vars.REGION }}", true},
		{SiteJobIf, "${{ needs.prep.outcome }}", false}, // not a supported needs field
		{SiteJobIf, "${{ needs.prep.result }}", true},
		{SiteJobIf, "${{ runner.os }}", false},
		{SiteJobIf, "${{ steps.build.outcome }}", false},
		{SiteJobName, "${{ vars.REGION }}", true},
		{SiteJobName, "${{ secrets.TOKEN }}", false},
		{SiteJobName, "${{ steps.build.outcome }}", false},
		{SiteJobGroup, "${{ matrix.go }}", true},
		{SiteJobGroup, "${{ vars.REGION }}", false},
		{SiteWorkflowGroup, "${{ matrix.go }}", false},
		{SiteWorkflowGroup, "${{ inputs.mode }}", true},
		{SiteJobOutputs, "${{ steps.build.outputs.x }}", true},
		{SiteJobOutputs, "${{ secrets.TOKEN }}", true},
		{SiteJobOutputs, "${{ needs.prep.outputs.x }}", true},
		{SiteRunName, "${{ github.ref }}", true},
		{SiteRunName, "${{ inputs.mode }}", true},
		{SiteRunName, "${{ vars.REGION }}", false},
		{SiteRunName, "${{ secrets.TOKEN }}", false},
		{SiteRunsOn, "${{ matrix.go }}", true},
		{SiteRunsOn, "${{ inputs.mode }}", true},
		{SiteRunsOn, "${{ env.FOO }}", false},
		{SiteDefaultsRun, "${{ matrix.go }}", false},
		{SiteCallWith, "${{ github.sha }}", true},
		{SiteCallWith, "${{ matrix.go }}", false},
		{SiteJobTimeout, "${{ matrix.go }}", true},
		{SiteJobTimeout, "${{ secrets.TOKEN }}", false},
		{SiteStepTimeout, "${{ inputs.mode }}", true},
	}
	for _, tc := range cases {
		scope := base
		scope.Site = tc.site
		err := ValidateTemplate(tc.ref, scope)
		if tc.allow && err != nil {
			t.Errorf("%s: %s refused: %v", tc.site, tc.ref, err)
		}
		if !tc.allow && err == nil {
			t.Errorf("%s: %s accepted", tc.site, tc.ref)
		}
		if !tc.allow && err != nil && !strings.Contains(err.Error(), "not available") &&
			!strings.Contains(err.Error(), "not a declared") && !strings.Contains(err.Error(), "not a valid") &&
			!strings.Contains(err.Error(), "only needs") && !strings.Contains(err.Error(), "only steps") {
			t.Errorf("%s: %s error does not name the site/set: %v", tc.site, tc.ref, err)
		}
	}
	// Every site documents a non-empty context set (the docs table is generated
	// from this function).
	for _, site := range []Site{SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobGroup, SiteWorkflowGroup, SiteJobOutputs, SiteRunName, SiteRunsOn, SiteCallWith, SiteJobTimeout, SiteStepTimeout} {
		if len(SiteContexts(site)) == 0 {
			t.Errorf("%s has no contexts", site)
		}
	}
	if got := SiteContexts(SiteDefaultsRun); len(got) != 0 {
		t.Errorf("defaults.run admits contexts: %v", got)
	}
}

// TestRunsOnSubstitution pins PAR-053: matrix/inputs references are accepted and
// expanded; the group mapping and an unknown axis are named refusals.
func TestRunsOnSubstitution(t *testing.T) {
	ok := `name: R
on:
  workflow_dispatch:
    inputs:
      arch:
        default: amd64
jobs:
  build:
    runs-on: [docker, "${{ matrix.arch }}", "${{ inputs.arch }}"]
    strategy:
      matrix:
        arch: [amd64, arm64]
    steps:
      - run: echo hi
`
	w, err := Parse([]byte(ok))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	labels, err := ExpandRunsOn(w.Jobs["build"].RunsOn, map[string]string{"sha": "abc"}, map[string]string{"arch": "arm64"}, map[string]string{"arch": "amd64"})
	if err != nil {
		t.Fatalf("ExpandRunsOn: %v", err)
	}
	if strings.Join(labels, ",") != "docker,arm64,amd64" {
		t.Fatalf("labels = %v", labels)
	}
	for _, src := range []string{
		"name: R\non: push\njobs:\n  b:\n    runs-on: ${{ matrix.os }}\n    steps:\n      - run: x\n",
		"name: R\non: push\njobs:\n  b:\n    runs-on: {group: g, labels: [x]}\n    steps:\n      - run: x\n",
		"name: R\non: push\njobs:\n  b:\n    runs-on: [linux, \"${{ env.FOO }}\"]\n    steps:\n      - run: x\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Fatalf("accepted: %s", src)
		}
	}
	// An expression that resolves to nothing is a run-creation error, not an
	// empty label.
	if _, err := ExpandRunsOn([]string{"${{ inputs.arch }}"}, nil, nil, map[string]string{}); err == nil {
		t.Fatal("an empty label was accepted")
	}
}

// TestDefaultsAndRunName pins PAR-054/PAR-057's accepted shapes and the literal
// rule for defaults.run.
func TestDefaultsAndRunName(t *testing.T) {
	src := `name: F
run-name: Deploy ${{ inputs.env }} by ${{ github.actor }} on ${{ 1 == '1' && 'linux' || 'other' }}
on:
  workflow_dispatch:
    inputs:
      env:
        default: staging
defaults:
  run:
    shell: bash
    working-directory: sub
jobs:
  a:
    runs-on: linux
    defaults:
      run:
        shell: dash -e {0}
    steps:
      - run: echo hi
      - run: echo hi
        shell: sh
`
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.RunName == "" {
		t.Fatal("run-name lost")
	}
	title, err := RenderRunName(w.RunName, map[string]string{"actor": "ada"}, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("RenderRunName: %v", err)
	}
	if title != "Deploy prod by ada on linux" {
		t.Fatalf("title = %q", title)
	}
	job := jobValue(w, "a")
	shell, wd := DefaultsFor(w, &job)
	if shell != "dash -e {0}" || wd != "sub" {
		t.Fatalf("defaults = %q / %q", shell, wd)
	}
	for _, bad := range []string{
		"name: F\non: push\ndefaults:\n  run:\n    shell: ${{ matrix.os }}\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
		"name: F\non: push\njobs:\n  a:\n    runs-on: linux\n    defaults:\n      run:\n        working-directory: ${{ github.ref }}\n    steps:\n      - run: x\n",
		"name: F\nrun-name: ${{ secrets.TITLE }}\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
}

// jobValue returns a copy of one job (map values are not addressable).
func jobValue(w *Workflow, key string) Job { return w.Jobs[key] }

// TestTimeoutForms pins PAR-055's accepted forms and bounds.
func TestTimeoutForms(t *testing.T) {
	src := `name: T
on:
  workflow_dispatch:
    inputs:
      mins:
        default: "5"
jobs:
  a:
    runs-on: linux
    timeout-minutes: 20
    strategy:
      matrix:
        mins: [5, 10]
    steps:
      - run: echo hi
        timeout-minutes: 3
      - run: echo hi
        timeout-minutes: ${{ matrix.mins }}
  b:
    runs-on: linux
    timeout-minutes: ${{ inputs.mins }}
    steps:
      - run: echo hi
`
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.Jobs["a"].TimeoutMinutes.Value != 20 {
		t.Fatalf("literal timeout = %+v", w.Jobs["a"].TimeoutMinutes)
	}
	if w.Jobs["b"].TimeoutMinutes.Expr == "" {
		t.Fatal("expression timeout lost")
	}
	for _, bad := range []string{
		"name: T\non: push\njobs:\n  a:\n    runs-on: linux\n    timeout-minutes: 0\n    steps:\n      - run: x\n",
		"name: T\non: push\njobs:\n  a:\n    runs-on: linux\n    timeout-minutes: 361\n    steps:\n      - run: x\n",
		"name: T\non: push\njobs:\n  a:\n    runs-on: linux\n    timeout-minutes: ${{ secrets.X }}\n    steps:\n      - run: x\n",
		"name: T\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n        timeout-minutes: -1\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
	if n, err := EvalMinutes("5", nil, nil, nil); err != nil || n != 5 {
		t.Fatalf("EvalMinutes = %d, %v", n, err)
	}
	if _, err := EvalMinutes("'many'", nil, nil, nil); err == nil {
		t.Fatal("a non-numeric timeout was accepted")
	}
}

// TestFlowHelpersEdges covers the small branches the happy-path tests do not.
func TestFlowHelpersEdges(t *testing.T) {
	if err := validateDefaults("x", nil); err != nil {
		t.Fatal(err)
	}
	if err := validateDefaults("x", &Defaults{}); err != nil {
		t.Fatal(err)
	}
	if shell, wd := DefaultsFor(nil, nil); shell != "" || wd != "" {
		t.Fatalf("nil defaults = %q/%q", shell, wd)
	}
	wfOnly := &Workflow{Defaults: &Defaults{Run: &RunDefaults{Shell: "bash", WorkingDirectory: "wf"}}}
	if shell, wd := DefaultsFor(wfOnly, &Job{}); shell != "bash" || wd != "wf" {
		t.Fatalf("workflow defaults = %q/%q", shell, wd)
	}
	jobOnly := &Job{Defaults: &Defaults{Run: &RunDefaults{WorkingDirectory: "job"}}}
	if shell, wd := DefaultsFor(wfOnly, jobOnly); shell != "bash" || wd != "job" {
		t.Fatalf("per-key override = %q/%q", shell, wd)
	}
	if _, err := ExpandRunsOn([]string{"linux"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRunsOn([]string{"${{ bad }}"}, nil, nil, nil); err == nil {
		t.Fatal("malformed label resolved")
	}
	if err := validateRunsOn("j", Labels{"a", ""}, TemplateScope{}); err == nil {
		t.Fatal("an empty label was accepted")
	}
	// RenderRunName: empty, a missing value, an invalid span, the length cap and
	// whitespace collapsing.
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"${{ github.actor }}", ""},
	} {
		got, err := RenderRunName(tc.in, map[string]string{}, nil)
		if err != nil || got != tc.want {
			t.Errorf("RenderRunName(%q) = %q, %v", tc.in, got, err)
		}
	}
	if _, err := RenderRunName("${{ bad", nil, nil); err == nil {
		t.Fatal("an unterminated span was accepted")
	}
	if _, err := RenderRunName("${{ github.nope }}", nil, nil); err == nil {
		t.Fatal("an unsupported field was accepted")
	}
	long := strings.Repeat("x", RunNameMaxLen+20)
	if got, err := RenderRunName(long, nil, nil); err != nil || len(got) != RunNameMaxLen {
		t.Fatalf("long title = %d, %v", len(got), err)
	}
	if got, err := RenderRunName("  a\n b  ", nil, nil); err != nil || got != "a b" {
		t.Fatalf("collapsed = %q, %v", got, err)
	}
	// EvalMinutes bounds and expression errors.
	if _, err := EvalMinutes("0", nil, nil, nil); err == nil {
		t.Fatal("0 minutes accepted")
	}
	if _, err := EvalMinutes("361", nil, nil, nil); err == nil {
		t.Fatal("361 minutes accepted")
	}
	if _, err := EvalMinutes("${{ bad", nil, nil, nil); err == nil {
		t.Fatal("malformed timeout expression accepted")
	}
	// effectiveSite precedence.
	if (TemplateScope{CallOutput: true, Site: SiteJobIf}).effectiveSite() != SiteCallOutput {
		t.Fatal("call output site")
	}
	if (TemplateScope{CallWith: true}).effectiveSite() != SiteCallWith {
		t.Fatal("call with site")
	}
	if (TemplateScope{JobIf: true}).effectiveSite() != SiteJobIf {
		t.Fatal("job if site")
	}
}

// TestMaxParallelValidation pins PAR-052's bounds and the matrix requirement.
func TestMaxParallelValidation(t *testing.T) {
	src := `name: M
on: push
jobs:
  a:
    runs-on: linux
    strategy:
      max-parallel: 2
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - run: x
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, bad := range []string{
		"name: M\non: push\njobs:\n  a:\n    runs-on: linux\n    strategy:\n      max-parallel: 2\n    steps:\n      - run: x\n",
		"name: M\non: push\njobs:\n  a:\n    runs-on: linux\n    strategy:\n      max-parallel: 0\n      matrix:\n        go: [\"1.26\"]\n    steps:\n      - run: x\n",
		"name: M\non: push\njobs:\n  a:\n    runs-on: linux\n    strategy:\n      max-parallel: 257\n      matrix:\n        go: [\"1.26\"]\n    steps:\n      - run: x\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
}
