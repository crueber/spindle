// grep.go — the code-search primitive (04_git.md §17, docs/features/20_search.md).
//
// Code search runs ONE `git grep` subprocess against a repository's already
// materialized serving copy at a RESOLVED object id. It needs no worktree, no
// index file and no write: grep walks the commit's tree in-process, skips
// binary blobs (-I) and honors nothing from the working tree. That is why v1
// has no search index (20_search.md §3): results are exact at the sha the
// caller resolved, with no invalidation and zero push-path round trips.
//
// The argv is pinned here (04_git.md §17.1) and must not drift:
//
//	git grep --no-color -n --column -I -z --no-textconv --no-recurse-submodules \
//	  [-i] [-w] [-F|-E] -m <perFile+1> -e <pattern> <rev> -- <pathspec>...
//
// `-z` makes the record stream parseable for paths containing spaces, colons
// and newlines. The record shape (verified on git 2.53) is
//
//	<sha>:<path> \0 <line> \0 <column> \0 <text> \n
//
// where <text> is the raw line bytes and may itself contain NUL (a "text" blob
// with a NUL after git's 8 KiB binary probe), so the stream is parsed
// sequentially — never split on NUL. <text> excludes the line terminator: the
// parser drops exactly one trailing CR, so a CRLF line's content is reported
// without it regardless of the committed bytes or the git build.
//
// Exit codes: 0 = matches, 1 = no matches (SUCCESS with an empty result),
// anything else = error (git exits 128 for a bad pattern). Treating 1 as an
// error is the classic bug this parser refuses to make.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Grep caps (04_git.md §17.3, 20_search.md §6.2). The service owns the user
// facing defaults; these are the primitive's hard bounds so a caller bug can
// never make one search unbounded.
const (
	GrepMaxPatternBytes = 256
	GrepMaxPathPatterns = 8
	GrepMaxPathBytes    = 256
	GrepDefaultPerFile  = 5
	GrepDefaultLimit    = 200
	GrepDefaultMaxBytes = 2 << 20 // 2 MiB of grep stdout per repository
	GrepDefaultMaxText  = 4 << 10 // 4 KiB of text per match
	GrepDefaultTimeout  = 10 * time.Second

	GrepMaxPerFile      = 100
	GrepMaxLimit        = 1000
	GrepMaxOutputBytes  = 16 << 20
	GrepMaxTextBytes    = 64 << 10
	GrepMinTextBytes    = 8
	GrepMaxTimeout      = 60 * time.Second
	GrepDefaultPoolSize = 2
)

// ErrGrepPattern marks a pattern git (or the pre-flight validator) rejected.
// It is a 400-class failure, never "no matches".
var ErrGrepPattern = errors.New("invalid search pattern")

// GrepPatternError carries git's own diagnostic for a pattern that the
// pre-flight POSIX check missed but git's regcomp rejected.
type GrepPatternError struct{ Detail string }

func (e *GrepPatternError) Error() string { return "invalid search pattern: " + e.Detail }

func (e *GrepPatternError) Unwrap() error { return ErrGrepPattern }

// GrepQuery is one search: the pattern plus its knobs and caps. The zero value
// is valid — fixed string, ASCII case-insensitive, no word match, no pathspec,
// default caps and timeout (20_search.md §6.1).
type GrepQuery struct {
	Pattern       string
	Regex         bool     // POSIX ERE (-E); false = fixed string (-F)
	CaseSensitive bool     // false = -i (ASCII-only under LC_ALL=C)
	Word          bool     // -w
	Paths         []string // git pathspecs, passed after -- (magic prefixes rejected)

	PerFile  int           // matches kept per file; git is asked for PerFile+1
	Limit    int           // total matches kept
	MaxBytes int           // stdout bytes read before the scan is capped
	MaxText  int           // bytes kept per match text (middle-ellipsized)
	Timeout  time.Duration // hard subprocess deadline
}

// GrepMatch is one matching line.
type GrepMatch struct {
	Line   int    // 1-based line number
	Column int    // 1-based byte column of the match start
	Text   string // line bytes, capped at MaxText and middle-ellipsized
}

// GrepFile groups one path's matches.
type GrepFile struct {
	Path      string
	Matches   []GrepMatch
	Truncated bool // git's per-file cap fired (PerFile+1 records were seen)
}

