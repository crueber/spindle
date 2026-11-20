package webui

// coverage_core_test.go — coverage for the foundation helpers (time/initials/
// render fallbacks), the provider dispatch, /explore, and the small pure
// helpers in diff/graph. These are the branches the group suites do not drive
// because they belong to the shared layer rather than one page family.

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- template helper funcs ---------------------------------------------------

func TestCoverageHelperFuncs(t *testing.T) {
	hasPrefix := (&Server{}).funcs()["hasPrefix"].(func(string, string) bool)
	if !hasPrefix("refs/heads/main", "refs/heads/") {
		t.Fatal("hasPrefix must accept a real prefix")
	}
	if hasPrefix("main", "refs/") {
		t.Fatal("hasPrefix must reject a non-prefix")
	}

	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"not-a-time", ""},
		{stamp(-30 * time.Second), "just now"},
		{stamp(-5 * time.Minute), "5m ago"},
		{stamp(-3 * time.Hour), "3h ago"},
		{stamp(-3 * 24 * time.Hour), "3d ago"},
		{stamp(-60 * 24 * time.Hour), "2mo ago"},
		{stamp(-800 * 24 * time.Hour), "2y ago"},
	} {
		if got := relTime(tc.in); got != tc.want {
			t.Fatalf("relTime(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
	if got := itoa(0); got != "0" {
		t.Fatalf("itoa(0) = %q", got)
	}
	if got := initials(""); got != "?" {
		t.Fatalf("initials(empty) = %q", got)
	}
	if got := initials("a"); got != "A" {
		t.Fatalf("initials(single) = %q", got)
	}
	if got := initials("alice"); got != "A" {
		t.Fatalf("initials(word) = %q", got)
	}
}

// --- render fallbacks --------------------------------------------------------

// coverageRenderer is a Server with no error template: errorPage must fall
// back to the minimal document (never recurse into a missing template).
func coverageRenderer() *Server {
	return &Server{tpl: map[string]*template.Template{}}
}

func TestCoverageErrorPageFailsClosedWithoutTemplate(t *testing.T) {
	s := coverageRenderer()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	s.errorPage(rec, req, http.StatusBadGateway, "template tree missing")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "template tree missing") {
		t.Fatalf("body = %q", body)
	}

	// The text/plain twin skips the template entirely.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept", "text/plain")
	s.errorPage(rec, req, http.StatusForbidden, "nope")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("plain content-type = %q", ct)
	}

	// HEAD answers the status without a body.
	rec = httptest.NewRecorder()
	s.errorPage(rec, httptest.NewRequest("HEAD", "/x", nil), http.StatusNotFound, "gone")
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestCoverageFailPlainTextTwin(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept", "text/plain")
	fail(rec, req, http.StatusForbidden, "no entry")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "no entry") {
		t.Fatalf("body = %q", body)
	}
}

