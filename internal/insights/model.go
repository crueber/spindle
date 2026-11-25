// Package insights computes the repository Insights projection
// (docs/features/18_repo_insights.md): pulse, contributors, code frequency and
// the bucket facts a maintainer can honestly state about a repository.
//
// The bucket is the only database and the projection is the only state:
//
//	repos/<o>/<r>/insights/pulse.json    CAS'd, versioned, fully rebuildable
//
// A page read is ONE exact-key GET — no git, no LIST (law 4). The expensive
// part (one bounded `git log --shortstat`, plus bounded reads of the existing
// index/card sidecars) runs as a narrated task (law 7): the maintainer sweeper
// refreshes stale projections on its cadence, and a repository writer can start
// the same task from the page. A projection is stamped with the head sha it was
// computed at, so staleness is an equality check, not a guess.
//
// Nothing here is a new metric invented for the page. Clone/view traffic does
// NOT exist (there is no git access log), so the page says so instead of
// fabricating a number. The facts that do exist are read from their owning
// families: meta/social.json (stars/watchers/forks), release headers, package
// tags and Threads run records.
package insights

import (
	"strings"
	"time"
)

// Version is the pulse.json schema version; readers refuse unknown versions
// (the same fail-closed shape as policy.json and meta/stats.json).
const Version = 1

// DefaultWindow is the page's default period in days (30 / 90 / 365 are the
// only selectable windows; the projection carries all three).
const DefaultWindow = 30

// MaxContributors caps each window's contributor list. The window carries the
// true distinct count so the page can say "showing 50 of N".
const MaxContributors = 50

// DefaultMaxCommits bounds one history scan. A window with more commits than
// this is reported truncated in the UI, never silently sampled.
const DefaultMaxCommits = 20000

// DefaultMergeScanCap bounds the closed-PR sidecar reads one refresh performs
// to distinguish merged from closed-unmerged (the shared index card carries
// no merge flag). Past the cap the page states that merged counts cover the
// most recent window only.
const DefaultMergeScanCap = 200

// DefaultFactCap bounds each fact LIST (releases, package tags, Threads runs).
const DefaultFactCap = 100

// DefaultTTL is how long a projection stays fresh when the head has not moved.
const DefaultTTL = 24 * time.Hour

// TaskKind is the Seam 5 task kind recorded in the task table.
const TaskKind = "insights-refresh"

// Windows lists the selectable windows in page order.
var Windows = []int{30, 90, 365}

// Key returns repos/<o>/<r>/insights/pulse.json.
func Key(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/insights/pulse.json"
}

// Projection is the cached pulse.json body. All timestamps are RFC 3339 UTC;
// all bucketing is UTC (the page says so).
type Projection struct {
	Version    int                `json:"version"`
	ComputedAt string             `json:"computed_at"`
	HeadSHA    string             `json:"head_sha"`
	Windows    map[string]*Window `json:"windows"`
	Scan       ScanInfo           `json:"scan"`
	Collab     CollabInfo         `json:"collab"`
	Facts      Facts              `json:"facts"`
	// Languages is the committed-bytes language mix at HeadSHA (the Code
	// pulse card's fourth row). It is additive and omitempty: a projection
	// written before the field existed decodes with no rows and the card
	// hides only the language row. The estimate counts committed bytes, not
	// lines (18_repo_insights.md §3.7).
	Languages []Language `json:"languages,omitempty"`
	// LanguagesCapped is true when the tree held more regular blobs than the
	// scan counted; the mix is then explicitly bounded in the UI.
	LanguagesCapped bool `json:"languages_capped,omitempty"`
}

// Language is one row of the committed-bytes language mix. Pct is a whole
// number; the rows sum to 100 (the largest row absorbs the rounding
// residual). Ordering is bytes desc, name asc.
type Language struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Pct   int    `json:"pct"`
}

// ScanInfo records what the one git invocation covered.
type ScanInfo struct {
	Commits     int    `json:"commits"`      // commits parsed into the projection
	MaxCommits  int    `json:"max_commits"`  // the cap the scan ran with
	Truncated   bool   `json:"truncated"`    // the window held more than the cap
	FutureDated int    `json:"future_dated"` // commits dated after computed_at (excluded)
	Since       string `json:"since"`        // the --since floor (RFC 3339 UTC)
}

// CollabInfo records the provenance of the issue/PR counts.
type CollabInfo struct {
	// Known is false when the shared list index could not be read (the page
	// says "unavailable", never "zero").
	Known bool `json:"known"`
	// IndexBounded is true when the shared index has a compaction watermark:
	// threads older than it are not in the index and are not counted.
	IndexBounded bool `json:"index_bounded"`
	// MergeScanCapped is true when more closed PRs fell in the window than
	// the sidecar cap allowed us to classify.
	MergeScanCapped bool `json:"merge_scan_capped"`
	// PRSidecarReads is the number of pr.json reads the refresh performed
	// (bounded by DefaultMergeScanCap); an honest cost receipt.
	PRSidecarReads int `json:"pr_sidecar_reads"`
}

