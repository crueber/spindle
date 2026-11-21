// cli_commands_test.go — httptest fakes for every WP11 command family.
// Each test pins the request path/method/body/headers, the token header,
// the exit code, and (where it matters) the --json output shape. No live
// server, no store: the CLI is exercised as an HTTP client only.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- fake server ----------------------------------------------------------

type recordedReq struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	records []recordedReq
	routes  map[string]http.HandlerFunc
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{t: t, routes: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		f.mu.Lock()
		f.records = append(f.records, recordedReq{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
			Header: r.Header.Clone(), Body: body.Bytes(),
		})
		f.mu.Unlock()
		h, ok := f.routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// on registers a canned JSON response.
func (f *fakeAPI) on(method, path string, status int, body string) {
	f.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeAPI) onFunc(method, path string, h http.HandlerFunc) {
	f.routes[method+" "+path] = h
}

func (f *fakeAPI) last() recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.records[len(f.records)-1]
}

// first is last for the opening request of a multi-call command.
func (f *fakeAPI) first() recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.records[0]
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// runCmd runs a command with isolated env + captured output.
func runCmd(t *testing.T, srv *httptest.Server, token string, fn func(ctx context.Context, c *cli, args []string) int, args ...string) (int, string, string) {
	t.Helper()
	c := isolateCLI(t, srv.URL)
	if token != "" {
		t.Setenv("SPINDLE_TOKEN", token)
	}
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	code := fn(t.Context(), c, args)
	return code, out.String(), errOut.String()
}

// decodeBody asserts the last request's JSON body equals want (via round
// trip so key order never matters).
func decodeBody(t *testing.T, rec recordedReq, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
	}
	shouldEqualJSON(t, got, want)
}

func shouldEqualJSON(t *testing.T, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", g, w)
	}
}

// --- user / keys ----------------------------------------------------------

func TestUserCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 200, `{"principal":"alice","write":true,"admin":false,"anonymous":false}`)
	f.on("GET", "/api/v1/owners", 200, `["acme","alice"]`)
	f.on("GET", "/api/v1/users/alice", 200, `{"username":"alice","display_name":"Alice","created_at":"2026-01-01T00:00:00Z"}`)
	f.on("GET", "/api/v1/ssh-keys", 200, `[{"principal":"alice","fingerprint":"SHA256:abc","id":"k1","key":"ssh-ed25519 AAAA","title":"laptop","created_at":"2026-01-01T00:00:00Z"}]`)
	f.on("POST", "/api/v1/ssh-keys", 201, `{"principal":"alice","fingerprint":"SHA256:def","id":"k2","key":"ssh-rsa BBBB","title":"ci","created_at":"2026-01-02T00:00:00Z"}`)
	f.on("DELETE", "/api/v1/ssh-keys/k1", 200, `{"deleted":"k1"}`)

	code, out, _ := runCmd(t, srv, "tok", runUserWhoami)
	if code != exitOK || !strings.Contains(out, "alice") {
		t.Fatalf("whoami = %d %q", code, out)
	}
	if got := f.last().Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}

	code, out, _ = runCmd(t, srv, "tok", runUserList)
	if code != exitOK || !strings.Contains(out, "acme") || !strings.Contains(out, "alice") {
		t.Fatalf("user list = %d %q", code, out)
	}

	code, out, _ = runCmd(t, srv, "tok", runUserShow, "alice")
	if code != exitOK || !strings.Contains(out, "Alice") {
		t.Fatalf("user show = %d %q", code, out)
	}

	code, out, _ = runCmd(t, srv, "tok", runKeysList)
	if code != exitOK || !strings.Contains(out, "SHA256:abc") {
		t.Fatalf("keys list = %d %q", code, out)
	}

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(keyFile, []byte("ssh-rsa BBBB user@host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCmd(t, srv, "tok", runKeysAdd, "--file", keyFile, "--title", "ci")
	if code != exitOK || !strings.Contains(out, "SHA256:def") {
		t.Fatalf("keys add = %d %q", code, out)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/api/v1/ssh-keys" {
		t.Fatalf("keys add request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{"key": "ssh-rsa BBBB user@host", "title": "ci"})

	code, _, _ = runCmd(t, srv, "tok", runKeysRemove, "k1")
	if code != exitOK {
		t.Fatalf("keys remove = %d", code)
	}
	if rec := f.last(); rec.Method != "DELETE" || rec.Path != "/api/v1/ssh-keys/k1" {
		t.Fatalf("keys remove request = %s %s", rec.Method, rec.Path)
	}
}

// --- org ------------------------------------------------------------------

func TestOrgCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/orgs", 200, `["acme"]`)
	f.on("GET", "/api/v1/orgs/acme", 200, `{"org":"acme","display_name":"Acme","description":"","created_at":"2026-01-01T00:00:00Z"}`)
	f.on("POST", "/api/v1/orgs", 201, `{"org":"acme"}`)
	f.on("GET", "/api/v1/orgs/acme/members", 200, `{"version":1,"members":[{"principal":"alice","role":"owner","joined_at":"2026-01-01T00:00:00Z"}]}`)
	f.on("PUT", "/api/v1/orgs/acme/members/bob", 200, `{}`)
	f.on("DELETE", "/api/v1/orgs/acme/members/bob", 200, `{}`)
	f.on("GET", "/api/v1/orgs/acme/teams", 200, `[{"org":"acme","slug":"devs","name":"Devs","members":["alice"]}]`)
	f.on("POST", "/api/v1/orgs/acme/teams", 201, `{"org":"acme","slug":"devs","name":"Devs"}`)

	if code, _, _ := runCmd(t, srv, "", runOrgList); code != exitOK {
		t.Fatalf("org list = %d", code)
	}
	if code, out, _ := runCmd(t, srv, "", runOrgShow, "acme"); code != exitOK || !strings.Contains(out, "Acme") {
		t.Fatalf("org show = %d %q", code, out)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgCreate, "acme", "--display-name", "Acme"); code != exitOK {
		t.Fatalf("org create = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"org": "acme", "display_name": "Acme", "description": ""})

	if code, out, _ := runCmd(t, srv, "", runOrgMemberList, "acme"); code != exitOK || !strings.Contains(out, "alice") {
		t.Fatalf("member list = %d %q", code, out)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgMemberAdd, "acme", "bob", "--role", "owner"); code != exitOK {
		t.Fatalf("member add = %d", code)
	}
	if rec := f.last(); rec.Method != "PUT" || rec.Path != "/api/v1/orgs/acme/members/bob" {
		t.Fatalf("member add request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, f.last(), map[string]any{"role": "owner"})
	if code, _, _ := runCmd(t, srv, "tok", runOrgMemberRemove, "acme", "bob"); code != exitOK {
		t.Fatalf("member remove = %d", code)
	}
	if code, out, _ := runCmd(t, srv, "", runOrgTeamList, "acme"); code != exitOK || !strings.Contains(out, "devs") {
		t.Fatalf("team list = %d %q", code, out)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgTeamCreate, "acme", "--slug", "devs", "--name", "Devs"); code != exitOK {
		t.Fatalf("team create = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"slug": "devs", "name": "Devs", "description": ""})
}

func TestOrgTeamAddRepoNotEnabled(t *testing.T) {
	_, srv := newFakeAPI(t) // every route 404s
	code, _, errOut := runCmd(t, srv, "tok", runOrgTeamAddRepo, "acme", "devs", "acme/mono")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(errOut, "feature not enabled") {
		t.Fatalf("message = %q", errOut)
	}
}

// --- issues ---------------------------------------------------------------

func TestIssueCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/issues", 200, `{"issues":[{"num":7,"title":"Bug","state":"open","author":"alice","labels":["bug"],"assignees":[],"updated_at":"2026-01-01T00:00:00Z"}],"more":false}`)
	f.on("GET", "/acme/mono/api/issues/7", 200, `{"thread":{"num":7,"title":"Bug","state":"open","author":"alice","labels":["bug"],"assignees":[],"updated_at":"2026-01-01T00:00:00Z"},"events":[{"seq":1,"type":"opened","actor":"alice","at":"2026-01-01T00:00:00Z","body":"hello"}],"events_more":false}`)
	f.on("POST", "/acme/mono/api/issues", 201, `{"thread":{"num":9,"title":"New","state":"open","author":"alice"},"events":[]}`)
	f.on("PATCH", "/acme/mono/api/issues/9", 200, `{"thread":{"num":9,"title":"New","state":"open","labels":["bug"],"assignees":["bob"],"milestone":"3"}}`)
	f.on("POST", "/acme/mono/api/issues/7/comments", 201, `{"event":{"seq":2}}`)
	f.on("PATCH", "/acme/mono/api/issues/7", 200, `{"thread":{"num":7,"state":"closed"}}`)

	code, out, _ := runCmd(t, srv, "", runIssueList, "acme/mono", "--state", "open",
		"--label", "bug", "--label", "ui", "--assignee", "bob", "--milestone", "3", "--limit", "10")
	if code != exitOK || !strings.Contains(out, "Bug") {
		t.Fatalf("issue list = %d %q", code, out)
	}
	rec := f.last()
	q := rec.Query
	if q.Get("state") != "open" || q.Get("labels") != "bug,ui" || q.Get("assignee") != "bob" || q.Get("milestone") != "3" || q.Get("n") != "10" {
		t.Fatalf("issue list query = %v", q)
	}

	if code, out, _ := runCmd(t, srv, "", runIssueShow, "acme/mono", "7"); code != exitOK || !strings.Contains(out, "hello") {
		t.Fatalf("issue show = %d %q", code, out)
	}

	code, out, _ = runCmd(t, srv, "tok", runIssueCreate, "acme/mono", "--title", "New",
		"--body", "text", "--label", "bug", "--assignee", "bob", "--milestone", "3")
	if code != exitOK || !strings.Contains(out, "#9") {
		t.Fatalf("issue create = %d %q", code, out)
	}
	// ONE request: the metadata rides the create body (02_issues.md §7), so
	// a rejected label cannot leave an issue behind.
	rec = f.last()
	if rec.Method != "POST" || rec.Path != "/acme/mono/api/issues" {
		t.Fatalf("create request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{
		"title": "New", "body": "text",
		"labels": []any{"bug"}, "assignees": []any{"bob"}, "milestone": "3",
	})

	if code, _, _ := runCmd(t, srv, "tok", runIssueComment, "acme/mono", "7", "--body", "hi"); code != exitOK {
		t.Fatalf("comment = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"body": "hi"})

	if code, _, _ := runCmd(t, srv, "tok", runIssueClose, "acme/mono", "7", "--reason", "not_planned"); code != exitOK {
		t.Fatalf("close = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"state": "closed", "state_reason": "not_planned"})

	if code, _, _ := runCmd(t, srv, "tok", runIssueReopen, "acme/mono", "7"); code != exitOK {
		t.Fatalf("reopen = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"state": "open"})
}

func TestIssueListJSONMirrorsAPI(t *testing.T) {
	body := `{"issues":[{"num":7,"title":"Bug","state":"open"}],"more":false}`
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/issues", 200, body)
	code, out, _ := runCmd(t, srv, "", runIssueList, "acme/mono", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	shouldEqualJSON(t, got, want)
}

func TestIssueNotFoundExitThree(t *testing.T) {
	_, srv := newFakeAPI(t)
	code, _, errOut := runCmd(t, srv, "", runIssueShow, "acme/mono", "404")
	if code != exitNotFound {
		t.Fatalf("exit = %d, want %d (%s)", code, exitNotFound, errOut)
	}
}

// TestIssueCreateBadLabelLeavesNothingBehind proves create is ONE write: a
// rejected label is a single 400 POST, never a create-then-PATCH that leaves
// an unlabeled issue behind (the audit's issue #3).
func TestIssueCreateBadLabelLeavesNothingBehind(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.onFunc("POST", "/acme/mono/api/issues", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `invalid: unknown label "bug"`, http.StatusBadRequest)
	})
	code, out, errOut := runCmd(t, srv, "tok", runIssueCreate, "acme/mono", "--title", "T", "--label", "bug")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d (%s)", code, exitErr, errOut)
	}
	if !strings.Contains(errOut, "unknown label") {
		t.Fatalf("stderr = %q", errOut)
	}
	if f.count() != 1 {
		t.Fatalf("requests = %d, want exactly the create POST", f.count())
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/acme/mono/api/issues" {
		t.Fatalf("request = %s %s", rec.Method, rec.Path)
	}
	if strings.Contains(out, "Opened") {
		t.Fatalf("reported success after a rejected create: %q", out)
	}
}

// --- pull requests --------------------------------------------------------

func TestPRCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/pulls", 200, `{"pulls":[{"num":1,"title":"Feature","state":"open","author":"alice","base_ref":"main","head_ref":"feat","head_sha":"aaa","draft":false,"merged":false,"updated_at":"2026-01-01T00:00:00Z"}],"more":false}`)
	f.on("GET", "/acme/mono/api/pulls/1", 200, `{"thread":{"num":1,"title":"Feature","state":"open","author":"alice","updated_at":"2026-01-01T00:00:00Z"},"pr":{"num":1,"base":{"ref":"main","sha":"bbb"},"head":{"ref":"feat","sha":"aaa"},"body":"desc"},"head_live_sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","events":[]}`)
	f.on("POST", "/acme/mono/api/pulls", 201, `{"thread":{"num":2,"title":"New","state":"open"},"pr":{"num":2}}`)
	f.on("POST", "/acme/mono/api/pulls/1/merge", 202, `{"task":{"id":"t1","kind":"pull-merge","num":1,"state":"running"}}`)
	f.on("GET", "/acme/mono/api/pulls/1/merge/task", 200, `{"task":{"id":"t1","kind":"pull-merge","num":1,"state":"ok","summary":"merged"}}`)
	f.on("POST", "/acme/mono/api/pulls/1/reviews", 201, `{"review":{"seq":3,"state":"APPROVED"},"threads":[],"summary":{}}`)
	f.on("POST", "/acme/mono/api/pulls/1/comments", 201, `{"event":{"seq":4}}`)
	f.on("GET", "/acme/mono/api/checks/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", 200, `{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"success","total_counts":{"success":1},"statuses":[{"sha":"deadbeef","context":"ci","state":"success","updated_at":"2026-01-01T00:00:00Z"}]}`)

	if code, out, _ := runCmd(t, srv, "", runPRList, "acme/mono", "--state", "open", "--base", "main", "--head", "feat"); code != exitOK || !strings.Contains(out, "Feature") {
		t.Fatalf("pr list = %d %q", code, out)
	}
	if q := f.last().Query; q.Get("state") != "open" || q.Get("base") != "refs/heads/main" || q.Get("head") != "refs/heads/feat" {
		t.Fatalf("pr list query = %v", q)
	}

	if code, out, _ := runCmd(t, srv, "tok", runPRCreate, "acme/mono", "--title", "New", "--base", "main", "--head", "feat", "--body", "hi", "--draft"); code != exitOK || !strings.Contains(out, "#2") {
		t.Fatalf("pr create = %d %q", code, out)
	}
	decodeBody(t, f.last(), map[string]any{"title": "New", "base_ref": "refs/heads/main", "head_ref": "refs/heads/feat", "body": "hi", "draft": true})

	if code, _, _ := runCmd(t, srv, "tok", runPRMerge, "acme/mono", "1", "--method", "squash", "--delete-head"); code != exitOK {
		t.Fatalf("merge = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"strategy": "squash", "delete_head": true})

	if code, out, _ := runCmd(t, srv, "tok", runPRMerge, "acme/mono", "1", "--wait"); code != exitOK || !strings.Contains(out, "Merged #1") {
		t.Fatalf("merge --wait = %d %q", code, out)
	}

	if code, _, _ := runCmd(t, srv, "tok", runPRReview, "acme/mono", "1", "--approve", "--body", "lgtm"); code != exitOK {
		t.Fatalf("review = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{
		"state": "APPROVED", "body": "lgtm",
		"commit_sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})

	if code, _, _ := runCmd(t, srv, "tok", runPRComment, "acme/mono", "1", "--body", "hi"); code != exitOK {
		t.Fatalf("comment = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"body": "hi"})

	if code, out, _ := runCmd(t, srv, "", runPRChecks, "acme/mono", "1"); code != exitOK || !strings.Contains(out, "ci") {
		t.Fatalf("checks = %d %q", code, out)
	}
	if rec := f.last(); rec.Path != "/acme/mono/api/checks/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("checks path = %s", rec.Path)
	}
}

// --- releases -------------------------------------------------------------

func TestReleaseCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/releases", 200, `{"releases":[{"tag":"v1","name":"One","draft":false,"author":"alice","created_at":"2026-01-01T00:00:00Z","assets":[]}],"more":false}`)
	f.on("GET", "/acme/mono/api/releases/v1", 200, `{"tag":"v1","name":"One","body":"notes","draft":false,"author":"alice","created_at":"2026-01-01T00:00:00Z","assets":[]}`)
	f.on("PUT", "/acme/mono/api/releases/v1", 200, `{"tag":"v1","name":"One","body":"notes","draft":true,"prerelease":true}`)
	f.on("POST", "/acme/mono/api/releases/v1/assets/app.bin", 201, `{"name":"app.bin","size":3,"sha256":"`+sha256Hex([]byte("abc"))+`","browser_download_url":"/x"}`)
	f.on("DELETE", "/acme/mono/api/releases/v1", 200, `{"deleted":true}`)

	if code, out, _ := runCmd(t, srv, "", runReleaseList, "acme/mono"); code != exitOK || !strings.Contains(out, "v1") {
		t.Fatalf("release list = %d %q", code, out)
	}
	if code, out, _ := runCmd(t, srv, "", runReleaseShow, "acme/mono", "v1"); code != exitOK || !strings.Contains(out, "notes") {
		t.Fatalf("release show = %d %q", code, out)
	}
	if code, _, _ := runCmd(t, srv, "tok", runReleaseCreate, "acme/mono", "v1", "--name", "One", "--notes", "notes", "--draft", "--prerelease"); code != exitOK {
		t.Fatalf("release create = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"name": "One", "body": "notes", "draft": true, "prerelease": true})

	// No --name: the field is omitted and the server defaults the title to
	// the tag (a create and an update both behave the way gh does).
	if code, _, _ := runCmd(t, srv, "tok", runReleaseCreate, "acme/mono", "v1", "--notes", "notes"); code != exitOK {
		t.Fatalf("release create without --name = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"body": "notes", "draft": false, "prerelease": false})

	// --target + --draft seeds a not-yet-existing tag (WP3.3).
	if code, _, _ := runCmd(t, srv, "tok", runReleaseCreate, "acme/mono", "v1", "--draft", "--target", "refs/heads/main"); code != exitOK {
		t.Fatalf("release create --target = %d", code)
	}
	decodeBody(t, f.last(), map[string]any{"body": "", "draft": true, "prerelease": false, "target": "refs/heads/main"})
	if code, _, errOut := runCmd(t, srv, "tok", runReleaseCreate, "acme/mono", "v1", "--target", "main"); code != exitArg {
		t.Fatalf("--target without --draft = %d (%s)", code, errOut)
	}

	dir := t.TempDir()
	asset := filepath.Join(dir, "app.bin")
	if err := os.WriteFile(asset, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCmd(t, srv, "tok", runReleaseUpload, "acme/mono", "v1", asset); code != exitOK || !strings.Contains(out, "app.bin") {
		t.Fatalf("upload = %d %q", code, out)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/acme/mono/api/releases/v1/assets/app.bin" {
		t.Fatalf("upload request = %s %s", rec.Method, rec.Path)
	}
	if got := rec.Header.Get("X-Walgit-Asset-Sha256"); got != sha256Hex([]byte("abc")) {
		t.Fatalf("asset sha header = %q", got)
	}
	if string(rec.Body) != "abc" {
		t.Fatalf("upload body = %q", rec.Body)
	}
	if code, _, _ := runCmd(t, srv, "tok", runReleaseDelete, "acme/mono", "v1"); code != exitOK {
		t.Fatalf("release delete = %d", code)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- checks ---------------------------------------------------------------

func TestChecksCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/checks", 200, `{"checks":[{"sha":"abc123","state":"success","contexts":[],"updated_at":"2026-01-01T00:00:00Z"}],"more":false}`)
	f.on("GET", "/acme/mono/api/resolve/main", 200, `{"ref":"main","sha":"abc123def456","path":"","kind":"branch"}`)
	f.on("GET", "/acme/mono/api/checks/abc123def456", 200, `{"sha":"abc123def456","state":"failure","total_counts":{"failure":1},"statuses":[{"context":"ci","state":"failure","updated_at":"2026-01-01T00:00:00Z"}]}`)

	if code, out, _ := runCmd(t, srv, "", runChecksList, "acme/mono", "--limit", "5"); code != exitOK || !strings.Contains(out, "success") {
		t.Fatalf("checks list = %d %q", code, out)
	}
	if q := f.last().Query; q.Get("n") != "5" {
		t.Fatalf("checks list query = %v", q)
	}
	if code, out, _ := runCmd(t, srv, "", runChecksStatus, "acme/mono", "main"); code != exitOK || !strings.Contains(out, "failure") {
		t.Fatalf("checks status = %d %q", code, out)
	}
	if rec := f.last(); rec.Path != "/acme/mono/api/checks/abc123def456" {
		t.Fatalf("checks status path = %s", rec.Path)
	}
}

// --- packages (served, docs/features/14 §7) --------------------------------

func TestPackagesCommands(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/packages", 200, `{"repository":"acme/mono","packages":[{"tag":"v1.0","digest":"sha256:`+strings.Repeat("a", 64)+`","size":12,"platforms":["linux/amd64"],"updated_at":"2026-11-21T12:00:00Z"}],"manifests":[],"total_size":12,"more":false}`)
	f.on("GET", "/acme/mono/api/packages/sha256:abc", 200, `{"digest":"sha256:abc"}`)
	f.on("DELETE", "/acme/mono/api/packages/tags/v1.0", 204, "")
	f.on("DELETE", "/acme/mono/api/packages/sha256:abc", 204, "")

	code, out, errOut := runCmd(t, srv, "tok", runPackagesList, "acme/mono")
	if code != exitOK {
		t.Fatalf("list = %d (%s)", code, errOut)
	}
	for _, want := range []string{"TAG", "v1.0", "linux/amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q: %q", want, out)
		}
	}
	if rec := f.last(); rec.Path != "/acme/mono/api/packages" || rec.Method != "GET" {
		t.Fatalf("list request = %s %s", rec.Method, rec.Path)
	}

	if code, out, errOut := runCmd(t, srv, "tok", runPackagesShow, "acme/mono", "sha256:abc"); code != exitOK || !strings.Contains(out, "sha256:abc") {
		t.Fatalf("show = %d %q %q", code, out, errOut)
	}

	// --tag deletes a tag pointer without needing a digest positional.
	if code, out, _ := runCmd(t, srv, "tok", runPackagesDelete, "acme/mono", "--tag", "v1.0"); code != exitOK || !strings.Contains(out, "Deleted tag v1.0") {
		t.Fatalf("delete tag = %d %q", code, out)
	}
	if rec := f.last(); rec.Method != "DELETE" || rec.Path != "/acme/mono/api/packages/tags/v1.0" {
		t.Fatalf("delete tag request = %s %s", rec.Method, rec.Path)
	}
	if code, _, _ := runCmd(t, srv, "tok", runPackagesDelete, "acme/mono", "sha256:abc"); code != exitOK {
		t.Fatalf("delete digest = %d", code)
	}
	if rec := f.last(); rec.Path != "/acme/mono/api/packages/sha256:abc" {
		t.Fatalf("delete digest path = %s", rec.Path)
	}
	// A real 404 is a not-found (exit 3), never "feature not enabled".
	code, _, errOut = runCmd(t, srv, "tok", runPackagesList, "acme/ghost")
	if code != exitNotFound || strings.Contains(errOut, "feature not enabled") {
		t.Fatalf("missing repo = %d %q", code, errOut)
	}
	// Usage edges: --tag plus a digest is refused.
	c := isolateCLI(t, srv.URL)
	c.out, c.errOut = &bytes.Buffer{}, &bytes.Buffer{}
	if code := runPackagesDelete(t.Context(), c, []string{"acme/mono", "--tag", "v1", "sha256:abc"}); code != exitArg {
		t.Fatalf("tag+digest = %d", code)
	}
}

// TestPackagesDeleteTargetForms pins the (--tag T | DIGEST) contract: exactly
// one target, resolved from two positionals (tag) or three (digest).
func TestPackagesDeleteTargetForms(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("DELETE", "/acme/mono/api/packages/tags/v1", 200, `{}`)
	f.on("DELETE", "/acme/mono/api/packages/sha256:abc", 200, `{}`)

	if code, out, _ := runCmd(t, srv, "tok", runPackagesDelete, "acme/mono", "--tag", "v1"); code != exitOK || !strings.Contains(out, "tag v1") {
		t.Fatalf("--tag form = %d %q", code, out)
	}
	if rec := f.last(); rec.Method != "DELETE" || rec.Path != "/acme/mono/api/packages/tags/v1" {
		t.Fatalf("--tag request = %s %s", rec.Method, rec.Path)
	}
	if code, out, _ := runCmd(t, srv, "tok", runPackagesDelete, "acme/mono", "sha256:abc"); code != exitOK || !strings.Contains(out, "sha256:abc") {
		t.Fatalf("digest form = %d %q", code, out)
	}
	if rec := f.last(); rec.Method != "DELETE" || rec.Path != "/acme/mono/api/packages/sha256:abc" {
		t.Fatalf("digest request = %s %s", rec.Method, rec.Path)
	}
	for _, args := range [][]string{{"acme/mono"}, {"acme/mono", "--tag", "v1", "sha256:abc"}} {
		if code, _, errOut := runCmd(t, srv, "tok", runPackagesDelete, args...); code != exitArg {
			t.Fatalf("%v: exit = %d, want %d (%s)", args, code, exitArg, errOut)
		}
	}
}

// --- threads (served: GET/POST /{o}/{r}/api/threads) ----------------------

func TestThreadsCommandsHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/threads", 200, `{"runs":[{"id":"r1","workflow":"ci","event":"push","state":"queued","commit":"abcdef1234567890"}],"more":false}`)
	f.on("GET", "/acme/mono/api/threads/r1", 200, `{"run":{"id":"r1","workflow":"ci","state":"queued"},"jobs":[]}`)
	f.on("POST", "/acme/mono/api/threads", 201, `{"runs":[{"id":"r2","workflow":"ci.yml","state":"queued"}]}`)

	if code, out, _ := runCmd(t, srv, "", runThreadsList, "acme/mono"); code != exitOK || !strings.Contains(out, "r1") {
		t.Fatalf("threads list = %d %q", code, out)
	}
	if code, out, _ := runCmd(t, srv, "", runThreadsShow, "acme/mono", "r1"); code != exitOK || !strings.Contains(out, `"id": "r1"`) {
		t.Fatalf("threads show = %d %q", code, out)
	}
	// Dispatch is POST …/api/threads with {ref, workflow} — the served route.
	if code, out, _ := runCmd(t, srv, "tok", runThreadsRun, "acme/mono", "ci.yml", "--ref", "refs/heads/main"); code != exitOK || !strings.Contains(out, "r2") {
		t.Fatalf("threads run = %d %q", code, out)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/acme/mono/api/threads" {
		t.Fatalf("dispatch request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{"ref": "refs/heads/main", "workflow": "ci.yml"})
	if code, _, errOut := runCmd(t, srv, "tok", runThreadsRun, "acme/mono", "ci.yml"); code != exitArg || !strings.Contains(errOut, "--ref") {
		t.Fatalf("dispatch without --ref = %d %q", code, errOut)
	}
}

func TestThreadsShowUnknownRunIsNotFound(t *testing.T) {
	_, srv := newFakeAPI(t) // every route 404s
	code, _, errOut := runCmd(t, srv, "", runThreadsShow, "acme/mono", "r404")
	if code != exitNotFound {
		t.Fatalf("exit = %d, want %d (%s)", code, exitNotFound, errOut)
	}
	if strings.Contains(errOut, "feature not enabled") {
		t.Fatalf("unknown run misreported: %q", errOut)
	}
}

func TestThreadsCancelExplainsTheT2Gap(t *testing.T) {
	f, srv := newFakeAPI(t)
	code, _, errOut := runCmd(t, srv, "", runThreadsCancel, "acme/mono", "r1")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(errOut, "T2 runner protocol") {
		t.Fatalf("message = %q", errOut)
	}
	if f.count() != 0 {
		t.Fatalf("cancel called a route (%d requests)", f.count())
	}
}

// --- auth -----------------------------------------------------------------

func TestAuthLoginStatusLogout(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 200, `{"principal":"alice","write":true,"admin":true,"anonymous":false}`)

	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("wgt_tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := isolateCLI(t, srv.URL)
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := runAuthLogin(t.Context(), c, []string{"--token-file", tokFile}); code != exitOK {
		t.Fatalf("login = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "alice") {
		t.Fatalf("login output = %q", out.String())
	}
	st, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Servers[srv.URL].Token != "wgt_tok" || st.Servers[srv.URL].Principal != "alice" {
		t.Fatalf("state = %+v", st)
	}
	info, _ := os.Stat(statePath())
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}

	// status picks the token up from the state file (no env token).
	out.Reset()
	if code := runAuthStatus(t.Context(), c, []string{"--json"}); code != exitOK {
		t.Fatalf("status = %d (%s)", code, errOut.String())
	}
	var status authStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, out.String())
	}
	if !status.Authenticated || status.Principal != "alice" || status.TokenSource != "state" || !status.Admin {
		t.Fatalf("status = %+v", status)
	}
	if strings.Contains(out.String(), "wgt_tok") {
		t.Fatalf("status leaked the token: %q", out.String())
	}

	out.Reset()
	if code := runAuthLogout(t.Context(), c, nil); code != exitOK {
		t.Fatalf("logout = %d", code)
	}
	if _, ok := loadState(); ok != nil {
		t.Fatal("unexpected")
	}
	st, _ = loadState()
	if len(st.Servers) != 0 {
		t.Fatalf("logout left credentials: %+v", st.Servers)
	}
}

func TestAuthStatusWithoutTokenIsAuthExit(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 401, `authentication required`)
	c := isolateCLI(t, srv.URL)
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := runAuthStatus(t.Context(), c, []string{"--json"}); code != exitAuth {
		t.Fatalf("status = %d, want %d", code, exitAuth)
	}
	var status authStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if status.Authenticated {
		t.Fatal("reported authenticated without a token")
	}
}

func TestAuthRejectedTokenIsAuthExit(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 401, `invalid token`)
	code, _, errOut := runCmd(t, srv, "bad-token", runUserWhoami)
	if code != exitAuth {
		t.Fatalf("whoami = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(errOut, "auth login") {
		t.Fatalf("missing login hint: %q", errOut)
	}
}

func TestAuthTokenCommandsExplainTheGap(t *testing.T) {
	_, srv := newFakeAPI(t)
	for _, sub := range []string{"list", "create", "revoke"} {
		code, _, errOut := runCmd(t, srv, "", runAuthTokenUnsupported("token "+sub))
		if code != exitErr {
			t.Fatalf("token %s = %d", sub, code)
		}
		if !strings.Contains(errOut, "/_auth/tokens") {
			t.Fatalf("token %s: %q", sub, errOut)
		}
	}
}

// --- api escape hatch -----------------------------------------------------

func TestAPICommandSendsMethodBodyAndHeaders(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/api/v1/custom", 200, `{"ok":true}`)
	code, out, _ := runCmd(t, srv, "tok", runAPI, "/api/v1/custom",
		"-X", "POST", "-d", `{"x":1}`, "-H", "X-Extra: yes", "--json")
	if code != exitOK {
		t.Fatalf("api = %d", code)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/api/v1/custom" {
		t.Fatalf("request = %s %s", rec.Method, rec.Path)
	}
	if got := rec.Header.Get("X-Extra"); got != "yes" {
		t.Fatalf("X-Extra = %q", got)
	}
	if got := rec.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if string(rec.Body) != `{"x":1}` {
		t.Fatalf("body = %q", rec.Body)
	}
	if !strings.Contains(out, "\"ok\": true") {
		t.Fatalf("--json output = %q", out)
	}
}

func TestAPICommandDefaultsToPOSTWithData(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/api/v1/x", 200, `{}`)
	if code, _, _ := runCmd(t, srv, "", runAPI, "/api/v1/x", "-d", "{}"); code != exitOK {
		t.Fatalf("api = %d", code)
	}
	if f.last().Method != "POST" {
		t.Fatalf("method = %s", f.last().Method)
	}
}

func TestAPICommandNotFoundMapsToThree(t *testing.T) {
	_, srv := newFakeAPI(t)
	if code, _, _ := runCmd(t, srv, "", runAPI, "/api/v1/missing"); code != exitNotFound {
		t.Fatalf("exit = %d", code)
	}
}

func TestAuthStatusAtAuthNoneServer(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 200, `{"principal":"anon","write":true,"admin":true,"anonymous":false}`)
	code, out, _ := runCmd(t, srv, "", runAuthStatus, "--json")
	if code != exitOK {
		t.Fatalf("status = %d (auth-none needs no token)", code)
	}
	var status authStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Authenticated || status.Principal != "anon" || status.TokenSource != "none" {
		t.Fatalf("status = %+v", status)
	}
}

// --- coverage of the remaining surfaces ----------------------------------

// TestPRDiffAndClose pin the two review-workflow commands: `pr diff` streams
// the text/plain patch verbatim, `pr close` sets state=closed on the pull PATCH.
func TestPRDiffAndClose(t *testing.T) {
	f, srv := newFakeAPI(t)
	const patch = "diff --git a/x b/x\n--- a/x\n+++ b/x\n"
	f.onFunc("GET", "/acme/mono/api/pulls/3/diff", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(patch))
	})
	f.on("PUT", "/acme/mono/api/pulls/3", 200, `{"thread":{"num":3,"state":"closed"},"pr":{"num":3}}`)

	code, out, _ := runCmd(t, srv, "", runPRDiff, "acme/mono", "3")
	if code != exitOK || out != patch {
		t.Fatalf("pr diff = %d %q", code, out)
	}
	if rec := f.last(); rec.Method != "GET" || rec.Path != "/acme/mono/api/pulls/3/diff" {
		t.Fatalf("diff request = %s %s", rec.Method, rec.Path)
	}

	code, out, _ = runCmd(t, srv, "tok", runPRClose, "acme/mono", "3")
	if code != exitOK || !strings.Contains(out, "#3 closed") || !strings.Contains(out, "/acme/mono/pulls/3") {
		t.Fatalf("pr close = %d %q", code, out)
	}
	rec := f.last()
	if rec.Method != "PUT" || rec.Path != "/acme/mono/api/pulls/3" {
		t.Fatalf("close request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{"state": "closed"})
}

// TestPRListStateAllOmitsTheFilter: `--state all` is gh's spelling for the
// API's default (no state parameter), not a 400.
func TestPRListStateAllOmitsTheFilter(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/pulls", 200, `{"pulls":[],"more":false}`)
	if code, _, _ := runCmd(t, srv, "", runPRList, "acme/mono", "--state", "all"); code != exitOK {
		t.Fatalf("pr list --state all = %d", code)
	}
	if _, ok := f.last().Query["state"]; ok {
		t.Fatalf("state filter sent for --state all: %v", f.last().Query)
	}
}

// TestChecksStatusNormalizesFullRef: refs/heads/main resolves like `main`
// (every other command normalizes the same way).
func TestChecksStatusNormalizesFullRef(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/resolve/main", 200, `{"ref":"main","sha":"abc123def456","kind":"branch"}`)
	f.on("GET", "/acme/mono/api/checks/abc123def456", 200, `{"sha":"abc123def456","state":"success","total_counts":{},"statuses":[]}`)
	if code, _, errOut := runCmd(t, srv, "", runChecksStatus, "acme/mono", "refs/heads/main"); code != exitOK {
		t.Fatalf("checks status refs/heads/main = %d (%s)", code, errOut)
	}
	if rec := f.first(); rec.Path != "/acme/mono/api/resolve/main" {
		t.Fatalf("resolve path = %s", rec.Path)
	}
}

func TestPRShowHumanAndDraft(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/pulls/1", 200, `{"thread":{"num":1,"title":"Feature","state":"open","author":"alice","updated_at":"2026-01-01T00:00:00Z"},
		"pr":{"num":1,"base":{"ref":"refs/heads/main","sha":"bbb"},"head":{"ref":"refs/heads/feat","sha":"aaa"},"body":"desc","draft":true},
		"head_live_sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","events":[{"seq":1,"type":"opened","actor":"alice","at":"2026-01-01T00:00:00Z","body":"hi"}]}`)
	code, out, _ := runCmd(t, srv, "", runPRShow, "acme/mono", "1")
	if code != exitOK {
		t.Fatalf("pr show = %d", code)
	}
	for _, needle := range []string{"draft", "main←feat", "desc", "alice"} {
		if !strings.Contains(out, needle) {
			t.Errorf("pr show output misses %q: %q", needle, out)
		}
	}
}

func TestCompletionCommand(t *testing.T) {
	_, srv := newFakeAPI(t)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		code, out, _ := runCmd(t, srv, "", runCompletion, shell)
		if code != exitOK || !strings.Contains(out, "spindle") {
			t.Fatalf("completion %s = %d %q", shell, code, out)
		}
	}
	if code, _, _ := runCmd(t, srv, "", runCompletion, "powershell"); code != exitArg {
		t.Fatalf("unknown shell exit = %d, want %d", code, exitArg)
	}
}

func TestAPICommandAbsoluteURL(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/me", 200, `{"principal":"alice"}`)
	code, out, _ := runCmd(t, srv, "tok", runAPI, srv.URL+"/api/v1/me")
	if code != exitOK || !strings.Contains(out, "alice") {
		t.Fatalf("absolute URL api = %d %q", code, out)
	}
	if got := f.last().Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestAPICommandRejectsBadHeader(t *testing.T) {
	_, srv := newFakeAPI(t)
	if code, _, _ := runCmd(t, srv, "", runAPI, "/api/v1/me", "-H", "no-colon"); code != exitArg {
		t.Fatalf("bad -H exit = %d, want %d", code, exitArg)
	}
}

func TestAuthLoginMissingTokenFile(t *testing.T) {
	_, srv := newFakeAPI(t)
	c := isolateCLI(t, srv.URL)
	c.out, c.errOut = &bytes.Buffer{}, &bytes.Buffer{}
	if code := runAuthLogin(t.Context(), c, []string{"--token-file", filepath.Join(t.TempDir(), "missing")}); code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
}

func TestReleaseUploadMissingFile(t *testing.T) {
	_, srv := newFakeAPI(t)
	code, _, errOut := runCmd(t, srv, "tok", runReleaseUpload, "acme/mono", "v1", filepath.Join(t.TempDir(), "nope.bin"))
	if code != exitErr || !strings.Contains(errOut, "no such file") {
		t.Fatalf("upload missing file = %d %q", code, errOut)
	}
}

// TestOrgWebhookCommands pins the WP6.2 org webhook CLI: list/create/
// delete/ping/deliveries/redeliver against the served route shapes, the
// --json passthrough, and the exit-code mapping (404 → 3, 410 → 1).
func TestOrgWebhookCommands(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/orgs/acme/webhooks", 200,
		`{"webhooks":[{"id":"h1","url":"https://h.example/x","events":["commented"],"active":true,"secret_set":true}]}`)
	f.on("POST", "/api/v1/orgs/acme/webhooks", 201,
		`{"id":"h1","url":"https://h.example/x","events":["commented"],"active":true,"secret_set":true}`)
	f.on("DELETE", "/api/v1/orgs/acme/webhooks/h1", 204, ``)
	f.on("POST", "/api/v1/orgs/acme/webhooks/h1/ping", 200, `{"delivery":true}`)
	f.on("GET", "/api/v1/orgs/acme/webhooks/h1/deliveries", 200,
		`{"updated_at":"2026-01-01T00:00:00Z","entries":[{"seq":3,"event":"ping","status":200,"at":"2026-01-01T00:00:00Z","duration_ms":4}]}`)
	f.on("POST", "/api/v1/orgs/acme/webhooks/h1/deliveries/3/redeliver", 200,
		`{"delivery":true,"entry":{"seq":3,"event":"ping","status":200,"at":"2026-01-01T00:00:01Z","duration_ms":2}}`)

	if code, out, _ := runCmd(t, srv, "tok", runOrgWebhookList, "acme"); code != exitOK || !strings.Contains(out, "h1") || !strings.Contains(out, "active") {
		t.Fatalf("webhook list = %d %q", code, out)
	}
	if code, out, _ := runCmd(t, srv, "tok", runOrgWebhookList, "acme", "--json"); code != exitOK || !strings.Contains(out, `"webhooks"`) {
		t.Fatalf("webhook list --json = %d %q", code, out)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookCreate, "acme",
		"--url", "https://h.example/x", "--events", "commented,opened", "--secret", "s", "--inactive"); code != exitOK {
		t.Fatalf("webhook create = %d", code)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/api/v1/orgs/acme/webhooks" {
		t.Fatalf("create request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{
		"url": "https://h.example/x", "events": []any{"commented", "opened"},
		"secret": "s", "active": false, "insecure_tls": false,
	})
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookPing, "acme", "h1"); code != exitOK {
		t.Fatalf("webhook ping = %d", code)
	}
	if code, out, _ := runCmd(t, srv, "tok", runOrgWebhookDeliveries, "acme", "h1"); code != exitOK || !strings.Contains(out, "ping") || !strings.Contains(out, "4ms") {
		t.Fatalf("webhook deliveries = %d %q", code, out)
	}
	if code, out, _ := runCmd(t, srv, "tok", runOrgWebhookRedeliver, "acme", "h1", "3"); code != exitOK || !strings.Contains(out, "Redelivered #3 → 200") {
		t.Fatalf("webhook redeliver = %d %q", code, out)
	}
	if rec := f.last(); rec.Method != "POST" || rec.Path != "/api/v1/orgs/acme/webhooks/h1/deliveries/3/redeliver" {
		t.Fatalf("redeliver request = %s %s", rec.Method, rec.Path)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookDelete, "acme", "h1"); code != exitOK {
		t.Fatalf("webhook delete = %d", code)
	}
	if rec := f.last(); rec.Method != "DELETE" || rec.Path != "/api/v1/orgs/acme/webhooks/h1" {
		t.Fatalf("delete request = %s %s", rec.Method, rec.Path)
	}

	// Bad positionals: usage errors (exitArg), no request.
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookCreate, "acme"); code != exitArg {
		t.Fatalf("create without --url = %d", code)
	}
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookRedeliver, "acme", "h1", "x"); code != exitArg {
		t.Fatalf("redeliver bad seq = %d", code)
	}
}

// TestOrgWebhookExitCodes pins the standard mapping against real statuses.
func TestOrgWebhookExitCodes(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/api/v1/orgs/acme/webhooks/nope/ping", 404, `not found`)
	f.on("POST", "/api/v1/orgs/acme/webhooks/h1/deliveries/9/redeliver", 410, `gone: payload not retained`)
	if code, _, _ := runCmd(t, srv, "tok", runOrgWebhookPing, "acme", "nope"); code != exitNotFound {
		t.Fatalf("ping 404 = %d, want %d", code, exitNotFound)
	}
	if code, _, errOut := runCmd(t, srv, "tok", runOrgWebhookRedeliver, "acme", "h1", "9"); code != exitErr || !strings.Contains(errOut, "410") {
		t.Fatalf("redeliver 410 = %d (%q)", code, errOut)
	}
}
