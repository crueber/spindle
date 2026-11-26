// Failure-path and unit coverage for implicit supersede (PAR-150): the head
// adoption must fail closed and the cancel path must report a store outage as an
// error, never a half record. The happy paths live in supersede_test.go.
package threads

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

func TestSupersedeTriggerPredicates(t *testing.T) {
	adopt := map[string]bool{
		EventPush:     true,
		EventPullReq:  true,
		EventDispatch: false,
		EventSchedule: false,
	}
	for event, want := range adopt {
		if got := supersedeAdopt(event); got != want {
			t.Fatalf("supersedeAdopt(%q) = %v, want %v", event, got, want)
		}
	}
	displace := []struct {
		event, action string
		want          bool
	}{
		{EventPush, "", true},
		{EventPullReq, "synchronize", true},
		{EventPullReq, "Synchronize", true},
		{EventPullReq, "opened", false},
		{EventPullReq, "reopened", false},
		{EventDispatch, "", false},
		{EventSchedule, "", false},
	}
	for _, c := range displace {
		if got := supersedeDisplaces(c.event, c.action); got != c.want {
			t.Fatalf("supersedeDisplaces(%q,%q) = %v, want %v", c.event, c.action, got, c.want)
		}
	}
}

// putHead writes a raw head doc for the default fixture pair.
func putHead(t *testing.T, e *testEnv, doc string) {
	t.Helper()
	key := SupersedeKey("o", "r", ".spindle/workflows/ci.yml", "refs/heads/main")
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed head: %v", err)
	}
}

func TestSupersedeCorruptHeadIsReportedButRunStillExists(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	putHead(t, e, `{"run_id":`)
	runs := e.createPush(t, testSHA, "main")
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	// The bookkeeping failure is logged, never fatal: the run is the commit
	// point and exists and runs.
	if got := e.mustRun(t, runs[0].ID); !live(got) {
		t.Fatalf("run state = %s after a corrupt head", got.State)
	}
	if _, _, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", "0000000000000000001-aaaaaa", testSHA); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("adopt on a corrupt head: %v", err)
	}
}

func TestSupersedeGhostPredecessorIsHarmless(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	// A head naming a run that does not exist (a reclaimed run, a crashed
	// creation): nothing to cancel, no error.
	putHead(t, e, `{"workflow":".spindle/workflows/ci.yml","ref":"refs/heads/main","run_id":"0000000000000000001-aaaaaa"}`)
	runs := e.createPush(t, testSHA, "main")
	if len(runs) != 1 || !live(runs[0]) {
		t.Fatalf("ghost predecessor disturbed creation: %+v", runs)
	}
	if got := e.mustRun(t, runs[0].ID); got.State != RunQueued {
		t.Fatalf("new run state = %s", got.State)
	}
}

func TestSupersedeRefusesACrossWorkflowPredecessor(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	otherYAML := "name: Other\non:\n  push:\n    branches: [feature]\njobs:\n  one:\n    runs-on: linux\n    steps:\n      - run: echo one\n"
	e.git.add(testSHA, ".spindle/workflows/other.yml", otherYAML)
	// other.yml (every branch) runs on the feature push; ci.yml (main only) does
	// not.
	runs := e.createPush(t, testSHA, "feature")
	if len(runs) != 1 || runs[0].WorkflowPath != ".spindle/workflows/other.yml" {
		t.Fatalf("fixture runs = %+v", runs)
	}
	otherRun := runs[0]
	// Point ci.yml's main head at the OTHER workflow's run: a hand-edited or
	// legacy record. The defensive check must refuse to cancel it.
	if _, err := e.svc.casUpdate(ctx(), SupersedeKey("o", "r", ".spindle/workflows/ci.yml", "refs/heads/main"), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte(`{"workflow":".spindle/workflows/ci.yml","ref":"refs/heads/main","run_id":"` + otherRun.ID + `","version":9}`), true, nil
	}); err != nil {
		t.Fatalf("rewrite head: %v", err)
	}
	mainRuns := e.createPush(t, testSHA, "main")
	if len(mainRuns) != 1 || mainRuns[0].WorkflowPath != ".spindle/workflows/ci.yml" {
		t.Fatalf("main runs = %+v", mainRuns)
	}
	if got := e.mustRun(t, otherRun.ID); !live(got) {
		t.Fatalf("a cross-workflow predecessor was cancelled: state=%s", got.State)
	}
}

