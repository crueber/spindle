// insights_test.go — 04_git.md §16.4: the bounded history scan's parser and
// real-git shape. The parser tests pin the record stream (including a merge
// commit's missing shortstat and a name containing a newline); the integration
// tests pin --since, --max-count truncation and the hardening flags' effect on
// stats (merge commits contribute 0 lines).
package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commitFile(t *testing.T, dir, name, body, when string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	activityGit(t, dir, nil, "add", name)
	env := []string{
		"GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_DATE=" + when,
	}
	activityGit(t, dir, env, "commit", "-qm", "commit "+name+" "+when)
	return activityGit(t, dir, nil, "rev-parse", "HEAD")
}

func TestParseShortstat(t *testing.T) {
	cases := []struct {
		in       string
		add, del int
	}{
		{" 3 files changed, 10 insertions(+), 2 deletions(-)\n", 10, 2},
		{" 1 file changed, 1 insertion(+)\n", 1, 0},
		{" 1 file changed, 1 deletion(-)\n", 0, 1},
		{" 1 file changed, 0 insertions(+), 0 deletions(-)\n", 0, 0},
		{" 2 files changed\n", 0, 0},
		{"", 0, 0},
		{" garbage, no counts", 0, 0},
	}
	for _, tc := range cases {
		add, del := parseShortstat(tc.in)
		if add != tc.add || del != tc.del {
			t.Fatalf("parseShortstat(%q) = %d/%d, want %d/%d", tc.in, add, del, tc.add, tc.del)
		}
	}
}

func TestParseInsightsLogShapes(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	rec := "\x1e" + sha1 + "\x00Ann\x00ann@example.com\x002026-11-01T10:00:00Z\n\n 2 files changed, 5 insertions(+), 1 deletion(-)\n" +
		// A merge commit: no shortstat block at all.
		"\x1e" + sha2 + "\x00Bob\x00bob@example.com\x002026-11-02T10:00:00+02:00\n" +
		// A malformed record is skipped, not fatal.
		"\x1egarbage\x00no date\n"
	got := parseInsightsLog([]byte(rec))
	if len(got) != 2 {
		t.Fatalf("parsed %d commits, want 2: %+v", len(got), got)
	}
	if got[0].ID != sha1 || got[0].AuthorName != "Ann" || got[0].Additions != 5 || got[0].Deletions != 1 {
		t.Fatalf("first commit = %+v", got[0])
	}
	if got[1].ID != sha2 || got[1].Additions != 0 || got[1].Deletions != 0 {
		t.Fatalf("merge commit = %+v", got[1])
	}
	// The +02:00 timestamp normalizes to UTC (bucketing is UTC).
	if got[1].Committed.Hour() != 8 {
		t.Fatalf("timestamp not normalized: %v", got[1].Committed)
	}
}

func TestParseInsightsLogNameWithNewline(t *testing.T) {
	sha := strings.Repeat("c", 40)
	// The record prefix splits on NUL, so a newline inside a name cannot
	// shift the date into the name field.
	rec := "\x1e" + sha + "\x00Weird\nName\x00w@example.com\x002026-01-01T00:00:00Z\n"
	got := parseInsightsLog([]byte(rec))
	if len(got) != 1 || got[0].AuthorName != "Weird\nName" {
		t.Fatalf("got %+v", got)
	}
}

func TestInsightsCommitsRejectsInputs(t *testing.T) {
	ctx := context.Background()
	l := NewLayer()
	repo := &LocalRepo{Path: t.TempDir()}
	sha := strings.Repeat("a", 40)
	if _, _, err := l.InsightsCommits(ctx, nil, sha, time.Now(), 10); err == nil {
		t.Fatal("nil repo must fail")
	}
	if _, _, err := l.InsightsCommits(ctx, repo, "HEAD", time.Now(), 10); err == nil {
		t.Fatal("non-hex revision must fail")
	}
	if _, _, err := l.InsightsCommits(ctx, repo, sha, time.Time{}, 10); err == nil {
		t.Fatal("zero since must fail")
	}
	if _, _, err := l.InsightsCommits(ctx, repo, sha, time.Now(), 10); err == nil {
		t.Fatal("missing object must fail")
	}
}

