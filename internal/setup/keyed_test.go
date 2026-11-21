package setup

// keyed_test.go — the round-3 audit fix: a coercion failure names the key it
// came from, exactly like a config.Validate rejection. That key is what the
// /setup page uses to mark the offending row and open a collapsed Advanced
// group, so it is asserted here per field type and then rendered in
// internal/webui (TestSettingsSetupCoercionErrorMarksItsRow).

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

// TestCoerceFailuresCarryTheirKey walks every field type the schema can
// render with a value its parser rejects and asserts the failure is keyed.
// String values have no coercion failure mode (any text is a valid string), so
// the string row rides the validator stage that gates the same save
// (store.backend is an enum string); the assertion is the same: the error
// carries the key.
func TestCoerceFailuresCarryTheirKey(t *testing.T) {
	cases := []struct {
		name, field, value, want string
		// validate marks rows whose rejection comes from config.Validate
		// (only string/enum values, which the coercion layer cannot reject).
		validate bool
	}{
		{"string/enum", "store.backend", "bogus", "store.backend", true},
		{"size", "cache.max_bytes", "eleventy", "cache.max_bytes", false},
		{"duration", "cache.evict_idle_after", "bogus-duration", "cache.evict_idle_after", false},
		{"int", "server.max_concurrent_requests", "many", "server.max_concurrent_requests", false},
		{"uint", "wal.cas_max_retries", "-1", "wal.cas_max_retries", false},
		{"uint64", "wal.snapshot_every_entries", "1.5", "wal.snapshot_every_entries", false},
		{"float", "cache.disk_high_watermark", "high", "cache.disk_high_watermark", false},
		{"bool", "server.http2", "yes", "server.http2", false},
		// A struct slice (list) posts a TOML fragment; a non-fragment is the
		// list's own coercion failure.
		{"list", "bundles.strategy", "not a toml fragment = [", "bundles.strategy", false},
		// A struct-slice CELL rides the same Coerce path and names the cell.
		{"struct-slice cell", "bundles.strategy#0.keep", "many", "bundles.strategy[0].keep", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := config.Defaults()
			base.Bundles.Strategy = []config.BundleStrategy{{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0"}}
			c, err := ParseValues(url.Values{tc.field: {tc.value}}, base)
			if tc.validate {
				if err != nil {
					t.Fatalf("ParseValues rejected a string value: %v", err)
				}
				errs := Validate(c)
				if len(errs) == 0 {
					t.Fatalf("no validation error for %s=%q", tc.field, tc.value)
				}
				for _, e := range errs {
					if e.Key == "" {
						t.Errorf("keyless validation error: %q", e.Message)
					}
					if e.Key == tc.want {
						return
					}
				}
				t.Fatalf("validation errors %+v name no row for %s", errs, tc.field)
			}
			if err == nil {
				t.Fatalf("ParseValues accepted %s=%q", tc.field, tc.value)
			}
			if got := KeyOf(err); got != tc.want {
				t.Fatalf("%s=%q: KeyOf = %q, want %q (full error %q)", tc.field, tc.value, got, tc.want, err)
			}
			// The message keeps the key spelling (the cell spelling for a cell).
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q lost the key %q", err.Error(), tc.want)
			}
		})
	}
}

// TestUnknownSetupKeyCannotPassSilently is the negative control: a key the
// schema does not know fails, names itself, and changes nothing.
func TestUnknownSetupKeyCannotPassSilently(t *testing.T) {
	base := config.Defaults()
	base.Server.Listen = "127.0.0.1:8080"
	c, err := ParseValues(url.Values{"server.bogus_setting": {"1"}}, base)
	if err == nil {
		t.Fatal("an unknown key was accepted silently")
	}
	if c != nil {
		t.Fatalf("an unknown key returned a config: %+v", c)
	}
	if got := KeyOf(err); got != "server.bogus_setting" {
		t.Fatalf("KeyOf(%v) = %q, want the submitted key", err, got)
	}
	// Nested and top-level unknowns are keyed too (never the bare message).
	for _, key := range []string{"server.auth.nope", "nosuchsection.nope"} {
		if _, err := ParseValues(url.Values{key: {"1"}}, base); err == nil || KeyOf(err) != key {
			t.Fatalf("%s: err=%v key=%q", key, err, KeyOf(err))
		}
	}
}
