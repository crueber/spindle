// migration.go — the migration banner's read model (PAR-003,
// docs/features/21_parity_master.md §2.1).
//
// Threads discovers and runs ONE location: `.spindle/workflows`. A repository
// that still carries `.forgejo/workflows` or `.github/workflows` is therefore
// empty here, and an operator needs to be told why and what to run. This file
// answers that question for the Threads page:
//
//   - it lists the foreign directories' files at the default branch tip, for
//     DISPLAY ONLY — nothing is parsed, nothing is imported, nothing runs;
//   - returning them never creates a run and never feeds discovery: the engine
//     has no code path that reads a foreign directory (PAR-001/§2.1);
//   - the page prints the exact `spindle threads import --from … --repo .`
//     invocation for each directory it found.
//
// Reading a foreign directory is a hint, so a reader that cannot list one
// (an older GitReader) is not an error: the banner simply does not render.

package threads

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
)

// ForeignWorkflowDirs are the two directories the migration banner names, in
// display order. The platform name is what `threads import --from` takes.
var ForeignWorkflowDirs = []struct {
	Platform string
	Dir      string
}{
	{Platform: "forgejo", Dir: ".forgejo/workflows"},
	{Platform: "github", Dir: ".github/workflows"},
}

// ForeignWorkflows is the banner's read model: the foreign files found at one
// commit, grouped by source platform. Empty means no banner.
type ForeignWorkflows struct {
	// Forgejo/GitHub are repository-relative file paths (sorted).
	Forgejo []string `json:"forgejo,omitempty"`
	GitHub  []string `json:"github,omitempty"`
}

// Empty reports whether there is nothing to tell the reader.
func (f *ForeignWorkflows) Empty() bool {
	return f == nil || (len(f.Forgejo) == 0 && len(f.GitHub) == 0)
}

// TreeLister is the optional GitReader extension the banner uses. It is a
// separate interface so every test fake (and any reader without it) keeps
// compiling; a reader without it renders no banner rather than failing a page.
type TreeLister interface {
	// ListDir returns the direct children of one directory at one commit
	// (repository-relative, sorted). A missing directory is empty, not an
	// error.
	ListDir(ctx context.Context, repo, commit, dir string) ([]string, error)
}

// ListForeignWorkflows lists the foreign workflow files at the default branch
// tip for the migration banner. It requires only the repository read gate — the
// content is already readable to the caller — and never writes or runs
// anything. A repository with any `.spindle/workflows` file is the caller's
// business: the page only asks when that set is empty.
func (s *Service) ListForeignWorkflows(ctx context.Context, repo, commit string, p auth.Principal) (*ForeignWorkflows, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	if s.Git == nil {
		return nil, nil // no reader: no hint, never a broken page
	}
	lister, ok := s.Git.(TreeLister)
	if !ok {
		return nil, nil
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		resolved, rerr := s.Git.ResolveRef(ctx, repo, "HEAD")
		if rerr != nil {
			if errors.Is(rerr, ErrNotFound) {
				return nil, nil // an empty repository has no foreign files
			}
			return nil, rerr
		}
		commit = resolved
	} else if _, nerr := normalizeSHA(commit); nerr != nil {
		return nil, nerr
	}
	out := &ForeignWorkflows{}
	for _, dir := range ForeignWorkflowDirs {
		entries, lerr := lister.ListDir(ctx, repo, commit, dir.Dir)
		if lerr != nil {
			if errors.Is(lerr, ErrNotFound) {
				continue
			}
			return nil, lerr
		}
		var files []string
		for _, entry := range entries {
			if !foreignWorkflowPath(entry, dir.Dir) {
				continue
			}
			files = append(files, entry)
		}
		sort.Strings(files)
		switch dir.Platform {
		case "forgejo":
			out.Forgejo = files
		case "github":
			out.GitHub = files
		}
	}
	return out, nil
}

// foreignWorkflowPath reports whether p is a direct child of dir with a YAML
// extension. The listing is asked for the directory, so this only rejects
// nested entries and non-YAML names — the same shape discovery accepts, so the
// banner never names a file `threads import` would skip.
func foreignWorkflowPath(p, dir string) bool {
	prefix := dir + "/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return false
	}
	ext := path.Ext(rest)
	return ext == ".yml" || ext == ".yaml"
}

// ImportCommand is the exact invocation the banner prints for one platform
// directory (docs/CLI.md §5.3): the directory is the default source, so no
// --path is needed.
func ImportCommand(platform string) string {
	return "spindle threads import --from " + platform + " --repo ."
}
