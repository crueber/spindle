// parity_w5_test.go — PAR-039: the `job` and `strategy` contexts.
//
// The acceptance line is "Each resolves in a step; `job.status` reflects a
// tolerated failure correctly". These tests pin the contract:
//
//   - every documented field validates at the sites GitHub's availability row
//     allows and is a NAMED error everywhere else (the closed-set rule);
//   - each field resolves through the shared evaluator and the substitution
//     path, so the parser and the runner cannot disagree;
//   - the run-creation helpers (job name, concurrency group, runs-on, timeout)
//     carry the same strategy values per matrix cell.
package workflow

import (
	"strings"
	"testing"
)

const jobStrategyWorkflow = `name: Contexts
on:
  push:
jobs:
  build:
    name: build ${{ strategy.job-index }}/${{ strategy.job-total }}
    runs-on: linux
    strategy:
      fail-fast: false
      max-parallel: 1
      matrix:
        go: ["1.26", "1.27"]
    timeout-minutes: 10
    outputs:
      index: ${{ strategy.job-index }}
    steps:
      - name: cell
        run: echo "${{ strategy.job-index }} ${{ strategy.job-total }} ${{ strategy.fail-fast }} ${{ strategy.max-parallel }}"
      - name: tolerated
        run: exit 1
        continue-on-error: true
      - name: after
        if: job.status == 'success'
        run: echo "${{ job.status }}"
`

// TestJobAndStrategyContextsParse: the documented fields validate in every
// site the availability table allows.
func TestJobAndStrategyContextsParse(t *testing.T) {
	if _, err := Parse([]byte(jobStrategyWorkflow)); err != nil {
		t.Fatalf("job/strategy workflow did not parse: %v", err)
	}
}

// TestJobAndStrategyContextAvailability: the table is the contract — a site
// outside the documented row is a named error, never an empty value.
func TestJobAndStrategyContextAvailability(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"strategy in a job if", `name: C
on: [push]
jobs:
  build:
    if: strategy.job-index == '0'
    runs-on: linux
    steps:
      - run: echo hi
`, "not available in a job if:"},
		{"job in a job if", `name: C
on: [push]
jobs:
  build:
    if: job.status == 'success'
    runs-on: linux
    steps:
      - run: echo hi
`, "not available in a job if:"},
		{"job in a job name", `name: C
on: [push]
jobs:
  build:
    name: build ${{ job.status }}
    runs-on: linux
    steps:
      - run: echo hi
`, "not available in a job name"},
		{"strategy in run-name", `name: C
run-name: run ${{ strategy.job-index }}
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`, "not available in run-name"},
		{"unknown strategy field", `name: C
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo ${{ strategy.job-number }}
`, "not a supported strategy field (supported: fail-fast, job-index, job-total, max-parallel)"},
		{"unknown job field", `name: C
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - if: job.check_run_id == '1'
        run: echo hi
`, "not a supported job field (supported: status)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s parsed; want a named error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestJobAndStrategyEvaluate: every field resolves through the evaluator with
// the values the run path supplies, and values are fine as whole expressions.
func TestJobAndStrategyEvaluate(t *testing.T) {
	scope := EvalScope{
		Job:      map[string]string{"status": "failure"},
		Strategy: map[string]string{"job-index": "1", "job-total": "3", "fail-fast": "false", "max-parallel": "2"},
	}
	cases := map[string]string{
		"${{ job.status }}":            "failure",
		"${{ strategy.job-index }}":    "1",
		"${{ strategy.job-total }}":    "3",
		"${{ strategy.fail-fast }}":    "false",
		"${{ strategy.max-parallel }}": "2",
	}
	for expr, want := range cases {
		got, err := EvalString(strings.TrimSuffix(strings.TrimPrefix(expr, "${{ "), " }}"), scope)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", expr, got, want)
		}
	}
	// A whole expression can combine them (PAR-038's value rule).
	got, err := EvalString("strategy.job-index == '0' && 'first' || 'rest'", scope)
	if err != nil || got != "rest" {
		t.Fatalf("combined = %q, %v", got, err)
	}
	// The substitution path renders the same values (one pass, data only).
	sub, err := Substitute("cell ${{ strategy.job-index }}/${{ strategy.job-total }} is ${{ job.status }}",
		TemplateValues{Job: scope.Job, Strategy: scope.Strategy})
	if err != nil || sub != "cell 1/3 is failure" {
		t.Fatalf("Substitute = %q, %v", sub, err)
	}
	// A plain job has an empty strategy context: a supported reference reads
	// empty, never the literal text (the documented absent-value rule).
	plain, err := EvalString("strategy.job-index", EvalScope{})
	if err != nil || plain != "" {
		t.Fatalf("plain strategy = %q, %v", plain, err)
	}
}

