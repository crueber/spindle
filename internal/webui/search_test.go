package webui

// search_test.go — the two Feature 20 page surfaces (docs/features/20_search.md
// §7; docs/go/12_web_ui.md §2.3.1). The assertions are rendered-page facts, not
// template reads:
//
//   - the repo header carries the compact search box on every repository page;
//   - the repo page renders the masthead + Code tab, exactly one h1, a labelled
//     native GET form, and one primary action (its own Search submit);
//   - results group by file, link to the SHA-pinned blob line, and wrap the
//     matched span in <mark> while every user byte stays escaped;
//   - completeness is stated: truncated, timed out, not cached, not searched,
//     failed, and searched-of-considered counts;
//   - the gate is the Code page's (anonymous private = 401, signed-in denial =
//     404) and a page answer is no-store without an ETag;
//   - the owner page needs an owner, filters nothing itself and leaks no denied
//     repository (the service already filtered; the page renders what it gets).
//
// The fake is the api.SearchService seam, so the page is exercised exactly as
// composition wires it (api.Env.Search).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- fakes ------------------------------------------------------------------

type searchFakeService struct {
	calls  int
	scope  search.Scope
	params search.Params
	answer *search.Answer
	err    error
}

func (f *searchFakeService) Search(_ context.Context, scope search.Scope, params search.Params, _ auth.Principal) (*search.Answer, error) {
	f.calls++
	f.scope, f.params = scope, params
	if f.err != nil {
		return nil, f.err
	}
	if f.answer != nil {
		// Mirror the service's normalized echo (internal/search Query.options):
		// the page renders what was actually searched, not what was asked.
		f.answer.Scope = scope
		if p := strings.TrimSpace(params.Type); p != "" {
			f.answer.Type = search.Type(p)
		} else if scope.Kind == search.ScopeRepo {
			f.answer.Type = search.TypeCode
		} else {
			f.answer.Type = search.TypeIssues
		}
		paths := params.Paths
		if paths == nil {
			paths = []string{}
		}
		limit := params.Limit
		if limit <= 0 {
			limit = search.DefaultLimit
		}
		f.answer.Options = search.QueryOptions{
			Regex: params.Regex, CaseSensitive: params.Case, Word: params.Word,
			Paths: paths, Limit: limit,
		}
	}
	return f.answer, nil
}

// searchFixture is the repo fixture with the search seam bound and the repo
// summary/registry the chrome needs.
func searchFixture(t *testing.T) (*repoFixture, *searchFakeService) {
	t.Helper()
	f := newRepoFixture(t, true)
	svc := &searchFakeService{}
	f.s.env.API.Search = svc
	return f, svc
}

