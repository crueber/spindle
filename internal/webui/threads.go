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
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// ThreadsDeps is the Threads page seam (composition binds the service methods
// with the repo id joined).
type ThreadsDeps struct {
	List func(ctx context.Context, owner, repo string, p auth.Principal, after string, n int, filter threads.RunFilter) (*threads.RunPage, error)
	Get  func(ctx context.Context, owner, repo, runID string, p auth.Principal) (*threads.RunView, error)
	// Workflows lists the discovered workflow files and their per-file parse
	// state at commit ("" = the default branch tip). It is the page's error
	// surface: a broken file is a row with its error, never a silent no-op.
	Workflows func(ctx context.Context, owner, repo, commit string, p auth.Principal) (*threads.WorkflowList, error)
	// Cancel/Rerun are the operator verbs (15 §3). The page handler gates on
	// the repository read role to reach the tab; the service re-checks the
	// write role, so a reader's POST is a real 403 (the API's own gate).
	Cancel func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	Rerun  func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	// Dispatch is the manual trigger (15 §6, round 2): the page's Run workflow
	// form posts ref + workflow + declared inputs through the SAME service call
	// as POST …/api/threads. The service re-checks the write role, so the form
	// is an affordance, not the gate.
	Dispatch func(ctx context.Context, owner, repo, ref, workflow string, inputs map[string]string, p auth.Principal) ([]*threads.Run, error)
	// Approve/Deny are the first-time-contributor gate's decisions (15 §5.2).
	// The service re-checks the maintain role, so the control is an affordance.
	Approve func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	Deny    func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	// Role resolves the caller's repo role for the verb affordance (nil =
	// only a write-capable principal sees the buttons).
	Role func(ctx context.Context, owner, repo string, p auth.Principal) (string, error)
	// OpenArtifact opens one run artifact for the ZIP download; the service
	// checks the repository read gate and the expiry, so the page handler never
	// decides access itself (15 §2.5).
	OpenArtifact func(ctx context.Context, owner, repo, runID, name string, p auth.Principal) (*threads.ArtifactDownload, error)
	// Environments lists the repository's environment names for the
	// `environment`-typed dispatch picker (PAR-023). Nil leaves the picker
	// empty; the service still validates the submitted value.
	Environments func(ctx context.Context, owner, repo string, p auth.Principal) ([]string, error)
	// Settings/UpdateSettings are the per-repository Threads switches (PAR-015,
	// PAR-118, PAR-153). The service enforces maintain+ and audits every change;
	// the page renders the values and the form.
	Settings       func(ctx context.Context, owner, repo string, p auth.Principal) (*threads.SettingsView, error)
	UpdateSettings func(ctx context.Context, owner, repo string, in threads.SettingsInput, p auth.Principal) (*threads.SettingsView, error)
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
	m.HandleFunc("POST /{owner}/{repo}/threads/dispatch", s.threadsDispatch)
	m.HandleFunc("POST /{owner}/{repo}/threads/settings", s.threadsSettings)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}", s.threadRun)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}/artifacts/{artifact}", s.threadArtifactDownload)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/cancel", s.threadRunCancel)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/rerun", s.threadRunRerun)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/approve", s.threadRunApprove)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/deny", s.threadRunDeny)
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
	// DurationSeconds is the run's derived wall time (0 = not started or too
	// short to render).
	DurationSeconds int64
	// Actor is the trigger principal (the run-list actor filter and column).
	Actor string
	// PendingEnvironments names the deployments a held run waits for, and
	// ApprovalKind says whether the hold is a deployment or the fork gate
	// (PAR-135: the badge names the environment, never a bare "pending").
	PendingEnvironments []string
	ApprovalKind        string
	// PendingGroup is the concurrency group this run waits for (PAR-152), so a
	// queued run behind another says why instead of showing a silent spinner.
	PendingGroup string
	// HoldReason names a capacity hold (PAR-153); PRTarget/WriteTokens carry
	// the two per-repository security decisions a reader must see (PAR-015,
	// PAR-118). Ref is the triggering ref (refs/pull/N/merge for a PR).
	HoldReason  string
	PRTarget    bool
	WriteTokens bool
	Ref         string
}

