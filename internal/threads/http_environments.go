package threads

// http_environments.go — the deployment-environment surface on both JSON lanes
// (docs/PLAN-THREADS-ENVIRONMENTS.md §9):
//
//	GET    {lane}/threads/environments                     list (maintain+)
//	GET    {lane}/threads/environments/{name}              one (maintain+)
//	PUT    {lane}/threads/environments/{name}              create/replace (maintain+)
//	DELETE {lane}/threads/environments/{name}              delete (maintain+)
//	GET    {lane}/threads/environments/{name}/secrets      names only (maintain+)
//	PUT    {lane}/threads/environments/{name}/secrets/{s}  write-only value
//	DELETE {lane}/threads/environments/{name}/secrets/{s}
//
// Every method is the same service call the settings page makes; the service
// re-checks the role, so the page and the API cannot drift. Secret values are
// accepted and never echoed — reads report names and timestamps only.

import "net/http"

// environments dispatches the environments subtree (path[0] the first segment
// after "threads/environments").
func (h *Handler) environments(w http.ResponseWriter, r *http.Request, repo string, path []string) {
	switch {
	case len(path) == 0:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.listEnvironments(w, r, repo)
	case len(path) == 1:
		switch r.Method {
		case http.MethodGet:
			h.getEnvironment(w, r, repo, path[0])
		case http.MethodPut:
			h.putEnvironment(w, r, repo, path[0])
		case http.MethodDelete:
			h.deleteEnvironment(w, r, repo, path[0])
		default:
			methodNotAllowed(w, "GET", "PUT", "DELETE")
		}
	case len(path) == 2 && path[1] == "secrets":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.listEnvironmentSecrets(w, r, repo, path[0])
	case len(path) == 3 && path[1] == "secrets":
		switch r.Method {
		case http.MethodPut:
			h.putEnvironmentSecret(w, r, repo, path[0], path[2])
		case http.MethodDelete:
			h.deleteEnvironmentSecret(w, r, repo, path[0], path[2])
		default:
			methodNotAllowed(w, "PUT", "DELETE")
		}
	case len(path) == 2 && path[1] == "variables":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.listEnvironmentVariables(w, r, repo, path[0])
	case len(path) == 3 && path[1] == "variables":
		switch r.Method {
		case http.MethodPut:
			h.putEnvironmentVariable(w, r, repo, path[0], path[2])
		case http.MethodDelete:
			h.deleteEnvironmentVariable(w, r, repo, path[0], path[2])
		default:
			methodNotAllowed(w, "PUT", "DELETE")
		}
	default:
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.Svc.ListEnvironments(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": list, "enabled": h.Svc.SecretsEnabled()})
}

func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	env, err := h.Svc.GetEnvironment(r.Context(), repo, name, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env, "enabled": h.Svc.SecretsEnabled()})
}

// putEnvironment accepts the protection rules (PAR-131):
//
//	{"required_reviewers":[…], "team_reviewers":["org/team"],
//	 "wait_timer_seconds":600, "prevent_self_review":true,
//	 "deployment_branches":["main","release/*"]}
//
// The reviewers are validated against the repository role ladder at save time
// and the limits are named (six reviewers+teams, a 6-hour timer).
func (h *Handler) putEnvironment(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in EnvironmentInput
	if !decodeStrict(w, r, 64<<10, map[string]bool{
		"required_reviewers": true, "team_reviewers": true, "wait_timer_seconds": true,
		"prevent_self_review": true, "deployment_branches": true,
	}, &in) {
		return
	}
	env, err := h.Svc.PutEnvironment(r.Context(), repo, name, in, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env})
}

func (h *Handler) deleteEnvironment(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteEnvironment(r.Context(), repo, name, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listEnvironmentSecrets(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	secrets, err := h.Svc.ListEnvironmentSecrets(r.Context(), repo, name, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": secrets, "enabled": h.Svc.SecretsEnabled()})
}

// putEnvironmentSecret takes {value} and never echoes it.
func (h *Handler) putEnvironmentSecret(w http.ResponseWriter, r *http.Request, repo, name, secret string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"value": true}, &in) {
		return
	}
	if err := h.Svc.SetEnvironmentSecret(r.Context(), repo, name, secret, in.Value, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteEnvironmentSecret(w http.ResponseWriter, r *http.Request, repo, name, secret string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteEnvironmentSecret(r.Context(), repo, name, secret, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listEnvironmentVariables(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	vars, err := h.Svc.ListEnvironmentVariables(r.Context(), repo, name, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variables": vars, "enabled": h.Svc.SecretsEnabled()})
}

// putEnvironmentVariable takes {value} and never echoes it.
func (h *Handler) putEnvironmentVariable(w http.ResponseWriter, r *http.Request, repo, name, variable string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"value": true}, &in) {
		return
	}
	if err := h.Svc.SetEnvironmentVariable(r.Context(), repo, name, variable, in.Value, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteEnvironmentVariable(w http.ResponseWriter, r *http.Request, repo, name, variable string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteEnvironmentVariable(r.Context(), repo, name, variable, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
