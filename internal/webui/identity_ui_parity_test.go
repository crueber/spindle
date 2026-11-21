// identity_ui_parity_test.go — WP9 G9/G10/G17 page coverage: collaborator
// invitations on Settings → Access, the owner-page avatar upload, and the
// user's starred-repositories page. The seams are bound to the REAL identity
// service and the API's own visibility rule (memory store), so these tests pin
// page/API parity rather than a fake's behaviour.
package webui

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/social"
	"github.com/crueber/spindle/internal/store"
)

// uiPNG1x1 / uiPNG2x1 are real PNGs the identity upload path decodes and
// center-crops (different widths, so identical stored bytes would mean the
// second upload was ignored).
var (
	uiPNG1x1 = uiPNG(1, 1, 40)
	uiPNG2x1 = uiPNG(2, 1, 200)
)

func uiPNG(w, h int, red uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: red, G: 120, B: uint8(200 - red/2), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// uiSeedRepo materializes the manifest probe CreateRepoInvite's repoAlive
// checks (a repo is role-addressable while repos/<o>/<r>/manifest.pb exists).
func uiSeedRepo(t *testing.T, mem store.ObjectStore, owner, repo string) {
	t.Helper()
	if _, err := store.PutBytes(context.Background(), mem, "repos/"+owner+"/"+repo+"/manifest.pb",
		[]byte("manifest"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
		t.Fatalf("seed repo manifest: %v", err)
	}
}

// uiMultipart serves one multipart form POST through the page mux.
func uiMultipart(t *testing.T, s *Server, target, field, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("multipart field: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Host = "example.com"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// --- G9: collaborator invitations -------------------------------------------

func TestSettingsAccessInviteLifecycle(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	uiSeedRepo(t, h.store, "acme", "monorepo")

	// The Access tab renders the admin invite form.
	code, body := seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("GET access = %d (%s)", code, body)
	}
	settingsMustContain(t, body, "Invite a collaborator", "invite form")
	settingsMustContain(t, body, `id="invite-subject"`, "invite email field")
	settingsMustContain(t, body, "no pending invitations", "empty invite list")

	// Create: the response is the page (never a redirect) carrying the
	// one-shot accept link, and the pending row carries the same link.
	rec := settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access", url.Values{
		"invite_action": {"create"},
		"subject":       {"dave@example.com"},
		"role":          {"write"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create invite = %d: %s", rec.Code, rec.Body.String())
	}
	created := rec.Body.String()
	settingsMustContain(t, created, "invitation created", "create notice")
	settingsMustContain(t, created, "dave@example.com", "pending row")
	settingsMustContain(t, created, `/invitations?id=`, "accept link")
	settingsMustContain(t, created, `token=`, "accept link carries the capability token")
	settingsMustContain(t, created, `data-copy="#invite-link"`, "one-shot copy affordance")

	// The link is the human accept page, not the raw API path.
	if strings.Contains(created, `/api/v1/invitations/`) {
		t.Fatalf("accept link leaked the API path: %s", settingsTruncate(created, 400))
	}

	// The invitation is pending in the bucket, with the page's 7-day TTL.
	invs, err := h.ident.ListRepoInvites(ctx, "acme", "monorepo", 100)
	if err != nil || len(invs) != 1 {
		t.Fatalf("ListRepoInvites: %v %+v", err, invs)
	}
	inv := invs[0]
	if inv.Subject != "dave@example.com" || inv.Role != "write" {
		t.Fatalf("invite = %+v", inv)
	}

	// Accept binds the role to the INVITEE: dave resolves write, and the
	// issuing admin's role is untouched (the actor is not the invitee).
	dave := auth.Principal{Name: "dave@example.com", Email: "dave@example.com"}
	bound, err := h.ident.AcceptInviteFor(ctx, dave, inv.ID)
	if err != nil || bound != identity.InviteRepo {
		t.Fatalf("accept = %q, %v", bound, err)
	}
	role, _ := h.ident.Resolve(ctx, "acme", "monorepo", dave)
	if role != identity.RoleWrite {
		t.Fatalf("dave role = %q (want write)", role)
	}
	aliceRole, _ := h.ident.Resolve(ctx, "acme", "monorepo", seamAdmin())
	if aliceRole != identity.RoleAdmin {
		t.Fatalf("inviter role = %q (the invitee's binding must not touch the actor)", aliceRole)
	}

	// The accepted invite is gone from the page.
	code, body = seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("GET access after accept = %d", code)
	}
	settingsMustContain(t, body, "no pending invitations", "accepted invite gone")

	// Cancel: create, cancel through the page, the link is dead afterwards.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access", url.Values{
		"invite_action": {"create"},
		"subject":       {"erin@example.com"},
		"role":          {"read"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create invite 2 = %d: %s", rec.Code, rec.Body.String())
	}
	erin, _ := h.ident.ListRepoInvites(ctx, "acme", "monorepo", 100)
	if len(erin) != 1 {
		t.Fatalf("pending after create 2 = %+v", erin)
	}
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access/"+erin[0].ID,
		url.Values{"invite_action": {"cancel"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel invite = %d: %s", rec.Code, rec.Body.String())
	}
	if left, _ := h.ident.ListRepoInvites(ctx, "acme", "monorepo", 100); len(left) != 0 {
		t.Fatalf("pending after cancel = %+v", left)
	}
	if _, aerr := h.ident.AcceptInvite(ctx, "erin@example.com", erin[0].ID); aerr == nil {
		t.Fatal("accepting a cancelled invitation must fail")
	}
	// Cancelling again is the API's 404, rendered as the page's error.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access/"+erin[0].ID,
		url.Values{"invite_action": {"cancel"}}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-cancel = %d, want 404", rec.Code)
	}
}

func TestSettingsAccessInviteValidationAndFloor(t *testing.T) {
	h := newSeamsHarness(t)
	uiSeedRepo(t, h.store, "acme", "monorepo")

	// The service's own validation, rendered inline with the API's 400: a
	// bad role and a non-email subject.
	for _, form := range []url.Values{
		{"invite_action": {"create"}, "subject": {"dave@example.com"}, "role": {"super"}},
		{"invite_action": {"create"}, "subject": {"bad!!principal"}, "role": {"read"}},
	} {
		rec := settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access", form, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid invite %v = %d: %s", form, rec.Code, rec.Body.String())
		}
		settingsMustContain(t, rec.Body.String(), "invalid", "validation message")
	}

	// The typed subject survives a rejected write (no lost edit).
	rec := settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access", url.Values{
		"invite_action": {"create"}, "subject": {"dave@example.com"}, "role": {"super"},
	}, nil)
	settingsMustContain(t, rec.Body.String(), `value="dave@example.com"`, "draft subject restored")

	// A triage collaborator gets the explanatory arm, never the form and
	// never a capability token; the write is refused before the seam runs.
	ctx := context.Background()
	if _, err := h.ident.PutAccess(ctx, "acme", "monorepo", "", identity.VisibilityPrivate,
		[]identity.AccessBinding{{Subject: "user:carol@example.com", Role: identity.RoleTriage}}); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	carol := auth.Principal{Name: "carol@example.com", Email: "carol@example.com"}
	h.s.auth = pageAuthFor(carol)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("triage GET access = %d: %s", code, body)
	}
	settingsMustContain(t, body, "Only a repository admin can invite collaborators.", "triage explanation")
	settingsMustContain(t, body, "Only a repository admin can see pending invitations.", "triage list explanation")
	settingsMustNotContain(t, body, "identity service is not wired", "triage must not be told the service is unwired")
	settingsMustNotContain(t, body, `name="subject"`, "triage invite form field")
	settingsMustNotContain(t, body, `name="invite_action" value="create"`, "triage invite form action")
	settingsMustNotContain(t, body, "invite-link", "triage capability link")
	// The access write form is admin-only too: a triage reader never gets a
	// form whose submit would 403.
	settingsMustNotContain(t, body, `name="visibility"`, "triage visibility form")
	settingsMustNotContain(t, body, `name="binding_count"`, "triage bindings form")
	settingsMustContain(t, body, "Only a repository admin can change visibility or role bindings.", "triage access read-only note")

	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/access", url.Values{
		"invite_action": {"create"}, "subject": {"dave@example.com"}, "role": {"read"},
	}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("triage create = %d, want 403", rec.Code)
	}
	// The seam's own floor, independent of the page gate.
	_, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", carol, SettingsRepoWrite{
		Action: SettingsActionInviteCreate, Values: map[string]string{"subject": "dave@example.com", "role": "read"},
	})
	if werr == nil || werr.Status != http.StatusForbidden {
		t.Fatalf("seam non-admin invite = %+v, want 403", werr)
	}
}

