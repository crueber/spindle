package importer

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// okTranslate translates and fails the test on an error or an unparseable
// output.
func okTranslate(t *testing.T, platform Platform, src string) *Result {
	t.Helper()
	res, err := Translate(platform, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("the converted file does not parse: %v\n%s", err, res.Output)
	}
	return res
}

func TestHeaderAndComments(t *testing.T) {
	src := `# a note the author wrote
name: CI
on: push
jobs:
  build:
    runs-on: linux
    steps:
      # why this step exists
      - run: make
`
	res := okTranslate(t, Forgejo, src)
	out := string(res.Output)
	if !strings.HasPrefix(out, "# Converted from a forgejo workflow") {
		t.Errorf("missing conversion banner:\n%s", out)
	}
	if !strings.Contains(out, "# a note the author wrote") {
		t.Error("the author's top comment was dropped")
	}
	if !strings.Contains(out, "# why this step exists") {
		t.Error("the author's step comment was dropped")
	}
	if strings.Contains(out, "2026") || strings.Contains(out, "import-forgejo-") {
		t.Errorf("the file content carries a timestamp (idempotence broken):\n%s", out)
	}
}

func TestNeedsAndLabels(t *testing.T) {
	src := `on: push
jobs:
  test:
    runs-on: docker
    steps:
      - run: "true"
  build:
    runs-on: [docker, arm64]
    needs: test
    steps:
      - run: "true"
`
	res := okTranslate(t, Forgejo, src)
	out := string(res.Output)
	if !strings.Contains(out, "needs: [test]") {
		t.Errorf("needs was not rewritten:\n%s", out)
	}
	if strings.Count(out, "- self-hosted") != 2 {
		t.Errorf("both jobs should get the default labels:\n%s", out)
	}
	if !strings.Contains(out, "- arm64") {
		t.Errorf("an unmapped label should be carried verbatim:\n%s", out)
	}
	var labels, needs bool
	for _, n := range res.Notes {
		if strings.HasPrefix(n.Path, "jobs.test.runs-on") || strings.HasPrefix(n.Path, "jobs.build.runs-on") {
			labels = true
		}
		if n.Path == "jobs.build.needs" {
			needs = true
		}
	}
	if !labels || !needs {
		t.Errorf("missing label/needs notes: %+v", res.Notes)
	}
}

func TestBuiltinActions(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout@v4
      - uses: actions/upload-artifact@v4
        with:
          name: dist
          path: dist
      - uses: actions/download-artifact@v4
        with:
          name: dist
      - uses: ./.spindle/actions/lint
      - uses: docker://alpine:3.20
        with:
          args: echo hi
`
	res := okTranslate(t, GitHub, src)
	out := string(res.Output)
	for _, want := range []string{
		"uses: actions/checkout\n", "uses: actions/upload-artifact\n", "uses: actions/download-artifact\n",
		"uses: ./.spindle/actions/lint", "uses: docker://alpine:3.20",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(res.Untranslated) != 0 {
		t.Errorf("built-ins must not be refused: %+v", res.Untranslated)
	}
	var refNote bool
	for _, n := range res.Notes {
		if strings.Contains(n.Reason, "@ref is dropped") {
			refNote = true
		}
	}
	if !refNote {
		t.Errorf("no note for the dropped @ref: %+v", res.Notes)
	}
}

func TestSecretsInRun(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: linux
    steps:
      - run: |
          echo "${{ secrets.REGISTRY_TOKEN }}"
          echo "${{ secrets.GITHUB_TOKEN }}"
        env:
          TOKEN: ${{ secrets.REGISTRY_TOKEN }}
`
	res := okTranslate(t, Forgejo, src)
	out := string(res.Output)
	if strings.Contains(out, `echo "${{ secrets.`) {
		t.Errorf("a secret expression survived inside the script:\n%s", out)
	}
	if !strings.Contains(out, `echo "${REGISTRY_TOKEN}"`) || !strings.Contains(out, `echo "${SPINDLE_JOB_TOKEN}"`) {
		t.Errorf("the script did not get the env-variable form:\n%s", out)
	}
	if !strings.Contains(out, "TOKEN: ${{ secrets.REGISTRY_TOKEN }}") {
		t.Errorf("a secret in env must stay an expression:\n%s", out)
	}
}

