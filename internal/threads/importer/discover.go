package importer

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// ErrNoWorkflows means the source directory exists but holds no `.yml`/`.yaml`
// file. It is an error rather than an empty success: an import that finds
// nothing is almost always the wrong --from or --repo.
var ErrNoWorkflows = errors.New("importer: no workflow files found")

// Source is one source workflow file, discovered but not yet read.
type Source struct {
	// Rel is the repo-relative path with '/' separators.
	Rel string
	// Abs is the resolved absolute path.
	Abs string
}

// Discover lists the workflow files to import. With no path it reads the
// platform's workflow directory; an explicit path may name one file or a
// directory. Both are resolved (symlinks included) and must stay inside root:
// a repository is untrusted input, and a link out of it is a read the importer
// will not perform.
func Discover(root string, platform Platform, sub string) ([]Source, error) {
	prof, ok := profiles[platform]
	if !ok {
		return nil, fmt.Errorf("unknown platform %q", platform)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	info, err := os.Stat(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNoWorkflows, root)
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	target := sub
	if strings.TrimSpace(target) == "" {
		target = prof.dir
	}
	if filepath.IsAbs(target) {
		target = filepath.FromSlash(target)
	} else {
		target = filepath.Join(resolvedRoot, filepath.FromSlash(target))
	}
	rel, err := safeRel(resolvedRoot, target)
	if err != nil {
		return nil, err
	}
	info, err = os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	if !info.IsDir() {
		abs, err := safeAbs(resolvedRoot, target)
		if err != nil {
			return nil, err
		}
		return []Source{{Rel: rel, Abs: abs}}, nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	var out []Source
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".yml" && ext != ".yaml" {
			continue
		}
		abs, err := safeAbs(resolvedRoot, filepath.Join(target, name))
		if err != nil {
			return nil, err
		}
		out = append(out, Source{Rel: path.Join(rel, name), Abs: abs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	if len(out) == 0 {
		return nil, fmt.Errorf("%w under %s", ErrNoWorkflows, rel)
	}
	return out, nil
}

// safeAbs resolves target and requires it to stay inside root.
func safeAbs(root, target string) (string, error) {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoWorkflows, err)
	}
	if _, err := safeRel(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// safeRel returns target relative to root, refusing anything that escapes it.
// It compares resolved paths, so a symlink pointing outside the repository is
// caught even when its name looks local. A path that does not exist yet (an
// --out directory, a file about to be written) is resolved as far as it can be:
// its parent is resolved, so a symlinked directory cannot be used to write
// outside either.
func safeRel(root, target string) (string, error) {
	resolvedRoot, ok := resolvePath(root)
	if !ok {
		return "", fmt.Errorf("%w: cannot resolve %s", ErrNoWorkflows, root)
	}
	resolved, ok := resolvePath(target)
	if !ok {
		return "", fmt.Errorf("%w: cannot resolve %s", ErrNoWorkflows, target)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %s is outside %s", ErrNoWorkflows, target, root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %s escapes the repository root %s", ErrNoWorkflows, target, root)
	}
	return filepath.ToSlash(rel), nil
}

// resolvePath resolves a path that may not exist yet: it finds the deepest
// existing ancestor, resolves that (so a symlinked directory is followed) and
// appends the remaining segments.
func resolvePath(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	cur := filepath.Clean(abs)
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// OutputPath is the target path for a source: `.spindle/workflows/<name>.yml`,
// one flat directory (the only location the engine discovers). A `.yaml` source
// is normalized to `.yml` so the output name is stable.
func OutputPath(sourceRel string) (string, error) {
	base := path.Base(filepath.ToSlash(sourceRel))
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: cannot name an output for %s", ErrUnconvertible, sourceRel)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("%w: %s has a path separator in its name", ErrUnconvertible, sourceRel)
	}
	return workflow.Dir + "/" + name + ".yml", nil
}
