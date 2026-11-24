package webui

// threads_test.go — the Threads pages (docs/features/15 §6): the run list and
// one run's jobs render inside the repository chrome (one masthead + the tab
// strip, Threads current). The desktop/~390px requirement is a layout
// property of the shared chrome/list classes (the page adds no fixed width);
// the structural assertions here are the rendered-DOM proof the browser check
// pins for every other collab page.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
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
		Context: "threads/ci/build", RunsOn: "linux",
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
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, _ threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}, More: true}, nil
		},
		Get: func(_ context.Context, _, _, runID string, _ auth.Principal) (*threads.RunView, error) {
			if runID != run.ID {
				return nil, threads.ErrNotFound
			}
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
		Workflows: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{{
				Path: run.WorkflowPath, Name: run.Workflow, Digest: run.WorkflowDigest,
				Triggers: []string{"push", "pull_request"},
			}}}, nil
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
		"linux",
		"echo build",
		".spindle/workflows/ci.yml",
		"ada",
	)
}

func TestThreadsEmptyListPage(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
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
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
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

// Pass A deliverable 5: the Threads page lists every discovered workflow with
// its path, name and triggers, and shows a per-file parse error that leaves the
// other files (and the run list) intact.
func TestThreadsListShowsWorkflowsAndParseErrors(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{
				{Path: ".spindle/workflows/ci.yml", Name: "CI", Triggers: []string{"push", "pull_request"}},
				{Path: ".spindle/workflows/broken.yml", Name: "broken", Error: "workflow: unknown key \"inputs\""},
			}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantChrome(t, bodyText(rec), "acme/widgets", "threads")
	wantContains(t, rec,
		"Workflows",
		".spindle/workflows/ci.yml",
		".spindle/workflows/broken.yml",
		"pull_request",
		`id="workflow-spindle-workflows-broken-yml"`,
		"unknown key",
		"inputs",
		"parse error",
		run.ID, // the run list still renders next to a broken workflow
	)
}

func TestThreadsListWorkflowDiscoveryFailureIsVisible(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return nil, errors.New("git unavailable")
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "workflows: git unavailable")
}

func TestThreadsRunPageLinksWorkflowError(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bindThreadsFixture(t, run)
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{threadsFixtureJob(run)}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{
				{Path: run.WorkflowPath, Name: run.Workflow, Error: "workflow: bad shape"},
			}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"fails to parse",
		"workflow: bad shape",
		"/acme/widgets/threads#workflow-spindle-workflows-ci-yml",
	)
}

// --- Run workflow control (round 2) -----------------------------------------

// dispatchFixture binds a write-capable principal, one workflow that declares
// workflow_dispatch with all three input types, and a Dispatch seam that
// records what the form posted.
type dispatchCall struct {
	ref, workflow string
	inputs        map[string]string
}

func bindDispatchFixture(t *testing.T, principal auth.Principal, dispatchErr error) (*dispatchCall, *threads.Run) {
	t.Helper()
	run := threadsFixtureRun()
	call := &dispatchCall{}
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{
				{Path: ".spindle/workflows/build.yml", Name: "Build", Triggers: []string{"push"}},
				{Path: ".spindle/workflows/deploy.yml", Name: "Deploy", Triggers: []string{"workflow_dispatch"},
					Inputs: []threads.DispatchInput{
						{Name: "count", Type: "string", Default: "2", HasDefault: true},
						{Name: "dry_run", Type: "boolean", Default: "false", HasDefault: true},
						{Name: "env", Type: "choice", Required: true, Options: []string{"staging", "production"}},
					}},
			}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) {
			if principal.Write || principal.Admin {
				return "write", nil
			}
			return "read", nil
		},
		Dispatch: func(_ context.Context, _, _, ref, workflow string, inputs map[string]string, _ auth.Principal) ([]*threads.Run, error) {
			call.ref, call.workflow, call.inputs = ref, workflow, inputs
			if dispatchErr != nil {
				return nil, dispatchErr
			}
			return []*threads.Run{run}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	return call, run
}

