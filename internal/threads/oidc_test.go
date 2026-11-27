package threads

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// OIDC workflow fixtures: two independent jobs so the scoping tests can hold
// two request tokens at once.
const oidcWorkflow = `name: OIDC
on:
  push:
    branches: [main]
  pull_request:
jobs:
  cloud:
    runs-on: linux
    permissions:
      id-token: write
    steps:
      - run: echo cloud
  other:
    runs-on: linux
    permissions:
      id-token: write
    steps:
      - run: echo other
`

const oidcNoPermissionWorkflow = `name: Plain
on:
  push:
    branches: [main]
jobs:
  plain:
    runs-on: linux
    permissions:
      contents: read
    steps:
      - run: echo plain
`

// oidcEnv prepares an environment whose only run uses the given workflow and a
// fixed clock (the OIDC window/expiry assertions need control over now).
func oidcEnv(t *testing.T, yaml string) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", yaml)
	fixed := e.now
	e.svc.Now = func() time.Time { return fixed }
	e.svc.CheckoutBaseURL = "https://spindle.example"
	return e, &fixed
}

// oidcRun creates the push run and returns it.
func oidcRun(t *testing.T, e *testEnv) *Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun = %v, %v", runs, err)
	}
	return runs[0]
}

// fetchOIDC claims one job and returns its payload.
func fetchOIDC(t *testing.T, e *testEnv, runnerID string) *FetchResult {
	t.Helper()
	a := &runnerAuth{Doc: &RunnerDoc{ID: runnerID, Labels: []string{"linux"}, AllowAll: true, State: RunnerActive, AllowForkRuns: true}}
	job, err := e.svc.FetchJobFrom(ctx(), a, time.Millisecond, "https://spindle.example")
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	return job
}

// oidcHandler is the HTTP surface (Auth nil -> anonymous; these routes never
// consult a session).
func oidcHandler(e *testEnv) *Handler { return &Handler{Svc: e.svc} }

// postExchange drives the exchange endpoint.
func postExchange(t *testing.T, e *testEnv, job *FetchResult, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/threads/jobs/"+job.JobRef+"/attempt/"+strconv.Itoa(job.Attempt)+"/id-token", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, req)
	return rec
}

// fetchJWKS reads the JWKS through the HTTP surface.
func fetchJWKS(t *testing.T, e *testEnv) (body []byte, keys []oidcJWK) {
	t.Helper()
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("jwks status %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.Bytes()
	var doc struct {
		Keys []oidcJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("jwks json: %v", err)
	}
	return body, doc.Keys
}

// splitJWT decodes a compact JWT's header and claims and returns the signature.
func splitJWT(t *testing.T, token string) (map[string]any, map[string]any, []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("decode segment: %v", err)
		}
		return b
	}
	var header, claims map[string]any
	if err := json.Unmarshal(dec(parts[0]), &header); err != nil {
		t.Fatalf("header json: %v", err)
	}
	if err := json.Unmarshal(dec(parts[1]), &claims); err != nil {
		t.Fatalf("claims json: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature: %v", err)
	}
	return header, claims, sig
}

// verifyWithJWKS proves the signature against the served public key set.
func verifyWithJWKS(t *testing.T, token, kid string, keys []oidcJWK) {
	t.Helper()
	var jwk *oidcJWK
	for i := range keys {
		if keys[i].KID == kid {
			jwk = &keys[i]
			break
		}
	}
	if jwk == nil {
		t.Fatalf("kid %q is not in the JWKS", kid)
	}
	nb, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		t.Fatalf("n: %v", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		t.Fatalf("e: %v", err)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	parts := strings.Split(token, ".")
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify with kid %q: %v", kid, err)
	}
}

