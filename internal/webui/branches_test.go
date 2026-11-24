package webui

// branches_test.go — the Branches page and its create/delete writes: the gate
// matrix, the one-primary rule, ahead/behind rendering, delete-by-default
// refusal surfacing, and the honest unwired/empty states. The service's own
// policy/protection law is proven in internal/branches; here the seam is a
// fake so the page contract is isolated.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/branches"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

func (v *repoFakeView) Branches(_ context.Context, _ git.RepoId) (api.BranchOverview, error) {
	v.branchesN++
	if v.branchesErr != nil {
		return api.BranchOverview{}, v.branchesErr
	}
	return v.branches, nil
}

// branchFixture builds a writer-visible page with a fake service.
type branchWrites struct {
	created, deleted int
	name, from       string
	createErr        error
	deleteErr        error
}

func (w *branchWrites) deps() RepoDeps {
	return RepoDeps{
		BranchCreate: func(_ context.Context, _, _ string, _ auth.Principal, name, from string) error {
			w.created++
			w.name, w.from = name, from
			return w.createErr
		},
		BranchDelete: func(_ context.Context, _, _ string, _ auth.Principal, name string) error {
			w.deleted++
			w.name = name
			return w.deleteErr
		},
	}
}

func branchOverview() api.BranchOverview {
	return api.BranchOverview{
		Default:    "main",
		DefaultSHA: strings.Repeat("a", 40),
		AheadKnown: true,
		Branches: []api.BranchEntry{
			{Name: "main", SHA: strings.Repeat("a", 40), Subject: "merge everything", Author: "Ada", Committed: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), IsDefault: true},
			{Name: "feature/x", SHA: strings.Repeat("b", 40), Subject: "<unsafe> subject", Author: "Bob", Committed: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), Ahead: 3, Behind: 2},
		},
	}
}

func TestRepoBranchesRenders(t *testing.T) {
	w := &branchWrites{}
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	f.view.branches = branchOverview()
	body := f.mustGet("/demo/spindle/branches", http.StatusOK)

	for _, want := range []string{
		"feature/x", "3 ahead · 2 behind", "git clone --branch feature/x ",
		"/demo/spindle/compare/main...feature/x", "/demo/spindle/commit/",
		"Create branch", "default",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Subject text is escaped.
	if !strings.Contains(body, "&lt;unsafe&gt; subject") || strings.Contains(body, "<unsafe>") {
		t.Error("branch subject was not escaped")
	}
	// The default branch has no delete form; the other one does. The actions
	// block renders twice per row (desktop column + mobile under the branch
	// name; one copy is display:none), so the single deletable row yields two.
	if n := strings.Count(body, "branches/delete"); n != 2 {
		t.Errorf("delete forms = %d, want 2 (two responsive renderings of one row)", n)
	}
	// One primary: the create submit; Clone demoted.
	if n := strings.Count(body, "btn primary"); n != 1 {
		t.Errorf("primary buttons = %d, want 1", n)
	}
	if !strings.Contains(body, "Create branch") {
		t.Error("create form missing")
	}
}

func TestRepoBranchesAheadUnknownAndMore(t *testing.T) {
	ov := branchOverview()
	ov.AheadKnown, ov.More = false, true
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, (&branchWrites{}).deps())
	f.view.branches = ov
	body := f.mustGet("/demo/spindle/branches", http.StatusOK)
	if !strings.Contains(body, "ahead/behind unavailable") || !strings.Contains(body, "Showing the first 500") {
		t.Fatal("honest unknown/truncation notices missing")
	}
	if strings.Contains(body, "ahead ·") {
		t.Fatal("unknown counts must not render numbers")
	}
}

func TestRepoBranchesEmptyAndUnwired(t *testing.T) {
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{})
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := f.mustGet("/demo/spindle/branches", http.StatusOK)
	if !strings.Contains(body, "empty") || f.view.branchesN != 0 {
		t.Fatal("empty repo must guide, not read branches")
	}

	// Unwired browsing read: 503.
	g := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{})
	g.view.branches = branchOverview()
	g.s.env.API.Repo = struct{ api.RepoView }{g.view}
	if code, _ := g.get("/demo/spindle/branches"); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired read = %d", code)
	}

	// Read failure maps like every object read.
	h := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{})
	h.view.branchesErr = api.ErrNotFound
	if code, _ := h.get("/demo/spindle/branches"); code != http.StatusNotFound {
		t.Fatalf("404 = %d", code)
	}
	h.view.branchesErr = errors.New("boom")
	if code, _ := h.get("/demo/spindle/branches"); code != http.StatusServiceUnavailable {
		t.Fatalf("503 = %d", code)
	}
}

