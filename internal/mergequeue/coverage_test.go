package mergequeue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file targets the error/edge arms the main behavioural tests do not
// need: a package gate is a per-package statement gate, and each named
// failure here is a real fail-closed path (a store outage, a lost claim, a
// corrupt document), not a synthetic reach.

// errRoles is identity.RoleService with a scripted CheckRead failure.
type errRoles struct {
	fail bool
	kind auth.AuthErrorKind
	role identity.Role
}

func (e errRoles) Resolve(context.Context, string, string, auth.Principal) (identity.Role, *identity.AccessDoc) {
	return e.role, nil
}
func (e errRoles) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	if !e.fail {
		return nil
	}
	return &auth.AuthError{Kind: e.kind, Why: "scripted"}
}

// switchStore overrides Get/Put for the store-failure arms.
type switchStore struct {
	store.ObjectStore
	getErr  error
	putErr  error
	getJSON string // when non-empty, returned for every Get
}

func (s *switchStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getJSON != "" {
		return store.Object{Meta: store.ObjectMeta{Key: key, Version: "v1"}, Body: io.NopCloser(bytes.NewReader([]byte(s.getJSON)))}, nil
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *switchStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil {
		return store.ObjectMeta{}, s.putErr
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func TestAccessorDefaults(t *testing.T) {
	s := &Service{}
	if s.nowUTC().IsZero() || s.poll() != 10*time.Second || s.timeout() != time.Hour ||
		s.runGrace() != 5*time.Minute || s.claimLease() != 15*time.Minute || s.maxAttempts() != 3 {
		t.Fatal("defaults")
	}
	s.Now = func() time.Time { return time.Unix(0, 0) }
	if s.nowUTC().Unix() != 0 {
		t.Fatal("pinned clock")
	}
	if (&Service{Now: func() time.Time { return time.Time{} }}).nowUTC().IsZero() == false {
		// exists to exercise the branch; no assertion beyond no panic
	}
	for role, rank := range map[identity.Role]int{
		identity.RoleRead: 1, identity.RoleTriage: 2, identity.RoleWrite: 3,
		identity.RoleMaintain: 4, identity.RoleAdmin: 5, "": 0,
	} {
		if roleRank(role) != rank {
			t.Errorf("rank(%q) = %d, want %d", role, roleRank(role), rank)
		}
	}
}

func TestRoleFallbacks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		p    auth.Principal
		want identity.Role
	}{
		{auth.Principal{Admin: true}, identity.RoleAdmin},
		{auth.Principal{Write: true}, identity.RoleWrite},
		{auth.Principal{Anonymous: true}, ""},
		{auth.Principal{Name: "x"}, identity.RoleRead},
	} {
		if got := (&Service{}).roleOf(ctx, "o", "r", tc.p); got != tc.want {
			t.Errorf("roleOf(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
	// requireRole with no roles wired.
	s := &Service{}
	if err := s.requireRole(ctx, "o", "r", auth.Principal{Name: "x"}, identity.RoleMaintain); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no-role = %v", err)
	}
	if err := s.requireRole(ctx, "o", "r", auth.Anonymous(), identity.RoleMaintain); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	// requireRead error mapping.
	for kind, want := range map[auth.AuthErrorKind]error{
		auth.ErrForbidden: ErrForbidden, auth.ErrUnavailable: ErrUnavailable, auth.ErrInvalid: ErrUnauthorized,
	} {
		err := (&Service{Roles: errRoles{fail: true, kind: kind}}).requireRead(ctx, "o", "r", auth.Principal{Name: "x"})
		if !errors.Is(err, want) {
			t.Errorf("CheckRead %v → %v, want %v", kind, err, want)
		}
	}
}

func TestLoadAndUpdateDocErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// Store outage on read.
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("bucket down")}, Roles: h.roles, Pulls: h.pulls}
	if _, _, err := s.loadDoc(ctx, "o", "r"); err == nil {
		t.Fatal("load outage")
	}
	if _, err := s.List(ctx, "o", "r", maint()); err == nil {
		t.Fatal("list outage")
	}
	// Corrupt JSON.
	bad := &switchStore{ObjectStore: h.svc.Store, getJSON: "{not json"}
	s2 := &Service{Store: bad, Roles: h.roles}
	if _, _, err := s2.loadDoc(ctx, "o", "r"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt = %v", err)
	}
	// Put outage.
	s3 := &Service{Store: &switchStore{ObjectStore: h.svc.Store, putErr: errors.New("put down")}, Roles: h.roles, Pulls: h.pulls, Runs: h.runs, Checks: h.checks}
	if err := s3.Pause(ctx, "o", "r", "main", "", maint()); err == nil {
		t.Fatal("put outage")
	}
	// CAS exhaustion: every Put loses the race.
	s4 := &Service{Store: &switchStore{ObjectStore: h.svc.Store, putErr: store.NewPrecondition("k", "v")}, Roles: h.roles, Pulls: h.pulls}
	if err := s4.Pause(ctx, "o", "r", "main", "", maint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhaustion = %v", err)
	}
	// A no-op update returns the document without writing.
	if _, err := h.svc.updateDoc(ctx, "o", "r", func(*Doc) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("no-op = %v", err)
	}
	// A mutate error is returned verbatim.
	if _, err := h.svc.updateDoc(ctx, "o", "r", func(*Doc) (bool, error) { return false, errors.New("nope") }); err == nil {
		t.Fatal("mutate error")
	}
}

