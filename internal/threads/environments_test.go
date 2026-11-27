package threads

// environments_test.go — deployment environments
// (docs/PLAN-THREADS-ENVIRONMENTS.md): the record CRUD and its role floor, the
// reviewer gate on a run, approve/deny, the fork rule, deletion while pending,
// secret precedence and the payload guard.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

const environmentWorkflowYAML = `name: Deploy
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  ship:
    runs-on: linux
    environment: production
    steps:
      - run: ./deploy.sh
`

// envFixture is a testEnv with the deploy workflow, a maintainer, a listed
// write-role reviewer and an audit recorder.
type envFixture struct {
	*testEnv
	audit []string
	svc   *Service
}

func newEnvFixture(t *testing.T) *envFixture {
	t.Helper()
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", environmentWorkflowYAML)
	e.roles.roles["maint"] = "maintain"
	f := &envFixture{testEnv: e, svc: e.svc}
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		f.audit = append(f.audit, actor+" "+action+" "+target)
		return nil
	}
	return f
}

func (f *envFixture) maintain() auth.Principal { return auth.Principal{Name: "maint"} }
func (f *envFixture) reviewer() auth.Principal { return auth.Principal{Name: "writer"} }
func (f *envFixture) outsider() auth.Principal { return auth.Principal{Name: "reader"} }

// putEnvironment stores a record with the given reviewers (maintain actor).
func (f *envFixture) putEnvironment(t *testing.T, name string, reviewers ...string) *EnvironmentView {
	t.Helper()
	env, err := f.svc.PutEnvironment(ctx(), "o/r", name, EnvironmentInput{RequiredReviewers: reviewers}, f.maintain())
	if err != nil {
		t.Fatalf("PutEnvironment(%s): %v", name, err)
	}
	return env
}

// queueEntries counts queue ENTRIES (the head object is bookkeeping, not work).
func (f *envFixture) queueEntries(t *testing.T) int {
	t.Helper()
	n := 0
	if err := f.store.List(ctx(), QueuePrefix("o", "r"), "", func(m store.ObjectMeta) error {
		if !strings.HasSuffix(m.Key, "head.json") {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("list queue: %v", err)
	}
	return n
}

// deployRun creates the deploy workflow's push run.
func (f *envFixture) deployRun(t *testing.T, opts CreateOpts) *Run {
	t.Helper()
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.Ref == "" {
		opts.Ref = "refs/heads/main"
	}
	if opts.Actor == "" {
		opts.Actor = "writer"
	}
	runs, err := f.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, opts)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	return runs[0]
}

// envJob returns the run's "ship" job.
func envJob(t *testing.T, e *testEnv, runID string) *Job {
	t.Helper()
	for _, job := range mustJobs(t, e, runID) {
		if job.Name == "ship" {
			return job
		}
	}
	t.Fatalf("run %s has no ship job", runID)
	return nil
}

func TestEnvironmentCRUDAndFloor(t *testing.T) {
	f := newEnvFixture(t)
	env := f.putEnvironment(t, "production", "writer")
	if env.Name != "production" || len(env.RequiredReviewers) != 1 || env.RequiredReviewers[0] != "writer" {
		t.Fatalf("created = %+v", env)
	}
	if env.CreatedBy != "maint" || env.CreatedAt == "" || env.Version != 1 {
		t.Fatalf("created metadata = %+v", env)
	}
	// A writer (below maintain) cannot create one.
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "prod2", EnvironmentInput{}, f.writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer create err = %v, want forbidden", err)
	}
	// A reader cannot even read the list (it names secret names).
	if _, err := f.svc.ListEnvironments(ctx(), "o/r", f.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader list err = %v, want forbidden", err)
	}
	list, err := f.svc.ListEnvironments(ctx(), "o/r", f.maintain())
	if err != nil || len(list) != 1 || list[0].Name != "production" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Update replaces the reviewer set; the model version advances.
	updated := f.putEnvironment(t, "production")
	if len(updated.RequiredReviewers) != 0 || updated.Version != 2 || updated.CreatedAt != env.CreatedAt {
		t.Fatalf("updated = %+v", updated)
	}
	// Deletion is maintain-only, and absent is not an error.
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer delete err = %v", err)
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := f.svc.GetEnvironment(ctx(), "o/r", "production", f.maintain()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted = %v, want not found", err)
	}
	// The audit family recorded the mutations.
	for _, want := range []string{"maint " + AuditEnvironmentCreate, "maint " + AuditEnvironmentUpdate, "maint " + AuditEnvironmentDelete} {
		found := false
		for _, got := range f.audit {
			if strings.HasPrefix(got, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit %v lacks %q", f.audit, want)
		}
	}
}

func TestEnvironmentValidation(t *testing.T) {
	f := newEnvFixture(t)
	// A name outside the pinned charset is rejected.
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "prod east", EnvironmentInput{}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name err = %v", err)
	}
	// A reviewer without write access is rejected AT SAVE TIME (never a held
	// deployment waiting for someone who cannot approve).
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: []string{"reader"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("read-role reviewer err = %v", err)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: []string{"ghost"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown reviewer err = %v", err)
	}
	// Reviewers are deduped, lowercased and sorted; write access is enough.
	f.roles.roles["ada"] = "write"
	env, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: []string{" Ada ", "ada", "maint"}}, f.maintain())
	if err != nil {
		t.Fatalf("valid reviewers: %v", err)
	}
	if got := strings.Join(env.RequiredReviewers, ","); got != "ada,maint" {
		t.Fatalf("reviewers = %q", got)
	}
	// Too many reviewers is a named error, not a silent truncation.
	many := make([]string, 0, maxEnvironmentReviewers+1)
	for i := 0; i <= maxEnvironmentReviewers; i++ {
		many = append(many, fmt.Sprintf("r%d", i))
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: many}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many reviewers err = %v", err)
	}
}

