package packages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- fixtures -----------------------------------------------------------------

type fakeRoles struct {
	mu     sync.Mutex
	roles  map[string]identity.Role // "user|owner/repo" → role
	public map[string]bool          // "owner/repo" → anonymous readable
	err    *auth.AuthError          // CheckRead failure override
}

func newFakeRoles() *fakeRoles {
	return &fakeRoles{roles: map[string]identity.Role{}, public: map[string]bool{}}
}

func (f *fakeRoles) Resolve(_ context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	if r, ok := f.roles[p.Name+"|"+owner+"/"+repo]; ok {
		return r, nil
	}
	if p.Write {
		return identity.RoleWrite, nil
	}
	if f.public[owner+"/"+repo] {
		return identity.RoleRead, nil
	}
	return "", nil
}

func (f *fakeRoles) CheckRead(_ context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if p.Admin || p.Write {
		return nil
	}
	if f.public[owner+"/"+repo] {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if r, ok := f.roles[p.Name+"|"+owner+"/"+repo]; ok && roleRank(string(r)) >= roleRank(string(identity.RoleRead)) {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}

// CheckPush mirrors identity.CheckPush for the fixture: admin bypass, the
// self-namespace owner rule, then an explicit binding at write or above. The
// fixture's "writer" principal (the Write flag) stands in for the explicit
// binding the production rule requires — a host write flag grants nothing
// there, and TestCanPushReusesGitOwnerRule pins that with the real service.
func (f *fakeRoles) CheckPush(_ context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if strings.EqualFold(owner, p.Name) {
		return nil
	}
	if r, ok := f.roles[p.Name+"|"+owner+"/"+repo]; ok && roleRank(string(r)) >= roleRank(string(identity.RoleWrite)) {
		return nil
	}
	if p.Write {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "write access required"}
}

// testAuth resolves a principal from X-Test-User (anonymous otherwise); a
// Basic credential maps password "tok" to the username (the Docker login).
func testAuth(r *http.Request) (auth.Principal, *auth.AuthError) {
	if u, p, ok := r.BasicAuth(); ok && p == "tok" {
		return auth.Principal{Name: u}, nil
	}
	if u := r.Header.Get("X-Test-User"); u != "" {
		p := auth.Principal{Name: u, Write: r.Header.Get("X-Test-Write") == "1", Admin: r.Header.Get("X-Test-Admin") == "1"}
		if u == "writer" {
			p.Write = true
		}
		if u == "admin" {
			p.Admin = true
		}
		return p, nil
	}
	return auth.Anonymous(), nil
}

type fixture struct {
	svc     *Service
	h       *Handler
	st      *countStore
	roles   *fakeRoles
	exists  map[string]bool
	featOff map[string]bool
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	mem := store.NewMemory()
	cs := &countStore{Memory: mem}
	f := &fixture{
		st:      cs,
		roles:   newFakeRoles(),
		exists:  map[string]bool{"acme/demo": true},
		featOff: map[string]bool{},
		now:     time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	svc := New(cs, f.roles)
	svc.Features = func(_ context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		all := config.AllFeatures()
		if f.featOff[owner+"/"+repo] {
			all.Packages = false
		}
		return all, true
	}
	svc.Exists = func(_ context.Context, owner, repo string) (bool, error) {
		return f.exists[owner+"/"+repo], nil
	}
	svc.MaxBlobBytes = 1 << 20
	svc.MaxManifestBytes = 4096
	svc.AnonymousRead = true
	svc.TokenSecret = []byte("test-secret")
	svc.Now = func() time.Time { return f.now }
	svc.SpoolDir = t.TempDir()
	f.roles.public["acme/demo"] = true
	f.svc = svc
	f.h = &Handler{Svc: svc, Auth: testAuth}
	return f
}

// countStore counts Put/Get/List calls (budget assertions) and can inject a
// store failure.
type countStore struct {
	*store.Memory
	mu          sync.Mutex
	puts        int
	gets        int
	lists       int
	getFn       func(store.GetResult, error) (store.GetResult, error)
	getKeyFn    func(string, store.GetResult, error) (store.GetResult, error)
	putFn       func(key string, err error) error
	headFn      func(*store.ObjectMeta, error) (*store.ObjectMeta, error)
	headKeyFn   func(string, *store.ObjectMeta, error) (*store.ObjectMeta, error)
	deleteFn    func(error) error
	deleteKeyFn func(string, error) error
	listFn      func(error) error
	composeFn   func(error) error
}

func (c *countStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	m, err := c.Memory.Put(ctx, key, body, opts)
	if c.putFn != nil {
		if ferr := c.putFn(key, err); ferr != nil {
			return store.ObjectMeta{}, ferr
		}
	}
	return m, err
}

func (c *countStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	res, err := c.Memory.Get(ctx, key, opts)
	if c.getKeyFn != nil {
		return c.getKeyFn(key, res, err)
	}
	if c.getFn != nil {
		return c.getFn(res, err)
	}
	return res, err
}

func (c *countStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	err := c.Memory.List(ctx, prefix, startAfter, fn)
	if c.listFn != nil {
		if ferr := c.listFn(err); ferr != nil {
			return ferr
		}
	}
	return err
}

func (c *countStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	m, err := c.Memory.Head(ctx, key)
	if c.headKeyFn != nil {
		return c.headKeyFn(key, m, err)
	}
	if c.headFn != nil {
		return c.headFn(m, err)
	}
	return m, err
}

func (c *countStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	m, err := c.Memory.Compose(ctx, dst, sources, opts)
	if c.composeFn != nil {
		if ferr := c.composeFn(err); ferr != nil {
			return store.ObjectMeta{}, ferr
		}
	}
	return m, err
}

func (c *countStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	err := c.Memory.Delete(ctx, key, ifVersion)
	if c.deleteKeyFn != nil {
		if ferr := c.deleteKeyFn(key, err); ferr != nil {
			return ferr
		}
	}
	if c.deleteFn != nil {
		if ferr := c.deleteFn(err); ferr != nil {
			return ferr
		}
	}
	return err
}

func (c *countStore) counts() (puts, gets, lists int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts, c.gets, c.lists
}

func (f *fixture) resetCounts() { f.st.puts, f.st.gets, f.st.lists = 0, 0, 0 }

// req issues an authenticated request through the handler.
func (f *fixture) req(method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *fixture) as(user string, method, path string, body []byte) *httptest.ResponseRecorder {
	return f.req(method, path, body, map[string]string{"X-Test-User": user})
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushBlob uploads one blob through the real protocol and returns its digest.
func (f *fixture) pushBlob(t *testing.T, user string, data []byte) string {
	t.Helper()
	w := f.as(user, "POST", "/v2/acme/demo/blobs/uploads/", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start upload = %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("start upload: no Location")
	}
	w = f.as(user, "PATCH", loc, data)
	if w.Code != http.StatusAccepted {
		t.Fatalf("patch = %d body=%s", w.Code, w.Body.String())
	}
	dig := digestOf(data)
	w = f.as(user, "PUT", loc+"?digest="+dig, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("finalize = %d body=%s", w.Code, w.Body.String())
	}
	return dig
}

const imageManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`

// --- ping / challenge ---------------------------------------------------------

func TestPingAndChallenge(t *testing.T) {
	f := newFixture(t)
	w := f.req("GET", "/v2/", nil, nil)
	if w.Code != 200 || w.Body.String() != "{}" {
		t.Fatalf("ping = %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("api version = %q", got)
	}

	// Auth-required instances answer the Docker challenge.
	f.h.AuthRequired = true
	w = f.req("GET", "/v2/", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("challenged ping = %d", w.Code)
	}
	ch := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(ch, `Bearer realm="http://example.com/v2/token"`) || !strings.Contains(ch, `service="example.com"`) {
		t.Fatalf("challenge = %q", ch)
	}
	if !strings.Contains(w.Body.String(), `"code":"UNAUTHORIZED"`) {
		t.Fatalf("body = %s", w.Body.String())
	}

	// A Basic credential satisfies the ping (docker login probes it).
	r := httptest.NewRequest("GET", "/v2/", nil)
	r.SetBasicAuth("alice", "tok")
	ww := httptest.NewRecorder()
	f.h.ServeHTTP(ww, r)
	if ww.Code != 200 {
		t.Fatalf("basic ping = %d", ww.Code)
	}

	// HEAD carries the version header and no body.
	f.h.AuthRequired = false
	w = f.req("HEAD", "/v2", nil, nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("head ping = %d %q", w.Code, w.Body.String())
	}

	// Wrong method is 405.
	w = f.req("POST", "/v2/", nil, nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post ping = %d", w.Code)
	}

	// Non-/v2 paths are not ours.
	if f.h.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", "/acme/demo", nil)) {
		t.Fatal("Handle claimed a non-v2 path")
	}
	// A malformed bearer is a real 401.
	w = f.req("GET", "/v2/", nil, map[string]string{"Authorization": "Bearer wgp1.bogus.sig"})
	if w.Code != 401 {
		t.Fatalf("bad bearer ping = %d", w.Code)
	}
}

// --- push/pull ----------------------------------------------------------------

func TestPushPullByTagAndDigest(t *testing.T) {
	f := newFixture(t)
	dig := f.pushBlob(t, "writer", []byte("layer-bytes"))

	w := f.as("writer", "PUT", "/v2/acme/demo/manifests/1.0.0", []byte(imageManifest))
	if w.Code != http.StatusCreated {
		t.Fatalf("manifest put = %d %s", w.Code, w.Body.String())
	}
	mdig := digestOf([]byte(imageManifest))
	if got := w.Header().Get("Docker-Content-Digest"); got != mdig {
		t.Fatalf("digest = %q want %q", got, mdig)
	}

	for _, ref := range []string{"1.0.0", mdig} {
		w = f.as("reader", "GET", "/v2/acme/demo/manifests/"+ref, nil)
		if w.Code != 200 || w.Body.String() != imageManifest {
			t.Fatalf("get %s = %d %q", ref, w.Code, w.Body.String())
		}
		if w.Header().Get("Docker-Content-Digest") != mdig {
			t.Fatalf("get %s digest header", ref)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/vnd.oci.image.manifest.v1+json" {
			t.Fatalf("content type = %q", ct)
		}
		w = f.as("reader", "HEAD", "/v2/acme/demo/manifests/"+ref, nil)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("head %s = %d", ref, w.Code)
		}
	}

	w = f.as("reader", "GET", "/v2/acme/demo/blobs/"+dig, nil)
	if w.Code != 200 || w.Body.String() != "layer-bytes" {
		t.Fatalf("blob get = %d %q", w.Code, w.Body.String())
	}
	w = f.as("reader", "HEAD", "/v2/acme/demo/blobs/"+dig, nil)
	if w.Code != 200 || w.Header().Get("Docker-Content-Digest") != dig {
		t.Fatalf("blob head = %d", w.Code)
	}
	// Range read.
	w = f.req("GET", "/v2/acme/demo/blobs/"+dig, nil, map[string]string{"X-Test-User": "reader", "Range": "bytes=2-4"})
	if w.Code != http.StatusPartialContent || w.Body.String() != "yer" {
		t.Fatalf("range = %d %q", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 2-4/11" {
		t.Fatalf("content-range = %q", cr)
	}
	// An open-ended range reads to EOF; suffix/multi/foreign ranges fall
	// back to the full body.
	w = f.req("GET", "/v2/acme/demo/blobs/"+dig, nil, map[string]string{"X-Test-User": "reader", "Range": "bytes=3-"})
	if w.Code != http.StatusPartialContent || w.Body.String() != "er-bytes" {
		t.Fatalf("open range = %d %q", w.Code, w.Body.String())
	}
	for _, rng := range []string{"bytes=-3", "bytes=0-1,3-4", "items=0-1"} {
		w = f.req("GET", "/v2/acme/demo/blobs/"+dig, nil, map[string]string{"X-Test-User": "reader", "Range": rng})
		if w.Code != 200 || w.Body.String() != "layer-bytes" {
			t.Fatalf("range %q = %d %q", rng, w.Code, w.Body.String())
		}
	}
}

func TestMultiArchIndexRoundTrip(t *testing.T) {
	f := newFixture(t)
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":10,"platform":{"architecture":"amd64","os":"linux"}}]}`
	w := f.as("writer", "PUT", "/v2/acme/demo/manifests/latest", []byte(index))
	if w.Code != http.StatusCreated {
		t.Fatalf("index put = %d %s", w.Code, w.Body.String())
	}
	w = f.as("reader", "GET", "/v2/acme/demo/manifests/latest", nil)
	if w.Code != 200 || w.Body.String() != index {
		t.Fatalf("index get = %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("index content type = %q", ct)
	}
}

func TestDigestMismatchNeverStored(t *testing.T) {
	f := newFixture(t)
	// Manifest digest mismatch.
	wrong := "sha256:" + strings.Repeat("0", 64)
	w := f.as("writer", "PUT", "/v2/acme/demo/manifests/"+wrong, []byte(imageManifest))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"DIGEST_INVALID"`) {
		t.Fatalf("manifest mismatch = %d %s", w.Code, w.Body.String())
	}
	if _, err := f.st.Head(context.Background(), manifestKey("acme", "demo", DigestHex(wrong))); err != nil {
		t.Fatalf("head: %v", err)
	}
	real := DigestHex(digestOf([]byte(imageManifest)))
	if m, _ := f.st.Head(context.Background(), manifestKey("acme", "demo", real)); m != nil {
		t.Fatal("mismatched manifest was stored")
	}
	// Blob finalize mismatch.
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	w = f.as("writer", "PATCH", loc, []byte("data"))
	if w.Code != 202 {
		t.Fatalf("patch = %d", w.Code)
	}
	w = f.as("writer", "PUT", loc+"?digest="+wrong, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"DIGEST_INVALID"`) {
		t.Fatalf("blob mismatch = %d %s", w.Code, w.Body.String())
	}
	if m, _ := f.st.Head(context.Background(), blobKey("acme", "demo", DigestHex(wrong))); m != nil {
		t.Fatal("mismatched blob was stored")
	}
}

func TestResumedChunkedUpload(t *testing.T) {
	f := newFixture(t)
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	if w.Code != 202 {
		t.Fatalf("start = %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(w.Header().Get("Range"), "0-0") {
		t.Fatalf("start range = %q", w.Header().Get("Range"))
	}
	w = f.as("writer", "PATCH", loc, []byte("hello "))
	if w.Code != 202 || w.Header().Get("Range") != "0-5" {
		t.Fatalf("patch1 = %d range=%q", w.Code, w.Header().Get("Range"))
	}
	// Resume: the server reports the offset, and the next chunk continues.
	w = f.as("writer", "PUT", loc+"?digest=", nil)
	if w.Code != 400 {
		t.Fatalf("missing digest = %d", w.Code)
	}
	w = f.req("PATCH", loc, []byte("world"), map[string]string{"X-Test-User": "writer", "Content-Range": "6-10"})
	if w.Code != 202 || w.Header().Get("Range") != "0-10" {
		t.Fatalf("patch2 = %d range=%q body=%s", w.Code, w.Header().Get("Range"), w.Body.String())
	}
	// A gap is refused with 416.
	w = f.req("PATCH", loc, []byte("x"), map[string]string{"X-Test-User": "writer", "Content-Range": "99-99"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("gap = %d", w.Code)
	}
	w = f.req("GET", loc, nil, map[string]string{"X-Test-User": "writer"})
	if w.Code != http.StatusNoContent || w.Header().Get("Range") != "0-10" {
		t.Fatalf("status = %d range=%q", w.Code, w.Header().Get("Range"))
	}
	data := []byte("hello world")
	w = f.as("writer", "PUT", loc+"?digest="+digestOf(data), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("finalize = %d %s", w.Code, w.Body.String())
	}
	w = f.as("reader", "GET", "/v2/acme/demo/blobs/"+digestOf(data), nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("resumed blob = %d %q", w.Code, w.Body.String())
	}
}

func TestTagCASRace(t *testing.T) {
	f := newFixture(t)
	m1 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[],"config":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1}}`)
	m2 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[],"config":{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":1}}`)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := [][]byte{m1, m2}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := f.as("writer", "PUT", "/v2/acme/demo/manifests/race", bodies[i])
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			ok++
		} else if c != http.StatusConflict {
			t.Fatalf("race code = %d, want 201 or 409", c)
		}
	}
	if ok < 1 {
		t.Fatalf("no writer won the tag CAS: %v", codes)
	}
	w := f.as("reader", "GET", "/v2/acme/demo/manifests/race", nil)
	if w.Code != 200 {
		t.Fatalf("tag read = %d", w.Code)
	}
	// The pointer must resolve to one of the two manifests, never a blend.
	if w.Body.String() != string(m1) && w.Body.String() != string(m2) {
		t.Fatalf("tag points at unexpected bytes: %s", w.Body.String())
	}
}

