// WIRING — the merge queue pages (the Merge queue card;
// docs/features/22_merge_queue.md §8).
//
//	GET  /{owner}/{repo}/queue              collab/queue.html
//	POST /{owner}/{repo}/queue/enqueue      add a pull request
//	POST /{owner}/{repo}/queue/dequeue      remove one (maintainer or author)
//	POST /{owner}/{repo}/queue/pause        stop starting entries for a base
//	POST /{owner}/{repo}/queue/resume       resume a base
//	POST /{owner}/{repo}/pull/{num}/queue           enqueue from the PR panel
//	POST /{owner}/{repo}/pull/{num}/queue/dequeue   dequeue from the PR panel
//
// The PR panel itself is a partial rendered by collab.go from queueDeps.ForPR.
// Every verb re-checks authorization in the service (the merge action's
// maintain floor; dequeue also admits the PR author and its enqueuer); the
// page only hides affordances. A lost CAS or a refused gate is rendered with
// the service's named reason, never a silent no-op.
package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/mergequeue"
	"github.com/crueber/spindle/internal/server/auth"
)

// QueueDeps is the queue service seam (all methods are *mergequeue.Service's
// verbs verbatim; a nil field renders the affected page as "not wired").
type QueueDeps struct {
	List    func(ctx context.Context, owner, repo string, p auth.Principal) (*mergequeue.View, error)
	ForPR   func(ctx context.Context, owner, repo string, num int, p auth.Principal) (*mergequeue.PRView, error)
	Enqueue func(ctx context.Context, owner, repo string, p auth.Principal, in mergequeue.EnqueueInput) (*mergequeue.Entry, error)
	Dequeue func(ctx context.Context, owner, repo, base string, num int, p auth.Principal) (*mergequeue.Entry, error)
	Pause   func(ctx context.Context, owner, repo, base, reason string, p auth.Principal) error
	Resume  func(ctx context.Context, owner, repo, base string, p auth.Principal) error
}

var queueDeps QueueDeps

// BindQueue installs the queue service seam (composition, at mount time
// before the first request).
func BindQueue(d QueueDeps) { queueDeps = d }

func init() { registerGroup((*Server).mountQueue) }

func (s *Server) mountQueue() {
	m := &s.mux
	m.HandleFunc("GET /{owner}/{repo}/queue", s.queuePage)
	m.HandleFunc("GET /{owner}/{repo}/queue/{rest...}", s.collabNotFound)
	m.HandleFunc("POST /{owner}/{repo}/queue/enqueue", s.queueEnqueue)
	m.HandleFunc("POST /{owner}/{repo}/queue/dequeue", s.queueDequeue)
	m.HandleFunc("POST /{owner}/{repo}/queue/pause", s.queuePauseResume)
	m.HandleFunc("POST /{owner}/{repo}/queue/resume", s.queuePauseResume)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/queue", s.queueEnqueue)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/queue/dequeue", s.queueDequeue)
}

// --- page model ---------------------------------------------------------------

type queueRow struct {
	Number     int
	Position   int
	StateLabel string
	Chip       stateChip
	Title      string
	Author     string
	Enqueuer   string
	Reason     string
	EnqueuedAt string
	StartedAt  string
	FinishedAt string
	Href       string
	ShortMerge string
	Ref        string
	Base       string
	CanDequeue bool
}

type queueSection struct {
	Base         string
	Paused       bool
	PausedBy     string
	PausedReason string
	PausedAt     string
	Entries      []queueRow
	Recent       []queueRow
	EnqueueHref  string
	PauseHref    string
	ResumeHref   string
}

type queueAuditRow struct {
	At     string
	Actor  string
	Action string
	Base   string
	Number int
	Detail string
}

type queuePageData struct {
	Repo     *RepoChrome
	Base     string
	Sections []queueSection
	Recent   []queueRow
	Audit    []queueAuditRow
	CanMaint bool
	Error    string
	Notice   string
	AllHref  string
}

// queuePanel is the PR page's queue block.
type queuePanel struct {
	Wired         bool
	Base          string
	Paused        bool
	Entry         *mergequeue.Entry
	StateLabel    string
	Chip          stateChip
	Position      int
	Reason        string
	CanEnqueue    bool
	CanDequeue    bool
	EnqueueAction string
	DequeueAction string
	Href          string
}

const (
	queueRelBase       = "gh-readonly-queue/"
	queueStateQueued   = "queued"
	queueStateChecking = "validating"
	queueStateLanding  = "landing"
	queueStateLanded   = "landed"
	queueStateEjected  = "ejected"
	queueStateCancel   = "cancelled"
)