// --- G10: user avatar upload ------------------------------------------------

func TestOwnerAvatarUpload(t *testing.T) {
	mem := store.NewMemory()
	ident := identity.New(mem, config.Defaults())
	// RFC 3339 second granularity: advance the clock so two uploads in one
	// test observe a moved avatar_updated_at (the ?v= cache-bust).
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ticks := 0
	ident.Now = func() time.Time { ticks++; return base.Add(time.Duration(ticks) * time.Minute) }
	if _, err := ident.EnsureProfile(context.Background(), "alice"); err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	env, _ := alicePublicEnv(t)
	env.API.Avatars = ident // the real avatar URL projection (cache-busted)
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{
		UploadAvatar: ident.PutUserAvatarBytes,
		ProfileSave:  func(context.Context, string, OwnerProfileInput) error { return nil },
	})

	// The edit surface carries the file input; the accept list is the
	// identity verb's allowlist (no WebP/SVG).
	rec := publicGet(s, "/alice?edit=1")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		`action="/alice/avatar/upload"`,
		`enctype="multipart/form-data"`,
		`accept="image/png,image/jpeg,image/gif"`,
		`name="avatar"`,
	)

	// Upload: 303 back with the flash; the stored pointer names a PNG.
	rec = uiMultipart(t, s, "/alice/avatar/upload", "avatar", "me.png", uiPNG1x1)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	prof, err := ident.GetProfile(context.Background(), "alice")
	if err != nil || prof == nil || prof.AvatarContentType != "image/png" || prof.AvatarUpdatedAt == "" {
		t.Fatalf("profile after upload = %+v, %v", prof, err)
	}
	first := prof.AvatarUpdatedAt
	rawFirst, _, _ := ident.GetUserAvatar(context.Background(), "alice")

	// Replacing with a different image moves the cache-busted URL (the page
	// renders the new version, and the bytes really change).
	rec = uiMultipart(t, s, "/alice/avatar/upload", "avatar", "me2.png", uiPNG2x1)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("second upload = %d: %s", rec.Code, rec.Body.String())
	}
	prof2, _ := ident.GetProfile(context.Background(), "alice")
	if prof2.AvatarUpdatedAt == first {
		t.Fatal("re-upload must move avatar_updated_at (the ?v= cache-bust)")
	}
	raw, _, gerr := ident.GetUserAvatar(context.Background(), "alice")
	if gerr != nil || len(raw) == 0 || bytes.Equal(raw, rawFirst) {
		t.Fatalf("stored avatar bytes did not change: %d, %v", len(raw), gerr)
	}
	rec = publicGet(s, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "/api/v1/users/alice/avatar?v=")

	// A non-image body is the verb's 415, an oversized one the 413.
	rec = uiMultipart(t, s, "/alice/avatar/upload", "avatar", "notes.txt", []byte("not an image"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad type upload = %d, want 415: %s", rec.Code, rec.Body.String())
	}
	big := make([]byte, maxUserAvatarUploadBytes+10)
	copy(big, uiPNG1x1)
	rec = uiMultipart(t, s, "/alice/avatar/upload", "avatar", "huge.png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413: %s", rec.Code, rec.Body.String())
	}

	// Self-or-admin: bob cannot upload alice's avatar.
	rec = uiMultipart(t, s, "/bob/avatar/upload", "avatar", "me.png", uiPNG1x1)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("account-owner gate = %d, want 403", rec.Code)
	}
}

