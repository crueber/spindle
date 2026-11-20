// public_test.go — httptest coverage for the public page group: shape, gates,
// empty states, and the two inbox write paths. The HTML is parsed as text
// (html/template escapes every value, so substring assertions on landmarks,
// classes, and hrefs are both stable and honest); nothing here re-tests the
// renderers (markdown_test.go / diff_test.go own those).
//
// Every gate is asserted on both twins: the HTML error page (browsers) and the
// API's plain-text convention (Accept: text/plain, 07 §2).
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/identity"
	"github.com/crueber/walhub/internal/notify"
	"github.com/crueber/walhub/internal/pages"
	"github.com/crueber/walhub/internal/server/auth"
	"github.com/crueber/walhub/internal/sizecatalog"
	"github.com/crueber/walhub/internal/store"
)

// publicFixedNow is the clock the invite-expiry assertions run against.
var publicFixedNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// publicFakeRegistry is the api.RepoRegistry double: fixed owner→repos membership.
type publicFakeRegistry struct {
	owners []string
	repos  map[string][]string
}

func (f *publicFakeRegistry) Owners(context.Context) ([]string, error) { return f.owners, nil }
func (f *publicFakeRegistry) Repos(_ context.Context, owner string) ([]string, error) {
	return f.repos[owner], nil
}
func (f *publicFakeRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) {
	out := map[string]int{}
	for o, rs := range f.repos {
		if len(rs) > 0 {
			out[o] = len(rs)
		}
	}
	return out, nil
}
func (f *publicFakeRegistry) Exists(_ context.Context, id git.RepoId) (bool, error) {
	for _, r := range f.repos[id.Owner] {
		if r == id.Name {
			return true, nil
		}
	}
	return false, nil
}
func (f *publicFakeRegistry) Create(context.Context, git.RepoId, git.ObjectFormat) error { return nil }
func (f *publicFakeRegistry) Delete(context.Context, git.RepoId) error                   { return nil }

// publicFakeUserAvatar satisfies api.UserAvatar.
type publicFakeUserAvatar struct{ url string }

func (f publicFakeUserAvatar) UserAvatarURL(context.Context, string) string { return f.url }

// publicFakeOwnerEdit satisfies api.OwnerEditor (org-owner profile grant).
type publicFakeOwnerEdit struct{ allow bool }

func (f publicFakeOwnerEdit) CanEditOwnerProfile(context.Context, string, auth.Principal) (bool, error) {
	return f.allow, nil
}

func publicCfg(mode string, anonymousRead bool) *config.Config {
	cfg := &config.Config{}
	cfg.Server.Auth.Mode = mode
	cfg.Server.Auth.AnonymousRead = anonymousRead
	cfg.Server.Auth.SessionSecret = "s"
	cfg.Server.Auth.OAuthClientID = "id"
	cfg.Server.Auth.OAuthClientSecret = "secret"
	return cfg
}

func publicEnv(cfg *config.Config, reg api.RepoRegistry) Env {
	env := Env{
		Cfg:     cfg,
		Version: "test",
		API:     &api.Env{Repos: reg, Store: store.NewMemory()},
		Now:     func() time.Time { return publicFixedNow },
	}
	env.API.Now = env.Now
	return env
}

// newPublicServer builds the provider with a fixed principal and bindings.
func newPublicServer(t *testing.T, env Env, p auth.Principal, deps PublicDeps) *Server {
	t.Helper()
	s, err := New(env)
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	s.BindAuth(pages.Auth{
		Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) { return p, nil },
		Refresh:      func(http.ResponseWriter, *http.Request, auth.Principal) {},
	})
	BindPublic(deps)
	t.Cleanup(func() { BindPublic(PublicDeps{}) })
	return s
}

func publicPutProfile(t *testing.T, env Env, owner, display, location, tz, bio string) {
	t.Helper()
	body, err := json.Marshal(ownerProfileDoc{DisplayName: display, Location: location, Timezone: tz, BioMarkdown: bio})
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if _, err := store.PutBytes(context.Background(), env.API.Store, store.OwnerProfileKey(owner), body, store.PutOptions{
		Mode: store.PutCreate, ContentType: "application/json",
	}); err != nil {
		t.Fatalf("put profile: %v", err)
	}
}

func publicPutMirror(t *testing.T, env Env, owner, repo, upstream string) {
	t.Helper()
	body := []byte(`{"upstream_url":"` + upstream + `"}`)
	if _, err := store.PutBytes(context.Background(), env.API.Store, store.MirrorKey(owner, repo), body, store.PutOptions{
		Mode: store.PutCreate, ContentType: "application/json",
	}); err != nil {
		t.Fatalf("put mirror: %v", err)
	}
}

