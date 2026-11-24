// public.go — the public page group of the server-rendered UI (D-WEB-14): the
// landing page, the how-it-works deep dive, the API docs, the anonymous write
// interstitial, the owner profile/repositories/organizations tabs, the
// invitation inbox, and the notification tray.
//
// The routes are the browser routes web/src/index.jsx froze; every handler is
// a thin view model over the same seams the JSON API reads (law 8 — no HTTP
// loopback, no store writes here):
//
//	GET  /                          landing (static marketing, zero store reads)
//	GET  /how-it-works               developer deep dive (static)
//	GET  /api                        API docs (static table + live discovery)
//	GET  /login-required             anonymous write interstitial (Forgejo #502)
//	GET  /{owner}                    owner profile (identity + tabs)
//	GET  /{owner}/repositories       repositories tab (listing rows)
//	GET  /{owner}/organizations      organizations tab (memberships)
//	GET  /invitations                invitation inbox (Forgejo #362)
//	POST /invitations/{id}/accept    accept an invitation
//	POST /invitations/{id}/decline   decline an invitation
//	GET  /notifications              per-user tray (06 §7)
//	POST /notifications/read-all     mark the index window read
//	POST /notifications/{id}/read    mark one entry read
//	POST /notifications/{id}/unread  mark one entry unread
//
// Precedence: the literal patterns (/api, /invitations, /notifications,
// /login-required, /how-it-works) beat the /{owner} wildcard in HTTP ServeMux,
// and /{owner} reserves the names of pages owned elsewhere in the page tree
// (reservedOwnerNames) so a reserved single-segment route reaches its own
// page rather than rendering an owner by that name. Only GET /{$} (root
// exactly) claims "/" — "GET /" would claim every GET path.
//
// Gates: every page runs s.gated (require-read: anonymous callers are admitted
// only when anonymous_read is on), the same rule the API applies. Content is
// visibility-filtered through api.Env.VisibleRepos / VisibleOwners, so a private
// repo never leaks through a listing. Writes require a non-anonymous principal
// and a same-origin request.
//
// WIRING (cmd/spindle, once per process, before serving; ident = the identity
// service, notes = the notify service the composition already builds):
//
//	webui.BindPublic(webui.PublicDeps{
//		Org:          ident.GetOrg,
//		MemberOrgs:   ident.MemberOrgs,
//		MyInvites:    ident.MyInvitesFor,
//		PreviewInvite: ident.PreviewInviteFor,
//		AcceptInvite:  ident.AcceptInviteFor,
//		CancelInvite: func(ctx context.Context, principal, id string) (*identity.Invitation, error) {
//			return ident.CancelInvite(ctx, principal, id)
//		},
//		Tray:                 notes.Tray,
//		UnreadCount:          notes.UnreadCount,
//		ReadAll:              notes.ReadAll,
//		SetNotificationState: notes.SetState,
//		// Optional: StarCount (the social counters) and ExtraEndpoints (the
//		// templates passed to api.RegisterExposed, appended to /api's live list).
//		// Owner profile edit + avatar self-service (Forgejo #234/#376), from
//		// the same composition (ident = c.ident):
//		// ProfileSave needs one api export — see parity_public.go "PROFILE SAVE
//		// SEAM"; nil hides Edit. The avatar seams are exact method values.
//		ProfileSave:      <api.PutOwnerProfile adapter — parity_public.go>,
//		RegenerateAvatar: c.ident.RegenerateUserAvatar, // method value
//		RemoveAvatar:     c.ident.DeleteUserAvatar,     // method value
//	})
//
// A nil display dependency degrades that surface (no org badge, no member
// list) rather than failing the page. The two inbox seams are load-bearing,
// though: an unwired list answers 503 — an inbox that cannot read its source
// must not render as an empty inbox. Owner profiles are read from the bucket
// (owners/<owner>/profile.json, api.OwnerProfileKey) exactly like api's
// ownerProfileGet; repo listings come from api.Env (VisibleRepos) plus the
// shared size catalog, so the page and the JSON twin cannot disagree.
package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/httpsec"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/social"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
)

// PublicDeps carries the feature services the public pages read that are not
// on api.Env. Every field is optional: an unwired instance renders the page
// without that projection instead of failing (display metadata never 500s).
// The composition (cmd/spindle) binds them once via BindPublic; feature
// packages are never imported by the page layer for anything else (law 8).
type PublicDeps struct {
	// Org resolves an organization doc (nil, nil for a non-org owner).
	Org func(ctx context.Context, org string) (*identity.Org, error)
	// MemberOrgs lists the org names a principal belongs to (sorted; [] when
	// none — the organizations tab).
	MemberOrgs func(ctx context.Context, principal string) ([]string, error)

	// MyInvites lists the pending invitations addressed to the principal.
	MyInvites func(ctx context.Context, p auth.Principal) ([]identity.InboxEntry, error)
	// PreviewInvite resolves one invitation (subject- or token-authorized).
	PreviewInvite func(ctx context.Context, p auth.Principal, id, token string) (*identity.Invitation, error)
	// AcceptInvite binds the invitation to the principal and reports what it
	// bound ("membership" text from the service).
	AcceptInvite func(ctx context.Context, p auth.Principal, id string) (string, error)
	// CancelInvite declines/withdraws a pending invitation for the principal.
	CancelInvite func(ctx context.Context, principal, id string) (*identity.Invitation, error)

	// Tray is the per-user notification page (state/after filtered).
	Tray func(ctx context.Context, who, state, after string, n int) ([]notify.Notification, bool)
	// UnreadCount is the O(1) badge count.
	UnreadCount func(ctx context.Context, who string) int
	// ReadAll marks the unread index window read and reports how many moved.
	ReadAll func(ctx context.Context, who string) int
	// SetNotificationState flips one entry read/unread.
	SetNotificationState func(ctx context.Context, who, id string, read bool) (*notify.Notification, error)

	// StarCount projects one repo's star count for listing rows (optional:
	// nil renders no count — the SPA spends one social GET per row).
	StarCount func(ctx context.Context, owner, repo string) (int, bool)

	// ProfileSave replaces one owner profile (display name / location /
	// timezone / bio) through the same field budgets, timezone shape check and
	// bounded CAS the JSON API's PUT performs (api/profile.go). The page layer
	// never writes the bucket by hand, so composition binds this; nil hides
	// the Edit affordance (no button that cannot save). A non-nil error is the
	// API's text: 400 for a rejected field, 409 after the CAS budget, 503 for
	// a store failure.
	ProfileSave func(ctx context.Context, owner string, in OwnerProfileInput) error

	// RegenerateAvatar / RemoveAvatar are identity.Service's
	// RegenerateUserAvatar / DeleteUserAvatar method values (self-or-admin is
	// re-checked by the page before the call; the service owns the write and
	// the opt-out flag semantics). Nil hides those actions.
	RegenerateAvatar func(ctx context.Context, principal string) (*identity.Profile, error)
	RemoveAvatar     func(ctx context.Context, principal string) (*identity.Profile, error)
	// UploadAvatar installs an uploaded raster (identity.Service.
	// PutUserAvatarBytes: PNG/JPEG/GIF, 2 MiB cap, magic-sniff allowlist,
	// server-side center-crop to a square PNG). Nil hides the upload form.
	UploadAvatar func(ctx context.Context, principal string, data []byte) (*identity.Profile, error)

	// UserExists is identity.Service.UserExists: the canonical owner page
	// needs it to answer 404 for an unknown username instead of rendering an
	// empty profile (the profile read alone cannot tell "no profile yet" from
	// "no such user"). Nil degrades to rendering the page.
	UserExists func(ctx context.Context, username string) (bool, error)

	// Starred is social.Service.Starred: the user's starred repositories in
	// the JSON twin's repo-ascending order, with its cursor contract. Nil
	// renders the Starred tab as "not wired".
	Starred func(ctx context.Context, principal string, n int, after string) ([]social.StarEntry, bool, error)

	// ExtraEndpoints appends feature-owned discovery templates
	// (api.RegisterExposed call sites) to the /api page's live endpoint list.
	ExtraEndpoints []string

	// Blocks is the M1 moderation seam (PLAN-V2 §M.5): the profile page's
	// block/unblock control and the owner-only "Blocked (N)" disclosure.
	// Implemented by composition over *moderation.Service; nil hides the
	// control (no button that cannot save).
	Blocks BlockList
}

