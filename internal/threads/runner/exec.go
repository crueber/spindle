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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// DockerCLI is the subprocess boundary (law 2's shape: docker is spoken to as a
// CLI, never an SDK). Tests substitute a recorder here.
type DockerCLI interface {
	// Run executes one docker invocation and returns its output and exit code.
	// A non-zero exit code is data (docker's own failures are 125/126/127).
	Run(ctx context.Context, env []string, argv ...string) (stdout, stderr string, code int, err error)
	// Stream executes one invocation with stdout+stderr wired to w.
	Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (code int, err error)
	// StreamIn is Stream with stdin wired to r (the artifact transfer: a tar
	// archive out of or into the job container).
	StreamIn(ctx context.Context, env []string, r io.Reader, w io.Writer, argv ...string) (code int, err error)
}

// DockerCLIProcess is the real `docker` executable.
type DockerCLIProcess struct {
	// Bin is the docker (or podman) binary; "docker" when empty.
	Bin string
	// Env is the base environment added to every invocation (the runner's
	// private DOCKER_CONFIG, PAR-078). Per-call env is appended after it.
	Env []string
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
	cmd.Env = append(append(os.Environ(), d.Env...), env...)
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
	return d.StreamIn(ctx, env, nil, w, argv...)
}

// StreamIn implements DockerCLI.
func (d *DockerCLIProcess) StreamIn(ctx context.Context, env []string, r io.Reader, w io.Writer, argv ...string) (int, error) {
	cmd := exec.CommandContext(ctx, d.bin(), argv...)
	cmd.Env = append(append(os.Environ(), d.Env...), env...)
	cmd.Stdin = r
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
	// RunnerName is the registered runner's name (runner.name / RUNNER_NAME).
	RunnerName string
	// Images is the runner's image-source policy (--image-prefix/-all,
	// --image-pull, --image-require-digest); zero = today's behaviour.
	Images ImageConfig
	Logger *slog.Logger
	// ServiceReadyTimeout bounds the sidecar readiness wait (probe).
	ServiceReadyTimeout time.Duration
	// Artifacts is the artifact transport (the protocol Client). Nil makes an
	// artifact step fail with a named error.
	Artifacts ArtifactClient
	// Policy is the runner operator's container grants (PAR-179). The zero
	// value is the fail-closed default.
	Policy threads.ContainerPolicy
	// RegistryAuth is the runner-owned pull identity (PAR-078); nil = use the
	// daemon's ambient docker config.
	RegistryAuth *RegistryAuth
	// Caches is the cache transport (the protocol Client). Nil makes a cache
	// step fail with a named error.
	Caches CacheClient
}

// ExecInput is one claimed job.
type ExecInput struct {
	Job          *threads.FetchResult
	AttemptToken string
	// Caps is the server's artifact cap advertisement (StartView.Artifacts);
	// zero values fall back to the service defaults.
	Caps threads.ArtifactCaps
	// CacheCaps is the server's cache limit advertisement
	// (StartView.Caches); zero values fall back to the service defaults.
	CacheCaps threads.CacheCaps
}

