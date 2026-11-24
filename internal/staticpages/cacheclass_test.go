package staticpages

import (
	"net/http"
	"testing"
)

// TestCacheClassContract pins the package's only cache class (§4): 200s (and
// their 304 revalidations) are `public, max-age=600`; 404s and 301s carry no
// cache headers at all, so a fixed or removed site is visible within minutes.
func TestCacheClassContract(t *testing.T) {
	if CacheControl != "public, max-age=600" {
		t.Fatalf("CacheControl = %q", CacheControl)
	}
	f := newFixture(t)
	tests := []struct {
		name       string
		method     string
		target     string
		headers    map[string]string
		status     int
		wantCache  string
		wantRanges bool
	}{
		{name: "index 200", target: "/acme/site/", status: http.StatusOK, wantCache: CacheControl, wantRanges: true},
		{name: "asset 200", target: "/acme/site/assets/app.css", status: http.StatusOK, wantCache: CacheControl, wantRanges: true},
		{name: "html fallback 200", target: "/acme/site/about", status: http.StatusOK, wantCache: CacheControl, wantRanges: true},
		{name: "revalidation 304", target: "/acme/site/", status: http.StatusNotModified, wantCache: CacheControl,
			headers: map[string]string{"If-None-Match": `"` + oid("index.html") + `"`}},
		{name: "site 404", target: "/acme/site/missing", status: http.StatusNotFound},
		{name: "plain 404", target: "/acme/plain/missing", status: http.StatusNotFound},
		{name: "private 404", target: "/acme/private/", status: http.StatusNotFound},
		{name: "host 404", target: "/acme/site/", status: http.StatusNotFound, headers: map[string]string{"X-Host": "evil"}},
		{name: "site root 301", target: "/acme/site", status: http.StatusMovedPermanently},
		{name: "tree 301", target: "/acme/site/docs", status: http.StatusMovedPermanently},
		{name: "dotfile 404", target: "/acme/site/.env", status: http.StatusNotFound},
		{name: "405", method: http.MethodPost, target: "/acme/site/", status: http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := testHost
			if tc.name == "host 404" {
				host = "evil.localhost"
			}
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			rec := f.do(t, method, tc.target, host, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tc.wantCache)
			}
			if got := rec.Header().Get("Accept-Ranges"); (got == "bytes") != tc.wantRanges {
				t.Errorf("Accept-Ranges = %q, wantRanges=%v", got, tc.wantRanges)
			}
			if tc.wantCache == "" && rec.Header().Get("ETag") != "" && tc.status != http.StatusNotModified {
				t.Errorf("uncacheable response carries ETag %q", rec.Header().Get("ETag"))
			}
		})
	}
}
