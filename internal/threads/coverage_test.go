package threads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// coverage_test.go exercises the error and edge branches of the T1 surface:
// store failures, corrupt records, handler fallbacks, role ladders, and the
// git reader's failure classes.

// --- scripted stores -------------------------------------------------------

type errStore struct {
	*store.Memory
	getErr  error
	putErr  error
	listErr error
	phantom []string // valid keys reported by List but absent in the store
}

func (e *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if e.getErr != nil {
		return nil, e.getErr
	}
	return e.Memory.Get(ctx, key, opts)
}

func (e *errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if e.putErr != nil {
		return store.ObjectMeta{}, e.putErr
	}
	return e.Memory.Put(ctx, key, body, opts)
}

func (e *errStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if e.listErr != nil {
		return e.listErr
	}
	for _, k := range e.phantom {
		if strings.HasPrefix(k, prefix) {
			if err := fn(store.ObjectMeta{Key: k}); err != nil {
				return err
			}
		}
	}
	return e.Memory.List(ctx, prefix, after, fn)
}

// conflictStore fails every Put matching pred with a precondition error.
type conflictStore struct {
	*store.Memory
	pred func(key string) bool
}

func (c *conflictStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if c.pred(key) {
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return c.Memory.Put(ctx, key, body, opts)
}

// scriptedRoles returns a fixed CheckRead error and role.
type scriptedRoles struct{ readErr *auth.AuthError }

func (s scriptedRoles) Resolve(context.Context, string, string, auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.RoleRead, nil
}
func (s scriptedRoles) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return s.readErr
}

// --- errors and small helpers ---------------------------------------------

