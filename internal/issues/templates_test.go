package issues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// templates_test.go — §15: front-matter parsing, discovery, the cached
// projection + its no-op refresh, malformed-file skipping, and the guarantee
// that the read path never touches git (TemplateGit nil is enough to read).

type fakeTemplateGit struct {
	files  map[string]string
	commit string
	listed int
	read   int
	err    error
}

func (f *fakeTemplateGit) ListTemplateFiles(context.Context, string, string) ([]string, error) {
	f.listed++
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for p := range f.files {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeTemplateGit) ReadFile(_ context.Context, _, commit, p string) ([]byte, error) {
	f.read++
	if f.commit != "" && commit != f.commit {
		return nil, errors.New("wrong commit")
	}
	body, ok := f.files[p]
	if !ok {
		return nil, ErrNotFound
	}
	return []byte(body), nil
}

func (f *fakeTemplateGit) ResolveRef(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.commit, nil
}

func TestParseIssueTemplate(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		raw     string
		want    IssueTemplate
		wantErr error
	}{
		{
			name: "front matter",
			path: ".spindle/ISSUE_TEMPLATE/bug.md",
			raw:  "---\nname: Bug report\nabout: Something is broken\ntitle: \"[bug] \"\nlabels: bug, regression\nassignees: alice\n---\n\nSteps to reproduce\n",
			want: IssueTemplate{
				Path: ".spindle/ISSUE_TEMPLATE/bug.md", Name: "Bug report", About: "Something is broken",
				Title: "[bug] ", Labels: []string{"bug", "regression"}, Assignees: []string{"alice"},
				Body: "Steps to reproduce\n",
			},
		},
		{
			name: "bracketed lists and no body",
			path: ".spindle/ISSUE_TEMPLATE/feat.md",
			raw:  "---\nname: Feature\nlabels: [feature, ui]\nassignees: [bob]\n---\n",
			want: IssueTemplate{
				Path: ".spindle/ISSUE_TEMPLATE/feat.md", Name: "Feature",
				Labels: []string{"feature", "ui"}, Assignees: []string{"bob"}, Body: "",
			},
		},
		{
			name: "no front matter uses the stem",
			path: ".spindle/ISSUE_TEMPLATE/question.md",
			raw:  "What happened?\n",
			want: IssueTemplate{Path: ".spindle/ISSUE_TEMPLATE/question.md", Name: "question", Body: "What happened?\n"},
		},
		{
			name:    "unterminated front matter is refused",
			path:    ".spindle/ISSUE_TEMPLATE/broken.md",
			raw:     "---\nname: Broken\nbody without a close",
			wantErr: ErrInvalid,
		},
		{
			name:    "bad front matter line is refused",
			path:    ".spindle/ISSUE_TEMPLATE/bad.md",
			raw:     "---\nthis is not an assignment\n---\nbody\n",
			wantErr: ErrInvalid,
		},
		{
			name:    "oversized body is refused",
			path:    ".spindle/ISSUE_TEMPLATE/huge.md",
			raw:     "---\nname: Huge\n---\n" + strings.Repeat("x", MaxBodyBytes+1),
			wantErr: ErrInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseIssueTemplate(c.path, []byte(c.raw))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Path != c.want.Path || got.Name != c.want.Name || got.About != c.want.About ||
				got.Title != c.want.Title || got.Body != c.want.Body {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			if strings.Join(got.Labels, ",") != strings.Join(c.want.Labels, ",") {
				t.Fatalf("labels = %v, want %v", got.Labels, c.want.Labels)
			}
			if strings.Join(got.Assignees, ",") != strings.Join(c.want.Assignees, ",") {
				t.Fatalf("assignees = %v, want %v", got.Assignees, c.want.Assignees)
			}
		})
	}
}

