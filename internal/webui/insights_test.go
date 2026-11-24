// insights_test.go — the rendered Insights page contract: one h1, server-side
// period links, honest empty states, the no-JS bar tables, the traffic refusal,
// and the gate/refresh matrix. Every assertion is on the rendered HTML; the
// projection itself is computed and tested in internal/insights.
package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/server/auth"
)

var insightNow = time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)

func bindInsights(t *testing.T, load func(context.Context, string, string) (*insights.Projection, bool, error),
	refresh func(context.Context, string, string) error, role identity.Role) {
	t.Helper()
	old := repoDeps
	repoDeps = RepoDeps{
		Insights:        load,
		InsightsRefresh: refresh,
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role {
			return role
		},
	}
	t.Cleanup(func() { repoDeps = old })
}

// sampleProjection carries two buckets per window (the real projection has
// 30/13/13; the page only iterates what is there).
func sampleProjection(head string) *insights.Projection {
	buckets := func(n int) []insights.Bucket {
		return []insights.Bucket{
			{Start: "2026-11-20T00:00:00Z", Label: "Nov 20", Commits: 2, Additions: 6, Deletions: 1},
			{Start: "2026-11-21T00:00:00Z", Label: "Nov 21", Commits: 0, Additions: 0, Deletions: 0},
		}[:n]
	}
	w := func(days int) *insights.Window {
		return &insights.Window{
			Days: days, Bucket: "day", Start: "2026-10-23T00:00:00Z", End: "2026-11-21T15:00:00Z",
			Commits: 2, Additions: 6, Deletions: 1,
			Buckets: buckets(2),
			Contributors: []insights.Contributor{
				{Name: "Alice", Email: "alice@example.com", Commits: 2, LastActive: "2026-11-20T10:00:00Z"},
			},
			ContributorsTotal: 1,
			PRsOpened:         1, PRsMerged: 1, IssuesOpened: 1, IssuesClosed: 0,
		}
	}
	p := &insights.Projection{
		Version:    insights.Version,
		ComputedAt: insightNow.Format(time.RFC3339),
		HeadSHA:    head,
		Windows: map[string]*insights.Window{
			"30": w(30), "90": w(90), "365": w(365),
		},
		Scan:   insights.ScanInfo{Commits: 2, MaxCommits: 20000, Since: "2025-11-22T00:00:00Z"},
		Collab: insights.CollabInfo{Known: true},
		Facts: insights.Facts{
			Stars: 7, Watchers: 2, Forks: 3, Social: true,
			Releases: 2, ReleasesKnown: true,
			Packages: 1, PackagesKnown: true,
			ThreadsRuns: 4, ThreadsRunsKnown: true,
		},
	}
	return p
}

func countTag(t *testing.T, body, tag string) int {
	t.Helper()
	n := 0
	walk(parsePage(t, body), func(x *html.Node) {
		if x.Data == tag {
			n++
		}
	})
	return n
}

// sectionByLabel returns the first <section> whose aria-label is label.
func sectionByLabel(t *testing.T, doc *html.Node, label string) *html.Node {
	t.Helper()
	var out *html.Node
	walk(doc, func(n *html.Node) {
		if out == nil && n.Data == "section" && attr(n, "aria-label") == label {
			out = n
		}
	})
	if out == nil {
		t.Fatalf("no section labeled %q", label)
	}
	return out
}

// nodesWithAttr collects every node carrying name="value".
func nodesWithAttr(n *html.Node, name, value string) []*html.Node {
	var out []*html.Node
	walk(n, func(x *html.Node) {
		if attr(x, name) == value {
			out = append(out, x)
		}
	})
	return out
}

// hasAncestor reports whether n sits inside an element with the given tag.
func hasAncestor(n *html.Node, tag string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return true
		}
	}
	return false
}

// countTagIn counts elements with tag in one subtree.
func countTagIn(n *html.Node, tag string) int {
	count := 0
	walk(n, func(x *html.Node) {
		if x.Data == tag {
			count++
		}
	})
	return count
}

// textOf concatenates the rendered text of a subtree.
func textOf(n *html.Node) string {
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return b.String()
}

// barsOf returns the chart bars of one kind, in bucket order.
func barsOf(t *testing.T, body, kind string) []*html.Node {
	t.Helper()
	var out []*html.Node
	walk(parsePage(t, body), func(n *html.Node) {
		if n.Data == "span" && attr(n, "data-bar") == kind {
			out = append(out, n)
		}
	})
	return out
}

