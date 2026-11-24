package webui

// parity_public.go — owner-page parity with the SPA's Repos.jsx OwnerPage:
// the self/admin profile edit form (Forgejo #234) and avatar self-service
// (Forgejo #376; regenerate/remove, the two actions the SPA exposed as
// buttons).
//
//	POST /{owner}/profile             → 303 /{owner}?flash=Profile saved
//	POST /{owner}/avatar/regenerate   → 303 /{owner}?flash=Avatar regenerated
//	POST /{owner}/avatar/remove       → 303 /{owner}?flash=Avatar removed
//
// Gates are the JSON twins' gates, not new ones:
//
//   - same-origin (the forms group's rule), then the anonymous-write
//     interstitial (?next=), exactly like every other page write;
//   - profile save: the API PUT's canEditProfile rule — host admin, the
//     name-matched principal, or the wired org-owner grant (probe errors fail
//     closed with 503) — and its field budgets/messages, so the page rejects
//     what the API rejects and accepts what it accepts;
//   - avatar actions: the identity verb's self-or-admin rule (404/403/503
//     from the service map straight through).
//
// The profile save itself is a PublicDeps seam: the page layer never writes
// the bucket by hand (package doc), so composition binds the API's own PUT
// body (see the WIRING header in public.go). A nil seam hides Edit entirely.

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/crueber/spindle/internal/server/auth"
)

// Owner profile field budgets — a verbatim port of api/profile.go's
// constants and messages (the page must reject exactly what the API rejects;
// api's validators are package-private, so this is the documented duplicate).
//
// PROFILE SAVE SEAM (integration requirement: cmd/spindle + one api export)
//
// The page cannot call the JSON API over HTTP (law 8) and must not write the
// bucket by hand (package doc), so PublicDeps.ProfileSave is where composition
// supplies the same code path the API's PUT uses. internal/api keeps that path
// in ownerProfilePut (package-private today). Recommended: lift its body into
// ONE exported function —
//
//	// api.PutOwnerProfile replaces owners/<owner>/profile.json with the API's
//	// field budgets, timezone shape check and bounded CAS loop. Errors carry
//	// the handler's statuses: validation → 400 (message text), CAS exhaustion
//	// → 409, store failure → 503.
//	func PutOwnerProfile(ctx context.Context, st store.ObjectStore, owner string,
//	    display, location, timezone, bio string, now time.Time) (OwnerProfile, error)
//
// and bind it:
//
//	d.ProfileSave = func(ctx context.Context, owner string, in webui.OwnerProfileInput) error {
//	    _, err := api.PutOwnerProfile(ctx, apiEnv.Store, owner,
//	        in.DisplayName, in.Location, in.Timezone, in.BioMarkdown, time.Now())
//	    return err
//	}
//
// Until that export lands, leave the field nil: the page renders the profile
// without an Edit affordance (never a button that cannot save). The page
// validates the budgets below first, so the seam's re-validation is defence in
// depth, not a UX surface.
const (
	maxOwnerProfileText = 200   // display_name / location, runes
	maxOwnerProfileTZ   = 64    // timezone, bytes
	maxOwnerProfileBio  = 65536 // bio_markdown, bytes
)

// ownerTZShape is the API's timezone SHAPE check (not time.LoadLocation: the
// runtime image ships no tz database). Deliberately identical, including the
// forward-compatible "unknown-but-shaped is accepted" stance.
var ownerTZShape = regexp.MustCompile(`^[A-Za-z0-9_+\-]{1,32}(/[A-Za-z0-9_+\-]{1,32}){0,3}$`)

// validateOwnerProfile returns the API's rejection text for a decoded form
// ("" means valid). Empty strings are "unset" — clearing a field is posting
// "".
func validateOwnerProfile(in OwnerProfileInput) string {
	if utf8.RuneCountInString(in.DisplayName) > maxOwnerProfileText {
		return "display_name too long (max 200 characters)"
	}
	if utf8.RuneCountInString(in.Location) > maxOwnerProfileText {
		return "location too long (max 200 characters)"
	}
	if len(in.Timezone) > maxOwnerProfileTZ || (in.Timezone != "" && !ownerTZShape.MatchString(in.Timezone)) {
		return "timezone must be an IANA zone name (e.g. Europe/Berlin)"
	}
	if len(in.BioMarkdown) > maxOwnerProfileBio || !utf8.ValidString(in.BioMarkdown) {
		return "bio_markdown too long (max 64 KiB)"
	}
	return ""
}

