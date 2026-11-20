package server

// Setup editing semantics (§3.4): PUT/POST /api/v1/setup edit the CURRENT
// file-visible configuration — untouched keys keep their values, the
// zero-config first-run shape is the base when no file exists, and the
// standalone setup surface stays open whenever auth mode is "none".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/setup"
)

func setupMergeServer(t *testing.T, dataDir string) (*Server, http.Handler) {
	t.Helper()
	paths := []string{filepath.Join(dataDir, config.ConfigFileName)}
	// The effective config mirrors a real boot: defaults ⊕ file (first-run
	// shape when no file exists).
	cfg, err := config.LoadSetupBase(dataDir, paths)
	if err != nil {
		t.Fatal(err)
	}
	s, h := newTestServer(t, func(o *Options) {
		o.Config = cfg
	})
	s.cfg.DataDir = dataDir
	s.boot.ConfigPaths = paths
	s.boot.Mode = "normal"
	return s, h
}

func putSetup(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A save must EDIT the current file: untouched keys keep their values.
func TestSetupPutEditsCurrentFile(t *testing.T) {
	dataDir := t.TempDir()
	base := config.FirstRunDefaults(dataDir)
	base.Server.Listen = "127.0.0.1:9443"
	base.Server.Auth.Mode = "none"
	base.Server.Auth.AnonymousRead = true
	if err := config.SaveSetup(base, dataDir); err != nil {
		t.Fatal(err)
	}
	s, _ := setupMergeServer(t, dataDir)

	code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "2h"}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Server.Listen != "127.0.0.1:9443" {
		t.Fatalf("untouched listen reset: %q", after.Server.Listen)
	}
	if after.Server.Auth.Mode != "none" {
		t.Fatalf("untouched auth mode reset: %q", after.Server.Auth.Mode)
	}
	if time.Duration(after.Server.RequestTimeout) != 2*time.Hour {
		t.Fatalf("override not applied: %v", after.Server.RequestTimeout)
	}
	rr, _ := body["requires_restart"].([]any)
	if len(rr) != 1 || rr[0] != "server.request_timeout" {
		t.Fatalf("requires_restart = %v, want exactly the edited key", body["requires_restart"])
	}
}

// With no config file the base is the first-run shape, NOT the compiled-in
// defaults — a first save must not swap the filesystem store for s3 or move
// the listener off 0.0.0.0.
func TestSetupPutFirstRunKeepsFirstRunShape(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "defaults"
	if err := os.Remove(filepath.Join(dataDir, config.ConfigFileName)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "30m"}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Store.Backend != "filesystem" || after.Server.Listen != "0.0.0.0:8080" || !after.Server.AutoCreateOnPush {
		t.Fatalf("first-run shape lost: backend=%q listen=%q auto=%t",
			after.Store.Backend, after.Server.Listen, after.Server.AutoCreateOnPush)
	}
	if time.Duration(after.Server.RequestTimeout) != 30*time.Minute {
		t.Fatalf("override not applied: %v", after.Server.RequestTimeout)
	}
}

// The merged config is what gets validated: a request that is only valid
// against the CURRENT file must pass, and one that breaks the merge must 422.
func TestSetupTestValidatesMergedConfig(t *testing.T) {
	dataDir := t.TempDir()
	base := config.FirstRunDefaults(dataDir)
	base.Server.Auth.Mode = "oidc"
	base.Server.Auth.AnonymousRead = false
	base.Server.Auth.Issuer = "https://id.example.com"
	base.Server.Auth.AllowedDomains = []string{"example.com"}
	base.Server.Auth.OAuthClientID = "walhub"
	base.Server.Auth.OAuthClientSecret = "s3cret"
	base.Server.Auth.SessionSecret = strings.Repeat("x", 32) // trio complete (#344)
	if err := config.SaveSetup(base, dataDir); err != nil {
		t.Fatal(err)
	}
	s, _ := setupMergeServer(t, dataDir)
	t.Setenv("WALHUB_SETUP_TOKEN", "setup-tok")

	// mode=oidc alone is only valid because the FILE supplies the trio.
	req := httptest.NewRequest("POST", "/api/v1/setup/test?token=setup-tok",
		strings.NewReader(`{"overrides": {"server.auth.mode": "oidc"}}`))
	rec := httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merged validate = %d %s (file keys must satisfy the request)", rec.Code, rec.Body.String())
	}

	// Breaking the merge: clearing the file's oauth_client_secret leaves
	// client_id set — "both set or both unset" fails only against the merge.
	req = httptest.NewRequest("POST", "/api/v1/setup/test?token=setup-tok",
		strings.NewReader(`{"overrides": {"server.auth.oauth_client_secret": ""}}`))
	rec = httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid merge = %d %s", rec.Code, rec.Body.String())
	}
}

