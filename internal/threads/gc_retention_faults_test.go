package threads

// gc_retention_faults_test.go — the failure arms of the wave-2 retention walk
// (deleteRunWith, expireArtifactsMode, the keep-forever upload) and the
// approval decision when the environment vanished underneath it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestExpireArtifactsFailureArms pins the expiry walk's tolerant and failing
// branches: a malformed index row, a lobe-delete failure, a listing failure and
// a cancelled context.
func TestExpireArtifactsFailureArms(t *testing.T) {
	t.Run("malformed rows are tolerated", func(t *testing.T) {
		e, now := parityRetentionEnv(t)
		for _, key := range []string{
			ArtifactExpiryPrefix("o", "r") + "0000000000000000001-bad/../x.json", // invalid name
			ArtifactExpiryPrefix("o", "r") + "0000000000000000002-ok.json",       // unparseable body
		} {
			if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.svc.expireArtifactsMode(ctx(), "o", "r", *now, true, nil, &GCResult{}); err != nil {
			t.Fatalf("malformed rows failed the sweep: %v", err)
		}
	})
	t.Run("a failing blob delete surfaces", func(t *testing.T) {
		e, now := parityRetentionEnv(t)
		run := e.mustCreate(t)[0]
		claims := e.artifactClaims(t, run)
		e.finalizeBuild(t, claims, 1)
		*now = now.Add(2 * 24 * time.Hour)
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("delete", 0)
		if err := e.svc.expireArtifactsMode(ctx(), "o", "r", *now, true, nil, &GCResult{}); err == nil {
			t.Fatal("a failing artifact delete did not fail the expiry pass")
		}
	})
	t.Run("cancelled context and list failure", func(t *testing.T) {
		e, now := parityRetentionEnv(t)
		cctx, cancel := context.WithCancel(ctx())
		cancel()
		if err := e.svc.expireArtifactsMode(cctx, "o", "r", *now, true, nil, &GCResult{}); err != nil {
			t.Fatalf("a cancelled context over an empty family = %v", err)
		}
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("list", 0)
		if err := e.svc.expireArtifactsMode(ctx(), "o", "r", *now, true, nil, &GCResult{}); err == nil {
			t.Fatal("a failing expiry LIST did not fail the pass")
		}
	})
}

// TestDeleteRunWithFailureArms pins the run walk's header/expiry failures.
func TestDeleteRunWithFailureArms(t *testing.T) {
	t.Run("header read failure", func(t *testing.T) {
		e, _ := parityRetentionEnv(t)
		run := e.mustCreate(t)[0]
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("get", 0) // the run header's size probe
		if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
			t.Fatal("a failing header read did not fail the run delete")
		}
	})
	t.Run("expiry index listing failure", func(t *testing.T) {
		e, _ := parityRetentionEnv(t)
		run := e.mustCreate(t)[0]
		fs := &faultStore{Memory: e.store}
		e.svc.Store = fs
		fs.fail("list", 1) // run objects, then the artifact-expiry family
		if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
			t.Fatal("a failing expiry listing did not fail the run delete")
		}
	})
}

// TestRetainCancelledContextStops pins the retention loop's context arm: an aged
// run exists, the context is cancelled, and the pass returns the context error
// without deleting (a restart repeats the pass).
func TestRetainCancelledContextStops(t *testing.T) {
	e, now := retentionEnv(t)
	run := e.mustCreate(t)[0]
	finishRun(t, e, run)
	*now = now.Add(31 * 24 * time.Hour)
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	if _, err := e.svc.Retain(cctx, "o", "r", nil); err == nil {
		t.Fatal("a cancelled retention pass returned success")
	}
	if got, _ := e.svc.loadRun(ctx(), "o", "r", run.ID); got == nil {
		t.Fatal("a cancelled pass deleted the run anyway")
	}
}

// TestReleaseConcurrencyCorruptRecordIsTolerated pins the release path over a
// corrupt holder record: it is left for a steal, never a failed fold.
func TestReleaseConcurrencyCorruptRecordIsTolerated(t *testing.T) {
	e := newTestEnv(t)
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "g"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseConcurrency(ctx(), "o", "r", "0000000000000000001-aaaaaa", []string{"g"})
	if raw, _, err := store.GetBytes(ctx(), e.store, ConcurrencyKey("o", "r", "g"), store.GetOptions{}); err != nil || raw == nil {
		t.Fatalf("the corrupt record was not left in place: %v", err)
	}
}

// TestDeleteRunToleratesAMissingHeaderAndCancelledContext pins the walk's two
// remaining tolerant/refusal arms: a run header already gone is not an error
// (the remainder is still swept), and a cancelled context stops the walk.
func TestDeleteRunToleratesAMissingHeaderAndCancelledContext(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		e, _ := parityRetentionEnv(t)
		run := e.mustCreate(t)[0]
		if derr := e.store.Delete(ctx(), RunKey("o", "r", run.ID), ""); derr != nil {
			t.Fatal(derr)
		}
		if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err != nil {
			t.Fatalf("a missing header failed the sweep: %v", err)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		e, _ := parityRetentionEnv(t)
		run := e.mustCreate(t)[0]
		cctx, cancel := context.WithCancel(ctx())
		cancel()
		if _, err := e.svc.deleteRunWith(cctx, "o", "r", run.ID, e.svc.nowUTC(), true); err == nil {
			t.Fatal("a cancelled context walked the run")
		}
	})
}

// TestFinalizeArtifactKeepForever pins keep_forever at upload: no expiry row is
// written, so no later pass can reclaim the artifact.
func TestFinalizeArtifactKeepForever(t *testing.T) {
	e := newTestEnv(t)
	e.svc.KeepForever = true
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 1)
	if n := countPrefix(t, e, ArtifactExpiryPrefix("o", "r")); n != 0 {
		t.Fatalf("keep_forever wrote %d expiry rows", n)
	}
}

// TestApproveRunVanishedEnvironment pins the decision when the environment was
// deleted between creation and approval: the deployment fails closed, the run
// still releases its other jobs, and the audit names the decision.
func TestApproveRunVanishedEnvironment(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada"}})
	run := f.deployRun(t, CreateOpts{})
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatal(err)
	}
	// Only an admin may decide a run whose environment vanished.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("listed reviewer after deletion = %v, want forbidden", err)
	}
	released, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "root", Admin: true})
	if err != nil {
		t.Fatalf("admin approval: %v", err)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.State != JobFailure || job.Deployment.State != DeploymentFailed {
		t.Fatalf("vanished environment job = %s / %+v", job.State, job.Deployment)
	}
	if released.State == RunPendingApproval {
		t.Fatalf("the run stayed held after the decision: %+v", released)
	}
}

// TestRecordDeploymentApprovalRosterError pins the approval-time roster error:
// it surfaces and the deployment is not released.
func TestRecordDeploymentApprovalRosterError(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{"acme/releasers": {"ada"}}}
	f.putEnv(t, "production", EnvironmentInput{TeamReviewers: []string{"acme/releasers"}})
	run := f.deployRun(t, CreateOpts{})
	jobs := mustJobs(t, f.testEnv, run.ID)
	f.svc.TeamRoster = fakeTeamRoster{err: errors.New("directory down")}
	if _, err := f.svc.recordDeploymentApproval(ctx(), "o", "r", run.ID, jobs, auth.Principal{Name: "ada"}, "now"); err == nil {
		t.Fatal("a roster error at approval did not surface")
	}
}
