package main

// admin_config_blocker_test.go — G1's page-level acceptance gate.
//
// The audit measured POST /admin/config failing 422 in three ways (two struct slices
// rendering empty, two unsigned counters rendering empty, 17 empty lists
// coercing to [""]). This drives the REAL page path: build the same deps
// wireSettingsPages builds, GET /admin/config, scrape every form control out of the
// rendered HTML (the payload a browser sends), POST it back unmodified, and
// require a zero-edit save that leaves the config byte-identical.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
	"github.com/crueber/spindle/internal/webui"
)

const pageTokenLiteral = "page-token-secret-value-never-rendered"

// adminConfigPageFixture is a saveable config with one live value in every class the
// audit flagged.
func adminConfigPageFixture(t *testing.T) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.Server.Listen = "0.0.0.0:18080"
	c.Server.Roles = []string{"serve"}
	c.Server.MaxPushBytes = 64 << 30
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.OAuthClientID = "spindle"
	c.Server.Auth.OAuthClientSecret = "s3cret"
	c.Server.Auth.SessionSecret = strings.Repeat("k", 32)
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	c.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "alice@example.com", Token: pageTokenLiteral, Write: true},
		{Principal: "bob@example.com", TokenEnv: "BOB_TOKEN", Write: true},
	}
	c.WAL.SnapshotEveryEntries = 512
	c.WAL.CASMaxRetries = 7
	c.Cache.DiskHighWatermark = 0.75
	c.Upstream.Follow = []string{"main"}
	c.Bundles.Strategy = []config.BundleStrategy{
		{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2, BackfillMax: 1},
		{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "0 0 23 * * *", Chain: true},
	}
	if err := config.ValidateForSave(c); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	return c
}

// adminConfigPageHarness writes the fixture config to a temp data dir and boots the
// page provider with the real setup seams, returning the server, the config
// path and the exact file bytes.
func adminConfigPageHarness(t *testing.T) (*webui.Server, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFileName)
	base := adminConfigPageFixture(t)
	base.DataDir = dir
	body, err := config.MarshalTOML(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// The running config the page renders IS the file (defaults ⊕ file).
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
	return ui, path, body
}

