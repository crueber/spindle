package threads

// Wave 4 runtime halves: the expression operators and filters (PAR-038), the
// context availability table at evaluation time (PAR-040), job-if secrets/vars
// (PAR-058), run-name (PAR-057), runs-on substitution (PAR-053), defaults
// (PAR-054), timeout parity (PAR-055) and matrix max-parallel admission
// (PAR-052). hashFiles lives in internal/threads/runner/hashfiles_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestEvalIfOperatorsAndFilters(t *testing.T) {
	ctx := ExprContext{
		Success: true,
		GitHub:  map[string]string{"sha": "abc123", "ref": "refs/heads/main"},
		Matrix:  map[string]string{"go": "1.27"},
		Needs:   map[string]string{"build": "failure", "docs": "success"},
		NeedsOutputs: map[string]map[string]string{
			"build": {"digest": "deadbeef"},
		},
		Steps:           map[string]string{"test": "failure"},
		StepConclusions: map[string]string{"test": "success"},
		Outputs:         map[string]map[string]string{"test": {"sum": "42"}},
	}
	cases := []struct {
		expr string
		want bool
	}{
		// Loose cross-type equality and ordering.
		{"1 == '1'", true},
		{"true == 'true'", true},
		{"'a' < 'b'", true},
		{"'2' < '10'", true},
		{"'10' > '2'", true},
		{"2 <= 2 && 3 >= 2", true},
		{"github['sha'] == 'abc123'", true},
		{"github['event_name'] == ''", true},
		{"steps['test'].outputs['sum'] == '42'", true},
		// Value-returning logical operators and parentheses.
		{"(github.ref == 'refs/heads/dev' || github.ref == 'refs/heads/main') && success()", true},
		{"'true' || 'false'", true},
		// Filters.
		{"contains(needs.*.result, 'failure')", true},
		{"contains(steps.*.conclusion, 'success')", true},
		{"contains(steps.*.outputs.sum, '42')", true},
		{"join(needs.*.result, ',') == 'failure,success'", true},
		{"needs.*.outputs.digest == 'deadbeef'", false}, // a list is not a scalar
		{"!contains(needs.*.result, 'cancelled')", true},
	}
	for _, tc := range cases {
		got, err := EvalIf(tc.expr, ctx)
		if err != nil || got != tc.want {
			t.Errorf("EvalIf(%q) = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
	// `&&`/`||` return the operand: the value form is what a substitution site
	// receives, not "true".
	if got, err := EvalString("github.ref == 'refs/heads/main' && 'prod' || 'dev'", ctx); err != nil || got != "prod" {
		t.Fatalf("value expression = %q, %v", got, err)
	}
	if got, err := EvalString("format('{0}-{1}', matrix.go, github['sha'])", ctx); err != nil || got != "1.27-abc123" {
		t.Fatalf("format = %q, %v", got, err)
	}
}

// TestSubstituteExpressions pins that a `${{ … }}` span in a step field can be an
// expression (PAR-038) and that `github['key']` is the documented index form.
func TestSubstituteExpressions(t *testing.T) {
	vals := workflowTemplateValues()
	cases := []struct{ in, want string }{
		{"${{ github.ref == 'refs/heads/main' && 'prod' || 'dev' }}", "prod"},
		{"${{ github['sha'] }}", "abc123"},
		{"${{ matrix.go }}-${{ inputs.mode }}", "1.27-fast"},
		{"${{ steps.build.outputs.artifact }}", "dist.tar"},
	}
	for _, tc := range cases {
		got, err := workflow.Substitute(tc.in, vals)
		if err != nil || got != tc.want {
			t.Errorf("Substitute(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := workflow.Substitute("${{ steps.ghost.outputs.x }}", vals); err == nil {
		t.Fatal("an undeclared step output expression was accepted")
	}
}

// TestJobIfSecretsAndVars pins PAR-058: a trusted run reads the decrypted
// values, an untrusted run reads an empty secrets map (the condition is false,
// not an error), and vars resolve on both.
func TestJobIfSecretsAndVars(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetSecret(ctx(), "o/r", "DEPLOY_KEY", "very-secret-value", admin); err != nil {
		t.Fatalf("secret: %v", err)
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "REGION", "eu", admin); err != nil {
		t.Fatalf("variable: %v", err)
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: Gate
on: push
jobs:
  deploy:
    runs-on: linux
    if: secrets.DEPLOY_KEY != '' && vars.REGION == 'eu'
    steps:
      - run: echo deploy
  fallback:
    runs-on: linux
    if: secrets.DEPLOY_KEY == ''
    steps:
      - run: echo fallback
`)
	run := e.mustCreate(t)[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	byKey := map[string]*Job{}
	for _, job := range jobs {
		byKey[job.Key] = job
	}
	if byKey["deploy"].State != JobQueued || byKey["fallback"].State != JobSkipped {
		t.Fatalf("trusted states: deploy=%s fallback=%s (%s)", byKey["deploy"].State, byKey["fallback"].State, byKey["fallback"].Reason)
	}
	// The decision never writes a secret value into the record.
	if strings.Contains(byKey["fallback"].Reason, "very-secret-value") {
		t.Fatalf("reason leaks a secret: %q", byKey["fallback"].Reason)
	}

	// Direct evaluation of an untrusted run: an empty secrets map, never an
	// error and never the literal text.
	job := &Job{Spec: &Spec{If: "secrets.DEPLOY_KEY != ''", IfSecrets: true}}
	verdict, _, err := evaluateJobIfWith(job, &Run{Trusted: false}, needFacts{}, jobIfValues{Secrets: map[string]string{}})
	if err != nil || verdict != jobIfSkip {
		t.Fatalf("untrusted condition = %v, %v; want skipped", verdict, err)
	}
	// A var-only condition works untrusted too (vars are not secrets).
	job = &Job{Spec: &Spec{If: "vars.REGION == 'eu'", IfVars: true}}
	verdict, _, err = evaluateJobIfWith(job, &Run{Trusted: false}, needFacts{}, jobIfValues{Vars: map[string]string{"REGION": "eu"}})
	if err != nil || verdict != jobIfRun {
		t.Fatalf("untrusted vars = %v, %v", verdict, err)
	}
	// An untrusted run's loader reads nothing at all.
	vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfSecrets: true}}, &Run{Trusted: false})
	if err != nil || len(vals.Secrets) != 0 {
		t.Fatalf("untrusted loader = %v, %v", vals, err)
	}
}

func TestRunNameRenderedOnTheRecord(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: Deploy
run-name: Deploy ${{ github.event_name }} on ${{ github.ref_name }} by ${{ github.actor }}
on: push
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`)
	run := e.mustCreate(t)[0]
	if run.Title != "Deploy push on main by writer" {
		t.Fatalf("title = %q", run.Title)
	}
}

func TestRunsOnAndTimeoutsExpandPerCell(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: M
on: push
jobs:
  build:
    runs-on: [docker, "${{ matrix.arch }}"]
    timeout-minutes: ${{ matrix.mins }}
    strategy:
      max-parallel: 1
      matrix:
        arch: [amd64, arm64]
        mins: [5, 10]
    steps:
      - run: echo hi
        timeout-minutes: ${{ matrix.mins }}
  plain:
    runs-on: linux
    steps:
      - run: echo hi
`)
	run := e.mustCreate(t)[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		if job.Key == "plain" {
			if job.Spec.TimeoutMinutes != 360 {
				t.Fatalf("default timeout = %d, want 360", job.Spec.TimeoutMinutes)
			}
			continue
		}
		if len(job.Spec.RunsOn) != 2 || job.Spec.RunsOn[0] != "docker" {
			t.Fatalf("%s runs-on = %v", job.Key, job.Spec.RunsOn)
		}
		if job.Spec.RunsOn[1] != "amd64" && job.Spec.RunsOn[1] != "arm64" {
			t.Fatalf("%s arch label = %q", job.Key, job.Spec.RunsOn[1])
		}
		if job.Spec.TimeoutMinutes != 5 && job.Spec.TimeoutMinutes != 10 {
			t.Fatalf("%s timeout = %d", job.Key, job.Spec.TimeoutMinutes)
		}
		if job.Spec.Steps[0].TimeoutMinutes != job.Spec.TimeoutMinutes {
			t.Fatalf("%s step timeout = %d", job.Key, job.Spec.Steps[0].TimeoutMinutes)
		}
		seen[job.Spec.RunsOn[1]] = true
	}
	if !seen["amd64"] || !seen["arm64"] {
		t.Fatalf("cells = %v", seen)
	}
	for _, job := range jobs {
		if job.Key != "plain" {
			if got := maxParallelOf(job); got != 1 {
				t.Fatalf("max-parallel = %d", got)
			}
		}
	}
}

func TestDefaultsApplyToStepsAndStepWins(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: D
on: push
defaults:
  run:
    shell: bash
    working-directory: wf
jobs:
  a:
    runs-on: linux
    defaults:
      run:
        shell: dash -e {0}
    steps:
      - run: one
      - run: two
        shell: sh
        working-directory: own
`)
	run := e.mustCreate(t)[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	steps := jobs[0].Spec.Steps
	if steps[0].Shell != "dash -e {0}" || steps[0].WorkingDirectory != "wf" {
		t.Fatalf("defaults = %q / %q", steps[0].Shell, steps[0].WorkingDirectory)
	}
	if steps[1].Shell != "sh" || steps[1].WorkingDirectory != "own" {
		t.Fatalf("step override = %q / %q", steps[1].Shell, steps[1].WorkingDirectory)
	}
}

// TestMatrixMaxParallelAdmission pins PAR-052: six cells with max-parallel 2
// never have three admitted, and a finished cell lets the next one in.
func TestMatrixMaxParallelAdmission(t *testing.T) {
	e, runID := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 2
      matrix:
        v: [1, 2, 3, 4, 5, 6]
    steps:
      - run: echo ${{ matrix.v }}
`)
	r := needsRunner()
	var claimed []*Claim
	for i := 0; i < 3; i++ {
		claim, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if claim == nil {
			break
		}
		if claim.Entry.RunID != runID {
			t.Fatalf("claimed another run: %+v", claim.Entry)
		}
		claimed = append(claimed, claim)
	}
	if len(claimed) != 2 {
		t.Fatalf("admitted %d cells with max-parallel 2", len(claimed))
	}
	for _, c := range claimed {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", runID, c.Job.ID, JobUpdate{State: JobRunning}); err != nil {
			t.Fatalf("mark running: %v", err)
		}
	}
	// The queued siblings name the admission bound instead of a silent spinner.
	view, err := e.svc.GetRun(ctx(), "o/r", runID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	reasonFound := false
	for _, job := range view.Jobs {
		if job.State == JobQueued && job.Spec != nil && job.Spec.MatrixGroup == "build" {
			if !strings.Contains(job.QueueReason, "max-parallel 2") {
				t.Fatalf("queued cell reason = %q", job.QueueReason)
			}
			reasonFound = true
		}
	}
	if !reasonFound {
		t.Fatal("no queued bounded cell in the run view")
	}
	// A third probe finds nothing: every remaining cell is refused by the bound.
	if claim, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels); err != nil || claim != nil {
		t.Fatalf("third claim = %+v, %v", claim, err)
	}
	// Finishing one cell frees exactly one slot.
	e.svc.finishEntry(ctx(), "o", "r", claimed[0].Entry.Seq)
	next, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels)
	if err != nil || next == nil {
		t.Fatalf("after release: %+v, %v", next, err)
	}
	if next.Job.ID == claimed[1].Job.ID || next.Job.ID == claimed[0].Job.ID {
		t.Fatalf("claimed a running cell again: %s", next.Job.ID)
	}
	// And the bound still holds: two running (claimed[1] and next), no third.
	if claim, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels); err != nil || claim != nil {
		t.Fatalf("over-admitted after release: %+v, %v", claim, err)
	}
}

// TestContextAvailabilityAtRuntime pins the evaluator half of PAR-040: an
// unavailable context is a named evaluation error, not an empty value.
func TestContextAvailabilityAtRuntime(t *testing.T) {
	_, _, err := evaluateJobIfWith(&Job{Spec: &Spec{If: "runner.os == 'Linux'"}}, &Run{}, needFacts{}, jobIfValues{})
	if err == nil || !strings.Contains(err.Error(), "runner.os") {
		t.Fatalf("job runner context = %v", err)
	}
}

// workflowTemplateValues is one substitution site's value set.
func workflowTemplateValues() workflow.TemplateValues {
	return workflow.TemplateValues{
		GitHub:  map[string]string{"sha": "abc123", "ref": "refs/heads/main", "event_name": "push"},
		Matrix:  map[string]string{"go": "1.27"},
		Inputs:  map[string]string{"mode": "fast"},
		Steps:   map[string]map[string]string{"build": {"artifact": "dist.tar"}},
		Secrets: map[string]string{"TOKEN": "s3cret"},
	}
}

func TestLoadJobIfValuesEdges(t *testing.T) {
	e := newTestEnv(t)
	if vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", nil, nil); err != nil || vals.Secrets != nil {
		t.Fatalf("nil job = %+v, %v", vals, err)
	}
	if vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{}, nil); err != nil || vals.Vars != nil {
		t.Fatalf("nil spec = %+v, %v", vals, err)
	}
	// With no keyring, a var reference resolves to an empty map, not an error.
	if vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfVars: true}}, &Run{Trusted: true}); err != nil || len(vals.Vars) != 0 {
		t.Fatalf("disabled vars = %+v, %v", vals, err)
	}
}

func TestConclusionStateIncludesTimedOut(t *testing.T) {
	for in, want := range map[string]string{
		"success": JobSuccess, "failure": JobFailure, "failed": JobFailure,
		"error": JobFailure, "timed_out": JobTimedOut, "timed-out": JobTimedOut,
		"timeout": JobTimedOut, "cancelled": JobCancelled, "canceled": JobCancelled,
	} {
		got, err := conclusionState(in)
		if err != nil || got != want {
			t.Errorf("conclusionState(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := conclusionState("nope"); err == nil {
		t.Fatal("an unknown conclusion was accepted")
	}
}

func TestMatrixAdmissionPruneReleaseAndEdges(t *testing.T) {
	e, _ := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 2
      matrix:
        v: [1, 2, 3]
    steps:
      - run: echo ${{ matrix.v }}
`)
	r := needsRunner()
	first, err := e.svc.claimLoop(ctx(), "o", "r", r, r.Labels)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	// Re-admitting the same cell is refused: the winner already holds the slot,
	// and a losing probe must never release it.
	if ok, err := e.svc.admitMatrixCell(ctx(), "o", "r", first.Entry); err != nil || ok {
		t.Fatalf("re-admit = %v, %v", ok, err)
	}
	// A plain job has no group and is always admitted.
	if ok, err := e.svc.admitMatrixCell(ctx(), "o", "r", &QueueEntry{MaxParallel: 0}); err != nil || !ok {
		t.Fatalf("unbounded = %v, %v", ok, err)
	}
	// Release is idempotent, and a missing record/group/slot is a no-op.
	e.svc.releaseMatrixCell(ctx(), "o", "r", first.Entry.RunID, first.Entry.MatrixGroup, first.Job.ID)
	e.svc.releaseMatrixCell(ctx(), "o", "r", first.Entry.RunID, first.Entry.MatrixGroup, first.Job.ID)
	e.svc.releaseMatrixCell(ctx(), "o", "r", "nope", "nope", "nope")
	e.svc.releaseMatrixCell(ctx(), "o", "r", "", "", "")

	// The queue entry loader tolerates an absent key and a corrupt record.
	if got := e.svc.loadQueueEntry(ctx(), "o", "r", ""); got != nil {
		t.Fatalf("empty seq = %+v", got)
	}
	if got := e.svc.loadQueueEntry(ctx(), "o", "r", "missing"); got != nil {
		t.Fatalf("missing entry = %+v", got)
	}
	if _, perr := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", "bad"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatal(perr)
	}
	if got := e.svc.loadQueueEntry(ctx(), "o", "r", "bad"); got != nil {
		t.Fatalf("corrupt entry = %+v", got)
	}

	// A lapsed admission deadline whose claim is gone frees the slot: advance
	// past both the admission window and the claim lease, then admit a queued
	// sibling directly.
	e.advance(2 * time.Hour)
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	for _, seq := range head.Pending {
		entry, _, lerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if lerr != nil || entry == nil || entry.State != QueueQueued || entry.MaxParallel == 0 {
			continue
		}
		if ok, aerr := e.svc.admitMatrixCell(ctx(), "o", "r", entry); aerr != nil || !ok {
			t.Fatalf("admit after lapse = %v, %v", ok, aerr)
		}
		return
	}
	t.Fatal("no queued matrix cell to admit")
}

func TestPruneMatrixSlotsBranches(t *testing.T) {
	e := newTestEnv(t)
	now := e.now
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	// One live queued entry, one claimed with a live lease, one with a bad lease.
	put := func(seq string, entry *QueueEntry) {
		body, _ := json.Marshal(entry)
		if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", seq), body, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatal(err)
		}
	}
	liveLease := future.Format(dateTimeFmt)
	put("live", &QueueEntry{Seq: "live", RunID: "r1", JobID: "j1", State: QueueClaimed, LeaseUntil: &liveLease})
	put("queued", &QueueEntry{Seq: "queued", RunID: "r1", JobID: "j2", State: QueueQueued})
	badLease := "not-a-time"
	put("bad", &QueueEntry{Seq: "bad", RunID: "r1", JobID: "j3", State: QueueClaimed, LeaseUntil: &badLease})
	g := &matrixGroupAdmission{Max: 3, Running: map[string]matrixSlot{
		"future":  {Seq: "live", Until: future.Format(dateTimeFmt)},
		"refresh": {Seq: "live", Until: past.Format(dateTimeFmt)},
		"gone":    {Seq: "queued", Until: past.Format(dateTimeFmt)},
		"bad":     {Seq: "bad", Until: past.Format(dateTimeFmt)},
		"missing": {Seq: "nope", Until: past.Format(dateTimeFmt)},
	}}
	e.svc.pruneMatrixSlots(ctx(), "o", "r", g, now)
	if _, ok := g.Running["future"]; !ok {
		t.Fatal("a future slot was pruned")
	}
	if slot, ok := g.Running["refresh"]; !ok || slot.Until != liveLease {
		t.Fatalf("a live repeated claim was not refreshed: %+v", slot)
	}
	for _, gone := range []string{"gone", "bad", "missing"} {
		if _, ok := g.Running[gone]; ok {
			t.Errorf("slot %q was not pruned", gone)
		}
	}
}

func TestEvalStepTimeoutsRecursesAndFails(t *testing.T) {
	steps := []StepSpec{
		{TimeoutExpr: "matrix.m"},
		{Parallel: []StepSpec{{TimeoutExpr: "matrix.m"}}},
	}
	out, err := evalStepTimeouts(steps, nil, map[string]string{"m": "7"}, nil)
	if err != nil || out[0].TimeoutMinutes != 7 || out[0].TimeoutExpr != "" || out[1].Parallel[0].TimeoutMinutes != 7 {
		t.Fatalf("eval = %+v, %v", out, err)
	}
	if _, err := evalStepTimeouts([]StepSpec{{TimeoutExpr: "matrix.nope"}}, nil, nil, nil); err == nil {
		t.Fatal("an unresolvable step timeout was accepted")
	}
	if stepExprs([]StepSpec{{TimeoutMinutes: 5}}) {
		t.Fatal("a literal timeout reported an expression")
	}
}

func TestLoadJobIfValuesTrustedLeaves(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetVariable(ctx(), "o/r", "REGION", "eu", admin); err != nil {
		t.Fatal(err)
	}
	vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfVars: true}}, &Run{Trusted: true})
	if err != nil || vals.Vars["REGION"] != "eu" {
		t.Fatalf("vars = %+v, %v", vals, err)
	}
	// Secrets disabled: a trusted run reads an empty map, not an error.
	plain := newTestEnv(t)
	vals, err = plain.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfSecrets: true}}, &Run{Trusted: true})
	if err != nil || len(vals.Secrets) != 0 {
		t.Fatalf("disabled secrets = %+v, %v", vals, err)
	}
}