func TestAdoptRunHeadFailsClosedOnStoreErrors(t *testing.T) {
	e := newTestEnv(t)
	fs := newFaultStore()
	e.svc.Store = fs
	// GET failure.
	fs.fail("get", 0)
	if _, _, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", "0000000000000000001-aaaaaa", testSHA); err == nil {
		t.Fatal("adopt on a failing GET returned no error")
	}
	// PUT failure (a non-precondition store error).
	fs.fail("put", 0)
	if _, _, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", "0000000000000000001-aaaaaa", testSHA); err == nil {
		t.Fatal("adopt on a failing PUT returned no error")
	}
	fs.clear()
	// Every CAS losing four times concedes without an error and without writing.
	losing := &losingStore{Memory: store.NewMemory()}
	e.svc.Store = losing
	prev, newer, err := e.svc.adoptRunHead(ctx(), "o", "r", ".spindle/workflows/ci.yml", "refs/heads/main", "0000000000000000001-aaaaaa", testSHA)
	if err != nil || prev != "" || newer != "" {
		t.Fatalf("exhausted adopt: prev=%q newer=%q err=%v", prev, newer, err)
	}
}

// losingStore fails every conditional write with a precondition error, the shape
// of a permanently contended record.
type losingStore struct{ *store.Memory }

func (l *losingStore) Put(context.Context, string, store.PutBody, store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: "losing", Err: errors.New("lost the CAS")}
}

func TestCancelRunStoreFailuresAreErrors(t *testing.T) {
	cases := []struct {
		name  string
		op    string
		after int
		want  string
	}{
		{"header get", "get", 0, "injected fault"},
		{"jobs list", "list", 0, "injected fault"},
		{"cancel stamp put", "put", 0, "injected fault"},
		{"job update put", "put", 1, "injected fault"},
		{"fold list", "list", 1, "injected fault"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.useIndependentJobs()
			run := e.createPush(t, testSHA, "main")[0]
			fs := newFaultStore()
			fs.Memory = e.store
			e.svc.Store = fs
			fs.fail(c.op, c.after)
			_, err := e.svc.cancelRun(ctx(), "o", "r", run.ID, cancelWhy{Reason: CancelReasonSupersede, SupersededBy: "0000000000000000002-bbbbbb"})
			if err == nil {
				t.Fatalf("%s: cancelRun returned no error", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: error %q does not name %q", c.name, err, c.want)
			}
		})
	}
}