// Browsers attach Origin to every state-changing request, same-origin ones
// included; the CORS gate must allow an Origin that names the request's own
// host (regression: the setup Save button 403'd from the SPA at zero-config).
func TestCORSAllowsSameOriginWrites(t *testing.T) {
	if !sameOriginHost("http://walgit.localhost:8080", "walgit.localhost:8080") {
		t.Fatal("same origin must match")
	}
	if !sameOriginHost("https://hub.test/hub", "hub.test") {
		t.Fatal("origin with a path must still match its host")
	}
	if sameOriginHost("http://evil.test", "hub.test") {
		t.Fatal("foreign origin must not match")
	}
	if sameOriginHost("hub.test", "hub.test") {
		t.Fatal("origin without a scheme is malformed, never same-origin")
	}

	dataDir := t.TempDir()
	s, h := setupMergeServer(t, dataDir)
	s.cfg.Server.Auth.Mode = "none"
	s.cfg.Server.Auth.AnonymousRead = true
	s.boot.Mode = "normal"
	req := httptest.NewRequest("PUT", "http://walgit.localhost:8080/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.request_timeout": "2h"}}`))
	req.Header.Set("Origin", "http://walgit.localhost:8080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin PUT = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// The TOML decode failure must surface as an error (a nil error with a nil
// config would crash the caller).
func TestParseSetupBodyTOMLDecodeError(t *testing.T) {
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader("[server\nbroken"))
	if _, err := setup.ParseRequest(req, config.Defaults()); err == nil {
		t.Fatal("broken TOML must error")
	}
	// JSON list values take the default formatting branch.
	req = httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.cors_origins": ["a.test", "b.test"]}}`))
	c, err := setup.ParseRequest(req, config.Defaults())
	if err != nil || len(c.Server.CorsOrigins) != 2 {
		t.Fatalf("list override = %v err=%v", c, err)
	}
}

// setupBaseConfig falls back to the first-run shape when the candidate file
// cannot be parsed (a directory in the candidate list forces a decode error).
func TestSetupBaseConfigFallsBackOnBadFile(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.ConfigPaths = []string{dataDir} // a directory: stat ok, decode fails
	base := s.setupBaseConfig()
	if base.Store.Backend != "filesystem" || base.DataDir != dataDir {
		t.Fatalf("fallback base = backend %q dir %q", base.Store.Backend, base.DataDir)
	}
}

