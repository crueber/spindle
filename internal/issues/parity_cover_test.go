package issues

// parity_cover_test.go — the error and edge arms of the parity surfaces
// (pin/dependency/template verbs, their HTTP handlers, and the subprocess
// template reader against a real git repository). The happy paths live in
// parity_test.go / templates_test.go.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- subprocess template reader ---------------------------------------------

type fixedDirs struct {
	dir string
	err error
}

func (f fixedDirs) Dir(context.Context, string) (string, error) { return f.dir, f.err }

// gitRepo builds a real repository with one commit and returns (dir, sha).
func gitRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		out, rerr := cmd.CombinedOutput()
		if rerr != nil {
			t.Fatalf("git %v: %v: %s", args, rerr, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "seed")
	sha := run("rev-parse", "HEAD")
	// The reader talks to a BARE serving copy (GIT_DIR = the repository), so
	// clone the fixture bare and hand that path to the reader.
	bare := t.TempDir()
	clone := exec.Command(git, "clone", "-q", "--bare", dir, bare)
	clone.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, cerr := clone.CombinedOutput(); cerr != nil {
		t.Fatalf("clone --bare: %v: %s", cerr, out)
	}
	return bare, sha
}

func TestSubprocessTemplateGit(t *testing.T) {
	dir, sha := gitRepo(t, map[string]string{
		".spindle/ISSUE_TEMPLATE/bug.md":      "---\nname: Bug\n---\nbody\n",
		".spindle/ISSUE_TEMPLATE/notes.txt":   "ignored\n",
		".spindle/ISSUE_TEMPLATE/nested/x.md": "nested\n",
		"README.md":                           "hi\n",
	})
	g := NewSubprocessTemplateGit("", fixedDirs{dir: dir})
	if g.Timeout <= 0 || cap(g.sem) <= 0 {
		t.Fatalf("constructor defaults: %+v", g)
	}
	ctx := context.Background()
	files, err := g.ListTemplateFiles(ctx, "acme/repo", sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != ".spindle/ISSUE_TEMPLATE/bug.md" {
		t.Fatalf("files = %v (only direct .md children belong)", files)
	}
	body, err := g.ReadFile(ctx, "acme/repo", sha, files[0])
	if err != nil || !strings.Contains(string(body), "name: Bug") {
		t.Fatalf("read = %q, %v", body, err)
	}
	resolved, err := g.ResolveRef(ctx, "acme/repo", "HEAD")
	if err != nil || resolved != sha {
		t.Fatalf("resolve = %q, %v (want %q)", resolved, err, sha)
	}

	// Invalid inputs are refused before git runs.
	if _, err := g.ListTemplateFiles(ctx, "acme/repo", "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad commit err = %v", err)
	}
	if _, err := g.ReadFile(ctx, "acme/repo", sha, "-x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad path err = %v", err)
	}
	if _, err := g.ReadFile(ctx, "acme/repo", sha, "a:b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("colon path err = %v", err)
	}
	if _, err := g.ResolveRef(ctx, "acme/repo", "-x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad ref err = %v", err)
	}
	// Missing ref is the package's not-found class.
	if _, err := g.ResolveRef(ctx, "acme/repo", "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
	// Missing path is the subprocess not-found class.
	if _, err := g.ReadFile(ctx, "acme/repo", sha, ".spindle/ISSUE_TEMPLATE/nope.md"); err == nil {
		t.Fatal("missing path must error")
	}
	// No resolver is an error, never a panic.
	none := NewSubprocessTemplateGit("git", nil)
	if _, err := none.ListTemplateFiles(ctx, "acme/repo", sha); err == nil {
		t.Fatal("nil dirs must error")
	}
	if _, err := g.ListTemplateFiles(ctx, "acme/repo", sha); err != nil {
		t.Fatalf("resolver error propagation setup: %v", err)
	}
	bad := NewSubprocessTemplateGit("git", fixedDirs{err: errors.New("registry down")})
	if _, err := bad.ListTemplateFiles(ctx, "acme/repo", sha); err == nil {
		t.Fatal("resolver failure must surface")
	}

	// A missing directory is empty output, not an error.
	plainDir, plainSHA := gitRepo(t, map[string]string{"README.md": "x\n"})
	plain := NewSubprocessTemplateGit("git", fixedDirs{dir: plainDir})
	if files, err := plain.ListTemplateFiles(ctx, "acme/repo", plainSHA); err != nil || len(files) != 0 {
		t.Fatalf("missing dir = %v, %v", files, err)
	}

	// Oversized files are refused by the reader.
	bigDir, bigSHA := gitRepo(t, map[string]string{
		".spindle/ISSUE_TEMPLATE/huge.md": strings.Repeat("x", maxTemplateFileBytes+10),
	})
	big := NewSubprocessTemplateGit("git", fixedDirs{dir: bigDir})
	if _, err := big.ReadFile(ctx, "acme/repo", bigSHA, ".spindle/ISSUE_TEMPLATE/huge.md"); err == nil {
		t.Fatal("oversized file must error")
	}

	// A canceled context with a saturated pool exits on ctx, not on the git
	// process (deterministic: the pool has no free slot).
	cctx, cancel := context.WithCancel(context.Background())
	g.sem = make(chan struct{}, 1)
	g.sem <- struct{}{}
	cancel()
	if _, err := g.ListTemplateFiles(cctx, "acme/repo", sha); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v", err)
	}
	_ = time.Second
}

