package webui

// settings_test.go — the settings group's httptest surface: /setup, the repo
// settings tabs and their authority gates, /{org}/settings, and /keys.
//
// The template set is built from THIS group's files only (base.html + the
// settings partials + each settings page), and the routes are mounted with
// mountSettings directly, so a sibling migration group's half-written file
// cannot make these tests red. BindSettings is process-wide (server.go
// documents one provider per process); no test runs in parallel.

import (
	"context"
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
	deps      SettingsDeps
	principal auth.Principal
}

func settingsTestPrincipal() auth.Principal {
	return auth.Principal{Name: "alice", Email: "alice@example.com", Write: true, Admin: true}
}

// newSettingsServer builds a page provider with only this group's templates
// and routes. Every dependency defaults to nil ("not wired").
func newSettingsServer(t *testing.T, f *settingsFakes) *Server {
	t.Helper()
	cfg := config.Defaults()
	// A real auth mode: identity-owned pages (/keys) render only for a
	// signed-in principal, and auth none deliberately shows the shared
	// "authentication is disabled" notice instead (tested separately).
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.AnonymousRead = true
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
	deps := SettingsDeps{}
	if f != nil {
		deps = f.deps
	}
	BindSettings(deps)
	t.Cleanup(func() { BindSettings(SettingsDeps{}) })
	return s
}

func pageAuthFor(p auth.Principal) pages.Auth {
	return pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
}

// settingsTestTemplates parses base + this group's partials + each page.
func settingsTestTemplates(t *testing.T, s *Server) map[string]*template.Template {
	t.Helper()
	partials, err := fs.Glob(tplFS, "templates/partials/settings_*.html")
	if err != nil || len(partials) == 0 {
		t.Fatalf("settings partials: %v (%d)", err, len(partials))
	}
	// base.html composes the shared header/footer, the repository chrome
	// (repo_header/repo_tabs) it renders for a repo-scoped page, and the
	// identity notice; they are foundation/shared partials, not another
	// group's.
	shared := []string{
		"templates/partials/header.html",
		"templates/partials/footer.html",
		"templates/partials/repo_header.html",
		"templates/partials/repo_tabs.html",
		"templates/partials/identity_notice.html",
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
	for _, page := range []string{"setup", "repo", "org", "keys"} {
		files := append([]string{"templates/base.html"}, shared...)
		files = append(files, partials...)
		files = append(files, "templates/pages/settings/"+page+".html")
		tpl, err := template.New("base").Funcs(s.funcs()).ParseFS(tplFS, files...)
		if err != nil {
			t.Fatalf("parse settings/%s: %v", page, err)
		}
		out["settings/"+page] = tpl
	}
	return out
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

// settingsFakeView is the schema the setup tests render.
func settingsFakeView() *SettingsSetupView {
	return &SettingsSetupView{
		FileState: "absent",
		AuthMode:  "none",
		Sections: []SettingsSetupSection{
			{Section: "server", Essential: []SettingsSetupField{
				{Key: "server.listen", Type: "string", Value: "0.0.0.0:8080", Default: "0.0.0.0:8080"},
				{Key: "server.request_timeout", Type: "duration", Value: "1h0m0s", Default: "1h0m0s"},
			}, Advanced: []SettingsSetupField{
				{Key: "server.http2", Type: "bool", Value: "true", Default: "true"},
			}},
			{Section: "cache", Essential: []SettingsSetupField{
				{Key: "cache.max_bytes", Type: "size", Value: "21474836480", Default: "21474836480"},
			}},
		},
	}
}

// --- /setup ----------------------------------------------------------------

func TestSettingsSetupRendersSchemaSections(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) {
		return settingsFakeView(), nil
	}}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", rec.Code)
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
	settingsMustContain(t, body, `hx-post="/setup"`, "htmx hook")
}

