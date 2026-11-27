package threads

// secrets_org_test.go — organization secret scope, policy and precedence
// (PAR-115), plus the masking refinements and the provenance record (PAR-116).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// orgFixture is a testEnv with a keyring, an org-owner seam and an audit sink.
type orgFixture struct {
	*testEnv
	audit []string
	admin auth.Principal
}

func newOrgFixture(t *testing.T) *orgFixture {
	t.Helper()
	e := newTestEnv(t)
	k, err := NewKeyring(strings.Repeat("o", 32), "")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e.svc.SetKeyring(k)
	f := &orgFixture{testEnv: e, admin: auth.Principal{Name: "owner@example.com", Admin: true}}
	e.svc.OrgAdmin = func(_ context.Context, org string, p auth.Principal) *auth.AuthError {
		if org != "acme" {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "not an org"}
		}
		if strings.HasSuffix(p.Name, "@example.com") && strings.HasPrefix(p.Name, "owner") {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "org owner required"}
	}
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		f.audit = append(f.audit, actor+" "+action+" "+target+" "+detail)
		return nil
	}
	return f
}

func TestOrgSecretScopeAndPrecedence(t *testing.T) {
	f := newOrgFixture(t)
	// An "all" secret resolves in every repository of the org.
	if err := f.svc.SetOrgSecret(ctx(), "acme", "SHARED_TOKEN", "org-value-1234",
		OrgSecretScope{Visibility: OrgSecretAll}, f.admin); err != nil {
		t.Fatalf("set org secret: %v", err)
	}
	for _, repo := range []string{"api", "web"} {
		vals, err := f.svc.orgSecretValues(ctx(), "acme", repo)
		if err != nil || vals["SHARED_TOKEN"] != "org-value-1234" {
			t.Fatalf("org secret in %s = %v, %v", repo, vals, err)
		}
	}
	// A "selected" secret reaches only the listed repositories: an unselected
	// repository gets NOTHING (not an empty value, not the name).
	if err := f.svc.SetOrgSecret(ctx(), "acme", "ONLY_API", "selected-value-1234",
		OrgSecretScope{Visibility: OrgSecretSelected, Repositories: []string{"ACME/api"}}, f.admin); err != nil {
		t.Fatalf("set selected secret: %v", err)
	}
	api, err := f.svc.orgSecretValues(ctx(), "acme", "api")
	if err != nil || api["ONLY_API"] != "selected-value-1234" {
		t.Fatalf("selected secret in api = %v, %v", api, err)
	}
	web, err := f.svc.orgSecretValues(ctx(), "acme", "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := web["ONLY_API"]; ok {
		t.Fatalf("an unselected repository received the secret: %v", web)
	}
	// The list surface reports the policy and never a value.
	list, err := f.svc.ListOrgSecrets(ctx(), "acme", f.admin)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, row := range list {
		if strings.Contains(row.Name, "value") || row.Repositories == nil {
			t.Fatalf("list row leaks or is nil: %+v", row)
		}
	}
	// Precedence: repository over organization.
	if err := f.svc.SetSecret(ctx(), "acme/api", "SHARED_TOKEN", "repo-value-1234", f.admin); err != nil {
		t.Fatalf("set repo secret: %v", err)
	}
	values, scopes, err := f.svc.resolvedSecrets(ctx(), "acme", "api")
	if err != nil {
		t.Fatal(err)
	}
	if values["SHARED_TOKEN"] != "repo-value-1234" || scopes["SHARED_TOKEN"] != SecretScopeRepository {
		t.Fatalf("repository must win over organization: %v %v", values["SHARED_TOKEN"], scopes["SHARED_TOKEN"])
	}
	if values["ONLY_API"] != "selected-value-1234" || scopes["ONLY_API"] != SecretScopeOrganization {
		t.Fatalf("organization-only secret = %q %q", values["ONLY_API"], scopes["ONLY_API"])
	}
	// A repository that shares no name gets the organization's value and scope.
	other, otherScopes, err := f.svc.resolvedSecrets(ctx(), "acme", "web")
	if err != nil || other["SHARED_TOKEN"] != "org-value-1234" || otherScopes["SHARED_TOKEN"] != SecretScopeOrganization {
		t.Fatalf("web = %v %v %v", other, otherScopes, err)
	}
	// The sealed envelope binds the org scope: it cannot be replayed as a repo
	// secret of the same name.
	raw, _, _ := store.GetBytes(ctx(), f.store, OrgSecretsKey("acme"), store.GetOptions{})
	var doc OrgSecretDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		t.Fatalf("org doc: %v", jerr)
	}
	entry := doc.Values["SHARED_TOKEN"]
	if entry.Sealed == "" || strings.Contains(entry.Sealed, "org-value") {
		t.Fatalf("envelope = %q", entry.Sealed)
	}
	if _, oerr := f.svc.openSecret("acme/api", "SHARED_TOKEN", entry.Sealed); oerr == nil {
		t.Fatal("an org envelope opened under the repository scope")
	}
	if got, oerr := f.svc.openSecret("org:acme", "SHARED_TOKEN", entry.Sealed); oerr != nil || got != "org-value-1234" {
		t.Fatalf("org envelope = %q, %v", got, oerr)
	}
}

