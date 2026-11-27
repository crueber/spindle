package webui

// PAR-191/PAR-190 on the run page: the job graph (levels, matrix cells, blocked
// dependents) and the whole-run log download are rendered from the service's
// own read models. The graph is HTML (links + named edges), not an image, so it
// is keyboard reachable and reads without colour.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestThreadRunPageRendersTheJobGraph(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunFailure
	buildA := threadsFixtureJob(run)
	buildA.ID, buildA.Key, buildA.Name = "j-build-0", "build (1.22)", "build (1.22)"
	buildA.Context, buildA.State = "threads/ci/build (1.22)", threads.JobSuccess
	buildA.DurationSeconds = 12
	buildA.Spec = &threads.Spec{Matrix: map[string]string{"go": "1.22"}, MatrixGroup: "build", MatrixTotal: 2}
	buildB := threadsFixtureJob(run)
	buildB.ID, buildB.Key, buildB.Name = "j-build-1", "build (1.23)", "build (1.23)"
	buildB.Context, buildB.State = "threads/ci/build (1.23)", threads.JobFailure
	buildB.Spec = &threads.Spec{Matrix: map[string]string{"go": "1.23"}, MatrixGroup: "build", MatrixTotal: 2}
	test := threadsFixtureJob(run)
	test.ID, test.Key, test.Name = "j-test", "test", "test"
	test.Context, test.State = "threads/ci/test", threads.JobSkipped
	test.Needs = []string{"build"}
	test.Reason = "needs: build did not succeed"
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			// Graph nil: the page must derive the same DAG from the jobs (no
			// second source of truth, and an older service still renders).
			return &threads.RunView{Run: run, Jobs: []*threads.Job{buildA, buildB, test}}, nil
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
	wantContains(t, rec,
		"Job graph",
		"level 0",
		"level 1",
		`id="graph-build"`,
		`href="#graph-build"`,
		"cells (2)",
		"go=1.22",
		"threads/ci/build (1.22)",
		"12s",
		"blocked by",
		`id="job-j-build-0"`,
		"/acme/widgets/threads/"+run.ID+"/logs.zip",
		"/acme/widgets/threads/"+run.ID+"/logs.txt",
		`role="region"`,
		`tabindex="0"`,
		`aria-live="polite"`,
	)
	// The blocked dependent names its dependency and the state that held it —
	// the fact is text, never a hue alone.
	if !strings.Contains(body, "(failure)") {
		t.Fatalf("the blocked edge does not name its dependency's state:\n%s", body)
	}
	// The blocked chip alone is not the only signal: the sentence says so.
	if !strings.Contains(body, "cannot run until every dependency succeeds") {
		t.Fatalf("the blocked dependent has no explanation:\n%s", body)
	}
}

func TestThreadRunGraphModelGroupsAndLinksCells(t *testing.T) {
	run := threadsFixtureRun()
	a := threadsFixtureJob(run)
	a.ID, a.Key, a.Name = "j-a-0", "a (1)", "a (1)"
	a.Spec = &threads.Spec{Matrix: map[string]string{"x": "1", "y": "2"}, MatrixGroup: "a", MatrixIndex: 0, MatrixTotal: 2}
	b := threadsFixtureJob(run)
	b.ID, b.Key, b.Name = "j-a-1", "a (2)", "a (2)"
	b.Spec = &threads.Spec{Matrix: map[string]string{"x": "2", "y": "2"}, MatrixGroup: "a", MatrixIndex: 1, MatrixTotal: 2}
	dep := threadsFixtureJob(run)
	dep.ID, dep.Key, dep.Name = "j-b", "b", "b"
	dep.Needs = []string{"a"}
	model := threadRunGraphModel(&threads.RunView{Run: run, Jobs: []*threads.Job{a, b, dep}})
	if model == nil || len(model.Levels) != 2 {
		t.Fatalf("model = %+v", model)
	}
	if len(model.Levels[0].Nodes) != 1 || !model.Levels[0].Nodes[0].Multi {
		t.Fatalf("level 0 = %+v", model.Levels[0])
	}
	node := model.Levels[0].Nodes[0]
	if node.Href != "#job-j-a-0" || len(node.Cells) != 2 {
		t.Fatalf("node = %+v", node)
	}
	if got := node.Cells[0].Matrix; got != "x=1 · y=2" {
		t.Fatalf("matrix label = %q", got)
	}
	if model.Levels[1].Nodes[0].Href != "#job-j-b" {
		t.Fatalf("dependent href = %q", model.Levels[1].Nodes[0].Href)
	}
}

func TestRunLogsDownloadGatesAndStreams(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bundle := &threads.RunLogBundle{
		Run: run,
		Jobs: []threads.JobLog{{
			JobID: "j-build", Name: "build", State: threads.JobSuccess, Attempt: 1,
			Frames: []threads.LogFrame{{Seq: 1, Attempt: 1, Time: "2026-11-19T11:59:01Z", Stream: "output", Text: "hello\n"}},
		}},
	}
	BindThreads(ThreadsDeps{
		RunLogs: func(context.Context, string, string, string, auth.Principal) (*threads.RunLogBundle, error) {
			return bundle, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	for _, tc := range []struct{ path, ctype string }{
		{"/acme/widgets/threads/" + run.ID + "/logs.txt", "text/plain; charset=utf-8"},
		{"/acme/widgets/threads/" + run.ID + "/logs.zip", "application/zip"},
	} {
		rec := f.get(tc.path)
		wantStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Content-Type"); got != tc.ctype {
			t.Fatalf("%s content-type = %q, want %q", tc.path, got, tc.ctype)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Fatalf("%s disposition = %q", tc.path, cd)
		}
		if strings.HasSuffix(tc.path, ".txt") && !strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("text download body = %q", rec.Body.String())
		}
	}
	// An unwired seam is a real 503, not an empty 200 archive.
	BindThreads(ThreadsDeps{})
	rec := f.get("/acme/widgets/threads/" + run.ID + "/logs.zip")
	wantStatus(t, rec, http.StatusServiceUnavailable)
}
