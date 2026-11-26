package webui

// threads_verbs_test.go — the Threads operator verbs and live-log panels on
// the run page (docs/features/15 §3/§6), plus Settings → Secrets.

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

func TestThreadRunPageRendersAnnotationsAndSummary(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	job := threadsFixtureJob(run)
	job.Annotations = []threads.Annotation{{
		Level: "error", Message: "<script>alert(1)</script> & broke", File: "main.go",
		Line: 3, Col: 2, Title: "Build", Step: "test",
	}}
	job.Summary = "## Result\n\n<script>alert('x')</script>\n\n```mermaid\ngraph TD; A-->B;\n```\n"
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	wantContains(t, rec, "Annotations", "main.go:3:2", "Build", "step summary", "graph TD;")
	// The annotation message and the summary are DATA: the raw <script> must
	// never reach the page as markup.
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("a workflow-command payload was rendered as markup:\n%s", body)
	}
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
	secrets []threads.SecretView
	enabled bool
	err     error
	calls   []string
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
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/secrets", nil, nil)
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired secrets")
}

func TestSettingsThreadsSeam(t *testing.T) {
	t.Parallel()
	fake := &fakeThreadsSettings{
		enabled: true,
		secrets: []threads.SecretView{{Name: "DEPLOY_TOKEN", CreatedAt: "a", UpdatedAt: "b"}},
	}
	svc := SettingsServices{Threads: fake, API: &api.Env{}}
	admin := auth.Principal{Name: "admin", Admin: true}
	view, err := svc.repoAdmin(context.Background(), "acme", "monorepo", admin)
	if err != nil {
		t.Fatalf("repoAdmin: %v", err)
	}
	if !view.SecretsWired || !view.SecretsEnabled {
		t.Fatalf("wiring flags = %+v", view)
	}
	if len(view.Secrets) != 1 || view.Secrets[0].Name != "DEPLOY_TOKEN" {
		t.Fatalf("secrets = %+v", view.Secrets)
	}
	// Writes: each action reaches the seam with the form value.
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretPut, Values: map[string]string{"secret_name": "X", "secret_value": "v"}}); werr != nil {
		t.Fatalf("put = %v", werr)
	}
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretDelete, Values: map[string]string{"id": "X"}}); werr != nil {
		t.Fatalf("delete = %v", werr)
	}
	got := strings.Join(fake.calls, ",")
	for _, want := range []string{"list-secrets", "set:X=v", "delete:X"} {
		if !strings.Contains(got, want) {
			t.Fatalf("seam calls %q missing %q", got, want)
		}
	}
	// A failing seam maps onto the write status.
	fake.err = threads.ErrUnavailable
	if _, werr := svc.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretPut, Values: map[string]string{"secret_name": "X", "secret_value": "v"}}); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("secret error = %+v", werr)
	}
	// An unwired seam is a 503, not a panic.
	unwired := SettingsServices{}
	if _, werr := unwired.repoWrite(context.Background(), "acme", "monorepo", admin,
		SettingsRepoWrite{Action: SettingsActionSecretPut}); werr == nil {
		t.Fatal("unwired secret write must fail")
	}
}

func TestSettingsThreadsStatusMapping(t *testing.T) {
	t.Parallel()
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

// TestThreadRunPageShowsSkipReasonAndToleratedFailure pins the two new job
// markers on the run page: a job that did not run says why, and a failed job
// whose `continue-on-error` tolerated it is distinguishable from a success.
func TestThreadRunPageShowsSkipReasonAndToleratedFailure(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunSuccess
	skipped := threadsFixtureJob(run)
	skipped.ID, skipped.Key, skipped.Name = "j-skip", "skip", "skip"
	skipped.Context = "threads/ci/skip"
	skipped.State = threads.JobSkipped
	skipped.Reason = "if: github.ref_name == 'dev'"
	tolerated := threadsFixtureJob(run)
	tolerated.ID, tolerated.Key, tolerated.Name = "j-flaky", "flaky", "flaky"
	tolerated.Context = "threads/ci/flaky"
	tolerated.State = threads.JobFailure
	tolerated.Tolerated = true
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{skipped, tolerated}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{
		"if: github.ref_name == &#39;dev&#39;",
		"failure · tolerated",
		"why the job did not run",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page missing %q\n%s", want, body)
		}
	}
}
