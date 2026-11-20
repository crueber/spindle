package webui

// Tests for the repo group (internal/webui/repo.go + templates/pages/repo and
// templates/partials/repo_*). The fake RepoView / RepoRegistry stand in for
// the wal engine, so every assertion is about the handler/template contract:
// gates, 200/404 shapes, empty states, the server renderers' output.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/pages"
	"github.com/crueber/walhub/internal/server/auth"
)

// --- fakes ------------------------------------------------------------------

type repoFakeView struct {
	sha string

	summary    api.SummaryData
	summaryErr error

	resolution api.Resolution
	resolveErr error

	tree    api.TreeResult
	treeErr error

	blob    api.BlobResult
	blobErr error

	commits    api.CommitPage
	commitsErr error

	detail    api.CommitDetail
	commitErr error

	overview    api.OverviewData
	overviewErr error

	syncErr error

	syncN, resolveN, treeN, blobN, commitsN, commitN int
}

func newRepoFake() *repoFakeView {
	sha := strings.Repeat("a", 40)
	feats := config.AllFeatures()
	return &repoFakeView{
		sha: sha,
		summary: api.SummaryData{
			Head:     &api.Ref{Name: "refs/heads/main", SHA: sha},
			Branches: 1,
			Health:   api.RepoHealthHealthy,
			Features: &feats,
		},
		resolution: api.Resolution{Ref: "refs/heads/main", SHA: sha, Kind: "branch"},
		tree:       api.TreeResult{Ref: "refs/heads/main"},
		blob: api.BlobResult{
			Ref:      "refs/heads/main",
			SHA:      sha,
			Path:     "a.txt",
			Name:     "a.txt",
			Size:     4,
			Contents: []byte("a\nb\n"),
		},
	}
}

func (v *repoFakeView) Sync(context.Context, git.RepoId, api.SyncLevel) error {
	v.syncN++
	return v.syncErr
}

func (v *repoFakeView) Resolve(_ context.Context, _ git.RepoId, rest string) (api.Resolution, error) {
	v.resolveN++
	if v.resolveErr != nil {
		return api.Resolution{}, v.resolveErr
	}
	r := v.resolution
	if r.SHA == "" {
		r = api.Resolution{Ref: "refs/heads/main", SHA: v.sha, Kind: "branch"}
	}
	r.Path = ""
	if rest != "" {
		if segs := strings.SplitN(rest, "/", 2); len(segs) == 2 {
			r.Path = segs[1]
		}
	}
	return r, nil
}

func (v *repoFakeView) Head(context.Context, git.RepoId) (api.Ref, bool, error) {
	if v.summary.Head == nil {
		return api.Ref{}, false, nil
	}
	return *v.summary.Head, true, nil
}

func (v *repoFakeView) RefList(context.Context, git.RepoId, string, api.RefQuery) ([]api.Ref, bool, error) {
	return nil, false, nil
}

func (v *repoFakeView) Tree(_ context.Context, _ git.RepoId, rev, path string) (api.TreeResult, error) {
	v.treeN++
	if v.treeErr != nil {
		return api.TreeResult{}, v.treeErr
	}
	t := v.tree
	if t.SHA == "" {
		t.SHA = v.sha
	}
	t.Path = path
	return t, nil
}

func (v *repoFakeView) Blob(_ context.Context, _ git.RepoId, rev, path string, _ bool) (api.BlobResult, error) {
	v.blobN++
	if v.blobErr != nil {
		return api.BlobResult{}, v.blobErr
	}
	b := v.blob
	if b.SHA == "" {
		b.SHA = rev
	}
	return b, nil
}

func (v *repoFakeView) Commits(_ context.Context, _ git.RepoId, _, _ string, _, _ int) (api.CommitPage, error) {
	v.commitsN++
	if v.commitsErr != nil {
		return api.CommitPage{}, v.commitsErr
	}
	return v.commits, nil
}

func (v *repoFakeView) Commit(_ context.Context, _ git.RepoId, _ string) (api.CommitDetail, error) {
	v.commitN++
	if v.commitErr != nil {
		return api.CommitDetail{}, v.commitErr
	}
	return v.detail, nil
}

func (v *repoFakeView) Summary(context.Context, git.RepoId) (api.SummaryData, error) {
	if v.summaryErr != nil {
		return api.SummaryData{}, v.summaryErr
	}
	return v.summary, nil
}

func (v *repoFakeView) Overview(context.Context, git.RepoId) (api.OverviewData, error) {
	if v.overviewErr != nil {
		return api.OverviewData{}, v.overviewErr
	}
	return v.overview, nil
}

func (v *repoFakeView) Settings(context.Context, git.RepoId) (api.SettingsDoc, error) {
	return api.SettingsDoc{}, nil
}

func (v *repoFakeView) PublishSettings(context.Context, git.RepoId, []byte, string, string) (uint64, error) {
	return 0, nil
}

func (v *repoFakeView) SettingsHistory(context.Context, git.RepoId) (api.SettingsHistory, error) {
	return api.SettingsHistory{}, nil
}

func (v *repoFakeView) HeadSeq(context.Context, git.RepoId) (uint64, error) { return 0, nil }

func (v *repoFakeView) PushHistory(context.Context, git.RepoId, int) ([]api.PushRecord, error) {
	return nil, nil
}

type repoFakeRegistry struct {
	exists map[string]bool
	err    error
}

func (r *repoFakeRegistry) Owners(context.Context) ([]string, error) { return nil, r.err }
func (r *repoFakeRegistry) Repos(context.Context, string) ([]string, error) {
	return nil, r.err
}
func (r *repoFakeRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) {
	return map[string]int{}, r.err
}
func (r *repoFakeRegistry) Exists(_ context.Context, id git.RepoId) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	return r.exists[id.Owner+"/"+id.Name], nil
}
func (r *repoFakeRegistry) Create(context.Context, git.RepoId, git.ObjectFormat) error {
	return nil
}
func (r *repoFakeRegistry) Delete(context.Context, git.RepoId) error { return nil }

type repoFakeTasks struct {
	ops    []api.OpSpec
	recent []api.TaskRecord
}

func (f *repoFakeTasks) Ops() []api.OpSpec { return f.ops }
func (f *repoFakeTasks) List(context.Context, git.RepoId) ([]api.TaskRecord, []api.TaskRecord, error) {
	return nil, f.recent, nil
}
func (f *repoFakeTasks) Get(context.Context, git.RepoId, string) (api.TaskRecord, bool, error) {
	return api.TaskRecord{}, false, nil
}
func (f *repoFakeTasks) Begin(context.Context, git.RepoId, string, map[string]string) (api.TaskStream, error) {
	return api.TaskStream{}, errors.New("not used")
}
func (f *repoFakeTasks) Attach(context.Context, git.RepoId, string) (api.TaskStream, bool, error) {
	return api.TaskStream{}, false, nil
}

// --- fixture ----------------------------------------------------------------

