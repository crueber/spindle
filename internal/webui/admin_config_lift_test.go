package webui

// admin_config_lift_test.go — PLAN-ADMIN M3: the Configuration page's
// progressive disclosure over the whole schema. Search (?q), changed-only
// (?changed), per-field reset (?reset), section anchors, the saved-TOML
// disclosure, error navigation, and the unsaved-changes guard's markup.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/setup"
)

// adminLiftView is a small view with one default row, one changed Advanced
// row, one changed cache row and one default cache row, so every filter has
// something to include and something to exclude.
func adminLiftView() *AdminConfigView {
	return &AdminConfigView{
		FileState: "valid",
		AuthMode:  "none",
		TOML:      "[server]\nlisten = \"0.0.0.0:8080\"\n",
		Sections: []AdminConfigSection{
			{Section: "server", Essential: []AdminConfigField{
				{Key: "server.listen", Type: "string", Value: "0.0.0.0:8080", Default: "0.0.0.0:8080"},
				{Key: "server.request_timeout", Type: "duration", Value: "2h0m0s", Default: "1h0m0s"},
			}, Advanced: []AdminConfigField{
				{Key: "server.http2", Type: "bool", Value: "false", Default: "true"},
			}},
			{Section: "auth", Essential: []AdminConfigField{
				{Key: "server.auth.mode", Value: "none", Default: "none", Enum: []string{"none", "token", "oidc"}},
			}},
			{Section: "cache", Essential: []AdminConfigField{
				{Key: "cache.max_bytes", Type: "size", Value: "32212254720", Default: "21474836480"},
				{Key: "cache.mode", Value: "auto", Default: "auto", Enum: []string{"budget", "disk", "auto"}},
			}},
		},
	}
}

func adminLiftServer(t *testing.T, view *AdminConfigView) *Server {
	t.Helper()
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return view, nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			t.Fatal("a GET must never call Save")
			return AdminSaveResult{}, nil, nil
		},
	}}
	return newSettingsServer(t, f)
}

