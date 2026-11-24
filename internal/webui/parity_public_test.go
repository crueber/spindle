package webui

// parity_public_test.go — the owner page's profile edit form (Forgejo #234)
// and avatar self-service (Forgejo #376): the GET shape (who sees Edit /
// Regenerate / Remove), the POST success/validation/gates/cross-origin paths,
// and the API-identical status codes.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
)

// parityProfileDeps builds the profile/avatar seams with recording fakes.
type parityRecord struct {
	saves     []OwnerProfileInput
	regen     []string
	removed   []string
	saveErr   error
	avatarErr error
}

func parityProfileDeps(rec *parityRecord) PublicDeps {
	return PublicDeps{
		ProfileSave: func(_ context.Context, owner string, in OwnerProfileInput) error {
			if owner != "alice" {
				return errors.New("unexpected owner " + owner)
			}
			rec.saves = append(rec.saves, in)
			return rec.saveErr
		},
		RegenerateAvatar: func(_ context.Context, principal string) (*identity.Profile, error) {
			rec.regen = append(rec.regen, principal)
			if rec.avatarErr != nil {
				return nil, rec.avatarErr
			}
			return &identity.Profile{Principal: principal}, nil
		},
		RemoveAvatar: func(_ context.Context, principal string) (*identity.Profile, error) {
			rec.removed = append(rec.removed, principal)
			if rec.avatarErr != nil {
				return nil, rec.avatarErr
			}
			return &identity.Profile{Principal: principal}, nil
		},
	}
}

func TestPublicOwnerProfileEditForm(t *testing.T) {
	env, _ := alicePublicEnv(t)
	publicPutProfile(t, env, "alice", "Alice A", "Berlin", "Europe/Berlin", "**hi**")
	rec := &parityRecord{}
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(rec))

	// can_edit + a wired save seam: the sidebar offers Edit.
	body := publicGet(s, "/alice").Body.String()
	for _, want := range []string{
		"Alice A",
		`href="/alice?edit=1"`,
		"Edit profile",
		// The rendered bio is present while NOT editing.
		"<strong>hi</strong>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profile page missing %q", want)
		}
	}

	// ?edit=1 seeds the form from the stored doc and hides the rendered bio.
	body = publicGet(s, "/alice?edit=1").Body.String()
	for _, want := range []string{
		`action="/alice/profile"`,
		`id="profile-name" name="display_name" type="text" maxlength="200" value="Alice A"`,
		`name="location" type="text" maxlength="200" value="Berlin"`,
		`name="timezone" type="text" maxlength="64" value="Europe/Berlin"`,
		`name="bio_markdown" rows="6"`,
		"**hi**",
		"IANA zone name",
		`href="/alice"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
	if strings.Contains(body, "<strong>hi</strong>") {
		t.Error("the rendered bio survived while editing (#420)")
	}

	// A non-editor never sees the affordance, and ?edit=1 is inert.
	bob := newPublicServer(t, env, auth.Principal{Name: "bob", Write: true}, parityProfileDeps(rec))
	body = publicGet(bob, "/alice?edit=1").Body.String()
	for _, unwanted := range []string{`href="/alice?edit=1"`, `action="/alice/profile"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("non-editor saw %q", unwanted)
		}
	}

	// An unwired save seam hides Edit (no button that cannot write) but keeps
	// the profile itself.
	unwired := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{})
	body = publicGet(unwired, "/alice?edit=1").Body.String()
	if strings.Contains(body, `href="/alice?edit=1"`) || strings.Contains(body, `action="/alice/profile"`) {
		t.Error("unwired save seam rendered the editor")
	}
	if !strings.Contains(body, "Alice A") {
		t.Error("unwired save seam lost the profile page")
	}
}

