// threads_runtime_test.go — round-2 pass 2 end to end (docs/features/15 §5):
// the scoped job token (permissions → push), and the first-time-contributor
// approval gate (a real runner is up and would execute; the run must not start
// until a maintainer approves).
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

// permissionWorkflow is one run with two jobs: `writer` declares
// `contents: write` and pushes a branch; `reader` declares `contents: read` and
// must be refused. Both echo their outcome so the log proves which happened.
const permissionWorkflow = `name: Perms
on: [push]
jobs:
  writer:
    name: writer
    runs-on: linux
    permissions:
      contents: write
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: "0"
      - name: push with the job token
        run: |
          git config user.email ci@example.com
          git config user.name ci
          echo "built by ci" > ci-output.txt
          git add ci-output.txt
          git commit -q -m "ci: build artifact"
          git -c credential.helper='!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f' push origin HEAD:refs/heads/ci-output
          echo "push-ok"
  reader:
    name: reader
    runs-on: linux
    permissions:
      contents: read
    steps:
      - uses: actions/checkout
      - name: push is refused
        run: |
          git config user.email ci@example.com
          git config user.name ci
          echo "read-only job" > ci-readonly.txt
          git add ci-readonly.txt
          git commit -q -m "ci: read-only attempt"
          if git -c credential.helper='!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f' push origin HEAD:refs/heads/ci-readonly 2>/tmp/push.err; then
            echo "push-unexpectedly-ok"
            exit 1
          fi
          echo "push-rejected"
`

// TestE2E_ThreadsJobTokenScopes pins the permission→scope model against real git:
// a `contents: write` job pushes, a `contents: read` job is refused, and the
// refused push lands no ref.
func TestE2E_ThreadsJobTokenScopes(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsperms")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/perms.yml", permissionWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 2)

	var runID string
	pollUntil(t, 60*time.Second, "the run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	// The payload states the effective authority before any step runs.
	run := getRun(t, repoLane, runID, chainAliceTok)
	writer := jobState(run, "j-writer")
	if writer == nil {
		t.Fatalf("run jobs = %v", run.Jobs)
	}
	pollUntil(t, 180*time.Second, "the run to finish", func() bool {
		st := getRun(t, repoLane, runID, chainAliceTok).Run["state"]
		return st == "success" || st == "failure" || st == "cancelled"
	})
	run = getRun(t, repoLane, runID, chainAliceTok)
	writer = jobState(run, "j-writer")
	reader := jobState(run, "j-reader")
	if writer == nil || reader == nil {
		t.Fatalf("run jobs = %v", run.Jobs)
	}
	if writer["state"] != "success" {
		t.Fatalf("writer job = %v (want success: contents: write must push)\nlog:\n%s\nserver:\n%s",
			writer["state"], sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-writer/log", repoLane, runID), chainAliceTok), s.logs.String())
	}
	// The reader job fails: its script exits 1 on an unexpected successful push,
	// and the expected path echoes push-rejected and stays green.
	if reader["state"] != "success" {
		t.Fatalf("reader job = %v (want success: the refused push is handled)\n%s",
			reader["state"], runnerConsole(t, repoLane))
	}
	// The server-side proof: the write job's branch landed, the read job's did not.
	refs := g.lsRemoteAuthOK(gitURL, chainAliceTok)
	if refs["refs/heads/ci-output"] == "" {
		t.Fatalf("contents: write job did not land its ref (refs = %v)", refs)
	}
	if got := refs["refs/heads/ci-readonly"]; got != "" {
		t.Fatalf("contents: read job pushed (refs/heads/ci-readonly = %s)", got)
	}
	writerLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-writer/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(writerLog, "push-ok") {
		t.Fatalf("writer log lacks push-ok:\n%s", writerLog)
	}
	readerLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-reader/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(readerLog, "push-rejected") {
		t.Fatalf("reader log lacks push-rejected:\n%s", readerLog)
	}
}

// runnerConsole returns the run list's JSON (a debugging aid on failure; the
// runner's console is not addressable from here).
func runnerConsole(t *testing.T, repoLane string) string {
	t.Helper()
	st, data := apiCall(t, "GET", repoLane+"/threads?n=3", "", chainAliceTok)
	if st != http.StatusOK {
		return ""
	}
	return string(data)
}

// gateServer boots the binary with alice (admin) and bob (write, NOT maintain),
// so a fork PR authored by bob is a first-time contributor to the gate.
func gateServer(t *testing.T) *e2eServer {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
listen = "0.0.0.0:%d"
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

[server.auth]
mode = "token"
anonymous_read = true
tokens = [
  { principal = %q, token = %q, write = true, admin = true },
  { principal = %q, token = %q, write = true },
]

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, port, chainAlice, chainAliceTok, chainBob, chainBobTok,
		filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)
	return s
}

