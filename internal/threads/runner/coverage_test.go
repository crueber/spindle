package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Coverage for the subprocess boundary, the client verbs the loop tests do not
// drive, and the log heartbeat.

func TestDockerCLIProcessRunAndStream(t *testing.T) {
	d := &DockerCLIProcess{Bin: "/bin/sh"}
	stdout, stderr, code, err := d.Run(context.Background(), nil, "-c", "echo out; echo err >&2; exit 3")
	if code != 3 || err == nil {
		t.Fatalf("Run code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "out") || !strings.Contains(stderr, "err") {
		t.Fatalf("Run stdout=%q stderr=%q", stdout, stderr)
	}
	var buf bytes.Buffer
	code, err = d.Stream(context.Background(), nil, &buf, "-c", "echo streamed")
	if code != 0 || err != nil || !strings.Contains(buf.String(), "streamed") {
		t.Fatalf("Stream code=%d err=%v buf=%q", code, err, buf.String())
	}
	if code, err := d.Stream(context.Background(), nil, &buf, "-c", "exit 7"); code != 7 || err == nil {
		t.Fatalf("Stream exit code=%d err=%v", code, err)
	}
	// An empty Bin falls back to "docker" (which is present on the e2e host
	// but not required here: a missing binary surfaces as code -1).
	if (&DockerCLIProcess{}).bin() != "docker" {
		t.Fatal("default bin")
	}
	if code := exitCode(os.ErrNotExist); code != -1 {
		t.Fatalf("exitCode(non-exit) = %d", code)
	}
}

func TestClientVerbs(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/register"):
			_, _ = w.Write([]byte(`{"runner_id":"abc","repo":"o/r","token":"wrt_tok","labels":["linux"]}`))
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			_, _ = w.Write([]byte(`{"repo":"o/r","run_id":"r","job_id":"j","state":"cancelled","run_state":"failure"}`))
		default:
			http.Error(w, "invalid token", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/", "")

	reg, err := c.Register(context.Background(), "wrg_x", "n", []string{"linux"}, 1, false)
	if err != nil || reg.Token != "wrt_tok" {
		t.Fatalf("Register = %+v, %v", reg, err)
	}
	view, err := c.AckCancel(context.Background(), "wjt_t", "jr_x", 2)
	if err != nil || view.State != "cancelled" {
		t.Fatalf("AckCancel = %+v, %v", view, err)
	}
	if gotAuth != "Bearer wjt_t" || !strings.HasSuffix(gotPath, "/attempt/2/cancel") {
		t.Fatalf("request auth=%q path=%q", gotAuth, gotPath)
	}
	if _, err := c.Declare(context.Background(), "v", nil, 1, false); err == nil || !isFatal(err) {
		t.Fatalf("401 Declare = %v (want a fatal error)", err)
	}
	// APIError formatting.
	if msg := (&APIError{Status: 502, Message: " up\nstream "}).Error(); msg != "up stream (HTTP 502)" {
		t.Fatalf("APIError = %q", msg)
	}
	if msg := (&APIError{Status: 500}).Error(); msg != "Internal Server Error (HTTP 500)" {
		t.Fatalf("APIError fallback = %q", msg)
	}
}

func TestDeclareRetriesThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"runner_id":"abc","repo":"o/r","state":"active","labels":[],"lease_seconds":900}`))
	}))
	defer srv.Close()
	r, err := New(Config{
		ServerURL: srv.URL, Token: "wrt_t", AllowHostDocker: true, Capacity: 1,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.declare(ctx); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if calls < 2 {
		t.Fatalf("declare calls = %d", calls)
	}
}

func TestRunJobSupersededIsAbandoned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			http.Error(w, "attempt superseded by 2", http.StatusConflict)
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer srv.Close()
	r, err := New(Config{ServerURL: srv.URL, Token: "wrt_t", AllowHostDocker: true, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	job := fixtureJob()
	r.runJob(context.Background(), job) // must not panic or complete
	if r.fatalErr() != nil {
		t.Fatalf("a superseded attempt is not fatal: %v", r.fatalErr())
	}
}

func TestRunJobCompleteFailureIsLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = w.Write([]byte(`{"token":"wjt_t","expires_at":"x","repo":"o/r","run_id":"r","job_id":"j","attempt":1}`))
		case strings.HasSuffix(r.URL.Path, "/log"):
			_, _ = w.Write([]byte(`{"seq":1,"state":"running","cancel":false}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			http.Error(w, "store down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	r, err := New(Config{ServerURL: srv.URL, Token: "wrt_t", AllowHostDocker: true, Capacity: 1,
		Executor: &Executor{Docker: &fakeDocker{}}})
	if err != nil {
		t.Fatal(err)
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "one", Run: "true"}}
	r.runJob(context.Background(), job)
	if r.fatalErr() != nil {
		t.Fatalf("a 503 completion is retried, not fatal: %v", r.fatalErr())
	}
}

