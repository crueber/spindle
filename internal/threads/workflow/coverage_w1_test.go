package workflow

import (
	"strings"
	"testing"
)

// Coverage for the wave-1a additions that the behaviour tables do not reach.

func TestMatrixEnabledAndNull(t *testing.T) {
	if (Matrix{}).Enabled() {
		t.Fatal("zero matrix enabled")
	}
	if !(Matrix{Include: []MatrixCombination{{"a": "b"}}}).Enabled() {
		t.Fatal("include-only matrix not enabled")
	}
	if !(Matrix{Axes: map[string][]string{"go": {"1.27"}}}).Enabled() {
		t.Fatal("axis matrix not enabled")
	}
	if (Matrix{Axes: map[string][]string{"go": {}}}).Enabled() {
		t.Fatal("empty axis list enabled")
	}
	// `matrix:` with no value decodes to the zero value and yields one cell.
	w, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatalf("empty matrix rejected: %v", err)
	}
	if cells := w.Jobs["a"].Strategy.Matrix.Expand(); len(cells) != 1 || len(cells[0].Values) != 0 {
		t.Fatalf("empty matrix cells = %v", cells)
	}
	if files := (Matrix{Axes: map[string][]string{"go": {}}}).FieldNames(); len(files) != 0 {
		t.Fatalf("empty axis is a field: %v", files)
	}
}

func TestValidSecretName(t *testing.T) {
	for _, name := range []string{"TOKEN", "_x", "A1_b"} {
		if !ValidSecretName(name) {
			t.Errorf("%q rejected", name)
		}
	}
	for _, name := range []string{"", "1TOKEN", "A-B", "A.B", strings.Repeat("A", 65)} {
		if ValidSecretName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestTemplateRefsShapes(t *testing.T) {
	refs, err := TemplateRefs("${{ github.event.inputs.mode }}-${{ forgejo.ref }}-${{ secrets.X }}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0].Key != "event.inputs.mode" || refs[1].Context != "forgejo" || refs[2].Context != "secrets" {
		t.Fatalf("refs = %+v", refs)
	}
	for _, bad := range []string{"${{ github.event.inputs }}", "${{ github.event.inputs.1x }}", "${{ github.other.inputs.x }}"} {
		var err error
		if _, err = TemplateRefs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A job name references the declared axes and inputs, and nothing else.
	w, err := Parse([]byte(`name: J
on:
  workflow_dispatch:
    inputs:
      mode:
        type: string
jobs:
  a:
    runs-on: l
    name: cell-${{ matrix.go }}-${{ github.actor }}
    strategy:
      matrix:
        go: ["1.27"]
    steps:
      - run: x
`))
	if err != nil {
		t.Fatalf("job name: %v", err)
	}
	if w.Jobs["a"].Name == "" {
		t.Fatal("job name lost")
	}
	if _, err := Parse([]byte(`name: J
on:
  workflow_dispatch:
jobs:
  a:
    runs-on: l
    name: cell-${{ matrix.go }}
    steps:
      - run: x
`)); err == nil || !strings.Contains(err.Error(), "not a declared matrix key") {
		t.Fatalf("undeclared job-name axis = %v", err)
	}
}

func TestValidateIfErrorShapes(t *testing.T) {
	scope := TemplateScope{Axes: []string{"go"}, Steps: []string{"build"}, Needs: []string{"prep"}}
	for _, tc := range []struct{ expr, want string }{
		{"(success()", "missing )"},
		{"success() success()", "unexpected"},
		{"steps.build", "only steps.<id>"},
		{"needs.prep", "only needs.<job>"},
		{"steps.nope.outputs.x == '1'", "not declared before this one"},
		{"needs.nope.outputs.x == '1'", "no `needs:` dependency"},
		{"contains(github.ref 'x')", "expected , or )"},
		{"contains(github.ref,)", "expected a value"},
		{"!", "expected a value"},
		{"'unterminated", "unterminated string"},
		{"${{ }}", "is empty"},
		{"github == 'x'", "unknown identifier"},
	} {
		err := ValidateIf(tc.expr, scope)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateIf(%q) = %v, want %q", tc.expr, err, tc.want)
		}
	}
}

func TestSubstituteErrors(t *testing.T) {
	if _, err := Substitute("${{ github.ref", TemplateValues{}); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("unterminated = %v", err)
	}
	if _, err := Substitute("${{ vars.X }}", TemplateValues{}); err == nil || !strings.Contains(err.Error(), "unknown context") {
		t.Fatalf("unknown context = %v", err)
	}
	if _, err := Substitute("${{ needs.ghost.outputs.x }}", TemplateValues{}); err == nil || !strings.Contains(err.Error(), "no needs dependency") {
		t.Fatalf("unknown need = %v", err)
	}
	if _, err := Substitute("${{ steps.a.outputs.ghost }}", TemplateValues{Steps: map[string]map[string]string{"a": {"x": "1"}}}); err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("unknown output = %v", err)
	}
	// A bare literal is returned unchanged, without scanning.
	if got, err := Substitute("plain", TemplateValues{}); err != nil || got != "plain" {
		t.Fatalf("plain = %q, %v", got, err)
	}
}

