package webui

// parity_repo_test.go — the repo-group parity affordances: the header's
// star/watch toggles (GET shape + POST success/gates/cross-origin), the
// commit page's Create tag form (write gate, validation re-render, conflict,
// redirect), and the commits list's bounded per-row check pills.
//
// Every write is driven through s.mux exactly as the server does, so the
// assertions cover routing, the shared preflight, and the service seam.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/social"
	"github.com/crueber/spindle/internal/tags"
)

// --- helpers -----------------------------------------------------------------

// parityServer builds a repo fixture with an explicit auth mode + principal and
// installs the parity seams (reset on cleanup — RepoDeps is process-wide).
func parityServer(t *testing.T, mode string, p auth.Principal, deps RepoDeps) *repoFixture {
	t.Helper()
	f := newRepoFixture(t, true)
	cfg := &config.Config{}
	cfg.Server.Auth.AnonymousRead = true
	cfg.Server.Auth.Mode = mode
	cfg.Server.PublicURL = "http://example.test"
	f.principal = p
	f.newServer(cfg)
	BindRepo(deps)
	t.Cleanup(func() { BindRepo(RepoDeps{}) })
	return f
}

// parityDo serves one request through the page mux.
func parityDo(t *testing.T, s *Server, method, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func parityPost(t *testing.T, f *repoFixture, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return parityDo(t, f.s, http.MethodPost, target, form, hdr)
}

func parityWantContains(t *testing.T, rec *httptest.ResponseRecorder, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q\nbody: %.600s", want, rec.Body.String())
		}
	}
}

