// threads_approval_concurrency_test.go — wave-2 parity end to end
// (PAR-131/135/152): an environment hold, a reviewer's approval, a supersede
// that cancels a HELD run, a manual cancel of a held run, and the concurrency
// group's one-running/one-pending behaviour with a live runner.
//
// Skipped in -short mode and when docker is unavailable (the run executes in a
// container).
package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const approvalDeployWorkflow = `name: Deploy
on: [push]
jobs:
  ship:
    runs-on: linux
    environment: production
    steps:
      - run: echo "deploying $GITHUB_SHA"
`

const concGroupWorkflow = `name: Serial
on: [push]
concurrency:
  group: deploy-main
  cancel-in-progress: false
jobs:
  ship:
    runs-on: linux
    steps:
      - run: echo start; sleep 4; echo done
`

// TestE2E_ThreadsApprovalSupersedeAndCancel pins the interaction between an
// environment hold and the cancellation paths:
//
//   - a held run is approved by a listed reviewer and executes;
//   - a newer push supersedes a HELD run (it reads cancelled/supersede and can
//     no longer be approved: 409);
//   - a held run can be cancelled by an operator (also 409 afterwards);
//   - a concurrency group lets one run hold and one wait, and the waiting run
//     starts after the holder finishes (pending_group names the group).
func TestE2E_ThreadsApprovalSupersedeAndCancel(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := gateServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsprot")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	// The repository is created WITHOUT the deploy workflow: the environment
	// record must exist before the first run is created (an undefined
	// environment fails the deployment job closed), so the environment is
	// configured between two pushes.
	g.commitFile(work, "README.md", "prot\\n", "init")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// Bob may write (open PRs, be a reviewer) but is not a maintainer.
	access := mustAPI(t, "GET", repoLane+"/access", "", chainAliceTok)
	ver, _ := access["version"].(float64)
	mustAPI(t, "PUT", repoLane+"/access", fmt.Sprintf(
		`{"version":%d,"visibility":"public","role_bindings":[{"subject":%q,"role":"write"}]}`,
		int(ver), "user:bob"), chainAliceTok)
	// The environment holds for bob's approval (a direct reviewer; no timer).
	// The reviewer is the PRINCIPAL name (the OIDC subject's local part), not
	// the email the role binding uses: holdsForEnvironment compares p.Name.
	mustAPI(t, "PUT", repoLane+"/threads/environments/production",
		`{"required_reviewers":["bob"]}`, chainAliceTok)

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	// Now the workflow exists: this push is the first HELD run.
	g.commitFile(work, ".spindle/workflows/ci.yml", approvalDeployWorkflow, "ci")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")

	// newestPending returns the id of the newest pending_approval run ("" when
	// there is none yet).
	newestPending := func() string {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m := r.(map[string]any)
			if m["state"] == "pending_approval" {
				id, _ := m["id"].(string)
				return id
			}
		}
		return ""
	}
	waitRun := func(runID string) threadRun {
		t.Helper()
		pollUntil(t, 120*time.Second, "run "+runID, func() bool {
			st := getRun(t, repoLane, runID, chainAliceTok).Run["state"]
			return st == "success" || st == "failure" || st == "cancelled"
		})
		return getRun(t, repoLane, runID, chainAliceTok)
	}

	// 1. A held run is approved by the listed reviewer and executes.
	var heldID string
	pollUntil(t, 60*time.Second, "the first held run", func() bool {
		heldID = newestPending()
		return heldID != ""
	})
	held := getRun(t, repoLane, heldID, chainAliceTok)
	if held.Run["approval_kind"] != "environment" {
		t.Fatalf("held run kind = %v", held.Run["approval_kind"])
	}
	// A non-reviewer (alice is an admin, so use bob's absence from the record
	// instead) — the environment names bob, and bob's approval releases it.
	mustAPI(t, "POST", repoLane+"/threads/"+heldID+"/approve", "", chainBobTok)
	if final := waitRun(heldID); final.Run["state"] != "success" {
		t.Fatalf("approved run = %v", final.Run["state"])
	}

	// 2. A newer push supersedes a HELD run; the superseded hold cannot be
	// approved afterwards.
	g.commitFile(work, "step-2.txt", "two\n", "second")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")
	var supersededID string
	pollUntil(t, 60*time.Second, "the second held run", func() bool {
		supersededID = newestPending()
		return supersededID != "" && supersededID != heldID
	})
	g.commitFile(work, "step-3.txt", "three\n", "third")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")
	pollUntil(t, 60*time.Second, "the superseded hold to cancel", func() bool {
		return getRun(t, repoLane, supersededID, chainAliceTok).Run["state"] == "cancelled"
	})
	old := getRun(t, repoLane, supersededID, chainAliceTok)
	if old.Run["cancel_reason"] != "supersede" || old.Run["superseded_by"] == "" {
		t.Fatalf("superseded hold = %v / %v", old.Run["cancel_reason"], old.Run["superseded_by"])
	}
	if st, _ := apiCall(t, "POST", repoLane+"/threads/"+supersededID+"/approve", "", chainBobTok); st != http.StatusConflict {
		t.Fatalf("approving a superseded hold = %d, want 409", st)
	}

	// 3. A held run can be cancelled by an operator, and is then undecidable.
	var cancellableID string
	pollUntil(t, 60*time.Second, "the third held run", func() bool {
		cancellableID = newestPending()
		return cancellableID != "" && cancellableID != supersededID && cancellableID != heldID
	})
	mustAPI(t, "POST", repoLane+"/threads/"+cancellableID+"/cancel", "", chainAliceTok)
	cancelled := getRun(t, repoLane, cancellableID, chainAliceTok)
	if cancelled.Run["state"] != "cancelled" {
		t.Fatalf("cancelled hold = %v", cancelled.Run["state"])
	}
	if st, _ := apiCall(t, "POST", repoLane+"/threads/"+cancellableID+"/approve", "", chainBobTok); st != http.StatusConflict {
		t.Fatalf("approving a cancelled hold = %d, want 409", st)
	}

	// 4. Concurrency: one run holds the group and the next one waits in the
	// pending slot, then starts when the holder finishes.
	g.commitFile(work, ".spindle/workflows/ci.yml", concGroupWorkflow, "serial workflow")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")
	var holderID string
	pollUntil(t, 60*time.Second, "the holder run", func() bool {
		holderID = latestRunID(t, repoLane, chainAliceTok)
		if holderID == "" {
			return false
		}
		return getRun(t, repoLane, holderID, chainAliceTok).Run["state"] == "running"
	})
	g.commitFile(work, "step-4.txt", "four\n", "fourth")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")
	var waiterID string
	pollUntil(t, 60*time.Second, "the pending run", func() bool {
		waiterID = latestRunID(t, repoLane, chainAliceTok)
		if waiterID == "" || waiterID == holderID {
			return false
		}
		r := getRun(t, repoLane, waiterID, chainAliceTok).Run
		return r["state"] == "queued" && r["pending_group"] == "deploy-main"
	})
	if final := waitRun(holderID); final.Run["state"] != "success" {
		t.Fatalf("holder run = %v", final.Run["state"])
	}
	if final := waitRun(waiterID); final.Run["state"] != "success" {
		t.Fatalf("pending run after the holder = %v", final.Run["state"])
	}
	// The waiting run's header no longer claims to be waiting once it ran.
	if pg, _ := getRun(t, repoLane, waiterID, chainAliceTok).Run["pending_group"].(string); strings.TrimSpace(pg) != "" {
		t.Fatalf("a finished run still names pending_group %q", pg)
	}
}
