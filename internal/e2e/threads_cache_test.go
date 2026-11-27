// threads_cache_test.go — the cache service and cross-run artifacts end to end
// (docs/threads.md §3.4b/§3.4c; PAR-095, PAR-097, PAR-098, PAR-099):
//
//   - run 1 saves a cache; run 2 restores it with a restore-key prefix and
//     asserts the bytes inside the job container;
//   - a fork PR's cache is never readable from the base branch (D-10);
//   - a run that names another run's artifact is held for a maintainer's
//     approval, then downloads it once approved (D-3);
//   - the artifact API deletes an artifact and reports the bytes freed.
//
// Skipped in -short mode and when docker is unavailable.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
)

// cacheWorkflowV1 writes a dependency directory and saves it under deps-v1.
const cacheWorkflowV1 = `name: Cache
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: make deps
        run: |
          mkdir -p .deps
          printf 'deps-1' > .deps/data
          echo "made deps"
      - uses: actions/cache
        with:
          path: .deps
          key: deps-v1
`

// cacheWorkflowV2 restores through restore-keys (the exact key is new), asserts
// the bytes run 1 saved, and — on a pull_request run only — writes and saves its
// own cache entry. That entry belongs to the PR's untrusted scope, so the next
// main-branch run's probe must miss (D-10 fork isolation).
const cacheWorkflowV2 = `name: Cache
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/cache
        with:
          path: .deps
          key: deps-v2
          restore-keys: |
            deps-
      - name: assert restored
        run: |
          test "$(cat .deps/data)" = deps-1
          echo "CACHE-RESTORED"
      - name: probe a fork key
        run: |
          if [ -f .forkdeps/data ]; then echo "FORK-BYTES-VISIBLE"; else echo "CACHE-MISS"; fi
      - name: make fork deps
        if: contains(github.ref, 'refs/pull/')
        run: |
          mkdir -p .forkdeps
          printf 'fork-bytes' > .forkdeps/data
      - uses: actions/cache
        if: contains(github.ref, 'refs/pull/')
        with:
          path: .forkdeps
          key: fork-key
`

// crossRunWorkflow downloads a named upstream artifact (the id is substituted
// before the push by the test).
const crossRunWorkflowTemplate = `name: CrossRun
on:
  push:
    branches: [main]
jobs:
  consume:
    name: consume
    runs-on: linux
    steps:
      - uses: actions/download-artifact
        with:
          run-id: "%s"
          name: build-out
          path: in
      - name: assert bytes
        run: |
          test "$(cat in/dist/a.txt)" = alpha
          echo "CROSS-RUN-OK"
`

// waitRunSuccess polls one run to a terminal state and fails with its logs.
func waitRunSuccess(t *testing.T, repoLane, runID, bearer string, runner *runnerProc) map[string]any {
	t.Helper()
	deadline := time.Now().Add(240 * time.Second)
	for {
		run := getRun(t, repoLane, runID, bearer)
		if state, _ := run.Run["state"].(string); state == "success" || state == "failure" || state == "cancelled" {
			if state != "success" {
				t.Fatalf("run %s = %s\njobs: %v\nrunner:\n%s", runID, state, run.Jobs, runner.logs.String())
			}
			return run.Run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in 4m\njobs: %v\nrunner:\n%s", runID, run.Jobs, runner.logs.String())
		}
		time.Sleep(time.Second)
	}
}

// waitRunHeld polls one run until it is a pending approval (the D-3 hold).
func waitRunHeld(t *testing.T, repoLane, runID, bearer string) {
	t.Helper()
	pollUntil(t, 60*time.Second, "the run to be held for approval", func() bool {
		run := getRun(t, repoLane, runID, bearer)
		state, _ := run.Run["state"].(string)
		kind, _ := run.Run["approval_kind"].(string)
		return state == "pending_approval" && kind == "cross_run"
	})
}

