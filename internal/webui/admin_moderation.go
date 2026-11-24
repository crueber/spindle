// admin_moderation.go — the host-administration moderation queue and audit
// viewer (PLAN-V2 §M.3/M.4):
//
//	GET  /admin/reports                                   host admin
//	POST /admin/reports/{owner}/{repo}/{id}/{action}      host admin
//	GET  /admin/audit?after=&n=                           host admin
//
// Both pages ride the Administration area's one gate (AdminDeps.Access via
// requireAdminPage), so the page and /api/v1/moderation/* fail together. The
// reads are the same moderation verbs the JSON twin calls (AdminDeps.Reports /
// AdminDeps.Audit); the resolve form delegates to the repo twin's one writer.
//
// Mobile: rows wrap, the resolve form stacks, and the audit table is wrapped
// in the standard overflow-x-auto container (no page-level pan).
package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/cachepolicy"
)

// AdminReportRow is one global-queue row: the (repo,id) pointer the plan pins,
// projected with its links and resolve action.
type AdminReportRow struct {
	Repo        string
	Owner       string
	Name        string
	ID          int
	Kind        string
	Target      string
	Reporter    string
	Reason      string
	Note        string
	CreatedAt   string
	State       string
	ResolvedBy  string
	ResolvedAt  string
	Action      string
	ResolveNote string
	Cursor      string
	Href        string
	Settings    string
	ResolveURL  string
	StateLabel  string
	StateTone   string
}

// AdminReportsView is /admin/reports.
type AdminReportsView struct {
	State   string
	Rows    []AdminReportRow
	More    bool
	Next    string // cursor of the next page (last row's cursor)
	Reasons []SettingsReportCardReason
	// NotWired renders the honest degraded card (no seam) instead of "none".
	NotWired bool
	Error    string
}

// AdminAuditRow is one audit record as the viewer renders it.
type AdminAuditRow struct {
	Seq     int
	At      string
	Actor   string
	Action  string
	Target  string
	Reason  string
	Context string
}

// AdminAuditFilter is the audit viewer's server-side filter (?action=&actor=&repo=&since=).
type AdminAuditFilter struct {
	Action string // exact verb
	Actor  string // exact principal
	Repo   string // "owner/name" or "-" (instance-level: every non-repo target)
	Since  string // the spelling the operator typed (dates or a relative window)
	// SinceTime is Since resolved to an instant (zero when absent/unparsed);
	// the page handler owns the spelling so the API twin and the page cannot
	// disagree about what "7d" means.
	SinceTime time.Time
	// SinceLabel is the human reading ("since 2026-11-15 (7d)").
	SinceLabel string
}

// Active reports whether any filter is set (the page then says "filtered").
func (f AdminAuditFilter) Active() bool {
	return f.Action != "" || f.Actor != "" || f.Repo != "" || f.Since != ""
}

// AuditActions is the viewer's verb suggestion list (the datalist). It is the
// administrative vocabulary the audit family actually writes; a filter outside
// it still works (an exact match on whatever the record says).
var AuditActions = []string{
	"block", "unblock", "hide", "unhide", "lock", "unlock",
	"report_resolve", "report_dismiss", "config_save",
	"access_grant", "org_member_grant", "org_member_revoke",
	"team_member_grant", "team_member_revoke", "transfer", "packages_gc",
}

// AdminAuditView is /admin/audit (seq-cursor pages).
type AdminAuditView struct {
	Rows     []AdminAuditRow
	More     bool
	After    int
	Next     int
	NotWired bool
	Error    string

	// Filters: the applied values, the named inline errors (bad input never
	// 500s and never silently drops the operator's intent), and the suggestion
	// list for the verb field.
	Filter     AdminAuditFilter
	FilterErrs []string
	Actions    []string
	// NextQuery is the ?after=&<filters> suffix the "older records" link uses,
	// so paging survives filtering; ResetURL drops every filter.
	NextQuery string
	// Scanned is how many records this page examined (a filtered page can be
	// shorter than the page size without being the end of the log).
	Scanned int
}

// AdminReportCard is one wire card the queue builder projects (the same
// fields the JSON twin carries, plus its page cursor).
type AdminReportCard struct {
	Repo        string
	ID          int
	Kind        string
	Target      string
	Reporter    string
	Reason      string
	Note        string
	CreatedAt   string
	State       string
	ResolvedBy  string
	ResolvedAt  string
	Action      string
	ResolveNote string
	Cursor      string
}

