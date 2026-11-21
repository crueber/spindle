package runner

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// fixtureJob is one fetch payload with a job container, one service, a matrix
// cell, env, secrets and the step kinds the executor supports.
func fixtureJob() *threads.FetchResult {
	return &threads.FetchResult{
		Repo: "acme/mono", RunID: "1732104000000000001-a1b2c3", JobID: "j-build",
		JobRef: "jr_acme", Attempt: 2, Commit: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Event: "push", Branch: "main", Workflow: "CI", Trusted: true,
		Context:  "threads/ci/build",
		Checkout: &threads.CheckoutInfo{URL: "http://127.0.0.1:8088/acme/mono.git", ReadOnly: true},
		Secrets:  map[string]string{"TOKEN": "s3cr3t-value", "SHORT": "abc"},
		Mask:     []string{"s3cr3t-value"},
		Spec: &threads.Spec{
			Version:  threads.SpecVersion,
			Workflow: "CI",
			RunsOn:   []string{"linux"},
			Container: &threads.ContainerSpec{
				Image:   "golang:1.27",
				Env:     map[string]string{"CGO_ENABLED": "0"},
				Options: "--user 1000",
				Volumes: []string{"cache:/go/pkg/mod"},
			},
			Services: map[string]threads.ServiceSpec{
				"redis": {Image: "redis:7", Ports: []string{"6379:6379"}, Env: map[string]string{"A": "b"}},
			},
			Env:    map[string]string{"GOFLAGS": "-mod=readonly"},
			Matrix: map[string]string{"go": "1.27"},
			Steps: []threads.StepSpec{
				{Name: "checkout", Uses: "actions/checkout", With: map[string]string{"fetch-depth": "0"}},
				{Name: "test ${{ matrix.go }}", Run: "go test ./...", Shell: "bash", WorkingDirectory: "./sub",
					Env: map[string]string{"STEP": "1"}, ContinueOnError: true, TimeoutMinutes: 3},
				{Name: "conditional", Run: "echo hi", If: "github.ref == 'refs/heads/main'"},
			},
			TimeoutMinutes: 15,
		},
	}
}

func TestBuildPlanContainerAndNames(t *testing.T) {
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !strings.HasPrefix(p.Names.Container, "sp-j-") || !strings.HasPrefix(p.Names.Network, "sp-n-") || !strings.HasPrefix(p.Names.Volume, "sp-v-") {
		t.Fatalf("names = %+v", p.Names)
	}
	if p.Names.Services["redis"] == "" {
		t.Fatalf("service name = %+v", p.Names)
	}
	// A different attempt must get different docker objects (cleanup isolation).
	other := fixtureJob()
	other.Attempt = 3
	p2, _ := BuildPlan(PlanInput{Job: other})
	if p2.Names.Container == p.Names.Container {
		t.Fatal("attempt 3 reused attempt 2's container name")
	}
	if p.Names.Container == "" || p.Image != "golang:1.27" || p.Timeout == 0 {
		t.Fatalf("plan = %+v", p)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	for _, want := range []string{
		"create --name " + p.Names.Container,
		"--network " + p.Names.Network,
		"--network-alias job",
		"--add-host host.docker.internal:host-gateway",
		"--workdir " + DefaultWorkspace,
		"--mount type=volume,source=" + p.Names.Volume + ",target=" + DefaultWorkspace,
		"--user 1000",
		"--volume cache:/go/pkg/mod",
		"--env CGO_ENABLED=0",
		"--env GITHUB_SHA=" + job.Commit,
		"--env MATRIX_GO=1.27",
		"--env SPINDLE_TRUSTED=true",
		"--entrypoint /bin/sh",
		"golang:1.27 -c trap 'exit 0' TERM INT; while :; do sleep 3600; done",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("container argv missing %q\nargv: %s", want, argv)
		}
	}
	if strings.Contains(argv, "s3cr3t-value") {
		t.Fatal("secret value leaked into the container create argv")
	}
	// The loopback checkout URL is rewritten to the container gateway.
	if p.CheckoutURL != "http://host.docker.internal:8088/acme/mono.git" {
		t.Fatalf("checkout url = %q", p.CheckoutURL)
	}
}