// TestAdminConfigZeroEditFormSavesByteIdentical is the G1 acceptance test on the
// page's own save path.
func TestAdminConfigZeroEditFormSavesByteIdentical(t *testing.T) {
	ui, path, body := adminConfigPageHarness(t)

	// 1. The page renders.
	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	if strings.Contains(page, pageTokenLiteral) {
		t.Fatal("the rendered page contains a static token secret")
	}
	if !strings.Contains(page, setup.SecretMask) {
		t.Fatal("the page does not mask the token secret")
	}
	if !strings.Contains(page, `data-key="bundles.strategy"`) || !strings.Contains(page, `data-key="server.auth.tokens"`) {
		t.Fatal("a struct-slice row editor is missing from the page")
	}
	if !strings.Contains(page, `name="cfg.bundles.strategy#0.name"`) {
		t.Fatal("bundle strategy rows do not render their cells")
	}
	if !strings.Contains(page, `name="cfg.server.auth.tokens#0.token"`) {
		t.Fatal("token rows do not render their cells")
	}
	if !strings.Contains(page, `value="512"`) || !strings.Contains(page, `value="7"`) {
		t.Fatal("unsigned counters render empty")
	}

	// G18: every section splits into open essentials and one collapsed
	// Advanced group, partitioned from the single display-metadata list.
	doc := setupParsePage(t, page)
	groups := setupFindAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "details" && setupHasClass(n, "setup-advanced")
	})
	if len(groups) == 0 {
		t.Fatal("the page renders no collapsed Advanced group")
	}
	advancedNames := map[string]bool{}
	for _, d := range groups {
		if setupHasAttr(d, "open") {
			t.Fatal("an Advanced group renders open on a clean page")
		}
		for _, name := range setupCfgNames(d) {
			advancedNames[name] = true
		}
	}
	if !advancedNames["cfg.telemetry.log_format"] {
		t.Fatal("an advanced key (telemetry.log_format) is not inside a collapsed group")
	}
	if advancedNames["cfg.server.listen"] {
		t.Fatal("an essential key (server.listen) was swallowed by an Advanced group")
	}
	// Free-text enum keys are real selects over the schema's value set.
	for _, want := range []struct {
		key    string
		values []string
	}{
		{"bundles.serve_via", []string{"proxy", "signed_url"}},
		{"compaction.engine", []string{"git"}},
		{"git.upload_pack_engine", []string{"auto", "git", "gix"}},
		{"telemetry.log_format", []string{"pretty", "json"}},
	} {
		sel := setupFindSelect(doc, "cfg."+want.key)
		if sel == nil {
			t.Fatalf("%s does not render as a <select>", want.key)
		}
		if got := setupOptionValues(sel); !slices.Equal(got, want.values) {
			t.Fatalf("%s options = %v, want %v", want.key, got, want.values)
		}
	}

	// 2. Post back EXACTLY what the page rendered.
	form := scrapeAdminConfigForm(t, page)
	// A collapsed <details> submits its rows like an open one: a browser POSTs
	// every successful control regardless of the disclosure's state.
	for name := range advancedNames {
		if _, ok := form[name]; !ok {
			t.Fatalf("%s inside a collapsed Advanced group is missing from the payload", name)
		}
	}
	// The enum selects round-trip their stored value (the fixture is defaults
	// plus the fixture's own keys).
	if got := form.Get("cfg.bundles.serve_via"); got != "proxy" {
		t.Fatalf("scraped bundles.serve_via = %q, want proxy", got)
	}
	if got := form.Get("cfg.git.upload_pack_engine"); got != "auto" {
		t.Fatalf("scraped git.upload_pack_engine = %q, want auto", got)
	}
	if got := form.Get("cfg.server.auth.tokens#0.token"); got != setup.SecretMask {
		t.Fatalf("scraped token cell = %q, want the mask", got)
	}
	if got := form.Get("cfg.server.auth.tokens#1.token_env"); got != "BOB_TOKEN" {
		t.Fatalf("scraped token_env = %q", got)
	}
	if got := form.Get("cfg.wal.snapshot_every_entries"); got != "512" {
		t.Fatalf("scraped unsigned counter = %q", got)
	}
	if got := form.Get("cfg.cache.prewarm"); got != "" {
		t.Fatalf("scraped empty list = %q", got)
	}
	form.Set("op", "save")
	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config (unmodified) = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "saved=1") {
		t.Fatalf("save redirect = %q", loc)
	}

	// 3. The file is byte-identical: nothing the operator did not touch moved.
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, body) {
		t.Fatalf("zero-edit save changed the file:\n%s", tomlDiff(string(body), string(saved)))
	}

	// 4. A validation failure names the offending key and writes nothing.
	form.Set("cfg.bundles.strategy#0.schedule", "not a cron")
	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid save = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bundles.strategy") {
		t.Fatal("the rejection does not name the offending key")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, body) {
		t.Fatalf("a rejected save changed the file (err=%v):\n%s", err, tomlDiff(string(body), string(after)))
	}

	// 5. A second save is a no-op too.
	rec = adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second GET /admin/config = %d", rec.Code)
	}
	form = scrapeAdminConfigForm(t, rec.Body.String())
	form.Set("op", "save")
	if rec = adminConfigPageDo(t, ui, http.MethodPost, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("second POST /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, body) {
		t.Fatalf("second save drifted:\n%s", tomlDiff(string(body), string(again)))
	}
}

