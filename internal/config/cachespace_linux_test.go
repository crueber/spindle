//go:build linux

package config

import (
	"strings"
	"testing"
)

// TestCacheFilesystemStats pins the Linux statfs helper: a real directory
// reports a positive capacity, an empty path fails, and the tmpfs/overlay
// names the boot notice uses are spelled.
func TestCacheFilesystemStats(t *testing.T) {
	fs, err := cacheFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("cacheFilesystem: %v", err)
	}
	if fs.Total <= 0 {
		t.Fatalf("total = %d", fs.Total)
	}
	if _, err := cacheFilesystem(""); err == nil {
		t.Fatal("empty path statfs succeeded")
	}
	if _, err := cacheFilesystem("/nonexistent/spindle-cache-dir"); err == nil {
		t.Fatal("missing path statfs succeeded")
	}
	for magic, want := range map[uint32]string{
		cfgMagicTmpfs:   "tmpfs",
		cfgMagicOverlay: "overlayfs",
		cfgMagicExt4:    "ext4",
		cfgMagicXFS:     "xfs",
		cfgMagicBtrfs:   "btrfs",
		cfgMagicZFS:     "zfs",
		cfgMagicNFS:     "nfs",
		cfgMagicFUSE:    "fuse",
		cfgMagicCIFS:    "cifs",
		cfgMagicF2FS:    "f2fs",
		0xdeadbeef:      "",
	} {
		if got := cfgFSTypeName(magic); got != want {
			t.Errorf("cfgFSTypeName(%#x) = %q, want %q", magic, got, want)
		}
	}
	if !strings.Contains(strings.ToLower(cfgFSTypeName(cfgMagicTmpfs)), "tmpfs") {
		t.Fatal("tmpfs naming")
	}
}

// TestCacheSpace pins the read-only page projection (Administration →
// Maintenance): a real directory reports capacity + available bytes, and an
// unusable path is honestly unavailable rather than a fake zero.
func TestCacheSpace(t *testing.T) {
	info, ok := CacheSpace(t.TempDir())
	if !ok {
		t.Fatal("CacheSpace(tempdir) unavailable")
	}
	if info.Total <= 0 || info.Avail < 0 || info.Avail > info.Total {
		t.Fatalf("CacheSpace = %+v", info)
	}
	if _, ok := CacheSpace(""); ok {
		t.Fatal("CacheSpace(\"\") reported a filesystem")
	}
	if _, ok := CacheSpace("/nonexistent/spindle-cache-dir"); ok {
		t.Fatal("CacheSpace(missing) reported a filesystem")
	}
}
