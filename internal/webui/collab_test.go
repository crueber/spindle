package webui

// collab_test.go — the collaboration page family's httptest suite.
//
// The fakes are intentionally dumb: api.RepoView/RepoRegistry only answer
// existence + feature flags, and every CollabDeps field the pages read is
// bound to a function field the test owns. That keeps the assertions on
// page shape/gates/round-trips (spec §3 "Proposed httptest" lists) without
// re-testing the feature services, which have their own suites.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/checks"
	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/identity"
	"github.com/crueber/walhub/internal/issues"
	"github.com/crueber/walhub/internal/pages"
	"github.com/crueber/walhub/internal/policy"
	"github.com/crueber/walhub/internal/pulls"
	"github.com/crueber/walhub/internal/releases"
	"github.com/crueber/walhub/internal/review"
	"github.com/crueber/walhub/internal/server/auth"
	"github.com/crueber/walhub/internal/store"
)

// --- fakes -------------------------------------------------------------------

type collabFakeRegistry struct {
	exists map[string]bool
}

func (f *collabFakeRegistry) Owners(context.Context) ([]string, error) { return []string{}, nil }
func (f *collabFakeRegistry) Repos(context.Context, string) ([]string, error) {
	return []string{}, nil
}
func (f *collabFakeRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}
func (f *collabFakeRegistry) Exists(_ context.Context, id git.RepoId) (bool, error) {
	return f.exists[id.Owner+"/"+id.Name], nil
}
func (f *collabFakeRegistry) Create(context.Context, git.RepoId, git.ObjectFormat) error { return nil }
func (f *collabFakeRegistry) Delete(context.Context, git.RepoId) error                   { return nil }

type collabFakeView struct {
	summary api.SummaryData
	err     error
	known   map[string]bool
}

func (f *collabFakeView) Sync(context.Context, git.RepoId, api.SyncLevel) error { return nil }
func (f *collabFakeView) Resolve(context.Context, git.RepoId, string) (api.Resolution, error) {
	return api.Resolution{}, nil
}
func (f *collabFakeView) Head(context.Context, git.RepoId) (api.Ref, bool, error) {
	return api.Ref{}, false, nil
}
func (f *collabFakeView) RefList(context.Context, git.RepoId, string, api.RefQuery) ([]api.Ref, bool, error) {
	return []api.Ref{}, false, nil
}
func (f *collabFakeView) Tree(context.Context, git.RepoId, string, string) (api.TreeResult, error) {
	return api.TreeResult{}, nil
}
func (f *collabFakeView) Blob(context.Context, git.RepoId, string, string, bool) (api.BlobResult, error) {
	return api.BlobResult{}, nil
}
func (f *collabFakeView) Commits(context.Context, git.RepoId, string, string, int, int) (api.CommitPage, error) {
	return api.CommitPage{}, nil
}
func (f *collabFakeView) Commit(context.Context, git.RepoId, string) (api.CommitDetail, error) {
	return api.CommitDetail{}, nil
}
func (f *collabFakeView) Summary(_ context.Context, id git.RepoId) (api.SummaryData, error) {
	if f.err != nil {
		return api.SummaryData{}, f.err
	}
	if f.known != nil && !f.known[id.Owner+"/"+id.Name] {
		return api.SummaryData{}, api.ErrNotFound
	}
	return f.summary, nil
}
func (f *collabFakeView) Overview(context.Context, git.RepoId) (api.OverviewData, error) {
	return api.OverviewData{}, nil
}
func (f *collabFakeView) Settings(context.Context, git.RepoId) (api.SettingsDoc, error) {
	return api.SettingsDoc{}, nil
}
func (f *collabFakeView) PublishSettings(context.Context, git.RepoId, []byte, string, string) (uint64, error) {
	return 0, nil
}
func (f *collabFakeView) SettingsHistory(context.Context, git.RepoId) (api.SettingsHistory, error) {
	return api.SettingsHistory{}, nil
}
func (f *collabFakeView) HeadSeq(context.Context, git.RepoId) (uint64, error) { return 0, nil }
func (f *collabFakeView) PushHistory(context.Context, git.RepoId, int) ([]api.PushRecord, error) {
	return []api.PushRecord{}, nil
}

// --- harness -----------------------------------------------------------------

const testHost = "example.test"

type collabFixture struct {
	t    *testing.T
	srv  *Server
	view *collabFakeView
	reg  *collabFakeRegistry

	// principal resolved by the fake authenticator (zero = anonymous).
	principal auth.Principal
	// readGate, when set, is the identity require_read verdict.
	readGate func(owner, repo string, p auth.Principal) *auth.AuthError
}

func newCollabFixture(t *testing.T) *collabFixture {
	t.Helper()
	f := &collabFixture{
		t:         t,
		principal: auth.Anonymous(),
		view:      &collabFakeView{summary: api.SummaryData{Features: ptrFeatures(config.AllFeatures())}},
		reg:       &collabFakeRegistry{exists: map[string]bool{"acme/widgets": true}},
	}
	cfg := &config.Config{}
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.AnonymousRead = true
	env := Env{
		Cfg: cfg,
		API: &api.Env{Repos: f.reg, Repo: f.view},
		Now: func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) },
	}
	srv, err := New(env)
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	srv.BindAuth(pages.Auth{
		Authenticate: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return f.principal, nil
		},
		CheckRead: func(_ context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
			if f.readGate != nil {
				return f.readGate(owner, repo, p)
			}
			return nil
		},
	})
	f.view.known = f.reg.exists
	f.srv = srv
	t.Cleanup(func() { collabDeps = CollabDeps{} })
	return f
}

func ptrFeatures(f config.ResolvedFeatures) *config.ResolvedFeatures { return &f }

func (f *collabFixture) deps(d CollabDeps) { collabDeps = d }

// req builds a same-origin request (mutation tests need the Origin header).
func (f *collabFixture) req(method, path string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Host = testHost
	r.Header.Set("Origin", "http://"+testHost)
	return r
}

func (f *collabFixture) get(path string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, f.req("GET", path, nil)) {
		f.t.Fatalf("GET %s was not claimed by the webui mount", path)
	}
	return rec
}

func (f *collabFixture) post(path string, form url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, f.req("POST", path, form)) {
		f.t.Fatalf("POST %s was not claimed by the webui mount", path)
	}
	return rec
}

func bodyText(rec *httptest.ResponseRecorder) string { return rec.Body.String() }

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, firstN(bodyText(rec), 600))
	}
}

func wantContains(t *testing.T, rec *httptest.ResponseRecorder, needles ...string) {
	t.Helper()
	body := bodyText(rec)
	for _, n := range needles {
		if !strings.Contains(body, n) {
			t.Fatalf("body missing %q; body: %s", n, firstN(body, 800))
		}
	}
}

func wantNotContains(t *testing.T, rec *httptest.ResponseRecorder, needles ...string) {
	t.Helper()
	body := bodyText(rec)
	for _, n := range needles {
		if strings.Contains(body, n) {
			t.Fatalf("body unexpectedly contains %q", n)
		}
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- fixtures ----------------------------------------------------------------

func testThread() *issues.Thread {
	return &issues.Thread{
		Num: 3, Kind: "issue", Title: "Crash on save", State: "open",
		Author: "alice", CreatedAt: "2026-11-18T10:00:00Z", UpdatedAt: "2026-11-18T11:00:00Z",
		Labels: []string{"bug"}, Participants: []string{"alice", "bob"},
		CommentCount: 2, ReactionSummary: map[string]map[string]int{"000001": {"+1": 2}},
		Version: 3,
	}
}

func issueDeps() CollabDeps {
	th := testThread()
	return CollabDeps{
		IssuesList: func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
			return &issues.ListResult{Issues: []issues.Card{{
				Num: 3, Kind: "issue", Title: "Crash on save", State: "open",
				Labels: []string{"bug"}, Author: "alice",
				CreatedAt: "2026-11-18T10:00:00Z", UpdatedAt: "2026-11-18T11:00:00Z", CommentCount: 2,
			}}, More: false}, nil
		},
		IssuesGet: func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
			return &issues.ThreadView{
				Thread: th, EventsMore: true,
				Events: []*issues.Event{
					{Seq: 2, Type: issues.EventCommented, Actor: "bob", At: "2026-11-18T11:00:00Z", Body: ptr("second")},
					{Seq: 1, Type: issues.EventOpened, Actor: "alice", At: "2026-11-18T10:00:00Z", Body: ptr("first")},
					{Seq: 3, Type: issues.EventStateChanged, Actor: "alice", At: "2026-11-18T12:00:00Z", To: ptr("closed"), Reason: ptr("completed")},
					{Seq: 4, Type: issues.EventReactionChanged, Actor: "bob", At: "2026-11-18T12:30:00Z", TargetEventSeq: intPtr(1), Content: ptr("+1"), Op: ptr("add")},
				},
			}, nil
		},
		Labels: func(context.Context, string, string) ([]issues.Label, error) {
			return []issues.Label{{Name: "bug", Color: "d73a4a", Description: "broken"}}, nil
		},
		Milestones: func(context.Context, string, string) ([]*issues.Milestone, error) {
			return []*issues.Milestone{{ID: "000001", Title: "v1", State: "open"}}, nil
		},
	}
}

func ptr(s string) *string { return &s }
func intPtr(n int) *int    { return &n }

// --- issues list -------------------------------------------------------------

func TestCollabIssuesList(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(issueDeps())

	rec := f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Issues", "#3 Crash on save", "chip w-fit", `aria-label="issue filters"`, `name="state"`, `name="assignee"`, `name="milestone"`, "New issue")

	// ?state=all resolves to the both-states option and the service gets "".
	var got issues.ListFilter
	d := issueDeps()
	d.IssuesList = func(_ context.Context, _, _ string, _ auth.Principal, fl issues.ListFilter) (*issues.ListResult, error) {
		got = fl
		return &issues.ListResult{}, nil
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/issues?state=all"), http.StatusOK)
	if got.State != "" {
		t.Fatalf("state=all must omit the wire state, got %q", got.State)
	}
	if got.Labels != nil {
		t.Fatalf("no labels filter expected, got %v", got.Labels)
	}

	// Unknown state renders 400 inline (never a redirect, never a panic).
	rec = f.get("/acme/widgets/issues?state=bogus")
	wantStatus(t, rec, http.StatusBadRequest)

	// Empty state.
	d.IssuesList = func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
		return &issues.ListResult{}, nil
	}
	f.deps(d)
	rec = f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "No issues match")

	// Unknown repo → 404 page (never the SPA fallthrough).
	rec = f.get("/acme/unknown/issues")
	wantStatus(t, rec, http.StatusNotFound)
	wantContains(t, rec, "unknown repository")

	// Gate: private repo, anonymous caller.
	f.readGate = func(string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	rec = f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusUnauthorized)
}

func TestCollabIssuesFeatureOffHidesCreation(t *testing.T) {
	f := newCollabFixture(t)
	off := config.AllFeatures()
	off.Issues = false
	f.view.summary.Features = ptrFeatures(off)
	f.deps(issueDeps())
	rec := f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "#3 Crash on save")
	wantNotContains(t, rec, ">New issue</a>")
}

func TestCollabAnonymousWriteTarget(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Anonymous()
	cfg := f.srv.env.Cfg
	cfg.Server.Auth.Mode = "oidc"
	cfg.Server.Auth.AnonymousRead = true
	f.deps(issueDeps())
	rec := f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "/login-required?next=%2Facme%2Fwidgets%2Fissues%2Fnew")
}

