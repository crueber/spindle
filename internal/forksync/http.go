package forksync

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// Wire conventions: JSON success, plain-text errors, both lanes.

// ExposedTemplates lists the discovery endpoints this surface serves.
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/sync-fork",
}

// Handler is the Seam 1 surface.
type Handler struct {
	Svc  *Service
	Auth func(r *http.Request) (auth.Principal, *auth.AuthError)
}

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a sync-fork route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) < 4 || segs[0] == "api" || segs[0] == "api-browser" {
		return false
	}
	if segs[2] != "api" && segs[2] != "api-browser" {
		return false
	}
	owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
	if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
		return false
	}
	if len(segs[3:]) != 1 || segs[3] != "sync-fork" {
		return false
	}
	h.syncFork(w, r, owner, repo)
	return true
}

// ServeHTTP answers sync-fork routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) syncFork(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	switch r.Method {
	case http.MethodPost:
		if h.Svc == nil {
			writePlain(w, http.StatusServiceUnavailable, "sync fork not wired")
			return
		}
		id, err := h.Svc.Start(r.Context(), owner, repo, p)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := map[string]any{"task": map[string]any{"id": id}}
		if h.Svc.Tasks != nil {
			if info, ok := h.Svc.Tasks.Get(id); ok {
				out["task"] = info
			}
		}
		writeJSON(w, http.StatusAccepted, out)
	case http.MethodGet:
		if h.Svc == nil {
			writePlain(w, http.StatusServiceUnavailable, "sync fork not wired")
			return
		}
		if err := h.Svc.requireWrite(r.Context(), owner, repo, p); err != nil {
			// The outcome is maintainer-facing state; the read gate is the
			// same write gate the action uses (a reader learns nothing).
			writeErr(w, err)
			return
		}
		out := map[string]any{}
		if doc, err := h.Svc.LastOutcome(r.Context(), owner, repo); err != nil {
			writeErr(w, err)
			return
		} else if doc != nil {
			out["sync"] = doc
		}
		if h.Svc.Tasks != nil {
			if recs := r.URL.Query().Get("task"); recs != "" {
				if info, ok := h.Svc.Tasks.Get(recs); ok {
					out["task"] = info
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	default:
		methodNotAllowed(w, "GET", "POST")
	}
}

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if d, err := url.PathUnescape(s); err == nil {
			out = append(out, d)
		} else {
			out = append(out, s)
		}
	}
	return out
}

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
		writePlain(w, http.StatusInternalServerError, "sync fork: encode: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case isErr(err, ErrNotFound):
		return 404
	case isErr(err, ErrInvalid):
		return 400
	case isErr(err, ErrUnauthorized):
		return 401
	case isErr(err, ErrForbidden):
		return 403
	case isErr(err, ErrConflict):
		return 409
	case isErr(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

func isErr(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}
