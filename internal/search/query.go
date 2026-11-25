// query.go — the query grammar of docs/features/20_search.md §5.1 and its 400
// text (§10). Every lane (repo API, instance API, CLI, pages) funnels its
// decoded values through Parse, so the rules — and the sentences a caller
// reads — have exactly one home.
package search

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/crueber/spindle/internal/git"
)

// Params is one decoded search request, exactly the wire knobs of §5.1. The
// transport decodes but does not validate; Parse does.
type Params struct {
	Text  string   // q
	Type  string   // "" = the scope's default
	Regex bool     // 1 = POSIX ERE (-E)
	Case  bool     // 1 = exact bytes; the default is ASCII case-insensitive (-i)
	Word  bool     // 1 = whole word (-w)
	Paths []string // repeatable git pathspec
	Limit int      // 0 = DefaultLimit; clamped 1..MaxLimit
	Ref   string   // repository scope only; "" = the default branch
}

// Query is a validated request.
type Query struct {
	Text          string
	Type          Type
	Regex         bool
	CaseSensitive bool
	Word          bool
	Paths         []string
	Limit         int
	Ref           string
	Scope         Scope
}

// Parse validates params for scope and applies the defaults. It performs no
// I/O: a bad query never pays a listing, a resolve or a grep (§8).
func Parse(scope Scope, p Params) (Query, error) {
	q := Query{
		Text:          p.Text,
		Regex:         p.Regex,
		CaseSensitive: p.Case,
		Word:          p.Word,
		Limit:         p.Limit,
		Ref:           p.Ref,
		Scope:         scope,
	}
	switch scope.Kind {
	case ScopeRepo:
		if scope.Owner == "" || scope.Repo == "" {
			return Query{}, &InvalidError{Field: "scope", Why: "a repository search needs an owner and a repository"}
		}
	case ScopeOwner:
		if scope.Owner == "" {
			return Query{}, &InvalidError{Field: "owner", Why: "owner is required"}
		}
		if !validOwnerName(scope.Owner) {
			return Query{}, &InvalidError{Field: "owner", Why: fmt.Sprintf("owner %q is not a valid name", scope.Owner)}
		}
		if scope.Repo != "" {
			return Query{}, &InvalidError{Field: "repo", Why: "a repository name is not valid in owner scope"}
		}
	default:
		return Query{}, Wrap(ErrUnsupportedScope, ErrUnsupportedScope.Error())
	}

	// q: required, ≤ 256 bytes, no NUL/CR/LF. A fixed-string query is data
	// even when it starts with '-' (it rides -e, §5.1); the rules below keep
	// the expression and the NUL-delimited record grammar unambiguous.
	switch {
	case q.Text == "":
		return Query{}, &InvalidError{Field: "q", Why: "q is required"}
	case len(q.Text) > MaxPatternBytes:
		return Query{}, &InvalidError{Field: "q", Why: fmt.Sprintf("q exceeds %d bytes", MaxPatternBytes)}
	case strings.ContainsAny(q.Text, "\x00\n\r"):
		return Query{}, &InvalidError{Field: "q", Why: "q contains a NUL, CR or newline"}
	}
	if q.Regex {
		// RE2 in POSIX mode is the linear-time validator for the dialect git's
		// regcomp accepts (decision 3). git's own exit code is still
		// classified at run time: a corner RE2 accepts and regcomp rejects is
		// a 400 from the primitive, never an empty result.
		if _, err := regexp.CompilePOSIX(q.Text); err != nil {
			return Query{}, &InvalidError{Field: "q", Why: "invalid regular expression: " + err.Error()}
		}
	}

	// pathspecs: git defaults only. A leading ':' selects pathspec magic and a
	// leading '!'/'^' negates — both silently change what is searched, so both
	// are rejected (§5.1, decision 9).
	if len(p.Paths) > MaxPathPatterns {
		return Query{}, &InvalidError{Field: "path", Why: fmt.Sprintf("at most %d path patterns are accepted", MaxPathPatterns)}
	}
	q.Paths = make([]string, 0, len(p.Paths))
	for i, path := range p.Paths {
		switch {
		case path == "":
			return Query{}, &InvalidError{Field: "path", Why: fmt.Sprintf("path pattern %d is empty", i+1)}
		case len(path) > MaxPathBytes:
			return Query{}, &InvalidError{Field: "path", Why: fmt.Sprintf("path pattern %d exceeds %d bytes", i+1, MaxPathBytes)}
		case strings.ContainsAny(path, "\x00\n\r"):
			return Query{}, &InvalidError{Field: "path", Why: fmt.Sprintf("path pattern %d contains a NUL, CR or newline", i+1)}
		case strings.ContainsAny(path[:1], ":!^"):
			return Query{}, &InvalidError{Field: "path", Why: fmt.Sprintf("path pattern %d uses pathspec magic (a leading :, ! or ^ changes the query)", i+1)}
		}
		q.Paths = append(q.Paths, path)
	}

	switch {
	case q.Limit == 0:
		q.Limit = DefaultLimit
	case q.Limit < 0:
		q.Limit = 1
	case q.Limit > MaxLimit:
		q.Limit = MaxLimit
	}

	switch typ := Type(strings.TrimSpace(p.Type)); typ {
	case "":
		// §5.1: code in repository scope, issues on the global (owner) surface.
		if scope.Kind == ScopeRepo {
			q.Type = TypeCode
		} else {
			q.Type = TypeIssues
		}
	case TypeCode, TypeIssues, TypeAll:
		q.Type = typ
	default:
		return Query{}, &InvalidError{Field: "type", Why: fmt.Sprintf("type must be code, issues or all (got %q)", p.Type)}
	}

	if scope.Kind == ScopeRepo {
		if err := validateRef(q.Ref); err != nil {
			return Query{}, err
		}
	} else if q.Ref != "" {
		// §6.2: ref is repository-scope only. Silently ignoring it would search
		// a different thing than the caller asked for.
		return Query{}, &InvalidError{Field: "ref", Why: "ref is available in repository scope only"}
	}
	return q, nil
}

