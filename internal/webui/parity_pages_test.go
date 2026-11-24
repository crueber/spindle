package webui

// parity_pages_test.go — the §13 pin and §14 dependency page surfaces: the
// chips/controls, the triage floor on the page writes, and the fact that a
// row's toggle returns to the list it came from.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/server/auth"
)

func TestIssuesListPinnedRows(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := issueDeps()
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleTriage }
	d.IssuePins = func(context.Context, string, string) ([]int, error) { return []int{3}, nil }
	d.IssuesList = func(context.Context, string, string, auth.Principal, issues.ListFilter) (*issues.ListResult, error) {
		return &issues.ListResult{
			Issues: []issues.Card{{Num: 3, Kind: "issue", Title: "Crash on save", State: "open", Author: "alice", UpdatedAt: "2026-11-18T11:00:00Z"}},
			Pinned: []int{3},
		}, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `>pinned</span>`, `aria-label="Unpin issue #3"`,
		`action="/acme/widgets/issues/3/unpin"`, `name="next" value="/acme/widgets/issues"`)

	// A read-only viewer sees the chip but no toggle.
	f.principal = auth.Principal{Name: "reader"}
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead }
	f.deps(d)
	rec = f.get("/acme/widgets/issues")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `>pinned</span>`)
	wantNotContains(t, rec, `aria-label="Unpin issue #3"`)
}

func TestIssueDetailPinAndDependencies(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	d := issueDeps()
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleTriage }
	d.IssuePins = func(context.Context, string, string) ([]int, error) { return []int{3}, nil }
	d.IssueDeps = func(context.Context, string, string, int, auth.Principal) (*issues.DependencyView, error) {
		return &issues.DependencyView{
			Num:       3,
			Blocked:   true,
			Unblocked: false,
			BlockedBy: []issues.DependencyRef{{Num: 4, Title: "Schema migration", State: "open"}},
			Blocks:    []issues.DependencyRef{{Num: 5, Title: "Ship it", State: "closed"}},
		}, nil
	}
	f.deps(d)

	rec := f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		`aria-label="Unpin issue #3"`,
		`id="dependencies"`,
		`aria-label="Dependencies"`,
		`>blocked</span>`,
		`#4 Schema migration`, `#5 Ship it`,
		`action="/acme/widgets/issues/3/dependencies/blocked_by/4/delete"`,
		`action="/acme/widgets/issues/3/dependencies/blocks/5/delete"`,
		`aria-label="Link a dependency"`, `name="kind"`, `name="target"`,
	)

	// Read-only: the card is readable, the controls are gone.
	f.principal = auth.Principal{Name: "reader"}
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead }
	f.deps(d)
	rec = f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `aria-label="Dependencies"`, `#4 Schema migration`)
	wantNotContains(t, rec, `aria-label="Link a dependency"`, `/dependencies/blocks/5/delete`)

	// Unblocked state carries its own text chip.
	d.IssueDeps = func(context.Context, string, string, int, auth.Principal) (*issues.DependencyView, error) {
		return &issues.DependencyView{Num: 3, Unblocked: true}, nil
	}
	f.deps(d)
	rec = f.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `>unblocked</span>`, "Nothing blocks this issue.")
}