// BlockList is the profile page's read/write view of the block state. The
// page never writes the bucket by hand and never re-implements the list
// rule: the service owns storage, the audit entry, and the self/unknown
// checks.
type BlockList interface {
	// Blocked reports whether blocker has blocked subject.
	Blocked(ctx context.Context, blocker, subject string) (bool, error)
	// List returns the blocker's blocked principals (self-view only).
	List(ctx context.Context, principal string) ([]string, error)
	// Block adds subject and reports whether the state changed.
	Block(ctx context.Context, blocker, subject string) (bool, error)
	// Unblock removes subject and reports whether the state changed.
	Unblock(ctx context.Context, blocker, subject string) (bool, error)
}

// OwnerProfileInput is the owner edit form's four fields — exactly the shape
// api.ownerProfilePut decodes (unknown fields are not a page concept; the
// form posts only these names).
type OwnerProfileInput struct {
	DisplayName string
	Location    string
	Timezone    string
	BioMarkdown string
}

// publicDeps is the process-wide binding. It is written once at composition
// time (before the server accepts requests) and read by handlers; tests
// replace it directly. No lock: startup-only mutation, never per request.
var publicDeps PublicDeps

// BindPublic installs the feature-service bindings for the public pages.
func BindPublic(d PublicDeps) { publicDeps = d }

// reservedOwnerNames are single-segment browser routes owned by other page
// groups. GET /{owner} must not claim them: the owner handler returns without
// writing (the server's probeWriter sees an empty response and falls through
// to the next handler), exactly like the sentinel catch-all. An owner literally
// named one of these keeps its git/API paths and loses only its UI page — the
// same reservation class /orgs/new and friends use.
var reservedOwnerNames = map[string]bool{
	"explore":        true,
	"how-it-works":   true,
	"import":         true,
	"new":            true,
	"admin":          true,
	"keys":           true,
	"invitations":    true,
	"notifications":  true,
	"login-required": true,
}

func init() {
	registerGroup(func(s *Server) {
		s.mux.HandleFunc("GET /{$}", s.publicLanding)
		s.mux.HandleFunc("GET /how-it-works", s.publicHowItWorks)
		s.mux.HandleFunc("GET /api", s.publicAPIDocs)
		s.mux.HandleFunc("GET /login-required", s.publicLoginRequired)

		s.mux.HandleFunc("GET /invitations", s.publicInvitations)
		s.mux.HandleFunc("POST /invitations/{id}/accept", s.publicInvitationAccept)
		s.mux.HandleFunc("POST /invitations/{id}/decline", s.publicInvitationDecline)

		s.mux.HandleFunc("GET /notifications", s.publicNotifications)
		s.mux.HandleFunc("POST /notifications/read-all", s.publicNotificationsReadAll)
		s.mux.HandleFunc("POST /notifications/{id}/read", s.publicNotificationState)
		s.mux.HandleFunc("POST /notifications/{id}/unread", s.publicNotificationState)

		s.mux.HandleFunc("GET /{owner}", s.publicOwnerProfile)
		s.mux.HandleFunc("GET /{owner}/repositories", s.publicOwnerRepositories)
		s.mux.HandleFunc("GET /{owner}/organizations", s.publicOwnerOrganizations)
		s.mux.HandleFunc("GET /{owner}/starred", s.publicOwnerStarred)

		// Owner profile edit + avatar self-service (Forgejo #234/#376, upload
		// #601). Plain POSTs, 303 back with a notice; parity_public.go holds
		// the handlers.
		s.mux.HandleFunc("POST /{owner}/profile", s.publicOwnerProfileSave)
		s.mux.HandleFunc("POST /{owner}/avatar/regenerate", s.publicOwnerAvatarRegenerate)
		s.mux.HandleFunc("POST /{owner}/avatar/remove", s.publicOwnerAvatarRemove)
		s.mux.HandleFunc("POST /{owner}/avatar/upload", s.publicOwnerAvatarUpload)

		// Moderation block control (PLAN-V2 §M.5, M1): plain POSTs, 303 back
		// with a notice — the same write shape as the profile/avatar verbs.
		s.mux.HandleFunc("POST /{owner}/block", s.publicOwnerBlock)
		s.mux.HandleFunc("POST /{owner}/unblock", s.publicOwnerUnblock)
	})
}

// --- landing / how-it-works / api docs / login interstitial -------------------

// publicLanding answers GET /: static marketing with zero store reads (the
// front door spends no round trips). 390px: the concept sections stack
// (grid-cols-1 → md:grid-cols-2), the quickstart <pre> scrolls in place.
func (s *Server) publicLanding(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "public/landing", "", struct{}{})
}

// publicHowItWorks answers GET /how-it-works: the developer deep dive (static,
// zero API calls). 390px: single column; the two diagrams are fluid-width
// SVGs and every table/paragraph wraps rather than panning.
func (s *Server) publicHowItWorks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "public/how_it_works", "How it works", struct{}{})
}

// apiRouteRow is one row of the static §8 route table on the API docs page.
type apiRouteRow struct {
	Method string
	Path   string
	What   string
}

// apiDocsData is the /api view model: the static §8 route table (ported
// verbatim from web/src/pages/Apidocs.jsx, itself derived from 07_api.md §8)
// plus the live discovery document (GET /api/v1 shapes, nothing fetched).
type apiDocsData struct {
	Routes         []apiRouteRow
	Endpoints      []string
	APIVersion     int
	Base           string
	BrowserBase    string
	SDK            string
	AuthSetup      string
	AuthBrowser    string
	BrowserLogin   bool
	LoginURL       string
	Mode           string
	SDKSnippet     string
	CISnippet      string
	ReleaseSnippet string
	NotifySnippet  string
}

// publicAPIDocs answers GET /api. 390px: the route table sits in an
// overflow-x-auto wrapper (scrolls in place, never pans the page); the code
// snippets scroll horizontally inside .code-view.
//
// Integration note: internal/server's router currently registers "/api" as the
// JSON API lane before the wildcard that reaches the page provider, so a
// browser GET /api answers the API's 404. The composition must let the page
// provider answer browser GETs for this path (the header's API link and the
// SPA both point here); the handler itself needs no change.
func (s *Server) publicAPIDocs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "public/apidocs", "API", apiDocsData{
		Routes:         apiRouteTable(),
		Endpoints:      s.discoveryEndpoints(),
		APIVersion:     1,
		Base:           "/api/v1",
		BrowserBase:    "/api-browser/v1",
		SDK:            "/repos.js",
		AuthSetup:      "/services/setup.json",
		AuthBrowser:    "/api-browser/v1",
		BrowserLogin:   s.browserLoginEnabled(),
		LoginURL:       s.loginURL(),
		Mode:           "oidc",
		SDKSnippet:     sdkSnippet,
		CISnippet:      ciSnippet,
		ReleaseSnippet: releaseSnippet,
		NotifySnippet:  notifySnippet,
	})
}

// discoveryEndpoints derives the live endpoints[] exactly like
// internal/api/discovery.go: the exposed templates of the route table, deduped
// in registration order, plus the feature-owned templates the composition
// registered through api.RegisterExposed. It never invents a row (both the
// static table and this list are contracts).
func (s *Server) discoveryEndpoints() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, rt := range api.Routes(nil) {
		if !rt.Expose || rt.Template == "" || seen[rt.Template] {
			continue
		}
		seen[rt.Template] = true
		out = append(out, rt.Template)
	}
	for _, tmpl := range publicDeps.ExtraEndpoints {
		if tmpl == "" || seen[tmpl] {
			continue
		}
		seen[tmpl] = true
		out = append(out, tmpl)
	}
	return out
}

// publicLoginRequired answers GET /login-required (Forgejo #502): the
// anonymous write interstitial. ?next= names the attempted action (preserved
// through OIDC login) and ?action= names it in the copy. The login button
// renders only when the browser flow exists; otherwise the page says so
// (never a dead button). 390px: the action row wraps.
func (s *Server) publicLoginRequired(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	next := sanitizeNextPath(r.URL.Query().Get("next"))
	action := r.URL.Query().Get("action")
	if action == "" {
		action = "this action"
	}
	href := ""
	if u := s.loginURL(); u != "" {
		href = u + "?next=" + url.QueryEscape(next)
	}
	s.page(w, r, p, http.StatusOK, "public/login_required", "Log in to continue", struct {
		Next      string
		Action    string
		LoginHref string
	}{next, action, href})
}