func parityWantLocation(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body %.240q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// --- star / watch GET shape ---------------------------------------------------

func TestRepoHeaderStarWatchToggles(t *testing.T) {
	feats := config.AllFeatures()
	counts := social.SocialDoc{Stars: 7, Watchers: 3}
	starred := true
	deps := RepoDeps{
		SocialCounts: func(context.Context, auth.Principal, string, string) (social.SocialDoc, error) {
			return counts, nil
		},
		SocialViewer: func(context.Context, auth.Principal, string, string) (bool, bool) {
			return starred, false
		},
		Star:   func(context.Context, auth.Principal, string, string) (int, error) { return 8, nil },
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) { return 6, nil },
		WatchSet: func(_ context.Context, _ string, _, _ string, on bool) (notify.WatchState, error) {
			return notify.WatchState{Watching: on, Watchers: 4}, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	f.view.summary.Features = &feats

	body := f.mustGet("/demo/spindle", http.StatusOK)
	for _, want := range []string{
		// Counts left of the label (monospace), one pill idiom.
		`aria-pressed="true"`,
		`<span class="mono">7</span> Star`,
		`<span class="mono">3</span> Watch`,
		// A starred viewer sees Unstar; a non-watcher sees Watch.
		`action="/demo/spindle/unstar"`,
		`action="/demo/spindle/watch"`,
		// The redirect returns to the exact view.
		`name="next" value="/demo/spindle"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header missing %q", want)
		}
	}
	// A starred viewer sees the active (amber) state, never the page's one
	// primary CTA (Precision Dark §1 F3: amber means action, not state).
	if !strings.Contains(body, "btn-active") {
		t.Error("starred toggle lost its active state")
	}

	// Feature-disabled + not engaged: a disabled affordance with the reason,
	// never a form that could only 403 (#522).
	starred = false
	f.view.summary.Features = &config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true}
	body = f.mustGet("/demo/spindle", http.StatusOK)
	for _, want := range []string{
		"Starring is disabled for this repository",
		"Watching is disabled for this repository",
		`aria-disabled="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("disabled header missing %q", want)
		}
	}
	if strings.Contains(body, `action="/demo/spindle/star"`) || strings.Contains(body, `action="/demo/spindle/watch"`) {
		t.Error("disabled features still rendered writable forms")
	}
}

func TestRepoHeaderStarRetainedWhileDisabled(t *testing.T) {
	// A retained star/watch stays actionable while the feature is off: the
	// state must always be removable (#522).
	deps := RepoDeps{
		SocialCounts: func(context.Context, auth.Principal, string, string) (social.SocialDoc, error) {
			return social.SocialDoc{Stars: 2, Watchers: 5}, nil
		},
		SocialViewer: func(context.Context, auth.Principal, string, string) (bool, bool) { return true, true },
		Star:         func(context.Context, auth.Principal, string, string) (int, error) { return 3, nil },
		Unstar:       func(context.Context, auth.Principal, string, string) (int, error) { return 1, nil },
		WatchSet: func(context.Context, string, string, string, bool) (notify.WatchState, error) {
			return notify.WatchState{}, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true}
	f.view.summary.Features = &off

	body := f.mustGet("/demo/spindle", http.StatusOK)
	for _, want := range []string{`action="/demo/spindle/unstar"`, `action="/demo/spindle/unwatch"`} {
		if !strings.Contains(body, want) {
			t.Errorf("retained state lost its action %q", want)
		}
	}
	if strings.Contains(body, "is disabled for this repository") {
		t.Error("a retained state rendered the disabled explanation")
	}
}

func TestRepoHeaderStarCountsOnlyWhenTogglesUnwired(t *testing.T) {
	// Only the counters are wired: the header shows a count, not a dead button.
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		SocialCounts: func(context.Context, auth.Principal, string, string) (social.SocialDoc, error) {
			return social.SocialDoc{Stars: 4, Watchers: 1}, nil
		},
	})
	body := f.mustGet("/demo/spindle", http.StatusOK)
	if !strings.Contains(body, `<span class="mono">4</span> Star`) || !strings.Contains(body, `<span class="mono">1</span> Watch`) {
		t.Fatalf("counts missing: %.400s", body)
	}
	for _, unwanted := range []string{`action="/demo/spindle/star"`, `action="/demo/spindle/watch"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("unwired seam rendered %q", unwanted)
		}
	}
}

func TestRepoHeaderSocialReadFailureDegrades(t *testing.T) {
	// A store failure hides the pill (display metadata never fails the page).
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		SocialCounts: func(context.Context, auth.Principal, string, string) (social.SocialDoc, error) {
			return social.SocialDoc{}, errors.New("store down")
		},
		Star:   func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
	})
	body := f.mustGet("/demo/spindle", http.StatusOK)
	if strings.Contains(body, `action="/demo/spindle/star"`) || strings.Contains(body, "Star</span>") {
		t.Errorf("failed social read still rendered a toggle: %.400s", body)
	}
	if !strings.Contains(body, `title="Fork demo/spindle"`) {
		t.Error("the rest of the header did not render")
	}
}

// --- star / watch POSTs -------------------------------------------------------

func TestRepoStarTogglePost(t *testing.T) {
	var gotOn []bool
	var gotP auth.Principal
	deps := RepoDeps{
		Star: func(_ context.Context, p auth.Principal, owner, repo string) (int, error) {
			gotOn = append(gotOn, true)
			gotP = p
			if owner != "demo" || repo != "spindle" {
				t.Errorf("star called with %s/%s", owner, repo)
			}
			return 8, nil
		},
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) {
			gotOn = append(gotOn, false)
			return 6, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)

	rec := parityPost(t, f, "/demo/spindle/star", url.Values{"next": {"/demo/spindle/commits?skip=70"}}, nil)
	parityWantLocation(t, rec, "/demo/spindle/commits?skip=70")

	rec = parityPost(t, f, "/demo/spindle/unstar", url.Values{"next": {"/demo/spindle"}}, nil)
	parityWantLocation(t, rec, "/demo/spindle")

	if len(gotOn) != 2 || !gotOn[0] || gotOn[1] {
		t.Fatalf("toggles = %v, want [true false]", gotOn)
	}
	if gotP.Name != "alice" || gotP.Anonymous {
		t.Fatalf("principal = %+v", gotP)
	}

	// A hostile/absent next falls back to the repo page (no open redirect).
	for _, next := range []string{"//evil.example/x", "https://evil.example/", ""} {
		rec = parityPost(t, f, "/demo/spindle/star", url.Values{"next": {next}}, nil)
		parityWantLocation(t, rec, "/demo/spindle")
	}

	// htmx posts get HX-Redirect (never a partial swap of a whole page).
	rec = parityPost(t, f, "/demo/spindle/star", url.Values{"next": {"/demo/spindle"}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/demo/spindle" {
		t.Fatalf("htmx redirect = %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

func TestRepoStarToggleGates(t *testing.T) {
	deps := RepoDeps{
		Star:   func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)

	// Cross-origin: refused before anything else runs.
	rec := parityPost(t, f, "/demo/spindle/star", nil, map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("cross-origin = %d %.160q", rec.Code, rec.Body.String())
	}

	// An anonymous viewer under a real auth mode goes to the interstitial.
	f = parityServer(t, "oidc", auth.Anonymous(), deps)
	rec = parityPost(t, f, "/demo/spindle/star", url.Values{"next": {"/demo/spindle"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous star = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login-required?") ||
		!strings.Contains(loc, "next=%2Fdemo%2Fspindle") || !strings.Contains(loc, "action=Star+this+repository") {
		t.Fatalf("anonymous Location = %q", loc)
	}

	// Anonymous with no auth mode: a real 401 (nothing to route to).
	f = parityServer(t, "", auth.Anonymous(), deps)
	rec = parityPost(t, f, "/demo/spindle/star", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous/none star = %d, want 401", rec.Code)
	}

	// Feature-disabled: the service's 403 text reaches the page.
	f = parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		Star: func(context.Context, auth.Principal, string, string) (int, error) {
			return 0, errors.Join(social.ErrForbidden, errors.New("starring is disabled for this repository"))
		},
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
	})
	rec = parityPost(t, f, "/demo/spindle/star", nil, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "starring is disabled") {
		t.Fatalf("feature-disabled star = %d %.200q", rec.Code, rec.Body.String())
	}

	// An unwired seam: 503, never a panic.
	f = parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{})
	rec = parityPost(t, f, "/demo/spindle/star", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired star = %d, want 503", rec.Code)
	}
}

func TestRepoWatchTogglePost(t *testing.T) {
	var states []bool
	var principals []string
	deps := RepoDeps{
		WatchSet: func(_ context.Context, principal, owner, repo string, on bool) (notify.WatchState, error) {
			states = append(states, on)
			principals = append(principals, principal)
			return notify.WatchState{Watching: on, Watchers: 2}, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)

	parityWantLocation(t, parityPost(t, f, "/demo/spindle/watch", url.Values{"next": {"/demo/spindle"}}, nil), "/demo/spindle")
	parityWantLocation(t, parityPost(t, f, "/demo/spindle/unwatch", nil, nil), "/demo/spindle")
	if len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("watch toggles = %v", states)
	}
	if principals[0] != "alice" {
		t.Fatalf("principal name = %q", principals[0])
	}

	// Cross-origin refused.
	rec := parityPost(t, f, "/demo/spindle/watch", nil, map[string]string{"Referer": "http://evil.example/x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin watch = %d", rec.Code)
	}

	// A deleted repo from the service maps to 404.
	f = parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		WatchSet: func(context.Context, string, string, string, bool) (notify.WatchState, error) {
			return notify.WatchState{}, errors.Join(notify.ErrNotFound, errors.New("repo not found"))
		},
	})
	rec = parityPost(t, f, "/demo/spindle/watch", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing-repo watch = %d, want 404", rec.Code)
	}

	// An anonymous viewer is sent to the interstitial, not an in-band notice.
	f = parityServer(t, "oidc", auth.Anonymous(), deps)
	rec = parityPost(t, f, "/demo/spindle/unwatch", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "action=Unwatch+this+repository") {
		t.Fatalf("anonymous unwatch = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// --- create tag --------------------------------------------------------------

func tagFixture(t *testing.T) (*repoFixture, *[]tags.CreateInput) {
	t.Helper()
	sha := strings.Repeat("a", 40)
	calls := &[]tags.CreateInput{}
	deps := RepoDeps{
		Tags: func(_ context.Context, owner, repo string, _ auth.Principal, in tags.CreateInput) (*tags.Tag, error) {
			if owner != "demo" || repo != "spindle" {
				t.Errorf("CreateTag called with %s/%s", owner, repo)
			}
			*calls = append(*calls, in)
			return &tags.Tag{Name: in.Name, SHA: in.SHA, Ref: "refs/tags/" + in.Name}, nil
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleWrite },
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	f.view.detail = api.CommitDetail{
		Commit: api.Commit{SHA: sha, Subject: "add the thing", Body: "body"},
		Stats:  []api.Stat{{Path: "a.txt", Additions: 1, Deletions: 0}},
		Patch:  "diff --git a/a.txt b/a.txt\nnew file mode 100644\n--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1 @@\n+x\n",
	}
	return f, calls
}

func TestRepoCommitCreateTagFormAndRedirect(t *testing.T) {
	f, calls := tagFixture(t)
	sha := strings.Repeat("a", 40)

	body := f.mustGet("/demo/spindle/commit/"+sha, http.StatusOK)
	for _, want := range []string{
		"Create tag",
		`action="/demo/spindle/tag"`,
		`name="sha" value="` + sha + `"`,
		`name="name"`,
		`name="message"`,
		"Annotation (optional)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commit page missing %q", want)
		}
	}

	rec := parityPost(t, f, "/demo/spindle/tag", url.Values{
		"name": {"v1.0.0"}, "message": {"the release"}, "sha": {sha},
	}, nil)
	parityWantLocation(t, rec, "/demo/spindle/commits?ref=v1.0.0&flash=Tag+v1.0.0+created")

	if len(*calls) != 1 {
		t.Fatalf("CreateTag calls = %d", len(*calls))
	}
	got := (*calls)[0]
	if got.Name != "v1.0.0" || got.Message != "the release" || got.SHA != sha {
		t.Fatalf("CreateTag input = %+v", got)
	}

	// A lightweight tag (empty annotation) is the same route, message empty.
	rec = parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {" v2 "}, "sha": {sha}}, nil)
	parityWantLocation(t, rec, "/demo/spindle/commits?ref=v2&flash=Tag+v2+created")
	if (*calls)[1].Name != "v2" || (*calls)[1].Message != "" {
		t.Fatalf("lightweight input = %+v", (*calls)[1])
	}
}

func TestRepoCommitCreateTagGates(t *testing.T) {
	sha := strings.Repeat("a", 40)

	// Read-only viewer: the form never renders and a hand-rolled POST 403s.
	f, _ := tagFixture(t)
	f.principal = auth.Principal{Name: "bob", Write: true}
	f.rebind()
	BindRepo(RepoDeps{
		Tags: func(context.Context, string, string, auth.Principal, tags.CreateInput) (*tags.Tag, error) {
			return nil, nil
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead },
	})
	body := f.mustGet("/demo/spindle/commit/"+sha, http.StatusOK)
	if strings.Contains(body, `action="/demo/spindle/tag"`) {
		t.Error("read-only viewer saw the Create tag form")
	}
	rec := parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {sha}}, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "write access required") {
		t.Fatalf("read-only create = %d %.200q", rec.Code, rec.Body.String())
	}

	// Cross-origin.
	f, _ = tagFixture(t)
	rec = parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {sha}},
		map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin create = %d", rec.Code)
	}

	// Anonymous → interstitial.
	fAnon, _ := tagFixture(t)
	fAnon = parityServer(t, "oidc", auth.Anonymous(), RepoDeps{
		Tags: func(context.Context, string, string, auth.Principal, tags.CreateInput) (*tags.Tag, error) {
			return nil, nil
		},
	})
	rec = parityPost(t, fAnon, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {sha}}, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "action=Create+a+tag") {
		t.Fatalf("anonymous create = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Unwired seam.
	f = parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{})
	rec = parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {sha}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired create = %d, want 503", rec.Code)
	}
}

func TestRepoCommitCreateTagValidation(t *testing.T) {
	f, calls := tagFixture(t)
	sha := strings.Repeat("a", 40)

	// Empty name re-renders the commit page with the SPA's message + values.
	rec := parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"  "}, "message": {"note"}, "sha": {sha}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d, want 400", rec.Code)
	}
	parityWantContains(t, rec,
		"Give the tag a name.",
		`role="alert"`,
		`value="note"`, // the annotation survives the re-render
		"<details open>",
	)
	if len(*calls) != 0 {
		t.Fatalf("CreateTag called for an invalid form (%d calls)", len(*calls))
	}

	// A service rejection (duplicate tag) re-renders with its text + status.
	BindRepo(RepoDeps{
		Tags: func(context.Context, string, string, auth.Principal, tags.CreateInput) (*tags.Tag, error) {
			return nil, errors.Join(tags.ErrConflict, errors.New(`tag "v1" already exists`))
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleWrite },
	})
	rec = parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {sha}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate tag = %d, want 409", rec.Code)
	}
	parityWantContains(t, rec, `tag &#34;v1&#34; already exists`, `value="v1"`)
}

func TestRepoCommitCreateTagEmptyRepo(t *testing.T) {
	// The guide page has no form; a POST there is not a tag creation.
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		Tags: func(context.Context, string, string, auth.Principal, tags.CreateInput) (*tags.Tag, error) {
			return nil, nil
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleWrite },
	})
	f.view.summary.Head = nil
	f.view.summary.Branches, f.view.summary.Tags = 0, 0
	f.view.summary.Health = api.RepoHealthEmpty

	rec := parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {strings.Repeat("a", 40)}}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty-repo create = %d, want 404", rec.Code)
	}
}

