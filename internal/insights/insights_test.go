// insights_test.go — the pure aggregation math: UTC bucketing, window
// boundaries, contributor identity and the collaboration timeline. I/O lives
// in refresh_test.go.
package insights

import (
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
)

var testNow = time.Date(2026, 11, 21, 15, 4, 0, 0, time.UTC) // Saturday

func commit(at time.Time, email, name string, add, del int) git.InsightsCommit {
	return git.InsightsCommit{
		ID:          "c" + at.Format(time.RFC3339Nano),
		AuthorName:  name,
		AuthorEmail: email,
		Committed:   at.UTC(),
		Additions:   add,
		Deletions:   del,
	}
}

func TestParseWindow(t *testing.T) {
	cases := map[string]int{
		"": 30, "30": 30, "90": 90, "365": 365,
		"7": 30, "0": 30, "-1": 30, "abc": 30, "9999999999999": 30,
	}
	for in, want := range cases {
		if got := ParseWindow(in); got != want {
			t.Fatalf("ParseWindow(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestKey(t *testing.T) {
	if got, want := Key("acme", "api"), "repos/acme/api/insights/pulse.json"; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}

func TestBucketMath(t *testing.T) {
	// 30 daily buckets, inclusive of today.
	start := windowStart(testNow, 30)
	if want := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("30d start = %v, want %v", start, want)
	}
	w30 := buildWindow(nil, testNow, 30, collabTimes{})
	if len(w30.Buckets) != 30 {
		t.Fatalf("30d buckets = %d", len(w30.Buckets))
	}
	if w30.Buckets[0].Label != "Oct 23" || w30.Buckets[29].Label != "Nov 21" {
		t.Fatalf("labels = %q .. %q", w30.Buckets[0].Label, w30.Buckets[29].Label)
	}
	// Weeks are Monday-start; the 90d span starts on the Monday on/before the
	// 89-day-old day, so it is exactly 13 buckets for this Saturday.
	w90 := buildWindow(nil, testNow, 90, collabTimes{})
	if len(w90.Buckets) != 13 {
		t.Fatalf("90d buckets = %d: %+v", len(w90.Buckets), labelList(w90.Buckets))
	}
	if w90.Buckets[0].Start != "2026-08-24T00:00:00Z" {
		t.Fatalf("first week = %s", w90.Buckets[0].Start)
	}
	if w90.Buckets[12].Start != "2026-11-16T00:00:00Z" {
		t.Fatalf("last week = %s", w90.Buckets[12].Start)
	}
	// Months are calendar months; a 365-day window spanning November starts
	// in the partial month of last November.
	w365 := buildWindow(nil, testNow, 365, collabTimes{})
	if len(w365.Buckets) != 13 {
		t.Fatalf("365d buckets = %d: %+v", len(w365.Buckets), labelList(w365.Buckets))
	}
	if w365.Buckets[0].Label != "Nov 2025" || w365.Buckets[12].Label != "Nov 2026" {
		t.Fatalf("month labels = %q .. %q", w365.Buckets[0].Label, w365.Buckets[12].Label)
	}
	if w90.Bucket != "week" || w365.Bucket != "month" || w30.Bucket != "day" {
		t.Fatalf("bucket kinds = %q/%q/%q", w30.Bucket, w90.Bucket, w365.Bucket)
	}
}

func labelList(bs []Bucket) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Label)
	}
	return out
}

func TestWindowBoundariesAndBucketing(t *testing.T) {
	start := windowStart(testNow, 30)
	commits := []git.InsightsCommit{
		// Exactly at the inclusive start: first bucket.
		commit(start, "a@x", "A", 3, 1),
		// One second too old: excluded.
		commit(start.Add(-time.Second), "a@x", "A", 100, 100),
		// A timestamp with a +02:00 offset normalizes into the previous UTC
		// day (Nov 2 01:00+02:00 = Nov 1 23:00Z).
		commit(time.Date(2026, 11, 2, 1, 0, 0, 0, time.FixedZone("E", 2*3600)), "a@x", "A", 2, 0),
		// Same UTC day, later: same bucket.
		commit(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), "b@x", "B", 1, 2),
		// Future-dated (clock skew) lands in the current bucket, not dropped.
		commit(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), "c@x", "C", 5, 5),
	}
	w := buildWindow(commits, testNow, 30, collabTimes{})
	if w.Commits != 3 {
		t.Fatalf("commits = %d, want 3 (too-old and future-dated commits are excluded)", w.Commits)
	}
	if w.Buckets[0].Commits != 1 || w.Buckets[0].Additions != 3 {
		t.Fatalf("first bucket = %+v", w.Buckets[0])
	}
	// Nov 1 is index 9 (Oct 23 is 0).
	if w.Buckets[9].Commits != 2 || w.Buckets[9].Additions != 3 || w.Buckets[9].Deletions != 2 {
		t.Fatalf("Nov 1 bucket = %+v", w.Buckets[9])
	}
	if w.Buckets[29].Commits != 0 {
		t.Fatalf("future commit leaked into the current bucket: %+v", w.Buckets[29])
	}
	if w.Additions != 6 || w.Deletions != 3 {
		t.Fatalf("totals = %d/%d", w.Additions, w.Deletions)
	}
	if got := countFuture(commits, testNow); got != 1 {
		t.Fatalf("future-dated = %d, want 1", got)
	}
}

