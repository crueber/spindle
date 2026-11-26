package threads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// TestEventBuildersCoverTheDocumentedFields exercises every payload builder and
// the projection, including the scalar/list arms.
func TestEventBuildersCoverTheDocumentedFields(t *testing.T) {
	payloads := []map[string]any{
		PushEventPayload("o/r", "refs/heads/main", strings.Repeat("a", 40), strings.Repeat("b", 40), "ada"),
		PullEventPayload("o/r", "opened", 7, "title", "open", "ada", "refs/heads/main", strings.Repeat("a", 40),
			"refs/heads/topic", strings.Repeat("b", 40), "o/r", false, false, false, "ada"),
		ReleaseEventPayload("o/r", "published", "v1", "One", strings.Repeat("a", 40), "ada", false, true,
			[]ReleaseAsset{{Name: "z.tgz", Size: 3}, {Name: "a.tgz", Size: 1}}),
		IssueEventPayload("o/r", "opened", "ada", 5, "title", "open", []string{"bug"}),
		IssueCommentEventPayload("o/r", "created", "ada", "body", 5, "title", "open", []string{"bug"}, "bob"),
	}
	for _, p := range payloads {
		fields := EventFieldsOf(p)
		if len(fields) == 0 {
			t.Fatalf("no fields projected from %+v", p)
		}
		if _, err := EncodeEventPayload(p); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	// A list payload projects a sorted, comma-joined name column; a decoded
	// (JSON-round-tripped) list uses the generic []any form.
	rel := ReleaseEventPayload("o/r", "published", "v1", "One", "", "ada", false, false,
		[]ReleaseAsset{{Name: "z", Size: 1}, {Name: "a", Size: 1}})
	if got := EventFieldsOf(rel)["event.release.assets"]; got != "a,z" {
		t.Fatalf("assets = %q", got)
	}
	round, _ := json.Marshal(rel)
	var decoded map[string]any
	_ = json.Unmarshal(round, &decoded)
	if got := EventFieldsOf(decoded)["event.release.assets"]; got != "a,z" {
		t.Fatalf("decoded assets = %q", got)
	}
	if v, ok := eventScalar(float64(2.5)); !ok || v != "2.5" {
		t.Fatalf("float scalar = %q", v)
	}
	if v, ok := eventScalar(int64(3)); !ok || v != "3" {
		t.Fatalf("int64 scalar = %q", v)
	}
	if _, ok := eventScalar(struct{}{}); ok {
		t.Fatal("struct projected as a scalar")
	}
	if repoNamePart("noslash") != "noslash" || ownerPart("noslash") != "" {
		t.Fatal("repo split")
	}
	if !zeroOID("") || !zeroOID(strings.Repeat("0", 40)) || zeroOID(strings.Repeat("a", 40)) {
		t.Fatal("zeroOID")
	}
}

// TestVariablesHTTPAndOrgArms covers the variable HTTP surface, the delete
// paths, the org gate and the environment picker's service read.
func TestVariablesHTTPAndOrgArms(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "root", Admin: true}
	k, _ := NewKeyring(strings.Repeat("q", 32), "")
	e.svc.SetKeyring(k)

	// Repository variable CRUD.
	rec := doAuth(h, http.MethodPut, "/o/r/api/threads/variables/REGION", `{"value":"eu"}`, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put variable = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodGet, "/o/r/api/threads/variables", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list variables = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodDelete, "/o/r/api/threads/variables/REGION", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete variable = %d", rec.Code)
	}
	// Organization variable CRUD (host admin passes the org gate).
	rec = doAuth(h, http.MethodPut, "/api/v1/orgs/acme/threads/variables/REGION", `{"value":"eu-org"}`, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put org variable = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodGet, "/api/v1/orgs/acme/threads/variables", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list org variables = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodDelete, "/api/v1/orgs/acme/threads/variables/REGION", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete org variable = %d", rec.Code)
	}
	// The org gate: a non-admin with no checker wired is a named 503; with a
	// checker that refuses it is a 403.
	*cur = auth.Principal{Name: "ada"}
	if err := e.svc.SetOrgVariable(ctx(), "acme", "X", "y", auth.Principal{Name: "ada"}); err == nil {
		t.Fatal("org variable without a checker accepted")
	}
	e.svc.OrgAdmin = func(_ context.Context, _ string, _ auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "org owner required"}
	}
	if err := e.svc.SetOrgVariable(ctx(), "acme", "X", "y", auth.Principal{Name: "ada"}); err == nil {
		t.Fatal("refused org variable accepted")
	}
	e.svc.OrgAdmin = func(_ context.Context, _ string, _ auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "identity down"}
	}
	if err := e.svc.SetOrgVariable(ctx(), "acme", "X", "y", auth.Principal{Name: "ada"}); err == nil {
		t.Fatal("unavailable org gate accepted")
	}

	// The environment picker's read needs only the repository read gate.
	admin := auth.Principal{Name: "root", Admin: true}
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "prod", EnvironmentInput{}, admin); err != nil {
		t.Fatalf("PutEnvironment: %v", err)
	}
	names, err := e.svc.EnvironmentNames(ctx(), "o/r", admin)
	if err != nil || len(names) != 1 || names[0] != "prod" {
		t.Fatalf("EnvironmentNames = %v, %v", names, err)
	}
	// A path-filtered trigger with no DiffReader fails closed with a name.
	sha := strings.Repeat("a", 40)
	e.git.add(sha, ".spindle/workflows/paths.yml", `name: p
on:
  push:
    paths: ["src/**"]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`)
	e.svc.Git = struct{ GitReader }{e.svc.Git}
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{
		Branch: "main", RefName: "main", BaseSHA: strings.Repeat("b", 40),
	}); err == nil || !strings.Contains(err.Error(), "path filters") {
		t.Fatalf("no-diff-reader = %v", err)
	}
}

