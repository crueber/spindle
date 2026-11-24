package api

// browse.go — the repository-browsing reads the server-rendered pages need
// beyond the 07 §9 object recipes: per-line blame, the branch overview and
// the ref-to-ref comparison (docs/go/04_git.md §16). They are OPTIONAL
// extensions of RepoView: a view that does not implement RepoBrowse simply
// renders those pages as unavailable (the pages' own 503), so existing
// RepoView fakes and older binders keep compiling and keep their contract.
//
// Every method runs in the SAME serving copy and through the SAME gitCmd
// discipline as the §9 recipes (GIT_DIR, GIT_TERMINAL_PROMPT=0, request ctx).
// The argv is pinned in 04_git.md §16; parsers are pure and unit-tested.

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
)

// RepoBrowse is the optional browsing extension of RepoView.
type RepoBrowse interface {
	// Blame attributes the visible lines of one file at a resolved commit.
	Blame(ctx context.Context, id git.RepoId, sha, path string) (BlameResult, error)
	// Branches lists every branch with its tip commit and its ahead/behind
	// counts against the default branch — ONE for-each-ref spawn.
	Branches(ctx context.Context, id git.RepoId) (BranchOverview, error)
	// Compare reports the head-only commits and the patch from base's
	// merge-base to head.
	Compare(ctx context.Context, id git.RepoId, base, head string, n int) (CompareResult, error)
}

var _ RepoBrowse = (*walView)(nil)

// MaxBlameLines bounds one blame render. git blame is asked for one extra
// line so truncation is observable without a second read; the page states the
// cap instead of silently dropping the tail.
const MaxBlameLines = 10000

// blameTimeout bounds one blame spawn. Blame walks history per line — an
// adversarial history must not hold a request open indefinitely.
const blameTimeout = 20 * time.Second

// BlameLine is one attributed source line (final line number, not original).
type BlameLine struct {
	SHA        string    `json:"sha"`
	Author     string    `json:"author"`
	AuthorTime time.Time `json:"author_time"`
	LineNo     int       `json:"line"`
	Text       string    `json:"text"`
}

// BlameResult is one file's blame. Size/TooLarge/Binary carry the blob page's
// own guard inputs, so the page refuses with identical language and never
// runs blame on a file it would not display.
type BlameResult struct {
	Size      int64       `json:"size"`
	TooLarge  bool        `json:"too_large,omitempty"`
	Binary    bool        `json:"binary,omitempty"`
	Lines     []BlameLine `json:"lines"`
	Truncated bool        `json:"truncated,omitempty"`
}

// Blame parses `git blame --line-porcelain` for one file at one commit.
//
// The caller resolves the ref first and passes the commit sha; a sha-only rev
// means no user string is ever parsed as an option. The path is passed after
// `--` and is already validated by the tree walk that resolved it.
//
// --line-porcelain (not --porcelain) is the deliberate choice: every line
// record repeats its commit header, so attribution never depends on group
// state. A grouped stream splits commit metadata onto a group's first line;
// an unchanged trailing line in a different group, a CRLF file, or a merge
// commit then needs carry-over logic that is exactly where a parser drifts.
// The cost is a few hundred bytes per line, bounded by -L and the 2 MiB blob
// cap.
func (v *walView) Blame(ctx context.Context, id git.RepoId, sha, path string) (BlameResult, error) {
	if strings.TrimSpace(path) == "" {
		return BlameResult{}, fmt.Errorf("%w: blame requires a path", ErrNotFound)
	}
	if sha == "" || strings.HasPrefix(sha, "-") {
		return BlameResult{}, fmt.Errorf("%w: blame requires a resolved commit", ErrNotFound)
	}
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return BlameResult{}, err
	}
	spec := sha + ":" + path
	sizeOut, err := v.gitCmd(ctx, repo, "cat-file", "-s", spec)
	if err != nil {
		if v.unbornOnFailure(ctx, id) {
			return BlameResult{}, fmt.Errorf("%w: "+emptyMarker+"blame %s not found", ErrNotFound, path)
		}
		return BlameResult{}, fmt.Errorf("%w: blame %s not found", ErrNotFound, path)
	}
	var size int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(sizeOut)), "%d", &size); err != nil {
		return BlameResult{}, err
	}
	res := BlameResult{Size: size}
	if size > maxBlobBytes {
		res.TooLarge = true
		return res, nil
	}
	// One extra line: reaching it proves the cap dropped a tail.
	ctx, cancel := context.WithTimeout(ctx, blameTimeout)
	defer cancel()
	out, err := v.gitCmd(ctx, repo, "blame", "--line-porcelain",
		"-L", "1,"+strconv.Itoa(MaxBlameLines+1), sha, "--", path)
	if err != nil {
		if v.unbornOnFailure(ctx, id) {
			return BlameResult{}, fmt.Errorf("%w: "+emptyMarker+"blame %s unreadable", ErrNotFound, path)
		}
		return BlameResult{}, fmt.Errorf("%w: blame %s unreadable: %v", ErrNotFound, path, err)
	}
	lines := parseBlameLinePorcelain(out)
	if len(lines) > MaxBlameLines {
		lines = lines[:MaxBlameLines]
		res.Truncated = true
	}
	res.Lines = lines
	res.Binary = blameBinary(lines)
	return res, nil
}

