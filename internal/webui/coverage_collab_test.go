package webui

// coverage_collab_test.go — the collaboration family's remaining branches:
// the gate/role-floor edges, the read pages' service-outage and empty-state
// branches, every write handler's not-wired and invalid-input guards, the
// review/merge/policy render inputs, and the diff/anchor helpers.
//
// The happy paths and the inline-error writes live in collab_test.go; these
// cases exist so a regression in a guard cannot pass unnoticed.

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/checks"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/identity"
	"github.com/crueber/walhub/internal/issues"
	"github.com/crueber/walhub/internal/policy"
	"github.com/crueber/walhub/internal/pulls"
	"github.com/crueber/walhub/internal/releases"
	"github.com/crueber/walhub/internal/review"
	"github.com/crueber/walhub/internal/server/auth"
	"github.com/crueber/walhub/internal/store"
)

// postH sends a form POST with extra headers (Origin overrides included).
func (f *collabFixture) postH(path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := f.req("POST", path, form)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, r) {
		f.t.Fatalf("POST %s was not claimed by the webui mount", path)
	}
	return rec
}

// rawPostH sends a body verbatim (multipart and malformed forms).
func (f *collabFixture) rawPostH(path, contentType string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := f.req("POST", path, nil)
	r.Body = http.NoBody
	r = httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Host = testHost
	r.Header.Set("Origin", "http://"+testHost)
	r.Header.Set("Content-Type", contentType)
	r.ContentLength = int64(len(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, r) {
		f.t.Fatalf("POST %s was not claimed by the webui mount", path)
	}
	return rec
}

func TestCoverageCollabPureBranches(t *testing.T) {
	// Repo existence degrades to "missing" without any registry seam.
	if (&Server{env: Env{}}).collabRepoExists(context.Background(), "a", "b", nil) {
		t.Fatal("no API must mean no repo")
	}
	if (&Server{env: Env{API: &api.Env{}}}).collabRepoExists(context.Background(), "a", "b", nil) {
		t.Fatal("no registry/view must mean no repo")
	}

	f := newCollabFixture(t)

	// The identity seam wins over the flag fallback.
	f.deps(CollabDeps{RoleOf: func(context.Context, string, string, auth.Principal) identity.Role {
		return identity.RoleMaintain
	}})
	if got := f.srv.collabRole(context.Background(), "acme", "widgets", auth.Principal{Name: "alice"}); got != identity.RoleMaintain {
		t.Fatalf("collabRole seam = %q", got)
	}

	// Only token/oidc gate anonymous writes.
	f.srv.env.Cfg.Server.Auth.Mode = ""
	if f.srv.anonWrite(auth.Anonymous()) {
		t.Fatal("mode none must not gate anonymous writes")
	}

	if _, ok := issueNum("16777216"); ok {
		t.Fatal("issue numbers above 0xFFFFFF must not parse")
	}
	if n, ok := issueNum("16777215"); !ok || n != 16777215 {
		t.Fatalf("issueNum boundary = %d %v", n, ok)
	}
	if fullSHA(strings.Repeat("A", 40)) {
		t.Fatal("uppercase sha must not be accepted")
	}
	if got := afterParam(httptest.NewRequest("GET", "/?after=5", nil)); got != 5 {
		t.Fatalf("afterParam(5) = %d", got)
	}
	if got := milestoneTitleOf(nil, nil); got != "" {
		t.Fatalf("milestoneTitleOf(nil) = %q", got)
	}
	if got := sourceNum(map[string]any{"num": []int{1}}); got != "?" {
		t.Fatalf("sourceNum(unknown) = %q", got)
	}
	if got := sourceRepo(nil); got != "?" {
		t.Fatalf("sourceRepo(nil) = %q", got)
	}
	if got := sourceRepo(map[string]any{"repo": 3}); got != "?" {
		t.Fatalf("sourceRepo(non-string) = %q", got)
	}
	if got := closedStateLabel(ptr("completed")); got != "Closed as completed" {
		t.Fatalf("closedStateLabel(completed) = %q", got)
	}
	if got := issueCommentLock(&issues.Thread{State: "closed"}); !got.Locked {
		t.Fatal("a closed thread must lock comments")
	}
	if got := pullBadgeView(nil, &pulls.PRDoc{Merged: true}); got.Text != "Merged" {
		t.Fatalf("merged badge = %+v", got)
	}
	if got := excerptBody(strings.Repeat("a", 200), 0); len([]rune(got)) != 140 || !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt default width = %d chars", len([]rune(got)))
	}
	if allDigits("12a") {
		t.Fatal("allDigits must reject a non-digit")
	}
	names := make([]string, 60)
	for i := range names {
		names[i] = "user" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	if got := mentionNames(names); len(got) != 50 {
		t.Fatalf("mentionNames cap = %d", len(got))
	}
	if got := sortedThreadIndex([]threadCard{{TID: "b", Label: "z:2"}, {TID: "a", Label: "a:1"}}); got[0].TID != "a" {
		t.Fatalf("thread index tie-break = %+v", got)
	}
}

func TestCoverageCollabGateAndWriteGuards(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "bob"}
	f.readGate = func(string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no read"}
	}
	rec := httptest.NewRecorder()
	req := f.req("GET", "/acme/widgets/issues", nil)
	req.Header.Set("Accept", "text/plain")
	if !f.srv.Serve(rec, req) {
		t.Fatal("issues not claimed")
	}
	wantStatus(t, rec, http.StatusForbidden)

	// The same denial for an anonymous viewer is a 401 (it can authenticate).
	f.principal = auth.Anonymous()
	rec = httptest.NewRecorder()
	req = f.req("GET", "/acme/widgets/issues", nil)
	req.Header.Set("Accept", "text/plain")
	f.srv.Serve(rec, req)
	wantStatus(t, rec, http.StatusUnauthorized)

	// A principal failure is surfaced with the auth status.
	f.readGate = nil
	f.srv.auth.Authenticate = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "revoked"}
	}
	rec = httptest.NewRecorder()
	req = f.req("GET", "/acme/widgets/issues", nil)
	req.Header.Set("Accept", "text/plain")
	f.srv.Serve(rec, req)
	wantStatus(t, rec, http.StatusForbidden)

	// --- write gate edges -----------------------------------------------------

	// A POST to an unknown repo fails in collabEnv (the write never starts).
	f2 := newCollabFixture(t)
	f2.principal = auth.Principal{Name: "alice", Admin: true}
	rec = httptest.NewRecorder()
	req = f2.req("POST", "/acme/unknown/issues/3/close", nil)
	req.Header.Set("Accept", "text/plain")
	f2.srv.Serve(rec, req)
	wantStatus(t, rec, http.StatusNotFound)

	// With no auth mode and no role, a floor-gated write is a 401 (the
	// caller is a stranger, not an anonymous-mode login).
	f2.srv.env.Cfg.Server.Auth.Mode = ""
	f2.principal = auth.Anonymous()
	rec = httptest.NewRecorder()
	req = f2.req("POST", "/acme/widgets/issues/3/labels", url.Values{"label": {"x"}})
	req.Header.Set("Accept", "text/plain")
	f2.srv.Serve(rec, req)
	wantStatus(t, rec, http.StatusUnauthorized)
}