type repoFixture struct {
	t         *testing.T
	anonRead  bool
	s         *Server
	view      *repoFakeView
	reg       *repoFakeRegistry
	tasks     *repoFakeTasks
	principal auth.Principal
	authErr   *auth.AuthError
	checkRead func(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

func newRepoFixture(t *testing.T, anonRead bool) *repoFixture {
	t.Helper()
	f := &repoFixture{
		t:         t,
		anonRead:  anonRead,
		view:      newRepoFake(),
		reg:       &repoFakeRegistry{exists: map[string]bool{"demo/walhub": true}},
		principal: auth.Anonymous(),
	}
	cfg := &config.Config{}
	cfg.Server.Auth.AnonymousRead = anonRead
	cfg.Server.PublicURL = "http://example.test"
	f.s = f.newServer(cfg)
	return f
}

func (f *repoFixture) newServer(cfg *config.Config) *Server {
	f.t.Helper()
	apiEnv := &api.Env{Repo: f.view, Repos: f.reg, Cfg: cfg}
	if f.tasks != nil {
		apiEnv.Tasks = f.tasks
	}
	s, err := New(Env{Cfg: cfg, API: apiEnv, Version: "test"})
	if err != nil {
		f.t.Fatalf("webui.New: %v", err)
	}
	f.s = s
	f.rebind()
	return s
}

// rebind re-installs the authority closures after a test changed the
// principal or the CheckRead hook.
func (f *repoFixture) rebind() {
	f.s.BindAuth(pages.Auth{
		Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
			if f.authErr != nil {
				return auth.Principal{}, f.authErr
			}
			return f.principal, nil
		},
		CheckRead: f.checkRead,
	})
}

func (f *repoFixture) get(path string) (int, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	f.s.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (f *repoFixture) mustGet(path string, want int) string {
	f.t.Helper()
	code, body := f.get(path)
	if code != want {
		f.t.Fatalf("GET %s = %d, want %d\nbody: %.500s", path, code, want, body)
	}
	return body
}

// --- html helpers -----------------------------------------------------------

func parsePage(t *testing.T, body string) *html.Node {
	t.Helper()
	n, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	return n
}

func walk(n *html.Node, fn func(*html.Node)) {
	if n.Type == html.ElementNode {
		fn(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, cls string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == cls {
			return true
		}
	}
	return false
}

func byClass(t *testing.T, body, cls string) []*html.Node {
	t.Helper()
	var out []*html.Node
	walk(parsePage(t, body), func(n *html.Node) {
		if hasClass(n, cls) {
			out = append(out, n)
		}
	})
	return out
}

func text(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func tabsOf(t *testing.T, body string) []*html.Node {
	t.Helper()
	var out []*html.Node
	walk(parsePage(t, body), func(n *html.Node) {
		if attr(n, "data-tab") != "" {
			out = append(out, n)
		}
	})
	return out
}

func tabIDs(t *testing.T, body string) []string {
	t.Helper()
	var ids []string
	for _, n := range tabsOf(t, body) {
		ids = append(ids, attr(n, "data-tab"))
	}
	return ids
}

func activeTabOfRepoChrome(c *RepoChrome) string {
	for _, tab := range c.Tabs {
		if tab.Active {
			return tab.ID
		}
	}
	return ""
}

func activeTabOf(t *testing.T, body string) string {
	t.Helper()
	for _, n := range tabsOf(t, body) {
		if attr(n, "aria-current") == "page" {
			return attr(n, "data-tab")
		}
	}
	return ""
}

// --- tree -------------------------------------------------------------------

func TestRepoTreeRootRendersListing(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref: "refs/heads/main",
		Entries: []api.TreeEntry{
			{Name: "pkg", Type: "tree", Mode: "040000", Size: -1, SHA: strings.Repeat("b", 40)},
			{Name: "main.go", Type: "blob", Mode: "100644", Size: 1500, SHA: strings.Repeat("c", 40), CommitTime: "2026-01-02T03:04:05Z"},
		},
		Readme: &api.Readme{Name: "README.md", Contents: "# Hello\n\nWelcome."},
	}
	body := f.mustGet("/demo/walhub", http.StatusOK)

	for _, want := range []string{
		`class="repo-header`,
		`/demo/walhub/blob/main/main.go`,
		`/demo/walhub/tree/main/pkg`,
		`.rw-r--r--`,
		`<h1>Hello</h1>`,
		`Welcome.`,
		`README.md`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tree page missing %q", want)
		}
	}
	// The mode cell ships hidden below sm: (phones carry icons instead).
	if !strings.Contains(body, "hidden sm:table-cell") {
		t.Error("tree page lost the narrow-width mode column rule")
	}
	// Tab strip: Code..Settings in order, Code active at the root.
	if got, want := strings.Join(tabIDs(t, body), ","), "code,commits,issues,pulls,releases,settings"; got != want {
		t.Errorf("tabs = %s, want %s (Checks hidden: has_checks=false)", got, want)
	}
	if got := activeTabOf(t, body); got != "code" {
		t.Errorf("active tab = %q, want code", got)
	}
	// Root listing: no breadcrumb (nothing to trace).
	if strings.Contains(body, `class="crumbs`) {
		t.Error("root tree rendered a breadcrumb")
	}
}

func TestRepoTreeSubpathBreadcrumbKeepsShortRef(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref:     "refs/heads/main",
		Path:    "pkg/inner",
		Entries: []api.TreeEntry{{Name: "a.go", Type: "blob", Mode: "100644", Size: 12, SHA: strings.Repeat("d", 40)}},
	}
	body := f.mustGet("/demo/walhub/tree/main/pkg/inner", http.StatusOK)
	for _, want := range []string{
		`/demo/walhub/tree/main/pkg`,
		`<strong>inner</strong>`,
		`/demo/walhub/blob/main/pkg/inner/a.go`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("subpath tree missing %q", want)
		}
	}
	if strings.Contains(body, `href="/demo/walhub/tree/refs/heads`) {
		t.Error("breadcrumb links leaked the full ref name")
	}
	// /tree (no rest) answers the root listing too.
	if code, _ := f.get("/demo/walhub/tree"); code != http.StatusOK {
		t.Errorf("/tree = %d, want 200", code)
	}
}

func TestRepoTreeDoctabSelection(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{
		Ref: "refs/heads/main",
		Entries: []api.TreeEntry{
			{Name: "guide.md", Type: "blob", Mode: "100644", Size: 5, SHA: strings.Repeat("e", 40)},
			{Name: "README.md", Type: "blob", Mode: "100644", Size: 5, SHA: strings.Repeat("f", 40)},
		},
		Readme: &api.Readme{Name: "README.md", Contents: "# Read me"},
	}
	// README first, selected by default, rendered from the probed payload
	// (zero blob reads).
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, `aria-current="page"`) {
		t.Error("no selected doc tab")
	}
	if !strings.Contains(body, "<h1>Read me</h1>") {
		t.Error("README body missing")
	}
	if f.view.blobN != 0 {
		t.Errorf("probed README cost %d blob reads, want 0", f.view.blobN)
	}
	// Selecting the other tab fetches the blob and renders markdown.
	f.view.blob = api.BlobResult{Name: "guide.md", Path: "guide.md", Size: 8, Contents: []byte("## Guide")}
	body = f.mustGet("/demo/walhub?doc=guide.md", http.StatusOK)
	if !strings.Contains(body, "<h2>Guide</h2>") {
		t.Error("selected doc body missing")
	}
	if f.view.blobN != 1 {
		t.Errorf("blob reads = %d, want 1", f.view.blobN)
	}
}

func TestRepoTreeEmptyRepoIsAGuide(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	for _, want := range []string{
		`This repository is empty`,
		`git clone http://example.test/demo/walhub.git`,
		`git remote add origin http://example.test/demo/walhub.git`,
		`git push -u origin main`,
		`data-copy="#cmd-clone"`,
		`>empty<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty guide missing %q", want)
		}
	}
	if f.view.resolveN != 0 || f.view.treeN != 0 || f.view.blobN != 0 {
		t.Errorf("empty repo issued reads: resolve=%d tree=%d blob=%d", f.view.resolveN, f.view.treeN, f.view.blobN)
	}
}

func TestRepoEmptyMirrorNeverShowsPushGuide(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{
		Mirror: func(owner, repo string) (api.MirrorView, bool) {
			return api.MirrorView{UpstreamURL: "https://upstream.example/x.git", Schedule: "daily"}, true
		},
	}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "This repository is a mirror") {
		t.Error("mirror guide missing")
	}
	if strings.Contains(body, "git push -u origin main") {
		t.Error("mirror guide rendered push instructions")
	}
}

// --- blob -------------------------------------------------------------------

func TestRepoBlobPlainCode(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{
		Ref: "refs/heads/main", Name: "main.go", Path: "main.go", Size: 25,
		Contents: []byte("package main\n\nfunc main() {}\n"),
	}
	body := f.mustGet("/demo/walhub/blob/main/main.go", http.StatusOK)
	for _, want := range []string{
		`aria-label="File content by line"`,
		`data-line-select`,
		`data-line="1"`,
		`data-line-no`,
		`id="L3"`,
		`tok-kw`,
		`/demo/walhub/api/blob/main/main.go?raw`,
		`<span class="chip">go</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("blob page missing %q", want)
		}
	}
	// "a\nb\n" → 2 rows, no phantom third (splitFileLines).
	rows := byClass(t, body, "blob-row")
	if len(rows) != 3 {
		t.Errorf("blob rows = %d, want 3 (source has 3 lines)", len(rows))
	}
	// The empty line renders <br /> so the row keeps its height.
	if !strings.Contains(body, "<code><br /></code>") {
		t.Error("empty code line did not render <br />")
	}
}

func TestRepoBlobEscapesAndEmptySplit(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Ref: "refs/heads/main", Name: "a.txt", Path: "a.txt", Size: 4, Contents: []byte("a\nb\n")}
	body := f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if rows := byClass(t, body, "blob-row"); len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 for \"a\\nb\\n\"", len(rows))
	}

	f.view.blob = api.BlobResult{Ref: "refs/heads/main", Name: "x.html", Path: "x.html", Size: 20, Contents: []byte("<script>alert(1)</script>")}
	body = f.mustGet("/demo/walhub/blob/main/x.html", http.StatusOK)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("blob page rendered raw script markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("blob page did not escape the code")
	}
	if strings.Contains(body, `/api/blob/main/x.html?raw`) == false {
		t.Error("raw link missing")
	}
}

func TestRepoBlobPlaceholders(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Name: "big.bin", Path: "big.bin", Size: 5 << 20, TooLarge: true}
	body := f.mustGet("/demo/walhub/blob/main/big.bin", http.StatusOK)
	if !strings.Contains(body, "This file is too large to render") {
		t.Error("too-large copy missing")
	}
	if strings.Contains(body, "binary file,") {
		t.Error("too-large page also rendered the binary copy")
	}

	f.view.blob = api.BlobResult{Name: "img.png", Path: "img.png", Size: 2048, Binary: true}
	body = f.mustGet("/demo/walhub/blob/main/img.png", http.StatusOK)
	if !strings.Contains(body, "binary file, 2k") {
		t.Errorf("binary copy missing/wrong: %.400s", body)
	}
	if strings.Contains(body, "too large to render") {
		t.Error("binary page also rendered the too-large copy")
	}
}

func TestRepoBlobMarkdownPreviewAndCode(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Name: "README.md", Path: "README.md", Size: 20, Contents: []byte("# Title\n\n`x`")}
	body := f.mustGet("/demo/walhub/blob/main/README.md", http.StatusOK)
	if !strings.Contains(body, "<h1>Title</h1>") {
		t.Error("markdown preview body missing")
	}
	if !strings.Contains(body, "?view=code") {
		t.Error("code-view link missing")
	}

	body = f.mustGet("/demo/walhub/blob/main/README.md?view=code", http.StatusOK)
	if !strings.Contains(body, "code-view") {
		t.Error("code view did not render")
	}
	if strings.Contains(body, "<h1>Title</h1>") {
		t.Error("code view still rendered the markdown preview")
	}
}

