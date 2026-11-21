package webui

// forms_test.go — the create/import/manage form family.
//
// The harness builds a Server with ONLY this group's handlers and templates
// (the real parseTemplates globs every group's files; during the migration
// parallel groups own sibling files, so the group test stays isolated).
// Principals arrive through the X-Test-User header: anon | writer | reader |
// admin. Fakes implement the FormsDeps interfaces; no store or network.

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/mirror"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/releases"
	"github.com/crueber/spindle/internal/repoimport"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// --- fakes -------------------------------------------------------------------

type formsFakeIdentity struct {
	orgs           []string
	orgsErr        error
	role           identity.Role
	roles          map[string]identity.Role
	team           *identity.Team
	teamErr        error
	orgOwnerErr    *auth.AuthError
	createOwnerErr *auth.AuthError
	orgErr         error
	org            *identity.Org
	lastOrg        [4]string
	lastAdded      string
	lastRemoved    string
	memberErr      error
}

func (f *formsFakeIdentity) MemberOrgsFor(context.Context, auth.Principal) ([]string, error) {
	return f.orgs, f.orgsErr
}
func (f *formsFakeIdentity) CheckCreateOwner(context.Context, string, auth.Principal) *auth.AuthError {
	return f.createOwnerErr
}
func (f *formsFakeIdentity) CheckOrgOwner(context.Context, string, auth.Principal) *auth.AuthError {
	return f.orgOwnerErr
}
func (f *formsFakeIdentity) CreateOrg(_ context.Context, org, display, desc, creator string) (*identity.Org, error) {
	f.lastOrg = [4]string{org, display, desc, creator}
	if f.orgErr != nil {
		return nil, f.orgErr
	}
	if f.org != nil {
		return f.org, nil
	}
	return &identity.Org{Org: org, DisplayName: display, Description: desc}, nil
}
func (f *formsFakeIdentity) GetTeam(context.Context, string, string) (*identity.Team, store.Version, error) {
	return f.team, "", f.teamErr
}
func (f *formsFakeIdentity) SetTeamMember(_ context.Context, _, _, principal string) (*identity.Team, error) {
	f.lastAdded = principal
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	return f.team, nil
}
func (f *formsFakeIdentity) RemoveTeamMember(_ context.Context, _, _, principal string) (*identity.Team, error) {
	f.lastRemoved = principal
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	return f.team, nil
}
func (f *formsFakeIdentity) Resolve(_ context.Context, owner, repo string, _ auth.Principal) (identity.Role, *identity.AccessDoc) {
	if f.roles != nil {
		if r, ok := f.roles[owner+"/"+repo]; ok {
			return r, nil
		}
	}
	return f.role, nil
}

type formsFakeIssues struct {
	labels         []issues.Label
	labelsErr      error
	labelCreateErr error
	labelDeleteErr error
	affected       int
	lastLabel      [3]string
	deletedLabel   string
	packsCreated   []string

	milestones    []*issues.Milestone
	milestonesErr error
	issuesByMS    map[string][]issues.Card
	issueCreateN  int
	issueErr      error
	lastIssue     [2]string
	milestoneErr  error
	lastState     string
	deletedMS     string
	lastMSTitle   string
}

func (f *formsFakeIssues) CreateIssue(_ context.Context, _, _ string, _ auth.Principal, title, body string) (*issues.Thread, *issues.Event, error) {
	f.lastIssue = [2]string{title, body}
	if f.issueErr != nil {
		return nil, nil, f.issueErr
	}
	return &issues.Thread{Num: f.issueCreateN, Title: title}, nil, nil
}
func (f *formsFakeIssues) ListIssues(_ context.Context, _, _ string, _ auth.Principal, fl issues.ListFilter) (*issues.ListResult, error) {
	return &issues.ListResult{Issues: f.issuesByMS[fl.Milestone]}, nil
}
func (f *formsFakeIssues) ListLabels(context.Context, string, string) ([]issues.Label, error) {
	return f.labels, f.labelsErr
}
func (f *formsFakeIssues) CreateLabel(_ context.Context, _, _ string, _ auth.Principal, name, color, desc string) (*issues.Label, error) {
	f.lastLabel = [3]string{name, color, desc}
	if f.labelCreateErr != nil {
		return nil, f.labelCreateErr
	}
	f.packsCreated = append(f.packsCreated, name)
	return &issues.Label{Name: name, Color: color, Description: desc}, nil
}
func (f *formsFakeIssues) DeleteLabel(_ context.Context, _, _ string, _ auth.Principal, name string) (int, error) {
	f.deletedLabel = name
	return f.affected, f.labelDeleteErr
}
func (f *formsFakeIssues) ListMilestones(context.Context, string, string) ([]*issues.Milestone, error) {
	return f.milestones, f.milestonesErr
}
func (f *formsFakeIssues) CreateMilestone(_ context.Context, _, _ string, _ auth.Principal, title, desc string, _ *string) (*issues.Milestone, error) {
	f.lastMSTitle = title
	return &issues.Milestone{ID: "000001", Title: title}, f.milestoneErr
}
func (f *formsFakeIssues) UpdateMilestone(_ context.Context, _, _ string, _ auth.Principal, id string, _, _, _, state *string) (*issues.Milestone, error) {
	if state != nil {
		f.lastState = *state
	}
	return &issues.Milestone{ID: id}, f.milestoneErr
}
func (f *formsFakeIssues) DeleteMilestone(_ context.Context, _, _ string, _ auth.Principal, id string) error {
	f.deletedMS = id
	return f.milestoneErr
}

type formsFakePulls struct {
	emptyChild bool
	forkChild  string
	forkErr    error
	lastFork   pulls.ForkInput
	openNum    int
	openErr    error
	lastOpen   pulls.OpenInput
	lastBase   string
	forks      []pulls.ForkListEntry
	forksErr   error
}

func (f *formsFakePulls) ListForks(_ context.Context, _, _ string, _ auth.Principal, _ string, _ int) (*pulls.ForkListPage, error) {
	if f.forksErr != nil {
		return nil, f.forksErr
	}
	return &pulls.ForkListPage{Forks: f.forks}, nil
}

func (f *formsFakePulls) StartFork(_ context.Context, _, _ string, _ auth.Principal, in pulls.ForkInput) (*pulls.TaskRecord, string, error) {
	f.lastFork = in
	if f.forkErr != nil {
		return nil, "", f.forkErr
	}
	child := f.forkChild
	if child == "" && !f.emptyChild {
		child = in.TargetOwner + "/" + in.Name
	}
	return &pulls.TaskRecord{ID: "t1"}, child, nil
}
func (f *formsFakePulls) OpenPR(_ context.Context, owner, repo string, _ auth.Principal, in pulls.OpenInput, _ string) (*pulls.Thread, *pulls.PRDoc, error) {
	f.lastOpen = in
	f.lastBase = owner + "/" + repo
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	return &pulls.Thread{Num: f.openNum}, nil, nil
}

type formsFakeReleases struct {
	autodraft    *releases.Autodraft
	autodraftErr error
	putErr       error
	lastTag      string
	lastIn       releases.ReleaseInput
}

func (f *formsFakeReleases) PutRelease(_ context.Context, _, _ string, _ auth.Principal, tag string, in releases.ReleaseInput, _ store.Version) (*releases.Release, bool, error) {
	f.lastTag = tag
	f.lastIn = in
	if f.putErr != nil {
		return nil, false, f.putErr
	}
	return &releases.Release{Tag: tag}, true, nil
}
func (f *formsFakeReleases) Autodraft(context.Context, string, string, auth.Principal, string, string) (*releases.Autodraft, error) {
	return f.autodraft, f.autodraftErr
}

type formsFakeImport struct {
	res     *repoimport.BeginResult
	rec     *wal.TaskRecord
	err     error
	state   repoimport.TaskState
	stateOK bool
	lastURL string
}

func (f *formsFakeImport) Begin(_ context.Context, _ auth.Principal, params repoimport.Params, _ string) (*repoimport.BeginResult, *wal.TaskRecord, error) {
	f.lastURL = params.Source.URL
	return f.res, f.rec, f.err
}

func (f *formsFakeImport) State(string) (repoimport.TaskState, bool) { return f.state, f.stateOK }

type formsFakeMirror struct{ synced []string }

func (f *formsFakeMirror) SyncAsync(_ context.Context, owner, name, _ string, _ bool) string {
	f.synced = append(f.synced, owner+"/"+name)
	return "sync-1"
}

type formsFakeRegistry struct {
	names     []string
	createErr error
	created   []string
}

func (f *formsFakeRegistry) Owners(context.Context) ([]string, error) { return nil, nil }
func (f *formsFakeRegistry) Repos(context.Context, string) ([]string, error) {
	return f.names, nil
}
func (f *formsFakeRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) { return nil, nil }
func (f *formsFakeRegistry) Exists(context.Context, git.RepoId) (bool, error)        { return false, nil }
func (f *formsFakeRegistry) Create(_ context.Context, id git.RepoId, _ git.ObjectFormat) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, id.String())
	return nil
}
func (f *formsFakeRegistry) Delete(context.Context, git.RepoId) error { return nil }

type formsFakeRepoView struct {
	summaryErr error
	refErr     error
	summary    api.SummaryData
	branches   []api.Ref
	tags       []api.Ref
	commits    map[string]api.CommitPage
}

func (f *formsFakeRepoView) Sync(context.Context, git.RepoId, api.SyncLevel) error { return nil }
func (f *formsFakeRepoView) Resolve(context.Context, git.RepoId, string) (api.Resolution, error) {
	return api.Resolution{}, nil
}
func (f *formsFakeRepoView) Head(context.Context, git.RepoId) (api.Ref, bool, error) {
	if f.summary.Head != nil {
		return *f.summary.Head, true, nil
	}
	return api.Ref{}, false, nil
}
func (f *formsFakeRepoView) RefList(_ context.Context, _ git.RepoId, ns string, _ api.RefQuery) ([]api.Ref, bool, error) {
	if f.refErr != nil {
		return nil, false, f.refErr
	}
	if ns == "tags" {
		return f.tags, false, nil
	}
	return f.branches, false, nil
}
func (f *formsFakeRepoView) Tree(context.Context, git.RepoId, string, string) (api.TreeResult, error) {
	return api.TreeResult{}, nil
}
func (f *formsFakeRepoView) Blob(context.Context, git.RepoId, string, string, bool) (api.BlobResult, error) {
	return api.BlobResult{}, nil
}
func (f *formsFakeRepoView) Commits(_ context.Context, _ git.RepoId, ref, _ string, _, _ int) (api.CommitPage, error) {
	if page, ok := f.commits[ref]; ok {
		return page, nil
	}
	return api.CommitPage{}, errors.New("not found")
}
func (f *formsFakeRepoView) Commit(context.Context, git.RepoId, string) (api.CommitDetail, error) {
	return api.CommitDetail{}, nil
}
func (f *formsFakeRepoView) Summary(context.Context, git.RepoId) (api.SummaryData, error) {
	return f.summary, f.summaryErr
}
func (f *formsFakeRepoView) Overview(context.Context, git.RepoId) (api.OverviewData, error) {
	return api.OverviewData{}, nil
}
func (f *formsFakeRepoView) Settings(context.Context, git.RepoId) (api.SettingsDoc, error) {
	return api.SettingsDoc{}, nil
}
func (f *formsFakeRepoView) PublishSettings(context.Context, git.RepoId, []byte, string, string) (uint64, error) {
	return 0, nil
}
func (f *formsFakeRepoView) SettingsHistory(context.Context, git.RepoId) (api.SettingsHistory, error) {
	return api.SettingsHistory{}, nil
}
func (f *formsFakeRepoView) HeadSeq(context.Context, git.RepoId) (uint64, error) { return 0, nil }
func (f *formsFakeRepoView) PushHistory(context.Context, git.RepoId, int) ([]api.PushRecord, error) {
	return nil, nil
}

// --- harness -----------------------------------------------------------------

