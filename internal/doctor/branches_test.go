package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// listFailStore fails List for one prefix substring (a family- or check-scoped
// fault, which is what a live bucket actually does).
type listFailStore struct {
	store.ObjectStore
	contains string
}

func (s *listFailStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if strings.Contains(prefix, s.contains) {
		return fmt.Errorf("boom listing %s", prefix)
	}
	return s.ObjectStore.List(ctx, prefix, startAfter, fn)
}

// manyOwners yields n owner prefixes under repos/ and nothing else.
type manyOwners struct {
	*store.Memory
	n int
}

func (m *manyOwners) ListPrefixes(_ context.Context, prefix string, fn func(string) error) error {
	if prefix != "repos/" {
		return nil
	}
	for i := 0; i < m.n; i++ {
		if err := fn(fmt.Sprintf("repos/o%05d/", i)); err != nil {
			return err
		}
	}
	return nil
}

// manyRepos yields one owner with n repositories.
type manyRepos struct {
	*store.Memory
	n int
}

func (m *manyRepos) ListPrefixes(_ context.Context, prefix string, fn func(string) error) error {
	if prefix == "repos/" {
		return fn("repos/acme/")
	}
	for i := 0; i < m.n; i++ {
		if err := fn(fmt.Sprintf("repos/acme/r%05d/", i)); err != nil {
			return err
		}
	}
	return nil
}

func TestRunOrphanAndManifestErrors(t *testing.T) {
	ctx := context.Background()
	base := store.NewMemory()
	putManifest(t, base, "acme/app")

	if _, err := Run(ctx, &listFailStore{ObjectStore: base, contains: "/threads/runs/"}, Options{}); err == nil {
		t.Fatal("orphan List error: Run = nil")
	}
	if _, err := Run(ctx, newErrStore(base).failOn("Head", 1), Options{}); err == nil {
		t.Fatal("manifest Head error: Run = nil")
	}
}

func TestEnumerateCaps(t *testing.T) {
	s := newScanner(&manyOwners{Memory: store.NewMemory(), n: maxFamilyCount + 1}, Options{})
	if err := s.enumerateRepos(context.Background()); err != nil {
		t.Fatalf("owners: %v", err)
	}
	s2 := newScanner(&manyRepos{Memory: store.NewMemory(), n: maxFamilyCount + 1}, Options{Repos: 2})
	if err := s2.enumerateRepos(context.Background()); err != nil {
		t.Fatalf("repos: %v", err)
	}
	if !s2.repoTruncated || s2.repoTotal != maxFamilyCount {
		t.Fatalf("repoTotal = %d truncated=%v, want %d/true", s2.repoTotal, s2.repoTruncated, maxFamilyCount)
	}
}

func TestListIntoBranches(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	put(t, st, "repos/a/b/issues/000001/thread.json", []byte(`{"num":1}`))

	// A match that rejects every key covers the skip arm.
	s := newScanner(st, Options{})
	fr := s.familyReport("x")
	if err := s.listInto(ctx, family{name: "x"}, fr, "repos/a/b/issues/", func(string) bool { return false }); err != nil {
		t.Fatalf("skip arm: %v", err)
	}
	if fr.Total != 0 {
		t.Fatalf("total = %d, want 0", fr.Total)
	}

	// A canceled context is returned from the callback.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	s2 := newScanner(&syntheticListStore{Memory: store.NewMemory(), n: 1}, Options{})
	if err := s2.listInto(cctx, family{name: "x"}, s2.familyReport("x"), "repos/", nil); err == nil {
		t.Fatal("canceled context: nil error")
	}

	// A decode failure inside the loop aborts the family walk.
	s3 := newScanner(newErrStore(st).failOn("Get", 1), Options{})
	if err := s3.listInto(ctx, family{name: "x"}, s3.familyReport("x"), "repos/a/b/issues/", nil); err == nil {
		t.Fatal("decode error: nil error")
	}
}

func TestScanFamilyRootPrefixError(t *testing.T) {
	s := newScanner(newErrStore(store.NewMemory()).failOn("List", 1), Options{})
	if err := s.scanFamily(context.Background(), family{name: "x", rootPrefixes: []string{"users/"}}); err == nil {
		t.Fatal("root prefix: nil error")
	}
}

func TestCheckOrphansEarlyReturns(t *testing.T) {
	ctx := context.Background()
	base := store.NewMemory()
	mk := func(st store.ObjectStore) *scanner {
		s := newScanner(st, Options{})
		s.repos = []string{"a/b"}
		return s
	}
	if err := mk(newErrStore(base).failOn("List", 1)).checkOrphans(ctx); err == nil {
		t.Fatal("artifact error not returned")
	}
	if err := mk(newErrStore(base).failOn("Get", 1)).checkOrphans(ctx); err == nil {
		t.Fatal("queue error not returned")
	}
	if err := mk(newErrStore(base).failOn("Get", 2)).checkOrphans(ctx); err == nil {
		t.Fatal("report error not returned")
	}
}

func TestArtifactCheckSkipsUnrelatedKeys(t *testing.T) {
	st := store.NewMemory()
	putJSON(t, st, "repos/a/b/threads/runs/r.json", map[string]any{"id": "r"})
	put(t, st, "repos/a/b/artifacts/manifest.json", []byte(`{}`))
	s := newScanner(st, Options{})
	s.repos = []string{"a/b"}
	if err := s.checkArtifactOrphans(context.Background()); err != nil {
		t.Fatalf("artifact check: %v", err)
	}
	if len(s.report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", s.report.Findings)
	}
}