// exchangeOK drives the exchange and returns the token + decoded header/claims.
func exchangeOK(t *testing.T, e *testEnv, job *FetchResult, audience string) (string, map[string]any, map[string]any) {
	t.Helper()
	if job.IDToken == nil {
		t.Fatal("job payload carries no ID token request")
	}
	rec := postExchange(t, e, job, job.IDToken.RequestToken, `{"audience":"`+audience+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Value == "" {
		t.Fatalf("exchange body %q: %v", rec.Body.String(), err)
	}
	h, c, _ := splitJWT(t, out.Value)
	return out.Value, h, c
}

func TestOIDCDiscoveryAndJWKS(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	h := oidcHandler(e)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery status %d: %s", rec.Code, rec.Body.String())
	}
	var doc oidcDiscovery
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery json: %v", err)
	}
	if doc.Issuer != "https://spindle.example" {
		t.Fatalf("issuer = %q", doc.Issuer)
	}
	if doc.JWKSURI != "https://spindle.example/.well-known/jwks.json" {
		t.Fatalf("jwks_uri = %q", doc.JWKSURI)
	}
	if len(doc.IDTokenSigningAlgValuesSupported) != 1 || doc.IDTokenSigningAlgValuesSupported[0] != "RS256" {
		t.Fatalf("alg values = %v", doc.IDTokenSigningAlgValuesSupported)
	}
	for _, want := range []string{"repository", "sub", "jti", "run_attempt"} {
		found := false
		for _, c := range doc.ClaimsSupported {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("claims_supported lacks %q: %v", want, doc.ClaimsSupported)
		}
	}

	body, keys := fetchJWKS(t, e)
	if len(keys) != 1 {
		t.Fatalf("jwks keys = %d", len(keys))
	}
	k := keys[0]
	if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" || k.KID == "" || k.N == "" || k.E == "" {
		t.Fatalf("jwk = %+v", k)
	}
	// The public document must never carry private material.
	if bytes.Contains(body, []byte("PRIVATE")) || bytes.Contains(body, []byte(`"d"`)) {
		t.Fatalf("jwks leaked private key material: %s", body)
	}
	// Unknown well-known paths fall through (false), and non-GET is refused.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown well-known = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST jwks = %d", rec.Code)
	}
	if cc := rec.Header().Get("Allow"); cc != "GET" {
		t.Fatalf("Allow = %q", cc)
	}
}

func TestOIDCExchangeClaimsAndSignature(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-1")
	if job.IDToken == nil {
		t.Fatal("id-token: write job got no request token")
	}
	if !strings.HasPrefix(job.IDToken.RequestToken, idTokenRequestPrefix) {
		t.Fatalf("request token = %q", job.IDToken.RequestToken)
	}
	if !strings.HasPrefix(job.IDToken.RequestURL, "https://spindle.example/api/v1/threads/jobs/") ||
		!strings.HasSuffix(job.IDToken.RequestURL, "/attempt/1/id-token") {
		t.Fatalf("request URL = %q", job.IDToken.RequestURL)
	}
	// Hash-only storage: the record must not contain the plaintext.
	raw, _, err := e.svc.getJSON(ctx(), IDTokenRequestKey("o", "r", run.ID, job.JobID, job.Attempt))
	if err != nil || raw == nil {
		t.Fatalf("request record = %s, %v", raw, err)
	}
	if bytes.Contains(raw, []byte(job.IDToken.RequestToken)) {
		t.Fatal("request-token record stored the plaintext")
	}

	token, header, claims := exchangeOK(t, e, job, "https://cloud.example")
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %+v", header)
	}
	kid, _ := header["kid"].(string)
	_, keys := fetchJWKS(t, e)
	verifyWithJWKS(t, token, kid, keys)

	rec, _, lerr := e.svc.loadJobAndRun(ctx(), "o", "r", run.ID, job.JobID)
	if lerr != nil || rec == nil {
		t.Fatalf("load job: %v", lerr)
	}

	want := map[string]any{
		"iss":              "https://spindle.example",
		"sub":              "repo:o/r:ref:refs/heads/main",
		"aud":              "https://cloud.example",
		"repository":       "o/r",
		"repository_owner": "o",
		"ref":              "refs/heads/main",
		"ref_type":         "branch",
		"ref_protected":    false,
		"sha":              testSHA,
		"workflow":         "OIDC",
		"job":              rec.Key,
		"run_id":           run.ID,
		"event_name":       "push",
		// PAR-120 additions: the claim set is pinned, and the environment
		// claim is absent on a job that names no environment.
		"job_workflow_ref":   "o/r/.spindle/workflows/ci.yml@refs/heads/main",
		"runner_environment": "self-hosted",
	}
	for k, v := range want {
		if claims[k] != v {
			t.Fatalf("claim %s = %#v, want %#v (all: %+v)", k, claims[k], v, claims)
		}
	}
	if _, found := claims["environment"]; found {
		t.Fatalf("environment claim present on a job without an environment: %+v", claims)
	}
	if got := len(claims); got != 20 {
		t.Fatalf("claim count = %d, want the exact 20-claim set: %+v", got, claims)
	}
	exp, _ := claims["exp"].(float64)
	iat, _ := claims["iat"].(float64)
	nbf, _ := claims["nbf"].(float64)
	if int64(exp-iat) != int64(oidcTokenTTL/time.Second) || nbf != iat {
		t.Fatalf("times: iat=%v nbf=%v exp=%v", iat, nbf, exp)
	}
	if claims["jti"] == "" || claims["jti"] == nil {
		t.Fatalf("jti = %v", claims["jti"])
	}
	if claims["run_attempt"] != float64(1) {
		t.Fatalf("run_attempt = %v", claims["run_attempt"])
	}
	// A second exchange mints a distinct jti with the same identity.
	_, _, claims2 := exchangeOK(t, e, job, "https://cloud.example")
	if claims2["jti"] == claims["jti"] {
		t.Fatal("two exchanges reused a jti")
	}
}