func TestWeekAndMonthEdges(t *testing.T) {
	// Sunday 23:59 and Monday 00:00 are different ISO weeks.
	sun := time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC) // Sunday
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	w := buildWindow([]git.InsightsCommit{commit(sun, "a@x", "A", 1, 0), commit(mon, "a@x", "A", 1, 0)}, testNow, 90, collabTimes{})
	var count int
	for _, b := range w.Buckets {
		count += b.Commits
	}
	if w.Commits != 2 || count != 2 {
		t.Fatalf("commits = %d buckets=%d", w.Commits, count)
	}
	sunStart := bucketStart(sun, bucketWeek)
	monStart := bucketStart(mon, bucketWeek)
	if sunStart.Equal(monStart) {
		t.Fatal("Sunday and Monday must be different ISO weeks")
	}

	// Month edge: last instant of October vs first of November.
	oct := time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	m := buildWindow([]git.InsightsCommit{commit(oct, "a@x", "A", 1, 0), commit(nov, "a@x", "A", 1, 0)}, testNow, 365, collabTimes{})
	var octN, novN int
	for _, b := range m.Buckets {
		if b.Label == "Oct 2026" {
			octN = b.Commits
		}
		if b.Label == "Nov 2026" {
			novN = b.Commits
		}
	}
	if octN != 1 || novN != 1 {
		t.Fatalf("oct/nov = %d/%d", octN, novN)
	}
}

func TestContributorIdentityAndOrdering(t *testing.T) {
	commits := []git.InsightsCommit{
		commit(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), "A@X", "Alice", 1, 0),
		commit(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), "a@x", "Alicia", 1, 0),
		commit(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), "b@x", "Bob", 1, 0),
	}
	w := buildWindow(commits, testNow, 30, collabTimes{})
	if w.ContributorsTotal != 2 || len(w.Contributors) != 2 {
		t.Fatalf("contributors = %d/%d: %+v", w.ContributorsTotal, len(w.Contributors), w.Contributors)
	}
	if w.Contributors[0].Email != "a@x" || w.Contributors[0].Commits != 2 {
		t.Fatalf("top = %+v", w.Contributors[0])
	}
	// The newest name wins for a shared email.
	if w.Contributors[0].Name != "Alice" {
		t.Fatalf("name = %q, want the newest (Alice)", w.Contributors[0].Name)
	}
	if w.Contributors[0].LastActive != "2026-11-20T10:00:00Z" {
		t.Fatalf("last active = %q", w.Contributors[0].LastActive)
	}
	if w.Contributors[1].Email != "b@x" {
		t.Fatalf("second = %+v", w.Contributors[1])
	}
}

