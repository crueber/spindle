// settings_wiring_test.go — the WP9 G2 guard: composition must not leave a
// settings seam the page renders nil. A nil seam does not fail loudly — the
// page renders its "not wired" card and every submit 503s (the org settings
// regression this guards), so the guard asserts the bindings and then proves
// OrgWrite actually round-trips through identity.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/webui"
)

func TestSettingsWiringBindsEveryRenderedSeam(t *testing.T) {
	cfg := config.Defaults()
	st := store.NewMemory()
	ident := identity.New(st, cfg)
	notes, _ := newNotifyService(st, ident)
	c := &collabWiring{ident: ident, notifySvc: notes}

	webui.BindSettings(webui.SettingsDeps{})
	t.Cleanup(func() { webui.BindSettings(webui.SettingsDeps{}) })
	wireSettingsPages(cfg, server.BootState{Mode: "normal"}, &api.Env{}, c)
	d := webui.SettingsBindings()

	// The surfaces the page renders unconditionally with a normal store
	// boot. Mirror/Push are bound only when those services exist (not in
	// this harness) and are not part of the guard.
	seams := []struct {
		name string
		ok   bool
	}{
		{"SetupAccess", d.SetupAccess != nil},
		{"SetupView", d.SetupView != nil},
		{"SetupValidate", d.SetupValidate != nil},
		{"SetupSave", d.SetupSave != nil},
		{"SetupAuthTest", d.SetupAuthTest != nil},
		{"RepoRole", d.RepoRole != nil},
		{"RepoGeneral", d.RepoGeneral != nil},
		{"RepoScheduled", d.RepoScheduled != nil},
		{"RepoPolicy", d.RepoPolicy != nil},
		{"RepoProtection", d.RepoProtection != nil},
		{"RepoConfig", d.RepoConfig != nil},
		{"RepoAccess", d.RepoAccess != nil},
		{"RepoAdmin", d.RepoAdmin != nil},
		{"RepoWrite", d.RepoWrite != nil},
		{"OrgView", d.OrgView != nil},
		{"OrgWrite", d.OrgWrite != nil},
		{"OrgWebhookWrite", d.OrgWebhookWrite != nil},
	}
	for _, s := range seams {
		if !s.ok {
			t.Errorf("composition left SettingsDeps.%s nil (the page renders but writes 503)", s.name)
		}
	}
	if d.OrgView == nil || d.OrgWrite == nil {
		t.Fatal("org settings not wired; write-path proof skipped")
	}

	// Not just non-nil: the seam writes through identity, and its authority
	// is the API's CheckOrgOwner (owner or host admin; everyone else 403).
	ctx := context.Background()
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	owner := auth.Principal{Name: "alice@example.com", Write: true}
	res, werr := d.OrgWrite(ctx, "acme", owner, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgProfileSave,
		Values: map[string]string{"display_name": "Acme Renamed", "description": "d", "location": "Berlin", "timezone": "UTC", "bio_markdown": "**hi**"},
	})
	if werr != nil || res.Notice == "" {
		t.Fatalf("owner profile write = %+v %+v", res, werr)
	}
	org, err := ident.GetOrg(ctx, "acme")
	if err != nil || org == nil || org.DisplayName != "Acme Renamed" || org.Location != "Berlin" {
		t.Fatalf("profile did not round-trip: %+v err %v", org, err)
	}
	if _, werr := d.OrgWrite(ctx, "acme", auth.Principal{Name: "bob@example.com", Write: true}, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgProfileSave, Values: map[string]string{"display_name": "hijack"},
	}); werr == nil || werr.Status != http.StatusForbidden {
		t.Fatalf("non-owner org write = %+v, want 403", werr)
	}
	admin := auth.Principal{Name: "root@example.com", Admin: true}
	if _, werr := d.OrgWrite(ctx, "acme", admin, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgMemberSet, Values: map[string]string{"principal": "bob@example.com", "role": "member"},
	}); werr != nil {
		t.Fatalf("host admin org write = %+v", werr)
	}
}

