package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

// postTokenSignIn drives POST /_auth/token.
func postTokenSignIn(t *testing.T, h http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://x/_auth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" && c.Value != "" {
			return c
		}
	}
	return nil
}

// TestTokenSignInMintsAdminSession is the F1 acceptance path: an admin static
// token pasted on the setup page mints a signed session, the cookie
// authenticates through the token-mode tree, /setup renders the form (200), and
// removing the static token invalidates the session on the next request
// (fail-closed — the session names the principal, it does not copy the token).
func TestTokenSignInMintsAdminSession(t *testing.T) {
	s, h := newTestServer(t, nil)
	rec := postTokenSignIn(t, h, url.Values{"token": {"tok123"}, "next": {"/setup"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-in status = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/setup" {
		t.Fatalf("location = %q, want /setup", loc)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no walgit_session cookie minted")
	}
	if !c.HttpOnly || c.MaxAge <= 0 {
		t.Fatalf("cookie attributes = %+v, want HttpOnly with a positive MaxAge", c)
	}

	// The cookie authenticates: /api/v1/setup is admin-only and answers 200.
	req := httptest.NewRequest(http.MethodGet, "http://x/api/v1/setup", nil)
	req.AddCookie(c)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("setup with session = %d, want 200 (%s)", rec2.Code, rec2.Body.String())
	}

	// Fail closed: the token disappears from server.auth.tokens → the session
	// no longer resolves (PrincipalForName finds no live credential).
	s.cfg.Server.Auth.Tokens = nil
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	req = httptest.NewRequest(http.MethodGet, "http://x/api/v1/setup", nil)
	req.AddCookie(c)
	rec4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec4, req)
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("removed-token session = %d, want 401", rec4.Code)
	}
}

// TestTokenSignInRejectsBadAndNonAdminTokens pins the status matrix and that a
// rejected value is never echoed into the response.
func TestTokenSignInRejectsBadAndNonAdminTokens(t *testing.T) {
	_, h := newTestServer(t, func(o *Options) {
		o.Config.Server.Auth.Tokens = append(o.Config.Server.Auth.Tokens,
			config.StaticToken{Principal: "reader", Token: "read-tok", Write: true})
	})
	t.Run("invalid token is 401", func(t *testing.T) {
		rec := postTokenSignIn(t, h, url.Values{"token": {"SUPERSECRET-nope"}})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "SUPERSECRET-nope") {
			t.Fatalf("submitted token echoed into the error page:\n%s", rec.Body.String())
		}
		if sessionCookie(rec) != nil {
			t.Fatal("no session may be minted for an invalid token")
		}
	})
	t.Run("valid non-admin token is 403", func(t *testing.T) {
		rec := postTokenSignIn(t, h, url.Values{"token": {"read-tok"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if sessionCookie(rec) != nil {
			t.Fatal("no session may be minted for a non-admin token")
		}
	})
	t.Run("empty token is 401", func(t *testing.T) {
		rec := postTokenSignIn(t, h, url.Values{"token": {""}})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

// TestTokenSignInGates covers the endpoint's guards: cross-site POSTs are
// refused before the token is read, a token host without a session secret
// answers 501 (never a half-minted cookie), and OIDC mode keeps the endpoint
// absent (404) so the OIDC flow is unaffected.
func TestTokenSignInGates(t *testing.T) {
	t.Run("cross-site refused", func(t *testing.T) {
		_, h := newTestServer(t, nil)
		req := httptest.NewRequest(http.MethodPost, "http://x/_auth/token",
			strings.NewReader("token=tok123"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://evil.test")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("no session secret", func(t *testing.T) {
		_, h := newTestServer(t, func(o *Options) {
			o.Config.Server.Auth.SessionSecret = ""
			o.Config.Server.Auth.Mode = "token"
		})
		rec := postTokenSignIn(t, h, url.Values{"token": {"tok123"}})
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
		if sessionCookie(rec) != nil {
			t.Fatal("no session may be minted without a session_secret")
		}
		// The browser page is honest for token mode: no OIDC blame.
		req := httptest.NewRequest(http.MethodGet, "http://x/_auth/login?next=/setup", nil)
		req.Header.Set("Accept", "text/html")
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotImplemented {
			t.Fatalf("/_auth/login status = %d, want 501", rec2.Code)
		}
		if strings.Contains(rec2.Body.String(), "OIDC") {
			t.Fatalf("token-mode 501 must not mention OIDC:\n%s", rec2.Body.String())
		}
	})
	t.Run("oidc mode has no token endpoint", func(t *testing.T) {
		_, h := newTestServer(t, func(o *Options) {
			o.Config.Server.Auth.Mode = "oidc"
			o.Config.Server.Auth.OAuthClientID = "client"
			o.Config.Server.Auth.OAuthClientSecret = "secret"
			o.Config.Server.Auth.AllowedDomains = []string{"example.com"}
		})
		rec := postTokenSignIn(t, h, url.Values{"token": {"tok123"}})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}
