// settings_scheduled_test.go — WP9 G6/G14/G15/G16: the Scheduled editors,
// the webhook edit/rotate controls, policy removal and the Settings → WAL
// panel. The page-driven cases extract the rendered form (the exact payload a
// browser posts) and assert both the round trip and the "nothing written on
// failure" guarantee.

package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server/auth"
)

const scheduledDoc = `description = "keep me"

[features]
issues = false

[bundles]
min_commits = 2
min_bytes = "8MiB"
main_only = true
serve_via = "proxy"
signed_url_ttl = "2h"
signed_url_for = ["refs/heads/main"]
extra_refs = ["refs/tags/*"]
advertise = true
advertise_filtered = false
require = ["bundle-uri"]

[[bundles.strategy]]
name = "daily"
kind = "full"
schedule = "0 0 3 * * *"
keep = 3
backfill_max = 1
min_commits = 2
filter = "blob:none"
refs = ["refs/heads/main"]

[maintenance]
interval = "7m"
checkpoints = true
max_pack_bytes = "2GiB"
disk = "ssd"
host = "node-1"
fsck_interval = "12h"
follow_interval = "3m"

[compaction]
enabled = true
factor = 4
trigger_packs = 8
trigger_bytes = "256MiB"
lease_ttl = "2m"
retention_superseded = "24h"
engine = "git"

[upstream]
git = "https://up.example/x/y.git"
lfs = "https://up.example/x/y.git/info/lfs"
follow = ["refs/heads/*"]
`

// settingsExtractForm returns the values a browser submits for the first form
// whose action contains m[0]atch: inputs (checked checkboxes only), the chosen
// option of each select, and textareas.
func settingsExtractForm(t *testing.T, body, actionMatch string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && n.Data == "form" &&
			strings.Contains(settingsNodeAttr(n, "action"), actionMatch) {
			return n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if got := find(c); got != nil {
				return got
			}
		}
		return nil
	}
	form := find(doc)
	if form == nil {
		t.Fatalf("no form with action %q", actionMatch)
	}
	vals := url.Values{}
	var collect func(*html.Node)
	collect = func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.Data {
		case "input":
			name := settingsNodeAttr(n, "name")
			if name != "" {
				switch settingsNodeAttr(n, "type") {
				case "checkbox", "radio":
					if settingsNodeHas(n, "checked") {
						v := settingsNodeAttr(n, "value")
						if v == "" {
							v = "on"
						}
						vals.Add(name, v)
					}
				default:
					vals.Add(name, settingsNodeAttr(n, "value"))
				}
			}
			return
		case "textarea":
			if name := settingsNodeAttr(n, "name"); name != "" {
				vals.Add(name, settingsNodeText(n))
			}
			return
		case "select":
			name := settingsNodeAttr(n, "name")
			if name == "" {
				return
			}
			chosen := ""
			first := true
			var options func(*html.Node)
			options = func(o *html.Node) {
				if o.Type == html.ElementNode && o.Data == "option" {
					v := settingsNodeAttr(o, "value")
					if first {
						chosen, first = v, false
					}
					if settingsNodeHas(o, "selected") {
						chosen = v
					}
					return
				}
				for c := o.FirstChild; c != nil; c = c.NextSibling {
					options(c)
				}
			}
			options(n)
			vals.Add(name, chosen)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	collect(form)
	return vals
}

func settingsNodeAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func settingsNodeHas(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func settingsNodeText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func TestSettingsScheduledEditorsRoundTrip(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc.TOML = scheduledDoc
	h.repo.doc.Revision = 5

	rec := settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/scheduled", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET scheduled = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{
		`value="8MiB"`, `value="2GiB"`, `value="256MiB"`, `value="7m"`, `value="3m"`,
		`value="daily"`, `value="refs/heads/*"`, `name="bundles.override"`, `name="compaction.override"`,
	} {
		settingsMustContain(t, page, want, "scheduled page")
	}

	form := settingsExtractForm(t, page, "/settings/scheduled")
	if form.Get("bundles.override") != "true" || form.Get("upstream.override") != "true" {
		t.Fatalf("stored overrides not checked: bundles=%q upstream=%q", form.Get("bundles.override"), form.Get("upstream.override"))
	}

	// Unchanged submit: byte-for-byte the same sections, and the passenger
	// keys ([features]) ride along untouched.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST scheduled = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(h.repo.published) == 0 {
		t.Fatal("nothing was published")
	}
	rs, err := config.ParseRepoSettings(h.repo.published)
	if err != nil {
		t.Fatalf("published document does not parse: %v\n%s", err, h.repo.published)
	}
	if rs.Description != "keep me" {
		t.Fatalf("description changed: %q", rs.Description)
	}
	if rs.Features == nil || rs.Features.Issues == nil || *rs.Features.Issues {
		t.Fatalf("features passenger changed: %+v", rs.Features)
	}
	if rs.Bundles == nil || rs.Bundles.MinCommits != 2 || rs.Bundles.MinBytes != config.ByteSize(8<<20) {
		t.Fatalf("bundles changed: %+v", rs.Bundles)
	}
	if len(rs.Bundles.Strategy) != 1 || rs.Bundles.Strategy[0].Keep != 3 ||
		rs.Bundles.Strategy[0].Schedule != "0 0 3 * * *" || len(rs.Bundles.Strategy[0].Refs) != 1 {
		t.Fatalf("strategies changed: %+v", rs.Bundles.Strategy)
	}
	if rs.Maintenance == nil || rs.Maintenance.Interval.String() != "7m0s" || rs.Maintenance.Host != "node-1" {
		t.Fatalf("maintenance changed: %+v", rs.Maintenance)
	}
	if rs.Compaction == nil || rs.Compaction.Factor != 4 || rs.Compaction.TriggerBytes != config.ByteSize(256<<20) {
		t.Fatalf("compaction changed: %+v", rs.Compaction)
	}
	if rs.Upstream == nil || rs.Upstream.Git != "https://up.example/x/y.git" || len(rs.Upstream.Follow) != 1 {
		t.Fatalf("upstream changed: %+v", rs.Upstream)
	}

	// Row edit + add: row 0 keep 3→5, and a second full strategy in the add row.
	form.Set("bundles.strategy#0.keep", "5")
	form.Set("bundles.strategy#1.name", "weekly")
	form.Set("bundles.strategy#1.kind", "full")
	form.Set("bundles.strategy#1.schedule", "0 0 23 * * 0")
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST row edit = %d (%s)", rec.Code, rec.Body.String())
	}
	rs, err = config.ParseRepoSettings(h.repo.published)
	if err != nil {
		t.Fatalf("row edit document: %v", err)
	}
	if len(rs.Bundles.Strategy) != 2 || rs.Bundles.Strategy[0].Keep != 5 || rs.Bundles.Strategy[1].Name != "weekly" {
		t.Fatalf("row edit strategies = %+v", rs.Bundles.Strategy)
	}

	// Remove row 0 through its remove control.
	form.Set("bundles.strategy#0.__remove", "true")
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST row remove = %d (%s)", rec.Code, rec.Body.String())
	}
	rs, err = config.ParseRepoSettings(h.repo.published)
	if err != nil {
		t.Fatalf("row remove document: %v", err)
	}
	if len(rs.Bundles.Strategy) != 1 || rs.Bundles.Strategy[0].Name != "weekly" {
		t.Fatalf("after remove strategies = %+v", rs.Bundles.Strategy)
	}
}

func TestSettingsScheduledUncheckRemovesOverride(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc.TOML = scheduledDoc

	rec := settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/scheduled", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET scheduled = %d", rec.Code)
	}
	form := settingsExtractForm(t, rec.Body.String(), "/settings/scheduled")
	form.Del("bundles.override")
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST scheduled = %d (%s)", rec.Code, rec.Body.String())
	}
	rs, err := config.ParseRepoSettings(h.repo.published)
	if err != nil {
		t.Fatalf("published document: %v\n%s", err, h.repo.published)
	}
	if rs.Bundles != nil {
		t.Fatalf("bundles override survived an unchecked save: %+v", rs.Bundles)
	}
	if rs.Maintenance == nil || rs.Compaction == nil {
		t.Fatalf("untouched sections were dropped: %+v", rs)
	}
}

