package staticpages

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- fakes -------------------------------------------------------------------

type fakeDirs struct {
	dir   string
	err   error
	calls *int
}

func (d fakeDirs) Dir(ctx context.Context, repo string) (string, error) {
	if d.calls != nil {
		(*d.calls)++
	}
	if d.err != nil {
		return "", d.err
	}
	return d.dir, nil
}

type fakeSettings struct {
	mu    sync.Mutex
	sites map[string]SiteSettings
	err   error
	calls int
}

func (f *fakeSettings) Site(ctx context.Context, owner, repo string) (SiteSettings, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return SiteSettings{}, f.err
	}
	return f.sites[owner+"/"+repo], nil
}

type fakeVisible struct {
	private map[string]bool
	err     error
	calls   int
}

func (f *fakeVisible) CanRead(ctx context.Context, owner, repo string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return !f.private[owner+"/"+repo], nil
}

type fakeGit struct {
	mu          sync.Mutex
	calls       []string
	commit      string
	entries     map[string]Entry
	blobs       map[string][]byte
	resolveErr  error
	lookupErr   map[string]error
	readErr     error
	lookupCalls int
	blobCalls   int
}

func (g *fakeGit) record(call string) {
	g.mu.Lock()
	g.calls = append(g.calls, call)
	g.mu.Unlock()
}

func (g *fakeGit) reset() {
	g.mu.Lock()
	g.calls = nil
	g.lookupCalls, g.blobCalls = 0, 0
	g.mu.Unlock()
}

func (g *fakeGit) callList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *fakeGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	g.record("resolve:" + ref)
	if g.resolveErr != nil {
		return "", g.resolveErr
	}
	return g.commit, nil
}

func (g *fakeGit) Lookup(ctx context.Context, dir, treeish, path string) (Entry, bool, error) {
	g.record("lookup:" + path)
	g.mu.Lock()
	g.lookupCalls++
	g.mu.Unlock()
	if err := g.lookupErr[path]; err != nil {
		return Entry{}, false, err
	}
	e, ok := g.entries[path]
	return e, ok, nil
}

func (g *fakeGit) ReadBlob(ctx context.Context, dir, oid string, w io.Writer) error {
	g.record("blob:" + oid)
	g.mu.Lock()
	g.blobCalls++
	g.mu.Unlock()
	if g.readErr != nil {
		return g.readErr
	}
	body, ok := g.blobs[oid]
	if !ok {
		return ErrNotFound
	}
	_, err := w.Write(body)
	return err
}

// --- fixture -----------------------------------------------------------------

const (
	testHost  = "static.localhost"
	testLimit = 64
)

var (
	siteIndex = "<h1>site</h1>"
	site404   = "<h1>gone</h1>"
)

func oid(seed string) string {
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])
}

