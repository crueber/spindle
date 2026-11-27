// runner_config.go — offline runner registration (PAR-180).
//
// A runner can be declared in a file instead of minted in a browser and pasted
// onto a command line:
//
//	spindle runner serve --config /etc/spindle/runner.toml
//
// One file can carry several [[runner]] entries, each with its own credential
// (therefore its own admin-set policy/labels scope), so ONE process serves two
// or more scopes: a repository-scoped worker beside an instance-scoped one, or
// two organizations' fleets on one host. Each entry gets its own runner loop;
// the docker/image/container-policy settings are process-wide.
//
// Secrets never appear in argv: an entry names token_file (0600, the normal
// case) or a one-time registration_file. A literal `token`/`register_token` is
// accepted for a file with restrictive permissions, but is never echoed.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// runnerFile is the TOML shape.
type runnerFile struct {
	Runner []runnerFileEntry `toml:"runner"`
}

// runnerFileEntry is one [[runner]] table.
type runnerFileEntry struct {
	Name     string   `toml:"name"`
	Server   string   `toml:"server"`
	Labels   []string `toml:"labels"`
	Capacity int      `toml:"capacity"`
	// Token is the long-lived credential (wrt_). Prefer token_file.
	Token string `toml:"token"`
	// TokenFile is a 0600 file holding the credential; --register writes it.
	TokenFile string `toml:"token_file"`
	// RegisterToken / RegisterFile bootstrap a new runner (one-time wrg_).
	RegisterToken string  `toml:"register_token"`
	RegisterFile  string  `toml:"register_file"`
	AllowForkRuns bool    `toml:"allow_fork_runs"`
	Ephemeral     bool    `toml:"ephemeral"`
	MaxCPUs       float64 `toml:"max_cpus"`
	MaxMemory     string  `toml:"max_memory"`
}

// loadRunnerFile parses and validates the config. At least one entry is
// required; every entry must name a server (or inherit the global one, applied
// by the caller) and exactly one credential source.
func loadRunnerFile(path string) ([]runnerFileEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc runnerFile
	if _, derr := toml.Decode(string(raw), &doc); derr != nil {
		return nil, fmt.Errorf("%s: %v", path, derr)
	}
	if len(doc.Runner) == 0 {
		return nil, fmt.Errorf("%s: no [[runner]] entry", path)
	}
	for i := range doc.Runner {
		e := &doc.Runner[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Server = strings.TrimSpace(e.Server)
		e.Token = strings.TrimSpace(e.Token)
		e.TokenFile = strings.TrimSpace(e.TokenFile)
		e.RegisterToken = strings.TrimSpace(e.RegisterToken)
		e.RegisterFile = strings.TrimSpace(e.RegisterFile)
		sources := 0
		for _, v := range []string{e.Token, e.TokenFile, e.RegisterToken, e.RegisterFile} {
			if v != "" {
				sources++
			}
		}
		if sources == 0 {
			return nil, fmt.Errorf("%s: runner %d (%s) has no credential (token_file, token, register_file or register_token)", path, i+1, e.Name)
		}
		if sources > 1 {
			return nil, fmt.Errorf("%s: runner %d (%s) names more than one credential source; use one of token_file/register_file", path, i+1, e.Name)
		}
		if e.Capacity < 0 || e.Capacity > 64 {
			return nil, fmt.Errorf("%s: runner %d: capacity must be 0..64", path, i+1)
		}
		if e.Ephemeral && e.Capacity > 1 {
			return nil, fmt.Errorf("%s: runner %d: an ephemeral runner is a one-job worker (capacity 1)", path, i+1)
		}
		if e.MaxCPUs < 0 || e.MaxCPUs > 1024 {
			return nil, fmt.Errorf("%s: runner %d: max_cpus must be 0..1024", path, i+1)
		}
	}
	return doc.Runner, nil
}

// readSecretFile reads a 0600-byte credential file and trims it. A file with
// group/world permission is refused: a runner credential is a secret at rest.
func readSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must not be group/world readable (chmod 600)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// writeSecretFile writes a credential with 0600.
func writeSecretFile(path, secret string) error {
	return os.WriteFile(path, []byte(strings.TrimSpace(secret)+"\n"), 0o600)
}

// errPoolRefused is the named D-14 refusal for a managed pool command.
var errPoolRefused = errors.New("a managed runner pool is off by default (D-14): spindle does not autoscale workers. Run `spindle runner serve --ephemeral` workers under your own supervisor (one registration token each), or a fleet of capacity-1 runners")