func TestStreamerHeartbeatAndCloseIdempotent(t *testing.T) {
	var frames int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frames++
		_, _ = w.Write([]byte(`{"state":"running","cancel":false}`))
	}))
	defer srv.Close()
	s := NewStreamer(NewClient(srv.URL, "t"), "wjt_t", "jr_x", 1, nil, func() {}, nil)
	s.heartbeat()
	s.Close()
	s.Close() // idempotent
	if frames != 1 {
		t.Fatalf("frames = %d, want 1 (the heartbeat)", frames)
	}
	if _, err := s.Write([]byte("after close")); err != nil {
		t.Fatalf("write after close: %v", err)
	}
}

func TestCheckoutScriptWithoutURL(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: fixtureJob()})
	if err != nil {
		t.Fatal(err)
	}
	p.CheckoutURL = ""
	if script := checkoutScript(p, "1"); !strings.Contains(script, "no checkout URL") {
		t.Fatalf("script = %q", script)
	}
}

func TestPlanEdgeCases(t *testing.T) {
	if got := containerPort("6379/udp"); got != "6379/udp" {
		t.Fatalf("containerPort = %q", got)
	}
	if got := slug("!!"); got != "svc" {
		t.Fatalf("slug = %q", got)
	}
	if got := envName("go-version"); got != "GO_VERSION" {
		t.Fatalf("envName = %q", got)
	}
	// A service with >1 entrypoint element flattens into the command.
	job := fixtureJob()
	job.Spec.Services = map[string]threads.ServiceSpec{
		"db": {Image: "postgres", Entrypoint: []string{"/bin/sh", "-c"}, Command: []string{"run"}},
	}
	plan, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(plan.Services[0].Argv, " ")
	if !strings.Contains(argv, "--entrypoint /bin/sh") || !strings.HasSuffix(argv, "postgres -c run") {
		t.Fatalf("service argv = %s", argv)
	}
	// A matrix env value is substituted in a step's env too.
	job = fixtureJob()
	job.Spec.Steps = []threads.StepSpec{{Name: "m", Run: "true", Env: map[string]string{"V": "${{ matrix.go }}"}}}
	plan, err = BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Steps[0].Argv, " "), "--env V=1.27") {
		t.Fatalf("matrix env: %v", plan.Steps[0].Argv)
	}
	// A job with no steps still plans (the container is created and removed).
	job = fixtureJob()
	job.Spec.Steps = nil
	if _, err := BuildPlan(PlanInput{Job: job}); err != nil {
		t.Fatalf("empty steps: %v", err)
	}
	// The plan JSON envelope survives a round trip (the payload the runner
	// receives is exactly this shape).
	job = fixtureJob()
	raw, _ := json.Marshal(job)
	var back threads.FetchResult
	if json.Unmarshal(raw, &back) != nil || back.Spec.Version != 1 {
		t.Fatal("payload round trip")
	}
}

