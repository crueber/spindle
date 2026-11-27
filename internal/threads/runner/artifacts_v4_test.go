package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// The v4 artifact inputs on the runner side (PAR-097/PAR-098), and the cache +
// artifact client wire shapes.

func TestParseUploadArtifactV4Inputs(t *testing.T) {
	p := mustPlanArtifacts(t, map[string]string{
		"path": "dist", "name": "out", "overwrite": "true",
		"include-hidden-files": "yes", "compression-level": "9",
	}, map[string]string{"path": "in"})
	up := p.Steps[0].Artifact
	if !up.Overwrite || !up.IncludeHidden || up.CompressionLevel == nil || *up.CompressionLevel != 9 {
		t.Fatalf("v4 upload = %+v", up)
	}
	// Defaults: no overwrite, hidden excluded, no explicit level.
	p = mustPlanArtifacts(t, map[string]string{"path": "dist"}, map[string]string{"path": "in"})
	if up := p.Steps[0].Artifact; up.Overwrite || up.IncludeHidden || up.CompressionLevel != nil {
		t.Fatalf("v4 defaults = %+v", up)
	}
	cases := map[string]string{
		"bad overwrite": "overwrite must be true or false",
		"bad hidden":    "include-hidden-files must be true or false",
		"bad level":     "compression-level must be 0",
	}
	withs := map[string]map[string]string{
		"bad overwrite": {"path": "d", "overwrite": "maybe"},
		"bad hidden":    {"path": "d", "include-hidden-files": "maybe"},
		"bad level":     {"path": "d", "compression-level": "12"},
	}
	for name, want := range cases {
		p := mustPlanArtifacts(t, withs[name], map[string]string{"path": "in"})
		if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, want) {
			t.Fatalf("%s = %+v", name, p.Steps[0])
		}
	}
}

func TestParseDownloadArtifactV4Inputs(t *testing.T) {
	p := mustPlanArtifacts(t, map[string]string{"path": "d"},
		map[string]string{"path": "in", "pattern": "dist-*", "merge-multiple": "true"})
	down := p.Steps[1].Artifact
	if down.Pattern != "dist-*" || !down.MergeMultiple || down.Name != "" {
		t.Fatalf("pattern download = %+v", down)
	}
	p = mustPlanArtifacts(t, map[string]string{"path": "d"},
		map[string]string{"path": "in", "artifact-ids": "12,34", "run-id": "1732104000000000001-a1b2c3"})
	down = p.Steps[1].Artifact
	if len(down.ArtifactIDs) != 2 || down.ArtifactIDs[0] != 12 || down.RunID != "1732104000000000001-a1b2c3" {
		t.Fatalf("ids download = %+v", down)
	}
	// No name: download all.
	p = mustPlanArtifacts(t, map[string]string{"path": "d"}, map[string]string{"path": "in"})
	if p.Steps[1].Artifact.Name != "" || p.Steps[1].Kind != "artifact-download" {
		t.Fatalf("download all = %+v", p.Steps[1])
	}
	// The refusals.
	refusals := map[string]map[string]string{
		"pattern must not be empty":                    {"pattern": " "},
		"merge-multiple needs pattern or artifact-ids": {"name": "a", "merge-multiple": "true"},
		"mutually exclusive":                           {"name": "a", "pattern": "b"},
		"github-token is not supported":                {"github-token": "x"},
		"repository is not supported":                  {"repository": "o/r"},
		"invalid pattern":                              {"pattern": "a/[b"},
		"artifact-ids must be a comma-separated":       {"artifact-ids": "x"},
		"artifact-ids must not be empty":               {"artifact-ids": " "},
		"run-id must not be empty":                     {"run-id": " "},
		"unsupported with key":                         {"nope": "1"},
		"merge-multiple must be true or false":         {"pattern": "x", "merge-multiple": "maybe"},
	}
	for want, with := range refusals {
		full := map[string]string{"path": "in"}
		for k, v := range with {
			full[k] = v
		}
		p := mustPlanArtifacts(t, map[string]string{"path": "d"}, full)
		if p.Steps[1].Kind != "unsupported" || !strings.Contains(p.Steps[1].SkipReason, want) {
			t.Fatalf("%s = %q (%s)", want, p.Steps[1].Kind, p.Steps[1].SkipReason)
		}
	}
}