func TestCoverageCollabIssueReadBranches(t *testing.T) {
	f := newCollabFixture(t)

	// A generic thread-read outage re-renders the page with the error line.
	d := issueDeps()
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return nil, errors.New("issues exploded")
	}
	f.deps(d)
	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "issues exploded")

	// A vanished thread is the family error page (never a blank body).
	d = issueDeps()
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return nil, issues.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/issues/3"), http.StatusNotFound)

	// A closed thread renders the completed reason line.
	d = issueDeps()
	th := testThread()
	th.State, th.StateReason = "closed", ptr(issues.ReasonCompleted)
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return &issues.ThreadView{Thread: th}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/issues/3"), "Closed as completed")

	// Unknown repo on a thread page is a 404 before the service read.
	wantStatus(t, f.get("/acme/unknown/issues/3"), http.StatusNotFound)

	// Label/milestone option reads are advisory: an outage is a notice.
	d = issueDeps()
	d.Labels = func(context.Context, string, string) ([]issues.Label, error) {
		return nil, errors.New("labels down")
	}
	f.deps(d)
	rec = f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "labels and milestones unavailable")

	// A milestone outage with no label read is the same notice (first error
	// wins), and the milestone filter falls back to "unknown".
	d = issueDeps()
	d.Labels = nil
	d.Milestones = func(context.Context, string, string) ([]*issues.Milestone, error) {
		return nil, errors.New("milestones down")
	}
	f.deps(d)
	rec = f.get("/acme/widgets/issues?milestone=000001&state=open")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "labels and milestones unavailable")

	// A vanished filter is flagged when the option set is empty.
	d = issueDeps()
	d.Milestones = func(context.Context, string, string) ([]*issues.Milestone, error) { return nil, nil }
	f.deps(d)
	rec = f.get("/acme/widgets/issues?milestone=zzz&state=open")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "milestone")

	// A known milestone filter selects the option row.
	d = issueDeps()
	f.deps(d)
	rec = f.get("/acme/widgets/issues?milestone=000001&state=open")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "v1")

	// The list's 404 is the error page, not an empty list.
	d = issueDeps()
	d.IssuesList = func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
		return nil, issues.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/issues"), http.StatusNotFound)

	// Nil loaders degrade to no options (never a panic).
	BindCollab(CollabDeps{})
	if got := f.srv.loadLabels(context.Background(), collabRepo{Owner: "a", Name: "b"}); got != nil {
		t.Fatalf("unwired labels = %v", got)
	}
	if got := f.srv.loadMilestones(context.Background(), collabRepo{Owner: "a", Name: "b"}); got != nil {
		t.Fatalf("unwired milestones = %v", got)
	}

	// Invalid issue numbers on every write are 404s (the service never runs).
	f.principal = auth.Principal{Name: "alice", Admin: true}
	f.deps(issueDeps())
	for _, path := range []string{
		"/acme/widgets/issues/0/close", "/acme/widgets/issues/0/reopen",
		"/acme/widgets/issues/0/labels", "/acme/widgets/issues/0/milestone",
		"/acme/widgets/issues/0/reactions/1", "/acme/widgets/issues/0/reactions/1/x/delete",
	} {
		wantStatus(t, f.post(path, url.Values{"label": {"x"}, "milestone": {"m"}, "content": {"+1"}}), http.StatusNotFound)
	}

	// A label patch failure re-renders the thread inline.
	d = issueDeps()
	d.IssuePatch = func(context.Context, string, string, int, auth.Principal, issues.IssuePatch) (*issues.Thread, error) {
		return nil, issues.ErrInvalid
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"needs-test"}})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, "role=\"alert\"")

	// Not-wired label/milestone writes are 503s for a privileged caller.
	f.deps(CollabDeps{})
	wantStatus(t, f.post("/acme/widgets/issues/3/labels", url.Values{"label": {"x"}}), http.StatusServiceUnavailable)
	wantStatus(t, f.post("/acme/widgets/issues/3/milestone", url.Values{"milestone": {"m"}}), http.StatusServiceUnavailable)

	// A cross-origin reaction is refused by the write gate.
	wantStatus(t, f.postH("/acme/widgets/issues/3/reactions/1", url.Values{"content": {"+1"}},
		map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden)
	wantStatus(t, f.postH("/acme/widgets/issues/3/reactions/1/x/delete", nil,
		map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden)
	wantStatus(t, f.postH("/acme/widgets/issues/3/close", nil,
		map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden)
}

func TestCoverageCollabPullListBranches(t *testing.T) {
	f := newCollabFixture(t)
	f.deps(pullDeps())

	// Unknown repo and unmatched state values are handled before the service.
	wantStatus(t, f.get("/acme/unknown/pulls"), http.StatusNotFound)
	wantContains(t, f.get("/acme/widgets/pulls?state=all"), "Pull requests")
	wantContains(t, f.get("/acme/widgets/pulls?base=main&head=feat"), "base=main")

	// Service errors: a 404 is the error page, anything else keeps the page.
	d := pullDeps()
	d.PullsList = func(context.Context, string, string, auth.Principal, pulls.ListFilter) (*pulls.ListResult, error) {
		return nil, pulls.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/pulls"), http.StatusNotFound)

	d = pullDeps()
	d.PullsList = func(context.Context, string, string, auth.Principal, pulls.ListFilter) (*pulls.ListResult, error) {
		return nil, errors.New("pulls exploded")
	}
	f.deps(d)
	rec := f.get("/acme/widgets/pulls")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "pulls exploded")
}

func TestCoverageCollabPullPageBranches(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	// An unknown repo on the conversation page is a 404 before the read.
	wantStatus(t, f.get("/acme/unknown/pull/5"), http.StatusNotFound)

	// A generic PR-read outage renders the page with the error.
	d := pullDeps()
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return nil, errors.New("pull exploded")
	}
	f.deps(d)
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "pull exploded")

	// A fork PR names its fork; the head sha falls back to the doc when the
	// live head is absent; the review summary cache and requested reviewers
	// feed the merge box.
	summary := []byte(`{"decision":"APPROVED","approvals":2,"threads_total":3,"threads_unresolved":1}`)
	forkPR := &pulls.PRDoc{
		Num: 5, Kind: "pr", Draft: false,
		Base: pulls.Endpoint{Repo: "acme/widgets", Ref: "main", SHA: strings.Repeat("a", 40)},
		Head: pulls.Endpoint{Repo: "acme/widgets", Ref: "feat", SHA: strings.Repeat("b", 40)},
		Fork: &pulls.ForkInfo{Repo: "bob/widgets"},
	}
	th := pullThread()
	th.ReviewSummary = summary
	d = pullDeps()
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return &pulls.PullView{Thread: th, PR: forkPR, HeadLive: "", BaseLive: strings.Repeat("a", 40)},
			nil
	}
	d.ReviewsList = func(context.Context, string, string, int, auth.Principal, int, int) (*review.ListResult, error) {
		return &review.ListResult{Reviews: []*review.ReviewEvent{{
			Kind: review.KindReviewDismissed, Seq: 4, At: "2026-11-18T10:00:00Z", By: "carol",
			Reason: "stale", Dismisses: intPtr(3), CommitSHA: strings.Repeat("b", 40),
		}}}, nil
	}
	d.RequestsGet = func(context.Context, string, string, int, auth.Principal) (*review.ReviewRequests, error) {
		return &review.ReviewRequests{Reviewers: []review.RequestedReviewer{{Principal: "dave", By: "alice"}}}, nil
	}
	checksDoc := &policy.Document{Rules: []*policy.Rule{
		{Match: policy.Match{}, Effect: &policy.ProtectEffect{RequireChecks: []string{"ci/test"}}},
		{Match: policy.Match{}, Effect: &policy.RequiredReviewsEffect{MinApprovals: 1}},
	}}
	d.PolicyOf = func(context.Context, git.RepoId) (*policy.Document, error) { return checksDoc, nil }
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "bob/widgets", "Dismissed", "dave", "ci/test")
}

