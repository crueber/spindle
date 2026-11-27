// Package cipolicy pins the stewardship rule for CI: a workflow may not install
// distribution packages while it runs. The toolchain is a prepped image
// (docker/ci.Dockerfile) built once and referenced by digest; a job that runs
// apt/apk/dnf/yum is downloading from a distribution mirror on every push, which
// is both slow and discourteous to those mirrors.
//
// This test scans the CI definitions that this repository actually executes. The
// parity corpus under internal/e2e/testdata is deliberately excluded: those are
// byte-stamped copies of other projects' workflows, not ours, and they are parsed,
// never executed by this gate.
package cipolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// banned tokens are package-install commands in any of the distributions our CI
// images derive from. Matching is on the line, after comments are stripped.
var banned = []string{
	"apt-get",
	"add-apt-repository",
	"apt install",
	"apk add",
	"dnf install",
	"microdnf install",
	"yum install",
}

// workflowGlobs are the CI definitions this repository runs: the development
// gate on the forge, and the workflows installed on the GitHub mirror.
var workflowGlobs = []string{
	".forgejo/workflows/*.yml",
	"scripts/publish/*.yml",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %s has no go.mod: %v", root, err)
	}
	return root
}

func TestCIInstallsNothingAtRunTime(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, glob := range workflowGlobs {
		matches, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("no CI workflow files found; the stewardship check would be vacuous")
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			lower := strings.ToLower(line)
			for _, token := range banned {
				if strings.Contains(lower, token) {
					t.Errorf("%s:%d installs packages at run time (%q): %s\nthe CI toolchain must come from the prepped image (docker/ci.Dockerfile), not a distribution mirror",
						rel, i+1, token, trimmed)
				}
			}
		}
	}
}

// TestGitHubGateRunsThePreppedImage keeps the published gate honest: the GitHub
// workflow must run in the pinned toolchain image rather than on the runner's
// stock packages. If the image is ever removed, the run-time install ban above
// still applies, but this catches the case where someone quietly goes back to a
// bare runner with preinstalled-but-older tools.
func TestGitHubGateRunsThePreppedImage(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "publish", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)
	if !strings.Contains(body, "spindle-ci@sha256:") {
		t.Error("scripts/publish/ci.yml does not pin the prepped CI image by digest")
	}
	if n := strings.Count(body, "container:"); n < 2 {
		t.Errorf("expected the gate and a11y jobs to run in a container, found %d container blocks", n)
	}
}
