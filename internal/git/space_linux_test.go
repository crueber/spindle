//go:build linux

package git

import "testing"

// TestCacheFilesystemLinux pins the statfs helper and the magic-number names
// used in the out-of-space diagnosis.
func TestCacheFilesystemLinux(t *testing.T) {
	fs, err := cacheFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("cacheFilesystem: %v", err)
	}
	if fs.Total <= 0 || fs.Avail < 0 || fs.Avail > fs.Total {
		t.Fatalf("statfs = %+v", fs)
	}
	if _, err := cacheFilesystem(""); err == nil {
		t.Fatal("empty path statfs succeeded")
	}
	for magic, want := range map[uint32]string{
		magicTmpfs:   "tmpfs",
		magicOverlay: "overlayfs",
		magicExt4:    "ext4",
		magicXFS:     "xfs",
		magicBtrfs:   "btrfs",
		magicZFS:     "zfs",
		magicNFS:     "nfs",
		magicFUSE:    "fuse",
		magicCIFS:    "cifs",
		magicF2FS:    "f2fs",
		0xdeadbeef:   "",
	} {
		if got := fsTypeName(magic); got != want {
			t.Errorf("fsTypeName(%#x) = %q, want %q", magic, got, want)
		}
	}
}