// do serves one request through the page mux (handlers run exactly as the
// server calls them).
func publicDo(s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func publicGet(s *Server, target string) *httptest.ResponseRecorder {
	return publicDo(s, http.MethodGet, target, nil)
}

func publicWantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %.240q)", rec.Code, want, rec.Body.String())
	}
}

func publicWantContains(t *testing.T, rec *httptest.ResponseRecorder, wants ...string) {
	t.Helper()
	body := rec.Body.String()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("body missing %q", w)
		}
	}
}

func publicWantNotContains(t *testing.T, rec *httptest.ResponseRecorder, unwanted ...string) {
	t.Helper()
	body := rec.Body.String()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Fatalf("body unexpectedly contains %q", u)
		}
	}
}

func alicePublicEnv(t *testing.T) (Env, *publicFakeRegistry) {
	t.Helper()
	reg := &publicFakeRegistry{owners: []string{"alice", "acme"}, repos: map[string][]string{
		"alice": {"hello", "notes"},
		"acme":  {"widget"},
	}}
	env := publicEnv(publicCfg("none", true), reg)
	env.API.RepoVisibility = func(_ context.Context, owner, repo string) (string, bool) {
		if owner == "alice" && repo == "notes" {
			return "private", true
		}
		return "public", true
	}
	env.API.Avatars = publicFakeUserAvatar{url: "/api/v1/users/alice/avatar?v=1"}
	return env, reg
}

// --- static pages ------------------------------------------------------------

func TestPublicLanding(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})

	rec := publicGet(s, "/")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"a git host whose only database is an object store",
		`class="btn primary px-4 py-2" href="/explore"`,
		`href="#quickstart"`,
		`id="quickstart"`,
		"Push in 30 seconds",
		"/_ui/concepts/push-still.gif",
		`href="/how-it-works"`,
	)
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if et := rec.Header().Get("ETag"); et == "" {
		t.Fatal("missing ETag")
	}

	// Conditional GET revalidates (the shell rule).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	nr := httptest.NewRecorder()
	s.mux.ServeHTTP(nr, req)
	if nr.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d, want 304", nr.Code)
	}
}

func TestPublicHowItWorks(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicGet(s, "/how-it-works")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"How walhub works",
		`href="#idea"`, `href="#wal"`, `href="#push"`,
		`href="#read"`, `href="#checkpoints"`, `href="#boundaries"`,
		`id="checkpoints"`,
		"Text version of this diagram",
		"the bucket is the repository",
		`href="https://cursor.com/blog/git-at-any-scale" target="_blank" rel="noopener"`,
	)
}

func TestPublicAPIDocs(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicGet(s, "/api")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"Discovery (GET /api/v1)",
		`<code class="font-mono text-xs">/api/v1</code>`,
		"API version",
		"Checks — reporting from external CI",
		"Pulls and review",
		"Streams: notifications, collab, tasks",
		"Identity: profiles, orgs, access",
		`class="code-view overflow-x-auto p-3"`,
		`/api/v1/notifications/stream`,
	)
	// Every static row is rendered (115 rows in the JSX table).
	if n := strings.Count(rec.Body.String(), "<tr>"); n < 115 {
		t.Fatalf("route table rows = %d, want >= 115", n)
	}
	// The live discovery list is derived from the route table, deduped.
	if !strings.Contains(rec.Body.String(), `<li class="chip font-mono">/{owner}/{repo}/api/commits</li>`) {
		t.Fatal("discovery endpoints list missing the issues template")
	}
}

func TestPublicLoginRequired(t *testing.T) {
	// OIDC + browser trio → the login button points at /_auth/login.
	env, _ := alicePublicEnv(t)
	env.Cfg = publicCfg("oidc", true)
	s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})

	rec := publicGet(s, "/login-required?next=%2Falice%2Fhello%3Ftab%3Dissues&action=Star%20this%20repository")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"Log in to continue",
		"<strong>Star this repository</strong>",
		`href="/_auth/login?next=%2Falice%2Fhello%3Ftab%3Dissues"`,
		"Continuing to: /alice/hello?tab=issues",
	)

	// An absolute/scheme target is refused (no open redirect).
	rec = publicGet(s, "/login-required?next=%2F%2Fevil.example%2F")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Continuing to: /")
	publicWantNotContains(t, rec, "evil.example")

	// No OIDC flow → the page says so, no dead button.
	env2 := publicEnv(publicCfg("none", true), nil)
	s2 := newPublicServer(t, env2, auth.Anonymous(), PublicDeps{})
	rec = publicGet(s2, "/login-required")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Log in is not enabled on this server.")
	publicWantNotContains(t, rec, ">Log in<")
}

