package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestRawContentType(t *testing.T) {
	cases := map[string]string{
		"docs/demo.webp": "image/webp",
		"a.PNG":          "image/png",
		"a.jpeg":         "image/jpeg",
		"a.gif":          "image/gif",
		"a.avif":         "image/avif",
		"a.bmp":          "image/bmp",
		"a.ico":          "image/x-icon",
		"logo.svg":       "image/svg+xml",
		"a.mp4":          "video/mp4",
		"a.webm":         "video/webm",
		"a.ogv":          "video/ogg",
		"a.mp3":          "audio/mpeg",
		"a.wav":          "audio/wav",
		"a.ogg":          "audio/ogg",
		"a.m4a":          "audio/mp4",
		// other active/text content stays text/plain (nosniff keeps it inert)
		"README.md": "text/plain; charset=utf-8",
		"a.html":    "text/plain; charset=utf-8",
		"a":         "text/plain; charset=utf-8",
	}
	for p, want := range cases {
		if got := rawContentType(p); got != want {
			t.Errorf("rawContentType(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestBlobRawSVGIsSandboxed pins the SVG lane (audit FAIL 2): a real
// image/svg+xml type so a README <img> renders, plus a CSP that makes a
// direct navigation inert, and nosniff on every raw response.
func TestBlobRawSVGIsSandboxed(t *testing.T) {
	f := newFixture(t)
	f.view.resolves["demo/walgit/"+fakeSHA] = Resolution{Ref: "", SHA: fakeSHA, Kind: "commit", Revision: 7}
	f.view.resolves["demo/walgit/"+fakeSHA+"/logo.svg"] = Resolution{Ref: "", SHA: fakeSHA, Path: "logo.svg", Kind: "commit", Revision: 7}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|logo.svg"] = svg

	w := f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/logo.svg?raw")
	if w.Code != http.StatusOK {
		t.Fatalf("svg raw = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Fatalf("svg content-type = %q, want image/svg+xml", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Fatalf("svg CSP = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("svg nosniff = %q", got)
	}
	if w.Body.String() != string(svg) {
		t.Fatalf("svg body changed: %q", w.Body.String())
	}

	// The raster sibling keeps its type and does NOT carry the sandbox
	// policy (nothing to sandbox).
	f.view.resolves["demo/walgit/"+fakeSHA+"/badge.png"] = Resolution{Ref: "", SHA: fakeSHA, Path: "badge.png", Kind: "commit", Revision: 7}
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|badge.png"] = []byte("\x89PNG\r\n\x1a\n")
	w = f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/badge.png?raw")
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("png content-type = %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "sandbox") {
		t.Fatalf("png carries the svg sandbox CSP: %q", got)
	}

	// HTML stays octet-typed text (never executable): the release-asset lane
	// owns attachment semantics, the raw lane keeps text/plain + nosniff.
	f.view.resolves["demo/walgit/"+fakeSHA+"/evil.html"] = Resolution{Ref: "", SHA: fakeSHA, Path: "evil.html", Kind: "commit", Revision: 7}
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|evil.html"] = []byte("<script>alert(1)</script>")
	w = f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/evil.html?raw")
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("html raw content-type = %q", got)
	}
}
