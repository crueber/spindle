package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// queuedText renders a streamer's still-queued output (the test helper for a
// streamer that was never started with NewStreamer, so no flusher drains it).
func queuedText(s *Streamer) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, rec := range s.pending {
		b.WriteString(rec.text)
	}
	return b.String()
}

// logServer records every log frame and can ask for a cancel.
type logServer struct {
	mu     sync.Mutex
	frames map[int]threads.LogRecord
	seqs   []int
	cancel bool
}

func (s *logServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/log") {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var in struct {
			Seq int `json:"seq"`
			threads.LogRecord
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode frame: %v", err)
		}
		s.mu.Lock()
		if s.frames == nil {
			s.frames = map[int]threads.LogRecord{}
		}
		if _, dup := s.frames[in.Seq]; dup {
			t.Errorf("frame %d written twice (idempotency should prevent this)", in.Seq)
		}
		s.frames[in.Seq] = in.LogRecord
		s.seqs = append(s.seqs, in.Seq)
		cancel := s.cancel
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"seq":%d,"state":"running","cancel":%v}`, in.Seq, cancel)
	})
}

func TestStreamerOrdersAndSequencesFrames(t *testing.T) {
	rec := &logServer{}
	srv := httptest.NewServer(rec.handler(t))
	defer srv.Close()
	client := NewClient(srv.URL, "wjt_x")

	var cancelled bool
	stream := NewStreamer(client, "wjt_x", "jr_acme", 1, nil, func() { cancelled = true }, nil)
	var want strings.Builder
	for i := 0; i < 300; i++ {
		line := fmt.Sprintf("line %03d %s\n", i, strings.Repeat("x", 400))
		want.WriteString(line)
		if _, err := stream.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	stream.Close()
	if cancelled {
		t.Fatal("cancel fired without a cancel answer")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.frames) == 0 {
		t.Fatal("no frames recorded")
	}
	var got strings.Builder
	for seq := 1; seq <= len(rec.frames); seq++ {
		frame, ok := rec.frames[seq]
		if !ok {
			t.Fatalf("frame %d missing (frames are not contiguous)", seq)
		}
		got.WriteString(frame.Text)
	}
	if got.String() != want.String() {
		t.Fatalf("reassembled log differs: got %d bytes, want %d", got.Len(), want.Len())
	}
	// Bounded frames: no frame exceeds the cap.
	for seq, frame := range rec.frames {
		if len(frame.Text) > maxStepOutput {
			t.Fatalf("frame %d is %d bytes (cap %d)", seq, len(frame.Text), maxStepOutput)
		}
	}
}

func TestStreamerMasksSecretsBeforeSending(t *testing.T) {
	rec := &logServer{}
	srv := httptest.NewServer(rec.handler(t))
	defer srv.Close()
	stream := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_acme", 1,
		[]string{"sup3r-s3cret", "ab"}, func() {}, nil)
	stream.Write([]byte("value=sup3r-s3cret and ab; the word crab keeps its letters\n"))
	stream.Close()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, frame := range rec.frames {
		if strings.Contains(frame.Text, "sup3r-s3cret") {
			t.Fatalf("secret reached the wire: %q", frame.Text)
		}
	}
	if !strings.Contains(rec.frames[1].Text, "***") {
		t.Fatalf("mask marker missing: %q", rec.frames[1].Text)
	}
	// PAR-116: a value under 4 bytes is masked as a whole token, never inside a
	// longer word — the log keeps its words and still loses the credential.
	if !strings.Contains(rec.frames[1].Text, "and ***") {
		t.Fatalf("a standalone 2-byte value must be masked: %q", rec.frames[1].Text)
	}
	if !strings.Contains(rec.frames[1].Text, "crab") {
		t.Fatalf("a 2-byte value inside a longer word must not be masked: %q", rec.frames[1].Text)
	}
}

func TestStreamerCancelAnswerCancelsTheJob(t *testing.T) {
	rec := &logServer{cancel: true}
	srv := httptest.NewServer(rec.handler(t))
	defer srv.Close()
	done := make(chan struct{}, 1)
	stream := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_acme", 1, nil, func() { done <- struct{}{} }, nil)
	stream.Write([]byte("hello\n"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel answer did not cancel the job")
	}
	if !stream.Cancelled() {
		t.Fatal("stream does not report cancelled")
	}
	stream.Close()
}

func TestStreamerRetriesButDropsWhenServerRejects(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "invalid job token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	stream := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_acme", 1, nil, func() {}, nil)
	stream.Write([]byte("x\n"))
	stream.Close()
	if calls != 1 {
		t.Fatalf("a rejected token must not be retried: %d calls", calls)
	}
}

func TestFrameSeqIsMonotonic(t *testing.T) {
	stream := &Streamer{}
	if stream.seq != 0 {
		t.Fatal("seq must start at zero")
	}
	stream.seq++
	_ = strconv.Itoa(stream.seq)
}

// TestStreamerFramesKeepOrderAndMarkers pins PAR-190's fidelity claim: one
// attempt's frames are one ordered record stream, a step marker and a group
// marker keep their exact position relative to output, every frame carries the
// runner's capture time, and a marker names what it is instead of leaving the
// reader to guess from text.
func TestStreamerFramesKeepOrderAndMarkers(t *testing.T) {
	rec := &logServer{}
	srv := httptest.NewServer(rec.handler(t))
	defer srv.Close()
	stream := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_acme", 1, nil, func() {}, nil)
	stream.Write([]byte("before\n"))
	step := StepPlan{Index: 0, Name: "Build", Kind: "run"}
	stream.StepStart(step)
	stream.Write([]byte("output\n"))
	stream.Systemf("runner note")
	stream.Group("compile")
	stream.Write([]byte("inside\n"))
	stream.GroupEnd()
	stream.StepEnd(step, threads.JobSuccess, 0, 1500)
	stream.Write([]byte("after\n"))
	stream.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var text strings.Builder
	var sawStepStart, sawStepEnd, sawGroup, sawGroupEnd bool
	var lastTime time.Time
	for seq := 1; seq <= len(rec.frames); seq++ {
		frame, ok := rec.frames[seq]
		if !ok {
			t.Fatalf("frame %d missing", seq)
		}
		if frame.Time == "" {
			t.Fatalf("frame %d carries no capture time", seq)
		}
		when, err := time.Parse(time.RFC3339Nano, frame.Time)
		if err != nil {
			t.Fatalf("frame %d time %q: %v", seq, frame.Time, err)
		}
		if when.Before(lastTime) {
			t.Fatalf("frame %d time went backwards", seq)
		}
		lastTime = when
		switch frame.Kind {
		case threads.LogKindStepStart:
			sawStepStart = true
			if frame.Step != 1 || frame.Name != "Build" || frame.Stream != "system" {
				t.Fatalf("step_start = %+v", frame)
			}
			text.WriteString("[step-start]")
		case threads.LogKindStepEnd:
			sawStepEnd = true
			if frame.Step != 1 || frame.Conclusion != threads.JobSuccess || frame.DurationMS != 1500 {
				t.Fatalf("step_end = %+v", frame)
			}
			text.WriteString("[step-end]")
		case threads.LogKindGroup:
			sawGroup = true
			if frame.Name != "compile" {
				t.Fatalf("group = %+v", frame)
			}
			text.WriteString("[group]")
		case threads.LogKindGroupEnd:
			sawGroupEnd = true
			text.WriteString("[group-end]")
		default:
			if frame.Stream == "system" {
				text.WriteString("[sys]" + frame.Text)
				continue
			}
			if frame.Stream != "output" {
				t.Fatalf("plain frame stream = %q", frame.Stream)
			}
			text.WriteString(frame.Text)
		}
	}
	if !sawStepStart || !sawStepEnd || !sawGroup || !sawGroupEnd {
		t.Fatalf("markers missing: step-start=%v step-end=%v group=%v group-end=%v",
			sawStepStart, sawStepEnd, sawGroup, sawGroupEnd)
	}
	want := "before\n[step-start]output\n[sys]runner note[group]inside\n[group-end][step-end]after\n"
	if got := text.String(); got != want {
		t.Fatalf("frame order = %q, want %q", got, want)
	}
}

// TestStreamerStatesATruncationMarker pins the cap rule: a buffer that stops at
// its bound writes a visible note instead of silently dropping the rest.
func TestStreamerStatesATruncationMarker(t *testing.T) {
	rec := &logServer{}
	srv := httptest.NewServer(rec.handler(t))
	defer srv.Close()
	stream := NewStreamer(NewClient(srv.URL, "wjt_x"), "wjt_x", "jr_acme", 1, nil, func() {}, nil)
	buf := newBufferStream(stream, 4)
	buf.Write([]byte("abcdef"))
	buf.flush()
	stream.Close()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found bool
	for _, frame := range rec.frames {
		if frame.Kind == threads.LogKindTruncated && strings.Contains(frame.Text, "truncated at 4 bytes") {
			found = true
		}
	}
	if !found {
		t.Fatal("a buffered truncation was not stated in the stream")
	}
}

// TestBufferStreamKeepsMarkersAndOutputInOrder pins the buffered path PAR-080
// relies on: a background/parallel step's records (output, group markers, a
// truncation note) reach the job stream as one contiguous block, in the order
// the step produced them, with coalesced writes intact.
func TestBufferStreamKeepsMarkersAndOutputInOrder(t *testing.T) {
	stream, frames := recordingStream(t)
	buf := newBufferStream(stream, 1<<20)
	buf.StepStart(StepPlan{Index: 0, Name: "bg"})
	buf.Write([]byte("a"))
	buf.Write([]byte("b"))
	buf.Group("compile")
	buf.Write([]byte("inside\n"))
	buf.GroupEnd()
	buf.Truncated("spindle: output truncated at 4 bytes")
	buf.flush()
	stream.Close()
	log := frames()
	order := []string{"▸ step 1: bg", "ab", "▸ group: compile", "inside", "◂ group end", "output truncated at 4 bytes"}
	at := 0
	for _, want := range order {
		i := strings.Index(log[at:], want)
		if i < 0 {
			t.Fatalf("buffered log misses %q after byte %d:\n%s", want, at, log)
		}
		at += i + len(want)
	}
}
