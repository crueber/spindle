package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// TestTranslateEmptyShapes covers the empty/null spellings, which are legal in
// both source dialects and must be carried without a spurious refusal.
func TestTranslateEmptyShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"on null", "on:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"push null", "on:\n  push:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"pull_request null", "on:\n  pull_request:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"workflow_dispatch null", "on:\n  workflow_dispatch:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"permissions null", "on: push\npermissions:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"concurrency null", "on: push\nconcurrency:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"concurrency scalar", "on: push\nconcurrency: ci-${{ github.ref }}\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"concurrency cancel only", "on: push\nconcurrency:\n  cancel-in-progress: true\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"concurrency empty", "on: push\nconcurrency: {}\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"environment null", "on: push\njobs:\n  a:\n    runs-on: x\n    environment:\n    steps:\n      - run: 'true'\n"},
		{"strategy null", "on: push\njobs:\n  a:\n    runs-on: x\n    strategy:\n    steps:\n      - run: 'true'\n"},
		{"matrix null", "on: push\njobs:\n  a:\n    runs-on: x\n    strategy:\n      matrix:\n    steps:\n      - run: 'true'\n"},
		{"services null", "on: push\njobs:\n  a:\n    runs-on: x\n    services:\n    steps:\n      - run: 'true'\n"},
		{"container null", "on: push\njobs:\n  a:\n    runs-on: x\n    container:\n    steps:\n      - run: 'true'\n"},
		{"needs null", "on: push\njobs:\n  a:\n    runs-on: x\n    needs:\n    steps:\n      - run: 'true'\n"},
		{"branches null", "on:\n  push:\n    branches:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"tags scalar", "on:\n  push:\n    tags: v*\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
		{"flow mapping with refusal", "on: {push: {branches: [main], pathz: ['x/**']}}\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Translate(Forgejo, []byte(tc.src))
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if _, err := workflow.Parse(res.Output); err != nil {
				t.Fatalf("does not parse: %v\n%s", err, res.Output)
			}
			// Only the flow case has a refusal; the rest must be clean.
			if tc.name == "flow mapping with refusal" {
				if len(res.Untranslated) != 1 {
					t.Errorf("untranslated = %+v", res.Untranslated)
				}
				if !strings.Contains(string(res.Output), "# UNTRANSLATED: on.push.pathz") {
					t.Errorf("the refusal comment is missing:\n%s", res.Output)
				}
				return
			}
			if len(res.Untranslated) != 0 {
				t.Errorf("a legal empty shape was refused: %+v\n%s", res.Untranslated, res.Output)
			}
		})
	}
}

func TestDecodeRejectsEmptyDocument(t *testing.T) {
	if _, err := Translate(Forgejo, []byte("---\n")); err == nil {
		t.Error("an empty document should fail")
	}
}

func TestReplaceStepWithFailureAppends(t *testing.T) {
	steps := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	replaceStepWithFailure(steps, -1, "x: y")
	if len(steps.Content) != 1 {
		t.Fatalf("append branch: %+v", steps)
	}
}

func TestApplyRollsBackOnAddFailure(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	// An ignored target makes `git add` fail after the branch was created.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".spindle/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, root)
	_, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Apply: true, Branch: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "git add") {
		t.Fatalf("err = %v, want a git add failure", err)
	}
	branch, _ := gitOut(context.Background(), root, "symbolic-ref", "--short", "HEAD")
	if strings.TrimSpace(branch) != "main" {
		t.Errorf("HEAD = %q, want main after a rollback", branch)
	}
}
