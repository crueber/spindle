package webui

// coverage_settings_test.go — the settings group's remaining branches: the
// provider-failure and form-parse guards on every route, the setup validator's
// three outcomes, the "one-shot secret" page, the org avatar/delete paths, and
// the pure spelling/diff helpers. The happy paths live in settings_test.go.

import (
	"bytes"
	"context"
	"crypto/tls"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/server/auth"
)

// coverageRawPost sends a body verbatim (malformed forms are the point) with a
// same-origin browser request.
func coverageRawPost(s *Server, target, contentType, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.Host = "example.com"
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", "http://example.com")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	return rec
}

// TestCoverageSettingsAuthFailureGuards drives every settings route with a
// failing authenticator: none of them may fall through to a page render.
func TestCoverageSettingsAuthFailureGuards(t *testing.T) {
	s := newSettingsServer(t, settingsRepoFakes("admin"))
	s.auth.Authenticate = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "credential revoked"}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/setup"},
		{http.MethodPost, "/setup"},
		{http.MethodPost, "/setup/auth-test"},
		{http.MethodGet, "/keys"},
		{http.MethodPost, "/keys"},
		{http.MethodPost, "/keys/id/delete"},
		{http.MethodGet, "/acme/monorepo/settings/general"},
		{http.MethodPost, "/acme/monorepo/settings/general"},
		{http.MethodGet, "/acme/settings"},
		{http.MethodPost, "/acme/settings"},
		{http.MethodPost, "/acme/settings/preview"},
	} {
		rec := settingsDo(t, s, tc.method, tc.path, nil, map[string]string{"Accept": "text/plain"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "credential revoked") {
			t.Fatalf("%s %s body = %q", tc.method, tc.path, rec.Body.String())
		}
	}
}

func TestCoverageSettingsSetupValidationOutcomes(t *testing.T) {
	// The validator is optional: a build without it answers 503, never a
	// silent "valid" redirect.
	s := newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
	}})
	rec := settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"validate"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("validate without a validator = %d", rec.Code)
	}

	// Validator outage is a 503 with the reason.
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupValidate: func(context.Context, map[string]string) ([]SettingsSetupError, error) {
			return nil, &settingsTestErr{"validator crashed"}
		},
	}})
	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"validate"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("validator outage = %d", rec.Code)
	}

	// Errors reject with the typed values; success redirects with validated=1.
	var seen map[string]string
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupValidate: func(_ context.Context, values map[string]string) ([]SettingsSetupError, error) {
			seen = values
			if values["server.listen"] == "bad" {
				return []SettingsSetupError{{Key: "server.listen", Message: "not a listen address"}}, nil
			}
			return nil, nil
		},
	}})
	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{
		"op": {"validate"}, "token": {"tok"}, "cfg.server.listen": {"bad"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validate reject = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not a listen address", "validate reject")
	if seen["server.listen"] != "bad" {
		t.Fatalf("validator saw %v", seen)
	}

	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{
		"op": {"validate"}, "token": {"tok"}, "cfg.server.listen": {"0.0.0.0:8080"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("validate ok = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "validated=1") || !strings.Contains(loc, "token=tok") {
		t.Fatalf("validate Location = %q", loc)
	}

	// A view outage during the reject still renders the typed form.
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) {
			return nil, &settingsTestErr{"schema down"}
		},
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, []SettingsSetupError{{Message: "bad value"}}, nil
		},
	}})
	rec = settingsDo(t, s, http.MethodPost, "/setup", url.Values{"op": {"save"}, "cfg.server.listen": {"x"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reject without schema = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "bad value", "reject without schema")

	// op=save with an unparseable form body is a 400 (the values never reach
	// the binding).
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsFakeView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			t.Fatal("malformed form reached the saver")
			return SettingsSetupResult{}, nil, nil
		},
	}})
	rec = coverageRawPost(s, "/setup", "application/x-www-form-urlencoded", "%zz", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed setup form = %d", rec.Code)
	}
	// A nil view is an empty (still renderable) schema.
	s = newSettingsServer(t, &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return nil, nil },
	}})
	rec = settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil setup view = %d", rec.Code)
	}
}

