package threads

import (
	"errors"
	"strings"
	"testing"
)

const prTypedWorkflowYAML = `name: PR Typed
on:
  pull_request:
    types: [opened]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

// TestPullRequestTypesFilter creates runs only for the actions a workflow
// names (15_threads.md §6.2). The pulls service emits opened, synchronize and
// reopened; a `types: [opened]` workflow must not run for the other two.
func TestPullRequestTypesFilter(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/pr.yml", prTypedWorkflowYAML)
	// The default fixture file declares a bare pull_request trigger, so it
	// fires on every action; the typed file is the one under test.
	for _, tc := range []struct {
		action string
		typed  int // runs from pr.yml
		all    int // total runs (pr.yml + ci.yml)
	}{
		{"opened", 1, 2},
		{"synchronize", 0, 1},
		{"reopened", 0, 1},
	} {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq,
			CreateOpts{Branch: "main", BaseBranch: "main", Action: tc.action})
		if err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		typed := 0
		for _, r := range runs {
			if r.WorkflowPath == ".spindle/workflows/pr.yml" {
				typed++
			}
		}
		if len(runs) != tc.all || typed != tc.typed {
			t.Errorf("%s: runs = %d (typed %d), want %d (typed %d)", tc.action, len(runs), typed, tc.all, tc.typed)
		}
	}
}

// TestPullRequestTypesDefaultFiresEveryAction pins the default: a trigger with
// no `types:` filter still fires on each emitted action.
func TestPullRequestTypesDefaultFiresEveryAction(t *testing.T) {
	e := newTestEnv(t)
	for _, action := range []string{"opened", "synchronize", "reopened"} {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq,
			CreateOpts{Branch: "main", BaseBranch: "main", Action: action})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if len(runs) != 1 {
			t.Errorf("%s: runs = %d, want 1", action, len(runs))
		}
	}
}

// TestPullRequestUnsupportedTypeIsPerFileError pins the parse contract: a
// `types: [closed]` file disables only itself and its error names the
// supported set; the sibling workflow still runs.
func TestPullRequestUnsupportedTypeIsPerFileError(t *testing.T) {
	e := newTestEnv(t)
	src := strings.Replace(prTypedWorkflowYAML, "[opened]", "[deleted]", 1)
	e.git.add(testSHA, ".spindle/workflows/closed.yml", src)
	files, err := Discover(ctx(), e.git, "o/r", testSHA)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var found bool
	for _, f := range files {
		if f.Path != ".spindle/workflows/closed.yml" {
			continue
		}
		found = true
		if f.Workflow != nil || !strings.Contains(f.Error, "unsupported type") {
			t.Fatalf("closed.yml = %+v (want a parse error naming the unsupported type)", f)
		}
		for _, supported := range []string{"opened", "synchronize", "reopened", "closed"} {
			if !strings.Contains(f.Error, supported) {
				t.Errorf("error %q does not name the supported type %q", f.Error, supported)
			}
		}
	}
	if !found {
		t.Fatal("closed.yml not discovered")
	}
	// opened still creates the sibling run and never one for closed.yml.
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq,
		CreateOpts{Branch: "main", BaseBranch: "main", Action: "opened"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 1 || runs[0].WorkflowPath != ".spindle/workflows/ci.yml" {
		t.Fatalf("runs = %+v, want only the parseable ci.yml", runs)
	}
}

// TestPullRequestActionRejected pins fail-closed CreateRun: a pull_request
// without a known action is ErrInvalid naming the vocabulary, so a caller
// cannot invent an edge whose `types` filter would silently never match.
func TestPullRequestActionRejected(t *testing.T) {
	e := newTestEnv(t)
	for _, action := range []string{"", "deleted", "locked"} {
		_, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq,
			CreateOpts{Branch: "main", BaseBranch: "main", Action: action})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("action %q: err = %v, want ErrInvalid", action, err)
			continue
		}
		for _, supported := range []string{"opened", "synchronize", "reopened", "closed"} {
			if !strings.Contains(err.Error(), supported) {
				t.Errorf("action %q: error %q does not name %q", action, err, supported)
			}
		}
	}
}
