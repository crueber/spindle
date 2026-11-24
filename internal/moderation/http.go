// http.go — the Seam 1 surface for blocks (PLAN-V2 §M.3): top-level twins on
// both lanes, plain-text errors, no-store bodies, []-never-null.
//
//	GET    /api/v1/users/{principal}/blocks              self or host admin
//	PUT    /api/v1/users/{principal}/blocks/{subject}    self or host admin
//	DELETE /api/v1/users/{principal}/blocks/{subject}    self or host admin
//
// The API-browser lane is the same handler. The list is private to its
// owner: only the principal themself or a host admin may read or edit it
// (the plan's role matrix, §M.5) — a 403 is the answer for everyone else,
// and it never distinguishes "blocked you" from "not your list".
package moderation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
)

// ExposedTemplates lists the discovery endpoints[] entries (14 §14.12 lane
// rule; registered from composition via api.RegisterExposed in the same
// change — law 12/Forgejo #272). Methods share a template.
var ExposedTemplates = []string{
	"/api/v1/users/{principal}/blocks",
	"/api/v1/users/{principal}/blocks/{subject}",
	"/api/v1/moderation/reports",
	"/api/v1/moderation/reports/{owner}/{repo}/{id}/resolve",
	"/api/v1/moderation/audit",
	"/{owner}/{repo}/api/moderation",
	"/{owner}/{repo}/api/moderation/reports",
	"/{owner}/{repo}/api/moderation/reports/{id}/resolve",
	"/{owner}/{repo}/api/moderation/reports/{id}/dismiss",
}

// Authenticator resolves the request principal through Seam 2 (the server's
// auth chain, injected by composition).
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Handler is the Seam 1 surface.
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

// Handle answers one request; false when the path is not a moderation route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	// Repo-scoped lane: /{owner}/{repo}/api[-browser]/moderation[/reports…]
	// (M2 document, M3 reports).
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") &&
		segs[0] != "api" && segs[0] != "api-browser" {
		if len(segs) == 4 && segs[3] == "moderation" && segs[0] != "" && segs[1] != "" {
			h.repoDoc(w, r, segs[0], normPrincipal(segs[1]))
			return true
		}
		if h.handleRepoReports(w, r, segs, segs[0], normPrincipal(segs[1])) {
			return true
		}
		return false
	}
	if len(segs) < 4 || (segs[0] != "api" && segs[0] != "api-browser") || segs[1] != "v1" {
		return false
	}
	if h.handleReports(w, r, segs) {
		return true
	}
	if segs[2] != "users" {
		return false
	}
	principal := normPrincipal(segs[3])
	if principal == "" {
		return false
	}
	rest := segs[4:]
	if len(rest) == 0 || rest[0] != "blocks" {
		return false
	}
	rest = rest[1:]
	switch len(rest) {
	case 0:
		h.getList(w, r, principal)
		return true
	case 1:
		h.mutate(w, r, principal, normPrincipal(rest[0]))
		return true
	}
	return false
}

// ServeHTTP answers block routes and 404s otherwise.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// getList serves GET /api/v1/users/{principal}/blocks.
func (h *Handler) getList(w http.ResponseWriter, r *http.Request, principal string) {
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
	if !canView(p, principal) {
		writePlain(w, http.StatusForbidden, "not your block list")
		return
	}
	if err := h.requireKnownPrincipal(r, principal); err != nil {
		writeErr(w, err)
		return
	}
	doc, err := h.Svc.Blocks(r.Context(), principal)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Version int      `json:"version"`
		Blocked []string `json:"blocked"`
	}{Version: blockVersionOf(doc), Blocked: doc.Blocked})
}

// mutate serves PUT and DELETE on one subject.
func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, principal, subject string) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
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
	if !canView(p, principal) {
		writePlain(w, http.StatusForbidden, "not your block list")
		return
	}
	if err := h.requireKnownPrincipal(r, principal); err != nil {
		writeErr(w, err)
		return
	}
	var (
		version int
		changed bool
		err     error
	)
	if r.Method == http.MethodPut {
		version, changed, err = h.Svc.Block(r.Context(), principal, subject)
	} else {
		version, changed, err = h.Svc.Unblock(r.Context(), principal, subject)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if !changed {
		if r.Method == http.MethodPut {
			writePlain(w, http.StatusConflict, "already blocked")
		} else {
			writePlain(w, http.StatusNotFound, "not blocked")
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Version int `json:"version"`
	}{Version: version})
}

// --- repo moderation document (M2) -------------------------------------------

// repoDoc serves GET and PUT /{owner}/{repo}/api[-browser]/moderation. GET is
// read-gated (the page gate is the same identity hook); PUT is the one-writer
// verb whose per-field floors live inside Apply.
func (h *Handler) repoDoc(w http.ResponseWriter, r *http.Request, owner, repo string) {
	switch r.Method {
	case http.MethodGet:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return
		}
		doc, err := h.Svc.DocFor(r.Context(), owner, repo, p)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	case http.MethodPut:
		// Browser-lane CSRF: a present Origin/Referer must name this host;
		// absent means a non-browser client (same rule as the page forms,
		// where absent is refused — an API client has no ambient cookies).
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
		var in Doc
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writePlain(w, http.StatusBadRequest, "unreadable body")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if derr := dec.Decode(&in); derr != nil {
			writePlain(w, http.StatusBadRequest, "invalid JSON: "+derr.Error())
			return
		}
		version, err := h.Svc.Apply(r.Context(), owner, repo, p, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Version int `json:"version"`
		}{Version: version})
	default:
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// apiSameOrigin accepts a request with no Origin/Referer (a token client) and
// refuses one that names a different host.
func apiSameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

// requireKnownPrincipal is the profile-existence probe: 404 for an unknown
// principal (the same answer GET /api/v1/users/{principal} gives).
func (h *Handler) requireKnownPrincipal(r *http.Request, principal string) error {
	if h.Svc.Namespaces == nil {
		return nil
	}
	exists, err := h.Svc.Namespaces.UserExists(r.Context(), principal)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: unknown principal", ErrNotFound)
	}
	return nil
}

// canView is the one self-or-host-admin rule both API lanes use. Callers
// reject anonymous principals first (401), so this is never asked about one.
func canView(p auth.Principal, principal string) bool {
	return p.Admin || normPrincipal(p.Name) == normPrincipal(principal)
}

func blockVersionOf(doc BlocksDoc) int {
	if doc.Version == 0 {
		return blocksVersion
	}
	return doc.Version
}

// --- writers (the 07 §2 conventions, local to this package) -----------------

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "moderation: encode")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
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

// statusFor maps a sentinel to its HTTP status.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}

// splitPath splits and decodes each path segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		d, err := url.PathUnescape(s)
		if err != nil {
			d = s
		}
		out = append(out, d)
	}
	return out
}
