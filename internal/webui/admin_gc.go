// admin_gc.go — the packages GC control on the Maintenance page (PLAN-ADMIN
// §3.5/§8.6): a read-only dry run, a confirmed one-click pass, the durable
// last-run facts, and the live task card.
//
//	POST /admin/maintenance/gc          op=dry-run|apply  (AdminDeps.PackagesGCStart)
//	GET  /admin/maintenance/gc/stream   ?id=<task id>      (AdminDeps.PackagesGCAttach)
//
// The pass itself is the EXISTING packages-gc task (internal/packages,
// docs/features/14 §6) running at instance scope: the page starts it through
// the registered task seam (composition binds the same *wal.TaskTable the
// per-repo op lane uses), and joins a running pass instead of racing it.
// Narration (progress bars + the log tail) is the shared task card the
// transfer/import cards use — islands/tasks.js consumes the same data-task-*
// markup and the same SSE envelope.
//
// # Why the page does not count on the request path
//
// A GC's "how much would this reclaim" answer needs a mark pass over every
// registry — a LIST, which law 4 forbids on a request path. So the dry run is
// itself a task: the page starts it, the card narrates the walk, and the
// stored summary carries the number. The page only ever reads the stored
// summary and the live task record (exact-key reads, no LIST).
//
// # Last-run facts across restarts
//
// The task table is instance memory; packages/gc_state.json is the durable
// answer ("when it ran, what it reclaimed, whether it failed"). The page shows
// the most recent pass and the most recent applying pass separately, so a dry
// run can never masquerade as the last real run.
package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
)

// AdminGCTask is a started (or joined) packages-GC pass.
type AdminGCTask struct {
	ID     string
	Kind   string
	DryRun bool
	Joined bool
	Title  string
	Stream string
}

// AdminGCRun is one pass as the page renders it.
type AdminGCRun struct {
	At      string // RFC 3339 (the template renders the relative age)
	Age     string
	DryRun  bool
	Actor   string
	Repos   int
	Deleted int
	Bytes   string
	Uploads int
	OK      bool
	Error   string
	TaskID  string
	Summary string
}

// AdminGCStatus is the Maintenance page's packages-GC section.
type AdminGCStatus struct {
	// NotWired: the build has no task table/packages seam for a manual pass.
	// The scheduled cadence and the last-run facts can still be reported.
	NotWired bool
	// Restricted: the setup-only boot — no store exists to sweep, so there is
	// no control and no last run to report (the copy says why).
	Restricted bool
	Error      string

	// Running is the live instance pass (nil = none); LiveID is its task id
	// for the card's stream.
	Running *AdminGCRun
	LiveID  string

	Last      *AdminGCRun
	LastApply *AdminGCRun

	// Cadence and grace are the maintainer's own numbers (never duplicated
	// rules): the per-repo due gate and the just-finalized-blob grace.
	Cadence string
	Grace   string
	// Scheduled states what the maintainer already does, in one sentence.
	Scheduled string
}

// AdminGCStatusFrom projects the packages seam's answer for the page. The
// cadence/grace arguments are the maintainer's numbers, formatted by the
// caller (the page handler owns configuration spelling).
func AdminGCStatusFrom(st packages.GCStatus, cadence, grace string) *AdminGCStatus {
	v := &AdminGCStatus{Cadence: cadence, Grace: grace}
	if st.Running != nil {
		dry := st.Running.Params["dry_run"] == "1"
		v.Running = &AdminGCRun{
			At:     st.Running.Started,
			Age:    relTime(st.Running.Started),
			DryRun: dry,
			TaskID: st.Running.ID,
		}
		v.LiveID = st.Running.ID
	}
	if st.State != nil {
		v.Last = adminGCRun(st.State.Last)
		v.LastApply = adminGCRun(st.State.LastApply)
	}
	return v
}

