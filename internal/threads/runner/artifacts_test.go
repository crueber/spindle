package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Built-in artifact steps (15 §2.5): planning, glob expansion,
// if-no-files-found, caps, and the tar streams in both directions.

// streamText reads a streamer's buffered log (test helper).
func streamText(s *Streamer) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// artifactJob is a fetch payload with the two artifact steps.
func artifactJob(uploadWith, downloadWith map[string]string) *threads.FetchResult {
	return &threads.FetchResult{
		Repo: "acme/mono", RunID: "1732104000000000001-a1b2c3", JobID: "j-build",
		JobRef: "jr_acme", Attempt: 2, Commit: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Event: "push", Branch: "main", Workflow: "CI", Trusted: true,
		Context: "threads/ci/build",
		Spec: &threads.Spec{
			Version: threads.SpecVersion, RunsOn: []string{"linux"},
			Steps: []threads.StepSpec{
				{Uses: "actions/upload-artifact", With: uploadWith},
				{Uses: "actions/download-artifact", With: downloadWith},
			},
		},
	}
}

func mustPlanArtifacts(t *testing.T, uploadWith, downloadWith map[string]string) *Plan {
	t.Helper()
	p, err := BuildPlan(PlanInput{Job: artifactJob(uploadWith, downloadWith), AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestPlanArtifactSteps(t *testing.T) {
	p := mustPlanArtifacts(t,
		map[string]string{"name": "build-out", "path": "dist\ncoverage/*.out", "if-no-files-found": "error", "retention-days": "7"},
		map[string]string{"name": "build-out", "path": "in"})
	up, down := p.Steps[0], p.Steps[1]
	if up.Kind != "artifact-upload" || up.Artifact == nil {
		t.Fatalf("upload step = %+v", up)
	}
	if up.Artifact.Name != "build-out" || len(up.Artifact.Patterns) != 2 ||
		up.Artifact.Patterns[1] != "coverage/*.out" || up.Artifact.IfNoFilesFound != "error" || up.Artifact.RetentionDays != 7 {
		t.Fatalf("upload artifact spec = %+v", up.Artifact)
	}
	if down.Kind != "artifact-download" || down.Artifact.Name != "build-out" || down.Artifact.Destination != DefaultWorkspace+"/in" {
		t.Fatalf("download step = %+v (%+v)", down, down.Artifact)
	}
	// Defaults: name "artifact", if-no-files-found warn.
	p = mustPlanArtifacts(t, map[string]string{"path": "dist"}, map[string]string{})
	if p.Steps[0].Artifact.Name != "artifact" || p.Steps[0].Artifact.IfNoFilesFound != "warn" {
		t.Fatalf("defaults = %+v", p.Steps[0].Artifact)
	}
	if p.Steps[1].Artifact.Destination != DefaultWorkspace {
		t.Fatalf("download default destination = %q", p.Steps[1].Artifact.Destination)
	}
}

func TestPlanArtifactStepNamedErrors(t *testing.T) {
	cases := []struct {
		name  string
		job   *threads.FetchResult
		step  int
		wants string
	}{
		{"unknown upload key", artifactJob(map[string]string{"path": "d", "retention_day": "7"}, nil), 0, `unsupported with key "retention_day"`},
		{"missing path", artifactJob(map[string]string{"name": "x"}, nil), 0, "path is required"},
		{"bad if-no-files-found", artifactJob(map[string]string{"path": "d", "if-no-files-found": "explode"}, nil), 0, "if-no-files-found must be warn, error or ignore"},
		{"retention out of range", artifactJob(map[string]string{"path": "d", "retention-days": "91"}, nil), 0, "retention-days must be 0"},
		{"retention not a number", artifactJob(map[string]string{"path": "d", "retention-days": "soon"}, nil), 0, "must be an integer"},
		{"bad name", artifactJob(map[string]string{"path": "d", "name": "a/b"}, nil), 0, "must use letters"},
		{"absolute path", artifactJob(map[string]string{"path": "/etc/passwd"}, nil), 0, "must be relative to the workspace"},
		{"climbing path", artifactJob(map[string]string{"path": "../secret"}, nil), 0, "escapes the workspace"},
		{"unknown download key", artifactJob(map[string]string{"path": "d"}, map[string]string{"pattern": "x"}), 1, `unsupported with key "pattern"`},
		{"climbing destination", artifactJob(map[string]string{"path": "d"}, map[string]string{"path": "../../etc"}), 1, "escapes the workspace"},
	}
	for _, tc := range cases {
		p, err := BuildPlan(PlanInput{Job: tc.job, AttemptToken: "wjt_test"})
		if err != nil {
			t.Fatalf("%s: BuildPlan: %v", tc.name, err)
		}
		step := p.Steps[tc.step]
		if step.Kind != "unsupported" || !strings.Contains(step.SkipReason, tc.wants) {
			t.Fatalf("%s = %q (%s), want %q", tc.name, step.Kind, step.SkipReason, tc.wants)
		}
	}
	// A marketplace action still says so.
	p, _ := BuildPlan(PlanInput{Job: &threads.FetchResult{Spec: &threads.Spec{Version: threads.SpecVersion, RunsOn: []string{"x"},
		Steps: []threads.StepSpec{{Uses: "actions/setup-node"}}}}, AttemptToken: "t"})
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "no marketplace") {
		t.Fatalf("marketplace step = %+v", p.Steps[0])
	}
}

func TestMatchArtifactPaths(t *testing.T) {
	files := []string{
		"dist/a.txt", "dist/app.js", "dist/sub/b.txt", "dist/sub/deep/c.js",
		"notes.md", "coverage/total.out", "coverage/unit.out", "src/main.go",
	}
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"exact file", []string{"notes.md"}, []string{"notes.md"}},
		{"directory", []string{"dist"}, []string{"dist/a.txt", "dist/app.js", "dist/sub/b.txt", "dist/sub/deep/c.js"}},
		{"single-segment glob", []string{"dist/*.js"}, []string{"dist/app.js"}},
		{"recursive glob", []string{"dist/**/*.js"}, []string{"dist/app.js", "dist/sub/deep/c.js"}},
		{"double star covers zero segments", []string{"dist/**/b.txt"}, []string{"dist/sub/b.txt"}},
		{"question mark", []string{"dist/a.?xt"}, []string{"dist/a.txt"}},
		{"character class", []string{"dist/[ab].txt"}, []string{"dist/a.txt"}},
		{"multi-line union, deduped and sorted", []string{"coverage/*.out", "notes.md", "dist/a.txt"}, []string{"coverage/total.out", "coverage/unit.out", "dist/a.txt", "notes.md"}},
		{"workspace root", []string{"."}, []string{
			"coverage/total.out", "coverage/unit.out", "dist/a.txt", "dist/app.js", "dist/sub/b.txt",
			"dist/sub/deep/c.js", "notes.md", "src/main.go"}},
		{"no match", []string{"nothing/*"}, nil},
		{"directory matched by a glob includes its files", []string{"dist/*"}, []string{
			"dist/a.txt", "dist/app.js", "dist/sub/b.txt", "dist/sub/deep/c.js"}},
	}
	for _, tc := range cases {
		got, err := matchArtifactPaths(files, tc.patterns)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := matchArtifactPaths(files, []string{"dist/[abc"}); err == nil {
		t.Fatal("unterminated character class accepted")
	}
}

// fakeArtifacts is the artifact transport double.
type fakeArtifacts struct {
	mu        sync.Mutex
	uploaded  map[string]string // path → bytes
	blobs     []threads.ArtifactFile
	finalized *threads.ArtifactFinalizeInput
	manifest  *threads.Artifact
	readErr   error
}

func newFakeArtifacts() *fakeArtifacts {
	return &fakeArtifacts{uploaded: map[string]string{}}
}

func (f *fakeArtifacts) PutArtifactBlob(_ context.Context, _, _ string, _ int, _ string, index int, filePath string, size int64, body io.Reader) (*threads.ArtifactBlobView, error) {
	b, err := io.ReadAll(io.LimitReader(body, size))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size {
		return nil, fmt.Errorf("short body: %d of %d", len(b), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded[filePath] = string(b)
	file := threads.ArtifactFile{Path: filePath, Size: size, SHA256: "digest", Index: index}
	f.blobs = append(f.blobs, file)
	return &threads.ArtifactBlobView{Index: index, Size: size, SHA256: "digest"}, nil
}

func (f *fakeArtifacts) FinalizeArtifact(_ context.Context, _, _ string, _ int, in threads.ArtifactFinalizeInput) (*threads.ArtifactView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := in
	f.finalized = &cp
	return &threads.ArtifactView{Artifact: threads.Artifact{Name: in.Name, Files: in.Files}}, nil
}

func (f *fakeArtifacts) ArtifactManifest(_ context.Context, _, _ string, _ int, _ string) (*threads.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.manifest, nil
}

func (f *fakeArtifacts) GetArtifactBlob(_ context.Context, _, _ string, _ int, name string, index int) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifest == nil {
		return nil, 0, fmt.Errorf("no manifest")
	}
	for _, file := range f.manifest.Files {
		if file.Index == index {
			body := f.uploaded[file.Path]
			return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
		}
	}
	return nil, 0, fmt.Errorf("no file %d", index)
}

// tarFixture builds a tar carrying path → bytes in a fixed order.
func tarFixture(files [][2]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func artifactPlan(t *testing.T, job *threads.FetchResult) *Plan {
	t.Helper()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestUploadArtifactStreamsTarAndFinalizes(t *testing.T) {
	docker := &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a.txt\n./dist/sub/b.txt\n./README.md\n"}},
		streamInOutput: tarFixture([][2]string{{"dist/a.txt", "alpha"}, {"dist/sub/b.txt", "beta"}}),
	}
	art := newFakeArtifacts()
	ex := &Executor{Docker: docker, Artifacts: art}
	job := artifactJob(map[string]string{"name": "build", "path": "dist"}, nil)
	plan := artifactPlan(t, job)
	stream := &Streamer{}
	if err := ex.uploadArtifact(context.Background(), plan, plan.Steps[0], stream); err != nil {
		t.Fatalf("upload: %v\n%s", err, streamText(stream))
	}
	if got := art.uploaded["dist/a.txt"]; got != "alpha" {
		t.Fatalf("uploaded a = %q", got)
	}
	if got := art.uploaded["dist/sub/b.txt"]; got != "beta" {
		t.Fatalf("uploaded b = %q", got)
	}
	if _, ok := art.uploaded["README.md"]; ok {
		t.Fatal("a file outside the pattern was uploaded")
	}
	if art.finalized == nil || art.finalized.Name != "build" || len(art.finalized.Files) != 2 {
		t.Fatalf("finalize = %+v", art.finalized)
	}
	if !docker.called("exec -i " + plan.Names.Container + " tar -cf - -C " + DefaultWorkspace) {
		t.Fatalf("tar argv missing: %v", docker.snapshot())
	}
	if !strings.Contains(streamText(stream), `uploaded artifact "build": 2 file(s)`) {
		t.Fatalf("log missing the upload notice:\n%s", streamText(stream))
	}
}

func TestUploadArtifactIfNoFilesFound(t *testing.T) {
	job := artifactJob(map[string]string{"path": "dist"}, nil)
	plan := artifactPlan(t, job)
	for _, tc := range []struct {
		mode    string
		wantErr bool
	}{
		{"warn", false},
		{"ignore", false},
		{"error", true},
	} {
		with := map[string]string{"path": "dist", "if-no-files-found": tc.mode}
		p := artifactPlan(t, artifactJob(with, nil))
		ex := &Executor{Docker: &fakeDocker{runOutputs: []runOutput{{match: "find . -type f", out: "./other.txt\n"}}}, Artifacts: newFakeArtifacts()}
		stream := &Streamer{}
		err := ex.uploadArtifact(context.Background(), p, p.Steps[0], stream)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s = %v, want error %v", tc.mode, err, tc.wantErr)
		}
		if tc.wantErr && !strings.Contains(err.Error(), "no files were found") {
			t.Fatalf("%s error = %v", tc.mode, err)
		}
		if !tc.wantErr && !strings.Contains(streamText(stream), "if-no-files-found: "+tc.mode) {
			t.Fatalf("%s log = %q", tc.mode, streamText(stream))
		}
	}
	// No tar and no finalize happened when nothing matched.
	docker := &fakeDocker{runOutputs: []runOutput{{match: "find . -type f", out: ""}}}
	art := newFakeArtifacts()
	ex := &Executor{Docker: docker, Artifacts: art}
	_ = ex.uploadArtifact(context.Background(), plan, plan.Steps[0], &Streamer{})
	if docker.called("tar -cf") || art.finalized != nil {
		t.Fatalf("an empty artifact was uploaded: %v", docker.snapshot())
	}
}

func TestUploadArtifactCapsAndToolFailures(t *testing.T) {
	job := artifactJob(map[string]string{"path": "dist"}, nil)
	p := artifactPlan(t, job)
	p.ArtifactCaps = threads.ArtifactCaps{MaxBytesPerRun: 8, MaxBytesPerFile: 4, MaxFiles: 10}
	docker := &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a.txt\n"}},
		streamInOutput: tarFixture([][2]string{{"dist/a.txt", "alpha!"}}), // 6 bytes > 4
	}
	art := newFakeArtifacts()
	ex := &Executor{Docker: docker, Artifacts: art}
	err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{})
	if err == nil || !strings.Contains(err.Error(), "max_artifact_bytes_per_file") {
		t.Fatalf("per-file cap = %v", err)
	}
	if art.finalized != nil {
		t.Fatal("an over-cap artifact was finalized")
	}
	// The listing tool is missing (a distroless image): a named error.
	docker = &fakeDocker{runOutputs: []runOutput{{match: "find . -type f", out: "find: not found", code: 127}}}
	ex = &Executor{Docker: docker, Artifacts: art}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "needs `find`") {
		t.Fatalf("missing find = %v", err)
	}
	// No transport configured.
	ex = &Executor{Docker: docker}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "no artifact transport") {
		t.Fatalf("no transport = %v", err)
	}
}