// TestEnvironmentHoldsUntilReviewerApproves is the core flow: pending → listed
// reviewer approves → the job starts with its environment secrets.
func TestEnvironmentHoldsUntilReviewerApproves(t *testing.T) {
	f := newEnvFixture(t)
	k, err := NewKeyring(strings.Repeat("k", 32), "")
	if err != nil {
		t.Fatal(err)
	}
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production", "writer")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "DEPLOY_TOKEN", "env-secret-value", f.maintain()); err != nil {
		t.Fatalf("env secret: %v", err)
	}
	run := f.deployRun(t, CreateOpts{})
	if run.State != RunPendingApproval || run.ApprovalKind != ApprovalEnvironment ||
		strings.Join(run.Environments, ",") != "production" {
		t.Fatalf("run = %+v", run)
	}
	// Nothing is enqueued: the hold IS the queue.
	if got := f.queueEntries(t); got != 0 {
		t.Fatalf("queue entries = %d, want 0", got)
	}
	// The job record carries the deployment, pending, with the declared URL.
	job := envJob(t, f.testEnv, run.ID)
	if job.Deployment == nil || job.Deployment.State != DeploymentPending || job.Deployment.Environment != "production" {
		t.Fatalf("job deployment = %+v", job.Deployment)
	}
	// A maintainer who is NOT listed cannot decide; a reader cannot either.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.maintain()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-reviewer maintainer err = %v, want forbidden", err)
	}
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader err = %v, want forbidden", err)
	}
	// The view tells the page who may decide.
	view, err := f.svc.GetRun(ctx(), "o/r", run.ID, f.reviewer())
	if err != nil {
		t.Fatal(err)
	}
	if view.Approval == nil || !view.Approval.CanDecide || view.Approval.Reviewers[0] != "writer" {
		t.Fatalf("reviewer approval view = %+v", view.Approval)
	}
	if outsider, _ := f.svc.GetRun(ctx(), "o/r", run.ID, f.outsider()); outsider.Approval.CanDecide {
		t.Fatal("a read principal may not decide")
	}
	approved, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reviewer())
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != RunQueued || approved.ApprovedBy != "writer" || approved.ApprovedAt == nil {
		t.Fatalf("approved = %+v", approved)
	}
	job = envJob(t, f.testEnv, run.ID)
	if job.Deployment.State != DeploymentApproved || job.Deployment.ApprovedBy != "writer" || job.Deployment.ApprovedAt == nil {
		t.Fatalf("deployment = %+v", job.Deployment)
	}
	// The job is now claimable and its payload carries the environment secret.
	runner := needsRunner()
	var payload *FetchResult
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil || claim == nil {
			t.Fatalf("claim %d = %+v, %v", i, claim, cerr)
		}
		res, perr := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch: %v", perr)
		}
		if claim.Job.Name == "ship" {
			payload = res
		}
	}
	if payload == nil || payload.Secrets["DEPLOY_TOKEN"] != "env-secret-value" || len(payload.Mask) == 0 {
		t.Fatalf("ship payload = %+v", payload)
	}
	// A second decision is a conflict.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reviewer()); !errors.Is(err, ErrConflict) {
		t.Fatalf("second approve err = %v", err)
	}
	// Only names ever leave the read surface.
	names, err := f.svc.ListEnvironmentSecrets(ctx(), "o/r", "production", f.maintain())
	if err != nil || len(names) != 1 || names[0].Name != "DEPLOY_TOKEN" {
		t.Fatalf("secret names = %+v, %v", names, err)
	}
}

