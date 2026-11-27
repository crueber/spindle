package threads

// PAR-080 plan round-trip: the parser's execution keys reach threads.StepSpec
// exactly as the runner's plan reads them (background/wait/wait-all/cancel and
// a recursive parallel group), with no invented fields.
import (
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestStepExecutionKeysReachStepSpec(t *testing.T) {
	src := []byte(`name: W
on: push
jobs:
  test:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: server
        name: dev
        run: ./bin/server
        background: true
        timeout-minutes: 5
      - name: smoke
        run: ./bin/smoke
      - wait: server
      - cancel: all
      - wait-all: true
      - name: lint
        parallel:
          - id: go
            run: ./bin/lint-go
          - run: ./bin/lint-web
            continue-on-error: true
`)
	w, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	job := w.Jobs["test"]
	spec := buildSpec("W", w, job, nil)
	if len(spec.Steps) != 7 {
		t.Fatalf("steps = %d", len(spec.Steps))
	}
	server := spec.Steps[1]
	if !server.Background || server.ID != "server" || server.TimeoutMinutes != 5 {
		t.Fatalf("background step = %+v", server)
	}
	if spec.Steps[3].Wait != "server" || spec.Steps[3].Background {
		t.Fatalf("wait step = %+v", spec.Steps[3])
	}
	if spec.Steps[4].Cancel != "all" {
		t.Fatalf("cancel step = %+v", spec.Steps[4])
	}
	if !spec.Steps[5].WaitAll {
		t.Fatalf("wait-all step = %+v", spec.Steps[5])
	}
	group := spec.Steps[6]
	if len(group.Parallel) != 2 {
		t.Fatalf("parallel group = %+v", group)
	}
	if group.Parallel[0].ID != "go" || group.Parallel[0].Run != "./bin/lint-go" {
		t.Fatalf("member 0 = %+v", group.Parallel[0])
	}
	if !group.Parallel[1].ContinueOnError || group.Parallel[1].Background {
		t.Fatalf("member 1 = %+v", group.Parallel[1])
	}
	// A non-parallel step never carries an empty slice (omitempty semantics).
	if spec.Steps[0].Parallel != nil {
		t.Fatalf("checkout step parallel = %#v", spec.Steps[0].Parallel)
	}
}
