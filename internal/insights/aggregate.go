// aggregate.go — the pure aggregation layer: UTC bucketing, per-window
// commit/line totals, distinct authors and the collaboration counts. No I/O,
// no clock (now is a parameter), so the math is tested directly.
package insights

import (
	"sort"
	"strconv"
	"time"

	"github.com/crueber/spindle/internal/git"
)

// collabTimes is the issue/PR timeline the shared index (plus the bounded
// pr.json merge classification) yields. Times are UTC.
type collabTimes struct {
	prsOpened    []time.Time
	issuesOpened []time.Time
	prsMerged    []time.Time
	issuesClosed []time.Time

	known           bool
	indexBounded    bool
	mergeScanCapped bool
	sidecarReads    int
}

// contributorAcc accumulates one email's window activity.
type contributorAcc struct {
	name       string
	nameAt     time.Time
	commits    int
	lastActive time.Time
}

// buildWindows aggregates the scanned commits and the collab timeline into
// every selectable window. commits must be newest-first (the git layer's
// order); aggregation does not depend on it except for the display-name pick,
// which is date-compared anyway.
func buildWindows(commits []git.InsightsCommit, now time.Time, ct collabTimes) map[string]*Window {
	now = now.UTC()
	out := make(map[string]*Window, len(Windows))
	for _, days := range Windows {
		out[strconv.Itoa(days)] = buildWindow(commits, now, days, ct)
	}
	return out
}

func buildWindow(commits []git.InsightsCommit, now time.Time, days int, ct collabTimes) *Window {
	kind := specFor(days)
	start := windowStart(now, days)
	first := bucketStart(start, kind)
	last := bucketStart(now, kind)

	w := &Window{
		Days:               days,
		Bucket:             kind.String(),
		Start:              rfc3339(start),
		End:                rfc3339(now),
		ContributorsTotal:  0,
		ContributorsCapped: false,
	}
	// Bound the loop: a selectable window never needs more than 366 buckets.
	for t, guard := first, 0; !t.After(last) && guard < 400; t, guard = nextBucket(t, kind), guard+1 {
		w.Buckets = append(w.Buckets, Bucket{Start: rfc3339(t), Label: bucketLabel(t, kind)})
	}

	authors := map[string]*contributorAcc{}
	for _, c := range commits {
		at := c.Committed.UTC()
		// A commit dated after the projection instant is outside every
		// window (clock skew or a deliberate future date); Scan.FutureDated
		// reports it rather than scattering it into today's bar.
		if at.Before(start) || at.After(now) {
			continue
		}
		bi := bucketOffset(first, at, kind)
		b := &w.Buckets[bi]
		b.Commits++
		b.Additions += c.Additions
		b.Deletions += c.Deletions
		w.Commits++
		w.Additions += c.Additions
		w.Deletions += c.Deletions

		key := normalizeEmail(c.AuthorEmail)
		if key == "" {
			// An empty email is legal in a malformed commit; keying it on the
			// name keeps two distinct empty-email authors distinct.
			key = "name:" + normalizeEmail(c.AuthorName)
		}
		acc := authors[key]
		if acc == nil {
			acc = &contributorAcc{name: c.AuthorName, nameAt: at}
			authors[key] = acc
		} else if at.After(acc.nameAt) {
			// The newest name wins for a shared email (the person renamed).
			acc.name, acc.nameAt = c.AuthorName, at
		}
		acc.commits++
		if at.After(acc.lastActive) {
			acc.lastActive = at
		}
	}

	w.ContributorsTotal = len(authors)
	for key, acc := range authors {
		email := key
		if len(key) > 5 && key[:5] == "name:" {
			email = ""
		}
		w.Contributors = append(w.Contributors, Contributor{
			Name:       acc.name,
			Email:      email,
			Commits:    acc.commits,
			LastActive: rfc3339(acc.lastActive),
		})
	}
	sort.Slice(w.Contributors, func(i, j int) bool {
		a, b := w.Contributors[i], w.Contributors[j]
		if a.Commits != b.Commits {
			return a.Commits > b.Commits
		}
		if a.LastActive != b.LastActive {
			return a.LastActive > b.LastActive
		}
		return a.Email < b.Email
	})
	if len(w.Contributors) > MaxContributors {
		w.Contributors = w.Contributors[:MaxContributors]
		w.ContributorsCapped = true
	}
	if w.Contributors == nil {
		w.Contributors = []Contributor{}
	}

	w.PRsOpened = countIn(ct.prsOpened, start, now)
	w.PRsMerged = countIn(ct.prsMerged, start, now)
	w.IssuesOpened = countIn(ct.issuesOpened, start, now)
	w.IssuesClosed = countIn(ct.issuesClosed, start, now)
	return w
}

// bucketOffset is the bucket index of at for a list whose first bucket starts
// at first. at is always inside [windowStart, now] (the caller filters), and
// the bucket list spans bucketStart(windowStart)..bucketStart(now), so the
// result is in range by construction; the clamp is belt-and-braces against a
// sub-second clock skew at the `now` boundary.
func bucketOffset(first, at time.Time, kind bucketKind) int {
	var n int
	switch kind {
	case bucketWeek:
		n = int(at.Sub(first) / (7 * 24 * time.Hour))
	case bucketMonth:
		n = (at.Year()-first.Year())*12 + int(at.Month()) - int(first.Month())
	default:
		n = int(at.Sub(first) / (24 * time.Hour))
	}
	if n < 0 {
		return 0
	}
	return n
}

// countIn counts the timestamps inside [start, now] (inclusive at both ends:
// the window's last instant is "now", and a stamp exactly at now is today).
func countIn(times []time.Time, start, now time.Time) int {
	n := 0
	for _, t := range times {
		t = t.UTC()
		if !t.Before(start) && !t.After(now) {
			n++
		}
	}
	return n
}

// countFuture counts commits dated after the projection instant (excluded
// from every window; surfaced so the page can explain a scan > window total).
func countFuture(commits []git.InsightsCommit, now time.Time) int {
	n := 0
	for _, c := range commits {
		if c.Committed.After(now) {
			n++
		}
	}
	return n
}

// parseStamp parses one sidecar timestamp; a bad stamp is dropped (the
// projection never invents a date).
func parseStamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
