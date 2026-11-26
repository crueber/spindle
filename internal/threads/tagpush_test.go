package threads

// Tag pushes (PAR-010, docs/threads.md §6.1): a landed `refs/tags/…` create
// starts the runs whose `on.push.tags` globs match, exactly as a branch push
// does for `on.push.branches`. A tag run has NO branch — the tag name selects
// the trigger and lives in Run.Ref (`refs/tags/v1.2.3`), never in Run.Branch —
// so a tag can never be mistaken for a branch of the same name downstream.

import (
	"sort"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// tagSHA is a commit distinct from the fixture's testSHA, so each case owns the
// exact workflow file set at the revision the run reads.
const tagSHA = "3333333333333333333333333333333333333333"

const tagTriggerWorkflow = `name: Release
on:
  push:
    tags: ['v*']
jobs:
  publish:
    runs-on: linux
    steps:
      - run: echo release
`

const branchTriggerWorkflow = `name: Main
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

const barePushWorkflow = `name: Any
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

// pushOpts is the emitter's CreateOpts shape for one push (cmd/spindle sends
// exactly this): a branch names Branch, a tag names RefName and leaves Branch
// empty.
func pushOpts(kind workflow.RefKind, ref string) CreateOpts {
	short := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	opts := CreateOpts{Ref: ref, RefName: short, RefKind: kind, Actor: "push"}
	if kind == workflow.RefBranch {
		opts.Branch = short
	}
	return opts
}

// TestPushRefKindSelectsTrigger is the firing matrix: the ref KIND picks the
// filter list, the short name is globbed, and neither kind can leak into the
// other's trigger.
func TestPushRefKindSelectsTrigger(t *testing.T) {
	tagsOnly := map[string]string{".spindle/workflows/release.yml": tagTriggerWorkflow}
	branchesOnly := map[string]string{".spindle/workflows/ci.yml": branchTriggerWorkflow}
	bare := map[string]string{".spindle/workflows/any.yml": barePushWorkflow}
	releaseGlob := map[string]string{".spindle/workflows/release.yml": strings.Replace(tagTriggerWorkflow,
		"tags: ['v*']", "tags: ['release-*']", 1)}
	both := map[string]string{
		".spindle/workflows/release.yml": tagTriggerWorkflow,
		".spindle/workflows/ci.yml":      branchTriggerWorkflow,
	}
	cases := []struct {
		name  string
		files map[string]string
		kind  workflow.RefKind
		ref   string
		want  []string
	}{
		{"tag create fires the tags glob", both, workflow.RefTag, "refs/tags/v1.2.3",
			[]string{".spindle/workflows/release.yml"}},
		{"tag never fires branches-only", branchesOnly, workflow.RefTag, "refs/tags/v1.2.3", nil},
		{"tag outside the glob fires nothing", releaseGlob, workflow.RefTag, "refs/tags/v1.2.3", nil},
		{"branch push fires branches", both, workflow.RefBranch, "refs/heads/main",
			[]string{".spindle/workflows/ci.yml"}},
		{"branch never fires tags-only", tagsOnly, workflow.RefBranch, "refs/heads/main", nil},
		{"branch named like a tag fires no tags glob", tagsOnly, workflow.RefBranch, "refs/heads/v1.2.3", nil},
		{"bare on: push fires a branch", bare, workflow.RefBranch, "refs/heads/main",
			[]string{".spindle/workflows/any.yml"}},
		{"bare on: push fires a tag", bare, workflow.RefTag, "refs/tags/v1.2.3",
			[]string{".spindle/workflows/any.yml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			for path, body := range tc.files {
				e.git.add(tagSHA, path, body)
			}
			runs, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(tc.kind, tc.ref))
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			got := make([]string, 0, len(runs))
			for _, run := range runs {
				got = append(got, run.WorkflowPath)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("fired files = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTagRunRecordsRefAndNoBranch pins the run model's contract for a tag: the
// full ref in Ref (github.ref and the OIDC token's ref), the tag name as
// github.ref_name, an empty Branch, and the unchanged push identity.
func TestTagRunRecordsRefAndNoBranch(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(tagSHA, ".spindle/workflows/release.yml", tagTriggerWorkflow)
	runs, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefTag, "refs/tags/v1.2.3"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("CreateRun = %d run(s), %v", len(runs), err)
	}
	run := runs[0]
	if run.Event != EventPush || run.Commit != tagSHA || run.Repo != "o/r" || run.Actor != "push" {
		t.Fatalf("tag run header = %+v", run)
	}
	if run.Branch != "" {
		t.Fatalf("a tag run has no branch, got %q", run.Branch)
	}
	if run.Ref != "refs/tags/v1.2.3" {
		t.Fatalf("tag run Ref = %q, want the full refs/tags/v1.2.3", run.Ref)
	}
	gh := githubContext(run, run.Workflow)
	for key, want := range map[string]string{
		"ref": "refs/tags/v1.2.3", "ref_name": "v1.2.3", "event_name": "push",
		"repository": "o/r", "sha": tagSHA, "workflow": "Release",
	} {
		if gh[key] != want {
			t.Errorf("github.%s = %q, want %q", key, gh[key], want)
		}
	}

	// The branch counterpart keeps the plain branch in Branch (and ref_name).
	e.git.add(tagSHA, ".spindle/workflows/ci.yml", branchTriggerWorkflow)
	branchRuns, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefBranch, "refs/heads/main"))
	if err != nil || len(branchRuns) != 1 {
		t.Fatalf("branch CreateRun = %d run(s), %v", len(branchRuns), err)
	}
	branch := branchRuns[0]
	if branch.Branch != "main" || branch.Ref != "refs/heads/main" {
		t.Fatalf("branch run Branch/Ref = %q/%q", branch.Branch, branch.Ref)
	}
	if got := githubContext(branch, branch.Workflow)["ref_name"]; got != "main" {
		t.Fatalf("branch github.ref_name = %q", got)
	}
}

// TestTagRunJobIfSeesTheTag is the end-to-end proof that the github context is
// threaded into run creation: the same guard passes for a tag push and skips a
// branch push (via the job's `if:`), so no consumer reads the tag as a branch.
func TestTagRunJobIfSeesTheTag(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(tagSHA, ".spindle/workflows/gate.yml", `name: Gate
on: [push]
jobs:
  publish:
    runs-on: linux
    if: startsWith(github.ref, 'refs/tags/') && github.ref_name == 'v1.2.3'
    steps:
      - run: echo release
`)
	tagRuns, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefTag, "refs/tags/v1.2.3"))
	if err != nil || len(tagRuns) != 1 {
		t.Fatalf("tag CreateRun = %d run(s), %v", len(tagRuns), err)
	}
	if job := e.jobRecord(t, tagRuns[0].ID, "j-publish"); job.State != JobQueued {
		t.Fatalf("tag run's guarded job state = %q (%s), want queued", job.State, job.Reason)
	}
	branchRuns, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefBranch, "refs/heads/v1.2.3"))
	if err != nil || len(branchRuns) != 1 {
		t.Fatalf("branch CreateRun = %d run(s), %v", len(branchRuns), err)
	}
	if job := e.jobRecord(t, branchRuns[0].ID, "j-publish"); job.State != JobSkipped {
		t.Fatalf("branch run's tag-guarded job state = %q, want skipped", job.State)
	}
}