// TestThreadsDispatchFormRendersTypedInputs: the page offers the Run workflow
// control, lists only workflow_dispatch files, and renders the declared inputs
// as typed controls (select for choice/boolean, text for string) with defaults.
func TestThreadsDispatchFormRendersTypedInputs(t *testing.T) {
	bindChromeProjections(t)
	bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"Run workflow",
		"threads-dispatch",
		`action="/acme/widgets/threads/dispatch"`,
		".spindle/workflows/deploy.yml",
		`name="ref"`,
		`value="main"`,
		`name="input.env"`,
		`name="input.dry_run"`,
		`name="input.count"`,
		`value="2"`,
		`<option value="staging"`,
		"run workflow",
		"btn primary",
	)
	// The build workflow does not declare the manual trigger: it is not an
	// option of the select (it is still listed in the Workflows section).
	if strings.Contains(bodyText(rec), `<option value=".spindle/workflows/build.yml"`) {
		t.Fatal("a non-workflow_dispatch file was offered in the Run workflow select")
	}
	// Exactly one primary action on the page.
	if n := strings.Count(bodyText(rec), `class="btn primary"`); n != 1 {
		t.Fatalf("primary buttons = %d, want 1 (style guideline §1 F3)", n)
	}
}

// TestThreadsDispatchFormHiddenForReader: the affordance follows the write role
// (the service is still the gate).
func TestThreadsDispatchFormHiddenForReader(t *testing.T) {
	bindChromeProjections(t)
	bindDispatchFixture(t, auth.Principal{Name: "bob"}, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "bob"}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(bodyText(rec), "threads-dispatch") || strings.Contains(bodyText(rec), "Run workflow") {
		t.Fatal("a read-only principal was offered the dispatch form")
	}
}

// TestThreadsDispatchPostsInputs: the form posts ref + workflow + typed inputs
// through the same service call and redirects to the created run.
func TestThreadsDispatchPostsInputs(t *testing.T) {
	bindChromeProjections(t)
	call, run := bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.post("/acme/widgets/threads/dispatch", map[string][]string{
		"workflow":      {".spindle/workflows/deploy.yml"},
		"ref":           {"release/1.0"},
		"input.env":     {"production"},
		"input.count":   {"5"},
		"input.dry_run": {"true"},
	})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/threads/"+run.ID+"?notice=workflow+dispatched" {
		t.Fatalf("redirect = %q", loc)
	}
	if call.workflow != ".spindle/workflows/deploy.yml" || call.ref != "release/1.0" {
		t.Fatalf("dispatch call = %+v", call)
	}
	if call.inputs["env"] != "production" || call.inputs["count"] != "5" || call.inputs["dry_run"] != "true" {
		t.Fatalf("dispatch inputs = %v", call.inputs)
	}
}

// TestThreadsDispatchRendersInlineError: a validation failure re-renders the
// page (422) with the reason and the submitted values, not a bare error page.
func TestThreadsDispatchRendersInlineError(t *testing.T) {
	bindChromeProjections(t)
	bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true},
		fmt.Errorf("%w: input %q must be one of staging, production, got %q", threads.ErrUnprocessable, "env", "moon"))
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.post("/acme/widgets/threads/dispatch", map[string][]string{
		"workflow":    {".spindle/workflows/deploy.yml"},
		"ref":         {"main"},
		"input.env":   {"moon"},
		"input.count": {"7"},
	})
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantContains(t, rec,
		`input &#34;env&#34; must be one of`,
		"Run workflow",
		`value="7"`, // the submitted count survives the re-render
	)
}

// --- check-state fidelity (round 2) -----------------------------------------

// TestThreadsCancelledRunReadsCancelled pins the UI text: a cancelled run says
// cancelled on the run page and the list (never "failure"), while a failure
// still says failure.
func TestThreadsCancelledRunReadsCancelled(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunCancelled
	job := threadsFixtureJob(run)
	job.State = threads.JobCancelled
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()

	list := f.get("/acme/widgets/threads")
	wantStatus(t, list, http.StatusOK)
	wantContains(t, list, "cancelled")
	// The state suffix only appears in the filter's <option> list; the ROW (a
	// chip) must say cancelled, never failure.
	if strings.Contains(bodyText(list), `chip-neutral">failure`) || strings.Contains(bodyText(list), `<span class="chip chip-neutral">failure`) {
		t.Fatal("the run list called a cancelled run a failure")
	}
	detail := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, detail, http.StatusOK)
	wantContains(t, detail, "cancelled", "(cancelled)")
}

