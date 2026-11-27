// threads_triggers_test.go — the round-2 PR head-push defect, end to end
// (docs/features/15_threads.md §2.1/§2.3): a push to an open PR's head must
// produce its `pull_request` synchronize run within seconds — not after the
// events sweep — and the PR head must only ever move forward.
//
// The server under test is configured with a ONE HOUR sweep interval, so the
// only way a run can appear is the push→bridge wake-up. No runner and no
// docker are needed: the assertion is run creation (the executor is covered by
// threads_runner_test.go).
package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
) // triggerServer boots the binary with a sweep interval nothing in this test can
// wait for, so any prompt delivery must come from the push wake-up.
func triggerServer(t *testing.T) *e2eServer {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	p := newOIDP(t)
	chainAliceTok = p.idToken(chainAlice, "spindle-admin")
	e2eDefaultBearer = chainAliceTok
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

%s
[events]
sweep_interval = "1h"

[threads]
# The Wave 3 scenarios exercise the triggers, not the first-time fork hold;
# keeping the gate off makes the fork run execute (and fail/fold) immediately.
approve_first_time_forks = false

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)
	return s
}

// triggerWorkflow fires on push and on every pull_request edge.
const triggerWorkflow = `name: Triggers
on:
  push:
  pull_request:
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo "run ${{ github.event_name }}"
`

// threadRunRow is the subset of the run list this test reads.
type threadRunSummary struct {
	ID             string         `json:"id"`
	Commit         string         `json:"commit"`
	Event          string         `json:"event"`
	Branch         string         `json:"branch"`
	Ref            string         `json:"ref"`
	Workflow       string         `json:"workflow"`
	WorkflowPath   string         `json:"workflow_path"`
	WorkflowCommit string         `json:"workflow_commit"`
	State          string         `json:"state"`
	Trusted        bool           `json:"trusted"`
	Actor          string         `json:"actor"`
	EventPayload   map[string]any `json:"event_payload"`
}

// eventHeadSHA is the head commit the event names. A pull_request run reports
// the server-computed merge commit as `commit` (PAR-024), so the PR head lives
// in the payload; every other event reports it directly.
func eventHeadSHA(r threadRunSummary) string {
	if pr, ok := r.EventPayload["pull_request"].(map[string]any); ok {
		if head, ok := pr["head"].(map[string]any); ok {
			if sha, ok := head["sha"].(string); ok && sha != "" {
				return sha
			}
		}
	}
	return r.Commit
}

// runsFor polls GET …/api/threads and returns the runs matching event/commit
// (newest first), plus the number of matching runs found after the deadline.
func runsFor(t *testing.T, lane, event, commit string) []threadRunSummary {
	t.Helper()
	var page struct {
		Runs []threadRunSummary `json:"runs"`
	}
	getJSON(t, lane+"/threads?n=200", &page)
	var out []threadRunSummary
	for _, r := range page.Runs {
		if r.Event == event && eventHeadSHA(r) == commit {
			out = append(out, r)
		}
	}
	return out
}