func TestInputsAndAliases(t *testing.T) {
	src := `on:
  workflow_dispatch:
    inputs:
      mode:
        type: string
env:
  CHECKOUT: |
    git remote add origin "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}.git"
    curl "$GITHUB_SERVER_URL/api/v1/x"
    api="$GITHUB_API_URL/x"
jobs:
  build:
    runs-on: linux
    if: ${{ github.actor != '' }}
    steps:
      - run: echo "${{ github.event.inputs.mode }}"
        with:
          mode: ${{ github.event.inputs.mode }}
`
	res := okTranslate(t, Forgejo, src)
	out := string(res.Output)
	if strings.Contains(out, "github.event.inputs") {
		t.Errorf("event.inputs survived:\n%s", out)
	}
	if !strings.Contains(out, "${SPINDLE_SERVER}/${GITHUB_REPOSITORY}") || !strings.Contains(out, "$SPINDLE_SERVER/api/v1/x") {
		t.Errorf("GITHUB_SERVER_URL was not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "$GITHUB_API_URL") {
		t.Errorf("GITHUB_API_URL is provided by the runner and must be carried verbatim:\n%s", out)
	}
	var actor bool
	for _, n := range res.Notes {
		if n.Construct == "github.actor" {
			actor = true
		}
		if n.Construct == "GITHUB_API_URL" {
			t.Errorf("GITHUB_API_URL is provided by the runner now; it must not be reported: %+v", res.Notes)
		}
	}
	if !actor {
		t.Errorf("missing actor note: %+v", res.Notes)
	}
}

func TestAnchorsResolve(t *testing.T) {
	src := `on:
  push: &common
    branches: [main]
  pull_request: *common
jobs:
  build:
    runs-on: linux
    steps:
      - run: "true"
`
	res := okTranslate(t, GitHub, src)
	out := string(res.Output)
	if len(res.Untranslated) != 0 {
		t.Fatalf("an alias was mis-translated: %+v\n%s", res.Untranslated, out)
	}
	if strings.Contains(out, "*common") {
		t.Errorf("output still carries an alias that would dangle:\n%s", out)
	}
	if strings.Count(out, "branches: [main]") != 2 {
		t.Errorf("the alias was not resolved in both places:\n%s", out)
	}
}

func TestAnchorsInsideRefusedSubtree(t *testing.T) {
	src := `on:
  push:
    branches: [main]
x-defaults: &d
  shell: bash
jobs:
  build:
    runs-on: linux
    defaults: *d
    steps:
      - run: "true"
`
	res := okTranslate(t, GitHub, src)
	if len(res.Untranslated) == 0 {
		t.Fatalf("the unsupported key was not refused: %+v", res.Untranslated)
	}
}

func TestPlatformProfiles(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
`
	gh := okTranslate(t, GitHub, src)
	if !strings.Contains(string(gh.Output), "runs-on: ubuntu-latest") {
		t.Errorf("GitHub hosted labels must be carried verbatim:\n%s", gh.Output)
	}
	var hosted bool
	for _, n := range gh.Notes {
		if strings.Contains(n.Reason, "hosted Linux label") {
			hosted = true
		}
	}
	if !hosted {
		t.Errorf("no hosted-label note: %+v", gh.Notes)
	}
	fj := okTranslate(t, Forgejo, src)
	if !strings.Contains(string(fj.Output), "runs-on: ubuntu-latest") {
		t.Errorf("Forgejo labels are exact and must be carried verbatim:\n%s", fj.Output)
	}
	if profiles[GitHub].dir == profiles[Forgejo].dir {
		t.Error("the two platforms must discover different directories")
	}
}

func TestWindowsHostedLabelNote(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: windows-latest
    steps:
      - run: "true"
`
	res := okTranslate(t, GitHub, src)
	var noted bool
	for _, n := range res.Notes {
		if strings.Contains(n.Reason, "non-goal") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("a Windows hosted runner must be named as a non-goal: %+v", res.Notes)
	}
}

func TestDispatchInputsCarried(t *testing.T) {
	src := `on:
  workflow_dispatch:
    inputs:
      bump:
        description: "how much"
        type: choice
        options: [patch, minor]
        default: patch
      dry:
        type: boolean
        required: false
      name:
        default: 'x'
jobs:
  build:
    runs-on: linux
    steps:
      - run: "true"
`
	res := okTranslate(t, Forgejo, src)
	out := string(res.Output)
	for _, want := range []string{"type: choice", "options: [patch, minor]", "type: boolean", "default: patch"} {
		if !strings.Contains(out, want) {
			t.Errorf("dispatch input lost %q:\n%s", want, out)
		}
	}
	if len(res.Untranslated) != 0 {
		t.Errorf("a supported input was refused: %+v", res.Untranslated)
	}
}

func TestScheduleCarried(t *testing.T) {
	src := `on:
  schedule:
    - cron: '0 3 * * *'
    - cron: '30 5 * * 1-5'
jobs:
  build:
    runs-on: linux
    steps:
      - run: "true"
`
	res := okTranslate(t, Forgejo, src)
	if !strings.Contains(string(res.Output), "cron: '0 3 * * *'") {
		t.Errorf("schedule lost:\n%s", res.Output)
	}
	if len(res.Untranslated) != 0 {
		t.Errorf("valid crons were refused: %+v", res.Untranslated)
	}
}

func TestEnvironmentCarried(t *testing.T) {
	src := `on: push
jobs:
  deploy:
    runs-on: linux
    environment:
      name: production
      url: https://example.com
    steps:
      - run: "true"
`
	res := okTranslate(t, GitHub, src)
	out := string(res.Output)
	if !strings.Contains(out, "name: production") || !strings.Contains(out, "https://example.com") {
		t.Errorf("environment lost:\n%s", out)
	}
}

func TestTagsOnlyNote(t *testing.T) {
	src := `on:
  push:
    tags: ["v*"]
jobs:
  release:
    runs-on: linux
    steps:
      - run: "true"
`
	res := okTranslate(t, Forgejo, src)
	var reason string
	for _, n := range res.Notes {
		if strings.Contains(n.Construct, "tags-only") {
			reason = n.Reason
		}
	}
	if reason == "" {
		t.Fatalf("a tags-only trigger needs a note: %+v", res.Notes)
	}
	// The note states the trigger's semantics: the filter is carried as
	// written, a matching tag push starts a run, and a branch push never does.
	if !strings.Contains(reason, "converted as written") || !strings.Contains(reason, "starts a run") ||
		!strings.Contains(reason, "never matches a branch push") {
		t.Errorf("the tags-only note does not state the current semantics: %q", reason)
	}
	if strings.Contains(reason, "starts no run") {
		t.Errorf("the tags-only note still carries the stale caveat: %q", reason)
	}
	// Pin the claim against the parser: the converted file fires for a matching
	// tag push and not for a branch push.
	w, err := workflow.Parse(res.Output)
	if err != nil {
		t.Fatalf("the converted file does not parse: %v", err)
	}
	if !w.Fires("push", workflow.RefTag, "v1.2.3", "") {
		t.Error("a matching tag push should start a run")
	}
	if w.Fires("push", workflow.RefBranch, "main", "") {
		t.Error("a tags-only trigger must not match a branch push")
	}
}

func TestParsePlatform(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Platform
		ok   bool
	}{
		{"forgejo", Forgejo, true},
		{"Forgejo", Forgejo, true},
		{" github ", GitHub, true},
		{"gitea", "", false},
		{"", "", false},
	} {
		got, err := ParsePlatform(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("ParsePlatform(%q) = %q, %v", tc.in, got, err)
			}
		} else if err == nil {
			t.Errorf("ParsePlatform(%q) should fail", tc.in)
		}
	}
}

