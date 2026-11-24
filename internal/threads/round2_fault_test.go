package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// round2_fault_test.go injects store faults into the round-2 paths: every one
// must fail closed (an unreadable watermark must never fire, an unreadable
// evidence source must hold the run) and a retention failure must surface
// rather than silently keep deleting.

// TestFaultsOnRetention runs a retention pass with each store operation failing
// in turn.
func TestFaultsOnRetention(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	e.svc.Retention = time.Hour
	run := e.mustCreate(t)[0]
	for _, job := range mustJobs(t, e, run.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	ageRun(t, e, run.ID, e.svc.nowUTC().Add(-2*time.Hour))

	// The run LIST failing fails the sweep.
	fs.fail("list", 0)
	if _, err := e.svc.Retain(ctx(), "o", "r", nil); err == nil {
		t.Fatal("Retain with a failing list must fail")
	}
	fs.clear()
	// The job LIST (log family) failing fails the run delete.
	fs.fail("list", 1) // the run listing succeeds, the first job's log listing fails
	if _, err := e.svc.Retain(ctx(), "o", "r", nil); err == nil {
		t.Fatal("Retain with a failing log list must fail")
	}
	fs.clear()
	// A failing delete of a job record fails the pass.
	fs.fail("delete", 0)
	if _, err := e.svc.Retain(ctx(), "o", "r", nil); err == nil {
		t.Fatal("Retain with a failing delete must fail")
	}
	fs.clear()
	if _, err := e.svc.Retain(ctx(), "o", "r", nil); err != nil {
		t.Fatalf("clean Retain: %v", err)
	}
}

// TestFaultsOnSchedule pins that an unreadable or unwritable watermark can
// never produce a run.
func TestFaultsOnSchedule(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	e.svc.Now = func() time.Time { return time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC) }
	e.git.refs["HEAD"] = testSHA
	// The watermark read failing fails the sweep (and fires nothing).
	fs.fail("get", 0)
	if _, err := e.svc.SweepSchedule(ctx(), "o", "r", nil); err == nil {
		t.Fatal("SweepSchedule with a failing watermark read must fail")
	}
	fs.clear()
	// The watermark write failing fails the sweep (and fires nothing).
	fs.fail("put", 0)
	if _, err := e.svc.SweepSchedule(ctx(), "o", "r", nil); err == nil {
		t.Fatal("SweepSchedule with a failing watermark write must fail")
	}
	fs.clear()
	if got := len(runsOf(t, e)); got != 0 {
		t.Fatalf("runs = %d, want 0", got)
	}
	// The clean pass fires.
	if res, err := e.svc.SweepSchedule(ctx(), "o", "r", nil); err != nil || res.Fired != 1 {
		t.Fatalf("clean sweep = %+v, %v", res, err)
	}
	// A watermark read failure after a fire is an error, never a re-fire.
	fs.fail("get", 0)
	if _, err := e.svc.SweepSchedule(ctx(), "o", "r", nil); err == nil {
		t.Fatal("SweepSchedule with a failing watermark read must fail")
	}
	fs.clear()
	if got := len(runsOf(t, e)); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

// TestFaultsOnApprovalAndJobToken pins the gate's and the credential's
// fail-closed reads.
func TestFaultsOnApprovalAndJobToken(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	e.svc.Approvals = &fakeApprovals{merged: map[string]bool{}}
	e.svc.ApproveFirstTimeForks = true
	held := forkPR(t, e, "newcomer")[0]

	// The approval CAS failing surfaces.
	fs.fail("put", 0)
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, admin); err == nil {
		t.Fatal("ApproveRun with a failing write must fail")
	}
	fs.clear()
	// Deny: the denial stamp fails, then the cancel path does.
	denied := forkPR(t, e, "other")[0]
	fs.fail("put", 0)
	if _, err := e.svc.DenyRun(ctx(), "o/r", denied.ID, admin); err == nil {
		t.Fatal("DenyRun with a failing write must fail")
	}
	fs.clear()
	if _, err := e.svc.DenyRun(ctx(), "o/r", denied.ID, admin); err != nil {
		t.Fatalf("clean deny: %v", err)
	}
	fs.clear()

	// The job-token resolver fails closed when the job record is unreadable.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run := e.mustCreate(t)[0]
	_, view := startJob(t, e, run)
	fs.fail("get", 0)
	if _, aerr, ok := e.svc.resolveCloneCredential(ctx(), view.Token); !ok || aerr == nil {
		t.Fatalf("unreadable job accepted the token (ok=%v aerr=%v)", ok, aerr)
	}
	fs.clear()
	if _, aerr, ok := e.svc.resolveCloneCredential(ctx(), view.Token); !ok || aerr != nil {
		t.Fatalf("clean resolve failed: %v", aerr)
	}
	// A non-job token is never claimed.
	if _, _, ok := e.svc.resolveCloneCredential(ctx(), "wrt_x.y.z"); ok {
		t.Fatal("a non-job token was claimed")
	}
}

// TestApprovalHTTPVerbs pins the JSON API surface (approve/deny, role gate,
// conflict and not-found mapping) — the page calls the same service verbs.
func TestApprovalHTTPVerbs(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Approvals = &fakeApprovals{merged: map[string]bool{}}
	e.svc.ApproveFirstTimeForks = true
	h := &Handler{Svc: e.svc}
	admin := auth.Principal{Name: "admin", Admin: true}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	held := forkPR(t, e, "newcomer")[0]

	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodPost, "/o/r/api/threads/"+held.ID+"/approve", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodPost, "/o/r/api/threads/"+held.ID+"/approve", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodPost, "/o/r/api/threads/"+held.ID+"/deny", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("deny of an approved run = %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/o/r/api/threads/"+held.ID+"/deny", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET deny = %d, want 405", rec.Code)
	}
}

// TestListRunsFilterQuery pins the API filter parameters end to end.
func TestListRunsFilterQuery(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	e.mustCreate(t)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/o/r/api/threads?state=success", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered list = %d", rec.Code)
	}
	if got := rec.Body.String(); got == "" || !containsAll(got, `"runs":[]`) {
		t.Fatalf("filtered list body = %s", got)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestDropQueueEntriesEdges pins the maintenance LIST's tolerant paths (a
// corrupt entry is skipped, a store failure is not fatal).
func TestDropQueueEntriesEdges(t *testing.T) {
	e := newTestEnv(t)
	if _, err := store.PutBytes(ctx(), e.store, QueuePrefix("o", "r")+"0000000000000000001-aaaaaa.json",
		[]byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	res := &GCResult{}
	e.svc.dropQueueEntries(ctx(), "o", "r", map[string]bool{"gone": true}, res)
	if res.QueueDeleted != 0 {
		t.Fatalf("corrupt entry counted: %+v", res)
	}
	// A listing failure is silent.
	fs := newFaultStore()
	e.svc.Store = fs
	fs.fail("list", 0)
	e.svc.dropQueueEntries(ctx(), "o", "r", map[string]bool{"gone": true}, res)
}

// TestDeleteDeadRunnersEdges pins the tolerant paths of the runner sweep.
func TestDeleteDeadRunnersEdges(t *testing.T) {
	e := newTestEnv(t)
	// A corrupt registry object is skipped, never fatal.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("bad"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	res := &GCResult{}
	e.svc.deleteDeadRunners(ctx(), time.Now().Add(-time.Hour), res)
	if res.RunnersDeleted != 0 {
		t.Fatalf("corrupt runner counted: %+v", res)
	}
	// A cancelled context stops the loop.
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	e.svc.deleteDeadRunners(cctx, time.Now().Add(-time.Hour), res)
}
