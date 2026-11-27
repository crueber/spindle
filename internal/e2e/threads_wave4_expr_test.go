// threads_wave4_expr_test.go — Wave 4 end to end (docs/threads.md §3.3, §3.6,
// §3.7; docs/WORKFLOWS.md §5.2/§5.3): a real runner executes `hashFiles` in the
// job container, a six-cell matrix with `max-parallel: 2` never has three cells
// running, a job `if:` reads a real repository secret, and `run-name` reaches the
// run record.
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

const wave4ExprWorkflow = `name: Wave4
run-name: Wave 4 ${{ github.event_name }} on ${{ github.ref_name }}
on: [push]
jobs:
  gated:
    runs-on: linux
    if: secrets.DEPLOY_KEY != ''
    steps:
      - run: echo gated-ok
  no_secret:
    runs-on: linux
    if: secrets.DEPLOY_KEY == ''
    steps:
      - run: echo should-not-run
  hash:
    runs-on: linux
    steps:
      - name: make files
        run: |
          printf 'module example.com/wave4\n' > go.sum
          mkdir -p sub
          printf 'dep\n' > sub/go.sum
      - name: hash twice
        run: |
          A="${{ hashFiles('**/go.sum') }}"
          B="${{ hashFiles('**/go.sum') }}"
          C="${{ hashFiles('no-such-dir/*') }}"
          test -n "$A"
          test "$A" = "$B"
          test -z "$C"
          echo "hash-ok $A"
  json:
    runs-on: linux
    steps:
      - name: round trip
        if: fromJSON(toJSON(github)).event_name == 'push'
        run: |
          test '${{ toJSON(fromJSON('{"a":[1,2]}')) }}' = '{"a":[1,2]}'
          echo json-ok
      - name: json gate off
        if: fromJSON(toJSON(github)).event_name == 'pull_request'
        run: exit 1
  bounded:
    needs: [hash]
    runs-on: linux
    strategy:
      max-parallel: 2
      matrix:
        v: [1, 2, 3, 4, 5, 6]
    steps:
      - run: sleep 2; echo "cell ${{ matrix.v }}"
`

func TestE2E_ThreadsWave4ExpressionsAndFlow(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsw4")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	if st, body := apiCall(t, "PUT", repoLane+"/threads/secrets/DEPLOY_KEY", `{"value":"wave4-secret"}`, chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("set secret: %d %s", st, body)
	}

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/wave4.yml", wave4ExprWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 4)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})

	// While the bounded matrix runs, sample the run view: at most two cells of
	// the group may be `running` at once (PAR-052).
	maxBoundedRunning := 0
	var run threadRun
	deadline := time.Now().Add(240 * time.Second)
	for {
		run = getRun(t, repoLane, runID, chainAliceTok)
		running := 0
		for _, job := range run.Jobs {
			key, _ := job["key"].(string)
			if strings.HasPrefix(key, "bounded") && job["state"] == "running" {
				running++
			}
		}
		if running > maxBoundedRunning {
			maxBoundedRunning = running
		}
		if state, _ := run.Run["state"].(string); state == "success" || state == "failure" || state == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in 4m\njobs: %v\nrunner:\n%s", runID, run.Jobs, runner.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("run = %v, want success\njobs: %v\nhash log:\n%s\nrunner:\n%s",
			state, run.Jobs,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-hash/log", repoLane, runID), chainAliceTok),
			runner.logs.String())
	}
	if maxBoundedRunning > 2 {
		t.Fatalf("observed %d bounded cells running at once with max-parallel: 2", maxBoundedRunning)
	}
	if maxBoundedRunning == 0 {
		t.Fatalf("never observed a bounded cell running\njobs: %v", run.Jobs)
	}

	// The job `if:` read the real secret: gated ran, its complement was skipped.
	if st := jobState(run, "j-gated"); st == nil || st["state"] != "success" {
		t.Fatalf("j-gated = %v", st)
	}
	if st := jobState(run, "j-no_secret"); st == nil || st["state"] != "skipped" {
		t.Fatalf("j-no-secret = %v; jobs: %v", st, run.Jobs)
	}
	// hashFiles produced a deterministic non-empty value and an empty no-match.
	hashLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-hash/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(hashLog, "hash-ok ") {
		t.Fatalf("hash log misses the deterministic check:\n%s", hashLog)
	}
	// toJSON/fromJSON evaluate in a real step `if:` and a real `run:` span; the
	// false branch would have exited 1, so the job's success proves the gate.
	if st := jobState(run, "j-json"); st == nil || st["state"] != "success" {
		t.Fatalf("j-json = %v\njobs: %v", st, run.Jobs)
	}
	jsonLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-json/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(jsonLog, "json-ok") {
		t.Fatalf("json log misses the round-trip check:\n%s", jsonLog)
	}
	// run-name is on the record (and therefore in the list and the API).
	if title, _ := run.Run["title"].(string); title != "Wave 4 push on main" {
		t.Fatalf("run title = %q", title)
	}
	// The run LIST renders the same title (not only the API).
	listPage := httpGetAuthed(t, s.base+"/"+owner+"/"+repo+"/threads", chainAliceTok)
	if !strings.Contains(listPage, "Wave 4 push on main") {
		t.Fatalf("run list does not render the run title")
	}
}