func TestEvalStepTimeoutsParallelError(t *testing.T) {
	steps := []StepSpec{{Parallel: []StepSpec{{TimeoutExpr: "matrix.nope"}}}}
	if _, err := evalStepTimeouts(steps, nil, nil, nil); err == nil {
		t.Fatal("a parallel member's bad timeout was accepted")
	}
	if stepExprs([]StepSpec{{Parallel: []StepSpec{{TimeoutExpr: "matrix.m"}}}}) != true {
		t.Fatal("a nested expression was missed")
	}
}

func TestLoadJobIfValuesCorruptSecretFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	if _, err := store.PutBytes(ctx(), e.store, SecretsPrefix("o", "r")+"BROKEN.json", []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfSecrets: true}}, &Run{Trusted: true}); err == nil {
		t.Fatal("a corrupt secret record did not fail the job-if read")
	}
	if _, err := store.PutBytes(ctx(), e.store, VarsKey("o", "r"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfVars: true}}, &Run{Trusted: true}); err == nil {
		t.Fatal("a corrupt variable record did not fail the job-if read")
	}
}

func TestLookupVarCase(t *testing.T) {
	if got := lookupVar(map[string]string{"REGION": "eu"}, "REGION"); got != "eu" {
		t.Fatalf("direct = %q", got)
	}
	if got := lookupVar(map[string]string{"REGION": "eu"}, "region"); got != "eu" {
		t.Fatalf("folded = %q", got)
	}
	if got := lookupVar(nil, "x"); got != "" {
		t.Fatalf("nil = %q", got)
	}
}

