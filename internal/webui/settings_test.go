package webui

// settings_test.go — the settings group's httptest surface: /setup, the repo
// settings tabs and their authority gates, /{org}/settings, and /keys.
//
// The template set is built from THIS group's files only (base.html + the
// settings partials + each settings page), and the routes are mounted with
// mountSettings directly, so a sibling migration group's half-written file
// cannot make these tests red. BindSettings/BindAdmin are process-wide
// (server.go documents one provider per process), so these tests are
// sequential: only tests that bind nothing call t.Parallel()
// (testsupport_test.go, docs/go/15_testing.md §6.7).

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
)

// --- harness ---------------------------------------------------------------

type settingsFakes struct {
	deps      testDeps
	principal auth.Principal
}

// testDeps carries a group harness's fakes for BOTH seams: the Administration
// area's AdminDeps and the repo/org SettingsDeps. Promoted fields can be set in
// one composite literal, so a test names only the seam it exercises.
// RunnerTenancy is the /admin/runners tenancy seam (its own binding: the mint
// carries the repository policy, which AdminDeps.MintRunner cannot).
type testDeps struct {
	AdminDeps
	SettingsDeps
	RunnerTenancy AdminRunnerTenancyDeps
}

func settingsTestPrincipal() auth.Principal {
	return auth.Principal{Name: "alice", Email: "alice@example.com", Write: true, Admin: true}
}

// newSettingsServer builds a page provider with only this group's templates
// and routes. Every dependency defaults to nil ("not wired").
func newSettingsServer(t *testing.T, f *settingsFakes) *Server {
	t.Helper()
	cfg := config.Defaults()
	// A complete OIDC set: the normal-boot shape every page test describes.
	cfg.Server.Auth.AnonymousRead = true
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.OAuthClientID = "spindle"
	cfg.Server.Auth.OAuthClientSecret = "s3cret"
	cfg.Server.Auth.SessionSecret = strings.Repeat("x", 32)
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}
	p := settingsTestPrincipal()
	if f != nil && f.principal.Name != "" {
		p = f.principal
	}
	s := &Server{
		env:  Env{Cfg: cfg},
		auth: pageAuthFor(p),
		tpl:  map[string]*template.Template{},
	}
	s.tpl = settingsTestTemplates(t, s)
	s.mountSettings()
	deps := testDeps{}
	if f != nil {
		deps = f.deps
	}
	s.mountAdmin()
	BindSettings(deps.SettingsDeps)
	BindAdmin(deps.AdminDeps)
	BindAdminRunnerTenancy(deps.RunnerTenancy)
	t.Cleanup(func() {
		BindSettings(SettingsDeps{})
		BindAdmin(AdminDeps{})
		BindAdminRunnerTenancy(AdminRunnerTenancyDeps{})
	})
	return s
}

// settingsTemplates is the group's fixed template subset, parsed once
// (testsupport_test.go).
var settingsTemplates onceTemplates

func pageAuthFor(p auth.Principal) pages.Auth {
	return pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
}

// settingsTestTemplates parses base + this group's partials + each page. The
// subset is fixed, so it is parsed once per test binary (testsupport_test.go).
func settingsTestTemplates(t *testing.T, s *Server) map[string]*template.Template {
	t.Helper()
	return settingsTemplates.get(t, func() (map[string]*template.Template, error) {
		return buildSettingsTemplates(s)
	})
}

func buildSettingsTemplates(s *Server) (map[string]*template.Template, error) {
	partials, err := fs.Glob(tplFS, "templates/partials/settings_*.html")
	if err != nil || len(partials) == 0 {
		return nil, fmt.Errorf("settings partials: %v (%d)", err, len(partials))
	}
	// The Administration area owns admin_*.html partials; its Access page also
	// composes the shared empty-state block (collab_common.html), which every
	// page set carries at runtime because render.go parses every partial.
	adminPartials, err := fs.Glob(tplFS, "templates/partials/admin_*.html")
	if err != nil || len(adminPartials) == 0 {
		return nil, fmt.Errorf("admin partials: %v (%d)", err, len(adminPartials))
	}
	partials = append(partials, adminPartials...)
	// base.html composes the shared header/footer, the repository chrome
	// (repo_header/repo_tabs) it renders for a repo-scoped page, and the
	// identity notice; they are foundation/shared partials, not another
	// group's.
	shared := []string{
		"templates/partials/header.html",
		// The header's theme menu is its own partial (PLAN-THEMES §7.4); the
		// curated list here must carry it the way render.go's glob does.
		"templates/partials/theme_menu.html",
		"templates/partials/footer.html",
		"templates/partials/repo_header.html",
		"templates/partials/repo_tabs.html",
		"templates/partials/repo_wal_ops.html",
		"templates/partials/collab_common.html",
	}
	out := map[string]*template.Template{}
	// The shared error page: a minimal stub rather than templates/pages/
	// error.html, because that page compares an int with the group-shared
	// `eq` func (helpers.go defines eq as string-only), which makes
	// render→errorPage→render recurse. Reported to the integrator; the stub
	// keeps these tests about the settings group.
	const errStub = `{{define "base"}}<!doctype html><html><body><main id="app">{{template "content" .}}</main></body></html>{{end}}` +
		`{{define "content"}}<h1>{{.Chrome.Title}}</h1><p>{{.Data.Message}}</p>{{end}}`
	errTpl := template.Must(template.New("base").Funcs(s.funcs()).Parse(errStub))
	out["error"] = errTpl
	for _, page := range []string{"repo", "org", "keys"} {
		files := append([]string{"templates/base.html"}, shared...)
		files = append(files, partials...)
		files = append(files, "templates/pages/settings/"+page+".html")
		tpl, err := template.New("base").Funcs(s.funcs()).ParseFS(tplFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse settings/%s: %w", page, err)
		}
		out["settings/"+page] = tpl
	}
	for _, page := range []string{"overview", "config", "access", "directory", "maintenance", "reports", "audit", "runners", "restart"} {
		files := append([]string{"templates/base.html"}, shared...)
		files = append(files, partials...)
		files = append(files, "templates/pages/admin/"+page+".html")
		tpl, err := template.New("base").Funcs(s.funcs()).ParseFS(tplFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse admin/%s: %w", page, err)
		}
		out["admin/"+page] = tpl
	}
	return out, nil
}

// do drives one request through the group's routes.
func settingsDo(t *testing.T, s *Server, method, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	r.Host = "example.com"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// Every write in these tests is same-origin unless a test overrides it.
		r.Header.Set("Origin", "http://example.com")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	return rec
}

func settingsMustContain(t *testing.T, body, want, what string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("%s: response does not contain %q\n---\n%s", what, want, settingsTruncate(body, 4000))
	}
}

func settingsMustNotContain(t *testing.T, body, unwanted, what string) {
	t.Helper()
	if strings.Contains(body, unwanted) {
		t.Fatalf("%s: response should not contain %q\n---\n%s", what, unwanted, settingsTruncate(body, 4000))
	}
}

func settingsTruncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// --- fake SSH key registry -------------------------------------------------

type settingsFakeKeys struct {
	rows  []api.SshKeyRecord
	calls []string
}

func (f *settingsFakeKeys) List(_ context.Context, _ string) ([]api.SshKeyRecord, error) {
	return append([]api.SshKeyRecord(nil), f.rows...), nil
}

func (f *settingsFakeKeys) Add(_ context.Context, principal, key, title string) (api.SshKeyRecord, error) {
	if !strings.HasPrefix(key, "ssh-") {
		return api.SshKeyRecord{}, &settingsTestErr{"not an authorized_keys line: expected \"ssh-ed25519 AAAA... [comment]\""}
	}
	rec := api.SshKeyRecord{
		Principal: principal, ID: "id-" + title, Key: key, Title: title,
		CreatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
	f.rows = append(f.rows, rec)
	f.calls = append(f.calls, "add:"+title)
	return rec, nil
}

func (f *settingsFakeKeys) Get(_ context.Context, _, id string) (api.SshKeyRecord, error) {
	for _, r := range f.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return api.SshKeyRecord{}, &settingsTestErr{"not found"}
}

func (f *settingsFakeKeys) Delete(_ context.Context, _, id string) error {
	for i, r := range f.rows {
		if r.ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			f.calls = append(f.calls, "delete:"+id)
			return nil
		}
	}
	return &settingsTestErr{"not found"}
}

type settingsTestErr struct{ msg string }

func (e *settingsTestErr) Error() string { return e.msg }

// adminFakeView is the schema the setup tests render.
func adminFakeView() *AdminConfigView {
	return &AdminConfigView{
		FileState: "absent",
		AuthMode:  "oidc",
		Sections: []AdminConfigSection{
			{Section: "server", Essential: []AdminConfigField{
				{Key: "server.listen", Type: "string", Value: "0.0.0.0:8080", Default: "0.0.0.0:8080"},
				{Key: "server.request_timeout", Type: "duration", Value: "1h0m0s", Default: "1h0m0s"},
			}, Advanced: []AdminConfigField{
				{Key: "server.http2", Type: "bool", Value: "true", Default: "true"},
			}},
			{Section: "cache", Essential: []AdminConfigField{
				{Key: "cache.max_bytes", Type: "size", Value: "21474836480", Default: "21474836480"},
			}},
		},
	}
}

// --- /setup ----------------------------------------------------------------

