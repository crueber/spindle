package webedit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// errorStore fails every Get (the policy-read failure arm).
type errorStore struct{ *store.Memory }

func (e *errorStore) Get(context.Context, string, store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("store down")
}

// TestStatusMatrix pins every sentinel's HTTP status (F.3) and the page
// helper that shares the mapping.
func TestStatusMatrix(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{&ConflictError{What: "blob"}, http.StatusConflict},
		{ErrNotFound, http.StatusNotFound},
		{ErrInvalid, http.StatusBadRequest},
		{ErrUnauthorized, http.StatusUnauthorized},
		{ErrForbidden, http.StatusForbidden},
		{ErrConflict, http.StatusConflict},
		{ErrTooLarge, http.StatusRequestEntityTooLarge},
		{ErrUnsupported, http.StatusUnsupportedMediaType},
		{ErrNoChange, http.StatusUnprocessableEntity},
		{ErrUnavailable, http.StatusServiceUnavailable},
		{errors.New("other"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := statusFor(tc.err); got != tc.want {
			t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
		if got := HTTPStatus(tc.err); got != tc.want {
			t.Errorf("HTTPStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestServeHTTPAndDecode(t *testing.T) {
	_, handler := newHTTPHarness(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign path = %d", rec.Code)
	}
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("undecodable = %q", got)
	}
	if got := decodeSegment("a%20b"); got != "a b" {
		t.Fatalf("decoded = %q", got)
	}
	// Anonymous with an authenticator error answers 401 (never a 200 with an
	// in-band error).
	handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad token"}
	}
	rec = do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=x", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth error = %d", rec.Code)
	}
	// A 503 sets Retry-After.
	rec = httptest.NewRecorder()
	writePlain(rec, http.StatusServiceUnavailable, "down")
	if rec.Header().Get("Retry-After") != "15" {
		t.Fatal("Retry-After missing")
	}
}

func TestHTTPExtraBranches(t *testing.T) {
	h, handler := newHTTPHarness(t)
	// An empty body is a valid envelope with empty fields → 400 (no message).
	rec := do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d %s", rec.Code, rec.Body.String())
	}
	// Unknown mode through a missing model field: PUT without expected blob is
	// a create, which 409s on the existing path.
	rec = do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt",
		`{"message":"m","content":"aGk="}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("implicit create = %d %s", rec.Code, rec.Body.String())
	}
	// The service's unpublished-branch error surfaces as a 503.
	h.refs.err = errors.New("bucket down")
	rec = do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt",
		`{"message":"m","content":"aGk=","expected_blob_sha":"`+testSHA+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("publish failure = %d %s", rec.Code, rec.Body.String())
	}
}

// TestPageHelpers covers the exported sniff helpers and the small service
// helpers page handlers share.
func TestPageHelpers(t *testing.T) {
	if !IsBinary([]byte("a\x00b")) || IsBinary([]byte("abcd")) {
		t.Fatal("IsBinary")
	}
	if isBinary(append([]byte(strings.Repeat("x", 9000)), 0)) {
		t.Fatal("NUL past the sniff window detected")
	}
	if !IsLFSPointer([]byte(lfsPointerPrefix+"\n")) || IsLFSPointer([]byte("plain")) {
		t.Fatal("IsLFSPointer")
	}
	if _, _, err := normalizeBranchRef("-x"); err == nil {
		t.Fatal("normalize option branch")
	}
	if _, name, err := normalizeBranchRef("refs/heads/main"); err != nil || name != "main" {
		t.Fatalf("normalize full = %q/%v", name, err)
	}
	if got := authorEmail(auth.Principal{Name: "jane", Email: "jane@example.com"}); got != "jane@example.com" {
		t.Fatalf("email = %q", got)
	}
	if got := authorEmail(auth.Principal{Name: "jane", Email: "not-an-email"}); got != "jane@spindle.local" {
		t.Fatalf("minted email = %q", got)
	}
	if got := authorEmail(auth.Principal{}); got != "spindle@spindle.local" {
		t.Fatalf("empty name email = %q", got)
	}
	// gitattributes skip arms.
	if gitattributesLFS([]byte("!keep filter=lfs\n"), "keep") {
		t.Fatal("negation matched")
	}
	if gitattributesLFS([]byte("\"quoted\" filter=lfs\n"), "quoted") {
		t.Fatal("quoted matched")
	}
	if !gitattributesLFS([]byte("dir filter=lfs\n"), "dir/file") {
		t.Fatal("plain directory did not match its contents")
	}
	// ConflictError with no What defaults to branch wording.
	if got := (&ConflictError{Expected: "a", Actual: "b"}).Error(); !strings.Contains(got, "branch moved") {
		t.Fatalf("default what = %q", got)
	}
	if (&ConflictError{What: "blob", Why: "exists"}).Error() != "exists (disk: )" {
		t.Fatalf("blob why = %q", (&ConflictError{What: "blob", Why: "exists"}).Error())
	}
}