type fixture struct {
	h         *Handler
	git       *fakeGit
	settings  *fakeSettings
	visible   *fakeVisible
	dirsCalls int
	logs      *bytes.Buffer // captured infrastructure-failure log (§7)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	g := &fakeGit{
		commit:    oid("commit"),
		entries:   map[string]Entry{},
		blobs:     map[string][]byte{},
		lookupErr: map[string]error{},
	}
	blob := func(name, body string) {
		g.entries[name] = Entry{Mode: "100644", Type: "blob", OID: oid(name), Size: int64(len(body))}
		g.blobs[oid(name)] = []byte(body)
	}
	tree := func(name string) {
		g.entries[name] = Entry{Mode: "040000", Type: "tree", OID: oid(name + "/tree"), Size: -1}
	}
	link := func(name, target string) {
		g.entries[name] = Entry{Mode: "120000", Type: "blob", OID: oid(name), Size: int64(len(target))}
		g.blobs[oid(name)] = []byte(target)
	}
	gitlink := func(name string) {
		g.entries[name] = Entry{Mode: "160000", Type: "commit", OID: oid(name), Size: -1}
	}

	blob("index.html", siteIndex)
	blob("index.htm", "<h1>htm index</h1>")
	blob("404.html", site404)
	blob("about.html", "<p>about</p>")
	blob("choice", "raw")
	blob("choice.html", "<p>choice html</p>")
	blob("file.txt", "textfile")
	blob("with space.txt", "spaced")
	blob("both.txt", "text")
	blob("textfile", "plain")

	tree("docs")
	blob("docs/index.html", "<h1>docs</h1>")
	blob("docs/page.html", "<p>page</p>")

	tree("assets")
	blob("assets/app.css", "body{margin:0}")
	blob("assets/app.js", "console.log(1)")

	tree("nolisting")
	tree("symdir")
	link("symdir/index.html", "index.htm")
	blob("symdir/index.htm", "<h1>sym fallback</h1>")

	link("link", "index.html")
	gitlink("sub")

	g.entries["big.bin"] = Entry{Mode: "100644", Type: "blob", OID: oid("big.bin"), Size: testLimit + 1}
	g.blobs[oid("big.bin")] = []byte(strings.Repeat("x", testLimit+1))

	// liar.txt claims 4 bytes but writes far more: the capped buffer must stop it.
	g.entries["liar.txt"] = Entry{Mode: "100644", Type: "blob", OID: oid("liar.txt"), Size: 4}
	g.blobs[oid("liar.txt")] = []byte(strings.Repeat("y", 500))

	g.entries["weird.bin"] = Entry{Mode: "100644", Type: "blob", OID: oid("weird.bin"), Size: -1}

	// Rendered-through-root site: only what lives under public/ exists.
	blob("public/index.html", "<h1>public</h1>")
	blob("public/404.html", "<h1>public gone</h1>")

	// An index-order fixture: only index.htm exists under order/.
	blob("order/index.htm", "<h1>only htm</h1>")

	// A site whose own 404.html is over the cap: the response degrades to the
	// plain-text 404 instead of a 503 or a truncated page.
	g.entries["huge404/404.html"] = Entry{Mode: "100644", Type: "blob", OID: oid("huge404/404.html"), Size: testLimit + 1}
	g.blobs[oid("huge404/404.html")] = []byte(strings.Repeat("z", testLimit+1))

	settings := &fakeSettings{sites: map[string]SiteSettings{
		"acme/site":    {Enabled: true},
		"acme/docsref": {Enabled: true, Ref: "refs/heads/site"},
		"acme/rooted":  {Enabled: true, Root: "public"},
		"acme/order":   {Enabled: true, Root: "order"},
		"acme/plain":   {Enabled: true, Root: "plainroot"},
		"acme/huge404": {Enabled: true, Root: "huge404"},
		"acme/off":     {Enabled: false},
		"acme/badroot": {Enabled: true, Root: "public/../etc"},
		"acme/badref":  {Enabled: true, Ref: "refs/heads/../nope"},
	}}
	visible := &fakeVisible{private: map[string]bool{"acme/private": true}}

	f := &fixture{git: g, settings: settings, visible: visible, logs: &bytes.Buffer{}}
	f.h = New(Options{
		Host:         testHost,
		MaxFileBytes: testLimit,
		Dirs:         fakeDirs{dir: "/serving/acme/site.git", calls: &f.dirsCalls},
		Git:          g,
		Settings:     settings,
		Visible:      visible,
		Logger:       slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	return f
}

// do drives the handler the way the listener will, with an explicit Host.
func (f *fixture) do(t *testing.T, method, target, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if host == "" {
		host = testHost
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) gitCalls() []string { return f.git.callList() }

// --- the resolution table (§3) ----------------------------------------------

func TestServeHTTPResolutionTable(t *testing.T) {
	f := newFixture(t)
	indexETag := `"` + oid("index.html") + `"`

	tests := []struct {
		name        string
		method      string
		target      string
		host        string
		headers     map[string]string
		status      int
		body        string
		noBody      bool
		contentType string
		location    string
		etag        string
		noCache     bool
		allow       string
		rangeHeader string
	}{
		{
			name: "repo root without slash redirects", target: "/acme/site",
			status: http.StatusMovedPermanently, location: "/acme/site/", noCache: true, noBody: true,
		},
		{
			name: "repo root redirect preserves the query", target: "/acme/site?ref=1&x=%20",
			status: http.StatusMovedPermanently, location: "/acme/site/?ref=1&x=%20", noCache: true,
		},
		{
			name: "repo root serves index", target: "/acme/site/",
			status: http.StatusOK, body: siteIndex, contentType: "text/html; charset=utf-8",
			etag: indexETag,
		},
		{
			name: "nested directory serves its index", target: "/acme/site/docs/",
			status: http.StatusOK, body: "<h1>docs</h1>", contentType: "text/html; charset=utf-8",
			etag: `"` + oid("docs/index.html") + `"`,
		},
		{
			name: "directory without a slash redirects", target: "/acme/site/docs?q=1",
			status: http.StatusMovedPermanently, location: "/acme/site/docs/?q=1", noCache: true,
		},
		{
			name: "blob is served with its pinned type", target: "/acme/site/assets/app.css",
			status: http.StatusOK, body: "body{margin:0}", contentType: "text/css; charset=utf-8",
			etag: `"` + oid("assets/app.css") + `"`,
		},
		{
			name: "html fallback serves x.html with 200", target: "/acme/site/about",
			status: http.StatusOK, body: "<p>about</p>", contentType: "text/html; charset=utf-8",
			etag: `"` + oid("about.html") + `"`,
		},
		{
			name: "an existing blob wins over the html fallback", target: "/acme/site/choice",
			status: http.StatusOK, body: "raw", contentType: "application/octet-stream",
		},
		{
			name: "directory without index is 404 (never a listing)", target: "/acme/site/nolisting/",
			status: http.StatusNotFound, body: site404, contentType: "text/html; charset=utf-8", noCache: true,
		},
		{
			name: "trailing slash on a file is 404", target: "/acme/site/file.txt/",
			status: http.StatusNotFound, body: site404, contentType: "text/html; charset=utf-8", noCache: true,
		},
		{
			name: "missing path serves the site 404.html", target: "/acme/site/nope",
			status: http.StatusNotFound, body: site404, contentType: "text/html; charset=utf-8", noCache: true,
		},
		{
			name: "missing path without a 404.html is plain text", target: "/acme/plain/nope",
			status: http.StatusNotFound, body: "not found", contentType: "text/plain; charset=utf-8", noCache: true,
		},
		{
			name: "matching if-none-match is 304", target: "/acme/site/",
			headers: map[string]string{"If-None-Match": indexETag},
			status:  http.StatusNotModified, noBody: true, etag: indexETag,
		},
		{
			name: "weak if-none-match is 304", target: "/acme/site/",
			headers: map[string]string{"If-None-Match": `W/` + indexETag},
			status:  http.StatusNotModified, noBody: true, etag: indexETag,
		},
		{
			name: "if-none-match star is 304", target: "/acme/site/",
			headers: map[string]string{"If-None-Match": "*"},
			status:  http.StatusNotModified, noBody: true, etag: indexETag,
		},
		{
			name: "non-matching if-none-match is 200", target: "/acme/site/",
			headers: map[string]string{"If-None-Match": `"other", W/"third"`},
			status:  http.StatusOK, body: siteIndex, contentType: "text/html; charset=utf-8", etag: indexETag,
		},
		{
			name: "range is a 206", target: "/acme/site/file.txt",
			headers: map[string]string{"Range": "bytes=0-3"},
			status:  http.StatusPartialContent, body: "text", contentType: "text/plain; charset=utf-8",
			rangeHeader: "bytes 0-3/8",
		},
		{
			name: "range past eof is a 416", target: "/acme/site/file.txt",
			headers:     map[string]string{"Range": "bytes=99-"},
			status:      http.StatusRequestedRangeNotSatisfiable,
			rangeHeader: "bytes */8",
		},
		{
			name: "head mirrors get without a body", method: http.MethodHead, target: "/acme/site/",
			status: http.StatusOK, noBody: true, contentType: "text/html; charset=utf-8", etag: indexETag,
		},
		{
			name: "post is 405 with allow", method: http.MethodPost, target: "/acme/site/",
			status: http.StatusMethodNotAllowed, body: "method not allowed",
			contentType: "text/plain; charset=utf-8", allow: "GET, HEAD", noCache: true,
		},
		{
			name: "put is 405", method: http.MethodPut, target: "/acme/site/index.html",
			status: http.StatusMethodNotAllowed, allow: "GET, HEAD",
		},
		{
			name: "host mismatch is an empty 404", target: "/acme/site/", host: "evil.localhost",
			status: http.StatusNotFound, noBody: true, noCache: true,
		},
		{
			name: "host comparison ignores the port", target: "/acme/site/", host: "static.localhost:8081",
			status: http.StatusOK, body: siteIndex,
		},
		{
			name: "host comparison is case-insensitive", target: "/acme/site/", host: "STATIC.LocalHost",
			status: http.StatusOK, body: siteIndex,
		},
		{
			name: "a trailing-dot host is a different host and is refused", target: "/acme/site/",
			host: "static.localhost.", status: http.StatusNotFound, noBody: true, noCache: true,
		},
		{
			name: "private repository is a 404", target: "/acme/private/",
			status: http.StatusNotFound, body: "not found", contentType: "text/plain; charset=utf-8",
		},
		{
			name: "disabled site is a 404", target: "/acme/off/",
			status: http.StatusNotFound, body: "not found",
		},
		{
			name: "unknown site is a 404", target: "/acme/absent/",
			status: http.StatusNotFound, body: "not found",
		},
		{
			name: "bad configured root is a 404", target: "/acme/badroot/",
			status: http.StatusNotFound, body: "not found",
		},
		{
			name: "bad configured ref is a 404", target: "/acme/badref/",
			status: http.StatusNotFound, body: "not found",
		},
		{
			name: "symlink is never served", target: "/acme/site/link",
			status: http.StatusNotFound, body: site404,
		},
		{
			name: "gitlink is never served", target: "/acme/site/sub",
			status: http.StatusNotFound, body: site404,
		},
		{
			name: "oversized file is a 404 without a content read", target: "/acme/site/big.bin",
			status: http.StatusNotFound, body: site404,
		},
		{
			name: "a wrong entry size cannot overrun the cap", target: "/acme/site/liar.txt",
			status: http.StatusNotFound, body: site404,
		},
		{
			name: "index.htm is used when index.html is absent", target: "/acme/order/",
			status: http.StatusOK, body: "<h1>only htm</h1>", contentType: "text/html; charset=utf-8",
		},
		{
			name: "an unservable index falls through to the next name", target: "/acme/site/symdir/",
			status: http.StatusOK, body: "<h1>sym fallback</h1>",
		},
		{
			name: "site root from settings", target: "/acme/rooted/",
			status: http.StatusOK, body: "<h1>public</h1>", contentType: "text/html; charset=utf-8",
		},
		{
			name: "site root 404 page from settings", target: "/acme/rooted/missing",
			status: http.StatusNotFound, body: "<h1>public gone</h1>",
		},
		{
			name: "duplicate slashes collapse", target: "/acme/site//docs//",
			status: http.StatusOK, body: "<h1>docs</h1>",
		},
		{
			name: "dot segments are dropped", target: "/acme/site/./docs/./index.html",
			status: http.StatusOK, body: "<h1>docs</h1>",
		},
		{
			name: "percent-encoded names decode", target: "/acme/site/with%20space.txt",
			status: http.StatusOK, body: "spaced", contentType: "text/plain; charset=utf-8",
		},
		{
			name: "paths are case-sensitive", target: "/acme/site/INDEX.HTML",
			status: http.StatusNotFound, body: site404,
		},
		{
			name: "head of a rejected path has no body", method: http.MethodHead, target: "/acme/site/.env",
			status: http.StatusNotFound, noBody: true,
		},
		{
			name: "a path with no repository is a 404", target: "/acme",
			status: http.StatusNotFound, body: "not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.git.reset()
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			rec := f.do(t, method, tc.target, tc.host, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.body)
			}
			if tc.noBody && rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); tc.contentType != "" && ct != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.contentType)
			}
			if loc := rec.Header().Get("Location"); tc.location != "" || loc != "" {
				if loc != tc.location {
					t.Errorf("Location = %q, want %q", loc, tc.location)
				}
			}
			if tc.etag != "" && rec.Header().Get("ETag") != tc.etag {
				t.Errorf("ETag = %q, want %q", rec.Header().Get("ETag"), tc.etag)
			}
			if tc.allow != "" && rec.Header().Get("Allow") != tc.allow {
				t.Errorf("Allow = %q, want %q", rec.Header().Get("Allow"), tc.allow)
			}
			if tc.rangeHeader != "" && rec.Header().Get("Content-Range") != tc.rangeHeader {
				t.Errorf("Content-Range = %q, want %q", rec.Header().Get("Content-Range"), tc.rangeHeader)
			}
			if tc.noCache && rec.Header().Get("Cache-Control") != "" {
				t.Errorf("Cache-Control = %q, want none", rec.Header().Get("Cache-Control"))
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("missing nosniff on %d", rec.Code)
			}
		})
	}
}

