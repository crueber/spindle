package webui

// a11y_test.go — the accessibility floor, asserted on rendered HTML rather
// than templates: the skip link, the one-h1 rule (with markdown demoted so a
// README never competes with the page h1), table scopes and names, the
// account-menu name, and the busy/live contract on the merge-task fragment.
// Substring assertions are honest here because html/template escapes every
// value; counting "<h1" openings is the page's whole population of top-level
// headings (markdown demotion keeps rendered bodies at h2+).
//
// This file pins the semantics AGENTS.md's accessibility section makes
// binding: a change that drops the skip link, doubles the h1, or loses the
// merge fragment's aria-busy fails here.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
)

func a11yH1Count(body string) int {
	return strings.Count(body, "<h1")
}

// TestBaseChromeSkipLinkAndLandmarks: every page opens with the skip link
// and carries the four landmarks (header/nav/main/footer) plus lang=en.
func TestBaseChromeSkipLinkAndLandmarks(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref: "refs/heads/main",
		Entries: []api.TreeEntry{
			{Name: "main.go", Type: "blob", Mode: "100644", Size: 12, SHA: strings.Repeat("c", 40)},
		},
		Readme: &api.Readme{Name: "README.md", Contents: "# Hello"},
	}
	for _, path := range []string{"/demo/spindle", "/demo/spindle/tree/main", "/demo/spindle/commits"} {
		body := f.mustGet(path, http.StatusOK)
		if !strings.Contains(body, `<a class="skip-link" href="#app">`) {
			t.Errorf("%s: no skip link to #app", path)
		}
		for _, landmark := range []string{"<header", "<nav", `<main id="app"`, "<footer"} {
			if !strings.Contains(body, landmark) {
				t.Errorf("%s: missing landmark %q", path, landmark)
			}
		}
		if !strings.Contains(body, `<html lang="en"`) {
			t.Errorf("%s: html lang missing", path)
		}
	}
}

// TestRepoPagesOwnExactlyOneH1: the repo masthead is the one h1; the README's
// markdown h1 demotes to h2 under it (the blob page adds its filename h2).
func TestRepoPagesOwnExactlyOneH1(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref: "refs/heads/main",
		Entries: []api.TreeEntry{
			{Name: "main.go", Type: "blob", Mode: "100644", Size: 12, SHA: strings.Repeat("c", 40)},
		},
		Readme: &api.Readme{Name: "README.md", Contents: "# Hello"},
	}
	for _, path := range []string{"/demo/spindle", "/demo/spindle/tree/main", "/demo/spindle/blob/main/main.go"} {
		if body := f.mustGet(path, http.StatusOK); a11yH1Count(body) != 1 {
			t.Errorf("%s: h1 count = %d, want 1 (markdown headings must demote)", path, a11yH1Count(body))
		}
	}
	if body := f.mustGet("/demo/spindle", http.StatusOK); !strings.Contains(body, "<h2>Hello</h2>") {
		t.Error("README h1 did not demote to h2")
	}
}

// TestAccountPageHeadingsAreH1: pages with no repo masthead promote their own
// title to h1 instead of starting the outline at h2.
func TestAccountPageHeadingsAreH1(t *testing.T) {
	for _, page := range []string{"/keys", "/invitations", "/notifications"} {
		s := identityServer(t, "oidc", auth.Principal{Name: "alice@example.com", Write: true})
		rec := publicGet(s, page)
		publicWantStatus(t, rec, http.StatusOK)
		if n := a11yH1Count(rec.Body.String()); n != 1 {
			t.Errorf("%s: h1 count = %d, want 1", page, n)
		}
	}
}

// TestTreeTablesScopedAndNamed: the file tree names itself (a screen reader
// lands on it blind) and every column header carries scope="col".
func TestTreeTablesScopedAndNamed(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref: "refs/heads/main",
		Entries: []api.TreeEntry{
			{Name: "main.go", Type: "blob", Mode: "100644", Size: 12, SHA: strings.Repeat("c", 40), CommitTime: "2026-01-02T03:04:05Z"},
		},
		Readme: &api.Readme{Name: "README.md", Contents: "# Hello"},
	}
	body := f.mustGet("/demo/spindle/tree/main", http.StatusOK)
	if !strings.Contains(body, `<table class="data-table tree-table" aria-label="files">`) {
		t.Error("tree table has no accessible name")
	}
	if got := strings.Count(body, `th scope="col"`); got < 4 {
		t.Errorf("tree table scoped headers = %d, want >= 4", got)
	}
}

// TestPullDetailTitleIsH2: the PR conversation's title is an h2 under the
// repo masthead h1 (the issue detail's shape), never a second h1.
func TestPullDetailTitleIsH2(t *testing.T) {
	f := newCollabFixture(t)
	body := f.get("/acme/widgets/pull/5").Body.String()
	if strings.Contains(body, `<h1 class="min-w-0 text-xl font-semibold tracking-tight">`) {
		t.Error("PR detail renders its title as a second h1")
	}
	if n := a11yH1Count(body); n != 1 {
		t.Errorf("PR detail h1 count = %d, want 1 (the repo masthead)", n)
	}
	if !strings.Contains(body, `<h2 class="min-w-0 text-xl font-semibold tracking-tight">`) {
		t.Error("PR detail title is not an h2")
	}
}

// TestMergeTaskFragmentAnnouncesState: the polled merge-task fragment is a
// polite live region and marks itself aria-busy while the task runs.
func TestMergeTaskFragmentAnnouncesState(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada"}
	f.deps(CollabDeps{
		RoleOf: func(context.Context, string, string, auth.Principal) identity.Role {
			return identity.RoleMaintain
		},
		MergeTask: func(string, string) *pulls.TaskRecord {
			return &pulls.TaskRecord{ID: "t1", Kind: "pull-merge", State: "running", Progress: []string{"fetching"}}
		},
	})
	rec := f.get("/acme/widgets/pull/5/merge/task")
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, `aria-live="polite"`) || !strings.Contains(body, `aria-label="Merge task"`) {
		t.Error("merge fragment is not a labelled polite live region")
	}
	if !strings.Contains(body, `aria-busy="true"`) {
		t.Error(`running merge fragment missing aria-busy="true"`)
	}
}

// TestHeaderTargetsArePadded: the header's standalone links are padded to the
// button-row height (24px+ touch targets, WCAG 2.5.8), not bare 20px text.
func TestHeaderTargetsArePadded(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	body := f.mustGet("/demo/spindle", http.StatusOK)
	for _, want := range []string{
		`<a href="/" class="brand shrink-0 py-1 text-[15px]">spindle</a>`,
		`<a href="/explore" class="inline-flex items-center py-1">explore</a>`,
		`<a href="/api" class="nav-link inline-flex items-center py-1">API</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header control missing 24px target padding: %q", want)
		}
	}
}
