package threads

// deployments_test.go — PAR-132/133/136: the deployment record family, the
// API, the job-token authorization matrix and the per-environment
// serialization queue.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func deploymentJobToken(repo string, level string) auth.Principal {
	return auth.Principal{Name: "ci:r1", JobRepo: repo, JobPerms: map[string]string{"deployments": level}}
}

func TestDeploymentCreateStatusAndHistory(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")

	w := f.writer()
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{
		Ref: "refs/heads/main", Environment: "production", Description: "release",
		URL: "https://prod.example.com", LogURL: "https://ci.example.com/1",
	}, w)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if dep.State != DeploymentStatePending || dep.QueuePosition != 0 {
		t.Fatalf("created = %+v", dep)
	}
	if dep.SHA != testSHA || dep.Environment != "production" || dep.URL != "https://prod.example.com" {
		t.Fatalf("record = %+v", dep)
	}
	if len(dep.Statuses) != 1 || dep.Statuses[0].State != DeploymentStatePending {
		t.Fatalf("statuses = %+v", dep.Statuses)
	}

	updated, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{
		State: DeploymentStateInProgress, Description: "rolling out",
	}, w)
	if err != nil {
		t.Fatalf("status in_progress: %v", err)
	}
	if updated.State != DeploymentStateInProgress || len(updated.Statuses) != 2 {
		t.Fatalf("in_progress = %+v", updated)
	}
	if updated.Statuses[1].Creator != "writer" || updated.Statuses[1].Description != "rolling out" {
		t.Fatalf("status actor/description = %+v", updated.Statuses[1])
	}
	done, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{
		State: DeploymentStateSuccess, EnvironmentURL: "https://prod.example.com/v2",
	}, w)
	if err != nil {
		t.Fatalf("status success: %v", err)
	}
	if done.State != DeploymentStateSuccess || done.URL != "https://prod.example.com/v2" {
		t.Fatalf("done = %+v", done)
	}
	if len(done.Statuses) != 3 {
		t.Fatalf("history = %+v", done.Statuses)
	}

	// The list is newest first and filters by environment.
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w); err != nil {
		t.Fatalf("second create: %v", err)
	}
	f.putEnvironment(t, "staging")
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "staging"}, w); err != nil {
		t.Fatalf("staging create: %v", err)
	}
	page, err := f.svc.ListDeployments(ctx(), "o/r", "production", w, "", 10)
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Deployments) != 2 {
		t.Fatalf("production deployments = %d, want 2", len(page.Deployments))
	}
	if page.Deployments[0].ID <= page.Deployments[1].ID {
		t.Fatalf("not newest-first: %s then %s", page.Deployments[0].ID, page.Deployments[1].ID)
	}
	all, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 10)
	if err != nil || len(all.Deployments) != 3 {
		t.Fatalf("all deployments = %d, err %v", len(all.Deployments), err)
	}
}

func TestDeploymentAuthorizationMatrix(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")

	in := DeploymentInput{Ref: "main", Environment: "production"}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, f.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader create = %v, want forbidden", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous create = %v, want unauthorized", err)
	}

	// A job token gets exactly its scoped authority: same repo + deployments:
	// write. It is never admin and never reaches another repository.
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, deploymentJobToken("o/r", PermWrite)); err != nil {
		t.Fatalf("deployments: write job token create: %v", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, deploymentJobToken("o/r", PermRead)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("deployments: read job token create = %v, want forbidden", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, deploymentJobToken("o/other", PermWrite)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other-repo job token = %v, want forbidden", err)
	}
	// A contents: write token has no deployments authority even though its
	// repository role is write.
	contents := auth.Principal{Name: "ci:r1", JobRepo: "o/r", JobPerms: map[string]string{"contents": "write"}}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", in, contents); !errors.Is(err, ErrForbidden) {
		t.Fatalf("contents-only job token = %v, want forbidden", err)
	}

	// Reading accepts deployments: read, and status writes still need write.
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", in, f.writer())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.GetDeployment(ctx(), "o/r", dep.ID, deploymentJobToken("o/r", PermRead)); err != nil {
		t.Fatalf("deployments: read job token get: %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess}, deploymentJobToken("o/r", PermRead)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read token status = %v, want forbidden", err)
	}
}

func TestDeploymentRequiresAnExistingEnvironmentAndPolicy(t *testing.T) {
	f := newEnvFixture(t)
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "nope"}, f.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undefined environment = %v, want not found", err)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{DeploymentBranches: []string{"main"}}, f.maintain()); err != nil {
		t.Fatalf("put environment: %v", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "refs/heads/dev", Environment: "production"}, f.writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("branch-policy refusal = %v, want forbidden", err)
	}
}

func TestDeploymentQueueSerializesAndPromotes(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()

	first, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.State != DeploymentStatePending || first.QueuePosition != 0 {
		t.Fatalf("first = %+v", first)
	}
	second, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.State != DeploymentStateQueued || second.QueuePosition != 1 {
		t.Fatalf("second = %+v", second)
	}
	if !strings.Contains(second.QueueReason, "production") || !strings.Contains(second.QueueReason, "busy") {
		t.Fatalf("queue reason = %q", second.QueueReason)
	}
	// A queued deployment cannot jump the line.
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", second.ID, DeploymentStatusInput{State: DeploymentStateInProgress}, w); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued in_progress = %v, want conflict", err)
	}

	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", first.ID, DeploymentStatusInput{State: DeploymentStateSuccess}, w); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	promoted, err := f.svc.GetDeployment(ctx(), "o/r", second.ID, w)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if promoted.State != DeploymentStatePending || promoted.QueuePosition != 0 || promoted.QueueReason != "" {
		t.Fatalf("promoted = %+v", promoted)
	}
}

