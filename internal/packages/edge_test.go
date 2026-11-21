package packages

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// errReader fails after n bytes (spool/spoolHashed read-error branches).
type errReader struct{ n int }

func (e *errReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, errors.New("read exploded")
	}
	if len(p) > e.n {
		p = p[:e.n]
	}
	for i := range p {
		p[i] = 'x'
	}
	e.n -= len(p)
	return len(p), nil
}

func TestErrorTypeAndRank(t *testing.T) {
	e := &Error{Status: 418, Code: "X", Message: "msg"}
	if e.Error() != "X: msg" {
		t.Fatalf("Error() = %q", e.Error())
	}
	for role, want := range map[string]int{
		"read": 1, "triage": 2, "write": 3, "maintain": 4, "admin": 5, "READ": 1, "": 0, "nonsense": 0,
	} {
		if got := roleRank(role); got != want {
			t.Fatalf("roleRank(%q) = %d, want %d", role, got, want)
		}
	}
}

func TestServiceDefaultsWithoutClock(t *testing.T) {
	mem := store.NewMemory()
	svc := New(mem, nil) // Now nil → time.Now; TokenSecret empty → per-process key
	if svc.now().IsZero() {
		t.Fatal("now must fall back to the wall clock")
	}
	k1 := svc.key()
	k2 := svc.key()
	if len(k1) == 0 || string(k1) != string(k2) {
		t.Fatal("key must be stable within the process")
	}
	// A configured secret is used verbatim.
	s2 := New(mem, nil)
	s2.TokenSecret = []byte("configured")
	if string(s2.key()) != "configured" {
		t.Fatal("configured secret not honored")
	}
}

func TestUploadOffsetBranches(t *testing.T) {
	f := newFixture(t)
	// Missing data and no tombstone → 0.
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	id := w.Header().Get("Docker-Upload-UUID")
	off, oerr := f.svc.UploadOffset(context.Background(), "acme", "demo", id)
	if oerr != nil || off != 0 {
		t.Fatalf("empty offset = %d %v", off, oerr)
	}
	// Signed but malformed timestamp.
	if _, oerr := f.svc.UploadOffset(context.Background(), "acme", "demo", "zzz-abc-def"); oerr == nil {
		t.Fatal("bad timestamp accepted")
	}
	// Head error surfaces.
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) {
		return nil, errors.New("head boom")
	}
	if _, oerr := f.svc.UploadOffset(context.Background(), "acme", "demo", id); oerr == nil || oerr.Status != 500 {
		t.Fatalf("head error = %+v", oerr)
	}
	f.st.headFn = nil
}

func TestServiceErrorPathMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	boom := errors.New("boom")

	// Manifest writes: store error on the object and on the tag pointer.
	f.st.putFn = func(string, error) error { return boom }
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", "x", []byte(imageManifest), ""); oerr == nil || oerr.Status != 500 {
		t.Fatalf("manifest put error = %+v", oerr)
	}
	f.st.putFn = nil

	// Tag pointer read error during putTag.
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return nil, boom }
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", "x", []byte(imageManifest), ""); oerr == nil || oerr.Status != 500 {
		t.Fatalf("tag probe error = %+v", oerr)
	}
	// Manifest reads: tag read error and digest read error.
	if _, oerr := f.svc.GetManifest(ctx, "acme", "demo", "x"); oerr == nil || oerr.Status != 500 {
		t.Fatalf("tag get error = %+v", oerr)
	}
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return nil, boom }
	if _, oerr := f.svc.GetManifest(ctx, "acme", "demo", "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("manifest get error = %+v", oerr)
	}
	f.st.getFn = nil

	// Blob HEAD/GET error mapping.
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) { return nil, boom }
	if _, oerr := f.svc.HeadBlob(ctx, "acme", "demo", "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("head blob error = %+v", oerr)
	}
	f.st.headFn = nil
	if _, oerr := f.svc.HeadBlob(ctx, "acme", "demo", "nope"); oerr == nil || oerr.Code != CodeDigestInvalid {
		t.Fatalf("head blob digest = %+v", oerr)
	}
	if _, oerr := f.svc.HeadBlob(ctx, "bad owner", "demo", "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("head blob repo = %+v", oerr)
	}
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return nil, boom }
	if _, oerr := f.svc.GetBlob(ctx, "acme", "demo", "sha256:"+strings.Repeat("a", 64), nil); oerr == nil || oerr.Status != 500 {
		t.Fatalf("get blob error = %+v", oerr)
	}
	f.st.getFn = nil
	if _, oerr := f.svc.GetBlob(ctx, "acme", "demo", "nope", nil); oerr == nil || oerr.Code != CodeDigestInvalid {
		t.Fatalf("get blob digest = %+v", oerr)
	}
	if _, oerr := f.svc.GetBlob(ctx, "bad owner", "demo", "sha256:"+strings.Repeat("a", 64), nil); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("get blob repo = %+v", oerr)
	}

	// Deletes: store error on both ref shapes.
	f.st.deleteFn = func(error) error { return boom }
	if oerr := f.svc.DeleteManifest(ctx, "acme", "demo", "tag"); oerr == nil || oerr.Status != 500 {
		t.Fatalf("delete tag error = %+v", oerr)
	}
	if oerr := f.svc.DeleteManifest(ctx, "acme", "demo", "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("delete digest error = %+v", oerr)
	}
	f.st.deleteFn = nil
	if oerr := f.svc.DeleteManifest(ctx, "bad owner", "demo", "x"); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("delete repo = %+v", oerr)
	}

	// List error.
	f.st.listFn = func(error) error { return boom }
	if _, _, oerr := f.svc.ListTags(ctx, "acme", "demo", "", 10); oerr == nil || oerr.Status != 500 {
		t.Fatalf("list error = %+v", oerr)
	}
	f.st.listFn = nil
	if _, _, oerr := f.svc.ListTags(ctx, "bad owner", "demo", "", 10); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("list repo = %+v", oerr)
	}

	// Abort: put/delete errors and unknown ids.
	f.st.putFn = func(string, error) error { return boom }
	if oerr := f.svc.AbortUpload(ctx, "acme", "demo", "bad"); oerr == nil || oerr.Code != CodeBlobUploadUnknown {
		t.Fatalf("abort bad id = %+v", oerr)
	}
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	id := w.Header().Get("Docker-Upload-UUID")
	if oerr := f.svc.AbortUpload(ctx, "acme", "demo", id); oerr == nil || oerr.Status != 500 {
		t.Fatalf("abort put error = %+v", oerr)
	}
	f.st.putFn = nil
	f.st.deleteFn = func(error) error { return boom }
	if oerr := f.svc.AbortUpload(ctx, "acme", "demo", id); oerr == nil || oerr.Status != 500 {
		t.Fatalf("abort delete error = %+v", oerr)
	}
	f.st.deleteFn = nil
	if oerr := f.svc.AbortUpload(ctx, "bad owner", "demo", "a-b-c"); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("abort bad repo = %+v", oerr)
	}

	// Spool errors: a failing body and an over-cap body.
	if _, _, oerr := f.svc.spool(&errReader{n: 3}, 100); oerr == nil || oerr.Status != 500 {
		t.Fatalf("spool read error = %+v", oerr)
	}
	if _, _, oerr := f.svc.spool(strings.NewReader(strings.Repeat("x", 10)), 4); oerr == nil || oerr.Code != CodeSizeInvalid {
		t.Fatalf("spool cap = %+v", oerr)
	}
	if _, _, _, oerr := f.svc.spoolHashed(&errReader{n: 3}, 100); oerr == nil || oerr.Status != 500 {
		t.Fatalf("spoolHashed read error = %+v", oerr)
	}
	if _, _, _, oerr := f.svc.spoolHashed(strings.NewReader(strings.Repeat("x", 10)), 4); oerr == nil || oerr.Code != CodeSizeInvalid {
		t.Fatalf("spoolHashed cap = %+v", oerr)
	}

	// Finalize: bad id, bad digest, missing staging, unexpected variant,
	// read error, and a delete failure after a successful write.
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", "bad", "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Code != CodeBlobUploadUnknown {
		t.Fatalf("finalize id = %+v", oerr)
	}
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	id = w.Header().Get("Docker-Upload-UUID")
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, "nope"); oerr == nil || oerr.Code != CodeDigestInvalid {
		t.Fatalf("finalize digest = %+v", oerr)
	}
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Code != CodeBlobUploadUnknown {
		t.Fatalf("finalize empty = %+v", oerr)
	}
	if _, oerr := f.svc.FinalizeUpload(ctx, "bad owner", "demo", id, "sha256:"+strings.Repeat("a", 64)); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("finalize repo = %+v", oerr)
	}
	data := []byte("payload")
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader(data)); oerr != nil {
		t.Fatalf("append = %+v", oerr)
	}
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return store.NotModified{}, nil }
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, digestOf(data)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("finalize variant = %+v", oerr)
	}
	f.st.getFn = func(res store.GetResult, err error) (store.GetResult, error) {
		o, ok := res.(store.Object)
		if !ok {
			return res, err
		}
		o.Body = io.NopCloser(&errReader{n: 0})
		return o, nil
	}
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, digestOf(data)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("finalize read error = %+v", oerr)
	}
	f.st.getFn = nil
	f.st.deleteFn = func(error) error { return boom }
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, digestOf(data)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("finalize delete error = %+v", oerr)
	}
	f.st.deleteFn = nil
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	id = w.Header().Get("Docker-Upload-UUID")
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader(data)); oerr != nil {
		t.Fatalf("append2 = %+v", oerr)
	}
	if _, oerr := f.svc.FinalizeUpload(ctx, "acme", "demo", id, digestOf([]byte("wrong"))); oerr == nil || oerr.Code != CodeDigestInvalid {
		t.Fatalf("finalize mismatch = %+v", oerr)
	}

	// Append: head error, then store write error, then compose error.
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) { return nil, boom }
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader(data)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("append head error = %+v", oerr)
	}
	f.st.headFn = nil
	f.st.putFn = func(string, error) error { return boom }
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader(data)); oerr == nil || oerr.Status != 500 {
		t.Fatalf("append put error = %+v", oerr)
	}
	f.st.putFn = nil

	// Role gates with a failing resolver.
	svc := New(f.st, f.roles)
	f.roles.err = &auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}
	if err := svc.CanPull(ctx, "acme", "demo", auth.Principal{Name: "z"}); err == nil || err.Status != 403 {
		t.Fatalf("forbidden pull = %+v", err)
	}
	if err := svc.CanPush(ctx, "acme", "demo", auth.Principal{Name: "z", Write: true}); err != nil {
		t.Fatalf("write pull = %+v", err)
	}
	f.roles.err = nil
}

