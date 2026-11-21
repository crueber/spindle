package threads

import (
	"encoding/json"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Pass A deliverable 3 (docs/features/15_threads.md §2): the job record carries
// a versioned Spec with everything a runner needs — services, job
// env/if/timeout/continue-on-error, and every step field — so the runner never
// reads the workflow file.

const specWorkflow = `name: Spec CI
on: [push, pull_request]
env:
  GLOBAL: "1"
permissions:
  contents: read
concurrency: ci-main
jobs:
  build:
    name: Build
    runs-on: [linux, docker]
    container:
      image: golang:1.27
      env: {CGO_ENABLED: "0"}
      ports: ["8080:8080"]
      command: ["sleep", "infinity"]
      entrypoint: ["/bin/sh"]
      options: "--user 1000"
      volumes: ["cache:/go/pkg/mod"]
    services:
      redis:
        image: redis:7
        env: {REDIS_MAXMEMORY: 64mb}
        ports: ["6379:6379"]
        command: ["redis-server", "--save", ""]
        entrypoint: ["redis-server"]
        options: "--health-cmd ping"
        volumes: ["redis:/data"]
    env:
      JOB: "yes"
      GLOBAL: "2"
    if: github.ref == 'refs/heads/main'
    timeout-minutes: 15
    continue-on-error: true
    permissions: {contents: write}
    strategy:
      fail-fast: false
      matrix:
        go: ["1.26", "1.27"]
    concurrency: {group: "build-${{ matrix.go }}", cancel-in-progress: true}
    steps:
      - name: checkout
        uses: actions/checkout
        with: {fetch-depth: "0"}
        env: {A: b}
        if: always()
        shell: bash
        working-directory: ./sub
      - run: go build ./...
`

func TestJobSpecCarriesExecutionPayload(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/spec.yml", specWorkflow)
	run := e.mustCreate(t)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(view.Jobs) != 2 {
		t.Fatalf("jobs = %d (matrix go × 2)", len(view.Jobs))
	}
	// The matrix expands into one job per combination (pass B2), sorted by axis
	// value: "1.26" then "1.27"; each carries its own cell and concurrency group.
	job := view.Jobs[0]
	if job.Name != "Build (1.26)" || job.Spec.Matrix["go"] != "1.26" || job.Spec.Concurrency.Group != "build-1.26" {
		t.Fatalf("first cell = name %q matrix %v concurrency %v", job.Name, job.Spec.Matrix, job.Spec.Concurrency)
	}
	if second := view.Jobs[1]; second.Name != "Build (1.27)" || second.Spec.Matrix["go"] != "1.27" || second.Spec.Concurrency.Group != "build-1.27" {
		t.Fatalf("second cell = name %q matrix %v", second.Name, second.Spec.Matrix)
	}
	if job.ID == view.Jobs[1].ID || job.Context == view.Jobs[1].Context {
		t.Fatalf("matrix cells share identity: %q/%q %q/%q", job.ID, view.Jobs[1].ID, job.Context, view.Jobs[1].Context)
	}
	spec := job.Spec
	if spec == nil {
		t.Fatal("job has no spec")
	}
	if spec.Version != SpecVersion || SpecVersion != 1 {
		t.Fatalf("spec version = %d", spec.Version)
	}
	if spec.Workflow != "Spec CI" {
		t.Fatalf("spec workflow = %q", spec.Workflow)
	}
	if len(spec.RunsOn) != 2 || spec.RunsOn[0] != "linux" || spec.RunsOn[1] != "docker" {
		t.Fatalf("spec runs-on = %v", spec.RunsOn)
	}
	if spec.Env["GLOBAL"] != "2" || spec.Env["JOB"] != "yes" {
		t.Fatalf("spec env = %v (job must override workflow)", spec.Env)
	}
	if spec.If != "github.ref == 'refs/heads/main'" || spec.TimeoutMinutes != 15 || !spec.ContinueOnError {
		t.Fatalf("spec job fields = %+v", spec)
	}
	if spec.Permissions["contents"] != "write" {
		t.Fatalf("spec permissions = %v", spec.Permissions)
	}
	if spec.Concurrency == nil || spec.Concurrency.Group != "build-1.26" || !spec.Concurrency.CancelInProgress {
		t.Fatalf("spec concurrency = %+v", spec.Concurrency)
	}
	if spec.Strategy == nil || spec.Strategy.FailFast == nil || *spec.Strategy.FailFast || len(spec.Strategy.Matrix["go"]) != 2 {
		t.Fatalf("spec strategy = %+v", spec.Strategy)
	}
	c := spec.Container
	if c == nil || c.Image != "golang:1.27" || c.Env["CGO_ENABLED"] != "0" || len(c.Ports) != 1 ||
		len(c.Command) != 2 || len(c.Entrypoint) != 1 || c.Options != "--user 1000" || len(c.Volumes) != 1 {
		t.Fatalf("spec container = %+v", c)
	}
	svc := spec.Services["redis"]
	if svc.Image != "redis:7" || svc.Env["REDIS_MAXMEMORY"] != "64mb" || len(svc.Ports) != 1 ||
		len(svc.Command) != 3 || len(svc.Entrypoint) != 1 || svc.Options == "" || len(svc.Volumes) != 1 {
		t.Fatalf("spec service = %+v", svc)
	}
	if len(spec.Steps) != 2 {
		t.Fatalf("spec steps = %+v", spec.Steps)
	}
	first := spec.Steps[0]
	if first.Name != "checkout" || first.Uses != "actions/checkout" || first.With["fetch-depth"] != "0" ||
		first.Env["A"] != "b" || first.If != "always()" || first.Shell != "bash" || first.WorkingDirectory != "./sub" {
		t.Fatalf("spec step = %+v", first)
	}
	// Convenience fields stay a projection of the spec (check naming carries the
	// matrix cell).
	if job.RunsOn != "linux, docker" || job.Container != "golang:1.27" || job.Context != "threads/spec-ci/build-1.26" {
		t.Fatalf("convenience fields = runs_on %q container %q context %q", job.RunsOn, job.Container, job.Context)
	}
	if len(job.Steps) != 2 || job.Steps[0].Uses != "actions/checkout" {
		t.Fatalf("convenience steps = %+v", job.Steps)
	}

	// The record round-trips through JSON with the spec intact (what the runner
	// reads from the bucket).
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Job
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Spec == nil || back.Spec.Version != SpecVersion || back.Spec.Services["redis"].Image != "redis:7" ||
		len(back.Spec.Steps) != 2 || back.Spec.Env["JOB"] != "yes" {
		t.Fatalf("round-trip spec = %+v", back.Spec)
	}
}

// TestBuildSpecWorkflowConcurrencyFallback pins the workflow-level default: a
// job without its own concurrency inherits the workflow's group.
func TestBuildSpecWorkflowConcurrencyFallback(t *testing.T) {
	wf := &workflow.Workflow{Concurrency: &workflow.Concurrency{Group: "wf", CancelInProgress: true}}
	spec := buildSpec("CI", wf, workflow.Job{RunsOn: workflow.Labels{"linux"}}, nil)
	if spec.Concurrency == nil || spec.Concurrency.Group != "wf" || !spec.Concurrency.CancelInProgress {
		t.Fatalf("concurrency = %+v", spec.Concurrency)
	}
	if spec := buildSpec("CI", &workflow.Workflow{}, workflow.Job{RunsOn: workflow.Labels{"linux"}}, nil); spec.Concurrency != nil {
		t.Fatalf("no concurrency = %+v", spec.Concurrency)
	}
}

// TestCheckMirrorReadsSpecWorkflow pins the checks/context migration: the
// mirror's label comes from the job's spec, so a job record alone is enough and
// the context string is unchanged.
func TestCheckMirrorReadsSpecWorkflow(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range view.Jobs {
		if job.Spec == nil || job.specWorkflow("fallback") != "CI" {
			t.Fatalf("spec workflow = %+v", job.Spec)
		}
		if job.Context != CheckContext("CI", job.Name) {
			t.Fatalf("context %q != spec-derived %q", job.Context, CheckContext("CI", job.Name))
		}
	}
	if len(e.checks.reports) == 0 {
		t.Fatal("no check reports")
	}
	known := map[string]bool{}
	for _, job := range view.Jobs {
		known[job.Context] = true
	}
	for _, rep := range e.checks.reports {
		if !known[rep.Context] {
			t.Fatalf("mirror context %q is not a spec-derived job context %v", rep.Context, known)
		}
	}
}