// Window is one selectable period's slice of the projection.
type Window struct {
	Days    int    `json:"days"`
	Bucket  string `json:"bucket"` // day | week | month
	Start   string `json:"start"`  // first bucket start (RFC 3339 UTC)
	End     string `json:"end"`    // computed-at instant (RFC 3339 UTC)
	Commits int    `json:"commits"`
	// Additions/Deletions are the --shortstat totals for the window.
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`

	Buckets            []Bucket      `json:"buckets"`
	Contributors       []Contributor `json:"contributors"`
	ContributorsTotal  int           `json:"contributors_total"`
	ContributorsCapped bool          `json:"contributors_capped"`

	PRsOpened    int `json:"prs_opened"`
	PRsMerged    int `json:"prs_merged"`
	IssuesOpened int `json:"issues_opened"`
	IssuesClosed int `json:"issues_closed"`
}

// Bucket is one period row (a UTC day, a Monday-start week, or a calendar
// month). Start is inclusive; the next bucket's start is the exclusive end.
type Bucket struct {
	Start     string `json:"start"` // first instant, RFC 3339 UTC
	Label     string `json:"label"` // display label, English
	Commits   int    `json:"commits"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// Contributor is one distinct author in a window. Identity is the lowercased
// email (a person may commit under several names); Name is the newest name
// seen for that email.
type Contributor struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Commits    int    `json:"commits"`
	LastActive string `json:"last_active"` // RFC 3339 UTC
}

// Facts are the repository facts the bucket actually knows. Each *Known flag
// distinguishes "verified zero" from "could not read"; Capped marks a count
// that stopped at the cap.
type Facts struct {
	Stars    int  `json:"stars"`
	Watchers int  `json:"watchers"`
	Forks    int  `json:"forks"`
	Social   bool `json:"social_known"`

	Releases       int  `json:"releases"`
	ReleasesKnown  bool `json:"releases_known"`
	ReleasesCapped bool `json:"releases_capped"`
	// LastReleaseAt is the newest published release's created_at from the
	// releases/latest.json pointer (empty when the family has no pointer).
	// "Releases" is a count; this is the one stamp the pulse card needs for
	// "Last release 3mo ago". Additive and omitempty.
	LastReleaseAt string `json:"last_release_at,omitempty"`

	Packages       int  `json:"packages"`
	PackagesKnown  bool `json:"packages_known"`
	PackagesCapped bool `json:"packages_capped"`

	ThreadsRuns       int  `json:"threads_runs"`
	ThreadsRunsKnown  bool `json:"threads_runs_known"`
	ThreadsRunsCapped bool `json:"threads_runs_capped"`
}

// Logger narrates one refresh (the maintain.TaskLogger / wal.Task shape).
// Nil is safe: the refresh is silent then.
type Logger interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// Stale reports whether the projection must be recomputed for head at now.
// Equality on the head sha is the primary gate; the TTL refreshes a projection
// whose head did not move (so contributors/facts do not drift forever).
func Stale(p *Projection, head string, now time.Time, ttl time.Duration) bool {
	if p == nil || p.Version != Version {
		return true
	}
	if p.HeadSHA != head {
		return true
	}
	at, err := time.Parse(time.RFC3339, p.ComputedAt)
	if err != nil {
		return true
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return now.UTC().Sub(at.UTC()) > ttl
}

// bucketKind is the bucketing shape for one window.
type bucketKind int

const (
	bucketDay bucketKind = iota
	bucketWeek
	bucketMonth
)

func (k bucketKind) String() string {
	switch k {
	case bucketWeek:
		return "week"
	case bucketMonth:
		return "month"
	default:
		return "day"
	}
}

// specFor maps a selectable window to its bucketing.
func specFor(days int) bucketKind {
	switch days {
	case 90:
		return bucketWeek
	case 365:
		return bucketMonth
	default:
		return bucketDay
	}
}

// dayStart truncates to UTC midnight.
func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// bucketStart is the first instant of t's bucket in UTC.
func bucketStart(t time.Time, kind bucketKind) time.Time {
	t = dayStart(t)
	switch kind {
	case bucketWeek:
		// ISO weeks start Monday; Go's Sunday=0 needs the +6 shift.
		off := (int(t.Weekday()) + 6) % 7
		return t.AddDate(0, 0, -off)
	case bucketMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return t
	}
}

// windowStart is the inclusive first day of a "last N days" window: today and
// the N-1 days before it (UTC).
func windowStart(now time.Time, days int) time.Time {
	return dayStart(now).AddDate(0, 0, -(days - 1))
}

// nextBucket advances one bucket.
func nextBucket(t time.Time, kind bucketKind) time.Time {
	switch kind {
	case bucketWeek:
		return t.AddDate(0, 0, 7)
	case bucketMonth:
		return t.AddDate(0, 1, 0)
	default:
		return t.AddDate(0, 0, 1)
	}
}

// bucketLabel renders the English display label (the UI is English).
func bucketLabel(t time.Time, kind bucketKind) string {
	switch kind {
	case bucketWeek:
		return "week of " + t.Format("Jan 2")
	case bucketMonth:
		return t.Format("Jan 2006")
	default:
		return t.Format("Jan 2")
	}
}

// scanFloor is the earliest instant any window needs. The one git scan is
// taken at this floor and every window filters from it.
func scanFloor(now time.Time) time.Time {
	earliest := windowStart(now, Windows[0])
	for _, d := range Windows[1:] {
		if s := windowStart(now, d); s.Before(earliest) {
			earliest = s
		}
	}
	return earliest
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ValidateWindow normalizes the page's ?window= value.
func ValidateWindow(days int) int {
	for _, w := range Windows {
		if days == w {
			return w
		}
	}
	return DefaultWindow
}

// ParseWindow parses the page query value, falling back to the default.
func ParseWindow(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return DefaultWindow
		}
		n = n*10 + int(s[i]-'0')
		if n > 1_000_000 {
			return DefaultWindow
		}
	}
	if s == "" {
		return DefaultWindow
	}
	return ValidateWindow(n)
}

// normalizeEmail is the contributor identity.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