func TestJobDeploymentRecordsInTheSharedHistory(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	run := f.deployRun(t, CreateOpts{})
	ship := envJob(t, f.testEnv, run.ID)
	if ship.Deployment == nil || ship.Deployment.DeploymentID == "" {
		t.Fatalf("job deployment = %+v", ship.Deployment)
	}
	record, err := f.svc.GetDeployment(ctx(), "o/r", ship.Deployment.DeploymentID, f.writer())
	if err != nil {
		t.Fatalf("deployment record: %v", err)
	}
	if record.Environment != "production" || record.RunID != run.ID || record.JobID != ship.ID {
		t.Fatalf("record = %+v", record)
	}
	// The job's terminal state appends a history entry and releases the slot.
	if _, err := f.svc.UpdateJob(ctx(), "o/r", run.ID, ship.ID, JobUpdate{State: JobFailure, Reason: "boom"}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	record, err = f.svc.GetDeployment(ctx(), "o/r", ship.Deployment.DeploymentID, f.writer())
	if err != nil {
		t.Fatalf("deployment record after: %v", err)
	}
	if record.State != DeploymentStateFailure {
		t.Fatalf("record state = %q, want failure", record.State)
	}
	if len(record.Statuses) < 2 {
		t.Fatalf("history = %+v", record.Statuses)
	}
}

func TestJobDeploymentQueueSerializesRuns(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")

	create := func() *Run {
		runs, err := f.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{
			Branch: "main", Ref: "refs/heads/main", Actor: "writer", Workflow: "deploy.yml",
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		return runs[0]
	}
	first, second := create(), create()
	firstShip, secondShip := envJob(t, f.testEnv, first.ID), envJob(t, f.testEnv, second.ID)
	if firstShip.Deployment.QueuePosition != 0 {
		t.Fatalf("first queue position = %d", firstShip.Deployment.QueuePosition)
	}
	if secondShip.Deployment.QueuePosition != 1 || secondShip.Deployment.QueueReason == "" {
		t.Fatalf("second deployment = %+v", secondShip.Deployment)
	}
	if _, err := f.svc.UpdateJob(ctx(), "o/r", first.ID, firstShip.ID, JobUpdate{State: JobSuccess}); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	promoted := envJob(t, f.testEnv, second.ID)
	if promoted.Deployment.QueuePosition != 0 || promoted.Deployment.QueueReason != "" {
		t.Fatalf("promoted job deployment = %+v", promoted.Deployment)
	}
}

func TestDeploymentRetentionAndGC(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	f.svc.Retention = time.Hour
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, f.writer())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess}, f.writer()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	f.advance(2 * time.Hour)
	if _, err := f.svc.GetDeployment(ctx(), "o/r", dep.ID, f.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired get = %v, want not found", err)
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", f.writer(), "", 10); err != nil || len(page.Deployments) != 0 {
		t.Fatalf("expired list = %d, err %v", len(page.Deployments), err)
	}
	if _, err := f.svc.GC(ctx(), "o", "r"); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if raw, _, err := f.svc.getJSON(ctx(), DeploymentKey("o", "r", dep.ID)); err != nil || raw != nil {
		t.Fatalf("GC did not delete the record (raw=%v err=%v)", raw, err)
	}
}

func TestEnvironmentVariablesOverrideRepositoryVariables(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)
	admin := auth.Principal{Name: "maint", Admin: true}
	if err := f.svc.SetVariable(ctx(), "o/r", "REGION", "repo-value", admin); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "REGION", "env-value", admin); err != nil {
		t.Fatalf("SetEnvironmentVariable: %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "ONLY_ENV", "yes", admin); err != nil {
		t.Fatalf("SetEnvironmentVariable ONLY_ENV: %v", err)
	}
	vars, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "production", admin)
	if err != nil {
		t.Fatalf("ListEnvironmentVariables: %v", err)
	}
	if len(vars) != 2 || vars[0].Name != "ONLY_ENV" || vars[1].Name != "REGION" {
		t.Fatalf("variables = %+v", vars)
	}
	env, err := f.svc.environmentVariableValues(ctx(), "o", "r", "production")
	if err != nil {
		t.Fatalf("environmentVariableValues: %v", err)
	}
	if env["REGION"] != "env-value" || env["ONLY_ENV"] != "yes" {
		t.Fatalf("environment variables = %v", env)
	}
	// A reserved prefix is refused here too (the same rule as repository vars).
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "GITHUB_TOKEN", "x", admin); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reserved prefix = %v, want invalid", err)
	}
	if err := f.svc.DeleteEnvironmentVariable(ctx(), "o/r", "production", "REGION", admin); err != nil {
		t.Fatalf("delete: %v", err)
	}
	env, err = f.svc.environmentVariableValues(ctx(), "o", "r", "production")
	if err != nil || env["REGION"] != "" {
		t.Fatalf("after delete = %v, err %v", env, err)
	}
}

func TestCancelJobGranularityAndActor(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	jobs := mustJobs(t, e, run.ID)
	var build, test *Job
	for _, j := range jobs {
		if j.Key == "build" {
			build = j
		}
		if j.Key == "test" {
			test = j
		}
	}
	if build == nil || test == nil {
		t.Fatalf("jobs = %+v", jobs)
	}
	cancelled, err := e.svc.CancelJob(ctx(), "o/r", run.ID, build.ID, false, e.writer())
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if cancelled.State != JobCancelled || cancelled.CancelledBy != "writer" || cancelled.CancelReason != CancelReasonOperator {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	sibling, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	for _, j := range sibling.Jobs {
		if j.ID == build.ID {
			continue
		}
		if terminal(j.State) && j.State == JobCancelled {
			t.Fatalf("sibling %s was cancelled with the single-job cancel", j.ID)
		}
	}
	// A force cancel of a running attempt is terminal at once and honest.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, test.ID, JobUpdate{State: JobRunning}); err != nil {
		t.Fatalf("set running: %v", err)
	}
	forced, err := e.svc.CancelJob(ctx(), "o/r", run.ID, test.ID, true, e.writer())
	if err != nil {
		t.Fatalf("force CancelJob: %v", err)
	}
	if forced.State != JobCancelled || !forced.ForceCancelled || forced.CancelledBy != "writer" {
		t.Fatalf("forced = %+v", forced)
	}
	// The record is write-once: a later cancel does not rewrite the actor.
	again, err := e.svc.CancelJob(ctx(), "o/r", run.ID, test.ID, false, e.writer())
	if err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if again.CancelledBy != "writer" {
		t.Fatalf("actor rewritten to %q", again.CancelledBy)
	}
}

func TestDeploymentsAPIAndEnvironmentVariablesHTTP(t *testing.T) {
	e, h, cur := newHTTP(t)
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	e.svc.SetKeyring(key)
	*cur = auth.Principal{Name: "maint", Admin: true}
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, *cur); err != nil {
		t.Fatalf("put environment: %v", err)
	}

	rec := do(h, http.MethodPost, "/o/r/api/threads/deployments",
		`{"ref":"refs/heads/main","environment":"production","description":"d","url":"https://prod.test","log_url":"https://log.test","task":"deploy","transient_environment":true,"production_environment":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Deployment *DeploymentDoc `json:"deployment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Deployment == nil {
		t.Fatalf("create body = %s (%v)", rec.Body.String(), err)
	}
	id := created.Deployment.ID

	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments?n=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments?n=0", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad n = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/o/r/api/threads/deployments/"+id, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/"+id+"/statuses", `{"state":"in_progress","description":"go","environment_url":"https://prod.test/2","log_url":"https://log.test/2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/"+id+"/statuses", `{"state":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/"+id+"/statuses", `{"state":"success","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"main","environment":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown environment = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"","environment":"production"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty ref = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"main","environment":"production","url":"ftp://x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", rec.Code)
	}

	// The environment variable surface on both lanes.
	if rec := do(h, http.MethodGet, "/o/r/api/threads/environments/production/variables", ""); rec.Code != http.StatusOK {
		t.Fatalf("vars list = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production/variables/REGION", `{"value":"eu"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("vars put = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/o/r/api/threads/environments/production/variables", "")
	if !strings.Contains(rec.Body.String(), `"REGION"`) {
		t.Fatalf("vars list = %s", rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/o/r/api/threads/environments/production/variables/REGION", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("vars delete = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/o/r/api/threads/environments/production/secrets/NOPE", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("absent secret delete = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/environments/production/variables/REGION", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("vars POST = %d", rec.Code)
	}

	// The job-token matrix on the HTTP boundary: own repo + deployments: write,
	// never another repository.
	*cur = deploymentJobToken("o/r", PermWrite)
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"main","environment":"production"}`); rec.Code != http.StatusCreated {
		t.Fatalf("job token create = %d: %s", rec.Code, rec.Body.String())
	}
	*cur = deploymentJobToken("o/x", PermWrite)
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"main","environment":"production"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("other repo job token = %d", rec.Code)
	}
	*cur = deploymentJobToken("o/r", PermRead)
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments", ""); rec.Code != http.StatusOK {
		t.Fatalf("read token list = %d", rec.Code)
	}
}

