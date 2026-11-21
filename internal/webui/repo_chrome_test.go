package webui

// repo_chrome_test.go — the repository chrome contract (12_web_ui.md §2.2.1):
// EVERY repo-scoped page renders the same masthead + tab strip above its own
// content, with the correct tab current. Before this contract only the repo
// group's five templates carried the chrome, so the header "disappeared" when
// moving from Commits to Issues/Pulls/Releases/Settings.
//
// The assertions are deliberately page-shaped (the parsed DOM), not
// implementation-shaped: they would fail if the chrome were moved, duplicated,
// or rendered from a second data source.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
)

// chromeDescription is the marker the gating tests search for.
const chromeDescription = "Widgets for the lab"

// bindChromeProjections installs the boot-time projections the masthead reads
// (counts, checks visibility, fork count) and restores the previous set.
func bindChromeProjections(t *testing.T) {
	t.Helper()
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	BindRepo(RepoDeps{
		Visibility: func(string, string) (string, bool) { return "public", true },
		CollabCounts: func(string, string) (api.CollabCounts, bool) {
			return api.CollabCounts{OpenIssues: 2, OpenPulls: 1}, true
		},
		ChecksSummary: func(string, string) (api.ChecksSummary, bool) {
			return api.ChecksSummary{HasChecks: true}, true
		},
		ForkInfo: func(string, string) (api.ForkSummary, bool) {
			return api.ForkSummary{Count: 1}, true
		},
	})
}

// busySummary is a non-empty repo with a description, a head and counts.
func busySummary() api.SummaryData {
	return api.SummaryData{
		Head:        &api.Ref{Name: "refs/heads/main", SHA: strings.Repeat("a", 40)},
		Branches:    3,
		Tags:        1,
		Health:      api.RepoHealthHealthy,
		Description: chromeDescription,
		Features:    ptrFeatures(config.AllFeatures()),
	}
}

// wantChrome asserts the page rendered exactly ONE masthead + tab strip, that
// it is the real one (description + branch count), and which tab is current.
func wantChrome(t *testing.T, body, full, tab string) {
	t.Helper()
	if got := len(byClass(t, body, "repo-header")); got != 1 {
		t.Fatalf("repository masthead count = %d, want 1\nbody: %.600s", got, body)
	}
	if !strings.Contains(body, chromeDescription) {
		t.Errorf("masthead is missing the repository description")
	}
	if !strings.Contains(body, "3 branches") {
		t.Errorf("masthead is missing the branch count")
	}
	tabs := tabsOf(t, body)
	if len(tabs) == 0 {
		t.Fatalf("no tab strip rendered\nbody: %.600s", body)
	}
	if got := activeTabOf(t, body); got != tab {
		t.Errorf("active tab = %q, want %q (tabs: %v)", got, tab, tabIDs(t, body))
	}
	// Every tab link is scoped to THIS repository.
	for _, n := range tabs {
		href := attr(n, "href")
		if !strings.HasPrefix(href, "/"+full) {
			t.Errorf("tab %q href = %q, want a /%s path", attr(n, "data-tab"), href, full)
		}
	}
}

