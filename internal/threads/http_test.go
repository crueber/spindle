package threads

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// HTTP/API matrix (15 §6): read gates the lists, write gates dispatch, and the
// public/private shape matches the sibling feature surfaces.

func newHTTP(t *testing.T) (*testEnv, *Handler, *auth.Principal) {
	t.Helper()
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc}
	cur := e.public()
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return cur, nil }
	return e, h, &cur
}

func do(h *Handler, method, path string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAPIListAndGet(t *testing.T) {
	e, h, _ := newHTTP(t)
	run := e.mustCreate(t)[0]

	rec := do(h, http.MethodGet, "/o/r/api/threads", "")
	if rec.Code != 200 {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var page RunPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID != run.ID {
		t.Fatalf("list = %+v", page)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("threads GETs must be no-store")
	}

	rec = do(h, http.MethodGet, "/o/r/api/threads/"+run.ID, "")
	if rec.Code != 200 {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var view RunView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("get json: %v", err)
	}
	if view.Run.ID != run.ID || len(view.Jobs) != 2 {
		t.Fatalf("view = %+v", view)
	}

	if rec := do(h, http.MethodGet, "/o/r/api/threads/nope", ""); rec.Code != 404 {
		t.Fatalf("unknown run = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads?n=0", ""); rec.Code != 400 {
		t.Fatalf("n=0 = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads?n=999", ""); rec.Code != 400 {
		t.Fatalf("n=999 = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads?after=bogus", ""); rec.Code != 400 {
		t.Fatalf("bad cursor = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/o/r/api/threads", ""); rec.Code != 405 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/"+run.ID, ""); rec.Code != 405 {
		t.Fatalf("POST detail = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/x/y", ""); rec.Code != 404 {
		t.Fatalf("deep path = %d", rec.Code)
	}
	// Non-threads paths fall through to the core mux (Handle false).
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/o/r/issues", nil)) {
		t.Fatal("Handle must return false for /issues")
	}
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/o/r/api/threads", nil)) != true {
		t.Fatal("Handle must claim the threads API")
	}
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1", nil)) {
		t.Fatal("Handle must not claim top-level api paths")
	}
}

func TestAPIPermissionMatrix(t *testing.T) {
	_, h, cur := newHTTP(t)
	// public repo: anonymous reads.
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 200 {
		t.Fatalf("public anon list = %d", rec.Code)
	}
	// private repo: anonymous 401, stranger 403, reader 200, writer 200.
	h.Svc.Roles.(*fakeRoles).public = false
	recAnon := do(h, http.MethodGet, "/o/r/api/threads", "")
	if recAnon.Code != 401 {
		t.Fatalf("private anon list = %d", recAnon.Code)
	}
	if recAnon.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
	*cur = auth.Principal{Name: "stranger"}
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 403 {
		t.Fatalf("private stranger list = %d", rec.Code)
	}
	*cur = auth.Principal{Name: "reader"}
	if rec := do(h, http.MethodGet, "/o/r/api/threads", ""); rec.Code != 200 {
		t.Fatalf("private reader list = %d", rec.Code)
	}
	// Dispatch: read 403, writer 201, anonymous 401.
	if rec := do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main"}`); rec.Code != 403 {
		t.Fatalf("reader dispatch = %d", rec.Code)
	}
	*cur = auth.Principal{Name: "writer"}
	rec := do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main"}`)
	if rec.Code != 201 {
		t.Fatalf("writer dispatch = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Runs []*Run `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Runs) != 1 {
		t.Fatalf("dispatch body = %s (%v)", rec.Body.String(), err)
	}
	*cur = auth.Anonymous()
	h.Svc.Roles.(*fakeRoles).public = true
	if rec := do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main"}`); rec.Code != 401 {
		t.Fatalf("anonymous dispatch = %d", rec.Code)
	}
}

func TestAPIDispatchBodies(t *testing.T) {
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer"}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown field", `{"ref":"refs/heads/main","secrets":true}`, 400},
		{"bad json", `{`, 400},
		{"missing ref", `{}`, 400},
		{"unknown workflow", `{"ref":"refs/heads/main","workflow":"ghost.yml"}`, 404},
		{"unknown ref", `{"ref":"refs/heads/gone"}`, 404},
		{"ok", `{"ref":"refs/heads/main","workflow":"ci.yml"}`, 201},
	}
	for _, tc := range cases {
		if rec := do(h, http.MethodPost, "/o/r/api/threads", tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestAPIUnavailable(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer"}
	e.svc.Git = nil
	recNoGit := do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main"}`)
	if recNoGit.Code != 503 {
		t.Fatalf("no git = %d", recNoGit.Code)
	}
	if recNoGit.Header().Get("Retry-After") != "15" {
		t.Fatal("503 must carry Retry-After")
	}
}

// TestExposedCoversRoutes pins the template↔route correspondence (no phantom
// entries, nothing missing) the sibling packages' discovery tests enforce.
func TestExposedCoversRoutes(t *testing.T) {
	e, h, _ := newHTTP(t)
	run := e.mustCreate(t)[0]
	samples := map[string]string{
		"/{owner}/{repo}/api/threads":       "/o/r/api/threads",
		"/{owner}/{repo}/api/threads/{run}": "/o/r/api/threads/" + run.ID,
	}
	if len(samples) != len(ExposedTemplates) {
		t.Fatalf("ExposedTemplates = %v (samples out of date)", ExposedTemplates)
	}
	for _, tmpl := range ExposedTemplates {
		path, ok := samples[tmpl]
		if !ok {
			t.Fatalf("ExposedTemplates lists %q with no sample", tmpl)
		}
		rec := httptest.NewRecorder()
		if !h.Handle(rec, httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Fatalf("%s: Handle returned false (phantom template)", tmpl)
		}
		if rec.Code != 200 {
			t.Fatalf("%s: sample answered %d", tmpl, rec.Code)
		}
	}
	// /api-browser twin.
	rec := httptest.NewRecorder()
	if !h.Handle(rec, httptest.NewRequest(http.MethodGet, "/o/r/api-browser/threads", nil)) {
		t.Fatal("api-browser twin not claimed")
	}
}