func TestDeploymentQueueEdgesAndCancelWaiter(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	first, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	// Cancelling the queued waiter removes it; the holder is unaffected.
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", second.ID, DeploymentStatusInput{State: DeploymentStateInactive}, w); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued terminal = %v, want conflict", err)
	}
	if _, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "dep:"+second.ID); err != nil {
		t.Fatalf("release waiter: %v", err)
	}
	q, err := f.svc.loadEnvironmentQueue(ctx(), "o", "r", "production")
	if err != nil || q == nil || len(q.Entries) != 0 || q.Holder != "dep:"+first.ID {
		t.Fatalf("queue after waiter cancel = %+v, err %v", q, err)
	}
	// The one visible reason falls back to the token when no label was stored.
	if got := queueBusyReason("prod", &deploymentQueueDoc{Holder: "dep:x"}); !strings.Contains(got, "dep:x") {
		t.Fatalf("reason = %q", got)
	}
	if ValidDeploymentState("SUCCESS") != true || ValidDeploymentState("green") != false {
		t.Fatal("state validation")
	}
}

func TestDeploymentValidationEnvVariableArmsAndQueueFull(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()

	long := strings.Repeat("x", MaxDeploymentDescription+1)
	cases := []struct {
		name string
		in   DeploymentInput
	}{
		{"description too long", DeploymentInput{Ref: "main", Environment: "production", Description: long}},
		{"task too long", DeploymentInput{Ref: "main", Environment: "production", Task: strings.Repeat("t", MaxDeploymentTask+1)}},
		{"bad url", DeploymentInput{Ref: "main", Environment: "production", URL: "ftp://x"}},
		{"bad log url", DeploymentInput{Ref: "main", Environment: "production", LogURL: "not-a-url"}},
		{"bad sha", DeploymentInput{Ref: "main", Environment: "production", SHA: "xyz"}},
	}
	for _, tc := range cases {
		if _, err := f.svc.CreateDeployment(ctx(), "o/r", tc.in, w); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want invalid", tc.name, err)
		}
	}
	// No git backend + an explicit sha is accepted; an unresolvable ref with no
	// git backend stays empty rather than failing an otherwise valid request.
	saved := f.svc.Git
	f.svc.Git = nil
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production", SHA: strings.Repeat("a", 40)}, w)
	if err != nil {
		t.Fatalf("no-git create: %v", err)
	}
	if dep.SHA != strings.Repeat("a", 40) {
		t.Fatalf("sha = %q", dep.SHA)
	}
	// A full queue is a named conflict, never a silent drop.
	f.svc.Git = saved
	for i := 0; i < MaxEnvironmentQueue; i++ {
		if _, qerr := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w); qerr != nil {
			t.Fatalf("waiter %d: %v", i, qerr)
		}
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w); !errors.Is(err, ErrConflict) {
		t.Fatalf("queue-full create = %v, want conflict", err)
	}

	// No keyring: a variable seal/open fails closed; the resolver returns empty
	// only when secrets are disabled.
	noKey := &Service{keys: &Keyring{}}
	if _, err := noKey.sealEnvironmentVariable("o/r", "production", "X", "v"); err == nil {
		t.Fatal("seal without a key accepted")
	}
	if _, err := noKey.openEnvironmentVariable("o/r", "production", "X", "abc"); err == nil {
		t.Fatal("open without a key accepted")
	}
	if vals, err := noKey.environmentVariableValues(ctx(), "o", "r", "production"); err != nil || len(vals) != 0 {
		t.Fatalf("no-secrets environmentVariableValues = %v, %v", vals, err)
	}

	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)
	// Unknown environment and a corrupt stored value both fail closed.
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "nope", "X", "v", f.maintain()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown env set = %v", err)
	}
	if _, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "nope", f.maintain()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown env list = %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "CORRUPT", "v", f.maintain()); err != nil {
		t.Fatalf("set corrupt: %v", err)
	}
	badKey := EnvironmentVariableKey("o", "r", "production", "CORRUPT")
	if err := f.store.Delete(ctx(), badKey, ""); err != nil {
		t.Fatalf("delete good value: %v", err)
	}
	if _, err := store.PutBytes(ctx(), f.store, badKey, []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put corrupt value: %v", err)
	}
	if _, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "production", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt list = %v, want corrupt", err)
	}
	if _, err := f.svc.environmentVariableValues(ctx(), "o", "r", "production"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt values = %v, want corrupt", err)
	}

	// A running attempt on the non-force path only receives the signal.
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	job := mustJobs(t, e, run.ID)[0]
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobRunning}); err != nil {
		t.Fatalf("running: %v", err)
	}
	signalled, err := e.svc.CancelJob(ctx(), "o/r", run.ID, job.ID, false, e.writer())
	if err != nil {
		t.Fatalf("soft cancel: %v", err)
	}
	if signalled.State != JobRunning || !signalled.CancelRequested || signalled.CancelledBy != "writer" {
		t.Fatalf("soft cancel = %+v", signalled)
	}

	// Prune skips a corrupt record and a live one.
	if _, err := store.PutBytes(ctx(), f.store, DeploymentsPrefix("o", "r")+"bad.json", []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put corrupt deployment: %v", err)
	}
	if _, err := f.svc.pruneDeployments(ctx(), "o", "r"); err != nil {
		t.Fatalf("prune: %v", err)
	}
}