// sanitizeNextPath is the server-side mirror of lib/writeGate.js
// sanitizeNextClient and delegates to the one shared rule: a local path
// starting with exactly one "/"; anything else (scheme, "//host", "/\host",
// control characters, empty) falls back to "/". No open redirect.
func sanitizeNextPath(next string) string {
	return httpsec.SafeNext(next, "/")
}

// browserLoginEnabled is the discovery §8 gate (the session/client trio). It
// mirrors api.browserLoginEnabled without importing server internals; config
// validation refuses a config without the trio, so a false here means the
// restricted boot (which serves no pages).
func (s *Server) browserLoginEnabled() bool {
	if s.env.Cfg == nil {
		return false
	}
	a := s.env.Cfg.Server.Auth
	return a.SessionSecret != "" &&
		a.OAuthClientID != "" && a.OAuthClientSecret != ""
}

// loginURL is the browser login entry the interstitial links to ("" when the
// flow cannot start).
func (s *Server) loginURL() string {
	if !s.browserLoginEnabled() {
		return ""
	}
	return "/_auth/login"
}

// --- owner pages -------------------------------------------------------------

// ownerProfileDoc is the stored owners/<owner>/profile.json shape
// (internal/api/profile.go OwnerProfile; the key is truth, never the body's
// owner field).
type ownerProfileDoc struct {
	DisplayName string `json:"display_name"`
	Location    string `json:"location"`
	Timezone    string `json:"timezone"`
	BioMarkdown string `json:"bio_markdown"`
}

// repoRowVM is one listing row: link, visibility/mirror badges, optional star
// count, and the last-active stamp carried by the shared size catalog
// (Forgejo #247 — no per-row fetch).
type repoRowVM struct {
	Owner           string
	Name            string
	Full            string
	At              string
	Empty           bool
	Mirror          bool
	MirrorTitle     string
	VisibilityLabel string
	VisibilityTitle string
	Stars           int
	HasStars        bool
}

// ownerProfileForm is the profile edit form's seeded state (the four fields
// the API's PUT accepts, in the order the form shows them).
type ownerProfileForm struct {
	DisplayName string
	Location    string
	Timezone    string
	BioMarkdown string
}

// ownerData is the view model shared by the three owner views: the sidebar
// tabs (all views), the identity block (profile view), the listing
// (repositories view), and the membership list (organizations view).
type ownerData struct {
	Owner string
	View  string // "profile" | "repos" | "orgs"

	IsOrg       bool
	DisplayName string
	// Handle renders the @slug line under the display name (user profiles
	// only, and only when a display name was set — the JSX Show condition).
	Handle      bool
	Description string
	Where       string // "location · timezone", filtered
	BioMarkdown string
	AvatarURL   string

	RepoCount  int
	Rows       []repoRowVM
	MemberOrgs []string

	CanManage bool

	// Profile edit (Forgejo #234): CanEdit is the server's can_edit rule AND a
	// wired save seam; Editing is the ?edit=1 URL state the SPA kept in a
	// signal (client-only view state becomes URL state here). Profile seeds
	// the form — the stored doc normally, the submitted values after a
	// rejected save.
	CanEdit bool
	Editing bool
	Profile ownerProfileForm

	// Avatar self-service (Forgejo #376): Self is the identity service's
	// self-or-admin rule for the avatar verbs (the page only shows the
	// controls; the service re-checks). Remove renders only when an avatar is
	// actually displayed (the JSX Show). UploadAvatar is the uploaded-raster
	// verb (Forgejo #601); Initials is the fallback chip when no avatar is
	// installed (the shell's identity-menu fallback).
	Self            bool
	CanRegenAvatar  bool
	CanRemoveAvatar bool
	CanUploadAvatar bool
	Initials        string

	// Starred view (WP9 G17): MoreAfter is the page cursor for the "Older"
	// link; empty means the list ended.
	MoreAfter string

	// Moderation (PLAN-V2 §M.5, M1). CanBlock is the signed-in, non-self,
	// non-org viewer case; Blocked is whether the viewer already blocks this
	// owner (the control then offers Unblock). The list itself is PRIVATE to
	// the blocker: BlockedPrincipals/BlockCount render on one's own profile
	// only (BlockListSelf), never on someone else's page.
	CanBlock          bool
	Blocked           bool
	BlockedPrincipals []string
	BlockCount        int
	BlockListSelf     bool

	Notice string
	Err    string
}

func (s *Server) publicOwnerProfile(w http.ResponseWriter, r *http.Request) {
	s.publicOwner(w, r, "profile")
}

func (s *Server) publicOwnerRepositories(w http.ResponseWriter, r *http.Request) {
	s.publicOwner(w, r, "repos")
}

func (s *Server) publicOwnerOrganizations(w http.ResponseWriter, r *http.Request) {
	s.publicOwner(w, r, "orgs")
}

// publicOwnerStarred serves GET /{owner}/starred (WP9 G17).
func (s *Server) publicOwnerStarred(w http.ResponseWriter, r *http.Request) {
	s.publicOwner(w, r, "starred")
}

// publicOwner serves the three owner views from one view model so the header,
// tabs, gates, and visibility filter can never fork by tab (the SPA's
// OwnerPage rationale). 390px: the profile-layout grid is one column
// (sm:grid-cols-[minmax(0,1fr)_12rem] stacks), the listing is one column, the
// sidebar tabs sit below the main column — no fixed widths, no overflow.
func (s *Server) publicOwner(w http.ResponseWriter, r *http.Request, view string) {
	owner := r.PathValue("owner")
	// A single-segment path that names a page owned elsewhere is not an
	// owner: write nothing so the next handler can answer it.
	if reservedOwnerNames[owner] {
		return
	}
	if !validOwnerPath(owner) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	data, ok := s.ownerPageData(w, r, owner, view, p)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "public/owner", owner, data)
}

// validOwnerPath is the owner-segment charset check (the same ParseRepoId rule
// the API's validProfileOwner uses).
func validOwnerPath(owner string) bool {
	_, err := git.ParseRepoId(owner + "/x")
	return err == nil
}

