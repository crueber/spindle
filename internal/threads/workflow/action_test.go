package workflow

import (
	"strings"
	"testing"
)

// The action metadata parser (docs/threads.md §3.4a): the accepted shapes, and
// every refused one. A refusal is a named error because it becomes the JOB's
// reason (the action is resolved at run creation, before a runner exists).

const compositeActionYAML = `
name: Setup toolchain
description: install and configure
author: someone
branding:
  icon: box
  color: blue
inputs:
  version:
    description: tool version
    required: true
    default: "1.4"
  dry run:
    description: a name with a space
outputs:
  path:
    description: installed path
    value: ${{ steps.install.outputs.path }}
runs:
  using: composite
  steps:
    - id: prepare
      run: echo preparing $INPUT_VERSION
      shell: bash
    - id: install
      run: ./install.sh
      shell: sh
      env:
        TARGET: ${{ inputs.version }}
    - uses: docker://alpine:3.20
      with:
        args: --check
`

const dockerActionYAML = `
name: Lint image
inputs:
  level:
    default: warn
runs:
  using: docker
  image: docker://ghcr.io/acme/lint:1.2
  entrypoint: /lint
  args: ["--strict"]
  env:
    LINT_MODE: ci
`

func TestParseActionCompositeShape(t *testing.T) {
	a, err := ParseAction([]byte(compositeActionYAML))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Name != "Setup toolchain" || !a.IsComposite() || a.Runs.Using != ActionUsingComposite {
		t.Fatalf("action = %+v", a)
	}
	if len(a.Inputs) != 2 || a.Inputs["version"].Default != "1.4" || !a.Inputs["version"].HasDefault || !a.Inputs["version"].Required {
		t.Fatalf("inputs = %+v", a.Inputs)
	}
	if _, ok := a.Inputs["dry run"]; !ok {
		t.Fatal("an input name with a space must be accepted (it maps to INPUT_DRY_RUN)")
	}
	if got := a.Outputs["path"].Value; got != "${{ steps.install.outputs.path }}" {
		t.Fatalf("output value = %q", got)
	}
	if len(a.Runs.Steps) != 3 {
		t.Fatalf("steps = %+v", a.Runs.Steps)
	}
	if a.Runs.Steps[0].Shell != "bash" || !strings.Contains(a.Runs.Steps[0].Run, "INPUT_VERSION") {
		t.Fatalf("step[0] = %+v", a.Runs.Steps[0])
	}
	if a.Runs.Steps[2].Uses != "docker://alpine:3.20" || a.Runs.Steps[2].With["args"] != "--check" {
		t.Fatalf("step[2] = %+v", a.Runs.Steps[2])
	}
	// The INPUT_ spelling is GitHub's: uppercase, spaces → underscores.
	if got := InputEnvName("dry run"); got != "INPUT_DRY_RUN" {
		t.Fatalf("InputEnvName = %q", got)
	}
}

func TestParseActionDockerShape(t *testing.T) {
	a, err := ParseAction([]byte(dockerActionYAML))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Runs.Using != ActionUsingDocker || a.Runs.Image != "ghcr.io/acme/lint:1.2" {
		t.Fatalf("runs = %+v", a.Runs)
	}
	if strings.Join(a.Runs.Entrypoint, " ") != "/lint" || strings.Join(a.Runs.Args, " ") != "--strict" {
		t.Fatalf("entrypoint/args = %v %v", a.Runs.Entrypoint, a.Runs.Args)
	}
	if a.Runs.Env["LINT_MODE"] != "ci" {
		t.Fatalf("env = %v", a.Runs.Env)
	}
	// A docker action cannot declare outputs: it has no steps to read them
	// from, so the declaration is a named refusal.
	if _, err := ParseAction([]byte(`
outputs:
  report:
    value: ${{ steps.x.outputs.y }}
runs:
  using: docker
  image: docker://alpine
`)); err == nil || !strings.Contains(err.Error(), "cannot declare outputs") {
		t.Fatalf("docker outputs = %v", err)
	}
}

