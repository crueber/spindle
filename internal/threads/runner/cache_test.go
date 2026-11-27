package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// The built-in cache steps: the input table, the plan kinds, restore/save over
// the fake docker and a fake transport, the deferred save, and the setup-*
// recipes (PAR-095/PAR-096).

// cacheJob builds a job whose single step is the given cache action.
func cacheJob(uses string, with map[string]string) *threads.FetchResult {
	return &threads.FetchResult{
		Repo: "acme/mono", RunID: "1732104000000000001-a1b2c3", JobID: "j-build",
		JobRef: "jr_acme", Attempt: 1, Commit: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Event: "push", Branch: "main", Workflow: "CI", Trusted: true, Context: "threads/ci/build",
		Spec: &threads.Spec{Version: threads.SpecVersion, RunsOn: []string{"linux"},
			Steps: []threads.StepSpec{{Uses: uses, With: with}}},
	}
}

func mustPlanCache(t *testing.T, uses string, with map[string]string) *Plan {
	t.Helper()
	p, err := BuildPlan(PlanInput{Job: cacheJob(uses, with), AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestParseCacheStepTable(t *testing.T) {
	// The path list is order-independent for the fingerprint and sorted inside.
	a := mustPlanCache(t, "actions/cache", map[string]string{"path": "b\n.a", "key": "k"})
	b := mustPlanCache(t, "actions/cache", map[string]string{"path": ".a,b", "key": "k"})
	if a.Steps[0].Cache.Version != b.Steps[0].Cache.Version {
		t.Fatalf("fingerprints differ: %q vs %q", a.Steps[0].Cache.Version, b.Steps[0].Cache.Version)
	}
	if a.Steps[0].Kind != "cache" || !a.Steps[0].Cache.Deferred {
		t.Fatalf("actions/cache plan = %+v", a.Steps[0])
	}
	restore := mustPlanCache(t, "actions/cache/restore@v4", map[string]string{"path": "p", "key": "k", "restore-keys": "a\nb", "fail-on-cache-miss": "true", "lookup-only": "yes"})
	if restore.Steps[0].Kind != "cache-restore" || !restore.Steps[0].Cache.FailOnMiss || !restore.Steps[0].Cache.LookupOnly ||
		len(restore.Steps[0].Cache.RestoreKeys) != 2 || restore.Steps[0].Cache.Deferred {
		t.Fatalf("restore plan = %+v", restore.Steps[0])
	}
	save := mustPlanCache(t, "actions/cache/save", map[string]string{"path": "p", "key": "k"})
	if save.Steps[0].Kind != "cache-save" || save.Steps[0].Cache.Deferred {
		t.Fatalf("save plan = %+v", save.Steps[0])
	}

	cases := []struct {
		name  string
		uses  string
		with  map[string]string
		wants string
	}{
		{"missing key", "actions/cache", map[string]string{"path": "p"}, "key is required"},
		{"missing path", "actions/cache", map[string]string{"key": "k"}, "path is required"},
		{"unknown key", "actions/cache", map[string]string{"path": "p", "key": "k", "nope": "1"}, `unsupported with key "nope"`},
		{"cross-os", "actions/cache", map[string]string{"path": "p", "key": "k", "enableCrossOsArchive": "true"}, "enableCrossOsArchive is not supported"},
		{"bad bool", "actions/cache", map[string]string{"path": "p", "key": "k", "fail-on-cache-miss": "maybe"}, "must be true or false"},
		{"absolute path", "actions/cache", map[string]string{"path": "/root/.npm", "key": "k"}, "must be relative to the workspace"},
		{"tilde path", "actions/cache", map[string]string{"path": "~/.npm", "key": "k"}, "must not use ~"},
		{"backslash path", "actions/cache", map[string]string{"path": `a\b`, "key": "k"}, "must use / separators"},
		{"climbing path", "actions/cache", map[string]string{"path": "../x", "key": "k"}, "escapes the workspace"},
		{"workspace root", "actions/cache", map[string]string{"path": ".", "key": "k"}, "escapes the workspace"},
		{"long key", "actions/cache", map[string]string{"path": "p", "key": strings.Repeat("k", threads.MaxCacheKeyLen+1)}, "longer than"},
	}
	for _, tc := range cases {
		p := mustPlanCache(t, tc.uses, tc.with)
		if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, tc.wants) {
			t.Fatalf("%s = %q (%s), want %q", tc.name, p.Steps[0].Kind, p.Steps[0].SkipReason, tc.wants)
		}
	}
	// False spellings and an empty path list are named errors.
	if _, err := parseCacheBool("x", "no"); err != nil {
		t.Fatalf("parseCacheBool(no) = %v", err)
	}
	if _, err := parseCacheBool("x", ""); err != nil {
		t.Fatalf("parseCacheBool(empty) = %v", err)
	}
	if _, err := parseCacheBool("x", "0"); err != nil {
		t.Fatalf("parseCacheBool(0) = %v", err)
	}
	if _, err := splitCachePaths("\n , \n"); err == nil {
		t.Fatal("splitCachePaths accepted an empty list")
	}
	if _, err := splitCachePaths("/abs"); err == nil {
		t.Fatal("splitCachePaths accepted an absolute path")
	}
	if got := splitCacheRestoreKeys("\n a \n b \n"); len(got) != 2 {
		t.Fatalf("splitCacheRestoreKeys = %v", got)
	}
	if _, err := parseArtifactBool("x", "nope"); err == nil {
		t.Fatal("parseArtifactBool accepted nonsense")
	}
	if _, err := parseArtifactBool("x", "0"); err != nil {
		t.Fatalf("parseArtifactBool(0) = %v", err)
	}
	if _, err := parseCompressionLevel("0"); err != nil {
		t.Fatalf("parseCompressionLevel(0) = %v", err)
	}
	if _, err := parseArtifactIDs("1, ,2"); err != nil {
		t.Fatalf("parseArtifactIDs = %v", err)
	}
	// More paths than the cap.
	many := make([]string, maxCachePaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	p := mustPlanCache(t, "actions/cache", map[string]string{"path": strings.Join(many, "\n"), "key": "k"})
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "over the limit") {
		t.Fatalf("many paths = %+v", p.Steps[0])
	}
	// Too many restore-keys.
	p = mustPlanCache(t, "actions/cache", map[string]string{"path": "p", "key": "k",
		"restore-keys": strings.Repeat("r\n", threads.MaxCacheRestoreKeys+1)})
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "restore-keys") {
		t.Fatalf("too many restore keys = %+v", p.Steps[0])
	}
}

func TestSetupCacheRecipes(t *testing.T) {
	cases := map[string][]string{
		"actions/setup-node":   {"actions/cache", "npm_config_cache", "npm-"},
		"actions/setup-go":     {"GOCACHE", "GOMODCACHE"},
		"actions/setup-python": {"PIP_CACHE_DIR", "pip-"},
		"actions/setup-java":   {"not one of the mapped ecosystems"},
	}
	for uses, wants := range cases {
		p := mustPlanCache(t, uses, map[string]string{"cache": "npm", "node-version": "22"})
		if p.Steps[0].Kind != "unsupported" {
			t.Fatalf("%s = %+v", uses, p.Steps[0])
		}
		for _, want := range wants {
			if !strings.Contains(p.Steps[0].SkipReason, want) {
				t.Fatalf("%s refusal misses %q: %s", uses, want, p.Steps[0].SkipReason)
			}
		}
	}
	// A recipe with no inputs still names the ecosystem.
	p := mustPlanCache(t, "actions/setup-node", nil)
	if !strings.Contains(p.Steps[0].SkipReason, "actions/cache") {
		t.Fatalf("bare setup-node = %s", p.Steps[0].SkipReason)
	}
	if got := restoreHint("npm-${{ runner.os }}-${{ github.sha }}"); got != "npm-${{ runner.os }}-" {
		t.Fatalf("restoreHint = %q", got)
	}
	if got := restoreHint("no-template"); got != "no-template" {
		t.Fatalf("restoreHint fallback = %q", got)
	}
}

// fakeCaches is the cache transport double.
type fakeCaches struct {
	lookup      *threads.CacheView
	lookupErr   error
	blob        string
	blobSize    int64
	blobErr     error
	putErr      error
	saveErr     error
	putSHA      string
	putBody     string
	saved       *threads.SaveCacheInput
	lookupCalls int
}

func (f *fakeCaches) CacheLookup(context.Context, string, string, int, threads.CacheLookupInput) (*threads.CacheView, error) {
	f.lookupCalls++
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.lookup == nil {
		return &threads.CacheView{Hit: false, Reason: "miss"}, nil
	}
	return f.lookup, nil
}

func (f *fakeCaches) GetCacheBlob(context.Context, string, string, int, string) (io.ReadCloser, int64, error) {
	if f.blobErr != nil {
		return nil, 0, f.blobErr
	}
	size := f.blobSize
	if size == 0 {
		size = int64(len(f.blob))
	}
	return io.NopCloser(strings.NewReader(f.blob)), size, nil
}

func (f *fakeCaches) PutCacheBlob(_ context.Context, _, _ string, _ int, sha string, size int64, body io.Reader) error {
	if f.putErr != nil {
		return f.putErr
	}
	raw, _ := io.ReadAll(body)
	if int64(len(raw)) != size {
		return fmt.Errorf("size mismatch: %d of %d", len(raw), size)
	}
	f.putSHA, f.putBody = sha, string(raw)
	return nil
}

func (f *fakeCaches) SaveCache(_ context.Context, _, _ string, _ int, in threads.SaveCacheInput) (*threads.CacheView, error) {
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	cp := in
	f.saved = &cp
	return &threads.CacheView{Hit: true, Key: in.Key, SHA256: in.SHA256, Size: in.Size}, nil
}

// cacheTar builds a tar of one file (the save fixture).
func cacheTar(path, body string) string {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	return buf.String()
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestRestoreCache(t *testing.T) {
	payload := cacheTar(".npm/x", "bytes")
	view := &threads.CacheView{Hit: true, Exact: true, Key: "npm-1", Version: "v1", SHA256: shaHex(payload), Size: int64(len(payload)), Scope: "default"}

	ex := &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{lookup: view, blob: payload}}
	p := mustPlanCache(t, "actions/cache", map[string]string{"path": ".npm", "key": "npm-1"})
	if err := ex.restoreCache(context.Background(), p, p.Steps[0], &Streamer{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if p.Steps[0].Cache.Hit != true || p.Steps[0].Cache.Exact != true {
		t.Fatalf("hit state = %+v", p.Steps[0].Cache)
	}
	// The container's tar received the payload.
	fd := ex.Docker.(*fakeDocker)
	if !fd.called("tar -xf -") || fd.streamInSinks[0].String() != payload {
		t.Fatalf("extract argv = %v", fd.snapshot())
	}

	// A miss is not a failure and starts no extraction.
	ex = &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{}}
	p = mustPlanCache(t, "actions/cache", map[string]string{"path": ".npm", "key": "npm-1"})
	if err := ex.restoreCache(context.Background(), p, p.Steps[0], &Streamer{}); err != nil {
		t.Fatalf("miss = %v", err)
	}
	if p.Steps[0].Cache.Hit || ex.Docker.(*fakeDocker).called("tar") {
		t.Fatal("a miss started an extraction")
	}

	// fail-on-cache-miss turns the miss into the step's error.
	p = mustPlanCache(t, "actions/cache", map[string]string{"path": ".npm", "key": "k", "fail-on-cache-miss": "true"})
	if err := ex.restoreCache(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "fail-on-cache-miss") {
		t.Fatalf("fail-on-cache-miss = %v", err)
	}

	// Every transport/docker failure is named.
	cases := []struct {
		name string
		ex   *Executor
		step StepPlan
	}{
		{"no plan", &Executor{Caches: &fakeCaches{}}, StepPlan{}},
		{"no transport", &Executor{Docker: &fakeDocker{}}, StepPlan{Cache: &CacheStep{}}},
		{"lookup error", &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{lookupErr: errors.New("down")}}, StepPlan{Cache: &CacheStep{}}},
		{"blob error", &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{lookup: view, blobErr: errors.New("down")}}, StepPlan{Cache: &CacheStep{}}},
		{"size mismatch", &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{lookup: view, blob: payload, blobSize: 3}}, StepPlan{Cache: &CacheStep{}}},
		{"digest mismatch", &Executor{Docker: &fakeDocker{}, Caches: &fakeCaches{lookup: view, blob: "other"}}, StepPlan{Cache: &CacheStep{}}},
	}
	for i := range cases {
		tc := &cases[i]
		if tc.step.Cache == nil && tc.name != "no plan" {
			tc.step.Cache = &CacheStep{Key: "k"}
		}
		if tc.name == "no plan" {
			tc.step.Cache = nil
		}
		if err := tc.ex.restoreCache(context.Background(), &Plan{JobToken: "t", Names: Names{Container: "c"}, Workspace: DefaultWorkspace}, tc.step, &Streamer{}); err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
	}
	// A container tar failure names the exit.
	ex = &Executor{Docker: &fakeDocker{streamByMatch: []runOutput{{match: "tar -xf", code: 2}}, streamInErr: errors.New("tar: bad")},
		Caches: &fakeCaches{lookup: view, blob: payload}}
	p = mustPlanCache(t, "actions/cache", map[string]string{"path": ".npm", "key": "k"})
	if err := ex.restoreCache(context.Background(), p, p.Steps[0], &Streamer{}); err == nil || !strings.Contains(err.Error(), "extract") {
		t.Fatalf("extract failure = %v", err)
	}
}