// TestTagAndBranchSameShortNameAreIndependent pins the supersede contract: the
// implicit key is (workflow file, ref) with the ref's PREFIX included, so a tag
// and a branch both named v1.2.3 land on different keys and neither replaces
// the other. Verified against the strings a key builder reads, not assumed.
func TestTagAndBranchSameShortNameAreIndependent(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(tagSHA, ".spindle/workflows/any.yml", barePushWorkflow)
	branchRuns, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefBranch, "refs/heads/v1.2.3"))
	if err != nil || len(branchRuns) != 1 {
		t.Fatalf("branch CreateRun = %d run(s), %v", len(branchRuns), err)
	}
	tagRuns, err := e.svc.CreateRun(ctx(), "o/r", tagSHA, EventPush, pushOpts(workflow.RefTag, "refs/tags/v1.2.3"))
	if err != nil || len(tagRuns) != 1 {
		t.Fatalf("tag CreateRun = %d run(s), %v", len(tagRuns), err)
	}
	branch, tag := branchRuns[0], tagRuns[0]
	if branch.ID == tag.ID {
		t.Fatalf("tag and branch runs share the id %q", branch.ID)
	}
	// SupersedeKey(owner, repo, workflowPath, ref): the ref includes the
	// prefix, so the two keys differ and the tag never supersedes the branch.
	supersedeKey := func(r *Run) string { return "o/r|" + r.WorkflowPath + "|" + r.Ref }
	if supersedeKey(branch) == supersedeKey(tag) {
		t.Fatalf("tag and branch share the supersede key %q", supersedeKey(tag))
	}
	page, err := e.svc.ListRuns(ctx(), "o/r", e.public(), "", 50)
	if err != nil || len(page.Runs) != 2 {
		t.Fatalf("runs after both pushes = %d, %v (want both independent runs)", len(page.Runs), err)
	}
	for _, run := range page.Runs {
		if run.State != RunQueued {
			t.Fatalf("run %s state = %q: one push superseded the other", run.ID, run.State)
		}
	}
}

// TestRefKindIsPushOnly pins the normalization: a non-push event cannot state a
// tag kind, so a dispatch or PR never selects a push filter it does not have.
func TestRefKindIsPushOnly(t *testing.T) {
	kind, name, branch := refTargetOf(EventDispatch, CreateOpts{
		RefKind: workflow.RefTag, RefName: "v1.2.3", Branch: "main",
	})
	if kind != workflow.RefBranch {
		t.Fatalf("dispatch kind = %v, want branch", kind)
	}
	if name != "v1.2.3" || branch != "main" {
		t.Fatalf("dispatch names = %q/%q, want the supplied name and branch", name, branch)
	}
	// A push that names no RefName keeps the old contract: Branch is the name.
	kind, name, branch = refTargetOf(EventPush, CreateOpts{Branch: "main"})
	if kind != workflow.RefBranch || name != "main" || branch != "main" {
		t.Fatalf("branch push fallback = %v/%q/%q", kind, name, branch)
	}
	// A tag push clears the branch even if the caller set one by mistake.
	kind, name, branch = refTargetOf(EventPush, CreateOpts{RefKind: workflow.RefTag, RefName: "v1", Branch: "v1"})
	if kind != workflow.RefTag || name != "v1" || branch != "" {
		t.Fatalf("tag push = %v/%q/%q, want tag/v1/\"\"", kind, name, branch)
	}
}
