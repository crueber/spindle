package config

import (
	"strings"
	"testing"
	"time"
)

// TestPackagesDefaults pins the docs/features/14 §5 defaults.
func TestPackagesDefaults(t *testing.T) {
	c := testBaseConfig()
	if c.Packages.MaxBlobBytes != 10<<30 {
		t.Fatalf("max_blob_bytes = %d", c.Packages.MaxBlobBytes)
	}
	if c.Packages.MaxManifestBytes != 4<<20 {
		t.Fatalf("max_manifest_bytes = %d", c.Packages.MaxManifestBytes)
	}
	if time.Duration(c.Packages.UploadTTL) != 24*time.Hour {
		t.Fatalf("upload_ttl = %v", time.Duration(c.Packages.UploadTTL))
	}
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
}

// TestValidatePackages rejects non-positive limits (a silent default swap is
// exactly what the setup page would then display wrong).
func TestValidatePackages(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"zero blob", func(c *Config) { c.Packages.MaxBlobBytes = 0 }, "packages.max_blob_bytes"},
		{"negative manifest", func(c *Config) { c.Packages.MaxManifestBytes = -1 }, "packages.max_manifest_bytes"},
		{"zero ttl", func(c *Config) { c.Packages.UploadTTL = 0 }, "packages.upload_ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mut(c)
			_, errs := Validate(c)
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors %v missing %q", errs, tc.want)
			}
		})
	}
}

// TestRepoFeaturesPackages: the [features] packages key resolves like its
// siblings (absent = on, explicit false = off) and rides the ETag bits.
func TestRepoFeaturesPackages(t *testing.T) {
	off := false
	on := true
	rs, err := ParseRepoSettings([]byte("[features]\npackages = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rs.Features.Resolve().Packages {
		t.Fatal("packages=false must disable")
	}
	if !AllFeatures().Packages {
		t.Fatal("default must be enabled")
	}
	if got := (&RepoFeatures{Packages: &on}).Resolve().Packages; !got {
		t.Fatal("explicit true must enable")
	}
	if got := (&RepoFeatures{Packages: &off}).Resolve(); got.Packages {
		t.Fatal("pointer false must disable")
	}
	if bits := (ResolvedFeatures{}).ETagBits(); len(bits) != 7 || bits != "0000000" {
		t.Fatalf("bits = %q", bits)
	}
	if bits := AllFeatures().ETagBits(); bits != "1111111" {
		t.Fatalf("all-on bits = %q", bits)
	}
	// An explicit off flips only the seventh bit.
	f := AllFeatures()
	f.Packages = false
	if bits := f.ETagBits(); bits != "1111110" {
		t.Fatalf("packages-off bits = %q", bits)
	}
}

// TestPackagesQuotaDefaultAndValidation: the instance default quota is
// unlimited (0) — an upgrade must not start refusing pushes — and a negative
// quota is a validation error, never a silently substituted default.
func TestPackagesQuotaDefaultAndValidation(t *testing.T) {
	c := testBaseConfig()
	if c.Packages.QuotaBytes != 0 {
		t.Fatalf("quota_bytes default = %d, want 0 (unlimited)", c.Packages.QuotaBytes)
	}
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	c.Packages.QuotaBytes = -1
	_, errs := Validate(c)
	found := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "packages.quota_bytes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors %v missing packages.quota_bytes", errs)
	}
}

// TestRepoPackagesPolicy: the [packages] repo section parses, validates and
// resolves over the instance default (nil = inherit; explicit 0 = unlimited),
// and the retention rules keep their spelling through a TOML round trip.
func TestRepoPackagesPolicy(t *testing.T) {
	defaults := Packages{QuotaBytes: 50 << 20}
	rs, err := ParseRepoSettings([]byte(`
description = "images"
[packages]
quota_bytes = "5MiB"
[[packages.retention]]
pattern = "dev-*"
keep_last = 3
[[packages.retention]]
max_age = "168h"
`))
	if err != nil {
		t.Fatal(err)
	}
	pol := rs.PackagesPolicy(defaults)
	if pol.QuotaBytes != 5<<20 {
		t.Fatalf("quota = %d", pol.QuotaBytes)
	}
	if len(pol.Retention) != 2 || pol.Retention[0].KeepLast != 3 ||
		time.Duration(pol.Retention[1].MaxAge) != 168*time.Hour {
		t.Fatalf("retention = %+v", pol.Retention)
	}
	if !pol.Retention[0].Match("dev-1") || pol.Retention[0].Match("v1") {
		t.Fatal("pattern matching")
	}
	if !pol.Retention[1].Match("anything") {
		t.Fatal("an empty pattern must match every tag")
	}
	if rs.Packages.QuotaBytes.String() != "5MiB" {
		t.Fatalf("quota spelling = %s", rs.Packages.QuotaBytes.String())
	}

	// Unset inherits the instance default; explicit zero is unlimited for the
	// repository (the pointer discipline).
	if got := PackagesPolicyOf([]byte(""), defaults); got.QuotaBytes != 50<<20 {
		t.Fatalf("absent section = %d", got.QuotaBytes)
	}
	zero, err := ParseRepoSettings([]byte("[packages]\nquota_bytes = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := zero.PackagesPolicy(defaults); got.QuotaBytes != 0 {
		t.Fatalf("explicit zero = %d, want unlimited", got.QuotaBytes)
	}
	// An unreadable body resolves to the default, never to a made-up quota.
	if got := PackagesPolicyOf([]byte("not toml ="), defaults); got.QuotaBytes != 50<<20 {
		t.Fatalf("unreadable body = %d", got.QuotaBytes)
	}
	// The section never merges into the host config.
	merged, err := rs.Merge(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if merged.Packages.QuotaBytes != 0 {
		t.Fatalf("[packages] merged into the host config: %d", merged.Packages.QuotaBytes)
	}
}

// TestRepoPackagesValidationRejectsBrokenPolicies closes the fail-closed side:
// a policy that can never keep anything, a negative bound, a bad glob and a
// rule flood are publish-time errors.
func TestRepoPackagesValidationRejectsBrokenPolicies(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"negative quota", "[packages]\nquota_bytes = -1\n", "quota_bytes"},
		{"negative keep", "[packages]\n[[packages.retention]]\nkeep_last = -1\n", "keep_last"},
		{"negative age", "[packages]\n[[packages.retention]]\nmax_age = \"-1h\"\n", "max_age"},
		{"empty rule", "[packages]\n[[packages.retention]]\npattern = \"x\"\n", "neither keep_last nor max_age"},
		{"bad glob", "[packages]\n[[packages.retention]]\npattern = \"[\"\nkeep_last = 1\n", "not a glob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRepoSettings([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	rules := strings.Builder{}
	rules.WriteString("[packages]\n")
	for i := 0; i < MaxPackageRetentionRules+1; i++ {
		rules.WriteString("[[packages.retention]]\nkeep_last = 1\n")
	}
	if _, err := ParseRepoSettings([]byte(rules.String())); err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("rule flood error = %v", err)
	}
	// An unknown key inside the section is still rejected.
	if _, err := ParseRepoSettings([]byte("[packages]\nquota_bytes = \"1KiB\"\nnope = 1\n")); err == nil {
		t.Fatal("unknown [packages] key accepted")
	}
}