// TestContentsOfServedFile pins the exact bytes and headers of one 200 so a
// regression in the ServeContent wiring is visible in one place.
func TestContentsOfServedFile(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/acme/site/assets/app.js", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != CacheControl {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q", got)
	}
	if got := rec.Header().Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified = %q, want none", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "14" {
		t.Errorf("Content-Length = %q", got)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("body = %q", rec.Body.String())
	}
	// resolve, lookup, blob — the documented happy-path budget (§7).
	if got := f.gitCalls(); len(got) != 3 || got[0] != "resolve:" || !strings.HasPrefix(got[1], "lookup:assets/app.js") {
		t.Errorf("git calls = %v", got)
	}
}

// Test304CostsNoContentRead pins the subprocess budget for a revalidation.
func Test304CostsNoContentRead(t *testing.T) {
	f := newFixture(t)
	f.git.reset()
	rec := f.do(t, http.MethodGet, "/acme/site/", "", map[string]string{
		"If-None-Match": `"` + oid("index.html") + `"`,
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d", rec.Code)
	}
	f.git.mu.Lock()
	blobs := f.git.blobCalls
	f.git.mu.Unlock()
	if blobs != 0 {
		t.Errorf("blob reads on 304 = %d, want 0", blobs)
	}
	if got := f.gitCalls(); len(got) != 2 {
		t.Errorf("git calls = %v, want resolve+lookup", got)
	}
}

// TestOversizedFileCostsNoContentRead is the v1 rule: the size check happens
// before any cat-file for the requested file (the 404 page may still be read).
func TestOversizedFileCostsNoContentRead(t *testing.T) {
	f := newFixture(t)
	f.git.reset()
	rec := f.do(t, http.MethodGet, "/acme/site/big.bin", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	for _, call := range f.gitCalls() {
		if call == "blob:"+oid("big.bin") {
			t.Fatalf("oversized file was read: %v", f.gitCalls())
		}
	}
}

// TestBareRepoRedirectDoesNoRepoWork pins the redirect's cost: a directory URL
// without its trailing slash is answered from the visibility gate and the
// per-repo settings alone. No serving-copy sync (RepoDirs.Dir) and no git
// subprocess — even when the copy is cold, which is the point of the reorder.
func TestBareRepoRedirectDoesNoRepoWork(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			// A cold copy: consulting Dir would fail with 503.
			f.h.dirs = fakeDirs{err: errors.New("cold serving copy"), calls: &f.dirsCalls}
			rec := f.do(t, method, "/acme/site?ref=main", "", nil)
			if rec.Code != http.StatusMovedPermanently {
				t.Fatalf("status = %d, want 301 (body %q)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != "/acme/site/?ref=main" {
				t.Errorf("Location = %q, want /acme/site/?ref=main", got)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "" {
				t.Errorf("Cache-Control = %q, want none", cc)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
			if f.dirsCalls != 0 {
				t.Errorf("RepoDirs.Dir calls = %d, want 0", f.dirsCalls)
			}
			if calls := f.gitCalls(); len(calls) != 0 {
				t.Errorf("git calls = %v, want none", calls)
			}
			if f.visible.calls != 1 || f.settings.calls != 1 {
				t.Errorf("gates: visible=%d settings=%d, want 1 each", f.visible.calls, f.settings.calls)
			}
		})
	}
}

// TestUnpublishedSiteDoesNoRepoWork keeps every 404 in front of the sync and
// the subprocesses.
func TestUnpublishedSiteDoesNoRepoWork(t *testing.T) {
	for _, target := range []string{"/acme/absent/", "/acme/off/", "/acme/badroot/", "/acme/badref/"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t)
			rec := f.do(t, http.MethodGet, target, "", nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if f.dirsCalls != 0 {
				t.Errorf("RepoDirs.Dir calls = %d, want 0", f.dirsCalls)
			}
			if calls := f.gitCalls(); len(calls) != 0 {
				t.Errorf("git calls = %v, want none", calls)
			}
		})
	}
}

