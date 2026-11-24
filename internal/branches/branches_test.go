package branches

// branches_test.go — the service's gate/policy/publish contract with fakes
// plus a real-git resolver test (the argv is pinned in 04_git.md §16.2).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type fakeGate struct {
	readErr, pushErr *auth.AuthError
	readN, pushN     int
}

func (g *fakeGate) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	g.readN++
	return g.readErr
}
func (g *fakeGate) CheckPush(context.Context, string, string, auth.Principal) *auth.AuthError {
	g.pushN++
	return g.pushErr
}

type fakeDirs struct {
	dir string
	err error
}

func (d *fakeDirs) Dir(context.Context, string) (string, error) { return d.dir, d.err }

type fakeGit struct {
	sha string
	err error
	rev string
}

func (g *fakeGit) ResolveCommit(_ context.Context, _, rev string) (string, error) {
	g.rev = rev
	if g.err != nil {
		return "", g.err
	}
	return g.sha, nil
}

type fakeRefs struct {
	created, deleted bool
	repo, name, sha  string
	createErr        error
	deleteErr        error
}

func (r *fakeRefs) CreateBranch(_ context.Context, repo, name, sha string, _ map[string]string) error {
	r.created, r.repo, r.name, r.sha = true, repo, name, sha
	return r.createErr
}
func (r *fakeRefs) DeleteBranch(_ context.Context, repo, name, old string, _ map[string]string) error {
	r.deleted, r.repo, r.name, r.sha = true, repo, name, old
	return r.deleteErr
}

const testSHA = "1111111111111111111111111111111111111111"

type fixture struct {
	svc    *Service
	gate   *fakeGate
	git    *fakeGit
	refs   *fakeRefs
	st     store.ObjectStore
	def    string
	defErr error
	dir    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		gate: &fakeGate{},
		git:  &fakeGit{sha: testSHA},
		refs: &fakeRefs{},
		st:   store.NewMemory(),
		def:  "main",
		dir:  t.TempDir(),
	}
	f.svc = &Service{
		Store: f.st, Gate: f.gate, Dirs: &fakeDirs{dir: f.dir}, Git: f.git, Refs: f.refs,
		DefaultBranch: func(context.Context, string, string) (string, error) { return f.def, f.defErr },
	}
	return f
}

func writer() auth.Principal { return auth.Principal{Name: "jane", Write: true} }

func putPolicy(t *testing.T, f *fixture, body string) {
	t.Helper()
	_, err := store.PutBytes(context.Background(), f.st, PolicyKey("acme", "widgets"), []byte(body), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateBranchName(t *testing.T) {
	if got, err := ValidateBranchName("refs/heads/feature/x"); err != nil || got != "feature/x" {
		t.Fatalf("valid = %q %v", got, err)
	}
	if got, err := ValidateBranchName("  topic  "); err != nil || got != "topic" {
		t.Fatalf("trim = %q %v", got, err)
	}
	for _, bad := range []string{"", "HEAD", "refs/heads/", "-x", "a..b", "a//b", "a b", "a*b", "a:b", "a.lock", "a@{1}"} {
		if _, err := ValidateBranchName(bad); err == nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateBranchName(%q) = %v", bad, err)
		}
	}
}

func TestCreatePublishesZeroOld(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "feature/x", "main"); err != nil {
		t.Fatal(err)
	}
	if !f.refs.created || f.refs.name != "feature/x" || f.refs.sha != testSHA || f.refs.repo != "acme/widgets" {
		t.Fatalf("publish = %+v", f.refs)
	}
	if f.git.rev != "main" || f.gate.readN != 1 || f.gate.pushN != 1 {
		t.Fatalf("gate/rev = %+v %q", f.gate, f.git.rev)
	}

	// Empty `from` starts at the default branch.
	f2 := newFixture(t)
	if err := f2.svc.Create(context.Background(), "acme", "widgets", writer(), "topic", ""); err != nil {
		t.Fatal(err)
	}
	if f2.git.rev != "main" {
		t.Fatalf("default start = %q", f2.git.rev)
	}

	// Unborn default with empty from: a clear 400.
	f3 := newFixture(t)
	f3.def = ""
	if err := f3.svc.Create(context.Background(), "acme", "widgets", writer(), "topic", ""); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("unborn default = %v", err)
	}
}