func TestContributorCap(t *testing.T) {
	var commits []git.InsightsCommit
	for i := 0; i < MaxContributors+10; i++ {
		commits = append(commits, commit(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "u"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@x", "U", 1, 0))
	}
	w := buildWindow(commits, testNow, 30, collabTimes{})
	if w.ContributorsTotal != MaxContributors+10 {
		t.Fatalf("total = %d", w.ContributorsTotal)
	}
	if len(w.Contributors) != MaxContributors || !w.ContributorsCapped {
		t.Fatalf("list = %d capped=%v", len(w.Contributors), w.ContributorsCapped)
	}
}

func TestEmptyWindow(t *testing.T) {
	old := commit(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), "a@x", "A", 1, 0)
	w := buildWindow([]git.InsightsCommit{old}, testNow, 30, collabTimes{})
	if w.Commits != 0 || w.Additions != 0 || w.Deletions != 0 {
		t.Fatalf("old commit leaked into the window: %+v", w)
	}
	if w.Contributors == nil || len(w.Contributors) != 0 || w.ContributorsTotal != 0 {
		t.Fatalf("empty contributors = %+v", w.Contributors)
	}
	for _, b := range w.Buckets {
		if b.Commits != 0 {
			t.Fatalf("bucket not zero: %+v", b)
		}
	}
}

func TestCollabWindowCounts(t *testing.T) {
	start := windowStart(testNow, 30)
	ct := collabTimes{
		known: true,
		prsOpened: []time.Time{
			start, start.Add(-time.Second), testNow, testNow.Add(time.Second),
		},
		prsMerged:    []time.Time{start.Add(24 * time.Hour)},
		issuesOpened: []time.Time{start, start},
		issuesClosed: []time.Time{testNow},
	}
	w := buildWindow(nil, testNow, 30, ct)
	if w.PRsOpened != 2 { // start + now; before-start and after-now excluded
		t.Fatalf("prs opened = %d", w.PRsOpened)
	}
	if w.PRsMerged != 1 {
		t.Fatalf("prs merged = %d", w.PRsMerged)
	}
	if w.IssuesOpened != 2 || w.IssuesClosed != 1 {
		t.Fatalf("issues = %d/%d", w.IssuesOpened, w.IssuesClosed)
	}
}

func TestStale(t *testing.T) {
	now := testNow
	fresh := &Projection{Version: Version, HeadSHA: "abc", ComputedAt: now.Add(-time.Hour).Format(time.RFC3339)}
	if Stale(nil, "abc", now, DefaultTTL) != true {
		t.Fatal("nil must be stale")
	}
	if Stale(fresh, "abc", now, DefaultTTL) {
		t.Fatal("fresh projection reported stale")
	}
	if !Stale(fresh, "def", now, DefaultTTL) {
		t.Fatal("moved head must be stale")
	}
	old := &Projection{Version: Version, HeadSHA: "abc", ComputedAt: now.Add(-25 * time.Hour).Format(time.RFC3339)}
	if !Stale(old, "abc", now, DefaultTTL) {
		t.Fatal("TTL expiry must be stale")
	}
	if !Stale(&Projection{Version: 2, HeadSHA: "abc", ComputedAt: now.Format(time.RFC3339)}, "abc", now, DefaultTTL) {
		t.Fatal("unknown version must be stale")
	}
	bad := &Projection{Version: Version, HeadSHA: "abc", ComputedAt: "not-a-date"}
	if !Stale(bad, "abc", now, DefaultTTL) {
		t.Fatal("unparseable computed_at must be stale")
	}
}

func TestLanguagesFrom(t *testing.T) {
	// Whole-number percentages sum to 100; the largest remainder takes the
	// residual deterministically: 1/3 each rounds to 34/33/33 (bytes order
	// decides the tie, and "Alpha" wins the name tie-break).
	got := languagesFrom([]git.InsightsLanguage{
		{Name: "Beta", Bytes: 1}, {Name: "Alpha", Bytes: 1}, {Name: "Gamma", Bytes: 1},
	})
	if len(got) != 3 {
		t.Fatalf("rows = %+v", got)
	}
	var sum int
	for _, g := range got {
		sum += g.Pct
	}
	if sum != 100 {
		t.Fatalf("percentages sum to %d: %+v", sum, got)
	}
	if got[0].Name != "Alpha" || got[0].Pct != 34 {
		t.Fatalf("largest-remainder winner = %+v", got[0])
	}

	// Zero-byte languages are dropped; an all-empty tree has no mix.
	got = languagesFrom([]git.InsightsLanguage{
		{Name: "Go", Bytes: 0}, {Name: "Other", Bytes: 0},
	})
	if got != nil {
		t.Fatalf("all-empty tree = %+v", got)
	}
	got = languagesFrom([]git.InsightsLanguage{
		{Name: "Go", Bytes: 100}, {Name: "Empty", Bytes: 0},
	})
	if len(got) != 1 || got[0].Pct != 100 {
		t.Fatalf("zero-byte row not dropped: %+v", got)
	}

	// Deterministic output for the same input, twice.
	in := []git.InsightsLanguage{{Name: "Go", Bytes: 84}, {Name: "JavaScript", Bytes: 9}, {Name: "CSS", Bytes: 7}}
	a, b := languagesFrom(in), languagesFrom(in)
	if len(a) != 3 || a[0].Pct != 84 || a[1].Pct != 9 || a[2].Pct != 7 {
		t.Fatalf("mix = %+v", a)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %+v vs %+v", i, a, b)
		}
	}
}
