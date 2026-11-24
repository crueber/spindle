package webui

// The Configuration page's image-policy rows (threads.image_allow_prefixes /
// threads.image_require_digest, docs/PLAN-ARTIFACT-KEEPER.md §5.2): they render
// from the real setup schema with the list/bool controls and their notes, and a
// save posts them through setup.ParseValues so RestartKeys reports the change.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/setup"
)

// threadsSectionView builds the page view of the [threads] card from the real
// schema (the shape cmd/spindle's adminConfigViewFn produces).
func threadsSectionView(effective *config.Config) *AdminConfigView {
	sec := AdminConfigSection{Section: "threads"}
	for _, g := range setup.BuildSchema(effective, config.Defaults()) {
		if g.Section != "threads" {
			continue
		}
		for _, k := range g.Keys {
			f := AdminConfigField{
				Key: k.Key, Type: k.Type,
				Value:   setup.DisplayValue(k),
				Default: setup.DisplayValue(setup.Field{Type: k.Type, Value: k.Default}),
			}
			if k.Type == "list" || k.Type == "bool" {
				sec.Essential = append(sec.Essential, f)
			} else {
				sec.Advanced = append(sec.Advanced, f)
			}
		}
	}
	v := &AdminConfigView{FileState: "valid", AuthMode: "oidc", Sections: []AdminConfigSection{sec}}
	adminPrepareConfig(v)
	return v
}

// oidcDefaults is a complete, valid configuration (OIDC is the only auth).
func oidcDefaults() *config.Config {
	c := config.Defaults()
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.OAuthClientID = "id"
	c.Server.Auth.OAuthClientSecret = "secret"
	c.Server.Auth.SessionSecret = strings.Repeat("x", 32)
	return c
}

func TestAdminConfigRendersThreadsImagePolicy(t *testing.T) {
	eff := config.Defaults()
	eff.Threads.ImageAllowPrefixes = []string{"docker.io/library", "ak.example/dockerhub"}
	eff.Threads.ImageRequireDigest = true
	v := threadsSectionView(eff)

	byKey := map[string]AdminConfigField{}
	for _, f := range append(v.Sections[0].Essential, v.Sections[0].Advanced...) {
		byKey[f.Key] = f
	}
	allow, ok := byKey["threads.image_allow_prefixes"]
	if !ok || allow.Control != "list" {
		t.Fatalf("image_allow_prefixes row = %+v", allow)
	}
	if allow.Value != "docker.io/library, ak.example/dockerhub" || allow.Default != "" {
		t.Fatalf("allowlist value/default = %q / %q", allow.Value, allow.Default)
	}
	if !strings.Contains(allow.Note, "refused at creation") {
		t.Fatalf("allowlist note = %q", allow.Note)
	}
	digest, ok := byKey["threads.image_require_digest"]
	if !ok || digest.Control != "bool" || !digest.Bool {
		t.Fatalf("image_require_digest row = %+v", digest)
	}
	if !digest.Advanced {
		t.Fatal("image_require_digest must stay in the collapsed Advanced group")
	}
	if !strings.Contains(digest.Note, "@sha256:") {
		t.Fatalf("digest note = %q", digest.Note)
	}

	f := &settingsFakes{deps: testDeps{ConfigView: func(context.Context) (*AdminConfigView, error) { return v, nil }}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="cfg.threads.image_allow_prefixes"`,
		`value="docker.io/library, ak.example/dockerhub"`,
		`name="cfg.threads.image_require_digest"`,
		`data-section="threads"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page misses %q", want)
		}
	}
	if !strings.Contains(body, "refused at creation") {
		t.Fatal("the allowlist note did not render")
	}
}

func TestAdminConfigSavesThreadsImagePolicy(t *testing.T) {
	base := oidcDefaults()
	effective := oidcDefaults()
	v := threadsSectionView(base)

	var saved map[string]string
	var restart []string
	f := &settingsFakes{deps: testDeps{
		ConfigView: func(context.Context) (*AdminConfigView, error) { return v, nil },
		Save: func(_ context.Context, values map[string]string) (AdminSaveResult, []AdminError, error) {
			saved = values
			form := url.Values{}
			for k, val := range values {
				form.Set(k, val)
			}
			cand, err := setup.ParseValues(form, base)
			if err != nil {
				return AdminSaveResult{}, []AdminError{{Key: setup.KeyOf(err), Message: err.Error()}}, nil
			}
			restart = setup.RestartKeys(effective, cand)
			return AdminSaveResult{Saved: true, RequiresRestart: restart}, nil, nil
		},
	}}
	s := newSettingsServer(t, f)
	form := url.Values{
		"op":                               {"save"},
		"cfg.threads.image_allow_prefixes": {"docker.io/library, ak.example/dockerhub"},
		"cfg.threads.image_require_digest": {"true"},
	}
	rec := settingsDo(t, s, http.MethodPost, "/admin/config", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d (%s)", rec.Code, rec.Body.String())
	}
	if saved["threads.image_allow_prefixes"] != "docker.io/library, ak.example/dockerhub" {
		t.Fatalf("saved values = %v", saved)
	}
	if saved["threads.image_require_digest"] != "true" {
		t.Fatalf("saved values = %v", saved)
	}
	joined := strings.Join(restart, ",")
	for _, want := range []string{"threads.image_allow_prefixes", "threads.image_require_digest"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("RestartKeys = %q, missing %s", joined, want)
		}
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "restart=") || !strings.Contains(loc, "threads.image_allow_prefixes") {
		t.Fatalf("redirect = %q", loc)
	}
}
