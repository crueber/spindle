// grep_test.go — 04_git.md §17 / docs/features/20_search.md §6:
// the `git grep` primitive against real git fixtures, plus the streaming
// parser, the caps, the exit-code classification and the pool isolation.
//
// Fixtures are real repositories (built in t.TempDir(), then cloned bare so
// the measured copy is the serving shape); the git identity comes from
// TestMain. No test is skipped: a stub `git` script stands in only for
// process-level behavior (timeout, cancellation, exit codes) that a real
// repository cannot produce on demand.
package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// grepFixture is one serving-copy fixture. First is the first commit (ref.txt
// says "version one"); Second adds the awkward paths and changes ref.txt.
type grepFixture struct {
	Layer  *Layer
	Repo   *LocalRepo
	First  string
	Second string
	Weird  string // the path containing a colon and a newline
}

func grepWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newGrepFixture builds a worktree repo, commits twice, then clones it bare so
// the searched copy has no worktree at all (the production serving shape).
func newGrepFixture(t *testing.T) *grepFixture {
	t.Helper()
	src := t.TempDir()
	work := filepath.Join(src, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	activityGit(t, work, nil, "init", "-b", "main", "-q")
	// Pin line-ending conversion in the FIXTURE repo: a developer's global
	// core.autocrlf=true would otherwise store crlf.txt as LF, so the local
	// run would pass while CI (no global config) sees the real CRLF blob.
	// The repo's "tests that spawn git pin their identity" rule, extended to
	// eol conversion.
	activityGit(t, work, nil, "config", "core.autocrlf", "false")

	grepWrite(t, work, "ref.txt", "hello version one\n")
	grepWrite(t, work, "README.md", "# demo\nhello world\n")
	grepWrite(t, work, "cmd/app/main.go", "package main\n\nfunc main() {\n\thello()\n}\n")
	grepWrite(t, work, "pkg/util.go", "package pkg\n// HelloUtil does things\nfunc HelloUtil() {}\n")
	grepWrite(t, work, "docs/notes.txt", "hello from notes\nsecond line\n")
	grepWrite(t, work, "uni.txt", "Привет мир\nCAFÉ time\n")
	grepWrite(t, work, "bin.dat", "hello\x00binary payload\n")
	grepWrite(t, work, "long.txt", "start "+strings.Repeat("x", 60000)+" needle end\n")
	grepWrite(t, work, "crlf.txt", "hello crlf\r\n")
	grepWrite(t, work, "nonewline.txt", "no newline hello")
	grepWrite(t, work, "deep/a/b/c.go", "package c\n")
	activityGit(t, work, nil, "-c", "core.autocrlf=false", "add", "-A")
	activityGit(t, work, nil, "commit", "-qm", "first")
	first := activityGit(t, work, nil, "rev-parse", "HEAD")

	// Second commit: a changed ref.txt (ref-selection proof), a symlink, a
	// gitlink, and a path with a colon and a newline in its name.
	grepWrite(t, work, "ref.txt", "hello version two\n")
	if err := os.Symlink("cmd/app/main.go", filepath.Join(work, "link.txt")); err != nil {
		t.Fatal(err)
	}
	activityGit(t, work, nil, "update-index", "--add", "--cacheinfo", "160000,"+first+",sub")
	weird := "weird:name\nfile.txt"
	grepWrite(t, work, weird, "needle here\n")
	activityGit(t, work, nil, "-c", "core.autocrlf=false", "add", "-A")
	activityGit(t, work, nil, "commit", "-qm", "second")
	second := activityGit(t, work, nil, "rev-parse", "HEAD")

	bare := filepath.Join(src, "serving.git")
	activityGit(t, src, nil, "clone", "--bare", "-q", work, bare)

	l := NewLayer()
	return &grepFixture{
		Layer:  l,
		Repo:   &LocalRepo{Path: bare},
		First:  first,
		Second: second,
		Weird:  weird,
	}
}

// gitBlobRaw reads a blob's exact bytes (no trimming) from the serving copy.
func gitBlobRaw(t *testing.T, gitDir, spec string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+gitDir, "cat-file", "blob", spec).Output()
	if err != nil {
		t.Fatalf("cat-file blob %s: %v", spec, err)
	}
	return string(out)
}

