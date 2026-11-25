// search_test.go — the code-search API lanes (docs/features/20_search.md §6/§9/§10;
// docs/go/07_api.md §9.13): the frozen envelope, the gate order, the failure
// table, no-store, the owner-scope listing convention and discovery.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- search-service fakes (the api package tests the wire, not the engine) ----

type fakeSearchRepos struct {
	mu         sync.Mutex
	names      []string
	listErr    error
	target     search.Target
	resolveErr error
	ready      bool
	warmErr    error

	resolved []string // "repo@ref" the service asked for
	listed   []string
	warmed   []string
}

func (f *fakeSearchRepos) ListOwner(_ context.Context, owner string) ([]string, error) {
	f.mu.Lock()
	f.listed = append(f.listed, owner)
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string(nil), f.names...), nil
}

func (f *fakeSearchRepos) ResolveRepo(_ context.Context, repo, ref string) (search.Target, error) {
	f.mu.Lock()
	f.resolved = append(f.resolved, repo+"@"+ref)
	f.mu.Unlock()
	if f.resolveErr != nil {
		return search.Target{}, f.resolveErr
	}
	return f.target, nil
}

func (f *fakeSearchRepos) WarmRefs(_ context.Context, repo string) (search.Target, bool, error) {
	f.mu.Lock()
	f.warmed = append(f.warmed, repo)
	f.mu.Unlock()
	if f.warmErr != nil {
		return search.Target{}, false, f.warmErr
	}
	return f.target, f.ready, nil
}

type fakeSearchGrep struct {
	mu    sync.Mutex
	res   *git.GrepResult
	err   error
	revs  []string
	query git.GrepQuery
}

func (g *fakeSearchGrep) Grep(_ context.Context, _ *git.LocalRepo, rev string, q git.GrepQuery) (*git.GrepResult, error) {
	g.mu.Lock()
	g.revs = append(g.revs, rev)
	g.query = q
	g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	return g.res, nil
}

type fakeSearchAccess struct {
	deny     map[string]auth.AuthErrorKind // repo name → denial kind
	wholeErr *auth.AuthError
}

func (a *fakeSearchAccess) CheckRead(_ context.Context, _, repo string, _ auth.Principal) *auth.AuthError {
	if a.wholeErr != nil {
		return a.wholeErr
	}
	if k, ok := a.deny[repo]; ok {
		return &auth.AuthError{Kind: k, Why: "denied"}
	}
	return nil
}

// stubSearchService answers the interface directly for the one mapping row the
// real service cannot produce (an unexpected, unclassified error).
type stubSearchService struct {
	ans *search.Answer
	err error
}

func (s stubSearchService) Search(context.Context, search.Scope, search.Params, auth.Principal) (*search.Answer, error) {
	return s.ans, s.err
}

// searchFixture wires the real service over the fakes (the API tests exercise
// the mapping and the envelope, not the engine).
type searchFixture struct {
	*fixture
	repos  *fakeSearchRepos
	grep   *fakeSearchGrep
	access *fakeSearchAccess
}

func newSearchFixture(t *testing.T, opts search.Options) *searchFixture {
	t.Helper()
	f := newFixture(t)
	sf := &searchFixture{
		fixture: f,
		repos:   &fakeSearchRepos{ready: true},
		grep:    &fakeSearchGrep{res: searchGrepResult()},
		access:  &fakeSearchAccess{deny: map[string]auth.AuthErrorKind{}},
	}
	sf.repos.target = search.Target{
		Repo: &git.LocalRepo{Path: "/tmp/serving.git"},
		Ref:  "main",
		SHA:  fakeSHA,
	}
	if opts.Git == nil {
		opts.Git = sf.grep
	}
	if opts.Repos == nil {
		opts.Repos = sf.repos
	}
	if opts.Access == nil {
		opts.Access = sf.access
	}
	f.env.Search = search.New(opts)
	return sf
}

func searchGrepResult() *git.GrepResult {
	return &git.GrepResult{
		GrepStats: git.GrepStats{FileCount: 1, Matches: 2},
		Files: []git.GrepFile{{
			Path: "cmd/app/main.go",
			Matches: []git.GrepMatch{
				{Line: 12, Column: 6, Text: "func main() {"},
				{Line: 40, Column: 6, Text: "func mainLoop() {"},
			},
		}},
	}
}

