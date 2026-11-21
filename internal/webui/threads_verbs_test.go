package webui

// threads_verbs_test.go — the Threads operator verbs and live-log panels on
// the run page (docs/features/15 §3/§6), plus Settings → Runners/Secrets.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestThreadRunPageRenderOperatorVerbs(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunRunning
	job := threadsFixtureJob(run)
	job.State = threads.JobRunning
	job.StepResults = []threads.StepResult{{Name: "test", Conclusion: "success"}}
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Cancel: func(context.Context, string, string, string, auth.Principal) error { return nil },
		Rerun:  func(context.Context, string, string, string, auth.Principal) error { return nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"cancel run",
		"re-run",
		"/acme/widgets/threads/"+run.ID+"/cancel",
		"/acme/widgets/threads/"+run.ID+"/rerun",
		"data-threadlog",
		"data-threadlog-url=\"/acme/widgets/api/threads/"+run.ID+"/jobs/j-build/log\"",
		"data-threadlog-live=\"1\"",
		"success", // the reported step result
	)
}

func TestThreadRunPageVerbsGateOnRole(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "read", nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "reader"}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "cancel run") {
		t.Fatal("a reader must not see the operator verbs")
	}
	// A write role sees them, and a failed lookup grants nothing.
	BindThreads(ThreadsDeps{
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	if !threadCanWrite(context.Background(), collabRepo{Owner: "acme", Name: "widgets"}, auth.Principal{Name: "x"}) {
		t.Fatal("a write role must enable the verbs")
	}
	BindThreads(ThreadsDeps{
		Role: func(context.Context, string, string, auth.Principal) (string, error) {
			return "", errors.New("identity down")
		},
	})
	if threadCanWrite(context.Background(), collabRepo{Owner: "acme", Name: "widgets"}, auth.Principal{Name: "x"}) {
		t.Fatal("a failed role lookup must not grant the verbs")
	}
	if threadCanWrite(context.Background(), collabRepo{}, auth.Principal{Name: "x"}) {
		t.Fatal("an unwired role must not grant the verbs")
	}
}

func TestThreadRunCancelAndRerunPosts(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	var cancelled, rerun bool
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{}}, nil
		},
		Cancel: func(_ context.Context, owner, repo, runID string, _ auth.Principal) error {
			cancelled = owner == "acme" && repo == "widgets" && runID == run.ID
			return nil
		},
		Rerun: func(_ context.Context, _, _, _ string, _ auth.Principal) error {
			rerun = true
			return nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.post("/acme/widgets/threads/"+run.ID+"/cancel", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=cancelled") {
		t.Fatalf("cancel redirect = %q", loc)
	}
	if !cancelled {
		t.Fatal("the cancel verb did not reach the service")
	}
	rec = f.post("/acme/widgets/threads/"+run.ID+"/rerun", url.Values{})
	if rec.Code != http.StatusSeeOther || !rerun {
		t.Fatalf("rerun = %d (%v)", rec.Code, rerun)
	}
	// A service refusal is the API's own status (a reader's POST is a 403).
	BindThreads(ThreadsDeps{
		Cancel: func(context.Context, string, string, string, auth.Principal) error {
			return threads.ErrForbidden
		},
		Rerun: func(context.Context, string, string, string, auth.Principal) error {
			return threads.ErrConflict
		},
	})
	if rec := f.post("/acme/widgets/threads/"+run.ID+"/cancel", url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden cancel = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/threads/"+run.ID+"/rerun", url.Values{}); rec.Code != http.StatusConflict {
		t.Fatalf("conflict rerun = %d", rec.Code)
	}
	// Cross-origin writes are refused before the service is reached.
	req := f.req("POST", "/acme/widgets/threads/"+run.ID+"/cancel", nil)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	f.srv.Serve(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin cancel = %d", rr.Code)
	}
	// An unwired service is a 503, never a silent redirect.
	BindThreads(ThreadsDeps{})
	if rec := f.post("/acme/widgets/threads/"+run.ID+"/cancel", url.Values{}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired cancel = %d", rec.Code)
	}
}

// fakeThreadsSettings is the SettingsServices.Threads seam.
type fakeThreadsSettings struct {
	runners   []threads.RunnerView
	secrets   []threads.SecretView
	enabled   bool
	err       error
	calls     []string
	mintToken *threads.RegistrationToken
}

func (f *fakeThreadsSettings) ListRunners(context.Context, string, auth.Principal) ([]threads.RunnerView, error) {
	f.calls = append(f.calls, "list-runners")
	return f.runners, f.err
}

