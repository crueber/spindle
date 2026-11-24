package webedit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

const testSHA = "1111111111111111111111111111111111111111"

func ctx() context.Context { return context.Background() }

func reader() auth.Principal { return auth.Principal{Name: "jane"} }
func writer() auth.Principal { return auth.Principal{Name: "jane", Write: true} }
func admin() auth.Principal  { return auth.Principal{Name: "root", Admin: true} }

// --- fakes ------------------------------------------------------------------

type fakeGate struct {
	pushErr *auth.AuthError
	readErr *auth.AuthError
}

func (g *fakeGate) CheckPush(context.Context, string, string, auth.Principal) *auth.AuthError {
	return g.pushErr
}
func (g *fakeGate) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return g.readErr
}

type fakeDirs struct{ dir string }

func (d *fakeDirs) Dir(context.Context, string) (string, error) {
	if d.dir == "" {
		return "", errors.New("no dir")
	}
	return d.dir, nil
}

type fakeGit struct {
	branches  map[string]string
	files     map[string]*File
	attrs     []byte
	noChange  bool
	buildErr  error
	lookupErr error
	packErr   error
	hashErr   error
	builds    []CommitBuild
	diff      string
	pack      []byte
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		branches: map[string]string{"main": testSHA},
		files: map[string]*File{
			"hello.txt": {Path: "hello.txt", SHA: testSHA, Mode: "100644", Size: 6, Content: []byte("hello\n")},
		},
		pack: []byte("PACKDATA"),
		diff: "diff --git a/hello.txt b/hello.txt\n--- a/hello.txt\n+++ b/hello.txt\n@@ -1 +1 @@\n-hello\n+bye\n",
	}
}

func (g *fakeGit) ResolveBranch(_ context.Context, _, branch string) (string, error) {
	sha, ok := g.branches[branch]
	if !ok {
		return "", fmt.Errorf("%w: unknown branch %q", ErrNotFound, branch)
	}
	return sha, nil
}

func (g *fakeGit) LookupFile(_ context.Context, _, _, p string) (*File, error) {
	if g.lookupErr != nil {
		return nil, g.lookupErr
	}
	f, ok := g.files[p]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	cp := *f
	return &cp, nil
}

func (g *fakeGit) RootFile(context.Context, string, string, string) ([]byte, bool, error) {
	if g.attrs == nil {
		return nil, false, nil
	}
	return g.attrs, true, nil
}

func (g *fakeGit) HashBlob(_ context.Context, _ string, content []byte) (string, error) {
	if g.hashErr != nil {
		return "", g.hashErr
	}
	return fmt.Sprintf("%040x", len(content)+7), nil
}

func (g *fakeGit) BuildCommit(_ context.Context, _ string, in CommitBuild) (string, string, error) {
	g.builds = append(g.builds, in)
	if g.buildErr != nil {
		return "", "", g.buildErr
	}
	if g.noChange {
		return "", "", fmt.Errorf("%w: file content is unchanged", ErrNoChange)
	}
	return "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333", nil
}

func (g *fakeGit) DiffTree(context.Context, string, string, string) (string, error) {
	return g.diff, nil
}

func (g *fakeGit) PackTip(context.Context, string, string) ([]byte, error) {
	if g.packErr != nil {
		return nil, g.packErr
	}
	return g.pack, nil
}

type publishCall struct {
	repo, ref, old, new string
	pack                []byte
	meta                map[string]string
}

type fakeRefs struct {
	calls []publishCall
	err   error
}

func (r *fakeRefs) UpdateRefWithPack(_ context.Context, repo, ref, old, newSHA string, pack []byte, meta map[string]string) error {
	r.calls = append(r.calls, publishCall{repo: repo, ref: ref, old: old, new: newSHA, pack: pack, meta: meta})
	return r.err
}

type fakePR struct {
	calls []OpenInput
	num   int
	err   error
}

func (p *fakePR) OpenPR(_ context.Context, _, _ string, in OpenInput, _ auth.Principal) (int, error) {
	p.calls = append(p.calls, in)
	return p.num, p.err
}

type harness struct {
	svc  *Service
	git  *fakeGit
	refs *fakeRefs
	pr   *fakePR
	gate *fakeGate
	st   store.ObjectStore
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	h := &harness{
		git:  newFakeGit(),
		refs: &fakeRefs{},
		pr:   &fakePR{num: 7},
		gate: &fakeGate{},
		st:   st,
	}
	h.svc = New(st, h.gate)
	h.svc.Git = h.git
	h.svc.Dirs = &fakeDirs{dir: "/repo"}
	h.svc.Refs = h.refs
	h.svc.PR = h.pr
	h.svc.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return h
}

