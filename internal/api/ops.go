package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- ops (§12.2) ----------------------------------------------------------------------

// opsTable is the frozen op/param surface (§12.2).
var opsTable = []OpSpec{
	{Op: "fsck", Params: []OpParam{{Name: "connectivity", Values: []string{"1"}}}},
	{Op: "repair"},
	{Op: "follow"},
	{Op: "rev-index", Params: []OpParam{{Name: "pack"}}},
	{Op: "compact", Params: []OpParam{{Name: "force", Values: []string{"1"}}, {Name: "base", Values: []string{"1"}}}},
	{Op: "bundle", Params: []OpParam{{Name: "strategy"}, {Name: "slot"}}},
	{Op: "checkpoint", Params: []OpParam{{Name: "trigger"}}},
	{Op: "sync"},
	{Op: "rematerialize"},
}

// bundleStrategies lists the configured strategy names/kinds (§12.2).
func bundleStrategies(h *handlers, r *http.Request) []map[string]string {
	out := []map[string]string{}
	if h.env.Cfg == nil {
		return out
	}
	for _, s := range h.env.Cfg.Bundles.Strategy {
		out = append(out, map[string]string{"name": s.Name, "kind": s.Kind})
	}
	return out
}

func (h *handlers) opsList(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	available := opsTable
	if h.env.Tasks != nil {
		if t := h.env.Tasks.Ops(); len(t) > 0 {
			available = t
		}
	}
	_, recent, err := h.env.Tasks.List(r.Context(), RepoOf(r))
	if err != nil {
		recent = []TaskRecord{}
	}
	if recent == nil {
		recent = []TaskRecord{}
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Available        []OpSpec            `json:"available"`
		Recent           []TaskRecord        `json:"recent"`
		BundleStrategies []map[string]string `json:"bundle_strategies"`
	}{Available: available, Recent: recent, BundleStrategies: bundleStrategies(h, r)})
}

// opStart begins (or joins) an op and attaches the SSE stream (§12.2/§10.3).
func (h *handlers) opStart(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthWrite) {
		return
	}
	op := r.PathValue("op")
	spec := h.findOpSpec(op)
	if spec == nil {
		writePlain(w, http.StatusNotFound, "unknown op: "+op)
		return
	}
	params := map[string]string{}
	qs := r.URL.Query()
	for _, p := range spec.Params {
		v := qs.Get(p.Name)
		if v == "" {
			if p.Values == nil { // bare param names are required (§12.2)
				writePlain(w, http.StatusBadRequest, "missing required param: "+p.Name)
				return
			}
			continue
		}
		if len(p.Values) > 0 {
			okParam := false
			for _, allowed := range p.Values {
				if allowed == v {
					okParam = true
					break
				}
			}
			if !okParam {
				writePlain(w, http.StatusBadRequest, "invalid "+p.Name+": "+v)
				return
			}
		}
		params[p.Name] = v
	}
	st, err := h.env.Tasks.Begin(r.Context(), RepoOf(r), op, params)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	s, ok := NewSSE(w, r)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	defer s.Close()
	s.pump(st)
}

func findOp(op string) *OpSpec {
	for i := range opsTable {
		if opsTable[i].Op == op {
			return &opsTable[i]
		}
	}
	return nil
}

// findOpSpec resolves an op against the Tasks-provided table first (Seam 5
// extension kinds), then the frozen table — starting is the union, so a
// listed extension op is always startable and frozen ops keep working.
func (h *handlers) findOpSpec(op string) *OpSpec {
	if h.env.Tasks != nil {
		for _, s := range h.env.Tasks.Ops() {
			if s.Op == op {
				spec := s
				return &spec
			}
		}
	}
	return findOp(op)
}

// --- tasks (§12.3) ---------------------------------------------------------------------

type tasksBody struct {
	Hostname string       `json:"hostname"`
	Running  []TaskRecord `json:"running"`
	Recent   []TaskRecord `json:"recent"`
}

func (h *handlers) tasksList(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	running, recent, err := h.env.Tasks.List(r.Context(), id)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, tasksBody{
		Hostname: h.env.Hostname,
		Running:  nonNil(running),
		Recent:   nonNil(recent),
	})
}

// taskGet: the TaskRecord JSON, or — with Accept: text/event-stream — the
// attach stream (one task packet, replay, live, terminal; §12.3).
//
// The record's OWN repo is the read authority once the id is known: a task can
// outlive or move its lane (the repo-transfer task tears the source prefix down
// mid-task, so the lane the client started on stops resolving). Narration is
// gated on the repo the record names — or the move destination it carries in
// params["to"], because the access document travelled with the prefix. Unknown
// ids keep the lane's verdict (the repo gate, then 404), byte-identical to the
// pre-transfer behavior.
func (h *handlers) taskGet(w http.ResponseWriter, r *http.Request) {
	if h.env.Tasks == nil {
		writePlain(w, http.StatusServiceUnavailable, "task table not configured")
		return
	}
	id := RepoOf(r)
	taskID := r.PathValue("id")
	if acceptsSSE(r) {
		st, found, err := h.env.Tasks.Attach(r.Context(), id, taskID)
		if err != nil {
			mapViewErr(w, err)
			return
		}
		if !found {
			// Unknown id: the lane still owns its own verdict.
			if !h.open(w, r, AuthRead) {
				return
			}
			writePlain(w, http.StatusNotFound, "unknown task: "+taskID)
			return
		}
		if !h.taskReadable(w, r, st.Record) {
			return
		}
		s, ok := NewSSE(w, r)
		if !ok {
			writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
			return
		}
		defer s.Close()
		s.pump(st)
		return
	}
	rec, ok, err := h.env.Tasks.Get(r.Context(), id, taskID)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	if !ok {
		if !h.open(w, r, AuthRead) {
			return
		}
		// 404 if unknown on this instance (records are instance-memory only).
		writePlain(w, http.StatusNotFound, "unknown task: "+taskID)
		return
	}
	if !h.taskReadable(w, r, rec) {
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, rec)
}

// taskReadable gates a known task record on the repo it names (and, for a
// move, the destination in params["to"]), never on the lane path.
func (h *handlers) taskReadable(w http.ResponseWriter, r *http.Request, rec TaskRecord) bool {
	if h.env.Access == nil {
		// No identity surface on this build: the lane gate is the only
		// authority there is (a transfer cannot exist without identity).
		return h.open(w, r, AuthRead)
	}
	p := h.env.PrincipalOf(r)
	check := func(id string) bool {
		owner, repo, ok := strings.Cut(id, "/")
		if !ok || owner == "" || repo == "" {
			return false
		}
		return h.env.Access.CheckRead(r.Context(), owner, repo, p) == nil
	}
	if check(rec.Repo) || check(rec.Params["to"]) {
		return true
	}
	mapAccessErr(w, &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"})
	return false
}

// taskRecordFrom converts an engine record (bind_wal.go uses this so the
// TaskRecord conversion lives in exactly one place).
func taskRecordFrom(id, kind, repo, hostname string, startedMs int64, ok *bool, summary string, logTail []string, params map[string]string) TaskRecord {
	return TaskRecord{
		ID:       id,
		Kind:     kind,
		Repo:     repo,
		Hostname: hostname,
		Started:  strconv.FormatInt(startedMs, 10),
		OK:       ok,
		Summary:  summary,
		LogTail:  nonNil(logTail),
		Params:   params,
	}
}

var _ = git.RepoId{} // keep the import if the table changes shape
