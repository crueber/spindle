package threads

// The server-side container option and mount policy (docs/threads.md §8.7):
// attack strings, the benign set, and the run-creation/rerun refusal shape.

import (
	"strings"
	"testing"
)

func TestContainerOptionPolicyAttackStrings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options string
		volume  []string
		field   string
		token   string
	}{
		{"privileged", "--privileged", nil, "container.options", "--privileged"},
		{"pid host", "--pid=host", nil, "container.options", "--pid=host"},
		{"pid host spaced", "--pid host", nil, "container.options", "--pid"},
		{"ipc host", "--ipc=host", nil, "container.options", "--ipc=host"},
		{"network host", "--network=host", nil, "container.options", "--network=host"},
		{"userns host", "--userns=host", nil, "container.options", "--userns=host"},
		{"cgroupns host", "--cgroupns=host", nil, "container.options", "--cgroupns=host"},
		{"uts host", "--uts=host", nil, "container.options", "--uts=host"},
		{"cap add all", "--cap-add=ALL", nil, "container.options", "--cap-add=ALL"},
		{"cap add spaced", "--cap-add SYS_ADMIN", nil, "container.options", "--cap-add"},
		{"device", "--device /dev/sda", nil, "container.options", "--device"},
		{"security opt", "--security-opt seccomp=unconfined", nil, "container.options", "--security-opt"},
		{"mount bind root", "--mount type=bind,source=/,target=/host", nil, "container.options", "--mount"},
		{"short volume", "-v /:/host", nil, "container.options", "-v"},
		{"short volume attached", "-v/:/host", nil, "container.options", "-v/:/host"},
		{"volume equals", "--volume=/:/host", nil, "container.options", "--volume=/:/host"},
		{"tmpfs", "--tmpfs /host", nil, "container.options", "--tmpfs"},
		{"volumes from", "--volumes-from other", nil, "container.options", "--volumes-from"},
		{"group add", "--group-add 0", nil, "container.options", "--group-add"},
		{"add host", "--add-host evil:10.0.0.1", nil, "container.options", "--add-host"},
		{"entrypoint override", "--entrypoint /bin/sh", nil, "container.options", "--entrypoint"},
		{"publish", "--publish 0.0.0.0:22:22", nil, "container.options", "--publish"},
		{"ulimit core", "--ulimit core=0", nil, "container.options", "core=0"},
		{"init with value", "--init=true", nil, "container.options", "--init=true"},
		{"bare argument", "--cpus 2 --privileged", nil, "container.options", "--privileged"},
		{"quoted privileged", `'--privileged'`, nil, "container.options", "--privileged"},
		{"double-quoted device", `--device "/dev/sda"`, nil, "container.options", "--device"},
		{"quoted bind mount", `-v "/:/host"`, nil, "container.options", "-v"},
		{"bare positional", "hostname", nil, "container.options", "hostname"},
		{"volume list", "", []string{"/:/host"}, "container.volumes", "/:/host"},
		{"named volume list", "", []string{"cache:/go/pkg/mod"}, "container.volumes", "cache:/go/pkg/mod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckWorkflowContainerOptions("container", tc.options, tc.volume)
			if r == nil {
				t.Fatalf("options %q volumes %v were allowed", tc.options, tc.volume)
			}
			if r.Field != tc.field {
				t.Fatalf("field = %q, want %q (reason: %s)", r.Field, tc.field, r.Reason)
			}
			if !strings.Contains(r.Reason, tc.token) {
				t.Fatalf("reason %q does not name %q", r.Reason, tc.token)
			}
		})
	}
}

func TestContainerOptionPolicyBenignSet(t *testing.T) {
	for _, options := range []string{
		"--cpus 2 --memory 512m",
		"--cpus=2 --memory=512m --memory-swap=1g",
		"-m 512m",
		"-m=512m",
		"--shm-size 256m",
		"--shm-size=256m",
		"--ulimit nofile=1024",
		"--ulimit nofile=1024:2048",
		"--init",
		"--read-only",
		"--user 1000",
		"--user=1000:1000",
		"-u 1000",
		"--workdir /src",
		"-w /src",
		`--cpus "2"`,
		`--user "1000" --read-only --init`,
		"",
	} {
		t.Run(options, func(t *testing.T) {
			if r := CheckWorkflowContainerOptions("container", options, nil); r != nil {
				t.Fatalf("benign options %q refused: %s", options, r.Reason)
			}
		})
	}
}

