package webui

// admin_test.go — the Administration area (docs/PLAN-ADMIN.md M1/M2): the one
// gate across all four pages, the nav rule, the router's /setup deletion, the
// shared shell's a11y floor, and the Overview's instance/health/first-run data.
//
// The harness is settings_test.go's (one page provider, both seams bound); the
// admin routes are mounted by newSettingsServer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
)

// adminTestView is the schema the area renders (the settings harness fake).
func adminTestPage(t *testing.T, deps testDeps, p auth.Principal) *Server {
	t.Helper()
	return newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
}

// adminDeny is a setupAccess-shaped rule: anonymous → 401, non-admin → 403.
func adminDeny(_ *http.Request, p auth.Principal) *auth.AuthError {
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if !p.Admin {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin required"}
	}
	return nil
}

func adminBaseDeps() testDeps {
	return testDeps{AdminDeps: AdminDeps{
		Access:     adminDeny,
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Health: func() AdminHealthView {
			return AdminHealthView{State: "ready", Summary: "The instance answers and is accepting work."}
		},
	}}
}

// TestAdminPagesGateMatrix is the one-gate matrix: the restricted boot (the
// only open state), a signed-in non-admin, a signed-in host admin, and an
// anonymous caller under real auth. The status is the API's (§3.4): 401
// unauthenticated, 403 non-admin, and the denied page is the designed view —
// never a bare code with no recovery copy.
func TestAdminPagesGateMatrix(t *testing.T) {
	pages := []string{"/admin", "/admin/config", "/admin/access", "/admin/maintenance"}
	openAccess := func(*http.Request, auth.Principal) *auth.AuthError { return nil }
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		access func(*http.Request, auth.Principal) *auth.AuthError
		want   int
		denied bool
	}{
		{"restricted boot anonymous", auth.Anonymous(), openAccess, http.StatusOK, false},
		{"restricted boot signed in", auth.Principal{Name: "alice", Write: true}, openAccess, http.StatusOK, false},
		{"real auth anonymous", auth.Anonymous(), adminDeny, http.StatusUnauthorized, true},
		{"real auth non-admin", auth.Principal{Name: "bob", Write: true}, adminDeny, http.StatusForbidden, true},
		{"real auth host admin", auth.Principal{Name: "alice", Write: true, Admin: true}, adminDeny, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := adminBaseDeps()
			deps.Access = tc.access
			s := adminTestPage(t, deps, tc.p)
			for _, page := range pages {
				rec := settingsDo(t, s, http.MethodGet, page, nil, nil)
				if rec.Code != tc.want {
					t.Fatalf("GET %s (%s) = %d, want %d", page, tc.name, rec.Code, tc.want)
				}
				body := rec.Body.String()
				if tc.denied {
					settingsMustContain(t, body, "Access restricted", page+" denied view")
					settingsMustNotContain(t, body, `name="cfg.server.listen"`, page+" must not leak the config form")
					settingsMustNotContain(t, body, "Save</button>", page+" must not leak a Save control")
				} else {
					settingsMustNotContain(t, body, "Access restricted", page+" rendered denied")
				}
			}
		})
	}
}

// TestSetupRouteIsGone pins the pre-1.0 deletion: no alias, no redirect, so
// both verbs are the plain 404.
func TestSetupRouteIsGone(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := settingsDo(t, s, method, "/setup", nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s /setup = %d, want 404 (deleted, never aliased)", method, rec.Code)
		}
	}
	if rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /setup/auth-test = %d, want 404", rec.Code)
	}
}

