package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Local composite actions and docker:// actions at the runner boundary
// (docs/threads.md §3.4a): expansion order, INPUT_* environment, nested
// composites, outputs, `if:`, the depth/cycle refusals (a hostile payload must
// not loop the runner), the docker argv (same workspace, same network, no host
// socket / privileged / capabilities / secrets), and the runner's own image
// policy on action images.

// actionJob is a job whose spec carries the embedded action definitions.
func actionJob() *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "setup", Name: "toolchain", Uses: "./.spindle/actions/setup",
			With: map[string]string{"version": "2.0"}},
		{Name: "use", Run: `use "${{ steps.setup.outputs.bin }}"`, Shell: "sh",
			If: "steps.setup.outcome == 'success'"},
	}
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/setup": {
			Path: ".spindle/actions/setup", Using: "composite",
			Inputs: map[string]threads.ActionInputSpec{
				"version": {Default: "1.0", HasDefault: true},
				"dry run": {},
			},
			Outputs: map[string]string{"bin": "${{ steps.install.outputs.bin }}"},
			Steps: []threads.StepSpec{
				{ID: "install", Name: "install", Run: `install "$INPUT_VERSION" "$INPUT_DRY_RUN"`, Shell: "bash"},
				{Name: "nested", Uses: "./.spindle/actions/deep",
					With: map[string]string{"level": "trace"}},
			},
		},
		".spindle/actions/deep": {
			Path: ".spindle/actions/deep", Using: "composite",
			Inputs: map[string]threads.ActionInputSpec{"level": {Default: "warn", HasDefault: true}},
			Steps: []threads.StepSpec{
				{ID: "deeprun", Run: `echo "$INPUT_LEVEL"`, Shell: "sh",
					Env: map[string]string{"EXTRA": "1"}},
				{Name: "image", Uses: "docker://alpine:3.20",
					With: map[string]string{"args": "--check"}},
			},
		},
	}
	return job
}

func TestBuildPlanExpandsCompositeAction(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: actionJob(), AttemptToken: "wjt_t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("top-level steps = %d, want 2 (the composite is one step)", len(p.Steps))
	}
	caller := p.Steps[0]
	if caller.Kind != "composite" || caller.ID != "setup" || caller.Composite == nil {
		t.Fatalf("caller = %+v", caller)
	}
	if caller.Composite.Path != ".spindle/actions/setup" || caller.Name != "toolchain" {
		t.Fatalf("caller = %+v", caller)
	}
	if got := len(caller.Composite.Steps); got != 2 {
		t.Fatalf("expanded steps = %d: %+v", got, caller.Composite.Steps)
	}
	install := caller.Composite.Steps[0]
	if install.Kind != "run" || install.LocalID != "install" || install.Parent != "toolchain" {
		t.Fatalf("install = %+v", install)
	}
	argv := strings.Join(install.Argv, " ")
	for _, want := range []string{
		"exec --workdir " + DefaultWorkspace,
		"--env INPUT_VERSION=2.0", // with: overrides the default
		"--env INPUT_DRY_RUN=",    // a declared input with no default is present and empty
		"--env SPINDLE_JOB_TOKEN", // a composite run step is repository code: token like any script
		"bash --noprofile --norc -eo pipefail -c",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("install argv missing %q\nargv: %s", want, argv)
		}
	}
	nested := caller.Composite.Steps[1]
	if nested.Kind != "composite" || nested.Parent != "toolchain" || nested.Composite == nil {
		t.Fatalf("nested = %+v", nested)
	}
	deepRun := nested.Composite.Steps[0]
	if deepRun.Parent != "toolchain / nested" || deepRun.LocalID != "deeprun" {
		t.Fatalf("deep run = %+v", deepRun)
	}
	if got := strings.Join(deepRun.Argv, " "); !strings.Contains(got, "--env INPUT_LEVEL=trace") || !strings.Contains(got, "--env EXTRA=1") {
		t.Fatalf("deep run argv = %q", got)
	}
	image := nested.Composite.Steps[1]
	if image.Kind != "docker" || image.Docker == nil || image.Docker.Image != "alpine:3.20" {
		t.Fatalf("image step = %+v", image)
	}
	if got := strings.Join(image.Argv, " "); !strings.HasSuffix(got, " alpine:3.20 --check") || strings.Contains(got, "INPUT_ARGS") {
		// PAR-072: `with.args` overrides a docker:// image's CMD (it is not an
		// input).
		t.Fatalf("docker image argv = %q, want the image followed by args: --check (and no INPUT_ARGS)", got)
	}
	// Unique flat output files: inner steps share the caller's display index.
	seen := map[string]bool{}
	var walk func([]StepPlan)
	walk = func(steps []StepPlan) {
		for _, st := range steps {
			if st.OutputPath != "" {
				if seen[st.OutputPath] {
					t.Fatalf("duplicate output path %q", st.OutputPath)
				}
				seen[st.OutputPath] = true
			}
			if st.Composite != nil {
				walk(st.Composite.Steps)
			}
		}
	}
	walk(p.Steps)
}

