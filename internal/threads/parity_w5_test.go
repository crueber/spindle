// parity_w5_test.go — PAR-039 at the run-creation seam: one matrix cell's
// `strategy.*` context is written once and read by the server (job name, lock
// key, runs-on) and the runner (step substitution) from the same fields.
package threads

import (
	"strings"
	"testing"
)

const strategyContextWorkflow = `name: Strategy
on: [push]
jobs:
  build:
    name: build ${{ strategy.job-index }}/${{ strategy.job-total }}
    runs-on: "${{ strategy.fail-fast == 'true' && 'linux' || 'linux' }}"
    strategy:
      fail-fast: false
      max-parallel: 1
      matrix:
        go: ["1.26", "1.27"]
    concurrency:
      group: "cell-${{ strategy.job-index }}"
    steps:
      - run: echo hi
`

// TestStrategyContextPerCell: the expansion writes job-index/job-total and the
// strategy values the server resolves at creation time.
func TestStrategyContextPerCell(t *testing.T) {
	e := newTestEnv(t)
	run := e.createWith(t, ".spindle/workflows/strategy.yml", strategyContextWorkflow)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 2 {
		t.Fatalf("jobs = %d", len(view.Jobs))
	}
	for i, job := range view.Jobs {
		wantName := []string{"build 0/2 (1.26)", "build 1/2 (1.27)"}[i]
		if job.Name != wantName {
			t.Fatalf("job %d name = %q, want %q", i, job.Name, wantName)
		}
		if job.Spec.MatrixIndex != i || job.Spec.MatrixTotal != 2 {
			t.Fatalf("cell %d index/total = %d/%d", i, job.Spec.MatrixIndex, job.Spec.MatrixTotal)
		}
		want := map[string]string{
			"job-index": []string{"0", "1"}[i], "job-total": "2",
			"fail-fast": "false", "max-parallel": "1",
		}
		got := StrategyContextOf(job.Spec)
		for key, val := range want {
			if got[key] != val {
				t.Fatalf("cell %d strategy.%s = %q, want %q", i, key, got[key], val)
			}
		}
		if job.Spec.Concurrency == nil || job.Spec.Concurrency.Group != "cell-"+want["job-index"] {
			t.Fatalf("cell %d group = %+v", i, job.Spec.Concurrency)
		}
		if len(job.Spec.RunsOn) != 1 || job.Spec.RunsOn[0] != "linux" {
			t.Fatalf("cell %d runs-on = %v", i, job.Spec.RunsOn)
		}
	}
}

// TestStrategyContextDefaults: fail-fast defaults true, max-parallel defaults to
// the product size, and a plain job has no strategy context at all.
func TestStrategyContextDefaults(t *testing.T) {
	strat := &StrategySpec{Matrix: map[string][]string{"go": {"1", "2", "3"}}}
	got := strategyContextFor(strat, 2, 3)
	if got["fail-fast"] != "true" || got["max-parallel"] != "3" || got["job-index"] != "2" || got["job-total"] != "3" {
		t.Fatalf("defaults = %v", got)
	}
	off := false
	if v := strategyContextFor(&StrategySpec{Matrix: map[string][]string{"go": {"1"}}, FailFast: &off}, 0, 1); v["fail-fast"] != "false" {
		t.Fatalf("fail-fast false = %v", v)
	}
	if v := strategyContextFor(nil, 0, 1); v != nil {
		t.Fatalf("nil strategy = %v", v)
	}
	// A stored plain job (no product) never gets a context, even if a stray
	// index rode the record.
	if v := StrategyContextOf(&Spec{MatrixIndex: 0, MatrixTotal: 0}); v != nil {
		t.Fatalf("plain job = %v", v)
	}
	if v := StrategyContextOf(nil); v != nil {
		t.Fatalf("nil spec = %v", v)
	}
}

// TestStrategyContextPlainJobRendersEmpty: a supported strategy reference in a
// plain job's step is empty (never the literal text), and the engine accepts
// the file.
func TestStrategyContextPlainJobRendersEmpty(t *testing.T) {
	e := newTestEnv(t)
	yaml := `name: Plain
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - name: "cell ${{ strategy.job-index }}"
        run: echo "${{ strategy.job-total }}"
`
	run := e.createWith(t, ".spindle/workflows/plain.yml", yaml)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(view.Jobs))
	}
	if view.Jobs[0].Spec.MatrixTotal != 0 {
		t.Fatalf("plain job carried a matrix total: %+v", view.Jobs[0].Spec)
	}
	if got := StrategyContextOf(view.Jobs[0].Spec); got != nil {
		t.Fatalf("plain job strategy = %v", got)
	}
	if strings.Contains(view.Jobs[0].Name, "strategy") {
		t.Fatalf("unresolved strategy text leaked into the job name: %q", view.Jobs[0].Name)
	}
}