// TestOrgInviteLinkOneShotAcceptGrantsRoleAndCancelConflicts is the org
// invitation page contract end to end over the real identity service: the
// create response is the only place the accept link exists (a later page read
// is redacted), accepting grants the role the API defines, and a cancelled
// invitation's link answers 409.
func TestOrgInviteLinkOneShotAcceptGrantsRoleAndCancelConflicts(t *testing.T) {
	cfg := config.Defaults()
	ident := identity.New(store.NewMemory(), cfg)
	write := orgWriteFn(ident)
	view := orgViewFn(ident, nil)
	ctx := context.Background()
	owner := auth.Principal{Name: "alice@example.com", Write: true}
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", owner.Name); err != nil {
		t.Fatal(err)
	}

	invite := func(email, role string) webui.SettingsWriteResult {
		t.Helper()
		res, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{
			Action: webui.SettingsOrgInviteMake,
			Values: map[string]string{"subject": email, "role": role},
		})
		if werr != nil {
			t.Fatalf("invite %s: %+v", email, werr)
		}
		return res
	}

	res := invite("jane@example.com", "member")
	if !strings.HasPrefix(res.Link, "/invitations?id=") || !strings.Contains(res.Link, "&token=") {
		t.Fatalf("create link = %q, want the human /invitations page", res.Link)
	}
	if res.Invite == nil || res.Invite.AcceptURL != res.Link || res.Invite.ID == "" {
		t.Fatalf("created row = %+v (link %q)", res.Invite, res.Link)
	}
	pending, _ := ident.ListOrgInvites(ctx, "acme", 10)
	if len(pending) != 1 || !strings.Contains(res.Link, pending[0].Token) {
		t.Fatalf("create link does not carry the stored token: %q %+v", res.Link, pending)
	}

	// The page read never carries the token: the pending row renders masked.
	page, err := view(ctx, "acme", owner)
	if err != nil || page == nil {
		t.Fatalf("view: %v", err)
	}
	if len(page.Invites) != 1 || page.Invites[0].AcceptURL != "" {
		t.Fatalf("pending read exposed an accept link: %+v", page.Invites)
	}
	bobView, err := view(ctx, "acme", auth.Principal{Name: "bob@example.com", Write: true})
	if err != nil || bobView == nil {
		t.Fatalf("member view: %v", err)
	}
	if bobView.CanManage || len(bobView.Invites) != 0 {
		t.Fatalf("non-owner read invitations: %+v", bobView.Invites)
	}
	if _, werr := write(ctx, "acme", auth.Principal{Name: "bob@example.com", Write: true}, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgInviteMake, Values: map[string]string{"subject": "x@example.com", "role": "member"},
	}); werr == nil || werr.Status != http.StatusForbidden {
		t.Fatalf("non-owner invite = %+v, want 403", werr)
	}

	// Accept grants the invited org role (member, then owner).
	if bound, aerr := ident.AcceptInvite(ctx, "jane@example.com", pending[0].ID); aerr != nil || bound != "org" {
		t.Fatalf("accept = %q %v", bound, aerr)
	}
	m, _ := ident.GetMembers(ctx, "acme")
	roleOf := func(name string) identity.OrgRole {
		for _, row := range m.Members {
			if row.Principal == name {
				return row.Role
			}
		}
		return ""
	}
	if got := roleOf("jane@example.com"); got != identity.OrgMember {
		t.Fatalf("jane role = %q, want member", got)
	}
	res = invite("bob@example.com", "owner")
	ownerInv := res.Invite
	if bound, aerr := ident.AcceptInvite(ctx, "bob@example.com", ownerInv.ID); aerr != nil || bound != "org" {
		t.Fatalf("owner accept = %q %v", bound, aerr)
	}
	m, _ = ident.GetMembers(ctx, "acme")
	if got := roleOf("bob@example.com"); got != identity.OrgOwner {
		t.Fatalf("bob role = %q, want owner", got)
	}

	// Cancel deletes the issuer object: the copied link answers 409.
	res = invite("carl@example.com", "member")
	cancelled := res.Invite.ID
	if _, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgInviteRm, Values: map[string]string{"id": cancelled},
	}); werr != nil {
		t.Fatalf("cancel: %+v", werr)
	}
	if _, aerr := ident.AcceptInvite(ctx, "carl@example.com", cancelled); !errors.Is(aerr, identity.ErrConflict) {
		t.Fatalf("accept after cancel = %v, want conflict", aerr)
	}
	invID := strings.SplitN(strings.TrimPrefix(res.Link, "/invitations?id="), "&", 2)[0]
	token := ""
	if rest := strings.SplitN(res.Link, "&token=", 2); len(rest) == 2 {
		token = rest[1]
	}
	if _, perr := ident.PreviewInvite(ctx, "carl@example.com", invID, token); !errors.Is(perr, identity.ErrConflict) {
		t.Fatalf("preview after cancel = %v, want conflict", perr)
	}
}