// --- host and method gates ---------------------------------------------------

// TestServeHTTPIPv6LiteralHost pins the bracketed-literal comparison: the Host
// header "[::1]:port" matches a configured "[::1]", while a different spelling
// of the same address is a different string and is refused (documented in
// normalizeHost, §2).
func TestServeHTTPIPv6LiteralHost(t *testing.T) {
	f := newFixture(t)
	f.h = New(Options{
		Host: "[::1]", MaxFileBytes: testLimit, Dirs: fakeDirs{dir: "/x"},
		Git: f.git, Settings: f.settings, Visible: f.visible,
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	rec := f.do(t, http.MethodGet, "/acme/site/", "[::1]:8099", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != siteIndex {
		t.Fatalf("[::1]:8099 → status %d body %q, want 200", rec.Code, rec.Body.String())
	}
	for _, host := range []string{"[0:0:0:0:0:0:0:1]", "[0:0:0:0:0:0:0:1]:8099"} {
		rec = f.do(t, http.MethodGet, "/acme/site/", host, nil)
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s → status %d body %q, want empty 404", host, rec.Code, rec.Body.String())
		}
	}
}

// --- the isolation claim (§2, §4) --------------------------------------------

// TestStaticResponsesCarryNoAppHeaders is the regression test for the security
// decision in §2: user bytes are served on an origin that shares no cookie jar
// with the app, so the listener must never emit a cookie, a CORS header, framing
// or an app CSP — on any status, including the ones ServeContent writes.
func TestStaticResponsesCarryNoAppHeaders(t *testing.T) {
	f := newFixture(t)
	indexETag := `"` + oid("index.html") + `"`
	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		status  int
	}{
		{name: "200", method: http.MethodGet, target: "/acme/site/", status: http.StatusOK},
		{name: "304", method: http.MethodGet, target: "/acme/site/",
			headers: map[string]string{"If-None-Match": indexETag}, status: http.StatusNotModified},
		{name: "404", method: http.MethodGet, target: "/acme/site/nope", status: http.StatusNotFound},
		{name: "301", method: http.MethodGet, target: "/acme/site", status: http.StatusMovedPermanently},
	}
	forbidden := []string{"set-cookie", "access-control-", "x-frame-options", "content-security-policy"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, tc.method, tc.target, "", tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			for _, name := range forbidden {
				for key := range rec.Header() {
					if strings.HasPrefix(strings.ToLower(key), name) {
						t.Errorf("%s carried %s: %q", tc.name, key, rec.Header().Values(key))
					}
				}
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("nosniff = %q, want nosniff", got)
			}
		})
	}
}

// TestHeadIfNoneMatchIs304WithoutBody covers the HEAD revalidation shape: 304,
// the ETag, no body, no Content-Length, and no content read.
func TestHeadIfNoneMatchIs304WithoutBody(t *testing.T) {
	f := newFixture(t)
	f.git.reset()
	etag := `"` + oid("file.txt") + `"`
	rec := f.do(t, http.MethodHead, "/acme/site/file.txt", "", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q, want none on a 304", cl)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("ETag = %q, want %q", got, etag)
	}
	f.git.mu.Lock()
	blobs := f.git.blobCalls
	f.git.mu.Unlock()
	if blobs != 0 {
		t.Errorf("blob reads on a HEAD 304 = %d, want 0", blobs)
	}
}

