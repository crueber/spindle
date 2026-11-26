package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// threads_sweep_test.go pins the maintainer half of round-2 pass 2 at the
// composition seam: the sweepers the maintainer loop runs are exactly the
// `threads-gc` and `threads-schedule` task kinds, each due-gated, and both
// bodies do their job through the real service.

const cronWorkflow = `name: Nightly
on:
  schedule:
    - cron: "* * * * *"
jobs:
  tick:
    runs-on: linux
    steps:
      - run: echo tick
`

// sweepEnv is a service over a real-ish git fake with a schedule workflow.
func sweepEnv(t *testing.T) (*threads.Service, *triggerGit) {
	t.Helper()
	g := newTriggerGit()
	g.add("1111111111111111111111111111111111111111", ".spindle/workflows/nightly.yml", cronWorkflow)
	g.refs["HEAD"] = "1111111111111111111111111111111111111111"
	g.refs["refs/heads/main"] = "1111111111111111111111111111111111111111"
	svc := triggerService(t, g)
	return svc, g
}

// TestThreadsSweepersAreTheRegisteredKinds pins the Threads sweepers the
// maintainer runs (a renamed kind would orphan the pass silently): the queue
// repair and the retention pass share `threads-gc` (two per-repo cadences), and
// cron evaluation is `threads-schedule`.
func TestThreadsSweepersAreTheRegisteredKinds(t *testing.T) {
	svc, _ := sweepEnv(t)
	sw := newThreadsSweepers(svc)
	if len(sw) != 4 {
		t.Fatalf("sweepers = %d, want 4", len(sw))
	}
	kinds := map[string]bool{}
	var gc int
	for _, s := range sw {
		kinds[s.Kind()] = true
		if s.Kind() == threads.KindThreadsGC {
			gc++
		}
	}
	if gc != 2 || !kinds[threads.KindThreadsGC] || !kinds[threads.KindThreadsSchedule] ||
		!kinds[threads.KindThreadsQueueReconcile] {
		t.Fatalf("kinds = %v (gc sweepers = %d)", kinds, gc)
	}
	// The due gate: due once, then not until the cadence elapses.
	for _, s := range sw {
		if !s.(*threadsSweeper).Due("o/r") {
			t.Fatalf("%s: first Due = false", s.Kind())
		}
		if s.(*threadsSweeper).Due("o/r") {
			t.Fatalf("%s: Due twice inside the cadence", s.Kind())
		}
		// Another repository has its own gate: one interval sweeps EVERY
		// repository, not just the first one (the round-3 retention defect).
		if !s.(*threadsSweeper).Due("o/other") {
			t.Fatalf("%s: a fresh repo was not due", s.Kind())
		}
	}
	// The serving composition lists them (with no packages service wired).
	list := maintainSweepers(&collabWiring{threadsSvc: svc})
	found := map[string]bool{}
	for _, s := range list {
		found[s.Kind()] = true
	}
	if !found[threads.KindThreadsGC] || !found[threads.KindThreadsSchedule] {
		t.Fatalf("maintainSweepers = %v", found)
	}
}

