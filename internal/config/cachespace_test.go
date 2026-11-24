package config

import (
	"errors"
	"strings"
	"testing"
)

// TestAdaptCacheBudget pins BUG-6's safe default: a cache.dir on a filesystem
// smaller than the compiled budget gets its DEFAULT lowered to the eviction
// watermark (with a notice naming path, filesystem and sizes), while an
// explicitly configured cache.max_bytes is never changed.
func TestAdaptCacheBudget(t *testing.T) {
	old := statCacheDir
	t.Cleanup(func() { statCacheDir = old })

	total := int64(1 << 30) // 1 GiB "tmpfs"
	statCacheDir = func(string) (cacheFSSize, error) {
		return cacheFSSize{FSType: "tmpfs", Total: total}, nil
	}
	want := ByteSize(int64(float64(total)*0.9) / (1 << 20) * (1 << 20))

	c := Defaults()
	c.Cache.Dir = "/var/lib/spindle/cache"
	note := AdaptCacheBudget(c)
	if note == "" {
		t.Fatal("no notice for a default larger than the filesystem")
	}
	if c.Cache.MaxBytes != want {
		t.Fatalf("cache.max_bytes = %s, want %s", c.Cache.MaxBytes, want)
	}
	for _, s := range []string{c.Cache.Dir, "tmpfs", ByteSize(total).String(), Defaults().Cache.MaxBytes.String(), want.String(), "set cache.max_bytes explicitly"} {
		if !strings.Contains(note, s) {
			t.Errorf("notice %q missing %q", note, s)
		}
	}

	// An explicitly configured value is reported, never shrunk.
	c = Defaults()
	c.Cache.Dir = "/var/lib/spindle/cache"
	c.Cache.MaxBytes = 20 << 30
	c.markExplicit("cache.max_bytes")
	before := c.Cache.MaxBytes
	note = AdaptCacheBudget(c)
	if c.Cache.MaxBytes != before {
		t.Fatalf("explicit cache.max_bytes was shrunk: %s -> %s", before, c.Cache.MaxBytes)
	}
	for _, s := range []string{"exceeds", "tmpfs", c.Cache.Dir, "lower cache.max_bytes"} {
		if !strings.Contains(note, s) {
			t.Errorf("warning %q missing %q", note, s)
		}
	}

	// Fits, unknown filesystem, zero/empty config: silent no-ops.
	statCacheDir = func(string) (cacheFSSize, error) { return cacheFSSize{FSType: "ext4", Total: 1 << 40}, nil }
	c = Defaults()
	c.Cache.Dir = "/var/lib/spindle/cache"
	if note := AdaptCacheBudget(c); note != "" {
		t.Fatalf("fitting budget produced a notice: %q", note)
	}
	statCacheDir = func(string) (cacheFSSize, error) { return cacheFSSize{}, errors.New("statfs down") }
	if note := AdaptCacheBudget(c); note != "" {
		t.Fatalf("unknown filesystem produced a notice: %q", note)
	}
	if AdaptCacheBudget(nil) != "" {
		t.Fatal("nil config produced a notice")
	}
	c = Defaults()
	c.Cache.Dir = ""
	if AdaptCacheBudget(c) != "" {
		t.Fatal("empty cache.dir produced a notice")
	}
}

// TestExplicitProvenance pins that file/env provenance drives the clamp: a
// file that sets only cache.dir leaves cache.max_bytes at the default (clamp
// eligible), while a file or env value marks it configured.
func TestExplicitProvenance(t *testing.T) {
	dir := tmpDataDir(t)
	env := func(key string) string {
		if key == "SPINDLE_DATA_DIR" {
			return dir
		}
		return ""
	}
	path := writeConfig(t, dir, ConfigFileName, withAuth("[cache]\ndir = \"/tmp/spindle-cache\"\n"))
	c, _, err := Load([]string{path}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Explicit("cache.dir") {
		t.Fatal("cache.dir set by the file not recorded")
	}
	if c.Explicit("cache.max_bytes") {
		t.Fatal("cache.max_bytes not in the file but recorded as explicit")
	}

	t.Setenv("SPINDLE__CACHE__MAX_BYTES", `"1GiB"`)
	c, _, err = Load([]string{path}, env)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if !c.Explicit("cache.max_bytes") {
		t.Fatal("env-set cache.max_bytes not recorded")
	}
	if int64(c.Cache.MaxBytes) != 1<<30 {
		t.Fatalf("cache.max_bytes = %d", int64(c.Cache.MaxBytes))
	}
}

// TestAdaptCacheBudgetTinyFS covers the degenerate filesystems: an unusable
// total and a total so small the rounded limit is zero both stay silent.
func TestAdaptCacheBudgetTinyFS(t *testing.T) {
	old := statCacheDir
	t.Cleanup(func() { statCacheDir = old })
	c := Defaults()
	c.Cache.Dir = "/var/lib/spindle/cache"

	statCacheDir = func(string) (cacheFSSize, error) { return cacheFSSize{Total: 0}, nil }
	if note := AdaptCacheBudget(c); note != "" {
		t.Fatalf("zero-total filesystem produced a notice: %q", note)
	}
	statCacheDir = func(string) (cacheFSSize, error) { return cacheFSSize{Total: 10}, nil }
	if note := AdaptCacheBudget(c); note != "" {
		t.Fatalf("tiny filesystem produced a notice: %q", note)
	}
	if c.Cache.MaxBytes != Defaults().Cache.MaxBytes {
		t.Fatalf("tiny filesystem changed the budget: %s", c.Cache.MaxBytes)
	}

	// Unknown filesystem type still gets a correct message.
	statCacheDir = func(string) (cacheFSSize, error) { return cacheFSSize{Total: 1 << 30}, nil }
	note := AdaptCacheBudget(c)
	if !strings.Contains(note, "the filesystem") {
		t.Fatalf("unknown-fs notice = %q", note)
	}
}