// --- per-row check pills ------------------------------------------------------

func TestRepoCommitsCheckPills(t *testing.T) {
	sha := strings.Repeat("a", 40)
	older := strings.Repeat("9", 40)
	newest := strings.Repeat("7", 40)
	commits := []api.Commit{
		{SHA: newest, Subject: "newest"},
		{SHA: sha, Subject: "middle"},
		{SHA: older, Subject: "oldest"},
	}
	var gotN int
	deps := RepoDeps{
		ChecksIndex: func(_ context.Context, owner, repo string, p auth.Principal, after string, n int) (*checks.ChecksPage, error) {
			gotN = n
			if owner != "demo" || repo != "spindle" || after != "" {
				t.Errorf("ChecksIndex(%s/%s, after=%q)", owner, repo, after)
			}
			return &checks.ChecksPage{Checks: []checks.IndexSHA{
				{SHA: newest, State: checks.StateSuccess},
				{SHA: sha, State: checks.StateFailure},
			}}, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: newest, Commits: commits}

	body := f.mustGet("/demo/spindle/commits", http.StatusOK)
	if gotN != commitsChecksBatch || gotN > 200 {
		t.Fatalf("ChecksIndex n = %d, want the bounded %d", gotN, commitsChecksBatch)
	}
	for _, want := range []string{
		`href="/demo/spindle/checks/` + newest + `"`,
		"checks: success",
		`href="/demo/spindle/checks/` + sha + `"`,
		"checks: failure",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commits page missing %q", want)
		}
	}
	// The sha outside the index window renders no pill rather than a guess.
	if strings.Contains(body, `/checks/`+older) {
		t.Error("an unknown sha rendered a guessed check state")
	}
}