func (f *grepFixture) grep(t *testing.T, rev string, q GrepQuery) *GrepResult {
	t.Helper()
	res, err := f.Layer.Grep(context.Background(), f.Repo, rev, q)
	if err != nil {
		t.Fatalf("Grep(%q): %v", q.Pattern, err)
	}
	return res
}

func fileByPath(res *GrepResult, path string) *GrepFile {
	for i := range res.Files {
		if res.Files[i].Path == path {
			return &res.Files[i]
		}
	}
	return nil
}

func TestGrepMatchesShapeAndBinarySkip(t *testing.T) {
	f := newGrepFixture(t)
	res := f.grep(t, f.Second, GrepQuery{Pattern: "hello"})

	if res.Files == nil {
		t.Fatal("Files is nil; the API contract requires an empty slice, never null")
	}
	// The tab-indented call in main.go is a precise line/column assertion.
	main := fileByPath(res, "cmd/app/main.go")
	if main == nil || len(main.Matches) != 1 {
		t.Fatalf("cmd/app/main.go = %+v, want one match", main)
	}
	if got := main.Matches[0]; got.Line != 4 || got.Column != 2 || got.Text != "\thello()" {
		t.Fatalf("main.go match = %+v, want line 4 col 2 text %q", got, "\thello()")
	}
	// -i made "HelloUtil" match; the word is intact in the text.
	util := fileByPath(res, "pkg/util.go")
	if util == nil || len(util.Matches) != 2 {
		t.Fatalf("pkg/util.go = %+v, want two matches", util)
	}
	// -I skipped the binary blob even though it contains the needle.
	if got := fileByPath(res, "bin.dat"); got != nil {
		t.Fatalf("binary file was searched: %+v", got)
	}
	// CRLF and a no-trailing-newline last line both parse.
	// The parser drops the CR of a CRLF terminator (the fixture pins
	// core.autocrlf=false, so the blob really is CRLF on every host).
	if crlf := fileByPath(res, "crlf.txt"); crlf == nil || crlf.Matches[0].Text != "hello crlf" {
		t.Fatalf("crlf match = %+v", crlf)
	}
	if nn := fileByPath(res, "nonewline.txt"); nn == nil || nn.Matches[0].Text != "no newline hello" {
		t.Fatalf("no-newline match = %+v", nn)
	}
	if res.Matches != 8 || res.FileCount != 7 {
		t.Fatalf("stats = %+v, want 8 matches in 7 files", res.GrepStats)
	}
	if res.Truncated || res.TimedOut {
		t.Fatalf("unexpected cap state: %+v", res.GrepStats)
	}
}

func TestGrepNoMatchesIsEmptySuccess(t *testing.T) {
	f := newGrepFixture(t)
	res := f.grep(t, f.Second, GrepQuery{Pattern: "zzz-no-such-needle"})
	if res.Matches != 0 || len(res.Files) != 0 || res.Files == nil {
		t.Fatalf("no-match result = %+v", res)
	}
	if res.Truncated || res.TimedOut {
		t.Fatalf("no-match state = %+v", res.GrepStats)
	}
}

func TestGrepUnicodeAndASCIICaseFolding(t *testing.T) {
	f := newGrepFixture(t)
	// Exact non-ASCII bytes match.
	res := f.grep(t, f.Second, GrepQuery{Pattern: "Привет"})
	if res.Matches != 1 || fileByPath(res, "uni.txt") == nil {
		t.Fatalf("unicode literal search = %+v", res)
	}
	// LC_ALL=C folds ASCII only: a lowercase Cyrillic needle must NOT match
	// the capitalised line, and café must not match CAFÉ.
	for _, needle := range []string{"привет", "café"} {
		res = f.grep(t, f.Second, GrepQuery{Pattern: needle})
		if res.Matches != 0 {
			t.Fatalf("%q matched under C-locale folding: %+v", needle, res.Files)
		}
	}
	// Exact bytes with case sensitivity do match the capitalised form.
	res = f.grep(t, f.Second, GrepQuery{Pattern: "CAFÉ", CaseSensitive: true})
	if res.Matches != 1 {
		t.Fatalf("case-sensitive CAFÉ = %+v", res)
	}
}

