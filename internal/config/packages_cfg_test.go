package config

import (
	"strings"
	"testing"
	"time"
)

// TestPackagesDefaults pins the docs/features/14 §5 defaults.
func TestPackagesDefaults(t *testing.T) {
	c := Defaults()
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