// threadsListData is the run-list page model.
type threadsListData struct {
	Repo collabRepo

	Rows        []threadRunRow
	More        bool
	After       string
	OlderHref   string
	RefreshHref string

	// Workflows is the discovered set at the default branch tip, with each
	// file's parse state (a broken file shows its error and the others still
	// list). WorkflowsError reports a discovery failure without hiding runs.
	Workflows      []threadWorkflowRow
	WorkflowsError string

	// Run workflow (round 2): the manual dispatch control. Dispatch lists the
	// files that declare `workflow_dispatch`; DispatchSelected is the file whose
	// declared inputs are rendered (chosen with GET ?dispatch=<path>, default
	// the first). The form posts ref + workflow + inputs to
	// POST …/threads/dispatch, the page twin of POST …/api/threads.
	CanWrite         bool
	Dispatch         []threadDispatchWorkflow
	DispatchSelected string
	DispatchRef      string
	DispatchInputs   []threadDispatchInput
	DispatchError    string
	DispatchNotice   string

	// Run-list filters (round-2 pass 2): workflow · state · actor. The form is
	// a plain GET, so the filter lives in the URL and a link keeps it.
	FilterWorkflow string
	FilterState    string
	FilterActor    string
	States         []string
	FilterActive   bool
	// FilterWorkflowOptions is the workflow select's option list (the
	// discovered workflows plus the active value when it is not discovered).
	FilterWorkflowOptions []string

	// Settings is the repository's Threads switches (PAR-015/PAR-118/PAR-153);
	// CanMaintain gates the edit form (the service re-checks and audits).
	Settings      *threads.SettingsView
	SettingsError string
	CanMaintain   bool
	Notice        string

	Error string
}

// threadDispatchWorkflow is one option of the Run workflow select.
type threadDispatchWorkflow struct {
	Path  string
	Name  string
	Label string
}

// threadDispatchInput is one declared workflow_dispatch input as a form row.
type threadDispatchInput struct {
	Name        string
	Type        string
	Description string
	Required    bool
	Default     string
	Options     []string
	Value       string // submitted (or default) value on a re-render
}

// threadWorkflowRow is one discovered workflow file.
type threadWorkflowRow struct {
	Path     string
	Name     string
	Anchor   string
	Triggers []string
	Error    string
	// Dispatch marks a file that declares workflow_dispatch; Inputs is its
	// declared input schema (the Run workflow form renders it).
	Dispatch bool
	Inputs   []threads.DispatchInput
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
	data.Workflows, data.WorkflowsError = threadWorkflowRows(r.Context(), rc, p)
	data.States = []string{threads.RunQueued, threads.RunRunning, threads.RunPendingApproval, threads.RunSuccess, threads.RunFailure, threads.RunCancelled, threads.RunSkipped}
	data.FilterWorkflow = strings.TrimSpace(r.URL.Query().Get("workflow"))
	data.FilterState = strings.TrimSpace(r.URL.Query().Get("state"))
	data.FilterActor = strings.TrimSpace(r.URL.Query().Get("actor"))
	data.FilterActive = data.FilterWorkflow != "" || data.FilterState != "" || data.FilterActor != ""
	for _, wf := range data.Workflows {
		data.FilterWorkflowOptions = append(data.FilterWorkflowOptions, wf.Name)
	}
	data.CanWrite = threadCanWrite(r.Context(), rc, p)
	data.CanMaintain = threadCanMaintain(r.Context(), rc, p)
	data.Notice = r.URL.Query().Get("notice")
	if threadsDeps.Settings != nil {
		if settings, serr := threadsDeps.Settings(r.Context(), rc.Owner, rc.Name, p); serr == nil {
			data.Settings = settings
		} else {
			data.SettingsError = serr.Error()
		}
	}
	data.DispatchRef = threadDispatchDefaultRef(rc)
	threadDispatchModel(&data, r.URL.Query().Get("dispatch"), nil, threadEnvironmentNames(r.Context(), rc, p, data.Workflows))
	if threadsDeps.List == nil {
		data.Error = "threads service not wired"
		s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusServiceUnavailable, "collab/threads", "Threads", data)
		return
	}
	page, err := threadsDeps.List(r.Context(), rc.Owner, rc.Name, p, data.After, 50, threads.RunFilter{
		Workflow: data.FilterWorkflow, State: data.FilterState, Actor: data.FilterActor,
	})
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
			DurationSeconds: run.DurationSeconds, Actor: run.Actor,
			PendingEnvironments: run.Environments, ApprovalKind: run.ApprovalKind,
			PendingGroup: run.PendingGroup, HoldReason: run.HoldReason,
			PRTarget: run.PRTarget, WriteTokens: run.WriteTokens, Ref: run.Ref,
		})
	}
	if data.More && len(page.Runs) > 0 {
		data.OlderHref = pagerHref(r, "after", page.Runs[len(page.Runs)-1].ID)
	}
	s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusOK, "collab/threads", "Threads", data)
}

