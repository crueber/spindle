// reports_http.go — the Seam 1 surface for reports and the audit viewer
// (PLAN-V2 §M.3). Both lanes answer through the same handler.
//
//	GET  /api/v1/moderation/reports?state=&after=&n=             host admin
//	POST /api/v1/moderation/reports/{owner}/{repo}/{id}/resolve  host admin
//	GET  /api/v1/moderation/audit?after=&n=                      host admin
//	GET  /{owner}/{repo}/api[-browser]/moderation/reports        repo admin
//	POST /{owner}/{repo}/api[-browser]/moderation/reports        authenticated read
//	POST /{owner}/{repo}/api[-browser]/moderation/reports/{id}/resolve  repo admin
//	POST /{owner}/{repo}/api[-browser]/moderation/reports/{id}/dismiss  repo admin
//
// Floors live inside the verbs (the one-gate rule); the handlers only resolve
// the principal, decode, and map sentinels to the 07 §2 statuses. Every body
// is no-store and every list is []-not-null.
package moderation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// handleReports dispatches the top-level moderation family. ok=false when the
// path is not one of this file's routes.
func (h *Handler) handleReports(w http.ResponseWriter, r *http.Request, segs []string) bool {
	// /api[-browser]/v1/moderation/...
	if len(segs) >= 4 && segs[2] == "moderation" {
		switch {
		case len(segs) == 4 && segs[3] == "reports":
			h.globalReports(w, r)
			return true
		case len(segs) == 4 && segs[3] == "audit":
			h.auditViewer(w, r)
			return true
		case len(segs) == 8 && segs[3] == "reports" && segs[7] == "resolve":
			// /api/v1/moderation/reports/{owner}/{repo}/{id}/resolve
			h.globalResolve(w, r, normPrincipal(segs[4]), normPrincipal(segs[5]), segs[6])
			return true
		}
	}
	return false
}

// handleRepoReports dispatches /{owner}/{repo}/api[-browser]/moderation/…
func (h *Handler) handleRepoReports(w http.ResponseWriter, r *http.Request, segs []string, owner, repo string) bool {
	if len(segs) < 5 || segs[3] != "moderation" || segs[4] != "reports" {
		return false
	}
	switch len(segs) {
	case 5:
		switch r.Method {
		case http.MethodGet:
			h.repoReportList(w, r, owner, repo)
		case http.MethodPost:
			h.repoReportFile(w, r, owner, repo)
		default:
			writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return true
	case 7:
		if (segs[6] == "resolve" || segs[6] == "dismiss") && r.Method == http.MethodPost {
			h.repoReportResolve(w, r, owner, repo, segs[5], segs[6])
			return true
		}
		if r.Method == http.MethodPost {
			writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
			return true
		}
		return false
	}
	return false
}

// repoReportList serves GET …/moderation/reports (repo admin).
func (h *Handler) repoReportList(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	rows, more, err := h.Svc.ListReports(r.Context(), owner, repo, p, r.URL.Query().Get("state"), after, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []ReportCard{}
	}
	writeJSON(w, http.StatusOK, struct {
		Reports []ReportCard `json:"reports"`
		More    bool         `json:"more"`
	}{Reports: rows, More: more})
}

// repoReportFile serves POST …/moderation/reports (authenticated, read floor).
func (h *Handler) repoReportFile(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if !apiSameOrigin(r) {
		writePlain(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in ReportInput
	if !decodeBody(w, r, &in) {
		return
	}
	card, err := h.Svc.FileReport(r.Context(), owner, repo, p, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ID     int        `json:"id"`
		Report ReportCard `json:"report"`
	}{ID: card.ID, Report: card})
}

// repoReportResolve serves POST …/reports/{id}/resolve and …/dismiss. The
// dismiss verb is the same endpoint shape: it accepts only `note` and records
// action none (the plan's "no shim" — one verb, one audit spelling).
func (h *Handler) repoReportResolve(w http.ResponseWriter, r *http.Request, owner, repo, idStr, verb string) {
	if !apiSameOrigin(r) {
		writePlain(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, perr := strconv.Atoi(idStr)
	if perr != nil || id < 1 {
		writePlain(w, http.StatusBadRequest, "bad report id")
		return
	}
	var in ReportResolve
	if !decodeBody(w, r, &in) {
		return
	}
	action := in.Action
	if verb == "dismiss" {
		if action != "" && action != ResolveNone {
			writePlain(w, http.StatusBadRequest, "dismiss accepts only note")
			return
		}
		action = ResolveNone
	}
	card, err := h.Svc.ResolveReport(r.Context(), owner, repo, id, p, action, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Report ReportCard `json:"report"`
	}{Report: card})
}

// globalReports serves GET /api/v1/moderation/reports (host admin).
func (h *Handler) globalReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	rows, more, err := h.Svc.ListGlobalReports(r.Context(), p, r.URL.Query().Get("state"), r.URL.Query().Get("after"), n)
	if err != nil {
		writeErr(w, err)
		return
	}
	type globalRow struct {
		GlobalCard
		Cursor string `json:"cursor"`
	}
	out := make([]globalRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, globalRow{GlobalCard: row, Cursor: GlobalCursor(row)})
	}
	writeJSON(w, http.StatusOK, struct {
		Reports []globalRow `json:"reports"`
		More    bool        `json:"more"`
	}{Reports: out, More: more})
}

// globalResolve serves POST /api/v1/moderation/reports/{owner}/{repo}/{id}/resolve
// (host admin): the repo twin delegates here, so there is exactly one writer.
func (h *Handler) globalResolve(w http.ResponseWriter, r *http.Request, owner, repo, idStr string) {
	if r.Method != http.MethodPost {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !apiSameOrigin(r) {
		writePlain(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous || !p.Admin {
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return
		}
		writePlain(w, http.StatusForbidden, "host admin required")
		return
	}
	if owner == "" || repo == "" {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	id, perr := strconv.Atoi(idStr)
	if perr != nil || id < 1 {
		writePlain(w, http.StatusBadRequest, "bad report id")
		return
	}
	var in ReportResolve
	if !decodeBody(w, r, &in) {
		return
	}
	card, err := h.Svc.ResolveReport(r.Context(), owner, repo, id, p, in.Action, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Repo   string     `json:"repo"`
		Report ReportCard `json:"report"`
	}{Repo: owner + "/" + repo, Report: card})
}

// auditViewer serves GET /api/v1/moderation/audit (host admin, no-store).
func (h *Handler) auditViewer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !p.Admin {
		writePlain(w, http.StatusForbidden, "host admin required")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	entries, more, err := h.Svc.AuditSince(r.Context(), after, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	if entries == nil {
		entries = []AuditEntry{}
	}
	writeJSON(w, http.StatusOK, struct {
		Entries []AuditEntry `json:"entries"`
		More    bool         `json:"more"`
	}{Entries: entries, More: more})
}

// decodeBody decodes one strict JSON body (unknown fields are a 400) and
// writes the error itself.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(v); derr != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+derr.Error())
		return false
	}
	return true
}