func TestUsesBase(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantBase string
		wantRef  bool
	}{
		{"actions/checkout@v4", "actions/checkout", true},
		{"actions/checkout", "actions/checkout", false},
		{"actions/setup-go@v5", "actions/setup-go", true},
		{"docker://ghcr.io/o/i:1@sha256:abc", "docker://ghcr.io/o/i:1@sha256:abc", false},
		{"./path", "./path", false},
	} {
		base, ref := usesBase(tc.in)
		if base != tc.wantBase || ref != tc.wantRef {
			t.Errorf("usesBase(%q) = %q,%v want %q,%v", tc.in, base, ref, tc.wantBase, tc.wantRef)
		}
	}
}

func TestFailureScript(t *testing.T) {
	got := failureScript("it's broken\nnow")
	if strings.Contains(got, "\nnow") {
		t.Errorf("a newline survived into the script: %q", got)
	}
	if !strings.Contains(got, `it'\''s broken`) {
		t.Errorf("single quote not escaped: %q", got)
	}
	if !strings.Contains(got, "exit 1") {
		t.Errorf("the script does not fail: %q", got)
	}
}

func TestOneLine(t *testing.T) {
	long := strings.Repeat("a", 400)
	if got := oneLine(long); len(got) != 300 {
		t.Errorf("oneLine length = %d, want 300", len(got))
	}
	if got := oneLine("a\nb\r c"); got != "a b  c" {
		t.Errorf("oneLine = %q", got)
	}
}

