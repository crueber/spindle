package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobMatchMatrix(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"main", "main", true},
		{"main", "maine", false},
		{"release/*", "release/1.2", true},
		{"release/*", "main", false},
		{"*", "anything", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"a*b*c", "zXXbYYc", false},
		{"a*", "a", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
	if !matchAny(nil, "x") {
		t.Error("empty filter must match")
	}
	if !matchAny([]string{"dev", "release/*"}, "release/2") {
		t.Error("second pattern did not match")
	}
	if matchAny([]string{"dev"}, "main") {
		t.Error("unexpected match")
	}
}

func TestParseInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("jobs: [unclosed\n")); err == nil {
		t.Fatal("invalid YAML accepted")
	}
	// A scalar `on:` is not supported and must be a clear error, not a panic.
	if _, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("scalar on: accepted")
	}
}

func TestParseFullShape(t *testing.T) {
	src := `
name: full
on:
  schedule:
    - cron: "0 3 * * *"
  workflow_dispatch:
env:
  CGO_ENABLED: "0"
jobs:
  build:
    name: Build
    runs-on: linux
    container: golang:1.27
    timeout-minutes: 30
    continue-on-error: true
    services:
      db:
        image: postgres:16
        env: {POSTGRES_PASSWORD: x}
        ports: ["5432:5432"]
        command: ["postgres"]
    env: {GOFLAGS: "-mod=readonly"}
    steps:
      - uses: checkout
        with: {fetch-depth: "0"}
      - run: go build ./...
        shell: bash
        working-directory: ./src
        env: {CI: "1"}
        if: success()
  test:
    runs-on: linux
    needs: [build]
    if: always()
    steps:
      - run: go test ./...
`
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(w.On.Schedule) != 1 || w.On.Schedule[0].Cron != "0 3 * * *" {
		t.Fatalf("schedule = %+v", w.On.Schedule)
	}
	if !w.On.DispatchSet {
		t.Fatal("dispatch presence not recorded")
	}
	job := w.Jobs["build"]
	if job.Container != "golang:1.27" || job.TimeoutMinutes != 30 || !job.ContinueOnError {
		t.Fatalf("job = %+v", job)
	}
	if svc := job.Services["db"]; svc.Image != "postgres:16" || len(svc.Ports) != 1 {
		t.Fatalf("service = %+v", svc)
	}
	if len(job.Steps) != 2 || job.Steps[0].With["fetch-depth"] != "0" || job.Steps[1].Shell != "bash" {
		t.Fatalf("steps = %+v", job.Steps)
	}
	if !w.Fires("schedule", "") == false {
		t.Fatal("schedule must not match Fires")
	}
}

func TestDiamondNeedsHasNoCycle(t *testing.T) {
	src := `
jobs:
  a:
    runs-on: l
    needs: [b, c]
    steps: [{run: x}]
  b:
    runs-on: l
    needs: [d]
    steps: [{run: x}]
  c:
    runs-on: l
    needs: [d]
    steps: [{run: x}]
  d:
    runs-on: l
    steps: [{run: x}]
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("diamond rejected: %v", err)
	}
}

func TestParseFileErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "w.yml")
	if err := os.WriteFile(good, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(good); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := ParseFile(filepath.Join(dir, "missing.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

func TestFilesEmptyDir(t *testing.T) {
	root := t.TempDir()
	got, err := Files(root)
	if err != nil || len(got) != 0 {
		t.Fatalf("Files = %v, %v", got, err)
	}
}

func TestFiresBranchesAndDispatch(t *testing.T) {
	w, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Fires("push", "main") || w.Fires("push", "nope") {
		t.Fatal("push filter wrong")
	}
	if !w.Fires("pull_request", "main") || w.Fires("pull_request", "nope") {
		t.Fatal("pr filter wrong")
	}
	if !w.Fires("workflow_dispatch", "") {
		t.Fatal("dispatch missing")
	}
	if w.Fires("push", "") {
		t.Fatal("empty branch must not match a filtered push")
	}
	if strings.TrimSpace(w.Name) == "" {
		t.Fatal("name lost")
	}
}
