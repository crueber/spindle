// threads_wave3_test.go — the Wave 3 GitHub-only triggers end to end:
// repository_dispatch → run, workflow_run → approval for an untrusted parent,
// and the fork-run write-token policy. No runner and no docker are needed:
// every assertion is about run creation, the payload/approval state, and the
// token decision recorded on the run header.
package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// dispatchWorkflowE2E declares the API-dispatched trigger.
const dispatchWorkflowE2E = `name: Dispatch
on:
  repository_dispatch:
    types: [deploy]
jobs:
  deploy:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.client_payload.environment }}"
`

// followUpWorkflowE2E follows the Build workflow's completed edge.
const followUpWorkflowE2E = `name: Follow
on:
  workflow_run:
    workflows: [Build]
    types: [completed]
jobs:
  consume:
    runs-on: linux
    steps:
      - run: echo follow
`

// failingBuildWorkflowE2E fails at creation (an undefined environment fails a
// job closed), so the upstream run folds terminal without a runner and its
// `completed` edge is emitted for the follow-up.
const failingBuildWorkflowE2E = `name: Build
on: [push, pull_request]
jobs:
  build:
    runs-on: linux
    environment: missing-environment
    steps:
      - run: echo build
`

// forkWorkflowE2E asks for write; without the repository switch the fork run
// gets a read-only token.
const forkWorkflowE2E = `name: Fork
on: pull_request
permissions:
  contents: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

func TestE2ERepositoryDispatchCreatesRun(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("disp")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/dispatch.yml", dispatchWorkflowE2E, "workflow")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// The write role is the gate; the admin token has it.
	res := mustAPI(t, "POST", lane+"/threads/dispatches",
		`{"event_type":"deploy","client_payload":{"environment":"prod"}}`, chainAliceTok)
	runs, ok := res["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("dispatch answer = %v", res)
	}
	row, _ := runs[0].(map[string]any)
	id, _ := row["id"].(string)
	view := mustAPI(t, "GET", lane+"/threads/"+id, "", chainAliceTok)
	run, _ := view["run"].(map[string]any)
	if run["event"] != "repository_dispatch" {
		t.Fatalf("run event = %v", run["event"])
	}
	payload, _ := run["event_payload"].(map[string]any)
	cp, _ := payload["client_payload"].(map[string]any)
	if cp["environment"] != "prod" {
		t.Fatalf("client_payload = %v", payload["client_payload"])
	}

	// A token without the write role is refused (anonymous has none).
	if st, _ := apiCall(t, "POST", lane+"/threads/dispatches",
		`{"event_type":"deploy"}`, ""); st != 401 && st != 403 {
		t.Fatalf("anonymous dispatch status = %d", st)
	}
}

func TestE2EWorkflowRunUntrustedParentNeedsApproval(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("wfr")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	repoLane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/build.yml", failingBuildWorkflowE2E, "build")
	g.commitFile(work, ".spindle/workflows/follow.yml", followUpWorkflowE2E, "follow")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// A fork and a cross-fork PR: its pull_request run reads the base file and
	// is untrusted.
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	forkLane := s.base + "/" + owner + "/" + forkName + "/api"
	pollUntil(t, 60*time.Second, "fork to become servable", func() bool {
		st, _ := apiCall(t, "GET", forkLane, "", chainAliceTok)
		return st == 200
	})
	g.run(work, "checkout", "-b", "prbranch")
	g.commitFile(work, "pr.txt", "one\n", "pr head")
	g.runAuth(work, chainAliceTok, "push", "origin", "prbranch")
	g.runAuth(work, chainAliceTok, "push", s.gitURL(owner, forkName), "prbranch")
	pollUntil(t, 60*time.Second, "fork branch visible", func() bool {
		refs := g.lsRemoteAuthOK(s.gitURL(owner, forkName), chainAliceTok)
		if refs == nil {
			return false
		}
		_, ok := refs["refs/heads/prbranch"]
		return ok
	})
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"Fork PR","base_ref":"refs/heads/main","head_ref":"refs/heads/prbranch","fork":{"repo":%q}}`, owner+"/"+forkName),
		chainAliceTok)

	// The follow-up held for approval is the assertion: it appears on the
	// workflow_run `completed` edge of the untrusted pull_request run.
	var held string
	pollUntil(t, 30*time.Second, "held workflow_run follow-up", func() bool {
		var page struct {
			Runs []struct {
				ID           string `json:"id"`
				Event        string `json:"event"`
				State        string `json:"state"`
				ApprovalKind string `json:"approval_kind"`
			} `json:"runs"`
		}
		getJSON(t, lane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event == "workflow_run" && r.State == "pending_approval" && r.ApprovalKind == "workflow_run" {
				held = r.ID
				return true
			}
		}
		return false
	})
	// A maintainer approves it; the run queues (nothing claims it without a
	// runner, which is the honest idle state).
	mustAPI(t, "POST", lane+"/threads/"+held+"/approve", "", chainAliceTok)
	view := mustAPI(t, "GET", lane+"/threads/"+held, "", chainAliceTok)
	run, _ := view["run"].(map[string]any)
	if run["state"] != "queued" {
		t.Fatalf("approved follow-up state = %v", run["state"])
	}
}

