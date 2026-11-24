package config

// rate_test.go — the [rate_limit] spellings (PLAN-V2 §M.7): parse the three
// accepted units, refuse everything else with the config key named, and pin
// the defaults the plan fixes (off, 30/min, 10/h, 5/h, 10/min, trust_proxy
// false).

import (
	"strings"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		spec   string
		limit  int
		window time.Duration
	}{
		{"30/min", 30, time.Minute},
		{"10/h", 10, time.Hour},
		{"5/s", 5, time.Second},
		{"1/h", 1, time.Hour},
		{"1000000000/h", 1000000000, time.Hour},
	}
	for _, tc := range cases {
		got, err := ParseRate(tc.spec)
		if err != nil {
			t.Fatalf("ParseRate(%q): %v", tc.spec, err)
		}
		if got.Limit != tc.limit || got.Window != tc.window {
			t.Fatalf("ParseRate(%q) = %d/%s, want %d/%s", tc.spec, got.Limit, got.Window, tc.limit, tc.window)
		}
	}
	for _, bad := range []string{
		"", "30", "30/m", "30 per minute", "0/min", "-5/min", "30/", "/min",
		"30/min ", " 30/min", "1.5/min", "30/d", "30/sec", "999999999999/min",
	} {
		if _, err := ParseRate(bad); err == nil {
			t.Fatalf("ParseRate(%q) accepted a nonsense spelling", bad)
		}
	}
	if got := (RateSpec{Limit: 30, Window: time.Minute}).PerSecond(); got != 0.5 {
		t.Fatalf("PerSecond = %v, want 0.5", got)
	}
	if got := (RateSpec{}).PerSecond(); got != 0 {
		t.Fatalf("zero PerSecond = %v", got)
	}
}

// TestRateLimitEnvOverlay pins the dotted path: SPINDLE__RATE_LIMIT__* must
// reach the section (a missing segment is recorded as an unknown key and
// silently ignored — the field lesson in AGENTS.md §2).
func TestRateLimitEnvOverlay(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil,
		"SPINDLE__RATE_LIMIT__ENABLED=true",
		"SPINDLE__RATE_LIMIT__COLLAB_WRITE=12/min",
		"SPINDLE__RATE_LIMIT__TRUST_PROXY=true",
	)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if !c.RateLimit.Enabled || c.RateLimit.CollabWrite != "12/min" || !c.RateLimit.TrustProxy {
		t.Fatalf("env overlay missed the section: %+v", c.RateLimit)
	}
	if len(overrides) != 0 {
		t.Fatalf("unexpected ignored overrides: %+v", overrides)
	}
	if !c.Explicit("rate_limit.collab_write") {
		t.Fatal("rate_limit.collab_write not marked explicit")
	}
}

// TestRateLimitDefaultsAndParse pins the plan's defaults.
func TestRateLimitDefaultsAndParse(t *testing.T) {
	rl := Defaults().RateLimit
	if rl.Enabled {
		t.Fatal("rate_limit.enabled defaults on; a zero-config boot must not change behaviour (L10)")
	}
	if rl.TrustProxy {
		t.Fatal("rate_limit.trust_proxy defaults on; X-Forwarded-For is untrusted by default")
	}
	rates, err := rl.ParseRates()
	if err != nil {
		t.Fatalf("defaults do not parse: %v", err)
	}
	for _, c := range []struct {
		got  RateSpec
		want string
	}{
		{rates.CollabWrite, "30/min"},
		{rates.IssueOpen, "10/h"},
		{rates.Report, "5/h"},
		{rates.AuthFail, "10/min"},
	} {
		want, _ := ParseRate(c.want)
		if c.got != want {
			t.Fatalf("rate = %+v, want %+v (%s)", c.got, want, c.want)
		}
	}
}

// TestRateLimitErrorsNameTheirKey is the settings-page contract: a bad value
// yields an error carrying the exact config key, so the row can be marked.
func TestRateLimitErrorsNameTheirKey(t *testing.T) {
	for _, key := range []string{"collab_write", "issue_open", "report", "auth_fail"} {
		c := Defaults()
		switch key {
		case "collab_write":
			c.RateLimit.CollabWrite = "nonsense"
		case "issue_open":
			c.RateLimit.IssueOpen = "0/h"
		case "report":
			c.RateLimit.Report = "5/day"
		case "auth_fail":
			c.RateLimit.AuthFail = ""
		}
		_, errs := Validate(c)
		found := false
		for _, err := range errs {
			if strings.Contains(err.Error(), "rate_limit."+key) {
				found = true
			}
		}
		if !found {
			t.Fatalf("rate_limit.%s: no error names the key (got %v)", key, errs)
		}
	}
	// A valid section validates clean (the other always-on rules need the
	// OIDC set; Validate's auth rule is out of scope here).
	c := Defaults()
	c.RateLimit.Enabled = true
	if _, err := c.RateLimit.ParseRates(); err != nil {
		t.Fatalf("valid section rejected: %v", err)
	}
}
