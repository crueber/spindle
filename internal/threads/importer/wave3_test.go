package importer

// PAR-075/PAR-076 importer coverage: the reusable-workflow forms are carried
// where they now parse, and everything the target does not implement stays a
// named refusal (never an inert key).

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestReusableCallIsCarried(t *testing.T) {
	src := `name: CI
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  deploy:
    needs: [build]
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: ${{ github.ref_name }}
    secrets:
      DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}
`
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 0 {
		t.Fatalf("untranslated = %+v", res.Untranslated)
	}
	out := string(res.Output)
	for _, want := range []string{"uses: ./.spindle/workflows/deploy.yml", "target: ${{ github.ref_name }}", "DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output misses %q:\n%s", want, out)
		}
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, res.Output)
	}
}

func TestReusableRemoteCallIsCarried(t *testing.T) {
	sha := strings.Repeat("a", 40)
	src := "name: CI\non: push\njobs:\n  deploy:\n    uses: acme/other/.spindle/workflows/deploy.yml@" + sha + "\n"
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 0 || !strings.Contains(string(res.Output), sha) {
		t.Fatalf("remote call = %+v\n%s", res.Untranslated, res.Output)
	}
}

func TestReusableCrossRepoInheritIsRefused(t *testing.T) {
	src := `name: CI
on: push
jobs:
  deploy:
    uses: acme/other/.spindle/workflows/deploy.yml@` + strings.Repeat("a", 40) + `
    secrets: inherit
`
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 1 {
		t.Fatalf("untranslated = %+v", res.Untranslated)
	}
	if !strings.Contains(res.Untranslated[0].Reason, "D-6") {
		t.Fatalf("reason = %q", res.Untranslated[0].Reason)
	}
	if strings.Contains(string(res.Output), "\n    secrets: inherit") {
		t.Fatalf("the inherit block survived:\n%s", res.Output)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, res.Output)
	}
}

func TestWorkflowCallTriggerIsCarried(t *testing.T) {
	src := `name: Deploy
on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
    secrets:
      DEPLOY_TOKEN:
        required: true
    outputs:
      url:
        value: ${{ jobs.release.outputs.url }}
jobs:
  release:
    runs-on: linux
    outputs:
      url: ${{ steps.p.outputs.url }}
    steps:
      - id: p
        run: echo "url=x" >> "$GITHUB_OUTPUT"
`
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 0 {
		t.Fatalf("untranslated = %+v\n%s", res.Untranslated, res.Output)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, res.Output)
	}
	out := string(res.Output)
	for _, want := range []string{"workflow_call:", "type: string", "DEPLOY_TOKEN:", "value: ${{ jobs.release.outputs.url }}"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output misses %q:\n%s", want, out)
		}
	}
}

func TestWorkflowCallUnsupportedDeclarationIsDropped(t *testing.T) {
	src := `name: Deploy
on:
  workflow_call:
    inputs:
      flavour:
        type: choice
        options: [a, b]
jobs:
  release:
    runs-on: linux
    steps:
      - run: echo x
`
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 1 || !strings.Contains(res.Untranslated[0].Reason, "workflow_call") {
		t.Fatalf("untranslated = %+v", res.Untranslated)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, res.Output)
	}
}

func TestReusableWithSecretReferenceIsRefused(t *testing.T) {
	src := `name: CI
on: push
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      token: ${{ secrets.TOKEN }}
`
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 1 || !strings.Contains(res.Untranslated[0].Reason, "not available in a reusable-workflow `with:` value") {
		t.Fatalf("untranslated = %+v", res.Untranslated)
	}
}

func TestReusableFormsRefusedTable(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := []struct{ name, src, want string }{
		{"secrets-without-uses", "name: C\non: push\njobs:\n  a:\n    runs-on: linux\n    secrets: inherit\n    steps:\n      - run: echo a\n", "only valid on a reusable-workflow caller"},
		{"secrets-not-a-map", "name: C\non: push\njobs:\n  a:\n    uses: ./.spindle/workflows/x.yml\n    secrets: 3\n", "must be `inherit` or a mapping"},
		{"uses-escape", "name: C\non: push\njobs:\n  a:\n    uses: ./../x.yml\n", "path inside this repository"},
		{"uses-wrong-dir", "name: C\non: push\njobs:\n  a:\n    uses: ./.spindle/actions/x.yml\n", "only read from .spindle/workflows/"},
		{"uses-not-yaml", "name: C\non: push\njobs:\n  a:\n    uses: ./.spindle/workflows/x.txt\n", ".yml or .yaml"},
		{"uses-docker", "name: C\non: push\njobs:\n  a:\n    uses: docker://alpine:3.20\n", "not an image or a URL"},
		{"uses-url", "name: C\non: push\njobs:\n  a:\n    uses: https://example.test/x.yml\n", "not an image or a URL"},
		{"uses-no-pin", "name: C\non: push\njobs:\n  a:\n    uses: acme/other/.spindle/workflows/x.yml\n", "pinned to a commit"},
		{"uses-bad-sha", "name: C\non: push\njobs:\n  a:\n    uses: acme/other/.spindle/workflows/x.yml@" + strings.Repeat("z", 40) + "\n", "40-character commit SHA"},
		{"uses-foreign-dir", "name: C\non: push\njobs:\n  a:\n    uses: acme/other/.github/workflows/x.yml@" + sha + "\n", "only read from .spindle/workflows/"},
		{"uses-bad-shape", "name: C\non: push\njobs:\n  a:\n    uses: acme/other/x.yml@" + sha + "\n", "only read from .spindle/workflows/"},
		{"workflow-call-scalar", "name: C\non:\n  workflow_call: 3\njobs:\n  a: {runs-on: linux, steps: [{run: echo a}]}\n", "must be a mapping"},
		{"uses-not-string", "name: C\non: push\njobs:\n  a:\n    uses: [x]\n", "must be a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Translate(GitHub, []byte(c.src))
			if err != nil {
				// A refusal may also surface as an unconvertible source (a
				// malformed `uses:` list); that is still fail-closed.
				if !strings.Contains(err.Error(), c.want) && !strings.Contains(err.Error(), "cannot convert") {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				return
			}
			if len(res.Untranslated) == 0 || !strings.Contains(res.Untranslated[0].Reason, c.want) {
				t.Fatalf("untranslated = %+v, want %q", res.Untranslated, c.want)
			}
			if _, perr := workflow.Parse(res.Output); perr != nil {
				t.Fatalf("does not parse: %v\n%s", perr, res.Output)
			}
		})
	}
}

func TestReusableSameRepoInheritIsCarried(t *testing.T) {
	src := "name: C\non: push\njobs:\n  a:\n    uses: ./.spindle/workflows/x.yml\n    secrets: inherit\n"
	res, err := Translate(GitHub, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Untranslated) != 0 || !strings.Contains(string(res.Output), "secrets: inherit") {
		t.Fatalf("inherit = %+v\n%s", res.Untranslated, res.Output)
	}
}
