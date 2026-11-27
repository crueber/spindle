// migration_test.go — PAR-003: the migration banner's read model.
//
// The banner LISTS foreign workflow files for display and hands the page the
// exact importer command; it never parses them, never imports them and never
// creates a run (the engine has no foreign-discovery path — PAR-001).
package threads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListForeignWorkflows(t *testing.T) {
	e := newTestEnv(t)
	e.git.refs["HEAD"] = testSHA
	e.git.add(testSHA, ".forgejo/workflows/ci.yml", "on: [push]\n")
	e.git.add(testSHA, ".forgejo/workflows/release.yaml", "on: [push]\n")
	e.git.add(testSHA, ".forgejo/workflows/notes.txt", "not a workflow\n")
	e.git.add(testSHA, ".forgejo/workflows/nested/deep.yml", "on: [push]\n")
	e.git.add(testSHA, ".github/workflows/publish.yml", "on: [push]\n")
	e.git.add(testSHA, ".spindle/workflows/own.yml", "on: [push]\n")

	got, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "", e.public())
	if err != nil {
		t.Fatalf("ListForeignWorkflows: %v", err)
	}
	want := &ForeignWorkflows{
		Forgejo: []string{".forgejo/workflows/ci.yml", ".forgejo/workflows/release.yaml"},
		GitHub:  []string{".github/workflows/publish.yml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreign = %+v, want %+v", got, want)
	}
	if got.Empty() {
		t.Fatal("a non-empty hint reported Empty")
	}
	if cmd := ImportCommand("forgejo"); cmd != "spindle threads import --from forgejo --repo ." {
		t.Fatalf("import command = %q", cmd)
	}
	if cmd := ImportCommand("github"); cmd != "spindle threads import --from github --repo ." {
		t.Fatalf("import command = %q", cmd)
	}
}

// TestListForeignWorkflowsNoForeignDirs: a repository with neither directory is
// an empty hint, never an error.
func TestListForeignWorkflowsNoForeignDirs(t *testing.T) {
	e := newTestEnv(t)
	e.git.refs["HEAD"] = testSHA
	e.git.add(testSHA, ".spindle/workflows/own.yml", "on: [push]\n")
	got, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "", e.public())
	if err != nil {
		t.Fatalf("ListForeignWorkflows: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("hint = %+v, want empty", got)
	}
	var nilHint *ForeignWorkflows
	if !nilHint.Empty() {
		t.Fatal("a nil hint reported content")
	}
}

// TestListForeignWorkflowsEmptyRepo: an empty repository resolves no HEAD, and
// the hint is empty rather than an error the page would have to render.
func TestListForeignWorkflowsEmptyRepo(t *testing.T) {
	e := newTestEnv(t)
	got, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "", e.public())
	if err != nil || !got.Empty() {
		t.Fatalf("empty repo = %+v, %v", got, err)
	}
}

// TestListForeignWorkflowsRefusals: the read gate is the API's gate, an
// invalid commit is refused, and a listing failure is surfaced (the page
// chooses not to render the hint).
func TestListForeignWorkflowsRefusals(t *testing.T) {
	e := newTestEnv(t)
	e.git.refs["HEAD"] = testSHA
	e.git.add(testSHA, ".forgejo/workflows/ci.yml", "on: [push]\n")
	// A role service that denies the repository read (the same gate every
	// Threads read uses).
	e.roles.public = false
	if _, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "", e.public()); err == nil {
		t.Fatal("a reader outside the gate read the foreign listing")
	}
	e.roles.public = true
	if _, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "not-a-sha", e.public()); err == nil {
		t.Fatal("an invalid commit was accepted")
	}
	e.git.dirErr = errors.New("git down")
	if _, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", testSHA, e.public()); err == nil ||
		!strings.Contains(err.Error(), "git down") {
		t.Fatalf("listing failure = %v", err)
	}
}

