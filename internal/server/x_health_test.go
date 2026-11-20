package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI is a RouteProvider stub for the api lanes.
type fakeAPI struct {
	served int
}

func (f *fakeAPI) Serve(w http.ResponseWriter, r *http.Request) {
	f.served++
	w.WriteHeader(http.StatusTeapot)
}

func TestHealthzAndPrewarm(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"testsha"`) {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body.String())
	}
	SetPrewarmPending(3)
	defer SetPrewarmPending(0)
	rec = httptest.NewRecorder()
	s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "warming") {
		t.Fatalf("warming readyz = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"prewarm_pending":3`) {
		t.Fatalf("prewarm count missing: %s", rec.Body.String())
	}
	// prewarmTimedOut: 0 timeout never gates.
	if s.prewarmTimedOut() {
		t.Fatal("zero timeout must not time out")
	}
}

func TestSDKReposJS(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/repos.js", nil)
	rec := httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
		t.Fatalf("repos.js = %d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	etag := rec.Header().Get("ETag")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/javascript") {
		t.Fatalf("repos.js content-type = %q (module MIME is load-bearing)", ct)
	}
	if !strings.Contains(rec.Body.String(), "ReposClient") {
		t.Fatalf("repos.js must be the real SDK bundle (D-WEB-2), got %q", rec.Body.String()[:120])
	}
	// Conditional GET → 304.
	req = httptest.NewRequest("GET", "/repos.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("304 = %d", rec.Code)
	}
	// HEAD: headers only.
	req = httptest.NewRequest("HEAD", "/repos.js", nil)
	rec = httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head = %d len=%d", rec.Code, rec.Body.Len())
	}
}

// TestUIAssetResolvers: the embedded page assets must actually be reachable —
// the /repos.js SDK bundle, the stylesheet, the island bundle, the vendored
// htmx files and the concept GIFs (regression: uiAsset/webAsset were hardcoded
// stubs, blanking every page). There is no SPA shell any more (D-WEB-14).
func TestUIAssetResolvers(t *testing.T) {
	for _, name := range []string{"app.css", "ui.js", "htmx.min.js"} {
		if b, ok := uiAsset(name); !ok || len(b) == 0 {
			t.Fatalf("uiAsset(%q) must resolve the built asset (run make web)", name)
		}
	}
	if b, ok := webAsset("dist/repos.js"); !ok || !strings.Contains(string(b), "ReposClient") {
		t.Fatal("webAsset must resolve the SDK bundle (run make web)")
	}
	if b, ok := uiAsset("concepts/push-still.gif"); !ok || len(b) == 0 {
		t.Fatal("uiAsset must resolve the concept GIFs")
	}
	// The SPA shell and its hashed bundle lane are gone.
	if _, ok := uiAsset("index.html"); ok {
		t.Fatal("uiAsset must not serve an SPA shell")
	}
	if _, ok := uiAsset("../go.mod"); ok {
		t.Fatal("uiAsset must refuse traversal-style names")
	}
	if _, ok := uiAsset("etc/passwd"); ok {
		t.Fatal("uiAsset must refuse paths outside the UI tree")
	}
	if _, ok := uiAsset("src/main.js"); ok {
		t.Fatal("raw source lanes must 404 after the vite cutover (D-WEB-6)")
	}
	if _, ok := uiAsset("assets/../../go.mod"); ok {
		t.Fatal("traversal via assets/ prefix must fail")
	}
}