func TestHTTPErrorBranches(t *testing.T) {
	f := newFixture(t)
	// ServeHTTP's not-ours branch.
	r := httptest.NewRequest("GET", "/not-v2", nil)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("foreign path = %d", w.Code)
	}
	// A bad escape leaves the segment verbatim.
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("decodeSegment = %q", got)
	}
	// putManifest with a failing body reader.
	r = httptest.NewRequest("PUT", "/v2/acme/demo/manifests/x", &errReader{n: 0})
	r.Header.Set("X-Test-User", "writer")
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body error = %d", w.Code)
	}
	// deleteManifest store failure.
	f.st.deleteFn = func(error) error { return errors.New("nope") }
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/x", nil); w.Code != 500 {
		t.Fatalf("delete error = %d", w.Code)
	}
	f.st.deleteFn = nil
	// abortUpload service failure (tombstone write).
	f.st.putFn = func(string, error) error { return errors.New("nope") }
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	if w := f.as("writer", "DELETE", loc, nil); w.Code != 500 {
		t.Fatalf("abort error = %d", w.Code)
	}
	f.st.putFn = nil
	// uploadStatus head failure.
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) { return nil, errors.New("nope") }
	if w := f.as("writer", "GET", loc, nil); w.Code != 500 {
		t.Fatalf("status error = %d", w.Code)
	}
	f.st.headFn = nil
	// uploadStatus unknown (aborted) → 404.
	if w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil); w.Code != 202 {
		t.Fatalf("start = %d", w.Code)
	}
	loc2 := w.Header().Get("Location")
	if w := f.as("writer", "DELETE", loc2, nil); w.Code != 204 {
		t.Fatalf("abort = %d", w.Code)
	}
	if w := f.as("writer", "GET", loc2, nil); w.Code != 404 {
		t.Fatalf("status aborted = %d", w.Code)
	}
	// listTags store failure.
	f.st.listFn = func(error) error { return errors.New("nope") }
	if w := f.as("reader", "GET", "/v2/acme/demo/tags/list", nil); w.Code != 500 {
		t.Fatalf("list error = %d", w.Code)
	}
	f.st.listFn = nil
	// getBlob store failure after authorize.
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return nil, errors.New("nope") }
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/sha256:"+strings.Repeat("a", 64), nil); w.Code != 500 {
		t.Fatalf("blob error = %d", w.Code)
	}
	f.st.getFn = nil
	// HEAD blob store failure.
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) { return nil, errors.New("nope") }
	if w := f.as("reader", "HEAD", "/v2/acme/demo/blobs/sha256:"+strings.Repeat("a", 64), nil); w.Code != 500 {
		t.Fatalf("blob head error = %d", w.Code)
	}
	f.st.headFn = nil
	// putManifest store failure.
	f.st.putFn = func(string, error) error { return errors.New("nope") }
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/y", []byte(imageManifest)); w.Code != 500 {
		t.Fatalf("manifest store error = %d", w.Code)
	}
	f.st.putFn = nil
	// A token-scoped principal without the action is 403 before the role gate.
	tok, _ := f.svc.MintToken(auth.Principal{Name: "writer"}, []string{"repository:acme/demo:pull"})
	if w := f.req("PUT", "/v2/acme/demo/manifests/z", []byte(imageManifest), map[string]string{"Authorization": "Bearer " + tok}); w.Code != 403 {
		t.Fatalf("scope-denied push = %d", w.Code)
	}
	// The X-Walgit-Authorization header carries the registry token too.
	if w := f.req("GET", "/v2/acme/demo/tags/list", nil, map[string]string{"X-Walgit-Authorization": "Bearer " + tok}); w.Code != 200 {
		t.Fatalf("x-walgit token = %d", w.Code)
	}
	// realm derives from a TLS request when public_url is empty.
	r = httptest.NewRequest("GET", "/v2/", nil)
	r.TLS = &tls.ConnectionState{}
	if got := f.h.realm(r); got != "https://example.com/v2/token" {
		t.Fatalf("tls realm = %q", got)
	}
	// marshal failures fall back to a minimal body.
	writeJSON(httptest.NewRecorder(), 200, map[string]any{"bad": make(chan int)})
	writeOCI(httptest.NewRecorder(), 500, &Error{Status: 500, Code: "X", Detail: make(chan int)})
}