func TestMatrixAdmissionCorruptRecordFailsClosed(t *testing.T) {
	e, runID := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 1
      matrix:
        v: [1, 2]
    steps:
      - run: echo ${{ matrix.v }}
`)
	key := MatrixAdmissionKey("o", "r", runID)
	if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	entry := &QueueEntry{RunID: runID, JobID: "j-build-1", MatrixGroup: "build", MaxParallel: 1, Seq: "s"}
	if _, err := e.svc.admitMatrixCell(ctx(), "o", "r", entry); err == nil {
		t.Fatal("a corrupt admission record was ignored")
	}
	// Release tolerates the same record (best effort on the terminal path).
	e.svc.releaseMatrixCell(ctx(), "o", "r", runID, "build", "j-build-1")
}

func TestCreateRunRefusesAnExpansionItCannotResolve(t *testing.T) {
	// A runs-on reference that resolves to nothing fails job expansion (the
	// parser has already checked the reference exists; the cell decides its
	// value).
	if _, err := expandJobSpec("a", "a", &Spec{RunsOn: []string{"${{ inputs.arch }}"}}, expandContext{}); err == nil {
		t.Fatal("an unresolvable runs-on expanded")
	}
	e2 := newTestEnv(t)
	e2.git.add(testSHA, ".spindle/workflows/ci.yml", `name: Bad
on: push
jobs:
  a:
    runs-on: linux
    timeout-minutes: ${{ matrix.t }}
    strategy:
      matrix:
        t: [999]
    steps:
      - run: x
`)
	if _, err := e2.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"}); err == nil {
		t.Fatal("an out-of-range timeout expression created a run")
	}
}

func TestMatrixAdmissionDocShapes(t *testing.T) {
	e, runID := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 1
      matrix:
        v: [1, 2]
    steps:
      - run: echo ${{ matrix.v }}
`)
	key := MatrixAdmissionKey("o", "r", runID)
	// A record with no groups map initialises one.
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(`{"run_id":"`+runID+`"}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	entry := &QueueEntry{RunID: runID, JobID: "j-build-1", MatrixGroup: "build", MaxParallel: 1, Seq: "s1"}
	if ok, err := e.svc.admitMatrixCell(ctx(), "o", "r", entry); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	// A group whose Running map is nil initialises it and admits.
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(`{"run_id":"`+runID+`","groups":{"build":{"max":1}}}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.admitMatrixCell(ctx(), "o", "r", entry); err != nil || !ok {
		t.Fatalf("nil running = %v, %v", ok, err)
	}
}