func TestQueueIndexCorruptAndLimit(t *testing.T) {
	st := store.NewMemory()
	put(t, st, "threads/queue/index.json", []byte(`{`))
	s := newScanner(st, Options{})
	if err := s.checkQueueOrphans(context.Background()); err != nil || len(s.report.Findings) != 0 {
		t.Fatalf("corrupt index: %v %+v", err, s.report.Findings)
	}

	st2 := store.NewMemory()
	putJSON(t, st2, "threads/queue/index.json", map[string]any{
		"version": 1,
		"queued": []any{
			map[string]any{"repo": "a/b", "run": "r1", "job": "j1"},
			map[string]any{"repo": "a/b", "run": "r2", "job": "j2"},
		},
	})
	s2 := newScanner(st2, Options{Limit: 1})
	if err := s2.checkQueueOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReportCheckLimitAndErrors(t *testing.T) {
	st := store.NewMemory()
	putJSON(t, st, "moderation/reports/index.json", map[string]any{
		"version": 1,
		"open": []any{
			map[string]any{"id": 1, "repo": "a/b"},
			map[string]any{"id": 2, "repo": "a/b"},
		},
	})
	s := newScanner(st, Options{Limit: 1})
	s.repos = []string{"missing/repo"}
	if err := s.checkReportOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A per-repo index whose record HEAD fails aborts the check.
	st2 := store.NewMemory()
	putJSON(t, st2, "repos/a/b/meta/moderation/reports/index.json", map[string]any{
		"version": 1, "open": []any{map[string]any{"id": 1}},
	})
	s2 := newScanner(newErrStore(st2).failOn("Head", 1), Options{})
	s2.repos = []string{"a/b"}
	if err := s2.checkReportOrphans(context.Background()); err == nil {
		t.Fatal("per-repo head error not returned")
	}
}

func TestForkCheckBranches(t *testing.T) {
	ctx := context.Background()

	// Invalid fork.json and invalid parent index are skipped, not findings.
	st := store.NewMemory()
	putManifest(t, st, "a/b")
	putManifest(t, st, "p/q")
	put(t, st, "repos/a/b/fork.json", []byte(`{`))
	putJSON(t, st, "repos/p/q/meta/forks.json", map[string]any{"version": 1, "forks": []any{}})
	s := newScanner(st, Options{})
	s.repos = []string{"a/b"}
	if err := s.checkForkOrphans(ctx); err != nil {
		t.Fatalf("invalid docs: %v", err)
	}

	// A child fork.json the parent index lists, with the child's own index
	// absent: the parent row is checked, the child read succeeds.
	st2 := store.NewMemory()
	putManifest(t, st2, "a/b")
	putManifest(t, st2, "p/q")
	putJSON(t, st2, "repos/a/b/fork.json", map[string]any{"parent": "p/q", "version": 1})
	putJSON(t, st2, "repos/a/b/meta/forks.json", map[string]any{
		"version": 1,
		"forks":   []any{map[string]any{"repo": "c/d"}, map[string]any{"repo": "e/f"}},
	})
	putJSON(t, st2, "repos/c/d/fork.json", map[string]any{"parent": "a/b", "version": 1})
	s2 := newScanner(st2, Options{Limit: 5})
	s2.repos = []string{"a/b"}
	if err := s2.checkForkOrphans(ctx); err != nil {
		t.Fatalf("child rows: %v", err)
	}
	var ghost bool
	for _, f := range s2.report.Findings {
		if f.Check == CheckFork && strings.HasSuffix(f.Key, "e/f/fork.json") {
			ghost = true
		}
	}
	if !ghost {
		t.Fatalf("findings = %+v, want the missing child fork.json", s2.report.Findings)
	}
}

func TestForkIndexHas(t *testing.T) {
	rows := []struct {
		Repo string `json:"repo"`
	}{{Repo: "a/b"}, {Repo: "c/d"}}
	if !forkIndexHas(rows, "c/d") {
		t.Fatal("forkIndexHas missed a row")
	}
	if forkIndexHas(rows, "x/y") {
		t.Fatal("forkIndexHas invented a row")
	}
}

func TestManifestCheckListErrors(t *testing.T) {
	ctx := context.Background()
	base := store.NewMemory()
	put(t, base, "repos/a/b/meta/stats.json", []byte(`{}`))

	// checkpoints/ prefix listing fails.
	s := newScanner(newErrStore(base).failOn("ListPrefixes", 1), Options{})
	s.repos = []string{"a/b"}
	if err := s.checkManifests(ctx); err == nil {
		t.Fatal("prefix error not returned")
	}
	// The log/ fallback listing fails.
	s2 := newScanner(newErrStore(base).failOn("List", 1), Options{})
	s2.repos = []string{"a/b"}
	if err := s2.checkManifests(ctx); err == nil {
		t.Fatal("log error not returned")
	}
	// Log history present: the repository is reported.
	s3 := newScanner(base, Options{})
	s3.repos = []string{"a/b"}
	put(t, base, "repos/a/b/log/0000000000000001.pb", []byte{0})
	if err := s3.checkManifests(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s3.report.Findings) != 1 || s3.report.Findings[0].Check != CheckManifest {
		t.Fatalf("findings = %+v, want repo.manifest", s3.report.Findings)
	}
}
