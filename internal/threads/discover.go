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

// WorkflowFile is one workflow file at the selected revision. A file that does
// not parse is still a WorkflowFile: Error carries the reason and Workflow is
// nil. Parsing is per-file, so one bad file never disables the others (15
// §2.1: the all-or-nothing failure mode is a bug, not a policy).
type WorkflowFile struct {
	// Path is repository-relative (".spindle/workflows/ci.yml").
	Path string
	// Name is the display name: the YAML `name`, falling back to the
	// basename without its extension (the stem when the file is broken).
	Name string
	// Digest is the sha256 of the file bytes (the run header pins it, so a
	// run can be audited against the exact workflow content).
	Digest string
	// Workflow is the parsed, validated document; nil when Error is set.
	Workflow *workflow.Workflow
	// Error is the parse error for THIS file ("" when it parsed).
	Error string
}

// State projects one file onto its wire/UI shape (path, name, triggers, error,
// and — for a workflow_dispatch file — the typed input schema the Run workflow
// form renders).
func (f *WorkflowFile) State() *WorkflowState {
	st := &WorkflowState{Path: f.Path, Name: f.Name, Digest: f.Digest, Triggers: []string{}}
	if f.Workflow != nil {
		st.Triggers = f.Workflow.Events()
		if st.Triggers == nil {
			st.Triggers = []string{}
		}
		st.Inputs = dispatchInputs(f)
	}
	st.Error = f.Error
	return st
}

// WorkflowState is GET …/api/threads/workflows's per-file entry and the Threads
// page's workflow row: the per-workflow error object the audit asked for.
type WorkflowState struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Digest   string   `json:"digest,omitempty"`
	Triggers []string `json:"triggers"`
	// Inputs is the declared workflow_dispatch schema (empty for a file
	// without the manual trigger). The Run workflow control renders its typed
	// fields from this.
	Inputs []DispatchInput `json:"inputs,omitempty"`
	// Error is the parse error for this file; empty when it parsed.
	Error string `json:"error,omitempty"`
}

// WorkflowList is GET …/api/threads/workflows: the discovered files at one
// commit and their parse state (source of truth is git; nothing is persisted).
type WorkflowList struct {
	Commit    string           `json:"commit"`
	Workflows []*WorkflowState `json:"workflows"`
}

// maxWorkflowBytes bounds one workflow file (a control-plane object, never a
// build input; 1 MiB is far more than any executable workflow needs).
const maxWorkflowBytes = 1 << 20

// Discover reads and parses every workflow file at commit, independently. A
// missing directory is not an error (nil, nil — the no-workflow repo is a
// silent no-op). A malformed file is recorded on its WorkflowFile.Error and
// does NOT fail the call: the other workflows stay usable and the Threads page
// surfaces the error per file (15 §2.1). Only infrastructure failures (unreadable
// tree/blob, git down) are returned.
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
			if errors.Is(rerr, ErrNotFound) {
				// The tree listed a blob that vanished from the serving
				// copy: a per-file error, not a repo-wide outage.
				files = append(files, &WorkflowFile{Path: p, Name: workflowStem(p), Error: rerr.Error()})
				continue
			}
			return nil, fmt.Errorf("%w: read %s: %v", ErrUnavailable, p, rerr)
		}
		sum := sha256.Sum256(raw)
		f := &WorkflowFile{Path: p, Name: workflowStem(p), Digest: hex.EncodeToString(sum[:])}
		if len(raw) > maxWorkflowBytes {
			f.Error = fmt.Sprintf("%v: %s is larger than %d bytes", ErrUnprocessable, p, maxWorkflowBytes)
			files = append(files, f)
			continue
		}
		w, perr := workflow.Parse(raw)
		if perr != nil {
			f.Error = perr.Error()
			files = append(files, f)
			continue
		}
		f.Name = workflowName(p, w)
		f.Workflow = w
		files = append(files, f)
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

// workflowStem is the file name without its YAML extension (the display name
// of an unparseable file, where the YAML `name` is unavailable).
func workflowStem(p string) string {
	return strings.TrimSuffix(strings.TrimSuffix(path.Base(p), ".yml"), ".yaml")
}

// workflowName prefers the YAML name, then the file stem.
func workflowName(p string, w *workflow.Workflow) string {
	if name := strings.TrimSpace(w.Name); name != "" {
		return name
	}
	return workflowStem(p)
}

// SelectFiles applies the trigger match (15 §1: push, pull_request,
// workflow_dispatch; schedule is the maintainer's job and never fires here).
// kind names the ref kind (branch or tag; only push distinguishes them) and ref
// is the push branch, the tag name or the PR target branch — an empty ref does
// not match a filtered trigger. action is the pull_request lifecycle action
// being delivered, which `types` filters on. want restricts a dispatch to one
// file by basename or repository-relative path. Files that failed to parse
// never match.
func SelectFiles(files []*WorkflowFile, event string, kind workflow.RefKind, ref, action, want string) []*WorkflowFile {
	return SelectFilesWithChanges(files, event, kind, ref, action, want, nil, false)
}

// SelectFilesWithChanges is SelectFiles plus the changed-path set. known is
// false when no diff was computed; a `paths`/`paths-ignore` trigger then does
// not fire (fail closed).
func SelectFilesWithChanges(files []*WorkflowFile, event string, kind workflow.RefKind, ref, action, want string, changed []string, known bool) []*WorkflowFile {
	out := make([]*WorkflowFile, 0, len(files))
	for _, f := range files {
		if f.Workflow == nil {
			continue // per-file isolation: a broken file creates no run
		}
		if !f.Workflow.FiresWithChanges(event, kind, ref, action, changed, known) {
			continue
		}
		if want != "" && !matchesWorkflow(f, want) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// NeedsChangedPaths reports whether any parsed file declares a path filter for
// the event. It is the budget gate: only then does CreateRun spend the one
// name-only git diff (zero store round trips).
func NeedsChangedPaths(files []*WorkflowFile, event string) bool {
	for _, f := range files {
		if f.Workflow == nil {
			continue
		}
		switch event {
		case EventPush:
			if p := f.Workflow.On.Push; p != nil && (len(p.Paths) > 0 || len(p.PathsIgnore) > 0) {
				return true
			}
		case EventPullReq:
			if p := f.Workflow.On.PullRequest; p != nil && (len(p.Paths) > 0 || len(p.PathsIgnore) > 0) {
				return true
			}
		}
	}
	return false
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

// brokenWorkflowFiles returns the files at the selected revision that failed to
// parse (want restricts to one dispatch target; empty means every file). They
// are reported per file and never block the files that did parse.
func brokenWorkflowFiles(files []*WorkflowFile, want string) []*WorkflowFile {
	var out []*WorkflowFile
	for _, f := range files {
		if f.Error == "" {
			continue
		}
		if want != "" && !matchesWorkflow(f, want) {
			continue
		}
		out = append(out, f)
	}
	return out
}
