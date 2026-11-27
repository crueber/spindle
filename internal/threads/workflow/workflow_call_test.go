package workflow

// PAR-075/PAR-076: the reusable-workflow contract (`on.workflow_call`) and the
// composite `github.action_path` field (PAR-074). Every refusal here is a NAMED
// parse error: an accepted key that the engine cannot enforce would be the
// accepted-and-inert failure the parser exists to prevent.

import (
	"strings"
	"testing"
)

const reusableCalleeSource = `name: Deploy
on:
  workflow_call:
    inputs:
      target:
        description: where to deploy
        type: string
        required: true
      dry-run:
        type: boolean
        default: false
      retries:
        type: number
        default: 2
    secrets:
      DEPLOY_TOKEN:
        description: registry token
        required: true
      OPTIONAL:
        description: an optional token
    outputs:
      url:
        description: the deployed URL
        value: ${{ jobs.release.outputs.url }}
jobs:
  release:
    runs-on: linux
    outputs:
      url: ${{ steps.publish.outputs.url }}
    steps:
      - id: publish
        run: echo "url=https://example.test" >> "$GITHUB_OUTPUT"
        env:
          TOKEN: ${{ secrets.DEPLOY_TOKEN }}
`

func TestWorkflowCallParses(t *testing.T) {
	w, err := Parse([]byte(reusableCalleeSource))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	call := w.On.WorkflowCall
	if call == nil {
		t.Fatal("on.workflow_call is nil")
	}
	if got := strings.Join(call.CallInputNames(), ","); got != "dry-run,retries,target" {
		t.Fatalf("inputs = %q", got)
	}
	if got := strings.Join(call.CallSecretNames(), ","); got != "DEPLOY_TOKEN,OPTIONAL" {
		t.Fatalf("secrets = %q", got)
	}
	if got := strings.Join(call.CallOutputNames(), ","); got != "url" {
		t.Fatalf("outputs = %q", got)
	}
	if call.Inputs["dry-run"].Type != "boolean" || call.Inputs["retries"].Type != "number" {
		t.Fatalf("types = %+v", call.Inputs)
	}
	if !call.Inputs["target"].Required {
		t.Fatal("target must be required")
	}
	if !call.Secrets["DEPLOY_TOKEN"].Required || call.Secrets["OPTIONAL"].Required {
		t.Fatalf("secret declarations = %+v", call.Secrets)
	}
}

