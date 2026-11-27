package threads

// http_deployments.go — the deployments API on both JSON lanes (PAR-133):
//
//	GET  {lane}/threads/deployments?environment=&after=&n=   list (read gate)
//	POST {lane}/threads/deployments                          create (write / deployments: write)
//	GET  {lane}/threads/deployments/{id}                     one (read gate)
//	POST {lane}/threads/deployments/{id}/statuses            append a status
//
// A workflow job token is accepted only for its own repository and only with
// the `deployments` scope (`write` for both create and a status): it can never
// act as an admin or reach another repository (PAR-133/S10). Every method is
// the same service call the environment page and the job gate use.

import (
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) deployments(w http.ResponseWriter, r *http.Request, repo string, path []string) {
	switch {
	case len(path) == 0:
		switch r.Method {
		case http.MethodGet:
			h.listDeployments(w, r, repo)
		case http.MethodPost:
			h.createDeployment(w, r, repo)
		default:
			methodNotAllowed(w, "GET", "POST")
		}
	case len(path) == 1:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.getDeployment(w, r, repo, path[0])
	case len(path) == 2 && path[1] == "statuses":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.createDeploymentStatus(w, r, repo, path[0])
	default:
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) listDeployments(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	n := defaultPageSize
	if v := strings.TrimSpace(q.Get("n")); v != "" {
		parsed, cerr := strconv.Atoi(v)
		if cerr != nil || parsed < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if parsed > maxPageSize {
			writePlain(w, http.StatusBadRequest, "invalid n: max 200")
			return
		}
		n = parsed
	}
	page, err := h.Svc.ListDeployments(r.Context(), repo, q.Get("environment"), p, q.Get("after"), n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// createDeployment accepts:
//
//	{"ref":"main","environment":"production","description":"…",
//	 "url":"https://…","log_url":"https://…","sha":"<40/64 hex>",
//	 "task":"deploy","transient_environment":false,"production_environment":true}
func (h *Handler) createDeployment(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in DeploymentInput
	if !decodeStrict(w, r, 64<<10, map[string]bool{
		"ref": true, "environment": true, "description": true, "url": true,
		"log_url": true, "sha": true, "task": true,
		"transient_environment": true, "production_environment": true,
	}, &in) {
		return
	}
	doc, err := h.Svc.CreateDeployment(r.Context(), repo, in, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"deployment": doc})
}

func (h *Handler) getDeployment(w http.ResponseWriter, r *http.Request, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	doc, err := h.Svc.GetDeployment(r.Context(), repo, id, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": doc})
}

func (h *Handler) createDeploymentStatus(w http.ResponseWriter, r *http.Request, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in DeploymentStatusInput
	if !decodeStrict(w, r, 32<<10, map[string]bool{
		"state": true, "description": true, "environment_url": true, "log_url": true,
	}, &in) {
		return
	}
	doc, err := h.Svc.UpdateDeploymentStatus(r.Context(), repo, id, in, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"deployment": doc})
}