// TestEnvironmentDenyFailsTheDeployment pins denial: the gated job fails with a
// named reason, its secrets are never resolved, and the job is never queued.
func TestEnvironmentDenyFailsTheDeployment(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production", "writer")
	run := f.deployRun(t, CreateOpts{})
	if _, err := f.svc.DenyRun(ctx(), "o/r", run.ID, f.maintain()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-reviewer deny err = %v", err)
	}
	denied, err := f.svc.DenyRun(ctx(), "o/r", run.ID, f.reviewer())
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if denied.DeniedBy != "writer" || denied.DeniedAt == nil {
		t.Fatalf("denied = %+v", denied)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.State != JobFailure {
		t.Fatalf("denied job = %s deployment=%+v runkind=%q want failure", job.State, job.Deployment, denied.ApprovalKind)
	}
	if !strings.Contains(job.Reason, `deployment to environment "production" denied by writer`) {
		t.Fatalf("job reason = %q", job.Reason)
	}
	if job.Deployment.State != DeploymentDenied || job.Deployment.DeniedBy != "writer" {
		t.Fatalf("deployment = %+v", job.Deployment)
	}
	// Exactly one entry: the build job continues, the deployment never queues.
	if got := f.queueEntries(t); got != 1 {
		t.Fatalf("denied run enqueued %d jobs, want the build job only", got)
	}
	// The run itself is still live: the build job continues, only the
	// deployment failed.
	final, err := f.svc.loadRun(ctx(), "o", "r", run.ID)
	if err != nil || (final.State != RunQueued && final.State != RunRunning) {
		t.Fatalf("run after deny = %+v, %v", final, err)
	}
	// An audit entry names the environment, never a secret.
	if len(f.audit) == 0 || !strings.Contains(f.audit[len(f.audit)-1], AuditEnvironmentDeny) {
		t.Fatalf("audit = %v", f.audit)
	}
}

// TestEnvironmentUndefinedFailsClosed pins the typo rule: an environment with no
// record fails the job with a named reason instead of deploying anywhere.
func TestEnvironmentUndefinedFailsClosed(t *testing.T) {
	f := newEnvFixture(t)
	run := f.deployRun(t, CreateOpts{})
	job := envJob(t, f.testEnv, run.ID)
	if job.State != JobFailure || !strings.Contains(job.Reason, `environment "production" is not defined`) {
		t.Fatalf("undefined environment job = %s / %q", job.State, job.Reason)
	}
	if job.Deployment != nil {
		t.Fatalf("undefined environment carried a deployment: %+v", job.Deployment)
	}
	// The run itself stays live: the build job is queued and will run.
	final, err := f.svc.loadRun(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != RunQueued {
		t.Fatalf("run state = %s, want queued (the build job is still live)", final.State)
	}
	// The build job is still enqueued: one bad deployment does not silence the
	// workflow.
	if got := f.queueEntries(t); got != 1 {
		t.Fatalf("queue entries = %d, want the build job only", got)
	}
}

// TestEnvironmentWithoutReviewersRuns pins the ungated environment: the job
// queues immediately, the record says not_required, and its secrets resolve.
func TestEnvironmentWithoutReviewersRuns(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "DEPLOY_TOKEN", "v-deploy", f.maintain()); err != nil {
		t.Fatal(err)
	}
	run := f.deployRun(t, CreateOpts{})
	if run.State != RunQueued {
		t.Fatalf("run = %s, want queued", run.State)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.Deployment == nil || job.Deployment.State != DeploymentNotRequired {
		t.Fatalf("deployment = %+v", job.Deployment)
	}
	runner := needsRunner()
	// The build job comes first (sorted keys), so claim twice or search.
	var payload *FetchResult
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if claim == nil {
			break
		}
		res, perr := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch: %v", perr)
		}
		if claim.Job.Name == "ship" {
			payload = res
		}
	}
	if payload == nil || payload.Secrets["DEPLOY_TOKEN"] != "v-deploy" {
		t.Fatalf("ship payload = %+v", payload)
	}
}

// TestEnvironmentSecretsPrecedence pins GitHub's rule: an environment secret
// overrides a repository secret of the same name, and both are masked.
func TestEnvironmentSecretsPrecedence(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := f.svc.SetSecret(ctx(), "o/r", "TOKEN", "repo-token-value", admin); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetSecret(ctx(), "o/r", "REPO_ONLY", "repo-only-value", admin); err != nil {
		t.Fatal(err)
	}
	f.putEnvironment(t, "production")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "env-token-value", f.maintain()); err != nil {
		t.Fatal(err)
	}
	f.deployRun(t, CreateOpts{})
	runner := needsRunner()
	var ship *FetchResult
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil || claim == nil {
			t.Fatalf("claim %d = %+v, %v", i, claim, cerr)
		}
		res, perr := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch: %v", perr)
		}
		if claim.Job.Name == "ship" {
			ship = res
		} else if res.Secrets["TOKEN"] != "repo-token-value" {
			t.Fatalf("the build job must get the REPO secret, got %q", res.Secrets["TOKEN"])
		}
	}
	if ship == nil {
		t.Fatal("ship job was not claimed")
	}
	if ship.Secrets["TOKEN"] != "env-token-value" {
		t.Fatalf("environment secret did not override the repository one: %q", ship.Secrets["TOKEN"])
	}
	if ship.Secrets["REPO_ONLY"] != "repo-only-value" {
		t.Fatalf("the repository secret was dropped: %+v", ship.Secrets)
	}
	masked := maskSecretValues("a env-token-value b repo-only-value", ship.Mask)
	if strings.Contains(masked, "env-token-value") || strings.Contains(masked, "repo-only-value") {
		t.Fatalf("mask = %q", masked)
	}
}

// TestEnvironmentForkRunNeverGetsSecrets pins the trust rule: a fork run naming
// a reviewed environment stays pending, its actor cannot self-approve, and even
// an approved fork fetch carries no environment secret.
func TestEnvironmentForkRunNeverGetsSecrets(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.svc.ApproveFirstTimeForks = true
	f.putEnvironment(t, "production", "writer")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "DEPLOY_TOKEN", "v-deploy", f.maintain()); err != nil {
		t.Fatal(err)
	}
	runs, err := f.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened", Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Fork: true, Actor: "newcomer",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := runs[0]
	if run.State != RunPendingApproval || run.ApprovalKind != ApprovalEnvironment || run.Trusted {
		t.Fatalf("fork run = %+v", run)
	}
	// The actor has no write role, so it cannot approve its own deployment.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "newcomer"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("fork actor approve err = %v", err)
	}
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reviewer()); err != nil {
		t.Fatalf("listed reviewer approve: %v", err)
	}
	runner := needsRunner()
	var ship *FetchResult
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil || claim == nil {
			t.Fatalf("claim %d = %+v, %v", i, claim, cerr)
		}
		res, perr := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch: %v", perr)
		}
		if claim.Job.Name == "ship" {
			ship = res
		}
	}
	if ship == nil {
		t.Fatal("ship job was not claimed")
	}
	if !ship.SecretsDenied || len(ship.Secrets) != 0 || len(ship.Mask) != 0 {
		t.Fatalf("fork payload secrets = %+v denied=%v", ship.Secrets, ship.SecretsDenied)
	}
}

