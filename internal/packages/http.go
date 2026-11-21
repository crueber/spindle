package packages

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
)

// Handler is the OCI Distribution surface (docs/features/14 §3). It mounts as
// a top-level ExtraRoutes seam (server.ChainTop): Handle answers /v2/* and
// reports false for everything else, so the core router keeps ownership of
// every other path (law 8 — this package never imports internal/server).
type Handler struct {
	Svc *Service
	// Auth resolves the request principal through Seam 2 (the server's
	// AuthenticateForwarded, injected by composition). Nil → anonymous.
	Auth Authenticator
	// AuthRequired reports that the instance is not in auth mode `none`, so
	// GET /v2/ answers the Docker challenge instead of 200 (the client needs
	// it to start the token flow). Composition sets it.
	AuthRequired bool
}

// ServeHTTP answers registry routes and 404s otherwise (the httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
	}
}

// Handle answers one /v2 request; false when the path is not ours.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) == 0 || segs[0] != "v2" {
		return false
	}
	h.serveV2(w, r, segs)
	return true
}

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
}

// --- dispatch (§3 route table) ------------------------------------------------

func (h *Handler) serveV2(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 1 || (len(segs) == 2 && segs[1] == ""):
		h.ping(w, r)
		return
	case len(segs) == 2 && (segs[1] == "token" || segs[1] == "auth"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.token(w, r)
		return
	case len(segs) < 5:
		writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
		return
	}

	owner, repo, kind := segs[1], segs[2], segs[3]
	rest := segs[4:]
	switch kind {
	case "manifests":
		if len(rest) != 1 || rest[0] == "" {
			writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
			return
		}
		ref := rest[0]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.getManifest(w, r, owner, repo, ref)
		case http.MethodPut:
			h.putManifest(w, r, owner, repo, ref)
		case http.MethodDelete:
			h.deleteManifest(w, r, owner, repo, ref)
		default:
			methodNotAllowed(w, "GET", "HEAD", "PUT", "DELETE")
		}
	case "blobs":
		h.blobs(w, r, owner, repo, rest)
	case "tags":
		if len(rest) != 1 || rest[0] != "list" {
			writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.listTags(w, r, owner, repo)
	default:
		writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
	}
}

func (h *Handler) blobs(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) {
	if rest[0] == "uploads" {
		switch {
		case len(rest) == 1 || (len(rest) == 2 && rest[1] == ""):
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.startUpload(w, r, owner, repo)
		case len(rest) == 2:
			id := rest[1]
			switch r.Method {
			case http.MethodPatch:
				h.patchUpload(w, r, owner, repo, id)
			case http.MethodPut:
				h.finalizeUpload(w, r, owner, repo, id)
			case http.MethodDelete:
				h.abortUpload(w, r, owner, repo, id)
			case http.MethodGet:
				h.uploadStatus(w, r, owner, repo, id)
			default:
				methodNotAllowed(w, "GET", "PATCH", "PUT", "DELETE")
			}
		default:
			writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
		}
		return
	}
	if len(rest) != 1 {
		writeOCI(w, http.StatusNotFound, ociErr(404, CodeNameUnknown, "not found"))
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getBlob(w, r, owner, repo, rest[0])
	default:
		methodNotAllowed(w, "GET", "HEAD")
	}
}

// --- auth plumbing ------------------------------------------------------------

// authenticate resolves the principal: a registry bearer token verifies
// locally first (no store read), then the injected Seam 2 chain.
func (h *Handler) authenticate(r *http.Request) (auth.Principal, *tokenClaims, *auth.AuthError) {
	cred := r.Header.Get("Authorization")
	if cred == "" {
		cred = r.Header.Get("X-Walgit-Authorization")
	}
	if len(cred) >= 7 && strings.EqualFold(cred[:7], "bearer ") {
		tok := strings.TrimSpace(cred[7:])
		if strings.HasPrefix(tok, tokenPrefix) {
			claims, ok := h.Svc.VerifyToken(tok)
			if !ok {
				return auth.Principal{}, nil, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid registry token"}
			}
			return auth.Principal{Name: claims.Sub}, claims, nil
		}
	}
	if h.Auth != nil {
		p, aerr := h.Auth(r)
		return p, nil, aerr
	}
	return auth.Anonymous(), nil, nil
}