// wantsCode / wantsIssues split the requested sections.
func (q Query) wantsCode() bool   { return q.Type == TypeCode || q.Type == TypeAll }
func (q Query) wantsIssues() bool { return q.Type == TypeIssues || q.Type == TypeAll }

// options renders the normalised query for the wire options echo (§6.2).
func (q Query) options() QueryOptions {
	paths := q.Paths
	if paths == nil {
		paths = []string{}
	}
	return QueryOptions{
		Regex:         q.Regex,
		CaseSensitive: q.CaseSensitive,
		Word:          q.Word,
		Paths:         paths,
		Limit:         q.Limit,
	}
}

// grepQuery is the primitive request. Caps are passed explicitly (not left to
// the primitive's zero-value defaults) so the service's contract is visible
// here, and Timeout is the hard per-subprocess deadline.
func (q Query) grepQuery() git.GrepQuery {
	var paths []string
	if len(q.Paths) > 0 {
		paths = append([]string(nil), q.Paths...)
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxMatchesPerRepo {
		// Defensive: Parse already clamps to DefaultLimit..MaxLimit, but a
		// hand-built Query must never raise the engine's cap.
		limit = MaxMatchesPerRepo
	}
	return git.GrepQuery{
		Pattern:       q.Text,
		Regex:         q.Regex,
		CaseSensitive: q.CaseSensitive,
		Word:          q.Word,
		Paths:         paths,
		PerFile:       MatchesPerFile,
		Limit:         limit,
		MaxBytes:      MaxOutputBytes,
		MaxText:       MaxTextBytes,
		Timeout:       GrepTimeout,
	}
}

// validateRef applies §4's malformed-ref rules (400). A well-formed but
// unknown ref is the source's ErrNotFound (404), not this function's business.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	switch {
	case len(ref) > 256:
		return &InvalidError{Field: "ref", Why: "ref exceeds 256 bytes"}
	case ref[0] == '-':
		return &InvalidError{Field: "ref", Why: "ref must not start with '-'"}
	case strings.ContainsAny(ref, "\x00\n\r"):
		return &InvalidError{Field: "ref", Why: "ref contains a NUL, CR or newline"}
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return &InvalidError{Field: "ref", Why: "ref contains a control character"}
		}
	}
	return nil
}

// validOwnerName mirrors git.RepoId's part rule (ASCII letters, digits, '.',
// '_' and '-', 1..100 bytes, not "." or ".." and no leading dot) without
// importing the parser: the owner arrives as a query parameter, and an
// arbitrary string must not reach a store listing.
func validOwnerName(s string) bool {
	if len(s) == 0 || len(s) > 100 || s == ".." || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}
