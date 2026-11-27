// threads_runner_test.go — pass B2 end to end (docs/features/15_threads.md §4/§6):
// a real runner subprocess against a live spindle server, executing real docker
// containers, with the checks surface and the merge gate observing the result.
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable.
package e2e

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// basicAuth renders git's Basic credential for a user:password pair.
func basicAuth(cred string) string {
	return base64.StdEncoding.EncodeToString([]byte(cred))
}

// threadsServer boots the binary with an OIDC config (the in-test IdP), a
// secrets key and an anonymous-read public default, on a throwaway data dir.
// An optional public URL sets server.public_url (the checkout base job
// containers use).
func threadsServer(t *testing.T, publicURL ...string) *e2eServer {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	pub := ""
	if len(publicURL) > 0 && publicURL[0] != "" {
		pub = fmt.Sprintf("public_url = %q\n", publicURL[0])
	}
	p := newOIDP(t)
	chainAliceTok = p.idToken(chainAlice, "spindle-admin")
	e2eDefaultBearer = chainAliceTok
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
listen = "0.0.0.0:%d"
%sauto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

%s

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, port, pub, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)
	return s
}

// requireDocker skips the scenario when no daemon answers.
func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("docker e2e: skipped in -short mode")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// startThreadsRunner boots `spindle runner serve` as a subprocess and returns a
// stop function (SIGTERM, which the runner handles by cancelling in-flight jobs,
// cleaning containers and reporting them cancelled). The harness runner is a
// disposable test host, so it opts in to fork runs: the fork suites exercise
// execution, while the default-off gating is pinned by the threads unit tests.
func startThreadsRunner(t *testing.T, bin, serverURL, registerToken, labels string, capacity int) *runnerProc {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "runner.token")
	cmd := exec.Command(bin, "runner", "serve",
		"--server", serverURL,
		"--register", registerToken,
		"--token-file", tokenFile,
		"--labels", labels,
		"--capacity", fmt.Sprint(capacity),
		"--allow-host-docker",
		"--allow-fork-runs",
		"--image", "alpine/git:latest",
		"--fetch-timeout", "5")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	rp := &runnerProc{cmd: cmd, logs: logs, t: t}
	t.Cleanup(rp.stop)
	return rp
}

type runnerProc struct {
	cmd  *exec.Cmd
	logs *syncBuffer
	t    *testing.T
}

func (r *runnerProc) stop() {
	if r.cmd.Process == nil {
		return
	}
	_ = r.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = r.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = r.cmd.Process.Kill()
		<-done
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForConsole waits (bounded) for the runner console to carry marker. The
// runner logs "job complete" only after its completion write returns, so a run
// record that already reads success does not yet imply the console line exists;
// a single read races the runner process (v0.11.1's tag gate flaked on exactly
// that). The marker itself stays a hard requirement: pollUntil fails the test
// when it never arrives, so a lost line is a failure, not a longer wait.
func waitForConsole(t *testing.T, runner *runnerProc, marker string) {
	t.Helper()
	pollUntil(t, 30*time.Second, fmt.Sprintf("the runner console to carry %q", marker), func() bool {
		return strings.Contains(runner.logs.String(), marker)
	})
}

// mintRunnerToken mints an instance-scoped registration token through the
// host-admin API (repoLane is kept for call-site readability; the route is
// instance-scoped).
func mintRunnerToken(t *testing.T, repoLane, token string) string {
	t.Helper()
	u, err := url.Parse(repoLane)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Fatalf("repo lane %q is not an absolute URL", repoLane)
	}
	origin := u.Scheme + "://" + u.Host
	res := mustAPI(t, "POST", origin+"/api/v1/admin/runners/tokens", `{"allow_all":true}`, token)
	tok, _ := res["token"].(string)
	if !strings.HasPrefix(tok, "wrg_") {
		t.Fatalf("registration token answer = %v", res)
	}
	return tok
}

// threadRun is one run joined with its jobs and published artifacts (the API's
// shape).
type threadRun struct {
	Run       map[string]any
	Jobs      []map[string]any
	Artifacts []map[string]any
}

