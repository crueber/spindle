package config

// search_test.go — the additive [search] section (docs/features/20_search.md
// §5.2/§11, docs/go/11_config_cli.md §2): two operator-only keys, a *bool whose
// absent value means enabled, a zero max_concurrent that means the default, and
// a bounded ceiling that names its key.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchDefaults(t *testing.T) {
	s := Defaults().Search
	if s.Enabled == nil {
		t.Fatal("compiled default must be a set pointer: the page renders the switch from it")
	}
	if !s.IsEnabled() {
		t.Fatal("search must be enabled by default (it is a read of the serving copy)")
	}
	if s.MaxConcurrent != 2 {
		t.Fatalf("max_concurrent = %d, want 2", s.MaxConcurrent)
	}
	// The zero section is enabled too: nil means enabled, never disabled.
	if !(Search{}).IsEnabled() {
		t.Fatal("a nil enabled pointer must mean enabled")
	}
	if (Search{Enabled: Bool(false)}).IsEnabled() {
		t.Fatal("an explicit false must mean disabled")
	}
}

func TestSearchValidate(t *testing.T) {
	good := func() *Config {
		c := testBaseConfig()
		c.Search = Defaults().Search
		return c
	}
	if _, errs := Validate(good()); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	// 0 = the default, accepted as written (the loader does not fabricate a
	// value the operator cannot see).
	zero := good()
	zero.Search.MaxConcurrent = 0
	if _, errs := Validate(zero); len(errs) != 0 {
		t.Fatalf("zero must be accepted: %v", errs)
	}
	for _, n := range []int{1, SearchMaxConcurrent} {
		c := good()
		c.Search.MaxConcurrent = n
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("max_concurrent %d must validate: %v", n, errs)
		}
	}
	for _, n := range []int{-1, SearchMaxConcurrent + 1} {
		c := good()
		c.Search.MaxConcurrent = n
		_, errs := Validate(c)
		found := false
		for _, err := range errs {
			if strings.Contains(err.Error(), "search.max_concurrent") {
				found = true
			}
		}
		if !found {
			t.Fatalf("max_concurrent %d was accepted: %v", n, errs)
		}
	}
}

func TestSearchTOMLDecode(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		t.Helper()
		path := filepath.Join(dir, "spindle.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// An explicit false (and a non-default concurrency) decodes.
	c := Defaults()
	if err := decodeFile(write("[search]\nenabled = false\nmax_concurrent = 4\n"), c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Search.IsEnabled() {
		t.Fatal("enabled = false did not decode")
	}
	if c.Search.MaxConcurrent != 4 {
		t.Fatalf("max_concurrent = %d, want 4", c.Search.MaxConcurrent)
	}
	// An absent section keeps the compiled default.
	c = Defaults()
	if err := decodeFile(write("[server]\nmax_tree_log = 10\n"), c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.Search.IsEnabled() || c.Search.MaxConcurrent != 2 {
		t.Fatalf("absent section changed the defaults: %+v", c.Search)
	}
}

// TestSearchEnvOverlay pins the exact dotted path: a missing segment is
// silently recorded as an unknown key, which is how env overlays have gone
// wrong before (AGENTS.md field lessons).
func TestSearchEnvOverlay(t *testing.T) {
	env := map[string]string{
		"SPINDLE__SEARCH__ENABLED":        "false",
		"SPINDLE__SEARCH__MAX_CONCURRENT": "3",
	}
	c := Defaults()
	overrides, err := applyEnv(c, func(k string) string { return env[k] }, envList(env))
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("ignored overrides = %+v, want none", overrides)
	}
	if c.Search.IsEnabled() {
		t.Fatal("SPINDLE__SEARCH__ENABLED=false was ignored")
	}
	if c.Search.MaxConcurrent != 3 {
		t.Fatalf("max_concurrent = %d, want 3", c.Search.MaxConcurrent)
	}
}

func TestSearchMarshalKeepsAbsentNil(t *testing.T) {
	// A nil *bool writes no key (an unset operator knob is not invented);
	// a set one writes the explicit value.
	c := Defaults()
	c.Search.Enabled = nil
	body, err := MarshalTOML(c)
	if err != nil {
		t.Fatal(err)
	}
	section := string(body[strings.Index(string(body), "[search]"):])
	if end := strings.Index(section, "\n["); end >= 0 {
		section = section[:end]
	}
	if strings.Contains(section, "enabled") {
		t.Fatalf("a nil enabled was marshaled:\n%s", section)
	}
	if !strings.Contains(section, "max_concurrent = 2") {
		t.Fatalf("max_concurrent missing:\n%s", section)
	}
	c.Search.Enabled = Bool(false)
	body, err = MarshalTOML(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "enabled = false") {
		t.Fatalf("an explicit false was not marshaled:\n%s", body)
	}
}