func TestTagsListPaging(t *testing.T) {
	f := newFixture(t)
	for _, tag := range []string{"alpha", "beta", "gamma"} {
		if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/"+tag, []byte(imageManifest)); w.Code != 201 {
			t.Fatalf("put %s = %d", tag, w.Code)
		}
	}
	w := f.as("reader", "GET", "/v2/acme/demo/tags/list?n=2", nil)
	if w.Code != 200 {
		t.Fatalf("list = %d", w.Code)
	}
	var page struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Name != "acme/demo" || len(page.Tags) != 2 || page.Tags[0] != "alpha" || page.Tags[1] != "beta" {
		t.Fatalf("page = %+v", page)
	}
	link := w.Header().Get("Link")
	if !strings.Contains(link, "/v2/acme/demo/tags/list?n=2&last=beta") || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("link = %q", link)
	}
	w = f.as("reader", "GET", "/v2/acme/demo/tags/list?n=2&last=beta", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Tags) != 1 || page.Tags[0] != "gamma" {
		t.Fatalf("second page = %+v", page)
	}
	if w.Header().Get("Link") != "" {
		t.Fatalf("unexpected next link: %q", w.Header().Get("Link"))
	}
	// Invalid n is a 400 with the spec envelope.
	w = f.as("reader", "GET", "/v2/acme/demo/tags/list?n=0", nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"errors"`) {
		t.Fatalf("bad n = %d %s", w.Code, w.Body.String())
	}
}

func TestDeleteTagAndManifest(t *testing.T) {
	f := newFixture(t)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/keep", []byte(imageManifest)); w.Code != 201 {
		t.Fatalf("put = %d", w.Code)
	}
	// Deleting the tag leaves the manifest reachable by digest.
	w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/keep", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete tag = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/keep", nil); w.Code != 404 {
		t.Fatalf("get deleted tag = %d", w.Code)
	}
	mdig := digestOf([]byte(imageManifest))
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/"+mdig, nil); w.Code != 200 {
		t.Fatalf("digest after tag delete = %d", w.Code)
	}
	w = f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+mdig, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete digest = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/"+mdig, nil); w.Code != 404 {
		t.Fatalf("get deleted manifest = %d", w.Code)
	}
	// An invalid ref is not a tag and not a digest.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/bad/tag", nil); w.Code != 404 {
		t.Fatalf("bad ref delete = %d", w.Code)
	}
}

// --- auth matrix --------------------------------------------------------------

func TestAuthMatrix(t *testing.T) {
	f := newFixture(t)
	f.roles.public["acme/demo"] = true
	f.roles.roles["read|acme/demo"] = identity.RoleRead
	f.roles.roles["write|acme/demo"] = identity.RoleWrite
	f.roles.roles["admin|acme/demo"] = identity.RoleAdmin
	body := []byte(imageManifest)

	// Anonymous pull of a public repo works.
	if w := f.req("GET", "/v2/acme/demo/manifests/missing", nil, nil); w.Code != 404 {
		t.Fatalf("anon pull manifest = %d", w.Code)
	}
	// Anonymous push is a real 401 with the challenge.
	w := f.req("POST", "/v2/acme/demo/blobs/uploads/", nil, nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "scope=") {
		t.Fatalf("anon push = %d challenge=%q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	// Anonymous push to a PRIVATE repo is denied at the read gate when a
	// read-only credential is required first? Push is denied outright.
	// read can pull but not push.
	if w := f.as("read", "PUT", "/v2/acme/demo/manifests/x", body); w.Code != http.StatusForbidden {
		t.Fatalf("read push = %d", w.Code)
	}
	// write can push.
	if w := f.as("write", "PUT", "/v2/acme/demo/manifests/x", body); w.Code != http.StatusCreated {
		t.Fatalf("write push = %d %s", w.Code, w.Body.String())
	}
	// admin can delete.
	if w := f.as("admin", "DELETE", "/v2/acme/demo/manifests/x", nil); w.Code != http.StatusAccepted {
		t.Fatalf("admin delete = %d", w.Code)
	}
	// Anonymous pull of a private repo is 401 (the identity gate).
	delete(f.roles.public, "acme/demo")
	if w := f.req("GET", "/v2/acme/demo/manifests/x", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anon private pull = %d", w.Code)
	}
	// Feature off ⇒ 404 for every route, even with write.
	f.featOff["acme/demo"] = true
	if w := f.as("write", "PUT", "/v2/acme/demo/manifests/y", body); w.Code != 404 {
		t.Fatalf("feature-off push = %d", w.Code)
	}
	if w := f.as("read", "GET", "/v2/acme/demo/tags/list", nil); w.Code != 404 {
		t.Fatalf("feature-off tags = %d", w.Code)
	}
	delete(f.featOff, "acme/demo")
	// Unknown repo ⇒ NAME_UNKNOWN.
	if w := f.as("write", "POST", "/v2/acme/nope/blobs/uploads/", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "NAME_UNKNOWN") {
		t.Fatalf("unknown repo = %d %s", w.Code, w.Body.String())
	}
}

func TestBearerTokenFlow(t *testing.T) {
	f := newFixture(t)
	f.roles.public["acme/demo"] = true
	f.roles.roles["alice|acme/demo"] = identity.RoleWrite
	f.h.AuthRequired = true

	// The ping challenges; the client exchanges Basic for a token.
	r := httptest.NewRequest("GET", "/v2/", nil)
	r.SetBasicAuth("alice", "tok")
	tok := httptest.NewRecorder()
	f.h.ServeHTTP(tok, r)

	tr := httptest.NewRequest("GET", "/v2/token?service=example.com&scope=repository:acme/demo:pull,push", nil)
	tr.SetBasicAuth("alice", "tok")
	tw := httptest.NewRecorder()
	f.h.ServeHTTP(tw, tr)
	if tw.Code != 200 {
		t.Fatalf("token = %d %s", tw.Code, tw.Body.String())
	}
	var resp struct {
		Token   string `json:"token"`
		Expires int64  `json:"expires_in"`
		Access  []struct {
			Type    string   `json:"type"`
			Name    string   `json:"name"`
			Actions []string `json:"actions"`
		} `json:"access"`
	}
	if err := json.Unmarshal(tw.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.Expires <= 0 || len(resp.Access) != 1 {
		t.Fatalf("token response = %+v", resp)
	}
	if len(resp.Access[0].Actions) != 2 {
		t.Fatalf("granted actions = %v", resp.Access[0].Actions)
	}
	// The token authorizes a push without Basic.
	w := f.req("PUT", "/v2/acme/demo/manifests/tok", []byte(imageManifest), map[string]string{"Authorization": "Bearer " + resp.Token})
	if w.Code != http.StatusCreated {
		t.Fatalf("token push = %d %s", w.Code, w.Body.String())
	}

	// A pull-only token cannot push.
	tr = httptest.NewRequest("GET", "/v2/token?scope=repository:acme/demo:pull", nil)
	tr.SetBasicAuth("alice", "tok")
	tw = httptest.NewRecorder()
	f.h.ServeHTTP(tw, tr)
	_ = json.Unmarshal(tw.Body.Bytes(), &resp)
	w = f.req("PUT", "/v2/acme/demo/manifests/tok2", []byte(imageManifest), map[string]string{"Authorization": "Bearer " + resp.Token})
	if w.Code != http.StatusForbidden {
		t.Fatalf("pull token push = %d", w.Code)
	}

	// Anonymous token minting: public pull is granted, push is not.
	tr = httptest.NewRequest("GET", "/v2/token?scope=repository:acme/demo:pull", nil)
	tw = httptest.NewRecorder()
	f.h.ServeHTTP(tw, tr)
	if tw.Code != 200 {
		t.Fatalf("anon token = %d %s", tw.Code, tw.Body.String())
	}
	// Anonymous token for a private repo is refused.
	delete(f.roles.public, "acme/demo")
	tr = httptest.NewRequest("GET", "/v2/token?scope=repository:acme/demo:pull", nil)
	tw = httptest.NewRecorder()
	f.h.ServeHTTP(tw, tr)
	if tw.Code != http.StatusUnauthorized {
		t.Fatalf("anon private token = %d", tw.Code)
	}
}

func TestTokenVerification(t *testing.T) {
	f := newFixture(t)
	tok, exp := f.svc.MintToken(auth.Principal{Name: "alice"}, []string{"repository:acme/demo:pull"})
	if !exp.After(f.now) {
		t.Fatalf("exp = %v", exp)
	}
	claims, ok := f.svc.VerifyToken(tok)
	if !ok || claims.Sub != "alice" || !claims.scopeGrants("acme/demo", "pull") || claims.scopeGrants("acme/demo", "push") {
		t.Fatalf("claims = %+v ok=%v", claims, ok)
	}
	for _, bad := range []string{"", "wgp1", "wgp1.a", "wgp1.!!!!.sig", tok + "x", strings.Replace(tok, "wgp1.", "wgp2.", 1)} {
		if _, ok := f.svc.VerifyToken(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	// Expiry.
	f.now = f.now.Add(DefaultTokenTTL + time.Minute)
	if _, ok := f.svc.VerifyToken(tok); ok {
		t.Fatal("expired token accepted")
	}
}

// --- upload TTL and limits -----------------------------------------------------

func TestUploadTTLAndLimits(t *testing.T) {
	f := newFixture(t)
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	f.now = f.now.Add(DefaultUploadTTL + time.Minute)
	w = f.as("writer", "PATCH", loc, []byte("late"))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "BLOB_UPLOAD_UNKNOWN") {
		t.Fatalf("expired upload = %d %s", w.Code, w.Body.String())
	}
	// Abort is idempotent (204) for an unknown id? No: an unsigned id is
	// unknown; a signed id that expired is also unknown.
	w = f.as("writer", "DELETE", "/v2/acme/demo/blobs/uploads/not-a-real-id", nil)
	if w.Code != 404 {
		t.Fatalf("abort unknown = %d", w.Code)
	}
	// Oversized manifest.
	big := bytes.Repeat([]byte(" "), int(f.svc.maxManifest())+10)
	w = f.as("writer", "PUT", "/v2/acme/demo/manifests/big", big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big manifest = %d", w.Code)
	}
	// Oversized blob chunk.
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc = w.Header().Get("Location")
	big = bytes.Repeat([]byte("x"), int(f.svc.maxBlob())+10)
	w = f.as("writer", "PATCH", loc, big)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "SIZE_INVALID") {
		t.Fatalf("big blob = %d %s", w.Code, w.Body.String())
	}
	// Abort cleans the staging object.
	w = f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc = w.Header().Get("Location")
	if w := f.as("writer", "PATCH", loc, []byte("x")); w.Code != 202 {
		t.Fatalf("patch = %d", w.Code)
	}
	if w := f.as("writer", "DELETE", loc, nil); w.Code != 204 {
		t.Fatalf("abort = %d", w.Code)
	}
	if w := f.as("writer", "PATCH", loc, []byte("x")); w.Code != 404 {
		t.Fatalf("patch after abort = %d", w.Code)
	}
}

func TestBudgetNoList(t *testing.T) {
	f := newFixture(t)
	const layers = 2
	digs := make([]string, layers)
	f.resetCounts()
	for i := 0; i < layers; i++ {
		digs[i] = f.pushBlob(t, "writer", []byte{byte(i), 'l', 'a', 'y', 'e', 'r'})
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/app", []byte(imageManifest)); w.Code != 201 {
		t.Fatalf("manifest = %d", w.Code)
	}
	puts, gets, lists := f.st.counts()
	// 2N blob writes (staging + final blob) + manifest + tag pointer.
	if puts != 2*layers+2 {
		t.Fatalf("push puts = %d, want %d", puts, 2*layers+2)
	}
	// One tag-pointer probe + one staging read per blob (finalize verifies
	// the digest while spooling; the blob is never read back after commit).
	if gets != 1+layers {
		t.Fatalf("push reads = %d, want %d", gets, 1+layers)
	}
	if lists != 0 {
		t.Fatalf("push listed the bucket %d times", lists)
	}
	f.resetCounts()
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/app", nil); w.Code != 200 {
		t.Fatalf("pull manifest = %d", w.Code)
	}
	for _, d := range digs {
		if w := f.as("reader", "GET", "/v2/acme/demo/blobs/"+d, nil); w.Code != 200 {
			t.Fatalf("pull blob = %d", w.Code)
		}
	}
	_, gets, lists = f.st.counts()
	if gets != 1+1+layers {
		t.Fatalf("pull gets = %d, want %d", gets, 2+layers)
	}
	if lists != 0 {
		t.Fatalf("pull listed the bucket %d times", lists)
	}
}

// --- error mapping and edge paths ---------------------------------------------

func TestErrorCodesAndEdges(t *testing.T) {
	f := newFixture(t)
	// Unknown blob / manifest / digest.
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/sha256:"+strings.Repeat("a", 64), nil); w.Code != 404 || !strings.Contains(w.Body.String(), "BLOB_UNKNOWN") {
		t.Fatalf("unknown blob = %d %s", w.Code, w.Body.String())
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/not-a-digest", nil); w.Code != 400 || !strings.Contains(w.Body.String(), "DIGEST_INVALID") {
		t.Fatalf("bad digest = %d %s", w.Code, w.Body.String())
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/nope", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "MANIFEST_UNKNOWN") {
		t.Fatalf("unknown manifest = %d %s", w.Code, w.Body.String())
	}
	// Wrong methods.
	if w := f.as("reader", "POST", "/v2/acme/demo/manifests/x", nil); w.Code != 405 || w.Header().Get("Allow") == "" {
		t.Fatalf("manifest post = %d", w.Code)
	}
	if w := f.as("reader", "DELETE", "/v2/acme/demo/blobs/sha256:"+strings.Repeat("a", 64), nil); w.Code != 405 {
		t.Fatalf("blob delete = %d", w.Code)
	}
	if w := f.as("reader", "POST", "/v2/token", nil); w.Code != 405 {
		t.Fatalf("token post = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/tags/nope", nil); w.Code != 404 {
		t.Fatalf("tags shape = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme", nil); w.Code != 404 {
		t.Fatalf("short path = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests", nil); w.Code != 404 {
		t.Fatalf("no ref = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/uploads/x/y", nil); w.Code != 404 {
		t.Fatalf("upload tail = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/a/b", nil); w.Code != 404 {
		t.Fatalf("blob tail = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/nope/x", nil); w.Code != 404 {
		t.Fatalf("unknown kind = %d", w.Code)
	}
	// Invalid repository names never reach the store.
	if w := f.as("reader", "GET", "/v2/bad%20owner/demo/tags/list", nil); w.Code != 404 {
		t.Fatalf("bad owner = %d", w.Code)
	}
	// Store failure maps to 500 (never a panic).
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) {
		return nil, errors.New("boom")
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/x", nil); w.Code != 500 || !strings.Contains(w.Body.String(), "UNKNOWN") {
		t.Fatalf("store error = %d %s", w.Code, w.Body.String())
	}
	f.st.getFn = nil
	// Unexpected Get result variant.
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) {
		return store.NotModified{}, nil
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/sha256:"+strings.Repeat("a", 64), nil); w.Code != 500 {
		t.Fatalf("bad variant = %d", w.Code)
	}
	f.st.getFn = nil
	// Exists hook failure.
	old := f.svc.Exists
	f.svc.Exists = func(context.Context, string, string) (bool, error) { return false, errors.New("gone") }
	if w := f.as("reader", "GET", "/v2/acme/demo/tags/list", nil); w.Code != 500 {
		t.Fatalf("exists error = %d", w.Code)
	}
	f.svc.Exists = old
	// Identity unavailable.
	f.roles.err = &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	if w := f.req("GET", "/v2/acme/demo/tags/list", nil, nil); w.Code != 500 {
		t.Fatalf("identity down = %d", w.Code)
	}
	f.roles.err = nil
}

func TestMapStoreErr(t *testing.T) {
	oe := &Error{Status: 418, Code: "TEAPOT"}
	if got := mapStoreErr(oe); got != oe {
		t.Fatal("existing *Error must pass through")
	}
	if got := mapStoreErr(store.NewNotFound("k")); got.Code != CodeBlobUnknown || got.Status != 404 {
		t.Fatalf("notfound = %+v", got)
	}
	if got := mapStoreErr(store.NewInvalid("k", errors.New("bad"))); got.Code != CodeDigestInvalid {
		t.Fatalf("invalid = %+v", got)
	}
	if got := mapStoreErr(errors.New("x")); got.Status != 500 {
		t.Fatalf("other = %+v", got)
	}
	if got := mapStoreErr(nil); got != nil {
		t.Fatal("nil should stay nil")
	}
}

func TestParseHelpers(t *testing.T) {
	if !IsDigest("sha256:"+strings.Repeat("a", 64)) || IsDigest("sha256:short") || IsDigest("sha512:"+strings.Repeat("a", 64)) {
		t.Fatal("IsDigest")
	}
	for _, tag := range []string{"latest", "1.0.0", "_x", "A-b_c.d"} {
		if !IsTag(tag) {
			t.Fatalf("IsTag(%q) = false", tag)
		}
	}
	for _, tag := range []string{"", ".", "..", "-bad", "with/slash", strings.Repeat("x", 129)} {
		if IsTag(tag) {
			t.Fatalf("IsTag(%q) = true", tag)
		}
	}
	for _, raw := range []string{"repository:acme/demo:pull,push", "repository:acme/demo:pull"} {
		sc, ok := parseScope(raw)
		if !ok || !sc.grants("acme/demo", "pull") {
			t.Fatalf("parseScope(%q) = %+v %v", raw, sc, ok)
		}
		if raw == "repository:acme/demo:pull,push" && !sc.grants("acme/demo", "push") {
			t.Fatal("push not granted")
		}
	}
	for _, raw := range []string{"", "repository", "repository:", "other:x:y", "repository:x", "repository::pull"} {
		if _, ok := parseScope(raw); ok {
			t.Fatalf("parseScope(%q) accepted", raw)
		}
	}
	if (&tokenClaims{Scopes: []string{"nonsense"}}).scopeGrants("o/r", "pull") {
		t.Fatal("invalid scopes must not grant")
	}
	if got := parseRange("bytes=0-4"); got == nil || got[0] != 0 || got[1] != 5 {
		t.Fatalf("range = %v", got)
	}
	if got := parseRange("bytes=3-3"); got == nil || got[1] != 4 {
		t.Fatalf("range single = %v", got)
	}
	for _, h := range []string{"", "bytes=", "bytes=5", "bytes=x-2", "bytes=5-2", "bytes=0-1,2-3"} {
		if got := parseRange(h); got != nil {
			t.Fatalf("parseRange(%q) = %v", h, got)
		}
	}
	if n, ok := parseChunkStart("bytes 4-9"); !ok || n != 4 {
		t.Fatalf("chunk start = %d %v", n, ok)
	}
	if n, ok := parseChunkStart("12-20"); !ok || n != 12 {
		t.Fatalf("chunk start = %d %v", n, ok)
	}
	for _, h := range []string{"", "x", "-2", "a-b"} {
		if _, ok := parseChunkStart(h); ok {
			t.Fatalf("parseChunkStart(%q) accepted", h)
		}
	}
	// The manifest media type falls back when the body has none.
	if got := manifestMediaType([]byte(`{}`)); got != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestTagCASRetryWithoutCurrent(t *testing.T) {
	f := newFixture(t)
	other := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[],"config":{"digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","size":1}}`)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/retry", []byte(imageManifest)); w.Code != 201 {
		t.Fatalf("seed = %d", w.Code)
	}
	// A store that loses the CAS without telling us the current version: the
	// retry re-reads the pointer instead of failing.
	calls := 0
	f.st.putFn = func(key string, err error) error {
		if err == nil && strings.Contains(key, "/tags/") && calls < 1 {
			calls++
			return store.NewPrecondition("k", "")
		}
		return err
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/retry", other); w.Code != 201 {
		t.Fatalf("retry put = %d %s", w.Code, w.Body.String())
	}
	// Two consecutive losses exhaust the retry and answer 409.
	calls = 0
	f.st.putFn = func(key string, err error) error {
		if err == nil && strings.Contains(key, "/tags/") {
			calls++
			return store.NewPrecondition("k", "")
		}
		return err
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/retry", []byte(imageManifest)); w.Code != 409 {
		t.Fatalf("exhausted retry = %d %s", w.Code, w.Body.String())
	}
}

