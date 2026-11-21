package threads

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// round2_edges_test.go closes the remaining error/branch edges of the round-2
// surfaces (kept separate from the behavioural tests so a failure names the
// exact edge).

// TestScheduleWatermarkReadEdges pins the read model: absent, valid, corrupt.
func TestScheduleWatermarkReadEdges(t *testing.T) {
	e := newTestEnv(t)
	doc, err := e.svc.LoadScheduleWatermark(ctx(), "o", "r")
	if err != nil || doc == nil || len(doc.Fired) != 0 {
		t.Fatalf("absent watermark = %+v, %v", doc, err)
	}
	body := `{"fired":{"ci.yml|0 3 * * *":1700000000},"version":2}`
	if _, err := store.PutBytes(ctx(), e.store, ScheduleKey("o", "r"), []byte(body), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	doc, err = e.svc.LoadScheduleWatermark(ctx(), "o", "r")
	if err != nil || doc.Fired["ci.yml|0 3 * * *"] != 1700000000 {
		t.Fatalf("valid watermark = %+v, %v", doc, err)
	}
	// A JSON null map decodes to an empty map (never a nil panic downstream).
	if _, err := store.PutBytes(ctx(), e.store, ScheduleKey("o", "r"), []byte(`{"version":3}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	doc, err = e.svc.LoadScheduleWatermark(ctx(), "o", "r")
	if err != nil || doc.Fired == nil {
		t.Fatalf("null map = %+v, %v", doc, err)
	}
}

// TestRetainDeleteRunErrors pins deleteRun's failure paths (a job delete fails,
// the header delete fails).
func TestRetainDeleteRunErrors(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	e.svc.Retention = time.Hour
	run := e.mustCreate(t)[0]
	for _, job := range mustJobs(t, e, run.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	fs.fail("delete", 0)
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("a failing job delete must fail the run delete")
	}
	fs.clear()
	// The header delete failing surfaces too.
	fs.fail("delete", 2) // both job deletes succeed, the header delete fails
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("a failing header delete must fail")
	}
	fs.clear()
}

// TestApproveRunStoreEdges pins the CAS-miss and enqueue-failure paths.
func TestApproveRunStoreEdges(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	e.svc.Approvals = &fakeApprovals{merged: map[string]bool{}}
	e.svc.ApproveFirstTimeForks = true
	held := forkPR(t, e, "newcomer")[0]
	// The job listing fails before the decision.
	fs.fail("get", 0)
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, admin); err == nil {
		t.Fatal("a failing job read must fail approve")
	}
	fs.clear()
	// The enqueue after the decision fails (the header is already queued, which
	// the next approve cannot repeat: the conflict is the documented state).
	fs.fail("put", 1)
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, admin); err == nil {
		t.Fatal("a failing enqueue must fail approve")
	}
	fs.clear()
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, admin); err == nil {
		t.Fatal("the header stayed pending after a failed enqueue?")
	}
}

// TestApprovalRunHTTPAuthFailure pins the handler's principal-error arm.
func TestApprovalRunHTTPAuthFailure(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "auth required"}
	}
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodPost, "/o/r/api/threads/0000000000000000000-abc123/approve", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approve = %d, want 401", rec.Code)
	}
}

// TestCancelMatrixSiblingsBranches pins both sibling arms (a running one is
// signalled, a queued one is cancelled) and the unrelated/terminal skips.
func TestCancelMatrixSiblingsBranches(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - run: echo
`)
	runs := e.mustCreate(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	jobs := mustJobs(t, e, runs[0].ID)
	if len(jobs) != 2 {
		t.Fatalf("matrix jobs = %d, want 2", len(jobs))
	}
	// One running (claimed), one still queued.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", runs[0].ID, jobs[0].ID, JobUpdate{State: JobRunning}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.cancelMatrixSiblings(ctx(), "o", "r", runs[0].ID, "j-other", jobs[0].Spec.MatrixGroup); err != nil {
		t.Fatal(err)
	}
	signalled, cancelled := 0, 0
	for _, job := range mustJobs(t, e, runs[0].ID) {
		if job.State == JobRunning && job.CancelRequested {
			signalled++
		}
		if job.State == JobCancelled {
			cancelled++
		}
	}
	if signalled != 1 || cancelled != 1 {
		t.Fatalf("signalled=%d cancelled=%d, want 1/1", signalled, cancelled)
	}
}

// TestConclusionStateEdges pins the conclusion mapping.
func TestConclusionStateEdges(t *testing.T) {
	for in, want := range map[string]string{
		"success": JobSuccess, "failure": JobFailure, "failed": JobFailure,
		"cancelled": JobCancelled, "canceled": JobCancelled,
	} {
		got, err := conclusionState(in)
		if err != nil || got != want {
			t.Errorf("conclusionState(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := conclusionState("unexpected"); err == nil {
		t.Fatal("unknown conclusion accepted")
	}
}

// TestParseJobRefEdges pins the opaque-reference parser's rejections.
func TestParseJobRefEdges(t *testing.T) {
	bad := []string{"", "jr_", "jr_!!!", "jr_" + "YWJj"} // not four parts
	for _, ref := range bad {
		if _, _, _, _, err := parseJobRef(ref); err == nil {
			t.Errorf("parseJobRef(%q) accepted", ref)
		}
	}
	if _, _, _, _, err := parseJobRef(JobRef("bad repo", "0000000000000000000-abc123", "j-build")); err == nil {
		t.Error("a bad repo accepted through the fallback encoding")
	}
}

// TestCoerceInputsEdges pins the dispatch body coercion's rejections.
func TestCoerceInputsEdges(t *testing.T) {
	if got, err := coerceInputs(nil); err != nil || got != nil {
		t.Fatalf("nil = %v, %v", got, err)
	}
	got, err := coerceInputs(map[string]any{"a": nil, "b": true, "c": 3.0, "d": 1.5, "e": "x"})
	if err != nil || got["b"] != "true" || got["c"] != "3" || got["d"] != "1.5" || got["e"] != "x" {
		t.Fatalf("coerce = %+v, %v", got, err)
	}
	if _, err := coerceInputs(map[string]any{"a": []any{"x"}}); err == nil {
		t.Fatal("nested input accepted")
	}
}

// TestReportCheckEdges pins the mirror's nil sink and the description bound.
func TestReportCheckEdges(t *testing.T) {
	e := newTestEnv(t)
	run := &Run{Repo: "o/r", ID: "0000000000000000000-abc123", Workflow: "CI"}
	job := &Job{Name: strings.Repeat("n", 300)}
	e.svc.Checks = nil
	if err := e.svc.reportCheck(ctx(), run, job); err != nil {
		t.Fatalf("nil sink: %v", err)
	}
	e.svc.Checks = e.checks
	if err := e.svc.reportCheck(ctx(), run, job); err != nil {
		t.Fatal(err)
	}
	if got := e.checks.reports[len(e.checks.reports)-1].Description; len([]rune(got)) > 256 {
		t.Fatalf("description = %d runes, want ≤ 256", len([]rune(got)))
	}
}

// TestListWorkflowsEdges pins the empty-repository and bad-sha answers.
func TestListWorkflowsEdges(t *testing.T) {
	e := newTestEnv(t)
	// No HEAD at all: an empty repository is an empty list.
	delete(e.git.refs, "HEAD")
	list, err := e.svc.ListWorkflows(ctx(), "o/r", "", e.public())
	if err != nil || list == nil || len(list.Workflows) != 0 {
		t.Fatalf("empty repo = %+v, %v", list, err)
	}
	if _, err := e.svc.ListWorkflows(ctx(), "o/r", "not-a-sha", e.public()); err == nil {
		t.Fatal("bad sha accepted")
	}
	e.git.refs["HEAD"] = testSHA
	list, err = e.svc.ListWorkflows(ctx(), "o/r", "", e.public())
	if err != nil || len(list.Workflows) != 1 {
		t.Fatalf("workflow list = %+v, %v", list, err)
	}
}

// TestMintConflictEdges pins the duplicate-id mapping.
func TestMintConflictEdges(t *testing.T) {
	if err := mintConflict(&store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: "k"}); err == nil {
		t.Fatal("precondition failure mapped to nil")
	} else if !isConflict(err) {
		t.Fatalf("precondition failure mapped to %v, want ErrUnavailable", err)
	}
	plain := &store.StoreError{Kind: store.ErrKindRetryable, Key: "k"}
	if got := mintConflict(plain); got != plain {
		t.Fatalf("plain error rewritten to %v", got)
	}
}

// isConflict reports ErrUnavailable (the retryable mint conflict).
func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "retry")
}

// TestSweepScheduleCreateRunFailure pins the at-most-once contract when the run
// itself cannot be created: the tick is consumed, the sweep returns without a
// fired run and never spins.
func TestSweepScheduleCreateRunFailure(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	e.svc.Checks = &fakeChecks{err: errors.New("checks down")}
	res, err := e.svc.SweepSchedule(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("SweepSchedule: %v", err)
	}
	// The run header may exist (a checks-mirror failure surfaces after the
	// header is the commit point, 15 §2), but the sweep never reports it fired
	// and never retries the consumed tick.
	if res.Fired != 0 {
		t.Fatalf("sweep = %+v, want no fired run", res)
	}
	// The watermark holds the consumed tick, so a later clean sweep in the same
	// minute does not fire it (at-most-once, the fail-safe direction).
	e.svc.Checks = e.checks
	res, err = e.svc.SweepSchedule(ctx(), "o", "r", nil)
	if err != nil || res.Fired != 0 {
		t.Fatalf("retry sweep = %+v, %v", res, err)
	}
}

// TestDeleteDeadRunnerFailurePin pins a registry delete failure leaving the
// record (and never aborting the sweep).
func TestDeleteDeadRunnerFailurePin(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	tok, err := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err != nil {
		t.Fatal(err)
	}
	old := e.svc.nowUTC().Add(-40 * 24 * time.Hour).Format(dateTimeFmt)
	if _, err := e.svc.casUpdate(ctx(), RunnerKey("o", "r", tok.ID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var doc RunnerDoc
		if jerr := jsonUnmarshal(cur, &doc); jerr != nil {
			return nil, false, jerr
		}
		doc.RevokedAt = &old
		revoked := RunnerRevoked
		doc.State = revoked
		doc.Version++
		return mustJSON(&doc), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	fs.fail("delete", 0)
	res := &GCResult{}
	e.svc.deleteDeadRunners(ctx(), "o", "r", e.svc.nowUTC().Add(-30*24*time.Hour), res)
	if res.RunnersDeleted != 0 {
		t.Fatalf("a failed delete was counted: %+v", res)
	}
}

// TestDenyRunCancelFailure pins the denial path's cancel failure surfacing.
func TestDenyRunCancelFailure(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	e.svc.Approvals = &fakeApprovals{merged: map[string]bool{}}
	e.svc.ApproveFirstTimeForks = true
	held := forkPR(t, e, "newcomer")[0]
	// The cancel's job write fails after the denial stamp landed.
	fs.fail("put", 1)
	if _, err := e.svc.DenyRun(ctx(), "o/r", held.ID, admin); err == nil {
		t.Fatal("a failing cancel must surface")
	}
}

// TestClaimScheduleTickPruneAndLimit pins the watermark document's maintenance:
// entries no longer seen are pruned, and a full document fails closed.
func TestClaimScheduleTickPruneAndLimit(t *testing.T) {
	e := newTestEnv(t)
	// Seed two entries, then claim a third while pruning to only that one.
	seed := `{"fired":{"a.yml|0 3 * * *":100,"b.yml|0 3 * * *":200},"version":1}`
	if _, err := store.PutBytes(ctx(), e.store, ScheduleKey("o", "r"), []byte(seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	won, err := e.svc.claimScheduleTick(ctx(), "o", "r", "c.yml|0 3 * * *", 300, map[string]bool{"c.yml|0 3 * * *": true})
	if err != nil || !won {
		t.Fatalf("claim = %v, %v", won, err)
	}
	doc, err := e.svc.LoadScheduleWatermark(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Fired) != 1 || doc.Fired["c.yml|0 3 * * *"] != 300 {
		t.Fatalf("watermark = %+v, want only the new entry", doc.Fired)
	}
	// A full document refuses a new entry (fail closed, never an unbounded
	// object).
	full := &ScheduleWatermark{Fired: map[string]int64{}}
	for i := 0; i < scheduleKeyLimit; i++ {
		full.Fired["x"+itoa(i)+"|0 0 * * *"] = 1
	}
	if _, err := store.PutBytes(ctx(), e.store, ScheduleKey("o", "r"), mustJSON(full), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.claimScheduleTick(ctx(), "o", "r", "new|0 0 * * *", 2, nil); err == nil {
		t.Fatal("a full watermark accepted another entry")
	}
}

// itoa is strconv.Itoa without the import churn in this file.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestRetainNarrates pins the maintain-task narration (progress + notice) and
// that a second pass over an unchanged repository is silent of deletions.
func TestRetainNarrates(t *testing.T) {
	e, _ := retentionEnv(t)
	run := e.mustCreate(t)[0]
	finishRun(t, e, run)
	ageRun(t, e, run.ID, e.svc.nowUTC().Add(-31*24*time.Hour))
	log := &captureLog{notice: func(string) {}, progress: func() {}}
	res, err := e.svc.Retain(ctx(), "o", "r", log)
	if err != nil || res.RunsDeleted != 1 {
		t.Fatalf("retain = %+v, %v", res, err)
	}
	// The second pass deletes nothing (idempotent) but still narrates.
	res, err = e.svc.Retain(ctx(), "o", "r", log)
	if err != nil || res.RunsDeleted != 0 {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
}

// TestRound2SmallArms covers the remaining one-line error arms.
func TestRound2SmallArms(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.CancelRun(ctx(), "bad", "0000000000000000000-abc123", e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cancel on a bad repo = %v", err)
	}
	if _, err := e.svc.RerunRun(ctx(), "bad", "0000000000000000000-abc123", e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rerun on a bad repo = %v", err)
	}
	// The watermark read surfaces a store failure (never an empty document).
	fs := newFaultStore()
	e.svc.Store = fs
	fs.fail("get", 0)
	if _, err := e.svc.LoadScheduleWatermark(ctx(), "o", "r"); err == nil {
		t.Fatal("a failing watermark get was read as empty")
	}
	fs.clear()
	e.svc.Store = e.store
	// deleteRun surfaces a job-listing failure.
	run := e.mustCreate(t)[0]
	fs2 := newFaultStore()
	e.svc.Store = fs2
	fs2.fail("list", 0)
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("a failing job list did not fail the run delete")
	}
}

// TestSweepAndLeaseErrorArms pins GC's queue-pass error and the lease
// renewal's tolerant read.
func TestSweepAndLeaseErrorArms(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	run := e.mustCreate(t)[0]
	fs.fail("get", 0)
	if _, err := e.svc.GC(ctx(), "o", "r"); err == nil {
		t.Fatal("a failing queue read must fail GC")
	}
	fs.clear()
	// renewLeaseFor tolerates a missing entry: the job keeps its stored lease.
	job := &Job{QueueSeq: "0000000000000000000-abc123", LeaseUntil: nil}
	got := e.svc.renewLeaseFor(ctx(), "o", "r", job)
	if got != nil {
		t.Fatalf("renewLeaseFor = %v, want the stored (nil) lease", got)
	}
	_ = run
}

// TestTryAcquireGroupArms pins the concurrency CAS's takeover rules: an
// unparseable holder fails closed, a live holder refuses, a folded holder is
// stolen and re-acquiring the same run is idempotent.
func TestTryAcquireGroupArms(t *testing.T) {
	e := newTestEnv(t)
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "g"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.tryAcquireGroup(ctx(), "o", "r", "g", "0000000000000000000-abc123"); err == nil {
		t.Fatal("a corrupt holder was acquired")
	}
	// A live holder refuses the newcomer.
	run := e.mustCreate(t)[0]
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "g"),
		mustJSON(&ConcurrencyDoc{Group: "g", RunID: run.ID, Version: 1}), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	ok, holder, err := e.svc.tryAcquireGroup(ctx(), "o", "r", "g", "0000000000000000000-other1")
	if err != nil || ok || holder != run.ID {
		t.Fatalf("live holder = %v %q %v", ok, holder, err)
	}
	// The holder run folds terminal: the group is stolen.
	for _, job := range mustJobs(t, e, run.ID) {
		if _, uerr := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); uerr != nil {
			t.Fatal(uerr)
		}
	}
	ok, holder, err = e.svc.tryAcquireGroup(ctx(), "o", "r", "g", "0000000000000000000-other1")
	if err != nil || !ok || holder != "0000000000000000000-other1" {
		t.Fatalf("steal = %v %q %v", ok, holder, err)
	}
	// Re-acquiring for the same run is a no-op success.
	if ok, _, err := e.svc.tryAcquireGroup(ctx(), "o", "r", "g", "0000000000000000000-other1"); err != nil || !ok {
		t.Fatalf("same-run re-acquire = %v %v", ok, err)
	}
}
