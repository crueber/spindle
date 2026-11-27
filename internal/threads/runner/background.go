package runner

// Background and parallel steps (PAR-080, D-7; docs/threads.md §3.4b).
//
// A `background: true` step is started asynchronously and a later step waits for
// it (`wait: <id>` / `wait-all: true`) or terminates it (`cancel: <id>`). Every
// background process is started in its OWN session and process group INSIDE the
// job container (setsid), so one kill reaches the whole tree — the command and
// everything it forked — without killing the job. A docker-action step is
// backgrounded as its own container, and its kill is `docker rm -f`.
//
// Teardown is unconditional and ordered: a background is killed on every path
// (cancel, its own timeout, and the end of the job, whatever the conclusion),
// by a fresh-context `docker exec` that sends SIGTERM, waits, then SIGKILLs the
// process group and verifies through /proc that no live member survived. The
// job cannot be reported `success` while a process it started is still alive,
// and a failed reap is a named job failure, never silently inherited by the next
// job on a capacity-N runner.
//
// ### Concurrency
//
// Hazard: a background step and the foreground steps run at the same time, and
// a parallel group's members run concurrently, so the command-file state, the
// step-output maps and the workflow-command annotations are shared mutable
// state. Avoidance: a background/parallel step writes to its OWN commandFiles
// clone and its own buffered stream; the main goroutine merges the clone in
// step order when the step is joined (wait, cancel, or job end). The job-wide
// sinks (stepOutcomes, jobCommands, jobSummary) are mutex-guarded. Only the main
// goroutine calls join/merge, so merge order is the declaration order, never a
// completion race. Hazard: a process outliving its job. Avoidance: the job-end
// path in Execute joins/kills every registered background before the deferred
// docker cleanup, and `killGroup` verifies the group is gone. Hazard: a leaked
// goroutine. Avoidance: each background's watcher selects on the job context and
// the step's done channel, and both close on every path.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// bgKillScript terminates one background process group and reports whether a
// live member survived SIGKILL. It runs inside the job container with the pid
// file as $1 (the session leader's pid, written by the wrapper below).
const bgKillScript = `
pf=$1
p=$(cat "$pf" 2>/dev/null) || { rm -f "$pf"; exit 0; }
[ -n "$p" ] || { rm -f "$pf"; exit 0; }
kill -TERM -"$p" 2>/dev/null || true
i=0
while kill -0 -"$p" 2>/dev/null; do
  i=$((i+1))
  [ "$i" -ge 100 ] && break
  sleep 0.1
done
kill -KILL -"$p" 2>/dev/null || true
sleep 0.1
leaked=0
for d in /proc/[0-9]*; do
  pid=${d#/proc/}
  [ "$pid" = "$$" ] && continue
  st=$(cat "$d/stat" 2>/dev/null) || continue
  rest=${st##*) }
  set -- $rest
  [ "${3:-}" = "$p" ] || continue
  [ "${1:-}" = "Z" ] && continue
  leaked=1
done
rm -f "$pf"
[ "$leaked" = 0 ] || { echo "process group $p survived SIGKILL" >&2; exit 1; }
exit 0
`

// bgStartScript runs the step in its own session while keeping the docker exec
// attached. The outer shell (the docker exec process) waits for the session
// leader and relays its exit status through a status file: a plain
// `exec setsid …` returns as soon as setsid forks — which it does whenever the
// exec'd process is already a group leader — and the runner would then report a
// still-running background step as finished.
const bgStartScript = `pf=$1; shift
rc=$pf.rc
setsid sh -c 'pf=$1; shift; rcf=$1; shift; echo $$ > "$pf"; "$@"; echo $? > "$rcf"' sh "$pf" "$rc" "$@" &
wait $!
p=$(cat "$pf" 2>/dev/null)
if [ -n "$p" ]; then
  while kill -0 -"$p" 2>/dev/null; do sleep 0.2; done
fi
code=$(cat "$rc" 2>/dev/null || echo 0)
rm -f "$rc"
exit ${code:-0}`

// bgPIDFile is the in-container pid file of one flat step index (unique across
// composite expansion, like the output/script files).
func bgPIDFile(index int) string { return "/tmp/spindle-bg-" + strconv.Itoa(index+1) + ".pid" }