type formsHarness struct {
	s   *Server
	cfg *config.Config
}

func newFormsHarness(t *testing.T, mode string) *formsHarness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = mode
	cfg.Server.Auth.AnonymousRead = true
	cfg.Import.AllowPrivateNetworks = true
	cfg.Import.URLAllowlist = []string{"github.com"}

	s := &Server{env: Env{Cfg: cfg, API: &api.Env{}}}
	s.auth = pages.Auth{
		Authenticate: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			switch r.Header.Get("X-Test-User") {
			case "writer":
				return auth.Principal{Name: "alice", Write: true}, nil
			case "reader":
				return auth.Principal{Name: "bob"}, nil
			case "admin":
				return auth.Principal{Name: "root", Write: true, Admin: true}, nil
			default:
				return auth.Anonymous(), nil
			}
		},
	}
	formsParseTemplates(t, s)
	mountForms(s)
	return &formsHarness{s: s, cfg: cfg}
}

// formsParseTemplates parses base + this group's partials + one group page at
// a time (isolated from sibling groups' template files).
func formsParseTemplates(t *testing.T, s *Server) {
	t.Helper()
	s.tpl = map[string]*template.Template{}
	groupPages, err := fs.Glob(tplFS, "templates/pages/forms/*.html")
	if err != nil || len(groupPages) == 0 {
		t.Fatalf("forms templates: %v", err)
	}
	parts, err := fs.Glob(tplFS, "templates/partials/forms_*.html")
	if err != nil {
		t.Fatalf("forms partials: %v", err)
	}
	for _, p := range groupPages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/pages/"), ".html")
		files := append([]string{
			"templates/base.html",
			"templates/partials/header.html",
			"templates/partials/footer.html",
			// The layout's repository chrome (12_web_ui.md §2.2.1).
			"templates/partials/repo_header.html",
			"templates/partials/repo_tabs.html",
			p,
		}, parts...)
		tpl, perr := template.New("base").Funcs(s.funcs()).ParseFS(tplFS, files...)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		s.tpl[name] = tpl
	}
	// The shared error page is what errorPage renders. The FOUNDATION's
	// error.html currently compares an int through the string-only `eq`
	// template func (helpers.go:16), so executing it fails and render.go
	// recurses (render.go:111 → errorPage → render). Until that lands as a
	// bool field, the harness stubs the error set so failures stay visible.
	errTpl, perr := template.New("base").Funcs(s.funcs()).Parse(
		`{{define "base"}}<html><body>{{template "content" .}}</body></html>{{end}}` +
			`{{define "content"}}<h1>error</h1><p>{{.Data.Message}}</p>{{end}}`)
	if perr != nil {
		t.Fatalf("parse error stub: %v", perr)
	}
	s.tpl["error"] = errTpl
}

func (h *formsHarness) request(t *testing.T, method, path string, form url.Values, who string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if who != "" {
		r.Header.Set("X-Test-User", who)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	// A browser sends Origin on every state-changing request; model that so
	// the fail-closed same-origin gate is exercised, not bypassed. Tests that
	// want a cross-origin or headerless post set the header explicitly.
	if method != http.MethodGet && method != http.MethodHead &&
		r.Header.Get("Origin") == "" && r.Header.Get("Referer") == "" {
		r.Header.Set("Origin", "http://"+r.Host)
	}
	w := httptest.NewRecorder()
	h.s.Serve(w, r)
	return w
}

func formsUse(t *testing.T, d FormsDeps) {
	t.Helper()
	prev := formsDeps
	formsDeps = d
	t.Cleanup(func() { formsDeps = prev })
}

func formsExpectStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, want, formsTruncate(w.Body.String()))
	}
}

func formsTruncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func formsExpectContains(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("body does not contain %q (body: %s)", want, formsTruncate(w.Body.String()))
	}
}

func formsExpectLocation(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := w.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// --- HTML helpers ------------------------------------------------------------

func formsParseHTML(t *testing.T, w *httptest.ResponseRecorder) *html.Node {
	t.Helper()
	n, err := html.Parse(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	return n
}

func formsFindID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode && formsAttr(n, "id") == id {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := formsFindID(c, id); got != nil {
			return got
		}
	}
	return nil
}

func formsAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func formsText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func formsFindTag(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := formsFindTag(c, tag); got != nil {
			return got
		}
	}
	return nil
}

func formsOptions(t *testing.T, w *httptest.ResponseRecorder, id string) []string {
	t.Helper()
	root := formsParseHTML(t, w)
	sel := formsFindID(root, id)
	if sel == nil {
		t.Fatalf("select #%s not found", id)
	}
	var out []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "option":
				out = append(out, formsAttr(c, "value"))
			case "optgroup":
				walk(c)
			}
		}
	}
	walk(sel)
	return out
}

func formsValue(t *testing.T, w *httptest.ResponseRecorder, id string) string {
	t.Helper()
	root := formsParseHTML(t, w)
	n := formsFindID(root, id)
	if n == nil {
		t.Fatalf("element #%s not found", id)
	}
	return formsAttr(n, "value")
}

func formsHasID(t *testing.T, w *httptest.ResponseRecorder, id string) bool {
	t.Helper()
	return formsFindID(formsParseHTML(t, w), id) != nil
}

// --- route / gate matrix -----------------------------------------------------

func TestFormsPagesRender(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{
		Identity: &formsFakeIdentity{role: identity.RoleAdmin, team: &identity.Team{Org: "alice", Slug: "core", Name: "Core"}},
		Issues:   &formsFakeIssues{},
		Pulls:    &formsFakePulls{},
		Releases: &formsFakeReleases{},
		Import:   &formsFakeImport{},
		Mirror:   &formsFakeMirror{},
	})
	paths := []struct{ path, marker string }{
		{"/new", "New repository"},
		{"/import", "Import repository"},
		{"/orgs/new", "New organization"},
		{"/alice/thing/fork", "Fork alice/thing"},
		{"/alice/thing/releases/new", "New release"},
		{"/alice/thing/issues/new", "New issue"},
		{"/alice/thing/pulls/new", "New pull request"},
		{"/alice/teams/core", "alice/core"},
		{"/alice/thing/labels", "Labels"},
		{"/alice/thing/milestones", "Milestones"},
	}
	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
			w := h.request(t, http.MethodGet, tc.path, nil, "writer", nil)
			formsExpectStatus(t, w, http.StatusOK)
			formsExpectContains(t, w, tc.marker)
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("content-type = %q", ct)
			}
		})
	}
}

// --- /new --------------------------------------------------------------------

func TestFormsNewGet(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{orgs: []string{"acme", "zeta"}, role: identity.RoleAdmin}})
	w := h.request(t, http.MethodGet, "/new?owner=acme", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if got := formsOptions(t, w, "new-owner"); len(got) != 3 || got[0] != "alice" || got[1] != "acme" {
		t.Fatalf("owner options = %v", got)
	}
	if got := formsOptions(t, w, "new-format"); len(got) != 2 || got[0] != "sha1" {
		t.Fatalf("format options = %v", got)
	}
	if got := formsOptions(t, w, "new-visibility"); len(got) != 3 || got[2] != "private" {
		t.Fatalf("visibility options = %v", got)
	}
	if !formsHasID(t, w, "new-name-error") {
		t.Fatal("missing reserved-height name error slot")
	}
	formsExpectContains(t, w, "/import")
}

func TestFormsNewPostCreates(t *testing.T) {
	h := newFormsHarness(t, "none")
	reg := &formsFakeRegistry{}
	h.s.env.API = &api.Env{Repos: reg}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{
		"owner": {"alice"}, "name": {"thing"}, "format": {"sha1"}, "visibility": {"public"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/thing")
	if len(reg.created) != 1 || reg.created[0] != "alice/thing" {
		t.Fatalf("created = %v", reg.created)
	}
}

func TestFormsNewPostValidationKeepsValues(t *testing.T) {
	h := newFormsHarness(t, "none")
	reg := &formsFakeRegistry{}
	h.s.env.API = &api.Env{Repos: reg}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{
		"owner": {"alice"}, "name": {"bad name!"}, "format": {"sha1"}, "visibility": {"public"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid name")
	if got := formsValue(t, w, "new-name"); got != "bad name!" {
		t.Fatalf("preserved name = %q", got)
	}
	if len(reg.created) != 0 {
		t.Fatalf("created = %v", reg.created)
	}
}

func TestFormsNewPostConflictHasWinnerLink(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{createErr: api.ErrExists}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{
		"owner": {"alice"}, "name": {"thing"}, "format": {"sha1"}, "visibility": {"public"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, "take me there")
	formsExpectContains(t, w, `href="/alice/thing"`)
}

func TestFormsNewPostWriteRequired(t *testing.T) {
	h := newFormsHarness(t, "none")
	reg := &formsFakeRegistry{}
	h.s.env.API = &api.Env{Repos: reg}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{"owner": {"alice"}, "name": {"thing"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "write access required")
	if len(reg.created) != 0 {
		t.Fatalf("created = %v", reg.created)
	}
}

func TestFormsCrossOriginPostRejected(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer",
		map[string]string{"Origin": "https://evil.example"})
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "cross-origin request refused")
}

func TestFormsRefererSameOriginAllowed(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	r := httptest.NewRequest(http.MethodPost, "/orgs/new", strings.NewReader("org=acme"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-User", "writer")
	r.Header.Set("Referer", "http://example.com/orgs/new")
	w := httptest.NewRecorder()
	h.s.Serve(w, r)
	// Referer host is example.com, request host is example.com → allowed.
	formsExpectStatus(t, w, http.StatusSeeOther)
}

func TestFormsAnonymousWriteRedirectsToInterstitial(t *testing.T) {
	h := newFormsHarness(t, "token")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/orgs/new?x=1", url.Values{"org": {"acme"}}, "", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login-required?next=%2Forgs%2Fnew%3Fx%3D1&action=") {
		t.Fatalf("Location = %q", loc)
	}
	if !strings.Contains(loc, "Create+a+new+organization") {
		t.Fatalf("Location lacks action copy: %q", loc)
	}
}

func TestFormsAnonymousGetShowsInterstitialLink(t *testing.T) {
	h := newFormsHarness(t, "token")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodGet, "/new", nil, "", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "/login-required?next=%2Fnew")
	formsExpectContains(t, w, "sign in to create a new repository")
}

// --- /orgs/new ---------------------------------------------------------------

func TestFormsOrgNewPost(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{
		"org": {"Acme"}, "display_name": {"Acme Corp"}, "description": {"what"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/acme/settings")
	if id.lastOrg != [4]string{"acme", "Acme Corp", "what", "alice"} {
		t.Fatalf("CreateOrg args = %v", id.lastOrg)
	}
}

func TestFormsOrgNewValidation(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"Bad_Name"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "lowercase letters, digits, and hyphens")
	if got := formsValue(t, w, "orgnew-name"); got != "bad_name" {
		t.Fatalf("preserved org = %q", got)
	}
}

func TestFormsOrgNewConflictInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{orgErr: identity.ErrConflict}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, "conflict")
	formsExpectContains(t, w, "orgnew-name")
}

// --- /{owner}/{name}/fork ----------------------------------------------------

func TestFormsForkGetDefaultsAndBranches(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary: api.SummaryData{Head: &api.Ref{Name: "refs/heads/main"}},
		branches: []api.Ref{
			{Name: "refs/heads/dev"},
		},
	}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}})
	w := h.request(t, http.MethodGet, "/alice/thing/fork", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `href="/alice/thing"`)
	if got := formsValue(t, w, "fork-name"); got != "thing-fork" {
		t.Fatalf("default name = %q", got)
	}
	if got := formsOptions(t, w, "fork-branch"); len(got) != 3 || got[0] != "" || got[1] != "refs/heads/main" {
		t.Fatalf("branch options = %v", got)
	}
}