// --- commits ----------------------------------------------------------------

func commitsFixture(sha string) []api.Commit {
	older := strings.Repeat("9", 40)
	return []api.Commit{{
		SHA:         sha,
		Parents:     []string{older},
		Author:      "Ada",
		AuthorEmail: "ada@example.test",
		AuthorDate:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		CommitDate:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Subject:     "fix the thing",
		Body:        "body",
		Trailers:    []api.Trailer{{Key: "Signed-off-by", Value: "Ada <ada@example.test>"}},
	}}
}

func TestRepoCommitsListAndPager(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: commitsFixture(sha), More: true}
	body := f.mustGet("/demo/walhub/commits?path=src&ref=main&skip=35", http.StatusOK)
	for _, want := range []string{
		"fix the thing",
		`/demo/walhub/commit/` + sha,
		"history of <code",
		"1 trailers",
		`aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commits page missing %q", want)
		}
	}
	if got := activeTabOf(t, body); got != "commits" {
		t.Errorf("active tab = %q, want commits", got)
	}
	// The pager keeps path AND ref AND the next skip (the SPA dropped ref).
	for _, want := range []string{"path=src", "ref=main", "skip=36"} {
		if !strings.Contains(body, want) {
			t.Errorf("older link lost %q: %s", want, olderHrefOf(t, body))
		}
	}
}

func olderHrefOf(t *testing.T, body string) string {
	t.Helper()
	for _, n := range byClass(t, body, "pill") {
		if strings.Contains(text(n), "older") {
			return attr(n, "href")
		}
	}
	return ""
}

func TestRepoCommitsEmptyWindowAndGraph(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha}
	body := f.mustGet("/demo/walhub/commits", http.StatusOK)
	if !strings.Contains(body, "No commits in this view.") {
		t.Error("empty window copy missing")
	}
	if strings.Contains(body, "older →") {
		t.Error("pager rendered with more=false")
	}
	if !strings.Contains(body, "divide-y") {
		t.Error("graph-off list lost its row dividers")
	}

	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: commitsFixture(sha)}
	body = f.mustGet("/demo/walhub/commits?graph=1", http.StatusOK)
	if !strings.Contains(body, "graph-on") {
		t.Error("?graph=1 did not add graph-on")
	}
	if strings.Contains(body, "divide-y") {
		t.Error("graph-on list kept row dividers (breaks the rail gutter)")
	}
	if !strings.Contains(body, "commit-rail") || !strings.Contains(body, "graph-dot") {
		t.Error("graph rail markup missing")
	}
	if !strings.Contains(body, "Graph lanes derive per page") {
		t.Error("graph note missing")
	}
}

func TestRepoCommitDetail(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	patch := "diff --git a/a.txt b/a.txt\nindex 111..222 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n hello\n-old\n+new\n"
	f.view.detail = api.CommitDetail{
		Commit: api.Commit{
			SHA:         sha,
			Parents:     []string{strings.Repeat("9", 40), strings.Repeat("8", 40)},
			Author:      "Ada",
			AuthorEmail: "ada@example.test",
			AuthorDate:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			CommitDate:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Subject:     "fix the thing",
			Body:        "see https://example.test/x and " + strings.Repeat("b", 40) + " and <b>x</b>",
			Trailers: []api.Trailer{
				{Key: "Signed-off-by", Value: "Ada <ada@example.test>"},
				{Key: "Merge-Queue-Commit", Value: strings.Repeat("c", 40)},
				{Key: "Custom", Value: "plain text"},
			},
		},
		Stats: []api.Stat{{Path: "a.txt", Additions: 1, Deletions: 1}},
		Patch: patch,
	}
	body := f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	for _, want := range []string{
		"fix the thing",
		"merge parents (2)",
		`href="https://example.test/x"`,
		`/demo/walhub/commit/` + strings.Repeat("b", 40),
		"&lt;b&gt;x&lt;/b&gt;",
		"3 trailers",
		"Merge queue",
		"People",
		"Other",
		"across 1 file (server stats)",
		"+1",
		"−1",
		`id="f-a-txt"`,
		`data-line-select`,
		`diff-add`,
		`diff-del`,
		`class="diff-hunk`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commit page missing %q", want)
		}
	}
	// Unified numbering is display-only: context line 1 on both sides, the
	// replacement is old 2 / new 2.
	if !strings.Contains(body, `<tr class="diff-row" data-line="1" data-side="new">`) {
		t.Error("context row numbering missing")
	}
	if !strings.Contains(body, `href="#L2"`) {
		t.Error("diff gutter anchor missing")
	}
}

func TestRepoCommitBinaryFile(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.detail = api.CommitDetail{
		Commit: api.Commit{SHA: sha, Subject: "img"},
		Stats:  []api.Stat{{Path: "img.png", Additions: -1, Deletions: -1}},
		Patch:  "diff --git a/img.png b/img.png\nnew file mode 100644\nindex 000..111\nBinary files /dev/null and b/img.png differ\n",
	}
	body := f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	if !strings.Contains(body, "Binary file not shown") {
		t.Error("binary file row missing")
	}
	if !strings.Contains(body, "+0") || !strings.Contains(body, "−0") {
		t.Error("binary-only commit did not render the zero diffstat")
	}
}

// --- wal --------------------------------------------------------------------

func walFixture() api.OverviewData {
	return api.OverviewData{
		Repo:     "demo/walhub",
		CloneURL: "http://example.test/demo/walhub.git",
		Hostname: "node-1",
		Health: api.Health{
			Status: "degraded",
			Issues: []string{"pack index mismatch"},
			Deep:   true,
			Suggestions: []api.Suggestion{{
				Op: "fsck", Reason: "missing objects", Auto: true,
			}},
		},
		Fsck: &api.FsckInfo{
			MissingTotal: 25, Missing: []string{strings.Repeat("d", 40)}, Problems: 2,
			RepairedSeq: 7, Upstream: "https://upstream.example/x.git",
			RepairStalled: true,
		},
		Manifest: api.ManifestInfo{
			Version: 4, NextSeq: 11, MinSeq: 2, Entries: 9, TailEntries: 3,
			Segments:   []string{"seg-1", "seg-2", "seg-3", "seg-4", "seg-5", "seg-6", "seg-7"},
			Checkpoint: map[string]any{"seq": uint64(6), "key": "k"},
		},
		Local:       api.LocalInfo{Version: 4, NextSeq: 11, Bootstrap: true, Reconciled: true, SizeBytes: 2048},
		Packs:       api.PacksInfo{Live: 3, LiveBytes: 4096, Pushes: 12},
		Bundles:     []api.BundleInfo{{SHA: strings.Repeat("1", 40), Size: 1024, Strategy: "default", Kind: "full", CreationToken: "t1"}, {SHA: strings.Repeat("2", 40), Size: 512, Strategy: "default", Kind: "incremental", BaseID: strings.Repeat("f", 40), CreationToken: "t2"}},
		BundlePlan:  api.BundlePlanInfo{Slots: []api.PlanSlot{{Strategy: "default", Slot: 1, Status: "built", Detail: "ok", BundleID: strings.Repeat("1", 40)}, {Strategy: "default", Slot: 2, Status: "too_small"}, {Strategy: "default", Slot: 3, Status: "error"}}},
		Compactions: []api.CompactionInfo{{Seq: 3, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Before: 5, After: 1, Hostname: "node-1"}},
	}
}

func TestRepoWALRendersCards(t *testing.T) {
	f := newRepoFixture(t, true)
	f.tasks = &repoFakeTasks{
		ops: []api.OpSpec{
			{Op: "fsck", Params: []api.OpParam{{Name: "connectivity", Values: []string{"1"}}, {Name: "pack"}}},
			{Op: "bundle", Params: []api.OpParam{{Name: "strategy", Values: []string{"default", "shallow"}}}},
		},
		recent: []api.TaskRecord{{Kind: "fsck", Summary: "audited", Finished: "2026-01-02T03:04:05Z"}},
	}
	f.s = f.newServer(f.cfgForTest())
	f.view.overview = walFixture()
	body := f.mustGet("/demo/walhub/wal", http.StatusOK)

	for _, want := range []string{
		"node-1",
		"pack index mismatch",
		"deep fsck:",
		"fsck",
		"missing objects",
		"25 missing objects",
		"repair stalled",
		"3 live packs · 4.0 KiB · 12 pushes recorded",
		"no issues",
		"base missing",
		"built 1 · skipped 0 · too-small 1 · unavailable 1",
		"seg-7",
		"all",
		"connectivity",
		"(required)",
		"one of default",
		"recent ops",
	} {
		if strings.Contains(body, want) == false {
			// "no issues" is absent when issues exist; handled below.
			if want == "no issues" {
				continue
			}
			t.Errorf("wal page missing %q", want)
		}
	}
	if strings.Contains(body, "no issues") {
		t.Error("wal page rendered 'no issues' while health carries issues")
	}
	if strings.Contains(body, "seg-1") {
		t.Error("segments default should be the newest 5 only")
	}
	// ?all=1 widens the segment table.
	body = f.mustGet("/demo/walhub/wal?all=1", http.StatusOK)
	if !strings.Contains(body, "seg-1") || !strings.Contains(body, "newest 5") {
		t.Error("?all=1 did not widen the segment table")
	}
	if got := activeTabOf(t, body); got != "settings" {
		t.Errorf("wal active tab = %q, want settings (#123)", got)
	}
}

