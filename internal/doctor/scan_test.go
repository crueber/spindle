package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// errStore fails the Nth call of one operation (1-based) and delegates
// everything else to the memory backend.
type errStore struct {
	store.ObjectStore
	fail  map[string]int
	calls map[string]int
}

func newErrStore(inner store.ObjectStore) *errStore {
	return &errStore{ObjectStore: inner, fail: map[string]int{}, calls: map[string]int{}}
}

func (e *errStore) failOn(op string, n int) *errStore {
	e.fail[op] = n
	return e
}

func (e *errStore) boom(op string) error {
	e.calls[op]++
	if n, ok := e.fail[op]; ok && e.calls[op] == n {
		return errors.New("boom " + op)
	}
	return nil
}

func (e *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if err := e.boom("Get"); err != nil {
		return nil, err
	}
	return e.ObjectStore.Get(ctx, key, opts)
}

func (e *errStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if err := e.boom("Head"); err != nil {
		return nil, err
	}
	return e.ObjectStore.Head(ctx, key)
}

func (e *errStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if err := e.boom("List"); err != nil {
		return err
	}
	return e.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (e *errStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if err := e.boom("ListPrefixes"); err != nil {
		return err
	}
	return e.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

// syntheticListStore yields n objects from List without storing them.
type syntheticListStore struct {
	*store.Memory
	n int
}

func (s *syntheticListStore) List(_ context.Context, _, _ string, fn func(store.ObjectMeta) error) error {
	for i := 0; i < s.n; i++ {
		if err := fn(store.ObjectMeta{Key: fmt.Sprintf("repos/a/b/issues/%06d/thread.json", i)}); err != nil {
			return err
		}
	}
	return nil
}

func newScanner(st store.ObjectStore, opts Options) *scanner {
	opts = opts.withDefaults()
	return &scanner{st: st, opts: opts, report: NewReport(opts), families: map[string]*FamilyReport{}}
}

func TestRunRejectsNilStore(t *testing.T) {
	if _, err := Run(context.Background(), nil, Options{}); err == nil {
		t.Fatal("Run(nil store) = nil error")
	}
}

func TestRunStoreErrors(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifest(t, st, "acme/app")

	cases := []struct {
		name string
		err  *errStore
	}{
		{"list owners", newErrStore(st).failOn("ListPrefixes", 1)},
		{"list repos", newErrStore(st).failOn("ListPrefixes", 2)},
		{"exact get", newErrStore(st).failOn("Get", 1)},
		{"family list", newErrStore(st).failOn("List", 1)},
	}
	for _, tc := range cases {
		if _, err := Run(ctx, tc.err, Options{}); err == nil {
			t.Errorf("%s: Run = nil error", tc.name)
		}
	}
}

func TestScannerErrorPaths(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifest(t, st, "acme/app")

	t.Run("exactInto get", func(t *testing.T) {
		s := newScanner(newErrStore(st).failOn("Get", 1), Options{})
		if err := s.exactInto(ctx, families()[0], s.familyReport("manifest"), "repos/acme/app/manifest.pb"); err == nil {
			t.Fatal("exactInto = nil error")
		}
	})
	t.Run("decodeKey get", func(t *testing.T) {
		s := newScanner(newErrStore(st).failOn("Get", 1), Options{})
		if err := s.decodeKey(ctx, families()[0], s.familyReport("manifest"), "repos/acme/app/manifest.pb"); err == nil {
			t.Fatal("decodeKey = nil error")
		}
	})
	t.Run("listInto list", func(t *testing.T) {
		s := newScanner(newErrStore(st).failOn("List", 1), Options{})
		if err := s.listInto(ctx, families()[0], s.familyReport("x"), "repos/", nil); err == nil {
			t.Fatal("listInto = nil error")
		}
	})
	t.Run("readObject get", func(t *testing.T) {
		s := newScanner(newErrStore(st).failOn("Get", 1), Options{})
		if _, _, err := s.readObject(ctx, "x"); err == nil {
			t.Fatal("readObject = nil error")
		}
	})
	t.Run("exists head", func(t *testing.T) {
		s := newScanner(newErrStore(st).failOn("Head", 1), Options{})
		if _, err := s.exists(ctx, "x"); err == nil {
			t.Fatal("exists = nil error")
		}
	})
	t.Run("decodeKey notfound", func(t *testing.T) {
		s := newScanner(st, Options{})
		if err := s.decodeKey(ctx, families()[0], s.familyReport("manifest"), "repos/ghost/repo/manifest.pb"); err != nil {
			t.Fatalf("notfound = %v", err)
		}
	})
	t.Run("budget exhausted", func(t *testing.T) {
		s := newScanner(st, Options{Budget: 1})
		s.decoded = 1
		if err := s.decodeKey(ctx, families()[0], s.familyReport("manifest"), "repos/acme/app/manifest.pb"); err != nil {
			t.Fatal(err)
		}
		if !s.budgetExhausted {
			t.Fatal("budget not marked exhausted")
		}
		if raw, ok, err := s.readObject(ctx, "x"); err != nil || ok || raw != nil {
			t.Fatal("readObject after budget")
		}
	})
	t.Run("exact budget and limit", func(t *testing.T) {
		s := newScanner(st, Options{Budget: 1})
		s.decoded = 1
		if err := s.exactInto(ctx, families()[0], s.familyReport("manifest"), "repos/acme/app/manifest.pb"); err != nil {
			t.Fatal(err)
		}
		s2 := newScanner(st, Options{Limit: 1})
		fr := s2.familyReport("manifest")
		fr.Sampled = 1
		if err := s2.exactInto(ctx, families()[0], fr, "repos/acme/app/manifest.pb"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("exact notfound", func(t *testing.T) {
		s := newScanner(st, Options{})
		if err := s.exactInto(ctx, families()[0], s.familyReport("manifest"), "repos/ghost/repo/manifest.pb"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestListIsCountCapped(t *testing.T) {
	st := &syntheticListStore{Memory: store.NewMemory(), n: maxFamilyCount + 1}
	s := newScanner(st, Options{Limit: 2})
	fr := s.familyReport("collab")
	if err := s.listInto(context.Background(), family{name: "collab"}, fr, "repos/a/b/issues/", nil); err != nil {
		t.Fatalf("listInto: %v", err)
	}
	if !fr.Truncated {
		t.Fatalf("family = %+v, want truncated", fr)
	}
	if fr.Total != maxFamilyCount {
		t.Fatalf("total = %d, want %d", fr.Total, maxFamilyCount)
	}
	if fr.Sampled != 0 {
		t.Fatalf("sampled = %d, want 0 (objects are not stored)", fr.Sampled)
	}
}

func TestRepoCapIsReported(t *testing.T) {
	st := store.NewMemory()
	putManifest(t, st, "acme/one")
	putManifest(t, st, "acme/two")
	rep, err := Run(context.Background(), st, Options{Repos: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Summary.ReposSampled != 1 || rep.Summary.ReposTotal != 2 || !rep.Summary.ReposTruncated {
		t.Fatalf("summary = %+v, want 1 of 2 truncated", rep.Summary)
	}
}

func TestJudgeSizeOutlier(t *testing.T) {
	s := newScanner(store.NewMemory(), Options{})
	fr := s.familyReport("x")
	s.judge(family{name: "x", maxSize: 1}, fr, "k", []byte("toolong"), 7)
	if len(s.report.Findings) != 1 || s.report.Findings[0].Check != CheckSize {
		t.Fatalf("findings = %+v, want one size finding", s.report.Findings)
	}
	if fr.Findings != 1 {
		t.Fatalf("family findings = %d", fr.Findings)
	}
	// maxSize 0 and a nil decoder are no-ops.
	s.judge(family{name: "y"}, fr, "k", nil, 100)
}

func TestWriteTextBranchesAndCleanWord(t *testing.T) {
	rep := NewReport(Options{})
	rep.Add(Finding{Check: CheckDecode, Key: "k", Reason: "r"})
	rep.Families = []FamilyReport{{Family: "x", Sampled: 1, Total: 2, Truncated: true}}
	rep.Summary.ReposSampled, rep.Summary.ReposTotal, rep.Summary.ReposTruncated = 1, 2, true
	rep.Summary.BudgetExhausted = true
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"cap reached", "budget reached", "not clean"} {
		if !strings.Contains(out, want) {
			t.Errorf("text missing %q:\n%s", want, out)
		}
	}
	// A writer that fails on the Nth call covers each error return.
	for n := 0; n < 5; n++ {
		if err := rep.WriteText(&failWriter{okWrites: n}); err == nil {
			t.Errorf("writer failing on write %d: nil error", n)
		}
	}
	if cleanWord(0) != "clean" {
		t.Fatal("cleanWord(0)")
	}
}

type failWriter struct{ okWrites int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.okWrites <= 0 {
		return 0, io.ErrClosedPipe
	}
	w.okWrites--
	return len(p), nil
}

func TestSortedFamilyNamesIncludesUnknown(t *testing.T) {
	fam := map[string]*FamilyReport{
		"zeta":     {Family: "zeta"},
		"manifest": {Family: "manifest"},
	}
	got := sortedFamilyNames(fam)
	if len(got) != 2 || got[0] != "manifest" || got[1] != "zeta" {
		t.Fatalf("names = %v", got)
	}
}

func TestOrphanErrorPaths(t *testing.T) {
	ctx := context.Background()
	base := store.NewMemory()
	putManifest(t, base, "a/b")
	putJSON(t, base, "repos/a/b/fork.json", map[string]any{"parent": "p/q", "version": 1})
	putJSON(t, base, "repos/a/b/threads/runs/r/artifacts/out/manifest.json", map[string]any{
		"name": "out", "run": "r", "job": "j",
	})
	putJSON(t, base, "threads/queue/index.json", map[string]any{"version": 1, "queued": []any{map[string]any{"repo": "a/b", "run": "r", "job": "j"}}})
	putJSON(t, base, "moderation/reports/index.json", map[string]any{"version": 1, "open": []any{map[string]any{"id": 1, "repo": "a/b"}}})

	mk := func(e *errStore) *scanner {
		s := newScanner(e, Options{})
		s.repos = []string{"a/b"}
		return s
	}
	cases := []struct {
		name string
		err  *errStore
		run  func(*scanner) error
	}{
		{"artifact list", newErrStore(base).failOn("List", 1), func(s *scanner) error { return s.checkArtifactOrphans(ctx) }},
		{"artifact head", newErrStore(base).failOn("Head", 1), func(s *scanner) error { return s.checkArtifactOrphans(ctx) }},
		{"queue get", newErrStore(base).failOn("Get", 1), func(s *scanner) error { return s.checkQueueOrphans(ctx) }},
		{"queue head", newErrStore(base).failOn("Head", 1), func(s *scanner) error { return s.checkQueueOrphans(ctx) }},
		{"report get", newErrStore(base).failOn("Get", 1), func(s *scanner) error { return s.checkReportOrphans(ctx) }},
		{"report head", newErrStore(base).failOn("Head", 1), func(s *scanner) error { return s.checkReportOrphans(ctx) }},
		{"fork get", newErrStore(base).failOn("Get", 1), func(s *scanner) error { return s.checkForkOrphans(ctx) }},
		{"fork head", newErrStore(base).failOn("Head", 1), func(s *scanner) error { return s.checkForkOrphans(ctx) }},
		{"fork index get", newErrStore(base).failOn("Get", 2), func(s *scanner) error { return s.checkForkOrphans(ctx) }},
		{"manifest head", newErrStore(base).failOn("Head", 1), func(s *scanner) error { return s.checkManifests(ctx) }},
	}
	for _, tc := range cases {
		if err := tc.run(mk(tc.err)); err == nil {
			t.Errorf("%s: nil error", tc.name)
		}
	}
}

func TestOrphanOddRows(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifest(t, st, "a/b")
	// Queue row with empty fields.
	putJSON(t, st, "threads/queue/index.json", map[string]any{
		"version": 1, "queued": []any{map[string]any{}, map[string]any{"repo": "a/b", "run": "", "job": ""}},
	})
	// Global report row with no repo, and an invalid per-repo index.
	putJSON(t, st, "moderation/reports/index.json", map[string]any{
		"version": 1, "open": []any{map[string]any{"id": 1}},
	})
	put(t, st, "repos/a/b/meta/moderation/reports/index.json", []byte(`{`))
	// Invalid fork.json + invalid parent index + empty child row.
	put(t, st, "repos/a/b/fork.json", []byte(`{`))
	putJSON(t, st, "repos/a/b/meta/forks.json", map[string]any{
		"version": 1, "forks": []any{map[string]any{"repo": ""}, map[string]any{"repo": "ghost/child"}},
	})
	putJSON(t, st, "repos/ghost/child/fork.json", map[string]any{"parent": "someone/else", "version": 1})

	s := newScanner(st, Options{})
	s.repos = []string{"a/b"}
	if err := s.checkOrphans(ctx); err != nil {
		t.Fatalf("checkOrphans: %v", err)
	}
	// ghost/child is listed but names a different parent -> one finding.
	found := false
	for _, f := range s.report.Findings {
		if f.Check == CheckFork && f.Key == "repos/ghost/child/fork.json" && strings.Contains(f.Reason, "names parent") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want the mismatched parent", s.report.Findings)
	}
}

func TestArtifactRunParsing(t *testing.T) {
	if repo, run := artifactRun("repos/a/b/threads/runs/r1/artifacts/out/manifest.json"); repo != "a/b" || run != "r1" {
		t.Fatalf("artifactRun = %q %q", repo, run)
	}
	if _, run := artifactRun("repos/a/b/artifacts/manifest.json"); run != "" {
		t.Fatalf("non-run key parsed as %q", run)
	}
}

func TestManifestCheckWithoutRefsIsQuiet(t *testing.T) {
	st := store.NewMemory()
	putJSON(t, st, "repos/stale/only/meta/stats.json", map[string]any{"version": 1})
	s := newScanner(st, Options{})
	s.repos = []string{"stale/only"}
	if err := s.checkManifests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none (sidecar-only prefix is expected)", s.report.Findings)
	}
}

func TestConfigFindingHelpers(t *testing.T) {
	fs := ConfigFindings(
		[]string{"server.listen warning"},
		[]error{nil, errors.New("server.auth.issuer is required"), errors.New("server.auth.issuer is required")},
	)
	if len(fs) != 2 {
		t.Fatalf("findings = %+v, want error + warning (deduped)", fs)
	}
	if fs[0].Check != CheckConfig || fs[1].Check != CheckConfigWarn {
		t.Fatalf("checks = %+v", fs)
	}
	if lf := LoadFindings([]error{nil, errors.New("disk on fire")}); len(lf) != 1 || lf[0].Check != CheckConfigLoad || lf[0].Key != "config" {
		t.Fatalf("load findings = %+v", lf)
	}
	if ValidateConfig(nil) != nil {
		t.Fatal("ValidateConfig(nil) != nil")
	}
}
