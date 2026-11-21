package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// DockerCLI is the subprocess boundary (law 2's shape: docker is spoken to as a
// CLI, never an SDK). Tests substitute a recorder here.
type DockerCLI interface {
	// Run executes one docker invocation and returns its output and exit code.
	// A non-zero exit code is data (docker's own failures are 125/126/127).
	Run(ctx context.Context, env []string, argv ...string) (stdout, stderr string, code int, err error)
	// Stream executes one invocation with stdout+stderr wired to w.
	Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (code int, err error)
}

// DockerCLIProcess is the real `docker` executable.
type DockerCLIProcess struct {
	// Bin is the docker (or podman) binary; "docker" when empty.
	Bin string
}

func (d *DockerCLIProcess) bin() string {
	if d != nil && strings.TrimSpace(d.Bin) != "" {
		return d.Bin
	}
	return "docker"
}

// Run implements DockerCLI.
func (d *DockerCLIProcess) Run(ctx context.Context, env []string, argv ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, d.bin(), argv...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		code = exitCode(err)
	}
	return out.String(), errb.String(), code, err
}

// Stream implements DockerCLI.
func (d *DockerCLIProcess) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	cmd := exec.CommandContext(ctx, d.bin(), argv...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err != nil {
		return exitCode(err), err
	}
	return 0, nil
}

// exitCode extracts a process exit code (128+n for a signal).
func exitCode(err error) int {
	var ee *exec.ExitError
	if ok := asExit(err, &ee); ok {
		return ee.ExitCode()
	}
	return -1
}