// TestAdminConfigRejectedAdvancedKeyOpensItsGroup proves a rejected key marks
// the right row in the right group: the row carries the validator's message
// AND its collapsed Advanced disclosure renders open, so the error is visible
// without hunting the page.
func TestAdminConfigRejectedAdvancedKeyOpensItsGroup(t *testing.T) {
	ui, path, body := adminConfigPageHarness(t)
	form := url.Values{}
	form.Set("cfg.store.multipart_part_size", "128MiB") // > threshold (64MiB)
	form.Set("op", "validate")
	rec := adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid advanced key = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	doc := setupParsePage(t, rec.Body.String())
	group := setupFindDataAttr(doc, "data-advanced-for", "store")
	if group == nil {
		t.Fatal("the store section has no Advanced group")
	}
	if !setupHasAttr(group, "open") {
		t.Fatal("the Advanced group holding a rejected key renders collapsed")
	}
	var marked []*html.Node
	for _, n := range setupFindAll(group, func(n *html.Node) bool {
		return n.Type == html.ElementNode && setupHasClass(n, "setup-row")
	}) {
		if strings.Contains(setupText(n), "must be <=") {
			marked = append(marked, n)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("the rejected advanced row is marked %d times, want once", len(marked))
	}
	row := marked[0]
	if key := setupAttr(row, "data-key"); !strings.HasPrefix(key, "store.multipart") {
		t.Fatalf("the message landed on %q, not a multipart row", key)
	}
	if !setupContains(group, row) {
		t.Fatal("the rejected row is not inside its Advanced group")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, body) {
		t.Fatalf("a rejected validate changed the file (err=%v)", err)
	}
}

// setupParsePage parses rendered HTML for structural assertions.
func setupParsePage(t *testing.T, page string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	return doc
}

// setupFindAll returns every element matching pred.
func setupFindAll(root *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && pred(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func setupHasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key != "class" {
			continue
		}
		for _, c := range strings.Fields(a.Val) {
			if c == class {
				return true
			}
		}
	}
	return false
}

func setupHasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// setupAttr returns an element attribute's value ("" when absent).
func setupAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// setupFindDataAttr finds the first element whose attr equals val.
func setupFindDataAttr(root *html.Node, attr, val string) *html.Node {
	found := setupFindAll(root, func(n *html.Node) bool {
		for _, a := range n.Attr {
			if a.Key == attr && a.Val == val {
				return true
			}
		}
		return false
	})
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

func setupFindSelect(root *html.Node, name string) *html.Node {
	found := setupFindAll(root, func(n *html.Node) bool {
		if n.Data != "select" {
			return false
		}
		for _, a := range n.Attr {
			if a.Key == "name" && a.Val == name {
				return true
			}
		}
		return false
	})
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

func setupOptionValues(sel *html.Node) []string {
	var out []string
	for _, o := range setupFindAll(sel, func(n *html.Node) bool { return n.Data == "option" }) {
		val := ""
		for _, a := range o.Attr {
			if a.Key == "value" {
				val = a.Val
			}
		}
		out = append(out, val)
	}
	return out
}

// setupCfgNames returns the cfg.* control names inside n.
func setupCfgNames(n *html.Node) []string {
	var out []string
	for _, el := range setupFindAll(n, func(n *html.Node) bool {
		switch n.Data {
		case "input", "select", "textarea":
			return true
		}
		return false
	}) {
		for _, a := range el.Attr {
			if a.Key == "name" && strings.HasPrefix(a.Val, "cfg.") {
				out = append(out, a.Val)
			}
		}
	}
	return out
}

// setupContains reports whether target is a descendant of root.
func setupContains(root, target *html.Node) bool {
	for _, n := range setupFindAll(root, func(*html.Node) bool { return true }) {
		if n == target {
			return true
		}
	}
	return false
}

// setupText is the concatenated text content of n.
func setupText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for x := c.FirstChild; x != nil; x = x.NextSibling {
			walk(x)
		}
	}
	walk(n)
	return b.String()
}

// TestAdminConfigRevokeAndRotate drives the token editor's two credential
// operations through the page (G3): tick remove on row 0, type a new secret in
// row 1's masked field, save, and read the file back.
func TestAdminConfigRevokeAndRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFileName)
	base := adminConfigPageFixture(t)
	base.DataDir = dir
	body, err := config.MarshalTOML(base)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal(err)
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

	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	form := scrapeAdminConfigForm(t, rec.Body.String())
	form.Set("op", "save")
	form.Set("cfg.server.auth.tokens#0.__remove", "true")
	form.Set("cfg.server.auth.tokens#1.token", "rotated-by-the-page")
	// Add a token from the blank row (the page's add affordance): the secret
	// is typed once here and must never render back.
	form.Set("cfg.server.auth.tokens#2.principal", "carol@example.com")
	form.Set("cfg.server.auth.tokens#2.token", "carol-typed-once")
	form.Set("cfg.server.auth.tokens#2.write", "false")
	form.Set("cfg.server.auth.tokens#2.write", "true")
	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(string(saved)), " ")
	if !strings.Contains(got, `[[server.auth.tokens]] principal = "bob@example.com"`) {
		t.Fatalf("bob's token row missing after the revoke:\n%s", saved)
	}
	if strings.Contains(got, "alice@example.com") {
		t.Fatalf("revoked token row survived:\n%s", saved)
	}
	if !strings.Contains(got, `token = "rotated-by-the-page"`) {
		t.Fatalf("rotation was not written:\n%s", saved)
	}
	if !strings.Contains(got, `token_env = "BOB_TOKEN"`) {
		t.Fatalf("token_env lost on rotate:\n%s", saved)
	}
	if !strings.Contains(got, `[[server.auth.tokens]] principal = "carol@example.com" token = "carol-typed-once" token_env = "" write = true`) {
		t.Fatalf("the added token row was not written:\n%s", saved)
	}
}