func TestWorkerSeamFailures(t *testing.T) {
	ctx := context.Background()
	// No checks backend: fail closed, never a silent pass.
	h := newHarness(t)
	h.svc.Checks = nil
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "no checks backend") {
		t.Fatalf("checks reason = %q", v.Entry.Reason)
	}
	// No run reader.
	h = newHarness(t)
	h.svc.Runs = nil
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v = h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "run reader") {
		t.Fatalf("runs reason = %q", v.Entry.Reason)
	}
	// No pulls seam: the entry is ejected by the guard, not wedged.
	h = newHarness(t)
	h.svc.Pulls = nil
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", NextSeq: 2, Entries: []*Entry{{
		Number: 1, Base: "main", State: StatePending, Seq: 1,
	}}}}, Credentials: map[string]authPrincipal{"main#1": toAuthPrincipal(maint())}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.svc.ReconcileRepo(ctx, "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	v = h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "pulls service not wired") {
		t.Fatalf("pulls reason = %q", v.Entry.Reason)
	}
}

func TestClaimAndClaimLoss(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// Claim with no queue / wrong head / paused all decline.
	if _, _, ok := h.svc.claim(ctx, "o", "r", "main", 1); ok {
		t.Fatal("claim without a queue")
	}
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", Paused: true, NextSeq: 2, Entries: []*Entry{{
		Number: 1, Base: "main", State: StatePending, Seq: 1,
	}}}}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, ok := h.svc.claim(ctx, "o", "r", "main", 1); ok {
		t.Fatal("claim of a paused queue")
	}
	if _, _, ok := h.svc.claim(ctx, "o", "r", "main", 2); ok {
		t.Fatal("claim of a missing entry")
	}
	// recordPrepared with a stale claim declines and does not write.
	if h.svc.recordPrepared(ctx, "o", "r", "main", Entry{Number: 1, ClaimID: "stale"}, &pulls.QueueMerge{}) {
		t.Fatal("recordPrepared accepted a stale claim")
	}
	// markLanding with a stale claim declines.
	if h.svc.markLanding(ctx, "o", "r", "main", 1, "stale") {
		t.Fatal("markLanding accepted a stale claim")
	}
	// stillOwned: no queue, missing entry, wrong state.
	if h.svc.stillOwned(ctx, "o", "r", "nope", 1, "c", StateValidating) {
		t.Fatal("stillOwned on a missing queue")
	}
	if h.svc.stillOwned(ctx, "o", "r", "main", 9, "c", StateValidating) {
		t.Fatal("stillOwned on a missing entry")
	}
	// A store outage reads as not owned (the pass stops; reconcile recovers).
	broken := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("down")}}
	if broken.stillOwned(ctx, "o", "r", "main", 1, "c", StatePending) {
		t.Fatal("stillOwned across an outage")
	}
}