func TestSettingsScheduledValidationFailureWritesNothing(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc.TOML = scheduledDoc

	rec := settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/scheduled", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET scheduled = %d", rec.Code)
	}
	form := settingsExtractForm(t, rec.Body.String(), "/settings/scheduled")
	form.Set("compaction.trigger_bytes", "notasize")
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad size = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if len(h.repo.published) != 0 {
		t.Fatalf("a rejected edit was published: %s", h.repo.published)
	}
	settingsMustContain(t, rec.Body.String(), `id="compaction.trigger_bytes"`, "marked row")
	settingsMustContain(t, rec.Body.String(), `value="notasize"`, "preserved edit")
	settingsMustContain(t, rec.Body.String(), "err-line", "inline error")

	// A cross-field validation error (duplicate strategy names) names the row.
	form.Set("compaction.trigger_bytes", "256MiB")
	form.Set("bundles.strategy#1.name", "daily")
	form.Set("bundles.strategy#1.kind", "full")
	form.Set("bundles.strategy#1.schedule", "0 0 1 * * *")
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/scheduled", form, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate strategy = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if len(h.repo.published) != 0 {
		t.Fatalf("invalid document published: %s", h.repo.published)
	}
	settingsMustContain(t, rec.Body.String(), "duplicates", "validation message")
}

func TestSettingsScheduledRequiresAdmin(t *testing.T) {
	f := settingsRepoFakes("write")
	f.deps.RepoRole = func(context.Context, string, string, auth.Principal) (string, error) {
		return "write", nil
	}
	s := newSettingsServer(t, f)

	called := false
	f.deps.RepoWrite = func(context.Context, string, string, auth.Principal, SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
		called = true
		return SettingsWriteResult{}, nil
	}
	BindSettings(f.deps)
	for _, tab := range []string{"scheduled", "webhooks", "policy"} {
		if rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("write role GET %s = %d, want 403", tab, rec.Code)
		}
		rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/"+tab, url.Values{
			"bundles.override": {"true"}, "confirm": {"acme/monorepo"},
		}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("write role POST %s = %d, want 403", tab, rec.Code)
		}
	}
	if called {
		t.Fatal("the write seam was reached by a non-admin")
	}
}

func TestSettingsWebhookEditKeepsSecretAndRotatesItFirstClass(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	hookURL := "https://example.com/hook"
	secret := "old-secret"
	hk, err := h.notes.CreateHook(ctx, "acme", "monorepo", "alice", notify.HookSpec{
		URL: &hookURL, Events: []string{"opened"}, Secret: &secret,
	})
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}

	newURL := "https://example.com/hook2"
	rec := settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/webhooks/"+hk.ID, url.Values{
		"webhook_action": {"update"},
		"url":            {newURL},
		"events":         {"opened, closed"},
		"active":         {"false", "true"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update hook = %d (%s)", rec.Code, rec.Body.String())
	}
	got := h.notes.GetHook(ctx, "acme", "monorepo", hk.ID)
	if got == nil || got.URL != newURL || !got.Active || len(got.Events) != 2 {
		t.Fatalf("hook after update = %+v", got)
	}
	if got.Secret != secret {
		t.Fatalf("update changed the secret: %q", got.Secret)
	}

	// Rotate is a separate control: a blank secret is a validation error.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/webhooks/"+hk.ID, url.Values{
		"webhook_action": {"rotate"},
		"secret":         {""},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank rotation = %d, want 422", rec.Code)
	}
	if got := h.notes.GetHook(ctx, "acme", "monorepo", hk.ID); got.Secret != secret {
		t.Fatalf("blank rotation changed the secret: %q", got.Secret)
	}

	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/webhooks/"+hk.ID, url.Values{
		"webhook_action": {"rotate"},
		"secret":         {"new-secret"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate = %d (%s)", rec.Code, rec.Body.String())
	}
	got = h.notes.GetHook(ctx, "acme", "monorepo", hk.ID)
	if got.Secret != "new-secret" || got.URL != newURL || len(got.Events) != 2 {
		t.Fatalf("hook after rotation = %+v", got)
	}

	// The rendered row exposes both forms (edit + first-class rotation).
	rec = settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/webhooks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET webhooks = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Rotate signing secret", "rotation control")
	settingsMustContain(t, rec.Body.String(), `value="update"`, "edit action")
}