// Duration and byte-size values round-trip: the schema serves "1h0m0s" /
// plain integers, and Sscanf-style parsing would truncate both.
// The setup UI sends the same spellings the TOML file accepts: sizes as
// "64MiB", durations with the d/w suffixes, floats and unsigned counters as
// plain numbers, and struct slices (bundles.strategy) as a [[strategy]]
// fragment. Every one must round-trip through the overrides channel — these
// previously failed with "not an int"/"unsupported type", so the page could
// not save its own edits.
func TestSetupPutAcceptsUISpellings(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)

	fragment := "[[strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 23 * * 0\"\nkeep = 2"
	body, err := json.Marshal(map[string]any{"overrides": map[string]any{
		"server.max_push_bytes":      "2GiB",
		"maintenance.fsck_interval":  "24h",
		"wal.snapshot_every_entries": 1000,
		"cache.disk_high_watermark":  "0.9",
		"bundles.strategy":           fragment,
	}})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := putSetup(t, s, string(body))
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if int64(after.Server.MaxPushBytes) != 2<<30 {
		t.Fatalf("size coerced wrong: %d", after.Server.MaxPushBytes)
	}
	if time.Duration(after.Maintenance.FsckInterval) != 24*time.Hour {
		t.Fatalf("d/w duration coerced wrong: %v", after.Maintenance.FsckInterval)
	}
	if after.WAL.SnapshotEveryEntries != 1000 {
		t.Fatalf("uint coerced wrong: %d", after.WAL.SnapshotEveryEntries)
	}
	if after.Cache.DiskHighWatermark != 0.9 {
		t.Fatalf("float coerced wrong: %v", after.Cache.DiskHighWatermark)
	}
	if len(after.Bundles.Strategy) != 1 || after.Bundles.Strategy[0].Name != "weekly" ||
		after.Bundles.Strategy[0].Kind != "full" || after.Bundles.Strategy[0].Keep != 2 {
		t.Fatalf("strategy fragment decoded wrong: %+v", after.Bundles.Strategy)
	}
}

