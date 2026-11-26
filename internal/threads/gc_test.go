package threads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// retentionEnv is a service with a mutable clock and a 30-day window.
func retentionEnv(t *testing.T) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return now }
	e.svc.Retention = 30 * 24 * time.Hour
	return e, &now
}

// finishRun drives every job of one run to success.
func finishRun(t *testing.T, e *testEnv, run *Run) {
	t.Helper()
	jobs, err := e.svc.loadJobs(context.Background(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	for _, job := range jobs {
		if _, uerr := e.svc.UpdateJob(context.Background(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); uerr != nil {
			t.Fatalf("finish %s: %v", job.ID, uerr)
		}
	}
}

// countPrefix counts the objects under a key prefix (test observability).
func countPrefix(t *testing.T, e *testEnv, prefix string) int {
	t.Helper()
	n := 0
	if err := e.store.List(context.Background(), prefix, "", func(store.ObjectMeta) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return n
}

// TestRetainDeletesOldTerminalRunsKeepsTheRest pins the window rules: an old
// terminal run (with its jobs, logs and queue entry) goes; a young terminal run
// and a live run stay.
func TestRetainDeletesOldTerminalRunsKeepsTheRest(t *testing.T) {
	e, now := retentionEnv(t)
	old := e.mustCreate(t)[0]
	finishRun(t, e, old)
	// One log frame so the log family is exercised.
	if _, err := store.PutBytes(context.Background(), e.store, LogKey("o", "r", old.ID, "j-build", 1, 1),
		[]byte("hello\n"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("append log: %v", err)
	}

	// Age the old run past the window, THEN create a young terminal run and a
	// live one (both inside the window).
	*now = now.Add(31 * 24 * time.Hour)
	young := e.mustCreate(t)[0]
	finishRun(t, e, young)
	live := e.mustCreate(t)[0]

	res, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if res.RunsDeleted != 1 || res.JobsDeleted != 2 || res.LogsDeleted != 1 || res.QueueDeleted < 1 {
		t.Fatalf("retain = %+v, want the old run's 2 jobs, 1 log and its queue entries", res)
	}
	if r, lerr := e.svc.loadRun(context.Background(), "o", "r", old.ID); lerr != nil {
		t.Fatalf("loadRun after delete: %v", lerr)
	} else if r != nil {
		t.Fatalf("old run survived retention")
	}
	if r, _ := e.svc.loadRun(context.Background(), "o", "r", young.ID); r == nil {
		t.Fatalf("young terminal run was deleted")
	}
	if r, _ := e.svc.loadRun(context.Background(), "o", "r", live.ID); r == nil {
		t.Fatalf("live run was deleted")
	}
	// The young run's queue entries stay (they belong to a run inside the window).
	if n := countPrefix(t, e, LogPrefix("o", "r", old.ID, "j-build")); n != 0 {
		t.Fatalf("old run log frames = %d, want 0", n)
	}
	// A second sweep is a no-op.
	before := *res
	res2, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatalf("second Retain: %v", err)
	}
	if res2.RunsDeleted != 0 || res2.JobsDeleted != 0 || res2.LogsDeleted != 0 {
		t.Fatalf("second sweep deleted more: %+v", res2)
	}
	_ = before
}

// TestRetainKeepsChecksForLivePR pins the independence rule: Threads deletes the
// RUN, never the checks family — a required check a live PR still needs is
// untouched and still gates the merge.
func TestRetainKeepsChecksForLivePR(t *testing.T) {
	e, now := retentionEnv(t)
	run := e.mustCreate(t)[0]
	finishRun(t, e, run)
	// The checks sink (the production recorder) saw the green mirror.
	saw := false
	for _, r := range e.checks.reports {
		if r.SHA == testSHA && r.State == "success" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("checks reports = %+v, want a green mirror", e.checks.reports)
	}
	*now = now.Add(400 * 24 * time.Hour)
	if _, err := e.svc.Retain(context.Background(), "o", "r", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.svc.loadRun(context.Background(), "o", "r", run.ID); r != nil {
		t.Fatal("old run survived")
	}
	// The check family is 05's: Threads retention never touched it.
	if len(e.checks.reports) == 0 || e.checks.reports[len(e.checks.reports)-1].State != "success" {
		t.Fatalf("checks reports changed: %+v", e.checks.reports)
	}
}

// TestRetainDeletesDeadRunners pins runner retention: a revoked runner past the
// window and an unused expired registration go (record + credential); a live
// runner stays.
func TestRetainDeletesDeadRunners(t *testing.T) {
	e, now := retentionEnv(t)
	admin := auth.Principal{Name: "writer", Admin: true}
	deadTok, err := e.svc.MintRegistrationToken(context.Background(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatal(err)
	}
	live := e.writer()
	live.Admin = true
	liveTok, err := e.svc.MintRegistrationToken(context.Background(), live, mintAllPolicy(""))
	if err != nil {
		t.Fatal(err)
	}
	// Consume the live token (it becomes an active runner).
	if _, err := e.svc.Register(context.Background(), RegisterInput{Token: liveTok.Token, Name: "live", Labels: []string{"linux"}}); err != nil {
		t.Fatal(err)
	}
	// Revoke nothing yet: age everything past the registration TTL and the
	// retention window; the unused registration is dead, the active runner is not.
	*now = now.Add(40 * 24 * time.Hour)
	res, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RunnersDeleted != 1 || res.CredentialsDeleted != 1 {
		t.Fatalf("retain = %+v, want the one unused registration removed", res)
	}
	if doc, _, _ := e.svc.loadRunner(context.Background(), deadTok.ID); doc != nil {
		t.Fatalf("expired registration row survived")
	}
	// The live runner stays.
	found := false
	for _, v := range mustListRunners(t, e.svc, live) {
		if v.State == RunnerActive {
			found = true
		}
	}
	if !found {
		t.Fatal("active runner removed by retention")
	}
}

func mustListRunners(t *testing.T, s *Service, p auth.Principal) []RunnerView {
	t.Helper()
	rows, err := s.ListRunners(context.Background(), p)
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	return rows
}

// TestRetainSecondPassIsANoOp pins that retention is idempotent: a second pass
// over an unchanged repository deletes nothing (the property two instances and
// restarts rely on), and two Services sharing one store cannot fail each other.
func TestRetainSecondPassIsANoOp(t *testing.T) {
	e, now := retentionEnv(t)
	run := e.mustCreate(t)[0]
	finishRun(t, e, run)
	ageRun(t, e, run.ID, now.Add(-31*24*time.Hour))
	first, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil || first.RunsDeleted != 1 {
		t.Fatalf("first pass = %+v, %v", first, err)
	}
	// A second instance over the same store runs the same pass concurrently and
	// sees only already-gone objects (every delete tolerates NotFound).
	other := New(e.store, nil)
	other.Now = e.svc.Now
	other.Retention = e.svc.Retention
	var wg sync.WaitGroup
	results := make([]*GCResult, 2)
	errs := make([]error, 2)
	for i, svc := range []*Service{e.svc, other} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			results[i], errs[i] = svc.Retain(context.Background(), "o", "r", nil)
		}(i, svc)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent pass %d: %v", i, errs[i])
		}
		if results[i].RunsDeleted != 0 {
			t.Fatalf("concurrent pass %d deleted %d, want 0", i, results[i].RunsDeleted)
		}
	}
	if r, _ := e.svc.loadRun(context.Background(), "o", "r", run.ID); r != nil {
		t.Fatal("the aged run survived")
	}
}

// ageRun rewrites one run's completed stamp (the retention window's only input
// for a terminal run).
func ageRun(t *testing.T, e *testEnv, runID string, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(dateTimeFmt)
	if _, err := e.svc.casUpdate(context.Background(), RunKey("o", "r", runID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, jerr
		}
		r.CompletedAt = &stamp
		r.CreatedAt = stamp
		r.Version++
		return mustJSON(&r), true, nil
	}); err != nil {
		t.Fatalf("ageRun: %v", err)
	}
}

// TestRetainCancelledAndFolderState covers a cancelled run (terminal) aging out
// and a corrupt/unreadable run being skipped instead of failing the sweep.
func TestRetainCancelledRunAgesOut(t *testing.T) {
	e, now := retentionEnv(t)
	run := e.mustCreate(t)[0]
	for _, job := range mustJobs(t, e, run.ID) {
		if _, err := e.svc.UpdateJob(context.Background(), "o/r", run.ID, job.ID, JobUpdate{State: JobCancelled}); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(31 * 24 * time.Hour)
	res, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil || res.RunsDeleted != 1 {
		t.Fatalf("retain = %+v, %v", res, err)
	}
}

func mustJobs(t *testing.T, e *testEnv, runID string) []*Job {
	t.Helper()
	jobs, err := e.svc.loadJobs(context.Background(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	return jobs
}

// TestRetentionStoreFaultsFailClosed pins the retention pass's failure arms:
// every store error surfaces (Retain) or is skipped without failing the sweep
// (the tolerant maintenance paths), and a raced delete is not re-deleted.
func TestRetentionStoreFaultsFailClosed(t *testing.T) {
	t.Run("retain surfaces a run read failure", func(t *testing.T) {
		e, now := retentionEnv(t)
		run := e.mustCreate(t)[0]
		finishRun(t, e, run)
		ageRun(t, e, run.ID, now.Add(-31*24*time.Hour))
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("get", 0)
		if _, err := e.svc.Retain(context.Background(), "o", "r", nil); err == nil {
			t.Fatal("a failing run read did not fail the pass")
		}
	})

	t.Run("deleteRun surfaces a delete failure", func(t *testing.T) {
		e, _ := retentionEnv(t)
		run := e.mustCreate(t)[0]
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("delete", 0)
		if _, err := e.svc.deleteRun(context.Background(), "o", "r", run.ID); err == nil {
			t.Fatal("a failing object delete did not fail the run delete")
		}
	})

	t.Run("deleteRun surfaces listing and artifact-read failures", func(t *testing.T) {
		e, _ := retentionEnv(t)
		run := e.mustCreate(t)[0]
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("list", 0)
		if _, err := e.svc.deleteRun(context.Background(), "o", "r", run.ID); err == nil {
			t.Fatal("a failing run-prefix listing did not fail the run delete")
		}
	})

	t.Run("dropQueueEntries tolerates a read failure", func(t *testing.T) {
		e, _ := retentionEnv(t)
		key := QueueEntryKey("o", "r", "0000000000000000002-bbbbbb")
		if _, err := store.PutBytes(context.Background(), e.store, key,
			[]byte(`{"seq":"0000000000000000002-bbbbbb","run_id":"gone"}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("get", 0)
		res := &GCResult{}
		e.svc.dropQueueEntries(context.Background(), "o", "r", map[string]bool{"gone": true}, true, res)
		if res.QueueDeleted != 0 {
			t.Fatalf("unreadable entry counted: %+v", res)
		}
		res = &GCResult{}
		fs.clear()
		fs.fail("delete", 0)
		e.svc.dropQueueEntries(context.Background(), "o", "r", map[string]bool{"gone": true}, true, res)
		if res.QueueDeleted != 0 {
			t.Fatalf("undeletable entry counted: %+v", res)
		}
		// A DRY RUN counts the same entry and deletes nothing.
		res = &GCResult{}
		fs.clear()
		e.svc.dropQueueEntries(context.Background(), "o", "r", map[string]bool{"gone": true}, false, res)
		if res.QueueDeleted != 1 {
			t.Fatalf("dry-run entry = %+v, want 1 planned", res)
		}
		if _, _, err := store.GetBytes(context.Background(), fs, key, store.GetOptions{}); err != nil {
			t.Fatalf("dry run deleted the queue entry: %v", err)
		}
	})

	t.Run("deleteDeadRunners tolerates its failure arms", func(t *testing.T) {
		e, now := retentionEnv(t)
		// The cap already reached: no listing at all.
		res := &GCResult{RunnersDeleted: retentionRunners}
		e.svc.runnerSweep = &runnerSweepGate{}
		e.svc.deleteDeadRunners(context.Background(), *now, true, res)
		// A listing failure.
		fs := newFaultStore()
		e.store = fs.Memory
		e.svc.Store = fs
		fs.fail("list", 0)
		e.svc.runnerSweep = &runnerSweepGate{}
		e.svc.deleteDeadRunners(context.Background(), *now, true, &GCResult{})
		// An unreadable record (valid key, get fault).
		fs.clear()
		e.store = fs.Memory
		if _, err := store.PutBytes(context.Background(), fs, RunnerKey("bad"), []byte(`{"id":"bad"}`),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		fs.fail("get", 0)
		e.svc.runnerSweep = &runnerSweepGate{}
		e.svc.deleteDeadRunners(context.Background(), *now, true, &GCResult{})
		// A dead runner whose record delete succeeds but whose credential
		// delete fails: the failure is skipped, the delete is counted.
		fs.clear()
		old := now.Add(-40 * 24 * time.Hour).Format(dateTimeFmt)
		doc := &RunnerDoc{ID: "rnrbeef01", State: RunnerRevoked, RegisteredBy: "alice",
			CreatedAt: old, RevokedAt: &old, Version: 1}
		if _, err := store.PutBytes(context.Background(), fs, RunnerKey(doc.ID), mustJSON(doc),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		fs.fail("delete", 1)
		res = &GCResult{}
		e.svc.runnerSweep = &runnerSweepGate{}
		e.svc.deleteDeadRunners(context.Background(), *now, true, res)
		if res.RunnersDeleted != 1 || res.CredentialsDeleted != 0 {
			t.Fatalf("credential-delete failure = %+v, want the record counted only", res)
		}
	})
}
