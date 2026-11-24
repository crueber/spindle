package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
)

// Pass A trigger wiring (docs/features/15 §1/§2): the pull_request edges and
// the merge task's server-side publish both land on the SAME run-creation path
// as a client push, through the existing pulls seams.

const triggerWorkflow = `name: CI
on: [push, pull_request]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

// triggerGit is a minimal GitReader over in-memory commits and refs.
type triggerGit struct {
	files map[string]map[string][]byte
	refs  map[string]string
}

func newTriggerGit() *triggerGit {
	return &triggerGit{files: map[string]map[string][]byte{}, refs: map[string]string{}}
}

func (g *triggerGit) add(commit, path, body string) {
	if g.files[commit] == nil {
		g.files[commit] = map[string][]byte{}
	}
	g.files[commit][path] = []byte(body)
}

func (g *triggerGit) ListWorkflowFiles(_ context.Context, _, commit string) ([]string, error) {
	var out []string
	for p := range g.files[commit] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func (g *triggerGit) ReadFile(_ context.Context, _, commit, path string) ([]byte, error) {
	body, ok := g.files[commit][path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", threads.ErrNotFound, path)
	}
	return body, nil
}

func (g *triggerGit) ResolveRef(_ context.Context, _, ref string) (string, error) {
	sha, ok := g.refs[ref]
	if !ok {
		return "", fmt.Errorf("%w: %s", threads.ErrNotFound, ref)
	}
	return sha, nil
}

func triggerService(t *testing.T, g *triggerGit) *threads.Service {
	t.Helper()
	svc := threads.New(store.NewMemory(), nil)
	svc.Git = g
	return svc
}

// awaitRuns polls the store list until want runs exist (threadsRun detaches).
func awaitRuns(t *testing.T, svc *threads.Service, want int) []*threads.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader"}, "", 50)
		if err == nil && len(page.Runs) >= want {
			return page.Runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %d runs: last err %v, got %d", want, err, len(page.Runs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPullEventSinkCreatesPullRequestRun(t *testing.T) {
	sha := strings.Repeat("a", 40)
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/ci.yml", triggerWorkflow)
	svc := triggerService(t, g)
	sink := threadsPullEventOf(&collabWiring{threadsSvc: svc})
	if sink == nil {
		t.Fatal("nil sink with a service wired")
	}
	sink(context.Background(), pulls.PullEvent{
		Repo: "o/r", Action: "opened", Num: 7,
		BaseRef: "main", HeadRef: "feature", HeadSHA: sha, HeadRepo: "o/r", Actor: "ada",
	})
	runs := awaitRuns(t, svc, 1)
	run := runs[0]
	if run.Event != threads.EventPullReq || run.Commit != sha || run.Branch != "main" ||
		run.Ref != "refs/pull/7/head" || !run.Trusted || run.Actor != "ada" {
		t.Fatalf("run = %+v", run)
	}
	if run.WorkflowCommit != sha {
		t.Fatalf("same-repo PR must build the head workflow revision: %+v", run)
	}
}

func TestPullEventSinkForkUsesBaseWorkflowUntrusted(t *testing.T) {
	base := strings.Repeat("b", 40)
	head := strings.Repeat("c", 40)
	g := newTriggerGit()
	g.refs["refs/heads/main"] = base
	// Base workflow triggers; the fork head's file is different and must not
	// be read (trust class = base branch tip).
	g.add(base, ".spindle/workflows/ci.yml", triggerWorkflow)
	g.add(head, ".spindle/workflows/ci.yml", "name: Evil\non: pull_request\njobs:\n  x:\n    runs-on: linux\n    steps:\n      - run: rm -rf /\n")
	svc := triggerService(t, g)
	sink := threadsPullEventOf(&collabWiring{threadsSvc: svc})
	sink(context.Background(), pulls.PullEvent{
		Repo: "o/r", Action: "opened", Num: 3,
		BaseRef: "main", HeadRef: "feat", HeadSHA: head, HeadRepo: "f/r", Fork: true, Actor: "eve",
	})
	runs := awaitRuns(t, svc, 1)
	run := runs[0]
	if run.Trusted {
		t.Fatal("a fork PR run must be untrusted")
	}
	if run.Commit != head || run.WorkflowCommit != base {
		t.Fatalf("trust-class selection = commit %s workflow_commit %s", run.Commit, run.WorkflowCommit)
	}
	if run.Workflow != "CI" {
		t.Fatalf("base workflow not used: %+v", run)
	}
}

// TestPullEventSinkHonoursTypes pins the wiring: the sink forwards the pulls
// action into CreateRun, so a `types: [opened]` workflow runs for an opened PR
// and (because the action is required) never for an edge the file lists out.
func TestPullEventSinkHonoursTypes(t *testing.T) {
	sha := strings.Repeat("e", 40)
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/ci.yml", strings.Replace(triggerWorkflow,
		"on: [push, pull_request]",
		"on:\n  pull_request:\n    types: [opened]", 1))
	svc := triggerService(t, g)
	sink := threadsPullEventOf(&collabWiring{threadsSvc: svc})
	sink(context.Background(), pulls.PullEvent{
		Repo: "o/r", Action: "opened", Num: 9,
		BaseRef: "main", HeadRef: "feat", HeadSHA: sha, HeadRepo: "o/r", Actor: "ada",
	})
	runs := awaitRuns(t, svc, 1)
	if runs[0].WorkflowPath != ".spindle/workflows/ci.yml" {
		t.Fatalf("typed workflow did not run on opened: %+v", runs[0])
	}
	// A synchronize delivery for the same file creates nothing (poll a short
	// window: threadsRun detaches, so there is no completion signal).
	sink(context.Background(), pulls.PullEvent{
		Repo: "o/r", Action: "synchronize", Num: 9,
		BaseRef: "main", HeadRef: "feat", HeadSHA: sha, HeadRepo: "o/r", Actor: "ada",
	})
	time.Sleep(100 * time.Millisecond)
	if page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader"}, "", 50); err == nil && len(page.Runs) != 1 {
		t.Fatalf("types: [opened] created a synchronize run: %+v", page.Runs)
	}
}

func TestBranchPublishedSinkCreatesPushRun(t *testing.T) {
	sha := strings.Repeat("d", 40)
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/ci.yml", triggerWorkflow)
	svc := triggerService(t, g)
	sink := threadsBranchPublishedOf(&collabWiring{threadsSvc: svc})
	if sink == nil {
		t.Fatal("nil sink with a service wired")
	}
	sink(context.Background(), "o/r", "refs/heads/main", strings.Repeat("1", 40), sha)
	runs := awaitRuns(t, svc, 1)
	run := runs[0]
	if run.Event != threads.EventPush || run.Commit != sha || run.Branch != "main" || run.Actor != "merge" {
		t.Fatalf("merge push run = %+v", run)
	}
	// Non-branch refs and deletes are no-ops (same rule as the push hook).
	sink(context.Background(), "o/r", "refs/tags/v1", "", sha)
	sink(context.Background(), "o/r", "refs/heads/main", sha, strings.Repeat("0", 40))
	time.Sleep(50 * time.Millisecond)
	if page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader"}, "", 50); err == nil && len(page.Runs) != 1 {
		t.Fatalf("non-branch/delete publishes created runs: %+v", page.Runs)
	}
}

func TestThreadTriggerSinksNilWithoutWiring(t *testing.T) {
	if threadsPullEventOf(nil) != nil || threadsBranchPublishedOf(nil) != nil {
		t.Fatal("nil collab must yield nil trigger sinks")
	}
	if threadsPullEventOf(&collabWiring{}) != nil || threadsBranchPublishedOf(&collabWiring{}) != nil {
		t.Fatal("a wiring without a threads service must yield nil trigger sinks")
	}
}
