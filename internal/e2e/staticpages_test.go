// staticpages_test.go — Feature 19 (docs/features/19_static_sites.md) end to
// end: a repository whose committed bytes are published on the dedicated
// static listener, on its own port and its own Host, while the application
// origin keeps its own routes.
//
// The scenario drives the feature exactly as it is wired: the host section
// comes through the SPINDLE__ env overlay (exact dotted paths —
// SPINDLE__STATIC_SITES__*), the per-repo [static_site] document is written
// through the JSON settings API the settings page uses (never a hand-written
// bucket object), and publication is observed through the static listener only.
//
// Real server subprocess, real git binary, real OIDC bootstrap (oidp_test.go).
// Runs in the e2e tier (make e2e), like the other e2e scenarios.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	staticIndexHTML = "<h1>hello site</h1>"
	staticCSS       = "body{color:red}"
	staticDocsHTML  = "<h1>docs index</h1>"
	staticAboutHTML = "<h1>about page</h1>"
	static404HTML   = "<h1>gone</h1>"
	staticSecret    = "top-secret\n"
)

// staticResp is one raw response from the static listener (or the app port for
// the untouched-app sanity check).
type staticResp struct {
	status int
	header http.Header
	body   []byte
}

// doStatic issues one request. host, when non-empty, overrides the Host header
// (req.Host, since net/http ignores a "Host" entry in Header); the connection
// still goes to the URL's address, which is exactly what a misconfigured proxy
// would do. Every request carries a hard timeout.
func doStatic(t *testing.T, method, url, host string, headers map[string]string) (staticResp, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		// The 301s are assertions, not navigation: never follow them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return staticResp{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return staticResp{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// mustStatic is doStatic for assertions: a transport failure fails the test.
func mustStatic(t *testing.T, method, url, host string, headers map[string]string) staticResp {
	t.Helper()
	r, err := doStatic(t, method, url, host, headers)
	if err != nil {
		t.Fatalf("%s %s (Host %q): %v", method, url, host, err)
	}
	return r
}

// staticTestHost is the Host the static listener is configured with
// (SPINDLE__STATIC_SITES__HOST). It is deliberately distinct from the app's own
// hostname: the listener must never answer on the session origin (§2).
const staticTestHost = "static.localhost"

// mustSite issues a request to the static listener with the configured Host.
// The connection still goes to 127.0.0.1 (the app's host), so passing the site
// host explicitly also proves the listener's Host gate is what routes it.
func mustSite(t *testing.T, method, url string, headers map[string]string) staticResp {
	t.Helper()
	return mustStatic(t, method, url, staticTestHost, headers)
}

// expectStatic asserts one response shape and logs the observed values so a
// verbose run prints them. wantBody == nil skips the body comparison; wantCT
// == "" skips the Content-Type comparison.
func expectStatic(t *testing.T, what string, r staticResp, wantStatus int, wantBody *string, wantCT string) {
	t.Helper()
	if r.status != wantStatus {
		t.Fatalf("%s: status = %d, want %d (body %q)", what, r.status, wantStatus, r.body)
	}
	if wantBody != nil && string(r.body) != *wantBody {
		t.Fatalf("%s: body = %q, want %q", what, r.body, *wantBody)
	}
	if wantCT != "" {
		if got := r.header.Get("Content-Type"); got != wantCT {
			t.Fatalf("%s: Content-Type = %q, want %q", what, got, wantCT)
		}
	}
	t.Logf("%-46s %d  Content-Type=%q  Cache-Control=%q  ETag=%q  bytes=%d",
		what, r.status, r.header.Get("Content-Type"), r.header.Get("Cache-Control"),
		r.header.Get("ETag"), len(r.body))
}

func strptr(s string) *string { return &s }

// TestE2E_StaticSites publishes one repository and drives the whole static-site
// contract against the static listener: index, asset, directory redirects,
// .html fallback, site 404, revalidation, ranges, HEAD, 405, the Host gate
// (including the app's own hostname, which must be refused), dotfile/symlink
// refusal, the private-repo 404, and enabled = false binding nothing. It then
// proves the app origin still serves its own repo page.
func TestE2E_StaticSites(t *testing.T) {
	if testing.Short() {
		t.Skip("static-site scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	p := newOIDP(t)
	admin := p.idToken("alice@example.com", "spindle-admin")
	e2eDefaultBearer = admin
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))

	// The static listener gets its own free port and one Host, distinct from
	// the app's own hostname (127.0.0.1) — the configuration validation refuses
	// the collision, and the Host gate below proves the refusal at request time.
	// The overlay paths are exact (docs/go/11_config_cli.md §3): a missing
	// segment would be recorded as an unknown key and silently ignored, leaving
	// the section off.
	staticPort := freePort(t)
	s := startServerWithEnv(t, bin, dataDir, freePort(t),
		"SPINDLE__STATIC_SITES__ENABLED=true",
		fmt.Sprintf("SPINDLE__STATIC_SITES__ADDRESS=127.0.0.1:%d", staticPort),
		"SPINDLE__STATIC_SITES__HOST="+staticTestHost,
	)
	t.Cleanup(s.stop)

	// ---- 1. publish the repository through the real git path -------------------
	repo := uniqueRepo("static")
	work := filepath.Join(t.TempDir(), "site")
	g.initRepo(work)
	g.commitFile(work, "index.html", staticIndexHTML, "site index")
	g.commitFile(work, "assets/app.css", staticCSS, "site css")
	g.commitFile(work, "docs/index.html", staticDocsHTML, "site docs")
	g.commitFile(work, "pages/about.html", staticAboutHTML, "site about")
	g.commitFile(work, "404.html", static404HTML, "site 404")
	g.commitFile(work, ".secret", staticSecret, "site dotfile")
	if err := os.Symlink("index.html", filepath.Join(work, "link.html")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	g.run(work, "add", "link.html")
	g.run(work, "commit", "-m", "site symlink")
	g.run(work, "remote", "add", "origin", s.gitURL(testOwner, repo))
	g.run(work, "push", "-u", "origin", "main")

	// ---- 2. opt the repo in through the settings API the page uses -------------
	appLane := s.base + "/" + testOwner + "/" + repo + "/api"
	status, body := httpRequestAuth(t, http.MethodPut, appLane+"/settings",
		[]byte("[static_site]\nenabled = true\n"), admin)
	if status != http.StatusOK {
		t.Fatalf("PUT %s/settings = %d, want 200 (body %s)", appLane, status, body)
	}
	var published struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &published); err != nil || published.Revision == 0 {
		t.Fatalf("settings PUT response = %s, want {\"revision\":N>0}", body)
	}
	var doc struct {
		TOML     string `json:"toml"`
		Revision uint64 `json:"revision"`
	}
	getJSON(t, appLane+"/settings", &doc)
	if doc.Revision != published.Revision ||
		!strings.Contains(doc.TOML, "[static_site]") ||
		!strings.Contains(doc.TOML, "enabled = true") {
		t.Fatalf("settings read-back = %+v, want the published [static_site] enabled = true at revision %d",
			doc, published.Revision)
	}
	t.Logf("published [static_site] enabled = true at revision %d", doc.Revision)

	// ---- the static listener is up and the site is published --------------------
	siteBase := fmt.Sprintf("http://127.0.0.1:%d/%s/%s", staticPort, testOwner, repo)
	// The listener binds during serve; wait for a response (then for the
	// published state), and fail with the last response so a wiring fault is
	// visible rather than hidden behind a timeout.
	deadline := time.Now().Add(20 * time.Second)
	var last staticResp
	var lastErr error
	for {
		last, lastErr = doStatic(t, http.MethodGet, siteBase+"/", staticTestHost, nil)
		if lastErr == nil && last.status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("static listener %s never served the published site within 20s: status %d err %v body %q",
				siteBase+"/", last.status, lastErr, last.body)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// ---- 3. the static-site contract, on the static listener only ---------------
	index := mustSite(t, http.MethodGet, siteBase+"/", nil)
	expectStatic(t, "GET /{o}/{r}/ (index)", index, http.StatusOK, strptr(staticIndexHTML), "text/html; charset=utf-8")
	if cc := index.header.Get("Cache-Control"); cc != "public, max-age=600" {
		t.Fatalf("index Cache-Control = %q, want %q", cc, "public, max-age=600")
	}
	etag := index.header.Get("ETag")
	if len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Fatalf("index ETag = %q, want a quoted strong validator", etag)
	}

	css := mustSite(t, http.MethodGet, siteBase+"/assets/app.css", nil)
	expectStatic(t, "GET …/assets/app.css", css, http.StatusOK, strptr(staticCSS), "text/css; charset=utf-8")

	bare := mustSite(t, http.MethodGet, siteBase, nil)
	expectStatic(t, "GET /{o}/{r} (no slash)", bare, http.StatusMovedPermanently, nil, "")
	if got, want := bare.header.Get("Location"), "/"+testOwner+"/"+repo+"/"; got != want {
		t.Fatalf("bare Location = %q, want %q", got, want)
	}
	t.Logf("%-46s %d  Location=%q", "GET /{o}/{r}", bare.status, bare.header.Get("Location"))

	bareQ := mustSite(t, http.MethodGet, siteBase+"?x=1", nil)
	expectStatic(t, "GET /{o}/{r}?x=1", bareQ, http.StatusMovedPermanently, nil, "")
	if got, want := bareQ.header.Get("Location"), "/"+testOwner+"/"+repo+"/?x=1"; got != want {
		t.Fatalf("bare Location with query = %q, want %q", got, want)
	}
	t.Logf("%-46s %d  Location=%q", "GET /{o}/{r}?x=1", bareQ.status, bareQ.header.Get("Location"))

	docsRedirect := mustSite(t, http.MethodGet, siteBase+"/docs", nil)
	expectStatic(t, "GET …/docs (tree, no slash)", docsRedirect, http.StatusMovedPermanently, nil, "")
	if got, want := docsRedirect.header.Get("Location"), "/"+testOwner+"/"+repo+"/docs/"; got != want {
		t.Fatalf("docs Location = %q, want %q", got, want)
	}
	t.Logf("%-46s %d  Location=%q", "GET …/docs", docsRedirect.status, docsRedirect.header.Get("Location"))

	docs := mustSite(t, http.MethodGet, siteBase+"/docs/", nil)
	expectStatic(t, "GET …/docs/ (directory index)", docs, http.StatusOK, strptr(staticDocsHTML), "text/html; charset=utf-8")

	about := mustSite(t, http.MethodGet, siteBase+"/pages/about", nil)
	expectStatic(t, "GET …/pages/about (.html fallback)", about, http.StatusOK, strptr(staticAboutHTML), "text/html; charset=utf-8")

	nope := mustSite(t, http.MethodGet, siteBase+"/nope", nil)
	expectStatic(t, "GET …/nope (site 404.html)", nope, http.StatusNotFound, strptr(static404HTML), "text/html; charset=utf-8")
	if cc, et := nope.header.Get("Cache-Control"), nope.header.Get("ETag"); cc != "" || et != "" {
		t.Fatalf("404 cache headers: Cache-Control=%q ETag=%q, want neither", cc, et)
	}

	revalidated := mustSite(t, http.MethodGet, siteBase+"/", map[string]string{"If-None-Match": etag})
	expectStatic(t, "GET / (If-None-Match)", revalidated, http.StatusNotModified, strptr(""), "")
	if got := revalidated.header.Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}

	ranged := mustSite(t, http.MethodGet, siteBase+"/assets/app.css", map[string]string{"Range": "bytes=0-3"})
	expectStatic(t, "GET …/app.css (Range 0-3)", ranged, http.StatusPartialContent, strptr("body"), "text/css; charset=utf-8")
	if cr := ranged.header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-3/") {
		t.Fatalf("Content-Range = %q, want bytes 0-3/<size>", cr)
	}
	t.Logf("%-46s %d  Content-Range=%q", "GET …/app.css Range 0-3", ranged.status, ranged.header.Get("Content-Range"))

	head := mustSite(t, http.MethodHead, siteBase+"/", nil)
	expectStatic(t, "HEAD /", head, http.StatusOK, strptr(""), "text/html; charset=utf-8")
	if head.header.Get("ETag") != etag || head.header.Get("Cache-Control") != "public, max-age=600" {
		t.Fatalf("HEAD headers differ from GET: ETag=%q Cache-Control=%q",
			head.header.Get("ETag"), head.header.Get("Cache-Control"))
	}

	post := mustSite(t, http.MethodPost, siteBase+"/", nil)
	expectStatic(t, "POST / (method gate)", post, http.StatusMethodNotAllowed, nil, "")
	if allow := post.header.Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("405 Allow = %q, want %q", allow, "GET, HEAD")
	}
	t.Logf("%-46s %d  Allow=%q", "POST /", post.status, post.header.Get("Allow"))

	// ---- the Host gate ----------------------------------------------------------
	// The configured Host is static.localhost. Its own port spelling is the
	// same host by design (§2: the comparison is port-insensitive), so an
	// explicit ":<static port>" keeps serving — a 404 there would be a
	// port-sensitivity regression:
	sameHost := mustStatic(t, http.MethodGet, siteBase+"/", fmt.Sprintf("%s:%d", staticTestHost, staticPort), nil)
	expectStatic(t, "GET / Host=static.localhost:<static port> (port-insensitive)", sameHost, http.StatusOK,
		strptr(staticIndexHTML), "text/html; charset=utf-8")
	// The application's own host:port is a DIFFERENT origin and must be refused
	// with a bodiless 404, even though the connection is the same loopback
	// address: that is the proxy-pointed-at-the-wrong-listener case §2 exists
	// for, and answering here would serve user HTML/JS on the session origin.
	appHost := fmt.Sprintf("127.0.0.1:%d", s.port)
	appHostReq := mustStatic(t, http.MethodGet, siteBase+"/", appHost, nil)
	expectStatic(t, "GET / Host=127.0.0.1:<app port> (the app origin)", appHostReq, http.StatusNotFound, strptr(""), "")
	if len(appHostReq.body) != 0 {
		t.Fatalf("app-origin Host 404 carried %d body bytes, want none", len(appHostReq.body))
	}
	// Any other hostname is refused the same way. 127.0.0.2 is only a Host
	// spelling here (the connection still goes to 127.0.0.1).
	wrongHost := mustStatic(t, http.MethodGet, siteBase+"/", fmt.Sprintf("127.0.0.2:%d", s.port), nil)
	expectStatic(t, "GET / Host=127.0.0.2:<app port>", wrongHost, http.StatusNotFound, strptr(""), "")
	if len(wrongHost.body) != 0 {
		t.Fatalf("wrong-Host 404 carried %d body bytes, want none", len(wrongHost.body))
	}

	// ---- dotfile and symlink refusal -------------------------------------------
	dotfile := mustSite(t, http.MethodGet, siteBase+"/.secret", nil)
	expectStatic(t, "GET …/.secret (dotfile)", dotfile, http.StatusNotFound, nil, "")
	if bytes.Contains(dotfile.body, []byte(staticSecret)) {
		t.Fatalf("dotfile refused with its own bytes in the body: %q", dotfile.body)
	}
	symlink := mustSite(t, http.MethodGet, siteBase+"/link.html", nil)
	expectStatic(t, "GET …/link.html (symlink)", symlink, http.StatusNotFound, nil, "")
	if string(symlink.body) == "index.html" {
		t.Fatalf("symlink served its target text as a blob: %q", symlink.body)
	}

	// ---- 4. the app origin is untouched ----------------------------------------
	// The repo page route on the application port, before the visibility flip.
	appPage := mustStatic(t, http.MethodGet, s.base+"/"+testOwner+"/"+repo, "", nil)
	expectStatic(t, "GET <app>/{o}/{r} (app page)", appPage, http.StatusOK, nil, "")

	// ---- 5. a private repository is a plain 404 on the static listener ---------
	// The access document is an authenticated read (triage), so both calls carry
	// the admin bearer — the same shape the collaboration e2e uses.
	st, data := apiCall(t, http.MethodGet, appLane+"/access", "", admin)
	if st != http.StatusOK {
		t.Fatalf("GET %s/access = %d, want 200 (body %s)", appLane, st, data)
	}
	var access struct {
		Version float64 `json:"version"`
	}
	if err := json.Unmarshal(data, &access); err != nil {
		t.Fatalf("decode access: %v (body %s)", err, data)
	}
	st, data = apiCall(t, http.MethodPut, appLane+"/access",
		fmt.Sprintf(`{"version":%d,"visibility":"private","role_bindings":[]}`, int(access.Version)), admin)
	if st < 200 || st > 299 {
		t.Fatalf("PUT %s/access = %d, want 2xx (body %s)", appLane, st, data)
	}
	var after struct {
		Visibility string `json:"visibility"`
	}
	st, data = apiCall(t, http.MethodGet, appLane+"/access", "", admin)
	if st != http.StatusOK {
		t.Fatalf("GET %s/access after the flip = %d (body %s)", appLane, st, data)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("decode access after the flip: %v (body %s)", err, data)
	}
	if after.Visibility != "private" {
		t.Fatalf("access visibility = %q after the flip, want private", after.Visibility)
	}

	privateIndex := mustSite(t, http.MethodGet, siteBase+"/", nil)
	expectStatic(t, "GET / (private repo)", privateIndex, http.StatusNotFound, strptr("not found"), "text/plain; charset=utf-8")
	if bytes.Contains(privateIndex.body, []byte(staticIndexHTML)) {
		t.Fatalf("private repo 404 leaked site bytes: %q", privateIndex.body)
	}
	if cc, et := privateIndex.header.Get("Cache-Control"), privateIndex.header.Get("ETag"); cc != "" || et != "" {
		t.Fatalf("private 404 cache headers: Cache-Control=%q ETag=%q, want neither", cc, et)
	}

	// ---- 6. enabled = false binds nothing --------------------------------------
	// A disabled section is not "a listener that answers less": serve never
	// constructs it, so the configured address is not bound at all. The first
	// instance is stopped (its own cleanup is idempotent), then a second one
	// boots with the section explicitly off and the same shape of address, and
	// connecting there must be refused — an accepted connection would mean every
	// instance keeps an unused origin open.
	s.stop()
	offPort := freePort(t)
	offAddr := fmt.Sprintf("127.0.0.1:%d", offPort)
	off := startServerWithEnv(t, bin, dataDir, freePort(t),
		"SPINDLE__STATIC_SITES__ENABLED=false",
		"SPINDLE__STATIC_SITES__ADDRESS="+offAddr,
	)
	t.Cleanup(off.stop)
	conn, dialErr := net.DialTimeout("tcp", offAddr, 2*time.Second)
	if dialErr == nil {
		conn.Close()
		t.Fatalf("static address %s accepted a connection with static_sites.enabled = false; the listener must not be constructed", offAddr)
	}
	t.Logf("%-46s refused  err=%v", "dial "+offAddr+" (enabled = false)", dialErr)
}
