package actions

import (
	"strings"
	"testing"
)

const compositeYAML = `
name: Cosign Installer
description: install cosign
inputs:
  install-dir:
    description: where
    default: "."
  version:
    required: true
  dry-run:
    default: false
outputs:
  cosign-path:
    description: path
    value: ${{ steps.install.outputs.path }}
runs:
  using: composite
  steps:
    - id: install
      name: install
      shell: bash
      run: |
        echo "installing $INPUT_VERSION into ${{ inputs.install-dir }}"
        echo "path=$GITHUB_ACTION_PATH/cosign" >> "$GITHUB_OUTPUT"
    - name: image
      uses: docker://alpine:3.20
      with:
        args: --version
    - name: nested
      uses: sigstore/cosign-installer/nested@v3
`

func TestParseActionComposite(t *testing.T) {
	a, err := ParseAction([]byte(compositeYAML))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Using != UsingComposite || a.Name != "Cosign Installer" {
		t.Fatalf("action = %+v", a)
	}
	if len(a.Steps) != 3 || a.Steps[0].ID != "install" || a.Steps[0].Shell != "bash" {
		t.Fatalf("steps = %+v", a.Steps)
	}
	if a.Steps[2].Uses != "sigstore/cosign-installer/nested@v3" {
		t.Fatalf("nested remote uses = %q (PAR-074)", a.Steps[2].Uses)
	}
	if got := a.Inputs["dry-run"]; !got.HasDefault || got.Default != "false" {
		t.Fatalf("default = %+v (a YAML scalar default is its text)", got)
	}
	if !a.Inputs["version"].Required {
		t.Fatal("version must be required")
	}
	if a.Outputs["cosign-path"] == "" {
		t.Fatal("output template missing")
	}
}

func TestParseActionNode(t *testing.T) {
	a, err := ParseAction([]byte(`
name: hello
inputs:
  who:
    default: world
runs:
  using: node20
  main: dist/index.js
  pre: dist/pre.js
  post: dist/post.js
  pre-if: runner.os == 'Linux'
  post-if: always()
`))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Using != UsingNode20 || a.Main != "dist/index.js" || a.Pre != "dist/pre.js" || a.Post != "dist/post.js" {
		t.Fatalf("node action = %+v", a)
	}
	if a.PreIf == "" || a.PostIf != "always()" {
		t.Fatalf("hook conditions = %q/%q", a.PreIf, a.PostIf)
	}
}

func TestParseActionNode24(t *testing.T) {
	a, err := ParseAction([]byte("runs:\n  using: node24\n  main: main.js\n"))
	if err != nil || a.Using != UsingNode24 {
		t.Fatalf("node24 = %+v %v", a, err)
	}
}

func TestParseActionDocker(t *testing.T) {
	a, err := ParseAction([]byte(`
runs:
  using: docker
  image: docker://alpine:3.20
  entrypoint: /bin/sh
  args: ["-c", "echo hi"]
  env:
    MODE: strict
`))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Image != "docker://alpine:3.20" || a.IsDockerfile() {
		t.Fatalf("image = %q", a.Image)
	}
	if len(a.Entrypoint) != 1 || len(a.Args) != 2 || a.Env["MODE"] != "strict" {
		t.Fatalf("docker action = %+v", a)
	}
}

func TestParseActionDockerfile(t *testing.T) {
	for _, image := range []string{"Dockerfile", "./Dockerfile", "docker/Dockerfile"} {
		a, err := ParseAction([]byte("runs:\n  using: docker\n  image: " + image + "\n"))
		if err != nil {
			t.Fatalf("%q: %v", image, err)
		}
		if !a.IsDockerfile() || a.Image != strings.TrimPrefix(image, "./") {
			t.Fatalf("%q = %+v", image, a)
		}
	}
}