func TestOwnerAvatarRemovalShowsInitials(t *testing.T) {
	mem := store.NewMemory()
	ident := identity.New(mem, config.Defaults())
	if _, err := ident.EnsureProfile(context.Background(), "alice"); err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	env, _ := alicePublicEnv(t)
	env.API.Avatars = ident
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{
		UploadAvatar:     ident.PutUserAvatarBytes,
		RemoveAvatar:     ident.DeleteUserAvatar,
		RegenerateAvatar: ident.RegenerateUserAvatar,
	})

	// No avatar installed → the initials chip is the visible anchor.
	rec := publicGet(s, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, `aria-hidden="true">A<`)
	publicWantContains(t, rec, `class="profile-avatar flex h-24 w-24`)

	// Install, then remove: the image disappears and the initials return.
	if _, err := ident.PutUserAvatarBytes(context.Background(), "alice", uiPNG1x1); err != nil {
		t.Fatalf("PutUserAvatarBytes: %v", err)
	}
	rec = publicGet(s, "/alice")
	publicWantContains(t, rec, `/api/v1/users/alice/avatar?v=`)
	rec = publicDo(s, http.MethodPost, "/alice/avatar/remove", map[string]string{"Origin": "http://example.com"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d: %s", rec.Code, rec.Body.String())
	}
	rec = publicGet(s, "/alice")
	publicWantContains(t, rec, `aria-hidden="true">A<`)
	publicWantNotContains(t, rec, `/api/v1/users/alice/avatar?v=`)
}