// threadDispatchModel fills the Run workflow model: the dispatchable files, the
// selected one's declared inputs, and (when submitted != nil) the values the
// dispatcher posted, so an inline error re-render keeps them.
func threadDispatchModel(data *threadsListData, selected string, submitted map[string]string, environments []string) {
	selected = strings.TrimSpace(selected)
	var chosen *threadWorkflowRow
	for i := range data.Workflows {
		wf := &data.Workflows[i]
		if !wf.Dispatch {
			continue
		}
		data.Dispatch = append(data.Dispatch, threadDispatchWorkflow{
			Path: wf.Path, Name: wf.Name, Label: threadDispatchLabel(wf),
		})
		if chosen == nil || wf.Path == selected {
			if chosen == nil {
				chosen = wf
			}
			if wf.Path == selected {
				chosen = wf
			}
		}
	}
	if chosen == nil {
		data.DispatchSelected = ""
		return
	}
	data.DispatchSelected = chosen.Path
	for _, in := range chosen.Inputs {
		row := threadDispatchInput{
			Name: in.Name, Type: in.Type, Description: in.Description,
			Required: in.Required, Default: in.Default, Options: in.Options,
			Value: in.Default,
		}
		if in.Type == "environment" {
			// The picker's choices are the repository's environments, resolved
			// server-side (the workflow file names no options).
			row.Options = environments
		}
		if submitted != nil {
			row.Value = submitted[in.Name]
		}
		data.DispatchInputs = append(data.DispatchInputs, row)
	}
}

// threadEnvironmentNames resolves the picker list for an `environment` input,
// and only when a discovered workflow actually declares one (no LIST for the
// ordinary dispatch page).
func threadEnvironmentNames(ctx context.Context, rc collabRepo, p auth.Principal, workflows []threadWorkflowRow) []string {
	if threadsDeps.Environments == nil {
		return nil
	}
	needed := false
	for _, wf := range workflows {
		for _, in := range wf.Inputs {
			if in.Type == "environment" {
				needed = true
				break
			}
		}
	}
	if !needed {
		return nil
	}
	names, err := threadsDeps.Environments(ctx, rc.Owner, rc.Name, p)
	if err != nil {
		return nil
	}
	return names
}

// threadDispatchLabel is the select option text: the workflow's display name
// plus its path, or just the path when the name is only the file stem (no
// duplicated "ci — .spindle/workflows/ci.yml").
func threadDispatchLabel(wf *threadWorkflowRow) string {
	stem := strings.TrimSuffix(strings.TrimSuffix(path.Base(wf.Path), ".yml"), ".yaml")
	if wf.Name != "" && wf.Name != stem {
		return wf.Name + " — " + wf.Path
	}
	return wf.Path
}

