package threads

import (
	"encoding/json"
	"errors"
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

// TestAPIWorkflows pins GET …/api/threads/workflows (the per-file parse state
// and error object) — including the one-bad-file case.
func TestAPIWorkflows(t *testing.T) {
	e, h, _ := newHTTP(t)
	e.git.refs["HEAD"] = testSHA
	e.git.add(testSHA, ".spindle/workflows/broken.yml", "name: broken\non: [\n")
	rec := do(h, http.MethodGet, "/o/r/api/threads/workflows", "")
	if rec.Code != 200 {
		t.Fatalf("workflows = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("workflows GETs must be no-store")
	}
	var list WorkflowList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("json: %v", err)
	}
	if list.Commit != testSHA || len(list.Workflows) != 2 {
		t.Fatalf("list = %+v", list)
	}
	var broken, good *WorkflowState
	for _, wf := range list.Workflows {
		if wf.Path == ".spindle/workflows/broken.yml" {
			broken = wf
		} else {
			good = wf
		}
	}
	if broken == nil || broken.Error == "" || broken.Name != "broken" {
		t.Fatalf("broken = %+v", broken)
	}
	if good == nil || good.Error != "" || len(good.Triggers) != 4 || good.Name != "CI" {
		t.Fatalf("good = %+v", good)
	}
	// An explicit commit is honoured; a malformed one is a 400.
	if rec := do(h, http.MethodGet, "/o/r/api/threads/workflows?commit=nope", ""); rec.Code != 400 {
		t.Fatalf("bad commit = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/workflows", "{}"); rec.Code != 405 {
		t.Fatalf("POST workflows = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/not-a-repo/api/threads/workflows", ""); rec.Code != 404 {
		t.Fatalf("bad repo = %d", rec.Code)
	}
}

func TestAPIListEmptyRepoWorkflows(t *testing.T) {
	// A repo with no resolvable HEAD lists no workflows (not a 404).
	e, h, _ := newHTTP(t)
	e.git.refs = map[string]string{}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/workflows", ""); rec.Code != 200 {
		t.Fatalf("empty repo workflows = %d: %s", rec.Code, rec.Body.String())
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
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer", Admin: true}
	run := e.mustCreate(t)[0]
	jid := "j-build"
	if _, perr := e.svc.PutEnvironment(ctx(), "o/r", "prod", EnvironmentInput{}, *cur); perr != nil {
		t.Fatalf("create environment: %v", perr)
	}
	minted, merr := e.svc.MintRegistrationToken(ctx(), *cur, mintAllPolicy(""))
	if merr != nil {
		t.Fatalf("mint registration token: %v", merr)
	}
	type sample struct {
		method string
		path   string
		// claimed-only routes (the runner protocol authenticates with its own
		// token, so an anonymous probe is a real 401 — the sample only asserts
		// the surface claims the path).
		claimedOnly bool
	}
	// notFoundOK marks routes whose correct answer for an empty fixture is 404
	// (an artifact/cache entry that does not exist): the sample then only
	// asserts the path is CLAIMED, which is what this test is about.
	notFoundOK := map[string]bool{
		"/{owner}/{repo}/api/threads/{run}/artifacts/{name}":    true,
		"/{owner}/{repo}/api/threads/caches/{id}":               true,
		"/{owner}/{repo}/api/threads/deployments/{id}":          true,
		"/{owner}/{repo}/api/threads/deployments/{id}/statuses": true,
	}
	samples := map[string]sample{
		"/{owner}/{repo}/api/threads":                    {http.MethodGet, "/o/r/api/threads", false},
		"/{owner}/{repo}/api/threads/workflows":          {http.MethodGet, "/o/r/api/threads/workflows", false},
		"/{owner}/{repo}/api/threads/dispatches":         {http.MethodPost, "/o/r/api/threads/dispatches", false},
		"/{owner}/{repo}/api/threads/settings":           {http.MethodGet, "/o/r/api/threads/settings", false},
		"/{owner}/{repo}/api/threads/{run}":              {http.MethodGet, "/o/r/api/threads/" + run.ID, false},
		"/{owner}/{repo}/api/threads/{run}/cancel":       {http.MethodPost, "/o/r/api/threads/" + run.ID + "/cancel", false},
		"/{owner}/{repo}/api/threads/{run}/rerun":        {http.MethodPost, "/o/r/api/threads/" + run.ID + "/rerun", false},
		"/{owner}/{repo}/api/threads/{run}/rerun-failed": {http.MethodPost, "/o/r/api/threads/" + run.ID + "/rerun-failed", false},
		"/{owner}/{repo}/api/threads/{run}/approve":      {http.MethodPost, "/o/r/api/threads/" + run.ID + "/approve", false},
		"/{owner}/{repo}/api/threads/{run}/deny":         {http.MethodPost, "/o/r/api/threads/" + run.ID + "/deny", false},
		"/{owner}/{repo}/api/threads/{run}/logs.zip": {
			http.MethodGet, "/o/r/api/threads/" + run.ID + "/logs.zip", false},
		"/{owner}/{repo}/api/threads/{run}/logs.txt": {
			http.MethodGet, "/o/r/api/threads/" + run.ID + "/logs.txt", false},
		"/{owner}/{repo}/api/threads/{run}/jobs/{job}/log": {
			http.MethodGet, "/o/r/api/threads/" + run.ID + "/jobs/" + jid + "/log?accept=json", false},
		"/{owner}/{repo}/api/threads/{run}/jobs/{job}/cancel": {
			http.MethodPost, "/o/r/api/threads/" + run.ID + "/jobs/" + jid + "/cancel", false},
		"/{owner}/{repo}/api/threads/{run}/artifacts": {http.MethodGet, "/o/r/api/threads/" + run.ID + "/artifacts", false},
		"/{owner}/{repo}/api/threads/{run}/artifacts/{name}": {
			http.MethodGet, "/o/r/api/threads/" + run.ID + "/artifacts/build", false},
		"/{owner}/{repo}/api/threads/caches":              {http.MethodGet, "/o/r/api/threads/caches", false},
		"/{owner}/{repo}/api/threads/caches/{id}":         {http.MethodDelete, "/o/r/api/threads/caches/abc", false},
		"/api/v1/admin/runners":                           {http.MethodGet, "/api/v1/admin/runners", false},
		"/api/v1/admin/runners/tokens":                    {http.MethodPost, "/api/v1/admin/runners/tokens", false},
		"/api/v1/admin/runners/{id}":                      {http.MethodDelete, "/api/v1/admin/runners/" + minted.ID, false},
		"/api/v1/admin/runners/{id}/policy":               {http.MethodPost, "/api/v1/admin/runners/" + minted.ID + "/policy", false},
		"/api/v1/admin/runner-groups":                     {http.MethodGet, "/api/v1/admin/runner-groups", false},
		"/api/v1/admin/runner-groups/{id}":                {http.MethodDelete, "/api/v1/admin/runner-groups/build", false},
		"/{owner}/{repo}/api/threads/secrets":             {http.MethodGet, "/o/r/api/threads/secrets", false},
		"/{owner}/{repo}/api/threads/schedules":           {http.MethodGet, "/o/r/api/threads/schedules", false},
		"/{owner}/{repo}/api/threads/schedules/enable":    {http.MethodPost, "/o/r/api/threads/schedules/enable", false},
		"/{owner}/{repo}/api/threads/schedules/run-now":   {http.MethodPost, "/o/r/api/threads/schedules/run-now", false},
		"/{owner}/{repo}/api/threads/pause":               {http.MethodPost, "/o/r/api/threads/pause", false},
		"/{owner}/{repo}/api/threads/queue":               {http.MethodGet, "/o/r/api/threads/queue", false},
		"/{owner}/{repo}/api/threads/notifications":       {http.MethodGet, "/o/r/api/threads/notifications", false},
		"/{owner}/{repo}/api/threads/notifications/read":  {http.MethodPost, "/o/r/api/threads/notifications/read", false},
		"/{owner}/{repo}/api/threads/subscription":        {http.MethodGet, "/o/r/api/threads/subscription", false},
		"/{owner}/{repo}/api/threads/validate":            {http.MethodPost, "/o/r/api/threads/validate", false},
		"/{owner}/{repo}/api/threads/secrets/{name}":      {http.MethodPut, "/o/r/api/threads/secrets/TOKEN", false},
		"/{owner}/{repo}/api/threads/variables":           {http.MethodGet, "/o/r/api/threads/variables", false},
		"/{owner}/{repo}/api/threads/variables/{name}":    {http.MethodPut, "/o/r/api/threads/variables/REGION", false},
		"/api/v1/orgs/{org}/threads/variables":            {http.MethodGet, "/api/v1/orgs/acme/threads/variables", false},
		"/api/v1/orgs/{org}/threads/variables/{name}":     {http.MethodPut, "/api/v1/orgs/acme/threads/variables/REGION", false},
		"/api/v1/orgs/{org}/threads/secrets":              {http.MethodGet, "/api/v1/orgs/acme/threads/secrets", false},
		"/api/v1/orgs/{org}/threads/secrets/{name}":       {http.MethodPut, "/api/v1/orgs/acme/threads/secrets/TOKEN", false},
		"/api/v1/orgs/{org}/threads/settings":             {http.MethodGet, "/api/v1/orgs/acme/threads/settings", false},
		"/{owner}/{repo}/api/threads/environments":        {http.MethodGet, "/o/r/api/threads/environments", false},
		"/{owner}/{repo}/api/threads/environments/{name}": {http.MethodPut, "/o/r/api/threads/environments/prod", false},
		"/{owner}/{repo}/api/threads/environments/{name}/secrets": {
			http.MethodGet, "/o/r/api/threads/environments/prod/secrets", false},
		"/{owner}/{repo}/api/threads/environments/{name}/secrets/{secret}": {
			http.MethodPut, "/o/r/api/threads/environments/prod/secrets/TOKEN", false},
		"/{owner}/{repo}/api/threads/environments/{name}/variables": {
			http.MethodGet, "/o/r/api/threads/environments/prod/variables", false},
		"/{owner}/{repo}/api/threads/environments/{name}/variables/{variable}": {
			http.MethodPut, "/o/r/api/threads/environments/prod/variables/REGION", false},
		"/{owner}/{repo}/api/threads/deployments": {
			http.MethodGet, "/o/r/api/threads/deployments", false},
		"/{owner}/{repo}/api/threads/deployments/{id}": {
			http.MethodGet, "/o/r/api/threads/deployments/d-nope", false},
		"/{owner}/{repo}/api/threads/deployments/{id}/statuses": {
			http.MethodPost, "/o/r/api/threads/deployments/d-nope/statuses", false},
		"/api/v1/threads/runners/register":                {http.MethodPost, "/api/v1/threads/runners/register", true},
		"/api/v1/threads/runners/declare":                 {http.MethodPost, "/api/v1/threads/runners/declare", true},
		"/api/v1/threads/runners/fetch":                   {http.MethodPost, "/api/v1/threads/runners/fetch", true},
		"/api/v1/threads/runners/drain":                   {http.MethodPost, "/api/v1/threads/runners/drain", true},
		"/api/v1/threads/runners/resume":                  {http.MethodPost, "/api/v1/threads/runners/resume", true},
		"/api/v1/threads/runners/rotate":                  {http.MethodPost, "/api/v1/threads/runners/rotate", true},
		"/api/v1/threads/runners/deregister":              {http.MethodPost, "/api/v1/threads/runners/deregister", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/start":    {http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/start", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/log":      {http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/log", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/complete": {http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/complete", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/cancel":   {http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/cancel", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/id-token": {http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/id-token", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts": {
			http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/artifacts", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}": {
			http.MethodGet, "/api/v1/threads/jobs/jr_x/attempt/1/artifacts/build", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{index}": {
			http.MethodGet, "/api/v1/threads/jobs/jr_x/attempt/1/artifacts/build/blobs/0", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/prepare": {
			http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/artifacts/build/prepare", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/lookup": {
			http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/cache/lookup", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/save": {
			http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/cache/save", true},
		"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/blobs/{sha}": {
			http.MethodGet, "/api/v1/threads/jobs/jr_x/attempt/1/cache/blobs/" + strings.Repeat("a", 64), true},
	}
	if len(samples) != len(ExposedTemplates) {
		t.Fatalf("ExposedTemplates = %v (samples out of date)", ExposedTemplates)
	}
	for _, tmpl := range ExposedTemplates {
		s, ok := samples[tmpl]
		if !ok {
			t.Fatalf("ExposedTemplates lists %q with no sample", tmpl)
		}
		var body *strings.Reader
		if s.method == http.MethodPut {
			body = strings.NewReader(`{"value":"x"}`)
		} else {
			body = strings.NewReader("")
		}
		rec := httptest.NewRecorder()
		if !h.Handle(rec, httptest.NewRequest(s.method, s.path, body)) {
			t.Fatalf("%s: Handle returned false (phantom template)", tmpl)
		}
		if s.claimedOnly {
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
				t.Fatalf("%s: anonymous protocol probe answered %d", tmpl, rec.Code)
			}
			if rec.Header().Get(runnerVersionHeader) != runnerVersion {
				t.Fatalf("%s: missing %s header", tmpl, runnerVersionHeader)
			}
			continue
		}
		if rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s: sample answered %d", tmpl, rec.Code)
		}
		if rec.Code == http.StatusNotFound && !notFoundOK[tmpl] {
			t.Fatalf("%s: sample answered 404", tmpl)
		}
	}
	// /api-browser twin.
	rec := httptest.NewRecorder()
	if !h.Handle(rec, httptest.NewRequest(http.MethodGet, "/o/r/api-browser/threads", nil)) {
		t.Fatal("api-browser twin not claimed")
	}
}

// TestRunLogsAPI pins the whole-run download's HTTP shape and gate: both formats
// are attachments named after the run, and an unknown run is a real 404 (never
// an empty archive that reads like a run with no output).
func TestRunLogsAPI(t *testing.T) {
	e, h, _ := newHTTP(t)
	run := e.mustCreate(t)[0]
	for _, tc := range []struct{ path, ctype string }{
		{"/o/r/api/threads/" + run.ID + "/logs.txt", "text/plain; charset=utf-8"},
		{"/o/r/api/threads/" + run.ID + "/logs.zip", "application/zip"},
	} {
		rec := do(h, http.MethodGet, tc.path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", tc.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != tc.ctype {
			t.Fatalf("%s content-type = %q", tc.path, got)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, run.ID) {
			t.Fatalf("%s disposition = %q, want the run id", tc.path, cd)
		}
	}
	rec := do(h, http.MethodGet, "/o/r/api/threads/1795176002000000000-nope/logs.zip", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", rec.Code)
	}
}

// failingHTTPWriter accepts headers and then fails every body write, which is a
// client that went away mid-download: the handler must not PANIC or re-render
// into the archive; it logs the aborted copy.
type failingHTTPWriter struct {
	hdr  http.Header
	code int
}

func (w *failingHTTPWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *failingHTTPWriter) WriteHeader(code int) { w.code = code }
func (w *failingHTTPWriter) Write([]byte) (int, error) {
	return 0, errors.New("client gone")
}

func TestRunLogsAPISurvivesAWriteFailure(t *testing.T) {
	e, h, _ := newHTTP(t)
	run := e.mustCreate(t)[0]
	for _, path := range []string{
		"/o/r/api/threads/" + run.ID + "/logs.zip",
		"/o/r/api/threads/" + run.ID + "/logs.txt",
	} {
		w := &failingHTTPWriter{}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(w, req)
		if w.code != http.StatusOK {
			t.Fatalf("%s status = %d", path, w.code)
		}
	}
}
