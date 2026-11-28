// merge_queue_test.go — the merge queue end to end against real git (the
// Merge queue card; docs/features/22_merge_queue.md).
//
// The scenarios use the real binary, real refs and the real merge task, but
// no runner: the merge_group run is created by the queue's documented ref
// push, and the required check is reported on the queue head through the
// normal checks API (the same stored view the merge gate reads). That keeps
// the acceptance focused on the queue's contract — order, the temporary ref,
// the gate, ejection, restart recovery and dequeue — rather than Docker.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mergeQueueServer configures the queue scenarios: a one-hour event sweep
// (prompt delivery must come from the publish wake-up), a one-second
// maintenance interval (the restart recovery pass runs promptly), and the
// first-time fork gate off (unrelated here).
func mergeQueueServer(t *testing.T) *e2eServer {
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

[maintenance]
interval = "1s"

[threads]
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

const queueWorkflow = `name: Queue
on:
  push:
  merge_group:
    types: [checks_requested]
jobs:
  checks:
    runs-on: linux
    steps:
      - run: echo "checking ${{ github.event.merge_group.head_sha }}"
`

const queuePolicy = `{"version":1,"rules":[{"name":"queue-ci","match":{"refs":["refs/heads/main"]},` +
	`"effect":{"protect":{"restricts":["create","delete"],"require_checks":["threads/queue/checks"]}}}]}`

// queueEntry finds one pull request's entry in the queue view: an active row
// first, then a recent outcome (a landed/ejected entry leaves the queue).
func queueEntry(t *testing.T, lane, token string, num int) map[string]any {
	t.Helper()
	v := mustAPI(t, "GET", lane+"/merge-queue", "", token)
	for _, key := range []string{"queues", "recent"} {
		rows, _ := v[key].([]any)
		for _, raw := range rows {
			if key == "queues" {
				q, _ := raw.(map[string]any)
				entries, _ := q["entries"].([]any)
				for _, e := range entries {
					m, _ := e.(map[string]any)
					if n, _ := m["number"].(float64); int(n) == num {
						return m
					}
				}
				continue
			}
			m, _ := raw.(map[string]any)
			if n, _ := m["number"].(float64); int(n) == num {
				return m
			}
		}
	}
	return nil
}

func waitQueueState(t *testing.T, lane, token string, num int, want ...string) map[string]any {
	t.Helper()
	var last map[string]any
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		last = queueEntry(t, lane, token, num)
		if last != nil {
			st, _ := last["state"].(string)
			for _, w := range want {
				if st == w {
					return last
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("queue entry #%d never reached %v; last=%+v", num, want, last)
	return nil
}

// waitPrepared waits for the claimed entry to have its temporary merge commit:
// the queue records "validating" at claim time, and the ref/shas immediately
// after the merge-tree + commit-tree + publish.
func waitPrepared(t *testing.T, lane, token string, num int) map[string]any {
	t.Helper()
	var last map[string]any
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		last = queueEntry(t, lane, token, num)
		if last != nil {
			st, _ := last["state"].(string)
			sha, _ := last["merge_sha"].(string)
			if st == "validating" && sha != "" {
				return last
			}
			if st == "ejected" || st == "cancelled" || st == "landed" {
				t.Fatalf("entry #%d left the queue before it was prepared: %+v", num, last)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("queue entry #%d never prepared; last=%+v", num, last)
	return nil
}

// waitMirroredQueueCheck waits for the merge_group run's job to mirror its
// pending `threads/queue/checks` status, so a subsequent manual report is the
// LAST write for that (sha, context). The run is created asynchronously after
// the ref publish; posting first would race the mirror's pending write and
// leave the queue waiting on a check the test already reported.
func waitMirroredQueueCheck(t *testing.T, lane, sha string) {
	t.Helper()
	pollUntil(t, 30*time.Second, "the merge_group job's mirrored pending check", func() bool {
		st, data := apiCall(t, "GET", lane+"/checks/"+sha, "", chainAliceTok)
		if st != http.StatusOK {
			return false
		}
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return false
		}
		statuses, _ := v["statuses"].([]any)
		for _, raw := range statuses {
			m, _ := raw.(map[string]any)
			if ctx, _ := m["context"].(string); ctx == "threads/queue/checks" {
				state, _ := m["state"].(string)
				return state == "pending"
			}
		}
		return false
	})
}

// waitMerged waits for a queue-driven merge and, on timeout, prints the pull
// request's queue entry and merge task so the failure names the stuck state.
func waitMerged(t *testing.T, lane, token string, num, entryNum int, sha string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := prMerged(t, lane, token, num); ok {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	entry := queueEntry(t, lane, token, entryNum)
	_, prRaw := apiCall(t, "GET", fmt.Sprintf("%s/pulls/%d", lane, num), "", token)
	_, taskRaw := apiCall(t, "GET", fmt.Sprintf("%s/pulls/%d/merge/task", lane, num), "", token)
	_, checksRaw := apiCall(t, "GET", fmt.Sprintf("%s/checks/%s", lane, sha), "", token)
	t.Fatalf("pull request #%d did not merge; queue entry=%+v pr=%s task=%s checks=%s", num, entry, strings.TrimSpace(string(prRaw)), strings.TrimSpace(string(taskRaw)), strings.TrimSpace(string(checksRaw)))
}

// prMerged reports the PR's merged flag.
func prMerged(t *testing.T, lane, token string, num int) (bool, string) {
	t.Helper()
	v := mustAPI(t, "GET", fmt.Sprintf("%s/pulls/%d", lane, num), "", token)
	pr, _ := v["pr"].(map[string]any)
	merged, _ := pr["merged"].(bool)
	sha, _ := pr["merge_commit_sha"].(string)
	if sha == "" {
		sha, _ = pr["merge_commit_sha"].(string)
	}
	return merged, sha
}

// seedQueueRepo creates the workflow + a file on main and returns the work
// directory and the git URL.
func seedQueueRepo(t *testing.T, s *e2eServer, g *gitClient, repo string) (string, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/queue.yml", queueWorkflow, "workflow")
	g.commitFile(work, "app.txt", "v1\n", "seed")
	gitURL := s.gitURL(testOwner, repo)
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	return work, gitURL
}

func openQueuePR(t *testing.T, lane, title, branch string) int {
	t.Helper()
	pr := mustAPI(t, "POST", lane+"/pulls",
		fmt.Sprintf(`{"title":%q,"base_ref":"refs/heads/main","head_ref":"refs/heads/%s"}`, title, branch), chainAliceTok)
	return int(jget(t, pr, "thread", "num").(float64))
}

// TestE2E_MergeQueueTwoPRsLandInOrder is the card's acceptance: two queued
// pull requests land in order, each through a merge_group run on the
// documented temporary ref, and the refs are cleaned up.
func TestE2E_MergeQueueTwoPRsLandInOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("merge queue e2e runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := mergeQueueServer(t)
	owner, repo := testOwner, uniqueRepo("mergequeue")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work, gitURL := seedQueueRepo(t, s, g, repo)
	mustAPI(t, "PUT", lane+"/policy", queuePolicy, chainAliceTok)

	// PR 1: a commit on topic1.
	g.run(work, "checkout", "-q", "-b", "topic1", "main")
	g.commitFile(work, "one.txt", "one\n", "one")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic1")
	pr1 := openQueuePR(t, lane, "First", "topic1")

	// PR 2: a separate branch off main.
	g.run(work, "checkout", "-q", "-b", "topic2", "main")
	g.commitFile(work, "two.txt", "two\n", "two")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic2")
	pr2 := openQueuePR(t, lane, "Second", "topic2")

	for _, pr := range []int{pr1, pr2} {
		mustAPI(t, "POST", lane+"/merge-queue/enqueue",
			fmt.Sprintf(`{"base":"main","number":%d}`, pr), chainAliceTok)
	}

	// The head is prepared and the merge_group run exists for its tip.
	e1 := waitPrepared(t, lane, chainAliceTok, pr1)
	sha1, _ := e1["merge_sha"].(string)
	if len(sha1) != 40 {
		t.Fatalf("entry 1 merge sha = %q", sha1)
	}
	ref, _ := e1["ref"].(string)
	if !strings.HasPrefix(ref, "refs/heads/gh-readonly-queue/main/pr-1-") {
		t.Fatalf("entry 1 ref = %q", ref)
	}
	// The queue ref started exactly one merge_group run, and no push run.
	pollUntil(t, 30*time.Second, "the merge_group run for the queue head", func() bool {
		return len(runsFor(t, lane, "merge_group", sha1)) == 1
	})
	if got := runsFor(t, lane, "push", sha1); len(got) != 0 {
		t.Fatalf("queue ref also created %d push run(s)", len(got))
	}

	// Report the required check on the queue head; the entry lands.
	waitMirroredQueueCheck(t, lane, sha1)
	mustAPI(t, "POST", fmt.Sprintf("%s/checks/statuses/%s", lane, sha1),
		`{"context":"threads/queue/checks","state":"success"}`, chainAliceTok)
	waitMerged(t, lane, chainAliceTok, pr1, pr1, sha1)
	_ = waitQueueState(t, lane, chainAliceTok, pr1, "landed")

	// The second advances on its own and lands once its head is green.
	e2 := waitPrepared(t, lane, chainAliceTok, pr2)
	sha2, _ := e2["merge_sha"].(string)
	if sha2 == "" || sha2 == sha1 {
		t.Fatalf("entry 2 merge sha = %q (entry 1 %q)", sha2, sha1)
	}
	waitMirroredQueueCheck(t, lane, sha2)
	mustAPI(t, "POST", fmt.Sprintf("%s/checks/statuses/%s", lane, sha2),
		`{"context":"threads/queue/checks","state":"success"}`, chainAliceTok)
	waitMerged(t, lane, chainAliceTok, pr2, pr2, sha2)
	_ = waitQueueState(t, lane, chainAliceTok, pr2, "landed")

	// The landed history is in queue order: PR 1's merge commit is an
	// ancestor of PR 2's, and both files are on main.
	_, sha1 = prMerged(t, lane, chainAliceTok, pr1)
	_, sha2 = prMerged(t, lane, chainAliceTok, pr2)
	if len(sha1) != 40 || len(sha2) != 40 {
		t.Fatalf("merged shas = %q / %q", sha1, sha2)
	}
	g.run(work, "fetch", "-q", "origin")
	g.run(work, "merge-base", "--is-ancestor", sha1, sha2)
	for _, file := range []string{"one.txt", "two.txt"} {
		if got := strings.TrimSpace(g.run(work, "show", "origin/main:"+file)); got == "" {
			t.Fatalf("%s missing from main after the queue merged both", file)
		}
	}

	// Every temporary ref is gone on both exit paths.
	refs := strings.TrimSpace(g.run(work, "ls-remote", gitURL, "refs/heads/gh-readonly-queue/*"))
	if refs != "" {
		t.Fatalf("temporary queue refs remain:\n%s", refs)
	}
}

// TestE2E_MergeQueueFailingCheckEjects: a failing required check ejects the
// entry with the named reason, the PR stays open, and the ref is deleted.
func TestE2E_MergeQueueFailingCheckEjects(t *testing.T) {
	if testing.Short() {
		t.Skip("merge queue e2e runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := mergeQueueServer(t)
	owner, repo := testOwner, uniqueRepo("mergequeue")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	work, gitURL := seedQueueRepo(t, s, g, repo)
	mustAPI(t, "PUT", lane+"/policy", queuePolicy, chainAliceTok)

	g.run(work, "checkout", "-q", "-b", "topic", "main")
	g.commitFile(work, "topic.txt", "change\n", "topic")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")
	pr := openQueuePR(t, lane, "Fails", "topic")
	mustAPI(t, "POST", lane+"/merge-queue/enqueue", fmt.Sprintf(`{"base":"main","number":%d}`, pr), chainAliceTok)

	e := waitPrepared(t, lane, chainAliceTok, pr)
	sha, _ := e["merge_sha"].(string)
	waitMirroredQueueCheck(t, lane, sha)
	mustAPI(t, "POST", fmt.Sprintf("%s/checks/statuses/%s", lane, sha),
		`{"context":"threads/queue/checks","state":"failure","description":"tests failed"}`, chainAliceTok)

	done := waitQueueState(t, lane, chainAliceTok, pr, "ejected")
	reason, _ := done["reason"].(string)
	if !strings.Contains(reason, "threads/queue/checks (failure)") {
		t.Fatalf("ejection reason = %q", reason)
	}
	if merged, _ := prMerged(t, lane, chainAliceTok, pr); merged {
		t.Fatal("a failing queue entry must not merge")
	}
	pollUntil(t, 30*time.Second, "the ejected entry's ref to be deleted", func() bool {
		refs := strings.TrimSpace(g.run(work, "ls-remote", gitURL, "refs/heads/gh-readonly-queue/*"))
		return refs == ""
	})
}

// TestE2E_MergeQueueRestartResumes: killing the server mid-validation and
// booting it again recovers the claim (deleting the stale ref, re-preparing
// the merge commit) and the entry lands when green — a restart repeats work,
// it never wedges.
func TestE2E_MergeQueueRestartResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("merge queue e2e runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := mergeQueueServer(t)
	owner, repo := testOwner, uniqueRepo("mergequeue")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	work, _ := seedQueueRepo(t, s, g, repo)
	mustAPI(t, "PUT", lane+"/policy", queuePolicy, chainAliceTok)

	g.run(work, "checkout", "-q", "-b", "topic", "main")
	g.commitFile(work, "topic.txt", "change\n", "topic")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")
	pr := openQueuePR(t, lane, "Restart", "topic")
	mustAPI(t, "POST", lane+"/merge-queue/enqueue", fmt.Sprintf(`{"base":"main","number":%d}`, pr), chainAliceTok)

	e := waitPrepared(t, lane, chainAliceTok, pr)
	oldSHA, _ := e["merge_sha"].(string)
	if oldSHA == "" {
		t.Fatal("no prepared merge commit before the restart")
	}

	// Kill the process mid-validation (the claim is fresh; only boot
	// recovery can take it over promptly).
	s.stop()
	bin := buildSpindle(t)
	port := freePort(t)
	s2 := startServer(t, bin, s.dataDir, port)
	t.Cleanup(s2.stop)
	lane2 := s2.base + "/" + owner + "/" + repo + "/api"

	// The boot pass force-recovers the stale claim and re-prepares: a new
	// merge commit (the old one is deleted with its ref).
	pollUntil(t, 90*time.Second, "the restarted queue to re-prepare the entry", func() bool {
		cur := queueEntry(t, lane2, chainAliceTok, pr)
		if cur == nil {
			return false
		}
		st, _ := cur["state"].(string)
		sha, _ := cur["merge_sha"].(string)
		return st == "validating" && sha != "" && sha != oldSHA
	})
	cur := queueEntry(t, lane2, chainAliceTok, pr)
	newSHA, _ := cur["merge_sha"].(string)
	waitMirroredQueueCheck(t, lane2, newSHA)
	mustAPI(t, "POST", fmt.Sprintf("%s/checks/statuses/%s", lane2, newSHA),
		`{"context":"threads/queue/checks","state":"success"}`, chainAliceTok)
	waitMerged(t, lane2, chainAliceTok, pr, pr, newSHA)
}

// TestE2E_MergeQueueDequeueCancels: a maintainer (or the author) removes an
// entry; it never lands, the ref is deleted, and the PR stays open.
func TestE2E_MergeQueueDequeueCancels(t *testing.T) {
	if testing.Short() {
		t.Skip("merge queue e2e runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := mergeQueueServer(t)
	owner, repo := testOwner, uniqueRepo("mergequeue")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	work, gitURL := seedQueueRepo(t, s, g, repo)
	mustAPI(t, "PUT", lane+"/policy", queuePolicy, chainAliceTok)

	g.run(work, "checkout", "-q", "-b", "topic", "main")
	g.commitFile(work, "topic.txt", "change\n", "topic")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")
	pr := openQueuePR(t, lane, "Cancel", "topic")
	mustAPI(t, "POST", lane+"/merge-queue/enqueue", fmt.Sprintf(`{"base":"main","number":%d}`, pr), chainAliceTok)
	_ = waitPrepared(t, lane, chainAliceTok, pr)

	mustAPI(t, "POST", lane+"/merge-queue/dequeue",
		fmt.Sprintf(`{"base":"main","number":%d}`, pr), chainAliceTok)
	done := waitQueueState(t, lane, chainAliceTok, pr, "cancelled")
	if reason, _ := done["reason"].(string); !strings.Contains(reason, "dequeued") {
		t.Fatalf("cancel reason = %q", reason)
	}
	if merged, _ := prMerged(t, lane, chainAliceTok, pr); merged {
		t.Fatal("a dequeued entry must not merge")
	}
	pollUntil(t, 30*time.Second, "the cancelled entry's ref to be deleted", func() bool {
		return strings.TrimSpace(g.run(work, "ls-remote", gitURL, "refs/heads/gh-readonly-queue/*")) == ""
	})
}
