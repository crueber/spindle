package archive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

func TestStatusForTable(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 200},
		{ErrNotFound, 404},
		{ErrInvalid, 400},
		{ErrUnauthorized, 401},
		{ErrForbidden, 403},
		{ErrUnavailable, 503},
		{errors.New("boom"), 500},
	}
	for _, tc := range cases {
		if got := statusFor(tc.err); got != tc.want {
			t.Fatalf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestMatchETagVariants(t *testing.T) {
	for _, h := range []string{"*", `"abc"`, `W/"abc"`, `"x", "abc"`} {
		if !matchETag(h, "abc") {
			t.Fatalf("failed to match %q", h)
		}
	}
	if matchETag(`"other"`, "abc") || matchETag("", "abc") {
		t.Fatal("matched a different tag")
	}
}

func TestSanitizeEdges(t *testing.T) {
	if got := sanitizePrefix("///"); got != "repo" {
		t.Fatalf("prefix %q", got)
	}
	if got := sanitizeName("///"); got != "---" {
		t.Fatalf("name %q", got)
	}
	if got := sanitizePrefix("a-b_c.d"); got != "a-b_c.d" {
		t.Fatalf("prefix %q", got)
	}
}

func TestHandlerAuthErrorAndNil(t *testing.T) {
	h := &Handler{Svc: &Service{}, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}
	}}
	r := httptest.NewRequest("GET", "/a/b/archive/main.tar.gz", nil)
	w := httptest.NewRecorder()
	if !h.HandleRepo(w, r, git.RepoId{Owner: "a", Name: "b"}, []string{"archive", "main.tar.gz"}) {
		t.Fatal("not claimed")
	}
	if w.Code != 401 {
		t.Fatalf("auth: %d", w.Code)
	}
	h = &Handler{Svc: nil}
	w = httptest.NewRecorder()
	h.HandleRepo(w, r, git.RepoId{Owner: "a", Name: "b"}, []string{"archive", "main.tar.gz"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc: %d", w.Code)
	}
}

func TestServeHTTPAnswers404(t *testing.T) {
	w := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}

func TestRequireReadPaths(t *testing.T) {
	svc := &Service{}
	if err := svc.requireRead(context.Background(), "o", "r", auth.Principal{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.requireRead(context.Background(), "o", "r", auth.Principal{Name: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.requireRead(context.Background(), "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous: %v", err)
	}
	svc.Roles = &fakeRoles{aerr: &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}}
	if err := svc.requireRead(context.Background(), "o", "r", auth.Principal{Name: "u"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
}

func TestTreeAndStreamBackendFailures(t *testing.T) {
	dir := t.TempDir()
	g := NewSubprocessGit(filepath.Join(dir, "no-such-git"))
	if _, err := g.Tree(context.Background(), dir, "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("tree: %v", err)
	}
	err := g.Stream(context.Background(), dir, []string{"archive"}, io.Discard)
	if err == nil {
		t.Fatal("stream with a missing binary succeeded")
	}
}

func TestCancelledPoolSkipsRun(t *testing.T) {
	g := NewSubprocessGit("")
	g.Pool = newGitPool(1)
	g.Pool.sem <- struct{}{} // occupy the only slot
	defer func() { <-g.Pool.sem }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Tree(ctx, t.TempDir(), "main"); err == nil {
		t.Fatal("canceled tree succeeded")
	}
}

func TestStreamWriterFailureKillsChild(t *testing.T) {
	svc, _, _ := newService(t)
	failing := &failWriter{}
	err := svc.Git.Stream(context.Background(), svc.Dirs.(fixedDirs).dir,
		ArchiveArgv("main", "tar.gz", "widget"), failing)
	if err == nil {
		t.Fatal("expected a copy error")
	}
}

type failWriter struct{}

func (f *failWriter) Write(p []byte) (int, error) { return 0, errors.New("client gone") }

func TestArchiveStreamUnauthorizedRole(t *testing.T) {
	svc, _, _ := newService(t)
	svc.Roles = &fakeRoles{aerr: &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad principal"}}
	h := &Handler{Svc: svc}
	if w := do(h, "GET", "/acme/widget/archive/main.tar.gz", nil); w.Code != 401 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestNewServiceConstructor(t *testing.T) {
	svc := New(&fakeRoles{role: string(identity.RoleRead)})
	if svc.Roles == nil {
		t.Fatal("roles not set")
	}
}

func TestSubprocessGitDefaultPool(t *testing.T) {
	svc, _, _ := newService(t)
	g := &SubprocessGit{}
	// nil Pool + empty Binary exercise the defaults.
	if _, err := g.Tree(context.Background(), svc.Dirs.(fixedDirs).dir, "main"); err != nil {
		t.Fatal(err)
	}
}

func TestTempDirWritable(t *testing.T) {
	if _, err := os.Stat(os.TempDir()); err != nil {
		t.Fatal(err)
	}
}
