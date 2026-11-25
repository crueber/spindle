// pulse_test.go — the Code page's "Code pulse" card contract
// (docs/features/18_repo_insights.md §4.1). Every assertion is on the
// rendered HTML: the four rows, the sparkline's accessible label, the exact
// text list, the honesty states, and the "no projection → no card" rule.
package webui

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/server/auth"
)

var pulseNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// pulseProjection is a full, current projection: 3 daily buckets (two active),
// a 90-day window with Alice at 8 of 10 commits, a release stamp and a mix.
func pulseProjection() *insights.Projection {
	buckets := []insights.Bucket{
		{Start: "2026-11-23T00:00:00Z", Label: "Nov 23", Commits: 2},
		{Start: "2026-11-24T00:00:00Z", Label: "Nov 24", Commits: 0},
		{Start: "2026-11-25T00:00:00Z", Label: "Nov 25", Commits: 5},
	}
	w30 := &insights.Window{
		Days: 30, Bucket: "day", Commits: 7, Buckets: buckets,
		Contributors: []insights.Contributor{{Name: "Alice", Email: "a@x", Commits: 7}}, ContributorsTotal: 1,
	}
	w90 := &insights.Window{
		Days: 90, Bucket: "week", Commits: 10,
		Contributors: []insights.Contributor{
			{Name: "Alice", Email: "a@x", Commits: 8},
			{Name: "Bob", Email: "b@x", Commits: 2},
		},
		ContributorsTotal: 3,
	}
	w365 := &insights.Window{Days: 365, Bucket: "month", Commits: 40}
	return &insights.Projection{
		Version:    insights.Version,
		ComputedAt: pulseNow.Add(-12 * time.Minute).Format(time.RFC3339),
		HeadSHA:    strings.Repeat("a", 40),
		Windows:    map[string]*insights.Window{"30": w30, "90": w90, "365": w365},
		Scan:       insights.ScanInfo{Commits: 40, MaxCommits: 20000},
		Collab:     insights.CollabInfo{Known: true},
		Facts: insights.Facts{
			Releases: 2, ReleasesKnown: true,
			LastReleaseAt: "2026-08-25T12:00:00Z",
		},
		Languages: []insights.Language{
			{Name: "Go", Bytes: 840, Pct: 84},
			{Name: "JavaScript", Bytes: 90, Pct: 9},
			{Name: "CSS", Bytes: 50, Pct: 5},
			{Name: "HTML", Bytes: 20, Pct: 2},
		},
	}
}

// bindPulse installs an Insights seam returning p (nil = absent) and fixes the
// request clock so the card's relative strings are deterministic.
func bindPulse(t *testing.T, f *repoFixture, p *insights.Projection, found bool) {
	t.Helper()
	old := repoDeps
	repoDeps = RepoDeps{
		Insights: func(context.Context, string, string) (*insights.Projection, bool, error) {
			return p, found, nil
		},
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead },
	}
	t.Cleanup(func() { repoDeps = old })
	f.s.env.Now = func() time.Time { return pulseNow }
}

// pulseFixture is a Code page with a newest commit at pulseNow - 2 days so the
// cadence row has the page's own head-commit time.
func pulseFixture(t *testing.T, p *insights.Projection, found bool) (*repoFixture, string) {
	t.Helper()
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref:     "refs/heads/main",
		Entries: []api.TreeEntry{{Name: "main.go", Type: "blob", Mode: "100644", Size: 12}},
	}
	f.view.commits = api.CommitPage{SHA: strings.Repeat("a", 40), Commits: []api.Commit{{
		SHA: strings.Repeat("a", 40), Subject: "pulse", Author: "ada",
		AuthorDate: pulseNow.Add(-48 * time.Hour),
	}}}
	bindPulse(t, f, p, found)
	return f, f.mustGet("/demo/spindle", 200)
}