// --- repository scope ---------------------------------------------------------

func TestSearchRepoAPIShape(t *testing.T) {
	sf := newSearchFixture(t, search.Options{})
	w := sf.do("GET", "/demo/hello/api/search?q=func+main", nil, nil, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("cache-control = %q, want %q", cc, ccNoStore)
	}
	if et := w.Header().Get("ETag"); et != "" {
		t.Fatalf("ETag = %q; search must never revalidate", et)
	}
	var ans search.Answer
	decodeJSON(t, w, &ans)
	if ans.Query != "func main" || ans.Type != search.TypeCode {
		t.Fatalf("query/type = %q/%q", ans.Query, ans.Type)
	}
	if ans.Scope.Kind != search.ScopeRepo || ans.Scope.Owner != "demo" || ans.Scope.Repo != "hello" {
		t.Fatalf("scope = %+v", ans.Scope)
	}
	if ans.Options.Limit != search.DefaultLimit || ans.Options.Paths == nil {
		t.Fatalf("options = %+v (paths must be [], limit default)", ans.Options)
	}
	if ans.Code == nil {
		t.Fatal("code section missing")
	}
	if !ans.Code.Complete || ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 {
		t.Fatalf("code completeness = %+v", ans.Code)
	}
	if len(ans.Code.Results) != 1 {
		t.Fatalf("results = %+v", ans.Code.Results)
	}
	r := ans.Code.Results[0]
	if r.Repo != "demo/hello" || r.Ref != "main" || r.SHA != fakeSHA || r.Status != search.StatusOK {
		t.Fatalf("result = %+v", r)
	}
	if !r.Complete || r.Truncated || r.TimedOut {
		t.Fatalf("result completeness = %+v", r)
	}
	if r.MatchCount != 2 || r.FileCount != 1 || len(r.Files) != 1 {
		t.Fatalf("result counts = %+v", r)
	}
	if got := r.Files[0].Matches[0]; got.Line != 12 || got.Column != 6 || got.Text != "func main() {" {
		t.Fatalf("first match = %+v", got)
	}
	// The service resolved the default ref and grepped the resolved sha.
	if len(sf.repos.resolved) != 1 || sf.repos.resolved[0] != "demo/hello@" {
		t.Fatalf("resolved = %v", sf.repos.resolved)
	}
	if len(sf.grep.revs) != 1 || sf.grep.revs[0] != fakeSHA {
		t.Fatalf("grep revs = %v (the answer is pinned to the resolved sha)", sf.grep.revs)
	}
}

func TestSearchRepoBrowserLane(t *testing.T) {
	sf := newSearchFixture(t, search.Options{})
	w := sf.do("GET", "/demo/hello/api-browser/search?q=func", nil, nil, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("browser lane status = %d body=%q", w.Code, w.Body.String())
	}
}

func TestSearchRepoBadQuery(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"missing q", "/demo/hello/api/search", "q is required"},
		{"empty q", "/demo/hello/api/search?q=", "q is required"},
		{"invalid regex", "/demo/hello/api/search?q=%28&regex=1", "invalid regular expression"},
		{"regex switch", "/demo/hello/api/search?q=x&regex=yes", "regex must be 0 or 1"},
		{"case switch", "/demo/hello/api/search?q=x&case=2", "case must be 0 or 1"},
		{"word switch", "/demo/hello/api/search?q=x&word=true", "word must be 0 or 1"},
		{"limit integer", "/demo/hello/api/search?q=x&limit=abc", "limit must be an integer"},
		{"malformed ref", "/demo/hello/api/search?q=x&ref=-bad", "ref"},
		{"unknown type", "/demo/hello/api/search?q=x&type=bogus", "type must be code, issues or all"},
		{"too many paths", "/demo/hello/api/search?q=x&path=a&path=b&path=c&path=d&path=e&path=f&path=g&path=h&path=i", "at most 8 path patterns"},
		{"pathspec magic", "/demo/hello/api/search?q=x&path=%3A!bad", "pathspec magic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSearchFixture(t, search.Options{})
			w := sf.do("GET", tc.url, nil, nil, readP())
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.want)
			}
			if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
				t.Fatalf("error cache-control = %q, want no-store", cc)
			}
		})
	}
}

func TestSearchRepoNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unknown repo", search.Wrap(search.ErrNotFound, "not found"), "not found"},
		{"unknown ref", search.Wrap(search.ErrNotFound, `unknown revision "nope"`), `unknown revision "nope"`},
		{"empty repo", search.Wrap(search.ErrNotFound, "empty repository: demo/hello"), "empty repository: demo/hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSearchFixture(t, search.Options{})
			sf.repos.resolveErr = tc.err
			w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchRepoUnavailable(t *testing.T) {
	t.Run("warming", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		sf.repos.resolveErr = search.Wrap(search.ErrWarming, search.ErrWarming.Error())
		w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra != "5" {
			t.Fatalf("Retry-After = %q, want 5", ra)
		}
		if !strings.Contains(w.Body.String(), "search is warming this repository") {
			t.Fatalf("body = %q", w.Body.String())
		}
	})
	t.Run("remote-served", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		sf.repos.resolveErr = search.Wrap(search.ErrNotLocal, search.ErrNotLocal.Error())
		w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "local serving copy") {
			t.Fatalf("body = %q", w.Body.String())
		}
	})
	t.Run("grep failure is never a 500", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		sf.grep.err = errors.New("git exploded with a path in it")
		w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
		if got := strings.TrimSpace(w.Body.String()); got != search.ErrUnavailable.Error() {
			t.Fatalf("body = %q, want the neutral message", got)
		}
	})
	t.Run("service refusals map to 401/403", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		sf.access.wholeErr = &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "denied"}
		w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status = %d body=%q", w.Code, w.Body.String())
		}
		sf.access.wholeErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "denied"}
		w = sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusForbidden {
			t.Fatalf("forbidden status = %d body=%q", w.Code, w.Body.String())
		}
	})
	t.Run("instance scope refusal is a 400", func(t *testing.T) {
		f := newFixture(t)
		f.env.Search = stubSearchService{err: search.Wrap(search.ErrUnsupportedScope, search.ErrUnsupportedScope.Error())}
		w := f.do("GET", "/api/v1/search?q=x&owner=demo&type=code", nil, nil, readP())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
	})
	t.Run("unclassified failure is the neutral 503", func(t *testing.T) {
		f := newFixture(t)
		f.env.Search = stubSearchService{err: errors.New("boom with a path in it")}
		w := f.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
		if got := strings.TrimSpace(w.Body.String()); got != search.ErrUnavailable.Error() {
			t.Fatalf("body = %q, want the neutral message", got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{Disabled: true})
		w := sf.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "disabled") {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
	})
	t.Run("unwired", func(t *testing.T) {
		f := newFixture(t) // no Env.Search
		w := f.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("repo status = %d", w.Code)
		}
		w = f.do("GET", "/api/v1/search?owner=demo&q=x&type=code", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("owner status = %d", w.Code)
		}
	})
}

// --- the read gate (repo convention: 401 anonymous / 403 signed-in) -----------

type denyReadAccess struct{ kind auth.AuthErrorKind }

func (d denyReadAccess) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return &auth.AuthError{Kind: d.kind, Why: "denied"}
}

func TestSearchRepoGateBeforeQueryValidation(t *testing.T) {
	// The dispatcher's CheckRead runs BEFORE decode, so a gated request with
	// no q is 401/403, never 400 (§8 step 3 before step 4).
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = false
	f.env.Access = denyReadAccess{kind: auth.ErrUnauthorized}
	w := f.do("GET", "/demo/hello/api/search", nil, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	f.env.Access = denyReadAccess{kind: auth.ErrForbidden}
	w = f.do("GET", "/demo/hello/api/search", nil, nil, readP())
	if w.Code != http.StatusForbidden {
		t.Fatalf("signed-in status = %d body=%q", w.Code, w.Body.String())
	}

	f.env.Access = denyReadAccess{kind: auth.ErrUnavailable}
	w = f.do("GET", "/demo/hello/api/search?q=x", nil, nil, readP())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("identity-down status = %d body=%q", w.Code, w.Body.String())
	}
}

// --- owner scope ---------------------------------------------------------------

