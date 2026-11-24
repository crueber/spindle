package setup

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

// FillEffectiveSecrets is the admin save's bridge between a masked form field
// and a secret that lives in the ENVIRONMENT rather than the file: it fills an
// empty secret scalar for validation without touching the candidate's other
// values (and never mutates the candidate itself).
func TestFillEffectiveSecrets(t *testing.T) {
	cand := config.Defaults()
	eff := config.Defaults()
	eff.Server.Auth.OAuthClientSecret = "client-secret"
	eff.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	eff.Server.Listen = "0.0.0.0:9999" // non-secret: must not be copied

	got := FillEffectiveSecrets(cand, eff)
	if got.Server.Auth.OAuthClientSecret != "client-secret" {
		t.Fatalf("oauth client secret = %q", got.Server.Auth.OAuthClientSecret)
	}
	if got.Server.Auth.SessionSecret != strings.Repeat("s", 32) {
		t.Fatal("session secret not filled")
	}
	if got.Server.Listen == "0.0.0.0:9999" {
		t.Fatal("a non-secret value was copied")
	}
	if cand.Server.Auth.OAuthClientSecret != "" || cand.Server.Auth.SessionSecret != "" {
		t.Fatal("the candidate was mutated")
	}

	// A value the file already carries is authoritative (the operator may be
	// rotating it); only empties are filled.
	cand.Server.Auth.SessionSecret = "own-value"
	got = FillEffectiveSecrets(cand, eff)
	if got.Server.Auth.SessionSecret != "own-value" {
		t.Fatalf("existing secret overwritten: %q", got.Server.Auth.SessionSecret)
	}
}
