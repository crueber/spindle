package threads

// PAR-191: the run graph is a read model over the job records. These cases pin
// the three claims the page makes: a matrix job is ONE node holding its cells,
// an edge's state is the dependency it names, and a dependent whose dependency
// did not succeed is marked blocked with the key that stopped it.

import "testing"

// graphJob builds one stored-shaped job record.
func graphJob(id, key, name, state string, needs []string, matrix map[string]string, group string) *Job {
	spec := &Spec{Matrix: matrix, MatrixGroup: group}
	return &Job{
		ID: id, RunID: "run-1", Repo: "o/r", Key: key, Name: name,
		Context: "threads/ci/" + key, State: state, Needs: needs, Spec: spec,
	}
}

func findGraphNode(g *RunGraph, key string) *RunGraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].Key == key {
			return &g.Nodes[i]
		}
	}
	return nil
}

func TestRunGraphGroupsMatrixCellsUnderTheirJob(t *testing.T) {
	jobs := []*Job{
		graphJob("j-build-0", "build (1.22)", "build (1.22)", JobSuccess, nil, map[string]string{"go": "1.22"}, "build"),
		graphJob("j-build-1", "build (1.23)", "build (1.23)", JobFailure, nil, map[string]string{"go": "1.23"}, "build"),
		graphJob("j-test", "test", "test", JobSkipped, []string{"build"}, nil, ""),
	}
	g := BuildRunGraph(jobs)
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d, want one per workflow job key: %+v", len(g.Nodes), g.Nodes)
	}
	build := findGraphNode(g, "build")
	if build == nil || len(build.Cells) != 2 {
		t.Fatalf("build node = %+v, want both cells grouped", build)
	}
	if build.State != JobFailure {
		t.Fatalf("build state = %s, want failure (one cell failed)", build.State)
	}
	if len(g.Edges) != 1 || g.Edges[0].From != "build" || g.Edges[0].To != "test" || g.Edges[0].State != JobFailure {
		t.Fatalf("edges = %+v", g.Edges)
	}
	test := findGraphNode(g, "test")
	if test == nil || !test.Blocked || len(test.BlockedBy) != 1 || test.BlockedBy[0] != "build" {
		t.Fatalf("test node = %+v, want blocked by build", test)
	}
	if build.Level != 0 || test.Level != 1 {
		t.Fatalf("levels build=%d test=%d, want 0/1", build.Level, test.Level)
	}
	// A cell's own check context and matrix survive the grouping: the page's
	// "one row per cell with its check and duration" is this data.
	seen := map[string]bool{}
	for _, c := range build.Cells {
		seen[c.Context] = true
		if c.Matrix["go"] == "" {
			t.Fatalf("cell lost its matrix values: %+v", c)
		}
	}
	if !seen["threads/ci/build (1.22)"] || !seen["threads/ci/build (1.23)"] {
		t.Fatalf("cell contexts = %+v", seen)
	}
}

func TestRunGraphLongestPathLevels(t *testing.T) {
	jobs := []*Job{
		graphJob("a", "a", "a", JobSuccess, nil, nil, ""),
		graphJob("b", "b", "b", JobSuccess, []string{"a"}, nil, ""),
		graphJob("c", "c", "c", JobSuccess, []string{"a"}, nil, ""),
		graphJob("d", "d", "d", JobQueued, []string{"b", "c"}, nil, ""),
	}
	g := BuildRunGraph(jobs)
	for key, want := range map[string]int{"a": 0, "b": 1, "c": 1, "d": 2} {
		if n := findGraphNode(g, key); n == nil || n.Level != want {
			t.Fatalf("level(%s) = %+v, want %d", key, n, want)
		}
	}
	if g.Nodes[0].Key != "a" {
		t.Fatalf("first node = %s, want the root first", g.Nodes[0].Key)
	}
}

func TestRunGraphCycleDoesNotRecurseForever(t *testing.T) {
	jobs := []*Job{
		graphJob("a", "a", "a", JobQueued, []string{"b"}, nil, ""),
		graphJob("b", "b", "b", JobQueued, []string{"a"}, nil, ""),
	}
	g := BuildRunGraph(jobs)
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d", len(g.Nodes))
	}
	// A corrupt record cycle must still render (levels stop deepening) instead
	// of hanging the page.
	for _, n := range g.Nodes {
		if n.Level < 0 || n.Level > 2 {
			t.Fatalf("cycle level = %d", n.Level)
		}
	}
}

func TestRunGraphToleratedFailureStaysGreen(t *testing.T) {
	jobs := []*Job{
		graphJob("j", "j", "j", JobFailure, nil, nil, ""),
	}
	jobs[0].Tolerated = true
	if g := BuildRunGraph(jobs); g.Nodes[0].State != JobSuccess {
		t.Fatalf("a tolerated failure folded to %s, want success (the run fold's rule)", g.Nodes[0].State)
	}
}

