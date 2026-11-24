package webui

// parity_handlers_cover_test.go — the refusal/error arms of the parity page
// writes (bad number, anonymous, unwired service, service error) and the
// moderation action dispatcher's unknown/refused arms.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

func TestParityPageWriteErrorArms(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := issueDeps()
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleTriage }
	d.IssuePin = func(context.Context, string, string, int, auth.Principal, bool) ([]int, bool, error) {
		return nil, false, errors.New("boom")
	}
	d.IssueDepAdd = func(context.Context, string, string, int, auth.Principal, string, int) (bool, error) {
		return false, errors.New("boom")
	}
	d.IssueDepRemove = func(context.Context, string, string, int, auth.Principal, string, int) (bool, error) {
		return false, errors.New("boom")
	}
	f.deps(d)

	// Bad numbers are 404 page errors before any seam runs.
	if rec := f.post("/acme/widgets/issues/0/pin", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("pin num 0 = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/0/dependencies", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("dep num 0 = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies/blocks/0/delete", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("dep target 0 = %d", rec.Code)
	}
	// Service failures re-render with the inline error.
	if rec := f.post("/acme/widgets/issues/3/pin", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pin boom = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies", url.Values{"kind": {"blocks"}, "target": {"4"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dep add boom = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies/blocks/4/delete", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dep remove boom = %d", rec.Code)
	}

	// Anonymous viewers are sent to the login interstitial.
	f.principal = auth.Anonymous()
	if rec := f.post("/acme/widgets/issues/3/pin", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("anon pin = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("anon dep = %d", rec.Code)
	}

	// Unwired seams answer 503, never panic.
	f.principal = auth.Principal{Name: "alice", Write: true}
	f.deps(CollabDeps{RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleTriage }})
	if rec := f.post("/acme/widgets/issues/3/pin", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pin unwired = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies", url.Values{"kind": {"blocks"}, "target": {"4"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dep add unwired = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/3/dependencies/blocks/4/delete", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dep remove unwired = %d", rec.Code)
	}
}

func TestModerateActionArms(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := CollabDeps{RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleMaintain }}
	f.deps(d)
	// Unknown action and unknown conversation are 404.
	if rec := f.post("/acme/widgets/issues/3/moderation/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/issues/0/moderation/lock", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown conversation = %d", rec.Code)
	}
	// Every unwired verb is a 503 (never a silent success).
	for _, action := range []string{"hide", "unhide", "lock", "unlock"} {
		if rec := f.post("/acme/widgets/issues/3/moderation/"+action, url.Values{"reason": {"x"}, "target": {"thread:000003:event:000000000001"}}); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s unwired = %d", action, rec.Code)
		}
	}
	// The PR twin routes through the same dispatcher (re-renders the PR page
	// on a service refusal).
	d.ModLock = func(context.Context, string, string, int, auth.Principal, string, string) (int, bool, error) {
		return 0, false, errors.New("boom")
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/pull/5/moderation/lock", url.Values{"mode": {"collaborators"}}); rec.Code < 400 {
		t.Fatalf("pull lock boom = %d", rec.Code)
	}
}

func TestModerationLockComposition(t *testing.T) {
	base := lockState{Locked: true, Reason: "This conversation is closed"}
	if got := moderationLock(base, false, "collaborators", "x", true); got != base {
		t.Fatalf("unlocked must pass the base through: %+v", got)
	}
	// all: refuses everyone and names the moderator.
	got := moderationLock(base, true, "all", "", false)
	if !got.Locked || !got.Moderated || got.Mode != "all" || !strings.Contains(got.Reason, "no reason given") {
		t.Fatalf("all-mode lock = %+v", got)
	}
	// collaborators + write+: live composer, banner still names the mode.
	got = moderationLock(base, true, "COLLABORATORS", "focus", true)
	if got.Locked || !got.Moderated || !got.CollaboratorsOnly || !strings.Contains(got.Reason, "Locked to collaborators: focus") {
		t.Fatalf("collaborators lock (writer) = %+v", got)
	}
	// collaborators + read: refused with the same wording.
	got = moderationLock(base, true, "collaborators", "focus", false)
	if !got.Locked || !got.CollaboratorsOnly {
		t.Fatalf("collaborators lock (reader) = %+v", got)
	}
}

func TestIssueDetailWithoutDependencySeam(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	f.deps(issueDeps()) // no IssueDeps / IssuePins
	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, `id="dependencies"`, `aria-label="Unpin issue #3"`)
}
