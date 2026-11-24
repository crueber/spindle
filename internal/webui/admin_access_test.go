package webui

// admin_access_test.go — M4, the full Access page (docs/PLAN-ADMIN.md §3.4 as
// amended by PLAN-OIDC-ONLY): every fact the page renders, its config-row
// links, the live "your access" diagnostic, the static-token table (never a
// secret), the reused discovery probe, and the non-admin denied view that must
// not leak the configured group names or issuer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
)

// adminAccessTestConfig is a complete OIDC set with every Access fact made
// distinguishable, so a missing section fails by name.
func adminAccessTestConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Server.PublicURL = ""
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.OAuthClientID = "spindle-client"
	cfg.Server.Auth.OAuthClientSecret = "client-secret-value"
	cfg.Server.Auth.GroupsClaim = "groups"
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin", "ops-admins"}
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.AllowedEmails = []string{"cto@elsewhere.org"}
	cfg.Server.Auth.WriteDomains = []string{"example.com"}
	cfg.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	cfg.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "ci-bot", Token: "literal-secret-value", Write: true},
		{Principal: "reader", TokenEnv: "CI_READ_TOKEN"},
		{Principal: "no-credential"},
	}
	return cfg
}

func adminAccessAdminPrincipal() auth.Principal {
	return auth.Principal{
		Name: "alice", Email: "alice@example.com", Write: true, Admin: true,
		Groups: []string{"spindle-admin"},
	}
}

// TestAdminAccessPageRendersEverySection: an admin sees the six cards, every
// configured fact beside the config row it comes from (the M3 anchor), the
// redirect URI copy affordance, the token table, and the reused discovery form.
func TestAdminAccessPageRendersEverySection(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), adminAccessAdminPrincipal())
	s.env.Cfg = adminAccessTestConfig()

	body := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()

	for _, want := range []string{
		"Sign-in (OIDC)", "Your access", "Admission rules",
		"Machine credentials", "Discovery test", "Sessions",
		"https://id.example.com", "spindle-client", "http://example.com/_auth/callback",
		"spindle-admin", "ops-admins",
		"example.com", "cto@elsewhere.org",
		"ci-bot", "token_env CI_READ_TOKEN", "never expires",
		"session lifetime", "access token lifetime", "30d", "90d",
		"re-derived on every request",
	} {
		settingsMustContain(t, body, want, "access page section/fact")
	}

	// Every fact names its config row and links to the editor's row anchor.
	for _, key := range []string{
		"server.auth.issuer", "server.auth.oauth_client_id", "server.auth.groups_claim",
		"server.auth.admin_groups", "server.auth.anonymous_read",
		"server.auth.allowed_domains", "server.auth.allowed_emails", "server.auth.write_domains",
		"server.auth.session_ttl", "server.auth.access_token_ttl",
		"server.auth.session_secret", "server.auth.oauth_client_secret",
		"server.auth.tokens", "server.public_url",
	} {
		settingsMustContain(t, body, `href="/admin/config#row-`+key+`"`, "config row link "+key)
	}

	// The redirect URI is a copyable fact, and the discovery result is a polite
	// live region fed by the EXISTING probe route (no invented route).
	settingsMustContain(t, body, `id="access-redirect-uri"`, "redirect URI target")
	settingsMustContain(t, body, `data-copy="#access-redirect-uri"`, "redirect URI copy button")
	settingsMustContain(t, body, `id="admin-access-auth-test" aria-live="polite"`, "discovery live region")
	settingsMustContain(t, body, `action="/admin/config/auth-test"`, "discovery form reuses the probe")
	settingsMustNotContain(t, body, `action="/admin/access`, "no invented access write route")

	if n := a11yH1Count(body); n != 1 {
		t.Fatalf("access page h1 count = %d, want 1", n)
	}
}

// TestAdminAccessTokenTableNeverRendersSecret: the static-token table shows the
// principal, scope and credential origin, and the existing masked-secret
// spelling — never the literal token or the client secret.
func TestAdminAccessTokenTableNeverRendersSecret(t *testing.T) {
	s := adminTestPage(t, adminBaseDeps(), adminAccessAdminPrincipal())
	s.env.Cfg = adminAccessTestConfig()

	body := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	for _, secret := range []string{"literal-secret-value", "client-secret-value", strings.Repeat("s", 32)} {
		settingsMustNotContain(t, body, secret, "the page must never render a stored secret")
	}
	settingsMustContain(t, body, setup.SecretMask, "the masked-secret spelling")
	settingsMustContain(t, body, "read-only", "a read-only token's scope")
	settingsMustContain(t, body, "write", "a write token's scope")
}

