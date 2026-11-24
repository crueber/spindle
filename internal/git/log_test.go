// log_test.go — the bounded push-payload commit listing (04_git.md §9.11):
// real git, a two-commit range, a create (no old side) and the error arms the
// caller degrades to "no commits".
package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func logRangeRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a\n")
	activityGit(t, dir, []string{"GIT_AUTHOR_NAME=Amy", "GIT_AUTHOR_EMAIL=amy@example.com"}, "add", "a.txt")
	activityGit(t, dir, []string{"GIT_AUTHOR_NAME=Amy", "GIT_AUTHOR_EMAIL=amy@example.com",
		"GIT_AUTHOR_DATE=2026-11-20T10:00:00+00:00"}, "commit", "-qm", "first commit")
	base := activityGit(t, dir, nil, "rev-parse", "HEAD")
	write("b.txt", "b\n")
	activityGit(t, dir, nil, "add", "b.txt")
	activityGit(t, dir, []string{"GIT_AUTHOR_DATE=2026-11-21T10:00:00+00:00"}, "commit", "-qm", "second commit")
	tip := activityGit(t, dir, nil, "rev-parse", "HEAD")
	return dir, base, tip
}

func TestLogRange_UpdateListsOnlyNewCommits(t *testing.T) {
	dir, base, tip := logRangeRepo(t)
	l := NewLayer()
	commits, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, base, tip, 50)
	if err != nil {
		t.Fatalf("LogRange: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %+v, want 1", commits)
	}
	c := commits[0]
	if c.ID != tip || c.Message != "second commit" || c.Author == "" || c.At != "2026-11-21T10:00:00Z" {
		t.Fatalf("commit = %+v", c)
	}
}

func TestLogRange_CreateWalksReachableHistoryBounded(t *testing.T) {
	dir, _, tip := logRangeRepo(t)
	l := NewLayer()
	// Zero/empty old = a create: newest `limit` commits reachable from tip.
	commits, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, strings.Repeat("0", 40), tip, 1)
	if err != nil {
		t.Fatalf("LogRange: %v", err)
	}
	if len(commits) != 1 || commits[0].Message != "second commit" {
		t.Fatalf("create listing = %+v", commits)
	}
	commits, err = l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", tip, 50)
	if err != nil || len(commits) != 2 {
		t.Fatalf("full create listing = (%d, %v)", len(commits), err)
	}
	if commits[0].Message != "second commit" || commits[1].Message != "first commit" {
		t.Fatalf("order = %+v", commits)
	}
}

func TestLogRange_ErrorsNeverPanic(t *testing.T) {
	dir, _, tip := logRangeRepo(t)
	l := NewLayer()
	if _, err := l.LogRange(context.Background(), nil, "", tip, 5); err == nil {
		t.Fatal("nil repo must error")
	}
	if _, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", "not-a-sha", 5); err == nil {
		t.Fatal("bad new oid must error")
	}
	if _, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", strings.Repeat("0", 40), 5); err == nil {
		t.Fatal("zero new oid must error")
	}
	if _, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", strings.Repeat("a", 40), 5); err == nil {
		t.Fatal("missing object must error")
	}
	if _, err := l.LogRange(context.Background(), &LocalRepo{Path: t.TempDir()}, "", tip, 5); err == nil {
		t.Fatal("not a repository must error")
	}
	// limit clamps (0 → default, huge → cap) without failing.
	if commits, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", tip, 0); err != nil || len(commits) != 2 {
		t.Fatalf("limit 0 = (%d, %v)", len(commits), err)
	}
	if commits, err := l.LogRange(context.Background(), &LocalRepo{Path: dir}, "", tip, 9999); err != nil || len(commits) != 2 {
		t.Fatalf("limit cap = (%d, %v)", len(commits), err)
	}
}

func TestParseLogRange_SkipsGarbage(t *testing.T) {
	rec := strings.Join([]string{
		strings.Repeat("a", 40) + "\x00Amy\x00amy@x\x002026-01-01T00:00:00Z\x00one",
		"not-a-record",
		strings.Repeat("b", 40) + "\x00Bob\x00\x002026-01-02T00:00:00Z\x00two",
	}, "\x1e") + "\x1e"
	got := parseLogRange([]byte(rec))
	if len(got) != 2 || got[0].Message != "one" || got[1].Message != "two" {
		t.Fatalf("parseLogRange = %+v", got)
	}
	if parseLogRange(nil) == nil {
		// Empty output is an empty slice, never nil, at the call site.
		t.Log("nil input tolerated")
	}
}
