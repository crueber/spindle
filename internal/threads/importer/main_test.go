package importer

import (
	"os"
	"testing"
)

// TestMain pins the git identity for the subprocesses the importer starts.
// Runners and containers have no global git config, so a `git commit` inside
// --apply would otherwise fail for a reason that has nothing to do with the
// importer (the same rule internal/git and internal/maintain follow).
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "Importer Test",
		"GIT_AUTHOR_EMAIL":    "import@example.test",
		"GIT_COMMITTER_NAME":  "Importer Test",
		"GIT_COMMITTER_EMAIL": "import@example.test",
	} {
		_ = os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
