package runner

// Real-docker proof for the job container's mapping keys (PAR-077). Unlike the
// argv assertions, these exercise the daemon: a workflow-owned entrypoint/
// command really keeps the container alive, a declared port is really published
// on a loopback host port, an allow-listed volume is really a bind mount, and a
// command that exits is a named failure.
//
// Skipped unless SPINDLE_RUNNER_DOCKER_E2E=1 and a daemon answers.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// dockerKeyJob is a job whose only step proves the container is usable.
func dockerKeyJob(image string) *threads.FetchResult {
	job := fixtureJob()
	job.Checkout = nil
	job.Spec.Services = nil
	job.Spec.Container = &threads.ContainerSpec{Image: image}
	job.Spec.Steps = []threads.StepSpec{{Name: "alive", Run: "test -d \"$GITHUB_WORKSPACE\" && echo alive"}}
	return job
}

func TestDockerJobContainerKeys(t *testing.T) {
	dockerE2E(t)
	ctx := context.Background()

	t.Run("entrypoint and command own the lifetime", func(t *testing.T) {
		job := dockerKeyJob("alpine/git:latest")
		job.Spec.Container.Entrypoint = []string{"/bin/sh"}
		job.Spec.Container.Command = []string{"-c", "sleep infinity"}
		job.Spec.Steps = []threads.StepSpec{{Name: "pid1", Run: "tr '\\0' ' ' < /proc/1/cmdline"}}
		p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if p.ContainerLifetime != ContainerLifetimeWorkflow {
			t.Fatalf("lifetime = %q", p.ContainerLifetime)
		}
		e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine/git:latest"}
		stream, frames := recordingStream(t)
		res := e.Execute(ctx, ExecInput{Job: job}, stream)
		stream.Close()
		if res.Conclusion != threads.JobSuccess {
			t.Fatalf("conclusion = %s (%s)\n%s", res.Conclusion, res.Error, frames())
		}
		// PID 1 is the workflow's own command, not the runner's keep-alive.
		log := frames()
		if !strings.Contains(log, "sleep infinity") {
			t.Fatalf("the workflow's entrypoint/command is not PID 1:\n%s", log)
		}
		if strings.Contains(log, "while :; do sleep 3600") {
			t.Fatalf("the runner's keep-alive still owns the container:\n%s", log)
		}
	})

	t.Run("command alone leaves the image entrypoint", func(t *testing.T) {
		job := dockerKeyJob("alpine:3.20")
		job.Spec.Container.Command = []string{"sleep", "infinity"}
		p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		argv := strings.Join(p.ContainerArgv, " ")
		if strings.Contains(argv, "--entrypoint") {
			t.Fatalf("a command alone must keep the image entrypoint: %s", argv)
		}
		e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine:3.20"}
		stream, frames := recordingStream(t)
		res := e.Execute(ctx, ExecInput{Job: job}, stream)
		stream.Close()
		if res.Conclusion != threads.JobSuccess {
			t.Fatalf("conclusion = %s (%s)\n%s", res.Conclusion, res.Error, frames())
		}
	})

	t.Run("an exiting command is a named failure", func(t *testing.T) {
		job := dockerKeyJob("alpine:3.20")
		job.Spec.Container.Command = []string{"true"}
		e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine:3.20"}
		stream, frames := recordingStream(t)
		res := e.Execute(ctx, ExecInput{Job: job}, stream)
		stream.Close()
		if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "exited") ||
			!strings.Contains(res.Error, "must keep a process running") {
			t.Fatalf("result = %+v\n%s", res, frames())
		}
	})

	t.Run("an allow-listed volume is a real bind mount", func(t *testing.T) {
		dir := t.TempDir()
		job := dockerKeyJob("alpine/git:latest")
		job.Spec.Container.Volumes = []string{dir + ":/mnt/host"}
		job.Spec.Steps = []threads.StepSpec{{Name: "write", Run: "echo from-container > /mnt/host/marker"}}
		e := &Executor{
			Docker:       &DockerCLIProcess{},
			DefaultImage: "alpine/git:latest",
			Policy:       threads.ContainerPolicy{ValidVolumes: []string{dir}},
		}
		stream, frames := recordingStream(t)
		res := e.Execute(ctx, ExecInput{Job: job}, stream)
		stream.Close()
		if res.Conclusion != threads.JobSuccess {
			t.Fatalf("conclusion = %s (%s)\n%s", res.Conclusion, res.Error, frames())
		}
		body, err := os.ReadFile(filepath.Join(dir, "marker"))
		if err != nil || !strings.Contains(string(body), "from-container") {
			t.Fatalf("the volume was not mounted: %v (%q)", err, body)
		}
	})

	t.Run("a declared port is published on an ephemeral loopback port", func(t *testing.T) {
		job := dockerKeyJob("alpine/git:latest")
		job.Spec.Container.Ports = []string{"8080"}
		p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine/git:latest"}
		t.Cleanup(func() { e.cleanup(p) })
		if err := e.setup(ctx, p, quietStream(t)); err != nil {
			t.Fatalf("setup: %v", err)
		}
		/* The mapping docker actually made: loopback only, ephemeral. */
		out, err := exec.Command("docker", "port", p.Names.Container, "8080/tcp").CombinedOutput()
		if err != nil {
			t.Fatalf("port: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		mapping := strings.TrimSpace(string(out))
		if !strings.HasPrefix(mapping, "127.0.0.1:") {
			t.Fatalf("the publish is not loopback-only: %q", mapping)
		}
		hostPort := mapping[strings.LastIndex(mapping, ":")+1:]
		if hostPort == "8080" {
			t.Fatalf("the host port is fixed rather than ephemeral: %q", mapping)
		}
		// A listener inside the container is reachable through the mapping.
		listener := exec.Command("docker", "exec", "-d", p.Names.Container, "sh", "-c",
			"nc -lk -p 8080 -e /bin/cat")
		if out, err := listener.CombinedOutput(); err != nil {
			t.Fatalf("listener: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, derr := net.DialTimeout("tcp", "127.0.0.1:"+hostPort, time.Second)
			if derr == nil {
				_, _ = conn.Write([]byte("ping\n"))
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 32)
				n, _ := conn.Read(buf)
				_ = conn.Close()
				if strings.Contains(string(buf[:n]), "ping") {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the published port never answered: %v", derr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// TestDockerActionStatePreToPost is the real-daemon proof of the `$GITHUB_STATE`
// hand-off (PAR-092): the pre hook writes state, the main fails, and the post
// hook still runs with `STATE_<name>` (and a post failure fails the job).
func TestDockerActionStatePreToPost(t *testing.T) {
	dockerE2E(t)
	const image = "node:22-alpine"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, perr := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); perr != nil {
			t.Skipf("node image unavailable: %v (%s)", perr, strings.TrimSpace(string(out)))
		}
	}
	job := nodeJob("node20", true, true)
	job.Spec.Container = &threads.ContainerSpec{Image: image}
	job.Checkout = nil
	spec := job.Spec.Actions[nodeKey]
	spec.Path = "/spindle/remote-actions/state-e2e"
	spec.Files = []threads.ActionFile{
		{Path: "action.yml", Mode: 0o644, Data: []byte("name: state\n")},
		{Path: "dist/pre.js", Mode: 0o644, Data: []byte("require('fs').appendFileSync(process.env.GITHUB_STATE, 'cache-primary-key=from-pre\\n')\n")},
		{Path: "dist/index.js", Mode: 0o644, Data: []byte("process.exit(1)\n")},
		{Path: "dist/post.js", Mode: 0o644, Data: []byte("console.log('post-state=' + process.env['STATE_cache-primary-key'])\n")},
	}
	job.Spec.Actions[nodeKey] = spec
	e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: image}
	stream, frames := recordingStream(t)
	res := e.Execute(context.Background(), ExecInput{Job: job}, stream)
	stream.Close()
	log := frames()
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s, want the main's failure\n%s", res.Conclusion, log)
	}
	if !strings.Contains(log, "post-state=from-pre") {
		t.Fatalf("the post hook did not receive the pre hook's state:\n%s", log)
	}
	// A failing post hook fails an otherwise green job, naming the hook.
	green := nodeJob("node20", true, true)
	green.Spec.Container = &threads.ContainerSpec{Image: image}
	green.Checkout = nil
	spec = green.Spec.Actions[nodeKey]
	spec.Path = "/spindle/remote-actions/state-e2e-fail"
	spec.Files = []threads.ActionFile{
		{Path: "action.yml", Mode: 0o644, Data: []byte("name: state\n")},
		{Path: "dist/pre.js", Mode: 0o644, Data: []byte("require('fs').appendFileSync(process.env.GITHUB_STATE, 'k=v\\n')\n")},
		{Path: "dist/index.js", Mode: 0o644, Data: []byte("process.exit(0)\n")},
		{Path: "dist/post.js", Mode: 0o644, Data: []byte("process.exit(3)\n")},
	}
	green.Spec.Actions[nodeKey] = spec
	stream2, frames2 := recordingStream(t)
	res2 := e.Execute(context.Background(), ExecInput{Job: green}, stream2)
	stream2.Close()
	if res2.Conclusion != threads.JobFailure || !strings.Contains(res2.Error, "post hook") {
		t.Fatalf("post failure = %+v\n%s", res2, frames2())
	}
}
