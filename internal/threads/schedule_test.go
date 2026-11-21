package threads

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// scheduleEnv builds a service on a store with a pinned clock (the shared
// testEnv advances a second per call, which schedule math must not depend on).
func scheduleEnv(t *testing.T, at time.Time) (*testEnv, *time.Time) { //nolint:unparam // the clock is mutated by the day-rollover case
	t.Helper()
	e := newTestEnv(t)
	now := at
	e.svc.Now = func() time.Time { return now }
	e.git.refs["HEAD"] = testSHA
	e.git.branch = "main"
	return e, &now
}

// runsOf lists the run headers directly (test observability).
func runsOf(t *testing.T, e *testEnv) []*Run {
	t.Helper()
	ids, err := e.svc.runIDs(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("runIDs: %v", err)
	}
	out := make([]*Run, 0, len(ids))
	for _, id := range ids {
		run, lerr := e.svc.loadRun(context.Background(), "o", "r", id)
		if lerr != nil || run == nil {
			t.Fatalf("loadRun %s: %v", id, lerr)
		}
		out = append(out, run)
	}
	return out
}

// TestSweepScheduleFiresOncePerTick pins the core cron contract: the tick at or
// before `now` fires once, a second sweep in the same minute creates nothing,
// and the run is a `schedule` run on the default branch.
func TestSweepScheduleFiresOncePerTick(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC) // 03:00 tick + 17s
	e, nowp := scheduleEnv(t, at)

	res, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatalf("SweepSchedule: %v", err)
	}
	if res.Evaluated != 1 || res.Fired != 1 || len(res.Runs) != 1 {
		t.Fatalf("sweep = %+v, want one evaluated entry and one fired run", res)
	}
	run := res.Runs[0]
	if run.Event != EventSchedule || run.Branch != "main" || run.Ref != "refs/heads/main" || run.Actor != "schedule" {
		t.Fatalf("run = %+v, want a schedule run on refs/heads/main", run)
	}
	if run.WorkflowPath != ".spindle/workflows/ci.yml" {
		t.Fatalf("workflow path = %q", run.WorkflowPath)
	}
	// Same minute again: no second run.
	res, err = e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res.Fired != 0 {
		t.Fatalf("second sweep fired %d runs, want 0", res.Fired)
	}
	if got := len(runsOf(t, e)); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
	// The next day's tick fires exactly once more.
	*nowp = time.Date(2026, 11, 22, 3, 0, 5, 0, time.UTC)
	res, err = e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil || res.Fired != 1 {
		t.Fatalf("next-day sweep = %+v, %v", res, err)
	}
	if got := len(runsOf(t, e)); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

// TestSweepScheduleRestartDoesNotDoubleFire pins the persisted watermark: a
// fresh Service (every in-memory thing gone) over the same store fires nothing.
func TestSweepScheduleRestartDoesNotDoubleFire(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err != nil {
		t.Fatal(err)
	}
	// A restart: a NEW service instance over the same store, same clock.
	restarted := New(e.store, e.roles)
	restarted.Git = e.git
	restarted.Checks = e.checks
	restarted.Now = func() time.Time { return at }
	res, err := restarted.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fired != 0 {
		t.Fatalf("restart fired %d runs, want 0 (the watermark is the commit point)", res.Fired)
	}
	if got := len(runsOf(t, e)); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

// TestSweepScheduleTwoInstancesDedupeIntraMinute pins cross-instance dedupe: two
// instances sweeping the same tick concurrently create exactly one run.
func TestSweepScheduleTwoInstancesDedupeIntraMinute(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 30, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	second := New(e.store, e.roles)
	second.Git = e.git
	second.Checks = e.checks
	second.Now = func() time.Time { return at }

	var wg sync.WaitGroup
	fired := make([]int, 2)
	errs := make([]error, 2)
	svcs := []*Service{e.svc, second}
	for i, svc := range svcs {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			res, err := svc.SweepSchedule(context.Background(), "o", "r", nil)
			errs[i] = err
			if res != nil {
				fired[i] = res.Fired
			}
		}(i, svc)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("instance %d: %v", i, errs[i])
		}
	}
	if fired[0]+fired[1] != 1 {
		t.Fatalf("fired %v, want exactly one total", fired)
	}
	if got := len(runsOf(t, e)); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

// TestSweepScheduleSkipsDisabledAndUnscheduledFiles pins per-file isolation: a
// broken workflow (unsupported cron is a parse error) fires nothing, a file with
// no `schedule:` fires nothing, and neither hides the good one.
func TestSweepScheduleSkipsDisabledAndUnscheduledFiles(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	e.git.add(testSHA, ".spindle/workflows/pushonly.yml",
		"name: push only\non: [push]\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")
	e.git.add(testSHA, ".spindle/workflows/badcron.yml",
		"name: bad cron\non:\n  schedule:\n    - cron: \"0 3 * * MON\"\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")

	res, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evaluated != 1 || res.Fired != 1 {
		t.Fatalf("sweep = %+v, want only the one valid schedule evaluated", res)
	}
	runs := runsOf(t, e)
	if len(runs) != 1 || runs[0].Event != EventSchedule {
		t.Fatalf("runs = %+v, want one schedule run", runs)
	}
}

// TestSweepScheduleEmptyRepoAndPrune covers the no-HEAD no-op and the pruning of
// watermark entries whose workflow file disappeared.
func TestSweepScheduleEmptyRepoAndPrune(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	// No HEAD: an empty repository is a silent no-op.
	delete(e.git.refs, "HEAD")
	res, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil || res.Fired != 0 {
		t.Fatalf("empty repo sweep = %+v, %v", res, err)
	}
	e.git.refs["HEAD"] = testSHA
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err != nil {
		t.Fatal(err)
	}
	wm, err := e.svc.LoadScheduleWatermark(context.Background(), "o", "r")
	if err != nil || len(wm.Fired) != 1 {
		t.Fatalf("watermark = %+v, %v", wm, err)
	}
	// The workflow goes away (the file deleted on the default branch): the next
	// sweep has nothing to evaluate and prunes nothing (it must not forget a
	// watermark just because the file stopped firing).
	e.git.add(testSHA, ".spindle/workflows/ci.yml",
		"name: CI\non: [push]\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(runsOf(t, e)); got != 1 {
		t.Fatalf("runs = %d, want 1 (no new tick)", got)
	}
}

// TestSweepScheduleStoreErrorFailsClosed pins that a corrupt watermark is an
// error, never a double fire.
func TestSweepScheduleCorruptWatermark(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	if _, err := store.PutBytes(context.Background(), e.store, ScheduleKey("o", "r"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err == nil {
		t.Fatal("corrupt watermark accepted")
	}
	if got := len(runsOf(t, e)); got != 0 {
		t.Fatalf("runs = %d, want 0", got)
	}
}

// TestScheduleEntriesProjection covers the UI projection: sorted, and an entry
// the scheduler cannot evaluate is MARKED rather than silently dropped.
func TestScheduleEntriesProjection(t *testing.T) {
	if entries := ScheduleEntries(nil); len(entries) != 0 {
		t.Fatalf("nil workflow entries = %v", entries)
	}
	w := &workflow.Workflow{}
	w.On.ScheduleSet = true
	w.On.Schedule = []workflow.CronEntry{{Cron: "0 3 * * *"}, {Cron: "0 3 * * MON"}}
	entries := ScheduleEntries(w)
	want := []string{"0 3 * * *", "0 3 * * MON (unsupported)"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entries = %v, want %v", entries, want)
		}
	}
}
