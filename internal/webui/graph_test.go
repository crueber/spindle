package webui

import (
	"strings"
	"testing"
)

func gc(sha string, parents ...string) GraphCommit {
	return GraphCommit{SHA: sha, Short: sha, Parents: parents}
}

func laneShas(lanes []laneRef) []string {
	out := make([]string, len(lanes))
	for i, l := range lanes {
		if l.ok {
			out[i] = l.sha
		}
	}
	return out
}

func laneNums(rows []graphRow) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.lane
	}
	return out
}

func eqInts(t *testing.T, label string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
}

func eqStrs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
}

// --- lane assignment (same vectors as commit-graph-506.test.js) -------------

func TestAssignLanesEmptyWindow(t *testing.T) {
	rows, width := assignLanes(nil)
	if len(rows) != 0 || width != 0 {
		t.Fatalf("empty window: rows=%v width=%d", rows, width)
	}
	rows, width = assignLanes([]GraphCommit{gc("a")})
	if width != 1 || len(rows) != 1 || rows[0].lane != 0 {
		t.Fatalf("single commit: width=%d rows=%+v", width, rows)
	}
	eqStrs(t, "top", laneShas(rows[0].top), []string{""})
	eqStrs(t, "bottom", laneShas(rows[0].bottom), nil)
}

func TestAssignLanesLinearThreadsOneLane(t *testing.T) {
	rows, width := assignLanes([]GraphCommit{gc("c", "b"), gc("b", "a"), gc("a")})
	if width != 1 {
		t.Fatalf("width = %d, want 1", width)
	}
	eqInts(t, "lanes", laneNums(rows), []int{0, 0, 0})
	for _, r := range rows {
		if d := rowDiagonals(r); len(d) != 0 {
			t.Fatalf("%s: unexpected diagonals %v", r.parents, d)
		}
	}
	eqStrs(t, "rows[0].bottom", laneShas(rows[0].bottom), []string{"b"})
	eqStrs(t, "rows[1].top", laneShas(rows[1].top), []string{"b"})
}

func TestAssignLanesBranchRejoinsBase(t *testing.T) {
	rows, width := assignLanes([]GraphCommit{gc("side", "base"), gc("main", "base"), gc("base")})
	if width != 2 {
		t.Fatalf("width = %d, want 2", width)
	}
	eqInts(t, "lanes", laneNums(rows), []int{0, 1, 0})
	d := rowDiagonals(rows[1])
	if len(d) != 1 || d[0].from != 1 || d[0].to != 0 {
		t.Fatalf("side tip diagonals = %v, want [{1 0}]", d)
	}
	if d := rowDiagonals(rows[2]); len(d) != 0 {
		t.Fatalf("base row diagonals = %v, want none", d)
	}
}

func TestAssignLanesMergeFansOut(t *testing.T) {
	rows, width := assignLanes([]GraphCommit{
		gc("m", "a", "b"), gc("a", "x"), gc("b", "x"), gc("x"),
	})
	if width != 2 {
		t.Fatalf("width = %d, want 2", width)
	}
	eqInts(t, "lanes", laneNums(rows), []int{0, 0, 1, 0})
	eqStrs(t, "merge bottom", laneShas(rows[0].bottom), []string{"a", "b"})
	d := rowDiagonals(rows[0])
	if len(d) != 1 || d[0].from != 0 || d[0].to != 1 {
		t.Fatalf("merge diagonals = %v, want [{0 1}]", d)
	}
	d = rowDiagonals(rows[2])
	if len(d) != 1 || d[0].from != 1 || d[0].to != 0 {
		t.Fatalf("side join diagonals = %v, want [{1 0}]", d)
	}
}

func TestAssignLanesBranchThenMerge(t *testing.T) {
	rows, width := assignLanes([]GraphCommit{
		gc("tip", "m"), gc("m", "main", "feat"), gc("main", "root"), gc("feat", "root"), gc("root"),
	})
	if width != 2 {
		t.Fatalf("width = %d, want 2", width)
	}
	eqInts(t, "lanes", laneNums(rows), []int{0, 0, 0, 1, 0})
	eqStrs(t, "merge parents", rows[1].parents, []string{"main", "feat"})
	d := rowDiagonals(rows[1])
	if len(d) != 1 || d[0].from != 0 || d[0].to != 1 {
		t.Fatalf("branch diagonals = %v", d)
	}
	d = rowDiagonals(rows[3])
	if len(d) != 1 || d[0].from != 1 || d[0].to != 0 {
		t.Fatalf("rejoin diagonals = %v", d)
	}
}

func TestAssignLanesPerWindow(t *testing.T) {
	rows, width := assignLanes([]GraphCommit{gc("old2", "old1"), gc("old1", "older")})
	if width != 1 {
		t.Fatalf("width = %d, want 1", width)
	}
	eqInts(t, "lanes", laneNums(rows), []int{0, 0})
	eqStrs(t, "older", laneShas(rows[1].bottom), []string{"older"})
}