func TestParseActionDockerEntrypointList(t *testing.T) {
	src := `
runs:
  using: docker
  image: docker://alpine
  entrypoint: ["/bin/sh", "-c"]
`
	a, err := ParseAction([]byte(src))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if len(a.Runs.Entrypoint) != 2 || a.Runs.Entrypoint[1] != "-c" {
		t.Fatalf("entrypoint = %v", a.Runs.Entrypoint)
	}
}

func TestParseActionRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing using", `
runs:
  steps:
    - run: echo hi
      shell: sh
`, "using is required"},
		{"unsupported using", `
runs:
  using: node20
  main: index.js
`, "node20"},
		{"composite without steps", `
runs:
  using: composite
`, "needs at least one step"},
		{"missing shell", `
runs:
  using: composite
  steps:
    - run: echo hi
`, "must name its shell"},
		{"bad shell", `
runs:
  using: composite
  steps:
    - run: echo hi
      shell: fish
`, "unsupported shell"},
		{"composite with both", `
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
      uses: docker://alpine
`, "both run and uses"},
		{"composite with neither", `
runs:
  using: composite
  steps:
    - name: nothing
`, "needs run or uses"},
		{"composite with an image", `
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
  image: docker://alpine
`, "image"},
		{"marketplace inside composite", `
runs:
  using: composite
  steps:
    - uses: actions/checkout@v4
`, "not available inside a composite action"},
		{"dockerfile image", `
runs:
  using: docker
  image: Dockerfile
`, "docker://"},
		{"docker without image", `
runs:
  using: docker
  entrypoint: /bin/sh
`, "needs image"},
		{"docker with outputs", `
outputs:
  x:
    value: ${{ steps.a.outputs.b }}
runs:
  using: docker
  image: docker://alpine
`, "cannot declare outputs"},
		{"docker with steps", `
runs:
  using: docker
  image: docker://alpine
  steps:
    - run: echo hi
      shell: sh
`, "steps"},
		{"unknown top-level key", `
name: x
runz:
  using: composite
`, "unknown key"},
		{"unknown input key", `
inputs:
  a:
    descriptio: typo
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, "unknown key"},
		{"unknown step key", `
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
      shel: bash
`, "unknown key"},
		{"output without value", `
outputs:
  x:
    description: nothing
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, "value is required"},
		{"output names an unknown step", `
outputs:
  x:
    value: ${{ steps.nope.outputs.y }}
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, `no step with id "nope"`},
		{"output uses another context", `
outputs:
  x:
    value: ${{ needs.build.outputs.y }}
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, "not supported"},
		{"unterminated output template", `
outputs:
  x:
    value: ${{ steps.a.outputs.y
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, "unterminated"},
		{"bad input name", `
inputs:
  "a=b":
    default: x
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`, "must not contain '='"},
		{"empty file", "", "empty file"},
		{"two documents", compositeActionYAML + "\n---\n" + compositeActionYAML, "more than one document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAction([]byte(tc.src))
			if err == nil {
				t.Fatalf("ParseAction accepted the file")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

// TestActionInputsAndEnv pins the input-default and INPUT_* rules.
func TestActionInputsAndEnv(t *testing.T) {
	a, err := ParseAction([]byte(compositeActionYAML))
	if err != nil {
		t.Fatal(err)
	}
	env := a.InputEnv(map[string]string{"version": "2.0"})
	if env["INPUT_VERSION"] != "2.0" || env["INPUT_DRY_RUN"] != "" {
		t.Fatalf("InputEnv = %v", env)
	}
	if got := a.UndeclaredInputs(map[string]string{"version": "1", "typo": "x"}); len(got) != 1 || got[0] != "typo" {
		t.Fatalf("UndeclaredInputs = %v", got)
	}
	if got := a.DeclaredInputs(); strings.Join(got, ",") != "dry run,version" {
		t.Fatalf("DeclaredInputs = %v", got)
	}
	// A missing default contributes an empty value, never an absent name.
	a2, err := ParseAction([]byte(`
inputs:
  x:
    description: no default
runs:
  using: composite
  steps:
    - run: echo hi
      shell: sh
`))
	if err != nil {
		t.Fatal(err)
	}
	if env := a2.InputEnv(nil); len(env) != 1 || env["INPUT_X"] != "" {
		t.Fatalf("InputEnv without a default = %v", env)
	}
}