// TestOrgUsernameInviteAcceptedByTokenPrincipal is defect 4 (round-3 audit):
// the page form addresses an email OR a username, so a static-token principal
// (no verified email) can be invited and can claim a username-addressed
// invitation. An email-addressed invite still resolves through the email
// inbox, and a foreign principal still conflicts.
func TestOrgUsernameInviteAcceptedByTokenPrincipal(t *testing.T) {
	cfg := config.Defaults()
	ident := identity.New(store.NewMemory(), cfg)
	write := orgWriteFn(ident)
	ctx := context.Background()
	owner := auth.Principal{Name: "alice@example.com", Write: true}
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", owner.Name); err != nil {
		t.Fatal(err)
	}
	invite := func(subject, role string) *identity.Invitation {
		t.Helper()
		res, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{
			Action: webui.SettingsOrgInviteMake,
			Values: map[string]string{"subject": subject, "role": role},
		})
		if werr != nil {
			t.Fatalf("invite %s: %+v", subject, werr)
		}
		if res.Invite == nil || res.Link == "" {
			t.Fatalf("invite %s returned no row/link: %+v", subject, res)
		}
		return &identity.Invitation{ID: res.Invite.ID, Subject: res.Invite.Subject}
	}

	// A username-addressed invite is claimable by the email-less principal.
	inv := invite("carol", "member")
	if _, err := ident.AcceptInviteFor(ctx, auth.Principal{Name: "carol", Write: true}, inv.ID); err != nil {
		t.Fatalf("username invite accept = %v", err)
	}
	m, _ := ident.GetMembers(ctx, "acme")
	found := false
	for _, row := range m.Members {
		if row.Principal == "carol" && row.Role == identity.OrgMember {
			found = true
		}
	}
	if !found {
		t.Fatalf("carol is not an org member: %+v", m.Members)
	}

	// A foreign principal cannot claim it (409 conflict, not a silent bind).
	inv = invite("dave", "member")
	if _, err := ident.AcceptInviteFor(ctx, auth.Principal{Name: "mallory"}, inv.ID); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("foreign accept = %v, want conflict", err)
	}

	// An email-addressed invite is unchanged: the principal carrying that
	// verified email still claims it, and an email-less lookup still conflicts.
	inv = invite("erin@example.com", "member")
	if _, err := ident.AcceptInviteFor(ctx, auth.Principal{Name: "erin", Email: "erin@example.com"}, inv.ID); err != nil {
		t.Fatalf("email invite accept = %v", err)
	}
	inv = invite("frank@example.com", "member")
	if _, err := ident.AcceptInviteFor(ctx, auth.Principal{Name: "frank"}, inv.ID); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("email-less accept of an email invite = %v, want conflict", err)
	}
}

