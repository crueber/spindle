package runner

// The runner's own container option/mount gate (docs/threads.md §8.7): the plan
// refuses a hostile argv even when the server that created the job did not, and
// the job never starts a container.

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func TestPlanRefusesWorkflowContainerOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options string
		volumes []string
		source  string
		token   string
	}{
		{"privileged", "--privileged", nil, "container.options", "--privileged"},
		{"pid host", "--pid=host", nil, "container.options", "--pid=host"},
		{"ipc host", "--ipc host", nil, "container.options", "--ipc"},
		{"network host", "--network=host", nil, "container.options", "--network=host"},
		{"userns host", "--userns=host", nil, "container.options", "--userns=host"},
		{"cgroupns host", "--cgroupns=host", nil, "container.options", "--cgroupns=host"},
		{"cap add all", "--cap-add=ALL", nil, "container.options", "--cap-add=ALL"},
		{"device", "--device /dev/sda", nil, "container.options", "--device"},
		{"security opt", "--security-opt seccomp=unconfined", nil, "container.options", "--security-opt"},
		{"mount bind root", "--mount type=bind,source=/,target=/host", nil, "container.options", "--mount"},
		{"short volume", "-v /:/host", nil, "container.options", "-v"},
		{"volume equals", "--volume=/:/host", nil, "container.options", "--volume=/:/host"},
		{"tmpfs", "--tmpfs /host", nil, "container.options", "--tmpfs"},
		{"quoted privileged", `"--privileged"`, nil, "container.options", "--privileged"},
		{"quoted bind mount", `-v "/:/host"`, nil, "container.options", "-v"},
		{"host docker socket volume", "", []string{"/var/run/docker.sock:/var/run/docker.sock"}, "container.volumes", "docker.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := fixtureJob()
			job.Spec.Container.Options = tc.options
			job.Spec.Container.Volumes = tc.volumes
			_, err := BuildPlan(PlanInput{Job: job})
			if err == nil {
				t.Fatalf("options %q volumes %v were planned", tc.options, tc.volumes)
			}
			if !strings.Contains(err.Error(), tc.source) || !strings.Contains(err.Error(), tc.token) {
				t.Fatalf("error = %q, want %q and %q", err, tc.source, tc.token)
			}
		})
	}
}

func TestPlanRefusesServiceContainerOptions(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = map[string]threads.ServiceSpec{
		"postgres": {Image: "postgres:17", Options: "--privileged"},
	}
	_, err := BuildPlan(PlanInput{Job: job})
	if err == nil || !strings.Contains(err.Error(), "services.postgres.options") || !strings.Contains(err.Error(), "--privileged") {
		t.Fatalf("service options = %v", err)
	}
	job = fixtureJob()
	job.Spec.Services = map[string]threads.ServiceSpec{
		"postgres": {Image: "postgres:17", Volumes: []string{"/:/host"}},
	}
	_, err = BuildPlan(PlanInput{Job: job})
	if err == nil || !strings.Contains(err.Error(), "services.postgres.volumes") {
		t.Fatalf("service volumes = %v", err)
	}
}

func TestPlanAllowsResourceOptions(t *testing.T) {
	job := fixtureJob()
	job.Spec.Container.Options = "--cpus 2 --memory 512m --init --read-only --user 1000 --ulimit nofile=1024"
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	if !strings.Contains(argv, "--cpus 2 --memory 512m") || !strings.Contains(argv, "--ulimit nofile=1024") {
		t.Fatalf("argv = %s", argv)
	}
	// The runner's own workspace mount is the ONLY mount on the job container.
	if got := strings.Count(argv, "--mount"); got != 1 {
		t.Fatalf("mounts = %d in %s", got, argv)
	}
}

// TestExecuteRefusesToStartAHostileContainer pins the defensive executor: a
// refused plan reaches no docker call at all and the job fails with a spindle:
// diagnostic (the server was older or bypassed).
func TestExecuteRefusesToStartAHostileContainer(t *testing.T) {
	docker := &fakeDocker{streamOutput: "ok\n"}
	job := fixtureJob()
	job.Spec.Container.Options = "--privileged"
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token"}, stream)
	stream.Close()
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "container.options") {
		t.Fatalf("result = %s / %q", res.Conclusion, res.Error)
	}
	if calls := docker.snapshot(); len(calls) != 0 {
		t.Fatalf("docker was called for a refused job: %v", calls)
	}
	joined := frames()
	if !strings.Contains(joined, "spindle: cannot plan job") || !strings.Contains(joined, "--privileged") {
		t.Fatalf("frames do not carry the refusal: %s", joined)
	}
}