// TestEnvironmentDeletedWhilePendingFailsClosed pins the race the task calls
// out: a reviewer decision on a run whose environment was deleted fails the job
// with a named reason instead of releasing it.
func TestEnvironmentDeletedWhilePendingFailsClosed(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production", "writer")
	run := f.deployRun(t, CreateOpts{})
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatal(err)
	}
	// The reviewer is no longer listed (nothing to be listed in), so the
	// decision is refused; a repo admin may still decide, and then the job
	// fails closed with the reason.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reviewer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reviewer on a deleted environment err = %v", err)
	}
	admin := auth.Principal{Name: "admin", Admin: true}
	approved, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, admin)
	if err != nil {
		t.Fatalf("admin approve: %v", err)
	}
	if approved == nil {
		t.Fatal("nil run")
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.State != JobFailure || !strings.Contains(job.Reason, "no longer exists") {
		t.Fatalf("job after deleted environment = %s / %q", job.State, job.Reason)
	}
	if job.Deployment.State != DeploymentFailed {
		t.Fatalf("deployment = %+v", job.Deployment)
	}
	if got := f.queueEntries(t); got != 1 {
		t.Fatalf("queue entries = %d, want the build job only", got)
	}
}

// TestEnvironmentRerunReholds pins that a re-run is a NEW decision: the gate is
// re-applied and the approval stamps are cleared.
func TestEnvironmentRerunReholds(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production", "writer")
	run := f.deployRun(t, CreateOpts{})
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, f.reviewer()); err != nil {
		t.Fatal(err)
	}
	// Finish both jobs so the run is terminal and rerunnable.
	for _, job := range mustJobs(t, f.testEnv, run.ID) {
		if _, err := f.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	rerun, err := f.svc.RerunRun(ctx(), "o/r", run.ID, f.writer())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rerun.State != RunPendingApproval || rerun.ApprovalKind != ApprovalEnvironment ||
		rerun.ApprovedBy != "" || rerun.ApprovedAt != nil {
		t.Fatalf("rerun = %+v", rerun)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.State != JobQueued || job.Deployment == nil || job.Deployment.State != DeploymentPending ||
		job.Deployment.ApprovedBy != "" {
		t.Fatalf("rerun job = %s / %+v", job.State, job.Deployment)
	}
	// Nothing claimable names the deployment: the re-run's only live job is
	// the build one.
	runner := needsRunner()
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if claim == nil {
			break
		}
		if claim.Job.Name == "ship" {
			t.Fatal("a re-run enqueued the environment deployment before approval")
		}
	}
}