// withProcessGroup wraps a `docker exec` argv so the command runs in its own
// session while the exec stays attached: `… <container> sh -c '<supervisor>'
// sh <pidfile> <step argv>`. The supervisor itself runs setsid, waits for the
// session leader and relays its exit status (bgStartScript).
func withProcessGroup(argv []string, container, pidfile string) ([]string, bool) {
	idx := -1
	for i, arg := range argv {
		if arg == container {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(argv) {
		return argv, false
	}
	out := make([]string, 0, len(argv)+4)
	out = append(out, argv[:idx+1]...)
	out = append(out, "sh", "-c", bgStartScript, "sh", pidfile)
	out = append(out, argv[idx+1:]...)
	return out, true
}

// bgStep is one running background step.
type bgStep struct {
	set     *backgroundSet
	step    StepPlan
	pid     string // pid file (run steps) or "" (docker steps)
	buf     *bufferStream
	cmds    *commandFiles
	started time.Time

	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
	done   chan struct{}

	mu          sync.Mutex
	res         threads.StepResult
	timedOut    bool
	killedFor   string
	joined      bool
	final       threads.StepResult
	leakErr     string
	workerEnded bool
}

// backgroundSet is one job's registry of background steps.
type backgroundSet struct {
	e       *Executor
	plan    *Plan
	stream  *Streamer
	steps   *stepOutcomes
	jobCmds *jobCommands
	summary *jobSummary
	cmds    *commandFiles

	mu    sync.Mutex
	byID  map[string]*bgStep
	order []*bgStep
}

func newBackgroundSet(e *Executor, plan *Plan, stream *Streamer, steps *stepOutcomes, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary) *backgroundSet {
	return &backgroundSet{
		e: e, plan: plan, stream: stream, steps: steps,
		cmds: cmds, jobCmds: jobCmds, summary: summary,
		byID: map[string]*bgStep{},
	}
}

// start launches one background step. It returns the placeholder result the
// job's step list carries while the step runs; the final result replaces it when
// the step is joined (a worker that exits on its own is joined at job end, and a
// non-zero exit is surfaced there, never dropped).
func (s *backgroundSet) start(ctx context.Context, step StepPlan, stream StepStream, steps *stepOutcomes, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary) threads.StepResult {
	out := threads.StepResult{Number: step.Index + 1, ID: step.ID, Name: step.Name, Conclusion: "running"}
	if ctx.Err() != nil || stream.Cancelled() {
		out.Conclusion = threads.JobCancelled
		return out
	}
	pid := ""
	if step.Kind != "docker" {
		// Process-group kill needs `setsid` in the job image. Fail the step by
		// name before starting anything: a background process we cannot reap is
		// exactly the leak D-7 forbids.
		if !s.e.containerHasCommand(ctx, s.plan, "setsid") {
			out.Conclusion = threads.JobFailure
			out.Error = "background steps need `setsid` in the job image (util-linux or busybox). Install it, or run this step in the foreground"
			stream.Systemf("spindle: step %q: %s", step.Name, out.Error)
			return out
		}
		pid = bgPIDFile(step.OutputIndex)
		wrapped, ok := withProcessGroup(step.Argv, s.plan.Names.Container, pid)
		if !ok {
			out.Conclusion = threads.JobFailure
			out.Error = "internal: could not wrap the step argv for background execution"
			return out
		}
		step.Argv = wrapped
	}
	bgCtx, cancel := context.WithCancel(ctx)
	b := &bgStep{
		set: s, step: step, pid: pid, buf: newBufferStream(s.stream, backgroundOutputLimit),
		cmds: cmds.clone(), started: time.Now(), ctx: bgCtx, cancel: cancel, done: make(chan struct{}),
	}
	s.mu.Lock()
	s.byID[step.ID] = b
	s.order = append(s.order, b)
	s.mu.Unlock()
	stream.Systemf("▸ step %d: %s (background)", step.Index+1, step.Name)
	go b.run()
	// A cancelled job (operator cancel, SIGTERM, job timeout) tears the process
	// group down immediately rather than waiting for the job-end pass.
	go func() {
		select {
		case <-ctx.Done():
			_ = b.kill("cancel")
		case <-b.done:
		}
	}()
	if step.Timeout > 0 {
		b.timer = time.AfterFunc(step.Timeout, func() { _ = b.kill("timeout") })
	}
	return out
}

// run is the background worker: the ordinary step body against its own buffered
// stream and command-file clone.
func (b *bgStep) run() {
	defer close(b.done)
	res := b.set.e.runStepBody(b.ctx, b.set.plan, b.step, b.buf, b.set.steps, nil, b.cmds, b.set.jobCmds, b.set.summary, b.started)
	b.mu.Lock()
	b.res = res
	b.workerEnded = true
	b.mu.Unlock()
}

// kill terminates the process group (run step) or container (docker action).
// It is idempotent: a second cancel/timeout does nothing.
func (b *bgStep) kill(reason string) error {
	b.mu.Lock()
	if b.killedFor != "" {
		b.mu.Unlock()
		return nil
	}
	b.killedFor = reason
	if reason == "timeout" {
		b.timedOut = true
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var err error
	if b.step.Kind == "docker" {
		_, _, _, _ = b.set.e.Docker.Run(ctx, nil, "rm", "-f", dockerActionName(b.set.plan, b.step.OutputIndex))
	} else {
		_, stderr, code, kerr := b.set.e.Docker.Run(ctx, nil, "exec", b.set.plan.Names.Container, "sh", "-c", bgKillScript, "sh", b.pid)
		if kerr != nil || code != 0 {
			msg := strings.TrimSpace(stderr)
			if msg == "" && kerr != nil {
				msg = kerr.Error()
			}
			if msg == "" {
				msg = fmt.Sprintf("exit %d", code)
			}
			err = fmt.Errorf("kill process group: %s", msg)
		}
	}
	// Release the host-side docker exec/run goroutine after the in-container
	// kill; the group is already gone (or the leak error says so).
	b.cancel()
	return err
}

// join finalizes the step exactly once and returns its result: flush the
// buffered output into the job log, emit the step-end line, merge the step's
// command-file writes into the job (declaration order), and record the final
// outcome for `steps.<id>`. It must be called from the job's main goroutine.
func (b *bgStep) join() threads.StepResult {
	b.mu.Lock()
	if b.joined {
		res := b.final
		b.mu.Unlock()
		return res
	}
	b.joined = true
	res := b.res
	ended := b.workerEnded
	timedOut := b.timedOut
	reason := b.killedFor
	b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.buf.flush()
	switch {
	case timedOut:
		res.Conclusion = threads.JobFailure
		res.ExitCode = -1
		res.Error = fmt.Sprintf("timed out after %s", b.step.Timeout)
	case !ended:
		// The worker did not return within the bounded wait after a kill: the
		// container-side group was killed, but the host CLI is still blocked.
		res.Conclusion = threads.JobCancelled
		res.Error = "the background process did not stop within the bounded wait"
	case res.Conclusion == "" || res.Conclusion == threads.JobCancelled:
		if reason != "" {
			res.Conclusion = threads.JobCancelled
		}
	}
	if res.Conclusion == "" {
		res.Conclusion = threads.JobSuccess
	}
	if reason != "" && res.Conclusion != threads.JobFailure {
		b.set.stream.Systemf("spindle: background step %q stopped (%s)", b.step.Name, reason)
	}
	if err := b.set.cmds.mergeFrom(b.cmds); err != nil && res.Conclusion == threads.JobSuccess {
		res.Conclusion = threads.JobFailure
		res.Error = b.set.stream.Mask(err.Error())
	}
	b.set.steps.setFinal(b.step, res)
	b.set.stream.StepEnd(b.step, res.Conclusion, res.ExitCode, res.DurationMS)
	b.mu.Lock()
	b.final = res
	b.mu.Unlock()
	return res
}

// wait joins a background, killing it first when the job is cancelled. It
// returns the final result.
func (b *bgStep) wait(ctx context.Context) threads.StepResult {
	select {
	case <-b.done:
	case <-ctx.Done():
		_ = b.kill("cancel")
		select {
		case <-b.done:
		case <-time.After(30 * time.Second):
		}
	}
	return b.join()
}

// lookup finds a registered background by id.
func (s *backgroundSet) lookup(id string) *bgStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[strings.ToLower(strings.TrimSpace(id))]
}

// outstanding returns the registered backgrounds not yet joined, in declaration
// order.
func (s *backgroundSet) outstanding() []*bgStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bgStep, 0, len(s.order))
	for _, b := range s.order {
		b.mu.Lock()
		joined := b.joined
		b.mu.Unlock()
		if !joined {
			out = append(out, b)
		}
	}
	return out
}

