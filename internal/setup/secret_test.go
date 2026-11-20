package setup

import (
	"net/url"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestIsSecretKey(t *testing.T) {
	rows := []struct {
		key  string
		want bool
	}{
		{"server.auth.session_secret", true},
		{"server.auth.oauth_client_secret", true},
		{"server.auth.token", true},
		{"events.webhook_secret", true},
		{"wal.push_broker_token", true},
		// Locations, durations and paths, not values.
		{"server.auth.token_env", false},
		{"server.auth.access_token_ttl", false},
		{"store.s3.secret_key_env", false},
		{"server.ssh.host_key", false},
		{"server.auth.tokens", false},
		// Ordinary settings.
		{"server.listen", false},
		{"server.auth.issuer", false},
	}
	for _, tc := range rows {
		if got := IsSecretKey(tc.key); got != tc.want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestParseValuesSecretsAreOverwriteOnly pins UI-04's save contract: the page
// only ever sees the mask, so a round-tripped mask (or an empty field) keeps
// the stored secret; a different value overwrites it.
func TestParseValuesSecretsAreOverwriteOnly(t *testing.T) {
	base := config.Defaults()
	base.Server.Auth.SessionSecret = "stored-session"
	base.Server.Auth.OAuthClientSecret = "stored-oauth"
	base.Events.WebhookSecret = "stored-hook"
	base.WAL.PushBrokerToken = "stored-token"
	base.Server.Listen = "0.0.0.0:8080"

	v := url.Values{}
	v.Set("server.auth.session_secret", SecretMask) // untouched row
	v.Set("server.auth.oauth_client_secret", "")    // empty = unchanged
	v.Set("events.webhook_secret", "new-hook")      // overwrite
	v.Set("wal.push_broker_token", SecretMask)
	v.Set("store.s3.secret_key_env", "MY_S3_SECRET") // an env NAME, not a secret
	v.Set("server.listen", "127.0.0.1:9999")

	c, err := ParseValues(v, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Auth.SessionSecret != "stored-session" {
		t.Fatalf("mask overwrote the session secret: %q", c.Server.Auth.SessionSecret)
	}
	if c.Server.Auth.OAuthClientSecret != "stored-oauth" {
		t.Fatalf("empty overwrote the oauth secret: %q", c.Server.Auth.OAuthClientSecret)
	}
	if c.Events.WebhookSecret != "new-hook" {
		t.Fatalf("explicit secret not applied: %q", c.Events.WebhookSecret)
	}
	if c.WAL.PushBrokerToken != "stored-token" {
		t.Fatalf("mask overwrote the push broker token: %q", c.WAL.PushBrokerToken)
	}
	if c.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("ordinary key not applied: %q", c.Server.Listen)
	}
}
