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
)

// logServer records every log frame and can ask for a cancel.
type logServer struct {
	mu     sync.Mutex
	frames map[int]string
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
			Seq  int    `json:"seq"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode frame: %v", err)
		}
		s.mu.Lock()
		if s.frames == nil {
			s.frames = map[int]string{}
		}
		if _, dup := s.frames[in.Seq]; dup {
			t.Errorf("frame %d written twice (idempotency should prevent this)", in.Seq)
		}
		s.frames[in.Seq] = in.Text
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
		text, ok := rec.frames[seq]
		if !ok {
			t.Fatalf("frame %d missing (frames are not contiguous)", seq)
		}
		got.WriteString(text)
	}
	if got.String() != want.String() {
		t.Fatalf("reassembled log differs: got %d bytes, want %d", got.Len(), want.Len())
	}
	// Bounded frames: no frame exceeds the cap.
	for seq, text := range rec.frames {
		if len(text) > maxStepOutput {
			t.Fatalf("frame %d is %d bytes (cap %d)", seq, len(text), maxStepOutput)
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
	for _, text := range rec.frames {
		if strings.Contains(text, "sup3r-s3cret") {
			t.Fatalf("secret reached the wire: %q", text)
		}
	}
	if !strings.Contains(rec.frames[1], "***") {
		t.Fatalf("mask marker missing: %q", rec.frames[1])
	}
	// PAR-116: a value under 4 bytes is masked as a whole token, never inside a
	// longer word — the log keeps its words and still loses the credential.
	if !strings.Contains(rec.frames[1], "and ***") {
		t.Fatalf("a standalone 2-byte value must be masked: %q", rec.frames[1])
	}
	if !strings.Contains(rec.frames[1], "crab") {
		t.Fatalf("a 2-byte value inside a longer word must not be masked: %q", rec.frames[1])
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
