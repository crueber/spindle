package importer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoTree builds a directory tree from rel path → content ("" = directory).
func repoTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if content == "" && strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDiscoverDefaultDirs(t *testing.T) {
	root := repoTree(t, map[string]string{
		".forgejo/workflows/ci.yml":       "on: push\n",
		".forgejo/workflows/release.yaml": "on: push\n",
		".forgejo/workflows/notes.md":     "not a workflow\n",
		".forgejo/workflows/nested/x.yml": "on: push\n",
		".github/workflows/ci.yml":        "on: push\n",
		".github/workflows/other.yml":     "on: push\n",
		".forgejo/workflows/.hidden.yml":  "on: push\n",
	})
	srcs, err := Discover(root, Forgejo, "")
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, s := range srcs {
		rels = append(rels, s.Rel)
	}
	want := ".forgejo/workflows/.hidden.yml,.forgejo/workflows/ci.yml,.forgejo/workflows/release.yaml"
	if strings.Join(rels, ",") != want {
		t.Errorf("forgejo sources = %v, want %s", rels, want)
	}
	gh, err := Discover(root, GitHub, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh) != 2 || gh[0].Rel != ".github/workflows/ci.yml" || gh[1].Rel != ".github/workflows/other.yml" {
		t.Errorf("github sources = %+v", gh)
	}
}

func TestDiscoverExplicitPath(t *testing.T) {
	root := repoTree(t, map[string]string{
		".forgejo/workflows/ci.yml": "on: push\n",
		"ci/release.yml":            "on: push\n",
	})
	// Relative file.
	srcs, err := Discover(root, Forgejo, "ci/release.yml")
	if err != nil || len(srcs) != 1 || srcs[0].Rel != "ci/release.yml" {
		t.Fatalf("relative file: %+v %v", srcs, err)
	}
	// Absolute file inside the repo.
	srcs, err = Discover(root, Forgejo, filepath.Join(root, "ci", "release.yml"))
	if err != nil || len(srcs) != 1 {
		t.Fatalf("absolute file: %+v %v", srcs, err)
	}
	// Directory.
	srcs, err = Discover(root, Forgejo, "ci")
	if err != nil || len(srcs) != 1 || srcs[0].Rel != "ci/release.yml" {
		t.Fatalf("directory: %+v %v", srcs, err)
	}
}

func TestDiscoverRefusesEscapes(t *testing.T) {
	root := repoTree(t, map[string]string{
		".forgejo/workflows/ci.yml": "on: push\n",
	})
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.yml")
	if err := os.WriteFile(secret, []byte("on: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret.yml", secret, "..", "/etc/passwd"} {
		if _, err := Discover(root, Forgejo, path); err == nil {
			t.Errorf("Discover(%q) should refuse an escape", path)
		}
	}
	// A symlink whose name looks local but points outside is refused.
	link := filepath.Join(root, ".forgejo/workflows/evil.yml")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Discover(root, Forgejo, ""); err == nil {
		t.Error("a symlink escaping the repository was accepted")
	}
	// A symlink to a file inside the repository is resolved and accepted.
	inside := filepath.Join(root, ".forgejo/workflows/real.yml")
	if err := os.WriteFile(inside, []byte("on: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	srcs, err := Discover(root, Forgejo, "")
	if err != nil {
		t.Fatalf("an internal symlink was refused: %v", err)
	}
	// ci.yml, real.yml, and the internal symlink (which resolves to real.yml
	// but keeps its own name).
	if len(srcs) != 3 {
		t.Errorf("sources = %+v", srcs)
	}
}

func TestDiscoverNoWorkflows(t *testing.T) {
	root := repoTree(t, map[string]string{".forgejo/workflows/x.md": "nope"})
	if _, err := Discover(root, Forgejo, ""); !errors.Is(err, ErrNoWorkflows) {
		t.Fatalf("err = %v, want ErrNoWorkflows", err)
	}
	// A missing platform directory is the same answer.
	if _, err := Discover(root, GitHub, ""); !errors.Is(err, ErrNoWorkflows) {
		t.Fatalf("err = %v, want ErrNoWorkflows", err)
	}
}

func TestDiscoverBadRoots(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "missing"), Forgejo, ""); err == nil {
		t.Error("a missing root should fail")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(file, Forgejo, ""); err == nil {
		t.Error("a file as root should fail")
	}
	if _, err := Discover(t.TempDir(), "gitea", ""); err == nil {
		t.Error("an unknown platform should fail")
	}
}

func TestDiscoverRootIsSymlink(t *testing.T) {
	real := repoTree(t, map[string]string{".forgejo/workflows/ci.yml": "on: push\n"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	srcs, err := Discover(link, Forgejo, "")
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources = %+v err=%v", srcs, err)
	}
}

func TestSafeRel(t *testing.T) {
	root := t.TempDir()
	if _, err := safeRel(root, filepath.Join(root, "a", "b")); err != nil {
		t.Errorf("an inside path was refused: %v", err)
	}
	if _, err := safeRel(root, filepath.Join(root, "..", "x")); err == nil {
		t.Error("a path with .. should be refused")
	}
}