// finishAll is the job-end pass: every background not waited for is joined
// (an already-exited one keeps its own result; a running one is killed), and the
// failures/leaks are returned for the job conclusion. It never leaves a process
// group alive silently.
func (s *backgroundSet) finishAll() (failed []string, leaked []string) {
	for _, b := range s.outstanding() {
		select {
		case <-b.done:
			// exited on its own; join surfaces a non-zero exit below
		default:
			if err := b.kill("job end"); err != nil {
				leaked = append(leaked, fmt.Sprintf("background step %q: %v", b.step.Name, err))
			}
			select {
			case <-b.done:
			case <-time.After(30 * time.Second):
				leaked = append(leaked, fmt.Sprintf("background step %q did not stop within 30s of the job-end kill", b.step.Name))
			}
		}
		res := b.join()
		if res.Conclusion == threads.JobFailure && !b.step.ContinueOnError {
			errText := res.Error
			if errText == "" {
				errText = fmt.Sprintf("exit code %d", res.ExitCode)
			}
			failed = append(failed, fmt.Sprintf("background step %q failed and was not waited for: %s", b.step.Name, errText))
		}
	}
	return failed, leaked
}

// mergeResults replaces each background placeholder ("running") in the job's
// reported step list with its final result. Matching is by id, which PAR-080
// requires a background step to declare.
func (s *backgroundSet) mergeResults(results []threads.StepResult) []threads.StepResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range results {
		b, ok := s.byID[results[i].ID]
		if !ok || results[i].ID == "" {
			continue
		}
		b.mu.Lock()
		joined, final := b.joined, b.final
		b.mu.Unlock()
		if joined {
			results[i] = final
		}
	}
	return results
}