func TestPutTagGetErrorAndNonPrecondition(t *testing.T) {
	f := newFixture(t)
	// putTag's non-precondition write error.
	f.st.putFn = func(key string, err error) error {
		if strings.Contains(key, "/tags/") {
			return store.NewOther(key, errors.New("other"))
		}
		return err
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/t", []byte(imageManifest)); w.Code != 500 {
		t.Fatalf("tag other error = %d", w.Code)
	}
	f.st.putFn = nil
	// ListTags tolerates an empty key segment in the callback.
	if _, err := f.st.Put(context.Background(), tagsPrefix("acme", "demo"), store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	tags, _, oerr := f.svc.ListTags(context.Background(), "acme", "demo", "", 10)
	if oerr != nil {
		t.Fatalf("tags = %v %+v", tags, oerr)
	}
	for _, tag := range tags {
		if tag == "" {
			t.Fatal("empty tag leaked from the prefix object")
		}
	}
}

func TestAuthorizationFailureBranches(t *testing.T) {
	f := newFixture(t)
	f.roles.public["acme/demo"] = false
	delete(f.roles.roles, "nobody|acme/demo")
	// Every route's authorize failure answers 401 with the challenge.
	cases := []struct{ method, path string }{
		{"PUT", "/v2/acme/demo/manifests/x"},
		{"DELETE", "/v2/acme/demo/manifests/x"},
		{"POST", "/v2/acme/demo/blobs/uploads/"},
		{"PATCH", "/v2/acme/demo/blobs/uploads/some-id"},
		{"DELETE", "/v2/acme/demo/blobs/uploads/some-id"},
		{"GET", "/v2/acme/demo/blobs/uploads/some-id"},
		{"PUT", "/v2/acme/demo/blobs/uploads/some-id?digest=sha256:" + strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		w := f.req(c.method, c.path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestHTTPAdjacentBranches(t *testing.T) {
	f := newFixture(t)
	// Empty manifest ref.
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/", nil); w.Code != 404 {
		t.Fatalf("empty ref = %d", w.Code)
	}
	// finalize with a body whose append exceeds the cap.
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	big := strings.Repeat("y", int(f.svc.maxBlob())+10)
	w = f.as("writer", "PUT", loc+"?digest=sha256:"+strings.Repeat("a", 64), []byte(big))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("finalize chunk cap = %d", w.Code)
	}
	// patch offset probe error (Content-Range + failing HEAD).
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc = w.Header().Get("Location")
	f.st.headFn = func(*store.ObjectMeta, error) (*store.ObjectMeta, error) { return nil, errors.New("boom") }
	r := httptest.NewRequest("PATCH", loc, strings.NewReader("x"))
	r.Header.Set("X-Test-User", "writer")
	r.Header.Set("Content-Range", "0-0")
	ww := httptest.NewRecorder()
	f.h.ServeHTTP(ww, r)
	if ww.Code != 500 {
		t.Fatalf("patch offset error = %d", ww.Code)
	}
	f.st.headFn = nil
	// Handler with no Auth seam is anonymous.
	anon := &Handler{Svc: f.svc}
	r = httptest.NewRequest("GET", "/v2/timeout/manifests/x", nil)
	ww = httptest.NewRecorder()
	anon.ServeHTTP(ww, r)
	if ww.Code != 404 && ww.Code != 401 {
		t.Fatalf("nil auth = %d", ww.Code)
	}
	// writeOCI nil error falls back to UNKNOWN.
	rec := httptest.NewRecorder()
	writeOCI(rec, 500, nil)
	if !strings.Contains(rec.Body.String(), "UNKNOWN") {
		t.Fatalf("nil error body = %s", rec.Body.String())
	}
}

func TestServiceMoreEdges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Invalid ref on the service delete path.
	if oerr := f.svc.DeleteManifest(ctx, "acme", "demo", "bad/tag"); oerr == nil || oerr.Code != CodeManifestUnknown {
		t.Fatalf("delete bad ref = %+v", oerr)
	}
	// aborted() error surfaces through UploadOffset when the data is absent.
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	id := w.Header().Get("Docker-Upload-UUID")
	f.st.headFn = func(m *store.ObjectMeta, err error) (*store.ObjectMeta, error) {
		if m == nil {
			return nil, errors.New("tombstone boom")
		}
		return m, err
	}
	if _, oerr := f.svc.UploadOffset(ctx, "acme", "demo", id); oerr == nil || oerr.Status != 500 {
		t.Fatalf("aborted error = %+v", oerr)
	}
	f.st.headFn = nil
	// Compose error on the second chunk.
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader([]byte("one"))); oerr != nil {
		t.Fatalf("append = %+v", oerr)
	}
	f.st.composeFn = func(error) error { return errors.New("compose boom") }
	if _, oerr := f.svc.AppendUpload(ctx, "acme", "demo", id, bytes.NewReader([]byte("two"))); oerr == nil || oerr.Status != 500 {
		t.Fatalf("compose error = %+v", oerr)
	}
	f.st.composeFn = nil
	// Spool temp-file creation failure: SpoolDir is a file AND TMPDIR is a
	// file, so there is nowhere to stage.
	dir := t.TempDir()
	file := dir + "/f"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.SpoolDir = file
	t.Setenv("TMPDIR", file)
	if _, _, oerr := f.svc.spool(strings.NewReader("x"), 10); oerr == nil || oerr.Status != 500 {
		t.Fatalf("spool temp error = %+v", oerr)
	}
	if _, _, _, oerr := f.svc.spoolHashed(strings.NewReader("x"), 10); oerr == nil || oerr.Status != 500 {
		t.Fatalf("spoolHashed temp error = %+v", oerr)
	}
}

func TestRangePastEOFAndTokenMintRefusal(t *testing.T) {
	f := newFixture(t)
	dig := f.pushBlob(t, "writer", []byte("short"))
	// A range entirely past EOF is 416, not a 500.
	w := f.req("GET", "/v2/acme/demo/blobs/"+dig, nil, map[string]string{"X-Test-User": "reader", "Range": "bytes=99-120"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past EOF = %d %s", w.Code, w.Body.String())
	}
	// A registry token cannot be exchanged for a wider token.
	tok, _ := f.svc.MintToken(auth.Principal{Name: "writer"}, []string{"repository:acme/demo:pull"})
	r := httptest.NewRequest("GET", "/v2/token?scope=repository:acme/demo:pull,push", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token exchange = %d %s", rec.Code, rec.Body.String())
	}
	// An enormous end bound does not overflow the exclusive conversion.
	if got := parseRange("bytes=0-9223372036854775807"); got == nil || got[1] != 1<<63-1 {
		t.Fatalf("max range = %v", got)
	}
}