// GrepStats is the completeness bookkeeping the API projects verbatim
// (complete/truncated/timed_out in 20_search.md §7.1).
type GrepStats struct {
	FileCount int  // files with at least one kept match
	Matches   int  // kept matches
	Bytes     int  // stdout bytes observed (stops just past MaxBytes)
	Truncated bool // a match or byte cap fired
	TimedOut  bool // the deadline fired; Files/Matches are partial
}

// GrepResult is one repository's search answer. Files is always non-nil so the
// JSON projection emits [] rather than null (07_api.md §2).
type GrepResult struct {
	GrepStats
	Files []GrepFile
}

// GrepAt searches the bare git directory dir at rev (a resolved 40/64-hex
// object id — never a ref name or other user string).
func (l *Layer) GrepAt(ctx context.Context, dir, rev string, q GrepQuery) (*GrepResult, error) {
	if dir == "" {
		return nil, errInvalidInput("grep: empty git dir")
	}
	return l.grep(ctx, dir, rev, q)
}

// Grep searches repo's serving copy at rev. It is the only entry point search
// callers need; GrepAt exists for a caller that already holds the git dir.
func (l *Layer) Grep(ctx context.Context, repo *LocalRepo, rev string, q GrepQuery) (*GrepResult, error) {
	if repo == nil || repo.Path == "" {
		return nil, errInvalidInput("grep: nil repo")
	}
	return l.grep(ctx, repo.Path, rev, q)
}

// defaultScanPool bounds a Layer that was built without NewLayer (a bare
// struct literal in a test or an older composition). It is deliberately NOT
// the control-plane git pool: a search must never starve clone/upload-pack.
var defaultScanPool = NewPool(GrepDefaultPoolSize)