func TestAdminConfigRendersSchemaSections(t *testing.T) {
	f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) {
		return adminFakeView(), nil
	}}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-section="server"`, "setup")
	settingsMustContain(t, body, `data-section="cache"`, "setup")
	settingsMustContain(t, body, `data-key="server.listen"`, "setup")
	// The examples come from the display metadata (FIELDS) when the schema
	// does not carry them; both spellings are the ones the validator accepts.
	settingsMustContain(t, body, "0.0.0.0:8080", "setup example")
	settingsMustContain(t, body, "e.g.", "setup example label")
	// Durations render in the spec spelling, sizes in their unit spelling.
	settingsMustContain(t, body, `value="1h"`, "duration spelling")
	settingsMustContain(t, body, `value="20GiB"`, "size spelling")
	// Advanced rows stay mounted inside the native disclosure (#168).
	settingsMustContain(t, body, `data-advanced-for="server"`, "advanced group")
	settingsMustContain(t, body, `name="cfg.server.http2"`, "advanced row mounted")
	settingsMustContain(t, body, `hx-post="/admin/config"`, "htmx hook")
}

func TestAdminConfigDeniedRendersRestrictedCard(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		Access: func(*http.Request, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin principal required"}
		},
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /admin/config (denied) = %d, want 403 restricted card", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Access restricted", "denied page")
	settingsMustContain(t, body, "admin principal required", "denied detail")
	settingsMustNotContain(t, body, `name="cfg.server.listen"`, "denied page")
}

func TestAdminConfigSaveSuccessRedirectsWithRestartKeys(t *testing.T) {
	var saved map[string]string
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(_ context.Context, values map[string]string) (AdminSaveResult, []AdminError, error) {
			saved = values
			return AdminSaveResult{Saved: true, RequiresRestart: []string{"server.listen", "cache.max_bytes"}}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)
	form := url.Values{
		"op":                  {"save"},
		"cfg.server.listen":   {"0.0.0.0:9090"},
		"cfg.server.http2":    {"false", "true"}, // hidden false + checked true
		"cfg.cache.max_bytes": {"32GiB"},
	}
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	settingsMustContain(t, loc, "saved=1", "redirect")
	settingsMustContain(t, loc, "restart=server.listen%2Ccache.max_bytes", "redirect restart keys")
	settingsMustNotContain(t, loc, "token=", "no setup token on the redirect")
	if got := saved["server.http2"]; got != "true" {
		t.Fatalf("repeated boolean field resolved to %q, want the last (checkbox) value", got)
	}
	if saved["server.listen"] != "0.0.0.0:9090" {
		t.Fatalf("saved values = %v", saved)
	}
	// The redirect target renders the restart list (the flash).
	rec = settingsDo(t, s, http.MethodGet, loc, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", loc, rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Restart required for:", "saved page")
	settingsMustContain(t, rec.Body.String(), "cache.max_bytes", "saved page restart keys")
}

// TestAdminConfigSaveNamesWrittenFile: the save's own answer (AdminSaveResult.Path)
// reaches the operator twice — the redirect carries it and the saved page names it.
// "Written to <data-dir>/spindle.toml" was a lie under an explicit --config.
func TestAdminConfigSaveNamesWrittenFile(t *testing.T) {
	const target = "/etc/spindle/spindle.toml"
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) {
			v := adminFakeView()
			v.SavePath = target
			return v, nil
		},
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{Saved: true, Path: target}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)

	// The Configuration page names the file Save writes before any save.
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), target, "configuration page")

	rec = settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	settingsMustContain(t, loc, "path="+url.QueryEscape(target), "redirect path")
	rec = settingsDo(t, s, http.MethodGet, loc, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", loc, rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Written to", "saved banner")
	settingsMustContain(t, body, target, "saved banner path")
	settingsMustNotContain(t, body, "&lt;data-dir&gt;/spindle.toml", "saved banner fallback copy")
}

func TestAdminConfigValidationErrorReRendersTypedValues(t *testing.T) {
	errs := []AdminError{{Key: "server.request_timeout", Message: "server.request_timeout: not a duration"}}
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Validate: func(_ context.Context, values map[string]string) ([]AdminError, error) {
			if values["server.request_timeout"] != "1 fortnight" {
				t.Fatalf("validator saw %q", values["server.request_timeout"])
			}
			return errs, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{
		"op": {"validate"}, "cfg.server.request_timeout": {"1 fortnight"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation error = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "not a duration", "error line")
	settingsMustContain(t, body, `class="err-line"`, "error line class")
	settingsMustContain(t, body, `value="1 fortnight"`, "typed value preserved")
	settingsMustContain(t, body, "validation failed — nothing was saved", "error banner")
}

// TestAdminConfigMasksStoredSecrets pins UI-04: a stored secret (session,
// OAuth client, webhook, S3 key) never appears in the page body in any auth
// mode — the row shows the mask and is editable by overwrite only.
func TestAdminConfigMasksStoredSecrets(t *testing.T) {
	const secret = "s3cr3t-stored-value-do-not-render"
	for _, name := range []string{"admin config"} {
		t.Run(name, func(t *testing.T) {
			f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) {
				v := adminFakeView()
				v.Sections[0].Essential = append(v.Sections[0].Essential,
					AdminConfigField{Key: "server.auth.session_secret", Type: "string", Value: secret, Secret: true},
					AdminConfigField{Key: "server.auth.issuer", Type: "url", Value: "https://id.example.com"},
				)
				return v, nil
			}}}
			s := newSettingsServer(t, f)
			rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /admin/config = %d", rec.Code)
			}
			body := rec.Body.String()
			settingsMustNotContain(t, body, secret, "stored secret in the page body")
			settingsMustContain(t, body, setup.SecretMask, "masked secret row")
			settingsMustContain(t, body, `name="cfg.server.auth.session_secret"`, "secret row")
			settingsMustContain(t, body, `type="password"`, "secret password control")
			// Non-secret neighbours still render their value.
			settingsMustContain(t, body, "https://id.example.com", "issuer value")
		})
	}
}

// TestAdminConfigRejectNeverEchoesSecrets: a rejected save must not bounce a
// typed secret back into the re-rendered form.
func TestAdminConfigRejectNeverEchoesSecrets(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) {
			v := adminFakeView()
			v.Sections[0].Essential = append(v.Sections[0].Essential,
				AdminConfigField{Key: "server.auth.session_secret", Type: "string", Secret: true})
			return v, nil
		},
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, []AdminError{{Key: "server.listen", Message: "not a listen address"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{
		"op":                             {"save"},
		"cfg.server.auth.session_secret": {"typed-secret-value"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected save = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustNotContain(t, body, "typed-secret-value", "typed secret echoed back")
	settingsMustContain(t, body, "not a listen address", "error line")
}

func TestAdminConfigSaveReportsValidationErrors(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, []AdminError{{Message: "server.auth.issuer: required"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save validation = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "server.auth.issuer: required", "save error")
}

func TestAdminConfigHTMXErrorSwaps(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, []AdminError{{Key: "server.listen", Message: "not a listen address"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx error = %d, want 200 (htmx swaps only 2xx)", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not a listen address", "htmx error body")
}

func TestAdminConfigCrossOriginWriteRefused(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			t.Fatal("cross-origin write must not reach the service")
			return AdminSaveResult{}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", rec.Code)
	}
	// No origin header at all is also refused.
	rec = settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, map[string]string{"Origin": ""})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("origin-less write = %d, want 403", rec.Code)
	}
}

func TestAdminAuthTestFragment(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		AuthTest: func(_ context.Context, issuer, clientID, redirect string) (*AdminAuthTest, []AdminError) {
			if issuer != "https://id.example.com" || clientID != "spindle" {
				t.Fatalf("auth test saw %q %q", issuer, clientID)
			}
			return &AdminAuthTest{
				Issuer: issuer, AuthorizationEndpoint: issuer + "/auth",
				TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks", ClientIDPresent: true,
			}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test", url.Values{
		"issuer": {"https://id.example.com"}, "client_id": {"spindle"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth test = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Discovery OK", "auth test")
	settingsMustContain(t, body, "jwks", "auth test endpoints")
}

func TestAdminAuthTestErrors(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		AuthTest: func(context.Context, string, string, string) (*AdminAuthTest, []AdminError) {
			return nil, []AdminError{{Key: "server.auth.issuer", Message: "discovery fetch failed"}}
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test", url.Values{"issuer": {"https://nope.invalid"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth test errors = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "discovery fetch failed", "auth test error")
}

func TestAdminConfigNotWired(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired setup = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired setup")

	rec = settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired save = %d, want 503", rec.Code)
	}
}

func TestAdminConfigOnlyBanner(t *testing.T) {
	view := adminFakeView()
	view.FileState = "invalid"
	view.Errors = []AdminError{{Key: "store.s3.bucket", Message: "bucket is required"}}
	f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) { return view, nil }}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	body := rec.Body.String()
	settingsMustContain(t, body, "Restricted mode — the configuration file is invalid.", "restricted-mode banner")
	settingsMustContain(t, body, "bucket is required", "boot errors")
}

// --- repo settings ---------------------------------------------------------

func settingsRepoFakes(role string) *settingsFakes {
	return &settingsFakes{deps: testDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return role, nil },
		RepoGeneral: func(context.Context, string, string) (*SettingsGeneral, error) {
			return &SettingsGeneral{
				Description:  "a monorepo",
				SelfApproval: true,
				Features: []SettingsFeatureRow{
					{Key: "issues", Enabled: true},
					{Key: "pulls", Enabled: false},
				},
			}, nil
		},
	}}
}

func TestSettingsRepoGeneralTabRenders(t *testing.T) {
	f := settingsRepoFakes("admin")
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET general = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `id="settings-panel"`, "panel")
	settingsMustContain(t, body, `value="a monorepo"`, "description")
	settingsMustContain(t, body, `aria-current="page"`, "active tab")
	settingsMustContain(t, body, `href="/acme/monorepo/settings/scheduled"`, "nav link")
	settingsMustContain(t, body, `name="feature_issues"`, "feature toggle")
	settingsMustContain(t, body, `name="feature_pulls"`, "feature toggle")
	settingsMustContain(t, body, `name="allow_self_approval"`, "review toggle")
	// The seven feature keys are always present even when the dep sends a
	// partial set (fail-open: absent means enabled).
	settingsMustContain(t, body, `name="feature_star"`, "feature default")
	settingsMustContain(t, body, `name="feature_packages"`, "registry toggle")
	// Nav order is the sidebar contract.
	navOrder := []string{"General", "Scheduled tasks", "Mirror", "Push policy",
		"Effective config &amp; history", "Access", "CI tokens", "Secrets", "Webhooks", "WAL", "Danger Zone"}
	pos := -1
	for _, label := range navOrder {
		at := strings.Index(body, label)
		if at < 0 {
			t.Fatalf("nav entry %q missing", label)
		}
		if at < pos {
			t.Fatalf("nav entry %q out of order", label)
		}
		pos = at
	}
}

func TestSettingsRepoBareSettingsRedirectsToDefaultTab(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("read"))
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bare settings = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/acme/monorepo/settings/general" {
		t.Fatalf("Location = %q", got)
	}
}

func TestSettingsRepoUnknownTabAndPushmirrorAlias(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoMirror = func(context.Context, string, string) (*SettingsMirror, error) {
		return &SettingsMirror{Configured: true, Upstream: "https://up.example/acme/mono.git", LastResult: "ok"}, nil
	}
	s := newSettingsServer(t, f)

	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/bogus", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unknown tab = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/acme/monorepo/settings/general" {
		t.Fatalf("unknown tab Location = %q", got)
	}

	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/pushmirror", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pushmirror alias = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-settings-tab="mirror"`, "alias resolves to mirror")
	settingsMustContain(t, body, "Pull mirror", "mirror panel")
	settingsMustContain(t, body, "remove mirror", "configured mirror")
}

func TestSettingsRepoAdminOnlyTabForbiddenForNonAdmin(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("read"))
	for _, tab := range []string{"tokens", "webhooks", "policy", "config", "danger",
		"protection", "general", "scheduled", "mirror"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s as reader = %d, want 403", tab, rec.Code)
		}
	}
	// Access is triage-gated too.
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/access", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("access as reader = %d, want 403", rec.Code)
	}
	// A reader's nav never shows the admin-only entries (hidden, not 403'd).
	body := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/wal", nil, nil).Body.String()
	settingsMustNotContain(t, body, "/settings/tokens", "reader nav")
	settingsMustNotContain(t, body, "/settings/danger", "reader nav")
	settingsMustNotContain(t, body, "/settings/mirror", "reader nav")
	settingsMustNotContain(t, body, "/settings/general", "reader nav")
}

// TestSettingsRepoGuestTabsMirrorAPIGate is the page-contract pin for the
// audit's gate inconsistency: under token auth with anonymous_read, an
// anonymous reader must get the SAME 403 on every editing tab the POSTs and
// the admin tabs answer — never a live form, and never a configured mirror
// upstream URL.
func TestSettingsRepoGuestTabsMirrorAPIGate(t *testing.T) {
	f := settingsRepoFakes("read")
	f.principal = auth.Principal{Name: "anonymous", Anonymous: true}
	f.deps.RepoMirror = func(context.Context, string, string) (*SettingsMirror, error) {
		return &SettingsMirror{Configured: true, Upstream: "https://secret.internal/upstream.git"}, nil
	}
	s := newSettingsServer(t, f)
	for _, tab := range []string{"general", "scheduled", "mirror", "policy", "protection",
		"config", "access", "tokens", "webhooks", "danger"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("guest GET %s = %d, want 403", tab, rec.Code)
		}
		settingsMustNotContain(t, rec.Body.String(), "secret.internal", "guest body leaks the upstream")
	}
}

func TestSettingsRepoWriteForbiddenForNonAdmin(t *testing.T) {
	f := settingsRepoFakes("write")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("non-admin write must not reach the service")
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", url.Values{"description": {"nope"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin write = %d, want 403", rec.Code)
	}
}

