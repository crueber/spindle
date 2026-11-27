package config

// threads_knobs_test.go — the operator knobs (PAR-179): defaults, the accepted
// spellings and the named bounds the Configuration page marks its rows with.
// The numbers are the documented limits (docs/threads.md §9.2), not arbitrary.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestThreadsKnobDefaults(t *testing.T) {
	c := Defaults()
	if c.Threads.Retention != Duration(30*24*time.Hour) {
		t.Fatalf("retention default = %s", time.Duration(c.Threads.Retention))
	}
	if c.Threads.ArtifactRetention != 0 {
		t.Fatalf("artifact_retention default = %s, want 0 (= the run window)", time.Duration(c.Threads.ArtifactRetention))
	}
	if c.Threads.KeepForever {
		t.Fatal("keep_forever defaults on")
	}
	if c.Threads.MaxQueueAttempts != 3 {
		t.Fatalf("max_queue_attempts default = %d, want 3", c.Threads.MaxQueueAttempts)
	}
	if c.Threads.JobLease != Duration(15*time.Minute) {
		t.Fatalf("job_lease default = %s, want 15m", time.Duration(c.Threads.JobLease))
	}
	if _, errs := Validate(validOIDC()); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
}

func TestValidateThreadsKnobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string // error fragment; "" = valid
	}{
		{"90-day run window", func(c *Config) { c.Threads.Retention = Duration(90 * 24 * time.Hour) }, ""},
		{"one-day run window", func(c *Config) { c.Threads.Retention = Duration(24 * time.Hour) }, ""},
		{"one-year run window", func(c *Config) { c.Threads.Retention = Duration(365 * 24 * time.Hour) }, ""},
		{"sub-day run window", func(c *Config) { c.Threads.Retention = Duration(time.Hour) }, "threads.retention must be between"},
		{"sub-day run window names keep_forever", func(c *Config) { c.Threads.Retention = Duration(2 * time.Hour) }, "keep_forever"},
		{"over a year", func(c *Config) { c.Threads.Retention = Duration(366 * 24 * time.Hour) }, "threads.retention must be between"},
		{"negative", func(c *Config) { c.Threads.Retention = Duration(-time.Hour) }, "must not be negative"},
		{"90-day artifact window", func(c *Config) { c.Threads.ArtifactRetention = Duration(90 * 24 * time.Hour) }, ""},
		{"hour artifact window", func(c *Config) { c.Threads.ArtifactRetention = Duration(time.Hour) }, "threads.artifact_retention must be between"},
		{"one attempt", func(c *Config) { c.Threads.MaxQueueAttempts = 1 }, ""},
		{"twenty attempts", func(c *Config) { c.Threads.MaxQueueAttempts = 20 }, ""},
		{"zero attempts keeps the default", func(c *Config) { c.Threads.MaxQueueAttempts = 0 }, ""},
		{"twenty-one attempts", func(c *Config) { c.Threads.MaxQueueAttempts = 21 }, "threads.max_queue_attempts"},
		{"negative attempts", func(c *Config) { c.Threads.MaxQueueAttempts = -1 }, "threads.max_queue_attempts"},
		{"five-minute lease", func(c *Config) { c.Threads.JobLease = Duration(5 * time.Minute) }, ""},
		{"thirty-minute lease", func(c *Config) { c.Threads.JobLease = Duration(30 * time.Minute) }, ""},
		{"four-minute lease", func(c *Config) { c.Threads.JobLease = Duration(4 * time.Minute) }, "threads.job_lease"},
		{"thirty-one-minute lease", func(c *Config) { c.Threads.JobLease = Duration(31 * time.Minute) }, "threads.job_lease"},
		{"keep forever", func(c *Config) { c.Threads.KeepForever = true }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validOIDC()
			tc.mutate(c)
			_, errs := Validate(c)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("must validate: %v", errs)
				}
				return
			}
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %v, want a fragment %q", errs, tc.want)
			}
		})
	}
}

// TestThreadsKnobsTOMLRoundTrip pins the file spellings the sample in
// threads.md uses: durations and a boolean through the real decoder.
func TestThreadsKnobsTOMLRoundTrip(t *testing.T) {
	src := `
[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "id"
oauth_client_secret = "secret"
session_secret = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

[threads]
retention = "720h"
artifact_retention = "2160h"
keep_forever = true
max_queue_attempts = 7
job_lease = "20m"
`
	path := t.TempDir() + "/spindle.toml"
	if werr := os.WriteFile(path, []byte(src), 0o600); werr != nil {
		t.Fatal(werr)
	}
	c, _, err := LoadExplicit(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if time.Duration(c.Threads.ArtifactRetention) != 90*24*time.Hour ||
		!c.Threads.KeepForever || c.Threads.MaxQueueAttempts != 7 ||
		time.Duration(c.Threads.JobLease) != 20*time.Minute {
		t.Fatalf("decoded = %+v", c.Threads)
	}
}

// TestThreadsSMTP (PAR-197/D-27): email is off by default, and a configured
// mailer is validated by name — a typo cannot look enabled while never
// delivering.
func TestThreadsSMTP(t *testing.T) {
	c := Defaults()
	if c.Threads.SMTP.Host != "" {
		t.Fatalf("smtp defaults on: %+v", c.Threads.SMTP)
	}
	if got := time.Duration(c.Threads.SMTP.Timeout); got != 10*time.Second {
		t.Fatalf("smtp timeout default = %s", got)
	}
	if _, errs := Validate(validOIDC()); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string // error fragment; "" = valid
	}{
		{"off", func(c *Config) {}, ""},
		{"configured starttls", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "ci@example.com", TLS: "starttls"}
		}, ""},
		{"configured implicit", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "Spindle <ci@example.com>", TLS: "implicit", Username: "ci", Password: "x"}
		}, ""},
		{"configured none", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "127.0.0.1", From: "ci@example.com", TLS: "none"}
		}, ""},
		{"unknown tls", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "ci@example.com", TLS: "ssl"}
		}, "threads.smtp.tls"},
		{"host without from", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com"}
		}, "threads.smtp.from"},
		{"bad from", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "not-an-address"}
		}, "threads.smtp.from"},
		{"from without host", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{From: "ci@example.com"}
		}, "threads.smtp.host"},
		{"credential without tls", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "ci@example.com", TLS: "none", Username: "ci", Password: "x"}
		}, "credentials are never sent"},
		{"username without password", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "ci@example.com", TLS: "starttls", Username: "ci"}
		}, "must be set together"},
		{"port out of range", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp.example.com", From: "ci@example.com", Port: 70000}
		}, "threads.smtp.port"},
		{"host with scheme", func(c *Config) {
			c.Threads.SMTP = ThreadsSMTP{Host: "smtp://example.com", From: "ci@example.com"}
		}, "bare hostname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validOIDC()
			tc.mutate(c)
			_, errs := Validate(c)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("must validate: %v", errs)
				}
				return
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					return
				}
			}
			t.Fatalf("errors %v miss %q", errs, tc.want)
		})
	}
}