func (f *fakeThreadsSettings) MintRegistrationToken(context.Context, string, auth.Principal) (*threads.RegistrationToken, error) {
	f.calls = append(f.calls, "mint")
	if f.err != nil {
		return nil, f.err
	}
	tok := f.mintToken
	if tok == nil {
		tok = &threads.RegistrationToken{ID: "abcd1234", Token: "wrg_x.abcd1234.secret", ExpiresAt: "2026-11-20T12:15:00Z"}
	}
	return tok, nil
}

func (f *fakeThreadsSettings) RevokeRunner(_ context.Context, _ string, id string, _ auth.Principal) error {
	f.calls = append(f.calls, "revoke:"+id)
	return f.err
}

func (f *fakeThreadsSettings) ListSecrets(context.Context, string, auth.Principal) ([]threads.SecretView, error) {
	f.calls = append(f.calls, "list-secrets")
	return f.secrets, f.err
}

func (f *fakeThreadsSettings) SetSecret(_ context.Context, _ string, name, value string, _ auth.Principal) error {
	f.calls = append(f.calls, "set:"+name+"="+value)
	return f.err
}

func (f *fakeThreadsSettings) DeleteSecret(_ context.Context, _ string, name string, _ auth.Principal) error {
	f.calls = append(f.calls, "delete:"+name)
	return f.err
}

func (f *fakeThreadsSettings) SecretsEnabled() bool { return f.enabled }

func TestSettingsRunnersTab(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{
			RunnersWired: true, SecretsWired: true, SecretsEnabled: true,
			Runners: []SettingsRunner{{
				ID: "abcd1234", Name: "build-01", Labels: "linux, docker", State: "active",
				Version: "1.2.3", CreatedAt: "2026-11-19T11:00:00Z", LastSeenAt: "2026-11-19T12:00:00Z",
			}, {ID: "efgh5678", State: "pending"}},
			Secrets: []SettingsSecret{{Name: "DEPLOY_TOKEN", CreatedAt: "x", UpdatedAt: "2026-11-19T11:05:00Z"}},
		}, nil
	}
	written := ""
	f.deps.RepoWrite = func(_ context.Context, _, _ string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		written = w.Action
		if w.Action == SettingsActionRunnerToken {
			return SettingsWriteResult{Secret: "wrg_x.abcd1234.secret", Notice: "runner registration token created"}, nil
		}
		return SettingsWriteResult{Notice: "ok"}, nil
	}
	s := newSettingsServer(t, f)

	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/runners", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("runners tab = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Runners", "build-01", "linux, docker", "active", "pending",
		"2026-11-19T12:00:00Z", "never", "mint token", "revoke"} {
		settingsMustContain(t, body, want, "runners tab")
	}
	// The mint answer renders the one-time token in the panel (never a URL).
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/runners", url.Values{"name": {"build-01"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d want 200", rec.Code)
	}
	if written != SettingsActionRunnerToken {
		t.Fatalf("mint action = %q", written)
	}
	settingsMustContain(t, rec.Body.String(), "wrg_x.abcd1234.secret", "mint secret")
	settingsMustContain(t, rec.Body.String(), `id="runner-registration-token"`, "copy target")
	// The revoke path form maps to the revoke action.
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/runners/abcd1234", url.Values{"runner_action": {"revoke"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d (%s)", rec.Code, rec.Body.String())
	}
	if written != SettingsActionRunnerRevoke {
		t.Fatalf("revoke action = %q", written)
	}
}

func TestSettingsSecretsTab(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{
			SecretsWired: true, SecretsEnabled: true,
			Secrets: []SettingsSecret{{Name: "DEPLOY_TOKEN", CreatedAt: "x", UpdatedAt: "2026-11-19T11:05:00Z"}},
		}, nil
	}
	written := ""
	f.deps.RepoWrite = func(_ context.Context, _, _ string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		written = w.Action
		return SettingsWriteResult{Notice: "ok"}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/secrets", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets tab = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"DEPLOY_TOKEN", "•••••• (set)", "Write-only", "secret_name", "secret_value"} {
		settingsMustContain(t, body, want, "secrets tab")
	}
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/secrets", url.Values{"secret_name": {"NEW"}, "secret_value": {"v"}}, nil)
	if rec.Code != http.StatusSeeOther || written != SettingsActionSecretPut {
		t.Fatalf("put secret = %d (%q)", rec.Code, written)
	}
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/secrets/DEPLOY_TOKEN", url.Values{"secret_action": {"delete"}}, nil)
	if rec.Code != http.StatusSeeOther || written != SettingsActionSecretDelete {
		t.Fatalf("delete secret = %d (%q)", rec.Code, written)
	}
	// A disabled keyring renders the explanation instead of a form.
	f2 := settingsRepoFakes("admin")
	f2.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{SecretsWired: true, SecretsEnabled: false}, nil
	}
	s = newSettingsServer(t, f2)
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/secrets", nil, nil)
	settingsMustContain(t, rec.Body.String(), "Secrets are unavailable", "disabled notice")
	// An unwired service keeps the degraded card.
	f3 := settingsRepoFakes("admin")
	f3.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{}, nil
	}
	s = newSettingsServer(t, f3)
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/runners", nil, nil)
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired runners")
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/secrets", nil, nil)
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired secrets")
}

