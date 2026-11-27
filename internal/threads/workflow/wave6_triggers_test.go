package workflow

// Wave 6 parser edges: the new trigger vocabulary is closed and every spelling
// (scalar, sequence, mapping, null) round-trips.

import (
	"strings"
	"testing"
)

func TestWave6TriggerSpellings(t *testing.T) {
	sources := []string{
		"on: fork",
		"on: watch",
		"on: check_run",
		"on: [label, milestone, pull_request_review, pull_request_review_comment, check_run, fork, watch]",
		"on: {label: , milestone: , pull_request_review: , pull_request_review_comment: , check_run: , fork: , watch: }",
		"on: {pull_request_review: {types: [submitted, dismissed]}}",
		"on: {pull_request_review_comment: {types: [created]}}",
		"on: {label: {types: [created, edited, deleted]}}",
		"on: {milestone: {types: [created, opened, closed, edited, deleted]}}",
		"on: {check_run: {types: [created, completed]}}",
	}
	for _, src := range sources {
		w, err := Parse([]byte(src + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		events := strings.Join(w.Events(), ",")
		for _, want := range []string{"label", "milestone", "pull_request_review", "pull_request_review_comment", "check_run", "fork", "watch"} {
			if strings.Contains(src, want) && !strings.Contains(events, want) {
				t.Fatalf("%s: Events() = %s, missing %s", src, events, want)
			}
		}
	}
}

func TestWave6TriggerRefusalsAndBadKeys(t *testing.T) {
	cases := []string{
		"on: {fork: {types: [started]}}",
		"on: {watch: {types: [started]}}",
		"on: {check_run: {branches: [main]}}",
		"on: {label: {branches: [main]}}",
		"on: {milestone: {branches: [main]}}",
		"on: {pull_request_review: {branches: [main]}}",
		"on: {pull_request_review_comment: {branches: [main]}}",
		"on: {check_run: {types: []}}",
		"on: {label: {types: []}}",
		"on: {milestone: {types: []}}",
		"on: {pull_request_review: {types: []}}",
		"on: {pull_request_review_comment: {types: []}}",
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n")); err == nil {
			t.Fatalf("%s must be a parse error", src)
		}
	}
	// The definition types are closed.
	for _, src := range []string{
		"on: {label: {types: [opened]}}",
		"on: {milestone: {types: [deleted_by_user]}}",
		"on: {check_run: {types: [requested_action]}}",
		"on: {pull_request_review: {types: [edited]}}",
		"on: {pull_request_review_comment: {types: [deleted]}}",
	} {
		_, err := Parse([]byte(src + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
		if err == nil || !strings.Contains(err.Error(), "unsupported type") {
			t.Fatalf("%s: %v", src, err)
		}
	}
	// The named refusals stay refusals.
	for _, name := range []string{"discussion", "discussion_comment", "pull_request_review_thread", "check_suite", "status"} {
		if _, err := Parse([]byte("on: " + name + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n")); err == nil {
			t.Fatalf("%s must be refused by name", name)
		}
	}
}