func TestPublicGateRequiresRead(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.Cfg = publicCfg("oidc", false) // anonymous_read off
	s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})

	for _, path := range []string{
		"/", "/how-it-works", "/api", "/login-required",
		"/invitations", "/notifications",
		"/alice", "/alice/repositories", "/alice/organizations",
	} {
		rec := publicDo(s, http.MethodGet, path, map[string]string{"Accept": "text/plain"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (body %.160q)", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "authentication required") {
			t.Fatalf("%s: missing denial message: %.160q", path, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("%s: text twin content-type = %q", path, rec.Header().Get("Content-Type"))
		}
	}

	// The browser twin is the shared HTML error page (401 + a sign-in path).
	rec := publicGet(s, "/notifications")
	publicWantStatus(t, rec, http.StatusUnauthorized)
	publicWantContains(t, rec, "Sign in required", "authentication required", `href="/"`)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("html twin content-type = %q", ct)
	}
}

func TestPublicReservedOwnerNamesFallThrough(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})

	// Every SPA-only single-segment route is reserved: the owner handler must
	// write nothing so the server can fall back to the SPA.
	for _, name := range []string{
		"explore", "how-it-works", "import", "new", "setup", "keys",
		"invitations", "notifications", "login-required",
	} {
		if !reservedOwnerNames[name] {
			t.Fatalf("%q is not reserved: the /{owner} wildcard would claim the SPA route", name)
		}
	}
	for name := range reservedOwnerNames {
		if name == "explore" {
			continue // the foundation owns GET /explore with a real page
		}
		req := httptest.NewRequest(http.MethodGet, "/"+name, nil)
		req.SetPathValue("owner", name)
		rec := httptest.NewRecorder()
		s.publicOwnerProfile(rec, req)
		if rec.Body.Len() != 0 || rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: reserved owner handler wrote a response (code %d, body %.80q)", name, rec.Code, rec.Body.String())
		}
	}
}

// --- owner pages -------------------------------------------------------------

func TestOwnerProfilePage(t *testing.T) {
	env, _ := alicePublicEnv(t)
	publicPutProfile(t, env, "alice", "Alice A", "Berlin", "Europe/Berlin", "**hello** bio")
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})

	rec := publicGet(s, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"Alice A",
		"@alice",
		"Berlin · Europe/Berlin",
		"<strong>hello</strong> bio",
		`aria-current="page">Profile</a>`,
		`aria-label="2 repositories">2</span>`,
		`href="/alice/repositories"`,
		`href="/alice/organizations"`,
		`/api/v1/users/alice/avatar?v=1`,
	)
	// The profile view is identity only — no listing on this tab.
	publicWantNotContains(t, rec, "import into alice")

	// Unknown owner: the empty profile, tabs still render (never a 404).
	rec = publicGet(s, "/nobody")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "<h1 class=\"text-2xl font-semibold\">nobody</h1>", "Profile")
}

func TestOwnerRepositoriesTab(t *testing.T) {
	env, _ := alicePublicEnv(t)
	publicPutMirror(t, env, "alice", "hello", "https://upstream.example/alice/hello.git")
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
		StarCount: func(_ context.Context, owner, repo string) (int, bool) {
			if owner == "alice" && repo == "hello" {
				return 3, true
			}
			return 0, false
		},
	})

	rec := publicGet(s, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		`href="/alice/hello"`,
		`href="/alice/notes"`,
		"2 repositories",
		`title="mirror of https://upstream.example/alice/hello.git · pull-only"`,
		`title="private — owner or organization members only"`,
		`>private</span>`,
		`(3 ⭐)`,
		`aria-label="2 repositories"`,
		`href="/import?owner=alice"`,
	)

	// Empty state for an owner with no repos.
	rec = publicGet(s, "/nobody/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "0 repositories", "nothing under nobody yet", `href="/import?owner=nobody"`)
}

func TestOwnerOrganizationsTab(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
		MemberOrgs: func(context.Context, string) ([]string, error) {
			return []string{"zeta", "alpha"}, nil
		},
	})
	rec := publicGet(s, "/alice/organizations")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		`aria-label="Organizations"`,
		`href="/alpha">alpha</a>`,
		`href="/zeta">zeta</a>`,
		`aria-current="page">Organizations</a>`,
	)
	// Sorted (alpha before zeta).
	if strings.Index(rec.Body.String(), `href="/alpha"`) > strings.Index(rec.Body.String(), `href="/zeta"`) {
		t.Fatal("member orgs are not sorted")
	}

	// Empty membership list.
	s2 := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
		MemberOrgs: func(context.Context, string) ([]string, error) { return nil, nil },
	})
	rec = publicGet(s2, "/alice/organizations")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "No organizations")
}

