package runner

// Defence in depth: a runner that did not opt in to fork runs refuses one it is
// handed anyway (the server should never offer it), reporting a failed attempt
// with a spindle: reason instead of executing fork code.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func TestRunJobRefusesForkRunWithoutOptIn(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var completion threads.CompleteInput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = w.Write([]byte(`{"token":"wjt_x","repo":"o/r","run_id":"r","job_id":"j","attempt":1}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(body, &completion)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"repo":"o/r","run_id":"r","job_id":"j","state":"failure","run_state":"failure"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r, err := New(Config{
		ServerURL: srv.URL, Token: "wrt_x", AllowHostDocker: true, Capacity: 1,
		Client: NewClient(srv.URL, "wrt_x"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.runJob(context.Background(), &threads.FetchResult{
		Repo: "o/r", RunID: "r", JobID: "j", JobRef: "jr_x", Attempt: 1, Trusted: false,
		Secrets: map[string]string{}, Mask: nil,
	})

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	gotCompletion := completion
	mu.Unlock()
	if len(gotPaths) != 2 || !strings.HasSuffix(gotPaths[0], "/start") || !strings.HasSuffix(gotPaths[1], "/complete") {
		t.Fatalf("protocol calls = %v, want start then complete (never an execute)", gotPaths)
	}
	if gotCompletion.Conclusion != threads.JobFailure || len(gotCompletion.Steps) != 1 {
		t.Fatalf("completion = %+v", gotCompletion)
	}
	if !strings.Contains(gotCompletion.Steps[0].Error, "--allow-fork-runs") {
		t.Fatalf("step error = %q, want the opt-in hint", gotCompletion.Steps[0].Error)
	}
}
