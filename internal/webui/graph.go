package webui

import (
	"html/template"
	"strconv"
	"strings"
)

// Commit-graph geometry, ported from web/src/lib/commit-graph.js. The values
// match app.css (#506/#512): lane colours live in --graph-* variables, the
// SVG carries only .gl-N classes and 2px currentColor strokes.
const (
	graphLanes   = 8 // palette size (.gl-0..7)
	graphLaneGap = 12
	graphLanePad = 8
	graphRowH    = 48
	graphRowMid  = 24
)

// GraphCommit is one row of the commits window the lane rail is derived from
// (the {sha, parents} payload plus the display fields the list row uses).
type GraphCommit struct {
	SHA     string
	Short   string
	Subject string
	Author  string
	When    string
	Parents []string
	Refs    []string
}

// RenderCommitGraph emits the inline SVG the commits list embeds: one
// already-escaped .commit-rail block per commit, in input order. Ports the
// lane layout and colours of commit-graph.js — lanes are derived from the
// visible window only, first-parent threading, branch-out on extra parents,
// diagonals on divergence/convergence, hollow node on merges.
//
// Per-row blocks (rather than one tall SVG) keep the rail aligned with
// variable-height rows, exactly like the JS GraphRail. A caller that owns
// its own row markup can use RenderCommitGraphRows and embed one block per
// row.
func RenderCommitGraph(commits []GraphCommit) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="commit-graph">`)
	for _, row := range RenderCommitGraphRows(commits) {
		b.WriteString(string(row))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// RenderCommitGraphRows returns the rail markup for each commit, index-aligned
// with the input. Embed element i in commit row i.
func RenderCommitGraphRows(commits []GraphCommit) []template.HTML {
	rows, width := assignLanes(commits)
	out := make([]template.HTML, 0, len(rows))
	for _, row := range rows {
		out = append(out, template.HTML(renderRail(row, width, commits[row.index])))
	}
	return out
}

type graphRow struct {
	index   int
	lane    int
	parents []string
	top     []laneRef
	bottom  []laneRef
}

type laneRef struct {
	sha string
	ok  bool
}

// assignLanes threads first-parent lanes over the visible window (newest
// first). Each row carries the lane-boundary snapshots entering and leaving
// it; width is the widest boundary seen.
func assignLanes(commits []GraphCommit) ([]graphRow, int) {
	rows := make([]graphRow, 0, len(commits))
	var lanes []laneRef
	width := 0
	snap := func() []laneRef {
		if len(lanes) > width {
			width = len(lanes)
		}
		return append([]laneRef(nil), lanes...)
	}
	for idx, c := range commits {
		sha := c.SHA
		parents := make([]string, 0, len(c.Parents))
		for _, p := range c.Parents {
			parents = append(parents, p)
		}
		lane := indexOfLane(lanes, sha)
		if lane == -1 {
			if free := freeLane(lanes); free != -1 {
				lane = free
			} else {
				lane = len(lanes)
				lanes = append(lanes, laneRef{})
			}
		}
		top := snap()
		if len(parents) == 0 {
			lanes[lane] = laneRef{}
		} else {
			first := parents[0]
			switch m := indexOfLane(lanes, first); {
			case m == -1:
				lanes[lane] = laneRef{sha: first, ok: true}
			case m != lane:
				lanes[lane] = laneRef{}
			}
		}
		if len(parents) > 1 {
			for _, p := range parents[1:] {
				if indexOfLane(lanes, p) != -1 {
					continue
				}
				if free := freeLane(lanes); free != -1 {
					lanes[free] = laneRef{sha: p, ok: true}
				} else {
					lanes = append(lanes, laneRef{sha: p, ok: true})
				}
			}
		}
		for len(lanes) > 0 && !lanes[len(lanes)-1].ok {
			lanes = lanes[:len(lanes)-1]
		}
		bottom := snap()
		rows = append(rows, graphRow{index: idx, lane: lane, parents: parents, top: top, bottom: bottom})
	}
	return rows, width
}

func indexOfLane(lanes []laneRef, sha string) int {
	for i, l := range lanes {
		if l.ok && l.sha == sha {
			return i
		}
	}
	return -1
}

func freeLane(lanes []laneRef) int {
	for i, l := range lanes {
		if !l.ok {
			return i
		}
	}
	return -1
}

type graphDiagonal struct {
	from int
	to   int
}

// rowDiagonals lists the slanted lane pairs in a row's lower half: a parent
// already carried on another lane joins/branches there. Deduplicated by
// target lane.
func rowDiagonals(row graphRow) []graphDiagonal {
	var out []graphDiagonal
	seen := make(map[int]bool, len(row.parents))
	for _, p := range row.parents {
		m := indexOfLane(row.bottom, p)
		if m == -1 || m == row.lane || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, graphDiagonal{from: row.lane, to: m})
	}
	return out
}

func laneX(lane int) int {
	return graphLanePad + lane*graphLaneGap
}

func railWidth(width int) int {
	if width < 1 {
		width = 1
	}
	return graphLanePad*2 + (width-1)*graphLaneGap
}

func laneClass(lane int) string {
	if lane < 0 {
		lane = -lane
	}
	return "gl-" + strconv.Itoa(lane%graphLanes)
}

func renderRail(row graphRow, width int, c GraphCommit) string {
	w := railWidth(width)
	var b strings.Builder
	b.WriteString(`<div class="commit-rail relative self-stretch shrink-0" style="width:`)
	b.WriteString(strconv.Itoa(w))
	b.WriteString(`px"`)
	if c.SHA != "" {
		b.WriteString(` data-sha="` + escapeAttr(c.SHA) + `"`)
	}
	b.WriteString(` aria-hidden="true"><svg class="absolute inset-0 h-full w-full" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(w))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(graphRowH))
	b.WriteString(`" preserveAspectRatio="none" aria-hidden="true">`)
	if c.Short != "" || c.Subject != "" {
		b.WriteString("<title>")
		b.WriteString(escapeText(c.Short))
		if c.Short != "" && c.Subject != "" {
			b.WriteString(": ")
		}
		b.WriteString(escapeText(c.Subject))
		b.WriteString("</title>")
	}
	for k, lane := range row.top {
		if !lane.ok {
			continue
		}
		writeRailLine(&b, laneX(k), 0, laneX(k), graphRowMid, laneClass(k))
	}
	for k, lane := range row.bottom {
		if !lane.ok {
			continue
		}
		writeRailLine(&b, laneX(k), graphRowMid, laneX(k), graphRowH, laneClass(k))
	}
	for _, d := range rowDiagonals(row) {
		writeRailLine(&b, laneX(d.from), graphRowMid, laneX(d.to), graphRowH, laneClass(d.to))
	}
	b.WriteString(`</svg><span class="graph-dot `)
	b.WriteString(laneClass(row.lane))
	if len(row.parents) > 1 {
		b.WriteString(" graph-dot-merge")
	}
	b.WriteString(`" style="left:`)
	b.WriteString(strconv.Itoa(laneX(row.lane)))
	b.WriteString(`px"></span></div>`)
	return b.String()
}

func writeRailLine(b *strings.Builder, x1, y1, x2, y2 int, class string) {
	b.WriteString(`<line x1="`)
	b.WriteString(strconv.Itoa(x1))
	b.WriteString(`" y1="`)
	b.WriteString(strconv.Itoa(y1))
	b.WriteString(`" x2="`)
	b.WriteString(strconv.Itoa(x2))
	b.WriteString(`" y2="`)
	b.WriteString(strconv.Itoa(y2))
	b.WriteString(`" class="`)
	b.WriteString(class)
	b.WriteString(`" stroke="currentColor" stroke-width="2" vector-effect="non-scaling-stroke"></line>`)
}