// Result is the job's conclusion and per-step outcomes.
type Result struct {
	Conclusion string // success | failure | cancelled
	Steps      []threads.StepResult
	// Outputs are the job's declared outputs, resolved from the collected step
	// outputs (nil for a job that did not succeed or declares none).
	Outputs map[string]string
	// Annotations are the workflow-command annotations the attempt produced
	// (PAR-093); Summary is the collected step summary markdown (PAR-092).
	Annotations []threads.Annotation
	Summary     string
	Error       string
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
		Images:       e.Images,
		ServerURL:    e.ServerURL,
		RunnerName:   e.RunnerName,
		Caps:         in.Caps,
		Policy:       e.Policy,
		CacheCaps:    in.CacheCaps,
	})
	if err != nil {
		res.Conclusion = threads.JobFailure
		res.Error = err.Error()
		stream.Systemf("spindle: cannot plan job: %v", err)
		return res
	}
	// Workflow-command annotations and the step summary live for the whole
	// attempt and are reported on EVERY return path (a hard failure still
	// carries the annotations that explain it).
	jobCmds := newJobCommands(stream)
	summary := &jobSummary{}
	// Registry credentials are the runner's, never a workflow's: their secret
	// values join the job's mask list so an accidental echo is redacted
	// (PAR-078).
	stream.addMask(e.RegistryMasks())
	var bg *backgroundSet
	// The job timeout is registered BEFORE the cleanup defer so the cleanup
	// (which kills the background processes and reports a leak) runs FIRST:
	// defer order is LIFO, and a canceled jobCtx would let a background worker
	// observe cancellation and exit before the job-end kill could prove whether
	// its process group actually stopped (D-7). The pass must see a live ctx.
	jobCtx := ctx
	if plan.Timeout > 0 {
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithTimeout(ctx, plan.Timeout)
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			res.Conclusion = threads.JobFailure
			res.Error = fmt.Sprintf("runner panic: %v", r)
			stream.Systemf("spindle: runner panic: %v", r)
			e.log().Error("threads runner: panic in job", "job", plan.JobRef, "panic", r)
		}
		// Background processes are killed at job end UNCONDITIONALLY (D-7):
		// before the containers the deferred cleanup removes.
		if bg != nil {
			failed, leaked := bg.finishAll()
			res.Steps = bg.mergeResults(res.Steps)
			if len(leaked) > 0 {
				if res.Conclusion == threads.JobSuccess {
					res.Conclusion = threads.JobFailure
				}
				res.Error = joinErrors(res.Error, strings.Join(leaked, "; "))
				stream.Systemf("spindle: %s", strings.Join(leaked, "; "))
			}
			if len(failed) > 0 {
				if res.Conclusion == threads.JobSuccess {
					res.Conclusion = threads.JobFailure
				}
				res.Error = joinErrors(res.Error, strings.Join(failed, "; "))
				stream.Systemf("spindle: %s", strings.Join(failed, "; "))
			}
		}
		res.Annotations = jobCmds.snapshot()
		res.Summary = summary.text()
		e.cleanup(plan)
	}()

	// The plan's own secret values join the stream's mask list: a substituted
	// `${{ secrets.X }}` is masked in the log exactly like the env-delivered
	// token, whatever the payload's mask list carried (docs/threads.md §3.6).
	stream.addMask(plan.Mask)
	stream.Systemf("spindle: job %s (%s) attempt %d on %s", in.Job.JobID, in.Job.Context, plan.Attempt, plan.Image)
	// The checkout base is stated once at job start: an operator debugging a
	// container that cannot resolve the origin needs the URL and its
	// host:port BEFORE the fetch fails (docs/threads.md §4.4).
	if plan.CheckoutURL != "" {
		stream.Systemf("spindle: %s", checkoutNotice(plan))
		e.log().Info("threads runner: job checkout", "job", plan.JobRef, "url", plan.CheckoutURL)
	}

	if err := e.setup(jobCtx, plan, stream); err != nil {
		res.Conclusion = e.failOrCancel(jobCtx, ctx, stream, err)
		res.Error = err.Error()
		// A setup failure (a service that never became ready, a container that
		// could not be created) must be visible in the job's own log: the
		// conclusion is reported without a free-text error, so a silent log would
		// leave the reader guessing (docs/threads.md §4.2).
		stream.Systemf("spindle: %v", err)
		return res
	}

	// hashFiles (PAR-041) walks the workspace inside the job container. The
	// cache lives for this attempt and is invalidated after every step, so two
	// calls in one step see one walk and a later step sees the files it made.
	hashCache := newHashFilesCache()
	plan.HashFiles = e.hashFilesFunc(jobCtx, plan, hashCache)

	anyFailed := false
	hardFailure := false
	steps := newStepOutcomes()
	// The job's command files ($GITHUB_ENV / $GITHUB_PATH): accumulated across
	// the steps that already ran and layered onto each later step's environment
	// (docs/threads.md §3.10). Nothing here touches the runner process.
	cmds := newCommandFiles()
	// Background/parallel bookkeeping (PAR-080): created after setup so a
	// background step can rely on the job container existing.
	bg = newBackgroundSet(e, plan, stream, steps, cmds, jobCmds, summary)
	// Action post hooks run after the main steps — including after a hard
	// failure and before the deferred cleanup (this defer is registered later,
	// so it runs first). A post hook's result can still fail the job.
	defer e.runPostSteps(jobCtx, plan, stream, steps, cmds, jobCmds, summary, &res)
	// Docker-in-job (PAR-081/D-8): state once, at the top, what "docker build
	// works" means on this job — the failure remedy for a `docker: not found`
	// exit repeats it.
	if plan.DockerHost == "" {
		stream.Systemf("spindle: no docker daemon or socket is attached to this job (D-8); use a daemonless builder in the job image (kaniko, or buildah with --storage-driver=vfs --isolation=chroot)")
	}
	for i := range plan.Steps {
		step := plan.Steps[i]
		if jobCtx.Err() != nil || stream.Cancelled() {
			state := threads.JobCancelled
			if jobTimedOut(jobCtx, ctx) {
				state = threads.JobTimedOut
			}
			outcome := threads.StepResult{
				Number: step.Index + 1, ID: step.ID, Name: step.Name, Conclusion: state,
			}
			steps.record(step, outcome)
			res.Steps = append(res.Steps, outcome)
			continue
		}
		// Every step is re-resolved against the outputs/outcomes collected so
		// far: a reference that names a step which never published that output
		// is a named step error (docs §3.9), never an empty substitution.
		plan.StepOutcomes, plan.StepConclusions, _ = steps.snapshot()
		resolved, rerr := plan.ResolveStep(step, outputContext(steps))
		var outcome threads.StepResult
		var inner []threads.StepResult
		if rerr != nil {
			outcome = threads.StepResult{
				Number: step.Index + 1, ID: step.ID, Name: step.Name,
				Conclusion: threads.JobFailure, Error: stream.Mask(rerr.Error()),
			}
			stream.Systemf("spindle: step %q: %v", step.Name, rerr)
		} else {
			step = resolved
			outcome, inner = e.runStep(jobCtx, plan, step, stream, bg, steps, nil, hardFailure, cmds, jobCmds, summary)
		}
		steps.record(step, outcome)
		hashCache.invalidate() // the step may have changed the workspace
		res.Steps = append(res.Steps, outcome)
		// A composite action's inner step results follow its summary row (the
		// page shows the expanded steps, docs/threads.md §3.4a).
		res.Steps = append(res.Steps, inner...)
		switch outcome.Conclusion {
		case threads.JobFailure, threads.JobTimedOut:
			anyFailed = true
			if !step.ContinueOnError {
				hardFailure = true
				// Remaining steps are skipped (recorded, so the run page shows
				// the whole list rather than a truncated one). Their ids are
				// recorded too, so `steps.<id>.outcome == 'skipped'` holds for
				// a later condition — though a hard failure means there is no
				// later step to evaluate.
				for j := i + 1; j < len(plan.Steps); j++ {
					rest := plan.Steps[j]
					skipped := threads.StepResult{
						Number: rest.Index + 1, ID: rest.ID, Name: rest.Name, Conclusion: "skipped",
					}
					steps.record(rest, skipped)
					res.Steps = append(res.Steps, skipped)
				}
				res.Conclusion = threads.JobFailure
				res.Error = outcome.Error
				if jobTimedOut(jobCtx, ctx) {
					res.Conclusion = threads.JobTimedOut
				} else if jobCtx.Err() != nil || stream.Cancelled() {
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
	case jobTimedOut(jobCtx, ctx):
		res.Conclusion = threads.JobTimedOut
	case stream.Cancelled() || jobCtx.Err() != nil:
		res.Conclusion = threads.JobCancelled
	case anyFailed && hardFailure:
		res.Conclusion = threads.JobFailure
	default:
		// The deferred `actions/cache` saves (a restore-keys fallback stays
		// green; only the deferred team's turn happens here).
		e.runDeferredCaches(jobCtx, plan, stream)
		// A continue-on-error failure keeps the job green (GitHub's semantics);
		// the step result still records the failure.
		res.Conclusion = threads.JobSuccess
	}
	// Job outputs are resolved only for a successful job: a failed or cancelled
	// job exposes none, and a template that names an unpublished step output is
	// a named error (the job fails) rather than an empty value crossing a job
	// boundary (docs/threads.md §3.9).
	if res.Conclusion == threads.JobSuccess {
		plan.StepOutcomes, plan.StepConclusions, _ = steps.snapshot()
		outs, oerr := evaluateJobOutputs(plan, steps)
		if oerr != nil {
			res.Conclusion = threads.JobFailure
			res.Error = stream.Mask(oerr.Error())
			stream.Systemf("spindle: %v", oerr)
		} else if len(outs) > 0 {
			res.Outputs = outs
			stream.Systemf("spindle: job outputs: %s", outputNames(outs))
		}
	}
	return res
}

// joinErrors appends one job-level error to another without losing either
// (a cleanup failure must be visible even when the job already failed).
func joinErrors(existing, add string) string {
	existing, add = strings.TrimSpace(existing), strings.TrimSpace(add)
	switch {
	case existing == "":
		return add
	case add == "":
		return existing
	}
	return existing + "; " + add
}

// outputNames lists an output map's names, sorted, for a log line (values may
// be secret-bearing, so they are never echoed).
func outputNames(outs map[string]string) string {
	names := make([]string, 0, len(outs))
	for name := range outs {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// failOrCancel classifies a setup error: a cancelled context is a cancel, not a
// failure.
func (e *Executor) failOrCancel(ctx, parent context.Context, stream *Streamer, err error) string {
	if jobTimedOut(ctx, parent) {
		return threads.JobTimedOut
	}
	if ctx.Err() != nil || stream.Cancelled() {
		return threads.JobCancelled
	}
	return threads.JobFailure
}

// jobTimedOut distinguishes the job's OWN deadline from an operator cancel: a
// job context past its deadline while the parent (runner) context is still
// alive reads `timed_out` (PAR-055), never `cancelled`.
func jobTimedOut(jobCtx, parent context.Context) bool {
	return jobCtx != nil && jobCtx.Err() == context.DeadlineExceeded && (parent == nil || parent.Err() == nil)
}

// setup creates the network, services and the job container, and waits for the
// services to be reachable.
func (e *Executor) setup(ctx context.Context, plan *Plan, stream *Streamer) error {
	// A shared operator network (PAR-179) is used as-is; the per-job network is
	// only created when nothing else names one.
	if plan.NetworkMode == plan.Names.Network {
		if err := e.docker(ctx, stream, nil, "network", "create", plan.Names.Network); err != nil {
			return fmt.Errorf("create job network: %w", err)
		}
	}
	if err := e.docker(ctx, stream, nil, "volume", "create", plan.Names.Volume); err != nil {
		return fmt.Errorf("create workspace volume: %w", err)
	}
	for _, svc := range plan.Services {
		stream.Systemf("spindle: starting service %s (%s)", svc.ID, svc.Name)
		if err := e.createContainer(ctx, stream, plan, svc.Image, svc.RequestedImage, svc.Argv); err != nil {
			return fmt.Errorf("create service %s: %w", svc.ID, err)
		}
		if err := e.docker(ctx, stream, nil, "start", svc.Name); err != nil {
			return fmt.Errorf("start service %s: %w", svc.ID, err)
		}
	}
	if err := e.createContainer(ctx, stream, plan, plan.Image, plan.RequestedImage, plan.ContainerArgv); err != nil {
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
	return e.writeJobFiles(ctx, plan)
}

// writeJobFiles seeds the container's per-job files before the first step: the
// event JSON ($GITHUB_EVENT_PATH) and the writable runner directories. The
// payload is bounded (64 KiB) and public by construction, so it travels as one
// exec argument; it is never read from the repository or the network.
func (e *Executor) writeJobFiles(ctx context.Context, plan *Plan) error {
	payload := string(plan.EventJSON)
	if payload == "" {
		payload = "{}"
	}
	// The runner-owned action directories live in the container's own rootfs
	// (not the workspace volume), so a checked-out repository can never
	// pre-create them or point one at a symlink.
	_, stderr, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c",
		`mkdir -p "$1" "$2" "$5" "$6" && chmod 0777 "$1" "$2" "$5" "$6" && printf '%s' "$3" > "$4" && chmod 0644 "$4"`,
		"sh", RunnerTemp, RunnerToolCache, payload, EventPath, threads.RemoteActionsRoot, threads.RemoteStateRoot)
	if err != nil || code != 0 {
		return fmt.Errorf("write the job event file: exit %d: %v: %s", code, err, strings.TrimSpace(stderr))
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

// stepOutcomes is one job's `steps` context: the raw outcome of every step
// that declared an `id:`, its conclusion after continue-on-error, and the
// outputs that step published. A later step's `if:` reads them
// (`steps.<id>.outcome`, `steps.<id>.conclusion`, `steps.<id>.outputs.<name>`),
// which is how a workflow branches on an earlier result.
type stepOutcomes struct {
	// mu guards every map: a background step and each member of a parallel
	// group record from their own goroutine (PAR-080).
	mu         sync.Mutex
	Outcome    map[string]string
	Conclusion map[string]string
	Outputs    map[string]map[string]string
	// outputs and bytes account the whole job against the documented caps.
	outputs int
	bytes   int
}

// newStepOutcomes builds an empty context.
func newStepOutcomes() *stepOutcomes {
	return &stepOutcomes{
		Outcome: map[string]string{}, Conclusion: map[string]string{},
		Outputs: map[string]map[string]string{},
	}
}

// record stores one completed step under its id. An anonymous step is not
// addressable, so it is skipped. The conclusion differs from the outcome only
// for a failed step whose `continue-on-error` tolerates it (GitHub's rule). The
// step's output entry starts empty, so a later substitution of an output it
// never published says "did not publish" rather than "no such step".
func (o *stepOutcomes) record(step StepPlan, result threads.StepResult) {
	if step.ID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.recordLocked(step, result)
}

// recordLocked is record for a caller already holding mu.
func (o *stepOutcomes) recordLocked(step StepPlan, result threads.StepResult) {
	if step.ID == "" {
		return
	}
	outcome := result.Conclusion
	o.Outcome[step.ID] = outcome
	conclusion := outcome
	if (outcome == threads.JobFailure || outcome == threads.JobTimedOut) && step.ContinueOnError {
		conclusion = threads.JobSuccess
	}
	o.Conclusion[step.ID] = conclusion
	if _, ok := o.Outputs[step.ID]; !ok {
		o.Outputs[step.ID] = map[string]string{}
	}
}

// setFinal records a background step's final outcome after its placeholder
// ("running") was recorded when it started.
func (o *stepOutcomes) setFinal(step StepPlan, result threads.StepResult) {
	o.record(step, result)
}

// snapshot copies the three context maps for a reader that must not race a
// concurrent writer (a step `if:` while a background step publishes outputs).
func (o *stepOutcomes) snapshot() (outcome, conclusion map[string]string, outputs map[string]map[string]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome = make(map[string]string, len(o.Outcome))
	conclusion = make(map[string]string, len(o.Conclusion))
	outputs = make(map[string]map[string]string, len(o.Outputs))
	for k, v := range o.Outcome {
		outcome[k] = v
	}
	for k, v := range o.Conclusion {
		conclusion[k] = v
	}
	for id, outs := range o.Outputs {
		cp := make(map[string]string, len(outs))
		for k, v := range outs {
			cp[k] = v
		}
		outputs[id] = cp
	}
	return outcome, conclusion, outputs
}

// addOutputs merges one step's outputs into the job context, enforcing the
// per-job count and byte caps (a runaway job cannot fill the job record). The
// caps are checked before anything is merged, so a refused collection leaves no
// partial outputs behind (the step fails).
func (o *stepOutcomes) addOutputs(id string, outs map[string]string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur := o.Outputs[id]
	if cur == nil {
		cur = map[string]string{}
	}
	added, bytes := 0, 0
	for name, val := range outs {
		if _, ok := cur[name]; !ok {
			added++
		}
		bytes += len(name) + len(val)
	}
	if o.outputs+added > workflow.MaxOutputsPerJob {
		return fmt.Errorf("step %q published outputs over the cap of %d per job", id, workflow.MaxOutputsPerJob)
	}
	if o.bytes+bytes > workflow.MaxOutputBytes {
		return fmt.Errorf("step %q published outputs over the cap of %d bytes per job", id, workflow.MaxOutputBytes)
	}
	for name, val := range outs {
		cur[name] = val
	}
	o.Outputs[id] = cur
	o.outputs += added
	o.bytes += bytes
	return nil
}

// localScope is one composite action execution's namespace: its own steps'
// outcomes and outputs. `steps.<id>` inside the action resolves here (the job's
// ids stay visible too, see exprContext); the caller's declared outputs are
// evaluated from it when the last step has run.
type localScope struct {
	outputs    map[string]map[string]string
	outcome    map[string]string
	conclusion map[string]string
	count      int
	bytes      int
}

// newLocalScope builds an empty action namespace.
func newLocalScope() *localScope {
	return &localScope{
		outputs: map[string]map[string]string{},
		outcome: map[string]string{}, conclusion: map[string]string{},
	}
}

// record stores one inner step's outcome under its action-local id. An empty id
// is anonymous and unaddressable (its outputs are discarded, as on GitHub).
func (s *localScope) record(id, outcome, conclusion string) {
	if id == "" {
		return
	}
	s.outcome[id] = outcome
	s.conclusion[id] = conclusion
	if _, ok := s.outputs[id]; !ok {
		s.outputs[id] = map[string]string{}
	}
}

// addOutputs merges one inner step's outputs, enforcing the same per-action
// caps as a job (a runaway action cannot fill the job record either).
func (s *localScope) addOutputs(id string, outs map[string]string) error {
	if id == "" {
		return nil
	}
	cur := s.outputs[id]
	if cur == nil {
		cur = map[string]string{}
	}
	added, bytes := 0, 0
	for name, val := range outs {
		if _, ok := cur[name]; !ok {
			added++
		}
		bytes += len(name) + len(val)
	}
	if s.count+added > workflow.MaxOutputsPerJob {
		return fmt.Errorf("step %q published outputs over the cap of %d per action", id, workflow.MaxOutputsPerJob)
	}
	if s.bytes+bytes > workflow.MaxOutputBytes {
		return fmt.Errorf("step %q published outputs over the cap of %d bytes per action", id, workflow.MaxOutputBytes)
	}
	for name, val := range outs {
		cur[name] = val
	}
	s.outputs[id] = cur
	s.count += added
	s.bytes += bytes
	return nil
}

// runStep executes one step (or one composite action and its inner steps) and
// returns the step's own result plus the inner results, in execution order.
// A background step is started and reported as `running`; its final result
// replaces that placeholder when it is waited for or the job ends (PAR-080).
func (e *Executor) runStep(ctx context.Context, plan *Plan, step StepPlan, stream StepStream, bg *backgroundSet, steps *stepOutcomes, scope *localScope, hardFailure bool, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary) (threads.StepResult, []threads.StepResult) {
	out := threads.StepResult{Number: step.Index + 1, ID: step.LocalID, Name: step.Name}
	if step.ID != "" {
		out.ID = step.ID // a top-level id is what the job's steps.<id> names
	}
	cond, err := EvalIf(step.If, e.exprContext(plan, hardFailure, stream, steps, scope))
	if err != nil {
		out.Conclusion = threads.JobFailure
		out.Error = err.Error()
		stream.Systemf("spindle: step %q: %v", step.Name, err)
		return out, nil
	}
	if !cond {
		out.Conclusion = "skipped"
		stream.Systemf("spindle: step %q skipped (if: %s)", step.Name, step.If)
		return out, nil
	}
	// The step STARTED: a post hook registered for it must run even if the
	// main program fails (GitHub's rule; PAR-071).
	plan.markRan(step.OutputIndex)
	// A fetched action's resolution record is stated once per job: the author
	// sees the source, the ref, the pinned commit and the content digest.
	if step.Origin != nil {
		plan.logAction(stream, step.Origin)
	}
	switch step.Kind {
	case "composite":
		return e.runCompositeStep(ctx, plan, step, stream, bg, steps, scope, hardFailure, cmds, jobCmds, summary)
	case "control":
		return e.runControlStep(ctx, stream, bg, step)
	case "parallel":
		return e.runParallelStep(ctx, plan, step, stream, steps, hardFailure, cmds, jobCmds, summary)
	case "unsupported":
		out.Conclusion = threads.JobFailure
		out.Error = step.SkipReason
		stream.Systemf("spindle: step %q: %s", step.Name, step.SkipReason)
		return out, nil
	}
	if step.Background {
		if bg == nil {
			out.Conclusion = threads.JobFailure
			out.Error = "internal: background step outside a job"
			return out, nil
		}
		return bg.start(ctx, step, stream, steps, cmds, jobCmds, summary), nil
	}
	stream.StepStart(step)
	started := time.Now()
	res := e.runStepBody(ctx, plan, step, stream, steps, scope, cmds, jobCmds, summary, started)
	stream.StepEnd(step, res.Conclusion, res.ExitCode, res.DurationMS)
	return res, nil
}

// runStepBody executes one resolved, conditioned step's process and collects
// its command files, summary and outputs. It emits no StepStart/StepEnd: the
// caller owns those, so a background step can announce its start immediately
// and emit its end when the job waits for it (or the job ends).
func (e *Executor) runStepBody(ctx context.Context, plan *Plan, step StepPlan, stream StepStream, steps *stepOutcomes, scope *localScope, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary, started time.Time) (out threads.StepResult) {
	out = threads.StepResult{Number: step.Index + 1, ID: step.LocalID, Name: step.Name}
	if step.ID != "" {
		out.ID = step.ID
	}
	if step.Kind == "docker" {
		// The action container is removed on every path (a successful step
		// first has its output file copied out).
		defer e.removeActionContainer(plan, step)
	}
	// A custom `shell:` command reads its script from a file: write it before
	// the step runs, and fall back to sh when a `bash` step's image has no bash
	// (PAR-079). This runs BEFORE the command-file wrapper: the fallback
	// rebuilds the command tail, and rebuilding after PATH had been prepended
	// would drop the wrapper.
	// A remote action materializes its pinned content before anything runs, and
	// a JavaScript action's declared runtime is probed before the first node
	// step (a missing runtime is a named failure, never a skip). A Dockerfile
	// action is built on the runner host before its container is created.
	if step.Node != nil {
		if err := e.materializeAction(ctx, plan, step.Node.Dir, step.Name, step.Node.Files); err != nil {
			out.DurationMS = time.Since(started).Milliseconds()
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(err.Error())
			return out
		}
		if err := e.ensureNodeRuntime(ctx, plan, step.Node.Using, step.Name); err != nil {
			out.DurationMS = time.Since(started).Milliseconds()
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(err.Error())
			stream.Systemf("spindle: step %q: %v", step.Name, err)
			return out
		}
	}
	if step.Docker != nil && step.Docker.Dockerfile != "" {
		image, berr := e.buildDockerfileAction(ctx, plan, step, stream)
		if berr != nil {
			out.DurationMS = time.Since(started).Milliseconds()
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(berr.Error())
			stream.Systemf("spindle: step %q: %v", step.Name, berr)
			return out
		}
		step.Docker.Image = image
		if step.Docker.ImageIndex >= 0 && step.Docker.ImageIndex < len(step.Argv) {
			step.Argv[step.Docker.ImageIndex] = image
		}
	}
	step, skerr := e.prepareStepScript(ctx, plan, step, stream)
	if skerr != nil {
		out.DurationMS = time.Since(started).Milliseconds()
		out.Conclusion = threads.JobFailure
		out.Error = stream.Mask(skerr.Error())
		stream.Systemf("spindle: step %q: %v", step.Name, skerr)
		return out
	}
	// The command-file state of the steps that already ran is layered onto this
	// step's argv (env values) and command (PATH prepending). A docker action's
	// argv is the image's own and is never wrapped.
	step = applyCommandFiles(plan, step, cmds)
	// Workflow commands are parsed out of the step's own output (PAR-093): the
	// sink consumes `::name::` lines, records annotations/masks and forwards
	// everything else to the log.
	sink := newCommandSink(stream, jobCmds, step)
	err := e.runStepCommand(ctx, plan, step, sink, stream)
	sink.Close()
	// The command files are processed for the step that just ran, whatever its
	// conclusion: a step that writes an env var and then fails still changed the
	// job environment for the steps after it (GitHub's rule).
	if cerr := e.collectCommandFiles(ctx, plan, step, cmds, stream); cerr != nil {
		if err == nil {
			err = &stepError{code: -1, msg: cerr.Error()}
		} else {
			stream.Systemf("spindle: step %q: %v", step.Name, cerr)
		}
	}
	// The step summary is collected for a step that succeeded AND one that
	// failed (a failing step's summary explains it): a read/cap error fails a
	// step that otherwise succeeded, never a step that already failed.
	if serr := e.collectStepSummary(ctx, plan, step, summary, stream); serr != nil {
		if err == nil {
			err = &stepError{code: -1, msg: serr.Error()}
		} else {
			stream.Systemf("spindle: step %q: %v", step.Name, serr)
		}
	}
	if err != nil {
		out.DurationMS = time.Since(started).Milliseconds()
		if ctx.Err() != nil || stream.Cancelled() {
			out.Conclusion = threads.JobCancelled
			return out
		}
		out.Conclusion = threads.JobFailure
		if se, ok := err.(*stepError); ok && se.timedOut {
			out.Conclusion = threads.JobTimedOut
		}
		out.ExitCode = stepExitCode(err)
		out.Error = stream.Mask(err.Error())
		return out
	}
	out.DurationMS = time.Since(started).Milliseconds()
	// Collect what the step wrote to its output file. Only a step that
	// succeeded contributes, and only one with an id (job-level or
	// action-local) is addressable; a parse, read or cap failure fails the
	// step with its named error.
	if step.ID != "" || step.LocalID != "" {
		outs, oerr := e.collectStepOutputs(ctx, plan, step)
		if oerr == nil {
			// The deprecated `::set-output::` alias shares the output namespace;
			// a name written through BOTH channels keeps the $GITHUB_OUTPUT value
			// (the supported channel wins, documented in docs/WORKFLOWS.md §6.5).
			for name, val := range sink.setOutputs {
				if _, ok := outs[name]; ok {
					continue
				}
				if outs == nil {
					outs = map[string]string{}
				}
				outs[name] = val
			}
			oerr = e.storeStepOutputs(step, outs, steps, scope)
		}
		if oerr != nil {
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(oerr.Error())
			stream.Systemf("spindle: step %q: %v", step.Name, oerr)
			return out
		}
	}
	// A cache step's own outputs: `cache-hit` is "true" only on an EXACT key
	// match (GitHub's rule), so a restore-keys fallback reads "false".
	if cacheStepKind(step.Kind) && (step.ID != "" || step.LocalID != "") {
		if outs := cacheHitOutput(step); len(outs) > 0 {
			if oerr := e.storeStepOutputs(step, outs, steps, scope); oerr != nil {
				out.Conclusion = threads.JobFailure
				out.Error = stream.Mask(oerr.Error())
				stream.Systemf("spindle: step %q: %v", step.Name, oerr)
				stream.StepEnd(step, threads.JobFailure, -1, out.DurationMS)
				return out
			}
		}
	}
	// `actions/cache` saves at the end of the job, not here: the files the step
	// asked to cache are usually written by the steps AFTER it.
	if step.Kind == "cache" && step.Cache != nil && !step.Cache.LookupOnly {
		plan.deferCacheSave(step.Cache)
	}
	out.Conclusion = threads.JobSuccess
	return out
}

// storeStepOutputs publishes one step's collected outputs: an action-local id
// goes to the action's scope, a top-level id to the job's steps.<id>. An
// anonymous step's writes are discarded.
func (e *Executor) storeStepOutputs(step StepPlan, outs map[string]string, steps *stepOutcomes, scope *localScope) error {
	switch {
	case step.LocalID != "" && scope != nil:
		return scope.addOutputs(step.LocalID, outs)
	case step.ID != "":
		if len(outs) == 0 {
			return nil
		}
		return steps.addOutputs(step.ID, outs)
	}
	return nil
}

// runCompositeStep expands one local composite action at execution time: its
// steps run in order against the action's own output namespace, and its
// declared outputs are evaluated from the last step's results and published
// under the CALLER's id (or its action-local id when the action is nested).
// A failing inner step (without its own continue-on-error) fails the caller and
// skips the rest of the action; the caller's continue-on-error then decides
// whether the job tolerates it.
func (e *Executor) runCompositeStep(ctx context.Context, plan *Plan, step StepPlan, stream StepStream, bg *backgroundSet, steps *stepOutcomes, parent *localScope, hardFailure bool, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary) (threads.StepResult, []threads.StepResult) {
	out := threads.StepResult{Number: step.Index + 1, ID: step.ID, Name: step.Name}
	if step.LocalID != "" {
		out.ID = step.LocalID
	}
	if step.Composite == nil {
		out.Conclusion = threads.JobFailure
		out.Error = "internal: composite step has no expansion"
		return out, nil
	}
	cctx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}
	started := time.Now()
	scope := newLocalScope()
	if len(step.Composite.Files) > 0 {
		if merr := e.materializeAction(ctx, plan, step.Composite.Dir, step.Name, step.Composite.Files); merr != nil {
			out.DurationMS = time.Since(started).Milliseconds()
			out.Conclusion = threads.JobFailure
			out.Error = merr.Error()
			stream.Systemf("spindle: step %q: %v", step.Name, merr)
			return out, nil
		}
	}
	stream.Systemf("spindle: step %d: %s (composite action %s)", step.Index+1, step.Name, step.Composite.Path)
	var inner []threads.StepResult
	failed := false
	for i := range step.Composite.Steps {
		child := step.Composite.Steps[i]
		if failed || cctx.Err() != nil || stream.Cancelled() {
			skipped := threads.StepResult{
				Number: step.Index + 1, ID: child.LocalID, Name: child.Name, Conclusion: "skipped",
			}
			scope.record(child.LocalID, "skipped", "skipped")
			inner = append(inner, skipped)
			continue
		}
		// A composite action's own steps read the ACTION's inputs as
		// `${{ inputs.* }}` (PAR-074); the caller's resolved values are known
		// only now (they may read an earlier step's outputs).
		if child.Template != nil && len(step.Composite.Inputs) > 0 {
			child.Template.ActionInputs = step.Composite.Inputs
			child.Template.ThirdParty = step.Composite.ThirdParty
			// The action's declared inputs are the INPUT_* layer of every inner
			// step; their values are only final now (a caller's `with:` may read
			// an earlier step's output), so the plan-time copy is replaced
			// rather than expanded against this action's own inputs.
			if child.Template.Env == nil {
				child.Template.Env = map[string]string{}
			}
			for name, val := range step.Composite.Inputs {
				child.Template.Env[workflow.InputEnvName(name)] = val
			}
		}
		resolved, rerr := plan.ResolveChildStep(child, outputContext(steps), scope.outputs)
		if rerr != nil {
			skipped := threads.StepResult{Number: step.Index + 1, ID: child.LocalID, Name: child.Name,
				Conclusion: threads.JobFailure, Error: stream.Mask(rerr.Error())}
			inner = append(inner, skipped)
			failed = true
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(rerr.Error())
			stream.Systemf("spindle: %s step %q: %v", step.Name, child.Name, rerr)
			continue
		}
		childRes, childInner := e.runStep(cctx, plan, resolved, stream, bg, steps, scope, hardFailure || failed, cmds, jobCmds, summary)
		inner = append(inner, childRes)
		inner = append(inner, childInner...)
		scope.record(resolved.LocalID, childRes.Conclusion, conclusionOf(childRes, resolved.ContinueOnError))
		if (childRes.Conclusion == threads.JobFailure || childRes.Conclusion == threads.JobTimedOut) && !resolved.ContinueOnError {
			failed = true
			out.Conclusion = threads.JobFailure
			out.Error = childRes.Error
		}
	}
	out.DurationMS = time.Since(started).Milliseconds()
	if out.Conclusion == "" {
		switch {
		case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
			// The composite's own timeout expired: the job container is killed
			// like any other step timeout (the only reliable in-container kill).
			e.killJobContainer(plan)
			out.Conclusion = threads.JobTimedOut
			out.Error = fmt.Sprintf("timed out after %s", step.Timeout)
		case ctx.Err() != nil || stream.Cancelled():
			out.Conclusion = threads.JobCancelled
		default:
			// Evaluate the action's declared outputs over its own steps.
			outs, oerr := evalCompositeOutputs(plan, step.Composite.Outputs, scope.outputs)
			if oerr != nil {
				out.Conclusion = threads.JobFailure
				out.Error = stream.Mask(oerr.Error())
			} else if oerr := e.storeStepOutputs(step, outs, steps, parent); oerr != nil {
				out.Conclusion = threads.JobFailure
				out.Error = stream.Mask(oerr.Error())
			} else {
				out.Conclusion = threads.JobSuccess
			}
		}
	}
	stream.StepEnd(step, out.Conclusion, 0, out.DurationMS)
	return out, inner
}

// conclusionOf applies the continue-on-error rule to one inner step's outcome.
func conclusionOf(res threads.StepResult, tolerated bool) string {
	if (res.Conclusion == threads.JobFailure || res.Conclusion == threads.JobTimedOut) && tolerated {
		return threads.JobSuccess
	}
	return res.Conclusion
}

// evalCompositeOutputs evaluates a composite action's `outputs:` mapping over
// its own steps' outputs. The mapping is a substituted site like any other
// (docs/threads.md §3.6), so `github.*`, `matrix.*`, `env.*` and a secret
// resolve here too; a reference to a step that never published its output is a
// named error (fail closed), like a job output.
func evalCompositeOutputs(plan *Plan, templates map[string]string, local map[string]map[string]string) (map[string]string, error) {
	if len(templates) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		val, err := plan.expandValue(templates[name], plan.Env, local, true)
		if err != nil {
			return nil, fmt.Errorf("action output %q: %v", name, err)
		}
		out[name] = val
	}
	if err := threads.ValidateOutputs(out); err != nil {
		return nil, err
	}
	return out, nil
}

// outputContext is a snapshot of the step-output map of a mid-run context
// (nil-safe for the unit tests that drive one step without a job). It is a
// copy because a background step may publish outputs while a later step is
// resolved.
func outputContext(steps *stepOutcomes) map[string]map[string]string {
	if steps == nil {
		return nil
	}
	steps.mu.Lock()
	defer steps.mu.Unlock()
	out := make(map[string]map[string]string, len(steps.Outputs))
	for id, outs := range steps.Outputs {
		cp := make(map[string]string, len(outs))
		for k, v := range outs {
			cp[k] = v
		}
		out[id] = cp
	}
	return out
}

// stepError carries the docker exit code through the error path.
type stepError struct {
	code int
	msg  string
	// timedOut marks the step's own `timeout-minutes` expiry (PAR-055): the
	// step conclusion is `timed_out`, distinct from a non-zero exit.
	timedOut bool
}

func (s *stepError) Error() string { return s.msg }

func stepExitCode(err error) int {
	if se, ok := err.(*stepError); ok {
		return se.code
	}
	return -1
}

// runStepCommand executes the step argv with its timeout. A step timeout
// terminates the step's process tree by killing the container it runs in (the
// only reliable in-container kill) — the job container for a run/checkout step,
// the action container for a docker action — which fails the job. Secret values
// and the checkout token travel in the docker client's environment (`--env
// NAME`), never in argv; a docker action gets NEITHER: its environment is
// entirely in the argv (`--env K=V`), because the image is third-party code and
// a fork run still receives no secrets (15 §5.2).
func (e *Executor) runStepCommand(ctx context.Context, plan *Plan, step StepPlan, out io.Writer, stream StepStream) error {
	stepCtx := ctx
	// A BACKGROUND step's timeout is owned by its backgroundSet timer: it kills
	// the step's process group and then cancels this context, so the exec
	// returns after the group is gone. Wrapping it in a second deadline here
	// would cancel the CLI first and could leave the in-container tree alive.
	if step.Timeout > 0 && !step.Background {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}
	// The two built-in artifact actions move bytes over the job channel rather
	// than running a process in the container (15 §2.5).
	switch step.Kind {
	case "artifact-upload":
		return e.uploadArtifact(stepCtx, plan, step, stream)
	case "artifact-download":
		return e.downloadArtifact(stepCtx, plan, step, stream)
	case "cache", "cache-restore":
		return e.restoreCache(stepCtx, plan, step, stream)
	case "cache-save":
		return e.saveCache(stepCtx, plan, step, stream)
	}
	var fullEnv []string
	if step.Kind != "docker" {
		// Repository secrets reach a repository-code step by name; a fetched
		// action's code gets only its declared `env:` interface (D-5). The
		// declared interface is credentialEnv, which is always applied.
		if !step.ThirdParty {
			fullEnv = make([]string, 0, len(plan.Secrets)+2)
			for name, value := range plan.Secrets {
				fullEnv = append(fullEnv, name+"="+value)
			}
		}
		fullEnv = append(fullEnv, credentialEnv(plan, step.EnvFrom, step.EnvSecret)...)
	}
	code, err := e.Docker.Stream(stepCtx, fullEnv, out, step.Argv...)
	if stepCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		// The step timed out. A FOREGROUND step's process tree is killed by
		// killing the container it runs in (the only reliable in-container kill).
		// A BACKGROUND step is already in its own session: its timeout timer kills
		// that process group (background.go), and killing the job container here
		// would take every later step with it.
		if !step.Background {
			if step.Kind == "docker" {
				e.removeActionContainer(plan, step)
			} else {
				e.killJobContainer(plan)
			}
		}
		return &stepError{code: -1, msg: fmt.Sprintf("timed out after %s", step.Timeout), timedOut: true}
	}
	if code != 0 {
		msg := fmt.Sprintf("exit code %d", code)
		if err != nil {
			msg = err.Error()
		}
		// Docker-in-job (PAR-081/D-8): a `docker` command in an image without a
		// daemon exits 127. Name the daemonless path and the operator grant, so
		// the reader is not left with "not found".
		if code == 127 && plan.DockerHost == "" && mentionsDocker(step.Run) {
			msg = fmt.Sprintf("exit code %d: this job has no docker daemon or socket (D-8); build with a daemonless builder in the job image (kaniko, or buildah --storage-driver=vfs --isolation=chroot), or run on a runner whose operator granted --docker-host", code)
		}
		return &stepError{code: code, msg: msg}
	}
	if err != nil {
		return &stepError{code: -1, msg: err.Error()}
	}
	return nil
}

// mentionsDocker reports whether a script invokes a docker-family CLI (the
// daemonless remedy is relevant only then).
func mentionsDocker(script string) bool {
	for _, word := range []string{"docker ", "docker-compose", "podman "} {
		if strings.Contains(script, word) {
			return true
		}
	}
	return false
}

// prepareStepScript finishes a script step's argv at execution time (PAR-079):
//
//   - a custom `shell:` command reads its script from a file, written here with
//     `docker exec -i` so the script never becomes one huge argv element;
//   - `shell: bash` on an image without bash falls back to `sh -e` with a log
//     line (GitHub's documented Linux fallback), instead of failing every step
//     on an alpine image with exit 127.
//
// Both are container-side facts, so they cannot be decided at plan time (the
// plan is also shown before a container exists).
func (e *Executor) prepareStepScript(ctx context.Context, plan *Plan, step StepPlan, stream StepStream) (StepPlan, error) {
	if step.Kind != "run" {
		return step, nil
	}
	if strings.EqualFold(strings.TrimSpace(step.Shell), "bash") && !e.containerHasCommand(ctx, plan, "bash") {
		cmd, needsFile, err := shellCommand("sh", step.Run, step.ScriptFile)
		if err != nil {
			return step, err
		}
		stream.Systemf("spindle: bash is not installed in this image; step %q falls back to sh -e", step.Name)
		step = replaceStepCommand(step, plan.Names.Container, cmd)
		if !needsFile {
			step.ScriptFile = ""
		}
	}
	if step.ScriptFile == "" {
		return step, nil
	}
	code, err := e.Docker.StreamIn(ctx, nil, strings.NewReader(step.Run), io.Discard,
		"exec", "-i", plan.Names.Container, "sh", "-c", "cat > "+shQuote(step.ScriptFile))
	if err != nil || code != 0 {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return step, fmt.Errorf("could not write the step's script file %s: %s", step.ScriptFile, msg)
	}
	return step, nil
}

// containerHasCommand reports whether a command is on the job container's PATH.
// The probe is a shell builtin (`command -v`), so it needs nothing but the
// container's own sh.
func (e *Executor) containerHasCommand(ctx context.Context, plan *Plan, name string) bool {
	_, _, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c", "command -v "+shQuote(name)+" >/dev/null 2>&1")
	return err == nil && code == 0
}

// killJobContainer kills the job container (a step timeout's process-tree kill).
func (e *Executor) killJobContainer(plan *Plan) {
	killCtx, killCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer killCancel()
	_, _, _, _ = e.Docker.Run(killCtx, nil, "kill", plan.Names.Container)
}

// removeActionContainer removes one docker action's container, on every path.
// A fresh context is used so a cancelled job cannot strand the container.
func (e *Executor) removeActionContainer(plan *Plan, step StepPlan) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, _, _ = e.Docker.Run(ctx, nil, "rm", "-f", dockerActionName(plan, step.OutputIndex))
}

// exprContext builds the `if` context for one step. hardFailure reports whether
// an earlier step failed WITHOUT continue-on-error (success()/failure() follow
// the job status, which a tolerated failure does not change). steps carries the
// ids of the steps that already ran; scope, when inside a composite action,
// adds the action's own step ids and outputs (an action-local id shadows a job
// id of the same name — the action is its own namespace).
func (e *Executor) exprContext(plan *Plan, hardFailure bool, stream StepStream, steps *stepOutcomes, scope *localScope) ExprContext {
	ctx := ExprContext{
		Success: !hardFailure, Failure: hardFailure, Cancelled: stream.Cancelled(),
		GitHub: plan.githubValues(), Runner: plan.Runner, Vars: plan.Vars,
		Matrix: plan.Matrix, Inputs: plan.Inputs, Env: plan.Env,
		// A step `if:` reads the run's secrets on a trusted run (empty on a fork)
		// and the job token under its GitHub/Forgejo names, the same values a
		// step field substitutes (docs/threads.md §3.7).
		Secrets: plan.secretValues(), HashFiles: plan.HashFiles, Site: workflow.SiteStepIf,
		Needs: plan.NeedsResults, NeedsOutputs: plan.Needs,
	}
	if steps != nil {
		outcome, conclusion, outputs := steps.snapshot()
		ctx.Steps, ctx.StepConclusions = outcome, conclusion
		ctx.Outputs = outputs
	}
	if scope != nil {
		ctx.Steps = mergeOutcomes(ctx.Steps, scope.outcome)
		ctx.StepConclusions = mergeOutcomes(ctx.StepConclusions, scope.conclusion)
		ctx.Outputs = mergeOutputMap(ctx.Outputs, scope.outputs)
	}
	return ctx
}

// mergeOutcomes layers two id→value maps (over wins) without mutating either.
func mergeOutcomes(base, over map[string]string) map[string]string {
	if len(over) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// mergeOutputMap layers two id→outputs maps (over wins).
func mergeOutputMap(base, over map[string]map[string]string) map[string]map[string]string {
	if len(over) == 0 {
		return base
	}
	out := make(map[string]map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
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

// createContainer runs one `docker create` and, on failure, adds the named
// image diagnostic to the job log (image_diagnostic.go). It is separate from
// docker() because a create is the one call whose failure is usually about
// the image source, and an operator needs the mirror context in the log — not
// only docker's sentence.
func (e *Executor) createContainer(ctx context.Context, stream *Streamer, plan *Plan, image, requested string, argv []string) error {
	stdout, stderr, code, err := e.Docker.Run(ctx, nil, argv...)
	if err != nil && code == 0 {
		return err
	}
	if code == 0 {
		return nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = strings.TrimSpace(stdout)
	}
	for _, line := range imageDiagnostic(plan, image, requested, detail) {
		stream.Systemf("%s", line)
	}
	return fmt.Errorf("docker %s: exit %d: %s", strings.Join(argv, " "), code, detail)
}

// cleanup removes every object this job created, on every path. It never uses
// the (possibly cancelled) job context: a fresh timeout is the point.
func (e *Executor) cleanup(plan *Plan) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, name := range append([]string{plan.Names.Container}, serviceNames(plan)...) {
		_, _, _, _ = e.Docker.Run(ctx, nil, "rm", "-f", name)
	}
	// Docker action containers are named deterministically from the plan, so
	// cleanup needs no bookkeeping: an action container left by a cancelled or
	// failed step is removed here too.
	for _, step := range plan.dockerSteps() {
		_, _, _, _ = e.Docker.Run(ctx, nil, "rm", "-f", dockerActionName(plan, step.OutputIndex))
	}
	for _, image := range plan.builtImageList() {
		_, _, _, _ = e.Docker.Run(ctx, nil, "image", "rm", "-f", image)
	}
	_, _, _, _ = e.Docker.Run(ctx, nil, "network", "rm", plan.Names.Network)
	_, _, _, _ = e.Docker.Run(ctx, nil, "volume", "rm", "-f", plan.Names.Volume)
}

// dockerSteps lists every docker action step of the plan, composite inner steps
// included (cleanup and name derivation).
func (p *Plan) dockerSteps() []StepPlan {
	var out []StepPlan
	var walk func(steps []StepPlan)
	walk = func(steps []StepPlan) {
		for _, st := range steps {
			if st.Kind == "docker" {
				out = append(out, st)
			}
			if st.Composite != nil {
				walk(st.Composite.Steps)
			}
		}
	}
	walk(p.Steps)
	return out
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
