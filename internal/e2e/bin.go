// Package e2e drives the real spindle binary end to end: it builds
// ./cmd/spindle once per test process, boots it as a subprocess against a
// throwaway data dir, and exercises the git smart-HTTP path, the JSON API
// lane, the boot/setup lifecycle, and the events webhook bridge with a real
// git client (docs/go/15_testing.md §5.2/§5.3). The binary is a black box:
// this package imports nothing from internal/.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// binOnce builds the spindle binary exactly once per test process; every
// scenario shares the result. binDir is removed by TestMain.
var (
	binOnce sync.Once
	binPath string
	binDir  string
	binErr  error
)

// buildSpindle builds ./cmd/spindle once and returns the executable path.
// Fails the test if the binary cannot be produced.
func buildSpindle(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			binErr = err
			return
		}
		dir, err := os.MkdirTemp("", "spindle-e2e-bin-")
		if err != nil {
			binErr = err
			return
		}
		binDir = dir
		path := filepath.Join(dir, "spindle")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/spindle")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build ./cmd/spindle: %w\n%s", err, out)
			return
		}
		binPath = path
	})
	if binErr != nil {
		t.Fatalf("build spindle binary: %v", binErr)
	}
	return binPath
}

// repoRoot walks up from the package directory to the go.mod of the spindle
// module, so the suite works from any working directory go test chooses.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