func TestDownloadArtifactExtractsTarIntoTheContainer(t *testing.T) {
	art := newFakeArtifacts()
	art.uploaded["dist/a.txt"] = "alpha"
	art.uploaded["dist/sub/b.txt"] = "beta"
	art.manifest = &threads.Artifact{
		Name: "build", TotalSize: 9,
		Files: []threads.ArtifactFile{
			{Path: "dist/a.txt", Size: 5, Index: 0},
			{Path: "dist/sub/b.txt", Size: 4, Index: 1},
		},
	}
	docker := &fakeDocker{}
	ex := &Executor{Docker: docker, Artifacts: art}
	job := artifactJob(map[string]string{"path": "dist"}, map[string]string{"name": "build", "path": "in"})
	plan := artifactPlan(t, job)
	stream := &Streamer{}
	if err := ex.downloadArtifact(context.Background(), plan, plan.Steps[1], stream); err != nil {
		t.Fatalf("download: %v\n%s", err, streamText(stream))
	}
	if !docker.called("exec " + plan.Names.Container + " mkdir -p " + DefaultWorkspace + "/in") {
		t.Fatalf("mkdir argv missing: %v", docker.snapshot())
	}
	if !docker.called("tar -xf - -C " + DefaultWorkspace + "/in") {
		t.Fatalf("extract argv missing: %v", docker.snapshot())
	}
	docker.mu.Lock()
	sink := ""
	if len(docker.streamInSinks) > 0 {
		sink = docker.streamInSinks[len(docker.streamInSinks)-1].String()
	}
	docker.mu.Unlock()
	tr := tar.NewReader(strings.NewReader(sink))
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
	if got["dist/a.txt"] != "alpha" || got["dist/sub/b.txt"] != "beta" {
		t.Fatalf("extracted tar = %v", got)
	}
	if !strings.Contains(streamText(stream), `downloaded artifact "build"`) {
		t.Fatalf("log = %q", streamText(stream))
	}
}

