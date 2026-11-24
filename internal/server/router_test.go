package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
	"os"
)

func TestSetupOnlyMode(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only", Errors: []string{"store.backend: unknown backend \"tape\""}}})
	s.SetUI(&fakeUI{paths: map[string]bool{"/admin": true, "/admin/config": true}})
	h := s.Handler()

	// The §3.4 subset answers.
	allowed := map[string]int{
		"/healthz":                    http.StatusOK,
		"/readyz":                     http.StatusOK, // 200 restricted: ready for the admin surface
		"/admin":                      http.StatusOK,
		"/admin/config":               http.StatusOK,
		"/api/v1/setup":               http.StatusOK,
		"/services/public/install.sh": http.StatusOK,
	}
	for path, want := range allowed {
		req := httptest.NewRequest("GET", "http://x"+path, nil)
		req.Header.Set("User-Agent", "curl/8")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s = %d, want %d (body %q)", path, rec.Code, want, rec.Body.String())
		}
	}

	// Everything else → 503 plain text with a pointer to /admin.
	others := []string{"/", "/metrics", "/api/v1/me", "/services/setup.json",
		"/o/r.git/info/refs?service=git-upload-pack", "/repos.js", "/_auth/login"}
	for _, path := range others {
		req := httptest.NewRequest("GET", "http://x"+path, nil)
		req.Header.Set("User-Agent", "curl/8")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503", path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "/admin") {
			t.Fatalf("%s body missing /admin pointer: %q", path, body)
		}
	}

	// readyz is 200 with the restricted status (the ingress contract) and
	// still carries the exact config validation errors.
	req := httptest.NewRequest("GET", "http://x/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"restricted"`) ||
		!strings.Contains(rec.Body.String(), `"surface":"admin"`) ||
		!strings.Contains(rec.Body.String(), `"version":"`) ||
		!strings.Contains(rec.Body.String(), "tape") {
		t.Fatalf("readyz body = %q", rec.Body.String())
	}
}

