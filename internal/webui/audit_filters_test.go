package webui

// audit_filters_test.go — /admin/audit's server-rendered filters (PLAN-ADMIN
// §8.6): each filter alone and combined, cursor paging preserved under a
// filter, bad input as a named inline error (never a 500), the empty states,
// no-store, and the since-window parser. The filter semantics themselves are
// moderation's (audit_test.go); this file proves the page passes them through
// and never loses the operator's intent.

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// auditCapture is an Audit seam that records every filter it is asked for.
type auditCapture struct {
	filters []AdminAuditFilter
	afters  []int
	view    *AdminAuditView
	err     error
}

func auditDeps(c *auditCapture) testDeps {
	deps := adminBaseDeps()
	deps.Audit = func(_ context.Context, _ auth.Principal, after, n int, f AdminAuditFilter) (*AdminAuditView, error) {
		c.filters = append(c.filters, f)
		c.afters = append(c.afters, after)
		if c.err != nil {
			return nil, c.err
		}
		if c.view != nil {
			v := *c.view
			v.After = after
			return &v, nil
		}
		return &AdminAuditView{After: after}, nil
	}
	return deps
}

func TestAuditPageFiltersReachTheSeam(t *testing.T) {
	cases := []struct {
		query string
		want  AdminAuditFilter
	}{
		{"", AdminAuditFilter{}},
		{"?action=config_save", AdminAuditFilter{Action: "config_save"}},
		{"?actor=Alice%40Example.com", AdminAuditFilter{Actor: "alice@example.com"}},
		{"?repo=acme%2Fwidgets", AdminAuditFilter{Repo: "acme/widgets"}},
		{"?repo=-", AdminAuditFilter{Repo: "-"}},
		{"?action=TRANSFER&actor=bob&repo=beta%2Fapp&since=2026-11-15",
			AdminAuditFilter{Action: "transfer", Actor: "bob", Repo: "beta/app", Since: "2026-11-15",
				SinceTime: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}},
	}
	for _, c := range cases {
		c := c
		cap := &auditCapture{}
		s := adminTestPage(t, auditDeps(cap), settingsTestPrincipal())
		rec := settingsDo(t, s, http.MethodGet, "/admin/audit"+c.query, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", c.query, rec.Code)
		}
		if len(cap.filters) != 1 {
			t.Fatalf("%s: seam calls = %d", c.query, len(cap.filters))
		}
		got := cap.filters[0]
		if got.Action != c.want.Action || got.Actor != c.want.Actor || got.Repo != c.want.Repo {
			t.Fatalf("%s: filter = %+v, want %+v", c.query, got, c.want)
		}
		if c.want.Since != "" && !got.SinceTime.Equal(c.want.SinceTime) {
			t.Fatalf("%s: since = %v, want %v", c.query, got.SinceTime, c.want.SinceTime)
		}
		if c.want.Since != "" && got.SinceLabel == "" {
			t.Fatalf("%s: no since label rendered", c.query)
		}
	}
}

// TestAuditPageCursorSurvivesFilters: the "older records" link carries the
// scan cursor AND every filter, and the next request hands both back.
func TestAuditPageCursorSurvivesFilters(t *testing.T) {
	cap := &auditCapture{view: &AdminAuditView{
		Rows: []AdminAuditRow{{Seq: 9, At: "2026-11-20T10:00:00Z", Actor: "bob", Action: "hide"}},
		More: true, Next: 9, Scanned: 9,
	}}
	s := adminTestPage(t, auditDeps(cap), settingsTestPrincipal())
	body := settingsDo(t, s, http.MethodGet, "/admin/audit?action=hide&actor=bob&repo=acme%2Fw&since=7d", nil, nil).Body.String()
	for _, want := range []string{"after=9", "action=hide", "actor=bob", "repo=acme%2Fw", "since=7d", "older records"} {
		settingsMustContain(t, body, want, "paging link keeps the filter")
	}
	// The next page request resolves the same filter.
	s2 := adminTestPage(t, auditDeps(&auditCapture{}), settingsTestPrincipal())
	settingsDo(t, s2, http.MethodGet, "/admin/audit?after=9&action=hide", nil, nil)
}

