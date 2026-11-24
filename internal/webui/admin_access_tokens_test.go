package webui

// admin_access_tokens_test.go — the Access page's machine-credential table
// after the lifecycle fields landed (06_server_http.md §8.2.1): identity (name
// or hash prefix, never the secret), expiry or the "never expires" warning,
// and the last-used timestamp with its truncated source. Usage is read once
// from auth/token-usage.json; an absent or corrupt object degrades to "not
// recorded yet" without failing the page.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func accessTokenConfig() *config.Config {
	cfg := adminAccessTestConfig()
	cfg.Server.Auth.Tokens = []config.StaticToken{
		// Named, expiring in the future, with a recorded use.
		{Principal: "ci-bot", Name: "ci-old", Token: "old-secret-value", Write: true,
			ExpiresAt: "2027-01-01T00:00:00Z"},
		// Unnamed literal: identified by its hash prefix.
		{Principal: "deploy", Token: "literal-secret-value", Write: true},
		// Expired.
		{Principal: "legacy", Name: "legacy", Token: "legacy-secret", ExpiresAt: "2020-01-01T00:00:00Z"},
		// Misconfigured expiry (validation would refuse the boot; the page is honest).
		{Principal: "typo", Name: "typo", Token: "typo-secret", ExpiresAt: "soon"},
	}
	return cfg
}

func seedTokenUsage(t *testing.T, st store.ObjectStore) {
	t.Helper()
	doc := auth.NewTokenUsageDoc()
	doc.Set("ci-old", auth.TokenUsageEntry{Name: "ci-old", Principal: "ci-bot",
		LastUsed: "2026-11-23T10:00:00Z", Source: "198.51.100.4", UserAgent: "git/2.55.0"})
	doc.Set(auth.TokenUsageIdentity("", "literal-secret-value"), auth.TokenUsageEntry{
		Principal: "deploy", LastUsed: "2026-11-22T09:30:00Z", Source: "203.0.113.9"})
	raw := doc.Encode()
	if _, err := store.PutBytes(context.Background(), st, auth.TokenUsageKey, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminAccessTokenLifecycleTable(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), adminAccessAdminPrincipal())
	st := store.NewMemory()
	s.env.Cfg = accessTokenConfig()
	s.env.API = &api.Env{Store: st}
	s.env.Now = func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }
	seedTokenUsage(t, st)

	body := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()

	// Identity: the configured name, and a hash prefix for the unnamed row.
	settingsMustContain(t, body, "ci-old", "named token identity")
	sum := sha256.Sum256([]byte("literal-secret-value"))
	prefix := "sha256:" + hex.EncodeToString(sum[:6])
	settingsMustContain(t, body, prefix, "unnamed token identified by hash prefix")

	// Expiry states: a real instant, "never expires" as text (not colour
	// alone), an expired row, and the honest invalid spelling.
	settingsMustContain(t, body, "2027-01-01T00:00:00Z", "future expiry")
	settingsMustContain(t, body, "never expires", "never-expiring warning")
	settingsMustContain(t, body, "2020-01-01T00:00:00Z", "expired row's instant")
	settingsMustContain(t, body, "invalid expiry", "unparseable expiry")
	settingsMustContain(t, body, "configured token", "the summary warning")

	// Last used + source, and the honest empty state for the unobserved rows.
	settingsMustContain(t, body, "198.51.100.4 · git/2.55.0", "last-used source and agent")
	settingsMustContain(t, body, "203.0.113.9", "unnamed token's source")
	settingsMustContain(t, body, "not recorded yet", "unobserved row")

	// No secret is ever rendered.
	for _, secret := range []string{"old-secret-value", "literal-secret-value", "legacy-secret", "typo-secret"} {
		settingsMustNotContain(t, body, secret, "the token table must never render a secret")
	}

	// The rotation procedure and its honest downtime statement are visible.
	settingsMustContain(t, body, "Rotation without downtime is a rolling deploy", "rotation heading")
	settingsMustContain(t, body, "no in-process config reload", "honest single-instance statement")

	// The table's columns are labelled and semantic.
	settingsMustContain(t, body, `aria-label="static machine tokens"`, "table accessible name")
	for _, col := range []string{"name / principal", "scope", "expires", "last used"} {
		settingsMustContain(t, body, ">"+col+"<", "column "+col)
	}
}

// TestAdminAccessTokenTableDegradesWithoutUsage: a corrupt usage object is not
// a page failure — the table renders with "not recorded yet".
func TestAdminAccessTokenTableDegradesWithoutUsage(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), adminAccessAdminPrincipal())
	st := store.NewMemory()
	s.env.Cfg = accessTokenConfig()
	s.env.API = &api.Env{Store: st}
	if _, err := store.PutBytes(context.Background(), st, auth.TokenUsageKey,
		[]byte("{corrupt"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	rec := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("access page with corrupt usage = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not recorded yet", "degraded last-used cell")
}