func TestOIDCPermissionOffGetsNoToken(t *testing.T) {
	e, _ := oidcEnv(t, oidcNoPermissionWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-2")
	if job.IDToken != nil {
		t.Fatalf("job without id-token: write got a request token: %+v", job.IDToken)
	}
	// Defence in depth: even a request-token record (a bug or a stale object)
	// cannot exchange without the effective permission.
	tokReq, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", run.ID, job.JobID, job.Attempt, time.Hour, "https://spindle.example")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ref := JobRef("o/r", run.ID, job.JobID)
	_, xerr := e.svc.ExchangeIDToken(ctx(), ref, job.Attempt, tokReq.RequestToken, "aud", "https://spindle.example")
	if !errors.Is(xerr, ErrForbidden) {
		t.Fatalf("exchange without permission = %v, want ErrForbidden", xerr)
	}
}

func TestOIDCForkRunDenied(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	e.svc.ApproveFirstTimeForks = false // the token gate is what is under test
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/1/head",
		BaseBranch: "main", BaseCommit: testSHA, Fork: true, Actor: "guest",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("fork CreateRun = %v, %v", runs, err)
	}
	run := runs[0]
	job := fetchOIDC(t, e, "rnr-oidc-3")
	if job.Trusted {
		t.Fatal("fork run fetched as trusted")
	}
	if job.IDToken != nil {
		t.Fatalf("fork run got a request token: %+v", job.IDToken)
	}
	req, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", run.ID, job.JobID, job.Attempt, time.Hour, "https://spindle.example")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, xerr := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, req.RequestToken, "aud", "https://spindle.example")
	if !errors.Is(xerr, ErrForbidden) {
		t.Fatalf("fork exchange = %v, want ErrForbidden", xerr)
	}
}