// TestOversizedNotFoundPageFallsBackToPlainText covers the second degradation
// case: a 404.html that exists but exceeds the cap is not servable, so the
// response is the one-line plain-text 404 (a truncated page or a 503 would both
// be wrong).
func TestOversizedNotFoundPageFallsBackToPlainText(t *testing.T) {
	f := newFixture(t)
	f.git.reset()
	rec := f.do(t, http.MethodGet, "/acme/huge404/nope", "", nil)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "not found" {
		t.Fatalf("status %d body %q, want plain-text 404", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, call := range f.gitCalls() {
		if call == "blob:"+oid("huge404/404.html") {
			t.Fatalf("oversized 404 page was read: %v", f.gitCalls())
		}
	}
}

// --- infrastructure logging (§7, m1) -----------------------------------------

// TestInfrastructureFailuresLogOnce pins the one log record per failed request:
// repo, operation and the error the runner carried (argv + stderr), only on the
// 503s. Visitor-facing 4xx responses and the happy path stay silent.
func TestInfrastructureFailuresLogOnce(t *testing.T) {
	cases := []struct {
		name   string
		config func(*fixture)
		target string
		op     string
		path   string
		errMsg string
	}{
		{
			name:   "dir failure",
			config: func(f *fixture) { f.h.dirs = fakeDirs{err: errors.New("sync boom")} },
			target: "/acme/site/", op: "dir", errMsg: "sync boom",
		},
		{
			name:   "resolve failure",
			config: func(f *fixture) { f.git.resolveErr = errors.New("resolve boom") },
			target: "/acme/site/", op: "resolve", errMsg: "resolve boom",
		},
		{
			name:   "lookup failure",
			config: func(f *fixture) { f.git.lookupErr["index.html"] = errors.New("lookup boom") },
			target: "/acme/site/", op: "lookup", path: "index.html", errMsg: "lookup boom",
		},
		{
			name:   "blob read failure",
			config: func(f *fixture) { f.git.readErr = errors.New("read boom") },
			target: "/acme/site/", op: "read", path: "index.html", errMsg: "read boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.config(f)
			rec := f.do(t, http.MethodGet, tc.target, "", nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			logs := f.logs.String()
			if n := strings.Count(logs, "static site request failed"); n != 1 {
				t.Fatalf("log records = %d, want exactly 1:\n%s", n, logs)
			}
			for _, want := range []string{
				"level=ERROR", "repo=acme/site", "operation=" + tc.op, tc.errMsg,
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("log does not contain %q:\n%s", want, logs)
				}
			}
			if tc.path != "" && !strings.Contains(logs, "path="+tc.path) {
				t.Errorf("log does not name the path %q:\n%s", tc.path, logs)
			}
		})
	}

	t.Run("404 and 200 stay silent", func(t *testing.T) {
		f := newFixture(t)
		if rec := f.do(t, http.MethodGet, "/acme/site/", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		f.git.resolveErr = fmt.Errorf("%w: gone", ErrNotFound)
		if rec := f.do(t, http.MethodGet, "/acme/site/", "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("status %d", rec.Code)
		}
		if logs := f.logs.String(); logs != "" {
			t.Fatalf("a 200 or a 404 logged:\n%s", logs)
		}
	})

	t.Run("nil logger is safe", func(t *testing.T) {
		f := newFixture(t)
		f.h.logger = nil // a partially built Handler must not panic on the 503 path
		f.h.dirs = fakeDirs{err: errors.New("sync boom")}
		if rec := f.do(t, http.MethodGet, "/acme/site/", "", nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", rec.Code)
		}
		if logs := f.logs.String(); logs != "" {
			t.Fatalf("a nil logger wrote: %s", logs)
		}
	})
}

func TestServeHTTPEmptyHostRefusesEverything(t *testing.T) {
	f := newFixture(t)
	f.h = New(Options{Host: "", Dirs: fakeDirs{dir: "/x"}, Git: f.git, Settings: f.settings, Visible: f.visible})
	rec := f.do(t, http.MethodGet, "/acme/site/", "static.localhost", nil)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q, want empty 404", rec.Code, rec.Body.String())
	}
	f.git.reset()
	rec = f.do(t, http.MethodGet, "/acme/site/", "", nil)
	if rec.Code != http.StatusNotFound || len(f.gitCalls()) != 0 {
		t.Fatalf("status %d calls %v", rec.Code, f.gitCalls())
	}
}

// TestServeHTTPHostGateRunsBeforeMethod pins the documented order (§2, §7): a
// request under a Host the listener does not serve gets the bodiless 404 for
// every method, including the ones it would answer 405 for under its own host.
// The Allow header is not revealed on the foreign host either.
func TestServeHTTPHostGateRunsBeforeMethod(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{http.MethodDelete, http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			rec := f.do(t, method, "/acme/site/", "wrong.host", nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != "" {
				t.Errorf("Allow = %q, want none on a foreign host", allow)
			}
			// Under the served host the same method is the documented 405.
			rec = f.do(t, method, "/acme/site/", "", nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status %d under the served host, want 405", rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("Allow = %q, want GET, HEAD", allow)
			}
		})
	}
}

// --- rejected paths never reach git -----------------------------------------

