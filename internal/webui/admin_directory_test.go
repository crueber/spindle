// admin_directory_test.go — /admin/directory (docs/PLAN-ADMIN.md §3.7): the
// host-admin gate, the two bounded lists and their cursors, the search, the
// typed deactivation confirmation with its reason, the reactivation, the
// ownership annotation and the empty/not-wired states.

package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// directoryCall records one SetUserDisabled invocation.
type directoryCall struct {
	username string
	disabled bool
	reason   string
}

// adminDirectoryDeps is the directory's fake seam: two accounts (one bound and
// active, one bound and deactivated) plus one profile-only principal.
func adminDirectoryDeps(calls *[]directoryCall) testDeps {
	deps := adminBaseDeps()
	deps.DirectoryUsers = func(_ context.Context, q identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		rows := []identity.DirectoryUser{
			{Username: "alice", DisplayName: "Alice", Bound: true},
			{Username: "bob", DisplayName: "Bob", Bound: true, Disabled: true,
				DisabledBy: "root", DisabledAt: "2026-11-20T10:00:00Z", Reason: "spam account"},
			{Username: "ghost", DisplayName: "", Bound: false},
		}
		var out []identity.DirectoryUser
		for _, r := range rows {
			if q.After != "" && r.Username <= q.After {
				continue
			}
			if q.Query != "" && !strings.Contains(r.Username, q.Query) && !strings.Contains(strings.ToLower(r.DisplayName), q.Query) {
				continue
			}
			out = append(out, r)
		}
		page := &identity.DirectoryUserPage{Rows: out, Examined: len(rows)}
		if q.Query != "" {
			page.Bound = identity.DirectoryScanBound
			page.Truncated = true
		}
		return page, nil
	}
	deps.DirectoryOrgs = func(_ context.Context, q identity.DirectoryQuery) (*identity.DirectoryOrgPage, error) {
		return &identity.DirectoryOrgPage{
			Rows: []identity.DirectoryOrg{
				{Name: "acme", DisplayName: "Acme Team", Members: 3},
				{Name: "beta", DisplayName: "", Members: 1},
			},
			Next:    "beta",
			Scanned: 2,
		}, nil
	}
	deps.UserOrgRoles = func(_ context.Context, names []string) (map[string][]identity.OrgMembership, bool, error) {
		return map[string][]identity.OrgMembership{
			"alice": {{Org: "acme", Owner: true}, {Org: "beta"}},
			"bob":   {{Org: "acme"}},
		}, false, nil
	}
	deps.SetUserDisabled = func(_ context.Context, username string, disabled bool, reason string) error {
		if calls != nil {
			*calls = append(*calls, directoryCall{username, disabled, reason})
		}
		return nil
	}
	return deps
}

func TestAdminDirectoryGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		want   int
		denied bool
	}{
		{"anonymous", auth.Anonymous(), http.StatusUnauthorized, true},
		{"signed-in non-admin", auth.Principal{Name: "bob", Write: true}, http.StatusForbidden, true},
		{"host admin", settingsTestPrincipal(), http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(nil), principal: tc.p})
			rec := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil)
			if rec.Code != tc.want {
				t.Fatalf("GET /admin/directory = %d, want %d", rec.Code, tc.want)
			}
			body := rec.Body.String()
			if tc.denied {
				settingsMustContain(t, body, "Access restricted", "denied view")
				settingsMustNotContain(t, body, "deactivate…", "denied view offers no control")
				settingsMustNotContain(t, body, "Alice", "denied view leaks no account")
			} else if n := a11yH1Count(body); n != 1 {
				t.Fatalf("h1 count = %d, want 1", n)
			}
		})
	}
	// The POST pair shares the one gate.
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(nil), principal: auth.Anonymous()})
	for _, path := range []string{"/admin/directory/deactivate", "/admin/directory/reactivate"} {
		if rec := settingsDo(t, s, http.MethodPost, path, url.Values{"username": {"alice"}}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401", path, rec.Code)
		}
	}
}

func TestAdminDirectoryUsersList(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(nil), principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		// the one h1 and the honest nav label
		"<h1", "Users and organizations", `href="/admin/directory"`,
		// rows: username, display name, state text, link to the profile
		`href="/alice"`, "Alice", `href="/bob"`, "Bob",
		"active", "deactivated", "spam account", "root",
		// state is text, not colour only
		"chip-open", "chip-closed",
		// membership annotation (one organization walk per page)
		`href="/acme"`, "(owner)", `href="/beta"`,
		// the typed confirmation and its reason
		`data-danger-confirm="alice"`, `name="reason"`, "deactivate account",
		// the profile-only row offers no control
		"no sign-in account to deactivate",
		// search form
		`action="/admin/directory?tab=users"`, `name="q"`,
		// tabs
		`aria-current="true">Users</a>`,
	} {
		settingsMustContain(t, body, want, "users directory")
	}
	// One primary action on the page (the search).
	if got := strings.Count(body, "btn primary"); got != 1 {
		t.Fatalf("primary buttons = %d, want 1\n%s", got, settingsTruncate(body, 3000))
	}
	// No email address is rendered anywhere.
	settingsMustNotContain(t, body, "@example.com", "directory must not leak emails")
}