func TestSettingsRepoWriteSuccessAndError(t *testing.T) {
	var got SettingsRepoWrite
	fail := true
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(_ context.Context, owner, repo string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		got = w
		if owner != "acme" || repo != "monorepo" {
			t.Fatalf("write target = %s/%s", owner, repo)
		}
		if fail {
			return SettingsWriteResult{}, &SettingsWriteError{Status: http.StatusConflict, Message: "changed under you, reload"}
		}
		return SettingsWriteResult{Notice: "features saved"}, nil
	}
	s := newSettingsServer(t, f)

	form := url.Values{
		"description":         {"new words"},
		"feature_issues":      {"false"},
		"feature_pulls":       {"false", "true"},
		"allow_self_approval": {"false"},
	}
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", form, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("write conflict = %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "changed under you, reload", "inline error")
	settingsMustContain(t, body, `value="new words"`, "typed value preserved")
	if got.Action != SettingsActionGeneral {
		t.Fatalf("action = %q", got.Action)
	}
	if got.Values["feature_issues"] != "false" {
		t.Fatalf("form values = %v", got.Values)
	}

	fail = false
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("write success = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/acme/monorepo/settings/general?notice=features+saved" {
		t.Fatalf("Location = %q", got)
	}
}

func TestSettingsRepoVisibilityGateHidesRepo(t *testing.T) {
	f := settingsRepoFakes("admin")
	s := newSettingsServer(t, f)
	s.auth.CheckRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}
	}
	rec := settingsDo(t, s, http.MethodGet, "/acme/secret/settings/general", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invisible repo settings = %d, want 404", rec.Code)
	}
}

func TestSettingsRepoRoleErrorIsServiceUnavailable(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoRole = func(context.Context, string, string, auth.Principal) (string, error) {
		return "", &settingsTestErr{"identity store down"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("role error = %d, want 503", rec.Code)
	}
}

func TestSettingsRepoNoRoleIsForbidden(t *testing.T) {
	for _, role := range []string{"none", ""} {
		s := newSettingsServer(t, settingsRepoFakes(role))
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role %q general = %d, want 403", role, rec.Code)
		}
		rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/wal", nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role %q wal = %d, want 403", role, rec.Code)
		}
	}
}

func TestSettingsRepoDangerTypedConfirm(t *testing.T) {
	var actions []string
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(_ context.Context, _, _ string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		actions = append(actions, w.Action)
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)

	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/danger", url.Values{
		"danger_action": {"delete"}, "confirm": {"acme/mon"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong confirm = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "type acme/monorepo exactly to confirm", "danger error")
	if len(actions) != 0 {
		t.Fatalf("unconfirmed delete reached the service: %v", actions)
	}

	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/danger", url.Values{
		"danger_action": {"delete"}, "confirm": {"acme/monorepo"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirmed delete = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(actions) != 1 || actions[0] != SettingsActionDelete {
		t.Fatalf("actions = %v", actions)
	}
}

func TestSettingsRepoPanelsRenderAndGaps(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoScheduled = func(context.Context, string, string) (*SettingsScheduled, error) {
		return &SettingsScheduled{
			Sections:    settingsScheduledSections(nil, config.Defaults(), nil),
			HostFacts:   []SettingsKV{{Key: "host", Value: "node-1"}, {Key: "roles", Value: "serve, maintain"}},
			UpstreamGit: "https://up.example/acme/mono.git",
			FollowLine:  "following every 5m",
		}, nil
	}
	f.deps.RepoWAL = func(context.Context, string, string) (*SettingsWAL, error) {
		return &SettingsWAL{
			Owner: "acme", Name: "monorepo", Health: "ok", HealthTone: "chip chip-open",
			ServedBy: "node-1", CloneURL: "https://example.com/acme/monorepo.git",
			Facts: []SettingsKV{{Key: "entries", Value: "3"}}, Packs: "1 live packs",
			Panel: walOpsPanel{
				Ops:    []walOp{{Op: "fsck", Params: []walParam{{Name: "connectivity", Kind: "checkbox", Values: []string{"1"}}}}},
				OpsURL: "/acme/monorepo/api/ops/",
				CanRun: true,
			},
		}, nil
	}
	f.deps.RepoPolicy = func(context.Context, string, string) (*SettingsPolicy, error) {
		return &SettingsPolicy{Text: "protect default", DryRunDone: true, DryRunAllow: 2, DryRunDeny: 1,
			DryRunRefs: []SettingsKV{{Key: "1", Value: "allowed"}}}, nil
	}
	f.deps.RepoConfig = func(context.Context, string, string) (*SettingsConfig, error) {
		return &SettingsConfig{
			Text: "[features]\nissues = true\n", Message: "tune",
			Revisions: []SettingsRevision{{Rev: 3, Author: "alice", At: "2026-11-01T00:00:00Z", Message: "tighten",
				Diff: settingsLineDiff("a\nb\n", "a\nc\n")}},
		}, nil
	}
	f.deps.RepoAccess = func(context.Context, string, string, auth.Principal) (*SettingsAccess, error) {
		return &SettingsAccess{
			Role: "admin", Visibility: "public", Version: 7,
			Collaborators: []SettingsCollaborator{{Principal: "user:alice", Role: "admin", Source: "binding"}},
			Bindings:      []SettingsBinding{{Subject: "user:bob", Role: "write"}},
			Teams:         []SettingsOption{{ID: "platform", Label: "Platform"}},
		}, nil
	}
	f.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{
			TokensWired: true, WebhooksWired: true,
			Tokens:   []SettingsToken{{ID: "t1", Name: "ci", Scopes: "read", CreatedBy: "alice", CreatedAt: "2026-11-01T00:00:00Z", Status: "active"}},
			Webhooks: []SettingsWebhook{{ID: "w1", URL: "https://example.com/hook", Active: true, SecretSet: true, LastPing: "delivered"}},
		}, nil
	}
	s := newSettingsServer(t, f)

	for _, tc := range []struct {
		tab  string
		want []string
	}{
		{"scheduled", []string{"Bundle strategies", "Compaction", "Override host config", "Placement &amp; host facts", "node-1", "following every 5m"}},
		{"policy", []string{"Push policy", "protect default", "Dry run:", "2", "Remove policy"}},
		{"config", []string{"Effective config", "Publish message", "History", "#3", "tighten", "- b"}},
		{"access", []string{"Effective access", "user:alice", "Role bindings", "user:bob", "version 7"}},
		{"tokens", []string{"Create a CI token", "ci", "revoke"}},
		{"webhooks", []string{"Add a webhook", "https://example.com/hook", "ping", "delivered", "edit webhook", "Rotate signing secret"}},
		{"wal", []string{"WAL health", "entries", "fsck", "data-op-run"}},
		{"danger", []string{"Danger Zone", "Transfer this repository", "Delete this repository", "data-danger-expected"}},
	} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tc.tab, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tc.tab, rec.Code)
		}
		for _, want := range tc.want {
			settingsMustContain(t, rec.Body.String(), want, tc.tab+" tab")
		}
	}
}

func TestSettingsRepoAccessReadOnlyCardIsNotNested(t *testing.T) {
	// Defect 6 (round-3 audit): the read-only branch opened a
	// data-access-readonly section and never closed it, so the explanatory
	// card nested inside it and "Visibility & role bindings" rendered twice
	// for a triage/write principal (the PR tab's read floor is triage; a
	// "read" principal gets the 403 the API gate gives, by design).
	for _, role := range []string{"triage", "write"} {
		f := settingsRepoFakes(role)
		f.deps.RepoAccess = func(context.Context, string, string, auth.Principal) (*SettingsAccess, error) {
			return &SettingsAccess{
				Role: role, Visibility: "private", Version: 4,
				Bindings: []SettingsBinding{{Subject: "user:alice", Role: "admin"}},
			}, nil
		}
		s := newSettingsServer(t, f)
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/access", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s access page = %d", role, rec.Code)
		}
		body := rec.Body.String()
		if got := strings.Count(body, "data-access-readonly"); got != 1 {
			t.Fatalf("%s access page renders %d data-access-readonly sections, want exactly one", role, got)
		}
		if got := strings.Count(body, "Visibility &amp; role bindings"); got != 1 {
			t.Fatalf("%s access page renders the heading %d times, want once (nested card)", role, got)
		}
		// The read-only view still shows the stored facts and never a form.
		settingsMustContain(t, body, "user:alice", "read-only bindings")
		settingsMustContain(t, body, "Only a repository admin can change visibility or role bindings.", "admin-only note")
		settingsMustNotContain(t, body, `name="binding_count"`, "read-only page has no admin form")
	}
}

func TestSettingsRepoTokenCreateShowsSecretOnce(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
		return &SettingsAdmin{TokensWired: true, WebhooksWired: true, Tokens: []SettingsToken{{ID: "t2", Name: "ci2", Status: "active"}}}, nil
	}
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{Secret: "whsec_once", Notice: "token created — copy it now, it is shown once"}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/tokens", url.Values{"name": {"ci2"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token create = %d, want 200 (the secret must not enter a URL)", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "whsec_once", "one-time secret")
	settingsMustContain(t, body, "shown once", "one-time notice")
	settingsMustContain(t, body, `id="ci-token-secret"`, "copy target")
}

// --- /keys -----------------------------------------------------------------