// parseBlameLinePorcelain parses --line-porcelain output into per-line
// records. Records are `<sha> <orig> <final> [<count>]`, key/value header
// lines (`author `, `author-time `, …), then a tab-prefixed content line.
// With --line-porcelain every record carries the full header, so the parser
// keeps no cross-record state. Malformed/truncated streams skip the affected
// record instead of failing the page.
func parseBlameLinePorcelain(out []byte) []BlameLine {
	lines := []BlameLine{}
	var cur *BlameLine
	var authorTime int64
	flush := func() {
		if cur == nil {
			return
		}
		if authorTime > 0 {
			cur.AuthorTime = time.Unix(authorTime, 0).UTC()
		}
		lines = append(lines, *cur)
		cur = nil
		authorTime = 0
	}
	for _, raw := range bytes.Split(out, []byte("\n")) {
		line := string(raw)
		if strings.HasPrefix(line, "\t") {
			if cur != nil {
				cur.Text = line[1:]
			}
			flush()
			continue
		}
		if sha, _, no, ok := parseBlameHeader(line); ok {
			flush()
			cur = &BlameLine{SHA: sha, LineNo: no}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "author "):
			cur.Author = line[len("author "):]
		case strings.HasPrefix(line, "author-time "):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[len("author-time "):]), 10, 64); err == nil {
				authorTime = n
			}
		}
	}
	flush()
	return lines
}

// parseBlameHeader recognises a porcelain record header and returns its
// commit, original line and FINAL line number. The third field is the final
// (file) line number; the second is the pre-image number and is not the
// gutter a reader expects.
func parseBlameHeader(line string) (sha string, orig, final int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || len(fields) > 4 {
		return "", 0, 0, false
	}
	if !isHexOID(fields[0]) {
		return "", 0, 0, false
	}
	orig, err1 := strconv.Atoi(fields[1])
	final, err2 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	if len(fields) == 4 {
		if _, err := strconv.Atoi(fields[3]); err != nil {
			return "", 0, 0, false
		}
	}
	return fields[0], orig, final, true
}

// isHexOID reports a 40/64-char lowercase/uppercase hex object id.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// blameBinary applies git's own NUL-in-first-8KiB heuristic to the attributed
// content (the Blob recipe's rule), so the blame page refuses with the blob
// page's language.
func blameBinary(lines []BlameLine) bool {
	n := 0
	for _, l := range lines {
		if n >= 8000 {
			break
		}
		text := l.Text
		if n+len(text) > 8000 {
			text = text[:8000-n]
		}
		if strings.IndexByte(text, 0) >= 0 {
			return true
		}
		n += len(l.Text) + 1
	}
	return false
}

// MaxBranchRows bounds the branch overview. The list is one spawn and the
// page is a table; 500 rows is a comfortable read and an honest truncation
// point (the page says so) rather than an unbounded render.
const MaxBranchRows = 500

// BranchEntry is one branch row.
type BranchEntry struct {
	Name      string    `json:"name"`
	SHA       string    `json:"sha"`
	Subject   string    `json:"subject"`
	Author    string    `json:"author"`
	Committed time.Time `json:"committed"`
	Ahead     int       `json:"ahead"`
	Behind    int       `json:"behind"`
	IsDefault bool      `json:"is_default,omitempty"`
}

