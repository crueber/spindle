package webedit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GitRunner is the git subprocess seam (docs/go/04_git.md §15: exact argv,
// never a Go git library). Production is SubprocessGit; tests substitute a
// fake. Every method shells out to the `git` binary through the bounded pool.
type GitRunner interface {
	// ResolveBranch resolves refs/heads/<name> to its commit sha.
	ResolveBranch(ctx context.Context, dir, branch string) (string, error)
	// LookupFile reads one path at a commit. Absent path ⇒ ErrNotFound.
	LookupFile(ctx context.Context, dir, commit, path string) (*File, error)
	// RootFile reads a file at the commit root (the .gitattributes guard);
	// ok=false when absent.
	RootFile(ctx context.Context, dir, commit, name string) ([]byte, bool, error)
	// HashBlob writes content as a blob and returns its oid.
	HashBlob(ctx context.Context, dir string, content []byte) (string, error)
	// BuildCommit mints the commit from plumbing only and returns
	// (commit, tree). A no-op change returns ErrNoChange.
	BuildCommit(ctx context.Context, dir string, in CommitBuild) (string, string, error)
	// DiffTree renders the pending patch between a commit and a tree.
	DiffTree(ctx context.Context, dir, baseCommit, tree string) (string, error)
	// PackTip packs every object reachable from tip and no existing ref.
	// Empty when tip contributes nothing new.
	PackTip(ctx context.Context, dir, tip string) ([]byte, error)
}

// CommitBuild is one plumbing commit request.
type CommitBuild struct {
	Base       string // parent commit sha
	Path       string
	BlobSHA    string // "" for a delete
	Delete     bool
	Mode       string // git file mode for create/edit ("100644" default)
	Message    string
	AuthorName string
	AuthorMail string
	When       time.Time
}

// SubprocessGit is the production runner.
type SubprocessGit struct {
	Binary  string
	Pool    *gitPool
	Timeout time.Duration
}

// NewSubprocessGit builds the production runner.
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	return &SubprocessGit{Binary: binary, Pool: newGitPool(0), Timeout: 120 * time.Second}
}

type gitPool struct{ sem chan struct{} }

func newGitPool(capacity int) *gitPool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &gitPool{sem: make(chan struct{}, capacity)}
}

func (p *gitPool) run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

type boundedStderr struct{ buf bytes.Buffer }

func (b *boundedStderr) Write(p []byte) (int, error) {
	room := 8192 - b.buf.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		p = p[:room]
	}
	return b.buf.Write(p)
}

func (b *boundedStderr) String() string { return b.buf.String() }

// gitExecError carries a non-zero git exit, so callers can tell "absent"
// (ls-tree empty) from a backend failure.
type gitExecError struct {
	argv   []string
	stderr string
	code   int
}

func (e *gitExecError) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(e.argv, " "), strings.TrimSpace(e.stderr))
}

func (g *SubprocessGit) timeoutFor(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, g.Timeout)
}

// run executes argv in dir, feeds stdin, and returns stdout. extraEnv entries
// are appended after the base environment.
func (g *SubprocessGit) run(ctx context.Context, dir string, argv []string, stdin []byte, extraEnv []string) ([]byte, error) {
	pool := g.Pool
	if pool == nil {
		pool = newGitPool(0)
	}
	var out []byte
	var runErr error
	var stderr bytes.Buffer
	perr := pool.run(ctx, func() error {
		cctx, cancel := g.timeoutFor(ctx)
		defer cancel()
		cmd := exec.CommandContext(cctx, g.Binary, argv...)
		cmd.Dir = dir
		env := []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
		env = append(env, extraEnv...)
		cmd.Env = env
		if len(stdin) > 0 {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var buf bytes.Buffer
		var eb boundedStderr
		cmd.Stdout = &buf
		cmd.Stderr = &eb
		runErr = cmd.Run()
		out = buf.Bytes()
		stderr = bytes.Buffer{}
		stderr.WriteString(eb.String())
		return runErr
	})
	if perr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out, &gitExecError{argv: argv, stderr: stderr.String(), code: exitErr.ExitCode()}
		}
		return nil, fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], runErr)
	}
	return out, nil
}

