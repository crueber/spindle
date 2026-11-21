package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
name: build
on:
  push:
    branches: [main, "release/*"]
  pull_request:
    branches: [main]
  workflow_dispatch:
jobs:
  test:
    runs-on: linux
    container: golang:1.27
    steps:
      - uses: checkout
      - run: go test ./...
`

func TestParseMinimal(t *testing.T) {
	w, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.Name != "build" || len(w.Jobs) != 1 {
		t.Fatalf("workflow = %+v", w)
	}
	job := w.Jobs["test"]
	if job.RunsOn != "linux" || job.Container != "golang:1.27" || len(job.Steps) != 2 {
		t.Fatalf("job = %+v", job)
	}
	if job.Steps[0].Uses != "checkout" || job.Steps[1].Run != "go test ./..." {
		t.Fatalf("steps = %+v", job.Steps)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	src := strings.Replace(minimal, "    runs-on: linux", "    runs-on: linux\n    runson: typo", 1)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestParseRequiresJobsRunsOnAndSteps(t *testing.T) {
	for name, src := range map[string]string{
		"no jobs":    "name: x\non: {}\n",
		"no runs-on": "jobs:\n  a:\n    steps:\n      - run: x\n",
		"no steps":   "jobs:\n  a:\n    runs-on: linux\n",
		"both verbs": "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n        uses: checkout\n",
		"no verb":    "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - name: x\n",
		"bad needs":  "jobs:\n  a:\n    runs-on: linux\n    needs: [ghost]\n    steps:\n      - run: x\n",
		"cycle":      "jobs:\n  a:\n    runs-on: linux\n    needs: [b]\n    steps:\n      - run: x\n  b:\n    runs-on: linux\n    needs: [a]\n    steps:\n      - run: x\n",
		"two docs":   "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n---\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: y\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse([]byte("  \n")); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
	if _, err := Parse([]byte("---\n")); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestFires(t *testing.T) {
	w, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		event, branch string
		want          bool
	}{
		{"push", "main", true},
		{"push", "feature/x", false},
		{"push", "release/1.2", true},
		{"pull_request", "main", true},
		{"pull_request", "dev", false},
		{"workflow_dispatch", "", true},
		{"schedule", "", false},
		{"unknown", "", false},
	}
	for _, tc := range cases {
		if got := w.Fires(tc.event, tc.branch); got != tc.want {
			t.Errorf("Fires(%q,%q) = %v, want %v", tc.event, tc.branch, got, tc.want)
		}
	}
	// A workflow with no trigger block never fires.
	empty := &Workflow{Jobs: map[string]Job{}}
	if empty.Fires("push", "main") || empty.Fires("workflow_dispatch", "") {
		t.Error("triggerless workflow fired")
	}
	// No branch filter matches every branch.
	all := &Workflow{On: Triggers{Push: &PushTrigger{}}}
	if !all.Fires("push", "anything") {
		t.Error("empty filter did not match")
	}
}

func TestFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b.yml", "a.yaml", "ignore.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(minimal), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || filepath.Base(got[0]) != "a.yaml" || filepath.Base(got[1]) != "b.yml" {
		t.Fatalf("Files = %v", got)
	}
	if _, err := ParseFile(got[0]); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
}