func TestRunGraphSkipsAMissingDependency(t *testing.T) {
	jobs := []*Job{graphJob("j", "j", "j", JobQueued, []string{"ghost"}, nil, "")}
	g := BuildRunGraph(jobs)
	if len(g.Edges) != 0 {
		t.Fatalf("a needs: naming no record became an edge: %+v", g.Edges)
	}
	if n := g.Nodes[0]; len(n.Needs) != 1 || n.Needs[0] != "ghost" {
		t.Fatalf("needs = %+v, want the declared key kept", n.Needs)
	}
}

func TestRunGraphGroupsAReusableCallUnderItsCaller(t *testing.T) {
	calleeA := graphJob("j-a", "callee-a", "callee-a", JobSuccess, nil, nil, "")
	calleeA.Spec.Call = &callSpec{Caller: "fanout"}
	calleeB := graphJob("j-b", "callee-b", "callee-b", JobSuccess, nil, nil, "")
	calleeB.Spec.Call = &callSpec{Caller: "fanout"}
	after := graphJob("j-c", "after", "after", JobQueued, []string{"fanout"}, nil, "")
	g := BuildRunGraph([]*Job{calleeA, calleeB, after})
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d, want the call as one node: %+v", len(g.Nodes), g.Nodes)
	}
	call := findGraphNode(g, "fanout")
	if call == nil || len(call.Cells) != 2 {
		t.Fatalf("call node = %+v", call)
	}
	if dep := findGraphNode(g, "after"); dep == nil || len(dep.Needs) != 1 || dep.Needs[0] != "fanout" {
		t.Fatalf("dependent = %+v", dep)
	}
}

func TestRunGraphMixedSkippedCellsAreSuccess(t *testing.T) {
	jobs := []*Job{
		graphJob("j-a-0", "a (1)", "a (1)", JobSuccess, nil, map[string]string{"x": "1"}, "a"),
		graphJob("j-a-1", "a (2)", "a (2)", JobSkipped, nil, map[string]string{"x": "2"}, "a"),
	}
	if g := BuildRunGraph(jobs); g.Nodes[0].State != JobSuccess {
		t.Fatalf("mixed success/skip folded to %s, want success", g.Nodes[0].State)
	}
	allSkip := []*Job{
		graphJob("j-a-0", "a (1)", "a (1)", JobSkipped, nil, map[string]string{"x": "1"}, "a"),
		graphJob("j-a-1", "a (2)", "a (2)", JobSkipped, nil, map[string]string{"x": "2"}, "a"),
	}
	if g := BuildRunGraph(allSkip); g.Nodes[0].State != JobSkipped {
		t.Fatalf("all-skipped folded to %s, want skipped", g.Nodes[0].State)
	}
}

func TestRunGraphDeduplicatesADeclaredDependency(t *testing.T) {
	jobs := []*Job{
		graphJob("j-a", "a", "a", JobSuccess, nil, nil, ""),
		graphJob("j-b", "b", "b", JobQueued, []string{"a", "a"}, nil, ""),
	}
	g := BuildRunGraph(jobs)
	if dep := findGraphNode(g, "b"); dep == nil || len(dep.Needs) != 1 {
		t.Fatalf("duplicate needs not deduped: %+v", dep)
	}
}

func TestGraphKeyOfANilJob(t *testing.T) {
	if got := graphKey(nil); got != "" {
		t.Fatalf("graphKey(nil) = %q", got)
	}
}

func TestGraphHelpersCoverTheFallbacks(t *testing.T) {
	if got := graphNodeName(&Job{Key: "k"}, "k"); got != "k" {
		t.Fatalf("unnamed plain job = %q, want its key", got)
	}
	grouped := &Job{Key: "build (1)", Name: "build (1)", Spec: &Spec{MatrixGroup: "build"}}
	if got := graphNodeName(grouped, "build"); got != "build" {
		t.Fatalf("matrix node name = %q, want the group key", got)
	}
	called := &Job{Key: "callee", Name: "callee", Spec: &Spec{Call: &callSpec{Caller: "fanout"}}}
	if got := graphNodeName(called, "fanout"); got != "fanout" {
		t.Fatalf("call node name = %q, want the caller key", got)
	}
	if got := foldGraphState(nil); got != JobQueued {
		t.Fatalf("empty fold = %q, want queued", got)
	}
	// A nil record in the slice is skipped, not a node.
	if g := BuildRunGraph([]*Job{nil, graphJob("j", "j", "j", JobSuccess, nil, nil, "")}); len(g.Nodes) != 1 {
		t.Fatalf("nil job produced a node: %+v", g.Nodes)
	}
}