func TestStatusForMatrix(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: x", ErrNotFound), 404},
		{fmt.Errorf("%w: x", ErrInvalid), 400},
		{fmt.Errorf("%w: x", ErrUnauthorized), 401},
		{fmt.Errorf("%w: x", ErrForbidden), 403},
		{fmt.Errorf("%w: x", ErrConflict), 409},
		{fmt.Errorf("%w: x", ErrUnprocessable), 422},
		{fmt.Errorf("%w: x", ErrUnavailable), 503},
		{errors.New("boom"), 500},
	}
	for _, tc := range cases {
		if got := statusFor(tc.err); got != tc.want {
			t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestSlugSegmentEdges(t *testing.T) {
	cases := []struct{ in, want string }{
		{"...", "unnamed"},
		{"  Build  ", "build"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + "-b", strings.Repeat("a", 39)},
		{"a__b", "a__b"},
	}
	for _, tc := range cases {
		if got := slugSegment(tc.in); got != tc.want {
			t.Errorf("slugSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNonNilStepsNonEmpty(t *testing.T) {
	in := []Step{{Run: "x"}}
	if got := nonNilSteps(in); len(got) != 1 {
		t.Fatalf("nonNilSteps = %v", got)
	}
}

func TestNowUTCWithoutClock(t *testing.T) {
	if got := (&Service{}).nowUTC(); got.IsZero() {
		t.Fatal("a Service without Now must still return a time")
	}
}

func TestRoleRankTable(t *testing.T) {
	cases := map[string]int{"read": 1, "triage": 2, "write": 3, "maintain": 4, "admin": 5, "": 0, "bogus": 0}
	for role, want := range cases {
		if got := roleRank(role); got != want {
			t.Errorf("roleRank(%q) = %d, want %d", role, got, want)
		}
	}
}

func TestRoleAndGatesWithoutSeam(t *testing.T) {
	svc := &Service{Roles: nil}
	if got := svc.roleOf(ctx(), "o", "r", auth.Principal{Admin: true}); got != "admin" {
		t.Fatalf("admin role = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", auth.Principal{Write: true}); got != "write" {
		t.Fatalf("write role = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", auth.Anonymous()); got != "" {
		t.Fatalf("anonymous role = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", auth.Principal{Name: "x"}); got != "read" {
		t.Fatalf("signed-in role = %q", got)
	}
	if err := svc.requireRole(ctx(), "o", "r", auth.Principal{Admin: true}, "admin"); err != nil {
		t.Fatalf("admin requireRole: %v", err)
	}
	if err := svc.requireRead(ctx(), "o", "r", auth.Principal{Write: true}); err != nil {
		t.Fatalf("write requireRead: %v", err)
	}
	if err := svc.requireRead(ctx(), "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous requireRead = %v", err)
	}
	if err := svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "x"}); err != nil {
		t.Fatalf("signed-in requireRead: %v", err)
	}
	if err := svc.requireRole(ctx(), "o", "r", auth.Principal{Name: "x"}, "write"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("insufficient requireRole = %v", err)
	}
}

func TestRequireReadErrorKinds(t *testing.T) {
	cases := []struct {
		kind auth.AuthErrorKind
		want error
	}{
		{auth.ErrForbidden, ErrForbidden},
		{auth.ErrUnavailable, nil},
		{auth.ErrInvalid, ErrUnauthorized},
	}
	for _, tc := range cases {
		svc := &Service{Roles: scriptedRoles{readErr: &auth.AuthError{Kind: tc.kind, Why: "x"}}}
		err := svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "x"})
		if tc.want == nil {
			if err == nil || !strings.Contains(err.Error(), "identity unavailable") {
				t.Errorf("kind %v: err = %v", tc.kind, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("kind %v: err = %v, want %v", tc.kind, err, tc.want)
		}
	}
	// nil error passes.
	svc := &Service{Roles: scriptedRoles{}}
	if err := svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "x"}); err != nil {
		t.Fatalf("nil gate: %v", err)
	}
}

func TestStoreErrorsAndCastLoop(t *testing.T) {
	mem := store.NewMemory()
	base := New(mem, nil)
	boom := errors.New("store down")

	svc := &Service{Store: &errStore{Memory: mem, getErr: boom}}
	if _, _, err := svc.getJSON(ctx(), "k"); err == nil {
		t.Fatal("getJSON must surface a store error")
	}
	if _, err := svc.casUpdate(ctx(), "k", 2, func([]byte, store.Version) ([]byte, bool, error) {
		return nil, false, nil
	}); err == nil {
		t.Fatal("casUpdate must surface a read error")
	}
	// Put error (non-precondition) surfaces immediately.
	svc.Store = &errStore{Memory: mem, putErr: boom}
	if _, err := svc.casUpdate(ctx(), "k", 2, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return []byte("x"), true, nil
	}); !errors.Is(err, boom) {
		t.Fatalf("put error = %v", err)
	}
	// attempts <= 0 defaults to 5 and a no-write callback returns nil.
	if _, err := base.casUpdate(ctx(), "absent", 0, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur != nil {
			t.Fatal("absent key must read nil")
		}
		return nil, false, nil
	}); err != nil {
		t.Fatalf("no-write cas = %v", err)
	}
	// A callback error aborts.
	if _, err := base.casUpdate(ctx(), "absent", 2, func([]byte, store.Version) ([]byte, bool, error) {
		return nil, false, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("callback err = %v", err)
	}
}

// --- discovery / naming edges ---------------------------------------------

func TestWorkflowNameFallback(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/plain.yml", "on:\n  push:\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: y\n")
	f.add(testSHA, ".spindle/workflows/other.yaml", "on:\n  push:\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: y\n")
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "other" || files[1].Name != "plain" {
		t.Fatalf("names = %+v", files)
	}
}

func TestMatchesWorkflowEmpty(t *testing.T) {
	f := &WorkflowFile{Path: ".spindle/workflows/ci.yml"}
	if !matchesWorkflow(f, "") || !matchesWorkflow(f, "ci.yml") || matchesWorkflow(f, "other.yml") {
		t.Fatal("matchesWorkflow matrix")
	}
}

func TestWorkflowPathEdges(t *testing.T) {
	cases := map[string]bool{
		".spindle/workflows/ci.yml":     true,
		".spindle/workflows/ci.yaml":    true,
		".spindle/workflows/README.md":  false,
		".spindle/workflows/sub/ci.yml": false,
		".spindle/workflows/":           false,
		".github/workflows/ci.yml":      false,
		"spindle/workflows/ci.yml":      false,
	}
	for p, want := range cases {
		if got := workflowPath(p); got != want {
			t.Errorf("workflowPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// --- git reader edges -----------------------------------------------------

func TestReadFileInvalidPaths(t *testing.T) {
	dir, head := gitFixture(t)
	g := newGitReader(t, dir)
	for _, p := range []string{"", "-x", "a:b"} {
		if _, err := g.ReadFile(ctx(), "o/r", head, p); !errors.Is(err, ErrUnprocessable) {
			t.Errorf("ReadFile(%q) = %v, want ErrUnprocessable", p, err)
		}
	}
}

func TestGitMissingBinaryIsUnavailable(t *testing.T) {
	dir, head := gitFixture(t)
	g := NewSubprocessGit("spindle-no-such-git-binary")
	g.SetDirs(staticDirs{"o/r": dir + "/.git"})
	if _, err := g.ListWorkflowFiles(ctx(), "o/r", head); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing binary err = %v", err)
	}
	if g.Binary != "spindle-no-such-git-binary" || g.Timeout <= 0 {
		t.Fatalf("reader = %+v", g)
	}
	// An empty binary defaults to "git".
	if NewSubprocessGit("").Binary != "git" {
		t.Fatal("empty binary must default to git")
	}
}

func TestCapBufferEdges(t *testing.T) {
	unlimited := &capBuffer{}
	if _, err := unlimited.Write([]byte("abcdef")); err != nil || unlimited.String() != "abcdef" {
		t.Fatalf("unlimited = %q %v", unlimited.String(), err)
	}
	full := &capBuffer{limit: 2}
	_, _ = full.Write([]byte("ab"))
	_, _ = full.Write([]byte("cd")) // keep == 0 branch
	if full.String() != "ab" || !full.truncated {
		t.Fatalf("full = %q %v", full.String(), full.truncated)
	}
}

// --- handler fallbacks ----------------------------------------------------

func TestHandlerWithoutAuthIsAnonymous(t *testing.T) {
	e := newTestEnv(t)
	e.roles.public = true
	h := &Handler{Svc: e.svc}
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 200 {
		t.Fatalf("anonymous handler = %d", rec.Code)
	}
	e.roles.public = false
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 401 {
		t.Fatalf("anonymous private = %d", rec.Code)
	}
}

func TestHandlePathEdges(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	cases := []struct {
		path  string
		claim bool
	}{
		{"/o/r/api/threads", true},
		{"/o/r/api-browser/threads", true},
		{"/o/r/issues", false},
		{"/api/v1/repos", false},
		{"/o/r", false},
		{"/%2E/r/api/threads", false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		if got := h.Handle(rec, httptest.NewRequest(http.MethodGet, tc.path, nil)); got != tc.claim {
			t.Errorf("Handle(%q) = %v, want %v", tc.path, got, tc.claim)
		}
	}
	// A non-threads repo lane is not claimed by handleRepo.
	if h.handleRepo(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/o/r/issues", nil), "o/r", []string{"issues"}) {
		t.Fatal("handleRepo claimed a non-threads lane")
	}
}

func TestDecodeSegmentInvalid(t *testing.T) {
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("decodeSegment = %q", got)
	}
	if got := decodeSegment("a%2Fb"); got != "a/b" {
		t.Fatalf("decodeSegment = %q", got)
	}
}

func TestWriteErrKinds(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, 403},
		{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, 503},
		{&auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}, 401},
		{fmt.Errorf("%w: nope", ErrNotFound), 404},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("writeErr(%v) = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestWriteJSONMarshalError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, make(chan int))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("marshal error status = %d", rec.Code)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDecodeStrictEdges(t *testing.T) {
	cases := []struct {
		name string
		body io.Reader
	}{
		{"null", strings.NewReader("null")},
		{"array", strings.NewReader("[1]")},
		{"unreadable", failingReader{}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/o/r/api/threads", tc.body)
		if decodeStrict(rec, req, 1<<20, map[string]bool{}, &map[string]any{}) {
			t.Errorf("%s: decodeStrict accepted", tc.name)
		}
	}
}

func TestHandlerErrors(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	// Auth error on list.
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 503 {
		t.Fatalf("auth error = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/x", ""); rec.Code != 503 {
		t.Fatalf("get auth error = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads", `{}`); rec.Code != 503 {
		t.Fatalf("dispatch auth error = %d", rec.Code)
	}
	// Non-numeric n.
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
	if rec := do(h, http.MethodGet, "/o/r/api/threads?n=x", ""); rec.Code != 400 {
		t.Fatalf("n=x = %d", rec.Code)
	}
	// Corrupt run record surfaces a 500-class error.
	if _, err := store.PutBytes(ctx(), e.store, RunKey("o", "r", "0000000000000000001-abc123"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/0000000000000000001-abc123", ""); rec.Code != 500 {
		t.Fatalf("corrupt run = %d", rec.Code)
	}
}

// --- service error/edge branches ------------------------------------------

func TestCreateOneConflicts(t *testing.T) {
	e := newTestEnv(t)
	// Job write collides.
	svc := *e.svc
	svc.Store = &conflictStore{Memory: e.store, pred: func(key string) bool {
		return strings.Contains(key, "/jobs/")
	}}
	if _, err := svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("job conflict err = %v", err)
	}
	// Run header collides.
	e2 := newTestEnv(t)
	svc2 := *e2.svc
	svc2.Store = &conflictStore{Memory: e2.store, pred: func(key string) bool {
		return strings.Contains(key, "/threads/runs/") && !strings.Contains(key, "/jobs/")
	}}
	if _, err := svc2.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("run conflict err = %v", err)
	}
}

func TestCheckMirrorNilSink(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Checks = nil
	run := e.mustCreate(t)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil || len(view.Jobs) != 2 {
		t.Fatalf("nil sink run = %v %v", view, err)
	}
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, view.Jobs[0].ID, JobUpdate{State: JobSuccess}); err != nil {
		t.Fatalf("nil sink update: %v", err)
	}
}

func TestDispatchNilGit(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Git = nil
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", e.writer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil git dispatch = %v", err)
	}
}

func TestCorruptRecords(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	// Corrupt run header.
	raw := []byte("{not json")
	if _, err := store.PutBytes(ctx(), e.store, RunKey("o", "r", run.ID), raw,
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt run err = %v", err)
	}
	// Corrupt job record.
	e2 := newTestEnv(t)
	run2 := e2.mustCreate(t)[0]
	if _, err := store.PutBytes(ctx(), e2.store, JobKey("o", "r", run2.ID, "j-build"), raw,
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.svc.GetRun(ctx(), "o/r", run2.ID, e2.public()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt job err = %v", err)
	}
	if _, err := e2.svc.UpdateJob(ctx(), "o/r", run2.ID, "j-build", JobUpdate{State: JobSuccess}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt update err = %v", err)
	}
	// Corrupt run header blocks UpdateJob's run load.
	e3 := newTestEnv(t)
	run3 := e3.mustCreate(t)[0]
	if _, err := store.PutBytes(ctx(), e3.store, RunKey("o", "r", run3.ID), raw,
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e3.svc.UpdateJob(ctx(), "o/r", run3.ID, "j-build", JobUpdate{State: JobSuccess}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt run update err = %v", err)
	}
}

func TestListRunsSkipsVanishedAndListErrors(t *testing.T) {
	e := newTestEnv(t)
	e.mustCreate(t)
	phantomID := "0000000000000000099-abc123"
	svc := *e.svc
	svc.Store = &errStore{Memory: e.store, phantom: []string{RunsPrefix("o", "r") + phantomID + ".json"}}
	page, err := svc.ListRuns(ctx(), "o/r", e.public(), "", 10)
	if err != nil {
		t.Fatalf("list with phantom: %v", err)
	}
	for _, r := range page.Runs {
		if r.ID == phantomID {
			t.Fatal("a vanished run leaked into the page")
		}
	}
	// List failure surfaces.
	svc.Store = &errStore{Memory: e.store, listErr: errors.New("down")}
	if _, err := svc.ListRuns(ctx(), "o/r", e.public(), "", 10); err == nil {
		t.Fatal("list error swallowed")
	}
	ids, _ := e.svc.runIDs(ctx(), "o", "r")
	if len(ids) == 0 {
		t.Fatal("fixture lost its run")
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", ids[0], e.public()); err != nil {
		t.Fatalf("get after list error: %v", err)
	}
}

func TestRunIDsFiltersForeignKeys(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	for _, k := range []string{
		RunsPrefix("o", "r") + "not-a-run.json",
		RunsPrefix("o", "r") + "notes.txt",
		RunsPrefix("o", "r") + "0000000000000000001-abc123/extra.json",
	} {
		if _, err := store.PutBytes(ctx(), e.store, k, []byte("{}"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := e.svc.runIDs(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id != run.ID {
			t.Fatalf("foreign key leaked: %v", ids)
		}
	}
	svc := *e.svc
	svc.Store = &errStore{Memory: e.store, listErr: errors.New("down")}
	if _, err := svc.runIDs(ctx(), "o", "r"); err == nil {
		t.Fatal("runIDs swallowed a list error")
	}
}

func TestFoldRunMissingRun(t *testing.T) {
	e := newTestEnv(t)
	if err := e.svc.foldRun(ctx(), "o", "r", "0000000000000000001-abc123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fold missing run = %v", err)
	}
}

func TestUpdateJobExplicitTimes(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	started := "2026-11-20T12:00:05Z"
	completed := "2026-11-20T12:00:09Z"
	job, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, view.Jobs[0].ID, JobUpdate{
		State: JobSuccess, StartedAt: &started, CompletedAt: &completed,
	})
	if err != nil {
		t.Fatalf("explicit times: %v", err)
	}
	if job.StartedAt == nil || *job.StartedAt != started || job.CompletedAt == nil || *job.CompletedAt != completed {
		t.Fatalf("times = %+v", job)
	}
}

func TestEarliestStartPick(t *testing.T) {
	a, b := "2026-11-20T12:00:05Z", "2026-11-20T12:00:01Z"
	got := earliestStart([]*Job{{StartedAt: &a}, {StartedAt: nil}, {StartedAt: &b}})
	if got == nil || *got != b {
		t.Fatalf("earliestStart = %v", got)
	}
	if earliestStart(nil) != nil {
		t.Fatal("no jobs must have no start")
	}
}

func TestReadFileTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("# pad\n", maxWorkflowBytes)
	runGit := func(argv ...string) string {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, ".spindle", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".spindle", "workflows", "big.yml"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "big")
	head := runGit("rev-parse", "HEAD")
	g := newGitReader(t, dir)
	if _, err := g.ReadFile(ctx(), "o/r", head, ".spindle/workflows/big.yml"); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("truncated read = %v", err)
	}
	if _, err := g.ReadFile(ctx(), "o/r", "nope", ".spindle/workflows/big.yml"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad commit read = %v", err)
	}
}

func TestCapBufferNormalWrite(t *testing.T) {
	b := &capBuffer{limit: 10}
	_, _ = b.Write([]byte("abc"))
	if b.String() != "abc" || b.truncated {
		t.Fatalf("normal = %q %v", b.String(), b.truncated)
	}
}

func TestDecodeStrictTypeError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/o/r/api/threads", strings.NewReader(`{"ref":5}`))
	var body struct {
		Ref string `json:"ref"`
	}
	if decodeStrict(rec, req, 1<<20, map[string]bool{"ref": true}, &body) {
		t.Fatal("type error accepted")
	}
	if rec.Code != 400 {
		t.Fatalf("type error status = %d", rec.Code)
	}
}

func TestHandlerListServiceError(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "reader"}, nil
	}
	svc := *e.svc
	svc.Store = &errStore{Memory: e.store, listErr: errors.New("down")}
	h.Svc = &svc
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 500 {
		t.Fatalf("list service error = %d", rec.Code)
	}
}

func TestServiceBadRepoPaths(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.GetRun(ctx(), "bad", "0000000000000000001-abc123", e.public()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("GetRun bad repo = %v", err)
	}
	if _, err := e.svc.ListRuns(ctx(), "bad", e.public(), "", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ListRuns bad repo = %v", err)
	}
	if _, err := e.svc.DispatchRun(ctx(), "bad", "refs/heads/main", "", e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DispatchRun bad repo = %v", err)
	}
	if err := e.svc.foldRun(ctx(), "o", "r", "0000000000000000001-abc123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foldRun missing run = %v", err)
	}
	if p, err := e.svc.ListRuns(ctx(), "o/r", e.public(), "", 0); err != nil || len(p.Runs) != 0 {
		t.Fatalf("n=0 page = %+v %v", p, err)
	}
}

