package main

// admin_config_audit3_test.go — the round-3 settings audit's two /setup defects, on
// the REAL page path (the same seam wiring a browser drives):
//
//  1. a coercion-invalid value is keyed: its row carries the error line, its
//     collapsed Advanced group opens, and the errors table names the key;
//  2. the first canonicalising save keeps the operator's previous file as
//     spindle.toml.bak-<utc> (once), a no-op save writes nothing, and the page
//     says both things next to Save.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
	"github.com/crueber/spindle/internal/webui"
	"golang.org/x/net/html"
)

// adminConfigPageHarnessFile boots the page provider over an exact config file body
// (the caller owns the bytes) and returns the server, the path and the boot.
func adminConfigPageHarnessFile(t *testing.T, body []byte) (*webui.Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFileName)
	if err := config.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := setup.BaseConfig(dir, []string{path})
	boot := server.BootState{Mode: "normal", ConfigPaths: []string{path}}
	ui, err := webui.New(webui.Env{Cfg: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("webui: %v", err)
	}
	ui.BindAuth(pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "admin@example.com", Admin: true}, nil
	}})
	wireSettingsPages(cfg, boot, nil, nil)
	wireAdminPages(cfg, boot, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(func() {
		webui.BindSettings(webui.SettingsDeps{})
		webui.BindAdmin(webui.AdminDeps{})
	})
	return ui, path
}

// setupFieldRow finds the .setup-row rendered for key.
func setupFieldRow(t *testing.T, doc *html.Node, key string) *html.Node {
	t.Helper()
	for _, n := range setupFindAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && setupHasClass(n, "setup-row")
	}) {
		if setupAttr(n, "data-key") == key {
			return n
		}
	}
	return nil
}

// setupRowErrorText is the .err-line text inside a row ("" when unmarked).
func setupRowErrorText(row *html.Node) string {
	var b strings.Builder
	for _, n := range setupFindAll(row, func(n *html.Node) bool {
		return n.Type == html.ElementNode && setupHasClass(n, "err-line")
	}) {
		b.WriteString(setupText(n))
	}
	return b.String()
}

// setupAdvancedAncestor returns the enclosing Advanced <details>, if any.
func setupAdvancedAncestor(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "details" && setupHasClass(p, "setup-advanced") {
			return p
		}
	}
	return nil
}

// TestAdminConfigCoercionErrorMarksItsRow is defect 1's rendered acceptance
// test. It walks every field type with a value its parser rejects and asserts
// the rejection is keyed and rendered on the offending row (and that an
// Advanced row opens its group), which the keyless pre-fix error could not do.
func TestAdminConfigCoercionErrorMarksItsRow(t *testing.T) {
	harnessBody, err := config.MarshalTOML(adminConfigPageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, field, value, row string
	}{
		// string/enum: any text coerces, so the validator is the gate; the
		// assertion (a key on the row) is the same.
		{"string/enum", "cfg.store.backend", "bogus", "store.backend"},
		{"size", "cfg.cache.max_bytes", "eleventy", "cache.max_bytes"},
		// The audit's exact reproduction: a duration in a collapsed group.
		{"duration", "cfg.cache.evict_idle_after", "bogus-duration", "cache.evict_idle_after"},
		{"int", "cfg.server.max_concurrent_requests", "many", "server.max_concurrent_requests"},
		{"uint", "cfg.wal.cas_max_retries", "-1", "wal.cas_max_retries"},
		{"uint64", "cfg.wal.snapshot_every_entries", "1.5", "wal.snapshot_every_entries"},
		{"float", "cfg.cache.disk_high_watermark", "high", "cache.disk_high_watermark"},
		{"bool", "cfg.server.http2", "yes", "server.http2"},
		{"list", "cfg.bundles.strategy", "not a toml fragment = [", "bundles.strategy"},
		// A struct-slice cell: the cell key maps onto its row editor.
		{"struct-slice cell", "cfg.bundles.strategy#0.keep", "many", "bundles.strategy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui, _ := adminConfigPageHarnessFile(t, harnessBody)
			form := map[string][]string{"op": {"save"}, tc.field: {tc.value}}
			rec := adminConfigPageDo(t, ui, http.MethodPost, form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s=%q = %d, want 422: %s", tc.field, tc.value, rec.Code, rec.Body.String())
			}
			doc := setupParsePage(t, rec.Body.String())
			row := setupFieldRow(t, doc, tc.row)
			if row == nil {
				t.Fatalf("no row rendered for %s", tc.row)
			}
			if msg := setupRowErrorText(row); msg == "" {
				t.Fatalf("%s=%q did not mark the %s row", tc.field, tc.value, tc.row)
			} else if !strings.Contains(msg, tc.row) && !strings.Contains(msg, strings.ReplaceAll(tc.field, "cfg.", "")) {
				t.Fatalf("%s row error %q names no key", tc.row, msg)
			}
			// A collapsed Advanced group holding the rejected row renders open.
			if group := setupAdvancedAncestor(row); group != nil && !setupHasAttr(group, "open") {
				t.Fatalf("the Advanced group holding the rejected %s row renders collapsed", tc.row)
			}
			// The errors table's KEY cell is never empty (the pre-fix defect).
			table := setupFindAll(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "table" })
			found := false
			for _, tbl := range table {
				for _, code := range setupFindAll(tbl, func(n *html.Node) bool { return n.Data == "code" }) {
					if strings.Contains(setupText(code), tc.row) {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("the errors table has no KEY cell naming %s", tc.row)
			}
		})
	}
}

// TestAdminConfigUnknownKeyCannotPassSilently is the negative control: an
// unknown key is rejected, named in the table, and writes nothing.
func TestAdminConfigUnknownKeyCannotPassSilently(t *testing.T) {
	body, err := config.MarshalTOML(adminConfigPageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ui, path := adminConfigPageHarnessFile(t, body)
	rec := adminConfigPageDo(t, ui, http.MethodPost, map[string][]string{
		"op": {"save"}, "cfg.server.bogus_setting": {"1"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown key = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "server.bogus_setting") {
		t.Fatal("the rejection does not name the unknown key")
	}
	if after, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(after, body) {
		t.Fatalf("a rejected unknown key changed the file (err=%v)", rerr)
	}
}
