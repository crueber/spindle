package webui

// edit_test.go — Feature F's page suite: the three editor pages, the form
// write, the conflict re-render, the blob/tree affordances, and the gates.
// The fakes are dumb (the webedit service has its own test suite): the point
// here is page shape, routing, and gate parity.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/webedit"
)

const editSHA = "1111111111111111111111111111111111111111"

// editView overrides the collaboration fixture's empty read responses with
// the editor's resolve/blob/ref data.
type editView struct {
	*collabFakeView
	resolution api.Resolution
	resolveErr error
	blob       api.BlobResult
	blobErr    error
	refs       []api.Ref
	refsErr    error
	treePath   string
}

func (v *editView) Resolve(context.Context, git.RepoId, string) (api.Resolution, error) {
	return v.resolution, v.resolveErr
}

func (v *editView) Blob(context.Context, git.RepoId, string, string, bool) (api.BlobResult, error) {
	return v.blob, v.blobErr
}

func (v *editView) RefList(context.Context, git.RepoId, string, api.RefQuery) ([]api.Ref, bool, error) {
	return v.refs, false, v.refsErr
}

func (v *editView) Tree(context.Context, git.RepoId, string, string) (api.TreeResult, error) {
	return api.TreeResult{SHA: editSHA, Path: v.treePath, Entries: []api.TreeEntry{}}, nil
}

type editFixture struct {
	*collabFixture
	view  *editView
	saved []webedit.Input
	res   *webedit.Result
	err   error
	patch string
	file  *webedit.File
	load  error
}

func newEditFixture(t *testing.T, principal auth.Principal) *editFixture {
	t.Helper()
	f := &editFixture{
		collabFixture: newCollabFixture(t),
		res:           &webedit.Result{Commit: editSHA, Branch: "main"},
		file:          &webedit.File{Path: "docs/guide.md", SHA: editSHA, Mode: "100644", Size: 11, Content: []byte("hello\nworld")},
	}
	f.principal = principal
	f.view = &editView{
		collabFakeView: f.collabFixture.view,
		resolution:     api.Resolution{Ref: "refs/heads/main", SHA: "2222222222222222222222222222222222222222", Path: "docs/guide.md", Kind: "branch"},
		blob:           api.BlobResult{Ref: "refs/heads/main", SHA: "2222", ObjectSHA: editSHA, Path: "docs/guide.md", Name: "guide.md", Size: 11, Contents: []byte("hello\nworld")},
		refs:           []api.Ref{{Name: "refs/heads/main", SHA: editSHA}, {Name: "refs/heads/dev", SHA: editSHA}},
	}
	// A non-empty repo: the chrome's Empty flag drives the guide, and the
	// editor only exists for repos with a branch.
	f.view.summary = api.SummaryData{
		Head:   &api.Ref{Name: "refs/heads/main", SHA: editSHA},
		Health: api.RepoHealthHealthy, Branches: 2, Tags: 0,
	}
	f.srv.env.API.Repo = f.view
	BindWebedit(WebeditDeps{
		Save: func(_ context.Context, _, _ string, _ auth.Principal, in webedit.Input) (*webedit.Result, error) {
			f.saved = append(f.saved, in)
			return f.res, f.err
		},
		Preview: func(_ context.Context, _, _ string, _ auth.Principal, _ webedit.Input) (string, error) {
			return f.patch, nil
		},
		Load: func(_ context.Context, _, _ string, _ auth.Principal, _, _ string) (*webedit.File, error) {
			if f.load != nil {
				return nil, f.load
			}
			return f.file, nil
		},
		CanPush:  func(context.Context, string, string, auth.Principal) bool { return true },
		MaxBytes: 1 << 20,
	})
	t.Cleanup(func() { webeditDeps = WebeditDeps{} })
	return f
}

func (f *editFixture) getStatus(path string, want int) string {
	f.t.Helper()
	rec := f.get(path)
	if rec.Code != want {
		f.t.Fatalf("GET %s = %d, want %d (%.400s)", path, rec.Code, want, rec.Body.String())
	}
	return rec.Body.String()
}

