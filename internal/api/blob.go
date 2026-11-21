package api

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

// --- GET …/blob/{rest}[?raw] (§9.5: rev may contain slashes — Resolve splits) ---

type blobBody struct {
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Contents string `json:"contents,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
}

func (h *handlers) blob(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	// Greedy tail (issue #251): the rev may itself contain slashes, so the
	// handler passes the whole tail to Resolve and lets its longest-prefix
	// match split ref from path — the same treatment resolve already has.
	rest := r.PathValue("rest")
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), rest)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	path := res.Path
	if path == "" {
		writePlain(w, http.StatusNotFound, "blob requires a path")
		return
	}
	ctx := r.Context()
	class := ccSWR
	if revIsFullSHA(refPartOf(rest, path)) {
		class = ccImmutable
	}

	if _, raw := r.URL.Query()["raw"]; raw {
		// ?raw bypasses JSON: full raw bytes (§14: the 2 MiB cap
		// is a JSON-shape rule; raw is the download path). Passive media gets
		// its real type so a rendered README can embed it (nosniff is on, so
		// text/plain would refuse to display an image); everything else is
		// text/plain as before.
		br, err := h.env.Repo.Blob(ctx, RepoOf(r), res.SHA, path, true)
		if err != nil {
			mapViewErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", class)
		w.Header().Set("ETag", `"`+res.SHA+`"`)
		w.Header().Set("Content-Type", rawContentType(path))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(br.Contents)
		return
	}

	render := func() ([]byte, error) {
		br, err := h.env.Repo.Blob(ctx, RepoOf(r), res.SHA, path, false)
		if err != nil {
			return nil, err
		}
		body := blobBody{
			Ref:      res.Ref,
			SHA:      res.SHA,
			Path:     path,
			Name:     baseName(path),
			Size:     br.Size,
			Binary:   br.Binary,
			TooLarge: br.TooLarge,
		}
		if !br.Binary && !br.TooLarge {
			body.Contents = string(br.Contents)
		}
		return json.Marshal(body)
	}
	raw, err := h.env.renderImmutable(ctx, "blob/"+res.SHA+"/"+path, res.Revision, res.SHA, render)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, raw)
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// rawContentType is the raw lane's Content-Type. Only passive media gets a
// real type: text stays text/plain, and SVG is excluded on purpose (SVG can
// carry script and the raw lane is same-origin — GitHub serves raw SVG as
// text too).
func rawContentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogv":
		return "video/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	}
	return "text/plain; charset=utf-8"
}
