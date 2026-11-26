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
	if len(job.RunsOn) != 1 || job.RunsOn[0] != "linux" || job.Container == nil || job.Container.Image != "golang:1.27" || len(job.Steps) != 2 {
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
		event, ref string
		kind       RefKind
		action     string
		want       bool
	}{
		{"push", "main", RefBranch, "", true},
		{"push", "feature/x", RefBranch, "", false},
		{"push", "release/1.2", RefBranch, "", true},
		{"push", "v1.2.3", RefTag, "", false}, // branches-only: tags never fire
		{"pull_request", "main", RefBranch, "opened", true},
		{"pull_request", "dev", RefBranch, "opened", false},
		{"workflow_dispatch", "", RefBranch, "", true},
		{"schedule", "", RefBranch, "", false},
		{"unknown", "", RefBranch, "", false},
	}
	for _, tc := range cases {
		if got := w.Fires(tc.event, tc.kind, tc.ref, tc.action); got != tc.want {
			t.Errorf("Fires(%q,%v,%q,%q) = %v, want %v", tc.event, tc.kind, tc.ref, tc.action, got, tc.want)
		}
	}
	// A workflow with no trigger block never fires.
	empty := &Workflow{Jobs: map[string]Job{}}
	if empty.Fires("push", RefBranch, "main", "") || empty.Fires("workflow_dispatch", RefBranch, "", "") {
		t.Error("triggerless workflow fired")
	}
	// No branch filter matches every branch — and a bare push also fires on a
	// tag (GitHub's rule: `on: push` means both kinds).
	all := &Workflow{On: Triggers{Push: &PushTrigger{}}}
	if !all.Fires("push", RefBranch, "anything", "") {
		t.Error("empty filter did not match")
	}
	if !all.Fires("push", RefTag, "v1", "") {
		t.Error("a bare push must fire on a tag")
	}
}

// TestPushTagSemantics pins PAR-010/PAR-011: a tag push fires `tags` globs, and
// a tags-only trigger never matches a branch push (the old empty-`branches`
// rule made it fire on every branch).
func TestPushTagSemantics(t *testing.T) {
	cases := []struct {
		name, trigger string
		kind          RefKind
		ref           string
		want          bool
	}{
		{"tags only, branch push", "tags: [\"v*\"]", RefBranch, "main", false},
		{"tags only, matching tag", "tags: [\"v*\"]", RefTag, "v1.2.3", true},
		{"tags only, other tag", "tags: [\"v*\"]", RefTag, "nightly", false},
		{"branches only, matching branch", "branches: [main]", RefBranch, "main", true},
		{"branches only, tag push", "branches: [main]", RefTag, "main", false},
		{"both filters, branch", "branches: [main]\n    tags: [\"v*\"]", RefBranch, "main", true},
		{"both filters, tag", "branches: [main]\n    tags: [\"v*\"]", RefTag, "v9", true},
		{"both filters, wrong tag", "branches: [main]\n    tags: [\"v*\"]", RefTag, "nightly", false},
		{"mirror glob", "tags: [\"release/*\"]", RefTag, "release/1.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "on:\n  push:\n    " + tc.trigger + "\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
			w, err := Parse([]byte(src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := w.Fires("push", tc.kind, tc.ref, ""); got != tc.want {
				t.Fatalf("%v push %q = %v, want %v", tc.kind, tc.ref, got, tc.want)
			}
		})
	}
	// A bare `on: push` with no filters fires on both kinds.
	w, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Fires("push", RefBranch, "main", "") || !w.Fires("push", RefTag, "v1", "") {
		t.Fatal("bare push must fire on branches and tags")
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

// testGlob / testMatchAny are thin wrappers over the compiled filter, so the
// existing matching tables exercise the shipped dialect.
func testGlob(pattern, name string) bool {
	f, err := compileFilter([]string{pattern}, false)
	return err == nil && f.Match(name)
}

func testMatchAny(patterns []string, name string) bool {
	return matchAnyList(patterns, name)
}
