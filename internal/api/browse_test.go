package api

// browse_test.go — the §16 browsing reads over a REAL git serving copy (the
// bind_wal_test fixture discipline): blame attribution must equal what git
// says, including unchanged trailing lines, CRLF files and merge commits; the
// branch overview's ahead/behind is one batched for-each-ref; the compare
// patch must be the patch `git diff` itself produces.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

func browseView(t *testing.T, bare *git.LocalRepo) (*walView, git.RepoId) {
	t.Helper()
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: bare}, rev: 1}
	f := newEngineFixture(t, eng)
	return f.view(), repoID()
}

// browseRepo builds a bare serving copy with a deliberately awkward history:
// an unchanged trailing line, a CRLF file, and a merge whose conflict
// resolution is attributable to the merge commit itself.
type browseRepo struct {
	bare   *git.LocalRepo
	merge  string
	first  string
	second string
}

func newBrowseRepo(t *testing.T) browseRepo {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	barePath := filepath.Join(dir, "walgit.git")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, dir, "init", "--bare", "-b", "main", barePath)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	// A CRLF fixture must reach the blob verbatim; the runner's global
	// autocrlf would normalize it away.
	runGit(t, work, "config", "core.autocrlf", "false")

	// First commit: three lines; the trailing line will stay unchanged.
	if err := os.WriteFile(filepath.Join(work, "story.txt"), []byte("alpha\nbravo\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "story.txt")
	runGit(t, work, "commit", "-m", "seed")
	first := runGit(t, work, "rev-parse", "HEAD")

	// Second commit changes the first two lines, leaves "tail" alone.
	if err := os.WriteFile(filepath.Join(work, "story.txt"), []byte("alpha2\nbravo2\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "story.txt")
	runGit(t, work, "commit", "-m", "change head")
	second := runGit(t, work, "rev-parse", "HEAD")

	// CRLF file: git stores CRLF verbatim; blame text must keep it.
	if err := os.WriteFile(filepath.Join(work, "crlf.txt"), []byte("one\r\ntwo\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "crlf.txt")
	runGit(t, work, "commit", "-m", "crlf")

	// Merge with a conflict resolution line that exists in neither parent:
	// that line IS attributable to the merge commit.
	runGit(t, work, "checkout", "-q", "-b", "left", first)
	if err := os.WriteFile(filepath.Join(work, "story.txt"), []byte("LEFT\nbravo\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "commit", "-am", "left")
	runGit(t, work, "checkout", "-q", "main")
	runGit(t, work, "checkout", "-q", "-b", "right", "HEAD~1")
	if err := os.WriteFile(filepath.Join(work, "story.txt"), []byte("RIGHT\nbravo\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "commit", "-am", "right")
	runGit(t, work, "checkout", "-q", "main")
	// A conflicted merge exits non-zero by design; the conflict is the point.
	cmd := exec.Command("git", "merge", "--no-ff", "-m", "merge", "left")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	_ = cmd.Run()
	if err := os.WriteFile(filepath.Join(work, "story.txt"), []byte("MERGED\nbravo2\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "story.txt")
	runGit(t, work, "commit", "-m", "merge resolved")
	merge := runGit(t, work, "rev-parse", "HEAD")

	// A branch for the ahead/behind surface.
	runGit(t, work, "branch", "feature", second)

	// Binary and oversize guards.
	if err := os.WriteFile(filepath.Join(work, "bin.dat"), []byte("ok\x00nul\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 2<<20+9)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(filepath.Join(work, "big.dat"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "bin.dat", "big.dat")
	runGit(t, work, "commit", "-m", "guards")

	runGit(t, work, "push", barePath, "main", "feature")
	runGit(t, barePath, "pack-refs", "--all")
	bare := &git.LocalRepo{Root: dir, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: barePath}
	return browseRepo{bare: bare, merge: merge, first: first, second: second}
}

// --- parser -------------------------------------------------------------------

func TestParseBlameLinePorcelain(t *testing.T) {
	merge := strings.Repeat("c", 40)
	one := strings.Repeat("1", 40)
	two := strings.Repeat("2", 40)
	stream := strings.Join([]string{
		one + " 1 1 1", "author Ada", "author-mail <ada@example.com>", "author-time 1700000000", "author-tz +0000",
		"committer Ada", "committer-mail <ada@example.com>", "committer-time 1700000000", "committer-tz +0000",
		"summary first", "filename story.txt", "\talpha",
		one + " 2 2 1", "author Ada", "author-time 1700000000", "\tbravo",
		two + " 3 3 1", "author Bob", "author-time 1700003600", "\ttail",
		merge + " 4 4 1", "author Merge Bot", "author-time 1700007200", "\tMERGED",
		"", // trailing newline artifact
	}, "\n")
	lines := parseBlameLinePorcelain([]byte(stream))
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4: %+v", len(lines), lines)
	}
	if lines[0].SHA != one || lines[0].Author != "Ada" || lines[0].LineNo != 1 || lines[0].Text != "alpha" {
		t.Fatalf("line 0 = %+v", lines[0])
	}
	// The unchanged trailing line keeps its own attribution (sha two, not the
	// previous line's group) — the --line-porcelain guarantee.
	if lines[2].SHA != two || lines[2].Text != "tail" || lines[2].LineNo != 3 {
		t.Fatalf("trailing line = %+v", lines[2])
	}
	if lines[3].SHA != merge || lines[3].Author != "Merge Bot" {
		t.Fatalf("merge line = %+v", lines[3])
	}
	if got := lines[3].AuthorTime; !got.Equal(time.Unix(1700007200, 0).UTC()) {
		t.Fatalf("author time = %v", got)
	}
	// CRLF: the carriage return is content, not a parse error.
	crlf := one + " 1 1 1\nauthor Ada\nauthor-time 1\n\tsay hi\r\n" + one + " 2 2 1\nauthor Ada\nauthor-time 1\n\tdone\r\n"
	cl := parseBlameLinePorcelain([]byte(crlf))
	if len(cl) != 2 || cl[0].Text != "say hi\r" || cl[1].Text != "done\r" || cl[1].LineNo != 2 {
		t.Fatalf("crlf lines = %+v", cl)
	}
	// Malformed headers and header lines outside a record are skipped, never
	// fatal (an interrupted stream must not 500 a page).
	bad := "not-a-sha 1 1\norphan author X\n" + one + " x y\n" + one + " 1 1 1\nauthor Ok\n\tfine"
	bl := parseBlameLinePorcelain([]byte(bad))
	if len(bl) != 1 || bl[0].Text != "fine" || bl[0].LineNo != 1 {
		t.Fatalf("bad stream = %+v", bl)
	}
}

func TestParseBlameHeaderAndHex(t *testing.T) {
	sha := strings.Repeat("a", 40)
	if _, _, final, ok := parseBlameHeader(sha + " 1 7 3"); !ok || final != 7 {
		t.Fatalf("header = ok %v final %d", ok, final)
	}
	if _, _, _, ok := parseBlameHeader(sha + " 1 7 3 4"); ok {
		t.Fatal("five fields must not parse")
	}
	if _, _, _, ok := parseBlameHeader(sha + " x 7"); ok {
		t.Fatal("non-numeric line must not parse")
	}
	if _, _, _, ok := parseBlameHeader(sha + " 1 7 z"); ok {
		t.Fatal("non-numeric count must not parse")
	}
	if isHexOID("abc") || isHexOID(strings.Repeat("g", 40)) {
		t.Fatal("isHexOID admitted a non-oid")
	}
	if !isHexOID(strings.Repeat("A", 64)) {
		t.Fatal("isHexOID must accept uppercase sha256")
	}
}

func TestBlameBinarySniff(t *testing.T) {
	if blameBinary([]BlameLine{{Text: "ok"}}) {
		t.Fatal("plain text flagged binary")
	}
	if !blameBinary([]BlameLine{{Text: "ok\x00nul"}}) {
		t.Fatal("NUL not flagged")
	}
	// NUL beyond the first 8 KiB is not git's heuristic.
	lines := []BlameLine{{Text: strings.Repeat("a", 9000) + "\x00"}}
	if blameBinary(lines) {
		t.Fatal("NUL past 8 KiB flagged")
	}
	huge := []BlameLine{{Text: strings.Repeat("a", 9000)}, {Text: "\x00"}}
	if blameBinary(huge) {
		t.Fatal("NUL past the accumulated 8 KiB window flagged")
	}
}

// --- real git ----------------------------------------------------------------

func TestBlameMatchesGit(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()
	res, err := v.Blame(ctx, id, fix.merge, "story.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.TooLarge || res.Binary {
		t.Fatalf("unexpected guard: %+v", res)
	}
	if len(res.Lines) != 3 {
		t.Fatalf("lines = %d: %+v", len(res.Lines), res.Lines)
	}
	if res.Lines[0].Text != "MERGED" || res.Lines[0].SHA != fix.merge {
		t.Fatalf("merge-attributed line = %+v", res.Lines[0])
	}
	if res.Lines[2].Text != "tail" || res.Lines[2].SHA != fix.first {
		t.Fatalf("unchanged trailing line = %+v", res.Lines[2])
	}

	// Independent oracle: grouped --porcelain carries the sha once per group;
	// a test-local parser expands it. Matching it proves the line-porcelain
	// parser's group independence.
	out := runGit(t, fix.bare.Path, "blame", "--porcelain", fix.merge, "--", "story.txt")
	want := parseGroupedBlame(out)
	if len(want) != len(res.Lines) {
		t.Fatalf("oracle %d lines vs %d", len(want), len(res.Lines))
	}
	for i := range want {
		if want[i].sha != res.Lines[i].SHA || want[i].text != res.Lines[i].Text || want[i].no != res.Lines[i].LineNo {
			t.Fatalf("line %d: oracle %+v vs %+v", i, want[i], res.Lines[i])
		}
	}

	// CRLF content survives verbatim.
	crlf, err := v.Blame(ctx, id, "main", "crlf.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(crlf.Lines) != 2 || crlf.Lines[0].Text != "one\r" || crlf.Lines[1].Text != "two\r" {
		t.Fatalf("crlf = %+v", crlf.Lines)
	}
}

type groupedLine struct {
	sha  string
	no   int
	text string
}

// parseGroupedBlame is the test's independent oracle for `--porcelain`:
// group headers carry the sha, content lines carry the final number.
func parseGroupedBlame(out string) []groupedLine {
	var lines []groupedLine
	sha := ""
	next := 0
	for _, raw := range strings.Split(out, "\n") {
		if strings.HasPrefix(raw, "\t") {
			next++
			lines = append(lines, groupedLine{sha: sha, no: next, text: raw[1:]})
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) >= 3 && isHexOID(fields[0]) {
			sha = fields[0]
		}
	}
	return lines
}

func TestBlameGuardsAndErrors(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()

	// Binary: content guards, language shared with the blob page.
	bin, err := v.Blame(ctx, id, "main", "bin.dat")
	if err != nil {
		t.Fatal(err)
	}
	if !bin.Binary || bin.TooLarge {
		t.Fatalf("binary result = %+v", bin)
	}
	if len(bin.Lines) == 0 {
		t.Fatal("binary blame should still carry its lines for the size line")
	}
	// Too large: refused before blame runs.
	big, err := v.Blame(ctx, id, "main", "big.dat")
	if err != nil {
		t.Fatal(err)
	}
	if !big.TooLarge || big.Binary || big.Lines != nil {
		t.Fatalf("too-large result = %+v", big)
	}

	if _, err := v.Blame(ctx, id, "main", "absent.txt"); err == nil || !strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Fatalf("absent path = %v", err)
	}
	if _, err := v.Blame(ctx, id, "", "story.txt"); err == nil {
		t.Fatal("empty rev must be refused")
	}
	if _, err := v.Blame(ctx, id, "-1", "story.txt"); err == nil {
		t.Fatal("option-shaped rev must be refused")
	}
	if _, err := v.Blame(ctx, id, "main", ""); err == nil {
		t.Fatal("empty path must be refused")
	}
	// Unborn repo: the empty-repo marker rides the error (the page guide).
	empty := &git.LocalRepo{Root: t.TempDir(), ID: id, Path: filepath.Join(t.TempDir(), "e.git")}
	runGit(t, filepath.Dir(empty.Path), "init", "--bare", empty.Path)
	emptyEng := &fakeEngine{obj: wal.ObjectAccess{Local: empty}, rev: 1, man: &proto.Manifest{Repo: "demo/walgit", ObjectFormat: "sha1"}}
	ev := newEngineFixture(t, emptyEng).view()
	if _, err := ev.Blame(ctx, id, strings.Repeat("a", 40), "story.txt"); err == nil || !strings.Contains(err.Error(), emptyMarker) {
		t.Fatalf("unborn blame = %v", err)
	}
}

// TestBlameFiveThousandLines is the responsiveness contract: a 5,000-line file
// must render without a pathological walk. The bound is generous (CI hosts
// vary); the log line is the number the report cites.
func TestBlameFiveThousandLines(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: 5k-line blame")
	}
	fix := newBrowseRepo(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "w")
	barePath := filepath.Join(dir, "b.git")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, dir, "init", "--bare", "-b", "main", barePath)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(work, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "big.txt")
	runGit(t, work, "commit", "-m", "five thousand")
	runGit(t, work, "push", barePath, "main")
	bare := &git.LocalRepo{Root: dir, ID: fix.bare.ID, Path: barePath}
	v, id := browseView(t, bare)
	start := time.Now()
	res, err := v.Blame(context.Background(), id, "main", "big.txt")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 5000 || res.Truncated {
		t.Fatalf("lines = %d truncated=%v", len(res.Lines), res.Truncated)
	}
	t.Logf("blame of 5000 lines: %s", elapsed.Round(time.Millisecond))
	if elapsed > 10*time.Second {
		t.Fatalf("blame of 5000 lines took %s (budget 10s)", elapsed)
	}
}

// TestBlameTruncatesPastCap pins the -L bound: one extra line is asked for so
// the page can state the cap instead of silently dropping a tail.
func TestBlameTruncatesPastCap(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: cap-sized blame")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "w")
	barePath := filepath.Join(dir, "b.git")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, dir, "init", "--bare", "-b", "main", barePath)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	var b strings.Builder
	for i := 0; i <= MaxBlameLines; i++ {
		b.WriteString("x\n")
	}
	if err := os.WriteFile(filepath.Join(work, "long.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "long.txt")
	runGit(t, work, "commit", "-m", "long")
	runGit(t, work, "push", barePath, "main")
	bare := &git.LocalRepo{Root: dir, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: barePath}
	v, id := browseView(t, bare)
	res, err := v.Blame(context.Background(), id, "main", "long.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Lines) != MaxBlameLines {
		t.Fatalf("truncated=%v lines=%d", res.Truncated, len(res.Lines))
	}
}

// --- branch overview ---------------------------------------------------------

func TestParseBranchRows(t *testing.T) {
	line := "main\t" + strings.Repeat("a", 40) + "\t2026-01-02T03:04:05Z\t3 4\tAda\tSubject with\ta tab"
	rows := parseBranchRows([]byte(line+"\n"), true)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Name != "main" || r.Ahead != 3 || r.Behind != 4 || r.Author != "Ada" || r.Subject != "Subject with\ta tab" {
		t.Fatalf("row = %+v", r)
	}
	if r.Committed.UTC().Format("2006-01-02T15:04:05Z") != "2026-01-02T03:04:05Z" {
		t.Fatalf("date = %v", r.Committed)
	}
	// Without the atom the shape has five fields and no counts.
	rows = parseBranchRows([]byte("main\tsha\t2026-01-02T03:04:05Z\tAda\tSubject\n"), false)
	if len(rows) != 1 || rows[0].Ahead != 0 || rows[0].Behind != 0 || rows[0].Subject != "Subject" {
		t.Fatalf("plain rows = %+v", rows)
	}
	// Malformed lines are skipped, never fatal.
	bad := strings.Join([]string{
		"only\tone",
		"x\tsha\tbad-date\tnot-ahead\tauthor\tsubject",
		"y\tsha\t2026-01-02T03:04:05Z\t1 2\tauthor",
		"",
	}, "\n")
	if got := parseBranchRows([]byte(bad), true); len(got) != 0 {
		t.Fatalf("bad rows = %+v", got)
	}
	for _, tc := range []struct {
		in            string
		ahead, behind int
		ok            bool
	}{
		{"0 0", 0, 0, true},
		{"2 7", 2, 7, true},
		{"-1 2", 0, 0, false},
		{"1", 0, 0, false},
		{"x y", 0, 0, false},
		{"1 2 3", 0, 0, false},
	} {
		a, b, ok := parseAheadBehind(tc.in)
		if a != tc.ahead || b != tc.behind || ok != tc.ok {
			t.Fatalf("parseAheadBehind(%q) = %d %d %v", tc.in, a, b, ok)
		}
	}
}

func TestBranchesOverviewMatchesGit(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()
	got, err := v.Branches(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Default != "main" || got.DefaultSHA == "" {
		t.Fatalf("default = %+v", got)
	}
	if !got.AheadKnown {
		t.Fatal("ahead/behind must be known on this git")
	}
	byName := map[string]BranchEntry{}
	for _, b := range got.Branches {
		byName[b.Name] = b
		if b.Subject == "" {
			t.Fatalf("branch %s has no subject", b.Name)
		}
	}
	if len(byName) != 2 || byName["feature"].SHA != fix.second {
		t.Fatalf("branches = %+v", got.Branches)
	}
	// Oracle: the per-branch count the batched atom must equal.
	for _, name := range []string{"main", "feature"} {
		out := runGit(t, fix.bare.Path, "rev-list", "--count", "--left-right", "main..."+name)
		var ahead, behind int
		if _, err := fmt.Sscanf(out, "%d %d", &behind, &ahead); err != nil {
			t.Fatalf("oracle %s: %v", name, err)
		}
		if byName[name].Ahead != ahead || byName[name].Behind != behind {
			t.Fatalf("%s ahead/behind = %d/%d, oracle %d/%d", name, byName[name].Ahead, byName[name].Behind, ahead, behind)
		}
	}
	if !byName["main"].IsDefault || byName["feature"].IsDefault {
		t.Fatal("default flag wrong")
	}
}

func TestBranchesUnbornAndTruncation(t *testing.T) {
	// Unborn: no default, no rows, no atom (AheadKnown=false) — never a
	// fabricated zero count.
	dir := t.TempDir()
	bare := filepath.Join(dir, "e.git")
	runGit(t, dir, "init", "--bare", "-b", "main", bare)
	empty := &git.LocalRepo{Root: dir, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: bare}
	v, id := browseView(t, empty)
	got, err := v.Branches(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// The configured default (HEAD) is known; its sha is not, so no atom ran.
	if got.Default != "main" || got.DefaultSHA != "" || len(got.Branches) != 0 || got.AheadKnown {
		t.Fatalf("unborn overview = %+v", got)
	}

	// Truncation: MaxBranchRows+1 branches report More and drop the tail.
	work := filepath.Join(dir, "w")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	runGit(t, work, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "f")
	runGit(t, work, "commit", "-m", "one")
	for i := 0; i <= MaxBranchRows; i++ {
		runGit(t, work, "branch", fmt.Sprintf("b%03d", i))
	}
	runGit(t, work, "push", bare, "main", "refs/heads/*:refs/heads/*")
	full := &git.LocalRepo{Root: dir, ID: id, Path: bare}
	v2, _ := browseView(t, full)
	got, err = v2.Branches(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.More || len(got.Branches) != MaxBranchRows {
		t.Fatalf("truncation: more=%v rows=%d", got.More, len(got.Branches))
	}
}

// --- compare -----------------------------------------------------------------

func TestCompareMatchesGit(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()

	// first (base) → merge (head): head-only commits are the merge chain.
	got, err := v.Compare(ctx, id, fix.first, fix.merge, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identical || got.NoCommonAncestor {
		t.Fatalf("flags = %+v", got)
	}
	if got.MergeBase != fix.first {
		t.Fatalf("merge-base = %s, want %s", got.MergeBase, fix.first)
	}
	if got.Ahead == 0 || got.Behind != 0 {
		t.Fatalf("ahead/behind = %d/%d", got.Ahead, got.Behind)
	}
	if len(got.Commits) != got.Ahead {
		t.Fatalf("commits = %d, ahead = %d", len(got.Commits), got.Ahead)
	}
	// The patch is byte-identical to git's own merge-base..head diff.
	want := runGit(t, fix.bare.Path, "diff", "--no-color", "--no-renames", fix.first, fix.merge)
	if got.Patch != want+"\n" && strings.TrimRight(got.Patch, "\n") != want {
		t.Fatalf("patch differs from git diff\n--- got\n%s\n--- want\n%s", got.Patch, want)
	}
	// Stats agree with --numstat.
	if len(got.Stats) == 0 {
		t.Fatal("no stats")
	}
	statOut := runGit(t, fix.bare.Path, "diff", "--numstat", "--no-renames", fix.first, fix.merge)
	if !strings.Contains(statOut, got.Stats[0].Path) {
		t.Fatalf("stats %+v vs %q", got.Stats, statOut)
	}

	// Reverse direction: base=head branch, head=first → behind, no head-only
	// commits, and an empty patch (nothing to bring).
	rev, err := v.Compare(ctx, id, fix.merge, fix.first, 50)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Ahead != 0 || rev.Behind == 0 || len(rev.Commits) != 0 || rev.Patch != "" {
		t.Fatalf("reverse = %+v", rev)
	}

	// Identical refs: an honest empty comparison, no spawns past the check.
	same, err := v.Compare(ctx, id, fix.merge, fix.merge, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Identical || same.Patch != "" || len(same.Commits) != 0 {
		t.Fatalf("identical = %+v", same)
	}
}

func TestCompareTruncationAndDisjoint(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()
	// n=1 truncates the head-only list with More.
	got, err := v.Compare(ctx, id, fix.first, fix.merge, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.More || len(got.Commits) != 1 {
		t.Fatalf("more=%v commits=%d", got.More, len(got.Commits))
	}

	// An orphan branch shares no ancestor: marked, base→head diff still shown.
	root := t.TempDir()
	work := filepath.Join(root, "w")
	bare := filepath.Join(root, "b.git")
	runGit(t, root, "init", "-b", "main", work)
	runGit(t, root, "init", "--bare", "-b", "main", bare)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	runGit(t, work, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "a")
	runGit(t, work, "commit", "-m", "a")
	runGit(t, work, "checkout", "--orphan", "orphan")
	runGit(t, work, "rm", "-rf", "--cached", ".")
	_ = os.Remove(filepath.Join(work, "a"))
	if err := os.WriteFile(filepath.Join(work, "b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "b")
	runGit(t, work, "commit", "-m", "orphan root")
	orphan := runGit(t, work, "rev-parse", "HEAD")
	main := runGit(t, work, "rev-parse", "main")
	runGit(t, work, "push", bare, "main", "orphan")
	bareRepo := &git.LocalRepo{Root: root, ID: id, Path: bare}
	v2, _ := browseView(t, bareRepo)
	got, err = v2.Compare(ctx, id, main, orphan, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoCommonAncestor || got.MergeBase != "" {
		t.Fatalf("disjoint flags = %+v", got)
	}
	if got.Patch == "" {
		t.Fatal("disjoint compare must still show the tree difference")
	}
	if _, err := v2.Compare(ctx, id, "main", "main", 50); err == nil {
		t.Fatal("unresolved revs must be refused (resolved shas only)")
	}
}

func TestComparePatchCap(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "w")
	bare := filepath.Join(root, "b.git")
	runGit(t, root, "init", "-b", "main", work)
	runGit(t, root, "init", "--bare", "-b", "main", bare)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	runGit(t, work, "config", "core.autocrlf", "false")
	line := strings.Repeat("x", 120) + "\n"
	if err := os.WriteFile(filepath.Join(work, "big.txt"), []byte(strings.Repeat(line, 25000)), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "big.txt")
	runGit(t, work, "commit", "-m", "big")
	head := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "checkout", "--orphan", "empty")
	runGit(t, work, "rm", "-rf", "--cached", ".")
	_ = os.Remove(filepath.Join(work, "big.txt"))
	runGit(t, work, "commit", "--allow-empty", "-m", "empty")
	empty := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", bare, "main", "empty")
	bareRepo := &git.LocalRepo{Root: root, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: bare}
	v, id := browseView(t, bareRepo)
	got, err := v.Compare(context.Background(), id, empty, head, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PatchTruncated || len(got.Patch) != MaxComparePatch {
		t.Fatalf("patch cap: truncated=%v len=%d", got.PatchTruncated, len(got.Patch))
	}
}

// --- failure paths through a scripted git binary -----------------------------

// fakeGit writes a shell script that delegates to the real git except for the
// shape named in body (a case pattern on "$1" and/or the format argument).
func fakeGit(t *testing.T, body string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git absent")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "git-fake")
	script := "#!/bin/sh\nREAL=" + real + "\n" + body + "\nexec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBranchesFailurePaths(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()

	// No serving copy: ErrPending from localView, no spawn.
	if _, err := (&walView{}).Branches(ctx, id); err != ErrPending {
		t.Fatalf("pending branches = %v", err)
	}

	// for-each-ref fails both with and without the atom: a real error.
	v.binary = fakeGit(t, `if [ "$1" = for-each-ref ]; then echo boom >&2; exit 128; fi`)
	if _, err := v.Branches(ctx, id); err == nil || !strings.Contains(err.Error(), "branches of") {
		t.Fatalf("for-each-ref failure = %v", err)
	}

	// Old git: the ahead-behind atom is rejected; the fallback enumerates
	// without it and reports unknown counts (never a fabricated zero).
	v.binary = fakeGit(t, `if [ "$1" = for-each-ref ]; then
  for a in "$@"; do case "$a" in *ahead-behind*) echo "malformed format" >&2; exit 128;; esac; done
fi`)
	got, err := v.Branches(ctx, id)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if got.AheadKnown || len(got.Branches) != 2 {
		t.Fatalf("fallback overview = %+v", got)
	}
	for _, b := range got.Branches {
		if b.Ahead != 0 || b.Behind != 0 || b.Subject == "" {
			t.Fatalf("fallback row = %+v", b)
		}
	}
}

func TestCompareFailurePaths(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()

	if _, err := (&walView{}).Compare(ctx, id, fix.first, fix.merge, 10); err != ErrPending {
		t.Fatalf("pending compare = %v", err)
	}
	// Non-hex revs are refused before any spawn.
	if _, err := v.Compare(ctx, id, "main", fix.merge, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unresolved rev = %v", err)
	}
	if got := shortSHA10("abc"); got != "abc" {
		t.Fatalf("shortSHA10 = %q", got)
	}
	if got := shortSHA10(fix.merge); got != fix.merge[:10] {
		t.Fatalf("shortSHA10 = %q", got)
	}
	// n<=0 resolves to the default cap.
	got, err := v.Compare(ctx, id, fix.first, fix.merge, 0)
	if err != nil || len(got.Commits) == 0 {
		t.Fatalf("n=0 compare = %v %d", err, len(got.Commits))
	}

	// rev-list fails: the error names the abbreviated sides and the sentinel.
	v.binary = fakeGit(t, `if [ "$1" = rev-list ]; then echo boom >&2; exit 128; fi`)
	if _, err := v.Compare(ctx, id, fix.first, fix.merge, 10); !errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), fix.first[:10]) {
		t.Fatalf("rev-list failure = %v", err)
	}
	// rev-list emits garbage: an honest parse error, not a zero comparison.
	v.binary = fakeGit(t, `if [ "$1" = rev-list ]; then echo "not counts"; exit 0; fi`)
	if _, err := v.Compare(ctx, id, fix.first, fix.merge, 10); err == nil || !strings.Contains(err.Error(), "compare counts") {
		t.Fatalf("count parse = %v", err)
	}
	// log fails after the counts succeeded.
	v.binary = fakeGit(t, `if [ "$1" = log ]; then echo boom >&2; exit 128; fi`)
	if _, err := v.Compare(ctx, id, fix.first, fix.merge, 10); !errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "compare history") {
		t.Fatalf("log failure = %v", err)
	}
	// A failing diff degrades to an empty patch/stats, never a failed compare
	// (display metadata vs. the commit list's own read).
	v.binary = fakeGit(t, `if [ "$1" = diff ]; then echo boom >&2; exit 128; fi`)
	got, err = v.Compare(ctx, id, fix.first, fix.merge, 10)
	if err != nil {
		t.Fatalf("diff failure = %v", err)
	}
	if got.Patch != "" || len(got.Stats) != 0 || len(got.Commits) == 0 {
		t.Fatalf("diff failure result = %+v", got)
	}
}

func TestBlameFailurePaths(t *testing.T) {
	fix := newBrowseRepo(t)
	v, id := browseView(t, fix.bare)
	ctx := context.Background()
	if _, err := (&walView{}).Blame(ctx, id, fix.merge, "story.txt"); err != ErrPending {
		t.Fatalf("pending blame = %v", err)
	}
	// A non-numeric cat-file size is a hard error (corrupt object metadata).
	v.binary = fakeGit(t, `if [ "$1" = cat-file ] && [ "$2" = -s ]; then echo nope; exit 0; fi`)
	if _, err := v.Blame(ctx, id, fix.merge, "story.txt"); err == nil {
		t.Fatal("bad size must fail")
	}
	// The blame spawn itself fails.
	v.binary = fakeGit(t, `if [ "$1" = blame ]; then echo boom >&2; exit 128; fi`)
	if _, err := v.Blame(ctx, id, fix.merge, "story.txt"); !errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("blame failure = %v", err)
	}
}
