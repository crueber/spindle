package threads

import (
	"errors"
	"strings"
	"testing"
)

// PAR-059: re-run failed jobs only. The run's attempt advances, the failed jobs
// and their dependents run again, and every unrelated successful job keeps its
// state, attempt and check.

const rerunFailedYAML = `name: Rerun
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
    steps:
      - run: echo build
  test:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo test
  docs:
    runs-on: linux
    steps:
      - run: echo docs
`

func TestRerunFailedOnlyPreservesSuccessfulJobs(t *testing.T) {
	e, runID := needsEnv(t, rerunFailedYAML)

	// lint and docs have no dependencies: both are claimable at once.
	first := map[string]bool{}
	for i := 0; i < 2; i++ {
		id := e.claimOne(t, runID)
		if id == "" {
			t.Fatal("expected two independent jobs to be claimable")
		}
		first[id] = true
	}
	if !first["j-lint"] || !first["j-docs"] {
		t.Fatalf("first claims = %v", first)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("dependent claimed before its dependency finished: %q", got)
	}
	e.finish(t, runID, "j-lint", JobSuccess)
	e.finish(t, runID, "j-docs", JobSuccess)
	if got := e.claimOne(t, runID); got != "j-build" {
		t.Fatalf("build claim = %q", got)
	}
	e.finish(t, runID, "j-build", JobFailure)
	if got := e.jobState(t, runID, "j-test"); got != JobSkipped {
		t.Fatalf("dependent of the failure = %q", got)
	}

	// Seed the failed job with an annotation and a summary so the rerun's
	// attempt reset (which clears both) is exercised.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", runID, "j-build", JobUpdate{
		Annotations: []Annotation{{Level: "error", Message: "boom"}},
		Summary:     strPtr("## failed"),
	}); err != nil {
		t.Fatalf("seed annotations: %v", err)
	}

	docsReports := 0
	for _, rep := range e.checks.snapshot() {
		if rep.Context == CheckContext("Rerun", "docs") {
			docsReports++
		}
	}
	lint := e.jobRecord(t, runID, "j-lint")
	docs := e.jobRecord(t, runID, "j-docs")

	run, err := e.svc.RerunFailedRun(ctx(), "o/r", runID, e.writer())
	if err != nil {
		t.Fatalf("RerunFailedRun: %v", err)
	}
	if run.Attempt != 2 {
		t.Fatalf("run attempt = %d, want 2", run.Attempt)
	}
	// The failed job and its dependent are queued for a new attempt.
	for _, id := range []string{"j-build", "j-test"} {
		job := e.jobRecord(t, runID, id)
		if job.State != JobQueued || job.Attempt != 2 {
			t.Fatalf("%s after rerun = %s attempt %d", id, job.State, job.Attempt)
		}
		if job.Annotations != nil || job.Summary != "" {
			t.Fatalf("%s kept the previous attempt's annotations/summary: %+v %q", id, job.Annotations, job.Summary)
		}
	}
	// The green jobs are untouched: same state, same attempt, same check
	// history (no new report was appended for them).
	for _, before := range []*Job{lint, docs} {
		after := e.jobRecord(t, runID, before.ID)
		if after.State != JobSuccess || after.Attempt != before.Attempt {
			t.Fatalf("%s was rerun: %s attempt %d (was %s attempt %d)",
				after.ID, after.State, after.Attempt, before.State, before.Attempt)
		}
		if after.StepResults != nil {
			t.Fatalf("%s kept its step results: %+v", after.ID, after.StepResults)
		}
	}
	got := 0
	for _, rep := range e.checks.snapshot() {
		if rep.Context == CheckContext("Rerun", "docs") {
			got++
		}
	}
	if got != docsReports {
		t.Fatalf("the successful job's check was rewritten: %d reports, want %d", got, docsReports)
	}

	// The rerun sets run again, in order.
	if got := e.claimOne(t, runID); got != "j-build" {
		t.Fatalf("rerun first claim = %q", got)
	}
	e.finish(t, runID, "j-build", JobSuccess)
	if got := e.claimOne(t, runID); got != "j-test" {
		t.Fatalf("rerun second claim = %q", got)
	}
	e.finish(t, runID, "j-test", JobSuccess)
	if final, _ := e.svc.loadRun(ctx(), "o", "r", runID); final.State != RunSuccess {
		t.Fatalf("run after the rerun = %s", final.State)
	}
}

func strPtr(s string) *string { return &s }

func TestRerunFailedRefusesAGreenRun(t *testing.T) {
	e, runID := needsEnv(t, independentWorkflowYAML)
	for {
		id := e.claimOne(t, runID)
		if id == "" {
			break
		}
		e.finish(t, runID, id, JobSuccess)
	}
	if _, err := e.svc.RerunFailedRun(ctx(), "o/r", runID, e.writer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("green rerun = %v, want a named conflict", err)
	}
	if _, err := e.svc.RerunFailedRun(ctx(), "o/r", runID, e.reader()); err == nil {
		t.Fatal("a reader was allowed to rerun")
	}
}

// failedRerunSet is the selection rule in one table: a failure plus its
// transitive dependents, never an independent green job or a cancelled matrix
// sibling.
func TestFailedRerunSelection(t *testing.T) {
	jobs := []*Job{
		{ID: "a", Key: "a", State: JobSuccess},
		{ID: "b", Key: "b", State: JobFailure},
		{ID: "c", Key: "c", State: JobSkipped, Needs: []string{"b"}},
		{ID: "d", Key: "d", State: JobSkipped, Needs: []string{"c"}},
		{ID: "e", Key: "e", State: JobSuccess},
		{ID: "f", Key: "f", State: JobCancelled},
	}
	got := failedRerunSet(jobs)
	names := make([]string, 0, len(got))
	for _, j := range got {
		names = append(names, j.ID)
	}
	if strings.Join(names, ",") != "b,c,d" {
		t.Fatalf("selection = %v", names)
	}
}