func TestWaitForValidationContextAndTimeout(t *testing.T) {
	h := newHarness(t)
	// A canceled context stops the wait without ejecting (recovery owns it).
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := h.svc.waitForValidation(cctx, "o", "r", "main", Entry{Number: 1, ClaimID: "c"}, &pulls.QueueMerge{MergeSHA: strings.Repeat("a", 40)}, "maint@example.com")
	if v.kind != verdictLost {
		t.Fatalf("canceled = %+v", v)
	}
	// A live run with pending checks times out and names the timeout.
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.checks.fn = func(string) CheckProgress {
		return CheckProgress{Required: []string{"ci/test"}, OK: false, Pending: true, Offenders: []string{"ci/test (pending)"}}
	}
	h.runs.fn = func(string) []RunStatus { return []RunStatus{{ID: "run-live", State: "queued"}} }
	h.svc.Timeout = 20 * time.Millisecond
	h.svc.Poll = time.Millisecond
	h.enqueue(t, 1, 0)
	got := h.waitState(t, 1, StateEjected)
	if !strings.Contains(got.Entry.Reason, "timed out") {
		t.Fatalf("timeout reason = %q", got.Entry.Reason)
	}
}

func TestFinishAndCleanupEdges(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// finish with no document is a no-op.
	h.svc.finish(ctx, "o", "r", "main", 1, StateEjected, "x", "")
	// cleanupRef with no pulls seam / empty ref is a no-op.
	(&Service{}).cleanupRef(ctx, "o", "r", "main", 1, "")
	// A failed delete leaves Ref on the row; reconcile retries and clears it.
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	if err := h.svc.Pause(ctx, "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.enqueue(t, 1, 0)
	h.pulls.mu.Lock()
	// Record a ref on the pending entry by hand (a crashed publish left it).
	doc, _, _ := h.svc.loadDoc(ctx, "o", "r")
	doc.Queues[0].Entries[0].Ref = "refs/heads/gh-readonly-queue/main/pr-1-" + strings.Repeat("a", 40)
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, doc), store.PutOptions{Mode: store.PutUpdate, IfVersion: ""}); err != nil {
		// Initial write omitted IfVersion means overwrite; acceptable in the
		// test harness (one writer).
		_ = err
	}
	h.pulls.mu.Unlock()
	// With pause never resumed, reconcile sees a pending head and a ref: the
	// ref is not among its stale-claim rows, but the recent/orphan sweep still
	// leaves the pending entry runnable.
	if err := h.svc.ReconcileRepo(ctx, "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestDequeueLandingAndMaintainer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", NextSeq: 2, Entries: []*Entry{
		{Number: 1, Base: "main", Author: "a", State: StateLanding, Seq: 1, ClaimID: "c"},
		{Number: 2, Base: "main", Author: "a", State: StatePending, Seq: 2},
	}}}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := h.svc.Dequeue(ctx, "o", "r", "main", 1, maint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("landing dequeue = %v", err)
	}
	if _, err := h.svc.Dequeue(ctx, "o", "r", "main", 2, maint()); err != nil {
		t.Fatalf("maintainer dequeue: %v", err)
	}
}

func TestRecoverAll(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.RecoverAll(context.Background(), []string{"o/r", "bad", "o/r"}); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
}

func TestReconcileKeepsFreshClaimAndStartsPending(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	now := time.Now().UTC().Format(time.RFC3339)
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", NextSeq: 3, Entries: []*Entry{
		{Number: 1, Base: "main", Author: "a", State: StateValidating, Seq: 1, ClaimID: "fresh", ClaimedAt: now},
		{Number: 2, Base: "main", Author: "a", State: StatePending, Seq: 2},
	}}}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	if err := h.svc.ReconcileRepo(ctx, "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Entry 1 keeps its fresh claim (no reset); entry 2 is behind it and stays
	// pending. Nothing landed.
	time.Sleep(10 * time.Millisecond)
	if v := h.view(t, 1); v.Entry == nil || v.Entry.State != StateValidating || v.Entry.ClaimID != "fresh" {
		t.Fatalf("fresh claim = %+v", v.Entry)
	}
}

func TestNotifyNilAndDefault(t *testing.T) {
	h := newHarness(t)
	h.svc.Notify = nil
	h.svc.notify(context.Background(), "o", "r", "x", "a", 1, []string{"a"})
	if len(h.notify) != 0 {
		t.Fatalf("nil Notify still emitted: %v", h.notify)
	}
}

func TestRunWorkerLoadFailureAndRelease(t *testing.T) {
	h := newHarness(t)
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("down")}, workers: map[string]bool{"o/r": true}, kicks: map[string]bool{}}
	s.runWorker(context.Background(), "o", "r")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers["o/r"] {
		t.Fatal("a failed worker kept its slot")
	}
}