func TestInsightsCommitsRealGit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	old := commitFile(t, dir, "a.txt", "a\n", "2026-01-01T00:00:00Z")
	mid := commitFile(t, dir, "a.txt", "a\nb\n", "2026-06-01T00:00:00Z")
	head := commitFile(t, dir, "a.txt", "a\nb\nc\n", "2026-11-01T00:00:00Z")

	l := NewLayer()
	repo := &LocalRepo{Path: dir}
	// --since excludes the January commit.
	commits, truncated, err := l.InsightsCommits(ctx, repo, head, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("InsightsCommits: %v", err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(commits) != 2 || commits[0].ID != head || commits[1].ID != mid {
		t.Fatalf("commits = %+v", commits)
	}
	if commits[0].Additions != 1 || commits[0].Deletions != 0 {
		t.Fatalf("stats = %+v", commits[0])
	}
	// Since after every commit → empty, not an error.
	empty, _, err := l.InsightsCommits(ctx, repo, head, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty window = %d %v", len(empty), err)
	}
	// Cap asks for one extra: reaching it reports truncation and drops the tail.
	capped, trunc, err := l.InsightsCommits(ctx, repo, head, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil || !trunc || len(capped) != 2 {
		t.Fatalf("cap = %d trunc=%v err=%v", len(capped), trunc, err)
	}
	if capped[0].ID != head || capped[1].ID != mid {
		t.Fatalf("cap kept the wrong commits: %+v", capped)
	}
	_ = old
}

func TestInsightsCommitsMergeContributesNoLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	base := commitFile(t, dir, "a.txt", "a\n", "2026-11-01T00:00:00Z")
	activityGit(t, dir, nil, "checkout", "-q", "-b", "side")
	_ = commitFile(t, dir, "b.txt", "b\n", "2026-11-02T00:00:00Z")
	activityGit(t, dir, nil, "checkout", "-q", "main")
	env := []string{"GIT_AUTHOR_DATE=2026-11-03T00:00:00Z", "GIT_COMMITTER_DATE=2026-11-03T00:00:00Z"}
	activityGit(t, dir, env, "merge", "--no-ff", "-m", "merge side", "side")
	head := activityGit(t, dir, nil, "rev-parse", "HEAD")

	l := NewLayer()
	commits, _, err := l.InsightsCommits(ctx, &LocalRepo{Path: dir}, head, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("InsightsCommits: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("commits = %d: %+v", len(commits), commits)
	}
	if commits[0].ID != head {
		t.Fatalf("merge not first: %+v", commits[0])
	}
	if commits[0].Additions != 0 || commits[0].Deletions != 0 {
		t.Fatalf("merge contributed lines: %+v", commits[0])
	}
	_ = base
}

func TestParseInsightsLogMalformedRecords(t *testing.T) {
	sha := strings.Repeat("d", 40)
	rec := "\x1e\x00no id\n" + // empty oid
		"\x1e\x00a\x00b\n" + // too few fields
		"\x1e" + strings.Repeat("0", 40) + "\x00A\x00a@x\x002026-01-01T00:00:00Z\n" + // zero oid
		"\x1e" + sha + "\x00A\x00a@x\x00not-a-date\n" + // bad date
		"\x1e" + sha + "\x00A\x00a@x\x002026-01-01T00:00:00Z\n"
	got := parseInsightsLog([]byte(rec))
	if len(got) != 1 || got[0].ID != sha {
		t.Fatalf("malformed records = %+v", got)
	}
}

func TestParseShortstatEdgeNumbers(t *testing.T) {
	// A matching suffix with no digits returns 0.
	if add, _ := parseShortstat(" insertions(+)"); add != 0 {
		t.Fatalf("no digits = %d", add)
	}
	// An out-of-range integer is ignored rather than saturating.
	big := strings.Repeat("9", 30) + " insertions(+)"
	if add, _ := parseShortstat(big); add != 0 {
		t.Fatalf("overflow = %d", add)
	}
}

func TestInsightsCommitsClampsCap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	head := commitFile(t, dir, "a.txt", "a\n", "2026-11-01T00:00:00Z")
	l := NewLayer()
	// cap <= 0 resolves to the 5000 default; a cap above the hard ceiling is
	// clamped to InsightsScanMax (the repo has one commit either way).
	if got, _, err := l.InsightsCommits(ctx, &LocalRepo{Path: dir}, head, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 0); err != nil || len(got) != 1 {
		t.Fatalf("zero cap = %d %v", len(got), err)
	}
	if got, _, err := l.InsightsCommits(ctx, &LocalRepo{Path: dir}, head, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), InsightsScanMax+1); err != nil || len(got) != 1 {
		t.Fatalf("clamped cap = %d %v", len(got), err)
	}
}