func TestCoverageSettingsKeysPageBranches(t *testing.T) {
	keys := &settingsFakeKeys{rows: []api.SshKeyRecord{{ID: "id-1", Key: "ssh-ed25519 AAAA x", Title: "laptop"}}}
	s := newSettingsServer(t, &settingsFakes{})
	s.env.API = &api.Env{SSHKeys: keys}

	// The ?error= flash is rendered verbatim.
	rec := settingsDo(t, s, http.MethodGet, "/keys?error=registry+rejected+the+key", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys with error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "registry rejected the key", "error flash")

	// A listing outage is inline, never a blank page.
	broken := &coverageBrokenKeys{}
	s.env.API = &api.Env{SSHKeys: broken}
	rec = settingsDo(t, s, http.MethodGet, "/keys", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys outage = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "key registry unavailable", "keys outage")

	// htmx rejects swap a 200 page (htmx only swaps 2xx bodies).
	s.env.API = &api.Env{SSHKeys: keys}
	rec = settingsDo(t, s, http.MethodPost, "/keys", url.Values{"key": {"not-a-key"}, "title": {"x"}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx key reject = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "authorized_keys", "htmx key reject")

	rec = settingsDo(t, s, http.MethodPost, "/keys/missing/delete", url.Values{}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx delete reject = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not found", "htmx delete reject")

	// Malformed key forms are refused before the registry.
	rec = coverageRawPost(s, "/keys", "application/x-www-form-urlencoded", "%zz", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed key form = %d", rec.Code)
	}

	// A cross-origin delete is refused before the registry.
	rec = settingsDo(t, s, http.MethodPost, "/keys/id-1/delete", url.Values{}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin key delete = %d", rec.Code)
	}
}

// coverageBrokenKeys is an SSHKeyStore whose List fails (the page must say so
// rather than hide the outage).
type coverageBrokenKeys struct{}

func (coverageBrokenKeys) List(context.Context, string) ([]api.SshKeyRecord, error) {
	return nil, &settingsTestErr{"store unreachable"}
}
func (coverageBrokenKeys) Add(context.Context, string, string, string) (api.SshKeyRecord, error) {
	return api.SshKeyRecord{}, &settingsTestErr{"store unreachable"}
}
func (coverageBrokenKeys) Get(context.Context, string, string) (api.SshKeyRecord, error) {
	return api.SshKeyRecord{}, &settingsTestErr{"store unreachable"}
}
func (coverageBrokenKeys) Delete(context.Context, string, string) error {
	return &settingsTestErr{"store unreachable"}
}

func TestCoverageSettingsRepoPanelBranches(t *testing.T) {
	// A general-tab read outage renders inline.
	f := settingsRepoFakes("admin")
	f.deps.RepoGeneral = func(context.Context, string, string) (*SettingsGeneral, error) {
		return nil, &settingsTestErr{"settings doc unreadable"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/general", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("general outage = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "settings doc unreadable", "general outage")

	// The push-mirror panel fills its option sets when the binding omits them.
	f = settingsRepoFakes("admin")
	f.deps.RepoPushMirror = func(context.Context, string, string) (*SettingsPushMirror, error) {
		return &SettingsPushMirror{}, nil
	}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/mirror", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mirror panel = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Every hour", "push schedule presets")
	settingsMustContain(t, rec.Body.String(), "SSH deploy key", "push auth kinds")
}

func TestCoverageSettingsRepoWriteGuards(t *testing.T) {
	// Cross-origin writes are refused before any service call.
	f := settingsRepoFakes("admin")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("cross-origin write reached the service")
		return SettingsWriteResult{}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", url.Values{"description": {"x"}}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin repo write = %d", rec.Code)
	}
	rec = coverageRawPost(s, "/acme/monorepo/settings/general", "application/x-www-form-urlencoded", "%zz", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed repo write = %d", rec.Code)
	}

	// A CI token is rendered once, even when the admin panel data is absent.
	f = settingsRepoFakes("admin")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{Secret: "wht_secret_once", Notice: "token created"}, nil
	}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/tokens", url.Values{"name": {"ci"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token create = %d (%s)", rec.Code, rec.Body.String())
	}
	settingsMustContain(t, rec.Body.String(), "wht_secret_once", "token secret")
	settingsMustContain(t, rec.Body.String(), "token created", "token notice")

	// htmx write errors swap a 200 page.
	f = settingsRepoFakes("admin")
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Message: "description too long"}
	}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", url.Values{"description": {"x"}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx repo write error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "description too long", "htmx repo write error")

	// A general write that fails before the panel loaded still restores the
	// typed description (nil panel → the helper allocates it).
	f = settingsRepoFakes("admin")
	f.deps.RepoGeneral = func(context.Context, string, string) (*SettingsGeneral, error) {
		return nil, nil
	}
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Message: "nope"}
	}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/general", url.Values{"description": {"typed description"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("general reject = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "typed description", "restored description")
}