// TestLookupVarAndContainerPolicyArms covers two small helpers the new
// contexts reach.
func TestLookupVarAndContainerPolicyArms(t *testing.T) {
	if got := lookupVar(map[string]string{"REGION": "eu"}, "region"); got != "eu" {
		t.Fatalf("lookupVar = %q", got)
	}
	if got := lookupVar(nil, "x"); got != "" {
		t.Fatalf("nil lookupVar = %q", got)
	}
	if (ContainerOptionRefusal{Reason: "x"}).String() != "x" {
		t.Fatal("ContainerOptionRefusal.String")
	}
}

// TestDiffNamesAgainstRealGit pins the production path-filter reader: a
// two-commit diff, a root/new-ref diff and a merge-base diff.
func TestDiffNamesAgainstRealGit(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "root")
	sha1 := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "a")
	sha2 := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "b")
	sha3 := git("rev-parse", "HEAD")

	g := NewSubprocessGit("git")
	g.SetDirs(fixedDirs(filepath.Join(dir, ".git")))
	paths, err := g.DiffNames(context.Background(), "o/r", sha2, sha3, false)
	if err != nil || len(paths) != 1 || paths[0] != "b.txt" {
		t.Fatalf("push diff = %v, %v", paths, err)
	}
	paths, err = g.DiffNames(context.Background(), "o/r", "", sha2, false)
	if err != nil || len(paths) != 1 || paths[0] != "a.txt" {
		t.Fatalf("new-ref diff = %v, %v", paths, err)
	}
	if _, err := g.DiffNames(context.Background(), "o/r", sha1, sha3, false); err != nil {
		t.Fatalf("root diff: %v", err)
	}
	if _, err := g.DiffNames(context.Background(), "o/r", sha1, sha3, true); err != nil {
		t.Fatalf("merge-base diff: %v", err)
	}
}

// fixedDirs resolves every repository to one directory (the SubprocessGit test
// adapter).
type fixedDirs string

func (d fixedDirs) Dir(context.Context, string) (string, error) { return string(d), nil }