// searchGet drives one page request and keeps the recorder (headers matter for
// the cache class).
func searchGet(f *repoFixture, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

const searchTestSHA = "4f1c9d2e7a5b3c8d1e0f2a4b6c8d0e2f4a6b8c0d"

// repoSearchAnswer is one repository-scope answer with two files.
func repoSearchAnswer() *search.Answer {
	return &search.Answer{
		Query:   "needle",
		Type:    search.TypeCode,
		Options: search.QueryOptions{Limit: 100},
		Scope:   search.Scope{Kind: search.ScopeRepo, Owner: "demo", Repo: "spindle"},
		Code: &search.CodeSection{
			Complete:        true,
			ReposConsidered: 1,
			ReposSearched:   1,
			Results: []search.Result{{
				Repo:       "demo/spindle",
				Ref:        "refs/heads/main",
				SHA:        searchTestSHA,
				Status:     search.StatusOK,
				Complete:   true,
				MatchCount: 2,
				FileCount:  2,
				Files: []search.File{
					{Path: "cmd/app/main.go", Matches: []search.Match{{Line: 12, Column: 6, Text: "func needle() {"}}},
					{Path: "pkg/util/util.go", Matches: []search.Match{{Line: 3, Column: 1, Text: "// needle helper"}}},
				},
			}},
		},
	}
}

func applySearchAnswer(svc *searchFakeService, ans *search.Answer) *searchFakeService {
	svc.answer = ans
	return svc
}

// --- repo page: form, chrome, header box ------------------------------------

func TestRepoSearchHeaderBoxOnEveryRepoPage(t *testing.T) {
	t.Parallel()
	f, _ := searchFixture(t)
	for _, path := range []string{"/demo/spindle", "/demo/spindle/commits", "/demo/spindle/search"} {
		body := f.mustGet(path, http.StatusOK)
		if !strings.Contains(body, `action="/demo/spindle/search"`) {
			t.Errorf("%s: no repo-header search form action", path)
		}
		if !strings.Contains(body, `role="search"`) {
			t.Errorf("%s: header form is not a search landmark", path)
		}
		if !strings.Contains(body, `id="repo-search-q"`) || !strings.Contains(body, `for="repo-search-q"`) {
			t.Errorf("%s: header search input is not labelled", path)
		}
		// The box is chrome: its submit is a plain .btn, never the page primary.
		if strings.Contains(body, `class="btn primary whitespace-nowrap">Search</button>`) && path != "/demo/spindle/search" {
			t.Errorf("%s: header submit carries the primary class", path)
		}
	}
}

func TestRepoSearchPageEmptyQuery(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	rec := searchGet(f, "/demo/spindle/search")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	if svc.calls != 0 {
		t.Errorf("empty query called the service %d times, want 0", svc.calls)
	}
	for _, want := range []string{
		`method="get" action="/demo/spindle/search"`,
		`id="search-q"`,
		`name="q"`,
		`name="type" value="code" checked`,
		`id="search-ref"`,
		`name="regex" value="1"`,
		`name="path"`,
		"Enter a term to search this repository.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search form missing %q", want)
		}
	}
	if n := a11yH1Count(body); n != 1 {
		t.Errorf("h1 count = %d, want 1 (the repo masthead)", n)
	}
	if got := activeTabOf(t, body); got != "code" {
		t.Errorf("active tab = %q, want code", got)
	}
	if !strings.Contains(body, `<h2 class="text-xl font-semibold tracking-tight">Search <span class="font-mono`) {
		t.Error("page title is not an h2 under the masthead h1")
	}
}

func TestRepoSearchEmptyRepository(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	f.view.summary.Health = api.RepoHealthEmpty
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	wantStatus(t, rec, http.StatusOK)
	if svc.calls != 0 {
		t.Errorf("empty repository called the service %d times, want 0", svc.calls)
	}
	wantContains(t, rec, "This repository is empty", `action="/demo/spindle/search"`)
}

// --- repo page: results -----------------------------------------------------

func TestRepoSearchRendersGroupedFileResults(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, repoSearchAnswer())
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)

	// The SHA-pinned blob link, with the line anchor the blob page renders.
	shaHref := "/demo/spindle/blob/" + searchTestSHA + "/cmd/app/main.go"
	if !strings.Contains(body, `href="`+shaHref+`"`) {
		t.Errorf("missing file link %q", shaHref)
	}
	if !strings.Contains(body, `href="`+shaHref+`#L12"`) {
		t.Errorf("missing line anchor %q#L12", shaHref)
	}
	// The row states path (heading), line and column (the anchor) and the text.
	if !strings.Contains(body, `>L12:6</a>`) {
		t.Errorf("the result row does not show line:column: %.400s", body)
	}
	// Grouped by file, matches as a list.
	if !strings.Contains(body, "<ul") || !strings.Contains(body, "cmd/app/main.go") || !strings.Contains(body, "pkg/util/util.go") {
		t.Error("results are not grouped by file with a list of matches")
	}
	// The matched span is server-rendered and escaped.
	if !strings.Contains(body, "func <mark") && !strings.Contains(body, `<mark class="rounded-sm bg-[var(--pd-sel)]`) {
		t.Error("match is not wrapped in <mark>")
	}
	if !strings.Contains(body, ">needle</mark>") {
		t.Errorf("the marked span is not the matched text: %.400s", body)
	}
	// The count line states matches, files and what was searched.
	wantContains(t, rec, "2 matches in 2 files", "searched main ("+shortSHA(searchTestSHA)+")")
	if !strings.Contains(body, `role="status"`) {
		t.Error("the count line is not a status region")
	}
	if got := countPrimaryButtons([]byte(body)); got != 1 {
		t.Errorf("primary buttons = %d, want 1 (the search submit; Clone demotes)", got)
	}
}

