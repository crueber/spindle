package threads

import (
	"context"
	"strings"
	"testing"
)

const pathsWorkflow = `name: src only
on:
  push:
    branches: [main]
    paths: ["src/**"]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

// TestPushPathsFilterAndBudget pins PAR-012: a docs-only push creates no run, a
// mixed commit does, and the diff is computed once per event (zero diff calls
// when no workflow declares a path filter).
func TestPushPathsFilterAndBudget(t *testing.T) {
	old := strings.Repeat("a", 40)
	next := strings.Repeat("b", 40)
	other := strings.Repeat("c", 40)
	e := newTestEnv(t)
	e.git.add(next, ".spindle/workflows/src.yml", pathsWorkflow)
	e.git.add(other, ".spindle/workflows/plain.yml", `name: plain
on: [push]
jobs:
  b:
    runs-on: linux
    steps:
      - run: echo hi
`)
	opts := CreateOpts{Branch: "main", RefName: "main", Ref: "refs/heads/main", BaseSHA: old, Actor: "dev"}
	e.git.diffs[old+".."+next] = []string{"docs/readme.md"}
	runs, err := e.svc.CreateRun(ctx(), "o/r", next, EventPush, opts)
	if err != nil {
		t.Fatalf("docs-only push: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a docs-only push created %d run(s)", len(runs))
	}
	if e.git.diffCalls != 1 {
		t.Fatalf("diff calls = %d, want exactly one per event", e.git.diffCalls)
	}
	e.git.diffs[old+".."+next] = []string{"docs/readme.md", "src/a.go"}
	runs, err = e.svc.CreateRun(ctx(), "o/r", next, EventPush, opts)
	if err != nil {
		t.Fatalf("mixed push: %v", err)
	}
	if len(runs) != 1 || runs[0].WorkflowPath != ".spindle/workflows/src.yml" {
		t.Fatalf("mixed push runs = %+v", runs)
	}

	// A repository whose files declare no path filter spends no diff at all.
	plainOld, plainNew := strings.Repeat("d", 40), strings.Repeat("e", 40)
	e.git.add(plainNew, ".spindle/workflows/plain.yml", `name: plain
on: [push]
jobs:
  b:
    runs-on: linux
    steps:
      - run: echo hi
`)
	before := e.git.diffCalls
	if _, err := e.svc.CreateRun(ctx(), "o/r", plainNew, EventPush, CreateOpts{
		Branch: "main", RefName: "main", Ref: "refs/heads/main", BaseSHA: plainOld, Actor: "dev",
	}); err != nil {
		t.Fatalf("plain push: %v", err)
	}
	if e.git.diffCalls != before {
		t.Fatalf("a workflow without paths computed a diff (%d -> %d)", before, e.git.diffCalls)
	}
}

// TestPullRequestPathsFilterUsesMergeBase pins the PR rule: the changed set is
// base...head (merge base), and a docs-only PR creates no run.
func TestPullRequestPathsFilterUsesMergeBase(t *testing.T) {
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	e := newTestEnv(t)
	e.git.add(head, ".spindle/workflows/pr.yml", `name: pr
on:
  pull_request:
    branches: [main]
    paths: ["src/**"]
jobs:
  b:
    runs-on: linux
    steps:
      - run: echo hi
`)
	e.git.diffs[base+"..."+head] = []string{"docs/a.md"}
	runs, err := e.svc.CreateRun(context.Background(), "o/r", head, EventPullReq, CreateOpts{
		Branch: "main", RefName: "main", BaseBranch: "main", BaseSHA: base, Action: "opened", Actor: "dev",
	})
	if err != nil {
		t.Fatalf("docs-only PR: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("docs-only PR created %d run(s)", len(runs))
	}
	e.git.diffs[base+"..."+head] = []string{"src/a.go"}
	runs, err = e.svc.CreateRun(context.Background(), "o/r", head, EventPullReq, CreateOpts{
		Branch: "main", RefName: "main", BaseBranch: "main", BaseSHA: base, Action: "opened", Actor: "dev",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("src PR = %+v, %v", runs, err)
	}
}
