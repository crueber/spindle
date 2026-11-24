package main

// admin_config_toml_test.go — the Configuration page's read-only TOML
// disclosure (PLAN-ADMIN M3) is the exact document Save writes, with secrets
// masked. It comes from the same setup.BaseConfig the save path edits, so the
// view and the file cannot drift.

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

func TestAdminConfigTOMLViewMatchesSave(t *testing.T) {
	dir := t.TempDir()
	cfg := config.FirstRunDefaults(dir)
	seedOIDC(cfg)
	// Save first: the view is a projection of the SAVED document, and OIDC is
	// mandatory now, so a save of pristine defaults would (correctly) refuse.
	if _, err := config.SaveSetup(cfg, dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := filepath.Join(dir, config.ConfigFileName)
	view, err := adminConfigViewFn(cfg, server.BootState{Mode: "normal", ConfigPaths: []string{path}})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.TOML == "" {
		t.Fatal("the configuration view has no TOML document")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The view is the SAVED document with every secret masked; byte equality
	// therefore holds after masking the same values in the file.
	want := string(saved)
	for _, secret := range []string{cfg.Server.Auth.SessionSecret, cfg.Server.Auth.OAuthClientSecret} {
		if secret != "" {
			want = strings.ReplaceAll(want, secret, setup.SecretMask)
		}
	}
	if want != view.TOML {
		t.Fatalf("the TOML view is not the masked Save document\n--- view ---\n%s\n--- want ---\n%s", view.TOML, want)
	}
}

func TestAdminConfigTOMLViewMasksSecrets(t *testing.T) {
	dir := t.TempDir()
	const secret = "session-secret-never-rendered-0123456789"
	cfg := config.Defaults()
	cfg.DataDir = dir
	seedOIDC(cfg)
	cfg.Server.Auth.SessionSecret = secret
	if _, err := config.SaveSetup(cfg, dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	view, err := adminConfigViewFn(cfg, server.BootState{Mode: "normal", ConfigPaths: []string{filepath.Join(dir, config.ConfigFileName)}})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view.TOML, secret) {
		t.Fatal("the TOML view publishes a stored secret")
	}
	if !strings.Contains(view.TOML, setup.SecretMask) {
		t.Fatal("the TOML view does not show the secret's mask")
	}
}

func TestAdminConfigTOMLViewHiddenWhileInvalid(t *testing.T) {
	cfg := config.FirstRunDefaults(t.TempDir())
	view, err := adminConfigViewFn(cfg, server.BootState{Mode: "setup_only"})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.TOML != "" {
		t.Fatal("restricted mode must not present a saved-configuration document")
	}
}

// seedOIDC fills the mandatory auth block: a config without it is invalid now,
// and these tests are about the TOML view, not about validation.
func seedOIDC(c *config.Config) {
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	c.Server.Auth.OAuthClientID = "spindle"
	c.Server.Auth.OAuthClientSecret = "client-secret"
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
}