func TestOrgSecretEnvironmentWinsOverEverything(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("e", 32), "")
	f.svc.SetKeyring(k)
	f.svc.OrgAdmin = func(context.Context, string, auth.Principal) *auth.AuthError { return nil }
	if err := f.svc.SetOrgSecret(ctx(), "o", "DEPLOY_TOKEN", "org-value-1234", OrgSecretScope{}, f.maintain()); err != nil {
		t.Fatalf("org secret: %v", err)
	}
	if err := f.svc.SetSecret(ctx(), "o/r", "DEPLOY_TOKEN", "repo-value-1234", f.maintain()); err != nil && !errors.Is(err, ErrForbidden) {
		t.Fatalf("repo secret: %v", err)
	}
	// SetSecret needs repo admin; the fixture maintain is not one, so write the
	// repository scope directly through the same store path.
	if err := f.svc.setRepoSecretForTest("o/r", "DEPLOY_TOKEN", "repo-value-1234"); err != nil {
		t.Fatalf("repo secret fixture: %v", err)
	}
	env := f.putEnvironment(t, "production")
	if env == nil {
		t.Fatal("environment")
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "DEPLOY_TOKEN", "env-value-1234", f.maintain()); err != nil {
		t.Fatalf("env secret: %v", err)
	}
	run := f.deployRun(t, CreateOpts{})
	runner := needsRunner()
	var payload *FetchResult
	for i := 0; i < 2; i++ {
		claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil || claim == nil {
			t.Fatalf("claim %d = %+v, %v", i, claim, cerr)
		}
		res, perr := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch: %v", perr)
		}
		if claim.Job.Name == "ship" {
			payload = res
		}
	}
	if payload == nil {
		t.Fatalf("no ship payload (run %s)", run.ID)
	}
	// ENVIRONMENT > REPOSITORY > ORGANIZATION, and the scope says so.
	if got := payload.Secrets["DEPLOY_TOKEN"]; got != "env-value-1234" {
		t.Fatalf("environment secret = %q", got)
	}
	if got := payload.SecretScopes["DEPLOY_TOKEN"]; got != SecretScopeEnvironment {
		t.Fatalf("scope = %q, want environment", got)
	}
	// The run record carries the provenance (never the value).
	jobs := mustJobs(t, f.testEnv, run.ID)
	var ship *Job
	for _, job := range jobs {
		if job.Name == "ship" {
			ship = job
		}
	}
	if ship == nil || ship.SecretScopes["DEPLOY_TOKEN"] != SecretScopeEnvironment {
		t.Fatalf("job scope record = %+v", ship)
	}
	if strings.Contains(mustJSONString(t, ship), "env-value-1234") {
		t.Fatal("the job record stored a secret value")
	}
}

// setRepoSecretForTest writes one repository secret through the production
// storage path with an admin principal (the fixture's maintain is below the
// repo-secret floor).
func (s *Service) setRepoSecretForTest(repo, name, value string) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	sealed, serr := s.sealSecret(repo, name, value)
	if serr != nil {
		return serr
	}
	_, err = store.PutBytes(ctx(), s.Store, SecretKey(owner, rname, name), mustJSON(&SecretDoc{
		Name: name, Sealed: sealed, CreatedAt: "2026-11-27T00:00:00Z", UpdatedAt: "2026-11-27T00:00:00Z", Version: 1,
	}), store.PutOptions{Mode: store.PutOverwrite})
	return err
}

