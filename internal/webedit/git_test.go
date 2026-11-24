package webedit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain pins the identity every spawned git process needs (runners and
// containers have no global git config) — the internal/git precedent.
func TestMain(m *testing.M) {
	os.Setenv("GIT_AUTHOR_NAME", "webedit-test")
	os.Setenv("GIT_AUTHOR_EMAIL", "webedit-test@spindle.local")
	os.Setenv("GIT_COMMITTER_NAME", "webedit-test")
	os.Setenv("GIT_COMMITTER_EMAIL", "webedit-test@spindle.local")
	os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	code := m.Run()
	os.Exit(code)
}

// gitIn runs git in dir and returns trimmed stdout (test helper).
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBareRepo builds a bare repo seeded with one commit on refs/heads/main.
func newBareRepo(t *testing.T) (bare, base string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "init", "-q", "-b", "main")
	gitIn(t, work, "config", "user.name", "seed")
	gitIn(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "-q", "-m", "seed")
	base = gitIn(t, work, "rev-parse", "HEAD")
	bare = filepath.Join(root, "bare.git")
	gitIn(t, root, "clone", "-q", "--bare", work, bare)
	return bare, base
}

func TestSubprocessGitCommitLifecycle(t *testing.T) {
	bare, base := newBareRepo(t)
	g := NewSubprocessGit("")
	ctx := context.Background()
	when := time.Unix(1700000000, 0).UTC()

	// Create a nested file.
	blob, err := g.HashBlob(ctx, bare, []byte("nested content\n"))
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	commit, tree, err := g.BuildCommit(ctx, bare, CommitBuild{
		Base: base, Path: "a/b/c.txt", BlobSHA: blob, Message: "add nested",
		AuthorName: "alice", AuthorMail: "alice@example.com", When: when,
	})
	if err != nil {
		t.Fatalf("build commit: %v", err)
	}
	if tree == "" || commit == "" {
		t.Fatalf("empty tree/commit")
	}
	if got := gitIn(t, bare, "cat-file", "-p", commit+":a/b/c.txt"); got != "nested content" {
		t.Fatalf("created content = %q", got)
	}
	if got := gitIn(t, bare, "rev-parse", commit+"^"); got != base {
		t.Fatalf("parent = %q, want %q", got, base)
	}
	if got := gitIn(t, bare, "log", "-1", "--format=%an <%ae>", commit); got != "alice <alice@example.com>" {
		t.Fatalf("author = %q", got)
	}
	if got := gitIn(t, bare, "log", "-1", "--format=%cn <%ce>", commit); got != "spindle <spindle@localhost>" {
		t.Fatalf("committer = %q", got)
	}

	// Ancestor tree rebuilt: a is a tree containing b, b contains c.txt.
	if got := gitIn(t, bare, "cat-file", "-t", commit+":a"); got != "tree" {
		t.Fatalf("a type = %q", got)
	}
	if got := gitIn(t, bare, "cat-file", "-t", commit+":a/b"); got != "tree" {
		t.Fatalf("a/b type = %q", got)
	}

	// Edit the nested file.
	blob2, _ := g.HashBlob(ctx, bare, []byte("edited\n"))
	commit2, _, err := g.BuildCommit(ctx, bare, CommitBuild{
		Base: commit, Path: "a/b/c.txt", BlobSHA: blob2, Message: "edit nested",
		AuthorName: "alice", AuthorMail: "alice@example.com", When: when,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := gitIn(t, bare, "cat-file", "-p", commit2+":a/b/c.txt"); got != "edited" {
		t.Fatalf("edited content = %q", got)
	}

	// No-op guard: rebuilding the same tree is ErrNoChange.
	if _, _, err := g.BuildCommit(ctx, bare, CommitBuild{
		Base: commit2, Path: "a/b/c.txt", BlobSHA: blob2, Message: "same",
		AuthorName: "alice", AuthorMail: "alice@example.com", When: when,
	}); err == nil || !isErr(err, ErrNoChange) {
		t.Fatalf("no-op = %v, want ErrNoChange", err)
	}

	// Delete the last file in the directory: the directory entry vanishes.
	commit3, _, err := g.BuildCommit(ctx, bare, CommitBuild{
		Base: commit2, Path: "a/b/c.txt", Delete: true, Message: "delete nested",
		AuthorName: "alice", AuthorMail: "alice@example.com", When: when,
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, lerr := g.LookupFile(ctx, bare, commit3, "a"); !isErr(lerr, ErrNotFound) && !isErr(lerr, ErrInvalid) {
		t.Fatalf("directory survived delete: %v", lerr)
	}
	if _, lerr := g.LookupFile(ctx, bare, commit3, "a/b"); !isErr(lerr, ErrNotFound) && !isErr(lerr, ErrInvalid) {
		t.Fatalf("nested directory survived delete: %v", lerr)
	}
	if got := gitIn(t, bare, "ls-tree", "-r", "--name-only", commit3); got != "hello.txt" {
		t.Fatalf("tree after delete = %q, want hello.txt", got)
	}
}

func TestSubprocessGitPackAndDiff(t *testing.T) {
	bare, base := newBareRepo(t)
	g := NewSubprocessGit("")
	ctx := context.Background()
	blob, _ := g.HashBlob(ctx, bare, []byte("one\ntwo\n"))
	commit, tree, err := g.BuildCommit(ctx, bare, CommitBuild{
		Base: base, Path: "d.txt", BlobSHA: blob, Message: "add d",
		AuthorName: "bob", AuthorMail: "bob@example.com", When: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := g.DiffTree(ctx, bare, base, tree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(patch, "diff --git a/d.txt b/d.txt") || !strings.Contains(patch, "+two") {
		t.Fatalf("patch = %q", patch)
	}
	pack, err := g.PackTip(ctx, bare, commit)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		t.Fatalf("pack = %d bytes, header %q", len(pack), pack[:min(4, len(pack))])
	}
	// A tip reachable from no ref packs only the new objects (blob + tree +
	// commit); a second pack of the same tip excludes everything now that a
	// ref is planted at the commit.
	gitIn(t, bare, "update-ref", "refs/heads/other", commit)
	pack2, err := g.PackTip(ctx, bare, commit)
	if err != nil {
		t.Fatalf("pack2: %v", err)
	}
	if n := packObjectCount(t, pack2); n != 0 {
		t.Fatalf("contained tip packed %d objects, want 0", n)
	}
}

// packObjectCount parses the pack header's object count.
func packObjectCount(t *testing.T, pack []byte) uint32 {
	t.Helper()
	if len(pack) < 12 {
		if len(pack) == 0 {
			return 0
		}
		t.Fatalf("short pack: %d bytes", len(pack))
	}
	return uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11])
}

func TestSubprocessGitLookupGuards(t *testing.T) {
	bare, base := newBareRepo(t)
	g := NewSubprocessGit("")
	ctx := context.Background()

	f, err := g.LookupFile(ctx, bare, base, "hello.txt")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if f.SHA == "" || f.Mode != "100644" || string(f.Content) != "hello\n" || f.Binary || f.LFS {
		t.Fatalf("file = %+v", f)
	}
	if _, err := g.LookupFile(ctx, bare, base, "absent.txt"); !isErr(err, ErrNotFound) {
		t.Fatalf("absent = %v", err)
	}
	if err := ValidatePath("../etc/passwd"); err == nil {
		t.Fatal("parent segment accepted")
	}
	if err := ValidatePath(".git/config"); err == nil {
		t.Fatal(".git segment accepted")
	}
	if err := ValidatePath("-rf"); err == nil {
		t.Fatal("leading dash accepted")
	}
}
