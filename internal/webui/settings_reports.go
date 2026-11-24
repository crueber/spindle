// settings_reports.go — the repository Settings → Reports tab (PLAN-V2 §M.3/
// M.4) and its per-row resolve forms.
//
//	GET  /{owner}/{name}/settings/reports                 repo admin
//	POST /{owner}/{name}/settings/reports/{id}/{action}   repo admin
//
// The tab is the repo-side queue: open reports first, then the resolved
// recent tail, each in the canonical list anatomy. Every column is text (the
// state is a label, never colour alone) and the per-row form carries the
// three resolve actions (`hidden`, `locked`, `none`) plus an optional note.
//
// The page is a PROJECTION: it reads through the same moderation verbs the
// JSON API uses (SettingsDeps.RepoReports / RepoReportResolve), so the page
// and its API twin cannot disagree. The page gates the tab (admin, never
// rendered to a lesser role); the verb re-checks the floor.
package webui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
)

// SettingsReportCard is one report row as the settings page sees it (a narrow
// view of moderation.ReportCard; composition adapts).
type SettingsReportCard struct {
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
}

// SettingsRepoReports is the Reports tab payload.
type SettingsRepoReports struct {
	State    string // open | resolved | all
	Open     []SettingsReportRow
	Resolved []SettingsReportRow
	// Reasons is the per-row resolve reason/action set (hidden|locked|none).
	Reasons []SettingsReportCardReason
	// NotWired renders the honest "not wired" card instead of an empty queue
	// (a missing seam is not the same fact as "No reports").
	NotWired bool
	Error    string
}

// SettingsReportRow is one rendered queue row.
type SettingsReportRow struct {
	SettingsReportCard
	Href        string // the reported conversation (or the profile for user reports)
	Action      string // the row form's action
	StateLabel  string
	StateTone   string
	ActionLabel string
}

// settingsReportResolveActions is the per-row resolve action set (the API's
// `action` enum, plan §M.3).
var settingsReportResolveActions = []SettingsReportCardReason{
	{ID: "hidden", Label: "Hide the content"},
	{ID: "locked", Label: "Lock the conversation"},
	{ID: "none", Label: "Dismiss (no content change)"},
}

// SettingsReportCardReason is one reason radio or resolve action choice.
type SettingsReportCardReason struct {
	ID    string
	Label string
}

// SettingsReportsFrom projects the service cards into the page payload (the
// composition adapter calls it; the page never builds rows from raw cards).
func SettingsReportsFrom(owner, name, state string, open, resolved []SettingsReportCard) *SettingsRepoReports {
	v := &SettingsRepoReports{State: state, Reasons: settingsReportResolveActions}
	if v.State == "" {
		v.State = "all"
	}
	for _, c := range open {
		v.Open = append(v.Open, settingsReportRow(owner, name, c))
	}
	for _, c := range resolved {
		v.Resolved = append(v.Resolved, settingsReportRow(owner, name, c))
	}
	return v
}

// ShowOpen / ShowResolved are the filter predicates the template renders.
func (v *SettingsRepoReports) ShowOpen() bool {
	return v == nil || v.State == "all" || v.State == "open"
}

func (v *SettingsRepoReports) ShowResolved() bool {
	return v == nil || v.State == "all" || v.State == "resolved"
}

func settingsReportRow(owner, name string, c SettingsReportCard) SettingsReportRow {
	row := SettingsReportRow{SettingsReportCard: c}
	row.Action = "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) +
		"/settings/reports/" + strconv.Itoa(c.ID) + "/resolve"
	row.Href = settingsReportHref(owner, name, c)
	switch c.State {
	case "resolved":
		row.StateLabel, row.StateTone = "resolved", "chip chip-open"
	case "dismissed":
		row.StateLabel, row.StateTone = "dismissed", "chip chip-neutral"
	default:
		row.StateLabel, row.StateTone = "open", "chip chip-draft"
	}
	if c.Action != "" {
		row.ActionLabel = c.Action
	}
	return row
}

// settingsReportHref is the conversation a report points at; a user report
// points at the profile.
func settingsReportHref(owner, name string, c SettingsReportCard) string {
	if c.Kind == "user" {
		if p, ok := strings.CutPrefix(c.Target, "user:"); ok {
			return "/" + url.PathEscape(p)
		}
		return "/" + url.PathEscape(owner)
	}
	num := 0
	if t, err := parseReportNum(c.Target); err == nil {
		num = t
	}
	if num < 1 {
		return "/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	}
	return "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pull/" + strconv.Itoa(num)
}

// parseReportNum extracts the conversation number from the plan's target
// grammar (thread:<num>:…, review:<num>:…, review_thread:<num>:…). The page
// only formats; validation happens in the service.
func parseReportNum(target string) (int, error) {
	parts := strings.Split(target, ":")
	if len(parts) < 2 {
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.ParseInt(parts[1], 16, 64)
	if err != nil || n < 1 {
		return 0, strconv.ErrSyntax
	}
	return int(n), nil
}

// settingsReportAction answers POST /{owner}/{name}/settings/reports/{id}/{action}.
func (s *Server) settingsReportAction(w http.ResponseWriter, r *http.Request) {
	p, role, ok := s.settingsRepoAccess(w, r)
	if !ok {
		return
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	if !settingsRoleAtLeast(role, "admin") {
		s.errorPage(w, r, http.StatusForbidden, "resolving reports requires the admin role")
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if p.Anonymous {
		s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
		return
	}
	id, perr := strconv.Atoi(r.PathValue("id"))
	if perr != nil || id < 1 {
		s.errorPage(w, r, http.StatusNotFound, "unknown report")
		return
	}
	// The path names the verb (resolve|dismiss); a resolve carries the
	// moderation action (`hidden|locked|none`) in the form body.
	verb := strings.ToLower(strings.TrimSpace(r.PathValue("action")))
	var action string
	switch verb {
	case "dismiss":
		action = "none"
	case "resolve":
		action = strings.ToLower(strings.TrimSpace(r.PostFormValue("action")))
	default:
		s.errorPage(w, r, http.StatusNotFound, "unknown report action")
		return
	}
	switch action {
	case "hidden", "locked", "none":
	default:
		s.errorPage(w, r, http.StatusNotFound, "unknown report action")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	d := settingsDeps()
	if d.RepoReportResolve == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "report moderation is not wired on this build")
		return
	}
	if err := d.RepoReportResolve(r.Context(), owner, name, id, p, action, r.PostFormValue("note")); err != nil {
		page := s.settingsRepoPage(r, p, owner, name, "reports", role, true)
		page.Error = err.Error()
		s.settingsRepoFinish(w, r, p, page, nil)
		return
	}
	redirectPage(w, r, "/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/settings/reports?saved=1")
}

// settingsReportsRead loads the tab (nil seam = honest not-wired card).
func settingsReportsRead(ctx context.Context, d SettingsDeps, p auth.Principal, owner, name, state string) *SettingsRepoReports {
	if d.RepoReports == nil {
		return &SettingsRepoReports{State: state, NotWired: true}
	}
	v, err := d.RepoReports(ctx, owner, name, p, state)
	if err != nil {
		return &SettingsRepoReports{State: state, Error: err.Error()}
	}
	if v == nil {
		return &SettingsRepoReports{State: state, NotWired: true}
	}
	return v
}