func TestCoverageRenderExecutionErrorFailsClosed(t *testing.T) {
	// {{.Data.Field}} on a nil Data errors at execution time (the "broken
	// template at request time" case render must not propagate).
	tpl := template.Must(template.New("base").Parse(`{{define "base"}}{{.Data.Field}}{{end}}`))
	s := &Server{tpl: map[string]*template.Template{"broken": tpl}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	s.render(rec, req, http.StatusOK, "broken", PageData{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	// The template error text stays in the log; the body carries the generic
	// sentence only (backend detail is not rendered).
	if body := rec.Body.String(); !strings.Contains(body, "internal error") || strings.Contains(body, "nil pointer") {
		t.Fatalf("body = %q", body)
	}
}

// --- provider dispatch -------------------------------------------------------

// coverageFlusher records Flush calls without implementing anything else.
type coverageFlusher struct {
	header http.Header
	calls  int
}

func (f *coverageFlusher) Header() http.Header         { return f.header }
func (f *coverageFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (f *coverageFlusher) WriteHeader(int)             {}
func (f *coverageFlusher) Flush()                      { f.calls++ }

// coverageWriter is a ResponseWriter with no Flusher: probeWriter must not
// invent one.
type coverageWriter struct{ header http.Header }

func (w *coverageWriter) Header() http.Header         { return w.header }
func (w *coverageWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *coverageWriter) WriteHeader(int)             {}

func TestCoverageServeDispatch(t *testing.T) {
	var nilServer *Server
	if nilServer.Serve(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("a nil provider must not claim a request")
	}
	unbound := &Server{}
	if unbound.Serve(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("a provider without auth must not claim a request")
	}

	// probeWriter forwards Flush only when the underlying writer has it.
	fl := &coverageFlusher{header: http.Header{}}
	pw := &probeWriter{ResponseWriter: fl}
	pw.Flush()
	if fl.calls != 1 {
		t.Fatalf("flush calls = %d", fl.calls)
	}
	pw = &probeWriter{ResponseWriter: &coverageWriter{header: http.Header{}}}
	pw.Flush() // must not panic
}

func TestCoverageAuthErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
		{auth.ErrUnauthorized, http.StatusUnauthorized},
	} {
		if got := authErrorStatus(&auth.AuthError{Kind: tc.kind}); got != tc.want {
			t.Fatalf("authErrorStatus(%v) = %d want %d", tc.kind, got, tc.want)
		}
	}
}

func TestCoveragePrincipalFailure(t *testing.T) {
	s := coverageRenderer()
	s.auth = pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "denied by policy"}
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/explore", nil)
	req.Header.Set("Accept", "text/plain")
	if _, ok := s.principal(rec, req); ok {
		t.Fatal("principal must report the failure")
	}
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "denied by policy") {
		t.Fatalf("principal failure = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if _, ok := s.gated(rec, httptest.NewRequest("GET", "/explore", nil)); ok {
		t.Fatal("gated must propagate a principal failure")
	}
}

// --- /explore ----------------------------------------------------------------

// coverageRegistry is a RepoRegistry double whose behaviour each test owns.
type coverageRegistry struct {
	owners    []string
	repos     map[string][]string
	counts    map[string]int
	ownersErr error
	countsErr error
}

func (r *coverageRegistry) Owners(context.Context) ([]string, error) { return r.owners, r.ownersErr }
func (r *coverageRegistry) Repos(_ context.Context, owner string) ([]string, error) {
	return r.repos[owner], nil
}
func (r *coverageRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) {
	return r.counts, r.countsErr
}
func (r *coverageRegistry) Exists(_ context.Context, id git.RepoId) (bool, error) {
	for _, n := range r.repos[id.Owner] {
		if n == id.Name {
			return true, nil
		}
	}
	return false, nil
}
func (r *coverageRegistry) Create(context.Context, git.RepoId, git.ObjectFormat) error { return nil }
func (r *coverageRegistry) Delete(context.Context, git.RepoId) error                   { return nil }

func newCoverageExploreServer(t *testing.T, reg api.RepoRegistry) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.AnonymousRead = true
	s, err := New(Env{Cfg: cfg, API: &api.Env{Repos: reg}})
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	s.BindAuth(pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}})
	return s
}

