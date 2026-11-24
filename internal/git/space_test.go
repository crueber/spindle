package git

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestSpaceErrorNamesCacheDiagnosis pins BUG-6: an out-of-space local write
// must say which path, which filesystem and which capacity — and keep both the
// ErrNoSpace sentinel and the errno reachable.
func TestSpaceErrorNamesCacheDiagnosis(t *testing.T) {
	anchor := t.TempDir()
	target := anchor + "/pack-x.pack"
	err := wrapSpace(anchor, target, &os.PathError{Op: "write", Path: target, Err: syscall.ENOSPC})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("errors.Is(ErrNoSpace) = false: %v", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("errors.Is(ENOSPC) = false: %v", err)
	}
	var se *SpaceError
	if !errors.As(err, &se) {
		t.Fatalf("got %T, want *SpaceError", err)
	}
	if se.Path != target || se.Anchor != anchor {
		t.Errorf("path/anchor = %q/%q", se.Path, se.Anchor)
	}
	msg := err.Error()
	for _, want := range []string{target, anchor, "no space left on device", "cache.dir", "cache.max_bytes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if se.Total > 0 && !strings.Contains(msg, "free of") {
		t.Errorf("known capacity but message has no capacity: %q", msg)
	}

	// Non-space errors pass through untouched.
	plain := errors.New("boom")
	if got := wrapSpace(anchor, target, plain); got != plain {
		t.Fatalf("non-space error wrapped: %v", got)
	}
	if wrapSpace(anchor, target, nil) != nil {
		t.Fatal("nil error wrapped")
	}
}

func TestIsNoSpaceText(t *testing.T) {
	for _, s := range []string{
		"fatal: write error: No space left on device",
		"error: unable to write file: No space LEFT ON DEVICE",
		"write failed: Disk quota exceeded",
		"ENOSPC",
		"EDQUOT",
	} {
		if !isNoSpaceText(s) {
			t.Errorf("isNoSpaceText(%q) = false", s)
		}
	}
	for _, s := range []string{"", "fatal: pack has bad object", "fsck: dangling commit"} {
		if isNoSpaceText(s) {
			t.Errorf("isNoSpaceText(%q) = true", s)
		}
	}
}

// TestPackFailureClassifiesSpace pins the index-pack mapping: a space stderr
// becomes the cache diagnosis, an ordinary rejection stays a rejection.
func TestPackFailureClassifiesSpace(t *testing.T) {
	anchor := t.TempDir()
	scratch := anchor + "/walgit-ingest-1"
	space := packFailure(anchor, scratch, "fatal: write error: No space left on device", nil)
	if !errors.Is(space, ErrNoSpace) || !errors.Is(space, ErrPackRejected) {
		t.Fatalf("space failure sentinels lost: %v", space)
	}
	var se *SpaceError
	if !errors.As(space, &se) || se.Path != scratch {
		t.Fatalf("space failure = %#v", space)
	}
	if !strings.Contains(space.Error(), "cache.max_bytes") {
		t.Errorf("space failure lacks the remedy: %v", space)
	}

	// An errno-only cause (git exited before writing stderr) also classifies.
	errnoOnly := packFailure(anchor, scratch, "", fmt.Errorf("index-pack: %w", syscall.ENOSPC))
	if !errors.Is(errnoOnly, ErrNoSpace) {
		t.Fatalf("errno cause not classified: %v", errnoOnly)
	}

	reject := packFailure(anchor, scratch, "fatal: inflate error", errors.New("exit status 1"))
	var pr *PackRejectedError
	if !errors.As(reject, &pr) || pr.Detail != "fatal: inflate error" {
		t.Fatalf("ordinary rejection = %#v", reject)
	}
	if errors.Is(reject, ErrNoSpace) {
		t.Fatal("ordinary rejection classified as no-space")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB", -1: "unknown"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestSpaceErrorBranches covers the message variants: an unknown filesystem,
// no capacity, anchor == path, and a missing anchor (statfs unavailable).
func TestSpaceErrorBranches(t *testing.T) {
	cause := errors.New("quota")
	e := &SpaceError{Path: "/cache/x", Anchor: "/cache/x", Cause: cause}
	msg := e.Error()
	if !strings.Contains(msg, "/cache/x") || !strings.Contains(msg, "quota") {
		t.Fatalf("message = %q", msg)
	}
	if strings.Count(msg, "/cache/x") != 1 {
		t.Fatalf("anchor duplicated in %q", msg)
	}
	e = &SpaceError{Path: "/cache/x", Anchor: "/cache", FS: "tmpfs", Total: 1 << 20, Avail: 0, Cause: cause}
	if msg := e.Error(); !strings.Contains(msg, "tmpfs with 0 B free of 1.0 MiB") {
		t.Fatalf("capacity message = %q", msg)
	}
	// Missing anchor: statfs fails, the message still names the path/cause.
	err := newSpaceError("/nonexistent/spindle-anchor", "/nonexistent/spindle-anchor/x", cause)
	if !errors.Is(err, ErrNoSpace) || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("missing-anchor error = %v", err)
	}
	if err.FS != "" || err.Total != 0 {
		t.Fatalf("missing anchor reported capacity: %#v", err)
	}
}