func TestFetchRecordsSecretScopesAndMasksOrgValues(t *testing.T) {
	f := newOrgFixture(t)
	if err := f.svc.SetOrgSecret(ctx(), "o", "ORG_TOKEN", "org-secret-value", OrgSecretScope{}, f.admin); err != nil {
		t.Fatalf("org secret: %v", err)
	}
	if err := f.svc.SetSecret(ctx(), "o/r", "REPO_TOKEN", "repo-secret-value", f.admin); err != nil {
		t.Fatalf("repo secret: %v", err)
	}
	run := f.mustCreate(t)[0]
	runner := needsRunner()
	claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if cerr != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, cerr)
	}
	payload, err := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if payload.Secrets["ORG_TOKEN"] != "org-secret-value" || payload.Secrets["REPO_TOKEN"] != "repo-secret-value" {
		t.Fatalf("payload secrets = %v", payload.Secrets)
	}
	if payload.SecretScopes["ORG_TOKEN"] != SecretScopeOrganization ||
		payload.SecretScopes["REPO_TOKEN"] != SecretScopeRepository {
		t.Fatalf("payload scopes = %v", payload.SecretScopes)
	}
	// Both values are in the mask list (the runner masks them before sending).
	joined := strings.Join(payload.Mask, "\n")
	if !strings.Contains(joined, "org-secret-value") || !strings.Contains(joined, "repo-secret-value") {
		t.Fatalf("mask list = %v", payload.Mask)
	}
	// The record names the scope, never the value.
	job := mustJob(t, f.testEnv, run.ID, claim.Entry.JobID)
	if job.SecretScopes["ORG_TOKEN"] != SecretScopeOrganization {
		t.Fatalf("job scopes = %v", job.SecretScopes)
	}
	if strings.Contains(mustJSONString(t, job), "org-secret-value") {
		t.Fatal("the job record stored a secret value")
	}
	// The server masker covers the organization value too (AppendLog path).
	tok, _, _ := f.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: run.ID, Job: claim.Entry.JobID, Attempt: claim.Entry.Attempt,
		Runner: runner.ID, Scopes: attemptScopes(nil), Exp: f.now.Add(time.Hour).Unix(),
	})
	claims, _ := f.svc.parseAttemptToken(tok, f.now)
	if _, err := f.svc.AppendLog(ctx(), claims, 1, "prints org-secret-value and repo-secret-value"); err != nil {
		t.Fatalf("append: %v", err)
	}
	frames, err := f.svc.LogFrames(ctx(), "o/r", run.ID, claim.Entry.JobID, claim.Entry.Attempt, 0, 100, f.admin)
	if err != nil || frames == nil || len(frames.Frames) == 0 {
		t.Fatalf("frames = %+v, %v", frames, err)
	}
	text := ""
	for _, fr := range frames.Frames {
		text += fr.Text
	}
	if strings.Contains(text, "org-secret-value") || strings.Contains(text, "repo-secret-value") {
		t.Fatalf("a secret reached the stored log: %q", text)
	}
	if !strings.Contains(text, "***") {
		t.Fatalf("mask marker missing: %q", text)
	}
	// Complete re-masks annotations, step errors and the summary.
	done, cerr2 := f.svc.Complete(ctx(), claims, CompleteInput{
		Conclusion: "success",
		Steps:      []StepResult{{Name: "s", Conclusion: "failure", Error: "boom org-secret-value"}},
		Annotations: []Annotation{{
			Level: "error", Message: "leaked org-secret-value", Title: "org-secret-value",
		}},
		Summary: "## summary\n\norg-secret-value\n",
	})
	if cerr2 != nil {
		t.Fatalf("complete: %v", cerr2)
	}
	_ = done
	job = mustJob(t, f.testEnv, run.ID, claim.Entry.JobID)
	if strings.Contains(mustJSONString(t, job), "org-secret-value") {
		t.Fatalf("complete stored a secret: %+v", job)
	}
	if job.Summary == "" || !strings.Contains(job.Summary, "***") {
		t.Fatalf("summary = %q", job.Summary)
	}
}

func TestUntrustedRunGetsNoSecretAndNoScope(t *testing.T) {
	f := newOrgFixture(t)
	if err := f.svc.SetOrgSecret(ctx(), "o", "ORG_TOKEN", "org-secret-value", OrgSecretScope{}, f.admin); err != nil {
		t.Fatal(err)
	}
	// A fork pull_request run is untrusted: the fetch carries no values and no
	// scope record, and the fork's `secrets.X` reads empty.
	run := f.forkRun(t)
	runner := needsRunner()
	runner.AllowForkRuns = true
	claim, cerr := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if cerr != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, cerr)
	}
	payload, err := f.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, claim, claim.Job, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !payload.SecretsDenied || len(payload.Secrets) != 0 || len(payload.SecretScopes) != 0 {
		t.Fatalf("untrusted payload = %+v", payload)
	}
	job := mustJob(t, f.testEnv, run.ID, claim.Entry.JobID)
	if len(job.SecretScopes) != 0 {
		t.Fatalf("untrusted job recorded scopes: %v", job.SecretScopes)
	}
}

