// threads_jobsem_test.go — round 4 end to end (docs/features/15 §4.1): a job
// whose `if:` is false never executes, and a failed job with
// `continue-on-error: true` does not fail the run. Driven by the real runner
// binary against real containers (the decision itself is server-side, so the
// proof is that the container never starts and the run conclusion ignores the
// tolerated failure).
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jobSemanticsWorkflow has three jobs: one skipped by its condition, one that
// fails but is tolerated, and one plain green job. The run must conclude
// success, the skipped job must carry its reason, and the tolerated failure
// must be visible on its own record.
const jobSemanticsWorkflow = `name: JobSemantics
on: [push]
jobs:
  skip-me:
    name: skip-me
    runs-on: linux
    if: github.event_name == 'schedule'
    steps:
      - run: echo "must-never-run"
  flaky:
    name: flaky
    runs-on: linux
    continue-on-error: true
    steps:
      - run: echo "flaky-ran" && exit 1
  gate:
    name: gate
    runs-on: linux
    steps:
      - run: echo "gate-ok"
`

// TestE2E_ThreadsJobIfAndToleratedFailure pins the two semantics changes with a
// live runner: `if: false` is skipped without a container, and a tolerated
// failure leaves the run green while its job record still reads `failure`.
func TestE2E_ThreadsJobIfAndToleratedFailure(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsjobif")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/if.yml", jobSemanticsWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 2)

	var runID string
	pollUntil(t, 60*time.Second, "the run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	pollUntil(t, 240*time.Second, "the run to finish", func() bool {
		st := getRun(t, repoLane, runID, chainAliceTok).Run["state"]
		return st == "success" || st == "failure" || st == "cancelled"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if run.Run["state"] != "success" {
		t.Fatalf("run = %v (want success: the only failure is tolerated)\n%s",
			run.Run["state"], runner.logs.String())
	}
	skipped := jobState(run, "j-skip-me")
	if skipped == nil {
		t.Fatalf("run jobs = %v", run.Jobs)
	}
	if skipped["state"] != "skipped" {
		t.Fatalf("if: false job = %v, want skipped", skipped["state"])
	}
	if reason, _ := skipped["reason"].(string); reason == "" {
		t.Fatalf("skipped job has no reason: %v", skipped)
	}
	if skipped["started_at"] != nil {
		t.Fatalf("a skipped job started: %v", skipped["started_at"])
	}
	// The skipped job never ran: its log carries no step output (the runner
	// never claimed it, so there is no container to print from).
	if logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-skip-me/log", repoLane, runID), chainAliceTok); strings.Contains(logText, "must-never-run") {
		t.Fatalf("the skipped job executed:\n%s", logText)
	}
	flaky := jobState(run, "flaky")
	if flaky == nil {
		flaky = jobState(run, "j-flaky")
	}
	if flaky == nil {
		t.Fatalf("flaky job missing: %v", run.Jobs)
	}
	if flaky["state"] != "failure" {
		t.Fatalf("tolerated job = %v, want failure (the record is honest)", flaky["state"])
	}
	if tolerated, _ := flaky["tolerated"].(bool); !tolerated {
		t.Fatalf("tolerated job does not say so: %v", flaky)
	}
	if flaky["step_results"] == nil {
		t.Fatalf("tolerated job has no step results: %v", flaky)
	}
	flakyLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/%s/log", repoLane, runID, flaky["id"]), chainAliceTok)
	if !strings.Contains(flakyLog, "flaky-ran") {
		t.Fatalf("the tolerated job did not execute:\n%s", flakyLog)
	}
	gate := jobState(run, "j-gate")
	if gate == nil || gate["state"] != "success" {
		t.Fatalf("gate job = %v, want success", gate)
	}
}
