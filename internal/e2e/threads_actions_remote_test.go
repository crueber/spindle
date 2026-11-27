// threads_actions_remote_test.go — remote actions end to end (PAR-070/071/072/
// 074/096) against a real server, a real runner and a real docker daemon:
//
//   - a digest-pinned action runs (no allowlist needed for a full commit SHA);
//   - an allowlisted repository's tag runs (D-5: the allowlist is what lets a
//     mutable ref through);
//   - a non-allowlisted mutable ref is refused BY NAME at run creation;
//   - a fetched composite resolves a NESTED remote `uses:` and reads
//     `${{ inputs.* }}` / `${{ github.action_path }}` (PAR-074);
//   - a JavaScript action on an image with no node fails BY NAME, never skipped
//     (PAR-071);
//   - the run header records every resolved action (source, ref, commit, digest).
//
// Skipped in -short mode and when docker is unavailable.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const helloActionYML = `name: hello
description: say hello
inputs:
  who:
    description: who to greet
    default: world
outputs:
  greeting:
    description: the greeting
    value: ${{ steps.say.outputs.greeting }}
runs:
  using: composite
  steps:
    - id: say
      shell: sh
      run: |
        echo "hello $INPUT_WHO from $GITHUB_WORKSPACE"
        echo "greeting=hi-$INPUT_WHO" >> "$GITHUB_OUTPUT"
    - name: action path
      shell: sh
      run: sh "$GITHUB_ACTION_PATH/script.sh"
`

const helloScript = `#!/bin/sh
echo "hello-action-script-ok $INPUT_WHO"
`

const installScript = `#!/bin/sh
echo "cosign-action-path-ok $1"
`

// cosignActionYML is the sigstore/cosign-installer shape: a composite whose
// steps read the action's own inputs and a path under github.action_path, and
// which calls ANOTHER remote action (PAR-074's acceptance).
func cosignActionYML(nested string) string {
	return `name: cosign installer
description: install cosign
inputs:
  cosign-version:
    default: latest
outputs:
  installed:
    description: the nested greeting
    value: ${{ steps.nested.outputs.greeting }}
runs:
  using: composite
  steps:
    - id: local
      shell: sh
      run: sh "${{ github.action_path }}/install.sh" "${{ inputs.cosign-version }}"
    - id: nested
      uses: ` + nested + `
      with:
        who: ${{ inputs.cosign-version }}
`
}

const nodeActionYML = `name: node hello
inputs:
  who:
    default: world
runs:
  using: node20
  main: index.js
  post: post.js
  post-if: always()
`

const nodeIndexJS = `const fs = require("fs");
console.log("node-action-ok who=" + (process.env.INPUT_WHO || "") + " path=" + (process.env.GITHUB_ACTION_PATH || ""));
fs.appendFileSync(process.env.GITHUB_OUTPUT, "greeting=from-node\n");
`

const nodePostJS = `console.log("node-post-ok")
`

const dockerfileActionYML = `name: dockerfile action
runs:
  using: docker
  image: Dockerfile
  args: ["echo dockerfile-ok"]