// --- issue thread ------------------------------------------------------------

func TestCollabIssuePage(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	f.deps(issueDeps())

	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "#3", "Crash on save", "chip-open", "alice opened", "first", "second",
		`id="event-1"`, `id="event-2"`, "reaction-row", "👍 2", `aria-label="issue filters"`[:0]+`aria-label="Issue metadata"`,
		"Older events")

	// Chronological order: event 1 renders before event 2.
	body := bodyText(rec)
	if strings.Index(body, `id="event-1"`) > strings.Index(body, `id="event-2"`) {
		t.Fatal("timeline must render oldest → newest")
	}
	// reaction_changed rows are folded into the summary, never rendered raw.
	wantNotContains(t, rec, `id="event-4"`)

	// Unknown number / non-numeric → 404.
	wantStatus(t, f.get("/acme/widgets/issues/0"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/issues/nope"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/issues/3/bogus"), http.StatusNotFound)

	// Comment round-trip.
	var commented string
	d := issueDeps()
	d.IssueComment = func(_ context.Context, _, _ string, num int, _ auth.Principal, body string) (*issues.Event, error) {
		commented = body
		return &issues.Event{Seq: 7}, nil
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/comments", url.Values{"body": {"hello"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/issues/3#event-7" {
		t.Fatalf("redirect = %q", loc)
	}
	if commented != "hello" {
		t.Fatalf("service got body %q", commented)
	}

	// Validation error: the service's 400 re-renders the thread with an alert.
	d.IssueComment = func(context.Context, string, string, int, auth.Principal, string) (*issues.Event, error) {
		return nil, errors.Join(issues.ErrInvalid, errors.New("body must not be empty"))
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/comments", url.Values{"body": {""}})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, `role="alert"`, "body must not be empty")

	// Cross-origin post is refused before the service runs.
	req := f.req("POST", "/acme/widgets/issues/3/comments", url.Values{"body": {"x"}})
	req.Header.Set("Origin", "http://evil.test")
	rec = httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("post not claimed")
	}
	wantStatus(t, rec, http.StatusForbidden)
}