// TestVariablesHandlerBranches drives the variable handler directly (the route
// test only proves the path is claimed) and the org handler's method refusals.
func TestVariablesHandlerBranches(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "root", Admin: true}
	k, _ := NewKeyring(strings.Repeat("z", 32), "")
	e.svc.SetKeyring(k)
	req := httptest.NewRequest(http.MethodGet, "/o/r/api/threads/variables", nil)
	h.variables(httptest.NewRecorder(), req, "o/r", nil)
	req = httptest.NewRequest(http.MethodPost, "/o/r/api/threads/variables", nil)
	rec := httptest.NewRecorder()
	h.variables(rec, req, "o/r", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/o/r/api/threads/variables/X", nil)
	rec = httptest.NewRecorder()
	h.variables(rec, req, "o/r", []string{"X"})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("extra segment = %d", rec.Code)
	}
	// The named PUT/DELETE branches, direct.
	req = httptest.NewRequest(http.MethodPut, "/o/r/api/threads/variables/X", strings.NewReader(`{"value":"v"}`))
	h.variables(httptest.NewRecorder(), req, "o/r", []string{"X"})
	req = httptest.NewRequest(http.MethodDelete, "/o/r/api/threads/variables/X", nil)
	h.variables(httptest.NewRecorder(), req, "o/r", []string{"X"})
	// The org lane's method refusal.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orgs/acme/threads/variables", nil)
	rec = httptest.NewRecorder()
	h.handleOrgVariables(rec, req, "acme", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("org method = %d", rec.Code)
	}
}

// TestEventListTruncation pins the 100-entry caps on the issue labels and
// release assets.
func TestEventListTruncation(t *testing.T) {
	labels := make([]string, 0, 120)
	assets := make([]ReleaseAsset, 0, 120)
	for i := 0; i < 120; i++ {
		labels = append(labels, "l")
		assets = append(assets, ReleaseAsset{Name: "a"})
	}
	issue := IssueEventPayload("o/r", "opened", "ada", 1, "t", "open", labels)
	comment := IssueCommentEventPayload("o/r", "created", "ada", "b", 1, "t", "open", labels, "bob")
	rel := ReleaseEventPayload("o/r", "published", "v1", "n", "", "ada", false, false, assets)
	if n := len(issue["issue"].(map[string]any)["labels"].([]string)); n != maxEventListEntries {
		t.Fatalf("labels = %d", n)
	}
	if n := len(comment["issue"].(map[string]any)["labels"].([]string)); n != maxEventListEntries {
		t.Fatalf("comment labels = %d", n)
	}
	if n := len(rel["release"].(map[string]any)["assets"].([]map[string]any)); n != maxEventListEntries {
		t.Fatalf("assets = %d", n)
	}
}