// runControlStep executes a `wait`/`wait-all`/`cancel` directive. A wait fails
// the control step when the waited background failed (the failure is never
// silently dropped); a cancel succeeds once the process group is gone.
func (e *Executor) runControlStep(ctx context.Context, stream StepStream, bg *backgroundSet, step StepPlan) (threads.StepResult, []threads.StepResult) {
	out := threads.StepResult{Number: step.Index + 1, ID: step.ID, Name: step.Name}
	if bg == nil {
		out.Conclusion = threads.JobFailure
		out.Error = "internal: a wait/cancel step runs outside a job"
		return out, nil
	}
	stream.Systemf("▸ step %d: %s", step.Index+1, step.Name)
	defer func() { stream.StepEnd(step, out.Conclusion, 0, 0) }()
	if step.Cancel != "" {
		targets, err := bg.targets(step.Cancel)
		if err != nil {
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(err.Error())
			return out, nil
		}
		for _, b := range targets {
			if kerr := b.kill("cancel"); kerr != nil {
				out.Conclusion = threads.JobFailure
				out.Error = stream.Mask(kerr.Error())
				return out, nil
			}
			b.wait(ctx)
		}
		out.Conclusion = threads.JobSuccess
		stream.Systemf("spindle: step %q cancelled %d background step(s)", step.Name, len(targets))
		return out, nil
	}
	ref := step.Wait
	if step.WaitAll || strings.EqualFold(ref, "all") {
		ref = "all"
	}
	targets, err := bg.targets(ref)
	if err != nil {
		out.Conclusion = threads.JobFailure
		out.Error = stream.Mask(err.Error())
		return out, nil
	}
	var failures []string
	for _, b := range targets {
		res := b.wait(ctx)
		if res.Conclusion == threads.JobFailure {
			msg := res.Error
			if msg == "" {
				msg = fmt.Sprintf("exit code %d", res.ExitCode)
			}
			failures = append(failures, fmt.Sprintf("background step %q: %s", b.step.Name, msg))
		}
	}
	if len(failures) > 0 {
		out.Conclusion = threads.JobFailure
		out.Error = stream.Mask(strings.Join(failures, "; "))
		stream.Systemf("spindle: step %q: %s", step.Name, out.Error)
		return out, nil
	}
	out.Conclusion = threads.JobSuccess
	return out, nil
}

