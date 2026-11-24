// admin_directory.go — /admin/directory, the instance users and organizations
// directory and its one mutation (docs/PLAN-ADMIN.md §3.7, the §8.6 follow-up
// card that needed a law-8 schema decision).
//
//	GET  /admin/directory                    users (default tab) | ?tab=orgs
//	POST /admin/directory/deactivate         typed confirm + reason → disabled
//	POST /admin/directory/reactivate         clears the flag (tokens stay dead)
//
// # Why this page exists
//
// Before it, identity administration WAS the Access page: static tokens and
// the OIDC admin mapping. There was no instance-wide directory seam at all —
// an administrator could not see who has an account, let alone stop one
// signing in. Deactivation is a credential fact, so the write lands on
// users/<username>/user.json and is enforced at the credential boundary
// (internal/server/auth.go accountGate), never page by page.
//
// # Bounds
//
// Both lists are server-rendered, bounded and cursor-paged: the users list
// walks account names in name order with a delimiter LIST until the page
// fills; a search examines at most identity.DirectoryScanBound accounts and
// says so when it stopped. One organization walk per page annotates the rows'
// memberships and ownership (the "a deactivated account still owns these
// organizations" fact), bounded by identity.DirectoryOrgScanBound.
//
// # Privacy
//
// Host-admin only. The page never renders an email address (the profile page
// owns the self-only email rule), a token, or any store key. A deactivated
// row carries its state as TEXT, never colour alone.

package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// adminDirectoryOrgRole is one annotated membership on a user row.
type adminDirectoryOrgRole struct {
	Org   string
	Owner bool
}

// adminDirectoryUserRow is one users-directory row.
type adminDirectoryUserRow struct {
	Username    string
	DisplayName string
	// Bound is false when the principal has only a profile (no sign-in
	// account): the row then says so and offers no control.
	Bound      bool
	Disabled   bool
	DisabledBy string
	DisabledAt string
	Reason     string
	Roles      []adminDirectoryOrgRole
}

// adminDirectoryOrgRow is one organizations-directory row.
type adminDirectoryOrgRow struct {
	Name        string
	DisplayName string
	Members     int
}

// ChipClass / ChipLabel render the account state as a chip WITH its text (a
// state is never colour-only).
func (r adminDirectoryUserRow) ChipClass() string {
	switch {
	case !r.Bound:
		return "chip chip-neutral"
	case r.Disabled:
		return "chip-closed"
	default:
		return "chip-open"
	}
}

func (r adminDirectoryUserRow) ChipLabel() string {
	switch {
	case !r.Bound:
		return "no sign-in account"
	case r.Disabled:
		return "deactivated"
	default:
		return "active"
	}
}

// AdminDirectoryView is /admin/directory.
type AdminDirectoryView struct {
	Tab   string // "users" | "orgs"
	Query string
	After string

	Users        []adminDirectoryUserRow
	UsersNext    string
	Examined     int
	ScanBound    int
	Truncated    bool
	RolesScanned int
	Orgs         []adminDirectoryOrgRow
	OrgsNext     string

	// RowError names the row whose action was refused (the inline error is
	// re-rendered beside it); the banner carries the sentence.
	RowError string
	// Notice is the result of the action that redirected here.
	Notice string
	// Wired is false when the identity directory seam is not bound.
	Wired bool
	// Error is a read failure (store-level), rendered as an alert.
	Error string
}

// adminDirectoryTab normalizes ?tab=.
func adminDirectoryTab(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "orgs") {
		return "orgs"
	}
	return "users"
}

// adminDirectoryURL builds a tab link that keeps the current search/cursor.
func adminDirectoryURL(tab, query, after string) string {
	v := url.Values{}
	if tab == "orgs" {
		v.Set("tab", "orgs")
	}
	if query != "" {
		v.Set("q", query)
	}
	if after != "" {
		v.Set("after", after)
	}
	if len(v) == 0 {
		return "/admin/directory"
	}
	return "/admin/directory?" + v.Encode()
}

// UsersTabURL / OrgsTabURL are the tab links (a tab switch starts at the
// first page of that list: a cursor is a position in one list, not the other).
func (v *AdminDirectoryView) UsersTabURL() string { return adminDirectoryURL("users", v.Query, "") }
func (v *AdminDirectoryView) OrgsTabURL() string  { return adminDirectoryURL("orgs", "", "") }

