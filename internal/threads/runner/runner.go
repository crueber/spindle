package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Runner is the long-poll loop over the INSTANCE queue (15 §3/§4,
// docs/PLAN-INSTANCE-RUNNERS.md): it is registered once by a host
// administrator and every fetch is a global claim, so the same runner may be
// offered jobs from any repository.
//
// Capacity: the runner keeps `Capacity` jobs in flight, one goroutine each,
// every one with its own long-poll, container, network and workspace volume.
// The server has no worker counter — the CAS claim is the arbitration — so any
// number of runners may serve the instance, and a capacity-N runner is N
// concurrent fetches. Capacity 1 (the default) keeps one daemon shared by one
// job at a time, which is the isolation-preserving shape for untrusted code.
//
// ### Concurrency
// Hazard: a worker loop spinning when the server answers instantly (a proxy
// returning 204 without the long-poll). Avoidance: a fetch that returned in
// under a second sleeps before the next probe, and every error backs off
// exponentially (1 s → 30 s). Hazard: SIGTERM abandoning containers.
// Avoidance: each job context derives from the root context, so cancellation
// kills the docker exec, the executor's deferred cleanup removes every object,
// and the job is reported `cancelled` on a fresh context. Hazard: unbounded
// goroutines. Avoidance: exactly Capacity workers plus one flusher per in-flight
// job; both stop with their job.
type Runner struct {
	cfg    Config
	client *Client
	exec   *Executor

	fatalMu sync.Mutex
	fatal   error
}

// Config is the runner's settings (flags/env in cmd/spindle/cli_runner.go).
type Config struct {
	ServerURL string
	Token     string
	Name      string
	Labels    []string
	Capacity  int
	Version   string

	DockerBin       string
	DefaultImage    string
	WorkspaceDir    string
	AllowHostDocker bool
	ServerOrigin    string
	// AllowForkRuns opts this runner in to executing fork pull-request code
	// (default false). The server never offers it a fork run unless the registry
	// records the flag; the runner also refuses one it was handed anyway
	// (defence in depth, docs/threads.md §8.7).
	AllowForkRuns bool
	// ImagePrefix/ImagePrefixAll/ImagePull/ImageRequireDigest are the
	// image-source policy (docs/threads.md §8.1/§8.6). Empty/zero values keep
	// today's behaviour.
	ImagePrefix        string
	ImagePrefixAll     bool
	ImagePull          string
	ImageRequireDigest bool

	FetchWait  time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration

	Logger   *slog.Logger
	Client   *Client
	Executor *Executor
}

// New builds a runner. It validates the settings that cannot be fixed later
// (server URL, token, capacity) and refuses a configuration that would expose
// the host root filesystem to job containers without an explicit
// acknowledgement (15 §4).
func New(cfg Config) (*Runner, error) {
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, fmt.Errorf("runner: the server URL is required (--server or SPINDLE_SERVER)")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("runner: a runner token is required (--token-file, SPINDLE_RUNNER_TOKEN, or --register)")
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	if cfg.Capacity > threads.MaxRunnerCapacity {
		cfg.Capacity = threads.MaxRunnerCapacity
	}
	if cfg.FetchWait <= 0 {
		cfg.FetchWait = 20 * time.Second
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	images := ImageConfig{
		Prefix:        cfg.ImagePrefix,
		PrefixAll:     cfg.ImagePrefixAll,
		Pull:          cfg.ImagePull,
		RequireDigest: cfg.ImageRequireDigest,
	}
	if err := ValidateImageConfig(images); err != nil {
		return nil, fmt.Errorf("runner: %v", err)
	}
	hostDocker := hostDockerExposed(cfg.DockerBin)
	if hostDocker && !cfg.AllowHostDocker {
		return nil, fmt.Errorf("runner: the configured docker daemon exposes the host root filesystem (DOCKER_HOST unix:///var/run/docker.sock); pass --allow-host-docker to acknowledge, or point DOCKER_HOST at a per-runner docker:dind daemon")
	}
	client := cfg.Client
	if client == nil {
		client = NewClient(cfg.ServerURL, cfg.Token)
	}
	exec := cfg.Executor
	if exec == nil {
		exec = &Executor{
			Docker:       &DockerCLIProcess{Bin: cfg.DockerBin},
			DefaultImage: cfg.DefaultImage,
			WorkspaceDir: cfg.WorkspaceDir,
			Images:       images,
			ServerURL:    cfg.ServerOrigin,
			Logger:       cfg.Logger,
			Artifacts:    client,
		}
	}
	if exec.Images == (ImageConfig{}) {
		exec.Images = images
	}
	if exec.Artifacts == nil {
		exec.Artifacts = client
	}
	if exec.WorkspaceDir == "" {
		exec.WorkspaceDir = cfg.WorkspaceDir
	}
	if exec.DefaultImage == "" {
		exec.DefaultImage = cfg.DefaultImage
	}
	return &Runner{cfg: cfg, client: client, exec: exec}, nil
}

// hostDockerExposed reports the default host-socket configuration: an unset
// DOCKER_HOST (or the stock /var/run/docker.sock) hands job containers the
// host root filesystem through the daemon. Any other DOCKER_HOST is the
// operator's explicit declaration (15 §4).
func hostDockerExposed(_ string) bool {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	switch host {
	case "", "unix:///var/run/docker.sock", "/var/run/docker.sock":
		return true
	}
	return false
}

// Run declares the runner and serves until ctx is cancelled (SIGTERM). It
// returns the first fatal error (a rejected credential) and nil on a clean
// stop.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.declare(ctx); err != nil {
		return err
	}
	r.cfg.Logger.Info("threads runner: serving",
		"server", r.cfg.ServerURL, "labels", strings.Join(r.cfg.Labels, ","), "capacity", r.cfg.Capacity,
		"allow_fork_runs", r.cfg.AllowForkRuns)
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Capacity; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			r.worker(ctx, worker)
		}(i)
	}
	wg.Wait()
	if err := r.fatalErr(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		r.cfg.Logger.Info("threads runner: stopped")
	}
	return nil
}

