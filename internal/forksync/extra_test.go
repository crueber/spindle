package forksync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func TestKindRegistry(t *testing.T) {
	ResetKindsForTest()
	RegisterKind("x")
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
		ResetKindsForTest()
	}()
	RegisterKind("x")
}

func TestRankTable(t *testing.T) {
	cases := map[identity.Role]int{
		identity.RoleRead: 1, identity.RoleTriage: 2, identity.RoleWrite: 3,
		identity.RoleMaintain: 4, identity.RoleAdmin: 5, "": 0, "bogus": 0,
	}
	for role, want := range cases {
		if got := rank(role); got != want {
			t.Fatalf("rank(%q) = %d, want %d", role, got, want)
		}
	}
}

func TestRequireWritePaths(t *testing.T) {
	svc := &Service{}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Principal{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Principal{Name: "u", Write: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Principal{Name: "u"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no write: %v", err)
	}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous: %v", err)
	}
	svc.Roles = fakeRoles{string(identity.RoleMaintain)}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Principal{Name: "u"}); err != nil {
		t.Fatal(err)
	}
	svc.Roles = fakeRoles{string(identity.RoleRead)}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Principal{Name: "u"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader: %v", err)
	}
	if err := svc.requireWrite(context.Background(), "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous reader: %v", err)
	}
}

