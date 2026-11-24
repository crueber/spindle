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
	ID      string `json:"id"`
	Commit  string `json:"commit"`
	Event   string `json:"event"`
	Branch  string `json:"branch"`
	State   string `json:"state"`
	Trusted bool   `json:"trusted"`
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
		if r.Event == event && r.Commit == commit {
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
