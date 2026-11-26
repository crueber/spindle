package threads

// environment_protection_edges_test.go — the tolerant and refusal branches of
// the wave-2 environment/concurrency/retention code: a corrupt group record, a
// stale holder or pending slot, a delayed creation, a dry-run runner sweep, the
// approval projection's team expansion, and the expiry fallbacks.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestConcurrencyGroupRefusalsAndStaleSlots pins the group record's edge paths.
func TestConcurrencyGroupRefusalsAndStaleSlots(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA

	// A corrupt record never fails the run's creation (the header is the commit
	// point); the claim path refuses instead, which the next assertions pin.
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "deploy-main"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	extra, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(extra) != 1 {
		t.Fatalf("creation with a corrupt group record = %d (%v)", len(extra), err)
	}
	// A corrupt record also refuses the claim-path pending registration and
	// acquisition.
	if _, err := e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", "0000000000000000001-aaaaaa"); err == nil {
		t.Fatal("notePendingGroup ignored a corrupt record")
	}
	if _, _, err := e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", "0000000000000000001-aaaaaa"); err == nil {
		t.Fatal("tryAcquireGroup ignored a corrupt record")
	}
	// Clear the fault and the probe run before the rest of the scenario.
	if derr := e.store.Delete(ctx(), ConcurrencyKey("o", "r", "deploy-main"), ""); derr != nil {
		t.Fatal(derr)
	}
	if _, cerr := e.svc.cancelRunSystem(ctx(), "o", "r", extra[0].ID); cerr != nil {
		t.Fatalf("cancel the probe run: %v", cerr)
	}

	// A holder nobody has (stale) is not live: preparation registers nothing.
	doc := &ConcurrencyDoc{Group: "deploy-main", RunID: "0000000000000000009-ffffff", Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "deploy-main"), mustJSON(doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	pol := groupPolicy{Group: "deploy-main"}
	if got, err := e.svc.prepareGroup(ctx(), "o", "r", pol, "0000000000000000002-bbbbbb"); err != nil || got != "" {
		t.Fatalf("stale holder = %q (%v), want free", got, err)
	}

	// A live holder with a NEWER live pending run leaves that slot alone (a
	// delayed creation is superseded as pending).
	first := e.mustCreate(t)[0]
	holdGroup(t, e, first)
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatal(err)
	}
	// A run created "before" the pending one (a smaller id) sees the newer slot.
	if got, err := e.svc.prepareGroup(ctx(), "o", "r", pol, "0000000000000000001-aaaaaa"); err != nil || got != "deploy-main" {
		t.Fatalf("delayed creation = %q (%v), want it to see the group but not displace", got, err)
	}
	if doc := loadGroupDoc(t, e, "deploy-main"); doc.Pending != second[0].ID {
		t.Fatalf("a delayed creation displaced the newer pending run: %+v", doc)
	}

	// notePendingGroup's no-record and idempotent branches.
	if pending, err := e.svc.notePendingGroup(ctx(), "o", "r", "absent-group", "0000000000000000002-bbbbbb"); err != nil || pending {
		t.Fatalf("no record = %v (%v)", pending, err)
	}
	pending, err := e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", first.ID)
	if err != nil || pending {
		t.Fatalf("the holder waited on its own group = %v (%v)", pending, err)
	}
	pending, err = e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", second[0].ID)
	if err != nil || !pending {
		t.Fatalf("the pending run was not recognised = %v (%v)", pending, err)
	}

	// Releasing the PENDING run (a cancel) clears both fields it could own.
	e.svc.releaseConcurrency(ctx(), "o", "r", second[0].ID, []string{"deploy-main"})
	if doc := loadGroupDoc(t, e, "deploy-main"); doc.Pending != "" || doc.RunID != first.ID {
		t.Fatalf("release of the pending run = %+v", doc)
	}
	e.svc.releaseConcurrency(ctx(), "o", "r", first.ID, []string{"deploy-main", ""})
	if doc := loadGroupDoc(t, e, "deploy-main"); doc.RunID != "" {
		t.Fatalf("release of the holder = %+v", doc)
	}

	// admitConcurrency's trivial and cancel-in-progress branches.
	if ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", "x", nil); aerr != nil || !ok {
		t.Fatalf("nil spec = %v (%v)", ok, aerr)
	}
	if ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", "x", &Spec{Concurrency: &ConcurrencySpec{Group: "  "}}); aerr != nil || !ok {
		t.Fatalf("blank group = %v (%v)", ok, aerr)
	}
}