func TestPublicOwnerProfileSave(t *testing.T) {
	env, _ := alicePublicEnv(t)
	rec := &parityRecord{}
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(rec))

	form := url.Values{
		"display_name": {"Alice A"},
		"location":     {"Berlin"},
		"timezone":     {"Europe/Berlin"},
		"bio_markdown": {"hello **world**"},
	}
	req := publicPostForm(s, "/alice/profile", form)
	if req.Code != http.StatusSeeOther {
		t.Fatalf("save = %d, want 303 (%.240q)", req.Code, req.Body.String())
	}
	if loc := req.Header().Get("Location"); loc != "/alice?flash=Profile+saved" {
		t.Fatalf("Location = %q", loc)
	}
	if len(rec.saves) != 1 || rec.saves[0].DisplayName != "Alice A" ||
		rec.saves[0].Location != "Berlin" || rec.saves[0].Timezone != "Europe/Berlin" ||
		rec.saves[0].BioMarkdown != "hello **world**" {
		t.Fatalf("save input = %+v", rec.saves)
	}

	// The flash transport renders the notice on the next GET.
	body := publicGet(s, "/alice?flash=Profile+saved").Body.String()
	if !strings.Contains(body, "Profile saved") || !strings.Contains(body, `role="status"`) {
		t.Errorf("flash notice missing: %.300s", body)
	}

	// A too-long display name re-renders with the API's message and the
	// submitted values (400, never a 500).
	bad := url.Values{"display_name": {strings.Repeat("x", 201)}, "location": {"Oslo"}}
	res := publicPostForm(s, "/alice/profile", bad)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("long name = %d, want 400", res.Code)
	}
	for _, want := range []string{
		"display_name too long (max 200 characters)",
		`role="alert"`,
		`value="Oslo"`,
		strings.Repeat("x", 60), // the submitted text survives
	} {
		if !strings.Contains(res.Body.String(), want) {
			t.Errorf("validation re-render missing %q", want)
		}
	}
	if len(rec.saves) != 1 {
		t.Fatalf("invalid form reached the service (%d saves)", len(rec.saves))
	}

	// The timezone SHAPE rule is the API's (not time.LoadLocation): up to four
	// slash-free segments are fine, an illegal character is not.
	res = publicPostForm(s, "/alice/profile", url.Values{"timezone": {"bad zone!"}})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "timezone must be an IANA zone name") {
		t.Fatalf("bad timezone = %d %.200q", res.Code, res.Body.String())
	}
	// A multi-segment IANA-shaped zone is accepted (forward compatible).
	res = publicPostForm(s, "/alice/profile", url.Values{"timezone": {"America/Argentina/Buenos_Aires"}})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("multi-segment timezone = %d, want 303", res.Code)
	}

	// A service failure re-renders with its text at the mapped status.
	rec.saveErr = errors.Join(identity.ErrConflict, errors.New("profile changed concurrently"))
	res = publicPostForm(s, "/alice/profile", url.Values{"display_name": {"A"}})
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "profile changed concurrently") {
		t.Fatalf("service failure = %d %.200q", res.Code, res.Body.String())
	}
}

func TestPublicOwnerProfileSaveGates(t *testing.T) {
	env, _ := alicePublicEnv(t)
	rec := &parityRecord{}
	deps := parityProfileDeps(rec)
	form := url.Values{"display_name": {"Alice"}}

	// Cross-origin refused.
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, deps)
	res := publicDoPost(s, "/alice/profile", form, map[string]string{"Origin": "http://evil.example"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-origin save = %d", res.Code)
	}

	// Not the owner (and no grant): the API's 403, same text.
	other := newPublicServer(t, env, auth.Principal{Name: "bob", Write: true}, deps)
	res = publicPostForm(other, "/alice/profile", form)
	if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "only this owner may edit the profile") {
		t.Fatalf("non-editor save = %d %.160q", res.Code, res.Body.String())
	}

	// The wired org-owner grant lets a non-name-matched principal through
	// (api's OwnerEditor seam, the same one the JSON PUT consults).
	envGrant := publicEnv(publicCfg("token", true), &publicFakeRegistry{owners: []string{"acme"}, repos: map[string][]string{"acme": {"widget"}}})
	envGrant.API.OwnerEdit = publicFakeOwnerEdit{allow: true}
	granted := newPublicServer(t, envGrant, auth.Principal{Name: "carol", Write: true}, PublicDeps{
		ProfileSave: func(context.Context, string, OwnerProfileInput) error { return nil },
	})
	res = publicPostForm(granted, "/acme/profile", form)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("granted save = %d, want 303 (%.200q)", res.Code, res.Body.String())
	}

	// Anonymous under a real auth mode → the login interstitial.
	anon := newPublicServer(t, publicEnv(publicCfg("oidc", true), nil), auth.Anonymous(), deps)
	res = publicPostForm(anon, "/alice/profile", form)
	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "/login-required?") {
		t.Fatalf("anonymous save = %d %q", res.Code, res.Header().Get("Location"))
	}

	// Unwired seam: 503 (the form would never have rendered).
	unwired := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{})
	res = publicPostForm(unwired, "/alice/profile", form)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired save = %d, want 503", res.Code)
	}

	// A syntactically invalid owner slug is a 404 (the API's validProfileOwner).
	res = publicPostForm(unwired, "/bad!name/profile", form)
	if res.Code != http.StatusNotFound {
		t.Fatalf("invalid owner = %d, want 404", res.Code)
	}
}

