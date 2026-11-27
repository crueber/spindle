package threads

// Wave 6: the remaining trigger/edge partials (PAR-020, PAR-021). These tests
// pin the closed vocabulary, the emitters' payloads and the default-branch
// rule; the service-owned emitters are pinned in their own packages.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestNewTriggerVocabulary(t *testing.T) {
	cases := []struct {
		event  string
		source string
		action string
		want   bool
	}{
		{"pull_request_review", "on: {pull_request_review: {types: [submitted]}}", "submitted", true},
		{"pull_request_review", "on: {pull_request_review: {types: [submitted]}}", "dismissed", false},
		{"pull_request_review", "on: pull_request_review", "dismissed", true},
		{"pull_request_review_comment", "on: {pull_request_review_comment: {types: [created]}}", "created", true},
		{"label", "on: label", "deleted", true},
		{"milestone", "on: {milestone: {types: [closed]}}", "closed", true},
		{"milestone", "on: {milestone: {types: [closed]}}", "opened", false},
		{"check_run", "on: {check_run: {types: [completed]}}", "completed", true},
		{"check_run", "on: {check_run: {types: [completed]}}", "created", false},
		{"fork", "on: fork", "", true},
		{"watch", "on: watch", "started", true},
	}
	for _, c := range cases {
		w, err := workflow.Parse([]byte(c.source + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
		if err != nil {
			t.Fatalf("%s: parse: %v", c.source, err)
		}
		if got := w.Fires(c.event, workflow.RefBranch, "main", c.action); got != c.want {
			t.Fatalf("%s/%s = %v, want %v", c.event, c.action, got, c.want)
		}
	}
	// Types the owning service cannot emit are named parse errors, not inert.
	for _, src := range []string{
		"on: {pull_request_review: {types: [edited]}}",
		"on: {pull_request_review_comment: {types: [deleted]}}",
		"on: {label: {types: [opened]}}",
		"on: {milestone: {types: [merged]}}",
		"on: {check_run: {types: [rerequested]}}",
	} {
		if _, err := workflow.Parse([]byte(src + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n")); err == nil {
			t.Fatalf("%s must be a parse error", src)
		}
	}
}

func TestNewRefusedTriggerNames(t *testing.T) {
	for _, name := range []string{"discussion", "discussion_comment", "pull_request_review_thread"} {
		_, err := workflow.Parse([]byte("on: " + name + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s must be a named refusal, got %v", name, err)
		}
	}
	// The implemented names are NOT in the refusal table (an accepted trigger
	// must have an emitter).
	for _, name := range []string{"fork", "watch", "check_run"} {
		if _, refused := workflow.RefusedTriggers[name]; refused {
			t.Fatalf("%s is emitted but still listed as refused", name)
		}
	}
}

func TestLabelMilestoneTriggersCreateDefaultBranchRuns(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.branch = "main"
	e.git.add(testSHA, ".spindle/workflows/defs.yml", `name: Definitions
on: [label, milestone]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`)
	for _, tc := range []struct {
		event   string
		action  string
		payload map[string]any
		key     string
	}{
		{EventLabel, "created", LabelEventPayload("o/r", "created", "writer", "bug", "ff0000", "broken"), "label"},
		{EventMilestone, "closed", MilestoneEventPayload("o/r", "closed", "writer", "m1", "v1", "closed", "first", ""), "milestone"},
	} {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, tc.event, CreateOpts{
			Action: tc.action, Actor: "writer", EventPayload: tc.payload,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		if len(runs) != 1 || runs[0].Event != tc.event {
			t.Fatalf("%s runs = %+v", tc.event, runs)
		}
		if runs[0].Branch != "main" || runs[0].Ref != "refs/heads/main" {
			t.Fatalf("%s run ref = %q/%q, want the default branch", tc.event, runs[0].Branch, runs[0].Ref)
		}
		if _, ok := runs[0].EventPayload[tc.key]; !ok {
			t.Fatalf("%s payload lacks %q: %+v", tc.event, tc.key, runs[0].EventPayload)
		}
		// The repository-level event reads the default branch's file, so the
		// reported commit is the default tip even when the caller hands another.
		if runs[0].Commit != testSHA {
			t.Fatalf("%s commit = %s", tc.event, runs[0].Commit)
		}
	}
	// A definition type the service cannot emit is refused.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventLabel, CreateOpts{Action: "opened"}); err == nil {
		t.Fatal("label.opened must be refused")
	}
}

func TestReviewTriggersCreatePullFamilyRuns(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.branch = "main"
	e.git.add(testSHA, ".spindle/workflows/reviews.yml", `name: Reviews
on: [pull_request_review, pull_request_review_comment]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`)
	reviewPayload := ReviewEventPayload("o/r", "submitted", "writer", "writer",
		"approved", "looks good", testSHA, 7, "refs/heads/main", "refs/heads/topic", testSHA, "o/r")
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReview, CreateOpts{
		Action: "submitted", Branch: "main", BaseBranch: "main", Actor: "writer",
		PullNumber: 7, Ref: "refs/pull/7/head", EventPayload: reviewPayload,
	})
	if err != nil {
		t.Fatalf("CreateRun(review): %v", err)
	}
	if len(runs) != 1 || runs[0].Event != EventPullReview {
		t.Fatalf("review runs = %+v", runs)
	}
	commentPayload := ReviewCommentEventPayload("o/r", "created", "writer", "nit", "00000001",
		"a.go", "NEW", testSHA, 3, 7, "refs/heads/main", "refs/heads/topic", testSHA, "o/r")
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReviewComment, CreateOpts{
		Action: "created", Branch: "main", BaseBranch: "main", Actor: "writer",
		PullNumber: 7, Ref: "refs/pull/7/head", EventPayload: commentPayload,
	}); err != nil {
		t.Fatalf("CreateRun(review comment): %v", err)
	}
	// A review type the service never emits is refused.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReview, CreateOpts{Action: "edited", Branch: "main"}); err == nil {
		t.Fatal("pull_request_review.edited must be refused")
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReviewComment, CreateOpts{Action: "deleted", Branch: "main"}); err == nil {
		t.Fatal("pull_request_review_comment.deleted must be refused")
	}
}

