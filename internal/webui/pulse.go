// pulse.go — the Code page's "Code pulse" card (docs/features/18_repo_insights.md
// §4.1): momentum, top-contributor share, cadence and the language mix, read
// from the SAME cached projection the Insights page uses.
//
// One exact-key GET (`repos/<o>/<r>/insights/pulse.json`) rides the tree
// request; it never runs git and never LISTs. No projection → no card (the
// page is unchanged). A projection written before the language fields
// existed → the language row is hidden, the rest still renders.
//
// Everything is server-rendered: the sparkline is inline SVG with a role/label
// that states its range and total, and every fact the sparkline or the stacked
// bar draws is repeated in text (colour and shape are never the only signal).
package webui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/insights"
)

// pulseLang is one language row of the card's mix: a text fact plus a
// decorative segment width.
type pulseLang struct {
	Name    string
	Pct     int
	Opacity string
}

// codePulse is the card's render model. The handler derives every string; the
// template only prints (the same rule as the Insights page).
type codePulse struct {
	// Momentum: the 90-day total in text, the 30-day daily buckets as an
	// inline sparkline whose aria-label states range, total and peak.
	Commits90   int
	SparkPoints string
	SparkLabel  string

	// Top contributor (90-day window): TopShare is the value of the
	// "Top contributor" row ("78%") and ContributorLine is the caption line
	// beneath it ("3 contributors", "60+ contributors" when capped). Both
	// empty means the block is omitted (a zero-commit window).
	TopShare        string
	ContributorLine string

	// Cadence: the page's head-commit time and the newest release stamp.
	LastCommit   string // RFC 3339; empty = omit the row
	ReleaseRow   bool
	ReleaseStamp string // RFC 3339 when the row is a date
	ReleaseText  string // "No release yet" when ReleasesKnown && Releases == 0

	// Language mix: text list (top 4 + Other) plus the decorative bar.
	Languages    []pulseLang
	LanguageText string
	HasLanguages bool

	// Footer and honesty notes.
	ComputedAgo  string
	InsightsHref string
	Note         string
}

// buildCodePulse derives the card from a found projection. latest is the
// Code page's head-commit strip (nil when that read failed). Returning nil
// means "render no card": the page never invents a pulse.
func buildCodePulse(p *insights.Projection, latest *repoCommit, repo *RepoChrome, now time.Time) *codePulse {
	if p == nil || repo == nil {
		return nil
	}
	w30 := p.Windows[strconv.Itoa(insights.DefaultWindow)]
	w90 := p.Windows["90"]
	if w30 == nil || w90 == nil {
		// A projection from a different window set: fail closed to no card
		// rather than print half of one.
		return nil
	}
	now = now.UTC()
	cp := &codePulse{
		Commits90:    w90.Commits,
		InsightsHref: "/" + repo.Full + "/insights",
	}
	cp.SparkPoints, cp.SparkLabel = sparkline(w30.Buckets, w90.Commits)

	if w90.Commits > 0 && len(w90.Contributors) > 0 {
		top := w90.Contributors[0]
		pct := int(math.Round(float64(top.Commits) * 100 / float64(w90.Commits)))
		if pct > 100 {
			pct = 100
		}
		count := strconv.Itoa(w90.ContributorsTotal)
		if w90.ContributorsCapped {
			// ContributorsTotal is the window's true distinct count; the list
			// itself is truncated at 50, so the "+" is conservative.
			count += "+"
		}
		cp.TopShare = strconv.Itoa(pct) + "%"
		cp.ContributorLine = fmt.Sprintf("%s %s", count, plural(w90.ContributorsTotal, "contributor", "contributors"))
	}

	if latest != nil && latest.When != "" {
		cp.LastCommit = latest.When
	}
	if p.Facts.ReleasesKnown {
		switch {
		case p.Facts.Releases == 0:
			cp.ReleaseRow = true
			cp.ReleaseText = "No release yet"
		case p.Facts.LastReleaseAt != "":
			cp.ReleaseRow = true
			cp.ReleaseStamp = p.Facts.LastReleaseAt
		}
	}

	buildPulseLanguages(cp, p.Languages)

	if ago := relTimeAt(p.ComputedAt, now); ago != "" {
		cp.ComputedAgo = ago
	} else {
		cp.ComputedAgo = "earlier"
	}
	var notes []string
	if p.Scan.Truncated {
		notes = append(notes, fmt.Sprintf("Commit counts cover the most recent %s commits.", thousands(p.Scan.MaxCommits)))
	}
	if p.LanguagesCapped {
		notes = append(notes, fmt.Sprintf("The language mix covers the first %s files.", thousands(git.InsightsLanguageScanMax)))
	}
	cp.Note = strings.Join(notes, " ")
	return cp
}