// threadDispatchDefaultRef is the ref the form pre-fills: the default branch's
// short name when known, else "main".
func threadDispatchDefaultRef(rc collabRepo) string {
	if label := strings.TrimSpace(rc.HeadLabel); label != "" && !looksLikeSHA(label) {
		return label
	}
	return "main"
}

// looksLikeSHA reports a full-ish hex sha (the HeadLabel fallback for a repo
// whose default branch name is unknown).
func looksLikeSHA(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// threadsDispatch answers POST …/threads/dispatch: the Run workflow form. It
// calls the same service method as the JSON API, then redirects to the created
// run. A validation failure re-renders the page with the error (and the
// submitted values) inline — never a bare error page for a form the user can
// fix.
func (s *Server) threadsDispatch(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		s.collabError(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	if threadsDeps.Dispatch == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	workflow := strings.TrimSpace(r.PostForm.Get("workflow"))
	ref := strings.TrimSpace(r.PostForm.Get("ref"))
	submitted := map[string]string{}
	for key, vals := range r.PostForm {
		name, ok := strings.CutPrefix(key, "input.")
		if !ok || len(vals) == 0 {
			continue
		}
		submitted[name] = vals[0]
	}
	if ref == "" {
		ref = threadDispatchDefaultRef(rc)
	}
	runs, err := threadsDeps.Dispatch(r.Context(), rc.Owner, rc.Name, ref, workflow, submitted, p)
	if err == nil && len(runs) == 0 {
		err = fmt.Errorf("%w: no workflow_dispatch workflow matched", threads.ErrNotFound)
	}
	if err == nil {
		http.Redirect(w, r, "/"+rc.Full+"/threads/"+url.PathEscape(runs[0].ID)+"?notice="+url.QueryEscape("workflow dispatched"), http.StatusSeeOther)
		return
	}
	status := collabErrStatus(err)
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
		s.collabError(w, r, status, err.Error())
		return
	}
	// A fixable form error: re-render the page with the values and the reason.
	data := threadsListData{
		Repo: rc, RefreshHref: "/" + rc.Full + "/threads",
		CanWrite: true, DispatchError: err.Error(), DispatchRef: ref,
	}
	data.Workflows, data.WorkflowsError = threadWorkflowRows(r.Context(), rc, p)
	data.States = []string{threads.RunQueued, threads.RunRunning, threads.RunPendingApproval, threads.RunSuccess, threads.RunFailure, threads.RunCancelled, threads.RunSkipped}
	data.FilterWorkflow = strings.TrimSpace(r.URL.Query().Get("workflow"))
	data.FilterState = strings.TrimSpace(r.URL.Query().Get("state"))
	data.FilterActor = strings.TrimSpace(r.URL.Query().Get("actor"))
	data.FilterActive = data.FilterWorkflow != "" || data.FilterState != "" || data.FilterActor != ""
	for _, wf := range data.Workflows {
		data.FilterWorkflowOptions = append(data.FilterWorkflowOptions, wf.Name)
	}
	threadDispatchModel(&data, workflow, submitted, threadEnvironmentNames(r.Context(), rc, p, data.Workflows))
	if threadsDeps.List != nil {
		if page, lerr := threadsDeps.List(r.Context(), rc.Owner, rc.Name, p, "", 50, threads.RunFilter{}); lerr == nil {
			data.More = page.More
			for _, run := range page.Runs {
				data.Rows = append(data.Rows, threadRunRow{
					ID: run.ID, Short: shortRunID(run.ID), Href: "/" + rc.Full + "/threads/" + run.ID,
					Commit: run.Commit, ShortCommit: shortSHA(run.Commit),
					Event: run.Event, Workflow: run.Workflow, Branch: run.Branch,
					State: run.State, Trusted: run.Trusted, UpdatedAt: run.UpdatedAt,
					DurationSeconds: run.DurationSeconds,
				})
			}
		}
	}
	s.pageRepo(w, r, p, s.collabChrome(r, rc, p), status, "collab/threads", "Threads", data)
}

// threadsSettings answers POST …/threads/settings: the per-repository Threads
// switches. The service enforces maintain+ and audits every changed switch, so
// the form is an affordance, never the gate. A refusal is a plain error page
// that names the rule (e.g. the head-checkout switch needs pull_request_target).
func (s *Server) threadsSettings(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		s.collabError(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	if threadsDeps.UpdateSettings == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	cap := 0
	if raw := strings.TrimSpace(r.PostForm.Get("concurrency_cap")); raw != "" {
		parsed, cerr := strconv.Atoi(raw)
		if cerr != nil || parsed < 0 || parsed > 500 {
			s.collabError(w, r, http.StatusBadRequest, "concurrency_cap must be an integer 0..500")
			return
		}
		cap = parsed
	}
	in := threads.SettingsInput{
		SendWriteTokens:           r.PostForm.Get("send_write_tokens") == "on",
		PullRequestTarget:         r.PostForm.Get("pull_request_target") == "on",
		PRTargetAllowHeadCheckout: r.PostForm.Get("pr_target_allow_head_checkout") == "on",
		ConcurrencyCap:            cap,
	}
	if _, err := threadsDeps.UpdateSettings(r.Context(), rc.Owner, rc.Name, in, p); err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape("threads settings saved"), http.StatusSeeOther)
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
	// WorkflowError is the parse error the run's workflow file currently
	// carries (the file may have broken after the run was created); the page
	// links to the per-file error on the Threads page.
	WorkflowError     string
	WorkflowErrorPath string
	WorkflowErrorHref string
	// CanWrite enables the operator verbs (cancel/rerun); the service enforces
	// the role regardless of what the page renders.
	CanWrite bool
	// CanApprove enables the first-time-contributor gate's approve/deny
	// controls (maintain+); the service enforces the role.
	CanApprove bool
	// Approval is the run's gate state (pending/approved/denied), rendered so a
	// held run says why it is held.
	Approval threads.ApprovalView
	// Notice is the ?notice= text a verb redirect carries (cancel/rerun/
	// dispatch feedback).
	Notice string
	// JobLogs is one live-log panel per job (the island attaches by URL over
	// the existing SSE transport — no new transport, 15 §6).
	JobLogs []threadJobLog
	// Artifacts are the files this run published (15 §2.5): name, size, age and
	// a ZIP download link. They render under the jobs.
	Artifacts []threadArtifact
	// Error is the page-level error text (empty on a clean render); the empty
	// section uses it to avoid claiming "no jobs" for a failed read.
	Error string
}

// threadJobLog is one job's live-log panel state.
type threadJobLog struct {
	JobID  string
	State  string
	LogURL string
	// Live is true while the job can still produce frames.
	Live bool
}

// threadArtifact is one published artifact row on the run page.
type threadArtifact struct {
	Name  string
	Size  string
	Files int
	Href  string
	// CreatedAt/ExpiresAt are the run's own timestamps (rendered with the shared
	// collab_time template, never re-formatted here).
	CreatedAt string
	ExpiresAt string
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
		Notice:      r.URL.Query().Get("notice"),
	}
	data.WorkflowError, data.WorkflowErrorPath = threadWorkflowError(r.Context(), rc, p, view.Run)
	if data.WorkflowError != "" {
		data.WorkflowErrorHref = "/" + rc.Full + "/threads#" + workflowAnchor(data.WorkflowErrorPath)
	}
	data.CanWrite = threadCanWrite(r.Context(), rc, p)
	data.CanApprove = data.CanWrite && threadCanMaintain(r.Context(), rc, p)
	data.Approval = threads.Approval(view.Run)
	if view.Approval != nil {
		// The service's principal-aware projection: a listed reviewer with only
		// write access sees the environment controls too (the verb re-checks).
		data.Approval = *view.Approval
		data.CanApprove = data.CanWrite && view.Approval.CanDecide
	}
	data.CanApprove = data.CanApprove || data.Approval.CanDecide
	for _, job := range view.Jobs {
		data.JobLogs = append(data.JobLogs, threadJobLog{
			JobID:  job.ID,
			State:  job.State,
			LogURL: "/" + rc.Full + "/api/threads/" + url.PathEscape(view.Run.ID) + "/jobs/" + url.PathEscape(job.ID) + "/log",
			Live:   job.State == threads.JobQueued || job.State == threads.JobRunning,
		})
	}
	for _, art := range view.Artifacts {
		data.Artifacts = append(data.Artifacts, threadArtifact{
			Name: art.Name, Size: fmtAssetSize(art.TotalSize), Files: len(art.Files),
			Href: "/" + rc.Full + "/threads/" + url.PathEscape(view.Run.ID) +
				"/artifacts/" + url.PathEscape(art.Name),
			CreatedAt: art.CreatedAt, ExpiresAt: art.ExpiresAt,
		})
	}
	s.pageRepo(w, r, p, s.collabChrome(r, rc, p), http.StatusOK, "collab/thread_detail", "Run "+data.ShortRun, data)
}