func TestSettingsPolicyRemovalAndEmptyEditor(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "monorepo"}
	doc := []byte(`{"version":1,"groups":[],"rules":[]}`)
	if werr := api.PutPolicy(ctx, h.apiE, id, doc); werr != nil {
		t.Fatalf("seed policy: %+v", werr)
	}

	rec := settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/policy", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET policy = %d", rec.Code)
	}
	page := rec.Body.String()
	settingsMustContain(t, page, "Remove policy", "removal control")
	settingsMustContain(t, page, `data-danger-expected="acme/monorepo"`, "typed confirmation")

	// Wrong typed confirmation: nothing deleted.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/policy", url.Values{
		"policy_action": {"remove"}, "confirm": {"acme/mono"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong confirm = %d, want 422", rec.Code)
	}
	if raw, ver, _ := api.PolicyRaw(ctx, h.apiE, id); ver == "" || len(raw) == 0 {
		t.Fatal("a rejected confirmation deleted the policy")
	}

	// Correct confirmation removes it.
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/policy", url.Values{
		"policy_action": {"remove"}, "confirm": {"acme/monorepo"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirmed remove = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ver, _ := api.PolicyRaw(ctx, h.apiE, id); ver != "" {
		t.Fatal("the policy document survived removal")
	}

	// An emptied editor is removal, not a 400.
	if werr := api.PutPolicy(ctx, h.apiE, id, doc); werr != nil {
		t.Fatalf("re-seed policy: %+v", werr)
	}
	rec = settingsDo(t, h.s, http.MethodPost, "/acme/monorepo/settings/policy", url.Values{
		"policy": {""},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("empty editor save = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ver, _ := api.PolicyRaw(ctx, h.apiE, id); ver != "" {
		t.Fatal("an emptied editor did not remove the policy")
	}
}

func TestSettingsSeamsWALPanelReadsOverview(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/wal")
	if code != http.StatusOK {
		t.Fatalf("GET wal = %d", code)
	}
	settingsMustContain(t, body, "WAL health", "wal panel")
	settingsMustContain(t, body, "Manifest", "manifest facts")
	settingsMustContain(t, body, "the full WAL view", "link to the WAL page")

	// With a task table the panel renders the run forms; a broken overview
	// leaves the honest error banner instead of a fabricated panel.
	h.apiE.Tasks = scheduledTasks{}
	code, body = seamsGet(t, h, "/acme/monorepo/settings/wal")
	if code != http.StatusOK || !strings.Contains(body, `data-op-url="/acme/monorepo/api/ops/fsck"`) {
		t.Fatalf("wal run forms = %d, body has form: %v", code, strings.Contains(body, "data-op-run"))
	}
	h.repo.overviewErr = errors.New("overview down")
	code, body = seamsGet(t, h, "/acme/monorepo/settings/wal")
	if code != http.StatusOK || !strings.Contains(body, "overview down") {
		t.Fatalf("wal overview error = %d (%s)", code, body)
	}
}

// scheduledTasks is a minimal api.Tasks table for the WAL panel's read path.
type scheduledTasks struct{}

func (scheduledTasks) Ops() []api.OpSpec {
	return []api.OpSpec{{Op: "fsck", Params: []api.OpParam{{Name: "connectivity", Values: []string{"1"}}}}}
}
func (scheduledTasks) List(context.Context, git.RepoId) ([]api.TaskRecord, []api.TaskRecord, error) {
	return nil, []api.TaskRecord{{Kind: "fsck", Summary: "clean"}}, nil
}
func (scheduledTasks) Get(context.Context, git.RepoId, string) (api.TaskRecord, bool, error) {
	return api.TaskRecord{}, false, nil
}
func (scheduledTasks) Begin(context.Context, git.RepoId, string, map[string]string) (api.TaskStream, error) {
	return api.TaskStream{}, nil
}
func (scheduledTasks) Attach(context.Context, git.RepoId, string) (api.TaskStream, bool, error) {
	return api.TaskStream{}, false, nil
}

func TestSettingsScheduledCoercionErrors(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc.TOML = scheduledDoc
	ctx := context.Background()

	base := func() map[string]string {
		page := settingsScheduledSections(nil, h.apiE.Cfg, nil)
		values := map[string]string{"scheduled_form": "1"}
		for _, sec := range page {
			values[sec.ID+".override"] = "true"
			for _, f := range sec.Fields {
				values[f.Name] = f.Value
			}
			for _, row := range sec.Rows {
				values[row.BaseKey] = strconv.Itoa(row.Base)
				for _, c := range row.Cells {
					values[c.Name] = c.Value
				}
			}
		}
		return values
	}
	for _, tc := range []struct {
		name, key, value, wantField string
	}{
		{"bad size", "bundles.min_bytes", "nope", "bundles.min_bytes"},
		{"bad duration", "maintenance.interval", "soonish", "maintenance.interval"},
		{"bad int", "compaction.factor", "many", "compaction.factor"},
		{"bad bool", "bundles.advertise", "maybe", "bundles.advertise"},
		{"bad extra size", "compaction.trigger_bytes", "-1", "compaction.trigger_bytes"},
		{"bad strategy int", "bundles.strategy#0.keep", "lots", "bundles.strategy#0.keep"},
		{"blank int", "bundles.min_commits", "", "bundles.min_commits"},
		{"blank size", "compaction.trigger_bytes", "", "compaction.trigger_bytes"},
		{"blank duration", "maintenance.interval", "", "maintenance.interval"},
		{"bad bundle bool", "bundles.main_only", "maybe", "bundles.main_only"},
		{"bad bundle ttl", "bundles.signed_url_ttl", "soon", "bundles.signed_url_ttl"},
		{"bad advertise filtered", "bundles.advertise_filtered", "maybe", "bundles.advertise_filtered"},
		{"bad checkpoints", "maintenance.checkpoints", "maybe", "maintenance.checkpoints"},
		{"bad max pack", "maintenance.max_pack_bytes", "nope", "maintenance.max_pack_bytes"},
		{"bad fsck interval", "maintenance.fsck_interval", "nope", "maintenance.fsck_interval"},
		{"bad follow interval", "maintenance.follow_interval", "nope", "maintenance.follow_interval"},
		{"bad compaction enabled", "compaction.enabled", "maybe", "compaction.enabled"},
		{"bad trigger packs", "compaction.trigger_packs", "x", "compaction.trigger_packs"},
		{"bad lease", "compaction.lease_ttl", "x", "compaction.lease_ttl"},
		{"bad retention", "compaction.retention_superseded", "x", "compaction.retention_superseded"},
		{"bad backfill", "bundles.strategy#0.backfill_max", "x", "bundles.strategy#0.backfill_max"},
		{"bad row min commits", "bundles.strategy#0.min_commits", "x", "bundles.strategy#0.min_commits"},
	} {
		values := base()
		values[tc.key] = tc.value
		_, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", seamAdmin(), SettingsRepoWrite{
			Action: SettingsActionScheduled, Values: values, Multi: map[string][]string{},
		})
		if werr == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		if werr.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d", tc.name, werr.Status)
		}
		if werr.Field != tc.wantField {
			t.Fatalf("%s: field = %q, want %q (%s)", tc.name, werr.Field, tc.wantField, werr.Message)
		}
	}
	if len(h.repo.published) != 0 {
		t.Fatalf("a rejected coercion published: %s", h.repo.published)
	}

	// An empty POST (no form marker) is refused, never "uncheck everything".
	noForm := base()
	delete(noForm, "scheduled_form")
	if _, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", seamAdmin(), SettingsRepoWrite{
		Action: SettingsActionScheduled, Values: noForm, Multi: map[string][]string{},
	}); werr == nil || !strings.Contains(werr.Message, "incomplete") {
		t.Fatalf("missing form marker = %+v", werr)
	}

	// A publish failure is reported, never swallowed.
	h.repo.publishErr = errors.New("wal down")
	_, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", seamAdmin(), SettingsRepoWrite{
		Action: SettingsActionScheduled, Values: base(), Multi: map[string][]string{},
	})
	if werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("publish failure = %+v", werr)
	}
}