func TestHiddenFileMatching(t *testing.T) {
	files := []string{"dist/a.txt", ".env", "dist/.hidden/b.txt", "src/.keep"}
	got, err := matchArtifactPaths(files, []string{"."}, false)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if strings.Join(got, ",") != "dist/a.txt" {
		t.Fatalf("hidden-excluded = %v", got)
	}
	got, _ = matchArtifactPaths(files, []string{"."}, true)
	if len(got) != 4 {
		t.Fatalf("hidden-included = %v", got)
	}
	if hasHiddenSegment("a/b") || !hasHiddenSegment("a/.b") {
		t.Fatal("hasHiddenSegment")
	}
}

// selectJob is one download step over a scripted artifact list.
func selectJob(with map[string]string) *threads.FetchResult {
	return &threads.FetchResult{
		Repo: "acme/mono", RunID: "1732104000000000001-a1b2c3", JobID: "j-build",
		JobRef: "jr_acme", Attempt: 1, Commit: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Event: "push", Branch: "main", Workflow: "CI", Trusted: true, Context: "threads/ci/build",
		Spec: &threads.Spec{Version: threads.SpecVersion, RunsOn: []string{"linux"},
			Steps: []threads.StepSpec{{Uses: "actions/download-artifact", With: with}}},
	}
}

func TestSelectArtifactByPatternIDsAndAll(t *testing.T) {
	list := []*threads.Artifact{
		{ID: 1, Name: "dist-a"}, {ID: 2, Name: "dist-b"}, {ID: 3, Name: "other"},
	}
	fc := &fakeArtifacts{list: list}
	ex := &Executor{Artifacts: fc}
	plan := &Plan{JobToken: "t", JobRef: "jr", Attempt: 1}

	// pattern
	p, _ := BuildPlan(PlanInput{Job: selectJob(map[string]string{"pattern": "dist-*"}), AttemptToken: "t"})
	got, err := ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact)
	if err != nil || len(got) != 2 || got[0].Name != "dist-a" || got[1].Name != "dist-b" {
		t.Fatalf("pattern = %v %v", got, err)
	}
	// ids
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{"artifact-ids": "3"}), AttemptToken: "t"})
	got, err = ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact)
	if err != nil || len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("ids = %v %v", got, err)
	}
	// all (sorted)
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{}), AttemptToken: "t"})
	got, err = ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact)
	if err != nil || len(got) != 3 || got[0].Name != "dist-a" || got[2].Name != "other" {
		t.Fatalf("all = %v %v", got, err)
	}
	// name
	fc.manifest = list[0]
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{"name": "dist-a"}), AttemptToken: "t"})
	got, err = ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact)
	if err != nil || len(got) != 1 || got[0].Name != "dist-a" {
		t.Fatalf("name = %v %v", got, err)
	}
	// A manifest that answers with another name is refused.
	fc.manifest = &threads.Artifact{ID: 9, Name: "wrong"}
	if _, err := ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact); err == nil {
		t.Fatal("a mismatched manifest name was accepted")
	}
	// A list failure is surfaced.
	fc.readErr = errors.New("down")
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{}), AttemptToken: "t"})
	if _, err := ex.selectArtifacts(context.Background(), plan, p.Steps[0].Artifact); err == nil {
		t.Fatal("a list failure was swallowed")
	}
	fc.readErr = nil
	// artifactSelector renders each shape.
	for _, art := range []*ArtifactStep{{Name: "a"}, {Pattern: "p"}, {ArtifactIDs: []int64{1}}, {}} {
		if artifactSelector(art) == "" {
			t.Fatalf("empty selector string for %+v", art)
		}
	}
}