func TestRejectedPathsDoNotReachGit(t *testing.T) {
	bad := []string{
		"/acme/site/../etc/passwd",
		"/acme/site/%2e%2e/etc/passwd",
		"/acme/site/..%2fetc/passwd",
		"/acme/site/a/../../b",
		"/acme/site/a%5cb",
		"/acme/site/a%2Fb",
		"/acme/site/%00",
		"/acme/site/a%0ab",
		"/acme/site/%7f",
		"/acme/site/.env",
		"/acme/site/%2Eenv",
		"/acme/site/.git/config",
		"/acme/site/docs/.hidden",
		"/acme/site//../x",
		"/.acme/site/index.html",
		"/acme/site%2Fx/index.html",
		"/acme/site/index.html%00.png",
	}
	f := newFixture(t)
	for _, target := range bad {
		t.Run(target, func(t *testing.T) {
			f.git.reset()
			rec := f.do(t, http.MethodGet, target, "", nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", rec.Code)
			}
			if calls := f.gitCalls(); len(calls) != 0 {
				t.Fatalf("git calls = %v, want none", calls)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "" {
				t.Fatalf("Cache-Control = %q, want none", cc)
			}
		})
	}
}

// TestPrivateRepoDoesNoStoreOrGitWork keeps the visibility gate in front of the
// settings read and every subprocess.
func TestPrivateRepoDoesNoStoreOrGitWork(t *testing.T) {
	f := newFixture(t)
	f.settings.calls = 0
	f.git.reset()
	rec := f.do(t, http.MethodGet, "/acme/private/", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if f.settings.calls != 0 {
		t.Errorf("settings calls = %d, want 0", f.settings.calls)
	}
	if f.dirsCalls != 0 {
		t.Errorf("RepoDirs.Dir calls = %d, want 0", f.dirsCalls)
	}
	if calls := f.gitCalls(); len(calls) != 0 {
		t.Errorf("git calls = %v", calls)
	}
}

// --- configured seams --------------------------------------------------------

func TestConfiguredRefIsPassedToResolve(t *testing.T) {
	f := newFixture(t)
	f.git.reset()
	rec := f.do(t, http.MethodGet, "/acme/docsref/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	calls := f.gitCalls()
	if len(calls) == 0 || calls[0] != "resolve:refs/heads/site" {
		t.Fatalf("git calls = %v", calls)
	}
}

func TestCustomIndexFiles(t *testing.T) {
	f := newFixture(t)
	f.h = New(Options{
		Host: testHost, MaxFileBytes: testLimit, IndexFiles: []string{"index.htm"},
		Dirs: fakeDirs{dir: "/x"}, Git: f.git, Settings: f.settings, Visible: f.visible,
	})
	rec := f.do(t, http.MethodGet, "/acme/site/", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>htm index</h1>" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestInvalidIndexNamesAreFiltered(t *testing.T) {
	f := newFixture(t)
	f.h = New(Options{
		Host: testHost, MaxFileBytes: testLimit,
		IndexFiles: []string{"", ".hidden", "a/b", "..", "index.html"},
		Dirs:       fakeDirs{dir: "/x"}, Git: f.git, Settings: f.settings, Visible: f.visible,
	})
	rec := f.do(t, http.MethodGet, "/acme/site/", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != siteIndex {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestNoUsableIndexNamesUsesDefaults(t *testing.T) {
	h := New(Options{Host: testHost, IndexFiles: []string{"", "/", "x/y", "a\x01b", strings.Repeat("a", 256)}})
	if len(h.indexFiles) != 2 || h.indexFiles[0] != "index.html" {
		t.Fatalf("indexFiles = %v", h.indexFiles)
	}
	if h.maxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("maxFileBytes = %d", h.maxFileBytes)
	}
}

func TestOptionDefaults(t *testing.T) {
	h := New(Options{Host: "Static.LocalHost:9000"})
	if h.host != "static.localhost" {
		t.Errorf("host = %q", h.host)
	}
	if h.git == nil || h.logger == nil {
		t.Errorf("defaults not wired: git=nil %v logger=nil %v", h.git == nil, h.logger == nil)
	}
	if h.logger != slog.Default() {
		t.Error("logger should default to slog.Default()")
	}
	if h.hostOK("STATIC.localhost:1234") != true {
		t.Error("host comparison should ignore case and port")
	}
	if h.hostOK("other") {
		t.Error("host comparison accepted a mismatch")
	}
}

// --- failure mapping ---------------------------------------------------------

func TestFailureMapping(t *testing.T) {
	tests := []struct {
		name   string
		config func(f *fixture)
		target string
		status int
		body   string
	}{
		{
			name:   "visible error is a 404",
			config: func(f *fixture) { f.visible.err = errors.New("store down") },
			target: "/acme/site/", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "settings error is a 404",
			config: func(f *fixture) { f.settings.err = errors.New("store down") },
			target: "/acme/site/", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "unwired seams are a 503",
			config: func(f *fixture) {}, target: "/acme/site/", status: http.StatusServiceUnavailable,
		},
		{
			name:   "dir error is a 503",
			config: func(f *fixture) { f.h.dirs = fakeDirs{err: errors.New("sync failed")} },
			target: "/acme/site/", status: http.StatusServiceUnavailable,
		},
		{
			name:   "missing ref is a 404",
			config: func(f *fixture) { f.git.resolveErr = fmt.Errorf("%w: no commit", ErrNotFound) },
			target: "/acme/site/", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "invalid ref is a 404",
			config: func(f *fixture) { f.git.resolveErr = fmt.Errorf("%w: bad", ErrInvalid) },
			target: "/acme/site/", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "git failure on resolve is a 503",
			config: func(f *fixture) { f.git.resolveErr = errors.New("boom") },
			target: "/acme/site/", status: http.StatusServiceUnavailable,
		},
		{
			name:   "lookup not found is a 404",
			config: func(f *fixture) { f.git.lookupErr["index.html"] = fmt.Errorf("%w: gone", ErrNotFound) },
			target: "/acme/site/", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "lookup failure is a 503",
			config: func(f *fixture) { f.git.lookupErr["index.html"] = errors.New("boom") },
			target: "/acme/site/", status: http.StatusServiceUnavailable,
		},
		{
			name:   "file lookup not found is a 404",
			config: func(f *fixture) { f.git.lookupErr["assets/app.css"] = fmt.Errorf("%w: gone", ErrNotFound) },
			target: "/acme/site/assets/app.css", status: http.StatusNotFound, body: "not found",
		},
		{
			name:   "file lookup failure is a 503",
			config: func(f *fixture) { f.git.lookupErr["assets/app.css"] = errors.New("boom") },
			target: "/acme/site/assets/app.css", status: http.StatusServiceUnavailable,
		},
		{
			name:   "html fallback lookup failure is a 503",
			config: func(f *fixture) { f.git.lookupErr["about.html"] = errors.New("boom") },
			target: "/acme/site/about", status: http.StatusServiceUnavailable,
		},
		{
			name:   "blob failure is a 503",
			config: func(f *fixture) { f.git.readErr = errors.New("boom") },
			target: "/acme/site/", status: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.name == "unwired seams are a 503" {
				f.h = New(Options{Host: testHost, Git: f.git, Settings: f.settings, Visible: f.visible})
			}
			tc.config(f)
			rec := f.do(t, http.MethodGet, tc.target, "", nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.body)
			}
			if rec.Header().Get("Cache-Control") != "" {
				t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

// TestNotFoundPageReadFailureFallsBackToPlainText covers a 404.html that exists
// but cannot be read.
func TestNotFoundPageReadFailureFallsBackToPlainText(t *testing.T) {
	f := newFixture(t)
	// The 404 page exists but its blob cannot be read: the response degrades to
	// the one-line plain-text 404 rather than a 503.
	req := httptest.NewRequest(http.MethodGet, "/acme/site/nope", nil)
	req.Host = testHost
	rec := httptest.NewRecorder()
	h := New(Options{
		Host: testHost, MaxFileBytes: testLimit, Dirs: fakeDirs{dir: "/x"},
		Git:      &readFailOn{"404.html", f.git},
		Settings: f.settings, Visible: f.visible,
	})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "not found" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

// readFailOn fails ReadBlob only for one path's oid, so the 404-page fallback is
// reachable without breaking the first lookup.
type readFailOn struct {
	path string
	base *fakeGit
}

func (r *readFailOn) Resolve(ctx context.Context, dir, ref string) (string, error) {
	return r.base.Resolve(ctx, dir, ref)
}

func (r *readFailOn) Lookup(ctx context.Context, dir, treeish, path string) (Entry, bool, error) {
	return r.base.Lookup(ctx, dir, treeish, path)
}

func (r *readFailOn) ReadBlob(ctx context.Context, dir, object string, w io.Writer) error {
	if object == oid(r.path) {
		return errors.New("unreadable")
	}
	return r.base.ReadBlob(ctx, dir, object, w)
}

func TestZeroHandlerIs404(t *testing.T) {
	// A host matches, but no seam is wired: nothing is published.
	h := &Handler{host: testHost}
	req := httptest.NewRequest(http.MethodGet, "/acme/site/", nil)
	req.Host = testHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestNilGitIs503(t *testing.T) {
	h := &Handler{
		host: testHost, maxFileBytes: testLimit, indexFiles: DefaultIndexFiles(),
		dirs:     fakeDirs{dir: "/x"},
		settings: &fakeSettings{sites: map[string]SiteSettings{"acme/site": {Enabled: true}}},
		visible:  &fakeVisible{},
	}
	req := httptest.NewRequest(http.MethodGet, "/acme/site/", nil)
	req.Host = testHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

// --- direct helper tests -----------------------------------------------------

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"static.localhost":     "static.localhost",
		"Static.LocalHost:80":  "static.localhost",
		"static.localhost:443": "static.localhost",
		"  spaced.example  ":   "spaced.example",
		"[::1]:8080":           "::1",
		"[::1]":                "::1",
		"[::1":                 "[::1",
		"::1":                  "::1",
		"":                     "",
	}
	for in, want := range tests {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTargetNormalisation(t *testing.T) {
	tests := []struct {
		path     string
		ok       bool
		owner    string
		repo     string
		rest     string
		trailing bool
	}{
		{path: "/acme/site", ok: true, owner: "acme", repo: "site"},
		{path: "/acme/site/", ok: true, owner: "acme", repo: "site", trailing: true},
		{path: "/acme/site/docs", ok: true, owner: "acme", repo: "site", rest: "docs"},
		{path: "/acme/site/docs/", ok: true, owner: "acme", repo: "site", rest: "docs", trailing: true},
		{path: "/acme/site//docs//", ok: true, owner: "acme", repo: "site", rest: "docs", trailing: true},
		{path: "/acme/site/./a/./b", ok: true, owner: "acme", repo: "site", rest: "a/b"},
		{path: "/acme/site/a%20b", ok: true, owner: "acme", repo: "site", rest: "a b"},
		{path: "/acme/site/%2e", ok: true, owner: "acme", repo: "site"},
		{path: "/acme/site/a/../b", ok: false},
		{path: "/acme/site/..", ok: false},
		{path: "/acme/site/%2e%2e", ok: false},
		{path: "/acme/site/a%2fb", ok: false},
		{path: "/acme/site/a\\b", ok: false},
		{path: "/acme/site/%00", ok: false},
		{path: "/acme/site/%09", ok: false},
		{path: "/acme/site/%7f", ok: false},
		{path: "/acme/site/%zz", ok: false},
		{path: "/acme/site/.hidden", ok: false},
		{path: "/acme/site/a/.hidden", ok: false},
		{path: "/.acme/site/", ok: false},
		{path: "/acme/.site/", ok: false},
		{path: "/acme", ok: false},
		{path: "/", ok: false},
		{path: "", ok: false},
		{path: "acme/site/", ok: false},
		{path: "/" + strings.Repeat("a", 101) + "/site/", ok: false},
		{path: "/acme/sit e/", ok: false},
		{path: "/ac me/site/", ok: false},
	}
	for _, tc := range tests {
		tgt, err := parseTarget(tc.path)
		if tc.ok != (err == nil) {
			t.Errorf("parseTarget(%q) error = %v, want ok=%v", tc.path, err, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		if tgt.owner != tc.owner || tgt.repo != tc.repo || tgt.rest != tc.rest || tgt.trailing != tc.trailing {
			t.Errorf("parseTarget(%q) = %+v, want %+v", tc.path, tgt, tc)
		}
	}
}

func TestCleanRoot(t *testing.T) {
	good := []string{"", "public", "docs/site", "a-b_c.d"}
	for _, in := range good {
		if got, ok := cleanRoot(in); !ok || got != in {
			t.Errorf("cleanRoot(%q) = %q, %v", in, got, ok)
		}
	}
	bad := []string{"/public", "public/", "../x", "a/../b", "a//b", "a/.b", ".git", "a\\b", "a\x00b", strings.Repeat("a", 600)}
	for _, in := range bad {
		if _, ok := cleanRoot(in); ok {
			t.Errorf("cleanRoot(%q) accepted", in)
		}
	}
}

func TestValidRef(t *testing.T) {
	good := []string{"", "HEAD", "refs/heads/site", "site", "v1.0.0", "refs/tags/v1", "release-1.2"}
	for _, in := range good {
		if !validRef(in) {
			t.Errorf("validRef(%q) rejected", in)
		}
	}
	bad := []string{
		// The same rule as internal/config's validGitRefName, plus a length cap.
		"@", ".hidden", "refs/heads/.hidden", "a/.b", "a.lock", "refs/heads/x.lock", "a.lock/b",
		"-x", "/x", "x/", "x.", "x.lock", "a..b", "a@{b}", "a//b",
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b",
		"a\x00b", "a\x7fb", strings.Repeat("a", 300),
	}
	for _, in := range bad {
		if validRef(in) {
			t.Errorf("validRef(%q) accepted", in)
		}
	}
}

func TestETagMatches(t *testing.T) {
	tests := []struct {
		header string
		ok     bool
	}{
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`"x", "abc"`, true},
		{`"x", W/"abc"`, true},
		{`*`, true},
		{`abc`, true}, // unquoted tolerance, mirrors archive.matchETag
		{`"other"`, false},
		{``, false},
	}
	for _, tc := range tests {
		if got := etagMatches(tc.header, "abc"); got != tc.ok {
			t.Errorf("etagMatches(%q) = %v, want %v", tc.header, got, tc.ok)
		}
	}
}

func TestContentType(t *testing.T) {
	tests := map[string]string{
		"index.html":       "text/html; charset=utf-8",
		"INDEX.HTML":       "text/html; charset=utf-8",
		"index.htm":        "text/html; charset=utf-8",
		"app.css":          "text/css; charset=utf-8",
		"app.js":           "text/javascript; charset=utf-8",
		"app.mjs":          "text/javascript; charset=utf-8",
		"data.json":        "application/json; charset=utf-8",
		"app.js.map":       "application/json; charset=utf-8",
		"notes.txt":        "text/plain; charset=utf-8",
		"README.md":        "text/markdown; charset=utf-8",
		"logo.svg":         "image/svg+xml",
		"photo.jpeg":       "image/jpeg",
		"font.woff2":       "font/woff2",
		"module.wasm":      "application/wasm",
		"site.webmanifest": "application/manifest+json",
		"archive.zip":      "application/zip",
		"unknown.xyz":      "application/octet-stream",
		"no-ext":           "application/octet-stream",
		"image.PNG":        "image/png",
	}
	for name, want := range tests {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestValidOID(t *testing.T) {
	if !validOID(oid("x")) {
		t.Error("40-hex oid rejected")
	}
	if !validOID(strings.Repeat("ab", 32)) {
		t.Error("64-hex oid rejected")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("A", 40)} {
		if validOID(bad) {
			t.Errorf("validOID(%q) accepted", bad)
		}
	}
}

// TestReadBlobSizeGuard pins the pre-read cap and the liar path: an entry that
// claims a legal size but streams more must fail closed (ErrTooLarge), not
// truncate or overrun.
func TestReadBlobSizeGuard(t *testing.T) {
	f := newFixture(t)
	entry := Entry{Mode: "100644", Type: "blob", OID: oid("index.html"), Size: testLimit + 1}
	if _, err := f.h.readBlob(context.Background(), "/x", entry); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	negative := Entry{Mode: "100644", Type: "blob", OID: oid("index.html"), Size: -1}
	if _, err := f.h.readBlob(context.Background(), "/x", negative); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	liar := Entry{Mode: "100644", Type: "blob", OID: oid("liar.txt"), Size: 4}
	if _, err := f.h.readBlob(context.Background(), "/x", liar); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("lying size err = %v, want ErrTooLarge", err)
	}
}

// capProbeGit records the capacity the handler's buffer had when the read would
// have started: m2 requires a lazy buffer, so a request queued for a pool slot
// holds no body memory and only active reads (bounded by the pool) allocate.
type capProbeGit struct {
	*fakeGit
	capAtEntry []int
}

func (p *capProbeGit) ReadBlob(ctx context.Context, dir, object string, w io.Writer) error {
	if b, ok := w.(*cappedBuffer); ok {
		p.capAtEntry = append(p.capAtEntry, cap(b.buf))
	}
	return p.fakeGit.ReadBlob(ctx, dir, object, w)
}

func TestReadBlobBufferIsLazy(t *testing.T) {
	f := newFixture(t)
	probe := &capProbeGit{fakeGit: f.git}
	f.h.git = probe
	// An entry that claims the whole cap: the buffer still starts nil rather
	// than reserving the declared size before the pool slot is held.
	entry := Entry{Mode: "100644", Type: "blob", OID: oid("file.txt"), Size: testLimit}
	body, err := f.h.readBlob(context.Background(), "/x", entry)
	if err != nil || string(body) != "textfile" {
		t.Fatalf("readBlob = %q, %v", body, err)
	}
	if len(probe.capAtEntry) != 1 || probe.capAtEntry[0] != 0 {
		t.Fatalf("buffer capacity at read start = %v, want one nil buffer (0)", probe.capAtEntry)
	}
}

func TestCappedBufferGrowsLazily(t *testing.T) {
	b := &cappedBuffer{max: 4}
	if b.buf != nil {
		t.Fatalf("zero buffer = %v, want nil", b.buf)
	}
	for _, part := range []string{"ab", "cd"} {
		if n, err := b.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if string(b.buf) != "abcd" || b.overflowed {
		t.Fatalf("buffer = %q overflowed=%v", b.buf, b.overflowed)
	}
	if _, err := b.Write([]byte("e")); !errors.Is(err, ErrTooLarge) || !b.overflowed {
		t.Fatalf("overflowing Write err = %v overflowed=%v, want ErrTooLarge", err, b.overflowed)
	}
	if len(b.buf) != 4 {
		t.Fatalf("buffer after a refused write = %q, want the 4 accepted bytes", b.buf)
	}
}

func TestWritePlainNilRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	writePlain(rec, nil, http.StatusBadRequest, "bad")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "bad" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestParseTreeEntryRejectsMalformedRecords(t *testing.T) {
	malformed := []string{
		"100644 blob " + oid("x") + "\tname\x00",         // no size column
		"100644 blob " + oid("x") + " nope\tname\x00",    // unparseable size
		"100644 blob " + oid("x") + " -5\tname\x00",      // negative size
		"100644 blob not-an-oid 5\tname\x00",             // bad oid
		"100644 blob " + oid("x") + " 5 extra\tname\x00", // too many fields
	}
	for _, raw := range malformed {
		if _, _, err := parseTreeEntry([]byte(raw), "name"); err == nil {
			t.Errorf("parseTreeEntry(%q) accepted a malformed record", raw)
		}
	}
	// An unrelated record (or no tab at all) is simply not the entry we asked for.
	for _, raw := range []string{
		"not-a-record\x00",
		"100644 blob " + oid("other") + " 5\tother\x00",
	} {
		if _, found, err := parseTreeEntry([]byte(raw), "name"); err != nil || found {
			t.Errorf("parseTreeEntry(%q) = found %v err %v", raw, found, err)
		}
	}
	entry, found, err := parseTreeEntry([]byte("040000 tree "+oid("x")+" -\tdir\x00"), "dir")
	if err != nil || !found || entry.Size != -1 || entry.Type != "tree" {
		t.Fatalf("tree record = %+v, %v, %v", entry, found, err)
	}
	if _, found, err := parseTreeEntry(nil, "name"); err != nil || found {
		t.Fatalf("empty output = found %v err %v", found, err)
	}
}