func TestWorkflowCallBareDeclarationIsCallable(t *testing.T) {
	w, err := Parse([]byte("name: Bare\non: workflow_call\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo a\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.On.WorkflowCall == nil || len(w.On.WorkflowCall.Inputs) != 0 {
		t.Fatalf("bare workflow_call = %+v", w.On.WorkflowCall)
	}
	// The empty mapping spelling too.
	w2, err := Parse([]byte("name: Bare\non:\n  workflow_call:\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo a\n"))
	if err != nil || w2.On.WorkflowCall == nil {
		t.Fatalf("empty workflow_call: %v %+v", err, w2.On.WorkflowCall)
	}
}

func TestNormalizeCallInputs(t *testing.T) {
	decl := map[string]CallInput{
		"target":  {Type: "string", Required: true},
		"dry":     {Type: "boolean", Default: "false", HasDefault: true},
		"retries": {Type: "number", Default: "2", HasDefault: true},
	}
	got, err := NormalizeCallInputs(decl, map[string]string{"target": "prod"})
	if err != nil {
		t.Fatalf("NormalizeCallInputs: %v", err)
	}
	if got["target"] != "prod" || got["dry"] != "false" || got["retries"] != "2" {
		t.Fatalf("resolved = %v", got)
	}
	if _, err := NormalizeCallInputs(decl, map[string]string{}); err == nil || !strings.Contains(err.Error(), "required input") {
		t.Fatalf("missing required = %v", err)
	}
	if _, err := NormalizeCallInputs(decl, map[string]string{"target": "prod", "typo": "x"}); err == nil || !strings.Contains(err.Error(), "not a declared `on.workflow_call` input") {
		t.Fatalf("unknown input = %v", err)
	}
	if _, err := NormalizeCallInputs(decl, map[string]string{"target": "prod", "dry": "yes"}); err == nil || !strings.Contains(err.Error(), "is a boolean") {
		t.Fatalf("bad boolean = %v", err)
	}
	if _, err := NormalizeCallInputs(decl, map[string]string{"target": "prod", "retries": "many"}); err == nil || !strings.Contains(err.Error(), "is a number") {
		t.Fatalf("bad number = %v", err)
	}
}

func TestWorkflowCallRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "missing-type",
			src:  "name: C\non:\n  workflow_call:\n    inputs:\n      x: {required: true}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "type is required",
		},
		{
			name: "bad-type",
			src:  "name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: object}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "not supported",
		},
		{
			name: "bad-boolean-default",
			src:  "name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: boolean, default: yes}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "is a boolean",
		},
		{
			name: "bad-number-default",
			src:  "name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: number, default: many}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "is a number",
		},
		{
			name: "output-not-a-jobs-ref",
			src:  "name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ steps.a.outputs.x }}\"}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "must read a job output",
		},
		{
			name: "output-missing-job",
			src:  "name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.nope.outputs.x }}\"}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
			want: "names no job of this file",
		},
		{
			name: "output-undeclared-name",
			src:  "name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.a.outputs.nope }}\"}\njobs:\n  a: {runs-on: linux, outputs: {real: \"${{ steps.s.outputs.x }}\"}, steps: [{id: s, run: echo a}]}\n",
			want: "does not declare the output",
		},
		{
			name: "output-names-a-caller",
			src:  "name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.call.outputs.x }}\"}\njobs:\n  call: {uses: \"./.spindle/workflows/x.yml\"}\n",
			want: "names a reusable-workflow caller",
		},
		{
			name: "undeclared-secret-reference",
			src:  "name: C\non:\n  workflow_call:\n    secrets:\n      DECLARED: {}\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n        env:\n          T: \"${{ secrets.OTHER }}\"\n",
			want: "is not declared by this workflow's `on.workflow_call.secrets`",
		},
		{
			name: "with-without-uses",
			src:  "name: C\non: [push]\njobs:\n  a:\n    runs-on: linux\n    with: {x: y}\n    steps: [{run: echo a}]\n",
			want: "declares `with:` but no `uses:`",
		},
		{
			name: "secrets-without-uses",
			src:  "name: C\non: [push]\njobs:\n  a:\n    runs-on: linux\n    secrets: inherit\n    steps: [{run: echo a}]\n",
			want: "declares `secrets:` but no `uses:`",
		},
		{
			name: "with-secret-reference",
			src:  "name: C\non: [push]\njobs:\n  a:\n    uses: \"./.spindle/workflows/x.yml\"\n    with:\n      token: \"${{ secrets.T }}\"\n",
			want: "is not available in a reusable-workflow `with:` value",
		},
		{
			name: "with-needs-reference",
			src:  "name: C\non: [push]\njobs:\n  build: {runs-on: linux, steps: [{run: echo b}]}\n  a:\n    needs: [build]\n    uses: \"./.spindle/workflows/x.yml\"\n    with:\n      token: \"${{ needs.build.outputs.t }}\"\n",
			want: "is not available in a reusable-workflow `with:` value",
		},
		{
			name: "cross-repo-inherit",
			src:  "name: C\non: [push]\njobs:\n  a:\n    uses: \"acme/other/.spindle/workflows/x.yml@1111111111111111111111111111111111111111\"\n    secrets: inherit\n",
			want: "D-6",
		},
		{
			name: "secrets-literal",
			src:  "name: C\non: [push]\njobs:\n  a:\n    uses: \"./.spindle/workflows/x.yml\"\n    secrets:\n      TOKEN: hunter2\n",
			want: "must be exactly ${{ secrets.<name> }}",
		},
		{
			name: "caller-outputs",
			src:  "name: C\non: [push]\njobs:\n  a:\n    uses: \"./.spindle/workflows/x.yml\"\n    outputs: {x: \"${{ steps.s.outputs.x }}\"}\n",
			want: "", // a caller `outputs:` map is refused at run creation, not parse (workflow.Job is shared with run jobs)
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if c.want == "" {
				if err != nil && !strings.Contains(err.Error(), "outputs") {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

const compositeActionSource = `name: tool
inputs:
  version:
    default: "1.0"
outputs:
  bin:
    value: ${{ steps.install.outputs.bin }}
runs:
  using: composite
  steps:
    - id: install
      run: echo "${{ github.action_path }}/install.sh" "${{ inputs.version }}"
      shell: bash
    - id: check
      if: github.action_path != ''
      run: ls "${{ github.action_path }}"
      shell: sh
    - uses: ./nested
      with:
        dir: ${{ github.action_path }}
`

func TestCompositeActionReadsActionPath(t *testing.T) {
	a, err := ParseAction([]byte(compositeActionSource))
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if len(a.Runs.Steps) != 3 {
		t.Fatalf("steps = %d", len(a.Runs.Steps))
	}
}

func TestActionPathOutsideCompositeIsRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "workflow-step",
			src:  "name: W\non: [push]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo \"${{ github.action_path }}\"\n",
			want: "only available in a composite action's own steps",
		},
		{
			name: "job-if",
			src:  "name: W\non: [push]\njobs:\n  a:\n    runs-on: linux\n    if: github.action_path != ''\n    steps:\n      - run: echo a\n",
			want: "only available in a composite action's own steps",
		},
		{
			name: "concurrency-group",
			src:  "name: W\non: [push]\nconcurrency:\n  group: \"${{ github.action_path }}\"\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo a\n",
			want: "is not supported",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestCompositeActionTemplateRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "undeclared-input",
			src: `name: a
runs:
  using: composite
  steps:
    - run: echo "${{ inputs.nope }}"
      shell: bash
`,
			want: "is not a declared workflow_dispatch input",
		},
		{
			name: "unknown-step",
			src: `name: a
runs:
  using: composite
  steps:
    - run: echo "${{ steps.nope.outputs.x }}"
      shell: bash
`,
			want: "names a step that is not declared before this one",
		},
		{
			name: "output-reads-action-path",
			src: `name: a
outputs:
  x:
    value: ${{ github.action_path }}
runs:
  using: composite
  steps:
    - id: s
      run: echo x
      shell: bash
`,
			want: "an output reads steps.<id>.outputs.<name>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseAction([]byte(c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestSubstituteCallOutputs(t *testing.T) {
	jobs := map[string]map[string]string{
		"release": {"url": "https://example.test", "sha": "abc"},
	}
	got, err := SubstituteCallOutputs(`${{ jobs.release.outputs.url }}/page`, jobs)
	if err != nil || got != "https://example.test/page" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := SubstituteCallOutputs("literal", jobs); err != nil || got != "literal" {
		t.Fatalf("literal = %q, %v", got, err)
	}
	if _, err := SubstituteCallOutputs(`${{ jobs.nope.outputs.url }}`, jobs); err == nil || !strings.Contains(err.Error(), "no job") {
		t.Fatalf("unknown job = %v", err)
	}
	if _, err := SubstituteCallOutputs(`${{ jobs.release.outputs.nope }}`, jobs); err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("unknown output = %v", err)
	}
	if _, err := SubstituteCallOutputs(`${{ needs.release.outputs.url }}`, jobs); err == nil || !strings.Contains(err.Error(), "unknown context") {
		t.Fatalf("wrong context = %v", err)
	}
	if _, err := SubstituteCallOutputs(`${{ jobs.release.outputs.url`, jobs); err == nil {
		t.Fatal("an unterminated span must fail")
	}
}

func TestNeedsOutputNames(t *testing.T) {
	refs, err := NeedsOutputNames(`echo "${{ needs.deploy.outputs.url }}" && ${{ needs.build.outputs.sha }}`)
	if err != nil {
		t.Fatalf("NeedsOutputNames: %v", err)
	}
	if len(refs) != 2 || refs[0].Job != "deploy" || refs[0].Output != "url" || refs[1].Job != "build" {
		t.Fatalf("refs = %+v", refs)
	}
	// An if: expression is scanned the same way, and a token inside a longer
	// identifier is not a reference.
	refs, err = NeedsOutputNames(`needs.deploy.outputs.url == 'x' && myneeds.a.outputs.b != '' && needs.deploy.result == 'success'`)
	if err != nil {
		t.Fatalf("expression: %v", err)
	}
	if len(refs) != 1 || refs[0].Job != "deploy" || refs[0].Output != "url" {
		t.Fatalf("expression refs = %+v", refs)
	}
	refs, err = NeedsOutputNames(`nothing here`)
	if err != nil || len(refs) != 0 {
		t.Fatalf("no refs = %+v, %v", refs, err)
	}
}

func TestWholeSecretRef(t *testing.T) {
	ok := map[string]string{
		"${{ secrets.TOKEN }}": "TOKEN",
		"  ${{secrets._X1}}  ": "_X1",
	}
	for in, want := range ok {
		got, valid := wholeSecretRef(in)
		if !valid || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, valid, want)
		}
	}
	for _, in := range []string{
		"hunter2",
		"${{ secrets.TOKEN }}/x",
		"${{ github.token }}",
		"${{ secrets.TOKEN }}${{ secrets.OTHER }}",
		"${{ }}",
		"prefix ${{ secrets.TOKEN }}",
	} {
		if _, valid := wholeSecretRef(in); valid {
			t.Errorf("%q was accepted as a secret reference", in)
		}
	}
}