// waitRun polls for a run of (event, commit) and returns how long it took.
func waitRun(t *testing.T, lane, event, commit string, budget time.Duration) (time.Duration, []threadRunSummary) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(budget)
	for {
		if got := runsFor(t, lane, event, commit); len(got) > 0 {
			return time.Since(start), got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s run for %s within %v (a push never woke the events bridge)", event, commit, budget)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2E_PRHeadPushTriggersImmediately is the round-2 blocker's regression:
// a head push on an open PR creates its `pull_request` synchronize run without
// waiting for the sweep, records the new head, and a replayed/duplicate
// delivery neither rewinds the head nor creates a second run for the old sha.
func TestE2E_PRHeadPushTriggersImmediately(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("trig")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/triggers.yml", triggerWorkflow, "workflow")
	g.commitFile(work, "app.txt", "v1\n", "seed")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	g.run(work, "checkout", "-b", "topic")
	head1 := g.commitFile(work, "topic.txt", "one\n", "topic one")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")

	prRes := mustAPI(t, "POST", lane+"/pulls",
		`{"title":"Head push PR","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","body":"trigger audit"}`,
		chainAliceTok)
	prNum := int(jget(t, prRes, "thread", "num").(float64))

	// The opened edge: one run on the opened head.
	if dur, _ := waitRun(t, lane, "pull_request", head1, 10*time.Second); dur > 5*time.Second {
		t.Logf("opened run appeared after %v", dur.Round(time.Millisecond))
	}

	// Push a second commit to the SAME PR head — the synchronize edge.
	head2 := g.commitFile(work, "topic.txt", "two\n", "topic two")
	pushStart := time.Now()
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")

	latency, runs := waitRun(t, lane, "pull_request", head2, 10*time.Second)
	if latency > 5*time.Second {
		t.Fatalf("synchronize run took %v (sweep is 1h; the push must wake the bridge)", latency)
	}
	t.Logf("synchronize run for %s in %v (push took %v)", head2[:10],
		latency.Round(time.Millisecond), time.Since(pushStart).Round(time.Millisecond))
	if len(runs) != 1 {
		t.Fatalf("synchronize runs = %d, want exactly 1", len(runs))
	}
	if got := runsFor(t, lane, "push", head2); len(got) != 1 {
		t.Fatalf("push runs for the new head = %d, want 1", len(got))
	}

	// The PR head is the new commit (forward-only reconciliation).
	prView := mustAPI(t, "GET", fmt.Sprintf("%s/pulls/%d", lane, prNum), "", chainAliceTok)
	if pr, ok := prView["pr"].(map[string]any); ok {
		if head, _ := pr["head"].(map[string]any); head != nil {
			if sha, _ := head["sha"].(string); sha != head2 {
				t.Fatalf("PR head = %s, want %s", sha, head2)
			}
		}
	}
	// A duplicate delivery (the same push observed twice, e.g. a restart
	// replay) must not create a second run for the old sha: explicitly ask the
	// bridge to re-run its catch-up by POSTing the notify route.
	postNotify(t, s, owner+"/"+repo)
	time.Sleep(300 * time.Millisecond)
	if got := runsFor(t, lane, "pull_request", head1); len(got) != 1 {
		t.Fatalf("pull_request runs for the OLD head = %d, want 1 (no replay run)", len(got))
	}
	if got := runsFor(t, lane, "pull_request", head2); len(got) != 1 {
		t.Fatalf("pull_request runs for the head = %d after replay, want 1", len(got))
	}
}

// waitRunAction polls for a pull_request run whose payload action matches and
// whose id is not notID.
func waitRunAction(t *testing.T, lane, action, notID string) threadRunSummary {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, lane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event != "pull_request" || r.ID == notID {
				continue
			}
			if got, _ := r.EventPayload["action"].(string); got == action {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pull_request %s run appeared", action)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// metadataWorkflowE2E fires on every pull_request edge. Its second job only
// queues when the expression context carries the merge ref and a commit that
// is not the PR head, so a metadata delivery proves the same context a
// lifecycle run has.
const metadataWorkflowE2E = `name: Metadata
on:
  pull_request:
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
  merge-only:
    if: contains(github.ref, '/merge') && github.sha != github.event.pull_request.head.sha
    runs-on: linux
    steps:
      - run: echo "${{ github.ref }} ${{ github.sha }}"
`

// assertMergeRefRun pins the merge-ref contract (PAR-024) for one run: the ref
// and commit are the merge pair, the payload names the PR head and the merge
// sha, and the workflow file is read from wantWorkflowCommit (the head for a
// same-repository PR, the base tip for a fork).
func assertMergeRefRun(t *testing.T, r threadRunSummary, prNum int, head, wantWorkflowCommit string) {
	t.Helper()
	wantRef := fmt.Sprintf("refs/pull/%d/merge", prNum)
	if r.Ref != wantRef {
		t.Fatalf("run ref = %q, want %q (run %+v)", r.Ref, wantRef, r)
	}
	if r.Commit == "" || r.Commit == head {
		t.Fatalf("run commit = %q, want the merge commit (head %s)", r.Commit, head)
	}
	if r.WorkflowCommit != wantWorkflowCommit {
		t.Fatalf("workflow_commit = %q, want %q", r.WorkflowCommit, wantWorkflowCommit)
	}
	pr, _ := r.EventPayload["pull_request"].(map[string]any)
	if pr == nil {
		t.Fatalf("pull_request payload missing: %+v", r.EventPayload)
	}
	if got, _ := pr["merge_commit_sha"].(string); got != r.Commit {
		t.Fatalf("payload merge_commit_sha = %q, run commit = %q", got, r.Commit)
	}
	prHead, _ := pr["head"].(map[string]any)
	if got, _ := prHead["sha"].(string); got != head {
		t.Fatalf("payload head sha = %q, want %q", got, head)
	}
}

// assertMergeOnlyJobQueued reads the run view and requires the merge-only job to
// be queued (not skipped): its job-level `if:` evaluated github.ref and
// github.sha against the merge pair at creation.
func assertMergeOnlyJobQueued(t *testing.T, lane, runID string) {
	t.Helper()
	view := getRun(t, lane, runID, chainAliceTok)
	var found bool
	for _, j := range view.Jobs {
		if j["key"] != "merge-only" {
			continue
		}
		found = true
		if j["state"] != "queued" {
			t.Fatalf("merge-only job state = %v, want queued (%v)", j["state"], j["reason"])
		}
	}
	if !found {
		t.Fatalf("run view has no merge-only job: %+v", view.Jobs)
	}
}

// TestE2EMetadataLabelUsesTheMergeRef is the producer regression: a
// metadata-only pull_request delivery (the issues service's labeled edge) must
// create the same merge-ref run as a lifecycle edge. Before the fix the run
// reported the head commit and refs/pull/<n>/head, because the producer did not
// pass the PR number to CreateRun.
func TestE2EMetadataLabelUsesTheMergeRef(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("prmeta")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/metadata.yml", metadataWorkflowE2E, "workflow")
	g.commitFile(work, "app.txt", "v1\n", "seed")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	g.run(work, "checkout", "-b", "topic")
	head := g.commitFile(work, "topic.txt", "one\n", "topic")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")

	prRes := mustAPI(t, "POST", lane+"/pulls",
		`{"title":"Metadata merge ref","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","body":"producer regression"}`,
		chainAliceTok)
	prNum := int(jget(t, prRes, "thread", "num").(float64))

	// The lifecycle edge is the baseline: opened already uses the merge pair.
	opened := waitNewRun(t, lane, "pull_request", nil)
	assertMergeRefRun(t, opened, prNum, head, head)
	if opened.WorkflowPath != ".spindle/workflows/metadata.yml" {
		t.Fatalf("opened workflow path = %q", opened.WorkflowPath)
	}

	// The metadata-only delivery: a label write on the same PR.
	mustAPI(t, "POST", lane+"/labels", `{"name":"bug","color":"ff0000"}`, chainAliceTok)
	mustAPI(t, "PUT", fmt.Sprintf("%s/pulls/%d", lane, prNum), `{"labels":["bug"]}`, chainAliceTok)
	labeled := waitRunAction(t, lane, "labeled", opened.ID)
	assertMergeRefRun(t, labeled, prNum, head, head)
	if labeled.WorkflowPath != ".spindle/workflows/metadata.yml" {
		t.Fatalf("labeled workflow path = %q", labeled.WorkflowPath)
	}
	if labeled.Branch != "main" {
		t.Fatalf("labeled run branch = %q, want main", labeled.Branch)
	}
	if labeled.Ref != opened.Ref || labeled.Commit != opened.Commit ||
		labeled.WorkflowCommit != opened.WorkflowCommit || labeled.Trusted != opened.Trusted {
		t.Fatalf("labeled run context differs from opened: labeled %+v opened %+v", labeled, opened)
	}
	assertMergeOnlyJobQueued(t, lane, labeled.ID)
}

// TestE2EForkMetadataLabelKeepsTheBaseWorkflow pins the fork-safety half of the
// same fix: a labeled delivery on a fork PR still reads the BASE tip's workflow
// file, untrusted, and reports the merge pair. The trust class must not move
// just because the run now takes the merge-ref path.
func TestE2EForkMetadataLabelKeepsTheBaseWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("forkmeta")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	base := g.commitFile(work, ".spindle/workflows/forkmeta.yml", metadataWorkflowE2E, "workflow")
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
	head := g.commitFile(work, "pr.txt", "one\n", "pr head")
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
	prRes := mustAPI(t, "POST", lane+"/pulls",
		fmt.Sprintf(`{"title":"Fork metadata","base_ref":"refs/heads/main","head_ref":"refs/heads/prbranch","fork":{"repo":%q}}`, owner+"/"+forkName),
		chainAliceTok)
	prNum := int(jget(t, prRes, "thread", "num").(float64))

	opened := waitNewRun(t, lane, "pull_request", nil)
	if opened.Trusted {
		t.Fatalf("fork opened run must be untrusted: %+v", opened)
	}
	assertMergeRefRun(t, opened, prNum, head, base)

	mustAPI(t, "POST", lane+"/labels", `{"name":"bug","color":"ff0000"}`, chainAliceTok)
	mustAPI(t, "PUT", fmt.Sprintf("%s/pulls/%d", lane, prNum), `{"labels":["bug"]}`, chainAliceTok)
	labeled := waitRunAction(t, lane, "labeled", opened.ID)
	if labeled.Trusted {
		t.Fatalf("fork labeled run must stay untrusted: %+v", labeled)
	}
	assertMergeRefRun(t, labeled, prNum, head, base)
	if labeled.Ref != opened.Ref || labeled.Commit != opened.Commit ||
		labeled.WorkflowCommit != opened.WorkflowCommit {
		t.Fatalf("fork labeled run differs from opened: labeled %+v opened %+v", labeled, opened)
	}
}

// postNotify wakes the events bridge explicitly (the admin defer-route the
// bridge exposes), which replays the cursor range — the duplicate-delivery
// probe above.
func postNotify(t *testing.T, s *e2eServer, repo string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.base+"/_events/notify?repo="+repo, nil)
	if err != nil {
		t.Fatalf("notify request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+chainAliceTok)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notify status = %d", resp.StatusCode)
	}
}