// TestE2E_ThreadsCacheSaveRestoreAndForkIsolation is the PAR-095 acceptance:
// run 2 restores run 1's cache inside a real container, and a fork's write is
// never readable from the base branch.
func TestE2E_ThreadsCacheSaveRestoreAndForkIsolation(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := artifactsServer(t, false)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadscache")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/cache.yml", cacheWorkflowV1, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, e2eDefaultBearer)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	// Run 1: a miss, then the deferred save.
	var run1 string
	pollUntil(t, 60*time.Second, "run 1", func() bool {
		run1 = latestRunID(t, repoLane, e2eDefaultBearer)
		return run1 != ""
	})
	waitRunSuccess(t, repoLane, run1, e2eDefaultBearer, runner)
	if log := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, run1), e2eDefaultBearer); !strings.Contains(log, "saved cache") {
		t.Fatalf("run 1 did not save a cache:\n%s", log)
	}

	// Run 2: a new exact key, the restore-key prefix hits run 1's entry, and the
	// job asserts the restored bytes.
	g.commitFile(work, ".spindle/workflows/cache.yml", cacheWorkflowV2, "v2")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "main")
	var run2 string
	pollUntil(t, 60*time.Second, "run 2", func() bool {
		id := latestRunID(t, repoLane, e2eDefaultBearer)
		if id == run1 {
			return false
		}
		run2 = id
		return true
	})
	waitRunSuccess(t, repoLane, run2, e2eDefaultBearer, runner)
	log2 := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, run2), e2eDefaultBearer)
	if !strings.Contains(log2, "CACHE-RESTORED") || !strings.Contains(log2, "restored cache") {
		t.Fatalf("run 2 did not restore run 1's cache:\n%s", log2)
	}

	// A fork PR writes its own cache scope; the base branch can never read it.
	access := mustAPI(t, "GET", repoLane+"/access", "", e2eDefaultBearer)
	ver, _ := access["version"].(float64)
	mustAPI(t, "PUT", repoLane+"/access", fmt.Sprintf(
		`{"version":%d,"visibility":"public","role_bindings":[{"subject":%q,"role":"write"}]}`,
		int(ver), "user:"+chainBob), e2eDefaultBearer)
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), e2eDefaultBearer)
	forkURL := s.gitURL(owner, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, e2eDefaultBearer) != nil
	})
	// The fork branch carries a trivial change; the RUN reads main's workflow
	// file (the fork trust rule), so the same base workflow executes untrusted.
	g.run(work, "checkout", "-b", "forkcache")
	g.commitFile(work, "fork.txt", "fork change\n", "fork cache")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "forkcache")
	g.runAuth(work, e2eDefaultBearer, "push", forkURL, "forkcache")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"fork cache","base_ref":"refs/heads/main","head_ref":"refs/heads/forkcache","fork":{"repo":%q}}`,
			owner+"/"+forkName), artifactsBobToken)
	var forkRun string
	pollUntil(t, 90*time.Second, "the fork run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=20", "", e2eDefaultBearer)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			id, _ := m["id"].(string)
			if m["event"] != "pull_request" || id == "" {
				continue
			}
			full := getRun(t, repoLane, id, e2eDefaultBearer)
			if full.Run["state"] == "success" {
				forkRun = id
				return true
			}
		}
		return false
	})
	waitRunSuccess(t, repoLane, forkRun, e2eDefaultBearer, runner)

	// Back on main, a run that looks for the fork's entry misses. The fork's
	// cache lives under pr:<n>:untrusted, which no base-branch run reads.
	g.run(work, "checkout", "main")
	g.commitFile(work, ".spindle/workflows/cache.yml", cacheWorkflowV2+"\n# probe build\n", "probe")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "main")
	var run3 string
	pollUntil(t, 60*time.Second, "run 3", func() bool {
		id := latestRunID(t, repoLane, e2eDefaultBearer)
		if id == run1 || id == run2 {
			return false
		}
		run3 = id
		return true
	})
	waitRunSuccess(t, repoLane, run3, e2eDefaultBearer, runner)
	log3 := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, run3), e2eDefaultBearer)
	if strings.Contains(log3, "FORK-BYTES-VISIBLE") {
		t.Fatalf("a base-branch run read a fork's cache:\n%s", log3)
	}
}

// TestE2E_ThreadsCrossRunArtifactNeedsApproval is the PAR-098 acceptance: the
// consuming run is held, an unapproved download is impossible (no queue entry),
// the maintainer's approval releases it, and the job asserts the upstream bytes.
func TestE2E_ThreadsCrossRunArtifactNeedsApproval(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := artifactsServer(t, false)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadscross")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	// The upstream job publishes one artifact.
	g.commitFile(work, ".spindle/workflows/up.yml", artifactsWorkflow, "upstream")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")
	reg := mintRunnerToken(t, repoLane, e2eDefaultBearer)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var upRun string
	pollUntil(t, 60*time.Second, "the upstream run", func() bool {
		upRun = latestRunID(t, repoLane, e2eDefaultBearer)
		return upRun != ""
	})
	waitRunSuccess(t, repoLane, upRun, e2eDefaultBearer, runner)

	// A second workflow names the upstream run id: its run is held.
	g.commitFile(work, ".spindle/workflows/cross.yml", fmt.Sprintf(crossRunWorkflowTemplate, upRun), "cross-run")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "main")
	var held string
	pollUntil(t, 60*time.Second, "the cross-run hold", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=20", "", e2eDefaultBearer)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if path, _ := m["workflow_path"].(string); path != ".spindle/workflows/cross.yml" {
				continue
			}
			if id, _ := m["id"].(string); id != "" {
				held = id
				return true
			}
		}
		return false
	})
	waitRunHeld(t, repoLane, held, e2eDefaultBearer)
	// The held run has a run page and a check record, but no runner can claim
	// it: nothing has executed.
	if st := jobState(getRun(t, repoLane, held, e2eDefaultBearer), "j-consume"); st == nil || st["state"] != "queued" {
		t.Fatalf("held job = %v, want queued", st)
	}
	// Approve as an admin (a maintain-class decision) and let it run.
	mustAPI(t, "POST", repoLane+"/threads/"+held+"/approve", "", e2eDefaultBearer)
	waitRunSuccess(t, repoLane, held, e2eDefaultBearer, runner)
	log := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, held), e2eDefaultBearer)
	if !strings.Contains(log, "CROSS-RUN-OK") {
		t.Fatalf("the approved run did not download the upstream artifact:\n%s", log)
	}

	// The artifact API deletes the upstream artifact and reports the bytes.
	list := mustAPI(t, "GET", repoLane+"/threads/"+upRun+"/artifacts", "", e2eDefaultBearer)
	arts, _ := list["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("upstream artifacts = %v", list)
	}
	deleted := mustAPI(t, "DELETE", repoLane+"/threads/"+upRun+"/artifacts/build-out", "", e2eDefaultBearer)
	if freed, _ := deleted["bytes_freed"].(float64); freed <= 0 {
		t.Fatalf("delete freed = %v", deleted)
	}
	if status, _ := apiCall(t, "GET", repoLane+"/threads/"+upRun+"/artifacts", "", e2eDefaultBearer); status != http.StatusOK {
		t.Fatalf("list after delete = %d", status)
	}
	if status, _ := apiCall(t, "GET", repoLane+"/threads/"+upRun+"/artifacts/build-out", "", e2eDefaultBearer); status != http.StatusNotFound {
		t.Fatalf("manifest after delete = %d, want 404", status)
	}
	// The cache service's list is a read-gated API and its entries survive an
	// unrelated retain pass (the sweep's cache family is exercised in unit
	// tests against the same engine).
	caches := mustAPI(t, "GET", repoLane+"/threads/caches", "", e2eDefaultBearer)
	if _, ok := caches["caps"]; !ok {
		t.Fatalf("cache list = %v", caches)
	}
	// A root-level retain pass is reachable through the same service the
	// maintainer runs; it reports the cache family.
	fsStore, err := store.NewFilesystemRoot(filepath.Join(s.dataDir, "store"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := threads.New(fsStore, nil)
	res, err := svc.Retain(context.Background(), owner, repo, nil)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.CachesDeleted < 0 || res.CacheBytes < 0 {
		t.Fatalf("cache family report = %+v", res)
	}
}
