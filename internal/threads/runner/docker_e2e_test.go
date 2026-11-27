package runner

// Real-docker runner e2e (PAR-080/PAR-081/PAR-078). Different from the fake
// boundary: the process-group kill, the parallel overlap and the daemonless
// build are properties of a real container, so they are driven here against the
// local daemon. Skipped unless SPINDLE_RUNNER_DOCKER_E2E=1 and a daemon answers
// (`make` never runs them implicitly; they cost a kaniko image and ~30s).
//
//	docker run -d …              a real job container
//	docker exec …                the step argv BuildPlan produced
//	/docker exec sh -c ps …      assert no process survived the group kill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// dockerE2E skips unless explicitly enabled and a daemon answers.
func dockerE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("SPINDLE_RUNNER_DOCKER_E2E") != "1" {
		t.Skip("real-docker runner e2e: set SPINDLE_RUNNER_DOCKER_E2E=1 to run")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// dockerRun starts a detached idle container with the job workspace path and
// removes it on cleanup.
func dockerRun(t *testing.T, name, image string) {
	t.Helper()
	argv := []string{"run", "-d", "--name", name, "--entrypoint", "/bin/sh", image,
		"-c", "mkdir -p " + DefaultWorkspace + "; while :; do sleep 3600; done"}
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", argv...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v (%s)", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if out, err := exec.Command("docker", "exec", name, "sh", "-c", "command -v setsid && true").CombinedOutput(); err != nil {
		t.Fatalf("the test image lacks setsid: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func TestDockerBackgroundTreeKilledOnTimeout(t *testing.T) {
	dockerE2E(t)
	job := bgJob(threads.StepSpec{ID: "tree", Name: "tree", Run: "sleep 300 & sleep 300", Background: true})
	p, err := BuildPlan(PlanInput{Job: job, DefaultImage: "alpine/git:latest"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	dockerRun(t, p.Names.Container, "alpine/git:latest")
	e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine/git:latest"}
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	p.Steps[0].Timeout = 300 * time.Millisecond
	out, _ := e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	if out.Conclusion != "running" {
		t.Fatalf("placeholder = %+v", out)
	}
	res := bg.lookup("tree").wait(context.Background())
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "timed out") {
		t.Fatalf("timeout result = %+v", res)
	}
	// The whole process tree (the shell AND its backgrounded child) is gone.
	out2, err := exec.Command("docker", "exec", p.Names.Container, "sh", "-c", "ps | grep '[s]leep 300' || true").CombinedOutput()
	if err != nil {
		t.Fatalf("ps: %v (%s)", err, strings.TrimSpace(string(out2)))
	}
	if strings.Contains(string(out2), "sleep 300") {
		t.Fatalf("a process survived the group kill:\n%s", out2)
	}
}

func TestDockerParallelPairOverlaps(t *testing.T) {
	dockerE2E(t)
	// Each member waits for the other's marker: they only both succeed when they
	// actually run at the same time (a sequential run times out and fails).
	rendezvous := func(mine, theirs string) string {
		return "touch " + mine + ".started; i=0; while [ ! -f " + theirs + ".started ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done; [ -f " + theirs + ".started ]"
	}
	job := bgJob(threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
		{Name: "a", Run: rendezvous("a", "b")},
		{Name: "b", Run: rendezvous("b", "a")},
	}})
	p, err := BuildPlan(PlanInput{Job: job, DefaultImage: "alpine/git:latest"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	dockerRun(t, p.Names.Container, "alpine/git:latest")
	e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: "alpine/git:latest"}
	stream, _ := recordingStream(t)
	out, inner := e.runStep(context.Background(), p, p.Steps[0], stream, nil, newStepOutcomes(), nil, false, newCommandFiles(), newJobCommands(stream), &jobSummary{})
	stream.Close()
	if out.Conclusion != threads.JobSuccess {
		t.Fatalf("the members did not overlap: group = %+v", out)
	}
	if len(inner) != 2 || inner[0].Name != "a" || inner[1].Name != "b" {
		t.Fatalf("member results = %+v", inner)
	}
}

func TestDockerDaemonlessBuildWithKaniko(t *testing.T) {
	dockerE2E(t)
	const image = "gcr.io/kaniko-project/executor:debug"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, perr := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); perr != nil {
			t.Skipf("kaniko image unavailable: %v (%s)", perr, strings.TrimSpace(string(out)))
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A daemonless build needs no socket: the kaniko image carries the builder,
	// and the job gets no docker endpoint (the default policy).
	job := bgJob(threads.StepSpec{Name: "build", Run: "/kaniko/executor --context=dir:///workspace --dockerfile=/workspace/Dockerfile --no-push --tar-path=/workspace/out/image.tar --destination=localhost/spindle-w3:latest --force"})
	job.Spec.Container = &threads.ContainerSpec{Image: image}
	p, err := BuildPlan(PlanInput{Job: job, DefaultImage: image})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if strings.Contains(strings.Join(p.ContainerArgv, " "), "docker.sock") || p.DockerHost != "" {
		t.Fatalf("the job container was granted a docker endpoint: %v", p.ContainerArgv)
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o777); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "run", "-d", "--name", p.Names.Container,
		"--entrypoint", "/bin/sh", "-v", dir+":/workspace", image, "-c", "mkdir -p "+DefaultWorkspace+"; while :; do sleep 3600; done").CombinedOutput(); err != nil {
		t.Fatalf("start job container: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", p.Names.Container).Run() })
	e := &Executor{Docker: &DockerCLIProcess{}, DefaultImage: image}
	stream, frames := recordingStream(t)
	out, _ := e.runStep(context.Background(), p, p.Steps[0], stream, nil, newStepOutcomes(), nil, false, newCommandFiles(), newJobCommands(stream), &jobSummary{})
	stream.Close()
	if out.Conclusion != threads.JobSuccess {
		t.Fatalf("daemonless build = %+v\n%s", out, frames())
	}
	if st, err := os.Stat(filepath.Join(dir, "out", "image.tar")); err != nil || st.Size() == 0 {
		t.Fatalf("the build produced no tar: %v", err)
	}
}

func TestDockerRegistryCredentialRedacted(t *testing.T) {
	dockerE2E(t)
	const secret = "w3-registry-password-value"
	job := bgJob(threads.StepSpec{Name: "echo", Run: "echo pw=$SPINDLE_TEST_PW"})
	job.Spec.Env = map[string]string{"SPINDLE_TEST_PW": secret}
	e := &Executor{
		Docker:       &DockerCLIProcess{},
		DefaultImage: "alpine/git:latest",
		RegistryAuth: &RegistryAuth{masks: []string{secret}},
	}
	stream, frames := recordingStream(t)
	res := e.Execute(context.Background(), ExecInput{Job: job}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	log := frames()
	if strings.Contains(log, secret) {
		t.Fatalf("the runner-owned credential reached the job log:\n%s", log)
	}
	if !strings.Contains(log, "***") {
		t.Fatalf("the credential was not masked:\n%s", log)
	}
	_ = fmt.Sprint(len(log))
}