// TestThreadsInProgressStateAndDuration pins the distinct in_progress state and
// the derived duration on the run page and the list.
func TestThreadsInProgressStateAndDuration(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunRunning
	run.DurationSeconds = 72 // 1m 12s
	job := threadsFixtureJob(run)
	job.State = threads.JobRunning
	job.DurationSeconds = 72
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()

	detail := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, detail, http.StatusOK)
	wantContains(t, detail, "running", "took 1m 12s")

	list := f.get("/acme/widgets/threads")
	wantStatus(t, list, http.StatusOK)
	wantContains(t, list, "1m 12s")
	// The dot vocabulary carries a distinct in_progress value (hollow circle
	// + pulse via `.dot-progress`), not "pending" (PLAN-THEMES §6.1).
	if !strings.Contains(bodyText(detail), "dot-progress") {
		t.Fatal("a running job did not render the in_progress check dot")
	}
}

// TestThreadsDispatchEdgeCases covers the control's other paths: default ref
// fallbacks, untyped label rendering, and the POST's refusal paths.
func TestThreadsDispatchEdgeCases(t *testing.T) {
	bindChromeProjections(t)
	bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	// A summary whose head name is a sha (unborn/unknown default branch) still
	// yields a usable ref field.
	f.view.summary = api.SummaryData{
		Head:     &api.Ref{Name: "", SHA: strings.Repeat("b", 40)},
		Features: ptrFeatures(config.AllFeatures()),
	}
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `value="main"`)
	if !looksLikeSHA(strings.Repeat("b", 40)) || looksLikeSHA("main") || looksLikeSHA("abc") {
		t.Fatal("looksLikeSHA misclassified")
	}
	if got := threadDispatchLabel(&threadWorkflowRow{Path: ".spindle/workflows/ci.yml", Name: "ci"}); got != ".spindle/workflows/ci.yml" {
		t.Fatalf("stem-named label = %q", got)
	}
	if got := threadDispatchLabelefault("deploy.yml"); got == "" {
		t.Fatal("empty label")
	}
}

// threadDispatchLabelefault exercises the empty-select path (no dispatch file).
func threadDispatchLabelefault(path string) string {
	return threadDispatchLabel(&threadWorkflowRow{Path: path, Name: "Deploy"})
}

// TestThreadsDispatchRefusals pins each POST refusal: not wired, cross-origin,
// no matching workflow (404), and a forbidden service error.
func TestThreadsDispatchRefusals(t *testing.T) {
	bindChromeProjections(t)
	// Not wired.
	BindThreads(ThreadsDeps{})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	wantStatus(t, f.post("/acme/widgets/threads/dispatch", map[string][]string{"workflow": {"ci.yml"}}), http.StatusServiceUnavailable)

	// No dispatch file matched: 404 (the service's ErrNotFound).
	_, _ = bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	BindThreads(ThreadsDeps{
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Dispatch: func(context.Context, string, string, string, string, map[string]string, auth.Principal) ([]*threads.Run, error) {
			return nil, nil // no workflow fired
		},
	})
	f.post("/acme/widgets/threads/dispatch", map[string][]string{"ref": {"main"}})
	rec := f.post("/acme/widgets/threads/dispatch", map[string][]string{"ref": {"main"}})
	wantStatus(t, rec, http.StatusNotFound)

	// A forbidden service refusal is the error page, not a form re-render.
	BindThreads(ThreadsDeps{
		Dispatch: func(context.Context, string, string, string, string, map[string]string, auth.Principal) ([]*threads.Run, error) {
			return nil, threads.ErrForbidden
		},
	})
	wantStatus(t, f.post("/acme/widgets/threads/dispatch", map[string][]string{"ref": {"main"}}), http.StatusForbidden)

	// Cross-origin posts are refused before the service runs.
	bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	req := f.req("POST", "/acme/widgets/threads/dispatch", map[string][]string{"ref": {"main"}})
	req.Header.Set("Origin", "http://evil.example")
	out := httptest.NewRecorder()
	if !f.srv.Serve(out, req) {
		t.Fatal("dispatch route not claimed")
	}
	wantStatus(t, out, http.StatusForbidden)
}

// TestRunDotStateVocabulary pins the run-page dot projection for every state.
func TestRunDotStateVocabulary(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		threads.RunQueued:    "pending",
		threads.RunRunning:   "in_progress",
		threads.RunSuccess:   "success",
		threads.RunFailure:   "failure",
		threads.RunCancelled: "cancelled",
		threads.RunSkipped:   "skipped",
	} {
		if got := runDotState(state); got != want {
			t.Errorf("runDotState(%s) = %q, want %q", state, got, want)
		}
	}
}

