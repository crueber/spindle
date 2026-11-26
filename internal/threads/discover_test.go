package threads

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Workflow discovery (15 §1/§7): single/multiple/none, malformed is a visible
// error, nested and non-YAML files are ignored, and trigger matching covers
// push branch filters, PRs, dispatch, and the ignored schedule.

const secondWorkflow = `name: Docs
on:
  push:
jobs:
  lint:
    runs-on: linux
    steps:
      - run: echo lint
`

const baseWorkflow = `name: Base CI
on:
  pull_request:
jobs:
  verify:
    runs-on: linux
    steps:
      - run: echo base
`

func TestDiscoverSingleFile(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	got := files[0]
	if got.Path != ".spindle/workflows/ci.yml" || got.Name != "CI" {
		t.Fatalf("file = %+v", got)
	}
	if len(got.Digest) != 64 {
		t.Fatalf("digest = %q (want a sha256 hex)", got.Digest)
	}
	if _, ok := got.Workflow.Jobs["build"]; !ok {
		t.Fatal("parsed workflow lost the build job")
	}
}

func TestDiscoverMultipleFilesSorted(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	f.add(testSHA, ".spindle/workflows/docs.yaml", secondWorkflow)
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].Path != ".spindle/workflows/ci.yml" || files[1].Path != ".spindle/workflows/docs.yaml" {
		t.Fatalf("order = %s, %s", files[0].Path, files[1].Path)
	}
	if files[1].Name != "Docs" {
		t.Fatalf("fallback name = %q", files[1].Name)
	}
}

func TestDiscoverNoFilesIsNotAnError(t *testing.T) {
	f := newFakeGit()
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil || files != nil {
		t.Fatalf("discover = %v, %v (want nil, nil)", files, err)
	}
}

func TestDiscoverMalformedIsPerFileError(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/broken.yml", "name: nope\non: [\n")
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("a broken file must not fail discovery: %v", err)
	}
	if len(files) != 1 || files[0].Workflow != nil || files[0].Error == "" {
		t.Fatalf("files = %+v (want one file with Error)", files)
	}
	if !strings.Contains(files[0].Error, "broken.yml") && !strings.Contains(files[0].Error, "yaml") {
		t.Fatalf("error must explain the parse failure: %q", files[0].Error)
	}
}

func TestDiscoverUnknownKeyIsPerFileError(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/ci.yml", "name: CI\non:\n  push:\njobs:\n  b:\n    runs-on: x\n    runson: typo\n    steps:\n      - run: y\n")
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("unknown key must not fail discovery: %v", err)
	}
	if len(files) != 1 || files[0].Error == "" {
		t.Fatalf("files = %+v (want one file with Error)", files)
	}
}

func TestDiscoverIgnoresNestedAndNonYAML(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	f.list = []string{
		".spindle/workflows/ci.yml",
		".spindle/workflows/nested/deep.yml", // nested: ignored
		".spindle/workflows/README.md",       // not YAML: ignored
		".spindle/workflows/",                // directory itself: ignored
		".github/workflows/ci.yml",           // other forge: ignored
	}
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(files) != 1 || files[0].Path != ".spindle/workflows/ci.yml" {
		t.Fatalf("files = %+v (want only ci.yml)", files)
	}
}

func TestDiscoverOversizedFileIsPerFileError(t *testing.T) {
	f := newFakeGit()
	big := "name: big\non: push\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: echo\n" + strings.Repeat("# pad\n", maxWorkflowBytes)
	f.add(testSHA, ".spindle/workflows/big.yml", big)
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatalf("an oversized file must not fail discovery: %v", err)
	}
	if len(files) != 1 || !strings.Contains(files[0].Error, "larger than") {
		t.Fatalf("oversized files = %+v", files)
	}
}

func TestDiscoverBackendFailuresAreUnavailable(t *testing.T) {
	f := newFakeGit()
	f.listErr = errors.New("boom")
	if _, err := Discover(ctx(), f, "o/r", testSHA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("list err = %v (want ErrUnavailable)", err)
	}
	f.listErr = nil
	f.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	f.readErr = errors.New("boom")
	if _, err := Discover(ctx(), f, "o/r", testSHA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("read err = %v (want ErrUnavailable)", err)
	}
	f.readErr = nil
	if _, err := Discover(ctx(), nil, "o/r", testSHA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil git err = %v (want ErrUnavailable)", err)
	}
}

// TestSelectFilesTriggerMatrix pins 15 §1: push branch filters, PR filters,
// dispatch presence (including a null key) and the schedule event selecting
// exactly the files that declare it (round-2 pass 2).
func TestSelectFilesTriggerMatrix(t *testing.T) {
	f := newFakeGit()
	f.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	f.add(testSHA, ".spindle/workflows/scheduled.yml", "name: cron\non:\n  schedule:\n    - cron: \"0 3 * * *\"\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: echo\n")
	files, err := Discover(ctx(), f, "o/r", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		event, branch, action, want string
		n                           int
	}{
		{EventPush, "main", "", "", 1},              // branch filter matches
		{EventPush, "dev", "", "", 0},               // branch filter excludes
		{EventPush, "", "", "", 0},                  // a branch-filtered trigger matches no empty branch
		{EventPullReq, "main", "opened", "", 1},     // PR trigger present
		{EventPullReq, "main", "", "", 1},           // no filter: any (or no) action matches
		{EventDispatch, "", "", "", 1},              // workflow_dispatch present (null form)
		{EventDispatch, "", "", "ci.yml", 1},        // dispatch filter by basename
		{EventDispatch, "", "", "docs.yml", 0},      // unknown file: no match
		{EventSchedule, "", "", "", 2},              // both fixture files declare `schedule:`
		{EventSchedule, "", "", "scheduled.yml", 1}, // a tick names exactly one file
		{EventSchedule, "", "", "nope.yml", 0},      // and never an unknown one
		{"bogus", "", "", "", 0},                    // unknown event: no match
	}
	for _, tc := range cases {
		got := SelectFiles(files, tc.event, workflow.RefBranch, tc.branch, tc.action, tc.want)
		if len(got) != tc.n {
			t.Errorf("SelectFiles(%s,%q,%q,%q) = %d, want %d", tc.event, tc.branch, tc.action, tc.want, len(got), tc.n)
		}
	}
	if got := SelectFiles(files, EventDispatch, workflow.RefBranch, "", "", ".spindle/workflows/ci.yml"); len(got) != 1 {
		t.Errorf("dispatch by full path = %d, want 1", len(got))
	}
}