func TestRepoCommitsCheckPillsEmptyIndex(t *testing.T) {
	sha := strings.Repeat("a", 40)
	older := strings.Repeat("9", 40)
	deps := RepoDeps{
		ChecksIndex: func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
			return &checks.ChecksPage{}, nil
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: []api.Commit{
		{SHA: sha, Subject: "single parent", Parents: []string{older}},
		{SHA: older, Subject: "merge", Parents: []string{sha, strings.Repeat("8", 40)}},
	}}

	body := f.mustGet("/demo/spindle/commits", http.StatusOK)
	if !strings.Contains(body, "checks: no contexts reported") {
		t.Errorf("zero-check repo lost the neutral pill: %.400s", body)
	}
	// The parent annotations are unchanged by the extra column.
	for _, want := range []string{"parent ", "merge parents (2)"} {
		if !strings.Contains(body, want) {
			t.Errorf("commits list lost %q", want)
		}
	}
}

func TestRepoCommitsCheckPillsFailureDegrades(t *testing.T) {
	sha := strings.Repeat("a", 40)
	deps := RepoDeps{
		ChecksIndex: func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
			return nil, errors.New("index unreadable")
		},
	}
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, deps)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: commitsFixture(sha)}

	body := f.mustGet("/demo/spindle/commits", http.StatusOK)
	if !strings.Contains(body, "fix the thing") {
		t.Error("a checks-index failure lost the commits list")
	}
	if strings.Contains(body, "/checks/") {
		t.Error("a checks-index failure rendered a pill")
	}
}

