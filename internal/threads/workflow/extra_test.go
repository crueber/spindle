package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Extra coverage for the presence-scan triggers (a bare `push:`/`pull_request:`
// key decodes to a nil pointer but is a real trigger) and the glob matcher.

func TestFiresBareTriggerKeys(t *testing.T) {
	w, err := Parse([]byte(`name: bare
on:
  push:
  pull_request:
jobs:
  a:
    runs-on: linux
    steps:
      - run: x
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !w.On.PushSet || !w.On.PullRequestSet {
		t.Fatalf("presence flags = %+v", w.On)
	}
	for _, event := range []string{"push", "pull_request"} {
		if !w.Fires(event, "anything") {
			t.Errorf("bare %s trigger did not fire", event)
		}
	}
	if w.Fires("workflow_dispatch", "") {
		t.Error("bare push/pr workflow must not fire on dispatch")
	}
}

func TestGlobMatchEdges(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"release/*", "release/1.2", true},
		{"release/*", "releases/1.2", false},
		{"release/*", "release/", true},
		{"*-rc*", "v1-rc2", true},
		{"*-rc*", "v1", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"a*b*c", "ayybyyc", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestParseFileAndFilesEdges(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Fatal("ParseFile must fail on a missing file")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(bad, []byte("on: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(bad); err == nil {
		t.Fatal("ParseFile must surface a parse error")
	}
	if got, err := Files(dir); err != nil || len(got) != 0 {
		t.Fatalf("Files(no workflow dir) = %v, %v", got, err)
	}
	if _, err := Parse([]byte("name: x\non: {}\njobs: {}\n")); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("empty jobs err = %v", err)
	}
}
