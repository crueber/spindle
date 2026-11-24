package webui

// admin_restart_test.go — POST /admin/restart (docs/PLAN-ADMIN.md §5 as
// amended; docs/go/12_web_ui.md §2.10): the area gate, the typed confirmation,
// the one flushed signal through an INJECTED seam (a test never signals its own
// process), the audit record, the concurrent second request, the Configuration
// "Restart required" card, and the restricted-boot lane.
//
// The harness is settings_test.go's (one page provider, both seams bound).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// adminRestartDeps returns the base deps plus a recording Restart seam.
func adminRestartDeps(signals *int32, fired chan struct{}) testDeps {
	deps := adminBaseDeps()
	deps.Restart = func() {
		atomic.AddInt32(signals, 1)
		if fired != nil {
			select {
			case fired <- struct{}{}:
			default:
			}
		}
	}
	return deps
}

// zeroRestartDelay shortens the pre-signal pause for the duration of one test.
func zeroRestartDelay(t *testing.T) {
	t.Helper()
	old := adminRestartDelay
	adminRestartDelay = 0
	t.Cleanup(func() { adminRestartDelay = old })
}

func waitSignal(t *testing.T, fired <-chan struct{}) {
	t.Helper()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the injected restart seam was never called")
	}
}

// TestAdminRestartGateMatrix: the control is host-admin only, with the API's
// statuses (401 anonymous, 403 non-admin), and a correct request reaches the
// seam.
func TestAdminRestartGateMatrix(t *testing.T) {
	zeroRestartDelay(t)
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		access func(*http.Request, auth.Principal) *auth.AuthError
		want   int
		fires  bool
	}{
		{"anonymous", auth.Anonymous(), adminDeny, http.StatusUnauthorized, false},
		{"non-admin", auth.Principal{Name: "bob", Write: true}, adminDeny, http.StatusForbidden, false},
		{"host admin", settingsTestPrincipal(), adminDeny, http.StatusOK, true},
		{"restricted boot (open area)", auth.Anonymous(), func(*http.Request, auth.Principal) *auth.AuthError { return nil }, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var signals int32
			fired := make(chan struct{}, 1)
			deps := adminRestartDeps(&signals, fired)
			deps.Access = tc.access
			s := newSettingsServer(t, &settingsFakes{deps: deps, principal: tc.p})
			rec := settingsDo(t, s, http.MethodPost, "/admin/restart", url.Values{"confirm": {"restart"}}, nil)
			if rec.Code != tc.want {
				t.Fatalf("POST /admin/restart = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.fires {
				waitSignal(t, fired)
				settingsMustContain(t, rec.Body.String(), "Restarting", "the accepted response is the restarting page")
			} else if adminRestartClaimed() {
				t.Fatal("a refused request claimed the one-signal guard")
			}
		})
	}
}

// TestAdminRestartWrongConfirmation: a missing/wrong word is a 422 page with a
// named inline error and no signal.
func TestAdminRestartWrongConfirmation(t *testing.T) {
	zeroRestartDelay(t)
	var signals int32
	deps := adminRestartDeps(&signals, nil)
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	for _, form := range []url.Values{{"confirm": {""}}, {"confirm": {"Restart"}}, {"confirm": {"restart now"}}} {
		rec := settingsDo(t, s, http.MethodPost, "/admin/restart", form, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("POST /admin/restart (%v) = %d, want 422", form, rec.Code)
		}
		body := rec.Body.String()
		settingsMustContain(t, body, `Type <code class="mono text-xs">restart</code> to confirm`, "the inline error names the word")
		settingsMustContain(t, body, `role="alert"`, "the inline error is an alert")
		settingsMustContain(t, body, `id="admin-restart-confirm"`, "the form is rendered again")
		settingsMustContain(t, body, `data-danger-confirm="restart"`, "the re-rendered form keeps the typed confirm")
	}
	if adminRestartClaimed() {
		t.Fatal("a rejected confirmation claimed the one-signal guard")
	}
	if got := atomic.LoadInt32(&signals); got != 0 {
		t.Fatalf("seam called %d times on rejected confirmations", got)
	}
}

