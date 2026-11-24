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

// A deployment may supply the OIDC secrets through SPINDLE__SERVER__AUTH__*
// rather than the file. The page renders them masked; a masked field means
// "keep the current value", so a save that changes something else must neither
// fail validation nor persist the env secret into spindle.toml, and it must not
// claim the secret changed (a false restart prompt).
func TestAdminSaveKeepsEnvironmentOnlySecrets(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.OAuthClientID = "spindle"
	cfg.Server.Auth.OAuthClientSecret = "env-client-secret"
	cfg.Server.Auth.SessionSecret = strings.Repeat("e", 32)
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}
	boot := server.BootState{Mode: "normal", FileState: "absent"} // no file yet

	save := adminSaveFn(cfg, boot)
	res, errs, err := save(context.Background(), map[string]string{
		"server.auth.issuer":              "https://id.example.com",
		"server.auth.allowed_domains":     "example.com",
		"server.auth.oauth_client_id":     "spindle",
		"server.auth.admin_groups":        "spindle-admin",
		"server.auth.oauth_client_secret": setup.SecretMask,
		"server.auth.session_secret":      setup.SecretMask,
		"server.public_url":               "https://spindle.example.com",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("save errors: %+v", errs)
	}
	if !res.Saved {
		t.Fatal("save did not report Saved")
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "spindle.toml"))
	if rerr != nil {
		t.Fatalf("read saved config: %v", rerr)
	}
	for _, secret := range []string{"env-client-secret", strings.Repeat("e", 32)} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("environment-only secret was persisted to spindle.toml")
		}
	}
	for _, k := range res.RequiresRestart {
		if k == "server.auth.oauth_client_secret" || k == "server.auth.session_secret" {
			t.Fatalf("unchanged env secret reported restart-required: %s", k)
		}
	}
}
