package staticpages

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// siteCtx is the resolved serving state of one published repository.
type siteCtx struct {
	repo   string // owner/repo, for the one infrastructure log line
	dir    string // serving copy (bare repo path)
	commit string // resolved commit oid
	root   string // validated site root, "" = repository root
}

// ServeHTTP is the whole static listener: one Host, GET/HEAD only, public
// repositories, committed bytes (docs/features/19_static_sites.md §2, §3, §7).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if !h.hostOK(r.Host) {
		// Any other Host is refused with a bodiless 404 for every method (§2):
		// the listener must not reveal even which methods it answers under a
		// hostname it does not serve.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlain(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tgt, err := parseTarget(r.URL.EscapedPath())
	if err != nil {
		// A rejected path never reaches the store or a subprocess (§3).
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	h.serve(w, r, tgt)
}

// hostOK compares the request Host to the configured one, ignoring case and
// port. An empty configured host refuses everything.
func (h *Handler) hostOK(reqHost string) bool {
	if h.host == "" {
		return false
	}
	return normalizeHost(reqHost) == h.host
}

// serve runs the request flow: visibility, settings, dir, ref, then resolution.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, tgt target) {
	ctx := r.Context()
	repo := tgt.owner + "/" + tgt.repo
	if h.visible == nil || h.settings == nil {
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	ok, err := h.visible.CanRead(ctx, tgt.owner, tgt.repo)
	if err != nil || !ok {
		// Private/unknown repositories are indistinguishable from a missing
		// site, and no git work happens for them.
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	site, err := h.settings.Site(ctx, tgt.owner, tgt.repo)
	if err != nil || !site.Enabled {
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	root, rootOK := cleanRoot(site.Root)
	if !rootOK || !validRef(site.Ref) {
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	if tgt.rest == "" && !tgt.trailing {
		// /{o}/{r} always names the site directory. The redirect needs no
		// repository content — not the synced serving copy, not a resolved ref —
		// so it is answered before the Dir call and any subprocess (§3).
		h.redirect(w, r, tgt, false)
		return
	}
	if h.dirs == nil || h.git == nil {
		// A composition bug, not a request failure: nothing to log beyond the
		// status (there is no error value to name).
		writePlain(w, r, http.StatusServiceUnavailable, "static sites are not wired")
		return
	}
	dir, err := h.dirs.Dir(ctx, repo)
	if err != nil {
		h.unavailable(w, r, repo, "dir", "", err, "static site unavailable")
		return
	}
	commit, err := h.git.Resolve(ctx, dir, site.Ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			writePlain(w, r, http.StatusNotFound, "not found")
			return
		}
		h.unavailable(w, r, repo, "resolve", "", err, "static site content unavailable")
		return
	}
	sc := siteCtx{repo: repo, dir: dir, commit: commit, root: root}

	if tgt.trailing {
		h.serveIndex(w, r, sc, tgt)
		return
	}
	h.serveFile(w, r, sc, tgt)
}

// serveIndex resolves a directory request ("…/x/" or the site root) through the
// configured index names. A directory without an index is a 404 — never a
// listing (§3).
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, sc siteCtx, tgt target) {
	base := joinPath(sc.root, tgt.rest)
	for _, name := range h.indexFiles {
		candidate := joinPath(base, name)
		entry, found, err := h.git.Lookup(r.Context(), sc.dir, sc.commit, candidate)
		if err != nil {
			h.gitFail(w, r, sc, candidate, err)
			return
		}
		if !found || !h.servable(entry) {
			continue
		}
		h.deliver(w, r, sc, candidate, entry)
		return
	}
	h.notFound(w, r, sc)
}

// serveFile resolves a path that does not end in "/": a blob is served, a tree
// redirects to its trailing-slash form, a miss falls back to "<path>.html" (§3).
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, sc siteCtx, tgt target) {
	name := joinPath(sc.root, tgt.rest)
	entry, found, err := h.git.Lookup(r.Context(), sc.dir, sc.commit, name)
	if err != nil {
		h.gitFail(w, r, sc, name, err)
		return
	}
	if found {
		switch {
		case entry.Type == "tree":
			h.redirect(w, r, tgt, true)
		case h.servable(entry):
			h.deliver(w, r, sc, name, entry)
		default:
			// Symlinks, gitlinks and oversized files are not served (§3, §5).
			h.notFound(w, r, sc)
		}
		return
	}
	fallback := name + ".html"
	entry, found, err = h.git.Lookup(r.Context(), sc.dir, sc.commit, fallback)
	if err != nil {
		h.gitFail(w, r, sc, fallback, err)
		return
	}
	if found && h.servable(entry) {
		h.deliver(w, r, sc, fallback, entry)
		return
	}
	h.notFound(w, r, sc)
}

// servable is the v1 admission rule for one entry: a regular blob within the
// per-file cap. Trees, symlinks (120000), gitlinks (160000) and oversized files
// are all 404.
func (h *Handler) servable(e Entry) bool {
	return e.regularFile() && e.Size >= 0 && e.Size <= h.maxFileBytes
}

