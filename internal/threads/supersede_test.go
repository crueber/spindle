// Implicit supersede (PAR-150, docs/threads.md §3.1a): a new push to a ref, and
// a PR synchronize, cancel the in-flight run of the commit they supersede when
// the workflow declares no concurrency group. The reconciliation with
// `concurrency` (PAR-152) is tested here too: a run that declares a group is
// never implicitly cancelled, and a group-free run never cancels it either.
package threads

import (
	"context"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// createPush creates the push run for (sha, branch) through the public path.
func (e *testEnv) createPush(t *testing.T, sha, branch string) []*Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{
		Branch: branch, Ref: "refs/heads/" + branch, Actor: "writer",
	})
	if err != nil {
		t.Fatalf("CreateRun(%s,%s): %v", branch, sha, err)
	}
	return runs
}

// createPR creates the pull_request run for one lifecycle edge on PR 7.
func (e *testEnv) createPR(t *testing.T, sha, action string) []*Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPullReq, CreateOpts{
		Branch: "main", Action: action, Ref: "refs/pull/7/head",
		BaseBranch: "main", Actor: "writer",
	})
	if err != nil {
		t.Fatalf("CreateRun(pr %s): %v", action, err)
	}
	return runs
}

// mustRun reads one run header or fails.
func (e *testEnv) mustRun(t *testing.T, id string) *Run {
	t.Helper()
	run, err := e.svc.loadRun(ctx(), "o", "r", id)
	if err != nil || run == nil {
		t.Fatalf("loadRun(%s): %v (nil=%v)", id, err, run == nil)
	}
	return run
}

// live reports a run that can still execute.
func live(r *Run) bool { return r != nil && !terminal(r.State) }

func TestSupersedeSameRefCancelsTheOlderRun(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", independentWorkflowYAML)

	first := e.createPush(t, testSHA, "main")
	if len(first) != 1 {
		t.Fatalf("runs = %d", len(first))
	}
	if got := e.mustRun(t, first[0].ID); got.State != RunQueued {
		t.Fatalf("first push state = %s", got.State)
	}

	next := e.createPush(t, second, "main")
	if len(next) != 1 {
		t.Fatalf("runs = %d", len(next))
	}
	old := e.mustRun(t, first[0].ID)
	if old.State != RunCancelled {
		t.Fatalf("superseded run state = %s, want cancelled", old.State)
	}
	if old.CancelReason != CancelReasonSupersede {
		t.Fatalf("cancel reason = %q", old.CancelReason)
	}
	if old.SupersededBy != next[0].ID {
		t.Fatalf("superseded_by = %q, want %q", old.SupersededBy, next[0].ID)
	}
	if old.CompletedAt == nil {
		t.Fatal("superseded run has no completed_at")
	}
	// The new run is untouched and is the one live run of the ref.
	cur := e.mustRun(t, next[0].ID)
	if !live(cur) {
		t.Fatalf("new run state = %s, want live", cur.State)
	}
	// Its queued jobs carry the reason the page shows.
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.State != JobCancelled || j.CancelRequested {
			t.Fatalf("job %s state=%s cancel=%v", j.ID, j.State, j.CancelRequested)
		}
		if j.Reason != "superseded by run "+next[0].ID {
			t.Fatalf("job %s reason = %q", j.ID, j.Reason)
		}
	}
	// The checks mirror sees the cancellation for the superseded commit.
	found := false
	for _, r := range e.checks.reports {
		if r.SHA == testSHA && r.State == "cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("checks never mirrored a cancelled state for the superseded sha: %+v", e.checks.reports)
	}
}