func TestGrepFixedRegexAndWord(t *testing.T) {
	f := newGrepFixture(t)
	// Fixed: the dot is a literal, so nothing matches.
	if res := f.grep(t, f.Second, GrepQuery{Pattern: "h.llo", CaseSensitive: true}); res.Matches != 0 {
		t.Fatalf("fixed string matched a regex: %+v", res.Files)
	}
	// POSIX ERE: the dot is a wildcard.
	if res := f.grep(t, f.Second, GrepQuery{Pattern: "h.llo", Regex: true, CaseSensitive: true}); res.Matches == 0 {
		t.Fatal("regex search found nothing")
	}
	// -w: "hello" must not match inside "HelloUtil".
	res := f.grep(t, f.Second, GrepQuery{Pattern: "hello", Word: true, CaseSensitive: true})
	if fileByPath(res, "pkg/util.go") != nil || fileByPath(res, "cmd/app/main.go") == nil {
		t.Fatalf("word search = %+v", res.Files)
	}
}

func TestGrepPathspecFiltering(t *testing.T) {
	f := newGrepFixture(t)
	res := f.grep(t, f.Second, GrepQuery{Pattern: "hello", Paths: []string{"*.go"}})
	if len(res.Files) != 2 {
		t.Fatalf("*.go files = %+v, want main.go + util.go", res.Files)
	}
	for _, file := range res.Files {
		if !strings.HasSuffix(file.Path, ".go") {
			t.Fatalf("pathspec leaked %q", file.Path)
		}
	}
	res = f.grep(t, f.Second, GrepQuery{Pattern: "hello", Paths: []string{"cmd/"}})
	if len(res.Files) != 1 || res.Files[0].Path != "cmd/app/main.go" {
		t.Fatalf("cmd/ = %+v", res.Files)
	}
	res = f.grep(t, f.Second, GrepQuery{Pattern: "needle"})
	// The colon+newline path parses out of -z output, and long text is clipped
	// around the column with ellipses on both sides.
	if fileByPath(res, f.Weird) == nil {
		t.Fatalf("weird path missing from %+v", res.Files)
	}
	long := fileByPath(res, "long.txt")
	if long == nil {
		t.Fatal("long.txt missing")
	}
	text := long.Matches[0].Text
	if len(text) > GrepDefaultMaxText {
		t.Fatalf("text is %d bytes, cap is %d", len(text), GrepDefaultMaxText)
	}
	// The match sits near the end of the line, so the window opens with an
	// ellipsis and needs none at the end; the needle is still visible.
	if !strings.Contains(text, "needle") || !strings.HasPrefix(text, "…") {
		t.Fatalf("clipped text = %q", text[:min(80, len(text))])
	}
}

func TestGrepRefSelection(t *testing.T) {
	f := newGrepFixture(t)
	res := f.grep(t, f.First, GrepQuery{Pattern: "version two"})
	if res.Matches != 0 {
		t.Fatalf("first commit sees the second: %+v", res.Files)
	}
	res = f.grep(t, f.First, GrepQuery{Pattern: "version one"})
	if res.Matches != 1 {
		t.Fatalf("first commit = %+v", res.Files)
	}
	res = f.grep(t, f.Second, GrepQuery{Pattern: "version one"})
	if res.Matches != 0 {
		t.Fatalf("second commit still sees version one: %+v", res.Files)
	}
	// A ref NAME is not a revision: the primitive only accepts resolved ids.
	if _, err := f.Layer.Grep(context.Background(), f.Repo, "main", GrepQuery{Pattern: "x"}); err == nil {
		t.Fatal("Grep accepted a ref name")
	}
}

func TestGrepCaps(t *testing.T) {
	f := newGrepFixture(t)
	// Total match cap: stop at the limit and report truncation.
	res := f.grep(t, f.Second, GrepQuery{Pattern: "hello", Limit: 3})
	if res.Matches != 3 || !res.Truncated {
		t.Fatalf("limit cap = %+v", res.GrepStats)
	}
	// Per-file cap: ask for 1; util.go has two matches, so its extra record is
	// what makes the per-file truncation observable.
	res = f.grep(t, f.Second, GrepQuery{Pattern: "hello", PerFile: 1, Limit: 200})
	util := fileByPath(res, "pkg/util.go")
	if util == nil || len(util.Matches) != 1 || !util.Truncated {
		t.Fatalf("per-file cap = %+v", util)
	}
	if res.Truncated {
		t.Fatal("a per-file cap alone must not mark the whole repo truncated")
	}
	// Byte cap: stops mid-stream and keeps every complete record before it.
	res = f.grep(t, f.Second, GrepQuery{Pattern: "hello", MaxBytes: 120})
	if !res.Truncated || res.Matches == 0 || res.Matches >= 8 {
		t.Fatalf("byte cap = %+v", res.GrepStats)
	}
	if res.Bytes < 120 {
		t.Fatalf("bytes seen = %d, want at least the cap", res.Bytes)
	}
	// Text cap is a projection, not a completeness cap.
	res = f.grep(t, f.Second, GrepQuery{Pattern: "hello", MaxText: 12})
	if res.Truncated {
		t.Fatal("text clipping must not mark the result truncated")
	}
	for _, file := range res.Files {
		for _, m := range file.Matches {
			if len(m.Text) > 12 {
				t.Fatalf("text %q is over the 12-byte cap", m.Text)
			}
		}
	}
}