// deliver writes a blob response, turning a size-cap surprise into the site's
// 404 and anything else into a logged 503.
func (h *Handler) deliver(w http.ResponseWriter, r *http.Request, sc siteCtx, name string, entry Entry) {
	if err := h.serveBlob(w, r, sc.dir, name, entry); err != nil {
		if errors.Is(err, ErrTooLarge) {
			h.notFound(w, r, sc)
			return
		}
		h.unavailable(w, r, sc.repo, "read", name, err, "static site content unavailable")
	}
}

// serveBlob answers a 200 (or 304/206 via ServeContent) for one blob. The ETag
// is the blob oid, strong, and the modtime is zero — v1 has no Last-Modified, so
// no subprocess is spent on commit timestamps (§4).
func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, dir, name string, entry Entry) error {
	hdr := w.Header()
	hdr.Set("Content-Type", contentType(name))
	hdr.Set("ETag", `"`+entry.OID+`"`)
	hdr.Set("Cache-Control", CacheControl)

	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, entry.OID) {
		w.WriteHeader(http.StatusNotModified) // revalidation costs no blob read
		return nil
	}
	body, err := h.readBlob(r.Context(), dir, entry)
	if err != nil {
		return err
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	return nil
}

// readBlob reads at most MaxFileBytes of the blob. The entry size is checked by
// the caller, so an oversized file costs no content subprocess; the capped
// buffer is the belt-and-braces guard against a runner whose size was wrong.
//
// The buffer starts nil and grows as the reader writes (§8): a request queued
// for a pool slot holds no body memory, and only the reads actually running
// (bounded by the package's own pool) allocate — a declared size is never
// trusted enough to preallocate from.
func (h *Handler) readBlob(ctx context.Context, dir string, entry Entry) ([]byte, error) {
	if entry.Size < 0 || entry.Size > h.maxFileBytes {
		return nil, ErrTooLarge
	}
	buf := &cappedBuffer{max: h.maxFileBytes}
	err := h.git.ReadBlob(ctx, dir, entry.OID, buf)
	if buf.overflowed {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, err
	}
	return buf.buf, nil
}

// cappedBuffer is an io.Writer that refuses to hold more than max bytes. It
// grows lazily (no capacity hint), so an idle buffer costs nothing.
type cappedBuffer struct {
	buf        []byte
	max        int64
	overflowed bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if int64(len(c.buf))+int64(len(p)) > c.max {
		c.overflowed = true
		return 0, ErrTooLarge
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// notFound serves the site's 404.html with status 404 when one exists, else a
// one-line plain-text 404. It never carries cache headers (§4) and there is no
// SPA rewrite (§3).
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, sc siteCtx) {
	name := joinPath(sc.root, "404.html")
	entry, found, err := h.git.Lookup(r.Context(), sc.dir, sc.commit, name)
	if err == nil && found && h.servable(entry) {
		if body, rerr := h.readBlob(r.Context(), sc.dir, entry); rerr == nil {
			hdr := w.Header()
			hdr.Del("ETag")
			hdr.Del("Cache-Control")
			hdr.Set("Content-Type", "text/html; charset=utf-8")
			hdr.Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusNotFound)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
	}
	writePlain(w, r, http.StatusNotFound, "not found")
}

// gitFail maps a lookup failure onto a status. A missing object is a 404 like
// any other miss; everything else is the listener's problem, not the visitor's,
// so it is logged once and answered with a 503.
func (h *Handler) gitFail(w http.ResponseWriter, r *http.Request, sc siteCtx, path string, err error) {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
		writePlain(w, r, http.StatusNotFound, "not found")
		return
	}
	h.unavailable(w, r, sc.repo, "lookup", path, err, "static site content unavailable")
}

// unavailable is the only 503 writer, and the only logger call in this package:
// one record per failed request naming the repository, the operation and the
// error, so the argv+stderr a git *failure* carries reaches the server log
// instead of vanishing (§7). A 4xx is the visitor's business and is never
// logged.
func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request, repo, op, path string, err error, msg string) {
	if h.logger != nil && err != nil {
		attrs := []any{"repo", repo, "operation", op, "error", err}
		if path != "" {
			attrs = append(attrs, "path", path)
		}
		h.logger.ErrorContext(r.Context(), "static site request failed", attrs...)
	}
	writePlain(w, r, http.StatusServiceUnavailable, msg)
}

// redirect issues the 301 for a directory named without its trailing slash,
// preserving the query (§3). It carries no cache headers by design.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, tgt target, addSlash bool) {
	p := "/" + tgt.owner + "/" + tgt.repo + "/"
	if tgt.rest != "" {
		p += tgt.rest
		if addSlash {
			p += "/"
		}
	}
	loc := (&url.URL{Path: p, RawQuery: r.URL.RawQuery}).String()
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusMovedPermanently)
}

// writePlain answers with a small text/plain body (no body on HEAD) and clears
// any content headers a failed attempt may have left behind.
func writePlain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	hdr := w.Header()
	hdr.Del("ETag")
	hdr.Del("Cache-Control")
	hdr.Del("Content-Range")
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, msg)
}

// etagMatches compares one If-None-Match header against the blob oid. Weak
// prefixes and list members are accepted; "*" matches anything.
func etagMatches(header, oid string) bool {
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" {
			return true
		}
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p == oid {
			return true
		}
	}
	return false
}