// ownerPageData builds the shared owner view model (and writes the response on
// failure). The profile-save handler reuses it so a rejected save re-renders
// the exact page it came from, with the submitted values.
func (s *Server) ownerPageData(w http.ResponseWriter, r *http.Request, owner, view string, p auth.Principal) (*ownerData, bool) {
	if s.env.API == nil || s.env.API.Repos == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "repository registry unavailable")
		return nil, false
	}
	names, err := s.env.API.VisibleRepos(r.Context(), owner, p)
	if err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "repositories unavailable")
		return nil, false
	}
	data := &ownerData{Owner: owner, View: view, RepoCount: len(names), Notice: formsFlash(r)}

	// Org-ness rides one identity read (the SPA's orgs.get); a failed probe
	// degrades to a user profile — display metadata never fails the page.
	var org *identity.Org
	if publicDeps.Org != nil {
		if o, oerr := publicDeps.Org(r.Context(), owner); oerr == nil {
			org = o
		}
	}
	if org != nil {
		data.IsOrg = true
		data.DisplayName = firstNonEmpty(org.DisplayName, owner)
		data.Description = org.Description
		data.Where = joinWhere(org.Location, org.Timezone)
		data.BioMarkdown = org.BioMarkdown
		if org.AvatarContentType != "" {
			data.AvatarURL = "/api/v1/orgs/" + url.PathEscape(strings.ToLower(owner)) + "/avatar"
			if org.AvatarUpdatedAt != "" {
				data.AvatarURL += "?v=" + url.QueryEscape(org.AvatarUpdatedAt)
			}
		}
		data.CanManage = !p.Anonymous && s.canEditOwner(r.Context(), owner, p)
	} else {
		prof := s.readOwnerProfile(r.Context(), owner)
		data.DisplayName = firstNonEmpty(prof.DisplayName, owner)
		data.Handle = prof.DisplayName != ""
		data.Where = joinWhere(prof.Location, prof.Timezone)
		data.BioMarkdown = prof.BioMarkdown
		data.Profile = ownerProfileForm{
			DisplayName: prof.DisplayName,
			Location:    prof.Location,
			Timezone:    prof.Timezone,
			BioMarkdown: prof.BioMarkdown,
		}
		if s.env.API.Avatars != nil && !p.Anonymous {
			data.AvatarURL = s.env.API.Avatars.UserAvatarURL(r.Context(), owner)
		}
		// Forgejo #234: the API's can_edit (host admin, name match, wired
		// org-owner grant). The Edit affordance also needs a save seam, or it
		// would be a button that cannot write.
		data.CanEdit = !p.Anonymous && publicDeps.ProfileSave != nil && s.canEditOwner(r.Context(), owner, p)
		data.Editing = view == "profile" && data.CanEdit && r.URL.Query().Get("edit") == "1"
		// Forgejo #376: avatar self-service is self-or-admin, exactly the
		// identity verb's gate.
		data.Self = !p.Anonymous && (p.Admin || strings.EqualFold(p.Name, owner))
		data.CanRegenAvatar = data.Self && publicDeps.RegenerateAvatar != nil
		data.CanRemoveAvatar = data.CanRegenAvatar && publicDeps.RemoveAvatar != nil && data.AvatarURL != ""
		data.CanUploadAvatar = data.Self && publicDeps.UploadAvatar != nil
		data.Initials = initials(owner)

		// Moderation (PLAN-V2 §M.5, M1): the block list is private to the
		// blocker. Self sees the disclosure; any other signed-in viewer sees
		// only the control (Block/Unblock), never the list. A nil seam hides
		// both (no button that cannot save).
		if publicDeps.Blocks != nil && !p.Anonymous {
			if data.Self {
				data.BlockListSelf = true
				if names, berr := publicDeps.Blocks.List(r.Context(), owner); berr == nil {
					data.BlockedPrincipals = names
					data.BlockCount = len(names)
				}
			} else if !data.IsOrg {
				data.CanBlock = true
				if blocked, berr := publicDeps.Blocks.Blocked(r.Context(), p.Name, owner); berr == nil {
					data.Blocked = blocked
				}
			}
		}
	}

	switch view {
	case "repos":
		data.Rows = s.ownerRepoRows(r.Context(), owner, names)
	case "orgs":
		if publicDeps.MemberOrgs != nil {
			orgs, merr := publicDeps.MemberOrgs(r.Context(), owner)
			if merr == nil {
				sort.Strings(orgs)
				data.MemberOrgs = orgs
			}
		}
	case "starred":
		// Starred is a USER surface (WP9 G17): an org has no star inbox, and
		// an unknown username is a real 404 — never an empty list that
		// pretends the user exists.
		if org != nil || data.IsOrg {
			s.errorPage(w, r, http.StatusNotFound, "not found")
			return nil, false
		}
		if publicDeps.UserExists != nil {
			exists, uerr := publicDeps.UserExists(r.Context(), owner)
			if uerr != nil {
				s.errorPage(w, r, http.StatusServiceUnavailable, "identity unavailable")
				return nil, false
			}
			if !exists {
				// A repo owner is an account even without a user.json binding
				// or a profile doc (a token-mode principal). Only a namespace
				// with no identity trace AND no repositories is unknown.
				repos, rerr := s.env.API.Repos.Repos(r.Context(), owner)
				if rerr != nil {
					s.errorPage(w, r, http.StatusServiceUnavailable, "repository registry unavailable")
					return nil, false
				}
				if len(repos) == 0 {
					s.errorPage(w, r, http.StatusNotFound, "not found")
					return nil, false
				}
			}
		}
		rows, moreAfter, serr := s.ownerStarredRows(r.Context(), owner, p, r.URL.Query().Get("after"))
		if serr != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "starred repositories unavailable")
			return nil, false
		}
		data.Rows = rows
		data.MoreAfter = moreAfter
	}
	return data, true
}

// ownerStarredRows projects the star index into the shared repo-row shape
// (WP9 G17). Visibility is the API's own rule — VisibleRepos per distinct
// writer owner — so a private repo the viewer cannot read never appears (the
// JSON twin deliberately does not filter; the page must). Order is the
// service's repo-ascending keyset order, preserved verbatim; the cursor is
// the JSON twin's `<starred_at>|<repo>` of the last returned entry.
func (s *Server) ownerStarredRows(ctx context.Context, owner string, p auth.Principal, after string) ([]repoRowVM, string, error) {
	if publicDeps.Starred == nil {
		return nil, "", nil
	}
	entries, more, err := publicDeps.Starred(ctx, owner, 50, after)
	if err != nil {
		return nil, "", err
	}
	type pick struct{ owner, name, full, at string }
	picks := make([]pick, 0, len(entries))
	visible := map[string]map[string]bool{}
	for _, e := range entries {
		o, n, ok := strings.Cut(e.Repo, "/")
		if !ok || o == "" || n == "" || strings.Contains(n, "/") {
			continue
		}
		set, ok := visible[o]
		if !ok {
			names, verr := s.env.API.VisibleRepos(ctx, o, p)
			if verr != nil {
				return nil, "", verr
			}
			set = make(map[string]bool, len(names))
			for _, name := range names {
				set[name] = true
			}
			visible[o] = set
		}
		if set[n] {
			picks = append(picks, pick{owner: o, name: n, full: e.Repo, at: e.StarredAt})
		}
	}
	// One catalog read supplies the activity stamp / empty flag for every
	// row (the owner listing's own projection); a corrupt catalog degrades to
	// null measurements, never to a failed page.
	var cat *proto.RepoCatalog
	if s.env.API.Store != nil {
		if c, cerr := sizecatalog.ReadCatalog(ctx, s.env.API.Store); cerr == nil {
			cat = c
		}
	}
	rows := make([]repoRowVM, 0, len(picks))
	for _, pk := range picks {
		vm := repoRowVM{Owner: pk.owner, Name: pk.name, Full: pk.full}
		if cat != nil {
			if pr := sizecatalog.RowsForOwner(pk.owner, []string{pk.name}, cat); len(pr) == 1 {
				if pr[0].LastCommitTime != nil {
					vm.At = *pr[0].LastCommitTime
				}
				if pr[0].SizeBytes != nil && *pr[0].SizeBytes == 0 {
					vm.Empty = true
				}
			}
		}
		rows = append(rows, vm)
	}
	s.fillRowFlags(ctx, owner, rows)
	// The cursor names the last RENDERED entry: resuming after an invisible
	// key would echo a repo the viewer cannot read into the URL (and the
	// ?after= link). Without a rendered row there is no safe cursor, so the
	// page ends rather than leaking one.
	next := ""
	if more && len(picks) > 0 {
		last := picks[len(picks)-1]
		next = last.at + "|" + last.full
	}
	return rows, next, nil
}

// readOwnerProfile reads owners/<owner>/profile.json; absent or unreadable
// docs read as the empty profile (the api §8 unknown-owner convention). A
// corrupt doc degrades the same way — the bio is display metadata.
func (s *Server) readOwnerProfile(ctx context.Context, owner string) ownerProfileDoc {
	var doc ownerProfileDoc
	if s.env.API == nil || s.env.API.Store == nil {
		return doc
	}
	raw, _, err := store.GetBytes(ctx, s.env.API.Store, store.OwnerProfileKey(owner), store.GetOptions{})
	if err != nil {
		return ownerProfileDoc{}
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ownerProfileDoc{}
	}
	return doc
}

// canEditOwner is the owner-profile write rule (api/canEditProfile): host
// admin, name-matched principal, or the wired org-owner grant. Only used to
// decide whether the organization settings link renders; the settings page
// re-checks on save.
func (s *Server) canEditOwner(ctx context.Context, owner string, p auth.Principal) bool {
	if p.Anonymous {
		return false
	}
	if p.Admin || strings.EqualFold(p.Name, owner) {
		return true
	}
	if s.env.API != nil && s.env.API.OwnerEdit != nil {
		if ok, err := s.env.API.OwnerEdit.CanEditOwnerProfile(ctx, owner, p); err == nil {
			return ok
		}
	}
	return false
}