func TestDownloadMultipleAndMergeMultiple(t *testing.T) {
	one := &threads.Artifact{ID: 1, Name: "dist-a", Files: []threads.ArtifactFile{{Path: "a.txt", Size: 1, Index: 0}}, TotalSize: 1}
	two := &threads.Artifact{ID: 2, Name: "dist-b", Files: []threads.ArtifactFile{{Path: "b.txt", Size: 1, Index: 0}}, TotalSize: 1}
	fc := &fakeArtifacts{list: []*threads.Artifact{one, two}, manifest: one, body: "a"}
	fd := &fakeDocker{}
	ex := &Executor{Docker: fd, Artifacts: fc}
	plan := &Plan{JobToken: "t", JobRef: "jr", Attempt: 1, Names: Names{Container: "c"}, Workspace: DefaultWorkspace,
		ArtifactCaps: threads.ArtifactCaps{MaxBytesPerRun: 100}}

	// Without merge-multiple each artifact lands in its own directory.
	p, _ := BuildPlan(PlanInput{Job: selectJob(map[string]string{"path": "in"}), AttemptToken: "t"})
	step := p.Steps[0]
	if err := ex.downloadArtifact(context.Background(), plan, step, &Streamer{}); err != nil {
		t.Fatalf("download all: %v", err)
	}
	if !fd.called("mkdir -p "+plan.Workspace+"/in/dist-a") || !fd.called("mkdir -p "+plan.Workspace+"/in/dist-b") {
		t.Fatalf("per-artifact dirs = %v", fd.snapshot())
	}
	// merge-multiple: one tree.
	fd = &fakeDocker{}
	ex.Docker = fd
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{"path": "in", "pattern": "dist-*", "merge-multiple": "true"}), AttemptToken: "t"})
	if err := ex.downloadArtifact(context.Background(), plan, p.Steps[0], &Streamer{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if fd.count("mkdir -p "+plan.Workspace+"/in") != 2 || fd.called("in/dist-a") {
		t.Fatalf("merged dirs = %v", fd.snapshot())
	}
	// A selection over the download cap is refused before any docker call.
	fd = &fakeDocker{}
	ex.Docker = fd
	plan.ArtifactCaps.MaxBytesPerRun = 1
	if err := ex.downloadArtifact(context.Background(), plan, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "download cap") {
		t.Fatalf("download cap = %v", err)
	}
	if fd.called("mkdir") {
		t.Fatal("an over-cap download created a directory")
	}
	plan.ArtifactCaps.MaxBytesPerRun = 100
	// A mkdir failure is named.
	ex.Docker = &fakeDocker{runOutputs: []runOutput{{match: "mkdir", errOut: "no space", code: 1}}}
	if err := ex.downloadArtifact(context.Background(), plan, p.Steps[0], &Streamer{}); err == nil {
		t.Fatal("mkdir failure swallowed")
	}
	// An empty selection is named.
	ex.Artifacts = &fakeArtifacts{list: nil}
	p, _ = BuildPlan(PlanInput{Job: selectJob(map[string]string{"pattern": "zzz"}), AttemptToken: "t"})
	if err := ex.downloadArtifact(context.Background(), plan, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "no artifact matched") {
		t.Fatalf("empty selection = %v", err)
	}
	// No transport / no plan.
	if err := (&Executor{}).downloadArtifact(context.Background(), plan, step, &Streamer{}); err == nil {
		t.Fatal("no transport swallowed")
	}
	if err := (&Executor{Artifacts: fc}).downloadArtifact(context.Background(), plan, StepPlan{}, &Streamer{}); err == nil {
		t.Fatal("no plan swallowed")
	}
}

func TestZipSlipManifestPathIsRejected(t *testing.T) {
	// A crafted manifest (a corrupted store) names an escaping path.
	evil := &threads.Artifact{ID: 1, Name: "evil", Files: []threads.ArtifactFile{
		{Path: "../../etc/passwd", Size: 1, Index: 0}}, TotalSize: 1}
	fc := &fakeArtifacts{manifest: evil, body: "x"}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- writeArtifactTarFromRun(context.Background(), pw, fc,
			&Plan{JobToken: "t", JobRef: "jr", Attempt: 1}, "", evil)
	}()
	_, _ = io.Copy(io.Discard, pr)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("zip-slip = %v", err)
	}
	// cleanArchivePath refuses every escape shape.
	for _, p := range []string{"", "/abs", `a\b`, "../x", "..", "a/../../b", "a\x00b"} {
		if _, err := cleanArchivePath(p); err == nil {
			t.Fatalf("%q accepted", p)
		}
	}
	if got, err := cleanArchivePath("dist/a.txt"); err != nil || got != "dist/a.txt" {
		t.Fatalf("clean = %q %v", got, err)
	}
	// A blob read that ends short aborts the tar.
	short := &threads.Artifact{Name: "short", Files: []threads.ArtifactFile{{Path: "s.txt", Size: 9, Index: 0}}, TotalSize: 9}
	fc = &fakeArtifacts{manifest: short, body: "x"}
	pr, pw = io.Pipe()
	go func() {
		done <- writeArtifactTarFromRun(context.Background(), pw, fc,
			&Plan{JobToken: "t", JobRef: "jr", Attempt: 1}, "", short)
	}()
	_, _ = io.Copy(io.Discard, pr)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "manifest says") {
		t.Fatalf("short blob = %v", err)
	}
}

