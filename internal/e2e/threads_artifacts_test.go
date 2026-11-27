// threads_artifacts_test.go — run artifacts end to end (docs/features/15 §2.5,
// docs/threads.md §3.4): a real job container uploads files with
// actions/upload-artifact, a dependent job downloads them with
// actions/download-artifact and asserts their content, the run page and the API
// list them, the ZIP download carries the right headers, threads-gc reclaims
// them with the run, and a fork PR run's artifact is never readable from
// another repository.
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable.
package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
)

// artifactsServer boots the binary on a throwaway data dir with an OIDC
// admin principal (the in-test IdP), anonymous reads, and (optionally) the fork
// approval gate. It fails fast when the fixture's config is rejected (a
// restricted boot answers /healthz but every repository route is 503).
func artifactsServer(t *testing.T, approveForks bool) *e2eServer {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	p := newOIDP(t)
	writeSpindleConfig(t, dataDir, artifactsConfig(port, dataDir, approveForks, p))
	// The lane signs in as an admin from the admin group; the second principal
	// exercises the non-admin read path.
	e2eDefaultBearer = p.idToken("alice@example.com", "spindle-admin")
	artifactsBobToken = p.idToken("bob@example.com")
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)
	s.requireConfigured(20 * time.Second)
	return s
}

// artifactsBobToken is the non-admin principal's bearer for this lane.
var artifactsBobToken string

