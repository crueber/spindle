package workflow

import (
	"strings"
	"testing"
)

// Edge coverage for the action metadata parser: every shape a strict parser
// must refuse with a NAMED error (the message becomes the job's reason), and
// the scalar/list and default spellings GitHub accepts.

func TestParseActionEdgeShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"root is a list", "- a\n- b\n", "action:"},
		{"runs is a scalar", "runs: composite\n", "action.runs: expected a mapping"},
		{"using is not a string", "runs:\n  using: [composite]\n", "action.runs.using"},
		{"inputs is not a mapping", "inputs: [a]\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "action.inputs: expected a mapping"},
		{"outputs is not a mapping", "outputs: [a]\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "action.outputs: expected a mapping"},
		{"steps is not a list", "runs:\n  using: composite\n  steps: nope\n", "action.runs.steps: expected a list"},
		{"step is not a mapping", "runs:\n  using: composite\n  steps: [nope]\n", "action.runs.steps[0]"},
		{"entrypoint is a mapping", "runs:\n  using: docker\n  image: docker://alpine\n  entrypoint: {a: b}\n", "entrypoint"},
		{"args list has a mapping", "runs:\n  using: docker\n  image: docker://alpine\n  args: [a, {b: c}]\n", "args"},
		{"env is not a mapping", "runs:\n  using: docker\n  image: docker://alpine\n  env: [a]\n", "env"},
		{"image is a list", "runs:\n  using: docker\n  image: [docker://alpine]\n", "action.runs.image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAction([]byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseActionInputAndOutputDetails(t *testing.T) {
	a, err := ParseAction([]byte(`
name: x
description: d
author: a
branding:
  icon: box
inputs:
  a:
    description: one
    required: true
  b:
    default: true
outputs:
  o:
    description: out
    value: ${{ steps.step.outputs.x }}
runs:
  using: composite
  steps:
    - id: step
      run: echo x
      shell: python3
`))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if !a.Inputs["a"].Required || a.Inputs["a"].HasDefault {
		t.Fatalf("input a = %+v", a.Inputs["a"])
	}
	if a.Inputs["b"].Default != "true" {
		t.Fatalf("a boolean default is the scalar text: %+v", a.Inputs["b"])
	}
	if a.Outputs["o"].Description != "out" {
		t.Fatalf("output = %+v", a.Outputs["o"])
	}
}

func TestParseActionRefusalDetails(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"empty input name", "inputs:\n  \"\":\n    default: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "an input name must not be empty"},
		{"long input name", "inputs:\n  \"" + strings.Repeat("x", 129) + "\":\n    default: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "longer than 128"},
		{"newline input name", "inputs:\n  \"a\\nb\":\n    default: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "must not contain"},
		{"input default is a mapping", "inputs:\n  a:\n    default: {x: y}\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "default must be a scalar"},
		{"input unknown key", "inputs:\n  a:\n    defaults: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "unknown key"},
		{"output unknown key", "outputs:\n  a:\n    val: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "unknown key"},
		{"output bad name", "outputs:\n  \"a b\":\n    value: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "invalid output name"},
		{"docker:// with no image in a composite", "runs:\n  using: composite\n  steps:\n    - uses: docker://\n", "needs an image reference"},
		{"missing using with no runs keys", "runs: {}\n", "using is required"},
		{"unknown action key", "nam: x\nruns:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n", "unknown key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAction([]byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestParseActionEveryShell pins the shell vocabulary a composite run step may
// use (the runner's shell resolution is the same set).
func TestParseActionEveryShell(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "python", "python3", "pwsh", "powershell", "cmd", "bash {0}"} {
		src := "runs:\n  using: composite\n  steps:\n    - run: echo x\n      shell: " + shell + "\n"
		if _, err := ParseAction([]byte(src)); err != nil {
			t.Errorf("shell %q: %v", shell, err)
		}
	}
	if _, err := ParseAction([]byte("runs:\n  using: composite\n  steps:\n    - run: echo x\n      shell: \"\"\n")); err == nil {
		t.Fatal("an empty shell must be refused")
	}
}

// TestParseActionUsesVocabulary pins what a composite step may `uses:`: a
// repository-local action or a docker image. A bare name is neither.
func TestParseActionUsesVocabulary(t *testing.T) {
	base := "runs:\n  using: composite\n  steps:\n    - uses: "
	for _, tc := range []struct {
		uses string
		ok   bool
		want string
	}{
		{"./.spindle/actions/x", true, ""},
		{"docker://alpine:3.20", true, ""},
		{"someaction", false, "not a supported action reference"},
		{"owner/repo@v1", false, "not available inside a composite action"},
	} {
		_, err := ParseAction([]byte(base + tc.uses + "\n"))
		if tc.ok && err != nil {
			t.Fatalf("%q: %v", tc.uses, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%q: %v, want %q", tc.uses, err, tc.want)
		}
	}
}