func TestSettingsThreadsSeam(t *testing.T) {
	fake := &fakeThreadsSettings{
		enabled: true,
		runners: []threads.RunnerView{{
			ID: "abcd1234", Name: "build-01", Labels: []string{"linux", "docker"},
			State: threads.RunnerActive, DeclaredVersion: "1.2.3", CreatedAt: "2026-11-19T11:00:00Z",
			LastSeenAt: strPtr("2026-11-19T12:00:00Z"),
		}},
		secrets: []threads.SecretView{{Name: "DEPLOY_TOKEN", CreatedAt: "a", UpdatedAt: "b"}},
	}
	svc := SettingsServices{Threads: fake, API: &api.Env{}}
	admin := auth.Principal{Name: "admin", Admin: true}
	view, err := svc.repoAdmin(context.Background(), "acme", "monorepo", admin)
	if err != nil {
		t.Fatalf("repoAdmin: %v", err)
	}
	if !view.RunnersWired || !view.SecretsWired || !view.SecretsEnabled {
		t.Fatalf("wiring flags = %+v", view)
	}
	if len(view.Runners) != 1 || view.Runners[0].LastSeenAt != "2026-11-19T12:00:00Z" || view.Runners[0].Labels != "linux, docker" {
		t.Fatalf("runners = %+v", view.Runners)
	}
	if len(view.Secrets) != 1 || view.Secrets[0].Name != "DEPLOY_TOKEN" {
		t.Fatalf("secrets = %+v", view.Secrets)
	}
	// Writes: token mint carries the one-time secret; each action reaches the
	// seam with the form value.
	res, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionRunnerToken, Values: map[string]string{}})
	if werr != nil || res.Secret == "" || !strings.Contains(res.Notice, "registration token") {
		t.Fatalf("mint = %+v, %v", res, werr)
	}
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionRunnerRevoke, Values: map[string]string{"id": "abcd1234"}}); werr != nil {
		t.Fatalf("revoke = %v", werr)
	}
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretPut, Values: map[string]string{"secret_name": "X", "secret_value": "v"}}); werr != nil {
		t.Fatalf("put = %v", werr)
	}
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretDelete, Values: map[string]string{"id": "X"}}); werr != nil {
		t.Fatalf("delete = %v", werr)
	}
	got := strings.Join(fake.calls, ",")
	for _, want := range []string{"list-runners", "list-secrets", "mint", "revoke:abcd1234", "set:X=v", "delete:X"} {
		if !strings.Contains(got, want) {
			t.Fatalf("seam calls %q missing %q", got, want)
		}
	}
	// A failing seam maps onto the write status.
	fake.err = threads.ErrUnavailable
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionRunnerToken}); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("mint error = %+v", werr)
	}
	// An unwired seam is a 503, not a panic.
	unwired := SettingsServices{}
	if _, werr := unwired.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretPut}); werr == nil {
		t.Fatal("unwired secret write must fail")
	}
}

func TestSettingsThreadsStatusMapping(t *testing.T) {
	for err, want := range map[error]int{
		threads.ErrNotFound:     http.StatusNotFound,
		threads.ErrInvalid:      http.StatusUnprocessableEntity,
		threads.ErrUnauthorized: http.StatusUnauthorized,
		threads.ErrForbidden:    http.StatusForbidden,
		threads.ErrUnavailable:  http.StatusServiceUnavailable,
		errors.New("other"):     http.StatusUnprocessableEntity,
	} {
		if got := settingsThreadsStatus(err); got != want {
			t.Errorf("settingsThreadsStatus(%v) = %d, want %d", err, got, want)
		}
	}
}

func strPtr(s string) *string { return &s }
