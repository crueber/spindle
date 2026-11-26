package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docsPath is the user-facing Threads reference, relative to this package.
const docsPath = "../../../docs/threads.md"

// TestDocExamplesParse pins docs/threads.md to the parser: every fenced ```yaml block in the
// reference is a COMPLETE workflow file and must parse. Fragments and console output live in
// non-yaml fences, so an example can never drift into something the parser rejects.
func TestDocExamplesParse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(docsPath))
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	blocks := fencedBlocks(string(raw), "yaml")
	if len(blocks) < 3 {
		t.Fatalf("%s carries %d fenced yaml block(s); the reference should have at least 3", docsPath, len(blocks))
	}
	for i, block := range blocks {
		if _, perr := Parse([]byte(block)); perr != nil {
			t.Errorf("docs/threads.md yaml block %d does not parse: %v\n%s", i+1, perr, block)
		}
	}
	// Action metadata examples are fenced ```yaml-action (a workflow parser
	// would refuse them by design); they must parse with the action parser.
	actions := fencedBlocks(string(raw), "yaml-action")
	if len(actions) == 0 {
		t.Fatal("docs/threads.md carries no fenced yaml-action example for local actions")
	}
	for i, block := range actions {
		if _, perr := ParseAction([]byte(block)); perr != nil {
			t.Errorf("docs/threads.md yaml-action block %d does not parse: %v\n%s", i+1, perr, block)
		}
	}
}

// fencedBlocks returns the bodies of every fenced block with the exact info
// string, in document order.
func fencedBlocks(doc, info string) []string {
	var out []string
	var cur []string
	in := false
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if !in {
			if trimmed == "```"+info {
				in, cur = true, nil
			}
			continue
		}
		if trimmed == "```" {
			out = append(out, strings.Join(cur, "\n"))
			in = false
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// workflowsGuidePath is the authoring guide, relative to this package.
const workflowsGuidePath = "../../../docs/WORKFLOWS.md"

// TestWorkflowsGuideExamplesParse pins docs/WORKFLOWS.md to the parser. A ```yaml block is a
// complete spindle workflow and must parse; ```yaml-action is action metadata and must parse with
// ParseAction; ```yaml-forgejo is another CI system's original, kept beside its spindle
// translation and deliberately not parsed here (it is the thing being migrated away from).
func TestWorkflowsGuideExamplesParse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(workflowsGuidePath))
	if err != nil {
		t.Fatalf("read %s: %v", workflowsGuidePath, err)
	}
	workflows := fencedBlocks(string(raw), "yaml")
	if len(workflows) == 0 {
		t.Fatalf("%s carries no fenced yaml examples", workflowsGuidePath)
	}
	for i, block := range workflows {
		if _, perr := Parse([]byte(block)); perr != nil {
			t.Errorf("%s yaml block %d does not parse: %v\n%s", workflowsGuidePath, i+1, perr, block)
		}
	}
	actions := fencedBlocks(string(raw), "yaml-action")
	if len(actions) == 0 {
		t.Fatalf("%s carries no fenced yaml-action example", workflowsGuidePath)
	}
	for i, block := range actions {
		if _, perr := ParseAction([]byte(block)); perr != nil {
			t.Errorf("%s yaml-action block %d does not parse: %v\n%s", workflowsGuidePath, i+1, perr, block)
		}
	}
}

// TestDocUnsupportedSpellings pins the reference's "not supported" claims: each spelling the doc
// says is a parse error must really be one, so the section cannot rot into wishful thinking.
func TestDocUnsupportedSpellings(t *testing.T) {
	valid := func(extra string) string {
		return "name: X\non: push\n" + extra + "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"
	}
	cases := []struct {
		name string
		src  string
	}{
		{"defaults block", valid("defaults:\n  run:\n    shell: bash\n")},
		{"push paths filter", "name: X\non:\n  push:\n    paths: [src/**]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"step id starting with a digit", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - id: 1build\n        run: echo hi\n"},
		{"step id with a dot", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - id: unit.test\n        run: echo hi\n"},
		{"duplicate step id", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - id: build\n        run: echo one\n      - id: build\n        run: echo two\n"},
		{"matrix include with an array value", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    strategy:\n      matrix:\n        include:\n          - go: [\"1.26\", \"1.27\"]\n    steps:\n      - run: echo hi\n"},
		{"strategy max-parallel", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    strategy:\n      max-parallel: 2\n    steps:\n      - run: echo hi\n"},
		{"unknown trigger", "name: X\non: [tag]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"bare on schedule", "name: X\non: [schedule]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"empty schedule list", "name: X\non:\n  schedule: []\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"cron macro", "name: X\non:\n  schedule:\n    - cron: \"@daily\"\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"cron weekday name", "name: X\non:\n  schedule:\n    - cron: \"0 3 * * MON\"\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"cron with seconds", "name: X\non:\n  schedule:\n    - cron: \"0 0 3 * * *\"\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"},
		{"step with run and uses", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n        uses: actions/checkout\n"},
		{"needs unknown job", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    needs: [nope]\n    steps:\n      - run: echo hi\n"},
		{"needs cycle", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    needs: [b]\n    steps:\n      - run: echo hi\n  b:\n    runs-on: linux\n    needs: [a]\n    steps:\n      - run: echo hi\n"},
		{"no jobs", "name: X\non: push\n"},
		{"job without runs-on", "name: X\non: push\njobs:\n  a:\n    steps:\n      - run: echo hi\n"},
		{"job without steps", "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n"},
		{"unknown top-level key", valid("matrix: {}\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.src)); err == nil {
				t.Fatalf("expected a parse error for %s", tc.name)
			}
		})
	}
}

// TestDocValueTypes pins the reference's type claims: scalar `env` and `with` values are coerced to
// text (so `fetch-depth: 0` and `COUNT: 3` parse), while a nested value is a parse error.
func TestDocValueTypes(t *testing.T) {
	step := func(with string) string {
		return "name: X\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - uses: actions/checkout\n" + with
	}
	for _, with := range []string{"        with:\n          fetch-depth: 0\n", "        with:\n          fetch-depth: \"0\"\n"} {
		w, err := Parse([]byte(step(with)))
		if err != nil {
			t.Fatalf("%q rejected: %v", with, err)
		}
		if got := w.Jobs["a"].Steps[0].With["fetch-depth"]; got != "0" {
			t.Errorf("fetch-depth decoded as %q, want \"0\"", got)
		}
	}
	w, err := Parse([]byte("name: X\non: push\nenv:\n  COUNT: 3\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("scalar env value rejected: %v", err)
	}
	if got := w.Env["COUNT"]; got != "3" {
		t.Errorf("env COUNT = %q, want \"3\"", got)
	}
	if _, err := Parse([]byte("name: X\non: push\nenv:\n  NESTED:\n    a: b\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n")); err == nil {
		t.Errorf("nested env value parsed; the doc claims a scalar value")
	}
	if _, err := Parse([]byte(step("        with:\n          options:\n            depth: 1\n"))); err == nil {
		t.Errorf("nested with value parsed; the doc claims a scalar value")
	}
}
