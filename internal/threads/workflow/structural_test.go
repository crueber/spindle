package workflow

// structural_test.go — the structural fields (`runs-on`, `container.image`,
// `services.<id>.image`, `environment`) and the env layers above a step
// (workflow, job, container, service) never take `${{ … }}`:
// docs/WORKFLOWS.md §6.2. A span there is a named parse error, because the
// structural value is chosen before a matrix cell or dispatch input exists and
// an env value at those layers is copied verbatim — accepting one can only
// queue a job against a literal label, hand the image gate a literal string, or
// put `${{ … }}` into the process environment.

import (
	"errors"
	"strings"
	"testing"
)

// structuralWorkflow wraps one job body in an otherwise valid workflow.
func structuralWorkflow(job string) string {
	return "name: S\non: [push]\njobs:\n  build:\n" + job
}

func TestStructuralFieldsRejectExpressions(t *testing.T) {
	cases := []struct {
		name string
		job  string
		path string
	}{
		{
			name: "runs-on scalar",
			job:  "    runs-on: ${{ matrix.os }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.runs-on",
		},
		{
			name: "runs-on list element",
			job:  "    runs-on: [self-hosted, \"${{ matrix.arch }}\"]\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.runs-on[1]",
		},
		{
			name: "container scalar image",
			job:  "    runs-on: linux\n    container: \"debian:${{ matrix.variant }}\"\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.container.image",
		},
		{
			name: "container mapping image",
			job:  "    runs-on: linux\n    container:\n      image: \"debian:${{ matrix.variant }}\"\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.container.image",
		},
		{
			name: "service image",
			job:  "    runs-on: linux\n    services:\n      db:\n        image: \"postgres:${{ matrix.pg }}\"\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.services.db.image",
		},
		{
			name: "environment scalar name",
			job:  "    runs-on: linux\n    environment: ${{ matrix.env }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.environment",
		},
		{
			name: "environment mapping name",
			job:  "    runs-on: linux\n    environment:\n      name: ${{ matrix.env }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.environment",
		},
		{
			name: "environment url",
			job:  "    runs-on: linux\n    environment:\n      name: prod\n      url: ${{ steps.deploy.outputs.url }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.environment.url",
		},
		{
			name: "job env value",
			job:  "    runs-on: linux\n    env:\n      FOO: ${{ matrix.os }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.env.FOO",
		},
		{
			name: "container env value",
			job:  "    runs-on: linux\n    container:\n      image: debian:bookworm\n      env:\n        FOO: ${{ github.ref }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.container.env.FOO",
		},
		{
			name: "service env value",
			job:  "    runs-on: linux\n    services:\n      db:\n        image: postgres:17\n        env:\n          FOO: ${{ secrets.DSN }}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.services.db.env.FOO",
		},
		{
			// The defect's repro: accepted, then carried verbatim, so the run
			// queued against no runner and the image gate saw a literal.
			name: "repro from the parity matrix",
			job: "    runs-on: ${{ matrix.os }}\n    container:\n      image: debian:${{ matrix.variant }}\n" +
				"    strategy:\n      matrix: {os: [linux], variant: [bookworm]}\n    steps:\n      - run: echo hi\n",
			path: "jobs.build.runs-on",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(structuralWorkflow(tc.job)))
			if err == nil {
				t.Fatal("parsed; a structural field accepted a ${{ }} span")
			}
			if !errors.Is(err, ErrStructuralExpression) {
				t.Fatalf("err = %v, want ErrStructuralExpression", err)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error does not name %q: %v", tc.path, err)
			}
			if !strings.Contains(err.Error(), "does not take ${{ }}") {
				t.Errorf("error does not say the field takes no expression: %v", err)
			}
			if !strings.Contains(err.Error(), "literally") {
				t.Errorf("error does not name the remedy: %v", err)
			}
		})
	}
}

