package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// quietStream is a Streamer whose frames go to a stub server (the executor
// tests care about docker argv and step outcomes, not the wire).
func quietStream(t *testing.T) *Streamer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"running","cancel":false}`))
	}))
	t.Cleanup(srv.Close)
	s := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_x", 1, nil, func() {}, nil)
	t.Cleanup(s.Close)
	return s
}

// recordingStream is a Streamer whose frames are captured, for assertions on
// the runner-emitted log lines (the checkout base at job start).
func recordingStream(t *testing.T) (*Streamer, func() string) {
	t.Helper()
	var mu sync.Mutex
	var b strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var frame struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&frame)
		mu.Lock()
		b.WriteString(frame.Text)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"running","cancel":false}`))
	}))
	t.Cleanup(srv.Close)
	s := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_x", 1, nil, func() {}, nil)
	return s, func() string {
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

func executorWith(docker DockerCLI) *Executor {
	return &Executor{Docker: docker, DefaultImage: "alpine/git:latest", ServiceReadyTimeout: 2 * time.Second}
}

func TestExecuteLogsTheCheckoutBaseAtJobStart(t *testing.T) {
	docker := &fakeDocker{streamOutput: "ok\n"}
	job := fixtureJob()
	job.Spec.Services = nil
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	got := frames()
	// The fixture's origin is 127.0.0.1:8088; the container copy is
	// host.docker.internal:8088, and both are stated once at job start.
	for _, want := range []string{
		"checkout from http://host.docker.internal:8088/acme/mono.git",
		"reachable inside the job container as host.docker.internal:8088",
		"payload URL http://127.0.0.1:8088/acme/mono.git rewritten",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("job log misses %q:\n%s", want, got)
		}
	}
}

func TestExecuteHappyPathAndCleanup(t *testing.T) {
	docker := &fakeDocker{streamOutput: "test output\n"}
	exec := executorWith(docker)
	job := fixtureJob()
	job.Spec.Services = nil // keep the readiness path out of the happy test
	res := exec.Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if res.Steps[0].Conclusion != threads.JobSuccess || res.Steps[1].Conclusion != threads.JobSuccess || res.Steps[2].Conclusion != threads.JobSuccess {
		t.Fatalf("step outcomes = %+v", res.Steps)
	}
	// Every object is removed on the success path too.
	for _, want := range []string{"network create", "volume create", "create --name sp-j-", "start sp-j-", "rm -f sp-j-", "network rm", "volume rm -f"} {
		if !docker.called(want) {
			t.Errorf("docker never ran %q\ncalls: %v", want, docker.snapshot())
		}
	}
	// The checkout step's token travelled in the docker client's env, not argv.
	found := false
	for i, call := range docker.snapshot() {
		if strings.Contains(call, "credential.helper") {
			for _, kv := range docker.envs[i] {
				if kv == "SPINDLE_JOB_TOKEN=wjt_token_value" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("the checkout token was not supplied through the docker client environment")
	}
	// Secrets likewise: env array, never argv.
	for _, call := range docker.snapshot() {
		if strings.Contains(call, "s3cr3t-value") {
			t.Fatalf("secret value in argv: %s", call)
		}
	}
}

func TestExecuteCleanupOnSetupFailure(t *testing.T) {
	docker := &fakeDocker{}
	docker.runOutputs = []runOutput{{match: "network create", code: 125}}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: fixtureJob(), AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "create job network") {
		t.Fatalf("res = %+v", res)
	}
	if !docker.called("rm -f sp-j-") || !docker.called("network rm") || !docker.called("volume rm") {
		t.Fatalf("cleanup did not run on the setup-failure path: %v", docker.snapshot())
	}
}

func TestExecuteCleanupOnPanic(t *testing.T) {
	docker := &panicDocker{}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: fixtureJob(), AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "panic") {
		t.Fatalf("res = %+v", res)
	}
	if !docker.called("rm -f sp-j-") {
		t.Fatalf("cleanup did not run after a panic: %v", docker.snapshot())
	}
}

// panicDocker panics on the first non-create call (after setup started) so the
// deferred cleanup is exercised.
type panicDocker struct {
	fakeDocker
	mu   sync.Mutex
	sent bool
}