// TestStrategyExpansionHelpers: the run-creation helpers carry the cell's
// strategy values (job name, concurrency group, runs-on, timeout).
func TestStrategyExpansionHelpers(t *testing.T) {
	strategy := map[string]string{"job-index": "1", "job-total": "2", "fail-fast": "true", "max-parallel": "2"}
	if got := FillTemplateWithStrategy("build ${{ strategy.job-index }}", nil, nil, nil, strategy); got != "build 1" {
		t.Fatalf("FillTemplateWithStrategy = %q", got)
	}
	group, err := ExpandGroupWithStrategy("ci-${{ strategy.job-index }}", nil, nil, nil, strategy)
	if err != nil || group != "ci-1" {
		t.Fatalf("ExpandGroupWithStrategy = %q, %v", group, err)
	}
	// An absent value still fails a group closed (it is a lock key).
	if _, err := ExpandGroupWithStrategy("ci-${{ strategy.job-index }}", nil, nil, nil, nil); err == nil {
		t.Fatal("a group with no strategy value was accepted")
	}
	labels, err := ExpandRunsOnWithStrategy([]string{"linux", "cpu-${{ strategy.max-parallel }}"}, nil, nil, nil, strategy)
	if err != nil || len(labels) != 2 || labels[1] != "cpu-2" {
		t.Fatalf("ExpandRunsOnWithStrategy = %v, %v", labels, err)
	}
	mins, err := EvalMinutesWithStrategy("strategy.job-index == '0' && '5' || '7'", nil, nil, nil, strategy)
	if err != nil || mins != 7 {
		t.Fatalf("EvalMinutesWithStrategy = %d, %v", mins, err)
	}
	// The un-suffixed forms keep the old behaviour for callers with no cell.
	if got := FillTemplate("x-${{ strategy.job-index }}", nil, nil, nil); got != "x-${{ strategy.job-index }}" {
		t.Fatalf("FillTemplate without strategy = %q", got)
	}
}

// TestStrategyGroupTemplateValidation: a job-level group may name strategy.*, a
// workflow-level one may not (it expands before any cell exists), and an
// unknown field is a named error.
func TestStrategyGroupTemplateValidation(t *testing.T) {
	if err := ValidateGroupTemplate("ci-${{ strategy.job-index }}", []string{"go"}, nil, false); err != nil {
		t.Fatalf("job-level strategy group refused: %v", err)
	}
	if err := ValidateGroupTemplate("ci-${{ strategy.job-index }}", nil, nil, true); err == nil ||
		!strings.Contains(err.Error(), "not available in a workflow-level group") {
		t.Fatalf("workflow-level strategy group = %v", err)
	}
	if err := ValidateGroupTemplate("ci-${{ strategy.nope }}", nil, nil, false); err == nil ||
		!strings.Contains(err.Error(), "not a supported strategy field") {
		t.Fatalf("unknown strategy field = %v", err)
	}
}

// TestJobAndStrategyValidationErrorBranches exercises the evaluator's and the
// validator's refusal paths directly, so a stored file that bypassed the
// parser still cannot read a field the engine has no value for.
func TestJobAndStrategyValidationErrorBranches(t *testing.T) {
	if _, err := EvalString("job.nope", EvalScope{Job: map[string]string{"status": "success"}}); err == nil ||
		!strings.Contains(err.Error(), "supported: status") {
		t.Fatalf("eval job.nope = %v", err)
	}
	if _, err := EvalString("strategy.nope", EvalScope{Strategy: map[string]string{"job-index": "0"}}); err == nil ||
		!strings.Contains(err.Error(), "supported: fail-fast") {
		t.Fatalf("eval strategy.nope = %v", err)
	}
	if err := ValidateIf("job.nope == '1'", TemplateScope{Site: SiteStepIf}); err == nil ||
		!strings.Contains(err.Error(), "not a supported job field") {
		t.Fatalf("ValidateIf job.nope = %v", err)
	}
	if err := ValidateIf("strategy.nope == '1'", TemplateScope{Site: SiteStepIf}); err == nil ||
		!strings.Contains(err.Error(), "not a supported strategy field") {
		t.Fatalf("ValidateIf strategy.nope = %v", err)
	}
	// A site the table excludes is refused by ValidateTemplate too.
	if err := ValidateTemplate("${{ job.status }}", TemplateScope{Site: SiteJobName}); err == nil ||
		!strings.Contains(err.Error(), "not available in a job name") {
		t.Fatalf("ValidateTemplate = %v", err)
	}
}