// artifactsConfig renders the test instance's spindle.toml.
func artifactsConfig(port int, dataDir string, approveForks bool, p *oidp) string {
	threadsCfg := "[threads]\n"
	if approveForks {
		threadsCfg += "approve_first_time_forks = true\n"
	} else {
		threadsCfg += "approve_first_time_forks = false\n"
	}
	return fmt.Sprintf(`
[server]
listen = "0.0.0.0:%d"
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

%s
anonymous_read = true

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q

%s
`, port, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache"), threadsCfg)
}

// artifactsWorkflow is the acceptance scenario: build writes two files and
// uploads them; consume needs build, downloads the artifact, and asserts the
// exact bytes. A failure in either half fails the job (and the test).
const artifactsWorkflow = `name: Artifacts
on: [push]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: make files
        run: |
          mkdir -p dist/sub
          printf 'alpha' > dist/a.txt
          printf 'beta' > dist/sub/b.txt
          echo "made two files"
      - name: upload
        uses: actions/upload-artifact
        with:
          name: build-out
          path: dist
  consume:
    name: consume
    runs-on: linux
    needs: [build]
    steps:
      - uses: actions/download-artifact
        with:
          name: build-out
          path: in
      - name: assert contents
        run: |
          test "$(cat in/dist/a.txt)" = alpha
          test "$(cat in/dist/sub/b.txt)" = beta
          echo "artifact-roundtrip-ok"
`

// liveWorkflow is the retention pass's control: a dispatched run whose job asks
// for a label no runner declares, so it stays QUEUED (live) for the whole test.
// The pass must never reclaim it.
const liveWorkflow = `name: Live
on: [workflow_dispatch]
jobs:
  wait:
    name: wait
    runs-on: [never-declared]
    steps:
      - run: echo never
`

// TestE2E_ThreadsArtifactsUploadDownloadAndGC is the artifact acceptance: real
// containers, both directions, the page/API surfaces, the ZIP download, and the
// retention pass reclaiming the run, its blobs and its expiry rows while a LIVE
// run survives.
func TestE2E_ThreadsArtifactsUploadDownloadAndGC(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := artifactsServer(t, false)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsart")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/artifacts.yml", artifactsWorkflow, "ci")
	g.commitFile(work, ".spindle/workflows/live.yml", liveWorkflow, "live fixture")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, e2eDefaultBearer)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, e2eDefaultBearer)
		return runID != ""
	})
	var run threadRun
	deadline := time.Now().Add(180 * time.Second)
	for {
		run = getRun(t, repoLane, runID, e2eDefaultBearer)
		if state, _ := run.Run["state"].(string); state == "success" || state == "failure" || state == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in 3m\njobs: %v\nrunner:\n%s", runID, run.Jobs, runner.logs.String())
		}
		time.Sleep(time.Second)
	}
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("run = %v, want success\njobs: %v\nbuild log:\n%s\nconsume log:\n%s\nrunner:\n%s", state, run.Jobs,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), e2eDefaultBearer),
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, runID), e2eDefaultBearer),
			runner.logs.String())
	}
	if st := jobState(run, "j-consume"); st == nil || st["state"] != "success" {
		t.Fatalf("consume job = %v (artifact round trip failed)\n%s", st, runner.logs.String())
	}
	// The dependent job's log proves the downloaded bytes, not just a green
	// step (the assertion script echoed `artifact-roundtrip-ok`).
	if logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, runID), e2eDefaultBearer); !strings.Contains(logText, "artifact-roundtrip-ok") {
		t.Fatalf("consume log lacks the content assertion:\n%s", logText)
	}
	if logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), e2eDefaultBearer); !strings.Contains(logText, `uploaded artifact "build-out": 2 file(s)`) {
		t.Fatalf("build log lacks the upload summary:\n%s", logText)
	}

	// The API list is the manifest read model.
	list := mustAPI(t, "GET", repoLane+"/threads/"+runID+"/artifacts", "", e2eDefaultBearer)
	arts, _ := list["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("artifact list = %v", list)
	}
	art, _ := arts[0].(map[string]any)
	if art["name"] != "build-out" || art["total_size"] != float64(9) || art["files"].([]any) == nil {
		t.Fatalf("artifact record = %v", art)
	}
	if files, _ := art["files"].([]any); len(files) != 2 {
		t.Fatalf("artifact files = %v", files)
	}
	// The run detail carries the same artifacts.
	if got := getRun(t, repoLane, runID, e2eDefaultBearer); len(got.Artifacts) != 1 {
		t.Fatalf("run view artifacts = %v", got.Artifacts)
	}

	// The run page lists the artifact with its download link.
	page := httpGetAuthed(t, s.base+"/"+owner+"/"+repo+"/threads/"+runID, e2eDefaultBearer)
	for _, want := range []string{"Artifacts", "build-out", "/artifacts/build-out"} {
		if !strings.Contains(page, want) {
			t.Fatalf("run page misses %q", want)
		}
	}

	// The ZIP download: right headers, right entries and bytes.
	resp := httpDo(t, "GET", s.base+"/"+owner+"/"+repo+"/threads/"+runID+"/artifacts/build-out", e2eDefaultBearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "build-out.zip") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	got := map[string]string{}
	for _, entry := range zr.File {
		rc, oerr := entry.Open()
		if oerr != nil {
			t.Fatalf("zip entry %s: %v", entry.Name, oerr)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[entry.Name] = string(b)
	}
	if got["dist/a.txt"] != "alpha" || got["dist/sub/b.txt"] != "beta" {
		t.Fatalf("zip entries = %v", got)
	}

	// A second upload of the same name is refused (names are unique per run):
	// the runner reports it, the run that published first is untouched.
	// (Exercised at the unit/API level; here we prove the committed artifact
	// survived every read above.)

	// A live control run: dispatched with a label no runner declares, so it
	// stays QUEUED for the whole test. The retention pass must leave it alone.
	// The collection POST is the dispatch API (POST …/api/threads).
	dispatched := mustAPI(t, "POST", repoLane+"/threads",
		`{"workflow":".spindle/workflows/live.yml","ref":"main"}`, e2eDefaultBearer)
	liveRuns, _ := dispatched["runs"].([]any)
	if len(liveRuns) != 1 {
		t.Fatalf("dispatch = %v", dispatched)
	}
	liveRunID, _ := liveRuns[0].(map[string]any)["id"].(string)
	if liveRunID == "" {
		t.Fatalf("dispatch run id = %v", liveRuns[0])
	}
	if live := getRun(t, repoLane, liveRunID, e2eDefaultBearer); live.Run["state"] != "queued" {
		t.Fatalf("control run = %v, want queued (its label has no runner)", live.Run["state"])
	}

	// Retention, DRIVEN rather than waited for. The maintainer's retention
	// sweeper (cmd/spindle/threads_sweep.go retentionSweep) calls exactly
	// svc.Retain; the schedule itself is asserted by the composition test
	// TestThreadsGCSweeperAppliesRetention, so this test does not wait for a
	// tick. The pass runs against the SAME on-disk store the live server reads,
	// with the window elapsed on the service's own clock.
	storeRoot := filepath.Join(s.dataDir, "store")
	fsStore, err := store.NewFilesystemRoot(storeRoot, 4)
	if err != nil {
		t.Fatalf("open the server's store: %v", err)
	}
	svc := threads.New(fsStore, nil)
	svc.Retention = 30 * 24 * time.Hour // the documented default window
	svc.Now = func() time.Time { return time.Now().UTC().Add(31 * 24 * time.Hour) }
	res, err := svc.Retain(context.Background(), owner, repo, nil)
	if err != nil {
		t.Fatalf("retention pass: %v", err)
	}
	if res.RunsDeleted != 1 || res.ArtifactsDeleted != 1 || res.ArtifactBytes <= 0 || res.QueueDeleted < 1 {
		t.Fatalf("retention = %+v, want the terminal run, its artifact, its bytes and its queue entry", res)
	}

	// The running server reads the same store: the run and its artifact are
	// gone through every surface.
	if status, _ := apiCall(t, "GET", repoLane+"/threads/"+runID+"/artifacts", "", e2eDefaultBearer); status != http.StatusNotFound {
		t.Fatalf("artifact API after retention = %d, want 404", status)
	}
	if resp := httpDo(t, "GET", s.base+"/"+owner+"/"+repo+"/threads/"+runID+"/artifacts/build-out", e2eDefaultBearer); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("download after retention = %d, want 404", resp.StatusCode)
	}
	if status, _ := apiCall(t, "GET", repoLane+"/threads/"+runID, "", e2eDefaultBearer); status != http.StatusNotFound {
		t.Fatalf("run view after retention = %d, want 404", status)
	}

	// The live run is untouched: still queued, its job still queued.
	live := getRun(t, repoLane, liveRunID, e2eDefaultBearer)
	if live.Run["state"] != "queued" {
		t.Fatalf("the live run was reclaimed: %v", live.Run["state"])
	}
	if st := jobState(live, "j-wait"); st == nil || st["state"] != "queued" {
		t.Fatalf("the live run's job = %v, want queued", st)
	}

	// The bytes and the expiry rows are really gone (not only the read model):
	// every artifact object under the reclaimed run and every expiry index row.
	if left := storeFiles(storeRoot, "/threads/runs/"+runID+"/artifacts/"); len(left) != 0 {
		t.Fatalf("%d artifact objects survived retention: %v", len(left), left)
	}
	if rows := storeFiles(storeRoot, "/threads/artifact-expiry/"); len(rows) != 0 {
		t.Fatalf("expiry rows survived retention: %v", rows)
	}
}

