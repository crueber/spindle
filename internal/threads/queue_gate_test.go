package threads

// The claim probe's job-`if:`/`needs:` gate (queue.go, 15 §4.1). The cascade
// normally decides a dependent before a probe ever runs; these tests drive the
// probe's own branches (a decision the cascade has not made, so a probe cannot
// rely on it) and its fail-closed reads.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// requeueJobRaw rewrites one job record back to `queued` WITHOUT the transition
// machinery, so the run's cascade does not run: this is the state a probe must
// handle on its own (for example when another instance's cascade did not).
func (e *testEnv) requeueJobRaw(t *testing.T, runID, jobID string) *Job {
	t.Helper()
	raw, _, err := e.svc.getJSON(ctx(), JobKey("o", "r", runID, jobID))
	if err != nil || raw == nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}
	var job Job
	if jerr := json.Unmarshal(raw, &job); jerr != nil {
		t.Fatalf("decode job %s: %v", jobID, jerr)
	}
	job.State = JobQueued
	job.CompletedAt = nil
	job.Reason = ""
	job.Version++
	body, _ := json.Marshal(&job)
	if _, perr := store.PutBytes(ctx(), e.store, JobKey("o", "r", runID, jobID), body, store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatalf("requeue raw: %v", perr)
	}
	return &job
}

// enqueueOne puts one job back on the queue (a fresh sequence).
func (e *testEnv) enqueueOne(t *testing.T, job *Job) {
	t.Helper()
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", job.RunID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if _, eerr := e.svc.enqueueJob(ctx(), "o", "r", job, entryNeedMap(needsIndex(jobs), job)); eerr != nil {
		t.Fatalf("enqueue: %v", eerr)
	}
}

// TestProbeDecidesAFalseJobIfWithoutTheCascade pins the probe's own decision: a
// dependent left queued (a cascade that did not run) is terminalised by the
// probe, with the condition on the record, instead of being offered forever.
func TestProbeDecidesAFalseJobIfWithoutTheCascade(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	e.finish(t, runID, "j-lint", JobFailure) // the cascade skips j-build
	build := e.requeueJobRaw(t, runID, "j-build")
	e.enqueueOne(t, build)

	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("a dependent of a failed job was offered: %q", got)
	}
	after := e.jobRecord(t, runID, "j-build")
	if after.State != JobSkipped || after.Reason != "needs: lint concluded failure" {
		t.Fatalf("probe outcome = %q (%q)", after.State, after.Reason)
	}
}

// TestProbeFailsAnUnsupportedDependentIf pins that an expression the subset
// cannot read fails the job at the probe too (never a silent skip).
func TestProbeFailsAnUnsupportedDependentIf(t *testing.T) {
	workflow := `name: BadDependentIf
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
    if: '!needs.lint.result'
    steps:
      - run: echo build
`
	e, runID := needsEnv(t, workflow)
	e.finish(t, runID, "j-lint", JobSuccess)
	// The cascade already failed j-build; restore the pre-decision state.
	build := e.requeueJobRaw(t, runID, "j-build")
	e.enqueueOne(t, build)

	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("a job with an unreadable condition was offered: %q", got)
	}
	after := e.jobRecord(t, runID, "j-build")
	if after.State != JobFailure || after.Reason == "" {
		t.Fatalf("probe outcome = %q (%q), want failure with the reason", after.State, after.Reason)
	}
}

// TestProbeSkipsADependentWhoseDependencyIsUnreadable pins the fail-closed read:
// a dependency record that is gone means the job can never be evaluated.
func TestProbeSkipsADependentWhoseDependencyIsUnreadable(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	// Remove the dependency record and re-offer the dependent.
	if derr := e.store.Delete(ctx(), JobKey("o", "r", runID, "j-lint"), ""); derr != nil {
		t.Fatalf("delete dep: %v", derr)
	}
	build, err := e.svc.loadJobCached(ctx(), "o", "r", runID, "j-build", nil)
	if err != nil || build == nil {
		t.Fatalf("load build: %v", err)
	}
	e.enqueueOne(t, build)

	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("a job with an unreadable dependency was offered: %q", got)
	}
	after := e.jobRecord(t, runID, "j-build")
	if after.State != JobSkipped || after.Reason != "needs: a dependency record could not be read" {
		t.Fatalf("probe outcome = %q (%q)", after.State, after.Reason)
	}
}

// TestProbeSurfacesADecisionWriteFailure pins that a failed terminal write is an
// error, not a silently claimed job.
func TestProbeSurfacesADecisionWriteFailure(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	e.finish(t, runID, "j-lint", JobFailure)
	build := e.requeueJobRaw(t, runID, "j-build")
	e.enqueueOne(t, build)
	e.checks.err = errors.New("checks down")
	defer func() { e.checks.err = nil }()
	if _, err := e.svc.claimLoop(ctx(), "o", "r", needsRunner(), []string{"linux"}); err == nil {
		t.Fatal("a failed decision write was swallowed")
	}
}
