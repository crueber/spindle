// threads_wave6_triggers_test.go — the Wave 6 trigger/edge partials end to
// end: PAR-020 (review, inline comment, PR issue_comment, label, milestone) and
// PAR-021 (fork, watch, check_run), plus the per-organization cap (PAR-153) and
// the workflow_run cross-run artifact hand-off (PAR-019). No runner is needed
// for the trigger assertions; the two runner-backed tests say so.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wave6Workflows declares one workflow per emitted trigger, all on the same
// default branch, so every transition can be observed as its own run.
var wave6Workflows = map[string]string{
	"defs.yml": `name: Definitions
on: [label, milestone]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`,
	"reviews.yml": `name: Reviews
on: [pull_request_review, pull_request_review_comment]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`,
	"comments.yml": `name: Comments
on: [issue_comment]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`,
	"fork.yml": `name: Forks
on: [fork]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo fork
`,
	"watch.yml": `name: Watchers
on: [watch]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`,
	"checks.yml": `name: Checks
on: [check_run]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo check
`,
}

// waitForEvent polls the run list for one run of event and returns it.
func waitForEvent(t *testing.T, lane, event string, timeout time.Duration) threadRunSummary {
	t.Helper()
	var got threadRunSummary
	pollUntil(t, timeout, "a "+event+" run", func() bool {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, lane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event == event {
				got = r
				return true
			}
		}
		return false
	})
	return got
}

func TestE2EWave6TriggerEdges(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	owner, repo := testOwner, uniqueRepo("wave6edges")
	lane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	for name, body := range wave6Workflows {
		g.commitFile(work, ".spindle/workflows/"+name, body, name)
	}
	mainSHA := g.commitFile(work, "README.md", "default branch tip\n", "readme")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// PAR-020: a label and a milestone definition.
	mustAPI(t, "POST", lane+"/labels", `{"name":"bug","color":"ff0000","description":"broken"}`, chainAliceTok)
	if r := waitForEvent(t, lane, "label", 25*time.Second); r.HoldReason != "" {
		t.Fatalf("label run held: %q", r.HoldReason)
	}
	ms := mustAPI(t, "POST", lane+"/milestones", `{"title":"v1","description":"first"}`, chainAliceTok)
	msID := fmt.Sprint(ms["milestone"].(map[string]any)["id"])
	mustAPI(t, "PATCH", lane+"/milestones/"+msID, `{"state":"closed"}`, chainAliceTok)
	waitForEvent(t, lane, "milestone", 25*time.Second)

	// A PR, a conversation comment, a review and an inline review comment.
	g.run(work, "checkout", "-q", "-b", "topic")
	headSHA := g.commitFile(work, "topic.txt", "change\n", "topic")
	g.runAuth(work, chainAliceTok, "push", "origin", "topic")
	pr := mustAPI(t, "POST", lane+"/pulls",
		`{"title":"Wave 6","base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`, chainAliceTok)
	prNum := int(jget(t, pr, "thread", "num").(float64))
	mustAPI(t, "POST", fmt.Sprintf("%s/pulls/%d/comments", lane, prNum),
		`{"body":"a conversation comment"}`, chainAliceTok)
	waitForEvent(t, lane, "issue_comment", 25*time.Second)
	ctxSHA := strings.Repeat("ab", 32)
	anchor := fmt.Sprintf(`{"path":"topic.txt","side":"NEW","new_start":1,"new_lines":1,"commit_sha":%q,"context_sha":%q}`, headSHA, ctxSHA)
	mustAPI(t, "POST", fmt.Sprintf("%s/pulls/%d/reviews", lane, prNum),
		fmt.Sprintf(`{"state":"COMMENTED","body":"review body","commit_sha":%q,"threads":[{"anchor":%s,"body":"nit"}]}`, headSHA, anchor),
		chainAliceTok)
	waitForEvent(t, lane, "pull_request_review", 25*time.Second)
	waitForEvent(t, lane, "pull_request_review_comment", 25*time.Second)

	// PAR-021: a fork, a star, and an external check report.
	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), chainAliceTok)
	waitForEvent(t, lane, "fork", 150*time.Second)
	if st, _ := apiCall(t, "PUT", lane+"/star", "", chainAliceTok); st != 200 && st != 204 {
		t.Fatalf("star status = %d", st)
	}
	waitForEvent(t, lane, "watch", 25*time.Second)
	mustAPI(t, "POST", lane+"/checks/statuses/"+mainSHA,
		`{"context":"ci/build","state":"success","description":"ok"}`, chainAliceTok)
	run := waitForEvent(t, lane, "check_run", 25*time.Second)
	// The check_run run reads the default branch's file; its payload names the
	// reported head and the check, not the reporter's commit.
	view := getRun(t, lane, run.ID, chainAliceTok)
	payload, _ := view.Run["event_payload"].(map[string]any)
	cr, _ := payload["check_run"].(map[string]any)
	if cr["name"] != "ci/build" || cr["head_sha"] != strings.ToLower(mainSHA) {
		t.Fatalf("check_run payload = %v", payload)
	}
	if view.Run["commit"] != mainSHA {
		t.Fatalf("a check_run run reported commit %v, want the default branch %s", view.Run["commit"], mainSHA)
	}
}