// adminGCRun projects a stored pass.
func adminGCRun(r *packages.GCRun) *AdminGCRun {
	if r == nil {
		return nil
	}
	out := &AdminGCRun{
		At: r.At, DryRun: r.DryRun, Actor: r.Actor, Repos: r.Repos,
		Deleted: r.Deleted, Bytes: humanBytes(r.Bytes), Uploads: r.Uploads,
		OK: r.OK, Error: r.Error, TaskID: r.TaskID,
	}
	out.Age = relTime(r.At)
	verb := "reclaimed"
	if out.DryRun {
		verb = "would reclaim"
	}
	out.Summary = verb + " " + out.Bytes + " in " + itoa(out.Deleted) + " unreferenced blob(s) across " +
		itoa(out.Repos) + " registr" + plural(out.Repos, "y", "ies")
	if out.Uploads > 0 {
		out.Summary += " plus " + itoa(out.Uploads) + " expired upload(s)"
	}
	if r.PrunedTags > 0 {
		out.Summary += "; retention pruned " + itoa(r.PrunedTags) + " tag(s) and " +
			itoa(r.PrunedManifests) + " manifest version(s)"
	}
	if r.UsageDrift != 0 {
		out.Summary += "; repaired " + humanBytes(r.UsageDrift) + " of usage-counter drift"
	}
	return out
}

// gcTaskTitle is the card's plain-language title (dry run vs apply).
func gcTaskTitle(dry bool) string {
	if dry {
		return "counting what a sweep would reclaim"
	}
	return "reclaiming unreferenced package blobs"
}

// adminGCStart answers POST /admin/maintenance/gc: op=dry-run|apply. Apply is
// confirmed server-side too (a missing tick is a named inline error, never a
// silent start) — the page's checkbox is the affordance, this is the rule.
func (s *Server) adminGCStart(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "maintenance")
	if !ok {
		return
	}
	// The shared write preflight: a state-changing POST is same-origin only
	// (the Configuration page's rule, one function).
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	op := strings.TrimSpace(r.PostFormValue("op"))
	dryRun := op == "dry-run"
	if !dryRun && op != "apply" {
		s.adminGCBack(w, r, p, page, http.StatusUnprocessableEntity, "choose a dry run or an apply pass")
		return
	}
	if !dryRun && r.PostFormValue("confirm") != "apply" {
		s.adminGCBack(w, r, p, page, http.StatusUnprocessableEntity,
			`confirm the apply pass first — type "apply" in the confirmation field; it deletes unreferenced package blobs immediately, and a dry run counts without deleting`)
		return
	}
	d := adminDeps()
	if d.PackagesGCStart == nil {
		s.adminGCBack(w, r, p, page, http.StatusServiceUnavailable, "the packages GC control is not wired on this build")
		return
	}
	actor := p.Name
	if actor == "" {
		actor = "system"
	}
	task, err := d.PackagesGCStart(r.Context(), actor, dryRun)
	if err != nil {
		s.adminGCBack(w, r, p, page, http.StatusServiceUnavailable, err.Error())
		return
	}
	if task == nil || task.ID == "" {
		s.adminGCBack(w, r, p, page, http.StatusServiceUnavailable, "the packages GC task did not start")
		return
	}
	// Never a silent wait: land on the page with the card attached (law 7).
	target := "/admin/maintenance?gc=" + url.QueryEscape(task.ID)
	if task.DryRun {
		target += "&gc_dry=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// adminGCBack re-renders the page with a named error (status preserved).
func (s *Server) adminGCBack(w http.ResponseWriter, r *http.Request, p auth.Principal, page *AdminPage, status int, msg string) {
	page.Error = msg
	s.maintenanceLoadView(r.Context(), page)
	s.adminShell(p, page, "maintenance")
	s.maintenanceFill(r.Context(), page)
	s.adminRender(w, r, p, status, page)
}

// adminGCStream answers GET /admin/maintenance/gc/stream?id=<task>: the shared
// SSE envelope for the card (task packet, replay, live narration, terminal
// result). It is the same pump the repo task stream uses — one transport, no
// second progress protocol.
func (s *Server) adminGCStream(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdminPage(w, r, "maintenance")
	if !ok {
		return
	}
	d := adminDeps()
	if d.PackagesGCAttach == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "the packages GC stream is not wired on this build")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		s.errorPage(w, r, http.StatusBadRequest, "a task id is required")
		return
	}
	st, found, err := d.PackagesGCAttach(r.Context(), id)
	if err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !found {
		s.errorPage(w, r, http.StatusNotFound, "unknown or expired task")
		return
	}
	api.PumpTaskStream(w, r, st)
}
