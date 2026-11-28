package pulls

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func queueMaintainer() auth.Principal { return auth.Principal{Name: "merger@example.com"} }

func TestQueueRefName(t *testing.T) {
	got := QueueRefName("refs/heads/release/1.0", 7, strings.Repeat("A", 40))
	want := "refs/heads/gh-readonly-queue/release/1.0/pr-7-" + strings.Repeat("a", 40)
	if got != want {
		t.Fatalf("QueueRefName = %q, want %q", got, want)
	}
}

func TestQueueCandidate(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	c, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, writer())
	if err != nil {
		t.Fatalf("QueueCandidate: %v", err)
	}
	if c.Num != 1 || c.BaseRef != "refs/heads/main" || c.HeadRef != "refs/heads/topic" || c.Author == "" || c.Merged || c.Draft {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 2, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := e.svc.QueueCandidate(ctx(), "o", "r", 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
}

func TestPrepareQueueMergePublishesDocumentedRef(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	qm, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer())
	if err != nil {
		t.Fatalf("PrepareQueueMerge: %v", err)
	}
	if qm.Ref != QueueRefName("refs/heads/main", 1, hexSHA(2)) {
		t.Fatalf("ref = %q", qm.Ref)
	}
	if qm.BaseSHA != hexSHA(1) || qm.HeadSHA != hexSHA(2) || qm.Strategy != StrategyMerge {
		t.Fatalf("merge = %+v", qm)
	}
	if len(qm.MergeSHA) != 40 {
		t.Fatalf("merge sha = %q", qm.MergeSHA)
	}
	if got := e.refs.Refs["o/r"][qm.Ref]; got != qm.MergeSHA {
		t.Fatalf("published ref = %q, want merge sha %q", got, qm.MergeSHA)
	}
	// The objects ride the same publish (a queue ref with no objects would
	// give the merge_group run an unfetchable commit).
	calls := e.refs.updatesFor(qm.Ref)
	if len(calls) != 1 || calls[0].Pack == "" {
		t.Fatalf("publish calls = %+v", calls)
	}
	// A caller without maintain is refused (the landing floor).
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer = %v", err)
	}
}

func TestPrepareQueueMergeRefusals(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	// Dirty merge.
	e.git.TrialErr = errDirty
	e.git.TrialConflicts = []string{"a.txt"}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("dirty = %v", err)
	}
	e.git.TrialErr = nil
	// Up to date.
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(1)] = true
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); err == nil {
		t.Fatal("up-to-date should refuse")
	}
	e.git.Ancestors = map[string]bool{}
	// A draft PR is refused.
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(p *PRDoc) (bool, error) {
		p.Draft = true
		return true, nil
	}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := e.svc.PrepareQueueMerge(ctx(), "o", "r", 1, queueMaintainer()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("draft = %v", err)
	}
}

func TestLandQueuedMergeRequiresPreparedParents(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	// Base moved.
	if _, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), &QueueMerge{
		BaseSHA: hexSHA(9), HeadSHA: hexSHA(2), MergeSHA: hexSHA(7), Strategy: StrategyMerge,
	}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "base moved") {
		t.Fatalf("moved base = %v", err)
	}
	// Head moved.
	if _, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), &QueueMerge{
		BaseSHA: hexSHA(1), HeadSHA: hexSHA(9), MergeSHA: hexSHA(7), Strategy: StrategyMerge,
	}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "head moved") {
		t.Fatalf("moved head = %v", err)
	}
	if _, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil merge = %v", err)
	}
}

func TestLandQueuedMergeGatesOnQueueHead(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	gate := &fakeChecksGate{}
	e.svc.Checks = gate
	queueHead := hexSHA(7)
	rec, err := e.svc.LandQueuedMerge(ctx(), "o", "r", 1, queueMaintainer(), &QueueMerge{
		BaseSHA: hexSHA(1), HeadSHA: hexSHA(2), MergeSHA: queueHead, Strategy: StrategyMerge,
	})
	if err != nil {
		t.Fatalf("LandQueuedMerge: %v", err)
	}
	if rec == nil || rec.State != TaskOK {
		t.Fatalf("task = %+v", rec)
	}
	if gate.lastHead != queueHead {
		t.Fatalf("gate head = %q, want the queue head %q", gate.lastHead, queueHead)
	}
	updates := e.refs.updatesFor("refs/heads/main")
	if len(updates) != 1 {
		t.Fatalf("base updates = %+v", updates)
	}
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	if !pr.Merged {
		t.Fatal("PR not merged")
	}
}

func TestDeleteQueueRef(t *testing.T) {
	e := newTestEnv()
	e.refs.Refs["o/r"] = map[string]string{
		"refs/heads/gh-readonly-queue/main/pr-1-" + hexSHA(2): hexSHA(3),
		"refs/heads/main": hexSHA(1),
	}
	ref := "refs/heads/gh-readonly-queue/main/pr-1-" + hexSHA(2)
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := e.refs.Refs["o/r"][ref]; ok {
		t.Fatal("ref still present")
	}
	// Deleting again is success (idempotent).
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", ref); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	// A real branch can never be deleted through this seam.
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", "refs/heads/main"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("branch delete = %v", err)
	}
	e.svc.Refs = nil
	if err := e.svc.DeleteQueueRef(ctx(), "o", "r", ref); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no publisher = %v", err)
	}
}
