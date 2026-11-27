package runner

// PAR-179 container-half and PAR-078 registry-auth tests: the operator knobs are
// validated and bounded, the zero policy keeps today's fail-closed behaviour,
// and credentials never reach argv or a log frame.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func policyJob(options string, volumes ...string) *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Container = &threads.ContainerSpec{Image: "golang:1.27", Options: options, Volumes: volumes}
	job.Spec.Steps = []threads.StepSpec{{Name: "test", Run: "true"}}
	return job
}

func TestDefaultPolicyRefusesWorkflowPrivilege(t *testing.T) {
	_, err := BuildPlan(PlanInput{Job: policyJob("--privileged")})
	if err == nil || !strings.Contains(err.Error(), "--container-privileged") {
		t.Fatalf("error = %v, want a named --container-privileged refusal", err)
	}
	p, err := BuildPlan(PlanInput{Job: policyJob("--privileged"), Policy: threads.ContainerPolicy{Privileged: true}})
	if err != nil {
		t.Fatalf("granted policy: %v", err)
	}
	if argv := strings.Join(p.ContainerArgv, " "); !strings.Contains(argv, "--privileged") {
		t.Fatalf("container argv misses the granted flag: %s", argv)
	}
}

func TestValidVolumesAllowlist(t *testing.T) {
	allowed := threads.ContainerPolicy{ValidVolumes: []string{"/srv/cache/*"}}
	// A matching host path is admitted and reaches docker create.
	if p, err := BuildPlan(PlanInput{Job: policyJob("", "/srv/cache/a:/cache:ro"), Policy: allowed}); err != nil {
		t.Fatalf("matching volume refused: %v", err)
	} else if argv := strings.Join(p.ContainerArgv, " "); !strings.Contains(argv, "--volume /srv/cache/a:/cache:ro") {
		t.Fatalf("volume not in argv: %s", argv)
	}
	for _, tc := range []struct {
		name    string
		volume  string
		policy  threads.ContainerPolicy
		wantErr string
	}{
		{"default refuses every volume", "/srv/cache/a:/cache", threads.ContainerPolicy{}, "--container-valid-volume"},
		{"outside the allowlist", "/etc/passwd:/etc/passwd", allowed, "not in the runner's --container-valid-volume allowlist"},
		{"named volume", "cache:/go/pkg/mod", allowed, "absolute host source"},
		{"relative source", "./cache:/cache", allowed, "absolute host source"},
		{"host root", "/:/host", allowed, "host root"},
		{"bad options", "/srv/cache/a:/cache:shared", allowed, "volume options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(PlanInput{Job: policyJob("", tc.volume), Policy: tc.policy})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestOperatorNetworkKnob(t *testing.T) {
	job := policyJob("")
	job.Spec.Services = map[string]threads.ServiceSpec{"redis": {Image: "redis:7"}}
	p, err := BuildPlan(PlanInput{Job: job, Policy: threads.ContainerPolicy{Network: "spindle-jobs"}})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.NetworkMode != "spindle-jobs" {
		t.Fatalf("network mode = %q", p.NetworkMode)
	}
	if argv := strings.Join(p.ContainerArgv, " "); !strings.Contains(argv, "--network spindle-jobs") {
		t.Fatalf("job argv = %s", argv)
	}
	if argv := strings.Join(p.Services[0].Argv, " "); !strings.Contains(argv, "--network spindle-jobs") {
		t.Fatalf("service argv = %s", argv)
	}
	// The per-job network is not created (and the shared one is never removed).
	runJob := policyJob("")
	docker := &fakeDocker{}
	ex := executorWith(docker)
	ex.Policy = threads.ContainerPolicy{Network: "spindle-jobs"}
	res := ex.Execute(context.Background(), ExecInput{Job: runJob}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if docker.called("network create") {
		t.Fatalf("the per-job network was created despite a shared operator network: %v", docker.snapshot())
	}
}

func TestDockerHostGrant(t *testing.T) {
	job := policyJob("")
	job.Spec.Services = map[string]threads.ServiceSpec{"redis": {Image: "redis:7"}}
	job.Spec.Steps = []threads.StepSpec{{Name: "act", Uses: "docker://alpine:3.20"}}
	p, err := BuildPlan(PlanInput{Job: job, Policy: threads.ContainerPolicy{DockerHost: "unix:///run/docker.sock"}})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	for _, want := range []string{
		"--mount type=bind,source=/run/docker.sock,target=/run/docker.sock",
		"--env DOCKER_HOST=unix:///run/docker.sock",
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("job argv misses %q: %s", want, argv)
		}
	}
	if svc := strings.Join(p.Services[0].Argv, " "); strings.Contains(svc, "DOCKER_HOST") || strings.Contains(svc, "docker.sock") {
		t.Fatalf("a service inherited the grant: %s", svc)
	}
	if action := strings.Join(p.Steps[0].Argv, " "); strings.Contains(action, "DOCKER_HOST") {
		t.Fatalf("a docker action inherited the grant: %s", action)
	}
	// A tcp:// endpoint is environment only (no socket mount).
	p2, err := BuildPlan(PlanInput{Job: policyJob(""), Policy: threads.ContainerPolicy{DockerHost: "tcp://dind:2375"}})
	if err != nil {
		t.Fatalf("tcp grant: %v", err)
	}
	argv2 := strings.Join(p2.ContainerArgv, " ")
	if !strings.Contains(argv2, "--env DOCKER_HOST=tcp://dind:2375") || strings.Contains(argv2, "type=bind,source=/run") {
		t.Fatalf("tcp argv = %s", argv2)
	}
}

func TestValidateContainerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy threads.ContainerPolicy
		want   string
	}{
		{"host network", threads.ContainerPolicy{Network: "host"}, "job isolation"},
		{"none network", threads.ContainerPolicy{Network: "none"}, "job isolation"},
		{"container namespace", threads.ContainerPolicy{Network: "container:abc"}, "host namespace"},
		{"bad network name", threads.ContainerPolicy{Network: "spindle/jobs"}, "not a valid docker network name"},
		{"relative volume", threads.ContainerPolicy{ValidVolumes: []string{"cache/*"}}, "absolute host path"},
		{"parent traversal", threads.ContainerPolicy{ValidVolumes: []string{"/srv/../etc/*"}}, "absolute host path"},
		{"bad docker host scheme", threads.ContainerPolicy{DockerHost: "ssh://root@host"}, "unix:///absolute/socket or tcp://host:port"},
		{"relative socket", threads.ContainerPolicy{DockerHost: "unix://run/docker.sock"}, "absolute socket path"},
		{"ok", threads.ContainerPolicy{Network: "jobs", ValidVolumes: []string{"/srv/cache/*"}, DockerHost: "tcp://dind:2375"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := threads.ValidateContainerPolicy(tc.policy)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunnerRefusesHostGrantWithForkRuns(t *testing.T) {
	base := Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", AllowHostDocker: true}
	for _, tc := range []struct {
		name   string
		policy threads.ContainerPolicy
	}{
		{"privilege", threads.ContainerPolicy{Privileged: true}},
		{"docker host", threads.ContainerPolicy{DockerHost: "tcp://dind:2375"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.AllowForkRuns = true
			cfg.Policy = tc.policy
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "cannot be combined with --container-privileged") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRegistryAuthLoginUsesStdinAndPrivateConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "stdin")
	argv := filepath.Join(dir, "argv")
	cfgFile := filepath.Join(dir, "auth.txt")
	if err := os.WriteFile(cfgFile, []byte("registry.example.com bob s3cr3t-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat > \"$FAKE_STDIN\"\nprintf '%s\\n' \"$@\" > \"$FAKE_ARGV\"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_STDIN", record)
	t.Setenv("FAKE_ARGV", argv)
	auth, err := PrepareRegistryAuth(context.Background(), script, cfgFile)
	if err != nil {
		t.Fatalf("PrepareRegistryAuth: %v", err)
	}
	defer auth.Cleanup()
	if st, err := os.Stat(auth.ConfigDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode = %v (%v)", st.Mode().Perm(), err)
	}
	gotArgv, _ := os.ReadFile(argv)
	if strings.Contains(string(gotArgv), "s3cr3t-pw") {
		t.Fatalf("the password reached argv:\n%s", gotArgv)
	}
	for _, want := range []string{"login", "--password-stdin", "--username", "bob", "registry.example.com"} {
		if !strings.Contains(string(gotArgv), want) {
			t.Fatalf("argv misses %q:\n%s", want, gotArgv)
		}
	}
	gotStdin, _ := os.ReadFile(record)
	if strings.TrimSpace(string(gotStdin)) != "s3cr3t-pw" {
		t.Fatalf("password did not travel on stdin: %q", gotStdin)
	}
	if masks := auth.Masks(); len(masks) != 1 || masks[0] != "s3cr3t-pw" {
		t.Fatalf("masks = %v", masks)
	}
	// A group/world-readable credential file is refused.
	if err := os.Chmod(cfgFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRegistryAuth(context.Background(), script, cfgFile); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("readable file error = %v", err)
	}
}

func TestRegistryMasksRedactTheJobLog(t *testing.T) {
	docker := &fakeDocker{streamOutput: "pw=s3cr3t-pw\n"}
	e := executorWith(docker)
	e.RegistryAuth = &RegistryAuth{masks: []string{"s3cr3t-pw"}}
	job := bgJob(threads.StepSpec{Name: "echo", Run: "echo pw"})
	stream, frames := recordingStream(t)
	res := e.Execute(context.Background(), ExecInput{Job: job}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if log := frames(); strings.Contains(log, "s3cr3t-pw") || !strings.Contains(log, "***") {
		t.Fatalf("registry credential was not redacted:\n%s", log)
	}
}

func TestDockerInJobRemedy(t *testing.T) {
	job := bgJob(threads.StepSpec{Name: "build", Run: "docker build ."})
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "docker build", code: 127}}}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "daemonless") || !strings.Contains(res.Error, "--docker-host") {
		t.Fatalf("remedy error = %q", res.Error)
	}
	// A granted runner has a daemon; no remedy text is appended.
	docker2 := &fakeDocker{streamByMatch: []runOutput{{match: "docker build", code: 127}}}
	e2 := executorWith(docker2)
	e2.Policy = threads.ContainerPolicy{DockerHost: "tcp://dind:2375"}
	job2 := bgJob(threads.StepSpec{Name: "build", Run: "docker build ."})
	res2 := e2.Execute(context.Background(), ExecInput{Job: job2}, quietStream(t))
	if strings.Contains(res2.Error, "daemonless") {
		t.Fatalf("a granted runner should not print the daemonless remedy: %q", res2.Error)
	}
}

func TestBackgroundRequiresSetsid(t *testing.T) {
	docker := &fakeDocker{runOutputs: []runOutput{
		{match: "command -v 'setsid'", code: 1},
	}}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 1", Background: true})
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "setsid") {
		t.Fatalf("conclusion = %s (%q)", res.Conclusion, res.Error)
	}
}
