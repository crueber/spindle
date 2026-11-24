package git

import (
	"errors"
	"fmt"
	"strings"
)

// Local write failures for space (BUG-6) are diagnosed here instead of
// reaching the client as a generic "unpacker error". The git layer owns the
// paths (the repo git-dir lives under cache.dir) and can name the filesystem
// and its capacity; the server adds the configured cache.max_bytes.

// ErrNoSpace marks a local git write that failed for lack of space or over a
// quota. Callers use errors.Is to pick the cache-space remedy text.
var ErrNoSpace = errors.New("git cache out of space")

// SpaceError is an out-of-space failure while writing the local cache copy.
// It names the path, the filesystem holding it, and the capacity so the
// operator does not have to guess which mount ran dry.
type SpaceError struct {
	Path   string // the write target (a file, or the scratch dir)
	Anchor string // the repo/cache directory the path lives under
	FS     string // filesystem type ("tmpfs", "ext4", …) when known
	Total  int64  // bytes on the filesystem, when known
	Avail  int64  // bytes free on the filesystem, when known
	Cause  error
}

func (e *SpaceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "git cache out of space writing %s", e.Path)
	if e.Anchor != "" && e.Anchor != e.Path {
		fmt.Fprintf(&b, " (repo cache %s)", e.Anchor)
	}
	if e.FS != "" || e.Total > 0 {
		b.WriteString(" on ")
		if e.FS != "" {
			b.WriteString(e.FS)
		} else {
			b.WriteString("a filesystem")
		}
		if e.Total > 0 {
			fmt.Fprintf(&b, " with %s free of %s", humanBytes(e.Avail), humanBytes(e.Total))
		}
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	b.WriteString(" — the write target is under the git cache (cache.dir) or the pack-feed temp (TMPDIR); free space on that filesystem, raise its quota, or lower cache.max_bytes")
	return b.String()
}

// Is reports ErrNoSpace so errors.Is(err, ErrNoSpace) works through wrapping.
func (e *SpaceError) Is(target error) bool { return target == ErrNoSpace }

// Unwrap keeps the underlying error (including syscall.ENOSPC) reachable.
func (e *SpaceError) Unwrap() error { return e.Cause }

func newSpaceError(anchor, path string, cause error) *SpaceError {
	e := &SpaceError{Path: path, Anchor: anchor, Cause: cause}
	if fs, err := cacheFilesystem(anchor); err == nil {
		e.FS, e.Total, e.Avail = fs.FSType, fs.Total, fs.Avail
	}
	return e
}

// wrapSpace converts a space failure into a SpaceError naming anchor/path;
// other errors pass through unchanged.
func wrapSpace(anchor, path string, err error) error {
	if err == nil || !isNoSpace(err) {
		return err
	}
	return newSpaceError(anchor, path, err)
}

// isNoSpace reports a filesystem-full/quota failure, either from a syscall
// cause or from git's stderr (index-pack prints the raw strerror text).
func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if isNoSpaceSyscall(err) {
		return true
	}
	return isNoSpaceText(err.Error())
}

// packFailure classifies one failed index-pack run: a space fault (stderr
// wording or a wrapped errno) becomes a SpaceError that still satisfies
// errors.Is(ErrPackRejected); anything else stays the plain rejection.
func packFailure(anchor, scratch, detail string, runErr error) error {
	reject := &PackRejectedError{Detail: detail}
	if !isNoSpaceText(detail) && !isNoSpace(runErr) {
		return reject
	}
	var cause error = reject
	if runErr != nil && !isNoSpaceText(detail) {
		cause = fmt.Errorf("%v: %w", runErr, reject)
	}
	return newSpaceError(anchor, scratch, cause)
}

// isNoSpaceText matches git's/libc's ENOSPC/EDQUOT wordings.
func isNoSpaceText(s string) bool {
	l := strings.ToLower(s)
	for _, marker := range []string{
		"no space left on device",
		"disk quota exceeded",
		"enospc",
		"edquot",
	} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

// humanBytes renders a byte count for an operator-facing message (GiB/MiB/KiB
// with one decimal, plain bytes below 1 KiB).
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
