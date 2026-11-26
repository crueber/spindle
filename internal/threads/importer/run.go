package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options is one `spindle threads import` invocation.
type Options struct {
	// Repo is the repository checkout to read.
	Repo string
	// Platform selects the dialect.
	Platform Platform
	// Path overrides discovery with one file or directory (repo-relative, or
	// absolute inside the repository).
	Path string
	// Out writes the converted files under this directory instead of stdout.
	Out string
	// Apply commits the converted files on a new branch.
	Apply bool
	// Branch names that branch; empty means
	// `spindle/import-<platform>-<UTC timestamp>`.
	Branch string
	// AllowPartial permits --apply when constructs were refused.
	AllowPartial bool
}

// gitBin is the git binary. A test may replace it to exercise the error paths.
var gitBin = "git"

// nowUTC is the clock for the default branch name. A test may replace it.
var nowUTC = func() time.Time { return time.Now().UTC() }

// BranchPrefix is the namespace of the branches --apply creates.
const BranchPrefix = "spindle/import-"

// DefaultBranch is the branch name for a run: deterministic given the clock,
// prefixed so the branch is obviously machine-made, and never a push target
// (nothing here pushes at all).
func DefaultBranch(platform Platform) string {
	return BranchPrefix + string(platform) + "-" + nowUTC().Format("20060102T150405Z")
}

// outputFile is one converted file waiting to be written.
type outputFile struct {
	report FileReport
	data   []byte
}

// Run imports a repository's workflows. It never edits the source directory: a
// dry run writes nothing, --out writes outside the repository, and --apply
// writes the target files on a new branch and commits them.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if _, ok := profiles[opts.Platform]; !ok {
		return nil, fmt.Errorf("unknown platform %q", opts.Platform)
	}
	if strings.TrimSpace(opts.Repo) == "" {
		return nil, errors.New("a repository directory is required (--repo)")
	}
	if opts.Apply && opts.Out != "" {
		return nil, errors.New("--apply and --out are mutually exclusive")
	}
	report := &Report{Platform: opts.Platform, Repo: opts.Repo, Outputs: map[string][]byte{}}
	sources, err := Discover(opts.Repo, opts.Platform, opts.Path)
	if err != nil {
		return report, err
	}
	var files []outputFile
	seen := map[string]string{}
	for _, src := range sources {
		raw, err := readCapped(src.Abs)
		if err != nil {
			return report, fmt.Errorf("%s: %w", src.Rel, err)
		}
		out, err := Translate(opts.Platform, raw)
		if err != nil {
			return report, fmt.Errorf("%s: %w", src.Rel, err)
		}
		target, err := OutputPath(src.Rel)
		if err != nil {
			return report, fmt.Errorf("%s: %w", src.Rel, err)
		}
		if prev, dup := seen[target]; dup {
			return report, fmt.Errorf("%s and %s both convert to %s; rename one source file", prev, src.Rel, target)
		}
		seen[target] = src.Rel
		fr := FileReport{
			Source:       src.Rel,
			Output:       target,
			Translated:   out.Translated,
			Parses:       true,
			Untranslated: out.Untranslated,
			Notes:        out.Notes,
		}
		report.Files = append(report.Files, fr)
		if len(out.Untranslated) > 0 {
			report.Partial = true
		}
		report.Outputs[target] = out.Output
		files = append(files, outputFile{report: fr, data: out.Output})
	}
	switch {
	case opts.Apply:
		if !report.OK() && !opts.AllowPartial {
			return report, fmt.Errorf("%d untranslated construct(s) across %d file(s); review the report, fix the source, or pass --allow-partial",
				report.Untranslated(), len(report.Files))
		}
		branch := strings.TrimSpace(opts.Branch)
		if branch == "" {
			branch = DefaultBranch(opts.Platform)
		}
		repo, err := inspectRepo(ctx, opts.Repo)
		if err != nil {
			return report, err
		}
		if err := commitOnBranch(ctx, repo, branch, files, opts.Platform); err != nil {
			return report, err
		}
		report.Applied = true
		report.Branch = branch
	case opts.Out != "":
		for _, f := range files {
			if err := writeOutput(opts.Out, f.report.Output, f.data); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

// readCapped reads a source file with the size cap applied.
func readCapped(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSourceBytes {
		return nil, fmt.Errorf("%w: the file is larger than %d bytes", ErrUnconvertible, MaxSourceBytes)
	}
	return data, nil
}

// writeOutput writes one converted file under base, refusing an escape or a
// symlink at the target.
func writeOutput(base, rel string, data []byte) error {
	abs := filepath.Join(base, filepath.FromSlash(rel))
	if _, err := safeRel(base, abs); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; refusing to write through it", rel)
	}
	return os.WriteFile(abs, data, 0o644)
}

// --- git ------------------------------------------------------------------

// gitRepo is the checkout's state at the moment --apply starts.
type gitRepo struct {
	top      string
	branch   string // current branch, empty when HEAD is detached
	detached bool
	head     string // commit HEAD names (for the detached rollback)
}

// inspectRepo validates that dir is the root of a clean git work tree. A dirty
// tree is refused because --apply creates a commit: an operator's uncommitted
// work must never end up in it (and the rollback path could not restore it).
func inspectRepo(ctx context.Context, dir string) (*gitRepo, error) {
	top, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git work tree: %v", dir, err)
	}
	top = strings.TrimSpace(top)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	realTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		return nil, err
	}
	if realDir != realTop {
		return nil, fmt.Errorf("--apply must run from the repository root (%s), not %s", realTop, realDir)
	}
	status, err := gitOut(ctx, realTop, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %v", err)
	}
	if strings.TrimSpace(status) != "" {
		return nil, fmt.Errorf("the working tree has uncommitted changes; commit or stash them before --apply (import must not mix with your work)")
	}
	repo := &gitRepo{top: realTop}
	repo.head, err = gitOut(ctx, realTop, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git rev-parse HEAD: %v", err)
	}
	repo.head = strings.TrimSpace(repo.head)
	branch, err := gitOut(ctx, realTop, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		repo.detached = true
	} else {
		repo.branch = strings.TrimSpace(branch)
	}
	return repo, nil
}

