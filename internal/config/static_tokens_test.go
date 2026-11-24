package config

// static_tokens_test.go — the static machine token's lifecycle fields
// (06_server_http.md §8.2.1): optional name, optional RFC 3339 expires_at,
// accepted at load unless unparseable; a past instant is valid and simply
// already expired.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authWithTokens(tokens ...StaticToken) *Config {
	c := validOIDC()
	c.Server.Auth.Tokens = tokens
	return c
}

func TestValidateStaticTokenNames(t *testing.T) {
	t.Run("duplicate name", func(t *testing.T) {
		_, errs := Validate(authWithTokens(
			StaticToken{Principal: "a", Token: "t1", Name: "ci"},
			StaticToken{Principal: "b", Token: "t2", Name: " ci "},
		))
		errsContain(t, errs, "server.auth.tokens[1].name")
		errsContain(t, errs, "is used twice")
	})
	t.Run("unnamed rows are fine and do not collide", func(t *testing.T) {
		_, errs := Validate(authWithTokens(
			StaticToken{Principal: "a", Token: "t1"},
			StaticToken{Principal: "b", Token: "t2", Name: "   "},
		))
		if len(errs) != 0 {
			t.Fatalf("unnamed rows must validate: %v", errs)
		}
	})
}

func TestValidateStaticTokenExpiry(t *testing.T) {
	t.Run("RFC 3339 accepted", func(t *testing.T) {
		for _, exp := range []string{
			"2026-12-31T23:59:59Z",
			"2026-12-31T23:59:59.500Z",
			"2026-12-31T23:59:59+02:00",
		} {
			_, errs := Validate(authWithTokens(StaticToken{Principal: "ci", Token: "t", ExpiresAt: exp}))
			if len(errs) != 0 {
				t.Fatalf("%q must validate: %v", exp, errs)
			}
		}
	})
	t.Run("past instant is allowed (already expired, not an error)", func(t *testing.T) {
		_, errs := Validate(authWithTokens(StaticToken{
			Principal: "ci", Token: "t",
			ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}))
		if len(errs) != 0 {
			t.Fatalf("a past expires_at is legal: %v", errs)
		}
	})
	t.Run("malformed rejected with its key", func(t *testing.T) {
		for _, exp := range []string{"next tuesday", "2026-12-31", "2026-13-01T00:00:00Z"} {
			_, errs := Validate(authWithTokens(StaticToken{Principal: "ci", Token: "t", ExpiresAt: exp}))
			errsContain(t, errs, "server.auth.tokens[0].expires_at")
			errsContain(t, errs, "RFC 3339")
		}
	})
}

// TestLoadRejectsMalformedStaticTokenExpiry is the "validated at load" gate:
// LoadExplicit runs Validate, so a bad expires_at refuses to boot rather than
// booting a credential with unknown lifetime.
func TestLoadRejectsMalformedStaticTokenExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	body := `
[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "id"
oauth_client_secret = "secret"
session_secret = "0123456789abcdef0123456789abcdef"

[[server.auth.tokens]]
principal = "ci-bot"
name = "ci-bot"
token = "tok"
expires_at = "2027-01-01"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadExplicit(path, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "server.auth.tokens[0].expires_at") {
		t.Fatalf("load error = %v, want the expires_at key", err)
	}
}
