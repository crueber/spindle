package threads

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/server/auth"
)

// PAR-113: the job token is a scoped platform API credential. These tests
// drive the real issues HTTP surface with a wjt_ token, so the claim is proved
// end to end: the same credential the runner uses for git authenticates the
// platform API, and its authority is the effective `permissions:` map — never
// wider, and never in another repository.

// jobTokenPrincipal starts one job of a workflow with the given permissions and
// returns its attempt token plus the resolved platform principal.
func jobTokenPrincipal(t *testing.T, e *testEnv, permsYAML string) (tok string, p auth.Principal, runID, jobID string) {
	t.Helper()
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
permissions:
`+permsYAML+`
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run := e.mustCreate(t)[0]
	jobID, view := startJob(t, e, run)
	if strings.TrimSpace(view.Token) == "" {
		t.Fatal("no job token issued")
	}
	p, aerr, ok := e.svc.resolveCloneCredential(ctx(), view.Token)
	if !ok || aerr != nil {
		t.Fatalf("resolve job token: ok=%v err=%v", ok, aerr)
	}
	return view.Token, p, run.ID, jobID
}

// issuesSurface builds the real issues handler over the same store, resolving
// its principal through the job-token credential path (the composition shape).
func issuesSurface(t *testing.T, e *testEnv) (*issues.Handler, *identity.Service, *issues.Service) {
	t.Helper()
	ids := identity.New(e.store, nil)
	iss := issues.New(e.store, ids)
	h := &issues.Handler{Svc: iss}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		tok := bearerToken(r.Header.Get("Authorization"))
		if tok == "" {
			return auth.Anonymous(), nil
		}
		p, aerr, ok := e.svc.resolveCloneCredential(r.Context(), tok)
		if !ok {
			return auth.Anonymous(), nil
		}
		return p, aerr
	}
	return h, ids, iss
}

func doIssues(t *testing.T, h *issues.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPAR113JobTokenScopedAPICredential(t *testing.T) {
	e := newTestEnv(t)
	h, ids, iss := issuesSurface(t, e)
	th, _, err := iss.CreateIssue(ctx(), "o", "r", auth.Principal{Name: "writer"}, "hello", "body", nil)
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	// issues: write → the issue surface acts as triage: comment and label.
	writeTok, writeP, _, _ := jobTokenPrincipal(t, e, "  issues: write")
	if writeP.JobPerms["issues"] != PermWrite {
		t.Fatalf("job principal perms = %v", writeP.JobPerms)
	}
	rec := doIssues(t, h, http.MethodPost, fmt.Sprintf("/o/r/api/issues/%d/comments", th.Num), writeTok,
		`{"body":"built by ci"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issues: write comment = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doIssues(t, h, http.MethodPost, "/o/r/api/labels", writeTok, `{"name":"ci","color":"00ff00"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issues: write label = %d (%s)", rec.Code, rec.Body.String())
	}

	// contents: read → read role: read and comment, but no triage action.
	readTok, readP, _, _ := jobTokenPrincipal(t, e, "  contents: read")
	if containsScope(PermissionScopes(readP.JobPerms), ScopePush) {
		t.Fatalf("contents: read token carries push: %v", readP.JobPerms)
	}
	rec = doIssues(t, h, http.MethodPost, fmt.Sprintf("/o/r/api/issues/%d/comments", th.Num), readTok,
		`{"body":"read-only note"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("contents: read comment = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doIssues(t, h, http.MethodPost, "/o/r/api/labels", readTok, `{"name":"nope","color":"ff0000"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("contents: read label = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// The same token is refused another repository's write surface.
	rec = doIssues(t, h, http.MethodPost, "/o/other/api/labels", writeTok, `{"name":"leak","color":"ff0000"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-repository label = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// A token cannot mint another credential: every mint path requires admin,
	// a role the job-token mapping never reaches.
	if err := ids.CheckRole(ctx(), "o", "r", writeP, identity.RoleAdmin); err == nil {
		t.Fatal("a job token with contents/issues write resolved to admin")
	}
	if err := ids.CheckRole(ctx(), "o", "r", readP, identity.RoleWrite); err == nil {
		t.Fatal("a contents: read job token resolved to write")
	}
	if writeP.Admin || writeP.Write {
		t.Fatalf("job principal carries host flags: %+v", writeP)
	}

	// The Threads operator surface (dispatch/cancel/rerun/approvals) is never a
	// job-token capability: `actions` is a refused scope, so a contents: write
	// job must not be able to dispatch a run and route around the job-push
	// suppression rule.
	writeTok2, _, _, _ := jobTokenPrincipal(t, e, "  contents: write")
	apiHandler := e.svc
	jobP, aerr, ok := apiHandler.resolveCloneCredential(ctx(), writeTok2)
	if !ok || aerr != nil {
		t.Fatalf("resolve: %v %v", ok, aerr)
	}
	if _, err := apiHandler.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, jobP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("job token dispatch = %v, want ErrForbidden", err)
	}
	if _, err := apiHandler.CancelRun(ctx(), "o/r", "some-run", jobP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("job token cancel = %v, want ErrForbidden", err)
	}
}

// TestPAR113JobTokenExpiryAndRevocation pins the credential's lifetime on the
// API path: an expired token is rejected before any store read, and a terminal
// job's token is revoked.
func TestPAR113JobTokenExpiryAndRevocation(t *testing.T) {
	e := newTestEnv(t)
	h, _, iss := issuesSurface(t, e)
	th, _, err := iss.CreateIssue(ctx(), "o", "r", auth.Principal{Name: "writer"}, "hello", "body", nil)
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	tok, _, runID, jobID := jobTokenPrincipal(t, e, "  issues: write")
	path := fmt.Sprintf("/o/r/api/issues/%d/comments", th.Num)
	if rec := doIssues(t, h, http.MethodPost, path, tok, `{"body":"live"}`); rec.Code != http.StatusCreated {
		t.Fatalf("live token comment = %d (%s)", rec.Code, rec.Body.String())
	}

	// Expiry: the credential is a signature check, before any store read.
	claims, perr := e.svc.parseAttemptToken(tok, e.svc.nowUTC())
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	later := time.Unix(claims.Exp+1, 0).UTC()
	if _, aerr, ok := e.svc.resolveCloneCredentialAt(ctx(), tok, later); !ok || aerr == nil {
		t.Fatalf("expired job token still resolved (ok=%v aerr=%v)", ok, aerr)
	}

	// Revocation: a terminal job's token is refused by the identity authority.
	if _, uerr := e.svc.UpdateJob(ctx(), "o/r", runID, jobID, JobUpdate{State: JobSuccess}); uerr != nil {
		t.Fatalf("complete job: %v", uerr)
	}
	if _, aerr, ok := e.svc.resolveCloneCredential(ctx(), tok); !ok || aerr == nil {
		t.Fatalf("terminal job's token still resolved (ok=%v aerr=%v)", ok, aerr)
	}
}
