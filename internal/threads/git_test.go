package threads

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// git_test.go pins the production GitReader against REAL git: the exact argv
// from docs/go/04_git.md §14 (ls-tree / show / rev-parse), a missing workflow
// directory, and a missing repo dir. The argv shape is the contract; a
// library substitute is never allowed (AGENTS law 2).

type staticDirs map[string]string

func (s staticDirs) Dir(_ context.Context, repo string) (string, error) {
	dir, ok := s[repo]
	if !ok {
		return "", errors.New("unknown repo")
	}
	return dir, nil
}

// gitFixture builds a bare-ish repo (a normal repo with a worktree is fine:
// git show/ls-tree only read objects) with one commit carrying workflows.
func gitFixture(t *testing.T) (dir string, head string) {
	t.Helper()
	dir = t.TempDir()
	runGit := func(argv ...string) string {
		t.Helper()
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, ".spindle", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".spindle", "workflows", "ci.yml"), []byte(workflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".spindle", "workflows", "notes.txt"), []byte("not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "workflows")
	head = runGit("rev-parse", "HEAD")
	return dir, head
}

func newGitReader(t *testing.T, dir string) *SubprocessGit {
	t.Helper()
	g := NewSubprocessGit("")
	g.Timeout = 30 * time.Second
	// The production reader runs against a BARE repo dir (the serve-synced
	// cache); a normal fixture's .git holds the same shape.
	g.SetDirs(staticDirs{"o/r": filepath.Join(dir, ".git")})
	return g
}

func TestSubprocessGitListAndRead(t *testing.T) {
	dir, head := gitFixture(t)
	g := newGitReader(t, dir)

	paths, err := g.ListWorkflowFiles(context.Background(), "o/r", head)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(paths) != 1 || paths[0] != ".spindle/workflows/ci.yml" {
		t.Fatalf("paths = %v", paths)
	}

	raw, err := g.ReadFile(context.Background(), "o/r", head, paths[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "jobs:") {
		t.Fatalf("content = %q", raw)
	}

	sha, err := g.ResolveRef(context.Background(), "o/r", "refs/heads/main")
	if err != nil || sha != head {
		t.Fatalf("resolve = %q, %v (want %q)", sha, err, head)
	}
	if _, err := g.ResolveRef(context.Background(), "o/r", "refs/heads/gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
	if _, err := g.ResolveRef(context.Background(), "o/r", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ref err = %v", err)
	}
	if _, err := g.ResolveRef(context.Background(), "o/r", "-x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("option-like ref err = %v", err)
	}
}

func TestSubprocessGitDiscover(t *testing.T) {
	dir, head := gitFixture(t)
	g := newGitReader(t, dir)
	files, err := Discover(context.Background(), g, "o/r", head)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(files) != 1 || files[0].Name != "CI" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSubprocessGitFailures(t *testing.T) {
	dir, head := gitFixture(t)
	g := newGitReader(t, dir)
	ctx := context.Background()

	// Unknown commit: git exits non-zero → the not-found family, never a
	// panic.
	if _, err := g.ListWorkflowFiles(ctx, "o/r", "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("unknown commit must error")
	}
	// Invalid sha fails before spawning.
	if _, err := g.ListWorkflowFiles(ctx, "o/r", "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sha err = %v", err)
	}
	// Missing path.
	if _, err := g.ReadFile(ctx, "o/r", head, ".spindle/workflows/ghost.yml"); err == nil {
		t.Fatal("missing file must error")
	}
	// Paths that could confuse <commit>:<path> are rejected.
	if _, err := g.ReadFile(ctx, "o/r", head, "a:b"); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("colon path err = %v", err)
	}
	if _, err := g.ReadFile(ctx, "o/r", head, ""); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("empty path err = %v", err)
	}
	// No dirs wired: fail closed.
	bare := NewSubprocessGit("")
	if _, err := bare.ListWorkflowFiles(ctx, "o/r", head); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no dirs err = %v", err)
	}
	// Unknown repo: unavailable.
	if _, err := g.ListWorkflowFiles(ctx, "other/repo", head); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown repo err = %v", err)
	}
	// Cancelled context: unavailable, not a hang.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g.ListWorkflowFiles(cctx, "o/r", head); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled err = %v", err)
	}
}

func TestSubprocessGitMissingWorkflowDirIsEmpty(t *testing.T) {
	dir := t.TempDir()
	runGit := func(argv ...string) string {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "readme")
	head := runGit("rev-parse", "HEAD")
	g := newGitReader(t, dir)
	files, err := Discover(context.Background(), g, "o/r", head)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("no-workflow repo = %d files", len(files))
	}
}

func TestCapBufferTruncates(t *testing.T) {
	b := &capBuffer{limit: 4}
	if _, err := b.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if !b.truncated || b.String() != "abcd" {
		t.Fatalf("cap buffer = %q truncated=%v", b.String(), b.truncated)
	}
	var s boundedStderr
	_, _ = s.Write([]byte(strings.Repeat("x", 9000)))
	if !strings.Contains(s.String(), "…") {
		t.Fatal("bounded stderr must mark a tail")
	}
}
