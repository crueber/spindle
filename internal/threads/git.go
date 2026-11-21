package threads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

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
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
	stdout := &capBuffer{limit: maxWorkflowBytes + 1}
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