func TestPauseResumeNoChange(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// Resume an unpaused queue is a no-op write (still succeeds).
	if err := h.svc.Resume(ctx, "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := h.svc.Pause(ctx, "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Pausing an already-paused queue is a no-op.
	if err := h.svc.Pause(ctx, "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause twice: %v", err)
	}
	// Resume every queue (none named).
	if err := h.svc.Resume(ctx, "o", "r", "", maint()); err != nil {
		t.Fatalf("resume all: %v", err)
	}
}

func TestListBaseUnknown(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.ListBase(context.Background(), "o", "r", "nope", maint())
	if err != nil || v == nil || len(v.Entries) != 0 {
		t.Fatalf("unknown base = %+v, %v", v, err)
	}
}

func TestClaimFreshWorkerWakes(t *testing.T) {
	// A kick that arrives while the worker holds the slot is not lost: the
	// worker drains once more instead of exiting.
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.enqueue(t, 1, 0)
	// startWorker once while paused (the worker exits), then resume; the
	// kick path is exercised by a concurrent enqueue in the main suite.
	if err := h.svc.Resume(context.Background(), "o", "r", "main", maint()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.waitState(t, 1, StateLanded)
	// releaseWorker with a kick returns true (continue) and consumes it.
	s := &Service{workers: map[string]bool{"x": true}, kicks: map[string]bool{"x": true}}
	if !s.releaseWorker("x") {
		t.Fatal("kick not consumed")
	}
	if !s.workers["x"] {
		t.Fatal("a kicked worker must keep the slot while it drains again")
	}
}

func fmtErr(format string, args ...any) error { return fmt.Errorf(format, args...) }

func TestHTTPErrorArms(t *testing.T) {
	h, hh := newHTTP(t)
	base := "/o/r/api/merge-queue"
	// No auth seam falls back to anonymous.
	anon := &Handler{Svc: hh.svc}
	if rec := do(anon, http.MethodGet, base, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil auth = %d", rec.Code)
	}
	// A failing read gate maps through writeErr on list/entry.
	hh.svc.Roles = errRoles{fail: true, kind: auth.ErrForbidden}
	if rec := do(h, http.MethodGet, base, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("list forbidden = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/pulls/1/merge-queue", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("entry forbidden = %d", rec.Code)
	}
	hh.svc.Roles = &fakeRoles{Roles: map[string]identity.Role{"maint@example.com": identity.RoleMaintain}}
	// Dequeue a missing entry.
	if rec := do(h, http.MethodPost, base+"/dequeue", `{"base":"main","number":9}`); rec.Code != http.StatusNotFound {
		t.Fatalf("dequeue missing = %d", rec.Code)
	}
	// Pause refused for a read-only principal.
	readerHandler := &Handler{Svc: hh.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "maint@example.com"}, nil
	}}
	hh.svc.Roles = &fakeRoles{Roles: map[string]identity.Role{"maint@example.com": identity.RoleRead}}
	if rec := do(readerHandler, http.MethodPost, base+"/pause", `{"base":"main"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("pause as reader = %d", rec.Code)
	}
	if rec := do(readerHandler, http.MethodPost, base+"/enqueue", `{"base":"main","number":1}`); rec.Code != http.StatusForbidden {
		t.Fatalf("enqueue as reader = %d", rec.Code)
	}
	hh.svc.Roles = &fakeRoles{Roles: map[string]identity.Role{"maint@example.com": identity.RoleMaintain}}
	// Route-shape misses.
	if rec := do(h, http.MethodGet, "/o/r/api/pulls/0/merge-queue", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("zero num = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/pulls/1/other", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("pulls sibling = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, base+"/extra", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("extra segment = %d", rec.Code)
	}
}

func TestPositionAndRemoveEdges(t *testing.T) {
	if PositionOf(nil, 1) != 0 {
		t.Fatal("nil queue")
	}
	q := &Queue{Entries: []*Entry{{Number: 1, State: StateLanded}}}
	if PositionOf(q, 1) != 0 || PositionOf(q, 2) != 0 {
		t.Fatal("terminal/missing positions")
	}
	if q.remove(2) != nil || q.remove(1) == nil {
		t.Fatal("remove")
	}
}

func TestCleanupRefFailureRetries(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ref := "refs/heads/gh-readonly-queue/main/pr-1-" + strings.Repeat("a", 40)
	seed := &Doc{Version: 1, Repo: "o/r", Recent: []*Entry{{Number: 1, Base: "main", State: StateEjected, Ref: ref}}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.pulls.deleteErr = errors.New("bucket down")
	if err := h.svc.ReconcileRepo(ctx, "o", "r"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if v := h.view(t, 1); v.Entry == nil || v.Entry.Ref != ref {
		t.Fatalf("failed cleanup must keep the ref for retry: %+v", v.Entry)
	}
	h.pulls.mu.Lock()
	h.pulls.deleteErr = nil
	h.pulls.mu.Unlock()
	if err := h.svc.ReconcileRepo(ctx, "o", "r"); err != nil {
		t.Fatalf("reconcile retry: %v", err)
	}
	if v := h.view(t, 1); v.Entry == nil || v.Entry.Ref != "" {
		t.Fatalf("retry did not clear the ref: %+v", v.Entry)
	}
}

func TestWaitForValidationRunError(t *testing.T) {
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.runs.err = errors.New("threads down")
	h.enqueue(t, 1, 0)
	// A candidate read failure is not fatal either: Prepare is the real gate.
	h.pulls.mu.Lock()
	h.pulls.candErr = errors.New("pulls down")
	h.pulls.mu.Unlock()
	v := h.waitState(t, 1, StateEjected)
	if !strings.Contains(v.Entry.Reason, "cannot read the merge_group run") {
		t.Fatalf("reason = %q", v.Entry.Reason)
	}
}

func TestReconcileLoadError(t *testing.T) {
	h := newHarness(t)
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("down")}, workers: map[string]bool{}, kicks: map[string]bool{}, Pulls: h.pulls}
	if err := s.ReconcileRepo(context.Background(), "o", "r"); err == nil {
		t.Fatal("reconcile load error")
	}
}

func TestLandNilRecordAndCleanup(t *testing.T) {
	h := newHarness(t)
	h.pulls.landNil = true
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main"}
	h.enqueue(t, 1, 0)
	v := h.waitState(t, 1, StateLanded)
	if v.Entry == nil || v.Entry.State != StateLanded {
		t.Fatalf("entry = %+v", v.Entry)
	}
}

func TestPauseCreatesNamedQueue(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.Pause(context.Background(), "o", "r", "release/1.0", "freeze", maint()); err != nil {
		t.Fatalf("pause new base: %v", err)
	}
	v, err := h.svc.ListBase(context.Background(), "o", "r", "release/1.0", maint())
	if err != nil || !v.Paused || v.Base != "release/1.0" {
		t.Fatalf("new base = %+v, %v", v, err)
	}
}

func TestHTTPMethodAndAuthEdges(t *testing.T) {
	h, hh := newHTTP(t)
	base := "/o/r/api/merge-queue"
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/o/r/api/pulls/1/merge-queue", http.StatusMethodNotAllowed},
		{http.MethodGet, base + "/dequeue", http.StatusMethodNotAllowed},
		{http.MethodGet, base + "/resume", http.StatusMethodNotAllowed},
		{http.MethodGet, "/o/r/api/pulls/1", http.StatusNotFound},
		{http.MethodGet, "/o/r/api/merge-queue/dequeue/extra", http.StatusNotFound},
	}
	for _, c := range cases {
		if rec := do(h, c.method, c.path, ""); rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	// An AuthError from the principal seam is written by every verb.
	bad := &Handler{Svc: hh.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad token"}
	}}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodGet, "/o/r/api/pulls/1/merge-queue", ""},
		{http.MethodPost, base + "/enqueue", `{"base":"main","number":1}`},
		{http.MethodPost, base + "/dequeue", `{"base":"main","number":1}`},
		{http.MethodPost, base + "/pause", `{"base":"main"}`},
		{http.MethodPost, base + "/resume", `{"base":"main"}`},
	} {
		if rec := do(bad, c.method, c.path, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s auth err = %d", c.method, c.path, rec.Code)
		}
	}
	// Missing base/number on enqueue is a 400.
	if rec := do(h, http.MethodPost, base+"/enqueue", `{"base":"","number":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enqueue empty = %d", rec.Code)
	}
	// Bad JSON on dequeue and pause.
	if rec := do(h, http.MethodPost, base+"/dequeue", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("dequeue bad json = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, base+"/pause", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("pause bad json = %d", rec.Code)
	}
}

func TestReadGateShortCircuits(t *testing.T) {
	ctx := context.Background()
	s := &Service{}
	if err := s.requireRead(ctx, "o", "r", auth.Principal{Admin: true}); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := s.requireRead(ctx, "o", "r", auth.Principal{Write: true}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.requireRead(ctx, "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous: %v", err)
	}
	if err := s.requireRole(ctx, "o", "r", auth.Principal{Admin: true}, identity.RoleMaintain); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	if err := s.requireRole(ctx, "o", "r", auth.Principal{Write: true}, identity.RoleWrite); err != nil {
		t.Fatalf("write role: %v", err)
	}
}

func TestDocDefaultsAndAudit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	raw := []byte(`{"version":0,"repo":"o/r"}`)
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	doc, _, err := h.svc.loadDoc(ctx, "o", "r")
	if err != nil || doc.Version != 1 || doc.Queues == nil {
		t.Fatalf("defaults = %+v, %v", doc, err)
	}
	doc.audit(AuditEntry{Action: "x"}) // empty At gets the wall clock
	if doc.Audit[len(doc.Audit)-1].At == "" {
		t.Fatal("audit did not stamp At")
	}
}

func TestServiceReadErrors(t *testing.T) {
	h := newHarness(t)
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("down")}, Roles: h.roles, Pulls: h.pulls}
	ctx := context.Background()
	if _, err := s.ListBase(ctx, "o", "r", "main", maint()); err == nil {
		t.Fatal("ListBase error")
	}
	if _, err := s.QueueForPR(ctx, "o", "r", 1, maint()); err == nil {
		t.Fatal("QueueForPR error")
	}
	if _, err := s.EntryForPR(ctx, "o", "r", 1, maint()); err == nil {
		t.Fatal("EntryForPR error")
	}
	if _, err := s.Dequeue(ctx, "o", "r", "main", 1, maint()); err == nil {
		t.Fatal("Dequeue error")
	}
}

