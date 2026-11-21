// WIRING — Threads pages (docs/features/15_threads.md §6): the repository run
// list and one run's jobs.
//
//	GET /{owner}/{repo}/threads            collab/threads.html
//	GET /{owner}/{repo}/threads/{run}      collab/thread_detail.html
//
// The pages read through the same service methods the JSON API uses
// (ThreadsDeps binds *threads.Service.ListRuns/GetRun from composition), so a
// page and its API twin can never disagree about gating or shape. Listing is
// gated by the repository read gate (collabEnv); the pages are read-only in T1
// — dispatch is the JSON API's POST, and cancel/rerun arrive with the T2
// runner protocol.
package webui

import (
	"context"
	"net/http"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// ThreadsDeps is the Threads page seam (composition binds the service methods
// with the repo id joined).
type ThreadsDeps struct {
	List func(ctx context.Context, owner, repo string, p auth.Principal, after string, n int) (*threads.RunPage, error)
	Get  func(ctx context.Context, owner, repo, runID string, p auth.Principal) (*threads.RunView, error)
}

var threadsDeps ThreadsDeps

// BindThreads installs the Threads service seam (composition, once at mount
// time).
func BindThreads(d ThreadsDeps) { threadsDeps = d }

func init() { registerGroup((*Server).mountThreads) }

// mountThreads registers the Threads page family. The run wildcard is a single
// segment; the family catch-all answers 404 for deeper paths so the server's
// repo dispatch never inherits a half-claimed family.
func (s *Server) mountThreads() {
	m := &s.mux
	m.HandleFunc("GET /{owner}/{repo}/threads", s.threadsList)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}", s.threadRun)
	m.HandleFunc("GET /{owner}/{repo}/threads/{rest...}", s.collabNotFound)
}

// threadRunRow is one list row.
type threadRunRow struct {
	ID          string
	Short       string
	Href        string
	Commit      string
	ShortCommit string
	Event       string
	Workflow    string
	Branch      string
	State       string
	Trusted     bool
	UpdatedAt   string
}

// threadsListData is the run-list page model.
type threadsListData struct {
	Repo collabRepo

	Rows        []threadRunRow
	More        bool
	After       string
	OlderHref   string
	RefreshHref string

	Error string
}

// threadsList renders the repository's runs, newest first.
func (s *Server) threadsList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	data := threadsListData{
		Repo:        rc,
		RefreshHref: r.URL.RequestURI(),
		After:       r.URL.Query().Get("after"),
	}
	if threadsDeps.List == nil {
		data.Error = "threads service not wired"
		s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusServiceUnavailable, "collab/threads", "Threads", data)
		return
	}
	page, err := threadsDeps.List(r.Context(), rc.Owner, rc.Name, p, data.After, 50)
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.pageRepo(w, r, p, s.collabChrome(r, rc, p), collabErrStatus(err), "collab/threads", "Threads", data)
		return
	}
	data.More = page.More
	for _, run := range page.Runs {
		data.Rows = append(data.Rows, threadRunRow{
			ID: run.ID, Short: shortRunID(run.ID), Href: "/" + rc.Full + "/threads/" + run.ID,
			Commit: run.Commit, ShortCommit: shortSHA(run.Commit),
			Event: run.Event, Workflow: run.Workflow, Branch: run.Branch,
			State: run.State, Trusted: run.Trusted, UpdatedAt: run.UpdatedAt,
		})
	}
	if data.More && len(page.Runs) > 0 {
		data.OlderHref = pagerHref(r, "after", page.Runs[len(page.Runs)-1].ID)
	}
	s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusOK, "collab/threads", "Threads", data)
}

// threadRunData is the run-detail page model.
type threadRunData struct {
	Repo collabRepo
	Run  *threads.Run
	Jobs []*threads.Job
	// ShortCommit is the 10-char commit link text; ShortRun is the display
	// run id.
	ShortCommit string
	ShortRun    string
	CommitHref  string
}

// threadRun renders one run and its jobs.
func (s *Server) threadRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if threadsDeps.Get == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	view, err := threadsDeps.Get(r.Context(), rc.Owner, rc.Name, runID, p)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	data := threadRunData{
		Repo: rc, Run: view.Run, Jobs: view.Jobs,
		ShortCommit: shortSHA(view.Run.Commit),
		ShortRun:    shortRunID(view.Run.ID),
		CommitHref:  "/" + rc.Full + "/commit/" + view.Run.Commit,
	}
	s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusOK, "collab/thread_detail", "Run "+data.ShortRun, data)
}

// shortRunID renders a compact run id: the random tail plus the time's
// minutes are noise, so the display form is the last 10 chars of the id.
func shortRunID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 10 {
		return id[len(id)-10:]
	}
	return id
}

// runDotState maps a run/job state onto the shared check dot vocabulary
// (success/pending/failure/error; queued and running are in flight).
func runDotState(state string) string {
	switch state {
	case threads.RunSuccess:
		return "success"
	case threads.RunFailure:
		return "failure"
	case threads.RunCancelled:
		return "error"
	default:
		return "pending"
	}
}