func TestDeploymentCorruptFaultsAndHTTPMethods(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()

	// A corrupt queue document is a named corruption on every entry point.
	if _, err := store.PutBytes(ctx(), f.store, EnvironmentQueueKey("o", "r", "production"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put corrupt queue: %v", err)
	}
	if _, _, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "t", "t", "", ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("acquire corrupt queue = %v", err)
	}
	if _, err := f.svc.loadEnvironmentQueue(ctx(), "o", "r", "production"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("load corrupt queue = %v", err)
	}
	if _, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "t"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("release corrupt queue = %v", err)
	}
	if err := f.store.Delete(ctx(), EnvironmentQueueKey("o", "r", "production"), ""); err != nil {
		t.Fatalf("delete corrupt queue: %v", err)
	}

	// A corrupt deployment record fails the status, get and list reads.
	badID := "d-bad"
	if _, err := store.PutBytes(ctx(), f.store, DeploymentKey("o", "r", badID), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put corrupt deployment: %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", badID, DeploymentStatusInput{State: DeploymentStateSuccess}, w); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("status corrupt = %v", err)
	}
	if _, err := f.svc.GetDeployment(ctx(), "o/r", badID, w); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("get corrupt = %v", err)
	}
	if _, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("list corrupt = %v", err)
	}

	// Store faults propagate from list and prune, never a nil answer.
	fs := newFaultStore()
	f.svc.Store = fs
	fs.fail("list", 0)
	if _, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 5); err == nil {
		t.Fatal("list with a failing store returned nil")
	}
	if _, err := f.svc.pruneDeployments(ctx(), "o", "r"); err == nil {
		t.Fatal("prune with a failing store returned nil")
	}
	f.svc.Store = f.store

	// Retention edges: a live record never expires, an unparseable timestamp
	// does not, and a zero window disables the rule.
	live := &DeploymentDoc{State: DeploymentStateInProgress, CreatedAt: "2000-01-01T00:00:00Z"}
	if f.svc.deploymentExpired(live, time.Now()) {
		t.Fatal("a live deployment expired")
	}
	badTime := &DeploymentDoc{State: DeploymentStateSuccess, CreatedAt: "yesterday"}
	if f.svc.deploymentExpired(badTime, time.Now()) {
		t.Fatal("an unparseable timestamp expired")
	}
	f.svc.KeepForever = true
	old := &DeploymentDoc{State: DeploymentStateSuccess, CreatedAt: "2000-01-01T00:00:00Z"}
	if f.svc.deploymentExpired(old, time.Now()) {
		t.Fatal("keep_forever expired a record")
	}
	f.svc.KeepForever = false

	// The status history is bounded: the newest entry always survives.
	statuses := []DeploymentStatus{}
	for i := 0; i < MaxDeploymentStatuses+5; i++ {
		statuses = appendDeploymentStatus(statuses, DeploymentStatus{State: DeploymentStateInProgress, Description: string(rune('a' + i%26))})
	}
	if len(statuses) != MaxDeploymentStatuses {
		t.Fatalf("history length = %d", len(statuses))
	}

	// CancelJob refusals: unknown job, read role, unknown run, and a job in a
	// run that already ended.
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	job := mustJobs(t, e, run.ID)[0]
	if _, err := e.svc.CancelJob(ctx(), "o/r", run.ID, "j-nope", false, e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}
	if _, err := e.svc.CancelJob(ctx(), "o/r", run.ID, job.ID, false, e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader cancel = %v", err)
	}
	if _, err := e.svc.CancelJob(ctx(), "o/r", "0000000000000000000-aaaaaa", job.ID, false, e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}

	// The HTTP dispatch answers each wrong method and an unknown subtree.
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint", Admin: true}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/deployments", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT deployments = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/x", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST one = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments/x/statuses", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET statuses = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments/x/other", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subtree = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/environments/x/variables/Y", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST variable = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/environments/x/variables", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown env variables = %d", rec.Code)
	}
}

func TestDeploymentAndVariableInputRefusals(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)

	if _, err := f.svc.CreateDeployment(ctx(), "not a repo", DeploymentInput{Ref: "main", Environment: "production"}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "bad name"}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad environment name = %v", err)
	}
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: strings.Repeat("r", 600), Environment: "production"}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long ref = %v", err)
	}
	if _, err := f.svc.ListDeployments(ctx(), "not a repo", "", w, "", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list bad repo = %v", err)
	}
	if _, err := f.svc.ListDeployments(ctx(), "o/r", "bad name", w, "", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list bad environment = %v", err)
	}
	if _, err := f.svc.GetDeployment(ctx(), "not a repo", "d", w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("get bad repo = %v", err)
	}
	if _, err := f.svc.GetDeployment(ctx(), "o/r", "missing", w); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}

	// An environment-variable record whose allowlist names a value that no
	// longer exists is "listed, absent", not an error (never a silent empty
	// value in a job).
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "GONE", "x", f.maintain()); err != nil {
		t.Fatalf("set GONE: %v", err)
	}
	if err := f.store.Delete(ctx(), EnvironmentVariableKey("o", "r", "production", "GONE"), ""); err != nil {
		t.Fatalf("delete GONE value: %v", err)
	}
	if vars, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "production", f.maintain()); err != nil || len(vars) != 0 {
		t.Fatalf("listed, absent = %v, %v", vars, err)
	}
	if vals, err := f.svc.environmentVariableValues(ctx(), "o", "r", "production"); err != nil || vals["GONE"] != "" {
		t.Fatalf("values listed, absent = %v, %v", vals, err)
	}

	// Variable input refusals and a corrupt allowlist record.
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "bad name", "x", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad variable name = %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "BIG", strings.Repeat("x", (64<<10)+1), f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize value = %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "bad name", "X", "x", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad environment name = %v", err)
	}
	if err := f.svc.DeleteEnvironmentVariable(ctx(), "o/r", "bad name", "X", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete bad environment = %v", err)
	}
	if err := f.svc.DeleteEnvironmentVariable(ctx(), "o/r", "production", "bad name", f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete bad name = %v", err)
	}
	if _, err := f.svc.casUpdate(ctx(), EnvironmentKey("o", "r", "production"), 1, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("{"), true, nil
	}); err != nil {
		t.Fatalf("corrupt environment record: %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "AFTER", "x", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("set with corrupt record = %v", err)
	}
	if err := f.svc.DeleteEnvironmentVariable(ctx(), "o/r", "production", "AFTER", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("delete with corrupt record = %v", err)
	}
	if _, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "production", f.maintain()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("list with corrupt record = %v", err)
	}
	if _, err := f.svc.environmentVariableValues(ctx(), "o", "r", "production"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("values with corrupt record = %v", err)
	}

	// HTTP: oversize n and a malformed body.
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint", Admin: true}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments?n=201", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("n=201 = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed create = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/x/statuses", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/x/variables/Y", `{"value":"v","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("variable unknown key = %d", rec.Code)
	}
}

