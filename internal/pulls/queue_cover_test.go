package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestQueueCandidateErrors covers the enqueue read's failure arms.
func TestQueueCandidateErrors(t *testing.T) {
	e := newTestEnv()
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing thread = %v", err)
	}
	openBasic(t, e, "o", "r")
	// A thread whose pr.json vanished is not a PR.
	if err := e.store.Delete(ctx(), PRKey("o", "r", 1), ""); err != nil {
		t.Fatalf("delete pr: %v", err)
	}
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pr = %v", err)
	}
	// An issue number is not a pull request.
	th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
	th.Kind = "issue"
	th.NextEventSeq++
	if _, err := store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), encodeThread(th), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("write thread: %v", err)
	}
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue = %v", err)
	}
}

func TestPrepareQueueMergeErrors(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))

	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 9, queueMaintainer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// No git backend.
	saved := e.svc.Git
	e.svc.Git = nil
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no git = %v", err)
	}
	e.svc.Git = saved
	// A dirs failure.
	e.dirs.UnknownErr = errors.New("dirs down")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dirs = %v", err)
	}
	e.dirs.UnknownErr = nil
	// Unknown base/head refs.
	e.delGitRef("o/r", "refs/heads/main")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("base ref = %v", err)
	}
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.delGitRef("o/r", "refs/heads/topic")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("head ref = %v", err)
	}
	e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(2)})
	// A merged PR refuses.
	markMergedPR(t, e, "o", "r", 1)
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("merged = %v", err)
	}
	// A closed thread refuses (fresh env).
	e = newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("closed = %v", err)
	}
}

func TestPrepareQueueMergePolicyErrors(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	// Corrupt policy fails closed.
	putPolicyRaw(t, e, `{"version":1,"rules":[{"name":"x"]}`)
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupt policy = %v", err)
	}
	// A merge_methods set without merge refuses.
	putPolicy(t, e, `{"version":1,"rules":[{"name":"sq","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"merge_methods":["squash"]}}}]}`)
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("method = %v", err)
	}
	// require_up_to_date with a behind head refuses.
	putPolicy(t, e, `{"version":1,"rules":[{"name":"up","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_up_to_date":true}}}]}`)
	e.git.Ancestors = map[string]bool{}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "behind") {
		t.Fatalf("up-to-date = %v", err)
	}
	// IsAncestor outage.
	e.git.IsAncestorErr = errors.New("ancestry down")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("ancestry outage must fail")
	}
}

func TestPrepareQueueMergeGitErrors(t *testing.T) {
	setup := func() *testEnv {
		e := newTestEnv()
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		return e
	}
	// A non-conflict trial failure propagates.
	e := setup()
	e.git.TrialErr = errors.New("git exploded")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("trial error")
	}
	// commit-tree failure.
	e = setup()
	e.git.CommitErr = errors.New("commit down")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("commit error")
	}
	// A publish failure names the queue ref.
	e = setup()
	e.refs.UpdateErr = errors.New("publish down")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "publish queue ref") {
		t.Fatalf("publish = %v", err)
	}
	// The fork bridge failure.
	e = setup()
	th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
	_ = th
	pr, prVer, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	pr.Head.Repo = "other/fork"
	if err := e.svc.savePR(ctx(), "o", "r", pr, prVer); err != nil {
		t.Fatalf("save pr: %v", err)
	}
	e.seedRefs("other/fork", map[string]string{"refs/heads/topic": hexSHA(2)})
	e.git.FetchErr = errors.New("bridge down")
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "fork object bridge") {
		t.Fatalf("bridge = %v", err)
	}
}

func TestDeleteQueueRefErrorArms(t *testing.T) {
	e := newTestEnv()
	ref := "refs/heads/gh-readonly-queue/main/pr-1-" + hexSHA(2)
	// A precondition (absent) is success.
	e.refs.DeleteErr = store.NewPrecondition("k", "v")
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", ref); err != nil {
		t.Fatalf("precondition = %v", err)
	}
	// A real failure propagates.
	e.refs.DeleteErr = errors.New("bucket down")
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", ref); err == nil {
		t.Fatal("delete failure")
	}
}

func TestLandQueuedMergeJoinAndCancel(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	qm := &QueueMerge{BaseSHA: hexSHA(1), HeadSHA: hexSHA(2), MergeSHA: hexSHA(7), Strategy: StrategyMerge}

	// A human merge holds the (repo, pull-merge) slot: the queue waits, then
	// takes its turn and lands its own PR.
	held, _ := e.svc.tasks.begin("o/r", TaskKindMerge)
	held.rec.initMerge(99, StrategyMerge)
	done := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		e.svc.tasks.end("o/r", TaskKindMerge)
		close(done)
	}()
	rec, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), qm)
	if err != nil {
		t.Fatalf("land after join: %v", err)
	}
	<-done
	if rec == nil || rec.State != TaskOK {
		t.Fatalf("record = %+v", rec)
	}
	// A canceled context while joined returns promptly.
	held, _ = e.svc.tasks.begin("o/r", TaskKindMerge)
	held.rec.initMerge(98, StrategyMerge)
	defer e.svc.tasks.end("o/r", TaskKindMerge)
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	if _, err := e.svc.LandQueuedMerge(cctx, "o", "r", 1, queueMaintainer(), qm); err == nil {
		t.Fatal("canceled join must return an error")
	}
}

func TestQueueStoreReadErrors(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	e.failGet(ThreadKey("o", "r", 1), errors.New("threads down"))
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer()); err == nil {
		t.Fatal("QueueCandidate thread outage")
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("Prepare thread outage")
	}
	e.clearFails()
	e.failGet(PRKey("o", "r", 1), errors.New("prs down"))
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer()); err == nil {
		t.Fatal("QueueCandidate pr outage")
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("Prepare pr outage")
	}
}

func TestLandQueuedMergeJoinSeesAlreadyMerged(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	markMergedPR(t, e, "o", "r", 1)
	held, _ := e.svc.tasks.begin("o/r", TaskKindMerge)
	held.rec.initMerge(99, StrategyMerge)
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.svc.tasks.end("o/r", TaskKindMerge)
	}()
	rec, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), &QueueMerge{
		BaseSHA: hexSHA(1), HeadSHA: hexSHA(2), MergeSHA: hexSHA(7), Strategy: StrategyMerge,
	})
	if err != nil {
		t.Fatalf("land joined merged: %v", err)
	}
	if rec == nil {
		t.Fatal("no record")
	}
}

func TestPrepareQueueMergeUpToDateAllowed(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	putPolicy(t, e, `{"version":1,"rules":[{"name":"up","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_up_to_date":true}}}]}`)
	e.git.Ancestors[hexSHA(1)+"\x00"+hexSHA(2)] = true
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err != nil {
		t.Fatalf("up-to-date contained head: %v", err)
	}
}

func TestPrepareQueueMergeTrialFailure(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	// The mergeability recompute's trial is clean; the queue's own trial is
	// dirty, so the queue names the conflicts (the task's own second sight).
	e.git.TrialQueue = []TrialOutcome{
		{Tree: "4444444444444444444444444444444444444444"},
		{Conflicts: []string{"a.txt"}, Err: errDirty},
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("trial dirty = %v", err)
	}
	// A non-dirty trial failure propagates.
	e.git.TrialQueue = []TrialOutcome{
		{Tree: "4444444444444444444444444444444444444444"},
		{Err: errors.New("git exploded")},
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("trial error")
	}
}
