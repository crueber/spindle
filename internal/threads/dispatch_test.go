package threads

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// workflow_dispatch inputs (15 §1/§6, round 2): the dispatch API accepts,
// validates and records them, and the values reach every job's spec so the
// runner can substitute `inputs.*`.

const dispatchInputsYAML = `name: Deploy
on:
  workflow_dispatch:
    inputs:
      env:
        description: target environment
        type: choice
        options: [staging, production]
        required: true
      count:
        description: replicas
        type: string
        default: "2"
      dry_run:
        description: plan only
        type: boolean
        default: "false"
      tag:
        description: image tag
jobs:
  deploy:
    runs-on: linux
    steps:
      - run: echo "${{ inputs.env }} ${{ inputs.count }} ${{ inputs.dry_run }}"
`

// dispatchEnv seeds the inputs workflow next to the default one.
func dispatchEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", dispatchInputsYAML)
	return e
}

// mustDispatch dispatches deploy.yml with the given inputs.
func (e *testEnv) mustDispatch(t *testing.T, inputs map[string]string) []*Run {
	t.Helper()
	runs, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "deploy.yml", inputs, e.writer())
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	return runs
}

// TestDispatchInputsTypesDefaultsAndRecording pins the happy path: declared
// types validate, defaults fill omitted optionals, booleans normalize, and the
// resolved values are recorded on the run and every job spec.
func TestDispatchInputsTypesDefaultsAndRecording(t *testing.T) {
	e := dispatchEnv(t)
	runs := e.mustDispatch(t, map[string]string{"env": "staging", "dry_run": "TRUE"})
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	run := runs[0]
	want := map[string]string{"env": "staging", "count": "2", "dry_run": "true"}
	if len(run.Inputs) != len(want) {
		t.Fatalf("run inputs = %v, want %v", run.Inputs, want)
	}
	for k, v := range want {
		if run.Inputs[k] != v {
			t.Fatalf("run inputs = %v, want %v", run.Inputs, want)
		}
	}
	if _, ok := run.Inputs["tag"]; ok {
		t.Fatalf("an omitted optional input must not appear: %v", run.Inputs)
	}
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.writer())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(view.Jobs) != 1 || view.Jobs[0].Spec == nil {
		t.Fatalf("jobs = %+v", view.Jobs)
	}
	if got := view.Jobs[0].Spec.Inputs["env"]; got != "staging" {
		t.Fatalf("job spec inputs = %v", view.Jobs[0].Spec.Inputs)
	}
}

