package threads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/actions"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// SubprocessGit is the production GitReader: stock git, exact argv (04_git.md
// §14), run under a bounded semaphore with the §2 subprocess discipline
// (GIT_TERMINAL_PROMPT=0, PATH-only env, bounded stderr, ctx timeout). It is a
// read-only reader: no ref, no object and no worktree is ever written.
type SubprocessGit struct {
	Binary  string // git.binary (default "git")
	Timeout time.Duration
	sem     chan struct{}
	dirs    runner
}

// NewSubprocessGit builds the production reader.
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	capacity := 4 * runtime.GOMAXPROCS(0)
	if capacity <= 0 {
		capacity = 4
	}
	return &SubprocessGit{Binary: binary, Timeout: 30 * time.Second, sem: make(chan struct{}, capacity)}
}

// ListWorkflowFiles runs
//
//	git ls-tree -z --name-only -r <commit> -- .spindle/workflows
//
// A missing directory is empty output (nil, nil), not an error.
func (g *SubprocessGit) ListWorkflowFiles(ctx context.Context, repo, commit string) ([]string, error) {
	if _, err := normalizeSHA(commit); err != nil {
		return nil, err
	}
	out, _, err := g.run(ctx, repo, []string{"ls-tree", "-z", "--name-only", "-r", commit, "--", workflow.Dir})
	if err != nil {
		return nil, notFoundIfExit(err)
	}
	raw := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
		if workflowPath(p) {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// DiffReader is the optional GitReader extension the path filters use. It is a
// separate interface so every test fake that predates path filtering keeps
// compiling; a reader without it cannot evaluate a path-filtered trigger and
// such a trigger fails closed (never fires).
type DiffReader interface {
	// DiffNames returns the repository-relative paths changed between two
	// commits. An empty from lists the files a root/new commit introduces.
	// mergeBase compares from...to (the PR rule: the merge base).
	DiffNames(ctx context.Context, repo, from, to string, mergeBase bool) ([]string, error)
}

// DiffNames runs one name-only diff (PAR-012). The list is capped at the same
// bound as a workflow read; a truncated list is an error, never a partial
// filter decision.
func (g *SubprocessGit) DiffNames(ctx context.Context, repo, from, to string, mergeBase bool) ([]string, error) {
	to = strings.ToLower(strings.TrimSpace(to))
	if _, err := normalizeSHA(to); err != nil {
		return nil, err
	}
	from = strings.ToLower(strings.TrimSpace(from))
	var argv []string
	switch {
	case mergeBase:
		if _, err := normalizeSHA(from); err != nil {
			return nil, fmt.Errorf("%w: a merge-base diff needs a full base commit", ErrInvalid)
		}
		argv = []string{"diff", "--name-only", "-z", from + "..." + to}
	case from != "" && !zeroOID(from):
		if _, err := normalizeSHA(from); err != nil {
			return nil, err
		}
		argv = []string{"diff", "--name-only", "-z", from, to}
	default:
		// A new ref has no pre-image; diff-tree lists the commit's own change
		// set (all files for a root commit).
		argv = []string{"diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", to}
	}
	out, truncated, err := g.run(ctx, repo, argv)
	if err != nil {
		return nil, notFoundIfExit(err)
	}
	if truncated {
		return nil, fmt.Errorf("%w: the changed-file list at %s is larger than %d bytes", ErrUnprocessable, to, maxWorkflowBytes)
	}
	raw := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// zeroOID reports git's all-zero id (a ref create/delete pre-image).
func zeroOID(sha string) bool {
	if sha == "" {
		return true
	}
	for _, c := range sha {
		if c != '0' {
			return false
		}
	}
	return true
}

// ReadFile runs `git show <commit>:<path>`.
func (g *SubprocessGit) ReadFile(ctx context.Context, repo, commit, p string) ([]byte, error) {
	if _, err := normalizeSHA(commit); err != nil {
		return nil, err
	}
	if p == "" || strings.HasPrefix(p, "-") || strings.Contains(p, ":") {
		return nil, fmt.Errorf("%w: invalid workflow path %q", ErrUnprocessable, p)
	}
	out, truncated, err := g.run(ctx, repo, []string{"show", commit + ":" + p})
	if err != nil {
		return nil, notFoundIfExit(err)
	}
	if truncated {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrUnprocessable, p, maxWorkflowBytes)
	}
	return []byte(out), nil
}

// ReadFileUpTo is ReadFile with an explicit size cap, for action content (a
// bundled JavaScript action is larger than a workflow file).
func (g *SubprocessGit) ReadFileUpTo(ctx context.Context, repo, commit, p string, max int64) ([]byte, error) {
	if _, err := normalizeSHA(commit); err != nil {
		return nil, err
	}
	if p == "" || strings.HasPrefix(p, "-") || strings.Contains(p, ":") {
		return nil, fmt.Errorf("%w: invalid action path %q", ErrUnprocessable, p)
	}
	if max <= 0 {
		max = maxWorkflowBytes
	}
	limit := int(max) + 1
	if limit < maxWorkflowBytes+1 || limit < 0 {
		limit = maxWorkflowBytes + 1
	}
	out, truncated, err := g.runEnvLimit(ctx, repo, []string{"show", commit + ":" + p}, nil, limit)
	if err != nil {
		return nil, notFoundIfExit(err)
	}
	if truncated {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrUnprocessable, p, max)
	}
	return []byte(out), nil
}

// maxActionTreeBytes bounds one `git ls-tree` listing of an action directory.
const maxActionTreeBytes = 64 << 20

// ListTree runs
//
//	git ls-tree -r -z --long <commit> -- <dir>
//
// and returns the regular files under dir. A symlink or submodule is returned
// with its mode so the caller can refuse it by name; the listing itself never
// follows either. A missing directory is an empty list (the caller names the
// "no metadata" refusal).
func (g *SubprocessGit) ListTree(ctx context.Context, repo, commit, dir string) ([]actions.TreeEntry, error) {
	if _, err := normalizeSHA(commit); err != nil {
		return nil, err
	}
	if strings.HasPrefix(dir, "-") || strings.Contains(dir, ":") || strings.Contains(dir, "\x00") {
		return nil, fmt.Errorf("%w: invalid action directory %q", ErrUnprocessable, dir)
	}
	argv := []string{"ls-tree", "-r", "-z", "--long", commit}
	if dir != "" {
		argv = append(argv, "--", dir)
	}
	out, truncated, err := g.runEnvLimit(ctx, repo, argv, nil, maxActionTreeBytes+1)
	if err != nil {
		if nf := notFoundIfExit(err); errors.Is(nf, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("%w: the action tree at %s is larger than %d bytes", ErrUnprocessable, dir, maxActionTreeBytes)
	}
	entries := make([]actions.TreeEntry, 0, 16)
	for _, rec := range strings.Split(strings.TrimRight(out, "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok || p == "" {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 4 {
			continue
		}
		mode, merr := strconv.ParseUint(fields[0], 8, 32)
		if merr != nil {
			continue
		}
		size := int64(-1)
		if fields[3] != "-" {
			if n, serr := strconv.ParseInt(fields[3], 10, 64); serr == nil {
				size = n
			}
		}
		entries = append(entries, actions.TreeEntry{Path: p, Mode: uint32(mode), Size: size})
	}
	return entries, nil
}

// ResolveRef runs `git rev-parse --verify --quiet <ref>^{commit}`.
func (g *SubprocessGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("%w: invalid ref %q", ErrInvalid, ref)
	}
	out, _, err := g.run(ctx, repo, []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"})
	if err != nil {
		if nf := notFoundIfExit(err); errors.Is(nf, ErrNotFound) {
			return "", fmt.Errorf("%w: unknown ref %q", ErrNotFound, ref)
		}
		return "", err
	}
	return normalizeSHA(strings.TrimSpace(out))
}

// DefaultBranch runs `git symbolic-ref --quiet --short HEAD` (the branch a
// scheduled run belongs to; a detached HEAD reports nothing, and the scheduler
// then creates the run without a branch name).
func (g *SubprocessGit) DefaultBranch(ctx context.Context, repo string) (string, error) {
	out, _, err := g.run(ctx, repo, []string{"symbolic-ref", "--quiet", "--short", "HEAD"})
	if err != nil {
		if nf := notFoundIfExit(err); errors.Is(nf, ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runner is the directory resolver composition injects (the serve-synced dirs
// adapter; tests use a temp dir).
type runner interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// SetDirs installs the per-repo synced-directory resolver. Without one every
// call fails closed with ErrUnavailable (composition always sets it).
func (g *SubprocessGit) SetDirs(d runner) { g.dirs = d }

// run executes one argv in the repo's synced directory under the semaphore.
// stdout is capped at maxWorkflowBytes+1 (a read must never buffer an
// arbitrarily large blob); stderr keeps its tail (8 KiB). A non-zero exit maps
// to ErrNotFound for the "unknown revision/path" family and ErrUnavailable for
// anything else — never a panic, never misreported as absent.
func (g *SubprocessGit) run(ctx context.Context, repo string, argv []string) (string, bool, error) {
	return g.runEnv(ctx, repo, argv, nil)
}

// runEnv is run with extra environment entries (the merge commit pins its
// author/committer identity and date so the derived commit is reproducible).
func (g *SubprocessGit) runEnv(ctx context.Context, repo string, argv, extraEnv []string) (string, bool, error) {
	return g.runEnvLimit(ctx, repo, argv, extraEnv, maxWorkflowBytes+1)
}

// runEnvLimit is runEnv with an explicit stdout cap, for the action reader
// (a bundled JavaScript action is legitimately larger than a workflow file).
func (g *SubprocessGit) runEnvLimit(ctx context.Context, repo string, argv, extraEnv []string, limit int) (string, bool, error) {
	dir, derr := g.dirOf(ctx, repo)
	if derr != nil {
		return "", false, derr
	}
	if g.sem != nil {
		select {
		case g.sem <- struct{}{}:
			defer func() { <-g.sem }()
		case <-ctx.Done():
			return "", false, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.Binary, argv...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}, extraEnv...)
	stdout := &capBuffer{limit: limit}
	var stderr boundedStderr
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	out := stdout.String()
	truncated := stdout.truncated
	if runErr == nil {
		return out, truncated, nil
	}
	if ctx.Err() != nil || cctx.Err() != nil {
		return "", false, fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], cctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return "", false, &gitExitError{code: exitErr.ExitCode(), stderr: strings.TrimSpace(stderr.String())}
	}
	return "", false, fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], runErr)
}

// --- PAR-024: the simulated merge commit ------------------------------------

// mergeCommitEpoch pins the derived merge commit's date so the same base+head
// pair always mints the same commit: repeated PR events reuse one object (git's
// content addressing), and the rerun never reports a different sha for
// identical trees. 2000-01-01T00:00:00Z.
const mergeCommitEpoch = "946684800 +0000"

// MergeCommit builds the pull_request merge commit in the repository's serving
// copy and returns its sha (docs/go/04_git.md §14a):
//
//	git merge-tree --write-tree <base> <head>
//	git commit-tree <tree> -p <base> -p <head> -m "Merge <head> into <base>"
//
// The commit is DERIVED state: it is not published through the WAL and not a
// durable ref, exactly like a trial merge. It exists in the serving copy so a
// job's checkout by sha can fetch it; after a serving-copy wipe the next PR
// event rebuilds the same commit. A conflicting merge is an error (the caller
// falls back to the head commit and says so).
func (g *SubprocessGit) MergeCommit(ctx context.Context, repo, base, head string) (string, error) {
	if !shaRe.MatchString(strings.ToLower(strings.TrimSpace(base))) || !shaRe.MatchString(strings.ToLower(strings.TrimSpace(head))) {
		return "", fmt.Errorf("%w: merge-ref inputs must be full commit shas", ErrInvalid)
	}
	base, head = strings.ToLower(strings.TrimSpace(base)), strings.ToLower(strings.TrimSpace(head))
	out, _, err := g.run(ctx, repo, []string{"merge-tree", "--write-tree", base, head})
	if err != nil {
		return "", fmt.Errorf("%w: merge-tree %s..%s: %v", ErrUnprocessable, base, head, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || !shaRe.MatchString(strings.TrimSpace(lines[0])) {
		return "", fmt.Errorf("%w: merge-tree produced no tree", ErrCorrupt)
	}
	tree := strings.TrimSpace(lines[0])
	msg := "Merge " + head + " into " + base
	sha, _, cerr := g.runEnv(ctx, repo, []string{"commit-tree", tree, "-p", base, "-p", head, "-m", msg}, []string{
		"GIT_AUTHOR_NAME=spindle", "GIT_AUTHOR_EMAIL=spindle@localhost",
		"GIT_COMMITTER_NAME=spindle", "GIT_COMMITTER_EMAIL=spindle@localhost",
		"GIT_AUTHOR_DATE=" + mergeCommitEpoch, "GIT_COMMITTER_DATE=" + mergeCommitEpoch,
	})
	if cerr != nil {
		return "", fmt.Errorf("%w: commit-tree %s: %v", ErrUnavailable, tree, cerr)
	}
	sha = strings.TrimSpace(sha)
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("%w: commit-tree produced %q", ErrCorrupt, sha)
	}
	return sha, nil
}

// gitExitError is a non-zero git exit (the object/ref/path did not resolve —
// a caller-visible not-found, never a backend outage).
type gitExitError struct {
	code   int
	stderr string
}

func (e *gitExitError) Error() string {
	return fmt.Sprintf("git exit %d: %s", e.code, e.stderr)
}

// notFoundIfExit maps a non-zero git exit (a missing object/ref/path) onto
// the not-found class; backend failures pass through untouched.
func notFoundIfExit(err error) error {
	var ge *gitExitError
	if errors.As(err, &ge) {
		return fmt.Errorf("%w: %v", ErrNotFound, ge)
	}
	return err
}

// dirOf resolves the repo's synced directory.
func (g *SubprocessGit) dirOf(ctx context.Context, repo string) (string, error) {
	if g.dirs == nil {
		return "", fmt.Errorf("%w: git dirs not wired", ErrUnavailable)
	}
	dir, err := g.dirs.Dir(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("%w: repo unavailable: %v", ErrUnavailable, err)
	}
	return dir, nil
}

// capBuffer keeps at most limit bytes and records truncation. It always
// reports a full write so the child is never killed mid-output.
type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.limit > 0 {
		if c.buf.Len()+len(p) > c.limit {
			keep := c.limit - c.buf.Len()
			if keep > 0 {
				_, _ = c.buf.Write(p[:keep])
			}
			c.truncated = true
			return len(p), nil
		}
	}
	_, _ = c.buf.Write(p)
	return len(p), nil
}

func (c *capBuffer) String() string { return c.buf.String() }

// boundedStderr keeps the TAIL of stderr (8 KiB) for error text.
type boundedStderr struct {
	buf  []byte
	full bool
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	const cap = 8 << 10
	if len(p) >= cap {
		b.buf = append(b.buf[:0], p[len(p)-cap:]...)
		b.full = true
		return len(p), nil
	}
	if len(b.buf)+len(p) > cap {
		b.full = true
		keep := cap - len(p)
		copy(b.buf, b.buf[len(b.buf)-keep:])
		b.buf = b.buf[:keep]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedStderr) String() string {
	if b.full {
		return "…" + string(b.buf)
	}
	return string(b.buf)
}
