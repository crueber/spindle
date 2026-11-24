package webui

// insights.go — the repository Insights page (`/{o}/{r}/insights`): pulse,
// contributors, code frequency and the facts the bucket can honestly state
// (docs/features/18_repo_insights.md).
//
// The page NEVER runs git and never LISTs: it reads the CAS'd projection
// `repos/<o>/<r>/insights/pulse.json` in ONE exact-key GET through the
// RepoDeps seam. The projection is computed by the `insights-refresh` task
// (maintainer cadence + a writer-triggered refresh form). Staleness is the
// stored head sha against the chrome's head (plus a 24 h age note), never a
// silent recompute on read.
//
// The gate is repoGate + repoOpen — exactly the Code page's gate, so a private
// repository is a 404 and the cache key (which contains owner/repo) can never
// mix repositories. The refresh POST adds the write floor and the same-origin
// preflight; it starts/joins the narrated task and returns immediately (law 7:
// no silent wait).

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/insights"
)

type insightWindowLink struct {
	Days   int
	Label  string
	Active bool
}

type insightBar struct {
	Label     string
	Commits   int
	CommitPct int
	Additions int
	Deletions int
	AddPct    int
	DelPct    int
	DelBottom int // percent from the baseline where the deletion segment starts
}

type insightContributor struct {
	Name       string
	Email      string
	Commits    int
	LastActive string
}

type insightsPage struct {
	Repo    *RepoChrome
	Guide   *repoGuide
	Window  int
	Windows []insightWindowLink

	Projection *insights.Projection
	Block      *insights.Window

	Bars         []insightBar
	Contributors []insightContributor

	// Chart caption facts (rendered as text; the bars are decoration). The
	// summary strings are built here so the template only prints.
	BucketNoun   string
	PeriodCount  int
	PeriodFirst  string
	PeriodLast   string
	PulseActive  int
	PulseSummary string
	FreqActive   int
	FreqSummary  string

	Stale      bool
	StaleText  string
	ComputedAt string
	ScanNote   string
	CollabNote string
	FutureNote string

	CanRefresh    bool
	RefreshAction string
	Flash         string
	Err           string
}