// ownerRepoRows builds the activity-ordered listing rows: one catalog read for
// size/activity (the same sizecatalog fold api's detailed listing uses), then
// the badge projections. Sort is the explore ordering (activity desc), so the
// owner page and /explore agree.
func (s *Server) ownerRepoRows(ctx context.Context, owner string, names []string) []repoRowVM {
	rows := make([]sizecatalog.Row, 0, len(names))
	if s.env.API.Store != nil {
		cat, err := sizecatalog.ReadCatalog(ctx, s.env.API.Store)
		if err != nil {
			// A corrupt catalog is a sick bucket, but the listing is still the
			// authoritative membership: degrade to null measurements (the api
			// contract says delete/absent degrades, corrupt 503s — the page
			// keeps its content and drops the optional columns rather than
			// losing the page to a badge).
			cat = nil
		}
		rows = sizecatalog.RowsForOwner(owner, names, cat)
	} else {
		for _, n := range names {
			rows = append(rows, sizecatalog.Row{Owner: owner, Name: n})
		}
	}
	rows = sizecatalog.FilterSort(rows, "activity", "desc", nil, nil)
	out := make([]repoRowVM, 0, len(rows))
	for _, row := range rows {
		vm := repoRowVM{Owner: owner, Name: row.Name, Full: owner + "/" + row.Name}
		if row.LastCommitTime != nil {
			vm.At = *row.LastCommitTime
		}
		if row.SizeBytes != nil && *row.SizeBytes == 0 {
			vm.Empty = true
		}
		out = append(out, vm)
	}
	s.fillRowFlags(ctx, owner, out)
	return out
}

// fillRowFlags stamps the per-row badges (visibility, mirror, optional star
// count) onto the listing rows.
//
// ### Concurrency
//
// Hazard: N sequential store/hook probes would serialize the listing behind N
// round trips (the SPA issued them in parallel). Avoidance: one goroutine per
// row with at most 8 in flight; each goroutine writes only its own slice
// index, so no lock is needed and the WaitGroup joins every probe before the
// response renders (no shared map, no channel carrying results). No lock is
// held across any store call. ctx cancellation stops launching new probes and
// the in-flight ones drain through ctx; the join still runs before return.
func (s *Server) fillRowFlags(ctx context.Context, owner string, rows []repoRowVM) {
	if len(rows) == 0 {
		return
	}
	const maxInFlight = 8
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// rows carry their own owner when a list spans namespaces (the
			// starred view); the caller's owner is the single-owner default.
			o := rows[i].Owner
			if o == "" {
				o = owner
			}
			if s.env.API != nil && s.env.API.RepoVisibility != nil {
				if vis, ok := s.env.API.RepoVisibility(ctx, o, rows[i].Name); ok {
					rows[i].VisibilityLabel, rows[i].VisibilityTitle = visibilityBadge(vis)
				}
			}
			if s.env.API != nil && s.env.API.Store != nil {
				if body, _, err := store.GetBytes(ctx, s.env.API.Store, store.MirrorKey(o, rows[i].Name), store.GetOptions{}); err == nil && body != nil {
					rows[i].Mirror = true
					rows[i].MirrorTitle = "mirror · pull-only"
					var doc struct {
						UpstreamURL string `json:"upstream_url"`
					}
					if json.Unmarshal(body, &doc) == nil && strings.TrimSpace(doc.UpstreamURL) != "" {
						rows[i].MirrorTitle = "mirror of " + strings.TrimSpace(doc.UpstreamURL) + " · pull-only"
					}
				}
			}
			if publicDeps.StarCount != nil {
				if n, ok := publicDeps.StarCount(ctx, o, rows[i].Name); ok && n > 0 {
					rows[i].Stars, rows[i].HasStars = n, true
				}
			}
		}(i)
	}
	wg.Wait()
}

// visibilityBadge is the server twin of lib/visibility.js visibilityBadge:
// the row's spelling becomes the pill; unknown/empty renders nothing (never
// assume private).
func visibilityBadge(vis string) (label, title string) {
	switch strings.ToLower(vis) {
	case "public":
		return "public", "public — anyone may read"
	case "authenticated":
		return "authenticated", "private — signed-in users may read"
	case "private":
		return "private", "private — owner or organization members only"
	default:
		return "", ""
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// joinWhere renders the "location · timezone" meta line, dropping empty
// fields (the JSX [location, timezone].filter(Boolean).join(" · ")).
func joinWhere(location, timezone string) string {
	switch {
	case location != "" && timezone != "":
		return location + " · " + timezone
	case location != "":
		return location
	default:
		return timezone
	}
}

// --- invitations -------------------------------------------------------------

// inviteRowVM is one inbox row (or the shared-link preview).
type inviteRowVM struct {
	ID           string
	Kind         string
	Scope        string
	Role         string
	InvitedBy    string
	ExpiresAt    string
	ExpiresLabel string // absolute UTC date for a live expiry (rel reads a future stamp as "just now")
	CreatedAt    string
	State        string
	Expired      bool
}

// invitationsData is the /invitations view model.
type invitationsData struct {
	Rows []inviteRowVM
	Note string
	Err  string

	LinkID  string
	Link    *inviteRowVM
	LinkErr string
}

// publicInvitations answers GET /invitations: the invitee inbox (Forgejo
// #362). A shared accept link deep-links here as ?id=&token= and previews that
// invitation on top.
//
// This is an identity-owned page: the API answers 401 for an anonymous caller,
// so the page does too (never the inbox of the synthetic "anonymous"
// principal). On an unsecured instance (auth none) there is no identity to own
// an inbox, so the shared notice points at /admin/config instead of rendering one.
// 390px: rows wrap (flex-wrap), the action row wraps.
func (s *Server) publicInvitations(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	data := invitationsData{
		Note: r.URL.Query().Get("note"),
	}
	switch s.identityAccessFor(p) {
	case identitySignIn:
		s.errorPage(w, r, http.StatusUnauthorized, "sign in to view your invitations")
		return
	}
	if publicDeps.MyInvites == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "invitations unavailable")
		return
	}
	// A shared deep link previews for the signed-in subject (the token is
	// never echoed back).
	data.LinkID = r.URL.Query().Get("id")
	rows, err := publicDeps.MyInvites(r.Context(), p)
	if err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "invitations unavailable")
		return
	}
	now := s.env.now()
	for _, inv := range rows {
		data.Rows = append(data.Rows, inviteRow(inv, now))
	}
	if data.LinkID != "" && publicDeps.PreviewInvite != nil {
		pv, perr := publicDeps.PreviewInvite(r.Context(), p, data.LinkID, r.URL.Query().Get("token"))
		if perr != nil {
			data.LinkErr = friendlyInviteErr(perr)
		} else if pv != nil {
			row := inviteRowVM{
				ID: pv.ID, Kind: pv.Kind, Role: pv.Role, InvitedBy: pv.InvitedBy,
				ExpiresAt: pv.ExpiresAt, CreatedAt: pv.CreatedAt, State: pv.State,
			}
			if row.Kind == "" {
				row.Kind = inviteKindOf(pv.Repo)
			}
			row.Scope = pv.Repo
			if row.Scope == "" {
				row.Scope = pv.Org
			}
			row.Expired = inviteExpired(pv.ExpiresAt, now)
			row.ExpiresLabel = inviteExpiryLabel(pv.ExpiresAt, now)
			data.Link = &row
		}
	}
	s.page(w, r, p, http.StatusOK, "public/invitations", "Invitations", data)
}

// inviteRow maps one inbox entry to display fields (lib/invites.js rules:
// kind from the row's kind or which scope is set; scope is repo then org;
// expiry fails open on display, the server still rejects an expired accept).
func inviteRow(inv identity.InboxEntry, now time.Time) inviteRowVM {
	row := inviteRowVM{
		ID: inv.ID, Kind: inviteKindOf(inv.Repo), Role: inv.Role,
		InvitedBy: inv.InvitedBy, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
	}
	row.Scope = inv.Repo
	if row.Scope == "" {
		row.Scope = inv.Org
	}
	row.Expired = inviteExpired(inv.ExpiresAt, now)
	row.ExpiresLabel = inviteExpiryLabel(inv.ExpiresAt, now)
	if row.Scope == "" {
		row.Scope = inv.ID
	}
	return row
}

