package workflow

import (
	"fmt"
	"strings"
	"testing"
)

// Concurrency-group templates (round-2 audit): a group is the lock that decides
// which run executes, so its `${{ … }}` vocabulary is closed and validated at
// parse time. An unsupported or undeclared reference fails THIS file naming the
// key and the supported set — never a literal fallback that would make every
// branch share one key.

const groupFile = `name: G
on: %s
concurrency: %s
jobs:
  build:
    runs-on: linux
    %s
    steps:
      - run: true
`

// parseGroup builds a one-job workflow with the given trigger, workflow
// concurrency and job body (the job body may carry its own concurrency/matrix).
func parseGroup(t *testing.T, on, concurrency, body string) (*Workflow, error) {
	t.Helper()
	return Parse([]byte(fmt.Sprintf(groupFile, on, concurrency, body)))
}

func TestParseConcurrencyGroupAcceptssupportedContexts(t *testing.T) {
	cases := []struct{ name, on, group, body string }{
		{"no expression", "push", "ci-main", ""},
		{"github ref", "push", `"ci-${{ github.ref }}"`, ""},
		{"github fields", "push", `"${{ github.repository }}-${{ github.workflow }}-${{ github.event_name }}-${{ github.sha }}-${{ github.run_id }}-${{ github.ref_name }}"`, ""},
		{"declared input", "push", "~", ""},
		{"job matrix axis", "push", "~", "concurrency: \"build-${{ matrix.go }}\"\n    strategy:\n      matrix:\n        go: [\"1.26\", \"1.27\"]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseGroup(t, tc.on, tc.group, tc.body); err != nil {
				t.Fatalf("Parse: %v", err)
			}
		})
	}
}