func TestCoverageExplorePage(t *testing.T) {
	reg := &coverageRegistry{
		owners: []string{"zeta", "acme"},
		repos:  map[string][]string{"acme": {"widgets"}, "zeta": {"tools"}},
		counts: map[string]int{"acme": 4, "zeta": 2},
	}
	s := newCoverageExploreServer(t, reg)

	rec := httptest.NewRecorder()
	s.explore(rec, httptest.NewRequest("GET", "/explore", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explore = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "acme") || !strings.Contains(body, "zeta") {
		t.Fatalf("explore body missing owners: %q", body)
	}

	// ?format=text is the plain-text twin, NAMES sorted by the registry order.
	rec = httptest.NewRecorder()
	s.explore(rec, httptest.NewRequest("GET", "/explore?format=text", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explore text = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "owners\nzeta\nacme\n" {
		t.Fatalf("explore text = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("text content-type = %q", ct)
	}

	// Count errors degrade to zero counts, never a failed page.
	reg.countsErr = errors.New("counts down")
	rec = httptest.NewRecorder()
	s.explore(rec, httptest.NewRequest("GET", "/explore", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explore with count outage = %d", rec.Code)
	}

	// An owner-list outage is a 503 page (the registry is the page's data).
	reg.ownersErr = errors.New("owners down")
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/explore", nil)
	req.Header.Set("Accept", "text/plain")
	s.explore(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("explore outage = %d", rec.Code)
	}

	// A gated page refuses an anonymous caller when anonymous_read is off.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/explore", nil)
	req.Header.Set("Accept", "text/plain")
	s.auth.Authenticate = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), nil
	}
	s.env.Cfg.Server.Auth.AnonymousRead = false
	s.explore(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated explore = %d", rec.Code)
	}
	s.env.Cfg.Server.Auth.AnonymousRead = true

	// No registry at all: an empty-but-valid page (zero-config first run).
	rec = httptest.NewRecorder()
	s.env.API = nil
	s.explore(rec, httptest.NewRequest("GET", "/explore", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explore without API = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.explore(rec, httptest.NewRequest("GET", "/explore?format=text", nil))
	if got := rec.Body.String(); got != "owners\n" {
		t.Fatalf("explore text without API = %q", got)
	}
}

// --- diff / graph small helpers ---------------------------------------------

func TestCoverageAtoiOr(t *testing.T) {
	if got := atoiOr("", 7); got != 7 {
		t.Fatalf("atoiOr(empty) = %d", got)
	}
	if got := atoiOr("nope", 7); got != 7 {
		t.Fatalf("atoiOr(bad) = %d", got)
	}
	if got := atoiOr("12", 7); got != 12 {
		t.Fatalf("atoiOr(12) = %d", got)
	}
}

func TestCoverageGraphLaneReuse(t *testing.T) {
	// A window whose next tip is not a pending parent reuses the freed lane
	// instead of widening the graph.
	rows, width := assignLanes([]GraphCommit{gc("a", "b", "c"), gc("b"), gc("c"), gc("d")})
	if width != 2 || len(rows) != 4 {
		t.Fatalf("reuse window: width=%d rows=%d", width, len(rows))
	}
	if rows[3].lane != 0 {
		t.Fatalf("freed lane was not reused: %+v", rows[3])
	}

	// A merge whose second parent is already tracked stays on its lane.
	rows, _ = assignLanes([]GraphCommit{gc("m", "a", "b"), gc("n", "a", "b")})
	if len(rows) != 2 {
		t.Fatalf("dedupe window rows = %d", len(rows))
	}

	// A merge whose second parent is new takes a freed middle lane.
	// A hole left by a released first parent is reused by a later tip.
	rows, _ = assignLanes([]GraphCommit{gc("m", "a", "b"), gc("a", "b"), gc("z")})
	if len(rows) != 3 || rows[2].lane != 0 {
		t.Fatalf("mid-hole reuse = %+v", rows)
	}

	rows, _ = assignLanes([]GraphCommit{gc("m", "a", "b"), gc("a", "b"), gc("b", "x", "y")})
	if len(rows) != 3 {
		t.Fatalf("hole window rows = %d", len(rows))
	}
	shas := laneShas(rows[2].bottom)
	if len(shas) != 2 || shas[0] != "y" || shas[1] != "x" {
		t.Fatalf("second parent did not fill the freed lane: %v", shas)
	}

	if got := freeLane(nil); got != -1 {
		t.Fatalf("freeLane(nil) = %d", got)
	}
	if got := freeLane([]laneRef{{ok: true}, {}}); got != 1 {
		t.Fatalf("freeLane(hole) = %d", got)
	}
}

func TestCoverageRenderRailSkipsHoles(t *testing.T) {
	// m(a,b) then a(b) leaves a freed middle lane in row a's bottom snapshot;
	// the rail must skip it rather than draw a lane at index 0.
	rows := RenderCommitGraphRows([]GraphCommit{gc("m", "a", "b"), gc("a", "b")})
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if !strings.Contains(string(rows[1]), "<svg") {
		t.Fatalf("row markup = %q", string(rows[1]))
	}
}
