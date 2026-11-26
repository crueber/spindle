package webui

// environments_webui_test.go — Settings → Environments and the run page's
// deployment surface (docs/PLAN-THREADS-ENVIRONMENTS.md §7). New file, so the
// parallel settings-test rewrite and this tab do not touch one another.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// fakeEnvThreads is a self-contained ThreadsSettings that also carries the
// environment surface (the concrete *threads.Service does both; this fake proves
// the type-assertion binding). It deliberately depends on no other test file.
type fakeEnvThreads struct {
	envs    []threads.EnvironmentView
	secrets map[string][]threads.SecretView
	calls   []string
	enabled bool
	// last is the input of the most recent PutEnvironment (the protection
	// rules' write path, PAR-131).
	last threads.EnvironmentInput
}

func newFakeEnvThreads() *fakeEnvThreads {
	return &fakeEnvThreads{
		secrets: map[string][]threads.SecretView{},
		enabled: true,
	}
}

// The repository-secret half of ThreadsSettings (unused by these tests).
func (f *fakeEnvThreads) ListSecrets(context.Context, string, auth.Principal) ([]threads.SecretView, error) {
	return nil, nil
}

func (f *fakeEnvThreads) SetSecret(context.Context, string, string, string, auth.Principal) error {
	return nil
}

func (f *fakeEnvThreads) DeleteSecret(context.Context, string, string, auth.Principal) error {
	return nil
}

func (f *fakeEnvThreads) ListEnvironments(context.Context, string, auth.Principal) ([]threads.EnvironmentView, error) {
	return f.envs, nil
}

func (f *fakeEnvThreads) PutEnvironment(_ context.Context, _ string, name string, in threads.EnvironmentInput, _ auth.Principal) (*threads.EnvironmentView, error) {
	f.calls = append(f.calls, "put:"+name+":"+strings.Join(in.RequiredReviewers, ","))
	f.last = in
	return &threads.EnvironmentView{
		Name: name, RequiredReviewers: in.RequiredReviewers, TeamReviewers: in.TeamReviewers,
		WaitTimerSeconds: in.WaitTimerSeconds, PreventSelfReview: in.PreventSelfReview,
		DeploymentBranches: in.DeploymentBranches,
	}, nil
}

func (f *fakeEnvThreads) DeleteEnvironment(_ context.Context, _ string, name string, _ auth.Principal) error {
	f.calls = append(f.calls, "delete:"+name)
	return nil
}

func (f *fakeEnvThreads) ListEnvironmentSecrets(_ context.Context, _ string, env string, _ auth.Principal) ([]threads.SecretView, error) {
	return f.secrets[env], nil
}

func (f *fakeEnvThreads) SetEnvironmentSecret(_ context.Context, _ string, env, name, _ string, _ auth.Principal) error {
	f.calls = append(f.calls, "secret-put:"+env+":"+name)
	return nil
}

func (f *fakeEnvThreads) DeleteEnvironmentSecret(_ context.Context, _ string, env, name string, _ auth.Principal) error {
	f.calls = append(f.calls, "secret-delete:"+env+":"+name)
	return nil
}

func (f *fakeEnvThreads) SecretsEnabled() bool { return f.enabled }

// envSettingsServer wires the settings group over the fake environment service.
func envSettingsServer(t *testing.T, role string, fake *fakeEnvThreads) *Server {
	t.Helper()
	f := settingsRepoFakes(role)
	// A non-nil API is what makes the seam wire its write path; the
	// environment actions never read it.
	services := SettingsServices{Threads: fake, API: &api.Env{}}
	f.deps.SettingsDeps.BindSettingsServices(services)
	// Keep the fixture's own role/general answers over the seam's nil ones.
	f.deps.SettingsDeps.RepoRole = f.deps.RepoRole
	f.deps.SettingsDeps.RepoGeneral = f.deps.RepoGeneral
	BindSettings(f.deps.SettingsDeps)
	t.Cleanup(func() { BindSettings(SettingsDeps{}) })
	return newSettingsServer(t, f)
}