// bucketSeries builds n consecutive UTC days with activity only in bucket
// active (0-based), so the chart's per-bucket scale is observable.
func bucketSeries(n, active, commits, additions, deletions int) []insights.Bucket {
	start := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	out := make([]insights.Bucket, 0, n)
	for i := 0; i < n; i++ {
		day := start.AddDate(0, 0, i)
		b := insights.Bucket{Start: day.Format(time.RFC3339), Label: day.Format("Jan 2")}
		if i == active {
			b.Commits, b.Additions, b.Deletions = commits, additions, deletions
		}
		out = append(out, b)
	}
	return out
}

// projectionWithBuckets rewrites the 30-day window of the sample projection.
func projectionWithBuckets(head string, buckets []insights.Bucket, commits, additions, deletions int) *insights.Projection {
	p := sampleProjection(head)
	w := p.Windows["30"]
	w.Buckets = buckets
	w.Commits, w.Additions, w.Deletions = commits, additions, deletions
	return p
}

func newInsightsFixture(t *testing.T, role identity.Role) *repoFixture {
	t.Helper()
	f := newRepoFixture(t, true)
	f.principal = auth.Principal{Name: "alice@example.com"}
	f.view.summary = api.SummaryData{
		Head:     &api.Ref{Name: "refs/heads/main", SHA: strings.Repeat("a", 40)},
		Branches: 1, Health: api.RepoHealthHealthy,
	}
	bindInsights(t, func(_ context.Context, owner, repo string) (*insights.Projection, bool, error) {
		if owner != "demo" || repo != "spindle" {
			return nil, false, nil
		}
		return sampleProjection(f.view.summary.Head.SHA), true, nil
	}, func(context.Context, string, string) error { return nil }, role)
	return f
}

