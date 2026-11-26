package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// Trigger filter patterns (docs/WORKFLOWS.md §3, PAR-013).
//
// Branches, tags and changed paths are matched with the Actions filter dialect:
//
//	*   zero or more characters, not `/`
//	**  zero or more characters, including `/`
//	?   zero or one of the preceding character
//	+   one or more of the preceding character
//	!   at the start of a pattern, negation; order matters (last match wins)
//
// A `branches-ignore`/`tags-ignore`/`paths-ignore` list is an exclusion list:
// a name fires when no pattern in it matches. The include form is ordered: a
// matching negative pattern after a positive one excludes, and a matching
// positive pattern after a negative one includes again. The dialect is compiled
// at parse time, so a malformed pattern is that file's parse error, never a
// filter that silently matches nothing.
//
// This is deliberately GitHub's cheat sheet, not shell globbing: `?` and `+`
// quantify the PRECEDING character (so `v?` matches "" and "v"), which is what
// the Actions docs specify and what a Forgejo author's `branches: ['release?']`
// means.

// compiledFilter is one ordered pattern list.
type compiledFilter struct {
	patterns []*compiledPattern
	// exclude marks a *-ignore list: a match excludes rather than includes.
	exclude bool
}

// compiledPattern is one pattern and its negated flag.
type compiledPattern struct {
	raw     string
	negated bool
	re      *regexp.Regexp
}

// compileFilter validates and compiles one pattern list. exclude compiles a
// `*-ignore` list (where `!` is refused: an ignored pattern is already the
// negative form, and a second negation has no documented meaning).
func compileFilter(patterns []string, exclude bool) (*compiledFilter, error) {
	f := &compiledFilter{exclude: exclude, patterns: make([]*compiledPattern, 0, len(patterns))}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			return nil, fmt.Errorf("filter pattern must not be empty")
		}
		negated := false
		if strings.HasPrefix(p, "!") {
			if exclude {
				return nil, fmt.Errorf("a *-ignore list does not take a negated pattern (%q); list the pattern without `!`", raw)
			}
			negated = true
			p = strings.TrimSpace(strings.TrimPrefix(p, "!"))
			if p == "" {
				return nil, fmt.Errorf("a negated filter pattern needs a pattern after `!`")
			}
		}
		re, err := compileGlob(p)
		if err != nil {
			return nil, fmt.Errorf("filter pattern %q: %v", raw, err)
		}
		f.patterns = append(f.patterns, &compiledPattern{raw: raw, negated: negated, re: re})
	}
	return f, nil
}

// compileGlob translates one filter pattern into an anchored regular
// expression. The translation is the documented dialect and nothing else.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	atoms := make([]string, 0, len(pattern))
	wildcard := make([]bool, 0, len(pattern))
	prevIsWildcard := func() bool {
		return len(wildcard) > 0 && wildcard[len(wildcard)-1]
	}
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				atoms = append(atoms, `.*`)
				wildcard = append(wildcard, true)
				for i+1 < len(pattern) && pattern[i+1] == '*' {
					i++
				}
				continue
			}
			atoms = append(atoms, `[^/]*`)
			wildcard = append(wildcard, true)
		case '?', '+':
			if len(atoms) == 0 || prevIsWildcard() {
				return nil, fmt.Errorf("`%c` must follow a literal character", c)
			}
			atoms[len(atoms)-1] += string(c)
		default:
			atoms = append(atoms, regexp.QuoteMeta(string(c)))
			wildcard = append(wildcard, false)
		}
	}
	re, err := regexp.Compile(`^` + strings.Join(atoms, "") + `$`)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %v", err)
	}
	return re, nil
}

// Match applies the list to one name. A nil/empty include list matches
// everything; a nil/empty ignore list excludes nothing.
func (f *compiledFilter) Match(name string) bool {
	if f == nil || len(f.patterns) == 0 {
		return !f.isIgnore()
	}
	if f.exclude {
		for _, p := range f.patterns {
			if p.re.MatchString(name) {
				return false
			}
		}
		return true
	}
	matched := false
	for _, p := range f.patterns {
		if !p.re.MatchString(name) {
			continue
		}
		// Ordered include/exclude: the LAST matching pattern decides.
		matched = !p.negated
	}
	return matched
}

func (f *compiledFilter) isIgnore() bool { return f != nil && f.exclude }

// matchAnyList compiles a list for the one-shot helpers (used by tests and by
// callers that build a trigger by hand).
func matchAnyList(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	f, err := compileFilter(patterns, false)
	if err != nil {
		return false
	}
	return f.Match(name)
}

// pathFiltered reports whether a trigger declares any path filter.
func pathFiltered(paths, pathsIgnore []string) bool {
	return len(paths) > 0 || len(pathsIgnore) > 0
}

// matchPaths applies paths/paths-ignore to a commit's changed files. known is
// false when the caller could not compute the diff: a path-filtered trigger
// then does not fire (fail closed — a docs-only or code-only decision cannot be
// guessed).
func matchPaths(paths, pathsIgnore []string, changed []string, known bool) bool {
	if !pathFiltered(paths, pathsIgnore) {
		return true
	}
	if !known {
		return false
	}
	if len(paths) > 0 {
		f, err := compileFilter(paths, false)
		if err != nil {
			return false
		}
		fired := false
		for _, name := range changed {
			if f.Match(name) {
				fired = true
				break
			}
		}
		if !fired {
			return false
		}
	}
	if len(pathsIgnore) > 0 {
		f, err := compileFilter(pathsIgnore, true)
		if err != nil {
			return false
		}
		for _, name := range changed {
			if !f.Match(name) {
				return false // the path matched an ignore pattern
			}
		}
	}
	return true
}
