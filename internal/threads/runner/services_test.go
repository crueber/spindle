package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Service parity (PAR-078): the documented health options reach docker create,
// and readiness gates the job on their verdict — a 60 s-style miss (bounded here
// by the executor's timeout) fails the job naming the service.

func healthJob(options string) *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = map[string]threads.ServiceSpec{
		"postgres": {
			Image:   "postgres:17",
			Env:     map[string]string{"POSTGRES_PASSWORD": "ci"},
			Ports:   []string{"5432"},
			Options: options,
		},
	}
	job.Spec.Steps = []threads.StepSpec{{Name: "test", Run: "true", Shell: "sh"}}
	return job
}

func TestServiceHealthOptionsReachDockerCreate(t *testing.T) {
	job := healthJob(`--health-cmd "pg_isready -U postgres" --health-interval 10s --health-timeout 5s --health-retries 5`)
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.Services[0].Argv, " ")
	for _, want := range []string{
		"--network-alias postgres",
		"--health-cmd pg_isready -U postgres",
		"--health-interval 10s",
		"--health-timeout 5s",
		"--health-retries 5",
		"--publish 127.0.0.1::5432",
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("service argv misses %q: %s", want, argv)
		}
	}
}

func TestServiceHealthRefusals(t *testing.T) {
	cases := []string{
		"--health-cmd -pg_isready",   // a value that looks like a flag
		"--health-interval nonsense", // not a duration
		"--health-timeout 0s",        // not positive
		"--health-retries 0",         // not a retry count
		"--no-healthcheck",           // not a workflow-controlled knob
	}
	for _, options := range cases {
		reason := threads.CheckWorkflowContainerOptions("services.postgres", options, nil)
		if reason == nil {
			t.Errorf("%q was accepted", options)
			continue
		}
		if !strings.Contains(reason.Reason, "services.postgres.options") {
			t.Errorf("%q refusal = %q", options, reason.Reason)
		}
	}
	// A health command with spaces (quoted) is legitimate: it is a command, not
	// a single token.
	if r := threads.CheckWorkflowContainerOptions("services.postgres", `--health-cmd "pg_isready -U postgres -d ci"`, nil); r != nil {
		t.Fatalf("quoted health command refused: %s", r.Reason)
	}
}

// TestServiceUnhealthyFailsTheJob pins the readiness gate's verdict: an
// unhealthy sidecar fails setup immediately and names the service.
func TestServiceUnhealthyFailsTheJob(t *testing.T) {
	d := &fakeDocker{runOutputs: []runOutput{{match: "inspect", out: "unhealthy\n"}}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: healthJob("--health-cmd pg_isready"), AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "service postgres is unhealthy") {
		t.Fatalf("unhealthy = %s %q", res.Conclusion, res.Error)
	}
}

// TestServiceReadinessTimeoutNamesTheService pins the miss path: a service that
// never becomes healthy (still `starting`) fails the job naming the service and
// its state once the (test-short) timeout passes.
func TestServiceReadinessTimeoutNamesTheService(t *testing.T) {
	d := &fakeDocker{runOutputs: []runOutput{{match: "inspect", out: "starting\n"}}}
	e := executorWith(d)
	e.ServiceReadyTimeout = 20 * time.Millisecond
	res := e.Execute(context.Background(), ExecInput{Job: healthJob("--health-cmd pg_isready"), AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "service postgres not ready") {
		t.Fatalf("timeout = %s %q", res.Conclusion, res.Error)
	}
}