// TestVariableErrorArms covers the validation, authorization and corruption
// branches of the variable store.
func TestVariableErrorArms(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("m", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}
	reader := auth.Principal{Name: "ada"}

	if err := e.svc.SetVariable(ctx(), "bad", "X", "v", admin); err == nil {
		t.Fatal("bad repo accepted")
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "X", "v", reader); err == nil {
		t.Fatal("non-admin accepted")
	}
	if _, err := e.svc.ListVariables(ctx(), "bad", admin); err == nil {
		t.Fatal("bad repo listed")
	}
	if err := e.svc.DeleteVariable(ctx(), "bad", "X", admin); err == nil {
		t.Fatal("bad repo deleted")
	}
	// An absent delete is a no-op; a present one removes.
	if err := e.svc.DeleteVariable(ctx(), "o/r", "MISSING", admin); err != nil {
		t.Fatalf("absent delete: %v", err)
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "X", "v", admin); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteVariable(ctx(), "o/r", "X", admin); err != nil {
		t.Fatal(err)
	}
	// Corrupt documents fail closed.
	raw := []byte(`{"values":{"X":{"name":"X","sealed":"not-base64!"}}}`)
	if _, err := store.PutBytes(ctx(), e.store, VarsKey("o", "r"), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.variableValues(ctx(), "o", "r"); err == nil {
		t.Fatal("corrupt variable envelope accepted")
	}
	if _, err := store.PutBytes(ctx(), e.store, VarsKey("o", "r"), []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListVariables(ctx(), "o/r", admin); err == nil {
		t.Fatal("corrupt variable doc listed")
	}
	if _, err := e.svc.ListOrgVariables(ctx(), "acme", admin); err != nil {
		t.Fatalf("absent org doc: %v", err)
	}
	if err := e.svc.DeleteOrgVariable(ctx(), "acme", "MISSING", admin); err != nil {
		t.Fatalf("absent org delete: %v", err)
	}
	// HTTP: an unauthenticated read is a real 401.
	_, h, cur := newHTTP(t)
	*cur = auth.Anonymous()
	rec := doAuth(h, http.MethodGet, "/o/r/api/threads/variables", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
}

// TestVariableHTTPErrorArms covers the handlers' refusal and error branches.
func TestVariableHTTPErrorArms(t *testing.T) {
	e, h, cur := newHTTP(t)
	k, _ := NewKeyring(strings.Repeat("n", 32), "")
	e.svc.SetKeyring(k)
	*cur = auth.Anonymous()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/o/r/api/threads/variables/X"},
		{http.MethodDelete, "/o/r/api/threads/variables/X"},
		{http.MethodPut, "/api/v1/orgs/acme/threads/variables/X"},
		{http.MethodDelete, "/api/v1/orgs/acme/threads/variables/X"},
	} {
		rec := doAuth(h, tc.method, tc.path, `{"value":"x"}`, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	// An authenticated admin with an invalid variable name is a 400-class
	// validation error, not a panic.
	*cur = auth.Principal{Name: "root", Admin: true}
	rec := doAuth(h, http.MethodPut, "/o/r/api/threads/variables/GITHUB_TOKEN", `{"value":"x"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reserved name = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodPut, "/o/r/api/threads/variables/X", `{"value":""}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty value = %d", rec.Code)
	}
	// The org lane's list with no checker wired for a non-admin is unavailable.
	*cur = auth.Principal{Name: "ada"}
	rec = doAuth(h, http.MethodGet, "/api/v1/orgs/acme/threads/variables", "", "")
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusForbidden {
		t.Errorf("org list refusal = %d", rec.Code)
	}
}

// TestCreateRunNewErrorArms covers the creation-time refusal branches the new
// triggers introduced.
func TestCreateRunNewErrorArms(t *testing.T) {
	e := newTestEnv(t)
	sha := strings.Repeat("a", 40)
	e.git.refs["HEAD"] = sha
	e.git.add(sha, ".spindle/workflows/ci.yml", `name: CI
on: [push, issue_comment, release, pull_request]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`)
	// An over-cap payload is refused before any run exists.
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{
		Branch: "main", RefName: "main", EventPayload: map[string]any{"big": strings.Repeat("z", maxEventPayloadBytes)},
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("over-cap payload = %v", err)
	}
	// A path-filtered trigger whose diff fails is a named failure (fail closed).
	e.git.diffs[strings.Repeat("b", 40)+".."+sha] = nil
	e.git.diffErr = errors.New("diff down")
	e.git.add(sha, ".spindle/workflows/paths.yml", `name: p
on:
  push:
    paths: ["src/**"]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`)
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{
		Branch: "main", RefName: "main", BaseSHA: strings.Repeat("b", 40),
	}); err == nil {
		t.Fatal("diff failure accepted")
	}
	e.git.diffErr = nil
	// An issue_comment run reads the default branch; a missing HEAD is a named
	// not-found, never a silent no-op.
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventIssueComment, CreateOpts{
		Action: "created", Actor: "ada",
	}); err != nil {
		t.Fatalf("issue comment without HEAD: %v", err)
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventRelease, CreateOpts{Action: "deleted"}); err != nil {
		t.Fatalf("release without HEAD: %v", err)
	}
}

// TestVariableAndEnvironmentRefusalArms covers the remaining refusal branches.
func TestVariableAndEnvironmentRefusalArms(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("r", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}
	if err := e.svc.SetVariable(ctx(), "o/r", "X", strings.Repeat("v", (64<<10)+1), admin); err == nil {
		t.Fatal("oversize variable accepted")
	}
	if err := e.svc.DeleteOrgVariable(ctx(), "acme", "GITHUB_TOKEN", admin); err == nil {
		t.Fatal("reserved org name deleted")
	}
	if _, err := e.svc.EnvironmentNames(ctx(), "bad", admin); err == nil {
		t.Fatal("bad environment repo accepted")
	}
	e.roles.public = false
	if _, err := e.svc.EnvironmentNames(ctx(), "o/r", auth.Anonymous()); err == nil {
		t.Fatal("anonymous environment read on a private repo accepted")
	}
}

// TestVariableEnvelopeArms covers seal/open failures (no key, bad base64,
// short envelope, wrong AAD).
func TestVariableEnvelopeArms(t *testing.T) {
	noKey := &Service{keys: &Keyring{}}
	if _, err := noKey.sealVariable("repo:o/r", "X", "v"); err == nil {
		t.Fatal("seal without a key accepted")
	}
	if _, err := noKey.openVariable("repo:o/r", "X", "abc"); err == nil {
		t.Fatal("open without a key accepted")
	}
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), "")
	e.svc.SetKeyring(k)
	if _, err := e.svc.openVariable("repo:o/r", "X", "!!!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := e.svc.openVariable("repo:o/r", "X", "YQ=="); err == nil {
		t.Fatal("short envelope accepted")
	}
	sealed, err := e.svc.sealVariable("repo:o/r", "X", "v")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.openVariable("repo:o/r", "Y", sealed); err == nil {
		t.Fatal("AAD-mismatched envelope opened")
	}
}