func TestRepoSearchCompletenessStates(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		f, svc := searchFixture(t)
		ans := repoSearchAnswer()
		ans.Code.Complete = false
		ans.Code.Results[0].Complete = false
		ans.Code.Results[0].Truncated = true
		ans.Code.Results[0].MatchCount = 200
		applySearchAnswer(svc, ans)
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusOK)
		wantContains(t, rec,
			"This answer is incomplete",
			"truncated",
			"showing the first 200 matches; refine the query",
		)
	})

	t.Run("timed out", func(t *testing.T) {
		f, svc := searchFixture(t)
		ans := repoSearchAnswer()
		ans.Code.Complete = false
		ans.Code.Results[0].Complete = false
		ans.Code.Results[0].TimedOut = true
		applySearchAnswer(svc, ans)
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusOK)
		wantContains(t, rec, "timed out", "deadline fired; these results are partial", "This answer is incomplete")
	})

	t.Run("no matches", func(t *testing.T) {
		f, svc := searchFixture(t)
		ans := repoSearchAnswer()
		ans.Code.Results[0].Files = nil
		ans.Code.Results[0].MatchCount = 0
		ans.Code.Results[0].FileCount = 0
		applySearchAnswer(svc, ans)
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusOK)
		wantContains(t, rec, "No matches")
	})
}

func TestRepoSearchEscapesUserText(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	ans := &search.Answer{
		Query:   `<script>alert("x")</script>`,
		Type:    search.TypeCode,
		Options: search.QueryOptions{Limit: 100},
		Scope:   search.Scope{Kind: search.ScopeRepo, Owner: "demo", Repo: "spindle"},
		Code: &search.CodeSection{
			Complete:        true,
			ReposConsidered: 1,
			ReposSearched:   1,
			Results: []search.Result{{
				Repo: "demo/spindle", Ref: "refs/heads/main", SHA: searchTestSHA,
				Status: search.StatusOK, Complete: true, MatchCount: 1, FileCount: 1,
				Files: []search.File{{
					Path: "evil<script>.txt",
					Matches: []search.Match{{
						Line: 1, Column: 5,
						Text: `saw <script>alert(1)</script> <b>needle</b>`,
					}},
				}},
			}},
		},
	}
	applySearchAnswer(svc, ans)
	rec := searchGet(f, "/demo/spindle/search?q=%3Cscript%3Ealert(%22x%22)%3C%2Fscript%3E")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)

	if strings.Contains(body, "</script>alert") || strings.Contains(body, "<b>needle</b>") || strings.Contains(body, `<script>alert(1)`) {
		t.Fatalf("user bytes reached the page unescaped: %.600s", body)
	}
	// The query echo is escaped in the input value.
	if !strings.Contains(body, `value="&lt;script&gt;`) {
		t.Errorf("the query echo is not escaped: %.400s", body)
	}
	// The match text is escaped around the server's <mark>.
	if !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "&lt;/script&gt;") {
		t.Errorf("the matched line is not escaped: %.600s", body)
	}
	// The path is escaped in the link and in the heading.
	if strings.Contains(body, `href="/demo/spindle/blob/`+searchTestSHA+`/evil<script>`) {
		t.Error("the result path reached the href unescaped")
	}
}

// --- repo page: failure mapping ---------------------------------------------

func TestRepoSearchInvalidQueryKeepsTheForm(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.err = &search.InvalidError{Field: "q", Why: "invalid regular expression: missing ]"}
	rec := searchGet(f, "/demo/spindle/search?q=%5B&type=code")
	wantStatus(t, rec, http.StatusBadRequest)
	body := bodyText(rec)
	if svc.calls != 1 || svc.params.Regex {
		t.Fatalf("service call = %d (regex %v)", svc.calls, svc.params.Regex)
	}
	for _, want := range []string{`role="alert"`, "invalid regular expression", `id="search-q"`, "value=\"[\""} {
		if !strings.Contains(body, want) {
			t.Errorf("rejected query page missing %q: %.400s", want, body)
		}
	}
}

func TestRepoSearchInvalidKnobs(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	for _, tc := range []struct{ path, want string }{
		{"/demo/spindle/search?q=x&regex=2", "regex must be 0 or 1"},
		{"/demo/spindle/search?q=x&case=yes", "case must be 0 or 1"},
		{"/demo/spindle/search?q=x&word=on", "word must be 0 or 1"},
		{"/demo/spindle/search?q=x&type=bogus", "type must be code, issues or all"},
	} {
		rec := searchGet(f, tc.path)
		wantStatus(t, rec, http.StatusBadRequest)
		wantContains(t, rec, tc.want, `role="alert"`)
	}
	if svc.calls != 0 {
		t.Errorf("shape errors reached the service %d times, want 0", svc.calls)
	}
}