func TestCreateGateAndErrors(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Create(context.Background(), "acme", "widgets", auth.Anonymous(), "x", "main"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	f.svc.Gate = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", auth.Principal{Name: "bob"}, "x", "main"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no write = %v", err)
	}
	f.svc.Gate = f.gate
	// Gate errors map onto the sentinels (visibility first, then push floor).
	f.gate.readErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read gate = %v", err)
	}
	f.gate.readErr = &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sign in"}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("read gate = %v", err)
	}
	f.gate.readErr = &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("read gate = %v", err)
	}
	f.gate.readErr = nil
	f.gate.pushErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "no push"}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "no push") {
		t.Fatalf("push gate = %v", err)
	}
	f.gate.pushErr = &auth.AuthError{Kind: auth.ErrUnavailable, Why: "db"}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("push gate = %v", err)
	}
	f.gate.pushErr = nil

	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("option-shaped from = %v", err)
	}
	f.git.err = errors.New("bad revision")
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve = %v", err)
	}
	f.git.err = nil

	// Unwired seams fail loudly instead of silently succeeding.
	f.svc.Refs = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no publisher = %v", err)
	}
	f.svc.Refs = f.refs
	f.svc.Dirs = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no dirs = %v", err)
	}
	f.svc.Dirs = &fakeDirs{err: errors.New("no repo")}
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dir error = %v", err)
	}
	f.svc.Dirs = &fakeDirs{dir: f.dir}
	f.svc.DefaultBranch = func(context.Context, string, string) (string, error) { return "", errors.New("store") }
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("default seam error = %v", err)
	}
	f.svc.DefaultBranch = func(context.Context, string, string) (string, error) { return "main", nil }
	f.svc.Store = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no store = %v", err)
	}
}

func TestCreatePolicyAndPublisherErrors(t *testing.T) {
	f := newFixture(t)
	putPolicy(t, f, `{"version":1,"rules":[{"name":"lockdown","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"restricts":["create"]}}}]}`)
	err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "release/1", "main")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "rejected by rule 'lockdown'") {
		t.Fatalf("policy = %v", err)
	}
	if f.refs.created {
		t.Fatal("policy denial must not publish")
	}
	// A parse error fails closed.
	putPolicy(t, f, `{"version":1,`)
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "topic", "main"); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("unparseable = %v", err)
	}
	// Publisher conflict propagates as the sentinel.
	f2 := newFixture(t)
	f2.refs.createErr = ErrConflict
	if err := f2.svc.Create(context.Background(), "acme", "widgets", writer(), "topic", "main"); !errors.Is(err, ErrConflict) {
		t.Fatalf("publisher = %v", err)
	}
}

func TestDeleteRefusesDefaultAndProtected(t *testing.T) {
	// Default branch: refused before resolve/publish.
	f := newFixture(t)
	err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "main")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("default delete = %v", err)
	}
	if f.refs.deleted || f.git.rev != "" {
		t.Fatal("default delete touched git/publisher")
	}

	// Policy-protected branch: the rule name is reported, nothing published.
	putPolicy(t, f, `{"version":1,"rules":[{"name":"keep-release","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"restricts":["delete"]}}}]}`)
	err = f.svc.Delete(context.Background(), "acme", "widgets", writer(), "release/1")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "rejected by rule 'keep-release'") {
		t.Fatalf("protected delete = %v", err)
	}
	if f.refs.deleted {
		t.Fatal("protected delete published")
	}

	// A bypass author still needs the ref's own rule to admit them; the
	// managed rule's bypass role comes from the same document.
	putPolicy(t, f, `{"version":1,"rules":[{"name":"keep-release","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"restricts":["delete"],"bypass":["jane"]}}}]}`)
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "release/1"); err != nil {
		t.Fatalf("bypassed delete = %v", err)
	}
	if !f.refs.deleted || f.refs.name != "release/1" || f.refs.sha != testSHA {
		t.Fatalf("delete publish = %+v", f.refs)
	}
}