`

const dockerfileAction = `FROM node:22-trixie
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["echo wrong-cmd"]
`

func TestE2E_ThreadsRemoteActions(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServerWithConfig(t, `
[threads.actions]
mode = "allowlist"
allow = ["e2e/*"]
require_sha = true
`)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("remoteactions")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	// The action repository on THIS instance: a composite (with a script), a
	// nested composite (cosign shape) and a node action.
	actionsRepo := uniqueRepo("actions")
	actionsURL := s.gitURL(owner, actionsRepo)
	awork := t.TempDir()
	g.initRepo(awork)
	g.commitFile(awork, "hello/action.yml", helloActionYML, "hello action")
	g.commitFile(awork, "hello/script.sh", helloScript, "hello script")
	g.commitFile(awork, "cosign/action.yml", cosignActionYML("unused@0000000000000000000000000000000000000000"), "cosign action")
	g.commitFile(awork, "cosign/install.sh", installScript, "cosign script")
	g.commitFile(awork, "node/action.yml", nodeActionYML, "node action")
	g.commitFile(awork, "node/index.js", nodeIndexJS, "node program")
	g.commitFile(awork, "node/post.js", nodePostJS, "node post")
	g.commitFile(awork, "dockerfile/action.yml", dockerfileActionYML, "dockerfile action")
	g.commitFile(awork, "dockerfile/Dockerfile", dockerfileAction, "dockerfile")
	g.run(awork, "remote", "add", "origin", actionsURL)
	g.runAuth(awork, chainAliceTok, "push", "-u", "origin", "main")
	g.run(awork, "tag", "-a", "v1", "-m", "v1")
	g.runAuth(awork, chainAliceTok, "push", "origin", "refs/tags/v1")
	sha := strings.TrimSpace(g.run(awork, "rev-parse", "HEAD"))

	// Rewrite the cosign composite's nested reference now that the commit is
	// known, then push that as its own commit (the pinned variant).
	g.commitFile(awork, "cosign/action.yml", cosignActionYML(owner+"/"+actionsRepo+"/hello@"+sha)+`
`, "cosign nested")
	g.runAuth(awork, chainAliceTok, "push", "origin", "main")
	head := strings.TrimSpace(g.run(awork, "rev-parse", "HEAD"))

	workflow := fmt.Sprintf(`name: Remote actions
on: [push]
jobs:
  pinned:
    name: pinned
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: hello
        uses: %s/%s/hello@%s
        with:
          who: pinned
      - name: consume
        run: echo "pinned=${{ steps.hello.outputs.greeting }}"
  allowed-tag:
    name: tag
    runs-on: linux
    steps:
      - id: hello
        uses: %s/%s/hello@v1
        with:
          who: tagged
      - name: consume
        run: echo "tagged=${{ steps.hello.outputs.greeting }}"
  nested:
    name: nested
    runs-on: linux
    steps:
      - id: cosign
        uses: %s/%s/cosign@%s
        with:
          cosign-version: "v2.4.0"
      - name: consume
        run: echo "cosign=${{ steps.cosign.outputs.installed }}"
  node:
    name: node
    runs-on: linux
    container:
      image: node:22-trixie
    steps:
      - id: hello
        uses: %s/%s/node@%s
        with:
          who: node
      - name: consume
        run: echo "node=${{ steps.hello.outputs.greeting }}"
  node-missing:
    name: node-missing
    runs-on: linux
    continue-on-error: true
    steps:
      - id: hello
        uses: %s/%s/node@%s
  dockerfile:
    name: dockerfile
    runs-on: linux
    steps:
      - id: img
        uses: %s/%s/dockerfile@%s
  denied:
    name: denied
    runs-on: linux
    continue-on-error: true
    steps:
      - uses: other/thing@v1
