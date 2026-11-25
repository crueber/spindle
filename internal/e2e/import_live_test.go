// import_live_test.go — the operator's two loops, end to end against the real
// binary and the in-test IdP:
//
//  1. Saving the import SSRF policy on /admin/config takes effect on the NEXT
//     import, with no restart. Before the fix the page kept answering
//     `source host "…" resolves to a private address
//     (import.allow_private_networks=false)` after a successful save.
//
//  2. Booted with an explicit --config file, the save writes THAT file. Before
//     the fix it wrote <data-dir>/spindle.toml, so a restart loaded the old
//     values from the explicit file and every edit was lost.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// browserPost issues a form POST on the canonical host with the Origin header a
// browser sends (the settings and forms handlers refuse a headerless write).
func browserPost(t *testing.T, c *http.Client, port int, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	u := fmt.Sprintf("http://walgit.localhost:%d%s", port, path)
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", fmt.Sprintf("http://walgit.localhost:%d", port))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	body := new(strings.Builder)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	resp.Body.Close()
	return resp, body.String()
}

// signIn drives the real OIDC login as a host admin and returns the client.
func signIn(t *testing.T, port int, p *oidp) *http.Client {
	t.Helper()
	c := oidcBrowserClient(t, port)
	if status, body := loginThroughIdP(t, c, port, p, "alice@example.com", "spindle-admin", "/"); status != http.StatusOK {
		t.Fatalf("admin sign-in = %d (%s)", status, body)
	}
	return c
}

// makeFileSource builds a real bare repo on local disk and returns its file://
// URL plus the commit sha (git http-backend is not needed: the file transport
// exercises the same import pipeline — normalize → SSRF gate → clone → publish).
func makeFileSource(t *testing.T, g *gitClient) (url, sha string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "source.git")
	g.run(t.TempDir(), "init", "--bare", "-b", "main", bare)
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	sha = g.commitFile(work, "README.md", "imported live\n", "seed")
	g.run(work, "push", bare, "main")
	return "file://" + bare, sha
}