// TestEnvironmentSecretsFloorAndUnavailable pins the secret endpoints' floor and
// the disabled-keyring answer.
func TestEnvironmentSecretsFloorAndUnavailable(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	// No sealing key: a clear 503-class error, never a plaintext store.
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", f.maintain()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled keyring err = %v", err)
	}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v1", f.writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer set err = %v", err)
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "missing", "TOKEN", "v1", f.maintain()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown environment err = %v", err)
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "bad name", "v1", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad secret name err = %v", err)
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty value err = %v", err)
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v1", f.maintain()); err != nil {
		t.Fatal(err)
	}
	// The value is sealed (never in the clear) and bound to (env, name).
	raw, _, err := store.GetBytes(ctx(), f.store, EnvironmentSecretKey("o", "r", "production", "TOKEN"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"v1"`) {
		t.Fatal("environment secret stored in the clear")
	}
	var doc SecretDoc
	if jerr := jsonUnmarshalTest(raw, &doc); jerr != nil {
		t.Fatal(jerr)
	}
	if _, oerr := f.svc.openEnvironmentSecret("o/r", "production", "OTHER", doc.Sealed); oerr == nil {
		t.Fatal("the ciphertext must be bound to its name")
	}
	if _, oerr := f.svc.openEnvironmentSecret("o/r", "staging", "TOKEN", doc.Sealed); oerr == nil {
		t.Fatal("the ciphertext must be bound to its environment")
	}
	if got, oerr := f.svc.openEnvironmentSecret("o/r", "production", "TOKEN", doc.Sealed); oerr != nil || got != "v1" {
		t.Fatalf("round-trip = %q, %v", got, oerr)
	}
	// Deleting drops the allowlist entry too (no orphaned name).
	if err := f.svc.DeleteEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", f.maintain()); err != nil {
		t.Fatal(err)
	}
	env, err := f.svc.GetEnvironment(ctx(), "o/r", "production", f.maintain())
	if err != nil || len(env.Secrets) != 0 {
		t.Fatalf("after delete = %+v, %v", env, err)
	}
}

// jsonUnmarshalTest keeps the test file's imports small.
func jsonUnmarshalTest(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// TestEnvironmentFaults pins the failure side: every read/write that cannot
// complete fails closed with a real error and never a half record.
func TestEnvironmentFaults(t *testing.T) {
	e := newTestEnv(t)
	fs := newFaultStore()
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Now = e.svc.Now
	e.svc = svc
	e.roles.roles["maint"] = "maintain"
	maint := auth.Principal{Name: "maint"}
	fk, kerr := NewKeyring(strings.Repeat("k", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	svc.SetKeyring(fk)
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	// A failing read fails every reader (never "no environments").
	fs.fail("get", 0)
	if _, err := svc.GetEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("GetEnvironment with a failing get must fail")
	}
	if _, err := svc.ListEnvironments(ctx(), "o/r", maint); err == nil {
		t.Fatal("ListEnvironments with a failing list must fail")
	}
	if _, err := svc.environmentSecretValues(ctx(), "o", "r", "production"); err == nil {
		t.Fatal("environmentSecretValues with a failing get must fail")
	}
	fs.clear()
	// A failing list fails the delete (the secrets would be orphaned).
	fs.fail("list", 0)
	if err := svc.DeleteEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("DeleteEnvironment with a failing list must fail")
	}
	fs.clear()
	// A failing delete fails the delete.
	fs.fail("delete", 0)
	if err := svc.DeleteEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("DeleteEnvironment with a failing delete must fail")
	}
	fs.clear()
	// A corrupt record is ErrCorrupt, never silently empty.
	if _, perr := fs.Memory.Put(ctx(), EnvironmentKey("o", "r", "production"), store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if _, err := svc.GetEnvironment(ctx(), "o/r", "production", maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt get = %v, want ErrCorrupt", err)
	}
	if _, err := svc.ListEnvironments(ctx(), "o/r", maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt list = %v, want ErrCorrupt", err)
	}
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt put = %v, want ErrCorrupt", err)
	}
	if _, err := svc.ListEnvironmentSecrets(ctx(), "o/r", "production", maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt secret list = %v, want ErrCorrupt", err)
	}
	// A failing put fails the record write and the sealed write.
	fs.fail("put", 0)
	if _, err := svc.PutEnvironment(ctx(), "o/r", "staging", EnvironmentInput{}, maint); err == nil {
		t.Fatal("PutEnvironment with a failing put must fail")
	}
	fs.clear()
	if err := fs.Memory.Delete(ctx(), EnvironmentKey("o", "r", "production"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	fs.fail("put", 0)
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); err == nil {
		t.Fatal("SetEnvironmentSecret with a failing allowlist write must fail")
	}
	fs.clear()
	// A corrupt allowlist CAS fails the add.
	if _, perr := fs.Memory.Put(ctx(), EnvironmentKey("o", "r", "production"), store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt allowlist = %v, want ErrCorrupt", err)
	}
}

// TestEnvironmentSecretPayloadEdges pins the payload-side fail-closed rules: a
// deleted environment and a corrupt/missing envelope.
func TestEnvironmentSecretPayloadEdges(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "value-1", f.maintain()); err != nil {
		t.Fatal(err)
	}
	// The allowlist's listed-but-absent name is skipped, not injected empty.
	if err := f.store.Delete(ctx(), EnvironmentSecretKey("o", "r", "production", "TOKEN"), ""); err != nil {
		t.Fatal(err)
	}
	vals, err := f.svc.environmentSecretValues(ctx(), "o", "r", "production")
	if err != nil || len(vals) != 0 {
		t.Fatalf("missing ciphertext = %+v, %v", vals, err)
	}
	// A corrupt envelope fails the resolution.
	f.putEnvironment(t, "production")
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "value-2", f.maintain()); err != nil {
		t.Fatal(err)
	}
	_, meta, gerr := store.GetBytes(ctx(), f.store, EnvironmentSecretKey("o", "r", "production", "TOKEN"), store.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if _, perr := f.store.Put(ctx(), EnvironmentSecretKey("o", "r", "production", "TOKEN"),
		store.PutBody{Bytes: []byte(`{"name":"TOKEN","sealed":"%%%"}`)}, store.PutOptions{IfVersion: meta.Version}); perr != nil {
		t.Fatal(perr)
	}
	if _, err := f.svc.environmentSecretValues(ctx(), "o", "r", "production"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt envelope = %v, want ErrCorrupt", err)
	}
	// A deleted environment is a named error, never a partial map.
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.environmentSecretValues(ctx(), "o", "r", "production"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted environment = %v, want ErrNotFound", err)
	}
	// guardDeploymentSecrets: a gone environment fails, an unapproved reviewed
	// deployment fails, an ungated one passes.
	job := &Job{Spec: &Spec{Environment: &EnvironmentSpec{Name: "production"}}, Deployment: &Deployment{Environment: "production", State: DeploymentPending}}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", job); !errors.Is(err, ErrNotFound) {
		t.Fatalf("guard deleted = %v", err)
	}
	job.Spec.Environment = nil
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", job); err != nil {
		t.Fatalf("a job without an environment must pass: %v", err)
	}
}

// TestEnvironmentApprovalProjectionAndIntersection pins the principal-aware
// projection and the multi-environment rule: a run naming two gated
// environments may only be decided by a reviewer listed on BOTH (or an admin).
func TestEnvironmentApprovalProjectionAndIntersection(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["grace"] = "write"
	f.roles.roles["maint"] = "maintain"
	f.putEnvironment(t, "production", "writer")
	f.putEnvironment(t, "staging", "grace")
	run := f.deployRun(t, CreateOpts{})
	// Not held yet (one env), then simulate a run naming both.
	if run.State != RunPendingApproval || run.ApprovalKind != ApprovalEnvironment {
		t.Fatalf("run = %+v", run)
	}
	run.Environments = []string{"production", "staging"}
	// A reviewer listed on only one of them cannot decide.
	if err := f.svc.holdsForEnvironment(ctx(), "o", "r", f.reviewer(), run); !errors.Is(err, ErrForbidden) {
		t.Fatalf("single-environment reviewer err = %v, want forbidden", err)
	}
	if err := f.svc.holdsForEnvironment(ctx(), "o", "r", auth.Principal{Name: "admin", Admin: true}, run); err != nil {
		t.Fatalf("admin must always decide: %v", err)
	}
	// The projection names the union of the reviewers and refuses the same
	// principals as the verb does.
	av := f.svc.ApprovalFor(ctx(), "o", "r", run, f.reviewer())
	if av.CanDecide || len(av.Reviewers) != 2 {
		t.Fatalf("projection = %+v", av)
	}
	// A repo maintainer is not an admin: without a listing they cannot decide
	// either (the environment rule is the reviewer set, not the role ladder).
	if f.svc.ApprovalFor(ctx(), "o", "r", run, f.maintain()).CanDecide {
		t.Fatal("an unlisted maintainer must not decide an environment hold")
	}
	// A fork hold (no environments) projects the maintainer rule.
	forkRun := &Run{ID: run.ID, State: RunPendingApproval, ApprovalKind: ApprovalFork}
	if f.svc.ApprovalFor(ctx(), "o", "r", forkRun, f.reviewer()).CanDecide {
		t.Fatal("a write principal must not decide a fork hold")
	}
	if !f.svc.ApprovalFor(ctx(), "o", "r", forkRun, f.maintain()).CanDecide {
		t.Fatal("a maintainer must decide a fork hold")
	}
	// A finished run projects as not pending, whatever the kind.
	done := &Run{ID: run.ID, State: RunSuccess, ApprovalKind: ApprovalEnvironment}
	if av := f.svc.ApprovalFor(ctx(), "o", "r", done, f.maintain()); av.Pending {
		t.Fatalf("done projection = %+v", av)
	}
}

// TestEnvironmentRecordReadEdges covers the record edges the payload tests do
// not reach: a listed secret whose doc is absent or corrupt, deleting a secret
// on a vanished/corrupt record, and the view projection's nil arm.
func TestEnvironmentRecordReadEdges(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production")
	// List a name whose document is absent: skipped, not an error.
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v1", f.maintain()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx(), EnvironmentSecretKey("o", "r", "production", "TOKEN"), ""); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.ListEnvironmentSecrets(ctx(), "o/r", "production", f.maintain()); err != nil || len(got) != 0 {
		t.Fatalf("absent ciphertext list = %+v, %v", got, err)
	}
	// A corrupt secret doc under a listed name fails the list.
	if _, perr := f.store.Put(ctx(), EnvironmentSecretKey("o", "r", "production", "TOKEN"),
		store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if _, err := f.svc.ListEnvironmentSecrets(ctx(), "o/r", "production", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt secret list = %v, want ErrCorrupt", err)
	}
	// Deleting the secret still drops the allowlist entry.
	if err := f.svc.DeleteEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", f.maintain()); err != nil {
		t.Fatal(err)
	}
	// Deleting a secret on a vanished environment is a no-op after the value
	// delete (the CAS finds no record).
	if err := f.svc.DeleteEnvironmentSecret(ctx(), "o/r", "gone", "TOKEN", f.maintain()); err != nil {
		t.Fatalf("delete on a vanished environment = %v", err)
	}
	// Deleting a secret on a corrupt record is ErrCorrupt (never a silent drop).
	if err := f.store.Delete(ctx(), EnvironmentKey("o", "r", "production"), ""); err != nil {
		t.Fatal(err)
	}
	if _, perr := f.store.Put(ctx(), EnvironmentKey("o", "r", "production"),
		store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if err := f.svc.DeleteEnvironmentSecret(ctx(), "o/r", "production", "OTHER", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("delete on corrupt record = %v, want ErrCorrupt", err)
	}
	// The projection's nil arm.
	if got := environmentView(nil); got != nil {
		t.Fatalf("nil view = %+v", got)
	}
}

// TestEnvironmentUnitEdges collects the remaining branch edges directly (the
// helpers and their guard arms).
func TestEnvironmentUnitEdges(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production", "writer")
	if _, err := f.svc.GetEnvironment(ctx(), "o/r", "bad name", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid get name = %v", err)
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "bad name", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid delete name = %v", err)
	}
	// guardDeploymentSecrets: a reviewed environment with a pending deployment
	// is refused, and a nil job passes.
	job := &Job{Spec: &Spec{Environment: &EnvironmentSpec{Name: "production"}},
		Deployment: &Deployment{Environment: "production", State: DeploymentPending}}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", job); !errors.Is(err, ErrForbidden) {
		t.Fatalf("pending reviewed env = %v, want ErrForbidden", err)
	}
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", nil); err != nil {
		t.Fatalf("nil job = %v", err)
	}
	job.Deployment = nil
	if err := f.svc.guardDeploymentSecrets(ctx(), "o", "r", job); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing deployment = %v, want ErrForbidden", err)
	}
	// The gated list helpers dedupe and skip a non-queued job.
	jobs := []*Job{
		{State: JobQueued, Deployment: &Deployment{Environment: "production", State: DeploymentPending}},
		{State: JobQueued, Deployment: &Deployment{Environment: "production", State: DeploymentPending}},
		{State: JobFailure, Deployment: &Deployment{Environment: "staging", State: DeploymentPending}},
	}
	if got := pendingEnvironments(jobs); len(got) != 1 || got[0] != "production" {
		t.Fatalf("pendingEnvironments = %v", got)
	}
	if got := uniqueEnvNames([]string{"b", "a", "b"}); strings.Join(got, ",") != "a,b" {
		t.Fatalf("uniqueEnvNames = %v", got)
	}
	// Own the failure side of the resolution and the delete loop.
	fs := newFaultStore()
	e := newTestEnv(t)
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Now = e.svc.Now
	e.roles.roles["maint"] = "maintain"
	maint := auth.Principal{Name: "maint"}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	svc.SetKeyring(k)
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v1", maint); err != nil {
		t.Fatal(err)
	}
	// A failing read fails the resolution.
	fs.fail("get", 0)
	if _, err := svc.environmentSecretValues(ctx(), "o", "r", "production"); err == nil {
		t.Fatal("resolution with a failing get must fail")
	}
	fs.clear()
	// A failing delete inside the environment delete loop fails the delete.
	fs.fail("delete", 0)
	if err := svc.DeleteEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("a failing secret delete must fail the environment delete")
	}
	fs.clear()
	// A failing sealed-value write fails the set (the allowlist is already
	// updated; the name then reads as listed-but-absent, which resolution
	// skips).
	fs.fail("put", 1)
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "OTHER", "v2", maint); err == nil {
		t.Fatal("a failing sealed write must fail the set")
	}
	fs.clear()
	// A corrupt sealed record fails both the set (CAS decode) and the delete.
	if _, perr := fs.Memory.Put(ctx(), EnvironmentSecretKey("o", "r", "production", "OTHER"),
		store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "OTHER", "v3", maint); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt sealed set = %v, want ErrCorrupt", err)
	}
	// holdsForEnvironment surfaces a record read failure.
	fs.fail("get", 0)
	if err := svc.holdsForEnvironment(ctx(), "o", "r", maint, &Run{Environments: []string{"production"}}); err == nil {
		t.Fatal("a failing environment read must fail the decision")
	}
	fs.clear()
}

// TestEnvironmentEnvelopeShapes pins the envelope decoder's short/corrupt arms
// and a failing read in the name list.
func TestEnvironmentEnvelopeShapes(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production")
	// A one-byte envelope is shorter than a nonce: ErrCorrupt.
	if _, err := f.svc.openEnvironmentSecret("o/r", "production", "TOKEN", "AA=="); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short envelope = %v, want ErrCorrupt", err)
	}
	// A name in the allowlist whose document read fails reports the store
	// error, never an empty list.
	fs := newFaultStore()
	e := newTestEnv(t)
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Now = e.svc.Now
	e.roles.roles["maint"] = "maintain"
	maint := auth.Principal{Name: "maint"}
	svc.SetKeyring(k)
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); err != nil {
		t.Fatal(err)
	}
	fs.fail("get", 1) // the record read succeeds, the secret read fails
	if _, err := svc.ListEnvironmentSecrets(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("a failing secret read must fail the list")
	}
}

// TestEnvironmentDecisionFaults pins the decision verbs' failure side: an
// unreadable environment record or a failing job write leaves the run held
// (never released without its decision), and a failing allowlist write fails a
// secret delete.
func TestEnvironmentDecisionFaults(t *testing.T) {
	e := newTestEnv(t)
	fs := newFaultStore()
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Checks = e.checks
	svc.Now = e.svc.Now
	e.roles.roles["maint"] = "maintain"
	e.roles.roles["writer"] = "write"
	svc.Git = e.git
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", environmentWorkflowYAML)
	maint := auth.Principal{Name: "maint"}
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{RequiredReviewers: []string{"writer"}}, maint); err != nil {
		t.Fatal(err)
	}
	runs, err := svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	run := runs[0]
	// An unreadable record fails the decision (the run stays held).
	fs.fail("get", 0)
	if _, err := svc.ApproveRun(ctx(), "o/r", run.ID, maint); err == nil {
		t.Fatal("approve with a failing read must fail")
	}
	if _, err := svc.DenyRun(ctx(), "o/r", run.ID, maint); err == nil {
		t.Fatal("deny with a failing read must fail")
	}
	fs.clear()
	still, lerr := svc.loadRun(ctx(), "o", "r", run.ID)
	if lerr != nil || still.State != RunPendingApproval {
		t.Fatalf("run after failed decisions = %+v, %v", still, lerr)
	}
	// A failing deployment-record write fails the approval (nothing enqueued).
	fs.fail("put", 1)
	if _, err := svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "writer"}); err == nil {
		t.Fatal("approve with a failing job write must fail")
	}
	fs.clear()
	// The secret delete's allowlist CAS failure surfaces.
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	svc.SetKeyring(k)
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); err != nil {
		t.Fatal(err)
	}
	fs.fail("put", 0)
	if err := svc.DeleteEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", maint); err == nil {
		t.Fatal("a failing allowlist write must fail the delete")
	}
}

// TestEnvironmentRemainingEdges closes the small reachable arms: the reviewer
// name cap, keeping a co-listed secret on delete, the list's unknown/failing
// reads, and the payload guard's store failure.
func TestEnvironmentRemainingEdges(t *testing.T) {
	f := newEnvFixture(t)
	long := strings.Repeat("r", 101)
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: []string{long}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long reviewer name = %v", err)
	}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	f.svc.SetKeyring(k)
	f.putEnvironment(t, "production")
	for _, name := range []string{"ALPHA", "BETA"} {
		if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", name, "v-"+name, f.maintain()); err != nil {
			t.Fatal(err)
		}
	}
	// Deleting one keeps the other listed.
	if err := f.svc.DeleteEnvironmentSecret(ctx(), "o/r", "production", "ALPHA", f.maintain()); err != nil {
		t.Fatal(err)
	}
	env, err := f.svc.GetEnvironment(ctx(), "o/r", "production", f.maintain())
	if err != nil || strings.Join(env.Secrets, ",") != "BETA" {
		t.Fatalf("allowlist after one delete = %+v, %v", env, err)
	}
	// Listing an unknown environment is a 404-class error.
	if _, err := f.svc.ListEnvironmentSecrets(ctx(), "o/r", "missing", f.maintain()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown list = %v", err)
	}
	// The payload guard and the payload read surface a store failure.
	fs := newFaultStore()
	e := newTestEnv(t)
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Now = e.svc.Now
	e.roles.roles["maint"] = "maintain"
	svc.SetKeyring(k)
	maint := auth.Principal{Name: "maint"}
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); err != nil {
		t.Fatal(err)
	}
	job := &Job{Spec: &Spec{Environment: &EnvironmentSpec{Name: "production"}},
		Deployment: &Deployment{Environment: "production", State: DeploymentNotRequired}}
	fs.fail("get", 0)
	if err := svc.guardDeploymentSecrets(ctx(), "o", "r", job); err == nil {
		t.Fatal("guard with a failing read must fail")
	}
	fs.clear()
	fs.fail("get", 1) // record ok, secret read fails
	if _, err := svc.environmentSecretValues(ctx(), "o", "r", "production"); err == nil {
		t.Fatal("resolution with a failing secret read must fail")
	}
}

// TestEnvironmentStoreFailureArms covers the delete/get/list read failures that
// each mutation must surface rather than swallow.
func TestEnvironmentStoreFailureArms(t *testing.T) {
	fs := newFaultStore()
	e := newTestEnv(t)
	e.store = fs.Memory
	svc := New(fs, e.roles)
	svc.Git = e.git
	svc.Now = e.svc.Now
	e.roles.roles["maint"] = "maintain"
	maint := auth.Principal{Name: "maint"}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	svc.SetKeyring(k)
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, maint); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetEnvironmentSecret(ctx(), "o/r", "production", "TOKEN", "v", maint); err != nil {
		t.Fatal(err)
	}
	// Get: a failing read.
	fs.fail("get", 0)
	if _, err := svc.GetEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("get with a failing read must fail")
	}
	fs.clear()
	// List secrets: a failing record read.
	fs.fail("get", 0)
	if _, err := svc.ListEnvironmentSecrets(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("secret list with a failing record read must fail")
	}
	fs.clear()
	// Delete: the secrets are removed, then the record delete fails.
	fs.fail("delete", 1)
	if err := svc.DeleteEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("a failing record delete must fail the environment delete")
	}
	fs.clear()
	// DeleteEnvironment's List failure (a repository with no secrets prefix).
	fs.fail("list", 0)
	if err := svc.DeleteEnvironment(ctx(), "o/r", "production", maint); err == nil {
		t.Fatal("a failing list must fail the environment delete")
	}
}

// phantomStore lists one key that does not exist (a record deleted between the
// list and the read): the list must skip it, never fail or emit an empty row.
type phantomStore struct {
	*store.Memory
	phantom string
}

func (p *phantomStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if err := p.Memory.List(ctx, prefix, after, fn); err != nil {
		return err
	}
	if p.phantom != "" && strings.HasPrefix(p.phantom, prefix) {
		if err := fn(store.ObjectMeta{Key: p.phantom}); err != nil {
			return err
		}
	}
	return nil
}

// TestEnvironmentVanishingRecordAndAnonymousReviewer covers the list's
// skip-a-vanished-record arm and the no-principal reviewer refusal.
func TestEnvironmentVanishingRecordAndAnonymousReviewer(t *testing.T) {
	ps := &phantomStore{Memory: store.NewMemory(), phantom: EnvironmentKey("o", "r", "ghost")}
	svc := New(ps, nil)
	e := newTestEnv(t)
	svc.Now = func() time.Time { return e.now }
	admin := auth.Principal{Name: "admin", Admin: true}
	if _, err := svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, admin); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListEnvironments(ctx(), "o/r", admin)
	if err != nil || len(list) != 1 || list[0].Name != "production" {
		t.Fatalf("list with a phantom record = %+v, %v", list, err)
	}
	ps.phantom = "" // stop injecting before the decision check
	// An empty principal name with write authority cannot decide: the decision
	// needs a named reviewer.
	// The record is written directly: reviewer validation needs a role service,
	// and this fixture deliberately has none.
	if _, perr := ps.Memory.Put(ctx(), EnvironmentKey("o", "r", "production"),
		store.PutBody{Bytes: []byte(`{"name":"production","required_reviewers":["ada"],"created_at":"2026-11-23T00:00:00Z","updated_at":"2026-11-23T00:00:00Z","version":1}`)},
		store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	err = svc.holdsForEnvironment(ctx(), "o", "r", auth.Principal{Write: true}, &Run{Environments: []string{"production"}})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "signed-in reviewer") {
		t.Fatalf("anonymous reviewer = %v", err)
	}
}