func TestBuildPlanServices(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("services = %+v", p.Services)
	}
	svc := p.Services[0]
	argv := strings.Join(svc.Argv, " ")
	for _, want := range []string{
		"create --name " + svc.Name,
		"--network " + p.Names.Network,
		"--network-alias redis",
		"--publish 127.0.0.1::6379",
		"--env A=b",
		"redis:7",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("service argv missing %q\nargv: %s", want, argv)
		}
	}
	if len(svc.ProbePorts) != 1 || svc.ProbePorts[0] != "6379" {
		t.Fatalf("probe ports = %v", svc.ProbePorts)
	}
}

func TestBuildPlanSteps(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 3 {
		t.Fatalf("steps = %+v", p.Steps)
	}
	checkout := p.Steps[0]
	if checkout.Kind != "checkout" || !strings.Contains(checkout.Run, " fetch -q ") ||
		!strings.Contains(checkout.Run, "credential.helper") || !strings.Contains(checkout.Run, "FETCH_HEAD") {
		t.Fatalf("checkout step = %+v", checkout)
	}
	if strings.Contains(strings.Join(checkout.Argv, " "), fixtureJob().Commit) == false {
		t.Fatalf("checkout argv lacks the commit: %v", checkout.Argv)
	}
	if !strings.Contains(checkout.Run, "origin 'cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'") {
		t.Fatalf("checkout script lacks the exact sha: %s", checkout.Run)
	}
	// fetch-depth 0 drops --depth.
	if strings.Contains(checkout.Run, "--depth") {
		t.Fatalf("fetch-depth: 0 must not pass --depth: %s", checkout.Run)
	}
	if !containsString(checkout.EnvFrom, "SPINDLE_JOB_TOKEN") {
		t.Fatalf("checkout must take the job token by env NAME: %v", checkout.EnvFrom)
	}

	step := p.Steps[1]
	if step.Kind != "run" || step.Name != "test 1.27" {
		t.Fatalf("matrix name substitution = %+v", step)
	}
	if step.WorkingDirectory != DefaultWorkspace+"/sub" {
		t.Fatalf("workdir = %q", step.WorkingDirectory)
	}
	if !step.ContinueOnError || step.Timeout == 0 {
		t.Fatalf("step flags = %+v", step)
	}
	argv := strings.Join(step.Argv, " ")
	for _, want := range []string{
		"exec --workdir " + DefaultWorkspace + "/sub",
		"--env GOFLAGS=-mod=readonly",
		"--env STEP=1",
		"--env TOKEN", // secret by NAME, never by value
		p.Names.Container,
		"bash -eo pipefail -c go test ./...",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("exec argv missing %q\nargv: %s", want, argv)
		}
	}
	if strings.Contains(argv, "s3cr3t-value") {
		t.Fatal("secret value leaked into the exec argv")
	}
	if p.Steps[2].If != "github.ref == 'refs/heads/main'" {
		t.Fatalf("if = %q", p.Steps[2].If)
	}
}

func TestBuildPlanUnsupportedUsesAndShell(t *testing.T) {
	job := fixtureJob()
	job.Spec.Steps = []threads.StepSpec{
		{Name: "market", Uses: "some/action@v1"},
		{Name: "weird shell", Run: "true", Shell: "fish"},
	}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, "no marketplace") {
		t.Fatalf("unsupported uses = %+v", p.Steps[0])
	}
	if p.Steps[1].Kind != "unsupported" || !strings.Contains(p.Steps[1].SkipReason, "unsupported shell") {
		t.Fatalf("unsupported shell = %+v", p.Steps[1])
	}
}

func TestBuildPlanRefusesUnknownSpecAndReusableWorkflow(t *testing.T) {
	job := fixtureJob()
	job.Spec.Version = 99
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "spec version") {
		t.Fatalf("spec version = %v", err)
	}
	job = fixtureJob()
	job.Spec.Uses = "./.spindle/workflows/x.yml"
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "reusable") {
		t.Fatalf("reusable = %v", err)
	}
}

func TestBuildPlanDefaultImageAndShells(t *testing.T) {
	job := fixtureJob()
	job.Spec.Container = nil
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != DefaultImage {
		t.Fatalf("default image = %q", p.Image)
	}
	cases := map[string]string{
		"":         "sh -e -c",
		"sh":       "sh -e -c",
		"bash":     "bash -eo pipefail -c",
		"bash {0}": "bash -eo pipefail -c",
		"python":   "python3 -c",
		"pwsh":     "pwsh -command",
		"cmd":      "cmd /c",
	}
	for shell, want := range cases {
		argv, err := shellCommand(shell, "echo x")
		if err != nil {
			t.Fatalf("shell %q: %v", shell, err)
		}
		if got := strings.Join(argv, " "); !strings.HasPrefix(got, want) {
			t.Errorf("shell %q = %q, want prefix %q", shell, got, want)
		}
	}
}