func TestUploadArtifactOverwriteAndHidden(t *testing.T) {
	// The upload path calls prepare for an overwrite and passes the v4 inputs to
	// finalize.
	payload := tarFixture([][2]string{{"dist/a.txt", "alpha"}})
	fd := &fakeDocker{streamInOutput: payload}
	fc := newFakeArtifacts()
	ex := &Executor{Docker: fd, Artifacts: fc}
	p := mustPlanArtifacts(t, map[string]string{
		"path": "dist", "name": "out", "overwrite": "true", "compression-level": "4", "include-hidden-files": "true",
	}, map[string]string{"path": "in"})
	step := p.Steps[0]
	step.Artifact.Patterns = []string{"dist"}
	// listWorkspace runs `find`; the fake answers with the fixture listing.
	fd.runOutputs = []runOutput{{match: "find . -type f", out: "./dist/a.txt\n"}}
	if err := ex.uploadArtifact(context.Background(), p, step, &Streamer{}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if fc.prepared != 1 || fc.preparedOverwrite != true {
		t.Fatalf("prepare = %d %v", fc.prepared, fc.preparedOverwrite)
	}
	if fc.finalized == nil || !fc.finalized.Overwrite || fc.finalized.CompressionLevel == nil || *fc.finalized.CompressionLevel != 4 {
		t.Fatalf("finalize input = %+v", fc.finalized)
	}
}

// --- client wire shapes ------------------------------------------------------

func TestClientCacheAndArtifactV4Endpoints(t *testing.T) {
	type seen struct {
		method, path, query string
		body                []byte
	}
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.Path, r.URL.RawQuery, body})
		switch {
		case strings.HasSuffix(r.URL.Path, "/cache/lookup"):
			_ = json.NewEncoder(w).Encode(threads.CacheView{Hit: true, Exact: true, Key: "k", SHA256: strings.Repeat("a", 64), Size: 3})
		case strings.HasSuffix(r.URL.Path, "/cache/save"):
			_ = json.NewEncoder(w).Encode(threads.CacheView{Hit: true, Key: "k"})
		case strings.Contains(r.URL.Path, "/cache/blobs/"):
			_, _ = w.Write([]byte("abc"))
		case strings.HasSuffix(r.URL.Path, "/prepare"):
			_ = json.NewEncoder(w).Encode(map[string]any{"existed": true})
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []map[string]any{{"name": "a"}}})
		case strings.Contains(r.URL.Path, "/artifacts/") && strings.Contains(r.URL.Path, "/blobs/"):
			_, _ = w.Write([]byte("x"))
		case strings.Contains(r.URL.Path, "/artifacts/"):
			_ = json.NewEncoder(w).Encode(threads.Artifact{Name: "a"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	ctx := context.Background()

	hit, err := c.CacheLookup(ctx, "tok", "jr", 1, threads.CacheLookupInput{Key: "k", Version: "v"})
	if err != nil || !hit.Hit || !hit.Exact {
		t.Fatalf("CacheLookup = %+v %v", hit, err)
	}
	body, size, err := c.GetCacheBlob(ctx, "tok", "jr", 1, strings.Repeat("a", 64))
	if err != nil || size != 3 {
		t.Fatalf("GetCacheBlob = %d %v", size, err)
	}
	raw, _ := io.ReadAll(body)
	_ = body.Close()
	if string(raw) != "abc" {
		t.Fatalf("blob body = %q", raw)
	}
	if err := c.PutCacheBlob(ctx, "tok", "jr", 1, strings.Repeat("a", 64), 3, strings.NewReader("abc")); err != nil {
		t.Fatalf("PutCacheBlob: %v", err)
	}
	if _, err := c.SaveCache(ctx, "tok", "jr", 1, threads.SaveCacheInput{Key: "k", SHA256: strings.Repeat("a", 64), Size: 3}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if existed, err := c.PrepareArtifact(ctx, "tok", "jr", 1, "build", true); err != nil || !existed {
		t.Fatalf("PrepareArtifact = %v %v", existed, err)
	}
	list, err := c.ArtifactList(ctx, "tok", "jr", 1, "srcrun")
	if err != nil || len(list) != 1 || list[0].Name != "a" {
		t.Fatalf("ArtifactList = %v %v", list, err)
	}
	man, err := c.ArtifactManifestFromRun(ctx, "tok", "jr", 1, "srcrun", "a")
	if err != nil || man.Name != "a" {
		t.Fatalf("ArtifactManifestFromRun = %v %v", man, err)
	}
	blob, _, err := c.GetArtifactBlobFromRun(ctx, "tok", "jr", 1, "srcrun", "a", 0)
	if err != nil {
		t.Fatalf("GetArtifactBlobFromRun: %v", err)
	}
	_, _ = io.Copy(io.Discard, blob)
	_ = blob.Close()

	// The paths and queries carry the documented parameters.
	want := map[string]string{
		"POST /api/v1/threads/jobs/jr/attempt/1/cache/lookup":                          "",
		"GET /api/v1/threads/jobs/jr/attempt/1/cache/blobs/" + strings.Repeat("a", 64): "",
		"PUT /api/v1/threads/jobs/jr/attempt/1/cache/blobs/" + strings.Repeat("a", 64): "size=3",
		"POST /api/v1/threads/jobs/jr/attempt/1/cache/save":                            "",
		"POST /api/v1/threads/jobs/jr/attempt/1/artifacts/build/prepare":               "",
		"GET /api/v1/threads/jobs/jr/attempt/1/artifacts":                              "run_id=srcrun",
		"GET /api/v1/threads/jobs/jr/attempt/1/artifacts/a":                            "run_id=srcrun",
		"GET /api/v1/threads/jobs/jr/attempt/1/artifacts/a/blobs/0":                    "run_id=srcrun",
	}
	for _, s := range got {
		key := s.method + " " + s.path
		if wantQuery, ok := want[key]; ok {
			if s.query != wantQuery {
				t.Fatalf("%s query = %q, want %q", key, s.query, wantQuery)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatalf("unseen endpoints: %v (got %v)", want, got)
	}

	// A non-2xx is the API error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer bad.Close()
	c = NewClient(bad.URL, "tok")
	if _, err := c.CacheLookup(ctx, "tok", "jr", 1, threads.CacheLookupInput{}); err == nil {
		t.Fatal("CacheLookup swallowed a 404")
	}
	if _, _, err := c.GetCacheBlob(ctx, "tok", "jr", 1, strings.Repeat("a", 64)); err == nil {
		t.Fatal("GetCacheBlob swallowed a 404")
	}
	if err := c.PutCacheBlob(ctx, "tok", "jr", 1, strings.Repeat("a", 64), 1, strings.NewReader("x")); err == nil {
		t.Fatal("PutCacheBlob swallowed a 404")
	}
	if _, err := c.SaveCache(ctx, "tok", "jr", 1, threads.SaveCacheInput{}); err == nil {
		t.Fatal("SaveCache swallowed a 404")
	}
	if _, err := c.PrepareArtifact(ctx, "tok", "jr", 1, "a", false); err == nil {
		t.Fatal("PrepareArtifact swallowed a 404")
	}
	if _, err := c.ArtifactList(ctx, "tok", "jr", 1, ""); err == nil {
		t.Fatal("ArtifactList swallowed a 404")
	}
	if _, err := c.ArtifactManifestFromRun(ctx, "tok", "jr", 1, "", "a"); err == nil {
		t.Fatal("ArtifactManifestFromRun swallowed a 404")
	}
	if _, _, err := c.GetArtifactBlobFromRun(ctx, "tok", "jr", 1, "", "a", 0); err == nil {
		t.Fatal("GetArtifactBlobFromRun swallowed a 404")
	}
	// A URL that cannot be built is a named error (an invalid base URL).
	c = &Client{BaseURL: "http://[::1", HTTP: &http.Client{}}
	if _, err := c.CacheLookup(ctx, "t", "j", 1, threads.CacheLookupInput{}); err == nil {
		t.Fatal("an invalid base URL was accepted")
	}
	if _, _, err := c.GetCacheBlob(ctx, "t", "j", 1, "sha"); err == nil {
		t.Fatal("an invalid base URL was accepted (blob)")
	}
	if err := c.PutCacheBlob(ctx, "t", "j", 1, "sha", 1, strings.NewReader("x")); err == nil {
		t.Fatal("an invalid base URL was accepted (put)")
	}
	if _, err := c.SaveCache(ctx, "t", "j", 1, threads.SaveCacheInput{}); err == nil {
		t.Fatal("an invalid base URL was accepted (save)")
	}
	if _, err := c.PrepareArtifact(ctx, "t", "j", 1, "a", true); err == nil {
		t.Fatal("an invalid base URL was accepted (prepare)")
	}
	if _, err := c.ArtifactList(ctx, "t", "j", 1, ""); err == nil {
		t.Fatal("an invalid base URL was accepted (list)")
	}
	if _, err := c.ArtifactManifestFromRun(ctx, "t", "j", 1, "", "a"); err == nil {
		t.Fatal("an invalid base URL was accepted (manifest)")
	}
	if _, _, err := c.GetArtifactBlobFromRun(ctx, "t", "j", 1, "", "a", 0); err == nil {
		t.Fatal("an invalid base URL was accepted (blob get)")
	}
	_ = fmt.Sprint()
}