func TestMatrixValidateEdges(t *testing.T) {
	// No axes and no include is not an over-cap matrix.
	if err := (Matrix{}).Validate(); err != nil {
		t.Fatalf("empty matrix = %v", err)
	}
	// The include entries count towards the cap.
	m := Matrix{Axes: map[string][]string{"go": {"1", "2"}}}
	for i := 0; i < MaxMatrixCells; i++ {
		m.Include = append(m.Include, MatrixCombination{"i": "x"})
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "more than 256") {
		t.Fatalf("include cap = %v", err)
	}
	if got := (Matrix{Axes: map[string][]string{"go": {"1", "2", "3"}}}).Expand(); len(got) != 3 {
		t.Fatalf("expand = %d cells", len(got))
	}
}

func TestNeedsNullAccepted(t *testing.T) {
	// `needs:` with no value is "no dependencies", not an error.
	w, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    needs:\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatalf("null needs: %v", err)
	}
	if len(w.Jobs["a"].Needs) != 0 {
		t.Fatalf("needs = %v", w.Jobs["a"].Needs)
	}
}

func TestRefKindAndEventInputFill(t *testing.T) {
	if RefBranch.String() != "branch" || RefTag.String() != "tag" {
		t.Fatalf("RefKind.String = %q/%q", RefBranch, RefTag)
	}
	// A job name can read a declared dispatch input through both spellings.
	got := FillTemplate("${{ github.event.inputs.mode }}-${{ inputs.mode }}",
		map[string]string{"ref": "x"}, nil, map[string]string{"mode": "fast"})
	if got != "fast-fast" {
		t.Fatalf("FillTemplate event input = %q", got)
	}
	// wholeSpan rejects a string that is not exactly one span.
	if _, ok := wholeSpan("${{ a }} ${{ b }}"); ok {
		t.Fatal("two spans reported as one")
	}
	if _, ok := wholeSpan("x${{ a }}"); ok {
		t.Fatal("prefixed span reported as one")
	}
}

func TestMatrixShapeErrors(t *testing.T) {
	src := func(matrix string) string {
		return "name: M\non: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n" + matrix + "\n    steps:\n      - run: x\n"
	}
	// A null include/exclude declares an empty list.
	if _, err := Parse([]byte(src("        include:\n        exclude:\n"))); err != nil {
		t.Fatalf("null include/exclude rejected: %v", err)
	}
	for _, matrix := range []string{
		"        exclude: 1\n",                      // scalar exclude
		"        include:\n          - {\"\": x}\n", // empty key
		"        include:\n          - key:\n",      // null value
	} {
		if _, err := Parse([]byte(src(matrix))); err == nil {
			t.Fatalf("matrix %q accepted", matrix)
		}
	}
	if _, err := Parse([]byte("name: M\non: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix: [a]\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("sequence matrix accepted")
	}
}

func TestGroupEventInputRejected(t *testing.T) {
	_, err := parseGroup(t, "workflow_dispatch", `"ci-${{ github.event.inputs.mode }}"`, "")
	if err == nil || !strings.Contains(err.Error(), "write ${{ inputs.<name> }}") {
		t.Fatalf("group event.inputs = %v", err)
	}
}