func TestFormsForkForksDisabledExplainer(t *testing.T) {
	h := newFormsHarness(t, "none")
	off := false
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: &config.ResolvedFeatures{Forks: off, Issues: true, Pulls: true, Releases: true}}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}})
	w := h.request(t, http.MethodGet, "/alice/thing/fork", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Forking is disabled for this repository.")
	if formsHasID(t, w, "fork-name") {
		t.Fatal("form must be absent when forks are disabled")
	}
}

func TestFormsForkPost(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	p := &formsFakePulls{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: p})
	w := h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{
		"owner": {"acme"}, "name": {"thing-fork"}, "visibility": {"private"},
		"branch": {"refs/heads/dev"}, "description": {"why"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/acme/thing-fork")
	want := pulls.ForkInput{TargetOwner: "acme", Name: "thing-fork", Visibility: "private", Branch: "refs/heads/dev", Description: "why"}
	if p.lastFork != want {
		t.Fatalf("StartFork input = %+v", p.lastFork)
	}
}

func TestFormsForkPostConflictWinner(t *testing.T) {
	h := newFormsHarness(t, "none")
	p := &formsFakePulls{forkErr: pulls.ErrConflict}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: p})
	w := h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{
		"owner": {"acme"}, "name": {"thing-fork"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, `href="/acme/thing-fork"`)
}

// --- releases ----------------------------------------------------------------

func TestFormsReleaseNewTagsAndCreate(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary: api.SummaryData{Features: formsAllFeatures()},
		tags:    []api.Ref{{Name: "refs/tags/v1.0.0"}, {Name: "refs/tags/v2.0.0"}},
	}}
	rel := &formsFakeReleases{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: rel})
	w := h.request(t, http.MethodGet, "/alice/thing/releases/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if got := formsOptions(t, w, "release-tag"); len(got) != 3 || got[1] != "v1.0.0" {
		t.Fatalf("tag options = %v", got)
	}
	formsExpectContains(t, w, "2 tags available")

	w = h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{
		"tag": {"v2.0.0"}, "name": {"Second"}, "body": {"notes"}, "draft": {"on"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/thing/releases/v2.0.0")
	if rel.lastTag != "v2.0.0" || rel.lastIn.Name == nil || *rel.lastIn.Name != "Second" || !*rel.lastIn.Draft {
		t.Fatalf("PutRelease = %q %+v", rel.lastTag, rel.lastIn)
	}
}

func TestFormsReleaseNewNoTagsDisabled(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: &formsFakeReleases{}})
	w := h.request(t, http.MethodGet, "/alice/thing/releases/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "No tags yet — create one from a commit page or push one with git.")
	root := formsParseHTML(t, w)
	sel := formsFindID(root, "release-tag")
	if sel == nil || !formsHasAttr(sel, "disabled") {
		t.Fatalf("tag select should be disabled: %+v", sel)
	}
}

func formsAllFeatures() *config.ResolvedFeatures {
	f := config.AllFeatures()
	return &f
}

func formsHasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func TestFormsReleaseNewUnknownTagInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	rel := &formsFakeReleases{putErr: releases.ErrNotFound}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: rel})
	w := h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{
		"tag": {"nope"}, "body": {"x"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	formsExpectContains(t, w, "not found")
	if got := formsValue(t, w, "release-tag"); got != "" {
		t.Fatalf("select value attr = %q", got)
	}
}

func TestFormsReleaseAutodraft(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary: api.SummaryData{Features: formsAllFeatures()},
		tags:    []api.Ref{{Name: "refs/tags/v3"}},
	}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: &formsFakeReleases{
		autodraft: &releases.Autodraft{Tag: "v3", Since: "v2", Body: "- #1 One"},
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{
		"tag": {"v3"}, "action": {"autodraft"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "changes since v2")
	formsExpectContains(t, w, "- #1 One")
}

// --- issues ------------------------------------------------------------------

func TestFormsIssueNewPreviewAndCreate(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	iss := &formsFakeIssues{issueCreateN: 7}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}, Issues: iss})
	w := h.request(t, http.MethodGet, "/alice/thing/issues/new", nil, "reader", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "New issue")

	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{
		"title": {"Bug"}, "body": {"hello **world**"}, "action": {"preview"},
	}, "reader", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "markdown-body")
	formsExpectContains(t, w, "<strong>world</strong>")
	if iss.lastIssue != [2]string{"", ""} {
		t.Fatal("preview must not create")
	}

	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{
		"title": {"Bug"}, "body": {""},
	}, "reader", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/thing/issues/7")
	if iss.lastIssue != [2]string{"Bug", ""} {
		t.Fatalf("CreateIssue args = %v", iss.lastIssue)
	}
}

func TestFormsIssueNewDisabledExplainer(t *testing.T) {
	h := newFormsHarness(t, "none")
	off := false
	f := formsAllFeatures()
	f.Issues = off
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: f}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodGet, "/alice/thing/issues/new", nil, "reader", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Issues are disabled for this repository.")
	if formsHasID(t, w, "issue-title") {
		t.Fatal("form must be absent when issues are disabled")
	}
}

// --- pulls -------------------------------------------------------------------

func TestFormsPullNewRoleGate(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodGet, "/alice/thing/pulls/new", nil, "reader", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "opening pull requests needs the write role — your role: read.")
}

func TestFormsPullNewPreviewStates(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary:  api.SummaryData{Features: formsAllFeatures()},
		branches: []api.Ref{{Name: "refs/heads/main"}, {Name: "refs/heads/dev"}},
		commits: map[string]api.CommitPage{
			"dev": {
				SHA: "head1",
				Commits: []api.Commit{
					{SHA: "c2", Subject: "second"},
					{SHA: "c1", Subject: "first"},
				},
			},
			"main": {
				SHA:     "base1",
				Commits: []api.Commit{{SHA: "c1", Subject: "first"}},
			},
		},
	}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})

	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/main"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "same endpoint")

	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/dev"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "1 ahead")
	formsExpectContains(t, w, "second")
}

func TestFormsPullNewOpenCrossRepo(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary:  api.SummaryData{Features: formsAllFeatures()},
		branches: []api.Ref{{Name: "refs/heads/main"}},
	}}
	p := &formsFakePulls{openNum: 3}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: p})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"acme/fork"}, "from_ref": {"refs/heads/main"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"title": {"Merge it"}, "body": {"body"}, "draft": {"on"}, "action": {"open"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/thing/pull/3")
	if p.lastBase != "alice/thing" {
		t.Fatalf("base repo = %q", p.lastBase)
	}
	if p.lastOpen.Fork == nil || p.lastOpen.Fork.Repo != "acme/fork" {
		t.Fatalf("fork = %+v", p.lastOpen.Fork)
	}
	if !p.lastOpen.Draft || p.lastOpen.Title != "Merge it" {
		t.Fatalf("open input = %+v", p.lastOpen)
	}
}

func TestFormsPullNewErrorCopy(t *testing.T) {
	h := newFormsHarness(t, "none")
	p := &formsFakePulls{openErr: errors.New("head commit not reachable — push first")}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: p})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/dev"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"title": {"T"}, "action": {"open"},
	}, "writer", nil)
	formsExpectContains(t, w, "push the From branch first.")
}

// --- teams -------------------------------------------------------------------

func TestFormsTeamPageAndWrites(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{team: &identity.Team{Org: "acme", Slug: "core", Name: "Core", Members: []string{"alice", "bob"}}}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodGet, "/acme/teams/core", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Members (2)")
	formsExpectContains(t, w, "member@example.com")

	w = h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"carol@example.com"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/acme/teams/core?flash=") {
		t.Fatalf("Location = %q", loc)
	}
	if id.lastAdded != "carol@example.com" {
		t.Fatalf("added = %q", id.lastAdded)
	}

	w = h.request(t, http.MethodPost, "/acme/teams/core/members/remove", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if id.lastRemoved != "bob" {
		t.Fatalf("removed = %q", id.lastRemoved)
	}
}

func TestFormsTeamNonOwnerForbidden(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{
		team:        &identity.Team{Org: "acme", Slug: "core", Members: []string{"alice"}},
		orgOwnerErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "org owner required"},
	}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"x"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "org owner required")
	if id.lastAdded != "" {
		t.Fatalf("write went through: %q", id.lastAdded)
	}
}

func TestFormsTeamUnknownIs404(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodGet, "/acme/teams/nope", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	formsExpectContains(t, w, "unknown team")
}

// --- labels ------------------------------------------------------------------

func TestFormsLabelsEmptyShowsPacks(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodGet, "/alice/thing/labels", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "GitHub defaults")
	formsExpectContains(t, w, "GitLab defaults")
	formsExpectContains(t, w, "d73a4a")
}

func TestFormsLabelsListHidesPacks(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{
		labels: []issues.Label{{Name: "bug", Color: "d73a4a", Description: "Something isn't working"}},
	}})
	w := h.request(t, http.MethodGet, "/alice/thing/labels", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), "GitHub defaults") {
		t.Fatal("pack cards must only render on an empty list")
	}
	formsExpectContains(t, w, "background-color: #d73a4a")
}