func TestAdminDirectorySearchAndBound(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(nil), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/directory?q=bob", nil, nil).Body.String()
	settingsMustContain(t, body, "bob", "matching row")
	settingsMustContain(t, body, `chip-draft">1<`, "one row matched, the rest filtered")
	settingsMustContain(t, body, "Examined 3 accounts", "the bound is stated")
	settingsMustContain(t, body, "bound 120 per search", "named bound")
	settingsMustContain(t, body, "More accounts may match", "truncation is honest")
	settingsMustContain(t, body, "clear", "the search can be cleared")
	// A miss renders the empty state, not an error.
	body = settingsDo(t, s, http.MethodGet, "/admin/directory?q=zzz", nil, nil).Body.String()
	settingsMustContain(t, body, "No account matches that search", "search miss empty state")
}

func TestAdminDirectoryPagingCursor(t *testing.T) {
	deps := adminDirectoryDeps(nil)
	deps.DirectoryUsers = func(_ context.Context, q identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		page := &identity.DirectoryUserPage{Examined: 1}
		switch {
		case q.After == "":
			page.Rows = []identity.DirectoryUser{{Username: "alice", Bound: true}}
			page.Next = "alice"
		case q.After == "alice":
			page.Rows = []identity.DirectoryUser{{Username: "bob", Bound: true}}
		}
		return page, nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/directory?q=a", nil, nil).Body.String()
	settingsMustContain(t, body, "/admin/directory?after=alice", "next-page link carries the cursor and the query")
	settingsMustContain(t, body, "next page", "next label")
	settingsMustContain(t, body, "first page", "first-page link")
	body = settingsDo(t, s, http.MethodGet, "/admin/directory?q=a&after=alice", nil, nil).Body.String()
	settingsMustContain(t, body, `href="/bob"`, "second page row")
	// A cursor that runs past the end says so.
	body = settingsDo(t, s, http.MethodGet, "/admin/directory?after=zzz", nil, nil).Body.String()
	settingsMustContain(t, body, "End of the accounts this search examined", "end label")
}

func TestAdminDirectoryOrgsTab(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(nil), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/directory?tab=orgs", nil, nil).Body.String()
	for _, want := range []string{
		`aria-current="true">Organizations</a>`,
		`href="/acme"`, "Acme Team", "members", `<td class="tabular">3</td>`,
		`href="/beta"`, "next page",
	} {
		settingsMustContain(t, body, want, "organizations tab")
	}
	if n := a11yH1Count(body); n != 1 {
		t.Fatalf("orgs tab h1 = %d, want 1", n)
	}
	// The orgs tab has no deactivate control.
	settingsMustNotContain(t, body, "deactivate account", "orgs tab has no account control")
}

func TestAdminDirectoryDeactivateConfirmation(t *testing.T) {
	var calls []directoryCall
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(&calls), principal: settingsTestPrincipal()})
	for _, tc := range []struct {
		name   string
		form   url.Values
		want   int
		detail string
	}{
		{"missing reason", url.Values{"username": {"bob"}, "confirm": {"bob"}}, http.StatusUnprocessableEntity, "a reason is required"},
		{"wrong confirmation", url.Values{"username": {"bob"}, "confirm": {"Alice!"}, "reason": {"spam"}}, http.StatusUnprocessableEntity, "type the username exactly"},
		{"missing confirmation", url.Values{"username": {"bob"}, "reason": {"spam"}}, http.StatusUnprocessableEntity, "type the username exactly"},
		{"self", url.Values{"username": {"alice"}, "confirm": {"alice"}, "reason": {"x"}}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "self" {
				// The signed-in admin IS alice in this harness.
				rec := settingsDo(t, s, http.MethodPost, "/admin/directory/deactivate", tc.form, nil)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("self-deactivate = %d, want 422", rec.Code)
				}
				settingsMustContain(t, rec.Body.String(), "you cannot deactivate your own account", "self refusal")
				return
			}
			rec := settingsDo(t, s, http.MethodPost, "/admin/directory/deactivate", tc.form, nil)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d\n%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			settingsMustContain(t, rec.Body.String(), tc.detail, "named inline error")
		})
	}
	if len(calls) != 0 {
		t.Fatalf("a refused form reached the seam: %+v", calls)
	}

	// The accepted form: the reason and the account reach the seam once, and
	// the redirect carries the result back to the row.
	rec := settingsDo(t, s, http.MethodPost, "/admin/directory/deactivate",
		url.Values{"username": {"bob"}, "confirm": {"bob"}, "reason": {"  spam account  "}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deactivate = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "q=bob") || !strings.Contains(loc, "result=deactivated") {
		t.Fatalf("deactivate location = %q", loc)
	}
	if len(calls) != 1 || calls[0].username != "bob" || !calls[0].disabled || calls[0].reason != "spam account" {
		t.Fatalf("seam calls = %+v", calls)
	}
	body := settingsDo(t, s, http.MethodGet, "/admin/directory?q=bob&result=deactivated", nil, nil).Body.String()
	settingsMustContain(t, body, "Account deactivated", "result notice")
	settingsMustContain(t, body, "revoked permanently", "the token consequence is stated")
}

