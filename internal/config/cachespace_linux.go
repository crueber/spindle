//go:build linux

package config

import (
	"errors"
	"syscall"
)

// Linux filesystem magic numbers worth naming at boot (include/uapi/linux/magic.h).
const (
	cfgMagicTmpfs   = 0x01021994
	cfgMagicOverlay = 0x794c7630
	cfgMagicExt4    = 0xef53
	cfgMagicXFS     = 0x58465342
	cfgMagicBtrfs   = 0x9123683e
	cfgMagicZFS     = 0x2fc12fc1
	cfgMagicNFS     = 0x6969
	cfgMagicFUSE    = 0x65735546
	cfgMagicCIFS    = 0xff534d42
	cfgMagicF2FS    = 0xf2f52010
)

// cacheFilesystem stats the filesystem holding dir.
func cacheFilesystem(dir string) (cacheFSSize, error) {
	if dir == "" {
		return cacheFSSize{}, errors.New("no path")
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return cacheFSSize{}, err
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return cacheFSSize{}, errors.New("statfs: no block size")
	}
	return cacheFSSize{
		FSType: cfgFSTypeName(uint32(st.Type)),
		Total:  int64(st.Blocks) * bsize,
		Avail:  int64(st.Bavail) * bsize,
	}, nil
}

func cfgFSTypeName(magic uint32) string {
	switch magic {
	case cfgMagicTmpfs:
		return "tmpfs"
	case cfgMagicOverlay:
		return "overlayfs"
	case cfgMagicExt4:
		return "ext4"
	case cfgMagicXFS:
		return "xfs"
	case cfgMagicBtrfs:
		return "btrfs"
	case cfgMagicZFS:
		return "zfs"
	case cfgMagicNFS:
		return "nfs"
	case cfgMagicFUSE:
		return "fuse"
	case cfgMagicCIFS:
		return "cifs"
	case cfgMagicF2FS:
		return "f2fs"
	default:
		return ""
	}
}
