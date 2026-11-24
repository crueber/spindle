// threads_outputs_test.go — step and job outputs end to end (docs/threads.md
// §3.9): a real container writes `$SPINDLE_OUTPUT`, a later step gates on it,
// the job's `outputs:` carry it across the job boundary, and a dependent job
// asserts `needs.<job>.outputs.<name>`. Also: a failed step's output is absent
// (reading it fails the job closed), and an output that carries a secret is not
// masked as DATA across the boundary (the documented caveat) while the log line
// that echoes it is still masked.
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable.
package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const outputsWorkflow = `name: Outputs
on: [push]
jobs:
  build:
    name: build
    runs-on: linux
    outputs:
      digest: ${{ steps.gen.outputs.digest }}
      payload: ${{ steps.gen.outputs.payload }}
      token: ${{ steps.gen.outputs.token }}
    steps:
      - name: generate
        id: gen
        run: |
          echo "digest=abc123" >> "$SPINDLE_OUTPUT"
          printf 'payload<<EOF\nline one\nline two\nEOF\n' >> "$SPINDLE_OUTPUT"
          echo "token=$OUTPUT_SECRET" >> "$SPINDLE_OUTPUT"
          echo "auth header is $OUTPUT_SECRET"
      - name: gate on the step output
        id: gate
        if: steps.gen.outputs.digest == 'abc123'
        run: echo "step-gate-ok"
      - name: substituted into env and run
        env:
          DIGEST: ${{ steps.gen.outputs.digest }}
        run: |
          test "$DIGEST" = abc123
          test "${{ steps.gen.outputs.digest }}" = abc123
          echo "substitution-ok"
  consume:
    name: consume
    runs-on: linux
    needs: [build]
    if: needs.build.outputs.digest == 'abc123'
    steps:
      - name: assert the needs outputs
        env:
          DIGEST: ${{ needs.build.outputs.digest }}
          PAYLOAD: ${{ needs.build.outputs.payload }}
        run: |
          test "$DIGEST" = abc123
          test "$PAYLOAD" = "$(printf 'line one\nline two')"
          # The output carried the secret verbatim across the job boundary:
          # outputs are data, not masked (docs/threads.md §3.9).
          test "${{ needs.build.outputs.token }}" = "$OUTPUT_SECRET"
          echo "needs-ok"
      - name: echo the output that carried a secret
        run: echo "cross-job-secret=${{ needs.build.outputs.token }}"
  broken:
    name: broken
    runs-on: linux
    continue-on-error: true
    outputs:
      leak: ${{ steps.fail.outputs.leak }}
    steps:
      - name: fail after writing
        id: fail
        continue-on-error: true
        run: |
          echo "leak=should-not-appear" >> "$SPINDLE_OUTPUT"
          exit 1
`

// TestE2E_ThreadsStepAndJobOutputs is the outputs acceptance: a step output
// gates a later step, the job outputs reach a dependent job, a failed step's
// output is absent (a named failure), and a secret-bearing output crosses the
// job boundary as data (documented) while its log echo is masked.
func TestE2E_ThreadsStepAndJobOutputs(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsout")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const secret = "output-secret-value"
	if st, body := apiCall(t, "PUT", repoLane+"/threads/secrets/OUTPUT_SECRET",
		`{"value":"`+secret+`"}`, chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("put secret = %d: %s", st, body)
	}

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/outputs.yml", outputsWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 2)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	var run threadRun
	deadline := time.Now().Add(180 * time.Second)
	for {
		run = getRun(t, repoLane, runID, chainAliceTok)
		if state, _ := run.Run["state"].(string); state == "success" || state == "failure" || state == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in 3m\njobs: %v\nrunner:\n%s", runID, run.Jobs, runner.logs.String())
		}
		time.Sleep(time.Second)
	}
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("run = %v, want success\njobs: %v\nbuild log:\n%s\nconsume log:\n%s\nbroken log:\n%s\nrunner:\n%s",
			state, run.Jobs,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), chainAliceTok),
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, runID), chainAliceTok),
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-broken/log", repoLane, runID), chainAliceTok),
			runner.logs.String())
	}
	for id, want := range map[string]string{"j-build": "success", "j-consume": "success", "j-broken": "failure"} {
		st := jobState(run, id)
		if st == nil || st["state"] != want {
			t.Fatalf("job %s = %v, want %s\njobs: %v", id, st, want, run.Jobs)
		}
	}
	// The job record carries the reported outputs (the API read model).
	build := jobState(run, "j-build")
	outputs, _ := build["outputs"].(map[string]any)
	if outputs["digest"] != "abc123" || outputs["payload"] != "line one\nline two" {
		t.Fatalf("stored outputs = %v", outputs)
	}

	buildLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{"step-gate-ok", "substitution-ok", "auth header is ***"} {
		if !strings.Contains(buildLog, want) {
			t.Fatalf("build log misses %q:\n%s", want, buildLog)
		}
	}
	if strings.Contains(buildLog, secret) {
		t.Fatalf("the build log leaked the secret value:\n%s", buildLog)
	}

	consumeLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(consumeLog, "needs-ok") {
		t.Fatalf("consume log lacks its assertions:\n%s", consumeLog)
	}
	// Documented: outputs are data, not masked after collection — the dependent
	// step's own assertion proved the secret value crossed verbatim. The LOG
	// LINE that echoes it is still masked by the existing log masking, so a
	// workflow cannot print an output-borne secret into the log for free.
	if !strings.Contains(consumeLog, "cross-job-secret=***") || strings.Contains(consumeLog, secret) {
		t.Fatalf("a log line echoing an output-borne secret was not masked:\n%s", consumeLog)
	}

	brokenLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-broken/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(brokenLog, "did not publish the output") {
		t.Fatalf("a failed step's output was not reported as absent:\n%s", brokenLog)
	}
	if strings.Contains(brokenLog, "should-not-appear") {
		t.Fatalf("a failed step's output value leaked into the job:\n%s", brokenLog)
	}
}
