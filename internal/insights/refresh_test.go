// refresh_test.go — the I/O halves: Load (one GET), Refresh (the task body),
// staleness, the CAS write, and the bounded family reads. The store is the
// real memory backend; only the git scan and the serving-copy resolvers are
// faked, so the store discipline (exact keys, versioned writes) is exercised.
package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/store"
)

type fakeScanner struct {
	commits    []git.InsightsCommit
	truncated  bool
	languages  []git.InsightsLanguage
	langCapped bool
	err        error
	langErr    error
	calls      int
	langCalls  int
	rev        string
	since      time.Time
	cap        int
	langCap    int
}

func (f *fakeScanner) InsightsCommits(_ context.Context, _ *git.LocalRepo, rev string, since time.Time, capN int) ([]git.InsightsCommit, bool, error) {
	f.calls++
	f.rev, f.since, f.cap = rev, since, capN
	if f.err != nil {
		return nil, false, f.err
	}
	return f.commits, f.truncated, nil
}

func (f *fakeScanner) InsightsLanguages(_ context.Context, _ *git.LocalRepo, rev string, capN int) ([]git.InsightsLanguage, bool, error) {
	f.langCalls++
	f.rev, f.langCap = rev, capN
	if f.langErr != nil {
		return nil, false, f.langErr
	}
	return f.languages, f.langCapped, nil
}

// wrapStore lets a test fail one operation without reimplementing the store.
type wrapStore struct {
	store.ObjectStore
	getErr  error
	listErr error
	putFail int
	putErr  error
	puts    int
}

func (w *wrapStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if w.getErr != nil {
		return nil, w.getErr
	}
	return w.ObjectStore.Get(ctx, key, opts)
}

func (w *wrapStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if w.listErr != nil {
		return w.listErr
	}
	return w.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (w *wrapStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	w.puts++
	if w.putErr != nil {
		return store.ObjectMeta{}, w.putErr
	}
	if w.putFail > 0 {
		w.putFail--
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
	}
	return w.ObjectStore.Put(ctx, key, body, opts)
}

type fixture struct {
	svc        *Service
	st         *store.Memory
	scanner    *fakeScanner
	clock      time.Time
	head       string
	headErr    error
	serveErr   error
	serveCalls int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		st:      store.NewMemory(),
		scanner: &fakeScanner{},
		clock:   testNow,
		head:    "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
	}
	f.rebuild(t)
	return f
}

func (f *fixture) rebuild(t *testing.T) {
	t.Helper()
	f.serveCalls = 0
	f.svc = New(Deps{
		Store: f.st,
		Git:   f.scanner,
		Head: func(context.Context, string, string) (string, error) {
			return f.head, f.headErr
		},
		Serve: func(context.Context, string, string) (*git.LocalRepo, string, error) {
			f.serveCalls++
			if f.serveErr != nil {
				return nil, "", f.serveErr
			}
			return &git.LocalRepo{Path: "/tmp/serving"}, f.head, nil
		},
		Now: func() time.Time { return f.clock },
	})
}

func putJSON(t *testing.T, st store.ObjectStore, key string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), key, store.PutBody{Bytes: body}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
}

func seedCollab(t *testing.T, st store.ObjectStore) {
	t.Helper()
	putJSON(t, st, "repos/acme/api/issues/index.json", map[string]any{
		"version": 1, "compacted_through": "",
		"open": []map[string]any{
			{"num": 1, "kind": "pr", "state": "open", "title": "open pr", "author": "a",
				"created_at": "2026-11-20T10:00:00Z", "updated_at": "2026-11-20T10:00:00Z"},
		},
		"closed_recent": []map[string]any{
			{"num": 2, "kind": "pr", "state": "closed", "title": "merged pr", "author": "a",
				"created_at": "2026-11-01T10:00:00Z", "updated_at": "2026-11-10T10:00:00Z"},
			{"num": 3, "kind": "issue", "state": "closed", "title": "closed issue", "author": "a",
				"created_at": "2026-11-02T10:00:00Z", "updated_at": "2026-11-12T10:00:00Z"},
			// Reopened + reclosed: one card, counted once at its latest close.
			{"num": 3, "kind": "issue", "state": "closed", "title": "closed issue", "author": "a",
				"created_at": "2026-11-02T10:00:00Z", "updated_at": "2026-11-13T10:00:00Z"},
		},
	})
	putJSON(t, st, "repos/acme/api/pulls/000002/pr.json", map[string]any{
		"num": 2, "kind": "pr", "merged": true, "merged_at": "2026-11-10T10:00:00Z",
	})
}

func TestRefreshHappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedCollab(t, f.st)
	putJSON(t, f.st, "repos/acme/api/meta/social.json", map[string]any{"stars": 7, "watchers": 2, "forks": 3})
	putJSON(t, f.st, "repos/acme/api/releases/v1.json", map[string]any{"tag": "v1"})
	putJSON(t, f.st, "repos/acme/api/releases/v2.json", map[string]any{"tag": "v2"})
	putJSON(t, f.st, "repos/acme/api/releases/latest.json", map[string]any{"tag": "v2"})
	putJSON(t, f.st, "repos/acme/api/packages/registry/tags/latest", map[string]any{"digest": "sha256:x"})
	putJSON(t, f.st, "repos/acme/api/threads/runs/r1.json", map[string]any{"id": "r1"})
	f.scanner.commits = []git.InsightsCommit{
		commit(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), "a@x", "A", 10, 2),
		commit(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), "b@x", "B", 4, 1),
	}
	f.scanner.truncated = true
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	p, ok, err := f.svc.Load(ctx, "acme", "api")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if p.Version != Version || p.HeadSHA != f.head {
		t.Fatalf("projection = %+v", p)
	}
	if p.ComputedAt != testNow.Format(time.RFC3339) {
		t.Fatalf("computed_at = %q", p.ComputedAt)
	}
	if p.Scan.Commits != 2 || !p.Scan.Truncated || p.Scan.MaxCommits != DefaultMaxCommits {
		t.Fatalf("scan = %+v", p.Scan)
	}
	if p.Collab.PRSidecarReads != 1 || !p.Collab.Known || p.Collab.MergeScanCapped {
		t.Fatalf("collab = %+v", p.Collab)
	}
	w30 := p.Windows["30"]
	if w30 == nil || w30.Commits != 2 || w30.Additions != 14 || w30.Deletions != 3 {
		t.Fatalf("window 30 = %+v", w30)
	}
	if w30.PRsOpened != 2 || w30.PRsMerged != 1 || w30.IssuesOpened != 1 || w30.IssuesClosed != 1 {
		t.Fatalf("collab counts = %+v", w30)
	}
	if len(p.Windows["90"].Buckets) != 13 || len(p.Windows["365"].Buckets) != 13 {
		t.Fatal("all three windows must be in the projection")
	}
	want := Facts{Stars: 7, Watchers: 2, Forks: 3, Social: true,
		Releases: 2, ReleasesKnown: true, Packages: 1, PackagesKnown: true,
		ThreadsRuns: 1, ThreadsRunsKnown: true}
	if p.Facts != want {
		t.Fatalf("facts = %+v, want %+v", p.Facts, want)
	}
	if f.serveCalls != 1 {
		t.Fatalf("serve calls = %d", f.serveCalls)
	}
	if f.scanner.rev != f.head || !f.scanner.since.Equal(scanFloor(testNow)) {
		t.Fatalf("scan args = rev %q since %v", f.scanner.rev, f.scanner.since)
	}

	// A second refresh must UPDATE the existing object (the CAS path), not
	// fail its create.
	f.scanner.calls = 0
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if f.scanner.calls != 1 {
		t.Fatalf("second refresh did not rescan: %d", f.scanner.calls)
	}
}