func TestLoadJobsErrors(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	// Phantom job key is skipped.
	svc := *e.svc
	svc.Store = &errStore{Memory: e.store, phantom: []string{JobKey("o", "r", run.ID, "j-ghost")}}
	view, err := svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatalf("phantom job: %v", err)
	}
	if len(view.Jobs) != 2 {
		t.Fatalf("phantom job leaked: %d jobs", len(view.Jobs))
	}
	// Job list failure surfaces.
	svc.Store = &errStore{Memory: e.store, listErr: errors.New("down")}
	if _, err := svc.GetRun(ctx(), "o/r", run.ID, e.public()); err == nil {
		t.Fatal("job list error swallowed")
	}
	// Job read failure surfaces.
	svc.Store = &errStore{Memory: e.store, getErr: errors.New("down")}
	if _, err := svc.GetRun(ctx(), "o/r", run.ID, e.public()); err == nil {
		t.Fatal("job read error swallowed")
	}
	// Run read failure surfaces.
	if _, err := svc.GetRun(ctx(), "o/r", run.ID, e.public()); err == nil {
		t.Fatal("run read error swallowed")
	}
}

func TestUnnamedJobAndContainer(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on:
  push:
jobs:
  build:
    runs-on: linux
    container: golang:1.27
    steps:
      - run: go build ./...
`)
	run := e.mustCreate(t)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(view.Jobs))
	}
	if view.Jobs[0].Name != "build" || view.Jobs[0].Container != "golang:1.27" {
		t.Fatalf("job = %+v", view.Jobs[0])
	}
}