// TestListForeignWorkflowsEdges: a service with no reader, a ref resolution
// failure, and the path filter's refusals (so the hint can never name a file
// the importer would skip).
func TestListForeignWorkflowsEdges(t *testing.T) {
	e := newTestEnv(t)
	svc := *e.svc
	svc.Git = nil
	if got, err := svc.ListForeignWorkflows(context.Background(), "o/r", testSHA, e.public()); got != nil || err != nil {
		t.Fatalf("no reader = %+v, %v", got, err)
	}
	e.git.refErr = errors.New("ref resolution down")
	if _, err := e.svc.ListForeignWorkflows(context.Background(), "o/r", "", e.public()); err == nil ||
		!strings.Contains(err.Error(), "ref resolution down") {
		t.Fatalf("ref failure = %v", err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{".forgejo/workflows/ci.yml", true},
		{".forgejo/workflows/ci.yaml", true},
		{".forgejo/workflows/notes.txt", false},
		{".forgejo/ci.yml", false},
		{".forgejo/workflows/", false},
		{".forgejo/workflows/nested/ci.yml", false},
	}
	for _, tc := range cases {
		if got := foreignWorkflowPath(tc.path, ".forgejo/workflows"); got != tc.want {
			t.Fatalf("foreignWorkflowPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestSubprocessGitListDirRefusals pins the reader's own guards: a
// flag-shaped/empty directory and a bad commit are named refusals, and a
// missing repository is a backend error (not "no foreign files").
func TestSubprocessGitListDirRefusals(t *testing.T) {
	g := NewSubprocessGit("")
	g.SetDirs(staticDirs{})
	if _, err := g.ListDir(context.Background(), "o/r", testSHA, ""); err == nil {
		t.Fatal("an empty directory name was accepted")
	}
	if _, err := g.ListDir(context.Background(), "o/r", testSHA, "a\x00b"); err == nil {
		t.Fatal("a NUL directory name was accepted")
	}
	if _, err := g.ListDir(context.Background(), "o/r", "not-a-sha", ".forgejo/workflows"); err == nil {
		t.Fatal("an invalid commit was accepted")
	}
	if _, err := g.ListDir(context.Background(), "o/r", testSHA, ".forgejo/workflows"); err == nil {
		t.Fatal("an unknown repository was reported as an empty listing")
	}
}

// plainReader is a GitReader WITHOUT the optional TreeLister extension: the
// banner must simply not render rather than failing the page.
type plainReader struct{ GitReader }

func TestListForeignWorkflowsWithoutTreeLister(t *testing.T) {
	e := newTestEnv(t)
	svc := *e.svc
	svc.Git = plainReader{e.git}
	got, err := svc.ListForeignWorkflows(context.Background(), "o/r", testSHA, e.public())
	if err != nil || got != nil {
		t.Fatalf("reader without ListDir = %+v, %v", got, err)
	}
}

// foreignNotFoundReader answers "no such directory" for the Forgejo directory
// while listing the GitHub one: a reader whose tree lacks a foreign dir is a
// partial hint, never an error (the banner shows what exists).
type foreignNotFoundReader struct{ *fakeGit }

func (f foreignNotFoundReader) ListDir(ctx context.Context, repo, commit, dir string) ([]string, error) {
	if dir == ".forgejo/workflows" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	return f.fakeGit.ListDir(ctx, repo, commit, dir)
}

func TestListForeignWorkflowsMissingOneDir(t *testing.T) {
	e := newTestEnv(t)
	svc := *e.svc
	svc.Git = foreignNotFoundReader{e.git}
	e.git.add(testSHA, ".github/workflows/release.yml", "on: [push]\n")
	got, err := svc.ListForeignWorkflows(context.Background(), "o/r", testSHA, e.public())
	if err != nil {
		t.Fatalf("ListForeignWorkflows: %v", err)
	}
	if len(got.Forgejo) != 0 || !reflect.DeepEqual(got.GitHub, []string{".github/workflows/release.yml"}) {
		t.Fatalf("partial hint = %+v", got)
	}
}

// TestSubprocessGitListDir pins the production reader against real git: direct
// children only, a missing directory is empty, and a non-directory path is a
// named refusal.
func TestSubprocessGitListDir(t *testing.T) {
	dir := t.TempDir()
	runGit := func(argv ...string) {
		t.Helper()
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+t.TempDir(),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	for path, body := range map[string]string{
		".forgejo/workflows/ci.yml":       "on: [push]\n",
		".forgejo/workflows/nested/x.yml": "on: [push]\n",
		".github/workflows/rel.yaml":      "on: [push]\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "fixtures")
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = dir
	out, err := headCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))

	g := NewSubprocessGit("")
	g.Timeout = 30 * time.Second
	g.SetDirs(staticDirs{"o/r": filepath.Join(dir, ".git")})
	got, err := g.ListDir(context.Background(), "o/r", head, ".forgejo/workflows")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	want := []string{".forgejo/workflows/ci.yml", ".forgejo/workflows/nested/x.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListDir = %v, want %v", got, want)
	}
	missing, err := g.ListDir(context.Background(), "o/r", head, ".gitlab/workflows")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing dir = %v, %v", missing, err)
	}
	if _, err := g.ListDir(context.Background(), "o/r", head, "--all"); err == nil {
		t.Fatal("a flag-shaped directory was accepted")
	}
}
