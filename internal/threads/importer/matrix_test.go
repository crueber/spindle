package importer

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// matrixCells translates one source and returns the expanded cells of `build`.
func matrixCells(t *testing.T, src string) (*Result, []workflow.MatrixCell) {
	t.Helper()
	res, err := Translate(Forgejo, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	w, err := workflow.Parse(res.Output)
	if err != nil {
		t.Fatalf("the converted file does not parse: %v\n%s", err, res.Output)
	}
	job, ok := w.Jobs["build"]
	if !ok || job.Strategy == nil {
		t.Fatalf("job build has no strategy:\n%s", res.Output)
	}
	return res, job.Strategy.Matrix.Expand()
}

func TestMatrixIncludeOnly(t *testing.T) {
	// The Scriobh release shape: no axes, one object per cell.
	res, cells := matrixCells(t, `on: push
jobs:
  build:
    runs-on: linux
    strategy:
      fail-fast: false
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
          - { goos: darwin, goarch: arm64 }
          - { goos: windows, goarch: amd64 }
    steps:
      - run: echo "${{ matrix.goos }}/${{ matrix.goarch }}"
`)
	if len(res.Untranslated) != 0 {
		t.Fatalf("include-only matrix must convert cleanly: %+v", res.Untranslated)
	}
	want := []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"}
	if len(cells) != len(want) {
		t.Fatalf("cells = %+v, want %d", cells, len(want))
	}
	for i, cell := range cells {
		if got := cell.Values["goos"] + "/" + cell.Values["goarch"]; got != want[i] {
			t.Errorf("cell %d = %q, want %q", i, got, want[i])
		}
	}
	// Shape and values are preserved as written.
	out := string(res.Output)
	for _, line := range []string{
		"include:",
		"{goos: linux, goarch: amd64}",
		"{goos: linux, goarch: arm64}",
		"{goos: darwin, goarch: arm64}",
		"{goos: windows, goarch: amd64}",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lost %q:\n%s", line, out)
		}
	}
}

func TestMatrixIncludeAddsKey(t *testing.T) {
	// An include entry adds a key to the combinations whose axes it matches;
	// the target implements GitHub's merge rules (docs/threads.md §3.3a).
	res, cells := matrixCells(t, `on: push
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        os: [linux, darwin]
        include:
          - { os: linux, experimental: "true" }
    steps:
      - run: echo "${{ matrix.os }} ${{ matrix.experimental }}"
`)
	if len(res.Untranslated) != 0 {
		t.Fatalf("unexpected refusal: %+v", res.Untranslated)
	}
	if len(cells) != 2 {
		t.Fatalf("cells = %+v, want 2", cells)
	}
	if got := cells[0].Values["experimental"]; got != "true" {
		t.Errorf("linux cell experimental = %q, want true", got)
	}
	if got := cells[1].Values["experimental"]; got != "" {
		t.Errorf("darwin cell must not gain the include key, got %q", got)
	}
}

func TestMatrixExcludeFilters(t *testing.T) {
	res, cells := matrixCells(t, `on: push
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        os: [linux, darwin, windows]
        exclude:
          - { os: windows }
    steps:
      - run: echo "${{ matrix.os }}"
`)
	if len(res.Untranslated) != 0 {
		t.Fatalf("unexpected refusal: %+v", res.Untranslated)
	}
	if len(cells) != 2 {
		t.Fatalf("cells = %+v, want 2 (windows excluded)", cells)
	}
	for _, cell := range cells {
		if cell.Values["os"] == "windows" {
			t.Errorf("the excluded cell is still present: %+v", cells)
		}
	}
}

func TestMatrixObjectAxisStillRefusedAndDeclared(t *testing.T) {
	// The one genuinely unconvertible matrix shape: the axis stays declared (so
	// a reference parses) and the job gets a step that fails, because the cell
	// set is unknown.
	res := okTranslate(t, Forgejo, `on: push
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        os:
          - { name: linux, arch: amd64 }
          - { name: darwin, arch: arm64 }
    steps:
      - run: echo "${{ matrix.os }}"
`)
	if len(res.Untranslated) != 1 || res.Untranslated[0].Path != "jobs.build.strategy.matrix.os" {
		t.Fatalf("untranslated = %+v", res.Untranslated)
	}
	out := string(res.Output)
	if !strings.Contains(out, "# UNTRANSLATED: jobs.build.strategy.matrix.os") {
		t.Errorf("missing the named comment:\n%s", out)
	}
	if !strings.Contains(out, "os: [untranslated-object-cell]") {
		t.Errorf("the axis key is not kept declared with a placeholder:\n%s", out)
	}
	if !strings.Contains(out, "see the UNTRANSLATED comments") {
		t.Errorf("the job has no failing step:\n%s", out)
	}
}

