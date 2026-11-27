package threads

// Run graph (PAR-191): the run's job dependency DAG, at workflow-job-key
// granularity. A matrix job is ONE node holding its cells, because that is how
// the workflow and `needs:` name it; a reusable-workflow call is one node
// holding the callee jobs, because that is the key a dependent waits on. Every
// cell still carries its own check context and duration, so "one row per cell"
// survives inside the node.
//
// The graph is truthful about blocking: an edge's state is the dependency's
// folded state, and a node whose dependency is not green is marked blocked with
// the failing keys named. It is a read model derived from the job records (no
// stored graph, no second source of truth).

import (
	"sort"
	"strings"
)

// RunGraph is one run's dependency graph.
type RunGraph struct {
	Nodes []RunGraphNode `json:"nodes"`
	Edges []RunGraphEdge `json:"edges"`
}

// RunGraphNode is one workflow job (a matrix job's cells are grouped here).
type RunGraphNode struct {
	// Key is the workflow job key a `needs:` names.
	Key  string `json:"key"`
	Name string `json:"name"`
	// State is the folded cell state (see foldGraphState).
	State string `json:"state"`
	// Needs names the keys this node waits on (declaration order preserved).
	Needs []string `json:"needs"`
	// Level is the longest dependency depth (0 = no needs), used by the page to
	// lay the graph out left to right.
	Level int `json:"level"`
	// Blocked is true when a dependency did not succeed, so this node cannot
	// run (or did not).
	Blocked bool `json:"blocked,omitempty"`
	// BlockedBy names the failed/skipped dependencies (empty when not blocked).
	BlockedBy []string `json:"blocked_by,omitempty"`
	// Reason is the node's own explanation when it did not run (a blocked
	// `needs:`, a false `if:`), joined from its cells.
	Reason string         `json:"reason,omitempty"`
	Cells  []RunGraphCell `json:"cells"`
}

// RunGraphCell is one job record: a plain job's single cell, or one matrix
// expansion. Its check context and duration are what the page's cell row shows.
type RunGraphCell struct {
	JobID           string            `json:"job_id"`
	Name            string            `json:"name"`
	Context         string            `json:"context"`
	State           string            `json:"state"`
	Matrix          map[string]string `json:"matrix,omitempty"`
	DurationSeconds int64             `json:"duration_seconds,omitempty"`
	QueueReason     string            `json:"queue_reason,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Tolerated       bool              `json:"tolerated,omitempty"`
}

// RunGraphEdge is one dependency: From (the dependency key) → To (the
// dependent key). State is the dependency's folded state, so a reader can see
// which edge is holding the dependent.
type RunGraphEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"`
}

// graphKey is the key a node is grouped under: the reusable caller when the
// job belongs to a call, else the matrix group, else the job's own key. It
// matches needsIndex, so the graph and the scheduler agree about what a
// `needs:` names.
func graphKey(j *Job) string {
	if j == nil {
		return ""
	}
	if j.Spec != nil && j.Spec.Call != nil && j.Spec.Call.Caller != "" {
		return j.Spec.Call.Caller
	}
	if j.Spec != nil && j.Spec.MatrixGroup != "" {
		return j.Spec.MatrixGroup
	}
	return j.Key
}