func TestServiceFallbacks(t *testing.T) {
	mem := store.NewMemory()
	svc := New(mem, nil) // nil roles → principal flags
	pub := auth.Principal{Name: "alice"}
	if err := svc.CanPull(context.Background(), "a", "b", pub); err != nil {
		t.Fatalf("pull = %v", err)
	}
	// AnonymousRead default false → 401.
	if err := svc.CanPull(context.Background(), "a", "b", auth.Anonymous()); err == nil {
		t.Fatal("anonymous pull must be denied without roles or anonymous_read")
	}
	svc.AnonymousRead = true
	if err := svc.CanPull(context.Background(), "a", "b", auth.Anonymous()); err != nil {
		t.Fatalf("anon pull = %v", err)
	}
	if err := svc.CanPush(context.Background(), "a", "b", auth.Anonymous()); err == nil || err.Status != 401 {
		t.Fatalf("anon push = %+v", err)
	}
	if err := svc.CanPush(context.Background(), "a", "b", pub); err == nil || err.Status != 403 {
		t.Fatalf("no-write push = %+v", err)
	}
	if err := svc.CanPush(context.Background(), "a", "b", auth.Principal{Name: "w", Write: true}); err != nil {
		t.Fatalf("write push = %v", err)
	}
	if err := svc.CanPush(context.Background(), "a", "b", auth.Principal{Name: "adm", Admin: true}); err != nil {
		t.Fatalf("admin push = %v", err)
	}
	// Limits resolve to the compiled-in defaults.
	if svc.maxBlob() != DefaultMaxBlobBytes || svc.maxManifest() != DefaultMaxManifestBytes ||
		svc.uploadTTL() != DefaultUploadTTL || svc.tokenTTL() != DefaultTokenTTL {
		t.Fatal("default limits")
	}
	// Features nil / ok=false both fail open.
	if !svc.FeaturesOn(context.Background(), "a", "b") {
		t.Fatal("nil features must be enabled")
	}
	svc.Features = func(context.Context, string, string) (config.ResolvedFeatures, bool) {
		return config.ResolvedFeatures{}, false
	}
	if !svc.FeaturesOn(context.Background(), "a", "b") {
		t.Fatal("unreadable features must fail open")
	}
	svc.Features = func(context.Context, string, string) (config.ResolvedFeatures, bool) {
		return config.ResolvedFeatures{}, true
	}
	if svc.FeaturesOn(context.Background(), "a", "b") {
		t.Fatal("explicit off must disable")
	}
	// No Exists hook → exists.
	if ok, err := svc.RepoExists(context.Background(), "a", "b"); err != nil || !ok {
		t.Fatalf("RepoExists = %v %v", ok, err)
	}
	// Missing repo.
	svc.Exists = func(context.Context, string, string) (bool, error) { return false, nil }
	if ok, _ := svc.RepoExists(context.Background(), "a", "b"); ok {
		t.Fatal("missing repo reported present")
	}
}