func TestSettingsSetupDeniedRendersRestrictedCard(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupAccess: func(*http.Request, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin principal required"}
		},
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup (denied) = %d, want 200 restricted card", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Access restricted", "denied page")
	settingsMustContain(t, body, "admin principal required", "denied detail")
	settingsMustNotContain(t, body, `name="cfg.server.listen"`, "denied page")
}

func TestSettingsSetupSaveSuccessRedirectsWithRestartKeys(t *testing.T) {
	var saved map[string]string
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(_ context.Context, values map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			saved = values
			return SettingsSetupResult{Saved: true, RequiresRestart: []string{"server.listen", "cache.max_bytes"}}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)
	form := url.Values{
		"op":                  {"save"},
		"cfg.server.listen":   {"0.0.0.0:9090"},
		"cfg.server.http2":    {"false", "true"}, // hidden false + checked true
		"cfg.cache.max_bytes": {"32GiB"},
	}
	rec := settingsDo(t, s, http.MethodPost, "/setup", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup = %d, want 303 (%s)", rec.Code, rec.Body.String())
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

func TestSettingsSetupValidationErrorReRendersTypedValues(t *testing.T) {
	errs := []SettingsSetupError{{Key: "server.request_timeout", Message: "server.request_timeout: not a duration"}}
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupValidate: func(_ context.Context, values map[string]string) ([]SettingsSetupError, error) {
			if values["server.request_timeout"] != "1 fortnight" {
				t.Fatalf("validator saw %q", values["server.request_timeout"])
			}
			return errs, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{
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

// TestSettingsSetupMasksStoredSecrets pins UI-04: a stored secret (session,
// OAuth client, webhook, S3 key) never appears in the page body in any auth
// mode — the row shows the mask and is editable by overwrite only.
func TestSettingsSetupMasksStoredSecrets(t *testing.T) {
	const secret = "s3cr3t-stored-value-do-not-render"
	for _, mode := range []string{"none", "token", "oidc"} {
		t.Run(mode, func(t *testing.T) {
			f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) {
				v := settingsFakeView()
				v.Sections[0].Essential = append(v.Sections[0].Essential,
					SettingsSetupField{Key: "server.auth.session_secret", Type: "string", Value: secret, Secret: true},
					SettingsSetupField{Key: "server.auth.issuer", Type: "url", Value: "https://id.example.com"},
				)
				return v, nil
			}}}
			s := newSettingsServer(t, f)
			s.env.Cfg.Server.Auth.Mode = mode
			rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /setup = %d", rec.Code)
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

// TestSettingsSetupRejectNeverEchoesSecrets: a rejected save must not bounce a
// typed secret back into the re-rendered form.
func TestSettingsSetupRejectNeverEchoesSecrets(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) {
			v := settingsFakeView()
			v.Sections[0].Essential = append(v.Sections[0].Essential,
				SettingsSetupField{Key: "server.auth.session_secret", Type: "string", Secret: true})
			return v, nil
		},
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, []SettingsSetupError{{Key: "server.listen", Message: "not a listen address"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{
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

func TestSettingsSetupSaveReportsValidationErrors(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, []SettingsSetupError{{Message: "server.auth.issuer: required"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save validation = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "server.auth.issuer: required", "save error")
}

func TestSettingsSetupHTMXErrorSwaps(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, []SettingsSetupError{{Key: "server.listen", Message: "not a listen address"}}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx error = %d, want 200 (htmx swaps only 2xx)", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not a listen address", "htmx error body")
}

func TestSettingsSetupCrossOriginWriteRefused(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			t.Fatal("cross-origin write must not reach the service")
			return SettingsSetupResult{}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", rec.Code)
	}
	// No origin header at all is also refused.
	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, map[string]string{"Origin": ""})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("origin-less write = %d, want 403", rec.Code)
	}
}

func TestSettingsSetupAuthTestFragment(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupAuthTest: func(_ context.Context, issuer, clientID, redirect string) (*SettingsSetupAuthTest, []SettingsSetupError) {
			if issuer != "https://id.example.com" || clientID != "spindle" {
				t.Fatalf("auth test saw %q %q", issuer, clientID)
			}
			return &SettingsSetupAuthTest{
				Issuer: issuer, AuthorizationEndpoint: issuer + "/auth",
				TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks", ClientIDPresent: true,
			}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test", url.Values{
		"issuer": {"https://id.example.com"}, "client_id": {"spindle"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth test = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Discovery OK", "auth test")
	settingsMustContain(t, body, "jwks", "auth test endpoints")
}

func TestSettingsSetupAuthTestErrors(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{
		SetupAuthTest: func(context.Context, string, string, string) (*SettingsSetupAuthTest, []SettingsSetupError) {
			return nil, []SettingsSetupError{{Key: "server.auth.issuer", Message: "discovery fetch failed"}}
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test", url.Values{"issuer": {"https://nope.invalid"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth test errors = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "discovery fetch failed", "auth test error")
}

func TestSettingsSetupNotWired(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{})
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired setup = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired setup")

	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired save = %d, want 503", rec.Code)
	}
}

func TestSettingsSetupOnlyBanner(t *testing.T) {
	view := settingsFakeView()
	view.FileState = "invalid"
	view.Errors = []SettingsSetupError{{Key: "store.s3.bucket", Message: "bucket is required"}}
	f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) { return view, nil }}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	body := rec.Body.String()
	settingsMustContain(t, body, "Setup-only mode — the config file is invalid.", "setup-only banner")
	settingsMustContain(t, body, "bucket is required", "boot errors")
}

// --- repo settings ---------------------------------------------------------

func settingsRepoFakes(role string) *settingsFakes {
	return &settingsFakes{deps: SettingsDeps{
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
		"Effective config &amp; history", "Access", "CI tokens", "Webhooks", "WAL", "Danger Zone"}
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
	for _, tab := range []string{"tokens", "webhooks", "policy", "config", "danger"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s as reader = %d, want 403", tab, rec.Code)
		}
	}
	// Access is triage-gated, general is readable.
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/access", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("access as reader = %d, want 403", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("general as reader = %d, want 200", rec.Code)
	}
	// A reader's nav never shows the admin-only entries (hidden, not 403'd).
	body := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil).Body.String()
	settingsMustNotContain(t, body, "/settings/tokens", "reader nav")
	settingsMustNotContain(t, body, "/settings/danger", "reader nav")
	settingsMustContain(t, body, "/settings/mirror", "reader nav")
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

func TestSettingsRepoNoRoleIsNotFound(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("none"))
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("role none = %d, want 404", rec.Code)
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
			Strategies:  []SettingsStrategy{{Name: "history", Kind: "history", Schedule: "daily", Next: "2026-11-20T00:00:00Z"}},
			HostFacts:   []SettingsKV{{Key: "host", Value: "node-1"}, {Key: "roles", Value: "serve, maintain"}},
			UpstreamGit: "https://up.example/acme/mono.git",
			FollowLine:  "following every 5m",
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
		{"scheduled", []string{"Bundle strategies", "history", "Placement &amp; host facts", "node-1", "following every 5m"}},
		{"policy", []string{"Push policy", "protect default", "Dry run:", "2"}},
		{"config", []string{"Effective config", "Publish message", "History", "#3", "tighten", "- b"}},
		{"access", []string{"Effective access", "user:alice", "Role bindings", "user:bob", "version 7"}},
		{"tokens", []string{"Create a CI token", "ci", "revoke"}},
		{"webhooks", []string{"Add a webhook", "https://example.com/hook", "ping", "delivered"}},
		{"wal", []string{"WAL", "wal-panel.html"}},
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
	f := &settingsFakes{deps: SettingsDeps{}}
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
	return &settingsFakes{deps: SettingsDeps{
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
		"org_action": {"invite"}, "email": {"jane@example.com"}, "role": {"member"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite = %d, want 200 (bearer link must not enter a URL)", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "/invitations?token=abc123", "invite link")
	settingsMustContain(t, body, `id="invite-link"`, "copy target")
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

	f := &settingsFakes{deps: SettingsDeps{OrgView: func(context.Context, string, auth.Principal) (*SettingsOrgPage, error) {
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
	if !settingsRoleAtLeast("admin", "triage") || settingsRoleAtLeast("read", "triage") {
		t.Fatal("role ordering")
	}
	if settingsRoleRank("") != 5 {
		t.Fatal("unwired role must not deny (the server gate already ran)")
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
	if len(nav) != 4 {
		t.Fatalf("non-owner nav = %d entries, want 4", len(nav))
	}
	if len(settingsOrgNav("acme", "profile", true)) != 6 {
		t.Fatal("owner nav")
	}
}

func TestSettingsSameOrigin(t *testing.T) {
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
	r := httptest.NewRequest(http.MethodGet, "http://example.com/setup", nil)
	if got := settingsRedirectURI(r, "https://git.example.com/"); got != "https://git.example.com/_auth/callback" {
		t.Fatalf("public URL = %q", got)
	}
	if got := settingsRedirectURI(r, ""); got != "http://example.com/_auth/callback" {
		t.Fatalf("request origin = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := settingsRedirectURI(r, ""); got != "https://example.com/_auth/callback" {
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
		{"webhooks/w1", url.Values{"webhook_action": {"delete"}}, SettingsActionWebhookDelete},
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

func TestSettingsSetupExtraBranches(t *testing.T) {
	// Validated + saved query flags render the flashes.
	clean := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
	}}
	s := newSettingsServer(t, clean)
	rec := settingsDo(t, s, http.MethodGet, "/setup?validated=1&saved=1", nil, nil)
	settingsMustContain(t, rec.Body.String(), "Valid — nothing was saved.", "validate flash")
	settingsMustNotContain(t, rec.Body.String(), `name="token"`, "no setup token field")

	// The access verdict can arrive on the view (the API's spelling).
	view := settingsFakeView()
	view.Access = "admin_required"
	view.Detail = "admin principal required"
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return view, nil },
	}})
	rec = settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	settingsMustContain(t, rec.Body.String(), "Access restricted", "view-denied")
	settingsMustContain(t, rec.Body.String(), "admin principal required", "view-denied detail")

	// A schema read error is a 503, never a half page.
	f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) {
		return nil, &settingsTestErr{"schema unavailable"}
	}}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("schema error = %d, want 503", rec.Code)
	}

	// Access refusal on a WRITE is a real 403 (not the restricted card).
	f = &settingsFakes{deps: SettingsDeps{
		SetupAccess: func(*http.Request, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "token required"}
		},
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("denied setup write = %d, want 403", rec.Code)
	}

	// Save service failure surfaces as 503.
	f = &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, nil, &settingsTestErr{"disk full"}
		},
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("save failure = %d, want 503", rec.Code)
	}

	// Validate/save with the function unwired are distinct 503s.
	f = &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
	}}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"validate"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired validate = %d", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired save = %d", rec.Code)
	}

	// The OIDC test endpoint refuses without the probe and across origins.
	if rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test", url.Values{"issuer": {"https://id"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired auth test = %d", rec.Code)
	}
	f = &settingsFakes{deps: SettingsDeps{SetupAuthTest: func(context.Context, string, string, string) (*SettingsSetupAuthTest, []SettingsSetupError) {
		return &SettingsSetupAuthTest{Issuer: "https://id"}, nil
	}}}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/setup/auth-test", url.Values{"issuer": {"https://id"}},
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin auth test = %d, want 403", rec.Code)
	}
	// And with no admin principal the probe is still admin-gated.
	f.deps.SetupAccess = func(*http.Request, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	s = newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test", url.Values{"issuer": {"https://id"}}, nil); rec.Code != http.StatusUnauthorized {
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
	f = &settingsFakes{deps: SettingsDeps{
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
	s.settingsRenderFragment(rec, r, "settings/setup", "settings-no-such-define", nil)
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
	for _, tc := range []struct {
		field SettingsSetupField
		want  string
	}{
		{SettingsSetupField{Key: "a.b", Type: "bool", Value: "TRUE"}, "bool"},
		{SettingsSetupField{Key: "a.c", Type: "enum", Value: "x"}, "select"},
		{SettingsSetupField{Key: "a.d", Type: "int"}, "number"},
		{SettingsSetupField{Key: "a.e", Type: "float"}, "number"},
		{SettingsSetupField{Key: "a.f", Type: "toml"}, "textarea"},
		{SettingsSetupField{Key: "a.g", Type: "list"}, "list"},
		{SettingsSetupField{Key: "a.h", Type: "globs"}, "list"},
		{SettingsSetupField{Key: "a.i", Type: "string", Secret: true}, "password"},
		{SettingsSetupField{Key: "a.j", Type: "string"}, "text"},
		{SettingsSetupField{Key: "server.auth.mode"}, "select"}, // metadata supplies enum
		{SettingsSetupField{Key: "server.auth.admin_groups", Type: "list"}, "list"},
		{SettingsSetupField{Key: "server.auth.groups_claim", Type: "string"}, "text"},
		{SettingsSetupField{Key: "server.request_timeout", Value: "1h0m0s"}, "text"},
		{SettingsSetupField{Key: "cache.max_bytes", Value: "1024"}, "text"},
	} {
		settingsPrepareField(&tc.field)
		if tc.field.Control != tc.want {
			t.Fatalf("%s control = %q, want %q", tc.field.Key, tc.field.Control, tc.want)
		}
		if tc.field.InputID != "setup-in-"+strings.ReplaceAll(tc.field.Key, ".", "--") {
			t.Fatalf("%s input id = %q", tc.field.Key, tc.field.InputID)
		}
	}
	// The WP13 keys render with real examples and notes in the setup page.
	for _, tc := range []struct{ key, example, note string }{
		{"server.auth.admin_groups", "spindle-admins", "empty disables group admins"},
		{"server.auth.groups_claim", "groups", "ID-token claim"},
	} {
		f := SettingsSetupField{Key: tc.key}
		settingsPrepareField(&f)
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

func TestSettingsSetupAdminFallbackWithoutAccessDep(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, nil, nil
		},
	}})
	// auth mode token + a non-admin principal, no SetupAccess binding: the
	// handler refuses writes and shows the restricted card on reads.
	s.env.Cfg.Server.Auth.Mode = "token"
	p := settingsTestPrincipal()
	p.Admin = false
	s.auth = pageAuthFor(p)
	if rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("fallback GET = %d", rec.Code)
	} else {
		settingsMustContain(t, rec.Body.String(), "Access restricted", "fallback restricted")
	}
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("fallback write = %d, want 403", rec.Code)
	}
	// A host admin passes the fallback.
	p.Admin = true
	s.auth = pageAuthFor(p)
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("admin fallback write = %d, want 303", rec.Code)
	}
}