func TestPublicOwnerAvatarActions(t *testing.T) {
	env, _ := alicePublicEnv(t)
	rec := &parityRecord{}
	deps := parityProfileDeps(rec)

	// Self: both verbs render beside the avatar.
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, deps)
	body := publicGet(s, "/alice").Body.String()
	for _, want := range []string{
		`action="/alice/avatar/regenerate"`,
		`action="/alice/avatar/remove"`,
		"Regenerate avatar",
		"Remove avatar",
		`src="/api/v1/users/alice/avatar?v=1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("self avatar sidebar missing %q", want)
		}
	}

	// Another viewer sees the avatar but no controls.
	bob := newPublicServer(t, env, auth.Principal{Name: "bob", Write: true}, deps)
	body = publicGet(bob, "/alice").Body.String()
	if strings.Contains(body, `action="/alice/avatar/`) {
		t.Error("non-self viewer saw avatar controls")
	}
	if !strings.Contains(body, `src="/api/v1/users/alice/avatar?v=1"`) {
		t.Error("non-self viewer lost the avatar image")
	}

	// Host admin passes the identity verbs' self-or-admin gate.
	admin := newPublicServer(t, env, auth.Principal{Name: "root", Admin: true, Write: true}, deps)
	body = publicGet(admin, "/alice").Body.String()
	if !strings.Contains(body, `action="/alice/avatar/regenerate"`) {
		t.Error("admin lost the avatar controls")
	}

	// POST regenerate / remove: 303 with a notice, service called with the slug.
	res := publicPostForm(s, "/alice/avatar/regenerate", nil)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/alice?flash=Avatar+regenerated" {
		t.Fatalf("regenerate = %d %q", res.Code, res.Header().Get("Location"))
	}
	res = publicPostForm(s, "/alice/avatar/remove", nil)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/alice?flash=Avatar+removed" {
		t.Fatalf("remove = %d %q", res.Code, res.Header().Get("Location"))
	}
	if len(rec.regen) != 1 || rec.regen[0] != "alice" || len(rec.removed) != 1 || rec.removed[0] != "alice" {
		t.Fatalf("avatar calls = regen %v removed %v", rec.regen, rec.removed)
	}

	// Gates.
	res = publicPostForm(bob, "/alice/avatar/remove", nil)
	if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "self or admin") {
		t.Fatalf("non-self remove = %d %.160q", res.Code, res.Body.String())
	}
	res = publicDoPost(s, "/alice/avatar/remove", nil, map[string]string{"Origin": "http://evil.example"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-origin remove = %d", res.Code)
	}
	anon := newPublicServer(t, publicEnv(publicCfg("oidc", true), nil), auth.Anonymous(), deps)
	res = publicPostForm(anon, "/alice/avatar/remove", nil)
	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "/login-required?") {
		t.Fatalf("anonymous remove = %d %q", res.Code, res.Header().Get("Location"))
	}
	unwired := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{})
	res = publicPostForm(unwired, "/alice/avatar/remove", nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired remove = %d, want 503", res.Code)
	}

	// A service error maps to its status (unknown principal → 404). BindPublic
	// is process-wide, so re-install this test's deps after the unwired probe
	// above rebound them.
	BindPublic(deps)
	rec.avatarErr = errors.Join(identity.ErrNotFound, errors.New("unknown principal"))
	res = publicPostForm(s, "/alice/avatar/remove", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing avatar principal = %d, want 404", res.Code)
	}
}

func TestPublicOwnerAvatarRemoveNeedsAvatar(t *testing.T) {
	// No avatar to show → Regenerate still offers the opt-back-in, Remove does
	// not (the JSX Show).
	env, _ := alicePublicEnv(t)
	env.API.Avatars = publicFakeUserAvatar{url: ""}
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(&parityRecord{}))

	body := publicGet(s, "/alice").Body.String()
	if !strings.Contains(body, `action="/alice/avatar/regenerate"`) {
		t.Error("avatarless self lost Regenerate (opt back in)")
	}
	if strings.Contains(body, `action="/alice/avatar/remove"`) {
		t.Error("avatarless self saw Remove")
	}
}

// --- helpers -----------------------------------------------------------------

// parityOwnerEditErr is an OwnerEditor whose probe fails (fail-closed 503).
type parityOwnerEditErr struct{}

func (parityOwnerEditErr) CanEditOwnerProfile(context.Context, string, auth.Principal) (bool, error) {
	return false, errors.New("authz probe down")
}

