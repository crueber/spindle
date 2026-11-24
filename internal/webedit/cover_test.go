package webedit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSubprocessGitResolveAndRootFile covers the resolve/root-file reads the
// service uses (the lifecycle test covers commit construction).
func TestSubprocessGitResolveAndRootFile(t *testing.T) {
	bare, base := newBareRepo(t)
	g := NewSubprocessGit("")
	ctxb := context.Background()

	if got, err := g.ResolveBranch(ctxb, bare, "main"); err != nil || got != base {
		t.Fatalf("ResolveBranch = %q/%v", got, err)
	}
	// Short and full spellings both work.
	if got, err := g.ResolveBranch(ctxb, bare, "refs/heads/main"); err != nil || got != base {
		t.Fatalf("full ref = %q/%v", got, err)
	}
	if _, err := g.ResolveBranch(ctxb, bare, "nope"); !isErr(err, ErrNotFound) {
		t.Fatalf("absent branch = %v", err)
	}
	if _, err := g.ResolveBranch(ctxb, bare, "-x"); !isErr(err, ErrInvalid) {
		t.Fatalf("option branch = %v", err)
	}
	if _, ok, err := g.RootFile(ctxb, bare, base, ".gitattributes"); err != nil || ok {
		t.Fatalf("absent attrs = %v/%v", ok, err)
	}
	// A directory at the root name is an error, not a file.
	if _, ok, err := g.RootFile(ctxb, bare, base, "."); err == nil || ok {
		t.Fatalf("dot root = %v/%v", ok, err)
	}
}