func TestTranslateRejectsUnconvertible(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"whitespace", "  \n"},
		{"not yaml", "jobs: [\n"},
		{"more than one document", "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n---\non: push\n"},
		{"top level sequence", "- a\n- b\n"},
		{"too large", "# " + strings.Repeat("x", MaxSourceBytes) + "\non: push\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Translate(Forgejo, []byte(tc.src)); err == nil {
				t.Fatal("Translate succeeded, want an error")
			}
		})
	}
	if _, err := Translate("gitea", []byte("on: push\njobs: {}\n")); err == nil {
		t.Error("an unknown platform must fail")
	}
}

func TestOutputPath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{".forgejo/workflows/ci.yml", ".spindle/workflows/ci.yml", true},
		{".github/workflows/release.yaml", ".spindle/workflows/release.yml", true},
		{"ci.yaml", ".spindle/workflows/ci.yml", true},
		{".yml", "", false},
		{"..", "", false},
		{".", "", false},
	} {
		got, err := OutputPath(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("OutputPath(%q) = %q, %v want %q", tc.in, got, err, tc.want)
			}
		} else if err == nil {
			t.Errorf("OutputPath(%q) should fail, got %q", tc.in, got)
		}
	}
}

func TestNotesAreSortedAndStable(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: docker
    steps:
      - run: echo "$GITHUB_SERVER_URL/$GITHUB_SERVER_URL"
`
	a := okTranslate(t, Forgejo, src)
	b := okTranslate(t, Forgejo, src)
	var pathsA, pathsB []string
	for _, n := range a.Notes {
		pathsA = append(pathsA, n.Path)
	}
	for _, n := range b.Notes {
		pathsB = append(pathsB, n.Path)
	}
	sort.Strings(pathsA)
	sort.Strings(pathsB)
	if strings.Join(pathsA, ",") != strings.Join(pathsB, ",") {
		t.Errorf("note order is not deterministic: %v vs %v", pathsA, pathsB)
	}
	if string(a.Output) != string(b.Output) {
		t.Error("two conversions of one source differ")
	}
}

func TestDocumentNodeCommentsSurvive(t *testing.T) {
	src := "# header\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: 'true'\n"
	res := okTranslate(t, Forgejo, src)
	var root yaml.Node
	if err := yaml.Unmarshal(res.Output, &root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Output), "# header") {
		t.Error("the header comment was lost")
	}
}