func TestSettingsKeysListAddDeleteRoundTrip(t *testing.T) {
	store := &settingsFakeKeys{rows: []api.SshKeyRecord{{
		ID: "id-one", Key: "ssh-rsa ZZZZUNIQUEBLOB1234 ada@laptop",
		Title: "laptop", CreatedAt: time.Now().Add(-2 * time.Hour),
	}}}
	f := &settingsFakes{deps: testDeps{}}
	s := newSettingsServer(t, f)
	s.env.API = &api.Env{SSHKeys: store}

	rec := settingsDo(t, s, http.MethodGet, "/keys", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /keys = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "SSH keys", "keys page")
	settingsMustContain(t, body, "laptop", "key row")
	// shortKey elides the middle of the blob.
	settingsMustContain(t, body, "ssh-rsa ZZZZUNIQ…1234", "short key")
	settingsMustContain(t, body, "ada@laptop", "key comment")

	// Add: the registry validates; a valid line lands and redirects.
	rec = settingsDo(t, s, http.MethodPost, "/keys", url.Values{
		"key": {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISECOND ci@box"}, "title": {"ci"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add key = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/keys?notice=added" {
		t.Fatalf("add key Location = %q", got)
	}
	if len(store.rows) != 2 {
		t.Fatalf("rows after add = %d", len(store.rows))
	}
	rec = settingsDo(t, s, http.MethodGet, "/keys?notice=added", nil, nil)
	settingsMustContain(t, rec.Body.String(), "key added", "add notice")
	settingsMustContain(t, rec.Body.String(), "ci@box", "added row")

	// Delete: the row (and its elided form) disappears.
	rec = settingsDo(t, s, http.MethodPost, "/keys/id-one/delete", url.Values{}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete key = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/keys?notice=removed" {
		t.Fatalf("delete key Location = %q", got)
	}
	rec = settingsDo(t, s, http.MethodGet, "/keys?notice=removed", nil, nil)
	settingsMustContain(t, rec.Body.String(), "key removed", "remove notice")
	// The form's placeholder also spells "ada@laptop"; the row itself is the
	// elided key cell.
	settingsMustNotContain(t, rec.Body.String(), "ZZZZUNIQ…1234", "removed row")
}

func TestSettingsKeysRegistryErrorRenderedInline(t *testing.T) {
	store := &settingsFakeKeys{}
	s := newSettingsServer(t, &settingsFakes{})
	s.env.API = &api.Env{SSHKeys: store}
	rec := settingsDo(t, s, http.MethodPost, "/keys", url.Values{"key": {"not-a-key"}, "title": {"bad"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "not an authorized_keys line", "registry error")
	settingsMustContain(t, body, "not-a-key", "typed value preserved")
	if len(store.rows) != 0 {
		t.Fatalf("invalid key was stored")
	}
	// An empty key never reaches the registry.
	rec = settingsDo(t, s, http.MethodPost, "/keys", url.Values{"key": {"   "}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty key = %d, want 400", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "paste an authorized_keys line first", "empty key message")
}

func TestSettingsKeysNotWiredAndAnonymousGate(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	rec := settingsDo(t, s, http.MethodGet, "/keys", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired keys = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not configured", "unwired keys")

	// anonymous with anonymous_read off gets the 401 page.
	s.env.Cfg.Server.Auth.AnonymousRead = false
	f := &settingsFakes{principal: auth.Anonymous()}
	s2 := newSettingsServer(t, f)
	s2.env.Cfg.Server.Auth.AnonymousRead = false
	rec = settingsDo(t, s2, http.MethodGet, "/keys", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous keys = %d, want 401", rec.Code)
	}
}

// --- org settings ----------------------------------------------------------

func settingsOrgFakes(canManage bool) *settingsFakes {
	return &settingsFakes{deps: testDeps{
		OrgView: func(_ context.Context, org string, _ auth.Principal) (*SettingsOrgPage, error) {
			return &SettingsOrgPage{
				Org: org, CanManage: canManage,
				Profile: &SettingsOrgProfile{
					DisplayName: "Acme", Description: "builds things", Timezone: "UTC",
					BioMarkdown: "**hello**", ReadOnly: !canManage,
				},
				Members:  []SettingsOrgMember{{Principal: "user:alice", Role: "owner"}, {Principal: "user:bob", Role: "member"}},
				Teams:    []SettingsOrgTeam{{Slug: "platform", Name: "Platform", Members: []string{"user:alice"}}},
				Invites:  []SettingsOrgInvite{{ID: "inv1", Subject: "jane@example.com", Role: "member", InvitedBy: "alice", ExpiresAt: "2026-12-01T00:00:00Z"}},
				Activity: []SettingsOrgEvent{{Seq: 9, At: "2026-11-01T00:00:00Z", Action: "org.member.add", Actor: "alice", Title: "added bob"}},
			}, nil
		},
	}}
}

func TestSettingsOrgNonOwnerReadOnlyTabs(t *testing.T) {
	s := newSettingsServer(t, settingsOrgFakes(false))
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /acme/settings = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "read-only — org owner required", "read-only note")
	settingsMustContain(t, body, "Members", "tab")
	settingsMustNotContain(t, body, "Activity", "owner-only tab")
	settingsMustNotContain(t, body, "Danger", "owner-only tab")
	// Member roles are text, not a select, for non-owners.
	settingsMustNotContain(t, body, `name="org_action" value="member"`, "member form")

	for _, tab := range []string{"activity", "danger"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/settings?tab="+tab, nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner %s = %d, want 403", tab, rec.Code)
		}
	}
}

func TestSettingsOrgOwnerTabsAndWrite(t *testing.T) {
	var got SettingsOrgWrite
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(_ context.Context, org string, _ auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		got = w
		if org != "acme" {
			t.Fatalf("org = %q", org)
		}
		return SettingsWriteResult{Notice: "member saved"}, nil
	}
	s := newSettingsServer(t, f)

	rec := settingsDo(t, s, http.MethodGet, "/acme/settings", nil, nil)
	body := rec.Body.String()
	settingsMustContain(t, body, "Activity", "owner tab")
	settingsMustContain(t, body, "Danger", "owner tab")
	rec = settingsDo(t, s, http.MethodGet, "/acme/settings?tab=members", nil, nil)
	settingsMustContain(t, rec.Body.String(), "user:bob", "members tab")
	settingsMustContain(t, rec.Body.String(), "Add a member", "members tab form")
	settingsMustContain(t, rec.Body.String(), `name="org_action" value="member"`, "member form")

	rec = settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"member"}, "principal": {"user:carol"}, "role": {"owner"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("org write = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/acme/settings?tab=members&notice=member+saved" {
		t.Fatalf("org write Location = %q", got)
	}
	if got.Action != SettingsOrgMemberSet || got.Get("principal") != "user:carol" {
		t.Fatalf("org write = %+v", got)
	}
}

func TestSettingsOrgWriteForbiddenForNonOwner(t *testing.T) {
	f := settingsOrgFakes(false)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("non-owner write must not reach the service")
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"member"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner write = %d, want 403", rec.Code)
	}
}

func TestSettingsOrgInviteLinkRenderedNotRedirected(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{Link: "/invitations?token=abc123", Notice: "invitation created"}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"invite"}, "subject": {"jane@example.com"}, "role": {"member"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite = %d, want 200 (bearer link must not enter a URL)", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "/invitations?token=abc123", "invite link")
	settingsMustContain(t, body, `id="invite-link"`, "copy target")
}

// TestSettingsOrgInviteOneShotLinkAndMaskedPending is the org twin of the
// repo Access contract: the create response renders the human accept link in
// the page body and on the new pending row (copy control), while every later
// read — whose token the org invitation collection redacts — renders a masked
// marker instead. Nothing ever redirects the link into a URL.
func TestSettingsOrgInviteOneShotLinkAndMaskedPending(t *testing.T) {
	const link = "/invitations?id=inv2&token=org-token-2"
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{
			Notice: "invitation created — share the link below",
			Link:   link,
			Invite: &SettingsOrgInvite{
				ID: "inv2", Subject: "jane@example.com", Role: "owner",
				InvitedBy: "alice", ExpiresAt: "2026-12-01T00:00:00Z", AcceptURL: link,
			},
		}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"invite"}, "subject": {"jane@example.com"}, "role": {"owner"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite = %d, want 200 (a bearer link must not enter a redirect)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("invite redirected to %q", loc)
	}
	// The form addresses an email OR a username: a static-token principal has
	// no verified email, so an email-only input would make it uninvitable.
	settingsMustContain(t, rec.Body.String(), `name="subject" type="text"`, "username-addressed invite form")
	settingsMustContain(t, rec.Body.String(), "Email or username", "invite form label")
	settingsMustNotContain(t, rec.Body.String(), `type="email"`, "invite form still email-only")
	body := rec.Body.String()
	settingsMustContain(t, body, "/invitations?id=inv2&amp;token=org-token-2", "one-shot share link")
	settingsMustContain(t, body, `id="invite-link"`, "share card copy target")
	settingsMustContain(t, body, `id="invite-accept-inv2"`, "new row's copy target")
	settingsMustContain(t, body, `data-copy="#invite-accept-inv2"`, "new row's copy button")

	// A later read redacts: the fake OrgView row (like orgViewFn) carries no
	// AcceptURL, so the pending table shows the masked marker and no token.
	rec = settingsDo(t, s, http.MethodGet, "/acme/settings?tab=invitations", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("invitations tab = %d", rec.Code)
	}
	body = rec.Body.String()
	settingsMustContain(t, body, "token ••••••••", "masked pending marker")
	settingsMustContain(t, body, "invite-remove", "cancel control")
	settingsMustNotContain(t, body, "org-token-2", "a pending read leaks the token")
	settingsMustNotContain(t, body, "?token=", "a pending read leaks an accept URL")
	settingsMustNotContain(t, body, `id="invite-link"`, "share card survives a read")
}

// TestSettingsOrgInvitationsOwnerOnly pins the page gate to the API gate: the
// org invitation collection is CheckOrgOwner, so a non-owner neither sees the
// tab nor can fetch it, and never receives a token.
func TestSettingsOrgInvitationsOwnerOnly(t *testing.T) {
	s := newSettingsServer(t, settingsOrgFakes(false))
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("org page = %d", rec.Code)
	}
	settingsMustNotContain(t, rec.Body.String(), `tab=invitations`, "non-owner invitation tab")
	for _, target := range []string{"/acme/settings?tab=invitations"} {
		rec = settingsDo(t, s, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner %s = %d, want 403", target, rec.Code)
		}
	}
}

func TestSettingsOrgWriteErrorRendered(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Status: http.StatusConflict, Message: "org owns repositories — transfer them first"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"delete"}, "confirm": {"acme"},
	}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("org delete conflict = %d, want 409", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "org owns repositories — transfer them first", "org conflict")
}

func TestSettingsOrgDeleteConfirmRequired(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("unconfirmed delete reached the service")
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"delete"}, "confirm": {"acm"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong org confirm = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "type acme exactly to confirm", "org confirm error")
}

func TestSettingsOrgPreviewRendersMarkdownServerSide(t *testing.T) {
	s := newSettingsServer(t, settingsOrgFakes(true))
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings/preview", url.Values{"bio_markdown": {"**bold** and [x](https://e.example)"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `class="markdown-body"`, "preview wrapper")
	settingsMustContain(t, body, "<strong>bold</strong>", "rendered markdown")
	settingsMustContain(t, body, `href="https://e.example"`, "rendered link")
	// Raw HTML in the source is escaped (no client markdown/DOMPurify).
	rec = settingsDo(t, s, http.MethodPost, "/acme/settings/preview", url.Values{"bio_markdown": {"<script>alert(1)</script>"}}, nil)
	settingsMustNotContain(t, rec.Body.String(), "<script>alert(1)</script>", "preview escapes script")
}

func TestSettingsOrgNotFoundAndNotWired(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired org = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired org")

	f := &settingsFakes{deps: testDeps{OrgView: func(context.Context, string, auth.Principal) (*SettingsOrgPage, error) {
		return nil, &settingsTestErr{"no such org"}
	}}}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodGet, "/nope/settings", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown org = %d, want 404", rec.Code)
	}
}

// --- pure helpers ----------------------------------------------------------

func TestSettingsSpecSpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"1h0m0s", "1h"},
		{"168h0m0s", "1w"},
		{"24h0m0s", "1d"},
		{"90s", "90s"},
		{"1h30m", "90m"},
		{"", ""},
		{"7d", "7d"},
		{"bogus", "bogus"},
	} {
		if got := fmtSpecDuration(tc.in); got != tc.want {
			t.Fatalf("fmtSpecDuration(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"68719476736", "64GiB"},
		{"21474836480", "20GiB"},
		{"1024", "1KiB"},
		{"5", "5B"},
		{"nope", "nope"},
	} {
		if got := fmtSpecSize(tc.in); got != tc.want {
			t.Fatalf("fmtSpecSize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSettingsShortKey(t *testing.T) {
	t.Parallel()
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE ada@laptop"
	got := shortKey(line)
	if got != "ssh-ed25519 AAAAC3Nz…MPLE ada@laptop" {
		t.Fatalf("shortKey = %q", got)
	}
	if shortKey("short") != "short" {
		t.Fatal("short line should pass through")
	}
}

func TestSettingsLineDiff(t *testing.T) {
	t.Parallel()
	d := settingsLineDiff("a\nb\nc", "a\nc\nd")
	var signs []string
	for _, l := range d {
		signs = append(signs, l.Sign+l.Text)
	}
	joined := strings.Join(signs, "|")
	if !strings.Contains(joined, "-b") || !strings.Contains(joined, "+d") {
		t.Fatalf("diff = %q", joined)
	}
	if settingsLineDiff("same", "same") != nil {
		t.Fatal("identical documents should have no diff")
	}
}

func TestSettingsRoleAndTabHelpers(t *testing.T) {
	t.Parallel()
	if !settingsRoleAtLeast("admin", "triage") || settingsRoleAtLeast("read", "triage") {
		t.Fatal("role ordering")
	}
	if settingsRoleRank("") != 0 || settingsRoleRank("none") != 0 ||
		settingsRoleRank("bogus") != 0 {
		t.Fatal("an empty/unknown role must rank below every tab floor (fail closed)")
	}
	if settingsTabByID("") != "general" || settingsTabByID("pushmirror") != "mirror" ||
		settingsTabByID("nope") != "" || settingsTabByID("DANGER") != "danger" {
		t.Fatal("repo tab resolution")
	}
	if settingsOrgTabByID("") != "profile" || settingsOrgTabByID("bogus") != "" {
		t.Fatal("org tab resolution")
	}
	nav := settingsOrgNav("acme", "members", false)
	for _, n := range nav {
		if n.Active != (n.ID == "members") {
			t.Fatalf("active flag on %q", n.ID)
		}
	}
	if len(nav) != 3 {
		t.Fatalf("non-owner nav = %d entries, want 3 (invitation reads are owner-only)", len(nav))
	}
	if len(settingsOrgNav("acme", "profile", true)) != 7 {
		t.Fatal("owner nav")
	}
}

func TestSettingsSameOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		origin string
		ref    string
		want   bool
	}{
		{"get always ok", http.MethodGet, "", "", true},
		{"same origin", http.MethodPost, "http://example.com", "", true},
		{"same origin case-insensitive", http.MethodPost, "http://EXAMPLE.com", "", true},
		{"referer fallback", http.MethodPost, "", "http://example.com/acme", true},
		{"cross origin", http.MethodPost, "https://evil.example", "", false},
		{"no header", http.MethodPost, "", "", false},
		{"unparseable", http.MethodPost, "::not a url", "", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "http://example.com/acme", strings.NewReader(""))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.ref != "" {
			r.Header.Set("Referer", tc.ref)
		}
		if got := settingsSameOrigin(r); got != tc.want {
			t.Fatalf("%s: settingsSameOrigin = %v, want %v", tc.name, got, tc.want)
		}
	}
	if settingsHTMX(httptest.NewRequest(http.MethodPost, "/", nil)) {
		t.Fatal("htmx detection without header must be false")
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("HX-Request", "true")
	if !settingsHTMX(r) {
		t.Fatal("htmx detection")
	}
}

func TestSettingsRedirectURI(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "http://example.com/admin/config", nil)
	if got := adminRedirectURI(r, "https://git.example.com/"); got != "https://git.example.com/_auth/callback" {
		t.Fatalf("public URL = %q", got)
	}
	if got := adminRedirectURI(r, ""); got != "http://example.com/_auth/callback" {
		t.Fatalf("request origin = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := adminRedirectURI(r, ""); got != "https://example.com/_auth/callback" {
		t.Fatalf("forwarded proto = %q", got)
	}
}

// --- branch coverage: write actions, dep failures, not-wired paths ----------

func TestSettingsRepoWriteActions(t *testing.T) {
	var actions []string
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(_ context.Context, _, _ string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		actions = append(actions, w.Action)
		if w.Action == SettingsActionTransfer {
			return SettingsWriteResult{Redirect: "/bob/monorepo"}, nil
		}
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	base := "/acme/monorepo/settings/"

	for _, tc := range []struct {
		path string
		form url.Values
		want string
	}{
		{"mirror", url.Values{"mirror_action": {"put"}, "upstream_url": {"https://u.example/r.git"}}, SettingsActionMirrorPut},
		{"mirror", url.Values{"mirror_action": {"schedule"}, "schedule": {"weekly"}}, SettingsActionMirrorSchedule},
		{"mirror", url.Values{"mirror_action": {"sync"}}, SettingsActionMirrorSync},
		{"mirror", url.Values{"mirror_action": {"remove"}}, SettingsActionMirrorRemove},
		{"mirror", url.Values{"push_action": {"put"}}, SettingsActionPushPut},
		{"mirror", url.Values{"push_action": {"schedule"}}, SettingsActionPushSchedule},
		{"mirror", url.Values{"push_action": {"keygen"}}, SettingsActionPushKeygen},
		{"mirror", url.Values{"push_action": {"sync"}}, SettingsActionPushSync},
		{"mirror", url.Values{"push_action": {"remove"}}, SettingsActionPushRemove},
		{"policy", url.Values{"policy": {"allow all"}}, SettingsActionPolicy},
		{"policy", url.Values{"policy_action": {"dryrun"}, "n": {"5"}}, SettingsActionPolicyDryRun},
		{"config", url.Values{"config": {"a = 1"}}, SettingsActionConfig},
		{"config", url.Values{"config_action": {"clear"}}, SettingsActionConfigClear},
		{"tokens", url.Values{"name": {"ci"}}, SettingsActionTokenCreate},
		{"webhooks", url.Values{"url": {"https://e.example/h"}}, SettingsActionWebhookCreate},
		{"bogus", url.Values{}, SettingsActionGeneral},
	} {
		rec := settingsDo(t, s, http.MethodPost, base+tc.path, tc.form, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d (%s)", tc.path, rec.Code, rec.Body.String())
		}
	}
	// Path-parameterised actions.
	for _, tc := range []struct {
		path string
		form url.Values
		want string
	}{
		{"tokens/t1", url.Values{"token_action": {"revoke"}}, SettingsActionTokenRevoke},
		{"webhooks/w1", url.Values{"webhook_action": {"delete"}, "confirm": {"w1"}}, SettingsActionWebhookDelete},
		{"webhooks/w1", url.Values{"webhook_action": {"ping"}}, SettingsActionWebhookPing},
		{"danger", url.Values{"danger_action": {"transfer"}, "confirm": {"acme/monorepo"}, "owner": {"bob"}}, SettingsActionTransfer},
	} {
		rec := settingsDo(t, s, http.MethodPost, base+tc.path, tc.form, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d (%s)", tc.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Location"); tc.want == SettingsActionTransfer && got != "/bob/monorepo?saved=1" {
			t.Fatalf("transfer Location = %q", got)
		}
	}
	want := []string{
		SettingsActionMirrorPut, SettingsActionMirrorSchedule, SettingsActionMirrorSync, SettingsActionMirrorRemove,
		SettingsActionPushPut, SettingsActionPushSchedule, SettingsActionPushKeygen, SettingsActionPushSync, SettingsActionPushRemove,
		SettingsActionPolicy, SettingsActionPolicyDryRun, SettingsActionConfig, SettingsActionConfigClear,
		SettingsActionTokenCreate, SettingsActionWebhookCreate, SettingsActionGeneral,
		SettingsActionTokenRevoke, SettingsActionWebhookDelete, SettingsActionWebhookPing, SettingsActionTransfer,
	}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v\nwant %v", actions, want)
	}
}

func TestSettingsRepoWriteNotWired(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("admin"))
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", url.Values{"description": {"x"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired write = %d, want 503", rec.Code)
	}
}

func TestSettingsRepoPanelsNotWiredPlural(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("admin"))
	for _, tab := range []string{"scheduled", "mirror", "policy", "config", "access", "tokens", "webhooks"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tab, rec.Code)
		}
		settingsMustContain(t, rec.Body.String(), "not wired", tab+" unwired")
	}
}

func TestSettingsRepoPanelErrorsRenderInline(t *testing.T) {
	f := settingsRepoFakes("admin")
	boom := func() error { return &settingsTestErr{"panel read failed"} }
	f.deps.RepoScheduled = func(context.Context, string, string) (*SettingsScheduled, error) { return nil, boom() }
	f.deps.RepoMirror = func(context.Context, string, string) (*SettingsMirror, error) { return nil, boom() }
	f.deps.RepoPushMirror = func(context.Context, string, string) (*SettingsPushMirror, error) { return nil, boom() }
	f.deps.RepoPolicy = func(context.Context, string, string) (*SettingsPolicy, error) { return nil, boom() }
	f.deps.RepoConfig = func(context.Context, string, string) (*SettingsConfig, error) { return nil, boom() }
	f.deps.RepoAccess = func(context.Context, string, string, auth.Principal) (*SettingsAccess, error) { return nil, boom() }
	f.deps.RepoAdmin = func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) { return nil, boom() }
	s := newSettingsServer(t, f)
	for _, tab := range []string{"scheduled", "mirror", "policy", "config", "access", "tokens"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tab, rec.Code)
		}
		settingsMustContain(t, rec.Body.String(), "panel read failed", tab+" error")
	}
}

func TestSettingsRepoWriteRestoresPolicyAndConfigValues(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Message: "policy rejected"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/policy", url.Values{"policy": {"deny all"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("policy reject = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "deny all", "typed policy preserved")
	settingsMustContain(t, rec.Body.String(), "policy rejected", "policy error")

	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/config", url.Values{"config": {"bad = ["}, "message": {"oops"}}, nil)
	settingsMustContain(t, rec.Body.String(), "bad = [", "typed config preserved")
	settingsMustContain(t, rec.Body.String(), "oops", "typed message preserved")
}

func TestSettingsKeysNotWiredWriteAndDeleteError(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	if rec := settingsDo(t, s, http.MethodPost, "/keys", url.Values{"key": {"ssh-ed25519 AAAA x"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired add = %d, want 503", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/keys/id/delete", url.Values{}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired delete = %d, want 503", rec.Code)
	}

	store := &settingsFakeKeys{}
	s = newSettingsServer(t, &settingsFakes{})
	s.env.API = &api.Env{SSHKeys: store}
	rec := settingsDo(t, s, http.MethodPost, "/keys/unknown/delete", url.Values{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown delete = %d, want 400", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not found", "delete error")

	// Cross-origin key writes are refused before the registry is touched.
	rec = settingsDo(t, s, http.MethodPost, "/keys", url.Values{"key": {"ssh-ed25519 AAAA x"}},
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin key = %d, want 403", rec.Code)
	}
}

func TestAdminConfigExtraBranches(t *testing.T) {
	// Validated + saved query flags render the flashes.
	clean := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
	}}
	s := newSettingsServer(t, clean)
	rec := settingsDo(t, s, http.MethodGet, "/admin/config?validated=1&saved=1", nil, nil)
	settingsMustContain(t, rec.Body.String(), "Valid — nothing was saved.", "validate flash")
	settingsMustNotContain(t, rec.Body.String(), `name="token"`, "no setup token field")

	// The access verdict can arrive on the view (the API's spelling).
	view := adminFakeView()
	view.Access = "admin_required"
	view.Detail = "admin principal required"
	s = newSettingsServer(t, &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return view, nil },
	}})
	rec = settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	settingsMustContain(t, rec.Body.String(), "Access restricted", "view-denied")
	settingsMustContain(t, rec.Body.String(), "admin principal required", "view-denied detail")

	// A schema read error is a 503, never a half page.
	f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) {
		return nil, &settingsTestErr{"schema unavailable"}
	}}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("schema error = %d, want 503", rec.Code)
	}

	// Access refusal on a WRITE is a real 403 (not the restricted card).
	f = &settingsFakes{deps: testDeps{
		Access: func(*http.Request, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "token required"}
		},
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("denied setup write = %d, want 403", rec.Code)
	}

	// Save service failure surfaces as 503.
	f = &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, nil, &settingsTestErr{"disk full"}
		},
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("save failure = %d, want 503", rec.Code)
	}

	// Validate/save with the function unwired are distinct 503s.
	f = &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"validate"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired validate = %d", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired save = %d", rec.Code)
	}

	// The OIDC test endpoint refuses without the probe and across origins.
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test", url.Values{"issuer": {"https://id"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired auth test = %d", rec.Code)
	}
	f = &settingsFakes{deps: testDeps{AuthTest: func(context.Context, string, string, string) (*AdminAuthTest, []AdminError) {
		return &AdminAuthTest{Issuer: "https://id"}, nil
	}}}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/admin/config/auth-test", url.Values{"issuer": {"https://id"}},
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin auth test = %d, want 403", rec.Code)
	}
	// And with no admin principal the probe is still admin-gated.
	f.deps.Access = func(*http.Request, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test", url.Values{"issuer": {"https://id"}}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated auth test = %d, want 401", rec.Code)
	}
}