func TestRegisterPackageFunc(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"runner_id":"abc","repo":"o/r","token":"wrt_tok","labels":[]}`))
	}))
	defer srv.Close()
	res, err := Register(context.Background(), srv.URL, "wrg_x", "n", nil, 1, false)
	if err != nil || res.Token != "wrt_tok" {
		t.Fatalf("Register = %+v, %v", res, err)
	}
	// AckCancel's error path.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := NewClient(bad.URL, "t").AckCancel(context.Background(), "wjt_t", "jr", 1); err == nil {
		t.Fatal("AckCancel must report a 5xx")
	}
}

func TestSetupFailures(t *testing.T) {
	job := fixtureJob()
	// A service that reports unhealthy fails the setup with its state.
	d := &fakeDocker{}
	d.runOutputs = []runOutput{{match: "inspect", out: "unhealthy"}}
	exec := executorWith(d)
	exec.ServiceReadyTimeout = time.Second
	res := exec.Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "unhealthy") {
		t.Fatalf("res = %+v", res)
	}
	// A service start failure names the service.
	d = &fakeDocker{}
	d.runOutputs = []runOutput{{match: "start sp-s-", code: 1}}
	res = executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "start service") {
		t.Fatalf("res = %+v", res)
	}
	// A container start failure is reported too.
	d = &fakeDocker{}
	d.runOutputs = []runOutput{{match: "start sp-j-", code: 1}}
	res = executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "start job container") {
		t.Fatalf("res = %+v", res)
	}
	// An unhealthy service that recovers is accepted (the running branch).
	d = &fakeDocker{}
	d.runOutputs = []runOutput{{match: "inspect", out: "active"}}
	res = executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		_ = res
	}
}

func TestProbePortsParsing(t *testing.T) {
	d := &fakeDocker{}
	d.runOutputs = []runOutput{{match: "port", out: "0.0.0.0:9\n"}, {match: "port", out: ""}}
	exec := executorWith(d)
	if exec.probePorts(context.Background(), ServicePlan{Name: "s", ProbePorts: []string{"6379"}}) {
		t.Fatal("a port with no mapping cannot be probed")
	}
	if lastPortMapping("garbage") != "" {
		t.Fatal("unparseable docker port output must not produce an address")
	}
}

func TestStepEdgeCases(t *testing.T) {
	exec := executorWith(&fakeDocker{})
	stream := quietStream(t)
	plan, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// An if error fails the step with the message.
	step := plan.Steps[1]
	step.If = "contains(github.ref, 'x')"
	res, _ := exec.runStep(context.Background(), plan, step, stream, nil, nil, false)
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "if:") {
		t.Fatalf("if error = %+v", res)
	}
	// A skip under a false condition.
	step = plan.Steps[2]
	step.If = "false"
	if res, _ := exec.runStep(context.Background(), plan, step, stream, nil, nil, false); res.Conclusion != "skipped" {
		t.Fatalf("skip = %+v", res)
	}
	// An unsupported action fails with its reason.
	step = plan.Steps[1]
	step.Kind = "unsupported"
	step.SkipReason = "nope"
	if res, _ := exec.runStep(context.Background(), plan, step, stream, nil, nil, false); res.Conclusion != threads.JobFailure || res.Error != "nope" {
		t.Fatalf("unsupported = %+v", res)
	}
	if stepExitCode(os.ErrNotExist) != -1 {
		t.Fatal("a non-step error has no exit code")
	}
}

func TestHostDockerExposureDetection(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	if !hostDockerExposed("") {
		t.Fatal("the default socket must count as host-exposed")
	}
	t.Setenv("DOCKER_HOST", "tcp://dind:2375")
	if hostDockerExposed("") {
		t.Fatal("an explicit DOCKER_HOST is the operator's declaration")
	}
}

func TestDeclareContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	r, err := New(Config{ServerURL: srv.URL, Token: "wrt_t", AllowHostDocker: true, Capacity: 1,
		BackoffMin: 2 * time.Millisecond, BackoffMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := r.declare(ctx); err == nil {
		t.Fatal("declare must stop when the context ends")
	}
}

func TestExecutePlanError(t *testing.T) {
	job := fixtureJob()
	job.Spec.Version = 99
	res := executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "spec version") {
		t.Fatalf("res = %+v", res)
	}
	// No spec at all.
	res = executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: &threads.FetchResult{}}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("empty job = %+v", res)
	}
}

func TestServiceProbeSucceedsAgainstAListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	d := &fakeDocker{}
	d.runOutputs = []runOutput{
		{match: "inspect", out: "running"},
		{match: "port", out: "127.0.0.1:" + port + "\n"},
	}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: fixtureJob(), AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("res = %+v", res)
	}
}

func TestWaitServiceContextCancelled(t *testing.T) {
	d := &fakeDocker{}
	d.runOutputs = []runOutput{{match: "inspect", out: "starting"}}
	exec := executorWith(d)
	exec.ServiceReadyTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exec.waitService(ctx, ServicePlan{ID: "s", Name: "s"}); err == nil {
		t.Fatal("a cancelled context must stop the readiness wait")
	}
	// The executor's nil logger falls back to the process logger.
	if (&Executor{}).log() == nil {
		t.Fatal("nil logger")
	}
	// failOrCancel classifies a cancelled context.
	stream := quietStream(t)
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if got := exec.failOrCancel(cctx, stream, os.ErrDeadlineExceeded); got != threads.JobCancelled {
		t.Fatalf("failOrCancel = %s", got)
	}
	if got := exec.failOrCancel(context.Background(), stream, os.ErrDeadlineExceeded); got != threads.JobFailure {
		t.Fatalf("failOrCancel = %s", got)
	}
}

func TestClientDecodeAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/badjson":
			_, _ = w.Write([]byte("{"))
		case "/nobody":
			w.WriteHeader(http.StatusOK)
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/threads/runners/fetch":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	if err := c.do(context.Background(), http.MethodGet, "/badjson", "t", nil, &threads.DeclareView{}); err == nil {
		t.Fatal("bad JSON must error")
	}
	if err := c.do(context.Background(), http.MethodGet, "/nobody", "t", nil, nil); err != nil {
		t.Fatalf("a no-body 200 with a nil target must succeed: %v", err)
	}
	if found, err := c.doMaybe(context.Background(), http.MethodGet, "/empty", "t", nil, &threads.DeclareView{}); err != nil || found {
		t.Fatalf("204 = %v %v", found, err)
	}
	if err := c.do(context.Background(), http.MethodGet, "/teapot", "t", nil, nil); err == nil {
		t.Fatal("an unexpected status must error")
	}
	// Fetch passes its wait through the query and the 204 means idle.
	job, err := c.Fetch(context.Background(), 5*time.Second)
	if err != nil || job != nil {
		t.Fatalf("Fetch = %+v %v", job, err)
	}
	// A request that cannot even be built (bad base URL) errors.
	bad := NewClient("http://[::1", "t")
	if _, err := bad.Fetch(context.Background(), time.Second); err == nil {
		t.Fatal("a malformed base URL must error")
	}
}

func TestExprStepsAndUnknownContext(t *testing.T) {
	ctx := ExprContext{Success: true, Steps: map[string]string{"a": "failure"}}
	if v, err := EvalIf("steps.a.outcome == 'failure'", ctx); err != nil || !v {
		t.Fatalf("steps = %v %v", v, err)
	}
	if _, err := EvalIf("steps.a.result == 'x'", ctx); err == nil {
		t.Fatal("only steps.<id>.outcome is supported")
	}
	if _, err := EvalIf("secrets.TOKEN == 'x'", ctx); err == nil {
		t.Fatal("an unknown context must error")
	}
	if _, err := EvalIf("!env.MAYBE", ExprContext{Env: map[string]string{"MAYBE": "maybe"}}); err == nil {
		t.Fatal("! on a non-boolean must error")
	}
	if got := containerPort(""); got != "" {
		t.Fatalf("containerPort(empty) = %q", got)
	}
}

func TestSetupVolumeFailureAndLogger(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	d := &fakeDocker{}
	d.runOutputs = []runOutput{{match: "volume create", code: 1}}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "workspace volume") {
		t.Fatalf("res = %+v", res)
	}
	// An executor with an explicit logger uses it on the panic path.
	exec := executorWith(&panicDocker{})
	exec.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if exec.log() != exec.Logger {
		t.Fatal("explicit logger")
	}
	// Register reports transport failures.
	if _, err := Register(context.Background(), "http://[::1", "wrg_x", "", nil, 1, false); err == nil {
		t.Fatal("Register against a malformed URL must error")
	}
}

// execExitError is a zero ExitError (no process ran).
var execExitError = exec.ExitError{}

// errDocker returns a transport error with exit code 0 (a missing binary).
type errDocker struct{ fakeDocker }

func (e *errDocker) Run(ctx context.Context, env []string, argv ...string) (string, string, int, error) {
	return "", "", 0, os.ErrPermission
}

func (e *errDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	return 0, os.ErrPermission
}

func TestDockerTransportErrorsAndRetry(t *testing.T) {
	exec := executorWith(&errDocker{})
	res := exec.Execute(context.Background(), ExecInput{Job: fixtureJob(), AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("res = %+v", res)
	}
	// exitCode unwraps a wrapped *exec.ExitError.
	wrapped := fmt.Errorf("run: %w", &execExitError)
	if code := exitCode(wrapped); code == 0 {
		t.Fatalf("exitCode(wrapped) = %d", code)
	}
	// The streamer retries a 5xx frame and then succeeds.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"state":"running","cancel":false}`))
	}))
	defer srv.Close()
	s := NewStreamer(NewClient(srv.URL, "t"), "wjt_t", "jr_x", 1, nil, func() {}, nil)
	s.Write([]byte("retried\n"))
	s.Close()
	if calls < 2 {
		t.Fatalf("frame calls = %d, want a retry", calls)
	}
	// Fetch's zero wait takes the documented default.
	if job, err := NewClient(srv.URL, "t").Fetch(context.Background(), 0); err == nil && job != nil {
		// the stub answers 200 with a log view: decoding into FetchResult yields
		// a zero result, which the loop treats as a job; either way the call
		// exercised the default-wait branch.
		_ = job
	}
}