// asExit is errors.As for *exec.ExitError (kept tiny so exec.go's imports stay
// obvious).
func asExit(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Executor runs one planned job against a docker daemon.
type Executor struct {
	Docker       DockerCLI
	DefaultImage string
	WorkspaceDir string
	ServerURL    string
	Logger       *slog.Logger
	// ServiceReadyTimeout bounds the sidecar readiness wait (probe).
	ServiceReadyTimeout time.Duration
}

// ExecInput is one claimed job.
type ExecInput struct {
	Job          *threads.FetchResult
	AttemptToken string
}

// Result is the job's conclusion and per-step outcomes.
type Result struct {
	Conclusion string // success | failure | cancelled
	Steps      []threads.StepResult
	Error      string
}

func (e *Executor) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Execute runs one job to completion. Containers, services, the network and the
// volume are removed on EVERY path (success, failure, cancel, timeout, panic):
// defer cleanup runs with a fresh timeout context so a cancelled job context
// cannot strand a container.
func (e *Executor) Execute(ctx context.Context, in ExecInput, stream *Streamer) (res Result) {
	res.Conclusion = threads.JobSuccess
	plan, err := BuildPlan(PlanInput{
		Job:          in.Job,
		AttemptToken: in.AttemptToken,
		DefaultImage: e.DefaultImage,
		WorkspaceDir: e.WorkspaceDir,
		ServerURL:    e.ServerURL,
	})
	if err != nil {
		res.Conclusion = threads.JobFailure
		res.Error = err.Error()
		stream.Systemf("spindle: cannot plan job: %v", err)
		return res
	}
	defer func() {
		if r := recover(); r != nil {
			res.Conclusion = threads.JobFailure
			res.Error = fmt.Sprintf("runner panic: %v", r)
			stream.Systemf("spindle: runner panic: %v", r)
			e.log().Error("threads runner: panic in job", "job", plan.JobRef, "panic", r)
		}
		e.cleanup(plan)
	}()

	jobCtx := ctx
	if plan.Timeout > 0 {
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithTimeout(ctx, plan.Timeout)
		defer cancel()
	}
	stream.Systemf("spindle: job %s (%s) attempt %d on %s", in.Job.JobID, in.Job.Context, plan.Attempt, plan.Image)

	if err := e.setup(jobCtx, plan, stream); err != nil {
		res.Conclusion = e.failOrCancel(jobCtx, stream, err)
		res.Error = err.Error()
		return res
	}

	anyFailed := false
	hardFailure := false
	for i := range plan.Steps {
		step := plan.Steps[i]
		if jobCtx.Err() != nil || stream.Cancelled() {
			res.Steps = append(res.Steps, threads.StepResult{
				Number: step.Index + 1, Name: step.Name, Conclusion: threads.JobCancelled,
			})
			continue
		}
		outcome := e.runStep(jobCtx, plan, step, stream, res.Steps, hardFailure)
		res.Steps = append(res.Steps, outcome)
		switch outcome.Conclusion {
		case threads.JobFailure:
			anyFailed = true
			if !step.ContinueOnError {
				hardFailure = true
				// Remaining steps are skipped (recorded, so the run page shows
				// the whole list rather than a truncated one).
				for j := i + 1; j < len(plan.Steps); j++ {
					rest := plan.Steps[j]
					res.Steps = append(res.Steps, threads.StepResult{
						Number: rest.Index + 1, Name: rest.Name, Conclusion: "skipped",
					})
				}
				res.Conclusion = threads.JobFailure
				res.Error = outcome.Error
				if jobCtx.Err() != nil || stream.Cancelled() {
					res.Conclusion = threads.JobCancelled
				}
				return res
			}
		case threads.JobCancelled:
			res.Conclusion = threads.JobCancelled
			return res
		}
	}
	switch {
	case stream.Cancelled() || jobCtx.Err() != nil:
		res.Conclusion = threads.JobCancelled
	case anyFailed && hardFailure:
		res.Conclusion = threads.JobFailure
	default:
		// A continue-on-error failure keeps the job green (GitHub's semantics);
		// the step result still records the failure.
		res.Conclusion = threads.JobSuccess
	}
	return res
}

// failOrCancel classifies a setup error: a cancelled context is a cancel, not a
// failure.
func (e *Executor) failOrCancel(ctx context.Context, stream *Streamer, err error) string {
	if ctx.Err() != nil || stream.Cancelled() {
		return threads.JobCancelled
	}
	return threads.JobFailure
}

// setup creates the network, services and the job container, and waits for the
// services to be reachable.
func (e *Executor) setup(ctx context.Context, plan *Plan, stream *Streamer) error {
	if err := e.docker(ctx, stream, nil, "network", "create", plan.Names.Network); err != nil {
		return fmt.Errorf("create job network: %w", err)
	}
	if err := e.docker(ctx, stream, nil, "volume", "create", plan.Names.Volume); err != nil {
		return fmt.Errorf("create workspace volume: %w", err)
	}
	for _, svc := range plan.Services {
		stream.Systemf("spindle: starting service %s (%s)", svc.ID, svc.Name)
		if err := e.docker(ctx, stream, nil, svc.Argv...); err != nil {
			return fmt.Errorf("create service %s: %w", svc.ID, err)
		}
		if err := e.docker(ctx, stream, nil, "start", svc.Name); err != nil {
			return fmt.Errorf("start service %s: %w", svc.ID, err)
		}
	}
	if err := e.docker(ctx, stream, nil, plan.ContainerArgv...); err != nil {
		return fmt.Errorf("create job container: %w", err)
	}
	if err := e.docker(ctx, stream, nil, "start", plan.Names.Container); err != nil {
		return fmt.Errorf("start job container: %w", err)
	}
	for _, svc := range plan.Services {
		if err := e.waitService(ctx, svc); err != nil {
			return err
		}
	}
	return nil
}

// waitService waits until a sidecar is running/healthy and, when it declares
// ports, until each port accepts a TCP connection on its ephemeral host port.
func (e *Executor) waitService(ctx context.Context, svc ServicePlan) error {
	timeout := e.ServiceReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	health := ""
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, stderr, code, err := e.Docker.Run(ctx, nil, "inspect", "--format",
			"{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", svc.Name)
		health = strings.TrimSpace(out)
		if err == nil && code == 0 {
			switch health {
			case "unhealthy", "exited", "dead":
				return fmt.Errorf("service %s is %s: %s", svc.ID, health, strings.TrimSpace(stderr))
			case "healthy", "running":
				if len(svc.ProbePorts) == 0 {
					return nil
				}
				if e.probePorts(ctx, svc) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("service %s not ready after %s (state %q)", svc.ID, timeout, health)
}

// probePorts dials every published service port once. A publish failure (an
// image with no TCP listener) is reported by the caller's timeout.
func (e *Executor) probePorts(ctx context.Context, svc ServicePlan) bool {
	for _, port := range svc.ProbePorts {
		out, _, code, err := e.Docker.Run(ctx, nil, "port", svc.Name, port+"/tcp")
		if err != nil || code != 0 {
			return false
		}
		addr := lastPortMapping(out)
		if addr == "" {
			return false
		}
		d := net.Dialer{Timeout: 750 * time.Millisecond}
		conn, derr := d.DialContext(ctx, "tcp", addr)
		if derr != nil {
			return false
		}
		_ = conn.Close()
	}
	return true
}

// lastPortMapping picks the host address from `docker port` output
// ("127.0.0.1:49153" or "0.0.0.0:49153", one per line).
func lastPortMapping(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if i := strings.LastIndexByte(line, ':'); i > 0 {
			return "127.0.0.1" + line[i:]
		}
	}
	return ""
}

// runStep executes one step (or records its skip/unsupported failure).
func (e *Executor) runStep(ctx context.Context, plan *Plan, step StepPlan, stream *Streamer, prior []threads.StepResult, hardFailure bool) threads.StepResult {
	out := threads.StepResult{Number: step.Index + 1, Name: step.Name}
	cond, err := EvalIf(step.If, e.exprContext(plan, hardFailure, stream))
	if err != nil {
		out.Conclusion = threads.JobFailure
		out.Error = err.Error()
		stream.Systemf("spindle: step %q: %v", step.Name, err)
		return out
	}
	if !cond {
		out.Conclusion = "skipped"
		stream.Systemf("spindle: step %q skipped (if: %s)", step.Name, step.If)
		return out
	}
	if step.Kind == "unsupported" {
		out.Conclusion = threads.JobFailure
		out.Error = step.SkipReason
		stream.Systemf("spindle: step %q: %s", step.Name, step.SkipReason)
		return out
	}
	stream.StepStart(step)
	started := time.Now()
	if err := e.runStepCommand(ctx, plan, step, stream); err != nil {
		out.DurationMS = time.Since(started).Milliseconds()
		if ctx.Err() != nil || stream.Cancelled() {
			out.Conclusion = threads.JobCancelled
			stream.StepEnd(step, threads.JobCancelled, -1, out.DurationMS)
			return out
		}
		out.Conclusion = threads.JobFailure
		out.ExitCode = stepExitCode(err)
		out.Error = stream.Mask(err.Error())
		stream.StepEnd(step, threads.JobFailure, out.ExitCode, out.DurationMS)
		return out
	}
	out.DurationMS = time.Since(started).Milliseconds()
	out.Conclusion = threads.JobSuccess
	stream.StepEnd(step, threads.JobSuccess, 0, out.DurationMS)
	return out
}

// stepError carries the docker exit code through the error path.
type stepError struct {
	code int
	msg  string
}

func (s *stepError) Error() string { return s.msg }

func stepExitCode(err error) int {
	if se, ok := err.(*stepError); ok {
		return se.code
	}
	return -1
}

// runStepCommand executes the step argv with its timeout. A step timeout
// terminates the step's process tree by killing the job container (the only
// reliable in-container kill), which fails the job. Secret values and the
// checkout token travel in the docker client's environment (`--env NAME`), never
// in argv.
func (e *Executor) runStepCommand(ctx context.Context, plan *Plan, step StepPlan, stream *Streamer) error {
	stepCtx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}
	fullEnv := make([]string, 0, len(plan.Secrets)+1)
	for name, value := range plan.Secrets {
		fullEnv = append(fullEnv, name+"="+value)
	}
	if containsString(step.EnvFrom, "SPINDLE_JOB_TOKEN") {
		fullEnv = append(fullEnv, "SPINDLE_JOB_TOKEN="+plan.JobToken)
	}
	code, err := e.Docker.Stream(stepCtx, fullEnv, stream, step.Argv...)
	if stepCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		// The step timed out: kill the container so the in-container process
		// cannot outlive it, then report the timeout.
		killCtx, killCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _, _, _ = e.Docker.Run(killCtx, nil, "kill", plan.Names.Container)
		killCancel()
		return &stepError{code: -1, msg: fmt.Sprintf("timed out after %s", step.Timeout)}
	}
	if err != nil {
		msg := fmt.Sprintf("exit code %d", code)
		if code < 0 {
			msg = err.Error()
		}
		return &stepError{code: code, msg: msg}
	}
	if code != 0 {
		return &stepError{code: code, msg: fmt.Sprintf("exit code %d", code)}
	}
	return nil
}

