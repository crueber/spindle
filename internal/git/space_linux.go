//go:build linux

package git

import (
	"errors"
	"syscall"
)

// cacheFS is the filesystem capacity + type behind one cache directory.
type cacheFS struct {
	FSType string
	Total  int64
	Avail  int64
}

// linux statfs magic numbers (include/uapi/linux/magic.h) worth naming in an
// operator message; anything else stays "a filesystem" (empty FSType).
const (
	magicTmpfs   = 0x01021994
	magicOverlay = 0x794c7630
	magicExt4    = 0xef53
	magicXFS     = 0x58465342
	magicBtrfs   = 0x9123683e
	magicZFS     = 0x2fc12fc1
	magicNFS     = 0x6969
	magicFUSE    = 0x65735546
	magicCIFS    = 0xff534d42
	magicF2FS    = 0xf2f52010
)

// cacheFilesystem stats the filesystem holding dir.
func cacheFilesystem(dir string) (cacheFS, error) {
	if dir == "" {
		return cacheFS{}, errors.New("no path")
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return cacheFS{}, err
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return cacheFS{}, errors.New("statfs: no block size")
	}
	return cacheFS{
		FSType: fsTypeName(uint32(st.Type)),
		Total:  int64(st.Blocks) * bsize,
		Avail:  int64(st.Bavail) * bsize,
	}, nil
}

func fsTypeName(magic uint32) string {
	switch magic {
	case magicTmpfs:
		return "tmpfs"
	case magicOverlay:
		return "overlayfs"
	case magicExt4:
		return "ext4"
	case magicXFS:
		return "xfs"
	case magicBtrfs:
		return "btrfs"
	case magicZFS:
		return "zfs"
	case magicNFS:
		return "nfs"
	case magicFUSE:
		return "fuse"
	case magicCIFS:
		return "cifs"
	case magicF2FS:
		return "f2fs"
	default:
		return ""
	}
}

// isNoSpaceSyscall reports the errno causes of a full filesystem or an
// exceeded quota.
func isNoSpaceSyscall(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