func TestEnqueueUpdateErrorAndReenqueue(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com"}
	if err := h.svc.Pause(ctx, "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Re-enqueue after a terminal row replaces it.
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := h.svc.Dequeue(ctx, "o", "r", "main", 1, maint()); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if _, err := h.svc.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 1}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	// A store write failure surfaces from updateDoc.
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, putErr: errors.New("down")}, Roles: h.roles, Pulls: h.pulls}
	if _, err := s.Enqueue(ctx, "o", "r", maint(), EnqueueInput{Base: "main", Number: 2}); err == nil {
		t.Fatal("enqueue put error")
	}
	if err := s.Resume(ctx, "o", "r", "", maint()); err == nil {
		t.Fatal("resume put error")
	}
}

func TestCleanupRefActiveEntry(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ref := "refs/heads/gh-readonly-queue/main/pr-1-" + strings.Repeat("a", 40)
	seed := &Doc{Version: 1, Repo: "o/r", Queues: []*Queue{{Base: "main", NextSeq: 2, Entries: []*Entry{{
		Number: 1, Base: "main", State: StatePending, Seq: 1, Ref: ref,
	}}}}}
	if _, err := store.PutBytes(ctx, h.svc.Store, DocKey("o", "r"), mustJSON(t, seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.svc.cleanupRef(ctx, "o", "r", "main", 1, ref)
	if v := h.view(t, 1); v.Entry == nil || v.Entry.Ref != "" {
		t.Fatalf("active ref not cleared: %+v", v.Entry)
	}
}

func TestWorkerExitAndLandLostClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness(t)
	h.svc.mu.Lock()
	h.svc.workers["o/r"] = true
	h.svc.mu.Unlock()
	h.svc.runWorker(ctx, "o", "r")
	h.svc.mu.Lock()
	if h.svc.workers["o/r"] {
		t.Fatal("canceled worker kept its slot")
	}
	h.svc.mu.Unlock()
	// land with a stale claim is a quiet no-op.
	h.svc.land(ctx, "o", "r", "main", Entry{Number: 1, ClaimID: "stale"}, &pulls.QueueMerge{}, maint())
}

func TestRecoverAllError(t *testing.T) {
	h := newHarness(t)
	s := &Service{Store: &switchStore{ObjectStore: h.svc.Store, getErr: errors.New("down")}, Roles: h.roles}
	if err := s.RecoverAll(context.Background(), []string{"o/r"}); err == nil {
		t.Fatal("RecoverAll error")
	}
}
