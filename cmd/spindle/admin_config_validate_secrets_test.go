package main

// admin_config_validate_secrets_test.go — the Configuration page's Validate
// step must judge the same config Save would write. A secret supplied by the
// environment (or a flag) is not in the file, and validating the file
// candidate alone refused an unrelated edit and demanded a rotation that was
// not needed (reported 2026-11-25: changing import.url_allowlist failed on
// server.auth.oauth_client_secret).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/setup"
)

const validateFileTOML = `
[server]
listen = "0.0.0.0:8080"
roles = ["serve"]

[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "spindle"
admin_groups = ["spindle-admin"]
`

// validateFixture is the reported shape: the file carries the non-secret OIDC
// keys, while both secrets exist ONLY in the effective config (the env
// overlay), so the file candidate alone cannot satisfy the validator.
func validateFixture(t *testing.T) (*config.Config, server.BootState) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	if err := os.WriteFile(path, []byte(validateFileTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	paths := []string{path}
	effective := setup.BaseConfig(dir, paths)
	effective.DataDir = dir
	effective.Server.Auth.OAuthClientSecret = "env-only-client-secret"
	effective.Server.Auth.SessionSecret = strings.Repeat("k", 32)
	boot := server.BootState{Mode: "normal", FileState: "valid", ConfigPaths: paths}
	return effective, boot
}

func TestAdminValidateKeepsEffectiveSecrets(t *testing.T) {
	effective, boot := validateFixture(t)
	errs, err := adminValidateFn(effective, boot)(t.Context(), map[string]string{
		"import.url_allowlist": "example.com",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("an unrelated edit with env-only secrets must validate, got %+v", errs)
	}
}

// The fill must not paper over a genuinely missing secret.
func TestAdminValidateStillRefusesMissingSecrets(t *testing.T) {
	effective, boot := validateFixture(t)
	effective.Server.Auth.OAuthClientSecret = ""
	effective.Server.Auth.SessionSecret = ""
	errs, err := adminValidateFn(effective, boot)(t.Context(), map[string]string{
		"import.url_allowlist": "example.com",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "OIDC requires") {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %+v, want the OIDC required-secret error", errs)
	}
}
