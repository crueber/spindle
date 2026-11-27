// threads_migration_test.go — PAR-003: the Threads page's migration banner.
//
// A repository that carries `.forgejo/workflows` or `.github/workflows` and no
// `.spindle/workflows` is shown the foreign files and the exact
// `spindle threads import` invocation. The banner is a hint: the page asks the
// display-only seam, and nothing creates a run.
package webui

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestThreadsMigrationBannerRenders(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	foreignCalls := 0
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, _ threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{}, nil
		},
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
		Workflows: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{}}, nil
		},
		Foreign: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.ForeignWorkflows, error) {
			foreignCalls++
			return &threads.ForeignWorkflows{
				Forgejo: []string{".forgejo/workflows/ci.yml", ".forgejo/workflows/release.yaml"},
				GitHub:  []string{".github/workflows/publish.yml"},
			}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	for _, want := range []string{
		"threads-migration-heading",
		"This repository has Forgejo/GitHub workflows, but no Spindle workflows",
		".forgejo/workflows/ci.yml",
		".forgejo/workflows/release.yaml",
		".github/workflows/publish.yml",
		"spindle threads import --from forgejo --repo .",
		"spindle threads import --from github --repo .",
		"creates no run",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("banner misses %q", want)
		}
	}
	if foreignCalls != 1 {
		t.Fatalf("foreign seam calls = %d, want 1", foreignCalls)
	}
	// The banner is a list, not a run: the page still renders the run list's
	// empty state and no run link appears.
	if strings.Contains(body, "/acme/widgets/threads/"+run.ID) {
		t.Fatal("visiting the page created/rendered a run")
	}
}

// TestThreadsMigrationBannerHiddenWhenWorkflowsExist: a repository with a
// Spindle workflow never asks for the foreign listing (the banner is only the
// "no Spindle files" hint).
func TestThreadsMigrationBannerHiddenWhenWorkflowsExist(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	foreignCalls := 0
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, _ threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
		Workflows: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{{
				Path: run.WorkflowPath, Name: run.Workflow, Triggers: []string{"push"},
			}}}, nil
		},
		Foreign: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.ForeignWorkflows, error) {
			foreignCalls++
			return &threads.ForeignWorkflows{Forgejo: []string{".forgejo/workflows/ci.yml"}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(bodyText(rec), "threads-migration-heading") {
		t.Fatal("the migration banner rendered although a Spindle workflow exists")
	}
	if foreignCalls != 0 {
		t.Fatalf("foreign seam calls = %d, want 0", foreignCalls)
	}
}

// TestThreadsMigrationBannerSurvivesAFailingHint: the hint is display-only, so
// a listing failure renders no banner rather than breaking the page.
func TestThreadsMigrationBannerSurvivesAFailingHint(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	BindThreads(ThreadsDeps{
		List: func(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int, _ threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{}, nil
		},
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
		Workflows: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{}, nil
		},
		Foreign: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.ForeignWorkflows, error) {
			return nil, threads.ErrUnavailable
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(bodyText(rec), "threads-migration-heading") {
		t.Fatal("the banner rendered from a failed read")
	}
}