func TestFormsLabelCreateAndDelete(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{affected: 4}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: iss})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{
		"name": {"bug"}, "color": {"d73a4a"}, "color_custom": {"00ff00"}, "description": {"d"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.lastLabel != [3]string{"bug", "00ff00", "d"} {
		t.Fatalf("CreateLabel args = %v", iss.lastLabel)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "flash=label+created") {
		t.Fatalf("Location = %q", loc)
	}

	w = h.request(t, http.MethodPost, "/alice/thing/labels/delete", url.Values{"name": {"bug"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.deletedLabel != "bug" {
		t.Fatalf("deleted = %q", iss.deletedLabel)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "4+threads+updated") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFormsLabelDuplicateInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{
		labelCreateErr: fmt.Errorf("%w: label %q already exists", issues.ErrConflict, "bug"),
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"name": {"bug"}, "color": {"d73a4a"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, "already exists")
}

func TestFormsLabelPack(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{labels: []issues.Label{{Name: "Bug"}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: iss})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"pack": {"gitlab"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	// GitLab pack has 8 entries; "Bug" exists case-insensitively → 7 creates.
	if len(iss.packsCreated) != 7 {
		t.Fatalf("created %d labels: %v", len(iss.packsCreated), iss.packsCreated)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "7+labels+added") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFormsLabelForbidden(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{
		labelCreateErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "triage role required"},
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"name": {"x"}, "color": {"d73a4a"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "triage role required")
}

// --- milestones --------------------------------------------------------------

func TestFormsMilestonesPage(t *testing.T) {
	h := newFormsHarness(t, "none")
	open := &issues.Milestone{ID: "000001", Title: "v1.1", State: issues.StateOpen, OpenIssues: 2, ClosedIssues: 1, Percent: 33}
	closed := &issues.Milestone{ID: "000002", Title: "v1.0", State: issues.StateClosed, OpenIssues: 0, ClosedIssues: 5, Percent: 100}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{
		milestones: []*issues.Milestone{open, closed},
		issuesByMS: map[string][]issues.Card{
			"000001": {{Num: 4, Title: "A bug", State: issues.StateOpen}},
		},
	}})
	w := h.request(t, http.MethodGet, "/alice/thing/milestones", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `role="progressbar" aria-valuenow="33"`) {
		t.Fatalf("missing progressbar: %s", formsTruncate(w.Body.String()))
	}
	if !strings.Contains(w.Body.String(), "width: 33%") {
		t.Fatalf("missing progressbar: %s", formsTruncate(w.Body.String()))
	}
	formsExpectContains(t, w, "#4 A bug")
	formsExpectContains(t, w, "/alice/thing/issues?milestone=000001&amp;state=all")
	formsExpectContains(t, w, "Closed")
	formsExpectContains(t, w, "Reopen")
	formsExpectContains(t, w, "2 open · 1 closed")
}

func TestFormsMilestonesWrites(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: iss})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones", url.Values{"title": {"v2"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.lastMSTitle != "v2" {
		t.Fatalf("title = %q", iss.lastMSTitle)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "milestone+created") {
		t.Fatalf("Location = %q", loc)
	}

	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.lastState != "closed" {
		t.Fatalf("state = %q", iss.lastState)
	}

	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000002/delete", url.Values{}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.deletedMS != "000002" {
		t.Fatalf("deleted = %q", iss.deletedMS)
	}
}

func TestFormsMilestoneDeleteConflict(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{
		milestoneErr: issues.ErrConflict,
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones/000001/delete", url.Values{}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, "close it instead")
}

func TestFormsMilestoneBadID(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones/zzz/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
}

// --- import ------------------------------------------------------------------

func TestFormsImportGet(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodGet, "/import", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if got := formsOptions(t, w, "import-schedule"); len(got) != 5 || got[0] != "hourly" || got[4] != "monthly" {
		t.Fatalf("schedule options = %v", got)
	}
	root := formsParseHTML(t, w)
	token := formsFindID(root, "import-token")
	if token == nil || formsAttr(token, "type") != "password" {
		t.Fatalf("token field = %+v", token)
	}
	formsExpectContains(t, w, "Import limitations: LFS and ssh")
}

func TestFormsImportStartRendersRunningCard(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{res: &repoimport.BeginResult{TaskID: "task-42"}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `data-task-id="task-42"`)
	formsExpectContains(t, w, `data-task-stream="/api/v1/repos/imports/task-42"`)
	formsExpectContains(t, w, "The import keeps running if you navigate away")
	// The id on the card IS the id the status endpoint accepts: the page
	// links to it (requirement: a real, clickable link, not a sentence).
	formsExpectContains(t, w, `href="/api/v1/repos/imports/task-42"`)
	// The island is the only consumer of the bars/log hooks — pin both so a
	// template edit cannot orphan the island again.
	formsExpectContains(t, w, "data-task-bars")
	formsExpectContains(t, w, "data-task-log")
	formsExpectContains(t, w, "<noscript>")
	if imp.lastURL != "https://github.com/acme/monorepo.git" {
		t.Fatalf("canonical source = %q", imp.lastURL)
	}
}

// TestFormsImportTaskIDContract pins the mapping the page hands the island:
// data-task-id == the Begin TaskID (the import/service id the API's GET
// .../imports/{id} accepts) == the id in the stream URL == the id in the
// status link. The WAL table's own record id is never rendered.
func TestFormsImportTaskIDContract(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{
		res: &repoimport.BeginResult{TaskID: "i2f44aa"},
		// A joined import carries the table record: its id must not leak.
		rec: &wal.TaskRecord{ID: "e5d892bb", Kind: repoimport.KindRepoImport, Repo: "alice/monorepo"},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `data-task-id="i2f44aa"`)
	formsExpectContains(t, w, `data-task-stream="/api/v1/repos/imports/i2f44aa"`)
	formsExpectContains(t, w, `href="/api/v1/repos/imports/i2f44aa"`)
	if strings.Contains(w.Body.String(), "e5d892bb") {
		t.Fatalf("the table record id leaked into the page: %s", formsTruncate(w.Body.String()))
	}
}

// TestFormsImportFastFinishRendersDone covers the POST-render race: an import
// that reached its terminal state inside formsImportSettle must render the
// done card (with a real link into the repo), never a spinner.
func TestFormsImportFastFinishRendersDone(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{
		res:     &repoimport.BeginResult{TaskID: "i-fast"},
		stateOK: true,
		state: repoimport.TaskState{
			Finished: true,
			Outcome: &repoimport.Outcome{
				Repo:      "alice/monorepo",
				SourceURL: "https://github.com/acme/monorepo.git",
				HeadSHAs:  map[string]string{"refs/heads/main": "abc123"},
			},
		},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `href="/alice/monorepo"`)
	formsExpectContains(t, w, "refs/heads/main")
	formsExpectContains(t, w, "https://github.com/acme/monorepo.git")
	if strings.Contains(w.Body.String(), "data-task-id") {
		t.Fatalf("a finished import still rendered a progress card: %s", formsTruncate(w.Body.String()))
	}
}

// TestFormsImportFastFailureRendersFailureCard is the other half of the race:
// the failure summary + log land on the page, with a retry form and no
// spinner, and the source token is never echoed back.
func TestFormsImportFastFailureRendersFailureCard(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{
		res:     &repoimport.BeginResult{TaskID: "i-bad"},
		stateOK: true,
		state: repoimport.TaskState{
			Finished: true,
			Record: &wal.TaskRecord{
				ID: "t-bad", Kind: repoimport.KindRepoImport, Repo: "alice/monorepo",
				LogTail: []string{"clone start", "fatal: repository not found"},
			},
			Outcome: &repoimport.Outcome{Repo: "alice/monorepo", Err: &repoimport.StatusError{
				Status: 422, Message: "source not reachable: 404",
			}},
		},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
		"token": {"ghp-secret"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `aria-label="Import failed"`)
	formsExpectContains(t, w, "source not reachable: 404")
	formsExpectContains(t, w, "fatal: repository not found")
	formsExpectContains(t, w, "try again")
	if strings.Contains(w.Body.String(), "data-task-id") {
		t.Fatalf("a failed import still rendered a progress card: %s", formsTruncate(w.Body.String()))
	}
	if strings.Contains(w.Body.String(), "ghp-secret") {
		t.Fatalf("the source token was echoed into the failure page")
	}
}

// TestFormsImportTerminalFromRecord covers the branches formsImportTerminal
// reads when the outcome is absent (a pruned ring): success follows the
// record's OK, failure follows OK=false + Summary.
func TestFormsImportTerminalFromRecord(t *testing.T) {
	h := newFormsHarness(t, "none")
	ok, bad := true, false
	for _, tc := range []struct {
		name string
		rec  *wal.TaskRecord
		want string
	}{
		{"ok record", &wal.TaskRecord{ID: "t1", Repo: "alice/monorepo", OK: &ok}, `href="/alice/monorepo"`},
		{"failed record", &wal.TaskRecord{ID: "t2", Repo: "alice/monorepo", OK: &bad, Summary: "clone failed: timeout"}, "clone failed: timeout"},
	} {
		imp := &formsFakeImport{
			res:     &repoimport.BeginResult{TaskID: "i-rec"},
			stateOK: true,
			state:   repoimport.TaskState{Finished: true, Record: tc.rec},
		}
		formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
		w := h.request(t, http.MethodPost, "/import", url.Values{
			"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
		}, "writer", nil)
		formsExpectStatus(t, w, http.StatusOK)
		formsExpectContains(t, w, tc.want)
	}
}

// TestFormsImportRunningSeedsLog pins the still-running branch of the settle
// window: the card is rendered with the log the task already narrated.
func TestFormsImportRunningSeedsLog(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{
		res:     &repoimport.BeginResult{TaskID: "i-run"},
		stateOK: true,
		state: repoimport.TaskState{Record: &wal.TaskRecord{
			ID: "t-run", Repo: "alice/monorepo", LogTail: []string{"clone start https://github.com/acme/monorepo.git"},
		}},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `data-task-id="i-run"`)
	formsExpectContains(t, w, "clone start https://github.com/acme/monorepo.git")
}

func TestFormsImportNoopRendersDoneCard(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{res: &repoimport.BeginResult{NoOp: &repoimport.ImportDoc{
		SourceURL: "https://github.com/acme/monorepo.git",
		HeadSHAs:  map[string]string{"refs/heads/main": "abc123"},
	}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "already imported from this source — no-op")
	formsExpectContains(t, w, `href="/alice/monorepo"`)
	formsExpectContains(t, w, "refs/heads/main")
}

func TestFormsImportValidationInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {""}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "source_url is required")
	if got := formsValue(t, w, "import-name"); got != "monorepo" {
		t.Fatalf("preserved name = %q", got)
	}
}

func TestFormsImportMirrorCreate(t *testing.T) {
	h := newFormsHarness(t, "none")
	reg := &formsFakeRegistry{}
	h.s.env.API = &api.Env{Repos: reg, Store: store.NewMemory()}
	mir := &formsFakeMirror{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: mir})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
		"schedule": {"weekly"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/monorepo")
	if len(reg.created) != 1 || reg.created[0] != "alice/monorepo" {
		t.Fatalf("created = %v", reg.created)
	}
	if len(mir.synced) != 1 || mir.synced[0] != "alice/monorepo" {
		t.Fatalf("synced = %v", mir.synced)
	}
}

func TestFormsImportMirrorBadPreset(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
		"schedule": {"never"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "unknown schedule preset")
}

func TestFormsImportDangerousNeedsAdmin(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
		"schedule": {"daily"}, "dangerous": {"on"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "requires an authenticated admin")
}

func TestFormsImportServiceError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{err: &repoimport.StatusError{Status: 409, Message: "import target already exists"}}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"import"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"monorepo"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)
	formsExpectContains(t, w, "import target already exists")
}

// --- layout contract (390px) -------------------------------------------------

// TestFormsLayoutContract pins the AGENTS.md §2 mobile rules for every page in
// this group: no bare two-column grid (rows only split at sm:), no text-field
// class on a checkbox (Forgejo #533), and every `<label for=…>` resolves to a
// real id (so the row is a real control label at any width).
func TestFormsLayoutContract(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{
		Repos: &formsFakeRegistry{names: []string{"thing"}},
		Repo: &formsFakeRepoView{
			summary:  api.SummaryData{Head: &api.Ref{Name: "refs/heads/main"}, Features: formsAllFeatures()},
			branches: []api.Ref{{Name: "refs/heads/main"}},
			tags:     []api.Ref{{Name: "refs/tags/v1"}},
		},
	}
	formsUse(t, FormsDeps{
		Identity: &formsFakeIdentity{role: identity.RoleAdmin, team: &identity.Team{Org: "alice", Slug: "core", Name: "Core", Members: []string{"alice"}}},
		Issues: &formsFakeIssues{
			milestones: []*issues.Milestone{{ID: "000001", Title: "v1", State: issues.StateOpen, Percent: 10}},
		},
		Pulls:    &formsFakePulls{},
		Releases: &formsFakeReleases{},
		Import:   &formsFakeImport{},
		Mirror:   &formsFakeMirror{},
	})
	paths := []string{
		"/new", "/import", "/orgs/new",
		"/alice/thing/fork", "/alice/thing/releases/new", "/alice/thing/issues/new",
		"/alice/thing/pulls/new", "/alice/teams/core", "/alice/thing/labels", "/alice/thing/milestones",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := h.request(t, http.MethodGet, path, nil, "writer", nil)
			formsExpectStatus(t, w, http.StatusOK)
			root := formsParseHTML(t, w)
			ids := map[string]bool{}
			formsWalk(root, func(n *html.Node) {
				if n.Type != html.ElementNode {
					return
				}
				if id := formsAttr(n, "id"); id != "" {
					ids[id] = true
				}
			})
			formsWalk(root, func(n *html.Node) {
				if n.Type != html.ElementNode {
					return
				}
				for _, tok := range strings.Fields(formsAttr(n, "class")) {
					if tok == "grid-cols-2" || tok == "grid-cols-3" {
						t.Errorf("<%s> uses a bare %s (split only at sm:)", n.Data, tok)
					}
					if tok == "input" && formsAttr(n, "type") == "checkbox" {
						t.Errorf("checkbox carries the text-field class: %s", formsAttr(n, "class"))
					}
				}
				if n.Data == "label" {
					if forID := formsAttr(n, "for"); forID != "" && !ids[forID] {
						t.Errorf("<label for=%q> targets no element", forID)
					}
				}
			})
		})
	}
}

func formsWalk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		formsWalk(c, f)
	}
}

func TestFormsLabelsListErrorSurfaces(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{labelsErr: errFormsNoIssues}})
	w := h.request(t, http.MethodGet, "/alice/thing/labels", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "issues service not configured")
}

func TestFormsMilestonesListErrorSurfaces(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{milestonesErr: errFormsNoIssues}})
	w := h.request(t, http.MethodGet, "/alice/thing/milestones", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "issues service not configured")
}

