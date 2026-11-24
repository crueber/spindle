package staticpages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGitTimeout bounds one git subprocess. A blob read is capped by
// Options.MaxFileBytes, so a request that outlives this is a stuck machine, not
// a big file.
const DefaultGitTimeout = 30 * time.Second

// gitPool is the bounded semaphore of concurrent static-site git processes. It
// is the package's own pool: bulk site reads never share a semaphore with
// control-plane git traffic (13 §7).
type gitPool struct{ sem chan struct{} }

func newGitPool(capacity int) *gitPool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &gitPool{sem: make(chan struct{}, capacity)}
}

// run executes fn while holding a slot. A ready slot is taken first and the
// context re-checked, so a canceled pass has one deterministic outcome instead
// of racing a free slot against a dead context (the coin flip the AGENTS.md
// field lessons call out).
func (p *gitPool) run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// SubprocessGit is the production GitRunner. Commands run with GIT_DIR pointing
// at the serving copy, prompts disabled, and the request context as their
// lifetime (a client disconnect kills the child).
type SubprocessGit struct {
	Binary  string
	Timeout time.Duration

	once sync.Once
	pool *gitPool
}

// NewSubprocessGit builds a runner; binary "" means "git" from PATH.
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	return &SubprocessGit{Binary: binary, Timeout: DefaultGitTimeout}
}

func (g *SubprocessGit) bin() string {
	if g.Binary == "" {
		return "git"
	}
	return g.Binary
}

func (g *SubprocessGit) timeout() time.Duration {
	if g.Timeout <= 0 {
		return DefaultGitTimeout
	}
	return g.Timeout
}

func (g *SubprocessGit) slots() *gitPool {
	g.once.Do(func() { g.pool = newGitPool(0) })
	return g.pool
}

// gitError carries the argv and stderr of a failed command for the server log.
type gitError struct {
	argv   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	msg := "git " + strings.Join(e.argv, " ") + ": " + e.err.Error()
	if s := strings.TrimSpace(e.stderr); s != "" {
		if len(s) > 200 {
			s = s[:200]
		}
		msg += ": " + s
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// exec runs one git command in dir, streaming stdout to w, under a pool slot and
// a per-command timeout. No lock is held by the caller across this call.
func (g *SubprocessGit) exec(ctx context.Context, dir string, w io.Writer, argv ...string) error {
	cctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	perr := g.slots().run(cctx, func() error {
		cmd := exec.CommandContext(cctx, g.bin(), argv...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_DIR=" + dir,
		}
		cmd.Stdout = w
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return &gitError{argv: argv, err: err, stderr: stderr.String()}
		}
		return nil
	})
	if perr == nil {
		return nil
	}
	if ctx.Err() != nil || cctx.Err() != nil {
		return fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], context.Cause(cctx))
	}
	return perr
}

// Resolve peels ref ("" = HEAD) to a commit oid:
//
//	git rev-parse --verify --quiet <ref>^{commit}
//
// Only "this name does not resolve" is a 404. A corrupt object, a broken
// repository, an unreadable directory, a missing binary or a killed child is an
// infrastructure failure the listener answers with a logged 503 — so the outcome
// is classified from git's own diagnostics instead of assuming every non-zero
// exit means "no such ref" (m10).
func (g *SubprocessGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	name := ref
	if name == "" {
		name = "HEAD"
	}
	var out bytes.Buffer
	if err := g.exec(ctx, dir, &out, "rev-parse", "--verify", "--quiet", name+"^{commit}"); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return "", err
		}
		if unknownRevision(err) {
			return "", fmt.Errorf("%w: unknown ref %q", ErrNotFound, ref)
		}
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	oid := strings.ToLower(strings.TrimSpace(out.String()))
	if !validOID(oid) {
		return "", fmt.Errorf("%w: rev-parse returned no commit id", ErrUnavailable)
	}
	return oid, nil
}