func TestDownloadArtifactFailures(t *testing.T) {
	art := newFakeArtifacts()
	art.readErr = fmt.Errorf("artifact is gone")
	ex := &Executor{Docker: &fakeDocker{}, Artifacts: art}
	job := artifactJob(map[string]string{"path": "d"}, map[string]string{"name": "missing"})
	p := artifactPlan(t, job)
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "artifact is gone") {
		t.Fatalf("missing manifest = %v", err)
	}
	// A size mismatch between the manifest and the blob refuses the download.
	art = newFakeArtifacts()
	art.uploaded["a.txt"] = "short"
	art.manifest = &threads.Artifact{Name: "build", Files: []threads.ArtifactFile{{Path: "a.txt", Size: 50, Index: 0}}}
	ex = &Executor{Docker: &fakeDocker{}, Artifacts: art}
	job = artifactJob(map[string]string{"path": "d"}, map[string]string{"name": "build"})
	p = artifactPlan(t, job)
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "the manifest says") {
		t.Fatalf("size mismatch = %v", err)
	}
	// The container's tar is missing: the exit is the message.
	art = newFakeArtifacts()
	art.uploaded["a.txt"] = "abc"
	art.manifest = &threads.Artifact{Name: "build", Files: []threads.ArtifactFile{{Path: "a.txt", Size: 3, Index: 0}}}
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "tar -xf", code: 127}}}
	ex = &Executor{Docker: docker, Artifacts: art}
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "needs `tar`") {
		t.Fatalf("missing tar = %v", err)
	}
}

func TestArtifactCapsDefaults(t *testing.T) {
	c := artifactCaps(threads.ArtifactCaps{})
	if c.MaxBytesPerRun != threads.DefaultMaxArtifactBytesPerRun || c.MaxFiles != threads.MaxArtifactFiles {
		t.Fatalf("defaults = %+v", c)
	}
	c = artifactCaps(threads.ArtifactCaps{MaxBytesPerRun: 10, MaxBytesPerFile: 99})
	if c.MaxBytesPerFile != 10 {
		t.Fatalf("per-file clamp = %+v", c)
	}
	if humanSize(1536) != "1.5 KiB" || humanSize(512) != "512 B" {
		t.Fatalf("humanSize = %q / %q", humanSize(1536), humanSize(512))
	}
}
