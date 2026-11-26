// threads_wave2_test.go — Wave 2 runner surfaces end to end (docs/threads.md
// §3.4b, §3.9a, §4.2): a health-checked service gates a job, a repository-local
// reusable workflow's jobs run in order, and a step's annotation + summary reach
// the job record.
//
// Skipped in -short mode (the e2e tier) and when docker is unavailable.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wave2Workflow = `name: Wave2
on: [push]
jobs:
  probe:
    name: probe
    runs-on: linux
    services:
      db:
        image: alpine/git:latest
        entrypoint: ["sh"]
        command: ["-c", "while :; do sleep 1; done"]
        options: --health-cmd "true" --health-interval 1s --health-timeout 2s --health-retries 3
    steps:
      - name: reach the service by alias
        run: |
          getent hosts db >/dev/null
          echo "service-ok"
      - name: annotate and summarise
        run: |
          echo "::error file=main.go,line=3,col=2::the handler is wrong"
          printf '## Wave 2\n\n~~~mermaid\ngraph TD; A-->B;\n~~~\n' >> "$GITHUB_STEP_SUMMARY"
  deploy:
    uses: ./.spindle/workflows/deploy.yml
`

const wave2Callee = `name: Deploy
jobs:
  plan:
    runs-on: linux
    steps:
      - run: echo "deploy-plan-ok"
  apply:
    runs-on: linux
    needs: [plan]
    steps:
      - run: echo "deploy-apply-ok"
`

func TestE2E_ThreadsWave2RunnerSurfaces(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("threadsw2")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/wave2.yml", wave2Workflow, "ci")
	g.commitFile(work, ".spindle/workflows/deploy.yml", wave2Callee, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	runner := startThreadsRunner(t, bin, s.base, reg, "linux", 2)

	var runID string
	pollUntil(t, 60*time.Second, "a run to appear", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	var run threadRun
	deadline := time.Now().Add(180 * time.Second)
	for {
		run = getRun(t, repoLane, runID, chainAliceTok)
		if state, _ := run.Run["state"].(string); state == "success" || state == "failure" || state == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in 3m\njobs: %v\nrunner:\n%s", runID, run.Jobs, runner.logs.String())
		}
		time.Sleep(time.Second)
	}
	if state := run.Run["state"]; state != "success" {
		t.Fatalf("run = %v, want success\njobs: %v\nprobe log:\n%s\ndeploy log:\n%s\nrunner:\n%s",
			state, run.Jobs,
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-probe/log", repoLane, runID), chainAliceTok),
			sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-deploy-plan/log", repoLane, runID), chainAliceTok),
			runner.logs.String())
	}
	// The reusable workflow's jobs are records of the same run, in order.
	for id, want := range map[string]string{
		"j-probe": "success", "j-deploy-plan": "success", "j-deploy-apply": "success",
	} {
		st := jobState(run, id)
		if st == nil || st["state"] != want {
			t.Fatalf("job %s = %v, want %s (jobs: %v)", id, st, want, run.Jobs)
		}
	}

	probeLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-probe/log", repoLane, runID), chainAliceTok)
	for _, want := range []string{"starting service db", "service-ok"} {
		if !strings.Contains(probeLog, want) {
			t.Fatalf("probe log misses %q:\n%s", want, probeLog)
		}
	}
	deployLog := sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-deploy-plan/log", repoLane, runID), chainAliceTok)
	if !strings.Contains(deployLog, "deploy-plan-ok") {
		t.Fatalf("callee log = %s", deployLog)
	}

	// The annotation and the step summary are on the job record (the page
	// renders the summary with the sanitizing markdown renderer).
	probe := jobState(run, "j-probe")
	annotations, _ := probe["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("annotations = %v", probe["annotations"])
	}
	ann, _ := annotations[0].(map[string]any)
	if ann["level"] != "error" || ann["file"] != "main.go" || ann["line"] != float64(3) {
		t.Fatalf("annotation = %v", ann)
	}
	summary, _ := probe["summary"].(string)
	if !strings.Contains(summary, "## Wave 2") || !strings.Contains(summary, "~~~mermaid") {
		t.Fatalf("summary = %q", summary)
	}
}