func TestRepoWALWithoutFsckOrBundles(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.overview = api.OverviewData{
		Hostname: "node-1",
		Health:   api.Health{Status: "ok", Issues: []string{}},
	}
	body := f.mustGet("/demo/walhub/wal", http.StatusOK)
	if strings.Contains(body, "Object health") {
		t.Error("fsck card rendered with no report")
	}
	if !strings.Contains(body, "no issues") {
		t.Error("healthy wal page should say no issues")
	}
	if !strings.Contains(body, "no bundles yet") {
		t.Error("empty bundle copy missing")
	}
	if !strings.Contains(body, "no segments recorded") {
		t.Error("empty segment copy missing")
	}
	if !strings.Contains(body, "0 available · 0 recent") {
		t.Error("ops counts missing")
	}
}

// --- gates and 404s ---------------------------------------------------------

func TestRepoMissingRepoIs404(t *testing.T) {
	f := newRepoFixture(t, true)
	code, _ := f.get("/demo/nope")
	if code != http.StatusNotFound {
		t.Errorf("missing repo = %d, want 404", code)
	}
	// The provider answered with a real page (never the SPA fallback's 200).
	_, body := f.get("/demo/nope")
	if !strings.Contains(body, "repository not found") {
		t.Errorf("404 body is not the rendered error page: %.300s", body)
	}
}

func TestRepoMissingRefOrPathIs404(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: nope", api.ErrNotFound)
	code, _ := f.get("/demo/walhub/tree/nope")
	if code != http.StatusNotFound {
		t.Errorf("missing ref = %d, want 404", code)
	}
	f.view.resolveErr = nil
	f.view.treeErr = fmt.Errorf("%w: tree main:missing", api.ErrNotFound)
	code, _ = f.get("/demo/walhub/tree/main/missing")
	if code != http.StatusNotFound {
		t.Errorf("missing path = %d, want 404", code)
	}
	// A bare ref is not a blob.
	code, _ = f.get("/demo/walhub/blob/main")
	if code != http.StatusNotFound {
		t.Errorf("blob without a path = %d, want 404", code)
	}
}

func TestRepoVisibilityGate(t *testing.T) {
	// No identity surface + anonymous_read=false + anonymous → 401.
	f := newRepoFixture(t, false)
	if code, _ := f.get("/demo/walhub"); code != http.StatusUnauthorized {
		t.Errorf("anonymous with anonymous_read=false = %d, want 401", code)
	}

	// Identity verdict refuses → 404, never leaking the repo.
	f = newRepoFixture(t, true)
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no access"}
	}
	f.rebind()
	if code, _ := f.get("/demo/walhub"); code != http.StatusNotFound {
		t.Errorf("visibility denial = %d, want 404", code)
	}

	// Public repo admitted by the identity gate even with anonymous_read off
	// (Forgejo #345: visibility decides before the flag).
	f = newRepoFixture(t, false)
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError { return nil }
	f.rebind()
	if code, _ := f.get("/demo/walhub"); code != http.StatusOK {
		t.Errorf("public repo under anonymous_read=false = %d, want 200", code)
	}
}

// cfgForTest rebuilds the boot config so a test can swap the task surface
// (the Tasks seam is read at New time through api.Env).
func (f *repoFixture) cfgForTest() *config.Config {
	cfg := &config.Config{}
	cfg.Server.Auth.AnonymousRead = f.anonRead
	cfg.Server.PublicURL = "http://example.test"
	return cfg
}

func TestRepoMethodNotAllowed(t *testing.T) {
	// The page mux owns GET/HEAD only. A write method is NOT answered by the
	// provider (Serve → false), so the server's repo dispatch keeps answering
	// 405 + Allow: GET, HEAD — the exact wire contract from before the move.
	f := newRepoFixture(t, true)
	req := httptest.NewRequest(http.MethodPost, "/demo/walhub", nil)
	rec := httptest.NewRecorder()
	if f.s.Serve(rec, req) {
		t.Fatalf("POST /demo/walhub was answered by the page provider (%d)", rec.Code)
	}
}

func TestRepoDotGitPaths(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Ref: "refs/heads/main", Entries: []api.TreeEntry{{Name: "a.go", Type: "blob", Mode: "100644", Size: 3, SHA: strings.Repeat("c", 40)}}}
	body := f.mustGet("/demo/walhub.git", http.StatusOK)
	if !strings.Contains(body, "/demo/walhub/blob/main/a.go") {
		t.Error(".git root did not render the repo (links lost the stripped name)")
	}
	if code, _ := f.get("/demo/walhub.git/tree/main"); code != http.StatusOK {
		t.Errorf("/o/r.git/tree/main = %d, want 200", code)
	}
}

// --- unit helpers -----------------------------------------------------------

func TestRepoTabSection(t *testing.T) {
	cases := map[string]string{
		"/o/r":                         "code",
		"/o/r/tree/main":               "code",
		"/o/r/blob/main/cmd/checks.go": "code", // #25: filenames never select tabs
		"/o/r/commits":                 "commits",
		"/o/r/commit/abc":              "commits",
		"/o/r/issues/1":                "issues",
		"/o/r/labels":                  "issues",
		"/o/r/pulls/2":                 "pulls",
		"/o/r/pull/2":                  "pulls",
		"/o/r/checks":                  "checks",
		"/o/r/releases":                "releases",
		"/o/r/wal":                     "settings",
		"/o/r/settings":                "settings",
		"/o/r/unknown":                 "code",
	}
	for path, want := range cases {
		if got := repoTabSection(path); got != want {
			t.Errorf("repoTabSection(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRepoTabGating(t *testing.T) {
	f := newRepoFixture(t, true)
	feats := config.AllFeatures()
	feats.Issues = false
	feats.Releases = false
	f.view.summary.Features = &feats
	f.view.summary.Health = api.RepoHealthHealthy
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if got := strings.Join(tabIDs(t, body), ","); got != "code,commits,pulls,settings" {
		t.Errorf("tabs = %s, want code,commits,pulls,settings", got)
	}

	// A feature-disabled section re-highlights onto Code (#522). The path is
	// owned by the collab group, so assert on the chrome model directly.
	req := httptest.NewRequest(http.MethodGet, "/demo/walhub/issues", nil)
	c := f.s.buildRepoChrome(req, repoRequest{id: git.RepoId{Owner: "demo", Name: "walhub"}, full: "demo/walhub"}, f.view.summary)
	if got := activeTabOfRepoChrome(c); got != "code" {
		t.Errorf("active tab on disabled /issues = %q, want code", got)
	}

	// ...while an explicit has_checks=true shows the Checks tab.
	f = newRepoFixture(t, true)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{
		ChecksSummary: func(owner, repo string) (api.ChecksSummary, bool) {
			return api.ChecksSummary{HasChecks: true}, true
		},
		CollabCounts: func(owner, repo string) (api.CollabCounts, bool) {
			return api.CollabCounts{OpenIssues: 3, OpenPulls: 1}, true
		},
	}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	ids := strings.Join(tabIDs(t, body), ",")
	if !strings.Contains(ids, "checks") {
		t.Errorf("Checks tab missing with has_checks=true: %s", ids)
	}
	if !strings.Contains(body, `aria-label="3 open"`) || !strings.Contains(body, `aria-label="1 open"`) {
		t.Error("tab badges missing (counts > 0)")
	}
}

func TestRepoChromeProjectsHeader(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary.Description = "a tiny repo"
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{
		Visibility: func(owner, repo string) (string, bool) { return "private", true },
		ForkInfo: func(owner, repo string) (api.ForkSummary, bool) {
			return api.ForkSummary{Parent: "up/parent", Count: 4}, true
		},
		PushMirror: func(owner, repo string) (api.PushMirrorView, bool) {
			return api.PushMirrorView{UpstreamURL: "https://push.example/x.git", Due: true, LastSyncedAt: "2026-01-01T00:00:00Z"}, true
		},
		Fsck: func(owner, repo string) (api.FsckInfo, bool) {
			return api.FsckInfo{MissingTotal: 9}, true
		},
	}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	for _, want := range []string{
		"a tiny repo",
		`title="private — owner or organization members only"`,
		"forked from",
		"/up/parent",
		"mirror · push",
		"sync due",
		"9 missing",
		"Settings → WAL",
		"http://example.test/demo/walhub.git",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header missing %q", want)
		}
	}
}

func TestRepoHeadPill(t *testing.T) {
	f := newRepoFixture(t, true)
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, `title="refs/heads/main">main</span>`) {
		t.Error("head pill did not show the short ref with the full name as title")
	}
	// An unborn repo shows the empty pill (and the guide).
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, `<span class="pill">empty</span>`) {
		t.Error("unborn repo did not render the empty pill")
	}
}