func TestDeploymentHTTPAuthAndStoreFaultArms(t *testing.T) {
	// Every deployment/environment-variable handler answers a real 401 before
	// touching the service (the auth error branch is the boundary).
	_, h, _ := newHTTP(t)
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sign in"}
	}
	paths := []struct{ method, path, body string }{
		{http.MethodGet, "/o/r/api/threads/deployments", ""},
		{http.MethodPost, "/o/r/api/threads/deployments", `{"ref":"main","environment":"p"}`},
		{http.MethodGet, "/o/r/api/threads/deployments/d", ""},
		{http.MethodPost, "/o/r/api/threads/deployments/d/statuses", `{"state":"success"}`},
		{http.MethodGet, "/o/r/api/threads/environments/p/variables", ""},
		{http.MethodPut, "/o/r/api/threads/environments/p/variables/V", `{"value":"x"}`},
		{http.MethodDelete, "/o/r/api/threads/environments/p/variables/V", ""},
	}
	for _, tc := range paths {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	// Store faults on the read paths return the error, never a nil record.
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fs := newFaultStore()
	// Move the seeded objects into the fault store.
	fs.Memory = f.store
	f.svc.Store = fs
	fs.fail("get", 0)
	if _, err := f.svc.GetDeployment(ctx(), "o/r", dep.ID, w); err == nil {
		t.Fatal("get with a failing store returned nil")
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess}, w); err == nil {
		t.Fatal("status with a failing store returned nil")
	}
	if _, _, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "t", "t", "", ""); err == nil {
		t.Fatal("acquire with a failing store returned nil")
	}
	if _, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "dep:"+dep.ID); err == nil {
		t.Fatal("release with a failing store returned nil")
	}
	fs.clear()
	f.svc.Store = f.store

	// Promotion of a vanished job and a corrupt API record is best effort.
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "job:gone:gone", RunID: "gone", JobID: "gone"})
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "dep:d-missing"})
}

func TestDeploymentHelperBranches(t *testing.T) {
	if got := deploymentRecordState(nil, nil); got != "" {
		t.Fatalf("nil job state = %q", got)
	}
	if got := deploymentRecordState(&Job{Deployment: &Deployment{QueuePosition: 2}}, &Run{}); got != DeploymentStateQueued {
		t.Fatalf("queued = %q", got)
	}
	if got := deploymentRecordState(&Job{State: JobCancelled, Deployment: &Deployment{}}, &Run{}); got != DeploymentStateInactive {
		t.Fatalf("cancelled = %q", got)
	}
	if got := deploymentRecordState(&Job{State: JobSuccess, Deployment: &Deployment{}}, &Run{}); got != DeploymentStateSuccess {
		t.Fatalf("success = %q", got)
	}
	if got := deploymentRecordState(&Job{Deployment: &Deployment{State: DeploymentApproved}}, &Run{}); got != DeploymentStateInProgress {
		t.Fatalf("approved = %q", got)
	}
	if got := queueBusyReason("prod", &deploymentQueueDoc{}); !strings.Contains(got, "another deployment") {
		t.Fatalf("empty holder reason = %q", got)
	}

	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	// No queue object: release is a no-op, a stale waiter is removed, and an
	// empty holder clears cleanly.
	if promoted, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "t"); err != nil || promoted != nil {
		t.Fatalf("release with no queue = %+v, %v", promoted, err)
	}
	pos, reason, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "holder", "holder label", "", "")
	if err != nil || pos != 0 || reason != "" {
		t.Fatalf("acquire holder = %d %q %v", pos, reason, err)
	}
	// Idempotent holder.
	if pos, _, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "holder", "holder label", "", ""); err != nil || pos != 0 {
		t.Fatalf("idempotent holder = %d %v", pos, err)
	}
	// Idempotent waiter.
	if pos, _, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "waiter", "waiter label", "", ""); err != nil || pos != 1 {
		t.Fatalf("waiter = %d %v", pos, err)
	}
	if pos, reason, err := f.svc.acquireEnvironmentSlot(ctx(), "o", "r", "production", "waiter", "waiter label", "", ""); err != nil || pos != 1 || reason == "" {
		t.Fatalf("idempotent waiter = %d %q %v", pos, reason, err)
	}
	// Releasing an unknown token is a no-op (the holder and waiter survive).
	if promoted, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "ghost"); err != nil || promoted != nil {
		t.Fatalf("unknown release = %+v, %v", promoted, err)
	}
	// Releasing the holder with no waiters clears it.
	if _, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "holder"); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	// The waiter is now the holder; releasing it clears the queue.
	if _, err := f.svc.releaseEnvironmentSlot(ctx(), "o", "r", "production", "waiter"); err != nil {
		t.Fatalf("release promoted: %v", err)
	}
	q, err := f.svc.loadEnvironmentQueue(ctx(), "o", "r", "production")
	if err != nil || q == nil || q.Holder != "" {
		t.Fatalf("queue after drains = %+v, %v", q, err)
	}

	// Promote with a vanished job and a vanished API record is a no-op.
	f.svc.promoteDeployment(ctx(), "o", "r", "production", nil)
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "job:r:j", RunID: "r", JobID: "j"})

	// Refusal arms not yet exercised: bad repo on update/get/cancel, bad status
	// description and URLs.
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "not a repo", "d", DeploymentStatusInput{State: DeploymentStateSuccess}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update bad repo = %v", err)
	}
	dep, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	long := strings.Repeat("x", MaxDeploymentDescription+1)
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess, Description: long}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long status description = %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess, EnvironmentURL: "nope"}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status url = %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", dep.ID, DeploymentStatusInput{State: DeploymentStateSuccess, LogURL: "nope"}, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status log url = %v", err)
	}
	if _, err := f.svc.UpdateDeploymentStatus(ctx(), "o/r", "missing", DeploymentStatusInput{State: DeploymentStateSuccess}, w); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing status = %v", err)
	}
	if _, err := f.svc.CancelJob(ctx(), "not a repo", "r", "j", false, w); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cancel bad repo = %v", err)
	}

	// A corrupt job deployment record is ignored by the best-effort sync.
	e := newEnvFixture(t)
	e.putEnvironment(t, "production")
	run := e.deployRun(t, CreateOpts{})
	ship := envJob(t, e.testEnv, run.ID)
	if err := e.store.Delete(ctx(), DeploymentKey("o", "r", ship.Deployment.DeploymentID), ""); err != nil {
		t.Fatalf("delete job deployment: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, DeploymentKey("o", "r", ship.Deployment.DeploymentID), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("corrupt job deployment: %v", err)
	}
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, ship.ID, JobUpdate{State: JobFailure}); err != nil {
		t.Fatalf("update with corrupt deployment record: %v", err)
	}

	// A corrupt queue on promotion's read is a no-op, never a panic.
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "dep:" + dep.ID})
}

