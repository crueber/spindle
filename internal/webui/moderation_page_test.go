package webui

// moderation_page_test.go — M2 page projection matrix (PLAN-V2 §M.4/M.6.2):
// the hidden card replaces the body for a reader, the moderator sees the
// original behind an expand plus the unhide control, the lock banner and
// pill are owner-visible, the composer is disabled with the reason, and the
// page actions enforce the same floors as the API (triage hide, maintain
// lock).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/review"
	"github.com/crueber/spindle/internal/server/auth"
)

func withRole(d CollabDeps, role identity.Role) CollabDeps {
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return role }
	return d
}

func hiddenIssueDeps(moderator bool) CollabDeps {
	d := issueDeps()
	var hiddenBody *string
	if moderator {
		hiddenBody = ptr("secret comment body")
	}
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return &issues.ThreadView{
			Thread: testThread(),
			Events: []*issues.Event{
				{Seq: 2, Type: issues.EventCommented, Actor: "bob", At: "2026-11-18T11:00:00Z", Body: hiddenBody,
					Hidden: &issues.HiddenInfo{By: "mod", At: "2026-11-20T10:00:00Z", Reason: "spam"}},
				{Seq: 1, Type: issues.EventOpened, Actor: "alice", At: "2026-11-18T10:00:00Z", Body: ptr("visible body")},
			},
			Locked:     true,
			LockReason: "flame war",
		}, nil
	}
	return d
}

func TestIssueHiddenAndLockedPage(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "reader"}
	f.deps(withRole(hiddenIssueDeps(false), identity.RoleRead))
	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"Hidden by a moderator", "mod", "spam",
		"Locked by a moderator: flame war",
		">Locked<",
		`aria-disabled="true"`, // composer textarea + submit disabled
	)
	// The hidden body never reaches a reader's browser, and the moderator
	// affordances are absent.
	wantNotContains(t, rec, "Unhide", "Hide comment", "Lock conversation", "secret comment body")

	// A moderator gets the original behind the expand plus the unhide and
	// hide controls.
	f.principal = auth.Principal{Name: "mod"}
	f.deps(withRole(hiddenIssueDeps(true), identity.RoleTriage))
	rec = f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Show original", "Unhide", "Hide comment", "secret comment body")
	wantNotContains(t, rec, "Lock conversation")

	// A maintainer gets the lock control; when unlocked it offers "Lock".
	d := hiddenIssueDeps(true)
	d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
		return &issues.ThreadView{Thread: testThread()}, nil
	}
	f.principal = auth.Principal{Name: "maint"}
	f.deps(withRole(d, identity.RoleMaintain))
	rec = f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Lock conversation", "action=\"/acme/widgets/issues/3/moderation/lock\"")
	wantNotContains(t, rec, "Locked by a moderator")
}

func hiddenPullDeps(moderator bool) CollabDeps {
	d := pullDeps()
	pr := &pulls.PRDoc{Num: 5}
	var reviewBody string
	if moderator {
		pr.Body = "secret description body"
		reviewBody = "secret review body"
	}
	d.PullGet = func(context.Context, string, string, int, auth.Principal, int, int) (*pulls.PullView, error) {
		return &pulls.PullView{
			Thread:            pullThread(),
			PR:                pr,
			DescriptionHidden: &pulls.HiddenInfo{By: "mod", At: "2026-11-20T10:00:00Z", Reason: "abusive"},
			Events:            []*pulls.Event{{Seq: 2, Type: pulls.EventCommented, Actor: "carol", At: "2026-11-18T09:30:00Z", Hidden: &pulls.HiddenInfo{By: "mod", At: "t", Reason: "spam"}}},
			Locked:            true,
			LockReason:        "cooldown",
		}, nil
	}
	d.ReviewsList = func(context.Context, string, string, int, auth.Principal, int, int) (*review.ListResult, error) {
		reviews := []*review.ReviewEvent{{
			Seq: 1, Kind: review.KindReview, By: "alice", At: "2026-11-18T09:00:00Z",
			State: review.StateCommented, Body: reviewBody,
			Hidden: &review.HiddenInfo{By: "mod", At: "t", Reason: "abusive"},
		}}
		if moderator {
			// A visible review keeps the hide affordance assertable.
			reviews = append(reviews, &review.ReviewEvent{
				Seq: 2, Kind: review.KindReview, By: "bob", At: "2026-11-18T09:10:00Z",
				State: review.StateCommented, Body: "a visible review",
			})
		}
		return &review.ListResult{Reviews: reviews}, nil
	}
	return d
}