func TestGrepTimeout(t *testing.T) {
	dir := t.TempDir()
	l := NewLayer()
	l.Binary = slowGit(t, dir, "")
	start := time.Now()
	res, err := l.GrepAt(context.Background(), dir, strings.Repeat("a", 40), GrepQuery{
		Pattern: "x",
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("timeout returned an error: %v", err)
	}
	if !res.TimedOut || res.Truncated {
		t.Fatalf("timeout stats = %+v", res.GrepStats)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %v; the child was not killed", elapsed)
	}
}

func TestGrepCancellation(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	l := NewLayer()
	l.Binary = slowGit(t, dir, marker)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.GrepAt(ctx, dir, strings.Repeat("a", 40), GrepQuery{Pattern: "x", Timeout: 30 * time.Second})
		done <- err
	}()
	waitForFile(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the search")
	}
}

func TestGrepCancelWhileWaitingForScanSlot(t *testing.T) {
	f := newGrepFixture(t)
	f.Layer.ScanPool = NewPool(1)
	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		_ = f.Layer.ScanPool.Run(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Layer.Grep(ctx, f.Repo, f.Second, GrepQuery{Pattern: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel error = %v", err)
	}
}

func TestGrepNeverUsesTheControlPlanePool(t *testing.T) {
	f := newGrepFixture(t)
	f.Layer.Pool = NewPool(1) // the control-plane pool, saturated below
	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		_ = f.Layer.Pool.Run(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := f.Layer.Grep(ctx, f.Repo, f.Second, GrepQuery{Pattern: "hello"})
	if err != nil {
		t.Fatalf("Grep blocked on the control-plane pool: %v", err)
	}
	if res.Matches == 0 {
		t.Fatal("no matches")
	}
}

func TestGrepExitCodeClassification(t *testing.T) {
	dir := t.TempDir()
	rev := strings.Repeat("a", 40)

	// exit 0 with empty output is a successful empty answer.
	l := NewLayer()
	l.Binary = stubGit(t, dir, "exit0", "exit 0\n")
	res, err := l.GrepAt(context.Background(), dir, rev, GrepQuery{Pattern: "x"})
	if err != nil || res.Matches != 0 {
		t.Fatalf("exit 0 = %+v, %v", res, err)
	}

	// exit 1 is "no matches", never an error.
	l.Binary = stubGit(t, dir, "exit1", "exit 1\n")
	res, err = l.GrepAt(context.Background(), dir, rev, GrepQuery{Pattern: "x"})
	if err != nil || res.Matches != 0 {
		t.Fatalf("exit 1 = %+v, %v", res, err)
	}

	// A bad pattern at run time is the distinct pattern error, not "empty".
	l.Binary = stubGit(t, dir, "badexit", "echo \"fatal: -e option, 'a{2,1}': Invalid content\" >&2\nexit 2\n")
	_, err = l.GrepAt(context.Background(), dir, rev, GrepQuery{Pattern: "a+", Regex: true})
	if !errors.Is(err, ErrGrepPattern) {
		t.Fatalf("bad pattern error = %v", err)
	}
	if !strings.Contains(err.Error(), "Invalid content") {
		t.Fatalf("pattern error lost git's detail: %v", err)
	}

	// Any other failed exit is a subprocess failure (the service maps it 503).
	l.Binary = stubGit(t, dir, "otherexit", "echo 'fatal: not a git repository' >&2\nexit 128\n")
	_, err = l.GrepAt(context.Background(), dir, rev, GrepQuery{Pattern: "x", Regex: true})
	var ge *GitError
	if !errors.As(err, &ge) || ge.Kind != GitErrSubprocess {
		t.Fatalf("unexpected failure error = %v", err)
	}

	// A missing binary is the same subprocess class, not a panic.
	l.Binary = filepath.Join(dir, "no-such-git")
	_, err = l.GrepAt(context.Background(), dir, rev, GrepQuery{Pattern: "x"})
	if !errors.As(err, &ge) || ge.Kind != GitErrSubprocess {
		t.Fatalf("missing binary error = %v", err)
	}
}

func TestGrepValidation(t *testing.T) {
	f := newGrepFixture(t)
	ctx := context.Background()
	long := strings.Repeat("p", GrepMaxPatternBytes+1)
	zero := strings.Repeat("0", 40)

	cases := []struct {
		name string
		rev  string
		q    GrepQuery
	}{
		{"bad revision", "main", GrepQuery{Pattern: "x"}},
		{"zero revision", zero, GrepQuery{Pattern: "x"}},
		{"empty pattern", f.Second, GrepQuery{Pattern: ""}},
		{"long pattern", f.Second, GrepQuery{Pattern: long}},
		{"newline pattern", f.Second, GrepQuery{Pattern: "a\nb"}},
		{"cr pattern", f.Second, GrepQuery{Pattern: "a\rb"}},
		{"nul pattern", f.Second, GrepQuery{Pattern: "a\x00b"}},
		{"invalid regex", f.Second, GrepQuery{Pattern: "(", Regex: true}},
		{"too many paths", f.Second, GrepQuery{Pattern: "x", Paths: make([]string, GrepMaxPathPatterns+1)}},
		{"empty path", f.Second, GrepQuery{Pattern: "x", Paths: []string{""}}},
		{"long path", f.Second, GrepQuery{Pattern: "x", Paths: []string{strings.Repeat("a", GrepMaxPathBytes+1)}}},
		{"newline path", f.Second, GrepQuery{Pattern: "x", Paths: []string{"a\nb"}}},
		{"magic colon", f.Second, GrepQuery{Pattern: "x", Paths: []string{":(glob)*.go"}}},
		{"magic negate", f.Second, GrepQuery{Pattern: "x", Paths: []string{"!*.go"}}},
		{"magic caret", f.Second, GrepQuery{Pattern: "x", Paths: []string{"^*.go"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Layer.Grep(ctx, f.Repo, tc.rev, tc.q)
			if err == nil {
				t.Fatal("validation accepted the input")
			}
			if tc.name == "invalid regex" && !errors.Is(err, ErrGrepPattern) {
				t.Fatalf("invalid regex error = %v, want ErrGrepPattern", err)
			}
		})
	}

	if _, err := f.Layer.Grep(ctx, nil, f.Second, GrepQuery{Pattern: "x"}); err == nil {
		t.Fatal("nil repo accepted")
	}
	if _, err := f.Layer.GrepAt(ctx, "", f.Second, GrepQuery{Pattern: "x"}); err == nil {
		t.Fatal("empty dir accepted")
	}
}

func TestGrepQueryNormalization(t *testing.T) {
	cases := []struct {
		in   GrepQuery
		want GrepQuery
	}{
		{GrepQuery{}, GrepQuery{PerFile: GrepDefaultPerFile, Limit: GrepDefaultLimit,
			MaxBytes: GrepDefaultMaxBytes, MaxText: GrepDefaultMaxText, Timeout: GrepDefaultTimeout}},
		{GrepQuery{PerFile: 9999, Limit: 9999, MaxBytes: 1 << 30, MaxText: 1 << 30, Timeout: time.Hour},
			GrepQuery{PerFile: GrepMaxPerFile, Limit: GrepMaxLimit, MaxBytes: GrepMaxOutputBytes,
				MaxText: GrepMaxTextBytes, Timeout: GrepMaxTimeout}},
		{GrepQuery{PerFile: 3, Limit: 7, MaxBytes: 99, MaxText: 1, Timeout: time.Second},
			GrepQuery{PerFile: 3, Limit: 7, MaxBytes: 99, MaxText: GrepMinTextBytes, Timeout: time.Second}},
	}
	for _, tc := range cases {
		if got := tc.in.normalized(); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("normalized(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if s := (GrepQuery{Regex: true, Word: true, Paths: []string{"a"}}).String(); !strings.Contains(s, "regex=true") {
		t.Fatalf("String() = %q", s)
	}
}

func TestGrepArgvPinned(t *testing.T) {
	q := GrepQuery{Pattern: "-weird pattern", Regex: true, Word: true, Paths: []string{"a b", "*.go"}}.normalized()
	got := strings.Join(grepArgv(strings.Repeat("d", 40), q), "\x00")
	want := strings.Join([]string{
		"grep", "--no-color", "-n", "--column", "-I", "-z",
		"--no-textconv", "--no-recurse-submodules",
		"-i", "-w", "-E", "-m", "6", "-e", "-weird pattern", strings.Repeat("d", 40), "--",
		"a b", "*.go",
	}, "\x00")
	if got != want {
		t.Fatalf("argv =\n%s\nwant\n%s", got, want)
	}
	// The pathspec separator always rides the argv, even with no paths, and a
	// case-sensitive fixed search carries neither -i nor -E.
	got = strings.Join(grepArgv(strings.Repeat("d", 40), GrepQuery{Pattern: "x", CaseSensitive: true}.normalized()), "\x00")
	if !strings.HasSuffix(got, "x\x00"+strings.Repeat("d", 40)+"\x00--") {
		t.Fatalf("fixed argv = %s", got)
	}
	if strings.Contains(got, "-i\x00") || strings.Contains(got, "-E\x00") {
		t.Fatalf("fixed argv carries a flag it should not: %s", got)
	}
}

func TestGrepParserStreaming(t *testing.T) {
	rev := strings.Repeat("a", 40)
	q := GrepQuery{Pattern: "x"}.normalized()
	p := newGrepParser(rev, q, func() {})

	// Three complete records and a partial fourth, all fed one byte at a time:
	// the parser must never split on NUL, and a NUL inside the text is content.
	stream := rev + ":a.txt\x001\x002\x00hello world\n" +
		rev + ":" + "weird:name\nfile.txt\x003\x004\x00second\n" +
		"other:tricky:name\x005\x006\x00a\x00b\n" +
		rev + ":partial"
	for i := 0; i < len(stream); i++ {
		if _, err := p.Write([]byte{stream[i]}); err != nil {
			t.Fatal(err)
		}
	}
	res := p.result()
	if res.Matches != 3 || len(res.Files) != 3 {
		t.Fatalf("streamed result = %+v", res)
	}
	if res.Files[0].Path != "a.txt" || res.Files[0].Matches[0].Line != 1 || res.Files[0].Matches[0].Column != 2 {
		t.Fatalf("first record = %+v", res.Files[0])
	}
	if res.Files[1].Path != "weird:name\nfile.txt" || res.Files[1].Matches[0].Line != 3 {
		t.Fatalf("colon/newline path record = %+v", res.Files[1])
	}
	if res.Files[2].Path != "tricky:name" || res.Files[2].Matches[0].Text != "a\x00b" {
		t.Fatalf("NUL-in-text record = %+v", res.Files[2])
	}
	if res.Bytes != len(stream) {
		t.Fatalf("bytes = %d, want %d", res.Bytes, len(stream))
	}
}

func TestGrepParserStripsCRLFTerminator(t *testing.T) {
	rev := strings.Repeat("a", 40)
	q := GrepQuery{Pattern: "x"}.normalized()
	p := newGrepParser(rev, q, func() {})

	stream := rev + ":a.txt\x001\x001\x00hello crlf\r\n" +
		rev + ":b.txt\x001\x001\x00two\r\r\n" +
		rev + ":c.txt\x001\x001\x00lf only\n"
	if _, err := p.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	res := p.result()
	if len(res.Files) != 3 {
		t.Fatalf("files = %+v", res.Files)
	}
	if got := res.Files[0].Matches[0].Text; got != "hello crlf" {
		t.Fatalf("CRLF text = %q, want %q", got, "hello crlf")
	}
	// Exactly ONE trailing CR is dropped: a line that genuinely ends with a
	// CR character keeps it.
	if got := res.Files[1].Matches[0].Text; got != "two\r" {
		t.Fatalf("double-CR text = %q, want %q", got, "two\r")
	}
	if got := res.Files[2].Matches[0].Text; got != "lf only" {
		t.Fatalf("LF text = %q", got)
	}
}

func TestGrepCRLFFixtureBytesAndMatchText(t *testing.T) {
	f := newGrepFixture(t)
	// The fixture pins core.autocrlf=false, so the stored blob keeps its CRLF
	// bytes even on a host whose global config converts line endings.
	blob := gitBlobRaw(t, f.Repo.Path, f.Second+":crlf.txt")
	if blob != "hello crlf\r\n" {
		t.Fatalf("stored blob = %q, want the literal CRLF bytes", blob)
	}
	res := f.grep(t, f.Second, GrepQuery{Pattern: "hello crlf"})
	crlf := fileByPath(res, "crlf.txt")
	if crlf == nil || len(crlf.Matches) != 1 {
		t.Fatalf("crlf matches = %+v", crlf)
	}
	if got := crlf.Matches[0].Text; got != "hello crlf" {
		t.Fatalf("match text = %q, want the CR stripped", got)
	}
}

func TestGrepParserCapsAndOddFields(t *testing.T) {
	rev := strings.Repeat("a", 40)
	q := GrepQuery{Pattern: "x", PerFile: 1, Limit: 2}.normalized()
	cancelled := false
	p := newGrepParser(rev, q, func() { cancelled = true })

	// A huge line number exercises the overflow guard; the same file's second
	// record is the per-file extra, and the third file's record hits the total
	// cap.
	stream := rev + ":one.txt\x00999999999999\x001\x00first\n" +
		"one.txt\x00999999999999\x001\x00extra\n" +
		rev + ":two.txt\x001\x001\x00second\n" +
		rev + ":three.txt\x001\x001\x00third\n"
	if _, err := p.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	res := p.result()
	if len(res.Files) != 2 || !res.Files[0].Truncated || res.Matches != 2 || !res.Truncated {
		t.Fatalf("caps result = %+v", res)
	}
	if !cancelled {
		t.Fatal("total cap did not cancel the scan")
	}
	if res.Files[0].Matches[0].Line != 1<<30 {
		t.Fatalf("overflow guard line = %d", res.Files[0].Matches[0].Line)
	}
}

func TestGrepParserHelpers(t *testing.T) {
	// A stderr without a trailing newline is returned whole.
	if got := firstLine("fatal: boom"); got != "fatal: boom" {
		t.Fatalf("firstLine = %q", got)
	}
	if looksLikePatternError("fatal: not a git repository") {
		t.Fatal("unrelated stderr classified as a pattern error")
	}
	if atoiBytes([]byte("12x")) != 12 || atoiBytes([]byte("x")) != 0 {
		t.Fatal("atoiBytes accepted a non-numeric field")
	}
	// A zero-value parser still returns a non-nil Files slice (never null).
	if res := (&grepParser{}).result(); res.Files == nil {
		t.Fatal("zero parser returned a nil Files slice")
	}
	// A line number beyond the 30-bit guard clamps instead of overflowing.
	q := GrepQuery{Pattern: "x"}.normalized()
	p := newGrepParser(strings.Repeat("a", 40), q, func() {})
	_, _ = p.Write([]byte(strings.Repeat("a", 40) + ":f\x00" + strings.Repeat("9", 40) + "\x00x\x001\x00t\n"))
	if got := p.result(); len(got.Files) == 0 || got.Files[0].Matches[0].Line != 1<<30 {
		t.Fatalf("overflow guard = %+v", got.Files)
	}
}

func TestGrepParserByteCap(t *testing.T) {
	rev := strings.Repeat("a", 40)
	q := GrepQuery{Pattern: "x", MaxBytes: 10}.normalized()
	cancelled := false
	p := newGrepParser(rev, q, func() { cancelled = true })
	if _, err := p.Write([]byte(rev + ":a.txt\x001\x001\x00hello\n")); err != nil {
		t.Fatal(err)
	}
	res := p.result()
	if !res.Truncated || !cancelled || res.Matches != 0 || res.Bytes <= 10 {
		t.Fatalf("byte cap = %+v cancelled=%v", res.GrepStats, cancelled)
	}
	// A write after the cap drains without parsing or panicking.
	if _, err := p.Write([]byte("trailing")); err != nil {
		t.Fatal(err)
	}

	// A chunk that lands exactly on the cap leaves the parser uncapped; the
	// NEXT chunk then takes the room<=0 arm and cancels without parsing.
	q = GrepQuery{Pattern: "x", MaxBytes: 10}.normalized()
	p = newGrepParser(rev, q, func() {})
	if _, err := p.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if res := p.result(); res.Truncated {
		t.Fatalf("exactly-at-cap chunk capped early: %+v", res.GrepStats)
	}
	if _, err := p.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if res := p.result(); res.Matches != 0 || !res.Truncated {
		t.Fatalf("room<=0 chunk = %+v", res.GrepStats)
	}
}

func TestClipMatchText(t *testing.T) {
	short := "abc hello"
	if got := clipMatchText(short, 5, 100); got != short {
		t.Fatalf("short text changed: %q", got)
	}
	long := "head-" + strings.Repeat("x", 5000) + "MATCH" + strings.Repeat("y", 5000) + "-tail"
	got := clipMatchText(long, 5011, 64)
	if len(got) > 64 {
		t.Fatalf("clipped length = %d", len(got))
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "MATCH") {
		t.Fatalf("clipped text = %q", got)
	}

	uni := strings.Repeat("é", 4000)
	got = clipMatchText(uni, 2000, 101)
	if !utf8.ValidString(got) {
		t.Fatalf("clip split a rune: %q", got)
	}
	// An odd byte column parks the window on a continuation byte; the clip
	// must advance to the next rune start.
	got = clipMatchText(uni, 1999, 101)
	if !utf8.ValidString(got) {
		t.Fatalf("continuation-byte window split a rune: %q", got)
	}
	// A window at the very start keeps no leading ellipsis; the tail case has
	// no trailing one.
	if got := clipMatchText(strings.Repeat("z", 200), 1, 20); strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("head window = %q", got)
	}
	if got := clipMatchText(strings.Repeat("z", 200), -50, 20); !utf8.ValidString(got) {
		t.Fatalf("negative column window = %q", got)
	}
	// A column past the end clamps to the text length (a match column always
	// comes from git, but the helper is total).
	if got := clipMatchText(strings.Repeat("z", 200), 500, 20); !strings.HasPrefix(got, "…") {
		t.Fatalf("column-past-end window = %q", got)
	}
	if got := clipMatchText(strings.Repeat("z", 200), 200, 20); !strings.HasPrefix(got, "…") || strings.HasSuffix(got, "…") {
		t.Fatalf("tail window = %q", got)
	}
	// Degenerate sizes and positions are still bounded and valid.
	if got := clipMatchText("abcdef", 0, 4); got != "abcd" {
		t.Fatalf("tiny cap = %q", got)
	}
	if got := clipMatchText("abcdef", -5, 500); got != "abcdef" {
		t.Fatalf("negative column = %q", got)
	}
	if got := clipMatchText(strings.Repeat("é", 10), 1, 20); got != strings.Repeat("é", 10) {
		t.Fatalf("rune-boundary short = %q", got)
	}
	if got := truncAtRune("abcdef", -1); got != "" {
		t.Fatalf("truncAtRune(-1) = %q", got)
	}
	if got := truncAtRune("abcdef", 100); got != "abcdef" {
		t.Fatalf("truncAtRune(large) = %q", got)
	}
	if got := truncAtRune("aé", 2); got != "a" {
		t.Fatalf("truncAtRune split a rune: %q", got)
	}
}

func TestGrepLayerWithoutScanPool(t *testing.T) {
	f := newGrepFixture(t)
	bare := &Layer{Binary: "git"} // no ScanPool: the package default must carry it
	res, err := bare.Grep(context.Background(), f.Repo, f.Second, GrepQuery{Pattern: "hello"})
	if err != nil {
		t.Fatalf("Grep on a bare Layer: %v", err)
	}
	if res.Matches == 0 {
		t.Fatal("no matches")
	}
}

// --- stubs ------------------------------------------------------------------

// stubGit writes an executable script that ignores its argv. Each stub gets
// its own directory so parallel tests cannot collide.
func stubGit(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// slowGit writes a `git` that (optionally) touches marker, then blocks. The
// test never sleeps for correctness: the cancellation test gates on the marker
// the fixture itself creates.
func slowGit(t *testing.T, root, marker string) string {
	body := "exec sleep 30\n"
	if marker != "" {
		body = ": > '" + marker + "'\n" + body
	}
	return stubGit(t, root, "slow", body)
}

// waitForFile polls a marker the fixture owns (never a bare sleep as proof).
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture never created %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
