// threads_checkout_test.go — the checkout-reachability diagnostic (docs/threads.md
// §4.4): when the job container cannot resolve the checkout origin, the job log
// must carry a NAMED error that states the URL tried, the in-container
// reachability requirement and the setting that fixes it — not git's bare
// `exit 128` with only "unable to access …" above it.
//
// The scenario is driven cheaply: server.public_url is an address that never
// resolves (RFC 2606's `.invalid` TLD), which is exactly what a default install
// does when it hands the container a host-only name. A real runner executes a
// real checkout and fails on the name.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// checkoutOnlyWorkflow's only step is the built-in checkout; a second step
// would never run, so its absence is also evidence the failure was the fetch.
const checkoutOnlyWorkflow = `name: Checkout
on: [push]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: echo "checkout succeeded"
`

func TestE2E_ThreadsCheckoutFailureNamesTheUnreachableURL(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	const publicBase = "http://checkout.invalid:8080"
	s := threadsServer(t, publicBase) // server.public_url → the checkout base
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsco")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)
	wantCheckout := publicBase + "/" + owner + "/" + repo + ".git"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/ci.yml", checkoutOnlyWorkflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	pollUntil(t, 120*time.Second, "the job to fail on the unreachable checkout", func() bool {
		run := getRun(t, repoLane, runID, chainAliceTok)
		return run.Run["state"] == "failure"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	job := jobState(run, "j-build")
	if job == nil || job["state"] != "failure" {
		t.Fatalf("job = %v, want failure", job)
	}

	logText := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-build/log", repoLane, runID), chainAliceTok)
	// The named diagnostic: the URL tried, the requirement, the fix.
	for _, want := range []string{
		"spindle: checkout from " + wantCheckout,
		"spindle: checkout failed for " + wantCheckout,
		"must resolve and be reachable from INSIDE the job container",
		"server.public_url",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("job log misses %q:\n%s", want, logText)
		}
	}
	// Git's own output stays as the evidence, not as the whole message.
	if !strings.Contains(logText, "unable to access") {
		t.Fatalf("git's access error was dropped from the log:\n%s", logText)
	}
	// The step that would have run after a successful checkout never did.
	if strings.Contains(logText, "checkout succeeded") {
		t.Fatalf("a step ran after a failed checkout:\n%s", logText)
	}
	// The runner console states the resolved base too (once per job).
	if console := runner.logs.String(); !strings.Contains(console, "job checkout") || !strings.Contains(console, wantCheckout) {
		t.Fatalf("runner console lacks the checkout base %s:\n%s", wantCheckout, console)
	}
}
