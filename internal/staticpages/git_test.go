package staticpages

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type gitFixture struct {
	dir    string // bare serving copy (what RepoDirs hands the runner)
	commit string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "serving.git")
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=2026-01-02T03:04:05Z",
	)
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run(t, base, "init", "-q", "-b", "main", work)
	write := func(rel, body string) {
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<h1>hello</h1>")
	write("docs/index.html", "<h1>docs</h1>")
	write("assets/app.css", "body{}")
	write("404.html", "<h1>gone</h1>")
	if err := os.Symlink("index.html", filepath.Join(work, "link.html")); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", "-A")
	run(t, work, "commit", "-q", "-m", "one")
	// A gitlink (160000) so the runner's mode parsing is pinned too.
	head := run(t, work, "rev-parse", "HEAD")
	run(t, work, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	run(t, work, "commit", "-q", "-m", "two")
	run(t, base, "clone", "-q", "--bare", work, bare)

	return &gitFixture{
		dir:    bare,
		commit: run(t, bare, "rev-parse", "HEAD"),
	}
}

func TestSubprocessGitExactArgv(t *testing.T) {
	fx := newGitFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "argv.log")
	script := filepath.Join(t.TempDir(), "git")
	body := "#!/bin/sh\n" +
		"printf 'DIR=%s PROMPT=%s ARGS=%s\\n' \"$GIT_DIR\" \"$GIT_TERMINAL_PROMPT\" \"$*\" >> " + shellQuote(logPath) + "\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	g := NewSubprocessGit(script)
	ctx := context.Background()

	commit, err := g.Resolve(ctx, fx.dir, "")
	if err != nil || commit != fx.commit {
		t.Fatalf("Resolve = %q, %v; want %q", commit, err, fx.commit)
	}
	entry, found, err := g.Lookup(ctx, fx.dir, commit, "index.html")
	if err != nil || !found {
		t.Fatalf("Lookup(index.html) = %+v, %v, %v", entry, found, err)
	}
	if entry.Mode != "100644" || entry.Type != "blob" || entry.Size != int64(len("<h1>hello</h1>")) {
		t.Errorf("index entry = %+v", entry)
	}
	indexOID := entry.OID

	if entry, found, err = g.Lookup(ctx, fx.dir, commit, "docs"); err != nil || !found {
		t.Fatalf("Lookup(docs) = %+v, %v, %v", entry, found, err)
	}
	if entry.Mode != "040000" || entry.Type != "tree" || entry.Size != -1 {
		t.Errorf("docs entry = %+v", entry)
	}
	if _, found, err = g.Lookup(ctx, fx.dir, commit, "missing.txt"); err != nil || found {
		t.Fatalf("Lookup(missing) found=%v err=%v", found, err)
	}
	if entry, found, err = g.Lookup(ctx, fx.dir, commit, "link.html"); err != nil || !found {
		t.Fatalf("Lookup(link) = %+v, %v, %v", entry, found, err)
	}
	if entry.Mode != "120000" {
		t.Errorf("symlink mode = %q, want 120000", entry.Mode)
	}
	if entry, found, err = g.Lookup(ctx, fx.dir, commit, "sub"); err != nil || !found {
		t.Fatalf("Lookup(sub) = %+v, %v, %v", entry, found, err)
	}
	if entry.Mode != "160000" || entry.Type != "commit" {
		t.Errorf("gitlink entry = %+v", entry)
	}

	var blob bytes.Buffer
	if err := g.ReadBlob(ctx, fx.dir, indexOID, &blob); err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if blob.String() != "<h1>hello</h1>" {
		t.Errorf("blob = %q", blob.String())
	}
	if commit2, err := g.Resolve(ctx, fx.dir, "refs/heads/main"); err != nil || commit2 != fx.commit {
		t.Fatalf("Resolve(refs/heads/main) = %q, %v", commit2, err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	want := []string{
		"DIR=" + fx.dir + " PROMPT=0 ARGS=rev-parse --verify --quiet HEAD^{commit}",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=ls-tree -z -l -- " + commit + " index.html",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=ls-tree -z -l -- " + commit + " docs",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=ls-tree -z -l -- " + commit + " missing.txt",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=ls-tree -z -l -- " + commit + " link.html",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=ls-tree -z -l -- " + commit + " sub",
		"DIR=" + fx.dir + " PROMPT=0 ARGS=cat-file blob " + indexOID,
		"DIR=" + fx.dir + " PROMPT=0 ARGS=rev-parse --verify --quiet refs/heads/main^{commit}",
	}
	if len(got) != len(want) {
		t.Fatalf("ran %d commands, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestSubprocessGitMissingRef(t *testing.T) {
	fx := newGitFixture(t)
	g := NewSubprocessGit("")
	_, err := g.Resolve(context.Background(), fx.dir, "refs/heads/nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := g.Resolve(context.Background(), fx.dir, "HEAD"); err != nil {
		t.Fatalf("Resolve(HEAD) on a repo with commits: %v", err)
	}
}

func TestSubprocessGitRejectsBadArguments(t *testing.T) {
	fx := newGitFixture(t)
	g := NewSubprocessGit("")
	ctx := context.Background()
	if _, _, err := g.Lookup(ctx, fx.dir, "short", "index.html"); !errors.Is(err, ErrInvalid) {
		t.Errorf("short treeish err = %v", err)
	}
	if _, _, err := g.Lookup(ctx, fx.dir, fx.commit, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty path err = %v", err)
	}
	if _, _, err := g.Lookup(ctx, fx.dir, fx.commit, ":magic"); !errors.Is(err, ErrInvalid) {
		t.Errorf("pathspec magic err = %v", err)
	}
	if err := g.ReadBlob(ctx, fx.dir, "not-an-oid", &bytes.Buffer{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad oid err = %v", err)
	}
	if err := g.ReadBlob(ctx, fx.dir, oid("x"), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil writer err = %v", err)
	}
}

func TestSubprocessGitLookupFailureIsUnavailable(t *testing.T) {
	fx := newGitFixture(t)
	g := NewSubprocessGit("")
	// A well-formed oid that names no object: ls-tree prints to stderr and fails.
	_, _, err := g.Lookup(context.Background(), fx.dir, strings.Repeat("a", 40), "index.html")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "ls-tree") {
		t.Errorf("error should name the argv: %v", err)
	}
}

func TestSubprocessGitCanceledContext(t *testing.T) {
	fx := newGitFixture(t)
	g := NewSubprocessGit("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Resolve(ctx, fx.dir, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if _, _, err := g.Lookup(ctx, fx.dir, fx.commit, "index.html"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lookup err = %v, want ErrUnavailable", err)
	}
	var blob bytes.Buffer
	if err := g.ReadBlob(ctx, fx.dir, oid("x"), &blob); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ReadBlob err = %v", err)
	}
}

// TestSubprocessGitStartFailures covers the errors that are not git refusals:
// a missing binary, and a resolver that exits 0 without producing a commit id.
func TestSubprocessGitStartFailures(t *testing.T) {
	fx := newGitFixture(t)
	missing := NewSubprocessGit(filepath.Join(t.TempDir(), "no-such-git"))
	ctx := context.Background()
	if _, err := missing.Resolve(ctx, fx.dir, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Resolve err = %v", err)
	}
	if _, _, err := missing.Lookup(ctx, fx.dir, fx.commit, "index.html"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Lookup err = %v", err)
	}
	if err := missing.ReadBlob(ctx, fx.dir, oid("index.html"), &bytes.Buffer{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReadBlob err = %v", err)
	}

	silentOK := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(silentOK, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSubprocessGit(silentOK).Resolve(ctx, fx.dir, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty rev-parse output err = %v", err)
	}
}

// TestGitErrorRendering pins the message shapes: argv, optional stderr, and
// the stderr truncation that keeps a huge git diagnostic out of the response.
func TestGitErrorRendering(t *testing.T) {
	plain := &gitError{argv: []string{"rev-parse", "--verify"}, err: errors.New("exit status 1")}
	if got, want := plain.Error(), "git rev-parse --verify: exit status 1"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	withStderr := &gitError{argv: []string{"cat-file"}, err: errors.New("exit status 1"), stderr: "  fatal: nope\n"}
	if got := withStderr.Error(); !strings.Contains(got, "fatal: nope") || strings.Contains(got, "\n") {
		t.Errorf("stderr not rendered cleanly: %q", got)
	}
	long := &gitError{argv: []string{"x"}, err: errors.New("e"), stderr: strings.Repeat("z", 500)}
	if got := long.Error(); len(got) != len("git x: e: ")+200 {
		t.Errorf("stderr not truncated to 200 bytes: %d", len(got))
	}
	if !errors.Is(&gitError{err: ErrInvalid}, ErrInvalid) {
		t.Error("gitError does not unwrap to its cause")
	}

	// A silent failing binary is not a name miss: without the verify exit status
	// or a git diagnostic there is nothing that says "no such ref" (m10).
	silent := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newGitFixture(t)
	if _, err := NewSubprocessGit(silent).Resolve(context.Background(), fx.dir, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// exitError builds a real *exec.ExitError with the given status, so the
// classification table exercises the exit-status branch without a git wrapper.
func exitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("exit %d did not produce *exec.ExitError: %v", code, err)
	}
	return ee
}

// TestUnknownRevisionClassification pins m10: only "this name does not
// resolve" is a 404. Silence plus the --verify exit status is a miss (that is
// what --quiet prints for an unknown ref); any diagnostic git wanted to name, or
// any other exit status, is an unavailable repository.
func TestUnknownRevisionClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "marker: needed a single revision",
			err:  &gitError{err: exitError(t, 128), stderr: "fatal: Needed a single revision"},
			want: true},
		{name: "marker: unknown revision, any case",
			err:  &gitError{err: exitError(t, 128), stderr: "FATAL: Unknown revision or path\n"},
			want: true},
		{name: "marker: not a valid object name",
			err:  &gitError{err: exitError(t, 128), stderr: "fatal: Not a valid object name: 'x'"},
			want: true},
		{name: "marker: bad revision",
			err:  &gitError{err: exitError(t, 128), stderr: "fatal: bad revision 'x..y'"},
			want: true},
		{name: "silent verify failure (a --quiet miss)",
			err:  &gitError{err: exitError(t, 1)},
			want: true},
		{name: "corrupt object",
			err:  &gitError{err: exitError(t, 128), stderr: "fatal: loose object is corrupt"},
			want: false},
		{name: "broken repository",
			err:  &gitError{err: exitError(t, 128), stderr: "fatal: not a git repository: '/x'"},
			want: false},
		{name: "silent non-verify exit",
			err:  &gitError{err: exitError(t, 3)},
			want: false},
		{name: "not a git error at all",
			err:  errors.New("boom"),
			want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unknownRevision(tc.err); got != tc.want {
				t.Fatalf("unknownRevision(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSubprocessGitResolveInfraFailures covers the real-repository shapes m10
// calls out: a directory that is not a repository, a serving copy that is gone,
// and a corrupt commit object are all unavailable — never a 404.
func TestSubprocessGitResolveInfraFailures(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	g := NewSubprocessGit("")
	ctx := context.Background()

	notRepo := t.TempDir()
	_, err := g.Resolve(ctx, notRepo, "")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("not-a-repository err = %v, want ErrUnavailable", err)
	}
	_, err = g.Resolve(ctx, filepath.Join(notRepo, "gone"), "")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("missing serving copy err = %v, want ErrUnavailable", err)
	}
	if _, err := g.Resolve(ctx, corruptLooseRepo(t), ""); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt object err = %v, want ErrUnavailable", err)
	}
	// The same repository without corruption resolves, so the fixture above is
	// only failing because of the corruption.
	fx := newGitFixture(t)
	if _, err := g.Resolve(ctx, fx.dir, ""); err != nil {
		t.Fatalf("healthy Resolve: %v", err)
	}
}

// corruptLooseRepo builds a bare repository with one loose commit object and
// then corrupts that object's bytes, so the failure is real object corruption
// rather than a fabricated stderr string.
func corruptLooseRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "corrupt.git")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "--bare", "-b", "main", dir)
	tree := run("--git-dir="+dir, "mktree") // empty stdin = the empty tree
	commit := run("--git-dir="+dir, "commit-tree", tree, "-m", "one")
	run("--git-dir="+dir, "update-ref", "refs/heads/main", commit)
	obj := filepath.Join(dir, "objects", commit[:2], commit[2:])
	if err := os.Chmod(obj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("not a zlib stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSubprocessGitResolveReadsGitDiagnostics proves the stderr plumbing: the
// loud "does not resolve" diagnostic is a 404 even at a non-verify exit status,
// while a repository failure at the same status is a 503.
func TestSubprocessGitResolveReadsGitDiagnostics(t *testing.T) {
	fx := newGitFixture(t)
	ctx := context.Background()
	script := func(body string) *SubprocessGit {
		path := filepath.Join(t.TempDir(), "git")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return NewSubprocessGit(path)
	}
	missing := script("printf 'fatal: Needed a single revision\\n' >&2\nexit 128\n")
	if _, err := missing.Resolve(ctx, fx.dir, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown-revision diagnostic err = %v, want ErrNotFound", err)
	}
	broken := script("printf 'error: unable to unpack header\\nfatal: loose object is corrupt\\n' >&2\nexit 128\n")
	_, err := broken.Resolve(ctx, fx.dir, "")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt-repository diagnostic err = %v, want ErrUnavailable", err)
	}
}

func TestSubprocessGitDefaults(t *testing.T) {
	if got := NewSubprocessGit("").bin(); got != "git" {
		t.Errorf("bin = %q", got)
	}
	zero := &SubprocessGit{}
	if zero.bin() != "git" || zero.timeout() != DefaultGitTimeout {
		t.Errorf("zero value = bin %q timeout %v", zero.bin(), zero.timeout())
	}
	short := &SubprocessGit{Timeout: time.Second}
	if short.timeout() != time.Second {
		t.Errorf("timeout = %v", short.timeout())
	}
	if NewSubprocessGit("").slots() == nil {
		t.Error("pool not built")
	}
}

func TestGitPoolCanceledWithFreeSlot(t *testing.T) {
	p := newGitPool(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := p.run(ctx, func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("fn ran with a canceled context")
	}
}

func TestGitPoolCanceledWhenFull(t *testing.T) {
	p := newGitPool(1)
	p.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := p.run(ctx, func() error { ran = true; return nil })
	<-p.sem
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("fn ran without a slot")
	}
}

func TestGitPoolDefaultCapacity(t *testing.T) {
	p := newGitPool(0)
	if cap(p.sem) <= 0 {
		t.Fatalf("capacity = %d", cap(p.sem))
	}
	if err := p.run(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// shellQuote single-quotes a path for the /bin/sh wrapper script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
