// fakes_test.go — the service-level doubles. They stand in only for seams the
// production composition provides (the repository source, the git primitive,
// the identity read gate, the P4 issue searcher); the repo-scope fixtures in
// service_test.go drive the REAL grep primitive over real bare repositories.
package search

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// TestMain pins the git identity for the real fixtures (the repo rule: tests
// that spawn git set GIT_AUTHOR_*/GIT_COMMITTER_* — runners and containers
// have no global git config).
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "t",
		"GIT_AUTHOR_EMAIL":    "t@t",
		"GIT_COMMITTER_NAME":  "t",
		"GIT_COMMITTER_EMAIL": "t@t",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

// ---- identity read gate ------------------------------------------------------

// matrixAccess mirrors identity.Service.CheckRead (01 §4.1) closely enough to
// drive the service's gating matrix: host admin allows; anonymous allows a
// public repository and is 401 elsewhere; a signed-in principal with the read
// role allows, and otherwise a public repository allows while a private one is
// 403. down simulates an identity-store outage (503-class).
type matrixAccess struct {
	mu     sync.Mutex
	public map[string]bool // "owner/name" → public
	role   map[string]bool // "owner/name" → signed-in principal has read
	down   bool
	calls  []string
}

func (m *matrixAccess) CheckRead(_ context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	id := owner + "/" + repo
	m.mu.Lock()
	m.calls = append(m.calls, id)
	down, public, role := m.down, m.public[id], m.role[id]
	m.mu.Unlock()

	if down {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "identity store is down"}
	}
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		if public {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if role || public {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}

// ---- repository source -------------------------------------------------------

// fakeRepos is the RepoSource double. resolve/warm are per-test behaviour; the
// zero value resolves nothing.
type fakeRepos struct {
	mu        sync.Mutex
	names     []string
	listErr   error
	listFn    func(ctx context.Context) ([]string, error)
	listCalls int
	resolve   func(repo, ref string) (Target, error)
	warm      func(ctx context.Context, repo string) (Target, bool, error)
}

func (f *fakeRepos) ListOwner(ctx context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	f.listCalls++
	fn := f.listFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string(nil), f.names...), nil
}

func (f *fakeRepos) ResolveRepo(_ context.Context, repo, ref string) (Target, error) {
	if f.resolve == nil {
		return Target{}, Wrap(ErrNotFound, "not found")
	}
	return f.resolve(repo, ref)
}

func (f *fakeRepos) WarmRefs(ctx context.Context, repo string) (Target, bool, error) {
	if f.warm == nil {
		return Target{}, false, nil
	}
	return f.warm(ctx, repo)
}

func (f *fakeRepos) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// ---- git primitive -----------------------------------------------------------

// fakeGit is the grep seam double and records concurrency per serving path so
// the tests can prove the request pool and the per-repository gate.
type fakeGit struct {
	mu       sync.Mutex
	active   map[string]int
	max      int
	calls    int
	patterns []string
	fn       func(ctx context.Context, repo *git.LocalRepo, rev string, q git.GrepQuery) (*git.GrepResult, error)
}

func (f *fakeGit) Grep(ctx context.Context, repo *git.LocalRepo, rev string, q git.GrepQuery) (*git.GrepResult, error) {
	f.mu.Lock()
	if f.active == nil {
		f.active = make(map[string]int)
	}
	f.active["*"]++
	if f.active["*"] > f.max {
		f.max = f.active["*"]
	}
	key := "<nil>"
	if repo != nil {
		key = repo.Path
	}
	f.active[key]++
	f.calls++
	f.patterns = append(f.patterns, q.Pattern)
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.active["*"]--
		f.active[key]--
		f.mu.Unlock()
	}()

	if f.fn == nil {
		return &git.GrepResult{Files: []git.GrepFile{}}, nil
	}
	return f.fn(ctx, repo, rev, q)
}

func (f *fakeGit) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.max
}

func (f *fakeGit) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---- issue searcher (P4 seam) ------------------------------------------------