func TestGraphGeometry(t *testing.T) {
	if laneX(0) != graphLanePad || laneX(1) != graphLanePad+graphLaneGap {
		t.Fatalf("laneX: %d %d", laneX(0), laneX(1))
	}
	if railWidth(1) != graphLanePad*2 || railWidth(4) != graphLanePad*2+3*graphLaneGap {
		t.Fatalf("railWidth: %d %d", railWidth(1), railWidth(4))
	}
	if railWidth(graphLanes) > 120 {
		t.Fatalf("8-lane rail too wide: %d", railWidth(graphLanes))
	}
	if railWidth(0) != graphLanePad*2 {
		t.Fatalf("empty rail width = %d", railWidth(0))
	}
	for i := 0; i < 16; i++ {
		if got, want := laneClass(i), "gl-"+string(rune('0'+i%8)); got != want {
			t.Fatalf("laneClass(%d) = %q, want %q", i, got, want)
		}
	}
	if laneClass(8) != "gl-0" || laneClass(-3) != "gl-3" {
		t.Fatalf("laneClass cycling: %q %q", laneClass(8), laneClass(-3))
	}
}

// --- rendering --------------------------------------------------------------

func TestRenderCommitGraphRails(t *testing.T) {
	commits := []GraphCommit{
		{SHA: "m", Short: "m", Subject: "merge two", Parents: []string{"a", "b"}},
		{SHA: "a", Short: "a", Subject: "first", Parents: []string{"x"}},
		{SHA: "b", Short: "b", Subject: "second", Parents: []string{"x"}},
		{SHA: "x", Short: "x", Subject: "root"},
	}
	rows := RenderCommitGraphRows(commits)
	if len(rows) != len(commits) {
		t.Fatalf("rows = %d, want %d", len(rows), len(commits))
	}
	for i, row := range rows {
		html := string(row)
		if !strings.Contains(html, `class="commit-rail relative self-stretch shrink-0"`) {
			t.Fatalf("row %d: no rail wrapper: %s", i, html)
		}
		if !strings.Contains(html, `stroke="currentColor"`) {
			t.Fatalf("row %d: stroke is not currentColor: %s", i, html)
		}
		if strings.Contains(html, `stroke="#`) || strings.Contains(html, `fill="#`) {
			t.Fatalf("row %d: color literal in markup: %s", i, html)
		}
		if !strings.Contains(html, `data-sha="`+commits[i].SHA+`"`) {
			t.Fatalf("row %d: missing data-sha: %s", i, html)
		}
	}
	if !strings.Contains(string(rows[0]), "graph-dot-merge") {
		t.Fatalf("merge commit missing hollow node: %s", rows[0])
	}
	if strings.Contains(string(rows[1]), "graph-dot-merge") {
		t.Fatalf("non-merge commit has merge node: %s", rows[1])
	}
	if !strings.Contains(string(rows[0]), `viewBox="0 0 28 48"`) {
		t.Fatalf("two-lane viewBox wrong: %s", rows[0])
	}
	// The merge row carries the divergence as a bottom diagonal (lane 0 → 1).
	if !strings.Contains(string(rows[0]), `<line x1="8" y1="24" x2="20" y2="48" class="gl-1"`) {
		t.Fatalf("merge diagonal missing: %s", rows[0])
	}
	graph := string(RenderCommitGraph(commits))
	if !strings.HasPrefix(graph, `<div class="commit-graph">`) || !strings.HasSuffix(graph, `</div>`) {
		t.Fatalf("wrapper shape: %s", graph)
	}
	if strings.Count(graph, "commit-rail") != len(commits) {
		t.Fatalf("rail count = %d", strings.Count(graph, "commit-rail"))
	}
	if empty := string(RenderCommitGraph(nil)); empty != `<div class="commit-graph"></div>` {
		t.Fatalf("empty graph = %q", empty)
	}
}

func TestRenderCommitGraphEscapesText(t *testing.T) {
	html := string(RenderCommitGraphRows([]GraphCommit{{
		SHA:     `a"b<c`,
		Short:   `s&s`,
		Subject: `<script>alert(1)</script>`,
	}})[0])
	for _, bad := range []string{`<script>`, `a"b`} {
		if strings.Contains(html, bad) {
			t.Fatalf("unescaped %q in %s", bad, html)
		}
	}
	for _, want := range []string{`data-sha="a&quot;b&lt;c"`, `s&amp;s`, `&lt;script&gt;alert(1)&lt;/script&gt;`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q in %s", want, html)
		}
	}
}

func TestRenderCommitGraphRowsStillShowLanes(t *testing.T) {
	commits := make([]GraphCommit, 0, 10)
	for i := 0; i < 10; i++ {
		commits = append(commits, gc("c", "p"))
	}
	rows := RenderCommitGraphRows(commits)
	if len(rows) != 10 {
		t.Fatalf("rows = %d", len(rows))
	}
	for i := 1; i < 10; i++ {
		if strings.Contains(string(rows[i]), `class="gl-9"`) {
			t.Fatalf("lane palette not cycled: %s", rows[i])
		}
	}
}
