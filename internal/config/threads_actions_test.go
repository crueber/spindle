package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/actions"
)

// Remote-action policy config (PAR-070/D-5): the default is allowlist mode with
// SHA required outside it, this instance as the only source, and a bounded
// fetch; every invalid value names its key.
func TestThreadsActionsDefaults(t *testing.T) {
	c := validOIDC()
	if c.Threads.Actions.Mode != "allowlist" || !c.Threads.Actions.RequireSHA {
		t.Fatalf("defaults = %+v", c.Threads.Actions)
	}
	if len(c.Threads.Actions.Allow) != 0 || c.Threads.Actions.Source != "" {
		t.Fatalf("defaults = %+v", c.Threads.Actions)
	}
	if int64(c.Threads.Actions.MaxBytes) != 16<<20 {
		t.Fatalf("max_bytes = %d", c.Threads.Actions.MaxBytes)
	}
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("default config must validate: %v", errs)
	}
}

func TestThreadsActionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
		ok   bool
	}{
		{"bad mode", func(c *Config) { c.Threads.Actions.Mode = "allowlsit" }, "threads.actions.mode", false},
		{"bad allow row", func(c *Config) { c.Threads.Actions.Allow = []string{"acme/*", "bad entry"} }, "threads.actions.allow[1]", false},
		{"empty allow row", func(c *Config) { c.Threads.Actions.Allow = []string{""} }, "threads.actions.allow[0]", false},
		{"http source", func(c *Config) { c.Threads.Actions.Source = "http://example.com" }, "threads.actions.source", false},
		{"ftp source", func(c *Config) { c.Threads.Actions.Source = "ftp://example.com" }, "threads.actions.source", false},
		{"https source", func(c *Config) { c.Threads.Actions.Source = "https://git.example.com" }, "", true},
		{"loopback http source", func(c *Config) { c.Threads.Actions.Source = "http://127.0.0.1:8080" }, "", true},
		{"negative max", func(c *Config) { c.Threads.Actions.MaxBytes = -1 }, "threads.actions.max_bytes", false},
		{"open mode", func(c *Config) { c.Threads.Actions.Mode = "open" }, "", true},
		{"deny mode", func(c *Config) { c.Threads.Actions.Mode = "deny" }, "", true},
	} {
		c := validOIDC()
		tc.mut(c)
		_, errs := Validate(c)
		if tc.ok && len(errs) != 0 {
			t.Fatalf("%s: unexpected errors %v", tc.name, errs)
		}
		if !tc.ok {
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: errors %v missing %q", tc.name, errs, tc.want)
			}
		}
	}
}

// TestThreadsActionsTOML loads the documented spelling.
func TestThreadsActionsTOML(t *testing.T) {
	src := `
[threads.actions]
mode = "allowlist"
allow = ["acme/tools", "actions/*"]
require_sha = true
source = "https://git.example.com"
max_bytes = "32MiB"
`
	c := Defaults()
	if _, err := toml.Decode(src, c); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Threads.Actions.Allow) != 2 || c.Threads.Actions.Source == "" || int64(c.Threads.Actions.MaxBytes) != 32<<20 {
		t.Fatalf("loaded = %+v", c.Threads.Actions)
	}
	if err := (actions.Policy{Mode: actions.Mode(c.Threads.Actions.Mode), Allow: c.Threads.Actions.Allow, RequireSHA: c.Threads.Actions.RequireSHA}).Validate(); err != nil {
		t.Fatalf("policy: %v", err)
	}
}
