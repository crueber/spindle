package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crueber/spindle/internal/httpsec"
)

// TestSecurityHeadersOnEveryLane pins UI-02 at the server middleware: page,
// API and not-found responses all carry the shared header set (06 §8.7).
func TestSecurityHeadersOnEveryLane(t *testing.T) {
	_, h := newTestServer(t, nil)
	for _, path := range []string{"/", "/api/v1", "/does-not-exist"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := map[string]string{
			"Content-Security-Policy": httpsec.ContentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "same-origin",
			"X-Content-Type-Options":  "nosniff",
		}
		for name, val := range want {
			if got := rec.Header().Get(name); got != val {
				t.Errorf("%s %s = %q, want %q", path, name, got, val)
			}
		}
	}
}

// TestSecurityHeadersRespectStricterRoute: a route that sets its own CSP (the
// release-asset sandbox lane) is not overwritten by the middleware.
func TestSecurityHeadersRespectStricterRoute(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.serverHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/o/r/releases/v1/assets/x.html", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Fatalf("route CSP overwritten: %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("missing XFO: %q", got)
	}
}