// TestOrgWriteSeamCoversEveryForm exercises the bound OrgWrite against a real
// identity service for every form the page renders (profile/avatar, members,
// teams, invitations, delete). This is the G2 write-path proof, not just a
// non-nil guard.
func TestOrgWriteSeamCoversEveryForm(t *testing.T) {
	cfg := config.Defaults()
	st := store.NewMemory()
	ident := identity.New(st, cfg)
	write := orgWriteFn(ident)
	ctx := context.Background()
	owner := auth.Principal{Name: "alice@example.com", Write: true}
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	must := func(action string, values map[string]string, file []byte) string {
		t.Helper()
		res, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{Action: action, Values: values, File: file})
		if werr != nil {
			t.Fatalf("%s: %+v", action, werr)
		}
		return res.Notice
	}

	// Profile + avatar + avatar removal.
	must(webui.SettingsOrgProfileSave, map[string]string{
		"display_name": "Acme Renamed", "description": "tagline", "location": "Berlin",
		"timezone": "UTC", "bio_markdown": "**hi**",
	}, nil)
	org, _ := ident.GetOrg(ctx, "acme")
	if org.DisplayName != "Acme Renamed" || org.BioMarkdown != "**hi**" {
		t.Fatalf("profile = %+v", org)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	must(webui.SettingsOrgAvatar, nil, png)
	if org, _ := ident.GetOrg(ctx, "acme"); org.AvatarContentType == "" {
		t.Fatal("avatar pointer missing")
	}
	must(webui.SettingsOrgAvatarRm, nil, nil)
	if org, _ := ident.GetOrg(ctx, "acme"); org.AvatarContentType != "" {
		t.Fatal("avatar not removed")
	}

	// Members: add, re-role, remove.
	must(webui.SettingsOrgMemberSet, map[string]string{"principal": "bob@example.com", "role": "member"}, nil)
	must(webui.SettingsOrgMemberSet, map[string]string{"principal": "bob@example.com", "role": "owner"}, nil)
	m, _ := ident.GetMembers(ctx, "acme")
	if len(m.Members) != 2 || m.Members[1].Role != identity.OrgOwner {
		t.Fatalf("members = %+v", m.Members)
	}
	must(webui.SettingsOrgMemberRm, map[string]string{"principal": "bob@example.com"}, nil)
	if m, _ := ident.GetMembers(ctx, "acme"); len(m.Members) != 1 {
		t.Fatalf("member not removed: %+v", m.Members)
	}

	// Teams: create, rename/description, add/remove member, delete.
	must(webui.SettingsOrgTeamSave, map[string]string{"slug": "devs", "name": "Devs", "description": "d"}, nil)
	must(webui.SettingsOrgTeamUpdate, map[string]string{"slug": "devs", "name": "Platform", "description": "renamed"}, nil)
	tm, _, _ := ident.GetTeam(ctx, "acme", "devs")
	if tm == nil || tm.Name != "Platform" || tm.Description != "renamed" {
		t.Fatalf("team = %+v", tm)
	}
	must(webui.SettingsOrgTeamAdd, map[string]string{"slug": "devs", "principal": "alice@example.com"}, nil)
	if tm, _, _ := ident.GetTeam(ctx, "acme", "devs"); len(tm.Members) != 1 {
		t.Fatalf("team member not added: %+v", tm.Members)
	}
	must(webui.SettingsOrgTeamMemberRm, map[string]string{"slug": "devs", "principal": "alice@example.com"}, nil)
	if tm, _, _ := ident.GetTeam(ctx, "acme", "devs"); len(tm.Members) != 0 {
		t.Fatalf("team member not removed: %+v", tm.Members)
	}
	must(webui.SettingsOrgTeamDel, map[string]string{"slug": "devs"}, nil)
	if tm, _, _ := ident.GetTeam(ctx, "acme", "devs"); tm != nil {
		t.Fatal("team not deleted")
	}

	// Invitations: create (share link) + cancel.
	res, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{
		Action: webui.SettingsOrgInviteMake, Values: map[string]string{"subject": "jane@example.com", "role": "member"},
	})
	if werr != nil || res.Link == "" {
		t.Fatalf("invite = %+v %+v", res, werr)
	}
	invs, err := ident.ListOrgInvites(ctx, "acme", 10)
	if err != nil || len(invs) != 1 {
		t.Fatalf("invites = %+v err %v", invs, err)
	}
	must(webui.SettingsOrgInviteRm, map[string]string{"id": invs[0].ID}, nil)
	if invs, _ := ident.ListOrgInvites(ctx, "acme", 10); len(invs) != 0 {
		t.Fatalf("invite not cancelled: %+v", invs)
	}

	// Unknown actions and non-owners fail closed before any write.
	if _, werr := write(ctx, "acme", owner, webui.SettingsOrgWrite{Action: "nope"}); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unknown action = %+v", werr)
	}
	if _, werr := write(ctx, "acme", auth.Principal{Name: "bob@example.com"}, webui.SettingsOrgWrite{Action: webui.SettingsOrgMemberSet}); werr == nil || werr.Status != http.StatusForbidden {
		t.Fatalf("non-owner = %+v, want 403", werr)
	}

	// Danger: delete redirects home (the org is gone afterwards).
	res, werr = write(ctx, "acme", owner, webui.SettingsOrgWrite{Action: webui.SettingsOrgDelete})
	if werr != nil || res.Redirect != "/" {
		t.Fatalf("delete = %+v %+v", res, werr)
	}
}
