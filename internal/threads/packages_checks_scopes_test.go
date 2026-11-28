package threads

// The packages/checks half of the job-token scope matrix (PAR-112, D-11),
// driven end to end through the REAL registries' HTTP surfaces: the wjt_
// attempt token is resolved by the same credential function the server chain
// uses, the OCI registry exchanges it at /v2/token for a repository-scoped
// registry token (packages: read = pull, write = pull+push), and the checks
// surface accepts a check run from `checks: write`. Cross-repository and
// missing-scope requests are real 403s.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
)

// jobSurfaceAuth resolves a wjt_ credential from the request exactly as the
// composition chain does (Bearer or the Basic password, the git shape).
func jobSurfaceAuth(e *testEnv) func(r *http.Request) (auth.Principal, *auth.AuthError) {
	return func(r *http.Request) (auth.Principal, *auth.AuthError) {
		tok := ""
		cred := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(strings.ToLower(cred), "bearer "):
			tok = strings.TrimSpace(cred[7:])
		case strings.HasPrefix(strings.ToLower(cred), "basic "):
			if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred[6:])); err == nil {
				if i := strings.IndexByte(string(raw), ':'); i >= 0 {
					tok = string(raw[i+1:])
				}
			}
		}
		if tok == "" {
			return auth.Anonymous(), nil
		}
		p, aerr, ok := e.svc.resolveCloneCredential(r.Context(), tok)
		if !ok {
			return auth.Anonymous(), nil
		}
		return p, aerr
	}
}

// scopesCommits resolves any full sha for the checks surface (the real
// CommitChecker is git; this proves the credential, not git).
type scopesCommits struct{}

func (scopesCommits) ResolveCommit(_ context.Context, _, sha string) (string, error) {
	return strings.ToLower(sha), nil
}

func doSurface(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.SetBasicAuth("ci", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func doBearer(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func doBearerBody(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

// TestE2E_ThreadsJobTokenPackagesAndChecks is the integration proof of the two
// scopes this change makes real.
func TestE2E_ThreadsJobTokenPackagesAndChecks(t *testing.T) {
	e := newTestEnv(t)
	authFn := jobSurfaceAuth(e)
	ids := identity.New(e.store, nil)

	psvc := packages.New(e.store, ids)
	psvc.TokenSecret = []byte("threads-scopes-test")
	psvc.MaxBlobBytes = 1 << 20
	psvc.MaxManifestBytes = 1 << 20
	psvc.SpoolDir = t.TempDir()
	ph := &packages.Handler{Svc: psvc, Auth: authFn, AuthRequired: true}

	csvc := checks.New(e.store, ids)
	csvc.Commits = scopesCommits{}
	ch := &checks.Handler{Svc: csvc, Auth: authFn}

	// imageManifest is a valid empty-layer OCI manifest (no blobs needed).
	imageManifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	tokenFor := func(tok, scope string) *httptest.ResponseRecorder {
		return doSurface(t, ph, http.MethodGet, "/v2/token?scope="+scope, tok, "")
	}
	minted := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Token == "" {
			t.Fatalf("token response = %d %s", rec.Code, rec.Body.String())
		}
		return body.Token
	}

	// packages: write pushes and pulls its own repository's image.
	writeTok, _, _, _ := jobTokenPrincipal(t, e, "  packages: write\n  checks: write")
	rec := tokenFor(writeTok, "repository:o/r:pull,push")
	if rec.Code != http.StatusOK {
		t.Fatalf("packages: write exchange = %d %s", rec.Code, rec.Body.String())
	}
	regTok := minted(t, rec)
	req := httptest.NewRequest(http.MethodPut, "/v2/o/r/manifests/ci", strings.NewReader(string(imageManifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Set("Authorization", "Bearer "+regTok)
	put := httptest.NewRecorder()
	ph.ServeHTTP(put, req)
	if put.Code != http.StatusCreated {
		t.Fatalf("packages: write push = %d %s", put.Code, put.Body.String())
	}
	if get := doBearer(t, ph, http.MethodGet, "/v2/o/r/manifests/ci", regTok); get.Code != http.StatusOK {
		t.Fatalf("packages: write pull = %d %s", get.Code, get.Body.String())
	}

	// checks: write creates a check run for its own repository.
	report := `{"context":"external/ci","state":"success","description":"green"}`
	if rec := doBearerBody(t, ch, http.MethodPost, "/o/r/api/checks/statuses/"+testSHA, writeTok, report); rec.Code != http.StatusOK {
		t.Fatalf("checks: write report = %d %s", rec.Code, rec.Body.String())
	}
	// checks: read reads it back.
	readChecksTok, _, _, _ := jobTokenPrincipal(t, e, "  checks: read")
	if rec := doBearer(t, ch, http.MethodGet, "/o/r/api/checks/statuses/"+testSHA, readChecksTok); rec.Code != http.StatusOK {
		t.Fatalf("checks: read = %d %s", rec.Code, rec.Body.String())
	}

	// packages: read pulls but never pushes.
	readTok, _, _, _ := jobTokenPrincipal(t, e, "  packages: read")
	if rec := tokenFor(readTok, "repository:o/r:pull,push"); rec.Code != http.StatusForbidden {
		t.Fatalf("packages: read push exchange = %d %s", rec.Code, rec.Body.String())
	}
	pullRec := tokenFor(readTok, "repository:o/r:pull")
	if pullRec.Code != http.StatusOK {
		t.Fatalf("packages: read exchange = %d %s", pullRec.Code, pullRec.Body.String())
	}
	pullTok := minted(t, pullRec)
	if get := doBearer(t, ph, http.MethodGet, "/v2/o/r/manifests/ci", pullTok); get.Code != http.StatusOK {
		t.Fatalf("packages: read pull = %d %s", get.Code, get.Body.String())
	}
	req = httptest.NewRequest(http.MethodPut, "/v2/o/r/manifests/readjob", strings.NewReader(string(imageManifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Set("Authorization", "Bearer "+pullTok)
	put = httptest.NewRecorder()
	ph.ServeHTTP(put, req)
	if put.Code != http.StatusForbidden {
		t.Fatalf("pull token pushed: %d", put.Code)
	}

	// Cross-repository is 403 (the job token names its own repository).
	if rec := tokenFor(writeTok, "repository:o/other:pull"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-repo package exchange = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doBearerBody(t, ch, http.MethodPost, "/o/other/api/checks/statuses/"+testSHA, writeTok, report); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-repo check report = %d %s", rec.Code, rec.Body.String())
	}

	// A job without the scopes is refused by name on both surfaces.
	noScopeTok, _, _, _ := jobTokenPrincipal(t, e, "  contents: read")
	if rec := tokenFor(noScopeTok, "repository:o/r:pull"); rec.Code != http.StatusForbidden {
		t.Fatalf("missing packages scope = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doBearerBody(t, ch, http.MethodPost, "/o/r/api/checks/statuses/"+testSHA, noScopeTok, report); rec.Code != http.StatusForbidden {
		t.Fatalf("missing checks scope = %d %s", rec.Code, rec.Body.String())
	}
	// The job token itself is never a registry bearer credential.
	if rec := doBearer(t, ph, http.MethodPut, "/v2/o/r/manifests/direct", writeTok); rec.Code != http.StatusForbidden {
		t.Fatalf("direct job token = %d %s", rec.Code, rec.Body.String())
	}
}