func TestSettingsOrgExtraBranches(t *testing.T) {
	// Every org action maps to its tab and reaches the seam.
	var got []string
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(_ context.Context, _ string, _ auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		got = append(got, w.Action)
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	for _, action := range []string{"profile", "avatar", "avatar-remove", "member", "member-remove",
		"team", "team-remove", "team-add", "team-del", "invite", "invite-remove"} {
		rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {action}, "id": {"x"}}, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("org action %s = %d (%s)", action, rec.Code, rec.Body.String())
		}
	}
	if len(got) != 11 {
		t.Fatalf("org actions = %v", got)
	}
	// Tab routing per action (Location carries the tab).
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"invite"}}, nil)
	if loc := rec.Header().Get("Location"); loc != "/acme/settings?tab=invitations&notice=saved" {
		t.Fatalf("invite Location = %q", loc)
	}
	// Unknown tab falls back to the profile.
	rec = settingsDo(t, s, http.MethodGet, "/acme/settings?tab=bogus", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown org tab = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Profile", "unknown tab falls back")
	// Preview refuses cross-origin posts.
	rec = settingsDo(t, s, http.MethodPost, "/acme/settings/preview", url.Values{"bio_markdown": {"x"}},
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin preview = %d, want 403", rec.Code)
	}
	// Not wired / not found.
	s = newSettingsServer(t, &settingsFakes{})
	if rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"member"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired org write = %d", rec.Code)
	}
	f = &settingsFakes{deps: testDeps{
		OrgView: func(context.Context, string, auth.Principal) (*SettingsOrgPage, error) {
			return nil, &settingsTestErr{"gone"}
		},
		OrgWrite: func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
			t.Fatal("a missing org must not reach the write seam")
			return SettingsWriteResult{}, nil
		},
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"member"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing org write = %d", rec.Code)
	}
}

func TestSettingsFragmentAndHelpers(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	s.settingsRenderFragment(rec, r, "settings/nope", "whatever", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing fragment set = %d, want 500", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.settingsRenderFragment(rec, r, "admin/config", "admin-no-such-define", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing fragment template = %d, want 500", rec.Code)
	}

	// The write seams' value accessors are part of the contract.
	rw := SettingsRepoWrite{Values: map[string]string{"a": "b"}}
	if rw.Get("a") != "b" || rw.Get("zz") != "" {
		t.Fatal("SettingsRepoWrite.Get")
	}
	ow := SettingsOrgWrite{Values: map[string]string{"c": "d"}}
	if ow.Get("c") != "d" || ow.Get("zz") != "" {
		t.Fatal("SettingsOrgWrite.Get")
	}
	// Unknown tab min-read defaults to read (never a silent admin gate).
	if settingsTabMinRead(settingsRepoTabs, "bogus") != "read" {
		t.Fatal("unknown tab min read")
	}
	if settingsOrgTabVisible("bogus", true) {
		t.Fatal("unknown org tab must not be visible")
	}
	// More spec-spelling shapes.
	for _, tc := range []struct{ in, want string }{
		{"500ms", "500ms"}, {"1.5h", "1.5h"}, {"90m", "90m"}, {"1h30m0s", "90m"}, {"1us", "0.000001s"}, {"12x", "12x"},
	} {
		if got := fmtSpecDuration(tc.in); got != tc.want {
			t.Fatalf("fmtSpecDuration(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSettingsFieldControlsAndQueryHelpers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field AdminConfigField
		want  string
	}{
		{AdminConfigField{Key: "a.b", Type: "bool", Value: "TRUE"}, "bool"},
		{AdminConfigField{Key: "a.c", Type: "enum", Value: "x"}, "select"},
		{AdminConfigField{Key: "a.d", Type: "int"}, "number"},
		{AdminConfigField{Key: "a.e", Type: "float"}, "number"},
		{AdminConfigField{Key: "a.f", Type: "toml"}, "textarea"},
		{AdminConfigField{Key: "a.g", Type: "list"}, "list"},
		{AdminConfigField{Key: "a.h", Type: "globs"}, "list"},
		{AdminConfigField{Key: "a.i", Type: "string", Secret: true}, "password"},
		{AdminConfigField{Key: "a.j", Type: "string"}, "text"},
		{AdminConfigField{Key: "store.backend"}, "select"}, // metadata supplies enum
		{AdminConfigField{Key: "server.auth.admin_groups", Type: "list"}, "list"},
		{AdminConfigField{Key: "server.auth.groups_claim", Type: "string"}, "text"},
		{AdminConfigField{Key: "server.request_timeout", Value: "1h0m0s"}, "text"},
		{AdminConfigField{Key: "cache.max_bytes", Value: "1024"}, "text"},
	} {
		adminPrepareField(&tc.field)
		if tc.field.Control != tc.want {
			t.Fatalf("%s control = %q, want %q", tc.field.Key, tc.field.Control, tc.want)
		}
		if tc.field.InputID != "admin-in-"+strings.ReplaceAll(tc.field.Key, ".", "--") {
			t.Fatalf("%s input id = %q", tc.field.Key, tc.field.InputID)
		}
	}
	// The WP13 keys render with real examples and notes in the setup page.
	for _, tc := range []struct{ key, example, note string }{
		{"server.auth.admin_groups", "spindle-admin", "guaranteed lockout"},
		{"server.auth.groups_claim", "groups", "ID-token claim"},
	} {
		f := AdminConfigField{Key: tc.key}
		adminPrepareField(&f)
		if f.Example != tc.example || !strings.Contains(f.Note, tc.note) {
			t.Fatalf("%s example/note = %q / %q", tc.key, f.Example, f.Note)
		}
	}
	if got := splitLines(""); got != nil {
		t.Fatalf("splitLines empty = %v", got)
	}
	if got := splitLines("a\r\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLines CRLF = %v", got)
	}
}

// TestAdminConfigPackagesQuotaRow: the new instance-default quota renders as a
// real Configuration row (schema-driven), with its size example and the
// "0 = unlimited" consequence stated — the same spelling the validator and
// setup.Coerce accept.
func TestAdminConfigPackagesQuotaRow(t *testing.T) {
	view := &AdminConfigView{Sections: []AdminConfigSection{{Section: "packages"}}}
	for _, g := range setup.BuildSchema(config.Defaults(), config.Defaults()) {
		if g.Section != "packages" {
			continue
		}
		for _, k := range g.Keys {
			view.Sections[0].Essential = append(view.Sections[0].Essential, AdminConfigField{
				Key: k.Key, Value: setup.DisplayValue(k),
			})
		}
	}
	adminPrepareConfig(view)
	var quota *AdminConfigField
	for i := range view.Sections[0].Essential {
		if view.Sections[0].Essential[i].Key == "packages.quota_bytes" {
			quota = &view.Sections[0].Essential[i]
		}
	}
	if quota == nil {
		t.Fatal("packages.quota_bytes is not in the packages schema card")
	}
	if quota.Example != "50GiB" || !strings.Contains(quota.Note, "0 = unlimited") {
		t.Fatalf("quota row = %+v", quota)
	}
	f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) { return view, nil }}}
	s := newSettingsServer(t, f)
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	settingsMustContain(t, body, `data-key="packages.quota_bytes"`, "quota row")
	settingsMustContain(t, body, "0 = unlimited", "quota consequence")
	settingsMustContain(t, body, `value="0B"`, "the unlimited default spelling")
}

// TestSettingsFieldMetaNamesOnlyRealSchemaKeys pins the display table to the
// schema: a dead key (the G18 trio telemetry.metrics_listen, git.max_pack_bytes,
// upstream.allow_import) is gone, and every remaining entry names a key the
// page actually renders.
func TestSettingsFieldMetaNamesOnlyRealSchemaKeys(t *testing.T) {
	t.Parallel()
	keys := map[string]bool{}
	for _, g := range setup.BuildSchema(config.Defaults(), config.Defaults()) {
		for _, f := range g.Keys {
			keys[f.Key] = true
		}
	}
	for key := range adminFieldMeta {
		if !keys[key] {
			t.Fatalf("adminFieldMeta carries dead key %q", key)
		}
	}
	for _, dead := range []string{"telemetry.metrics_listen", "git.max_pack_bytes", "upstream.allow_import"} {
		if _, ok := adminFieldMeta[dead]; ok {
			t.Fatalf("dead key %q is back in adminFieldMeta", dead)
		}
	}
}

// TestAdminConfigPartitionsAdvancedFromMeta pins the G18 grouping: the view
// partitions on the one display-metadata list, so every advanced key lands in
// the collapsed group and every other key stays essential — including when a
// producer puts everything in Essential (the old "0 details" bug).
func TestAdminConfigPartitionsAdvancedFromMeta(t *testing.T) {
	t.Parallel()
	v := &AdminConfigView{Sections: []AdminConfigSection{{Section: "telemetry"}, {Section: "server"}}}
	for _, g := range setup.BuildSchema(config.Defaults(), config.Defaults()) {
		if g.Section != "telemetry" && g.Section != "server" {
			continue
		}
		for _, k := range g.Keys {
			f := AdminConfigField{Key: k.Key, Value: setup.DisplayValue(k)}
			for i := range v.Sections {
				if v.Sections[i].Section == g.Section {
					v.Sections[i].Essential = append(v.Sections[i].Essential, f)
				}
			}
		}
	}
	adminPrepareConfig(v)
	for _, sec := range v.Sections {
		for _, f := range sec.Essential {
			if adminFieldMeta[f.Key].Advanced {
				t.Fatalf("%s is advanced but rendered essential", f.Key)
			}
		}
		for _, f := range sec.Advanced {
			if !adminFieldMeta[f.Key].Advanced {
				t.Fatalf("%s is essential but rendered advanced", f.Key)
			}
		}
		switch sec.Section {
		case "telemetry":
			if len(sec.Essential) != 0 || len(sec.Advanced) == 0 {
				t.Fatalf("telemetry partition = %d essential / %d advanced", len(sec.Essential), len(sec.Advanced))
			}
		case "server":
			var essential bool
			for _, f := range sec.Essential {
				if f.Key == "server.listen" {
					essential = true
				}
			}
			if !essential {
				t.Fatal("server.listen left the essentials")
			}
		}
	}
	// Idempotent: preparing again must not duplicate rows.
	before := len(v.Sections[0].Essential) + len(v.Sections[0].Advanced)
	adminPrepareConfig(v)
	if got := len(v.Sections[0].Essential) + len(v.Sections[0].Advanced); got != before {
		t.Fatalf("re-prepare changed the row count: %d → %d", before, got)
	}
	// A producer that pre-seeded the wrong side is normalised too, so the
	// metadata (not the producer) owns the partition.
	pre := &AdminConfigView{Sections: []AdminConfigSection{{
		Section:   "x",
		Essential: []AdminConfigField{{Key: "server.listen"}},
		Advanced:  []AdminConfigField{{Key: "telemetry.log_format"}, {Key: "server.public_url"}},
	}}}
	adminPrepareConfig(pre)
	if got := len(pre.Sections[0].Essential); got != 2 {
		t.Fatalf("pre-seeded essential count = %d, want 2", got)
	}
	if got := len(pre.Sections[0].Advanced); got != 1 || pre.Sections[0].Advanced[0].Key != "telemetry.log_format" {
		t.Fatalf("pre-seeded advanced = %+v", pre.Sections[0].Advanced)
	}
}

