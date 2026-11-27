package runner

// PAR-096: the setup-* cache mapping (docs/threads.md §3.4d.1).
//
// The mapping lives in internal/actions so the server (which rewrites a
// resolvable setup-* step around the built-in cache) and the runner (which
// still sees an unresolvable one and must name the replacement) cannot
// disagree. This file keeps the runner's small entry point.

import (
	"strings"

	"github.com/crueber/spindle/internal/actions"
)

// setupCacheRecipe renders the named refusal for one setup-* action the runner
// cannot execute (the action source has no such repository, or policy denied
// the mutable ref).
func setupCacheRecipe(base string, with map[string]string) string {
	return actions.SetupRefusal(base, with, "there is no resolvable action source for it")
}

// restoreHint trims a key to its stable prefix (everything before the commit).
// It is the runner's pinned spelling of the documented key shape (the server's
// rewrite uses the same rule in internal/actions).
func restoreHint(key string) string {
	if i := strings.LastIndex(key, "-${{ github.sha }}"); i > 0 {
		return key[:i+1]
	}
	return key
}