// --- G17: starred repositories ---------------------------------------------

// uiReadAccess is the api.Env.Access read gate double: "notes" is private to
// alice; everything else is readable.
type uiReadAccess struct{}

func (uiReadAccess) CheckRead(_ context.Context, _owner, repo string, p auth.Principal) *auth.AuthError {
	if repo == "notes" && p.Name != "alice" {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}
	}
	return nil
}

func TestOwnerStarredPageVisibilityAndOrder(t *testing.T) {
	reg := &publicFakeRegistry{owners: []string{"alice", "acme"}, repos: map[string][]string{
		"alice": {"hello", "notes"},
		"acme":  {"widget"},
	}}
	env := publicEnv(publicCfg("token", true), reg)
	env.API.Access = uiReadAccess{}
	env.API.RepoVisibility = func(_ context.Context, _owner, repo string) (string, bool) {
		if repo == "notes" {
			return "private", true
		}
		return "public", true
	}
	entries := []social.StarEntry{
		{Repo: "acme/widget", StarredAt: "2026-11-01T10:00:00Z"},
		{Repo: "alice/hello", StarredAt: "2026-11-02T10:00:00Z"},
		{Repo: "alice/notes", StarredAt: "2026-11-03T10:00:00Z"}, // invisible to carol
	}
	starred := func(_ context.Context, principal string, _ int, _ string) ([]social.StarEntry, bool, error) {
		if principal != "alice" {
			return nil, false, nil
		}
		return entries, true, nil
	}
	exists := func(_ context.Context, u string) (bool, error) { return u == "alice", nil }
	deps := PublicDeps{Starred: starred, UserExists: exists}

	s := newPublicServer(t, env, auth.Principal{Name: "carol"}, deps)
	rec := publicGet(s, "/alice/starred")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec,
		"Starred repositories",
		`href="/alice/hello"`,
		`href="/acme/widget"`,
	)
	// The private repo carol cannot read never appears (the JSON twin
	// deliberately does not filter; the page must).
	publicWantNotContains(t, rec, "alice/notes")
	// Order is the service's repo-ascending keyset, not starred_at.
	body := rec.Body.String()
	if ai, wi := strings.Index(body, `href="/acme/widget"`), strings.Index(body, `href="/alice/hello"`); ai < 0 || wi < 0 || ai > wi {
		t.Fatalf("starred order = widget@%d hello@%d, want widget first (repo-ascending)", ai, wi)
	}
	// The cursor names the last RENDERED row (never the invisible last key).
	publicWantContains(t, rec, `href="/alice/starred?after=2026-11-02T10%3A00%3A00Z%7Calice%2Fhello"`)
	publicWantNotContains(t, rec, "2026-11-03T10%3A00%3A00Z")

	// The owner sees her own private star.
	sAlice := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec = publicGet(sAlice, "/alice/starred")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "alice/notes")

	// Unknown user → 404 (never a fabricated empty list).
	rec = publicGet(s, "/nobody/starred")
	publicWantStatus(t, rec, http.StatusNotFound)

	// Organizations have no star inbox → 404.
	orgDeps := PublicDeps{
		Starred:    starred,
		UserExists: exists,
		Org: func(_ context.Context, o string) (*identity.Org, error) {
			if o == "acme" {
				return &identity.Org{Org: "acme"}, nil
			}
			return nil, nil
		},
	}
	sOrg := newPublicServer(t, env, auth.Principal{Name: "carol"}, orgDeps)
	rec = publicGet(sOrg, "/acme/starred")
	publicWantStatus(t, rec, http.StatusNotFound)

	// Empty star inbox → the honest empty state, no Older link.
	empty := func(context.Context, string, int, string) ([]social.StarEntry, bool, error) { return nil, false, nil }
	sEmpty := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{Starred: empty, UserExists: exists})
	rec = publicGet(sEmpty, "/alice/starred")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "No starred repositories")
	publicWantNotContains(t, rec, "older starred repositories")
}

