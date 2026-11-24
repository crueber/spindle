package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
)

// TestDescribeIngestFailure pins BUG-6's server half: a cache-space ingest
// failure reaches the git wire with the configured cache.dir/cache.max_bytes
// (the git layer already names the path, filesystem and capacity), and any
// other ingest failure is unchanged.
func TestDescribeIngestFailure(t *testing.T) {
	s := &Server{cfg: &config.Config{Cache: config.Cache{
		Dir:      "/var/lib/spindle/cache",
		MaxBytes: 512 << 20,
	}}}
	space := &git.SpaceError{
		Path:   "/var/lib/spindle/cache/o/r.git/walgit-ingest-1",
		Anchor: "/var/lib/spindle/cache/o/r.git",
		FS:     "tmpfs",
		Total:  256 << 20,
		Avail:  0,
		Cause:  errors.New("write: no space left on device"),
	}
	msg, isSpace := s.describeIngestFailure(space)
	if !isSpace {
		t.Fatal("cache-space failure not classified")
	}
	for _, want := range []string{"cache.dir", "cache.max_bytes=536870912", "/var/lib/spindle/cache",
		"tmpfs", "no space left on device"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}

	// Wrapped space errors are still classified.
	wrapped := errors.Join(errors.New("push"), space)
	if _, isSpace := s.describeIngestFailure(wrapped); !isSpace {
		t.Fatal("wrapped cache-space failure not classified")
	}

	// A plain ingest failure is passed through byte-for-byte.
	plain := errors.New("pack rejected: fatal: inflate error")
	msg, isSpace = s.describeIngestFailure(plain)
	if isSpace || msg != plain.Error() {
		t.Fatalf("plain failure = %q, space=%v", msg, isSpace)
	}
}
