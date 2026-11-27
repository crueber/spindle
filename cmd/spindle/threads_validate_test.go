// threads_validate_test.go — PAR-199: `spindle threads validate`.
//
// The acceptance line is "A file validates offline with byte-identical error
// text to the server for a table of bad files". The verb runs
// threads.ValidateWorkflowSource — the server authoring pane's validator — so
// these tests compare the CLI's issue text to that function's answer for a
// table of bad files, and pin the local contract: file/directory/stdin,
// --repo, --json, and the 0/1/2 exit codes.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

const validWorkflowYAML = `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// badWorkflowTable is the PAR-199 corpus: an unknown top-level key, a key the
// hand-written unmarshaler rejects, an unsupported construct, a malformed
// matrix cell and a YAML syntax error.
var badWorkflowTable = map[string]string{
	"unknown-key.yml": `name: CI
on: [push]
nope: true
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`,
	"unsupported-construct.yml": `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    tojson: true
    steps:
      - run: echo hi
`,
	"bad-matrix.yml": `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        go: []
      max-parallel: 0
    steps:
      - run: echo hi
`,
	"syntax.yml": `name: CI
on: [push
jobs: [`,
	"bad-site.yml": `name: CI
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo ${{ job.check_run_id }}
`,
}

func validateCLI(t *testing.T) (*cli, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	c := isolateCLI(t, "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	return c, &out, &errOut
}

func TestThreadsValidateValidFile(t *testing.T) {
	c, out, errOut := validateCLI(t)
	path := filepath.Join(t.TempDir(), "ci.yml")
	if err := os.WriteFile(path, []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runThreadsValidate(t.Context(), c, []string{path}); code != exitOK {
		t.Fatalf("validate = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), path+": ok") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestThreadsValidateMatchesTheServerErrorText(t *testing.T) {
	dir := t.TempDir()
	for name, body := range badWorkflowTable {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, out, errOut := validateCLI(t)
		if code := runThreadsValidate(t.Context(), c, []string{path}); code != exitErr {
			t.Fatalf("%s: validate = %d, want 1 (%s)", name, code, errOut.String())
		}
		// The server's answer for the same bytes (the authoring pane calls
		// exactly this function).
		server := threads.ValidateWorkflowSource(path, []byte(body))
		if server.OK {
			t.Fatalf("%s: the table entry is not a bad file", name)
		}
		for _, issue := range server.Issues {
			if !strings.Contains(out.String(), issue.Message) {
				t.Fatalf("%s: CLI output misses the server's text %q:\n%s", name, issue.Message, out.String())
			}
		}
	}
}

func TestThreadsValidateJSON(t *testing.T) {
	c, out, errOut := validateCLI(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "ci.yml")
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(good, []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(badWorkflowTable["unknown-key.yml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runThreadsValidate(t.Context(), c, []string{dir, "--json"}); code != exitErr {
		t.Fatalf("validate = %d (%s)", code, errOut.String())
	}
	var report threadsValidateReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if report.OK || len(report.Files) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, file := range report.Files {
		want := threads.ValidateWorkflowSource(file.File, readFile(t, file.File))
		if file.OK != want.OK || len(file.Issues) != len(want.Issues) {
			t.Fatalf("file %s = %+v, want %+v", file.File, file, want)
		}
		for i := range file.Issues {
			if file.Issues[i].Message != want.Issues[i].Message ||
				file.Issues[i].Line != want.Issues[i].Line ||
				file.Issues[i].Key != want.Issues[i].Key {
				t.Fatalf("issue %d = %+v, want %+v", i, file.Issues[i], want.Issues[i])
			}
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestThreadsValidateDirectoryAndRepoFlag(t *testing.T) {
	root := t.TempDir()
	workflows := filepath.Join(root, ".spindle", "workflows")
	if err := os.MkdirAll(workflows, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflows, "ci.yml"), []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflows, "notes.txt"), []byte("not a workflow"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, out, errOut := validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{"--repo", root}); code != exitOK {
		t.Fatalf("--repo = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ci.yml: ok") {
		t.Fatalf("output = %q", out.String())
	}
	// A second, bad file makes the directory invalid and the summary names it.
	if err := os.WriteFile(filepath.Join(workflows, "bad.yml"), []byte(badWorkflowTable["unknown-key.yml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	c, out, _ = validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{"--repo", root}); code != exitErr {
		t.Fatalf("bad dir = %d", code)
	}
	if !strings.Contains(out.String(), "2 file(s): 1 invalid") {
		t.Fatalf("summary = %q", out.String())
	}
	// A repository root without .spindle/workflows is exit 1 naming the path.
	c, _, errOut = validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{"--repo", t.TempDir()}); code != exitErr ||
		!strings.Contains(errOut.String(), ".spindle/workflows") {
		t.Fatalf("missing repo dir = %d (%s)", code, errOut.String())
	}
}

func TestThreadsValidateEmptyDirectory(t *testing.T) {
	c, _, errOut := validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{t.TempDir()}); code != exitErr ||
		!strings.Contains(errOut.String(), "no workflow files") {
		t.Fatalf("empty dir = %d (%s)", code, errOut.String())
	}
}

func TestThreadsValidateStdin(t *testing.T) {
	c, out, errOut := validateCLI(t)
	c.in = strings.NewReader(validWorkflowYAML)
	if code := runThreadsValidate(t.Context(), c, []string{"-"}); code != exitOK {
		t.Fatalf("stdin valid = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "-: ok") {
		t.Fatalf("output = %q", out.String())
	}
	c, out, _ = validateCLI(t)
	c.in = strings.NewReader(badWorkflowTable["syntax.yml"])
	if code := runThreadsValidate(t.Context(), c, []string{"-"}); code != exitErr {
		t.Fatalf("stdin invalid = %d", code)
	}
	if !strings.Contains(out.String(), "-:") {
		t.Fatalf("stdin issue line = %q", out.String())
	}
}

func TestThreadsValidateUsage(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{},                        // nothing to validate
		{"a.yml", "b.yml"},        // two positionals
		{"a.yml", "--repo", root}, // both targets
	}
	for _, args := range cases {
		c, _, _ := validateCLI(t)
		if code := runThreadsValidate(t.Context(), c, args); code != exitArg {
			t.Fatalf("args %v = %d, want 2", args, code)
		}
	}
	c, _, _ := validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{"-h"}); code != exitOK {
		t.Fatalf("-h = %d", code)
	}
}

func TestThreadsValidateMissingFile(t *testing.T) {
	c, _, errOut := validateCLI(t)
	if code := runThreadsValidate(t.Context(), c, []string{filepath.Join(t.TempDir(), "nope.yml")}); code != exitErr {
		t.Fatalf("missing file = %d", code)
	}
	if errOut.Len() == 0 {
		t.Fatal("a missing file printed no reason")
	}
}
