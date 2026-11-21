package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// GitReader reads repository content at a commit. It is the narrow seam
// composition binds to a stock-git subprocess (the argv lives in
// docs/go/04_git.md §14 — git is always a subprocess, law 2). Tests substitute
// a map-backed fake; internal/threads never links a git library.
type GitReader interface {
	// ListWorkflowFiles returns the repository-relative paths of the workflow
	// files (.spindle/workflows/*.yml|*.yaml) at commit, sorted.
	ListWorkflowFiles(ctx context.Context, repo, commit string) ([]string, error)
	// ReadFile returns the bytes of one path at commit. A missing path is an
	// error, never empty bytes.
	ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error)
	// ResolveRef resolves a branch, tag or sha to a full commit sha.
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
}

// WorkflowFile is one parsed workflow file at the selected revision.
type WorkflowFile struct {
	// Path is repository-relative (".spindle/workflows/ci.yml").
	Path string
	// Name is the display name: the YAML `name`, falling back to the
	// basename without its extension.
	Name string
	// Digest is the sha256 of the file bytes (the run header pins it, so a
	// run can be audited against the exact workflow content).
	Digest string
	// Workflow is the parsed, validated document.
	Workflow *workflow.Workflow
}

// maxWorkflowBytes bounds one workflow file (a control-plane object, never a
// build input; 1 MiB is far more than any executable workflow needs).
const maxWorkflowBytes = 1 << 20

// Discover reads, parses and validates every workflow file at commit. A missing
// directory is not an error (nil, nil — the no-workflow repo is a silent
// no-op), but a malformed file IS a visible error: a workflow that cannot
// execute must never enqueue a job (15 §1). The error names the file and
// wraps ErrUnprocessable.
func Discover(ctx context.Context, g GitReader, repo, commit string) ([]*WorkflowFile, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	paths, err := g.ListWorkflowFiles(ctx, repo, commit)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnprocessable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: workflow discovery: %v", ErrUnavailable, err)
	}
	files := make([]*WorkflowFile, 0, len(paths))
	for _, p := range paths {
		if !workflowPath(p) {
			continue
		}
		raw, rerr := g.ReadFile(ctx, repo, commit, p)
		if rerr != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrUnavailable, p, rerr)
		}
		if len(raw) > maxWorkflowBytes {
			return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrUnprocessable, p, maxWorkflowBytes)
		}
		w, perr := workflow.Parse(raw)
		if perr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrUnprocessable, p, perr)
		}
		sum := sha256.Sum256(raw)
		files = append(files, &WorkflowFile{
			Path:     p,
			Name:     workflowName(p, w),
			Digest:   hex.EncodeToString(sum[:]),
			Workflow: w,
		})
	}
	if len(files) == 0 {
		return nil, nil // no workflow files: the no-workflow repo is a no-op
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// workflowPath reports whether p is a direct child of the workflow directory
// with a YAML extension. `ls-tree` is asked for the directory, so this only
// rejects nested files and non-YAML names.
func workflowPath(p string) bool {
	if !strings.HasPrefix(p, workflow.Dir+"/") {
		return false
	}
	rest := strings.TrimPrefix(p, workflow.Dir+"/")
	if rest == "" || strings.Contains(rest, "/") {
		return false
	}
	ext := path.Ext(rest)
	return ext == ".yml" || ext == ".yaml"
}

// workflowName prefers the YAML name, then the file stem.
func workflowName(p string, w *workflow.Workflow) string {
	if name := strings.TrimSpace(w.Name); name != "" {
		return name
	}
	base := path.Base(p)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
}

// SelectFiles applies the trigger match (15 §1: push, pull_request,
// workflow_dispatch; schedule is the maintainer's job and never fires here).
// branch is the push branch or the PR target branch; an empty branch makes a
// branch-filtered trigger match everything (workflow.Fires' documented
// empty-filter rule). want restricts a dispatch to one file by basename or
// repository-relative path.
func SelectFiles(files []*WorkflowFile, event, branch, want string) []*WorkflowFile {
	out := make([]*WorkflowFile, 0, len(files))
	for _, f := range files {
		if !f.Workflow.Fires(event, branch) {
			continue
		}
		if want != "" && !matchesWorkflow(f, want) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// matchesWorkflow accepts a basename ("ci.yml") or the repository-relative
// path (".spindle/workflows/ci.yml").
func matchesWorkflow(f *WorkflowFile, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	return want == f.Path || want == path.Base(f.Path)
}