// TestRepoChromeOnCollabPages is the table the defect named: every
// collaboration page is repository-scoped, so every one carries the chrome.
func TestRepoChromeOnCollabPages(t *testing.T) {
	sha := strings.Repeat("d", 40)
	cases := []struct {
		name string
		path string
		tab  string
		deps CollabDeps
	}{
		{"issues", "/acme/widgets/issues", "issues", issueDeps()},
		{"issue", "/acme/widgets/issues/3", "issues", issueDeps()},
		{"pulls", "/acme/widgets/pulls", "pulls", pullDeps()},
		{"pull", "/acme/widgets/pull/5", "pulls", pullDeps()},
		{"pull_commits", "/acme/widgets/pull/5/commits", "pulls", pullDeps()},
		{"pull_files", "/acme/widgets/pull/5/files", "pulls", pullDeps()},
		{"checks", "/acme/widgets/checks", "checks", checkDeps()},
		{"check_detail", "/acme/widgets/checks/" + sha, "checks", checkDeps()},
		{"releases", "/acme/widgets/releases", "releases", releaseDeps()},
		{"release", "/acme/widgets/releases/v1.2.3", "releases", releaseDeps()},
		// Forks is reached from the masthead's fork counter (not a tab), so
		// the strip keeps its fail-open default: Code.
		{"forks", "/acme/widgets/forks", "code", pullDeps()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bindChromeProjections(t)
			f := newCollabFixture(t)
			f.view.summary = busySummary()
			f.deps(tc.deps)
			rec := f.get(tc.path)
			wantStatus(t, rec, http.StatusOK)
			wantChrome(t, bodyText(rec), "acme/widgets", tc.tab)
			// Open-issue/PR badges ride the same projections.
			if tc.tab == "issues" && !strings.Contains(bodyText(rec), `aria-label="2 open"`) {
				t.Error("Issues tab lost its open-count badge")
			}
		})
	}
}

// TestRepoChromeOnSettingsPages: the settings shell is repo-scoped too.
func TestRepoChromeOnSettingsPages(t *testing.T) {
	for _, tab := range []string{"general", "access"} {
		t.Run(tab, func(t *testing.T) {
			bindChromeProjections(t)
			f := settingsRepoFakes("admin")
			s := newSettingsServer(t, f)
			s.env.API = &api.Env{
				Repo:  &collabFakeView{summary: busySummary(), known: map[string]bool{"acme/monorepo": true}},
				Repos: &collabFakeRegistry{exists: map[string]bool{"acme/monorepo": true}},
			}
			rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET settings/%s = %d: %.400s", tab, rec.Code, rec.Body.String())
			}
			wantChrome(t, rec.Body.String(), "acme/monorepo", "settings")
			// The settings shell keeps its own heading/sidebar below the chrome.
			settingsMustContain(t, rec.Body.String(), `id="settings-panel"`, "panel")
			settingsMustContain(t, rec.Body.String(), `aria-label="Settings sections"`, "sidebar")
		})
	}
}

// TestRepoChromeOnRepoFormPages: the repo-scoped creation/tools forms (reached
// from the tab strip and the masthead) keep the chrome as well.
func TestRepoChromeOnRepoFormPages(t *testing.T) {
	cases := []struct {
		name string
		path string
		tab  string
	}{
		{"fork", "/acme/widgets/fork", "code"},
		{"issue_new", "/acme/widgets/issues/new", "issues"},
		{"pull_new", "/acme/widgets/pulls/new", "pulls"},
		{"release_new", "/acme/widgets/releases/new", "releases"},
		{"labels", "/acme/widgets/labels", "issues"},
		{"milestones", "/acme/widgets/milestones", "issues"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bindChromeProjections(t)
			BindForms(FormsDeps{Issues: &formsFakeIssues{
				labels:     []issues.Label{{Name: "bug", Color: "d73a4a"}},
				milestones: []*issues.Milestone{{ID: "000001", Title: "v1", State: "open"}},
			}})
			t.Cleanup(func() { BindForms(FormsDeps{}) })
			h := newFormsHarness(t, "none")
			h.s.env.API = &api.Env{
				Repo:  &collabFakeView{summary: busySummary(), known: map[string]bool{"acme/widgets": true}},
				Repos: &collabFakeRegistry{exists: map[string]bool{"acme/widgets": true}},
			}
			rec := h.request(t, http.MethodGet, tc.path, nil, "writer", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %.400s", tc.path, rec.Code, rec.Body.String())
			}
			wantChrome(t, rec.Body.String(), "acme/widgets", tc.tab)
		})
	}
}