func TestParseConcurrencyGroupAcceptsDeclaredInput(t *testing.T) {
	src := `name: G
on:
  workflow_dispatch:
    inputs:
      mode:
        default: fast
concurrency: "ci-${{ inputs.mode }}"
jobs:
  build:
    runs-on: linux
    steps:
      - run: true
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParseConcurrencyGroupRejectsUnresolvableRefs(t *testing.T) {
	cases := []struct {
		name, concurrency, body string
		want                    []string
	}{
		{
			name:        "workflow-level matrix is not available",
			concurrency: `"ci-${{ matrix.go }}"`,
			want:        []string{"concurrency.group", "matrix.go", "workflow-level", "move `concurrency` into the job"},
		},
		{
			name:        "undeclared matrix axis",
			concurrency: "~",
			body:        "concurrency: \"build-${{ matrix.os }}\"\n    strategy:\n      matrix:\n        go: [\"1.26\"]",
			want:        []string{"concurrency.group", "matrix.os", "not a declared matrix axis", "declared: go"},
		},
		{
			name:        "input outside a dispatch",
			concurrency: `"ci-${{ inputs.mode }}"`,
			want:        []string{"concurrency.group", "inputs.mode", "not a declared workflow_dispatch input", "declared: none"},
		},
		{
			name:        "unknown context names the supported set",
			concurrency: `"ci-${{ env.BRANCH }}"`,
			want:        []string{"concurrency.group", "env.BRANCH", "github.ref, github.ref_name, github.repository", "matrix.<axis>", "inputs.<name>"},
		},
		{
			name:        "unknown github field",
			concurrency: `"ci-${{ github.head_ref }}"`,
			want:        []string{"concurrency.group", "github.head_ref", "not supported", "github.ref"},
		},
		{
			name:        "operator expression is not a reference",
			concurrency: `"ci-${{ github.ref == 'refs/heads/main' }}"`,
			want:        []string{"concurrency.group", "is not a supported reference"},
		},
		{
			name:        "unterminated",
			concurrency: `"ci-${{ github.ref"`,
			want:        []string{"concurrency.group", "unterminated"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseGroup(t, "push", tc.concurrency, tc.body)
			if err == nil {
				t.Fatal("Parse accepted an unresolvable concurrency group")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestParseJobConcurrencyNamesTheJob(t *testing.T) {
	_, err := parseGroup(t, "push", "~", "concurrency: \"x-${{ matrix.os }}\"\n    strategy:\n      matrix:\n        go: [\"1.26\"]")
	if err == nil || !strings.Contains(err.Error(), "job build: concurrency.group") {
		t.Fatalf("error = %v, want the job named", err)
	}
}

func TestTemplateRefs(t *testing.T) {
	refs, err := TemplateRefs("ci-${{ github.ref }}-${{matrix.go}}-${{ inputs.Mode }}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Context != "github" || refs[0].Key != "ref" {
		t.Fatalf("ref 0 = %+v", refs[0])
	}
	if refs[1].Context != "matrix" || refs[1].Key != "go" {
		t.Fatalf("ref 1 = %+v", refs[1])
	}
	if refs[2].Context != "inputs" || refs[2].Key != "Mode" {
		t.Fatalf("ref 2 = %+v", refs[2])
	}
	if _, err := TemplateRefs("${{ github }}"); err == nil {
		t.Fatal("a bare context is not a reference")
	}
	if _, err := TemplateRefs("${{ github. }}"); err == nil {
		t.Fatal("an empty key is not a reference")
	}
	if _, err := TemplateRefs("${{ github.re f }}"); err == nil {
		t.Fatal("a key with a space is not a reference")
	}
}

func TestExpandGroupIsStrict(t *testing.T) {
	github := map[string]string{"ref": "refs/heads/main", "repository": "o/r"}
	group, err := ExpandGroup("ci-${{ github.ref }}-${{ matrix.go }}-${{ inputs.mode }}", github,
		map[string]string{"go": "1.27"}, map[string]string{"mode": "Fast"})
	if err != nil {
		t.Fatal(err)
	}
	if group != "ci-refs/heads/main-1.27-Fast" {
		t.Fatalf("group = %q", group)
	}
	// A plain group is unchanged (no expression, no cost).
	if got, gerr := ExpandGroup("ci-main", nil, nil, nil); gerr != nil || got != "ci-main" {
		t.Fatalf("plain group = %q, %v", got, gerr)
	}
	// Missing and empty values are errors naming the key, never an empty segment.
	for _, tc := range []struct {
		tmpl   string
		github map[string]string
		matrix map[string]string
		inputs map[string]string
		key    string
	}{
		{"ci-${{ github.ref }}", map[string]string{}, nil, nil, "github.ref"},
		{"ci-${{ github.ref }}", map[string]string{"ref": "  "}, nil, nil, "github.ref"},
		{"ci-${{ matrix.os }}", nil, map[string]string{"go": "1.27"}, nil, "matrix.os"},
		{"ci-${{ inputs.mode }}", nil, nil, map[string]string{}, "inputs.mode"},
	} {
		_, gerr := ExpandGroup(tc.tmpl, tc.github, tc.matrix, tc.inputs)
		if gerr == nil {
			t.Fatalf("%q resolved without %s", tc.tmpl, tc.key)
		}
		if !strings.Contains(gerr.Error(), tc.key) || !strings.Contains(gerr.Error(), "supported:") {
			t.Fatalf("error %q must name %s and the supported set", gerr, tc.key)
		}
	}
}

func TestFillTemplateLeavesUnknownRefsVisible(t *testing.T) {
	got := FillTemplate("cell-${{ matrix.v }}-${{ env.MISSING }}", map[string]string{"ref": "x"},
		map[string]string{"v": "a"}, nil)
	if got != "cell-a-${{ env.MISSING }}" {
		t.Fatalf("FillTemplate = %q", got)
	}
	if got := FillTemplate("plain", nil, nil, nil); got != "plain" {
		t.Fatalf("plain = %q", got)
	}
	if got := FillTemplate("${{ unterminated", nil, nil, nil); got != "${{ unterminated" {
		t.Fatalf("malformed = %q", got)
	}
}

func TestFillContextOnlyTouchesItsContext(t *testing.T) {
	got := FillContext("${{ matrix.go }}-${{ inputs.go }}-${{ matrix.other }}", "matrix", map[string]string{"go": "1.27"})
	if got != "1.27-${{ inputs.go }}-${{ matrix.other }}" {
		t.Fatalf("FillContext = %q", got)
	}
	// Authored names are case-sensitive (the step substitutions pin this).
	if got := FillContext("${{ matrix.GO }}", "matrix", map[string]string{"go": "1.27"}); got != "${{ matrix.GO }}" {
		t.Fatalf("case-sensitive = %q", got)
	}
}
