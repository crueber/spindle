// admin_restart.go — the restart control (docs/PLAN-ADMIN.md §5, as amended;
// docs/go/12_web_ui.md §2.10).
//
//	POST /admin/restart    typed-confirm ("restart") → signal the restart seam
//
// The page offers the action in two places, from one handler and one partial:
//
//   - Configuration: a "Restart required" card after a save whose
//     setup.RestartKeys list is non-empty (the key list links each #row-<key>);
//   - Maintenance: a Restart card that is always available to a host admin.
//
// # WHAT A RESTART IS
//
// The process EXITS. A supervisor brings it back: a container restart policy,
// a Swarm service task, or systemd Restart=. There is no in-process reload and
// the page says so; with restart: no the instance stays down until an operator
// starts it. The seam is AdminDeps.Restart, bound by composition to a graceful
// self-SIGTERM (the process's existing two-phase drain handles the rest).
//
// # ONE SIGNAL
//
// Two concurrent requests must not signal twice. The first request wins and the
// process-wide guard marks it; a later request renders "restart already
// requested" with 409 and does nothing. The guard is in memory by design (it
// bounds one process's lifetime, and a reboot clears it with the process).
// BindAdmin resets it, so a freshly composed area starts with no request.
//
// The signal runs after the response is flushed (a short delay, adminRestartDelay)
// because the response must reach the client before the listener drains. Tests
// inject AdminDeps.Restart with a fake — no test ever signals its own process.
package webui

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// adminRestartWord is the exact typed confirmation ("restart"), the danger-form
// idiom the repo settings already use (guideline §7).
const adminRestartWord = "restart"

// adminRestartDelay is the pause between flushing the "restarting" page and
// invoking the seam: enough for the response to leave the process before the
// listener drains. A variable so tests run it at zero.
var adminRestartDelay = 200 * time.Millisecond

var (
	adminRestartMu        sync.Mutex
	adminRestartRequested bool
)

// resetAdminRestart clears the one-signal guard. BindAdmin calls it: a newly
// composed area has not requested a restart, and the tests rebind per server.
func resetAdminRestart() {
	adminRestartMu.Lock()
	adminRestartRequested = false
	adminRestartMu.Unlock()
}

// claimAdminRestart reports whether this request owns the one signal. The first
// caller wins; every later caller is a no-op (the page answers 409).
func claimAdminRestart() bool {
	adminRestartMu.Lock()
	defer adminRestartMu.Unlock()
	if adminRestartRequested {
		return false
	}
	adminRestartRequested = true
	return true
}

// adminRestartClaimed reports the one-signal guard. The tests read it to prove
// a refused request never claimed the signal (never by sleeping).
func adminRestartClaimed() bool {
	adminRestartMu.Lock()
	defer adminRestartMu.Unlock()
	return adminRestartRequested
}

// AdminRestartView is the restart page's state: the confirmation form (a
// rejected word carries Error), the flushed "restarting" page, or the 409
// already-requested page.
type AdminRestartView struct {
	// Restarting is the answer AFTER the signal was accepted: the process is
	// about to exit, the supervisor restarts it.
	Restarting bool
	// AlreadyRequested is the concurrent second request (no second signal).
	AlreadyRequested bool
	// Error is the named inline error for a missing/wrong confirmation word.
	Error string
}

// AdminRestartKey is one restart-required key with the row anchor that explains
// it (the Configuration card's list).
type AdminRestartKey struct {
	Key  string
	Href string
}

// adminRestartKeyLinks builds the Configuration card's key list. Empty entries
// are dropped (the ?restart= query is operator-visible input).
func adminRestartKeyLinks(keys []string) []AdminRestartKey {
	out := make([]AdminRestartKey, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out = append(out, AdminRestartKey{Key: k, Href: "/admin/config#row-" + k})
	}
	return out
}

// adminRestartSubmit answers POST /admin/restart: the area gate, the same-origin
// write rule, the typed confirmation, then one delayed, flushed signal.
func (s *Server) adminRestartSubmit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := adminDeps()
	if status, detail, allowed := s.adminGate(r, p); !allowed {
		s.errorPage(w, r, status, detail)
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	if got := strings.TrimSpace(r.PostFormValue("confirm")); got != adminRestartWord {
		msg := `Type "` + adminRestartWord + `" to confirm. The instance has not been signalled.`
		if got != "" {
			msg = `The confirmation did not match. Type "` + adminRestartWord + `" to confirm. The instance has not been signalled.`
		}
		s.adminRestartPage(w, r, p, http.StatusUnprocessableEntity, &AdminRestartView{Error: msg})
		return
	}
	if d.Restart == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "restart is not wired on this build")
		return
	}
	if !claimAdminRestart() {
		s.adminRestartPage(w, r, p, http.StatusConflict, &AdminRestartView{AlreadyRequested: true})
		return
	}
	// The audit record follows the accepted request, before the signal: the
	// proof that a host admin asked for this restart survives in the audit
	// family. A nil AuditWrite is the restricted boot (no collaboration
	// wiring), where the request is the documented recovery path.
	if d.AuditWrite != nil {
		if err := d.AuditWrite(r.Context(), p.Name, "admin_restart", "instance:restart", "typed confirmation accepted"); err != nil {
			log.Printf("webui admin restart audit %s: %v", r.URL.Path, err)
		}
	}
	s.adminRestartPage(w, r, p, http.StatusOK, &AdminRestartView{Restarting: true})
	// Flush the page BEFORE the signal, so it reaches the client even though
	// the process is about to drain. The short delay keeps the response ahead
	// of the shutdown.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	delay := adminRestartDelay
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("webui admin restart seam panicked: %v", rec)
			}
		}()
		d.Restart()
	}()
}

// adminRestartPage renders the restart page (its own page set: the confirmation
// form, the flushed "restarting" answer, or the 409 page). It is never cached.
func (s *Server) adminRestartPage(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, view *AdminRestartView) {
	page := &AdminPage{Restart: view}
	// No nav entry is active: this is a transient control answer, not a
	// destination in the area's page map.
	s.adminShell(p, page, "")
	page.Section = "restart"
	page.Title = "Restart the instance"
	page.Purpose = "Confirm the restart: the process exits and its supervisor starts it again."
	switch {
	case view.Restarting:
		page.Title = "Restarting the instance"
		page.Purpose = "The process is shutting down now; its supervisor starts it again."
	case view.AlreadyRequested:
		page.Title = "Restart already requested"
		page.Purpose = "This instance already accepted a restart request."
	}
	w.Header().Set("Cache-Control", "no-store")
	s.adminRender(w, r, p, status, page)
}