// TestE2EOrgConcurrencyCap is PAR-153's per-organization half: an organization
// cap of 1 holds the second repository's run with exactly one named reason, and
// a terminal fold in the first releases it.
func TestE2EOrgConcurrencyCap(t *testing.T) {
	if testing.Short() {
		t.Skip("trigger e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	org := uniqueRepo("w6org")
	mustAPI(t, "POST", s.base+"/api/v1/orgs",
		fmt.Sprintf(`{"org":%q,"display_name":"Wave 6 Org"}`, org), chainAliceTok)
	mustAPI(t, "PUT", s.base+"/api/v1/orgs/"+org+"/threads/settings",
		`{"concurrency_cap":1}`, chainAliceTok)

	const workflow = `name: Org cap
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`
	seed := func(name string) (lane string) {
		work := filepath.Join(t.TempDir(), name)
		g.initRepo(work)
		g.commitFile(work, ".spindle/workflows/ci.yml", workflow, "ci")
		g.run(work, "remote", "add", "origin", s.gitURL(org, name))
		g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
		return s.base + "/" + org + "/" + name + "/api"
	}
	firstLane := seed("one")
	first := waitForEvent(t, firstLane, "push", 30*time.Second)
	if first.HoldReason != "" {
		t.Fatalf("the first run under a cap of 1 was held: %q", first.HoldReason)
	}
	// Wait until the first run is admitted into the organization index (the
	// settings projection reports the live count), so the second push observes
	// it deterministically.
	settingsURL := s.base + "/api/v1/orgs/" + org + "/threads/settings"
	pollUntil(t, 30*time.Second, "the first run's organization admission", func() bool {
		view := mustAPI(t, "GET", settingsURL, "", chainAliceTok)
		live, _ := view["live"].(float64)
		return live >= 1
	})
	secondLane := seed("two")
	second := waitForEvent(t, secondLane, "push", 30*time.Second)
	want := "organization concurrency cap 1 reached"
	pollUntil(t, 30*time.Second, "the second run's org-cap hold", func() bool {
		v := getRun(t, secondLane, second.ID, chainAliceTok)
		held, _ := v.Run["hold_reason"].(string)
		return held == want
	})
	view := getRun(t, secondLane, second.ID, chainAliceTok)
	for _, j := range view.Jobs {
		if j["state"] == "queued" && j["queue_reason"] != want {
			t.Fatalf("job reason = %v, want %q", j["queue_reason"], want)
		}
	}
	// Cancelling the first (terminal fold) releases the held run.
	mustAPI(t, "POST", firstLane+"/threads/"+first.ID+"/cancel", "", chainAliceTok)
	pollUntil(t, 30*time.Second, "the held run's release", func() bool {
		v := getRun(t, secondLane, second.ID, chainAliceTok)
		held, _ := v.Run["hold_reason"].(string)
		return held == ""
	})
}

// TestE2EWorkflowRunUntrustedParentArtifactDownload is PAR-019's acceptance:
// a follow-up workflow downloads the first (untrusted) run's artifact and is
// itself gated before a maintainer approves it.
func TestE2EWorkflowRunUntrustedParentArtifactDownload(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := artifactsServer(t, false) // the fork runs immediately; the follow-up is held
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("w6crossrun")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	// The parent is a fork pull_request run: it uploads one artifact.
	g.commitFile(work, ".spindle/workflows/build.yml", `name: Build
on: [pull_request]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: make files
        run: |
          mkdir -p dist
          printf 'alpha' > dist/a.txt
      - name: upload
        uses: actions/upload-artifact
        with:
          name: build-out
          path: dist
`, "build")
	// The follow-up names the upstream run from the trigger payload.
	g.commitFile(work, ".spindle/workflows/follow.yml", `name: Follow
on:
  workflow_run:
    workflows: [Build]
    types: [completed]
jobs:
  consume:
    name: consume
    runs-on: linux
    steps:
      - uses: actions/download-artifact
        with:
          run-id: ${{ github.event.workflow_run.id }}
          name: build-out
          path: in
      - name: assert bytes
        run: |
          test "$(cat in/dist/a.txt)" = alpha
          echo "PAR019-CROSS-RUN-OK"
`, "follow")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	forkName := repo + "-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, owner, forkName), e2eDefaultBearer)
	forkLane := s.base + "/" + owner + "/" + forkName + "/api"
	pollUntil(t, 60*time.Second, "fork to become servable", func() bool {
		st, _ := apiCall(t, "GET", forkLane, "", e2eDefaultBearer)
		return st == 200
	})
	g.run(work, "checkout", "-q", "-b", "prbranch")
	g.commitFile(work, "pr.txt", "one\n", "pr head")
	g.runAuth(work, e2eDefaultBearer, "push", "origin", "prbranch")
	g.runAuth(work, e2eDefaultBearer, "push", s.gitURL(owner, forkName), "prbranch")
	pollUntil(t, 60*time.Second, "fork branch visible", func() bool {
		refs := g.lsRemoteAuthOK(s.gitURL(owner, forkName), e2eDefaultBearer)
		if refs == nil {
			return false
		}
		_, ok := refs["refs/heads/prbranch"]
		return ok
	})
	mustAPI(t, "POST", repoLane+"/pulls",
		fmt.Sprintf(`{"title":"PAR-019 fork PR","base_ref":"refs/heads/main","head_ref":"refs/heads/prbranch","fork":{"repo":%q}}`, owner+"/"+forkName), e2eDefaultBearer)

	reg := mintRunnerToken(t, repoLane, e2eDefaultBearer)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)
	var parent string
	pollUntil(t, 60*time.Second, "the parent pull_request run", func() bool {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, repoLane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event == "pull_request" {
				parent = r.ID
				return true
			}
		}
		return false
	})
	waitRunSuccess(t, repoLane, parent, e2eDefaultBearer, runner)

	var held string
	pollUntil(t, 90*time.Second, "the held workflow_run follow-up", func() bool {
		var page struct {
			Runs []threadRunSummary `json:"runs"`
		}
		getJSON(t, repoLane+"/threads?n=200", &page)
		for _, r := range page.Runs {
			if r.Event == "workflow_run" && r.State == "pending_approval" && r.ApprovalKind == "workflow_run" {
				held = r.ID
				return true
			}
		}
		return false
	})
	// Nothing executed while held.
	heldView := getRun(t, repoLane, held, e2eDefaultBearer)
	for _, j := range heldView.Jobs {
		if j["state"] != "queued" {
			t.Fatalf("a held follow-up job ran: %v", j)
		}
	}
	mustAPI(t, "POST", repoLane+"/threads/"+held+"/approve", "", e2eDefaultBearer)
	waitRunSuccess(t, repoLane, held, e2eDefaultBearer, runner)
	log := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-consume/log", repoLane, held), e2eDefaultBearer)
	if !strings.Contains(log, "PAR019-CROSS-RUN-OK") {
		t.Fatalf("the approved follow-up did not download the parent artifact:\n%s", log)
	}
}
