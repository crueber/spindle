package threads

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// Live logs (15 §6): the same SSE envelope the task streams use (07 §9.3) —
// no new transport. A viewer attaches to one job attempt; the stream replays
// the stored frames (immutable, sequence-ordered) and then tails new ones until
// the job reaches a terminal state. The JSON twin (`?accept=json`, or no
// text/event-stream Accept) answers one bounded window instead.

// logTailInterval is the tail poll cadence. Frames are bucket objects, so the
// tail is a bounded prefix LIST per tick (the jobs are human-scale, not a hot
// path).
const logTailInterval = time.Second

// logStreamMax bounds one attachment (a viewer re-attaches; the page island
// does that automatically).
const logStreamMax = 30 * time.Minute

// acceptsEventStream reports whether the caller asked for SSE.
func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// logStream is the 07 §6 envelope writer (hand-rolled: the dependency budget
// forbids an SSE library). Packet writes and keepalives share one mutex so they
// cannot tear; a stalled client cannot pin the goroutine (15 s write deadline).
type logStream struct {
	w    http.ResponseWriter
	fl   http.Flusher
	rc   *http.ResponseController
	ctx  context.Context
	ka   *time.Ticker
	done chan struct{}
	mu   sync.Mutex
	end  bool
}

// logKeepalives counts live keepalive goroutines (never NumGoroutine: it is
// process-global and a healthy set reads as a leak).
var logKeepalives atomic.Int64

func newLogStream(w http.ResponseWriter, r *http.Request) (*logStream, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": walgit\n\n")
	fl.Flush()
	s := &logStream{w: w, fl: fl, rc: http.NewResponseController(w), ctx: r.Context(), done: make(chan struct{})}
	s.ka = time.NewTicker(10 * time.Second)
	logKeepalives.Add(1)
	go func() {
		defer logKeepalives.Add(-1)
		defer s.ka.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-s.ctx.Done():
				return
			case <-s.ka.C:
				if !s.comment(": keepalive") {
					return
				}
			}
		}
	}()
	return s, true
}

func (s *logStream) event(name, data string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end {
		return false
	}
	if s.ctx.Err() != nil {
		s.stopLocked()
		return false
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "+data+"\n\n"); err != nil {
		s.stopLocked()
		return false
	}
	s.fl.Flush()
	if name == "done" {
		s.stopLocked()
	}
	return true
}

func (s *logStream) comment(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end {
		return false
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(s.w, c+"\n\n"); err != nil {
		s.stopLocked()
		return false
	}
	s.fl.Flush()
	return true
}

func (s *logStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *logStream) stopLocked() {
	if !s.end {
		s.end = true
		close(s.done)
	}
	s.ka.Stop()
}

// streamLog answers the log endpoint: SSE when asked, a JSON window otherwise.
func (h *Handler) streamLog(w http.ResponseWriter, r *http.Request, repo, runID, jobID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	attempt, _ := strconv.Atoi(q.Get("attempt"))
	after, _ := strconv.Atoi(q.Get("after"))
	if q.Get("accept") == "json" || !acceptsEventStream(r) {
		limit, _ := strconv.Atoi(q.Get("n"))
		page, err := h.Svc.LogFrames(r.Context(), repo, runID, jobID, attempt, after, limit, p)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	// Validate the gate + job before switching protocols (a 401/404 must be a
	// real status, never a stream that opens and then dies).
	page, err := h.Svc.LogFrames(r.Context(), repo, runID, jobID, attempt, after, 500, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	s, ok := newLogStream(w, r)
	if !ok {
		writePlain(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	defer s.close()
	cursor := after
	deadline := time.Now().Add(logStreamMax)
	state := page.State
	for {
		for _, f := range page.Frames {
			if !s.event("log", marshalName(f)) {
				return
			}
			cursor = f.Seq
		}
		if terminal(state) {
			_ = s.event("done", marshalName(map[string]any{"state": state, "attempt": page.Attempt}))
			return
		}
		if time.Now().After(deadline) {
			_ = s.event("done", marshalName(map[string]any{"state": state, "attempt": page.Attempt, "timeout": true}))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(logTailInterval):
		}
		page, err = h.Svc.LogFrames(r.Context(), repo, runID, jobID, page.Attempt, cursor, 500, p)
		if err != nil {
			// A tail error is not fatal to the frame already delivered; report
			// it and end the stream (the island reconnects with backoff).
			_ = s.event("error", marshalName(map[string]any{"status": statusFor(err), "message": err.Error()}))
			return
		}
		state = page.State
	}
}

// compile-time assertion: the stream's principal source is the same chain.
var _ = auth.Anonymous
