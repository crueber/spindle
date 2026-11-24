package webedit

import (
	"path"
	"strings"
)

// gitattributesLFS is the documented minimal subset of .gitattributes F.4
// asks for: the FIRST whitespace-separated token is the pattern; a line marks
// LFS when one of the remaining tokens is exactly `filter=lfs`. Negated
// patterns (`!`), quoted patterns, macros and directory-scoped attribute
// files below the root are ignored (documented subset — the honest guard, not
// a full gitattributes engine). Supported patterns: an exact path, `dir/**`
// (that directory and everything below), `*` and `**` (everything).
func gitattributesLFS(body []byte, p string) bool {
	p = strings.TrimPrefix(p, "/")
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pattern := fields[0]
		if strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "\"") {
			continue
		}
		lfs := false
		for _, attr := range fields[1:] {
			if attr == "filter=lfs" {
				lfs = true
				break
			}
		}
		if !lfs {
			continue
		}
		pattern = strings.TrimPrefix(pattern, "/")
		switch {
		case pattern == "*" || pattern == "**":
			return true
		case strings.HasSuffix(pattern, "/**"):
			prefix := strings.TrimSuffix(pattern, "/**")
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
		case pattern == p:
			return true
		case !strings.Contains(pattern, "/"):
			// A basename glob (`*.png`) — the common LFS spelling.
			base := p
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			if ok, _ := path.Match(pattern, base); ok {
				return true
			}
			// A plain directory entry marks that directory's contents.
			if !strings.ContainsAny(pattern, "*?[") && strings.HasPrefix(p, pattern+"/") {
				return true
			}
		}
	}
	return false
}

// matchGitattributes is a tiny helper for tests and the service: it returns
// the parsed verdict for p, and whether the pattern parsing understood the
// file at all. path.Clean keeps a stray "./x" honest.
func matchGitattributes(body []byte, p string) bool {
	return gitattributesLFS(body, path.Clean("/" + p)[1:])
}