// exprContext builds the `if` context for one step. hardFailure reports whether
// an earlier step failed WITHOUT continue-on-error (success()/failure() follow
// the job status, which a tolerated failure does not change).
func (e *Executor) exprContext(plan *Plan, hardFailure bool, stream *Streamer) ExprContext {
	github := map[string]string{
		"ref":        plan.Env["GITHUB_REF"],
		"ref_name":   plan.Env["GITHUB_REF_NAME"],
		"sha":        plan.Env["GITHUB_SHA"],
		"event_name": plan.Env["GITHUB_EVENT_NAME"],
		"workflow":   plan.Env["GITHUB_WORKFLOW"],
		"job":        plan.Env["GITHUB_JOB"],
		"repository": plan.Env["GITHUB_REPOSITORY"],
		"run_id":     plan.Env["GITHUB_RUN_ID"],
	}
	return ExprContext{
		Success: !hardFailure, Failure: hardFailure, Cancelled: stream.Cancelled(),
		GitHub: github, Matrix: plan.Matrix, Inputs: plan.Inputs, Env: plan.Env,
	}
}

// docker runs one docker invocation and records a non-zero exit as an error
// with its output (so a failed create explains itself in the log).
func (e *Executor) docker(ctx context.Context, stream *Streamer, env []string, argv ...string) error {
	stdout, stderr, code, err := e.Docker.Run(ctx, env, argv...)
	if err != nil && code == 0 {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		return fmt.Errorf("docker %s: exit %d: %s", strings.Join(argv, " "), code, msg)
	}
	return nil
}

// cleanup removes every object this job created, on every path. It never uses
// the (possibly cancelled) job context: a fresh timeout is the point.
func (e *Executor) cleanup(plan *Plan) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, name := range append([]string{plan.Names.Container}, serviceNames(plan)...) {
		_, _, _, _ = e.Docker.Run(ctx, nil, "rm", "-f", name)
	}
	_, _, _, _ = e.Docker.Run(ctx, nil, "network", "rm", plan.Names.Network)
	_, _, _, _ = e.Docker.Run(ctx, nil, "volume", "rm", "-f", plan.Names.Volume)
}

// serviceNames lists the sidecar container names in declaration order.
func serviceNames(plan *Plan) []string {
	out := make([]string, 0, len(plan.Services))
	for _, svc := range plan.Services {
		out = append(out, svc.Name)
	}
	return out
}

// containsString reports membership in a small slice.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