func (l *Layer) grep(ctx context.Context, dir, rev string, q GrepQuery) (*GrepResult, error) {
	if !ValidOid(rev) || isZeroOid(rev) {
		return nil, errInvalidInput("grep: revision must be a resolved object id, got %q", rev)
	}
	q = q.normalized()
	if err := q.validate(); err != nil {
		return nil, err
	}
	pool := l.ScanPool
	if pool == nil {
		pool = defaultScanPool
	}
	var res *GrepResult
	err := pool.Run(ctx, func() error {
		var e error
		res, e = l.execGrep(ctx, dir, rev, q)
		return e
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// normalized applies the defaults and the primitive's hard bounds. A zero knob
// takes the search default; an out-of-range knob is clamped, never trusted.
func (q GrepQuery) normalized() GrepQuery {
	if q.PerFile <= 0 {
		q.PerFile = GrepDefaultPerFile
	}
	if q.PerFile > GrepMaxPerFile {
		q.PerFile = GrepMaxPerFile
	}
	if q.Limit <= 0 {
		q.Limit = GrepDefaultLimit
	}
	if q.Limit > GrepMaxLimit {
		q.Limit = GrepMaxLimit
	}
	if q.MaxBytes <= 0 {
		q.MaxBytes = GrepDefaultMaxBytes
	}
	if q.MaxBytes > GrepMaxOutputBytes {
		q.MaxBytes = GrepMaxOutputBytes
	}
	if q.MaxText <= 0 {
		q.MaxText = GrepDefaultMaxText
	}
	if q.MaxText < GrepMinTextBytes {
		q.MaxText = GrepMinTextBytes
	}
	if q.MaxText > GrepMaxTextBytes {
		q.MaxText = GrepMaxTextBytes
	}
	if q.Timeout <= 0 {
		q.Timeout = GrepDefaultTimeout
	}
	if q.Timeout > GrepMaxTimeout {
		q.Timeout = GrepMaxTimeout
	}
	return q
}

// validate rejects everything that must not reach argv. The pattern rides -e,
// so a leading dash is data, not an option; the remaining rules keep the
// expression bounded and the record grammar unambiguous.
func (q GrepQuery) validate() error {
	switch {
	case q.Pattern == "":
		return errInvalidInput("grep: empty pattern")
	case len(q.Pattern) > GrepMaxPatternBytes:
		return errInvalidInput("grep: pattern exceeds %d bytes", GrepMaxPatternBytes)
	case strings.ContainsAny(q.Pattern, "\x00\n\r"):
		return errInvalidInput("grep: pattern contains a NUL, CR or newline")
	}
	if q.Regex {
		// RE2 in POSIX mode is a linear-time validator for the dialect git's
		// regcomp accepts. It cannot catch every ERE corner; git's own exit
		// code is still classified below.
		if _, err := regexp.CompilePOSIX(q.Pattern); err != nil {
			return &GrepPatternError{Detail: err.Error()}
		}
	}
	if len(q.Paths) > GrepMaxPathPatterns {
		return errInvalidInput("grep: more than %d path patterns", GrepMaxPathPatterns)
	}
	for _, p := range q.Paths {
		switch {
		case p == "":
			return errInvalidInput("grep: empty path pattern")
		case len(p) > GrepMaxPathBytes:
			return errInvalidInput("grep: path pattern exceeds %d bytes", GrepMaxPathBytes)
		case strings.ContainsAny(p, "\x00\n\r"):
			return errInvalidInput("grep: path pattern contains a NUL, CR or newline")
		case strings.ContainsAny(p[:1], ":!^"):
			// A leading colon selects git pathspec magic and a leading !/^
			// negates; both silently change the meaning of the query.
			return errInvalidInput("grep: path pattern %q uses pathspec magic", p)
		}
	}
	return nil
}

// grepArgv is the pinned argv (04_git.md §17.1). It is a separate function so
// the shape is unit-tested literally.
func grepArgv(rev string, q GrepQuery) []string {
	argv := []string{
		"grep", "--no-color", "-n", "--column", "-I", "-z",
		"--no-textconv", "--no-recurse-submodules",
	}
	if !q.CaseSensitive {
		argv = append(argv, "-i")
	}
	if q.Word {
		argv = append(argv, "-w")
	}
	if q.Regex {
		argv = append(argv, "-E")
	} else {
		argv = append(argv, "-F")
	}
	// Ask for one extra match per file: git caps -m silently, so the extra
	// record is what makes per-file truncation observable without a rerun.
	argv = append(argv, "-m", strconv.Itoa(q.PerFile+1), "-e", q.Pattern, rev, "--")
	return append(argv, q.Paths...)
}

// execGrep runs one subprocess and returns its parsed result. Every failure is
// classified: caller cancellation is an error, the deadline and every cap are
// fields (partial results, nil error), exit 1 is empty success, and a bad
// pattern is ErrGrepPattern.
func (l *Layer) execGrep(ctx context.Context, dir, rev string, q GrepQuery) (*GrepResult, error) {
	argv := grepArgv(rev, q)

	// Two contexts: runCtx is cancelled by the cap writer (a runaway match
	// count must die at the cap), timeoutCtx carries the search deadline.
	// The PARENT ctx is never cancelled by us, so a caller disconnect stays
	// distinguishable from a deadline or a cap.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	timeoutCtx, cancelTimeout := context.WithTimeout(runCtx, q.Timeout)
	defer cancelTimeout()

	cmd := exec.CommandContext(timeoutCtx, l.Binary, argv...)
	cmd.Dir = dir
	cmd.Env = l.baseEnv(dir, "LC_ALL=C")
	cmd.WaitDelay = 2 * time.Second // a surviving grandchild must not wedge the scan

	stderr := newBounded(8 << 10)
	cmd.Stderr = stderr
	parser := newGrepParser(rev, q, cancelRun)
	cmd.Stdout = parser

	runErr := cmd.Run()
	res := parser.result()

	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case parser.capped:
		res.Truncated = true
		return res, nil
	case timeoutCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		return res, nil
	case runErr == nil:
		return res, nil
	case exitCode(runErr) == 1:
		// "No matches" is success with an empty result — never an error.
		return res, nil
	}

	if q.Regex && looksLikePatternError(stderr.String()) {
		return res, &GrepPatternError{Detail: firstLine(stderr.String())}
	}
	detail := "grep failed"
	if runErr != nil {
		detail = runErr.Error()
	}
	return res, &GitError{
		Kind:   GitErrSubprocess,
		Cmd:    strings.Join(argv, " "),
		Detail: detail,
		Stderr: stderr.String(),
	}
}

// exitCode extracts a child's exit status (0 for a successful exec).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// looksLikePatternError classifies git's regcomp diagnostics. The pre-flight
// POSIX check catches nearly every case; this is the backstop for a dialect
// corner RE2 accepts and regcomp does not.
func looksLikePatternError(stderr string) bool {
	for _, marker := range []string{"Unmatched", "Invalid", "regex", "-e option"} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// clipMatchText keeps at most max bytes of a line, centering the window on the
// match's byte column and marking each cut with an ellipsis. Cuts land on UTF-8
// rune boundaries so the result is always valid text (a minified bundle is one
// multi-megabyte line; 20_search.md §6.2).
func clipMatchText(text string, column, max int) string {
	if len(text) <= max {
		return text
	}
	const ellipsis = "…"
	if max <= 2*len(ellipsis) {
		return truncAtRune(text, max)
	}
	keep := max - 2*len(ellipsis)
	start := column - 1
	if start < 0 {
		start = 0
	}
	if start > len(text) {
		start = len(text)
	}
	cutStart := start - keep/3
	if cutStart < 0 {
		cutStart = 0
	}
	cutEnd := cutStart + keep
	if cutEnd > len(text) {
		// len(text) > max > keep, so the re-anchored start is always >= 0.
		cutEnd = len(text)
		cutStart = cutEnd - keep
	}
	cutStart = advanceToRuneStart(text, cutStart)
	cutEnd = retreatToRuneStart(text, cutEnd)

	out := text[cutStart:cutEnd]
	if cutStart > 0 {
		out = ellipsis + out
	}
	if cutEnd < len(text) {
		out += ellipsis
	}
	return out
}

// truncAtRune cuts s to at most max bytes without splitting a rune.
func truncAtRune(s string, max int) string {
	if max >= len(s) {
		return s
	}
	if max < 0 {
		max = 0
	}
	return s[:retreatToRuneStart(s, max)]
}

// advanceToRuneStart moves i forward past UTF-8 continuation bytes.
func advanceToRuneStart(s string, i int) int {
	for i < len(s) && i > 0 && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// retreatToRuneStart moves i backward until it starts a rune.
func retreatToRuneStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// --- the streaming NUL record parser ----------------------------------------

const (
	grepStatePath = iota
	grepStateLine
	grepStateColumn
	grepStateText
)

// grepParser is the io.Writer git's stdout streams into. It parses records
// incrementally (a record may straddle any write boundary), enforces the caps,
// and cancels the child process the moment a cap fires.
type grepParser struct {
	revPrefix []byte // "<rev>:"
	q         GrepQuery
	cancel    context.CancelFunc

	buf   []byte
	off   int
	state int

	curPath string
	curLine int
	curCol  int

	files       []GrepFile
	fileIdx     int    // index of curPath's entry, -1 until the first kept match
	trackedPath string // the path fileCount belongs to
	pathKnown   bool
	fileCount   int // records seen for the current file (per-file cap check)
	total       int
	bytesSeen   int
	capped      bool
}

func newGrepParser(rev string, q GrepQuery, cancel context.CancelFunc) *grepParser {
	return &grepParser{
		revPrefix: []byte(rev + ":"),
		q:         q,
		cancel:    cancel,
		files:     []GrepFile{},
		fileIdx:   -1,
		state:     grepStatePath,
	}
}

func (p *grepParser) Write(chunk []byte) (int, error) {
	n := len(chunk)
	p.bytesSeen += n
	if p.capped {
		return n, nil // drain; the child is already dying
	}
	// Parse exactly the bytes that fit under the cap, then cancel: a cap that
	// happens mid-chunk still keeps every complete record before it.
	room := p.q.MaxBytes - (p.bytesSeen - n)
	over := false
	if room <= 0 {
		p.capped = true
		p.cancel()
		return n, nil
	}
	if room < n {
		chunk = chunk[:room]
		over = true
	}
	p.buf = append(p.buf, chunk...)
	p.parse()
	if over && !p.capped {
		p.capped = true
		p.cancel()
	}
	return n, nil
}

// parse consumes complete fields from p.buf[p.off:], leaving a partial record
// for the next write. A file's matches arrive contiguously (git greps one blob
// at a time), so the current-file counter is the whole per-file bookkeeping.
func (p *grepParser) parse() {
	p.compact()
	for !p.capped {
		buf := p.buf[p.off:]
		switch p.state {
		case grepStatePath:
			i := bytes.IndexByte(buf, 0)
			if i < 0 {
				return
			}
			p.curPath = p.stripRevPrefix(buf[:i])
			p.off += i + 1
			p.state = grepStateLine
		case grepStateLine:
			i := bytes.IndexByte(buf, 0)
			if i < 0 {
				return
			}
			p.curLine = atoiBytes(buf[:i])
			p.off += i + 1
			p.state = grepStateColumn
		case grepStateColumn:
			i := bytes.IndexByte(buf, 0)
			if i < 0 {
				return
			}
			p.curCol = atoiBytes(buf[:i])
			p.off += i + 1
			p.state = grepStateText
		case grepStateText:
			// The text runs to the next newline. A NUL inside it is content
			// (a text blob whose NUL sits past git's binary probe), which is
			// exactly why this is a sequential state machine and not a split.
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				return
			}
			p.record(string(buf[:i]))
			p.off += i + 1
			p.state = grepStatePath
		}
	}
}

// compact drops the consumed prefix so a long record stream never retains
// bytes it already parsed.
func (p *grepParser) compact() {
	if p.off == 0 {
		return
	}
	n := copy(p.buf, p.buf[p.off:])
	p.buf = p.buf[:n]
	p.off = 0
}

// record applies the caps to one complete match. A file entry is created only
// when a match is actually kept, so Files never carries empty paths.
func (p *grepParser) record(text string) {
	// The record text runs to '\n', so a CRLF line leaves its CR in the text.
	// Drop exactly one trailing CR: the answer is the line's content without
	// its terminator, independent of the committed bytes or the git build
	// (04_git.md §17.1).
	if n := len(text); n > 0 && text[n-1] == '\r' {
		text = text[:n-1]
	}
	if !p.pathKnown || p.trackedPath != p.curPath {
		p.pathKnown = true
		p.trackedPath = p.curPath
		p.fileIdx = -1
		p.fileCount = 0
	}
	p.fileCount++
	if p.fileCount > p.q.PerFile {
		// git was asked for PerFile+1: this extra record proves the per-file
		// cap fired. Keep the first PerFile and say so.
		if p.fileIdx >= 0 {
			p.files[p.fileIdx].Truncated = true
		}
		return
	}
	if p.total >= p.q.Limit {
		p.capped = true
		p.cancel()
		return
	}
	if p.fileIdx < 0 {
		p.files = append(p.files, GrepFile{Path: p.curPath, Matches: []GrepMatch{}})
		p.fileIdx = len(p.files) - 1
	}
	p.files[p.fileIdx].Matches = append(p.files[p.fileIdx].Matches, GrepMatch{
		Line:   p.curLine,
		Column: p.curCol,
		Text:   clipMatchText(text, p.curCol, p.q.MaxText),
	})
	p.total++
}

// stripRevPrefix removes the known "<rev>:" field. A path may itself contain a
// colon (and a newline), so the fallback splits at the first colon only when
// the exact prefix is absent — which a single-rev grep never produces.
func (p *grepParser) stripRevPrefix(field []byte) string {
	if bytes.HasPrefix(field, p.revPrefix) {
		return string(field[len(p.revPrefix):])
	}
	if i := bytes.IndexByte(field, ':'); i >= 0 {
		return string(field[i+1:])
	}
	return string(field)
}

func (p *grepParser) result() *GrepResult {
	files := p.files
	if files == nil {
		files = []GrepFile{}
	}
	return &GrepResult{
		GrepStats: GrepStats{
			FileCount: len(files),
			Matches:   p.total,
			Bytes:     p.bytesSeen,
			Truncated: p.capped,
		},
		Files: files,
	}
}

func atoiBytes(b []byte) int {
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 1 << 30
		}
	}
	return n
}

// String renders the query's defining options for logs (no pattern text).
func (q GrepQuery) String() string {
	return fmt.Sprintf("regex=%t case_sensitive=%t word=%t paths=%d",
		q.Regex, q.CaseSensitive, q.Word, len(q.Paths))
}
