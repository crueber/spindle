package actions

// Remote-action policy (PAR-070, docs/features/21_parity_master.md §7 S1/D-5).
//
// D-5's decided shape is `allowlist` mode with `require_sha` outside the
// allowlist: a repository the operator has approved may be referenced by a tag
// or branch (resolved and pinned to a commit at fetch time, then recorded), and
// every other reference must name a full 40-hex commit SHA. The default is
// therefore deny-by-default for mutable refs and "pin it" for everything else,
// which is a refusal an author can act on rather than a trust decision they
// never see.
//
// The modes are:
//
//	deny      every remote action is refused (an air-gapped instance)
//	allowlist the default: `Allow` entries may use tags/branches; everything
//	          else must be digest-pinned (`RequireSHA` on, the default)
//	open      tags/branches are accepted everywhere (an explicit operator
//	          choice; resolution is still pinned and recorded)
//
// No mode weakens the other controls: the ref is always resolved to a commit,
// the content is always hashed, and the run always records both.

import (
	"fmt"
	"strings"
)

// Mode is the operator's remote-action policy mode.
type Mode string

const (
	// ModeDeny refuses every remote action.
	ModeDeny Mode = "deny"
	// ModeAllowlist permits mutable refs only for approved repositories and
	// requires a full commit SHA everywhere else (the default).
	ModeAllowlist Mode = "allowlist"
	// ModeOpen accepts mutable refs everywhere.
	ModeOpen Mode = "open"
)

// Policy is the instance's remote-action policy (threads.actions.*).
type Policy struct {
	Mode Mode
	// Allow holds `owner/repo`, `owner/*`, `*` (and `host/owner/repo` for an
	// explicitly configured source). Matching is case-insensitive.
	Allow []string
	// RequireSHA requires a full commit SHA for a mutable ref outside Allow.
	// It defaults to true; turning it off is an explicit weakening.
	RequireSHA bool
}

// DefaultPolicy is D-5's decided default: allowlist mode, deny by default and
// SHA required outside the allowlist.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeAllowlist, RequireSHA: true}
}

// Normalize returns the policy with a usable mode (empty selects allowlist)
// and trimmed entries.
func (p Policy) Normalize() Policy {
	out := p
	if out.Mode == "" {
		out.Mode = ModeAllowlist
	}
	out.Mode = Mode(strings.ToLower(strings.TrimSpace(string(out.Mode))))
	if out.Allow == nil {
		out.Allow = nil
	}
	trimmed := make([]string, 0, len(out.Allow))
	for _, entry := range out.Allow {
		if e := strings.TrimSpace(entry); e != "" {
			trimmed = append(trimmed, e)
		}
	}
	out.Allow = trimmed
	return out
}

// Validate checks a policy (the config loader calls this; the resolver treats
// an invalid policy as a refusal rather than guessing).
func (p Policy) Validate() error {
	n := p.Normalize()
	switch n.Mode {
	case ModeDeny, ModeAllowlist, ModeOpen:
	default:
		return fmt.Errorf("threads.actions.mode must be deny, allowlist or open (got %q)", p.Mode)
	}
	for i, entry := range p.Allow {
		if err := validateAllowEntry(entry); err != nil {
			return fmt.Errorf("threads.actions.allow[%d]: %v", i, err)
		}
	}
	return nil
}

// validateAllowEntry accepts `*`, `owner/*`, `owner/repo`, and the same with a
// source host prefix (`host/owner/repo`, `host/owner/*`).
func validateAllowEntry(entry string) error {
	if entry != strings.TrimSpace(entry) {
		return fmt.Errorf("%q has surrounding whitespace", entry)
	}
	if entry == "*" {
		return nil
	}
	if strings.Contains(entry, "://") {
		return fmt.Errorf("%q must not carry a scheme: give owner/repo, owner/*, or host/owner/repo", entry)
	}
	segs := strings.Split(entry, "/")
	if len(segs) < 2 || len(segs) > 3 {
		return fmt.Errorf("%q must be owner/repo, owner/*, or host/owner/repo", entry)
	}
	for _, seg := range segs {
		if seg == "" {
			return fmt.Errorf("%q has an empty path segment", entry)
		}
		if strings.HasPrefix(seg, "-") {
			return fmt.Errorf("%q has a segment starting with '-'", entry)
		}
	}
	last := segs[len(segs)-1]
	if last != "*" && (strings.ContainsAny(last, "*?[]") || strings.HasPrefix(last, ".")) {
		return fmt.Errorf("%q: only a trailing * wildcard is supported (owner/*)", entry)
	}
	return nil
}

// Decision is one policy decision, kept for the run record and the log.
type Decision struct {
	Allowed bool
	// Pinned reports that the reference named a full commit SHA.
	Pinned bool
	// Allowlisted reports that the repository matched Allow.
	Allowlisted bool
	// AllowEntry is the matched entry ("" when none matched).
	AllowEntry string
	// Reason is the named refusal for Allowed=false.
	Reason string
}

// Decide applies the policy to one reference. A full commit SHA is accepted in
// every mode except deny; a mutable ref needs an allowlist match, or open mode.
func (p Policy) Decide(ref Ref) Decision {
	n := p.Normalize()
	d := Decision{Pinned: ref.CommitSHA()}
	// An invalid policy fails closed: the operator's configuration is the
	// authority, and guessing what they meant is how a typo becomes a hole.
	if err := n.Validate(); err != nil {
		d.Reason = "remote action policy is invalid (" + err.Error() + "); refusing every remote action (fail closed)"
		return d
	}
	if n.Mode == ModeDeny {
		d.Reason = "remote actions are disabled (threads.actions.mode = deny); use a repository-local action (uses: ./<path>) or a docker:// image"
		return d
	}
	if d.Pinned {
		d.Allowed = true
		return d
	}
	if entry, ok := matchAllow(n.Allow, ref); ok {
		d.Allowed, d.Allowlisted, d.AllowEntry = true, true, entry
		return d
	}
	if n.Mode == ModeOpen || !n.RequireSHA {
		d.Allowed = true
		return d
	}
	d.Reason = fmt.Sprintf("action %s names the mutable ref %q and %s is not in threads.actions.allow; pin it to a full 40-character commit SHA (uses: %s@<sha>) or add the repository to the instance allowlist",
		ref.Raw, ref.Ref, ref.Identity(), ref.Repository()+pathSuffix(ref.Path))
	return d
}

// pathSuffix renders the action subpath for an author-facing hint.
func pathSuffix(path string) string {
	if path == "" {
		return ""
	}
	return "/" + path
}

// matchAllow returns the first allowlist entry matching the reference.
func matchAllow(entries []string, ref Ref) (string, bool) {
	id := strings.ToLower(ref.Identity())
	repo := strings.ToLower(ref.Repository())
	for _, entry := range entries {
		e := strings.ToLower(strings.TrimSpace(entry))
		switch {
		case e == "*":
			return entry, true
		case e == id || e == repo:
			return entry, true
		case strings.HasSuffix(e, "/*"):
			prefix := strings.TrimSuffix(e, "*")
			if strings.HasPrefix(id, prefix) || strings.HasPrefix(repo, prefix) {
				return entry, true
			}
		}
	}
	return "", false
}
