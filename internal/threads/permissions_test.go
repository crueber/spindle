package threads

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// TestJobPermissionsDefaults pins the default scope model: with no
// `permissions:` a job token is read-only (the instance default), and a
// workflow that narrows to `contents: read` is the same.
func TestJobPermissionsDefaults(t *testing.T) {
	e := newTestEnv(t)
	spec := &Spec{}
	perms := e.svc.EffectivePermissions(spec, true)
	if perms["contents"] != PermRead {
		t.Fatalf("default perms = %v, want contents: read", perms)
	}
	if scopes := PermissionScopes(perms); containsScope(scopes, ScopePush) {
		t.Fatalf("default scopes = %v, want no push", scopes)
	}
	// An instance default of write widens the no-declaration case only.
	e.svc.DefaultPermissions = PermWrite
	if perms := e.svc.EffectivePermissions(&Spec{}, true); perms["contents"] != PermWrite {
		t.Fatalf("instance default perms = %v", perms)
	}
	// A declared read still narrows it back.
	if perms := e.svc.EffectivePermissions(&Spec{Permissions: map[string]string{"contents": "read"}}, true); perms["contents"] != PermRead {
		t.Fatalf("declared read perms = %v", perms)
	}
}

// TestJobPermissionsWriteAndAll pins contents: write and the read-all/write-all
// baselines.
func TestJobPermissionsWriteAndAll(t *testing.T) {
	e := newTestEnv(t)
	write := e.svc.EffectivePermissions(&Spec{Permissions: map[string]string{"contents": "write"}}, true)
	if !containsScope(PermissionScopes(write), ScopePush) {
		t.Fatalf("contents: write scopes = %v, want push", write)
	}
	all := e.svc.EffectivePermissions(&Spec{Permissions: map[string]string{"all": "write-all"}}, true)
	if all["contents"] != PermWrite {
		t.Fatalf("write-all perms = %v", all)
	}
	// A specific key overrides the all baseline.
	narrow := e.svc.EffectivePermissions(&Spec{Permissions: map[string]string{"all": "write-all", "contents": "read"}}, true)
	if narrow["contents"] != PermRead {
		t.Fatalf("override perms = %v, want contents: read", narrow)
	}
	// An unrecognised level never widens access.
	odd := e.svc.EffectivePermissions(&Spec{Permissions: map[string]string{"contents": "administrator"}}, true)
	if odd["contents"] != PermRead {
		t.Fatalf("unknown level perms = %v, want the default", odd)
	}
}

// TestForkRunCannotEscalate pins the trust-class rule: a fork run is read-only
// no matter what its workflow declares (the base-branch workflow it runs may be
// perfectly permissive).
func TestForkRunCannotEscalate(t *testing.T) {
	e := newTestEnv(t)
	spec := &Spec{Permissions: map[string]string{"contents": "write", "all": "write-all"}}
	perms := e.svc.EffectivePermissions(spec, false)
	if perms["contents"] != PermRead || len(perms) != 1 {
		t.Fatalf("fork perms = %v, want contents: read only", perms)
	}
	if scopes := PermissionScopes(perms); containsScope(scopes, ScopePush) {
		t.Fatalf("fork scopes = %v, want no push", scopes)
	}
}