func TestMatrixReleaseAndIfValuesNoGroups(t *testing.T) {
	e, runID := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 1
      matrix:
        v: [1, 2]
    steps:
      - run: echo x
`)
	key := MatrixAdmissionKey("o", "r", runID)
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(`{"run_id":"`+runID+`"}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseMatrixCell(ctx(), "o", "r", runID, "build", "j-build-1")
	// A group that exists but does not hold this job is a no-op.
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(`{"run_id":"`+runID+`","groups":{"build":{"max":1,"running":{"j-other":{"seq":"s","until":"2999-01-01T00:00:00Z"}}}}}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseMatrixCell(ctx(), "o", "r", runID, "build", "j-build-1")
	if vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{}}, &Run{Trusted: true}); err != nil || vals.Secrets != nil || vals.Vars != nil {
		t.Fatalf("empty spec = %+v, %v", vals, err)
	}
}

func TestDependentVerdictPaths(t *testing.T) {
	e, runID := needsEnv(t, `name: Chain
on:
  push:
    branches: [main]
jobs:
  lint:
    runs-on: linux
    steps:
      - run: echo lint
  build:
    runs-on: linux
    needs: [lint]
    if: needs.lint.result == 'success'
    steps:
      - run: echo build
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]*Job{}
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	// A missing job record is not a decision (the claim path retires it).
	entry := &QueueEntry{RunID: runID, JobID: "j-ghost"}
	if v, _, err := e.svc.dependentVerdict(ctx(), "o", "r", entry, nil, nil); err != nil || v != jobIfRun {
		t.Fatalf("missing job = %v, %v", v, err)
	}
	// A legacy entry with no NeedKeys reads Needs ids.
	entry = &QueueEntry{RunID: runID, JobID: byKey["build"].ID, Needs: []string{byKey["lint"].ID}}
	if _, _, err := e.svc.dependentVerdict(ctx(), "o", "r", entry, nil, nil); err != nil {
		t.Fatalf("legacy needs = %v", err)
	}
	// NeedKeys pointing at a missing record makes the facts Missing, which is
	// still a decision (the cascade settles it elsewhere).
	entry.NeedKeys = map[string][]string{"lint": {"j-ghost"}}
	if _, _, err := e.svc.dependentVerdict(ctx(), "o", "r", entry, nil, nil); err != nil {
		t.Fatalf("missing dep = %v", err)
	}
	// A job whose condition is outside the language is an error verdict.
	bad := byKey["build"]
	bad.Spec.If = "runner.os == 'Linux'"
	// UpdateJob does not rewrite the spec; store a modified record directly.
	body, _ := json.Marshal(bad)
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", runID, bad.ID), body, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	entry = &QueueEntry{RunID: runID, JobID: bad.ID}
	if v, _, err := e.svc.dependentVerdict(ctx(), "o", "r", entry, nil, nil); err != nil || v != jobIfError {
		t.Fatalf("bad condition = %v, %v", v, err)
	}
	// A secret-read failure fails the probe (fail closed).
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	if _, err := store.PutBytes(ctx(), e.store, SecretsPrefix("o", "r")+"BROKEN.json", []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	bad.Spec.If = "secrets.X != ''"
	bad.Spec.IfSecrets = true
	body, _ = json.Marshal(bad)
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", runID, bad.ID), body, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.dependentVerdict(ctx(), "o", "r", entry, nil, nil); err == nil {
		t.Fatal("a secret read failure was ignored")
	}
}