// TestDurationText pins the compact duration renderer used by the run page.
func TestDurationText(t *testing.T) {
	t.Parallel()
	for seconds, want := range map[int64]string{
		0: "", -3: "", 5: "5s", 59: "59s", 60: "1m 00s", 72: "1m 12s",
		3599: "59m 59s", 3600: "1h 00m", 7322: "2h 02m",
	} {
		if got := durationText(seconds); got != want {
			t.Errorf("durationText(%d) = %q, want %q", seconds, got, want)
		}
	}
}

// TestThreadsListServiceFailureRendersPage pins the non-gate list error path:
// the page renders with the error text instead of a blank list.
func TestThreadsListServiceFailureRendersPage(t *testing.T) {
	bindChromeProjections(t)
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return nil, errors.New("store unavailable")
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "store unavailable")
}

// TestThreadWorkflowErrorSeams pins the run page's workflow-error lookup when
// the seam is unwired, errors, or the file parses cleanly.
func TestThreadWorkflowErrorSeams(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bindThreadsFixture(t, run)
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	wantStatus(t, f.get("/acme/widgets/threads/"+run.ID), http.StatusOK)

	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return nil, errors.New("git down")
		},
	})
	wantStatus(t, f.get("/acme/widgets/threads/"+run.ID), http.StatusOK)

	if got := looksLikeSHA("zzzzzzzz"); got {
		t.Fatal("a non-hex label was treated as a sha")
	}
}

// TestThreadsPageHelpers pins the small display helpers and the unwired/verb
// refusal paths (each is a real branch on the page surface).
func TestThreadsPageHelpers(t *testing.T) {
	if got := workflowAnchor(""); got != "workflow" {
		t.Fatalf("workflowAnchor(empty) = %q", got)
	}
	if got := shortRunID("abc"); got != "abc" {
		t.Fatalf("shortRunID(short) = %q", got)
	}

	bindChromeProjections(t)
	run := threadsFixtureRun()
	f := newCollabFixture(t)
	f.view.summary = busySummary()

	// Run page with the service unwired.
	BindThreads(ThreadsDeps{})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	wantStatus(t, f.get("/acme/widgets/threads/"+run.ID), http.StatusServiceUnavailable)

	// A verb with no service is a 503; a service refusal is its own status.
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{}}, nil
		},
	})
	wantStatus(t, f.post("/acme/widgets/threads/"+run.ID+"/cancel", nil), http.StatusServiceUnavailable)
	BindThreads(ThreadsDeps{
		Cancel: func(context.Context, string, string, string, auth.Principal) error {
			return threads.ErrForbidden
		},
	})
	wantStatus(t, f.post("/acme/widgets/threads/"+run.ID+"/cancel", nil), http.StatusForbidden)
}

// TestThreadsDispatchFormBranches pins the POST's remaining branches: a
// denied read gate, an unreadable form body, and an omitted ref (the form's
// default fills it).
func TestThreadsDispatchFormBranches(t *testing.T) {
	bindChromeProjections(t)
	call, _ := bindDispatchFixture(t, auth.Principal{Name: "ada", Write: true}, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()

	// A read-gate denial never reaches the form (the page's gate mirrors the
	// API's).
	f.readGate = func(_, _ string, _ auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no read"}
	}
	wantStatus(t, f.get("/acme/widgets/threads"), http.StatusForbidden)
	wantStatus(t, f.post("/acme/widgets/threads/dispatch", map[string][]string{"ref": {"main"}}), http.StatusForbidden)
	f.readGate = nil

	// An unreadable form body is a 400, never a panic.
	req := httptest.NewRequest("POST", "/acme/widgets/threads/dispatch", strings.NewReader("ref=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = testHost
	req.Header.Set("Origin", "http://"+testHost)
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("dispatch route not claimed")
	}
	wantStatus(t, rec, http.StatusBadRequest)

	// No ref in the body: the default branch fills it.
	out := f.post("/acme/widgets/threads/dispatch", map[string][]string{
		"workflow": {".spindle/workflows/deploy.yml"},
	})
	wantStatus(t, out, http.StatusSeeOther)
	if call.ref != "main" {
		t.Fatalf("default ref = %q, want main", call.ref)
	}
}

// TestThreadsApprovalGateRendered pins the gate's page surface: a held run
// explains itself, a maintain-capable caller sees approve (the page's primary)
// and deny, and a caller below maintain sees neither. The service is the gate;
// the page only renders the affordance.
func TestThreadsApprovalGateRendered(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunPendingApproval
	run.Trusted = false
	run.Actor = "newcomer"
	job := threadsFixtureJob(run)
	job.State = threads.JobQueued
	role := "maintain"
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return role, nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()

	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "waiting for a maintainer", "first-time contributor", "approve run", "deny")
	// Approve is the run page's one primary action (style guideline F3).
	if strings.Count(bodyText(rec), `btn primary`) != 1 {
		t.Fatalf("run page primaries = %d, want 1 (approve run)", strings.Count(bodyText(rec), "btn primary"))
	}
	// A write-but-not-maintain caller sees no decision controls.
	role = "write"
	rec = f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "waiting for a maintainer")
	if strings.Contains(bodyText(rec), "approve run") {
		t.Fatal("a non-maintainer saw the approve control")
	}
}