func TestBuildPlanDockerActionArgv(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: actionJob(), AttemptToken: "wjt_token_value"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	image := p.Steps[0].Composite.Steps[1].Composite.Steps[1]
	argv := image.Argv
	if len(argv) == 0 || argv[0] != "run" {
		t.Fatalf("argv = %v", argv)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--name " + dockerActionName(p, image.OutputIndex),
		"--network " + p.Names.Network,
		"--mount type=volume,source=" + p.Names.Volume + ",target=" + p.Workspace,
		"--workdir " + p.Workspace,
		"--pull=missing",
		"--add-host host.docker.internal:host-gateway",
		"alpine:3.20",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker action argv missing %q\nargv: %s", want, joined)
		}
	}
	// The job container is never used as a trampoline, and the action image is
	// never forced through a shell.
	if strings.Contains(joined, " exec ") || strings.Contains(joined, " -c ") {
		t.Fatalf("a docker action must run its own entrypoint: %s", joined)
	}
	// No host socket, no privilege, no capabilities, no user mounts.
	for _, forbidden := range []string{"docker.sock", "--privileged", "--cap-add", "--device", "--volume "} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("docker action argv carries %q: %s", forbidden, joined)
		}
	}
	// Neither the job token nor any secret is in the argv or in the docker CLI's
	// environment for this step (the action image is third-party code).
	for _, secret := range []string{"SPINDLE_JOB_TOKEN", "s3cr3t-value", "--env TOKEN"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("docker action argv leaks %q: %s", secret, joined)
		}
	}
	if len(image.EnvFrom) != 0 {
		t.Fatalf("a docker action must not take env by reference: %v", image.EnvFrom)
	}
}

func TestBuildPlanActionMetadataEntrypoint(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "lint", Uses: "./.spindle/actions/lint"}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/lint": {
			Path: ".spindle/actions/lint", Using: "docker",
			Image: "ghcr.io/acme/lint:1.0", Entrypoint: []string{"/lint", "--json"},
			Args: []string{"--strict"}, Env: map[string]string{"LINT_MODE": "ci"},
			Inputs: map[string]threads.ActionInputSpec{"level": {Default: "warn", HasDefault: true}},
		},
	}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	joined := strings.Join(p.Steps[0].Argv, " ")
	for _, want := range []string{
		"--entrypoint /lint", "--env LINT_MODE=ci", "--env INPUT_LEVEL=warn",
		"ghcr.io/acme/lint:1.0 --json --strict",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("metadata argv missing %q\nargv: %s", want, joined)
		}
	}
}

