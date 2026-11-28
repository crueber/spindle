package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// The on-push Threads hook (docs/threads.md §6.1): branch create/update refs
// start the runs their on.push.branches globs match, tag create/update refs
// start the runs their on.push.tags globs match; notes and deletes start
// nothing.

func TestThreadsRefTarget(t *testing.T) {
	sha := strings.Repeat("a", 40)
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name string
		ref  server.PushedRef
		want string
		kind workflow.RefKind
		ok   bool
	}{
		{"branch create", server.PushedRef{Ref: "refs/heads/main", New: sha}, "main", workflow.RefBranch, true},
		{"branch delete", server.PushedRef{Ref: "refs/heads/main", New: zero}, "", workflow.RefBranch, false},
		{"tag create", server.PushedRef{Ref: "refs/tags/v1.2.3", New: sha}, "v1.2.3", workflow.RefTag, true},
		{"tag delete", server.PushedRef{Ref: "refs/tags/v1.2.3", New: zero}, "", workflow.RefBranch, false},
		{"notes", server.PushedRef{Ref: "refs/notes/commits", New: sha}, "", workflow.RefBranch, false},
		{"pull ref", server.PushedRef{Ref: "refs/pull/1/head", New: sha}, "", workflow.RefBranch, false},
		{"empty branch", server.PushedRef{Ref: "refs/heads/", New: sha}, "", workflow.RefBranch, false},
		{"empty tag", server.PushedRef{Ref: "refs/tags/", New: sha}, "", workflow.RefBranch, false},
		{"merge queue ref", server.PushedRef{Ref: "refs/heads/gh-readonly-queue/main/pr-7-" + sha, New: sha}, "", workflow.RefMergeQueue, false},
		{"malformed queue ref", server.PushedRef{Ref: "refs/heads/gh-readonly-queue/main/pr-0-" + sha, New: sha}, "", workflow.RefMergeQueue, false},
		{"empty oid", server.PushedRef{Ref: "refs/heads/main", New: ""}, "", workflow.RefBranch, false},
	}
	for _, tc := range cases {
		name, kind, ok := threadsRefTarget(tc.ref)
		if ok != tc.ok || name != tc.want || (ok && kind != tc.kind) {
			t.Errorf("%s: threadsRefTarget(%+v) = %q,%v,%v want %q,%v,%v",
				tc.name, tc.ref, name, kind, ok, tc.want, tc.kind, tc.ok)
		}
	}
}

func TestIsZeroOID(t *testing.T) {
	if !isZeroOID(strings.Repeat("0", 40)) || !isZeroOID(strings.Repeat("0", 64)) || !isZeroOID("") {
		t.Fatal("zero oids must be deletes")
	}
	if isZeroOID(strings.Repeat("a", 40)) {
		t.Fatal("a real oid is not zero")
	}
}

// TestOnPushHookNilWithoutWiring pins the nil-safe combination.
func TestOnPushHookNilWithoutWiring(t *testing.T) {
	if onPushHookOf(nil) != nil {
		t.Fatal("nil collab must yield a nil hook")
	}
	if onPushHookOf(&collabWiring{}) != nil {
		t.Fatal("a wiring without services must yield a nil hook")
	}
}

// compile-time seam assertions stay pinned in threads.go; restate the role
// seam so a signature drift in identity fails here too.
var _ threads.RoleService = (*identity.Service)(nil)