// FirstURL is the "first page" link for the active tab.
func (v *AdminDirectoryView) FirstURL() string {
	if v.Tab == "orgs" {
		return adminDirectoryURL("orgs", "", "")
	}
	return adminDirectoryURL("users", v.Query, "")
}

// NextURL is the following-page link for the active tab ("" adds nothing).
func (v *AdminDirectoryView) NextURL() string {
	if v.Tab == "orgs" {
		return adminDirectoryURL("orgs", "", v.OrgsNext)
	}
	return adminDirectoryURL("users", v.Query, v.UsersNext)
}

// HasNext reports whether the active tab has a following page.
func (v *AdminDirectoryView) HasNext() bool {
	if v.Tab == "orgs" {
		return v.OrgsNext != ""
	}
	return v.UsersNext != ""
}

// SearchURL is the GET action of the users search form.
func (v *AdminDirectoryView) SearchURL() string { return "/admin/directory?tab=users" }

// adminDirectoryPage answers GET /admin/directory.
func (s *Server) adminDirectoryPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "directory")
	if !ok {
		return
	}
	s.adminShell(p, page, "directory")
	q := r.URL.Query()
	view := &AdminDirectoryView{
		Tab:   adminDirectoryTab(q.Get("tab")),
		Query: strings.TrimSpace(q.Get("q")),
		After: strings.TrimSpace(q.Get("after")),
	}
	s.adminDirectoryFill(r, view)
	// The action notices ride the query, never a session or a cookie.
	switch q.Get("result") {
	case "deactivated":
		view.Notice = "Account deactivated. Sign-in is refused from now, and every session and token minted before this moment is revoked permanently."
	case "reactivated":
		view.Notice = "Account reactivated. Sign-in works again; credentials revoked by the deactivation are not restored, so the account must sign in again."
	}
	page.Directory = view
	s.adminRender(w, r, p, http.StatusOK, page)
}

// adminDirectoryFill loads the active tab's bounded page (and, for users, the
// one membership walk that annotates every row).
func (s *Server) adminDirectoryFill(r *http.Request, view *AdminDirectoryView) {
	d := adminDeps()
	ctx := r.Context()
	limit := identity.DirectoryPageDefault
	if view.Tab == "orgs" {
		if d.DirectoryOrgs == nil {
			view.Error = "the identity directory is not wired on this build"
			return
		}
		view.Wired = true
		res, err := d.DirectoryOrgs(ctx, identity.DirectoryQuery{After: view.After, Limit: limit})
		if err != nil {
			view.Error = err.Error()
			return
		}
		for _, row := range res.Rows {
			view.Orgs = append(view.Orgs, adminDirectoryOrgRow{Name: row.Name, DisplayName: row.DisplayName, Members: row.Members})
		}
		view.OrgsNext = res.Next
		return
	}
	if d.DirectoryUsers == nil {
		view.Error = "the identity directory is not wired on this build"
		return
	}
	view.Wired = true
	res, err := d.DirectoryUsers(ctx, identity.DirectoryQuery{Query: view.Query, After: view.After, Limit: limit})
	if err != nil {
		view.Error = err.Error()
		return
	}
	view.UsersNext = res.Next
	view.Examined = res.Examined
	view.ScanBound = res.Bound
	view.Truncated = res.Truncated
	names := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		view.Users = append(view.Users, adminDirectoryUserRow{
			Username: row.Username, DisplayName: row.DisplayName, Bound: row.Bound,
			Disabled: row.Disabled, DisabledBy: row.DisabledBy, DisabledAt: row.DisabledAt,
			Reason: row.Reason,
		})
		names = append(names, row.Username)
	}
	// One organization walk for the whole page (never one per row): it is how
	// a row says "member of …/owner of …", and how a deactivated owner stays
	// visible after the action.
	if d.UserOrgRoles == nil || len(names) == 0 {
		return
	}
	roles, truncated, err := d.UserOrgRoles(ctx, names)
	if err != nil {
		view.Error = err.Error()
		return
	}
	view.RolesScanned = len(names)
	if truncated {
		view.Truncated = true
	}
	for i := range view.Users {
		for _, role := range roles[view.Users[i].Username] {
			view.Users[i].Roles = append(view.Users[i].Roles, adminDirectoryOrgRole{Org: role.Org, Owner: role.Owner})
		}
	}
}