// TestDispatchInputsValidation422 pins every rejection: 422 (ErrUnprocessable)
// with the offending input named.
func TestDispatchInputsValidation422(t *testing.T) {
	e := dispatchEnv(t)
	cases := []struct {
		name   string
		inputs map[string]string
		want   string // the offending input name the message must contain
	}{
		{"unknown input", map[string]string{"nope": "1"}, `"nope"`},
		{"required missing", map[string]string{}, `"env"`},
		{"choice not an option", map[string]string{"env": "moon"}, `"env"`},
		{"boolean not a bool", map[string]string{"env": "staging", "dry_run": "yes please"}, `"dry_run"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "deploy.yml", tc.inputs, e.writer())
			if !errors.Is(err, ErrUnprocessable) {
				t.Fatalf("err = %v, want ErrUnprocessable", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not name %s", err.Error(), tc.want)
			}
		})
	}
	// Inputs without a workflow name are ambiguous: 422, not a silent default.
	_, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", map[string]string{"env": "staging"}, e.writer())
	if !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("inputs without a workflow = %v", err)
	}
	// No run was created by any rejection.
	page, _ := e.svc.ListRuns(ctx(), "o/r", e.writer(), "", 50)
	if len(page.Runs) != 0 {
		t.Fatalf("a rejected dispatch created %d runs", len(page.Runs))
	}
}

// TestDispatchWithoutInputsStillWorks pins the backward-compatible call: a
// dispatch with no inputs and no workflow name creates a run per manual file.
func TestDispatchWithoutInputsStillWorks(t *testing.T) {
	e := dispatchEnv(t)
	runs, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, e.writer())
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if len(runs) != 2 { // ci.yml and deploy.yml both declare workflow_dispatch
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	for _, r := range runs {
		if r.Inputs != nil {
			t.Fatalf("run %s recorded inputs %v for an inputless dispatch", r.WorkflowPath, r.Inputs)
		}
	}
}

// TestDispatchInputsHTTP pins the wire contract: `inputs` is accepted (and is
// no longer "unknown field"), scalars are coerced, a mismatch is 422 naming the
// input, and the no-inputs body still answers 201.
func TestDispatchInputsHTTP(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = e.writer()
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", dispatchInputsYAML)

	rec := do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main","workflow":"deploy.yml","inputs":{"env":"staging","count":3}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dispatch = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Runs []*Run `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(created.Runs) != 1 || created.Runs[0].Inputs["count"] != "3" {
		t.Fatalf("runs = %+v", created.Runs)
	}

	rec = do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main","workflow":"deploy.yml","inputs":{"env":"moon"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad choice = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"env"`) {
		t.Fatalf("422 body does not name the input: %s", rec.Body.String())
	}

	// A non-scalar value is a 400 naming the input (a client bug, not a
	// schema mismatch).
	rec = do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main","workflow":"deploy.yml","inputs":{"env":{"x":1}}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"env"`) {
		t.Fatalf("nested input = %d: %s", rec.Code, rec.Body.String())
	}

	// The legacy body still works.
	rec = do(h, http.MethodPost, "/o/r/api/threads", `{"ref":"refs/heads/main"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("no-inputs dispatch = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowStateCarriesInputSchema pins the page/API discovery shape the Run
// workflow form renders (only workflow_dispatch files carry inputs).
func TestWorkflowStateCarriesInputSchema(t *testing.T) {
	e := dispatchEnv(t)
	list, err := e.svc.ListWorkflows(ctx(), "o/r", testSHA, e.writer())
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	var deploy, ci *WorkflowState
	for _, wf := range list.Workflows {
		switch wf.Path {
		case ".spindle/workflows/deploy.yml":
			deploy = wf
		case ".spindle/workflows/ci.yml":
			ci = wf
		}
	}
	if deploy == nil || len(deploy.Inputs) != 4 {
		t.Fatalf("deploy inputs = %+v", deploy)
	}
	// Sorted by name: count, dry_run, env, tag.
	if deploy.Inputs[0].Name != "count" || deploy.Inputs[2].Name != "env" {
		t.Fatalf("input order = %+v", deploy.Inputs)
	}
	env := deploy.Inputs[2]
	if env.Type != "choice" || !env.Required || len(env.Options) != 2 {
		t.Fatalf("env input = %+v", env)
	}
	count := deploy.Inputs[0]
	if count.Type != "string" || !count.HasDefault || count.Default != "2" {
		t.Fatalf("count input = %+v", count)
	}
	if ci != nil && len(ci.Inputs) != 0 {
		t.Fatalf("a file without workflow_dispatch carried inputs: %+v", ci.Inputs)
	}
}

// TestDispatchInputsReaderForbidden pins the gate: the page/API affordance is
// never the authorisation (a reader's dispatch is a real 403).
func TestDispatchInputsReaderForbidden(t *testing.T) {
	e := dispatchEnv(t)
	_, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "deploy.yml",
		map[string]string{"env": "staging"}, auth.Principal{Name: "reader"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader dispatch = %v", err)
	}
}

// TestDispatchRunEdgeCases pins the remaining dispatch branches: a named file
// that fails to parse is 422 with its own error, an untyped declared input is a
// string, and a nil file label degrades to "the workflow".
func TestDispatchRunEdgeCases(t *testing.T) {
	e := dispatchEnv(t)
	e.git.add(testSHA, ".spindle/workflows/broken.yml", "on: {workflow_dispatch: null}\njobs: {}\n")
	_, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "broken.yml", nil, e.writer())
	if !errors.Is(err, ErrUnprocessable) || !strings.Contains(err.Error(), "broken.yml") {
		t.Fatalf("broken dispatch = %v", err)
	}
	// An input type is optional: `type:` absent means string.
	untyped := `name: Untyped
on:
  workflow_dispatch:
    inputs:
      raw:
        description: anything
jobs:
  go:
    runs-on: linux
    steps:
      - run: echo hi
`
	e.git.add(testSHA, ".spindle/workflows/untyped.yml", untyped)
	runs, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "untyped.yml", map[string]string{"raw": "x"}, e.writer())
	if err != nil || len(runs) != 1 || runs[0].Inputs["raw"] != "x" {
		t.Fatalf("untyped dispatch = %+v, %v", runs, err)
	}
	// The label fallbacks used by the validation messages.
	if got := workflowLabel(nil); got != "the workflow" {
		t.Fatalf("workflowLabel(nil) = %q", got)
	}
	if _, err := resolveDispatchInputs(&WorkflowFile{}, map[string]string{"x": "1"}, nil); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("unknown input on an undeclared workflow = %v", err)
	}
}

// TestDurationHelpers pins the derived-duration edge cases (no start, an
// unparseable start, a clock that runs backwards).
func TestDurationHelpers(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	bad := "not a time"
	late := "2026-11-20T13:00:00Z"
	early := "2026-11-20T11:00:00Z"
	if got := elapsedSeconds(nil, nil, now); got != 0 {
		t.Fatalf("no start = %d", got)
	}
	if got := elapsedSeconds(&bad, nil, now); got != 0 {
		t.Fatalf("bad start = %d", got)
	}
	if got := elapsedSeconds(&late, nil, now); got != 0 {
		t.Fatalf("start in the future = %d", got)
	}
	if got := elapsedSeconds(&early, &bad, now); got != 3600 {
		t.Fatalf("bad completed = %d", got)
	}
	// fillDurations tolerates a nil run and nil jobs (defensive projection).
	e := newTestEnv(t)
	e.svc.fillDurations(nil, []*Job{nil})
}

// TestDispatchInputTypesExtended pins PAR-023: number coercion and the
// environment picker's server-side validation (an `environment` value must name
// an existing repository environment).
func TestDispatchInputTypesExtended(t *testing.T) {
	f := &WorkflowFile{Path: "deploy.yml", Workflow: &workflow.Workflow{
		On: workflow.Triggers{WorkflowDispatch: &workflow.DispatchTrigger{Inputs: map[string]workflow.Input{
			"replicas":    {Type: "number"},
			"environment": {Type: "environment", Required: true},
		}}},
	}}
	envs := []string{"staging", "production"}
	got, err := resolveDispatchInputs(f, map[string]string{"replicas": "3", "environment": "production"}, envs)
	if err != nil {
		t.Fatalf("valid dispatch: %v", err)
	}
	if got["replicas"] != "3" || got["environment"] != "production" {
		t.Fatalf("effective = %+v", got)
	}
	if _, err := resolveDispatchInputs(f, map[string]string{"replicas": "three", "environment": "production"}, envs); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("non-numeric number err = %v", err)
	}
	if _, err := resolveDispatchInputs(f, map[string]string{"replicas": "3", "environment": "moon"}, envs); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("unknown environment err = %v", err)
	}
}