func TestCheckContextNaming(t *testing.T) {
	cases := []struct{ wf, job, want string }{
		{"CI", "build", "threads/ci/build"},
		{"CI Pipeline", "Unit Tests", "threads/ci-pipeline/unit-tests"},
		{"", "", "threads/unnamed/unnamed"},
		{"a/b", "c:d*e", "threads/a-b/c-d-e"},
	}
	for _, tc := range cases {
		if got := CheckContext(tc.wf, tc.job); got != tc.want {
			t.Errorf("CheckContext(%q,%q) = %q, want %q", tc.wf, tc.job, got, tc.want)
		}
	}
	long := strings.Repeat("x", 100)
	got := CheckContext(long, long)
	if len(got) > 100 {
		t.Fatalf("context %q exceeds the checks grammar length", got)
	}
}

func TestFoldRunState(t *testing.T) {
	job := func(state string) *Job { return &Job{State: state} }
	cases := []struct {
		name string
		jobs []*Job
		want string
	}{
		{"none", nil, RunQueued},
		{"all queued", []*Job{job(JobQueued), job(JobQueued)}, RunQueued},
		{"one running", []*Job{job(JobQueued), job(JobRunning)}, RunRunning},
		{"success", []*Job{job(JobSuccess), job(JobSuccess)}, RunSuccess},
		{"failure wins", []*Job{job(JobSuccess), job(JobFailure)}, RunFailure},
		{"cancelled is distinct from failure", []*Job{job(JobSuccess), job(JobCancelled)}, RunCancelled},
		{"failure outranks cancelled", []*Job{job(JobFailure), job(JobCancelled)}, RunFailure},
		{"terminal + queued is running", []*Job{job(JobSuccess), job(JobQueued)}, RunRunning},
	}
	for _, tc := range cases {
		if got := foldRunState(tc.jobs); got != tc.want {
			t.Errorf("%s: fold = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestJobIDForCollisions(t *testing.T) {
	used := map[string]bool{}
	if got := jobIDFor("Build", used); got != "j-build" {
		t.Fatalf("id = %q", got)
	}
	if got := jobIDFor("build", used); got != "j-build-2" {
		t.Fatalf("collision id = %q", got)
	}
	if got := jobIDFor("!!!", used); got != "j-unnamed" {
		t.Fatalf("empty slug id = %q", got)
	}
}

// TestDiscoverIgnoresForeignWorkflowDirs is PAR-001's acceptance: the only
// discovered location is .spindle/workflows, and a repository whose CI lives in
// .forgejo/workflows is a no-workflow repository — zero files, no run. It pins
// both layers: a GitReader that (wrongly) hands back a foreign path is filtered
// by workflowPath, and a real tree with only a foreign directory yields nothing
// at all, so no trigger can ever create a run from it.
//
// The acceptance line's "named hint pointing at `spindle threads import`" is the
// migration banner, which is PAR-003 (Wave 2): it is a page-only projection on an
// explicit request, deliberately not a discovery path, so it is not asserted
// here.
func TestDiscoverIgnoresForeignWorkflowDirs(t *testing.T) {
	foreign := ".forgejo/workflows/ci.yml"
	body := "name: CI\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"

	t.Run("a reader that returns a foreign path is filtered", func(t *testing.T) {
		g := newFakeGit()
		g.list = []string{foreign, ".github/workflows/ci.yml", ".spindle/workflows/notes.txt"}
		files, err := Discover(ctx(), g, "o/r", testSHA)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(files) != 0 {
			t.Fatalf("discovered %d files from foreign directories: %+v", len(files), files)
		}
	})

	t.Run("a repository with only a forgejo directory creates no run", func(t *testing.T) {
		e := newTestEnv(t)
		// Replace the fixture with the foreign-only tree: the derived listing
		// contains just the .forgejo path.
		e.git = newFakeGit()
		e.svc.Git = e.git
		e.git.add(testSHA, foreign, body)

		files, err := Discover(ctx(), e.git, "o/r", testSHA)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(files) != 0 {
			t.Fatalf("Discover returned %+v", files)
		}
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
			Branch: "main", Ref: "refs/heads/main", Actor: "writer",
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if len(runs) != 0 {
			t.Fatalf("a foreign workflow directory created %d runs", len(runs))
		}
		list, err := e.svc.ListWorkflows(ctx(), "o/r", testSHA, auth.Principal{Name: "writer"})
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		if len(list.Workflows) != 0 {
			t.Fatalf("the workflow list shows foreign files: %+v", list.Workflows)
		}
		// Nothing was written to the run family either.
		ids, err := e.svc.runIDs(ctx(), "o", "r")
		if err != nil {
			t.Fatalf("runIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("run records exist for a no-workflow repo: %v", ids)
		}
	})
}