func TestRepoBranchesGates(t *testing.T) {
	w := &branchWrites{}
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	f.view.branches = branchOverview()
	f.reg.exists = map[string]bool{}
	if code, _ := f.get("/demo/spindle/branches"); code != http.StatusNotFound {
		t.Fatalf("invisible = %d", code)
	}

	// No identity surface + anonymous_read=false: 401 (the blob rule).
	g := newRepoFixture(t, false)
	g.view.branches = branchOverview()
	if code, _ := g.get("/demo/spindle/branches"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous read off = %d", code)
	}
}

func TestRepoBranchCreateWrites(t *testing.T) {
	w := &branchWrites{}
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	f.view.branches = branchOverview()
	rec := parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"topic"}, "from": {"feature/x"}}, nil)
	parityWantLocation(t, rec, "/demo/spindle/branches?flash=Branch+topic+created")
	if w.created != 1 || w.name != "topic" || w.from != "feature/x" {
		t.Fatalf("seam = %+v", w)
	}

	// Empty name: 400 page with the inline message; the seam is not called.
	rec = parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"  "}, "from": {"main"}}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Give the branch a name.") {
		t.Fatalf("empty name = %d", rec.Code)
	}
	if w.created != 1 {
		t.Fatal("empty name reached the service")
	}

	// Service refusals map onto status + text and re-render with the values.
	w.createErr = branches.ErrConflict
	rec = parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"topic"}, "from": {"main"}}, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "topic") {
		t.Fatalf("conflict = %d", rec.Code)
	}
	w.createErr = branches.ErrInvalid
	rec = parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"bad..name"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid = %d", rec.Code)
	}
}