func TestCancelRunKeepsAnExistingReason(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.createPush(t, testSHA, "main")[0]
	// A header that already carries a reason while still live is what a crashed
	// cancel leaves behind: the next cancel must not relabel it.
	if _, err := e.svc.stampCancel(ctx(), "o", "r", run.ID, cancelWhy{Actor: "maint", Reason: CancelReasonOperator}); err != nil {
		t.Fatal(err)
	}
	updated, err := e.svc.cancelRun(ctx(), "o", "r", run.ID, cancelWhy{Reason: CancelReasonSupersede, SupersededBy: "0000000000000000002-bbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CancelReason != CancelReasonOperator || updated.CancelledBy != "maint" || updated.SupersededBy != "" {
		t.Fatalf("cancel relabelled: reason=%q by=%q superseded=%q", updated.CancelReason, updated.CancelledBy, updated.SupersededBy)
	}
	// A nil pending is a no-op, and so is a nil run.
	if got := e.svc.finishSupersede(ctx(), "o", "r", run, nil); got != nil {
		t.Fatalf("nil pending returned %+v", got)
	}
	if got := e.svc.finishSupersede(ctx(), "o", "r", nil, &supersedePending{}); got != nil {
		t.Fatalf("nil run returned %+v", got)
	}
}

func TestStartSupersedeGates(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	// No ref: no key, no adoption.
	if got := e.svc.startSupersede(ctx(), "o", "r", &Run{ID: "0000000000000000001-aaaaaa"}, EventPush, ""); got != nil {
		t.Fatalf("a run with no ref adopted a head")
	}
	// A dispatch never registers one.
	if got := e.svc.startSupersede(ctx(), "o", "r", &Run{ID: "0000000000000000001-aaaaaa", Ref: "refs/heads/main"}, EventDispatch, ""); got != nil {
		t.Fatalf("a dispatch adopted a head")
	}
	// A nil run is a no-op.
	if got := e.svc.startSupersede(ctx(), "o", "r", nil, EventPush, ""); got != nil {
		t.Fatalf("a nil run adopted a head")
	}
}

// TestFinishSupersedeLogsAndNeverFails is the "bookkeeping outage must not fail
// creation" contract of finishSupersede: every verdict path that hits a store
// fault reports nil (the run exists and is the commit point) rather than an
// error the trigger hook would treat as "no run".
func TestFinishSupersedeLogsAndNeverFails(t *testing.T) {
	// The displaced run's header cannot be read.
	e := newTestEnv(t)
	e.useIndependentJobs()
	prev := e.createPush(t, testSHA, "main")[0]
	fs := newFaultStore()
	fs.Memory = e.store
	e.svc.Store = fs
	ch := make(chan supersedeResult, 1)
	ch <- supersedeResult{prev: prev.ID}
	fs.fail("get", 0)
	if got := e.svc.finishSupersede(ctx(), "o", "r", prev, &supersedePending{ch: ch, displaces: true}); got != nil {
		t.Fatalf("a failed predecessor read returned %+v", got)
	}

	// The displaced run cannot be cancelled (the read succeeds, the cancel's own
	// load fails).
	e = newTestEnv(t)
	e.useIndependentJobs()
	prev = e.createPush(t, testSHA, "main")[0]
	fs = newFaultStore()
	fs.Memory = e.store
	e.svc.Store = fs
	ch = make(chan supersedeResult, 1)
	ch <- supersedeResult{prev: prev.ID}
	fs.fail("get", 1)
	if got := e.svc.finishSupersede(ctx(), "o", "r", prev, &supersedePending{ch: ch, displaces: true}); got != nil {
		t.Fatalf("a failed cancel returned %+v", got)
	}
	fs.clear()
	if got := e.mustRun(t, prev.ID); got.State != RunQueued {
		t.Fatalf("predecessor = %s", got.State)
	}

	// This run lost to a newer one and cannot even cancel itself.
	e = newTestEnv(t)
	e.useIndependentJobs()
	self := e.createPush(t, testSHA, "main")[0]
	fs = newFaultStore()
	fs.Memory = e.store
	e.svc.Store = fs
	ch = make(chan supersedeResult, 1)
	ch <- supersedeResult{newer: "9999999999999999999-ffffff"}
	fs.fail("get", 0)
	if got := e.svc.finishSupersede(ctx(), "o", "r", self, &supersedePending{ch: ch}); got != nil {
		t.Fatalf("a failed self-cancel returned %+v", got)
	}
}

func TestCancelRunRunningJobStoreFailure(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.createPush(t, testSHA, "main")[0]
	jobs := mustJobs(t, e, run.ID)
	// A claimed attempt: the cancel signals it instead of terminalising it.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobs[0].ID, JobUpdate{State: JobRunning}); err != nil {
		t.Fatal(err)
	}
	fs := newFaultStore()
	fs.Memory = e.store
	e.svc.Store = fs
	fs.fail("put", 0)
	if _, err := e.svc.cancelRun(ctx(), "o", "r", run.ID, cancelWhy{Reason: CancelReasonOperator, Actor: "maint"}); err == nil {
		t.Fatal("a failing signal to a running job returned no error")
	}
}

func TestStampCancelRejectsUnknownAndCorruptHeaders(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.stampCancel(ctx(), "o", "r", "0000000000000000001-aaaaaa", cancelWhy{Reason: CancelReasonOperator}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stamp on an unknown run: %v", err)
	}
	run := e.createPush(t, testSHA, "main")[0]
	key := RunKey("o", "r", run.ID)
	meta, err := e.store.Head(ctx(), key)
	if err != nil || meta == nil {
		t.Fatalf("head %s: %v (nil=%v)", key, err, meta == nil)
	}
	if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
	if _, err := e.svc.stampCancel(ctx(), "o", "r", run.ID, cancelWhy{Reason: CancelReasonOperator}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("stamp on a corrupt run: %v", err)
	}
}
