package webui

// The Configuration page's rate-limit card (PLAN-V2 §M.4/M.7): the six
// rate_limit keys render from the real setup schema with their spelled rates,
// their notes and the boolean controls, and a save posts them through
// setup.ParseValues so RestartKeys reports the change (the limiter is built at
// boot).

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/setup"
)

// rateLimitSectionView builds the page view of the [rate_limit] card from the
// real schema (the shape cmd/spindle's adminConfigViewFn produces).
func rateLimitSectionView(effective *config.Config) *AdminConfigView {
	sec := AdminConfigSection{Section: "rate_limit"}
	for _, g := range setup.BuildSchema(effective, config.Defaults()) {
		if g.Section != "rate_limit" {
			continue
		}
		for _, k := range g.Keys {
			sec.Essential = append(sec.Essential, AdminConfigField{
				Key: k.Key, Type: k.Type,
				Value:   setup.DisplayValue(k),
				Default: setup.DisplayValue(setup.Field{Type: k.Type, Value: k.Default}),
			})
		}
	}
	v := &AdminConfigView{FileState: "valid", AuthMode: "oidc", Sections: []AdminConfigSection{sec}}
	adminPrepareConfig(v)
	return v
}

func TestAdminConfigRendersRateLimitCard(t *testing.T) {
	t.Parallel()
	v := rateLimitSectionView(config.Defaults())
	if len(v.Sections) != 1 {
		t.Fatalf("sections = %d", len(v.Sections))
	}
	byKey := map[string]AdminConfigField{}
	for _, f := range append(v.Sections[0].Essential, v.Sections[0].Advanced...) {
		byKey[f.Key] = f
	}
	for key, want := range map[string]string{
		"rate_limit.collab_write": "30/min",
		"rate_limit.issue_open":   "10/h",
		"rate_limit.report":       "5/h",
		"rate_limit.auth_fail":    "10/min",
	} {
		f, ok := byKey[key]
		if !ok {
			t.Fatalf("%s row missing", key)
		}
		if f.Value != want {
			t.Fatalf("%s value = %q, want %q", key, f.Value, want)
		}
		if f.Note == "" {
			t.Fatalf("%s has no note", key)
		}
	}
	if f := byKey["rate_limit.enabled"]; f.Control != "bool" || f.Bool {
		t.Fatalf("enabled row = %+v (must render off by default)", f)
	}
	if f := byKey["rate_limit.trust_proxy"]; !f.Advanced {
		t.Fatal("trust_proxy must sit in the collapsed Advanced group")
	}
	if note := byKey["rate_limit.trust_proxy"].Note; !strings.Contains(note, "X-Forwarded-For") {
		t.Fatalf("trust_proxy note = %q", note)
	}
}

// TestAdminConfigRateLimitSaveIsRestartRequired drives the page's save: the
// posted rates coerce, validate, and report a restart (the buckets are built at
// boot).
func TestAdminConfigRateLimitSaveIsRestartRequired(t *testing.T) {
	t.Parallel()
	base := oidcDefaults()          // the pre-save effective config
	candidateBase := oidcDefaults() // a separate copy: ParseValues edits in place
	values := url.Values{}
	for _, g := range setup.BuildSchema(base, config.Defaults()) {
		for k, vs := range setup.FormValues([]setup.Group{g}) {
			for _, v := range vs {
				values.Set(k, v)
			}
		}
	}
	values.Set("rate_limit.enabled", "true")
	values.Set("rate_limit.collab_write", "45/min")
	cand, err := setup.ParseValues(values, candidateBase)
	if err != nil {
		t.Fatalf("ParseValues: %v", err)
	}
	if errs := setup.Validate(cand); len(errs) > 0 {
		t.Fatalf("validate: %v", errs)
	}
	if !cand.RateLimit.Enabled || cand.RateLimit.CollabWrite != "45/min" {
		t.Fatalf("coerced section = %+v", cand.RateLimit)
	}
	keys := strings.Join(setup.RestartKeys(base, cand), ",")
	for _, want := range []string{"rate_limit.enabled", "rate_limit.collab_write"} {
		if !strings.Contains(keys, want) {
			t.Fatalf("restart keys %q missing %s", keys, want)
		}
	}
}

// TestAdminConfigSubmitAuditsSave pins the audit-completeness requirement on
// the page's one mutation: a successful save calls the injected audit writer
// with the actor and the config_save action.
func TestAdminConfigSubmitAuditsSave(t *testing.T) {
	var got struct{ actor, action, target, detail string }
	deps := adminBaseDeps()
	deps.Save = func(ctx context.Context, values map[string]string) (AdminSaveResult, []AdminError, error) {
		return AdminSaveResult{Saved: true, RequiresRestart: []string{"rate_limit.collab_write"}}, nil, nil
	}
	deps.AuditWrite = func(ctx context.Context, actor, action, target, detail string) error {
		got.actor, got.action, got.target, got.detail = actor, action, target, detail
		return nil
	}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", url.Values{"op": {"save"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d, want 303", rec.Code)
	}
	if got.actor != "alice" || got.action != "config_save" || got.target != "instance:config" {
		t.Fatalf("audit record = %+v", got)
	}
	if !strings.Contains(got.detail, "rate_limit.collab_write") {
		t.Fatalf("audit detail = %q, want the restart list", got.detail)
	}
}
