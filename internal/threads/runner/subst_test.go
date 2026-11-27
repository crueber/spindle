package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// substJob is the fetch payload the substitution tests share: a named actor, a
// canonical origin, no services (the readiness path is not what is under test).
func substJob() *threads.FetchResult {
	job := fixtureJob()
	job.Actor = "crueber"
	job.ServerURL = "https://spindle.example"
	job.Spec.Services = nil
	return job
}

// TestSubstituteGitHubInEverySite pins the execution wave's value table
// (docs/threads.md §3.6): `${{ github.* }}` resolves in `run`, `env`, `with`,
// `name`, `shell` and `working-directory`.
func TestSubstituteGitHubInEverySite(t *testing.T) {
	job := substJob()
	job.Spec.Matrix = map[string]string{"go": "1.27", "shell": "bash"}
	job.Spec.Steps = []threads.StepSpec{
		{
			Name:             "run ${{ github.ref_name }}",
			Run:              "echo ${{ github.ref }} ${{ github.actor }} ${{ github.sha }}",
			Shell:            "${{ matrix.shell }}",
			WorkingDirectory: "./${{ github.ref_name }}",
			Env:              map[string]string{"REF": "${{ github.ref }}", "ACTOR": "${{ github.actor }}"},
		},
		{
			Name: "addresses",
			Run:  "echo ${{ github.server_url }} ${{ github.api_url }}",
		},
		{
			Name: "inputs",
			Uses: "docker://alpine:3",
			With: map[string]string{"tag": "${{ github.sha }}"},
		},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if step.Name != "run main" {
		t.Fatalf("name = %q", step.Name)
	}
	if step.Shell != "bash" {
		t.Fatalf("shell = %q", step.Shell)
	}
	if step.WorkingDirectory != DefaultWorkspace+"/main" {
		t.Fatalf("workdir = %q", step.WorkingDirectory)
	}
	argv := strings.Join(step.Argv, " ")
	for _, want := range []string{
		"bash --noprofile --norc -eo pipefail -c echo refs/heads/main crueber " + job.Commit,
		"--env REF=refs/heads/main",
		"--env ACTOR=crueber",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv misses %q\nargv: %s", want, argv)
		}
	}
	addresses := strings.Join(p.Steps[1].Argv, " ")
	if !strings.Contains(addresses, "echo https://spindle.example https://spindle.example/api/v1") {
		t.Fatalf("addresses = %s", addresses)
	}
	dockerArgv := strings.Join(p.Steps[2].Argv, " ")
	if !strings.Contains(dockerArgv, "--env INPUT_TAG="+job.Commit) {
		t.Fatalf("with value not substituted into the docker action's inputs: %s", dockerArgv)
	}
}