func inviteKindOf(repo string) string {
	if repo != "" {
		return "repo"
	}
	return "org"
}

func inviteExpired(stamp string, now time.Time) bool {
	if stamp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false
	}
	return !t.After(now)
}

// inviteExpiryLabel renders a live expiry as an absolute UTC date. The
// foundation rel helper tiers only past stamps (a future stamp reads "just
// now"), and an invitations list is exactly where a future date matters.
// Unparseable stamps fall back to the raw value (display fails open; accept
// still fails closed server-side).
func inviteExpiryLabel(stamp string, now time.Time) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	if !t.After(now) {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// friendlyInviteErr is the JSX `friendly` mapping: expired and non-pending
// 409s, foreign invitations, and 401s read as sentences; everything else keeps
// the service message (feature errors are user-facing by design).
//
// A pending invitation the caller is not addressed by and a spent one are
// indistinguishable without an id→scope index, so the service names both
// causes; the page keeps that sentence. A spent invitation the inbox row
// proves (issuer object gone) gets the narrower one.
func friendlyInviteErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "expired"):
		return "invitation expired"
	case strings.Contains(msg, "not addressed to your account"):
		return msg // already the accurate two-cause sentence
	case strings.Contains(msg, "no longer valid"):
		return msg // spent, or its repository was deleted — both named
	case strings.Contains(msg, "already accepted or cancelled"):
		return "invitation was already accepted or cancelled"
	case strings.Contains(msg, "not pending"):
		return "invitation no longer pending"
	case strings.Contains(msg, "not yours") || strings.Contains(msg, "forbidden"):
		return "not your invitation"
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "sign in"):
		return "sign in to view your invitations"
	}
	return msg
}

// publicInvitationAccept answers POST /invitations/{id}/accept: bind the
// invitation, then 303 back to the inbox with the outcome in ?note= (the
// redirect-after-post rule; a GET reload can never replay the write).
func (s *Server) publicInvitationAccept(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if publicDeps.AcceptInvite == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "invitations unavailable")
		return
	}
	bound, err := publicDeps.AcceptInvite(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.redirectNote(w, r, "/invitations", friendlyInviteErr(err))
		return
	}
	note := "accepted"
	if bound != "" {
		note = "accepted — bound to " + bound
	}
	s.redirectNote(w, r, "/invitations", note)
}

// publicInvitationDecline answers POST /invitations/{id}/decline.
func (s *Server) publicInvitationDecline(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if publicDeps.CancelInvite == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "invitations unavailable")
		return
	}
	if _, err := publicDeps.CancelInvite(r.Context(), p.Name, r.PathValue("id")); err != nil {
		s.redirectNote(w, r, "/invitations", friendlyInviteErr(err))
		return
	}
	s.redirectNote(w, r, "/invitations", "invitation declined")
}

// redirectNote redirects to path with the outcome in ?note= (303: the write
// is done, a reload must not replay it).
func (s *Server) redirectNote(w http.ResponseWriter, r *http.Request, path, note string) {
	w.Header().Set("Location", path+"?note="+url.QueryEscape(note))
	w.WriteHeader(http.StatusSeeOther)
}

// --- notifications -----------------------------------------------------------

// notifRowVM is one tray row. href is the thread the row opens (the JSX
// threadHref rule); Hot selects the emerald reason chip.
type notifRowVM struct {
	ID        string
	Repo      string
	Num       int
	Kind      string
	Reason    string
	Title     string
	Actor     string
	State     string
	CreatedAt string
	Href      string
	Hot       bool
}

// notificationsData is the /notifications view model.
type notificationsData struct {
	State  string
	Rows   []notifRowVM
	More   bool
	After  string
	Unread int
}

// publicNotifications answers GET /notifications (06 §7): the per-user tray,
// paged (?state=all|read|unread, ?after= cursor, 50 per page). The live SSE
// stream is an island (islands group): the page renders the contract hooks
// (data-sse*) and a placeholder region the island refreshes — no EventSource
// here.
//
// Identity-owned, like /invitations: a signed-out caller under oidc/token gets
// the API's 401 (never the synthetic "anonymous" tray), and auth none renders
// the shared notice + /admin path instead of a tray with no owner. 390px: rows
// wrap, the header controls wrap, the row body truncates instead of panning.
func (s *Server) publicNotifications(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "read" && state != "unread" {
		state = ""
	}
	data := notificationsData{State: state, After: r.URL.Query().Get("after")}
	switch s.identityAccessFor(p) {
	case identitySignIn:
		s.errorPage(w, r, http.StatusUnauthorized, "sign in to see your notifications")
		return
	}
	if publicDeps.Tray == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "notifications unavailable")
		return
	}
	rows, more := publicDeps.Tray(r.Context(), p.Name, state, data.After, notify.TrayPageSize)
	for _, n := range rows {
		data.Rows = append(data.Rows, notifRow(n))
	}
	data.More = more
	// The next-page cursor is the last row of the current page (the JSX
	// `after` semantics: newest-first, exclusive boundary).
	if more && len(data.Rows) > 0 {
		data.After = data.Rows[len(data.Rows)-1].ID
	}
	if publicDeps.UnreadCount != nil {
		data.Unread = publicDeps.UnreadCount(r.Context(), p.Name)
	}
	s.page(w, r, p, http.StatusOK, "public/notifications", "Notifications", data)
}

// notifRow maps one notification to display fields (the JSX reasonChip /
// threadHref rules).
func notifRow(n notify.Notification) notifRowVM {
	row := notifRowVM{
		ID: n.ID, Repo: n.Repo, Num: n.Num, Kind: n.Kind, Reason: n.Reason,
		Title: n.Title, Actor: n.Actor, State: n.State, CreatedAt: n.CreatedAt,
	}
	switch n.Reason {
	case "mentioned", "team_mention", "assigned", "review_requested":
		row.Hot = true
	}
	switch {
	case n.Kind == "pull" && n.Num > 0:
		row.Href = "/" + n.Repo + "/pull/" + itoa(n.Num)
	case n.Num > 0:
		row.Href = "/" + n.Repo + "/issues/" + itoa(n.Num)
	default:
		row.Href = "/" + n.Repo
	}
	return row
}

// publicNotificationsReadAll answers POST /notifications/read-all.
func (s *Server) publicNotificationsReadAll(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if publicDeps.ReadAll == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "notifications unavailable")
		return
	}
	_ = publicDeps.ReadAll(r.Context(), p.Name)
	loc := "/notifications"
	if st := r.URL.Query().Get("state"); st == "read" || st == "unread" {
		loc += "?state=" + url.QueryEscape(st)
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusSeeOther)
}

// publicNotificationState answers POST /notifications/{id}/read and …/unread
// (the path decides the direction — the islands.md "POST form" contract).
func (s *Server) publicNotificationState(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if publicDeps.SetNotificationState == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "notifications unavailable")
		return
	}
	read := strings.HasSuffix(r.URL.Path, "/read")
	_, _ = publicDeps.SetNotificationState(r.Context(), p.Name, r.PathValue("id"), read)
	// Back to the shelf the caller came from (the state filter is a query
	// string, so it must survive the write).
	loc := "/notifications"
	if st := r.URL.Query().Get("state"); st == "read" || st == "unread" {
		loc += "?state=" + url.QueryEscape(st)
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusSeeOther)
}

// --- write plumbing ----------------------------------------------------------

// writePrincipal resolves an authenticated principal for a mutation: 401 for
// anonymous callers (the interstitial is a page, not a write target), 403 for
// an instance with authentication disabled (auth none — no identity exists to
// own the write, so the page-level explanation, not a credential request, is
// the honest answer), and 403 for a cross-origin POST (the same-origin rule
// every UI mutation carries). A request with neither Origin nor Referer is
// refused: only a browser attaches them, and a cookie-bearing non-browser
// client is already a deliberate actor.
func (s *Server) writePrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	switch s.identityAccessFor(p) {
	case identitySignIn:
		s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
		return p, false
	}
	if !publicSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request refused")
		return p, false
	}
	return p, true
}

