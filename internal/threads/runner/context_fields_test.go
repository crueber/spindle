package runner

// context_fields_test.go — the runner half of PAR-032: the full documented
// `github`/`spindle` identity/ref field set resolves from the fetched job, the
// two per-composite fields are supplied inside an action, and the fields the
// run model cannot value are named errors rather than empty strings.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestPlanCarriesTheFullGitHubFieldSet(t *testing.T) {
	job := fixtureJob()
	job.Actor = "ada"
	job.WorkflowPath = ".spindle/workflows/ci.yml"
	job.WorkflowCommit = "wfsha111"
	job.EventPayload = map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"head": map[string]any{"ref": "feature/x", "sha": "headsha"},
			"base": map[string]any{"ref": "main", "sha": "basesha"},
		},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t", ServerURL: "https://s.example"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := map[string]string{
		"ref":              "refs/heads/main",
		"ref_name":         "main",
		"ref_type":         "branch",
		"actor":            job.Actor,
		"triggering_actor": job.Actor,
		"repository":       "acme/mono",
		"repository_owner": "acme",
		"run_id":           job.RunID,
		"run_attempt":      "2",
		"workflow":         "CI",
		"workflow_ref":     "acme/mono/.spindle/workflows/ci.yml@refs/heads/main",
		"workflow_sha":     "wfsha111",
		"head_ref":         "feature/x",
		"base_ref":         "main",
	}
	for field, expected := range want {
		if got := p.GitHub[field]; got != expected {
			t.Errorf("github.%s = %q, want %q", field, got, expected)
		}
	}
	// The same values resolve through a step substitution and a step if:.
	values := p.stepValues(p.Env, nil, true)
	got, serr := p.substitute("${{ spindle.workflow_ref }} ${{ github.workflow_sha }} ${{ forgejo.head_ref }}", values)
	if serr != nil || got != "acme/mono/.spindle/workflows/ci.yml@refs/heads/main wfsha111 feature/x" {
		t.Fatalf("substitute = %q, %v", got, serr)
	}
	if ok, eerr := EvalIf(`spindle.triggering_actor == github.actor && forgejo.ref_type == 'branch'`,
		ExprContext{GitHub: p.GitHub, Site: workflow.SiteStepIf}); eerr != nil || !ok {
		t.Fatalf("if: field set = %v, %v", ok, eerr)
	}
	// The environment carries GitHub's own spellings too, so a script and the
	// context agree.
	for key, expected := range map[string]string{
		"GITHUB_REF_TYPE":         "branch",
		"GITHUB_REPOSITORY_OWNER": "acme",
		"GITHUB_RUN_ATTEMPT":      "2",
		"GITHUB_WORKFLOW_SHA":     "wfsha111",
		"GITHUB_HEAD_REF":         "feature/x",
		"GITHUB_BASE_REF":         "main",
		"GITHUB_TRIGGERING_ACTOR": job.Actor,
		"GITHUB_WORKFLOW_REF":     "acme/mono/.spindle/workflows/ci.yml@refs/heads/main",
	} {
		if p.ContainerEnv[key] != expected {
			t.Errorf("%s = %q, want %q", key, p.ContainerEnv[key], expected)
		}
	}
	// A field the model cannot value is a named error, never empty.
	if _, err := p.substitute("${{ github.actor_id }}", values); err == nil || !strings.Contains(err.Error(), "no value in this run") {
		t.Fatalf("actor_id = %v, want the named error", err)
	}
	if _, err := p.substitute("${{ github.ref_protected }}", values); err == nil || !strings.Contains(err.Error(), "ref_protected") {
		t.Fatalf("ref_protected = %v, want the named error", err)
	}
}

// TestCompositeStepSuppliesActionRef pins the two per-composite fields the
// runner owns: a remote action's own steps read its resolved ref, a local
// action reads the empty string (its documented value), and both resolve.
func TestCompositeStepSuppliesActionRef(t *testing.T) {
	job := fixtureJob()
	uses := threads.RemoteActionPrefix + "acme/act@v1"
	job.Spec.Steps = []threads.StepSpec{{Name: "inner", Uses: uses}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		uses: {
			Using: "composite", Path: ".spindle/actions/a",
			Origin: &threads.ActionOrigin{Repository: "acme/act", Ref: "v1", Commit: "c"},
			Steps:  []threads.StepSpec{{Run: "echo ${{ github.action_path }} ${{ github.action_ref }}"}},
		},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	var composite *StepPlan
	for i := range p.Steps {
		if p.Steps[i].Composite != nil {
			composite = &p.Steps[i]
			break
		}
	}
	if composite == nil || len(composite.Composite.Steps) == 0 {
		t.Fatalf("no composite plan: %+v", p.Steps)
	}
	inner := composite.Composite.Steps[0]
	if got := inner.Run; !strings.Contains(got, ".spindle/actions/a") || !strings.Contains(got, "v1") {
		t.Fatalf("composite run = %q, want action_path and action_ref resolved", got)
	}
	// A local composite action has no source ref: the documented answer is the
	// empty string, not a missing value.
	job2 := fixtureJob()
	job2.Spec.Steps = []threads.StepSpec{{Uses: "./.spindle/actions/local"}}
	job2.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/local": {Using: "composite", Path: ".spindle/actions/local",
			Steps: []threads.StepSpec{{Run: "echo [${{ github.action_ref }}]"}}},
	}
	p2, perr := BuildPlan(PlanInput{Job: job2, AttemptToken: "t"})
	if perr != nil {
		t.Fatalf("local composite BuildPlan: %v", perr)
	}
	got := p2.Steps[0].Composite.Steps[0].Run
	if got != "echo []" {
		t.Fatalf("local action_ref = %q, want the empty documented value", got)
	}
}