func TestOIDCAudienceRequired(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-4")
	for _, body := range []string{`{"audience":""}`, `{"audience":"   "}`, `{}`, `{"audience":"a\nb"}`} {
		rec := postExchange(t, e, job, job.IDToken.RequestToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if rec := postExchange(t, e, job, job.IDToken.RequestToken, `{"audience":"a","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", rec.Code)
	}
}

func TestOIDCRequestTokenScopedToOneJob(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	jobA := fetchOIDC(t, e, "rnr-oidc-5a")
	jobB := fetchOIDC(t, e, "rnr-oidc-5b")
	if jobA.JobID == jobB.JobID {
		t.Fatal("fixture claimed the same job twice")
	}
	// A's token against B's path must not resolve.
	rec := postExchange(t, e, jobB, jobA.IDToken.RequestToken, `{"audience":"aud"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-job exchange = %d (%s)", rec.Code, rec.Body.String())
	}
	// A's token against B's path but A's identity would still not resolve: the
	// record is keyed by path. Same token on its own path works.
	rec = postExchange(t, e, jobA, jobA.IDToken.RequestToken, `{"audience":"aud"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("own exchange = %d (%s)", rec.Code, rec.Body.String())
	}
	// The job token is a different credential and is refused with a hint.
	rec = postExchange(t, e, jobA, "wjt_abc.def", `{"audience":"aud"}`)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "request token") {
		t.Fatalf("job-token exchange = %d (%s)", rec.Code, rec.Body.String())
	}
	// No credential at all.
	rec = postExchange(t, e, jobA, "", `{"audience":"aud"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous exchange = %d", rec.Code)
	}
}

func TestOIDCRequestTokenExpiresAndRevokes(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-6")
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	// Past the request token's lifetime. The attempt TTL follows the job
	// timeout (PAR-055: the 360-minute default), so it is 6h10m here.
	*now = now.Add(7 * time.Hour)
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired exchange = %v, want ErrUnauthorized", err)
	}
	// A finished job revokes its token even inside the TTL.
	*now = now.Add(-7 * time.Hour)
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.JobID, JobUpdate{State: JobSuccess}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("finished-job exchange = %v, want ErrForbidden", err)
	}
}

func TestOIDCSigningKeyRotationKeepsOldTokensVerifiable(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-7")
	token1, header1, _ := exchangeOK(t, e, job, "aud")
	kid1, _ := header1["kid"].(string)

	// Age the active key past the rotation threshold in the bucket.
	raw, _, err := e.svc.getJSON(ctx(), OIDCSigningKeyPath())
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	var doc oidcKeyDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		t.Fatalf("key json: %v", jerr)
	}
	doc.Keys[0].CreatedAt = now.Add(-oidcRotationAge - time.Hour).Format(dateTimeFmt)
	if _, perr := store.PutBytes(ctx(), e.svc.Store, OIDCSigningKeyPath(), mustJSON(&doc),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); perr != nil {
		t.Fatalf("age key: %v", perr)
	}

	// The key cache is warm; a peer-visible change is bounded by its TTL, so
	// advance past the TTL to force the store read (and thus the rotation).
	*now = now.Add(oidcKeyCacheTTL + time.Second)
	token2, header2, _ := exchangeOK(t, e, job, "aud")
	kid2, _ := header2["kid"].(string)
	if kid1 == kid2 {
		t.Fatalf("rotation reused kid %q", kid1)
	}
	_, keys := fetchJWKS(t, e)
	if len(keys) != 2 {
		t.Fatalf("jwks after rotation has %d keys, want old+new", len(keys))
	}
	verifyWithJWKS(t, token1, kid1, keys)
	verifyWithJWKS(t, token2, kid2, keys)

	// A second read does not rotate again.
	token3, header3, _ := exchangeOK(t, e, job, "aud")
	if header3["kid"] != kid2 {
		t.Fatalf("second rotation: %v -> %v", kid2, header3["kid"])
	}
	verifyWithJWKS(t, token3, kid2, keys)
}

func TestOIDCSigningKeyCASRace(t *testing.T) {
	mem := store.NewMemory()
	a := New(mem, nil)
	b := New(mem, nil)
	const n = 6
	kids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			svc := a
			if i%2 == 1 {
				svc = b
			}
			set, err := svc.oidcKeySet(ctx())
			if err != nil {
				errs[i] = err
				return
			}
			kids[i] = set.activeKID
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
	}
	for i := 1; i < n; i++ {
		if kids[i] != kids[0] {
			t.Fatalf("instances disagree on the active kid: %v", kids)
		}
	}
	raw, _, err := a.getJSON(ctx(), OIDCSigningKeyPath())
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	var doc oidcKeyDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		t.Fatalf("key json: %v", jerr)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("CAS race left %d keys (a second trust root): %+v", len(doc.Keys), doc)
	}
}