func TestAuditPageBadInputIsNamedNotA500(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"?since=yesterday", "since: use a date"},
		{"?since=7", "since: use a date"},
		{"?action=two%20words", "action: a verb has no spaces"},
		{"?actor=two%20words", "actor: a principal has no spaces"},
		{"?repo=acme", `repo: use owner/name`},
	}
	for _, c := range cases {
		cap := &auditCapture{}
		s := adminTestPage(t, auditDeps(cap), settingsTestPrincipal())
		rec := settingsDo(t, s, http.MethodGet, "/admin/audit"+c.query, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", c.query, rec.Code)
		}
		settingsMustContain(t, rec.Body.String(), c.want, "named filter error")
		// The bad filter is ignored, not applied (the rest of the page works).
		if f := cap.filters[0]; f.Action == "two words" || f.Actor == "two words" || f.Repo == "acme" {
			t.Fatalf("%s: the bad value reached the seam: %+v", c.query, f)
		}
	}
}

func TestAuditPageEmptyStates(t *testing.T) {
	s := adminTestPage(t, auditDeps(&auditCapture{}), settingsTestPrincipal())
	body := settingsDo(t, s, http.MethodGet, "/admin/audit", nil, nil).Body.String()
	settingsMustContain(t, body, "No audit records yet.", "unfiltered empty state")

	s = adminTestPage(t, auditDeps(&auditCapture{}), settingsTestPrincipal())
	body = settingsDo(t, s, http.MethodGet, "/admin/audit?actor=nobody", nil, nil).Body.String()
	settingsMustContain(t, body, "No records match these filters", "filtered empty state")
	settingsMustContain(t, body, "clear", "the clear affordance")
}

func TestAuditPageIsNoStore(t *testing.T) {
	s := adminTestPage(t, auditDeps(&auditCapture{}), settingsTestPrincipal())
	rec := settingsDo(t, s, http.MethodGet, "/admin/audit", nil, nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestAuditPageScannedNoteAndActionList(t *testing.T) {
	cap := &auditCapture{view: &AdminAuditView{Scanned: 42, Next: 42}}
	s := adminTestPage(t, auditDeps(cap), settingsTestPrincipal())
	body := settingsDo(t, s, http.MethodGet, "/admin/audit", nil, nil).Body.String()
	settingsMustContain(t, body, "Examined 42 record(s)", "scanned note")
	settingsMustContain(t, body, `list="audit-actions"`, "verb suggestion list")
	settingsMustContain(t, body, `<option value="packages_gc">`, "a known verb is suggested")
}

func TestParseAuditSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in    string
		want  time.Time
		label string
		err   bool
	}{
		{"2026-11-15", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), "since 2026-11-15 (UTC midnight)", false},
		{"2026-11-20T10:00:00Z", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), "since 2026-11-20T10:00:00Z", false},
		{"7d", now.Add(-7 * 24 * time.Hour), "since 2026-11-15T12:00:00Z (7d)", false},
		{"12h", now.Add(-12 * time.Hour), "since 2026-11-22T00:00:00Z (12h)", false},
		{"90m", now.Add(-90 * time.Minute), "since 2026-11-22T10:30:00Z (90m)", false},
		{"2w", now.Add(-14 * 24 * time.Hour), "since 2026-11-08T12:00:00Z (2w)", false},
		{"", time.Time{}, "", false},
		{"7", time.Time{}, "", true},
		{"0d", time.Time{}, "", true},
		{"-3d", time.Time{}, "", true},
		{"yesterday", time.Time{}, "", true},
		{"7x", time.Time{}, "", true},
	}
	for _, c := range cases {
		got, label, err := parseAuditSince(c.in, now)
		if c.err {
			if err == "" {
				t.Fatalf("%q: expected an error", c.in)
			}
			continue
		}
		if err != "" {
			t.Fatalf("%q: %s", c.in, err)
		}
		if !got.Equal(c.want) {
			t.Fatalf("%q = %v, want %v", c.in, got, c.want)
		}
		if label != c.label {
			t.Fatalf("%q label = %q, want %q", c.in, label, c.label)
		}
	}
	// An empty filter encodes to nothing (no stray ?after=0 on page one).
	if got := auditQueryString(AdminAuditFilter{}, 0); got != "" {
		t.Fatalf("empty query = %q", got)
	}
	q, err := url.ParseQuery(auditQueryString(AdminAuditFilter{Action: "hide", Repo: "-"}, 4))
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("action") != "hide" || q.Get("repo") != "-" || q.Get("after") != "4" {
		t.Fatalf("query = %v", q)
	}
}