func TestCoverageSettingsOrgWriteBranches(t *testing.T) {
	// A nil org view is a 404, not a rendered empty page.
	f := &settingsFakes{deps: SettingsDeps{
		OrgView: func(context.Context, string, auth.Principal) (*SettingsOrgPage, error) { return nil, nil },
	}}
	s := newSettingsServer(t, f)
	if rec := settingsDo(t, s, http.MethodGet, "/acme/settings", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nil org view = %d", rec.Code)
	}

	// Cross-origin and malformed multipart writes are refused before the
	// service.
	f = settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		t.Fatal("invalid org write reached the service")
		return SettingsWriteResult{}, nil
	}
	s = newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {"member"}}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin org write = %d", rec.Code)
	}
	rec = coverageRawPost(s, "/acme/settings", "multipart/form-data; boundary=x", "--not-a-form", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed org multipart = %d", rec.Code)
	}

	// The avatar action streams the uploaded bytes to the service, and the
	// default action (no org_action field) is the profile save.
	var got SettingsOrgWrite
	f = settingsOrgFakes(true)
	f.deps.OrgWrite = func(_ context.Context, _ string, _ auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		got = w
		return SettingsWriteResult{}, nil
	}
	s = newSettingsServer(t, f)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("org_action", SettingsOrgAvatar)
	fw, _ := mw.CreateFormFile("avatar", "logo.png")
	_, _ = fw.Write([]byte("PNGDATA"))
	_ = mw.Close()
	rec = coverageRawPost(s, "/acme/settings", mw.FormDataContentType(), buf.String(), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("avatar upload = %d (%s)", rec.Code, rec.Body.String())
	}
	if string(got.File) != "PNGDATA" || got.Values["filename"] != "logo.png" {
		t.Fatalf("avatar payload = %q filename %q", got.File, got.Values["filename"])
	}

	rec = settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"display_name": {"Acme"}}, nil)
	if rec.Code != http.StatusSeeOther || got.Action != SettingsOrgProfileSave {
		t.Fatalf("default org action = %d %+v", rec.Code, got)
	}

	// An htmx unconfirmed delete swaps a 200 page carrying the confirm rule.
	rec = settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{
		"org_action": {SettingsOrgDelete}, "confirm": {"acm"},
	}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx org confirm = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "type acme exactly to confirm", "htmx org confirm")

	// htmx write errors swap a 200 page too.
	f = settingsOrgFakes(true)
	f.deps.OrgWrite = func(context.Context, string, auth.Principal, SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError) {
		return SettingsWriteResult{}, &SettingsWriteError{Status: http.StatusConflict, Message: "slug taken"}
	}
	s = newSettingsServer(t, f)
	rec = settingsDo(t, s, http.MethodPost, "/acme/settings", url.Values{"org_action": {SettingsOrgTeamSave}}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx org write error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "slug taken", "htmx org write error")

	// Preview rejects cross-origin and malformed forms.
	s = newSettingsServer(t, settingsOrgFakes(true))
	rec = settingsDo(t, s, http.MethodPost, "/acme/settings/preview", url.Values{"bio": {"x"}}, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin preview = %d", rec.Code)
	}
	rec = coverageRawPost(s, "/acme/settings/preview", "application/x-www-form-urlencoded", "%zz", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed preview = %d", rec.Code)
	}
}

func TestCoverageSettingsPureHelpers(t *testing.T) {
	if got := settingsRoleRank("maintain"); got != 4 {
		t.Fatalf("settingsRoleRank(maintain) = %d", got)
	}
	tabs := []settingsTab{{ID: "general", MinRead: ""}, {ID: "danger", MinRead: "admin"}}
	if got := settingsTabMinRead(tabs, "general"); got != "read" {
		t.Fatalf("settingsTabMinRead(default) = %q", got)
	}
	if got := settingsTabMinRead(tabs, "unknown"); got != "read" {
		t.Fatalf("settingsTabMinRead(unknown) = %q", got)
	}

	// The redirect URI follows the request scheme when no public URL is set.
	req := httptest.NewRequest(http.MethodGet, "https://spindle.example/setup", nil)
	req.TLS = &tls.ConnectionState{}
	if got := settingsRedirectURI(req, ""); got != "https://spindle.example/_auth/callback" {
		t.Fatalf("settingsRedirectURI(tls) = %q", got)
	}
	if got := settingsRedirectURI(httptest.NewRequest(http.MethodGet, "http://h/setup", nil), "https://public.example/"); got != "https://public.example/_auth/callback" {
		t.Fatalf("settingsRedirectURI(public) = %q", got)
	}

	// settingsFormValues ignores a key the form carried with no values (the
	// checkbox-none case) and keeps the last repeated value.
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.PostForm = url.Values{"empty": {}, "flag": {"false", "true"}, "other": {"a"}}
	vals := settingsFormValues(r, "")
	if _, ok := vals["empty"]; ok {
		t.Fatalf("empty key kept: %v", vals)
	}
	if vals["flag"] != "true" || vals["other"] != "a" {
		t.Fatalf("form values = %v", vals)
	}

	// Duration spellings: bare seconds, an unparseable unit, and a compound
	// value the regex deliberately rejects.
	for _, tc := range []struct{ in, want string }{
		{"90", "90s"},
		{"1x", "1x"},
		{"1.2.3h", "1.2.3h"},
		{"1.2.3.4x", "1.2.3.4x"},
		{"1h30ms", "3600.03s"},
	} {
		if got := fmtSpecDuration(tc.in); got != tc.want {
			t.Fatalf("fmtSpecDuration(%q) = %q want %q", tc.in, got, tc.want)
		}
	}

	// Line diff: an insertion and a trailing deletion both appear in order.
	ins := settingsLineDiff("b", "a\nb")
	if len(ins) != 2 || ins[0].Sign != "+" || ins[0].Text != "a" || ins[1].Text != "b" {
		t.Fatalf("insertion diff = %+v", ins)
	}
	del := settingsLineDiff("a\nb\nc", "a")
	if len(del) != 3 || del[1].Sign != "-" || del[2].Text != "c" {
		t.Fatalf("deletion diff = %+v", del)
	}
}