func TestRepoSearchColdAndRemoteAreNotices(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err        error
		wantStatus int
		want       string
	}{
		{search.Wrap(search.ErrWarming, search.ErrWarming.Error()), http.StatusServiceUnavailable, "warming this repository"},
		{search.Wrap(search.ErrNotLocal, search.ErrNotLocal.Error()), http.StatusServiceUnavailable, "needs a local serving copy"},
		{search.Wrap(search.ErrDisabled, search.ErrDisabled.Error()), http.StatusServiceUnavailable, "search is disabled"},
		{search.Wrap(search.ErrIssuesUnavailable, search.ErrIssuesUnavailable.Error()), http.StatusServiceUnavailable, "issue search is not available"},
		{errors.New("boom with a path in it"), http.StatusServiceUnavailable, "search is temporarily unavailable"},
	} {
		f, svc := searchFixture(t)
		svc.err = tc.err
		rec := searchGet(f, "/demo/spindle/search?q=needle&type=all")
		wantStatus(t, rec, tc.wantStatus)
		body := bodyText(rec)
		if !strings.Contains(body, tc.want) {
			t.Errorf("%v: missing %q", tc.err, tc.want)
		}
		// The box stays visible, and the unexpected error's text is never
		// echoed (it may carry a path).
		if !strings.Contains(body, `id="search-q"`) {
			t.Errorf("%v: the search box disappeared", tc.err)
		}
		if strings.Contains(body, "boom with a path in it") {
			t.Errorf("%v: the raw error leaked into the page", tc.err)
		}
	}
}

func TestRepoSearchWarmingSetsRetryAfter(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.err = search.Wrap(search.ErrWarming, search.ErrWarming.Error())
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
}

func TestRepoSearchUnknownRefIs404(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.err = search.Wrap(search.ErrNotFound, `unknown revision "nope"`)
	rec := searchGet(f, "/demo/spindle/search?q=needle&ref=nope")
	wantStatus(t, rec, http.StatusNotFound)
	wantContains(t, rec, "unknown revision")
	wantNotContains(t, rec, "repo-header", "repo-search-q")
}

func TestRepoSearchUnwiredService(t *testing.T) {
	t.Parallel()
	f, _ := searchFixture(t)
	f.s.env.API.Search = nil
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "search is temporarily unavailable", `id="search-q"`)
}

// --- gating and caching -----------------------------------------------------

func TestRepoSearchGateParity(t *testing.T) {
	t.Run("anonymous private is 401", func(t *testing.T) {
		f, _ := searchFixture(t)
		f.anonRead = false
		cfg := f.cfgForTest()
		cfg.Server.Auth.AnonymousRead = false
		f.s.env.Cfg = cfg
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusUnauthorized)
		wantNotContains(t, rec, "repo-header", "repo-search-q", "Search demo/spindle")
	})
	t.Run("signed-in denial is the 404 page", func(t *testing.T) {
		f, _ := searchFixture(t)
		f.principal = auth.Principal{Name: "alice"}
		f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no access"}
		}
		f.rebind()
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusNotFound)
		wantNotContains(t, rec, "repo-header", "repo-search-q", "demo/spindle")
	})
	t.Run("public repo stays readable with anonymous_read off", func(t *testing.T) {
		// The identity gate wins over the flag (Forgejo #345): a public repo
		// answers even when anonymous_read is off.
		f, svc := searchFixture(t)
		cfg := f.cfgForTest()
		cfg.Server.Auth.AnonymousRead = false
		f.s.env.Cfg = cfg
		f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError { return nil }
		f.rebind()
		applySearchAnswer(svc, repoSearchAnswer())
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, http.StatusOK)
	})
}

func TestSearchPagesAreNoStoreWithoutETag(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, repoSearchAnswer())
	for _, path := range []string{"/demo/spindle/search?q=needle", "/search?owner=demo&q=needle"} {
		rec := searchGet(f, path)
		wantStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
		if got := rec.Header().Get("ETag"); got != "" {
			t.Errorf("%s: ETag = %q on a no-store body", path, got)
		}
	}
	// A conditional request must not be answered 304 from a shared ETag.
	req := httptest.NewRequest(http.MethodGet, "/demo/spindle/search?q=needle", nil)
	req.Header.Set("If-None-Match", `"anything"`)
	rec := httptest.NewRecorder()
	f.s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("If-None-Match on a search page = %d, want 200", rec.Code)
	}
}

