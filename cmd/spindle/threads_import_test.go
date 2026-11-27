package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/importer"
)

const cliImportCI = `name: CI
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: docker
    steps:
      - run: go test ./...
`

const cliImportPartial = `name: Release
x-release-channel: stable
on: push
jobs:
  build:
    runs-on: docker
    steps:
      - run: echo hi
`

// importCheckout builds a directory that looks like a Forgejo checkout.
func importCheckout(t *testing.T, workflow string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".forgejo", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runImportCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{out: &out, errOut: &errOut}
	argv := append([]string{"threads", "import"}, args...)
	code = dispatch(context.Background(), argv, c)
	return code, out.String(), errOut.String()
}

func TestThreadsImportDryRun(t *testing.T) {
	root := importCheckout(t, cliImportCI)
	code, stdout, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root)
	if code != exitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "# ==> .spindle/workflows/ci.yml (from .forgejo/workflows/ci.yml)") {
		t.Errorf("dry run does not announce the target:\n%s", stdout)
	}
	if !strings.Contains(stdout, "name: CI") {
		t.Errorf("dry run does not print the converted file:\n%s", stdout)
	}
	if !strings.Contains(stderr, "1 file(s)") {
		t.Errorf("the summary should go to stderr on a dry run:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".spindle")); !os.IsNotExist(err) {
		t.Error("the dry run wrote into the repository")
	}
}

func TestThreadsImportJSON(t *testing.T) {
	root := importCheckout(t, cliImportCI)
	code, stdout, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d %s", code, stderr)
	}
	var rep importer.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a report: %v\n%s", err, stdout)
	}
	if rep.Platform != importer.Forgejo || rep.Partial || len(rep.Files) != 1 || !rep.Files[0].Parses {
		t.Errorf("report = %+v", rep)
	}
	if strings.Contains(stdout, "name: CI") {
		t.Error("--json must print the report only, not the file")
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestThreadsImportPartialExitsOne(t *testing.T) {
	root := importCheckout(t, cliImportPartial)
	code, stdout, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root, "--json")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	var rep importer.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a report: %v", err)
	}
	if !rep.Partial || len(rep.Files) != 1 || len(rep.Files[0].Untranslated) != 1 {
		t.Errorf("report = %+v", rep)
	}
	if !strings.Contains(stderr, "untranslated construct") {
		t.Errorf("stderr does not name the refusal: %q", stderr)
	}
}

func TestThreadsImportOut(t *testing.T) {
	root := importCheckout(t, cliImportCI)
	out := t.TempDir()
	code, stdout, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root, "--out", out)
	if code != exitOK {
		t.Fatalf("exit = %d %s", code, stderr)
	}
	if strings.Contains(stdout, "name: CI") {
		t.Errorf("--out must not print the file:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(out, ".spindle", "workflows", "ci.yml")); err != nil {
		t.Errorf("--out did not write: %v", err)
	}
}

func TestThreadsImportUsage(t *testing.T) {
	root := importCheckout(t, cliImportCI)
	cases := [][]string{
		{},
		{"--from", "forgejo"},
		{"--repo", root},
		{"--from", "gitea", "--repo", root},
		{"--from", "forgejo", "--repo", root, "--apply", "--out", t.TempDir()},
		{"--from", "forgejo", "--repo", root, "extra"},
	}
	for _, args := range cases {
		code, _, stderr := runImportCLI(t, args...)
		if code != exitArg {
			t.Errorf("args %v: exit = %d, want %d (%s)", args, code, exitArg, stderr)
		}
	}
}

func TestThreadsImportHelp(t *testing.T) {
	code, _, stderr := runImportCLI(t, "-h")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"usage: spindle threads import", "--from", "--repo", "--apply", "--allow-partial"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("help misses %q:\n%s", want, stderr)
		}
	}
}

func TestThreadsImportApplyFailsOutsideGit(t *testing.T) {
	root := importCheckout(t, cliImportCI)
	code, _, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root, "--apply")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(stderr, "not a git work tree") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestThreadsImportNoWorkflows(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := runImportCLI(t, "--from", "forgejo", "--repo", root)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(stderr, "no workflow files") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestThreadsGroupListsImport(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &cli{out: &out, errOut: &errOut}
	if code := groupRunner(threadsGroup)(context.Background(), c, nil); code != exitArg {
		t.Fatalf("bare threads = %d", code)
	}
	if !strings.Contains(errOut.String(), "import") {
		t.Errorf("the group table does not list import:\n%s", errOut.String())
	}
	if words := completionWords(); !containsWord(words["threads"], "import") {
		t.Errorf("completion misses import: %v", words["threads"])
	}
}