func TestEventsNotify(t *testing.T) {
	var woken string
	s, _ := newTestServer(t, func(o *Options) {
		o.Notifier = func(repo string) { woken = repo }
	})
	rec := httptest.NewRecorder()
	s.eventsNotify(rec, httptest.NewRequest("POST", "/_events/notify?repo=o/r", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anon notify = %d", rec.Code)
	}
	// Authorized → 202 + wake.
	req := httptest.NewRequest("POST", "/_events/notify?repo=o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.eventsNotify(rec, req)
	if rec.Code != http.StatusAccepted || woken != "o/r" {
		t.Fatalf("notify = %d wake=%q", rec.Code, woken)
	}
	// Without notifier wired → still 202.
	s2, _ := newTestServer(t, nil)
	req = httptest.NewRequest("POST", "/_events/notify", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s2.eventsNotify(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notify without bridge = %d", rec.Code)
	}
}

// TestPageRoutesGoToProvider: the browser page tree is answered by the page
// provider (internal/pages seam) — /, /explore, /how-it-works, /{owner},
// /{owner}/{repo}, the /{owner}/{repo}/<page> shapes, /{owner}/teams/{slug}
// and /setup. The server owns the shape (method gates, reservations) and the
// provider owns the rendering and gating.
func TestPageRoutesGoToProvider(t *testing.T) {
	ui := &fakeUI{paths: map[string]bool{
		"/":                true,
		"/explore":         true,
		"/how-it-works":    true,
		"/setup":           true,
		"/alice":           true,
		"/o/r":             true,
		"/o/r/tree/main":   true,
		"/acme/teams/core": true,
		"/api":             true,
	}}
	s, h := newTestServer(t, func(o *Options) { o.API = &fakeAPI{} })
	s.SetUI(ui)
	do := func(method, url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, url := range []string{
		"http://x/", "http://x/explore", "http://x/explore?format=text",
		"http://x/how-it-works", "http://x/setup", "http://x/alice",
		"http://x/o/r", "http://x/o/r/tree/main", "http://x/acme/teams/core",
	} {
		rec := do("GET", url)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="page"`) {
			t.Fatalf("GET %s must be answered by the page provider: %d %.120s", url, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s content-type = %q", url, ct)
		}
	}
	// /api is the page provider first; the JSON lane answers when it declines.
	if rec := do("GET", "http://x/api"); rec.Code != http.StatusOK {
		t.Fatalf("api docs page = %d", rec.Code)
	}
	plain, ph := newTestServer(t, func(o *Options) { o.API = &fakeAPI{} })
	plain.SetUI(&fakeUI{paths: map[string]bool{"/": true}}) // declines /api
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/api", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("declined /api must reach the JSON lane, got %d", rec.Code)
	}
	// Page shapes are GET/HEAD only.
	for _, url := range []string{"http://x/alice", "http://x/o/r/tree/main"} {
		if rec := do("POST", url); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", url, rec.Code)
		}
	}
	// A path the provider does not own is the plain 404 — never a shell.
	if rec := do("GET", "http://x/acme/nope/deeper"); rec.Code != http.StatusNotFound {
		t.Fatalf("unowned page path = %d, want 404", rec.Code)
	}
	if !strings.Contains(strings.Join(ui.seen, " "), "/acme/nope/deeper") {
		t.Fatalf("provider must be consulted for page shapes, saw %v", ui.seen)
	}
}

// TestPageRoutes404WithoutProvider: with no page provider mounted the page
// routes answer the plain 404. The SPA shell that used to stand in for them is
// deleted (D-WEB-14) — there is no second page tree.
func TestPageRoutes404WithoutProvider(t *testing.T) {
	_, h := newTestServer(t, nil)
	for _, url := range []string{
		"http://x/", "http://x/explore", "http://x/how-it-works", "http://x/setup",
		"http://x/alice", "http://x/o/r", "http://x/o/r/tree",
	} {
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s without a page provider = %d, want 404", url, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="root"`) {
			t.Fatalf("GET %s must not answer with an SPA shell", url)
		}
	}
}

func TestSetupJSONRecipes(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.PublicURL = "https://walgit.example.com"
	// Anonymous → 401.
	rec := httptest.NewRecorder()
	s.setupJSON(rec, httptest.NewRequest("GET", "/services/setup.json", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon setup.json = %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "http://x/services/setup.json?repo=o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup.json = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"manual_clone", "plain_clone", "blobless_clone",
		"bundle_list", "setup_text", "install.sh?repo=o/r", "host"} {
		if !strings.Contains(body, want) {
			t.Fatalf("setup.json missing %q: %s", want, body)
		}
	}
	// oidc mode exposes the token URL; there is no CA URL anymore (#165:
	// TLS terminates at the reverse proxy, never in-process).
	s.cfg.Server.Auth.Mode = "oidc"
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if !strings.Contains(rec.Body.String(), "/_auth/tokens") {
		t.Fatalf("token_url missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatalf("ca_url must be gone: %s", rec.Body.String())
	}
}

func TestHostSlug(t *testing.T) {
	cases := map[string]string{
		"walgit.example.com": "walgit-example-com",
		"LOCALHOST:8080":     "localhost-8080",
		"":                   "",
		"a_b.c":              "a-b-c",
	}
	for in, want := range cases {
		if got := hostSlug(in); got != want {
			t.Fatalf("hostSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeUIAssets(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/_ui/app.js", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", rec.Code)
	}
	// stringsTrimPrefix fold behavior (uiAsset is a stub; exercise the helper).
	if stringsTrimPrefix("X-UI/thing", "x-ui/") != "thing" {
		t.Fatal("stringsTrimPrefix must be case-insensitive")
	}
	if stringsTrimPrefix("other", "x") != "other" {
		t.Fatal("stringsTrimPrefix must keep non-matching input")
	}
	if !hasSuffixFold("App.JS", ".js") || hasSuffixFold("app.js", ".css") {
		t.Fatal("hasSuffixFold truth table broken")
	}
	if _, ok := uiAsset("app.js"); ok {
		t.Fatal("uiAsset is a stub and must miss")
	}
}

// TestUIAssetConcepts: /_ui/concepts/*.gif (issue #187) resolve + serve as
// image/gif with the no-cache (never immutable) class — stable filenames
// whose bytes change on regeneration. Requires the built UI (run make web;
// same precondition as TestUIAssetResolvers).
func TestUIAssetConcepts(t *testing.T) {
	s, _ := newTestServer(t, nil)
	b, ok := uiAsset("concepts/push.gif")
	if !ok || len(b) == 0 || string(b[:6]) != "GIF89a" {
		t.Fatal("concepts/push.gif must resolve to GIF bytes (run make web)")
	}
	if _, ok := uiAsset("concepts/push.png"); ok {
		t.Fatal("non-gif concept names must miss")
	}
	if _, ok := uiAsset("concepts/../../go.mod"); ok {
		t.Fatal("traversal via concepts/ prefix must fail")
	}
	// MIME + cache class + ETag through the handler.
	req := httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("concepts gif = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
		t.Fatalf("concepts content-type = %q, want image/gif", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("concepts cache-control = %q, want no-cache (never immutable)", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("concepts gif must carry an ETag")
	}
	req = httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional concepts gif = %d, want 304", rec.Code)
	}
}

// TestCompressBypassesImages: the compress middleware must not gzip
// image/* bodies (issue #187 B2 — gzipping LZW GIFs is pure CPU per
// request); SSE + precompressed bodies keep their existing bypass.
func TestCompressBypassesImages(t *testing.T) {
	s, _ := newTestServer(t, nil)
	run := func(ct string) *httptest.ResponseRecorder {
		h := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("bytes"))
		}))
		req := httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := run("image/gif"); rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("image/gif must bypass gzip")
	}
	if rec := run("text/event-stream"); rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("SSE must bypass gzip")
	}
	if rec := run("text/html; charset=utf-8"); rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("text/html must still gzip")
	}
}

// TestSetupPageSeam: GET /setup is a page-provider route. The §3.4 access
// rules for the page live in the page layer (internal/webui + the cmd/spindle
// wiring); the server owns only the routing, so a mounted provider answers and
// no provider is the plain 404 (never an SPA shell).
func TestSetupPageSeam(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.SetUI(&fakeUI{paths: map[string]bool{"/setup": true}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/setup", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="page"`) {
		t.Fatalf("setup page = %d, want the page provider", rec.Code)
	}
	// No provider → 404, never a shell.
	_, h2 := newTestServer(t, nil)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/setup", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("setup without a provider = %d, want 404", rec.Code)
	}
	// Setup-only mode keeps the POST fallback (§3.4 503) when the page layer
	// does not answer the submit — never a dead 405.
	s3, h3 := newTestServer(t, func(o *Options) { o.Boot = BootState{Mode: "setup_only"} })
	s3.SetUI(&fakeUI{paths: map[string]bool{}})
	rec = httptest.NewRecorder()
	h3.ServeHTTP(rec, httptest.NewRequest("POST", "http://x/setup", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup-only POST without a page answer = %d, want 503", rec.Code)
	}
}

func TestInstallSh(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.PublicURL = "https://wal.example.com"
	req := httptest.NewRequest("GET", "https://wal.example.com/services/public/install.sh?repo=o/r", nil)
	rec := httptest.NewRecorder()
	s.installSh(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/x-shellscript") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"wal-example-com", "o/r", "#!/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
	// Token mode → the credential helper is baked in.
	if !strings.Contains(body, "credential") && !strings.Contains(body, "helper") {
		t.Logf("helper not referenced in auth-token mode body (len %d)", len(body))
	}
	// No CA trust steps anymore (#165): the proxy terminates TLS.
	rec = httptest.NewRecorder()
	s.installSh(rec, req)
	if strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatal("install.sh must not reference ca.pem")
	}
}
