package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Log streaming (15 §2/§4, pass B2; PAR-190 log fidelity). Every job attempt
// owns ONE frame writer. Frames are bounded, sequenced, ordered by
// construction (a single flusher goroutine drains one record queue) and masked
// before they leave the process. A frame is a record, not a bare chunk: the
// runner stamps its capture time, says whether it emitted the line itself and
// names structural markers (a step start/end, a folded group, a truncation), so
// a reader renders step boundaries and groups instead of guessing from text.
//
// The same writer is the control channel: a log answer carrying cancel=true
// cancels the job context, and an idle job sends an empty heartbeat frame every
// 25 s (which renews the claim lease and polls for a cancel). That is why a
// silent spinner cannot happen.
const (
	frameFlush     = 32 << 10
	frameTicker    = 150 * time.Millisecond
	frameHeartbeat = 25 * time.Second
)

// pendingRecord is one queued log record (output bytes, a system line or a
// marker) waiting for the flusher. Keeping the queue typed — rather than one
// bytes.Buffer — is what makes a marker ordered exactly where it happened.
type pendingRecord struct {
	kind       string // "" output, or a threads.LogKind* marker
	stream     string // "output" (default) or "system"
	text       string
	step       int
	name       string
	conclusion string
	exitCode   int
	durationMS int64
}

// markerTime is one frame's capture time in the stored spelling.
func markerTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Streamer is one attempt's log writer (implements io.Writer for docker exec's
// combined output).
type Streamer struct {
	client  *Client
	token   string
	jobRef  string
	attempt int
	mask    []string
	maskMu  sync.RWMutex
	logger  *slog.Logger
	cancel  context.CancelFunc

	mu      sync.Mutex
	pending []pendingRecord
	bytes   int
	seq     int
	last    time.Time
	sent    int
	dropped int
	failed  bool
	refused bool
	closed  bool
	stopped chan struct{}
	// cancelled is set when the operator asked for a cancel (read under mu).
	cancelled bool
}

// NewStreamer builds and starts the flusher for one attempt.
func NewStreamer(client *Client, token, jobRef string, attempt int, mask []string, cancel context.CancelFunc, logger *slog.Logger) *Streamer {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Streamer{
		client: client, token: token, jobRef: jobRef, attempt: attempt,
		mask: append([]string{}, mask...), cancel: cancel, logger: logger,
		stopped: make(chan struct{}), last: time.Now(),
	}
	go s.loop()
	return s
}

// enqueue appends one record. A text record coalesces with the previous text
// record, so a chatty process does not allocate one record per Write.
func (s *Streamer) enqueue(rec pendingRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return
	}
	if rec.stream == "" {
		// A marker is the runner's own statement, never captured output.
		if rec.kind != "" {
			rec.stream = "system"
		} else {
			rec.stream = "output"
		}
	}
	if rec.kind == "" && rec.stream == "output" && len(s.pending) > 0 {
		last := &s.pending[len(s.pending)-1]
		if last.kind == "" && last.stream == "output" {
			last.text += rec.text
			s.bytes += len(rec.text)
			return
		}
	}
	s.pending = append(s.pending, rec)
	s.bytes += len(rec.text)
}

// Write buffers output (io.Writer). A writer that sees a flush boundary is
// still async: the flusher owns the HTTP call so a slow server backpressures
// through the buffer bound, not through the target process.
func (s *Streamer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.enqueue(pendingRecord{kind: "", text: string(p)})
	s.mu.Lock()
	full := s.bytes >= frameFlush
	s.mu.Unlock()
	if full {
		s.flush()
	}
	return len(p), nil
}

// Systemf writes a runner-emitted line (a skip note, a remedy, a background
// step's header). It is a system frame, never captured output.
func (s *Streamer) Systemf(format string, args ...any) {
	s.enqueue(pendingRecord{stream: "system", text: fmt.Sprintf(format, args...)})
}

// StepStart announces one step as a marker frame (PAR-190): the reader renders
// a boundary, not a line it happens to recognize.
func (s *Streamer) StepStart(step StepPlan) {
	name := step.Name
	if step.Kind == "checkout" {
		name += " (actions/checkout)"
	}
	s.enqueue(pendingRecord{kind: threads.LogKindStepStart, step: step.Index + 1, name: name})
}

// StepEnd records one step's outcome and duration as a marker frame.
func (s *Streamer) StepEnd(step StepPlan, conclusion string, code int, ms int64) {
	s.enqueue(pendingRecord{
		kind: threads.LogKindStepEnd, step: step.Index + 1, name: step.Name,
		conclusion: conclusion, exitCode: code, durationMS: ms,
	})
}

// Group opens a folded log group (`::group::`).
func (s *Streamer) Group(name string) {
	s.enqueue(pendingRecord{kind: threads.LogKindGroup, name: name})
}

// GroupEnd closes the innermost folded log group.
func (s *Streamer) GroupEnd() {
	s.enqueue(pendingRecord{kind: threads.LogKindGroupEnd})
}

// Truncated states a visible cap: the stream keeps everything it stored and
// says exactly what was dropped (never a silent truncation).
func (s *Streamer) Truncated(note string) {
	s.enqueue(pendingRecord{kind: threads.LogKindTruncated, stream: "system", text: note})
}

// Cancelled reports whether the operator's cancel reached this attempt.
func (s *Streamer) Cancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// Mask replaces every secret value in s (best effort, the same rule the server
// applies: exact substrings, longest first, values under 4 bytes skipped).
func (s *Streamer) Mask(text string) string {
	s.maskMu.RLock()
	mask := s.mask
	s.maskMu.RUnlock()
	return maskValues(text, mask)
}

// root is the StepStream seam: a nested buffer writes here.
func (s *Streamer) root() *Streamer { return s }