func TestClaimPayloadMergesEnvironmentVariables(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)
	admin := auth.Principal{Name: "maint", Admin: true}
	if err := f.svc.SetVariable(ctx(), "o/r", "REGION", "repo-value", admin); err != nil {
		t.Fatalf("repo variable: %v", err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "REGION", "env-value", admin); err != nil {
		t.Fatalf("environment variable: %v", err)
	}
	run := f.deployRun(t, CreateOpts{})
	// build has no environment; finish it so ship is next.
	jobID := f.claimOne(t, run.ID)
	if jobID == "" {
		t.Fatal("no job claimed")
	}
	if jobID != "j-build" {
		t.Fatalf("first claim = %s", jobID)
	}
	f.finish(t, run.ID, jobID, JobSuccess)
	shipID := f.claimOne(t, run.ID)
	if shipID != "j-ship" {
		t.Fatalf("second claim = %s", shipID)
	}
	claim, err := f.svc.claimLoop(ctx(), "o", "r", needsRunner(), []string{"linux"})
	if err != nil {
		t.Fatalf("claimLoop: %v", err)
	}
	_ = claim // ship was claimed above; fetchPayload is exercised below
	job, _, err := f.svc.loadJobAndRun(ctx(), "o", "r", run.ID, shipID)
	if err != nil || job == nil {
		t.Fatalf("load ship: %v", err)
	}
	payload, err := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: needsRunner()}, &Claim{
		Entry: &QueueEntry{RunID: run.ID, JobID: shipID, Attempt: job.Attempt},
		Job:   job, Run: run,
	}, job, "")
	if err != nil {
		t.Fatalf("fetchPayload: %v", err)
	}
	if payload.Vars["REGION"] != "env-value" {
		t.Fatalf("vars = %v (environment must win)", payload.Vars)
	}
	found := false
	for _, mask := range payload.Mask {
		if mask == "env-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("environment variable value is not masked: %v", payload.Mask)
	}
}

func TestDeploymentStoreWriteFaultsAndVariableEnvelopes(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()

	// A failing env read fails closed before any queue write.
	fs := newFaultStore()
	fs.Memory = f.store
	f.svc.Store = fs
	fs.fail("get", 0)
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w); err == nil {
		t.Fatal("create with a failing env read returned nil")
	}
	// Let the env read + the queue's first put through, then fail the record
	// put: the request fails rather than reporting a deployment that is not
	// stored.
	fs.clear()
	fs.fail("put", 1)
	if _, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w); err == nil {
		t.Fatal("create with a failing record put returned nil")
	}
	fs.clear()
	f.svc.Store = f.store

	// A deployments: read token with no deployments scope is refused by name.
	none := auth.Principal{Name: "ci:r1", JobRepo: "o/r", JobPerms: map[string]string{"contents": "read"}}
	if _, err := f.svc.ListDeployments(ctx(), "o/r", "", none, "", 5); !errors.Is(err, ErrForbidden) {
		t.Fatalf("token without deployments:read = %v", err)
	}

	// The sealed variable envelope rejects a bad base64, a short body and a
	// wrong AAD.
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)
	if _, err := f.svc.openEnvironmentVariable("o/r", "production", "X", "!!!"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad base64 = %v", err)
	}
	if _, err := f.svc.openEnvironmentVariable("o/r", "production", "X", "YQ=="); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short envelope = %v", err)
	}
	sealed, err := f.svc.sealEnvironmentVariable("o/r", "production", "X", "v")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := f.svc.openEnvironmentVariable("o/r", "staging", "X", sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong AAD = %v", err)
	}

	// The job sync derives its id when a record predates the field.
	f.putEnvironment(t, "staging")
	depDoc := &Deployment{Environment: "staging", State: DeploymentNotRequired}
	job := &Job{ID: "j-x", RunID: "r-x", Name: "x", State: JobQueued, Deployment: depDoc}
	run := &Run{ID: "r-x", Repo: "o/r", Actor: "writer", Ref: "refs/heads/main", Commit: testSHA}
	f.svc.syncJobDeployment(ctx(), "o", "r", job, run)
	if _, err := f.svc.GetDeployment(ctx(), "o/r", jobDeploymentID("r-x", "j-x"), w); err != nil {
		t.Fatalf("derived id sync: %v", err)
	}
	// A nil/undeployed job is a no-op.
	f.svc.syncJobDeployment(ctx(), "o", "r", &Job{}, run)
	f.svc.syncJobDeployment(ctx(), "o", "r", nil, nil)

	// A list over two environments reports only the requested one, newest
	// first, and an empty environment filter returns both.
	staging, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "staging"}, w)
	if err != nil {
		t.Fatalf("staging deployment: %v", err)
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "staging", w, "", 50); err != nil || len(page.Deployments) < 1 {
		t.Fatalf("staging page = %+v, %v", page, err)
	} else {
		found := false
		for _, d := range page.Deployments {
			if d.Environment != "staging" {
				t.Fatalf("filter leaked %q", d.Environment)
			}
			if d.ID == staging.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("staging deployment missing from its page")
		}
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", w, staging.ID, 50); err != nil || len(page.Deployments) != 0 {
		t.Fatalf("after cursor page = %+v, %v", page, err)
	}
}

func TestEnvironmentVariableHTTPErrorArms(t *testing.T) {
	e, h, cur := newHTTP(t)
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	e.svc.SetKeyring(key)
	*cur = auth.Principal{Name: "maint", Admin: true}
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, *cur); err != nil {
		t.Fatalf("put environment: %v", err)
	}
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/o/r/api/threads/environments/production/variables", "", http.StatusMethodNotAllowed},
		{http.MethodPut, "/o/r/api/threads/environments/nope/variables/V", `{"value":"x"}`, http.StatusNotFound},
		{http.MethodPut, "/o/r/api/threads/environments/production/variables/V", `{`, http.StatusBadRequest},
		{http.MethodPut, "/o/r/api/threads/environments/production/variables/bad%20name", `{"value":"x"}`, http.StatusBadRequest},
		{http.MethodDelete, "/o/r/api/threads/environments/production/variables/bad%20name", "", http.StatusBadRequest},
		{http.MethodGet, "/o/r/api/threads/environments/nope/variables", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	// A reader is refused the write surface, and can still list.
	*cur = auth.Principal{Name: "reader"}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production/variables/V", `{"value":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("reader put = %d", rec.Code)
	}
}