// publicSameOrigin enforces the browser mutation rule (PLAN §Writes): an
// Origin or Referer, when present, must name this host; a post that carries
// neither is refused (fail closed). Named per group so sibling group files can
// carry their own copy until the foundation promotes one helper; the policy is
// identical (collab.go sameOrigin).
func publicSameOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// --- API docs content --------------------------------------------------------

// apiRouteTable is the static §8 route table on /api. It is DERIVED, not
// independent (web/src/pages/Apidocs.jsx): every row's path shape matches one
// discovery endpoints[] template, and each feature package pins that
// correspondence both ways in TestExposedCoversRoutes. Both lanes (…/api/…
// and …/api-browser/…) serve every row; the table shows the /api spelling.
func apiRouteTable() []apiRouteRow {
	rows := [][3]string{
		{"GET", "/api/v1", "discovery document (endpoints, capabilities)"},
		{"GET", "/api/v1/owners", "owners on this host, sorted, from the store"},
		{"GET", "/api/v1/owners/{o}/repos", "short repo names of one owner"},
		{"POST", "/api/v1/repos", "explicit create → placeholder (write; 201 / 200 already:true / 409 with winner URL)"},
		{"POST", "/api/v1/repos/imports", "start (or join) a URL import → 202 {task, target} (write; idempotent no-op 200)"},
		{"GET", "/api/v1/repos/imports/{id}", "import status (read)"},
		{"POST", "/api/v1/repos/mirrors", "create a repo from a URL as a pull mirror (admin)"},
		{"GET|PUT|DELETE", "/{o}/{r}/api/mirror", "mirror sidecar: open read · replace/delete (admin)"},
		{"GET", "/{o}/{r}/api/mirror/sync", "last sync status (open read, like GET mirror)"},
		{"POST", "/{o}/{r}/api/mirror/sync", "sync now (admin)"},
		{"GET|PUT", "/api/v1/users/{principal}", "profile (read; self or admin to edit)"},
		{"GET", "/api/v1/users/{principal}/orgs", "orgs of a principal, sorted names, [] when none (read)"},
		{"GET|POST", "/api/v1/orgs", "orgs (read; write to create)"},
		{"GET|PUT|DELETE", "/api/v1/orgs/{org}", "org (read; owner to change; DELETE 204)"},
		{"GET", "/api/v1/orgs/{org}/members", "members (read)"},
		{"GET|PUT|DELETE", "/api/v1/orgs/{org}/members/{principal}", "membership (read; owner to change)"},
		{"GET|POST", "/api/v1/orgs/{org}/teams", "teams (read; owner to create)"},
		{"GET|PUT|DELETE", "/api/v1/orgs/{org}/teams/{slug}", "team (read; owner to change; DELETE 204)"},
		{"PUT|DELETE", "/api/v1/orgs/{org}/teams/{slug}/members/{principal}", "team membership (owner)"},
		{"GET|POST", "/api/v1/orgs/{org}/invitations", "org invites (owner)"},
		{"DELETE", "/api/v1/orgs/{org}/invitations/{id}", "cancel org invite (owner; 204)"},
		{"GET", "/api/v1/invitations", "my invitations (authenticated)"},
		{"GET|DELETE", "/api/v1/invitations/{id}", "preview (?token=) · decline (authenticated; DELETE 204)"},
		{"POST", "/api/v1/invitations/{id}/accept", "accept → {bound} (authenticated)"},
		{"GET|PUT", "/{o}/{r}/api/access", "access doc (triage read; admin replace with version CAS)"},
		{"GET", "/{o}/{r}/api/permissions", "my resolved role {role} (read)"},
		{"GET", "/{o}/{r}/api/collaborators", "effective bindings + source (read)"},
		{"GET", "/{o}/{r}/api/assignables", "mentions autocomplete source (read)"},
		{"GET|POST", "/{o}/{r}/api/invitations", "repo invites (admin)"},
		{"DELETE", "/{o}/{r}/api/invitations/{id}", "cancel repo invite (admin; 204)"},
		{"GET|POST", "/{o}/{r}/api/issues", "issues: list (?state=&labels=&assignee=&milestone=&since=&after=&n=, read) · create (authenticated + read)"},
		{"GET|PATCH", "/{o}/{r}/api/issues/{num}", "issue thread (no-cache + ETag) · patch (author or triage)"},
		{"GET", "/{o}/{r}/api/issues/{num}/events", "event tail (no-store)"},
		{"POST", "/{o}/{r}/api/issues/{num}/comments", "comment (authenticated + read)"},
		{"POST", "/{o}/{r}/api/issues/{num}/reactions", "react {target_event_seq, content} (authenticated + read)"},
		{"DELETE", "/{o}/{r}/api/issues/{num}/reactions/{seq}/{content}", "unreact (204)"},
		{"GET|POST", "/{o}/{r}/api/labels", "labels (read) · create (triage)"},
		{"PATCH|DELETE", "/{o}/{r}/api/labels/{name}", "update · delete → {threads_affected}"},
		{"GET|POST", "/{o}/{r}/api/milestones", "milestones (?state=, read) · create (triage)"},
		{"GET|PATCH|DELETE", "/{o}/{r}/api/milestones/{id}", "milestone, 6-hex id (read · triage · triage)"},
		{"POST", "/{o}/{r}/api/attachments", "upload attachment (authenticated + read; multipart)"},
		{"GET|HEAD", "/{o}/{r}/attachments/{sha}/{name}", "attachment bytes (static contract: ETag, ranges)"},
		{"GET|POST", "/{o}/{r}/api/pulls", "PRs: list (?state=, read) · open (write)"},
		{"GET|PUT", "/{o}/{r}/api/pulls/{num}", "PR detail (read) · update (author or triage)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/diff", "unified diff (read)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/commits", "PR commits (read)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/comments", "timeline comment (authenticated + read)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/merge", "merge (maintain; 202 task — poll …/merge/task)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/merge/task", "merge task record (read; SSE attach like tasks)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/update-branch", "fast-forward head onto base (write; 202 task)"},
		{"DELETE", "/{o}/{r}/api/pulls/{num}/head", "delete the merged head branch (maintain)"},
		{"POST", "/api/v1/repos/{o}/{r}/forks", "fork {target_owner?, name?, visibility?, branch?, description?} (write on parent + target admission; 202 task, 409 when taken)"},
		{"GET", "/api/v1/repos/{o}/{r}/forks", "fork list (?n=&after=, read; mutable no-cache + version ETag)"},
		{"GET|POST", "/{o}/{r}/api/pulls/{num}/reviews", "reviews (read) · submit {state, body, commit_sha?, threads?} (authenticated + read)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/reviews/{seq}", "one review (read)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/reviews/{seq}/dismiss", "dismiss (maintain)"},
		{"GET|POST", "/{o}/{r}/api/pulls/{num}/threads", "threads (?resolved=, read) · open {anchor, body} (authenticated + read)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/threads/{id}", "thread + comments (read)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/comments", "reply (authenticated + read)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/resolve", "resolve (author or triage)"},
		{"POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/unresolve", "unresolve (author or triage)"},
		{"GET|POST|DELETE", "/{o}/{r}/api/pulls/{num}/review-requests", "requested reviewers (read; triage to change)"},
		{"GET", "/{o}/{r}/api/pulls/{num}/review-suggest", "reviewer suggestions (?q=, read)"},
		{"GET", "/{o}/{r}/api/releases", "releases (?n=&after=, mutable no-cache + ETag)"},
		{"GET", "/{o}/{r}/api/releases/latest", "latest pointer (?include_prereleases=1)"},
		{"GET", "/{o}/{r}/api/releases/autodraft?tag=", "changelog draft from merged PRs (tag required)"},
		{"GET|PUT|DELETE", "/{o}/{r}/api/releases/{tag}", "release (read) · upsert {name, body, draft?, prerelease?} (write; If-Match) · delete (maintain)"},
		{"POST|DELETE", "/{o}/{r}/api/releases/{tag}/assets/{name}", "upload (write; X-Walgit-Asset-Sha256 required — § Releases) · delete (write)"},
		{"GET|HEAD", "/{o}/{r}/releases/{tag}/assets/{name}", "asset bytes (immutable, ETag, ranges)"},
		{"POST", "/{o}/{r}/api/tags", "create lightweight/annotated tag at a commit (write; 201, 409 when present)"},
		{"PUT|DELETE", "/{o}/{r}/api/star", "star / unstar (authenticated; unstar always works)"},
		{"GET", "/{o}/{r}/api/social", "counts + viewer flags (mutable no-cache + ETag)"},
		{"GET", "/api/v1/me/starred", "my stars (authenticated, no-store)"},
		{"GET", "/api/v1/users/{principal}/starred", "their stars (read)"},
		{"GET|PUT|DELETE", "/{o}/{r}/api/watch", "watch state: get (authenticated) · set/clear (authenticated + read)"},
		{"GET", "/api/v1/notifications", "tray (?state=&after=&n=, authenticated, no-store)"},
		{"GET", "/api/v1/notifications/unread_count", "O(1) unread count (authenticated)"},
		{"POST", "/api/v1/notifications/read_all", "mark the index window read → {updated} (authenticated)"},
		{"GET", "/api/v1/notifications/stream", "per-user SSE notification frames (authenticated; § Streams)"},
		{"POST", "/api/v1/notifications/{id}/read", "mark read (foreign ids 404, authenticated)"},
		{"POST", "/api/v1/notifications/{id}/unread", "mark unread (authenticated)"},
		{"GET|POST", "/{o}/{r}/api/webhooks", "webhooks without secrets (admin) · create (admin, 201)"},
		{"GET|PATCH|DELETE", "/{o}/{r}/api/webhooks/{id}", "webhook (admin; DELETE 204)"},
		{"POST", "/{o}/{r}/api/webhooks/{id}/ping", "test delivery → {delivery} (admin)"},
		{"GET", "/{o}/{r}/api/webhooks/{id}/deliveries", "recent deliveries (admin, no-store)"},
		{"GET", "/{o}/{r}/api/collab/stream", "repo activity SSE (read-gated; § Streams)"},
		{"GET|PUT|DELETE", "/{o}/{r}/api", "repo summary (mutable-collab no-cache + ETag, +placeholder projection when empty; open_issues/open_pulls tab-badge counts; features flag object) · create (write; ?placeholder=true) · delete (admin)"},
		{"GET", "/{o}/{r}/api/refs", "O(1) default-branch head"},
		{"GET", "/{o}/{r}/api/refs/{branches|tags}", "paged ref list (prefix/q/after/n) — SSE dialect available"},
		{"GET", "/{o}/{r}/api/resolve[/{rest}]", "ref/path split → {ref, sha, path, kind} (SWR)"},
		{"GET", "/{o}/{r}/api/tree/{rev}[/{path}]", "tree listing (immutable at full sha)"},
		{"GET", "/{o}/{r}/api/blob/{rev}/{path}", "blob (2 MiB cap → too_large; NUL/invalid UTF-8 → binary)"},
		{"GET", "/{o}/{r}/api/commits", "history: ?ref=&path=&skip=&n= (immutable at sha refs)"},
		{"GET", "/{o}/{r}/api/commit/{sha}", "commit detail: {commit, stats[], patch}"},
		{"GET|PUT|DELETE", "/{o}/{r}/api/policy", "push policy (admin; 400 with reasons, fail closed)"},
		{"POST", "/{o}/{r}/api/policy/validate", "validate a policy payload → {ok, errors[]}"},
		{"POST", "/{o}/{r}/api/policy/dry-run?last=N", "replay the last N pushes under the policy"},
		{"GET|PUT|DELETE", "/{o}/{r}/api/settings", "per-repo settings TOML (≤ 16 KiB; 4 allowed sections)"},
		{"GET", "/{o}/{r}/api/settings/effective", "effective [bundles]/[maintenance]/[compaction]/[upstream] as TOML"},
		{"GET", "/{o}/{r}/api/settings/history", "per-revision settings history"},
		{"GET", "/{o}/{r}/api/settings/describe", "strategies, host facts, upstream follow, effective fields"},
		{"GET", "/{o}/{r}/api/overview", "WAL health: manifest, local copy, packs, bundles, plan, compactions"},
		{"GET", "/{o}/{r}/api/ops", "available ops + recent tasks + bundle strategies"},
		{"POST", "/{o}/{r}/api/ops/{op}", "run an op (SSE attach; tasks)"},
		{"GET", "/{o}/{r}/api/tasks", "running + recent tasks (no-store)"},
		{"GET", "/{o}/{r}/api/tasks/{id}", "task record JSON, or SSE attach with Accept: text/event-stream"},
		{"GET", "/{o}/{r}/api/checks?after=&n=", "paged checks index (read; no-store; n default 50, max 200)"},
		{"GET", "/{o}/{r}/api/checks/{sha}", "combined worst-of state + per-state counts (read; no-store)"},
		{"GET", "/{o}/{r}/api/checks/statuses/{sha}", "per-context statuses for one sha (read; no-store)"},
		{"POST", "/{o}/{r}/api/checks/statuses/{sha}", "report one context result (repo write, or wct_ CI token with checks:write — § Checks CI below)"},
		{"GET|POST", "/{o}/{r}/api/checks/tokens", "CI tokens (admin): list without secrets · mint → 201 {id, token, scopes}, secret shown once"},
		{"DELETE", "/{o}/{r}/api/checks/tokens/{id}", "revoke a CI token (admin; 204, idempotent)"},
		{"GET", "/{o}/{r}/info/refs?service=…", "git smart HTTP v0/v2 advertisement"},
		{"POST", "/{o}/{r}/git-upload-pack", "fetch"},
		{"POST", "/{o}/{r}/git-receive-pack", "push"},
	}
	out := make([]apiRouteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, apiRouteRow{Method: r[0], Path: r[1], What: r[2]})
	}
	return out
}