// getRun fetches one run view.
func getRun(t *testing.T, repoLane, runID, token string) threadRun {
	t.Helper()
	res := mustAPI(t, "GET", repoLane+"/threads/"+runID, "", token)
	run, _ := res["run"].(map[string]any)
	jobs, _ := res["jobs"].([]any)
	out := threadRun{Run: run}
	for _, j := range jobs {
		if m, ok := j.(map[string]any); ok {
			out.Jobs = append(out.Jobs, m)
		}
	}
	if arts, ok := res["artifacts"].([]any); ok {
		for _, a := range arts {
			if m, ok := a.(map[string]any); ok {
				out.Artifacts = append(out.Artifacts, m)
			}
		}
	}
	return out
}

// latestRunID returns the newest run id.
func latestRunID(t *testing.T, repoLane, token string) string {
	t.Helper()
	res := mustAPI(t, "GET", repoLane+"/threads?n=1", "", token)
	runs, _ := res["runs"].([]any)
	if len(runs) == 0 {
		return ""
	}
	m, _ := runs[0].(map[string]any)
	id, _ := m["id"].(string)
	return id
}

// jobState finds one job by id in a run view.
func jobState(run threadRun, id string) map[string]any {
	for _, j := range run.Jobs {
		if j["id"] == id {
			return j
		}
	}
	return nil
}

// sseLog attaches to a job's log stream and returns the concatenated text
// frames it sees (replay + tail), ending at the terminal `done` event.
func sseLog(t *testing.T, url, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("log request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("log stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		t.Fatalf("log stream status %d: %s", resp.StatusCode, body)
	}
	var text strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if event == "log" {
				var frame struct {
					Text string `json:"text"`
				}
				if json.Unmarshal([]byte(data), &frame) == nil {
					text.WriteString(frame.Text)
				}
			}
			if event == "done" {
				return text.String()
			}
		}
	}
	return text.String()
}

// twoJobWorkflow is the acceptance shape: lint (no needs) and test (needs lint),
// both in the default alpine/git image, both checking out the run's commit. The
// lint step also proves `${{ … }}` substitution in a real container: every
// reference it reads is compared with the environment value it must equal
// (docs/threads.md §3.6).
const twoJobWorkflow = `name: CI
on: [push, pull_request]
jobs:
  lint:
    name: lint
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: lint ${{ github.ref_name }}
        env:
          LINT_MARKER: lint-marker
        run: |
          echo "linting $GITHUB_SHA"
          test -f .spindle/workflows/ci.yml && echo "checkout ok"
          test "${{ github.ref }}" = "$GITHUB_REF" && echo "ref substituted"
          test "${{ github.sha }}" = "$GITHUB_SHA" && echo "sha substituted"
          test "${{ github.repository }}" = "$GITHUB_REPOSITORY" && echo "repository substituted"
          test "${{ env.LINT_MARKER }}" = "lint-marker" && echo "env substituted"
          test -n "${{ github.actor }}" && echo "actor substituted"
          test "${{ github.server_url }}" = "$GITHUB_SERVER_URL" && echo "server url substituted"
  test:
    name: test
    runs-on: linux
    needs: [lint]
    steps:
      - uses: actions/checkout
      - run: echo "tests pass $MATRIX_NOTE"
`