func TestRepoTogglesRepoGate(t *testing.T) {
	// An invisible repo is a 404 before any toggle logic runs (the visibility
	// gate outranks everything else, #345).
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		Star:   func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
		Unstar: func(context.Context, auth.Principal, string, string) (int, error) { return 0, nil },
		WatchSet: func(context.Context, string, string, string, bool) (notify.WatchState, error) {
			return notify.WatchState{}, nil
		},
	})
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no read"}
	}
	f.rebind()

	for _, path := range []string{"/demo/spindle/star", "/demo/spindle/watch"} {
		rec := parityPost(t, f, path, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s on an invisible repo = %d, want 404", path, rec.Code)
		}
	}

	// The tag POST is behind the same gate.
	rec := parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tag on an invisible repo = %d, want 404", rec.Code)
	}
}

func TestRepoWatchUnwiredAndCreateTagViewFailure(t *testing.T) {
	// Watch without the write seam: 503, never a panic.
	f := parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{})
	rec := parityPost(t, f, "/demo/spindle/watch", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired watch = %d, want 503", rec.Code)
	}

	// Create tag when the commit view cannot be built: the view error's status.
	f = parityServer(t, "token", auth.Principal{Name: "alice", Write: true}, RepoDeps{
		Tags: func(context.Context, string, string, auth.Principal, tags.CreateInput) (*tags.Tag, error) {
			return nil, nil
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleWrite },
	})
	f.view.resolveErr = errors.New("git pool exhausted")
	rec = parityPost(t, f, "/demo/spindle/tag", url.Values{"name": {"v1"}, "sha": {strings.Repeat("a", 40)}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tag view failure = %d, want 503", rec.Code)
	}
}