func TestSearchOwnerScope(t *testing.T) {
	sf := newSearchFixture(t, search.Options{})
	sf.repos.names = []string{"hello", "walgit"}
	sf.access.deny["walgit"] = auth.ErrForbidden // denied: absent from every field
	w := sf.do("GET", "/api/v1/search?q=func&owner=demo&type=code", nil, nil, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("cache-control = %q", cc)
	}
	var ans search.Answer
	decodeJSON(t, w, &ans)
	if ans.Scope.Kind != search.ScopeOwner || ans.Scope.Owner != "demo" || ans.Scope.Repo != "" {
		t.Fatalf("scope = %+v", ans.Scope)
	}
	if ans.Code == nil || !ans.Code.Complete {
		t.Fatalf("code = %+v", ans.Code)
	}
	if ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 {
		t.Fatalf("counts = %+v (the denied repo contributes no count)", ans.Code)
	}
	if len(ans.Code.Results) != 1 || ans.Code.Results[0].Repo != "demo/hello" {
		t.Fatalf("results = %+v", ans.Code.Results)
	}
	for _, id := range sf.repos.warmed {
		if id == "demo/walgit" {
			t.Fatalf("denied repository was warmed: %v", sf.repos.warmed)
		}
	}
}

func TestSearchOwnerAnonymousPublic(t *testing.T) {
	sf := newSearchFixture(t, search.Options{})
	sf.repos.names = []string{"hello"}
	w := sf.do("GET", "/api/v1/search?q=func&owner=demo&type=code", nil, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous public owner search = %d body=%q", w.Code, w.Body.String())
	}
}

func TestSearchOwnerUnknownOwnerIsTheListingShape(t *testing.T) {
	// /api/v1/owners/{owner}/repos answers 200 [] for an unknown owner; owner
	// search follows the same convention: an empty code section, never 404.
	sf := newSearchFixture(t, search.Options{})
	sf.repos.names = nil
	w := sf.do("GET", "/api/v1/search?q=func&owner=nobody&type=code", nil, nil, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var raw map[string]any
	decodeJSON(t, w, &raw)
	code := raw["code"].(map[string]any)
	results, ok := code["results"].([]any)
	if !ok || len(results) != 0 {
		t.Fatalf("results = %v, want [] (never null)", code["results"])
	}
	if complete := code["complete"]; complete != true {
		t.Fatalf("complete = %v", complete)
	}
}

func TestSearchOwnerCappedAndCold(t *testing.T) {
	t.Run("candidate cap sets complete:false", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		names := make([]string, search.MaxOwnerCandidates+1)
		for i := range names {
			names[i] = fmt.Sprintf("repo%02d", i)
		}
		sf.repos.names = names
		w := sf.do("GET", "/api/v1/search?q=func&owner=demo&type=code", nil, nil, readP())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
		var ans search.Answer
		decodeJSON(t, w, &ans)
		if ans.Code.Complete {
			t.Fatal("a capped fan-out must report complete:false")
		}
		if ans.Code.ReposConsidered != search.MaxOwnerCandidates {
			t.Fatalf("considered = %d, want %d", ans.Code.ReposConsidered, search.MaxOwnerCandidates)
		}
	})
	t.Run("not warm is a per-repo status", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		sf.repos.names = []string{"hello"}
		sf.repos.ready = false
		w := sf.do("GET", "/api/v1/search?q=func&owner=demo&type=code", nil, nil, readP())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
		var ans search.Answer
		decodeJSON(t, w, &ans)
		if ans.Code.Complete || len(ans.Code.Results) != 1 {
			t.Fatalf("code = %+v", ans.Code)
		}
		r := ans.Code.Results[0]
		if r.Status != search.StatusNotWarm || r.Reason != "not cached on this instance" {
			t.Fatalf("result = %+v", r)
		}
	})
}