// commitOnBranch creates branch, writes files, and commits them. Nothing is
// pushed and nothing is ever forced. A failure after the branch is created
// restores the original checkout and deletes the branch.
func commitOnBranch(ctx context.Context, repo *gitRepo, branch string, files []outputFile, platform Platform) error {
	if branch == repo.branch && !repo.detached {
		return fmt.Errorf("--branch %q is the current branch; a converted file is committed on a new branch, never the current one", branch)
	}
	if out, err := gitOut(ctx, repo.top, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch name %q: %s", branch, strings.TrimSpace(out))
	}
	if _, err := gitOut(ctx, repo.top, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("branch %q already exists; choose another --branch (nothing is overwritten or force-pushed)", branch)
	}
	if _, err := gitOut(ctx, repo.top, "switch", "-c", branch); err != nil {
		return fmt.Errorf("git switch -c %s: %v", branch, err)
	}
	rollback := func() {
		for _, f := range files {
			_ = os.Remove(filepath.Join(repo.top, filepath.FromSlash(f.report.Output)))
		}
		if repo.detached {
			_, _ = gitOut(ctx, repo.top, "switch", "--detach", repo.head)
		} else {
			_, _ = gitOut(ctx, repo.top, "switch", repo.branch)
		}
		_, _ = gitOut(ctx, repo.top, "branch", "-D", branch)
	}
	for _, f := range files {
		if err := writeOutput(repo.top, f.report.Output, f.data); err != nil {
			rollback()
			return err
		}
	}
	args := []string{"add", "--"}
	for _, f := range files {
		args = append(args, f.report.Output)
	}
	if _, err := gitOut(ctx, repo.top, args...); err != nil {
		rollback()
		return fmt.Errorf("git add: %v", err)
	}
	if _, err := gitOut(ctx, repo.top, "commit", "-m", commitMessage(platform, files)); err != nil {
		rollback()
		return fmt.Errorf("git commit: %v (is user.name/user.email configured?)", err)
	}
	return nil
}

// commitMessage names every conversion and the untranslated count, so the
// branch explains itself in the log.
func commitMessage(platform Platform, files []outputFile) string {
	var b strings.Builder
	b.WriteString("threads: import ")
	b.WriteString(string(platform))
	b.WriteString(" workflows\n\nConverted with `spindle threads import --from ")
	b.WriteString(string(platform))
	b.WriteString("`:\n")
	for _, f := range files {
		b.WriteString("- ")
		b.WriteString(f.report.Source)
		b.WriteString(" -> ")
		b.WriteString(f.report.Output)
		if n := len(f.report.Untranslated); n > 0 {
			fmt.Fprintf(&b, " (%d untranslated)", n)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUntranslated constructs are marked `# UNTRANSLATED:` in the converted files;\n")
	b.WriteString("the source directories were read only and are unchanged.\n")
	return b.String()
}

// gitOut runs one git command in dir and returns its stdout. stderr is included
// in the error so a refusal names git's own reason.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBin, append([]string{"-C", dir}, args...)...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}
