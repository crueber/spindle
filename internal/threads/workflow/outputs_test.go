package workflow

// Step and job output parsing (docs/threads.md §3.9): `outputs:` is a real key,
// a template may read only what the position can resolve, and a step may read
// only an EARLIER step's output.

import (
	"errors"
	"strings"
	"testing"
)

const outputsWorkflow = `name: Pipeline
on: [push]
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
      label: "${{ steps.build.outputs.digest }}-${{ matrix.go }}"
    strategy:
      matrix:
        go: ["1.26", "1.27"]
    steps:
      - id: build
        run: make
      - name: use it
        run: echo ${{ steps.build.outputs.digest }}
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
`

func TestParseJobOutputs(t *testing.T) {
	w, err := Parse([]byte(outputsWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := w.Jobs["build"].Outputs
	if len(got) != 2 || got["digest"] != "${{ steps.build.outputs.digest }}" {
		t.Fatalf("outputs = %#v", got)
	}
}

func TestParseJobOutputsRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			"unknown step id",
			func(s string) string {
				return strings.Replace(s, "steps.build.outputs.digest", "steps.ghost.outputs.digest", 1)
			},
			"names a step that is not declared before this one",
		},
		{
			"unknown axis",
			func(s string) string { return strings.Replace(s, "${{ matrix.go }}", "${{ matrix.node }}", 1) },
			"not a declared matrix key",
		},
		{
			"unsupported context",
			func(s string) string { return strings.Replace(s, "${{ matrix.go }}", "${{ nope.x }}", 1) },
			"not available",
		},
		{
			"bad output name",
			func(s string) string {
				return strings.Replace(s, "digest: ${{ steps.build.outputs.digest }}", "bad name: x", 1)
			},
			"invalid output name",
		},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.mutate(outputsWorkflow)))
		if err == nil || !errors.Is(err, ErrBadOutput) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want ErrBadOutput containing %q", tc.name, err, tc.want)
		}
	}
}

func TestParseStepOutputRefsRejectsForwardAndUnknown(t *testing.T) {
	cases := []struct {
		name  string
		steps string
		want  string
	}{
		{"unknown", "- id: a\n        run: echo ${{ steps.nope.outputs.x }}", "not declared before this one"},
		{"self", "- id: a\n        run: echo ${{ steps.a.outputs.x }}", "not declared before this one"},
		{"forward", "- id: a\n        run: echo ${{ steps.b.outputs.x }}\n      - id: b\n        run: echo hi", "not declared before this one"},
	}
	for _, tc := range cases {
		src := "name: X\non: [push]\njobs:\n  j:\n    runs-on: linux\n    steps:\n      " + tc.steps + "\n"
		_, err := Parse([]byte(src))
		if err == nil || !errors.Is(err, ErrBadOutput) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestFillStepOutputs(t *testing.T) {
	values := map[string]map[string]string{"build": {"digest": "abc", "empty": ""}}
	got, err := FillStepOutputs("x=${{ steps.build.outputs.digest }} y=${{ steps.BUILD.outputs.empty }} z=${{ matrix.go }}", values)
	if err != nil {
		t.Fatal(err)
	}
	if got != "x=abc y= z=${{ matrix.go }}" {
		t.Fatalf("FillStepOutputs = %q", got)
	}
	// An unknown id or an unpublished name is a named error.
	if _, err := FillStepOutputs("${{ steps.ghost.outputs.x }}", values); err == nil || !strings.Contains(err.Error(), `no step with id "ghost"`) {
		t.Fatalf("unknown id = %v", err)
	}
	if _, err := FillStepOutputs("${{ steps.build.outputs.nope }}", values); err == nil || !strings.Contains(err.Error(), `did not publish the output "nope"`) {
		t.Fatalf("unknown output = %v", err)
	}
	if _, err := FillStepOutputs("${{ steps.build.outputs.x", values); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("unterminated = %v", err)
	}
}

func TestStepOutputRefs(t *testing.T) {
	refs, err := StepOutputRefs("${{ matrix.go }} ${{ steps.build.outputs.digest }} ${{ inputs.x }}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Step != "build" || refs[0].Output != "digest" || refs[0].Context != "steps" {
		t.Fatalf("refs = %+v", refs)
	}
	// An unsupported span is ignored (it stays literal by design), not an error.
	if refs, err := StepOutputRefs("${{ contains(github.ref, 'x') }}"); err != nil || len(refs) != 0 {
		t.Fatalf("unsupported span = %v, %v", refs, err)
	}
}

func TestValidateOutputsCaps(t *testing.T) {
	if err := ValidateOutputs(map[string]string{"a": "b"}); err != nil {
		t.Fatalf("small map: %v", err)
	}
	if err := ValidateOutputs(map[string]string{"bad name": "x"}); err == nil {
		t.Fatal("an invalid name must fail")
	}
	if err := ValidateOutputs(map[string]string{"a": strings.Repeat("x", MaxOutputValueBytes+1)}); err == nil {
		t.Fatal("an oversized value must fail")
	}
	many := map[string]string{}
	for i := 0; i < MaxOutputsPerJob+1; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("z", i/26)] = "v"
	}
	if err := ValidateOutputs(many); err == nil || !strings.Contains(err.Error(), "per job") {
		t.Fatalf("count cap: %v", err)
	}
	// The total byte cap is separate from the per-value cap.
	total := map[string]string{}
	for i := 0; i < MaxOutputsPerJob; i++ {
		total[string(rune('a'+i%26))+strings.Repeat("q", i)] = strings.Repeat("y", MaxOutputValueBytes)
	}
	if err := ValidateOutputs(total); err == nil || !strings.Contains(err.Error(), "bytes total") {
		t.Fatalf("total cap: %v", err)
	}
}

