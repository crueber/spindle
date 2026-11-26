package workflow

import (
	"strings"
	"testing"
)

// prWorkflow renders a minimal workflow whose pull_request trigger carries the
// given `types` value (raw YAML scalar/list text; empty means the bare key).
func prWorkflow(types string) string {
	pr := "  pull_request:\n"
	if types != "" {
		pr = "  pull_request:\n    types: " + types + "\n"
	}
	return "name: pr\non:\n" + pr + `jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`
}

// TestPullRequestTypesMatrix pins 15_threads.md §6.2: every supported type
// matches exactly its own emitted action and nothing else.
func TestPullRequestTypesMatrix(t *testing.T) {
	// The vocabulary itself is pinned: the actions Threads emits are exactly
	// these three (internal/pulls PullEvent).
	if got, want := strings.Join(PullRequestTypes, ","), "opened,synchronize,reopened,closed,edited,ready_for_review,converted_to_draft,labeled,unlabeled,assigned,unassigned,milestoned,demilestoned"; got != want {
		t.Fatalf("PullRequestTypes = %q, want %q", got, want)
	}
	actions := PullRequestTypes
	for _, typ := range PullRequestTypes {
		w, err := Parse([]byte(prWorkflow("[" + typ + "]")))
		if err != nil {
			t.Fatalf("types: [%s]: %v", typ, err)
		}
		for _, action := range actions {
			want := action == typ
			if got := w.Fires("pull_request", RefBranch, "main", action); got != want {
				t.Errorf("types: [%s] Fires(%s) = %v, want %v", typ, action, got, want)
			}
		}
		// A filter never matches an event whose action is unknown.
		if w.Fires("pull_request", RefBranch, "main", "") {
			t.Errorf("types: [%s] fired for an empty action", typ)
		}
	}
	// The multi-type spelling is the OR of its entries.
	w, err := Parse([]byte(prWorkflow("[opened, reopened]")))
	if err != nil {
		t.Fatalf("multi-type: %v", err)
	}
	for action, want := range map[string]bool{"opened": true, "synchronize": false, "reopened": true} {
		if got := w.Fires("pull_request", RefBranch, "main", action); got != want {
			t.Errorf("types: [opened, reopened] Fires(%s) = %v, want %v", action, got, want)
		}
	}
}

// TestPullRequestTypesDefault pins the default: an absent filter (bare key or
// no `types:`) fires on every emitted action.
func TestPullRequestTypesDefault(t *testing.T) {
	for name, src := range map[string]string{
		"bare key":      prWorkflow(""),
		"null types":    "name: pr\non:\n  pull_request:\n    types:\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n",
		"no pr filters": "name: pr\non: [pull_request]\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n",
	} {
		w, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, action := range PullRequestTypes {
			if !w.Fires("pull_request", RefBranch, "main", action) {
				t.Errorf("%s: did not fire on the default action %s", name, action)
			}
		}
	}
}

// TestPullRequestTypesRejected pins the closed vocabulary: a type Threads never
// emits is this file's parse error naming the supported set, never a silently
// dead trigger. `types: []` matches nothing, so it is rejected too.
func TestPullRequestTypesRejected(t *testing.T) {
	for _, types := range []string{"[deleted]", "[locked]", "[assigned, deleted]", "[]"} {
		_, err := Parse([]byte(prWorkflow(types)))
		if err == nil {
			t.Errorf("types: %s accepted", types)
			continue
		}
		if !strings.Contains(err.Error(), "pull_request.types") {
			t.Errorf("types: %s error %q does not name the key", types, err)
		}
		if strings.Contains(types, "[]") {
			if !strings.Contains(err.Error(), "empty list") {
				t.Errorf("empty types error %q does not explain the empty list", err)
			}
			continue
		}
		for _, supported := range PullRequestTypes {
			if !strings.Contains(err.Error(), supported) {
				t.Errorf("types: %s error %q does not name %q", types, err, supported)
			}
		}
	}
}