func TestForkWatchCheckRunTriggers(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.branch = "main"
	e.git.add(testSHA, ".spindle/workflows/edges.yml", `name: Edges
on: [fork, watch, check_run]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.action }}"
`)
	cases := []struct {
		event   string
		action  string
		payload map[string]any
	}{
		{EventFork, "", ForkEventPayload("o/r", "writer", "o/r-fork")},
		{EventWatch, "started", WatchEventPayload("o/r", "writer", "started")},
		{EventCheckRun, "completed", CheckRunEventPayload("o/r", "completed", testSHA, "ci/build", "completed", "success", "", "ok", "external-ci", 2)},
	}
	for _, tc := range cases {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, tc.event, CreateOpts{
			Action: tc.action, Actor: "writer", EventPayload: tc.payload,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		if len(runs) != 1 || runs[0].Event != tc.event {
			t.Fatalf("%s runs = %+v", tc.event, runs)
		}
		if runs[0].Branch != "main" {
			t.Fatalf("%s branch = %q", tc.event, runs[0].Branch)
		}
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventCheckRun, CreateOpts{Action: "rerequested"}); err == nil {
		t.Fatal("check_run.rerequested must be refused")
	}
}

func TestEventPayloadBoundsAndShapes(t *testing.T) {
	long := strings.Repeat("x", maxEventBodyBytes*2)
	label := LabelEventPayload("o/r", "created", "writer", long, "ff0000", long)
	lb, _ := label["label"].(map[string]any)
	if got := lb["description"].(string); len(got) > maxEventBodyBytes {
		t.Fatalf("label description was not bounded: %d bytes", len(got))
	}
	ms := MilestoneEventPayload("o/r", "edited", "writer", "m1", "v1", "open", "", "")
	if ms["milestone"].(map[string]any)["number"] != "m1" {
		t.Fatalf("milestone payload = %+v", ms)
	}
	cr := CheckRunEventPayload("o/r", "created", testSHA, "ci", "in_progress", "", "", "", "ci", 3)
	if cr["check_run"].(map[string]any)["annotations_count"] != 3 {
		t.Fatalf("check_run payload = %+v", cr)
	}
	fork := ForkEventPayload("o/r", "writer", "o/fork")
	if fork["forkee"].(map[string]any)["full_name"] != "o/fork" {
		t.Fatalf("fork payload = %+v", fork)
	}
	if WatchEventPayload("o/r", "writer", "started")["action"] != "started" {
		t.Fatal("watch payload lost its action")
	}
}
