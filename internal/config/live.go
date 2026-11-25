// live.go — the live [import] SSRF policy (docs/features/10_git_import.md).
//
// # Why this exists
//
// The import/mirror egress gate reads `[import]` at REQUEST time, but the
// `*Config` in a running process is the boot snapshot and is never mutated
// (11_config_cli.md §3.6). Before this holder, an operator who saved
// `import.allow_private_networks = true` on /admin/config got a successful
// save and a "restart required" redirect, while the Import page kept answering
// `source host "…" resolves to a private address (import.allow_private_networks=false)`
// until the process restarted — the save looked ineffective.
//
// LiveImport is the one mutable policy cell: composition constructs it at boot
// from the effective config, injects it into the gate call sites (webui Env,
// the import service, the mirror/push-mirror services, the fork-sync guard),
// and the admin save publishes the new section after the file is written. It
// is deliberately NOT a process-global config pointer: no package can reach it
// without composition handing it over, and only the [import] section — never
// the whole config — travels through it (11_config_cli.md §3.6's "a feature
// that needs hot-reload owns its own snapshot").
//
// # Precedence
//
// The boot ladder is defaults → file → env overlay, env winning. A save writes
// only the file, so the policy it publishes is the candidate's `[import]`
// section with the `SPINDLE__IMPORT__*` / `WALGIT__IMPORT__*` overlay
// reapplied: an environment-supplied key can never be overridden by a file
// save (see PublishFileSave).
package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// LiveImport is the process's live `[import]` policy: an immutable snapshot
// published atomically, read at each request-time egress gate. The zero value
// is empty but usable; use NewLiveImport so the first read answers the boot
// config instead of the compiled defaults. A nil *LiveImport is the unwired
// case: Get reports ok=false and every caller falls back to its startup copy
// (fail closed).
type LiveImport struct {
	imp atomic.Pointer[Import]
}

// NewLiveImport snapshots c's `[import]` section (nil c → compiled defaults,
// which deny loopback, file:// and an empty allowlist).
func NewLiveImport(c *Config) *LiveImport {
	l := &LiveImport{}
	if c == nil {
		l.Set(Defaults().Import)
		return l
	}
	l.Set(c.Import)
	return l
}

// Get returns the published section. ok is false for a nil holder (unwired):
// the caller keeps its startup copy.
func (l *LiveImport) Get() (Import, bool) {
	if l == nil {
		return Import{}, false
	}
	p := l.imp.Load()
	if p == nil {
		return Import{}, false
	}
	return *p, true
}

// Set publishes imp. The value is copied (URLAllowlist included), so a caller
// that keeps mutating its own *Config cannot change the live policy under a
// request that is reading it.
func (l *LiveImport) Set(imp Import) {
	if l == nil {
		return
	}
	cp := imp
	cp.URLAllowlist = append([]string(nil), imp.URLAllowlist...)
	l.imp.Store(&cp)
}

// ImportPolicy resolves the effective `[import]` section for a request-time
// gate: the live holder when wired, else startup (nil → compiled defaults,
// fail closed). It is the one call every gate uses, so the read points cannot
// disagree about precedence.
func ImportPolicy(live *LiveImport, startup *Config) Import {
	if imp, ok := live.Get(); ok {
		return imp
	}
	if startup != nil {
		return startup.Import
	}
	return Defaults().Import
}

// PublishFileSave publishes the policy a successful config-file save makes
// effective: candidate's `[import]` section with the environment overlay
// reapplied (env wins over a file value at boot, and it wins here too).
// environ is the process environment (os.Environ at the call site; tests pass
// an explicit slice). Nothing is published when the overlay cannot be
// re-applied — a booted process has already validated these keys, so the
// error is unreachable in production and the previous policy stays in place
// rather than a half-applied one.
func (l *LiveImport) PublishFileSave(candidate *Config, environ []string) (Import, error) {
	if candidate == nil {
		return ImportPolicy(l, nil), nil
	}
	imp, err := importEnvOverlay(candidate.Import, environ)
	if err != nil {
		return Import{}, err
	}
	l.Set(imp)
	return imp, nil
}

// importEnvOverlay reapplies the `import.*` entries of a SPINDLE__/WALGIT__
// environment overlay to imp. Other sections are ignored on purpose: this is
// the [import] section's after-save truth, not a second config load (an env
// key for a struct slice elsewhere must not make a save fail).
func importEnvOverlay(imp Import, environ []string) (Import, error) {
	entries := collectEnvEntries(environ, nil)
	keys := make([]string, 0, len(entries))
	for key := range entries {
		if strings.HasPrefix(key, "import.") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	c := &Config{Import: imp}
	for _, key := range keys {
		e := entries[key]
		if err := assignPath(c, strings.Split(key, "."), e.raw); err != nil {
			if errors.Is(err, errUnknownKey) {
				// Boot already recorded an unknown key as an ignored
				// override (11_config_cli.md §3.2); a save is not the place
				// to turn it fatal.
				continue
			}
			return Import{}, fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return c.Import, nil
}
