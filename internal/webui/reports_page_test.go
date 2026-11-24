package webui

// reports_page_test.go — M3 surfaces (PLAN-V2 §M.3/M.4): the Settings →
// Reports tab, the host /admin/reports queue and the /admin/audit viewer.
// Each test proves the render AND the gate: a lesser role gets the same 403
// the API gives and the nav entry is not rendered.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func settingsReportsFixture(role string) (*settingsFakes, *[]string) {
	var calls []string
	f := &settingsFakes{deps: testDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) {
			return role, nil
		},
		RepoReports: func(context.Context, string, string, auth.Principal, string) (*SettingsRepoReports, error) {
			return SettingsReportsFrom("acme", "monorepo", "all",
				[]SettingsReportCard{
					{ID: 7, Kind: "thread", Target: "thread:00000c:event:000000000003", Reporter: "bob", Reason: "spam",
						Note: "looks automated", CreatedAt: "2026-11-20T10:00:00Z", State: "open"},
				},
				[]SettingsReportCard{
					{ID: 6, Kind: "review", Target: "review:00000c:000000000001", Reporter: "carol", Reason: "abusive",
						CreatedAt: "2026-11-19T09:00:00Z", State: "resolved", Action: "hidden",
						ResolvedBy: "alice", ResolvedAt: "2026-11-20T11:00:00Z", ResolveNote: "confirmed"},
				}), nil
		},
		RepoReportResolve: func(_ context.Context, owner, repo string, id int, _ auth.Principal, action, note string) error {
			calls = append(calls, owner+"/"+repo+"#"+itoa(id)+":"+action+":"+note)
			return nil
		},
	}}
	return f, &calls
}

func TestSettingsReportsTabRendersCanonicalQueue(t *testing.T) {
	f, _ := settingsReportsFixture("admin")
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/reports", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET reports = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The nav exposes the tab to an admin, and the queue renders both sections
	// in the canonical list anatomy with the per-row resolve form.
	settingsMustContain(t, body, `href="/acme/monorepo/settings/reports"`, "reports nav")
	settingsMustContain(t, body, "Open reports", "open section")
	settingsMustContain(t, body, "#7 thread", "open row")
	settingsMustContain(t, body, "reason:", "reason column")
	settingsMustContain(t, body, "looks automated", "report note")
	settingsMustContain(t, body, `action="/acme/monorepo/settings/reports/7/resolve"`, "resolve action")
	settingsMustContain(t, body, `name="action" value="hidden"`, "hidden radio")
	settingsMustContain(t, body, `name="action" value="locked"`, "locked radio")
	settingsMustContain(t, body, `name="action" value="none" checked`, "none radio")
	settingsMustContain(t, body, "Resolve report #7", "submit label")
	settingsMustContain(t, body, "Resolved", "resolved section")
	settingsMustContain(t, body, "#6 review", "resolved row")
	settingsMustContain(t, body, "action: hidden", "terminal action chip")
	settingsMustContain(t, body, "confirmed", "decision note")
	// No page-level primary beyond the filter idiom: the resolve buttons are
	// the only .primary on the tab (and the filter's active state).
	if strings.Count(body, "btn primary") > 3 {
		t.Fatalf("too many primaries: %d", strings.Count(body, "btn primary"))
	}
}

func TestSettingsReportsTabGate(t *testing.T) {
	f, _ := settingsReportsFixture("read")
	s := newSettingsServer(t, f)
	// A reader is 403 on the tab (the settings page gate), and the nav entry
	// is not rendered on a tab they may see.
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/reports", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader tab = %d, want 403", rec.Code)
	}
	// A tab the reader may see (WAL is read-gated) still hides the admin-only
	// Reports entry.
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/wal", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reader wal = %d", rec.Code)
	}
	settingsMustNotContain(t, rec.Body.String(), `href="/acme/monorepo/settings/reports"`, "reports nav for a reader")
}