// TestE2E_ThreadsForkArtifactStaysInTheRepo proves the fork trust rule for
// artifacts: a fork PR run may publish (it is untrusted, so it gets no secrets),
// the base repository's readers can list and download it, and the fork
// repository's lane can never read it.
func TestE2E_ThreadsForkArtifactStaysInTheRepo(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := artifactsServer(t, false)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsforkart")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const forkWorkflow = `name: ForkArt
on: [pull_request]
jobs:
  publish:
    name: publish
    runs-on: linux
    steps:
      - name: make a file
        run: |
          mkdir -p out
          printf 'fork-bytes' > out/f.txt
      - uses: actions/upload-artifact
        with:
          name: fork-build
          path: out
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/forkart.yml", forkWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	access := mustAPI(t, "GET", repoLane+"/access", "", e2eDefaultBearer)
	ver, _ := access["version"].(float64)
	mustAPI(t, "PUT", repoLane+"/access", fmt.Sprintf(
		`{"version":%d,"visibility":"public","role_bindings":[{"subject":%q,"role":"write"}]}`,
		int(ver), "user:"+chainBob), e2eDefaultBearer)

	reg := mintRunnerToken(t, repoLane, e2eDefaultBearer)
	startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), e2eDefaultBearer)
	forkURL := s.gitURL(owner, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, e2eDefaultBearer) != nil
	})
	g.run(work, "checkout", "-b", "forkart")
	g.commitFile(work, "fork.txt", "from the fork\n", "fork change")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "forkart")
	g.runAuth(work, e2eDefaultBearer, "push", forkURL, "forkart")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"fork artifact","base_ref":"refs/heads/main","head_ref":"refs/heads/forkart","fork":{"repo":%q}}`,
			owner+"/"+forkName), artifactsBobToken)

	var runID string
	pollUntil(t, 60*time.Second, "the fork run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", e2eDefaultBearer)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] != "pull_request" {
				continue
			}
			id, _ := m["id"].(string)
			run := getRun(t, repoLane, id, e2eDefaultBearer)
			if run.Run["state"] == "success" {
				runID = id
				return true
			}
		}
		return false
	})
	run := getRun(t, repoLane, runID, e2eDefaultBearer)
	if trusted, _ := run.Run["trusted"].(bool); trusted {
		t.Fatalf("a fork PR run is trusted: %v", run.Run)
	}
	if len(run.Artifacts) != 1 || run.Artifacts[0]["name"] != "fork-build" {
		t.Fatalf("fork run artifacts = %v", run.Artifacts)
	}
	// The base repository's reader can download it.
	resp := httpDo(t, "GET", s.base+"/"+owner+"/"+repo+"/threads/"+runID+"/artifacts/fork-build", e2eDefaultBearer)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("fork-bytes")) {
		t.Fatalf("base-repo download = %d", resp.StatusCode)
	}
	// The fork's lane is a different repository: the run id is unknown there, so
	// the artifact route is a 404 and never a cross-repo read.
	forkLane := s.base + "/" + owner + "/" + forkName
	if status, _ := apiCall(t, "GET", forkLane+"/api/threads/"+runID+"/artifacts", "", e2eDefaultBearer); status != http.StatusNotFound {
		t.Fatalf("fork lane list = %d, want 404", status)
	}
	if resp := httpDo(t, "GET", forkLane+"/threads/"+runID+"/artifacts/fork-build", e2eDefaultBearer); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("fork lane download = %d, want 404", resp.StatusCode)
	}
	_ = gitURL
}

// httpDo issues one authenticated request.
func httpDo(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// httpGetAuthed returns one authenticated page's body.
func httpGetAuthed(t *testing.T, url, token string) string {
	t.Helper()
	resp := httpDo(t, "GET", url, token)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}
	return string(body)
}

// storeFiles lists filesystem-store objects whose path contains every marker
// (a check-status path such as checks/<sha>/threads/artifacts/<name>.json also
// contains "/artifacts/", so the run family is named explicitly).
func storeFiles(root string, markers ...string) []string {
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		slashed := filepath.ToSlash(path)
		for _, m := range markers {
			if !strings.Contains(slashed, m) {
				return nil
			}
		}
		out = append(out, path)
		return nil
	})
	return out
}
