package actions

// setup-* cache compatibility (PAR-096).
//
// A `setup-node`/`setup-go`/`setup-python` step is a remote JavaScript action
// (now executable through PAR-070/071), and its `cache:` input is a promise
// that the job's package cache is restored before and saved after. spindle's
// cache service already supports the key/restore-key shapes those inputs
// produce, so the server rewrites the step around a built-in `actions/cache`
// pair instead of refusing the whole action. Every ecosystem this table does
// not map is refused BY NAME with the reason, never silently ignored.

import (
	"fmt"
	"sort"
	"strings"
)

// SetupCache is one ecosystem's cache mapping.
type SetupCache struct {
	// Ecosystem names the managers the input accepts (for the message).
	Ecosystem string
	// Env points the tool at a workspace-relative cache directory
	// (`npm_config_cache: .npm`); every entry must be workspace-relative or the
	// next job has nothing to restore.
	Env map[string]string
	// Paths is the actions/cache `path` value (newline separated).
	Paths string
	// Key is the cache key; RestoreKey the stable prefix used as a fallback.
	Key, RestoreKey string
	// Notes is the human explanation appended to the refusal.
	Notes string
	// Managers maps a `cache:` input value to supported (empty value = the
	// action's own default manager).
	Managers []string
}

var setupCaches = map[string]SetupCache{
	"actions/setup-node": {
		Ecosystem: "npm, yarn or pnpm",
		Env:       map[string]string{"npm_config_cache": ".npm"},
		Paths:     ".npm",
		Key:       "npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		Notes:     "for yarn/pnpm point YARN_CACHE_FOLDER/PNPM_STORE_DIR at a workspace directory and cache that, or set npm_config_store_dir=.pnpm-store",
		Managers:  []string{"npm", "yarn", "pnpm"},
	},
	"actions/setup-go": {
		Ecosystem: "the Go build cache and module cache",
		Env:       map[string]string{"GOCACHE": ".cache/go-build", "GOMODCACHE": ".cache/gomod"},
		Paths:     ".cache/go-build\n.cache/gomod",
		Key:       "go-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		Notes:     "both directories must be set to workspace paths, or the cache has nothing to restore (GOMODCACHE=.cache/gomod)",
		Managers:  []string{"true", ""},
	},
	"actions/setup-python": {
		Ecosystem: "pip",
		Env:       map[string]string{"PIP_CACHE_DIR": ".cache/pip"},
		Paths:     ".cache/pip",
		Key:       "pip-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}",
		Notes:     "set PIP_CACHE_DIR in the job env so every pip invocation uses the cached directory",
		Managers:  []string{"pip", "pipenv", "true", ""},
	},
}

// SetupCacheFor returns the mapping for a setup-* action base ("owner/repo").
func SetupCacheFor(base string) (SetupCache, bool) {
	c, ok := setupCaches[strings.ToLower(strings.TrimSpace(base))]
	if !ok {
		return SetupCache{}, false
	}
	c.RestoreKey = restoreHint(c.Key)
	return c, true
}

// SetupCacheInput validates one step's `cache:` input. supported=false with a
// non-empty reason is the named refusal. An empty input means "no cache asked
// for" (reason empty, supported false).
func SetupCacheInput(base, cache string) (SetupCache, bool, string) {
	c, ok := SetupCacheFor(base)
	if !ok {
		return SetupCache{}, false, fmt.Sprintf("unsupported action %s: its cache layout is not one of the mapped ecosystems (npm/pip/go); use a run step plus actions/cache with a workspace-relative path (docs/threads.md §3.4d.1)", base)
	}
	value := strings.ToLower(strings.TrimSpace(cache))
	for _, manager := range c.Managers {
		if value == manager {
			return c, true, ""
		}
	}
	return c, false, fmt.Sprintf("unsupported action %s: cache: %q is not a manager this step maps (%s); use a run step plus actions/cache with path %q and key %s (docs/threads.md §3.4d.1). %s",
		base, cache, c.Ecosystem, c.Paths, c.Key, c.Notes)
}

// SetupRefusal is the named refusal for a setup-* action that cannot run at
// all (denied by policy, not found, or a runtime the job image does not have).
// It quotes the exact actions/cache replacement so an author can act.
func SetupRefusal(base string, with map[string]string, reason string) string {
	c, ok := SetupCacheFor(base)
	prefix := "unsupported action " + base
	if reason != "" {
		prefix += ": " + reason
	}
	if !ok {
		return prefix + "; its cache layout is not one of the mapped ecosystems (npm/pip/go) — use a run step plus actions/cache with a workspace-relative path (docs/threads.md §3.4d.1)"
	}
	var named []string
	for key := range with {
		named = append(named, strings.ToLower(key))
	}
	sort.Strings(named)
	suffix := ""
	if len(named) > 0 {
		suffix = " (inputs: " + strings.Join(named, ", ") + ")"
	}
	return prefix + suffix + ": the cache half is built in — replace the step with a run step that sets `env: " + envText(c.Env) +
		"` plus `uses: actions/cache` with `with: { path: '" + strings.ReplaceAll(c.Paths, "\n", "', path: '") +
		"', key: " + c.Key + ", restore-keys: " + c.RestoreKey + " }` (docs/threads.md §3.4d.1). " + c.Notes
}

// envText renders one mapping as `KEY: value, KEY2: value2`.
func envText(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+env[k])
	}
	return strings.Join(parts, ", ")
}

// restoreHint trims a key to its stable prefix (everything before the commit).
func restoreHint(key string) string {
	if i := strings.LastIndex(key, "-${{ github.sha }}"); i > 0 {
		return key[:i+1]
	}
	return key
}
