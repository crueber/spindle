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
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
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
	// Foreign is the migration banner's read (PAR-003): the files under
	// `.forgejo/workflows`/`.github/workflows` at the default branch tip, shown
	// with the exact `spindle threads import` command. It is display only —
	// nothing is parsed, imported or run — and nil leaves the hint off.
	Foreign func(ctx context.Context, owner, repo, commit string, p auth.Principal) (*threads.ForeignWorkflows, error)
	// Cancel/Rerun are the operator verbs (15 §3). The page handler gates on
	// the repository read role to reach the tab; the service re-checks the
	// write role, so a reader's POST is a real 403 (the API's own gate).
	Cancel func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	Rerun  func(ctx context.Context, owner, repo, runID string, p auth.Principal) error
	// CancelJob is the per-job cancel (PAR-195): force=false signals a running
	// attempt, force=true ends it at once. The service re-checks the write role.
	CancelJob func(ctx context.Context, owner, repo, runID, jobID string, force bool, p auth.Principal) error
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
	// RunLogs assembles EVERY job's current-attempt log for the whole-run
	// download (PAR-190). The service resolves the repository read gate.
	RunLogs func(ctx context.Context, owner, repo, runID string, p auth.Principal) (*threads.RunLogBundle, error)
	// Environments lists the repository's environment names for the
	// `environment`-typed dispatch picker (PAR-023). Nil leaves the picker
	// empty; the service still validates the submitted value.
	Environments func(ctx context.Context, owner, repo string, p auth.Principal) ([]string, error)
	// Settings/UpdateSettings are the per-repository Threads switches (PAR-015,
	// PAR-118, PAR-153). The service enforces maintain+ and audits every change;
	// the page renders the values and the form.
	Settings       func(ctx context.Context, owner, repo string, p auth.Principal) (*threads.SettingsView, error)
	UpdateSettings func(ctx context.Context, owner, repo string, in threads.SettingsInput, p auth.Principal) (*threads.SettingsView, error)
	// Wave-4 operations (PAR-154/PAR-155/PAR-157/PAR-196/PAR-197). Each is the
	// same service call the JSON twin uses, so the page cannot disagree about
	// gating or shape.
	ListSchedules   func(ctx context.Context, owner, repo string, p auth.Principal) (*threads.ScheduleList, error)
	SetSchedule     func(ctx context.Context, repo string, id threads.ScheduleEntryID, enabled bool, p auth.Principal) (*threads.ScheduleStatus, error)
	RunScheduleNow  func(ctx context.Context, repo string, id threads.ScheduleEntryID, p auth.Principal) ([]*threads.Run, error)
	SetPaused       func(ctx context.Context, repo string, paused bool, p auth.Principal) (*threads.SettingsView, error)
	Queue           func(ctx context.Context, repo string, p auth.Principal) (*threads.QueueSnapshot, error)
	Subscription    func(ctx context.Context, repo string, p auth.Principal) (*threads.SubscriptionView, error)
	SetSubscription func(ctx context.Context, repo string, subscribed bool, workflows []string, p auth.Principal) (*threads.SubscriptionView, error)
	Notifications   func(ctx context.Context, p auth.Principal, limit int) ([]threads.Notification, error)
	MarkRead        func(ctx context.Context, p auth.Principal) error
	Validate        func(ctx context.Context, repo string, p auth.Principal, file string, src []byte) (*threads.WorkflowValidation, error)
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
	m.HandleFunc("GET /{owner}/{repo}/threads/badge.svg", s.threadBadge)
	m.HandleFunc("POST /{owner}/{repo}/threads/dispatch", s.threadsDispatch)
	m.HandleFunc("POST /{owner}/{repo}/threads/settings", s.threadsSettings)
	m.HandleFunc("POST /{owner}/{repo}/threads/schedule", s.threadsSchedule)
	m.HandleFunc("POST /{owner}/{repo}/threads/pause", s.threadsPause)
	m.HandleFunc("POST /{owner}/{repo}/threads/subscribe", s.threadsSubscribe)
	m.HandleFunc("POST /{owner}/{repo}/threads/notifications/read", s.threadsNotificationsRead)
	m.HandleFunc("POST /{owner}/{repo}/threads/validate", s.threadsValidate)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}", s.threadRun)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}/artifacts/{artifact}", s.threadArtifactDownload)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}/logs.zip", s.threadRunLogsZip)
	m.HandleFunc("GET /{owner}/{repo}/threads/{run}/logs.txt", s.threadRunLogsText)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/cancel", s.threadRunCancel)
	m.HandleFunc("POST /{owner}/{repo}/threads/{run}/jobs/{job}/cancel", s.threadJobCancel)
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
	// Title is the rendered `run-name:` (PAR-057); empty when none.
	Title     string
	Branch    string
	State     string
	Trusted   bool
	UpdatedAt string
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
	// Migration is the migration banner (PAR-003): set only when the repository
	// has foreign workflow files and no `.spindle/workflows`, so the reader is
	// told to run `spindle threads import`. It is a hint — no run is created.
	Migration *threadMigration

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

	// Run-list filters (round-2 pass 2; wave 4 adds branch and event):
	// workflow · state · actor · branch · event. The form is a plain GET, so
	// the filter lives in the URL and a link keeps it.
	FilterWorkflow string
	FilterState    string
	FilterActor    string
	FilterBranch   string
	FilterEvent    string
	States         []string
	Events         []string
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
	// Wave-4 operations (PAR-154/PAR-155/PAR-157/PAR-196/PAR-197).
	Schedules        *threads.ScheduleList
	SchedulesError   string
	Queue            *threads.QueueSnapshot
	QueueError       string
	Subscription     *threads.SubscriptionView
	Notifications    []threads.Notification
	NotificationsErr string
	ValidateResult   *threads.WorkflowValidation
	ValidateFile     string
	ValidateSource   string
	ScheduleError    string
	ScheduleNotice   string

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