// TestApprovalForTeamAndTimerProjection pins the page projection's team
// expansion, the max wait deadline and the tolerant reads.
func TestApprovalForTeamAndTimerProjection(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	f.roles.roles["linus"] = "write"
	f.svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{"acme/releasers": {"ada", "grace"}}}
	f.putEnv(t, "production", EnvironmentInput{
		TeamReviewers: []string{"acme/releasers"}, RequiredReviewers: []string{"linus"},
		WaitTimerSeconds: 60,
	})
	run := f.deployRun(t, CreateOpts{})
	av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "ada"})
	if strings.Join(av.Reviewers, ",") != "ada,grace,linus" {
		t.Fatalf("reviewers = %v (team members must expand)", av.Reviewers)
	}
	if av.WaitUntil == nil || strings.Join(av.WaitingFor, ",") != "ada,grace,linus" {
		t.Fatalf("projection = %+v", av)
	}
	if !av.CanDecide {
		t.Fatalf("a team member may not decide: %+v", av)
	}
	// A team that stops resolving contributes nobody but does not fail the page.
	f.svc.TeamRoster = fakeTeamRoster{err: errors.New("directory down")}
	if av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "linus"}); len(av.Reviewers) != 1 || av.Reviewers[0] != "linus" {
		t.Fatalf("unreadable team projection = %+v", av)
	}
	// A roster read error at creation fails the deployment closed.
	run2 := f.deployRun(t, CreateOpts{})
	job := envJob(t, f.testEnv, run2.ID)
	if job.State != JobFailure || job.Deployment.State != DeploymentFailed {
		t.Fatalf("roster-down creation = %s / %+v", job.State, job.Deployment)
	}

}

// TestGCResultModes pins the summary line for the dry run and keep-forever
// modes (the task notice an operator reads).
func TestGCResultModes(t *testing.T) {
	base := GCResult{RunsDeleted: 1, RunBytes: 10, BytesFreed: 10, RetentionDays: 30, ArtifactDays: 90}
	got := base.String()
	if strings.Contains(got, "dry run") || strings.Contains(got, "keep forever") {
		t.Fatalf("apply summary = %q", got)
	}
	if !strings.Contains(got, "windows=30d/90d") {
		t.Fatalf("the summary does not report the effective windows: %q", got)
	}
	dry := base
	dry.DryRun = true
	if !strings.Contains(dry.String(), "dry run") {
		t.Fatalf("dry summary = %q", dry.String())
	}
	forever := GCResult{KeepForever: true}
	if !strings.Contains(forever.String(), "keep forever") {
		t.Fatalf("keep-forever summary = %q", forever.String())
	}
}