// TestJobTokenPushCreatesNoRun pins round 3's suppression rule at the Threads
// hook: a push marked byJob (a Threads job token inside `contents: write`)
// lands but creates no run — for a branch ref AND a tag ref — while the human
// push right after creates one run per ref.
func TestJobTokenPushCreatesNoRun(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/ci.yml", "name: CI\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo\n")
	g.refs["HEAD"] = sha
	g.refs["refs/heads/main"] = sha
	svc := triggerService(t, g)
	hook := threadsOnPushOf(&collabWiring{threadsSvc: svc})
	if hook == nil {
		t.Fatal("nil hook with a service wired")
	}
	id := git.RepoId{Owner: "o", Name: "r"}
	refs := []server.PushedRef{
		{Ref: "refs/heads/main", New: sha},
		{Ref: "refs/tags/v1.2.3", New: sha},
	}
	hook(id, refs, true)
	// Give the detached creator a chance to (wrongly) run: still zero.
	time.Sleep(150 * time.Millisecond)
	page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader", Write: true}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 0 {
		t.Fatalf("a job-token push created %d run(s)", len(page.Runs))
	}
	hook(id, refs, false)
	runs := awaitRuns(t, svc, 2)
	byRef := map[string]*threads.Run{}
	for _, run := range runs {
		if run.Event != threads.EventPush || run.Commit != sha || run.Actor != "push" {
			t.Fatalf("human push run = %+v", run)
		}
		byRef[run.Ref] = run
	}
	branch, tag := byRef["refs/heads/main"], byRef["refs/tags/v1.2.3"]
	if branch == nil || tag == nil {
		t.Fatalf("human push runs = %+v, want one run per ref", runs)
	}
	if branch.Branch != "main" {
		t.Fatalf("branch run Branch = %q", branch.Branch)
	}
	if tag.Branch != "" {
		t.Fatalf("tag run has a branch: %q", tag.Branch)
	}
}

// TestTagPushHookStartsTagWorkflow is the emitter half of PAR-010: a landed
// `refs/tags/v1.2.3` create creates the run of the `on.push.tags: ['v*']`
// workflow, with the full ref, no branch, and the unchanged push identity.
func TestTagPushHookStartsTagWorkflow(t *testing.T) {
	sha := "2222222222222222222222222222222222222222"
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/release.yml", "name: Release\non:\n  push:\n    tags: ['v*']\njobs:\n  publish:\n    runs-on: linux\n    steps:\n      - run: echo release\n")
	g.add(sha, ".spindle/workflows/ci.yml", "name: Main\non:\n  push:\n    branches: [main]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo build\n")
	svc := triggerService(t, g)
	hook := threadsOnPushOf(&collabWiring{threadsSvc: svc})
	hook(git.RepoId{Owner: "o", Name: "r"}, []server.PushedRef{{Ref: "refs/tags/v1.2.3", New: sha}}, false)
	runs := awaitRuns(t, svc, 1)
	run := runs[0]
	if run.WorkflowPath != ".spindle/workflows/release.yml" {
		t.Fatalf("tag push fired %q, want the tags workflow", run.WorkflowPath)
	}
	if run.Event != threads.EventPush || run.Commit != sha || run.Repo != "o/r" || run.Actor != "push" {
		t.Fatalf("tag run header = %+v", run)
	}
	if run.Ref != "refs/tags/v1.2.3" || run.Branch != "" {
		t.Fatalf("tag run Ref/Branch = %q/%q", run.Ref, run.Branch)
	}
}

// TestTagAndBranchPushSameNameCreateTwoRuns drives both refs through the hook:
// a branch and a tag with the same short name each start their own run, and the
// run records distinguish them (the supersede key reads the ref, prefix
// included, so neither replaces the other).
func TestTagAndBranchPushSameNameCreateTwoRuns(t *testing.T) {
	sha := "3333333333333333333333333333333333333333"
	g := newTriggerGit()
	g.add(sha, ".spindle/workflows/any.yml", "name: Any\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo\n")
	svc := triggerService(t, g)
	hook := threadsOnPushOf(&collabWiring{threadsSvc: svc})
	hook(git.RepoId{Owner: "o", Name: "r"}, []server.PushedRef{
		{Ref: "refs/heads/v1.2.3", New: sha},
		{Ref: "refs/tags/v1.2.3", New: sha},
	}, false)
	runs := awaitRuns(t, svc, 2)
	byRef := map[string]*threads.Run{}
	for _, run := range runs {
		byRef[run.Ref] = run
	}
	branch, tag := byRef["refs/heads/v1.2.3"], byRef["refs/tags/v1.2.3"]
	if branch == nil || tag == nil || branch.ID == tag.ID {
		t.Fatalf("runs = %+v, want independent branch and tag runs", runs)
	}
	if branch.Branch != "v1.2.3" || tag.Branch != "" {
		t.Fatalf("branch/tag Branch = %q/%q", branch.Branch, tag.Branch)
	}
}

