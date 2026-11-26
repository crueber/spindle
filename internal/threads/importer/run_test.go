package importer

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

// sampleWorkflow is a source the importer converts without refusals.
const sampleWorkflow = `name: CI
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: docker
    steps:
      - run: go test ./...
`

// partialWorkflow converts with one refusal (run-name).
const partialWorkflow = `name: Release
on: push
run-name: release ${{ github.ref }}
jobs:
  build:
    runs-on: docker
    steps:
      - run: echo hi
`

// gitRepoAt turns dir into a git work tree with one commit.
func gitRepoAt(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Importer Test", "GIT_AUTHOR_EMAIL=import@example.test",
			"GIT_COMMITTER_NAME=Importer Test", "GIT_COMMITTER_EMAIL=import@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-q", "-m", "initial")
}

// importRepo builds a git checkout with a Forgejo workflow in it.
func importRepo(t *testing.T, workflow string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".forgejo", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".forgejo", "workflows", "ci.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, root)
	return root
}

func TestRunDryRunWritesNothing(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Applied || rep.Branch != "" {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Files) != 1 || rep.Files[0].Output != ".spindle/workflows/ci.yml" {
		t.Errorf("files = %+v", rep.Files)
	}
	if _, ok := rep.Outputs[".spindle/workflows/ci.yml"]; !ok {
		t.Error("the converted bytes are missing from the report")
	}
	if _, err := os.Stat(filepath.Join(root, ".spindle")); !os.IsNotExist(err) {
		t.Error("a dry run wrote into the repository")
	}
	// Nothing appears in git status either.
	if out, err := gitOut(context.Background(), root, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("the work tree changed: %q %v", out, err)
	}
}

func TestRunOutWritesOutside(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	out := t.TempDir()
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied {
		t.Error("--out must not commit")
	}
	data, err := os.ReadFile(filepath.Join(out, ".spindle", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(rep.Outputs[".spindle/workflows/ci.yml"]) {
		t.Error("written file differs from the report")
	}
	if _, err := os.Stat(filepath.Join(root, ".spindle")); !os.IsNotExist(err) {
		t.Error("--out wrote into the repository")
	}
}

func TestRunApplyCreatesBranch(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || !strings.HasPrefix(rep.Branch, BranchPrefix+"forgejo-") {
		t.Errorf("report = %+v", rep)
	}
	// HEAD moved to the new branch; the original branch did not move.
	branch, err := gitOut(context.Background(), root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(branch) != rep.Branch {
		t.Errorf("HEAD = %q, want %q", branch, rep.Branch)
	}
	commits, err := gitOut(context.Background(), root, "rev-list", "--count", "main")
	if err != nil || strings.TrimSpace(commits) != "1" {
		t.Errorf("main moved: %q %v", commits, err)
	}
	// The file exists in the commit, not on main.
	if _, err := gitOut(context.Background(), root, "cat-file", "-e", "main:.spindle/workflows/ci.yml"); err == nil {
		t.Error("the converted file landed on the current branch")
	}
	if _, err := gitOut(context.Background(), root, "cat-file", "-e", rep.Branch+":.spindle/workflows/ci.yml"); err != nil {
		t.Errorf("the converted file is not in the new commit: %v", err)
	}
	msg, err := gitOut(context.Background(), root, "log", "-1", "--format=%s")
	if err != nil || !strings.Contains(msg, "import forgejo workflows") {
		t.Errorf("commit subject = %q %v", msg, err)
	}
	if out, err := gitOut(context.Background(), root, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("the work tree is dirty after a successful apply: %q %v", out, err)
	}
}

func TestRunApplyCustomBranch(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "ci/spindle-import"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Branch != "ci/spindle-import" {
		t.Errorf("branch = %q", rep.Branch)
	}
}

func TestRunApplyRefusesUntranslated(t *testing.T) {
	root := importRepo(t, partialWorkflow)
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true})
	if err == nil {
		t.Fatal("--apply on a lossy conversion must fail")
	}
	if rep == nil || !rep.Partial {
		t.Errorf("the report must still name the loss: %+v", rep)
	}
	if rep.Applied {
		t.Error("nothing may be committed")
	}
	branches, _ := gitOut(context.Background(), root, "branch", "--list")
	if strings.Contains(branches, BranchPrefix) {
		t.Errorf("a branch was created: %q", branches)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".spindle")); !os.IsNotExist(statErr) {
		t.Error("a refused apply wrote files")
	}
}

func TestRunApplyAllowPartial(t *testing.T) {
	root := importRepo(t, partialWorkflow)
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, AllowPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || !rep.Partial {
		t.Errorf("report = %+v", rep)
	}
	msg, _ := gitOut(context.Background(), root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "1 untranslated") {
		t.Errorf("the commit message does not name the loss:\n%s", msg)
	}
}

