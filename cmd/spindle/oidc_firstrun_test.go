package main

// oidc_firstrun_test.go — PLAN-OIDC-ONLY first run: the OIDC card's partial
// save, and the boot matrix's absent/unconfigured state.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
)

// TestFirstRunCardPartialPost pins the card's contract: it posts ONLY the five
// OIDC keys, and the ordinary save applies them on top of the file-visible base
// — every untouched first-run key (listen, store, cache, auto-create) survives.
func TestFirstRunCardPartialPost(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, config.ConfigFileName)
	cfg := config.FirstRunDefaults(dataDir)
	boot := server.BootState{Mode: "setup_only", FileState: "absent", ConfigPaths: []string{path}}
	save := adminSaveFn(cfg, boot, nil)

	values := map[string]string{
		"server.auth.issuer":              "https://id.example.com",
		"server.auth.oauth_client_id":     "spindle-web",
		"server.auth.oauth_client_secret": "client-secret",
		"server.auth.session_secret":      strings.Repeat("s", 64),
		"server.auth.allowed_domains":     "example.com",
		"server.auth.admin_groups":        "spindle-admin",
	}
	result, errs, err := save(context.Background(), values)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("save errors = %+v", errs)
	}
	if !result.Saved {
		t.Fatal("save did not report saved")
	}

	var got struct {
		Server struct {
			Listen           string `toml:"listen"`
			AutoCreateOnPush bool   `toml:"auto_create_on_push"`
			Auth             struct {
				Issuer        string   `toml:"issuer"`
				ClientID      string   `toml:"oauth_client_id"`
				ClientSecret  string   `toml:"oauth_client_secret"`
				SessionSecret string   `toml:"session_secret"`
				AdminGroups   []string `toml:"admin_groups"`
			} `toml:"auth"`
		} `toml:"server"`
		Store struct {
			Backend string `toml:"backend"`
			Root    string `toml:"root"`
		} `toml:"store"`
		Cache struct {
			Dir string `toml:"dir"`
		} `toml:"cache"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if _, err := toml.Decode(string(raw), &got); err != nil {
		t.Fatalf("saved file is not valid TOML: %v\n%s", err, raw)
	}
	if got.Server.Auth.Issuer != "https://id.example.com" ||
		got.Server.Auth.ClientID != "spindle-web" ||
		got.Server.Auth.ClientSecret != "client-secret" ||
		got.Server.Auth.SessionSecret != strings.Repeat("s", 64) ||
		len(got.Server.Auth.AdminGroups) != 1 || got.Server.Auth.AdminGroups[0] != "spindle-admin" {
		t.Fatalf("auth keys not written: %+v", got.Server.Auth)
	}
	// Untouched first-run keys survive the five-key POST.
	if got.Server.Listen != "0.0.0.0:8080" || !got.Server.AutoCreateOnPush {
		t.Fatalf("first-run server shape lost: %+v", got.Server)
	}
	if got.Store.Backend != "filesystem" || got.Store.Root != filepath.Join(dataDir, "store") {
		t.Fatalf("store shape lost: %+v", got.Store)
	}
	if got.Cache.Dir != filepath.Join(dataDir, "cache") {
		t.Fatalf("cache dir lost: %q", got.Cache.Dir)
	}
	// And the result boots normally (the saved file validates).
	loaded, _, err := config.Load([]string{path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("saved config does not boot: %v", err)
	}
	if loaded.Server.Auth.Issuer != "https://id.example.com" {
		t.Fatalf("loaded issuer = %q", loaded.Server.Auth.Issuer)
	}
}

// TestBootMatrixAbsentIsUnconfigured: no config file + no OIDC env is the
// restricted first-run state, not a defaults-mode serving boot.
func TestBootMatrixAbsentIsUnconfigured(t *testing.T) {
	clearEnv(t, "SPINDLE__SERVER__AUTH__ISSUER")
	clearEnv(t, "SPINDLE__SERVER__AUTH__ALLOWED_DOMAINS")
	clearEnv(t, "SPINDLE__SERVER__AUTH__OAUTH_CLIENT_ID")
	clearEnv(t, "SPINDLE__SERVER__AUTH__OAUTH_CLIENT_SECRET")
	clearEnv(t, "SPINDLE__SERVER__AUTH__SESSION_SECRET")
	clearEnv(t, "SPINDLE_CONFIG")
	clearEnv(t, "WALGIT_CONFIG")
	clearEnv(t, "PORT")
	dir := t.TempDir()
	t.Setenv("SPINDLE_DATA_DIR", dir)
	cfg, state, err := resolveConfig(&cli{dataDir: dir})
	if state != stateUnconfigured {
		t.Fatalf("state = %q, want unconfigured (err=%v)", state, err)
	}
	if err == nil || !strings.Contains(err.Error(), "server.auth.issuer") {
		t.Fatalf("err = %v, want the named OIDC rule", err)
	}
	if cfg == nil || cfg.Server.Auth.Issuer != "" {
		t.Fatalf("config = %+v, want the unconfigured shape carried for the boot", cfg)
	}

	// The restricted-absent view is the first-run onboarding, not "invalid".
	boot := server.BootState{Mode: "setup_only", FileState: "absent", ConfigPaths: []string{filepath.Join(dir, config.ConfigFileName)}}
	view, verr := adminConfigViewFn(cfg, boot)(context.Background())
	if verr != nil || view == nil {
		t.Fatalf("view: %v", verr)
	}
	if view.FileState != "absent" || !view.FirstRun {
		t.Fatalf("view = %+v, want absent first run", view)
	}
}
