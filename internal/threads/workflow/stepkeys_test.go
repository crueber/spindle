// PAR-080 parser side: background, wait/wait-all/cancel and parallel steps.
// The shapes mirror internal/threads/runner/plan.go exactly; an invalid
// combination is a named parse error, never an accepted-and-ignored key.
package workflow

import (
	"strings"
	"testing"
)

func parseSteps(t *testing.T, steps string) (*Workflow, error) {
	t.Helper()
	return Parse([]byte("name: W\non: push\njobs:\n  test:\n    runs-on: linux\n    steps:\n" + steps))
}

func TestStepExecutionKeysParse(t *testing.T) {
	valid := map[string]string{
		"background run": `
      - id: server
        run: ./bin/server
        background: true
      - run: ./bin/smoke
      - cancel: server`,
		"background docker": `
      - id: redis
        uses: docker://redis:7
        background: true
      - wait: redis`,
		"background local action": `
      - id: tool
        uses: ./.spindle/actions/tool
        background: true
      - wait-all: true`,
		"wait all scalar": `
      - id: server
        run: ./bin/server
        background: true
      - wait: all`,
		"cancel all": `
      - id: server
        run: ./bin/server
        background: true
      - cancel: all`,
		"parallel with ids and continue-on-error": `
      - name: lint
        parallel:
          - id: go
            run: ./bin/lint-go
          - id: web
            run: ./bin/lint-web
            continue-on-error: true`,
		"parallel group id": `
      - id: lint
        name: both linters
        if: always()
        parallel:
          - run: ./bin/lint-go
          - run: ./bin/lint-web`,
	}
	for name, steps := range valid {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSteps(t, steps); err != nil {
				t.Fatalf("must parse: %v", err)
			}
		})
	}
}

func TestStepExecutionKeysRefused(t *testing.T) {
	cases := map[string]struct {
		steps string
		want  string
	}{
		"wait and wait-all": {
			steps: `
      - id: s
        run: ./s
        background: true
      - wait: s
        wait-all: true`,
			want: "mutually exclusive",
		},
		"wait and cancel": {
			steps: `
      - id: s
        run: ./s
        background: true
      - wait: s
        cancel: s`,
			want: "mutually exclusive",
		},
		"control with run": {
			steps: `
      - id: s
        run: ./s
        background: true
      - wait: s
        run: echo no`,
			want: "runs no process",
		},
		"control with id": {
			steps: `
      - id: s
        run: ./s
        background: true
      - id: join
        wait: s`,
			want: "cannot own an id",
		},
		"wait unknown id": {
			steps: `      - wait: nope`,
			want:  `names background step "nope"`,
		},
		"wait forward reference": {
			steps: `
      - wait: later
      - id: later
        run: ./later
        background: true`,
			want: "started earlier",
		},
		"wait self reference": {
			steps: `
      - id: self
        run: ./self
        wait: self
        background: true`,
			want: "runs no process",
		},
		"background without id": {
			steps: `      - run: ./server
        background: true`,
			want: "needs an id",
		},
		"background on a built-in action": {
			steps: `
      - id: checkout
        uses: actions/checkout
        background: true`,
			want: "only on a run or docker step",
		},
		"background parallel group": {
			steps: `
      - id: group
        background: true
        parallel:
          - run: ./a`,
			want: "cannot itself be background",
		},
		"parallel group with run": {
			steps: `
      - run: ./a
        parallel:
          - run: ./b`,
			want: "takes only member steps",
		},
		"parallel empty": {
			steps: `      - parallel: []`,
			want:  "no member steps",
		},
		"parallel nested": {
			steps: `
      - parallel:
          - parallel:
              - run: ./a`,
			want: "nested parallel",
		},
		"parallel background member": {
			steps: `
      - parallel:
          - run: ./a
            background: true`,
			want: "ordinary run/uses step",
		},
		"parallel wait member": {
			steps: `
      - parallel:
          - wait: all`,
			want: "ordinary run/uses step",
		},
		"parallel member without verb": {
			steps: `
      - parallel:
          - id: empty`,
			want: "neither run nor uses",
		},
		"parallel member with both verbs": {
			steps: `
      - parallel:
          - run: ./a
            uses: docker://alpine`,
			want: "both run and uses",
		},
		"member reads a sibling member output": {
			steps: `
      - parallel:
          - id: first
            run: ./first
          - run: echo ${{ steps.first.outputs.url }}`,
			want: "not declared before this one",
		},
		"duplicate member id": {
			steps: `
      - parallel:
          - id: x
            run: ./a
          - id: x
            run: ./b`,
			want: "duplicate step id",
		},
		"duplicate member and outer id": {
			steps: `
      - id: x
        parallel:
          - id: x
            run: ./a`,
			want: "duplicate step id",
		},
		"wait list shape": {
			steps: `
      - id: s
        run: ./s
        background: true
      - wait: [s]`,
			want: "cannot unmarshal",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseSteps(t, tc.steps)
			if err == nil {
				t.Fatalf("must be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestCompositeActionRefusesExecutionKeys(t *testing.T) {
	for name, body := range map[string]string{
		"background": "- run: ./x\n  shell: sh\n  background: true\n  id: x",
		"wait":       "- wait: all",
		"wait-all":   "- wait-all: true",
		"cancel":     "- cancel: all",
		"parallel":   "- parallel:\n    - run: ./x\n      shell: sh",
	} {
		t.Run(name, func(t *testing.T) {
			src := "name: A\ndescription: d\nruns:\n  using: composite\n  steps:\n    " + strings.ReplaceAll(body, "\n", "\n    ") + "\n"
			if _, err := ParseAction([]byte(src)); err == nil || !strings.Contains(err.Error(), "not accepted inside a composite action") {
				t.Fatalf("composite %s: %v", name, err)
			}
		})
	}
}