// ResolveBranch resolves refs/heads/<branch> to a commit sha.
func (g *SubprocessGit) ResolveBranch(ctx context.Context, dir, branch string) (string, error) {
	ref, name, err := normalizeBranchRef(branch)
	if err != nil {
		return "", err
	}
	out, rerr := g.run(ctx, dir, []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}, nil, nil)
	if rerr != nil {
		var ge *gitExecError
		if errors.As(rerr, &ge) {
			return "", fmt.Errorf("%w: unknown branch %q", ErrNotFound, name)
		}
		return "", rerr
	}
	sha := strings.TrimSpace(string(out))
	if !isHexSHA(sha) {
		return "", fmt.Errorf("%w: rev-parse produced %q", ErrUnavailable, sha)
	}
	return sha, nil
}

// LookupFile reads path at commit. A tree at the path is ErrInvalid (the
// editor edits files). Modes 120000/160000 (symlink/submodule) are
// ErrUnsupported: a web edit must not silently retype them as regular files.
func (g *SubprocessGit) LookupFile(ctx context.Context, dir, commit, p string) (*File, error) {
	if err := ValidatePath(p); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, dir, []string{"ls-tree", "-z", commit, "--", p}, nil, nil)
	if err != nil {
		return nil, err
	}
	entry, ok := parseLSTreeZ(out)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if entry.typ == "tree" {
		return nil, fmt.Errorf("%w: %q is a directory", ErrInvalid, p)
	}
	if entry.mode == "120000" || entry.mode == "160000" {
		return nil, fmt.Errorf("%w: %q is a symlink or submodule; edit it from a clone", ErrUnsupported, p)
	}
	sizeOut, err := g.run(ctx, dir, []string{"cat-file", "-s", entry.oid}, nil, nil)
	if err != nil {
		return nil, err
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	body, err := g.run(ctx, dir, []string{"cat-file", "blob", entry.oid}, nil, nil)
	if err != nil {
		return nil, err
	}
	return &File{
		Path: p, SHA: entry.oid, Mode: entry.mode, Size: size, Content: body,
		Binary: isBinary(body), LFS: isLFSPointer(body),
	}, nil
}

type lsTreeEntry struct {
	mode string
	typ  string
	oid  string
}

// parseLSTreeZ parses one `ls-tree -z` row.
func parseLSTreeZ(out []byte) (lsTreeEntry, bool) {
	fields := bytes.SplitN(out, []byte{0}, 2)
	out = fields[0]
	tab := bytes.IndexByte(out, '\t')
	if tab < 0 {
		return lsTreeEntry{}, false
	}
	meta := strings.Fields(string(out[:tab]))
	if len(meta) != 3 {
		return lsTreeEntry{}, false
	}
	return lsTreeEntry{mode: meta[0], typ: meta[1], oid: meta[2]}, true
}