func TestDeploymentPagingAndPruneEdges(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	var dep *DeploymentDoc
	for i := 0; i < 3; i++ {
		created, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		dep = created
	}
	// n clamps to the defaults on both ends; a one-row page reports More.
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 0); err != nil || len(page.Deployments) != 3 {
		t.Fatalf("n=0 = %+v, %v", page, err)
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 999); err != nil || len(page.Deployments) != 3 {
		t.Fatalf("n=999 = %+v, %v", page, err)
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", w, "", 1); err != nil || len(page.Deployments) != 1 || !page.More {
		t.Fatalf("n=1 = %+v, %v", page, err)
	}
	// A token without deployments:read cannot get a record.
	none := auth.Principal{Name: "ci:r1", JobRepo: "o/r", JobPerms: map[string]string{"contents": "read"}}
	if _, err := f.svc.GetDeployment(ctx(), "o/r", dep.ID, deploymentJobToken("o/r", PermWrite)); err != nil {
		t.Fatalf("write token get = %v", err)
	}
	if _, err := f.svc.GetDeployment(ctx(), "o/r", "d", none); !errors.Is(err, ErrForbidden) {
		t.Fatalf("token get = %v", err)
	}
	// The URL cap is named.
	if err := validDeploymentURL("https://x/" + strings.Repeat("a", MaxDeploymentURL)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long URL = %v", err)
	}

	// A corrupt queue read is a store error, not a silent free slot.
	fs := newFaultStore()
	fs.Memory = f.store
	f.svc.Store = fs
	fs.fail("get", 0)
	if _, err := f.svc.loadEnvironmentQueue(ctx(), "o", "r", "production"); err == nil {
		t.Fatal("load queue with a failing store returned nil")
	}
	fs.clear()

	// keep_forever disables pruning entirely.
	f.svc.KeepForever = true
	if n, err := f.svc.pruneDeployments(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("keep_forever prune = %d, %v", n, err)
	}
	f.svc.KeepForever = false

	// A delete fault during prune surfaces (never a partial silent pass).
	oldDoc := &DeploymentDoc{ID: "d-old", State: DeploymentStateSuccess, Environment: "production",
		CreatedAt: "2000-01-01T00:00:00Z", Statuses: []DeploymentStatus{}}
	if _, err := store.PutBytes(ctx(), f.store, DeploymentKey("o", "r", "d-old"), mustJSON(oldDoc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put old: %v", err)
	}
	fs.clear()
	f.svc.Store = fs
	fs.fail("delete", 0)
	if _, err := f.svc.pruneDeployments(ctx(), "o", "r"); err == nil {
		t.Fatal("prune delete fault returned nil")
	}
	fs.clear()
}

func TestDeploymentMiscCoverageArms(t *testing.T) {
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint", Admin: true}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments?n=abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("n=abc = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments?n=201", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("n=201 = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/deployments/x/statuses", `{"state":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown deployment bad state = %d", rec.Code)
	}

	// A promoted record that was already pending is not rewritten as pending
	// twice (the state check is the branch).
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	doc := &DeploymentDoc{ID: "d-manual", Environment: "production", State: DeploymentStatePending,
		QueuePosition: 1, CreatedAt: "2026-11-20T00:00:00Z", Statuses: []DeploymentStatus{}}
	key := DeploymentKey("o", "r", "d-manual")
	if _, err := store.PutBytes(ctx(), f.store, key, mustJSON(doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put manual: %v", err)
	}
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "dep:d-manual"})
	promoted, err := f.svc.loadDeployment(ctx(), "o", "r", "d-manual")
	if err != nil || promoted.QueuePosition != 0 {
		t.Fatalf("promote pending = %+v, %v", promoted, err)
	}

	// CancelJob surfaces store faults on the read and the write.
	e2 := newTestEnv(t)
	run := e2.mustCreate(t)[0]
	job := mustJobs(t, e2, run.ID)[0]
	fs := newFaultStore()
	fs.Memory = e2.store
	e2.svc.Store = fs
	fs.fail("get", 0)
	if _, err := e2.svc.CancelJob(ctx(), "o/r", run.ID, job.ID, false, e2.writer()); err == nil {
		t.Fatal("cancel with a failing read returned nil")
	}
	fs.clear()
	e2.svc.Store = e2.store
	fs.Memory = e2.store
	e2.svc.Store = fs
	fs.fail("put", 0)
	if _, err := e2.svc.CancelJob(ctx(), "o/r", run.ID, job.ID, false, e2.writer()); err == nil {
		t.Fatal("cancel with a failing write returned nil")
	}
	fs.clear()
	e2.svc.Store = e2.store
}

// phantomListStore adds one listed key that a Get cannot resolve, modelling a
// record deleted between the LIST and the GET (the retention/GC race).
type phantomListStore struct {
	*store.Memory
	phantom string
}

func (p *phantomListStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if err := p.Memory.List(ctx, prefix, after, fn); err != nil {
		return err
	}
	return fn(store.ObjectMeta{Key: p.phantom})
}

func TestDeploymentRetentionRaceAndNilGuards(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	phantom := &phantomListStore{Memory: f.store, phantom: DeploymentsPrefix("o", "r") + "gone.json"}
	f.svc.Store = phantom
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", f.writer(), "", 5); err != nil || len(page.Deployments) != 0 {
		t.Fatalf("phantom list = %+v, %v", page, err)
	}
	if n, err := f.svc.pruneDeployments(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("phantom prune = %d, %v", n, err)
	}
	f.svc.Store = f.store

	// Nil-receiver and nil-document guards.
	var nilSvc *Service
	if n, err := nilSvc.pruneDeployments(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("nil prune = %d, %v", n, err)
	}
	if f.svc.deploymentExpired(nil, time.Now()) {
		t.Fatal("nil deployment expired")
	}
	if got := deploymentRecordState(&Job{}, &Run{}); got != "" {
		t.Fatalf("nil deployment record state = %q", got)
	}
	var nilSync *Service
	nilSync.syncJobDeployment(ctx(), "o", "r", &Job{}, &Run{})
	emptyStore := &Service{}
	if n, err := emptyStore.pruneDeployments(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("nil store prune = %d, %v", n, err)
	}
	if got := deploymentRecordState(&Job{State: JobQueued, Deployment: &Deployment{State: DeploymentNotRequired}}, &Run{}); got != DeploymentStatePending {
		t.Fatalf("default record state = %q", got)
	}
	if up := jobCancelUpdate("", CancelReasonOperator, false); up.State != JobCancelled || up.Reason != "" {
		t.Fatalf("system cancel update = %+v", up)
	}
	if err := validDeploymentURL(""); err != nil {
		t.Fatalf("empty url = %v", err)
	}
}

// onceGetStore serves one prepared body for the first GET of key and another
// on every later GET — the interleaving a real promotion CAS sees when the
// record vanishes or corrupts between the load and the CAS.
type onceGetStore struct {
	*store.Memory
	key   string
	first []byte
	next  []byte
	calls int
}

