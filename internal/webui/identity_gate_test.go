package webui

// identity_gate_test.go — the identity-owned page gate and the header nav
// matrix (the Forgejo #371 defect: an auth-enabled instance showed keys,
// invitations and setup in the primary nav and rendered an "Add a key" form to
// a signed-out visitor).
//
// The rules pinned here:
//
//   - keys/invitations are per-principal surfaces and appear ONLY in the
//     identity menu (never the primary nav, in any mode);
//   - setup stays in the primary nav only for the first-run/unsecured case
//     (auth mode "" / "none" with no signed-in principal); a signed-in admin
//     reaches /setup from the identity menu;
//   - /keys, /invitations and /notifications require a signed-in principal:
//     a signed-out caller under oidc/token gets the API's 401, and auth none
//     (no identity exists) renders the shared notice pointing at /setup;
//   - their writes refuse anonymous (401) and auth-none (403) callers before
//     any service call.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server/auth"
)

// identityServer builds the full provider (real templates) with one auth mode
// and principal, plus the public/settings seams these pages read.
func identityServer(t *testing.T, mode string, p auth.Principal) *Server {
	t.Helper()
	env := publicEnv(publicCfg(mode, true), &publicFakeRegistry{
		owners: []string{"alice"}, repos: map[string][]string{"alice": {"hello"}},
	})
	env.API.SSHKeys = &settingsFakeKeys{rows: []api.SshKeyRecord{{ID: "fp-1", Key: "ssh-ed25519 AAAA seeded"}}}
	s, err := New(env)
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	s.BindAuth(pageAuthFor(p))
	BindPublic(PublicDeps{
		MyInvites:   func(context.Context, auth.Principal) ([]identity.InboxEntry, error) { return nil, nil },
		Tray:        func(context.Context, string, string, string, int) ([]notify.Notification, bool) { return nil, false },
		UnreadCount: func(context.Context, string) int { return 0 },
		AcceptInvite: func(context.Context, auth.Principal, string) (string, error) {
			return "", nil
		},
		CancelInvite: func(context.Context, string, string) (*identity.Invitation, error) {
			return &identity.Invitation{ID: "inv-1"}, nil
		},
		ReadAll: func(context.Context, string) int { return 1 },
		SetNotificationState: func(_ context.Context, _, id string, read bool) (*notify.Notification, error) {
			return &notify.Notification{ID: id}, nil
		},
	})
	t.Cleanup(func() {
		BindPublic(PublicDeps{})
		BindSettings(SettingsDeps{})
	})
	BindSettings(SettingsDeps{})
	return s
}

// identityDo drives a form POST as a same-origin browser would.
func identityDo(s *Server, method, target string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	return rec
}