func (p *panicDocker) Run(ctx context.Context, env []string, argv ...string) (string, string, int, error) {
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "start") {
		p.mu.Lock()
		p.sent = true
		p.mu.Unlock()
		panic("boom")
	}
	return p.fakeDocker.Run(ctx, env, argv...)
}

func (p *panicDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	return 0, nil
}

func TestExecuteFailingStepSkipsTheRestAndFails(t *testing.T) {
	docker := &fakeDocker{streamCode: 1, streamOutput: "boom\n"}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "first", Run: "exit 1"},
		{Name: "second", Run: "echo never"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if len(res.Steps) != 2 || res.Steps[0].Conclusion != threads.JobFailure || res.Steps[0].ExitCode != 1 || res.Steps[1].Conclusion != "skipped" {
		t.Fatalf("steps = %+v", res.Steps)
	}
}

func TestExecuteContinueOnErrorKeepsJobGreen(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "exit 1", code: 1}}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "tolerated", Run: "exit 1", ContinueOnError: true},
		{Name: "after", Run: "echo ok", If: "success()"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (steps %+v)", res.Conclusion, res.Steps)
	}
	if res.Steps[0].Conclusion != threads.JobFailure || res.Steps[1].Conclusion != threads.JobSuccess {
		t.Fatalf("steps = %+v", res.Steps)
	}
}

func TestExecuteIfSkipsAndAlwaysRunsAfterFailure(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "exit 2", code: 2}}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "not-main", Run: "echo x", If: "github.ref == 'refs/heads/dev'"},
		{Name: "bad", Run: "exit 2", ContinueOnError: true},
		{Name: "on-always", Run: "echo cleanup", If: "always()"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Steps[0].Conclusion != "skipped" {
		t.Fatalf("if did not skip: %+v", res.Steps[0])
	}
	if res.Steps[2].Conclusion != threads.JobSuccess {
		t.Fatalf("always() step did not run after a tolerated failure: %+v", res.Steps[2])
	}
}

func TestExecuteCancelMidStep(t *testing.T) {
	d := &fakeDocker{blockOn: "exec --workdir"}
	ctx, cancel := context.WithCancel(context.Background())
	job := fixtureJob()
	job.Spec.Services = nil
	stream := quietStream(t)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res := executorWith(d).Execute(ctx, ExecInput{Job: job, AttemptToken: "t"}, stream)
	if res.Conclusion != threads.JobCancelled {
		t.Fatalf("conclusion = %s (steps %+v)", res.Conclusion, res.Steps)
	}
	if !d.called("rm -f sp-j-") {
		t.Fatalf("cleanup did not run on the cancel path: %v", d.snapshot())
	}
}

func TestStepTimeoutKillsTheContainer(t *testing.T) {
	d := &fakeDocker{blockOn: "exec --workdir"}
	exec := executorWith(d)
	stream := quietStream(t)
	plan, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[1]
	step.Timeout = 80 * time.Millisecond
	err = exec.runStepCommand(context.Background(), plan, step, stream, stream)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if se, ok := err.(*stepError); !ok || !se.timedOut {
		t.Fatalf("a step timeout must carry the timed_out marker, got %#v", err)
	}
	if !d.called("kill " + plan.Names.Container) {
		t.Fatalf("a timed-out step must kill its container: %v", d.snapshot())
	}
}

func TestServiceReadinessProbe(t *testing.T) {
	// The probe polls `docker port` then dials; with no listening port it gives
	// up after the (short) readiness timeout and fails the job.
	d := &fakeDocker{}
	d.runOutputs = []runOutput{
		{match: "inspect", out: "running"},
		{match: "port", out: "127.0.0.1:1"}, // nothing listens on ::1:1
	}
	exec := executorWith(d)
	exec.ServiceReadyTimeout = 300 * time.Millisecond
	res := exec.Execute(context.Background(), ExecInput{Job: fixtureJob(), AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "not ready") {
		t.Fatalf("res = %+v", res)
	}
	if !d.called("rm -f " + "sp-s-") {
		t.Fatalf("services were not cleaned up: %v", d.snapshot())
	}
}

func TestLastPortMapping(t *testing.T) {
	if got := lastPortMapping("0.0.0.0:1234\n127.0.0.1:5678\n"); got != "127.0.0.1:5678" {
		t.Fatalf("lastPortMapping = %q", got)
	}
	if got := lastPortMapping(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}