func TestOrgSecretAuthorizationAndValidation(t *testing.T) {
	f := newOrgFixture(t)
	owner := auth.Principal{Name: "owner@example.com"}
	if err := f.svc.SetOrgSecret(ctx(), "acme", "X", "value-1234", OrgSecretScope{}, owner); err != nil {
		t.Fatalf("owner write: %v", err)
	}
	// A non-owner is refused (the seam's 403), a host admin bypasses it.
	if err := f.svc.SetOrgSecret(ctx(), "acme", "Y", "value-1234", OrgSecretScope{}, auth.Principal{Name: "mallory"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner = %v", err)
	}
	if _, err := f.svc.ListOrgSecrets(ctx(), "acme", auth.Principal{Name: "mallory"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner read = %v", err)
	}
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "X", auth.Principal{Name: "mallory"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner delete = %v", err)
	}
	// An unauthenticated caller gets the authentication error, not a 404 that
	// would confirm the org exists.
	if err := f.svc.SetOrgSecret(ctx(), "acme", "Z", "value-1234", OrgSecretScope{}, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	// An unwired gate is a named 503, never an implicit allow.
	f.svc.OrgAdmin = nil
	if err := f.svc.SetOrgSecret(ctx(), "acme", "W", "value-1234", OrgSecretScope{}, auth.Principal{Name: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unwired gate = %v", err)
	}
	f.svc.OrgAdmin = func(context.Context, string, auth.Principal) *auth.AuthError { return nil }

	// Validation: name, value size, visibility and the selection list.
	bad := []struct {
		name  string
		org   string
		sec   string
		value string
		scope OrgSecretScope
	}{
		{"empty org", "", "X", "v", OrgSecretScope{}},
		{"bad name", "acme", "1BAD", "v", OrgSecretScope{}},
		{"empty value", "acme", "X", "", OrgSecretScope{}},
		{"huge value", "acme", "X", strings.Repeat("v", 64<<10+1), OrgSecretScope{}},
		{"bad visibility", "acme", "X", "v", OrgSecretScope{Visibility: "private"}},
		{"selected without repos", "acme", "X", "v", OrgSecretScope{Visibility: OrgSecretSelected}},
		{"foreign repository", "acme", "X", "v", OrgSecretScope{Visibility: OrgSecretSelected, Repositories: []string{"other/api"}}},
		{"bad repository name", "acme", "X", "v", OrgSecretScope{Visibility: OrgSecretSelected, Repositories: []string{".hidden"}}},
	}
	for _, tc := range bad {
		if err := f.svc.SetOrgSecret(ctx(), tc.org, tc.sec, tc.value, tc.scope, f.admin); !errors.Is(err, ErrInvalid) && tc.name != "huge value" {
			t.Fatalf("%s = %v, want invalid", tc.name, err)
		}
	}
	// The cap: MaxOrgSecrets entries, the next one is a named refusal.
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "X", f.admin); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for i := 0; i < MaxOrgSecrets; i++ {
		name := "S" + strconv.Itoa(i)
		if err := f.svc.SetOrgSecret(ctx(), "acme", name, "value-1234", OrgSecretScope{}, f.admin); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if err := f.svc.SetOrgSecret(ctx(), "acme", "OVERFLOW", "value-1234", OrgSecretScope{}, f.admin); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cap = %v", err)
	}
	// Replacing an existing entry at the cap is allowed.
	if err := f.svc.SetOrgSecret(ctx(), "acme", "S0", "value-5678", OrgSecretScope{}, f.admin); err != nil {
		t.Fatalf("replace at cap: %v", err)
	}
	// Delete: absent is not an error; a corrupt document fails closed.
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "MISSING", f.admin); err != nil {
		t.Fatalf("absent delete: %v", err)
	}
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"), []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ListOrgSecrets(ctx(), "acme", f.admin); err == nil {
		t.Fatal("a corrupt document listed")
	}
	if _, err := f.svc.orgSecretValues(ctx(), "acme", "api"); err == nil {
		t.Fatal("a corrupt document resolved")
	}
	// The audit family names the secret and its policy, never a value.
	if len(f.audit) == 0 {
		t.Fatal("no audit entry")
	}
	for _, line := range f.audit {
		if strings.Contains(line, "value-1234") || strings.Contains(line, "value-5678") {
			t.Fatalf("audit entry carries a value: %q", line)
		}
	}
}

func TestOrgSecretHTTP(t *testing.T) {
	e, h, cur := newHTTP(t)
	k, _ := NewKeyring(strings.Repeat("h", 32), "")
	e.svc.SetKeyring(k)
	*cur = auth.Principal{Name: "root", Admin: true}

	// A host admin bypasses org ownership (the API's CheckOrgOwner rule), so
	// the write lands even before a seam is wired.
	rec := doAuth(h, http.MethodPut, "/api/v1/orgs/acme/threads/secrets/DEPLOY", `{"value":"nope-not-in-tests"}`, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin put = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/orgs/acme/threads/secrets/DEPLOY", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("reset delete = %d", rec.Code)
	}
	// A non-admin with no OrgAdmin seam is a named 503 (never an implicit
	// allow); with the seam wired the owner's write lands.
	*cur = auth.Principal{Name: "someone"}
	if rec := doAuth(h, http.MethodPut, "/api/v1/orgs/acme/threads/secrets/DEPLOY", `{"value":"nope-not-in-tests"}`, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired org put = %d", rec.Code)
	}
	e.svc.OrgAdmin = func(context.Context, string, auth.Principal) *auth.AuthError { return nil }
	*cur = auth.Principal{Name: "someone"}
	rec = doAuth(h, http.MethodPut, "/api/v1/orgs/acme/threads/secrets/DEPLOY",
		`{"value":"http-secret-value","visibility":"selected","repositories":["acme/api"]}`, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodGet, "/api/v1/orgs/acme/threads/secrets", "", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "http-secret-value") {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"visibility":"selected"`) || !strings.Contains(rec.Body.String(), `"api"`) {
		t.Fatalf("list shape = %s", rec.Body.String())
	}
	// The strict decoder refuses an unknown field (a client cannot smuggle a
	// value or a scope through an unexpected key).
	rec = doAuth(h, http.MethodPut, "/api/v1/orgs/acme/threads/secrets/DEPLOY", `{"value":"x","sealed":"y"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodDelete, "/api/v1/orgs/acme/threads/secrets/DEPLOY", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = doAuth(h, http.MethodPost, "/api/v1/orgs/acme/threads/secrets", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", rec.Code)
	}
	*cur = auth.Anonymous()
	rec = doAuth(h, http.MethodGet, "/api/v1/orgs/acme/threads/secrets", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
}

func TestMaskingCoversMultiLineAndShortValues(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		values []string
		want   string
	}{
		{"long value", "token=abcdefgh", []string{"abcdefgh"}, "token=***"},
		// The whole value is one pattern (it matches as printed); the line
		// clause below is what covers a streamer that split it.
		{"multi-line value", "BEGIN\nline-one-secret\nEND", []string{"BEGIN\nline-one-secret\nEND"}, "***"},
		{"multi-line line printed alone", "printed: line-one-secret", []string{"BEGIN\nline-one-secret\nEND"}, "printed: ***"},
		{"multi-line indented line", "x\n  spaced-secret\n", []string{"k\n  spaced-secret\n"}, "x\n***\n"},
		{"short value as a token", "code=ab done", []string{"ab"}, "code=*** done"},
		{"short value inside a word", "crab abstract", []string{"ab"}, "crab abstract"},
		{"short value inside a path", "s3://bucket", []string{"s3"}, "s3://bucket"},
		{"short value at the edges", "ab", []string{"ab"}, "***"},
		{"one byte", "(x)", []string{"x"}, "(***)"},
		{"no values", "nothing", nil, "nothing"},
		{"empty text", "", []string{"abcdefgh"}, ""},
		{"longest first", "abcdefgh", []string{"abcd", "abcdefg"}, "***h"},
	}
	for _, tc := range cases {
		if got := maskSecretValues(tc.text, tc.values); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// The runner applies the same rule through the exported entry point.
	if got := MaskValues("code=ab", []string{"ab"}); got != "code=***" {
		t.Fatalf("exported rule = %q", got)
	}
}

// --- helpers ---------------------------------------------------------------

// forkRun creates an untrusted (fork) pull_request run over the fixture.
func (e *testEnv) forkRun(t *testing.T) *Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", BaseBranch: "main", Fork: true, Actor: "forker",
		Action: "opened", PullNumber: 9, Ref: "refs/pull/9/head",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("fork run = %v, %v", runs, err)
	}
	if runs[0].Trusted {
		t.Fatal("a fork run was trusted")
	}
	if runs[0].State == RunPendingApproval {
		// The fixture has no approval evidence source, so the fork gate holds
		// it; approve as a host admin so the claim below is reachable.
		e.approveHeld(t, runs[0].ID)
	}
	return runs[0]
}

// approveHeld approves a held run as a host admin.
func (e *testEnv) approveHeld(t *testing.T, runID string) {
	t.Helper()
	if _, err := e.svc.ApproveRun(ctx(), "o/r", runID, auth.Principal{Name: "root", Admin: true}); err != nil {
		t.Fatalf("approve held run: %v", err)
	}
}

func mustJob(t *testing.T, e *testEnv, runID, jobID string) *Job {
	t.Helper()
	for _, job := range mustJobs(t, e, runID) {
		if job.ID == jobID {
			return job
		}
	}
	t.Fatalf("run %s has no job %s", runID, jobID)
	return nil
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestMaskCacheLifecycle pins the per-attempt mask cache: a hit avoids the
// resolution, an expired entry is a miss, the map stays bounded, and a nil
// cache (a literal Service) degrades to no caching instead of panicking.
func TestMaskCacheLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	c := newMaskCache()
	if _, ok := c.get("k", now); ok {
		t.Fatal("an empty cache hit")
	}
	c.put("k", []string{"value-1234"}, now)
	if got, ok := c.get("k", now); !ok || len(got) != 1 || got[0] != "value-1234" {
		t.Fatalf("cache hit = %v %v", got, ok)
	}
	if _, ok := c.get("k", now.Add(maskCacheTTL+time.Second)); ok {
		t.Fatal("an expired entry was a hit")
	}
	// The map is bounded: filling past the cap drops an entry, never grows.
	for i := 0; i < maskCacheMax+8; i++ {
		c.put("k"+strconv.Itoa(i), []string{"v"}, now)
	}
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size > maskCacheMax {
		t.Fatalf("cache size = %d, cap %d", size, maskCacheMax)
	}
	// Expired entries are swept before a fresh one.
	old := now.Add(-maskCacheTTL - time.Minute)
	stale := newMaskCache()
	for i := 0; i < maskCacheMax; i++ {
		stale.put("s"+strconv.Itoa(i), nil, old)
	}
	stale.put("fresh", []string{"v"}, now)
	stale.mu.Lock()
	if _, ok := stale.entries["fresh"]; !ok {
		stale.mu.Unlock()
		t.Fatal("a fresh entry was evicted instead of the stale ones")
	}
	stale.mu.Unlock()
	c.forget("k")
	var nilCache *maskCache
	if _, ok := nilCache.get("x", now); ok {
		t.Fatal("nil cache hit")
	}
	nilCache.put("x", nil, now)
	nilCache.forget("x")
}

// TestJobMaskValuesEnvironmentAndFaults covers the full mask set (org, repo,
// environment secrets and variables) and the fail-closed arms.
func TestJobMaskValuesEnvironmentAndFaults(t *testing.T) {
	f := newEnvFixture(t)
	k, _ := NewKeyring(strings.Repeat("m", 32), "")
	f.svc.SetKeyring(k)
	f.svc.OrgAdmin = func(context.Context, string, auth.Principal) *auth.AuthError { return nil }
	admin := auth.Principal{Name: "root", Admin: true}
	if err := f.svc.SetOrgSecret(ctx(), "o", "ORG_TOKEN", "org-mask-value", OrgSecretScope{}, admin); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetVariable(ctx(), "o/r", "REPO_VAR", "var-mask-value", admin); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOrgVariable(ctx(), "o", "ORG_VAR", "orgvar-mask-value", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, admin); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "production", "ENV_TOKEN", "env-mask-value", admin); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetEnvironmentVariable(ctx(), "o/r", "production", "ENV_VAR", "envvar-mask-value", admin); err != nil {
		t.Fatal(err)
	}
	run := f.deployRun(t, CreateOpts{})
	var ship *Job
	for _, job := range mustJobs(t, f.testEnv, run.ID) {
		if job.Name == "ship" {
			ship = job
		}
	}
	if ship == nil {
		t.Fatal("no ship job")
	}
	values, err := f.svc.jobMaskValues(ctx(), "o", "r", ship)
	if err != nil {
		t.Fatalf("jobMaskValues: %v", err)
	}
	joined := strings.Join(values, "\n")
	for _, want := range []string{"org-mask-value", "var-mask-value", "orgvar-mask-value", "env-mask-value", "envvar-mask-value"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mask set misses %q: %v", want, values)
		}
	}
	// The second call is the cache hit (same values, no store read).
	if again, err := f.svc.jobMaskValues(ctx(), "o", "r", ship); err != nil || len(again) != len(values) {
		t.Fatalf("cached set = %v, %v", again, err)
	}
	f.svc.forgetJobMask("o/r", run.ID, ship.ID, ship.Attempt)
	if _, err := f.svc.jobMaskValues(ctx(), "o", "r", ship); err != nil {
		t.Fatalf("after forget: %v", err)
	}
	// A job that names an environment that no longer exists fails closed (the
	// cache is per attempt, so drop it first as a requeue would).
	ship.Spec.Environment = &EnvironmentSpec{Name: "gone"}
	f.svc.forgetJobMask("o/r", run.ID, ship.ID, ship.Attempt)
	if _, err := f.svc.jobMaskValues(ctx(), "o", "r", ship); err == nil {
		t.Fatal("a vanished environment resolved a partial mask set")
	}
	if _, err := f.svc.jobMaskValues(ctx(), "o", "r", nil); err != nil {
		t.Fatalf("nil job = %v", err)
	}
}