// TestParseActionRefusals names every strict rule: a typo is never a silent
// no-op, and an end-of-life runtime is a named refusal.
func TestParseActionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"empty", "", "empty"},
		{"unknown top key", "name: x\ntypo: y\nruns:\n  using: node20\n  main: m.js\n", "field typo"},
		{"missing using", "runs:\n  main: m.js\n", "using is required"},
		{"node16", "runs:\n  using: node16\n  main: m.js\n", "end-of-life"},
		{"unknown using", "runs:\n  using: wasm\n  steps: []\n", "not supported"},
		{"node without main", "runs:\n  using: node20\n", "main"},
		{"node main not js", "runs:\n  using: node20\n  main: m.sh\n", ".js"},
		{"node main escapes", "runs:\n  using: node20\n  main: ../m.js\n", "escapes"},
		{"node main absolute", "runs:\n  using: node20\n  main: /m.js\n", "must be action-directory-relative"},
		{"pre-if without pre", "runs:\n  using: node20\n  main: m.js\n  pre-if: always()\n", "pre-if needs a pre hook"},
		{"post-if without post", "runs:\n  using: node20\n  main: m.js\n  post-if: always()\n", "post-if needs a post hook"},
		{"node with steps", "runs:\n  using: node20\n  main: m.js\n  steps:\n    - run: x\n      shell: sh\n", "node action has using, main"},
		{"composite empty", "runs:\n  using: composite\n  steps: []\n", "at least one step"},
		{"composite run without shell", "runs:\n  using: composite\n  steps:\n    - run: echo hi\n", "must name its shell"},
		{"composite both run and uses", "runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: sh\n      uses: ./x\n", "both run and uses"},
		{"composite neither", "runs:\n  using: composite\n  steps:\n    - name: x\n", "needs run or uses"},
		{"composite control step", "runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: sh\n      background: true\n", "not accepted inside a composite"},
		{"composite bad uses", "runs:\n  using: composite\n  steps:\n    - uses: nope\n", "not a supported action reference"},
		{"composite local escape", "runs:\n  using: composite\n  steps:\n    - uses: ../x\n", "needs @<ref>"},
		{"composite duplicate id", "runs:\n  using: composite\n  steps:\n    - id: a\n      run: x\n      shell: sh\n    - id: A\n      run: y\n      shell: sh\n", "duplicate step id"},
		{"composite output without value", "runs:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\noutputs:\n  o:\n    description: d\n", "value is required"},
		{"docker image missing", "runs:\n  using: docker\n", "needs image"},
		{"docker bare image", "runs:\n  using: docker\n  image: alpine:3.20\n", "neither a docker:// image nor a Dockerfile"},
		{"docker output", "runs:\n  using: docker\n  image: docker://alpine:3.20\noutputs:\n  o:\n    value: ${{ steps.x }}\n", "cannot declare outputs"},
		{"composite with image", "runs:\n  using: composite\n  steps:\n    - run: x\n      shell: sh\n  image: docker://alpine:3.20\n", "composite action has using and steps"},
		{"two documents", "runs:\n  using: node20\n  main: m.js\n---\nruns:\n  using: node20\n  main: n.js\n", "more than one document"},
		{"bad input name", "inputs:\n  'a b':\n    default: x\nruns:\n  using: node20\n  main: m.js\n", "not a valid input name"},
	} {
		if _, err := ParseAction([]byte(tc.src)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestClassifyUses(t *testing.T) {
	for _, tc := range []struct {
		uses string
		kind usesKind
	}{
		{"./x", usesLocal},
		{"docker://alpine:3.20", usesDocker},
		{"owner/repo@v1", usesRemote},
		{"https://host/o/r@v1", usesRemote},
		{"owner/repo/sub@v1", usesRemote},
		{"actions/checkout", usesRemote},
	} {
		kind, _, err := classifyUses(tc.uses)
		if err != nil || kind != tc.kind {
			t.Fatalf("%q: kind = %v err = %v", tc.uses, kind, err)
		}
	}
	if _, _, err := classifyUses(""); err == nil {
		t.Fatal("empty uses must be refused")
	}
}

func TestStringMap(t *testing.T) {
	got := stringMap(map[string]any{"a": "x", "b": true, "c": 3, "d": nil, "e": 1.5, "f": int64(2), "g": uint64(4)})
	want := map[string]string{"a": "x", "b": "true", "c": "3", "d": "", "e": "1.5", "f": "2", "g": "4"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("stringMap[%q] = %q, want %q (%v)", k, got[k], v, got)
		}
	}
}