func (h *harness) policy(t *testing.T, body string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), h.st, PolicyKey("o", "r"), []byte(body), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

// --- validation tables ------------------------------------------------------

func TestValidatePath(t *testing.T) {
	ok := []string{"a", "a/b.txt", "dir/file name.md", ".github/workflows/ci.yml", "a.b/c-d_e"}
	for _, p := range ok {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v", p, err)
		}
	}
	bad := map[string]string{
		"": "empty", "/a": "leading /", "a/": "trailing /", "a//b": "empty segment",
		"a/../b": "dotdot", "./a": "dot", ".git/config": "git segment", "a/.GIT/x": "git segment ci",
		"-flag": "leading dash", "a\\b": "backslash", "a\x00b": "nul", "a\x1fb": "control",
		strings.Repeat("x", 513): "length", string([]byte{0xff, 'a'}): "utf8",
	}
	for p, why := range bad {
		if err := ValidatePath(p); !isErr(err, ErrInvalid) {
			t.Errorf("ValidatePath(%q) [%s] = %v, want ErrInvalid", p, why, err)
		}
	}
}

func TestValidateMessage(t *testing.T) {
	msg, desc, err := ValidateMessage("  Update docs  ", " body ")
	if err != nil || msg != "Update docs" || desc != "body" {
		t.Fatalf("trim = %q/%q/%v", msg, desc, err)
	}
	if _, _, err := ValidateMessage("   ", ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty = %v", err)
	}
	if _, _, err := ValidateMessage(strings.Repeat("x", MaxMessageBytes), ""); !isErr(err, ErrInvalid) {
		t.Fatalf("oversize = %v", err)
	}
}

func TestValidateBranch(t *testing.T) {
	for _, b := range []string{"main", "feature/x", "refs/heads/main", "a.b-c_d"} {
		if _, err := ValidateBranch(b); err != nil {
			t.Errorf("ValidateBranch(%q) = %v", b, err)
		}
	}
	for _, b := range []string{"", "HEAD", "-x", "a..b", "a b", "a~b", "a.lock"} {
		if _, err := ValidateBranch(b); !isErr(err, ErrInvalid) {
			t.Errorf("ValidateBranch(%q) = %v, want ErrInvalid", b, err)
		}
	}
}

func TestGitattributesSubset(t *testing.T) {
	body := []byte("# comment\n*.png filter=lfs diff=lfs merge=lfs -text\nassets/** filter=lfs\n!keep.bin filter=lfs\nexact.txt filter=lfs\n")
	cases := map[string]bool{
		"a.png": true, "assets/x/y.bin": true, "assets": true, "keep.bin": false, "exact.txt": true, "other.txt": false,
	}
	for p, want := range cases {
		if got := matchGitattributes(body, p); got != want {
			t.Errorf("matchGitattributes(%q) = %v, want %v", p, got, want)
		}
	}
	if gitattributesLFS([]byte("# nothing\n"), "a") {
		t.Fatal("comment matched")
	}
	if !gitattributesLFS([]byte("** filter=lfs\n"), "deep/a") {
		t.Fatal("global pattern missed")
	}
}

// --- gate -------------------------------------------------------------------

func TestGate(t *testing.T) {
	h := newHarness(t)
	in := Input{Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "edit", Content: []byte("x"), ExpectedBlobSHA: testSHA}

	if _, err := h.svc.Save(ctx(), "o", "r", auth.Anonymous(), in); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	h.gate.pushErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	if _, err := h.svc.Save(ctx(), "o", "r", reader(), in); !isErr(err, ErrForbidden) {
		t.Fatalf("push denied = %v", err)
	}
	// A host-wide write flag cannot override the push gate.
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); !isErr(err, ErrForbidden) {
		t.Fatalf("host write vs push gate = %v", err)
	}
	h.gate.pushErr = nil
	h.gate.readErr = &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "login"}
	if _, err := h.svc.Save(ctx(), "o", "r", reader(), in); !isErr(err, ErrUnauthorized) {
		t.Fatalf("read denied = %v", err)
	}
	h.gate.readErr = &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}
	if _, err := h.svc.Save(ctx(), "o", "r", reader(), in); !isErr(err, ErrInvalid) {
		t.Fatalf("invalid credential = %v", err)
	}
	h.gate.readErr = &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	if _, err := h.svc.Save(ctx(), "o", "r", reader(), in); !isErr(err, ErrUnavailable) {
		t.Fatalf("unavailable = %v", err)
	}
}