// declare announces the runner (version + labels) with retries until it
// succeeds, the context ends, or the credential is rejected.
func (r *Runner) declare(ctx context.Context) error {
	backoff := r.cfg.BackoffMin
	for {
		view, err := r.client.Declare(ctx, r.cfg.Version, r.cfg.Labels, r.cfg.Capacity, r.cfg.AllowForkRuns)
		if err == nil {
			// The repository policy is the ADMIN's (set at the mint); the runner
			// states it and cannot change it (docs/threads.md §8.7).
			if view != nil {
				r.cfg.Logger.Info("threads runner: repository policy",
					"policy", view.Policy.Describe(), "allow_fork_runs", view.AllowForkRuns)
			}
			return nil
		}
		if errors.Is(err, ErrUnauthorized) {
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.cfg.Logger.Warn("threads runner: declare failed; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, r.cfg.BackoffMax)
	}
}

// worker is one fetch/execute loop.
func (r *Runner) worker(ctx context.Context, n int) {
	backoff := r.cfg.BackoffMin
	for ctx.Err() == nil {
		started := time.Now()
		job, err := r.client.Fetch(ctx, r.cfg.FetchWait)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				r.setFatal(fmt.Errorf("%w: %v", ErrUnauthorized, err))
				return
			}
			if ctx.Err() != nil {
				return
			}
			r.cfg.Logger.Warn("threads runner: fetch failed; retrying", "worker", n, "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff, r.cfg.BackoffMax)
			continue
		}
		backoff = r.cfg.BackoffMin
		if job == nil {
			// The server long-polls; an instant 204 means something in between
			// answered, so never spin on it.
			if time.Since(started) < time.Second {
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
			continue
		}
		r.runJob(ctx, job)
	}
}

// runJob executes one claimed job and reports its conclusion.
func (r *Runner) runJob(ctx context.Context, job *threads.FetchResult) {
	log := r.cfg.Logger.With("job", job.JobRef, "attempt", job.Attempt, "repo", job.Repo)
	start, err := r.client.Start(ctx, job.JobRef, job.Attempt)
	if err != nil {
		if errors.Is(err, ErrSuperseded) {
			log.Info("threads runner: attempt superseded before start; abandoning")
			return
		}
		if errors.Is(err, ErrUnauthorized) {
			r.setFatal(fmt.Errorf("%w: %v", ErrUnauthorized, err))
			return
		}
		log.Warn("threads runner: start failed; job will be requeued by its lease", "err", err)
		return
	}
	// Defence in depth: the server is supposed to offer a fork run only to a
	// runner whose registry record opted in, but this runner must never execute
	// fork code on a host that did not declare it acceptable. Report it as a
	// failed job with the reason (the lease then has nothing to requeue).
	if !job.Trusted && !r.cfg.AllowForkRuns {
		msg := "spindle: this runner has not opted in to fork pull-request runs (--allow-fork-runs / SPINDLE_RUNNER_ALLOW_FORK_RUNS), so it refused to execute the job; start a runner that accepts fork code on a disposable host"
		log.Error("threads runner: refusing fork run: this runner did not opt in", "hint", "--allow-fork-runs")
		refuseCtx, refuseCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer refuseCancel()
		_, cerr := r.client.Complete(refuseCtx, start.Token, job.JobRef, job.Attempt, threads.CompleteInput{
			Conclusion: threads.JobFailure,
			Steps: []threads.StepResult{{
				Number: 1, Name: "fork run refused", Conclusion: threads.JobFailure, Error: msg,
			}},
		})
		if cerr != nil {
			log.Error("threads runner: reporting the fork refusal failed", "err", cerr)
		}
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := NewStreamer(r.client, start.Token, job.JobRef, job.Attempt, job.Mask, cancel, log)
	log.Info("threads runner: job started", "workflow", job.Workflow, "commit", job.Commit, "check", job.Context)

	res := r.exec.Execute(jobCtx, ExecInput{Job: job, AttemptToken: start.Token, Caps: start.Artifacts}, stream)

	// SIGTERM (or the job context) makes this a cancel, not a failure: the
	// containers are already gone (Execute's deferred cleanup).
	conclusion := res.Conclusion
	if ctx.Err() != nil || stream.Cancelled() {
		conclusion = threads.JobCancelled
	}
	stream.Close()

	// The completion write uses a fresh context: the job context may be the
	// cancelled one, and a lost conclusion is worse than a late one.
	doneCtx, doneCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer doneCancel()
	view, cerr := r.client.Complete(doneCtx, start.Token, job.JobRef, job.Attempt, threads.CompleteInput{
		Conclusion: conclusion, Steps: res.Steps, Outputs: res.Outputs,
	})
	switch {
	case cerr == nil:
		log.Info("threads runner: job complete", "conclusion", conclusion, "state", view.State, "run_state", view.Run)
	case errors.Is(cerr, ErrSuperseded):
		log.Info("threads runner: attempt superseded; conclusion discarded")
	case errors.Is(cerr, ErrUnauthorized):
		r.setFatal(fmt.Errorf("%w: %v", ErrUnauthorized, cerr))
	default:
		log.Error("threads runner: complete failed", "conclusion", conclusion, "err", cerr)
	}
}

// setFatal records a stop-the-world condition (a revoked credential).
func (r *Runner) setFatal(err error) {
	r.fatalMu.Lock()
	if r.fatal == nil {
		r.fatal = err
	}
	r.fatalMu.Unlock()
	r.cfg.Logger.Error("threads runner: stopping", "err", err)
}

// fatalErr returns the recorded fatal error.
func (r *Runner) fatalErr() error {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatal
}

// nextBackoff doubles a delay up to max.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

// Register consumes a registration token and returns the long-lived runner
// credential (the bootstrap path: `spindle runner serve --register wrg_…`).
func Register(ctx context.Context, serverURL, regToken, name string, labels []string, capacity int, allowForkRuns bool) (*threads.RegisterResult, error) {
	client := NewClient(serverURL, "")
	return client.Register(ctx, regToken, name, labels, capacity, allowForkRuns)
}