// TestVariableCorruptionArms covers the remaining corrupt-document branches.
func TestVariableCorruptionArms(t *testing.T) {
	if _, err := normalizeVariableName(""); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := normalizeVariableName(strings.Repeat("A", 70)); err == nil {
		t.Fatal("overlong name accepted")
	}
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("t", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}
	for _, key := range []string{VarsKey("o", "r"), OrgVarsKey("o")} {
		if _, err := store.PutBytes(ctx(), e.store, key, []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "X", "v", admin); err == nil {
		t.Fatal("corrupt repo doc accepted a write")
	}
	if err := e.svc.DeleteVariable(ctx(), "o/r", "X", admin); err == nil {
		t.Fatal("corrupt repo doc accepted a delete")
	}
	if _, err := e.svc.variableValues(ctx(), "o", "r"); err == nil {
		t.Fatal("corrupt org doc accepted")
	}
}

// TestVariableNoSecretsAndEncodeError covers the last small branches.
func TestVariableNoSecretsAndEncodeError(t *testing.T) {
	noKey := &Service{keys: &Keyring{}}
	vals, err := noKey.variableValues(ctx(), "o", "r")
	if err != nil || len(vals) != 0 {
		t.Fatalf("no-secrets variableValues = %v, %v", vals, err)
	}
	if _, err := EncodeEventPayload(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unmarshalable payload accepted")
	}
}

// TestDiffNamesRefusalsAndProjectionEdges covers the DiffNames validation arms
// and the event projection's non-map branch.
func TestDiffNamesRefusalsAndProjectionEdges(t *testing.T) {
	g := NewSubprocessGit("git")
	bad := strings.Repeat("z", 40)
	if _, err := g.DiffNames(context.Background(), "o/r", "", "not-a-sha", false); err == nil {
		t.Fatal("bad to-sha accepted")
	}
	if _, err := g.DiffNames(context.Background(), "o/r", "", bad, true); err == nil {
		t.Fatal("merge-base with no from accepted")
	}
	if _, err := g.DiffNames(context.Background(), "o/r", "not-a-sha", bad, false); err == nil {
		t.Fatal("bad from-sha accepted")
	}
	payload := map[string]any{
		"repository": "not-a-map",
		"action":     "opened",
		"comment":    map[string]any{"body": strings.Repeat("x", maxEventBodyBytes-1) + "é" + "tail"},
	}
	fields := EventFieldsOf(payload)
	if fields["event.action"] != "opened" {
		t.Fatalf("fields = %+v", fields)
	}
	if got := truncateBody(strings.Repeat("x", maxEventBodyBytes-1) + "é" + "tail"); len(got) != maxEventBodyBytes-1 {
		t.Fatalf("truncated body = %d bytes", len(got))
	}
}

// TestFetchJobResolvesVariables pins the claim-time vars path: a trusted job's
// payload carries the values and the mask list, and an untrusted run does not.
func TestFetchJobResolvesVariables(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("u", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetVariable(ctx(), "o/r", "REGION", "eu-secret-ish", admin); err != nil {
		t.Fatal(err)
	}
	run := e.mustCreate(t)[0]
	_ = run
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := e.svc.authorizeRunner(ctx(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	if job.Vars["REGION"] != "eu-secret-ish" {
		t.Fatalf("vars = %+v", job.Vars)
	}
	found := false
	for _, m := range job.Mask {
		if m == "eu-secret-ish" {
			found = true
		}
	}
	if !found {
		t.Fatalf("variable value not masked: %v", job.Mask)
	}
}

// TestVariableOrgGateKindsAndListEdges covers the org gate's error kinds and a
// decoded event list with non-name entries.
func TestVariableOrgGateKindsAndListEdges(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("w", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}
	e.svc.OrgAdmin = func(_ context.Context, _ string, _ auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "no"}
	}
	if err := e.svc.SetOrgVariable(ctx(), "acme", "X", "v", auth.Principal{Name: "ada"}); err == nil {
		t.Fatal("unauthorized org gate accepted")
	}
	e.svc.OrgAdmin = func(_ context.Context, _ string, _ auth.Principal) *auth.AuthError { return nil }
	if err := e.svc.SetOrgVariable(ctx(), "acme", "X", "v", auth.Principal{Name: "ada"}); err != nil {
		t.Fatalf("allowed org gate: %v", err)
	}
	if err := e.svc.DeleteOrgVariable(ctx(), "acme", "X", auth.Principal{Name: "ada"}); err != nil {
		t.Fatalf("allowed org delete: %v", err)
	}
	// A `{}` document has a nil map; deleting from it is a no-op.
	if _, err := store.PutBytes(ctx(), e.store, VarsKey("o", "r"), []byte(`{}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteVariable(ctx(), "o/r", "X", admin); err != nil {
		t.Fatalf("nil-map delete: %v", err)
	}
	// A decoded assets list with a non-name entry still projects the names.
	if got, _ := eventScalar([]any{"a", 3, map[string]any{"name": "b"}}); got != "a,b" {
		t.Fatalf("decoded list = %q", got)
	}
}

// TestCreateRunForkAndReleaseGuards covers a few CreateRun refusal arms.
func TestCreateRunForkAndReleaseGuards(t *testing.T) {
	e := newTestEnv(t)
	sha := strings.Repeat("a", 40)
	if _, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPullReq, CreateOpts{
		Action: "opened", Fork: true,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fork without base = %v", err)
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", "not-a-sha", EventRelease, CreateOpts{Action: "published"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad release commit = %v", err)
	}
}

// TestSmallWave2Helpers covers the few remaining one-line branches.
func TestSmallWave2Helpers(t *testing.T) {
	if got := serviceCommand(workflow.Service{Command: []string{"a"}, Cmd: []string{"b"}}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("serviceCommand precedence = %v", got)
	}
	if got := serviceCommand(workflow.Service{Cmd: []string{"b"}}); len(got) != 1 || got[0] != "b" {
		t.Fatalf("serviceCommand cmd = %v", got)
	}
	if got := serviceCommand(workflow.Service{}); len(got) != 0 {
		t.Fatalf("serviceCommand empty = %v", got)
	}
	if shortRef("refs/heads/main") != "main" || shortRef("main") != "main" {
		t.Fatal("shortRef")
	}
	if EventFieldsOf(nil) != nil {
		t.Fatal("nil payload projected fields")
	}
	if raw, err := EncodeEventPayload(nil); raw != nil || err != nil {
		t.Fatalf("nil encode = %v, %v", raw, err)
	}
}

// TestVariableHandlerServiceErrors covers the handlers' error responses.
func TestVariableHandlerServiceErrors(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "root", Admin: true}
	k, _ := NewKeyring(strings.Repeat("y", 32), "")
	e.svc.SetKeyring(k)
	req := httptest.NewRequest(http.MethodGet, "/bad/api/threads/variables", nil)
	rec := httptest.NewRecorder()
	h.listVariables(rec, req, "bad")
	if rec.Code < 400 {
		t.Fatalf("bad repo list = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPut, "/o/r/api/threads/variables/X", strings.NewReader(`{"unknown":1}`))
	rec = httptest.NewRecorder()
	h.putVariable(rec, req, "o/r", "X")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodDelete, "/bad/api/threads/variables/X", nil)
	rec = httptest.NewRecorder()
	h.deleteVariable(rec, req, "bad", "X")
	if rec.Code < 400 {
		t.Fatalf("bad repo delete = %d", rec.Code)
	}
	// A corrupt org document surfaces as a server error, not a panic.
	if _, err := store.PutBytes(ctx(), e.store, OrgVarsKey("acme"), []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/orgs/acme/threads/variables", nil)
	rec = httptest.NewRecorder()
	h.handleOrgVariables(rec, req, "acme", nil)
	if rec.Code < 400 {
		t.Fatalf("corrupt org list = %d", rec.Code)
	}
}
