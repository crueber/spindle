package webui

// settings_config_snippet_test.go — the repo settings config editor's
// "Insert static-site snippet" affordance (docs/features/19_static_sites.md
// §5). The button itself is inert without `web/src/islands/snippet.js`; this
// test pins the markup contract the island binds (type, data hooks, hint,
// status region) and its position, and that no other settings tab grows it.

import (
	"net/http"
	"strings"
	"testing"
)

func TestSettingsConfigSnippetAffordance(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/config")
	if code != http.StatusOK {
		t.Fatalf("GET config = %d (%s)", code, body)
	}

	for _, want := range []string{
		`>Insert static-site snippet</button>`,
		`type="button"`,
		`data-snippet-insert`,
		`data-snippet-target="#config-text"`,
		`data-snippet-status="#config-snippet-status"`,
		`aria-describedby="config-snippet-hint"`,
		`id="config-snippet-hint"`,
		`[static_sites]`,
		`id="config-snippet-status"`,
		`role="status"`,
		`aria-live="polite"`,
		`data-dirty-status`,
	} {
		settingsMustContain(t, body, want, "static-site snippet affordance")
	}

	// Position: the button sits between the card heading and the editor's
	// label (the requested place), never inside the textarea or below it.
	heading := strings.Index(body, "</h3>")
	button := strings.Index(body, "Insert static-site snippet")
	label := strings.Index(body, "TOML (published to the WAL)")
	editor := strings.Index(body, `id="config-text"`)
	if heading < 0 || button < 0 || label < 0 || editor < 0 {
		t.Fatalf("config page is missing heading/button/label/editor (heading %d, button %d, label %d, editor %d)", heading, button, label, editor)
	}
	if !(heading < button && button < label && label < editor) {
		t.Fatalf("snippet button position = %d, want heading(%d) < button < label(%d) < editor(%d)", button, heading, label, editor)
	}

	// One tab only: the affordance is not a shell-wide control.
	code, general := seamsGet(t, h, "/acme/monorepo/settings/general")
	if code != http.StatusOK {
		t.Fatalf("GET general = %d", code)
	}
	settingsMustNotContain(t, general, "Insert static-site snippet", "snippet button outside the config tab")
	settingsMustNotContain(t, general, "config-snippet-status", "snippet status outside the config tab")
}