// --- helpers sanity ----------------------------------------------------------

func TestFormsValidationMirrors(t *testing.T) {
	cases := []struct {
		owner, name string
		wantErr     bool
	}{
		{"alice", "thing", false},
		{"alice", "thing.git", false},
		{"alice", ".hidden", true},
		{"alice", "..", true},
		{"alice@example.com", "thing", true},
		{"alice", "bad name", true},
		{"", "thing", true},
	}
	for _, tc := range cases {
		_, _, msg := formsRepoNameErr(tc.owner, tc.name)
		if (msg != "") != tc.wantErr {
			t.Errorf("formsRepoNameErr(%q,%q) = %q", tc.owner, tc.name, msg)
		}
	}
	if formsNameCharsErr("") != "" {
		t.Error("empty name must not nag")
	}
	if formsNameCharsErr("ok-name_1.2") != "" {
		t.Error("valid charset rejected")
	}
	if formsNameCharsErr("nope!") == "" {
		t.Error("invalid charset accepted")
	}
	if !formsUIReservedOwner("New") || formsUIReservedOwner("alice") {
		t.Error("UI route collision mirror wrong")
	}
	if _, msg := formsOrgNameErr("Bad Org"); msg == "" {
		t.Error("org charset accepted")
	}
	if !formsRoleAtLeast(identity.RoleWrite, identity.RoleTriage) || formsRoleAtLeast(identity.RoleRead, identity.RoleWrite) {
		t.Error("role ladder wrong")
	}
	if !formsDangerousAllowed("token", auth.Principal{Name: "a", Admin: true}) {
		t.Error("admin under token should qualify")
	}
	if formsDangerousAllowed("none", auth.Principal{Name: "a", Admin: true}) {
		t.Error("auth-none admin must not qualify")
	}
}

func TestFormsCompareHistories(t *testing.T) {
	head := api.CommitPage{SHA: "h", Commits: []api.Commit{{SHA: "c3"}, {SHA: "c2"}, {SHA: "c1"}}, More: false}
	base := api.CommitPage{SHA: "b", Commits: []api.Commit{{SHA: "c0"}}, More: true}
	unique, ahead, behind, truncHead, truncBase := formsCompareHistories(head, base)
	if len(unique) != 3 || ahead != 3 || behind != 1 {
		t.Fatalf("unique=%d ahead=%d behind=%d", len(unique), ahead, behind)
	}
	if truncHead || !truncBase {
		t.Fatalf("truncation = %v/%v", truncHead, truncBase)
	}
	if formsFmtBounded(2, true) != "2+" || formsFmtBounded(2, false) != "2" {
		t.Fatal("fmtBounded")
	}
}

func TestFormsOriginCheck(t *testing.T) {
	h := newFormsHarness(t, "none")
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Host = "spindle.localhost:8080"
	r.Header.Set("Origin", "http://spindle.localhost:8080")
	if !h.s.formsOriginOK(r) {
		t.Fatal("same-origin rejected")
	}
	r.Header.Set("Origin", "http://evil.example")
	if h.s.formsOriginOK(r) {
		t.Fatal("cross-origin accepted")
	}
	r.Header.Del("Origin")
	r.Header.Set("Referer", "//evil.example/x")
	if h.s.formsOriginOK(r) {
		t.Fatal("protocol-relative referer accepted")
	}
	// Fail closed when neither header is present in a cookie-auth mode; the
	// auth-less none boot still accepts (no session to abuse).
	r.Header.Del("Referer")
	if !h.s.formsOriginOK(r) {
		t.Fatal("none mode rejected a headerless post")
	}
	h.s.env.Cfg.Server.Auth.Mode = "token"
	if h.s.formsOriginOK(r) {
		t.Fatal("token mode accepted a headerless post (fail-open)")
	}
	h.s.env.Cfg.Server.Auth.Mode = "none"
}

func TestFormsHXRedirect(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer",
		map[string]string{"HX-Request": "true"})
	formsExpectStatus(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Redirect"); got != "/acme/settings" {
		t.Fatalf("HX-Redirect = %q", got)
	}
	if w.Header().Get("Location") != "" {
		t.Fatal("HX request must not also send Location")
	}
}

// --- error mapping / small helpers ------------------------------------------

func TestFormsErrStatusTable(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusServiceUnavailable},
		{&auth.AuthError{Kind: auth.ErrForbidden}, http.StatusForbidden},
		{&auth.AuthError{Kind: auth.ErrUnavailable}, http.StatusServiceUnavailable},
		{&auth.AuthError{Kind: auth.ErrUnauthorized}, http.StatusUnauthorized},
		{&repoimport.StatusError{Status: http.StatusConflict, Message: "x"}, http.StatusConflict},
		{issues.ErrInvalid, http.StatusBadRequest},
		{issues.ErrConflict, http.StatusConflict},
		{issues.ErrNotFound, http.StatusNotFound},
		{issues.ErrForbidden, http.StatusForbidden},
		{issues.ErrUnauthorized, http.StatusUnauthorized},
		{pulls.ErrInvalid, http.StatusBadRequest},
		{pulls.ErrConflict, http.StatusConflict},
		{pulls.ErrNotFound, http.StatusNotFound},
		{pulls.ErrForbidden, http.StatusForbidden},
		{pulls.ErrUnauthorized, http.StatusUnauthorized},
		{pulls.ErrUnavailable, http.StatusServiceUnavailable},
		{releases.ErrInvalid, http.StatusBadRequest},
		{releases.ErrConflict, http.StatusConflict},
		{releases.ErrNotFound, http.StatusNotFound},
		{releases.ErrForbidden, http.StatusForbidden},
		{releases.ErrUnauthorized, http.StatusUnauthorized},
		{releases.ErrUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		if got := formsErrStatus(tc.err); got != tc.want {
			t.Errorf("formsErrStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if formsErrMessage(nil) != "" {
		t.Error("nil error message")
	}
	if formsErrMessage(issues.ErrInvalid) != "invalid" {
		t.Error("message must be verbatim")
	}
}

func TestFormsSmallHelpers(t *testing.T) {
	if formsLowerFirst("Create a repo") != "create a repo" || formsLowerFirst("") != "" {
		t.Error("lowerFirst")
	}
	if v, ok := formsVisibility(""); !ok || v != "public" {
		t.Error("empty visibility")
	}
	if _, ok := formsVisibility("secret"); ok {
		t.Error("bad visibility accepted")
	}
	if formsAuthStatus(&auth.AuthError{Kind: auth.ErrForbidden}) != http.StatusForbidden ||
		formsAuthStatus(&auth.AuthError{Kind: auth.ErrUnavailable}) != http.StatusServiceUnavailable ||
		formsAuthStatus(&auth.AuthError{Kind: auth.ErrInvalid}) != http.StatusUnauthorized {
		t.Error("auth status mapping")
	}
	if formsOwnerPart(strings.Repeat("a", 101)) || !formsOwnerPart("a") || !formsOwnerPart("a.b_c-d") {
		t.Error("owner part")
	}
	if formsSanitizeNext("/x") != "/x" || formsSanitizeNext("https://evil") != "/" || formsSanitizeNext("//evil") != "/" {
		t.Error("sanitize next")
	}
	r := httptest.NewRequest(http.MethodGet, "/x?flash="+strings.Repeat("f", 400), nil)
	if got := formsFlash(r); len(got) != formsFlashMax {
		t.Errorf("flash cap = %d", len(got))
	}
	if formsShortRef("refs/heads/main") != "main" || formsShortTag("refs/tags/v1") != "v1" {
		t.Error("short ref/tag")
	}
	if !formsContains([]string{"a", "b"}, "b") || formsContains(nil, "x") {
		t.Error("contains")
	}
	got := formsWithCurrent([]string{"b", "a/b"}, "a/b")
	if len(got) != 2 || got[0] != "a/b" || got[1] != "b" {
		t.Errorf("withCurrent = %v", got)
	}
	roles := map[string]identity.Role{"a/b": identity.RoleWrite, "a/c": identity.RoleRead}
	if out := formsReposByRole([]string{"a/b", "a/c"}, roles, identity.RoleWrite); len(out) != 1 || out[0] != "a/b" {
		t.Errorf("reposByRole = %v", out)
	}
	if heads := formsHeads(map[string]string{"refs/heads/b": "2", "refs/heads/a": "1"}); len(heads) != 2 || heads[0].Ref != "refs/heads/a" {
		t.Errorf("heads = %v", heads)
	}
	if presets := formsImportPresets(); len(presets) != 5 || presets[0].ID != "hourly" || presets[4].Label == "" {
		t.Errorf("presets = %v", presets)
	}
	if formsPreviewErrorMessage("From", issues.ErrNotFound) != "No such From branch — check the name." {
		t.Error("preview 404 copy")
	}
	if !strings.HasPrefix(formsPreviewErrorMessage("To", errors.New("boom")), "Couldn't compare:") {
		t.Error("preview generic copy")
	}
	if got := formsOpenErrorMessage(&repoimport.StatusError{Status: 409, Message: "already pairs"}); !strings.HasPrefix(got, "An open pull request already compares") {
		t.Errorf("open 409 copy = %q", got)
	}
	if got := formsOpenErrorMessage(&repoimport.StatusError{Status: 404, Message: "unknown revision"}); !strings.HasPrefix(got, "Unknown revision") {
		t.Errorf("open 404 copy = %q", got)
	}
	if got := formsOpenErrorMessage(errors.New("weird")); !strings.HasPrefix(got, "Couldn't open: weird") {
		t.Errorf("open generic copy = %q", got)
	}
}

func TestFormsLabelPackTable(t *testing.T) {
	if len(formsLabelPacks) != 2 {
		t.Fatalf("packs = %d", len(formsLabelPacks))
	}
	for _, pack := range formsLabelPacks {
		if len(pack.Labels) != 8 {
			t.Errorf("%s has %d labels", pack.ID, len(pack.Labels))
		}
		seen := map[string]bool{}
		for _, l := range pack.Labels {
			if len(l.Color) != 6 {
				t.Errorf("%s/%s color = %q", pack.ID, l.Name, l.Color)
			}
			if len(l.Description) > 200 {
				t.Errorf("%s/%s description too long", pack.ID, l.Name)
			}
			if seen[strings.ToLower(l.Name)] {
				t.Errorf("%s duplicate %q", pack.ID, l.Name)
			}
			seen[strings.ToLower(l.Name)] = true
			if l.Name == "" || len(l.Name) > 64 {
				t.Errorf("%s name out of bounds: %q", pack.ID, l.Name)
			}
		}
	}
}

func TestFormsBindForms(t *testing.T) {
	prev := formsDeps
	t.Cleanup(func() { formsDeps = prev })
	id := &formsFakeIdentity{}
	BindForms(FormsDeps{Identity: id})
	if formsDeps.Identity != id {
		t.Fatal("BindForms did not install the deps")
	}
}

func TestFormsNilServiceHelpers(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	r := httptest.NewRequest(http.MethodGet, "/alice/thing", nil)
	if _, ok := h.s.formsSummary(r, "bad", "thing"); ok {
		t.Error("summary with a bad id must fail")
	}
	if got := h.s.formsBranches(r, "bad owner", "thing"); got != nil {
		t.Errorf("branches = %v", got)
	}
	if got := h.s.formsTags(r, "bad owner", "thing"); got != nil {
		t.Errorf("tags = %v", got)
	}
	if got := h.s.formsBranchesFor(r, "nope"); got != nil {
		t.Errorf("branchesFor = %v", got)
	}
	if _, wired := h.s.formsRole(r, "a", "b", auth.Anonymous()); wired {
		t.Error("role must report unwired")
	}
	// formsSummary through a wired view.
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Branches: 3}}}
	if sum, ok := h.s.formsSummary(r, "alice", "thing"); !ok || sum.Branches != 3 {
		t.Error("summary wired read")
	}
}

// --- /new create completion --------------------------------------------------

type formsFakeAccessBoot struct {
	calls [][4]string
	err   error
}