// BranchOverview is the branches page's data. AheadKnown is false when the
// git build predates the `%(ahead-behind:)` atom (the page then renders "—",
// never a fabricated zero).
type BranchOverview struct {
	Default    string        `json:"default"`
	DefaultSHA string        `json:"default_sha,omitempty"`
	AheadKnown bool          `json:"ahead_known"`
	Branches   []BranchEntry `json:"branches"`
	More       bool          `json:"more,omitempty"`
}

// Branches reads every branch in one spawn. The ahead/behind counts come from
// for-each-ref's `%(ahead-behind:<default-sha>)` atom: the atom is evaluated
// by git per ref inside the ONE enumeration, so N branches never become N
// rev-list processes (the naive per-row shape). The default is passed as a
// resolved sha, not a ref name, because a ref name inside the format string
// could carry format metacharacters (refnames may contain '%' and parens).
//
// A git without the atom fails the spawn; the fallback re-enumerates without
// it and reports AheadKnown=false so the page stays honest instead of 503ing
// the whole branch list.
func (v *walView) Branches(ctx context.Context, id git.RepoId) (BranchOverview, error) {
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return BranchOverview{}, err
	}
	snap, err := v.layer.Snapshot(repo)
	if err != nil {
		return BranchOverview{}, err
	}
	out := BranchOverview{Branches: []BranchEntry{}}
	if snap == nil {
		return out, nil
	}
	defaultRef := snap.HeadTarget
	if strings.HasPrefix(defaultRef, "refs/heads/") {
		out.Default = strings.TrimPrefix(defaultRef, "refs/heads/")
		if e, ok := snap.Get(defaultRef); ok {
			out.DefaultSHA = entrySHA(e)
		}
	}
	base := "%(refname:strip=2)%09%(objectname)%09%(committerdate:iso-strict)%09%(authorname)%09%(subject)"
	withAhead := ""
	if out.DefaultSHA != "" {
		withAhead = "%(refname:strip=2)%09%(objectname)%09%(committerdate:iso-strict)%09%(ahead-behind:" + out.DefaultSHA + ")%09%(authorname)%09%(subject)"
	}
	run := func(format string) ([]byte, error) {
		return v.gitCmd(ctx, repo, "for-each-ref", "--sort=-committerdate",
			"--count="+itoa(MaxBranchRows+1), "--format="+format, "refs/heads")
	}
	var raw []byte
	if withAhead != "" {
		out.AheadKnown = true
		raw, err = run(withAhead)
		if err != nil {
			out.AheadKnown = false
			raw, err = run(base)
		}
	} else {
		raw, err = run(base)
	}
	if err != nil {
		return BranchOverview{}, fmt.Errorf("branches of %s/%s: %v", id.Owner, id.Name, err)
	}
	rows := parseBranchRows(raw, out.AheadKnown)
	if len(rows) > MaxBranchRows {
		rows = rows[:MaxBranchRows]
		out.More = true
	}
	for _, row := range rows {
		row.IsDefault = row.Name == out.Default
		out.Branches = append(out.Branches, row)
	}
	return out, nil
}

// parseBranchRows parses the for-each-ref stream. Fields are tab-separated
// with the subject LAST (a subject may itself contain tabs; refnames cannot).
// withAhead selects the six-field shape.
func parseBranchRows(out []byte, withAhead bool) []BranchEntry {
	rows := []BranchEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 6)
		if withAhead {
			if len(fields) != 6 {
				continue
			}
			ahead, behind, ok := parseAheadBehind(fields[3])
			if !ok {
				continue
			}
			rows = append(rows, BranchEntry{
				Name:      fields[0],
				SHA:       fields[1],
				Committed: parseRFC3339(fields[2]),
				Ahead:     ahead,
				Behind:    behind,
				Author:    fields[4],
				Subject:   fields[5],
			})
			continue
		}
		fields = strings.SplitN(line, "\t", 5)
		if len(fields) != 5 {
			continue
		}
		rows = append(rows, BranchEntry{
			Name:      fields[0],
			SHA:       fields[1],
			Committed: parseRFC3339(fields[2]),
			Author:    fields[3],
			Subject:   fields[4],
		})
	}
	return rows
}

// parseAheadBehind parses for-each-ref's "<ahead> <behind>" atom output.
func parseAheadBehind(field string) (ahead, behind int, ok bool) {
	parts := strings.Fields(field)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || a < 0 || b < 0 {
		return 0, 0, false
	}
	return a, b, true
}