func TestAdminConfigLiftSearchHonouredWithoutJS(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	rec := settingsDo(t, s, http.MethodGet, "/admin/config?q=listen", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET filtered = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-key="server.listen"`, "matching row")
	settingsMustNotContain(t, body, `data-key="cache.max_bytes"`, "non-matching row is narrowed")
	settingsMustNotContain(t, body, `data-section="cache"`, "a section with no match is dropped")
	settingsMustContain(t, body, "Showing 1 of 6 keys", "row count")
	settingsMustContain(t, body, `value="listen"`, "query survives in the search input")
	settingsMustContain(t, body, `name="q"`, "server-honoured query parameter")
	settingsMustContain(t, body, `data-filter-clear`, "way to clear the filter")
	settingsMustContain(t, body, `data-filter-count data-total="6"`, "client count uses the unfiltered total")
}

func TestAdminConfigLiftFilterOpensAdvancedGroups(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config?q=http2", nil, nil).Body.String()
	settingsMustContain(t, body, `data-key="server.http2"`, "advanced match")
	settingsMustContain(t, body, `data-advanced-for="server" open`, "a filtered Advanced group opens")
}

func TestAdminConfigLiftChangedOnly(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config?changed=1", nil, nil).Body.String()
	settingsMustContain(t, body, `data-key="server.request_timeout"`, "changed duration row")
	settingsMustContain(t, body, `data-key="server.http2"`, "changed advanced row")
	settingsMustContain(t, body, `data-key="cache.max_bytes"`, "changed size row")
	settingsMustNotContain(t, body, `data-key="server.listen"`, "default row hidden")
	settingsMustNotContain(t, body, `data-key="cache.mode"`, "default enum hidden")
	settingsMustNotContain(t, body, `data-section="auth"`, "a section with no changed row is dropped")
	settingsMustContain(t, body, "Showing 3 of 6 keys", "changed count")
	settingsMustContain(t, body, "3 changed", "total changed count")
	settingsMustContain(t, body, `name="changed" value="1"`, "the toggle is a named checkbox")
	settingsMustContain(t, body, "checked", "checked state round-trips")
}

// TestAdminConfigLiftFilterMatchesIsland mirrors web/test/unit/filter.test.js:
// the server's ?q= and the island's live filter are one predicate, so a row
// the server renders must survive the typed filter and vice versa.
func TestAdminConfigLiftFilterMatchesIsland(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, query string
		want       bool
	}{
		{"server.listen", "", true},
		{"server.listen", "   ", true},
		{"README.md", "readme", true},
		{"src/main.go", "MAIN", true},
		{"go.mod", ".mod", true},
		{"server.listen", "telemetry", false},
		{"", "x", false},
		{"server.auth.tokens", "AUTH", true},
	}
	for _, c := range cases {
		if got := adminMatchesQuery(c.key, c.query); got != c.want {
			t.Errorf("adminMatchesQuery(%q,%q) = %v, want %v", c.key, c.query, got, c.want)
		}
	}
}

func TestAdminConfigLiftResetSeedsDefaultSpelling(t *testing.T) {
	for _, tc := range []struct {
		name, key, want, notice string
	}{
		{"size", "cache.max_bytes", `value="20GiB"`, "reset to its compiled default (20GiB)"},
		{"duration", "server.request_timeout", `value="1h"`, "reset to its compiled default (1h)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := adminLiftServer(t, adminLiftView())
			body := settingsDo(t, s, http.MethodGet, "/admin/config?reset="+url.QueryEscape(tc.key), nil, nil).Body.String()
			settingsMustContain(t, body, tc.want, "reset lands on the default spelling")
			settingsMustContain(t, body, tc.notice, "reset notice")
			settingsMustContain(t, body, "nothing was saved", "reset is not a write")
		})
	}
	t.Run("unknown key", func(t *testing.T) {
		s := adminLiftServer(t, adminLiftView())
		body := settingsDo(t, s, http.MethodGet, "/admin/config?reset=no.such.key", nil, nil).Body.String()
		settingsMustNotContain(t, body, "reset to its compiled default", "no notice for an unknown key")
	})
}

func TestAdminConfigLiftResetLinkAffordance(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	// Only rows that differ from the default offer reset; the compiled default
	// is the affordance title.
	settingsMustContain(t, body, `href="?reset=cache.max_bytes#row-cache.max_bytes"`, "reset link")
	settingsMustContain(t, body, `title="Reset to the compiled default: 20GiB"`, "default shown as title")
	settingsMustNotContain(t, body, `href="?reset=server.listen#row-server.listen"`, "a default row has no reset")
}

func TestAdminConfigLiftTOMLViewIsSavedAndReadOnly(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	settingsMustContain(t, body, `id="admin-toml"`, "TOML disclosure")
	settingsMustContain(t, body, "read-only", "read-only label")
	settingsMustContain(t, body, "saved configuration", "source is the saved configuration")
	settingsMustContain(t, body, "listen = &#34;0.0.0.0:8080&#34;", "TOML document rendered (escaped)")
	settingsMustContain(t, body, `data-copy="#admin-toml-doc"`, "copy affordance")
}

func TestAdminConfigLiftAnchorsResolve(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	for _, want := range []string{
		`id="server"`, `id="auth"`, `id="server.auth"`, `id="cache"`,
		`href="#server"`, `href="#auth"`, `href="#cache"`,
		`class="card scroll-mt-16 p-4"`,
	} {
		settingsMustContain(t, body, want, "section anchor")
	}
	// Section anchors are links in the strip, never buttons.
	settingsMustNotContain(t, body, `href="#server" class="btn"`, "anchors are links")
}

func TestAdminConfigLiftSectionWhyCoversSchema(t *testing.T) {
	t.Parallel()
	// Totality, not a count: a pinned number breaks the moment a section is
	// added (the test-fixture sweep found this one), and an orphan line is the
	// failure that actually matters.
	groups := map[string]bool{}
	for _, g := range setup.BuildSchema(config.Defaults(), config.Defaults()) {
		groups[g.Section] = true
		if sectionWhy[g.Section] == "" {
			t.Errorf("section %q has no sectionWhy line", g.Section)
		}
	}
	for section := range sectionWhy {
		if !groups[section] {
			t.Errorf("sectionWhy has a line for %q, which is not a schema group", section)
		}
	}
}

func TestAdminConfigLiftErrorSummaryNavigates(t *testing.T) {
	view := adminLiftView()
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return view, nil },
		Save: func(context.Context, map[string]string) (AdminSaveResult, []AdminError, error) {
			return AdminSaveResult{}, []AdminError{
				{Key: "server.http2", Message: "server.http2: must be a boolean"},
				{Message: "something keyless went wrong"},
			}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected save = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	// The summary is an alert, each error links to its row (or has no link).
	settingsMustContain(t, body, `role="alert"`, "error summary announced")
	settingsMustContain(t, body, `href="#row-server.http2"`, "summary links the row")
	settingsMustContain(t, body, "server.http2: must be a boolean", "summary message")
	settingsMustContain(t, body, "something keyless went wrong", "keyless error stays visible")
	// The row is marked, the group opens, the control is the focus target.
	settingsMustContain(t, body, `id="row-server.http2"`, "row anchor")
	settingsMustContain(t, body, `data-invalid="1"`, "row marked")
	settingsMustContain(t, body, `data-error-focus`, "focus target")
	settingsMustContain(t, body, `<span class="chip chip-closed">invalid</span>`, "text state, not colour-only")
	settingsMustContain(t, body, `aria-invalid="true"`, "control state for AT")
	settingsMustContain(t, body, `aria-describedby="err-server.http2-0"`, "error line association")
	settingsMustContain(t, body, `id="err-server.http2-0"`, "error line id")
	settingsMustContain(t, body, `data-advanced-for="server" open`, "collapsed group opens")
}

func TestAdminConfigLiftDirtyGuardMarkup(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	settingsMustContain(t, body, `data-leave-guard`, "form is guarded")
	settingsMustContain(t, body, `data-dirty-indicator`, "save button dirty indicator")
	settingsMustContain(t, body, `data-dirty-status role="status" aria-live="polite"`, "dirty state announced")
	settingsMustContain(t, body, `id="admin-config-form"`, "filter island target")
	settingsMustContain(t, body, `data-filter="#admin-config-form"`, "search input wired")
	settingsMustContain(t, body, `type="search"`, "real search control")
	settingsMustContain(t, body, `for="admin-config-q"`, "labelled search input")
	// One primary per page, and it is Save (the filter submit is secondary).
	if n := strings.Count(body, `class="btn primary"`); n != 1 {
		t.Fatalf("primary buttons = %d, want exactly Save", n)
	}
	settingsMustContain(t, body, `name="op" value="save"`, "the one Save")
}

func TestAdminConfigLiftResetToAnEmptyDefault(t *testing.T) {
	view := adminLiftView()
	view.Sections[0].Essential = append(view.Sections[0].Essential, AdminConfigField{
		Key: "server.public_url", Type: "url", Value: "https://spindle.example.com", Default: "",
	})
	s := adminLiftServer(t, view)
	body := settingsDo(t, s, http.MethodGet, "/admin/config?reset=server.public_url", nil, nil).Body.String()
	settingsMustContain(t, body, "reset to its compiled default (empty)", "empty default is rendered honestly")
	settingsMustContain(t, body, `value=""`, "the field goes back to empty")
}

func TestAdminConfigLiftQueryBanners(t *testing.T) {
	s := adminLiftServer(t, adminLiftView())
	body := settingsDo(t, s, http.MethodGet, "/admin/config?notice=hello&error=bad+value", nil, nil).Body.String()
	settingsMustContain(t, body, "hello", "notice banner")
	settingsMustContain(t, body, "bad value", "error banner")
}

// TestAdminConfigLiftErrorLinkMapping pins the anchor resolution and the
// summary's edge cases directly: a struct-slice cell maps to its row editor, an
// unknown key maps to its section card, a keyless error has no link, and
// duplicates collapse.
func TestAdminConfigLiftErrorLinkMapping(t *testing.T) {
	t.Parallel()
	v := adminLiftView()
	v.Sections[1].Essential = append(v.Sections[1].Essential, AdminConfigField{Key: "server.auth.tokens", Type: "rows", Value: "", Default: ""})
	adminPrepareConfig(v)

	if got := adminAnchorFor(v, ""); got != "" {
		t.Fatalf("anchor for empty key = %q", got)
	}
	if got := adminAnchorFor(v, "server.auth.mode[0].scope"); got != "row-server.auth.mode" {
		t.Fatalf("cell key anchor = %q, want the row editor", got)
	}
	if got := adminAnchorFor(v, "no.such.key"); got != "" {
		t.Fatalf("unknown key anchor = %q, want empty", got)
	}
	adminMarkFocus(v, "")
	adminMarkFocus(v, "server.auth.mode[0].scope")
	if !v.Sections[1].Essential[0].Focus {
		t.Fatal("a cell error did not focus its row editor")
	}
	adminMarkFocus(v, "no.such.key") // no mounted row: must not panic

	// A boot-time view (no errs) builds links from the per-field errors.
	if links := adminErrorLinks(v, nil); len(links) != 0 {
		t.Fatalf("clean view links = %+v", links)
	}
	v.Sections[0].Essential[0].Errors = []AdminError{{Message: "listen is invalid"}}
	links := adminErrorLinks(v, nil)
	if len(links) != 1 || links[0].Key != "server.listen" || links[0].Href != "#row-server.listen" {
		t.Fatalf("field-error links = %+v", links)
	}
	// A boot view carries the aggregate messages too; the keyed copy supplies
	// the row link while a keyless message still renders without one.
	v.Errors = []AdminError{{Message: "listen is invalid"}, {Message: "whole file unparseable"}}
	links = adminErrorLinks(v, nil)
	if len(links) != 2 || links[0].Key != "server.listen" || links[1].Href != "" {
		t.Fatalf("boot-error links = %+v", links)
	}
	// Keyless view errors stay listed with no link.
	links = adminErrorLinks(&AdminConfigView{Errors: []AdminError{{Message: "whole file unparseable"}}}, nil)
	if len(links) != 1 || links[0].Href != "" || links[0].Message != "whole file unparseable" {
		t.Fatalf("keyless links = %+v", links)
	}
	// An unmounted key links its section; duplicates collapse.
	links = adminErrorLinks(v, []AdminError{
		{Key: "store.bucket", Message: "bucket is required"},
		{Key: "store.bucket", Message: "bucket is required"},
	})
	if len(links) != 1 || links[0].Href != "#store" {
		t.Fatalf("unmounted key links = %+v", links)
	}
}

func TestAdminConfigLiftResetSkipsSecretsAndRows(t *testing.T) {
	t.Parallel()
	v := adminLiftView()
	v.Sections[1].Essential = append(v.Sections[1].Essential,
		AdminConfigField{Key: "server.auth.session_secret", Type: "string", Secret: true, Value: "•••• (set)", Default: ""},
		AdminConfigField{Key: "server.auth.tokens", Type: "rows", Rows: []setup.Row{}, Default: ""},
	)
	adminPrepareConfig(v)
	if _, ok := adminResetConfig(v, "server.auth.session_secret"); ok {
		t.Fatal("a secret row must never reset")
	}
	if _, ok := adminResetConfig(v, "server.auth.tokens"); ok {
		t.Fatal("a row editor must never reset")
	}
	if _, ok := adminResetConfig(v, ""); ok {
		t.Fatal("an empty reset key is a no-op")
	}
}

func TestAdminConfigLiftQueryAndChangedCompose(t *testing.T) {
	view := adminLiftView()
	view.Sections[1].Essential[0].Value = "token" // server.auth.mode differs from the default
	s := adminLiftServer(t, view)
	body := settingsDo(t, s, http.MethodGet, "/admin/config?q=auth&changed=1", nil, nil).Body.String()
	settingsMustContain(t, body, `data-key="server.auth.mode"`, "the one changed auth row")
	settingsMustContain(t, body, "Showing 1 of 6 keys", "composed filter count")
	settingsMustNotContain(t, body, `data-section="server"`, "other sections narrow away")
	settingsMustContain(t, body, `name="changed" value="1"`, "toggle state survives the query")
}