// --- owner page -------------------------------------------------------------

func TestOwnerSearchPageNeedsOwner(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	rec := searchGet(f, "/search")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	if svc.calls != 0 {
		t.Errorf("a missing owner called the service %d times, want 0", svc.calls)
	}
	for _, want := range []string{
		"Version 1 searches one owner at a time",
		`href="/explore"`,
		`id="search-owner"`,
		`name="owner"`,
		`id="search-q"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner page missing %q", want)
		}
	}
	if n := a11yH1Count(body); n != 1 {
		t.Errorf("h1 count = %d, want 1", n)
	}
	if strings.Contains(body, `id="search-ref"`) {
		t.Error("the owner form rendered a ref control (repository scope only)")
	}
}

func TestOwnerSearchPageRequiresTheReadGate(t *testing.T) {
	t.Parallel()
	f, _ := searchFixture(t)
	cfg := f.cfgForTest()
	cfg.Server.Auth.AnonymousRead = false
	f.s.env.Cfg = cfg
	rec := searchGet(f, "/search?owner=demo&q=needle")
	wantStatus(t, rec, http.StatusUnauthorized)
}

func TestOwnerSearchPageGroupsByRepository(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, &search.Answer{
		Query:   "needle",
		Type:    search.TypeCode,
		Options: search.QueryOptions{Limit: 100},
		Scope:   search.Scope{Kind: search.ScopeOwner, Owner: "acme"},
		Code: &search.CodeSection{
			Complete:        false,
			ReposConsidered: 2,
			ReposSearched:   1,
			Results: []search.Result{
				{
					Repo: "acme/app", Ref: "refs/heads/main", SHA: searchTestSHA,
					Status: search.StatusOK, Complete: true, MatchCount: 1, FileCount: 1,
					Files: []search.File{{Path: "main.go", Matches: []search.Match{{Line: 4, Column: 1, Text: "// needle"}}}},
				},
				{
					Repo: "acme/cold", Ref: "refs/heads/main",
					Status: search.StatusNotWarm, Reason: "not cached on this instance",
				},
			},
		},
	})
	rec := searchGet(f, "/search?owner=acme&q=needle")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)

	if svc.scope != (search.Scope{Kind: search.ScopeOwner, Owner: "acme"}) {
		t.Errorf("scope = %+v", svc.scope)
	}
	if svc.params.Type != string(search.TypeCode) {
		t.Errorf("page default type = %q, want code", svc.params.Type)
	}
	// Grouped by repository, with the same blob anatomy as the repo page.
	for _, want := range []string{
		`href="/acme/app"`,
		`href="/acme/app/blob/` + searchTestSHA + `/main.go#L4"`,
		"acme/cold",
		"not cached",
		"not cached on this instance",
		"searched 1 of 2 repositories",
		"This answer is incomplete",
		">needle</mark>",
		`href="/search?owner=acme"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner page missing %q", want)
		}
	}
	if n := a11yH1Count(body); n != 1 {
		t.Errorf("h1 count = %d, want 1", n)
	}
}

// TestOwnerSearchPageRendersOnlyWhatItIsGiven: the service is the only
// authority on visibility. A denied repository is absent from the answer, so
// the page must not invent a name, a count or a hint about it.
func TestOwnerSearchPageRendersOnlyWhatItIsGiven(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, &search.Answer{
		Query:   "needle",
		Type:    search.TypeCode,
		Options: search.QueryOptions{Limit: 100},
		Scope:   search.Scope{Kind: search.ScopeOwner, Owner: "acme"},
		Code: &search.CodeSection{
			Complete: true, ReposConsidered: 1, ReposSearched: 1,
			Results: []search.Result{{
				Repo: "acme/app", Ref: "refs/heads/main", SHA: searchTestSHA,
				Status: search.StatusOK, Complete: true, MatchCount: 1, FileCount: 1,
				Files: []search.File{{Path: "main.go", Matches: []search.Match{{Line: 1, Column: 1, Text: "needle"}}}},
			}},
		},
	})
	rec := searchGet(f, "/search?owner=acme&q=needle")
	wantStatus(t, rec, http.StatusOK)
	for _, secret := range []string{"acme/secret", "2 repositories"} {
		if strings.Contains(bodyText(rec), secret) {
			t.Errorf("owner page mentions %q, which the service did not return", secret)
		}
	}
}

