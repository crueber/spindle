// insights.go — the bounded history scan behind the repository Insights page
// (04_git.md §16.4, docs/features/18_repo_insights.md).
//
// One invocation answers every git-derived fact the page shows: per-commit
// identity, committer date and line totals over a bounded window. It is a task
// path (maintainer/on-demand refresh), never a request path: the page reads the
// cached projection in the bucket.
//
// The argv is pinned and deliberately narrow: a resolved hex revision (never a
// user string), a commit cap (one extra asked for so truncation is observable
// without a second read), a --since floor in UTC, and hardening flags that stop
// repository config from changing the output or spawning helpers
// (--no-ext-diff, --no-textconv, --no-show-signature, --no-decorate).
package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// InsightsCommit is one commit's contribution to the Insights projection.
type InsightsCommit struct {
	ID          string
	AuthorName  string
	AuthorEmail string
	Committed   time.Time // committer date, UTC
	Additions   int
	Deletions   int
}

// insightsScanTimeout bounds one bounded history walk. It runs under the task
// context too; this is the hard ceiling for a pathological repository.
const insightsScanTimeout = 5 * time.Minute

// InsightsScanMax caps one scan regardless of the caller's request (the
// projection also chooses a smaller default; see internal/insights).
const InsightsScanMax = 100_000

// insightsFormat is the record prefix: one 0x1e per commit, fields NUL
// separated. The shortstat block follows the format line within the record.
const insightsFormat = "%x1e%H%x00%an%x00%ae%x00%cI"

// InsightsCommits runs `git log --no-renames --max-count=<cap+1>
// --since=<start> --format=<insightsFormat> --shortstat <rev>` and returns at
// most cap commits, newest first, plus whether the cap truncated the window.
//
// Additions/deletions are the commit's `--shortstat` totals; a merge commit
// contributes its existence but zero lines (git log shows no diff for a merge
// without -m, and re-walking merges would double-count their branches). Binary
// changes contribute zero. Every failure is hard — the caller records a failed
// refresh, never a half-truth projection.
func (l *Layer) InsightsCommits(ctx context.Context, repo *LocalRepo, rev string, since time.Time, cap int) ([]InsightsCommit, bool, error) {
	if repo == nil || repo.Path == "" {
		return nil, false, errInvalidInput("insights: nil repo")
	}
	if !ValidOid(rev) || isZeroOid(rev) {
		return nil, false, errInvalidInput("insights: bad revision %q", rev)
	}
	if since.IsZero() {
		return nil, false, errInvalidInput("insights: zero since")
	}
	if cap <= 0 {
		cap = 5000
	}
	if cap > InsightsScanMax {
		cap = InsightsScanMax
	}
	argv := []string{
		"log",
		"--no-renames",
		"--no-ext-diff",
		"--no-textconv",
		"--no-show-signature",
		"--no-decorate",
		"--max-count=" + strconv.Itoa(cap+1),
		"--since=" + since.UTC().Format(time.RFC3339),
		"--format=" + insightsFormat,
		"--shortstat",
		rev,
	}
	out, _, err := l.runCollect(ctx, execSpec{argv: argv, dir: repo.Path, timeout: insightsScanTimeout})
	if err != nil {
		return nil, false, fmt.Errorf("insights scan %s: %w", rev, err)
	}
	commits := parseInsightsLog(out)
	truncated := len(commits) > cap
	if truncated {
		commits = commits[:cap]
	}
	return commits, truncated, nil
}

// parseInsightsLog parses the §16.4 record stream. A malformed record is
// skipped (one bad commit must not fail the whole projection); a valid record
// requires an oid and a parseable committer date.
func parseInsightsLog(out []byte) []InsightsCommit {
	var commits []InsightsCommit
	records := strings.Split(string(out), "\x1e")
	commits = make([]InsightsCommit, 0, len(records))
	for _, rec := range records {
		if rec == "" {
			continue
		}
		// The NUL split is on the whole record: the shortstat block rides in
		// the last field, so names/emails containing newlines cannot shift
		// the field count.
		fields := strings.SplitN(rec, "\x00", 4)
		if len(fields) != 4 {
			continue
		}
		id := strings.TrimSpace(fields[0])
		if !ValidOid(id) || isZeroOid(id) {
			continue
		}
		dateText := fields[3]
		stats := ""
		if i := strings.IndexByte(dateText, '\n'); i >= 0 {
			stats = dateText[i+1:]
			dateText = dateText[:i]
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(dateText))
		if err != nil {
			continue
		}
		add, del := parseShortstat(stats)
		commits = append(commits, InsightsCommit{
			ID:          id,
			AuthorName:  strings.TrimSpace(fields[1]),
			AuthorEmail: strings.TrimSpace(fields[2]),
			Committed:   at.UTC(),
			Additions:   add,
			Deletions:   del,
		})
	}
	return commits
}

// parseShortstat extracts insertions/deletions from one `--shortstat` block
// (" 3 files changed, 10 insertions(+), 2 deletions(-)"). Any shape (singular
// nouns, insertions-only, deletions-only, binary-only) yields what it states;
// an absent block is 0/0.
func parseShortstat(s string) (add, del int) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasSuffix(part, "insertion(+)") || strings.HasSuffix(part, "insertions(+)"):
			add = leadingInt(part)
		case strings.HasSuffix(part, "deletion(-)") || strings.HasSuffix(part, "deletions(-)"):
			del = leadingInt(part)
		}
	}
	return add, del
}

func leadingInt(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0
	}
	return n
}
