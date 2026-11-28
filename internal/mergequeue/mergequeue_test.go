package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- fakes --------------------------------------------------------------------

type fakeRoles struct {
	Roles map[string]identity.Role
}

func (f *fakeRoles) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	return f.Roles[strings.ToLower(p.Name)], nil
}

func (f *fakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sign in"}
	}
	return nil
}

type fakePulls struct {
	mu        sync.Mutex
	cand      *pulls.QueueCandidate
	candErr   error
	prepErr   error
	landErr   error
	prepared  []*pulls.QueueMerge
	landed    []int
	deleted   []string
	landDelay chan struct{} // when non-nil, Land waits for close
	prepSeq   int64
	deleteErr error
	landNil   bool
}

func (f *fakePulls) QueueCandidate(_ context.Context, _, _ string, _ int, _ auth.Principal) (*pulls.QueueCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.candErr != nil {
		return nil, f.candErr
	}
	if f.cand == nil {
		return &pulls.QueueCandidate{Num: 1, Author: "author", State: "open", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", HeadSHA: strings.Repeat("a", 40)}, nil
	}
	c := *f.cand
	return &c, nil
}

func (f *fakePulls) PrepareQueueMerge(_ context.Context, _, repo string, num int, _ auth.Principal) (*pulls.QueueMerge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepErr != nil {
		return nil, f.prepErr
	}
	n := atomic.AddInt64(&f.prepSeq, 1)
	qm := &pulls.QueueMerge{
		Ref:      fmt.Sprintf("refs/heads/gh-readonly-queue/main/pr-%d-%s", num, strings.Repeat("a", 40)),
		MergeSHA: fmt.Sprintf("%040x", 1000+int(n)),
		BaseSHA:  strings.Repeat("b", 40),
		HeadSHA:  strings.Repeat("a", 40),
		Strategy: pulls.StrategyMerge,
	}
	f.prepared = append(f.prepared, qm)
	return qm, nil
}

func (f *fakePulls) LandQueuedMerge(_ context.Context, _, _ string, num int, _ auth.Principal, _ *pulls.QueueMerge) (*pulls.TaskRecord, error) {
	f.mu.Lock()
	delay := f.landDelay
	err := f.landErr
	f.mu.Unlock()
	if delay != nil {
		<-delay
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.landed = append(f.landed, num)
	nilRec := f.landNil
	f.mu.Unlock()
	if nilRec {
		return nil, nil
	}
	return &pulls.TaskRecord{State: pulls.TaskOK, Summary: fmt.Sprintf("merged #%d", num)}, nil
}

func (f *fakePulls) DeleteQueueRef(_ context.Context, _, _, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ref)
	return f.deleteErr
}

func (f *fakePulls) snapshot() (prepared []*pulls.QueueMerge, landed []int, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pulls.QueueMerge(nil), f.prepared...), append([]int(nil), f.landed...), append([]string(nil), f.deleted...)
}

type fakeRuns struct {
	mu  sync.Mutex
	fn  func(sha string) []RunStatus
	err error
}

func (f *fakeRuns) MergeGroupRuns(_ context.Context, _ string, sha string, _ time.Time) ([]RunStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.fn != nil {
		return f.fn(sha), nil
	}
	return []RunStatus{{ID: "run-" + sha[:4], State: "success"}}, nil
}

type fakeChecks struct {
	mu sync.Mutex
	fn func(sha string) CheckProgress
}

func (f *fakeChecks) RequiredChecksProgress(_ context.Context, _, _, sha, _, _ string) (CheckProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fn != nil {
		return f.fn(sha), nil
	}
	return CheckProgress{Required: []string{"ci/test"}, OK: true}, nil
}

