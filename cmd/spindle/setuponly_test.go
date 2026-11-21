package main

// setuponly_test.go — F5: an invalid config must boot setup-only on the FILE's
// listen address, and the setup page must pre-fill the file's parseable values
// (with the offending keys marked) instead of presenting first-run defaults.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/webui"
)

// clearEnv unsets key for the test and restores it afterwards (t.Setenv cannot
// express "unset", and an empty env override is a real value).
func clearEnv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func setupViewField(t *testing.T, v *webui.SettingsSetupView, key string) webui.SettingsSetupField {
	t.Helper()
	for _, sec := range v.Sections {
		for _, f := range sec.Essential {
			if f.Key == key {
				return f
			}
		}
		for _, f := range sec.Advanced {
			if f.Key == key {
				return f
			}
		}
	}
	t.Fatalf("setup view has no field %q", key)
	return webui.SettingsSetupField{}
}

// TestSetupOnlyBootEnvListenWinsOverFile is F5(b) at the serve seam: the
// setup-only boot uses the env overlay's listen address (the one serveHTTP
// binds), even when the invalid file names a different one.
func TestSetupOnlyBootEnvListenWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFileName)
	body := `
[server]
listen = "127.0.0.1:18210"
[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPINDLE__SERVER__LISTEN", "127.0.0.1:18731")
	t.Setenv("PORT", "")
	cfg, msgs := setupOnlyBootConfig(&cli{configPath: path}, dir, errors.New("config invalid"))
	if cfg.Server.Listen != "127.0.0.1:18731" {
		t.Fatalf("setup-only bind address = %q, want the env overlay 127.0.0.1:18731", cfg.Server.Listen)
	}
	if len(msgs) == 0 {
		t.Fatal("boot errors must still explain the invalid file")
	}
	// The page's prefill follows the effective (env-overlaid) value.
	view, err := setupViewFn(cfg, server.BootState{Mode: "setup_only", Errors: msgs, ConfigPaths: []string{path}})(context.Background())
	if err != nil || view == nil {
		t.Fatalf("setup view: %v", err)
	}
	if got := setupViewField(t, view, "server.listen").Value; got != "127.0.0.1:18731" {
		t.Fatalf("server.listen field = %q, want the env overlay", got)
	}
}

func TestSetupOnlyBootPrefillsFileValues(t *testing.T) {
	t.Setenv("PORT", "")
	clearEnv(t, "SPINDLE__SERVER__LISTEN")
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFileName)
	body := `
[server]
listen = "127.0.0.1:18210"
public_url = "https://spindle.example.com"

[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// The serve flow's F5 branch: best-effort config + one boot error per
	// validator finding.
	cfg, parseErrs := config.LoadSetupOnly(dir, []string{path}, nil)
	if cfg.Server.Listen != "127.0.0.1:18210" {
		t.Fatalf("setup-only listen = %q, want the file's 127.0.0.1:18210", cfg.Server.Listen)
	}
	_, verrs := config.Validate(cfg)
	msgs := make([]string, 0, len(parseErrs)+len(verrs))
	for _, e := range parseErrs {
		msgs = append(msgs, e.Error())
	}
	for _, e := range verrs {
		msgs = append(msgs, e.Error())
	}
	if len(verrs) == 0 {
		t.Fatal("fixture must fail validation")
	}

	boot := server.BootState{Mode: "setup_only", Errors: msgs, ConfigPaths: []string{path}}
	view, err := setupViewFn(cfg, boot)(context.Background())
	if err != nil || view == nil {
		t.Fatalf("setup view: %v", err)
	}
	if view.FileState != "invalid" {
		t.Fatalf("file_state = %q, want invalid", view.FileState)
	}
	if got := setupViewField(t, view, "server.listen").Value; got != "127.0.0.1:18210" {
		t.Fatalf("server.listen field = %q, want the file value", got)
	}
	if got := setupViewField(t, view, "server.public_url").Value; got != "https://spindle.example.com" {
		t.Fatalf("server.public_url field = %q, want the file value", got)
	}
	if got := setupViewField(t, view, "server.auth.mode").Value; got != "oidc" {
		t.Fatalf("server.auth.mode field = %q, want the file value", got)
	}
	// The validator's findings mark the offending rows.
	if len(setupViewField(t, view, "server.auth.allowed_domains").Errors) == 0 {
		t.Fatal("server.auth.allowed_domains must carry the validator error")
	}
	if len(setupViewField(t, view, "server.auth.session_secret").Errors) == 0 {
		t.Fatal("server.auth.session_secret must carry the browser-trio error")
	}
	// Non-offending keys stay unmarked.
	if errs := setupViewField(t, view, "server.listen").Errors; len(errs) != 0 {
		t.Fatalf("server.listen must not be marked: %v", errs)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "server.auth.allowed_domains") {
		t.Fatal("boot errors must name the missing allowlist (banner source)")
	}
}