// threadArtifactDownload streams one artifact as a ZIP (15 §2.5). The service
// is the gate: it resolves the caller's repository read role and the
// artifact's expiry, and returns the opened manifest; the page only sets the
// download headers and copies the archive.
func (s *Server) threadArtifactDownload(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	name := r.PathValue("artifact")
	if threadsDeps.OpenArtifact == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	dl, err := threadsDeps.OpenArtifact(r.Context(), rc.Owner, rc.Name, r.PathValue("run"), name, p)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/zip")
	hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := threads.WriteArtifactZip(r.Context(), w, dl); err != nil {
		// Headers are spent; the failed copy is logged, never rendered into a
		// zip's bytes.
		slog.Default().Warn("threads: artifact download aborted", "artifact", name, "err", err)
	}
}

// threadRunCancel answers POST …/threads/{run}/cancel from the page.
func (s *Server) threadRunCancel(w http.ResponseWriter, r *http.Request) {
	s.threadRunVerb(w, r, threadsDeps.Cancel, "cancelled")
}

// threadRunRerun answers POST …/threads/{run}/rerun from the page.
func (s *Server) threadRunRerun(w http.ResponseWriter, r *http.Request) {
	s.threadRunVerb(w, r, threadsDeps.Rerun, "re-run queued")
}