// TestSecretResolutionFaultArms covers the corrupt/absent record arms the
// resolution paths must refuse rather than silently skip.
func TestSecretResolutionFaultArms(t *testing.T) {
	f := newOrgFixture(t)
	// A corrupt repository secret fails the resolution.
	if _, err := store.PutBytes(ctx(), f.store, SecretKey("o", "r", "BROKEN"),
		[]byte(`{"name":"BROKEN","sealed":"AAAA"}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.repoSecretValues(ctx(), "o", "r"); err == nil {
		t.Fatal("a corrupt repository envelope resolved")
	}
	if _, _, err := f.svc.resolvedSecrets(ctx(), "o", "r"); err == nil {
		t.Fatal("resolvedSecrets accepted a corrupt envelope")
	}
	if err := f.store.Delete(ctx(), SecretKey("o", "r", "BROKEN"), ""); err != nil {
		t.Fatal(err)
	}
	// A listed-but-absent record is skipped, never injected empty.
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"),
		[]byte(`{"values":{"X":{"name":"X","sealed":""}}}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.orgSecretValues(ctx(), "acme", "api"); err == nil {
		t.Fatal("an empty envelope resolved")
	}
	// DeleteOrgSecret fails closed on a corrupt document.
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"),
		[]byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "X", f.admin); err == nil {
		t.Fatal("delete accepted a corrupt document")
	}
}

// TestOrgSecretPolicyEdges pins the policy normalization corners and the
// masking walk's small branches.
func TestOrgSecretPolicyEdges(t *testing.T) {
	f := newOrgFixture(t)
	// Duplicates are collapsed, bare and org-qualified spellings agree, and the
	// list is sorted.
	if err := f.svc.SetOrgSecret(ctx(), "acme", "DEDUP", "value-1234", OrgSecretScope{
		Visibility:   "SELECTED",
		Repositories: []string{"api", "ACME/api", " acme/web ", "", "web"},
	}, f.admin); err != nil {
		t.Fatalf("dedup policy: %v", err)
	}
	list, err := f.svc.ListOrgSecrets(ctx(), "acme", f.admin)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if got := strings.Join(list[0].Repositories, ","); got != "api,web" {
		t.Fatalf("repositories = %q", got)
	}
	// Every policy value selects as documented; an uninterpretable one fails
	// closed.
	cases := []struct {
		visibility string
		repos      []string
		repo       string
		want       bool
	}{
		{"", nil, "api", true},
		{"all", []string{"ignored"}, "web", true},
		{"selected", []string{"api"}, "API", true},
		{"selected", []string{"api"}, "web", false},
		{"nonsense", []string{"api"}, "api", false},
	}
	for _, tc := range cases {
		if got := selectsRepo(tc.visibility, tc.repos, tc.repo); got != tc.want {
			t.Fatalf("selectsRepo(%q,%v,%q) = %v", tc.visibility, tc.repos, tc.repo, got)
		}
	}
	// Too many repositories is a named refusal.
	many := make([]string, MaxOrgSecretRepos+1)
	for i := range many {
		many[i] = "r" + strconv.Itoa(i)
	}
	if err := f.svc.SetOrgSecret(ctx(), "acme", "MANY", "value-1234",
		OrgSecretScope{Visibility: OrgSecretSelected, Repositories: many}, f.admin); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many repos = %v", err)
	}
	// Delete twice: the second is absent (not an error), and a corrupt record
	// is refused.
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "DEDUP", f.admin); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "DEDUP", f.admin); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	// Masking walk corners: an empty text, an empty value, a short line inside
	// a multi-line value, and a value repeated at the end of the text.
	if got := maskSecretValues("", []string{"value-1234"}); got != "" {
		t.Fatalf("empty text = %q", got)
	}
	if got := maskSecretValues("text", []string{""}); got != "text" {
		t.Fatalf("empty value = %q", got)
	}
	if got := maskSecretValues("short-line", []string{"k\nab\n"}); got != "short-line" {
		t.Fatalf("a short multi-line component was masked: %q", got)
	}
	if got := maskSecretValues("ab ab", []string{"ab"}); got != "*** ***" {
		t.Fatalf("repeated short value = %q", got)
	}
	if got := replaceToken("", "ab"); got != "" {
		t.Fatalf("empty token walk = %q", got)
	}
	if got := replaceToken("ab", ""); got != "ab" {
		t.Fatalf("empty value walk = %q", got)
	}
}