// TestRepoChromeCodePageKeepsItsOwn: the repo group's templates render the
// chrome INSIDE their content and leave PageData.Repo nil, so the layout must
// not add a second masthead.
func TestRepoChromeCodePageKeepsItsOwn(t *testing.T) {
	bindChromeProjections(t)
	f := newRepoFixture(t, true)
	f.view.summary = busySummary()
	f.view.tree = api.TreeResult{Ref: "refs/heads/main"}
	for _, path := range []string{"/demo/spindle", "/demo/spindle/commits"} {
		body := f.mustGet(path, http.StatusOK)
		if got := len(byClass(t, body, "repo-header")); got != 1 {
			t.Errorf("%s: masthead count = %d, want exactly 1", path, got)
		}
		// code, commits, tags, issues, pulls, checks (busySummary), threads,
		// releases, settings — the union of the tabs the feature tracks added.
		if got := len(tabsOf(t, body)); got != 9 {
			t.Errorf("%s: tab count = %d, want 9", path, got)
		}
	}
	if got := activeTabOf(t, f.mustGet("/demo/spindle/commits", http.StatusOK)); got != "commits" {
		t.Errorf("commits active tab = %q", got)
	}
}

// TestRepoChromeForDegrades: the post-gate chrome read never writes and never
// errors — it answers nil, and the page renders without chrome.
func TestRepoChromeForDegrades(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/acme/widgets/issues", nil)
	// No RepoView wired at all.
	if c := (&Server{env: Env{}}).repoChromeFor(req, auth.Anonymous(), "acme", "widgets"); c != nil {
		t.Fatalf("chrome without a RepoView = %+v", c)
	}
	// Malformed owner.
	s := &Server{env: Env{API: &api.Env{Repo: &collabFakeView{summary: busySummary()}}}}
	if c := s.repoChromeFor(req, auth.Anonymous(), "..", "widgets"); c != nil {
		t.Fatalf("chrome for a malformed id = %+v", c)
	}
	if c := s.repoChromeFrom(req, auth.Anonymous(), "..", "widgets", busySummary()); c != nil {
		t.Fatalf("chrome-from for a malformed id = %+v", c)
	}
	// Summary outage.
	down := &Server{env: Env{API: &api.Env{Repo: &collabFakeView{err: errors.New("engine down")}}}}
	if c := down.repoChromeFor(req, auth.Anonymous(), "acme", "widgets"); c != nil {
		t.Fatalf("chrome during an outage = %+v", c)
	}
}

// page's own read gate passed, so a denied request keeps its original status
// and never carries the repository's name, description or tabs.
func TestRepoChromeGatedPagesLeakNothing(t *testing.T) {
	t.Run("collab anonymous private repo", func(t *testing.T) {
		f := newCollabFixture(t)
		f.view.summary = busySummary()
		f.deps(issueDeps())
		f.readGate = func(string, string, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no access"}
		}
		rec := f.get("/acme/widgets/issues")
		wantStatus(t, rec, http.StatusUnauthorized) // the group's existing gate
		wantNotContains(t, rec, chromeDescription, "repo-header", "data-tab=", "acme/widgets")
	})

	t.Run("settings denied viewer", func(t *testing.T) {
		f := settingsRepoFakes("admin")
		s := newSettingsServer(t, f)
		s.env.API = &api.Env{
			Repo:  &collabFakeView{summary: busySummary(), known: map[string]bool{"acme/monorepo": true}},
			Repos: &collabFakeRegistry{exists: map[string]bool{"acme/monorepo": true}},
		}
		s.BindAuth(pages.Auth{
			Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
				return settingsTestPrincipal(), nil
			},
			CheckRead: func(context.Context, string, string, auth.Principal) *auth.AuthError {
				return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no access"}
			},
		})
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("denied settings = %d, want 404", rec.Code)
		}
		settingsMustNotContain(t, rec.Body.String(), chromeDescription, "masthead on a denied page")
		settingsMustNotContain(t, rec.Body.String(), "repo-header", "masthead on a denied page")
	})
}