type harness struct {
	svc    *Service
	pulls  *fakePulls
	runs   *fakeRuns
	checks *fakeChecks
	roles  *fakeRoles
	notify []Notification
	refs   []string
	mu     sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		pulls:  &fakePulls{},
		runs:   &fakeRuns{},
		checks: &fakeChecks{},
		roles:  &fakeRoles{Roles: map[string]identity.Role{"maint@example.com": identity.RoleMaintain, "author@example.com": identity.RoleRead}},
	}
	svc := New(store.NewMemory())
	svc.Roles = h.roles
	svc.Pulls = h.pulls
	svc.Runs = h.runs
	svc.Checks = h.checks
	svc.Poll = time.Millisecond
	svc.Timeout = 2 * time.Second
	svc.RunGrace = 50 * time.Millisecond
	svc.ClaimLease = time.Minute
	svc.MaxAttempts = 3
	svc.Now = func() time.Time { return time.Now().UTC() }
	svc.Notify = func(_ context.Context, ev Notification) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.notify = append(h.notify, ev)
	}
	svc.RefPublished = func(_ context.Context, repo, ref, newSHA string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.refs = append(h.refs, ref+"@"+newSHA)
	}
	h.svc = svc
	return h
}

func (h *harness) enqueue(t *testing.T, num, priority int) *Entry {
	t.Helper()
	e, err := h.svc.Enqueue(context.Background(), "o", "r", maint(), EnqueueInput{Base: "main", Number: num, Priority: priority})
	if err != nil {
		t.Fatalf("enqueue %d: %v", num, err)
	}
	return e
}

func (h *harness) view(t *testing.T, num int) *PRView {
	t.Helper()
	v, err := h.svc.QueueForPR(context.Background(), "o", "r", num, maint())
	if err != nil {
		t.Fatalf("QueueForPR: %v", err)
	}
	return v
}