func TestOwnerOrgProfile(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.API.OwnerEdit = publicFakeOwnerEdit{allow: true}
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Admin: true}, PublicDeps{
		Org: func(_ context.Context, org string) (*identity.Org, error) {
			if org != "acme" {
				return nil, nil
			}
			return &identity.Org{
				Org: "acme", DisplayName: "Acme Corp", Description: "widgets",
				Location: "Paris", BioMarkdown: "we build *widgets*",
				AvatarContentType: "image/png", AvatarUpdatedAt: "2026-01-02T03:04:05Z",
			}, nil
		},
	})
	rec := publicGet(s, "/acme")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"Acme Corp",
		`aria-label="organization">org</span>`,
		"widgets",
		"Paris",
		"we build <em>widgets</em>",
		`/api/v1/orgs/acme/avatar?v=2026-01-02T03%3A04%3A05Z`,
		`href="/acme/settings">Manage organization</a>`,
		"Profile",
		"Repositories",
	)
	// Org profiles hide the Organizations tab (#370: member principals are
	// email spellings, not routable slugs).
	publicWantNotContains(t, rec, `href="/acme/organizations"`)
}

// --- invitations -------------------------------------------------------------

func publicInviteDeps(rows []identity.InboxEntry) PublicDeps {
	return PublicDeps{
		MyInvites: func(context.Context, auth.Principal) ([]identity.InboxEntry, error) {
			return rows, nil
		},
		PreviewInvite: func(_ context.Context, _ auth.Principal, id, token string) (*identity.Invitation, error) {
			if id == "inv-shared" && token == "tok" {
				return &identity.Invitation{ID: id, Kind: "repo", Repo: "acme/widget", Role: "write",
					InvitedBy: "alice", State: "pending", ExpiresAt: "2030-01-01T00:00:00Z"}, nil
			}
			return nil, errors.New("not yours")
		},
		AcceptInvite: func(context.Context, auth.Principal, string) (string, error) { return "membership", nil },
		CancelInvite: func(context.Context, string, string) (*identity.Invitation, error) {
			return &identity.Invitation{ID: "x"}, nil
		},
	}
}

func TestInvitationsAnonymous(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
	rec := publicGet(s, "/invitations")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Sign in to view your invitations")
	publicWantNotContains(t, rec, "accept</button>")
	// A shared deep link does not preview for a signed-out caller (no token
	// echo, no empty "loading" card).
	rec = publicGet(s, "/invitations?id=inv-shared&token=tok")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantNotContains(t, rec, "Shared invitation", "loading…")
}

func TestInvitationsInbox(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, publicInviteDeps([]identity.InboxEntry{
		{ID: "inv-repo", Repo: "acme/widget", Role: "write", InvitedBy: "bob",
			ExpiresAt: "2020-01-01T00:00:00Z"}, // expired vs publicFixedNow
		{ID: "inv-org", Org: "acme", Role: "member", InvitedBy: "bob",
			ExpiresAt: "2030-01-01T00:00:00Z"},
	}))
	rec := publicGet(s, "/invitations?note=accepted")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"accepted",
		`<span class="chip">repo</span>`,
		`<span class="chip">org</span>`,
		`<strong class="font-mono text-sm">acme/widget</strong>`,
		`<strong class="font-mono text-sm">acme</strong>`,
		`<span class="chip-closed">expired</span>`,
		`title="2030-01-01T00:00:00Z">2030-01-01</span>`,
		`action="/invitations/inv-org/accept"`,
		`action="/invitations/inv-org/decline"`,
		`<button type="submit" class="btn primary px-2 py-1 text-xs" disabled`,
	)

	// Shared deep link previews on top.
	rec = publicGet(s, "/invitations?id=inv-shared&token=tok")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		`aria-label="Shared invitation"`,
		`<strong class="font-mono">acme/widget</strong>`,
		`action="/invitations/inv-shared/accept"`,
	)

	// A bad token renders the friendly error, not a 500.
	rec = publicGet(s, "/invitations?id=inv-shared&token=wrong")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "not your invitation")
}

