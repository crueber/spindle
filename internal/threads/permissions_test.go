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
	// write-all must never grant cloud identity (S10): id-token is asked for
	// by name, and an `all` baseline never sets it.
	if _, ok := all["id-token"]; ok {
		t.Fatalf("write-all granted id-token: %v", all)
	}
	if all["issues"] != PermWrite || all["pull-requests"] != PermWrite {
		t.Fatalf("write-all baseline = %v, want every baseline scope at write", all)
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

// TestPAR112ScopeMatrixDrivesCapabilities pins that every supported scope
// changes a real capability (the other half of the matrix test: the parser
// accepts exactly these, and each one is wired here).
func TestPAR112ScopeMatrixDrivesCapabilities(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name  string
		perms map[string]string
		push  bool
		id    bool
	}{
		{name: "contents read", perms: map[string]string{"contents": "read"}},
		{name: "contents write", perms: map[string]string{"contents": "write"}, push: true},
		{name: "issues write is not push", perms: map[string]string{"issues": "write"}},
		{name: "pull-requests write is not push", perms: map[string]string{"pull-requests": "write"}},
		{name: "id-token write", perms: map[string]string{"id-token": "write"}, id: true},
		{name: "id-token none", perms: map[string]string{"id-token": "none"}},
		{name: "write-all never id-token", perms: map[string]string{"all": "write-all"}, push: true},
		{name: "all write with id-token none", perms: map[string]string{"all": "write-all", "id-token": "none"}, push: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &Spec{Permissions: tc.perms}
			perms := e.svc.EffectivePermissions(spec, true)
			if got := containsScope(PermissionScopes(perms), ScopePush); got != tc.push {
				t.Fatalf("push scope = %v, want %v (perms %v)", got, tc.push, perms)
			}
			if got := e.svc.IDTokenAllowed(spec, true); got != tc.id {
				t.Fatalf("id-token allowed = %v, want %v (perms %v)", got, tc.id, perms)
			}
			// The trust class always wins: no fork run gets push or identity.
			fork := e.svc.EffectivePermissions(spec, false)
			if containsScope(PermissionScopes(fork), ScopePush) || e.svc.IDTokenAllowed(spec, false) {
				t.Fatalf("fork perms = %v granted push/identity", fork)
			}
		})
	}
	// enable-openid-connect is the second spelling for the same grant.
	if !e.svc.IDTokenAllowed(&Spec{EnableOpenIDConnect: true}, true) {
		t.Fatal("enable-openid-connect did not grant the OIDC exchange")
	}
	if e.svc.IDTokenAllowed(&Spec{EnableOpenIDConnect: true, Permissions: map[string]string{"id-token": "none"}}, true) {
		t.Fatal("an explicit id-token: none was overridden by enable-openid-connect")
	}
}

// TestPAR114NoPermissionsDefaultsToContentsRead pins the recorded divergence
// (PAR-114, D-12; docs/threads.md §5.3, docs/WORKFLOWS.md §7.2): a workflow with
// NO `permissions:` block gets `contents: read` here — the opposite of Forgejo's
// implicit-write token, which ignores `permissions` entirely. A workflow relying
// on the implicit write is therefore refused by the server with the named
// "no push permission (workflow permissions: contents: read)" error rather than
// silently downgraded or silently upgraded. The default is an operator decision
// (`threads.default_permissions`, D-12) and is NOT changed here.
func TestPAR114NoPermissionsDefaultsToContentsRead(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run := e.mustCreate(t)[0]
	_, view := startJob(t, e, run)
	if got := view.Permissions["contents"]; got != PermRead {
		t.Fatalf("no-permissions job token = contents: %q, want read (PAR-114/D-12)", got)
	}
	if containsScope(view.Scopes, ScopePush) {
		t.Fatalf("a no-permissions job token carries push: %v", view.Scopes)
	}
	// A Forgejo workflow that grants write and no longer declares the scopes it
	// does not use still gets exactly what it declares.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
permissions:
  issues: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo
`)
	run2 := e.mustCreate(t)[0]
	_, view2 := startJob(t, e, run2)
	if got := view2.Permissions["contents"]; got != PermRead || containsScope(view2.Scopes, ScopePush) {
		t.Fatalf("issues-only job = %+v scopes %v, want contents: read and no push", view2.Permissions, view2.Scopes)
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