func TestRepoCreateTagMalformedBody(t *testing.T) {
	f, calls := tagFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/demo/spindle/tag", strings.NewReader("name=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "form parse failed") {
		t.Fatalf("malformed tag body = %d %.160q", rec.Code, rec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatal("malformed form reached the service")
	}
}

// --- small helpers ------------------------------------------------------------

func TestParityErrStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusServiceUnavailable},
		{tags.ErrInvalid, http.StatusBadRequest},
		{errors.Join(tags.ErrInvalid, errors.New("bad name")), http.StatusBadRequest},
		{social.ErrInvalid, http.StatusBadRequest},
		{notify.ErrInvalid, http.StatusBadRequest},
		{tags.ErrConflict, http.StatusConflict},
		{social.ErrConflict, http.StatusConflict},
		{tags.ErrNotFound, http.StatusNotFound},
		{social.ErrNotFound, http.StatusNotFound},
		{notify.ErrNotFound, http.StatusNotFound},
		{tags.ErrUnauthorized, http.StatusUnauthorized},
		{tags.ErrForbidden, http.StatusForbidden},
		{social.ErrForbidden, http.StatusForbidden},
		{notify.ErrForbidden, http.StatusForbidden},
		{tags.ErrUnavailable, http.StatusServiceUnavailable},
		{tags.ErrUnsupported, http.StatusUnprocessableEntity},
		{identity.ErrNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		if got := parityErrStatus(tc.err); got != tc.want {
			t.Errorf("parityErrStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if parityErrMessage(nil) != "" || parityErrMessage(errors.New("boom")) != "boom" {
		t.Error("parityErrMessage changed the service text")
	}
}

func TestRepoCanWrite(t *testing.T) {
	ctx := context.Background()
	if repoCanWrite(ctx, "demo", "spindle", auth.Anonymous()) {
		t.Error("anonymous principal passed the write gate")
	}
	if !repoCanWrite(ctx, "demo", "spindle", auth.Principal{Name: "x", Admin: true}) {
		t.Error("admin failed the write gate")
	}
	// No identity seam: the principal's write flag decides (fail open on the
	// flag, exactly like the SPA with no role answer).
	BindRepo(RepoDeps{})
	t.Cleanup(func() { BindRepo(RepoDeps{}) })
	if !repoCanWrite(ctx, "demo", "spindle", auth.Principal{Name: "x", Write: true}) {
		t.Error("write principal failed without a role seam")
	}
	if repoCanWrite(ctx, "demo", "spindle", auth.Principal{Name: "x"}) {
		t.Error("read principal passed without a role seam")
	}
	BindRepo(RepoDeps{RoleOf: func(context.Context, string, string, auth.Principal) identity.Role {
		return identity.RoleTriage
	}})
	if repoCanWrite(ctx, "demo", "spindle", auth.Principal{Name: "x", Write: true}) {
		t.Error("triage role passed the write gate")
	}
}
