// stream_leak_test.go — #73 regression: the per-user SSE keepalive
// goroutine (#13) must terminate on stream teardown. close() used to only
// Stop the ticker, whose channel Stop does not close, so `for range ka.C`
// blocked forever — one leaked goroutine per disconnected stream.
package notify

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// awaitKeepalives polls the package's own keepalive counter. It replaces the
// old runtime.NumGoroutine() assertions: the global count is racy under load
// (an unrelated goroutine exiting shrinks the delta, which reads as a leak —
// seen on CI as "goroutines still 7, want >= 8"), while this counter is owned
// by exactly the thing under test.
func awaitKeepalives(t *testing.T, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := activeKeepalives.Load()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: keepalives %d, want %d after 30s", what, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSEWriterKeepaliveExitsOnClose(t *testing.T) {
	const streams = 5
	writers := make([]*sseWriter, 0, streams)
	for i := 0; i < streams; i++ {
		r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil)
		w, ok := newSSEWriter(httptest.NewRecorder(), r)
		if !ok {
			t.Fatal("recorder must flush")
		}
		writers = append(writers, w)
	}
	awaitKeepalives(t, streams, "keepalive goroutines after attach")
	// The handler path: defer s.close() on disconnect.
	for _, w := range writers {
		w.close()
	}
	awaitKeepalives(t, 0, "keepalive goroutine leaked after close")
}

func TestSSEWriterKeepaliveExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil).WithContext(ctx)
	w, ok := newSSEWriter(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer w.close()
	awaitKeepalives(t, 1, "keepalive goroutine after attach")
	cancel()
	awaitKeepalives(t, 0, "keepalive goroutine leaked after ctx cancel")
}