// targets resolves a wait/cancel reference ("all" or one id) to the registered
// backgrounds.
func (s *backgroundSet) targets(ref string) ([]*bgStep, error) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, "all") || ref == "" {
		return s.outstanding(), nil
	}
	b := s.lookup(ref)
	if b == nil {
		return nil, fmt.Errorf("no background step named %q was started", ref)
	}
	return []*bgStep{b}, nil
}

// runParallelStep runs a group's members concurrently and reports them as one
// unit followed by the member results in declaration order. Output is buffered
// per member and flushed in order, so two members writing at once never
// interleave into an unreadable log.
func (e *Executor) runParallelStep(ctx context.Context, plan *Plan, step StepPlan, stream StepStream, steps *stepOutcomes, hardFailure bool, cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary) (threads.StepResult, []threads.StepResult) {
	out := threads.StepResult{Number: step.Index + 1, ID: step.ID, Name: step.Name}
	started := time.Now()
	stream.Systemf("▸ step %d: %s (parallel, %d members)", step.Index+1, step.Name, len(step.Parallel))
	type member struct {
		step  StepPlan
		buf   *bufferStream
		cmds  *commandFiles
		res   threads.StepResult
		inner []threads.StepResult
		err   string
	}
	members := make([]*member, len(step.Parallel))
	// Every member starts from the same accumulated command-file state; the
	// group merges the clones in declaration order afterwards, so completion
	// order can never decide the job environment.
	outputs := outputContext(steps)
	for i, m := range step.Parallel {
		resolved, err := plan.ResolveStep(m, outputs)
		if err != nil {
			members[i] = &member{step: m, err: stream.Mask(err.Error())}
			continue
		}
		members[i] = &member{step: resolved, buf: newBufferStream(stream.root(), parallelOutputLimit), cmds: cmds.clone()}
	}
	var wg sync.WaitGroup
	for i := range members {
		m := members[i]
		if m.buf == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.res, m.inner = e.runStep(ctx, plan, m.step, m.buf, nil, steps, nil, hardFailure, m.cmds, jobCmds, summary)
		}()
	}
	wg.Wait()
	var inner []threads.StepResult
	for _, m := range members {
		if m.buf != nil {
			m.buf.flush()
			if err := cmds.mergeFrom(m.cmds); err != nil && m.res.Conclusion == threads.JobSuccess {
				m.res.Conclusion = threads.JobFailure
				m.res.Error = stream.Mask(err.Error())
			}
		}
		if m.err != "" {
			m.res = threads.StepResult{Number: step.Index + 1, Name: m.step.Name, Conclusion: threads.JobFailure, Error: m.err}
		}
		inner = append(inner, m.res)
		inner = append(inner, m.inner...)
	}
	out.DurationMS = time.Since(started).Milliseconds()
	switch {
	case ctx.Err() != nil || stream.Cancelled():
		out.Conclusion = threads.JobCancelled
	default:
		out.Conclusion = threads.JobSuccess
		var failures []string
		for i, m := range members {
			if m.res.Conclusion != threads.JobFailure || m.step.ContinueOnError {
				continue
			}
			msg := m.res.Error
			if msg == "" {
				msg = fmt.Sprintf("exit code %d", m.res.ExitCode)
			}
			failures = append(failures, fmt.Sprintf("member %d (%s): %s", i+1, m.step.Name, msg))
		}
		if len(failures) > 0 {
			out.Conclusion = threads.JobFailure
			out.Error = stream.Mask(strings.Join(failures, "; "))
		}
	}
	stream.StepEnd(step, out.Conclusion, 0, out.DurationMS)
	return out, inner
}
