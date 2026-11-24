package threads

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Artifact routes (15 §2.5).
//
// On the authenticated job channel (attempt token, never a session cookie):
//
//	POST …/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts                     finalize
//	PUT  …/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{i}    one file
//	GET  …/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}              manifest
//	GET  …/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{i}    one file back
//
// On the repository lane (read gate):
//
//	GET  /{o}/{r}/api[-browser]/threads/{run}/artifacts                        the list
//
// The browser ZIP download is a page route (internal/webui) because it is a
// link, not an API call.

// attemptArtifacts dispatches the artifact sub-tree of one attempt.
func (h *Handler) attemptArtifacts(w http.ResponseWriter, r *http.Request, ref string, attempt int, rest []string) {
	switch {
	case len(rest) == 0:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.artifactFinalize(w, r, ref, attempt)
	case len(rest) == 1 && r.Method == http.MethodGet:
		h.artifactManifest(w, r, ref, attempt, rest[0])
	case len(rest) == 3 && rest[1] == "blobs" && r.Method == http.MethodPut:
		h.artifactBlobPut(w, r, ref, attempt, rest[0], rest[2])
	case len(rest) == 3 && rest[1] == "blobs" && r.Method == http.MethodGet:
		h.artifactBlobGet(w, r, ref, attempt, rest[0], rest[2])
	case len(rest) == 1:
		methodNotAllowed(w, "GET")
	case len(rest) == 3 && rest[1] == "blobs":
		methodNotAllowed(w, "PUT", "GET")
	default:
		writePlain(w, http.StatusNotFound, "unknown artifact endpoint")
	}
}

// artifactFinalize commits one artifact manifest.
func (h *Handler) artifactFinalize(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	var in ArtifactFinalizeInput
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{"name": true, "files": true, "retention_days": true}, &in) {
		return
	}
	view, err := h.Svc.FinalizeArtifact(r.Context(), claims, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// artifactBlobPut receives one file (the body is the bytes; ?path= and ?size=
// describe it, and the server recomputes the digest while receiving).
func (h *Handler) artifactBlobPut(w http.ResponseWriter, r *http.Request, ref string, attempt int, name, indexRaw string) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	index, ierr := strconv.Atoi(indexRaw)
	if ierr != nil {
		writePlain(w, http.StatusBadRequest, "artifact file index must be an integer")
		return
	}
	q := r.URL.Query()
	filePath, sizeRaw := q.Get("path"), q.Get("size")
	size, serr := strconv.ParseInt(sizeRaw, 10, 64)
	if serr != nil || size < 0 {
		writePlain(w, http.StatusBadRequest, "artifact file size must be a non-negative integer")
		return
	}
	_, perFile := h.Svc.artifactCaps()
	if size > perFile {
		writeErr(w, fmt.Errorf("%w: artifact %q: file %q is %s, over the per-file cap of %s (threads.max_artifact_bytes_per_file)",
			ErrTooLarge, name, filePath, humanBytes(size), humanBytes(perFile)))
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != size {
		writePlain(w, http.StatusBadRequest, "artifact body length does not match ?size=")
		return
	}
	// One byte of headroom so an over-long body fails at the reader, not in
	// the store.
	body := http.MaxBytesReader(w, r.Body, size+1)
	view, err := h.Svc.AddArtifactBlob(r.Context(), claims, name, index, filePath, size, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// artifactManifest answers the runner's download step.
func (h *Handler) artifactManifest(w http.ResponseWriter, r *http.Request, ref string, attempt int, name string) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	art, err := h.Svc.ArtifactManifestForJob(r.Context(), claims, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, art)
}

// artifactBlobGet streams one file back to the job container.
func (h *Handler) artifactBlobGet(w http.ResponseWriter, r *http.Request, ref string, attempt int, name, indexRaw string) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	index, ierr := strconv.Atoi(indexRaw)
	if ierr != nil {
		writePlain(w, http.StatusBadRequest, "artifact file index must be an integer")
		return
	}
	body, file, err := h.Svc.ArtifactBlobForJob(r.Context(), claims, name, index)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer body.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Length", strconv.FormatInt(file.Size, 10))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Spindle-Artifact-Path", file.Path)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// listArtifacts is GET …/api[-browser]/threads/{run}/artifacts.
func (h *Handler) listArtifacts(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	arts, err := h.Svc.ListArtifacts(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": arts})
}