// AdminReportsFrom projects wire cards into the /admin/reports view: rows,
// their links, the resolve action and the state chips.
func AdminReportsFrom(cards []AdminReportCard, state string, more bool) *AdminReportsView {
	view := &AdminReportsView{State: state, More: more, Reasons: settingsReportResolveActions}
	for _, c := range cards {
		owner, name, ok := strings.Cut(c.Repo, "/")
		if !ok {
			continue
		}
		row := adminReportRow(owner, name, c.ID, c.Kind, c.Target, c.Reporter, c.Reason, c.Note,
			c.CreatedAt, c.State, c.ResolvedBy, c.ResolvedAt, c.Action, c.ResolveNote, c.Cursor)
		view.Rows = append(view.Rows, row)
	}
	return view
}

// adminReportRow projects one wire row (the composition adapter fills the
// same fields from moderation.GlobalCard).
func adminReportRow(owner, name string, id int, kind, target, reporter, reason, note, createdAt, state, resolvedBy, resolvedAt, action, resolveNote, cursor string) AdminReportRow {
	row := AdminReportRow{
		Repo: owner + "/" + name, Owner: owner, Name: name, ID: id, Kind: kind,
		Target: target, Reporter: reporter, Reason: reason, Note: note, CreatedAt: createdAt,
		State: state, ResolvedBy: resolvedBy, ResolvedAt: resolvedAt, Action: action,
		ResolveNote: resolveNote, Cursor: cursor,
	}
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	row.Settings = base + "/settings/reports"
	row.ResolveURL = "/admin/reports/resolve"
	if kind == "user" {
		if p, ok := strings.CutPrefix(target, "user:"); ok {
			row.Href = "/" + url.PathEscape(p)
		} else {
			row.Href = base
		}
	} else if t, err := parseReportNum(target); err == nil && t > 0 {
		row.Href = base + "/pull/" + strconv.Itoa(t)
	} else {
		row.Href = base
	}
	switch state {
	case "resolved":
		row.StateLabel, row.StateTone = "resolved", "chip chip-open"
	case "dismissed":
		row.StateLabel, row.StateTone = "dismissed", "chip chip-neutral"
	default:
		row.StateLabel, row.StateTone = "open", "chip chip-draft"
	}
	return row
}

// adminReportsPage answers GET /admin/reports.
func (s *Server) adminReportsPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "reports")
	if !ok {
		return
	}
	s.adminShell(p, page, "reports")
	state := r.URL.Query().Get("state")
	if state != "resolved" && state != "all" {
		state = "open"
	}
	view := &AdminReportsView{State: state, Reasons: settingsReportResolveActions}
	d := adminDeps()
	if d.Reports == nil {
		view.NotWired = true
	} else if v, err := d.Reports(r.Context(), p, state, r.URL.Query().Get("after"), 100); err != nil {
		view.Error = err.Error()
	} else if v != nil {
		view = v
	}
	if view.More && len(view.Rows) > 0 {
		view.Next = view.Rows[len(view.Rows)-1].Cursor
	}
	if r.URL.Query().Get("resolved") == "1" {
		page.Notice = "report resolved"
	}
	page.Reports = view
	s.adminRender(w, r, p, http.StatusOK, page)
}

// adminReportResolve answers POST /admin/reports/resolve (owner, repo, id and
// action are form fields: a literal 5-segment admin path would overlap the
// repo family's wildcard POST patterns, and ServeMux refuses ambiguous
// patterns).
func (s *Server) adminReportResolve(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.requireAdminPage(w, r, "reports")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	owner, name := r.PostFormValue("owner"), r.PostFormValue("repo")
	if owner == "" || name == "" {
		s.errorPage(w, r, http.StatusBadRequest, "owner and repo are required")
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.PostFormValue("action")))
	if action == "dismiss" {
		action = "none"
	}
	switch action {
	case "hidden", "locked", "none":
	default:
		s.errorPage(w, r, http.StatusNotFound, "unknown report action")
		return
	}
	id, perr := strconv.Atoi(r.PostFormValue("id"))
	if perr != nil || id < 1 {
		s.errorPage(w, r, http.StatusNotFound, "unknown report")
		return
	}
	d := adminDeps()
	if d.ResolveReport == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "report moderation is not wired on this build")
		return
	}
	if err := d.ResolveReport(r.Context(), owner, name, id, p, action, r.PostFormValue("note")); err != nil {
		page := &AdminPage{}
		s.adminShell(p, page, "reports")
		page.Error = err.Error()
		page.Reports = &AdminReportsView{State: "open", Reasons: settingsReportResolveActions}
		s.adminRender(w, r, p, http.StatusConflict, page)
		return
	}
	// Return to the filter the operator was looking at (never lose the view).
	state := r.PostFormValue("state")
	if state != "resolved" && state != "all" {
		state = "open"
	}
	redirectPage(w, r, "/admin/reports?state="+url.QueryEscape(state)+"&resolved=1")
}

