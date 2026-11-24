package webui

// threads_actions_test.go — the run page's step list shows a local composite
// action EXPANDED, in place (docs/threads.md §3.4a): the caller's inner steps
// with the caller's name as their parent, at desktop and ~390px. The layout
// check lives with the shared page chrome (one h1, no page-level horizontal
// pan); this test pins the rendered rows.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestThreadRunPageShowsExpandedCompositeSteps(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	job := threadsFixtureJob(run)
	job.Steps = []threads.Step{
		{Name: "checkout", Uses: "actions/checkout"},
		{Name: "install", Parent: "toolchain", Run: `install "$INPUT_VERSION"`},
		{Name: "image", Parent: "toolchain / nested", Uses: "docker://alpine:3.20"},
		{Name: "after", Run: "echo done"},
	}
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
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{
		"INPUT_VERSION",
		"docker://alpine:3.20",
		"toolchain /",
		"toolchain / nested /",
		// The inner rows carry the expanded marker (a shared layout class), so
		// a rendered check can see the nesting.
		`title="expanded from this composite action step"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page misses %q", want)
		}
	}
	// The caller's own `uses:` row is replaced by its expansion.
	if strings.Contains(body, "./.spindle/actions/toolchain") {
		t.Fatal("the composite caller row must not render beside its expansion")
	}
	// The step list is one <ul>, with one row per expanded step.
	if got := strings.Count(body, `<li class="flex flex-wrap items-center gap-2 px-3 py-1.5 text-xs`); got < 4 {
		t.Fatalf("rendered step rows = %d", got)
	}
}

// TestThreadRunPageExpandedStepsNarrow renders the same page in the narrow
// (mobile) layout fixture and asserts no page-level horizontal pan marker is
// introduced by the new row markup (the wrapper scrolls, the page does not).
func TestThreadRunPageExpandedStepsNarrow(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	job := threadsFixtureJob(run)
	job.Steps = []threads.Step{{Name: "install", Parent: "toolchain", Run: strings.Repeat("x", 400)}}
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
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `class="mono min-w-0 flex-1 truncate"`) {
		t.Fatal("a long step command must be truncated inside its row, not widen the page")
	}
}
