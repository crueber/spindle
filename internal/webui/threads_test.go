package webui

// threads_test.go — the Threads pages (docs/features/15 §6): the run list and
// one run's jobs render inside the repository chrome (one masthead + the tab
// strip, Threads current). The desktop/~390px requirement is a layout
// property of the shared chrome/list classes (the page adds no fixed width);
// the structural assertions here are the rendered-DOM proof the browser check
// pins for every other collab page.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func threadsFixtureRun() *threads.Run {
	sha := strings.Repeat("d", 40)
	started := "2026-11-19T11:59:00Z"
	return &threads.Run{
		ID: "0000000000000000001-abcdef", Repo: "acme/widgets", Commit: sha,
		Event: threads.EventPush, Branch: "main", Ref: "refs/heads/main",
		Workflow: "CI", WorkflowPath: ".spindle/workflows/ci.yml",
		WorkflowCommit: sha, WorkflowDigest: strings.Repeat("a", 64),
		Trusted: true, State: threads.RunFailure, Actor: "ada", Attempt: 1,
		CreatedAt: "2026-11-19T11:58:00Z", UpdatedAt: "2026-11-19T12:01:00Z",
		StartedAt: &started, Version: 3,
	}
}

func threadsFixtureJob(run *threads.Run) *threads.Job {
	return &threads.Job{
		ID: "j-build", RunID: run.ID, Repo: run.Repo, Key: "build", Name: "build",
		Context: "threads/ci/build", RunsOn: "ubuntu-latest",
		Steps: []threads.Step{{Run: "echo build"}},
		Needs: []string{}, Attempt: 1, Trusted: true, State: threads.JobFailure,
		CreatedAt: "2026-11-19T11:58:00Z", UpdatedAt: "2026-11-19T12:01:00Z",
		Version: 2,
	}
}

func bindThreadsFixture(t *testing.T, run *threads.Run) {
	t.Helper()
	job := threadsFixtureJob(run)
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}, More: true}, nil
		},
		Get: func(_ context.Context, _, _, runID string, _ auth.Principal) (*threads.RunView, error) {
			if runID != run.ID {
				return nil, threads.ErrNotFound
			}
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
}

func TestThreadsListPageRendersChrome(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bindThreadsFixture(t, run)
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantChrome(t, bodyText(rec), "acme/widgets", "threads")
	wantContains(t, rec,
		"Threads",
		"/acme/widgets/threads/"+run.ID,
		"CI",
		"push",
		"/acme/widgets/commit/"+run.Commit,
		"failure",
		"abcde", // short run id tail
		"Older",
	)
}

func TestThreadsRunPageRendersJobs(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bindThreadsFixture(t, run)
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantChrome(t, bodyText(rec), "acme/widgets", "threads")
	wantContains(t, rec,
		"threads/ci/build",
		"ubuntu-latest",
		"echo build",
		".spindle/workflows/ci.yml",
		"ada",
	)
}

func TestThreadsEmptyListPage(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "No workflow runs yet", ".spindle/workflows/")
}

func TestThreadsNotWired(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	wantStatus(t, f.get("/acme/widgets/threads"), http.StatusServiceUnavailable)
	wantStatus(t, f.get("/acme/widgets/threads/0000000000000000001-abcdef"), http.StatusServiceUnavailable)
}

func TestThreadsPageErrors(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int) (*threads.RunPage, error) {
			return nil, threads.ErrUnauthorized
		},
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return nil, threads.ErrNotFound
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	wantStatus(t, f.get("/acme/widgets/threads"), http.StatusUnauthorized)
	wantStatus(t, f.get("/acme/widgets/threads/0000000000000000001-abcdef"), http.StatusNotFound)
}

func TestThreadsPageIsNotInTheAPILane(t *testing.T) {
	// The page family must not claim the JSON API (the Seam 1 chain owns it).
	bindThreadsFixture(t, threadsFixtureRun())
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := httptest.NewRecorder()
	if f.srv.Serve(rec, f.req("GET", "/acme/widgets/api/threads", nil)) {
		t.Fatal("the page provider claimed the JSON API lane")
	}
}
