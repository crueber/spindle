package runner

// PAR-096: the setup-* cache mapping. A `setup-node`/`setup-go`/`setup-python`
// step is marketplace JavaScript code, which Threads does not execute (there is
// no marketplace, PAR-070/PAR-071). Its CACHE half is supported by the built-in
// actions/cache service, so the refusal names the exact replacement instead of
// leaving an author to guess. The recipes keep every path inside the workspace:
// a tool's default cache directory (`~/.npm`, `~/.cache/pip`, `~/.cache/go-build`)
// lives in the container's ephemeral home, which the next job does not have.

import (
	"sort"
	"strings"
)

// setupRecipe is one ecosystem's replacement recipe.
type setupRecipe struct {
	ecosystem string
	env       string
	paths     string
	keyHint   string
	notes     string
}

// setupRecipes is the documented table (docs/threads.md §3.4c). A setup action
// that names a cache manager outside its ecosystem's list is still refused by
// name, and the refusal quotes the managers it can map.
var setupRecipes = map[string]setupRecipe{
	"actions/setup-node": {
		ecosystem: "npm, yarn or pnpm",
		env:       "npm_config_cache: .npm",
		paths:     ".npm",
		keyHint:   "npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		notes:     "for yarn/pnpm point YARN_CACHE_FOLDER/PNPM_STORE_DIR at a workspace directory and cache that, or set npm_config_store_dir=.pnpm-store",
	},
	"actions/setup-go": {
		ecosystem: "the Go build cache and module cache",
		env:       "GOCACHE: .cache/go-build, GOMODCACHE: .cache/gomod",
		paths:     ".cache/go-build\n.cache/gomod",
		keyHint:   "go-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		notes:     "both directories must be set to workspace paths, or the cache has nothing to restore",
	},
	"actions/setup-python": {
		ecosystem: "pip",
		env:       "PIP_CACHE_DIR: .cache/pip",
		paths:     ".cache/pip",
		keyHint:   "pip-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		notes:     "set PIP_CACHE_DIR in the job env so every pip invocation uses the cached directory",
	},
}

// setupCacheRecipe renders the named refusal for one setup-* action.
func setupCacheRecipe(base string, with map[string]string) string {
	recipe, ok := setupRecipes[base]
	if !ok {
		// setup-java / setup-dotnet / setup-ruby: the ecosystem's cache layout
		// is not mapped yet, so the refusal says exactly that.
		return "unsupported action " + base + ": there is no marketplace for remote actions, and this setup action's cache layout is not one of the mapped ecosystems (npm/pip/go). Use a run step plus actions/cache with a workspace-relative path (docs/threads.md §3.4c)"
	}
	var named []string
	for key := range with {
		named = append(named, strings.ToLower(key))
	}
	suffix := ""
	if len(named) > 0 {
		sort.Strings(named)
		suffix = " (inputs: " + strings.Join(named, ", ") + ")"
	}
	return "unsupported action " + base + suffix + " is a marketplace JavaScript action, which Threads does not execute; its cache half is built in — replace the step with a run step that sets `env: " + recipe.env +
		"` plus `uses: actions/cache` with `with: { path: '" + strings.ReplaceAll(recipe.paths, "\n", "', path: '") +
		"', key: " + recipe.keyHint + ", restore-keys: " + restoreHint(recipe.keyHint) + " }` (docs/threads.md §3.4c). " + recipe.notes
}

// restoreHint trims a key to its stable prefix (everything before the commit).
func restoreHint(key string) string {
	if i := strings.LastIndex(key, "-${{ github.sha }}"); i > 0 {
		return key[:i+1]
	}
	return key
}