func TestSearchOwnerGate(t *testing.T) {
	// Owner scope is a non-repo surface: anonymous_read=false is a real 401
	// before the service is consulted.
	sf := newSearchFixture(t, search.Options{})
	sf.env.Cfg.Server.Auth.AnonymousRead = false
	w := sf.do("GET", "/api/v1/search?owner=demo&q=x&type=code", nil, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous owner search = %d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

func TestSearchOwnerValidationAndIssues(t *testing.T) {
	t.Run("owner required", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		w := sf.do("GET", "/api/v1/search?q=func&type=code", nil, nil, readP())
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "owner is required") {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
	})
	t.Run("ref is repository scope only", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		w := sf.do("GET", "/api/v1/search?q=func&owner=demo&type=code&ref=main", nil, nil, readP())
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "repository scope") {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
	})
	t.Run("issue default names the P4 refusal", func(t *testing.T) {
		sf := newSearchFixture(t, search.Options{})
		w := sf.do("GET", "/api/v1/search?q=func&owner=demo", nil, nil, readP())
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "issue search is not available") {
			t.Fatalf("body = %q", w.Body.String())
		}
	})
}

// --- methods / discovery / contract -------------------------------------------

func TestSearchMethodAndDiscovery(t *testing.T) {
	sf := newSearchFixture(t, search.Options{})

	// Repo lane: GET-only per the dispatcher's table convention → HEAD/POST 405.
	for _, method := range []string{"HEAD", "POST"} {
		w := sf.do(method, "/demo/hello/api/search?q=x", nil, nil, readP())
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s repo route = %d, want 405", method, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "GET" {
			t.Fatalf("%s Allow = %q", method, allow)
		}
	}
	// Non-repo lane: same table row shape (ServeMux GET ⇒ HEAD, POST 405).
	w := sf.do("POST", "/api/v1/search?owner=demo&q=x", nil, nil, readP())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST owner route = %d, want 405", w.Code)
	}

	// Two-way discovery: both canonical templates are exposed AND owned by a
	// table row with a live handler (TestExposedCoversRoutes' rule for the
	// core table: no phantoms, nothing missing).
	want := map[string]string{
		"/api/v1/search":             "searchOwner",
		"/{owner}/{repo}/api/search": "searchRepo",
	}
	eps := discoveryEndpoints()
	seen := map[string]bool{}
	for _, e := range eps {
		if _, ok := want[e]; ok {
			seen[e] = true
		}
	}
	for tmpl := range want {
		if !seen[tmpl] {
			t.Errorf("discovery missing %q: %v", tmpl, eps)
		}
	}
	rows := Routes(nil)
	for tmpl := range want {
		found := false
		for _, rt := range rows {
			if rt.Template == tmpl {
				found = true
				if !rt.Expose {
					t.Errorf("%q row is not Expose", tmpl)
				}
				if rt.Handler == nil {
					t.Errorf("%q row has no handler", tmpl)
				}
			}
		}
		if !found {
			t.Errorf("no route row carries template %q", tmpl)
		}
	}
	// The browser twins answer the same handlers (they share the handler
	// pointer); no separate discovery entry is invented.
	var repo, browser *Route
	for i := range rows {
		rt := &rows[i]
		switch {
		case rt.Sub == "search":
			repo = rt
		case rt.Sub == "/api-browser/v1/search":
			browser = rt
		}
	}
	if repo == nil || browser == nil {
		t.Fatalf("missing lane rows: repo=%v browser=%v", repo, browser)
	}
	if browser.Expose || browser.Template != "" {
		t.Fatalf("browser twin must not add a discovery entry: %+v", browser)
	}
}

// --- decode helpers ------------------------------------------------------------

func TestDecodeSearchParams(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?q=a&type=all&ref=main&regex=1&case=1&word=1&path=a&path=b&limit=7", nil)
	p, err := decodeSearchParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "a" || p.Type != "all" || p.Ref != "main" || !p.Regex || !p.Case || !p.Word {
		t.Fatalf("params = %+v", p)
	}
	if len(p.Paths) != 2 || p.Paths[1] != "b" || p.Limit != 7 {
		t.Fatalf("paths/limit = %+v", p)
	}
	// Absent knobs keep the documented defaults (decoded as zero values).
	r = httptest.NewRequest("GET", "/x", nil)
	p, err = decodeSearchParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Regex || p.Case || p.Word || p.Limit != 0 || p.Paths != nil {
		t.Fatalf("defaults = %+v", p)
	}
	if _, err := decodeSwitch("0", "regex"); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLimit(" 12 "); err != nil {
		t.Fatal(err)
	}
}
