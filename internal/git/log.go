// log.go — bounded commit listing for push event payloads (04_git.md §9.11).
//
// This is the ONE git call the events/notify layers make to enrich a push
// webhook with real commit summaries. It is deliberately bounded (limit) and
// best-effort at the call site: a missing object or a cold serving copy must
// never fail the delivery — the payload then carries an empty commits list,
// which the receiver can distinguish from a delete (zero `after`).
package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EventCommit is one commit summary in a push event payload. Message is the
// subject line only (a full body can exceed the delivery-body cap).
type EventCommit struct {
	ID      string
	Author  string
	Email   string
	Message string
	At      string // author date, RFC 3339
}

// logRangeFormat is the §9.11 argv format string: five NUL-separated fields
// per commit, records separated by 0x1e (a subject may contain anything but
// NUL/0x1e).
const logRangeFormat = "%H%x00%an%x00%ae%x00%aI%x00%s%x1e"

// LogRangeTimeout bounds one commit listing (a local object read on a
// materialized serving copy; pool-accounted like every other Layer exec).
const LogRangeTimeout = 30 * time.Second

// LogRange runs `git log --no-renames --max-count=<limit> <new> [--not <old>]`
// and returns up to limit commits, newest first. old may be empty or the zero
// OID (a create: the whole reachable history of new, truncated to limit).
// Callers treat every error as "no commit summaries" and still deliver.
func (l *Layer) LogRange(ctx context.Context, repo *LocalRepo, old, new string, limit int) ([]EventCommit, error) {
	if repo == nil || repo.Path == "" {
		return nil, errInvalidInput("log range: nil repo")
	}
	if !ValidOid(new) || isZeroOid(new) {
		return nil, errInvalidInput("log range: bad new oid %q", new)
	}
	haveOld := old != "" && ValidOid(old) && !isZeroOid(old)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	argv := []string{
		"log", "--no-renames", "--max-count=" + strconv.Itoa(limit),
		"--format=" + logRangeFormat, new,
	}
	if haveOld {
		argv = append(argv, "--not", old)
	}
	out, _, err := l.runCollect(ctx, execSpec{argv: argv, dir: repo.Path, timeout: LogRangeTimeout})
	if err != nil {
		return nil, fmt.Errorf("log range %s..%s: %w", old, new, err)
	}
	return parseLogRange(out), nil
}

// parseLogRange parses the §9.11 record stream. An unparseable record is
// skipped (best-effort payload enrichment, never a hard failure).
func parseLogRange(out []byte) []EventCommit {
	commits := make([]EventCommit, 0, 8)
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimPrefix(rec, "\n")
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x00")
		if len(f) != 5 || !ValidOid(f[0]) {
			continue
		}
		commits = append(commits, EventCommit{
			ID: f[0], Author: f[1], Email: f[2], At: f[3], Message: f[4],
		})
	}
	return commits
}
