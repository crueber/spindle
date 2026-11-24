package server

// auth_admin_test.go — PLAN-OIDC-ONLY §1.2: administration is granted by ONE
// predicate, computed at the credential boundary:
//
//	signed in ∧ (verified groups_claim ∩ server.auth.admin_groups ≠ ∅)
//
// This test asserts every credential shape the server accepts produces exactly
// that verdict — nothing else (no static-token flag, no email/domain rule, no
// forwarded header, no hook upgrade).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

func TestAdminPredicateSingleSource(t *testing.T) {
	groupCases := []struct {
		name   string
		claim  any
		admin  bool
		detail string
	}{
		{"group present", []string{"spindle-admin"}, true, "array of strings"},
		{"group absent", []string{"other"}, false, "different group"},
		{"claim absent", nil, false, "no claim"},
		{"case-insensitive", []string{"Spindle-Admin"}, true, "EqualFold + trim"},
		{"single string", "spindle-admin", true, "the claim may be one string"},
		{"multiple groups", []any{"other", "spindle-admin"}, true, "intersection is enough"},
		{"array vs string both work", []string{"other", "spindle-admin"}, true, "mixed array"},
	}
	for _, tc := range groupCases {
		t.Run("ID token/"+tc.name, func(t *testing.T) {
			s, _, iss := setupGroupServer(t, nil)
			extra := map[string]any{}
			if tc.claim != nil {
				extra["groups"] = tc.claim
			}
			tok := idToken(t, iss, "dev@example.com", extra)
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			p, aerr := s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil {
				t.Fatalf("authenticate: %v", aerr)
			}
			if p.Admin != tc.admin {
				t.Fatalf("ID token (%s): Admin = %v, want %v (%+v)", tc.detail, p.Admin, tc.admin, p.Groups)
			}
			// The verdict is the same for the session and wgt_ wires: the
			// verified groups ride them and the policy re-applies.
			sess, err := s.authSvc.MintSession("dev@example.com", p.Groups...)
			if err != nil {
				t.Fatal(err)
			}
			req = httptest.NewRequest(http.MethodGet, "/x", nil)
			req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
			sp, aerr := s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil || sp.Admin != tc.admin {
				t.Fatalf("session Admin = %v (%v), want %v", sp.Admin, aerr, tc.admin)
			}
			wt, err := s.authSvc.MintToken("dev@example.com", p.Groups...)
			if err != nil {
				t.Fatal(err)
			}
			req = httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+wt.Wire)
			wp, aerr := s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil || wp.Admin != tc.admin {
				t.Fatalf("wgt_ Admin = %v (%v), want %v", wp.Admin, aerr, tc.admin)
			}
		})
	}

	t.Run("static token is never admin", func(t *testing.T) {
		s, _, iss := setupGroupServer(t, func(a *config.Auth) {
			a.Tokens = []config.StaticToken{{Principal: "dev@example.com", Token: "machine-token", Write: true}}
		})
		_ = iss
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer machine-token")
		p, aerr := s.authSvc.Authenticate(req, s.cfg)
		if aerr != nil {
			t.Fatalf("static token: %v", aerr)
		}
		if p.Admin || !p.Write {
			t.Fatalf("static token principal = %+v, want write + NOT admin", p)
		}
		// A static principal whose *name* matches an admin's email is still
		// not admin: names are not claims.
		s2, _, _ := setupGroupServer(t, func(a *config.Auth) {
			a.Tokens = []config.StaticToken{{Principal: "boss@example.com", Token: "boss-token", Write: true}}
		})
		req = httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer boss-token")
		if p, aerr := s2.authSvc.Authenticate(req, s2.cfg); aerr != nil || p.Admin {
			t.Fatalf("static admin-shaped name = %+v (%v), want not admin", p, aerr)
		}
	})

	t.Run("ExtraCredential shapes never gain admin", func(t *testing.T) {
		s, _ := newTestServer(t, nil)
		s.authSvc.ExtraCredential = func(token string) (auth.Principal, *auth.AuthError, bool) {
			if token == "wct_ci" || token == "wjt_job" || token == "wrt_runner" {
				return auth.Principal{Name: "ci:1"}, nil, true
			}
			return auth.Principal{}, nil, false
		}
		for _, tok := range []string{"wct_ci", "wjt_job", "wrt_runner"} {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			p, aerr := s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil {
				t.Fatalf("%s: %v", tok, aerr)
			}
			if p.Admin {
				t.Fatalf("%s principal = %+v, want not admin", tok, p)
			}
		}
	})

	t.Run("anonymous is never admin", func(t *testing.T) {
		s, _ := newTestServer(t, nil)
		p, aerr := s.authSvc.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil), s.cfg)
		if aerr != nil || p.Admin || !p.Anonymous {
			t.Fatalf("anonymous = %+v (%v)", p, aerr)
		}
	})
}
