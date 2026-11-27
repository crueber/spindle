package threads

// Wave-4 operational HTTP surface: schedules, pause, queue ordering,
// notifications/subscriptions, authoring validation and the runner fleet admin
// verbs. The repo-scoped routes live under /{o}/{r}/api/threads/…; the fleet
// admin verbs under /api/v1/admin/runners/…; the runner-authenticated verbs
// under /api/v1/threads/runners/…

import (
	"net/http"
	"strconv"
	"strings"
)

// --- schedules (PAR-154) -----------------------------------------------------

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.Svc.ListSchedules(r.Context(), strings.SplitN(repo, "/", 2)[0], strings.SplitN(repo, "/", 2)[1], p)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) scheduleEnable(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		ScheduleEntryID
		Enabled bool `json:"enabled"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"path": true, "cron": true, "timezone": true, "enabled": true}, &body) {
		return
	}
	row, err := h.Svc.SetScheduleEnabled(r.Context(), repo, body.ScheduleEntryID, body.Enabled, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (h *Handler) scheduleRunNow(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body ScheduleEntryID
	if !decodeStrict(w, r, 1<<20, map[string]bool{"path": true, "cron": true, "timezone": true}, &body) {
		return
	}
	runs, err := h.Svc.RunScheduleNow(r.Context(), repo, body, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"runs": runs})
}

// --- CI pause (PAR-157) ------------------------------------------------------

func (h *Handler) pauseRepo(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Paused bool `json:"paused"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"paused": true}, &body) {
		return
	}
	view, err := h.Svc.SetCIPaused(r.Context(), repo, body.Paused, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) queueSnapshot(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	snap, err := h.Svc.QueueSnapshotForRepo(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, snap)
}

// --- notifications and subscriptions (PAR-197) -------------------------------

func (h *Handler) listNotifications(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("n")); raw != "" {
		if v, err := atoiBounded(raw, 1, 100); err == nil {
			limit = v
		}
	}
	notes, err := h.Svc.ListNotifications(r.Context(), p, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"notifications": nonNilNotifications(notes)})
}

func (h *Handler) markNotificationsRead(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.MarkNotificationsRead(r.Context(), p); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"read": true})
}

func (h *Handler) getSubscription(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetSubscription(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) putSubscription(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Subscribed bool     `json:"subscribed"`
		Workflows  []string `json:"workflows,omitempty"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"subscribed": true, "workflows": true}, &body) {
		return
	}
	view, err := h.Svc.SetSubscription(r.Context(), repo, body.Subscribed, body.Workflows, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// --- authoring (PAR-196) -----------------------------------------------------

func (h *Handler) validateWorkflow(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		File    string `json:"file"`
		Content string `json:"content"`
	}
	if !decodeStrict(w, r, 2<<20, map[string]bool{"file": true, "content": true}, &body) {
		return
	}
	view, err := h.Svc.ValidateWorkflowForRepo(r.Context(), repo, p, body.File, []byte(body.Content))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// --- runner fleet admin (PAR-172…PAR-181) ------------------------------------

func (h *Handler) runnerFleetVerb(w http.ResponseWriter, r *http.Request, id string, verb []string) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return true
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch verb[0] {
	case "drain":
		if err := h.Svc.DrainRunner(r.Context(), id, p); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": RunnerDraining})
	case "resume":
		if err := h.Svc.ResumeRunner(r.Context(), id, p); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": RunnerActive})
	case "rotate-token":
		rot, err := h.Svc.RotateRunnerToken(r.Context(), id, p)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, rot)
	case "deregister":
		if err := h.Svc.DeregisterRunner(r.Context(), id, p); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "deregistered": true})
	case "group":
		var body struct {
			Group string `json:"group"`
		}
		if !decodeStrict(w, r, 1<<20, map[string]bool{"group": true}, &body) {
			return true
		}
		view, err := h.Svc.SetRunnerGroup(r.Context(), id, body.Group, p)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writePlain(w, http.StatusNotFound, "unknown runner operation")
	}
	return true
}

func (h *Handler) runnerGroups(w http.ResponseWriter, r *http.Request, rest []string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		groups, err := h.Svc.ListRunnerGroups(r.Context(), p)
		if err != nil {
			writeErr(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"groups": nonNilGroups(groups)})
		return true
	case len(rest) == 0 && r.Method == http.MethodPost:
		var in RunnerGroupInput
		if !decodeStrict(w, r, 1<<20, map[string]bool{"name": true, "id": true, "labels": true, "allow_all": true, "repos": true}, &in) {
			return true
		}
		view, err := h.Svc.CreateRunnerGroup(r.Context(), p, in)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusCreated, view)
		return true
	case len(rest) == 1 && r.Method == http.MethodPut:
		var in RunnerGroupInput
		if !decodeStrict(w, r, 1<<20, map[string]bool{"name": true, "id": true, "labels": true, "allow_all": true, "repos": true}, &in) {
			return true
		}
		view, err := h.Svc.UpdateRunnerGroup(r.Context(), p, rest[0], in)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, view)
		return true
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if err := h.Svc.DeleteRunnerGroup(r.Context(), p, rest[0]); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": rest[0]})
		return true
	}
	methodNotAllowed(w, "GET", "POST", "PUT", "DELETE")
	return true
}

// --- runner self-service protocol (PAR-180/PAR-181) --------------------------

func (h *Handler) runnerSelf(w http.ResponseWriter, r *http.Request, op []string) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return true
	}
	runner, err := h.Svc.authorizeRunner(r.Context(), bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		writeErr(w, err)
		return true
	}
	switch op[0] {
	case "drain":
		if err := h.Svc.DrainOwnRunner(r.Context(), runner); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": RunnerDraining})
	case "resume":
		if err := h.Svc.ResumeOwnRunner(r.Context(), runner); err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": RunnerActive})
	case "rotate":
		rot, rerr := h.Svc.RotateOwnRunnerToken(r.Context(), runner)
		if rerr != nil {
			writeErr(w, rerr)
			return true
		}
		writeJSON(w, http.StatusOK, rot)
	case "deregister":
		if derr := h.Svc.DeregisterOwnRunner(r.Context(), runner); derr != nil {
			writeErr(w, derr)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"deregistered": true})
	default:
		writePlain(w, http.StatusNotFound, "unknown runner operation")
	}
	return true
}

// --- small helpers -----------------------------------------------------------

func atoiBounded(raw string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, strconv.ErrRange
	}
	return n, nil
}

func nonNilNotifications(in []Notification) []Notification {
	if in == nil {
		return []Notification{}
	}
	return in
}

func nonNilGroups(in []RunnerGroupView) []RunnerGroupView {
	if in == nil {
		return []RunnerGroupView{}
	}
	return in
}