// TestAdminNavMarksCurrentPage: every page renders the four area links, and
// exactly the active one carries aria-current="page".
func TestAdminNavMarksCurrentPage(t *testing.T) {
	for _, tc := range []struct{ path, label string }{
		{"/admin", "Overview"},
		{"/admin/config", "Configuration"},
		{"/admin/access", "Access"},
		{"/admin/directory", "Directory"},
		{"/admin/maintenance", "Maintenance"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s := adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
			body := settingsDo(t, s, http.MethodGet, tc.path, nil, nil).Body.String()
			settingsMustContain(t, body, `aria-label="Administration sections"`, "area nav landmark")
			for _, href := range []string{"/admin", "/admin/config", "/admin/access", "/admin/directory", "/admin/maintenance"} {
				settingsMustContain(t, body, `href="`+href+`"`, "nav link "+href)
			}
			if got := strings.Count(body, `aria-current="page"`); got != 1 {
				t.Fatalf("%s: aria-current count = %d, want 1\n%s", tc.path, got, settingsTruncate(body, 3000))
			}
			settingsMustContain(t, body, `aria-current="page">`+tc.label+`</a>`, "current label")
		})
	}
}

// TestAdminShellHeadingsAndLandmarks: the a11y floor on the new pages — one h1,
// the skip link, the landmarks, and the page's purpose line.
func TestAdminShellHeadingsAndLandmarks(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
	for _, path := range []string{"/admin", "/admin/config", "/admin/access", "/admin/directory", "/admin/maintenance"} {
		body := settingsDo(t, s, http.MethodGet, path, nil, nil).Body.String()
		if n := a11yH1Count(body); n != 1 {
			t.Errorf("%s: h1 count = %d, want 1", path, n)
		}
		for _, want := range []string{
			`<a class="skip-link" href="#app">`, `<main id="app"`, `<header`, `<nav`, "<footer",
			`<p class="eyebrow">administration</p>`,
		} {
			settingsMustContain(t, body, want, path+" landmark")
		}
	}
}

// TestAdminOnePrimaryPerPage: F3's rule on the area — Configuration's Save and
// the Overview's first-run CTA are each the only primary; the read views have
// none.
func TestAdminOnePrimaryPerPage(t *testing.T) {
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "absent"
		v.FirstRun = true
		return v, nil
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/admin", 1}, // the first-run "Save configuration" CTA
		{"/admin/config", 1},
		{"/admin/access", 0},
		{"/admin/maintenance", 0},
	} {
		body := settingsDo(t, s, http.MethodGet, tc.path, nil, nil).Body.String()
		if got := strings.Count(body, `class="btn primary`); got != tc.want {
			t.Errorf("%s: primaries = %d, want %d", tc.path, got, tc.want)
		}
	}
}

// TestAdminOverviewFirstRunCard: the restricted-unconfigured Overview renders
// the five-field OIDC onboarding, the copyable redirect URI, the session-secret
// suggestion and the live Test, and no checklist or config editor shortcut.
func TestAdminOverviewFirstRunCard(t *testing.T) {
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "absent"
		v.FirstRun = true
		return v, nil
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = config.Defaults()
	body := settingsDo(t, s, http.MethodGet, "/admin", nil, nil).Body.String()
	for _, want := range []string{
		`id="admin-first-run"`, "First run: configure OIDC",
		"server.auth.issuer", "server.auth.allowed_domains", "server.auth.oauth_client_id",
		"server.auth.oauth_client_secret", "server.auth.session_secret",
		"server.auth.admin_groups", "spindle-admin",
		`id="first-run-redirect-uri"`, "/_auth/callback",
		"first-run-secret-suggest", "openssl rand -hex 32",
		"Save configuration", "Test discovery + signing keys",
		"Save does not contact the IdP",
	} {
		settingsMustContain(t, body, want, "first-run OIDC card")
	}
	settingsMustNotContain(t, body, "Choose where data lives", "the old checklist is gone")
	// The card posts only the five auth keys (a partial save on the file base).
	settingsMustContain(t, body, `name="cfg.server.auth.issuer"`, "issuer field posts its key")
}

// TestAdminFirstRunCardDisappearsWhenConfigured: a normal boot never renders
// the onboarding card.
func TestAdminFirstRunCardDisappearsWhenConfigured(t *testing.T) {
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "valid"
		return v, nil
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	body := settingsDo(t, s, http.MethodGet, "/admin", nil, nil).Body.String()
	settingsMustNotContain(t, body, `id="admin-first-run"`, "configured instance shows no onboarding card")
	settingsMustContain(t, body, "valid", "configuration status chip")
}