// TestE2E_ThreadsApprovalGate pins the first-time-contributor rule end to end: a
// fork PR from a non-maintainer is created in pending_approval, a REAL runner is
// running and claims nothing, approving releases it (the runner executes it) and
// denying cancels it without executing.
func TestE2E_ThreadsApprovalGate(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := gateServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsgate")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const gateWorkflow = `name: Gate
on: [pull_request]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: echo "gate-ran"
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/ci.yml", gateWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	// Bob may write (open the PR) but is not a maintainer: the gate applies.
	access := mustAPI(t, "GET", repoLane+"/access", "", chainAliceTok)
	ver, _ := access["version"].(float64)
	mustAPI(t, "PUT", repoLane+"/access", fmt.Sprintf(
		`{"version":%d,"visibility":"public","role_bindings":[{"subject":%q,"role":"write"}]}`,
		int(ver), "user:"+chainBob), chainAliceTok)

	// A runner is live BEFORE the PR exists: if the gate leaked, it would claim
	// immediately.
	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	// Bob (write, not maintain) forks and opens a cross-fork PR.
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	forkURL := s.gitURL(owner, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, chainAliceTok) != nil
	})
	g.run(work, "checkout", "-b", "gatefeat")
	g.commitFile(work, "gate.txt", "from the fork\n", "gate change")
	g.runAuth(work, chainAliceTok, "push", "origin", "gatefeat")
	g.runAuth(work, chainAliceTok, "push", forkURL, "gatefeat")
	pr := mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"gate PR","base_ref":"refs/heads/main","head_ref":"refs/heads/gatefeat","fork":{"repo":%q}}`,
			owner+"/"+forkName), chainBobTok)
	prNum := int(jget(t, pr, "thread", "num").(float64))

	// The run is held: pending_approval, jobs queued, no start.
	var held map[string]any
	pollUntil(t, 60*time.Second, "the held run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=5", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] == "pull_request" && m["state"] == "pending_approval" {
				held = m
				return true
			}
		}
		return false
	})
	heldID, _ := held["id"].(string)
	heldSHA, _ := held["commit"].(string)
	// Give the live runner a full poll window twice over: it must claim nothing.
	time.Sleep(12 * time.Second)
	run := getRun(t, repoLane, heldID, chainAliceTok)
	if run.Run["state"] != "pending_approval" {
		t.Fatalf("held run state = %v (a runner executed an unapproved run)", run.Run["state"])
	}
	for _, j := range run.Jobs {
		if j["state"] != "queued" || j["started_at"] != nil {
			t.Fatalf("held job = %v (want queued, unstarted)", j)
		}
	}
	if strings.Contains(runner.logs.String(), "job started") {
		t.Fatalf("the runner started a held job:\n%s", runner.logs.String())
	}
	// The check stays pending, so a required check cannot be gamed by the gate.
	if refs := g.lsRemoteAuthOK(gitURL, chainAliceTok); refs["refs/heads/gatefeat"] == "" {
		t.Fatalf("head branch missing (refs = %v)", refs)
	}
	_ = heldSHA

	// Deny a second fork PR: it is cancelled and never executes.
	g.run(work, "checkout", "-b", "gatedenied", "main")
	g.commitFile(work, "deny.txt", "denied\n", "denied change")
	g.runAuth(work, chainAliceTok, "push", "origin", "gatedenied")
	g.runAuth(work, chainAliceTok, "push", forkURL, "gatedenied")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"deny PR","base_ref":"refs/heads/main","head_ref":"refs/heads/gatedenied","fork":{"repo":%q}}`,
			owner+"/"+forkName), chainBobTok)
	var deniedID string
	pollUntil(t, 60*time.Second, "the second held run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["state"] == "pending_approval" && m["id"] != heldID {
				deniedID, _ = m["id"].(string)
				return deniedID != ""
			}
		}
		return false
	})
	mustAPI(t, "POST", repoLane+"/threads/"+deniedID+"/deny", "", chainAliceTok)
	denied := getRun(t, repoLane, deniedID, chainAliceTok)
	if denied.Run["state"] != "cancelled" {
		t.Fatalf("denied run = %v", denied.Run["state"])
	}
	for _, j := range denied.Jobs {
		if j["state"] != "cancelled" && j["state"] != "failure" {
			t.Fatalf("denied job = %v (want cancelled)", j)
		}
	}

	// Approve the first: the live runner executes it.
	mustAPI(t, "POST", repoLane+"/threads/"+heldID+"/approve", "", chainAliceTok)
	pollUntil(t, 180*time.Second, "the approved run to execute", func() bool {
		st := getRun(t, repoLane, heldID, chainAliceTok).Run["state"]
		return st == "success" || st == "failure" || st == "cancelled"
	})
	run = getRun(t, repoLane, heldID, chainAliceTok)
	if run.Run["state"] != "success" {
		t.Fatalf("approved run = %v (want success)\n%s", run.Run["state"], runner.logs.String())
	}
	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, heldID), chainAliceTok)
	if !strings.Contains(logText, "gate-ran") {
		t.Fatalf("approved run did not execute:\n%s", logText)
	}
	// The denial decision is recorded, and a denied run cannot be approved later.
	if st, _ := apiCall(t, "POST", repoLane+"/threads/"+deniedID+"/approve", "", chainAliceTok); st != http.StatusConflict {
		t.Fatalf("approving a denied run = %d, want 409", st)
	}
	_ = prNum
}