// TestChecksDurationAndMaintainGate pins the two small round-2 helpers: the
// commit panel's duration arithmetic and the approval gate's role probe.
func TestChecksDurationAndMaintainGate(t *testing.T) {
	started, completed := "2026-11-21T00:00:00Z", "2026-11-21T00:01:05Z"
	if got := checksDuration(&started, &completed); got != 65 {
		t.Fatalf("checksDuration = %d, want 65", got)
	}
	if got := checksDuration(nil, &completed); got != 0 {
		t.Fatalf("nil started = %d", got)
	}
	bad := "not a stamp"
	if got := checksDuration(&bad, &completed); got != 0 {
		t.Fatalf("bad stamp = %d", got)
	}
	reversed := "2026-11-20T00:00:00Z"
	if got := checksDuration(&completed, &reversed); got != 0 {
		t.Fatalf("reversed = %d", got)
	}

	// The maintain floor: admin always, a nil Role seam falls back to write,
	// and a resolved role must be maintain+.
	if !threadCanMaintain(context.Background(), collabRepo{}, auth.Principal{Admin: true}) {
		t.Fatal("admin not treated as maintainer")
	}
	prev := threadsDeps.Role
	threadsDeps.Role = nil
	t.Cleanup(func() { threadsDeps.Role = prev })
	if !threadCanMaintain(context.Background(), collabRepo{}, auth.Principal{Name: "w", Write: true}) {
		t.Fatal("write principal without a role seam not allowed")
	}
	if threadCanMaintain(context.Background(), collabRepo{}, auth.Principal{Name: "r"}) {
		t.Fatal("reader allowed to maintain")
	}
	threadsDeps.Role = func(context.Context, string, string, auth.Principal) (string, error) {
		return "", errors.New("identity down")
	}
	if threadCanMaintain(context.Background(), collabRepo{}, auth.Principal{Name: "x"}) {
		t.Fatal("an unresolved role was treated as maintainer")
	}
	threadsDeps.Role = func(context.Context, string, string, auth.Principal) (string, error) {
		return "write", nil
	}
	if threadCanMaintain(context.Background(), collabRepo{}, auth.Principal{Name: "x"}) {
		t.Fatal("write role treated as maintainer")
	}
}

// TestThreadsFiltersAndGateVerbs pins the round-2 list filter's rendered states
// and the approve/deny page verbs (one service call each, then a redirect).
func TestThreadsFiltersAndGateVerbs(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunPendingApproval
	filtered := threads.RunFilter{}
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, f threads.RunFilter) (*threads.RunPage, error) {
			filtered = f
			return &threads.RunPage{Runs: []*threads.Run{}}, nil
		},
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{threadsFixtureJob(run)}}, nil
		},
		Approve: func(_ context.Context, _, _, id string, _ auth.Principal) error {
			if id != run.ID {
				t.Errorf("approve id = %q", id)
			}
			return nil
		},
		Deny: func(_ context.Context, _, _, id string, _ auth.Principal) error {
			if id != run.ID {
				t.Errorf("deny id = %q", id)
			}
			return nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()

	rec := f.get("/acme/widgets/threads?workflow=CI&state=pending_approval&actor=bob")
	wantStatus(t, rec, http.StatusOK)
	if filtered.Workflow != "CI" || filtered.State != "pending_approval" || filtered.Actor != "bob" {
		t.Fatalf("filter passed to the service = %+v", filtered)
	}
	wantContains(t, rec, "No run matches this filter", "clear")

	// The page's approve and deny verbs each call the service and redirect.
	for _, verb := range []string{"approve", "deny"} {
		rec := f.post("/acme/widgets/threads/"+run.ID+"/"+verb, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d, want a redirect", verb, rec.Code)
		}
	}
}