func TestContainerDirAndSplitOptions(t *testing.T) {
	cases := map[string]string{
		"":        DefaultWorkspace,
		".":       DefaultWorkspace,
		"./src":   DefaultWorkspace + "/src",
		"/tmp/x/": "/tmp/x",
		"a/b":     DefaultWorkspace + "/a/b",
	}
	for in, want := range cases {
		if got := containerDir(DefaultWorkspace, in); got != want {
			t.Errorf("containerDir(%q) = %q, want %q", in, got, want)
		}
	}
	opts := splitOptions(`--user 1000 --memory "512m" --label 'a b'`)
	if strings.Join(opts, "|") != `--user|1000|--memory|512m|--label|a b` {
		t.Fatalf("splitOptions = %v", opts)
	}
}

func TestRewriteForContainer(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080/o/r.git":   "http://host.docker.internal:8080/o/r.git",
		"http://localhost:1/o/r.git":      "http://host.docker.internal:1/o/r.git",
		"https://git.example.com/o/r.git": "https://git.example.com/o/r.git",
	}
	for in, want := range cases {
		if got := rewriteForContainer(in); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSubstituteMatrixSharedRule(t *testing.T) {
	vals := map[string]string{"go": "1.27"}
	for in, want := range map[string]string{
		"go ${{ matrix.go }} x": "go 1.27 x",
		"${{matrix.go}}":        "1.27",
		"${{ matrix.nope }}":    "${{ matrix.nope }}",
	} {
		if got := threads.SubstituteMatrix(in, vals); got != want {
			t.Errorf("SubstituteMatrix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSubstituteInputsSharedRule pins the workflow_dispatch input spelling
// (15 §1): `${{ inputs.<name> }}` in four whitespace forms, unknown left alone,
// and input names are case-sensitive.
func TestSubstituteInputsSharedRule(t *testing.T) {
	vals := map[string]string{"env": "staging", "dry_run": "true"}
	for in, want := range map[string]string{
		"deploy to ${{ inputs.env }}": "deploy to staging",
		"${{inputs.env}}":             "staging",
		"${{ inputs.env}}":            "staging",
		"${{inputs.env }}":            "staging",
		"${{ inputs.Env }}":           "${{ inputs.Env }}",
		"${{ inputs.nope }}":          "${{ inputs.nope }}",
	} {
		if got := threads.SubstituteInputs(in, vals); got != want {
			t.Errorf("SubstituteInputs(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildPlanSubstitutesInputs pins that dispatch input values reach the
// step argv and the `if` context (the "inputs.* where the spec already supports
// it" requirement), next to matrix substitution.
func TestBuildPlanSubstitutesInputs(t *testing.T) {
	job := fixtureJob()
	job.Spec.Inputs = map[string]string{"env": "staging", "dry_run": "true"}
	job.Spec.Steps = []threads.StepSpec{
		{Name: "deploy ${{ inputs.env }}", Run: "deploy --env ${{ inputs.env }}"},
		{Name: "guarded", Run: "echo guarded", If: "inputs.dry_run == 'true'"},
	}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Inputs["env"] != "staging" {
		t.Fatalf("plan inputs = %v", p.Inputs)
	}
	if p.Steps[0].Name != "deploy staging" || !strings.Contains(strings.Join(p.Steps[0].Argv, " "), "deploy --env staging") {
		t.Fatalf("step 0 = %+v", p.Steps[0])
	}
	if p.Steps[1].If != "inputs.dry_run == 'true'" {
		t.Fatalf("step 1 if = %q", p.Steps[1].If)
	}
	ok, err := EvalIf(p.Steps[1].If, ExprContext{Success: true, Inputs: p.Inputs})
	if err != nil || !ok {
		t.Fatalf("if with inputs = %v, %v", ok, err)
	}
	if ok, _ := EvalIf("inputs.dry_run == 'true'", ExprContext{Success: true, Inputs: map[string]string{"dry_run": "false"}}); ok {
		t.Fatal("a false input value satisfied the condition")
	}
}