// TestRetentionDryRunRunnerSweep pins the dry-run runner family: a revoked
// runner past the window is counted and NOT deleted; apply then deletes it.
func TestRetentionDryRunRunnerSweep(t *testing.T) {
	e, now := retentionEnv(t)
	old := now.Add(-40 * 24 * time.Hour).Format(dateTimeFmt)
	doc := &RunnerDoc{ID: "rnrbeef02", State: RunnerRevoked, RegisteredBy: "alice",
		CreatedAt: old, RevokedAt: &old, Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(doc.ID), mustJSON(doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	e.svc.runnerSweep = &runnerSweepGate{}
	res := &GCResult{}
	e.svc.deleteDeadRunners(ctx(), now.Add(-e.svc.retention()), false, res)
	if res.RunnersDeleted != 1 || res.RunnerBytes <= 0 {
		t.Fatalf("dry runner sweep = %+v", res)
	}
	if raw, _, err := store.GetBytes(ctx(), e.store, RunnerKey(doc.ID), store.GetOptions{}); err != nil || raw == nil {
		t.Fatalf("the dry run deleted the runner: %v", err)
	}
	e.svc.runnerSweep = &runnerSweepGate{}
	res = &GCResult{}
	e.svc.deleteDeadRunners(ctx(), now.Add(-e.svc.retention()), true, res)
	if res.RunnersDeleted != 1 || res.CredentialsDeleted > 1 {
		t.Fatalf("apply runner sweep = %+v", res)
	}
	if _, _, err := store.GetBytes(ctx(), e.store, RunnerKey(doc.ID), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("the apply pass did not delete the runner: %v", err)
	}
}

// TestArtifactExpiryFallbacks pins the lifetime helper's documented fallbacks:
// an unparseable createdAt falls back to now, and an unset artifact window
// follows the run window (then the default).
func TestArtifactExpiryFallbacks(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	bad := &Artifact{CreatedAt: "not a time"}
	if got := artifactExpiresAt(&Run{}, bad, time.Hour, 0, now); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("fallback expiry = %s", got)
	}
	if got := artifactExpiresAt(&Run{}, bad, 0, 0, now); !got.Equal(now.Add(DefaultRetention)) {
		t.Fatalf("default expiry = %s", got)
	}
	days := &Artifact{CreatedAt: now.Format(dateTimeFmt), RetentionDays: 2}
	if got := artifactExpiresAt(&Run{}, days, time.Hour, 90*24*time.Hour, now); !got.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("retention-days expiry = %s", got)
	}
}

// TestJobRunsOnFallbacks pins the runs-on projection the label mapping reads.
func TestJobRunsOnFallbacks(t *testing.T) {
	if jobRunsOn(nil) != nil {
		t.Fatal("nil job has runs-on")
	}
	if got := jobRunsOn(&Job{RunsOn: "self-hosted, linux"}); strings.Join(got, ",") != "self-hosted, linux" {
		t.Fatalf("string fallback = %v", got)
	}
	if got := jobRunsOn(&Job{Spec: &Spec{RunsOn: []string{"linux"}}, RunsOn: "ignored"}); len(got) != 1 || got[0] != "linux" {
		t.Fatalf("spec wins = %v", got)
	}
	if jobRunsOn(&Job{}) != nil {
		t.Fatal("empty job has runs-on")
	}
}

// TestGuardDeploymentSecretsEdges pins the payload guard's remaining arms: an
// unapproved reviewed deployment, a vanished environment and a roster error.
func TestGuardDeploymentSecretsEdges(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"maint"}})
	run := f.deployRun(t, CreateOpts{})
	job := envJob(t, f.testEnv, run.ID)
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", job); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unapproved guard = %v, want forbidden", err)
	}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", &Job{}); err != nil {
		t.Fatalf("a job with no environment = %v", err)
	}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", &Job{Spec: &Spec{Environment: &EnvironmentSpec{Name: "ghost"}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("vanished environment = %v, want not found", err)
	}
	f.svc.TeamRoster = fakeTeamRoster{err: errors.New("down")}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", &Job{Spec: &Spec{Environment: &EnvironmentSpec{Name: "production"}}}); err == nil {
		t.Fatal("a roster error released the guard")
	}
}

// TestEnvironmentSelfReviewOtherArms pins the self-review check's neutral arms.
func TestEnvironmentSelfReviewOtherArms(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"maint"}})
	run := f.deployRun(t, CreateOpts{Actor: "writer"})
	if err := f.svc.checkEnvironmentSelfReview(ctx(), "o", "r", auth.Principal{Name: "writer"}, run, nil); err != nil {
		t.Fatalf("prevent-self-review off refused: %v", err)
	}
	if err := f.svc.checkEnvironmentSelfReview(ctx(), "o", "r", auth.Principal{Name: ""}, run, nil); err != nil {
		t.Fatalf("an anonymous principal tripped the rule: %v", err)
	}
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"maint"}, PreventSelfReview: true})
	if err := f.svc.checkEnvironmentSelfReview(ctx(), "o", "r", auth.Principal{Name: "maint"}, run, nil); err != nil {
		t.Fatalf("a different principal was refused: %v", err)
	}
}