// queueState is the one state→label/chip mapping (text is the non-colour
// channel: a hue-blind reader gets the same fact).
func queueState(state string, position int) (string, stateChip) {
	switch state {
	case mergequeue.StatePending:
		if position > 0 {
			return queueStateQueued, stateChip{fmt.Sprintf("%s · position %d", queueStateQueued, position), "chip chip-neutral"}
		}
		return queueStateQueued, stateChip{queueStateQueued, "chip chip-neutral"}
	case mergequeue.StateValidating:
		return queueStateChecking, stateChip{queueStateChecking, "chip chip-draft"}
	case mergequeue.StateLanding:
		return queueStateLanding, stateChip{queueStateLanding, "chip chip-draft"}
	case mergequeue.StateLanded:
		return queueStateLanded, stateChip{queueStateLanded, "chip chip-merged"}
	case mergequeue.StateEjected:
		return queueStateEjected, stateChip{queueStateEjected, "chip chip-closed"}
	case mergequeue.StateCancelled:
		return queueStateCancel, stateChip{queueStateCancel, "chip chip-neutral"}
	default:
		return state, stateChip{state, "chip chip-neutral"}
	}
}

func queueRowOf(e *mergequeue.Entry, base string, canMaint bool, viewer string) queueRow {
	label, chip := queueState(e.State, e.Position)
	row := queueRow{
		Number: e.Number, Position: e.Position, StateLabel: label, Chip: chip,
		Title: e.Title, Author: e.Author, Enqueuer: e.Enqueuer, Reason: e.Reason,
		EnqueuedAt: e.EnqueuedAt, StartedAt: e.StartedAt, FinishedAt: e.FinishedAt,
		ShortMerge: shortSHA(e.MergeSHA), Ref: e.Ref, Base: base,
	}
	row.Href = "/" + base
	v := strings.ToLower(strings.TrimSpace(viewer))
	row.CanDequeue = canMaint || v == strings.ToLower(e.Author) || v == strings.ToLower(e.Enqueuer)
	return row
}

func queueRowsOf(entries []*mergequeue.Entry, base string, canMaint bool, viewer string) []queueRow {
	out := make([]queueRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, queueRowOf(e, base, canMaint, viewer))
	}
	return out
}

// buildQueueSection shapes one base queue for the page.
func buildQueueSection(q *mergequeue.QueueView, canMaint bool, viewer string) queueSection {
	sec := queueSection{
		Base: q.Base, Paused: q.Paused, PausedBy: q.PausedBy,
		PausedReason: q.PausedReason, PausedAt: q.PausedAt,
		Entries: queueRowsOf(q.Entries, q.Base, canMaint, viewer),
	}
	return sec
}

// --- page ---------------------------------------------------------------------

func (s *Server) queuePage(w http.ResponseWriter, r *http.Request) {
	s.renderQueuePage(w, r, http.StatusOK, "", "")
}

func (s *Server) renderQueuePage(w http.ResponseWriter, r *http.Request, status int, errMsg, notice string) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	base := strings.TrimSpace(r.URL.Query().Get("base"))
	data := queuePageData{Repo: nil, Base: base, CanMaint: rc.CanMaint, Error: errMsg, Notice: notice}
	if queueDeps.List == nil {
		chrome := s.collabChrome(r, rc, p)
		data.Repo = chrome
		data.Error = "merge queue service not wired"
		s.pageRepo(w, r, p, chrome, http.StatusServiceUnavailable, "collab/queue", "Merge queue", data)
		return
	}
	view, err := queueDeps.List(r.Context(), rc.Owner, rc.Name, p)
	if err != nil {
		chrome := s.collabChrome(r, rc, p)
		data.Repo = chrome
		data.Error = err.Error()
		s.pageRepo(w, r, p, chrome, collabErrStatus(err), "collab/queue", "Merge queue", data)
		return
	}
	if view != nil {
		for _, q := range view.Queues {
			if base != "" && q.Base != base {
				continue
			}
			data.Sections = append(data.Sections, buildQueueSection(q, rc.CanMaint, p.Name))
		}
		for _, e := range view.Recent {
			if base != "" && e.Base != base {
				continue
			}
			data.Recent = append(data.Recent, queueRowOf(e, e.Base, rc.CanMaint, p.Name))
		}
		// Newest audit row first; the document stores append order.
		for i := len(view.Audit) - 1; i >= 0; i-- {
			if base != "" && view.Audit[i].Base != base {
				continue
			}
			a := view.Audit[i]
			data.Audit = append(data.Audit, queueAuditRow{
				At: a.At, Actor: a.Actor, Action: a.Action, Base: a.Base, Number: a.Number, Detail: a.Detail,
			})
			if len(data.Audit) >= 25 {
				break
			}
		}
	}
	// A filtered base with no section yet still renders an empty queue so
	// a maintainer can enqueue into it.
	if base != "" && len(data.Sections) == 0 {
		data.Sections = append(data.Sections, queueSection{Base: base})
	}
	data.AllHref = "/" + rc.Full + "/queue"
	data.Repo = s.collabChrome(r, rc, p)
	s.pageRepo(w, r, p, data.Repo, status, "collab/queue", "Merge queue", data)
}

// --- write verbs --------------------------------------------------------------