// TestSubstituteEnvLayers pins `${{ env.NAME }}` against the layered
// environment (workflow → job → container → step) with the step layer winning,
// and an `env:` value reading the layer below it.
func TestSubstituteEnvLayers(t *testing.T) {
	job := substJob()
	job.Spec.Env = map[string]string{"FOO": "job", "BAR": "jobbar"}
	job.Spec.Container.Env["FOO"] = "container"
	job.Spec.Steps = []threads.StepSpec{{
		Name: "env ${{ env.FOO }}",
		Run:  "echo ${{ env.FOO }} ${{ env.BAR }} ${{ env.ALIAS }}",
		Env:  map[string]string{"FOO": "step", "ALIAS": "${{ env.BAR }}"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if step.Name != "env step" {
		t.Fatalf("name = %q (the step layer must win)", step.Name)
	}
	argv := strings.Join(step.Argv, " ")
	for _, want := range []string{
		// The step's FOO wins; BAR only exists at the job layer; ALIAS resolved
		// against the layer below the step.
		"echo step jobbar jobbar",
		"--env FOO=step",
		"--env ALIAS=jobbar",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv misses %q\nargv: %s", want, argv)
		}
	}
}

// TestSubstituteMatrixAndInputs pins that the pre-existing sources still resolve
// next to the new contexts.
func TestSubstituteMatrixAndInputs(t *testing.T) {
	job := substJob()
	job.Spec.Matrix = map[string]string{"go": "1.27"}
	job.Spec.Inputs = map[string]string{"env": "staging"}
	job.Spec.Steps = []threads.StepSpec{{
		Name: "deploy ${{ inputs.env }}",
		Run:  "deploy --go ${{ matrix.go }} --env ${{ inputs.env }}",
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Steps[0].Name != "deploy staging" {
		t.Fatalf("name = %q", p.Steps[0].Name)
	}
	if argv := strings.Join(p.Steps[0].Argv, " "); !strings.Contains(argv, "deploy --go 1.27 --env staging") {
		t.Fatalf("argv = %s", argv)
	}
}

// TestSubstituteStepAndNeedsOutputs pins the strict output rule: a published
// output resolves, an undeclared one is a NAMED error (never an empty string).
func TestSubstituteStepAndNeedsOutputs(t *testing.T) {
	job := substJob()
	job.NeedsOutputs = map[string]map[string]string{"build": {"artifact": "dist.tgz"}}
	job.Spec.Steps = []threads.StepSpec{
		{ID: "a", Run: "echo b=one >> $GITHUB_OUTPUT"},
		{ID: "b", Run: "echo ${{ steps.a.outputs.b }} ${{ needs.build.outputs.artifact }}"},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		// The preview defers the output spans (no step has run yet).
		t.Fatalf("BuildPlan must not claim an output that has not been published: %v", err)
	}
	if !strings.Contains(p.Steps[1].Run, "${{ steps.a.outputs.b }}") {
		t.Fatalf("the plan preview resolved an unpublished output: %q", p.Steps[1].Run)
	}
	outputs := map[string]map[string]string{"a": {"b": "one"}}
	resolved, err := p.ResolveStep(p.Steps[1], outputs)
	if err != nil {
		t.Fatalf("ResolveStep: %v", err)
	}
	if argv := strings.Join(resolved.Argv, " "); !strings.Contains(argv, "echo one dist.tgz") {
		t.Fatalf("argv = %s", argv)
	}
	if _, err := p.ResolveStep(p.Steps[1], map[string]map[string]string{"a": {}}); err == nil ||
		!strings.Contains(err.Error(), `did not publish the output "b"`) {
		t.Fatalf("an unpublished output did not fail loudly: %v", err)
	}
	if _, err := p.ResolveStep(p.Steps[1], map[string]map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), `no step with id "a"`) {
		t.Fatalf("an unknown step id did not fail loudly: %v", err)
	}
	job.NeedsOutputs = map[string]map[string]string{"build": {}}
	p, err = BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := p.ResolveStep(p.Steps[1], outputs); err == nil ||
		!strings.Contains(err.Error(), `did not publish the output "artifact"`) {
		t.Fatalf("an unpublished needs output did not fail loudly: %v", err)
	}
}

// TestStepIfReadsActorAndSecret pins the step `if:` context (PAR-058's runner
// half): `github.actor` and `secrets.<name>` are readable there.
func TestStepIfReadsActorAndSecret(t *testing.T) {
	job := substJob()
	job.Spec.Steps = []threads.StepSpec{
		{Name: "actor", Run: "echo go", If: "github.actor == 'crueber'"},
		{Name: "secret", Run: "echo go", If: "secrets.TOKEN == 's3cr3t-value'"},
		{Name: "missing secret", Run: "echo go", If: "secrets.NOPE == ''"},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	ctx := (&Executor{}).exprContext(p, false, quietStream(t), nil, nil)
	for i, want := range []bool{true, true, true} {
		got, err := EvalIf(p.Steps[i].If, ctx)
		if err != nil {
			t.Fatalf("step %d if %q: %v", i, p.Steps[i].If, err)
		}
		if got != want {
			t.Fatalf("step %d if %q = %v, want %v", i, p.Steps[i].If, got, want)
		}
	}
	// The same context with a different actor must not satisfy the condition.
	other := substJob()
	other.Actor = "someone-else"
	otherPlan, err := BuildPlan(PlanInput{Job: other, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if ok, err := EvalIf("github.actor == 'crueber'", (&Executor{}).exprContext(otherPlan, false, quietStream(t), nil, nil)); err != nil || ok {
		t.Fatalf("a different actor satisfied the condition: %v, %v", ok, err)
	}
}

// TestSubstituteIsOnePass pins that a substituted VALUE is data: a secret whose
// body contains a reference is never rescanned.
func TestSubstituteIsOnePass(t *testing.T) {
	job := substJob()
	job.Secrets["TRICKY"] = "${{ github.ref }}"
	job.Mask = append(job.Mask, "${{ github.ref }}")
	job.Spec.Steps = []threads.StepSpec{{Name: "echo", Run: "echo ${{ secrets.TRICKY }}"}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	resolved, err := p.ResolveStep(p.Steps[0], nil)
	if err != nil {
		t.Fatalf("ResolveStep: %v", err)
	}
	argv := strings.Join(resolved.Argv, " ")
	if !strings.Contains(argv, "echo ${{ github.ref }}") {
		t.Fatalf("the substituted value was rescanned: %s", argv)
	}
	if resolved.Run != "echo ${{ github.ref }}" {
		t.Fatalf("a reference inside a secret value was expanded: %q", resolved.Run)
	}
}

// TestSubstituteInCompositeAction pins the substitution inside a local
// composite action: the caller's `with:`/`env:` reach the inner steps already
// substituted, an inner step's env reads the caller's layer, and a whole-value
// secret in the caller's env stays delivered by NAME.
func TestSubstituteInCompositeAction(t *testing.T) {
	job := substJob()
	job.Spec.Matrix = map[string]string{"go": "1.27"}
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/echo": {
			Path: ".spindle/actions/echo", Using: "composite",
			Inputs: map[string]threads.ActionInputSpec{"version": {HasDefault: true}},
			Steps: []threads.StepSpec{{
				ID: "inner", Run: `echo "$INPUT_VERSION" "${{ env.PLAIN }}"`, Shell: "sh",
				Env: map[string]string{"LOCAL": "${{ env.PLAIN }}"},
			}},
		},
	}
	job.Spec.Steps = []threads.StepSpec{{
		Name: "call", Uses: "./.spindle/actions/echo",
		With: map[string]string{"version": "${{ github.ref_name }}"},
		Env: map[string]string{
			"PASSED": "${{ secrets.TOKEN }}",
			"PLAIN":  "matrix-${{ matrix.go }}",
		},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	inner := p.Steps[0].Composite.Steps[0]
	argv := strings.Join(inner.Argv, " ")
	for _, want := range []string{
		`echo "$INPUT_VERSION" "matrix-1.27"`,
		"--env INPUT_VERSION=main",
		"--env LOCAL=matrix-1.27",
		"--env PASSED", // the whole-value secret: bare name, value from the client env
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("inner argv misses %q\nargv: %s", want, argv)
		}
	}
	if strings.Contains(argv, "s3cr3t-value") {
		t.Fatalf("the composite's secret reached the inner argv: %s", argv)
	}
	if got, ok := envOf(credentialEnv(p, inner.EnvFrom, inner.EnvSecret), "PASSED"); !ok || got != "s3cr3t-value" {
		t.Fatalf("PASSED = %q present=%v", got, ok)
	}
}

// TestSubstituteRejectsAMalformedSpan pins that the plan does not paper over a
// span the parser would have rejected: a malformed template fails the plan with
// a named error rather than reaching a shell as literal text.
func TestSubstituteRejectsAMalformedSpan(t *testing.T) {
	job := substJob()
	job.Spec.Steps = []threads.StepSpec{{Name: "bad", Run: "echo ${{ github.ref"}}
	if _, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"}); err == nil ||
		!strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("a malformed reference did not fail the plan: %v", err)
	}
}

// TestBuiltinEnvRefForTagBranchAndPR pins GITHUB_REF / GITHUB_REF_NAME in the
// job container env (docs/threads.md §3.5): a tag run has no branch, so the ref
// must come from the payload's full ref ("refs/tags/v1.2.3" / "v1.2.3"), while
// a pull request keeps its BASE branch and a legacy payload keeps deriving from
// Branch.
func TestBuiltinEnvRefForTagBranchAndPR(t *testing.T) {
	cases := []struct {
		name          string
		event, branch string
		ref           string
		wantRef       string
		wantRefName   string
	}{
		{"branch push", threads.EventPush, "main", "refs/heads/main", "refs/heads/main", "main"},
		{"tag push", threads.EventPush, "", "refs/tags/v1.2.3", "refs/tags/v1.2.3", "v1.2.3"},
		{"pull request keeps its base branch", threads.EventPullReq, "main", "refs/pull/42/head", "refs/heads/main", "main"},
		{"legacy payload without a ref", threads.EventPush, "dev", "", "refs/heads/dev", "dev"},
		{"no ref at all", threads.EventPush, "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := builtinEnv(&threads.FetchResult{Event: tc.event, Branch: tc.branch, Ref: tc.ref}, DefaultWorkspace, "", nil)
			if env["GITHUB_REF"] != tc.wantRef {
				t.Errorf("GITHUB_REF = %q, want %q", env["GITHUB_REF"], tc.wantRef)
			}
			if env["GITHUB_REF_NAME"] != tc.wantRefName {
				t.Errorf("GITHUB_REF_NAME = %q, want %q", env["GITHUB_REF_NAME"], tc.wantRefName)
			}
			if env["SPINDLE_REF"] != tc.wantRef {
				t.Errorf("SPINDLE_REF = %q, want %q", env["SPINDLE_REF"], tc.wantRef)
			}
		})
	}
}

// TestTagRunPlanCarriesTheTagRef is the tag-run shape end to end at the runner
// boundary: the container env and `${{ github.ref }}`/`${{ github.ref_name }}`
// all name the tag.
func TestTagRunPlanCarriesTheTagRef(t *testing.T) {
	job := substJob()
	job.Branch = ""
	job.Ref = "refs/tags/v1.2.3"
	job.Spec.Steps = []threads.StepSpec{{
		Name: "release ${{ github.ref_name }}",
		Run:  "echo ${{ github.ref }}",
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Env["GITHUB_REF"] != "refs/tags/v1.2.3" || p.Env["GITHUB_REF_NAME"] != "v1.2.3" {
		t.Fatalf("container env ref = %q / %q", p.Env["GITHUB_REF"], p.Env["GITHUB_REF_NAME"])
	}
	if p.GitHub["ref"] != "refs/tags/v1.2.3" || p.GitHub["ref_name"] != "v1.2.3" {
		t.Fatalf("github context = %v", p.GitHub)
	}
	if p.Steps[0].Name != "release v1.2.3" {
		t.Fatalf("name = %q", p.Steps[0].Name)
	}
	if argv := strings.Join(p.Steps[0].Argv, " "); !strings.Contains(argv, "echo refs/tags/v1.2.3") {
		t.Fatalf("argv = %s", argv)
	}
}

// TestSecretEnvAndVarsNamespacesDoNotConfuse pins PAR-116's namespace rule: a
// name that exists in `secrets`, `vars` AND the authored `env` reads from the
// context the reference names. In particular `${{ env.TOKEN }}` can never be a
// way to read a secret (the env context is the authored layer only, built
// before substitution), and `${{ vars.TOKEN }}` never reads a sealed secret.
func TestSecretEnvAndVarsNamespacesDoNotConfuse(t *testing.T) {
	job := substJob()
	job.Secrets = map[string]string{"TOKEN": "secret-value-1234"}
	job.Vars = map[string]string{"TOKEN": "vars-value-1234"}
	job.Spec.Env = map[string]string{"TOKEN": "env-value-1234"}
	job.Spec.Steps = []threads.StepSpec{{
		Name: "names", Run: "echo ${{ secrets.TOKEN }} ${{ vars.TOKEN }} ${{ env.TOKEN }}",
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.Steps[0].Argv, " ")
	if !strings.Contains(argv, "echo secret-value-1234 vars-value-1234 env-value-1234") {
		t.Fatalf("namespaces confused:\n%s", argv)
	}
	// A step `env:` that names a secret is delivered by NAME (never argv), and
	// the metric is the credential environment.
	job.Spec.Steps = []threads.StepSpec{{
		Name: "mapped", Run: "echo run", Env: map[string]string{"MAPPED": "${{ secrets.TOKEN }}"},
	}}
	p, err = BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	line := strings.Join(step.Argv, " ")
	if strings.Contains(line, "secret-value-1234") {
		t.Fatalf("a mapped secret reached argv: %s", line)
	}
	if got, ok := envOf(credentialEnv(p, step.EnvFrom, step.EnvSecret), "MAPPED"); !ok || got != "secret-value-1234" {
		t.Fatalf("MAPPED = %q present=%v", got, ok)
	}
	// An untrusted plan's secret context is empty: every reference reads the
	// empty string, never the literal text and never a value.
	job.Trusted = false
	job.Secrets = map[string]string{}
	job.Spec.Steps = []threads.StepSpec{{Name: "empty", Run: "echo [${{ secrets.TOKEN }}]"}}
	p, err = BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := strings.Join(p.Steps[0].Argv, " "); !strings.Contains(got, "echo []") {
		t.Fatalf("untrusted secret reference = %s", got)
	}
}

// TestSubstitutedSecretIsMaskedInTheLog pins masking: a `${{ secrets.X }}`
// spliced into `run:` reaches argv (as on GitHub) and the log stream masks it —
// even though the streamer was built without the payload's mask list, because
// the plan registers every secret it can substitute.
func TestSubstitutedSecretIsMaskedInTheLog(t *testing.T) {
	docker := &fakeDocker{streamOutput: "printed s3cr3t-value here\n"}
	job := substJob()
	job.Mask = nil // the plan must register the value itself
	job.Spec.Steps = []threads.StepSpec{{Name: "leak", Run: "echo ${{ secrets.TOKEN }}"}}
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_x"}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !docker.called("echo s3cr3t-value") {
		t.Fatalf("the secret was not substituted into the script: %v", docker.snapshot())
	}
	got := frames()
	if strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("the substituted secret reached the log: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("the log did not mask the secret: %s", got)
	}
}