// --- edge coverage: upload gate branches -----------------------------------

func TestOwnerAvatarUploadGateBranches(t *testing.T) {
	env, _ := alicePublicEnv(t)
	// Unwired seam → the honest 503 card (never a dead form).
	s := newPublicServer(t, env, auth.Principal{Name: "alice", Write: true}, PublicDeps{})
	rec := uiMultipart(t, s, "/alice/avatar/upload", "avatar", "me.png", uiPNG1x1)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired upload = %d, want 503", rec.Code)
	}
	// An invalid owner slug is a 404 before any service call.
	if rec := publicGet(s, "/.bad/hidden"); rec.Code == 0 {
		t.Fatal("unreachable")
	}

	mem := store.NewMemory()
	ident := identity.New(mem, config.Defaults())
	if _, err := ident.EnsureProfile(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	env2, _ := alicePublicEnv(t)
	s2 := newPublicServer(t, env2, auth.Principal{Name: "alice", Write: true}, PublicDeps{
		UploadAvatar: ident.PutUserAvatarBytes,
	})
	// Invalid path slug (leading dot) → 404, not a service call.
	req := httptest.NewRequest(http.MethodPost, "/.bad/avatar/upload", nil)
	req.Header.Set("Origin", "http://example.com")
	rr := httptest.NewRecorder()
	s2.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("invalid owner upload = %d, want 404", rr.Code)
	}
	// Missing file field → 400.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("note", "x")
	_ = mw.Close()
	req = httptest.NewRequest(http.MethodPost, "/alice/avatar/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://example.com")
	rr = httptest.NewRecorder()
	s2.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing file = %d, want 400", rr.Code)
	}
	// Empty file → 400.
	rec = uiMultipart(t, s2, "/alice/avatar/upload", "avatar", "empty.png", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty file = %d, want 400", rec.Code)
	}
	// A body that is not multipart at all → the malformed-body 400.
	req = httptest.NewRequest(http.MethodPost, "/alice/avatar/upload", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "http://example.com")
	rr = httptest.NewRecorder()
	s2.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unparsable body = %d, want 400", rr.Code)
	}
}

// --- edge coverage: starred list ------------------------------------------