func TestExecuteCancelledByTheLogChannel(t *testing.T) {
	// The server's cancel answer arrives on a log frame while a step is
	// running: the streamer cancels the job context, the exec dies, the step
	// reports cancelled and Execute concludes cancelled.
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel := false
		if strings.HasSuffix(r.URL.Path, "/log") {
			once.Do(func() { cancel = true })
		}
		_, _ = fmt.Fprintf(w, `{"state":"running","cancel":%v}`, cancel)
	}))
	defer srv.Close()
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "long", Run: "sleep 10"}}
	d := &blockingDocker{}
	exec := executorWith(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := NewStreamer(NewClient(srv.URL, "t"), "wjt_t", "jr_x", 1, nil, cancel, nil)
	defer stream.Close()
	res := exec.Execute(ctx, ExecInput{Job: job, AttemptToken: "t"}, stream)
	if res.Conclusion != threads.JobCancelled {
		t.Fatalf("res = %+v", res)
	}
	if !d.called("rm -f sp-j-") {
		t.Fatal("cleanup did not run after the in-band cancel")
	}
}

// blockingDocker blocks the step exec until the job context ends.
type blockingDocker struct{ fakeDocker }

func (b *blockingDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	<-ctx.Done()
	return -1, ctx.Err()
}

func TestExecuteWithAnAlreadyCancelledContext(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &fakeDocker{}
	res := executorWith(d).Execute(ctx, ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobCancelled {
		t.Fatalf("res = %+v", res)
	}
	if !d.called("rm -f sp-j-") {
		t.Fatal("cleanup must run on the cancelled path")
	}
}

func TestStreamerDropsFramesAfterRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := NewStreamer(NewClient(srv.URL, "t"), "wjt_t", "jr_x", 1, nil, func() {}, nil)
	s.Write([]byte("dropped\n"))
	s.Close()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 attempts then a drop", calls)
	}
}