// TestMatrixMaxParallelConcurrentClaims guards the admission CAS under the real
// shape: several runner probes racing for one bounded matrix must admit exactly
// max-parallel cells, and a losing probe must never release the winner's slot.
func TestMatrixMaxParallelConcurrentClaims(t *testing.T) {
	e, _ := needsEnv(t, `name: Bounded
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    strategy:
      max-parallel: 2
      matrix:
        v: [1, 2, 3, 4, 5, 6]
    steps:
      - run: echo ${{ matrix.v }}
`)
	var mu sync.Mutex
	got := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := e.svc.claimLoop(ctx(), "o", "r", needsRunner(), []string{"linux"})
			if err != nil {
				return
			}
			if claim != nil {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got != 2 {
		t.Fatalf("admitted %d cells with max-parallel 2", got)
	}
}

func TestMatrixSlotReasonBranches(t *testing.T) {
	if got := matrixSlotReason(nil, nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if got := matrixSlotReason(&Job{}, nil); got != "" {
		t.Fatalf("nil spec = %q", got)
	}
	if got := matrixSlotReason(&Job{Spec: &Spec{Strategy: &StrategySpec{MaxParallel: 2}}}, nil); got != "" {
		t.Fatalf("no group = %q", got)
	}
	job := &Job{Spec: &Spec{Strategy: &StrategySpec{MaxParallel: 2}, MatrixGroup: "build"}}
	if got := matrixSlotReason(job, []*Job{job, {}, {Spec: &Spec{MatrixGroup: "other"}}}); got != "" {
		t.Fatalf("free slot = %q", got)
	}
	running := &Job{State: JobRunning, Spec: &Spec{MatrixGroup: "build"}}
	if got := matrixSlotReason(job, []*Job{running, running, job}); !strings.Contains(got, "max-parallel 2") {
		t.Fatalf("full = %q", got)
	}
	lease := "2026-11-27T00:00:00Z"
	claimed := &Job{State: JobQueued, LeaseUntil: &lease, Spec: &Spec{MatrixGroup: "build"}}
	if got := matrixSlotReason(job, []*Job{claimed, claimed, job}); !strings.Contains(got, "max-parallel 2") {
		t.Fatalf("claimed full = %q", got)
	}
}

func TestLoadJobIfValuesNilRun(t *testing.T) {
	e := newTestEnv(t)
	vals, err := e.svc.loadJobIfValues(ctx(), "o", "r", &Job{Spec: &Spec{IfSecrets: true}}, nil)
	if err != nil || len(vals.Secrets) != 0 {
		t.Fatalf("nil run = %+v, %v", vals, err)
	}
}

func TestExpandJobSpecAndRunnerReasonErrors(t *testing.T) {
	// An over-cap matrix is refused by expansion (the parser would have caught
	// it in a real file; a stored spec still must fail closed).
	big := make([]string, 257)
	for i := range big {
		big[i] = "v"
	}
	if _, err := expandJobSpec("a", "a", &Spec{Strategy: &StrategySpec{Matrix: map[string][]string{"x": big}}}, expandContext{}); err == nil {
		t.Fatal("an over-cap matrix expanded")
	}
	// A step timeout expression that resolves out of range fails expansion.
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: Bad
on: push
jobs:
  a:
    runs-on: linux
    steps:
      - run: x
        timeout-minutes: ${{ matrix.t }}
    strategy:
      matrix:
        t: [999]
`)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"}); err == nil {
		t.Fatal("an out-of-range step timeout created a run")
	}
	// A corrupt runner registry record is a named error on the reason path.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("broken"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.loadRunnerDocsForReasons(ctx()); err == nil {
		t.Fatal("a corrupt runner record was ignored")
	}
}

func TestDeploymentQueueReasonIsNamed(t *testing.T) {
	e := newTestEnv(t)
	run := &Run{ID: "r1", Repo: "o/r", State: RunQueued}
	jobs := []*Job{
		{ID: "j-a", State: JobQueued, Deployment: &Deployment{Environment: "prod", QueuePosition: 2}},
		{ID: "j-b", State: JobQueued, Deployment: &Deployment{Environment: "prod", QueuePosition: 1, QueueReason: "waiting for prod (deployment 1 in line)"}},
		{ID: "j-c", State: JobRunning},
	}
	e.svc.annotateQueueReasons(ctx(), "o", "r", run, jobs)
	if got := jobs[0].QueueReason; !strings.Contains(got, "deployment 2 in line") {
		t.Fatalf("default deployment reason = %q", got)
	}
	if got := jobs[1].QueueReason; got != "waiting for prod (deployment 1 in line)" {
		t.Fatalf("custom deployment reason = %q", got)
	}
}

func TestRunnerReasonStoreError(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	body, _ := json.Marshal(&RunnerDoc{ID: "r1", Labels: []string{"linux"}, State: RunnerActive, AllowAll: true})
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("r1"), body, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	fs.fail("get", 0)
	if _, err := e.svc.loadRunnerDocsForReasons(ctx()); err == nil {
		t.Fatal("a store failure reading a runner was ignored")
	}
}

func TestJobCancelUpdateForced(t *testing.T) {
	up := jobCancelUpdate("ada", "supersede", true)
	if up.State != JobCancelled || up.Reason != "cancelled by ada (forced)" || up.ForceCancelled == nil || !*up.ForceCancelled {
		t.Fatalf("forced cancel = %+v", up)
	}
}

func TestEnvironmentHelperBranches(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.sealEnvironmentSecret("o/r", "prod", "X", "v"); err == nil {
		t.Fatal("sealing without a keyring was accepted")
	}
	if _, err := e.svc.openEnvironmentSecret("o/r", "prod", "X", "!!!"); err == nil {
		t.Fatal("opening without a keyring was accepted")
	}
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	if _, err := e.svc.openEnvironmentSecret("o/r", "prod", "X", "!!!"); err == nil {
		t.Fatal("a bad environment-secret envelope was accepted")
	}
	_ = jobDeploymentLabel(nil, nil, "prod")
	_ = jobDeploymentLabel(&Run{ID: "r1"}, &Job{Key: "k", Name: ""}, "prod")
	up := jobCancelUpdate("", "", false)
	if up.Reason != "" {
		t.Fatalf("anonymous cancel reason = %q", up.Reason)
	}
}

func TestEnvironmentWriteRefusals(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "prod", EnvironmentInput{}, auth.Principal{Name: "nobody"}); err == nil {
		t.Fatal("PutEnvironment without maintain was accepted")
	}
	if err := e.svc.DeleteEnvironment(ctx(), "o/r", "prod", auth.Principal{Name: "nobody"}); err == nil {
		t.Fatal("DeleteEnvironment without maintain was accepted")
	}
}

func TestEnvironmentInputValidation(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "root", Admin: true}
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "bad name!", EnvironmentInput{}, admin); err == nil {
		t.Fatal("an invalid environment name was accepted")
	}
	if err := e.svc.DeleteEnvironment(ctx(), "o/r", "bad name!", admin); err == nil {
		t.Fatal("an invalid environment name was deleted")
	}
	if err := e.svc.SetEnvironmentSecret(ctx(), "o/r", "prod", "X", "v", admin); err == nil {
		t.Fatal("an environment secret was stored without a keyring")
	}
}

func TestEnvironmentVariableRefusals(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "root", Admin: true}
	nobody := auth.Principal{Name: "nobody"}
	if _, err := e.svc.ListEnvironmentVariables(ctx(), "badrepo", "prod", admin); err == nil {
		t.Fatal("ListEnvironmentVariables accepted a malformed repo")
	}
	if err := e.svc.SetEnvironmentVariable(ctx(), "o/r", "prod", "X", "v", admin); err == nil {
		t.Fatal("SetEnvironmentVariable stored without a keyring")
	}
	if err := e.svc.DeleteEnvironmentVariable(ctx(), "o/r", "prod", "X", nobody); err == nil {
		t.Fatal("DeleteEnvironmentVariable without maintain was accepted")
	}
}

// phantomRunnerStore lists one key that is not stored, exercising the
// "listed but absent" skip on the reason path.
type phantomRunnerStore struct{ *store.Memory }

func (p *phantomRunnerStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if err := p.Memory.List(ctx, prefix, after, fn); err != nil {
		return err
	}
	if prefix == RunnersPrefix() {
		return fn(store.ObjectMeta{Key: RunnersPrefix() + "ghost.json", Size: 1})
	}
	return nil
}

func TestRunnerReasonListedAbsent(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Store = &phantomRunnerStore{Memory: e.store}
	if docs, err := e.svc.loadRunnerDocsForReasons(ctx()); err != nil || len(docs) != 0 {
		t.Fatalf("phantom runner = %+v, %v", docs, err)
	}
	if got := envSecretAAD("o/r", "prod", "X"); len(got) == 0 {
		t.Fatal("envSecretAAD returned nothing")
	}
}

func TestRegisterAcceptsFleetSizingFields(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"token":"` + mint.Token + `","name":"r1","labels":["linux"],"ephemeral":true,"max_cpus":2,"max_memory_bytes":1073741824}`
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusCreated {
		t.Fatalf("register with fleet fields = %d: %s", rec.Code, rec.Body.String())
	}
}
