package webui

// settings_role_matrix_test.go — the round-2 web audit regression matrix.
//
// The defect: settingsRoleRank defaulted an empty/unresolved role to admin, so
// an authenticated non-member of a public repo got every settings tab at 200
// with live mutation forms (and the Mirror tab printed the upstream URL). The
// matrix below pins each principal against every tab and asserts two things
// per cell: the status, and that a 200 never carries a mutation control the
// principal cannot use. The no-role row fails without the fail-closed rank.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// settingsMatrixFakes wires every tab's read seam with sentinel data so the
// absence assertions are meaningful: an admin 200 must actually render the
// forms (checked below), which proves the non-admin 200s would have shown them.
func settingsMatrixFakes(role string) *settingsFakes {
	f := &settingsFakes{deps: SettingsDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return role, nil },
		RepoGeneral: func(context.Context, string, string) (*SettingsGeneral, error) {
			return &SettingsGeneral{Description: "a monorepo", MaxLength: 200}, nil
		},
		RepoScheduled: func(context.Context, string, string) (*SettingsScheduled, error) {
			return &SettingsScheduled{}, nil
		},
		RepoMirror: func(context.Context, string, string) (*SettingsMirror, error) {
			return &SettingsMirror{Configured: true, Upstream: "https://secret.internal/upstream.git"}, nil
		},
		RepoPushMirror: func(context.Context, string, string) (*SettingsPushMirror, error) {
			return &SettingsPushMirror{}, nil
		},
		RepoPolicy: func(context.Context, string, string) (*SettingsPolicy, error) {
			return &SettingsPolicy{Text: "protect main"}, nil
		},
		RepoProtection: func(context.Context, string, string) (*SettingsProtection, error) {
			return &SettingsProtection{Refs: "refs/heads/main"}, nil
		},
		RepoConfig: func(context.Context, string, string) (*SettingsConfig, error) {
			return &SettingsConfig{Text: "[features]\n"}, nil
		},
		RepoAccess: func(context.Context, string, string, auth.Principal) (*SettingsAccess, error) {
			return &SettingsAccess{
				Role: role, Visibility: "public", Version: 3,
				Bindings: []SettingsBinding{{Subject: "user:owner", Role: "admin"}},
			}, nil
		},
		RepoAdmin: func(context.Context, string, string, auth.Principal) (*SettingsAdmin, error) {
			return &SettingsAdmin{TokensWired: true, WebhooksWired: true}, nil
		},
	}}
	return f
}

// settingsTabFloors is the tab table as the contract states it.
var settingsTabFloors = []struct {
	tab   string
	floor string
}{
	{"general", "admin"},
	{"scheduled", "admin"},
	{"mirror", "admin"},
	{"policy", "admin"},
	{"protection", "admin"},
	{"config", "admin"},
	{"access", "triage"},
	{"tokens", "admin"},
	{"webhooks", "admin"},
	{"wal", "read"},
	{"danger", "admin"},
}

func TestSettingsRoleMatrix(t *testing.T) {
	roles := []struct {
		name string
		role string
		anon bool
	}{
		{"anonymous", "", true},
		{"no-role", "", false},
		{"read", "read", false},
		{"triage", "triage", false},
		{"write", "write", false},
		{"maintain", "maintain", false},
		{"admin", "admin", false},
	}
	for _, row := range roles {
		t.Run(row.name, func(t *testing.T) {
			f := settingsMatrixFakes(row.role)
			if row.anon {
				f.principal = auth.Principal{Name: "anonymous", Anonymous: true}
			}
			s := newSettingsServer(t, f)
			for _, tf := range settingsTabFloors {
				want := http.StatusOK
				if !settingsRoleAtLeast(row.role, tf.floor) {
					want = http.StatusForbidden
				}
				rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tf.tab, nil, nil)
				if rec.Code != want {
					t.Fatalf("%s/%s = %d, want %d", row.name, tf.tab, rec.Code, want)
				}
				body := rec.Body.String()
				if rec.Code != http.StatusOK {
					if strings.Contains(body, "secret.internal") {
						t.Fatalf("%s/%s: denied body leaked the upstream URL", row.name, tf.tab)
					}
					continue
				}
				if row.name == "admin" {
					continue // the admin view is asserted unchanged separately
				}
				assertNoUnusableMutation(t, row.name, tf.tab, body)
			}
		})
	}
}

// assertNoUnusableMutation bans every mutation control on a non-admin 200:
// no htmx POST form at all, no named write fields, no secrets.
func assertNoUnusableMutation(t *testing.T, role, tab, body string) {
	t.Helper()
	markers := []string{
		`hx-post="/acme/monorepo/settings/`,
		"mirror_action",
		"danger_action",
		"upstream_url",
		"new_subject",
		"binding_count",
		"create token",
		"create webhook",
		"secret.internal",
	}
	for _, m := range markers {
		if strings.Contains(body, m) {
			t.Fatalf("%s/%s 200 carries an unusable mutation control %q", role, tab, m)
		}
	}
}

// TestSettingsAdminViewUnchanged proves the absence assertions above have
// teeth: the admin row renders the very forms non-admins must never see, and
// the Mirror tab still shows the configured upstream to an admin.
func TestSettingsAdminViewUnchanged(t *testing.T) {
	s := newSettingsServer(t, settingsMatrixFakes("admin"))
	cases := map[string][]string{
		"general":  {`hx-post="/acme/monorepo/settings/general"`, "<button type=\"submit\" class=\"btn primary\">Save</button>"},
		"mirror":   {`hx-post="/acme/monorepo/settings/mirror"`, "secret.internal/upstream.git", "mirror_action"},
		"access":   {`hx-post="/acme/monorepo/settings/access"`, "new_subject", "Save"},
		"tokens":   {`hx-post="/acme/monorepo/settings/tokens"`, "create token"},
		"webhooks": {`hx-post="/acme/monorepo/settings/webhooks"`, "create webhook"},
		"danger":   {`hx-post="/acme/monorepo/settings/danger"`, "danger_action"},
	}
	for tab, markers := range cases {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin GET %s = %d", tab, rec.Code)
		}
		body := rec.Body.String()
		for _, m := range markers {
			if !strings.Contains(body, m) {
				t.Fatalf("admin GET %s missing %q", tab, m)
			}
		}
	}
}

// TestSettingsTriageAccessTabIsReadOnly pins the one tab a non-admin may read
// that also has an admin-only write: triage sees the bindings, never the form.
func TestSettingsTriageAccessTabIsReadOnly(t *testing.T) {
	s := newSettingsServer(t, settingsMatrixFakes("triage"))
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/access", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("triage GET access = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "data-access-readonly", "read-only access panel")
	settingsMustContain(t, body, "user:owner", "bindings visible to triage")
	settingsMustNotContain(t, body, `hx-post="/acme/monorepo/settings/access"`, "triage access form")
	settingsMustNotContain(t, body, "new_subject", "triage add-binding field")
}
