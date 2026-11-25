// admin_save_path_page_test.go — the Configuration page NAMES the file Save
// writes, and the rendered save lands in the file the process loaded (an
// explicit config path), not in a data-dir shadow copy. Rendered through the
// real page provider + the real setup seams.
package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/webui"
)

// explicitConfigPageHarness boots the page provider over a config file that
// lives OUTSIDE the data dir (the --config shape) and returns the server, the
// loaded path and the data dir.
func explicitConfigPageHarness(t *testing.T) (*webui.Server, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "etc", "spindle")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "spindle.toml")
	base := adminConfigPageFixture(t)
	base.DataDir = dataDir
	body, err := config.MarshalTOML(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := setupBase(t, dataDir, path)
	boot := server.BootState{Mode: "normal", FileState: "valid", ConfigPaths: []string{path}}

	ui, uerr := webui.New(webui.Env{Cfg: cfg, Version: "test"})
	if uerr != nil {
		t.Fatalf("webui: %v", uerr)
	}
	ui.BindAuth(pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "admin@example.com", Admin: true}, nil
	}})
	wireSettingsPages(cfg, boot, nil, nil)
	live := config.NewLiveImport(cfg)
	wireAdminPages(cfg, boot, nil, nil, nil, nil, nil, nil, nil, live)
	t.Cleanup(func() {
		webui.BindSettings(webui.SettingsDeps{})
		webui.BindAdmin(webui.AdminDeps{})
	})
	return ui, path, dataDir
}

// setupBase is setup.BaseConfig behind a name that reads well at the call site.
func setupBase(t *testing.T, dataDir, path string) *config.Config {
	t.Helper()
	cfg, err := config.LoadSetupBase(dataDir, []string{path})
	if err != nil {
		t.Fatalf("load base: %v", err)
	}
	cfg.DataDir = dataDir
	return cfg
}

// TestAdminConfigPageNamesLoadedFile: the "file Save writes" section shows the
// absolute loaded path, not "<data-dir>/spindle.toml".
func TestAdminConfigPageNamesLoadedFile(t *testing.T) {
	ui, path, _ := explicitConfigPageHarness(t)
	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, path) {
		t.Fatalf("the page does not name the loaded config file %q", path)
	}
	if !strings.Contains(page, "The file Save writes") {
		t.Fatal("the TOML disclosure heading is missing")
	}
}

// TestAdminConfigPageSaveWritesLoadedFile is the rendered operator loop: load
// <tmp>/etc/spindle/spindle.toml via ConfigPaths, flip the import policy on the
// page, and assert the redirect names the path, the loaded file changed, and
// the data dir gained no spindle.toml.
func TestAdminConfigPageSaveWritesLoadedFile(t *testing.T) {
	ui, path, dataDir := explicitConfigPageHarness(t)
	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	form := scrapeAdminConfigForm(t, rec.Body.String())
	form.Set("op", "save")
	form.Set("cfg.import.allow_private_networks", "true")
	form.Set("cfg.import.url_allowlist", "github.com")

	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d: %s", rec.Code, rec.Body.String()[:400])
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, url.QueryEscape(path)) {
		t.Fatalf("redirect = %q, want the saved path %q", loc, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "allow_private_networks = true") {
		t.Fatalf("the loaded file does not carry the save:\n%s", body)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, config.ConfigFileName)); !os.IsNotExist(statErr) {
		t.Fatal("the page save created a data-dir shadow file")
	}

	// The saved banner names the file too.
	got := adminConfigPageDoURL(t, ui, loc)
	if !strings.Contains(got.Body.String(), "Written to") || !strings.Contains(got.Body.String(), path) {
		t.Fatalf("the saved banner does not name %q", path)
	}
}

// TestAdminConfigPageCreatesMissingExplicitFile is the create path of the rule,
// rendered: a first-run boot whose candidate list is one NAMED file that does
// not exist yet. The page names that path, and Save creates it — not a data-dir
// shadow the next `--config` boot cannot find.
func TestAdminConfigPageCreatesMissingExplicitFile(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "etc", "spindle", "spindle.toml")
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Fatalf("fixture must start without the file: %v", err)
	}
	cfg := config.FirstRunDefaults(dataDir)
	boot := server.BootState{Mode: "setup_only", FileState: "absent",
		ConfigPaths: []string{explicit}, ConfigExplicit: true}

	ui, uerr := webui.New(webui.Env{Cfg: cfg, Version: "test", Restricted: true})
	if uerr != nil {
		t.Fatalf("webui: %v", uerr)
	}
	ui.BindAuth(pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "admin@example.com", Admin: true}, nil
	}})
	wireSettingsPages(cfg, boot, nil, nil)
	wireAdminPages(cfg, boot, nil, nil, nil, nil, nil, nil, nil, config.NewLiveImport(cfg))
	t.Cleanup(func() {
		webui.BindSettings(webui.SettingsDeps{})
		webui.BindAdmin(webui.AdminDeps{})
	})

	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, explicit) {
		t.Fatalf("the page does not name the file %q", explicit)
	}
	form := scrapeAdminConfigForm(t, page)
	form.Set("op", "save")
	form.Set("cfg.server.auth.issuer", "https://id.example.com")
	form.Set("cfg.server.auth.oauth_client_id", "spindle-web")
	form.Set("cfg.server.auth.oauth_client_secret", "client-secret")
	form.Set("cfg.server.auth.session_secret", strings.Repeat("s", 64))
	form.Set("cfg.server.auth.allowed_domains", "example.com")
	form.Set("cfg.server.auth.admin_groups", "spindle-admin")
	form.Set("cfg.import.allow_private_networks", "true")

	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d: %.400s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape(explicit)) {
		t.Fatalf("redirect = %q, want the created path", loc)
	}
	body, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("the save did not create %s: %v", explicit, err)
	}
	for _, want := range []string{`allowed_domains = ["example.com"]`, "allow_private_networks = true"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the created file lacks %q:\n%s", want, body)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, config.ConfigFileName)); !os.IsNotExist(statErr) {
		t.Fatal("the page save created a data-dir shadow file")
	}
}