// RootFile reads a root-level file at commit (the .gitattributes guard).
func (g *SubprocessGit) RootFile(ctx context.Context, dir, commit, name string) ([]byte, bool, error) {
	f, err := g.LookupFile(ctx, dir, commit, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f.Content, true, nil
}

// HashBlob writes content as a blob and returns its oid.
func (g *SubprocessGit) HashBlob(ctx context.Context, dir string, content []byte) (string, error) {
	out, err := g.run(ctx, dir, []string{"hash-object", "-w", "--stdin"}, content, nil)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !isHexSHA(sha) {
		return "", fmt.Errorf("%w: hash-object produced %q", ErrUnavailable, sha)
	}
	return sha, nil
}

// BuildCommit mints the commit from plumbing only, in a request-scoped
// temporary index. The index file lives in a unique scratch dir that is
// removed on every exit path.
func (g *SubprocessGit) BuildCommit(ctx context.Context, dir string, in CommitBuild) (string, string, error) {
	if !isHexSHA(in.Base) {
		return "", "", fmt.Errorf("%w: invalid base commit", ErrInvalid)
	}
	if err := ValidatePath(in.Path); err != nil {
		return "", "", err
	}
	scratch, err := os.MkdirTemp("", "spindle-webedit-index-*")
	if err != nil {
		return "", "", fmt.Errorf("%w: index scratch: %v", ErrUnavailable, err)
	}
	defer os.RemoveAll(scratch)
	idx := filepath.Join(scratch, "index")
	// GIT_WORK_TREE points at the empty scratch dir: the index plumbing runs
	// against a bare repo, and `update-index --force-remove` refuses to run
	// without a worktree at all (it stats the index entry's path). The
	// scratch dir is empty, so no file is ever read or written.
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_WORK_TREE=" + scratch}

	if _, err := g.run(ctx, dir, []string{"read-tree", in.Base}, nil, env); err != nil {
		return "", "", fmt.Errorf("read-tree: %w", err)
	}
	if in.Delete {
		if _, err := g.run(ctx, dir, []string{"update-index", "--force-remove", "--", in.Path}, nil, env); err != nil {
			return "", "", fmt.Errorf("update-index remove: %w", err)
		}
	} else {
		if !isHexSHA(in.BlobSHA) {
			return "", "", fmt.Errorf("%w: invalid blob oid", ErrInvalid)
		}
		mode := in.Mode
		switch mode {
		case "100644", "100755":
		default:
			mode = "100644"
		}
		spec := mode + "," + in.BlobSHA + "," + in.Path
		if _, err := g.run(ctx, dir, []string{"update-index", "--add", "--cacheinfo", spec}, nil, env); err != nil {
			return "", "", fmt.Errorf("update-index add: %w", err)
		}
	}
	treeOut, err := g.run(ctx, dir, []string{"write-tree"}, nil, env)
	if err != nil {
		return "", "", fmt.Errorf("write-tree: %w", err)
	}
	tree := strings.TrimSpace(string(treeOut))
	if !isHexSHA(tree) {
		return "", "", fmt.Errorf("%w: write-tree produced %q", ErrUnavailable, tree)
	}
	// No-op guard: an unchanged tree means nothing to commit (F.4 → 422).
	_, derr := g.run(ctx, dir, []string{"diff-tree", "--quiet", in.Base, tree}, nil, nil)
	if derr == nil {
		return "", "", fmt.Errorf("%w: file content is unchanged", ErrNoChange)
	}
	var ge *gitExecError
	if !errors.As(derr, &ge) || ge.code != 1 {
		return "", "", fmt.Errorf("diff-tree: %w", derr)
	}
	stamp := strconv.FormatInt(in.When.Unix(), 10) + " +0000"
	identEnv := []string{
		"GIT_AUTHOR_NAME=" + sanitizeIdent(in.AuthorName),
		"GIT_AUTHOR_EMAIL=" + sanitizeIdent(in.AuthorMail),
		"GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=" + committerName,
		"GIT_COMMITTER_EMAIL=" + committerEmail,
		"GIT_COMMITTER_DATE=" + stamp,
	}
	out, err := g.run(ctx, dir, []string{"commit-tree", tree, "-p", in.Base, "-m", in.Message}, nil, identEnv)
	if err != nil {
		return "", "", fmt.Errorf("commit-tree: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	if !isHexSHA(commit) {
		return "", "", fmt.Errorf("%w: commit-tree produced %q", ErrUnavailable, commit)
	}
	return commit, tree, nil
}

// DiffTree renders the pending patch between a commit and a tree.
func (g *SubprocessGit) DiffTree(ctx context.Context, dir, baseCommit, tree string) (string, error) {
	out, err := g.run(ctx, dir, []string{"diff-tree", "-p", "--no-color", "--no-commit-id", baseCommit, tree}, nil, nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// PackTip packs every object reachable from tip and no existing ref. Caret
// negation rides stdin (pack-objects takes no --not/--all flags; the
// merge-task precedent in internal/pulls uses the same shape).
func (g *SubprocessGit) PackTip(ctx context.Context, dir, tip string) ([]byte, error) {
	if !isHexSHA(tip) {
		return nil, fmt.Errorf("%w: invalid tip", ErrInvalid)
	}
	refsOut, err := g.run(ctx, dir, []string{"for-each-ref", "--format=%(refname)"}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %w", err)
	}
	var stdin strings.Builder
	stdin.WriteString(tip + "\n")
	for _, ref := range strings.Split(strings.TrimSpace(string(refsOut)), "\n") {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		stdin.WriteString("^" + ref + "\n")
	}
	out, err := g.run(ctx, dir, []string{"pack-objects", "--revs", "--stdout"}, []byte(stdin.String()), nil)
	if err != nil {
		return nil, fmt.Errorf("pack-objects: %w", err)
	}
	return out, nil
}

// sanitizeIdent strips bytes that would break the identity header.
func sanitizeIdent(s string) string {
	s = strings.ReplaceAll(s, "<", "")
	s = strings.ReplaceAll(s, ">", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return "spindle"
	}
	return s
}

// isHexSHA accepts a 40/64 lowercase hex oid.
func isHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