`, owner, actionsRepo, sha, owner, actionsRepo, owner, actionsRepo, head, owner, actionsRepo, head, owner, actionsRepo, head, owner, actionsRepo, head)

	work := t.TempDir()
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/remote.yml", workflow, "remote actions")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 2)

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

	// The digest-pinned action ran and its output threaded to the next step.
	pinned := jobState(run, "j-pinned")
	if pinned == nil || pinned["state"] != "success" {
		t.Fatalf("pinned job = %+v\nlog:\n%s\nserver:\n%s", pinned,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-pinned/log", repoLane, runID), chainAliceTok), s.logs.String())
	}
	pinnedLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-pinned/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{"hello pinned from", "hello-action-script-ok pinned", "pinned=hi-pinned",
		"resolved to commit", "content sha256:", "policy sha-pinned"} {
		if !strings.Contains(pinnedLog, want) {
			t.Fatalf("pinned log missing %q:\n%s", want, pinnedLog)
		}
	}
	// The allowlisted tag ran.
	tag := jobState(run, "j-allowed-tag")
	if tag == nil || tag["state"] != "success" {
		t.Fatalf("tag job = %+v\n%s", tag, sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-allowed-tag/log", repoLane, runID), chainAliceTok))
	}
	tagLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-allowed-tag/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(tagLog, "tagged=hi-tagged") {
		t.Fatalf("tag log:\n%s", tagLog)
	}
	// The nested remote composite resolved; its action_path and inputs worked,
	// and its output came from the NESTED action.
	nested := jobState(run, "j-nested")
	if nested == nil || nested["state"] != "success" {
		t.Fatalf("nested job = %+v\n%s\nserver:\n%s", nested,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-nested/log", repoLane, runID), chainAliceTok), s.logs.String())
	}
	nestedLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-nested/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{"cosign-action-path-ok v2.4.0", "hello v2.4.0", "cosign=hi-v2.4.0"} {
		if !strings.Contains(nestedLog, want) {
			t.Fatalf("nested log missing %q:\n%s", want, nestedLog)
		}
	}
	// The JavaScript action runs on the job image's node: inputs, action path,
	// $GITHUB_OUTPUT and the post hook (PAR-071).
	node := jobState(run, "j-node")
	if node == nil || node["state"] != "success" {
		t.Fatalf("node job = %+v\n%s\nserver:\n%s", node,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-node/log", repoLane, runID), chainAliceTok), s.logs.String())
	}
	nodeLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-node/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{"node-action-ok who=node path=/spindle/remote-actions/", "node=from-node", "node-post-ok"} {
		if !strings.Contains(nodeLog, want) {
			t.Fatalf("node log missing %q:\n%s", want, nodeLog)
		}
	}
	// The same action on an image with no node: a NAMED failure, never a skip.
	missing := jobState(run, "j-node-missing")
	if missing == nil || missing["state"] != "failure" {
		t.Fatalf("node-missing job = %+v (want a named runtime failure)", missing)
	}
	missingLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-node-missing/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(missingLog, "needs a Node 20 runtime") || !strings.Contains(missingLog, "never skipped") {
		t.Fatalf("node-missing log:\n%s", missingLog)
	}
	// A Dockerfile action builds on the runner host and runs (PAR-072) without
	// giving the job container a docker socket.
	dockerfile := jobState(run, "j-dockerfile")
	if dockerfile == nil || dockerfile["state"] != "success" {
		t.Fatalf("dockerfile job = %+v\n%s\nserver:\n%s", dockerfile,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-dockerfile/log", repoLane, runID), chainAliceTok), s.logs.String())
	}
	dockerfileLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-dockerfile/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(dockerfileLog, "building docker action") || !strings.Contains(dockerfileLog, "dockerfile-ok") {
		t.Fatalf("dockerfile log:\n%s", dockerfileLog)
	}
	// The non-allowlisted mutable ref: refused at creation, with the policy
	// stated on the job.
	denied := jobState(run, "j-denied")
	if denied == nil || denied["state"] != "failure" {
		t.Fatalf("denied job = %+v", denied)
	}
	reason, _ := denied["reason"].(string)
	if !strings.Contains(reason, "not in threads.actions.allow") || !strings.Contains(reason, "40-character") {
		t.Fatalf("denied reason = %q", reason)
	}
	// The run header records every resolved action, with the decision.
	loads, _ := run.Run["action_loads"].([]any)
	if len(loads) < 3 {
		t.Fatalf("action_loads = %v", run.Run["action_loads"])
	}
	seen := map[string]bool{}
	for _, l := range loads {
		m, _ := l.(map[string]any)
		if m == nil {
			continue
		}
		switch m["policy"] {
		case "allowlisted":
			seen["allowlisted"] = true
		case "sha-pinned":
			seen["sha-pinned"] = true
		}
		if m["commit"] == "" || m["digest"] == "" {
			t.Fatalf("load missing pin: %v", m)
		}
	}
	if !seen["allowlisted"] || !seen["sha-pinned"] {
		t.Fatalf("action_loads decisions = %v", loads)
	}
}
