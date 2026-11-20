// main_test.go — pins the git identity for every git subprocess the tests
// spawn (field lesson: annotated tags and commit-tree need an
// author/committer; runners and containers have no global git config).
package repoimport

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "spindle-test",
		"GIT_AUTHOR_EMAIL":    "test@spindle.test",
		"GIT_COMMITTER_NAME":  "spindle-test",
		"GIT_COMMITTER_EMAIL": "test@spindle.test",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