// TestThreadsScheduleSweeperFiresOnce pins the cron task end to end through the
// sweeper the maintainer calls, and the watermark's no-double-fire rule.
func TestThreadsScheduleSweeperFiresOnce(t *testing.T) {
	svc, _ := sweepEnv(t)
	sw := newThreadsSweepers(svc)
	var schedule *threadsSweeper
	for _, s := range sw {
		if s.Kind() == threads.KindThreadsSchedule {
			schedule = s.(*threadsSweeper)
		}
	}
	if schedule == nil {
		t.Fatal("no schedule sweeper")
	}
	ctx := context.Background()
	if err := schedule.Sweep(ctx, "o/r", nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	runs := awaitRuns(t, svc, 1)
	if runs[0].Event != threads.EventSchedule {
		t.Fatalf("run event = %q, want schedule", runs[0].Event)
	}
	// A second pass in the same minute fires nothing.
	if err := schedule.Sweep(ctx, "o/r", nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	page, err := svc.ListRuns(ctx, "o/r", auth.Principal{Name: "reader", Write: true, Admin: true}, "", 50)
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("runs = %d, %v (want 1)", len(page.Runs), err)
	}
}

// TestThreadsGCSweeperAppliesRetention pins the retention task through the
// sweeper: a terminal run past a (deliberately tiny) window is deleted, and the
// second pass is a no-op.
func TestThreadsGCSweeperAppliesRetention(t *testing.T) {
	svc, _ := sweepEnv(t)
	svc.Retention = time.Millisecond
	// Create a push run and finish it deterministically through the service.
	commit := "1111111111111111111111111111111111111111"
	svc.Git = &sweepGit{inner: svc.Git, path: ".spindle/workflows/ci.yml",
		body: "name: CI\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo\n"}
	svc.Git.(*sweepGit).commit = commit
	runs, err := svc.CreateRun(context.Background(), "o/r", commit, threads.EventPush, threads.CreateOpts{Branch: "main"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun = %v, %v", runs, err)
	}
	view, err := svc.GetRun(context.Background(), "o/r", runs[0].ID, auth.Principal{Name: "a", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range view.Jobs {
		if _, uerr := svc.UpdateJob(context.Background(), "o/r", runs[0].ID, job.ID, threads.JobUpdate{State: threads.JobSuccess}); uerr != nil {
			t.Fatal(uerr)
		}
	}
	time.Sleep(5 * time.Millisecond) // older than the 1ms window

	var gc *threadsSweeper
	for _, s := range newThreadsSweepers(svc) {
		if tw := s.(*threadsSweeper); tw.cadence == threadsRetentionCadence {
			gc = tw
		}
	}
	if gc == nil {
		t.Fatal("no retention sweeper")
	}
	if err := gc.Sweep(context.Background(), "o/r", nil); err != nil {
		t.Fatalf("gc sweep: %v", err)
	}
	if _, err := svc.GetRun(context.Background(), "o/r", runs[0].ID, auth.Principal{Name: "a", Admin: true}); err == nil {
		t.Fatal("the aged run survived the retention task")
	}
}

// sweepGit is the push-workflow reader for the retention test (one file, one
// commit, no schedule).
type sweepGit struct {
	inner  threads.GitReader
	path   string
	body   string
	commit string
}

func (s *sweepGit) ListWorkflowFiles(context.Context, string, string) ([]string, error) {
	return []string{s.path}, nil
}
func (s *sweepGit) ReadFile(context.Context, string, string, string) ([]byte, error) {
	return []byte(s.body), nil
}
func (s *sweepGit) ResolveRef(context.Context, string, string) (string, error) { return s.commit, nil }

// --- round 3: per-repository retention and the cron first tick -------------

const retentionWorkflow = "name: CI\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo\n"

// retentionEnvAt is a service over a one-file push workflow readable for every
// repository, with a pinned clock and a two-hour retention window.
func retentionEnvAt(t *testing.T) (*threads.Service, *time.Time) {
	t.Helper()
	svc, _ := sweepEnv(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	svc.Retention = 2 * time.Hour
	svc.Git = &sweepGit{path: ".spindle/workflows/ci.yml", body: retentionWorkflow,
		commit: "1111111111111111111111111111111111111111"}
	return svc, &now
}

// retentionSweeper returns the per-repo retention task from a fresh sweeper set:
// a fresh set is a restart (the gates are in memory).
func retentionSweeper(t *testing.T, svc *threads.Service) *threadsSweeper {
	t.Helper()
	for _, s := range newThreadsSweepers(svc) {
		if tw := s.(*threadsSweeper); tw.cadence == threadsRetentionCadence {
			return tw
		}
	}
	t.Fatal("no retention sweeper")
	return nil
}

// finishedRun creates and finishes one push run in repo (the caller owns the
// fake clock, so aging is just advancing it).
func finishedRun(t *testing.T, svc *threads.Service, repo string) string {
	t.Helper()
	runs, err := svc.CreateRun(context.Background(), repo, "1111111111111111111111111111111111111111",
		threads.EventPush, threads.CreateOpts{Branch: "main"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun(%s) = %v, %v", repo, runs, err)
	}
	view, err := svc.GetRun(context.Background(), repo, runs[0].ID, auth.Principal{Name: "a", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range view.Jobs {
		if _, uerr := svc.UpdateJob(context.Background(), repo, runs[0].ID, job.ID,
			threads.JobUpdate{State: threads.JobSuccess}); uerr != nil {
			t.Fatal(uerr)
		}
	}
	return runs[0].ID
}

// gone reports whether one run object is absent.
func gone(t *testing.T, svc *threads.Service, repo, runID string) bool {
	t.Helper()
	_, err := svc.GetRun(context.Background(), repo, runID, auth.Principal{Name: "a", Admin: true})
	return err != nil
}

// TestRetentionSweepsEveryRepository is the round-3 acceptance gate: two
// repositories with expired terminal runs and a third with young runs, one
// pass over the repositories (the maintainer's own loop shape) sweeps both
// expired repositories within one interval and leaves the young one untouched;
// the second pass is a no-op.
func TestRetentionSweepsEveryRepository(t *testing.T) {
	svc, now := retentionEnvAt(t)
	oldA := finishedRun(t, svc, "o/alpha")
	oldB := finishedRun(t, svc, "o/beta")
	*now = now.Add(3 * time.Hour) // alpha/beta runs are past the 2h window
	young := finishedRun(t, svc, "o/young")

	ret := retentionSweeper(t, svc)
	for _, repo := range []string{"o/alpha", "o/beta", "o/young"} {
		if !ret.Due(repo) {
			t.Fatalf("%s was not due on a fresh interval", repo)
		}
		if err := ret.Sweep(context.Background(), repo, nil); err != nil {
			t.Fatalf("retention %s: %v", repo, err)
		}
	}
	for repo, run := range map[string]string{"o/alpha": oldA, "o/beta": oldB} {
		if !gone(t, svc, repo, run) {
			t.Fatalf("%s: expired run %s survived the interval (only the first repo was swept)", repo, run)
		}
	}
	if gone(t, svc, "o/young", young) {
		t.Fatal("a young terminal run was deleted")
	}
	// A second pass (a fresh gate set, like the next interval) is a no-op.
	second := retentionSweeper(t, svc)
	res, err := svc.Retain(context.Background(), "o", "alpha", nil)
	if err != nil || res.RunsDeleted != 0 || res.JobsDeleted != 0 {
		t.Fatalf("second alpha pass = %+v, %v", res, err)
	}
	res, err = svc.Retain(context.Background(), "o", "young", nil)
	if err != nil || res.RunsDeleted != 0 {
		t.Fatalf("second young pass = %+v, %v", res, err)
	}
	if !second.Due("o/alpha") {
		t.Fatal("a restarted gate must repeat a pass")
	}
}

// TestRetentionRestartsDoNotStarveEitherRepository pins the restart rule: the
// gates are in memory, so a restart repeats a pass for every repository. Six
// kill/restart cycles each sweep BOTH repositories (no repository is starved by
// another's slot).
func TestRetentionRestartsDoNotStarveEitherRepository(t *testing.T) {
	svc, now := retentionEnvAt(t)
	for cycle := 0; cycle < 6; cycle++ {
		runs := map[string]string{
			"o/alpha": finishedRun(t, svc, "o/alpha"),
			"o/beta":  finishedRun(t, svc, "o/beta"),
		}
		*now = now.Add(3 * time.Hour)   // every run of this cycle is expired
		ret := retentionSweeper(t, svc) // a fresh gate set == a restart
		for repo, run := range runs {
			if !ret.Due(repo) {
				t.Fatalf("cycle %d: %s was not due after a restart", cycle, repo)
			}
			if err := ret.Sweep(context.Background(), repo, nil); err != nil {
				t.Fatalf("cycle %d: %s: %v", cycle, repo, err)
			}
			if !gone(t, svc, repo, run) {
				t.Fatalf("cycle %d: %s expired run %s was starved across a restart", cycle, repo, run)
			}
		}
	}
}

// TestRetentionTwoInstancesDoNotDoubleDelete proves the cross-instance rule:
// two Services share one store, both run the same pass concurrently, and every
// delete is an already-gone-tolerant exact-key delete — so the loser frees
// nothing and neither pass fails.
func TestRetentionTwoInstancesDoNotDoubleDelete(t *testing.T) {
	svc, now := retentionEnvAt(t)
	other := threads.New(svc.Store, nil)
	other.Git = svc.Git
	other.Now = svc.Now
	other.Retention = svc.Retention
	oldA := finishedRun(t, svc, "o/alpha")
	oldB := finishedRun(t, svc, "o/beta")
	*now = now.Add(3 * time.Hour)

	one, two := retentionSweeper(t, svc), retentionSweeper(t, other)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, pair := range []struct {
		sw   *threadsSweeper
		repo string
	}{{one, "o/alpha"}, {one, "o/beta"}, {two, "o/alpha"}, {two, "o/beta"}} {
		wg.Add(1)
		go func(sw *threadsSweeper, repo string) {
			defer wg.Done()
			if err := sw.Sweep(context.Background(), repo, nil); err != nil {
				errs <- err
			}
		}(pair.sw, pair.repo)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent retention pass failed: %v", err)
	}
	for repo, run := range map[string]string{"o/alpha": oldA, "o/beta": oldB} {
		if !gone(t, svc, repo, run) {
			t.Fatalf("%s: %s survived two instances", repo, run)
		}
	}
	// The loser's next pass frees nothing.
	res, err := svc.Retain(context.Background(), "o", "alpha", nil)
	if err != nil || res.RunsDeleted != 0 {
		t.Fatalf("post-race pass = %+v, %v", res, err)
	}
}

// TestScheduleSweeperFirstTickIsTheNextMinute pins the round-3 cron rule: the
// schedule gate follows the wall-clock minute, not a full cadence measured from
// the previous pass. A fresh start at 08:37:57 fires tick 08:37 (the current
// boundary), and the very next pass in the 08:38 minute fires tick 08:38 —
// the old gate was "a hair early" at 08:38:57 and lost the tick entirely.
func TestScheduleSweeperFirstTickIsTheNextMinute(t *testing.T) {
	svc, _ := sweepEnv(t)
	now := time.Date(2026, 11, 21, 8, 37, 57, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	var schedule *threadsSweeper
	for _, s := range newThreadsSweepers(svc) {
		if tw := s.(*threadsSweeper); tw.Kind() == threads.KindThreadsSchedule {
			schedule = tw
		}
	}
	if schedule == nil {
		t.Fatal("no schedule sweeper")
	}
	schedule.now = svc.Now // one clock for the gate and the cron math
	ctx := context.Background()
	if !schedule.Due("o/r") {
		t.Fatal("the first sweep after a fresh start was not due")
	}
	if err := schedule.Sweep(ctx, "o/r", nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// The next pass lands at the start of the next minute: it MUST be due (the
	// cadence-from-last-pass gate said no, and tick 08:38 was lost).
	now = time.Date(2026, 11, 21, 8, 38, 0, 200e6, time.UTC)
	if !schedule.Due("o/r") {
		t.Fatal("a pass in the next minute was not due (a tick would be skipped)")
	}
	if err := schedule.Sweep(ctx, "o/r", nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	wm, err := svc.LoadScheduleWatermark(ctx, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	fired := wm.Fired[".spindle/workflows/nightly.yml|* * * * *|"]
	want := "2026-11-21T08:38"
	if fired != want {
		t.Fatalf("watermark = %q, want %q (tick 08:38 not one minute later)", fired, want)
	}
	// Inside the same minute the gate stays closed.
	now = now.Add(30 * time.Second)
	if schedule.Due("o/r") {
		t.Fatal("a second pass inside the same minute was due")
	}
	runs, err := svc.ListRuns(ctx, "o/r", auth.Principal{Name: "reader", Write: true, Admin: true}, "", 50)
	if err != nil || len(runs.Runs) != 2 {
		t.Fatalf("runs = %d, %v (want the 08:37 and 08:38 ticks)", len(runs.Runs), err)
	}
}