func TestSettingsSetupAuthTestAdminFallback(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupAuthTest: func(context.Context, string, string, string) (*SettingsSetupAuthTest, []SettingsSetupError) {
			t.Fatal("the discovery probe ran without authority")
			return nil, nil
		},
	}})
	// The unbound access seam applies the same fallback as GET and save: only
	// an admin may trigger the outbound OIDC discovery probe.
	s.env.Cfg.Server.Auth.Mode = "token"
	p := settingsTestPrincipal()
	p.Admin = false
	s.auth = pageAuthFor(p)
	rec := settingsDo(t, s, http.MethodPost, "/setup/auth-test",
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

	setup := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) {
		v := settingsFakeView()
		v.AuthMode = "oidc"
		v.PublicURL = "https://git.example.com"
		v.Sections = append(v.Sections, SettingsSetupSection{Section: "auth", Essential: []SettingsSetupField{
			{Key: "server.auth.mode", Type: "enum", Value: "oidc", Enum: []string{"none", "token", "oidc"}},
			{Key: "server.auth.issuer", Value: "https://id.example.com"},
			{Key: "server.auth.anonymous_read", Type: "bool", Value: "true"},
			{Key: "server.auth.session_secret", Secret: true, Value: ""},
		}})
		return v, nil
	}}}
	s := newSettingsServer(t, setup)
	write("setup.html", settingsDo(t, s, http.MethodGet, "/setup", nil, nil))

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
// page names the admin group rather than any setup token.
func TestSettingsSetupAdminGroupAccess(t *testing.T) {
	rule := func(_ *http.Request, p auth.Principal) *auth.AuthError {
		if p.Anonymous {
			return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required for setup"}
		}
		if !p.Admin {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "setup requires an admin principal (an OIDC admin group, admin email, or admin domain)"}
		}
		return nil
	}
	deps := SettingsDeps{
		SetupAccess: rule,
		SetupView:   func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{Saved: true}, nil, nil
		},
	}

	// A group admin (Principal.Admin is what the OIDC claim derivation sets)
	// sees the editable form.
	s := newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "dev", Email: "dev@example.com", Write: true, Admin: true}})
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group admin GET /setup = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `name="cfg.server.listen"`, "admin sees the form")
	settingsMustNotContain(t, body, "Access restricted", "admin is not denied")

	// A signed-in non-admin gets the restricted card (200) and a real 403 on
	// the write path, and the card names the group rule, not a token.
	s = newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "zoe", Email: "zoe@example.com", Write: true}})
	rec = settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-admin GET /setup = %d, want the restricted card", rec.Code)
	}
	body = rec.Body.String()
	settingsMustContain(t, body, "Access restricted", "non-admin denied")
	settingsMustContain(t, body, "spindle-admins", "card names the admin group")
	settingsMustContain(t, body, "server.auth.admin_groups", "card names the config key")
	settingsMustNotContain(t, body, "SPINDLE_SETUP_TOKEN", "no setup token text")
	settingsMustNotContain(t, body, `name="cfg.server.listen"`, "non-admin has no form")
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin POST /setup = %d, want 403", rec.Code)
	}

	// Anonymous is a 401 (unauthenticated), not a 403.
	s = newSettingsServer(t, &settingsFakes{deps: deps,
		principal: auth.Principal{Name: "anonymous", Anonymous: true}})
	if rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST /setup = %d, want 401", rec.Code)
	}
}