// TestAdminRestartSignalsOnceAndAudits: a correct request renders the restarting
// page, calls the injected seam exactly once, and records admin_restart; a
// second request on the same process is a 409 no-op.
func TestAdminRestartSignalsOnceAndAudits(t *testing.T) {
	zeroRestartDelay(t)
	var signals int32
	fired := make(chan struct{}, 4)
	deps := adminRestartDeps(&signals, fired)
	type audit struct{ actor, action, target, detail string }
	var audits []audit
	deps.AuditWrite = func(_ context.Context, actor, action, target, detail string) error {
		audits = append(audits, audit{actor, action, target, detail})
		return nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})

	rec := settingsDo(t, s, http.MethodPost, "/admin/restart", url.Values{"confirm": {"restart"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/restart = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Restarting", "shutting down now",
		"restarted by its supervisor", "container", "systemd", "restart: no",
	} {
		settingsMustContain(t, body, want, "restarting page states the truth")
	}
	settingsMustContain(t, body, "no in-process reload", "the page is honest about no in-process reload")
	if n := a11yH1Count(body); n != 1 {
		t.Fatalf("h1 count = %d, want 1", n)
	}
	waitSignal(t, fired)
	if got := atomic.LoadInt32(&signals); got != 1 {
		t.Fatalf("seam calls = %d, want 1", got)
	}
	if len(audits) != 1 || audits[0].action != "admin_restart" || audits[0].actor != "alice" ||
		audits[0].target != "instance:restart" {
		t.Fatalf("audit = %+v, want one admin_restart by alice on instance:restart", audits)
	}
	if !strings.Contains(audits[0].detail, "confirmation") {
		t.Fatalf("audit detail = %q, want the confirmation fact", audits[0].detail)
	}

	// The process-wide guard: a later request does not signal twice.
	rec = settingsDo(t, s, http.MethodPost, "/admin/restart", url.Values{"confirm": {"restart"}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second POST /admin/restart = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	settingsMustContain(t, rec.Body.String(), "Already requested", "the second request says so")
	if got := atomic.LoadInt32(&signals); got != 1 {
		t.Fatalf("seam calls after the second request = %d, want 1", got)
	}
	if len(audits) != 1 {
		t.Fatalf("audits after the second request = %d, want 1", len(audits))
	}
}

// TestAdminRestartConcurrentSingleSignal: two concurrent requests — the first
// wins, the other is a 409 no-op, and the seam fires exactly once.
func TestAdminRestartConcurrentSingleSignal(t *testing.T) {
	zeroRestartDelay(t)
	var signals int32
	fired := make(chan struct{}, 8)
	deps := adminRestartDeps(&signals, fired)
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})

	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.NewReader(url.Values{"confirm": {"restart"}}.Encode())
			r := httptest.NewRequest(http.MethodPost, "/admin/restart", body)
			r.Host = "example.com"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://example.com")
			rec := httptest.NewRecorder()
			<-start
			s.mux.ServeHTTP(rec, r)
			codes <- rec.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	okCount, conflictCount := 0, 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("concurrent restart answered %d", code)
		}
	}
	if okCount != 1 || conflictCount != n-1 {
		t.Fatalf("concurrent answers = %d ok / %d conflict, want 1 / %d", okCount, conflictCount, n-1)
	}
	waitSignal(t, fired)
	if got := atomic.LoadInt32(&signals); got != 1 {
		t.Fatalf("seam calls = %d, want exactly 1", got)
	}
}

// TestAdminRestartUnwired: a nil seam is an honest 503, never a fake success.
func TestAdminRestartUnwired(t *testing.T) {
	zeroRestartDelay(t)
	deps := adminBaseDeps() // Restart stays nil
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodPost, "/admin/restart", url.Values{"confirm": {"restart"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /admin/restart (unwired) = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAdminConfigRestartCard: a saved restart-required key renders the
// prominent card with the keys linked to their rows and the typed-confirm
// action; a save with no restart-required key does not.
func TestAdminConfigRestartCard(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminBaseDeps(), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/config?restart=server.listen,cache.max_bytes", nil, nil).Body.String()
	for _, want := range []string{
		`id="admin-restart-required"`, "Restart required", "Restart required for:",
		`href="/admin/config#row-server.listen"`, `href="/admin/config#row-cache.max_bytes"`,
		`action="/admin/restart"`, `data-danger-confirm="restart"`, `id="admin-restart-confirm"`,
		"restarted by its supervisor", "restart: no",
	} {
		settingsMustContain(t, body, want, "restart-required card")
	}
	// A no-op save renders the saved banner's honest line and no card.
	body = settingsDo(t, s, http.MethodGet, "/admin/config?saved=1&restart=", nil, nil).Body.String()
	settingsMustContain(t, body, "No key in this save requires a restart.", "no-op save banner")
	settingsMustNotContain(t, body, `id="admin-restart-required"`, "no card without restart keys")
	settingsMustNotContain(t, body, `action="/admin/restart"`, "no restart action without restart keys")
}
