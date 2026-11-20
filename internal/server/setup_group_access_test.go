package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

// setupGroupServer wires an oidc-mode server with the WP13 defaults:
// admin_groups ["spindle-admins"], groups_claim "groups", allowed domain
// example.com. mutate runs before the AuthService is (re)built.
func setupGroupServer(t *testing.T, mutate func(*config.Auth)) (*Server, http.Handler, *stubIssuer) {
	t.Helper()
	s, h, iss := oidcFull(t)
	s.cfg.Server.Auth.AdminGroups = []string{"spindle-admins"}
	s.cfg.Server.Auth.GroupsClaim = "groups"
	if mutate != nil {
		mutate(&s.cfg.Server.Auth)
	}
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	return s, h, iss
}

// idToken builds a signed ID token for the stub issuer with the common claims;
// extra adds/overrides group claims and the like.
func idToken(t *testing.T, iss *stubIssuer, email string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"aud":            "spindle",
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Add(-time.Minute).Unix(),
		"email":          email,
		"email_verified": true,
	}
	for k, v := range extra {
		claims[k] = v
	}
	return iss.mint(t, claims)
}

func setupGet(t *testing.T, h http.Handler, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The OIDC group claim is the setup/admin authority: a signature-verified
// groups list intersecting admin_groups grants Principal.Admin and opens
// /api/v1/setup; membership alone never grants Write and never bypasses the
// allowed-domain check.
func TestSetupAccessOIDCAdminGroups(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Auth)
		email  string
		extra  map[string]any
		want   int
		admin  bool
		write  bool
		denied bool
	}{
		{"group member", nil, "dev@example.com",
			map[string]any{"groups": []string{"spindle-admins"}}, http.StatusOK, true, true, false},
		{"other group", nil, "dev@example.com",
			map[string]any{"groups": []string{"other"}}, http.StatusForbidden, false, true, false},
		{"claim absent", nil, "dev@example.com", nil, http.StatusForbidden, false, true, false},
		{"case-insensitive", nil, "dev@example.com",
			map[string]any{"groups": []string{"Spindle-Admins"}}, http.StatusOK, true, true, false},
		{"single string claim", nil, "dev@example.com",
			map[string]any{"groups": "spindle-admins"}, http.StatusOK, true, true, false},
		{"configured claim name", func(a *config.Auth) { a.GroupsClaim = "roles" },
			"dev@example.com", map[string]any{"roles": []string{"spindle-admins"}},
			http.StatusOK, true, true, false},
		{"empty admin_groups never grants", func(a *config.Auth) { a.AdminGroups = nil },
			"dev@example.com", map[string]any{"groups": []string{"spindle-admins"}},
			http.StatusForbidden, false, true, false},
		{"groups do not grant write", func(a *config.Auth) { a.WriteDomains = []string{"writers.test"} },
			"dev@example.com", map[string]any{"groups": []string{"spindle-admins"}},
			http.StatusOK, true, false, false},
		{"email policy still applies", nil, "dev@notallowed.test",
			map[string]any{"groups": []string{"spindle-admins"}}, http.StatusForbidden, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h, iss := setupGroupServer(t, tc.mutate)
			tok := idToken(t, iss, tc.email, tc.extra)

			// The principal derivation (not just the handler status).
			req := httptest.NewRequest("GET", "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			p, aerr := s.authSvc.Authenticate(req, s.cfg)
			if tc.denied {
				if aerr == nil || aerr.Kind != auth.ErrForbidden {
					t.Fatalf("disallowed email must be Forbidden, got p=%+v aerr=%v", p, aerr)
				}
			} else {
				if aerr != nil {
					t.Fatalf("authenticate: %v", aerr)
				}
				if p.Admin != tc.admin || p.Write != tc.write {
					t.Fatalf("principal = %+v, want admin=%v write=%v", p, tc.admin, tc.write)
				}
			}

			if rec := setupGet(t, h, "Bearer "+tok); rec.Code != tc.want {
				t.Fatalf("GET /api/v1/setup = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// A group admin keeps the flag across the browser session path: the verified
// groups ride the walgit session wire, and the policy is re-applied per
// request. Admin email/domain rules keep working alongside groups, and a
// legacy 4-line session (no groups) stays valid.
func TestSetupAccessGroupsAcrossSessionAndEmailRules(t *testing.T) {
	s, h, _ := setupGroupServer(t, func(a *config.Auth) {
		a.AllowedDomains = []string{"example.com", "admins.test"}
		a.AdminEmails = []string{"boss@example.com"}
		a.AdminDomains = []string{"admins.test"}
	})

	// Group admin through the session cookie (what a real browser carries).
	sess, err := s.authSvc.MintSession("dev@example.com", "spindle-admins")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("group-admin session = %d (%s)", rec.Code, rec.Body.String())
	}
	// And through a minted wgt_ access token.
	tok, _ := s.authSvc.MintToken("dev@example.com", "spindle-admins")
	if rec := setupGet(t, h, "Bearer "+tok.Wire); rec.Code != http.StatusOK {
		t.Fatalf("group-admin wgt_ token = %d", rec.Code)
	}
	// A session with no admin group and no email rule is refused.
	plain, _ := s.authSvc.MintSession("dev@example.com")
	req = httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: plain.Wire})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin session = %d, want 403", rec.Code)
	}

	// Admin email and admin domain still grant admin (migration path).
	for _, email := range []string{"boss@example.com", "any@admins.test"} {
		sess, _ := s.authSvc.MintSession(email)
		p, aerr := s.authSvc.principalFromEmailGroups(email, nil)
		if aerr != nil || !p.Admin {
			t.Fatalf("%s admin = %+v aerr=%v", email, p, aerr)
		}
		req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
		req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s session = %d", email, rec.Code)
		}
	}

	// A 4-line session minted before groups existed still verifies.
	exp := time.Now().Add(time.Hour).Unix()
	payload := "session\n" + strconv.FormatInt(exp, 10) + "\n" +
		strconv.FormatInt(time.Now().Unix(), 10) + "\ndev@example.com"
	mac := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte(payload))
	legacy := b64url([]byte(payload)) + "." + b64url(mac)
	st, aerr := s.authSvc.VerifyToken(legacy)
	if aerr != nil || st.Email != "dev@example.com" || len(st.Groups) != 0 {
		t.Fatalf("legacy session = %+v aerr=%v", st, aerr)
	}
}

// groupsFromClaim decodes the configured claim defensively: array of strings,
// single string, or nothing. Anything unexpected grants no admin.
func TestGroupsFromClaimDecoding(t *testing.T) {
	cases := []struct {
		name  string
		raw   string // JSON object body; "" = no claim map
		claim string
		want  []string
	}{
		{"array", `{"groups":["a","b"]}`, "groups", []string{"a", "b"}},
		{"single string", `{"groups":"a"}`, "groups", []string{"a"}},
		{"absent", `{"other":["a"]}`, "groups", nil},
		{"empty claim name", `{"groups":["a"]}`, "", nil},
		{"null", `{"groups":null}`, "groups", nil},
		{"empty array", `{"groups":[]}`, "groups", nil},
		{"empty string", `{"groups":"  "}`, "groups", nil},
		{"non-string array elements", `{"groups":["a",7,null,{"x":1}]}`, "groups", []string{"a"}},
		{"number claim", `{"groups":7}`, "groups", nil},
		{"object claim", `{"groups":{"a":1}}`, "groups", nil},
		{"spaces trimmed", `{"groups":[" a "]}`, "groups", []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			if tc.raw != "" {
				if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
					t.Fatal(err)
				}
			}
			got := groupsFromClaim(raw, tc.claim)
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("groups = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