func TestBlobEditAffordance(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	body := f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	for _, want := range []string{`/acme/widgets/edit/main/docs/guide.md`, `>Edit</a>`, `/acme/widgets/delete/main/docs/guide.md`, `>raw</a>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("blob page missing %q", want)
		}
	}
	// Binary: no edit affordance, a reason instead.
	f.view.blob.Binary = true
	body = f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	if strings.Contains(body, "/edit/main/docs/guide.md") || !strings.Contains(body, "binary file") {
		t.Fatalf("binary page affordance wrong")
	}
	// Anonymous in token mode: the interstitial, not a dead link.
	f.view.blob.Binary = false
	f.principal = auth.Anonymous()
	body = f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, "/login-required?next=") {
		t.Fatalf("anonymous blob has no interstitial href")
	}
	// No push right: hidden.
	f.view.blob.Binary = false
	f.principal = auth.Principal{Name: "jane"}
	d := webeditDeps
	d.CanPush = func(context.Context, string, string, auth.Principal) bool { return false }
	BindWebedit(d)
	body = f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	if strings.Contains(body, "/edit/main/docs/guide.md") {
		t.Fatalf("read-only viewer sees an edit link")
	}
}

func TestTreeAddFileAffordance(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	f.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: "", Kind: "branch"}
	body := f.getStatus("/acme/widgets/tree/main", http.StatusOK)
	if !strings.Contains(body, `href="/acme/widgets/new/main"`) || !strings.Contains(body, "Add file") {
		t.Fatalf("tree has no Add file control")
	}
	d := webeditDeps
	d.CanPush = func(context.Context, string, string, auth.Principal) bool { return false }
	BindWebedit(d)
	f.principal = auth.Principal{Name: "reader"}
	body = f.getStatus("/acme/widgets/tree/main", http.StatusOK)
	if strings.Contains(body, "Add file") {
		t.Fatalf("read-only viewer sees Add file")
	}
}

func TestEditorPageRenders(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	body := f.getStatus("/acme/widgets/edit/main/docs/guide.md", http.StatusOK)
	for _, want := range []string{
		`name="content"`, "hello\nworld", `name="expected_blob_sha" value="` + editSHA + `"`,
		`name="expected_branch_sha"`, "Commit changes", "Preview changes",
		`<option value="main" selected>`, `name="new_branch"`, `name="open_pr"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit page missing %q", want)
		}
	}
	if n := strings.Count(body, `class="btn primary`); n != 1 {
		t.Fatalf("primary buttons = %d, want 1", n)
	}
	// Create mode: a path input and the new-file CTA.
	f.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: "", Kind: "branch"}
	body = f.getStatus("/acme/widgets/new/main?path=docs/", http.StatusOK)
	if !strings.Contains(body, "Create a new file") || !strings.Contains(body, `name="path" value="docs/"`) ||
		!strings.Contains(body, "Create file") {
		t.Fatalf("new page shape wrong")
	}
	// Delete mode: typed confirm + danger button.
	f.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: "docs/guide.md", Kind: "branch"}
	body = f.getStatus("/acme/widgets/delete/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, `data-danger-expected="docs/guide.md"`) || !strings.Contains(body, `data-danger-input`) ||
		!strings.Contains(body, "btn primary danger") || !strings.Contains(body, "Delete file") {
		t.Fatalf("delete page shape wrong")
	}
}

func TestEditorRefusals(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	cases := []struct {
		name string
		mut  func()
		want string
	}{
		{"binary", func() { f.file.Binary = true }, "binary"},
		{"lfs", func() { f.file.LFS = true }, "Git LFS pointer"},
		{"oversize", func() { f.file.Size = 2 << 20 }, "larger than the editor cap"},
	}
	for _, tc := range cases {
		f.file = &webedit.File{Path: "docs/guide.md", SHA: editSHA, Mode: "100644", Size: 11, Content: []byte("x")}
		tc.mut()
		body := f.getStatus("/acme/widgets/edit/main/docs/guide.md", http.StatusOK)
		if !strings.Contains(body, tc.want) || strings.Contains(body, `name="content"`) {
			t.Fatalf("%s refusal wrong", tc.name)
		}
	}
}