// unknownRevision reports whether a failed rev-parse means the requested name
// simply does not resolve, as opposed to a repository the listener cannot use.
//
// `--quiet` is what keeps a miss silent: for an unknown ref, a too-deep
// ancestor or an unresolvable object git prints nothing to stderr and exits 1
// (the `--verify` failure status). A real failure always says something — a
// corrupt loose object, "not a git repository", an I/O error — and exits 128, or
// never runs at all (a missing binary, a signal). Both shapes are unavailable,
// never a 404, so the stderr diagnostics are checked first and silence is only
// accepted with the verify exit status.
func unknownRevision(err error) bool {
	var gerr *gitError
	if !errors.As(err, &gerr) {
		return false
	}
	stderr := strings.ToLower(gerr.stderr)
	for _, marker := range []string{
		"needed a single revision", // rev-parse --verify <name>^{commit}
		"unknown revision",         // rev-parse in some git versions
		"not a valid object name",  // a malformed revision
		"bad revision",             // rev-parse --verify <bad>
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	if strings.TrimSpace(stderr) != "" {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// Lookup returns one tree entry by exact path:
//
//	git ls-tree -z -l -- <treeish> <path>
//
// The documented `-- <treeish>:<path>` form was rejected: it lists the named
// tree's children and fails outright on a blob, so it cannot return the entry
// for one path. The pathspec form answers for blobs, trees and misses alike, and
// the parsed name is compared against the request so a pathspec glob can never
// substitute another entry.
func (g *SubprocessGit) Lookup(ctx context.Context, dir, treeish, path string) (Entry, bool, error) {
	if !validOID(treeish) {
		return Entry{}, false, fmt.Errorf("%w: bad tree-ish %q", ErrInvalid, treeish)
	}
	if path == "" || strings.HasPrefix(path, ":") {
		return Entry{}, false, fmt.Errorf("%w: bad lookup path %q", ErrInvalid, path)
	}
	var out bytes.Buffer
	if err := g.exec(ctx, dir, &out, "ls-tree", "-z", "-l", "--", treeish, path); err != nil {
		// A missing path is a clean exit with no record (found=false). Any
		// non-zero exit means the commit or the repository is unusable, and that
		// is a 503, not a 404 — Resolve is the only place a 404 is decided (m10).
		return Entry{}, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return parseTreeEntry(out.Bytes(), path)
}

// ReadBlob streams one blob's bytes to w:
//
//	git cat-file blob <oid>
//
// A short write (the handler's size cap) surfaces as an error from Run; the
// caller keys off its own overflow flag rather than the wrapped error.
func (g *SubprocessGit) ReadBlob(ctx context.Context, dir, oid string, w io.Writer) error {
	if !validOID(oid) {
		return fmt.Errorf("%w: bad object id %q", ErrInvalid, oid)
	}
	if w == nil {
		return fmt.Errorf("%w: nil blob writer", ErrInvalid)
	}
	if err := g.exec(ctx, dir, w, "cat-file", "blob", oid); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// parseTreeEntry picks the record whose name is exactly want out of
// `ls-tree -z -l` output: "<mode> SP <type> SP <oid> SP+ <size> TAB <name> NUL".
func parseTreeEntry(out []byte, want string) (Entry, bool, error) {
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok || string(name) != want {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 4 {
			return Entry{}, false, fmt.Errorf("%w: malformed ls-tree record for %q", ErrUnavailable, want)
		}
		size := int64(-1)
		if fields[3] != "-" {
			n, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil || n < 0 {
				return Entry{}, false, fmt.Errorf("%w: bad size in ls-tree record for %q", ErrUnavailable, want)
			}
			size = n
		}
		if !validOID(fields[2]) {
			return Entry{}, false, fmt.Errorf("%w: bad object id in ls-tree record for %q", ErrUnavailable, want)
		}
		return Entry{Mode: fields[0], Type: fields[1], OID: fields[2], Size: size}, true, nil
	}
	return Entry{}, false, nil
}

// validOID accepts a full SHA-1 (40) or SHA-256 (64) hex object id. It is the
// last gate before an oid reaches `git cat-file`.
func validOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for i := 0; i < len(oid); i++ {
		c := oid[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

// Compile-time seam assertion.
var _ GitRunner = (*SubprocessGit)(nil)