func (s *Server) queueEnqueue(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "add a pull request to the merge queue")
	if !ok {
		return
	}
	base := strings.TrimSpace(r.PostFormValue("base"))
	num, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("number")))
	if num <= 0 {
		// The PR panel form carries no number field; the path does.
		num = pathNum(r)
	}
	priority, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("priority")))
	if base == "" {
		s.queueWriteError(w, r, rc, p, http.StatusBadRequest, "a base branch is required")
		return
	}
	if num <= 0 {
		s.queueWriteError(w, r, rc, p, http.StatusBadRequest, "a pull request number is required")
		return
	}
	if queueDeps.Enqueue == nil {
		s.queueWriteError(w, r, rc, p, http.StatusServiceUnavailable, "merge queue service not wired")
		return
	}
	if _, err := queueDeps.Enqueue(r.Context(), rc.Owner, rc.Name, p, mergequeue.EnqueueInput{Base: base, Number: num, Priority: priority}); err != nil {
		s.queueWriteError(w, r, rc, p, collabErrStatus(err), err.Error())
		return
	}
	s.queueRedirect(w, r, rc, base)
}

func (s *Server) queueDequeue(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "remove a pull request from the merge queue")
	if !ok {
		return
	}
	base := strings.TrimSpace(r.PostFormValue("base"))
	num, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("number")))
	if num <= 0 {
		num = pathNum(r)
	}
	if base == "" || num <= 0 {
		s.queueWriteError(w, r, rc, p, http.StatusBadRequest, "a base branch and pull request number are required")
		return
	}
	if queueDeps.Dequeue == nil {
		s.queueWriteError(w, r, rc, p, http.StatusServiceUnavailable, "merge queue service not wired")
		return
	}
	if _, err := queueDeps.Dequeue(r.Context(), rc.Owner, rc.Name, base, num, p); err != nil {
		s.queueWriteError(w, r, rc, p, collabErrStatus(err), err.Error())
		return
	}
	if pathNum(r) > 0 {
		http.Redirect(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num), http.StatusSeeOther)
		return
	}
	s.queueRedirect(w, r, rc, base)
}

func (s *Server) queuePauseResume(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabWrite(w, r, identity.RoleMaintain, "pause or resume the merge queue")
	if !ok {
		return
	}
	base := strings.TrimSpace(r.PostFormValue("base"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	pause := strings.HasSuffix(r.URL.Path, "/pause")
	var err error
	if pause {
		if queueDeps.Pause == nil {
			err = fmt.Errorf("pause not wired")
		} else {
			err = queueDeps.Pause(r.Context(), rc.Owner, rc.Name, base, reason, p)
		}
	} else {
		if queueDeps.Resume == nil {
			err = fmt.Errorf("resume not wired")
		} else {
			err = queueDeps.Resume(r.Context(), rc.Owner, rc.Name, base, p)
		}
	}
	if err != nil {
		s.queueWriteError(w, r, rc, p, collabErrStatus(err), err.Error())
		return
	}
	s.queueRedirect(w, r, rc, base)
}

func (s *Server) queueWriteError(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, status int, msg string) {
	if num := pathNum(r); num > 0 {
		s.renderPull(w, r, rc, p, num, status, msg, "")
		return
	}
	s.renderQueuePage(w, r, status, msg, "")
}

func (s *Server) queueRedirect(w http.ResponseWriter, r *http.Request, rc collabRepo, base string) {
	target := "/" + rc.Full + "/queue"
	if base != "" {
		target += "?base=" + url.QueryEscape(base)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// pathNum extracts the {num} path value (0 when absent).
func pathNum(r *http.Request) int {
	n, err := strconv.Atoi(r.PathValue("num"))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// queuePanelFor builds the PR page's queue block (nil = the service is not
// wired or the PR is terminal, so the template renders nothing).
func (s *Server) queuePanelFor(ctx context.Context, rc collabRepo, p auth.Principal, num int, baseRef string, terminal bool) *queuePanel {
	if queueDeps.ForPR == nil || terminal || num <= 0 {
		return nil
	}
	v, err := queueDeps.ForPR(ctx, rc.Owner, rc.Name, num, p)
	if err != nil || v == nil {
		return nil
	}
	base := v.Base
	if base == "" {
		base = strings.TrimPrefix(strings.TrimSpace(baseRef), "refs/heads/")
	}
	panel := &queuePanel{
		Wired: true, Base: base, Paused: v.Paused, Entry: v.Entry,
		Href:          "/" + rc.Full + "/queue?base=" + url.QueryEscape(base),
		EnqueueAction: fmt.Sprintf("/%s/pull/%d/queue", rc.Full, num),
		DequeueAction: fmt.Sprintf("/%s/pull/%d/queue/dequeue", rc.Full, num),
	}
	if v.Entry != nil {
		panel.Position = v.Entry.Position
		panel.StateLabel, panel.Chip = queueState(v.Entry.State, v.Entry.Position)
		panel.Reason = v.Entry.Reason
		active := v.Entry.State == mergequeue.StatePending || v.Entry.State == mergequeue.StateValidating || v.Entry.State == mergequeue.StateLanding
		pn := strings.ToLower(strings.TrimSpace(p.Name))
		panel.CanDequeue = active && (rc.CanMaint || pn == strings.ToLower(v.Entry.Author) || pn == strings.ToLower(v.Entry.Enqueuer))
		panel.CanEnqueue = rc.CanMaint && !active
		return panel
	}
	panel.CanEnqueue = rc.CanMaint
	return panel
}
