package workflow

import (
	"strings"
	"testing"
)

// Parity wave 1a (docs/features/21_parity_master.md §5): syntax core.

// TestNeedsScalarAndList pins PAR-050: `needs: test` is the same dependency set
// as `needs: [test]`.
func TestNeedsScalarAndList(t *testing.T) {
	base := func(needs string) string {
		return "name: N\non: push\njobs:\n  prep:\n    runs-on: l\n    steps:\n      - run: x\n  build:\n    runs-on: l\n    needs: " + needs + "\n    steps:\n      - run: x\n"
	}
	for _, wants := range []string{"prep", "[prep]", "['prep']"} {
		w, err := Parse([]byte(base(wants)))
		if err != nil {
			t.Fatalf("needs: %s rejected: %v", wants, err)
		}
		if got := []string(w.Jobs["build"].Needs); len(got) != 1 || got[0] != "prep" {
			t.Fatalf("needs: %s = %v", wants, got)
		}
	}
	// A scalar that names no job is still the same error.
	if _, err := Parse([]byte(base("ghost"))); err == nil || !strings.Contains(err.Error(), "needs names an unknown job") {
		t.Fatalf("unknown scalar need = %v", err)
	}
	// A mapping is not a dependency list.
	if _, err := Parse([]byte(base("{a: b}"))); err == nil {
		t.Fatal("mapping needs accepted")
	}
}

// TestMatrixIncludeExclude pins PAR-051 with GitHub's documented expansion
// examples.
func TestMatrixIncludeExclude(t *testing.T) {
	parse := func(matrix string) Matrix {
		t.Helper()
		src := "name: M\non: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n" + matrix + "\n    steps:\n      - run: x\n"
		w, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse: %v\n%s", err, src)
		}
		return w.Jobs["a"].Strategy.Matrix
	}
	cells := func(m Matrix) []string {
		out := make([]string, 0, len(m.Expand()))
		for _, cell := range m.Expand() {
			parts := make([]string, 0, len(cell.Order))
			for _, key := range cell.Order {
				parts = append(parts, key+"="+cell.Values[key])
			}
			out = append(out, strings.Join(parts, ","))
		}
		return out
	}
	check := func(name string, got, want []string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s:\n got %v\nwant %v", name, got, want)
		}
	}

	// GitHub's documented include example (fruit/animal): every cell gets
	// green, the cat cells are repainted pink, apple cells get the shape, and
	// the two banana entries become new combinations.
	m := parse(`        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
          - color: pink
            animal: cat
          - fruit: apple
            shape: circle
          - fruit: banana
          - fruit: banana
            animal: cat
`)
	check("documented include example", cells(m), []string{
		"animal=cat,fruit=apple,color=pink,shape=circle",
		"animal=cat,fruit=pear,color=pink",
		"animal=dog,fruit=apple,color=green,shape=circle",
		"animal=dog,fruit=pear,color=green",
		"fruit=banana",
		"animal=cat,fruit=banana",
	})

	// exclude removes every base combination matching ALL its pairs.
	m = parse(`        os: [linux, darwin, windows]
        go: ["1.26", "1.27"]
        exclude:
          - os: darwin
          - os: linux
            go: "1.26"
`)
	check("documented exclude", cells(m), []string{
		"go=1.26,os=windows", "go=1.27,os=linux", "go=1.27,os=windows",
	})

	// A matrix with only include entries: one cell per entry, no empty cell.
	// This is the Scriobh release shape.
	m = parse(`        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
`)
	check("include-only", cells(m), []string{"goarch=amd64,goos=linux", "goarch=arm64,goos=linux"})

	// An include entry that matches no surviving cell creates a new one; an
	// excluded cell is not resurrected.
	m = parse(`        go: ["1.26"]
        exclude:
          - { go: "1.26" }
        include:
          - { go: "1.26", experimental: "true" }
`)
	check("excluded cell not resurrected", cells(m), []string{"go=1.26,experimental=true"})

	// A plain product is unchanged.
	m = parse(`        go: ["1.26", "1.27"]
        os: [linux]
`)
	check("plain product", cells(m), []string{"go=1.26,os=linux", "go=1.27,os=linux"})
}

