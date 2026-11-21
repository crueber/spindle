package packages

// exposed_test.go — the discovery contract for the Packages JSON surface
// (Forgejo #272): one template per distinct path shape, and no phantom entry.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/packages",
		"/{owner}/{repo}/api/packages/{digest}",
		"/{owner}/{repo}/api/packages/tags/{tag}",
	}
	if len(ExposedTemplates) != len(want) {
		t.Fatalf("ExposedTemplates = %v, want %v", ExposedTemplates, want)
	}
	for i := range want {
		if ExposedTemplates[i] != want[i] {
			t.Fatalf("ExposedTemplates[%d] = %q, want %q", i, ExposedTemplates[i], want[i])
		}
	}
}

func exposedMatch(template, path string) bool {
	toks := strings.Split(strings.TrimPrefix(template, "/"), "/")
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(toks) != len(segs) {
		return false
	}
	for i, tok := range toks {
		if strings.HasPrefix(tok, "{") && strings.HasSuffix(tok, "}") {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if tok != segs[i] {
			return false
		}
	}
	return true
}

// TestExposedCoversAPIRoutes audits the surface both ways: every API route
// Handle claims matches a template, and every template covers a served route.
// The /v2 registry namespace is not a discovery entry (it is not the JSON
// API), so it is checked as a non-match.
func TestExposedCoversAPIRoutes(t *testing.T) {
	f := newFixture(t)
	f.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "admin", Admin: true}, nil
	}
	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"list", "GET", "/o/r/api/packages", true, "/{owner}/{repo}/api/packages"},
		{"list browser lane", "GET", "/o/r/api-browser/packages", true, "/{owner}/{repo}/api/packages"},
		{"list git suffix", "GET", "/o/r.git/api/packages", true, "/{owner}/{repo}/api/packages"},
		{"list wrong method", "POST", "/o/r/api/packages", true, "/{owner}/{repo}/api/packages"},
		{"get", "GET", "/o/r/api/packages/" + testDigestForExposed, true, "/{owner}/{repo}/api/packages/{digest}"},
		{"get wrong method", "PUT", "/o/r/api/packages/" + testDigestForExposed, true, "/{owner}/{repo}/api/packages/{digest}"},
		{"delete tag", "DELETE", "/o/r/api/packages/tags/v1", true, "/{owner}/{repo}/api/packages/tags/{tag}"},
		{"delete tag wrong method", "GET", "/o/r/api/packages/tags/v1", true, "/{owner}/{repo}/api/packages/tags/{tag}"},
		{"delete manifest", "DELETE", "/o/r/api/packages/" + testDigestForExposed, true, "/{owner}/{repo}/api/packages/{digest}"},
		// Claimed but NOT a discovery entry: the /v2 wire namespace is not
		// the JSON API (Forgejo #272 covers api lanes only).
		{"registry", "GET", "/v2/o/r/tags/list", true, ""},
		{"other family", "GET", "/o/r/api/releases", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"shallow", "GET", "/o/r/api", false, ""},
		{"deep", "GET", "/o/r/api/packages/a/b/c", true, ""},
	}
	covered := make([]bool, len(ExposedTemplates))
	for _, tc := range served {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		got := f.h.Handle(rec, req)
		if got != tc.want {
			t.Errorf("%s: Handle(%s %s) = %v, want %v (status %d)", tc.name, tc.method, tc.path, got, tc.want, rec.Code)
			continue
		}
		if !tc.want || tc.canonical == "" {
			continue
		}
		hit := false
		for i, tmpl := range ExposedTemplates {
			if exposedMatch(tmpl, exposedLane(tc.path)) {
				covered[i] = true
				if tmpl == tc.canonical {
					hit = true
				}
			}
		}
		if !hit {
			t.Errorf("%s: %s not covered by canonical template %q", tc.name, tc.path, tc.canonical)
		}
	}
	for i, tmpl := range ExposedTemplates {
		if !covered[i] {
			t.Errorf("template %q covers no served route (phantom entry)", tmpl)
		}
	}
}

const testDigestForExposed = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