// TestSubprocessGitTreeAndSymlinkGuards pins the type guards on the target
// path: a directory is a 400, a symlink a 415, never a silent retype.
func TestSubprocessGitTreeAndSymlinkGuards(t *testing.T) {
	bare, _ := newBareRepo(t)
	work := t.TempDir()
	gitIn(t, work, "clone", "-q", bare, "clone")
	clone := filepath.Join(work, "clone")
	if err := os.MkdirAll(filepath.Join(clone, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "dir", "f.txt"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(clone, "link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", ".")
	gitIn(t, clone, "commit", "-q", "-m", "tree+link")
	gitIn(t, clone, "push", "-q", "origin", "main")
	tip := gitIn(t, clone, "rev-parse", "HEAD")

	g := NewSubprocessGit("")
	ctxb := context.Background()
	if _, err := g.LookupFile(ctxb, bare, tip, "dir"); !isErr(err, ErrInvalid) {
		t.Fatalf("directory = %v", err)
	}
	if _, err := g.LookupFile(ctxb, bare, tip, "link"); !isErr(err, ErrUnsupported) {
		t.Fatalf("symlink = %v", err)
	}
	if f, err := g.LookupFile(ctxb, bare, tip, "dir/f.txt"); err != nil || string(f.Content) != "f\n" {
		t.Fatalf("nested = %+v/%v", f, err)
	}
	// An invalid commit is a backend error, not a not-found.
	if _, err := g.LookupFile(ctxb, bare, "not-a-sha", "hello.txt"); err == nil {
		t.Fatal("invalid commit read")
	}
}

// TestSubprocessGitBackendErrors covers the failure branches of the runner
// with a deliberately broken binary/dir.
func TestSubprocessGitBackendErrors(t *testing.T) {
	ctxb := context.Background()
	bad := &SubprocessGit{Binary: "spindle-no-such-git", Timeout: time.Second}
	if _, err := bad.ResolveBranch(ctxb, t.TempDir(), "main"); !isErr(err, ErrUnavailable) {
		t.Fatalf("missing binary = %v", err)
	}
	falseGit := &SubprocessGit{Binary: "/bin/false", Timeout: time.Second}
	if _, err := falseGit.ResolveBranch(ctxb, t.TempDir(), "main"); !isErr(err, ErrNotFound) {
		t.Fatalf("failing resolve = %v", err)
	}
	if _, err := falseGit.HashBlob(ctxb, t.TempDir(), []byte("x")); err == nil {
		t.Fatal("failing hash accepted")
	}
	if _, err := falseGit.PackTip(ctxb, t.TempDir(), testSHA); err == nil {
		t.Fatal("failing pack accepted")
	}
	if _, err := falseGit.DiffTree(ctxb, t.TempDir(), testSHA, testSHA); err == nil {
		t.Fatal("failing diff accepted")
	}
	// A canceled context yields an unavailable error, never a panic.
	cctx, cancel := context.WithCancel(ctxb)
	cancel()
	g := NewSubprocessGit("")
	if _, err := g.ResolveBranch(cctx, t.TempDir(), "main"); !isErr(err, ErrUnavailable) {
		t.Fatalf("canceled = %v", err)
	}

	bare, base := newBareRepo(t)
	if _, _, err := g.BuildCommit(ctxb, bare, CommitBuild{Base: "zz", Path: "x", Message: "m"}); !isErr(err, ErrInvalid) {
		t.Fatalf("bad base = %v", err)
	}
	if _, _, err := g.BuildCommit(ctxb, bare, CommitBuild{Base: base, Path: "../x", Message: "m"}); !isErr(err, ErrInvalid) {
		t.Fatalf("bad path = %v", err)
	}
	if _, _, err := g.BuildCommit(ctxb, bare, CommitBuild{Base: base, Path: "x", Message: "m"}); !isErr(err, ErrInvalid) {
		t.Fatalf("missing blob = %v", err)
	}
	// Deleting an absent path leaves the tree unchanged → ErrNoChange.
	if _, _, err := g.BuildCommit(ctxb, bare, CommitBuild{Base: base, Path: "absent.txt", Delete: true, Message: "m"}); !isErr(err, ErrNoChange) {
		t.Fatalf("delete absent = %v", err)
	}
	// A base that names no object fails in read-tree.
	if _, _, err := g.BuildCommit(ctxb, bare, CommitBuild{Base: strings.Repeat("a", 40), Path: "x", Delete: true, Message: "m"}); err == nil {
		t.Fatal("missing base tree accepted")
	}
	if _, err := g.PackTip(ctxb, bare, "nope"); !isErr(err, ErrInvalid) {
		t.Fatalf("bad tip = %v", err)
	}
}

func TestHexAndHelpers(t *testing.T) {
	if !isHexSHA(strings.Repeat("a", 40)) || !isHexSHA(strings.Repeat("f", 64)) {
		t.Fatal("valid oids rejected")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("A", 40)} {
		if isHexSHA(bad) {
			t.Errorf("isHexSHA(%q) = true", bad)
		}
	}
	if sanitizeIdent("<a>\r\nb") != "ab" {
		t.Fatalf("sanitize = %q", sanitizeIdent("<a>\r\nb"))
	}
	if sanitizeIdent(" \n ") != "spindle" {
		t.Fatalf("empty sanitize = %q", sanitizeIdent(" \n "))
	}
	if _, ok := parseLSTreeZ([]byte("nonsense")); ok {
		t.Fatal("garbage ls-tree parsed")
	}
	if _, ok := parseLSTreeZ([]byte("100644 blob\tx\x00")); ok {
		t.Fatal("short ls-tree parsed")
	}
	var b boundedStderr
	n, err := b.Write(bytes.Repeat([]byte("x"), 9000))
	if err != nil || n != 8192 || b.buf.Len() != 8192 {
		t.Fatalf("bounded write = %d/%v len=%d", n, err, b.buf.Len())
	}
	if n, _ := b.Write([]byte("more")); n != 4 {
		t.Fatal("post-full write")
	}
	ge := &gitExecError{argv: []string{"x"}, stderr: "boom", code: 1}
	if !strings.Contains(ge.Error(), "boom") {
		t.Fatalf("exec error = %q", ge.Error())
	}
}

func TestPoolAndNow(t *testing.T) {
	p := newGitPool(1)
	ctxb, cancel := context.WithCancel(context.Background())
	cancel()
	// Saturate the pool first: a ready slot AND a canceled ctx make the
	// select a coin flip (13_concurrency.md), so only the ctx arm is ready.
	p.sem <- struct{}{}
	if err := p.run(ctxb, func() error { return nil }); err == nil {
		t.Fatal("canceled pool ran")
	}
	<-p.sem
	ran := false
	if err := p.run(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatal("pool did not run")
	}
	s := &Service{}
	if s.now().IsZero() {
		t.Fatal("default clock zero")
	}
	s.Now = func() time.Time { return time.Unix(1, 0) }
	if s.now().Unix() != 1 {
		t.Fatal("pinned clock ignored")
	}
	if DefaultMaxBytes != 1<<20 {
		t.Fatal("default cap drifted")
	}
	_ = errors.New("")
}

// TestBuildCommitDirFileConflict forces the update-index failure arm: adding a
// regular file where the base tree has a directory is a D/F conflict.
func TestBuildCommitDirFileConflict(t *testing.T) {
	bare, base := newBareRepo(t)
	blob, err := NewSubprocessGit("").HashBlob(context.Background(), bare, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	// Create a tree with dir/ inside it first.
	first, _, err := NewSubprocessGit("").BuildCommit(context.Background(), bare, CommitBuild{
		Base: base, Path: "dir/file.txt", BlobSHA: blob, Message: "dir",
		AuthorName: "a", AuthorMail: "a@b", When: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewSubprocessGit("").BuildCommit(context.Background(), bare, CommitBuild{
		Base: first, Path: "dir", BlobSHA: blob, Message: "conflict",
		AuthorName: "a", AuthorMail: "a@b", When: time.Unix(1700000000, 0).UTC(),
	}); err == nil {
		t.Fatal("D/F conflict accepted")
	}
	// A valid-hex tip absent from the object db makes pack-objects fail.
	if _, err := NewSubprocessGit("").PackTip(context.Background(), bare, strings.Repeat("a", 40)); err == nil {
		t.Fatal("absent tip packed")
	}
}

// TestBuildCommitCommitTreeFailure makes the object db read-only after the
// blob lands, so commit-tree cannot write its object (the backend-error arm).
func TestBuildCommitCommitTreeFailure(t *testing.T) {
	bare, base := newBareRepo(t)
	g := NewSubprocessGit("")
	blob, err := g.HashBlob(context.Background(), bare, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(bare, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(objects, 0o755)
	_, _, err = g.BuildCommit(context.Background(), bare, CommitBuild{
		Base: base, Path: "x.txt", BlobSHA: blob, Message: "m",
		AuthorName: "a", AuthorMail: "a@b", When: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatal("read-only object db accepted the commit")
	}
}