func TestTemplatePathRules(t *testing.T) {
	cases := map[string]bool{
		".spindle/ISSUE_TEMPLATE/bug.md":        true,
		".spindle/ISSUE_TEMPLATE/BUG.MD":        true,
		".spindle/ISSUE_TEMPLATE/notes.txt":     false,
		".spindle/ISSUE_TEMPLATE/nested/bug.md": false,
		".spindle/workflows/ci.yml":             false,
		"README.md":                             false,
	}
	for p, want := range cases {
		if got := templatePath(p); got != want {
			t.Errorf("templatePath(%q) = %v, want %v", p, got, want)
		}
	}
	if !templateSHA(strings.Repeat("a", 40)) || !templateSHA(strings.Repeat("0", 64)) {
		t.Fatal("valid shas rejected")
	}
	for _, bad := range []string{"", strings.Repeat("a", 39), strings.Repeat("g", 40), strings.Repeat("A", 40)} {
		if templateSHA(bad) {
			t.Errorf("templateSHA(%q) = true", bad)
		}
	}
}

// --- dependency error/edge arms ---------------------------------------------

func TestDependencyEdgeArms(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")

	// num < 1 is unknown.
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", 0, janeP, DepBlocks, b.Num); !errors.Is(err, ErrNotFound) {
		t.Fatalf("num 0 err = %v", err)
	}
	// Remove with no document at all is a no-op.
	if changed, err := s.RemoveDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, b.Num); err != nil || changed {
		t.Fatalf("remove with no doc = %v, %v", changed, err)
	}
	// A corrupt link document fails closed on read and write.
	if _, err := store.PutBytes(reqCtx(), s.Store, DependencyKey("acme", "repo"), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DependenciesFor(reqCtx(), "acme", "repo", a.Num, janeP); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt read err = %v", err)
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, b.Num); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt write err = %v", err)
	}
	if _, err := s.RemoveDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, b.Num); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt remove err = %v", err)
	}
	// DependenciesFor gates reads and rejects unknown issues.
	roles.private["acme/repo"] = true
	if _, err := s.DependenciesFor(reqCtx(), "acme", "repo", a.Num, anonP); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon read err = %v", err)
	}
	delete(roles.private, "acme/repo")
	if _, err := s.DependenciesFor(reqCtx(), "acme", "repo", 999, janeP); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown read err = %v", err)
	}
	// A PR-kind target is 404 (issue endpoints never leak the shared family).
	pth := &Thread{Num: 3, Kind: "pr", Title: "pr", State: StateOpen, Author: "jane@example.com",
		CreatedAt: "2026-11-18T10:00:00Z", UpdatedAt: "2026-11-18T10:00:00Z",
		Participants: []string{"jane@example.com"}, NextEventSeq: 1, Version: 1}
	if _, err := store.PutBytes(reqCtx(), s.Store, ThreadKey("acme", "repo", 3), encodeThread(pth), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PR target err = %v", err)
	}
	// Missing blocker target is 404 too (the second loop arm).
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlockedBy, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blocker err = %v", err)
	}
}