func TestGateNilRolesFallsBackToFlags(t *testing.T) {
	h := newHarness(t)
	h.svc.Gate = nil
	in := Input{Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "edit", Content: []byte("x"), ExpectedBlobSHA: testSHA}
	if _, err := h.svc.Save(ctx(), "o", "r", auth.Anonymous(), in); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon = %v", err)
	}
	if _, err := h.svc.Save(ctx(), "o", "r", reader(), in); !isErr(err, ErrForbidden) {
		t.Fatalf("read = %v", err)
	}
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); err != nil {
		t.Fatalf("write = %v", err)
	}
}

// --- create / edit / delete -------------------------------------------------

func TestCreateHappy(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeCreate, Ref: "main", Path: "docs/new.md", Message: "add", Description: "why",
		Content: []byte("text\n"), ExpectedBranchSHA: testSHA,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Commit != "2222222222222222222222222222222222222222" || res.Branch != "main" || res.PRNum != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(h.refs.calls) != 1 || h.refs.calls[0].old != testSHA || h.refs.calls[0].ref != "refs/heads/main" {
		t.Fatalf("publish = %+v", h.refs.calls)
	}
	if string(h.git.builds[0].BlobSHA) == "" || h.git.builds[0].Message != "add\n\nwhy" {
		t.Fatalf("build = %+v", h.git.builds[0])
	}
	if h.git.builds[0].AuthorName != "jane" || h.git.builds[0].AuthorMail != "jane@spindle.local" {
		t.Fatalf("author = %s <%s>", h.git.builds[0].AuthorName, h.git.builds[0].AuthorMail)
	}
	if h.refs.calls[0].meta["principal"] != "jane" || h.refs.calls[0].meta["agent"] != "webedit" {
		t.Fatalf("meta = %+v", h.refs.calls[0].meta)
	}
}

func TestCreateRejectsExistingAndExpectedBlob(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeCreate, Ref: "main", Path: "hello.txt", Message: "add", Content: []byte("x"),
	}); !isErr(err, ErrConflict) {
		t.Fatalf("existing = %v", err)
	}
	var ce *ConflictError
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeCreate, Ref: "main", Path: "new.txt", Message: "add", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !errors.As(err, &ce) && !isErr(err, ErrInvalid) {
		t.Fatalf("expected blob on create = %v", err)
	}
}

func TestEditPreconditions(t *testing.T) {
	h := newHarness(t)
	base := Input{Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "edit", Content: []byte("bye\n")}

	in := base
	in.ExpectedBlobSHA = ""
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); !isErr(err, ErrInvalid) {
		t.Fatalf("missing expected blob = %v", err)
	}
	in = base
	in.ExpectedBlobSHA = "9999999999999999999999999999999999999999"
	var ce *ConflictError
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); !errors.As(err, &ce) || ce.What != "blob" {
		t.Fatalf("blob moved = %v (%+v)", err, ce)
	}
	in = base
	in.ExpectedBlobSHA = testSHA
	in.ExpectedBranchSHA = "8888888888888888888888888888888888888888"
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); !errors.As(err, &ce) || ce.What != "branch" {
		t.Fatalf("branch moved = %v", err)
	}
	in.Path = "absent.txt"
	in.ExpectedBlobSHA = testSHA
	in.ExpectedBranchSHA = ""
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), in); !isErr(err, ErrNotFound) {
		t.Fatalf("absent = %v", err)
	}
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{Mode: ModeEdit, Ref: "nope", Path: "hello.txt", Message: "x"}); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown ref = %v", err)
	}
}

func TestDelete(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeDelete, Ref: "main", Path: "hello.txt", Message: "remove", ExpectedBlobSHA: testSHA,
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Commit == "" {
		t.Fatal("no commit")
	}
	if !h.git.builds[0].Delete {
		t.Fatal("build not flagged delete")
	}
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeDelete, Ref: "main", Path: "hello.txt", Message: "remove", ExpectedBlobSHA: testSHA, NewBranch: "gone",
	}); !isErr(err, ErrInvalid) {
		t.Fatalf("delete-on-new-branch = %v", err)
	}
}