func TestAdminDirectoryReactivate(t *testing.T) {
	var calls []directoryCall
	s := newSettingsServer(t, &settingsFakes{deps: adminDirectoryDeps(&calls), principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodPost, "/admin/directory/reactivate", url.Values{"username": {"bob"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reactivate = %d, want 303", rec.Code)
	}
	if len(calls) != 1 || calls[0].username != "bob" || calls[0].disabled {
		t.Fatalf("seam calls = %+v", calls)
	}
	body := settingsDo(t, s, http.MethodGet, "/admin/directory?result=reactivated", nil, nil).Body.String()
	settingsMustContain(t, body, "Account reactivated", "reactivate notice")
	settingsMustContain(t, body, "not restored", "revoked credentials are not restored")
	// Cross-origin submission is refused before the seam.
	calls = nil
	rec = settingsDo(t, s, http.MethodPost, "/admin/directory/reactivate",
		url.Values{"username": {"bob"}}, map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden || len(calls) != 0 {
		t.Fatalf("cross-origin reactivate = %d, calls %+v", rec.Code, calls)
	}
}

func TestAdminDirectoryUnwiredAndEmpty(t *testing.T) {
	deps := adminBaseDeps() // no directory seams at all
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil).Body.String()
	settingsMustContain(t, body, "not wired on this build", "honest not-wired card")
	if rec := settingsDo(t, s, http.MethodPost, "/admin/directory/deactivate",
		url.Values{"username": {"bob"}, "confirm": {"bob"}, "reason": {"x"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired deactivate = %d, want 503", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/directory/reactivate",
		url.Values{"username": {"bob"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired reactivate = %d, want 503", rec.Code)
	}

	empty := adminBaseDeps()
	empty.DirectoryUsers = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		return &identity.DirectoryUserPage{}, nil
	}
	empty.DirectoryOrgs = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryOrgPage, error) {
		return &identity.DirectoryOrgPage{}, nil
	}
	s = newSettingsServer(t, &settingsFakes{deps: empty, principal: settingsTestPrincipal()})
	body = settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil).Body.String()
	settingsMustContain(t, body, "No accounts yet", "users empty state")
	body = settingsDo(t, s, http.MethodGet, "/admin/directory?tab=orgs", nil, nil).Body.String()
	settingsMustContain(t, body, "No organizations yet", "orgs empty state")
}

func TestAdminDirectoryReadErrorIsAnAlert(t *testing.T) {
	deps := adminBaseDeps()
	deps.DirectoryUsers = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		return nil, context.DeadlineExceeded
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read failure = %d, want a rendered page", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "context deadline exceeded", "read failure is visible")
	settingsMustContain(t, rec.Body.String(), `role="alert"`, "read failure is an alert")
}

// TestAdminDirectoryFillErrorPaths covers the read-failure arms (a failure is
// an alert, never a 500 and never a half page).
func TestAdminDirectoryFillErrorPaths(t *testing.T) {
	boom := errors.New("directory read failed")

	usersErr := adminBaseDeps()
	usersErr.DirectoryUsers = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		return nil, boom
	}
	s := newSettingsServer(t, &settingsFakes{deps: usersErr, principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil)
	settingsMustContain(t, rec.Body.String(), "directory read failed", "users read failure")

	orgsErr := adminBaseDeps()
	orgsErr.DirectoryOrgs = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryOrgPage, error) {
		return nil, boom
	}
	s = newSettingsServer(t, &settingsFakes{deps: orgsErr, principal: settingsTestPrincipal()})
	rec = settingsDo(t, s, http.MethodGet, "/admin/directory?tab=ORGS", nil, nil)
	settingsMustContain(t, rec.Body.String(), "directory read failed", "orgs read failure")
	settingsMustContain(t, rec.Body.String(), ">Organizations</a>", "tab normalization (?tab=ORGS)")

	rolesErr := adminBaseDeps()
	rolesErr.DirectoryUsers = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		return &identity.DirectoryUserPage{Rows: []identity.DirectoryUser{{Username: "alice", Bound: true}}}, nil
	}
	rolesErr.UserOrgRoles = func(context.Context, []string) (map[string][]identity.OrgMembership, bool, error) {
		return nil, false, boom
	}
	s = newSettingsServer(t, &settingsFakes{deps: rolesErr, principal: settingsTestPrincipal()})
	rec = settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil)
	settingsMustContain(t, rec.Body.String(), "directory read failed", "membership read failure")

	// A truncated membership walk is reported (the page never implies it saw
	// every organization).
	truncated := adminBaseDeps()
	truncated.DirectoryUsers = func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
		return &identity.DirectoryUserPage{Rows: []identity.DirectoryUser{{Username: "alice", Bound: true}}}, nil
	}
	truncated.UserOrgRoles = func(context.Context, []string) (map[string][]identity.OrgMembership, bool, error) {
		return map[string][]identity.OrgMembership{"alice": {{Org: "acme", Owner: true}}}, true, nil
	}
	s = newSettingsServer(t, &settingsFakes{deps: truncated, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/directory", nil, nil).Body.String()
	settingsMustContain(t, body, "acme", "annotated row")
	settingsMustContain(t, body, "(owner)", "owner marker")
}

// TestAdminDirectoryActionErrorStatuses pins the mapping from the identity
// sentinels to page statuses, and the required-username arms.
func TestAdminDirectoryActionErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		path   string
		form   url.Values
		want   int
		detail string
	}{
		{"deactivate unknown account", identity.ErrNotFound, "/admin/directory/deactivate",
			url.Values{"username": {"ghost"}, "confirm": {"ghost"}, "reason": {"x"}}, http.StatusNotFound, "not found"},
		{"deactivate conflict", identity.ErrConflict, "/admin/directory/deactivate",
			url.Values{"username": {"bob"}, "confirm": {"bob"}, "reason": {"x"}}, http.StatusConflict, "conflict"},
		{"deactivate unavailable", identity.ErrUnavailable, "/admin/directory/deactivate",
			url.Values{"username": {"bob"}, "confirm": {"bob"}, "reason": {"x"}}, http.StatusServiceUnavailable, "unavailable"},
		{"deactivate missing username", nil, "/admin/directory/deactivate",
			url.Values{}, http.StatusUnprocessableEntity, "a username is required"},
		{"reactivate missing username", nil, "/admin/directory/reactivate",
			url.Values{}, http.StatusUnprocessableEntity, "a username is required"},
		{"reactivate unknown account", identity.ErrNotFound, "/admin/directory/reactivate",
			url.Values{"username": {"ghost"}}, http.StatusNotFound, "not found"},
		{"reactivate conflict", identity.ErrConflict, "/admin/directory/reactivate",
			url.Values{"username": {"bob"}}, http.StatusConflict, "conflict"},
		{"reactivate unavailable", identity.ErrUnavailable, "/admin/directory/reactivate",
			url.Values{"username": {"bob"}}, http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []directoryCall
			deps := adminDirectoryDeps(&calls)
			deps.SetUserDisabled = func(_ context.Context, username string, disabled bool, reason string) error {
				if tc.err == nil {
					t.Fatalf("the seam must not be called for a refused form (%s)", tc.name)
				}
				return tc.err
			}
			s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
			rec := settingsDo(t, s, http.MethodPost, tc.path, tc.form, nil)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d\n%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			settingsMustContain(t, rec.Body.String(), tc.detail, "named failure")
		})
	}
}

// TestBindDirectoryFillsTheSeam proves the composition helper fills all four
// deps (the wiring guard's shape for this page).
func TestBindDirectoryFillsTheSeam(t *testing.T) {
	BindAdmin(AdminDeps{})
	t.Cleanup(func() { BindAdmin(AdminDeps{}) })
	BindDirectory(DirectorySeam{
		Users: func(context.Context, identity.DirectoryQuery) (*identity.DirectoryUserPage, error) {
			return &identity.DirectoryUserPage{}, nil
		},
		Orgs: func(context.Context, identity.DirectoryQuery) (*identity.DirectoryOrgPage, error) {
			return &identity.DirectoryOrgPage{}, nil
		},
		OrgRoles: func(context.Context, []string) (map[string][]identity.OrgMembership, bool, error) {
			return nil, false, nil
		},
		SetState: func(context.Context, string, bool, string) error { return nil },
	})
	d := adminDeps()
	if d.DirectoryUsers == nil || d.DirectoryOrgs == nil || d.UserOrgRoles == nil || d.SetUserDisabled == nil {
		t.Fatalf("BindDirectory left a seam nil: %+v", d)
	}
}