func TestCoverageCollabPullWriteGuards(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	// Invalid numbers are 404s on every pull write.
	for _, path := range []string{
		"/acme/widgets/pull/0/comments", "/acme/widgets/pull/0/close",
		"/acme/widgets/pull/0/ready", "/acme/widgets/pull/0/draft",
		"/acme/widgets/pull/0/reviews", "/acme/widgets/pull/0/reviews/1/dismiss",
		"/acme/widgets/pull/0/reviewers", "/acme/widgets/pull/0/threads",
		"/acme/widgets/pull/0/threads/00000001/comments",
		"/acme/widgets/pull/0/threads/00000001/resolve",
		"/acme/widgets/pull/0/threads/00000001/unresolve",
		"/acme/widgets/pull/0/merge", "/acme/widgets/pull/0/update-branch",
	} {
		wantStatus(t, f.post(path, url.Values{"body": {"x"}, "strategy": {"merge"}, "verdict": {"COMMENTED"}}), http.StatusNotFound)
	}
	wantStatus(t, f.get("/acme/widgets/pull/0/commits"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/pull/0/files"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/pull/0/merge/task"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/pull/0/threads/00000001"), http.StatusNotFound)

	// Unknown repos are 404s on both read and write PR surfaces.
	wantStatus(t, f.get("/acme/unknown/pull/5/commits"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/pull/5/files"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/pull/5/merge/task"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/pull/5/threads/00000001"), http.StatusNotFound)
	wantStatus(t, f.post("/acme/unknown/pull/5/comments", url.Values{"body": {"x"}}), http.StatusNotFound)

	// Cross-origin writes are refused after the env gate.
	for _, path := range []string{
		"/acme/widgets/pull/5/comments", "/acme/widgets/pull/5/close",
		"/acme/widgets/pull/5/ready", "/acme/widgets/pull/5/reviews",
		"/acme/widgets/pull/5/reviews/1/dismiss", "/acme/widgets/pull/5/reviewers",
		"/acme/widgets/pull/5/threads", "/acme/widgets/pull/5/threads/00000001/comments",
		"/acme/widgets/pull/5/threads/00000001/resolve",
		"/acme/widgets/pull/5/threads/00000001/unresolve",
		"/acme/widgets/pull/5/update-branch",
	} {
		rec := f.postH(path, url.Values{"body": {"x"}, "reviewers": {"x"}},
			map[string]string{"Origin": "https://evil.example", "Accept": "text/plain"})
		wantStatus(t, rec, http.StatusForbidden)
	}

	// Not-wired writes are 503s for a privileged caller.
	f.deps(CollabDeps{})
	for _, path := range []string{
		"/acme/widgets/pull/5/comments", "/acme/widgets/pull/5/close",
		"/acme/widgets/pull/5/ready", "/acme/widgets/pull/5/reviews",
		"/acme/widgets/pull/5/reviews/1/dismiss", "/acme/widgets/pull/5/reviewers",
		"/acme/widgets/pull/5/threads", "/acme/widgets/pull/5/threads/00000001/comments",
		"/acme/widgets/pull/5/threads/00000001/resolve",
		"/acme/widgets/pull/5/threads/00000001/unresolve",
		"/acme/widgets/pull/5/merge", "/acme/widgets/pull/5/update-branch",
	} {
		rec := f.post(path, url.Values{"body": {"x"}, "strategy": {"merge"}, "verdict": {"COMMENTED"}, "reviewers": {"x"}})
		wantStatus(t, rec, http.StatusServiceUnavailable)
	}
}

func TestCoverageCollabPullWriteBranchDetails(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	// The composite close posts the comment first; its failure aborts.
	d := pullDeps()
	d.PullUpdate = func(context.Context, string, string, int, auth.Principal, pulls.PRPatch) (*pulls.Thread, *pulls.PRDoc, error) {
		return pullThread(), nil, nil
	}
	d.PullComment = func(context.Context, string, string, int, auth.Principal, string) (*pulls.Event, error) {
		return nil, pulls.ErrLocked
	}
	f.deps(d)
	wantStatus(t, f.post("/acme/widgets/pull/5/close", url.Values{"body": {"done"}}), http.StatusConflict)

	// A review with no explicit sha resolves the head from the PR doc.
	var submitted review.SubmitInput
	d = pullDeps()
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		pr := &pulls.PRDoc{Head: pulls.Endpoint{SHA: strings.Repeat("b", 40)}}
		return &pulls.PullView{Thread: pullThread(), PR: pr, HeadLive: ""}, nil
	}
	d.ReviewSubmit = func(_ context.Context, _, _ string, _ int, _ auth.Principal, in review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error) {
		submitted = in
		return &review.ReviewEvent{Seq: 2}, nil, nil, nil
	}
	f.deps(d)
	wantStatus(t, f.post("/acme/widgets/pull/5/reviews", url.Values{"verdict": {"APPROVED"}}), http.StatusSeeOther)
	if submitted.CommitSHA != strings.Repeat("b", 40) {
		t.Fatalf("review sha fallback = %q", submitted.CommitSHA)
	}

	// Thread staging clamps an inverted range, and a failing diff/anchor step
	// re-renders rather than redirecting.
	var anchor review.Anchor
	d = pullDeps()
	d.ThreadOpen = func(_ context.Context, _, _ string, _ int, _ auth.Principal, a review.Anchor, _ string) (*review.ThreadHeader, error) {
		anchor = a
		return &review.ThreadHeader{TID: "00000003"}, nil
	}
	f.deps(d)
	rec := f.post("/acme/widgets/pull/5/threads", url.Values{
		"path": {"x.txt"}, "side": {"new"}, "start": {"1"}, "end": {"0"}, "body": {"why?"},
	})
	wantStatus(t, rec, http.StatusSeeOther)
	if anchor.NewLines != 1 || anchor.NewStart != 1 {
		t.Fatalf("clamped anchor = %+v", anchor)
	}

	// Head fallback for the anchor's commit sha.
	var anchorHead string
	d = pullDeps()
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		pr := &pulls.PRDoc{Head: pulls.Endpoint{SHA: strings.Repeat("c", 40)}}
		return &pulls.PullView{Thread: pullThread(), PR: pr, HeadLive: ""}, nil
	}
	d.ThreadOpen = func(_ context.Context, _, _ string, _ int, _ auth.Principal, a review.Anchor, _ string) (*review.ThreadHeader, error) {
		anchorHead = a.CommitSHA
		return &review.ThreadHeader{TID: "00000004"}, nil
	}
	f.deps(d)
	wantStatus(t, f.post("/acme/widgets/pull/5/threads", url.Values{
		"path": {"x.txt"}, "side": {"new"}, "start": {"1"}, "end": {"1"}, "body": {"why?"},
	}), http.StatusSeeOther)
	if anchorHead != strings.Repeat("c", 40) {
		t.Fatalf("anchor head fallback = %q", anchorHead)
	}

	// PullGet / PullDiff / ThreadOpen failures each surface their mapping.
	for _, tc := range []struct {
		name  string
		mut   func(*CollabDeps)
		form  url.Values
		code  int
		alert string
	}{
		{"pull get", func(d *CollabDeps) {
			d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
				return nil, pulls.ErrNotFound
			}
		}, nil, http.StatusNotFound, ""},
		{"diff", func(d *CollabDeps) {
			d.PullDiff = func(context.Context, string, string, int, auth.Principal) (string, error) {
				return "", errors.New("diff exploded")
			}
		}, nil, http.StatusServiceUnavailable, "diff exploded"},
		{"thread open", func(d *CollabDeps) {
			d.ThreadOpen = func(context.Context, string, string, int, auth.Principal, review.Anchor, string) (*review.ThreadHeader, error) {
				return nil, review.ErrInvalid
			}
		}, nil, http.StatusBadRequest, ""},
	} {
		dd := pullDeps()
		dd.ThreadOpen = func(context.Context, string, string, int, auth.Principal, review.Anchor, string) (*review.ThreadHeader, error) {
			return &review.ThreadHeader{TID: "00000005"}, nil
		}
		tc.mut(&dd)
		f.deps(dd)
		form := tc.form
		if form == nil {
			form = url.Values{"path": {"x.txt"}, "side": {"new"}, "start": {"1"}, "end": {"1"}, "body": {"why?"}}
		}
		rec := f.post("/acme/widgets/pull/5/threads", form)
		if rec.Code != tc.code {
			t.Fatalf("%s: status = %d want %d (%s)", tc.name, rec.Code, tc.code, bodyText(rec))
		}
		if tc.alert != "" {
			wantContains(t, rec, tc.alert)
		}
	}

	// Empty thread replies are rejected inline.
	d = pullDeps()
	d.ThreadComment = func(context.Context, string, string, int, string, auth.Principal, string) (*review.ThreadComment, error) {
		return &review.ThreadComment{Seq: 2}, nil
	}
	f.deps(d)
	wantStatus(t, f.post("/acme/widgets/pull/5/threads/00000001/comments", url.Values{"body": {"  "}}), http.StatusBadRequest)

	// A thread-read outage is mapped onto the error page.
	d = pullDeps()
	d.ThreadGet = func(context.Context, string, string, int, string, auth.Principal, int, int) (*review.ThreadView, error) {
		return nil, review.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/pull/5/threads/00000001"), http.StatusNotFound)

	// The lazy thread fragment without the review service is a 503.
	adm := newCollabFixture(t)
	adm.principal = auth.Principal{Name: "alice", Admin: true}
	adm.deps(CollabDeps{})
	wantStatus(t, adm.get("/acme/widgets/pull/5/threads/00000001"), http.StatusServiceUnavailable)

	// Commits: a negative skip clamps, and a service error re-renders.
	d = pullDeps()
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/pull/5/commits?skip=-1"), http.StatusOK)
	d = pullDeps()
	d.PullCommits = func(context.Context, string, string, int, auth.Principal, int, int) ([]pulls.CommitEntry, bool, error) {
		return nil, false, errors.New("commits exploded")
	}
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5/commits")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "commits exploded")

	// Files: a generic PullGet error renders the page with the error line.
	d = pullDeps()
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return nil, errors.New("files exploded")
	}
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5/files")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "files exploded")

	// An old-side selection is validated against old line numbers.
	d = pullDeps()
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/pull/5/files?path=x.txt&side=old&line=1"), http.StatusOK)
}

func TestCoverageCollabChecksBranches(t *testing.T) {
	f := newCollabFixture(t)
	sha := strings.Repeat("d", 40)

	// Unknown repo and unknown sha are 404s.
	wantStatus(t, f.get("/acme/unknown/checks"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/widgets/checks/nothex/contexts"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/checks/"+sha+"/contexts"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/checks/"+sha), http.StatusNotFound)

	// The context filter matches and misses (a miss says the filter only
	// applied to the loaded page).
	f.deps(checkDeps())
	rec := f.get("/acme/widgets/checks?context=ci")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, "filter applies")
	rec = f.get("/acme/widgets/checks?context=zzz")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "filter applies")

	// Missing check services on the detail and fragment endpoints.
	f.principal = auth.Principal{Name: "alice", Admin: true}
	f.deps(CollabDeps{})
	wantStatus(t, f.get("/acme/widgets/checks/"+sha+"/contexts"), http.StatusServiceUnavailable)

	// A vanished combined view is a 404 (the checks page's own rule).
	d := checkDeps()
	d.ChecksCombined = func(context.Context, string, string, string, auth.Principal) (*checks.CombinedView, error) {
		return nil, checks.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/checks/"+sha), http.StatusNotFound)

	// A vanished check index is a 404 too.
	d = checkDeps()
	d.ChecksList = func(context.Context, string, string, auth.Principal, string, int) (*checks.ChecksPage, error) {
		return nil, checks.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/checks"), http.StatusNotFound)
}

func TestCoverageCollabReleaseBranches(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}

	wantStatus(t, f.get("/acme/unknown/releases"), http.StatusNotFound)
	wantStatus(t, f.get("/acme/unknown/releases/v1"), http.StatusNotFound)

	// Error mapping: 404 is the error page, anything else keeps the page.
	d := releaseDeps()
	d.ReleasesList = func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
		return nil, false, releases.ErrNotFound
	}
	f.deps(d)
	wantStatus(t, f.get("/acme/widgets/releases"), http.StatusNotFound)

	d = releaseDeps()
	d.ReleasesList = func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
		return nil, false, errors.New("releases exploded")
	}
	f.deps(d)
	rec := f.get("/acme/widgets/releases")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "releases exploded")

	// Draft/prerelease counters and filters, plus the tag-as-name fallback and
	// an excerpt equal to the name being dropped.
	d = releaseDeps()
	d.ReleaseLatest = nil
	d.ReleasesList = func(context.Context, string, string, auth.Principal, int, string) ([]*releases.Release, bool, error) {
		return []*releases.Release{
			{Tag: "v0.1.0", Name: "", Draft: true, CreatedAt: "2026-11-10T00:00:00Z"},
			{Tag: "v0.2.0", Name: "v0.2.0", Prerelease: true, CreatedAt: "2026-11-11T00:00:00Z", Body: "# v0.2.0\n\nnotes"},
			{Tag: "v0.3.0", Name: "v0.3.0", CreatedAt: "2026-11-12T00:00:00Z"},
		}, false, nil
	}
	f.deps(d)
	rec = f.get("/acme/widgets/releases?filter=drafts")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "v0.1.0")
	wantNotContains(t, rec, "v0.3.0")
	rec = f.get("/acme/widgets/releases?filter=prereleases")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "v0.2.0")
	wantNotContains(t, rec, "v0.3.0")

	// A generic detail-read outage keeps the release page.
	d = releaseDeps()
	d.ReleaseGet = func(context.Context, string, string, auth.Principal, string) (*releases.Release, store.Version, error) {
		return nil, "", errors.New("release exploded")
	}
	f.deps(d)
	rec = f.get("/acme/widgets/releases/v1.2.3")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "release exploded")

	// The edit form pre-fills from the stored release.
	d = releaseDeps()
	f.deps(d)
	rec = f.get("/acme/widgets/releases/v1.2.3?edit=1")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `value="v1.2.3"`)

	// /releases/new is reserved for the forms composer: the collab page
	// handler answers 404 when reached directly (the literal route normally
	// wins in the mux).
	req := f.req("GET", "/acme/widgets/releases/new", nil)
	req.SetPathValue("owner", "acme")
	req.SetPathValue("repo", "widgets")
	req.SetPathValue("tag", "new")
	rec = httptest.NewRecorder()
	f.srv.releasePage(rec, req)
	wantStatus(t, rec, http.StatusNotFound)

	// Not-wired release writes are 503s (the role floor already passed).
	f.deps(CollabDeps{})
	for _, path := range []string{
		"/acme/widgets/releases/v1/save",
		"/acme/widgets/releases/v1/publish",
		"/acme/widgets/releases/v1/delete",
		"/acme/widgets/releases/v1/assets",
		"/acme/widgets/releases/v1/assets/sums.txt/delete",
	} {
		wantStatus(t, f.post(path, url.Values{"name": {"n"}, "body": {"b"}}), http.StatusServiceUnavailable)
	}

	// Asset upload: the filename names the asset when the form omits a name,
	// an absent part Content-Type defaults to octet-stream, and a service
	// rejection re-renders the release page.
	var seenName, seenType, seenSHA string
	var seenSize int64
	d = releaseDeps()
	d.AssetUpload = func(_ context.Context, _, _ string, _ auth.Principal, _, name string, body io.Reader, size int64, shaHex, ct string) (*releases.AssetEntry, error) {
		seenName, seenType, seenSHA, seenSize = name, ct, shaHex, size
		got, _ := io.ReadAll(body)
		if string(got) != "payload" {
			t.Fatalf("upload body = %q", got)
		}
		return nil, releases.ErrConflict
	}
	f.deps(d)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="raw.bin"`)
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("payload"))
	_ = mw.Close()
	rec = f.rawPostH("/acme/widgets/releases/v1.2.3/assets", mw.FormDataContentType(), buf.Bytes(), nil)
	// The failure must be reported on the release page, never a redirect, and a
	// service conflict is 409 (not the generic 503 fallback).
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("failed upload redirected: %s", bodyText(rec))
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("asset conflict status = %d, want 409: %s", rec.Code, bodyText(rec))
	}
	wantContains(t, rec, "conflict")
	if seenName != "raw.bin" || seenType != "application/octet-stream" {
		t.Fatalf("upload name/type = %q %q", seenName, seenType)
	}
	if seenSize != int64(len("payload")) || len(seenSHA) != 64 {
		t.Fatalf("upload size/sha = %d %q", seenSize, seenSHA)
	}

	// A cross-origin upload is refused by the write gate.
	wantStatus(t, f.postH("/acme/widgets/releases/v1.2.3/assets", nil,
		map[string]string{"Origin": "https://evil.example", "Accept": "text/plain"}), http.StatusForbidden)

	// An unwritable temp dir means the upload cannot be staged: the page says
	// so (503) instead of a bare 500.
	t.Setenv("TMPDIR", "/nonexistent/walhub-tmp")
	rec = f.rawPostH("/acme/widgets/releases/v1.2.3/assets", mw.FormDataContentType(), buf.Bytes(), nil)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "could not stage the upload")

	// Forks: a generic listing outage keeps the page.
	fd := pullDeps()
	fd.ForksList = func(context.Context, string, string, auth.Principal, string, int) (*pulls.ForkListPage, error) {
		return nil, errors.New("forks exploded")
	}
	f.deps(fd)
	rec = f.get("/acme/widgets/forks")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "forks exploded")
}

func TestCoverageCollabFragmentExecutionFailure(t *testing.T) {
	f := newCollabFixture(t)
	// A fragment whose template errors at execution must answer 500, never
	// panic or recurse into the same broken set.
	tpl := template.Must(template.New("base").Parse(`{{define "collab_thread_comments"}}{{template "does_not_exist" .}}{{end}}`))
	f.srv.tpl["collab/broken"] = tpl
	rec := httptest.NewRecorder()
	f.srv.collabFragment(rec, f.req("GET", "/x", nil), "collab/broken", "collab_thread_comments", nil)
	wantStatus(t, rec, http.StatusInternalServerError)
}

func TestCoverageCollabDiffAndVisibilityHelpers(t *testing.T) {
	files := ParseUnifiedDiff("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n")

	// An old-side stage marks the deletion row.
	stage := &stageSelection{Path: "x", Side: "old", Line: 1}
	out := buildDiff(files, nil, stage, "/acme/widgets/pull/5/files")
	staged := false
	for _, f := range out.Files {
		for _, h := range f.Hunks {
			for _, r := range h.Rows {
				if r.Staged && r.Side == "old" {
					staged = true
				}
			}
		}
	}
	if !staged {
		t.Fatal("old-side staging did not mark the deletion row")
	}

	// Two drifting threads sort by label (the comparator runs).
	drift := buildDiff(files, []threadCard{
		{TID: "2", Path: "x", Side: review.SideNew, Start: 99, Label: "x:99"},
		{TID: "1", Path: "x", Side: review.SideNew, Start: 98, Label: "x:98"},
	}, nil, "")
	if len(drift.Drifted) != 2 || drift.Drifted[0].Label != "x:98" {
		t.Fatalf("drift order = %+v", drift.Drifted)
	}

	// A hunk with no matching line is skipped; a later hunk anchors.
	twoHunks := ParseUnifiedDiff("diff --git a/x b/x\n--- a/x\n+++ b/x\n" +
		"@@ -1,1 +1,1 @@\n+x\n" +
		"@@ -10,2 +10,2 @@\n-old\n+new\n ctx\n")
	if a, ok := anchorFor(twoHunks, "x", "new", 10, 10, strings.Repeat("a", 40)); !ok || a.NewStart != 10 {
		t.Fatalf("second-hunk anchor = %+v ok=%v", a, ok)
	}

	// Context lines before the selection feed the drift hash.
	ctxBefore := ParseUnifiedDiff("diff --git a/y b/y\n--- a/y\n+++ b/y\n@@ -1,3 +1,3 @@\n a\n b\n-c\n+d\n")
	if a, ok := anchorFor(ctxBefore, "y", "new", 2, 2, strings.Repeat("a", 40)); !ok || a.ContextSHA == "" {
		t.Fatalf("ctx-before anchor = %+v ok=%v", a, ok)
	}

	// Close/draft visibility rules.
	if got := pullCloseVisibility(nil, &pulls.PRDoc{Merged: true}, auth.Principal{Name: "alice"}, identity.RoleAdmin); got[0] || got[1] {
		t.Fatalf("merged close visibility = %v", got)
	}
	if got := pullCloseVisibility(&pulls.Thread{State: "closed", Author: "alice"}, nil, auth.Principal{Name: "alice"}, identity.RoleRead); got[0] || !got[1] {
		t.Fatalf("closed close visibility = %v", got)
	}
	if got := pullDraftVisibility(nil, nil, auth.Principal{Name: "alice"}, identity.RoleRead); got[0] || got[1] {
		t.Fatalf("nil draft visibility = %v", got)
	}
}