func TestSaveCache(t *testing.T) {
	payload := cacheTar(".npm/x", "bytes")
	fc := &fakeCaches{}
	fd := &fakeDocker{streamInOutput: []byte(payload)}
	ex := &Executor{Docker: fd, Caches: fc}
	p := mustPlanCache(t, "actions/cache/save", map[string]string{"path": ".npm", "key": "npm-1"})
	if err := ex.saveCache(context.Background(), p, p.Steps[0], &Streamer{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fc.putSHA != shaHex(payload) || fc.putBody != payload {
		t.Fatalf("uploaded sha/body mismatch: %q", fc.putSHA)
	}
	if fc.saved == nil || fc.saved.Key != "npm-1" || fc.saved.SHA256 != shaHex(payload) || fc.saved.Size != int64(len(payload)) || fc.saved.Files != 1 {
		t.Fatalf("saved entry = %+v", fc.saved)
	}
	if !fd.called("tar -cf -") {
		t.Fatalf("save argv = %v", fd.snapshot())
	}

	// Failures: no plan, no transport, tar failure, an over-cap archive, upload
	// and commit failures.
	failures := []struct {
		name string
		ex   *Executor
		step StepPlan
		plan *Plan
	}{
		{"no plan", &Executor{Caches: fc}, StepPlan{}, p},
		{"no transport", &Executor{Docker: fd}, StepPlan{Cache: &CacheStep{Key: "k", Paths: []string{"p"}}}, p},
		{"tar failure", &Executor{Docker: &fakeDocker{streamCode: 2, streamInErr: errors.New("bad")}, Caches: fc}, StepPlan{Cache: &CacheStep{Key: "k", Paths: []string{"p"}}}, p},
		{"put failure", &Executor{Docker: &fakeDocker{streamInOutput: []byte(payload)}, Caches: &fakeCaches{putErr: errors.New("down")}}, StepPlan{Cache: &CacheStep{Key: "k", Paths: []string{"p"}}}, p},
		{"save failure", &Executor{Docker: &fakeDocker{streamInOutput: []byte(payload)}, Caches: &fakeCaches{saveErr: errors.New("down")}}, StepPlan{Cache: &CacheStep{Key: "k", Paths: []string{"p"}}}, p},
	}
	for _, tc := range failures {
		if err := tc.ex.saveCache(context.Background(), tc.plan, tc.step, &Streamer{}); err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
	}
	// The per-entry cap trips the limit writer.
	tiny := &Plan{JobToken: "t", Names: Names{Container: "c"}, Workspace: DefaultWorkspace, CacheCaps: threads.CacheCaps{MaxBytesPerEntry: 4}}
	ex = &Executor{Docker: &fakeDocker{streamInOutput: []byte(payload)}, Caches: fc}
	if err := ex.saveCache(context.Background(), tiny, StepPlan{Cache: &CacheStep{Key: "k", Paths: []string{"p"}}}, &Streamer{}); err == nil || !strings.Contains(err.Error(), "per-entry cap") {
		t.Fatalf("cap = %v", err)
	}
	// An archive that cannot be read reports 0 files (the diagnostic only).
	if n := countTarFiles("/nonexistent/spindle"); n != 0 {
		t.Fatalf("countTarFiles of a missing file = %d", n)
	}
}

func TestDeferredCacheSavesAndOutput(t *testing.T) {
	payload := cacheTar(".npm/x", "bytes")
	fc := &fakeCaches{}
	plan := &Plan{JobToken: "t", Names: Names{Container: "c"}, Workspace: DefaultWorkspace}
	ex := &Executor{Docker: &fakeDocker{streamInOutput: []byte(payload)}, Caches: fc}

	plan.deferCacheSave(nil) // nil-safe
	plan.deferCacheSave(&CacheStep{Key: "npm-1", Paths: []string{".npm"}})
	ex.runDeferredCaches(context.Background(), plan, &Streamer{})
	if fc.saved == nil || fc.saved.Key != "npm-1" {
		t.Fatalf("deferred save = %+v", fc.saved)
	}
	// A failed deferred save is a log line, never a job failure.
	badPlan := &Plan{JobToken: "t", Names: Names{Container: "c"}, Workspace: DefaultWorkspace}
	badPlan.deferCacheSave(&CacheStep{Key: "k", Paths: []string{"p"}})
	bad := &Executor{Docker: &fakeDocker{streamCode: 2}, Caches: fc}
	bad.runDeferredCaches(context.Background(), badPlan, &Streamer{})
	// No transport skips every save.
	none := &Executor{Docker: &fakeDocker{}}
	none.runDeferredCaches(context.Background(), badPlan, &Streamer{})

	// cache-hit output: exact only.
	exact := StepPlan{Cache: &CacheStep{Exact: true}}
	if got := cacheHitOutput(exact)["cache-hit"]; got != "true" {
		t.Fatalf("exact cache-hit = %q", got)
	}
	prefix := StepPlan{Cache: &CacheStep{Exact: false}}
	if got := cacheHitOutput(prefix)["cache-hit"]; got != "false" {
		t.Fatalf("prefix cache-hit = %q", got)
	}
	if cacheHitOutput(StepPlan{}) != nil {
		t.Fatal("a step with no cache produced outputs")
	}
	// The kind predicate.
	for _, kind := range []string{"cache", "cache-restore", "cache-save"} {
		if !cacheStepKind(kind) {
			t.Fatalf("%s is not a cache kind", kind)
		}
	}
	if cacheStepKind("artifact-upload") {
		t.Fatal("an artifact step is a cache kind")
	}
	// A tiny limit writer refuses and marks the overflow.
	lw := &limitWriter{w: io.Discard, max: 2}
	if _, err := lw.Write([]byte("abc")); err == nil || !lw.over {
		t.Fatalf("limit writer = %v", err)
	}
	// The cache caps fall back to the service defaults.
	if caps := cacheCaps(threads.CacheCaps{}); caps.MaxBytesPerEntry <= 0 || caps.MaxBytesPerRepo <= 0 || caps.MaxEntries <= 0 {
		t.Fatalf("default caps = %+v", caps)
	}
}

func TestExecuteCacheStepWiring(t *testing.T) {
	// The plan kinds reach the executor and the deferred save runs at the end
	// of a green job. This drives runStepCommand directly (no container needed
	// beyond the fake docker's scripted answers).
	payload := cacheTar(".npm/x", "bytes")
	fc := &fakeCaches{}
	ex := &Executor{Docker: &fakeDocker{streamInOutput: []byte(payload)}, Caches: fc}
	p := mustPlanCache(t, "actions/cache", map[string]string{"path": ".npm", "key": "npm-1"})
	// The plan's step is the cache action; the executor's switch routes it.
	step := p.Steps[0]
	if err := ex.runStepCommand(context.Background(), p, step, io.Discard, &Streamer{}); err != nil {
		t.Fatalf("runStepCommand(cache) = %v", err)
	}
	if step.Cache == nil || !step.Cache.Deferred {
		t.Fatalf("cache step = %+v", step)
	}
	// The save kind routes too.
	savePlan := mustPlanCache(t, "actions/cache/save", map[string]string{"path": ".npm", "key": "npm-2"})
	if err := ex.runStepCommand(context.Background(), savePlan, savePlan.Steps[0], io.Discard, &Streamer{}); err != nil {
		t.Fatalf("runStepCommand(cache-save) = %v", err)
	}
	if fc.saved == nil || fc.saved.Key != "npm-2" {
		t.Fatalf("cache-save route = %+v", fc.saved)
	}
}