// MaxCompareCommits bounds the compare page's commit list (the patch has its
// own byte cap). More=true means the list was truncated, never silently.
const MaxCompareCommits = 250

// MaxComparePatch bounds the unified diff bytes one compare render parses and
// ships. Larger comparisons are truncated with an explicit notice.
const MaxComparePatch = 2 << 20

// CompareResult is one ref-to-ref comparison.
type CompareResult struct {
	BaseSHA          string   `json:"base_sha"`
	HeadSHA          string   `json:"head_sha"`
	MergeBase        string   `json:"merge_base,omitempty"`
	Ahead            int      `json:"ahead"`
	Behind           int      `json:"behind"`
	Commits          []Commit `json:"commits"`
	More             bool     `json:"more,omitempty"`
	Stats            []Stat   `json:"stats"`
	Patch            string   `json:"patch,omitempty"`
	PatchTruncated   bool     `json:"patch_truncated,omitempty"`
	Identical        bool     `json:"identical,omitempty"`
	NoCommonAncestor bool     `json:"no_common_ancestor,omitempty"`
}

// Compare reads one base...head comparison. Both revs are already resolved
// shas (the page resolves refs twice: base and head), and every argv below
// operates on hex object ids, so no user string is option-shaped.
//
// Semantics (the PR compare shape):
//   - ahead/behind are the head-only / base-only commit counts
//     (`rev-list --count --left-right <base>...<head>`; left=base, right=head).
//   - the diff is merge-base(base,head) → head: what a merge of head would
//     bring. No common ancestor falls back to base → head and is marked, so
//     the page can say so instead of pretending the histories share a root.
//   - the commit list is `log <head> --not <base>` (head-only commits).
func (v *walView) Compare(ctx context.Context, id git.RepoId, base, head string, n int) (CompareResult, error) {
	if !isHexOID(base) || !isHexOID(head) {
		return CompareResult{}, fmt.Errorf("%w: compare needs resolved commits", ErrNotFound)
	}
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return CompareResult{}, err
	}
	if n <= 0 || n > MaxCompareCommits {
		n = MaxCompareCommits
	}
	res := CompareResult{BaseSHA: base, HeadSHA: head, Commits: []Commit{}, Stats: []Stat{}}
	if base == head {
		res.Identical = true
		return res, nil
	}
	countOut, err := v.gitCmd(ctx, repo, "rev-list", "--count", "--left-right", base+"..."+head)
	if err != nil {
		return CompareResult{}, fmt.Errorf("%w: compare %s...%s: %v", ErrNotFound, shortSHA10(base), shortSHA10(head), err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(countOut)), "%d %d", &res.Behind, &res.Ahead); err != nil {
		return CompareResult{}, fmt.Errorf("compare counts: %v", err)
	}
	mbOut, mberr := v.gitCmd(ctx, repo, "merge-base", base, head)
	diffBase := base
	if mberr == nil {
		if mb := strings.TrimSpace(string(mbOut)); mb != "" {
			res.MergeBase = mb
			diffBase = mb
		}
	} else {
		res.NoCommonAncestor = true
	}
	logOut, err := v.gitCmd(ctx, repo, "log", "--format="+gitFmtLog,
		"--max-count="+itoa(n+1), head, "--not", base)
	if err != nil {
		return CompareResult{}, fmt.Errorf("%w: compare history: %v", ErrNotFound, err)
	}
	commits := parseLogRecords(logOut)
	if len(commits) > n {
		commits = commits[:n]
		res.More = true
	}
	res.Commits = commits
	if statOut, serr := v.gitCmd(ctx, repo, "diff", "--numstat", "-z", "--no-renames", diffBase, head); serr == nil {
		res.Stats, _ = parseNumstatPatch(statOut)
	}
	if patchOut, perr := v.gitCmd(ctx, repo, "diff", "--no-color", "--no-renames", diffBase, head); perr == nil {
		if len(patchOut) > MaxComparePatch {
			patchOut = patchOut[:MaxComparePatch]
			res.PatchTruncated = true
		}
		res.Patch = string(patchOut)
	}
	return res, nil
}

// shortSHA10 is a local display helper for error text only.
func shortSHA10(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