func TestJobSecretsDecoding(t *testing.T) {
	w, err := Parse([]byte(`name: C
on: [push]
jobs:
  a:
    uses: ./.spindle/workflows/x.yml
    secrets: inherit
  b:
    uses: ./.spindle/workflows/x.yml
    secrets:
      TOKEN: ${{ secrets.REGISTRY_TOKEN }}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !w.Jobs["a"].Secrets.Inherit || w.Jobs["a"].Secrets.Values != nil {
		t.Fatalf("inherit = %+v", w.Jobs["a"].Secrets)
	}
	if w.Jobs["b"].Secrets.Inherit || w.Jobs["b"].Secrets.Values["TOKEN"] != "REGISTRY_TOKEN" {
		t.Fatalf("map = %+v", w.Jobs["b"].Secrets)
	}
	bad := map[string]string{
		"scalar": "secrets: everything\n",
		"list":   "secrets: [a]\n",
		"name":   "secrets:\n      \"bad name\": ${{ secrets.X }}\n",
		"value":  "secrets:\n      TOKEN: 123\n",
	}
	for name, block := range bad {
		src := "name: C\non: [push]\njobs:\n  a:\n    uses: ./.spindle/workflows/x.yml\n    " + block
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: refused block accepted: %s", name, block)
		}
	}
}

func TestWorkflowCallDeclarationRefusals(t *testing.T) {
	cases := map[string]string{
		"unknown-key":   "on:\n  workflow_call:\n    bogus: {}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"input-list":    "on:\n  workflow_call:\n    inputs: [x]\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"input-name":    "on:\n  workflow_call:\n    inputs:\n      \"1bad\": {type: string}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"input-keys":    "on:\n  workflow_call:\n    inputs:\n      x: {type: string, bogus: 1}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"secret-list":   "on:\n  workflow_call:\n    secrets: [x]\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"secret-name":   "on:\n  workflow_call:\n    secrets:\n      \"bad name\": {}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"secret-keys":   "on:\n  workflow_call:\n    secrets:\n      X: {bogus: 1}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"output-list":   "on:\n  workflow_call:\n    outputs: [x]\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"output-name":   "on:\n  workflow_call:\n    outputs:\n      \"bad name\": {value: x}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"output-keys":   "on:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.a.outputs.x }}\", bogus: 1}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"output-no-val": "on:\n  workflow_call:\n    outputs:\n      x: {description: no value}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"trigger-list":  "on:\n  workflow_call: [x]\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: refused declaration accepted", name)
		}
	}
}

func TestWorkflowCallNilsAndScalarForms(t *testing.T) {
	var call *WorkflowCallTrigger
	if call.CallInputNames() != nil || call.CallSecretNames() != nil || call.CallOutputNames() != nil {
		t.Fatal("nil declaration must list nothing")
	}
	// A bare scalar spelling is a legal callable.
	if _, err := Parse([]byte("name: C\non: workflow_call\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n")); err != nil {
		t.Fatalf("scalar trigger: %v", err)
	}
	if _, err := Parse([]byte("name: C\non: [push, workflow_call]\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n")); err != nil {
		t.Fatalf("list trigger: %v", err)
	}
	// Wrong scalar shapes inside the declaration decode as errors.
	for _, src := range []string{
		"name: C\non:\n  workflow_call: 3\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    inputs:\n      x: nope\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    secrets:\n      X: nope\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    outputs:\n      x: nope\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: [string]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: string, description: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    inputs:\n      x: {type: string, required: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    secrets:\n      X: {required: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    secrets:\n      X: {description: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.a.outputs.x }}\", description: [a]}\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n",
		"name: C\non:\n  workflow_call:\n    outputs:\n      x: {value: \"${{ jobs.a.outputs.x }}\"}\njobs:\n  a: {runs-on: linux, outputs: {x: \"${{ steps.s.outputs.x }}\"}, steps: [{id: s, run: echo a}]}\n  b: {runs-on: linux, steps: [{run: echo \"${{ needs.a.outputs.x }}\"}]}\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("accepted: %s", src)
		}
	}
}

func TestJobsContextOutsideCallOutput(t *testing.T) {
	src := "name: W\non: [push]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo \"${{ jobs.a.outputs.x }}\"\n"
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "only available in an `on.workflow_call.outputs") {
		t.Fatalf("jobs context = %v", err)
	}
	// The permission key helper is part of the closed vocabulary.
	if !IsZeroPermissionKey(KeyAll) || IsZeroPermissionKey("contents") {
		t.Fatal("IsZeroPermissionKey disagrees with the closed set")
	}
}

func TestSubstituteActionPathNeedsTheRunnerValue(t *testing.T) {
	if _, err := Substitute("${{ github.action_path }}", TemplateValues{}); err == nil {
		t.Fatal("an absent action_path must be an error, never the empty string")
	}
	got, err := Substitute("${{ github.action_path }}/install.sh", TemplateValues{
		GitHub: map[string]string{"action_path": "/spindle/workspace/.spindle/actions/setup"},
	})
	if err != nil || got != "/spindle/workspace/.spindle/actions/setup/install.sh" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := Substitute("${{ github.ref }}", TemplateValues{GitHub: map[string]string{"ref": "refs/heads/main"}}); err != nil {
		t.Fatalf("an ordinary github field must still substitute: %v", err)
	}
}