// TestSettingsRepoProtectionTabRenders is the WP2.3 page contract: the
// Branch protection tab renders the structured form (not raw JSON), the
// read-only effective view, and the mobile rules (no bare multi-column grid,
// sidebar nav intact, the panel swaps in place).
func TestSettingsRepoProtectionTabRenders(t *testing.T) {
	f := settingsRepoFakes("admin")
	f.deps.RepoProtection = func(context.Context, string, string) (*SettingsProtection, error) {
		return &SettingsProtection{
			Enabled: true, Refs: "refs/heads/main", Approvals: 2, BlockForce: true,
			PushRoles: []string{"admin"}, Roles: settingsProtectionRoles,
			Rules: []SettingsPolicyRule{{Name: "protection-push", Refs: "refs/heads/main", Summary: "restricts update", Managed: true}},
			Raw:   `{"version":1,"rules":[]}`,
		}, nil
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
	settingsMustContain(t, body, `name="refs"`, "structured refs")
	settingsMustContain(t, body, `name="approvals"`, "structured approvals")
	settingsMustContain(t, body, `name="push_roles" value="admin" class="h-4 w-4 accent-amber-500" checked`, "role bypass")
	settingsMustContain(t, body, `name="checks"`, "structured checks")
	settingsMustContain(t, body, `name="block_force"`, "force-push restriction")
	settingsMustContain(t, body, `name="block_delete"`, "deletion restriction")
	settingsMustContain(t, body, "Effective policy", "read-only view")
	settingsMustContain(t, body, "protection-push", "effective rule row")
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