// plain integers, and Sscanf-style parsing would truncate both.
func TestSetupCoerceDurationsAndSizes(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)

	code, body := putSetup(t, s,
		`{"overrides": {"server.request_timeout": "90m", "server.max_push_bytes": 68719476736}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(after.Server.RequestTimeout) != 90*time.Minute {
		t.Fatalf("duration coerced wrong: %v", after.Server.RequestTimeout)
	}
	if int64(after.Server.MaxPushBytes) != 68719476736 {
		t.Fatalf("byte size coerced wrong: %d", after.Server.MaxPushBytes)
	}
}

// Fix #23: every new [import] FIELDS example is accepted by
// POST /api/v1/setup/test in isolation, and the full set round-trips
// through PUT /api/v1/setup into <data-dir>/walhub.toml with effect.
func TestSetupImportKeysRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "normal"

	singles := map[string]any{
		"import.url_allowlist":          "git.example.com",
		"import.allow_private_networks": "false",
		"import.allow_file_urls":        "false",
		"import.clone_timeout":          "30m",
		"import.git_timeout":            "5m",
		"import.max_bytes":              "64GiB",
		"import.max_refs":               "100000",
		"import.max_concurrent":         "2",
	}
	for k, v := range singles {
		body, _ := json.Marshal(map[string]any{"overrides": map[string]any{k: v}})
		req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.setupTest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup/test %s=%v = %d %s, want 200", k, v, rec.Code, rec.Body.String())
		}
	}

	// The normalized UI payload (lists/bools/ints coerced, durations/sizes
	// spec-spelled) saves and persists with effect.
	payload, _ := json.Marshal(map[string]any{"overrides": map[string]any{
		"import.url_allowlist":          []any{"git.example.com"},
		"import.allow_private_networks": false,
		"import.allow_file_urls":        false,
		"import.clone_timeout":          "30m",
		"import.git_timeout":            "5m",
		"import.max_bytes":              "64GiB",
		"import.max_refs":               100000,
		"import.max_concurrent":         2,
	}})
	code, resp := putSetup(t, s, string(payload))
	if code != http.StatusOK {
		t.Fatalf("put import keys = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Import.URLAllowlist) != 1 || after.Import.URLAllowlist[0] != "git.example.com" {
		t.Fatalf("url_allowlist not persisted: %+v", after.Import.URLAllowlist)
	}
	if after.Import.AllowPrivateNetworks || after.Import.AllowFileURLs {
		t.Fatalf("bools not persisted: private=%t file=%t",
			after.Import.AllowPrivateNetworks, after.Import.AllowFileURLs)
	}
	if time.Duration(after.Import.CloneTimeout) != 30*time.Minute {
		t.Fatalf("clone_timeout not persisted: %v", after.Import.CloneTimeout)
	}
	if time.Duration(after.Import.GitTimeout) != 5*time.Minute {
		t.Fatalf("git_timeout not persisted: %v", after.Import.GitTimeout)
	}
	if int64(after.Import.MaxBytes) != 64<<30 {
		t.Fatalf("max_bytes not persisted: %d", after.Import.MaxBytes)
	}
	if after.Import.MaxRefs != 100000 || after.Import.MaxConcurrent != 2 {
		t.Fatalf("caps not persisted: refs=%d concurrent=%d", after.Import.MaxRefs, after.Import.MaxConcurrent)
	}
}

// Issue #301: the server.max_tree_log FIELDS example validates on
// POST /api/v1/setup/test and round-trips through PUT /api/v1/setup with
// effect (the per-entry tree-date walk cap).
func TestSetupTreeLogKeyRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "normal"

	body, _ := json.Marshal(map[string]any{"overrides": map[string]any{"server.max_tree_log": "200"}})
	req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup/test server.max_tree_log = %d %s, want 200", rec.Code, rec.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"overrides": map[string]any{"server.max_tree_log": 50}})
	if code, resp := putSetup(t, s, string(payload)); code != http.StatusOK {
		t.Fatalf("put server.max_tree_log = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Server.MaxTreeLog != 50 {
		t.Fatalf("max_tree_log not persisted: %d", after.Server.MaxTreeLog)
	}
}

// Issue #320: the server.serve_sync_timeout /
// server.serve_materialize_timeout FIELDS examples validate on
// POST /api/v1/setup/test and round-trip through PUT /api/v1/setup with
// effect (the bounded serve materialization: per-request wait + body cap).
func TestSetupServeTimeoutKeysRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "normal"

	for k, v := range map[string]string{
		"server.serve_sync_timeout":        "45s",
		"server.serve_materialize_timeout": "10m",
	} {
		body, _ := json.Marshal(map[string]any{"overrides": map[string]any{k: v}})
		req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.setupTest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup/test %s=%s = %d %s, want 200", k, v, rec.Code, rec.Body.String())
		}
	}
	payload, _ := json.Marshal(map[string]any{"overrides": map[string]any{
		"server.serve_sync_timeout":        "30s",
		"server.serve_materialize_timeout": "5m",
	}})
	if code, resp := putSetup(t, s, string(payload)); code != http.StatusOK {
		t.Fatalf("put serve timeouts = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(after.Server.ServeSyncTimeout) != 30*time.Second {
		t.Fatalf("serve_sync_timeout not persisted: %v", after.Server.ServeSyncTimeout)
	}
	if time.Duration(after.Server.ServeMaterializeTimeout) != 5*time.Minute {
		t.Fatalf("serve_materialize_timeout not persisted: %v", after.Server.ServeMaterializeTimeout)
	}
}

// The setup surface is open at ANY time while auth mode is "none" — first
// run, normal running mode, and setup-only mode alike — with no credential.
func TestSetupOpenWheneverAuthNone(t *testing.T) {
	for _, mode := range []string{"defaults", "normal", "setup_only"} {
		t.Run(mode, func(t *testing.T) {
			dataDir := t.TempDir()
			s, h := setupMergeServer(t, dataDir)
			s.cfg.Server.Auth.Mode = "none"
			s.cfg.Server.Auth.AnonymousRead = true
			s.boot.Mode = mode

			// The setup JSON API keeps the §3.4 access rule in every mode.
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/api/v1/setup", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/setup in %s mode = %d, want open (body %.80s)", mode, rec.Code, rec.Body.String())
			}
			// GET /setup is the server-rendered page: it reaches the page
			// provider in every mode (the provider owns the §3.4 page rule).
			ui := &fakeUI{paths: map[string]bool{"/setup": true}}
			s.SetUI(ui)
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/setup", nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="page"`) {
				t.Fatalf("GET /setup in %s mode = %d, want the page provider (body %.80s)", mode, rec.Code, rec.Body.String())
			}
			// And the save path answers too (a valid override, no credential).
			code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "45m"}}`)
			if code != http.StatusOK {
				t.Fatalf("PUT in %s mode = %d %v", mode, code, body)
			}
		})
	}
}

// The page assets must be served with the right caching class: stable names
// (app.css, the island bundle, the vendored htmx files, the concept GIFs) are
// no-cache + ETag; an unknown name is a 404. The hashed `assets/` lane stays
// immutable for content-addressed files if any ship.
func TestServeUIAssetsCaching(t *testing.T) {
	s, h := setupMergeServer(t, t.TempDir())
	s.cfg.Server.Auth.Mode = "none"
	s.cfg.Server.Auth.AnonymousRead = true
	s.boot.Mode = "normal"

	for _, tc := range []struct{ name, ctype string }{
		{"app.css", "text/css; charset=utf-8"},
		{"ui.js", "text/javascript; charset=utf-8"},
		{"htmx.min.js", "text/javascript; charset=utf-8"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/"+tc.name, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d (run make web)", tc.name, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s cache-control = %q, want no-cache", tc.name, cc)
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.ctype {
			t.Fatalf("%s content-type = %q, want %q", tc.name, ct, tc.ctype)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("%s must carry an ETag", tc.name)
		}
	}

	// The landing/how-it-works concept GIFs ride the same stable lane.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/concepts/push-still.gif", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("concept gif = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("concept gif cache-control = %q, want no-cache", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/nope.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown asset = %d", rec.Code)
	}
	// The SPA shell is gone: /_ui/index.html is not an asset any more.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/index.html", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("index.html = %d, want 404", rec.Code)
	}
}

// The auth subsection is its own schema group (the setup page's auth card),
// immediately after server, and the server group no longer lists auth keys.
func TestSetupSchemaSplitsAuthGroup(t *testing.T) {
	s, _ := setupMergeServer(t, t.TempDir())
	groups := setup.BuildSchema(s.cfg, config.Defaults())
	if len(groups) < 3 || groups[0].Section != "server" || groups[1].Section != "auth" || groups[2].Section != "store" {
		sections := make([]string, len(groups))
		for i, g := range groups {
			sections[i] = g.Section
		}
		t.Fatalf("group sections = %v, want [server auth store ...]", sections)
	}
	for _, k := range groups[0].Keys {
		if strings.HasPrefix(k.Key, "server.auth.") {
			t.Fatalf("server group still carries auth key %q", k.Key)
		}
	}
	foundMode := false
	for _, k := range groups[1].Keys {
		if !strings.HasPrefix(k.Key, "server.auth.") {
			t.Fatalf("auth group carries non-auth key %q", k.Key)
		}
		if k.Key == "server.auth.mode" {
			foundMode = true
		}
	}
	if !foundMode {
		t.Fatal("auth group is missing server.auth.mode")
	}
}

// POST /api/v1/setup/auth/test probes the OIDC discovery document of the
// issuer in the request: ok, issuer mismatch, unreachable, and missing
// required endpoints.
func TestSetupAuthTestDiscovery(t *testing.T) {
	doc := map[string]any{
		"issuer":                 "", // filled per case
		"authorization_endpoint": "https://id.example.com/authorize",
		"token_endpoint":         "https://id.example.com/token",
		"jwks_uri":               "https://id.example.com/jwks",
	}
	var served map[string]any
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(served)
	}))
	defer idp.Close()

	post := func(s *Server, body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/v1/setup/auth/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.setupAuthTest(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	errMsgs := func(out map[string]any) []string {
		var msgs []string
		for _, e := range out["errors"].([]any) {
			m, _ := e.(map[string]any)["message"].(string)
			msgs = append(msgs, m)
		}
		return msgs
	}

	s, _ := setupMergeServer(t, t.TempDir())

	// ok: discovery document whose issuer matches the configured value.
	served = map[string]any{"issuer": idp.URL, "authorization_endpoint": doc["authorization_endpoint"],
		"token_endpoint": doc["token_endpoint"], "jwks_uri": doc["jwks_uri"]}
	code, out := post(s, fmt.Sprintf(`{"issuer": %q, "client_id": "walhub-web", "redirect_uri": "http://localhost:8080/_auth/callback"}`, idp.URL))
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("ok case = %d %v", code, out)
	}
	if out["authorization_endpoint"] != "https://id.example.com/authorize" {
		t.Fatalf("endpoints missing from the response: %v", out)
	}

	// issuer mismatch: the document names a different issuer → 422.
	served["issuer"] = "https://elsewhere.example.com"
	code, out = post(s, fmt.Sprintf(`{"issuer": %q}`, idp.URL))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch case = %d %v", code, out)
	}
	if msgs := errMsgs(out); len(msgs) != 1 || !strings.Contains(msgs[0], "does not match") {
		t.Fatalf("mismatch messages = %v", msgs)
	}

	// missing required endpoint → 422.
	served["issuer"] = idp.URL
	delete(served, "jwks_uri")
	code, out = post(s, fmt.Sprintf(`{"issuer": %q}`, idp.URL))
	if code != http.StatusUnprocessableEntity || !strings.Contains(strings.Join(errMsgs(out), "; "), "jwks_uri") {
		t.Fatalf("missing-endpoint case = %d %v", code, out)
	}

	// unreachable issuer → 422 with a fetch failure message.
	code, out = post(s, `{"issuer": "http://127.0.0.1:1"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(strings.Join(errMsgs(out), "; "), "discovery fetch failed") {
		t.Fatalf("unreachable case = %d %v", code, out)
	}

	// missing issuer → 422, keyed to server.auth.issuer.
	code, out = post(s, `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-issuer case = %d %v", code, out)
	}
	if keys, _ := out["errors"].([]any); len(keys) == 0 || keys[0].(map[string]any)["key"] != "server.auth.issuer" {
		t.Fatalf("missing-issuer errors = %v", out)
	}
}

// Issue #215: server.ssh.external_port (the advertised SSH port) round-trips
// through the setup surface — accepted by POST /api/v1/setup/test in
// isolation, persisted by PUT with effect, rejected when out of range.
func TestSetupSSHExternalPortRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "normal"

	for _, v := range []string{"12222", "2222"} {
		body, _ := json.Marshal(map[string]any{"overrides": map[string]any{"server.ssh.external_port": v}})
		req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.setupTest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup/test external_port=%s = %d %s, want 200", v, rec.Code, rec.Body.String())
		}
	}
	for _, v := range []string{"70000", "-1", "many"} {
		body, _ := json.Marshal(map[string]any{"overrides": map[string]any{"server.ssh.external_port": v}})
		req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.setupTest(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("setup/test external_port=%s = %d %s, want 422", v, rec.Code, rec.Body.String())
		}
	}

	// The normalized UI payload (coerced int, like normalizeSetup emits)
	// saves and persists with effect.
	code, resp := putSetup(t, s,
		`{"overrides": {"server.ssh.listen": "0.0.0.0:2222", "server.ssh.external_port": 12222}}`)
	if code != http.StatusOK {
		t.Fatalf("put ssh keys = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Server.SSH.Listen != "0.0.0.0:2222" || after.Server.SSH.ExternalPort != 12222 {
		t.Fatalf("ssh keys not persisted: listen=%q external=%d",
			after.Server.SSH.Listen, after.Server.SSH.ExternalPort)
	}
	if got := after.Server.SSH.AdvertisedSSHPort(); got != 12222 {
		t.Fatalf("advertised port = %d, want 12222", got)
	}
	rr, _ := resp["requires_restart"].([]any)
	found := false
	for _, k := range rr {
		if k == "server.ssh.external_port" {
			found = true
		}
	}
	if !found {
		t.Fatalf("requires_restart = %v, want server.ssh.external_port listed", resp["requires_restart"])
	}
}
