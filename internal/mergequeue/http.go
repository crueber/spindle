package mergequeue

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// The queue's JSON API (docs/features/22_merge_queue.md §7). Same wire
// conventions as every feature surface (07 §2): JSON success, plain-text
// errors, both lanes, per-segment decoding, no-store on queue reads (the
// document changes with every entry step).
//
// The write verbs use the merge action's role floor (maintain); dequeue also
// admits the PR's author and its enqueuer. Every gate is re-checked in the
// service — the handler never decides authorization.

// ExposedTemplates lists the discovery entries this surface serves (the
// lane rule, law 12). Registered from composition via api.RegisterExposed.
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/merge-queue",
	"/{owner}/{repo}/api/merge-queue/enqueue",
	"/{owner}/{repo}/api/merge-queue/dequeue",
	"/{owner}/{repo}/api/merge-queue/pause",
	"/{owner}/{repo}/api/merge-queue/resume",
	"/{owner}/{repo}/api/pulls/{num}/merge-queue",
}

// Handler is the Seam 1 surface.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// Authenticator resolves the request principal through the server chain.
type Authenticator func(*http.Request) (auth.Principal, *auth.AuthError)

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a queue route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner, repo, segs[3:])
	}
	return false
}

// ServeHTTP answers queue routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) >= 2 && rest[0] == "pulls" {
		num, err := strconv.Atoi(rest[1])
		if err != nil || num <= 0 {
			return false
		}
		if len(rest) == 3 && rest[2] == "merge-queue" {
			if r.Method != http.MethodGet {
				methodNotAllowed(w, "GET")
				return true
			}
			h.entryForPR(w, r, owner, repo, num)
			return true
		}
		return false
	}
	if len(rest) == 0 || rest[0] != "merge-queue" {
		return false
	}
	rest = rest[1:]
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.list(w, r, owner, repo)
		return true
	}
	switch rest[0] {
	case "enqueue":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.enqueue(w, r, owner, repo)
		return true
	case "dequeue":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.dequeue(w, r, owner, repo)
		return true
	case "pause", "resume":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.pauseResume(w, r, owner, repo, rest[0] == "pause")
		return true
	}
	return false
}

// --- handlers -----------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	v, err := h.Svc.List(r.Context(), owner, repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) entryForPR(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	e, err := h.Svc.EntryForPR(r.Context(), owner, repo, num, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	if e == nil {
		writePlain(w, http.StatusNotFound, "pull request is not in the merge queue")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (h *Handler) enqueue(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Base     string `json:"base"`
		Number   int    `json:"number"`
		Priority int    `json:"priority"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Base) == "" || body.Number <= 0 {
		writePlain(w, http.StatusBadRequest, "base and number are required")
		return
	}
	e, err := h.Svc.Enqueue(r.Context(), owner, repo, p, EnqueueInput{Base: body.Base, Number: body.Number, Priority: body.Priority})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"entry": e})
}

func (h *Handler) dequeue(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Base   string `json:"base"`
		Number int    `json:"number"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Base) == "" || body.Number <= 0 {
		writePlain(w, http.StatusBadRequest, "base and number are required")
		return
	}
	e, err := h.Svc.Dequeue(r.Context(), owner, repo, body.Base, body.Number, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (h *Handler) pauseResume(w http.ResponseWriter, r *http.Request, owner, repo string, pause bool) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Base   string `json:"base"`
		Reason string `json:"reason"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	var err error
	if pause {
		err = h.Svc.Pause(r.Context(), owner, repo, body.Base, body.Reason, p)
	} else {
		err = h.Svc.Resume(r.Context(), owner, repo, body.Base, p)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// --- wire helpers -------------------------------------------------------------

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if d, err := url.PathUnescape(s); err == nil {
			s = d
		}
		out = append(out, s)
	}
	return out
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	// A second value would be ignored by Decode above; refuse it.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: trailing data")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "merge queue: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*auth.AuthError); ok {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writePlain(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		writePlain(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrForbidden):
		writePlain(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrUnauthorized):
		writePlain(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrInvalid):
		writePlain(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrUnavailable):
		writePlain(w, http.StatusServiceUnavailable, err.Error())
	default:
		writePlain(w, http.StatusInternalServerError, err.Error())
	}
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}