func TestEnvironmentTabRendersAndWrites(t *testing.T) {
	fake := newFakeEnvThreads()
	fake.envs = []threads.EnvironmentView{{
		Name: "production", RequiredReviewers: []string{"ada", "grace"},
		Secrets: []string{"DEPLOY_TOKEN"}, CreatedAt: "2026-11-23T00:00:00Z", UpdatedAt: "2026-11-23T01:00:00Z",
	}}
	fake.secrets["production"] = []threads.SecretView{{Name: "DEPLOY_TOKEN", UpdatedAt: "2026-11-23T01:00:00Z"}}
	s := envSettingsServer(t, "maintain", fake)

	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/environments", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET environments = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-settings-tab="environments"`,
		`href="/acme/monorepo/settings/environments"`,
		`name="environment_name"`,
		`name="environment_reviewers"`,
		"production",
		"ada, grace",
		"DEPLOY_TOKEN",
		`action="/acme/monorepo/settings/environments/production"`,
		`value="delete"`,
		`name="environment_secret_name"`,
	} {
		settingsMustContain(t, body, want, "environments tab")
	}
	// Values never render: a stored secret is a name and an ellipsis.
	settingsMustContain(t, body, "•••••• (set)", "secret rows are write-only")
	// No secret value can appear because the model has no value field at all.
	for _, row := range fake.secrets["production"] {
		settingsMustNotContain(t, body, row.Name+"=", "no value rendering")
	}

	// A maintainer's save reaches the service (the seam's maintain floor).
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments",
		url.Values{"environment_name": {"staging"}, "environment_reviewers": {"ada,grace"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST environments = %d: %s", rec.Code, rec.Body.String())
	}
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments/production",
		url.Values{"environment_action": {"secret-put"}, "environment_secret_name": {"NEW"}, "environment_secret_value": {"v"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST secret = %d: %s", rec.Code, rec.Body.String())
	}
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments/production",
		url.Values{"environment_action": {"secret-delete"}, "environment_secret_name": {"DEPLOY_TOKEN"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST delete secret = %d: %s", rec.Code, rec.Body.String())
	}
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments/production",
		url.Values{"environment_action": {"delete"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST delete environment = %d: %s", rec.Code, rec.Body.String())
	}
	want := []string{"put:staging:ada,grace", "secret-put:production:NEW", "secret-delete:production:DEPLOY_TOKEN", "delete:production"}
	if strings.Join(fake.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("service calls = %v, want %v", fake.calls, want)
	}
}

func TestEnvironmentTabFloorAndNotWired(t *testing.T) {
	// A read/write principal never sees the tab (maintain floor) and its POST
	// never reaches the service.
	fake := newFakeEnvThreads()
	s := envSettingsServer(t, "write", fake)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := settingsDo(t, s, method, "/acme/monorepo/settings/environments",
			url.Values{"environment_name": {"staging"}}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s environments as writer = %d, want 403", method, rec.Code)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a writer's write reached the service: %v", fake.calls)
	}
	// A Threads service without the environment methods leaves the tab honest.
	f := settingsRepoFakes("maintain")
	services := SettingsServices{Threads: &fakeThreadsSettings{}}
	f.deps.SettingsDeps.BindSettingsServices(services)
	BindSettings(f.deps.SettingsDeps)
	t.Cleanup(func() { BindSettings(SettingsDeps{}) })
	plain := newSettingsServer(t, f)
	rec := settingsDo(t, plain, http.MethodGet, "/acme/monorepo/settings/environments", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired GET = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired panel")

	// A typed-nil *threads.Service (a restricted build wires settings with no
	// Threads service) is unwired too: the tab must not call a nil receiver.
	if (SettingsServices{Threads: (*threads.Service)(nil)}).environments() != nil {
		t.Fatal("a typed-nil Threads service must read as unwired")
	}
	if (SettingsServices{}).environments() != nil {
		t.Fatal("no Threads service must read as unwired")
	}
}

// TestThreadRunPageEnvironmentSurface pins the run page: the environment, the
// pending hold with its reviewers, the URL a job declared, and the approve
// control only for a principal the service says may decide. No secret can
// appear (the page model has no secret field).
func TestThreadRunPageEnvironmentSurface(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunPendingApproval
	run.ApprovalKind = threads.ApprovalEnvironment
	run.Environments = []string{"production"}
	job := threadsFixtureJob(run)
	job.Deployment = &threads.Deployment{
		Environment: "production", URL: "https://prod.example.test/", State: threads.DeploymentPending,
	}
	approval := threads.ApprovalView{
		Pending: true, Kind: threads.ApprovalEnvironment, Environments: []string{"production"},
		Reviewers: []string{"ada"}, CanDecide: true,
	}
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}, Approval: &approval}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	// A listed reviewer with only write access: the service's CanDecide is the
	// authority (the page's role ladder alone would hide the control).
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"environment", "production",
		"required reviewer", "ada",
		"https://prod.example.test/",
		"deployment pending",
		"/acme/widgets/threads/"+run.ID+"/approve",
		"deny",
	)
	// The review banner names the environment, not the fork gate's copy.
	if strings.Contains(rec.Body.String(), "first-time contributor's fork pull request") {
		t.Fatal("an environment hold rendered the fork gate's explanation")
	}

	// Without CanDecide (a read principal) the controls are absent even when
	// the run is held.
	approval.CanDecide = false
	f.principal = auth.Principal{Name: "reader"}
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}, Approval: &approval}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "read", nil },
	})
	rec = f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "/approve") {
		t.Fatal("a read principal was offered the deployment decision")
	}
	wantContains(t, rec, "production")
}
