// sse_leak_test.go — sibling of #73: the api SSE envelope carried the
// same `for range ka.C` shape, so Close() (ticker Stop only) leaked one
// keepalive goroutine per finished task stream. The writer now exits via
// its done channel or the request context.
package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// awaitSSEKeepalives polls the writer's own counter (the same reason the
// notify sibling does: runtime.NumGoroutine is process-global and racy).
func awaitSSEKeepalives(t *testing.T, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := activeKeepalives.Load()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: keepalives %d, want %d after 5s", what, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSEKeepaliveExitsOnClose(t *testing.T) {
	const streams = 5
	closers := make([]*SSE, 0, streams)
	for i := 0; i < streams; i++ {
		s, ok := NewSSE(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
		if !ok {
			t.Fatal("recorder must flush")
		}
		closers = append(closers, s)
	}
	awaitSSEKeepalives(t, streams, "api keepalives after attach")
	for _, s := range closers {
		s.Close()
	}
	awaitSSEKeepalives(t, 0, "api keepalive goroutine leaked after Close")
}

func TestSSEKeepaliveExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	s, ok := NewSSE(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer s.Close()
	awaitSSEKeepalives(t, 1, "api keepalive after attach")
	cancel()
	awaitSSEKeepalives(t, 0, "api keepalive goroutine leaked after ctx cancel")
}
