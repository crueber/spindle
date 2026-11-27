// threads_wave6_reusable_test.go — PAR-076's remaining acceptance clause: a
// SHA-pinned remote reusable workflow EXECUTES on a real runner, receives only
// the explicitly passed secret, has its permissions downgraded to the caller's
// cap, and is audited (the audit itself is unit-tested).
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestE2E_ThreadsReusableCrossRepoExecution(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)

	upstream := uniqueRepo("w6up")
	caller := uniqueRepo("w6caller")
	upLane := s.base + "/" + testOwner + "/" + upstream + "/api"
	callerLane := s.base + "/" + testOwner + "/" + caller + "/api"

	const callee = `name: Remote Deploy
on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
    secrets:
      DEPLOY_TOKEN:
        required: true
permissions:
  contents: write
jobs:
  release:
    runs-on: linux
    steps:
      - name: deploy
        env:
          DEPLOY_TOKEN: ${{ secrets.DEPLOY_TOKEN }}
        run: |
          echo "target=${{ inputs.target }}"
          echo "secret=${DEPLOY_TOKEN:+present}"
`
	upWork := filepath.Join(t.TempDir(), "up")
	g.initRepo(upWork)
	upSHA := g.commitFile(upWork, ".spindle/workflows/deploy.yml", callee, "callee")
	g.run(upWork, "remote", "add", "origin", s.gitURL(testOwner, upstream))
	g.runAuth(upWork, chainAliceTok, "push", "-u", "origin", "main")

	// The caller repository must exist before it can carry a secret: create it
	// with a README push (no workflow file yet, so no run).
	callerWork := filepath.Join(t.TempDir(), "caller")
	g.initRepo(callerWork)
	g.commitFile(callerWork, "README.md", "caller\n", "init")
	g.run(callerWork, "remote", "add", "origin", s.gitURL(testOwner, caller))
	g.runAuth(callerWork, chainAliceTok, "push", "-u", "origin", "main")
	// The explicit secret lives in the CALLER's repository.
	secretVal := "w6-secret-value-1234"
	if st, body := apiCall(t, "PUT", callerLane+"/threads/secrets/REGISTRY_TOKEN",
		fmt.Sprintf(`{"value":%q}`, secretVal), chainAliceTok); st != 204 {
		t.Fatalf("set caller secret = %d %s", st, body)
	}

	const callerWorkflowTemplate = `name: Caller
on: [push]
permissions:
  contents: read
jobs:
  deploy:
    uses: %s/%s/.spindle/workflows/deploy.yml@%s
    with:
      target: production
    secrets:
      DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}
  verify:
    needs: [deploy]
    runs-on: linux
    steps:
      - run: echo verified
`
	g.commitFile(callerWork, ".spindle/workflows/ci.yml",
		fmt.Sprintf(callerWorkflowTemplate, testOwner, upstream, upSHA), "caller")
	g.runAuth(callerWork, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, callerLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)
	run := waitNewRun(t, callerLane, "push", nil)
	waitRunSuccess(t, callerLane, run.ID, chainAliceTok, runner)

	view := getRun(t, callerLane, run.ID, chainAliceTok)
	var release, verify map[string]any
	for _, j := range view.Jobs {
		switch j["id"] {
		case "j-deploy-release":
			release = j
		case "j-verify":
			verify = j
		}
	}
	if release == nil || release["state"] != "success" {
		t.Fatalf("callee job = %v (jobs: %v)", release, view.Jobs)
	}
	if verify == nil || verify["state"] != "success" {
		t.Fatalf("dependent job = %v (jobs: %v)", verify, view.Jobs)
	}
	log := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-deploy-release/log", callerLane, run.ID), chainAliceTok)
	if !strings.Contains(log, "target=production") {
		t.Fatalf("the pinned callee did not run with the supplied input:\n%s", log)
	}
	if !strings.Contains(log, "secret=present") || strings.Contains(log, secretVal) {
		t.Fatalf("explicit secret handling is wrong (value leaked or missing):\n%s", log)
	}
	// Permission downgrade: the callee asked for contents: write; the caller
	// capped it at read, and the expanded job carries the capped map.
	spec, _ := release["spec"].(map[string]any)
	perms, _ := spec["permissions"].(map[string]any)
	if perms["contents"] != "read" {
		t.Fatalf("callee permissions = %v, want contents: read (downgrade-only)", perms)
	}
	// The upstream repository's own workflow never started (only the caller
	// pushed); the callee is embedded in the caller's run.
	pollUntil(t, 5*time.Second, "no upstream run", func() bool {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, upLane+"/threads?n=10", &page)
		return len(page.Runs) == 0
	})
}
