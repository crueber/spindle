package webui

// threads_wave4_test.go — PAR-132/134/193/194/195 rendered surfaces: the
// environment tabs (deployments/variables), the run-list filters, the workflow
// badge and the per-job cancel control.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestPullPanelOffersThreadsRerun(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada"}
	d := pullDeps()
	d.ThreadsLatest = func(context.Context, string, string, string, auth.Principal) (*threads.Run, error) {
		return &threads.Run{ID: "run-abc", State: threads.RunSuccess}, nil
	}
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleWrite }
	f.deps(d)
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{
		"/acme/widgets/threads/run-abc", "re-run checks", `action="/acme/widgets/threads/run-abc/rerun"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR panel misses %q", want)
		}
	}
	// A read principal is offered the link but not the verb.
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead }
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "re-run checks") {
		t.Fatal("a read principal was offered the re-run verb")
	}
}

func TestEnvironmentTabsRenderDeploymentsAndVariables(t *testing.T) {
	fake := newFakeEnvThreads()
	fake.envs = []threads.EnvironmentView{{
		Name: "production", RequiredReviewers: []string{"ada"}, Secrets: []string{"TOKEN"},
		Vars: []string{"REGION"}, UpdatedAt: "2026-11-23T00:00:00Z",
	}}
	fake.secrets["production"] = []threads.SecretView{{Name: "TOKEN", UpdatedAt: "2026-11-23T00:00:00Z"}}
	fake.vars["production"] = []threads.VariableView{{Name: "REGION", UpdatedAt: "2026-11-23T00:00:00Z"}}
	fake.deployments["production"] = []*threads.DeploymentDoc{{
		ID: "d-1", Environment: "production", Ref: "refs/heads/main", State: threads.DeploymentStateSuccess,
		Creator: "ada", CreatedAt: "2026-11-23T00:00:00Z", UpdatedAt: "2026-11-23T00:00:00Z",
		Statuses: []threads.DeploymentStatus{
			{State: threads.DeploymentStatePending, Creator: "ada", CreatedAt: "2026-11-23T00:00:00Z"},
			{State: threads.DeploymentStateSuccess, Creator: "ada", CreatedAt: "2026-11-23T01:00:00Z"},
		},
	}}
	s := envSettingsServer(t, "maintain", fake)

	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/environments", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET environments = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		">Protection rules", ">Deployments", ">Secrets", ">Variables",
		"name=\"environment_variable_name\"", "name=\"environment_variable_value\"",
		"value=\"variable-put\"", "value=\"variable-delete\"",
		"REGION", "refs/heads/main", "2026-11-23T00:00:00Z",
	} {
		settingsMustContain(t, body, want, "environment tabs")
	}
	// The deployment history is text-labelled (state chip + history line), never
	// colour-only.
	settingsMustContain(t, body, "history:", "deployment history is labelled")
	settingsMustContain(t, body, "pending", "history carries every state")
	settingsMustContain(t, body, "success", "history carries every state")

	// The variable write reaches the same maintain-floor seam.
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments/production",
		url.Values{"environment_action": {"variable-put"}, "environment_variable_name": {"MODE"}, "environment_variable_value": {"blue"}}, nil)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("variable put = %d: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, call := range fake.calls {
		if call == "variable-put:production:MODE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("variable write did not reach the service: %v", fake.calls)
	}
}

func TestThreadsListFiltersBranchAndEvent(t *testing.T) {
	bindChromeProjections(t)
	var got threads.RunFilter
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, f threads.RunFilter) (*threads.RunPage, error) {
			got = f
			return &threads.RunPage{Runs: []*threads.Run{}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads?branch=main&event=push&status=running&actor=ada")
	wantStatus(t, rec, http.StatusOK)
	if got.Branch != "main" || got.Event != "push" || got.Actor != "ada" {
		t.Fatalf("filter = %+v", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="branch"`, `value="main"`,
		`name="event"`, `name="actor"`, `value="ada"`,
		`aria-label="filter runs"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("filter form misses %q", want)
		}
	}
}

func TestThreadsBadgeReflectsTheLatestRun(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, f threads.RunFilter) (*threads.RunPage, error) {
			if f.Workflow != "CI" {
				t.Fatalf("badge filter = %+v", f)
			}
			return &threads.RunPage{Runs: []*threads.Run{{ID: "r1", State: threads.RunSuccess}}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads/badge.svg?workflow=CI&branch=main")
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
		t.Fatalf("content-type = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"passing", "main", "<svg", "<title>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("badge misses %q: %s", want, body)
		}
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Fatal("badge has no cache policy")
	}
}

func TestThreadJobCancelControl(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	job := threadsFixtureJob(run)
	job.State = threads.JobRunning
	var gotForce bool
	var gotJob string
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		CancelJob: func(_ context.Context, _, _, _, jobID string, force bool, _ auth.Principal) error {
			gotJob, gotForce = jobID, force
			return nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{
		"/acme/widgets/threads/" + run.ID + "/jobs/" + job.ID + "/cancel",
		"cancel job", "force cancel", `name="force" value="on"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("job row misses %q", want)
		}
	}
	rec = f.post("/acme/widgets/threads/"+run.ID+"/jobs/"+job.ID+"/cancel", url.Values{"force": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("job cancel = %d: %s", rec.Code, rec.Body.String())
	}
	if gotJob != job.ID || !gotForce {
		t.Fatalf("cancel reached service with job=%q force=%v", gotJob, gotForce)
	}
}

func TestThreadsBadgeStateVocabulary(t *testing.T) {
	bindChromeProjections(t)
	states := []struct {
		run  string
		want string
	}{
		{threads.RunFailure, "failing"},
		{threads.RunCancelled, "cancelled"},
		{threads.RunSkipped, "skipped"},
		{threads.RunRunning, "running"},
		{threads.RunPendingApproval, "running"},
		{threads.RunQueued, "queued"},
		{"", "queued"},
	}
	for _, tc := range states {
		state := tc.run
		BindThreads(ThreadsDeps{
			List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, _ threads.RunFilter) (*threads.RunPage, error) {
				return &threads.RunPage{Runs: []*threads.Run{{ID: "r1", State: state}}}, nil
			},
		})
		f := newCollabFixture(t)
		rec := f.get("/acme/widgets/threads/badge.svg?workflow=CI")
		wantStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("badge for %s misses %q: %s", tc.run, tc.want, rec.Body.String())
		}
	}
	BindThreads(ThreadsDeps{})
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads/badge.svg")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("badge without workflow = %d", rec.Code)
	}
	// No run at all is the "no runs" badge, not an error.
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f = newCollabFixture(t)
	rec = f.get("/acme/widgets/threads/badge.svg?workflow=CI")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "no runs") {
		t.Fatalf("empty badge = %s", rec.Body.String())
	}
}

func TestThreadsBadgeListFailure(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return nil, threads.ErrUnavailable
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads/badge.svg?workflow=CI")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("badge failure = %d", rec.Code)
	}
}

func TestEnvironmentVariableWritesWhenUnwired(t *testing.T) {
	var services SettingsServices
	if _, werr := services.putEnvironmentVariable(context.Background(), "acme", "widgets", auth.Anonymous(), SettingsRepoWrite{Values: map[string]string{}}); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unwired variable put = %+v", werr)
	}
	if _, werr := services.deleteEnvironmentVariable(context.Background(), "acme", "widgets", auth.Anonymous(), SettingsRepoWrite{Values: map[string]string{}}); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unwired variable delete = %+v", werr)
	}
}
