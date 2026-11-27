package runner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/crueber/spindle/internal/threads"
)

// Step summaries (PAR-092, docs/WORKFLOWS.md §6.6). Every repository-code step
// receives `$GITHUB_STEP_SUMMARY` (aliased as `$SPINDLE_STEP_SUMMARY`), a file
// it may append markdown to. The runner collects the file after the step — on
// success AND failure, like GitHub — and joins the pieces into the job's
// summary, which the server renders with its sanitizing markdown renderer on
// the run page. The file is deliberately in the container's /tmp, not the
// workspace: a `git clean` in the checkout cannot lose it, and nothing in the
// repository shows up as untracked.
//
// The caps are GitHub's shape with one spindle bound:
//
//   - one step's summary may be 1 MiB (over that, the step fails with a named
//     error: a summary that big is a runaway, not documentation);
//   - a job keeps at most 20 step summaries (GitHub's count);
//   - the joined summary is capped at MaxJobSummaryBytes (4 MiB) so a job
//     record in the object store stays a record.
//
// `$GITHUB_STATE` (the pre/post hand-off) is a sibling surface but not this
// file's: it is the per-action file under /spindle/state, read at the end of a
// JavaScript action's pre/main phase and delivered to its post hook as
// `STATE_<name>` (state.go, PAR-092, docs/threads.md §3.9a). A `::save-state::`
// command feeds the same map.
const (
	// SummaryEnvVar / SummaryEnvAlias are the environment variables holding
	// this step's summary-file path.
	SummaryEnvVar   = "SPINDLE_STEP_SUMMARY"
	SummaryEnvAlias = "GITHUB_STEP_SUMMARY"

	// summaryFilePrefix is the per-step in-container path.
	summaryFilePrefix = "/tmp/spindle-summary-"

	// MaxStepSummaryBytes bounds one step's collected summary (GitHub's 1 MiB).
	MaxStepSummaryBytes = 1 << 20
	// MaxStepSummaries bounds how many step summaries one job keeps.
	MaxStepSummaries = 20
)

// summaryFilePath is the in-container summary file of one flat step index
// (unique across composite expansion, like the output file).
func summaryFilePath(index int) string {
	return summaryFilePrefix + strconv.Itoa(index+1)
}

// jobSummary accumulates one job's step summaries.
type jobSummary struct {
	mu      sync.Mutex
	parts   []string
	bytes   int
	entries int
}

// add appends one step's summary text. An empty text is a no-op (most steps
// write none), and the caps are checked before anything is stored so a refused
// summary leaves no partial entry behind.
func (s *jobSummary) add(stepName, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(text) > MaxStepSummaryBytes {
		return fmt.Errorf("step summary is %d bytes, over the cap of %d bytes per step", len(text), MaxStepSummaryBytes)
	}
	if s.entries >= MaxStepSummaries {
		return fmt.Errorf("the job already has %d step summaries, over the cap of %d", s.entries, MaxStepSummaries)
	}
	// A separator keeps two summaries from fusing into one heading/paragraph.
	piece := text
	if len(s.parts) > 0 && !strings.HasSuffix(piece, "\n") {
		piece += "\n"
	}
	if s.bytes+len(piece) > threads.MaxJobSummaryBytes {
		return fmt.Errorf("the job summary would exceed the cap of %d bytes", threads.MaxJobSummaryBytes)
	}
	s.parts = append(s.parts, piece)
	s.bytes += len(piece)
	s.entries++
	return nil
}

// text joins the collected summaries ("" when none).
func (s *jobSummary) text() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.parts) == 0 {
		return ""
	}
	return strings.Join(s.parts, "\n")
}

// collectStepSummary reads one finished step's summary file out of the job
// container and appends it to the job state. A missing file is normal (the step
// wrote no summary). A malformed or oversized file is a named step error when
// the step otherwise succeeded; when it already failed, the caller only logs
// it, because the step's own failure is the more useful message.
func (e *Executor) collectStepSummary(ctx context.Context, plan *Plan, step StepPlan, summary *jobSummary, stream StepStream) error {
	if plan == nil || summary == nil || step.SummaryPath == "" {
		return nil
	}
	if ctx.Err() != nil || stream.Cancelled() {
		return nil // the container may have been killed by a timeout/cancel
	}
	limit := MaxStepSummaryBytes
	script := "if [ -f " + shQuote(step.SummaryPath) + " ]; then head -c " + strconv.Itoa(limit+1) + " " + shQuote(step.SummaryPath) + "; else exit 3; fi"
	out, stderr, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c", script)
	if code == 3 {
		return nil
	}
	if err != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return fmt.Errorf("%s (%s): %s", SummaryEnvAlias, step.SummaryPath, msg)
	}
	if len(out) > limit {
		return fmt.Errorf("%s (%s): the file is over the cap of %d bytes per step", SummaryEnvAlias, step.SummaryPath, limit)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	// The summary is rendered markdown, but it must never leak a secret: the
	// same mask the log uses is applied before the text is stored.
	return summary.add(step.Name, stream.Mask(out))
}