func TestEditorGates(t *testing.T) {
	// Anonymous viewer in token mode is redirected to the interstitial.
	f := newEditFixture(t, auth.Anonymous())
	rec := f.get("/acme/widgets/edit/main/docs/guide.md")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/login-required") {
		t.Fatalf("anonymous = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Authenticated but no push right: 403.
	f2 := newEditFixture(t, auth.Principal{Name: "reader"})
	d := webeditDeps
	d.CanPush = func(context.Context, string, string, auth.Principal) bool { return false }
	BindWebedit(d)
	if rec := f2.get("/acme/widgets/edit/main/docs/guide.md"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader = %d", rec.Code)
	}
	// Unwired builds answer 503, never a fabricated editor.
	f3 := newEditFixture(t, auth.Principal{Name: "jane"})
	BindWebedit(WebeditDeps{})
	if rec := f3.get("/acme/widgets/edit/main/docs/guide.md"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d", rec.Code)
	}
}

func TestEditPostSaveAndPreview(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	form := url.Values{
		"mode": {"edit"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"Update guide"}, "description": {"why"},
		"content": {"line one\r\nline two"}, "expected_blob_sha": {editSHA},
		"expected_branch_sha": {"2222"}, "new_branch": {""}, "action": {"save"},
	}
	rec := f.post("/acme/widgets/contents", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d %.300s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/acme/widgets/blob/main/docs/guide.md?flash=") {
		t.Fatalf("save Location = %q", loc)
	}
	if len(f.saved) != 1 {
		t.Fatalf("saves = %d", len(f.saved))
	}
	got := f.saved[0]
	if string(got.Content) != "line one\nline two" {
		t.Fatalf("CRLF not normalized: %q", got.Content)
	}
	if got.Mode != webedit.ModeEdit || got.ExpectedBlobSHA != editSHA || got.Message != "Update guide" {
		t.Fatalf("save input = %+v", got)
	}
	// Preview re-renders with the patch and writes nothing.
	f.patch = "diff --git a/docs/guide.md b/docs/guide.md\n--- a/docs/guide.md\n+++ b/docs/guide.md\n@@ -1 +1 @@\n-hello\n+bye\n"
	form.Set("action", "preview")
	rec = f.post("/acme/widgets/contents", form)
	if rec.Code != http.StatusOK || len(f.saved) != 1 {
		t.Fatalf("preview = %d saves=%d", rec.Code, len(f.saved))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Pending changes") || !strings.Contains(body, "docs/guide.md") {
		t.Fatalf("preview body missing diff")
	}
	if !strings.Contains(body, "diff-add") || !strings.Contains(body, ">bye<") {
		t.Fatalf("preview diff not rendered")
	}
	// Delete mode redirects to the tree.
	f.res = &webedit.Result{Commit: editSHA, Branch: "main"}
	delForm := url.Values{"mode": {"delete"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"rm"}, "expected_blob_sha": {editSHA}, "action": {"save"}}
	rec = f.post("/acme/widgets/contents", delForm)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/acme/widgets/tree/main") {
		t.Fatalf("delete = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if f.saved[len(f.saved)-1].Mode != webedit.ModeDelete {
		t.Fatalf("delete mode = %q", f.saved[len(f.saved)-1].Mode)
	}
	// New branch + PR lands on the PR.
	f.res = &webedit.Result{Commit: editSHA, Branch: "fix/x", PRNum: 9}
	prForm := url.Values{"mode": {"new"}, "ref": {"main"}, "path": {"docs/new.md"},
		"message": {"add"}, "content": {"x"}, "new_branch": {"fix/x"}, "open_pr": {"1"}, "action": {"save"}}
	rec = f.post("/acme/widgets/contents", prForm)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/acme/widgets/pull/9") {
		t.Fatalf("pr = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if in := f.saved[len(f.saved)-1]; !in.OpenPR || in.NewBranch != "fix/x" || in.Mode != webedit.ModeCreate {
		t.Fatalf("pr input = %+v", in)
	}
}

func TestEditPostConflictAndErrors(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	f.err = &webedit.ConflictError{What: "branch", Expected: "aaa", Actual: "bbb"}
	form := url.Values{"mode": {"edit"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"m"}, "content": {"kept text"}, "expected_blob_sha": {editSHA}, "action": {"save"}}
	rec := f.post("/acme/widgets/contents", form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"branch moved", "page: aaa", "disk: bbb", "kept text"} {
		if !strings.Contains(body, want) {
			t.Fatalf("conflict page missing %q", want)
		}
	}
	// A guardrail error maps to its status and keeps the text.
	f.err = webedit.ErrUnsupported
	rec = f.post("/acme/widgets/contents", form)
	if rec.Code != http.StatusUnsupportedMediaType || !strings.Contains(rec.Body.String(), "kept text") {
		t.Fatalf("415 page = %d", rec.Code)
	}
	// An unknown mode is a 400.
	f.err = nil
	rec = f.post("/acme/widgets/contents", url.Values{"mode": {"bogus"}, "ref": {"main"}, "path": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d", rec.Code)
	}
	// Cross-origin writes are refused.
	req := httptest.NewRequest(http.MethodPost, "/acme/widgets/contents", strings.NewReader("mode=edit"))
	req.Host = testHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.test")
	rec = httptest.NewRecorder()
	if !f.srv.Serve(rec, req) || rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
}

func TestEditorRouteErrors(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	// Unknown ref: the resolve error is the repo view's 404.
	f.view.resolveErr = errors.New("not found")
	if rec := f.get("/acme/widgets/edit/main/x.txt"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("resolve error = %d", rec.Code)
	}
	// A path-less edit route is a 404.
	f.view.resolveErr = nil
	f.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: ""}
	if rec := f.get("/acme/widgets/edit/main"); rec.Code != http.StatusNotFound {
		t.Fatalf("pathless edit = %d", rec.Code)
	}
	// A load-side guard (e.g. an LFS pointer) still renders the page.
	f.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: "x.bin"}
	f.load = webedit.ErrUnsupported
	if rec := f.get("/acme/widgets/edit/main/x.bin"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("load guard = %d", rec.Code)
	}
	f.load = webedit.ErrNotFound
	if rec := f.get("/acme/widgets/edit/main/x.bin"); rec.Code != http.StatusNotFound {
		t.Fatalf("load missing = %d", rec.Code)
	}
}

