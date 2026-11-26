package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyRollsBackOnWriteFailure(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	// A directory where the converted file must go: the write fails after the
	// branch was created, and the rollback must restore the checkout.
	target := filepath.Join(root, ".spindle", "workflows", "ci.yml")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, root) // re-commit the directory so the tree is clean
	_, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "rollback"})
	if err == nil {
		t.Fatal("the apply should have failed")
	}
	branch, _ := gitOut(context.Background(), root, "symbolic-ref", "--short", "HEAD")
	if strings.TrimSpace(branch) != "main" {
		t.Errorf("HEAD = %q, want main after a rollback", branch)
	}
	if branches, _ := gitOut(context.Background(), root, "branch", "--list", "rollback"); strings.TrimSpace(branches) != "" {
		t.Errorf("the failed branch was kept: %q", branches)
	}
	if out, _ := gitOut(context.Background(), root, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("the rollback left the tree dirty: %q", out)
	}
}

func TestApplyRollsBackOnCommitFailure(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	hooks := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("err = %v, want a commit failure", err)
	}
	branch, _ := gitOut(context.Background(), root, "symbolic-ref", "--short", "HEAD")
	if strings.TrimSpace(branch) != "main" {
		t.Errorf("HEAD = %q, want main after a rollback", branch)
	}
	if branches, _ := gitOut(context.Background(), root, "branch", "--list", "rollback"); strings.TrimSpace(branches) != "" {
		t.Errorf("the failed branch was kept: %q", branches)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".spindle", "workflows", "ci.yml")); !os.IsNotExist(statErr) {
		t.Error("the failed write was not cleaned up")
	}
}

func TestWriteOutputRefusesSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".spindle")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := writeOutput(root, ".spindle/workflows/ci.yml", []byte("on: push\n"))
	if err == nil {
		t.Fatal("a symlinked parent directory must be refused")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "workflows", "ci.yml")); !os.IsNotExist(statErr) {
		t.Error("the write escaped through the symlink")
	}
}

func TestRunSourceNamedDotYml(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".yml"), []byte(sampleWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo}); err == nil ||
		!strings.Contains(err.Error(), "cannot name an output") {
		t.Fatalf("err = %v, want an output-name refusal", err)
	}
}

func TestDiscoverBrokenSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(sampleWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(dir, "broken.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Discover(root, Forgejo, ""); err == nil {
		t.Fatal("a broken symlink should be refused, not silently skipped")
	}
}

func TestDiscoverUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := Discover(root, Forgejo, ""); err == nil {
		t.Fatal("an unreadable workflow directory should fail")
	}
}
