package threads

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// headAtGit returns the fixture's HEAD sha.
func headAtGit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// The action reader's git methods against real git (docs/go/04_git.md §14):
// `ls-tree -r -z --long` enumerates an action directory (including the modes a
// symlink/submodule carries, so the caller refuses them by name), and
// ReadFileUpTo reads a bundled file larger than a workflow file.

func TestSubprocessGitActionTree(t *testing.T) {
	dir, _ := gitFixture(t)
	// Add an action directory with an executable script and a symlink.
	actionDir := filepath.Join(dir, "actions", "hello")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: node20\n  main: index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(actionDir, "link")); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) {
		t.Helper()
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "action")
	g := newGitReader(t, dir)

	entries, err := g.ListTree(context.Background(), "o/r", headAtGit(t, dir), "actions/hello")
	if err != nil {
		t.Fatalf("ListTree: %v", err)
	}
	modes := map[string]uint32{}
	for _, e := range entries {
		modes[filepath.Base(e.Path)] = e.Mode
	}
	if modes["action.yml"] != 0o100644 || modes["run.sh"] != 0o100755 || modes["link"] != 0o120000 {
		t.Fatalf("modes = %v", modes)
	}
	// A missing directory is an empty list, not an error.
	if entries, err := g.ListTree(context.Background(), "o/r", headAtGit(t, dir), "no/such"); err != nil || len(entries) != 0 {
		t.Fatalf("missing dir = %+v %v", entries, err)
	}
	// The listing refuses an option-looking directory.
	if _, err := g.ListTree(context.Background(), "o/r", headAtGit(t, dir), "--all"); err == nil {
		t.Fatal("an option-shaped directory must be refused")
	}

	// ReadFileUpTo reads a file up to an action-sized cap.
	big := strings.Repeat("x", 2<<20)
	if err := os.WriteFile(filepath.Join(actionDir, "big.bin"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "big")
	sha := headAtGit(t, dir)
	data, err := g.ReadFileUpTo(context.Background(), "o/r", sha, "actions/hello/big.bin", 4<<20)
	if err != nil || len(data) != len(big) {
		t.Fatalf("ReadFileUpTo = %d bytes, %v", len(data), err)
	}
	// Over the cap is a named refusal, not a truncated read.
	if _, err := g.ReadFileUpTo(context.Background(), "o/r", sha, "actions/hello/big.bin", 1024); err == nil {
		t.Fatal("over the cap must be refused")
	}
	// A missing path is not found.
	if _, err := g.ReadFileUpTo(context.Background(), "o/r", sha, "actions/hello/nope", 1<<20); err == nil {
		t.Fatal("a missing path must be refused")
	}
}