// buildPulseLanguages turns the projection rows into the card's top-4 + Other
// text list and the matching decorative segments. Percentages are already
// whole numbers summing to 100, so the text and the bar agree exactly.
func buildPulseLanguages(cp *codePulse, langs []insights.Language) {
	if len(langs) == 0 {
		return
	}
	const top = 4
	rows := langs
	other := 0
	if len(rows) > top {
		for _, l := range rows[top:] {
			other += l.Pct
		}
		rows = rows[:top]
	}
	opacities := []string{"1", ".72", ".52", ".38", ".28"}
	var parts []string
	mergedOther := false
	for i, l := range rows {
		pct := l.Pct
		if other > 0 && l.Name == git.OtherLanguage {
			pct += other
			mergedOther = true
		}
		cp.Languages = append(cp.Languages, pulseLang{Name: l.Name, Pct: pct, Opacity: opacities[i]})
		parts = append(parts, fmt.Sprintf("%s %d%%", l.Name, pct))
	}
	if other > 0 && !mergedOther {
		cp.Languages = append(cp.Languages, pulseLang{Name: git.OtherLanguage, Pct: other, Opacity: opacities[len(opacities)-1]})
		parts = append(parts, fmt.Sprintf("%s %d%%", git.OtherLanguage, other))
	}
	cp.LanguageText = strings.Join(parts, " · ")
	cp.HasLanguages = true
}

// sparkline renders the 30 daily buckets as one inline-SVG polyline and the
// text that makes it accessible: range, bucket count, total and peak in UTC.
func sparkline(buckets []insights.Bucket, commits90 int) (points, label string) {
	const w, h, pad = 240.0, 28.0, 1.0
	var total, peak int
	peakLabel := ""
	for _, b := range buckets {
		total += b.Commits
		if b.Commits > peak {
			peak, peakLabel = b.Commits, b.Label
		}
	}
	max := peak
	if max <= 0 {
		max = 1
	}
	var b strings.Builder
	for i, bkt := range buckets {
		x := pad
		if n := len(buckets); n > 1 {
			x = pad + float64(i)*(w-2*pad)/float64(n-1)
		}
		y := h - pad - float64(bkt.Commits)*(h-2*pad)/float64(max)
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	ninety := fmt.Sprintf("%d %s in the last 90 days", commits90, plural(commits90, "commit", "commits"))
	switch {
	case total == 0:
		label = fmt.Sprintf("Commit activity, last 30 days (UTC): no commits in %d daily buckets; %s.", len(buckets), ninety)
	case peak > 0:
		label = fmt.Sprintf("Commit activity, last 30 days (UTC): %d %s across %d daily buckets; peak %d on %s; %s.",
			total, plural(total, "commit", "commits"), len(buckets), peak, peakLabel, ninety)
	default:
		label = fmt.Sprintf("Commit activity, last 30 days (UTC): %d daily buckets; %s.", len(buckets), ninety)
	}
	return b.String(), label
}

// thousands formats a count with comma separators ("20,000") for prose.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