// TestContainerOptionPolicyFieldNames pins the service field spelling: the
// refusal must say exactly which sidecar carries the option.
func TestContainerOptionPolicyFieldNames(t *testing.T) {
	spec := &Spec{
		Services: map[string]ServiceSpec{
			"postgres": {Image: "postgres:17", Options: "--privileged"},
		},
	}
	r := containerOptionRefusalReason(spec)
	if r == nil || r.Field != "services.postgres.options" || !strings.Contains(r.Reason, "--privileged") {
		t.Fatalf("refusal = %+v", r)
	}
	spec = &Spec{Services: map[string]ServiceSpec{
		"postgres": {Image: "postgres:17", Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock"}},
	}}
	r = containerOptionRefusalReason(spec)
	if r == nil || r.Field != "services.postgres.volumes" {
		t.Fatalf("refusal = %+v", r)
	}
	if got := AllowedContainerOptions(); len(got) == 0 || !containsString(got, "--cpus") || containsString(got, "--privileged") {
		t.Fatalf("allowlist rendering = %v", got)
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

const containerOptionWorkflow = `name: CI
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
jobs:
  build:
    runs-on: linux
    container:
      image: golang:1.27
%CONTAINER%
    services:
      redis:
        image: redis:7
%SERVICE%
    steps:
      - run: go build ./...
`

// containerOptionEnv installs the parameterised workflow.
func containerOptionEnv(t *testing.T, container, service string) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	body := strings.Replace(containerOptionWorkflow, "%CONTAINER%", container, 1)
	body = strings.Replace(body, "%SERVICE%", service, 1)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", body)
	return e
}

// TestContainerOptionGateRefusesAtCreation is the server enforcement point: the
// refusal is a failed job with a Reason naming the field and the option, a red
// check mirror, and nothing enqueued.
func TestContainerOptionGateRefusesAtCreation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		container string
		service   string
		field     string
		token     string
	}{
		{"container privileged", "      options: --privileged", "", "container.options", "--privileged"},
		{"container pid host", "      options: '--pid=host'", "", "container.options", "--pid=host"},
		{"container volume", "      volumes:\n        - /:/host", "", "container.volumes", "/:/host"},
		{"service cap add", "", "        options: --cap-add=ALL", "services.redis.options", "--cap-add=ALL"},
		{"service volume", "", "        volumes:\n          - /var/run/docker.sock:/var/run/docker.sock", "services.redis.volumes", "docker.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := containerOptionEnv(t, tc.container, tc.service)
			run, job := createForClass(t, e, runClasses()[0])
			if job.State != JobFailure {
				t.Fatalf("job = %s (%q), want failure", job.State, job.Reason)
			}
			if !strings.Contains(job.Reason, tc.field) || !strings.Contains(job.Reason, tc.token) {
				t.Fatalf("reason = %q, want %q and %q", job.Reason, tc.field, tc.token)
			}
			if run.State != RunFailure {
				t.Fatalf("run = %s, want failure", run.State)
			}
			if len(e.checks.reports) != 1 || e.checks.reports[0].State != "failure" {
				t.Fatalf("checks = %+v, want one failure", e.checks.reports)
			}
			if got := countPrefix(t, e, QueuePrefix("o", "r")); got != 0 {
				t.Fatalf("queue objects = %d, a refused job must not be enqueued", got)
			}
		})
	}
}

// TestContainerOptionGateAllowsResourceFlags pins that the benign set reaches a
// runner (queued, no reason).
func TestContainerOptionGateAllowsResourceFlags(t *testing.T) {
	e := containerOptionEnv(t,
		"      options: --cpus 2 --memory 512m --init --read-only --user 1000\n      volumes: []",
		"        options: --shm-size 256m")
	run, job := createForClass(t, e, runClasses()[0])
	if job.State != JobQueued || job.Reason != "" || run.State != RunQueued {
		t.Fatalf("job = %s (%q) run = %s", job.State, job.Reason, run.State)
	}
}

// TestContainerOptionGateReappliesOnRerun: a refused job stays refused, with
// the same named reason, when an attempt is requested (the gate is shared).
func TestContainerOptionGateReappliesOnRerun(t *testing.T) {
	e := containerOptionEnv(t, "      options: --privileged", "")
	run, job := createForClass(t, e, runClasses()[0])
	if job.State != JobFailure {
		t.Fatalf("job = %s (%q)", job.State, job.Reason)
	}
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, e.writer()); err != nil {
		t.Fatalf("RerunRun: %v", err)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d (%v)", len(jobs), err)
	}
	if jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "container.options") {
		t.Fatalf("rerun job = %s (%q)", jobs[0].State, jobs[0].Reason)
	}
	if got := countPrefix(t, e, QueuePrefix("o", "r")); got != 0 {
		t.Fatalf("queue objects = %d after a refused rerun", got)
	}
}

// TestContainerOptionGateTrustClasses: the gate is not a fork-only rule.
func TestContainerOptionGateTrustClasses(t *testing.T) {
	for _, class := range runClasses() {
		t.Run(class.name, func(t *testing.T) {
			e := containerOptionEnv(t, "      options: --device /dev/sda", "")
			_, job := createForClass(t, e, class)
			if job.State != JobFailure || !strings.Contains(job.Reason, "--device") {
				t.Fatalf("job = %s (%q)", job.State, job.Reason)
			}
		})
	}
}

// TestServiceHealthOptionValidators pins the merged services health knobs
// (PAR-078): the command stays one in-container argv element and the numeric/
// duration values match docker's own parsers.
func TestServiceHealthOptionValidators(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"pg_isready -U postgres", true},
		{"true", true},
		{"", false},
		{"   ", false},
		{"--privileged", false},
		{"a\nb", false},
		{"a\rb", false},
		{"a\x00b", false},
	} {
		if got := validHealthCommand(tc.value); got != tc.want {
			t.Errorf("validHealthCommand(%q) = %v", tc.value, got)
		}
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"10s", true}, {"1m30s", true}, {"0s", false}, {"-1s", false}, {"nope", false},
	} {
		if got := validHealthDuration(tc.value); got != tc.want {
			t.Errorf("validHealthDuration(%q) = %v", tc.value, got)
		}
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"3", true}, {"1000", true}, {"0", false}, {"-1", false}, {"1001", false}, {"x", false},
	} {
		if got := validHealthRetries(tc.value); got != tc.want {
			t.Errorf("validHealthRetries(%q) = %v", tc.value, got)
		}
	}
	if !validUlimitValue("nofile=1024:2048") || validUlimitValue("nproc=4") {
		t.Fatal("validUlimitValue")
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"x", true}, {"", false}, {"-x", false}, {"a b", false}, {"a\x7f", false},
	} {
		if got := validGenericOptionValue(tc.value); got != tc.want {
			t.Errorf("validGenericOptionValue(%q) = %v", tc.value, got)
		}
	}
}