func TestBuildPlanInlineDockerUsesInputs(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{ID: "img", Name: "img", Uses: "docker://alpine:3.20", With: map[string]string{"who": "world"}}}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Kind != "docker" || p.Steps[0].Docker == nil || p.Steps[0].ID != "img" {
		t.Fatalf("step = %+v", p.Steps[0])
	}
	joined := strings.Join(p.Steps[0].Argv, " ")
	if !strings.Contains(joined, "--env INPUT_WHO=world") || !strings.Contains(joined, "alpine:3.20") {
		t.Fatalf("argv = %s", joined)
	}
	if !strings.Contains(joined, "--network "+p.Names.Network) {
		t.Fatalf("the job network is the action's network policy: %s", joined)
	}
}

// TestBuildPlanRefusesBadActionPayload pins the runner's defensive refusals: it
// never trusts the payload's action map, and it never loops.
func TestBuildPlanRefusesBadActionPayload(t *testing.T) {
	// Missing definition (a stale/hostile payload).
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./.spindle/actions/gone"}}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "not in the job payload") {
		t.Fatalf("missing definition = %v", err)
	}
	// Cycle.
	job = fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./a"}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		"a": {Path: "a", Using: "composite", Steps: []threads.StepSpec{{Uses: "./b"}}},
		"b": {Path: "b", Using: "composite", Steps: []threads.StepSpec{{Uses: "./a"}}},
	}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle = %v", err)
	}
	// Depth: 12 nested composites exceed the limit of 10.
	job = fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./a0"}}
	job.Spec.Actions = map[string]threads.ActionSpec{}
	for i := 0; i <= 11; i++ {
		spec := threads.ActionSpec{Path: fmt.Sprintf("a%d", i), Using: "composite"}
		if i < 11 {
			spec.Steps = []threads.StepSpec{{Uses: fmt.Sprintf("./a%d", i+1)}}
		} else {
			spec.Steps = []threads.StepSpec{{Run: "echo bottom", Shell: "sh"}}
		}
		job.Spec.Actions[spec.Path] = spec
	}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "deeper than the limit") {
		t.Fatalf("depth = %v", err)
	}
	// An undeclared input.
	job = fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./a", With: map[string]string{"typo": "x"}}}
	job.Spec.Actions = map[string]threads.ActionSpec{
		"a": {Path: "a", Using: "composite", Inputs: map[string]threads.ActionInputSpec{"real": {}},
			Steps: []threads.StepSpec{{Run: "echo hi", Shell: "sh"}}},
	}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), `"typo" is not declared`) {
		t.Fatalf("with key = %v", err)
	}
}

// TestBuildPlanActionImagePolicy pins the runner's half of the image gate: the
// same --image-prefix rewrite and --image-require-digest rule as job images,
// decided at plan time (before any network, volume or container exists).
func TestBuildPlanActionImagePolicy(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Container = &threads.ContainerSpec{Image: "golang@sha256:" + strings.Repeat("a", 64)}
	job.Spec.Steps = []threads.StepSpec{{Uses: "docker://ghcr.io/acme/lint:1.0"}}
	_, err := BuildPlan(PlanInput{Job: job, Images: ImageConfig{Prefix: "ak.example/mirror", PrefixAll: true}})
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	p, _ := BuildPlan(PlanInput{Job: job, Images: ImageConfig{Prefix: "ak.example/mirror", PrefixAll: true}})
	if got := p.Steps[0].Docker.Image; got != "ak.example/mirror/acme/lint:1.0" {
		t.Fatalf("rewritten image = %q", got)
	}
	_, err = BuildPlan(PlanInput{Job: job, Images: ImageConfig{RequireDigest: true}})
	if err == nil || !strings.Contains(err.Error(), "not digest-pinned") || !strings.Contains(err.Error(), "ghcr.io/acme/lint:1.0") {
		t.Fatalf("digest = %v", err)
	}
}