// TestAdminOverviewInstanceHealthAndMaintainer covers the Overview's read
// cards: instance facts, the readiness projection, and the maintainer card
// only when the seam is wired.
func TestAdminOverviewInstanceHealthAndMaintainer(t *testing.T) {
	deps := adminBaseDeps()
	deps.Health = func() AdminHealthView {
		return AdminHealthView{State: "warming", Summary: "prewarm is running",
			Warnings: []string{"Authentication is off."}, PrewarmPending: 2}
	}
	deps.Maintain = func() (AdminMaintainView, bool) {
		return AdminMaintainView{Passes: 12, LastPassSeconds: 90,
			CheckpointLagEntries: 3, CheckpointAgeSeconds: 45, MissingObjects: 1, RepairObjects: 0,
			FollowRefs: 7}, true
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = config.Defaults()
	s.env.Version = "1.2.3"
	body := settingsDo(t, s, http.MethodGet, "/admin", nil, nil).Body.String()
	for _, want := range []string{
		"Instance", "1.2.3", "OIDC", "filesystem", "Health", "warming",
		"Authentication is off.", "Maintainer", "12", "follow refs",
		`href="/healthz"`, `href="/readyz"`,
	} {
		settingsMustContain(t, body, want, "overview card")
	}

	// Unwired maintainer/health: the cards degrade honestly, never 500.
	deps.Health = nil
	deps.Maintain = nil
	s = adminTestPage(t, deps, settingsTestPrincipal())
	body = settingsDo(t, s, http.MethodGet, "/admin", nil, nil).Body.String()
	settingsMustContain(t, body, "Readiness is not wired on this build.", "degraded health card")
	settingsMustNotContain(t, body, `id="admin-maintainer"`, "absent maintainer card")
}

// TestAdminRestrictedModeRendersOnEveryPage: restricted mode is explained on
// the Overview and Configuration from the same boot state, and the chips/table
// carry it in text.
func TestAdminRestrictedModeRendersOnEveryPage(t *testing.T) {
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "invalid"
		v.Errors = []AdminError{{Key: "store.backend", Message: "unknown backend \"tape\""}}
		return v, nil
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	for _, path := range []string{"/admin", "/admin/config"} {
		body := settingsDo(t, s, http.MethodGet, path, nil, nil).Body.String()
		settingsMustContain(t, body, "Restricted mode — the configuration file is invalid.", path+" banner")
		settingsMustContain(t, body, "restricted", path+" state chip")
		settingsMustContain(t, body, "store.backend", path+" boot error key")
		settingsMustContain(t, body, `href="/admin/config"`, path+" recovery link")
	}
}

// TestAdminAccessAndMaintenanceReadViews: the two read pages show the config
// facts they own and link edits into the one editor (no duplicate forms).
func TestAdminAccessAndMaintenanceReadViews(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
	s.env.Cfg = config.Defaults()

	body := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	for _, want := range []string{"Sign-in (OIDC)", "Your access", "Admission rules",
		"Machine credentials", "Discovery test", "Sessions", "spindle-admin",
		`href="/admin/config#row-server.auth.admin_groups"`} {
		settingsMustContain(t, body, want, "access read view")
	}
	// The discovery probe is the page's only form; it reuses the Configuration
	// route and carries no save/edit control.
	settingsMustContain(t, body, `action="/admin/config/auth-test"`, "discovery test reuses the existing probe route")
	settingsMustNotContain(t, body, `name="op"`, "the access page has no save control")
	settingsMustNotContain(t, body, `name="cfg.`, "the access page never mounts the editor form")

	body = settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	for _, want := range []string{"Storage &amp; cache", "filesystem", "Health endpoints",
		`href="/healthz"`, `href="/readyz"`, `href="/metrics"`, `href="/admin/config#store"`} {
		settingsMustContain(t, body, want, "maintenance read view")
	}
	// The page's only form is the restart control (PLAN-ADMIN §5 as amended);
	// no configuration editor is mounted here.
	if got := strings.Count(body, "<form"); got != 1 {
		t.Fatalf("maintenance forms = %d, want 1 (the restart control)", got)
	}
	settingsMustContain(t, body, `action="/admin/restart"`, "maintenance restart control")
	settingsMustNotContain(t, body, `name="cfg.`, "the maintenance page never mounts the editor form")
}

// TestAdminCoverageBranches exercises the area's small error and display arms
// (nil seams, unreadable probes, and the store-target spellings) so the
// package's ≥95% gate holds with the area included.
func TestAdminCoverageBranches(t *testing.T) {
	// Bindings getter (composition's wiring guard).
	BindAdmin(AdminDeps{})
	if AdminBindings().Access != nil {
		t.Fatal("AdminBindings must return the installed seam")
	}

	// storeTarget: every backend spelling + nil config.
	if got := storeTarget(nil); got != "" {
		t.Fatalf("storeTarget(nil) = %q", got)
	}
	s3 := config.Defaults()
	s3.Store.Backend = "s3"
	s3.Store.Bucket = "b"
	s3.Store.Prefix = "/p/"
	if got := storeTarget(s3); got != "b/p" {
		t.Fatalf("s3 target = %q", got)
	}
	mem := config.Defaults()
	mem.Store.Backend = "memory"
	if got := storeTarget(mem); !strings.Contains(got, "in-memory") {
		t.Fatalf("memory target = %q", got)
	}

	// Overview with no config object and an unreadable repo probe: the
	// instance card stops after version, step 4 says the count is unavailable.
	deps := adminBaseDeps()
	deps.Repos = func(context.Context) (int, error) { return 0, errAdminTestProbe }
	s := adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = nil
	body := settingsDo(t, s, http.MethodGet, "/admin", nil, nil).Body.String()
	settingsMustContain(t, body, "version", "instance card without a config")
	settingsMustNotContain(t, body, `id="admin-first-run"`, "no checklist without a config")

	s = adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = config.Defaults()
	// Access/Maintenance with no config object render their empty states.
	s = adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
	s.env.Cfg = nil
	body = settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	settingsMustContain(t, body, "No effective sign-in configuration", "empty access facts")
	body = settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "Health endpoints", "maintenance without config")

	// The config page's view error is the API's 503, never a 500.
	deps = adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) { return nil, errAdminTestProbe }
	s = adminTestPage(t, deps, settingsTestPrincipal())
	if rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("config view error = %d, want 503", rec.Code)
	}
}

