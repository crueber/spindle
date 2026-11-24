package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// readyBody is the ingress contract's wire shape: the status field is the
// pinned fact (the 2026-11-22 incident: a restricted instance answered 503 and
// the health-gated ingress routed nothing, so /admin was unreachable).
type readyBody struct {
	Status  string   `json:"status"`
	Version string   `json:"version"`
	Surface string   `json:"surface"`
	Errors  []string `json:"errors"`
}

func readyzBody(t *testing.T, s *Server) (int, readyBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestReadyzRestrictedIsReadyForItsSurface pins the amended contract
// (PLAN-OIDC-ONLY §2.1, 06_server_http.md §10.2): the restricted boot reports
// 200 {"status":"restricted","surface":"admin","version":…} because that is
// the surface it actually serves. Warming (a store prewarm that a restricted
// boot does not run) must not downgrade it.
func TestReadyzRestrictedIsReadyForItsSurface(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) {
		o.Boot = BootState{Mode: "setup_only", FileState: "invalid",
			Errors: []string{`store.backend: unknown backend "tape"`}}
	})
	SetPrewarmPending(3)
	defer SetPrewarmPending(0)

	status, body := readyzBody(t, s)
	if status != http.StatusOK {
		t.Fatalf("restricted readyz = %d, want 200 (%+v)", status, body)
	}
	if body.Status != "restricted" {
		t.Fatalf("restricted readyz status = %q, want \"restricted\"", body.Status)
	}
	if body.Surface != "admin" {
		t.Fatalf("restricted readyz surface = %q, want \"admin\"", body.Surface)
	}
	if body.Version != "testsha" {
		t.Fatalf("restricted readyz version = %q, want testsha", body.Version)
	}
	if len(body.Errors) != 1 || body.Errors[0] != `store.backend: unknown backend "tape"` {
		t.Fatalf("restricted readyz errors = %v, want the boot validation errors", body.Errors)
	}

	// /healthz keeps its liveness meaning (and is not the readiness answer).
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !containsJSONStatus(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %s, want 200 ok", rec.Code, rec.Body.String())
	}
}

// containsJSONStatus avoids pulling strings into this file's contract test.
func containsJSONStatus(body, want string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	return m["status"] == want
}

// TestReadyzRestrictedStillUnreadyWhileDraining: readiness for the surface is
// not a constant — a phase-2 drain stops the ingress routing even in the
// restricted boot (the process is going away; a new request would fail).
func TestReadyzRestrictedStillUnreadyWhileDraining(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) {
		o.Boot = BootState{Mode: "setup_only", FileState: "invalid"}
	})
	s.drain.Begin2()

	status, body := readyzBody(t, s)
	if status != http.StatusServiceUnavailable || body.Status != "draining" {
		t.Fatalf("draining restricted readyz = %d %+v, want 503 draining", status, body)
	}
}

// TestReadyzNormalStaysReady guards the other half: a normal boot keeps the
// pre-existing readiness meaning (ready 200, warming 503) and never reports
// the restricted status.
func TestReadyzNormalStaysReady(t *testing.T) {
	s, _ := newTestServer(t, nil)

	status, body := readyzBody(t, s)
	if status != http.StatusOK || body.Status != "ready" {
		t.Fatalf("normal readyz = %d %+v, want 200 ready", status, body)
	}
	if body.Surface != "" {
		t.Fatalf("normal readyz surface = %q, want empty (no restricted claim)", body.Surface)
	}

	SetPrewarmPending(2)
	defer SetPrewarmPending(0)
	status, body = readyzBody(t, s)
	if status != http.StatusServiceUnavailable || body.Status != "warming" {
		t.Fatalf("warming readyz = %d %+v, want 503 warming", status, body)
	}
}