func TestSupersedeLeavesOtherRefsAndWorkflowsAlone(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	second := "3333333333333333333333333333333333333333"
	third := "4444444444444444444444444444444444444444"
	for _, sha := range []string{second, third} {
		e.git.add(sha, ".spindle/workflows/ci.yml", independentWorkflowYAML)
	}
	// A second workflow file at every commit: a different file must own its own
	// head.
	otherYAML := "name: Other\non:\n  push:\njobs:\n  one:\n    runs-on: linux\n    steps:\n      - run: echo one\n"
	for _, sha := range []string{testSHA, second, third} {
		e.git.add(sha, ".spindle/workflows/other.yml", otherYAML)
	}

	mainRuns := e.createPush(t, testSHA, "main")
	if len(mainRuns) != 2 {
		t.Fatalf("runs = %d (want one per workflow file)", len(mainRuns))
	}
	// A push on ANOTHER branch cancels nothing on main. Only other.yml fires
	// there (ci.yml filters on main), and its feature-branch head is a different
	// key from its main head.
	featureRuns := e.createPush(t, second, "feature")
	if len(featureRuns) != 1 {
		t.Fatalf("runs = %d (want the unfiltered workflow only)", len(featureRuns))
	}
	for _, r := range mainRuns {
		if got := e.mustRun(t, r.ID); !live(got) {
			t.Fatalf("run %s (%s) was cancelled by another ref: %s", got.ID, got.WorkflowPath, got.State)
		}
	}
	// A push on main supersedes main's run of THAT file only.
	nextRuns := e.createPush(t, third, "main")
	if len(nextRuns) != 2 {
		t.Fatalf("runs = %d", len(nextRuns))
	}
	for _, old := range mainRuns {
		var match *Run
		for _, n := range nextRuns {
			if n.WorkflowPath == old.WorkflowPath {
				match = n
			}
		}
		if match == nil {
			t.Fatalf("no successor for %s", old.WorkflowPath)
		}
		got := e.mustRun(t, old.ID)
		if got.CancelReason != CancelReasonSupersede || got.SupersededBy != match.ID {
			t.Fatalf("%s: state=%s reason=%q by=%q (want superseded by %s)",
				old.WorkflowPath, got.State, got.CancelReason, got.SupersededBy, match.ID)
		}
	}
	// And the feature-branch runs stay live.
	for _, r := range featureRuns {
		if got := e.mustRun(t, r.ID); !live(got) {
			t.Fatalf("feature run %s was cancelled: %s", r.ID, got.State)
		}
	}
}

// prWorkflowYAML fires on push and on every pull_request edge, so the PR
// lifecycle tests can create runs on refs/pull/<n>/head.
const prWorkflowYAML = `name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

func TestSupersedePRSynchronizeOnly(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", prWorkflowYAML)
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", prWorkflowYAML)

	opened := e.createPR(t, testSHA, "opened")
	if len(opened) != 1 {
		t.Fatalf("runs = %d", len(opened))
	}
	// A synchronize on the PR head supersedes the opened run.
	sync := e.createPR(t, second, "synchronize")
	if len(sync) != 1 {
		t.Fatalf("runs = %d", len(sync))
	}
	old := e.mustRun(t, opened[0].ID)
	if old.State != RunCancelled || old.SupersededBy != sync[0].ID {
		t.Fatalf("opened run after synchronize: state=%s by=%q", old.State, old.SupersededBy)
	}
	// Another `opened`-class edge (reopened) does NOT implicitly supersede: it is
	// not the head moving.
	third := "4444444444444444444444444444444444444444"
	e.git.add(third, ".spindle/workflows/ci.yml", prWorkflowYAML)
	reopened := e.createPR(t, third, "reopened")
	if len(reopened) != 1 {
		t.Fatalf("runs = %d", len(reopened))
	}
	if got := e.mustRun(t, sync[0].ID); !live(got) {
		t.Fatalf("synchronize run was cancelled by a reopened edge: %s", got.State)
	}
}

func TestSupersedeSkipsRunsWithAConcurrencyGroup(t *testing.T) {
	groupYAML := `name: CI
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: linux
    concurrency:
      group: deploy-${{ github.ref }}
    steps:
      - run: echo deploy
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", groupYAML)
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", groupYAML)

	first := e.createPush(t, testSHA, "main")
	if len(first) != 1 || len(first[0].Concurrency) == 0 {
		t.Fatalf("fixture did not declare a group: %+v", first)
	}
	next := e.createPush(t, second, "main")
	if len(next) != 1 {
		t.Fatalf("runs = %d", len(next))
	}
	// The older run manages its own group (PAR-152): supersede must not cancel it,
	// or `cancel-in-progress: false` would be a lie.
	if got := e.mustRun(t, first[0].ID); !live(got) {
		t.Fatalf("grouped run was implicitly cancelled: state=%s reason=%q", got.State, got.CancelReason)
	}
	// And no head record was written for a grouped workflow at all.
	if _, ver, err := e.svc.getJSON(ctx(), SupersedeKey("o", "r", ".spindle/workflows/ci.yml", "refs/heads/main")); err != nil || ver != "" {
		t.Fatalf("a grouped workflow adopted a supersede head (ver=%q err=%v)", ver, err)
	}
}

