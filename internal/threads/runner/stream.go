package runner

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// StepStream is the write side a step sees. The real attempt has exactly one
// Streamer; a background step and each member of a `parallel:` group write to a
// bufferStream instead, so their output is flushed as one block in step order
// (PAR-080) rather than interleaving with unrelated steps. Masking, cancel
// detection and the job-global workflow-command sinks stay on the real stream.
type StepStream interface {
	Write(p []byte) (int, error)
	Systemf(format string, args ...any)
	StepStart(step StepPlan)
	StepEnd(step StepPlan, conclusion string, code int, ms int64)
	Cancelled() bool
	Mask(text string) string
	// root is the real attempt stream a nested buffer writes to.
	root() *Streamer
}

// bufferStream is one deferred-output sink. It is safe for concurrent use (a
// background step's docker exec writes from its own goroutine) and bounded: a
// runaway process is truncated with a stated note instead of growing the
// runner's memory without limit.
type bufferStream struct {
	parent *Streamer
	limit  int

	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func newBufferStream(parent *Streamer, limit int) *bufferStream {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &bufferStream{parent: parent, limit: limit}
}

func (b *bufferStream) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *bufferStream) Systemf(format string, args ...any) {
	fmt.Fprintf(b, "\n"+format+"\n", args...)
}

func (b *bufferStream) StepStart(step StepPlan) {
	name := step.Name
	if step.Kind == "checkout" {
		name += " (actions/checkout)"
	}
	b.Systemf("▸ step %d: %s", step.Index+1, name)
}

func (b *bufferStream) StepEnd(step StepPlan, conclusion string, code int, ms int64) {
	dur := time.Duration(ms) * time.Millisecond
	if conclusion == threads.JobSuccess {
		b.Systemf("◂ step %d: %s in %s", step.Index+1, conclusion, dur.Round(time.Millisecond))
		return
	}
	b.Systemf("◂ step %d: %s (exit %d) in %s", step.Index+1, conclusion, code, dur.Round(time.Millisecond))
}

func (b *bufferStream) Cancelled() bool         { return b.parent.Cancelled() }
func (b *bufferStream) Mask(text string) string { return b.parent.Mask(text) }
func (b *bufferStream) root() *Streamer         { return b.parent }

// flush writes the buffered block to the real stream and states truncation.
// The parent masks every frame, so a secret echoed into a background log is
// masked exactly like a foreground step's.
func (b *bufferStream) flush() {
	b.mu.Lock()
	text := b.buf.String()
	truncated := b.truncated
	b.buf.Reset()
	b.truncated = false
	b.mu.Unlock()
	if text != "" {
		_, _ = b.parent.Write([]byte(text))
	}
	if truncated {
		b.parent.Systemf("spindle: output truncated at %d bytes (a background/parallel step produced more than the runner buffers)", b.limit)
	}
}

// backgroundOutputLimit bounds one background step's buffered log. A dev server
// that logs forever is truncated with a note; the process itself is still killed
// at job end.
const backgroundOutputLimit = 1 << 20

// parallelOutputLimit bounds one parallel member's buffered log.
const parallelOutputLimit = 4 << 20