func TestInvitationWrites(t *testing.T) {
	env, _ := alicePublicEnv(t)
	var accepted, declined string
	deps := publicInviteDeps(nil)
	deps.AcceptInvite = func(_ context.Context, _ auth.Principal, id string) (string, error) {
		accepted = id
		return "membership", nil
	}
	deps.CancelInvite = func(_ context.Context, _ string, id string) (*identity.Invitation, error) {
		declined = id
		return &identity.Invitation{ID: id}, nil
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	sameOrigin := map[string]string{"Origin": "http://example.com"}

	rec := publicDo(s, http.MethodPost, "/invitations/inv-1/accept", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/invitations?note=") {
		t.Fatalf("accept Location = %q", got)
	}
	if accepted != "inv-1" {
		t.Fatalf("AcceptInvite got %q", accepted)
	}

	rec = publicDo(s, http.MethodPost, "/invitations/inv-2/decline", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if declined != "inv-2" {
		t.Fatalf("CancelInvite got %q", declined)
	}

	// A service failure redirects with the friendly note (never a 500).
	deps.AcceptInvite = func(context.Context, auth.Principal, string) (string, error) {
		return "", errors.New("invitation expired")
	}
	BindPublic(deps)
	rec = publicDo(s, http.MethodPost, "/invitations/inv-3/accept", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if got := rec.Header().Get("Location"); !strings.Contains(got, "invitation+expired") {
		t.Fatalf("expired Location = %q", got)
	}

	// Cross-origin posts are refused (403, plain-text twin).
	rec = publicDo(s, http.MethodPost, "/invitations/inv-4/accept", map[string]string{
		"Origin": "https://evil.example", "Accept": "text/plain",
	})
	publicWantStatus(t, rec, http.StatusForbidden)
	// No Origin/Referer is refused too (fail closed).
	rec = publicDo(s, http.MethodPost, "/invitations/inv-4/accept", map[string]string{"Accept": "text/plain"})
	publicWantStatus(t, rec, http.StatusForbidden)

	// Anonymous writes are 401 (the interstitial is a page, not a target).
	anon := newPublicServer(t, env, auth.Anonymous(), deps)
	rec = publicDo(anon, http.MethodPost, "/invitations/inv-5/accept", map[string]string{
		"Origin": "http://example.com", "Accept": "text/plain",
	})
	publicWantStatus(t, rec, http.StatusUnauthorized)
}

// --- notifications -----------------------------------------------------------

func publicNotifDeps(t *testing.T) PublicDeps {
	t.Helper()
	return PublicDeps{
		Tray: func(_ context.Context, who, state, after string, n int) ([]notify.Notification, bool) {
			if n != notify.TrayPageSize {
				t.Errorf("Tray n = %d, want %d", n, notify.TrayPageSize)
			}
			return []notify.Notification{
				{ID: "n1", Repo: "acme/widget", Num: 3, Kind: "pull", Reason: "review_requested",
					Title: "Fix the thing", Actor: "bob", State: "unread", CreatedAt: "2026-11-19T10:00:00Z"},
				{ID: "n2", Repo: "acme/widget", Num: 7, Kind: "issue", Reason: "subscribed",
					Title: "<script>alert(1)</script>", Actor: "carol", State: "read", CreatedAt: "2026-11-18T10:00:00Z"},
			}, true
		},
		UnreadCount: func(context.Context, string) int { return 1 },
		ReadAll:     func(context.Context, string) int { return 1 },
		SetNotificationState: func(_ context.Context, who, id string, read bool) (*notify.Notification, error) {
			return &notify.Notification{ID: id, State: "read"}, nil
		},
	}
}

func TestNotificationsAnonymous(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
	rec := publicGet(s, "/notifications")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Sign in to see your notifications")
	publicWantNotContains(t, rec, "data-sse-url")
}

func TestNotificationsTray(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, publicNotifDeps(t))
	rec := publicGet(s, "/notifications?state=unread")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"1 unread",
		`data-sse="notifications"`,
		`data-sse-url="/api/v1/notifications/stream"`,
		`data-sse-mode="invalidate"`,
		`href="/acme/widget/pull/3"`,
		`href="/acme/widget/issues/7"`,
		`<span class="chip chip-open">review_requested</span>`,
		`border-l-4 border-l-emerald-500`,
		`aria-label="unread"`,
		`action="/notifications/n1/read?state=unread"`,
		`action="/notifications/n2/unread?state=unread"`,
		`action="/notifications/read-all?state=unread"`,
		`href="/notifications?after=n2&state=unread"`, // Older, cursor = last row
	)
	// Titles are escaped — the notification title is untrusted target data.
	publicWantNotContains(t, rec, "<script>alert(1)</script>")
	publicWantContains(t, rec, "&lt;script&gt;alert(1)&lt;/script&gt;")

	// The ?state= and ?after= filters are passed through to the tray seam
	// verbatim (the page never re-implements the paging rule).
	var gotState, gotAfter string
	env2, _ := alicePublicEnv(t)
	s2 := newPublicServer(t, env2, auth.Principal{Name: "alice"}, PublicDeps{
		Tray: func(_ context.Context, who, state, after string, n int) ([]notify.Notification, bool) {
			gotState, gotAfter = state, after
			return nil, false
		},
		UnreadCount: func(context.Context, string) int { return 0 },
	})
	rec = publicGet(s2, "/notifications?state=unread&after=n9")
	publicWantStatus(t, rec, http.StatusOK)
	if gotState != "unread" || gotAfter != "n9" {
		t.Fatalf("Tray got state=%q after=%q, want unread/n9", gotState, gotAfter)
	}
}

func TestNotificationsTrayEmpty(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
		Tray:        func(context.Context, string, string, string, int) ([]notify.Notification, bool) { return nil, false },
		UnreadCount: func(context.Context, string) int { return 0 },
	})
	rec := publicGet(s, "/notifications")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "nothing here — watching a repo or being mentioned lands notifications in this tray.")
}

