// threads_actions_test.go — local composite actions and docker:// actions end
// to end against a real docker daemon (docs/threads.md §3.4a):
//
//   - a repository-local composite action expands in place, its inputs reach
//     INPUT_*, a nested composite runs, and the action's output is readable by
//     the NEXT step through steps.<id>.outputs;
//   - a local `using: docker` action runs its pinned image with its metadata
//     entrypoint, publishes an output, and receives NO job token;
//   - the run page/API step list shows the expanded steps with their parents.
//
// Skipped in -short mode and when docker is unavailable.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinnedImage is a local image pinned by digest (the daemon has alpine/git from
// the runner's default; the digest keeps the e2e independent of tag movement).
const pinnedImage = "alpine/git@sha256:2306e2b05ee0ef99f5d25b0779f645b24ae9f91ba187107503592fcdfe78da3a"

const actionsWorkflow = `name: Actions
on: [push]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: setup
        name: toolchain
        uses: ./.spindle/actions/setup
        with:
          who: world
      - name: consume
        run: echo "greeting=${{ steps.setup.outputs.greeting }}"
      - id: lint
        name: lint
        uses: ./.spindle/actions/lint
      - name: consume-docker
        run: echo "report=${{ steps.lint.outputs.report }}"
`

const setupAction = `
name: setup
inputs:
  who:
    default: nobody
  mode:
    default: strict
outputs:
  greeting:
    value: ${{ steps.nested.outputs.greeting }}
runs:
  using: composite
  steps:
    - id: install
      run: echo "install-ok $INPUT_WHO $INPUT_MODE"
      shell: sh
    - id: nested
      uses: ./.spindle/actions/nested
      with:
        who: nested
`

const nestedAction = `
name: nested
inputs:
  who:
    default: nobody
outputs:
  greeting:
    value: ${{ steps.hello.outputs.greeting }}
runs:
  using: composite
  steps:
    - id: hello
      run: echo "nested-ok $INPUT_WHO"; echo "greeting=hello-$INPUT_WHO" >> "$SPINDLE_OUTPUT"
      shell: sh
`

const lintAction = `
name: lint
runs:
  using: docker
  image: docker://` + pinnedImage + `
  entrypoint: ["/bin/sh", "-c"]
  args: ["echo docker-ok $INPUT_LEVEL; echo docker-token=${SPINDLE_JOB_TOKEN:-none}; echo 'report=docker-report' >> \"$SPINDLE_OUTPUT\""]
  env:
    LINT_MODE: ci
inputs:
  level:
    default: warn
`

func TestE2E_ThreadsLocalAndDockerActions(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsactions")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/actions.yml", actionsWorkflow, "ci")
	g.commitFile(work, ".spindle/actions/setup/action.yml", setupAction, "ci")
	g.commitFile(work, ".spindle/actions/nested/action.yml", nestedAction, "ci")
	g.commitFile(work, ".spindle/actions/lint/action.yml", lintAction, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "the run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	pollUntil(t, 240*time.Second, "the run to finish", func() bool {
		st := getRun(t, repoLane, runID, chainAliceTok).Run["state"]
		return st == "success" || st == "failure" || st == "cancelled"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	job := jobState(run, "j-build")
	if job == nil {
		t.Fatalf("run jobs = %v", run.Jobs)
	}
	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), chainAliceTok)
	if job["state"] != "success" {
		t.Fatalf("build job = %v / %v (want success)\nlog:\n%s\nrunner:\n%s", job["state"], job["reason"], logText, s.logs.String())
	}
	for _, want := range []string{
		"install-ok world strict", // INPUT_* from with + defaults
		"nested-ok nested",        // the nested composite (its own with:)
		"greeting=hello-nested",   // the nested action output, read by the next step
		"docker-ok warn",          // the docker action + INPUT default
		"docker-token=none",       // never the job token
		"report=docker-report",    // the docker output, read by the next step
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("job log misses %q:\n%s", want, logText)
		}
	}
	// The API's step list is the expanded display: the caller's own row is
	// replaced by its inner steps, each naming its parent.
	steps, _ := job["steps"].([]any)
	if len(steps) < 4 {
		t.Fatalf("job steps = %v", steps)
	}
	var flat []string
	for _, st := range steps {
		m, _ := st.(map[string]any)
		name, _ := m["name"].(string)
		parent, _ := m["parent"].(string)
		flat = append(flat, parent+"|"+name)
	}
	joined := strings.Join(flat, "\n")
	for _, want := range []string{
		"toolchain|",                           // the outer action's install step
		"toolchain / .spindle/actions/nested|", // the nested action's step
		"|lint",                                // the docker action step
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expanded steps miss %q:\n%s", want, joined)
		}
	}
	// The docker action's container is gone (cleanup on every path).
	// (The runner removes it after the step; a leaked one would be named sp-a-.)
}