func TestStructuralFieldsLiteralValuesParse(t *testing.T) {
	src := `name: L
on: [push]
env:
  WORKFLOW_LEVEL: literal
jobs:
  build:
    runs-on: [self-hosted, linux]
    env:
      JOB_LEVEL: literal
    container:
      image: debian:bookworm
      env:
        CONTAINER_LEVEL: literal
    services:
      db:
        image: postgres:17
        env:
          SERVICE_LEVEL: literal
      cache:
        image: redis:7
    environment:
      name: production
      url: https://example.test/
    steps:
      - run: echo hi
  shorthand:
    runs-on: linux
    container: golang:1.27
    environment: staging
    steps:
      - run: echo hi
`
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("literal structural fields rejected: %v", err)
	}
	if got := w.Env["WORKFLOW_LEVEL"]; got != "literal" {
		t.Errorf("env.WORKFLOW_LEVEL = %q", got)
	}
	build := w.Jobs["build"]
	if got := build.Env["JOB_LEVEL"]; got != "literal" {
		t.Errorf("jobs.build.env.JOB_LEVEL = %q", got)
	}
	if got := build.RunsOn.String(); got != "self-hosted, linux" {
		t.Errorf("runs-on = %q", got)
	}
	if got := build.Container.Image; got != "debian:bookworm" {
		t.Errorf("container.image = %q", got)
	}
	if got := build.Container.Env["CONTAINER_LEVEL"]; got != "literal" {
		t.Errorf("jobs.build.container.env.CONTAINER_LEVEL = %q", got)
	}
	if got := build.Services["db"].Image; got != "postgres:17" {
		t.Errorf("services.db.image = %q", got)
	}
	if got := build.Services["db"].Env["SERVICE_LEVEL"]; got != "literal" {
		t.Errorf("jobs.build.services.db.env.SERVICE_LEVEL = %q", got)
	}
	if got := build.Services["cache"].Image; got != "redis:7" {
		t.Errorf("services.cache.image = %q", got)
	}
	if build.Environment == nil || build.Environment.Name != "production" ||
		build.Environment.URL != "https://example.test/" {
		t.Errorf("environment = %+v", build.Environment)
	}
	shorthand := w.Jobs["shorthand"]
	if got := shorthand.RunsOn.String(); got != "linux" {
		t.Errorf("shorthand runs-on = %q", got)
	}
	if got := shorthand.Container.Image; got != "golang:1.27" {
		t.Errorf("shorthand container image = %q", got)
	}
	if shorthand.Environment == nil || shorthand.Environment.Name != "staging" {
		t.Errorf("shorthand environment = %+v", shorthand.Environment)
	}
}

// TestWorkflowEnvRejectsExpression pins the workflow layer, whose path is not
// job-scoped.
func TestWorkflowEnvRejectsExpression(t *testing.T) {
	src := "name: W\non: [push]\nenv:\n  FOO: ${{ github.ref }}\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrStructuralExpression) {
		t.Fatalf("err = %v, want ErrStructuralExpression", err)
	}
	if !strings.Contains(err.Error(), "env.FOO") {
		t.Errorf("error does not name env.FOO: %v", err)
	}
	if !strings.Contains(err.Error(), "step `env:`") {
		t.Errorf("error does not name the remedy: %v", err)
	}
}

// TestStepEnvStillSubstitutes pins the boundary the rejection must not cross: a
// STEP env value is a substitution site (§6.2), so the same span stays legal
// there.
func TestStepEnvStillSubstitutes(t *testing.T) {
	src := `name: P
on: [push]
env:
  BASE: literal
jobs:
  build:
    runs-on: linux
    strategy:
      matrix: {os: [linux]}
    steps:
      - env:
          OS: ${{ matrix.os }}
          REF: ${{ github.ref }}
          LAYER: ${{ env.BASE }}
        run: echo hi
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("step env with a supported reference rejected: %v", err)
	}
}

// TestStructuralServiceErrorIsStable pins that two offending sidecars always
// report the same one (map iteration order must not leak into the message).
func TestStructuralServiceErrorIsStable(t *testing.T) {
	src := structuralWorkflow("    runs-on: linux\n    services:\n" +
		"      zeta:\n        image: \"z:${{ matrix.a }}\"\n" +
		"      alpha:\n        image: \"a:${{ matrix.b }}\"\n" +
		"    steps:\n      - run: echo hi\n")
	const want = "jobs.build.services.alpha.image"
	for i := 0; i < 20; i++ {
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("run %d: err = %v, want the %s error", i, err, want)
		}
	}
}

// TestStructuralFieldsOnReusableJob pins that a `uses:` job is checked too:
// it takes no runs-on, but a stray structural span is still a parse error
// rather than a value the parser quietly drops.
func TestStructuralFieldsOnReusableJob(t *testing.T) {
	src := "name: R\non: [push]\njobs:\n  call:\n    uses: ./.spindle/workflows/callee.yml\n" +
		"    runs-on: ${{ matrix.os }}\n"
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrStructuralExpression) {
		t.Fatalf("err = %v, want ErrStructuralExpression", err)
	}
}
