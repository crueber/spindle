// threads_pr_target_test.go — PAR-015's end-to-end proof (docs/threads.md §5.2,
// D-2): a FORK pull request's `pull_request_target` run executes the BASE tip's
// workflow file (never the head's), receives the BASE repository's secrets, and
// sets the commit's badge. The head checkout stays refused, which the unit tier
// pins; this scenario proves the execution half with a real runner and real
// containers.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestE2E_ThreadsPullRequestTarget(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsprt")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	// The BASE file: `pull_request_target` only, a base marker, and a probe that
	// reports whether the repository secret resolved WITHOUT printing it.
	const baseWorkflow = `name: PRT
on: [pull_request_target]
jobs:
  probe:
    name: probe
    runs-on: linux
    steps:
      - name: markers
        run: |
          echo "workflow-from-base"
          if [ -n "${DEPLOY_SECRET:-}" ]; then echo "secret-present"; else echo "secret-missing"; fi
`
	// The HEAD file (fork branch) would print a different marker if the head's
	// copy were executed. It must never run.
	const headWorkflow = `name: PRT
on: [pull_request]
jobs:
  probe:
    name: probe
    runs-on: linux
    steps:
      - run: echo "workflow-from-head"
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	baseSHA := g.commitFile(work, ".spindle/workflows/ci.yml", baseWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// The base repository's secret: a trusted pull_request_target run resolves
	// it; the value must never appear in the log.
	const secretValue = "pr-target-must-not-leak"
	if st, body := apiCall(t, "PUT", repoLane+"/threads/secrets/DEPLOY_SECRET",
		fmt.Sprintf(`{"value":%q}`, secretValue), chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("put secret = %d: %s", st, body)
	}
	// D-2: the trigger is off by default and a maintainer enables it.
	if st, body := apiCall(t, "PUT", repoLane+"/threads/settings",
		`{"pull_request_target":true}`, chainAliceTok); st != http.StatusOK {
		t.Fatalf("enable pull_request_target = %d: %s", st, body)
	}

	// Fork and push a head branch whose workflow file differs, then open the PR.
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	forkURL := s.gitURL(owner, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, chainAliceTok) != nil
	})
	g.run(work, "checkout", "-b", "forkprt")
	g.commitFile(work, ".spindle/workflows/ci.yml", headWorkflow, "head workflow")
	g.commitFile(work, "fork.txt", "from the fork\n", "fork change")
	g.runAuth(work, chainAliceTok, "push", "origin", "forkprt")
	g.runAuth(work, chainAliceTok, "push", forkURL, "forkprt")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"pr target","base_ref":"refs/heads/main","head_ref":"refs/heads/forkprt","fork":{"repo":%q}}`,
			owner+"/"+forkName), chainAliceTok)

	// The fork gate holds the run (a host admin is not resolvable from the bare
	// event actor, PLAN-OIDC-ONLY §1.3): approve it explicitly.
	var runID string
	pollUntil(t, 60*time.Second, "the held pull_request_target run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] != "pull_request_target" {
				continue
			}
			if m["state"] == "pending_approval" {
				runID, _ = m["id"].(string)
				return runID != ""
			}
		}
		return false
	})
	mustAPI(t, "POST", repoLane+"/threads/"+url.PathEscape(runID)+"/approve", "", chainAliceTok)

	// Execute it for real.
	runner := startThreadsRunner(t, bin, s.base, mintRunnerToken(t, repoLane, chainAliceTok), "linux", 1)
	pollUntil(t, 120*time.Second, "the pull_request_target run to finish", func() bool {
		run := getRun(t, repoLane, runID, chainAliceTok)
		state, _ := run.Run["state"].(string)
		return state == "success" || state == "failure"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("pull_request_target run = %v (%v)", state, run.Jobs)
	}
	if prTarget, _ := run.Run["pr_target"].(bool); !prTarget {
		t.Fatalf("run is not marked pr_target: %v", run.Run)
	}
	if trusted, _ := run.Run["trusted"].(bool); !trusted {
		t.Fatalf("a pull_request_target run must be trusted: %v", run.Run)
	}
	// The workflow file is the BASE tip's, never the head's.
	if got := run.Run["workflow_commit"]; got != baseSHA {
		t.Fatalf("workflow_commit = %v, want the base tip %s (head %s)", got, baseSHA, run.Run["commit"])
	}

	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-probe/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(logText, "workflow-from-base") {
		t.Fatalf("the base workflow did not execute:\n%s", logText)
	}
	if strings.Contains(logText, "workflow-from-head") {
		t.Fatalf("the HEAD workflow file executed under pull_request_target:\n%s", logText)
	}
	if !strings.Contains(logText, "secret-present") {
		t.Fatalf("the base repository secret did not resolve:\n%s", logText)
	}
	if strings.Contains(logText, secretValue) {
		t.Fatalf("the secret value reached the log:\n%s", logText)
	}

	// The badge is set: the run's check context exists on the commit it ran and
	// reads success in the combined view.
	checks := mustAPI(t, "GET", repoLane+"/checks/"+baseSHA, "", chainAliceTok)
	statuses, _ := checks["statuses"].([]any)
	found := false
	for _, raw := range statuses {
		st, _ := raw.(map[string]any)
		context, _ := st["context"].(string)
		if context != "threads/prt/probe" {
			continue
		}
		found = true
		if st["state"] != "success" {
			t.Fatalf("check %s state = %v", context, st["state"])
		}
	}
	if !found {
		body, _ := json.Marshal(checks)
		t.Fatalf("no threads/prt/probe check on %s: %s", baseSHA, body)
	}
	waitForConsole(t, runner, "job complete")
}