func TestDeleteErrors(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Delete(context.Background(), "acme", "widgets", auth.Anonymous(), "topic"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name = %v", err)
	}
	f.defErr = errors.New("boom")
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("default seam = %v", err)
	}
	f.defErr = nil
	f.svc.DefaultBranch = nil
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil default seam = %v", err)
	}
	f.svc.DefaultBranch = func(context.Context, string, string) (string, error) { return "main", nil }
	f.git.err = errors.New("missing")
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing branch = %v", err)
	}
	f.git.err = nil
	f.svc.Refs = nil
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no publisher = %v", err)
	}
	f.svc.Refs = f.refs
	f.refs.deleteErr = ErrConflict
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cas = %v", err)
	}
}

func TestPolicyReadErrors(t *testing.T) {
	f := newFixture(t)
	f.svc.Store = &failingStore{ObjectStore: f.st}
	if err := f.svc.Delete(context.Background(), "acme", "widgets", writer(), "topic"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("store read = %v", err)
	}
}

// failingStore fails every read.
type failingStore struct{ store.ObjectStore }

func (f *failingStore) Get(context.Context, string, store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("store down")
}

// --- real git resolver -------------------------------------------------------

func TestSubprocessGitResolve(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "bare.git")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", work)
	run("init", "--bare", "-b", "main", bare)
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", work, "add", "f")
	run("-C", work, "commit", "-m", "one")
	sha := run("-C", work, "rev-parse", "HEAD")
	run("-C", work, "push", bare, "main")
	resolver := NewSubprocessGit("")
	got, err := resolver.ResolveCommit(context.Background(), bare, "main")
	if err != nil || got != sha {
		t.Fatalf("resolve = %q %v", got, err)
	}
	got, err = resolver.ResolveCommit(context.Background(), bare, sha)
	if err != nil || got != sha {
		t.Fatalf("resolve sha = %q %v", got, err)
	}
	if _, err := resolver.ResolveCommit(context.Background(), bare, "absent"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("absent = %v", err)
	}
	if _, err := resolver.ResolveCommit(context.Background(), bare, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty = %v", err)
	}
	if _, err := resolver.ResolveCommit(context.Background(), bare, "-1"); err == nil {
		t.Fatal("option-shaped rev must fail")
	}
	// Anonymous principal on a nil gate is still refused.
	f := newFixture(t)
	f.svc.Gate = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", auth.Anonymous(), "x", "main"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("nil gate anonymous = %v", err)
	}
	f.svc.Refs = f.refs
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "x", "main"); err != nil {
		t.Fatalf("nil gate writer = %v", err)
	}
}

func TestMapAuthErrAllKinds(t *testing.T) {
	if mapAuthErr(nil) != nil {
		t.Fatal("nil auth error must map to nil")
	}
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want error
	}{
		{auth.ErrUnauthorized, ErrUnauthorized},
		{auth.ErrForbidden, ErrForbidden},
		{auth.ErrInvalid, ErrInvalid},
		{auth.ErrUnavailable, ErrUnavailable},
	} {
		if err := mapAuthErr(&auth.AuthError{Kind: tc.kind, Why: "w"}); !errors.Is(err, tc.want) {
			t.Fatalf("kind %v = %v", tc.kind, err)
		}
	}
}

func TestCreateNilDefaultSeam(t *testing.T) {
	f := newFixture(t)
	f.svc.DefaultBranch = nil
	if err := f.svc.Create(context.Background(), "acme", "widgets", writer(), "topic", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil default seam = %v", err)
	}
}

func TestResolveCommitContextAndTimeoutDefault(t *testing.T) {
	resolver := NewSubprocessGit("git")
	if resolver.Binary != "git" || resolver.Timeout != ResolveTimeout {
		t.Fatalf("constructor = %+v", resolver)
	}
	// Timeout <= 0 falls back to the package default.
	resolver.Timeout = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.ResolveCommit(ctx, t.TempDir(), "HEAD"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled ctx = %v", err)
	}
}