func TestE2EForkRunWriteTokenPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("fwt")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/fork.yml", forkWorkflowE2E, "workflow")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	forkLane := s.base + "/" + owner + "/" + forkName + "/api"
	pollUntil(t, 60*time.Second, "fork to become servable", func() bool {
		st, _ := apiCall(t, "GET", forkLane, "", chainAliceTok)
		return st == 200
	})
	g.run(work, "checkout", "-b", "prbranch")
	_ = g.commitFile(work, "pr.txt", "one\n", "pr head")
	g.runAuth(work, chainAliceTok, "push", "origin", "prbranch")
	g.runAuth(work, chainAliceTok, "push", s.gitURL(owner, forkName), "prbranch")
	pollUntil(t, 60*time.Second, "fork branch visible", func() bool {
		refs := g.lsRemoteAuthOK(s.gitURL(owner, forkName), chainAliceTok)
		if refs == nil {
			return false
		}
		_, ok := refs["refs/heads/prbranch"]
		return ok
	})
	mustAPI(t, "POST", lane+"/pulls",
		fmt.Sprintf(`{"title":"Fork token","base_ref":"refs/heads/main","head_ref":"refs/heads/prbranch","fork":{"repo":%q}}`, owner+"/"+forkName),
		chainAliceTok)
	first := waitNewRun(t, lane, "pull_request", nil)
	if first.Trusted {
		t.Fatalf("fork run = %+v", first)
	}
	// write_tokens is false by default.
	if got := runWriteTokens(t, lane, first.ID); got {
		t.Fatal("send_write_tokens is off by default")
	}

	// A maintainer flips the switch; the next run records it.
	mustAPI(t, "PUT", lane+"/threads/settings", `{"send_write_tokens":true}`, chainAliceTok)
	// A second cross-fork PR (a new head branch) creates a new opened edge; a
	// fork head push alone does not wake the synchronize path in this harness.
	g.run(work, "checkout", "-q", "-b", "prbranch2")
	head2 := g.commitFile(work, "pr2.txt", "two\n", "second pr head")
	g.runAuth(work, chainAliceTok, "push", "origin", "prbranch2")
	g.runAuth(work, chainAliceTok, "push", s.gitURL(owner, forkName), "prbranch2")
	pollUntil(t, 60*time.Second, "second fork branch visible", func() bool {
		refs := g.lsRemoteAuthOK(s.gitURL(owner, forkName), chainAliceTok)
		if refs == nil {
			return false
		}
		_, ok := refs["refs/heads/prbranch2"]
		return ok
	})
	seen := map[string]bool{first.ID: true}
	mustAPI(t, "POST", lane+"/pulls",
		fmt.Sprintf(`{"title":"Fork token 2","base_ref":"refs/heads/main","head_ref":"refs/heads/prbranch2","fork":{"repo":%q}}`, owner+"/"+forkName),
		chainAliceTok)
	second := waitNewRun(t, lane, "pull_request", seen)
	if !runWriteTokens(t, lane, second.ID) {
		t.Fatal("the enabled switch must be recorded on the fork run")
	}
	_ = head2
}

// waitNewRun waits for a run of event whose id is not in seen (nil = any) and
// returns it. The PR merge-ref change means a pull_request run's commit is the
// merge commit, not the head, so the wait cannot key on the head sha.
func waitNewRun(t *testing.T, lane, event string, seen map[string]bool) threadRunSummary {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, lane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event != event || (seen != nil && seen[r.ID]) {
				continue
			}
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new %s run appeared", event)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runWriteTokens reads the run header's write_tokens flag.
func runWriteTokens(t *testing.T, lane, id string) bool {
	t.Helper()
	view := mustAPI(t, "GET", lane+"/threads/"+id, "", chainAliceTok)
	run, _ := view["run"].(map[string]any)
	if run == nil {
		t.Fatalf("run %s missing", id)
	}
	flag, _ := run["write_tokens"].(bool)
	return flag
}
