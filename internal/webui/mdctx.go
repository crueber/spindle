package webui

import "strings"

// MarkdownCtx carries the repository coordinates a markdown document is
// rendered at, so relative links and images resolve like GitHub's: against the
// file's own directory and ref, not against the page's URL.
//
// Ref is the short ref name the page addresses ("main", "v1.2.3", a full SHA);
// Dir is the document's directory inside the repo ("" at the root). An empty
// Owner, Repo or Ref disables resolution — the renderer then passes relative
// URLs through untouched, which is what issue/PR previews without a repo
// coordinate (and the ctx-less RenderMarkdown) rely on.
type MarkdownCtx struct {
	Owner string
	Repo  string
	Ref   string
	Dir   string
}

// enabled reports whether the context can address a repo ref.
func (c *MarkdownCtx) enabled() bool {
	return c != nil && c.Owner != "" && c.Repo != "" && c.Ref != ""
}

// resolveURL maps an already-validated (see sanitizeURL) relative URL to the
// URL that serves it from this repo/ref. Absolute http(s)/mailto URLs,
// protocol-relative URLs and fragments are returned unchanged.
//
// Rules (GitHub parity):
//   - a leading "/" is repo-root-relative, not site-absolute;
//   - anything else resolves against Dir;
//   - "." and ".." normalize, with ".." clamped at the repo root;
//   - percent escapes are never decoded or re-encoded (%2e stays %2e);
//   - images and non-markdown links go to the raw-byte endpoint at the same
//     ref; *.md/*.markdown links go to the in-app blob page;
//   - the original ?query and #fragment are preserved.
func (c *MarkdownCtx) resolveURL(raw string, image bool) string {
	if !c.enabled() || raw == "" {
		return raw
	}
	// Fragment-only, protocol-relative and absolute-scheme targets are not
	// repo paths; sanitizeURL has already vouched for their safety.
	if raw[0] == '#' || strings.HasPrefix(raw, "//") || schemeRe.MatchString(raw) {
		return raw
	}
	path, query, frag := splitURLSuffix(raw)
	if path == "" {
		return raw // a query-only or fragment-only reference stays as authored
	}
	ref := encodeRepoPath(shortRefName(c.Ref))
	base := "/" + encodePathSegment(c.Owner) + "/" + encodePathSegment(c.Repo)
	target := normalizeRepoPath(c.Dir, path)
	if target == "" {
		// "."/".." at the root: a link lands on the tree, an image on the
		// raw root (there is no file to serve).
		if image {
			return base + "/api/blob/" + ref + "?raw=1" + addQuery(query) + frag
		}
		return base + "/tree/" + ref + query + frag
	}
	encoded := encodeRepoPath(target)
	if image || !isMarkdownName(target) {
		return base + "/api/blob/" + ref + "/" + encoded + "?raw=1" + addQuery(query) + frag
	}
	return base + "/blob/" + ref + "/" + encoded + query + frag
}

// splitURLSuffix separates a path from its query and fragment, preserving the
// leading "?"/"#" so the caller can re-attach them verbatim. A "#" always
// starts the fragment, even when a "?" precedes it.
func splitURLSuffix(u string) (path, query, frag string) {
	path = u
	if i := strings.IndexByte(path, '#'); i >= 0 {
		frag = path[i:]
		path = path[:i]
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		query = path[i:]
		path = path[:i]
	}
	return path, query, frag
}

// addQuery appends a source query to the raw endpoint's own ?raw=1, keeping
// the authored key rather than replacing it.
func addQuery(query string) string {
	if len(query) < 2 {
		return ""
	}
	return "&" + query[1:]
}

// normalizeRepoPath joins rel onto base (the document's directory) and
// resolves "." / ".." segments, clamped at the repo root. A leading "/" in rel
// makes it root-relative and discards base. Escapes are not decoded, so
// "%2e%2e" is an ordinary segment, never a parent hop.
func normalizeRepoPath(base, rel string) string {
	var segs []string
	if strings.HasPrefix(rel, "/") {
		rel = rel[1:]
	} else if base != "" {
		segs = strings.Split(base, "/")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	return strings.Join(segs, "/")
}

// encodeRepoPath percent-encodes each path segment, preserving "/" separators
// and any existing %XX escapes (which the source may legitimately carry, e.g.
// "my%20file.png").
func encodeRepoPath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = encodePathSegment(s)
	}
	return strings.Join(segs, "/")
}

// encodePathSegment escapes the characters a URL path segment cannot carry
// literally while leaving RFC 3986 pchar characters (and existing escapes)
// alone. This is deliberately not url.PathEscape: that would re-encode "%" and
// turn "a%20b.png" into "a%2520b.png", breaking authored escapes.
func encodePathSegment(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]) {
			b.WriteString(s[i : i+3])
			i += 2
			continue
		}
		if isPathSegmentByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// isPathSegmentByte is the RFC 3986 pchar set minus "%": unreserved,
// sub-delims, ":" and "@".
func isPathSegmentByte(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@", c) >= 0
}
