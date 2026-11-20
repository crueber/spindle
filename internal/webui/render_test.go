package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

// newBare builds a provider with the real templates and no wired services —
// enough to exercise the renderer and the error paths.
func newBare(t *testing.T) *Server {
	t.Helper()
	s, err := New(Env{Cfg: &config.Config{}, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestErrorPageRendersStatus(t *testing.T) {
	s := newBare(t)
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "Not found"},
		{http.StatusForbidden, "Forbidden"},
		{http.StatusUnauthorized, "Sign in required"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/missing", nil)
		s.errorPage(rec, req, tc.status, "detail text")
		if rec.Code != tc.status {
			t.Fatalf("status %d → %d", tc.status, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.want) || !strings.Contains(body, "detail text") {
			t.Fatalf("status %d body missing %q/detail: %.200s", tc.status, tc.want, body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("status %d cache-control = %q", tc.status, got)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("status %d has no ETag", tc.status)
		}
	}
}

// A missing page template must produce one 500 and stop: the renderer must
// never recurse into the error page (that shipped a stack overflow once).
func TestRenderMissingTemplateDoesNotRecurse(t *testing.T) {
	s := newBare(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.render(rec, req, http.StatusOK, "no/such/page", PageData{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "page template missing") {
		t.Fatalf("body = %.200s", rec.Body.String())
	}
}

// The error page compares an int status with the template builtin `eq`; this
// pins that the helper map does not shadow it with a string-only func.
func TestErrorPageBuiltinEq(t *testing.T) {
	s := newBare(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.errorPage(rec, req, http.StatusUnauthorized, "nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (builtin eq shadowed?)", rec.Code)
	}
}