// siteNav returns the primary <nav aria-label="Site"> block.
func siteNav(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav aria-label="Site"`)
	if start < 0 {
		t.Fatalf("no site nav in body")
	}
	end := strings.Index(body[start:], "</nav>")
	if end < 0 {
		t.Fatalf("unterminated site nav")
	}
	return body[start : start+end]
}

// TestIdentityNavMatrix pins the per-mode header: keys/invitations never in
// the primary nav, setup only while unsecured, and the identity menu only for
// a signed-in principal.
func TestIdentityNavMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		p         auth.Principal
		wantSetup bool
		wantMenu  bool
	}{
		{"none mode", "none", auth.None(), true, false},
		{"empty mode", "", auth.Principal{Name: "anon", Write: true, Admin: true}, true, false},
		{"oidc signed out", "oidc", auth.Anonymous(), false, false},
		{"token signed out", "token", auth.Anonymous(), false, false},
		{"token signed in", "token", auth.Principal{Name: "alice", Write: true}, false, true},
		{"oidc signed in", "oidc", auth.Principal{Name: "alice", Write: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := identityServer(t, tc.mode, tc.p)
			rec := publicGet(s, "/")
			publicWantStatus(t, rec, http.StatusOK)
			body := rec.Body.String()
			nav := siteNav(t, body)
			if strings.Contains(nav, "/keys") || strings.Contains(nav, "/invitations") {
				t.Fatalf("primary nav leaks keys/invitations: %s", nav)
			}
			if got := strings.Contains(nav, `href="/setup"`); got != tc.wantSetup {
				t.Fatalf("nav setup = %v, want %v: %s", got, tc.wantSetup, nav)
			}
			if got := strings.Contains(body, "/_auth/logout?next=/"); got != tc.wantMenu {
				t.Fatalf("identity menu = %v, want %v", got, tc.wantMenu)
			}
		})
	}
}

// TestIdentityMenuOwnsKeysInvitesAndSetup pins the signed-in destinations:
// the menu carries keys + invitations for everyone and Setup for host admins.
func TestIdentityMenuOwnsKeysInvitesAndSetup(t *testing.T) {
	admin := identityServer(t, "oidc", auth.Principal{Name: "alice", Write: true, Admin: true})
	body := publicGet(admin, "/").Body.String()
	nav := siteNav(t, body)
	if strings.Contains(nav, "/keys") || strings.Contains(nav, "/invitations") || strings.Contains(nav, "/setup") {
		t.Fatalf("admin nav still leaks identity links: %s", nav)
	}
	publicWantContains(t, publicGet(admin, "/"),
		`href="/keys">Keys</a>`, `href="/invitations">Invitations</a>`, `href="/setup">Setup</a>`)

	plain := identityServer(t, "oidc", auth.Principal{Name: "bob", Write: true})
	body = publicGet(plain, "/").Body.String()
	publicWantContains(t, publicGet(plain, "/"), `href="/keys">Keys</a>`, `href="/invitations">Invitations</a>`)
	if strings.Contains(body, `>Setup</a>`) {
		t.Fatalf("non-admin menu carries Setup")
	}
}

// identityPageCase is one identity-owned page and the marker its real body
// carries (never present on a denial or the auth-none notice).
type identityPageCase struct {
	name, path, marker string
}

var identityPages = []identityPageCase{
	{"keys", "/keys", "Add a key"},
	{"invitations", "/invitations", "No pending invitations"},
	{"notifications", "/notifications", "No notifications"},
}

// TestIdentityPagesRequireSignedIn is the table-driven gate: signed out under
// oidc/token denies (401 + no form), auth none explains the /setup path (200,
// no form), signed in renders the page.
func TestIdentityPagesRequireSignedIn(t *testing.T) {
	for _, page := range identityPages {
		t.Run(page.name, func(t *testing.T) {
			for _, mode := range []string{"oidc", "token"} {
				s := identityServer(t, mode, auth.Anonymous())
				rec := publicGet(s, page.path)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s signed out (%s) = %d, want 401", page.path, mode, rec.Code)
				}
				publicWantContains(t, rec, "Sign in required")
				publicWantNotContains(t, rec, page.marker, `<form method="post"`)
			}

			// No identity exists: an explanatory page with the /setup path,
			// never a management form and never a 404.
			for _, mode := range []string{"none", ""} {
				s := identityServer(t, mode, auth.Anonymous())
				rec := publicGet(s, page.path)
				publicWantStatus(t, rec, http.StatusOK)
				publicWantContains(t, rec, "Authentication is disabled", `href="/setup"`, "Configure authentication")
				publicWantNotContains(t, rec, page.marker, `<form method="post"`)
			}

			// Signed in: the real page.
			s := identityServer(t, "token", auth.Principal{Name: "alice", Write: true})
			rec := publicGet(s, page.path)
			publicWantStatus(t, rec, http.StatusOK)
			publicWantContains(t, rec, page.marker)
			publicWantNotContains(t, rec, "Authentication is disabled")
		})
	}
}

// TestIdentityDenialSignInLinkOnlyWithBrowserLogin: the 401 page names a
// sign-in path whenever a browser login exists — OIDC, or token mode with a
// session_secret (F1's token-paste form). Token mode without a secret has no
// login flow, so the page must not offer a dead button.
func TestIdentityDenialSignInLinkOnlyWithBrowserLogin(t *testing.T) {
	oidc := identityServer(t, "oidc", auth.Anonymous())
	rec := publicGet(oidc, "/keys")
	publicWantStatus(t, rec, http.StatusUnauthorized)
	publicWantContains(t, rec, `href="/_auth/login?next=%2Fkeys"`)

	token := identityServer(t, "token", auth.Anonymous())
	rec = publicGet(token, "/keys")
	publicWantStatus(t, rec, http.StatusUnauthorized)
	publicWantContains(t, rec, "sign in to manage your SSH keys")
	publicWantContains(t, rec, `href="/_auth/login?next=%2Fkeys"`)

	noSecret := identityServer(t, "token", auth.Anonymous())
	noSecret.env.Cfg.Server.Auth.SessionSecret = ""
	rec = publicGet(noSecret, "/keys")
	publicWantStatus(t, rec, http.StatusUnauthorized)
	publicWantNotContains(t, rec, "/_auth/login")
}

// TestIdentityWritesRefuseNoIdentity pins the write side: anonymous is 401
// even with anonymous_read on (the defect), auth none is 403 (no identity to
// own the write), and a signed-in principal proceeds.
func TestIdentityWritesRefuseNoIdentity(t *testing.T) {
	writes := []struct{ name, method, path string }{
		{"key add", http.MethodPost, "/keys"},
		{"key delete", http.MethodPost, "/keys/fp-1/delete"},
		{"invitation accept", http.MethodPost, "/invitations/inv-1/accept"},
		{"invitation decline", http.MethodPost, "/invitations/inv-1/decline"},
		{"notification read-all", http.MethodPost, "/notifications/read-all"},
		{"notification read", http.MethodPost, "/notifications/n1/read"},
		{"notification unread", http.MethodPost, "/notifications/n1/unread"},
	}
	form := url.Values{"key": {"ssh-ed25519 AAAA test"}}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			anon := identityServer(t, "oidc", auth.Anonymous())
			if rec := identityDo(anon, w.method, w.path, form); rec.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous %s %s = %d, want 401 (%s)", w.method, w.path, rec.Code, rec.Body.String())
			}

			none := identityServer(t, "none", auth.None())
			if rec := identityDo(none, w.method, w.path, form); rec.Code != http.StatusForbidden {
				t.Fatalf("auth-none %s %s = %d, want 403 (%s)", w.method, w.path, rec.Code, rec.Body.String())
			}

			signedIn := identityServer(t, "token", auth.Principal{Name: "alice", Write: true})
			rec := identityDo(signedIn, w.method, w.path, form)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("signed-in %s %s = %d, want 303 (%s)", w.method, w.path, rec.Code, rec.Body.String())
			}
		})
	}
}