func TestSupersedeNewGroupedRunLeavesTheOlderGroupFreeRunAlone(t *testing.T) {
	freeYAML := independentWorkflowYAML
	groupYAML := `name: CI
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    concurrency: ci
    steps:
      - run: echo build
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", freeYAML)
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", groupYAML)

	first := e.createPush(t, testSHA, "main")
	if len(first) != 1 {
		t.Fatalf("runs = %d", len(first))
	}
	e.createPush(t, second, "main")
	// The newer run declares a group, so it does not adopt the head and does not
	// cancel the older run: the group is the workflow's serialisation choice.
	if got := e.mustRun(t, first[0].ID); !live(got) {
		t.Fatalf("older group-free run was cancelled by a grouped successor: %s", got.State)
	}
}

func TestSupersedeLeavesTerminalRunsAndOtherEventsAlone(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", independentWorkflowYAML)

	// A finished run is not "in flight": a later push leaves it green.
	done := e.createPush(t, testSHA, "main")
	for _, j := range mustJobs(t, e, done[0].ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", done[0].ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.mustRun(t, done[0].ID); got.State != RunSuccess {
		t.Fatalf("fixture run state = %s", got.State)
	}
	e.createPush(t, second, "main")
	if got := e.mustRun(t, done[0].ID); got.State != RunSuccess || got.CancelReason != "" {
		t.Fatalf("terminal run was rewritten: state=%s reason=%q", got.State, got.CancelReason)
	}

	// A dispatch does not supersede anything, even on the same ref.
	third := "4444444444444444444444444444444444444444"
	e.git.add(third, ".spindle/workflows/ci.yml", independentWorkflowYAML)
	queued := e.createPush(t, third, "main")
	if _, err := e.svc.CreateRun(ctx(), "o/r", third, EventDispatch, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml", Actor: "writer",
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.mustRun(t, queued[0].ID); !live(got) {
		t.Fatalf("dispatch cancelled a push run: %s", got.State)
	}
}

func TestSupersedeCancelsPendingApprovalPredecessor(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", prWorkflowYAML)
	e.svc.Approvals = forkApprovals{}
	e.svc.ApproveFirstTimeForks = true
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", independentWorkflowYAML)

	held, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "synchronize", Ref: "refs/pull/7/head",
		BaseBranch: "main", Fork: true, BaseCommit: testSHA, Actor: "stranger",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].State != RunPendingApproval {
		t.Fatalf("fixture did not hold the fork run: %+v", held)
	}
	e.git.add(second, ".spindle/workflows/ci.yml", prWorkflowYAML)
	e.createPR(t, second, "synchronize")
	if got := e.mustRun(t, held[0].ID); got.State != RunCancelled {
		t.Fatalf("held run state = %s, want cancelled (a newer head supersedes it)", got.State)
	}
}

// forkApprovals reports no merged pull and no maintainer: the first-time gate
// holds every unknown contributor.
type forkApprovals struct{}

func (forkApprovals) MergedAuthor(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (forkApprovals) Maintainer(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func TestCancelIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.createPush(t, testSHA, "main")[0]

	first, err := e.svc.cancelRun(ctx(), "o", "r", run.ID, cancelWhy{Reason: CancelReasonSupersede, SupersededBy: "9999999999999999999-abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != RunCancelled || first.CancelReason != CancelReasonSupersede {
		t.Fatalf("first cancel: state=%s reason=%q", first.State, first.CancelReason)
	}
	completed := first.CompletedAt
	// A second cancel with a DIFFERENT reason changes nothing: the first reason
	// wins and the timings stay put (a retried cancel must not relabel history).
	second, err := e.svc.cancelRun(ctx(), "o", "r", run.ID, cancelWhy{Actor: "maint", Reason: CancelReasonOperator})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != RunCancelled || second.CancelReason != CancelReasonSupersede || second.CancelledBy != "" {
		t.Fatalf("second cancel rewrote the record: %+v", second)
	}
	if completed == nil || second.CompletedAt == nil || *completed != *second.CompletedAt {
		t.Fatalf("completed_at moved: %v -> %v", completed, second.CompletedAt)
	}
	// And the operator path through CancelRun is equally idempotent.
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, e.writer()); err != nil {
		t.Fatal(err)
	}
	if got := e.mustRun(t, run.ID); got.CancelReason != CancelReasonSupersede {
		t.Fatalf("CancelRun relabelled a superseded run: %q", got.CancelReason)
	}
}

func TestAdoptRunHeadIsForwardOnly(t *testing.T) {
	e := newTestEnv(t)
	older := "0000000000000000001-aaaaaa"
	middle := "0000000000000000002-bbbbbb"
	newest := "0000000000000000003-cccccc"
	key := SupersedeKey("o", "r", ".spindle/workflows/ci.yml", "refs/heads/main")

	prev, newer, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", older, testSHA)
	if err != nil || prev != "" || newer != "" {
		t.Fatalf("first adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
	prev, newer, err = e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", middle, testSHA)
	if err != nil || prev != older || newer != "" {
		t.Fatalf("second adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
	// An OLDER run adopting after a newer one is told it lost — the rule that
	// closes the read-then-cancel window.
	prev, newer, err = e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", older, testSHA)
	if err != nil || prev != "" || newer != middle {
		t.Fatalf("stale adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
	// A repeat of the current head is a no-op (a retried creation).
	prev, newer, err = e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", middle, testSHA)
	if err != nil || prev != "" || newer != "" {
		t.Fatalf("repeat adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
	prev, newer, err = e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", newest, testSHA)
	if err != nil || prev != middle || newer != "" {
		t.Fatalf("third adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
	raw, _, gerr := e.svc.getJSON(ctx(), key)
	if gerr != nil || raw == nil {
		t.Fatalf("head record: %v (nil=%v)", gerr, raw == nil)
	}
}

func TestSupersedeSelfWhenANewerHeadAlreadyExists(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	// Plant a head far in the future: any run this test creates is the older
	// side of the pair, so it must cancel itself and name the planted run.
	future := "9999999999999999999-ffffff"
	if _, _, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", future, testSHA); err != nil {
		t.Fatal(err)
	}
	runs := e.createPush(t, testSHA, "main")
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].State != RunCancelled {
		t.Fatalf("the loser stayed %s, want cancelled", runs[0].State)
	}
	if runs[0].CancelReason != CancelReasonSupersede || runs[0].SupersededBy != future {
		t.Fatalf("loser: reason=%q by=%q", runs[0].CancelReason, runs[0].SupersededBy)
	}
}

// TestSupersedeConcurrentCreationsLeaveOneLiveRun is the -race scenario: eight
// creations for one ref in parallel must converge on exactly one live run, each
// loser naming the winner. No sleeps: the assertion is over the store state the
// creations themselves wrote.
func TestSupersedeConcurrentCreationsLeaveOneLiveRun(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
				Branch: "main", Ref: "refs/heads/main", Actor: "writer",
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}
	ids, err := e.svc.runIDs(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != n {
		t.Fatalf("runs = %d, want %d", len(ids), n)
	}
	var liveIDs []string
	for _, id := range ids {
		run := e.mustRun(t, id)
		if live(run) {
			liveIDs = append(liveIDs, id)
			continue
		}
		if run.State != RunCancelled {
			t.Fatalf("run %s ended %s, want cancelled", id, run.State)
		}
		if run.CancelReason != CancelReasonSupersede {
			t.Fatalf("run %s cancelled as %q", id, run.CancelReason)
		}
		if run.SupersededBy == "" || run.SupersededBy == id {
			t.Fatalf("run %s superseded_by = %q", id, run.SupersededBy)
		}
	}
	if len(liveIDs) != 1 {
		t.Fatalf("%d live runs (%v), want exactly 1", len(liveIDs), liveIDs)
	}
	// Every loser names a run that exists: the chain is coherent, not a ghost.
	for _, id := range ids {
		run := e.mustRun(t, id)
		if run.SupersededBy != "" {
			if other := e.mustRun(t, run.SupersededBy); other.ID != run.SupersededBy {
				t.Fatalf("run %s names a missing successor %s", id, run.SupersededBy)
			}
		}
	}
	// The head names the live run.
	head, _, err := e.svc.getJSON(ctx(), SupersedeKey("o", "r", ".spindle/workflows/ci.yml", "refs/heads/main"))
	if err != nil || head == nil {
		t.Fatalf("head record missing: %v", err)
	}
	var doc SupersedeDoc
	if err := jsonUnmarshal(head, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.RunID != liveIDs[0] {
		t.Fatalf("head = %s, live run = %s", doc.RunID, liveIDs[0])
	}
}

// TestSupersedeKeysOnTheFullRef pins the tag rule (wave 1a's emitter + PAR-150):
// the supersede pair is (workflow file, FULL ref), so a branch and a tag of the
// same short name never cancel each other, while a re-pushed tag supersedes its
// own previous tag run.
func TestSupersedeKeysOnTheFullRef(t *testing.T) {
	anyRef := `name: CI
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", anyRef)
	second := "3333333333333333333333333333333333333333"
	e.git.add(second, ".spindle/workflows/ci.yml", anyRef)

	branchRuns, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		Branch: "v1", Ref: "refs/heads/v1", RefName: "v1", Actor: "writer",
	})
	if err != nil || len(branchRuns) != 1 {
		t.Fatalf("branch run: %v (%d)", err, len(branchRuns))
	}
	tagRuns, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		Ref: "refs/tags/v1", RefName: "v1", RefKind: workflow.RefTag, Actor: "writer",
	})
	if err != nil || len(tagRuns) != 1 {
		t.Fatalf("tag run: %v (%d)", err, len(tagRuns))
	}
	if tagRuns[0].Branch != "" || tagRuns[0].Ref != "refs/tags/v1" {
		t.Fatalf("tag run ref=%q branch=%q", tagRuns[0].Ref, tagRuns[0].Branch)
	}
	// The tag did not cancel the branch run of the same short name.
	if got := e.mustRun(t, branchRuns[0].ID); !live(got) {
		t.Fatalf("a tag run cancelled the branch run of the same name: %s", got.State)
	}
	// A re-pushed tag supersedes its own previous tag run.
	again, err := e.svc.CreateRun(ctx(), "o/r", second, EventPush, CreateOpts{
		Ref: "refs/tags/v1", RefName: "v1", RefKind: workflow.RefTag, Actor: "writer",
	})
	if err != nil || len(again) != 1 {
		t.Fatalf("second tag run: %v (%d)", err, len(again))
	}
	old := e.mustRun(t, tagRuns[0].ID)
	if old.State != RunCancelled || old.SupersededBy != again[0].ID {
		t.Fatalf("re-pushed tag: state=%s by=%q (want cancelled by %s)", old.State, old.SupersededBy, again[0].ID)
	}
	if got := e.mustRun(t, branchRuns[0].ID); !live(got) {
		t.Fatalf("the branch run was disturbed by tag traffic: %s", got.State)
	}
}
