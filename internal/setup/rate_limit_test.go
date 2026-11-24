package setup

// rate_limit_test.go — the Configuration-page round trip for the [rate_limit]
// section (PLAN-V2 §M.7, docs/go/11_config_cli.md): the schema carries every
// key, the form posts the documented spellings, an untouched save is a no-op,
// an edit appears in RestartKeys (the buckets are built at boot — there is no
// hot reload), and a nonsense rate is refused with its config key named.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestRateLimitSchemaRoundTrip(t *testing.T) {
	base := roundTripConfig(t)
	base.RateLimit = config.RateLimit{
		Enabled: true, CollabWrite: "45/min", IssueOpen: "12/h",
		Report: "6/h", AuthFail: "20/min", TrustProxy: true,
	}
	groups := BuildSchema(base, config.Defaults())
	for _, key := range []string{
		"rate_limit.enabled", "rate_limit.collab_write", "rate_limit.issue_open",
		"rate_limit.report", "rate_limit.auth_fail", "rate_limit.trust_proxy",
	} {
		if _, err := findField(groups, key); err != nil {
			t.Fatalf("%s missing from the schema: %v", key, err)
		}
	}
	values := FormValues(groups)
	for key, want := range map[string]string{
		"rate_limit.enabled":      "true",
		"rate_limit.collab_write": "45/min",
		"rate_limit.issue_open":   "12/h",
		"rate_limit.report":       "6/h",
		"rate_limit.auth_fail":    "20/min",
		"rate_limit.trust_proxy":  "true",
	} {
		if got := values.Get(key); got != want {
			t.Fatalf("rendered %s = %q, want %q", key, got, want)
		}
	}

	// Untouched: the whole-schema save changes nothing and needs no restart.
	cand, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues: %v", err)
	}
	if cand.RateLimit != base.RateLimit {
		t.Fatalf("round trip changed the section: %+v", cand.RateLimit)
	}
	if cand.RateLimit.Enabled && cand.RateLimit.CollabWrite != "45/min" {
		t.Fatalf("collab_write lost: %q", cand.RateLimit.CollabWrite)
	}
	if keys := RestartKeys(base, cand); len(keys) != 0 {
		t.Fatalf("an untouched save reported restart keys %v", keys)
	}

	// An edit is restart-required, and says so per key.
	edited := url.Values{}
	for k, v := range values {
		edited[k] = v
	}
	edited.Set("rate_limit.collab_write", "60/min")
	edited.Set("rate_limit.trust_proxy", "false")
	cand, err = ParseValues(edited, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues edited: %v", err)
	}
	if cand.RateLimit.CollabWrite != "60/min" || cand.RateLimit.TrustProxy {
		t.Fatalf("edits not coerced: %+v", cand.RateLimit)
	}
	keys := strings.Join(RestartKeys(base, cand), ",")
	for _, want := range []string{"rate_limit.collab_write", "rate_limit.trust_proxy"} {
		if !strings.Contains(keys, want) {
			t.Fatalf("restart keys %q missing %s", keys, want)
		}
	}
}

// TestRateLimitBadSpellingNamesItsKey drives the page's failure path: a
// nonsense rate is a validation error carrying the row's key, never a banner
// with no cause (the coercion-failure contract, docs/go/11_config_cli.md §7).
func TestRateLimitBadSpellingNamesItsKey(t *testing.T) {
	base := roundTripConfig(t)
	values := FormValues(BuildSchema(base, config.Defaults()))
	values.Set("rate_limit.report", "5 per hour")
	cand, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues rejected the spelling before Validate: %v", err)
	}
	errs := Validate(cand)
	found := false
	for _, e := range errs {
		if e.Key == "rate_limit.report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate did not report rate_limit.report (got %v)", errs)
	}
}
