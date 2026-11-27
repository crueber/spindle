package threads

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// The artifact routes on both lanes (15 §2.5): the attempt-scoped job channel
// (upload, finalize, manifest, blob) and the repository read lane (list).

// artifactToken mints a valid attempt token for the run's first job.
func artifactToken(t *testing.T, e *testEnv, run *Run) (string, AttemptClaims) {
	t.Helper()
	claims := e.artifactClaims(t, run)
	tok, _, err := e.svc.mintAttemptToken(claims)
	if err != nil {
		t.Fatalf("mint attempt token: %v", err)
	}
	return tok, claims
}

// artifactLane renders the attempt-scoped artifact base path.
func artifactLane(claims AttemptClaims) string {
	return "/api/v1/threads/jobs/" + JobRef("o/r", claims.Run, claims.Job) +
		"/attempt/" + fmt.Sprint(claims.Attempt) + "/artifacts"
}

func TestHTTPArtifactUploadFlow(t *testing.T) {
	e, h, cur := newHTTP(t)
	run := e.mustCreate(t)[0]
	*cur = e.writer()
	tok, claims := artifactToken(t, e, run)
	base := artifactLane(claims)

	// A blob PUT with ?path= and ?size=.
	rec := doAuth(h, http.MethodPut, base+"/build/blobs/0?path=dist%2Fa.txt&size=5", "alpha", tok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("blob put = %d: %s", rec.Code, rec.Body.String())
	}
	var blobView ArtifactBlobView
	if err := json.Unmarshal(rec.Body.Bytes(), &blobView); err != nil || blobView.Index != 0 || blobView.Size != 5 || blobView.SHA256 == "" {
		t.Fatalf("blob answer = %+v (%v)", blobView, err)
	}
	rec = doAuth(h, http.MethodPut, base+"/build/blobs/1?path=dist%2Fb.txt&size=4", "beta", tok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second blob = %d: %s", rec.Code, rec.Body.String())
	}
	// Finalize.
	body := `{"name":"build","files":[{"path":"dist/a.txt","size":5,"index":0},{"path":"dist/b.txt","size":4,"index":1}],"retention_days":0}`
	rec = doAuth(h, http.MethodPost, base, body, tok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("finalize = %d: %s", rec.Code, rec.Body.String())
	}
	var view ArtifactView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.Name != "build" || len(view.Files) != 2 || view.ExpiresAt == "" {
		t.Fatalf("finalize answer = %+v (%v)", view, err)
	}
	// A second finalize of the same name is a conflict through the handler.
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "other")
	rec = doAuth(h, http.MethodPost, base, `{"name":"build","files":[{"path":"dist/a.txt","size":5,"index":0}]}`, tok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate finalize = %d: %s", rec.Code, rec.Body.String())
	}
	// Unknown body keys are refused (fail closed).
	rec = doAuth(h, http.MethodPost, base, `{"name":"x","files":[],"bogus":true}`, tok)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("unknown finalize key = %d: %s", rec.Code, rec.Body.String())
	}
	// The manifest read.
	rec = doAuth(h, http.MethodGet, base+"/build", "", tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest = %d: %s", rec.Code, rec.Body.String())
	}
	var man Artifact
	if err := json.Unmarshal(rec.Body.Bytes(), &man); err != nil || man.Name != "build" || len(man.Files) != 2 {
		t.Fatalf("manifest answer = %+v (%v)", man, err)
	}
	// A blob read carries the bytes, the recorded length and the path header.
	rec = doAuth(h, http.MethodGet, base+"/build/blobs/1", "", tok)
	if rec.Code != http.StatusOK || rec.Body.String() != "beta" {
		t.Fatalf("blob read = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "4" || rec.Header().Get("X-Spindle-Artifact-Path") != "dist/b.txt" {
		t.Fatalf("blob headers = %v", rec.Header())
	}
	// A blob index outside the manifest is not a bucket read.
	if rec := doAuth(h, http.MethodGet, base+"/build/blobs/9", "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted blob = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, base+"/build/blobs/notanumber", "", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad blob index = %d", rec.Code)
	}
	// The reader list on both lanes.
	for _, lane := range []string{"/o/r/api/threads/", "/o/r/api-browser/threads/"} {
		rec := doAuth(h, http.MethodGet, lane+run.ID+"/artifacts", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s list = %d: %s", lane, rec.Code, rec.Body.String())
		}
		var list struct {
			Artifacts []*ArtifactView `json:"artifacts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Artifacts) != 1 || list.Artifacts[0].TotalSize != 9 {
			t.Fatalf("%s list = %+v (%v)", lane, list, err)
		}
	}
	// The run detail carries the artifacts too.
	rec = doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID, "", "")
	var runView RunView
	if err := json.Unmarshal(rec.Body.Bytes(), &runView); err != nil || len(runView.Artifacts) != 1 {
		t.Fatalf("run view artifacts = %+v (%v)", runView.Artifacts, err)
	}
}

func TestHTTPArtifactRouteErrors(t *testing.T) {
	e, h, cur := newHTTP(t)
	run := e.mustCreate(t)[0]
	*cur = e.writer()
	tok, claims := artifactToken(t, e, run)
	base := artifactLane(claims)

	// Authentication/authorisation: no token, another job's token, the wrong path.
	cases := []struct {
		name, method, path, body, token string
		want                            int
	}{
		{"no token", http.MethodPut, base + "/build/blobs/0?path=a&size=1", "x", "", http.StatusUnauthorized},
		{"put bad index", http.MethodPut, base + "/build/blobs/zero?path=a&size=1", "x", tok, http.StatusBadRequest},
		{"put bad size", http.MethodPut, base + "/build/blobs/0?path=a&size=no", "x", tok, http.StatusBadRequest},
		{"put negative size", http.MethodPut, base + "/build/blobs/0?path=a&size=-1", "", tok, http.StatusBadRequest},
		{"put size mismatch", http.MethodPut, base + "/build/blobs/0?path=a&size=4", "x", tok, http.StatusBadRequest},
		{"put over file cap", http.MethodPut, base + "/build/blobs/0?path=a&size=999999999", "x", tok, http.StatusRequestEntityTooLarge},
		{"put bad path", http.MethodPut, base + "/build/blobs/0?path=%2Fetc%2Fpasswd&size=1", "x", tok, http.StatusBadRequest},
		{"finalize no files", http.MethodPost, base, `{"name":"x"}`, tok, http.StatusBadRequest},
		{"finalize bad json", http.MethodPost, base, `{`, tok, http.StatusBadRequest},
		{"manifest unknown", http.MethodGet, base + "/nope", "", tok, http.StatusNotFound},
		{"manifest wrong method", http.MethodPost, base + "/build", "", tok, http.StatusMethodNotAllowed},
		{"finalize wrong method", http.MethodPut, base, "", tok, http.StatusMethodNotAllowed},
		{"unknown subroute", http.MethodGet, base + "/build/other", "", tok, http.StatusNotFound},
		{"unknown subroute deep", http.MethodGet, base + "/build/blobs/0/extra/more", "", tok, http.StatusNotFound},
		{"list unknown run", http.MethodGet, "/o/r/api/threads/0000000000000000001-abcdef/artifacts", "", "", http.StatusNotFound},
		{"list wrong method", http.MethodPost, "/o/r/api/threads/" + run.ID + "/artifacts", "", "", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		rec := doAuth(h, tc.method, tc.path, tc.body, tc.token)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// Another run's attempt token cannot name this run's artifact.
	other := e.mustCreate(t)[0]
	otherTok, otherClaims := artifactToken(t, e, other)
	rec := doAuth(h, http.MethodGet, base+"/build", "", otherTok)
	if rec.Code == http.StatusOK {
		t.Fatalf("a foreign attempt token read an artifact (%v)", otherClaims)
	}
	// The commit path and a blob body that ends early: both named failures.
	e.uploadFile(t, claims, "short", 0, "s.txt", "abc")
	rec = doAuth(h, http.MethodPost, base, `{"name":"short","files":[{"path":"s.txt","size":99,"index":0}]}`, tok)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "the manifest says") {
		t.Fatalf("short blob = %d: %s", rec.Code, rec.Body.String())
	}
	// A blob PUT whose body length disagrees with ?size= is refused before the
	// store is touched.
	rec = doAuth(h, http.MethodPut, base+"/short/blobs/5?path=x&size=10", "abc", tok)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "body length") {
		t.Fatalf("short body = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPArtifactZipAndExpiryBranches(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	view, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A missing blob aborts the ZIP with a named error (never a partial archive).
	blobKey := ArtifactBlobKey("o", "r", run.ID, "build", view.Artifact.Attempt, 0)
	if err := e.store.Delete(ctx(), blobKey, ""); err != nil {
		t.Fatalf("delete blob: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteArtifactZip(ctx(), &buf, view); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing blob zip = %v", err)
	}
	// Rewriting a blob with the wrong size is caught while copying.
	if _, err := e.store.Put(ctx(), blobKey, store.PutBody{Bytes: []byte("x")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("rewrite blob: %v", err)
	}
	buf.Reset()
	if err := WriteArtifactZip(ctx(), &buf, view); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "the manifest says") {
		t.Fatalf("size mismatch zip = %v", err)
	}
	// A canceled context aborts before reading.
	canceled, cancel := contextWithCancel()
	cancel()
	if err := WriteArtifactZip(canceled, io.Discard, view); err == nil {
		t.Fatalf("canceled zip succeeded")
	}
	// A download cap refuses an artifact over the run total.
	e.svc.MaxArtifactBytesPerRun = 1
	e.svc.MaxArtifactBytesPerFile = 1
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("download cap = %v", err)
	}
	// Zip entry count sanity for the healthy path (one entry per manifest file).
	e.svc.MaxArtifactBytesPerRun, e.svc.MaxArtifactBytesPerFile = 0, 0
	_ = restoreBlob(t, e, run.ID, 0, "alpha")
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	buf.Reset()
	if err := WriteArtifactZip(ctx(), &buf, dl); err != nil {
		t.Fatalf("healthy zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("zip = %v (%v)", zr, err)
	}
	// An artifact over the download cap is a 413 through the API's status map.
	if got := statusFor(fmt.Errorf("%w: too big", ErrTooLarge)); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("statusFor(ErrTooLarge) = %d", got)
	}
}

// restoreBlob rewrites one artifact blob with the given bytes.
func restoreBlob(t *testing.T, e *testEnv, runID string, index int, body string) error {
	t.Helper()
	_, err := e.store.Put(ctx(), ArtifactBlobKey("o", "r", runID, "build", 1, index), store.PutBody{Bytes: []byte(body)}, store.PutOptions{Mode: store.PutOverwrite})
	return err
}

func TestHTTPArtifactExpiryIndexBranches(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeArtifactNamed(t, claims, "short", 1)
	now := e.now.Add(48 * 60 * 60 * 1e9)
	res := &GCResult{}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", now, res); err != nil || res.ArtifactsDeleted != 1 {
		t.Fatalf("expire = %+v (%v)", res, err)
	}
	// Junk index rows are dropped, not fatal: a bad name, an unparseable body
	// and an unparseable timestamp.
	for key, body := range map[string]string{
		ArtifactExpiryPrefix("o", "r") + "0000000000000000001-run.json":                          `{"run":"bad","name":"../x","expires_at":"2020-01-01T00:00:00Z"}`,
		ArtifactExpiryPrefix("o", "r") + "0000000000000000002-run.json":                          "{",
		ArtifactExpiryPrefix("o", "r") + "0000000000000000003-run.json":                          `{"run":"1795176002000000000-abcdef","name":"a","expires_at":"nope"}`,
		ArtifactExpiryPrefix("o", "r") + "0000000000000000004-1795176002000000000-abcdef-a.json": `{"run":"1795176002000000000-abcdef","name":"a","expires_at":"2020-01-01T00:00:00Z"}`,
	} {
		if _, err := e.store.Put(ctx(), key, store.PutBody{Bytes: []byte(body)}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed index: %v", err)
		}
	}
	res = &GCResult{}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", now, res); err != nil {
		t.Fatalf("junk expiry: %v", err)
	}
	// A canceled pass stops promptly.
	canceled, cancel := contextWithCancel()
	cancel()
	_ = e.svc.expireArtifacts(canceled, "o", "r", now, &GCResult{})
}

func TestHTTPArtifactManifestAndBlobScope(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	// A token without the artifact scope is refused (the scope is the authority).
	noScope := claims
	noScope.Scopes = []string{ScopeLog}
	if _, err := e.svc.ArtifactManifestForJob(ctx(), noScope, "build"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unscoped manifest = %v", err)
	}
	// A malformed repository claim is a service error, not a panic.
	badRepo := claims
	badRepo.Repo = "not a repo"
	if _, err := e.svc.ArtifactManifestForJob(ctx(), badRepo, "build"); err == nil {
		t.Fatalf("malformed repo accepted")
	}
	// The manifest lookup rejects an invalid name before any store read.
	if _, err := e.svc.ArtifactManifestForJob(ctx(), claims, "../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad name manifest = %v", err)
	}
	// A blob whose stored size disagrees with the manifest is corrupt, not data.
	key := ArtifactBlobKey("o", "r", run.ID, "build", claims.Attempt, 1)
	if _, err := e.store.Put(ctx(), key, store.PutBody{Bytes: []byte("longer bytes")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt blob = %v", err)
	}
	// The open path's guards: an invalid run id, an unknown run, an invalid name.
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", "nope", "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad run id = %v", err)
	}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", "0000000000000000001-abcdef", "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "..", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad artifact name = %v", err)
	}
	// A corrupt manifest is a named ErrCorrupt.
	if _, err := e.store.Put(ctx(), ArtifactManifestKey("o", "r", run.ID, "broken"), store.PutBody{Bytes: []byte("{")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed corrupt manifest: %v", err)
	}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest list = %v", err)
	}
}

func TestHTTPArtifactReaderGateAndMethodGates(t *testing.T) {
	e, h, cur := newHTTP(t)
	e.roles.public = false
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	*cur = auth.Anonymous()
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
	*cur = e.reader()
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("reader list = %d", rec.Code)
	}
	*cur = auth.Principal{Name: "stranger"}
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger list = %d", rec.Code)
	}
}

// contextWithCancel is the short cancel helper the expiry/ZIP branches use.
func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