// fakeIssues is called once per candidate from the owner-scope fan-out
// (search.go issueOwner), so every mutable field is mutex-guarded and the
// recorded last call is read through the accessors below.
type fakeIssues struct {
	mu      sync.Mutex
	section IssueSection
	err     error
	got     IssueQuery
	scope   IssueScope
}

func (f *fakeIssues) SearchIssues(_ context.Context, scope IssueScope, q IssueQuery) (IssueSection, error) {
	f.mu.Lock()
	f.scope, f.got = scope, q
	sec, err := cloneIssueSection(f.section), f.err
	f.mu.Unlock()

	if err != nil {
		return IssueSection{}, err
	}
	return sec, nil
}

// scopeOf / queryOf return the last recorded call (last writer wins across the
// fan-out; the assertions that read them use a single candidate).
func (f *fakeIssues) scopeOf() IssueScope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scope
}

func (f *fakeIssues) queryOf() IssueQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// cloneIssueSection hands every call its own hits slice: the service's
// normalizeIssueSection writes Labels in place, so sharing one backing array
// across concurrent calls would race even with the lock held on the fake.
func cloneIssueSection(sec IssueSection) IssueSection {
	out := sec
	out.Hits = append([]IssueHit(nil), sec.Hits...)
	out.Repos = append([]IssueRepo(nil), sec.Repos...)
	return out
}

// ---- real git fixtures -------------------------------------------------------

// searchFixture is a real bare repository built by a worktree commit and then
// cloned bare, so the searched copy has no worktree at all (the production
// serving shape).
type searchFixture struct {
	Bare   string
	Main   string // first commit ("main" tip after the second commit is Second)
	Second string
	Branch string
	Layer  *git.Layer
	Target Target
}

// searchGit runs one git command in dir with a deterministic config environment.
func searchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile is the fixture writer (directories are created by the caller).
func writeFile(name, body string) error { return os.WriteFile(name, []byte(body), 0o600) }

// newSearchFixture builds a real bare repository: a worktree repo committed
// twice, then cloned bare so the searched copy has no worktree at all (the
// production serving shape).
func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	src := t.TempDir()
	work := filepath.Join(src, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	searchGit(t, work, "init", "-b", "main", "-q")
	searchGit(t, work, "config", "core.autocrlf", "false")
	write := func(name, body string) {
		t.Helper()
		full := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# demo\nhello world\n")
	write("cmd/app/main.go", "package main\n\nfunc main() {\n\thello()\n}\n")
	write("pkg/util.go", "package pkg\n// HelloUtil does things\nfunc HelloUtil() {}\n")
	write("docs/notes.txt", "nothing here\n")
	searchGit(t, work, "add", "-A")
	searchGit(t, work, "commit", "-qm", "first")
	first := searchGit(t, work, "rev-parse", "HEAD")
	write("README.md", "# demo\nhello version two\n")
	searchGit(t, work, "add", "-A")
	searchGit(t, work, "commit", "-qm", "second")
	second := searchGit(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(src, "serving.git")
	searchGit(t, src, "clone", "--bare", "-q", work, bare)
	return &searchFixture{
		Bare:   bare,
		Main:   first,
		Second: second,
		Branch: "main",
		Layer:  git.NewLayer(),
		Target: Target{Repo: &git.LocalRepo{Path: bare}, Ref: "main", SHA: second},
	}
}

// newBareRepo builds an empty bare repository (unborn HEAD).
func newBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "empty.git")
	searchGit(t, t.TempDir(), "init", "--bare", "-q", dir)
	return dir
}

// allowAll is the read gate a test uses when visibility is not under test.
func allowAll() Access { return allowAllGate{} }

type allowAllGate struct{}

func (allowAllGate) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return nil
}

// anon is the anonymous principal; user is a signed-in one.
var (
	anon = auth.Anonymous()
	user = auth.Principal{Name: "jane"}
	adm  = auth.Principal{Name: "root", Admin: true}
)

// repoScope / ownerScope build the two scope shapes.
func repoScope(owner, repo string) Scope { return Scope{Kind: ScopeRepo, Owner: owner, Repo: repo} }
func ownerScope(owner string) Scope      { return Scope{Kind: ScopeOwner, Owner: owner} }
