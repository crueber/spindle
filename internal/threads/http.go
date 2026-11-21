package threads

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// Wire conventions (07 §2, the sibling feature packages' shape): JSON success,
// plain-text errors, arrays [] never null, RFC 3339 UTC, per-segment decoding,
// no-store on every GET (run state mutates in place), both lanes everywhere.
// Anonymous-denied reads get a real 401 with WWW-Authenticate: Bearer.

// ExposedTemplates lists the discovery endpoints[] entries this surface serves
// (14 §14.12 lane rule; registered from composition via api.RegisterExposed —
// the checks/import precedent, law 12). One entry per distinct path shape.
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/threads",
	"/{owner}/{repo}/api/threads/{run}",
}

// Authenticator resolves the request principal through Seam 2 (the server
// chain, injected by composition). Nil falls back to anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Handler is the Seam 1 surface: the threads JSON API on both lanes.
// Composition chains it in front of the core mux; Handle reports false for
// non-threads paths so the core mux answers.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a threads route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) == 0 || segs[0] == "api" || segs[0] == "api-browser" {
		return false // no top-level threads routes; all are repo-scoped
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner+"/"+repo, segs[3:])
	}
	return false
}

// ServeHTTP answers threads routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// splitPath splits the escaped path and decodes each segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment survives
// verbatim (fail closed downstream).
func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// --- writers ---------------------------------------------------------------

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
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
	writePlain(w, statusFor(err), err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "threads: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeStrict unmarshals a body after rejecting unknown top-level keys (fail
// closed on writes).
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, allowed map[string]bool, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if keys == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: expected an object")
		return false
	}
	for k := range keys {
		if !allowed[k] {
			writePlain(w, http.StatusBadRequest, "unknown field "+strconv.Quote(k))
			return false
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- routing ---------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, repo string, rest []string) bool {
	if len(rest) == 0 || rest[0] != "threads" {
		return false
	}
	rest = rest[1:]
	switch {
	case len(rest) == 0:
		switch r.Method {
		case http.MethodGet:
			h.listRuns(w, r, repo)
			return true
		case http.MethodPost:
			h.dispatch(w, r, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	case len(rest) == 1:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getRun(w, r, repo, rest[0])
		return true
	}
	return false
}

// --- handlers --------------------------------------------------------------

func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	n := defaultPageSize
	if v := q.Get("n"); v != "" {
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
	page, err := h.Svc.ListRuns(r.Context(), repo, p, q.Get("after"), n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetRun(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Workflow string `json:"workflow"`
		Ref      string `json:"ref"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"workflow": true, "ref": true}, &body) {
		return
	}
	runs, err := h.Svc.DispatchRun(r.Context(), repo, body.Ref, body.Workflow, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"runs": runs})
}