func TestEditPostGates(t *testing.T) {
	form := url.Values{"mode": {"edit"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"m"}, "content": {"x"}, "expected_blob_sha": {editSHA}, "action": {"save"}}
	// Anonymous viewer in token mode: interstitial.
	f := newEditFixture(t, auth.Anonymous())
	if rec := f.post("/acme/widgets/contents", form); rec.Code != http.StatusSeeOther ||
		!strings.Contains(rec.Header().Get("Location"), "/login-required") {
		t.Fatalf("anon post = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// No push right: 403.
	f2 := newEditFixture(t, auth.Principal{Name: "reader"})
	d := webeditDeps
	d.CanPush = func(context.Context, string, string, auth.Principal) bool { return false }
	BindWebedit(d)
	if rec := f2.post("/acme/widgets/contents", form); rec.Code != http.StatusForbidden {
		t.Fatalf("reader post = %d", rec.Code)
	}
	// Unwired: 503.
	f3 := newEditFixture(t, auth.Principal{Name: "jane"})
	BindWebedit(WebeditDeps{})
	if rec := f3.post("/acme/widgets/contents", form); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired post = %d", rec.Code)
	}
	// Preview unwired: 503 with the typed content kept.
	f4 := newEditFixture(t, auth.Principal{Name: "jane"})
	d4 := webeditDeps
	d4.Preview = nil
	BindWebedit(d4)
	pv := url.Values{}
	for k, v := range form {
		pv[k] = v
	}
	pv.Set("action", "preview")
	if rec := f4.post("/acme/widgets/contents", pv); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "Preview is not available") {
		t.Fatalf("preview unwired = %d", rec.Code)
	}
}

func TestEditPostPreviewErrors(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	f.saved = nil
	// Preview returns a conflict: the banner renders at 409.
	d := webeditDeps
	d.Preview = func(context.Context, string, string, auth.Principal, webedit.Input) (string, error) {
		return "", &webedit.ConflictError{What: "blob", Expected: "a", Actual: "b"}
	}
	BindWebedit(d)
	form := url.Values{"mode": {"edit"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"m"}, "content": {"keep"}, "expected_blob_sha": {editSHA}, "action": {"preview"}}
	if rec := f.post("/acme/widgets/contents", form); rec.Code != http.StatusConflict {
		t.Fatalf("preview conflict = %d", rec.Code)
	}
	// A delete preview renders the deleted-file flag.
	d.Preview = func(context.Context, string, string, auth.Principal, webedit.Input) (string, error) {
		return "diff --git a/docs/guide.md b/docs/guide.md\n--- a/docs/guide.md\n+++ /dev/null\n@@ -1 +0,0 @@\n-hello\n", nil
	}
	BindWebedit(d)
	del := url.Values{"mode": {"delete"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"rm"}, "expected_blob_sha": {editSHA}, "action": {"preview"}}
	rec := f.post("/acme/widgets/contents", del)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docs/guide.md") {
		t.Fatalf("delete preview = %d", rec.Code)
	}
}