// TestAdminConfigOptions pins the select builder's three shapes: a stored
// value in the set selects its row, an empty value renders an "(unset)" row,
// and a value outside the set renders a "current: …" row so a save cannot
// silently rewrite it.
func TestAdminConfigOptions(t *testing.T) {
	t.Parallel()
	enum := []string{"proxy", "signed_url"}
	got := adminOptions(enum, "signed_url")
	if len(got) != 2 || !got[1].Selected || got[1].Value != "signed_url" {
		t.Fatalf("in-set options = %+v", got)
	}
	got = adminOptions(enum, "")
	if len(got) != 3 || !got[0].Selected || got[0].Value != "" || got[0].Label != "(unset)" {
		t.Fatalf("unset options = %+v", got)
	}
	got = adminOptions(enum, "future")
	if len(got) != 3 || !got[2].Selected || got[2].Value != "future" || !strings.Contains(got[2].Label, "current:") {
		t.Fatalf("custom options = %+v", got)
	}
	// The real schema drives the page's four G18 selects.
	for _, key := range []string{"bundles.serve_via", "compaction.engine", "git.upload_pack_engine", "telemetry.log_format"} {
		f := AdminConfigField{Key: key}
		adminPrepareField(&f)
		if f.Control != "select" || len(f.Options) == 0 {
			t.Fatalf("%s control = %q options = %v", key, f.Control, f.Options)
		}
	}
}

func TestSettingsRepoAccessWriteRestoreAndRedirectGate(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Message: "visibility changed elsewhere"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/access",
		url.Values{"visibility": {"private"}, "new_subject": {"user:zoe"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("access conflict = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "visibility changed elsewhere", "access error")
	settingsMustContain(t, body, `value="private" selected`, "visibility preserved")

	// A gated provider refuses the bare redirect too.
	anon := &settingsFakes{principal: auth.Anonymous()}
	s = newSettingsServer(t, anon)
	s.env.Cfg.Server.Auth.AnonymousRead = false
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bare settings while gated = %d, want 401", rec.Code)
	}
}

func TestAdminConfigAdminFallbackWithoutAccessDep(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, nil, nil
		},
	}})
	// auth mode token + a non-admin principal, no SetupAccess binding: the
	// handler refuses writes and shows the restricted card on reads.
	p := settingsTestPrincipal()
	p.Admin = false
	s.auth = pageAuthFor(p)
	if rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("fallback GET = %d, want 403", rec.Code)
	} else {
		settingsMustContain(t, rec.Body.String(), "Access restricted", "fallback restricted")
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("fallback write = %d, want 403", rec.Code)
	}
	// A host admin passes the fallback.
	p.Admin = true
	s.auth = pageAuthFor(p)
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("admin fallback write = %d, want 303", rec.Code)
	}
}

func TestAdminAuthTestAdminFallback(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: testDeps{
		AuthTest: func(context.Context, string, string, string) (*AdminAuthTest, []AdminError) {
			t.Fatal("the discovery probe ran without authority")
			return nil, nil
		},
	}})
	// The unbound access seam applies the same fallback as GET and save: only
	// an admin may trigger the outbound OIDC discovery probe.
	p := settingsTestPrincipal()
	p.Admin = false
	s.auth = pageAuthFor(p)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test",
		url.Values{"issuer": {"https://id.example.com"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin auth-test = %d, want 403", rec.Code)
	}
}

func TestSettingsOrgPreviewEmptyBody(t *testing.T) {
	s := newSettingsServer(t, settingsOrgFakes(true))
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings/preview", url.Values{}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty preview = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), `id="org-bio-preview"`, "empty preview wrapper")
}

// TestSettingsDumpPages is the browser-evidence hook: with WEBUI_DUMP_DIR set
// it writes the four pages' HTML (real fixture data) so the layout can be
// rendered at 390px and desktop before composition mounts the provider. It is
// a no-op in a normal test run.
func TestSettingsDumpPages(t *testing.T) {
	dir := os.Getenv("WEBUI_DUMP_DIR")
	if dir == "" {
		t.Skip("WEBUI_DUMP_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, rec.Code)
		}
		if err := os.WriteFile(filepath.Join(dir, name), rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	setup := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.AuthMode = "oidc"
		v.PublicURL = "https://git.example.com"
		v.Sections = append(v.Sections, AdminConfigSection{Section: "auth", Essential: []AdminConfigField{
			{Key: "server.auth.mode", Type: "enum", Value: "oidc", Enum: []string{"none", "token", "oidc"}},
			{Key: "server.auth.issuer", Value: "https://id.example.com"},
			{Key: "server.auth.anonymous_read", Type: "bool", Value: "true"},
			{Key: "server.auth.session_secret", Secret: true, Value: ""},
		}})
		return v, nil
	}}}
	s := newSettingsServer(t, setup)
	write("setup.html", settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil))

	repo := settingsRepoFakes("admin")
	repo.deps.RepoMirror = func(context.Context, string, string) (*SettingsMirror, error) {
		return &SettingsMirror{Configured: true, Upstream: "https://up.example/acme/monorepo.git",
			NextSync: "next sync in 3h", LastSynced: "2026-11-19 08:00 UTC", LastResult: "ok",
			Schedule: "daily", Failures: 0}, nil
	}
	repo.deps.RepoAccess = func(context.Context, string, string, auth.Principal) (*SettingsAccess, error) {
		return &SettingsAccess{Role: "admin", Visibility: "public", Version: 7, IsOrg: false,
			Collaborators: []SettingsCollaborator{{Principal: "user:alice", Role: "admin", Source: "binding"}},
			Bindings:      []SettingsBinding{{Subject: "user:bob", Role: "write"}}}, nil
	}
	s = newSettingsServer(t, repo)
	for _, tab := range []string{"general", "mirror", "access", "tokens", "danger"} {
		write("repo-"+tab+".html", settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil))
	}

	org := settingsOrgFakes(true)
	s = newSettingsServer(t, org)
	for _, tab := range []string{"profile", "members", "teams", "invitations", "activity", "danger"} {
		write("org-"+tab+".html", settingsDo(t, s, http.MethodGet, "/acme/settings?tab="+tab, nil, nil))
	}

	keys := &settingsFakes{}
	s = newSettingsServer(t, keys)
	s.env.API = &api.Env{SSHKeys: &settingsFakeKeys{rows: []api.SshKeyRecord{
		{ID: "k1", Title: "laptop", Key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE ada@laptop", CreatedAt: time.Now().Add(-3 * time.Hour)},
		{ID: "k2", Title: "", Key: "ssh-rsa AAAAB3NzaC1yc2EAAAADAQAB ci@build", CreatedAt: time.Now().Add(-30 * 24 * time.Hour)},
	}}}
	write("keys.html", settingsDo(t, s, http.MethodGet, "/keys", nil, nil))
}

// /setup access is the admin flag (WP13): group admins see the form, a
// signed-in non-admin gets the restricted card + a 403 on writes, and the
// card names the rule (server.auth.admin_groups) without leaking the configured
// group values or issuer.
func TestAdminConfigAdminGroupAccess(t *testing.T) {
	rule := func(_ *http.Request, p auth.Principal) *auth.AuthError {
		if p.Anonymous {
			return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required for setup"}
		}
		if !p.Admin {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "setup requires an admin principal (an OIDC admin group, admin email, or admin domain)"}
		}
		return nil
	}
	deps := testDeps{AdminDeps: AdminDeps{
		Access:     rule,
		ConfigView: func(context.Context) (*AdminConfigView, error) { return adminFakeView(), nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{Saved: true}, nil, nil
		},
	}}

	// A group admin (Principal.Admin is what the OIDC claim derivation sets)
	// sees the editable form.
	s := newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "dev", Email: "dev@example.com", Write: true, Admin: true}})
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group admin GET /admin/config = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `name="cfg.server.listen"`, "admin sees the form")
	settingsMustNotContain(t, body, "Access restricted", "admin is not denied")

	// A signed-in non-admin gets the restricted card with a real 403, and the
	// card names the group rule, not a token.
	s = newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "zoe", Email: "zoe@example.com", Write: true}})
	rec = settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /admin/config = %d, want the restricted card at 403", rec.Code)
	}
	body = rec.Body.String()
	settingsMustContain(t, body, "Access restricted", "non-admin denied")
	settingsMustContain(t, body, "verified groups do not grant administration", "card explains the signed-in account is not an admin")
	settingsMustContain(t, body, "server.auth.admin_groups", "card names the config key")
	settingsMustNotContain(t, body, "SPINDLE_SETUP_TOKEN", "no setup token text")
	settingsMustNotContain(t, body, `name="cfg.server.listen"`, "non-admin has no form")
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin POST /admin/config = %d, want 403", rec.Code)
	}

	// Anonymous is a 401 (unauthenticated), not a 403 — and the GET carries
	// the restricted card, not a bare error page.
	s = newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "anonymous", Anonymous: true}})
	rec = settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /admin/config = %d, want 401", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Access restricted", "anonymous card content")
	if rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST /admin/config = %d, want 401", rec.Code)
	}
}

// TestAdminConfigDeniedRecoveryCard: the denied card shows the caller's OWN
// live groups (never the configured admin group or issuer — PLAN-ADMIN M4),
// states the file/env recovery verbatim, and never offers a token-paste form
// (static tokens cannot administer).
func TestAdminConfigDeniedRecoveryCard(t *testing.T) {
	deny := func(_ *http.Request, p auth.Principal) *auth.AuthError {
		if p.Anonymous {
			return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required for setup"}
		}
		if !p.Admin {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "setup requires an admin principal"}
		}
		return nil
	}
	deps := testDeps{Access: deny}
	anon := auth.Principal{Name: "anonymous", Anonymous: true}

	t.Run("anonymous gets the card at 401", func(t *testing.T) {
		s := newSettingsServer(t, &settingsFakes{deps: deps, principal: anon})
		rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET /admin/config = %d, want 401", rec.Code)
		}
		body := rec.Body.String()
		settingsMustContain(t, body, "Access restricted", "denied card")
		settingsMustContain(t, body, "There is no unauthenticated back door.", "recovery verbatim")
		settingsMustContain(t, body, "SPINDLE__SERVER__AUTH__", "env recovery named")
		settingsMustContain(t, body, "/_auth/login", "sign-in link")
		settingsMustNotContain(t, body, `action="/_auth/token"`, "no token form")
		settingsMustNotContain(t, body, `name="cfg.server.listen"`, "no config form while denied")
	})

	t.Run("signed-in non-admin sees its live groups", func(t *testing.T) {
		s := newSettingsServer(t, &settingsFakes{deps: deps,
			principal: auth.Principal{Name: "zoe", Write: true, Groups: []string{"team-x"}}})
		rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
		body := rec.Body.String()
		settingsMustContain(t, body, "team-x", "live groups")
		settingsMustContain(t, body, "verified groups do not grant administration", "verdict line")
	})
}

