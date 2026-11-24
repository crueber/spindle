package webui

// threads_artifacts_test.go — the run page's artifact section and the ZIP
// download route (docs/features/15 §2.5, docs/threads.md §3.4). The page adds
// no fixed-width element: the rows compose the shared card/link/muted classes,
// so the 1440px and ~390px checks are the layout proof (rendered separately),
// and the structural assertions here pin names, sizes, links and headers.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
)

// artifactStore seeds one run with one committed artifact ("build", two files)
// into a memory store and returns the service that can open it.
func artifactStore(t *testing.T, run *threads.Run) *threads.Service {
	t.Helper()
	mem := store.NewMemory()
	svc := threads.New(mem, nil)
	ctx := context.Background()
	runRaw, _ := json.Marshal(run)
	if _, err := store.PutBytes(ctx, mem, threads.RunKey("acme", "widgets", run.ID), runRaw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	art := threads.Artifact{
		Name: "build", Run: run.ID, Job: "j-build", Attempt: 1,
		Files: []threads.ArtifactFile{
			{Path: "dist/a.txt", Size: 5, SHA256: strings.Repeat("a", 64), Index: 0},
			{Path: "dist/sub/b.txt", Size: 4, SHA256: strings.Repeat("b", 64), Index: 1},
		},
		TotalSize: 9, CreatedAt: "2026-11-19T11:59:00Z", Trusted: true,
	}
	raw, _ := json.Marshal(art)
	if _, err := store.PutBytes(ctx, mem, threads.ArtifactManifestKey("acme", "widgets", run.ID, "build"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	for _, f := range art.Files {
		body := map[int]string{0: "alpha", 1: "beta"}[f.Index]
		if _, err := store.PutBytes(ctx, mem, threads.ArtifactBlobKey("acme", "widgets", run.ID, "build", 1, f.Index),
			[]byte(body), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
	}
	return svc
}

func TestThreadsRunPageListsArtifacts(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	bindThreadsFixture(t, run)
	// The fixture's Get has no artifacts; re-bind with one so the section is
	// exercised (the real service populates this field in GetRun).
	job := threadsFixtureJob(run)
	BindThreads(ThreadsDeps{
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}, Artifacts: []*threads.ArtifactView{{
				Artifact: threads.Artifact{
					Name: "build", TotalSize: 9, CreatedAt: "2026-11-19T11:59:00Z",
					Files: []threads.ArtifactFile{{Path: "a"}, {Path: "b"}},
				},
				ExpiresAt: "2026-12-19T11:59:00Z",
			}}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	for _, want := range []string{
		"Artifacts", "build",
		"/acme/widgets/threads/" + run.ID + "/artifacts/build",
		"2 files",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page misses %q", want)
		}
	}
	// The artifact section is links only: no primary button is added inside it
	// (the page keeps its one `.btn.primary`).
	start := strings.Index(body, `aria-label="Artifacts"`)
	end := strings.Index(body[start:], "</section>")
	if start < 0 || end < 0 {
		t.Fatalf("artifact section not found")
	}
	section := body[start : start+end]
	if strings.Contains(section, "btn primary") {
		t.Fatalf("artifact section carries a primary button")
	}
	if !strings.Contains(section, "download") {
		t.Fatalf("artifact link is not a download link")
	}
}

func TestThreadsArtifactDownloadServesZip(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	svc := artifactStore(t, run)
	BindThreads(ThreadsDeps{
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
		OpenArtifact: func(ctx context.Context, _, _, runID, name string, p auth.Principal) (*threads.ArtifactDownload, error) {
			return svc.OpenArtifact(ctx, "acme/widgets", runID, name, p)
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	f.principal = auth.Principal{Name: "ada"} // a repository reader, never anonymous

	rec := httptestResponse(f, "GET", "/acme/widgets/threads/"+run.ID+"/artifacts/build")
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "build.zip") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	got := map[string]string{}
	for _, entry := range zr.File {
		rc, oerr := entry.Open()
		if oerr != nil {
			t.Fatalf("entry %s: %v", entry.Name, oerr)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[entry.Name] = string(b)
	}
	if got["dist/a.txt"] != "alpha" || got["dist/sub/b.txt"] != "beta" {
		t.Fatalf("zip entries = %v", got)
	}

	// An unknown name is a 404 (never an empty archive).
	rec = httptestResponse(f, "GET", "/acme/widgets/threads/"+run.ID+"/artifacts/missing")
	wantStatus(t, rec, http.StatusNotFound)
	// A service error that is not a sentinel is a 503, never a partial zip.
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
		OpenArtifact: func(context.Context, string, string, string, string, auth.Principal) (*threads.ArtifactDownload, error) {
			return nil, io.ErrUnexpectedEOF
		},
	})
	wantStatus(t, httptestResponse(f, "GET", "/acme/widgets/threads/"+run.ID+"/artifacts/build"), http.StatusServiceUnavailable)
}

func TestThreadsArtifactDownloadRequiresWiring(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	BindThreads(ThreadsDeps{
		Get: func(_ context.Context, _, _, _ string, _ auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	wantStatus(t, httptestResponse(f, "GET", "/acme/widgets/threads/"+run.ID+"/artifacts/build"),
		http.StatusServiceUnavailable)
}

// httptestResponse drives one page request through the provider.
func httptestResponse(f *collabFixture, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, f.req(method, path, nil)) {
		rec.Code = http.StatusNotFound
	}
	return rec
}