func TestGuardrails(t *testing.T) {
	h := newHarness(t)
	edit := func(content []byte) error {
		_, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
			Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: content, ExpectedBlobSHA: testSHA,
		})
		return err
	}
	if err := edit([]byte("a\x00b")); !isErr(err, ErrUnsupported) {
		t.Fatalf("binary = %v", err)
	}
	if err := edit([]byte(lfsPointerPrefix + "\noid sha256:x\n")); !isErr(err, ErrUnsupported) {
		t.Fatalf("lfs = %v", err)
	}
	h.svc.MaxBytes = 4
	if err := edit([]byte("too long")); !isErr(err, ErrTooLarge) {
		t.Fatalf("oversize = %v", err)
	}
	h.svc.MaxBytes = 0
	h.git.attrs = []byte("*.bin filter=lfs\n")
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeCreate, Ref: "main", Path: "x.bin", Message: "e", Content: []byte("plain"),
	}); !isErr(err, ErrUnsupported) {
		t.Fatalf("gitattributes = %v", err)
	}
	// Existing binary/LFS blobs are refused for edit and delete.
	h.git.files["bin.dat"] = &File{Path: "bin.dat", SHA: "4444444444444444444444444444444444444444", Mode: "100644", Binary: true}
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "bin.dat", Message: "e", Content: []byte("x"), ExpectedBlobSHA: "4444444444444444444444444444444444444444",
	}); !isErr(err, ErrUnsupported) {
		t.Fatalf("existing binary = %v", err)
	}
	h.git.files["ptr.dat"] = &File{Path: "ptr.dat", SHA: "5555555555555555555555555555555555555555", Mode: "100644", LFS: true}
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeDelete, Ref: "main", Path: "ptr.dat", Message: "e", ExpectedBlobSHA: "5555555555555555555555555555555555555555",
	}); !isErr(err, ErrUnsupported) {
		t.Fatalf("existing lfs = %v", err)
	}
}

func TestNoChange(t *testing.T) {
	h := newHarness(t)
	h.git.noChange = true
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("hello\n"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrNoChange) {
		t.Fatalf("no-op = %v", err)
	}
	patch, err := h.svc.Preview(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("hello\n"), ExpectedBlobSHA: testSHA,
	})
	if err != nil || patch != "" {
		t.Fatalf("preview no-op = %q/%v", patch, err)
	}
}

func TestPolicy(t *testing.T) {
	protect := `{"version":1,"rules":[{"name":"locked","match":{"refs":["refs/heads/main"],"principals":["jane"]},"effect":{"protect":{"restricts":["update"],"bypass":["svc:ci"]}}}]}`
	h := newHarness(t)
	h.policy(t, protect)
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrForbidden) {
		t.Fatalf("protected = %v", err)
	}
	// A server principal bypassing the rule publishes.
	if _, err := h.svc.Save(ctx(), "o", "r", auth.Principal{Name: "ci"}, Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); err != nil {
		t.Fatalf("bypass = %v", err)
	}
	// required-reviews' push half denies direct pushes to a gated ref.
	h2 := newHarness(t)
	h2.policy(t, `{"version":1,"rules":[{"name":"reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":1}}}]}`)
	if _, err := h2.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrForbidden) {
		t.Fatalf("reviews = %v", err)
	}
	// Proposing a new branch targets refs/heads/<new>, which the rule below
	// does not match.
	if _, err := h2.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "propose/fix",
	}); err != nil {
		t.Fatalf("new branch = %v", err)
	}
	// An unparseable policy fails closed.
	h3 := newHarness(t)
	h3.policy(t, `{not json`)
	if _, err := h3.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !isErr(err, ErrForbidden) {
		t.Fatalf("unparseable = %v", err)
	}
}

func TestNewBranchAndPR(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "propose", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "fix/docs",
	})
	if err != nil {
		t.Fatalf("new branch: %v", err)
	}
	if res.Branch != "fix/docs" || res.PRNum != 0 {
		t.Fatalf("result = %+v", res)
	}
	call := h.refs.calls[0]
	if call.ref != "refs/heads/fix/docs" || strings.Trim(call.old, "0") != "" {
		t.Fatalf("create publish = %+v", call)
	}
	if len(h.pr.calls) != 0 {
		t.Fatal("PR opened without open_pr")
	}
	// With open_pr, the PR opens with Ref as base and the new branch as head.
	h2 := newHarness(t)
	res, err = h2.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "propose", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "fix/docs", OpenPR: true,
	})
	if err != nil {
		t.Fatalf("open pr: %v", err)
	}
	if res.PRNum != 7 || len(h2.pr.calls) != 1 {
		t.Fatalf("pr = %+v calls=%+v", res, h2.pr.calls)
	}
	if h2.pr.calls[0].BaseRef != "main" || h2.pr.calls[0].HeadRef != "fix/docs" || h2.pr.calls[0].Title != "Edit hello.txt" {
		t.Fatalf("pr input = %+v", h2.pr.calls[0])
	}
	// open_pr without new_branch is a 400.
	if _, err := h2.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "x", Content: []byte("x"), ExpectedBlobSHA: testSHA, OpenPR: true,
	}); !isErr(err, ErrInvalid) {
		t.Fatalf("open_pr alone = %v", err)
	}
	// A PR failure after the branch lands is reported truthfully.
	h3 := newHarness(t)
	h3.pr.err = errors.New("numbering down")
	if _, err := h3.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "x", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "fix/x", OpenPR: true,
	}); !isErr(err, ErrUnavailable) || !strings.Contains(err.Error(), "branch \"fix/x\" created") {
		t.Fatalf("pr failure = %v", err)
	}
	// New branch equal to base is refused.
	if _, err := h3.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "x", Content: []byte("x"),
		ExpectedBlobSHA: testSHA, NewBranch: "main",
	}); !isErr(err, ErrInvalid) {
		t.Fatalf("same branch = %v", err)
	}
}