// authorize enforces the repo gate for one action ("pull"|"push").
// It writes the response and returns false on failure.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, owner, repo, action string) (auth.Principal, bool) {
	scope := pullScope(owner, repo)
	if action == "push" {
		scope = pushScope(owner, repo)
	}
	p, claims, aerr := h.authenticate(r)
	if aerr != nil {
		h.challenge(w, r, scope)
		writeOCI(w, http.StatusUnauthorized, errUnauthorized())
		return auth.Principal{}, false
	}
	if err := validateRepo(owner, repo); err != nil {
		writeOCI(w, err.Status, err)
		return auth.Principal{}, false
	}
	if !h.Svc.FeaturesOn(r.Context(), owner, repo) {
		// Feature off ⇒ every registry route 404s (fail closed, §7).
		writeOCI(w, http.StatusNotFound, errNameUnknown(owner+"/"+repo))
		return auth.Principal{}, false
	}
	exists, eerr := h.Svc.RepoExists(r.Context(), owner, repo)
	if eerr != nil {
		writeOCI(w, http.StatusInternalServerError, errInternal(eerr))
		return auth.Principal{}, false
	}
	if !exists {
		writeOCI(w, http.StatusNotFound, errNameUnknown(owner+"/"+repo))
		return auth.Principal{}, false
	}
	if claims != nil && !claims.scopeGrants(owner+"/"+repo, action) {
		// A short-lived token can never exceed its granted scopes.
		writeOCI(w, http.StatusForbidden, errDenied("token scope does not grant "+action))
		return auth.Principal{}, false
	}
	var oerr *Error
	if action == "push" {
		oerr = h.Svc.CanPush(r.Context(), owner, repo, p)
	} else {
		oerr = h.Svc.CanPull(r.Context(), owner, repo, p)
	}
	if oerr != nil {
		if oerr.Status == http.StatusUnauthorized {
			h.challenge(w, r, scope)
		}
		writeOCI(w, oerr.Status, oerr)
		return auth.Principal{}, false
	}
	return p, true
}

// --- ping and token -----------------------------------------------------------

func (h *Handler) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET", "HEAD")
		return
	}
	p, claims, aerr := h.authenticate(r)
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if aerr != nil {
		h.challenge(w, r)
		writeOCI(w, http.StatusUnauthorized, errUnauthorized())
		return
	}
	if h.AuthRequired && p.Anonymous && claims == nil {
		h.challenge(w, r)
		writeOCI(w, http.StatusUnauthorized, errUnauthorized())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "2")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("{}"))
	}
}

// token is the Docker token endpoint (GET /v2/token, /v2/auth alias). It
// authenticates the Basic credential through the server chain, then grants
// only the requested actions the principal actually holds. Anonymous callers
// receive pull scope for publicly readable repos (the §4 rule) and nothing
// else.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	p, claims, aerr := h.authenticate(r)
	if aerr != nil || claims != nil {
		// A registry bearer token is a result, never a credential for
		// minting another: Docker sends Basic here. Refusing this keeps a
		// pull-scoped token from asking for push.
		h.challenge(w, r)
		writeOCI(w, http.StatusUnauthorized, errUnauthorized())
		return
	}
	requested := r.URL.Query()["scope"]
	granted := make([]scope, 0, len(requested))
	grantedOut := make([]string, 0, len(requested))
	deniedAuth := false
	for _, raw := range requested {
		sc, ok := parseScope(raw)
		if !ok {
			continue
		}
		owner, repo, ok := strings.Cut(sc.Name, "/")
		if !ok || validateRepo(owner, repo) != nil {
			continue
		}
		out := scope{Kind: "repository", Name: sc.Name, acted: map[string]bool{}}
		if sc.acted["pull"] {
			if h.Svc.CanPull(r.Context(), owner, repo, p) == nil {
				out.acted["pull"] = true
			} else if p.Anonymous {
				deniedAuth = true
			}
		}
		if sc.acted["push"] && h.Svc.CanPush(r.Context(), owner, repo, p) == nil {
			out.acted["push"] = true
			out.acted["pull"] = true // push implies pull in the Docker model
		}
		if len(out.acted) > 0 {
			granted = append(granted, out)
			grantedOut = append(grantedOut, out.String())
		}
	}
	if p.Anonymous && deniedAuth && len(granted) == 0 {
		h.challenge(w, r, requested...)
		writeOCI(w, http.StatusUnauthorized, errUnauthorized())
		return
	}
	tok, exp := h.Svc.MintToken(p, grantedOut)
	access := make([]map[string]any, 0, len(granted))
	for _, sc := range granted {
		acts := make([]string, 0, 2)
		for _, a := range []string{"pull", "push"} {
			if sc.acted[a] {
				acts = append(acts, a)
			}
		}
		access = append(access, map[string]any{"type": "repository", "name": sc.Name, "actions": acts})
	}
	body := map[string]any{
		"token":        tok,
		"access_token": tok,
		"expires_in":   int64(exp.Sub(h.Svc.now()).Seconds()),
		"issued_at":    h.Svc.now().Format("2006-01-02T15:04:05Z"),
		"access":       access,
	}
	writeJSON(w, http.StatusOK, body)
}