var errAdminTestProbe = &settingsTestErr{"probe unavailable"}

// TestAdminDiagnosticHelpers pins the small display/diagnostic functions the
// Access and denied views read: the verdict text, the auth summary, the
// session-issued label and the one-time secret suggestion.
func TestAdminDiagnosticHelpers(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Auth.GroupsClaim = "groups"
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}

	if got := adminVerdict(auth.Anonymous(), &cfg.Server.Auth); !strings.Contains(got, "not signed in") {
		t.Fatalf("anonymous verdict = %q", got)
	}
	if got := adminVerdict(auth.Principal{Name: "a", Admin: true}, &cfg.Server.Auth); !strings.Contains(got, "admin —") {
		t.Fatalf("admin verdict = %q", got)
	}
	if got := adminVerdict(auth.Principal{Name: "a", Groups: []string{"team-x"}}, &cfg.Server.Auth); !strings.Contains(got, "team-x") || !strings.Contains(got, "spindle-admin") {
		t.Fatalf("non-admin verdict = %q", got)
	}
	if got := adminVerdict(auth.Principal{Name: "a"}, &cfg.Server.Auth); !strings.Contains(got, "(none)") {
		t.Fatalf("empty-groups verdict = %q", got)
	}
	empty := &config.Auth{}
	if got := adminVerdict(auth.Principal{Name: "a"}, empty); !strings.Contains(got, "groups") {
		t.Fatalf("empty config verdict = %q", got)
	}

	if got := authSummary(nil); got != "" {
		t.Fatalf("authSummary(nil) = %q", got)
	}
	if got := authSummary(&config.Config{}); !strings.Contains(got, "—") {
		t.Fatalf("authSummary(empty) = %q", got)
	}
	if got := authSummary(cfg); !strings.Contains(got, "spindle-admin") {
		t.Fatalf("authSummary = %q", got)
	}

	if got := randomSessionSecret(); len(got) != 64 {
		t.Fatalf("session suggestion = %q (len %d), want 64 hex chars", got, len(got))
	}

	s := &Server{}
	if got := sessionIssuedAtLabel(httptest.NewRequest("GET", "/", nil), s); got != "" {
		t.Fatalf("unwired session label = %q", got)
	}
	s.auth = pages.Auth{SessionInfo: func(*http.Request) (time.Time, bool) { return time.Time{}, false }}
	if got := sessionIssuedAtLabel(httptest.NewRequest("GET", "/", nil), s); got != "" {
		t.Fatalf("no-session label = %q", got)
	}
	s.auth = pages.Auth{SessionInfo: func(*http.Request) (time.Time, bool) {
		return time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), true
	}}
	if got := sessionIssuedAtLabel(httptest.NewRequest("GET", "/", nil), s); !strings.Contains(got, "2026-11-21") {
		t.Fatalf("session label = %q", got)
	}

	// adminFirstRunCard fills from the effective config, including the
	// already-set secret chips.
	page := &AdminPage{}
	s = &Server{env: Env{Cfg: cfg}}
	s.adminFirstRunCard(httptest.NewRequest("GET", "http://example.com/admin", nil), page)
	if page.FirstRunIssuer != "" || page.FirstRunRedirectURI != "http://example.com/_auth/callback" ||
		page.FirstRunAdminGroup != "spindle-admin" || page.FirstRunSecretSuggest == "" {
		t.Fatalf("first-run card = %+v", page)
	}
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.OAuthClientSecret = "set"
	cfg.Server.Auth.SessionSecret = strings.Repeat("x", 32)
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	page = &AdminPage{}
	s.adminFirstRunCard(httptest.NewRequest("GET", "http://example.com/admin", nil), page)
	if page.FirstRunIssuer != "https://id.example.com" || !page.FirstRunClientSecret || !page.FirstRunSessionSecret ||
		page.FirstRunAllowedDomains != "example.com" {
		t.Fatalf("filled first-run card = %+v", page)
	}

	// The configured receiver renders both secret chips and the values.
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "absent"
		v.FirstRun = true
		return v, nil
	}
	srv := adminTestPage(t, deps, settingsTestPrincipal())
	srv.env.Cfg = cfg
	body := settingsDo(t, srv, http.MethodGet, "/admin", nil, nil).Body.String()
	settingsMustContain(t, body, "leave empty to keep", "secret kept chips")
	settingsMustContain(t, body, "https://id.example.com", "issuer value")
}

