// api.go — the JSON API twin of the Packages page (docs/features/14 §7):
// list tags/digests, one manifest, delete a tag, delete a manifest. Both API
// lanes are served; the gates are the registry's own (feature toggle, repo
// existence, identity CheckRead for reads and identity.CheckPush for deletes),
// so a page and its JSON twin can never disagree (the §2.4 parity lesson).
package packages

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ExposedTemplates lists the discovery entries[] paths this surface serves
// (Forgejo #272; registered from composition). One entry per distinct shape:
// both lanes and every method share a template.
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/packages",
	"/{owner}/{repo}/api/packages/{digest}",
	"/{owner}/{repo}/api/packages/tags/{tag}",
}

// HandleAPI answers /{o}/{r}/api[-browser]/packages…; false when the path is
// not this surface.
func (h *Handler) HandleAPI(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) < 4 || (segs[2] != "api" && segs[2] != "api-browser") || segs[3] != "packages" {
		return false
	}
	owner := segs[0]
	repo := strings.TrimSuffix(segs[1], ".git")
	rest := segs[4:]
	switch {
	case len(rest) == 0:
		if r.Method != http.MethodGet {
			apiMethod(w, "GET")
			return true
		}
		h.apiList(w, r, owner, repo)
	case len(rest) == 2 && rest[0] == "tags":
		if r.Method != http.MethodDelete {
			apiMethod(w, "DELETE")
			return true
		}
		h.apiDeleteTag(w, r, owner, repo, rest[1])
	case len(rest) == 1:
		switch r.Method {
		case http.MethodGet:
			h.apiGet(w, r, owner, repo, rest[0])
		case http.MethodDelete:
			h.apiDeleteManifest(w, r, owner, repo, rest[0])
		default:
			apiMethod(w, "GET", "DELETE")
		}
	default:
		apiPlain(w, http.StatusNotFound, "not found")
	}
	return true
}

// apiGate reproduces the registry gate for the JSON lanes in plain-text
// statuses: authenticate (401 + challenge), feature toggle, repo existence,
// then the action's role rule. Returns ok=false after writing the response.
func (h *Handler) apiGate(w http.ResponseWriter, r *http.Request, owner, repo, action string) bool {
	scope := pullScope(owner, repo)
	if action == "push" {
		scope = pushScope(owner, repo)
	}
	p, _, aerr := h.authenticate(r)
	if aerr != nil {
		h.challenge(w, r, scope)
		apiPlain(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	if p.JobRepo != "" {
		apiPlain(w, http.StatusForbidden, "a job token must be exchanged at /v2/token for a registry token")
		return false
	}
	if err := validateRepo(owner, repo); err != nil {
		apiPlain(w, http.StatusNotFound, "repository not found")
		return false
	}
	if !h.Svc.FeaturesOn(r.Context(), owner, repo) {
		apiPlain(w, http.StatusNotFound, "packages are disabled for this repository")
		return false
	}
	exists, eerr := h.Svc.RepoExists(r.Context(), owner, repo)
	if eerr != nil {
		apiPlain(w, http.StatusServiceUnavailable, "repository lookup failed")
		return false
	}
	if !exists {
		apiPlain(w, http.StatusNotFound, "repository not found")
		return false
	}
	var oerr *Error
	if action == "push" {
		oerr = h.Svc.CanPush(r.Context(), owner, repo, p)
	} else {
		oerr = h.Svc.CanPull(r.Context(), owner, repo, p)
	}
	if oerr != nil {
		status := oerr.Status
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			status = http.StatusInternalServerError
		}
		if status == http.StatusUnauthorized {
			h.challenge(w, r, scope)
		}
		apiPlain(w, status, oerr.Message)
		return false
	}
	return true
}

func (h *Handler) apiList(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if !h.apiGate(w, r, owner, repo, "pull") {
		return
	}
	limit := DefaultTagPage
	if v := r.URL.Query().Get("n"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	data, oerr := h.Svc.ListPackages(r.Context(), owner, repo, limit)
	if oerr != nil {
		apiFromOCI(w, oerr)
		return
	}
	base := h.pullBase(r) + "/" + owner + "/" + repo
	for i := range data.Packages {
		data.Packages[i].Pull = "docker pull " + base + ":" + data.Packages[i].Tag
	}
	apiJSON(w, http.StatusOK, data)
}

func (h *Handler) apiGet(w http.ResponseWriter, r *http.Request, owner, repo, digest string) {
	if !h.apiGate(w, r, owner, repo, "pull") {
		return
	}
	view, oerr := h.Svc.GetPackage(r.Context(), owner, repo, digest)
	if oerr != nil {
		apiFromOCI(w, oerr)
		return
	}
	apiJSON(w, http.StatusOK, view)
}

func (h *Handler) apiDeleteTag(w http.ResponseWriter, r *http.Request, owner, repo, tag string) {
	if !h.apiGate(w, r, owner, repo, "push") {
		return
	}
	if !IsTag(tag) {
		apiPlain(w, http.StatusNotFound, "tag not found")
		return
	}
	if oerr := h.Svc.DeleteManifest(r.Context(), owner, repo, tag); oerr != nil {
		apiFromOCI(w, oerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiDeleteManifest(w http.ResponseWriter, r *http.Request, owner, repo, digest string) {
	if !h.apiGate(w, r, owner, repo, "push") {
		return
	}
	if !IsDigest(digest) {
		apiPlain(w, http.StatusBadRequest, "invalid manifest digest")
		return
	}
	if oerr := h.Svc.DeleteManifest(r.Context(), owner, repo, digest); oerr != nil {
		apiFromOCI(w, oerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pullBase is the registry host a client should use in a pull command: the
// configured public URL when set, else the request host.
func (h *Handler) pullBase(r *http.Request) string {
	if h.Svc != nil && h.Svc.PublicURL != "" {
		if u, err := url.Parse(h.Svc.PublicURL); err == nil && u.Host != "" {
			return u.Host
		}
		return strings.TrimSuffix(h.Svc.PublicURL, "/")
	}
	return r.Host
}

// --- writers ------------------------------------------------------------------

func apiJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		apiPlain(w, http.StatusInternalServerError, "encoding response failed")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func apiPlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

func apiMethod(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	apiPlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// apiFromOCI maps a registry error onto the JSON API's status (its body is
// plain text like every sibling feature surface, never the OCI envelope).
func apiFromOCI(w http.ResponseWriter, e *Error) {
	status := e.Status
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	apiPlain(w, status, e.Message)
}