func adminConfigPageDo(t *testing.T, ui *webui.Server, method string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, "http://example.com/admin/config", body)
	req.Host = "example.com"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
	}
	rec := httptest.NewRecorder()
	if !ui.Serve(rec, req) {
		t.Fatalf("%s /admin/config was not answered by the page provider", method)
	}
	return rec
}

// scrapeAdminConfigForm extracts every cfg.* control from the rendered page in
// document order — exactly the payload a browser submits (an unchecked
// checkbox/radio is skipped; a textarea contributes its text; a select its
// selected option).
func scrapeAdminConfigForm(t *testing.T, page string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	form := url.Values{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "input":
				name, val, typ := "", "", "text"
				isChecked := false
				for _, a := range n.Attr {
					switch a.Key {
					case "name":
						name = a.Val
					case "value":
						val = a.Val
					case "type":
						typ = strings.ToLower(a.Val)
					case "checked":
						isChecked = true
					}
				}
				if strings.HasPrefix(name, "cfg.") && (typ != "checkbox" && typ != "radio" || isChecked) {
					form.Add(name, val)
				}
			case "textarea", "select":
				name := ""
				for _, a := range n.Attr {
					if a.Key == "name" {
						name = a.Val
					}
				}
				if !strings.HasPrefix(name, "cfg.") {
					break
				}
				val := ""
				if n.Data == "textarea" {
					val = textContent(n)
				} else {
					val = selectedOption(n)
				}
				form.Add(name, val)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return form
}

func textContent(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func selectedOption(n *html.Node) string {
	var walk func(*html.Node) string
	walk = func(n *html.Node) string {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "option" {
				selected := false
				val := ""
				for _, a := range c.Attr {
					switch a.Key {
					case "selected":
						selected = true
					case "value":
						val = a.Val
					}
				}
				if selected {
					return val
				}
			}
			if v := walk(c); v != "" {
				return v
			}
		}
		return ""
	}
	return walk(n)
}

// tomlDiff names the lines that differ, so a failure reads like the audit's
// before/after diff instead of two wall-of-text dumps.
func tomlDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "line %d:\n  want %q\n  got  %q\n", i+1, wl, gl)
		}
	}
	return b.String()
}
