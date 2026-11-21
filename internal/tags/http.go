package tags

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

// Wire conventions (same as internal/releases and internal/api): JSON
// success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, no-store on mutations, both lanes everywhere.
// Anonymous-denied writes get a real 401 with WWW-Authenticate: Bearer
// (never a 200 with an in-band error).

// ExposedTemplates lists the discovery endpoints[] entries this surface
// serves (14 §14.12 lane rule) — registered from composition via
// api.RegisterExposed in the same change (law 12; Forgejo #272). Pinned
// by TestExposedTemplatesExact + TestExposedCoversRoutes (no phantoms,
// nothing missing).
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/tags",
}

// Handler is the Seam 1 surface: POST /{o}/{r}/api/tags (and the
// /api-browser twin). Composition chains it in front of the core api mux:
// Handle reports false for non-tags paths so the core mux answers.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Nil falls back to
// anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// principal resolves the request principal via the injected Authenticator.
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a tags route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if segs[0] == "api" || segs[0] == "api-browser" {
		return false // no top-level tags routes; all are repo-scoped
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		if len(segs[3:]) == 1 && segs[3] == "tags" {
			h.handleTags(w, r, owner, repo)
			return true
		}
		// WP4.2: ref-scoped tag deletion. Only DELETE is claimed here; the
		// core api owns GET …/api/refs/{branches|tags} and keeps serving it.
		if len(segs[3:]) == 3 && segs[3] == "refs" && segs[4] == "tags" && r.Method == http.MethodDelete {
			h.deleteTag(w, r, owner, repo, segs[5])
			return true
		}
	}
	return false
}

// ServeHTTP answers tags routes and 404s otherwise (httptest surface).
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
// verbatim (fail closed downstream: it won't match a tag shape).
func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
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
		writePlain(w, http.StatusInternalServerError, "tags: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeStrict unmarshals body into v after rejecting unknown top-level
// keys (fail closed: unknown keys on write are 400).
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

func (h *Handler) handleTags(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.createTag(w, r, owner, repo, p)
	case http.MethodGet:
		h.listTags(w, r, owner, repo, p)
	default:
		methodNotAllowed(w, "GET", "POST")
	}
}

// --- endpoints ---------------------------------------------------------------

var createTagFields = map[string]bool{"name": true, "sha": true, "message": true}

// MaxCreateBodyBytes bounds the POST …/tags body: room for a
// MaxTagMessageLen message plus JSON framing/escaping overhead.
const MaxCreateBodyBytes = 128 << 10

// tagWire is the create response (the created tag: sha is the commit for
// lightweight tags, the tag object oid for annotated ones).
type tagWire struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
}

// createTag answers POST …/tags: {name, sha, message?} → 201 tag (write;
// unknown sha 404, existing tag 409, bad name/message 400). An empty message
// creates a lightweight tag; a non-empty message mints an annotated tag
// object (#263) — never a silent downgrade.
func (h *Handler) createTag(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	var in CreateInput
	if !decodeStrict(w, r, MaxCreateBodyBytes, createTagFields, &in) {
		return
	}
	t, err := h.Svc.CreateTag(r.Context(), owner, repo, p, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tagWire{Name: t.Name, SHA: t.SHA, Ref: t.Ref})
}

// tagEntryWire is one /api/tags list row.
type tagEntryWire struct {
	Name      string `json:"name"`
	Commit    string `json:"commit"`
	Object    string `json:"object,omitempty"`
	Date      string `json:"date"`
	Author    string `json:"author"`
	Annotated bool   `json:"annotated"`
	Release   bool   `json:"release"`
}

// listTags answers GET …/api/tags → {tags: [{name, commit, date, author,
// release}]} (read; [] never null).
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	entries, err := h.Svc.ListTagEntries(r.Context(), owner, repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]tagEntryWire, 0, len(entries))
	for _, e := range entries {
		out = append(out, tagEntryWire{Name: e.Name, Commit: e.Commit, Object: e.Object,
			Date: e.Date, Author: e.Author, Annotated: e.Annotated, Release: e.Release})
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"tags": out})
}

// deleteTag answers DELETE …/api/refs/tags/{tag}: 200 {name, ref}; 404
// unknown tag; 409 release-backed or a concurrent move; 403 policy/write.
func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request, owner, repo, tag string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteTag(r.Context(), owner, repo, p, tag); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": tag, "ref": "refs/tags/" + tag})
}