func TestCollabIssueCloseReaction(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := issueDeps()
	var patched issues.IssuePatch
	d.IssuePatch = func(_ context.Context, _, _ string, _ int, _ auth.Principal, p issues.IssuePatch) (*issues.Thread, error) {
		patched = p
		return testThread(), nil
	}
	var reacted, unreacted string
	d.IssueReact = func(_ context.Context, _, _ string, _, _ int, _ auth.Principal, c string) (*issues.Thread, *issues.Event, bool, error) {
		reacted = c
		return testThread(), &issues.Event{Seq: 9}, true, nil
	}
	d.IssueUnreact = func(_ context.Context, _, _ string, _, _ int, _ auth.Principal, c string) (*issues.Thread, error) {
		unreacted = c
		return testThread(), nil
	}
	f.deps(d)

	rec := f.post("/acme/widgets/issues/3/close", url.Values{"reason": {"not_planned"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if patched.State == nil || *patched.State != "closed" || patched.StateReason == nil || *patched.StateReason != "not_planned" {
		t.Fatalf("close patch = %+v", patched)
	}

	rec = f.post("/acme/widgets/issues/3/reactions/1", url.Values{"content": {"+1"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if reacted != "+1" {
		t.Fatalf("reacted %q", reacted)
	}
	rec = f.post("/acme/widgets/issues/3/reactions/1/%2B1/delete", nil)
	wantStatus(t, rec, http.StatusSeeOther)
	if unreacted != "+1" {
		t.Fatalf("unreacted %q", unreacted)
	}

	// Reaction gate: triage is not needed (read + authenticated), but the
	// service's error surfaces inline.
	d.IssueReact = func(context.Context, string, string, int, int, auth.Principal, string) (*issues.Thread, *issues.Event, bool, error) {
		return nil, nil, false, errors.Join(issues.ErrLocked, errors.New("issue #3 is closed"))
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/reactions/1", url.Values{"content": {"+1"}})
	wantStatus(t, rec, http.StatusConflict)
	wantContains(t, rec, "is closed")
}

// --- pulls -------------------------------------------------------------------

func pullThread() *pulls.Thread {
	return &pulls.Thread{
		Num: 5, Kind: "pr", Title: "Add cache", State: "open", Author: "alice",
		CreatedAt: "2026-11-18T09:00:00Z", UpdatedAt: "2026-11-18T09:30:00Z",
		Participants: []string{"alice", "carol"}, CommentCount: 1, Version: 2,
	}
}

func pullDeps() CollabDeps {
	th := pullThread()
	pr := &pulls.PRDoc{
		Num: 5, Kind: "pr",
		Base: pulls.Endpoint{Repo: "acme/widgets", Ref: "main", SHA: strings.Repeat("a", 40)},
		Head: pulls.Endpoint{Repo: "acme/widgets", Ref: "feat", SHA: strings.Repeat("b", 40)},
		Body: "Adds a cache.", Draft: false,
	}
	return CollabDeps{
		PullsList: func(context.Context, string, string, auth.Principal, pulls.ListFilter) (*pulls.ListResult, error) {
			return &pulls.ListResult{Pulls: []pulls.PROut{{
				Num: 5, Title: "Add cache", State: "closed", Author: "alice",
				BaseRef: "main", HeadRef: "feat", Merged: true, UpdatedAt: "2026-11-18T09:30:00Z",
			}}}, nil
		},
		PullGet: func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
			return &pulls.PullView{
				Thread: th, PR: pr, HeadLive: strings.Repeat("b", 40), BaseLive: strings.Repeat("a", 40), HeadRefOk: true,
				Mergeable: &pulls.MergeableDoc{State: pulls.MergeableClean},
				Events: []*pulls.Event{
					{Seq: 2, Type: pulls.EventCommented, Actor: "carol", At: "2026-11-18T09:30:00Z", Body: ptr("nice")},
					{Seq: 1, Type: pulls.EventOpened, Actor: "alice", At: "2026-11-18T09:00:00Z"},
				},
			}, nil
		},
		PullDiff: func(context.Context, string, string, int, auth.Principal) (string, error) {
			return "diff --git a/x.txt b/x.txt\nindex 111..222 100644\n--- a/x.txt\n+++ b/x.txt\n@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n", nil
		},
		PullCommits: func(context.Context, string, string, int, auth.Principal, int, int) ([]pulls.CommitEntry, bool, error) {
			return []pulls.CommitEntry{{SHA: strings.Repeat("c", 40), Subject: "tidy", Author: "alice", At: "2026-11-18T08:00:00Z"}}, false, nil
		},
		ForksList: func(context.Context, string, string, auth.Principal, string, int) (*pulls.ForkListPage, error) {
			return &pulls.ForkListPage{Forks: []pulls.ForkListEntry{{Repo: "bob/widgets", ForkedAt: "2026-11-17T00:00:00Z"}}}, nil
		},
	}
}

func TestCollabPullsList(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(pullDeps())
	rec := f.get("/acme/widgets/pulls")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Pull requests", "#5 Add cache", "chip-merged", "main ← feat", "New pull request", "state=all")

	d := pullDeps()
	d.PullsList = func(context.Context, string, string, auth.Principal, pulls.ListFilter) (*pulls.ListResult, error) {
		return &pulls.ListResult{}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/pulls"), "No pull requests")
	wantContains(t, f.get("/acme/widgets/pulls?state=closed"), "No closed pull requests")
}

func TestCollabPullPage(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := pullDeps()
	var reviewed review.SubmitInput
	d.ReviewsList = func(context.Context, string, string, int, auth.Principal, int, int) (*review.ListResult, error) {
		return &review.ListResult{Reviews: []*review.ReviewEvent{{
			Kind: review.KindReview, Seq: 1, At: "2026-11-18T09:20:00Z", By: "carol",
			State: review.StateApproved, CommitSHA: strings.Repeat("b", 40), Body: "lgtm",
		}}}, nil
	}
	d.ThreadsList = func(context.Context, string, string, int, auth.Principal, *bool, string, int) (*review.ThreadListResult, error) {
		return &review.ThreadListResult{Threads: []*review.ThreadHeader{{
			TID: "00000001", Num: 5, Kind: "review_thread",
			Anchor:       review.Anchor{Path: "x.txt", Side: review.SideNew, NewStart: 1, NewLines: 1},
			CommentCount: 1, CreatedBy: "carol", UpdatedAt: "2026-11-18T09:25:00Z",
		}}}, nil
	}
	d.ReviewSubmit = func(_ context.Context, _, _ string, _ int, _ auth.Principal, in review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error) {
		reviewed = in
		return &review.ReviewEvent{Seq: 2}, nil, nil, nil
	}
	d.ChecksCombined = func(_ context.Context, _, _, sha string, _ auth.Principal) (*checks.CombinedView, error) {
		return &checks.CombinedView{SHA: sha, State: checks.StateSuccess, Statuses: []*checks.StatusDoc{
			{SHA: sha, Context: "ci/test", State: checks.StateSuccess, UpdatedAt: "2026-11-18T09:00:00Z", Creator: "ci"},
		}}, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "#5", "Add cache", "chip-open", "Adds a cache.", "opened", "nice",
		"Files", "x.txt", "diff-add", "diff-del", "Reviews", "Approved", "Reviewers", "Mergeability",
		"Able to be Merged", "ci/test", "merge pull request", `id="thread-00000001"`, "Comments")

	// Review submit round-trip.
	rec = f.post("/acme/widgets/pull/5/reviews", url.Values{"verdict": {"APPROVED"}, "body": {"ok"}, "commit_sha": {strings.Repeat("b", 40)}})
	wantStatus(t, rec, http.StatusSeeOther)
	if reviewed.State != review.StateApproved || reviewed.CommitSHA != strings.Repeat("b", 40) {
		t.Fatalf("review input = %+v", reviewed)
	}

	// Validation error re-renders with the alert.
	d.ReviewSubmit = func(context.Context, string, string, int, auth.Principal, review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error) {
		return nil, nil, nil, errors.Join(review.ErrInvalid, errors.New("state must be APPROVED|CHANGES_REQUESTED|COMMENTED"))
	}
	f.deps(d)
	rec = f.post("/acme/widgets/pull/5/reviews", url.Values{"verdict": {"NOPE"}, "commit_sha": {strings.Repeat("b", 40)}})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, `role="alert"`, "state must be")

	// Thread staging: the anchor is built server-side from the live diff.
	var anchor review.Anchor
	d.ThreadOpen = func(_ context.Context, _, _ string, _ int, _ auth.Principal, a review.Anchor, _ string) (*review.ThreadHeader, error) {
		anchor = a
		return &review.ThreadHeader{TID: "00000002"}, nil
	}
	f.deps(d)
	rec = f.post("/acme/widgets/pull/5/threads", url.Values{
		"path": {"x.txt"}, "side": {"new"}, "start": {"1"}, "end": {"1"}, "body": {"why?"},
		"next": {"/acme/widgets/pull/5/files"},
	})
	wantStatus(t, rec, http.StatusSeeOther)
	if anchor.Side != review.SideNew || anchor.NewStart != 1 || anchor.NewLines != 1 || anchor.Path != "x.txt" {
		t.Fatalf("anchor = %+v", anchor)
	}
	if anchor.CommitSHA != strings.Repeat("b", 40) || anchor.ContextSHA == "" {
		t.Fatalf("anchor head/hash = %+v", anchor)
	}

	// A stale selection re-renders the files page with the honest error.
	rec = f.post("/acme/widgets/pull/5/threads", url.Values{
		"path": {"nope.txt"}, "side": {"new"}, "start": {"9"}, "end": {"9"}, "body": {"x"},
	})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, "names no line in this diff")

	// Merge gate: write (not maintain) is refused on the form POST.
	f.principal = auth.Principal{Name: "alice", Write: true}
	rec = f.post("/acme/widgets/pull/5/merge", url.Values{"strategy": {"merge"}})
	wantStatus(t, rec, http.StatusForbidden)
}

func TestCollabPullFilesAndCommits(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(pullDeps())

	rec := f.get("/acme/widgets/pull/5/files")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Files on #5", "x.txt", "diff-add", "diff-del", `data-diff-row`)

	rec = f.get("/acme/widgets/pull/5/commits")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Commits on #5", "tidy", "alice")

	wantStatus(t, f.get("/acme/widgets/pull/5/bogus"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/pull/nope"), http.StatusNotFound)
}

func TestCollabForks(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(pullDeps())
	rec := f.get("/acme/widgets/forks")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Forks of", "bob/widgets", "forked 2026-11-17")

	d := pullDeps()
	d.ForksList = func(context.Context, string, string, auth.Principal, string, int) (*pulls.ForkListPage, error) {
		return &pulls.ForkListPage{}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/forks"), "No forks yet.")
}

// --- checks ------------------------------------------------------------------

func checkDeps() CollabDeps {
	sha := strings.Repeat("d", 40)
	return CollabDeps{
		ChecksList: func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
			return &checks.ChecksPage{Checks: []checks.IndexSHA{{
				SHA: sha, State: checks.StateFailure, UpdatedAt: "2026-11-18T09:00:00Z",
				Contexts: []checks.IndexContext{{Name: "ci/test", State: checks.StateFailure, UpdatedAt: "2026-11-18T09:00:00Z"}},
			}}}, nil
		},
		ChecksStatuses: func(_ context.Context, _, _, s string, _ auth.Principal) (*checks.StatusesView, error) {
			return &checks.StatusesView{SHA: s, Statuses: []*checks.StatusDoc{{
				SHA: s, Context: "ci/test", State: checks.StateFailure,
				TargetURL: "https://ci.test/1", Description: "3 failed", Creator: "ci",
				UpdatedAt: "2026-11-18T09:00:00Z",
			}}}, nil
		},
		ChecksCombined: func(_ context.Context, _, _, s string, _ auth.Principal) (*checks.CombinedView, error) {
			return &checks.CombinedView{SHA: s, State: checks.StateFailure, Statuses: []*checks.StatusDoc{{
				SHA: s, Context: "ci/test", State: checks.StateFailure, TargetURL: "https://ci.test/1",
				Description: "3 failed", Creator: "ci", UpdatedAt: "2026-11-18T09:00:00Z",
			}}}, nil
		},
	}
}

func TestCollabChecks(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(checkDeps())
	sha := strings.Repeat("d", 40)

	rec := f.get("/acme/widgets/checks")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Checks", "1 context", "failure", "bg-red-500", "reporting API", `hx-get="/acme/widgets/checks/`+sha+`/contexts"`)

	// The expand fallback (?expanded=) renders the context rows inline.
	rec = f.get("/acme/widgets/checks?expanded=" + sha)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "3 failed", `rel="noreferrer"`, "https://ci.test/1")

	// Empty index.
	d := checkDeps()
	d.ChecksList = func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
		return &checks.ChecksPage{}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/checks"), "No checks reported yet")

	// Detail page.
	f.deps(checkDeps())
	rec = f.get("/acme/widgets/checks/" + sha)
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Checks for", "ci/test", "3 failed", "bg-red-500")
	// Non-hex sha is 404, never a 400 page.
	wantStatus(t, f.get("/acme/widgets/checks/nothex"), http.StatusNotFound)

	// Fragment endpoint.
	rec = f.get("/acme/widgets/checks/" + sha + "/contexts")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "ci/test", `rel="noreferrer"`)
}

// --- releases ----------------------------------------------------------------

func releaseDeps() CollabDeps {
	published := "2026-11-17T00:00:00Z"
	rel := &releases.Release{
		Tag: "v1.2.3", TagSHA: strings.Repeat("e", 40), Name: "v1.2.3", Body: "# Notes\n\n- fixed things\n",
		Author: "alice", CreatedAt: "2026-11-17T00:00:00Z", PublishedAt: &published, UpdatedAt: published,
		Assets:  []releases.AssetEntry{{Name: "sums.txt", Size: 1536, SHA256: strings.Repeat("f", 64), ContentType: "text/plain"}},
		Version: 2,
	}
	return CollabDeps{
		ReleasesList: func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
			return []*releases.Release{rel}, false, nil
		},
		ReleaseGet: func(context.Context, string, string, auth.Principal, string) (*releases.Release, store.Version, error) {
			return rel, "v1", nil
		},
		ReleaseLatest: func(context.Context, string, string, auth.Principal, bool) (*releases.Release, store.Version, error) {
			return rel, "v1", nil
		},
	}
}

func TestCollabReleases(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true, Admin: true}
	d := releaseDeps()
	var saved releases.ReleaseInput
	d.ReleasePut = func(_ context.Context, _, _ string, _ auth.Principal, _ string, in releases.ReleaseInput, _ store.Version) (*releases.Release, bool, error) {
		saved = in
		return nil, false, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/releases")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Releases", "v1.2.3", "latest", "All (1)", "1 asset", "sums.txt")

	rec = f.get("/acme/widgets/releases/v1.2.3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "v1.2.3", "Release notes", "fixed things", "Assets (1)", "sums.txt", "1.5 KiB", "Delete")

	// Save round-trip.
	rec = f.post("/acme/widgets/releases/v1.2.3/save", url.Values{"name": {"v1.2.3"}, "body": {"new notes"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if saved.Body == nil || *saved.Body != "new notes" {
		t.Fatalf("saved input = %+v", saved)
	}

	// Validation error re-renders the edit form with the alert.
	d.ReleasePut = func(context.Context, string, string, auth.Principal, string, releases.ReleaseInput, store.Version) (*releases.Release, bool, error) {
		return nil, false, errors.Join(releases.ErrInvalid, errors.New("name too long"))
	}
	f.deps(d)
	rec = f.post("/acme/widgets/releases/v1.2.3/save", url.Values{"name": {"n"}, "body": {"b"}})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, `role="alert"`, "name too long", "name=\"name\"")

	// The reserved "new" sibling belongs to the forms group; this group must
	// not claim it (its literal route wins in the mux, so the assertion is
	// that /releases/{tag} never answers it as a release).
	// Unknown tag → 404.
	d.ReleaseGet = func(context.Context, string, string, auth.Principal, string) (*releases.Release, store.Version, error) {
		return nil, "", releases.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/releases/nope"), http.StatusNotFound)

	// Delete requires maintain: a write-only principal is refused.
	f.principal = auth.Principal{Name: "alice", Write: true}
	cfg := f.srv.env.Cfg
	_ = cfg
	f.deps(releaseDeps())
	rec = f.post("/acme/widgets/releases/v1.2.3/delete", nil)
	wantStatus(t, rec, http.StatusForbidden)

	// Empty list.
	d = releaseDeps()
	d.ReleasesList = func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
		return nil, false, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/releases"), "No releases yet")
}

// --- routes / fallthrough ----------------------------------------------------

func TestCollabRouteOwnership(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(pullDeps())

	// Unclaimed repo paths stay unclaimed (the SPA fallback keeps them).
	rec := httptest.NewRecorder()
	if f.srv.Serve(rec, f.req("GET", "/acme/widgets/wiki", nil)) {
		t.Fatal("an unclaimed repo subpath must not be claimed by the collab group")
	}
	// Write endpoints are not page routes: POST on the page URL is never
	// claimed by the page mount (during the migration the sentinel hands it
	// back; the server's own repo dispatch answers 405 + Allow: GET, HEAD).
	rec = httptest.NewRecorder()
	if f.srv.Serve(rec, f.req("POST", "/acme/widgets/issues", url.Values{})) {
		t.Fatal("POST on a page URL must not be claimed by the collab mount")
	}
}

func TestCollabHelpers(t *testing.T) {
	if got := issueEventText(&issues.Event{Type: issues.EventStateChanged, To: ptr("closed")}, nil); got != "closed" {
		t.Fatalf("unhonest close text: %q", got)
	}
	if got := issueEventText(&issues.Event{Type: issues.EventStateChanged, To: ptr("closed"), Reason: ptr("completed")}, nil); got != "closed as completed" {
		t.Fatalf("close text: %q", got)
	}
	if got := closedStateLabel(ptr("not_planned")); got != "Closed as not planned" {
		t.Fatalf("closedStateLabel = %q", got)
	}
	if got := excerptBody("\n# Heading here\nsecond", 140); got != "Heading here" {
		t.Fatalf("excerpt = %q", got)
	}
	if got := excerptBody("short", 3); got != "sh…" {
		t.Fatalf("excerpt truncation = %q", got)
	}
	shown, addable := reactionRows(map[string]map[string]int{"000003": {"+1": 2, "eyes": 1}}, 3)
	if len(shown) != 2 || shown[0].Content != "+1" || len(addable) != 6 {
		t.Fatalf("reactions = %+v / %+v", shown, addable)
	}
	if got := fmtAssetSize(1536); got != "1.5 KiB" {
		t.Fatalf("size = %q", got)
	}
	if got := pullListChip(pulls.PROut{State: "closed", Merged: true}); got.Text != "merged" {
		t.Fatalf("merged chip = %+v", got)
	}
	if got := pullBadgeView(&pulls.Thread{State: "open"}, &pulls.PRDoc{Draft: true}); got.Text != "Draft" {
		t.Fatalf("draft badge = %+v", got)
	}
	if _, ok := issueNum("0003"); ok {
		t.Fatal("leading zeros must not parse")
	}
	if !fullSHA(strings.Repeat("a", 40)) || fullSHA("abc") {
		t.Fatal("fullSHA")
	}
	// Zone: identical bytes keep the same drift hash.
	a1, ok := anchorFor(ParseUnifiedDiff("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n"), "x", "new", 1, 1, strings.Repeat("a", 40))
	if !ok {
		t.Fatal("anchorFor failed")
	}
	a2, _ := anchorFor(ParseUnifiedDiff("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n"), "x", "new", 1, 1, strings.Repeat("a", 40))
	if a1.ContextSHA != a2.ContextSHA {
		t.Fatal("drift hash is not deterministic")
	}
	if a1.Side != review.SideNew || a1.OldStart != 0 || a1.NewStart != 1 {
		t.Fatalf("anchor shape = %+v", a1)
	}
	if !sameOrigin(&http.Request{Host: "h", Header: http.Header{"Origin": {"http://h"}}}) {
		t.Fatal("same-origin rejected")
	}
	if sameOrigin(&http.Request{Host: "h", Header: http.Header{}}) {
		t.Fatal("missing Origin/Referer must be refused")
	}
}

// TestCollabMobileContracts pins the narrow-viewport rules in markup: list
// rows wrap instead of panning, the issue/PR two-column grid collapses to
// one column below md:, and diffs scroll inside their own card (never the
// page). Rendered proof at 390px is the integrator's Chromium pass; these
// assertions keep the classes from regressing in the meantime.
func TestCollabMobileContracts(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	f.deps(pullDeps())

	wantContains(t, f.get("/acme/widgets/pulls"), "flex min-w-0 flex-wrap", "flex-wrap")
	wantContains(t, f.get("/acme/widgets/pull/5"), "md:grid-cols-[minmax(0,1fr)_16rem]", "flex-wrap items-start")
	wantContains(t, f.get("/acme/widgets/pull/5/files"), "overflow-x-auto", "flex flex-wrap items-center gap-2")
	wantContains(t, f.get("/acme/widgets/forks"), "flex items-baseline justify-between gap-3")
	wantContains(t, f.get("/acme/widgets/checks"), "flex flex-wrap items-center gap-2")
}

// --- write endpoints ---------------------------------------------------------

func TestCollabIssueWrites(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := issueDeps()
	th := testThread()
	var patches []issues.IssuePatch
	d.IssuePatch = func(_ context.Context, _, _ string, _ int, _ auth.Principal, p issues.IssuePatch) (*issues.Thread, error) {
		patches = append(patches, p)
		return th, nil
	}
	f.deps(d)

	// Label toggle adds the missing name to the full next set.
	if rec := f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"needs-test"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("labels status %d", rec.Code)
	}
	if len(patches) != 1 || patches[0].Labels == nil || len(*patches[0].Labels) != 2 {
		t.Fatalf("label patch = %+v", patches)
	}
	// Toggling an applied label removes it.
	if rec := f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"bug"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("labels remove status %d", rec.Code)
	}
	if len(*patches[1].Labels) != 0 {
		t.Fatalf("label remove patch = %+v", patches[1])
	}
	// Missing label name is a 400 inline.
	if rec := f.post("/acme/widgets/issues/3/labels", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty label status %d", rec.Code)
	}
	// Milestone set and clear.
	if rec := f.post("/acme/widgets/issues/3/milestone", url.Values{"milestone": {"000001"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("milestone status %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/milestone", url.Values{"milestone": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("milestone clear status %d", rec.Code)
	}
	// Comment and close (composite): comment first, then the state patch.
	var comments int
	d.IssueComment = func(context.Context, string, string, int, auth.Principal, string) (*issues.Event, error) {
		comments++
		return &issues.Event{Seq: 5}, nil
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/close", url.Values{"body": {"done"}, "reason": {"completed"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("comment+close status %d", rec.Code)
	}
	if comments != 1 {
		t.Fatalf("comment count = %d", comments)
	}
	// Close with no reason defaults to completed (the split button shows it).
	if rec := f.post("/acme/widgets/issues/3/close", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("close status %d", rec.Code)
	}
	last := patches[len(patches)-1]
	if last.StateReason == nil || *last.StateReason != issues.ReasonCompleted {
		t.Fatalf("default reason = %+v", last.StateReason)
	}
	// A bogus reason is a 400 inline, never a silent completed.
	if rec := f.post("/acme/widgets/issues/3/close", url.Values{"reason": {"bogus"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus reason status %d", rec.Code)
	}
	// Reopen.
	if rec := f.post("/acme/widgets/issues/3/reopen", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("reopen status %d", rec.Code)
	}
	// Role floor: triage-only endpoints refuse a read-only principal.
	f.principal = auth.Principal{Name: "bob"}
	if rec := f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"x"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only label status %d", rec.Code)
	}
	// Anonymous write in oidc mode redirects to the interstitial.
	f.principal = auth.Anonymous()
	f.srv.env.Cfg.Server.Auth.Mode = "oidc"
	rec := f.post("/acme/widgets/issues/3/comments", url.Values{"body": {"hi"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login-required?next=") {
		t.Fatalf("anon write = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCollabPullWrites(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}
	d := pullDeps()
	var updated []pulls.PRPatch
	d.PullUpdate = func(_ context.Context, _, _ string, _ int, _ auth.Principal, p pulls.PRPatch) (*pulls.Thread, *pulls.PRDoc, error) {
		updated = append(updated, p)
		return pullThread(), nil, nil
	}
	d.PullComment = func(context.Context, string, string, int, auth.Principal, string) (*pulls.Event, error) {
		return &pulls.Event{Seq: 4}, nil
	}
	var dismissed string
	d.ReviewDismiss = func(_ context.Context, _, _ string, _, _ int, _ auth.Principal, reason string) (*review.ReviewEvent, *review.ReviewSummary, error) {
		dismissed = reason
		return &review.ReviewEvent{Seq: 3}, nil, nil
	}
	var added, removed []string
	d.RequestsAdd = func(_ context.Context, _, _ string, _ int, _ auth.Principal, ps []string) (*review.ReviewRequests, error) {
		added = ps
		return &review.ReviewRequests{}, nil
	}
	d.RequestsRemove = func(_ context.Context, _, _ string, _ int, _ auth.Principal, ps []string) (*review.ReviewRequests, error) {
		removed = ps
		return &review.ReviewRequests{}, nil
	}
	var replied string
	d.ThreadComment = func(_ context.Context, _, _ string, _ int, tid string, _ auth.Principal, body string) (*review.ThreadComment, error) {
		replied = tid + ":" + body
		return &review.ThreadComment{Seq: 2}, nil
	}
	var resolved, unresolved bool
	d.ThreadResolve = func(context.Context, string, string, int, string, auth.Principal) (*review.ThreadHeader, error) {
		resolved = true
		return &review.ThreadHeader{TID: "00000001"}, nil
	}
	d.ThreadUnresolve = func(context.Context, string, string, int, string, auth.Principal) (*review.ThreadHeader, error) {
		unresolved = true
		return &review.ThreadHeader{TID: "00000001"}, nil
	}
	d.ThreadGet = func(_ context.Context, _, _ string, _ int, _ string, _ auth.Principal, _, _ int) (*review.ThreadView, error) {
		return &review.ThreadView{Thread: &review.ThreadHeader{TID: "00000001"}, Comments: []*review.ThreadComment{
			{Seq: 1, At: "2026-11-18T09:20:00Z", By: "carol", Body: "why?"},
		}}, nil
	}
	var merged pulls.MergeInput
	d.MergeStart = func(_ context.Context, _, _ string, _ int, _ auth.Principal, in pulls.MergeInput, _ string) (*pulls.TaskRecord, error) {
		merged = in
		return &pulls.TaskRecord{ID: "t1", State: "running"}, nil
	}
	d.MergeTask = func(string, string) *pulls.TaskRecord {
		return &pulls.TaskRecord{ID: "t1", Kind: "pull-merge", State: "running", Progress: []string{"fetching"}}
	}
	var updatedBranch string
	d.UpdateBranch = func(_ context.Context, _, _ string, _ int, _ auth.Principal, sha string) (*pulls.TaskRecord, error) {
		updatedBranch = sha
		return &pulls.TaskRecord{ID: "t2", State: "running"}, nil
	}
	f.deps(d)

	if rec := f.post("/acme/widgets/pull/5/comments", url.Values{"body": {"hello"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("comment status %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/pull/5/comments", url.Values{"body": {"  "}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty comment status %d", rec.Code)
	}
	for _, path := range []string{"close", "reopen", "ready", "draft"} {
		if rec := f.post("/acme/widgets/pull/5/"+path, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s status %d", path, rec.Code)
		}
	}
	if len(updated) != 4 || updated[0].State == nil || *updated[0].State != "closed" {
		t.Fatalf("updates = %+v", updated)
	}
	if updated[2].Draft == nil || *updated[2].Draft != false {
		t.Fatalf("ready patch = %+v", updated[2])
	}
	if rec := f.post("/acme/widgets/pull/5/reviews/1/dismiss", url.Values{"reason": {"stale"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("dismiss status %d", rec.Code)
	}
	if dismissed != "stale" {
		t.Fatalf("dismiss reason %q", dismissed)
	}
	if rec := f.post("/acme/widgets/pull/5/reviewers", url.Values{"reviewers": {"carol, dave"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reviewers status %d", rec.Code)
	}
	if len(added) != 2 || added[0] != "carol" {
		t.Fatalf("added = %v", added)
	}
	if rec := f.post("/acme/widgets/pull/5/reviewers/remove", url.Values{"reviewers": {"carol"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reviewers remove status %d", rec.Code)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v", removed)
	}
	if rec := f.post("/acme/widgets/pull/5/reviewers", url.Values{"reviewers": {" "}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty reviewers status %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/pull/5/threads/00000001/comments", url.Values{"body": {"me too"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("thread reply status %d", rec.Code)
	}
	if replied != "00000001:me too" {
		t.Fatalf("replied %q", replied)
	}
	if rec := f.post("/acme/widgets/pull/5/threads/00000001/resolve", nil); rec.Code != http.StatusSeeOther || !resolved {
		t.Fatalf("resolve status %d (%v)", rec.Code, resolved)
	}
	if rec := f.post("/acme/widgets/pull/5/threads/00000001/unresolve", nil); rec.Code != http.StatusSeeOther || !unresolved {
		t.Fatalf("unresolve status %d (%v)", rec.Code, unresolved)
	}
	rec := f.get("/acme/widgets/pull/5/threads/00000001")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "why?")

	if rec := f.post("/acme/widgets/pull/5/merge", url.Values{"strategy": {"squash"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("merge status %d", rec.Code)
	}
	if merged.Strategy != pulls.StrategySquash {
		t.Fatalf("merge input = %+v", merged)
	}
	if rec := f.post("/acme/widgets/pull/5/merge", url.Values{"strategy": {"bogus"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad strategy status %d", rec.Code)
	}
	rec = f.get("/acme/widgets/pull/5/merge/task")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "merge task running", "fetching")

	if rec := f.post("/acme/widgets/pull/5/update-branch", url.Values{"expected_head_sha": {strings.Repeat("b", 40)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("update branch status %d", rec.Code)
	}
	if updatedBranch != strings.Repeat("b", 40) {
		t.Fatalf("update branch sha %q", updatedBranch)
	}
}

func TestCollabReleaseWrites(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}
	d := releaseDeps()
	var published *bool
	d.ReleasePut = func(_ context.Context, _, _ string, _ auth.Principal, _ string, in releases.ReleaseInput, _ store.Version) (*releases.Release, bool, error) {
		published = in.Draft
		return nil, false, nil
	}
	deleted := false
	d.ReleaseDelete = func(context.Context, string, string, auth.Principal, string) error {
		deleted = true
		return nil
	}
	var uploaded, assetDeleted string
	d.AssetUpload = func(_ context.Context, _, _ string, _ auth.Principal, _, name string, body io.Reader, size int64, shaHex, _ string) (*releases.AssetEntry, error) {
		b, _ := io.ReadAll(body)
		uploaded = name + "/" + string(b) + "/" + shaHex
		return &releases.AssetEntry{Name: name, Size: size}, nil
	}
	d.AssetDelete = func(_ context.Context, _, _ string, _ auth.Principal, _, name string) (*releases.AssetEntry, error) {
		assetDeleted = name
		return &releases.AssetEntry{Name: name}, nil
	}
	f.deps(d)

	if rec := f.post("/acme/widgets/releases/v1.2.3/publish", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("publish status %d", rec.Code)
	}
	if published == nil || *published != false {
		t.Fatalf("publish draft flag = %v", published)
	}
	if rec := f.post("/acme/widgets/releases/v1.2.3/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status %d", rec.Code)
	}
	if !deleted {
		t.Fatal("delete did not reach the service")
	}
	// multipart upload: the handler hashes the part and names it.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "sums.txt")
	fw, _ := mw.CreateFormFile("file", "sums.txt")
	_, _ = fw.Write([]byte("payload"))
	_ = mw.Close()
	req := f.req("POST", "/acme/widgets/releases/v1.2.3/assets", nil)
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("upload not claimed")
	}
	wantStatus(t, rec, http.StatusSeeOther)
	if !strings.HasPrefix(uploaded, "sums.txt/payload/") || len(strings.Split(uploaded, "/")[2]) != 64 {
		t.Fatalf("upload = %q", uploaded)
	}
	if rec := f.post("/acme/widgets/releases/v1.2.3/assets/sums.txt/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("asset delete status %d", rec.Code)
	}
	if assetDeleted != "sums.txt" {
		t.Fatalf("asset deleted %q", assetDeleted)
	}
}

// --- helper coverage ---------------------------------------------------------

func TestCollabPureHelpers(t *testing.T) {
	milestones := []*issues.Milestone{{ID: "000001", Title: "v1"}, {ID: "000002", Title: "v2", State: "closed"}}
	ev := func(typ string) *issues.Event { return &issues.Event{Type: typ} }

	if got := issueEventText(ev(issues.EventOpened), nil); got != "" {
		t.Fatalf("opened text %q", got)
	}
	e := ev(issues.EventTitleChanged)
	e.From, e.To = ptr("a"), ptr("b")
	if got := issueEventText(e, milestones); got != "retitled “a” → “b”" {
		t.Fatalf("title text %q", got)
	}
	e = ev(issues.EventLabelsChanged)
	e.Added, e.Removed = []string{"bug"}, []string{"wontfix", "dup"}
	if got := issueEventText(e, nil); !strings.Contains(got, "added the “bug” label") || !strings.Contains(got, "removed the “wontfix”, “dup” labels") {
		t.Fatalf("labels text %q", got)
	}
	e = ev(issues.EventAssigneesChanged)
	e.Added, e.Removed = []string{"alice"}, []string{"bob"}
	if got := issueEventText(e, nil); got != "assigned alice and unassigned bob" {
		t.Fatalf("assignees text %q", got)
	}
	if got := issueEventText(ev(issues.EventAssigneesChanged), nil); got != "changed the assignees" {
		t.Fatalf("empty assignees %q", got)
	}
	if got := issueEventText(ev(issues.EventLabelsChanged), nil); got != "changed the labels" {
		t.Fatalf("empty labels %q", got)
	}
	e = ev(issues.EventStateChanged)
	e.To = ptr("closed")
	if got := issueEventText(e, nil); got != "closed" {
		t.Fatalf("closed %q", got)
	}
	e.Reason = ptr("not_planned")
	if got := issueEventText(e, nil); got != "closed as not planned" {
		t.Fatalf("not planned %q", got)
	}
	e.To = ptr("open")
	if got := issueEventText(e, nil); got != "reopened" {
		t.Fatalf("reopen %q", got)
	}
	e = ev(issues.EventMilestoneChanged)
	e.From, e.To = ptr("000001"), ptr("000002")
	if got := issueEventText(e, milestones); got != "moved this from “v1” to “v2”" {
		t.Fatalf("milestone move %q", got)
	}
	e.From = nil
	if got := issueEventText(e, milestones); got != "added this to the “v2” milestone" {
		t.Fatalf("milestone add %q", got)
	}
	e.From, e.To = ptr("000001"), nil
	if got := issueEventText(e, milestones); got != "removed this from the “v1” milestone" {
		t.Fatalf("milestone remove %q", got)
	}
	e.From, e.To = nil, nil
	if got := issueEventText(e, milestones); got != "changed the milestone" {
		t.Fatalf("milestone none %q", got)
	}
	e.From, e.To = ptr("000001"), ptr("000002")
	if got := issueEventText(e, nil); !strings.Contains(got, "000001") {
		t.Fatalf("milestone unknown ids %q", got)
	}
	e = ev(issues.EventReferenced)
	e.Source = map[string]any{"num": float64(7)}
	if got := issueEventText(e, nil); got != "referenced this from #7" {
		t.Fatalf("referenced %q", got)
	}
	e = ev(issues.EventCrossReferenced)
	e.Source = map[string]any{"num": 7, "repo": "o/r"}
	if got := issueEventText(e, nil); got != "referenced this from o/r#7" {
		t.Fatalf("cross-referenced %q", got)
	}
	e = ev(issues.EventCrossReferenced)
	if got := issueEventText(e, nil); got != "referenced this from ?#?" {
		t.Fatalf("cross-referenced missing source %q", got)
	}
	e = ev(issues.EventClosedByPR)
	e.PRNum, e.Keyword = intPtr(9), ptr("closes")
	if got := issueEventText(e, nil); got != "closed this via #9 (closes)" {
		t.Fatalf("closed_by_pr %q", got)
	}
	e = ev(issues.EventClosedByPR)
	if got := issueEventText(e, nil); got != "closed this via #? ()" {
		t.Fatalf("closed_by_pr missing %q", got)
	}
	e = ev("")
	if got := issueEventText(e, nil); got != "event" {
		t.Fatalf("unknown %q", got)
	}
	if got := issueEventText(ev("future_kind"), nil); got != "future_kind" {
		t.Fatalf("future %q", got)
	}
	if got := closedStateLabel(nil); got != "Closed" {
		t.Fatalf("closed label %q", got)
	}

	// PR timeline text
	if got := pullEventText(&pulls.Event{Type: pulls.EventOpened}); got != "opened" {
		t.Fatalf("pr opened %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: pulls.EventCommented}); got != "" {
		t.Fatalf("pr comment %q", got)
	}
	e2 := &pulls.Event{Type: pulls.EventTitleChanged, From: ptr("a"), To: ptr("b")}
	if got := pullEventText(e2); got != "retitled “a” → “b”" {
		t.Fatalf("pr title %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: pulls.EventStateChanged, To: ptr("closed")}); got != "closed" {
		t.Fatalf("pr closed %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: pulls.EventStateChanged, To: ptr("open")}); got != "reopened" {
		t.Fatalf("pr reopen %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: pulls.EventDraftChanged, To: ptr("ready")}); got != "marked as ready for review" {
		t.Fatalf("pr ready %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: pulls.EventDraftChanged, To: ptr("draft")}); got != "converted to draft" {
		t.Fatalf("pr draft %q", got)
	}
	e2 = &pulls.Event{Type: pulls.EventMerged, MergeCommitSHA: ptr(strings.Repeat("a", 40)), Strategy: ptr("squash")}
	if got := pullEventText(e2); got != "merged as aaaaaaaaaaaa (squash)" {
		t.Fatalf("pr merged %q", got)
	}
	e2 = &pulls.Event{Type: pulls.EventHeadForcePushed, From: ptr(strings.Repeat("a", 40)), To: ptr(strings.Repeat("b", 40))}
	if got := pullEventText(e2); got != "head force-pushed aaaaaaaaaaaa → bbbbbbbbbbbb" {
		t.Fatalf("pr force push %q", got)
	}
	if got := pullEventText(&pulls.Event{Type: "future"}); got != "future" {
		t.Fatalf("pr future %q", got)
	}

	// Chips and verdicts
	if got := pullListChip(pulls.PROut{State: "closed"}); got.Text != "closed" {
		t.Fatalf("list closed %q", got.Text)
	}
	if got := pullListChip(pulls.PROut{State: "open", Draft: true}); got.Text != "draft" {
		t.Fatalf("list draft %q", got.Text)
	}
	if got := pullListChip(pulls.PROut{State: "open"}); got.Text != "open" {
		t.Fatalf("list open %q", got.Text)
	}
	if got := pullBadgeView(&pulls.Thread{State: "closed"}, &pulls.PRDoc{}); got.Text != "Closed" {
		t.Fatalf("badge closed %q", got.Text)
	}
	if got := pullBadgeView(nil, nil); got.Text != "Open" {
		t.Fatalf("badge open %q", got.Text)
	}
	for state, want := range map[string]string{
		review.StateApproved: "Approved", review.StateChangesRequested: "Changes requested",
		review.StateCommented: "Commented", review.DecisionReviewRequired: "Review required",
		review.StateDismissed: "Dismissed", "": "Unknown", "WEIRD": "WEIRD",
	} {
		if got := reviewVerdictLabel(state); got != want {
			t.Fatalf("verdict %q = %q want %q", state, got, want)
		}
		if got := reviewVerdictChip(state); got.Cls == "" {
			t.Fatalf("verdict chip %q empty class", state)
		}
	}
	for _, tc := range []struct{ state, want, cls string }{
		{checks.StateSuccess, "bg-emerald-500", ""}, {checks.StatePending, "bg-amber-400", ""},
		{checks.StateFailure, "bg-red-500", ""}, {checks.StateError, "bg-red-700", ""},
		{"other", "bg-zinc-400", ""},
	} {
		if got := checkStateDot(tc.state); !strings.Contains(got, tc.want) {
			t.Fatalf("dot %q = %q", tc.state, got)
		}
	}
	if zeroChecksTitle(nil, true) != "No checks configured" || zeroChecksTitle([]string{"a"}, true) != "No checks reported yet" || zeroChecksTitle(nil, false) != "No checks reported yet" {
		t.Fatal("zeroChecksTitle")
	}
	blockers := requiredCheckBlockers([]string{"a", "b", "c"}, []*checks.StatusDoc{{Context: "a", State: "success"}, {Context: "b", State: "failure"}})
	if strings.Join(blockers, ",") != "b (failure),c (missing)" {
		t.Fatalf("blockers = %v", blockers)
	}
	if requiredCheckBlockers(nil, nil) != nil {
		t.Fatal("no required checks must block nothing")
	}

	// merge machine + display
	pr := &pulls.PRDoc{}
	pr.Merged = true
	if got := pullMergeState(pr, nil, nil, "", nil, identity.RoleAdmin, nil); got.State != "merged" {
		t.Fatalf("merge state merged = %q", got.State)
	}
	pr = &pulls.PRDoc{Draft: true}
	if got := pullMergeState(pr, nil, nil, "", nil, identity.RoleAdmin, nil); got.State != "draft" {
		t.Fatalf("merge state draft = %q", got.State)
	}
	if got := pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableDirty}, nil, "", nil, identity.RoleAdmin, nil); got.State != "blocked" {
		t.Fatalf("merge state dirty = %q", got.State)
	}
	if got := pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableClean}, nil, "", nil, identity.RoleAdmin, nil); !got.CanMerge {
		t.Fatalf("merge state clean = %+v", got)
	}
	if got := pullMergeState(nil, nil, []string{"a"}, "", nil, identity.RoleAdmin, nil); got.State != "blocked" || !strings.Contains(got.Tooltip, "blocked") {
		t.Fatalf("merge state checks = %+v", got)
	}
	fail := &pulls.TaskRecord{State: "failed", Error: "boom"}
	if got := pullMergeState(nil, nil, nil, "", nil, identity.RoleAdmin, fail); got.State != "failed" {
		t.Fatalf("merge state failed = %q", got.State)
	}
	if got := pullMergeState(nil, nil, nil, review.DecisionChangesRequested, ptrBool(false), identity.RoleAdmin, nil); got.State != "ready" {
		t.Fatalf("changes requested without a rule must not block: %q", got.State)
	}
	if got := pullMergeState(nil, nil, nil, review.DecisionChangesRequested, nil, identity.RoleAdmin, nil); got.State != "blocked" {
		t.Fatalf("unknown rule presence must fail closed: %q", got.State)
	}
	for _, tc := range []struct {
		state, want string
		mergeable   *pulls.MergeableDoc
		draft, zero bool
		review      string
		blockers    []string
	}{
		{"merged", "Already merged", nil, false, false, "", nil},
		{"failed", "Merge failed", nil, false, false, "", nil},
		{"merging", "Merging…", nil, false, false, "", nil},
		{"draft", "Draft pull request", nil, false, false, "", nil},
		{"blocked", "Checks failing", nil, false, false, "", []string{"x"}},
		{"blocked", "Changes requested", nil, false, false, review.DecisionChangesRequested, nil},
		{"blocked", "Merge conflicts", &pulls.MergeableDoc{State: pulls.MergeableDirty}, false, false, "", nil},
		{"mergeable", "Able to be Merged", &pulls.MergeableDoc{State: pulls.MergeableClean}, false, false, "", nil},
		{"behind", "Able to be Merged", &pulls.MergeableDoc{State: pulls.MergeableBehind}, false, false, "", nil},
		{"ready", "No checks required", nil, false, true, "", nil},
		{"ready", "Checking mergeability", nil, false, false, "", nil},
		{"", "Unknown", nil, false, false, "", nil},
		{"weird", "weird", nil, false, false, "", nil},
	} {
		got := mergeabilityDisplay(tc.state, tc.mergeable, tc.blockers, tc.review, tc.draft, tc.zero)
		if got.Text != tc.want {
			t.Fatalf("mergeability(%q) = %q want %q", tc.state, got.Text, tc.want)
		}
	}

	// anchors, threads, diff placement
	files := ParseUnifiedDiff("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n")
	if a, ok := anchorFor(files, "x", "old", 1, 1, strings.Repeat("a", 40)); !ok || a.Side != review.SideOld || a.OldStart != 1 || a.NewStart != 0 {
		t.Fatalf("old anchor = %+v ok=%v", a, ok)
	}
	if _, ok := anchorFor(files, "x", "sideways", 1, 1, "a"); ok {
		t.Fatal("bad side accepted")
	}
	if _, ok := anchorFor(files, "missing", "new", 1, 1, strings.Repeat("a", 40)); ok {
		t.Fatal("unknown path accepted")
	}
	cards := []threadCard{{TID: "00000001", Path: "x", Side: review.SideNew, Start: 1, LineCount: 1, Label: "x:1"}}
	diff := buildDiff(files, cards, nil, "")
	if len(diff.Files) != 1 || len(diff.Files[0].Hunks) != 1 || len(diff.Files[0].Hunks[0].Rows) != 3 {
		t.Fatalf("diff rows = %+v", diff)
	}
	if len(diff.Files[0].Hunks[0].Rows[1].Threads) != 1 {
		t.Fatal("thread not placed under its line")
	}
	drift := buildDiff(files, []threadCard{{TID: "2", Path: "x", Side: review.SideNew, Start: 99, Label: "x:99"}}, nil, "")
	if len(drift.Drifted) != 1 {
		t.Fatal("stale anchor must land in the drifted list")
	}
	if got := diffSlug("a/b c.txt"); got != "a-b-c-txt" {
		t.Fatalf("slug = %q", got)
	}
	headers := []*review.ThreadHeader{{
		TID: "00000002", Anchor: review.Anchor{Path: "a/b.go", Side: review.SideOld, OldStart: 4, OldLines: 3},
		Resolved: true, ResolvedBy: "bob", CommentCount: 2, CreatedBy: "alice",
	}}
	hcs := threadCards(headers, "/o/r/pull/1", true)
	if hcs[0].Label != "a/b.go:4-6" || hcs[0].CommentsHref != "/o/r/pull/1/threads/00000002" || !hcs[0].CanResolve {
		t.Fatalf("thread card = %+v", hcs[0])
	}
	if got := sortedThreadIndex(hcs); len(got) != 1 {
		t.Fatal("thread index")
	}

	// labels, milestones, reactions, releases, sizes
	if got := labelChips([]string{"bug", "ghost"}, []issues.Label{{Name: "bug", Color: "d73a4a"}}); len(got) != 2 || !got[0].Known || got[1].Known {
		t.Fatalf("label chips = %+v", got)
	}
	if got := milestoneFor(nil, milestones, "o", "r"); got != nil {
		t.Fatal("nil milestone id must render nothing")
	}
	if got := milestoneFor(ptr("000002"), milestones, "o", "r"); got == nil || !got.Closed || got.Title != "v2" {
		t.Fatalf("milestone = %+v", got)
	}
	if got := milestoneFor(ptr("deadbeef"), milestones, "o", "r"); got == nil || got.Known || got.Title != "deadbeef" {
		t.Fatalf("unknown milestone = %+v", got)
	}
	if got := milestoneTitleOf(ptr("000001"), milestones); got != "v1" {
		t.Fatalf("milestone title %q", got)
	}
	if got := milestoneTitleOf(ptr("nope"), milestones); got != "nope" {
		t.Fatalf("milestone title fallback %q", got)
	}
	shown, addable := reactionRows(map[string]map[string]int{"000001": {"mystery": 1}}, 1)
	if len(shown) != 1 || shown[0].Glyph != "mystery" || len(addable) != len(reactionContents) {
		t.Fatalf("unknown reactions = %+v / %+v", shown, addable)
	}
	if _, extra := keyAssets([]releases.AssetEntry{{Name: "a"}, {Name: "b"}}, 0); extra != 2 {
		t.Fatalf("keyAssets extra = %d", extra)
	}
	if got := excerptBody("- item\n\nsecond", 140); got != "item" {
		t.Fatalf("excerpt list = %q", got)
	}
	if got := excerptBody("1. step\nnext", 140); got != "step" {
		t.Fatalf("excerpt ordered = %q", got)
	}
	if got := excerptBody("   ", 140); got != "" {
		t.Fatalf("excerpt blank = %q", got)
	}
	if got := fmtAssetSize(900); got != "900 B" {
		t.Fatalf("size small = %q", got)
	}
	if got := fmtAssetSize(2 << 30); got != "2 GiB" {
		t.Fatalf("size GiB = %q", got)
	}
	if got := fmtAssetSize(3 << 40); got != "3 TiB" {
		t.Fatalf("size TiB = %q", got)
	}

	// policy advisories
	doc := &policy.Document{Rules: []*policy.Rule{
		{Match: policy.Match{}, Effect: &policy.ProtectEffect{RequireChecks: []string{"ci/test", "ci/lint"}}},
		{Match: policy.Match{Refs: []string{"refs/heads/dev"}}, Effect: &policy.ProtectEffect{RequireChecks: []string{"ci/dev"}}},
		{Match: policy.Match{Refs: []string{"refs/heads/main"}}, Effect: &policy.RequiredReviewsEffect{MinApprovals: 1}},
	}}
	reqChecks, known := requiredChecks(doc, "refs/heads/main")
	if !known || strings.Join(reqChecks, ",") != "ci/lint,ci/test" {
		t.Fatalf("required checks = %v known=%v", reqChecks, known)
	}
	if _, known := requiredChecks(nil, "refs/heads/main"); known {
		t.Fatal("nil policy must report an unknown required set")
	}
	if !requiredReviewsApplies(doc, "refs/heads/main") || requiredReviewsApplies(doc, "refs/heads/other") || requiredReviewsApplies(nil, "refs/heads/main") {
		t.Fatal("requiredReviewsApplies")
	}

	// terminal merge detail falls back to the merged event
	sha := strings.Repeat("c", 40)
	d2 := terminalMergeDetail(nil, []*pulls.Event{{Type: pulls.EventMerged, MergeCommitSHA: &sha, Strategy: ptr("rebase"), Actor: "carol"}})
	if d2.SHA != sha || d2.Strategy != "rebase" || d2.By != "carol" {
		t.Fatalf("terminal detail = %+v", d2)
	}
	if got := short12(sha); len(got) != 12 {
		t.Fatalf("short12 = %q", got)
	}
	if got := defaultStr("", "merge"); got != "merge" {
		t.Fatalf("defaultStr = %q", got)
	}
	if got := mentionsOf([]string{"bob", "Alice", "alice", " "}); len(got) != 2 || got[0] != "Alice" {
		t.Fatalf("mentions = %v", got)
	}

	// gates / gates helpers
	if sameOrigin(&http.Request{Host: "h", Header: http.Header{"Referer": {"http://h/path"}}}) != true {
		t.Fatal("same-origin referer")
	}
	if sameOrigin(&http.Request{Host: "h", Header: http.Header{"Origin": {"http://other"}}}) {
		t.Fatal("cross-origin origin")
	}
	if sameOrigin(&http.Request{Host: "h", Header: http.Header{"Origin": {"::bogus"}}}) {
		t.Fatal("unparseable origin")
	}
	if sameOrigin(&http.Request{Host: "h", Header: http.Header{"Referer": {"::bogus"}}}) {
		t.Fatal("unparseable referer")
	}
	if got := collabErrStatus(errors.New("other")); got != http.StatusServiceUnavailable {
		t.Fatalf("generic error status %d", got)
	}
	if got := collabErrStatus(issues.ErrLocked); got != http.StatusConflict {
		t.Fatalf("locked status %d", got)
	}
	if got := collabErrStatus(pulls.ErrNotFound); got != http.StatusNotFound {
		t.Fatalf("notfound status %d", got)
	}
	if got := collabErrStatus(checks.ErrUnprocessable); got != http.StatusUnprocessableEntity {
		t.Fatalf("unprocessable status %d", got)
	}
	if got := collabErrStatus(releases.ErrForbidden); got != http.StatusForbidden {
		t.Fatalf("forbidden status %d", got)
	}
	if got := collabErrStatus(review.ErrUnauthorized); got != http.StatusUnauthorized {
		t.Fatalf("unauthorized status %d", got)
	}
	if got := collabErrStatus(review.ErrConflict); got != http.StatusConflict {
		t.Fatalf("conflict status %d", got)
	}
	if got := collabErrStatus(releases.ErrInvalid); got != http.StatusBadRequest {
		t.Fatalf("invalid status %d", got)
	}
	if got := safeNext("//evil", "/safe"); got != "/safe" {
		t.Fatalf("open redirect allowed: %q", got)
	}
	if got := safeNext("/ok", "/safe"); got != "/ok" {
		t.Fatalf("safeNext = %q", got)
	}
	if got := afterParam(httptest.NewRequest("GET", "/?after=x", nil)); got != 0 {
		t.Fatalf("afterParam invalid = %d", got)
	}
	if got := forkDate("2026-11-17T00:00:00Z"); got != "2026-11-17" {
		t.Fatalf("forkDate = %q", got)
	}
	if !fullSHA(strings.Repeat("a", 64)) || fullSHA(strings.Repeat("a", 41)) {
		t.Fatal("fullSHA sha length")
	}
	if _, ok := issueNum("99999999999"); ok {
		t.Fatal("issueNum too long")
	}
	if _, ok := issueNum("abc"); ok {
		t.Fatal("issueNum alpha")
	}
}

func ptrBool(b bool) *bool { return &b }

func mentionsOf(names []string) []string { return mentionNames(names) }

// TestCollabFragmentsAndFallbacks covers the fragment routes and the
// registry-only / service-down fallbacks.
func TestCollabFragmentsAndFallbacks(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true, Admin: true}
	d := pullDeps()
	cd := checkDeps()
	d.ChecksList, d.ChecksStatuses, d.ChecksCombined = cd.ChecksList, cd.ChecksStatuses, cd.ChecksCombined
	f.deps(d)

	rec := f.get("/acme/widgets/checks/" + strings.Repeat("d", 40) + "/contexts")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "ci/test")

	// Missing-page fragment is a 500 with a plain error line (no recursion).
	rec = httptest.NewRecorder()
	f.srv.collabFragment(rec, f.req("GET", "/x", nil), "collab/missing", "collab_check_contexts", nil)
	wantStatus(t, rec, http.StatusInternalServerError)

	// Repo view outage is not "missing": the page still answers with the
	// inline error from the service read.
	f.view.err = errors.New("view down")
	rec = f.get("/acme/widgets/pulls")
	wantStatus(t, rec, http.StatusOK)
	f.view.err = nil

	// Registry-only environments (Repo nil) use Repos.Exists.
	f.srv.env.API.Repo = nil
	rec = f.get("/acme/widgets/forks")
	wantStatus(t, rec, http.StatusOK)
	rec = f.get("/acme/unknown/forks")
	wantStatus(t, rec, http.StatusNotFound)
	f.srv.env.API.Repo = f.view

	// text/plain error twins (the API convention) bypass HTML pages.
	req := f.req("GET", "/acme/widgets/issues/0", nil)
	req.Header.Set("Accept", "text/plain")
	rec = httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("404 not claimed")
	}
	wantStatus(t, rec, http.StatusNotFound)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
}

// TestCollabPagersAndBranches covers the pager wiring (OlderHref is computed
// only when a window reports `more`) and the remaining small branches.
func TestCollabPagersAndBranches(t *testing.T) {
	BindCollab(CollabDeps{})
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	d := issueDeps()
	d.IssuesList = func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
		return &issues.ListResult{Issues: []issues.Card{{Num: 9, Title: "nine"}, {Num: 8, Title: "eight"}}, More: true}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/issues?state=open"), "after=8", "Older")

	pd := pullDeps()
	pd.PullsList = func(context.Context, string, string, auth.Principal, pulls.ListFilter) (*pulls.ListResult, error) {
		return &pulls.ListResult{Pulls: []pulls.PROut{{Num: 7, Title: "seven"}}, More: true}, nil
	}
	pd.PullCommits = func(context.Context, string, string, int, auth.Principal, int, int) ([]pulls.CommitEntry, bool, error) {
		return []pulls.CommitEntry{{SHA: strings.Repeat("a", 40), Subject: "s"}}, true, nil
	}
	pd.ForksList = func(context.Context, string, string, auth.Principal, string, int) (*pulls.ForkListPage, error) {
		return &pulls.ForkListPage{Forks: []pulls.ForkListEntry{{Repo: "bob/w"}}, More: true}, nil
	}
	f.deps(pd)
	wantContains(t, f.get("/acme/widgets/pulls"), "after=7", "Older")
	wantContains(t, f.get("/acme/widgets/pull/5/commits"), "skip=1", "Older")
	wantContains(t, f.get("/acme/widgets/forks"), "after=bob%2Fw", "Older")

	cdep := checkDeps()
	cdep.ChecksList = func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
		return &checks.ChecksPage{Checks: []checks.IndexSHA{{SHA: strings.Repeat("d", 40), State: "success"}}, More: true}, nil
	}
	f.deps(cdep)
	wantContains(t, f.get("/acme/widgets/checks"), "after="+strings.Repeat("d", 40), "Older")

	rd := releaseDeps()
	rd.ReleasesList = func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
		return []*releases.Release{{Tag: "v9", Name: "v9", CreatedAt: "2026-11-01T00:00:00Z"}}, true, nil
	}
	f.deps(rd)
	wantContains(t, f.get("/acme/widgets/releases"), "after=2026-11-01T00%3A00%3A00Z%7Cv9", "Older")

	// Filtered checks windows say so, and the error branches answer 503.
	cdep = checkDeps()
	f.deps(cdep)
	wantContains(t, f.get("/acme/widgets/checks?state=pending"), "filter applies to the loaded page")
	cdep = checkDeps()
	cdep.ChecksList = func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
		return nil, errors.New("checks down")
	}
	f.deps(cdep)
	wantStatus(t, f.get("/acme/widgets/checks"), http.StatusServiceUnavailable)

	d = issueDeps()
	d.IssuesList = func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
		return nil, errors.New("issues down")
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/issues"), http.StatusServiceUnavailable)

	// HX-Request redirect uses HX-Redirect, not a 303.
	d = issueDeps()
	d.IssueComment = func(context.Context, string, string, int, auth.Principal, string) (*issues.Event, error) {
		return &issues.Event{Seq: 2}, nil
	}
	f.deps(d)
	req := f.req("POST", "/acme/widgets/issues/3/comments", url.Values{"body": {"x"}})
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("post not claimed")
	}
	wantStatus(t, rec, http.StatusNoContent)
	if rec.Header().Get("HX-Redirect") != "/acme/widgets/issues/3#event-2" {
		t.Fatalf("HX-Redirect = %q", rec.Header().Get("HX-Redirect"))
	}

	// Small pure branches.
	if got := pullCommentLock(&pulls.Thread{State: "closed"}, nil); !got.Locked || got.Reason != "This conversation is closed" {
		t.Fatalf("pull lock = %+v", got)
	}
	if got := pullCommentLock(nil, &pulls.PRDoc{Merged: true}); !got.Locked || got.Reason != "Merged — commenting is locked" {
		t.Fatalf("merged lock = %+v", got)
	}
	if got := issueCommentLock(nil); got.Locked {
		t.Fatal("nil thread must read unlocked")
	}
	if got := pullCloseVisibility(nil, nil, auth.Anonymous(), ""); got[0] || got[1] {
		t.Fatal("anonymous close visibility")
	}
	if got := pullCloseVisibility(&pulls.Thread{State: "open", Author: "alice"}, nil, auth.Principal{Name: "alice"}, identity.RoleRead); !got[0] {
		t.Fatal("author close visibility")
	}
	if got := pullDraftVisibility(&pulls.Thread{State: "open", Author: "alice"}, &pulls.PRDoc{Draft: true}, auth.Principal{Name: "alice"}, identity.RoleRead); !got[0] {
		t.Fatal("draft visibility")
	}
	if got := pullDraftVisibility(&pulls.Thread{State: "open", Author: "alice"}, &pulls.PRDoc{}, auth.Principal{Name: "alice"}, identity.RoleRead); !got[1] {
		t.Fatal("ready visibility")
	}
	if got := pullDraftVisibility(&pulls.Thread{}, &pulls.PRDoc{Merged: true}, auth.Principal{Name: "alice"}, identity.RoleAdmin); got[0] || got[1] {
		t.Fatal("merged draft visibility")
	}
	if got := checkPillFor("o", "r", "sha", checks.StateSuccess, true); got.Label != "no checks" || got.Title != "checks: no contexts reported" {
		t.Fatalf("zero pill = %+v", got)
	}
	if got := checkPillFor("o", "r", "sha", checks.StateFailure, false); got.Label != "failure" || got.Href != "/o/r/checks/sha" {
		t.Fatalf("pill = %+v", got)
	}
	if got := sortedThreadIndex([]threadCard{{TID: "b", Label: "z:2"}, {TID: "a", Label: "a:1", Resolved: true}}); got[0].TID != "b" {
		t.Fatalf("index order = %+v", got)
	}
	if got := sourceNum(map[string]any{"num": "12"}); got != "12" {
		t.Fatalf("sourceNum string = %q", got)
	}
	if got := short12("abc"); got != "abc" {
		t.Fatalf("short12 short = %q", got)
	}
	if allDigits("") {
		t.Fatal("allDigits empty")
	}
	if got := forkDate("2026"); got != "2026" {
		t.Fatalf("forkDate short = %q", got)
	}
	if got := splitLabels(url.Values{"labels": {"a, b", "c"}}); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("splitLabels = %v", got)
	}
	if got := mentionNames(nil); got != nil {
		t.Fatalf("mentionNames nil = %v", got)
	}
	// loadLabels / loadMilestones degrade to nil on service errors.
	f.deps(CollabDeps{
		Labels:     func(context.Context, string, string) ([]issues.Label, error) { return nil, errors.New("down") },
		Milestones: func(context.Context, string, string) ([]*issues.Milestone, error) { return nil, errors.New("down") },
	})
	if got := f.srv.loadLabels(context.Background(), collabRepo{Owner: "a", Name: "b"}); got != nil {
		t.Fatalf("labels on error = %v", got)
	}
	if got := f.srv.loadMilestones(context.Background(), collabRepo{Owner: "a", Name: "b"}); got != nil {
		t.Fatalf("milestones on error = %v", got)
	}
}

// TestCollabWriteErrorBranches drives the service-error branch of each write
// handler: the error must surface inline with the mapped status (never a
// silent success, never a toast).
func TestCollabWriteErrorBranches(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	boom := func(err error) func() error { return func() error { return err } }
	_ = boom

	// issues: unreact
	d := issueDeps()
	d.IssueUnreact = func(context.Context, string, string, int, int, auth.Principal, string) (*issues.Thread, error) {
		return nil, issues.ErrNotFound
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/reactions/1/%2B1/delete", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unreact error status %d", rec.Code)
	}
	// issues: label patch error, milestone patch error
	d = issueDeps()
	d.IssuePatch = func(context.Context, string, string, int, auth.Principal, issues.IssuePatch) (*issues.Thread, error) {
		return testThread(), nil
	}
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return nil, issues.ErrNotFound
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("label get error status %d", rec.Code)
	}
	d = issueDeps()
	d.IssuePatch = func(context.Context, string, string, int, auth.Principal, issues.IssuePatch) (*issues.Thread, error) {
		return nil, issues.ErrForbidden
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/milestone", url.Values{"milestone": {"x"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("milestone error status %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/close", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("close error status %d", rec.Code)
	}
	// The composite comment fails first.
	d.IssueComment = func(context.Context, string, string, int, auth.Principal, string) (*issues.Event, error) {
		return nil, issues.ErrLocked
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/close", url.Values{"body": {"x"}}); rec.Code != http.StatusConflict {
		t.Fatalf("comment+close error status %d", rec.Code)
	}

	// pulls: every write error surfaces
	pd := pullDeps()
	pd.PullComment = func(context.Context, string, string, int, auth.Principal, string) (*pulls.Event, error) {
		return nil, pulls.ErrLocked
	}
	pd.PullUpdate = func(context.Context, string, string, int, auth.Principal, pulls.PRPatch) (*pulls.Thread, *pulls.PRDoc, error) {
		return nil, nil, pulls.ErrConflict
	}
	pd.ReviewDismiss = func(context.Context, string, string, int, int, auth.Principal, string) (*review.ReviewEvent, *review.ReviewSummary, error) {
		return nil, nil, review.ErrForbidden
	}
	pd.RequestsAdd = func(context.Context, string, string, int, auth.Principal, []string) (*review.ReviewRequests, error) {
		return nil, review.ErrUnprocessable
	}
	pd.RequestsRemove = func(context.Context, string, string, int, auth.Principal, []string) (*review.ReviewRequests, error) {
		return nil, review.ErrUnprocessable
	}
	pd.ThreadComment = func(context.Context, string, string, int, string, auth.Principal, string) (*review.ThreadComment, error) {
		return nil, review.ErrLocked
	}
	pd.ThreadResolve = func(context.Context, string, string, int, string, auth.Principal) (*review.ThreadHeader, error) {
		return nil, review.ErrNotFound
	}
	pd.MergeStart = func(context.Context, string, string, int, auth.Principal, pulls.MergeInput, string) (*pulls.TaskRecord, error) {
		return nil, pulls.ErrUnprocessable
	}
	pd.UpdateBranch = func(context.Context, string, string, int, auth.Principal, string) (*pulls.TaskRecord, error) {
		return nil, pulls.ErrConflict
	}
	f.deps(pd)
	for _, tc := range []struct {
		path string
		form url.Values
		want int
	}{
		{"/acme/widgets/pull/5/comments", url.Values{"body": {"x"}}, http.StatusConflict},
		{"/acme/widgets/pull/5/close", nil, http.StatusConflict},
		{"/acme/widgets/pull/5/ready", nil, http.StatusConflict},
		{"/acme/widgets/pull/5/reviews/1/dismiss", url.Values{"reason": {"x"}}, http.StatusForbidden},
		{"/acme/widgets/pull/5/reviewers", url.Values{"reviewers": {"x"}}, http.StatusUnprocessableEntity},
		{"/acme/widgets/pull/5/threads/00000001/comments", url.Values{"body": {"x"}}, http.StatusConflict},
		{"/acme/widgets/pull/5/threads/00000001/resolve", nil, http.StatusNotFound},
		{"/acme/widgets/pull/5/merge", url.Values{"strategy": {"merge"}}, http.StatusUnprocessableEntity},
		{"/acme/widgets/pull/5/update-branch", nil, http.StatusConflict},
	} {
		rec := f.post(tc.path, tc.form)
		if rec.Code != tc.want {
			t.Fatalf("%s status = %d want %d; body %.400s", tc.path, rec.Code, tc.want, bodyText(rec))
		}
	}
	// reviews: the service error re-renders the page (the sha is resolved by
	// PullGet, so this also covers the "head from the view" branch).
	pd = pullDeps()
	pd.ReviewSubmit = func(context.Context, string, string, int, auth.Principal, review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error) {
		return nil, nil, nil, review.ErrUnprocessable
	}
	f.deps(pd)
	if rec := f.post("/acme/widgets/pull/5/reviews", url.Values{"verdict": {"APPROVED"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("review error status %d", rec.Code)
	}
	// PullGet failure while resolving the head.
	pd = pullDeps()
	pd.ReviewSubmit = func(context.Context, string, string, int, auth.Principal, review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error) {
		return nil, nil, nil, nil
	}
	pd.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return nil, pulls.ErrNotFound
	}
	f.deps(pd)
	if rec := f.post("/acme/widgets/pull/5/reviews", url.Values{"verdict": {"APPROVED"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("review get error status %d", rec.Code)
	}
	// PullGet failure on the comments/commits/files read pages.
	pd = pullDeps()
	pd.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return nil, pulls.ErrNotFound
	}
	f.deps(pd)
	if rec := f.get("/acme/widgets/pull/5/commits"); rec.Code != http.StatusNotFound {
		t.Fatalf("commits 404 = %d", rec.Code)
	}
	if rec := f.get("/acme/widgets/pull/5/files"); rec.Code != http.StatusNotFound {
		t.Fatalf("files 404 = %d", rec.Code)
	}
	if rec := f.get("/acme/widgets/pull/5"); rec.Code != http.StatusNotFound {
		t.Fatalf("pull 404 = %d", rec.Code)
	}
	// Diff failure renders the honest alert with Retry.
	pd = pullDeps()
	pd.PullDiff = func(context.Context, string, string, int, auth.Principal) (string, error) {
		return "", errors.New("git exploded")
	}
	f.deps(pd)
	rec := f.get("/acme/widgets/pull/5/files")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "git exploded", "Retry")
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "git exploded")

	// Files: a valid selection renders the composer; a locked PR says so.
	pd = pullDeps()
	pd.PullGet = func(_ context.Context, _, _ string, _ int, _ auth.Principal, _, _ int) (*pulls.PullView, error) {
		th := pullThread()
		th.State = "closed"
		return &pulls.PullView{Thread: th, PR: &pulls.PRDoc{}, HeadLive: strings.Repeat("b", 40)}, nil
	}
	f.deps(pd)
	rec = f.get("/acme/widgets/pull/5/files?path=x.txt&side=new&line=1")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "commenting is locked")
	// Stale selection on an open PR re-renders the error line.
	pd = pullDeps()
	f.deps(pd)
	rec = f.get("/acme/widgets/pull/5/files?path=x.txt&side=bogus&line=1")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "names no line in this diff")
	rec = f.get("/acme/widgets/pull/5/files?path=nope&side=new&line=1")
	wantContains(t, rec, "names no line in this diff")
	rec = f.get("/acme/widgets/pull/5/files?path=x.txt&side=new&line=1")
	wantContains(t, rec, "commenting on", "Start thread")

	// checks: detail + fragments error branches
	cd := checkDeps()
	cd.ChecksCombined = func(context.Context, string, string, string, auth.Principal) (*checks.CombinedView, error) {
		return nil, errors.New("checks down")
	}
	f.deps(cd)
	if rec := f.get("/acme/widgets/checks/" + strings.Repeat("d", 40)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("check detail error = %d", rec.Code)
	}
	cd = checkDeps()
	cd.ChecksStatuses = func(context.Context, string, string, string, auth.Principal) (*checks.StatusesView, error) {
		return nil, checks.ErrNotFound
	}
	f.deps(cd)
	if rec := f.get("/acme/widgets/checks/" + strings.Repeat("d", 40) + "/contexts"); rec.Code != http.StatusNotFound {
		t.Fatalf("contexts error = %d", rec.Code)
	}
	// A checks row whose expand fetch fails renders the row without contexts.
	cd = checkDeps()
	cd.ChecksStatuses = func(context.Context, string, string, string, auth.Principal) (*checks.StatusesView, error) {
		return nil, errors.New("down")
	}
	f.deps(cd)
	if rec := f.get("/acme/widgets/checks?expanded=" + strings.Repeat("d", 40)); rec.Code != http.StatusOK {
		t.Fatalf("expanded error = %d", rec.Code)
	}

	// releases: publish/delete/upload/asset-delete error branches
	rd := releaseDeps()
	rd.ReleasePut = func(context.Context, string, string, auth.Principal, string, releases.ReleaseInput, store.Version) (*releases.Release, bool, error) {
		return nil, false, releases.ErrInvalid
	}
	rd.ReleaseDelete = func(context.Context, string, string, auth.Principal, string) error {
		return releases.ErrForbidden
	}
	rd.AssetUpload = func(context.Context, string, string, auth.Principal, string, string, io.Reader, int64, string, string) (*releases.AssetEntry, error) {
		return nil, releases.ErrConflict
	}
	rd.AssetDelete = func(context.Context, string, string, auth.Principal, string, string) (*releases.AssetEntry, error) {
		return nil, releases.ErrNotFound
	}
	f.deps(rd)
	if rec := f.post("/acme/widgets/releases/v1.2.3/publish", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("publish error = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/releases/v1.2.3/delete", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("delete error = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/releases/v1.2.3/assets/sums.txt/delete", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("asset delete error = %d", rec.Code)
	}
	// upload: not multipart
	if rec := f.post("/acme/widgets/releases/v1.2.3/assets", url.Values{"name": {"x"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("upload form error = %d", rec.Code)
	}
	// upload: multipart without a file
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "x")
	_ = mw.Close()
	req := f.req("POST", "/acme/widgets/releases/v1.2.3/assets", nil)
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	if !f.srv.Serve(rec, req) {
		t.Fatal("upload not claimed")
	}
	wantStatus(t, rec, http.StatusBadRequest)

	// forks: service error
	fd := pullDeps()
	fd.ForksList = func(context.Context, string, string, auth.Principal, string, int) (*pulls.ForkListPage, error) {
		return nil, pulls.ErrForbidden
	}
	f.deps(fd)
	if rec := f.get("/acme/widgets/forks"); rec.Code != http.StatusForbidden {
		t.Fatalf("forks error = %d", rec.Code)
	}

	// Failing PR comment write-back with a body-bearing error page.
	pd = pullDeps()
	pd.PullComment = func(context.Context, string, string, int, auth.Principal, string) (*pulls.Event, error) {
		return nil, errors.New("down")
	}
	f.deps(pd)
	if rec := f.post("/acme/widgets/pull/5/comments", url.Values{"body": {"x"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("comment 503 = %d", rec.Code)
	}
}

// TestCollabNotWired pins the fail-closed "service not wired" answer for
// every page family, plus the per-action role floors on the write endpoints
// that require more than read.
func TestCollabNotWired(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice"} // read-only
	f.deps(CollabDeps{})
	for _, path := range []string{
		"/acme/widgets/issues", "/acme/widgets/issues/3",
		"/acme/widgets/pulls", "/acme/widgets/pull/5",
		"/acme/widgets/pull/5/commits", "/acme/widgets/pull/5/files",
		"/acme/widgets/checks", "/acme/widgets/checks/" + strings.Repeat("a", 40),
		"/acme/widgets/releases", "/acme/widgets/releases/v1",
		"/acme/widgets/forks",
	} {
		rec := f.get(path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503; body %.300s", path, rec.Code, bodyText(rec))
		}
	}
	// Write endpoints likewise refuse rather than silently no-op.
	for _, path := range []string{
		"/acme/widgets/issues/3/comments", "/acme/widgets/issues/3/close",
		"/acme/widgets/issues/3/labels", "/acme/widgets/issues/3/milestone",
		"/acme/widgets/issues/3/reactions/1", "/acme/widgets/issues/3/reactions/1/x/delete",
		"/acme/widgets/pull/5/comments", "/acme/widgets/pull/5/merge",
		"/acme/widgets/pull/5/reviews", "/acme/widgets/pull/5/reviews/1/dismiss",
		"/acme/widgets/pull/5/reviewers", "/acme/widgets/pull/5/threads",
		"/acme/widgets/pull/5/threads/00000001/comments",
		"/acme/widgets/pull/5/update-branch",
		"/acme/widgets/releases/v1/save", "/acme/widgets/releases/v1/publish",
		"/acme/widgets/releases/v1/delete", "/acme/widgets/releases/v1/assets/x/delete",
	} {
		rec := f.post(path, url.Values{"body": {"x"}, "strategy": {"merge"}, "verdict": {"COMMENTED"}, "reason": {"completed"}, "label": {"l"}, "milestone": {"m"}, "reviewers": {"r"}, "content": {"+1"}})
		// Labels/milestone need triage, reviews need read+auth, merge/dismiss
		// need maintain, release writes need write: all are 403 for a plain
		// reader, except the read-gated write surfaces which reach the
		// not-wired guard (503).
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d; body %.300s", path, rec.Code, bodyText(rec))
		}
	}
	// Bad event seq / number parsing is 404 before any service call.
	if rec := f.post("/acme/widgets/issues/3/reactions/0", url.Values{"content": {"+1"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("bad reaction seq = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/reactions/0/x/delete", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad unreact seq = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/0/comments", url.Values{"body": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("bad issue num = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/pull/0/comments", url.Values{"body": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("bad pull num = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/pull/5/reviews/0/dismiss", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad review seq = %d", rec.Code)
	}
}