// --- manifests ----------------------------------------------------------------

func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request, owner, repo, ref string) {
	if _, ok := h.authorize(w, r, owner, repo, "pull"); !ok {
		return
	}
	m, oerr := h.Svc.GetManifest(r.Context(), owner, repo, ref)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", m.MediaType)
	hdr.Set("Docker-Content-Digest", m.Digest)
	hdr.Set("Content-Length", strconv.Itoa(len(m.Bytes)))
	hdr.Set("ETag", `"`+m.Digest+`"`)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(m.Bytes)
}

func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, owner, repo, ref string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.Svc.maxManifest()+1))
	if err != nil {
		writeOCI(w, http.StatusRequestEntityTooLarge, errSizeInvalid("manifest exceeds packages.max_manifest_bytes"))
		return
	}
	if int64(len(body)) > h.Svc.maxManifest() {
		writeOCI(w, http.StatusRequestEntityTooLarge, errSizeInvalid("manifest exceeds packages.max_manifest_bytes"))
		return
	}
	digest, oerr := h.Svc.PutManifest(r.Context(), owner, repo, ref, body, r.Header.Get("Content-Type"))
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	hdr := w.Header()
	hdr.Set("Location", "/v2/"+owner+"/"+repo+"/manifests/"+digest)
	hdr.Set("Docker-Content-Digest", digest)
	hdr.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) deleteManifest(w http.ResponseWriter, r *http.Request, owner, repo, ref string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	if oerr := h.Svc.DeleteManifest(r.Context(), owner, repo, ref); oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// --- blobs --------------------------------------------------------------------

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, owner, repo, digest string) {
	if _, ok := h.authorize(w, r, owner, repo, "pull"); !ok {
		return
	}
	if r.Method == http.MethodHead {
		meta, oerr := h.Svc.HeadBlob(r.Context(), owner, repo, digest)
		if oerr != nil {
			writeOCI(w, oerr.Status, oerr)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Type", "application/octet-stream")
		hdr.Set("Docker-Content-Digest", meta.Digest)
		hdr.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		hdr.Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}
	rng := parseRange(r.Header.Get("Range"))
	obj, oerr := h.Svc.GetBlob(r.Context(), owner, repo, digest, rng)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	defer obj.Body.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Docker-Content-Digest", digest)
	hdr.Set("Accept-Ranges", "bytes")
	if rng != nil {
		start, end := rng[0], rng[1]
		if end > obj.Meta.Size {
			end = obj.Meta.Size
		}
		hdr.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10)+"/"+strconv.FormatInt(obj.Meta.Size, 10))
		hdr.Set("Content-Length", strconv.FormatInt(end-start, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		hdr.Set("Content-Length", strconv.FormatInt(obj.Meta.Size, 10))
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, obj.Body)
}

// parseRange parses a single "bytes=start-end" range (inclusive end, open
// ends supported). A multi-range or malformed header returns nil (full body).
func parseRange(h string) *[2]int64 {
	if h == "" || !strings.HasPrefix(h, "bytes=") {
		return nil
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if strings.Contains(spec, ",") {
		return nil
	}
	startS, endS, ok := strings.Cut(spec, "-")
	if !ok || startS == "" {
		return nil // suffix ranges unsupported → full body
	}
	start, err := strconv.ParseInt(startS, 10, 64)
	if err != nil || start < 0 {
		return nil
	}
	end := int64(-1)
	if endS != "" {
		end, err = strconv.ParseInt(endS, 10, 64)
		if err != nil || end < start {
			return nil
		}
		if end < 1<<63-1 {
			end++ // exclusive
		}
	}
	if end < 0 {
		end = 1<<63 - 1
	}
	return &[2]int64{start, end}
}

func (h *Handler) startUpload(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	id, oerr := h.Svc.StartUpload(r.Context(), owner, repo)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	h.uploadHeaders(w, owner, repo, id, 0)
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) uploadHeaders(w http.ResponseWriter, owner, repo, id string, offset int64) {
	hdr := w.Header()
	hdr.Set("Location", "/v2/"+owner+"/"+repo+"/blobs/uploads/"+id)
	hdr.Set("Docker-Upload-UUID", id)
	if offset > 0 {
		hdr.Set("Range", "0-"+strconv.FormatInt(offset-1, 10))
	} else {
		hdr.Set("Range", "0-0")
	}
	hdr.Set("Content-Length", "0")
}

func (h *Handler) patchUpload(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	// A Content-Range on PATCH names the chunk start; reject a gap so a
	// resumed upload never silently stutters.
	if cr := r.Header.Get("Content-Range"); cr != "" {
		if start, ok := parseChunkStart(cr); ok {
			offset, oerr := h.Svc.UploadOffset(r.Context(), owner, repo, id)
			if oerr != nil {
				writeOCI(w, oerr.Status, oerr)
				return
			}
			if start != offset {
				h.uploadHeaders(w, owner, repo, id, offset)
				writeOCI(w, http.StatusRequestedRangeNotSatisfiable, ociErr(416, CodeSizeInvalid, "chunk start does not match the uploaded offset"))
				return
			}
		}
	}
	offset, oerr := h.Svc.AppendUpload(r.Context(), owner, repo, id, r.Body)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	h.uploadHeaders(w, owner, repo, id, offset)
	w.WriteHeader(http.StatusAccepted)
}

// parseChunkStart reads the start offset from "start-end" or "bytes start-end".
func parseChunkStart(cr string) (int64, bool) {
	spec := strings.TrimSpace(strings.TrimPrefix(cr, "bytes"))
	spec = strings.TrimSpace(spec)
	startS, _, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startS), 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

func (h *Handler) finalizeUpload(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		writeOCI(w, http.StatusBadRequest, errDigestInvalid("digest query parameter is required"))
		return
	}
	// The final PUT may carry the last chunk (the spec's monolithic finish).
	if r.ContentLength != 0 {
		if _, oerr := h.Svc.AppendUpload(r.Context(), owner, repo, id, r.Body); oerr != nil {
			writeOCI(w, oerr.Status, oerr)
			return
		}
	}
	if _, oerr := h.Svc.FinalizeUpload(r.Context(), owner, repo, id, digest); oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	hdr := w.Header()
	hdr.Set("Location", "/v2/"+owner+"/"+repo+"/blobs/"+digest)
	hdr.Set("Docker-Content-Digest", digest)
	hdr.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) abortUpload(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	if oerr := h.Svc.AbortUpload(r.Context(), owner, repo, id); oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	if _, ok := h.authorize(w, r, owner, repo, "push"); !ok {
		return
	}
	offset, oerr := h.Svc.UploadOffset(r.Context(), owner, repo, id)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	h.uploadHeaders(w, owner, repo, id, offset)
	w.WriteHeader(http.StatusNoContent)
}

// --- tags list ----------------------------------------------------------------

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if _, ok := h.authorize(w, r, owner, repo, "pull"); !ok {
		return
	}
	q := r.URL.Query()
	n := 100
	if v := q.Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			writeOCI(w, http.StatusBadRequest, ociErr(400, CodeUnknown, "invalid n: must be a positive integer"))
			return
		}
		n = parsed
	}
	tags, more, oerr := h.Svc.ListTags(r.Context(), owner, repo, q.Get("last"), n)
	if oerr != nil {
		writeOCI(w, oerr.Status, oerr)
		return
	}
	if more {
		last := tags[len(tags)-1]
		next := "/v2/" + owner + "/" + repo + "/tags/list?n=" + strconv.Itoa(n) + "&last=" + url.QueryEscape(last)
		w.Header().Set("Link", "<"+next+`>; rel="next"`)
	}
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": owner + "/" + repo, "tags": tags})
}

// --- writers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeOCI(w, http.StatusInternalServerError, errInternal(errors.New("encode: "+err.Error())))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeOCI(w http.ResponseWriter, status int, e *Error) {
	if e == nil {
		e = errInternal(errors.New("unknown error"))
	}
	b, err := json.Marshal(bodyOf(e))
	if err != nil {
		b = []byte(`{"errors":[{"code":"UNKNOWN","message":"error"}]}`)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writeOCI(w, http.StatusMethodNotAllowed, ociErr(405, CodeUnknown, "method not allowed"))
}
