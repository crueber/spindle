package moderation

// reports_cover_test.go — error paths and boundary branches of the M3 family
// (the per-package ≥95 % gate): fault-injected store writes, corrupt windows,
// the compaction task's failure arms, cursor edges and the HTTP method/route
// matrix. The happy paths live in reports_test.go / reports_http_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// flakyStore fails selected operations; everything else passes through.
type flakyStore struct {
	store.ObjectStore
	putErr    func(key string) error
	getErr    func(key string) error
	listErr   error
	listErrFn func(prefix string) error
}

func (f flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.putErr != nil {
		if err := f.putErr(key); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.getErr != nil {
		if err := f.getErr(key); err != nil {
			return nil, err
		}
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f flakyStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if f.listErrFn != nil {
		if err := f.listErrFn(prefix); err != nil {
			return err
		}
	}
	if f.listErr != nil {
		return f.listErr
	}
	return f.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

func boom(string) error { return errors.New("boom") }

func TestFileReportEvidenceFailureCompensates(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	s.Store = flakyStore{ObjectStore: s.Store, putErr: func(key string) error {
		if key == ReportRecordKey("acme", "widgets", 1) {
			return errors.New("record unavailable")
		}
		return nil
	}}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); err == nil {
		t.Fatal("record create failure must fail the filing")
	}
	// The compensation removed the open card: a retry is not a duplicate 409.
	ix, err := s.loadReportsIndex(ctx(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Open) != 0 {
		t.Fatalf("phantom card left behind: %+v", ix.Open)
	}
	s.Store = flakyStore{ObjectStore: s.Store, putErr: nil}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

func TestFileReportGlobalAppendFailureIsLogged(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	log := &captureLogger{}
	s.Logger = log
	s.Store = flakyStore{ObjectStore: s.Store, putErr: func(key string) error {
		if key == GlobalReportsKey() {
			return errors.New("global unavailable")
		}
		return nil
	}}
	card, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	})
	if err != nil || card.ID != 1 {
		t.Fatalf("filing = %+v %v", card, err)
	}
	if len(log.msgs) == 0 {
		t.Fatal("global append failure must be logged")
	}
}

func TestFileReportProbeAndIdentityErrors(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	s.Targets = fakeProbe{err: errors.New("probe down")}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("probe error = %v", err)
	}
	s.Targets = nil
	_, ns := testService(t)
	ns.err = errors.New("identity down")
	s.Namespaces = ns
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindUser, Target: ReportTargetRef{Principal: "carol"}, Reason: "spam",
	}); err == nil {
		t.Fatal("identity failure must fail closed")
	}
	// A blank reason is invalid.
	s.Namespaces = nil
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank reason = %v", err)
	}
	// Review and review_thread targets build their canonical keys.
	for _, in := range []ReportInput{
		{Kind: ReportKindReview, Target: ReportTargetRef{Num: 2, Seq: 1}, Reason: "spam"},
		{Kind: ReportKindReviewThread, Target: ReportTargetRef{Num: 2, Seq: 1, TID: "deadbeef"}, Reason: "spam"},
	} {
		if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, in); err != nil {
			t.Fatalf("%s = %v", in.Kind, err)
		}
	}
}

func TestResolveReportHideFailureLeavesReportOpen(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	card := fileThreadReport(t, s, "bob", 12, 3)
	// The target disappears in its family: the hide probe refuses after the
	// report was loaded, so the index must still show it open.
	s.Targets = fakeProbe{keys: map[string]bool{}}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, auth.Principal{Name: "alice"}, ResolveHidden, ""); err == nil {
		t.Fatal("hide of a vanished target must fail")
	}
	ix, err := s.loadReportsIndex(ctx(), "acme", "widgets")
	if err != nil || len(ix.Open) != 1 {
		t.Fatalf("index = %+v %v", ix, err)
	}
}

