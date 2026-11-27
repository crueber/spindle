package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func TestArtifactNameAndPathValidation(t *testing.T) {
	for _, name := range []string{"", ".", "..", "-x", "a/b", "a b", strings.Repeat("x", threads.MaxArtifactNameLen+1)} {
		if err := validateArtifactName(name); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	if err := validateArtifactName("build-out_1.tar.gz"); err != nil {
		t.Fatalf("good name: %v", err)
	}
	for _, raw := range []string{"~/x", "/abs/x", "../x", "   "} {
		if _, err := splitArtifactPatterns(raw); err == nil {
			t.Fatalf("pattern %q accepted", raw)
		}
	}
	if got, err := splitArtifactPatterns("./dist, coverage/*.out\nnotes.md"); err != nil || len(got) != 3 || got[0] != "dist" {
		t.Fatalf("patterns = %v (%v)", got, err)
	}
	for _, tc := range []struct {
		dir  string
		want string
	}{
		{".", DefaultWorkspace},
		{"sub/dir", DefaultWorkspace + "/sub/dir"},
		{"/abs", "/abs"},
	} {
		got, err := artifactDestination(DefaultWorkspace, tc.dir)
		if err != nil || got != tc.want {
			t.Fatalf("destination(%q) = %q (%v)", tc.dir, got, err)
		}
	}
	if _, err := artifactDestination(DefaultWorkspace, "../escape"); err == nil {
		t.Fatal("climbing destination accepted")
	}
	for _, tc := range []struct {
		name string
		ok   bool
	}{{"dist/a.txt", true}, {"./dist/a.txt", true}, {"../x", false}, {"/etc/passwd", false}, {".", false}} {
		got, err := cleanArtifactPathFromTar(tc.name)
		if tc.ok && (err != nil || got == "") {
			t.Fatalf("tar path %q = %q (%v)", tc.name, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("tar path %q accepted", tc.name)
		}
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {2048, "2.0 KiB"}, {3 << 20, "3.0 MiB"}, {5 << 30, "5.0 GiB"}} {
		if got := humanSize(tc.n); got != tc.want {
			t.Fatalf("humanSize(%d) = %q", tc.n, got)
		}
	}
}

// clientStub is an ArtifactClient whose behaviour each test scripts.
type clientStub struct {
	manifest *threads.Artifact
	getErr   error
	getBody  string
	getSize  int64
	putErr   error
	finalErr error
}

func (c *clientStub) PutArtifactBlob(context.Context, string, string, int, string, int, string, int64, io.Reader) (*threads.ArtifactBlobView, error) {
	if c.putErr != nil {
		return nil, c.putErr
	}
	return &threads.ArtifactBlobView{SHA256: "d"}, nil
}

func (c *clientStub) FinalizeArtifact(context.Context, string, string, int, threads.ArtifactFinalizeInput) (*threads.ArtifactView, error) {
	if c.finalErr != nil {
		return nil, c.finalErr
	}
	return &threads.ArtifactView{}, nil
}

func (c *clientStub) ArtifactManifest(context.Context, string, string, int, string) (*threads.Artifact, error) {
	return c.manifest, nil
}

func (c *clientStub) GetArtifactBlob(context.Context, string, string, int, string, int) (io.ReadCloser, int64, error) {
	return c.GetArtifactBlobFromRun(context.Background(), "", "", 0, "", "", 0)
}

func (c *clientStub) PrepareArtifact(context.Context, string, string, int, string, bool) (bool, error) {
	return false, c.putErr
}

func (c *clientStub) ArtifactList(context.Context, string, string, int, string) ([]*threads.Artifact, error) {
	if c.manifest == nil {
		return nil, nil
	}
	return []*threads.Artifact{c.manifest}, nil
}

func (c *clientStub) ArtifactManifestFromRun(context.Context, string, string, int, string, string) (*threads.Artifact, error) {
	return c.manifest, nil
}

func (c *clientStub) GetArtifactBlobFromRun(context.Context, string, string, int, string, string, int) (io.ReadCloser, int64, error) {
	if c.getErr != nil {
		return nil, 0, c.getErr
	}
	return io.NopCloser(strings.NewReader(c.getBody)), c.getSize, nil
}

func artifactPlanFor(t *testing.T, job *threads.FetchResult) *Plan {
	t.Helper()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func TestUploadArtifactEdgeFailures(t *testing.T) {
	job := artifactJob(map[string]string{"path": "dist"}, nil)
	p := artifactPlanFor(t, job)
	// No plan and no transport.
	ex := &Executor{Docker: &fakeDocker{}, Artifacts: &clientStub{}}
	if err := ex.uploadArtifact(context.Background(), p, StepPlan{Kind: "artifact-upload"}, &Streamer{}); err == nil {
		t.Fatal("upload without a plan succeeded")
	}
	ex = &Executor{Docker: &fakeDocker{runOutputs: []runOutput{{match: "find . -type f", out: "./dist/a\n"}}}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "no artifact transport") {
		t.Fatalf("no transport = %v", err)
	}
	// The container tar fails after a clean start.
	ex = &Executor{Docker: &fakeDocker{
		runOutputs:  []runOutput{{match: "find . -type f", out: "./dist/a\n"}},
		streamInErr: errors.New("container died"),
	}, Artifacts: &clientStub{}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil {
		t.Fatal("a failed container tar was accepted")
	}
	// An empty archive is named (tar succeeded but carried nothing).
	ex = &Executor{Docker: &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a\n"}},
		streamInOutput: tarFixture(nil),
	}, Artifacts: &clientStub{}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "no regular files") {
		t.Fatalf("empty archive = %v", err)
	}
	// A blob upload failure names the file.
	ex = &Executor{Docker: &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a\n"}},
		streamInOutput: tarFixture([][2]string{{"dist/a", "x"}}),
	}, Artifacts: &clientStub{putErr: errors.New("boom")}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "upload \"dist/a\"") {
		t.Fatalf("blob failure = %v", err)
	}
	// A finalize failure is surfaced.
	ex = &Executor{Docker: &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a\n"}},
		streamInOutput: tarFixture([][2]string{{"dist/a", "x"}}),
	}, Artifacts: &clientStub{finalErr: errors.New("conflict")}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "finalize artifact") {
		t.Fatalf("finalize failure = %v", err)
	}
	// A tar entry that names an unsafe path is refused.
	ex = &Executor{Docker: &fakeDocker{
		runOutputs:     []runOutput{{match: "find . -type f", out: "./dist/a\n"}},
		streamInOutput: tarFixture([][2]string{{"../escape", "x"}}),
	}, Artifacts: &clientStub{}}
	if err := ex.uploadArtifact(context.Background(), p, p.Steps[0], &Streamer{}); err == nil {
		t.Fatal("an unsafe tar entry was accepted")
	}
	// Too many matched files is a named failure (before any stream).
	p2 := artifactPlanFor(t, job)
	p2.ArtifactCaps = threads.ArtifactCaps{MaxFiles: 1, MaxBytesPerRun: 100}
	files := "./dist/a\n./dist/b\n"
	ex = &Executor{Docker: &fakeDocker{runOutputs: []runOutput{{match: "find . -type f", out: files}}}, Artifacts: &clientStub{}}
	if err := ex.uploadArtifact(context.Background(), p2, p2.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "over the cap of 1") {
		t.Fatalf("file-count cap = %v", err)
	}
}

func TestDownloadArtifactEdgeFailures(t *testing.T) {
	job := artifactJob(nil, map[string]string{"name": "build"})
	p := artifactPlanFor(t, job)
	manifest := &threads.Artifact{Name: "build", Files: []threads.ArtifactFile{{Path: "a", Size: 1, Index: 0}}, TotalSize: 1}
	// No transport and no plan.
	ex := &Executor{Docker: &fakeDocker{}}
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "no artifact transport") {
		t.Fatalf("no transport = %v", err)
	}
	if err := ex.downloadArtifact(context.Background(), p, StepPlan{Kind: "artifact-download"}, &Streamer{}); err == nil {
		t.Fatal("download without a plan succeeded")
	}
	// The download cap refuses an oversized artifact.
	ex = &Executor{Docker: &fakeDocker{}, Artifacts: &clientStub{manifest: &threads.Artifact{Name: "build", TotalSize: threads.DefaultMaxArtifactBytesPerRun + 1}}}
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "download cap") {
		t.Fatalf("download cap = %v", err)
	}
	// mkdir failing in the container is named.
	ex = &Executor{Docker: &fakeDocker{runOutputs: []runOutput{{match: "mkdir -p", out: "", code: 1}}}, Artifacts: &clientStub{manifest: manifest}}
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "create "+DefaultWorkspace) {
		t.Fatalf("mkdir failure = %v", err)
	}
	// A blob read error aborts before extraction.
	ex = &Executor{Docker: &fakeDocker{}, Artifacts: &clientStub{manifest: manifest, getErr: errors.New("gone")}}
	if err := ex.downloadArtifact(context.Background(), p, p.Steps[1], &Streamer{}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("blob error = %v", err)
	}
	// writeArtifactTar: a canceled context aborts before any blob is read, and a
	// body whose length disagrees with the manifest is refused.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	if err := writeArtifactTar(canceled, pw, &clientStub{manifest: manifest}, p, "build", manifest); err == nil {
		t.Fatal("canceled tar succeeded")
	}
	pr, pw = io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	if err := writeArtifactTar(context.Background(), pw, &clientStub{manifest: manifest, getBody: "toolong", getSize: 7}, p, "build", manifest); err == nil || !strings.Contains(err.Error(), "the manifest says") {
		t.Fatalf("size mismatch tar = %v", err)
	}
}
