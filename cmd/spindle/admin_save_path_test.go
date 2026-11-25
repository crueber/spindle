// admin_save_path_test.go — Save writes the file the process LOADED.
//
// The bug: with an explicit config path (--config / SPINDLE_CONFIG /
// WALGIT_CONFIG) the page rendered from that file, but the save wrote
// <data-dir>/spindle.toml unconditionally — a shadow file the loader never
// reads. The operator restarted and saw the OLD values ("allow_private_networks
// = false", no url_allowlist at all), because the edit went somewhere else.
//
// These tests pin the loader's own candidate order (config.SaveTargetPath):
// first existing candidate, else <data-dir>/spindle.toml.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/setup"
)

// validBase is the smallest config that passes setup.Validate (OIDC is the
// only authentication, so a save refuses an unconfigured document).
func validBase(dataDir string) *config.Config {
	c := config.Defaults()
	c.DataDir = dataDir
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.OAuthClientID = "spindle"
	c.Server.Auth.OAuthClientSecret = "client-secret"
	c.Server.Auth.SessionSecret = strings.Repeat("s", 64)
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	return c
}

// writeExplicitConfig writes a valid config at path (an operator-managed file
// outside the data dir, the --config shape).
func writeExplicitConfig(t *testing.T, path, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveSetupTo(path, validBase(dataDir), validBase(dataDir)); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// fileHas is a small TOML-content probe: the saved document must carry the
// key, and the wrong file must not exist at all.
func fileHas(t *testing.T, path, needle string) bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Contains(string(body), needle)
}

// TestAdminSaveWritesExplicitConfigFile is the operator's probe as a test:
// --config <tmp>/etc/spindle.toml, save the import policy on /admin/config,
// and the edit lands in THAT file — the data dir gets no shadow copy, and
// setup.BaseConfig (the reload the page and the next boot do) reads it back.
func TestAdminSaveWritesExplicitConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "etc", "spindle.toml")
	writeExplicitConfig(t, explicit, dataDir)

	cfg, _, err := config.Load([]string{explicit}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load explicit: %v", err)
	}
	// The pointer/env spellings resolve to the same single candidate.
	for _, env := range []string{"SPINDLE_CONFIG", "WALGIT_CONFIG"} {
		got, ok := config.ConfigFilePointer(func(k string) string {
			if k == env {
				return explicit
			}
			return ""
		})
		if !ok || got != explicit {
			t.Fatalf("%s pointer = %q ok=%v, want %q", env, got, ok, explicit)
		}
	}
	if cfg.Import.AllowPrivateNetworks {
		t.Fatal("fixture must start fail-closed")
	}

	boot := server.BootState{Mode: "normal", FileState: "valid", ConfigPaths: []string{explicit}}
	live := config.NewLiveImport(cfg)
	save := adminSaveFn(cfg, boot, live)
	res, errs, serr := save(context.Background(), map[string]string{
		"import.allow_private_networks": "true",
		"import.url_allowlist":          "github.com",
	})
	if serr != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, serr)
	}
	if res.Path != explicit {
		t.Fatalf("AdminSaveResult.Path = %q, want the loaded file %q", res.Path, explicit)
	}

	// The loaded file carries the edit…
	if !fileHas(t, explicit, "allow_private_networks = true") {
		t.Fatalf("%s does not carry the saved policy", explicit)
	}
	if !fileHas(t, explicit, "url_allowlist = [\"github.com\"]") {
		t.Fatalf("%s does not carry the saved allowlist", explicit)
	}
	// …and the data-dir shadow file was never created (the exact defect).
	shadow := filepath.Join(dataDir, config.ConfigFileName)
	if _, statErr := os.Stat(shadow); !os.IsNotExist(statErr) {
		t.Fatalf("a data-dir shadow file was written at %s", shadow)
	}
	// A reload (the page's own base, and the next boot's file leg) reads it.
	reloaded := setup.BaseConfig(dataDir, []string{explicit})
	if !reloaded.Import.AllowPrivateNetworks || len(reloaded.Import.URLAllowlist) != 1 {
		t.Fatalf("reload = %+v, want the saved policy", reloaded.Import)
	}
}

// The alias is a real loaded file too: a boot from <data-dir>/walgit.toml must
// rewrite THAT file, not create spindle.toml beside it.
func TestAdminSaveWritesLoadedAliasFile(t *testing.T) {
	dataDir := t.TempDir()
	alias := filepath.Join(dataDir, config.ConfigAliasName)
	writeExplicitConfig(t, alias, dataDir)

	cfg, _, err := config.Load(configCandidatesFor(dataDir), func(string) string { return "" })
	if err != nil {
		t.Fatalf("load alias: %v", err)
	}
	boot := server.BootState{Mode: "normal", FileState: "valid",
		ConfigPaths: []string{filepath.Join(dataDir, config.ConfigFileName), alias}}
	save := adminSaveFn(cfg, boot, config.NewLiveImport(cfg))
	res, errs, serr := save(context.Background(), map[string]string{"import.allow_file_urls": "true"})
	if serr != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, serr)
	}
	if res.Path != alias {
		t.Fatalf("AdminSaveResult.Path = %q, want the alias %q", res.Path, alias)
	}
	if !fileHas(t, alias, "allow_file_urls = true") {
		t.Fatal("the alias file does not carry the save")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, config.ConfigFileName)); !os.IsNotExist(statErr) {
		t.Fatal("a spindle.toml was created next to the loaded alias")
	}
}