// threadMigration is the migration banner's page model (PAR-003).
type threadMigration struct {
	Groups []threadMigrationGroup
}

// threadMigrationGroup is one foreign directory: its platform, its directory,
// the files the page lists and the exact importer command.
type threadMigrationGroup struct {
	Platform string
	Dir      string
	Files    []string
	Command  string
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
	data.Migration = threadMigrationHint(r.Context(), rc, p, data.Workflows)
	data.States = threadRunStates()
	data.Events = threadRunEvents()
	threadApplyFilter(&data, r)
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
	// Wave-4 read models. A schedule/queue read is repository-scoped (the
	// service re-checks); notifications are the signed-in user's own inbox.
	if threadsDeps.ListSchedules != nil {
		if list, lerr := threadsDeps.ListSchedules(r.Context(), rc.Owner, rc.Name, p); lerr == nil {
			data.Schedules = list
		} else {
			data.SchedulesError = lerr.Error()
		}
	}
	if threadsDeps.Queue != nil {
		if q, qerr := threadsDeps.Queue(r.Context(), rc.Full, p); qerr == nil {
			data.Queue = q
		} else {
			data.QueueError = qerr.Error()
		}
	}
	if threadsDeps.Subscription != nil && !p.Anonymous {
		if sub, serr := threadsDeps.Subscription(r.Context(), rc.Full, p); serr == nil {
			data.Subscription = sub
		}
	}
	if threadsDeps.Notifications != nil && !p.Anonymous {
		if notes, nerr := threadsDeps.Notifications(r.Context(), p, 10); nerr == nil {
			data.Notifications = notes
		} else {
			data.NotificationsErr = nerr.Error()
		}
	}
	data.ScheduleError = r.URL.Query().Get("schedule_error")
	data.ScheduleNotice = r.URL.Query().Get("schedule_notice")
	if v := r.URL.Query().Get("validate"); v != "" {
		// A validation result survives the redirect as a compact JSON blob
		// (the editor's answer is small and display-only; the source text is
		// not round-tripped to avoid putting a body in a URL).
		var res threads.WorkflowValidation
		if json.Unmarshal([]byte(v), &res) == nil {
			data.ValidateResult = &res
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
		Branch: data.FilterBranch, Event: data.FilterEvent,
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
			Event: run.Event, Workflow: run.Workflow, Title: run.Title, Branch: run.Branch,
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

// threadApplyFilter fills the run-list filter model from the query (one place,
// so the list and its error re-render cannot disagree).
func threadApplyFilter(data *threadsListData, r *http.Request) {
	q := r.URL.Query()
	data.FilterWorkflow = strings.TrimSpace(q.Get("workflow"))
	data.FilterState = strings.TrimSpace(q.Get("state"))
	data.FilterActor = strings.TrimSpace(q.Get("actor"))
	data.FilterBranch = strings.TrimSpace(q.Get("branch"))
	data.FilterEvent = strings.TrimSpace(q.Get("event"))
	data.FilterActive = data.FilterWorkflow != "" || data.FilterState != "" || data.FilterActor != "" ||
		data.FilterBranch != "" || data.FilterEvent != ""
}

// threadRunStates is the state filter's option list.
func threadRunStates() []string {
	return []string{threads.RunQueued, threads.RunRunning, threads.RunPendingApproval, threads.RunSuccess, threads.RunFailure, threads.RunCancelled, threads.RunSkipped}
}

// threadRunEvents is the event filter's option list (the triggers this engine
// emits; a filter never offers an event that cannot occur).
func threadRunEvents() []string {
	return []string{"push", "pull_request", "pull_request_target", "workflow_dispatch", "workflow_run", "schedule", "repository_dispatch", "release", "create", "delete", "deployment", "deployment_status", "issues", "issue_comment"}
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
	data.States = threadRunStates()
	data.Events = threadRunEvents()
	threadApplyFilter(&data, r)
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
					Event: run.Event, Workflow: run.Workflow, Title: run.Title, Branch: run.Branch,
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
		CIPaused:                  r.PostForm.Get("ci_paused") == "1",
		NotifyWebhook:             strings.TrimSpace(r.PostForm.Get("notify_webhook")),
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
	// Graph is the run's job dependency DAG (PAR-191): one node per workflow
	// job key, matrix cells grouped under their job, each node linked to its
	// job card. It is the service's own read model, never a second fold.
	Graph *threadRunGraph
	// LogsZipHref/LogsTxtHref download EVERY job's log (PAR-190); the service
	// resolves the read gate and the archive is built from the same frames the
	// panels tail.
	LogsZipHref string
	LogsTxtHref string
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

// threadRunGraph is the page's DAG model: nodes grouped by dependency level so
// the template can lay them out left to right (and stack them at ~390 px).
type threadRunGraph struct {
	Levels []threadRunGraphLevel
	Nodes  int
	Edges  int
}

// threadRunGraphLevel is one dependency depth.
type threadRunGraphLevel struct {
	Level int
	Nodes []threadRunGraphNode
}

// threadRunGraphNode is one workflow job and its cells.
type threadRunGraphNode struct {
	Key       string
	Name      string
	State     string
	Href      string
	Blocked   bool
	BlockedBy []string
	Reason    string
	Needs     []threadRunGraphNeed
	Cells     []threadRunGraphCell
	// Multi is true for a matrix job (or a reusable call): the cells render in
	// one disclosed group under the job, so "one row per cell" is still visible
	// without turning the graph into N nodes.
	Multi bool
}

// threadRunGraphNeed is one edge as the node sees it (the dependency's state
// says whether the edge satisfied the wait).
type threadRunGraphNeed struct {
	Key   string
	State string
	OK    bool
}

// threadRunGraphCell is one matrix cell (or a plain job's single row).
type threadRunGraphCell struct {
	JobID           string
	Name            string
	Context         string
	State           string
	Href            string
	Matrix          string
	DurationSeconds int64
	QueueReason     string
	Reason          string
	Tolerated       bool
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
	data.Graph = threadRunGraphModel(view)
	data.LogsZipHref = "/" + rc.Full + "/threads/" + url.PathEscape(view.Run.ID) + "/logs.zip"
	data.LogsTxtHref = "/" + rc.Full + "/threads/" + url.PathEscape(view.Run.ID) + "/logs.txt"
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

// threadJobCancel answers POST …/threads/{run}/jobs/{job}/cancel (PAR-195).
// The optional `force=on` form value forces the job terminal at once.
func (s *Server) threadJobCancel(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		s.collabError(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if threadsDeps.CancelJob == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	runID, jobID := r.PathValue("run"), r.PathValue("job")
	force := r.PostForm.Get("force") == "on"
	notice := "job cancel requested"
	if force {
		notice = "job force-cancelled"
	}
	if err := threadsDeps.CancelJob(r.Context(), rc.Owner, rc.Name, runID, jobID, force, p); err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads/"+url.PathEscape(runID)+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
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

// threadMigrationHint builds the migration banner (PAR-003): a repository with
// foreign workflow files and no `.spindle/workflows` gets the files and the
// exact importer command. It is display-only — the hint lists a directory, and
// nothing here creates a run. Any failure (or an unwired seam) leaves the hint
// off rather than breaking the page.
func threadMigrationHint(ctx context.Context, rc collabRepo, p auth.Principal, workflows []threadWorkflowRow) *threadMigration {
	if threadsDeps.Foreign == nil || len(workflows) > 0 {
		return nil
	}
	found, err := threadsDeps.Foreign(ctx, rc.Owner, rc.Name, "", p)
	if err != nil || found.Empty() {
		return nil
	}
	out := &threadMigration{}
	add := func(platform, dir string, files []string) {
		if len(files) == 0 {
			return
		}
		out.Groups = append(out.Groups, threadMigrationGroup{
			Platform: platform, Dir: dir, Files: files, Command: threads.ImportCommand(platform),
		})
	}
	add("forgejo", ".forgejo/workflows", found.Forgejo)
	add("github", ".github/workflows", found.GitHub)
	if len(out.Groups) == 0 {
		return nil
	}
	return out
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

// threadRunGraphModel projects the service's run graph onto the page (PAR-191).
// The DAG is grouped by dependency level rather than drawn as an SVG: nodes are
// links, edges are named on the node ("after build · success"), so the graph is
// keyboard reachable and survives 390 px without a second renderer.
func threadRunGraphModel(view *threads.RunView) *threadRunGraph {
	if view == nil || view.Run == nil {
		return nil
	}
	g := view.Graph
	if g == nil {
		g = threads.BuildRunGraph(view.Jobs)
	}
	if g == nil || len(g.Nodes) == 0 {
		return nil
	}
	states := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		states[n.Key] = n.State
	}
	out := &threadRunGraph{Nodes: len(g.Nodes), Edges: len(g.Edges)}
	byLevel := map[int]*threadRunGraphLevel{}
	levels := []int{}
	for _, n := range g.Nodes {
		node := threadRunGraphNode{
			Key: n.Key, Name: n.Name, State: n.State, Blocked: n.Blocked,
			BlockedBy: n.BlockedBy, Reason: n.Reason,
		}
		for _, need := range n.Needs {
			state, known := states[need]
			if !known {
				state = "unknown"
			}
			node.Needs = append(node.Needs, threadRunGraphNeed{Key: need, State: state, OK: state == "success"})
		}
		for i, c := range n.Cells {
			cell := threadRunGraphCell{
				JobID: c.JobID, Name: c.Name, Context: c.Context, State: c.State,
				Href: "#job-" + c.JobID, Matrix: threadMatrixLabel(c.Matrix),
				DurationSeconds: c.DurationSeconds, QueueReason: c.QueueReason,
				Reason: c.Reason, Tolerated: c.Tolerated,
			}
			if i == 0 {
				node.Href = cell.Href
			}
			node.Cells = append(node.Cells, cell)
		}
		if node.Href == "" {
			node.Href = "#run-jobs"
		}
		node.Multi = len(node.Cells) > 1
		lvl := byLevel[n.Level]
		if lvl == nil {
			lvl = &threadRunGraphLevel{Level: n.Level}
			byLevel[n.Level] = lvl
			levels = append(levels, n.Level)
		}
		lvl.Nodes = append(lvl.Nodes, node)
	}
	sortInts(levels)
	for _, l := range levels {
		out.Levels = append(out.Levels, *byLevel[l])
	}
	return out
}

// threadMatrixLabel renders one cell's matrix values ("go=1.22 · os=linux"),
// sorted by axis so two renders of the same cell agree.
func threadMatrixLabel(matrix map[string]string) string {
	if len(matrix) == 0 {
		return ""
	}
	keys := make([]string, 0, len(matrix))
	for k := range matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+matrix[k])
	}
	return strings.Join(parts, " · ")
}

// sortInts sorts a small int slice ascending (levels are discovered in node
// order, and the page renders them left to right).
func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// threadRunLogsZip / threadRunLogsText are the run page's whole-run downloads
// (PAR-190). The service resolves the read gate and assembles every job's log;
// the page only names the file.
func (s *Server) threadRunLogsZip(w http.ResponseWriter, r *http.Request) {
	s.threadRunLogs(w, r, true)
}

func (s *Server) threadRunLogsText(w http.ResponseWriter, r *http.Request) {
	s.threadRunLogs(w, r, false)
}

func (s *Server) threadRunLogs(w http.ResponseWriter, r *http.Request, asZip bool) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if threadsDeps.RunLogs == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	runID := r.PathValue("run")
	bundle, err := threadsDeps.RunLogs(r.Context(), rc.Owner, rc.Name, runID, p)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	name := runID + "-logs"
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if asZip {
		hdr.Set("Content-Type", "application/zip")
		hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
		w.WriteHeader(http.StatusOK)
		if werr := threads.WriteRunLogsZip(w, bundle); werr != nil {
			slog.Default().Warn("threads: run log zip aborted", "run", runID, "err", werr)
		}
		return
	}
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".txt"}))
	w.WriteHeader(http.StatusOK)
	if werr := threads.WriteRunLogsText(w, bundle); werr != nil {
		slog.Default().Warn("threads: run log download aborted", "run", runID, "err", werr)
	}
}