func (f *formsFakeAccessBoot) EnsureRepoAccess(_ context.Context, owner, repo, creator, visibility string) error {
	f.calls = append(f.calls, [4]string{owner, repo, creator, visibility})
	return f.err
}

// formsFailPutStore fails every Put (the store-down branch).
type formsFailPutStore struct{ store.ObjectStore }

func (formsFailPutStore) Put(context.Context, string, store.PutBody, store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("put failed")
}

func TestFormsNewWritesPlaceholder(t *testing.T) {
	h := newFormsHarness(t, "none")
	mem := store.NewMemory()
	boot := &formsFakeAccessBoot{}
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: mem, AccessBoot: boot, PlaceholderHints: &api.PlaceholderHints{}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{
		"owner": {"alice"}, "name": {"thing"}, "format": {"sha256"}, "visibility": {"private"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	raw, _, err := store.GetBytes(context.Background(), mem, "repos/alice/thing/meta/placeholder.json", store.GetOptions{})
	if err != nil || len(raw) == 0 {
		t.Fatalf("placeholder marker missing: %v", err)
	}
	if len(boot.calls) != 1 || boot.calls[0] != [4]string{"alice", "thing", "alice", "private"} {
		t.Fatalf("access boot = %v", boot.calls)
	}
}

func TestFormsNewPostGuardTable(t *testing.T) {
	cases := []struct {
		name   string
		form   url.Values
		deps   FormsDeps
		api    *api.Env
		mode   string
		who    string
		status int
		body   string
	}{
		{
			name: "bad format", form: url.Values{"owner": {"alice"}, "name": {"t"}, "format": {"md5"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}}, api: &api.Env{Repos: &formsFakeRegistry{}},
			who: "writer", status: http.StatusBadRequest, body: "unknown object format",
		},
		{
			name: "bad visibility", form: url.Values{"owner": {"alice"}, "name": {"t"}, "visibility": {"secret"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}}, api: &api.Env{Repos: &formsFakeRegistry{}},
			who: "writer", status: http.StatusBadRequest, body: "visibility must be",
		},
		{
			name: "foreign owner", form: url.Values{"owner": {"acme"}, "name": {"t"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{createOwnerErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "creation under foreign owner denied; allowed: alice"}}},
			api:  &api.Env{Repos: &formsFakeRegistry{}},
			who:  "writer", status: http.StatusForbidden, body: "foreign owner denied",
		},
		{
			name: "registry down", form: url.Values{"owner": {"alice"}, "name": {"t"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}}, api: nil,
			who: "writer", status: http.StatusServiceUnavailable, body: "repo registry not configured",
		},
		{
			name: "create failure", form: url.Values{"owner": {"alice"}, "name": {"t"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}},
			api:  &api.Env{Repos: &formsFakeRegistry{createErr: errors.New("store down")}},
			who:  "writer", status: http.StatusServiceUnavailable, body: "store down",
		},
		{
			name: "name only dots", form: url.Values{"owner": {"alice"}, "name": {".."}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}}, api: &api.Env{Repos: &formsFakeRegistry{}},
			who: "writer", status: http.StatusBadRequest, body: "invalid name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFormsHarness(t, tc.mode)
			h.s.env.API = tc.api
			formsUse(t, tc.deps)
			w := h.request(t, http.MethodPost, "/new", tc.form, tc.who, nil)
			formsExpectStatus(t, w, tc.status)
			formsExpectContains(t, w, tc.body)
		})
	}
}

// --- import guards -----------------------------------------------------------

func TestFormsImportSourceHint(t *testing.T) {
	if got, err := formsSourceHint("acme/monorepo"); got != "https://github.com/acme/monorepo.git" || err != "" {
		t.Fatalf("hint = %q %q", got, err)
	}
	if got, errText := formsSourceHint("ssh://example.com/x.git"); got == "" || errText == "" {
		t.Fatalf("ssh hint = %q %q", got, errText)
	}
	if _, errText := formsSourceHint("https://user:pw@example.com/x.git"); errText == "" {
		t.Fatal("credentialed URL must be rejected")
	}
	if got, errText := formsSourceHint("  "); got != "" || errText != "" {
		t.Fatalf("blank hint = %q %q", got, errText)
	}
}

func TestFormsImportOnceNilService(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "import service not configured")
}

func TestFormsImportTaskRecordFallback(t *testing.T) {
	h := newFormsHarness(t, "none")
	imp := &formsFakeImport{res: &repoimport.BeginResult{}, rec: &wal.TaskRecord{ID: "rec-9"}}
	formsUse(t, FormsDeps{Import: imp, Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `data-task-id="rec-9"`)
}

func TestFormsImportMirrorGuards(t *testing.T) {
	mem := store.NewMemory()
	cases := []struct {
		name   string
		form   url.Values
		deps   FormsDeps
		who    string
		status int
		body   string
	}{
		{
			name: "reader cannot create", who: "reader", status: http.StatusForbidden, body: "write access required",
		},
		{
			name: "bad target", who: "writer", status: http.StatusBadRequest, body: "invalid name",
			form: url.Values{"owner": {"alice"}, "name": {"bad name"}},
		},
		{
			name: "ssh refused", who: "writer", status: http.StatusBadRequest, body: "ssh and git://",
			form: url.Values{"owner": {"alice"}, "name": {"m"}, "source_url": {"ssh://example.com/x.git"}},
		},
		{
			name: "host outside allowlist", who: "writer", status: http.StatusBadRequest, body: "not in import.url_allowlist",
			form: url.Values{"owner": {"alice"}, "name": {"m"}, "source_url": {"https://gitlab.com/a/b.git"}},
		},
		{
			name: "owner gate", who: "writer", status: http.StatusForbidden, body: "denied",
			form: url.Values{"owner": {"acme"}, "name": {"m"}, "source_url": {"acme/monorepo"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{createOwnerErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "denied"}}},
		},
		{
			name: "target exists", who: "writer", status: http.StatusConflict, body: "repository already exists",
			form: url.Values{"owner": {"alice"}, "name": {"m"}, "source_url": {"acme/monorepo"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}},
		},
		{
			name: "already a mirror", who: "writer", status: http.StatusConflict, body: "already a mirror",
			form: url.Values{"owner": {"alice"}, "name": {"m"}, "source_url": {"acme/monorepo"}},
			deps: FormsDeps{Identity: &formsFakeIdentity{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFormsHarness(t, "none")
			reg := &formsFakeRegistry{}
			switch tc.name {
			case "target exists":
				reg.createErr = api.ErrExists
			case "already a mirror":
				if _, err := mirror.Create(context.Background(), mem, "alice", "m", "https://github.com/acme/monorepo.git", "daily"); err != nil {
					t.Fatalf("seed mirror: %v", err)
				}
			}
			h.s.env.API = &api.Env{Repos: reg, Store: mem}
			deps := tc.deps
			if deps.Identity == nil {
				deps.Identity = &formsFakeIdentity{}
			}
			deps.Mirror = &formsFakeMirror{}
			formsUse(t, deps)
			form := tc.form
			if form == nil {
				form = url.Values{"owner": {"alice"}, "name": {"m"}, "source_url": {"acme/monorepo"}}
			}
			form.Set("kind", "mirror")
			form.Set("schedule", "daily")
			w := h.request(t, http.MethodPost, "/import", form, tc.who, nil)
			formsExpectStatus(t, w, tc.status)
			formsExpectContains(t, w, tc.body)
		})
	}
}

func TestFormsImportMirrorServiceMissing(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"}, "schedule": {"daily"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "mirror service not configured")
}

// --- fork / release / issue / pull guards -----------------------------------

func TestFormsForkPostGuards(t *testing.T) {
	off := false
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: &config.ResolvedFeatures{Forks: off, Issues: true, Pulls: true, Releases: true}}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{"owner": {"alice"}, "name": {"t"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "Forking is disabled for this repository.")

	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	w = h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{"owner": {"alice"}, "name": {"bad name"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)

	w = h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{
		"owner": {"alice"}, "name": {"t"}, "description": {strings.Repeat("d", 1100)},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "description exceeds")

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}})
	w = h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{"owner": {"alice"}, "name": {"t"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "pulls service not configured")
}

func TestFormsPullInvalidRepositories(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"bogus"}, "from_ref": {"refs/heads/x"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/y"}, "title": {"t"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid From repository")

	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/x"},
		"to_repo": {"bogus"}, "to_ref": {"refs/heads/y"}, "title": {"t"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid To repository")
}

func TestFormsReleasePostGuards(t *testing.T) {
	off := false
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: &config.ResolvedFeatures{Releases: off, Issues: true, Pulls: true, Forks: true}}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: &formsFakeReleases{}})
	w := h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{"tag": {"v1"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "Releases are disabled for this repository.")

	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	w = h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{"body": {"x"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "Choose a tag for this release.")

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}})
	w = h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{"tag": {"v1"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "releases service not configured")
}

func TestFormsIssuePostGuards(t *testing.T) {
	off := false
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: &config.ResolvedFeatures{Issues: off, Pulls: true, Releases: true, Forks: true}}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{"title": {"x"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "Issues are disabled for this repository.")

	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}})
	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{"title": {"x"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "issues service not configured")

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleRead}, Issues: &formsFakeIssues{issueErr: fmt.Errorf("%w: title must not be empty", issues.ErrInvalid)}})
	w = h.request(t, http.MethodPost, "/alice/thing/issues/new", url.Values{"title": {" "}, "body": {"x"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "title must not be empty")
}

func TestFormsPullPostGuards(t *testing.T) {
	off := false
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: &config.ResolvedFeatures{Pulls: off, Issues: true, Releases: true, Forks: true}}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{"title": {"x"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "Pull requests are disabled for this repository.")

	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}}}
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/x"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/y"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "a title and a From branch are required")

	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/x"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/x"}, "title": {"t"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "same endpoint")

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}})
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/x"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/y"}, "title": {"t"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "pulls service not configured")
}

func TestFormsPullPreviewErrors(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary:  api.SummaryData{Features: formsAllFeatures()},
		branches: []api.Ref{{Name: "refs/heads/main"}},
		commits: map[string]api.CommitPage{
			"main": {SHA: "b", Commits: []api.Commit{{SHA: "b"}}},
		},
	}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/missing"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Couldn&#39;t compare")

	// To-side failure: the From page resolves, the To does not.
	h.s.env.API.Repo.(*formsFakeRepoView).commits["main"] = api.CommitPage{SHA: "same", Commits: []api.Commit{{SHA: "same"}}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/main"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/nope"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Couldn&#39;t compare")

	// Same commit on both sides → the no-op line.
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/fork"}, "from_ref": {"refs/heads/main"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "both endpoints point at the same commit")
}

// --- team dispatchers --------------------------------------------------------

func TestFormsTeamPutAndDeleteDispatchers(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{team: &identity.Team{Org: "acme", Slug: "core", Members: []string{"alice"}}}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPut, "/acme/teams/core/members/bob", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if id.lastAdded != "bob" {
		t.Fatalf("PUT added = %q", id.lastAdded)
	}
	w = h.request(t, http.MethodDelete, "/acme/teams/core/members/bob", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if id.lastRemoved != "bob" {
		t.Fatalf("DELETE removed = %q", id.lastRemoved)
	}
}

func TestFormsTeamInvalidPrincipal(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{team: &identity.Team{Org: "acme", Slug: "core"}}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"  "}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid principal")
}

func TestFormsTeamMemberErrorInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{
		team:      &identity.Team{Org: "acme", Slug: "core"},
		memberErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "roster locked"},
	}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "roster locked")
}

func TestFormsTeamGetError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{teamErr: &auth.AuthError{Kind: auth.ErrUnavailable, Why: "identity down"}}})
	w := h.request(t, http.MethodGet, "/acme/teams/core", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "identity down")
}

func TestFormsTeamServiceMissing(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	w := h.request(t, http.MethodGet, "/acme/teams/core", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "identity service not configured")
}

