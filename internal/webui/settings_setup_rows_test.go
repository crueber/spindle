package webui

// settings_setup_rows_test.go — the struct-slice row editor (G3/G4):
// server.auth.tokens and bundles.strategy render as repeating rows, and a
// rejected submit re-renders the operator's typed cells without ever echoing a
// typed secret.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/setup"
)

func settingsRowsField() SettingsSetupField {
	cells := func(i int, principal, token string, write bool) []setup.RowCell {
		return []setup.RowCell{
			{Name: "principal", Key: setup.RowCellKey("server.auth.tokens", i, "principal"), Label: "Principal", Type: "string", Value: principal},
			{Name: "token", Key: setup.RowCellKey("server.auth.tokens", i, "token"), Label: "Token", Type: "string", Secret: true, Value: token},
			{Name: "token_env", Key: setup.RowCellKey("server.auth.tokens", i, "token_env"), Label: "Token env", Type: "string"},
			{Name: "write", Key: setup.RowCellKey("server.auth.tokens", i, "write"), Label: "Write", Type: "bool", Bool: write},
			{Name: "admin", Key: setup.RowCellKey("server.auth.tokens", i, "admin"), Label: "Admin", Type: "bool"},
		}
	}
	f := SettingsSetupField{
		Key: "server.auth.tokens", Type: "rows", FromFile: true,
		Rows: []setup.Row{
			{Index: 0, Base: 0, BaseKey: setup.RowBaseKey("server.auth.tokens", 0), Caption: "alice@example.com",
				RemoveKey: setup.RowRemoveKey("server.auth.tokens", 0), Cells: cells(0, "alice@example.com", setup.SecretMask, true)},
			{Index: 1, Base: -1, BaseKey: setup.RowBaseKey("server.auth.tokens", 1), Caption: "new row",
				RemoveKey: setup.RowRemoveKey("server.auth.tokens", 1), Cells: cells(1, "", "", false)},
		},
	}
	return f
}

func settingsRowsView() *SettingsSetupView {
	v := settingsFakeView()
	v.Sections = append(v.Sections, SettingsSetupSection{Section: "auth", Essential: []SettingsSetupField{settingsRowsField()}})
	return v
}

func TestSettingsSetupRowsRenderEditor(t *testing.T) {
	f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) {
		return settingsRowsView(), nil
	}}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-key="server.auth.tokens"`, "row editor field")
	settingsMustContain(t, body, `class="setup-rows"`, "row fieldset")
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.principal"`, "principal cell")
	settingsMustContain(t, body, `value="alice@example.com"`, "principal value")
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.__base" value="0"`, "row identity")
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.__remove" value="true"`, "revoke control")
	settingsMustContain(t, body, setup.SecretMask, "secret cell mask")
	// The stored secret is the mask; the token cell is a password input.
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.token" type="password"`, "secret cell control")
	// The blank add row renders cells but no revoke control.
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#1.principal"`, "add-row cell")
	settingsMustNotContain(t, body, `name="cfg.server.auth.tokens#1.__remove"`, "add row has no remove")
	// A boolean cell posts the hidden-false/checked-true pair.
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.write" value="true"`, "bool cell")
}

func TestSettingsSetupRowsRejectKeepsTypedCells(t *testing.T) {
	const typed = "typed-rotated-secret-never-echoed"
	f := &settingsFakes{deps: SettingsDeps{
		SetupView: func(context.Context) (*SettingsSetupView, error) { return settingsRowsView(), nil },
		SetupSave: func(context.Context, map[string]string) (SettingsSetupResult, []SettingsSetupError, error) {
			return SettingsSetupResult{}, []SettingsSetupError{
				{Key: "server.auth.tokens", Message: "server.auth.tokens[0].principal: invalid"},
			}, nil
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/setup", map[string][]string{
		"op": {"save"},
		// row 0: rotate the secret + edit the principal
		"cfg.server.auth.tokens#0.__base":    {"0"},
		"cfg.server.auth.tokens#0.principal": {"alice-edited@example.com"},
		"cfg.server.auth.tokens#0.token":     {typed},
		"cfg.server.auth.tokens#0.token_env": {""},
		"cfg.server.auth.tokens#0.write":     {"false", "true"},
		"cfg.server.auth.tokens#0.admin":     {"false", "false"},
		// row 1: removed
		"cfg.server.auth.tokens#1.__remove": {"true"},
		// row 2: added from the blank row
		"cfg.server.auth.tokens#2.principal": {"carol@example.com"},
		"cfg.server.auth.tokens#2.token":     {"carol-typed-once"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected save = %d", rec.Code)
	}
	body := rec.Body.String()
	// The typed non-secret cells survive the rejection.
	settingsMustContain(t, body, `value="alice-edited@example.com"`, "typed principal")
	settingsMustContain(t, body, `value="carol@example.com"`, "typed add row")
	// The typed secrets do not: they come back as cells to re-enter.
	settingsMustNotContain(t, body, typed, "typed token echoed")
	settingsMustNotContain(t, body, "carol-typed-once", "typed new-token echoed")
	settingsMustContain(t, body, setup.SecretMask, "rotated row keeps the mask")
	// The removed row is gone; the error lands on the row editor.
	settingsMustNotContain(t, body, `value="alice@example.com"`, "removed row re-rendered")
	settingsMustContain(t, body, "invalid", "keyed error line")
	// Rebuilt keys are renumbered; identity follows __base, not position.
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#0.__base" value="0"`, "kept row identity")
	settingsMustContain(t, body, `name="cfg.server.auth.tokens#1.__base" value=""`, "added row is new")
}

func TestSettingsSetupRowsBlankEditor(t *testing.T) {
	// An empty struct slice still renders cells (so an operator can add one)
	// and posting them blank changes nothing.
	empty := settingsRowsField()
	empty.Rows = []setup.Row{{
		Index: 0, Base: -1, BaseKey: setup.RowBaseKey("server.auth.tokens", 0), Caption: "new row",
		RemoveKey: setup.RowRemoveKey("server.auth.tokens", 0),
		Cells: []setup.RowCell{
			{Name: "principal", Key: setup.RowCellKey("server.auth.tokens", 0, "principal"), Label: "Principal", Type: "string"},
			{Name: "token", Key: setup.RowCellKey("server.auth.tokens", 0, "token"), Label: "Token", Type: "string", Secret: true},
		},
	}}
	v := settingsFakeView()
	v.Sections = append(v.Sections, SettingsSetupSection{Section: "auth", Essential: []SettingsSetupField{empty}})
	f := &settingsFakes{deps: SettingsDeps{SetupView: func(context.Context) (*SettingsSetupView, error) { return v, nil }}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), `name="cfg.server.auth.tokens#0.principal"`, "empty editor cell")
	if strings.Contains(rec.Body.String(), `name="cfg.server.auth.tokens#0.__remove"`) {
		t.Fatal("a blank add row must not offer remove")
	}
}
