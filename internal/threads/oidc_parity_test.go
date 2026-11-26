package threads

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// PAR-119: the Forgejo enable key is a second spelling for the same grant.
const oidcEnableKeyWorkflow = `name: EnableKey
on:
  push:
    branches: [main]
  pull_request:
jobs:
  deploy:
    runs-on: linux
    enable-openid-connect: true
    steps:
      - run: echo deploy
`

// PAR-119: an explicit denial wins over the boolean.
const oidcEnableKeyDeniedWorkflow = `name: EnableKeyDenied
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: linux
    enable-openid-connect: true
    permissions:
      id-token: none
    steps:
      - run: echo deploy
`

// PAR-120: an environment job carries the environment claim and subject.
const oidcEnvironmentWorkflow = `name: Env
on:
  push:
    branches: [main]
jobs:
  ship:
    runs-on: linux
    environment: production
    permissions:
      id-token: write
    steps:
      - run: echo ship
`

func TestPAR119EnableOpenIDConnectMintsToken(t *testing.T) {
	e, _ := oidcEnv(t, oidcEnableKeyWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-enablekey")
	if job.IDToken == nil {
		t.Fatal("enable-openid-connect: true job got no request token")
	}
	_, _, claims := exchangeOK(t, e, job, "https://cloud.example")
	if claims["sub"] != "repo:o/r:ref:refs/heads/main" || claims["repository"] != "o/r" {
		t.Fatalf("claims = %+v", claims)
	}
	if run.Trusted != true {
		t.Fatal("fixture run is not trusted")
	}
}

func TestPAR119ExplicitNoneBeatsEnableKey(t *testing.T) {
	e, _ := oidcEnv(t, oidcEnableKeyDeniedWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-enablekey-denied")
	if job.IDToken != nil {
		t.Fatalf("id-token: none job got a request token: %+v", job.IDToken)
	}
	req, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", run.ID, job.JobID, job.Attempt, 0, "https://spindle.example")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, xerr := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, req.RequestToken, "aud", "https://spindle.example")
	if !errors.Is(xerr, ErrForbidden) {
		t.Fatalf("exchange with id-token: none = %v, want ErrForbidden", xerr)
	}
}

func TestPAR119EnableKeyNeverMintsForAFork(t *testing.T) {
	e, _ := oidcEnv(t, oidcEnableKeyWorkflow)
	e.svc.ApproveFirstTimeForks = false
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/3/head",
		BaseBranch: "main", BaseCommit: testSHA, Fork: true, Actor: "guest",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("fork CreateRun = %v, %v", runs, err)
	}
	job := fetchOIDC(t, e, "rnr-enablekey-fork")
	if job.IDToken != nil {
		t.Fatalf("fork run got an ID token from enable-openid-connect: %+v", job.IDToken)
	}
}

func TestPAR120EnvironmentClaimAndSubject(t *testing.T) {
	e, _ := oidcEnv(t, oidcEnvironmentWorkflow)
	admin := auth.Principal{Name: "admin", Admin: true}
	if _, err := e.svc.PutEnvironment(ctx(), "o/r", "production", EnvironmentInput{}, admin); err != nil {
		t.Fatalf("PutEnvironment: %v", err)
	}
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-env")
	if job.IDToken == nil {
		t.Fatal("environment job got no request token")
	}
	_, _, claims := exchangeOK(t, e, job, "aud-env")
	if claims["environment"] != "production" {
		t.Fatalf("environment claim = %v", claims["environment"])
	}
	if claims["sub"] != "repo:o/r:environment:production" {
		t.Fatalf("sub = %v, want the environment subject", claims["sub"])
	}
	if claims["ref"] != "refs/heads/main" || claims["ref_type"] != "branch" {
		t.Fatalf("ref claims = %v / %v", claims["ref"], claims["ref_type"])
	}
	if got := len(claims); got != 21 {
		t.Fatalf("environment claim count = %d, want 21: %+v", got, claims)
	}
	_ = run
}

func TestPAR120AudienceQuerySpelling(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-queryaud")
	// GitHub's request URL carries the audience as a query parameter and the
	// body may be empty.
	target := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/id-token?audience=" + url.QueryEscape("https://sts.example")
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.Header.Set("Authorization", "Bearer "+job.IDToken.RequestToken)
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query-audience exchange = %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Value == "" {
		t.Fatalf("query-audience answer %q: %v", rec.Body.String(), err)
	}
	_, claims, _ := splitJWT(t, out.Value)
	if claims["aud"] != "https://sts.example" {
		t.Fatalf("aud = %v", claims["aud"])
	}
}

const oidcTagWorkflow = `name: Tag
on: [push]
jobs:
  build:
    runs-on: linux
    permissions:
      id-token: write
    steps:
      - run: echo tag
`

func TestPAR120TagRefTypeAndProtectedRef(t *testing.T) {
	e, _ := oidcEnv(t, oidcTagWorkflow)
	// A blanket protect rule on release tags makes ref_protected true.
	policy := `{"version":1,"rules":[{"name":"protect-tags","match":{"refs":["refs/tags/**"]},"effect":{"protect":{"restricts":["create","update","delete"]}}}]}`
	if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/policy.json", []byte(policy), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		Branch: "", Ref: "refs/tags/v1.2.3", Actor: "writer",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("tag CreateRun = %v, %v", runs, err)
	}
	job := fetchOIDC(t, e, "rnr-tag")
	if job.IDToken == nil {
		t.Fatal("tag job got no request token")
	}
	_, _, claims := exchangeOK(t, e, job, "aud-tag")
	if claims["ref_type"] != "tag" || claims["ref"] != "refs/tags/v1.2.3" {
		t.Fatalf("ref claims = %v / %v", claims["ref_type"], claims["ref"])
	}
	if claims["ref_protected"] != true {
		t.Fatalf("ref_protected = %v, want true under a blanket protect rule", claims["ref_protected"])
	}
	if claims["job_workflow_ref"] != "o/r/.spindle/workflows/ci.yml@refs/tags/v1.2.3" {
		t.Fatalf("job_workflow_ref = %v", claims["job_workflow_ref"])
	}
}

func TestPAR120BranchIsNotProtectedByATagRule(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	policy := `{"version":1,"rules":[{"name":"protect-tags","match":{"refs":["refs/tags/**"]},"effect":{"protect":{"restricts":["update"]}}}]}`
	if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/policy.json", []byte(policy), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-unprot")
	if job.IDToken == nil {
		t.Fatal("job got no request token")
	}
	_, _, claims := exchangeOK(t, e, job, "aud")
	if claims["ref_protected"] != false {
		t.Fatalf("ref_protected = %v on an unprotected branch", claims["ref_protected"])
	}
}