func TestNotificationWrites(t *testing.T) {
	env, _ := alicePublicEnv(t)
	var flippedTo bool
	var flippedID string
	deps := publicNotifDeps(t)
	deps.SetNotificationState = func(_ context.Context, _ string, id string, read bool) (*notify.Notification, error) {
		flippedID, flippedTo = id, read
		return &notify.Notification{ID: id}, nil
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	sameOrigin := map[string]string{"Origin": "http://example.com"}

	rec := publicDo(s, http.MethodPost, "/notifications/read-all", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if rec.Header().Get("Location") != "/notifications" {
		t.Fatalf("read-all Location = %q", rec.Header().Get("Location"))
	}
	// The state filter survives the write (the caller stays on the unread tab).
	rec = publicDo(s, http.MethodPost, "/notifications/read-all?state=unread", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/notifications?state=unread" {
		t.Fatalf("filtered read-all Location = %q", loc)
	}

	rec = publicDo(s, http.MethodPost, "/notifications/n7/read?state=unread", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if !flippedTo || flippedID != "n7" {
		t.Fatalf("SetState got (%q, %v)", flippedID, flippedTo)
	}
	if loc := rec.Header().Get("Location"); loc != "/notifications?state=unread" {
		t.Fatalf("mark-read Location = %q", loc)
	}

	rec = publicDo(s, http.MethodPost, "/notifications/n8/unread", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if flippedTo {
		t.Fatal("unread path flipped read=true")
	}

	// Cross-origin writes are refused (HTML page for browsers, text twin when
	// asked).
	rec = publicDo(s, http.MethodPost, "/notifications/read-all", map[string]string{
		"Origin": "https://evil.example", "Accept": "text/plain",
	})
	publicWantStatus(t, rec, http.StatusForbidden)
	rec = publicDo(s, http.MethodPost, "/notifications/read-all", map[string]string{
		"Origin": "https://evil.example",
	})
	publicWantStatus(t, rec, http.StatusForbidden)
	publicWantContains(t, rec, "Forbidden", "cross-origin request refused")
}

// publicFakeAccess is the api.ReadAccess double: a fixed per-repo verdict.
type publicFakeAccess struct{ denyRepo string }

func (f publicFakeAccess) CheckRead(_ context.Context, owner, repo string, _ auth.Principal) *auth.AuthError {
	if repo == f.denyRepo {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "forbidden"}
	}
	return nil
}

func TestOwnerListingVisibilityFilter(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.API.Access = publicFakeAccess{denyRepo: "notes"}
	s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
	rec := publicGet(s, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, `href="/alice/hello"`, "1 repository")
	// The unreadable repo never reaches the page (no name, no count).
	publicWantNotContains(t, rec, "notes", "2 repositories")

	// Owners with no readable repo are omitted from /explore's source too —
	// the page uses the same VisibleOwners seam (foundation's reference page
	// owns that assertion); here the per-owner walk is what the owner page
	// relies on.
	env2, _ := alicePublicEnv(t)
	env2.API.Access = publicFakeAccess{denyRepo: "notes"}
	s2 := newPublicServer(t, env2, auth.Anonymous(), PublicDeps{})
	rec = publicGet(s2, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, `aria-label="1 repositories">1</span>`)
}

func TestOwnerInvalidSlugIs404(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	req := httptest.NewRequest(http.MethodGet, "/.bad", nil)
	req.SetPathValue("owner", ".bad")
	req.Header.Set("Accept", "text/plain")
	rec := httptest.NewRecorder()
	s.publicOwnerProfile(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invalid owner slug status = %d, want 404", rec.Code)
	}
}

// --- helper units ------------------------------------------------------------

func TestPublicVisibilityBadge(t *testing.T) {
	cases := []struct{ in, label, title string }{
		{"public", "public", "public — anyone may read"},
		{"authenticated", "authenticated", "private — signed-in users may read"},
		{"private", "private", "private — owner or organization members only"},
		{"PRIVATE", "private", "private — owner or organization members only"},
		{"", "", ""},
		{"secret", "", ""},
	}
	for _, c := range cases {
		label, title := visibilityBadge(c.in)
		if label != c.label || title != c.title {
			t.Fatalf("visibilityBadge(%q) = (%q, %q), want (%q, %q)", c.in, label, title, c.label, c.title)
		}
	}
}

func TestPublicFriendlyInviteErr(t *testing.T) {
	cases := map[string]string{
		"invitation expired":           "invitation expired",
		"invitation no longer pending": "invitation no longer pending",
		"that invitation is not yours": "not your invitation",
		"forbidden":                    "not your invitation",
		"unauthorized":                 "sign in to view your invitations",
		"bucket unavailable":           "bucket unavailable",
	}
	for in, want := range cases {
		if got := friendlyInviteErr(errors.New(in)); got != want {
			t.Fatalf("friendlyInviteErr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPublicInviteExpiry(t *testing.T) {
	if inviteExpired("", publicFixedNow) {
		t.Fatal("empty stamp must not read expired")
	}
	if inviteExpired("not-a-date", publicFixedNow) {
		t.Fatal("unparseable stamp must fail open on display")
	}
	if !inviteExpired("2020-01-01T00:00:00Z", publicFixedNow) {
		t.Fatal("past stamp must read expired")
	}
	if inviteExpired("2030-01-01T00:00:00Z", publicFixedNow) {
		t.Fatal("future stamp must not read expired")
	}
	if got := inviteExpiryLabel("", publicFixedNow); got != "" {
		t.Fatalf("empty label = %q", got)
	}
	if got := inviteExpiryLabel("nonsense", publicFixedNow); got != "nonsense" {
		t.Fatalf("raw label = %q", got)
	}
	if got := inviteExpiryLabel("2020-01-01T00:00:00Z", publicFixedNow); got != "" {
		t.Fatalf("expired label = %q", got)
	}
	if got := inviteExpiryLabel("2030-01-01T00:00:00Z", publicFixedNow); got != "2030-01-01" {
		t.Fatalf("live label = %q", got)
	}
}

func TestPublicJoinWhere(t *testing.T) {
	cases := [][3]string{
		{"Berlin", "Europe/Berlin", "Berlin · Europe/Berlin"},
		{"Berlin", "", "Berlin"},
		{"", "UTC", "UTC"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := joinWhere(c[0], c[1]); got != c[2] {
			t.Fatalf("joinWhere(%q,%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestPublicDiscoveryExtras(t *testing.T) {
	BindPublic(PublicDeps{ExtraEndpoints: []string{
		"/api/v1/repos/imports", // feature-owned, not in the core table
		"/api/v1/me",            // already exposed by the core table (deduped)
		"",                      // ignored
	}})
	t.Cleanup(func() { BindPublic(PublicDeps{}) })
	s := &Server{}
	eps := s.discoveryEndpoints()
	seenMe, seenImports := 0, 0
	for _, e := range eps {
		switch e {
		case "/api/v1/me":
			seenMe++
		case "/api/v1/repos/imports":
			seenImports++
		}
	}
	if seenMe != 1 || seenImports != 1 {
		t.Fatalf("/api/v1/me=%d imports=%d (extras must append once, deduped)", seenMe, seenImports)
	}
	if len(eps) < 10 {
		t.Fatalf("core endpoints missing: %v", eps)
	}
}

func TestPublicCanEditOwner(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := &Server{env: env}
	alice := auth.Principal{Name: "alice"}
	cases := []struct {
		name   string
		owner  string
		p      auth.Principal
		editor api.OwnerEditor
		want   bool
	}{
		{"admin", "acme", auth.Principal{Name: "root", Admin: true}, nil, true},
		{"self", "alice", alice, nil, true},
		{"self case-insensitive", "ALICE", alice, nil, true},
		{"anonymous", "alice", auth.Anonymous(), nil, false},
		{"other", "acme", alice, nil, false},
		{"org-owner grant", "acme", alice, publicFakeOwnerEdit{allow: true}, true},
		{"org-owner denial", "acme", alice, publicFakeOwnerEdit{allow: false}, false},
	}
	for _, c := range cases {
		env.API.OwnerEdit = c.editor
		if got := s.canEditOwner(context.Background(), c.owner, c.p); got != c.want {
			t.Fatalf("%s: canEditOwner = %v, want %v", c.name, got, c.want)
		}
	}
}

// errOwnerEdit is the probe-failure double (must fail closed, never 500).
type errOwnerEdit struct{}

func (errOwnerEdit) CanEditOwnerProfile(context.Context, string, auth.Principal) (bool, error) {
	return false, errors.New("identity unavailable")
}

func TestPublicCanEditOwnerProbeError(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.API.OwnerEdit = errOwnerEdit{}
	s := &Server{env: env}
	if s.canEditOwner(context.Background(), "acme", auth.Principal{Name: "alice"}) {
		t.Fatal("editor probe error must fail closed")
	}
}

func TestPublicReadOwnerProfileCorrupt(t *testing.T) {
	env, _ := alicePublicEnv(t)
	if _, err := store.PutBytes(context.Background(), env.API.Store, store.OwnerProfileKey("broken"),
		[]byte("{not json"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s := &Server{env: env}
	if doc := s.readOwnerProfile(context.Background(), "broken"); doc != (ownerProfileDoc{}) {
		t.Fatalf("corrupt profile = %+v, want zero", doc)
	}
	if doc := s.readOwnerProfile(context.Background(), "absent"); doc != (ownerProfileDoc{}) {
		t.Fatalf("absent profile = %+v, want zero", doc)
	}
	// No store wired → the empty profile (never a panic).
	env.API.Store = nil
	if doc := s.readOwnerProfile(context.Background(), "alice"); doc != (ownerProfileDoc{}) {
		t.Fatalf("nil store profile = %+v", doc)
	}
}

func TestPublicAuthModeDefault(t *testing.T) {
	s := &Server{env: Env{}}
	if got := s.authModeOrDefault(); got != "none" {
		t.Fatalf("nil cfg mode = %q", got)
	}
	if s.browserLoginEnabled() {
		t.Fatal("nil cfg must not enable browser login")
	}
	if s.loginURL() != "" {
		t.Fatal("nil cfg must not advertise a login URL")
	}
	// oidc without the session/client trio is a legacy config: no browser flow.
	cfg := &config.Config{}
	cfg.Server.Auth.Mode = "oidc"
	s = &Server{env: Env{Cfg: cfg}}
	if s.browserLoginEnabled() {
		t.Fatal("oidc without the trio must not enable browser login")
	}
	if got := s.authModeOrDefault(); got != "oidc" {
		t.Fatalf("mode = %q", got)
	}
}

func TestPublicServiceUnavailable(t *testing.T) {
	env, _ := alicePublicEnv(t)
	// Store/invite/tray seams unwired → 503 twins (never a panic or an empty 200).
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	for _, path := range []string{"/invitations", "/notifications"} {
		rec := publicDo(s, http.MethodGet, path, map[string]string{"Accept": "text/plain"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s unwired = %d, want 503", path, rec.Code)
		}
	}

	// Registry unwired → the owner page 503s.
	env2, _ := alicePublicEnv(t)
	env2.API.Repos = nil
	s2 := newPublicServer(t, env2, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicDo(s2, http.MethodGet, "/alice", map[string]string{"Accept": "text/plain"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry = %d, want 503", rec.Code)
	}

	// The invite list error path is a 503 too.
	s3 := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
		MyInvites: func(context.Context, auth.Principal) ([]identity.InboxEntry, error) {
			return nil, errors.New("bucket down")
		},
	})
	rec = publicDo(s3, http.MethodGet, "/invitations", map[string]string{"Accept": "text/plain"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("invite list error = %d, want 503", rec.Code)
	}
}

func TestPublicNotifRowFallbacks(t *testing.T) {
	// No thread number → the repo page; pull kind wins; the reason decides Hot.
	cases := []struct {
		in   notify.Notification
		href string
		hot  bool
	}{
		{notify.Notification{Repo: "acme/w", Kind: "release"}, "/acme/w", false},
		{notify.Notification{Repo: "acme/w", Num: 4, Kind: "pull", Reason: "assigned"}, "/acme/w/pull/4", true},
		{notify.Notification{Repo: "acme/w", Num: 4, Kind: "issue", Reason: "team_mention"}, "/acme/w/issues/4", true},
		{notify.Notification{Repo: "acme/w", Num: 4, Kind: "issue", Reason: "subscribed"}, "/acme/w/issues/4", false},
	}
	for _, c := range cases {
		row := notifRow(c.in)
		if row.Href != c.href || row.Hot != c.hot {
			t.Fatalf("notifRow(%+v) = href %q hot %v, want %q %v", c.in, row.Href, row.Hot, c.href, c.hot)
		}
	}
}

func TestOwnerListingDegradesWithoutCatalog(t *testing.T) {
	// A corrupt size catalog must not lose the listing (the membership is the
	// authoritative set; measurements are optional).
	env, _ := alicePublicEnv(t)
	if _, err := store.PutBytes(context.Background(), env.API.Store, sizecatalog.CatalogKey,
		[]byte("not a proto"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicGet(s, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, `href="/alice/hello"`, `href="/alice/notes"`, "2 repositories")

	// No store at all: the names still list (rows carry no measurements).
	env2, _ := alicePublicEnv(t)
	env2.API.Store = nil
	s2 := newPublicServer(t, env2, auth.Principal{Name: "alice"}, PublicDeps{})
	rec = publicGet(s2, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, `href="/alice/hello"`)
}

func TestInvitationDeclineErrorRedirects(t *testing.T) {
	env, _ := alicePublicEnv(t)
	deps := publicInviteDeps(nil)
	deps.CancelInvite = func(context.Context, string, string) (*identity.Invitation, error) {
		return nil, errors.New("invitation no longer pending")
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec := publicDo(s, http.MethodPost, "/invitations/inv-9/decline", map[string]string{"Origin": "http://example.com"})
	publicWantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "invitation+no+longer+pending") {
		t.Fatalf("decline error Location = %q", loc)
	}
}

func TestNotificationStateUnwired(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	for _, target := range []string{"/notifications/n1/read", "/notifications/read-all"} {
		rec := publicDo(s, http.MethodPost, target, map[string]string{
			"Origin": "http://example.com", "Accept": "text/plain",
		})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s unwired = %d, want 503", target, rec.Code)
		}
	}
}