func TestSetupOnlySetupAPI(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only", Errors: []string{"boom"}}})
	h := s.Handler()
	req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"file_state", "invalid", "groups", "server.listen"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestSetupAPISaveRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dataDir
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		DataDir: dataDir, Boot: BootState{Mode: "setup_only", FileState: "absent"}})
	h := s.Handler()

	// POST test: a complete config → 200 {errors: []}. (A partial save is
	// invalid now: OIDC is required, PLAN-OIDC-ONLY §4.2.)
	body := `{"overrides": {"server.listen": "127.0.0.1:9099",` +
		`"server.auth.issuer": "https://id.example.com",` +
		`"server.auth.allowed_domains": ["example.com"],` +
		`"server.auth.oauth_client_id": "id",` +
		`"server.auth.oauth_client_secret": "secret",` +
		`"server.auth.session_secret": "0123456789abcdef0123456789abcdef",` +
		`"server.auth.admin_groups": ["spindle-admin"]}}`
	req := httptest.NewRequest("POST", "http://x/api/v1/setup/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"errors":[]`) {
		t.Fatalf("test status = %d body = %s", rec.Code, rec.Body.String())
	}

	// POST test: unknown key → 422.
	req = httptest.NewRequest("POST", "http://x/api/v1/setup/test",
		strings.NewReader(`{"overrides": {"server.bogus": "1"}}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid test status = %d body = %s", rec.Code, rec.Body.String())
	}

	// PUT: atomic save via config.SaveSetup.
	req = httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("put status = %d body = %s", rec.Code, rec.Body.String())
	}
	if _, err := osStat(dataDir + "/spindle.toml"); err != nil {
		t.Fatalf("spindle.toml not written: %v", err)
	}
}

// The setup surface has no env-token escape hatch: SPINDLE_SETUP_TOKEN has no
// effect. After a normal boot the surface is admin-only (a real OIDC session
// minted with the admin group passes; a signed-in non-admin is 403; an
// unauthenticated request is 401).
func TestSetupAccessAdminOnly(t *testing.T) {
	t.Setenv("SPINDLE_SETUP_TOKEN", "sekrit")
	cfg := config.Defaults()
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.OAuthClientID = "id"
	cfg.Server.Auth.OAuthClientSecret = "secret"
	cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "normal", FileState: "valid"}})
	svc := NewAuthService(&cfg.Server.Auth, s.Now)
	s.authSvc = svc
	adminSess, err := svc.MintSession("root@example.com", "spindle-admin")
	if err != nil {
		t.Fatal(err)
	}
	userSess, err := svc.MintSession("user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	cases := []struct {
		name   string
		url    string
		auth   string
		cookie string
		want   int
	}{
		{"anonymous", "/api/v1/setup", "", "", http.StatusUnauthorized},
		{"env token as query", "/api/v1/setup?token=sekrit", "", "", http.StatusUnauthorized},
		{"env token as bearer", "/api/v1/setup", "Bearer sekrit", "", http.StatusUnauthorized},
		{"admin session", "/api/v1/setup", "", adminSess.Wire, http.StatusOK},
		{"non-admin session", "/api/v1/setup", "", userSess.Wire, http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "http://x"+tc.url, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		if tc.cookie != "" {
			req.AddCookie(&http.Cookie{Name: "walgit_session", Value: tc.cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s status = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestWildcardDispatch(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.engine.(*fakeEngine).exists = true
	s.engine.(*fakeEngine).bundles = BundleList{
		Fulls: []BundleEntry{{Strategy: "full", Name: "b1.bundle"}},
	}

	t.Run("junk is deliberate 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/nonsense/junk/xyz", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("404 must be plain text, got %q", ct)
		}
	})
	t.Run("bad repo id is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/o/../evil.git/info/refs?service=git-upload-pack", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("bundle object serves with static contract", func(t *testing.T) {
		st := s.store.(store.ObjectStore)
		if _, err := st.Put(nil, "repos/o/r/bundles/full/b1.bundle",
			store.PutBody{Bytes: []byte("BUNDLE-DATA")},
			store.PutOptions{ContentType: "application/x-git-bundle", Immutable: true}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://x/o/r.git/bundles/full/b1.bundle", nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/x-git-bundle" {
			t.Fatalf("content-type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("cache-control = %q", cc)
		}
		if body := rec.Body.String(); body != "BUNDLE-DATA" {
			t.Fatalf("body = %q", body)
		}
	})
	t.Run("bundle list 400 on unknown filter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://x/o/r.git/bundles/list?filter=weird", nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	_, h := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("Allow header missing")
	}
}

func TestMetricsExposition(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "busy")
	s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "busy")
	s.metrics.Gauge("walgit_http_inflight", "in-flight HTTP requests").Set(3)
	body := s.metrics.Render()
	if !strings.Contains(body, "# TYPE walgit_push_refused_total counter") {
		t.Fatalf("family header missing: %s", body)
	}
	if !strings.Contains(body, `walgit_push_refused_total{reason="busy"} 2`) {
		t.Fatalf("labeled sample missing: %s", body)
	}
	if !strings.Contains(body, "walgit_http_inflight 3") {
		t.Fatalf("gauge missing: %s", body)
	}
	// Lexicographic family order.
	names := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("families not sorted: %v", names)
		}
	}
}

func TestReadyzStates(t *testing.T) {
	t.Run("draining", func(t *testing.T) {
		cfg := config.Defaults()
		s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
			Boot: BootState{Mode: "normal"}})
		s.drain.Begin2()
		rec := httptest.NewRecorder()
		s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "15" {
			t.Fatal("Retry-After missing on draining readyz")
		}
	})
}

func TestHealthzAlwaysOK(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(),
		Boot: BootState{Mode: "setup_only"}})
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// osStat wraps os.Stat for the save test.
func osStat(p string) (interface{ Size() int64 }, error) {
	fi, err := os.Stat(p)
	return fi, err
}

// TestTeamPageRoute pins the 08 §1 org team page: "/{owner}/teams/{slug}"
// is a page route the provider answers (GET/HEAD only); other methods 405; a
// repo literally named "teams" keeps its 2-segment root.
func TestTeamPageRoute(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.SetUI(&fakeUI{paths: map[string]bool{"/acme/teams/dev": true, "/acme/teams": true}})
	rows := []struct {
		name   string
		method string
		target string
		want   int
		shell  bool
	}{
		{"team page", "GET", "http://x/acme/teams/dev", http.StatusOK, true},
		{"team page head", "HEAD", "http://x/acme/teams/dev", http.StatusOK, false},
		{"team page post rejected", "POST", "http://x/acme/teams/dev", http.StatusMethodNotAllowed, false},
		{"empty slug 404", "GET", "http://x/acme/teams/", http.StatusNotFound, false},
		{"repo named teams keeps root", "GET", "http://x/acme/teams", http.StatusOK, true},
		{"bad owner 404", "GET", "http://x/Bad%20Owner/teams/dev", http.StatusNotFound, false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Header.Set("Authorization", "Bearer tok123")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
			}
			if tc.shell && !strings.Contains(rec.Body.String(), `id="page"`) {
				t.Fatalf("%s %s missing the page provider's body: %q", tc.method, tc.target, rec.Body.String())
			}
		})
	}
}