func (h *harness) waitState(t *testing.T, num int, want ...string) *PRView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v := h.view(t, num)
		if v.Entry != nil {
			for _, w := range want {
				if v.Entry.State == w {
					return v
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := h.view(t, num), 0
	t.Fatalf("entry %d never reached %v: %+v", num, want, v.Entry)
	return nil
}

func maint() auth.Principal {
	return auth.Principal{Name: "maint@example.com", Groups: []string{"admins"}}
}
func author() auth.Principal {
	return auth.Principal{Name: "author@example.com"}
}
func reader() auth.Principal { return auth.Principal{Name: "reader@example.com"} }

// --- tests --------------------------------------------------------------------

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Enqueue(ctx, "o", "r", auth.Anonymous(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", reader(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader = %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "", Number: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty base = %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero number = %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1, Priority: 101}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad priority = %v", err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/other"}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong base = %v", err)
	}
	for _, c := range []*pulls.QueueCandidate{
		{Num: 1, Merged: true, BaseRef: "refs/heads/main"},
		{Num: 1, State: "closed", BaseRef: "refs/heads/main"},
		{Num: 1, Draft: true, State: "open", BaseRef: "refs/heads/main"},
	} {
		h.pulls.cand = c
		if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrConflict) {
			t.Fatalf("candidate %+v = %v", c, err)
		}
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	// Hold the queue so the duplicate check is not raced by the driver.
	if err := h.svc.Pause(ctx, "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.svc.Checks = nil // fail-closed seam, but enqueue does not need it
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	// A non-wired pulls seam is unavailable.
	h2 := newHarness(t)
	h2.svc.Pulls = nil
	if _, err := h2.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no pulls = %v", err)
	}
	if _, err := h2.svc.List(ctx, "o", "r", maint()); err != nil {
		t.Fatalf("List with no pulls: %v", err)
	}
}

func TestQueueDrainsInOrder(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	// The fake returns the same candidate for every number; Enqueue only
	// validates base/state, so this is enough for both entries.
	h.enqueue(t, 1, 0)
	h.enqueue(t, 2, 0)
	h.waitState(t, 1, StateLanded)
	h.waitState(t, 2, StateLanded)

	prepared, landed, deleted := h.pulls.snapshot()
	if len(landed) != 2 || landed[0] != 1 || landed[1] != 2 {
		t.Fatalf("landed = %v, want [1 2]", landed)
	}
	if len(prepared) != 2 {
		t.Fatalf("prepared = %d", len(prepared))
	}
	if !strings.HasPrefix(prepared[0].Ref, "refs/heads/gh-readonly-queue/main/pr-1-") {
		t.Fatalf("ref = %q", prepared[0].Ref)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted refs = %v", deleted)
	}
	v, _ := h.svc.List(context.Background(), "o", "r", maint())
	if len(v.Queues) != 1 || len(v.Queues[0].Entries) != 0 || len(v.Recent) != 2 {
		t.Fatalf("view after drain = %+v", v)
	}
	// Audit trail has the enqueue + landed rows, newest last.
	var actions []string
	for _, a := range v.Audit {
		actions = append(actions, a.Action)
	}
	joined := strings.Join(actions, ",")
	if !strings.Contains(joined, "enqueued") || !strings.Contains(joined, "landed") {
		t.Fatalf("audit = %v", actions)
	}
}

func TestPriorityRunsBeforeEarlierFIFO(t *testing.T) {
	h := newHarness(t)
	// Hold the queue: this asserts the derived positions, and the driver is
	// fast enough under -race to land the first entry between enqueues.
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	h.enqueue(t, 2, 10)
	v := h.view(t, 2)
	if v.Entry == nil || v.Entry.Position != 1 {
		t.Fatalf("priority position = %+v", v.Entry)
	}
	if p := h.view(t, 1); p.Entry == nil || p.Entry.Position != 2 {
		t.Fatalf("fifo position = %+v", p.Entry)
	}
}

func TestPriorityRunsFirstWhenReleased(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.enqueue(t, 1, 0)
	h.enqueue(t, 2, 10)
	if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.waitState(t, 1, StateLanded)
	h.waitState(t, 2, StateLanded)
	_, landed, _ := h.pulls.snapshot()
	if len(landed) != 2 || landed[0] != 2 || landed[1] != 1 {
		t.Fatalf("landed = %v, want [2 1]", landed)
	}
}

func TestFailingRequiredChecksEjectsAndAdvances(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	var calls int32
	h.checks.fn = func(sha string) CheckProgress {
		if atomic.AddInt32(&calls, 1) == 1 {
			return CheckProgress{Required: []string{"ci/test"}, OK: false, Pending: false, Offenders: []string{"ci/test (failure)"}}
		}
		return CheckProgress{Required: []string{"ci/test"}, OK: true}
	}
	h.enqueue(t, 1, 0)
	h.enqueue(t, 2, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "ci/test (failure)") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
	h.waitState(t, 2, StateLanded)
	_, landed, _ := h.pulls.snapshot()
	if len(landed) != 1 || landed[0] != 2 {
		t.Fatalf("landed = %v, the failed entry must not land", landed)
	}
}

func TestRunFailureEjects(t *testing.T) {
	h := newHarness(t)
	h.runs.fn = func(string) []RunStatus { return []RunStatus{{ID: "run-9", State: "failure"}} }
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "merge_group run run-9 ended failure") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestRunCancelledAndTimedOutEject(t *testing.T) {
	for _, state := range []string{"cancelled", "skipped", "timed_out"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			h.runs.fn = func(string) []RunStatus { return []RunStatus{{ID: "run", State: state}} }
			h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
			h.enqueue(t, 1, 0)
			v := h.waitState(t, 1, StateEjected)
			if !strings.Contains(v.Entry.Reason, state) {
				t.Fatalf("reason = %q", v.Entry.Reason)
			}
		})
	}
}