func TestSpoolFallback(t *testing.T) {
	f := newFixture(t)
	// A SpoolDir that is actually a file makes MkdirAll fail; the OS temp
	// dir takes over (the LFS spool precedent) and uploads still work.
	dir := t.TempDir()
	file := dir + "/not-a-dir"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.SpoolDir = file
	dig := f.pushBlob(t, "writer", []byte("fallback"))
	if w := f.as("reader", "GET", "/v2/acme/demo/blobs/"+dig, nil); w.Code != 200 {
		t.Fatalf("fallback blob = %d", w.Code)
	}
}

func TestTokenEndpointEdges(t *testing.T) {
	f := newFixture(t)
	// Malformed scopes are ignored, no scopes issues a token with no access.
	w := f.req("GET", "/v2/token?scope=bogus&scope=repository:acme/demo:teleport", nil, nil)
	if w.Code != 200 {
		t.Fatalf("no grant token = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Token  string `json:"token"`
		Access []any  `json:"access"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Token == "" || len(body.Access) != 0 {
		t.Fatalf("body = %+v", body)
	}
	// /v2/auth is an alias.
	if w := f.req("GET", "/v2/auth", nil, nil); w.Code != 200 {
		t.Fatalf("auth alias = %d", w.Code)
	}
	// Invalid static credentials on the token endpoint are a 401 challenge.
	r := httptest.NewRequest("GET", "/v2/token", nil)
	r.Header.Set("Authorization", "Bearer wgp1.bad.sig")
	ww := httptest.NewRecorder()
	f.h.ServeHTTP(ww, r)
	if ww.Code != 401 || ww.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("bad token = %d", ww.Code)
	}
	// The token challenge names the realm from public_url when configured.
	f.svc.PublicURL = "https://git.example/"
	f.svc.ServiceName = "registry.example"
	w = f.req("POST", "/v2/acme/demo/blobs/uploads/", nil, nil)
	ch := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(ch, `realm="https://git.example/v2/token"`) || !strings.Contains(ch, `service="registry.example"`) {
		t.Fatalf("challenge = %q", ch)
	}
	// Pull denied for an authenticated principal without read → 403 (no challenge).
	f.roles.roles["bob|acme/demo"] = identity.Role("")
	f.roles.public["acme/demo"] = false
	w = f.as("bob", "GET", "/v2/acme/demo/tags/list", nil)
	if w.Code != 403 {
		t.Fatalf("no-read = %d", w.Code)
	}
}

func TestAbortAndStartEdges(t *testing.T) {
	f := newFixture(t)
	// Wrong method on the uploads collection.
	if w := f.as("writer", "GET", "/v2/acme/demo/blobs/uploads/", nil); w.Code != 405 {
		t.Fatalf("uploads get = %d", w.Code)
	}
	// Unknown method on an upload id.
	if w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/xyz", nil); w.Code != 405 {
		t.Fatalf("upload post = %d", w.Code)
	}
	// Start upload validates the repo name.
	svcErr := f.svc.StartUpload
	_ = svcErr
	if _, err := f.svc.StartUpload(context.Background(), "bad owner", "demo"); err == nil {
		t.Fatal("invalid repo started an upload")
	}
	// Finalize with a bad digest shape.
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	if w := f.as("writer", "PATCH", loc, []byte("x")); w.Code != 202 {
		t.Fatalf("patch = %d", w.Code)
	}
	if w := f.as("writer", "PUT", loc+"?digest=nope", nil); w.Code != 400 {
		t.Fatalf("bad finalize digest = %d", w.Code)
	}
	// Finalize on an upload that does not exist.
	if w := f.as("writer", "PUT", "/v2/acme/demo/blobs/uploads/unsigned?digest=sha256:"+strings.Repeat("a", 64), nil); w.Code != 404 {
		t.Fatalf("unknown finalize = %d", w.Code)
	}
}

func TestPutManifestValidation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.PutManifest(context.Background(), "bad owner", "demo", "x", []byte("{}"), ""); err == nil {
		t.Fatal("bad repo must fail")
	}
	if _, err := f.svc.PutManifest(context.Background(), "acme", "demo", "bad/tag", []byte("{}"), ""); err == nil || err.Code != CodeManifestUnknown {
		t.Fatalf("bad ref = %+v", err)
	}
	if _, err := f.svc.PutManifest(context.Background(), "acme", "demo", "x", []byte("{}"), "text/plain"); err != nil {
		t.Fatalf("content-type override = %v", err)
	}
	// GetManifest with a malformed tag pointer body.
	if _, err := f.st.Put(context.Background(), tagKey("acme", "demo", "corrupt"), store.PutBody{Bytes: []byte("nope")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetManifest(context.Background(), "acme", "demo", "corrupt"); err == nil || err.Code != CodeManifestUnknown {
		t.Fatalf("corrupt tag = %+v", err)
	}
	if _, err := f.svc.GetManifest(context.Background(), "acme", "demo", "unparseable/tag"); err == nil {
		t.Fatal("bad ref must fail")
	}
}

func TestDigestBytesAndKeys(t *testing.T) {
	if got := DigestBytes([]byte("x")); got != digestOf([]byte("x")) {
		t.Fatalf("digest = %q", got)
	}
	if got := RepoBase("o", "r"); got != "repos/o/r/packages/registry" {
		t.Fatalf("base = %q", got)
	}
	if DigestHex("sha256:abc") != "abc" {
		t.Fatal("DigestHex")
	}
}