// TestAdminAccessNonAdminDoesNotLeak: a signed-in non-admin gets the API's 403
// and the designed denied view, and the body contains none of the configured
// admin group names, the issuer, the client id, or a config-row link.
func TestAdminAccessNonAdminDoesNotLeak(t *testing.T) {
	p := auth.Principal{Name: "mallory", Email: "mallory@example.com", Write: true,
		Groups: []string{"engineering"}}
	s := adminTestPage(t, adminBaseDeps(), p)
	s.env.Cfg = adminAccessTestConfig()

	rec := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /admin/access as non-admin = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "Access restricted", "the designed denied view")
	for _, leak := range []string{
		"spindle-admin", "ops-admins", "https://id.example.com", "spindle-client",
		"#row-server.auth", "#row-server.public_url", "ci-bot", "client-secret-value",
	} {
		settingsMustNotContain(t, body, leak, "non-admin leak")
	}
	// Their OWN verified groups are self-data, not a leak, and remain useful.
	settingsMustContain(t, body, "engineering", "the caller's own groups")
	settingsMustContain(t, body, "not shown outside the Administration area", "the no-leak statement")
}

// TestAdminAccessDiagnosticShowsBothLists: the page state a "valid config, wrong
// group" lockout needs — the session's delivered groups side by side with the
// configured admin groups and the concrete fix. Rendered through the area's
// open gate (the restricted-boot shape the gate matrix pins).
func TestAdminAccessDiagnosticShowsBothLists(t *testing.T) {
	deps := adminBaseDeps()
	deps.Access = func(*http.Request, auth.Principal) *auth.AuthError { return nil }
	p := auth.Principal{Name: "bob", Email: "bob@example.com", Write: true,
		Groups: []string{"engineering", "contractors"}}
	s := adminTestPage(t, deps, p)
	s.env.Cfg = adminAccessTestConfig()

	rec := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/access = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "The two group lists do not intersect", "the diagnostic")
	settingsMustContain(t, body, "engineering, contractors", "the delivered groups")
	settingsMustContain(t, body, "spindle-admin", "the configured group (admin-only page)")
	settingsMustContain(t, body, "sign out and sign in again", "the concrete fix")
	settingsMustContain(t, body, "server.auth.groups_claim", "the claim to check")
	settingsMustContain(t, body, "This session delivered", "both lists, labelled")
}

// TestAdminAccessDiscoveryNamesTheFailure: the probe's result is a polite live
// region and carries the precise failure the server reported.
func TestAdminAccessDiscoveryNamesTheFailure(t *testing.T) {
	deps := adminBaseDeps()
	deps.AuthTest = func(context.Context, string, string, string) (*AdminAuthTest, []AdminError) {
		return nil, []AdminError{{Key: "server.auth.issuer",
			Message: "discovery fetch failed: dial tcp 127.0.0.1:9: connect: connection refused"}}
	}
	s := adminTestPage(t, deps, adminAccessAdminPrincipal())
	s.env.Cfg = adminAccessTestConfig()

	page := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil).Body.String()
	settingsMustContain(t, page, `aria-live="polite"`, "the live region")

	rec := settingsDo(t, s, http.MethodPost, "/admin/config/auth-test",
		url.Values{"issuer": {"https://id.example.com"}, "client_id": {"spindle-client"},
			"redirect_uri": {"http://example.com/_auth/callback"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/config/auth-test = %d, want 200", rec.Code)
	}
	frag := rec.Body.String()
	settingsMustContain(t, frag, `aria-live="polite"`, "the fragment's live region")
	settingsMustContain(t, frag, "discovery fetch failed", "the failure's first line")
	settingsMustContain(t, frag, "connection refused", "the failure's cause")
}

// TestAdminAccessCardBranches covers the card builder's display arms: no config,
// a default claim name, and a row whose credential resolves to nothing.
func TestAdminAccessCardBranches(t *testing.T) {
	t.Parallel()
	c := adminAccessCard(nil, auth.Anonymous(), nil, nil)
	if c.Configured || c.Tokens != nil {
		t.Fatalf("nil config card = %+v", c)
	}

	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "empty"}}
	c = adminAccessCard(cfg, auth.Principal{Name: "a", Admin: true},
		httptest.NewRequest(http.MethodGet, "http://example.com/admin/access", nil), nil)
	if !c.Configured {
		t.Fatal("defaults config not marked configured")
	}
	found := false
	for _, f := range c.SignIn {
		if f.Key == "server.auth.groups_claim" {
			found = f.Value == "groups"
		}
	}
	if !found {
		t.Fatalf("empty groups_claim did not render the default: %+v", c.SignIn)
	}
	if len(c.Tokens) != 1 || !strings.Contains(c.Tokens[0].Credential, "dropped at startup") {
		t.Fatalf("empty token row = %+v", c.Tokens)
	}
}