func TestRefreshEmptyRepository(t *testing.T) {
	f := newFixture(t)
	f.head = ""
	if err := f.svc.Refresh(context.Background(), "acme", "empty", nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	p, ok, _ := f.svc.Load(context.Background(), "acme", "empty")
	if !ok || p.HeadSHA != "" || p.Scan.Commits != 0 {
		t.Fatalf("empty projection = %+v", p)
	}
	if f.scanner.calls != 0 || f.serveCalls != 0 || f.scanner.langCalls != 0 {
		t.Fatalf("empty repo must not run git: scan=%d lang=%d serve=%d", f.scanner.calls, f.scanner.langCalls, f.serveCalls)
	}
	if len(p.Windows["30"].Buckets) != 30 || p.Windows["30"].Commits != 0 {
		t.Fatalf("empty window = %+v", p.Windows["30"])
	}
	// Facts still read (an empty repo can have stars/releases).
	if !p.Facts.Social {
		t.Fatal("facts must be read for an empty repo")
	}
}

func TestRefreshIfStale(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// No projection → writes.
	refreshed, err := f.svc.RefreshIfStale(ctx, "acme", "api", nil)
	if err != nil || !refreshed {
		t.Fatalf("first = %v, %v", refreshed, err)
	}
	if f.scanner.calls != 1 {
		t.Fatalf("scan calls = %d", f.scanner.calls)
	}
	// Fresh + same head → no scan, no serve.
	f.serveCalls = 0
	refreshed, err = f.svc.RefreshIfStale(ctx, "acme", "api", nil)
	if err != nil || refreshed {
		t.Fatalf("second = %v, %v", refreshed, err)
	}
	if f.scanner.calls != 1 || f.serveCalls != 0 {
		t.Fatalf("fresh projection was rescanned: scan=%d serve=%d", f.scanner.calls, f.serveCalls)
	}
	// TTL expiry → refresh.
	f.clock = testNow.Add(25 * time.Hour)
	refreshed, err = f.svc.RefreshIfStale(ctx, "acme", "api", nil)
	if err != nil || !refreshed {
		t.Fatalf("ttl = %v, %v", refreshed, err)
	}
	// Head move → refresh.
	f.clock = testNow
	f.head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	refreshed, err = f.svc.RefreshIfStale(ctx, "acme", "api", nil)
	if err != nil || !refreshed {
		t.Fatalf("head = %v, %v", refreshed, err)
	}
}

func TestLoadShapes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if p, ok, err := f.svc.Load(ctx, "acme", "none"); p != nil || ok || err != nil {
		t.Fatalf("absent = %+v %v %v", p, ok, err)
	}
	// Corrupt JSON is an error (the page says unavailable).
	if _, err := f.st.Put(ctx, Key("acme", "bad"), store.PutBody{Bytes: []byte("{")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Load(ctx, "acme", "bad"); err == nil {
		t.Fatal("corrupt projection must error")
	}
	// An unknown version is treated as "not available" (fail closed).
	putJSON(t, f.st, Key("acme", "future"), map[string]any{"version": 99})
	if p, ok, err := f.svc.Load(ctx, "acme", "future"); p != nil || ok || err != nil {
		t.Fatalf("future version = %+v %v %v", p, ok, err)
	}
	// Nil service / nil store.
	var nilSvc *Service
	if _, _, err := nilSvc.Load(ctx, "a", "b"); err != nil {
		t.Fatalf("nil service: %v", err)
	}
}

func TestRefreshCollabShapes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Absent index → known, all zero.
	ct := f.svc.readCollab(ctx, "acme", "api", testNow)
	if !ct.known || len(ct.prsOpened) != 0 {
		t.Fatalf("absent index = %+v", ct)
	}
	// Compacted index sets the bounded flag; a closed issue is counted once.
	putJSON(t, f.st, "repos/acme/api/issues/index.json", map[string]any{
		"compacted_through": "00000a",
		"open":              []map[string]any{},
		"closed_recent": []map[string]any{
			{"num": 3, "kind": "issue", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-05T00:00:00Z"},
			{"num": 3, "kind": "issue", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-06T00:00:00Z"},
		},
	})
	ct = f.svc.readCollab(ctx, "acme", "api", testNow)
	if !ct.known || !ct.indexBounded {
		t.Fatalf("compacted = %+v", ct)
	}
	if len(ct.issuesOpened) != 1 || len(ct.issuesClosed) != 1 {
		t.Fatalf("duplicate card double-counted: %+v", ct)
	}
	// Corrupt index → unknown, never zero.
	if _, err := f.st.Put(ctx, "repos/acme/api/issues/index.json", store.PutBody{Bytes: []byte("{")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if ct = f.svc.readCollab(ctx, "acme", "api", testNow); ct.known {
		t.Fatal("corrupt index must be unknown")
	}
}

func TestReadCollabMergeCap(t *testing.T) {
	f := newFixture(t)
	f.svc.d.MergeScanCap = 2
	ctx := context.Background()
	putJSON(t, f.st, "repos/acme/api/issues/index.json", map[string]any{
		"open": []map[string]any{},
		"closed_recent": []map[string]any{
			{"num": 1, "kind": "pr", "state": "closed", "created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-02T00:00:00Z"},
			{"num": 2, "kind": "pr", "state": "closed", "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-02T00:00:00Z"},
			{"num": 3, "kind": "pr", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
			// Older than the widest window: never read.
			{"num": 4, "kind": "pr", "state": "closed", "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-02T00:00:00Z"},
		},
	})
	for _, num := range []int{2, 3, 4} {
		putJSON(t, f.st, fmt.Sprintf("repos/acme/api/pulls/%06x/pr.json", num), map[string]any{
			"num": num, "kind": "pr", "merged": true, "merged_at": "2026-11-02T00:00:00Z",
		})
	}
	ct := f.svc.readCollab(ctx, "acme", "api", testNow)
	if ct.sidecarReads != 2 || !ct.mergeScanCapped {
		t.Fatalf("reads=%d capped=%v", ct.sidecarReads, ct.mergeScanCapped)
	}
	if len(ct.prsMerged) != 2 {
		t.Fatalf("merged = %d", len(ct.prsMerged))
	}
	// A missing sidecar is skipped, not a merge.
	putJSON(t, f.st, "repos/acme/api/issues/index.json", map[string]any{
		"open": []map[string]any{},
		"closed_recent": []map[string]any{
			{"num": 8, "kind": "pr", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
		},
	})
	ct = f.svc.readCollab(ctx, "acme", "api", testNow)
	if len(ct.prsMerged) != 0 || ct.sidecarReads != 1 {
		t.Fatalf("missing sidecar = %+v", ct)
	}
	// merged with no merged_at falls back to the close/update stamp.
	putJSON(t, f.st, "repos/acme/api/pulls/000008/pr.json", map[string]any{"num": 8, "merged": true})
	ct = f.svc.readCollab(ctx, "acme", "api", testNow)
	if len(ct.prsMerged) != 1 || !ct.prsMerged[0].Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("fallback merged = %+v", ct.prsMerged)
	}
}

func TestReadFactsCapAndUnknown(t *testing.T) {
	f := newFixture(t)
	f.svc.d.FactCap = 2
	ctx := context.Background()
	for _, tag := range []string{"v1", "v2", "v3"} {
		putJSON(t, f.st, "repos/acme/api/releases/"+tag+".json", map[string]any{"tag": tag})
	}
	for _, tag := range []string{"a", "b", "c"} {
		putJSON(t, f.st, "repos/acme/api/packages/registry/tags/"+tag, map[string]any{"digest": "x"})
	}
	for _, run := range []string{"r1", "r2", "r3"} {
		putJSON(t, f.st, "repos/acme/api/threads/runs/"+run+".json", map[string]any{"id": run})
	}
	got := f.svc.readFacts(ctx, "acme", "api")
	if got.Releases != 2 || !got.ReleasesCapped || !got.ReleasesKnown {
		t.Fatalf("releases = %+v", got)
	}
	if got.Packages != 2 || !got.PackagesCapped || !got.PackagesKnown {
		t.Fatalf("packages = %+v", got)
	}
	if got.ThreadsRuns != 2 || !got.ThreadsRunsCapped || !got.ThreadsRunsKnown {
		t.Fatalf("runs = %+v", got)
	}
	if !got.Social || got.Stars != 0 {
		t.Fatalf("absent social must be verified zero: %+v", got)
	}
	// A LIST failure is unknown, not zero.
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, listErr: errors.New("list down")}
	got = f.svc.readFacts(ctx, "acme", "api")
	if got.ReleasesKnown || got.PackagesKnown || got.ThreadsRunsKnown {
		t.Fatalf("list failure must be unknown: %+v", got)
	}
}

func TestRefreshErrors(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.headErr = errors.New("head down")
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err == nil {
		t.Fatal("head error must fail the refresh")
	}
	f = newFixture(t)
	f.serveErr = errors.New("serve down")
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err == nil {
		t.Fatal("serve error must fail the refresh")
	}
	f = newFixture(t)
	f.scanner.err = errors.New("scan down")
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err == nil {
		t.Fatal("scan error must fail the refresh")
	}
	// A store write failure fails the refresh.
	f = newFixture(t)
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, putFail: 99}
	if err := f.svc.Refresh(ctx, "acme", "api", nil); err == nil {
		t.Fatal("write failure must fail the refresh")
	}
	// Unwired service.
	if err := New(Deps{}).Refresh(ctx, "a", "b", nil); err == nil {
		t.Fatal("unwired service must fail")
	}
}

func TestPutCASRetries(t *testing.T) {
	f := newFixture(t)
	ws := &wrapStore{ObjectStore: f.st, putFail: 2}
	f.svc.d.Store = ws
	if err := f.svc.put(context.Background(), Key("acme", "api"), []byte(`{"version":1}`)); err != nil {
		t.Fatalf("put retry: %v", err)
	}
	if ws.puts != 3 {
		t.Fatalf("puts = %d, want 3", ws.puts)
	}
	// The retry re-reads and updates the created object.
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, putFail: 99}
	if err := f.svc.put(context.Background(), Key("acme", "api"), []byte(`{"version":1}`)); err == nil {
		t.Fatal("exhausted CAS must fail")
	}
}

func TestRefreshServingCopyMissing(t *testing.T) {
	f := newFixture(t)
	f.svc.d.Serve = func(context.Context, string, string) (*git.LocalRepo, string, error) {
		return nil, f.head, nil
	}
	if err := f.svc.Refresh(context.Background(), "acme", "api", nil); err == nil {
		t.Fatal("nil serving copy must fail")
	}
}

func TestReadCollabContextCancelled(t *testing.T) {
	f := newFixture(t)
	putJSON(t, f.st, "repos/acme/api/issues/index.json", map[string]any{
		"open": []map[string]any{},
		"closed_recent": []map[string]any{
			{"num": 1, "kind": "pr", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ct := f.svc.readCollab(ctx, "acme", "api", testNow); ct.known {
		t.Fatalf("cancelled read must be unknown: %+v", ct)
	}
}

func TestNewDefaultsAndStaleMethod(t *testing.T) {
	// Stale is evaluated with the service clock; using testNow for the stamp
	// but the wall clock for "now" made this test go stale exactly 24h after
	// testNow was written (2026-11-22).
	s := New(Deps{Now: func() time.Time { return testNow }})
	if s.d.MaxCommits != DefaultMaxCommits || s.d.TTL != DefaultTTL ||
		s.d.MergeScanCap != DefaultMergeScanCap || s.d.FactCap != DefaultFactCap || s.d.Now == nil {
		t.Fatalf("defaults = %+v", s.d)
	}
	if !s.Stale(nil, "x") {
		t.Fatal("nil projection must be stale")
	}
	p := &Projection{Version: Version, HeadSHA: "x", ComputedAt: testNow.Format(time.RFC3339)}
	if s.Stale(p, "x") {
		t.Fatal("fresh projection reported stale")
	}
}

func TestStaleZeroTTL(t *testing.T) {
	p := &Projection{Version: Version, HeadSHA: "x", ComputedAt: testNow.Add(-time.Hour).Format(time.RFC3339)}
	if Stale(p, "x", testNow, 0) {
		t.Fatal("zero TTL must fall back to the default")
	}
	if _, ok := parseStamp("nope"); ok {
		t.Fatal("bad stamp parsed")
	}
}

func TestLoadStoreError(t *testing.T) {
	f := newFixture(t)
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, getErr: errors.New("down")}
	if _, _, err := f.svc.Load(context.Background(), "acme", "api"); err == nil {
		t.Fatal("store error must surface")
	}
	if _, err := f.svc.RefreshIfStale(context.Background(), "acme", "api", nil); err == nil {
		t.Fatal("stale check load failure must surface")
	}
}

type headErrStore struct{ *wrapStore }

func (h headErrStore) Head(context.Context, string) (*store.ObjectMeta, error) {
	return nil, errors.New("head down")
}

func TestPutHeadAndWriteErrors(t *testing.T) {
	f := newFixture(t)
	f.svc.d.Store = headErrStore{&wrapStore{ObjectStore: f.st}}
	if err := f.svc.put(context.Background(), Key("a", "b"), []byte("{}")); err == nil {
		t.Fatal("head error must surface")
	}
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, putErr: errors.New("write down")}
	if err := f.svc.put(context.Background(), Key("a", "b"), []byte("{}")); err == nil {
		t.Fatal("write error must surface")
	}
}

func TestReadCollabBadSidecars(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	putJSON(t, f.st, "repos/acme/api/issues/index.json", map[string]any{
		"open": []map[string]any{},
		"closed_recent": []map[string]any{
			{"num": 1, "kind": "pr", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
			{"num": 2, "kind": "pr", "state": "closed", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
		},
	})
	if _, err := f.st.Put(ctx, "repos/acme/api/pulls/000001/pr.json", store.PutBody{Bytes: []byte("{")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	putJSON(t, f.st, "repos/acme/api/pulls/000002/pr.json", map[string]any{"merged": true, "merged_at": "junk"})
	ct := f.svc.readCollab(ctx, "acme", "api", testNow)
	if len(ct.prsMerged) != 1 || ct.sidecarReads != 2 {
		t.Fatalf("bad sidecars = %+v", ct)
	}
	// A broken social read is unknown, not verified zero.
	f.svc.d.Store = &wrapStore{ObjectStore: f.st, getErr: errors.New("down")}
	if got := f.svc.readFacts(ctx, "acme", "api"); got.Social {
		t.Fatalf("social read failure must be unknown: %+v", got)
	}
	nopLogger{}.Notice("silent")
	nopLogger{}.Progress("silent", 0, 0, "")
}

func TestRefreshPopulatesLanguages(t *testing.T) {
	f := newFixture(t)
	f.scanner.languages = []git.InsightsLanguage{
		{Name: "Go", Bytes: 150, Files: 3},
		{Name: "JavaScript", Bytes: 50, Files: 1},
		{Name: "Other", Bytes: 0, Files: 1},
	}
	if err := f.svc.Refresh(context.Background(), "acme", "api", nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if f.scanner.langCalls != 1 || f.scanner.langCap != git.InsightsLanguageScanMax {
		t.Fatalf("language scan = %d calls cap %d", f.scanner.langCalls, f.scanner.langCap)
	}
	p, ok, err := f.svc.Load(context.Background(), "acme", "api")
	if err != nil || !ok {
		t.Fatalf("Load: %v %v", ok, err)
	}
	if len(p.Languages) != 2 {
		t.Fatalf("languages = %+v", p.Languages)
	}
	if p.Languages[0].Name != "Go" || p.Languages[0].Pct != 75 {
		t.Fatalf("Go row = %+v", p.Languages[0])
	}
	if p.Languages[1].Name != "JavaScript" || p.Languages[1].Pct != 25 {
		t.Fatalf("JavaScript row = %+v", p.Languages[1])
	}
	// The body carries the additive field under its pinned JSON name.
	raw, _, err := store.GetBytes(context.Background(), f.st, Key("acme", "api"), store.GetOptions{})
	if err != nil || !strings.Contains(string(raw), `"languages":[`) {
		t.Fatalf("projection JSON missing languages: %v %s", err, raw)
	}

	f.scanner.langCapped = true
	if err := f.svc.Refresh(context.Background(), "acme", "api", nil); err != nil {
		t.Fatalf("capped Refresh: %v", err)
	}
	p, _, _ = f.svc.Load(context.Background(), "acme", "api")
	if !p.LanguagesCapped {
		t.Fatal("capped language scan must be recorded")
	}
}

func TestRefreshLanguagesError(t *testing.T) {
	f := newFixture(t)
	f.scanner.langErr = errors.New("ls-tree down")
	if err := f.svc.Refresh(context.Background(), "acme", "api", nil); err == nil {
		t.Fatal("language scan failure must fail the refresh")
	}
	if _, ok, _ := f.svc.Load(context.Background(), "acme", "api"); ok {
		t.Fatal("a failed refresh must write no projection")
	}
}

func TestLoadOldProjectionWithoutLanguages(t *testing.T) {
	f := newFixture(t)
	// A projection written before the additive fields existed: version 1,
	// no languages, no last_release_at. It must load; the page hides the row.
	old := `{"version":1,"computed_at":"2026-11-20T00:00:00Z","head_sha":"` + f.head + `","windows":{},"scan":{"commits":0,"max_commits":20000},"collab":{"known":true},"facts":{"releases_known":true,"releases":0}}`
	if _, err := f.st.Put(context.Background(), Key("acme", "old"), store.PutBody{Bytes: []byte(old)}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := f.svc.Load(context.Background(), "acme", "old")
	if err != nil || !ok {
		t.Fatalf("old projection = %+v %v %v", p, ok, err)
	}
	if len(p.Languages) != 0 || p.LanguagesCapped {
		t.Fatalf("old projection grew languages: %+v", p)
	}
	if p.Facts.LastReleaseAt != "" {
		t.Fatalf("old projection grew a release stamp: %+v", p.Facts)
	}
}

func TestReadFactsLastRelease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	putJSON(t, f.st, "repos/acme/api/releases/latest.json", map[string]any{
		"tag": "v2", "created_at": "2026-08-01T12:00:00Z",
	})
	got := f.svc.readFacts(ctx, "acme", "api")
	if got.LastReleaseAt != "2026-08-01T12:00:00Z" {
		t.Fatalf("last release = %+v", got)
	}
	// A malformed pointer is ignored, never guessed.
	if _, err := f.st.Put(ctx, "repos/acme/api/releases/latest.json", store.PutBody{Bytes: []byte("{")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if again := f.svc.readFacts(ctx, "acme", "api"); again.LastReleaseAt != "" {
		t.Fatalf("corrupt pointer = %+v", again)
	}
}