func TestRunApplyRefusesDirtyTree(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if err := os.WriteFile(filepath.Join(root, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true}); err == nil ||
		!strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("err = %v, want a dirty-tree refusal", err)
	}
}

func TestRunApplyRefusesExistingBranch(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if _, err := gitOut(context.Background(), root, "branch", "taken"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "taken"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an existing-branch refusal", err)
	}
}

func TestRunApplyRefusesCurrentBranch(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "main"}); err == nil ||
		!strings.Contains(err.Error(), "current branch") {
		t.Fatalf("err = %v, want a current-branch refusal", err)
	}
}

func TestRunApplyInvalidBranch(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "bad..name"}); err == nil {
		t.Fatal("an invalid branch name must be refused")
	}
}

func TestRunApplyNotAGitRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".forgejo", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".forgejo", "workflows", "ci.yml"), []byte(sampleWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true}); err == nil ||
		!strings.Contains(err.Error(), "not a git work tree") {
		t.Fatalf("err = %v, want a not-a-repo refusal", err)
	}
}

func TestRunApplyFromSubdirectory(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: sub, Platform: Forgejo, Apply: true, Path: filepath.Join(root, ".github/workflows/ci.yml")}); err == nil ||
		!strings.Contains(err.Error(), "repository root") {
		t.Fatalf("err = %v, want a repository-root refusal", err)
	}
}

func TestRunApplyDetachedHead(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if _, err := gitOut(context.Background(), root, "checkout", "-q", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "from-detached"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied {
		t.Error("a detached HEAD should still be able to branch")
	}
}

func TestRunMutuallyExclusive(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Out: t.TempDir()}); err == nil {
		t.Fatal("--apply and --out together must fail")
	}
	if _, err := Run(context.Background(), Options{Repo: "", Platform: Forgejo}); err == nil {
		t.Fatal("an empty repo must fail")
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: "gitea"}); err == nil {
		t.Fatal("an unknown platform must fail")
	}
}

func TestRunNoWorkflows(t *testing.T) {
	root := t.TempDir()
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo}); !errors.Is(err, ErrNoWorkflows) {
		t.Fatalf("err = %v, want ErrNoWorkflows", err)
	}
}

func TestRunDuplicateOutputNames(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "ci.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sampleWorkflow), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo}); err == nil ||
		!strings.Contains(err.Error(), "both convert to") {
		t.Fatalf("err = %v, want a duplicate-output refusal", err)
	}
}

func TestRunOversizeSource(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	big := append([]byte("on: push\n"), make([]byte, MaxSourceBytes+1)...)
	for i := range big[9:] {
		big[9+i] = ' '
	}
	if err := os.WriteFile(filepath.Join(dir, "big.yml"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo}); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want a size refusal", err)
	}
}

func TestRunGitBinaryMissing(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	old := gitBin
	gitBin = "spindle-test-definitely-not-git"
	t.Cleanup(func() { gitBin = old })
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true}); err == nil {
		t.Fatal("a missing git binary must fail")
	}
}

func TestWriteOutputRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".spindle", "workflows")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "real.yml")
	if err := os.WriteFile(inside, []byte("on: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(target, "ci.yml")
	if err := os.Symlink(inside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeOutput(root, ".spindle/workflows/ci.yml", []byte("on: push\n")); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

func TestWriteOutputRefusesEscape(t *testing.T) {
	root := t.TempDir()
	if err := writeOutput(root, "../escape.yml", []byte("x")); err == nil {
		t.Fatal("a target outside the base must be refused")
	}
}

func TestDefaultBranch(t *testing.T) {
	old := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 11, 26, 10, 15, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = old })
	got := DefaultBranch(Forgejo)
	if got != "spindle/import-forgejo-20261126T101500Z" {
		t.Errorf("DefaultBranch = %q", got)
	}
	if DefaultBranch(GitHub) != "spindle/import-github-20261126T101500Z" {
		t.Errorf("DefaultBranch(github) = %q", DefaultBranch(GitHub))
	}
}

func TestReportHelpers(t *testing.T) {
	var nilReport *Report
	if nilReport.OK() {
		t.Error("a nil report is not OK")
	}
	rep := &Report{Files: []FileReport{
		{Parses: true, Translated: 2},
		{Parses: true, Translated: 3, Untranslated: []Item{{Path: "x"}}},
	}}
	if rep.OK() {
		t.Error("a partial report is not OK")
	}
	if rep.Untranslated() != 1 || rep.Translated() != 5 {
		t.Errorf("counts = %d/%d", rep.Untranslated(), rep.Translated())
	}
}

func TestGitOutReportsStderr(t *testing.T) {
	dir := t.TempDir()
	if _, err := gitOut(context.Background(), dir, "rev-parse", "--git-dir"); err == nil {
		t.Fatal("a non-repo should fail")
	}
}