// TestExecuteCompositeAction checks the whole path against the fake docker CLI:
// expansion order, INPUT_* env, the action's outputs published under the
// caller's id, and a later step reading them.
func TestExecuteCompositeAction(t *testing.T) {
	d := &fakeDocker{
		streamOutput: "ok\n",
		runOutputs: []runOutput{
			// The action's install step publishes `bin`; the caller's output
			// mapping republishes it as steps.setup.outputs.bin.
			{match: "spindle-output-1", out: "bin=/opt/bin\n"},
		},
	}
	job := actionJob()
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	// One caller result + three inner results for the composite, then the
	// later top-level step.
	// The caller, its two children (install, the nested action), the nested
	// action's two children, then the later top-level step.
	if len(res.Steps) != 6 {
		t.Fatalf("results = %+v", res.Steps)
	}
	if res.Steps[0].Name != "toolchain" || res.Steps[0].Conclusion != threads.JobSuccess {
		t.Fatalf("caller result = %+v", res.Steps[0])
	}
	if res.Steps[1].Name != "install" || res.Steps[1].Conclusion != threads.JobSuccess {
		t.Fatalf("inner result = %+v", res.Steps[1])
	}
	if res.Steps[2].Name != "nested" || res.Steps[2].Conclusion != threads.JobSuccess {
		t.Fatalf("nested result = %+v", res.Steps[2])
	}
	// The later step's argv carries the caller's output value.
	all := strings.Join(d.snapshot(), "\n")
	if !strings.Contains(all, `use "/opt/bin"`) {
		t.Fatalf("the caller's output did not reach the next step: %s", all)
	}
	// Inner steps ran in order, then the docker action, then the later step.
	install := strings.Index(strings.Join(d.snapshot(), "\n"), `install "$INPUT_VERSION"`)
	deep := strings.Index(strings.Join(d.snapshot(), "\n"), `echo "$INPUT_LEVEL"`)
	action := strings.Index(strings.Join(d.snapshot(), "\n"), "run --name sp-a-")
	if !(install >= 0 && deep > install && action > deep) {
		t.Fatalf("execution order = install %d, deep %d, docker %d", install, deep, action)
	}
}

// TestExecuteTopLevelDockerOutputs pins an inline docker:// step's outputs:
// copied out of its container and published under its own id (the docker
// action's output file entries are its outputs; there is no metadata mapping).
func TestExecuteTopLevelDockerOutputs(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Steps = []threads.StepSpec{
		{ID: "scan", Name: "scan", Uses: "docker://alpine:3.20"},
		{Name: "after", Run: `echo "${{ steps.scan.outputs.report }}"`, Shell: "sh"},
	}
	d := &fakeDocker{streamOutput: "ok\n", cpFiles: map[string]string{"spindle-output-1": "report=clean\n"}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !strings.Contains(strings.Join(d.snapshot(), "\n"), `echo "clean"`) {
		t.Fatalf("the docker action's output did not reach the next step: %v", d.snapshot())
	}
}

// TestExecuteCompositeDockerOutputs pins a docker action's own outputs: copied
// out of its container, published under the caller's id, readable by the caller
// and by a later step.
func TestExecuteCompositeDockerOutputs(t *testing.T) {
	d := &fakeDocker{streamOutput: "ok\n", cpFiles: map[string]string{"spindle-output-1": "report=clean\n"}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/img": {
			Path: ".spindle/actions/img", Using: "composite",
			Outputs: map[string]string{"report": "${{ steps.scan.outputs.report }}"},
			Steps: []threads.StepSpec{
				{ID: "scan", Uses: "docker://alpine:3.20"},
			},
		},
	}
	job.Spec.Steps = []threads.StepSpec{
		{ID: "img", Name: "img", Uses: "./.spindle/actions/img"},
		{Name: "after", Run: `echo "${{ steps.img.outputs.report }}"`, Shell: "sh"},
	}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	all := strings.Join(d.snapshot(), "\n")
	if !strings.Contains(all, `echo "clean"`) {
		t.Fatalf("the docker action's output did not reach the next step: %s", all)
	}
}