// configCandidatesFor mirrors cmd/spindle's defaultConfigPaths without a cli.
func configCandidatesFor(dataDir string) []string {
	return []string{
		filepath.Join(dataDir, config.ConfigFileName),
		filepath.Join(dataDir, config.ConfigAliasName),
	}
}

// No explicit path and no file: the first-run save still writes
// <data-dir>/spindle.toml (today's behaviour, preserved).
func TestAdminSaveFallsBackToDataDirFile(t *testing.T) {
	dataDir := t.TempDir()
	boot := server.BootState{Mode: "setup_only", FileState: "absent", ConfigPaths: configCandidatesFor(dataDir)}
	cfg := config.FirstRunDefaults(dataDir)
	if got := config.SaveTargetPath(dataDir, boot.ConfigPaths, boot.ConfigExplicit); got != filepath.Join(dataDir, config.ConfigFileName) {
		t.Fatalf("SaveTargetPath = %q, want the data-dir default", got)
	}
	save := adminSaveFn(cfg, boot, config.NewLiveImport(cfg))
	res, errs, serr := save(context.Background(), map[string]string{
		"server.auth.issuer":              "https://id.example.com",
		"server.auth.oauth_client_id":     "spindle-web",
		"server.auth.oauth_client_secret": "client-secret",
		"server.auth.session_secret":      strings.Repeat("s", 64),
		"server.auth.allowed_domains":     "example.com",
		"server.auth.admin_groups":        "spindle-admin",
		"import.allow_private_networks":   "true",
	})
	if serr != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, serr)
	}
	want := filepath.Join(dataDir, config.ConfigFileName)
	if res.Path != want {
		t.Fatalf("Path = %q, want %q", res.Path, want)
	}
	if !fileHas(t, want, "allow_private_networks = true") {
		t.Fatal("the data-dir file does not carry the save")
	}
}

// TestAdminSaveCreatesMissingExplicitConfig is the create path of the same rule:
// a boot whose candidate list is one NAMED file (--config / SPINDLE_CONFIG /
// WALGIT_CONFIG) that is not on disk. The save must create THAT file (and its
// directory) — not a data-dir shadow the next `--config` boot cannot find.
func TestAdminSaveCreatesMissingExplicitConfig(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "etc", "spindle", "spindle.toml")
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Fatalf("fixture must start without the file: %v", err)
	}
	// The restricted first-run shape: no file anywhere, OIDC unconfigured, the
	// operator names where the config belongs.
	cfg := config.FirstRunDefaults(dataDir)
	boot := server.BootState{Mode: "setup_only", FileState: "absent",
		ConfigPaths: []string{explicit}, ConfigExplicit: true}
	live := config.NewLiveImport(cfg)
	save := adminSaveFn(cfg, boot, live)

	res, errs, serr := save(context.Background(), map[string]string{
		"server.auth.issuer":              "https://id.example.com",
		"server.auth.oauth_client_id":     "spindle-web",
		"server.auth.oauth_client_secret": "client-secret",
		"server.auth.session_secret":      strings.Repeat("s", 64),
		"server.auth.allowed_domains":     "example.com",
		"server.auth.admin_groups":        "spindle-admin",
		"import.allow_private_networks":   "true",
	})
	if serr != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, serr)
	}
	if res.Path != explicit {
		t.Fatalf("Path = %q, want the named file %q", res.Path, explicit)
	}
	if !fileHas(t, explicit, "allow_private_networks = true") ||
		!fileHas(t, explicit, `allowed_domains = ["example.com"]`) {
		t.Fatal("the created explicit file does not carry the onboarding values")
	}
	// The next boot with the same --config reads it (§6.1 ladder: explicit).
	reloaded, _, lerr := config.LoadExplicit(explicit, func(string) string { return "" })
	if lerr != nil {
		t.Fatalf("the created file must load: %v", lerr)
	}
	if !reloaded.Import.AllowPrivateNetworks || reloaded.Server.Auth.Issuer != "https://id.example.com" {
		t.Fatalf("reload = %+v", reloaded)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, config.ConfigFileName)); !os.IsNotExist(statErr) {
		t.Fatal("a data-dir shadow file was created")
	}
}

// TestAdminSaveDevNullTargetsTheDataDir: `--config /dev/null` means defaults +
// environment, so the save writes a real data-dir file instead of the device.
func TestAdminSaveDevNullTargetsTheDataDir(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.FirstRunDefaults(dataDir)
	boot := server.BootState{Mode: "setup_only", FileState: "absent",
		ConfigPaths: []string{config.DevNullConfigPath}, ConfigExplicit: true}
	save := adminSaveFn(cfg, boot, config.NewLiveImport(cfg))
	res, errs, serr := save(context.Background(), map[string]string{
		"server.auth.issuer":              "https://id.example.com",
		"server.auth.oauth_client_id":     "spindle-web",
		"server.auth.oauth_client_secret": "client-secret",
		"server.auth.session_secret":      strings.Repeat("s", 64),
		"server.auth.allowed_domains":     "example.com",
		"server.auth.admin_groups":        "spindle-admin",
	})
	if serr != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, serr)
	}
	want := filepath.Join(dataDir, config.ConfigFileName)
	if res.Path != want {
		t.Fatalf("Path = %q, want %q", res.Path, want)
	}
	if !fileHas(t, want, `allowed_domains = ["example.com"]`) {
		t.Fatal("the data-dir file does not carry the save")
	}
}