func TestMatrixIncludeIdempotent(t *testing.T) {
	src := `on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: docker
    strategy:
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
    steps:
      - run: echo "${{ matrix.goos }}/${{ matrix.goarch }}"
`
	first := okTranslate(t, Forgejo, src)
	second := okTranslate(t, Forgejo, src)
	if string(first.Output) != string(second.Output) {
		t.Errorf("a second conversion of the same source differs:\n--- first ---\n%s\n--- second ---\n%s", first.Output, second.Output)
	}
}

func TestMatrixAxesCoverIncludeKeys(t *testing.T) {
	// A job-level concurrency group may read an include-only key: the
	// validator gets the parser's field set (axes + include keys).
	res := okTranslate(t, Forgejo, `on: push
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: darwin, goarch: arm64 }
    concurrency:
      group: build-${{ matrix.goos }}-${{ matrix.goarch }}
    steps:
      - run: "true"
`)
	if len(res.Untranslated) != 0 {
		t.Fatalf("an include key in a concurrency group was refused: %+v", res.Untranslated)
	}
}

func TestMatrixAxisOK(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *yaml.Node
		want bool
	}{
		{"scalar", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "linux"}, true},
		{"null scalar", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, true},
		{"list of scalars", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "a"}, {Kind: yaml.ScalarNode, Value: "b"},
		}}, true},
		{"object", &yaml.Node{Kind: yaml.MappingNode}, false},
		{"list with an object", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "a"}, {Kind: yaml.MappingNode},
		}}, false},
		{"list with a nested list", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{
			{Kind: yaml.SequenceNode},
		}}, false},
	} {
		if got := matrixAxisOK(tc.node); got != tc.want {
			t.Errorf("%s: matrixAxisOK = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMatrixAxesDirect(t *testing.T) {
	tr := &translator{}
	_ = tr
	// A strategy whose matrix mixes an include (with a non-object entry), an
	// exclude, a usable axis, an empty axis and an object axis.
	job := mapNode("strategy", "")
	matrix := mapNode(
		"os", "",
		"empty", "",
		"include", "",
		"exclude", "",
		"object", "",
	)
	job.Content[1].Kind = yaml.MappingNode
	job.Content[1].Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "matrix"}, matrix,
	}
	if i := mapIndex(matrix, "os"); i >= 0 {
		setSequence(matrix.Content[i+1], []string{"linux", "darwin"})
	}
	if i := mapIndex(matrix, "empty"); i >= 0 {
		setSequence(matrix.Content[i+1], nil)
	}
	if i := mapIndex(matrix, "include"); i >= 0 {
		matrix.Content[i+1].Kind = yaml.SequenceNode
		matrix.Content[i+1].Content = []*yaml.Node{
			mapNode("goos", "linux"),
			{Kind: yaml.ScalarNode, Value: "not-an-object"},
			mapNode("goarch", "amd64"),
		}
	}
	if i := mapIndex(matrix, "exclude"); i >= 0 {
		matrix.Content[i+1].Kind = yaml.SequenceNode
		matrix.Content[i+1].Content = []*yaml.Node{mapNode("os", "darwin")}
	}
	if i := mapIndex(matrix, "object"); i >= 0 {
		matrix.Content[i+1].Kind = yaml.MappingNode
	}
	got := matrixAxes(job)
	want := "goarch,goos,os"
	if strings.Join(got, ",") != want {
		t.Errorf("matrixAxes = %v, want %s", got, want)
	}
	if matrixAxes(mapNode("runs-on", "linux")) != nil {
		t.Error("a job without strategy should have no axes")
	}
}

func TestFailJobWithoutSteps(t *testing.T) {
	tr := &translator{}
	job := mapNode("runs-on", "linux")
	tr.failJob(job, "jobs.x.strategy", "reason") // no steps: no panic, nothing added
	if mapIndex(job, "steps") >= 0 {
		t.Error("failJob invented a steps block")
	}
	job = mapNode("steps", "")
	job.Content[1].Kind = yaml.ScalarNode
	tr.failJob(job, "jobs.x.strategy", "reason") // steps is not a sequence: no panic
}
