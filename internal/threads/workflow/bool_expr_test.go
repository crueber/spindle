package workflow

// bool_expr_test.go — the expression-valued boolean fields (PAR-056
// `continue-on-error`, PAR-151 `concurrency.cancel-in-progress`) and the
// substituted `container.image` validator (PAR-030).

import (
	"strings"
	"testing"
)

func TestBoolExprUnmarshal(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantTol  bool
		wantExpr string
		wantErr  string
	}{
		{name: "literal true", yaml: "continue-on-error: true", wantTol: true},
		{name: "literal false", yaml: "continue-on-error: false", wantTol: false},
		{name: "expression", yaml: "continue-on-error: ${{ matrix.experimental }}", wantExpr: "matrix.experimental"},
		{name: "empty span", yaml: "continue-on-error: ${{ }}", wantErr: "is empty"},
		{name: "not a bool", yaml: "continue-on-error: maybe", wantErr: "expected true, false or a ${{ … }} expression"},
		{name: "mapping", yaml: "continue-on-error: {a: b}", wantErr: "expected true, false or a ${{ … }} expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: B\non: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n        experimental: [true]\n    env:\n      X: y\n    " + tc.yaml + "\n    steps:\n      - run: echo hi\n"
			w, err := Parse([]byte(src))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			job := w.Jobs["a"]
			if job.ContinueOnError.Bool() != tc.wantTol || job.ContinueOnError.Expr != tc.wantExpr {
				t.Fatalf("BoolExpr = %+v, want value=%v expr=%q", job.ContinueOnError, tc.wantTol, tc.wantExpr)
			}
		})
	}
}

func TestBoolExprValidationUsesTheSiteTable(t *testing.T) {
	bad := []struct{ name, src, want string }{
		{
			name: "continue-on-error unknown context",
			src:  "name: B\non: push\njobs:\n  a:\n    runs-on: l\n    continue-on-error: ${{ secrets.FLAG }}\n    steps:\n      - run: echo hi\n",
			want: "not available in a job continue-on-error",
		},
		{
			name: "continue-on-error unknown axis",
			src:  "name: B\non: push\njobs:\n  a:\n    runs-on: l\n    continue-on-error: ${{ matrix.nope }}\n    strategy:\n      matrix:\n        experimental: [true]\n    steps:\n      - run: echo hi\n",
			want: "not a declared matrix key",
		},
		{
			name: "job cancel-in-progress cannot read secrets",
			src:  "name: B\non: push\njobs:\n  a:\n    runs-on: l\n    concurrency:\n      group: g\n      cancel-in-progress: ${{ secrets.FLAG }}\n    steps:\n      - run: echo hi\n",
			want: "not available in a job concurrency.cancel-in-progress",
		},
		{
			name: "workflow cancel-in-progress cannot read a matrix",
			src:  "name: B\non: push\nconcurrency:\n  group: g\n  cancel-in-progress: ${{ matrix.experimental }}\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: echo hi\n",
			want: "not available in a workflow concurrency.cancel-in-progress",
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.src)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEvalBoolAndEvalBoolExpr(t *testing.T) {
	scope := EvalScope{GitHub: map[string]string{"ref_name": "main"}, Matrix: map[string]string{"experimental": "true"}}
	cases := []struct {
		expr    string
		want    bool
		wantErr string
	}{
		{expr: "matrix.experimental", want: true},
		{expr: "github.ref_name != 'main'", want: false},
		{expr: "github.ref_name == 'main' && true", want: true},
		{expr: "", wantErr: "is empty"},
		{expr: "'maybe'", wantErr: "not a boolean"},
		{expr: "true false", wantErr: "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := EvalBool(tc.expr, scope)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("EvalBool(%q) = %v, %v; want %q", tc.expr, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("EvalBool(%q) = %v, %v; want %v", tc.expr, got, err, tc.want)
			}
			if got2, err2 := EvalBoolExpr(tc.expr, scope.GitHub, scope.Matrix, nil, nil); err2 != nil || got2 != tc.want {
				t.Fatalf("EvalBoolExpr(%q) = %v, %v", tc.expr, got2, err2)
			}
		})
	}
	// A malformed expression is the evaluator's named error.
	if _, err := EvalBool("github.", scope); err == nil {
		t.Fatal("a malformed expression was accepted")
	}
}

func TestExpandContainerImageErrors(t *testing.T) {
	cases := []struct {
		name  string
		image string
		gh    map[string]string
		want  string
	}{
		{name: "unterminated", image: "debian:${{ matrix.variant", want: "unterminated"},
		{name: "unknown input", image: "debian:${{ inputs.variant }}", want: "resolved to nothing"},
		{name: "unavailable field", image: "debian:${{ github.actor_id }}", want: "no numeric account id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpandContainerImage(tc.image, tc.gh, nil, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A literal image (no span) is unchanged and never consulted.
	if got, err := ExpandContainerImage("debian:bookworm", nil, nil, nil); err != nil || got != "debian:bookworm" {
		t.Fatalf("literal image = %q, %v", got, err)
	}
}

// TestSiteStringsCoverEveryPosition keeps the availability table's error text
// complete: every Site value has a human spelling (a new site cannot render as
// the zero value's "a step field").
func TestSiteStringsCoverEveryPosition(t *testing.T) {
	all := []Site{
		SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobGroup,
		SiteWorkflowGroup, SiteJobOutputs, SiteRunName, SiteRunsOn, SiteDefaultsRun,
		SiteContainerImage, SiteCallWith, SiteCallOutput, SiteJobTimeout, SiteStepTimeout,
		SiteJobContinueOnError, SiteJobConcurrencyCancel, SiteWorkflowConcurrencyCancel,
	}
	seen := map[string]bool{}
	for _, site := range all {
		s := site.String()
		if s == "" {
			t.Fatalf("%d has no spelling", site)
		}
		if site != SiteStepField && s == SiteStepField.String() {
			t.Fatalf("%d renders as the zero value", site)
		}
		if seen[s] {
			t.Fatalf("two sites render as %q", s)
		}
		seen[s] = true
	}
}
