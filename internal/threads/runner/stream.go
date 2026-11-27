package runner

import (
	"fmt"
	"sync"

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
	Group(name string)
	GroupEnd()
	Truncated(note string)
	Cancelled() bool
	Mask(text string) string
	// root is the real attempt stream a nested buffer writes to.
	root() *Streamer
}

// bufferStream is one deferred-output sink. It is safe for concurrent use (a
// background step's docker exec writes from its own goroutine) and bounded: a
// runaway process is truncated with a stated note instead of growing the
// runner's memory without limit. It buffers RECORDS, so a step marker or a
// worked-command group written inside the block keeps its exact position.
type bufferStream struct {
	parent *Streamer
	limit  int

	mu        sync.Mutex
	recs      []pendingRecord
	bytes     int
	truncated bool
}

func newBufferStream(parent *Streamer, limit int) *bufferStream {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &bufferStream{parent: parent, limit: limit}
}

func (b *bufferStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.bytes
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	text := p
	if len(text) > remaining {
		text = text[:remaining]
		b.truncated = true
	}
	if n := len(b.recs); n > 0 {
		last := &b.recs[n-1]
		if last.kind == "" && last.stream == "output" {
			last.text += string(text)
			b.bytes += len(text)
			return len(p), nil
		}
	}
	b.recs = append(b.recs, pendingRecord{stream: "output", text: string(text)})
	b.bytes += len(text)
	return len(p), nil
}

func (b *bufferStream) Systemf(format string, args ...any) {
	b.push(pendingRecord{stream: "system", text: fmt.Sprintf(format, args...)})
}

func (b *bufferStream) StepStart(step StepPlan) {
	name := step.Name
	if step.Kind == "checkout" {
		name += " (actions/checkout)"
	}
	b.push(pendingRecord{kind: threads.LogKindStepStart, step: step.Index + 1, name: name})
}

func (b *bufferStream) StepEnd(step StepPlan, conclusion string, code int, ms int64) {
	b.push(pendingRecord{
		kind: threads.LogKindStepEnd, step: step.Index + 1, name: step.Name,
		conclusion: conclusion, exitCode: code, durationMS: ms,
	})
}

func (b *bufferStream) Group(name string) {
	b.push(pendingRecord{kind: threads.LogKindGroup, name: name})
}

func (b *bufferStream) GroupEnd() {
	b.push(pendingRecord{kind: threads.LogKindGroupEnd})
}

func (b *bufferStream) Truncated(note string) {
	b.push(pendingRecord{kind: threads.LogKindTruncated, stream: "system", text: note})
}

func (b *bufferStream) push(rec pendingRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recs = append(b.recs, rec)
}

func (b *bufferStream) Cancelled() bool         { return b.parent.Cancelled() }
func (b *bufferStream) Mask(text string) string { return b.parent.Mask(text) }
func (b *bufferStream) root() *Streamer         { return b.parent }

// flush hands the buffered block to the real stream (one contiguous run in the
// parent's queue) and states truncation. The parent masks every frame, so a
// secret echoed into a background log is masked exactly like a foreground
// step's.
func (b *bufferStream) flush() {
	b.mu.Lock()
	recs := b.recs
	truncated := b.truncated
	b.recs = nil
	b.bytes = 0
	b.truncated = false
	b.mu.Unlock()
	if len(recs) > 0 {
		b.parent.enqueue(recs[0])
		for _, rec := range recs[1:] {
			b.parent.enqueue(rec)
		}
	}
	if truncated {
		b.parent.Truncated(fmt.Sprintf("spindle: output truncated at %d bytes (a background/parallel step produced more than the runner buffers)", b.limit))
	}
}

// backgroundOutputLimit bounds one background step's buffered log. A dev server
// that logs forever is truncated with a note; the process itself is still killed
// at job end.
const backgroundOutputLimit = 1 << 20

// parallelOutputLimit bounds one parallel member's buffered log.
const parallelOutputLimit = 4 << 20