// cardNodes returns the elements whose text contains "Code pulse" (the card's
// eyebrow), so ancestry can be checked structurally.
func cardNodes(doc *html.Node) []*html.Node {
	var out []*html.Node
	walk(doc, func(n *html.Node) {
		if n.Data == "div" && nodeText(n) == "Code pulse" {
			out = append(out, n)
		}
	})
	return out
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(strings.TrimSpace(x.Data))
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func TestCodePulseFullProjection(t *testing.T) {
	_, body := pulseFixture(t, pulseProjection(), true)

	for _, want := range []string{
		"Code pulse",
		"10 commits · 90d",
		"Top contributor",
		"80%",
		"3 contributors",
		"Last commit",
		"Last release",
		"2d ago",
		"3mo ago",
		"Go 84% · JavaScript 9% · CSS 5% · HTML 2%",
		"computed 12m ago",
		`href="/demo/spindle/insights"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pulse card missing %q", want)
		}
	}
	if strings.Contains(body, "Bus factor") || strings.Contains(body, "bus factor") {
		t.Error("the card must not call the contributor share a bus factor")
	}
	// The share is the row's value; the count is a caption line beneath it,
	// never a kv value (which would read as a suffix to the percentage).
	doc := parsePage(t, body)
	var caption *html.Node
	walk(doc, func(n *html.Node) {
		if n.Data == "p" && strings.TrimSpace(nodeText(n)) == "3 contributors" {
			caption = n
		}
		if n.Data == "span" && strings.Contains(attr(n, "class"), "kv-val") &&
			strings.TrimSpace(nodeText(n)) == "3 contributors" {
			t.Error("the contributor count must not be a kv value")
		}
	})
	if caption == nil {
		t.Error("the contributor count is not a caption line")
	}
	if !strings.Contains(body, ">Top contributor</span>") || !strings.Contains(body, ">80%</span>") {
		t.Error("the share is not the top-contributor row's value")
	}
	if strings.Contains(body, "80% · 3 contributors") {
		t.Error("the share and the count must not share a line")
	}
	// The sparkline is inline SVG with an accessible name stating range and
	// totals; the text repeats the fact.
	svg := nodesWithAttr(doc, "role", "img")
	if len(svg) != 1 {
		t.Fatalf("role=img nodes = %d, want 1", len(svg))
	}
	label := attr(svg[0], "aria-label")
	for _, want := range []string{"last 30 days (UTC)", "7 commits", "peak 5 on Nov 25", "10 commits in the last 90 days"} {
		if !strings.Contains(label, want) {
			t.Errorf("sparkline label %q missing %q", label, want)
		}
	}
	if svg[0].Data != "svg" || len(nodesIn(svg[0], "polyline")) != 1 {
		t.Error("sparkline is not a server-rendered <svg><polyline>")
	}
	// The card sits INSIDE the About <aside>, beneath the About box, and the
	// page keeps exactly one h1.
	cards := cardNodes(doc)
	if len(cards) != 1 {
		t.Fatalf("card eyebrows = %d, want 1", len(cards))
	}
	if !hasAncestor(cards[0], "aside") {
		t.Error("Code pulse card is not inside the About <aside>")
	}
	if n := countTag(t, body, "h1"); n != 1 {
		t.Errorf("page h1 count = %d, want 1", n)
	}
	// No JavaScript: the card subtree carries no script and no inline handler.
	for _, n := range nodesIn(cards[0], "script") {
		t.Errorf("card contains a script: %+v", n)
	}
	// Two cards in the rail: About first, pulse second.
	if strings.Index(body, ">About<") > strings.Index(body, "Code pulse") {
		t.Error("Code pulse card is not beneath the About box")
	}
}

func nodesIn(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	walk(n, func(x *html.Node) {
		if x.Data == tag {
			out = append(out, x)
		}
	})
	return out
}

func TestCodePulseCappedContributors(t *testing.T) {
	p := pulseProjection()
	p.Windows["90"].ContributorsCapped = true
	p.Windows["90"].ContributorsTotal = 60
	_, body := pulseFixture(t, p, true)
	doc := parsePage(t, body)
	text := nodeText(doc)
	if !strings.Contains(text, "60+ contributors") {
		t.Errorf("capped contributors did not render 60+: %s", body)
	}
	caption, kvValue := false, false
	walk(doc, func(n *html.Node) {
		txt := strings.TrimSpace(nodeText(n))
		switch {
		case n.Data == "p" && txt == "60+ contributors":
			caption = true
		case n.Data == "span" && strings.Contains(attr(n, "class"), "kv-val") && txt == "60+ contributors":
			kvValue = true
		}
	})
	if !caption {
		t.Errorf("capped count is not a caption line: %s", body)
	}
	if kvValue {
		t.Errorf("capped count must not be a kv value: %s", body)
	}
	if !strings.Contains(body, ">80%</span>") {
		t.Errorf("capped count lost the share line: %s", body)
	}
}

func TestCodePulseReleasesUnknown(t *testing.T) {
	p := pulseProjection()
	p.Facts = insights.Facts{ReleasesKnown: false}
	_, body := pulseFixture(t, p, true)
	if strings.Contains(body, "Last release") {
		t.Error("an unknown release read must omit the release row")
	}
	if !strings.Contains(body, "Last commit") {
		t.Error("the cadence row still renders when releases are unknown")
	}
}

func TestCodePulseNoReleaseYet(t *testing.T) {
	p := pulseProjection()
	p.Facts = insights.Facts{ReleasesKnown: true, Releases: 0}
	_, body := pulseFixture(t, p, true)
	if !strings.Contains(body, "No release yet") {
		t.Error("a verified-zero release count must say so")
	}
}

func TestCodePulseAbsentProjection(t *testing.T) {
	_, body := pulseFixture(t, nil, false)
	if strings.Contains(body, "Code pulse") {
		t.Error("an absent projection must render no card")
	}
	// The rest of the page is untouched.
	if !strings.Contains(body, "main.go") {
		t.Error("the file table did not render")
	}
}

func TestCodePulseOldProjectionHidesLanguageRow(t *testing.T) {
	p := pulseProjection()
	p.Languages = nil
	p.Facts.LastReleaseAt = ""
	_, body := pulseFixture(t, p, true)
	if strings.Contains(body, "Code pulse") == false {
		t.Fatal("old projection must still render the card")
	}
	if strings.Contains(body, ">Languages<") {
		t.Error("a projection without Languages must hide the language row")
	}
	if strings.Contains(body, "Last release") {
		t.Error("a release count without a stamp must omit the dated row")
	}
	if !strings.Contains(body, "10 commits · 90d") || !strings.Contains(body, ">Top contributor</span>") {
		t.Error("old projection lost the rows it still has")
	}
}

func TestCodePulseTopFourPlusOther(t *testing.T) {
	p := pulseProjection()
	p.Languages = []insights.Language{
		{Name: "Go", Bytes: 700, Pct: 70},
		{Name: "JavaScript", Bytes: 100, Pct: 10},
		{Name: "CSS", Bytes: 80, Pct: 8},
		{Name: "HTML", Bytes: 60, Pct: 6},
		{Name: "Shell", Bytes: 40, Pct: 4},
		{Name: "Other", Bytes: 20, Pct: 2},
	}
	_, body := pulseFixture(t, p, true)
	if !strings.Contains(body, "Go 70% · JavaScript 10% · CSS 8% · HTML 6% · Other 6%") {
		t.Errorf("top-4 + Other list wrong: %s", body)
	}
	if strings.Contains(body, "Shell 4%") {
		t.Error("the fifth language must fold into Other")
	}
}

func TestCodePulseTruncatedAndCappedNotes(t *testing.T) {
	p := pulseProjection()
	p.Scan.Truncated = true
	p.LanguagesCapped = true
	_, body := pulseFixture(t, p, true)
	for _, want := range []string{
		"Commit counts cover the most recent 20,000 commits.",
		"The language mix covers the first 50,000 files.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bounded note missing %q", want)
		}
	}
}

func TestCodePulseNoCommitsNoContributorBlock(t *testing.T) {
	p := pulseProjection()
	p.Windows["90"] = &insights.Window{Days: 90, Commits: 0}
	p.Windows["30"] = &insights.Window{Days: 30, Bucket: "day", Buckets: []insights.Bucket{
		{Start: "2026-11-24T00:00:00Z", Label: "Nov 24"},
		{Start: "2026-11-25T00:00:00Z", Label: "Nov 25"},
	}}
	_, body := pulseFixture(t, p, true)
	if strings.Contains(body, "Top contributor") {
		t.Error("a zero-commit window must omit the top-contributor block")
	}
	if strings.Contains(body, "Bus factor") {
		t.Error("a zero-commit window must omit the contributor block entirely")
	}
	if !strings.Contains(body, "0 commits · 90d") {
		t.Error("the zero-total momentum line must still be honest")
	}
	if !strings.Contains(body, "no commits in 2 daily buckets") {
		t.Error("the empty sparkline label must say no commits")
	}
}

func TestCodePulseInsightsReadErrorRendersNoCard(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Entries: []api.TreeEntry{{Name: "a.go", Type: "blob", Mode: "100644", Size: 1}}}
	old := repoDeps
	repoDeps = RepoDeps{Insights: func(context.Context, string, string) (*insights.Projection, bool, error) {
		return nil, false, context.DeadlineExceeded
	}}
	t.Cleanup(func() { repoDeps = old })
	body := f.mustGet("/demo/spindle", 200)
	if strings.Contains(body, "Code pulse") {
		t.Error("a failed projection read must not break the page or invent a card")
	}
}

func TestCodePulseOtherInTopFourMergesTheTail(t *testing.T) {
	p := pulseProjection()
	p.Languages = []insights.Language{
		{Name: "Other", Bytes: 950, Pct: 95},
		{Name: "JavaScript", Bytes: 20, Pct: 2},
		{Name: "Go", Bytes: 10, Pct: 1},
		{Name: "Markdown", Bytes: 10, Pct: 1},
		{Name: "CSS", Bytes: 10, Pct: 1},
	}
	_, body := pulseFixture(t, p, true)
	doc := parsePage(t, body)
	text := nodeText(doc)
	if !strings.Contains(text, "Other 96% · JavaScript 2% · Go 1% · Markdown 1%") {
		t.Errorf("Other top-4 row did not absorb the tail: %s", text)
	}
	if strings.Count(text, "Other ") != 1 {
		t.Errorf("duplicate Other rows: %s", text)
	}
}

func TestCodePulseSingularCommit(t *testing.T) {
	p := pulseProjection()
	p.Windows["90"] = &insights.Window{Days: 90, Commits: 1,
		Contributors: []insights.Contributor{{Name: "Ada", Email: "a@x", Commits: 1}}, ContributorsTotal: 1}
	_, body := pulseFixture(t, p, true)
	if !strings.Contains(body, "1 commit · 90d") {
		t.Errorf("singular momentum line missing: %s", body)
	}
}
