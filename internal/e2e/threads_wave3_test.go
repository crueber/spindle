// threads_wave3_test.go — Wave 3 reusable workflows end to end
// (docs/features/21_parity_master.md PAR-075/PAR-076, docs/threads.md §3.4b):
// a caller passes a typed input and a renamed secret to a callee, the callee's
// declared output is consumed by a later job, and a cross-repository reference
// this engine cannot run is refused by name rather than accepted as inert
// syntax.
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable for the
// execution half.
package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wave3CallerWorkflow = `name: Chain
on: [push]
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: production
    secrets:
      REGISTRY_TOKEN: ${{ secrets.REGISTRY_SECRET }}
  verify:
    needs: [deploy]
    runs-on: linux
    steps:
      - run: echo "deployed=${{ needs.deploy.outputs.url }}"
`

const wave3CalleeWorkflow = `name: Deploy
on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
    secrets:
      REGISTRY_TOKEN:
        required: true
    outputs:
      url:
        description: the deployed URL
        value: ${{ jobs.release.outputs.url }}
jobs:
  release:
    runs-on: linux
    outputs:
      url: ${{ steps.publish.outputs.url }}
    steps:
      - id: publish
        env:
          TARGET: ${{ inputs.target }}
          TOKEN: ${{ secrets.REGISTRY_TOKEN }}
        run: |
          echo "target=$TARGET"
          if [ -n "$TOKEN" ]; then echo "secret=present"; else echo "secret=missing"; exit 1; fi
          echo "url=https://example.test/$TARGET" >> "$GITHUB_OUTPUT"
`

func TestE2E_ThreadsReusableWorkflowChain(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsw3")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	// A first push creates the repository, so the secret exists before the run
	// that consumes it is created.
	g.commitFile(work, "README.md", "wave3\n", "init")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	if st, body := apiCall(t, "PUT", repoLane+"/threads/secrets/REGISTRY_SECRET",
		`{"value":"e2e-registry-value"}`, chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("set secret: status %d body %s", st, body)
	}

	g.commitFile(work, ".spindle/workflows/chain.yml", wave3CallerWorkflow, "caller")
	g.commitFile(work, ".spindle/workflows/deploy.yml", wave3CalleeWorkflow, "callee")
	g.runAuth(work, chainAliceTok, "push")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 2)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	run := waitRunTerminal(t, repoLane, runID, chainAliceTok, 180*time.Second, runner)
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("run = %v, want success\njobs: %v\nrelease log:\n%s\nverify log:\n%s\nrunner:\n%s",
			state, run.Jobs,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-deploy-release/log", repoLane, runID), chainAliceTok),
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-verify/log", repoLane, runID), chainAliceTok),
			runner.logs.String())
	}
	for _, id := range []string{"j-deploy-release", "j-verify"} {
		if st := jobState(run, id); st == nil || st["state"] != "success" {
			t.Fatalf("job %s = %v, want success (jobs: %v)", id, st, run.Jobs)
		}
	}

	releaseLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-deploy-release/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(releaseLog, "target=production") {
		t.Fatalf("the typed input did not reach the callee:\n%s", releaseLog)
	}
	if !strings.Contains(releaseLog, "secret=present") {
		t.Fatalf("the renamed secret did not reach the callee:\n%s", releaseLog)
	}
	if strings.Contains(releaseLog, "e2e-registry-value") {
		t.Fatalf("a secret value reached the log:\n%s", releaseLog)
	}
	verifyLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-verify/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(verifyLog, "deployed=https://example.test/production") {
		t.Fatalf("the callee output did not reach the dependent job:\n%s", verifyLog)
	}
}

// TestE2E_ThreadsReusableCrossRepoRefused proves both cross-repository refusals
// at the server boundary: `secrets: inherit` to a remote callee disables that
// file with a D-6 parse error (and creates no run), and a mutable remote ref
// creates a run whose caller job fails by name with no runner claim.
func TestE2E_ThreadsReusableCrossRepoRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	g := newGitClient(t)
	s := threadsServer(t)
	owner, repo := testOwner, uniqueRepo("threadsw3refuse")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/inherit.yml", `name: Inherit
on: [push]
jobs:
  deploy:
    uses: acme/other/.spindle/workflows/deploy.yml@`+strings.Repeat("a", 40)+`
    secrets: inherit
`, "inherit")
	g.commitFile(work, ".spindle/workflows/mutable.yml", `name: Mutable
on: [push]
jobs:
  deploy:
    uses: acme/other/.spindle/workflows/deploy.yml@main
`, "mutable")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// The inherit file is a parse error naming D-6; the mutable-ref file parses
	// and produces one run whose caller job fails by name.
	list := mustAPI(t, "GET", repoLane+"/threads/workflows", "", chainAliceTok)
	files, _ := list["workflows"].([]any)
	var inheritErr, mutableErr string
	for _, f := range files {
		m, _ := f.(map[string]any)
		switch m["path"] {
		case ".spindle/workflows/inherit.yml":
			inheritErr, _ = m["error"].(string)
		case ".spindle/workflows/mutable.yml":
			mutableErr, _ = m["error"].(string)
		}
	}
	if !strings.Contains(inheritErr, "D-6") {
		t.Fatalf("inherit parse error = %q", inheritErr)
	}
	if mutableErr != "" {
		t.Fatalf("the pinned-ref rule must be a job refusal, not a parse error: %q", mutableErr)
	}

	var runID string
	pollUntil(t, 60*time.Second, "the mutable-ref run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if run.Run["workflow"] != "Mutable" {
		t.Fatalf("run = %v, want the mutable-ref workflow only", run.Run)
	}
	st := jobState(run, "j-deploy")
	if st == nil || st["state"] != "failure" {
		t.Fatalf("caller job = %v, want a named failure", st)
	}
	if reason, _ := st["reason"].(string); !strings.Contains(reason, "40-character commit SHA") {
		t.Fatalf("caller refusal = %q", reason)
	}
}

// waitRunTerminal polls one run until it reaches a terminal state, failing with
// the job list and the runner log when it does not.
func waitRunTerminal(t *testing.T, repoLane, runID, token string, timeout time.Duration, runner *runnerProc) threadRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		run := getRun(t, repoLane, runID, token)
		switch state, _ := run.Run["state"].(string); state {
		case "success", "failure", "cancelled":
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in %s\njobs: %v\nrunner:\n%s", runID, timeout, run.Jobs, runner.logs.String())
		}
		time.Sleep(time.Second)
	}
}
