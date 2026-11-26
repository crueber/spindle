package threads

// retention_parity_test.go — retention and GC parity (PAR-158/159): the two
// windows (run/log and artifact), keep-forever, per-family reclaimed bytes, the
// dry-run/apply agreement, and the safety rule (never delete a live or attached
// run). The restart rule is structural: the pass is idempotent and the cadence
// gate only REPEATS work, never skips it (cmd/spindle/threads_sweep_test.go).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// parityRetentionEnv is a fixture with a settable clock, a 30-day run window and
// a 90-day artifact window.
func parityRetentionEnv(t *testing.T) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return now }
	e.svc.Retention = 30 * 24 * time.Hour
	// ArtifactRetention stays 0: the artifact follows the run window unless a
	// test gives it its own (the outliving case below).
	return e, &now
}

// TestRetentionDryRunMatchesApply pins PAR-159's core promise: a dry run reports
// exactly the numbers the apply pass then produces, and deletes nothing.
func TestRetentionDryRunMatchesApply(t *testing.T) {
	e, now := parityRetentionEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	finishRun(t, e, run)
	if _, err := store.PutBytes(ctx(), e.store, LogKey("o", "r", run.ID, "j-build", 1, 1),
		[]byte("hello log frame\n"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * 24 * time.Hour)

	plan, err := e.svc.PlanRetention(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.DryRun || plan.RunsDeleted != 1 || plan.ArtifactsDeleted != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.RunBytes <= 0 || plan.JobBytes <= 0 || plan.LogBytes <= 0 || plan.ArtifactBytes <= 0 || plan.QueueBytes <= 0 {
		t.Fatalf("plan bytes per family = %+v", plan)
	}
	if plan.BytesFreed != plan.RunBytes+plan.JobBytes+plan.LogBytes+plan.ArtifactBytes+plan.QueueBytes+plan.RunnerBytes {
		t.Fatalf("family bytes do not sum to the total: %+v", plan)
	}
	// Nothing was deleted by the plan.
	if r, lerr := e.svc.loadRun(ctx(), "o", "r", run.ID); lerr != nil || r == nil {
		t.Fatalf("the dry run deleted the run: %v", lerr)
	}
	if n := countPrefix(t, e, ArtifactPrefix("o", "r", run.ID)); n == 0 {
		t.Fatal("the dry run deleted the artifact")
	}

	apply, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if apply.DryRun || apply.RunsDeleted != plan.RunsDeleted || apply.JobsDeleted != plan.JobsDeleted ||
		apply.LogsDeleted != plan.LogsDeleted || apply.ArtifactsDeleted != plan.ArtifactsDeleted ||
		apply.QueueDeleted != plan.QueueDeleted || apply.BytesFreed != plan.BytesFreed ||
		apply.RunBytes != plan.RunBytes || apply.JobBytes != plan.JobBytes ||
		apply.LogBytes != plan.LogBytes || apply.ArtifactBytes != plan.ArtifactBytes ||
		apply.QueueBytes != plan.QueueBytes || apply.RunnerBytes != plan.RunnerBytes {
		t.Fatalf("apply = %+v\nplan  = %+v", apply, plan)
	}
	if r, _ := e.svc.loadRun(ctx(), "o", "r", run.ID); r != nil {
		t.Fatal("the apply pass did not delete the aged run")
	}
	// A second pass is a no-op (idempotence: a restart repeats, never over-deletes).
	again, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil || again.RunsDeleted != 0 || again.ArtifactsDeleted != 0 {
		t.Fatalf("second pass = %+v (%v)", again, err)
	}
}

// TestRetentionNeverDeletesAnAttachedRun pins the safety rule: a terminal run
// with a job whose lease is still in the future is pinned, even past the window.
func TestRetentionNeverDeletesAnAttachedRun(t *testing.T) {
	e, now := parityRetentionEnv(t)
	run := e.mustCreate(t)[0]
	finishRun(t, e, run)
	jobs := mustJobs(t, e, run.ID)
	lease := now.Add(60 * 24 * time.Hour).Format(dateTimeFmt)
	if _, err := e.svc.casUpdate(ctx(), JobKey("o", "r", run.ID, jobs[0].ID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var job Job
		if jerr := json.Unmarshal(cur, &job); jerr != nil {
			return nil, false, jerr
		}
		job.LeaseUntil = &lease
		job.Version++
		return mustJSON(&job), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * 24 * time.Hour)
	res, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.RunsDeleted != 0 {
		t.Fatalf("an attached run was reclaimed: %+v", res)
	}
	if r, _ := e.svc.loadRun(ctx(), "o", "r", run.ID); r == nil {
		t.Fatal("an attached run was deleted")
	}
	// Once the lease lapses the run ages out normally.
	*now = now.Add(61 * 24 * time.Hour)
	res, err = e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil || res.RunsDeleted != 1 {
		t.Fatalf("retention after the lease lapsed = %+v (%v)", res, err)
	}
}

// TestArtifactOutlivesItsRunWhenItsWindowAllows pins the two windows: a 90-day
// artifact window survives its 30-day run, and the expiry row reclaims it at
// its own time with the bytes reported under the artifact family.
func TestArtifactOutlivesItsRunWhenItsWindowAllows(t *testing.T) {
	e, now := parityRetentionEnv(t)
	e.svc.ArtifactRetention = 90 * 24 * time.Hour
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	finishRun(t, e, run)
	*now = now.Add(31 * 24 * time.Hour)

	res, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if res.RunsDeleted != 1 || res.ArtifactsDeleted != 0 {
		t.Fatalf("retain = %+v, want the run gone and the artifact kept", res)
	}
	if r, _ := e.svc.loadRun(ctx(), "o", "r", run.ID); r != nil {
		t.Fatal("the run survived its window")
	}
	if n := countPrefix(t, e, ArtifactPrefix("o", "r", run.ID)); n == 0 {
		t.Fatal("the artifact inside its own window was reclaimed with the run")
	}
	// At 91 days the artifact family reclaims it.
	*now = now.Add(60 * 24 * time.Hour)
	res, err = e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("retain at 91d: %v", err)
	}
	if res.ArtifactsDeleted != 1 || res.ArtifactBytes <= 0 {
		t.Fatalf("outliving artifact = %+v, want one artifact and its bytes", res)
	}
	if n := countPrefix(t, e, ArtifactPrefix("o", "r", run.ID)); n != 0 {
		t.Fatalf("artifact bytes survived their own window: %d objects", n)
	}
}

// TestKeepForeverReclaimsNothing pins the unbounded spelling: an ancient run and
// its artifact are kept, and the report says why.
func TestKeepForeverReclaimsNothing(t *testing.T) {
	e, now := parityRetentionEnv(t)
	e.svc.KeepForever = true
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	finishRun(t, e, run)
	*now = now.Add(400 * 24 * time.Hour)
	res, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if !res.KeepForever || res.RunsDeleted != 0 || res.ArtifactsDeleted != 0 || res.BytesFreed != 0 {
		t.Fatalf("keep_forever reclaimed: %+v", res)
	}
	if r, _ := e.svc.loadRun(ctx(), "o", "r", run.ID); r == nil {
		t.Fatal("keep_forever deleted the run")
	}
}
