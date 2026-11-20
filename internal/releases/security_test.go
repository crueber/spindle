package releases

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/git"
)

// TestAssetBytesNeverActiveContent pins the UI-01 fix: the uploader's declared
// Content-Type never decides how the origin serves an asset. Only inert raster
// images keep their type; every other type is a binary download, and every
// response is an attachment with nosniff and a sandbox CSP.
func TestAssetBytesNeverActiveContent(t *testing.T) {
	cases := []struct {
		declared string
		want     string
	}{
		{"text/html", "application/octet-stream"},
		{"text/html; charset=utf-8", "application/octet-stream"},
		{"application/xhtml+xml", "application/octet-stream"},
		{"image/svg+xml", "application/octet-stream"},
		{"image/svg+xml; charset=utf-8", "application/octet-stream"},
		{"application/javascript", "application/octet-stream"},
		{"text/javascript", "application/octet-stream"},
		{"text/plain", "application/octet-stream"},
		{"", "application/octet-stream"},
		{"image/png", "image/png"},
		{"image/jpeg", "image/jpeg"},
		{"image/gif", "image/gif"},
		{"image/webp", "image/webp"},
		{"image/avif", "image/avif"},
		{"IMAGE/PNG; foo=bar", "image/png"},
	}
	for _, tc := range cases {
		t.Run(tc.declared, func(t *testing.T) {
			x := newHarness(t)
			grantWrite(x)
			x.git.tags["v1"] = strings.Repeat("a", 40)
			putJSON(t, x, "v1", map[string]any{}, asWriter())

			body := []byte(`<html><body><script>document.title="PWNED"</script></body></html>`)
			hdr := mergeHeaders(asWriter(), map[string]string{
				"X-Walgit-Asset-Sha256": shaOf(body),
				"Content-Type":          tc.declared,
			})
			up := do(t, x, "POST", "/o/r/api/releases/v1/assets/x.html", body, hdr)
			if up.Code != http.StatusCreated {
				t.Fatalf("upload: %d %q", up.Code, up.Body.String())
			}

			id := git.RepoId{Owner: "o", Name: "r"}
			req := httptest.NewRequest(http.MethodGet, "/o/r/releases/v1/assets/x.html", nil)
			req.Header.Set("X-Test-Principal", "bob")
			rec := httptest.NewRecorder()
			if !x.handler.HandleRepo(rec, req, id, []string{"releases", "v1", "assets", "x.html"}) {
				t.Fatal("byte route not claimed")
			}
			res := rec.Result()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("bytes: %d", res.StatusCode)
			}
			if got := res.Header.Get("Content-Type"); got != tc.want {
				t.Fatalf("Content-Type = %q, want %q", got, tc.want)
			}
			if got := res.Header.Get("Content-Type"); strings.Contains(strings.ToLower(got), "html") {
				t.Fatalf("asset served as active content: %q", got)
			}
			cd := res.Header.Get("Content-Disposition")
			mt, params, derr := mime.ParseMediaType(cd)
			if derr != nil || mt != "attachment" || params["filename"] != "x.html" {
				t.Fatalf("Content-Disposition = %q (%v)", cd, derr)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("nosniff = %q", got)
			}
			if got := res.Header.Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
				t.Fatalf("CSP = %q", got)
			}
			if got := res.Header.Get("Cross-Origin-Resource-Policy"); got != "same-site" {
				t.Fatalf("CORP = %q", got)
			}
			// The bytes themselves are untouched (downloads need them), and the
			// download contract still works: ETag/304 and Range/206.
			got, _ := io.ReadAll(res.Body)
			if !bytes.Equal(got, body) {
				t.Fatal("bytes differ")
			}
			etag := res.Header.Get("ETag")

			cond := httptest.NewRequest(http.MethodGet, "/o/r/releases/v1/assets/x.html", nil)
			cond.Header.Set("X-Test-Principal", "bob")
			cond.Header.Set("If-None-Match", etag)
			crec := httptest.NewRecorder()
			x.handler.HandleRepo(crec, cond, id, []string{"releases", "v1", "assets", "x.html"})
			if crec.Code != http.StatusNotModified {
				t.Fatalf("304 = %d", crec.Code)
			}
			rng := httptest.NewRequest(http.MethodGet, "/o/r/releases/v1/assets/x.html", nil)
			rng.Header.Set("X-Test-Principal", "bob")
			rng.Header.Set("Range", "bytes=0-3")
			rrec := httptest.NewRecorder()
			x.handler.HandleRepo(rrec, rng, id, []string{"releases", "v1", "assets", "x.html"})
			if rrec.Code != http.StatusPartialContent || rrec.Body.String() != string(body[:4]) {
				t.Fatalf("206 = %d %q", rrec.Code, rrec.Body.String())
			}
			if rrec.Header().Get("Content-Type") != tc.want {
				t.Fatalf("206 Content-Type = %q", rrec.Header().Get("Content-Type"))
			}
			if !strings.HasPrefix(rrec.Header().Get("Content-Disposition"), "attachment;") {
				t.Fatalf("206 Content-Disposition = %q", rrec.Header().Get("Content-Disposition"))
			}
		})
	}
}

// TestUploadStoresInertContentType: upload normalization clamps the stored
// metadata too, so no consumer of the release JSON can see a type that would
// describe active content.
func TestUploadStoresInertContentType(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{}, asWriter())

	cases := []struct{ name, declared, want string }{
		{"page.html", "text/html", "application/octet-stream"},
		{"logo.svg", "image/svg+xml", "application/octet-stream"},
		{"pic.png", "image/png", "image/png"},
	}
	for _, tc := range cases {
		body := []byte("payload-" + tc.name)
		up := do(t, x, "POST", "/o/r/api/releases/v1/assets/"+tc.name, body,
			mergeHeaders(asWriter(), map[string]string{
				"X-Walgit-Asset-Sha256": shaOf(body),
				"Content-Type":          tc.declared,
			}))
		if up.Code != http.StatusCreated {
			t.Fatalf("%s upload: %d %q", tc.name, up.Code, up.Body.String())
		}
		var ae map[string]any
		if err := json.Unmarshal(up.Body.Bytes(), &ae); err != nil {
			t.Fatal(err)
		}
		if ae["content_type"] != tc.want {
			t.Fatalf("%s stored content_type = %v, want %s", tc.name, ae["content_type"], tc.want)
		}
	}
}

// TestAssetDispositionSanitizesName: the asset name is validated on upload, but
// the header builder must still encode it (quotes/newlines can never split the
// header).
func TestAssetDispositionSanitizesName(t *testing.T) {
	for _, name := range []string{
		`plain.txt`,
		`with space.txt`,
		`quote"name.txt`,
		"tab\tname.txt",
		`ünïcode.txt`,
		"\xffbad.txt",
	} {
		got := assetDisposition(name)
		if !strings.HasPrefix(got, "attachment;") || len(got) <= len("attachment;") {
			t.Fatalf("%q: %q", name, got)
		}
		if strings.ContainsAny(strings.TrimPrefix(got, "attachment;"), "\r\n") {
			t.Fatalf("%q: header injection in %q", name, got)
		}
	}
}