func TestRefreshTemplatesDiscoveryAndCache(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	git := &fakeTemplateGit{commit: strings.Repeat("a", 40), files: map[string]string{
		".spindle/ISSUE_TEMPLATE/bug.md":    "---\nname: Bug\nlabels: bug\n---\nRepro steps\n",
		".spindle/ISSUE_TEMPLATE/broken.md": "---\nname: Broken\n",
		".spindle/ISSUE_TEMPLATE/feat.md":   "---\nname: Feature\nabout: New thing\n---\nWhy?\n",
	}}
	s.TemplateGit = git

	changed, err := s.RefreshTemplates(reqCtx(), "acme", "repo", git.commit)
	if err != nil || !changed {
		t.Fatalf("refresh = %v, %v", changed, err)
	}
	doc, err := s.Templates(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Commit != git.commit || doc.RefreshedAt == "" || doc.Version != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	// The malformed file is skipped; the other two are offered sorted by path.
	if len(doc.Templates) != 2 || doc.Templates[0].Name != "Bug" || doc.Templates[1].Name != "Feature" {
		t.Fatalf("templates = %+v", doc.Templates)
	}
	if doc.Templates[1].About != "New thing" || doc.Templates[0].Body != "Repro steps\n" {
		t.Fatalf("templates = %+v", doc.Templates)
	}

	// Same commit, same content: no write (changed=false, version unchanged).
	changed, err = s.RefreshTemplates(reqCtx(), "acme", "repo", git.commit)
	if err != nil || changed {
		t.Fatalf("no-op refresh = %v, %v", changed, err)
	}
	doc2, _ := s.Templates(reqCtx(), "acme", "repo")
	if doc2.Version != 1 {
		t.Fatalf("no-op refresh bumped version to %d", doc2.Version)
	}

	// A new commit with a changed file rewrites the projection.
	git.commit = strings.Repeat("b", 40)
	git.files[".spindle/ISSUE_TEMPLATE/feat.md"] = "---\nname: Feature request\n---\nWhat and why?\n"
	changed, err = s.RefreshTemplates(reqCtx(), "acme", "repo", git.commit)
	if err != nil || !changed {
		t.Fatalf("changed refresh = %v, %v", changed, err)
	}
	doc3, _ := s.Templates(reqCtx(), "acme", "repo")
	if doc3.Commit != git.commit || doc3.Templates[1].Name != "Feature request" || doc3.Version != 2 {
		t.Fatalf("doc3 = %+v", doc3)
	}

	// A discovery failure surfaces (the caller logs; the old projection stays).
	git.err = errors.New("git down")
	if _, err := s.RefreshTemplates(reqCtx(), "acme", "repo", git.commit); err == nil {
		t.Fatal("refresh error not surfaced")
	}
	doc4, err := s.Templates(reqCtx(), "acme", "repo")
	if err != nil || doc4.Version != 2 {
		t.Fatalf("old projection must survive a failed refresh: %+v, %v", doc4, err)
	}
}

func TestTemplatesReadNeedsNoGit(t *testing.T) {
	s := testService(newFakeRoles())
	// No TemplateGit at all: the read path still serves the stored projection.
	doc, err := s.Templates(reqCtx(), "acme", "repo")
	if err != nil || doc == nil || len(doc.Templates) != 0 {
		t.Fatalf("empty read = %+v, %v", doc, err)
	}
	// Seed a projection directly and confirm the read returns it.
	raw, _ := json.Marshal(&TemplateDoc{Version: 3, Commit: strings.Repeat("c", 40),
		Templates: []IssueTemplate{{Path: ".spindle/ISSUE_TEMPLATE/bug.md", Name: "Bug", Body: "x\n"}}})
	if err := s.putCreate(reqCtx(), TemplateKey("acme", "repo"), raw); err != nil {
		t.Fatal(err)
	}
	doc, err = s.Templates(reqCtx(), "acme", "repo")
	if err != nil || len(doc.Templates) != 1 || doc.Templates[0].Name != "Bug" || doc.Version != 3 {
		t.Fatalf("seeded read = %+v, %v", doc, err)
	}
	if _, err := s.RefreshTemplates(reqCtx(), "acme", "repo", strings.Repeat("d", 40)); err != nil {
		t.Fatalf("nil TemplateGit refresh must be a no-op, got %v", err)
	}
}

func TestRefreshTemplatesFromDefaultBranch(t *testing.T) {
	s := testService(newFakeRoles())
	git := &fakeTemplateGit{commit: strings.Repeat("e", 40), files: map[string]string{
		".spindle/ISSUE_TEMPLATE/bug.md": "---\nname: Bug\n---\n",
	}}
	s.TemplateGit = git
	changed, err := s.RefreshTemplatesFromDefaultBranch(reqCtx(), "acme/repo")
	if err != nil || !changed {
		t.Fatalf("hook refresh = %v, %v", changed, err)
	}
	if _, err := s.RefreshTemplatesFromDefaultBranch(reqCtx(), "bad-id"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo id err = %v", err)
	}
	// Unborn HEAD: the reader reports not-found, the hook is a quiet no-op.
	s.TemplateGit = &fakeTemplateGit{err: wrapNotFound()}
	if changed, err := s.RefreshTemplatesFromDefaultBranch(reqCtx(), "acme/repo"); err != nil || changed {
		t.Fatalf("unborn HEAD = %v, %v", changed, err)
	}
}

func wrapNotFound() error { return errors.Join(ErrNotFound, errors.New("unknown ref")) }

func TestIssueTemplatesHTTP(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	raw, _ := json.Marshal(&TemplateDoc{Version: 1, Commit: strings.Repeat("f", 40),
		Templates: []IssueTemplate{{Path: ".spindle/ISSUE_TEMPLATE/bug.md", Name: "Bug", Body: "x\n"}}})
	if err := s.putCreate(reqCtx(), TemplateKey("acme", "repo"), raw); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, bobP)
	rec := doReq(h, http.MethodGet, "/acme/repo/api/issue-templates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Templates []IssueTemplate `json:"templates"`
		Commit    string          `json:"commit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Templates) != 1 || body.Templates[0].Name != "Bug" || body.Commit == "" {
		t.Fatalf("body = %+v", body)
	}
	if rec := doReq(h, http.MethodPost, "/acme/repo/api/issue-templates", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	// Corrupt projection is a 503-class store failure, never a silent empty.
	bad := []byte("{not json")
	if _, err := store.PutBytes(reqCtx(), s.Store, TemplateKey("acme", "repo"), bad, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(h, http.MethodGet, "/acme/repo/api/issue-templates", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt GET = %d", rec.Code)
	}
	// Read gate applies.
	roles.private["acme/repo"] = true
	if rec := doReq(testHandler(s, anonP), http.MethodGet, "/acme/repo/api/issue-templates", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon private GET = %d", rec.Code)
	}
}