// --- repo gate + write preflight --------------------------------------------

func TestFormsBadRepoIDIs404(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodGet, "/bad%20owner/thing/labels", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
}

func TestFormsInvisibleRepoIs404(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.cfg.Server.Auth.AnonymousRead = false
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodGet, "/alice/thing/labels", nil, "", nil)
	formsExpectStatus(t, w, http.StatusUnauthorized)
}

func TestFormsWritePreflight(t *testing.T) {
	h := newFormsHarness(t, "token")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	// Token mode + an anonymous viewer: same-origin passes, the gate then
	// redirects to the interstitial.
	w := h.request(t, http.MethodPost, "/new", url.Values{"owner": {"alice"}, "name": {"t"}}, "", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login-required") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFormsServiceNilGuards(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	guards := []struct{ path string }{
		{"/alice/thing/labels"},
		{"/alice/thing/milestones"},
		{"/alice/thing/issues/new"},
	}
	for _, g := range guards {
		w := h.request(t, http.MethodGet, g.path, nil, "writer", nil)
		if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d", g.path, w.Code)
		}
	}
	// Labels create with no issues service is a 503.
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"name": {"x"}, "color": {"d73a4a"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	w = h.request(t, http.MethodPost, "/alice/thing/milestones", url.Values{"title": {"x"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	w = h.request(t, http.MethodPost, "/alice/thing/labels/delete", url.Values{"name": {"x"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000001/delete", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
}

// --- gate matrix / remaining branches ---------------------------------------

func TestFormsAllPagesRequireAuth(t *testing.T) {
	h := newFormsHarness(t, "token")
	h.cfg.Server.Auth.AnonymousRead = false
	formsUse(t, FormsDeps{
		Identity: &formsFakeIdentity{role: identity.RoleAdmin, team: &identity.Team{Org: "a", Slug: "t"}},
		Issues:   &formsFakeIssues{}, Pulls: &formsFakePulls{}, Releases: &formsFakeReleases{},
		Import: &formsFakeImport{}, Mirror: &formsFakeMirror{},
	})
	paths := []string{
		"/new", "/import", "/orgs/new",
		"/alice/thing/fork", "/alice/thing/releases/new", "/alice/thing/issues/new",
		"/alice/thing/pulls/new", "/alice/teams/core", "/alice/thing/labels", "/alice/thing/milestones",
	}
	for _, path := range paths {
		w := h.request(t, http.MethodGet, path, nil, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", path, w.Code)
		}
	}
}

func TestFormsSameHostParseError(t *testing.T) {
	h := newFormsHarness(t, "none")
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Host = "spindle.localhost"
	r.Header.Set("Origin", "http://[::1")
	if h.s.formsOriginOK(r) {
		t.Fatal("unparseable origin accepted")
	}
}

func TestFormsSummaryError(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{summaryErr: errors.New("boom")}}
	r := httptest.NewRequest(http.MethodGet, "/alice/thing", nil)
	if _, ok := h.s.formsSummary(r, "alice", "thing"); ok {
		t.Fatal("summary error must be ok=false")
	}
}

func TestFormsServiceFailureIsErrorPage(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleTriage}, Issues: &formsFakeIssues{labelCreateErr: errors.New("store down")}})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"name": {"x"}, "color": {"d73a4a"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "store down")
}

func TestFormsTeamGateNoService(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "identity service not configured")
}

func TestFormsTeamRemoveByForm(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{team: &identity.Team{Org: "acme", Slug: "core"}}
	formsUse(t, FormsDeps{Identity: id})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members/remove", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if id.lastRemoved != "bob" {
		t.Fatalf("removed = %q", id.lastRemoved)
	}
	w = h.request(t, http.MethodPost, "/acme/teams/core/members/remove", url.Values{}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid principal")
}

func TestFormsLabelDeleteByPath(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{affected: 1}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: iss})
	w := h.request(t, http.MethodDelete, "/alice/thing/labels/bug", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if iss.deletedLabel != "bug" {
		t.Fatalf("deleted = %q", iss.deletedLabel)
	}
}

func TestFormsMilestoneCreateError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{milestoneErr: fmt.Errorf("%w: milestone title must be 1-256 characters", issues.ErrInvalid)}})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones", url.Values{"title": {""}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "milestone title must be 1-256 characters")
}

func TestFormsMilestoneReopenAndError(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: iss})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"open"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "milestone+reopened") {
		t.Fatalf("Location = %q", loc)
	}

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{milestoneErr: issues.ErrNotFound}})
	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	formsExpectContains(t, w, "not found")
}

func TestFormsOrgCreateInvalidInline(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{orgErr: fmt.Errorf("%w: invalid org", identity.ErrInvalid)}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "invalid org")
}

func TestFormsOrgCreateNoWrite(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "reader", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "write access required")
}

func TestFormsOrgCreateNilAnswer(t *testing.T) {
	h := newFormsHarness(t, "none")
	id := &formsFakeIdentity{}
	formsUse(t, FormsDeps{Identity: id})
	// A nil *identity.Org still redirects to the requested slug.
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/acme/settings")
}

func TestFormsImportMirrorRegistryError(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{createErr: errors.New("registry down")}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"}, "schedule": {"daily"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "registry down")
}

func TestFormsImportMirrorNilConfig(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.Cfg = nil
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"}, "schedule": {"daily"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
}

// --- branch sweep ------------------------------------------------------------

// TestFormsDispatchersIgnoreForeignPaths: every team dispatcher writes nothing
// for a path it does not own, so Serve reports "not migrated".
func TestFormsDispatchersIgnoreForeignPaths(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	cases := []struct{ method, path string }{
		{http.MethodGet, "/alice/thing/settings"},
		{http.MethodPost, "/alice/thing/settings/x"},
		{http.MethodPost, "/alice/thing/settings/x/y"},
		{http.MethodPut, "/alice/thing/settings/x/y"},
		{http.MethodDelete, "/alice/thing/settings/x/y"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		r.Header.Set("X-Test-User", "writer")
		w := httptest.NewRecorder()
		if h.s.Serve(w, r) {
			t.Errorf("%s %s was claimed by a dispatcher", tc.method, tc.path)
		}
	}
}

func TestFormsMalformedBodyTable(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{
		Identity: &formsFakeIdentity{role: identity.RoleAdmin, team: &identity.Team{Org: "a", Slug: "t"}},
		Issues:   &formsFakeIssues{}, Pulls: &formsFakePulls{}, Releases: &formsFakeReleases{},
		Import: &formsFakeImport{}, Mirror: &formsFakeMirror{},
	})
	paths := []string{
		"/new", "/import", "/orgs/new",
		"/alice/thing/fork", "/alice/thing/releases/new", "/alice/thing/issues/new",
		"/alice/thing/pulls/new", "/acme/teams/core/members", "/acme/teams/core/members/remove",
		"/alice/thing/labels", "/alice/thing/labels/delete", "/alice/thing/milestones",
		"/alice/thing/milestones/000001/state",
	}
	for _, path := range paths {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("owner=%zz"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Test-User", "writer")
		w := httptest.NewRecorder()
		h.s.Serve(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s malformed body = %d, want 400", path, w.Code)
		}
	}
}

func TestFormsPostRequiresAuth(t *testing.T) {
	h := newFormsHarness(t, "token")
	h.cfg.Server.Auth.AnonymousRead = false
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Issues: &formsFakeIssues{}, Pulls: &formsFakePulls{}, Releases: &formsFakeReleases{}, Import: &formsFakeImport{}})
	paths := []string{
		"/new", "/import", "/orgs/new",
		"/alice/thing/fork", "/alice/thing/releases/new", "/alice/thing/issues/new",
		"/alice/thing/pulls/new", "/acme/teams/core/members", "/acme/teams/core/members/remove",
		"/alice/thing/labels", "/alice/thing/labels/delete", "/alice/thing/milestones",
		"/alice/thing/milestones/000001/state", "/alice/thing/milestones/000001/delete",
	}
	for _, path := range paths {
		w := h.request(t, http.MethodPost, path, url.Values{}, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", path, w.Code)
		}
	}
}

func TestFormsInvisibleRepoCheckRead(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.auth.CheckRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodGet, "/alice/thing/labels", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
}

func TestFormsSummaryNilAPI(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = nil
	r := httptest.NewRequest(http.MethodGet, "/alice/thing", nil)
	if _, ok := h.s.formsSummary(r, "alice", "thing"); ok {
		t.Fatal("nil API must be ok=false")
	}
	if formsFeatureEnabled(api.SummaryData{}, "bogus") != true {
		t.Fatal("unknown feature must fail open")
	}
}

func TestFormsRefListError(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{refErr: errors.New("refs down")}}
	r := httptest.NewRequest(http.MethodGet, "/alice/thing", nil)
	if got := h.s.formsBranches(r, "alice", "thing"); got != nil {
		t.Errorf("branches = %v", got)
	}
	if got := h.s.formsTags(r, "alice", "thing"); got != nil {
		t.Errorf("tags = %v", got)
	}
}

func TestFormsOwnerOptionsEdge(t *testing.T) {
	h := newFormsHarness(t, "none")
	p := auth.Principal{Name: "alice", Write: true}
	id := &formsFakeIdentity{orgs: []string{"zeta", "zeta", "bad@example.com", "acme"}}
	formsUse(t, FormsDeps{Identity: id})
	got := h.s.formsOwnerOptions(httptest.NewRequest(http.MethodGet, "/new", nil), p)
	if len(got) != 3 || got[0] != "alice" || got[1] != "acme" || got[2] != "zeta" {
		t.Fatalf("owners = %v", got)
	}
	id.orgsErr = errors.New("roster down")
	got = h.s.formsOwnerOptions(httptest.NewRequest(http.MethodGet, "/new", nil), p)
	if len(got) != 1 || got[0] != "alice" {
		t.Fatalf("owners on roster error = %v", got)
	}
	if formsOwnerPart("bad@example.com") {
		t.Fatal("email principal must not be an owner option")
	}
}