// The snippets below are page content, verbatim from web/src/pages/Apidocs.jsx
// (the worked CI / asset / notification flows and the SDK import). They are
// plain text rendered inside <pre>: html/template escapes them.
const sdkSnippet = `<script type="module">
import repos from "https://<host>/repos.js";
repos.configure({ token: "…" }).repo("demo/hello").tree("main", "").then(console.log);
</script>`

const ciSnippet = `# 1. mint a CI token (repo admin; secret shown once)
curl -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"ci","scopes":["checks:write"]}' \
  https://<host>/demo/hello/api/checks/tokens
# -> {"id":"abcd1234","token":"wct_abcd1234.<secret>","scopes":["checks:write"]}
# 2. post a result from CI
curl -H "Authorization: Bearer wct_abcd1234.<secret>" -H "Content-Type: application/json" \
  -d '{"context":"ci/build","state":"success","target_url":"https://ci.example/r/1","description":"build #1"}' \
  https://<host>/demo/hello/api/checks/statuses/<sha>
# -> 200 {"sha":"…","context":"ci/build","state":"success",…}
# 3. read the combined view (read access; anonymous on public repos)
curl https://<host>/demo/hello/api/checks/<sha>
# -> {"sha":"…","state":"success","total_counts":{…},"statuses":[…]}`

const releaseSnippet = `# 1. upsert the header (repo write; If-Match optional for CAS)
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"v1.2","body":"notes","draft":false}' -X PUT \
  https://<host>/demo/hello/api/releases/v1.2
# 2. upload bytes (checksum header required; 201 with browser_download_url)
SHA=$(sha256sum dist.zip | cut -d' ' -f1)
curl -H "Authorization: Bearer $TOKEN" -H "X-Walgit-Asset-Sha256: $SHA" \
  --data-binary @dist.zip \
  https://<host>/demo/hello/api/releases/v1.2/assets/dist.zip
# 3. bytes download immutable; JSON revalidates every read
curl https://<host>/demo/hello/releases/v1.2/assets/dist.zip`

const notifySnippet = `# 1. tray (authenticated; newest-first, ?state=unread&n=50)
curl -H "Authorization: Bearer $TOKEN" https://<host>/api/v1/notifications
# -> {"notifications":[…],"more":false}
# 2. hold the per-user stream (named SSE frames until disconnect)
curl -H "Authorization: Bearer $TOKEN" -H "Accept: text/event-stream" \
  https://<host>/api/v1/notifications/stream
# 3. mark one entry read (foreign ids are 404, never 403)
curl -H "Authorization: Bearer $TOKEN" -X POST \
  https://<host>/api/v1/notifications/<id>/read`