// parityFailRegistry is a registry whose listing read fails.
type parityFailRegistry struct{ *publicFakeRegistry }

func (parityFailRegistry) Repos(context.Context, string) ([]string, error) {
	return nil, errors.New("registry down")
}

func TestValidateOwnerProfileRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   OwnerProfileInput
		want string
	}{
		{"empty is valid", OwnerProfileInput{}, ""},
		{"display name at budget", OwnerProfileInput{DisplayName: strings.Repeat("é", 200)}, ""},
		{"display name over budget", OwnerProfileInput{DisplayName: strings.Repeat("é", 201)}, "display_name too long (max 200 characters)"},
		{"location over budget", OwnerProfileInput{Location: strings.Repeat("x", 201)}, "location too long (max 200 characters)"},
		{"timezone over bytes", OwnerProfileInput{Timezone: strings.Repeat("a", 65)}, "timezone must be an IANA zone name (e.g. Europe/Berlin)"},
		{"timezone bad shape", OwnerProfileInput{Timezone: "not a zone"}, "timezone must be an IANA zone name (e.g. Europe/Berlin)"},
		{"timezone ok", OwnerProfileInput{Timezone: "Etc/GMT+5"}, ""},
		{"bio too long", OwnerProfileInput{BioMarkdown: strings.Repeat("b", maxOwnerProfileBio+1)}, "bio_markdown too long (max 64 KiB)"},
		{"bio invalid utf8", OwnerProfileInput{BioMarkdown: "\xff\xfe"}, "bio_markdown too long (max 64 KiB)"},
	}
	for _, tc := range cases {
		if got := validateOwnerProfile(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPublicOwnerProfileAuthzProbeFailsClosed(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.API.OwnerEdit = parityOwnerEditErr{}
	s := newPublicServer(t, env, auth.Principal{Name: "carol", Write: true}, PublicDeps{
		ProfileSave: func(context.Context, string, OwnerProfileInput) error { return nil },
	})
	res := publicPostForm(s, "/alice/profile", url.Values{"display_name": {"x"}})
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), "profile authorization unavailable") {
		t.Fatalf("probe failure = %d %.160q", res.Code, res.Body.String())
	}
}

func TestPublicOwnerWritesAuthFailure(t *testing.T) {
	env, _ := alicePublicEnv(t)
	deps := parityProfileDeps(&parityRecord{})
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, deps)
	// An invalid credential is a real 401 before any write logic runs.
	s.BindAuth(pages.Auth{
		Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad token"}
		},
		Refresh: func(http.ResponseWriter, *http.Request, auth.Principal) {},
	})
	for _, path := range []string{"/alice/profile", "/alice/avatar/remove"} {
		res := publicPostForm(s, path, url.Values{"display_name": {"x"}})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s: auth failure = %d, want 401", path, res.Code)
		}
	}
}

func TestPublicOwnerProfileMalformedBody(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(&parityRecord{}))
	// A body with an invalid percent escape fails ParseForm → 400, not a panic.
	req := httptest.NewRequest(http.MethodPost, "/alice/profile", strings.NewReader("display_name=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A browser form POST carries Origin; cookie-mode writes fail closed
	// without it (UI-07), so a raw request would 403 before parsing.
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "form parse failed") {
		t.Fatalf("malformed body = %d %.160q", rec.Code, rec.Body.String())
	}
}

func TestPublicOwnerProfileRerenderRegistryFailure(t *testing.T) {
	env, _ := alicePublicEnv(t)
	env.API.Repos = parityFailRegistry{&publicFakeRegistry{}}
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(&parityRecord{}))
	res := publicPostForm(s, "/alice/profile", url.Values{"display_name": {strings.Repeat("x", 201)}})
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("registry failure = %d, want 503", res.Code)
	}
}

func TestPublicOwnerAvatarInvalidSlug(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, parityProfileDeps(&parityRecord{}))
	res := publicPostForm(s, "/bad!name/avatar/regenerate", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("invalid avatar slug = %d, want 404", res.Code)
	}
}

// publicDoPost serves one POST with an optional urlencoded form body and
// headers through the page mux.
func publicDoPost(s *Server, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	// Browser-realistic same-origin write (the gate fails closed otherwise).
	if req.Header.Get("Origin") == "" && req.Header.Get("Referer") == "" {
		req.Header.Set("Origin", "http://"+req.Host)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func publicPostForm(s *Server, target string, form url.Values) *httptest.ResponseRecorder {
	return publicDoPost(s, target, form, nil)
}