// adminDirectoryActionError re-renders the directory with a named inline error
// (the row keeps its form, so a keyboard user lands on the cause).
func (s *Server) adminDirectoryActionError(w http.ResponseWriter, r *http.Request, p auth.Principal, view *AdminDirectoryView, username, msg string, status int) {
	page := &AdminPage{}
	s.adminShell(p, page, "directory")
	view.RowError = strings.ToLower(strings.TrimSpace(username))
	view.Error = msg
	s.adminDirectoryFill(r, view)
	page.Directory = view
	s.adminRender(w, r, p, status, page)
}

// adminDirectoryDeactivate answers POST /admin/directory/deactivate. The
// typed confirmation and the reason are enforced HERE as well as in the island
// (a missing or wrong value is a 422 with the named inline error, never a
// silent action); the write is identity's CAS'd transition, which audits it.
func (s *Server) adminDirectoryDeactivate(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.requireAdminPage(w, r, "directory")
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "a cross-origin form submission is refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	view := &AdminDirectoryView{Tab: "users", Query: username}
	switch {
	case username == "":
		s.adminDirectoryActionError(w, r, p, view, "", "a username is required", http.StatusUnprocessableEntity)
		return
	case p.Name != "" && strings.EqualFold(username, p.Name):
		s.adminDirectoryActionError(w, r, p, view, username,
			"you cannot deactivate your own account — another host administrator must do it", http.StatusUnprocessableEntity)
		return
	case confirm != username:
		s.adminDirectoryActionError(w, r, p, view, username,
			"type the username exactly to confirm the deactivation", http.StatusUnprocessableEntity)
		return
	case reason == "":
		s.adminDirectoryActionError(w, r, p, view, username,
			"a reason is required — it is recorded in the audit log", http.StatusUnprocessableEntity)
		return
	}
	d := adminDeps()
	if d.SetUserDisabled == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "the identity directory is not wired on this build")
		return
	}
	ctx := identity.WithActor(r.Context(), p.Name)
	if err := d.SetUserDisabled(ctx, username, true, reason); err != nil {
		status := http.StatusUnprocessableEntity
		switch {
		case errors.Is(err, identity.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, identity.ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, identity.ErrUnavailable):
			status = http.StatusServiceUnavailable
		}
		s.adminDirectoryActionError(w, r, p, view, username, err.Error(), status)
		return
	}
	redirectPage(w, r, "/admin/directory?q="+url.QueryEscape(username)+"&result=deactivated")
}

// adminDirectoryReactivate answers POST /admin/directory/reactivate. It is not
// destructive (the revoked credentials stay dead), so no typed confirmation —
// same-origin still applies.
func (s *Server) adminDirectoryReactivate(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.requireAdminPage(w, r, "directory")
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "a cross-origin form submission is refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	if username == "" {
		s.adminDirectoryActionError(w, r, p, &AdminDirectoryView{Tab: "users"}, "", "a username is required", http.StatusUnprocessableEntity)
		return
	}
	d := adminDeps()
	if d.SetUserDisabled == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "the identity directory is not wired on this build")
		return
	}
	ctx := identity.WithActor(r.Context(), p.Name)
	if err := d.SetUserDisabled(ctx, username, false, ""); err != nil {
		status := http.StatusUnprocessableEntity
		switch {
		case errors.Is(err, identity.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, identity.ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, identity.ErrUnavailable):
			status = http.StatusServiceUnavailable
		}
		s.adminDirectoryActionError(w, r, p, &AdminDirectoryView{Tab: "users", Query: username}, username, err.Error(), status)
		return
	}
	redirectPage(w, r, "/admin/directory?q="+url.QueryEscape(username)+"&result=reactivated")
}

// DirectorySeam is the identity service's directory surface the page binds
// (satisfied by *identity.Service). It is a named type so composition's wiring
// guard can assert the whole seam at once.
type DirectorySeam struct {
	Users    func(ctx context.Context, q identity.DirectoryQuery) (*identity.DirectoryUserPage, error)
	Orgs     func(ctx context.Context, q identity.DirectoryQuery) (*identity.DirectoryOrgPage, error)
	OrgRoles func(ctx context.Context, names []string) (map[string][]identity.OrgMembership, bool, error)
	SetState func(ctx context.Context, username string, disabled bool, reason string) error
}

// BindDirectory fills the Administration area's directory seam from an
// identity service (nil binds nothing: the page then renders its honest
// not-wired card). Composition calls it beside BindAdmin.
func BindDirectory(svc DirectorySeam) {
	d := adminBound
	d.DirectoryUsers = svc.Users
	d.DirectoryOrgs = svc.Orgs
	d.UserOrgRoles = svc.OrgRoles
	d.SetUserDisabled = svc.SetState
	adminBound = d
}