// ownerProfileCanEdit resolves the API PUT's authority chain after the write
// preflight: host admin or name match, else the wired org-owner grant. A probe
// error fails closed with 503 (never 403-as-404). It writes the denial.
func (s *Server) ownerProfileCanEdit(w http.ResponseWriter, r *http.Request, owner string, p auth.Principal) bool {
	if p.Admin || strings.EqualFold(p.Name, owner) {
		return true
	}
	if s.env.API != nil && s.env.API.OwnerEdit != nil {
		ok, err := s.env.API.OwnerEdit.CanEditOwnerProfile(r.Context(), owner, p)
		if err != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "profile authorization unavailable")
			return false
		}
		if ok {
			return true
		}
	}
	s.errorPage(w, r, http.StatusForbidden, "only this owner may edit the profile")
	return false
}

// publicOwnerProfileSave serves POST /{owner}/profile. A rejected field
// re-renders the profile view with the submitted values and the API's message
// (400); a service failure maps to its status and also re-renders, so the
// typed text is never lost. Success is a 303 to the profile with a notice —
// the redirect re-reads the stored doc, so no optimistic state exists.
func (s *Server) publicOwnerProfileSave(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !validOwnerPath(owner) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, p, "Edit this profile") {
		return
	}
	if publicDeps.ProfileSave == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "profile editing is not wired on this build")
		return
	}
	if !s.ownerProfileCanEdit(w, r, owner, p) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	in := OwnerProfileInput{
		DisplayName: r.PostFormValue("display_name"),
		Location:    r.PostFormValue("location"),
		// The timezone is the one field whose API shape check is byte-exact;
		// trimming here keeps a pasted " Europe/Berlin " from failing the
		// page's own check, and the seam stores the trimmed value.
		Timezone:    strings.TrimSpace(r.PostFormValue("timezone")),
		BioMarkdown: r.PostFormValue("bio_markdown"),
	}
	if msg := validateOwnerProfile(in); msg != "" {
		s.ownerProfileRerender(w, r, owner, p, in, msg, http.StatusBadRequest)
		return
	}
	if err := publicDeps.ProfileSave(r.Context(), owner, in); err != nil {
		s.ownerProfileRerender(w, r, owner, p, in, parityErrMessage(err), parityErrStatus(err))
		return
	}
	s.formsRedirectFlash(w, r, "/"+urlPathEscape(owner), "Profile saved")
}

// ownerProfileRerender re-renders the profile view with the submitted values,
// the edit form open, and the message inline.
func (s *Server) ownerProfileRerender(w http.ResponseWriter, r *http.Request, owner string, p auth.Principal, in OwnerProfileInput, msg string, status int) {
	data, ok := s.ownerPageData(w, r, owner, "profile", p)
	if !ok {
		return
	}
	data.Profile = ownerProfileForm{
		DisplayName: in.DisplayName,
		Location:    in.Location,
		Timezone:    in.Timezone,
		BioMarkdown: in.BioMarkdown,
	}
	data.Editing = true
	data.Err = msg
	s.page(w, r, p, status, "public/owner", owner, data)
}

// urlPathEscape percent-escapes one owner segment (the redirect target).
func urlPathEscape(s string) string { return url.PathEscape(s) }

// publicOwnerAvatarRegenerate / Remove serve the two avatar verbs the SPA
// exposed. Both share one gate chain: authenticated, same-origin, self-or-
// admin (the identity service re-checks its own copy on call).
func (s *Server) publicOwnerAvatarRegenerate(w http.ResponseWriter, r *http.Request) {
	s.publicOwnerAvatar(w, r, true)
}

// publicOwnerAvatarUpload serves POST /{owner}/avatar/upload (WP9 G10): the
// page half of identity.Service.PutUserAvatarBytes (PUT
// /api/v1/users/{principal}/avatar). The multipart body is capped exactly at
// the verb's 2 MiB (identity.maxUserAvatarUploadBytes, documented duplicate —
// the page must reject what the API rejects); the service owns the
// PNG/JPEG/GIF magic-sniff allowlist, the server-side center-crop and the
// self-or-admin authority. A rejected upload re-renders the error page with
// the verb's status (413 too large, 415 unsupported type).
func (s *Server) publicOwnerAvatarUpload(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !validOwnerPath(owner) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, p, "Change this avatar") {
		return
	}
	if !p.Admin && !strings.EqualFold(p.Name, owner) {
		s.errorPage(w, r, http.StatusForbidden, "self or admin")
		return
	}
	fn := publicDeps.UploadAvatar
	if fn == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "avatar upload is not wired on this build")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUserAvatarUploadBytes+1)
	if perr := r.ParseMultipartForm(maxUserAvatarUploadBytes + 1); perr != nil {
		if errors.Is(perr, http.ErrNotMultipart) {
			s.errorPage(w, r, http.StatusBadRequest, "choose an avatar file (PNG, JPEG or GIF)")
			return
		}
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "avatar too large (max 2 MiB)")
		return
	}
	f, _, ferr := r.FormFile("avatar")
	if ferr != nil {
		s.errorPage(w, r, http.StatusBadRequest, "choose an avatar file (PNG, JPEG or GIF)")
		return
	}
	defer f.Close()
	data, rerr := io.ReadAll(io.LimitReader(f, maxUserAvatarUploadBytes+1))
	if rerr != nil || int64(len(data)) > maxUserAvatarUploadBytes {
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "avatar too large (max 2 MiB)")
		return
	}
	if len(data) == 0 {
		s.errorPage(w, r, http.StatusBadRequest, "choose an avatar file (PNG, JPEG or GIF)")
		return
	}
	if _, err := fn(r.Context(), owner, data); err != nil {
		s.errorPage(w, r, formsErrStatus(err), formsErrMessage(err))
		return
	}
	s.formsRedirectFlash(w, r, "/"+urlPathEscape(owner), "Avatar uploaded")
}

