package webedit

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	walgit "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// Wire conventions (07_api.md / F.3): JSON success, plain-text errors,
// no-store on every mutation, both lanes, a real 401 with
// WWW-Authenticate: Bearer for anonymous writers.

// ExposedTemplates lists the discovery entries this surface serves
// (14 §14.12; registered from composition through api.RegisterExposed).
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/contents",
}

// Handler is the Seam 1 surface: PUT/POST/DELETE /{o}/{r}/api/contents and
// the /api-browser twin. Handle reports false for other paths.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// Authenticator resolves the request principal through Seam 2.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one contents request; false when the path is not ours.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) < 4 || segs[0] == "api" || segs[0] == "api-browser" {
		return false
	}
	if segs[2] != "api" && segs[2] != "api-browser" {
		return false
	}
	owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
	if _, err := walgit.ParseRepoId(owner + "/" + repo); err != nil {
		return false
	}
	if len(segs) != 4 || segs[3] != "contents" {
		return false
	}
	h.handleContents(w, r, owner, repo)
	return true
}

// ServeHTTP answers contents routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// writeRequest is the JSON body: F.3 verbatim.
type writeRequest struct {
	Message           string `json:"message"`
	Description       string `json:"description"`
	Content           string `json:"content"` // base64
	ExpectedBlobSHA   string `json:"expected_blob_sha"`
	ExpectedBranchSHA string `json:"expected_branch_sha"`
	NewBranch         string `json:"new_branch"`
	OpenPR            bool   `json:"open_pr"`
	PRTitle           string `json:"pr_title"`
	PRBody            string `json:"pr_body"`
}

type writeResponse struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Branch string `json:"branch"`
	PR     *struct {
		Num int `json:"num"`
	} `json:"pr,omitempty"`
}

func (h *Handler) handleContents(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "PUT, POST, DELETE")
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil || p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	q := r.URL.Query()
	ref := strings.TrimSpace(q.Get("ref"))
	path := strings.TrimSpace(q.Get("path"))
	if ref == "" || path == "" {
		writePlain(w, http.StatusBadRequest, "ref and path are required")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeErr(w, err)
		return
	}
	limit := h.Svc.maxBytes()*2 + 256*1024
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "request body unreadable")
		return
	}
	if int64(len(body)) > limit {
		writePlain(w, http.StatusRequestEntityTooLarge, "request body exceeds the editor cap")
		return
	}
	var req writeRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writePlain(w, http.StatusBadRequest, "request body is not valid JSON")
			return
		}
	}
	in := Input{
		Ref: ref, Path: path, Message: req.Message, Description: req.Description,
		ExpectedBlobSHA:   strings.TrimSpace(req.ExpectedBlobSHA),
		ExpectedBranchSHA: strings.TrimSpace(req.ExpectedBranchSHA),
		NewBranch:         strings.TrimSpace(req.NewBranch),
		OpenPR:            req.OpenPR, PRTitle: req.PRTitle, PRBody: req.PRBody,
	}
	switch r.Method {
	case http.MethodPost:
		in.Mode = ModeCreate
	case http.MethodDelete:
		in.Mode = ModeDelete
	default:
		if in.ExpectedBlobSHA != "" {
			in.Mode = ModeEdit
		} else {
			in.Mode = ModeCreate
		}
	}
	if r.Method != http.MethodDelete {
		content, cerr := DecodeContent(req.Content)
		if cerr != nil {
			writeErr(w, cerr)
			return
		}
		in.Content = content
	}
	res, serr := h.Svc.Save(r.Context(), owner, repo, p, in)
	if serr != nil {
		writeErr(w, serr)
		return
	}
	status := http.StatusCreated
	if r.Method == http.MethodDelete {
		status = http.StatusOK
	}
	// Location makes the SDK/CLI one-call flow: the PR when one opened,
	// else the landed file (or the branch root for a delete).
	loc := "/" + owner + "/" + repo + "/blob/" + escapeSegment(res.Branch) + "/" + escapePath(path)
	if res.PRNum > 0 {
		loc = "/" + owner + "/" + repo + "/pull/" + strconv.Itoa(res.PRNum)
	} else if r.Method == http.MethodDelete {
		loc = "/" + owner + "/" + repo + "/tree/" + escapeSegment(res.Branch)
	}
	w.Header().Set("Location", loc)
	var out writeResponse
	out.Commit.SHA = res.Commit
	out.Branch = res.Branch
	if res.PRNum > 0 {
		out.PR = &struct {
			Num int `json:"num"`
		}{Num: res.PRNum}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&out)
}

// --- wire helpers -----------------------------------------------------------

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

func escapeSegment(s string) string { return url.PathEscape(s) }

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
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

// statusFor is the F.3 error matrix.
func statusFor(err error) int {
	var ce *ConflictError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &ce):
		return http.StatusConflict
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, ErrNoChange):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, err error) {
	writePlain(w, statusFor(err), err.Error())
}

// HTTPStatus exposes the F.3 error matrix to page handlers (the same mapping
// the API answers, so a page POST and its API twin agree on the status).
func HTTPStatus(err error) int { return statusFor(err) }