func TestFormsNewPlaceholderStoreErrors(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: formsFailPutStore{store.NewMemory()}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w := h.request(t, http.MethodPost, "/new", url.Values{"owner": {"alice"}, "name": {"t"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "put failed")

	h2 := newFormsHarness(t, "none")
	h2.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory(), AccessBoot: &formsFakeAccessBoot{err: errors.New("access down")}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}})
	w = h2.request(t, http.MethodPost, "/new", url.Values{"owner": {"alice"}, "name": {"t"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "access down")
}

func TestFormsImportMirrorBadSource(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {""}, "owner": {"alice"}, "name": {"m"}, "schedule": {"daily"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "source_url is required")
}

func TestFormsOrgCreateWithoutIdentity(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	w := h.request(t, http.MethodPost, "/orgs/new", url.Values{"org": {"acme"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "identity service not configured")
}

func TestFormsForkEmptyChild(t *testing.T) {
	h := newFormsHarness(t, "none")
	p := &formsFakePulls{emptyChild: true}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: p})
	w := h.request(t, http.MethodPost, "/alice/thing/fork", url.Values{"owner": {"alice"}, "name": {"t"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/t")
}

func TestFormsReleaseAutodraftError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Releases: &formsFakeReleases{
		autodraftErr: fmt.Errorf("%w: unknown tag", releases.ErrNotFound),
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/releases/new", url.Values{"tag": {"v1"}, "action": {"autodraft"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	formsExpectContains(t, w, "unknown tag")
}

func TestFormsPullNewExtraOwnerRepos(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{
		Repos: &formsFakeRegistry{names: []string{"thing", "other"}},
		Repo:  &formsFakeRepoView{summary: api.SummaryData{Features: formsAllFeatures()}},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{roles: map[string]identity.Role{
		"alice/thing": identity.RoleWrite,
		"alice/other": identity.RoleRead,
	}}})
	w := h.request(t, http.MethodGet, "/alice/thing/pulls/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if got := formsOptions(t, w, "pull-to-repo"); len(got) != 1 || got[0] != "alice/thing" {
		t.Fatalf("To repos = %v", got)
	}
	if got := formsOptions(t, w, "pull-from-repo"); len(got) != 2 {
		t.Fatalf("From repos = %v", got)
	}
	// No identity wired: the role gate fails open and both selects carry the
	// owner's repos (the server is the authority).
	formsUse(t, FormsDeps{})
	w = h.request(t, http.MethodGet, "/alice/thing/pulls/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "New pull request")
	if got := formsOptions(t, w, "pull-to-repo"); len(got) != 1 || got[0] != "alice/thing" {
		t.Fatalf("To repos without identity = %v", got)
	}
}

func TestFormsCompareEdgeStates(t *testing.T) {
	h := newFormsHarness(t, "none")
	commits := make([]api.Commit, 0, 12)
	for i := 0; i < 12; i++ {
		commits = append(commits, api.Commit{SHA: "h" + string(rune('a'+i)), Subject: "s"})
	}
	commits = append(commits, api.Commit{SHA: ""})
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{
		summary:  api.SummaryData{Features: formsAllFeatures()},
		branches: []api.Ref{{Name: "refs/heads/dev"}, {Name: "refs/heads/main"}},
		commits: map[string]api.CommitPage{
			"dev":  {SHA: "head", Commits: commits},
			"main": {SHA: "base", Commits: []api.Commit{{SHA: "base"}}},
		},
	}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleWrite}, Pulls: &formsFakePulls{}})
	w := h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/dev"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "and 2 more")

	// Idle: a preview with no refs.
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{"action": {"preview"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "Pick a From and a To branch")

	// Compare unavailable: no Repo view wired.
	h.s.env.API = &api.Env{}
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"alice/thing"}, "from_ref": {"refs/heads/dev"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"action": {"preview"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "compare unavailable")
}

func TestFormsPackUnknownAndPartial(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{}})
	w := h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"pack": {"nope"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "unknown label pack")

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{
		labelCreateErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"},
	}})
	w = h.request(t, http.MethodPost, "/alice/thing/labels", url.Values{"pack": {"github"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "nope")
}

func TestFormsLabelDeleteServiceError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{
		labelDeleteErr: fmt.Errorf("%w: unknown label", issues.ErrNotFound),
	}})
	w := h.request(t, http.MethodPost, "/alice/thing/labels/delete", url.Values{"name": {"gone"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	formsExpectContains(t, w, "unknown label")
}

func TestFormsMilestonesEdgeRows(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{
		milestones: []*issues.Milestone{
			nil,
			{ID: "000001", Title: "one", State: issues.StateOpen, OpenIssues: 1, Percent: 0},
		},
	}})
	w := h.request(t, http.MethodGet, "/alice/thing/milestones", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "View 1 issue")
}

func TestFormsMilestoneStateDefaultsAndErrors(t *testing.T) {
	h := newFormsHarness(t, "none")
	iss := &formsFakeIssues{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: iss})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"bogus"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "state must be open|closed")
	if iss.lastState != "" {
		t.Fatalf("invalid state reached the service: %q", iss.lastState)
	}
	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)

	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{role: identity.RoleAdmin}, Issues: &formsFakeIssues{milestoneErr: issues.ErrConflict}})
	w = h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusConflict)

	w = h.request(t, http.MethodPost, "/alice/thing/milestones/zzz/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
	w = h.request(t, http.MethodDelete, "/alice/thing/milestones/zzz", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
}

func TestFormsTeamFailServerError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{
		team:      &identity.Team{Org: "acme", Slug: "core"},
		memberErr: errors.New("store down"),
	}})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "store down")
}

func TestFormsPostAnonRedirectsTable(t *testing.T) {
	h := newFormsHarness(t, "token")
	formsUse(t, FormsDeps{
		Identity: &formsFakeIdentity{role: identity.RoleAdmin, team: &identity.Team{Org: "a", Slug: "t"}},
		Issues:   &formsFakeIssues{}, Pulls: &formsFakePulls{}, Releases: &formsFakeReleases{},
		Import: &formsFakeImport{}, Mirror: &formsFakeMirror{},
	})
	paths := []string{
		"/new", "/import", "/orgs/new",
		"/alice/thing/fork", "/alice/thing/releases/new", "/alice/thing/issues/new",
		"/alice/thing/pulls/new", "/acme/teams/core/members", "/acme/teams/core/members/remove",
		"/alice/thing/labels", "/alice/thing/labels/delete", "/alice/thing/milestones",
		"/alice/thing/milestones/000001/state", "/alice/thing/milestones/000001/delete",
	}
	for _, path := range paths {
		w := h.request(t, http.MethodPost, path, url.Values{"owner": {"alice"}, "name": {"t"}}, "", nil)
		if w.Code != http.StatusSeeOther {
			t.Errorf("%s = %d, want 303 to the interstitial", path, w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login-required") {
			t.Errorf("%s Location = %q", path, loc)
		}
	}
}

func TestFormsNeedWriteImport(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"},
	}, "reader", nil)
	formsExpectStatus(t, w, http.StatusForbidden)
	formsExpectContains(t, w, "write access required")
}

func TestFormsFlashTargetWithQuery(t *testing.T) {
	h := newFormsHarness(t, "none")
	w := httptest.NewRecorder()
	h.s.formsRedirectFlash(w, httptest.NewRequest(http.MethodPost, "/x", nil), "/t?x=1", "hi")
	if loc := w.Header().Get("Location"); loc != "/t?x=1&flash=hi" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFormsFeatureEnabledUnknown(t *testing.T) {
	sum := api.SummaryData{Features: formsAllFeatures()}
	if !formsFeatureEnabled(sum, "bogus") {
		t.Fatal("unknown feature must stay enabled")
	}
}

func TestFormsSummaryBadIDWithView(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{}}
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if _, ok := h.s.formsSummary(r, "bad owner", "thing"); ok {
		t.Fatal("bad id must be ok=false")
	}
	if got := h.s.formsBranches(r, "bad owner", "thing"); got != nil {
		t.Error("branches with bad id")
	}
	if got := h.s.formsTags(r, "bad owner", "thing"); got != nil {
		t.Error("tags with bad id")
	}
}

func TestFormsImportNilBeginResult(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "import service not configured")
}

func TestFormsMirrorStoreFailure(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: formsFailPutStore{store.NewMemory()}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Mirror: &formsFakeMirror{}})
	w := h.request(t, http.MethodPost, "/import", url.Values{
		"kind": {"mirror"}, "source_url": {"acme/monorepo"}, "owner": {"alice"}, "name": {"m"}, "schedule": {"daily"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	formsExpectContains(t, w, "put failed")
}

func TestFormsCompareInvalidRepo(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{Repo: &formsFakeRepoView{}}
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	pv := h.s.formsCompare(r, &formsPullNewData{FromRepo: "bad owner/r", ToRepo: "a/b", FromRef: "refs/heads/x", ToRef: "refs/heads/y"})
	if pv.Status != "error" || pv.Error == "" {
		t.Fatalf("preview = %+v", pv)
	}
}

func TestFormsCompareBaseEmptySHA(t *testing.T) {
	head := api.CommitPage{SHA: "h", Commits: []api.Commit{{SHA: "c1"}}}
	base := api.CommitPage{SHA: "b", Commits: []api.Commit{{SHA: ""}, {SHA: "c1"}}}
	_, _, behind, _, _ := formsCompareHistories(head, base)
	if behind != 0 {
		t.Fatalf("behind = %d", behind)
	}
	if formsMilestoneID("zzz") != "" || formsMilestoneID("00000") != "" || formsMilestoneID("00000g") != "" || formsMilestoneID("ABCDEF") != "abcdef" {
		t.Fatal("milestone id validation")
	}
}

func TestFormsTeamRemoveError(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{
		team:      &identity.Team{Org: "acme", Slug: "core"},
		memberErr: fmt.Errorf("%w: unknown team", identity.ErrNotFound),
	}})
	w := h.request(t, http.MethodPost, "/acme/teams/core/members/remove", url.Values{"principal": {"bob"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusNotFound)
}

func TestFormsMilestoneStateNoService(t *testing.T) {
	h := newFormsHarness(t, "none")
	formsUse(t, FormsDeps{})
	w := h.request(t, http.MethodPost, "/alice/thing/milestones/000001/state", url.Values{"state": {"closed"}}, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
	w = h.request(t, http.MethodDelete, "/alice/thing/milestones/000001", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusServiceUnavailable)
}

// --- wiring assertions -------------------------------------------------------

// The real feature services satisfy the dependency interfaces: this is what
// composition binds (BindForms) and it must stay a zero-adapter seam. The
// issues service is deliberately absent — FormsIssues.ListLabels /
// ListMilestones are the exported reads internal/issues still owes (its
// handlers call the unexported loadLabels / listMilestones today), so
// `var _ FormsIssues = (*issues.Service)(nil)` does not compile yet.
var (
	_ FormsIdentity = (*identity.Service)(nil)
	_ FormsPulls    = (*pulls.Service)(nil)
	_ FormsReleases = (*releases.Service)(nil)
	_ FormsImport   = (*repoimport.Service)(nil)
	_ FormsMirror   = (*mirror.Service)(nil)
)

// TestFormsPullNewForkPicker covers WP2.1: the head picker lists the live
// fork index (grouped under "Forks"), only forks the viewer can read, and a
// fork head posts through the existing cross-owner OpenPR path.
func TestFormsPullNewForkPicker(t *testing.T) {
	h := newFormsHarness(t, "none")
	h.s.env.API = &api.Env{
		Repos: &formsFakeRegistry{names: []string{"thing"}},
		Repo: &formsFakeRepoView{
			summary:  api.SummaryData{Features: formsAllFeatures()},
			branches: []api.Ref{{Name: "refs/heads/main"}, {Name: "refs/heads/fork-work"}},
		},
	}
	p := &formsFakePulls{
		openNum: 9,
		forks: []pulls.ForkListEntry{
			{Repo: "acme/fork", ForkedAt: "2026-11-18T00:00:00Z"},
			{Repo: "mallory/private", ForkedAt: "2026-11-18T01:00:00Z"},
		},
	}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{roles: map[string]identity.Role{
		"alice/thing": identity.RoleWrite,
		"acme/fork":   identity.RoleRead,
	}}, Pulls: p})

	w := h.request(t, http.MethodGet, "/alice/thing/pulls/new", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, "fork-work")
	opts := formsOptions(t, w, "pull-from-repo")
	if !formsListContains(opts, "acme/fork") {
		t.Fatalf("fork missing from picker: %v", opts)
	}
	if formsListContains(opts, "mallory/private") {
		t.Fatalf("unreadable fork leaked into picker: %v", opts)
	}

	// Selecting the fork (server round trip, no JS) lists ITS branches.
	w = h.request(t, http.MethodGet, "/alice/thing/pulls/new?from_repo=acme/fork&head=refs/heads/main", nil, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if got := formsOptions(t, w, "pull-from-ref"); !formsListContains(got, "refs/heads/main") {
		t.Fatalf("fork branches = %v", got)
	}
	// The branch-discovery submit is a re-render, not a PR.
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"acme/fork"}, "action": {"refs"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusOK)
	if p.lastOpen.Title != "" {
		t.Fatalf("refs submit opened a PR: %+v", p.lastOpen)
	}

	// Opening from the picker passes the head repo through to OpenPR.
	w = h.request(t, http.MethodPost, "/alice/thing/pulls/new", url.Values{
		"from_repo": {"acme/fork"}, "from_ref": {"refs/heads/fork-work"},
		"to_repo": {"alice/thing"}, "to_ref": {"refs/heads/main"},
		"title": {"Cross-owner"}, "action": {"open"},
	}, "writer", nil)
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/thing/pull/9")
	if p.lastOpen.Fork == nil || p.lastOpen.Fork.Repo != "acme/fork" || p.lastOpen.HeadRef != "refs/heads/fork-work" {
		t.Fatalf("open input = %+v", p.lastOpen)
	}
}