func TestSettingsScheduledBrokenDocumentAndEmptyForm(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc.TOML = "not = [valid"
	code, body := seamsGet(t, h, "/acme/monorepo/settings/scheduled")
	if code != http.StatusOK {
		t.Fatalf("broken doc page = %d", code)
	}
	settingsMustContain(t, body, "repo settings", "the broken document is reported, never rendered as empty editors")

	// A doc that decodes but fails publish-internal coercion (unknown key) is
	// rejected without publishing.
	h.repo.doc.TOML = "[bundles]\nunknown_knob = 1\n"
	code, _, _ = seamsPost(t, h, "/acme/monorepo/settings/scheduled", url.Values{
		"scheduled_form":   {"1"},
		"bundles.override": {"true"}, "bundles.min_commits": {"1"}, "bundles.min_bytes": {"1MiB"},
		"bundles.main_only": {"false"}, "bundles.serve_via": {"proxy"},
		"bundles.signed_url_ttl": {"1h"}, "bundles.signed_url_for": {""},
		"bundles.extra_refs": {""}, "bundles.advertise": {"false"},
		"bundles.advertise_filtered": {"false"}, "bundles.require": {""},
		"bundles.strategy#0.__base": {"-1"},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unknown stored key = %d, want 503 (the stored doc is broken)", code)
	}
	if len(h.repo.published) != 0 {
		t.Fatalf("published despite an unknown stored key: %s", h.repo.published)
	}
}

func TestSettingsScheduledHelpers(t *testing.T) {
	// section/field value projections answer "" for an unknown key (never a
	// panic) and the select helper reports an off-enum current value.
	if settingsBundlesValue(config.Bundles{}, "nope") != "" ||
		settingsMaintenanceValue(config.Maintenance{}, "nope") != "" ||
		settingsCompactionValue(config.Compaction{}, "nope") != "" ||
		settingsUpstreamValue(config.Upstream{}, "nope") != "" {
		t.Fatal("unknown key projection was not empty")
	}
	if !(SettingsField{Value: "x", Enum: []string{"a"}}).Custom() ||
		(SettingsField{Value: "a", Enum: []string{"a"}}).Custom() ||
		(SettingsField{Value: ""}).Custom() {
		t.Fatal("Custom enum detection is wrong")
	}
	if cellKeyName("nodot") != "nodot" || cellKeyName("a.b.c") != "c" {
		t.Fatal("cellKeyName")
	}
	if got := settingsValidationField("bundles.strategy[2].name is required"); got != "bundles.strategy#2.name" {
		t.Fatalf("validation field = %q", got)
	}
	if got := settingsValidationField("maintenance.interval must be > 0"); got != "maintenance.interval" {
		t.Fatalf("validation field = %q", got)
	}
	if got := settingsValidationField("something else"); got != "" {
		t.Fatalf("keyless validation field = %q", got)
	}
	if got := settingsValidationField("bundles.strategy[0]"); got == "" {
		t.Fatal("a malformed strategy reference still marks the section")
	}
	if settingsListValue(" a, ,b ") == nil || len(settingsListValue(" a, ,b ")) != 2 {
		t.Fatal("settingsListValue")
	}

	// A form with no row keys at all still renders the blank add row.
	rows := settingsStrategyRowsFromForm(map[string]string{})
	if len(rows) != 1 || rows[0].Base != -1 || rows[0].Caption != "new row" {
		t.Fatalf("empty form rows = %+v", rows)
	}
	if settingsScheduledSections(nil, nil, nil) != nil {
		t.Fatal("a nil effective config must render no editors")
	}
	// Malformed row keys are ignored rather than panicking.
	malformed := settingsStrategyRowsFromForm(map[string]string{
		"bundles.strategy#": "x", "bundles.strategy#x.name": "y", "bundles.strategy#0": "z",
	})
	if len(malformed) != 1 {
		t.Fatalf("malformed rows = %+v", malformed)
	}
	// Removed rows disappear; posted values survive the overlay.
	sections := settingsScheduledSections(nil, config.Defaults(), nil)
	page := &SettingsRepoPage{Scheduled: &SettingsScheduled{Sections: sections}}
	settingsOverlayScheduledForm(page, SettingsRepoWrite{Values: map[string]string{
		"bundles.override":            "true",
		"bundles.min_commits":         "9",
		"bundles.strategy#0.__remove": "true",
	}}, &SettingsWriteError{Field: "bundles.min_commits", Message: "bad"})
	if page.Scheduled.Sections[0].Fields[0].Value != "9" {
		t.Fatalf("overlay lost the posted value: %+v", page.Scheduled.Sections[0].Fields[0])
	}
	if page.Scheduled.Sections[0].FieldErrors["bundles.min_commits"] != "bad" {
		t.Fatalf("overlay did not mark the field: %+v", page.Scheduled.Sections[0].FieldErrors)
	}
	settingsOverlayScheduledForm(&SettingsRepoPage{}, SettingsRepoWrite{}, nil)

	// Section/field overlays and the error marker's three destinations.
	if sec := settingsSectionOf(scheduledSections[0], true, map[string]string{"bundles.override": "false"}); sec.Override || !sec.Inherited {
		t.Fatalf("section overlay = %+v", sec)
	}
	if f := settingsFieldOf("bundles", scheduledSections[0].fields[0], "1", map[string]string{"bundles.min_commits": "2"}); f.Value != "2" {
		t.Fatalf("field overlay = %+v", f)
	}
	mapped := settingsScheduledSections(nil, config.Defaults(), map[string]string{
		"bundles.strategy#0.name": "posted", "bundles.strategy#0.__base": "0",
	})
	if len(mapped[0].Rows) == 0 || mapped[0].Rows[0].Caption != "posted" {
		t.Fatalf("row overlay = %+v", mapped[0].Rows)
	}
	mark := settingsScheduledSections(nil, config.Defaults(), nil)
	settingsMarkScheduledError(mark, "", "ignored")
	settingsMarkScheduledError(mark, "compaction", "boom")
	settingsMarkScheduledError(mark, "upstream.git", "bad url")
	settingsMarkScheduledError(mark, "nope", "ignored")
	if mark[2].Error != "boom" || mark[3].FieldErrors["upstream.git"] != "bad url" {
		t.Fatalf("error markers = %+v / %+v", mark[2], mark[3].FieldErrors)
	}
}

func TestSettingsWALPanelRunControls(t *testing.T) {
	f := settingsRepoFakes("read")
	f.principal = auth.Principal{Name: "bob", Email: "bob@example.com", Write: true}
	f.deps.RepoRole = func(context.Context, string, string, auth.Principal) (string, error) { return "read", nil }
	f.deps.RepoWAL = func(context.Context, string, string) (*SettingsWAL, error) {
		return &SettingsWAL{
			Owner: "acme", Name: "monorepo", Health: "ok", HealthTone: "chip chip-open",
			Facts: []SettingsKV{{Key: "entries", Value: "9"}}, Packs: "2 live packs",
			Panel: walOpsPanel{Ops: []walOp{{Op: "fsck"}}, OpsURL: "/acme/monorepo/api/ops/"},
		}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/wal", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET wal = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "entries", "page fact")
	settingsMustContain(t, body, "requires the write role", "read-only note")
	if strings.Contains(body, "data-op-run") {
		t.Fatal("a reader was offered a run form")
	}

	// A writer gets one form per op pointing at the existing API stream.
	f.deps.RepoRole = func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil }
	f.deps.RepoWAL = func(context.Context, string, string) (*SettingsWAL, error) {
		return &SettingsWAL{Owner: "acme", Name: "monorepo", Health: "ok", HealthTone: "chip chip-open",
			Panel: walOpsPanel{
				Ops: []walOp{
					{Op: "fsck", Params: []walParam{{Name: "connectivity", Kind: "checkbox", Values: []string{"1"}}}},
					{Op: "bundle", Params: []walParam{{Name: "strategy", Kind: "text", Required: true}}},
				},
				OpsURL: "/acme/monorepo/api/ops/", CanRun: true,
			}}, nil
	}
	BindSettings(f.deps)
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/wal", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET wal as writer = %d", rec.Code)
	}
	body = rec.Body.String()
	for _, want := range []string{
		`data-op-url="/acme/monorepo/api/ops/fsck"`,
		`data-op-url="/acme/monorepo/api/ops/bundle"`,
		`name="connectivity"`,
		`name="strategy"`,
		`data-op-log`,
		`data-op-task`,
	} {
		settingsMustContain(t, body, want, "writer WAL panel")
	}
}
