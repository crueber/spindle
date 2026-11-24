package webui

// parity_forms_test.go — §15 template discovery surface in the new-issue
// composer: the chooser, the ?template= prefill, and the metadata hand-off
// through CreateIssue (filtered to what the author may apply).

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/server/auth"
)

// templateFakeIssues is formsFakeIssues plus the optional §15 read and a
// capturing create.
type templateFakeIssues struct {
	formsFakeIssues
	doc    *issues.TemplateDoc
	meta   *issues.IssueMeta
	create int
}

func (f *templateFakeIssues) Templates(context.Context, string, string) (*issues.TemplateDoc, error) {
	return f.doc, nil
}

func (f *templateFakeIssues) CreateIssue(_ context.Context, _, _ string, _ auth.Principal, title, body string, meta *issues.IssueMeta) (*issues.Thread, *issues.Event, error) {
	f.create++
	f.lastIssue = [2]string{title, body}
	f.meta = meta
	return &issues.Thread{Num: 9, Title: title}, nil, nil
}

// templateFakeIdentity answers carol below triage (the dropped-assignee case)
// and otherwise defers to the shared fake.
type templateFakeIdentity struct{ *formsFakeIdentity }

func (f templateFakeIdentity) Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if normPrincipal(p.Name) == "carol" {
		return identity.RoleRead, nil
	}
	return f.formsFakeIdentity.Resolve(ctx, owner, repo, p)
}

func templateDoc() *issues.TemplateDoc {
	return &issues.TemplateDoc{Version: 1, Commit: "abc", Templates: []issues.IssueTemplate{
		{
			Path: ".spindle/ISSUE_TEMPLATE/bug.md", Name: "Bug report", About: "Something is broken",
			Title: "[bug] ", Labels: []string{"bug", "missing"}, Assignees: []string{"carol"},
			Body: "Steps to reproduce\n",
		},
		{Path: ".spindle/ISSUE_TEMPLATE/feat.md", Name: "Feature request", Body: "Why?\n"},
	}}
}

func TestFormsIssueTemplateChooserAndPrefill(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	iss := &templateFakeIssues{doc: templateDoc(), formsFakeIssues: formsFakeIssues{
		labels: []issues.Label{{Name: "bug", Color: "d73a4a"}},
	}}
	formsUse(t, FormsDeps{Identity: templateFakeIdentity{&formsFakeIdentity{role: identity.RoleTriage}}, Issues: iss})

	w := h.request(t, http.MethodGet, "/alice/thing/issues/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Issue templates")
	formsExpectContains(t, w, "Bug report")
	formsExpectContains(t, w, "Feature request")

	w = h.request(t, http.MethodGet, "/alice/thing/issues/new?template=.spindle%2FISSUE_TEMPLATE%2Fbug.md", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Steps to reproduce")
	formsExpectContains(t, w, "Something is broken")
	formsExpectContains(t, w, "start blank")
	formsExpectContains(t, w, `name="template" value=".spindle/ISSUE_TEMPLATE/bug.md"`)
	formsExpectContains(t, w, "Template labels:")
	// The title prefills with the template's title prefix.
	if got := formsValue(t, w, "issue-title"); got != "[bug] " {
		t.Fatalf("issue-title = %q, want the template title", got)
	}

	// Unknown selection: an inline error, never a silent blank.
	w = h.request(t, http.MethodGet, "/alice/thing/issues/new?template=.spindle%2FISSUE_TEMPLATE%2Fnope.md", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "unknown issue template")

	// POST with the selection applies only metadata that exists / resolves.
	iss.labels = []issues.Label{{Name: "bug", Color: "d73a4a"}}
	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{
		"title": {"[bug] crash"}, "body": {"Steps"}, "template": {".spindle/ISSUE_TEMPLATE/bug.md"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.create != 1 {
		t.Fatalf("create calls = %d", iss.create)
	}
	if iss.meta == nil || len(iss.meta.Labels) != 1 || iss.meta.Labels[0] != "bug" {
		t.Fatalf("meta labels = %+v (the unknown label must be dropped)", iss.meta)
	}
	// carol has no triage role in this fake → assignees dropped, and the
	// landing page says what was not applied.
	if len(iss.meta.Assignees) != 0 {
		t.Fatalf("meta assignees = %v", iss.meta.Assignees)
	}
	if loc := w.Header().Get("Location"); loc == "" || len(loc) < len("/alice/thing/issues/9?template_skipped=") {
		t.Fatalf("location = %q (expected the skipped-metadata notice)", loc)
	}
}

func TestFormsIssueTemplateReaderDropsMetadata(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	iss := &templateFakeIssues{doc: templateDoc(), formsFakeIssues: formsFakeIssues{
		labels: []issues.Label{{Name: "bug", Color: "d73a4a"}},
	}}
	formsUse(t, FormsDeps{Identity: templateFakeIdentity{&formsFakeIdentity{role: identity.RoleRead}}, Issues: iss})

	w := h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{
		"title": {"[bug] crash"}, "body": {"Steps"}, "template": {".spindle/ISSUE_TEMPLATE/bug.md"},
	}, "reader", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.create != 1 {
		t.Fatalf("create calls = %d (a reader may still file from a template)", iss.create)
	}
	if iss.meta != nil {
		t.Fatalf("reader applied metadata: %+v", iss.meta)
	}
	if loc := w.Header().Get("Location"); !contains(loc, "template_skipped=") {
		t.Fatalf("location = %q", loc)
	}

	// A template that vanished between render and submit is a 400, not a
	// create with a half-known selection.
	iss.doc = &issues.TemplateDoc{}
	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{
		"title": {"x"}, "body": {"y"}, "template": {".spindle/ISSUE_TEMPLATE/bug.md"},
	}, "reader", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "no longer exists")
	if iss.create != 1 {
		t.Fatalf("create calls after vanished template = %d", iss.create)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