func (s *Server) repoInsights(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	window := insights.ParseWindow(r.URL.Query().Get("window"))
	data := &insightsPage{
		Repo:   c,
		Window: window,
		Flash:  formsFlash(r),
	}
	for _, days := range insights.Windows {
		data.Windows = append(data.Windows, insightWindowLink{
			Days: days, Label: strconv.Itoa(days) + " days", Active: days == window,
		})
	}
	data.CanRefresh = repoCanWrite(r.Context(), rq.id.Owner, rq.id.Name, rq.p) && repoDeps.InsightsRefresh != nil
	data.RefreshAction = "/" + c.Full + "/insights/refresh"
	if c.Empty {
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/insights", "Insights · "+c.Full, data)
		return
	}
	if repoDeps.Insights == nil {
		data.Err = "Insights are not available on this build."
		s.page(w, r, rq.p, http.StatusOK, "repo/insights", "Insights · "+c.Full, data)
		return
	}
	p, found, err := repoDeps.Insights(r.Context(), rq.id.Owner, rq.id.Name)
	if err != nil {
		data.Err = "The insights projection could not be read right now."
	} else if found && p != nil {
		data.Projection = p
		data.ComputedAt = p.ComputedAt
		data.Block = p.Windows[strconv.Itoa(window)]
		if data.Block == nil {
			// A projection from another build/window set: fail closed to the
			// "not computed" state rather than render half a page.
			data.Projection = nil
		}
	}
	if data.Projection != nil {
		s.fillInsights(data, c, p)
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/insights", "Insights · "+c.Full, data)
}

// fillInsights derives the render model (bar widths, stale copy, honesty
// notes) from the projection. Templates only print.
func (s *Server) fillInsights(d *insightsPage, c *RepoChrome, p *insights.Projection) {
	w := d.Block
	var maxCommits int
	var maxLines int
	for _, b := range w.Buckets {
		if b.Commits > maxCommits {
			maxCommits = b.Commits
		}
		if b.Additions+b.Deletions > maxLines {
			maxLines = b.Additions + b.Deletions
		}
	}
	// Chart facts: the caption carries the numbers, so a zero bucket is flat
	// decoration rather than a row of nothing (18_repo_insights.md §4).
	var totalCommits, totalLines int
	var pulseActive, freqActive int
	var peakCommitLabel, peakLineLabel string
	var peakCommits, peakLines int
	for _, b := range w.Buckets {
		lines := b.Additions + b.Deletions
		totalCommits += b.Commits
		totalLines += lines
		if b.Commits > 0 {
			pulseActive++
			if b.Commits > peakCommits {
				peakCommits, peakCommitLabel = b.Commits, b.Label
			}
		}
		if lines > 0 {
			freqActive++
			if lines > peakLines {
				peakLines, peakLineLabel = lines, b.Label
			}
		}
		addPct := barPct(b.Additions, maxLines)
		delPct := barPct(b.Deletions, maxLines)
		// The two segments stack, so the 2% "visible" floor must never push
		// the column past the chart's 100% line.
		if addPct+delPct > 100 {
			delPct = 100 - addPct
		}
		d.Bars = append(d.Bars, insightBar{
			Label:     b.Label,
			Commits:   b.Commits,
			CommitPct: barPct(b.Commits, maxCommits),
			Additions: b.Additions,
			Deletions: b.Deletions,
			AddPct:    addPct,
			DelPct:    delPct,
			DelBottom: addPct,
		})
	}
	d.BucketNoun = w.Bucket
	d.PeriodCount = len(d.Bars)
	if d.PeriodCount > 0 {
		d.PeriodFirst = d.Bars[0].Label
		d.PeriodLast = d.Bars[d.PeriodCount-1].Label
	}
	if totalCommits > 0 && d.PeriodCount > 0 {
		d.PulseActive = pulseActive
		d.PulseSummary = fmt.Sprintf("%d %s across %s of %d; peak %s with %d %s.",
			totalCommits, plural(totalCommits, "commit", "commits"),
			activePeriods(pulseActive, w.Bucket), d.PeriodCount,
			peakCommitLabel, peakCommits, plural(peakCommits, "commit", "commits"))
	}
	if totalLines > 0 && d.PeriodCount > 0 {
		d.FreqActive = freqActive
		d.FreqSummary = fmt.Sprintf("%d %s changed across %s of %d; peak %s with %d %s.",
			totalLines, plural(totalLines, "line", "lines"),
			activePeriods(freqActive, w.Bucket), d.PeriodCount,
			peakLineLabel, peakLines, plural(peakLines, "line", "lines"))
	}
	for _, a := range w.Contributors {
		d.Contributors = append(d.Contributors, insightContributor{
			Name: a.Name, Email: a.Email, Commits: a.Commits, LastActive: a.LastActive,
		})
	}
	now := s.env.now()
	head := ""
	if c.Head != nil {
		head = c.Head.SHA
	}
	stale := insights.Stale(p, head, now, insights.DefaultTTL)
	d.Stale = stale
	switch {
	case p.HeadSHA != head && head != "":
		d.StaleText = "The stored projection was computed at another head; the numbers below describe that snapshot."
	case stale:
		d.StaleText = "This projection is more than 24 hours old."
	default:
		d.StaleText = ""
	}
	if p.Scan.Truncated {
		d.ScanNote = "Commit, contributor and line counts cover the most recent " +
			strconv.Itoa(p.Scan.MaxCommits) + " commits; this window holds more."
	}
	if p.Collab.Known && p.Collab.IndexBounded {
		d.CollabNote = "Issue and pull-request counts read the shared list index; threads older than its compaction watermark are not counted."
	} else if !p.Collab.Known {
		d.CollabNote = "Issue and pull-request counts are unavailable right now."
	}
	if p.Collab.MergeScanCapped {
		d.CollabNote = d.CollabNote + " Merged-PR counts cover the most recent " +
			strconv.Itoa(insights.DefaultMergeScanCap) + " closed pull requests."
	}
	if p.Scan.FutureDated > 0 {
		d.FutureNote = strconv.Itoa(p.Scan.FutureDated) +
			" commit(s) dated after the projection instant were excluded from every window."
	}
}

// barPct maps a count to a 0–100 width, keeping any nonzero value visible
// (2% floor) so a single-commit bar is never an empty cell.
func barPct(v, max int) int {
	if v <= 0 || max <= 0 {
		return 0
	}
	pct := v * 100 / max
	if pct < 2 {
		pct = 2
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// plural picks the singular or plural spelling for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// activePeriods phrases "1 active day" / "3 active days" for the chart
// caption. noun is the projection's bucket name (day/week/month).
func activePeriods(n int, noun string) string {
	return strconv.Itoa(n) + " active " + plural(n, noun, noun+"s")
}

// repoInsightsRefresh starts (or joins) the `insights-refresh` task and
// returns immediately. The projection appears when the task finishes; the
// page reload reads it.
func (s *Server) repoInsightsRefresh(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, rq.p, "Refresh repository insights") {
		return
	}
	if repoDeps.InsightsRefresh == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "insights refresh is not wired on this build")
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	if !repoCanWrite(r.Context(), rq.id.Owner, rq.id.Name, rq.p) {
		s.errorPage(w, r, http.StatusForbidden, "write access required")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	window := insights.ParseWindow(r.PostFormValue("window"))
	if err := repoDeps.InsightsRefresh(r.Context(), rq.id.Owner, rq.id.Name); err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "insights refresh could not be started")
		return
	}
	s.formsRedirectFlash(w, r, "/"+c.Full+"/insights?window="+strconv.Itoa(window), "Insights refresh started")
}