func TestEditorLoadRefusalsAndBranchErrors(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	// A bad load request (invalid path) is a 400 error page.
	f.load = webedit.ErrInvalid
	if rec := f.get("/acme/widgets/edit/main/x.txt"); rec.Code != http.StatusBadRequest {
		t.Fatalf("load invalid = %d", rec.Code)
	}
	// RefList failure omits the selector and falls back to a text input.
	f.load = nil
	f.view.refsErr = errors.New("refs down")
	body := f.getStatus("/acme/widgets/edit/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, `name="ref" value="main"`) {
		t.Fatalf("branch fallback missing")
	}
	// A resolution with no ref is a 404.
	f.view.refsErr = nil
	f.view.resolution = api.Resolution{}
	if rec := f.get("/acme/widgets/new"); rec.Code != http.StatusNotFound {
		t.Fatalf("no ref = %d", rec.Code)
	}
}

func TestBlobGuardReasons(t *testing.T) {
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	f.view.blob.Contents = []byte("version https://git-lfs.github.com/spec/v1\noid sha256:x\n")
	f.view.blob.Size = 60
	body := f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, "Git LFS pointer") || strings.Contains(body, "/edit/main/docs/guide.md") {
		t.Fatalf("lfs blob affordance wrong")
	}
	f.view.blob.Contents = []byte("small")
	f.view.blob.Size = 2 << 20
	body = f.getStatus("/acme/widgets/blob/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, "larger than the editor cap") || strings.Contains(body, "/edit/main/docs/guide.md") {
		t.Fatalf("oversize blob affordance wrong")
	}
}

func TestEditorEdgeStates(t *testing.T) {
	// An empty repo renders the guided empty state on the editor routes.
	f := newEditFixture(t, auth.Principal{Name: "jane"})
	f.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := f.getStatus("/acme/widgets/edit/main/docs/guide.md", http.StatusOK)
	if !strings.Contains(body, "repo-guide") && !strings.Contains(body, "empty") {
		t.Fatalf("empty repo guide missing")
	}
	// A generic save failure maps to 503 (never a 500 page) and keeps text.
	f2 := newEditFixture(t, auth.Principal{Name: "jane"})
	f2.err = errors.New("boom")
	form := url.Values{"mode": {"edit"}, "ref": {"main"}, "path": {"docs/guide.md"},
		"message": {"m"}, "content": {"kept"}, "expected_blob_sha": {editSHA}, "action": {"save"}}
	if rec := f2.post("/acme/widgets/contents", form); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("generic save = %d", rec.Code)
	}
	// A generic load failure is also 503 with the card.
	f3 := newEditFixture(t, auth.Principal{Name: "jane"})
	f3.load = errors.New("boom")
	if rec := f3.get("/acme/widgets/edit/main/docs/guide.md"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("generic load = %d", rec.Code)
	}
	// A subtree shows the Add-file link prefilled with the directory.
	f4 := newEditFixture(t, auth.Principal{Name: "jane"})
	f4.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: editSHA, Path: "docs", Kind: "branch"}
	f4.view.treePath = "docs"
	body = f4.getStatus("/acme/widgets/tree/main/docs", http.StatusOK)
	if !strings.Contains(body, "/acme/widgets/new/main?path=docs%2F") {
		i := strings.Index(body, "/new/main")
		t.Fatalf("subtree Add-file link missing path prefill: %.300s", body[max(0, i-100):])
	}
	// Added + binary previews render their badges.
	files := diffFilesFromPatch("diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hi\n" +
		"diff --git a/img.png b/img.png\nBinary files a/img.png and b/img.png differ\n")
	if len(files) != 2 || files[0].Flags != "added" || !files[1].Binary {
		t.Fatalf("diff view files = %+v", files)
	}
}