func (o *onceGetStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == o.key {
		o.calls++
		body := o.next
		if o.calls == 1 {
			body = o.first
		}
		if body == nil {
			return nil, store.NewNotFound(key)
		}
		return store.Object{Meta: store.ObjectMeta{Key: key, Version: "v1"}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
	return o.Memory.Get(ctx, key, opts)
}

func TestPromotionRaceBranches(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	doc := &DeploymentDoc{ID: "d-race", Environment: "production", State: DeploymentStateQueued,
		QueuePosition: 1, CreatedAt: "2026-11-20T00:00:00Z", Statuses: []DeploymentStatus{}}
	key := DeploymentKey("o", "r", "d-race")
	if _, err := store.PutBytes(ctx(), f.store, key, mustJSON(doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// The record vanishes between the load and the CAS.
	gone := &onceGetStore{Memory: f.store, key: key, first: mustJSON(doc), next: nil}
	f.svc.Store = gone
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "dep:d-race"})
	// The record corrupts between the load and the CAS.
	corrupt := &onceGetStore{Memory: f.store, key: key, first: mustJSON(doc), next: []byte("{")}
	f.svc.Store = corrupt
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "dep:d-race"})
	// A token that is neither a job nor a dep is ignored.
	f.svc.Store = f.store
	f.svc.promoteDeployment(ctx(), "o", "r", "production", &deploymentQueueEntry{Token: "weird"})
}

func TestCancelUpdateEmptyActor(t *testing.T) {
	if up := jobCancelUpdate("", CancelReasonForce, true); up.State != JobCancelled || up.ForceCancelled == nil || !*up.ForceCancelled {
		t.Fatalf("empty-actor force cancel = %+v", up)
	}
	if got := appendDeploymentStatus(nil, DeploymentStatus{State: DeploymentStatePending}); len(got) != 1 {
		t.Fatalf("append to nil = %+v", got)
	}
}

func TestDeploymentListSkipsNestedKeysAndHTTPFault(t *testing.T) {
	f := newEnvFixture(t)
	// A nested object under the deployment prefix is not a deployment record.
	if _, err := store.PutBytes(ctx(), f.store, DeploymentsPrefix("o", "r")+"nested/x.json", []byte("{}"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put nested: %v", err)
	}
	if page, err := f.svc.ListDeployments(ctx(), "o/r", "", f.writer(), "", 5); err != nil || len(page.Deployments) != 0 {
		t.Fatalf("nested list = %+v, %v", page, err)
	}

	// The HTTP list reports a store failure as a plain error, never a 200 with
	// an empty page.
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint", Admin: true}
	fs := newFaultStore()
	fs.Memory = e.store
	e.svc.Store = fs
	fs.fail("list", 0)
	if rec := do(h, http.MethodGet, "/o/r/api/threads/deployments", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list store fault = %d", rec.Code)
	}
	fs.clear()
}

func TestDeleteEnvironmentFailsQueuedDeployments(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	w := f.writer()
	first, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := f.svc.CreateDeployment(ctx(), "o/r", DeploymentInput{Ref: "main", Environment: "production"}, w)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatalf("delete environment: %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		doc, derr := f.svc.loadDeployment(ctx(), "o", "r", id)
		if derr != nil || doc == nil {
			t.Fatalf("deployment %s: %v", id, derr)
		}
		if doc.State != DeploymentStateFailure || doc.QueuePosition != 0 {
			t.Fatalf("deployment after delete = %+v", doc)
		}
	}
	if q, err := f.svc.loadEnvironmentQueue(ctx(), "o", "r", "production"); err != nil || q != nil {
		t.Fatalf("queue after delete = %+v, %v", q, err)
	}
}

func TestDeleteEnvironmentFailsQueuedJobDeployments(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	create := func() *Run {
		runs, err := f.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{
			Branch: "main", Ref: "refs/heads/main", Actor: "writer", Workflow: "deploy.yml",
		})
		if err != nil || len(runs) != 1 {
			t.Fatalf("CreateRun = %d, %v", len(runs), err)
		}
		return runs[0]
	}
	first, second := create(), create()
	firstShip, secondShip := envJob(t, f.testEnv, first.ID), envJob(t, f.testEnv, second.ID)
	if firstShip.Deployment.QueuePosition != 0 || secondShip.Deployment.QueuePosition != 1 {
		t.Fatalf("queue positions = %d, %d", firstShip.Deployment.QueuePosition, secondShip.Deployment.QueuePosition)
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, tc := range []struct{ runID, jobID string }{{first.ID, firstShip.ID}, {second.ID, secondShip.ID}} {
		job, _, err := f.svc.loadJobAndRun(ctx(), "o", "r", tc.runID, tc.jobID)
		if err != nil || job == nil {
			t.Fatalf("load %s: %v", tc.jobID, err)
		}
		if job.State != JobFailure || !strings.Contains(job.Reason, "was deleted") {
			t.Fatalf("job after delete = %+v", job)
		}
	}
}

func TestEnvironmentVariableIdempotenceAndQueueFailures(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnvironment(t, "production")
	key, kerr := NewKeyring(strings.Repeat("v", 32), "")
	if kerr != nil {
		t.Fatal(kerr)
	}
	f.svc.SetKeyring(key)
	admin := auth.Principal{Name: "maint", Admin: true}
	// A second identical name is idempotent on the allowlist.
	for i := 0; i < 2; i++ {
		if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "ONCE", "v", admin); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if vars, err := f.svc.ListEnvironmentVariables(ctx(), "o/r", "production", admin); err != nil || len(vars) != 1 {
		t.Fatalf("idempotent vars = %+v, %v", vars, err)
	}
	// A keyring-less service refuses to store a variable (fail closed).
	noKey := &Service{Store: f.store, Roles: f.roles, Now: f.svc.Now, keys: &Keyring{}}
	if err := noKey.SetEnvironmentVariable(ctx(), "o/r", "production", "NOPE", "v", admin); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no-key set = %v", err)
	}
	// Adding a name to a vanished record is a named not-found.
	if err := f.svc.addEnvironmentVariableName(ctx(), "o", "r", "missing", "X", "now"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("add to missing env = %v", err)
	}

	// The queue cleanup tolerates malformed tokens and skips a running job.
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	job := mustJobs(t, e, run.ID)[0]
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobRunning}); err != nil {
		t.Fatalf("running: %v", err)
	}
	queue := &deploymentQueueDoc{
		Holder: "weird",
		Entries: []deploymentQueueEntry{
			{Token: "job:missing-separator"},
			{Token: "dep:missing"},
			{Token: "job:" + run.ID + ":" + job.ID},
			{Token: "other"},
		},
	}
	if _, err := e.svc.casUpdate(ctx(), EnvironmentQueueKey("o", "r", "gone"), 1, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(queue), true, nil
	}); err != nil {
		t.Fatalf("put queue: %v", err)
	}
	e.svc.failEnvironmentQueue(ctx(), "o", "r", "gone")
}