func TestOwnerSearchPageRendersIssueSection(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, &search.Answer{
		Query:   "crash",
		Type:    search.TypeIssues,
		Options: search.QueryOptions{Limit: 100},
		Scope:   search.Scope{Kind: search.ScopeOwner, Owner: "acme"},
		Issues: &search.IssueSection{
			Complete: false,
			Scanned:  search.IssueScan{Threads: 18, Events: 500, IndexComplete: true},
			Hits: []search.IssueHit{{
				Repo: "acme/app", Num: 42, Kind: "issue", Title: "Crash on empty ref",
				State: "open", Author: "jane", Labels: []string{"bug"},
				UpdatedAt: "2026-11-20T10:11:12Z", Matched: "title",
				URL: "/acme/app/issues/42",
			}},
		},
	})
	rec := searchGet(f, "/search?owner=acme&q=crash&type=issues")
	wantStatus(t, rec, http.StatusOK)
	for _, want := range []string{
		`href="/acme/app/issues/42"`,
		"Crash on empty ref",
		"matched title",
		"scanned 18 threads and 500 events",
		"chip chip-open",
		"The issue scan hit its bound",
	} {
		if !strings.Contains(bodyText(rec), want) {
			t.Errorf("issue section missing %q", want)
		}
	}
}

func TestOwnerSearchPageNoCandidates(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	// An unknown owner and an owner whose repositories are all denied look the
	// same on the wire (the service omits denied names from every field): the
	// page states the empty candidate set without naming anything.
	applySearchAnswer(svc, &search.Answer{
		Query: "needle", Type: search.TypeCode, Options: search.QueryOptions{Limit: 100},
		Scope: search.Scope{Kind: search.ScopeOwner, Owner: "acme"},
		Code:  &search.CodeSection{Complete: true},
	})
	rec := searchGet(f, "/search?owner=acme&q=needle")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"No repositories were considered: the owner has none, or none are visible to you.",
		"Nothing was searched",
	)
}

func TestSearchFormsForwardTheDocumentedKnobs(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.answer = repoSearchAnswer()
	// The textarea posts one value with newlines; a hand-written URL may repeat
	// ?path=. Both reach the service as the same list.
	rec := searchGet(f, "/demo/spindle/search?q=needle&regex=1&case=1&word=1&ref=v1&path=cmd%2F%0A*.go&path=pkg")
	wantStatus(t, rec, http.StatusOK)
	if !svc.params.Regex || !svc.params.Case || !svc.params.Word {
		t.Errorf("switches not forwarded: %+v", svc.params)
	}
	if svc.params.Ref != "v1" {
		t.Errorf("ref = %q, want v1", svc.params.Ref)
	}
	want := []string{"cmd/", "*.go", "pkg"}
	if len(svc.params.Paths) != len(want) {
		t.Fatalf("paths = %v, want %v", svc.params.Paths, want)
	}
	for i := range want {
		if svc.params.Paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", svc.params.Paths, want)
		}
	}
	// The re-rendered form reflects the normalised query.
	wantContains(t, rec, `name="regex" value="1" checked`, `name="case" value="1" checked`, `name="word" value="1" checked`, `value="v1"`, "cmd/\n*.go\npkg")

	// Owner scope never forwards a ref.
	svc.params = search.Params{}
	searchGet(f, "/search?owner=acme&q=needle&ref=v1")
	if svc.params.Ref != "" {
		t.Errorf("owner scope forwarded ref %q", svc.params.Ref)
	}
}

func TestSearchTypeRadioAndOptionsOpen(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.answer = repoSearchAnswer()
	rec := searchGet(f, "/demo/spindle/search?q=needle&type=all&word=1")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `name="type" value="all" checked`, "<details class=\"mt-3\" open>")
}

// --- failure arms -----------------------------------------------------------

func TestRepoSearchRepoOpenFailure(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	f.reg.exists = map[string]bool{} // registry says the repo is gone
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	wantStatus(t, rec, http.StatusNotFound)
	wantNotContains(t, rec, "repo-header", "repo-search-q")
	if svc.calls != 0 {
		t.Errorf("a missing repository reached the service %d times", svc.calls)
	}
}

func TestOwnerSearchPageEmptyQuery(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	rec := searchGet(f, "/search?owner=acme")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Enter a term to search repositories under acme.")
	if svc.calls != 0 {
		t.Errorf("empty query called the service %d times", svc.calls)
	}
}