func TestRunSuccessButPendingRequiredChecksEjects(t *testing.T) {
	h := newHarness(t)
	h.checks.fn = func(string) CheckProgress {
		return CheckProgress{Required: []string{"ci/test"}, OK: false, Pending: true, Offenders: []string{"ci/test (pending)"}}
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "required checks") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestNoRunWithinGraceEjects(t *testing.T) {
	h := newHarness(t)
	h.runs.fn = func(string) []RunStatus { return nil }
	// No required contexts: the run itself must exist and finish green.
	h.checks.fn = func(string) CheckProgress { return CheckProgress{OK: true} }
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "no merge_group run started") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestPrepareFailureEjects(t *testing.T) {
	h := newHarness(t)
	h.pulls.prepErr = errors.New("conflict: pull request #1 has conflicts in a.txt")
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "conflicts") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestLandFailureEjects(t *testing.T) {
	h := newHarness(t)
	h.pulls.landErr = errors.New("conflict: base moved from a to b while queued")
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "base moved") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestMergedOutsideQueueRecordsLanded(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	h.pulls.mu.Lock()
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "closed", Merged: true, BaseRef: "refs/heads/main"}
	h.pulls.mu.Unlock()
	if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	v := h.waitState(t, 1, StateLanded)
	if v.Entry.Reason != "merged outside the queue" {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestClosedAndDraftWhileQueuedEject(t *testing.T) {
	for _, cand := range []*pulls.QueueCandidate{
		{Num: 1, State: "closed", BaseRef: "refs/heads/main"},
		{Num: 1, State: "open", Draft: true, BaseRef: "refs/heads/main"},
	} {
		h := newHarness(t)
		// Enqueue sees an open, non-draft candidate; the worker then sees the
		// changed PR (a human closed it or flipped it to draft).
		if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
			t.Fatalf("pause: %v", err)
		}
		h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
		h.pulls.candErr = nil
		h.enqueue(t, 1, 0)
		h.pulls.mu.Lock()
		h.pulls.cand = cand
		h.pulls.mu.Unlock()
		if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
			t.Fatalf("resume: %v", err)
		}
		v := h.waitState(t, 1, StateEjected)
		if !strings.Contains(v.Entry.Reason, "closed") && !strings.Contains(v.Entry.Reason, "draft") {
			t.Fatalf("reason = %q", v.Entry.Reason)
		}
	}
}

func TestPauseStopsStartsAndResumeRestarts(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "freeze", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.enqueue(t, 1, 0)
	time.Sleep(20 * time.Millisecond)
	if v := h.view(t, 1); v.Entry == nil || v.Entry.State != StatePending || !v.Paused {
		t.Fatalf("paused entry = %+v", v)
	}
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "again", maint()); err != nil {
		t.Fatalf("pause twice: %v", err)
	}
	if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.waitState(t, 1, StateLanded)
	if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume twice: %v", err)
	}
	if err := h.svc.Pause(context.Background(), "o", "r", "", "all", maint()); err != nil {
		t.Fatalf("pause all: %v", err)
	}
	if err := h.svc.Resume(context.Background(), "o", "r", "", maint()); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	// Authorization.
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader pause = %v", err)
	}
	if err := h.svc.Resume(context.Background(), "o", "r", "main", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous resume = %v", err)
	}
}