func TestDependencyCaps(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	hub := mustCreate(t, s, "acme", "repo", janeP, "hub", "")
	var targets []int
	for i := 0; i < MaxDependenciesPerIssue+1; i++ {
		targets = append(targets, mustCreate(t, s, "acme", "repo", janeP, "t", "").Num)
	}
	for _, n := range targets[:MaxDependenciesPerIssue] {
		if _, err := s.AddDependency(reqCtx(), "acme", "repo", hub.Num, janeP, DepBlockedBy, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", hub.Num, janeP, DepBlockedBy, targets[MaxDependenciesPerIssue]); !errors.Is(err, ErrConflict) {
		t.Fatalf("per-issue cap err = %v", err)
	}
}

func TestPinErrorArms(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	// Corrupt index: the pin loop surfaces the parse failure.
	if _, err := store.PutBytes(reqCtx(), s.Store, IndexKey("acme", "repo"), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinnedIssues(reqCtx(), "acme", "repo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("pinned read err = %v", err)
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", th.Num, janeP, true); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("pin write err = %v", err)
	}
	// A PR-kind thread is unknown to the pin verb.
	if _, err := store.PutBytes(reqCtx(), s.Store, IndexKey("acme", "repo"),
		[]byte(`{"version":1,"open":[],"closed_recent":[],"card_version":1}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	pth := &Thread{Num: 999, Kind: "pr", Title: "pr", State: StateOpen, Author: "jane@example.com",
		CreatedAt: "2026-11-18T10:00:00Z", UpdatedAt: "2026-11-18T10:00:00Z",
		Participants: []string{"jane@example.com"}, NextEventSeq: 1, Version: 1}
	if err := s.putCreate(reqCtx(), ThreadKey("acme", "repo", 999), encodeThread(pth)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", 999, janeP, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PR pin err = %v", err)
	}
	// The cap arm.
	var nums []int
	for i := 0; i < MaxPinned+1; i++ {
		nums = append(nums, mustCreate(t, s, "acme", "repo", janeP, "t", "").Num)
	}
	for _, n := range nums[:MaxPinned] {
		if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", n, janeP, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", nums[MaxPinned], janeP, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin cap err = %v", err)
	}
}

// --- HTTP error arms ---------------------------------------------------------

func TestParityHandlerErrorArms(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	h := testHandler(s, janeP)

	// Pin: unknown num and anonymous.
	if rec := doReq(h, http.MethodPut, "/acme/repo/api/issues/999/pin", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pin = %d", rec.Code)
	}
	if rec := doReq(testHandler(s, anonP), http.MethodPut, "/acme/repo/api/issues/"+itoa(a.Num)+"/pin", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon pin = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodGet, "/acme/repo/api/issues/"+itoa(a.Num)+"/pin", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET pin = %d", rec.Code)
	}

	// Dependencies: unknown fields, bad target, cycle, unknown target num.
	base := "/acme/repo/api/issues/" + itoa(a.Num) + "/dependencies"
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("target 0 = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing target = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodDelete, base+"/blocks/abc", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad target segment = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodDelete, base+"/blocks/999", ""); rec.Code != http.StatusOK {
		// removing a non-existent link is idempotent, but the response view
		// is built for the EXISTING issue only.
		t.Fatalf("remove absent = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"sideways","target":`+itoa(b.Num)+`}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":999}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":`+itoa(a.Num)+`}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("self link = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":`+itoa(b.Num)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("link = %d", rec.Code)
	}
	back := "/acme/repo/api/issues/" + itoa(b.Num) + "/dependencies"
	if rec := doReq(h, http.MethodPost, back, `{"kind":"blocks","target":`+itoa(a.Num)+`}`); rec.Code != http.StatusConflict {
		t.Fatalf("cycle = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(h, http.MethodPut, base, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT deps = %d", rec.Code)
	}
}

// --- exported reads -----------------------------------------------------------

func TestExportedReads(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	if _, err := s.ListLabels(reqCtx(), "acme", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", janeP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	ls, err := s.ListLabels(reqCtx(), "acme", "repo")
	if err != nil || len(ls) != 1 {
		t.Fatalf("labels = %v, %v", ls, err)
	}
	if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", janeP, "v1", "", nil); err != nil {
		t.Fatal(err)
	}
	ms, err := s.ListMilestones(reqCtx(), "acme", "repo")
	if err != nil || len(ms) != 1 {
		t.Fatalf("milestones = %v, %v", ms, err)
	}
}

func TestTemplatesCorruptAndSkipLogging(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.Log = nil // nil-logger arms
	if _, err := store.PutBytes(reqCtx(), s.Store, TemplateKey("acme", "repo"), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Templates(reqCtx(), "acme", "repo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt templates err = %v", err)
	}
	// A refresh over a corrupt projection fails closed, and per-file read
	// errors are skipped (not fatal).
	s.TemplateGit = &skipTemplateGit{commit: strings.Repeat("a", 40)}
	if _, err := s.RefreshTemplates(reqCtx(), "acme", "repo", strings.Repeat("a", 40)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("refresh over corrupt = %v", err)
	}
	if err := s.Store.Delete(reqCtx(), TemplateKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RefreshTemplates(reqCtx(), "acme", "repo", strings.Repeat("a", 40))
	if err != nil || !changed {
		t.Fatalf("refresh with a read failure = %v, %v (one good file survives)", changed, err)
	}
	doc, _ := s.Templates(reqCtx(), "acme", "repo")
	if len(doc.Templates) != 1 || doc.Templates[0].Name != "Good" {
		t.Fatalf("templates = %+v", doc.Templates)
	}
	if _, err := s.RefreshTemplates(reqCtx(), "acme", "repo", "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad commit err = %v", err)
	}
}

// skipTemplateGit lists one good and one unreadable file.
type skipTemplateGit struct {
	commit string
	refErr error
}

func (f *skipTemplateGit) ListTemplateFiles(context.Context, string, string) ([]string, error) {
	return []string{".spindle/ISSUE_TEMPLATE/bad.md", ".spindle/ISSUE_TEMPLATE/good.md"}, nil
}

func (f *skipTemplateGit) ReadFile(_ context.Context, _, _, p string) ([]byte, error) {
	if strings.HasSuffix(p, "bad.md") {
		return nil, errors.New("unreadable")
	}
	return []byte("---\nname: Good\n---\n"), nil
}

func (f *skipTemplateGit) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}

var _ = auth.Anonymous

func TestParitySmallArms(t *testing.T) {
	// pinSet drops non-positive numbers and dedupes.
	if got := pinSet([]int{0, -1, 3, 3, 2}); len(got) != 2 || got[0] != 3 || got[1] != 2 {
		t.Fatalf("pinSet = %v", got)
	}

	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	// Templates normalizes a document without the array.
	if _, err := store.PutBytes(reqCtx(), s.Store, TemplateKey("acme", "repo"), []byte(`{"version":1}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Templates(reqCtx(), "acme", "repo")
	if err != nil || doc.Templates == nil || len(doc.Templates) != 0 {
		t.Fatalf("normalized templates = %+v, %v", doc, err)
	}
	// A logger makes the per-file skip path run its log arm.
	s.Log = slog.Default()
	s.TemplateGit = &skipTemplateGit{commit: strings.Repeat("b", 40)}
	if _, err := s.RefreshTemplates(reqCtx(), "acme", "repo", strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	// A resolver failure that is NOT not-found surfaces.
	s.TemplateGit = &refErrTemplateGit{skipTemplateGit{commit: strings.Repeat("c", 40), refErr: errors.New("git down")}}
	if _, err := s.RefreshTemplatesFromDefaultBranch(reqCtx(), "acme/repo"); err == nil {
		t.Fatal("resolver failure must surface")
	}

	// Handler read-gate arms for the new GETs.
	anonH := testHandler(s, anonP)
	roles.private["acme/repo"] = true
	if rec := doReq(anonH, http.MethodGet, "/acme/repo/api/issue-templates", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon templates = %d", rec.Code)
	}
	// A corrupt link document surfaces through the deps handlers.
	th := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	th2 := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	delete(roles.private, "acme/repo")
	if _, err := store.PutBytes(reqCtx(), s.Store, DependencyKey("acme", "repo"), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, janeP)
	base := "/acme/repo/api/issues/" + itoa(th.Num) + "/dependencies"
	if rec := doReq(h, http.MethodGet, base, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt deps GET = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":`+itoa(th2.Num)+`}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt deps POST = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodDelete, base+"/blocks/"+itoa(th2.Num), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt deps DELETE = %d", rec.Code)
	}
	// A nil-binary reader defaults to "git"; the mode defaults are present.
	g := NewSubprocessTemplateGit("", nil)
	if g.Binary != "git" || g.Timeout <= 0 {
		t.Fatalf("reader defaults = %+v", g)
	}
}

// refErr makes ResolveRef fail with a non-not-found error.
type refErrTemplateGit struct{ skipTemplateGit }

func (f *refErrTemplateGit) ResolveRef(context.Context, string, string) (string, error) {
	return "", f.refErr
}

func (f *refErrTemplateGit) ListTemplateFiles(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func TestParityHandlerAuthErrorArms(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	h := &Handler{Svc: s, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "auth down"}
	}}
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPut, "/acme/repo/api/issues/" + itoa(th.Num) + "/pin", ""},
		{http.MethodGet, "/acme/repo/api/issues/" + itoa(th.Num) + "/dependencies", ""},
		{http.MethodPost, "/acme/repo/api/issues/" + itoa(th.Num) + "/dependencies", `{"kind":"blocks","target":2}`},
		{http.MethodDelete, "/acme/repo/api/issues/" + itoa(th.Num) + "/dependencies/blocks/2", ""},
		{http.MethodGet, "/acme/repo/api/issue-templates", ""},
	} {
		if rec := doReq(h, req.method, req.path, req.body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d", req.method, req.path, rec.Code)
		}
	}
}
