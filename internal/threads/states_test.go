package threads

import (
	"strings"
	"testing"
)

// Check-state fidelity (15 §2.1, round 2): a queued job is pending, a claimed
// one is in_progress, a cancelled one is cancelled (distinct from failure), and
// a run folds to cancelled when nothing failed.

func TestCheckStateVocabulary(t *testing.T) {
	for job, want := range map[string]string{
		JobQueued:    "pending",
		JobRunning:   "in_progress",
		JobSuccess:   "success",
		JobFailure:   "failure",
		JobCancelled: "cancelled",
	} {
		if got := checkState(job); got != want {
			t.Errorf("checkState(%s) = %q, want %q", job, got, want)
		}
	}
}

func TestCancelledRunIsNotAFailure(t *testing.T) {
	e := newTestEnv(t)
	runs := e.mustCreate(t)
	run := runs[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	for _, j := range jobs {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, j.ID, JobUpdate{State: JobCancelled}); err != nil {
			t.Fatalf("cancel %s: %v", j.ID, err)
		}
	}
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.writer())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if view.Run.State != RunCancelled {
		t.Fatalf("run state = %q, want cancelled", view.Run.State)
	}
	last := e.checks.reports[len(e.checks.reports)-1]
	if last.State != "cancelled" {
		t.Fatalf("cancelled job mirrored %q, want cancelled", last.State)
	}
}

func TestRunAndJobDurationsAreDerived(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	started := "2026-11-20T12:00:00Z"
	completed := "2026-11-20T12:01:12Z"
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobs[0].ID, JobUpdate{
		State: JobSuccess, StartedAt: &started, CompletedAt: &completed,
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.writer())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if view.Run.StartedAt == nil || view.Run.DurationSeconds <= 0 {
		t.Fatalf("run duration = %d (started %v)", view.Run.DurationSeconds, view.Run.StartedAt)
	}
	var done bool
	for _, j := range view.Jobs {
		if j.ID != jobs[0].ID {
			continue
		}
		done = true
		if j.DurationSeconds != 72 {
			t.Fatalf("job duration = %d, want 72", j.DurationSeconds)
		}
	}
	if !done {
		t.Fatal("job missing from the view")
	}
	// A live job measures to now (the fake clock advances 1s per read).
	liveStart := "2026-11-20T12:00:00Z"
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobs[1].ID, JobUpdate{
		State: JobRunning, StartedAt: &liveStart,
	}); err != nil {
		t.Fatalf("start second job: %v", err)
	}
	view, _ = e.svc.GetRun(ctx(), "o/r", run.ID, e.writer())
	for _, j := range view.Jobs {
		if j.ID == jobs[1].ID && j.DurationSeconds < 0 {
			t.Fatalf("live job duration = %d", j.DurationSeconds)
		}
	}
	// A stored record never persists the derived field.
	raw, _, err := e.svc.getJSON(ctx(), JobKey("o", "r", run.ID, jobs[0].ID))
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if strings.Contains(string(raw), `"duration_seconds"`) {
		t.Fatalf("duration_seconds was persisted: %s", raw)
	}
}
