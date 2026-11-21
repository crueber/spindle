package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSetupOnlyConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clearSetupEnv removes a SPINDLE__* override for the test (t.Setenv cannot
// express "unset").
func clearSetupEnv(t *testing.T, key string) {
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

// TestLoadSetupOnlyHonoursFileListen is F5: an invalid config still boots on
// the listener the file names, and the setup page's base carries every
// parseable file value instead of the first-run defaults.
func TestLoadSetupOnlyHonoursFileListen(t *testing.T) {
	clearSetupEnv(t, "SPINDLE__SERVER__LISTEN")
	path := writeSetupOnlyConfig(t, `
[server]
listen = "127.0.0.1:18210"
public_url = "https://spindle.example.com"
auto_create_on_push = false

[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
# deliberately missing allowed_domains + the browser-login trio: invalid
`)
	cfg, errs := LoadSetupOnly("/data", []string{path}, func(string) string { return "" })
	if len(errs) != 0 {
		t.Fatalf("best-effort parse reported %v", errs)
	}
	if cfg.Server.Listen != "127.0.0.1:18210" {
		t.Fatalf("listen = %q, want the file value 127.0.0.1:18210", cfg.Server.Listen)
	}
	if cfg.Server.PublicURL != "https://spindle.example.com" {
		t.Fatalf("public_url = %q, want the file value", cfg.Server.PublicURL)
	}
	if cfg.Server.AutoCreateOnPush {
		t.Fatal("auto_create_on_push = true, want the file's false")
	}
	if cfg.Server.Auth.Mode != "oidc" || cfg.Server.Auth.Issuer != "https://id.example.com" {
		t.Fatalf("auth = %+v, want the file values", cfg.Server.Auth)
	}
	// The file really is invalid: the boot banner must still explain why.
	if _, verrs := Validate(cfg); len(verrs) == 0 {
		t.Fatal("the fixture must fail validation (that is setup-only mode)")
	}
}

// TestLoadSetupOnlyFallsBackFromUnbindableListen: a malformed listen cannot be
// bound, so the recovery page would be unreachable — bind the default and
// report the file's bad value.
func TestLoadSetupOnlyFallsBackFromUnbindableListen(t *testing.T) {
	clearSetupEnv(t, "SPINDLE__SERVER__LISTEN")
	path := writeSetupOnlyConfig(t, "[server]\nlisten = \"not-a-host-port\"\n")
	cfg, errs := LoadSetupOnly("/data", []string{path}, func(string) string { return "" })
	if cfg.Server.Listen != Defaults().Server.Listen {
		t.Fatalf("listen = %q, want the default %q", cfg.Server.Listen, Defaults().Server.Listen)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "server.listen") || !strings.Contains(joined, "not-a-host-port") {
		t.Fatalf("errors = %q, want the bad server.listen named", joined)
	}
}

// TestLoadSetupOnlyKeepsParseableValuesOnUnknownKey: an unknown key is an error
// but must not discard the rest of the file (the operator sees their values and
// the one key to remove).
func TestLoadSetupOnlyKeepsParseableValuesOnUnknownKey(t *testing.T) {
	clearSetupEnv(t, "SPINDLE__SERVER__LISTEN")
	path := writeSetupOnlyConfig(t, `
[server]
listen = "0.0.0.0:19001"

[server.nonsense]
x = 1
`)
	cfg, errs := LoadSetupOnly("/data", []string{path}, func(string) string { return "" })
	if cfg.Server.Listen != "0.0.0.0:19001" {
		t.Fatalf("listen = %q, want the parseable file value", cfg.Server.Listen)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "unknown key") {
		t.Fatalf("errors = %q, want an unknown-key report", joined)
	}
}

// TestLoadSetupOnlyEnvListenWinsOverInvalidFile is F5(b): in setup-only mode
// the env overlay still wins over the file, exactly as it does on a valid boot.
// The served config's Listen is the address serveHTTP binds, so this asserts the
// bind address, not just a rendered value.
func TestLoadSetupOnlyEnvListenWinsOverInvalidFile(t *testing.T) {
	path := writeSetupOnlyConfig(t, `
[server]
listen = "127.0.0.1:18210"
[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
`)
	t.Setenv("SPINDLE__SERVER__LISTEN", "127.0.0.1:18731")
	cfg, errs := LoadSetupOnly("/data", []string{path}, nil)
	if cfg.Server.Listen != "127.0.0.1:18731" {
		t.Fatalf("listen = %q, want the env overlay 127.0.0.1:18731", cfg.Server.Listen)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	// The file is still invalid for its own reasons: setup-only mode is real.
	if _, verrs := Validate(cfg); len(verrs) == 0 {
		t.Fatal("the fixture must still fail validation")
	}
}

// TestLoadSetupOnlyAppliesEnvAndPort: the setup-only boot still honours the
// env overlay, and PORT moves the port on the file's host (not the default's).
func TestLoadSetupOnlyAppliesEnvAndPort(t *testing.T) {
	path := writeSetupOnlyConfig(t, "[server]\nlisten = \"127.0.0.1:18210\"\n")
	t.Setenv("SPINDLE__SERVER__PUBLIC_URL", "https://env.example.com")
	t.Setenv("PORT", "19443")
	cfg, errs := LoadSetupOnly("/data", []string{path}, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.Server.PublicURL != "https://env.example.com" {
		t.Fatalf("public_url = %q, want the env overlay", cfg.Server.PublicURL)
	}
	if cfg.Server.Listen != "127.0.0.1:19443" {
		t.Fatalf("listen = %q, want the PORT lockstep on the file host", cfg.Server.Listen)
	}
}

// TestLoadSetupOnlyFallbacks covers the no-file, unparseable, unknown-section,
// read-error and overlay-error branches: none may panic or discard the rest.
func TestLoadSetupOnlyFallbacks(t *testing.T) {
	clearSetupEnv(t, "SPINDLE__SERVER__LISTEN")
	t.Run("no file is the first-run shape", func(t *testing.T) {
		cfg, errs := LoadSetupOnly("/data", []string{filepath.Join(t.TempDir(), "absent.toml")}, nil)
		if len(errs) != 0 {
			t.Fatalf("errors = %v", errs)
		}
		if cfg.Store.Backend != "filesystem" || cfg.Server.Listen != "0.0.0.0:8080" {
			t.Fatalf("cfg = backend %q listen %q, want the first-run defaults", cfg.Store.Backend, cfg.Server.Listen)
		}
	})
	t.Run("malformed TOML keeps the defaults", func(t *testing.T) {
		path := writeSetupOnlyConfig(t, "[server\nlisten=")
		cfg, errs := LoadSetupOnly("/data", []string{path}, nil)
		if len(errs) == 0 {
			t.Fatal("malformed TOML must be reported")
		}
		if cfg.Server.Listen != Defaults().Server.Listen {
			t.Fatalf("listen = %q, want the default", cfg.Server.Listen)
		}
	})
	t.Run("removed server.tls key gets the hint", func(t *testing.T) {
		path := writeSetupOnlyConfig(t, "[server.tls]\ncert = \"x\"\n")
		_, errs := LoadSetupOnly("/data", []string{path}, nil)
		joined := ""
		for _, e := range errs {
			joined += e.Error() + "\n"
		}
		if !strings.Contains(joined, "server.tls") || !strings.Contains(joined, "reverse proxy") {
			t.Fatalf("errors = %q, want the removed-TLS hint", joined)
		}
	})
	t.Run("unreadable candidate is reported", func(t *testing.T) {
		dir := t.TempDir() // a directory candidate: ReadFile fails
		_, errs := LoadSetupOnly("/data", []string{dir}, nil)
		if len(errs) == 0 {
			t.Fatal("a read error must be reported")
		}
	})
	t.Run("bad env override keeps the file values", func(t *testing.T) {
		path := writeSetupOnlyConfig(t, "[server]\nlisten = \"127.0.0.1:18210\"\n")
		t.Setenv("SPINDLE__SERVER__MAX_CONCURRENT_REQUESTS", "lots")
		cfg, errs := LoadSetupOnly("/data", []string{path}, nil)
		if len(errs) == 0 {
			t.Fatal("a bad env override must be reported")
		}
		if cfg.Server.Listen != "127.0.0.1:18210" {
			t.Fatalf("listen = %q, want the file value preserved", cfg.Server.Listen)
		}
	})
	t.Run("bad PORT is reported", func(t *testing.T) {
		path := writeSetupOnlyConfig(t, "[server]\nlisten = \"127.0.0.1:18210\"\n")
		t.Setenv("PORT", "not-a-port")
		_, errs := LoadSetupOnly("/data", []string{path}, nil)
		if len(errs) == 0 {
			t.Fatal("an invalid PORT must be reported")
		}
	})
}

// TestLoadSetupBase pins the setup edit base: file values over defaults, the
// first-run shape when no candidate exists, and an error (not a fallback) for
// an unparseable file — the caller owns that fallback.
func TestLoadSetupBase(t *testing.T) {
	path := writeSetupOnlyConfig(t, "[server]\nlisten = \"127.0.0.1:18210\"\n")
	cfg, err := LoadSetupBase("/data", []string{path})
	if err != nil || cfg.Server.Listen != "127.0.0.1:18210" {
		t.Fatalf("LoadSetupBase = %v %v", cfg, err)
	}
	cfg, err = LoadSetupBase("/data", []string{filepath.Join(t.TempDir(), "absent.toml")})
	if err != nil || cfg.Store.Backend != "filesystem" || cfg.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("absent candidate = %+v %v, want the first-run shape", cfg, err)
	}
	bad := writeSetupOnlyConfig(t, "[server\n")
	if _, err := LoadSetupBase("/data", []string{bad}); err == nil {
		t.Fatal("an unparseable file must return the error")
	}
}

// TestValidateListenShape pins the new pre-boot check behind F5's banner.
func TestValidateListenShape(t *testing.T) {
	c := Defaults()
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	c.Server.Listen = "8080"
	if _, errs := Validate(c); len(errs) == 0 {
		t.Fatal("listen without a host must fail")
	}
	c.Server.Listen = ""
	if _, errs := Validate(c); len(errs) == 0 {
		t.Fatal("empty listen must fail")
	}
}