func TestPullHiddenAndLockedPage(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "reader"}
	f.deps(withRole(hiddenPullDeps(false), identity.RoleRead))
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Description hidden by a moderator", "Hidden by a moderator", "Locked by a moderator: cooldown", ">Locked<")
	wantNotContains(t, rec, "Unhide", "Hide review", "Lock conversation", "secret description body", "secret review body")

	f.principal = auth.Principal{Name: "mod"}
	f.deps(withRole(hiddenPullDeps(true), identity.RoleTriage))
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Description hidden by a moderator", "Unhide", "Hide review", "Show original", "secret description body", "secret review body")

	// The review-thread comment fragment redacts the same way.
	d := withRole(pullDeps(), identity.RoleRead)
	d.ThreadGet = func(context.Context, string, string, int, string, auth.Principal, int, int) (*review.ThreadView, error) {
		return &review.ThreadView{
			Thread: &review.ThreadHeader{TID: "deadbeef"},
			Comments: []*review.ThreadComment{{
				Kind: review.KindReviewThreadComment, Seq: 1, By: "alice", At: "t",
				Hidden: &review.HiddenInfo{By: "mod", At: "t", Reason: "spam"},
			}},
		}, nil
	}
	f.deps(d)
	wantContains(t, f.get("/acme/widgets/pull/5/threads/deadbeef"), "Hidden by a moderator", "Hidden by a moderator", "spam")
}

func TestModerationPageActions(t *testing.T) {
	var hidTarget, hidReason string
	var lockedNum int
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "maint"}
	d := withRole(issueDeps(), identity.RoleMaintain)
	d.ModHide = func(_ context.Context, _, _ string, _ auth.Principal, target, reason string) (int, bool, error) {
		hidTarget, hidReason = target, reason
		return 2, true, nil
	}
	d.ModLock = func(_ context.Context, _, _ string, num int, _ auth.Principal, _, _ string) (int, bool, error) {
		lockedNum = num
		return 3, true, nil
	}
	f.deps(d)

	rec := f.post("/acme/widgets/issues/3/moderation/hide", url.Values{
		"target": {"thread:000003:event:000000000002"}, "reason": {"spam"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hide status = %d: %s", rec.Code, firstN(bodyText(rec), 300))
	}
	if hidTarget != "thread:000003:event:000000000002" || hidReason != "spam" {
		t.Fatalf("hide call = (%q, %q)", hidTarget, hidReason)
	}
	rec = f.post("/acme/widgets/issues/3/moderation/lock", url.Values{"reason": {"cooldown"}})
	if rec.Code != http.StatusSeeOther || lockedNum != 3 {
		t.Fatalf("lock = %d num=%d", rec.Code, lockedNum)
	}
	// Same-origin is enforced (a cross-origin post 403s before the verb).
	r := f.req("POST", "/acme/widgets/issues/3/moderation/hide", url.Values{"target": {"x"}})
	r.Header.Set("Origin", "https://evil.example")
	rec = f.postReq(r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	// Unknown actions 404; the floor is the page's own mirror.
	rec = f.post("/acme/widgets/issues/3/moderation/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action = %d", rec.Code)
	}
	// Triage cannot lock (maintain floor) — the page refuses before the verb.
	f.deps(withRole(issueDeps(), identity.RoleTriage))
	rec = f.post("/acme/widgets/issues/3/moderation/lock", url.Values{"reason": {"x"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("triage lock = %d", rec.Code)
	}
	// Read cannot hide.
	f.deps(withRole(issueDeps(), identity.RoleRead))
	rec = f.post("/acme/widgets/issues/3/moderation/hide", url.Values{"target": {"thread:000003:event:000000000002"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read hide = %d", rec.Code)
	}
	// A service failure re-renders the thread with the error line.
	d = withRole(issueDeps(), identity.RoleMaintain)
	d.ModHide = func(context.Context, string, string, auth.Principal, string, string) (int, bool, error) {
		return 0, false, issues.ErrLocked
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/moderation/hide", url.Values{"target": {"thread:000003:event:000000000002"}})
	if rec.Code != http.StatusConflict || !strings.Contains(bodyText(rec), "commenting is locked") {
		t.Fatalf("write-back = %d: %s", rec.Code, firstN(bodyText(rec), 300))
	}
}

// postReq is the post helper for a fully-built request (origin overrides).
func (f *collabFixture) postReq(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	if !f.srv.Serve(rec, r) {
		f.t.Fatalf("POST %s was not claimed by the webui mount", r.URL.Path)
	}
	return rec
}
