package config

// rate.go — the [rate_limit] rate spellings (PLAN-V2 §M.7, docs/features/16
// §12). One parser for the whole surface: config.Validate uses it to refuse a
// nonsense value with its key named, the setup schema's coercion path validates
// through the same Validate, and internal/server parses the limiter's buckets
// with it at boot. There is no second spelling anywhere.

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// RateSpec is one parsed limit: Limit tokens per Window, refilled continuously
// (a token bucket of capacity Limit).
type RateSpec struct {
	Limit  int
	Window time.Duration
}

// PerSecond is the refill rate, in tokens per second.
func (r RateSpec) PerSecond() float64 {
	if r.Window <= 0 {
		return 0
	}
	return float64(r.Limit) / r.Window.Seconds()
}

// rateSpecRe is the accepted spelling: a positive integer, a slash, and one of
// the three window units. Deliberately strict — "30 per minute", "30/m" and
// "0/min" are all refused with the key named, because a rate the operator
// believes is active but the loader ignored is worse than a boot error.
var rateSpecRe = regexp.MustCompile(`^([1-9][0-9]{0,9})/(s|min|h)$`)

// ParseRate parses one rate spelling ("30/min", "10/h", "5/s").
func ParseRate(spec string) (RateSpec, error) {
	m := rateSpecRe.FindStringSubmatch(spec)
	if m == nil {
		return RateSpec{}, fmt.Errorf("%q is not a rate (accepted N/s, N/min or N/h with N > 0, e.g. 30/min)", spec)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return RateSpec{}, fmt.Errorf("%q is not a rate (N must be a positive integer)", spec)
	}
	var window time.Duration
	switch m[2] {
	case "s":
		window = time.Second
	case "min":
		window = time.Minute
	case "h":
		window = time.Hour
	}
	return RateSpec{Limit: n, Window: window}, nil
}

// Rates is a parsed [rate_limit] section. The field names are the config keys'
// (the limiter classes mirror them one-to-one), so a class can never be added
// to the limiter without a key to configure it.
type Rates struct {
	CollabWrite RateSpec
	IssueOpen   RateSpec
	Report      RateSpec
	AuthFail    RateSpec
}

// ParseRates parses every rate key. The first bad value is reported with its
// config key, so the settings page can mark the row (the coercion-failure
// convention): "rate_limit.collab_write: …".
func (r RateLimit) ParseRates() (Rates, error) {
	var out Rates
	for _, row := range []struct {
		key  string
		spec string
		dst  *RateSpec
	}{
		{"rate_limit.collab_write", r.CollabWrite, &out.CollabWrite},
		{"rate_limit.issue_open", r.IssueOpen, &out.IssueOpen},
		{"rate_limit.report", r.Report, &out.Report},
		{"rate_limit.auth_fail", r.AuthFail, &out.AuthFail},
	} {
		spec, err := ParseRate(row.spec)
		if err != nil {
			return Rates{}, fmt.Errorf("%s: %v", row.key, err)
		}
		*row.dst = spec
	}
	return out, nil
}