func TestIssuePinAndDependencyWrites(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	var pinned []bool
	var adds, removes int
	d := issueDeps()
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleTriage }
	d.IssuePin = func(_ context.Context, _, _ string, num int, _ auth.Principal, want bool) ([]int, bool, error) {
		if num != 3 {
			t.Fatalf("pin num = %d", num)
		}
		pinned = append(pinned, want)
		return []int{3}, true, nil
	}
	d.IssueDepAdd = func(_ context.Context, _, _ string, num int, _ auth.Principal, kind string, target int) (bool, error) {
		adds++
		if num != 3 || kind != issues.DepBlockedBy || target != 4 {
			t.Fatalf("add = %d %s %d", num, kind, target)
		}
		return true, nil
	}
	d.IssueDepRemove = func(_ context.Context, _, _ string, num int, _ auth.Principal, kind string, target int) (bool, error) {
		removes++
		if num != 3 || kind != issues.DepBlocks || target != 5 {
			t.Fatalf("remove = %d %s %d", num, kind, target)
		}
		return true, nil
	}
	f.deps(d)

	rec := f.post("/acme/widgets/issues/3/pin", url.Values{"next": {"/acme/widgets/issues"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/issues" {
		t.Fatalf("pin redirect = %q", loc)
	}
	rec = f.post("/acme/widgets/issues/3/unpin", url.Values{"next": {"/acme/widgets/issues?state=all"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/issues?state=all" {
		t.Fatalf("unpin redirect = %q", loc)
	}
	if len(pinned) != 2 || !pinned[0] || pinned[1] {
		t.Fatalf("pin calls = %v", pinned)
	}
	// An off-site "next" falls back to the issue page (no open redirect).
	rec = f.post("/acme/widgets/issues/3/pin", url.Values{"next": {"https://evil.test/x"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/acme/widgets/issues/3" {
		t.Fatalf("off-site next redirected to %q", loc)
	}

	rec = f.post("/acme/widgets/issues/3/dependencies", url.Values{"kind": {issues.DepBlockedBy}, "target": {"4"}})
	wantStatus(t, rec, http.StatusSeeOther)
	rec = f.post("/acme/widgets/issues/3/dependencies/blocks/5/delete", nil)
	wantStatus(t, rec, http.StatusSeeOther)
	if adds != 1 || removes != 1 {
		t.Fatalf("adds=%d removes=%d", adds, removes)
	}
	// A non-numeric target re-renders with the inline error, never a panic.
	rec = f.post("/acme/widgets/issues/3/dependencies", url.Values{"kind": {issues.DepBlockedBy}, "target": {"nope"}})
	wantStatus(t, rec, http.StatusBadRequest)
	wantContains(t, rec, "target must be an issue number")

	// Read role: refused before the seam.
	f.principal = auth.Principal{Name: "reader"}
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead }
	called := false
	d.IssuePin = func(context.Context, string, string, int, auth.Principal, bool) ([]int, bool, error) {
		called = true
		return nil, false, nil
	}
	f.deps(d)
	if rec := f.post("/acme/widgets/issues/3/pin", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read-role pin = %d", rec.Code)
	}
	if called {
		t.Fatal("read role reached the pin seam")
	}
}

// TestIssueLockModeSurfaces pins the collaborators-only lock rendering: the
// banner names the mode, a write+ viewer keeps a live composer, a reader sees
// it disabled, and the (pre-lock) control carries the mode select.
func TestIssueLockModeSurfaces(t *testing.T) {
	newFixture := func(t *testing.T, role identity.Role, principal auth.Principal, locked bool) *collabFixture {
		f := newCollabFixture(t)
		f.principal = principal
		d := issueDeps()
		d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return role }
		d.IssuesGet = func(context.Context, string, string, int, auth.Principal, int, int) (*issues.ThreadView, error) {
			return &issues.ThreadView{
				Thread:     testThread(),
				Locked:     locked,
				LockMode:   "collaborators",
				LockReason: "keep it focused",
				Events:     []*issues.Event{{Seq: 0, Type: issues.EventOpened, Actor: "alice", At: "2026-11-18T10:00:00Z", Body: ptr("body")}},
			}, nil
		}
		f.deps(d)
		return f
	}

	// Maintainer on an unlocked thread: the lock control offers both modes.
	maint := newFixture(t, identity.RoleMaintain, auth.Principal{Name: "alice", Write: true}, false)
	rec := maint.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, `name="mode"`, `<option value="all" selected>`, `<option value="collaborators">`)

	// Write+ on a collaborators-only lock: banner names the mode, composer
	// stays live.
	writer := newFixture(t, identity.RoleWrite, auth.Principal{Name: "alice", Write: true}, true)
	rec = writer.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Locked to collaborators: keep it focused")
	if strings.Contains(bodyText(rec), `disabled aria-disabled="true"`) {
		t.Fatal("a write+ viewer must keep a live composer under a collaborators-only lock")
	}

	// A reader is refused the same lock.
	reader := newFixture(t, identity.RoleRead, auth.Principal{Name: "bob"}, true)
	rec = reader.get("/acme/widgets/issues/3")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Locked to collaborators: keep it focused", `disabled aria-disabled="true"`)
	wantNotContains(t, rec, `name="mode"`)
}