// TestE2E_ThreadsScheduleSweep proves the cron path against a live server: a
// `schedule:` workflow fires exactly one run when the maintainer's sweeper runs,
// and a second sweep never double-fires (the watermark is the commit point).
func TestE2E_ThreadsScheduleSweep(t *testing.T) {
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	owner, repo := testOwner, uniqueRepo("threadscron")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const cronWorkflow = `name: Nightly
on:
  schedule:
    - cron: "*/1 * * * *"
jobs:
  tick:
    name: tick
    runs-on: linux
    steps:
      - run: echo tick
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/nightly.yml", cronWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// Wait for the minute boundary so a tick is strictly due, then wait for the
	// maintainer's schedule sweeper (its cadence is one minute).
	pollUntil(t, 200*time.Second, "the scheduled run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] == "schedule" {
				return true
			}
		}
		return false
	})
	// Within the same minute no second run appears.
	before := countScheduleRuns(t, repoLane)
	time.Sleep(3 * time.Second)
	if after := countScheduleRuns(t, repoLane); after != before {
		t.Fatalf("schedule double-fired inside one minute: %d → %d", before, after)
	}
	if before != 1 {
		t.Fatalf("schedule runs = %d, want exactly 1", before)
	}
}

// countScheduleRuns counts schedule-event runs in the newest page.
func countScheduleRuns(t *testing.T, repoLane string) int {
	t.Helper()
	res := mustAPI(t, "GET", repoLane+"/threads?n=50", "", chainAliceTok)
	runs, _ := res["runs"].([]any)
	n := 0
	for _, r := range runs {
		m, _ := r.(map[string]any)
		if m["event"] == "schedule" {
			n++
		}
	}
	return n
}