// threadRunApprove answers POST …/threads/{run}/approve (the gate's release).
func (s *Server) threadRunApprove(w http.ResponseWriter, r *http.Request) {
	s.threadRunVerb(w, r, threadsDeps.Approve, "approved — queued")
}

// threadRunDeny answers POST …/threads/{run}/deny (the gate's refusal).
func (s *Server) threadRunDeny(w http.ResponseWriter, r *http.Request) {
	s.threadRunVerb(w, r, threadsDeps.Deny, "denied — cancelled")
}

// threadRunVerb runs one operator verb and redirects back to the run page. A
// refusal is a plain error page (never a silent no-op): the service's role
// check is the gate, the page's read gate only got the caller here.
func (s *Server) threadRunVerb(w http.ResponseWriter, r *http.Request, verb func(ctx context.Context, owner, repo, runID string, p auth.Principal) error, done string) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		s.collabError(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	runID := r.PathValue("run")
	if verb == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	if err := verb(r.Context(), rc.Owner, rc.Name, runID, p); err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads/"+url.PathEscape(runID)+"?notice="+url.QueryEscape(done), http.StatusSeeOther)
}

// threadCanMaintain reports whether the caller holds maintain+ (the approval
// gate's floor). One role resolution; the service re-checks on the verb.
func threadCanMaintain(ctx context.Context, rc collabRepo, p auth.Principal) bool {
	if p.Admin {
		return true
	}
	if threadsDeps.Role == nil {
		return p.Write
	}
	role, err := threadsDeps.Role(ctx, rc.Owner, rc.Name, p)
	return err == nil && settingsRoleAtLeast(role, "maintain")
}