func TestMatrixValidation(t *testing.T) {
	src := func(matrix string) string {
		return "name: M\non: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n" + matrix + "\n    steps:\n      - run: x\n"
	}
	cases := []struct{ name, matrix string }{
		{"include is not a list", "        include:\n          go: 1.27\n"},
		{"include entry is not an object", "        include:\n          - 1.27\n"},
		{"include nested value", "        include:\n          - go: [\"1.26\"]\n"},
		{"exclude nested value", "        exclude:\n          - go: {version: \"1.26\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(src(tc.matrix))); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// A matrix over the cell cap is a parse error, not a failed run.
	var b strings.Builder
	b.WriteString("        ")
	for i := 0; i < 20; i++ {
		b.WriteString("a" + string(rune('a'+i)) + ": [\"1\", \"2\", \"3\", \"4\", \"5\", \"6\", \"7\", \"8\"]\n        ")
	}
	if _, err := Parse([]byte(src(b.String()))); err == nil || !strings.Contains(err.Error(), "more than 256") {
		t.Fatalf("over-cap matrix = %v, want the named bound", err)
	}
	// 256 cells is allowed; 257 is not.
	if _, err := Parse([]byte(src("        a: [\"" + strings.Repeat("x\", \"", 255) + "x\"]\n"))); err != nil {
		t.Fatalf("256 cells rejected: %v", err)
	}
}

// TestSecretsReferences pins PAR-031's parser half: secrets.<name> is accepted
// in the substituted sites and a malformed reference is a named error.
func TestSecretsReferences(t *testing.T) {
	src := func(site string) string {
		return "name: S\non: push\njobs:\n  a:\n    runs-on: l\n    steps:\n" + site
	}
	ok := []string{
		"      - env:\n          TOKEN: ${{ secrets.TOKEN }}\n        run: echo ${{ secrets.TOKEN }}\n",
		"      - uses: actions/checkout\n        with:\n          token: ${{ secrets.GITHUB_TOKEN }}\n",
		"      - name: ${{ secrets.TOKEN }}\n        run: true\n",
	}
	for i, site := range ok {
		if _, err := Parse([]byte(src(site))); err != nil {
			t.Fatalf("site %d rejected: %v\n%s", i, err, site)
		}
	}
	bad := []string{
		"      - run: echo ${{ secrets }}\n",
		"      - run: echo ${{ secrets.a.b }}\n",
		"      - run: echo ${{ secrets.1BAD }}\n",
		"      - run: echo ${{ secrets.TOKEN || 'x' }}\n",
	}
	for _, site := range bad {
		_, err := Parse([]byte(src(site)))
		if err == nil || !strings.Contains(err.Error(), "supported reference") && !strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad site %q = %v, want a named error", site, err)
		}
	}
}

// TestGitHubContextFields pins PAR-032: every accepted github/forgejo field is
// valid in a substituted site and in if:, and an unknown field names the set.
func TestGitHubContextFields(t *testing.T) {
	fields := []string{"event_name", "sha", "ref", "ref_name", "repository", "workflow", "job", "run_id", "actor"}
	src := func(site string) string {
		return "name: G\non: push\njobs:\n  a:\n    runs-on: l\n    steps:\n" + site
	}
	for _, field := range fields {
		for _, context := range []string{"github", "forgejo", "forge"} {
			step := "      - env:\n          V: ${{ " + context + "." + field + " }}\n        run: true\n"
			if _, err := Parse([]byte(src(step))); err != nil {
				t.Fatalf("%s.%s rejected: %v", context, field, err)
			}
			ifStep := "      - if: " + context + "." + field + " == 'x'\n        run: true\n"
			if _, err := Parse([]byte(src(ifStep))); err != nil {
				t.Fatalf("%s.%s in if rejected: %v", context, field, err)
			}
		}
	}
	// The event.inputs form requires a declared input.
	input := func(name string) string {
		return "name: G\non:\n  workflow_dispatch:\n    inputs:\n      mode:\n        type: string\njobs:\n  a:\n    runs-on: l\n    steps:\n      - env:\n          V: ${{ github.event.inputs." + name + " }}\n        run: true\n"
	}
	if _, err := Parse([]byte(input("mode"))); err != nil {
		t.Fatalf("declared event input rejected: %v", err)
	}
	if _, err := Parse([]byte(input("ghost"))); err == nil || !strings.Contains(err.Error(), "not a declared workflow_dispatch input") {
		t.Fatalf("undeclared event input = %v", err)
	}
	// An unknown field is an error naming the supported set, never empty.
	for _, site := range []string{
		"      - env:\n          V: ${{ github.head_ref }}\n        run: true\n",
		"      - if: github.head_ref == 'x'\n        run: true\n",
	} {
		_, err := Parse([]byte(src(site)))
		if err == nil || !strings.Contains(err.Error(), "github.head_ref") || !strings.Contains(err.Error(), "event_name") {
			t.Fatalf("unknown field = %v, want the field and the set named", err)
		}
	}
}

// TestSubstitutionSites pins PAR-030's parser half and the one-pass expander:
// run, with, env and name take references; a supported reference resolves; a
// secrets reference never survives as literal text.
func TestSubstitutionSites(t *testing.T) {
	values := TemplateValues{
		GitHub:  map[string]string{"ref": "refs/heads/main", "sha": "abc", "actor": "ada"},
		Matrix:  map[string]string{"go": "1.27"},
		Inputs:  map[string]string{"mode": "fast"},
		Env:     map[string]string{"FOO": "bar"},
		Secrets: map[string]string{"TOKEN": "s3cr3t", "empty": ""},
	}
	cases := []struct{ name, in, want string }{
		{"run", "echo ${{ github.sha }}", "echo abc"},
		{"with", "v=${{ matrix.go }}", "v=1.27"},
		{"env", "${{ inputs.mode }}", "fast"},
		{"name", "deploy ${{ env.FOO }}", "deploy bar"},
		{"secret", "token=${{ secrets.TOKEN }}", "token=s3cr3t"},
		{"secret case-insensitive", "token=${{ secrets.token }}", "token=s3cr3t"},
		{"absent secret is empty, never literal", "x${{ secrets.NOPE }}y", "xy"},
		{"forgejo alias", "${{ forgejo.actor }}", "ada"},
		{"event input", "${{ github.event.inputs.mode }}", "fast"},
		{"data is not rescanned", "${{ env.FOO }}", "bar"}, // value is written once
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Substitute(tc.in, values)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Substitute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// A value that itself looks like a reference is data, not a template.
	got, err := Substitute("${{ env.X }}", TemplateValues{Env: map[string]string{"X": "${{ secrets.TOKEN }}"}})
	if err != nil || got != "${{ secrets.TOKEN }}" {
		t.Fatalf("one-pass = %q, %v", got, err)
	}
	// Outputs stay strict.
	if out, err := Substitute("${{ steps.a.outputs.x }}", TemplateValues{Steps: map[string]map[string]string{"a": {"x": "1"}}}); err != nil || out != "1" {
		t.Fatalf("step output = %q, %v", out, err)
	}
	if _, err := Substitute("${{ steps.ghost.outputs.x }}", values); err == nil {
		t.Fatal("unknown step id resolved")
	}
}

// TestValidateIf pins PAR-037's parser half plus the closed context checks.
func TestValidateIf(t *testing.T) {
	scope := TemplateScope{
		Axes:   []string{"go"},
		Inputs: []string{"mode"},
		Steps:  []string{"build"},
		Needs:  []string{"prepare"},
	}
	ok := []string{
		"",
		"success()",
		"startsWith(github.ref, 'refs/tags/v')",
		"contains(github.ref, 'main') && !failure()",
		"format('{0}-{1}', matrix.go, inputs.mode) == '1.27-fast'",
		"steps.build.outcome == 'success'",
		"needs.prepare.result == 'success' || always()",
		"github.event.inputs.mode == 'fast'",
		"secrets.TOKEN != ''",
		"${{ inputs.mode }}",
		"true",
	}
	for _, expr := range ok {
		if err := ValidateIf(expr, scope); err != nil {
			t.Errorf("ValidateIf(%q): %v", expr, err)
		}
	}
	bad := []struct{ expr, want string }{
		{"endsWith(github.ref, 'x')", "unsupported function"},
		{"startsWith(github.ref)", "takes 2 argument"},
		{"format()", "at least one argument"},
		{"github.head_ref == 'x'", "not a supported context field"},
		{"matrix.nope == 'x'", "not a declared matrix key"},
		{"inputs.nope == 'x'", "not a declared workflow_dispatch input"},
		{"steps.ghost.outcome == 'success'", "not declared before this one"},
		{"needs.ghost.result == 'success'", "no `needs:` dependency"},
		{"vars.X == 'y'", "unknown context"},
		{"@", "expected a value"},
		{"${{ inputs.mode }} == 'x'", "whole condition"},
		{"false ||", "expected a value"},
	}
	for _, tc := range bad {
		err := ValidateIf(tc.expr, scope)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateIf(%q) = %v, want %q", tc.expr, err, tc.want)
		}
	}
	// A job-level if: cannot read secrets yet (PAR-058 is a later wave).
	jobScope := scope
	jobScope.JobIf = true
	if err := ValidateIf("secrets.TOKEN != ''", jobScope); err == nil || !strings.Contains(err.Error(), "not available in a job-level if") {
		t.Fatalf("job secrets = %v, want the named limitation", err)
	}
}

// TestStepTemplatesAreValidated pins that every substituted step field is
// checked at parse time, not left to fail mid-run.
func TestStepTemplatesAreValidated(t *testing.T) {
	src := func(site string) string {
		return "name: V\non: push\njobs:\n  a:\n    runs-on: l\n    steps:\n" + site
	}
	bad := []string{
		"      - run: echo ${{ matrix.undeclared }}\n",
		"      - run: echo ${{ inputs.undeclared }}\n",
		"      - env:\n          X: ${{ github.nope }}\n        run: true\n",
		"      - uses: actions/checkout\n        with:\n          token: ${{ steps.ghost.outputs.x }}\n",
	}
	for _, site := range bad {
		if _, err := Parse([]byte(src(site))); err == nil {
			t.Fatalf("accepted an unresolvable site:\n%s", site)
		}
	}
	// A declared axis/input is fine everywhere.
	good := `name: V
on:
  workflow_dispatch:
    inputs:
      mode:
        type: string
        default: fast
env:
  WORKFLOW: ok
jobs:
  a:
    runs-on: l
    strategy:
      matrix:
        go: ["1.27"]
        include:
          - { go: "1.28", experimental: "true" }
    steps:
      - name: build ${{ matrix.go }} ${{ inputs.mode }}
        env:
          X: ${{ matrix.experimental }}-${{ inputs.mode }}
        run: echo ${{ github.sha }} ${{ secrets.TOKEN }}
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("declared references rejected: %v", err)
	}
}
