// admin_config_pointer_test.go — N2 of the PR #757 review: an unset pointer
// scalar (*bool) must render its EFFECTIVE value. A nil *bool means the
// compiled default (config.Search.Enabled: nil = enabled), so the row must not
// show an off switch labelled "disabled" for something that is enabled — and
// the state chip must stay honest ("default", not "file").
package webui

import (
	"net/http"
	"testing"
)

func TestAdminConfigNilBoolRendersEffectiveDefault(t *testing.T) {
	view := &AdminConfigView{
		FileState: "valid",
		AuthMode:  "none",
		TOML:      "[search]\n",
		Sections: []AdminConfigSection{
			{Section: "search", Essential: []AdminConfigField{
				// A nil *bool: the producer renders the schema's empty value.
				{Key: "search.enabled", Type: "bool", Value: "", Default: "true"},
			}},
		},
	}
	s := adminLiftServer(t, view)
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()

	settingsMustContain(t, body, `data-key="search.enabled"`, "nil-pointer row renders")
	// The switch is on, so the state label and the posted value are honest.
	settingsMustContain(t, body,
		`id="admin-in-search--enabled" name="cfg.search.enabled" value="true" class="h-5 w-5 shrink-0 check" checked`,
		"an unset *bool renders the switch on (nil = the compiled default)")
	settingsMustContain(t, body, `>enabled</span>`, "the row says enabled, not disabled")
	// The chip says "default": the value came from the compiled default, not
	// from the file.
	settingsMustContain(t, body, `>default</span>`, "an unset pointer is at the default")
	settingsMustNotContain(t, body, `chip-neutral">file</span>`, "an unset pointer is not a file value")
}

func TestAdminConfigExplicitBoolStillRendersItsValue(t *testing.T) {
	view := &AdminConfigView{
		FileState: "valid",
		AuthMode:  "none",
		TOML:      "[server]\nhttp2 = false\n",
		Sections: []AdminConfigSection{
			{Section: "server", Essential: []AdminConfigField{
				{Key: "server.http2", Type: "bool", Value: "false", Default: "true"},
			}},
		},
	}
	s := adminLiftServer(t, view)
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	// The seeding is only for the unset case: an explicit false stays off and
	// keeps its "file" chip.
	settingsMustContain(t, body,
		`id="admin-in-server--http2" name="cfg.server.http2" value="true" class="h-5 w-5 shrink-0 check">`,
		"an explicit false stays off")
	settingsMustContain(t, body, `>disabled</span>`, "the row says disabled")
	settingsMustContain(t, body, `chip-neutral">file</span>`, "an explicit false is a file value")
}

func TestAdminConfigBoolWithoutDefaultStaysOff(t *testing.T) {
	// The default must never be invented: a bool row with no compiled default
	// and no value stays blank/off.
	view := &AdminConfigView{Sections: []AdminConfigSection{{Section: "search", Essential: []AdminConfigField{
		{Key: "search.enabled", Type: "bool"},
	}}}}
	s := adminLiftServer(t, view)
	body := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil).Body.String()
	settingsMustContain(t, body, `id="admin-in-search--enabled"`, "row renders")
	settingsMustNotContain(t, body,
		`id="admin-in-search--enabled" name="cfg.search.enabled" value="true" class="h-5 w-5 shrink-0 check" checked`,
		"no default means an off switch")
}

// TestAdminConfigNilBoolViewIsPrepared pins the mutation point itself: the
// producer (cmd/spindle) sends the raw blank value; adminPrepareConfig is what
// seeds it from Default before Bool/FromFile are derived.
func TestAdminConfigNilBoolViewIsPrepared(t *testing.T) {
	v := &AdminConfigView{Sections: []AdminConfigSection{{Section: "search", Essential: []AdminConfigField{
		{Key: "search.enabled", Type: "bool", Value: "", Default: "true"},
	}}}}
	adminPrepareConfig(v)
	f := v.Sections[0].Essential[0]
	if f.Value != "true" || !f.Bool {
		t.Fatalf("prepared row = value %q bool %v, want true/true", f.Value, f.Bool)
	}
	if f.FromFile {
		t.Fatal("an unset pointer must not count as a file value")
	}
	if v.ChangedKeys != 0 {
		t.Fatalf("changed keys = %d, want 0", v.ChangedKeys)
	}
	// Idempotent: preparing twice keeps the effective value.
	adminPrepareConfig(v)
	if f := v.Sections[0].Essential[0]; f.Value != "true" || !f.Bool {
		t.Fatalf("second prepare changed the row: %+v", f)
	}
}