func TestRepoInsightsRendersPulse(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)

	if got := countTag(t, body, "h1"); got != 1 {
		t.Fatalf("h1 count = %d, want exactly 1 (the masthead)", got)
	}
	if got := len(tabsOf(t, body)); got != 10 {
		t.Fatalf("tab count = %d, want 10 (no Checks summary is bound)", got)
	}
	if got := activeTabOf(t, body); got != "insights" {
		t.Fatalf("active tab = %q", got)
	}
	// Period links are server-rendered anchors with the active one marked.
	for _, want := range []string{`href="?window=30"`, `href="?window=90"`, `href="?window=365"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing period link %s", want)
		}
	}
	if !strings.Contains(body, `aria-current="page"`) {
		t.Error("active period link lacks aria-current")
	}
	// Pulse numbers and the contributor row.
	for _, want := range []string{"Pulse", ">2<", "Alice", "alice@example.com", "+6", "−1", "PRs merged"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Facts and the traffic refusal.
	for _, want := range []string{"Facts the bucket knows", "Traffic is not measured", "clone, fetch and view counts do not exist"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Bars are server-rendered heights; html/template must not have sanitized
	// the CSS context to ZgotmplZ.
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatal("template escaped a style value to ZgotmplZ")
	}
	if !strings.Contains(body, "height: 100%") {
		t.Error("commit bar height missing")
	}
	// The chart is the primary surface: a caption carries the numbers.
	if !strings.Contains(body, "2 commits across 1 active day of 2; peak Nov 20 with 2 commits.") {
		t.Error("pulse caption missing the chart facts")
	}
	// No page-level script; the shared base scripts are the only ones.
	if strings.Contains(body, "<script>") {
		t.Error("insights page added an inline script")
	}
}

func TestRepoInsightsWindowSelection(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	body := f.mustGet("/demo/spindle/insights?window=90", http.StatusOK)
	if !strings.Contains(body, "last 90 days") {
		t.Error("window=90 did not select the 90-day window")
	}
	// Invalid values fall back to the default, never an error page.
	body = f.mustGet("/demo/spindle/insights?window=7", http.StatusOK)
	if !strings.Contains(body, "last 30 days") {
		t.Error("window=7 did not fall back to 30")
	}
}

func TestRepoInsightsStaleBanner(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	stale := sampleProjection(strings.Repeat("b", 40))
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return stale, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "stale") || !strings.Contains(body, "computed at another head") {
		t.Error("stale projection did not warn")
	}
}

func TestRepoInsightsEmptyStates(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	// A window with no commits/PRs/issues: explicit sentences, not empty charts.
	empty := sampleProjection(strings.Repeat("a", 40))
	for _, w := range empty.Windows {
		w.Commits, w.Additions, w.Deletions = 0, 0, 0
		w.Buckets = []insights.Bucket{}
		w.Contributors = []insights.Contributor{}
		w.ContributorsTotal = 0
		w.PRsOpened, w.PRsMerged, w.IssuesOpened, w.IssuesClosed = 0, 0, 0, 0
	}
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return empty, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	for _, want := range []string{
		"No commits in this window.",
		"No code frequency data in this window.",
		"No pull requests or issues in this window.",
		"No commit authors in this window.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty state missing %q", want)
		}
	}
	if strings.Contains(body, "<figure") || strings.Contains(body, "data-bar=") {
		t.Error("an empty window rendered a chart instead of the sentence")
	}
	// A repo with one commit must still render (the chart has one row).
	one := sampleProjection(strings.Repeat("a", 40))
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return one, true, nil
	}
	if body := f.mustGet("/demo/spindle/insights", http.StatusOK); !strings.Contains(body, "Pulse") {
		t.Error("one-commit projection did not render")
	}
}

func TestRepoInsightsUnavailableAndUncomputed(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return nil, false, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "Not computed yet") {
		t.Error("missing not-computed state")
	}
	if strings.Contains(body, "/insights/refresh") {
		t.Error("a reader must not get the refresh control")
	}
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return nil, false, errors.New("bucket down")
	}
	body = f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "could not be read") {
		t.Error("read error not surfaced")
	}
	// A nil seam renders the unavailable card, never a panic.
	repoDeps.Insights = nil
	body = f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "not available on this build") {
		t.Error("nil seam not reported")
	}
}

func TestRepoInsightsEmptyRepositoryGuide(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	called := false
	bindInsights(t, func(context.Context, string, string) (*insights.Projection, bool, error) {
		called = true
		return nil, false, nil
	}, nil, identity.RoleRead)
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "push") {
		t.Error("empty repo should render the clone guide")
	}
	if called {
		t.Error("empty repo must not read a projection")
	}
}

func TestRepoInsightsPermissionMatrix(t *testing.T) {
	// Anonymous + anonymous_read off → 401 (the gated() rule).
	f := newInsightsFixture(t, identity.RoleRead)
	f.principal = auth.Anonymous()
	f.rebind()
	f.s = f.newServerWithAnon(false)
	if code, _ := f.get("/demo/spindle/insights"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous anonRead=false = %d, want 401", code)
	}
	// A private repo is invisible (404) even when anonymous_read is on.
	f = newInsightsFixture(t, identity.RoleRead)
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}
	}
	f.rebind()
	if code, body := f.get("/demo/spindle/insights"); code != http.StatusNotFound || strings.Contains(body, "Pulse") {
		t.Fatalf("private repo = %d (must be a 404 with no projection)", code)
	}
	// A public verdict admits the anonymous caller.
	f = newInsightsFixture(t, identity.RoleRead)
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError { return nil }
	f.rebind()
	if code, _ := f.get("/demo/spindle/insights"); code != http.StatusOK {
		t.Fatalf("public repo = %d, want 200", code)
	}
}

func TestRepoInsightsRefreshMatrix(t *testing.T) {
	// Reader: no form, POST is 403.
	f := newInsightsFixture(t, identity.RoleRead)
	if strings.Contains(f.mustGet("/demo/spindle/insights", http.StatusOK), "Refresh now") {
		t.Error("reader sees the refresh control")
	}
	rec := parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{"window": {"90"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader POST = %d, want 403", rec.Code)
	}
	// Writer: form + 303 with the flash and the repo id passed through.
	calls := 0
	got := ""
	f = newInsightsFixture(t, identity.RoleWrite)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps.InsightsRefresh = func(_ context.Context, owner, repo string) error {
		calls++
		got = owner + "/" + repo
		return nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "Refresh now") || !strings.Contains(body, "/demo/spindle/insights/refresh") {
		t.Error("writer missing the refresh form")
	}
	rec = parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{"window": {"90"}}, nil)
	parityWantLocation(t, rec, "/demo/spindle/insights?window=90&flash=Insights+refresh+started")
	if calls != 1 || got != "demo/spindle" {
		t.Fatalf("refresh seam calls=%d repo=%q", calls, got)
	}
	// A seam failure is a 503, not a silent redirect.
	repoDeps.InsightsRefresh = func(context.Context, string, string) error { return errors.New("no tasks") }
	rec = parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{"window": {"30"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("seam failure = %d, want 503", rec.Code)
	}
	// Cross-origin posts are refused.
	repoDeps.InsightsRefresh = func(context.Context, string, string) error { return nil }
	rec = parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{}, map[string]string{"Origin": "http://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", rec.Code)
	}
	// Anonymous (anonRead on) gets the login interstitial, never a run.
	f = newInsightsFixture(t, identity.RoleRead)
	f.principal = auth.Anonymous()
	rec = parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{}, map[string]string{"Origin": "http://example.com"})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/login-required") {
		t.Fatalf("anonymous refresh = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func (f *repoFixture) newServerWithAnon(anon bool) *Server {
	cfg := &config.Config{}
	cfg.Server.Auth.AnonymousRead = anon
	cfg.Server.PublicURL = "http://example.test"
	return f.newServer(cfg)
}

func TestRepoInsightsNoCrossRepoLeak(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	seen := ""
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps.Insights = func(_ context.Context, owner, repo string) (*insights.Projection, bool, error) {
		seen = owner + "/" + repo
		p := sampleProjection(strings.Repeat("a", 40))
		p.Facts.Stars = 999 // would be a visible leak if the wrong repo rendered
		if owner == "demo" && repo == "spindle" {
			p.Facts.Stars = 3
		}
		return p, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	if seen != "demo/spindle" {
		t.Fatalf("projection read for %q, want demo/spindle", seen)
	}
	if !strings.Contains(body, ">3<") || strings.Contains(body, "999") {
		t.Fatal("projection values crossed repositories")
	}
}

func TestRepoInsightsHonestyNotesAndUnknownWindow(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	p := sampleProjection(strings.Repeat("a", 40))
	p.Scan.Truncated = true
	p.Scan.MaxCommits = 5000
	p.Scan.FutureDated = 2
	p.Collab = insights.CollabInfo{Known: true, IndexBounded: true, MergeScanCapped: true}
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return p, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	for _, want := range []string{
		"most recent 5000 commits",
		"compaction watermark",
		"most recent 200 closed pull requests",
		"dated after the projection instant",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("honesty note missing %q", want)
		}
	}
	// Unknown collab read says unavailable, never zero.
	p.Collab = insights.CollabInfo{Known: false}
	body = f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "unavailable right now") {
		t.Error("unknown collab read not surfaced")
	}
	// A projection without the selected window renders the not-computed state
	// (fail closed, never a half page).
	delete(p.Windows, "30")
	body = f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "Not computed yet") {
		t.Error("missing-window projection did not fail closed")
	}
	// A projection older than 24 h is stale even with the same head.
	p = sampleProjection(strings.Repeat("a", 40))
	p.ComputedAt = insightNow.Add(-25 * time.Hour).Format(time.RFC3339)
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return p, true, nil
	}
	body = f.mustGet("/demo/spindle/insights", http.StatusOK)
	if !strings.Contains(body, "more than 24 hours old") {
		t.Error("TTL staleness not surfaced")
	}
}

func TestRepoInsightsZerosAreDetailOnDemand(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	doc := parsePage(t, body)
	for _, label := range []string{"Repository pulse", "Code frequency"} {
		section := sectionByLabel(t, doc, label)
		// The sample window has two buckets, one of them active. The primary
		// table holds the active one; the zero bucket is detail on demand.
		active := nodesWithAttr(section, "data-bucket-row", "active")
		all := nodesWithAttr(section, "data-bucket-row", "all")
		if len(active) != 1 {
			t.Errorf("%s: primary rows = %d, want 1 (active buckets only)", label, len(active))
		}
		if len(all) != 2 {
			t.Errorf("%s: full-table rows = %d, want 2", label, len(all))
		}
		for _, row := range active {
			if hasAncestor(row, "details") {
				t.Errorf("%s: an active row is hidden inside <details>", label)
			}
			if strings.Contains(textOf(row), "Nov 21") {
				t.Errorf("%s: the zero bucket leaked into the primary table", label)
			}
		}
		for _, row := range all {
			if !hasAncestor(row, "details") {
				t.Errorf("%s: a full-table row lives outside <details>", label)
			}
		}
		if got := countTagIn(section, "details"); got != 1 {
			t.Errorf("%s: details count = %d, want 1", label, got)
		}
		if !strings.Contains(textOf(section), "All 2 periods (including empty ones)") {
			t.Errorf("%s: details summary missing the all-periods label", label)
		}
	}
}

func TestRepoInsightsPulseChartScalesToPeak(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	// The complaint's shape: 30 buckets, one commit in one of them.
	p := projectionWithBuckets(strings.Repeat("a", 40), bucketSeries(30, 28, 1, 0, 0), 1, 0, 0)
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return p, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)

	// Commits without line changes keep the honest sentence, never an empty
	// second chart.
	if got := countTag(t, body, "figure"); got != 1 {
		t.Fatalf("figure count = %d, want 1 (pulse only)", got)
	}
	if !strings.Contains(body, "Commits in this window carry no line changes") {
		t.Error("missing the no-line-changes sentence")
	}
	bars := barsOf(t, body, "commit")
	if len(bars) != 30 {
		t.Fatalf("commit bars = %d, want 30", len(bars))
	}
	peak, flat := 0, 0
	for _, bar := range bars {
		switch style := attr(bar, "style"); style {
		case "height: 100%":
			peak++
		case "height: 0%":
			flat++
		default:
			t.Errorf("unexpected bar height %q", style)
		}
	}
	if peak != 1 || flat != 29 {
		t.Fatalf("peak bars = %d, flat bars = %d; want 1 and 29", peak, flat)
	}
	if !strings.Contains(body, "1 commit across 1 active day of 30; peak Nov 20 with 1 commit.") {
		t.Error("caption does not state the window's facts")
	}
	// The full 30-row table lives in <details>; the primary table has one row.
	doc := parsePage(t, body)
	pulse := sectionByLabel(t, doc, "Repository pulse")
	if got := len(nodesWithAttr(pulse, "data-bucket-row", "active")); got != 1 {
		t.Fatalf("primary pulse rows = %d, want 1", got)
	}
	if got := len(nodesWithAttr(pulse, "data-bucket-row", "all")); got != 30 {
		t.Fatalf("full pulse rows = %d, want 30", got)
	}
	if !strings.Contains(body, "All 30 periods (including empty ones)") {
		t.Error("missing the 30-period details label")
	}
}

func TestPluralAndActivePeriods(t *testing.T) {
	t.Parallel()
	if got := plural(1, "commit", "commits"); got != "commit" {
		t.Fatalf("plural(1) = %q", got)
	}
	if got := plural(2, "commit", "commits"); got != "commits" {
		t.Fatalf("plural(2) = %q", got)
	}
	if got := activePeriods(2, "week"); got != "2 active weeks" {
		t.Fatalf("activePeriods = %q", got)
	}
	if got := activePeriods(1, "day"); got != "1 active day" {
		t.Fatalf("activePeriods = %q", got)
	}
}

func TestRepoInsightsFrequencyStackStaysInChart(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleRead)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	// The 2% visibility floor on a peak bucket with both segments must not
	// push the stacked column past the chart's 100% line.
	p := projectionWithBuckets(strings.Repeat("a", 40), bucketSeries(30, 29, 1, 1, 99), 1, 1, 99)
	repoDeps.Insights = func(context.Context, string, string) (*insights.Projection, bool, error) {
		return p, true, nil
	}
	body := f.mustGet("/demo/spindle/insights", http.StatusOK)
	add := barsOf(t, body, "additions")
	del := barsOf(t, body, "deletions")
	if len(add) != 1 || len(del) != 1 {
		t.Fatalf("frequency bars: additions=%d deletions=%d, want 1 each", len(add), len(del))
	}
	if !strings.Contains(attr(add[0], "style"), "height: 2%") {
		t.Errorf("addition floor missing: %q", attr(add[0], "style"))
	}
	if !strings.Contains(attr(del[0], "style"), "bottom: 2%") || !strings.Contains(attr(del[0], "style"), "height: 98%") {
		t.Errorf("deletion segment overflowed the chart: %q", attr(del[0], "style"))
	}
}

func TestBarPct(t *testing.T) {
	t.Parallel()
	cases := []struct{ v, max, want int }{
		{0, 10, 0}, {1, 0, 0}, {-1, 10, 0}, {1, 10, 10}, {1, 100, 2}, {5, 10, 50}, {20, 10, 100},
	}
	for _, tc := range cases {
		if got := barPct(tc.v, tc.max); got != tc.want {
			t.Fatalf("barPct(%d,%d) = %d, want %d", tc.v, tc.max, got, tc.want)
		}
	}
}

func TestRepoInsightsRefreshUnwired(t *testing.T) {
	f := newInsightsFixture(t, identity.RoleWrite)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps.InsightsRefresh = nil
	rec := parityPost(t, f, "/demo/spindle/insights/refresh", url.Values{"window": {"30"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired refresh = %d, want 503", rec.Code)
	}
}
