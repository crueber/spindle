package runner

// The job container's mapping keys (PAR-077): entrypoint, command, ports and
// volumes are APPLIED, credentials and a TTY are refused by name, and any grant
// that widens isolation excludes fork runs.
//
// The parser spellings and the SERVER's creation-time refusal are in
// internal/threads (containerpolicy_test.go, w6_containers_test.go); this file
// is the runner boundary — the argv a docker daemon actually receives.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// containerKeyJob is a minimal fixture with a job container.
func containerKeyJob() *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "run", Run: "echo hi"}}
	return job
}

// TestJobContainerEntrypointCommandArgv pins who owns the container process:
// the runner's keep-alive by default, and the workflow's own process when it
// names `entrypoint`/`command` (docker's semantics: an entrypoint takes the
// remaining words plus the command as its arguments; a command alone overrides
// the image's CMD and leaves its ENTRYPOINT in place).
func TestJobContainerEntrypointCommandArgv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entrypoint []string
		command    []string
		lifetime   string
		want       []string
		absent     []string
	}{
		{
			name:     "neither keeps the runner keep-alive",
			lifetime: ContainerLifetimeKeepAlive,
			want:     []string{"--entrypoint /bin/sh", "golang:1.27 -c trap 'exit 0' TERM INT"},
		},
		{
			name:       "entrypoint and command apply the sleep infinity example",
			entrypoint: []string{"/bin/sh"},
			command:    []string{"-c", "sleep infinity"},
			lifetime:   ContainerLifetimeWorkflow,
			want:       []string{"--entrypoint /bin/sh golang:1.27 -c sleep infinity"},
			absent:     []string{"trap 'exit 0'"},
		},
		{
			name:       "a multi-word entrypoint leads and the command follows",
			entrypoint: []string{"/usr/bin/env", "FOO=1"},
			command:    []string{"bash", "-c", "sleep 999"},
			lifetime:   ContainerLifetimeWorkflow,
			want:       []string{"--entrypoint /usr/bin/env golang:1.27 FOO=1 bash -c sleep 999"},
		},
		{
			name:     "a command alone leaves the image entrypoint",
			command:  []string{"server", "--listen=:8080"},
			lifetime: ContainerLifetimeWorkflow,
			want:     []string{"golang:1.27 server --listen=:8080"},
			absent:   []string{"--entrypoint"},
		},
	} {
		job := containerKeyJob()
		job.Spec.Container.Entrypoint = tc.entrypoint
		job.Spec.Container.Command = tc.command
		p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
		if err != nil {
			t.Fatalf("%s: BuildPlan: %v", tc.name, err)
		}
		argv := strings.Join(p.ContainerArgv, " ")
		if p.ContainerLifetime != tc.lifetime {
			t.Fatalf("%s: lifetime = %q, want %q (argv %s)", tc.name, p.ContainerLifetime, tc.lifetime, argv)
		}
		for _, want := range tc.want {
			if !strings.Contains(argv, want) {
				t.Fatalf("%s: argv misses %q\nargv: %s", tc.name, want, argv)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(argv, absent) {
				t.Fatalf("%s: argv must not carry %q\nargv: %s", tc.name, absent, argv)
			}
		}
	}
}

// TestJobContainerPortsApplied: `ports` is a real ephemeral loopback publish,
// never a fixed host port, and the plan records what it applied.
func TestJobContainerPortsApplied(t *testing.T) {
	job := containerKeyJob()
	job.Spec.Container.Ports = []string{"8080", "53/udp"}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	for _, want := range []string{"--publish 127.0.0.1::8080", "--publish 127.0.0.1::53/udp"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv misses %q\nargv: %s", want, argv)
		}
	}
	if strings.Join(p.ContainerPorts, ",") != "8080,53/udp" {
		t.Fatalf("ContainerPorts = %v", p.ContainerPorts)
	}
	// A fixed host port is refused by name rather than silently rewritten.
	job.Spec.Container.Ports = []string{"8080:80"}
	if _, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"}); err == nil {
		t.Fatal("a host-side port must be refused")
	}
}

// TestJobContainerPortRefusals is the table of invalid `ports` entries.
func TestJobContainerPortRefusals(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want string
	}{
		{"8080:80", "give the container port only"},
		{"127.0.0.1:8080:80", "give the container port only"},
		{"", "empty port"},
		{"tcp", "container port number"},
		{"70000", "1…65535"},
		{"0", "1…65535"},
		{"8080/sctp", "tcp or udp"},
	} {
		if _, err := threads.ParseJobContainerPort(tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: err = %v, want %q", tc.spec, err, tc.want)
		}
	}
	for _, spec := range []string{"8080", " 8080/tcp ", "53/udp"} {
		if _, err := threads.ParseJobContainerPort(spec); err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
	}
}

// TestJobContainerCredentialsRefusedAtPlan is the runner's defence in depth: a
// spec that carries a workflow registry login never reaches docker, even if a
// server let it through.
func TestJobContainerCredentialsRefusedAtPlan(t *testing.T) {
	job := containerKeyJob()
	job.Spec.Container.Credentials = &threads.RegistryCredentialsSpec{Username: "u", Password: "p"}
	_, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err == nil || !strings.Contains(err.Error(), "container.credentials is refused") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "u") && strings.Contains(err.Error(), "p") {
		// The reason must name the field, never echo a credential.
		if strings.Contains(err.Error(), "u:") || strings.Contains(err.Error(), "=p") {
			t.Fatalf("the refusal leaked the credential: %v", err)
		}
	}
}

// TestJobContainerTTYRefused: `--tty` in options is a named refusal, not a
// silently inert flag (the runner's docker exec has no terminal).
func TestJobContainerTTYRefused(t *testing.T) {
	job := containerKeyJob()
	job.Spec.Container.Options = "--tty"
	_, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err == nil || !strings.Contains(err.Error(), "--tty") || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("err = %v", err)
	}
}

// TestJobContainerInitAndVolumesApplied pins the two keys that were already
// accepted and must stay applied: --init is an allowlisted option, and a volume
// is applied only under the runner's explicit allowlist.
func TestJobContainerInitAndVolumesApplied(t *testing.T) {
	job := containerKeyJob()
	job.Spec.Container.Options = "--init"
	job.Spec.Container.Volumes = []string{"/srv/cache:/cache:ro"}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t", Policy: threads.ContainerPolicy{ValidVolumes: []string{"/srv/cache"}}})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	if !strings.Contains(argv, " --init ") {
		t.Fatalf("--init was not applied: %s", argv)
	}
	if !strings.Contains(argv, "--volume /srv/cache:/cache:ro") {
		t.Fatalf("the allow-listed volume was not applied: %s", argv)
	}
	// The same volume on a runner that grants nothing is refused by name.
	_, err = BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "--container-valid-volume") {
		t.Fatalf("err = %v", err)
	}
}