func TestSettingsReportsResolveAction(t *testing.T) {
	f, calls := settingsReportsFixture("admin")
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/reports/7/resolve",
		url.Values{"action": {"hidden"}, "note": {"confirmed"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/acme/monorepo/settings/reports?saved=1" {
		t.Fatalf("Location = %q", got)
	}
	if len(*calls) != 1 || (*calls)[0] != "acme/monorepo#7:hidden:confirmed" {
		t.Fatalf("seam calls = %v", *calls)
	}
	// The dismiss spelling maps to action none.
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/reports/7/dismiss",
		url.Values{"note": {"not abuse"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("dismiss = %d", rec.Code)
	}
	if len(*calls) != 2 || (*calls)[1] != "acme/monorepo#7:none:not abuse" {
		t.Fatalf("seam calls = %v", *calls)
	}
	// Unknown action and cross-origin are refused before the seam.
	if rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/reports/7/delete",
		url.Values{"action": {"hidden"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad action = %d", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/reports/7/resolve",
		url.Values{"action": {"hidden"}}, map[string]string{"Origin": "http://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	// A reader cannot post the resolve (admin floor).
	f2, calls2 := settingsReportsFixture("read")
	s2 := newSettingsServer(t, f2)
	if rec := settingsDo(t, s2, http.MethodPost, "/acme/monorepo/settings/reports/7/resolve",
		url.Values{"action": {"hidden"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("reader resolve = %d", rec.Code)
	}
	if len(*calls2) != 0 {
		t.Fatalf("reader reached the seam: %v", *calls2)
	}
}

// --- /admin/reports and /admin/audit ---------------------------------------

func adminModerationFixture(deny bool) (*settingsFakes, *[]string) {
	var calls []string
	deps := testDeps{}
	if deny {
		deps.Access = func(*http.Request, auth.Principal) *auth.AuthError {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin group required"}
		}
	}
	deps.Reports = func(_ context.Context, _ auth.Principal, state, after string, n int) (*AdminReportsView, error) {
		return AdminReportsFrom([]AdminReportCard{
			{Repo: "acme/widgets", ID: 3, Kind: "thread", Target: "thread:00000c:event:000000000003",
				Reporter: "bob", Reason: "spam", CreatedAt: "2026-11-20T10:00:00Z", State: "open",
				Cursor: "2026-11-20T10:00:00Z|acme/widgets|000000000003"},
		}, state, false), nil
	}
	deps.Audit = func(_ context.Context, _ auth.Principal, after, n int, _ AdminAuditFilter) (*AdminAuditView, error) {
		return &AdminAuditView{
			Rows: []AdminAuditRow{{Seq: 4, At: "2026-11-20T12:00:00Z", Actor: "alice", Action: "report_resolve",
				Target: "repo:acme/widgets", Reason: "spam", Context: "report=000000000003;action=hidden"}},
			More: true, After: after, Next: 4,
		}, nil
	}
	deps.ResolveReport = func(_ context.Context, owner, repo string, id int, _ auth.Principal, action, note string) error {
		calls = append(calls, owner+"/"+repo+"#"+itoa(id)+":"+action+":"+note)
		return nil
	}
	return &settingsFakes{deps: deps}, &calls
}

func TestAdminReportsPageRendersAndGates(t *testing.T) {
	f, calls := adminModerationFixture(false)
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/reports?state=open", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/reports = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-admin-section="reports"`, "section")
	settingsMustContain(t, body, `<h1 class="text-xl font-semibold tracking-tight">Moderation reports</h1>`, "one h1")
	settingsMustContain(t, body, `href="/acme/widgets/settings/reports"`, "repo link")
	settingsMustContain(t, body, "#3 thread", "report row")
	settingsMustContain(t, body, `action="/admin/reports/resolve"`, "resolve twin")
	settingsMustContain(t, body, `name="owner" value="acme"`, "owner field")
	settingsMustContain(t, body, `name="repo" value="widgets"`, "repo field")
	settingsMustContain(t, body, `name="id" value="3"`, "id field")
	settingsMustContain(t, body, `name="action" value="locked"`, "resolve radio")
	// The resolve twin posts the form and lands back on the filter.
	rec = settingsDo(t, s, http.MethodPost, "/admin/reports/resolve",
		url.Values{"owner": {"acme"}, "repo": {"widgets"}, "id": {"3"}, "action": {"locked"}, "note": {"cool it"}, "state": {"open"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/reports?state=open&resolved=1" {
		t.Fatalf("Location = %q", got)
	}
	if len(*calls) != 1 || (*calls)[0] != "acme/widgets#3:locked:cool it" {
		t.Fatalf("seam calls = %v", *calls)
	}
}

func TestAdminReportsDenied(t *testing.T) {
	f, calls := adminModerationFixture(true)
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/reports", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied = %d, want 403", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Access restricted", "designed denied view")
	settingsMustNotContain(t, rec.Body.String(), "report_resolve", "queue when denied")
	if rec := settingsDo(t, s, http.MethodPost, "/admin/reports/resolve",
		url.Values{"owner": {"acme"}, "repo": {"widgets"}, "id": {"3"}, "action": {"none"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("denied resolve = %d", rec.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("denied resolve reached the seam: %v", *calls)
	}
}

func TestAdminAuditPageRendersPages(t *testing.T) {
	f, _ := adminModerationFixture(false)
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/audit", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/audit = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `data-admin-section="audit"`, "section")
	settingsMustContain(t, body, "Audit log", "title")
	settingsMustContain(t, body, `class="overflow-x-auto"`, "table wrapper")
	settingsMustContain(t, body, "#4", "seq cell")
	settingsMustContain(t, body, "report_resolve", "action cell")
	settingsMustContain(t, body, "report=000000000003;action=hidden", "context cell")
	settingsMustContain(t, body, `href="/admin/audit?after=4"`, "next page")

	// Denied viewers get the designed 403, not the log.
	f2, _ := adminModerationFixture(true)
	s2 := newSettingsServer(t, f2)
	rec = settingsDo(t, s2, http.MethodGet, "/admin/audit", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied audit = %d", rec.Code)
	}
	settingsMustNotContain(t, rec.Body.String(), "report_resolve", "audit rows when denied")
}

func TestSettingsReportsNotWiredIsHonest(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return "admin", nil },
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/reports", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("not-wired tab = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "Report moderation is not wired on this build yet.", "honest not-wired card")
}

func TestSettingsReportsResolveErrorRerenders(t *testing.T) {
	f, _ := settingsReportsFixture("admin")
	f.deps.RepoReportResolve = func(context.Context, string, string, int, auth.Principal, string, string) error {
		return &settingsTestErr{"report already resolved"}
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/reports/7/resolve",
		url.Values{"action": {"none"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("error write-back = %d\n%s", rec.Code, rec.Body.String())
	}
	settingsMustContain(t, rec.Body.String(), "report already resolved", "inline error")
	// A nil seam is a 503 with the honest not-wired line.
	f2 := &settingsFakes{deps: testDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return "admin", nil },
	}}
	s2 := newSettingsServer(t, f2)
	if rec := settingsDo(t, s2, http.MethodPost, "/acme/monorepo/settings/reports/7/resolve",
		url.Values{"action": {"none"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil seam = %d", rec.Code)
	}
}

func TestSettingsReportsReadErrorRenders(t *testing.T) {
	f := &settingsFakes{deps: testDeps{
		RepoRole: func(context.Context, string, string, auth.Principal) (string, error) { return "admin", nil },
		RepoReports: func(context.Context, string, string, auth.Principal, string) (*SettingsRepoReports, error) {
			return nil, &settingsTestErr{"queue read failed"}
		},
	}}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/reports", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read error page = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "queue read failed", "inline read error")
	// Unknown state defaults to all (both sections render).
	if got := settingsReportsViewState(rec.Body.String()); got != "all" {
		t.Fatalf("state = %q", got)
	}
}

// settingsReportsViewState reads the active filter from the rendered links.
func settingsReportsViewState(body string) string {
	for _, st := range []string{"open", "resolved", "all"} {
		if strings.Contains(body, `class="btn primary" href="?state=`+st) {
			return st
		}
	}
	return ""
}

func TestAdminModerationEdgeBranches(t *testing.T) {
	f, calls := adminModerationFixture(false)
	// Report rows cover both state chips, the user link and the conversation
	// fallback.
	f.deps.Reports = func(context.Context, auth.Principal, string, string, int) (*AdminReportsView, error) {
		return AdminReportsFrom([]AdminReportCard{
			{Repo: "acme/widgets", ID: 1, Kind: "user", Target: "user:carol", Reporter: "bob",
				Reason: "abusive", CreatedAt: "2026-11-20T10:00:00Z", State: "dismissed", Action: "none",
				ResolvedBy: "alice", ResolvedAt: "2026-11-21T10:00:00Z"},
			{Repo: "acme/widgets", ID: 2, Kind: "review", Target: "garbage", Reporter: "bob",
				Reason: "spam", CreatedAt: "2026-11-20T11:00:00Z", State: "resolved", Action: "locked"},
			{Repo: "bad", ID: 3, Kind: "thread", Target: "thread:000001:event:000000000001", State: "open"},
		}, "all", true), nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/admin/reports?state=all", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edge queue = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `href="/carol"`, "user report link")
	settingsMustContain(t, body, "dismissed", "dismissed chip")
	settingsMustContain(t, body, "resolved", "resolved chip")
	settingsMustContain(t, body, "href=\"/acme/widgets\"", "unparseable target fallback")
	// A page with more rows exposes the cursor link.
	settingsMustContain(t, body, "older reports", "cursor link")

	// Bad resolve submissions fail before the seam.
	for _, form := range []url.Values{
		{"owner": {""}, "repo": {"widgets"}, "id": {"1"}, "action": {"none"}},
		{"owner": {"acme"}, "repo": {"widgets"}, "id": {"x"}, "action": {"none"}},
		{"owner": {"acme"}, "repo": {"widgets"}, "id": {"1"}, "action": {"delete"}},
	} {
		rec := settingsDo(t, s, http.MethodPost, "/admin/reports/resolve", form, nil)
		if rec.Code < 400 {
			t.Fatalf("bad resolve %v = %d", form, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bad submissions reached the seam: %v", *calls)
	}

	// A seam error re-renders the queue at 409 with the message.
	f.deps.ResolveReport = func(context.Context, string, string, int, auth.Principal, string, string) error {
		return &settingsTestErr{"already terminal"}
	}
	s2 := newSettingsServer(t, f)
	rec = settingsDo(t, s2, http.MethodPost, "/admin/reports/resolve",
		url.Values{"owner": {"acme"}, "repo": {"widgets"}, "id": {"1"}, "action": {"none"}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("resolve error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "already terminal", "resolve error text")

	// A nil seam and a read error render the honest cards.
	f3 := &settingsFakes{deps: testDeps{}}
	s3 := newSettingsServer(t, f3)
	rec = settingsDo(t, s3, http.MethodGet, "/admin/reports", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("not wired = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired on this build", "not-wired report card")
	rec = settingsDo(t, s3, http.MethodGet, "/admin/audit", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit not wired = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired on this build", "not-wired audit card")

	f4, _ := adminModerationFixture(false)
	f4.deps.Audit = func(context.Context, auth.Principal, int, int, AdminAuditFilter) (*AdminAuditView, error) {
		return nil, &settingsTestErr{"audit unavailable"}
	}
	s4 := newSettingsServer(t, f4)
	rec = settingsDo(t, s4, http.MethodGet, "/admin/audit", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "audit unavailable", "audit error text")
}
