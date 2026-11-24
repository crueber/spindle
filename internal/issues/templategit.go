package issues

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"
)

// templategit.go is the stock-git read path for issue-template discovery
// (docs/go/04_git.md §14b): the repo's serving copy, read-only, exact argv,
// bounded semaphore + ctx timeout. It runs only from the post-push hook, NEVER
// from a composer/API request (law 6) — the request path reads the cached
// projection in meta/issue_templates.json instead.

// TemplateGit is the narrow git-read seam the template refresh consumes.
// Composition binds SubprocessTemplateGit; tests substitute a map-backed fake.
type TemplateGit interface {
	// ListTemplateFiles returns the repository-relative paths of
	// `.spindle/ISSUE_TEMPLATE/*.md` at commit, sorted. A missing directory
	// is empty, not an error.
	ListTemplateFiles(ctx context.Context, repo, commit string) ([]string, error)
	// ReadFile returns one path's bytes at commit.
	ReadFile(ctx context.Context, repo, commit, p string) ([]byte, error)
	// ResolveRef resolves a ref (HEAD for the default branch) to a commit sha.
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
}

// TemplateDir is the only directory discovery reads. GitHub's `.github` tree
// and YAML issue *forms* are deliberately not implemented (02 §15).
const TemplateDir = ".spindle/ISSUE_TEMPLATE"

// maxTemplateFileBytes bounds one template file read (the body is also capped
// at MaxBodyBytes by the composer contract).
const maxTemplateFileBytes = 128 << 10

// TemplateDirs resolves a repo id ("owner/name") to its synced serving copy.
// Composition injects the WAL-registry adapter; nil leaves the reader
// unavailable (tests only).
type TemplateDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// SubprocessTemplateGit is the production TemplateGit: stock git, the §14b
// argv, GIT_DIR set, GIT_TERMINAL_PROMPT=0, ctx-bounded, bounded stdout and
// stderr, own semaphore (never shares control-plane capacity with pushes).
type SubprocessTemplateGit struct {
	Binary  string
	Timeout time.Duration
	Dirs    TemplateDirs
	sem     chan struct{}
}

// NewSubprocessTemplateGit builds the reader (binary defaults to "git").
func NewSubprocessTemplateGit(binary string, dirs TemplateDirs) *SubprocessTemplateGit {
	if binary == "" {
		binary = "git"
	}
	capacity := 4 * runtime.GOMAXPROCS(0)
	if capacity <= 0 {
		capacity = 4
	}
	return &SubprocessTemplateGit{Binary: binary, Timeout: 30 * time.Second, Dirs: dirs, sem: make(chan struct{}, capacity)}
}

// errTemplateNotFound classifies a non-zero git exit (a missing path is not a
// 503; callers skip the file).
var errTemplateNotFound = errors.New("template path not found")

// run executes one argv in the repo's serving copy under the semaphore.
func (g *SubprocessTemplateGit) run(ctx context.Context, repo string, args ...string) ([]byte, error) {
	if g.Dirs == nil {
		return nil, fmt.Errorf("issue templates: no repository directory resolver")
	}
	dir, err := g.Dirs.Dir(ctx, repo)
	if err != nil {
		return nil, err
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-g.sem }()

	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.Binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR="+dir)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() != nil {
			return nil, cctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%w: %s", errTemplateNotFound, strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// ListTemplateFiles runs `git ls-tree -z --name-only -r <commit> -- <dir>`.
func (g *SubprocessTemplateGit) ListTemplateFiles(ctx context.Context, repo, commit string) ([]string, error) {
	if !templateSHA(commit) {
		return nil, fmt.Errorf("%w: invalid commit", ErrInvalid)
	}
	out, err := g.run(ctx, repo, "ls-tree", "-z", "--name-only", "-r", commit, "--", TemplateDir)
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			return nil, nil // missing directory = no templates
		}
		return nil, err
	}
	paths := make([]string, 0, 8)
	for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if templatePath(p) {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// ReadFile runs `git show <commit>:<path>` under the file cap.
func (g *SubprocessTemplateGit) ReadFile(ctx context.Context, repo, commit, p string) ([]byte, error) {
	if !templateSHA(commit) || p == "" || strings.HasPrefix(p, "-") || strings.Contains(p, ":") {
		return nil, fmt.Errorf("%w: invalid template path %q", ErrInvalid, p)
	}
	out, err := g.run(ctx, repo, "show", commit+":"+p)
	if err != nil {
		return nil, err
	}
	if len(out) > maxTemplateFileBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalid, p, maxTemplateFileBytes)
	}
	return out, nil
}

// ResolveRef runs `git rev-parse --verify --quiet <ref>^{commit}`.
func (g *SubprocessTemplateGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("%w: invalid ref %q", ErrInvalid, ref)
	}
	out, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			return "", fmt.Errorf("%w: unknown ref %q", ErrNotFound, ref)
		}
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !templateSHA(sha) {
		return "", fmt.Errorf("%w: bad commit %q", ErrInvalid, sha)
	}
	return sha, nil
}

// templateSHA reports a 40/64 lowercase-hex object id.
func templateSHA(v string) bool {
	if len(v) != 40 && len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// templatePath accepts a direct child of TemplateDir with a .md extension.
func templatePath(p string) bool {
	if path.Dir(p) != TemplateDir {
		return false
	}
	return strings.HasSuffix(strings.ToLower(p), ".md")
}
