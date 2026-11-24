package webui

// reports_affordance_test.go — the M3 report affordance (PLAN-V2 §M.4): a
// .link "Report" <details> form on every comment card and review body,
// posting to the repo page endpoint, with the confirmation rendering the
// report id. The page never decides who may file — the service does — but it
// hides the affordance from anonymous viewers (and the write gate answers a
// replayed anonymous POST).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/review"
	"github.com/crueber/spindle/internal/server/auth"
)

func TestReportAffordanceOnCommentCards(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "reader"}
	d := issueDeps()
	var got []ModerationReportInput
	d.ModReport = func(_ context.Context, owner, repo string, _ auth.Principal, in ModerationReportInput) (int, error) {
		got = append(got, in)
		return 11, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		`action="/acme/widgets/issues/3/report"`,
		`name="kind" value="thread"`,
		`name="seq" value="2"`,
		`name="reason" value="spam"`,
		`name="reason" value="abusive"`,
		`name="reason" value="off-topic"`,
		`name="reason" value="other" checked`,
		`name="note"`,
		">Report<",
	)

	// The form posts the structured target; the service builds the key.
	rec = f.post("/acme/widgets/issues/3/report", url.Values{
		"kind": {"thread"}, "seq": {"2"}, "reason": {"spam"}, "note": {"bot"},
	})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/issues/3?reported=11#event-2" {
		t.Fatalf("Location = %q", loc)
	}
	if len(got) != 1 || got[0].Kind != "thread" || got[0].Num != 3 || got[0].Seq != 2 || got[0].Reason != "spam" || got[0].Note != "bot" {
		t.Fatalf("seam input = %+v", got)
	}

	// The confirmation renders the report id as the page notice (never the
	// report's content).
	rec = f.get("/acme/widgets/issues/3?reported=11")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Report #11 filed", `role="status"`)

	// A duplicate is the API's 409, rendered inline on the conversation.
	d2 := issueDeps()
	d2.ModReport = func(context.Context, string, string, auth.Principal, ModerationReportInput) (int, error) {
		return 0, fmt.Errorf("%w: you already have an open report for this target", moderation.ErrConflict)
	}
	f.deps(d2)
	rec = f.post("/acme/widgets/issues/3/report", url.Values{"kind": {"thread"}, "seq": {"2"}, "reason": {"spam"}})
	wantStatus(t, rec, http.StatusConflict)
	wantContains(t, rec, "already have an open report")
}

func TestReportAffordanceAnonymousHasNoControl(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Anonymous()
	f.deps(issueDeps())
	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, ">Report<", "/acme/widgets/issues/3/report")

	// The replayed anonymous POST is gated to the login interstitial and the
	// write seam is never reached.
	called := false
	d := issueDeps()
	d.ModReport = func(context.Context, string, string, auth.Principal, ModerationReportInput) (int, error) {
		called = true
		return 1, nil
	}
	f.deps(d)
	rec = f.post("/acme/widgets/issues/3/report", url.Values{"kind": {"thread"}, "seq": {"2"}, "reason": {"spam"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/login-required") {
		t.Fatalf("Location = %q, want the login gate", loc)
	}
	if called {
		t.Fatal("anonymous report reached the write seam")
	}
}

func TestReportAffordanceOnReviewBody(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "reader"}
	d := pullDeps()
	var got ModerationReportInput
	d.ModReport = func(_ context.Context, owner, repo string, _ auth.Principal, in ModerationReportInput) (int, error) {
		got = in
		return 4, nil
	}
	d.ReviewsList = func(context.Context, string, string, int, auth.Principal, int, int) (*review.ListResult, error) {
		return &review.ListResult{Reviews: []*review.ReviewEvent{{
			Kind: "review", Seq: 2, By: "carol", At: "2026-11-18T09:30:00Z",
			State: review.StateCommented, Body: "looks fine",
		}}}, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		`action="/acme/widgets/pull/5/report"`,
		`name="kind" value="review"`,
		`name="seq" value="2"`,
		">Report<",
	)

	rec = f.post("/acme/widgets/pull/5/report", url.Values{
		"kind": {"review"}, "seq": {"2"}, "reason": {"abusive"}, "note": {"rude"},
	})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/pull/5?reported=4#reviews" {
		t.Fatalf("Location = %q", loc)
	}
	if got.Kind != "review" || got.Num != 5 || got.Seq != 2 || got.Reason != "abusive" {
		t.Fatalf("seam input = %+v", got)
	}
}