// TestAdminSmallHelpers exercises the remaining display branches (row
// blankness, the maintenance card with a wired maintainer, and the Access
// page's group facts) so the package gate keeps its margin.
func TestAdminSmallHelpers(t *testing.T) {
	if !adminRowBlank(setup.Row{Cells: []setup.RowCell{{Type: "string", Value: "  "}}}) {
		t.Fatal("blank row not recognized")
	}
	if !adminRowBlank(setup.Row{Cells: []setup.RowCell{{Type: "bool", Value: "false"}}}) {
		t.Fatal("unchecked bool is not evidence of a row")
	}
	if adminRowBlank(setup.Row{Cells: []setup.RowCell{{Type: "string", Value: "x"}}}) {
		t.Fatal("non-empty row called blank")
	}

	deps := adminBaseDeps()
	deps.Maintain = func() (AdminMaintainView, bool) {
		return AdminMaintainView{Passes: 3, LastPassSeconds: 3}, true
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = config.Defaults()
	s.env.Cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}
	s.env.Cfg.Store.Backend = "memory"
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "in-memory", "maintenance store target")
	body = settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	settingsMustContain(t, body, "spindle-admin", "access admin group row")
	settingsMustContain(t, body, "admin", "admin verdict chip")

	// A nil config leaves the Access page's empty state honest.
	s = adminTestPage(t, adminBaseDeps(), settingsTestPrincipal())
	s.env.Cfg = nil
	body = settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	settingsMustContain(t, body, "No effective sign-in configuration", "empty access state")
}