func TestSearchCallerCancellationWritesNothing(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	svc.err = context.Canceled
	rec := searchGet(f, "/demo/spindle/search?q=needle")
	if rec.Body.Len() != 0 {
		t.Errorf("a cancelled search wrote a body: %.200s", rec.Body.String())
	}
}

func TestSearchServiceAuthErrorsKeepTheirClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want int
	}{
		{search.Wrap(search.ErrUnauthorized, search.ErrUnauthorized.Error()), http.StatusUnauthorized},
		{search.Wrap(search.ErrForbidden, search.ErrForbidden.Error()), http.StatusForbidden},
	} {
		f, svc := searchFixture(t)
		svc.err = tc.err
		rec := searchGet(f, "/demo/spindle/search?q=needle")
		wantStatus(t, rec, tc.want)
	}
}

func TestOwnerSearchUnwiredAPI(t *testing.T) {
	t.Parallel()
	f, _ := searchFixture(t)
	f.s.env.API = nil
	rec := searchGet(f, "/search?owner=acme&q=needle")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "search is temporarily unavailable")
}

func TestRepoSearchDegradedStatuses(t *testing.T) {
	t.Parallel()
	f, svc := searchFixture(t)
	applySearchAnswer(svc, &search.Answer{
		Query: "needle", Type: search.TypeCode, Options: search.QueryOptions{Limit: 100},
		Scope: search.Scope{Kind: search.ScopeOwner, Owner: "acme"},
		Code: &search.CodeSection{
			ReposConsidered: 3,
			Results: []search.Result{
				{Repo: "acme/a", Ref: "refs/heads/main", Status: search.StatusNotSearched, Reason: "request deadline reached"},
				{Repo: "acme/b", Ref: "refs/heads/main", Status: search.StatusFailed, Reason: "search failed"},
				{Repo: "acme/c", Ref: "refs/heads/main", Status: search.Status("weird")},
			},
		},
	})
	rec := searchGet(f, "/search?owner=acme&q=needle")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"not searched", "request deadline reached",
		"failed", "search failed",
		"chip-neutral", "Nothing was searched",
	)
}

func TestApplyAnswerNil(t *testing.T) {
	t.Parallel()
	d := &searchPage{}
	d.applyAnswer(nil) // a nil answer is a no-op, never a panic
	if d.Queried || d.Code != nil || d.StatusLine != "" {
		t.Fatalf("nil answer mutated the page: %+v", d)
	}
}

// --- pure helpers -----------------------------------------------------------

func TestDecodeSearchSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw   string
		want  bool
		fails bool
	}{
		{"", false, false},
		{"0", false, false},
		{"1", true, false},
		{"true", false, true},
	} {
		got, err := decodeSearchSwitch(tc.raw, "regex")
		if tc.fails {
			if err == nil || !strings.Contains(err.Error(), "regex must be 0 or 1") {
				t.Errorf("%q: err = %v", tc.raw, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %v, %v", tc.raw, got, err)
		}
	}
}

func TestSplitSearchPaths(t *testing.T) {
	t.Parallel()
	got := splitSearchPaths([]string{"cmd/\n\n  *.go \n", "", "pkg"})
	want := []string{"cmd/", "*.go", "pkg"}
	if len(got) != len(want) {
		t.Fatalf("splitSearchPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitSearchPaths = %v, want %v", got, want)
		}
	}
}

func TestNormalizeSearchOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{" acme ", "acme"},
		{"@acme", "acme"},
		{"", ""},
	} {
		if got := normalizeSearchOwner(tc.in); got != tc.want {
			t.Errorf("normalizeSearchOwner(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLocateMatchSpan(t *testing.T) {
	t.Parallel()
	opts := search.QueryOptions{}
	t.Run("column hint", func(t *testing.T) {
		before, mark, after, ok := locateMatchSpan("func needle() {", 6, "needle", opts)
		if !ok || before != "func " || mark != "needle" || after != "() {" {
			t.Fatalf("got %q %q %q %v", before, mark, after, ok)
		}
	})
	t.Run("ellipsized text falls back to the first occurrence", func(t *testing.T) {
		// The column names the original line; the text was middle-ellipsized.
		before, mark, after, ok := locateMatchSpan("… needle …", 900, "needle", opts)
		if !ok || mark != "needle" || before != "… " || after != " …" {
			t.Fatalf("got %q %q %q %v", before, mark, after, ok)
		}
	})
	t.Run("case insensitive", func(t *testing.T) {
		before, mark, _, ok := locateMatchSpan("func Needle() {", 6, "needle", opts)
		if !ok || mark != "Needle" || before != "func " {
			t.Fatalf("got %q %q %v", before, mark, ok)
		}
	})
	t.Run("case sensitive misses", func(t *testing.T) {
		if _, _, _, ok := locateMatchSpan("func Needle() {", 6, "needle", search.QueryOptions{CaseSensitive: true}); ok {
			t.Fatal("case-sensitive locate matched a different case")
		}
	})
	t.Run("regex", func(t *testing.T) {
		_, mark, _, ok := locateMatchSpan("id = 42;", 1, `[0-9]+`, search.QueryOptions{Regex: true})
		if !ok || mark != "42" {
			t.Fatalf("regex mark = %q ok=%v", mark, ok)
		}
	})
	t.Run("regex with no match is unmarked", func(t *testing.T) {
		if _, _, _, ok := locateMatchSpan("nothing here", 1, `[0-9]+`, search.QueryOptions{Regex: true}); ok {
			t.Fatal("a non-matching regex marked a span")
		}
	})
	t.Run("invalid regex is unmarked", func(t *testing.T) {
		if _, _, _, ok := locateMatchSpan("text", 1, "[", search.QueryOptions{Regex: true}); ok {
			t.Fatal("an invalid regex marked a span")
		}
	})
	t.Run("case sensitive fallback", func(t *testing.T) {
		before, mark, after, ok := locateMatchSpan("xx needle yy", 1, "needle", search.QueryOptions{CaseSensitive: true})
		if !ok || mark != "needle" || before != "xx " || after != " yy" {
			t.Fatalf("got %q %q %q %v", before, mark, after, ok)
		}
	})
	t.Run("regex case-insensitive", func(t *testing.T) {
		_, mark, _, ok := locateMatchSpan("X NEEDLE x", 1, "needle", search.QueryOptions{Regex: true})
		if !ok || !strings.EqualFold(mark, "needle") {
			t.Fatalf("regex mark = %q ok=%v", mark, ok)
		}
	})
	t.Run("empty inputs render unmarked", func(t *testing.T) {
		for _, tc := range []struct {
			text    string
			pattern string
		}{
			{"", "needle"},
			{"needle", ""},
			{"nothing", "needle"},
			{"needle", "["},
		} {
			if _, _, _, ok := locateMatchSpan(tc.text, 1, tc.pattern, opts); ok {
				t.Errorf("%q/%q located a span", tc.text, tc.pattern)
			}
		}
	})
}

func TestIssueStateChip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ state, want string }{
		{"open", "chip chip-open"},
		{"closed", "chip chip-closed"},
		{"merged", "chip chip-merged"},
		{"draft", "chip chip-draft"},
		{"weird", "chip chip-neutral"},
	} {
		if got := issueStateChip(tc.state); got != tc.want {
			t.Errorf("issueStateChip(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestCodeStatusLine(t *testing.T) {
	t.Parallel()
	repoScope := &searchCodeVM{
		Complete: true,
		Searched: 1, Considered: 1,
		Repos: []searchRepoVM{{
			Repo: "a/b", Status: "ok", Ref: "main", Short: "abcdef1234",
			MatchCount: 3, FileCount: 2,
		}},
	}
	if got := codeStatusLine(repoScope, false); !strings.Contains(got, "3 matches in 2 files") || !strings.Contains(got, "searched main (abcdef1234)") {
		t.Errorf("repo scope line = %q", got)
	}
	ownerScope := &searchCodeVM{
		Complete: false, Searched: 1, Considered: 3,
		Repos: []searchRepoVM{
			{Repo: "a/b", Status: "ok", Truncated: true, MatchCount: 200},
			{Repo: "a/c", Status: "not_warm"},
			{Repo: "a/d", Status: "not_searched"},
		},
	}
	got := codeStatusLine(ownerScope, true)
	for _, want := range []string{"200 matches", "searched 1 of 3 repositories", "showing the first 200 matches"} {
		if !strings.Contains(got, want) {
			t.Errorf("owner scope line %q missing %q", got, want)
		}
	}
	if got := codeStatusLine(&searchCodeVM{}, true); !strings.Contains(got, "Nothing was searched") {
		t.Errorf("empty section line = %q", got)
	}
}