// threadCanWrite reports whether the caller may use the operator verbs. It is
// an affordance, not the gate: the service re-checks the write role.
func threadCanWrite(ctx context.Context, rc collabRepo, p auth.Principal) bool {
	if p.Admin || p.Write {
		return true
	}
	if threadsDeps.Role == nil {
		return false
	}
	role, err := threadsDeps.Role(ctx, rc.Owner, rc.Name, p)
	return err == nil && settingsRoleAtLeast(role, "write")
}

// threadWorkflowRows lists the discovered workflows at the default branch tip
// for the page. A discovery failure is returned as display text (the run list is
// independent of it); an unwired seam yields no rows.
func threadWorkflowRows(ctx context.Context, rc collabRepo, p auth.Principal) ([]threadWorkflowRow, string) {
	if threadsDeps.Workflows == nil {
		return nil, ""
	}
	list, err := threadsDeps.Workflows(ctx, rc.Owner, rc.Name, "", p)
	if err != nil {
		return nil, err.Error()
	}
	rows := make([]threadWorkflowRow, 0, len(list.Workflows))
	for _, wf := range list.Workflows {
		rows = append(rows, threadWorkflowRow{
			Path: wf.Path, Name: wf.Name, Anchor: workflowAnchor(wf.Path),
			Triggers: wf.Triggers, Error: wf.Error,
			Dispatch: threadDeclaresDispatch(wf.Triggers), Inputs: wf.Inputs,
		})
	}
	return rows, ""
}

// threadDeclaresDispatch reports the manual trigger from the discovery trigger
// list (the API's own projection, so page and API cannot disagree).
func threadDeclaresDispatch(triggers []string) bool {
	for _, t := range triggers {
		if t == "workflow_dispatch" {
			return true
		}
	}
	return false
}

// threadWorkflowError returns the current parse error of the run's workflow
// file (path and text), or ("", "") when it parses (or the seam is unwired).
func threadWorkflowError(ctx context.Context, rc collabRepo, p auth.Principal, run *threads.Run) (msg, path string) {
	if threadsDeps.Workflows == nil || run == nil {
		return "", ""
	}
	list, err := threadsDeps.Workflows(ctx, rc.Owner, rc.Name, "", p)
	if err != nil {
		return "", ""
	}
	for _, wf := range list.Workflows {
		if wf.Path == run.WorkflowPath && wf.Error != "" {
			return wf.Error, wf.Path
		}
	}
	return "", ""
}

// workflowAnchor is the stable fragment for one workflow row (the run page
// links a broken workflow to its entry on the Threads page).
var workflowAnchorInvalid = regexp.MustCompile(`[^a-z0-9]+`)

func workflowAnchor(path string) string {
	slug := strings.Trim(workflowAnchorInvalid.ReplaceAllString(strings.ToLower(path), "-"), "-")
	if slug == "" {
		return "workflow"
	}
	return "workflow-" + slug
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
// (success/pending/in_progress/failure/cancelled/skipped; queued is pending and
// running is in progress, matching the checks mirror's own mapping).
func runDotState(state string) string {
	switch state {
	case threads.RunSuccess:
		return "success"
	case threads.RunFailure:
		return "failure"
	case threads.RunCancelled:
		return "cancelled"
	case threads.RunSkipped:
		return "skipped"
	case threads.RunRunning:
		return "in_progress"
	default:
		return "pending"
	}
}