// TestSettingsRepoProtectionTabRenders is the Branch protection page
// contract after the ruleset rework (PLAN-V2 §2.4): the tab renders the
// ruleset list (not one form for one rule pair), the computed
// effective-rules view, the read-only rule table, and the collapsed raw
// editor.
func TestSettingsRepoProtectionTabRenders(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoProtection = func(context.Context, string, string) (*SettingsProtection, error) {
		return settingsProtectionView([]byte(`{"version":1,"rules":[
			{"name":"protection:main-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"bypass":["role:acme/monorepo:admin"],"require_checks":["ci/build"],"merge_methods":["squash"]}}},
			{"name":"protection:main-reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2}}},
			{"name":"release-lock","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"restricts":["delete"]}}}
		]}`), "acme", "monorepo"), nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET protection = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Branch protection", "tab heading")
	settingsMustContain(t, body, `id="settings-panel"`, "panel")
	settingsMustContain(t, body, `aria-current="page"`, "active nav")
	settingsMustContain(t, body, `href="/acme/monorepo/settings/protection"`, "nav link")
	// The list: one row per named ruleset, reachable for editing.
	settingsMustContain(t, body, "Protection rulesets", "ruleset list heading")
	settingsMustContain(t, body, "New ruleset", "new ruleset affordance")
	settingsMustContain(t, body, `href="/acme/monorepo/settings/protection/main"`, "ruleset edit link")
	settingsMustContain(t, body, "protects create, update", "ruleset summary chip")
	settingsMustContain(t, body, "merge methods: squash", "merge-method chip")
	settingsMustContain(t, body, "2 approvals", "reviews chip")
	// The effective view answers for a ref, computed by the engine.
	settingsMustContain(t, body, "What applies to a ref", "effective view")
	settingsMustContain(t, body, "protected by &#39;protection:main-push&#39;", "computed push verdict")
	settingsMustContain(t, body, "<dd class=\"mono text-xs\">ci/build</dd>", "required-checks row")
	settingsMustContain(t, body, "2 (rule <span class=\"mono text-xs\">protection:main-reviews</span>)", "approvals row")
	settingsMustContain(t, body, "<dd class=\"mono text-xs\">squash</dd>", "permitted methods row")
	// The read-only table carries every rule, including the unmanaged one.
	settingsMustContain(t, body, "release-lock", "unmanaged rule row")
	settingsMustContain(t, body, "protection:main-push", "managed rule row")
	settingsMustContain(t, body, "Advanced: edit the full policy JSON", "advanced editor")
	// The advanced editor is collapsed: raw JSON is never the primary path.
	if strings.Contains(body, `<textarea class="input mono text-xs" id="protection-raw" name="policy" rows="14" spellcheck="false" data-submit-keys>`+"\n"+`{"version"`) {
		t.Fatal("raw JSON rendered expanded as the primary editor")
	}
	for _, bad := range []string{`class="grid grid-cols-2"`, `class="grid grid-cols-3"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("bare multi-column grid at 390px: %s", bad)
		}
	}
}

// TestSettingsRepoUnknownRepoIs404 pins audit F3: the settings page mirrors
// the API's 404 for a repo that does not exist. Before the fix the render
// returned 200 with a full settings shell and an inline "not found".
func TestSettingsRepoUnknownRepoIs404(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("admin"))
	s.env.API = &api.Env{Repos: &collabFakeRegistry{exists: map[string]bool{"acme/monorepo": true}}}
	rec := settingsDo(t, s, http.MethodGet, "/nope/nothing/settings/general", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown repo settings = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	settingsMustNotContain(t, body, `id="settings-panel"`, "no settings shell")
	settingsMustNotContain(t, body, `aria-label="Settings sections"`, "no settings nav")
	// The known repo still renders.
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("known repo settings = %d, want 200", rec.Code)
	}
	// A registry failure is a 503, never a fabricated 200.
	s.env.API.Repos = &repoFakeRegistry{err: &settingsTestErr{"registry down"}}
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("registry failure = %d, want 503", rec.Code)
	}
}

// --- org webhooks (WP6.2) ---------------------------------------------------

// TestSettingsOrgWebhooksTab pins the org Webhooks tab: owner-only visibility,
// the canonical panel (create form, one-time secret state, deliveries with
// timings, redeliver only when retained, typed-confirm delete).
func TestSettingsOrgWebhooksTab(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgView = func(_ context.Context, org string, _ auth.Principal) (*SettingsOrgPage, error) {
		return &SettingsOrgPage{
			Org: org, CanManage: true,
			WebhooksWired: true,
			Webhooks: []SettingsWebhook{{
				ID: "h1", URL: "https://h.example/x", Events: "commented", Active: true, SecretSet: true,
				Recent: []SettingsDelivery{
					{Seq: "3", SeqNum: 3, Event: "ping", Status: 200, At: "2026-11-01T00:00:00Z", DurationMs: 4, Redeliverable: true},
					{Seq: "2", SeqNum: 2, Event: "commented", Status: 200, At: "2026-11-01T00:00:00Z"},
				},
			}},
		}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings?tab=webhooks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhooks tab = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Add a webhook", "create form")
	settingsMustContain(t, body, "•••••• (set)", "secret shown once as set")
	settingsMustContain(t, body, "recent deliveries", "deliveries table")
	settingsMustContain(t, body, "4ms", "duration")
	settingsMustContain(t, body, "redeliver", "retained row affordance")
	settingsMustContain(t, body, "payload not retained", "bodyless row is honest")
	settingsMustContain(t, body, `name="org_action" value="webhook-create"`, "create plumbing")
	settingsMustContain(t, body, `name="org_action" value="webhook-toggle"`, "toggle plumbing")
	settingsMustContain(t, body, `data-danger-expected="h1"`, "typed confirm")

	// A non-owner never sees the tab (and the direct URL is 403).
	g := settingsOrgFakes(false)
	gs := newSettingsServer(t, g)
	rec = settingsDo(t, gs, http.MethodGet, "/acme/settings", nil, nil)
	settingsMustNotContain(t, rec.Body.String(), "Webhooks", "owner-only tab")
	if rec := settingsDo(t, gs, http.MethodGet, "/acme/settings?tab=webhooks", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner webhooks = %d, want 403", rec.Code)
	}
}

// TestSettingsOrgWebhookWrites pins every tab action through the seam,
// including the delete confirmation and the unwired degrade.
func TestSettingsOrgWebhookWrites(t *testing.T) {
	var got SettingsOrgWrite
	f := settingsOrgFakes(true)
	f.deps.OrgWebhookWrite = func(_ context.Context, org string, _ auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		got = w
		if org != "acme" {
			t.Fatalf("org = %q", org)
		}
		return SettingsWriteResult{Notice: "redelivered #3 → 200"}, nil
	}
	s := newSettingsServer(t, f)
	for _, tc := range []struct{ action, want string }{
		{"webhook-create", "webhook-create"},
		{"webhook-toggle", "webhook-toggle"},
		{"webhook-rotate", "webhook-rotate"},
		{"webhook-ping", "webhook-ping"},
		{"webhook-redeliver", "webhook-redeliver"},
	} {
		rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
			"org_action": {tc.action}, "id": {"h1"}, "seq": {"3"},
		}, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d (%s)", tc.action, rec.Code, rec.Body.String())
		}
		if got.Action != tc.want || got.Get("id") != "h1" {
			t.Fatalf("%s write = %+v", tc.action, got)
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/settings?tab=webhooks&notice=redelivered+%233+%E2%86%92+200" {
			t.Fatalf("%s Location = %q", tc.action, loc)
		}
	}

	// Delete requires the exact hook id (server-side, not just the island).
	f.deps.OrgWebhookWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("unconfirmed delete reached the seam")
		return SettingsWriteResult{}, nil
	}
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {"webhook-delete"}, "id": {"h1"}, "confirm": {"h"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong confirm = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "type h1 exactly to confirm", "confirm error")

	// Unwired seam renders the honest 503, never a silent success.
	g := settingsOrgFakes(true)
	gs := newSettingsServer(t, g)
	rec = settingsDo(t, gs, http.MethodPost, "/acme/settings", url.Values{"org_action": {"webhook-create"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired org webhooks = %d, want 503", rec.Code)
	}
}

// TestSettingsRepoWebhookExtraActions pins toggle/rotate/redeliver plumbing
// on the repo tab (the actions WP6.2 adds to the shared row).
func TestSettingsRepoWebhookExtraActions(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{"toggle", SettingsActionWebhookToggle},
		{"rotate", SettingsActionWebhookRotate},
		{"redeliver", SettingsActionWebhookRedeliver},
	} {
		var got SettingsRepoWrite
		f := &settingsFakes{deps: testDeps{
			RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return "admin", nil },
			RepoWrite: func(_ context.Context, _, _ string, _ auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
				got = w
				return SettingsWriteResult{Notice: "done"}, nil
			},
		}}
		s := newSettingsServer(t, f)
		rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/webhooks/h1",
			url.Values{"webhook_action": {tc.action}, "seq": {"3"}, "secret": {"s"}}, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d (%s)", tc.action, rec.Code, rec.Body.String())
		}
		if got.Action != tc.want || got.Get("id") != "h1" {
			t.Fatalf("%s = %+v", tc.action, got)
		}
	}
}

// TestSettingsOrgWriteActionMatrix pins every org form action → seam action
// and its tab (WP9 G2: the page renders these, so they must round-trip).
func TestSettingsOrgWriteActionMatrix(t *testing.T) {
	cases := []struct {
		action, tab string
		values      url.Values
	}{
		{SettingsOrgProfileSave, "profile", url.Values{"display_name": {"Acme"}}},
		{SettingsOrgAvatar, "profile", url.Values{}},
		{SettingsOrgAvatarRm, "profile", url.Values{}},
		{SettingsOrgMemberSet, "members", url.Values{"principal": {"bob"}, "role": {"member"}}},
		{SettingsOrgMemberRm, "members", url.Values{"principal": {"bob"}}},
		{SettingsOrgTeamSave, "teams", url.Values{"slug": {"devs"}, "name": {"Devs"}, "description": {"d"}}},
		{SettingsOrgTeamUpdate, "teams", url.Values{"slug": {"devs"}, "name": {"Devs2"}}},
		{SettingsOrgTeamAdd, "teams", url.Values{"slug": {"devs"}, "principal": {"bob"}}},
		{SettingsOrgTeamMemberRm, "teams", url.Values{"slug": {"devs"}, "principal": {"bob"}}},
		{SettingsOrgTeamDel, "teams", url.Values{"slug": {"devs"}}},
		{SettingsOrgInviteMake, "invitations", url.Values{"subject": {"jane@example.com"}, "role": {"member"}}},
		{SettingsOrgInviteRm, "invitations", url.Values{"id": {"inv1"}}},
	}
	for _, tc := range cases {
		var got SettingsOrgWrite
		f := settingsOrgFakes(true)
		f.deps.OrgWrite = func(_ context.Context, org string, _ auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
			got = w
			return SettingsWriteResult{Notice: "saved"}, nil
		}
		s := newSettingsServer(t, f)
		form := url.Values{"org_action": {tc.action}}
		for k, vs := range tc.values {
			form[k] = vs
		}
		rec := settingsDo(t, s, http.MethodPost, "/acme/settings", form, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d (%s)", tc.action, rec.Code, rec.Body.String())
		}
		if got.Action != tc.action {
			t.Fatalf("%s seam action = %q", tc.action, got.Action)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tab="+tc.tab) {
			t.Fatalf("%s Location = %q, want tab=%s", tc.action, loc, tc.tab)
		}
	}
}

// TestSettingsOrgDeleteRedirectsHome pins the danger action's exit: the org
// is gone, so the page must not redirect back into it.
func TestSettingsOrgDeleteRedirectsHome(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{Notice: "organization deleted", Redirect: "/"}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"delete"}, "confirm": {"acme"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?notice=organization+deleted" {
		t.Fatalf("delete = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestSettingsOrgTeamCardsRoundTripForms pins the team card controls the
// audit named API-only (rename/description, per-member remove) plus the
// avatar remove control.
func TestSettingsOrgTeamCardsRoundTripForms(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgView = func(_ context.Context, org string, _ auth.Principal) (*SettingsOrgPage, error) {
		return &SettingsOrgPage{
			Org: org, CanManage: true,
			Profile: &SettingsOrgProfile{DisplayName: "Acme", AvatarURL: "/api/v1/orgs/acme/avatar"},
			Teams: []SettingsOrgTeam{{
				Slug: "devs", Name: "Devs", Description: "builds it", Members: []string{"user:alice", "user:bob"},
			}},
		}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings?tab=teams", nil, nil)
	body := rec.Body.String()
	for _, needle := range []string{
		`value="team-update"`, `value="team-member-remove"`, "builds it",
		`name="description"`, "user:bob",
	} {
		settingsMustContain(t, body, needle, "team card")
	}
	rec = settingsDo(t, s, http.MethodGet, "/acme/settings?tab=profile", nil, nil)
	settingsMustContain(t, rec.Body.String(), `value="avatar-remove"`, "avatar remove control")
}