func TestReportsWindowErrorPaths(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	// Corrupt repo index fails closed.
	if _, err := store.PutBytes(ctx(), s.Store, ReportsKey("acme", "widgets"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, "open", 0, 0); err == nil {
		t.Fatal("corrupt index must fail")
	}
	if _, _, err := s.ReportQueues(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, 0); err == nil {
		t.Fatal("corrupt queues must fail")
	}
	if _, err := s.State(ctx(), "acme", "widgets"); err != nil {
		t.Fatal(err) // the M2 document is a different object and stays readable
	}
	// Corrupt global index fails closed.
	if _, err := store.PutBytes(ctx(), s.Store, GlobalReportsKey(), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ListGlobalReports(ctx(), auth.Principal{Name: "root", Admin: true}, "open", "", 0); err == nil {
		t.Fatal("corrupt global index must fail")
	}
	// A missing global object reads empty.
	s2, _ := reportSvc(t, identity.RoleAdmin)
	if rows, more, err := s2.ListGlobalReports(ctx(), auth.Principal{Name: "root", Admin: true}, "open", "", 0); err != nil || rows != nil && len(rows) != 0 || more {
		t.Fatalf("empty global = %+v %v %v", rows, more, err)
	}
}

func TestReportStateFilterAndCursors(t *testing.T) {
	for in, want := range map[string]string{"": ReportOpen, "open": ReportOpen, "resolved": ReportResolved, "dismissed": ReportResolved, "ALL": "all"} {
		if got, err := ReportStateFilter(in); err != nil || got != want {
			t.Fatalf("%q = %q %v", in, got, err)
		}
	}
	if _, err := ReportStateFilter("closed"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed = %v", err)
	}
	// pageCards: negative cursor, page past the end, n bounds.
	if _, _, err := pageCards(nil, -1, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative cursor = %v", err)
	}
	rows := []ReportCard{{ID: 1}, {ID: 2}}
	if got, more, err := pageCards(rows, 0, 500); err != nil || more || len(got) != 2 {
		t.Fatalf("n cap = %+v %v %v", got, more, err)
	}
	if got, more, err := pageCards(rows, 9, 10); err != nil || more || len(got) != 0 {
		t.Fatalf("past end = %+v %v %v", got, more, err)
	}
	// pageGlobal: malformed cursor, page past the end, tie-break ordering.
	if _, _, err := pageGlobal(nil, "x|y", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor = %v", err)
	}
	g := []GlobalCard{
		{ReportCard: ReportCard{ID: 2, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "b/r"},
		{ReportCard: ReportCard{ID: 1, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "a/r"},
	}
	if !globalBefore(g[0], g[1]) {
		t.Fatal("repo tie-break")
	}
	g2 := []GlobalCard{
		{ReportCard: ReportCard{ID: 2, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "a/r"},
		{ReportCard: ReportCard{ID: 1, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "a/r"},
	}
	if !globalBefore(g2[0], g2[1]) {
		t.Fatal("id tie-break")
	}
	if _, _, err := pageGlobal(g, GlobalCursor(g[1]), 1); err != nil {
		t.Fatalf("cursor at end = %v", err)
	}
	// compactedTail keeps the previous watermark when there is no tail.
	if got := compactedTail(nil, "abc"); got != "abc" {
		t.Fatalf("empty tail = %q", got)
	}
	// normalize fills nil slices and the id allocator.
	ix := ReportsIndex{}
	normalizeReportsIndex(&ix)
	if ix.Open == nil || ix.ResolvedRecent == nil || ix.NextID != 1 {
		t.Fatalf("normalized = %+v", ix)
	}
	gx := GlobalReportsIndex{}
	normalizeGlobalIndex(&gx)
	if gx.Open == nil || gx.ResolvedRecent == nil {
		t.Fatalf("global normalized = %+v", gx)
	}
}

func TestReportsListedStatesAndQueues(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	c1 := fileThreadReport(t, s, "bob", 1, 1)
	c2 := fileThreadReport(t, s, "bob", 2, 1)
	admin := auth.Principal{Name: "alice"}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", c1.ID, admin, ResolveNone, ""); err != nil {
		t.Fatal(err)
	}
	// all = open + resolved; resolved = the tail only.
	all, _, err := s.ListReports(ctx(), "acme", "widgets", admin, "all", 0, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v %v", all, err)
	}
	resolved, _, err := s.ListReports(ctx(), "acme", "widgets", admin, "resolved", 0, 0)
	if err != nil || len(resolved) != 1 || resolved[0].ID != c1.ID {
		t.Fatalf("resolved = %+v %v", resolved, err)
	}
	open, tail, err := s.ReportQueues(ctx(), "acme", "widgets", admin, 1)
	if err != nil || len(open) != 1 || len(tail) != 1 || open[0].ID != c2.ID {
		t.Fatalf("queues = %+v/%+v %v", open, tail, err)
	}
	// Global all/resolved filters.
	global, _, err := s.ListGlobalReports(ctx(), auth.Principal{Name: "root", Admin: true}, "all", "", 0)
	if err != nil || len(global) != 2 {
		t.Fatalf("global all = %+v %v", global, err)
	}
	global, _, err = s.ListGlobalReports(ctx(), auth.Principal{Name: "root", Admin: true}, "resolved", "", 0)
	if err != nil || len(global) != 1 || global[0].ID != c1.ID {
		t.Fatalf("global resolved = %+v %v", global, err)
	}
}

func TestCompactReportsErrorArms(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	fileThreadReport(t, s, "bob", 1, 1)
	// LIST failure.
	s.Store = flakyStore{ObjectStore: s.Store, listErr: errors.New("list down")}
	if _, err := s.CompactReports(ctx()); err == nil {
		t.Fatal("list failure must surface")
	}
	s.Store = flakyStore{ObjectStore: s.Store}
	// A repo without an index is skipped (the owner prefix walk sees it).
	s2 := New(store.NewMemory())
	s2.Roles = &fakeRoles{role: identity.RoleAdmin}
	if _, err := store.PutBytes(ctx(), s2.Store, "repos/acme/other/README", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if n, err := s2.CompactReports(ctx()); err != nil || n != 0 {
		t.Fatalf("empty compact = %d %v", n, err)
	}
	// A corrupt per-repo index fails the rebuild closed.
	if _, err := store.PutBytes(ctx(), s2.Store, ReportsKey("acme", "bad"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CompactReports(ctx()); err == nil {
		t.Fatal("corrupt repo index must fail the rebuild")
	}
	// A failing global write surfaces.
	s3, _ := reportSvc(t, identity.RoleAdmin)
	fileThreadReport(t, s3, "bob", 1, 1)
	s3.Store = flakyStore{ObjectStore: s3.Store, putErr: func(key string) error {
		if key == GlobalReportsKey() {
			return errors.New("global down")
		}
		return nil
	}}
	if _, err := s3.CompactReports(ctx()); err == nil {
		t.Fatal("global write failure must surface")
	}
}

func TestCompactTaskErrorIsLogged(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	log := &chanLogger{ch: make(chan string, 1)}
	s.Logger = log
	s.Tasks = &failingTasks{started: make(chan struct{})}
	s.startCompactTask(ctx())
	select {
	case <-log.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("task failure must be logged")
	}
	// Nil Tasks is a no-op (no panic).
	s.Tasks = nil
	s.startCompactTask(ctx())
}

// chanLogger signals one warning on a buffered channel (no data races with a
// test that finishes before the background task).
type chanLogger struct{ ch chan string }

func (c *chanLogger) WarnContext(context.Context, string, ...any) {
	select {
	case c.ch <- "warn":
	default:
	}
}

// failingTasks returns an error without entering the task body and signals
// when Run was called.
type failingTasks struct {
	started chan struct{}
	once    sync.Once
}

func (f *failingTasks) Run(context.Context, string, string, map[string]string, func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	f.once.Do(func() { close(f.started) })
	return nil, errors.New("task table refusing")
}

func (f *failingTasks) Running(string, string) *wal.TaskRecord { return nil }

func TestReportsHTTPMethodAndRouteMatrix(t *testing.T) {
	sw := newSwitch(t, identity.RoleAdmin)
	sw.as(auth.Principal{Name: "root", Admin: true})
	hd := sw.h
	// Unclaimed moderation paths fall through (no route, never a write).
	for _, path := range []string{
		"/api/v1/moderation/unknown",
		"/api/v1/moderation/reports/a/b/1/resolve/extra",
		"/acme/widgets/api/moderation/reports/1",
		"/acme/widgets/api/moderation/other",
	} {
		if rec := do(hd, "GET", path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	// Wrong methods.
	if rec := doBody(hd, "POST", "/api/v1/moderation/reports", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("global POST = %d", rec.Code)
	}
	if rec := doBody(hd, "POST", "/api/v1/moderation/audit", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("audit POST = %d", rec.Code)
	}
	if rec := doBody(hd, "GET", "/api/v1/moderation/reports/acme/widgets/1/resolve", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("global resolve GET = %d", rec.Code)
	}
	if rec := doBody(hd, "POST", "/acme/widgets/api/moderation/reports/1", "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("repo item POST = %d", rec.Code)
	}
	if rec := doBody(hd, "PUT", "/acme/widgets/api/moderation/reports", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("repo PUT = %d", rec.Code)
	}
	// The global twin requires an id and a tenant.
	if rec := doBody(hd, "POST", "/api/v1/moderation/reports/a/b/x/resolve", `{"action":"none"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("global bad id = %d", rec.Code)
	}
	if rec := doBody(hd, "POST", "/api/v1/moderation/reports//b/1/resolve", `{"action":"none"}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Fatalf("global empty owner = %d", rec.Code)
	}
	if rec := doBody(hd, "POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("global bad body = %d", rec.Code)
	}
	// Cross-origin writes are refused on every write route.
	if rec := doBody(hd, "POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", `{"action":"none"}`); rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin request refused: %d", rec.Code)
	}
}

func TestRepoReportHTTPEdgeBranches(t *testing.T) {
	sw := newSwitch(t, identity.RoleAdmin)
	h := sw.as(auth.Principal{Name: "bob"})
	// Empty body decodes as {} → invalid (missing kind).
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d", rec.Code)
	}
	// Unreadable body (a body larger than the 1 MiB reader).
	big := make([]byte, 1<<21)
	for i := range big {
		big[i] = 'a'
	}
	req := httptest.NewRequest("POST", "/acme/widgets/api/moderation/reports", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if !h.Handle(rec, req) {
		rec.Code = http.StatusNotFound
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("huge body = %d", rec.Code)
	}
	// Unknown report id on the repo twin.
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/9/resolve", `{"action":"none"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", rec.Code)
	}
	// A report filed at one tenant is invisible to another (ids are per-repo).
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 1, 1)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d", rec.Code)
	}
	if rec := doBody(h, "POST", "/acme/other/api/moderation/reports/1/resolve", `{"action":"none"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign tenant = %d", rec.Code)
	}
}

func TestReportTargetGrammarErrors(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	// Review/review_thread probe failures surface (not a 400).
	s.Targets = fakeProbe{err: errors.New("probe down")}
	for _, in := range []ReportInput{
		{Kind: ReportKindReview, Target: ReportTargetRef{Num: 2, Seq: 1}, Reason: "spam"},
		{Kind: ReportKindReviewThread, Target: ReportTargetRef{Num: 2, Seq: 1, TID: "deadbeef"}, Reason: "spam"},
	} {
		if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, in); err == nil {
			t.Fatalf("%s probe error swallowed", in.Kind)
		}
	}
	// A bad thread id is a grammar error before the probe.
	s.Targets = fakeProbe{keys: map[string]bool{"review_thread": true}}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindReviewThread, Target: ReportTargetRef{Num: 2, Seq: 1, TID: "short"}, Reason: "spam",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad tid = %v", err)
	}
	// reportTargetOf answers every kind (the probe message).
	for _, tr := range []Target{
		{Kind: TargetThread, Num: 1, Seq: 2},
		{Kind: TargetReview, Num: 1, Seq: 2},
		{Kind: TargetReviewThread, Num: 1, TID: "deadbeef", Seq: 2},
	} {
		if reportTargetOf(tr) == "" {
			t.Fatalf("no target key for %+v", tr)
		}
	}
}

func TestReportNoteBounds(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	long := make([]byte, 2001)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam", Note: string(long),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long filing note = %v", err)
	}
	card := fileThreadReport(t, s, "bob", 1, 1)
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, auth.Principal{Name: "alice"}, ResolveNone, string(long)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long resolve note = %v", err)
	}
}

func TestReportPointersAndCursorEdges(t *testing.T) {
	if num, seq, tid := reportPointers(ReportKindUser, "user:bob"); num != 0 || seq != 0 || tid != "" {
		t.Fatalf("user pointers = %d %d %q", num, seq, tid)
	}
	if num, _, _ := reportPointers(ReportKindThread, "garbage"); num != 0 {
		t.Fatalf("bad target pointers = %d", num)
	}
	if _, _, _, err := parseGlobalCursor("a|b|zz"); err == nil {
		t.Fatal("bad hex cursor accepted")
	}
	if _, _, _, err := parseGlobalCursor("only|two"); err == nil {
		t.Fatal("short cursor accepted")
	}
	if globalBefore(GlobalCard{ReportCard: ReportCard{CreatedAt: "2026-11-20T10:00:00Z"}}, GlobalCard{ReportCard: ReportCard{CreatedAt: "2026-11-21T10:00:00Z"}}) {
		t.Fatal("older row sorted first")
	}
}

func TestGlobalWindowEdgeBranches(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	row := GlobalCard{ReportCard: ReportCard{ID: 1, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "acme/widgets"}
	if err := s.appendGlobalCard(ctx(), row); err != nil {
		t.Fatal(err)
	}
	// A duplicate append is a no-op (the retry shape).
	if err := s.appendGlobalCard(ctx(), row); err != nil {
		t.Fatal(err)
	}
	gx, _, _ := s.loadGlobalIndex(ctx())
	if len(gx.Open) != 1 {
		t.Fatalf("dup append = %+v", gx.Open)
	}
	// A resolve of a row the global window never saw is a no-op.
	if err := s.resolveGlobalCard(ctx(), "acme", "widgets", ReportCard{ID: 99}); err != nil {
		t.Fatal(err)
	}
	// A row already projected into the tail is a no-op.
	gx.ResolvedRecent = append(gx.ResolvedRecent, row)
	body, _ := json.Marshal(gx)
	meta, _ := s.Store.Head(ctx(), GlobalReportsKey())
	if _, err := store.PutBytes(ctx(), s.Store, GlobalReportsKey(), body, store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); err != nil {
		t.Fatal(err)
	}
	if err := s.resolveGlobalCard(ctx(), "acme", "widgets", ReportCard{ID: 1}); err != nil {
		t.Fatal(err)
	}
	// A mutate callback error surfaces (no write).
	if _, err := s.mutateGlobalIndex(ctx(), func(*GlobalReportsIndex) (bool, error) {
		return false, errors.New("mutation refused")
	}); err == nil {
		t.Fatal("mutation error swallowed")
	}
}

func TestWindowStoreErrors(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	base := s.Store
	s.Store = flakyStore{ObjectStore: base, getErr: func(key string) error {
		if key == ReportsKey("acme", "widgets") || key == GlobalReportsKey() {
			return errors.New("get down")
		}
		return nil
	}}
	if _, _, err := s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, "open", 0, 0); err == nil {
		t.Fatal("repo window get error swallowed")
	}
	if _, _, err := s.ListGlobalReports(ctx(), auth.Principal{Name: "root", Admin: true}, "open", "", 0); err == nil {
		t.Fatal("global window get error swallowed")
	}
	// A non-precondition PUT error surfaces from the repo CAS.
	s.Store = flakyStore{ObjectStore: base, putErr: func(key string) error {
		if key == ReportsKey("acme", "widgets") {
			return errors.New("put down")
		}
		return nil
	}}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); err == nil {
		t.Fatal("repo put error swallowed")
	}
	// Failing every write: the evidence Create fails, the compensation CAS
	// fails too, and the drop is logged (never a panic).
	log := &chanLogger{ch: make(chan string, 1)}
	s.Logger = log
	var mu sync.Mutex
	indexPuts := 0
	s.Store = flakyStore{ObjectStore: base, putErr: func(key string) error {
		if key == ReportRecordKey("acme", "widgets", 1) {
			return errors.New("record down")
		}
		if key == ReportsKey("acme", "widgets") {
			mu.Lock()
			indexPuts++
			n := indexPuts
			mu.Unlock()
			if n > 1 {
				return errors.New("index down")
			}
		}
		return nil
	}}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); err == nil {
		t.Fatal("record outage swallowed")
	}
	select {
	case <-log.ch:
	default:
		t.Fatal("compensation failure must be logged")
	}
}

func TestListRepoIndexesPrefixError(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	fileThreadReport(t, s, "bob", 1, 1)
	base := s.Store
	s.Store = flakyStore{ObjectStore: base, listErrFn: func(prefix string) error {
		if strings.HasPrefix(prefix, "repos/acme/") {
			return errors.New("repo prefix down")
		}
		return nil
	}}
	if _, err := s.CompactReports(ctx()); err == nil {
		t.Fatal("repo-prefix list error swallowed")
	}
}

func TestHTTPAuthErrorsAndCrossOrigin(t *testing.T) {
	svc, _ := testService(t)
	svc.Roles = &fakeRoles{role: identity.RoleAdmin}
	svc.Targets = fakeProbe{keys: map[string]bool{"thread": true}}
	for _, aerr := range []*auth.AuthError{
		{Kind: auth.ErrUnavailable, Why: "idp down"},
		{Kind: auth.ErrForbidden, Why: "no session"},
		{Kind: auth.ErrUnauthorized, Why: "sign in"},
	} {
		h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) { return auth.Principal{}, aerr }}
		rec := do(h, "GET", "/acme/widgets/api/moderation/reports")
		if rec.Code != map[auth.AuthErrorKind]int{auth.ErrUnavailable: 503, auth.ErrForbidden: 403, auth.ErrUnauthorized: 401}[aerr.Kind] {
			t.Fatalf("auth %v = %d", aerr.Kind, rec.Code)
		}
	}
	sw := newSwitch(t, identity.RoleAdmin)
	sw.as(auth.Principal{Name: "root", Admin: true})
	h := sw.h
	// Cross-origin writes are refused before the verb on every report route.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 1, 1)},
		{"POST", "/acme/widgets/api/moderation/reports/1/resolve", `{"action":"none"}`},
		{"POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", `{"action":"none"}`},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		if !h.Handle(rec, req) {
			rec.Code = http.StatusNotFound
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin %s = %d", c.path, rec.Code)
		}
	}
	// Invalid JSON on the repo resolve and an unknown verb.
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/1/resolve", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("repo resolve bad body = %d", rec.Code)
	}
	if rec := do(h, "PUT", "/acme/widgets/api/moderation/reports/1/resolve"); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve PUT = %d", rec.Code)
	}
	// The audit viewer surfaces a corrupt record as a 503, and a bad cursor
	// on the global queue is a 400.
	if _, err := store.PutBytes(ctx(), svc.Store, AuditKey(1), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	h2 := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "root", Admin: true}, nil
	}}
	if rec := do(h2, "GET", "/api/v1/moderation/audit"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt audit = %d", rec.Code)
	}
	if rec := do(h2, "GET", "/api/v1/moderation/reports?after=garbage"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad global cursor = %d", rec.Code)
	}
}

func TestReportQueuesFloorAndLimit(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	if _, _, err := s.ReportQueues(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader queues = %v", err)
	}
	s.Roles = &fakeRoles{role: identity.RoleAdmin}
	for i := 1; i <= 3; i++ {
		fileThreadReport(t, s, "bob", i, 1)
	}
	open, tail, err := s.ReportQueues(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, 1000)
	if err != nil || len(open) != 3 || len(tail) != 0 {
		t.Fatalf("queues cap = %d/%d %v", len(open), len(tail), err)
	}
}

func TestCompactTaskRunsOnRealTable(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	// One open row so the rebuild narrates a non-zero count.
	fileThreadReport(t, s, "bob", 1, 1)
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	reg := wal.NewRegistry(ctx(), store.NewMemory(), cfg)
	s.Tasks = reg.Tasks()
	s.startCompactTask(ctx())
	deadline := time.Now().Add(5 * time.Second)
	for {
		recent := reg.Tasks().List(reportsTaskRepo)
		if len(recent) == 1 && recent[0].Kind == TaskKindReportsCompact && recent[0].OK != nil {
			if !*recent[0].OK {
				t.Fatalf("compact task failed: %s", recent[0].Summary)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("compact task did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSmallCoverageBranches(t *testing.T) {
	// Unknown target kind answers no key (the probe-message helper).
	if got := reportTargetOf(Target{Kind: "bogus"}); got != "" {
		t.Fatalf("unknown target = %q", got)
	}
	s, _ := reportSvc(t, identity.RoleAdmin)
	// A no-op mutator returns the loaded window without a write.
	if _, err := s.mutateReportsIndexDoc(ctx(), "acme", "widgets", func(*ReportsIndex) (bool, error) {
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Resolved tails truncate to the limit (newest first).
	for i := 1; i <= 3; i++ {
		c := fileThreadReport(t, s, "bob", i, 1)
		if _, err := s.ResolveReport(ctx(), "acme", "widgets", c.ID, auth.Principal{Name: "alice"}, ResolveNone, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, tail, err := s.ReportQueues(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, 2)
	if err != nil || len(tail) != 2 || tail[0].ID != 3 {
		t.Fatalf("tail = %+v %v", tail, err)
	}
	// resolveGlobalCard walks past another repo's rows before finding its own.
	if err := s.appendGlobalCard(ctx(), GlobalCard{ReportCard: ReportCard{ID: 1, CreatedAt: "2026-11-20T10:00:00Z"}, Repo: "other/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := s.resolveGlobalCard(ctx(), "acme", "widgets", ReportCard{ID: 1}); err != nil {
		t.Fatal(err)
	}
	// The global mutation starts the compaction task when even the trimmed
	// open window is over the cap.
	tasks := &fakeTasks{started: make(chan struct{}, 1)}
	s.Tasks = tasks
	big := strings.Repeat("x", 1500)
	_, err = s.mutateGlobalIndex(ctx(), func(ix *GlobalReportsIndex) (bool, error) {
		for i := 0; i < 200; i++ {
			ix.Open = append(ix.Open, GlobalCard{ReportCard: ReportCard{
				ID: i + 10, CreatedAt: "2026-11-20T10:00:00Z", Note: big,
			}, Repo: "acme/widgets"})
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tasks.started:
	case <-time.After(2 * time.Second):
		t.Fatal("over-cap global write did not start the compact task")
	}
}