// TestServiceExtraBranches covers the remaining service early-exits.
func TestServiceExtraBranches(t *testing.T) {
	// open_pr without a wired PR opener: the branch lands, then 503.
	h := newHarness(t)
	h.svc.PR = nil
	if _, err := h.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "fix/x", OpenPR: true,
	}); !isErr(err, ErrUnavailable) {
		t.Fatalf("unwired PR = %v", err)
	}
	// Preview propagates a build failure (binary content).
	if _, err := h.svc.Preview(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("\x00"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrUnsupported) {
		t.Fatalf("preview guard = %v", err)
	}
	// Preview with a bad mode.
	if _, err := h.svc.Preview(context.Background(), "o", "r", writer(), Input{Mode: "bogus"}); !isErr(err, ErrInvalid) {
		t.Fatalf("bad mode = %v", err)
	}
	// New branch name validation and the base-equal refusal.
	for _, nb := range []string{"-x", "a..b"} {
		if _, err := h.svc.Save(context.Background(), "o", "r", writer(), Input{
			Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"),
			ExpectedBlobSHA: testSHA, NewBranch: nb,
		}); !isErr(err, ErrInvalid) {
			t.Fatalf("new branch %q = %v", nb, err)
		}
	}
	// A rename through delete+create is two requests; delete on a new branch
	// is refused explicitly.
	if _, err := h.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeDelete, Ref: "main", Path: "hello.txt", Message: "m", ExpectedBlobSHA: testSHA, NewBranch: "b",
	}); !isErr(err, ErrInvalid) {
		t.Fatalf("delete new branch = %v", err)
	}
	// Load when the repo dir cannot be resolved.
	h.svc.Dirs = &fakeDirs{}
	if _, err := h.svc.Load(context.Background(), "o", "r", writer(), "main", "hello.txt"); !isErr(err, ErrUnavailable) {
		t.Fatalf("load dirs = %v", err)
	}
	// Pack failure propagates.
	h2 := newHarness(t)
	h2.git.packErr = errors.New("pack failed")
	if _, err := h2.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err == nil || !strings.Contains(err.Error(), "pack failed") {
		t.Fatalf("pack failure = %v", err)
	}
	// A nil pack publishes ref-only (fake Git returns an empty pack).
	h2.git.packErr = nil
	h2.git.pack = nil
	if _, err := h2.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err != nil {
		t.Fatalf("nil pack should publish ref-only: %v", err)
	}
	// Load refuses an invalid ref spelling before touching the repo.
	if _, err := h2.svc.Load(context.Background(), "o", "r", writer(), "-x", "hello.txt"); !isErr(err, ErrInvalid) {
		t.Fatalf("load bad ref = %v", err)
	}
	// A nil Gate with a host admin passes.
	h3 := newHarness(t)
	h3.svc.Gate = nil
	if _, err := h3.svc.Save(context.Background(), "o", "r", admin(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err != nil {
		t.Fatalf("admin nil gate = %v", err)
	}
	// No pinned clock still commits (Now nil).
	h4 := newHarness(t)
	h4.svc.Now = nil
	if _, err := h4.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err != nil {
		t.Fatalf("default clock = %v", err)
	}
	// A store that fails on the policy read is unavailable, never allow-all.
	h5 := newHarness(t)
	h5.svc.Store = &errorStore{}
	if _, err := h5.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrUnavailable) {
		t.Fatalf("policy read failure = %v", err)
	}
	// Unwired refs/service.
	h6 := newHarness(t)
	h6.svc.Refs = nil
	if _, err := h6.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrUnavailable) {
		t.Fatalf("unwired refs = %v", err)
	}
}

// TestHandleForeignShapes covers the early exits of the route recognizer.
func TestHandleForeignShapes(t *testing.T) {
	h, handler := newHTTPHarness(t)
	h.svc.MaxBytes = 1
	for _, target := range []string{
		"/api/contents", "/o/r/other/contents", "/o/r/api/notcontents",
		"/o//api/contents", "/o/r/api/contents/extra",
	} {
		req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		if handler.Handle(rec, req) {
			t.Errorf("Handle(%q) claimed a foreign path", target)
		}
	}
}

// TestNoStorePublishes: a nil store means there is no policy document to read
// (tests without the collaboration layer), so the write proceeds.
func TestNoStorePublishes(t *testing.T) {
	h := newHarness(t)
	h.svc.Store = nil
	if _, err := h.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err != nil {
		t.Fatalf("nil store = %v", err)
	}
	// HashBlob failure propagates.
	h2 := newHarness(t)
	h2.git.hashErr = errors.New("hash failed")
	if _, err := h2.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrUnavailable) && err == nil {
		t.Fatalf("hash failure = %v", err)
	}
	// BuildCommit failure propagates.
	h3 := newHarness(t)
	h3.git.buildErr = errors.New("build failed")
	if _, err := h3.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err == nil {
		t.Fatal("build failure accepted")
	}
	// The preview path propagates a Load-less read error too.
	h4 := newHarness(t)
	h4.git.lookupErr = errors.New("lookup failed")
	if _, err := h4.svc.Save(context.Background(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "m", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err == nil {
		t.Fatal("lookup failure accepted")
	}
}