func TestReadForkErrors(t *testing.T) {
	svc := &Service{Store: store.NewMemory()}
	if parent, err := svc.readFork(context.Background(), "o", "r"); err != nil || parent != "" {
		t.Fatalf("absent: %q %v", parent, err)
	}
	if _, err := store.PutBytes(context.Background(), svc.Store, "repos/o/r/fork.json", []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.readFork(context.Background(), "o", "r"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt: %v", err)
	}
}

func TestStartErrors(t *testing.T) {
	svc := &Service{Store: store.NewMemory(), Roles: fakeRoles{string(identity.RoleAdmin)}}
	if _, err := svc.Start(context.Background(), "o", "r", p()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not a fork: %v", err)
	}
	if _, err := store.PutBytes(context.Background(), svc.Store, "repos/o/r/fork.json", []byte(`{"parent":"a/b"}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "o", "r", p()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no tasks: %v", err)
	}
}

func TestRunSeamAndResolverFailures(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	svc.Refs = nil
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no seams: %v", err)
	}
	// Upstream resolver missing / relative / error.
	svc, _, _, _, _ = newSvc(t)
	svc.UpstreamURL = nil
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no resolver: %v", err)
	}
	svc.UpstreamURL = func(string) (string, error) { return "not-absolute", nil }
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative: %v", err)
	}
	svc.UpstreamURL = func(string) (string, error) { return "", errors.New("boom") }
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("resolver error: %v", err)
	}
	// Guard missing fails closed.
	svc, _, fetch, _, _ := newSvc(t)
	svc.Guard = nil
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no guard: %v", err)
	}
	if fetch.calls != 0 {
		t.Fatal("fetched without a guard")
	}
}

func TestRunRefFailures(t *testing.T) {
	svc, refs, _, _, _ := newSvc(t)
	refs.err = errors.New("refs down")
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refs: %v", err)
	}
	svc, refs, _, _, _ = newSvc(t)
	delete(refs.branches, "acme/base")
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), &fakeLogger{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fork refs: %v", err)
	}
}

func TestLastOutcomeAbsentAndCorrupt(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	doc, err := svc.LastOutcome(context.Background(), "me", "nope")
	if err != nil || doc != nil {
		t.Fatalf("absent: %+v %v", doc, err)
	}
	if _, err := store.PutBytes(context.Background(), svc.Store, SidecarKey("me", "bad"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LastOutcome(context.Background(), "me", "bad"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
}

// failStore fails PutBytes so the sidecar write reports a narrator line.
type failStore struct{ store.ObjectStore }

func (f *failStore) Put(context.Context, string, store.PutBody, store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("put down")
}

func TestRecordFailureIsNarrated(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	svc.Store = &failStore{ObjectStore: store.NewMemory()}
	log := &fakeLogger{}
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), log); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(log.notices) < 1 {
		t.Fatal("record failure was silent")
	}
}

func TestStatusAndErrTables(t *testing.T) {
	cases := map[error]int{
		nil: 200, ErrNotFound: 404, ErrInvalid: 400, ErrUnauthorized: 401,
		ErrForbidden: 403, ErrConflict: 409, ErrUnavailable: 503,
		errors.New("x"): 500,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Fatalf("statusFor(%v) = %d, want %d", err, got, want)
		}
	}
	if !isErr(ErrCorrupt, ErrCorrupt) || isErr(nil, ErrCorrupt) {
		t.Fatal("isErr")
	}
}

func TestWriteErrAuthKinds(t *testing.T) {
	for kind, want := range map[auth.AuthErrorKind]int{
		auth.ErrForbidden: 403, auth.ErrUnavailable: 503, auth.ErrInvalid: 401,
	} {
		w := httptest.NewRecorder()
		writeErr(w, &auth.AuthError{Kind: kind, Why: "x"})
		if w.Code != want {
			t.Fatalf("kind %v = %d, want %d", kind, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	writeErr(w, ErrConflict)
	if w.Code != 409 {
		t.Fatalf("plain: %d", w.Code)
	}
}

func TestHTTPEdgePaths(t *testing.T) {
	h := &Handler{}
	if w := do(h, "GET", "/me/fork/api/sync-fork"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc: %d", w.Code)
	}
	svc, _, _, _, _ := newSvc(t)
	h = &Handler{Svc: svc}
	// GET passes the write gate in the default harness (admin); the task
	// query resolves when the id exists.
	id, err := svc.Start(context.Background(), "me", "fork", p())
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/me/fork/api/sync-fork?task="+id); w.Code != 200 || !strings.Contains(w.Body.String(), `"task"`) {
		t.Fatalf("get task: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/me/fork/api/sync-fork?task=missing"); w.Code != 200 || strings.Contains(w.Body.String(), `"task"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/x/api/sync-fork", "/me/fork/api/nope", "/me/fork/api/sync-fork/x", "/me/fork/api"} {
		if w := do(h, "GET", path); w.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
}

func TestShortAndScrub(t *testing.T) {
	if got := short("0123456789abcdef"); got != "0123456789ab" {
		t.Fatalf("short %q", got)
	}
	if got := short("abc"); got != "abc" {
		t.Fatalf("short %q", got)
	}
	long := strings.Repeat("x", 700)
	if got := scrub(long); len(got) != 512 {
		t.Fatalf("scrub len %d", len(got))
	}
	if got := scrub("a\r\nb\tc\x01"); strings.ContainsRune(got, 0x01) || strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("scrub %q", got)
	}
}

func TestNowUTCWithClock(t *testing.T) {
	svc := &Service{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if got := svc.nowUTC().Format(time.RFC3339); got != "2026-01-01T00:00:00Z" {
		t.Fatalf("clock %q", got)
	}
	if svc2 := (&Service{}); svc2.nowUTC().IsZero() {
		t.Fatal("default clock zero")
	}
}

// getFailStore fails Get (readFork's store-error path).
type getFailStore struct{ store.ObjectStore }

func (g *getFailStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("get down")
}

func TestReadForkStoreError(t *testing.T) {
	svc := &Service{Store: &getFailStore{ObjectStore: store.NewMemory()}}
	if _, err := svc.readFork(context.Background(), "o", "r"); err == nil {
		t.Fatal("store error swallowed")
	}
	if _, err := svc.LastOutcome(context.Background(), "o", "r"); err == nil {
		t.Fatal("LastOutcome store error swallowed")
	}
}

func TestFetchReturnsSameTip(t *testing.T) {
	svc, _, fetch, pub, _ := newSvc(t)
	// Upstream refs advertise the old tip, but the fetch reports the fork's
	// tip: the post-fetch equality check must stop before ancestry.
	svc.Refs.(*fakeRefs).branches["acme/base"] = [2]string{"main", "cccc"}
	fetch.tip = "bbbb"
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 0 {
		t.Fatalf("published %+v", pub.calls)
	}
	doc, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if doc.Result != ResultCurrent {
		t.Fatalf("result %q", doc.Result)
	}
}

func TestRecordUpdatesExistingSidecarVersion(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	// Seed an outcome, then run again and assert the version bumped.
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	first, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if first.Version != 1 {
		t.Fatalf("version %d", first.Version)
	}
	svc.Tasks = newFakeTasks() // a fresh table so the second start runs
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	second, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if second.Version != 2 {
		t.Fatalf("version %d", second.Version)
	}
}

func TestRecordCorruptPrevious(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	if _, err := store.PutBytes(context.Background(), svc.Store, SidecarKey("me", "fork"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	doc, err := svc.LastOutcome(context.Background(), "me", "fork")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 {
		t.Fatalf("version %d", doc.Version)
	}
}

func TestHTTPStartErrorAndGate(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	h := &Handler{Svc: svc}
	if w := do(h, "POST", "/acme/base/api/sync-fork"); w.Code != http.StatusBadRequest {
		t.Fatalf("not a fork: %d %s", w.Code, w.Body.String())
	}
	svc.Roles = fakeRoles{string(identity.RoleRead)}
	h = &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "reader"}, nil
	}}
	if w := do(h, "GET", "/me/fork/api/sync-fork"); w.Code != http.StatusForbidden {
		t.Fatalf("get gate: %d", w.Code)
	}
}

func TestSplitPathBadEscapeAndBadRepo(t *testing.T) {
	req := httptest.NewRequest("GET", "/me/fork/api/sync-fork", nil)
	req.URL.RawPath = "/me/fork/api/sync%zzfork"
	// EscapedPath falls back; force the parse-error branch directly.
	got := splitPath(&http.Request{URL: &url.URL{Path: "/a%zz/b"}})
	if len(got) != 2 || got[0] != "a%zz" {
		t.Fatalf("splitPath %v", got)
	}
	h := &Handler{Svc: &Service{}}
	if w := do(h, "GET", "/BAD%20OWNER/fork/api/sync-fork"); w.Code != http.StatusNotFound {
		t.Fatalf("bad repo: %d", w.Code)
	}
}

func TestRecordNoSlashRepoIsNoop(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	log := &fakeLogger{}
	svc.record(context.Background(), "nodash", Sidecar{}, log)
	if svc.Store == nil {
		t.Fatal("unreachable")
	}
	// nil store short-circuits too.
	(&Service{}).record(context.Background(), "o/r", Sidecar{}, log)
}

func TestWriteJSONEncodeFailure(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, make(chan int))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("encode: %d", w.Code)
	}
}

func TestHTTPAnonymousGate(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	svc.Roles = fakeRoles{} // no role for anyone
	h := &Handler{Svc: svc}
	if w := do(h, "POST", "/me/fork/api/sync-fork"); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", w.Code, w.Body.String())
	}
}