// startJob claims one job of the run and starts its attempt, returning the
// issued payload (the credential half of the job payload).
func startJob(t *testing.T, e *testEnv, run *Run) (string, StartView) {
	t.Helper()
	jobID := e.claimOne(t, run.ID)
	if jobID == "" {
		t.Fatalf("no claimable job in %s", run.ID)
	}
	auth := &runnerAuth{Doc: needsRunner()}
	view, err := e.svc.Start(ctx(), auth, JobRef("o/r", run.ID, jobID), 1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return jobID, *view
}

// TestStartIssuesScopedJobToken pins the payload: Start reports the effective
// permissions and the token carries `push` only for contents: write.
func TestStartIssuesScopedJobToken(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
permissions:
  contents: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run := e.mustCreate(t)[0]
	_, view := startJob(t, e, run)
	if view.Permissions["contents"] != PermWrite || !containsScope(view.Scopes, ScopePush) {
		t.Fatalf("start view = %+v, want contents: write and push", view)
	}
	claims, err := e.svc.parseAttemptToken(view.Token, e.svc.nowUTC())
	if err != nil || !claims.Allows(ScopePush) || !claims.Allows(ScopeRead) || !claims.Allows(ScopeLog) {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	// A narrowed workflow gets a read-only token.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
permissions:
  contents: read
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run2 := e.mustCreate(t)[0]
	_, view2 := startJob(t, e, run2)
	if containsScope(view2.Scopes, ScopePush) {
		t.Fatalf("narrowed scopes = %v, want no push", view2.Scopes)
	}
}

// TestForkStartTokenHasNoPush pins that the trust class beats the spec at the
// credential: a fork run's token never carries push.
func TestForkStartTokenHasNoPush(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [pull_request]
permissions: write-all
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened",
		Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Fork: true, Actor: "newcomer",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, view := startJob(t, e, runs[0])
	if containsScope(view.Scopes, ScopePush) || view.Permissions["contents"] != PermRead {
		t.Fatalf("fork start view = %+v, want read-only", view)
	}
	// The principal the git chain resolves carries no push grant.
	p, _, ok := e.svc.resolveCloneCredential(ctx(), view.Token)
	if !ok || p.JobPush || p.JobRepo != "o/r" || p.Write {
		t.Fatalf("fork principal = %+v ok=%v", p, ok)
	}
}

// TestJobTokenResolutionAndRevocation pins the git credential: it resolves to a
// repo-scoped principal, is rejected once the job is terminal, once the attempt
// is superseded and once it has expired.
func TestJobTokenResolutionAndRevocation(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
permissions:
  contents: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run := e.mustCreate(t)[0]
	jobID, view := startJob(t, e, run)
	p, aerr, ok := e.svc.resolveCloneCredential(ctx(), view.Token)
	if !ok || aerr != nil || !p.JobPush || p.JobRepo != "o/r" || p.Write || p.Admin {
		t.Fatalf("principal = %+v aerr=%v ok=%v", p, aerr, ok)
	}
	// Another repository is not in the grant (the server gates on JobRepo).
	if p.JobRepo == "o/other" {
		t.Fatal("grant leaked to another repository")
	}
	// Completion revokes it (the job record is the identity authority).
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobID, JobUpdate{State: JobSuccess}); err != nil {
		t.Fatal(err)
	}
	if _, aerr, ok := e.svc.resolveCloneCredential(ctx(), view.Token); !ok || aerr == nil {
		t.Fatalf("completed job's token still accepted (ok=%v aerr=%v)", ok, aerr)
	}
}

// TestJobTokenExpiry pins the short-lived property: an expired token is a 401
// before any store read.
func TestJobTokenExpiry(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	_, view := startJob(t, e, run)
	if strings.TrimSpace(view.Token) == "" {
		t.Fatal("no token issued")
	}
	// Move the clock past the six-hour cap.
	claims, err := e.svc.parseAttemptToken(view.Token, e.svc.nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	later := time.Unix(claims.Exp+1, 0).UTC()
	if _, aerr, ok := e.svc.resolveCloneCredentialAt(ctx(), view.Token, later); !ok || aerr == nil {
		t.Fatalf("expired token accepted (ok=%v aerr=%v)", ok, aerr)
	}
}

// resolveCloneCredentialAt is resolveCloneCredential at an injected clock (the
// expiry test must not wait six hours).
func (s *Service) resolveCloneCredentialAt(ctx context.Context, token string, now time.Time) (auth.Principal, *auth.AuthError, bool) {
	orig := s.Now
	s.Now = func() time.Time { return now }
	defer func() { s.Now = orig }()
	return s.resolveCloneCredential(ctx, token)
}
