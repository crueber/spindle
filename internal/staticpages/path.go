package staticpages

import (
	"net/url"
	"strings"
)

// target is a parsed, validated /{owner}/{repo}/{rest} request.
type target struct {
	owner    string
	repo     string
	rest     string // normalised, slash-joined, "" at the site root
	trailing bool   // the request path ended with "/"
}

// parseTarget splits and validates the escaped request path (§3). It performs
// no store or subprocess work: a rejected path never reaches git.
//
// Normalisation: each segment is percent-decoded, empty segments are collapsed
// (so "a//b" is "a/b"), "." segments are dropped. A segment containing "..",
// a backslash, NUL, a control byte, an encoded slash or a leading dot is
// rejected outright — there is no attempt to repair it.
func parseTarget(escapedPath string) (target, error) {
	if !strings.HasPrefix(escapedPath, "/") {
		return target{}, ErrInvalid
	}
	raw := strings.Split(escapedPath[1:], "/")
	trailing := len(raw) > 0 && raw[len(raw)-1] == ""
	segs := make([]string, 0, len(raw))
	for _, part := range raw {
		if part == "" { // collapsed "//" or the trailing empty segment
			continue
		}
		dec, err := url.PathUnescape(part)
		if err != nil {
			return target{}, ErrInvalid
		}
		if err := checkSegment(dec); err != nil {
			return target{}, err
		}
		if dec == "." {
			continue // "." is dropped, not rejected
		}
		segs = append(segs, dec)
	}
	if len(segs) < 2 {
		return target{}, ErrInvalid
	}
	owner, repo := segs[0], segs[1]
	if !validIDPart(owner) || !validIDPart(repo) {
		return target{}, ErrInvalid
	}
	return target{
		owner:    owner,
		repo:     repo,
		rest:     strings.Join(segs[2:], "/"),
		trailing: trailing,
	}, nil
}

// checkSegment rejects the bytes and shapes that must never reach git or the
// filesystem. "." is allowed here because the caller drops it.
func checkSegment(seg string) error {
	if seg == "" || seg == ".." {
		return ErrInvalid
	}
	if strings.ContainsAny(seg, "/\\") {
		return ErrInvalid // an encoded separator would smuggle a path boundary
	}
	for i := 0; i < len(seg); i++ {
		if c := seg[i]; c == 0 || c < 0x20 || c == 0x7f {
			return ErrInvalid
		}
	}
	if seg[0] == '.' && seg != "." {
		return ErrInvalid // dotfiles never serve (§3)
	}
	return nil
}

// validIDPart is the repository-id part rule (contract.go): ASCII
// [A-Za-z0-9._-], 1..100 bytes, not "." or "..", no leading dot.
func validIDPart(s string) bool {
	if len(s) == 0 || len(s) > 100 || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// joinPath joins two slash-relative paths without producing "//" or a leading
// slash for an empty base.
func joinPath(base, name string) string {
	switch {
	case base == "":
		return name
	case name == "":
		return base
	default:
		return base + "/" + name
	}
}

// cleanRoot validates the configured per-repo site root: a clean, relative
// path whose segments are ordinary directory names (§5). A dot-directory root
// is refused for the same reason dotfiles never serve.
func cleanRoot(root string) (string, bool) {
	if root == "" {
		return "", true
	}
	if len(root) > 512 || strings.HasPrefix(root, "/") || strings.HasSuffix(root, "/") {
		return "", false
	}
	for i := 0; i < len(root); i++ {
		if c := root[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "", false
		}
	}
	for _, seg := range strings.Split(root, "/") {
		if seg == "" || seg == "." || seg == ".." || seg[0] == '.' {
			return "", false
		}
	}
	return root, true
}

// validRef accepts the configured site ref: "" (HEAD) or a ref name that
// `git rev-parse` cannot read as an option. The rule is deliberately the same as
// internal/config's validGitRefName (settings.go) — no "@", no "..", no "@{", no
// "//", no component starting with "." or ending in ".lock", no leading "-" or
// "/", no trailing "/" or "." — plus a 250-byte length bound. The handler is the
// last gate before the subprocess, so a seam cannot hand rev-parse a name the
// settings validator would have rejected (§5).
func validRef(ref string) bool {
	if ref == "" {
		return true
	}
	if len(ref) > 250 || ref == "@" || strings.HasPrefix(ref, "-") ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return false
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		switch c := ref[i]; {
		case c < 0x20, c == 0x7f:
			return false
		case c == ' ', c == '~', c == '^', c == ':', c == '?', c == '*', c == '[', c == '\\':
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// cleanIndexFiles filters the configured index names down to plain file names
// and falls back to the default pair when nothing usable remains.
func cleanIndexFiles(in []string) []string {
	out := make([]string, 0, len(in))
	for _, name := range in {
		if validIndexName(name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return DefaultIndexFiles()
	}
	return out
}

// validIndexName accepts a single ordinary path segment.
func validIndexName(name string) bool {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\") {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return name != "." && name != ".." && name[0] != '.'
}