// TestExecuteCompositeIfAndFailure pins the inner `if:` (skipped, not failed)
// and the failure rule: an inner failure fails the caller, skips the rest of
// the action, and (without continue-on-error) fails the job.
func TestExecuteCompositeIfAndFailure(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/a": {
			Path: ".spindle/actions/a", Using: "composite",
			Steps: []threads.StepSpec{
				{Name: "skipped", Run: "echo no", Shell: "sh", If: "github.ref == 'refs/heads/nope'"},
				{Name: "boom", Run: "exit 7", Shell: "sh"},
				{Name: "never", Run: "echo never", Shell: "sh"},
			},
		},
	}
	job.Spec.Steps = []threads.StepSpec{
		{ID: "a", Name: "a", Uses: "./.spindle/actions/a"},
		{Name: "after", Run: "echo after", Shell: "sh"},
	}
	d := &fakeDocker{streamByMatch: []runOutput{{match: "exit 7", code: 7}}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	// Caller, skipped, boom, never(skipped), then the top-level skip.
	if len(res.Steps) != 5 {
		t.Fatalf("results = %+v", res.Steps)
	}
	if res.Steps[1].Conclusion != "skipped" || res.Steps[2].Conclusion != threads.JobFailure ||
		res.Steps[3].Conclusion != "skipped" || res.Steps[4].Conclusion != "skipped" {
		t.Fatalf("results = %+v", res.Steps)
	}
	if strings.Contains(strings.Join(d.snapshot(), "\n"), "echo after") {
		t.Fatal("a hard failure must skip the remaining job steps")
	}
}

// TestExecuteCompositeContinueOnError pins the caller's tolerance: the job stays
// green, the caller result records the failure.
func TestExecuteCompositeContinueOnError(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{
		".spindle/actions/a": {Path: ".spindle/actions/a", Using: "composite",
			Steps: []threads.StepSpec{{Run: "exit 3", Shell: "sh"}}},
	}
	job.Spec.Steps = []threads.StepSpec{
		{ID: "a", Name: "a", Uses: "./.spindle/actions/a", ContinueOnError: true},
		{Name: "after", Run: "echo ok", Shell: "sh"},
	}
	d := &fakeDocker{streamByMatch: []runOutput{{match: "exit 3", code: 3}}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobFailure {
		t.Fatalf("the tolerated failure must stay recorded: %+v", res.Steps[0])
	}
}

// TestExecuteForkDockerActionGetsNoSecrets asserts the fork invariant at the
// runner boundary: a fork payload carries no secrets (SecretsDenied, empty
// map), and a docker action must not receive the job token even on a trusted
// run.
func TestExecuteForkDockerActionGetsNoSecrets(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Trusted = false
	job.Secrets = map[string]string{}
	job.SecretsDenied = true
	job.Spec.Steps = []threads.StepSpec{{Name: "img", Uses: "docker://alpine:3.20"}}
	d := &fakeDocker{streamOutput: "ok\n"}
	stream := quietStream(t)
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	for i, call := range d.snapshot() {
		if !strings.Contains(call, "run --name sp-a-") {
			continue
		}
		if len(d.envs[i]) != 0 {
			t.Fatalf("a docker action's docker CLI call carries an environment: %v", d.envs[i])
		}
		for _, forbidden := range []string{"wjt_token_value", "SPINDLE_JOB_TOKEN", "s3cr3t"} {
			if strings.Contains(call, forbidden) {
				t.Fatalf("docker action argv leaks %q: %s", forbidden, call)
			}
		}
	}
}

// TestBuildPlanRequiresActionDefinitionsForUsesOutsideAComposite keeps the
// marketplace refusal intact (out of scope, stated in the docs).
func TestBuildPlanRemoteActionStaysRefused(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "owner/repo@v1"}}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "marketplace") {
		t.Fatalf("remote action = %+v", p.Steps[0])
	}
}

// TestBuildPlanLocalActionPathEscape refuses a host path out of the repository.
func TestBuildPlanLocalActionPathEscape(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Uses: "./../../etc"}}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "escapes the repository") {
		t.Fatalf("escape = %v", err)
	}
}