// mergeGroupFixture is the merge-queue emitter half of PAR-016: a base branch
// with a merge_group workflow and an ordinary push workflow, so one queue-ref
// push must produce exactly one run (the merge_group one).
func mergeGroupFixture(t *testing.T) (*triggerGit, *threads.Service, string, string) {
	t.Helper()
	base := "4444444444444444444444444444444444444444"
	head := "5555555555555555555555555555555555555555"
	g := newTriggerGit()
	g.refs["refs/heads/main"] = base
	g.add(base, ".spindle/workflows/queue.yml", `name: Queue
on:
  merge_group:
    types: [checks_requested]
jobs:
  queue-checks:
    runs-on: linux
    steps:
      - run: echo "checking ${{ github.event.merge_group.head_sha }}"
`)
	g.add(base, ".spindle/workflows/push.yml", `name: Push
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`)
	return g, triggerService(t, g), base, head
}

// TestMergeGroupPushHookStartsOneRun is PAR-016's emitter: a landed
// `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` push creates exactly one
// merge_group run — not a push run — with the queue ref, the queue head commit,
// the base branch's workflow and the documented payload.
func TestMergeGroupPushHookStartsOneRun(t *testing.T) {
	_, svc, base, head := mergeGroupFixture(t)
	hook := threadsOnPushOf(&collabWiring{threadsSvc: svc})
	ref := "refs/heads/gh-readonly-queue/main/pr-7-" + head
	hook(git.RepoId{Owner: "o", Name: "r"}, []server.PushedRef{{Ref: ref, Old: strings.Repeat("0", 40), New: head}}, false)

	runs := awaitRuns(t, svc, 1)
	// Give a wrongly-fired push/create run the same window the sibling tests
	// use; exactly one run is the assertion.
	time.Sleep(150 * time.Millisecond)
	page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader"}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("queue push created %d runs (%+v), want exactly the merge_group run", len(page.Runs), page.Runs)
	}
	run := runs[0]
	if run.Event != threads.EventMergeGroup || run.Commit != head || run.Ref != ref || run.Branch != "" {
		t.Fatalf("merge_group run header = %+v", run)
	}
	if run.WorkflowPath != ".spindle/workflows/queue.yml" || run.WorkflowCommit != base {
		t.Fatalf("base-branch workflow selection = %+v", run)
	}
	fields := threads.EventFieldsOf(run.EventPayload)
	want := map[string]string{
		"event.action":               "checks_requested",
		"event.number":               "7",
		"event.merge_group.head_ref": ref,
		"event.merge_group.head_sha": head,
		"event.merge_group.base_ref": "refs/heads/main",
		"event.merge_group.base_sha": base,
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %q, want %q", k, fields[k], v)
		}
	}
}

// TestMergeGroupPushHookIgnoresOtherRefs pins the other half of PAR-016: an
// ordinary branch push, a tag push and a malformed queue-shaped ref create no
// merge_group run (the malformed one creates nothing at all, not a push run).
func TestMergeGroupPushHookIgnoresOtherRefs(t *testing.T) {
	_, svc, base, head := mergeGroupFixture(t)
	hook := threadsOnPushOf(&collabWiring{threadsSvc: svc})
	malformed := "refs/heads/gh-readonly-queue/main/pr-0-" + head
	deleted := "refs/heads/gh-readonly-queue/main/pr-8-" + head
	hook(git.RepoId{Owner: "o", Name: "r"}, []server.PushedRef{
		{Ref: "refs/heads/main", Old: base, New: base},
		{Ref: "refs/tags/v1.2.3", Old: base, New: base},
		{Ref: malformed, New: head},
		{Ref: deleted, Old: head, New: strings.Repeat("0", 40)},
	}, false)
	// The two real pushes create push runs; the malformed queue ref must
	// create none. Wait for the pushes, then inspect the whole page.
	awaitRuns(t, svc, 2)
	time.Sleep(100 * time.Millisecond)
	page, err := svc.ListRuns(context.Background(), "o/r", auth.Principal{Name: "reader"}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range page.Runs {
		if run.Event == threads.EventMergeGroup {
			t.Fatalf("a non-queue ref created a merge_group run: %+v", run)
		}
		if run.Ref == malformed {
			t.Fatalf("a malformed queue ref created a %s run: %+v", run.Event, run)
		}
		if run.Ref == deleted {
			t.Fatalf("a queue ref delete created a %s run: %+v", run.Event, run)
		}
	}
}
