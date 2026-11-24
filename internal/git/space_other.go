//go:build !linux

package git

import "errors"

// cacheFS is the filesystem capacity + type behind one cache directory.
type cacheFS struct {
	FSType string
	Total  int64
	Avail  int64
}

// cacheFilesystem is unavailable off Linux: the message then names the path
// and the cause without a capacity (still far better than "unpacker error").
func cacheFilesystem(string) (cacheFS, error) { return cacheFS{}, errors.New("statfs unavailable") }

// isNoSpaceSyscall is false off Linux; isNoSpaceText still matches git's
// stderr wording.
func isNoSpaceSyscall(error) bool { return false }