// TestE2E_ThreadsRunnerExecutesJobs is the pass-B2 acceptance: a real runner
// executes a two-job workflow in real containers, the checks go in_progress →
// success, and the SSE log attach shows the step output.
func TestE2E_ThreadsRunnerExecutesJobs(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threads")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/ci.yml", twoJobWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	// in_progress: the claim mirrors pending + started_at (05's enum has no
	// separate in_progress), which is what a merge gate treats as not-green.
	seenPending := false
	pollUntil(t, 120*time.Second, "checks to go green", func() bool {
		run := getRun(t, repoLane, runID, chainAliceTok)
		if run.Run["state"] == "running" {
			seenPending = true
		}
		return run.Run["state"] == "success"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if !seenPending {
		t.Log("run completed before a running sample was observed (short jobs)")
	}
	lint := jobState(run, "j-lint")
	test := jobState(run, "j-test")
	if lint == nil || test == nil {
		t.Fatalf("run jobs = %v", run.Jobs)
	}
	if lint["state"] != "success" || test["state"] != "success" {
		t.Fatalf("job states = %v / %v", lint["state"], test["state"])
	}
	steps, _ := lint["step_results"].([]any)
	if len(steps) != 2 {
		t.Fatalf("lint step results = %v", steps)
	}
	first, _ := steps[0].(map[string]any)
	if first["conclusion"] != "success" {
		t.Fatalf("checkout step = %v", first)
	}

	// Checks mirror: both contexts success.
	combined := mustAPI(t, "GET", repoLane+"/checks/"+g.run(work, "rev-parse", "HEAD"), "", chainAliceTok)
	if got := jget(t, combined, "state"); got != "success" {
		t.Fatalf("combined checks = %v, want success (%v)", got, combined)
	}
	statuses, _ := combined["statuses"].([]any)
	contexts := map[string]string{}
	for _, st := range statuses {
		m, _ := st.(map[string]any)
		ctx, _ := m["context"].(string)
		state, _ := m["state"].(string)
		contexts[ctx] = state
	}
	if contexts["threads/ci/lint"] != "success" || contexts["threads/ci/test"] != "success" {
		t.Fatalf("check contexts = %v", contexts)
	}

	// Logs over SSE: the checkout + lint output is attached, in order.
	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-lint/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{
		"checked out", "linting", "checkout ok",
		"ref substituted", "sha substituted", "repository substituted",
		"env substituted", "actor substituted", "server url substituted",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log stream misses %q:\n%s", want, logText)
		}
	}
	waitForConsole(t, runner, "job complete")
}

// TestE2E_ThreadsFailingJobBlocksTheMerge proves the gate: a failing step yields
// a failure check, and a require_checks policy blocks the merge task.
func TestE2E_ThreadsFailingJobBlocksTheMerge(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsfail")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const failing = `name: CI
on: [push, pull_request]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: boom
        run: |
          echo "about to fail"
          exit 3
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/ci.yml", failing, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "the failing run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	pollUntil(t, 120*time.Second, "the failing run to finish", func() bool {
		return getRun(t, repoLane, runID, chainAliceTok).Run["state"] == "failure"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	build := jobState(run, "j-build")
	if build == nil || build["state"] != "failure" {
		t.Fatalf("build job = %v", build)
	}
	steps, _ := build["step_results"].([]any)
	if len(steps) < 2 {
		t.Fatalf("step results = %v", steps)
	}
	boom, _ := steps[1].(map[string]any)
	if boom["conclusion"] != "failure" || boom["exit_code"] != float64(3) {
		t.Fatalf("failing step = %v, want failure exit 3", boom)
	}

	// The merge gate: require the failing context, then a PR merge must not land.
	mustAPI(t, "PUT", repoLane+"/policy",
		`{"version":1,"rules":[{"name":"main-checks","match":{"refs":["refs/heads/main"]},`+
			`"effect":{"protect":{"restricts":["force-push"],"require_checks":["threads/ci/build"]}}}]}`,
		chainAliceTok)
	g.run(work, "checkout", "-b", "feature")
	headSHA := g.commitFile(work, "feature.txt", "change\n", "feature")
	g.runAuth(work, chainAliceTok, "push", "origin", "feature")
	pr := mustAPI(t, "POST", repoLane+"/pulls",
		`{"title":"failing checks","base_ref":"refs/heads/main","head_ref":"refs/heads/feature"}`, chainAliceTok)
	prNum := int(jget(t, pr, "thread", "num").(float64))
	_ = prNum
	// The feature push queued its own run; wait for its check to conclude
	// (the same failing workflow runs there too).
	pollUntil(t, 120*time.Second, "the feature run check to conclude", func() bool {
		combined := mustAPI(t, "GET", repoLane+"/checks/"+headSHA, "", chainAliceTok)
		return combined["state"] == "failure"
	})
	st, _ := apiCall(t, "POST", fmt.Sprintf("%s/pulls/%d/merge", repoLane, prNum), `{"strategy":"merge"}`, chainAliceTok)
	if st < 200 || st > 299 {
		t.Fatalf("POST merge = %d, want 202 (the gate fails the task, not the start)", st)
	}
	state := ""
	pollUntil(t, 90*time.Second, "the blocked merge task", func() bool {
		st, data := apiCall(t, "GET", fmt.Sprintf("%s/pulls/%d/merge/task", repoLane, prNum), "", chainAliceTok)
		if st != http.StatusOK {
			return true
		}
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return false
		}
		task, _ := v["task"].(map[string]any)
		state, _ = task["state"].(string)
		return state != "" && state != "running"
	})
	if state != "error" {
		t.Fatalf("merge task state = %q, want error (require_checks blocked on a real failing job)", state)
	}
	prView := mustAPI(t, "GET", fmt.Sprintf("%s/pulls/%d", repoLane, prNum), "", chainAliceTok)
	if doc, _ := prView["pr"].(map[string]any); doc != nil {
		if merged, _ := doc["merged"].(bool); merged {
			t.Fatal("PR merged with a failing required check")
		}
	}
}

// TestE2E_ThreadsForkPRSafety pins the three fork guarantees of 15 §5.2 against
// a live server: the base branch's workflow runs, no secret reaches the job, and
// the checkout credential cannot push. It then executes the fork job for real.
func TestE2E_ThreadsForkPRSafety(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsfork")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	const baseWorkflow = `name: CI
on: [pull_request]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: base marker
        run: |
          echo "workflow-from-base"
          echo "secret is [${DEPLOY_SECRET:-}]"
`
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	baseSHA := g.commitFile(work, ".spindle/workflows/ci.yml", baseWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// A repository secret that must never reach an untrusted run.
	if st, body := apiCall(t, "PUT", repoLane+"/threads/secrets/DEPLOY_SECRET", `{"value":"fork-must-not-see-this"}`, chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("put secret = %d: %s", st, body)
	}

	// Fork the repository and push a branch that a cross-fork PR can name.
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	forkURL := s.gitURL(owner, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, chainAliceTok) != nil
	})
	g.run(work, "checkout", "-b", "forkfeat")
	g.commitFile(work, "fork.txt", "from the fork\n", "fork change")
	// Reachability: the base repo must hold the head objects (the pulls
	// fork→base bridge does this in production; the e2e pushes it too).
	g.runAuth(work, chainAliceTok, "push", "origin", "forkfeat")
	g.runAuth(work, chainAliceTok, "push", forkURL, "forkfeat")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"fork PR","base_ref":"refs/heads/main","head_ref":"refs/heads/forkfeat","fork":{"repo":%q}}`,
			owner+"/"+forkName), chainAliceTok)
	// A group-only host admin is not resolvable from the bare actor name the
	// event carries (PLAN-OIDC-ONLY §1.3), so the fork run is held and this
	// admin approves it explicitly.
	approveForkRun := func(skip string) string {
		t.Helper()
		var id string
		pollUntil(t, 60*time.Second, "the held fork run", func() bool {
			res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
			runs, _ := res["runs"].([]any)
			for _, r := range runs {
				m, _ := r.(map[string]any)
				if m["event"] != "pull_request" || m["id"] == skip {
					continue
				}
				if m["state"] == "pending_approval" {
					id, _ = m["id"].(string)
					return id != ""
				}
			}
			return false
		})
		mustAPI(t, "POST", repoLane+"/threads/"+id+"/approve", "", chainAliceTok)
		return id
	}
	approveForkRun("")
	// ---- read-only checkout credential: a probe runner claims the job and
	// tries to push with the attempt token. -----------------------------------
	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	probe := mustAPI(t, "POST", s.base+"/api/v1/threads/runners/register",
		fmt.Sprintf(`{"token":%q,"labels":["linux"],"allow_fork_runs":true}`, reg), "")
	probeToken, _ := probe["token"].(string)
	if !strings.HasPrefix(probeToken, "wrt_") {
		t.Fatalf("probe register = %v", probe)
	}
	var forkJob map[string]any
	pollUntil(t, 60*time.Second, "the fork run job", func() bool {
		st, data := apiCall(t, "POST", s.base+"/api/v1/threads/runners/fetch?timeout=5", "", probeToken)
		if st != http.StatusOK {
			return false
		}
		return json.Unmarshal(data, &forkJob) == nil
	})
	if trusted, _ := forkJob["trusted"].(bool); trusted {
		t.Fatal("a fork PR job was marked trusted")
	}
	if denied, _ := forkJob["secrets_denied"].(bool); !denied {
		t.Fatal("a fork PR job was not marked secrets_denied")
	}
	if secrets, _ := forkJob["secrets"].(map[string]any); len(secrets) != 0 {
		t.Fatalf("fork job carries secrets: %v", secrets)
	}
	jobRef, _ := forkJob["job_ref"].(string)
	probeRunID, _ := forkJob["run_id"].(string)
	attempt := int(forkJob["attempt"].(float64))
	start := mustAPI(t, "POST", fmt.Sprintf("%s/api/v1/threads/jobs/%s/attempt/%d/start", s.base, jobRef, attempt), "", probeToken)
	jobToken, _ := start["token"].(string)
	if !strings.HasPrefix(jobToken, "wjt_") {
		t.Fatalf("start = %v", start)
	}
	// git push with the job credential: Basic ci:<wjt_…>. Read-only means 403.
	basic := basicAuth("ci:" + jobToken)
	cmd := exec.Command("git", "-c", "http.extraHeader=Authorization: Basic "+basic, "push", gitURL, "HEAD:refs/heads/ci-push-attempt")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+filepath.Join(g.sandbox, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1", "HOME="+g.sandbox)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a job token pushed to the repository — the checkout credential is not read-only:\n%s", out)
	}
	if refs := g.lsRemoteAuthOK(gitURL, chainAliceTok); refs["refs/heads/ci-push-attempt"] != "" {
		t.Fatal("the rejected push landed a ref")
	}
	// Acknowledge the cancel-free probe claim: the job is terminal so the real
	// runner below starts from a fresh PR run.
	mustAPI(t, "POST", fmt.Sprintf("%s/api/v1/threads/jobs/%s/attempt/%d/cancel", s.base, jobRef, attempt), "", jobToken)

	// ---- execute a fork job for real -----------------------------------------
	runner := startThreadsRunner(t, bin, s.base, mintRunnerToken(t, repoLane, chainAliceTok), "linux", 1)
	// A second PR (a distinct head branch: an open pair is unique per repo).
	g.run(work, "checkout", "-b", "forkfeat2", "main")
	g.commitFile(work, "fork2.txt", "second fork change\n", "fork change 2")
	g.runAuth(work, chainAliceTok, "push", "origin", "forkfeat2")
	g.runAuth(work, chainAliceTok, "push", forkURL, "forkfeat2")
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"fork PR 2","base_ref":"refs/heads/main","head_ref":"refs/heads/forkfeat2","fork":{"repo":%q}}`,
			owner+"/"+forkName), chainAliceTok)
	approveForkRun(probeRunID)

	var runID string
	pollUntil(t, 90*time.Second, "the fork run", func() bool {
		res := mustAPI(t, "GET", repoLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] != "pull_request" || m["id"] == probeRunID {
				continue
			}
			if trusted, _ := m["trusted"].(bool); trusted {
				continue
			}
			if m["state"] == "success" || m["state"] == "failure" {
				runID, _ = m["id"].(string)
				return runID != ""
			}
		}
		return false
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if trusted, _ := run.Run["trusted"].(bool); trusted {
		t.Fatal("fork run trusted")
	}
	if run.Run["workflow_commit"] != baseSHA {
		t.Fatalf("fork run workflow_commit = %v, want the base tip %s", run.Run["workflow_commit"], baseSHA)
	}
	if run.Run["state"] != "success" {
		t.Fatalf("fork run state = %v (%v)", run.Run["state"], run.Jobs)
	}
	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(logText, "workflow-from-base") {
		t.Fatalf("fork run did not execute the base workflow:\n%s", logText)
	}
	if !strings.Contains(logText, "secret is []") {
		t.Fatalf("fork job saw a secret value (want an empty variable):\n%s", logText)
	}
	if strings.Contains(logText, "fork-must-not-see-this") {
		t.Fatal("the repository secret leaked into a fork run's log")
	}
	waitForConsole(t, runner, "job complete")
}