// adminAuditPage answers GET /admin/audit. The page is a read view: filters
// are query params (?action=&actor=&repo=&since=) applied server-side, the
// cursor (?after=) is preserved by every link, and bad filter input is a named
// inline error — never a 500 and never a silently dropped filter.
func (s *Server) adminAuditPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "audit")
	if !ok {
		return
	}
	s.adminShell(p, page, "audit")
	q := r.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	if after < 0 {
		after = 0
	}
	filter, errs := parseAdminAuditFilter(q)
	view := &AdminAuditView{After: after, Filter: filter, FilterErrs: errs, Actions: AuditActions}
	d := adminDeps()
	if d.Audit == nil {
		view.NotWired = true
	} else if v, err := d.Audit(r.Context(), p, after, 100, filter); err != nil {
		view.Error = err.Error()
	} else if v != nil {
		view = v
		view.After = after
		view.Filter = filter
		view.FilterErrs = errs
		if view.Actions == nil {
			view.Actions = AuditActions
		}
	}
	// The cursor is the SCAN position (never past the log), so a filtered page
	// continues from where it stopped looking, not from its last match.
	next := view.Next
	if next < after {
		next = after
	}
	view.Next = next
	view.NextQuery = "/admin/audit?" + auditQueryString(filter, next)
	page.Audit = view
	// The viewer is a read of a private, changing log: never stored.
	w.Header().Set("Cache-Control", cachepolicy.NoStore)
	s.adminRender(w, r, p, http.StatusOK, page)
}

// auditQueryString encodes the filters plus the (optional) resume cursor.
func auditQueryString(f AdminAuditFilter, after int) string {
	q := url.Values{}
	if f.Action != "" {
		q.Set("action", f.Action)
	}
	if f.Actor != "" {
		q.Set("actor", f.Actor)
	}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
	}
	if f.Since != "" {
		q.Set("since", f.Since)
	}
	if after > 0 {
		q.Set("after", strconv.Itoa(after))
	}
	return q.Encode()
}

// parseAdminAuditFilter reads the four filters, returning the named errors for
// bad input. A bad value is reported AND ignored (the rest of the filter still
// applies) so the operator always sees records with the cause in view.
func parseAdminAuditFilter(q url.Values) (AdminAuditFilter, []string) {
	f := AdminAuditFilter{
		Action: strings.ToLower(strings.TrimSpace(q.Get("action"))),
		Actor:  normAuditValue(q.Get("actor")),
		Repo:   normAuditValue(q.Get("repo")),
		Since:  strings.TrimSpace(q.Get("since")),
	}
	var errs []string
	if f.Action != "" && strings.ContainsAny(f.Action, " \t") {
		errs = append(errs, "action: a verb has no spaces — clear the field or pick one from the list")
		f.Action = ""
	}
	if f.Actor != "" && strings.ContainsAny(f.Actor, " \t") {
		errs = append(errs, "actor: a principal has no spaces — use the exact name or email")
		f.Actor = ""
	}
	if f.Repo != "" && f.Repo != "-" && !strings.Contains(f.Repo, "/") {
		errs = append(errs, `repo: use owner/name, or "-" for instance-level records`)
		f.Repo = ""
	}
	if f.Since != "" {
		since, label, serr := parseAuditSince(f.Since, time.Now().UTC())
		if serr != "" {
			errs = append(errs, "since: "+serr)
			f.Since = ""
		} else {
			f.SinceTime, f.SinceLabel = since, label
		}
	}
	return f, errs
}

// parseAuditSince accepts a date (2006-01-02), an RFC 3339 stamp, or a
// relative window (90m, 12h, 7d, 2w) and answers the instant plus the label the
// page shows. A bad value answers a named error, never a zero instant.
func parseAuditSince(v string, now time.Time) (time.Time, string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, "", ""
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), "since " + t.UTC().Format(time.RFC3339), ""
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), "since " + v + " (UTC midnight)", ""
	}
	if d, ok := parseSinceWindow(v); ok {
		at := now.Add(-d).UTC()
		return at, "since " + at.Format(time.RFC3339) + " (" + v + ")", ""
	}
	return time.Time{}, "", "use a date (2026-11-15), an RFC 3339 stamp, or a window like 90m, 12h, 7d, 2w"
}

// parseSinceWindow parses "<n><m|h|d|w>" — the same spellings operators read in
// the rest of the UI. A bare number is refused (no unit, no meaning).
func parseSinceWindow(v string) (time.Duration, bool) {
	if len(v) < 2 {
		return 0, false
	}
	unit := v[len(v)-1]
	num, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || num <= 0 {
		return 0, false
	}
	switch unit {
	case 'm':
		return time.Duration(num) * time.Minute, true
	case 'h':
		return time.Duration(num) * time.Hour, true
	case 'd':
		return time.Duration(num) * 24 * time.Hour, true
	case 'w':
		return time.Duration(num) * 7 * 24 * time.Hour, true
	}
	return 0, false
}

func normAuditValue(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
