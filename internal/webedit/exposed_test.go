package webedit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the contents surface
// (Forgejo #272 rule: one entry per distinct path shape, extended in the same
// change as the route).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{"/{owner}/{repo}/api/contents"}
	if len(ExposedTemplates) != len(want) {
		t.Fatalf("ExposedTemplates = %v, want %v", ExposedTemplates, want)
	}
	for i := range want {
		if ExposedTemplates[i] != want[i] {
			t.Fatalf("ExposedTemplates[%d] = %q, want %q", i, ExposedTemplates[i], want[i])
		}
	}
}

// TestExposedCoversRoutes audits the surface both ways: every route the
// handler serves (both lanes, every method) fits the template, and the
// template covers at least one served route.
func TestExposedCoversRoutes(t *testing.T) {
	h, handler := newHTTPHarness(t)
	handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin(), nil }
	_ = h

	served := []struct {
		name, method, path string
		want               bool
	}{
		{"put", "PUT", "/o/r/api/contents?ref=main&path=x", true},
		{"post", "POST", "/o/r/api/contents?ref=main&path=x", true},
		{"delete", "DELETE", "/o/r/api/contents?ref=main&path=x", true},
		{"browser lane", "PUT", "/o/r/api-browser/contents?ref=main&path=x", true},
		{"git suffix", "PUT", "/o/r.git/api/contents?ref=main&path=x", true},
		{"deep", "PUT", "/o/r/api/contents/extra", false},
		{"other family", "PUT", "/o/r/api/tags", false},
		{"top level", "PUT", "/api/v1/contents", false},
		{"page", "PUT", "/o/r/contents", false},
	}
	for _, tc := range served {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"message":"m"}`))
		rec := httptest.NewRecorder()
		if got := handler.Handle(rec, req); got != tc.want {
			t.Errorf("%s: Handle = %v, want %v", tc.name, got, tc.want)
		}
	}
}