// addMask registers values with the mask list after the streamer started: the
// plan adds every secret it can substitute, so a `${{ secrets.X }}` spliced
// into a script is masked exactly like a variable delivered from the plan's
// environment, even when the payload's own mask list named it not at all.
func (s *Streamer) addMask(values []string) {
	if len(values) == 0 {
		return
	}
	s.maskMu.Lock()
	defer s.maskMu.Unlock()
	seen := make(map[string]bool, len(s.mask)+len(values))
	for _, v := range s.mask {
		seen[v] = true
	}
	merged := append([]string(nil), s.mask...)
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		merged = append(merged, v)
	}
	s.mask = merged
}

// maskValues is the shared masking rule (docs/threads.md §5.1): the server
// package owns the one implementation so a frame masked here and a frame
// masked before storage (AppendLog, Complete) cannot differ — including the
// multi-line and short-value rules.
func maskValues(text string, values []string) string {
	return threads.MaskValues(text, values)
}

// Close flushes the remaining buffer and stops the flusher. Idempotent.
func (s *Streamer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.stopped)
	s.flush()
	// A frame the server refused is a gap; if the server came back, say so
	// rather than leaving a silent hole in the log (the read side also renders
	// a sequence gap, so neither endpoint can hide one).
	s.mu.Lock()
	dropped := s.dropped
	refused := s.refused
	s.dropped = 0
	s.mu.Unlock()
	if dropped > 0 && !refused {
		s.sendOnce(pendingRecord{kind: threads.LogKindTruncated, stream: "system",
			text: fmt.Sprintf("spindle: %d log frame(s) were rejected by the server and are missing from this log", dropped)})
	}
}

// loop is the single flusher goroutine (one per job attempt, bounded by
// capacity).
func (s *Streamer) loop() {
	ticker := time.NewTicker(frameTicker)
	defer ticker.Stop()
	lastHeartbeat := time.Now()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		pending := len(s.pending) > 0
		idle := time.Since(lastHeartbeat)
		failed := s.failed
		s.mu.Unlock()
		if failed {
			return
		}
		if pending {
			s.flush()
			continue
		}
		if idle >= frameHeartbeat {
			lastHeartbeat = time.Now()
			s.heartbeat()
		}
	}
}

// take moves the queued records out under the lock (the flusher owns them).
func (s *Streamer) take() []pendingRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.pending
	s.pending = nil
	s.bytes = 0
	return recs
}

// flush sends every queued record, chunking output at the frame bound. A
// marker is its own frame, so its position relative to output is exact.
func (s *Streamer) flush() {
	for {
		recs := s.take()
		if len(recs) == 0 {
			return
		}
		for _, rec := range recs {
			if rec.kind == "" {
				if !s.sendText(rec) {
					return
				}
				continue
			}
			if !s.send(rec) {
				return
			}
		}
	}
}

// sendText splits one output/system record at the frame bound and sends each
// chunk in order.
func (s *Streamer) sendText(rec pendingRecord) bool {
	text := rec.text
	for len(text) > 0 {
		n := len(text)
		if n > maxStepOutput {
			n = maxStepOutput
		}
		chunk := pendingRecord{kind: "", stream: rec.stream, text: text[:n]}
		text = text[n:]
		if !s.send(chunk) {
			return false
		}
	}
	return true
}

// sendOnce is the best-effort single attempt the close-time gap notice uses
// (retrying three times against a server that just refused a frame would only
// stall the runner's exit).
func (s *Streamer) sendOnce(rec pendingRecord) bool {
	return s.sendTries(rec, 1)
}

// heartbeat sends an empty frame (lease renewal + cancel poll).
func (s *Streamer) heartbeat() {
	s.send(pendingRecord{kind: "", stream: "output", text: ""})
}

// send posts one frame, retrying transient failures three times. A frame is
// idempotent by sequence, so a retry after an ambiguous failure is safe. A
// final failure is counted and stated: the next successful send (or Close)
// writes a truncation marker, and a reader also sees the sequence gap.
func (s *Streamer) send(rec pendingRecord) bool { return s.sendTries(rec, 3) }

// sendTries posts one frame with a bounded retry budget.
func (s *Streamer) sendTries(rec pendingRecord, tries int) bool {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.last = time.Now()
	s.mu.Unlock()

	text := rec.text
	if rec.stream == "output" {
		text = s.Mask(text)
	}
	in := threads.LogRecord{
		Time: markerTime(), Stream: rec.stream, Kind: rec.kind, Text: text,
		Step: rec.step, Name: rec.name, Conclusion: rec.conclusion,
		ExitCode: rec.exitCode, DurationMS: rec.durationMS,
	}
	if rec.kind != "" && in.Stream == "" {
		in.Stream = "system"
	}
	if rec.kind != "" {
		in.Name = s.Mask(in.Name)
	}

	var err error
	for attempt := 0; attempt < tries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var view *threads.LogView
		view, err = s.client.Log(ctx, s.token, s.jobRef, s.attempt, seq, in)
		cancel()
		if err == nil {
			s.mu.Lock()
			s.sent++
			if view.Cancel && !s.cancelled {
				s.cancelled = true
				// The job context is cancelled from here: the running docker
				// exec dies with it and the executor concludes cancelled.
				s.cancel()
			}
			s.mu.Unlock()
			return true
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrSuperseded) {
			s.logger.Warn("threads runner: log frame rejected", "job", s.jobRef, "err", err)
			s.mu.Lock()
			s.dropped += 1
			s.failed = true
			s.refused = true
			s.mu.Unlock()
			return false
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	s.logger.Warn("threads runner: log frame dropped", "job", s.jobRef, "seq", seq, "err", err)
	s.mu.Lock()
	s.dropped += 1
	s.failed = true
	s.mu.Unlock()
	return false
}
