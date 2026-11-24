// account_gate_test.go — the deactivation gate at the credential boundary
// (docs/PLAN-ADMIN.md §3.7): a disabled account is refused for every
// OIDC-derived credential, the deactivation watermark kills sessions and
// wgt_ tokens permanently, machine credentials are untouched, and a store
// error fails closed.

package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

// gateClock is a mutable clock shared by the service and the test.
type gateClock struct{ t time.Time }

func (c *gateClock) now() time.Time { return c.t }

func gateService(t *testing.T) (*AuthService, *gateClock) {
	t.Helper()
	clock := &gateClock{t: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	a := &config.Auth{
		SessionSecret:  strings.Repeat("k", 32),
		AllowedDomains: []string{"example.com"},
		WriteDomains:   []string{"example.com"},
		AdminGroups:    []string{"spindle-admin"},
		GroupsClaim:    "groups",
		SessionTTL:     config.Duration(time.Hour),
		AccessTokenTTL: config.Duration(24 * time.Hour),
		Tokens:         []config.StaticToken{{Principal: "robot", Token: "tok-machine", Write: true}},
	}
	return NewAuthService(a, clock.now), clock
}

// sessionRequest builds a GET carrying the session cookie.
func sessionRequest(wire string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "walgit_session", Value: wire})
	return r
}

func TestDisabledAccountCannotAuthenticate(t *testing.T) {
	svc, _ := gateService(t)
	sess, err := svc.MintSession("alice@example.com", "spindle-admin")
	if err != nil {
		t.Fatal(err)
	}
	svc.AccountLookup = func(username string) (auth.AccountState, error) {
		if username != "alice" {
			t.Fatalf("gate looked up %q, want the resolved username alice", username)
		}
		return auth.AccountState{Disabled: true, DisabledBy: "root", Reason: "abuse"}, nil
	}
	p, aerr := svc.Authenticate(sessionRequest(sess.Wire), nil)
	if aerr == nil {
		t.Fatalf("disabled session authenticated as %+v", p)
	}
	if aerr.Kind != auth.ErrForbidden {
		t.Fatalf("kind = %v, want ErrForbidden", aerr.Kind)
	}
	// Neutral: the refusal names neither the administrator nor the reason.
	for _, leak := range []string{"root", "abuse"} {
		if strings.Contains(aerr.Why, leak) {
			t.Fatalf("refusal %q discloses %q", aerr.Why, leak)
		}
	}
	// An ID-token-derived principal (the login callback path) takes the same
	// gate: principalFromEmailGroups is what every OIDC credential resolves
	// through.
	if _, aerr := svc.principalFromEmailGroups("alice@example.com", nil, time.Time{}); aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("principalFromEmailGroups = %v, want ErrForbidden", aerr)
	}
}

func TestRevocationWatermarkKillsSessionsAndTokens(t *testing.T) {
	svc, clock := gateService(t)
	oldSess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	oldTok, err := svc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Deactivation one minute after both were minted.
	watermark := clock.t.Add(time.Minute)
	svc.AccountLookup = func(string) (auth.AccountState, error) {
		return auth.AccountState{TokensRevokedAt: watermark}, nil
	}
	clock.t = clock.t.Add(2 * time.Minute)

	p, aerr := svc.Authenticate(sessionRequest(oldSess.Wire), nil)
	if aerr == nil || aerr.Kind != auth.ErrUnauthorized {
		t.Fatalf("pre-deactivation session = %+v, %v; want 401 (revoked, not merely disabled)", p, aerr)
	}
	if _, aerr := svc.wgtPrincipal(oldTok.Wire); aerr == nil || aerr.Kind != auth.ErrUnauthorized {
		t.Fatalf("pre-deactivation wgt_ token err = %v, want 401", aerr)
	}

	// Reactivate: the flag clears, the watermark stays — the old credentials
	// remain dead, a freshly minted one works. (The wire carries `iat` at
	// second precision, so the comparison is whole seconds; a credential
	// minted a minute before the watermark is unambiguously revoked.)
	freshSess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	freshTok, err := svc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p, aerr := svc.Authenticate(sessionRequest(oldSess.Wire), nil); aerr == nil || aerr.Kind != auth.ErrUnauthorized || p.Name != "" {
		t.Fatalf("old session revived after reactivation: %+v %v", p, aerr)
	}
	if p, aerr := svc.Authenticate(sessionRequest(freshSess.Wire), nil); aerr != nil || p.Name != "alice" {
		t.Fatalf("fresh session = %+v, %v", p, aerr)
	}
	if p, aerr := svc.wgtPrincipal(freshTok.Wire); aerr != nil || p.Name != "alice" {
		t.Fatalf("fresh wgt_ token = %+v, %v", p, aerr)
	}
}

func TestMachineCredentialsAreNotUserBound(t *testing.T) {
	svc, _ := gateService(t)
	svc.AccountLookup = func(string) (auth.AccountState, error) {
		// Even if a row with this principal's name were deactivated, a static
		// token is a configuration entry, not an account.
		return auth.AccountState{Disabled: true}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok-machine")
	p, aerr := svc.Authenticate(r, nil)
	if aerr != nil || p.Name != "robot" || p.Anonymous {
		t.Fatalf("static machine credential = %+v, %v; want it untouched by deactivation", p, aerr)
	}
}

func TestAccountLookupErrorFailsClosed(t *testing.T) {
	svc, _ := gateService(t)
	sess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	svc.AccountLookup = func(string) (auth.AccountState, error) {
		return auth.AccountState{}, errors.New("store down")
	}
	p, aerr := svc.Authenticate(sessionRequest(sess.Wire), nil)
	if aerr == nil || aerr.Kind != auth.ErrUnavailable {
		t.Fatalf("store error = %+v, %v; want ErrUnavailable (never 'not disabled')", p, aerr)
	}
	// The anonymous path never consults the directory.
	if p, aerr := svc.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil), nil); aerr != nil || !p.Anonymous {
		t.Fatalf("anonymous = %+v, %v", p, aerr)
	}
}

func TestNoDirectoryWiredAllows(t *testing.T) {
	svc, _ := gateService(t)
	sess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p, aerr := svc.Authenticate(sessionRequest(sess.Wire), nil); aerr != nil || p.Name != "alice" {
		t.Fatalf("unwired directory = %+v, %v (feature off must not lock everyone out)", p, aerr)
	}
}

// TestDisabledRefusalIsARealStatus pins the HTTP surface: the credential
// boundary answers 403 (disabled) and 401 (revoked) on a real route — never a
// 200 with an in-band message.
func TestDisabledRefusalIsARealStatus(t *testing.T) {
	s, h := oidcServer(t)
	sess, err := s.authSvc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.authSvc.AccountLookup = func(string) (auth.AccountState, error) {
		return auth.AccountState{Disabled: true}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/_auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled /_auth/me = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"principal"`) {
		t.Fatalf("disabled response leaked a principal body: %s", rec.Body.String())
	}
	s.authSvc.AccountLookup = func(string) (auth.AccountState, error) {
		return auth.AccountState{TokensRevokedAt: s.Now().Add(time.Second)}, nil
	}
	// Same cookie, now with a watermark at its mint time → 401 (revoked, not
	// merely disabled).
	req = httptest.NewRequest(http.MethodGet, "/_auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked /_auth/me = %d, want 401", rec.Code)
	}
}