func TestRepoBranchCreateGates(t *testing.T) {
	// Reader: 403 before the seam.
	w := &branchWrites{}
	f := parityServer(t, "none", auth.Principal{Name: "reader"}, w.deps())
	f.view.branches = branchOverview()
	rec := parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"x"}}, nil)
	if rec.Code != http.StatusForbidden || w.created != 0 {
		t.Fatalf("reader create = %d", rec.Code)
	}

	// Anonymous in token mode: the login interstitial (303), never a write.
	g := parityServer(t, "token", auth.Anonymous(), w.deps())
	g.view.branches = branchOverview()
	rec = parityPost(t, g, "/demo/spindle/branches", url.Values{"name": {"x"}}, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/login-required") {
		t.Fatalf("anon create = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Cross-origin refused.
	rec = parityPost(t, g, "/demo/spindle/branches", url.Values{"name": {"x"}}, map[string]string{"Origin": "http://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}

	// Unwired write seam: 503 for a writer.
	u := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{})
	u.view.branches = branchOverview()
	rec = parityPost(t, u, "/demo/spindle/branches", url.Values{"name": {"x"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired create = %d", rec.Code)
	}
}

func TestRepoBranchDeleteWrites(t *testing.T) {
	w := &branchWrites{}
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	f.view.branches = branchOverview()
	rec := parityPost(t, f, "/demo/spindle/branches/delete", url.Values{"name": {"feature/x"}}, nil)
	parityWantLocation(t, rec, "/demo/spindle/branches?flash=Branch+feature%2Fx+deleted")
	if w.deleted != 1 || w.name != "feature/x" {
		t.Fatalf("delete seam = %+v", w)
	}

	// The default branch's service refusal surfaces verbatim (403 + alert).
	w.deleteErr = branches.ErrForbidden
	rec = parityPost(t, f, "/demo/spindle/branches/delete", url.Values{"name": {"main"}}, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("default delete = %d", rec.Code)
	}
	w.deleteErr = branches.ErrNotFound
	rec = parityPost(t, f, "/demo/spindle/branches/delete", url.Values{"name": {"gone"}}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing delete = %d", rec.Code)
	}
	w.deleteErr = nil
	rec = parityPost(t, f, "/demo/spindle/branches/delete", url.Values{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty delete = %d", rec.Code)
	}

	// Reader: 403; unwired: 503.
	r := parityServer(t, "none", auth.Principal{Name: "reader"}, w.deps())
	r.view.branches = branchOverview()
	if rec := parityPost(t, r, "/demo/spindle/branches/delete", url.Values{"name": {"x"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("reader delete = %d", rec.Code)
	}
	u := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{})
	u.view.branches = branchOverview()
	if rec := parityPost(t, u, "/demo/spindle/branches/delete", url.Values{"name": {"x"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired delete = %d", rec.Code)
	}
}

func TestRepoBranchesAnonWriteLink(t *testing.T) {
	// In token mode an anonymous reader sees the sign-in link, not a form.
	f := parityServer(t, "token", auth.Anonymous(), (&branchWrites{}).deps())
	f.view.branches = branchOverview()
	body := f.mustGet("/demo/spindle/branches", http.StatusOK)
	if !strings.Contains(body, "Sign in") || strings.Contains(body, "Create branch</button>") {
		t.Fatalf("anon write affordance wrong: %.500s", body)
	}
}

func TestBranchErrStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want int
	}{
		{branches.ErrInvalid, http.StatusBadRequest},
		{branches.ErrUnauthorized, http.StatusUnauthorized},
		{branches.ErrForbidden, http.StatusForbidden},
		{branches.ErrNotFound, http.StatusNotFound},
		{branches.ErrConflict, http.StatusConflict},
		{branches.ErrUnavailable, http.StatusServiceUnavailable},
		{errors.New("other"), http.StatusServiceUnavailable},
	} {
		if got := branchErrStatus(tc.err); got != tc.want {
			t.Fatalf("branchErrStatus(%v) = %d", tc.err, got)
		}
	}
	if branchErrMessage(nil) != "" {
		t.Fatal("nil message must be empty")
	}
}

func TestBranchesRoleGateUsesIdentity(t *testing.T) {
	// RoleOf (identity) with an explicit reader role must hide the form and
	// refuse the write even though the principal carries Write.
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, RepoDeps{
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead },
		BranchCreate: func(context.Context, string, string, auth.Principal, string, string) error {
			t.Fatal("write seam called for a reader role")
			return nil
		},
	})
	f.view.branches = branchOverview()
	body := f.mustGet("/demo/spindle/branches", http.StatusOK)
	if strings.Contains(body, "Create branch") {
		t.Fatal("reader role must not see the create form")
	}
}

func TestRepoBranchesWriteEdgeBranches(t *testing.T) {
	w := &branchWrites{}
	// Invisible repo on CREATE/DELETE: the read gate answers before any seam.
	f := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	f.view.branches = branchOverview()
	f.reg.exists = map[string]bool{}
	if rec := parityPost(t, f, "/demo/spindle/branches", url.Values{"name": {"x"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("invisible create = %d", rec.Code)
	}
	if rec := parityPost(t, f, "/demo/spindle/branches/delete", url.Values{"name": {"x"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("invisible delete = %d", rec.Code)
	}

	// Cross-origin delete is refused by the shared preflight.
	g := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	g.view.branches = branchOverview()
	if rec := parityPost(t, g, "/demo/spindle/branches/delete", url.Values{"name": {"x"}}, map[string]string{"Origin": "http://evil.test"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete = %d", rec.Code)
	}

	// A malformed form body is a 400 on both writes (never a 500).
	h := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	h.view.branches = branchOverview()
	for _, target := range []string{"/demo/spindle/branches", "/demo/spindle/branches/delete"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("name=%zz"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+req.Host)
		rec := httptest.NewRecorder()
		h.s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s malformed form = %d", target, rec.Code)
		}
	}

	// A rejected create on an EMPTY repo re-renders the guide with the message.
	e := parityServer(t, "none", auth.Principal{Name: "jane", Write: true}, w.deps())
	e.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	w.createErr = branches.ErrInvalid
	rec := parityPost(t, e, "/demo/spindle/branches", url.Values{"name": {"x"}}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "empty") {
		t.Fatalf("empty-repo create error = %d", rec.Code)
	}
}