func TestRepoLinkify(t *testing.T) {
	sha40 := strings.Repeat("a", 40)
	sha64 := strings.Repeat("b", 64)
	got := string(repoLinkify("hi https://x.test/y. "+sha40+" "+sha64+" <b>", "/o/r"))
	for _, want := range []string{
		`<a href="https://x.test/y" rel="noopener">https://x.test/y</a>.`,
		`href="/o/r/commit/` + sha40 + `"`,
		`href="/o/r/commit/` + sha64 + `"`,
		`&lt;b&gt;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("linkify missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<b>") {
		t.Error("linkify passed raw markup through")
	}
}

func TestRepoGroupTrailers(t *testing.T) {
	groups := groupTrailers([]api.Trailer{
		{Key: "Custom", Value: "plain"},
		{Key: "Co-authored-by", Value: "Ada <ada@example.test>"},
		{Key: "merge-queue-sha", Value: strings.Repeat("c", 40)},
	})
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].Name != "Merge queue" || groups[1].Name != "People" || groups[2].Name != "Other" {
		t.Errorf("group order = %s/%s/%s", groups[0].Name, groups[1].Name, groups[2].Name)
	}
	if groups[1].Rows[0].Email != "ada@example.test" {
		t.Errorf("mail trailer not split: %+v", groups[1].Rows[0])
	}
	if groups[0].Rows[0].SHA == "" {
		t.Error("sha trailer not detected")
	}
}

func TestRepoFormatters(t *testing.T) {
	if got := fmtSize(92); got != "92b" {
		t.Errorf("fmtSize(92) = %s", got)
	}
	if got := fmtSize(48400); got != "47.3k" {
		t.Errorf("fmtSize(48400) = %s", got)
	}
	if got := fmtSize(-1); got != "?" {
		t.Errorf("fmtSize(-1) = %s", got)
	}
	num, unit := fmtSizeParts(1500)
	if num != "1.5" || unit != "KB" {
		t.Errorf("fmtSizeParts(1500) = %s/%s", num, unit)
	}
	if got := fmtBytes(4096); got != "4.0 KiB" {
		t.Errorf("fmtBytes(4096) = %s", got)
	}
	if got := fmtBytes(-1); got != "—" {
		t.Errorf("fmtBytes(-1) = %s", got)
	}
	if got := fmtMode("100644", "blob"); got != ".rw-r--r--" {
		t.Errorf("fmtMode(100644) = %s", got)
	}
	if got := fmtMode("040000", "tree"); got != "drwxr-xr-x" {
		t.Errorf("fmtMode(040000) = %s", got)
	}
	if got := fmtMode("120000", "blob"); got != "lrwxrwxrwx" {
		t.Errorf("fmtMode(120000) = %s", got)
	}
	if got := fmtMode("160000", "commit"); got != "m---------" {
		t.Errorf("fmtMode(160000) = %s", got)
	}
	if got := fileAnchor("src/a b.go"); got != "f-src-a-b-go" {
		t.Errorf("fileAnchor = %s", got)
	}
	if got := shortRefName("refs/heads/feature/x"); got != "feature/x" {
		t.Errorf("shortRefName = %s", got)
	}
	if got := escapePath("a b/c?d"); got != "a%20b/c%3Fd" {
		t.Errorf("escapePath = %s", got)
	}
}

func TestNextSyncText(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		m    api.MirrorView
		want string
	}{
		{"due", api.MirrorView{Due: true, LastSyncedAt: "x"}, "sync due"},
		{"never", api.MirrorView{}, "never synced — first sync pending"},
		{"minutes", api.MirrorView{NextSyncAt: now.Add(30 * time.Minute).Format(time.RFC3339)}, "next sync in 30m"},
		{"hours", api.MirrorView{NextSyncAt: now.Add(5 * time.Hour).Format(time.RFC3339)}, "next sync in 5h"},
		{"days", api.MirrorView{NextSyncAt: now.Add(72 * time.Hour).Format(time.RFC3339)}, "next sync in 3d"},
		{"unknown", api.MirrorView{NextSyncAt: "not-a-time"}, "next sync unknown"},
	}
	for _, tc := range cases {
		if got := nextSyncText(tc.m, now); got != tc.want {
			t.Errorf("%s: nextSyncText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDocCandidates(t *testing.T) {
	entries := []api.TreeEntry{
		{Name: "zzz.md", Type: "blob"},
		{Name: "README.md", Type: "blob"},
		{Name: "b.markdown", Type: "blob"},
		{Name: "not-md.txt", Type: "blob"},
		{Name: "dir", Type: "tree"},
	}
	got := docCandidates(entries, &api.Readme{Name: "readme", Contents: "x"})
	want := []string{"README.md", "b.markdown", "zzz.md", "readme"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("docCandidates = %v, want %v", got, want)
	}
}

// --- error paths and remaining branches -------------------------------------

func TestRepoBindRepo(t *testing.T) {
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	d := RepoDeps{Visibility: func(owner, repo string) (string, bool) { return "public", true }}
	BindRepo(d)
	if repoDeps.Visibility == nil {
		t.Fatal("BindRepo did not install the projections")
	}
}

func TestRepoOpenErrorPaths(t *testing.T) {
	// No RepoView wired → 503 (the page cannot pretend the repo is empty).
	f := newRepoFixture(t, true)
	cfg := f.cfgForTest()
	s, err := New(Env{Cfg: cfg, API: &api.Env{Cfg: cfg}, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.BindAuth(pages.Auth{Authenticate: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), nil
	}})
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/demo/walhub", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no RepoView = %d, want 503", rec.Code)
	}

	// Registry failure → 503; Sync failure → 503; Summary not-found → 404.
	f = newRepoFixture(t, true)
	f.reg.err = errors.New("store down")
	if code, _ := f.get("/demo/walhub"); code != http.StatusServiceUnavailable {
		t.Errorf("registry error = %d, want 503", code)
	}
	f = newRepoFixture(t, true)
	f.view.syncErr = fmt.Errorf("%w: sync", api.ErrPending)
	if code, _ := f.get("/demo/walhub"); code != http.StatusServiceUnavailable {
		t.Errorf("sync error = %d, want 503", code)
	}
	f = newRepoFixture(t, true)
	f.view.summaryErr = fmt.Errorf("%w: gone", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub"); code != http.StatusNotFound {
		t.Errorf("summary not found = %d, want 404", code)
	}
	// Overview failure on /wal.
	f = newRepoFixture(t, true)
	f.view.overviewErr = errors.New("boom")
	if code, _ := f.get("/demo/walhub/wal"); code != http.StatusServiceUnavailable {
		t.Errorf("overview error = %d, want 503", code)
	}
}

func TestRepoErrorTextTwin(t *testing.T) {
	f := newRepoFixture(t, true)
	req := httptest.NewRequest(http.MethodGet, "/demo/nope", nil)
	req.Header.Set("Accept", "text/plain")
	rec := httptest.NewRecorder()
	f.s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing repo = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Accept: text/plain got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "repository not found") {
		t.Errorf("text twin body = %q", rec.Body.String())
	}
}

func TestRepoChromeFor(t *testing.T) {
	f := newRepoFixture(t, true)
	req := httptest.NewRequest(http.MethodGet, "/demo/walhub/issues", nil)
	rec := httptest.NewRecorder()
	c, ok := f.s.RepoChromeFor(rec, req, "demo", "walhub")
	if !ok || c == nil || c.Full != "demo/walhub" {
		t.Fatalf("RepoChromeFor = %+v, %v", c, ok)
	}
	if active := activeTabOfRepoChrome(c); active != "code" && active != "issues" {
		t.Errorf("chrome tab = %q", active)
	}
	// A visibility denial is a 404 (no existence leak).
	f.checkRead = func(context.Context, string, string, auth.Principal) *auth.AuthError {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "no access"}
	}
	f.rebind()
	rec = httptest.NewRecorder()
	if _, ok := f.s.RepoChromeFor(rec, req, "demo", "walhub"); ok {
		t.Error("RepoChromeFor answered for a denied viewer")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("denied chrome = %d, want 404", rec.Code)
	}
	f.checkRead = nil
	f.rebind()
	// Unknown repo → 404 written, ok=false.
	rec = httptest.NewRecorder()
	if _, ok := f.s.RepoChromeFor(rec, req, "demo", "nope"); ok {
		t.Error("RepoChromeFor answered for a missing repo")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing repo chrome = %d, want 404", rec.Code)
	}
	// Malformed id → 404.
	rec = httptest.NewRecorder()
	if _, ok := f.s.RepoChromeFor(rec, req, "..", "x"); ok {
		t.Error("RepoChromeFor accepted a malformed owner")
	}
}

func TestRepoBaseAndSSHURL(t *testing.T) {
	f := newRepoFixture(t, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "example.test"
	if got := f.s.repoBaseURL(req); got != "http://example.test" {
		t.Errorf("public_url base = %s", got)
	}
	f.s.env.Cfg.Server.PublicURL = ""
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := f.s.repoBaseURL(req); got != "https://example.test" {
		t.Errorf("forwarded base = %s", got)
	}
	req.Header.Del("X-Forwarded-Proto")
	req.TLS = &tls.ConnectionState{}
	if got := f.s.repoBaseURL(req); got != "https://example.test" {
		t.Errorf("TLS base = %s", got)
	}
	// SSH: no advertised port → no URL; a port → ssh:// with the port; 22
	// omits the port segment.
	req.TLS = nil
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "" {
		t.Errorf("SSH URL without a port = %q", got)
	}
	f.s.env.Cfg.Server.SSH.Listen = "0.0.0.0:2222"
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "ssh://git@example.test:2222/demo/walhub.git" {
		t.Errorf("SSH URL = %s", got)
	}
	f.s.env.Cfg.Server.SSH.Listen = "0.0.0.0:22"
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "ssh://git@example.test/demo/walhub.git" {
		t.Errorf("SSH URL on 22 = %s", got)
	}
	f.s.env.Cfg.Server.SSH.ExternalPort = 70000
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "" {
		t.Errorf("out-of-range port should not advertise: %q", got)
	}
}

func TestRepoMirrorGuideWording(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{Mirror: func(owner, repo string) (api.MirrorView, bool) {
		return api.MirrorView{
			UpstreamURL:  "https://upstream.example/x.git",
			LastSyncedAt: "2026-01-01T00:00:00Z",
			LastResult:   "failed",
			Due:          true,
		}, true
	}}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "Last synced 2026-01-01T00:00:00Z") {
		t.Error("mirror last-synced line missing")
	}
	if !strings.Contains(body, "last sync: failed") {
		t.Error("mirror failure line missing")
	}
}

func TestRepoPlaceholderGuide(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{
		Placeholder: func(owner, repo string) (RepoPlaceholder, bool) {
			return RepoPlaceholder{CreatedBy: "ada", CreatedAt: "2026-01-01", ExpiresAt: "2026-02-01"}, true
		},
		Mirror: func(owner, repo string) (api.MirrorView, bool) { return api.MirrorView{}, false },
	}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	for _, want := range []string{"reserved by ada", "expires 2026-02-01"} {
		if !strings.Contains(body, want) {
			t.Errorf("placeholder guide missing %q", want)
		}
	}
}

func TestRepoBlobErrorAndEmpty(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.blobErr = fmt.Errorf("%w: object", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub/blob/main/a.txt"); code != http.StatusNotFound {
		t.Errorf("blob not found = %d, want 404", code)
	}
	f = newRepoFixture(t, true)
	f.view.blobErr = errors.New("object store closed")
	if code, _ := f.get("/demo/walhub/blob/main/a.txt"); code != http.StatusServiceUnavailable {
		t.Errorf("blob failure = %d, want 503", code)
	}
	// Empty repo → the guide, no blob read.
	f = newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("empty repo blob did not render the guide")
	}
	if f.view.blobN != 0 {
		t.Errorf("empty repo blob reads = %d, want 0", f.view.blobN)
	}
	// Unresolved ref (raw sha view) → no raw link.
	f = newRepoFixture(t, true)
	f.view.resolution = api.Resolution{SHA: strings.Repeat("a", 40), Kind: "commit"}
	f.view.blob = api.BlobResult{Name: "a.txt", Path: "a.txt", Size: 1, Contents: []byte("x")}
	body = f.mustGet("/demo/walhub/blob/"+strings.Repeat("a", 40)+"/a.txt", http.StatusOK)
	if strings.Contains(body, "?raw") {
		t.Error("raw link rendered for an unresolved ref")
	}
	// Nested path derives the display name and breadcrumb.
	f = newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Ref: "refs/heads/main", Path: "src/deep/a.txt", Size: 1, Contents: []byte("x")}
	body = f.mustGet("/demo/walhub/blob/main/src/deep/a.txt", http.StatusOK)
	for _, want := range []string{"<h2 class=\"font-semibold\">a.txt</h2>", "/demo/walhub/tree/main/src/deep"} {
		if !strings.Contains(body, want) {
			t.Errorf("nested blob missing %q", want)
		}
	}
}

func TestRepoCommitsErrorAndEmpty(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.commitsErr = fmt.Errorf("%w: no such ref", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub/commits"); code != http.StatusNotFound {
		t.Errorf("commits not found = %d, want 404", code)
	}
	f = newRepoFixture(t, true)
	f.view.commitsErr = errors.New("engine down")
	if code, _ := f.get("/demo/walhub/commits"); code != http.StatusServiceUnavailable {
		t.Errorf("commits failure = %d, want 503", code)
	}
	f = newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := f.mustGet("/demo/walhub/commits", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("empty repo commits did not render the guide")
	}
	if f.view.commitsN != 0 {
		t.Errorf("empty repo commits reads = %d, want 0", f.view.commitsN)
	}
	// A commit resolution failure that carries the empty marker renders the
	// guide, not a 404.
	f = newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub/commits", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("unborn marker did not render the guide")
	}
}

func TestRepoCommitFlagsAndNoMessage(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.detail = api.CommitDetail{
		Commit: api.Commit{SHA: sha, Subject: ""},
		Patch:  "diff --git a/old.txt b/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\ndiff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\ndiff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+y\n",
	}
	body := f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	for _, want := range []string{"(no message)", "renamed from old.txt", "deleted", "added"} {
		if !strings.Contains(body, want) {
			t.Errorf("commit flags missing %q", want)
		}
	}
	// Resolve / Commit failures map to the shared statuses.
	f.view.resolveErr = fmt.Errorf("%w: sha", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub/commit/" + sha); code != http.StatusNotFound {
		t.Errorf("commit resolve failure = %d, want 404", code)
	}
	f.view.resolveErr = nil
	f.view.commitErr = errors.New("engine down")
	if code, _ := f.get("/demo/walhub/commit/" + sha); code != http.StatusServiceUnavailable {
		t.Errorf("commit failure = %d, want 503", code)
	}
	// Empty repo → guide.
	f = newRepoFixture(t, true)
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body = f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("empty repo commit did not render the guide")
	}
}

func TestRepoWALHelpers(t *testing.T) {
	if got := anyLabel(nil, "@"); got != "" {
		t.Errorf("anyLabel(nil) = %q", got)
	}
	if got := anyLabel(map[string]any{"seq": uint64(3)}, "@"); got != "@3" {
		t.Errorf("anyLabel(seq) = %q", got)
	}
	if got := anyLabel(map[string]any{"packs": []any{1, 2}}, ""); got != "2" {
		t.Errorf("anyLabel(packs) = %q", got)
	}
	if got := anyLabel("plain", ""); got != "plain" {
		t.Errorf("anyLabel(string) = %q", got)
	}
	if got := anyLabel(7, ""); got != "7" {
		t.Errorf("anyLabel(int) = %q", got)
	}
	if got := anyLabel(map[string]any{"key": "x"}, "@"); got != "" {
		t.Errorf("anyLabel(unknown map) = %q", got)
	}

	full := api.BundleInfo{SHA: strings.Repeat("1", 40), Kind: "full", Strategy: "s", CreationToken: "t2"}
	inc := api.BundleInfo{SHA: strings.Repeat("2", 40), Kind: "incremental", Strategy: "s", BaseID: full.SHA, CreationToken: "t1"}
	inc2 := api.BundleInfo{SHA: strings.Repeat("3", 40), Kind: "incremental", Strategy: "s", BaseID: full.SHA, CreationToken: "t0"}
	nodes := bundleNodes([]api.BundleInfo{full, inc, inc2})
	if len(nodes) != 1 || len(nodes[0].Children) != 2 {
		t.Fatalf("bundle tree = %+v", nodes)
	}
	if nodes[0].Children[0].Token != "t0" {
		t.Error("children are not creation_token sorted")
	}

	for _, tc := range []struct{ status, want string }{
		{"ok", "chip chip-open"}, {"built", "chip chip-open"},
		{"error", "chip chip-closed"}, {"failed", "chip chip-closed"},
		{"unavailable", "chip chip-closed"}, {"weird", "chip chip-closed"},
		{"degraded", "chip chip-draft"}, {"skipped", "chip chip-draft"},
		{"too_small", "chip chip-draft"}, {"too-small", "chip chip-draft"},
	} {
		if got := chipTone(tc.status); got != tc.want {
			t.Errorf("chipTone(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestRepoFormatterEdges(t *testing.T) {
	if got := fmtMode("755", "blob"); got != ".rwxr-xr-x" {
		t.Errorf("fmtMode(755) = %q", got)
	}
	if got := fmtMode("", "blob"); got != "" {
		t.Errorf("fmtMode(empty) = %q", got)
	}
	if got := fmtMode("wat", "blob"); got != "" {
		t.Errorf("fmtMode(wat) = %q", got)
	}
	if num, unit := fmtSizeParts(-1); num != "?" || unit != "" {
		t.Errorf("fmtSizeParts(-1) = %q/%q", num, unit)
	}
	num, unit := fmtSizeParts(2 << 20)
	if num != "2" || unit != "MB" {
		t.Errorf("fmtSizeParts(2MiB) = %q/%q", num, unit)
	}
	if got := fmtBytes(150 * 1024); got != "150 KiB" {
		t.Errorf("fmtBytes(150KiB) = %q", got)
	}
	if got := escapePath(""); got != "" {
		t.Errorf("escapePath(empty) = %q", got)
	}
}

func TestRepoDocHelpers(t *testing.T) {
	if got := docPath("", "README.md"); got != "README.md" {
		t.Errorf("docPath(root) = %q", got)
	}
	if got := docPath("pkg", "README.md"); got != "pkg/README.md" {
		t.Errorf("docPath(pkg) = %q", got)
	}
	if containsString([]string{"a"}, "") || !containsString([]string{"a"}, "a") {
		t.Error("containsString mis-answered")
	}
	if lessFold("B", "b") != true {
		t.Error("lessFold tie-break should fall back to the raw spelling")
	}
}

func TestRepoGateAndChromeEdges(t *testing.T) {
	f := newRepoFixture(t, true)
	// A repo segment that trims to nothing is a malformed id → 404.
	if code, _ := f.get("/demo/.git"); code != http.StatusNotFound {
		t.Errorf("/demo/.git = %d, want 404", code)
	}
	// Head with no ref name falls back to the short sha.
	f.view.summary.Head = &api.Ref{SHA: strings.Repeat("a", 40)}
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, ">aaaaaaaaaa</span>") {
		t.Error("head pill did not fall back to the short sha")
	}
	// An https advertisement is labelled HTTPS (never rewritten).
	f.s.env.Cfg.Server.PublicURL = "https://secure.example"
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "https://secure.example/demo/walhub.git") {
		t.Error("https clone URL not advertised verbatim")
	}
	if !strings.Contains(body, ">HTTPS clone<") {
		t.Error("clone protocol label did not derive from the scheme")
	}
	// Health "" with no head and zero refs is the empty state.
	f = newRepoFixture(t, true)
	f.view.summary = api.SummaryData{}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("refs==0 health-less summary did not render the guide")
	}
	// RepoChromeFor: anonymous + anonymous_read=false + no identity surface
	// is 401 (the gated rule), not a silent chrome.
	f = newRepoFixture(t, false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/walhub", nil)
	if _, ok := f.s.RepoChromeFor(rec, req, "demo", "walhub"); ok {
		t.Error("RepoChromeFor answered for a gated anonymous viewer")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("gated chrome = %d, want 401", rec.Code)
	}
}

func TestRepoTreeAndBlobMoreErrors(t *testing.T) {
	// Tree failure (generic) → 503; doc blob failure → 404.
	f := newRepoFixture(t, true)
	f.view.treeErr = errors.New("engine down")
	if code, _ := f.get("/demo/walhub"); code != http.StatusServiceUnavailable {
		t.Errorf("tree failure = %d, want 503", code)
	}
	f = newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Entries: []api.TreeEntry{{Name: "README.md", Type: "blob"}}}
	f.view.blobErr = fmt.Errorf("%w: readme", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub?doc=README.md"); code != http.StatusNotFound {
		t.Errorf("doc blob failure = %d, want 404", code)
	}
	// Unborn marker on resolve/tree still renders the guide.
	f = newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body := f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("unborn resolve did not render the guide")
	}
	f = newRepoFixture(t, true)
	f.view.treeErr = fmt.Errorf("%w: %stree", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("unborn tree did not render the guide")
	}
	// Submodule entry (type commit) renders the arrow and a tree link; a blob
	// with size -1 renders the "-" placeholder.
	f = newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Entries: []api.TreeEntry{
		{Name: "sub", Type: "commit", Mode: "160000", Size: -1},
		{Name: "weird", Type: "blob", Mode: "100755", Size: -1},
	}}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "↗") || !strings.Contains(body, "m---------") {
		t.Error("submodule row missing its icon/mode")
	}
	if !strings.Contains(body, ">-</td>") {
		t.Error("unknown blob size did not render the - placeholder")
	}

	// Blob: unborn marker on resolve → guide; generic resolve error → 503.
	f = newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("unborn blob resolve did not render the guide")
	}
	f = newRepoFixture(t, true)
	f.view.resolveErr = errors.New("engine down")
	if code, _ := f.get("/demo/walhub/blob/main/a.txt"); code != http.StatusServiceUnavailable {
		t.Errorf("blob resolve failure = %d, want 503", code)
	}
}

func TestRepoCommitsAndCommitMoreErrors(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.resolveErr = errors.New("engine down")
	if code, _ := f.get("/demo/walhub/commits"); code != http.StatusServiceUnavailable {
		t.Errorf("commits resolve failure = %d, want 503", code)
	}
	f = newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body := f.mustGet("/demo/walhub/commits", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("commits unborn marker did not render the guide")
	}
	// Unknown ref, no marker → 404.
	f = newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: nope", api.ErrNotFound)
	if code, _ := f.get("/demo/walhub/commits"); code != http.StatusNotFound {
		t.Errorf("commits missing ref = %d, want 404", code)
	}
	// Commit page: unborn marker on resolve and on the detail read.
	f = newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("commit unborn resolve did not render the guide")
	}
	f = newRepoFixture(t, true)
	f.view.commitErr = fmt.Errorf("%w: %sobject", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub/commit/"+sha, http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("commit unborn detail did not render the guide")
	}
}

func TestRepoWALFsckSampleAndTasks(t *testing.T) {
	f := newRepoFixture(t, true)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ov := walFixture()
	ov.Fsck = &api.FsckInfo{MissingTotal: 40, At: &at, RepairStalled: false}
	for i := 0; i < 30; i++ {
		ov.Fsck.Missing = append(ov.Fsck.Missing, strings.Repeat("e", 40))
	}
	ov.Health.Issues = nil
	f.view.overview = ov
	f.tasks = &repoFakeTasks{}
	f.s = f.newServer(f.cfgForTest())
	body := f.mustGet("/demo/walhub/wal", http.StatusOK)
	if !strings.Contains(body, "…and 20 more") {
		t.Errorf("missing-oid overflow line absent: %.300s", body)
	}
	if !strings.Contains(body, `datetime="2026-01-02T03:04:05Z"`) {
		t.Error("audit stamp missing")
	}
	if !strings.Contains(body, "no issues") {
		t.Error("empty issues list did not render the copy")
	}
}

func TestRepoLinkifyWithoutRepoBase(t *testing.T) {
	got := string(repoLinkify("sha "+strings.Repeat("a", 40), ""))
	if strings.Contains(got, "/commit/") {
		t.Errorf("repo-less linkify linked a sha: %s", got)
	}
	if !strings.Contains(got, strings.Repeat("a", 40)) {
		t.Error("repo-less linkify dropped the sha text")
	}
}

func TestRepoHelperRemainders(t *testing.T) {
	if containsString([]string{"a"}, "b") {
		t.Error("containsString found a missing value")
	}
	if got := fmtSize(1 << 50); got == "?" || !strings.HasSuffix(got, "PB") {
		t.Errorf("fmtSize(1<<50) = %q", got)
	}
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	if got := nextSyncText(api.MirrorView{NextSyncAt: now.Add(20 * time.Second).Format(time.RFC3339)}, now); got != "next sync < 1m" {
		t.Errorf("sub-minute next sync = %q", got)
	}
	// repoSSHURL: an unparseable base has no host → no advertisement; an IPv6
	// host on the default port stays bracketed.
	f := newRepoFixture(t, true)
	f.s.env.Cfg.Server.PublicURL = "http://"
	f.s.env.Cfg.Server.SSH.Listen = "0.0.0.0:22"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "" {
		t.Errorf("host-less SSH URL = %q", got)
	}
	f.s.env.Cfg.Server.PublicURL = ""
	req.Host = "[::1]:8080"
	if got := f.s.repoSSHURL(req, "demo/walhub"); got != "ssh://git@[::1]/demo/walhub.git" {
		t.Errorf("IPv6 SSH URL = %q", got)
	}
}

func TestRepoDocPlaceholders(t *testing.T) {
	f := newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Entries: []api.TreeEntry{{Name: "README.md", Type: "blob"}}}
	f.view.blob = api.BlobResult{Name: "README.md", Path: "README.md", Size: 3 << 20, TooLarge: true}
	body := f.mustGet("/demo/walhub?doc=README.md", http.StatusOK)
	if !strings.Contains(body, "This file is too large to render") {
		t.Error("doc too-large placeholder missing")
	}
	f.view.blob = api.BlobResult{Name: "README.md", Path: "README.md", Size: 1024, Binary: true}
	body = f.mustGet("/demo/walhub?doc=README.md", http.StatusOK)
	if !strings.Contains(body, "binary file, 1k") {
		t.Error("doc binary placeholder missing")
	}
	// A blob read that answers the unborn marker renders the guide, not 404.
	f.view.blobErr = fmt.Errorf("%w: %sreadme", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub?doc=README.md", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("doc unborn marker did not render the guide")
	}
}

func TestRepoCommitsAndWALRemainders(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	// A page whose Ref is empty falls back to the short sha in the crumb.
	f.view.commits = api.CommitPage{SHA: sha, Commits: commitsFixture(sha)}
	body := f.mustGet("/demo/walhub/commits", http.StatusOK)
	if !strings.Contains(body, "aaaaaaaaaa") {
		t.Error("ref-less commits page did not fall back to the short sha")
	}
	// ?all=1 with <=5 segments still offers "newest 5".
	f = newRepoFixture(t, true)
	f.view.overview = api.OverviewData{
		Hostname: "node-1",
		Health:   api.Health{Status: "ok"},
		Manifest: api.ManifestInfo{Segments: []string{"seg-1"}},
	}
	body = f.mustGet("/demo/walhub/wal?all=1", http.StatusOK)
	if !strings.Contains(body, "newest 5") {
		t.Error("all-view toggle missing on a short segment list")
	}
}

func TestRepoRouteDataFailures(t *testing.T) {
	for _, path := range []string{
		"/demo/walhub/blob/main/a.txt",
		"/demo/walhub/commits",
		"/demo/walhub/commit/" + strings.Repeat("a", 40),
		"/demo/walhub/wal",
	} {
		f := newRepoFixture(t, true)
		f.reg.err = errors.New("store down")
		if code, _ := f.get(path); code != http.StatusServiceUnavailable {
			t.Errorf("%s with registry failure = %d, want 503", path, code)
		}
	}
	// Blob resolve answering the unborn marker → guide.
	f := newRepoFixture(t, true)
	f.view.resolveErr = fmt.Errorf("%w: %sHEAD", api.ErrNotFound, emptyRepoMarker)
	body := f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("blob unborn resolve did not render the guide")
	}
}

func TestRepoCommitsGraphPagerAndVisibility(t *testing.T) {
	f := newRepoFixture(t, true)
	sha := strings.Repeat("a", 40)
	f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: commitsFixture(sha), More: true}
	body := f.mustGet("/demo/walhub/commits?graph=1", http.StatusOK)
	if !strings.Contains(olderHrefOf(t, body), "graph=1") {
		t.Error("older link dropped the graph flag")
	}

	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	for vis, want := range map[string]string{
		"public":        "public — anyone may read",
		"authenticated": "private — signed-in users may read",
		"":              "",
	} {
		v := vis
		repoDeps = RepoDeps{Visibility: func(owner, repo string) (string, bool) { return v, true }}
		f = newRepoFixture(t, true)
		body = f.mustGet("/demo/walhub", http.StatusOK)
		if want == "" {
			if strings.Contains(body, "visibility-badge") {
				t.Error("unknown visibility rendered a badge")
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("visibility %q badge title missing", vis)
		}
	}
}

func TestRepoCommitSHAFallbackAndBinaryStatus(t *testing.T) {
	f := newRepoFixture(t, true)
	requested := strings.Repeat("a", 40)
	f.view.detail = api.CommitDetail{
		Commit: api.Commit{SHA: "", Subject: "no sha"},
		Patch:  "diff --git a/x.bin b/x.bin\nindex 1..2 100644\nBinary files a/x.bin and b/x.bin differ\n",
	}
	body := f.mustGet("/demo/walhub/commit/"+requested, http.StatusOK)
	if !strings.Contains(body, `title="`+requested+`"`) {
		t.Error("commit page did not fall back to the resolved sha")
	}
	if !strings.Contains(body, "(binary)") {
		t.Error("modified binary file did not carry the binary flag")
	}
}

func TestRepoWALRemainingRows(t *testing.T) {
	f := newRepoFixture(t, true)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ov := api.OverviewData{
		Hostname: "node-1",
		Health:   api.Health{Status: "ok"},
		Manifest: api.ManifestInfo{LastPush: &at, Segments: []string{"s1"}},
		Bundles: []api.BundleInfo{
			{SHA: strings.Repeat("1", 40), Kind: "full", Strategy: "s"},
			{SHA: strings.Repeat("2", 40), Kind: "incremental", BaseID: strings.Repeat("1", 40), Strategy: "s"},
		},
		BundlePlan: api.BundlePlanInfo{Slots: []api.PlanSlot{{Status: "skipped"}, {Status: "built"}}},
	}
	f.view.overview = ov
	body := f.mustGet("/demo/walhub/wal", http.StatusOK)
	if !strings.Contains(body, "last push") || !strings.Contains(body, "2026-01-02") {
		t.Error("last-push stamp missing")
	}
	if !strings.Contains(body, "skipped 1") {
		t.Error("skipped slot not counted")
	}
	if !strings.Contains(body, "token —") {
		t.Error("bundle without a creation token did not render the dash")
	}
}

func TestRepoModeFallbackBits(t *testing.T) {
	if got := fmtMode("000", "blob"); got != ".---------" {
		t.Errorf("fmtMode(000) = %q", got)
	}
}

func TestRepoRemainingBranches(t *testing.T) {
	// Blob with no Name/Path in the payload: the handler derives both from the
	// resolved path.
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Ref: "refs/heads/main", Size: 1, Contents: []byte("x")}
	body := f.mustGet("/demo/walhub/blob/main/deep/a.txt", http.StatusOK)
	if !strings.Contains(body, ">a.txt</h2>") {
		t.Error("blob display name not derived from the resolved path")
	}
	// A blob read answering the unborn marker → guide.
	f = newRepoFixture(t, true)
	f.view.blobErr = fmt.Errorf("%w: %sobject", api.ErrNotFound, emptyRepoMarker)
	body = f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "This repository is empty") {
		t.Error("blob unborn marker did not render the guide")
	}
	// Empty rest (trailing slash) is a 404, not a directory listing.
	if code, _ := f.get("/demo/walhub/blob/"); code != http.StatusNotFound {
		t.Errorf("empty blob rest = %d, want 404", code)
	}
	// Ref-less tree falls back to the sha for entry links.
	f = newRepoFixture(t, true)
	f.view.tree = api.TreeResult{Ref: "", Entries: []api.TreeEntry{{Name: "a.go", Type: "blob", Mode: "100644", Size: 1}}}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "/demo/walhub/blob/"+strings.Repeat("a", 40)+"/a.go") {
		t.Error("ref-less tree did not link by sha")
	}
	// Empty guide with an advertised SSH URL renders both clone commands.
	f = newRepoFixture(t, true)
	f.s.env.Cfg.Server.SSH.Listen = "0.0.0.0:2222"
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body = f.mustGet("/demo/walhub", http.StatusOK)
	if !strings.Contains(body, "git clone ssh://git@example.test:2222/demo/walhub.git") {
		t.Error("SSH clone command missing from the empty guide")
	}
	// A past next-sync stamp without the due flag still reads "sync due".
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	if got := nextSyncText(api.MirrorView{NextSyncAt: now.Add(-time.Minute).Format(time.RFC3339)}, now); got != "sync due" {
		t.Errorf("past next sync = %q", got)
	}
	// Large sizes round to whole units (>= 100).
	if got := fmtSize(200 * 1024); got != "200k" {
		t.Errorf("fmtSize(200KiB) = %q", got)
	}
	// RepoChromeFor propagates a data failure (503), not a silent chrome.
	f = newRepoFixture(t, true)
	f.view.syncErr = errors.New("engine down")
	req := httptest.NewRequest(http.MethodGet, "/demo/walhub", nil)
	rec := httptest.NewRecorder()
	if _, ok := f.s.RepoChromeFor(rec, req, "demo", "walhub"); ok {
		t.Error("RepoChromeFor answered despite a sync failure")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("chrome data failure = %d, want 503", rec.Code)
	}
}

func TestReservedTwoSegmentPathsDecline(t *testing.T) {
	// The repo group's two-segment pattern must not swallow paths that are not
	// repositories (their groups register literal patterns today; this guard
	// keeps the SPA fallback alive for any that has not landed yet).
	f := newRepoFixture(t, true)
	for _, path := range []string{"/orgs/new", "/demo/repositories", "/demo/organizations", "/services/setup.json"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if !f.s.reservedNonRepoPath(req) {
			t.Errorf("%s was not declined by the repo group", path)
		}
	}
	for _, path := range []string{"/demo/walhub", "/demo/walhub/tree/main", "/demo/repositories.git"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if f.s.reservedNonRepoPath(req) {
			t.Errorf("%s was wrongly declined", path)
		}
	}
	// repoGate itself declines without writing (the caller falls back).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orgs/new", nil)
	if _, ok := f.s.repoGate(rec, req); ok {
		t.Error("repoGate claimed /orgs/new")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("declined path wrote %d bytes", rec.Body.Len())
	}
}

func TestRepoAuthFailurePages(t *testing.T) {
	// An invalid credential is a real 401 page (never a 200 shell), and the
	// status mapping follows authErrorStatus.
	f := newRepoFixture(t, true)
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrUnauthorized, http.StatusUnauthorized},
		{auth.ErrInvalid, http.StatusUnauthorized},
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		f.authErr = &auth.AuthError{Kind: tc.kind, Why: "credential rejected"}
		f.rebind()
		if code, _ := f.get("/demo/walhub"); code != tc.want {
			t.Errorf("auth kind %d = %d, want %d", tc.kind, code, tc.want)
		}
		for _, path := range []string{"/demo/walhub/wal", "/demo/walhub/blob/main/a.txt", "/demo/walhub/commits", "/demo/walhub/commit/" + strings.Repeat("a", 40)} {
			if code, _ := f.get(path); code != tc.want {
				t.Errorf("%s auth kind %d = %d, want %d", path, tc.kind, code, tc.want)
			}
		}
	}
	// The cross-group chrome seam propagates the failure, too.
	req := httptest.NewRequest(http.MethodGet, "/demo/walhub", nil)
	rec := httptest.NewRecorder()
	if _, ok := f.s.RepoChromeFor(rec, req, "demo", "walhub"); ok {
		t.Error("RepoChromeFor answered despite an auth failure")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("chrome auth failure = %d, want 503", rec.Code)
	}
}

func TestRepoBlobPathFallbackAndWALDefaultStatus(t *testing.T) {
	// The Blob payload may omit Path; the resolved path is the fallback (the
	// raw link and the breadcrumb both use it).
	f := newRepoFixture(t, true)
	f.view.blob = api.BlobResult{Ref: "refs/heads/main", Path: "", Name: "a.txt", Size: 1, Contents: []byte("x")}
	body := f.mustGet("/demo/walhub/blob/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "/demo/walhub/api/blob/main/a.txt?raw") {
		t.Error("raw link did not fall back to the resolved path")
	}
	// A view that omits Health.Status renders the "ok" default, never empty.
	f = newRepoFixture(t, true)
	f.view.overview = api.OverviewData{Hostname: "node-1", Health: api.Health{Status: ""}}
	body = f.mustGet("/demo/walhub/wal", http.StatusOK)
	if !strings.Contains(body, ">ok</span>") {
		t.Error("empty health status did not default to ok")
	}
}