// waitForRepoRef polls the smart-HTTP advertised refs until the imported repo
// answers with want (the import is a task; the page returns a progress card).
func waitForRepoRef(t *testing.T, g *gitClient, gitURL, want string, deadline time.Duration) {
	t.Helper()
	at := time.Now().Add(deadline)
	last := ""
	for time.Now().Before(at) {
		code, out := g.cmdDeadline(5*time.Second, g.sandbox, "ls-remote", gitURL)
		if code == 0 && strings.Contains(out, want) {
			return
		}
		last = out
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("repo %s never advertised %s (last: %q)", gitURL, want, last)
}

// TestImportPolicySaveAppliesWithoutRestart is the reported defect's
// acceptance test: refused, saved on /admin/config, accepted — one process.
func TestImportPolicySaveAppliesWithoutRestart(t *testing.T) {
	requireModernGit(t)
	p := newOIDP(t)
	dataDir := t.TempDir()
	writeE2EConfig(t, dataDir, p)
	s := startServer(t, buildSpindle(t), dataDir, freePort(t))
	t.Cleanup(s.stop)

	g := newGitClient(t)
	source, sha := makeFileSource(t, g)
	c := signIn(t, s.port, p)

	importForm := func(name string) url.Values {
		return url.Values{
			"kind": {"import"}, "source_url": {source},
			"owner": {testOwner}, "name": {name},
		}
	}

	// 1. The page refuses the file source while allow_file_urls=false.
	resp, body := browserPost(t, c, s.port, "/import", importForm("live-refused"))
	if resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "file:// sources are disabled (import.allow_file_urls=false)") {
		t.Fatalf("pre-save import = %d, want the file:// refusal (body %.400s)", resp.StatusCode, body)
	}

	// 2. The same page also refuses a loopback HTTP source as private — the
	// reported sentence (no listener needed: the gate resolves before dialing).
	_, body = browserPost(t, c, s.port, "/import", url.Values{
		"kind": {"import"}, "source_url": {"http://127.0.0.1/probe.git"},
		"owner": {testOwner}, "name": {"live-probe"}, "dangerous": {"on"},
	})
	if !strings.Contains(body, "resolves to a private address (import.allow_private_networks=false)") {
		t.Fatalf("pre-save loopback import lacks the reported sentence (body %.400s)", body)
	}

	// 3. Save the policy on /admin/config — the operator's save, no restart.
	resp, body = browserPost(t, c, s.port, "/admin/config", url.Values{
		"op":                                {"save"},
		"cfg.import.allow_private_networks": {"true"},
		"cfg.import.allow_file_urls":        {"true"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d, want 303 (body %.400s)", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "saved=1") {
		t.Fatalf("save redirect = %q, want saved=1", loc)
	}
	// The restart list must not name the import policy it just made live.
	if strings.Contains(loc, "import.") {
		t.Fatalf("restart list = %q, want no import.* key", loc)
	}
	// The saved page names the file it wrote (the data-dir file in this boot).
	savedGet := fmt.Sprintf("http://walgit.localhost:%d%s", s.port, loc)
	respGet, err := c.Get(savedGet)
	if err != nil {
		t.Fatalf("GET %s: %v", savedGet, err)
	}
	savedBody := new(strings.Builder)
	sb := make([]byte, 32<<10)
	for {
		n, rerr := respGet.Body.Read(sb)
		savedBody.Write(sb[:n])
		if rerr != nil {
			break
		}
	}
	respGet.Body.Close()
	wantPath := filepath.Join(dataDir, "spindle.toml")
	if !strings.Contains(savedBody.String(), wantPath) {
		t.Fatalf("the saved page does not name %q", wantPath)
	}

	// 4. The same import now passes the gate and creates the repository.
	resp, body = browserPost(t, c, s.port, "/import", importForm("live-accepted"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-save import = %d, want the progress page (body %.400s)", resp.StatusCode, body)
	}
	if strings.Contains(body, "import.allow_file_urls=false") {
		t.Fatalf("the refusal survived the save (body %.400s)", body)
	}
	waitForRepoRef(t, g, s.gitURL(testOwner, "live-accepted"), sha, 60*time.Second)

	// 5. The loopback probe no longer answers with the private-address refusal:
	// it starts (and fails later at the clone, since nothing listens on :80).
	_, body = browserPost(t, c, s.port, "/import", url.Values{
		"kind": {"import"}, "source_url": {"http://127.0.0.1/probe.git"},
		"owner": {testOwner}, "name": {"live-probe-2"}, "dangerous": {"on"},
	})
	if strings.Contains(body, "import.allow_private_networks=false") {
		t.Fatalf("the private-address refusal survived the save (body %.400s)", body)
	}
}

// TestImportPolicySaveSurvivesRestartFromExplicitConfig is the operator's exact
// loop: --config <tmp>/etc/spindle.toml, save import.* on /admin/config,
// restart, and the running config has the values.
func TestImportPolicySaveSurvivesRestartFromExplicitConfig(t *testing.T) {
	p := newOIDP(t)
	dataDir := t.TempDir()
	explicitDir := filepath.Join(t.TempDir(), "etc")
	if err := os.MkdirAll(explicitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(explicitDir, "spindle.toml")
	cfgBody := fmt.Sprintf(`[server]
auto_create_on_push = true

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache"))
	if err := os.WriteFile(explicit, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write explicit config: %v", err)
	}
	bin := buildSpindle(t)
	s := startServerArgs(t, bin, dataDir, freePort(t), []string{"--config", explicit})
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.stop()
		}
	})

	c := signIn(t, s.port, p)
	// The page names the file it will write.
	resp, page := browserDo(t, c, s.port, http.MethodGet, "/admin/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", resp.StatusCode)
	}
	if !strings.Contains(page, explicit) {
		t.Fatalf("the Configuration page does not name %q", explicit)
	}

	// Save the import policy through the page.
	resp, body := browserPost(t, c, s.port, "/admin/config", url.Values{
		"op":                                {"save"},
		"cfg.import.allow_private_networks": {"true"},
		"cfg.import.url_allowlist":          {"github.com"},
		"cfg.import.allow_file_urls":        {"true"},
		"cfg.import.max_concurrent":         {"2"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d, want 303 (body %.400s)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, url.QueryEscape(explicit)) {
		t.Fatalf("save redirect = %q, want the explicit path", loc)
	}

	// The edit landed in the loaded file, and the data dir gained no shadow.
	saved, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("read explicit config: %v", err)
	}
	for _, want := range []string{"allow_private_networks = true", `url_allowlist = ["github.com"]`, "allow_file_urls = true"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("the loaded file lacks %q:\n%s", want, saved)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "spindle.toml")); !os.IsNotExist(statErr) {
		t.Fatal("the page save created a data-dir shadow spindle.toml")
	}

	// The operator restarts: the running config must carry the values.
	s.stop()
	stopped = true
	s2 := startServerArgs(t, bin, dataDir, freePort(t), []string{"--config", explicit})
	t.Cleanup(s2.stop)
	status, raw := httpRequestAuth(t, http.MethodGet, s2.base+"/api/v1/setup", nil, p.idToken("alice@example.com", "spindle-admin"))
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/setup after restart = %d (%s)", status, raw)
	}
	var view struct {
		Groups []struct {
			Keys []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"keys"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	values := map[string]any{}
	for _, g := range view.Groups {
		for _, k := range g.Keys {
			values[k.Key] = k.Value
		}
	}
	if v, _ := values["import.allow_private_networks"].(bool); !v {
		t.Fatalf("import.allow_private_networks = %v after restart, want true", values["import.allow_private_networks"])
	}
	if v, _ := values["import.allow_file_urls"].(bool); !v {
		t.Fatalf("import.allow_file_urls = %v after restart, want true", values["import.allow_file_urls"])
	}
	if list, _ := values["import.url_allowlist"].([]any); len(list) != 1 || list[0] != "github.com" {
		t.Fatalf("import.url_allowlist = %v after restart, want [github.com]", values["import.url_allowlist"])
	}
}

// TestImportPolicySaveCreatesMissingExplicitConfig is the create path of the
// save-target rule against the real binary: booted with --config, the file is
// gone at save time (a config manager replaced or dropped it), so the save must
// create THAT path — not a data-dir shadow the next --config boot cannot find.
// (A boot whose --config file is missing from the start is exit 2 by contract,
// §6.1, so removing it mid-run is the way to reach this rule in a live process.)
func TestImportPolicySaveCreatesMissingExplicitConfig(t *testing.T) {
	p := newOIDP(t)
	dataDir := t.TempDir()
	explicitDir := filepath.Join(t.TempDir(), "etc")
	if err := os.MkdirAll(explicitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(explicitDir, "spindle.toml")
	cfgBody := fmt.Sprintf(`[server]
auto_create_on_push = true

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache"))
	if err := os.WriteFile(explicit, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write explicit config: %v", err)
	}
	bin := buildSpindle(t)
	s := startServerArgs(t, bin, dataDir, freePort(t), []string{"--config", explicit})
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.stop()
		}
	})

	c := signIn(t, s.port, p)
	// The file disappears under the running process.
	if err := os.Remove(explicit); err != nil {
		t.Fatalf("remove explicit config: %v", err)
	}

	// With the file gone the page's base is the first-run shape again, so the
	// save carries the OIDC keys too — exactly what the onboarding card posts.
	resp, body := browserPost(t, c, s.port, "/admin/config", url.Values{
		"op":                                  {"save"},
		"cfg.server.auth.issuer":              {p.issuer},
		"cfg.server.auth.oauth_client_id":     {p.client},
		"cfg.server.auth.oauth_client_secret": {p.secret},
		"cfg.server.auth.session_secret":      {"e2e-session-secret-0123456789abcdef"},
		"cfg.server.auth.allowed_domains":     {"example.com"},
		"cfg.server.auth.admin_groups":        {"spindle-admin"},
		"cfg.import.allow_private_networks":   {"true"},
		"cfg.import.url_allowlist":            {"github.com"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d, want 303 (body %.400s)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, url.QueryEscape(explicit)) {
		t.Fatalf("save redirect = %q, want the created path %q", loc, explicit)
	}
	created, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("the save did not create the named file: %v", err)
	}
	for _, want := range []string{"allow_private_networks = true", `url_allowlist = ["github.com"]`} {
		if !strings.Contains(string(created), want) {
			t.Fatalf("the created file lacks %q:\n%s", want, created)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "spindle.toml")); !os.IsNotExist(statErr) {
		t.Fatal("the save created a data-dir shadow spindle.toml")
	}

	// A restart with the same --config reads the values back.
	s.stop()
	stopped = true
	s2 := startServerArgs(t, bin, dataDir, freePort(t), []string{"--config", explicit})
	t.Cleanup(s2.stop)
	status, raw := httpRequestAuth(t, http.MethodGet, s2.base+"/api/v1/setup", nil, p.idToken("alice@example.com", "spindle-admin"))
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/setup after restart = %d (%s)", status, raw)
	}
	var view struct {
		Groups []struct {
			Keys []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"keys"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	values := map[string]any{}
	for _, g := range view.Groups {
		for _, k := range g.Keys {
			values[k.Key] = k.Value
		}
	}
	if v, _ := values["import.allow_private_networks"].(bool); !v {
		t.Fatalf("import.allow_private_networks = %v after restart, want true", values["import.allow_private_networks"])
	}
	if list, _ := values["import.url_allowlist"].([]any); len(list) != 1 || list[0] != "github.com" {
		t.Fatalf("import.url_allowlist = %v after restart, want [github.com]", values["import.url_allowlist"])
	}
}