func TestOwnerStarredEdges(t *testing.T) {
	reg := &publicFakeRegistry{owners: []string{"alice"}, repos: map[string][]string{"alice": {"hello"}}}
	env := publicEnv(publicCfg("token", true), reg)
	exists := func(_ context.Context, u string) (bool, error) { return u == "alice", nil }

	// Unwired seam → the honest empty state, no rows.
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{UserExists: exists})
	rec := publicGet(s, "/alice/starred")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "No starred repositories")

	// Malformed entries are skipped; `more` with no rendered row must not
	// leak an invisible repo name into the ?after= link.
	starred := func(context.Context, string, int, string) ([]social.StarEntry, bool, error) {
		return []social.StarEntry{
			{Repo: "noslash", StarredAt: "2026-11-01T10:00:00Z"},
			{Repo: "", StarredAt: "2026-11-01T10:00:00Z"},
			{Repo: "alice/hello/too/deep", StarredAt: "2026-11-01T10:00:00Z"},
		}, true, nil
	}
	s2 := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{Starred: starred, UserExists: exists})
	rec = publicGet(s2, "/alice/starred")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "No starred repositories")
	publicWantNotContains(t, rec, "after=")

	// A visibility probe failure is a 503 (never a half-filtered list that
	// could show a private row).
	failReg := &failingRepos{publicFakeRegistry: reg}
	envFail := publicEnv(publicCfg("token", true), failReg)
	star := func(context.Context, string, int, string) ([]social.StarEntry, bool, error) {
		return []social.StarEntry{{Repo: "alice/hello", StarredAt: "2026-11-01T10:00:00Z"}}, false, nil
	}
	s3 := newPublicServer(t, envFail, auth.Principal{Name: "alice"}, PublicDeps{Starred: star, UserExists: exists})
	rec = publicGet(s3, "/alice/starred")
	publicWantStatus(t, rec, http.StatusServiceUnavailable)
}

// failingRepos makes the owner listing fail (the starred visibility probe).
type failingRepos struct{ *publicFakeRegistry }

func (f *failingRepos) Repos(context.Context, string) ([]string, error) {
	return nil, errors.New("registry down")
}

// --- edge coverage: invitation seams --------------------------------------

func TestSettingsAccessInviteSeamEdges(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	uiSeedRepo(t, h.store, "acme", "monorepo")
	// An expired pending invite renders the expired chip (the list is a raw
	// read — expiry is display state, and accept still fails closed).
	// The seam's clock is the harness's fixed 2026-11-20; a TTL two days back
	// from the real clock is already expired there.
	if _, err := h.ident.CreateRepoInvite(ctx, "acme", "monorepo", "old@example.com", identity.RoleRead, "alice@example.com", -48*time.Hour); err != nil {
		t.Fatal(err)
	}
	code, body := seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("GET access = %d", code)
	}
	settingsMustContain(t, body, "expired", "expired invite chip")

	// The seam's id guard (a cancel without an id) is the 400.
	if _, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", seamAdmin(), SettingsRepoWrite{
		Action: SettingsActionInviteCancel, Values: map[string]string{},
	}); werr == nil || werr.Status != http.StatusBadRequest {
		t.Fatalf("cancel without id = %+v, want 400", werr)
	}

	// An invite-list store fault fails the read (never a silent empty list a
	// capability panel could misrepresent).
	svc := identity.New(listErrStore{ObjectStore: h.store}, config.Defaults())
	deps := SettingsServices{Ident: svc, Now: func() time.Time { return publicFixedNow }}
	if _, err := deps.repoAccess(ctx, "acme", "monorepo", seamAdmin()); err == nil {
		t.Fatal("invite-list fault must surface")
	}

	// A repo that does not exist is the API's 404 on create.
	if _, werr := h.deps.RepoWrite(ctx, "acme", "ghost", seamAdmin(), SettingsRepoWrite{
		Action: SettingsActionInviteCreate, Values: map[string]string{"subject": "x@y.z", "role": "read"},
	}); werr == nil || werr.Status != http.StatusNotFound {
		t.Fatalf("create on missing repo = %+v, want 404", werr)
	}
}

// listErrStore fails LIST only (the invite collection read).
type listErrStore struct{ store.ObjectStore }

func (listErrStore) List(context.Context, string, string, func(store.ObjectMeta) error) error {
	return errors.New("list down")
}
