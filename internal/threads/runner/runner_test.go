package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// stubProtocol speaks the runner protocol for the loop tests.
type stubProtocol struct {
	mu         sync.Mutex
	fetches    int
	fetchFails int // return 500 this many times first
	job        *threads.FetchResult
	completes  []threads.CompleteInput
	cancels    int
	declares   int
	startErr   int // HTTP status for start (0 = ok)
}

func (s *stubProtocol) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/threads/runners/declare", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.declares++
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"runner_id":"abc","repo":"acme/mono","state":"active","labels":["linux"],"lease_seconds":900}`))
	})
	mux.HandleFunc("/api/v1/threads/runners/fetch", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.fetches++
		fail := s.fetchFails > 0
		if fail {
			s.fetchFails--
		}
		job := s.job
		s.job = nil
		s.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if job == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	})
	mux.HandleFunc("/api/v1/threads/jobs/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/start"):
			s.mu.Lock()
			code := s.startErr
			s.mu.Unlock()
			if code != 0 {
				http.Error(w, "nope", code)
				return
			}
			_, _ = w.Write([]byte(`{"token":"wjt_attempt","expires_at":"2026-11-21T00:00:00Z","repo":"acme/mono","run_id":"r","job_id":"j","attempt":1}`))
		case strings.HasSuffix(path, "/log"):
			_, _ = w.Write([]byte(`{"seq":1,"state":"running","cancel":false}`))
		case strings.HasSuffix(path, "/complete"):
			var in threads.CompleteInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.mu.Lock()
			s.completes = append(s.completes, in)
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"repo":"acme/mono","run_id":"r","job_id":"j","state":"success","run_state":"success"}`))
		case strings.HasSuffix(path, "/cancel"):
			s.mu.Lock()
			s.cancels++
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"repo":"acme/mono","run_id":"r","job_id":"j","state":"cancelled","run_state":"cancelled"}`))
		default:
			t.Errorf("unexpected path %s", path)
			http.NotFound(w, r)
		}
	})
	return mux
}

func testRunner(t *testing.T, srvURL string, docker DockerCLI, capacity int) *Runner {
	t.Helper()
	r, err := New(Config{
		ServerURL: srvURL, Token: "wrt_test", Labels: []string{"linux"}, Capacity: capacity,
		DockerBin: "docker", AllowHostDocker: true, Version: "test",
		FetchWait: time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
		Executor: &Executor{Docker: docker, ServiceReadyTimeout: time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestRunnerClaimsExecutesAndCompletes(t *testing.T) {
	docker := &fakeDocker{streamOutput: "tests pass\n"}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "test", Run: "go test ./..."}}
	job.Attempt = 1
	job.Secrets = nil
	job.Mask = nil
	proto := &stubProtocol{job: job}
	srv := httptest.NewServer(proto.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testRunner(t, srv.URL, docker, 1).Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for {
		proto.mu.Lock()
		n := len(proto.completes)
		proto.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no completion within 10s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop on cancel")
	}
	proto.mu.Lock()
	defer proto.mu.Unlock()
	if proto.declares != 1 {
		t.Fatalf("declares = %d", proto.declares)
	}
	if len(proto.completes) != 1 || proto.completes[0].Conclusion != threads.JobSuccess {
		t.Fatalf("completes = %+v", proto.completes)
	}
	if len(proto.completes[0].Steps) != 1 || proto.completes[0].Steps[0].Conclusion != threads.JobSuccess {
		t.Fatalf("steps = %+v", proto.completes[0].Steps)
	}
	if !docker.called("rm -f sp-j-") {
		t.Fatalf("container not cleaned up: %v", docker.snapshot())
	}
}

func TestRunnerStopsOnRejectedCredential(t *testing.T) {
	proto := &stubProtocol{startErr: http.StatusUnauthorized}
	proto.job = fixtureJob()
	srv := httptest.NewServer(proto.handler(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := testRunner(t, srv.URL, &fakeDocker{}, 1).Run(ctx)
	if err == nil {
		// A 401 on fetch is the other route; here start 401s, which the worker
		// records as fatal too.
		t.Fatal("a rejected credential must stop the runner")
	}
}

func TestRunnerBacksOffInsteadOfSpinning(t *testing.T) {
	proto := &stubProtocol{fetchFails: 1000}
	srv := httptest.NewServer(proto.handler(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = testRunner(t, srv.URL, &fakeDocker{}, 1).Run(ctx)
	proto.mu.Lock()
	fetches := proto.fetches
	proto.mu.Unlock()
	if fetches == 0 {
		t.Fatal("runner never fetched")
	}
	if fetches > 20 {
		t.Fatalf("fetch loop did not back off: %d probes in 300ms", fetches)
	}
}

func TestRunnerSIGTERMCancelsTheJob(t *testing.T) {
	docker := &fakeDocker{blockOn: "exec --workdir"}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{Name: "slow", Run: "sleep 100"}}
	proto := &stubProtocol{job: job}
	srv := httptest.NewServer(proto.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testRunner(t, srv.URL, docker, 1).Run(ctx) }()
	time.Sleep(400 * time.Millisecond) // let the job start and block
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not stop on SIGTERM")
	}
	proto.mu.Lock()
	defer proto.mu.Unlock()
	if len(proto.completes) != 1 {
		t.Fatalf("completes = %+v", proto.completes)
	}
	if proto.completes[0].Conclusion != threads.JobCancelled {
		t.Fatalf("conclusion = %s, want cancelled", proto.completes[0].Conclusion)
	}
	if !docker.called("rm -f sp-j-") {
		t.Fatalf("container leaked on SIGTERM: %v", docker.snapshot())
	}
}

func TestRunnerRefusesHostDockerWithoutAck(t *testing.T) {
	_, err := New(Config{ServerURL: "http://x", Token: "t", Capacity: 1})
	if err == nil || !strings.Contains(err.Error(), "allow-host-docker") {
		t.Fatalf("New = %v", err)
	}
	if _, err := New(Config{ServerURL: "http://x", Token: "t", Capacity: 0, AllowHostDocker: true}); err != nil {
		t.Fatalf("New with ack: %v", err)
	}
	// The worker count is bounded by the same ceiling the server records.
	r, err := New(Config{ServerURL: "http://x", Token: "t", Capacity: 999, AllowHostDocker: true})
	if err != nil || r.cfg.Capacity != threads.MaxRunnerCapacity {
		t.Fatalf("capacity clamp = %+v, %v", r, err)
	}
}

func TestNextBackoffCaps(t *testing.T) {
	if got := nextBackoff(time.Second, 3*time.Second); got != 2*time.Second {
		t.Fatalf("backoff = %s", got)
	}
	if got := nextBackoff(2*time.Second, 3*time.Second); got != 3*time.Second {
		t.Fatalf("backoff cap = %s", got)
	}
}
