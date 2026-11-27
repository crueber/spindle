package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

func TestEphemeralRequiresCapacityOne(t *testing.T) {
	_, err := New(Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", Capacity: 2, Ephemeral: true})
	if err == nil || !strings.Contains(err.Error(), "capacity must be 1") {
		t.Fatalf("ephemeral capacity 2 = %v", err)
	}
}

func TestValidateRunnerSizingRejectsNegative(t *testing.T) {
	if err := threads.ValidateRunnerSizing(-1, 0); err == nil {
		t.Fatal("negative cpu accepted")
	}
	if err := threads.ValidateRunnerSizing(4, 16<<30); err != nil {
		t.Fatalf("valid sizing refused: %v", err)
	}
}

func TestRegisterRunnerCarriesDeclaration(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/threads/runners/register" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeTestJSON(w, map[string]any{"runner_id": "r1", "token": "wrt_new", "ephemeral": true})
	}))
	defer srv.Close()
	res, err := RegisterRunner(context.Background(), srv.URL, "wrg_x", RunnerOptions{
		Name: "one-shot", Labels: []string{"linux"}, Capacity: 1, Ephemeral: true, MaxCPUs: 4, MaxMemoryBytes: 16 << 30,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if res.Token != "wrt_new" {
		t.Fatalf("result = %+v", res)
	}
	if got["ephemeral"] != true || got["max_cpus"] != float64(4) || got["max_memory_bytes"] != float64(16<<30) {
		t.Fatalf("body = %+v", got)
	}
}

func TestFleetClientVerbs(t *testing.T) {
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = true
		if r.Header.Get("Authorization") != "Bearer wrt_x" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		writeTestJSON(w, map[string]any{"runner_id": "r1", "token": "wrt_new", "previous_valid_until": "2026-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_x")
	if err := c.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Deregister(context.Background()); err != nil {
		t.Fatal(err)
	}
	rot, err := c.Rotate(context.Background())
	if err != nil || rot.Token != "wrt_new" {
		t.Fatalf("rotate = %+v, %v", rot, err)
	}
	for _, p := range []string{"/api/v1/threads/runners/drain", "/api/v1/threads/runners/resume", "/api/v1/threads/runners/rotate", "/api/v1/threads/runners/deregister"} {
		if !seen[p] {
			t.Fatalf("missing call %s (seen %v)", p, seen)
		}
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestJobNamesUniquePerJobAndAttempt(t *testing.T) {
	a := jobNames(&threads.FetchResult{Repo: "o/r", RunID: "run1", JobID: "job1", Attempt: 1})
	b := jobNames(&threads.FetchResult{Repo: "o/r", RunID: "run1", JobID: "job2", Attempt: 1})
	c := jobNames(&threads.FetchResult{Repo: "o/r", RunID: "run1", JobID: "job1", Attempt: 2})
	seen := map[string]bool{}
	for _, n := range []Names{a, b, c} {
		if n.Container == "" || n.Network == "" || n.Volume == "" {
			t.Fatalf("incomplete names %+v", n)
		}
		for _, v := range []string{n.Container, n.Network, n.Volume} {
			if seen[v] {
				t.Fatalf("object name %q is reused across jobs/attempts", v)
			}
			seen[v] = true
		}
	}
	// Same job+attempt is deterministic (a retried plan names the same objects).
	again := jobNames(&threads.FetchResult{Repo: "o/r", RunID: "run1", JobID: "job1", Attempt: 1})
	if again.Container != a.Container || again.Network != a.Network || again.Volume != a.Volume {
		t.Fatalf("names are not deterministic: %+v vs %+v", a, again)
	}
}

func TestLegacyRegisterAndDeclareWrappers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"runner_id": "r1", "token": "wrt_x", "policy": map[string]any{"allow_all": true}})
	}))
	defer srv.Close()
	res, err := Register(context.Background(), srv.URL, "wrg_x", "name", []string{"linux"}, 1, false)
	if err != nil || res.RunnerID != "r1" {
		t.Fatalf("Register wrapper = %+v, %v", res, err)
	}
	view, err := NewClient(srv.URL, "wrt_x").Declare(context.Background(), "v", []string{"linux"}, 1, false)
	if err != nil || view == nil {
		t.Fatalf("Declare wrapper = %+v, %v", view, err)
	}
}

func TestNewValidationErrors(t *testing.T) {
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "server URL") {
		t.Fatalf("missing server = %v", err)
	}
	if _, err := New(Config{ServerURL: "http://127.0.0.1:1"}); err == nil || !strings.Contains(err.Error(), "runner token") {
		t.Fatalf("missing token = %v", err)
	}
	if _, err := New(Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", MaxCPUs: -1}); err == nil || !strings.Contains(err.Error(), "max cpus") {
		t.Fatalf("bad sizing = %v", err)
	}
}

func TestNewContainerPolicyErrors(t *testing.T) {
	if _, err := New(Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", Policy: ContainerPolicy{Network: "host"}}); err == nil {
		t.Fatal("host network accepted")
	}
	if _, err := New(Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", AllowForkRuns: true, Policy: ContainerPolicy{Privileged: true}}); err == nil {
		t.Fatal("fork runs with privilege accepted")
	}
}

func TestClientTransportErrors(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "wrt_x")
	ctx := context.Background()
	if err := c.Drain(ctx); err == nil {
		t.Fatal("drain transport error swallowed")
	}
	if _, err := c.Fetch(ctx, 1); err == nil {
		t.Fatal("fetch transport error swallowed")
	}
	if _, err := c.Start(ctx, "jr_x", 1); err == nil {
		t.Fatal("start transport error swallowed")
	}
	if err := c.Resume(ctx); err == nil {
		t.Fatal("resume transport error swallowed")
	}
	if err := c.Deregister(ctx); err == nil {
		t.Fatal("deregister transport error swallowed")
	}
	if _, err := c.Rotate(ctx); err == nil {
		t.Fatal("rotate transport error swallowed")
	}
}
