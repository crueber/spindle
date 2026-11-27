package threads

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Cache routes (docs/threads.md §3.4c; PAR-095).
//
// On the authenticated job channel (attempt token, never a session cookie):
//
//	POST …/api/v1/threads/jobs/{ref}/attempt/{n}/cache/lookup    restore lookup
//	GET  …/api/v1/threads/jobs/{ref}/attempt/{n}/cache/blobs/{sha}   payload back
//	PUT  …/api/v1/threads/jobs/{ref}/attempt/{n}/cache/blobs/{sha}   payload up
//	POST …/api/v1/threads/jobs/{ref}/attempt/{n}/cache/save      commit an entry
//
// On the repository lane:
//
//	GET    /{o}/{r}/api[-browser]/threads/caches        the list (read gate)
//	DELETE /{o}/{r}/api[-browser]/threads/caches/{id}   one entry (maintain)
//
// The run's SCOPES come from its header, never from a query parameter: an
// attempt token cannot ask for a different PR's or branch's cache.

// attemptCache dispatches the cache sub-tree of one attempt.
func (h *Handler) attemptCache(w http.ResponseWriter, r *http.Request, ref string, attempt int, rest []string) {
	switch {
	case len(rest) == 1 && rest[0] == "lookup":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.cacheLookup(w, r, ref, attempt)
	case len(rest) == 1 && rest[0] == "save":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.cacheSave(w, r, ref, attempt)
	case len(rest) == 2 && rest[0] == "blobs":
		switch r.Method {
		case http.MethodGet:
			h.cacheBlobGet(w, r, ref, attempt, rest[1])
		case http.MethodPut:
			h.cacheBlobPut(w, r, ref, attempt, rest[1])
		default:
			methodNotAllowed(w, "GET", "PUT")
		}
	default:
		writePlain(w, http.StatusNotFound, "unknown cache endpoint")
	}
}

// cacheLookup answers one restore request.
func (h *Handler) cacheLookup(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	var in CacheLookupInput
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{"key": true, "version": true, "restore_keys": true}, &in) {
		return
	}
	view, err := h.Svc.CacheLookupForJob(r.Context(), claims, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// cacheSave commits one entry into the run's writable scope.
func (h *Handler) cacheSave(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	var in SaveCacheInput
	if !decodeStrict(w, r, maxProtocolBody,
		map[string]bool{"key": true, "version": true, "sha256": true, "size": true, "files": true}, &in) {
		return
	}
	view, err := h.Svc.SaveCacheForJob(r.Context(), claims, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// cacheBlobPut receives one payload. ?size= is required (the store needs a
// known length) and the server recomputes the digest while receiving.
func (h *Handler) cacheBlobPut(w http.ResponseWriter, r *http.Request, ref string, attempt int, sha string) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	size, serr := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if serr != nil || size < 0 {
		writePlain(w, http.StatusBadRequest, "cache payload size must be a non-negative integer (?size=)")
		return
	}
	caps := h.Svc.CacheCaps()
	if size > caps.MaxBytesPerEntry {
		writeErr(w, fmt.Errorf("%w: the cache payload is %s, over the per-entry cap of %s (threads.cache_max_entry_bytes)",
			ErrTooLarge, humanBytes(size), humanBytes(caps.MaxBytesPerEntry)))
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != size {
		writePlain(w, http.StatusBadRequest, "cache payload length does not match ?size=")
		return
	}
	body := http.MaxBytesReader(w, r.Body, size+1)
	if err := h.Svc.PutCacheBlobForJob(r.Context(), claims, sha, size, body); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": sha, "size": size})
}

// cacheBlobGet streams one payload back to the job container.
func (h *Handler) cacheBlobGet(w http.ResponseWriter, r *http.Request, ref string, attempt int, sha string) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	body, size, err := h.Svc.CacheBlobForJob(r.Context(), claims, sha)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer body.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Spindle-Cache-SHA256", sha)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// listCaches is GET …/api[-browser]/threads/caches.
func (h *Handler) listCaches(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	entries, err := h.Svc.ListCaches(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"caches": entries, "caps": h.Svc.CacheCaps()})
}

// deleteCache is DELETE …/api[-browser]/threads/caches/{id}.
func (h *Handler) deleteCache(w http.ResponseWriter, r *http.Request, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	freed, err := h.Svc.DeleteCache(r.Context(), repo, id, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "bytes_freed": freed})
}