// BuildRunGraph folds sorted job records into the run graph. It never invents
// a node: a `needs:` naming a key with no job record (a corrupt or older
// record) is not drawn as an edge, and the node keeps the name in Needs.
func BuildRunGraph(jobs []*Job) *RunGraph {
	g := &RunGraph{}
	byKey := map[string]*RunGraphNode{}
	order := []string{}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		key := graphKey(job)
		node := byKey[key]
		if node == nil {
			node = &RunGraphNode{Key: key, Name: graphNodeName(job, key), Needs: []string{}}
			byKey[key] = node
			order = append(order, key)
		}
		cell := RunGraphCell{
			JobID: job.ID, Name: job.Name, Context: job.Context, State: job.State,
			Matrix: job.Spec.matrixCell(), DurationSeconds: job.DurationSeconds,
			QueueReason: job.QueueReason, Reason: job.Reason, Tolerated: job.Tolerated,
		}
		node.Cells = append(node.Cells, cell)
		for _, need := range job.Needs {
			if !stringInSlice(node.Needs, need) {
				node.Needs = append(node.Needs, need)
			}
		}
		if node.Reason == "" {
			node.Reason = job.Reason
		}
	}
	// Fold each node, then resolve levels and blocked dependents against the
	// folded dependency states (one pass after every node exists).
	for _, key := range order {
		byKey[key].State = foldGraphState(byKey[key].Cells)
	}
	for _, key := range order {
		node := byKey[key]
		node.Level = graphLevel(key, byKey, map[string]bool{}, 0)
		for _, need := range node.Needs {
			dep, ok := byKey[need]
			if !ok || dep == nil {
				continue
			}
			g.Edges = append(g.Edges, RunGraphEdge{From: need, To: key, State: dep.State})
			if dep.State != JobSuccess {
				node.Blocked = true
				node.BlockedBy = append(node.BlockedBy, need)
			}
		}
		g.Nodes = append(g.Nodes, *node)
	}
	sort.SliceStable(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Level != g.Nodes[j].Level {
			return g.Nodes[i].Level < g.Nodes[j].Level
		}
		return false // workflow order within a level
	})
	return g
}

// graphNodeName is the node's display name: the caller key for a reusable
// call, the job key for a matrix group, the record's name for a plain job.
func graphNodeName(job *Job, key string) string {
	if job.Spec != nil && ((job.Spec.Call != nil && job.Spec.Call.Caller != "") || job.Spec.MatrixGroup != "") {
		return key
	}
	if strings.TrimSpace(job.Name) != "" {
		return job.Name
	}
	return key
}

// foldGraphState folds a node's cell states the way the run fold does: any hard
// failure wins, then a cancellation, then any live cell keeps it live, skipped
// cells are neutral. Tolerated failures do not make the node red (they do not
// make the run red either), but the cell row still says `failure · tolerated`.
func foldGraphState(cells []RunGraphCell) string {
	if len(cells) == 0 {
		return JobQueued
	}
	anyQueued, anyLive, anyFailed, anyCancelled, anySkipped := true, false, false, false, false
	for _, c := range cells {
		if c.State != JobQueued {
			anyQueued = false
		}
		if !terminal(c.State) {
			anyLive = true
		}
		if (c.State == JobFailure || c.State == JobTimedOut) && !c.Tolerated {
			anyFailed = true
		}
		if c.State == JobCancelled {
			anyCancelled = true
		}
		if c.State == JobSkipped {
			anySkipped = true
		}
	}
	switch {
	case anyQueued:
		return JobQueued
	case anyLive:
		return JobRunning
	case anyFailed:
		return JobFailure
	case anyCancelled:
		return JobCancelled
	case anySkipped && allSkipped(cells):
		return JobSkipped
	default:
		return JobSuccess
	}
}

// allSkipped reports whether every cell was skipped (a node whose cells are a
// mix of success and skip is a success, like the run fold).
func allSkipped(cells []RunGraphCell) bool {
	for _, c := range cells {
		if c.State != JobSkipped {
			return false
		}
	}
	return true
}

// graphLevel is the longest dependency depth, cycle-guarded: a malformed record
// cycle stops deepening instead of recursing forever.
func graphLevel(key string, byKey map[string]*RunGraphNode, seen map[string]bool, depth int) int {
	node := byKey[key]
	if node == nil || len(node.Needs) == 0 || depth > len(byKey) {
		return 0
	}
	if seen[key] {
		return 0
	}
	seen[key] = true
	level := 0
	for _, need := range node.Needs {
		if _, ok := byKey[need]; !ok {
			continue
		}
		if l := graphLevel(need, byKey, seen, depth+1) + 1; l > level {
			level = l
		}
	}
	delete(seen, key)
	return level
}

// stringInSlice reports membership in a string slice (small lists,
// declaration order matters).
func stringInSlice(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// matrixCell returns the node cell's matrix values (nil for a plain job).
func (s *Spec) matrixCell() map[string]string {
	if s == nil || len(s.Matrix) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.Matrix))
	for k, v := range s.Matrix {
		out[k] = v
	}
	return out
}