func TestDequeueAuthorizationAndRefCleanup(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.enqueue(t, 1, 0)
	if _, err := h.svc.Dequeue(context.Background(), "o", "r", "main", 1, reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader dequeue = %v", err)
	}
	if _, err := h.svc.Dequeue(context.Background(), "o", "r", "other", 1, maint()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other base = %v", err)
	}
	if _, err := h.svc.Dequeue(context.Background(), "o", "r", "main", 9, maint()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing PR = %v", err)
	}
	// The author may dequeue their own PR.
	e, err := h.svc.Dequeue(context.Background(), "o", "r", "main", 1, author())
	if err != nil {
		t.Fatalf("author dequeue: %v", err)
	}
	if e.State != StateCancelled || !strings.Contains(e.Reason, "dequeued by author@example.com") {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := h.svc.Dequeue(context.Background(), "o", "r", "main", 1, maint()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dequeue twice = %v", err)
	}
}

func TestDequeueDeletesPreparedRef(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	// Keep the entry validating (checks pending) so the dequeued state is
	// one a person may cancel; landing is refused by design.
	h.checks.fn = func(string) CheckProgress {
		return CheckProgress{Required: []string{"ci/test"}, OK: false, Pending: true, Offenders: []string{"ci/test (pending)"}}
	}
	h.runs.fn = func(string) []RunStatus { return []RunStatus{{ID: "run-live", State: "queued"}} }
	h.enqueue(t, 1, 0)
	// Wait for the prepared ref to be recorded.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := h.view(t, 1); v.Entry != nil && v.Entry.Ref != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := h.svc.Dequeue(context.Background(), "o", "r", "main", 1, maint()); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	_, landed, deleted := h.pulls.snapshot()
	if len(landed) != 0 {
		t.Fatalf("cancelled entry landed: %v", landed)
	}
	if len(deleted) == 0 {
		t.Fatal("cancelled entry's ref was not deleted")
	}
}

func TestReconcileRecoversStaleClaim(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	// Seed a validating entry from a previous process: a stale claim with a
	// ref. Reconcile must delete the ref, reset to pending and start it.
	seed := &Doc{
		Version: 1, Repo: "o/r",
		Queues: []*Queue{{Base: "main", NextSeq: 2, Entries: []*Entry{{
			Number: 1, Base: "main", Author: "author@example.com", Enqueuer: "maint@example.com",
			State: StateValidating, Seq: 1,
			EnqueuedAt: "2026-01-01T00:00:00Z", ClaimID: "old", ClaimedAt: "2026-01-01T00:00:00Z",
			Ref: "refs/heads/gh-readonly-queue/main/pr-1-" + strings.Repeat("a", 40), Attempts: 1,
		}}}},
		Credentials: map[string]authPrincipal{"main#1": toAuthPrincipal(maint())},
	}
	raw := mustJSON(t, seed)
	if _, err := store.PutBytes(context.Background(), h.svc.Store, DocKey("o", "r"), raw, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.svc.ReconcileRepo(context.Background(), "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	h.waitState(t, 1, StateLanded)
	_, _, deleted := h.pulls.snapshot()
	foundRef := false
	for _, ref := range deleted {
		if strings.Contains(ref, "gh-readonly-queue/main/pr-1-") {
			foundRef = true
		}
	}
	if !foundRef {
		t.Fatalf("reconcile did not delete the stale ref: %v", deleted)
	}
	// The recovery row is audited.
	v, _ := h.svc.List(context.Background(), "o", "r", maint())
	found := false
	for _, a := range v.Audit {
		if a.Action == "recovered" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit = %+v", v.Audit)
	}
}

func TestReconcileEjectsAtAttemptLimit(t *testing.T) {
	h := newHarness(t)
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", NextSeq: 2, Entries: []*Entry{{
		Number: 1, Base: "main", Author: "author@example.com", State: StateLanding, Seq: 1,
		ClaimID: "old", ClaimedAt: "2026-01-01T00:00:00Z", Attempts: 3,
		Ref: "refs/heads/gh-readonly-queue/main/pr-1-" + strings.Repeat("a", 40),
	}}}}}
	if _, err := store.PutBytes(context.Background(), h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.svc.ReconcileRepo(context.Background(), "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	v := h.view(t, 1)
	if v.Entry == nil || v.Entry.State != StateEjected || !strings.Contains(v.Entry.Reason, "recovered 3 times") {
		t.Fatalf("entry = %+v", v.Entry)
	}
}

func TestReconcileCleansRecentRef(t *testing.T) {
	h := newHarness(t)
	seed := &Doc{Version: 1, Repo: "o/r", Recent: []*Entry{{
		Number: 4, Base: "main", Author: "a", State: StateEjected,
		Ref: "refs/heads/gh-readonly-queue/main/pr-4-" + strings.Repeat("a", 40),
	}}}
	if _, err := store.PutBytes(context.Background(), h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.svc.ReconcileRepo(context.Background(), "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_, _, deleted := h.pulls.snapshot()
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v", deleted)
	}
	v := h.view(t, 4)
	if v.Entry == nil || v.Entry.Ref != "" {
		t.Fatalf("recent entry = %+v", v.Entry)
	}
}

func TestReconcileNoDocumentAndNoPull(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.ReconcileRepo(context.Background(), "o", "r"); err != nil {
		t.Fatalf("empty reconcile: %v", err)
	}
	h2 := newHarness(t)
	h2.svc.Pulls = nil
	h2.svc.Checks = nil
	h2.svc.Runs = nil
	h2.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	// A missing pulls seams must fail the entry closed with a reason, not
	// wedge it.
	if _, err := h2.svc.Enqueue(context.Background(), "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no pulls enqueue = %v", err)
	}
}

func TestListBaseAndEntryForPR(t *testing.T) {
	h := newHarness(t)
	// Hold the queue: the driver is fast enough under -race to land the
	// entry between enqueue and the position assertion.
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if v, err := h.svc.ListBase(context.Background(), "o", "r", "main", maint()); err != nil || len(v.Entries) != 0 {
		t.Fatalf("empty base = %+v, %v", v, err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	if v, err := h.svc.ListBase(context.Background(), "o", "r", "refs/heads/main", maint()); err != nil || len(v.Entries) != 1 {
		t.Fatalf("base view = %+v, %v", v, err)
	}
	e, err := h.svc.EntryForPR(context.Background(), "o", "r", 1, maint())
	if err != nil || e == nil || e.Position != 1 {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	if e, _ := h.svc.EntryForPR(context.Background(), "o", "r", 99, maint()); e != nil {
		t.Fatalf("missing entry = %+v", e)
	}
}

func TestReadsRequireReadGate(t *testing.T) {
	h := newHarness(t)
	anon := auth.Anonymous()
	if _, err := h.svc.List(context.Background(), "o", "r", anon); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon list = %v", err)
	}
	if _, err := h.svc.ListBase(context.Background(), "o", "r", "main", anon); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon base = %v", err)
	}
	if _, err := h.svc.QueueForPR(context.Background(), "o", "r", 1, anon); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon PR = %v", err)
	}
	// A nil role service keeps the legacy open read.
	h.svc.Roles = nil
	if _, err := h.svc.List(context.Background(), "o", "r", auth.Principal{Name: "x"}); err != nil {
		t.Fatalf("nil roles list = %v", err)
	}
}

func TestOrderingAndView(t *testing.T) {
	q := &Queue{Base: "main", Entries: []*Entry{
		{Number: 1, Seq: 1, State: StatePending},
		{Number: 2, Seq: 2, State: StatePending, Priority: 5},
		{Number: 3, Seq: 3, State: StatePending, Priority: 5},
	}}
	if PositionOf(q, 2) != 1 || PositionOf(q, 3) != 2 || PositionOf(q, 1) != 3 {
		t.Fatalf("positions = %d %d %d", PositionOf(q, 2), PositionOf(q, 3), PositionOf(q, 1))
	}
	if h := q.head(); h == nil || h.Number != 2 {
		t.Fatalf("head = %+v", h)
	}
	qv := queueViewOf(q)
	if len(qv.Entries) != 3 || qv.Entries[0].Number != 2 || qv.Entries[0].Position != 1 {
		t.Fatalf("view = %+v", qv.Entries)
	}
	doc := &Doc{Repo: "o/r", Queues: []*Queue{q}}
	v := viewOf(doc)
	if v.Repo != "o/r" || len(v.Queues) != 1 || v.PausedAny {
		t.Fatalf("viewOf = %+v", v)
	}
	q.Paused = true
	if !viewOf(doc).PausedAny {
		t.Fatal("pausedAny")
	}
	if got := baseLabel("refs/heads/release/1.0"); got != "release/1.0" {
		t.Fatalf("baseLabel = %q", got)
	}
	if got := baseLabel("main"); got != "main" {
		t.Fatalf("baseLabel plain = %q", got)
	}
}

func TestAuditAndRecentBounds(t *testing.T) {
	d := &Doc{}
	for i := 0; i < 260; i++ {
		d.audit(AuditEntry{At: "2026-01-01T00:00:00Z", Action: "x"})
	}
	if len(d.Audit) != 200 {
		t.Fatalf("audit bound = %d", len(d.Audit))
	}
	for i := 0; i < 80; i++ {
		d.remember(&Entry{Number: i + 1, State: StateCancelled})
	}
	if len(d.Recent) != 50 {
		t.Fatalf("recent bound = %d", len(d.Recent))
	}
	// Replacing the same PR keeps one row, newest first.
	d.remember(&Entry{Number: 80, State: StateLanded})
	if len(d.Recent) != 50 || d.Recent[0].Number != 80 || d.Recent[0].State != StateLanded {
		t.Fatalf("recent replace = %+v", d.Recent[0])
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
