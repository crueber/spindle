// Package actions resolves and verifies remote actions for Threads
// (PAR-070/071/072/074/096, docs/threads.md §3.4d).
//
// It is deliberately a leaf: no store, no git reader, no HTTP server. A
// Resolver takes a Source (the instance's git, or one explicitly configured
// HTTP source) and returns the action's metadata and its exact file set,
// bounded and digest-pinned. The server embeds that payload in the job spec;
// the runner executes it without network access and without ever reading the
// repository again.
//
// The supply-chain contract this package implements:
//
//   - resolve the ref to a commit SHA at first fetch, and record it;
//   - pin the action's content to a sha256 over its files, and refuse when the
//     same commit ever yields different bytes (a changed digest is never a
//     cache hit);
//   - default-deny: remote actions are refused unless policy allows the
//     reference, and a mutable ref outside the allowlist is a named refusal;
//   - no implicit credential: nothing here mints, reads or attaches a token.
package actions

import (
	"fmt"
	"regexp"
	"strings"
)

// Ref is one parsed remote action reference: `owner/repo[/path…]@ref` or an
// absolute `https://…` URL.
type Ref struct {
	// Raw is the `uses:` value as authored.
	Raw string
	// Host is the source host for a full-URL reference ("" for the bare
	// `owner/repo` spelling).
	Host string
	// Scheme is "https" for a full URL ("" for the bare spelling).
	Scheme string
	// Owner and Repo identify the action repository.
	Owner string
	Repo  string
	// Path is the subdirectory holding action.yml ("" means the repository
	// root).
	Path string
	// Ref is the tag, branch or commit SHA after `@`.
	Ref string
}

// Repository returns "owner/repo" (the allowlist identity for the default
// source).
func (r Ref) Repository() string { return r.Owner + "/" + r.Repo }

// Identity returns the allowlist identity: "owner/repo" for the instance's own
// actions, "host/owner/repo" for an explicitly configured source host.
func (r Ref) Identity() string {
	if r.Host != "" {
		return r.Host + "/" + r.Repository()
	}
	return r.Repository()
}

// String renders the reference for logs and refusals.
func (r Ref) String() string { return r.Owner + "/" + r.Repo + "/" + r.Path + "@" + r.Ref }

// CommitSHA reports whether the ref is a full 40-hex commit SHA (a
// digest-pinned reference). A short SHA is NOT pinned: it is ambiguous and has
// to be resolved like a branch.
func (r Ref) CommitSHA() bool { return IsCommitSHA(r.Ref) }

// IsCommitSHA reports whether s is a full lowercase 40-hex git object id.
func IsCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maxRefLen bounds one authored `uses:` value.
const maxRefLen = 512

var (
	// ownerRe and repoRe are git host repository names (no spaces, no control
	// characters, no path separators). A leading '-' is refused because it
	// could be read as a git option.
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?$`)
)

// ErrRefSyntax marks an unparseable reference (a job-level refusal, never a
// silent skip).
var ErrRefSyntax = fmt.Errorf("invalid action reference")

// ParseRef decodes one remote `uses:` value. It accepts
// `owner/repo[/path]@ref` and `https://host/owner/repo[/path]@ref`. A local
// (`./…`), docker (`docker://…`) or bare built-in name is NOT a remote ref and
// returns ErrRefSyntax; callers decide those before calling ParseRef.
func ParseRef(raw string) (Ref, error) {
	uses := strings.TrimSpace(raw)
	if uses == "" {
		return Ref{}, fmt.Errorf("%w: empty", ErrRefSyntax)
	}
	if len(uses) > maxRefLen {
		return Ref{}, fmt.Errorf("%w: %d characters is over the limit of %d", ErrRefSyntax, len(uses), maxRefLen)
	}
	if strings.ContainsAny(uses, "\x00\r\n\t ") {
		return Ref{}, fmt.Errorf("%w: %q contains whitespace or a control character", ErrRefSyntax, uses)
	}
	ref := Ref{Raw: uses}
	rest := uses
	if strings.Contains(rest, "://") {
		scheme, after, ok := strings.Cut(rest, "://")
		if !ok || (scheme != "https" && scheme != "http") {
			return Ref{}, fmt.Errorf("%w: only https:// action URLs are accepted (got %q)", ErrRefSyntax, uses)
		}
		host, tail, ok := strings.Cut(after, "/")
		if !ok || host == "" || tail == "" {
			return Ref{}, fmt.Errorf("%w: %q needs a host and an owner/repo path", ErrRefSyntax, uses)
		}
		if !hostRe.MatchString(host) {
			return Ref{}, fmt.Errorf("%w: %q is not a valid source host", ErrRefSyntax, host)
		}
		ref.Scheme, ref.Host, rest = scheme, strings.ToLower(host), tail
	}
	at := strings.LastIndexByte(rest, '@')
	if at <= 0 || at == len(rest)-1 {
		return Ref{}, fmt.Errorf("%w: %q needs @<ref> (a tag, branch or 40-character commit SHA)", ErrRefSyntax, uses)
	}
	path, tag := rest[:at], rest[at+1:]
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return Ref{}, fmt.Errorf("%w: %q needs owner/repo", ErrRefSyntax, uses)
	}
	ref.Owner, ref.Repo = segs[0], segs[1]
	if !ownerRe.MatchString(ref.Owner) || !repoRe.MatchString(ref.Repo) {
		return Ref{}, fmt.Errorf("%w: %q is not a valid owner/repo name", ErrRefSyntax, segs[0]+"/"+segs[1])
	}
	for _, seg := range segs[2:] {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, `\:`) {
			return Ref{}, fmt.Errorf("%w: %q has an invalid path segment %q", ErrRefSyntax, uses, seg)
		}
		ref.Path = strings.Trim(ref.Path+"/"+seg, "/")
	}
	if err := validRefName(tag); err != nil {
		return Ref{}, fmt.Errorf("%w: %q: %v", ErrRefSyntax, uses, err)
	}
	ref.Ref = tag
	return ref, nil
}

// validRefName bounds the `@ref` part: a tag, branch or 40-hex SHA. It follows
// git's ref-name rules at the characters that can turn a read into an option
// or escape a path.
func validRefName(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("the ref is empty")
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("the ref must not start with '-'")
	case strings.ContainsAny(ref, " \t\r\n\x00~^:?*[\\"):
		return fmt.Errorf("the ref %q contains a character git refuses in a ref name", ref)
	case strings.Contains(ref, ".."), strings.HasSuffix(ref, "."), strings.HasSuffix(ref, "/"),
		strings.HasPrefix(ref, "/"), strings.Contains(ref, "@{"):
		return fmt.Errorf("the ref %q is not a valid git ref name", ref)
	}
	return nil
}