func TestNeedsOutputRefsAndFill(t *testing.T) {
	refs, err := NeedsOutputRefs("${{ needs.build.outputs.digest }} ${{ steps.a.outputs.x }}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Job != "build" || refs[0].Output != "digest" {
		t.Fatalf("refs = %+v", refs)
	}
	values := map[string]map[string]string{"build": {"digest": "abc"}}
	if got, err := FillNeedsOutputs("x=${{ needs.BUILD.outputs.digest }}", values); err != nil || got != "x=abc" {
		t.Fatalf("FillNeedsOutputs = %q, %v", got, err)
	}
	if _, err := FillNeedsOutputs("${{ needs.ghost.outputs.x }}", values); err == nil || !strings.Contains(err.Error(), "no needs dependency with id") {
		t.Fatalf("unknown job = %v", err)
	}
	if _, err := FillNeedsOutputs("${{ needs.build.outputs.nope }}", values); err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("unset output = %v", err)
	}
	// FillOutputs replaces both kinds in one string (the runner's path).
	steps := map[string]map[string]string{"a": {"x": "one"}}
	got, err := FillOutputs("${{ steps.a.outputs.x }}/${{ needs.build.outputs.digest }}", steps, values)
	if err != nil || got != "one/abc" {
		t.Fatalf("FillOutputs = %q, %v", got, err)
	}
	if _, err := FillOutputs("${{ steps.ghost.outputs.x }}", steps, values); err == nil {
		t.Fatal("FillOutputs must fail closed on an unknown step")
	}
	if refs, err := OutputRefs("${{ needs.build.outputs.x }} ${{ steps.a.outputs.x }}"); err != nil || len(refs) != 2 {
		t.Fatalf("OutputRefs = %+v, %v", refs, err)
	}
	if _, err := OutputRefs("${{ steps.a.outputs.x"); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("unterminated = %v", err)
	}
}

func TestValidOutputNameEdge(t *testing.T) {
	for _, ok := range []string{"a", "A_1", "1st", strings.Repeat("x", MaxOutputNameLen)} {
		if !ValidOutputName(ok) {
			t.Errorf("ValidOutputName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a.b", strings.Repeat("x", MaxOutputNameLen+1)} {
		if ValidOutputName(bad) {
			t.Errorf("ValidOutputName(%q) = true", bad)
		}
	}
}

func TestParseNeedsOutputPassThrough(t *testing.T) {
	const src = `name: Pass
on: [push]
jobs:
  build:
    runs-on: linux
    outputs:
      digest: hand-made
    steps:
      - run: echo hi
  forward:
    runs-on: linux
    needs: [build]
    outputs:
      digest: ${{ needs.build.outputs.digest }}
      axis: ${{ matrix.go }}
    strategy:
      matrix:
        go: ["1.27"]
    steps:
      - run: echo ${{ needs.build.outputs.digest }}
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("a pass-through output and a needs step reference must parse: %v", err)
	}
	// An unknown needs key is that file's parse error, in a job output and in
	// a step field respectively.
	badOutput := strings.Replace(src, "needs: [build]\n    outputs:\n      digest: ${{ needs.build.outputs.digest }}",
		"needs: [build]\n    outputs:\n      digest: ${{ needs.ghost.outputs.digest }}", 1)
	if _, err := Parse([]byte(badOutput)); err == nil || !strings.Contains(err.Error(), "no `needs:` dependency") {
		t.Fatalf("unknown needs key in outputs = %v", err)
	}
	badStep := strings.Replace(src, "run: echo ${{ needs.build.outputs.digest }}", "run: echo ${{ needs.ghost.outputs.digest }}", 1)
	if _, err := Parse([]byte(badStep)); err == nil || !strings.Contains(err.Error(), "no `needs:` dependency") {
		t.Fatalf("unknown needs key in a step = %v", err)
	}
}

func TestFillContextWithStepOutputSpan(t *testing.T) {
	// A string that mixes a matrix reference with a step-output reference must
	// still substitute the matrix part (the scanner accepts all four-segment
	// output references now).
	got := FillContext("go=${{ matrix.go }} out=${{ steps.a.outputs.x }}", "matrix", map[string]string{"go": "1.27"})
	if got != "go=1.27 out=${{ steps.a.outputs.x }}" {
		t.Fatalf("FillContext = %q", got)
	}
}

func TestFillOutputsSinglePass(t *testing.T) {
	steps := map[string]map[string]string{"a": {"x": "${{ needs.build.outputs.y }}"}}
	needs := map[string]map[string]string{"build": {"y": "secret"}}
	got, err := FillOutputs("${{ steps.a.outputs.x }}/${{ matrix.go }}", steps, needs)
	if err != nil {
		t.Fatal(err)
	}
	// A substituted VALUE is data: it is never rescanned for another reference,
	// and a span this position does not own is left as authored.
	if got != "${{ needs.build.outputs.y }}/${{ matrix.go }}" {
		t.Fatalf("FillOutputs = %q", got)
	}
}