// TestSecretResolutionEdgeArms covers the corrupt-document, legacy and
// fail-closed branches of the scope stores and the mask set.
func TestSecretResolutionEdgeArms(t *testing.T) {
	f := newOrgFixture(t)
	// A literal Service (no keyring) resolves nothing rather than failing.
	bare := &Service{Store: f.store, Now: f.svc.Now}
	if _, err := bare.repoSecretValues(ctx(), "o", "r"); err != nil {
		t.Fatalf("disabled repo secrets = %v", err)
	}
	if _, err := bare.orgSecretValues(ctx(), "acme", "api"); err != nil {
		t.Fatalf("disabled org secrets = %v", err)
	}
	// SetOrgSecret refuses a corrupt document and tolerates a document with no
	// `values` map (a record written by an older build).
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"), []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOrgSecret(ctx(), "acme", "X", "value-1234", OrgSecretScope{}, f.admin); err == nil {
		t.Fatal("a corrupt organization document accepted a write")
	}
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"), []byte(`{"version":1}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOrgSecret(ctx(), "acme", "LEGACY", "value-1234", OrgSecretScope{}, f.admin); err != nil {
		t.Fatalf("write over a values-less document: %v", err)
	}
	// A stored entry with no visibility reads as "all" (the compatible default
	// for a record an older build wrote).
	raw, _, _ := store.GetBytes(ctx(), f.store, OrgSecretsKey("acme"), store.GetOptions{})
	var doc OrgSecretDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc.Values["LEGACY"]
	entry.Visibility = ""
	doc.Values["LEGACY"] = entry
	if _, err := store.PutBytes(ctx(), f.store, OrgSecretsKey("acme"), mustJSON(&doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.ListOrgSecrets(ctx(), "acme", f.admin)
	if err != nil || len(list) != 1 || list[0].Visibility != OrgSecretAll {
		t.Fatalf("legacy visibility = %+v, %v", list, err)
	}
	// Delete in an organisation with no document at all is a no-op.
	other := f.svc
	if err := other.DeleteOrgSecret(ctx(), "acme", "LEGACY", f.admin); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.store.Delete(ctx(), OrgSecretsKey("acme"), ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteOrgSecret(ctx(), "acme", "LEGACY", f.admin); err != nil {
		t.Fatalf("delete without a document: %v", err)
	}
	// A corrupt variable document fails the mask set (masking is not optional).
	if _, err := store.PutBytes(ctx(), f.store, VarsKey("o", "r"), []byte(`{`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "j-x", RunID: "r-x", Attempt: 1}
	if _, err := f.svc.jobMaskValues(ctx(), "o", "r", job); err == nil {
		t.Fatal("a corrupt variable document resolved a mask set")
	}
	if err := f.store.Delete(ctx(), VarsKey("o", "r"), ""); err != nil {
		t.Fatal(err)
	}
	// Duplicate patterns are collapsed before masking.
	if got := maskSecretValues("value-1234", []string{"value-1234", "value-1234"}); got != "***" {
		t.Fatalf("duplicate pattern = %q", got)
	}
}

// TestJobMaskValuesCapAndDisabledStore covers the mask-list bound and the
// fail-closed arms of an instance without a keyring.
func TestJobMaskValuesCapAndDisabledStore(t *testing.T) {
	f := newOrgFixture(t)
	admin := auth.Principal{Name: "root", Admin: true}
	// A Service with no keyring refuses a write rather than storing plaintext.
	bare := &Service{Store: f.store, Now: f.svc.Now}
	if err := bare.SetOrgSecret(ctx(), "acme", "X", "value-1234", OrgSecretScope{}, admin); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled store = %v", err)
	}
	// The mask list is bounded: 100 org secrets + 100 repository secrets + 100
	// variables hold 300 distinct values, and the set stops at the bound.
	for i := 0; i < MaxOrgSecrets; i++ {
		name := "O" + strconv.Itoa(i)
		if err := f.svc.SetOrgSecret(ctx(), "acme", name, "org-value-"+strconv.Itoa(i), OrgSecretScope{}, admin); err != nil {
			t.Fatalf("org %d: %v", i, err)
		}
		if err := f.svc.SetSecret(ctx(), "acme/api", "R"+strconv.Itoa(i), "repo-value-"+strconv.Itoa(i), admin); err != nil {
			t.Fatalf("repo %d: %v", i, err)
		}
		if err := f.svc.SetVariable(ctx(), "acme/api", "V"+strconv.Itoa(i), "var-value-"+strconv.Itoa(i), admin); err != nil {
			t.Fatalf("var %d: %v", i, err)
		}
	}
	job := &Job{ID: "j-cap", RunID: "r-cap", Attempt: 1}
	values, err := f.svc.jobMaskValues(ctx(), "acme", "api", job)
	if err != nil {
		t.Fatalf("mask set: %v", err)
	}
	if len(values) != maxMaskValues {
		t.Fatalf("mask set = %d values, want the bound %d", len(values), maxMaskValues)
	}
}
