package setup

// tomlview_test.go — TOMLView is the Configuration page's read-only document:
// MarshalTOML's exact bytes with every secret value replaced by SecretMask
// (secrets never leave the server — PLAN-ADMIN §5).

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestTOMLViewMasksEverySecret(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.SessionSecret = "session-secret-value"
	c.Server.SecretsKey = "instance-secrets-key"
	c.Events.WebhookSecret = "webhook-secret-value"
	c.Server.Auth.Tokens = []config.StaticToken{{
		Principal: "alice@example.com", Token: "literal-token-value",
	}}

	view, err := TOMLView(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.MarshalTOML(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"session-secret-value", "instance-secrets-key", "webhook-secret-value", "literal-token-value"} {
		if bytes.Contains(view, []byte(secret)) {
			t.Fatalf("TOMLView leaked %q", secret)
		}
		if !bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("control: MarshalTOML should contain %q (the view masks it)", secret)
		}
		if !bytes.Contains(view, []byte(SecretMask)) {
			t.Fatalf("TOMLView does not carry the mask for %q", secret)
		}
	}
}

func TestTOMLViewIsOtherwiseByteIdenticalToSave(t *testing.T) {
	c := config.Defaults()
	c.Server.Listen = "127.0.0.1:18080"
	c.Server.PublicURL = "https://spindle.example.com"
	c.Cache.MaxBytes = 64 << 30
	c.Bundles.Strategy = []config.BundleStrategy{{Name: "base", Kind: "full", Schedule: "0 0 * * * *"}}

	view, err := TOMLView(c)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := config.MarshalTOML(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(view, saved) {
		t.Fatalf("a secret-free config's view drifted from Save's document\n--- view ---\n%s\n--- save ---\n%s", view, saved)
	}
	for _, want := range []string{"listen = ", "https://spindle.example.com", "64GiB", "[[bundles.strategy]]"} {
		if !strings.Contains(string(view), want) {
			t.Fatalf("view missing %q\n%s", want, view)
		}
	}
}