func TestOIDCRateLimitPerJob(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-8")
	for i := 0; i < idTokenExchangeLimit; i++ {
		if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); err != nil {
			t.Fatalf("exchange %d: %v", i+1, err)
		}
	}
	_, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("over-budget exchange = %v, want ErrRateLimited", err)
	}
	// The handler maps it to 429 + Retry-After.
	rec := postExchange(t, e, job, job.IDToken.RequestToken, `{"audience":"aud"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limit HTTP = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	*now = now.Add(idTokenExchangeWindow + time.Second)
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); err != nil {
		t.Fatalf("exchange after window: %v", err)
	}
}

func TestOIDCUnknownOrRevokedTokenFailsClosed(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-9")
	// A well-formed but unknown token.
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, idTokenRequestPrefix+"deadbeef", "aud", "https://spindle.example"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown token = %v", err)
	}
	// An attempt that does not exist (no record).
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt+1, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong attempt = %v", err)
	}
	// A malformed job reference.
	if _, err := e.svc.ExchangeIDToken(ctx(), "not-a-ref", 1, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad ref = %v", err)
	}
	// The signing key material never appears in any response or payload.
	if bytes.Contains(mustJSON(job), []byte("PRIVATE")) {
		t.Fatal("job payload leaked key material")
	}
	body, _ := fetchJWKS(t, e)
	if bytes.Contains(body, []byte("private_key_pem")) {
		t.Fatal("JWKS leaked the private key field name")
	}
}

func TestOIDCExchangeRequiresStore(t *testing.T) {
	h := &Handler{Svc: &Service{}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired jwks = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/threads/jobs/x/attempt/1/id-token", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired exchange = %d", rec.Code)
	}
}

// TestOIDCSameRepoPullRequestUsesPullRequestSubject pins the PR subject
// (PAR-120): `repo:<o>/<r>:pull_request` — a provider can scope a policy to PR
// runs without trusting the PR head's ref — while the `ref`/`sha` claims stay
// the base branch the run was built against (matching GITHUB_REF).
func TestOIDCSameRepoPullRequestUsesPullRequestSubject(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/7/head", BaseBranch: "main", Actor: "writer",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun = %v, %v", runs, err)
	}
	job := fetchOIDC(t, e, "rnr-oidc-pr")
	if !job.Trusted || job.IDToken == nil {
		t.Fatalf("same-repo PR payload = trusted %v, id-token %v", job.Trusted, job.IDToken)
	}
	_, _, claims := exchangeOK(t, e, job, "aud")
	if claims["ref"] != "refs/heads/main" || claims["sub"] != "repo:o/r:pull_request" {
		t.Fatalf("PR ref = %v / %v, want the base branch ref and the pull_request subject", claims["ref"], claims["sub"])
	}
	if claims["event_name"] != "pull_request" {
		t.Fatalf("event_name = %v", claims["event_name"])
	}
}

func TestOIDCRetentionDropsRequestRecords(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	payload := fetchOIDC(t, e, "rnr-oidc-gc")
	key := IDTokenRequestKey("o", "r", run.ID, payload.JobID, payload.Attempt)
	if raw, _, err := e.svc.getJSON(ctx(), key); err != nil || raw == nil {
		t.Fatalf("request record missing before delete: %v", err)
	}
	// The retention pass reclaims the whole run prefix, id-token request
	// records included (one LIST, not a per-job family walk).
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err != nil {
		t.Fatalf("deleteRun: %v", err)
	}
	if raw, _, err := e.svc.getJSON(ctx(), key); err != nil || raw != nil {
		t.Fatalf("request record survived retention: %s, %v", raw, err)
	}
}