func TestPublishConflict(t *testing.T) {
	h := newHarness(t)
	h.refs.err = fmt.Errorf("%w: CAS conflict", ErrConflict)
	var ce *ConflictError
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("x"), ExpectedBlobSHA: testSHA,
	}); !errors.As(err, &ce) {
		t.Fatalf("publish conflict = %v", err)
	}
	// The re-read fills the disk sha so the client can re-render from it.
	if ce.Actual != testSHA || ce.Expected != testSHA {
		t.Fatalf("conflict shas = %+v", ce)
	}
}

func TestPreviewAndLoad(t *testing.T) {
	h := newHarness(t)
	patch, err := h.svc.Preview(ctx(), "o", "r", writer(), Input{
		Mode: ModeEdit, Ref: "main", Path: "hello.txt", Message: "e", Content: []byte("bye\n"), ExpectedBlobSHA: testSHA,
	})
	if err != nil || !strings.Contains(patch, "+bye") {
		t.Fatalf("preview = %q/%v", patch, err)
	}
	if _, err := h.svc.Load(ctx(), "o", "r", writer(), "main", "hello.txt"); err != nil {
		t.Fatalf("load: %v", err)
	}
	h.gate.pushErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "read only"}
	if _, err := h.svc.Load(ctx(), "o", "r", reader(), "main", "hello.txt"); !isErr(err, ErrForbidden) {
		t.Fatalf("load gate = %v", err)
	}
	h.gate.pushErr = nil
	if _, err := h.svc.Load(ctx(), "o", "r", writer(), "main", "../x"); !isErr(err, ErrInvalid) {
		t.Fatalf("load path = %v", err)
	}
	if _, err := h.svc.Load(ctx(), "o", "r", writer(), "nope", "x"); !isErr(err, ErrNotFound) {
		t.Fatalf("load ref = %v", err)
	}
}

func TestUnwired(t *testing.T) {
	h := newHarness(t)
	h.svc.Git = nil
	if _, err := h.svc.Save(ctx(), "o", "r", writer(), Input{Mode: ModeEdit, Ref: "main", Path: "x", Message: "m", Content: []byte("x")}); !isErr(err, ErrUnavailable) {
		t.Fatalf("unwired = %v", err)
	}
	h2 := newHarness(t)
	h2.svc.Dirs = &fakeDirs{}
	if _, err := h2.svc.Save(ctx(), "o", "r", writer(), Input{Mode: ModeEdit, Ref: "main", Path: "x", Message: "m", Content: []byte("x")}); !isErr(err, ErrUnavailable) {
		t.Fatalf("dir failure = %v", err)
	}
}

func TestDecodeContent(t *testing.T) {
	if b, err := DecodeContent("aGVsbG8="); err != nil || string(b) != "hello" {
		t.Fatalf("padded = %q/%v", b, err)
	}
	if b, err := DecodeContent("aGVsbG8"); err != nil || string(b) != "hello" {
		t.Fatalf("raw = %q/%v", b, err)
	}
	if _, err := DecodeContent("!!!"); err == nil {
		t.Fatal("garbage decoded")
	}
}

func TestConflictErrorMessage(t *testing.T) {
	ce := &ConflictError{What: "branch", Expected: "aaa", Actual: "bbb"}
	if !strings.Contains(ce.Error(), "page: aaa") || !strings.Contains(ce.Error(), "disk: bbb") {
		t.Fatalf("branch message = %q", ce.Error())
	}
	be := &ConflictError{What: "blob", Actual: "ccc", Why: "file exists"}
	if !strings.Contains(be.Error(), "file exists") || !strings.Contains(be.Error(), "ccc") {
		t.Fatalf("blob message = %q", be.Error())
	}
	if !errors.Is(be, ErrConflict) {
		t.Fatal("conflict sentinel missing")
	}
}
