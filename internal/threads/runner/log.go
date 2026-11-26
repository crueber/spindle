package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Log streaming (15 §2/§4, pass B2): every job attempt owns ONE frame writer.
// Frames are bounded, sequenced, ordered by construction (a single flusher
// goroutine appends them under a mutex) and masked before they leave the
// process. The same writer is the control channel: a log answer carrying
// cancel=true cancels the job context, and an idle job sends an empty
// heartbeat frame every 25 s (which renews the claim lease and polls for a
// cancel). That is why a silent spinner cannot happen.
const (
	frameFlush     = 32 << 10
	frameTicker    = 150 * time.Millisecond
	frameHeartbeat = 25 * time.Second
)

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
	buf     bytes.Buffer
	seq     int
	last    time.Time
	sent    int
	failed  int
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

// Write buffers output (io.Writer). A writer that sees a flush boundary is
// still async: the flusher owns the HTTP call so a slow server backpressures
// through the buffer bound, not through the target process.
func (s *Streamer) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	n, err := s.buf.Write(p)
	full := s.buf.Len() >= frameFlush
	s.mu.Unlock()
	if full {
		s.flush()
	}
	return n, err
}

// Systemf writes a runner-emitted line (a step header, a skip note).
func (s *Streamer) Systemf(format string, args ...any) {
	fmt.Fprintf(s, "\n"+format+"\n", args...)
}

// StepStart announces one step.
func (s *Streamer) StepStart(step StepPlan) {
	name := step.Name
	if step.Kind == "checkout" {
		name += " (actions/checkout)"
	}
	s.Systemf("▸ step %d: %s", step.Index+1, name)
}

// StepEnd records one step's outcome in the stream.
func (s *Streamer) StepEnd(step StepPlan, conclusion string, code int, ms int64) {
	dur := time.Duration(ms) * time.Millisecond
	if conclusion == threads.JobSuccess {
		s.Systemf("◂ step %d: %s in %s", step.Index+1, conclusion, dur.Round(time.Millisecond))
		return
	}
	s.Systemf("◂ step %d: %s (exit %d) in %s", step.Index+1, conclusion, code, dur.Round(time.Millisecond))
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

// maskValues is the shared masking rule.
func maskValues(text string, values []string) string {
	if text == "" || len(values) == 0 {
		return text
	}
	sorted := append([]string(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, v := range sorted {
		if len(v) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
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
		pending := s.buf.Len() > 0
		idle := time.Since(lastHeartbeat)
		s.mu.Unlock()
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

// flush appends one frame from the buffer (no-op when empty).
func (s *Streamer) flush() {
	for {
		s.mu.Lock()
		if s.buf.Len() == 0 {
			s.mu.Unlock()
			return
		}
		chunk := make([]byte, min(s.buf.Len(), maxStepOutput))
		n, _ := s.buf.Read(chunk)
		s.seq++
		seq := s.seq
		s.last = time.Now()
		s.mu.Unlock()

		text := s.Mask(string(chunk[:n]))
		if !s.send(seq, text) {
			return
		}
	}
}

// heartbeat sends an empty frame (lease renewal + cancel poll).
func (s *Streamer) heartbeat() {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	s.send(seq, "")
}

// send posts one frame, retrying transient failures three times. A frame is
// idempotent by sequence, so a retry after an ambiguous failure is safe.
func (s *Streamer) send(seq int, text string) bool {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var view *threads.LogView
		view, err = s.client.Log(ctx, s.token, s.jobRef, s.attempt, seq, text)
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
			return false
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	s.logger.Warn("threads runner: log frame dropped", "job", s.jobRef, "seq", seq, "err", err)
	return false
}