// maxUserAvatarUploadBytes is identity's upload cap (2 MiB), duplicated here
// because the multipart reader must bound the body before the service sees
// it; identity.PutUserAvatarBytes still enforces its own copy.
const maxUserAvatarUploadBytes = int64(2 << 20)

func (s *Server) publicOwnerAvatarRemove(w http.ResponseWriter, r *http.Request) {
	s.publicOwnerAvatar(w, r, false)
}

func (s *Server) publicOwnerAvatar(w http.ResponseWriter, r *http.Request, regenerate bool) {
	owner := r.PathValue("owner")
	if !validOwnerPath(owner) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, p, "Change this avatar") {
		return
	}
	fn := publicDeps.RegenerateAvatar
	msg := "Avatar regenerated"
	if !regenerate {
		fn = publicDeps.RemoveAvatar
		msg = "Avatar removed"
	}
	if fn == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "avatar management is not wired on this build")
		return
	}
	// The identity verbs' own gate: self or host admin (normPrincipal compare).
	if !p.Admin && !strings.EqualFold(p.Name, owner) {
		s.errorPage(w, r, http.StatusForbidden, "self or admin")
		return
	}
	if _, err := fn(r.Context(), owner); err != nil {
		s.errorPage(w, r, parityErrStatus(err), parityErrMessage(err))
		return
	}
	s.formsRedirectFlash(w, r, "/"+urlPathEscape(owner), msg)
}

// --- moderation block control (PLAN-V2 §M.5, M1) -----------------------------

// publicOwnerBlock / publicOwnerUnblock serve POST /{owner}/block and
// /{owner}/unblock: the viewer's OWN list, so the gate is signed-in +
// same-origin + a different, existing user (no self-block, no org). The
// moderation verb owns storage, the audit record and the unknown-subject
// check; the page never writes the bucket by hand.
func (s *Server) publicOwnerBlock(w http.ResponseWriter, r *http.Request) {
	s.publicOwnerBlockToggle(w, r, true)
}

func (s *Server) publicOwnerUnblock(w http.ResponseWriter, r *http.Request) {
	s.publicOwnerBlockToggle(w, r, false)
}

func (s *Server) publicOwnerBlockToggle(w http.ResponseWriter, r *http.Request, block bool) {
	owner := r.PathValue("owner")
	if !validOwnerPath(owner) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	action := "Block this user"
	if !block {
		action = "Unblock this user"
	}
	if !s.parityWritePreflight(w, r, p, action) {
		return
	}
	if publicDeps.Blocks == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "moderation is not wired on this build")
		return
	}
	// The page mirrors the API's self rule: the list edited is always the
	// caller's own.
	blocker := p.Name
	if strings.EqualFold(blocker, owner) {
		s.errorPage(w, r, http.StatusBadRequest, "you cannot block yourself")
		return
	}
	// Orgs are not block targets (a block is a person relationship).
	if publicDeps.Org != nil {
		if org, oerr := publicDeps.Org(r.Context(), owner); oerr == nil && org != nil {
			s.errorPage(w, r, http.StatusBadRequest, "organizations cannot be blocked")
			return
		}
	}
	var (
		changed bool
		err     error
	)
	msg := "Blocked @" + owner
	if block {
		changed, err = publicDeps.Blocks.Block(r.Context(), blocker, owner)
	} else {
		changed, err = publicDeps.Blocks.Unblock(r.Context(), blocker, owner)
		msg = "Unblocked @" + owner
	}
	if err != nil {
		s.errorPage(w, r, parityErrStatus(err), parityErrMessage(err))
		return
	}
	if !changed {
		if block {
			msg = "Already blocked"
		} else {
			msg = "Not blocked"
		}
	}
	s.formsRedirectFlash(w, r, "/"+urlPathEscape(owner), msg)
}
