package threads

import (
	"encoding/json"
	"testing"
)

// Job-level `continue-on-error` (docs/features/21_parity_master.md PAR-056, D-4;
// docs/threads.md §3.3). GitHub's semantics are kept: the job record stays
// `failure` (marked tolerated), the RUN is not failed by it, `needs.<job>.result`
// reads success, and dependents run. Forgejo documents the key as ignored; that
// divergence is recorded in the docs and pinned here, not "fixed" towards it.

const continueOnErrorYAML = `name: Tolerated
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    continue-on-error: true
    steps:
      - run: echo build
  deploy:
    runs-on: linux
    needs: [build]
    if: needs.build.result == 'success'
    steps:
      - run: echo deploy
`

const continueOnErrorNoIfYAML = `name: ToleratedDefault
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    continue-on-error: true
    steps:
      - run: echo build
  deploy:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo deploy
`

// toleratedJob reads one job record back (state + tolerated).
func toleratedJob(t *testing.T, e *testEnv, runID, jobID string) *Job {
	t.Helper()
	raw, _, err := e.svc.getJSON(ctx(), JobKey("o", "r", runID, jobID))
	if err != nil || raw == nil {
		t.Fatalf("job %s: %v", jobID, err)
	}
	var job Job
	if jerr := json.Unmarshal(raw, &job); jerr != nil {
		t.Fatalf("job %s: %v", jobID, jerr)
	}
	return &job
}

const continueOnErrorSoloYAML = `name: ToleratedSolo
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    continue-on-error: true
    steps:
      - run: echo build
`

// TestContinueOnErrorJobKeepsTheRunGreen pins the run fold and the record.
func TestContinueOnErrorJobKeepsTheRunGreen(t *testing.T) {
	e, runID := needsEnv(t, continueOnErrorSoloYAML)
	if got := e.claimOne(t, runID); got != "j-build" {
		t.Fatalf("first claim = %q", got)
	}
	e.finish(t, runID, "j-build", JobRunning)
	e.finish(t, runID, "j-build", JobFailure)

	job := toleratedJob(t, e, runID, "j-build")
	if job.State != JobFailure || !job.Tolerated {
		t.Fatalf("build = state %s tolerated %v, want failure+tolerated", job.State, job.Tolerated)
	}
	run, err := e.svc.loadRun(ctx(), "o", "r", runID)
	if err != nil || run == nil {
		t.Fatalf("loadRun: %v", err)
	}
	if run.State != RunSuccess {
		t.Fatalf("run state = %s, want success (a tolerated failure keeps the run green)", run.State)
	}
	if run.State == RunFailure {
		t.Fatalf("a continue-on-error job failed the run")
	}
}

// TestContinueOnErrorDependentRuns pins `needs.<job>.result == 'success'` and
// the default condition: both must let the dependent run.
func TestContinueOnErrorDependentRuns(t *testing.T) {
	for name, workflow := range map[string]string{
		"explicit result condition": continueOnErrorYAML,
		"implicit success()":        continueOnErrorNoIfYAML,
	} {
		t.Run(name, func(t *testing.T) {
			e, runID := needsEnv(t, workflow)
			if got := e.claimOne(t, runID); got != "j-build" {
				t.Fatalf("first claim = %q", got)
			}
			e.finish(t, runID, "j-build", JobRunning)
			e.finish(t, runID, "j-build", JobFailure)
			if got := e.claimOne(t, runID); got != "j-deploy" {
				t.Fatalf("dependent claim = %q, want j-deploy", got)
			}
		})
	}
}

// TestDependencyResultToleratesOnlyAFailedToleratedJob pins the fold helper
// directly: a tolerated failure reads success, everything else reads its state.
func TestDependencyResultToleratesOnlyAFailedToleratedJob(t *testing.T) {
	cases := []struct {
		job  *Job
		want string
	}{
		{&Job{State: JobFailure, Tolerated: true}, JobSuccess},
		{&Job{State: JobFailure}, JobFailure},
		{&Job{State: JobSuccess, Tolerated: true}, JobSuccess},
		{&Job{State: JobCancelled}, JobCancelled},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := dependencyResult(tc.job); got != tc.want {
			t.Errorf("dependencyResult(%+v) = %q, want %q", tc.job, got, tc.want)
		}
	}
}

// TestFailedJobWithoutContinueOnErrorStillSkips pins that the tolerance is not
// a blanket rule: without the key, the dependent is skipped as before.
func TestFailedJobWithoutContinueOnErrorStillSkips(t *testing.T) {
	workflow := `name: Strict
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  deploy:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo deploy
`
	e, runID := needsEnv(t, workflow)
	if got := e.claimOne(t, runID); got != "j-build" {
		t.Fatalf("first claim = %q", got)
	}
	e.finish(t, runID, "j-build", JobRunning)
	e.finish(t, runID, "j-build", JobFailure)
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("dependent claimed after an untolerated failure: %q", got)
	}
	if state := e.jobState(t, runID, "j-deploy"); state != JobSkipped {
		t.Fatalf("deploy state = %s, want skipped", state)
	}
}
