// threads_wave4_test.go — Wave 4 deployments, environment serialization and
// cancel granularity end to end. No runner and no docker: the assertions are
// the deployment record family, the per-environment queue and the job-cancel
// record the API returns.
package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const deployWorkflowE2E = `name: Deploy
on: workflow_dispatch
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  ship:
    runs-on: linux
    environment: workflow-env
    steps:
      - run: echo ship
`

// deploymentID pulls a nested deployment id out of an API answer.
func deploymentID(t *testing.T, res map[string]any) string {
	t.Helper()
	dep, ok := res["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("answer has no deployment: %v", res)
	}
	id, _ := dep["id"].(string)
	if id == "" {
		t.Fatalf("deployment id empty: %v", dep)
	}
	return id
}

func TestE2E_ThreadsDeploymentsQueueAndJobCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("deployment e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	g := newGitClient(t)
	s := triggerServer(t)
	owner := testOwner
	repo := uniqueRepo("deploy")
	lane := s.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	head := g.commitFile(work, ".spindle/workflows/deploy.yml", deployWorkflowE2E, "workflow")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// Alice may write (she is the principal the environment names as reviewer).
	access := mustAPI(t, "GET", lane+"/access", "", chainAliceTok)
	ver, _ := access["version"].(float64)
	mustAPI(t, "PUT", lane+"/access", fmt.Sprintf(
		`{"version":%d,"visibility":"public","role_bindings":[{"subject":%q,"role":"maintain"}]}`,
		int(ver), "user:alice"), chainAliceTok)

	// The environment must exist before a deployment can target it (fail
	// closed, the same rule a job naming an undefined one follows).
	mustAPI(t, "PUT", lane+"/threads/environments/production",
		`{}`, chainAliceTok)
	// workflow-env has a required reviewer, so its deployment is the hold the
	// assertions below observe. The workflow is dispatch-only: the push that
	// lands the file must not create a competing run that would legitimately
	// occupy this environment's serialization slot first.
	mustAPI(t, "PUT", lane+"/threads/environments/workflow-env",
		`{"required_reviewers":["alice"]}`, chainAliceTok)
	mustAPI(t, "POST", lane+"/threads", `{"workflow":"deploy.yml","ref":"main"}`, chainAliceTok)

	// Two API deployments to one environment: the first holds it, the second is
	// visibly queued with the one named reason.
	first := mustAPI(t, "POST", lane+"/threads/deployments",
		`{"ref":"refs/heads/main","environment":"production","description":"release 1","url":"https://prod.example.test"}`, chainAliceTok)
	firstID := deploymentID(t, first)
	second := mustAPI(t, "POST", lane+"/threads/deployments",
		`{"ref":"refs/heads/main","environment":"production","description":"release 2"}`, chainAliceTok)
	secondID := deploymentID(t, second)
	secondDoc, _ := second["deployment"].(map[string]any)
	if secondDoc["state"] != "queued" {
		t.Fatalf("second deployment state = %v (want queued)", secondDoc["state"])
	}
	if reason, _ := secondDoc["queue_reason"].(string); reason == "" {
		t.Fatal("queued deployment carries no visible reason")
	}
	// A queued deployment cannot jump the line.
	if st, _ := apiCall(t, "POST", lane+"/threads/deployments/"+secondID+"/statuses",
		`{"state":"in_progress"}`, chainAliceTok); st != 409 {
		t.Fatalf("queued in_progress status = %d, want 409", st)
	}
	// Finishing the first promotes the second.
	mustAPI(t, "POST", lane+"/threads/deployments/"+firstID+"/statuses",
		`{"state":"in_progress","description":"rolling out"}`, chainAliceTok)
	mustAPI(t, "POST", lane+"/threads/deployments/"+firstID+"/statuses",
		`{"state":"success","environment_url":"https://prod.example.test/v2"}`, chainAliceTok)
	promoted := mustAPI(t, "GET", lane+"/threads/deployments/"+secondID, "", chainAliceTok)
	promotedDoc, _ := promoted["deployment"].(map[string]any)
	if promotedDoc["state"] != "pending" {
		t.Fatalf("promoted state = %v", promotedDoc["state"])
	}
	// The history is per environment and newest first.
	list := mustAPI(t, "GET", lane+"/threads/deployments?environment=production", "", chainAliceTok)
	deps, _ := list["deployments"].([]any)
	if len(deps) != 2 {
		t.Fatalf("production deployments = %d, want 2", len(deps))
	}

	// The push's workflow job wrote the same record family. No runner is
	// configured, so the ship job is visible as queued with its deployment.
	_, pushed := waitRun(t, lane, "workflow_dispatch", head, 60*time.Second)
	runID := pushed[0].ID
	view := mustAPI(t, "GET", lane+"/threads/"+runID, "", chainAliceTok)
	jobs, _ := view["jobs"].([]any)
	var shipID, deployment string
	var shipJob map[string]any
	for _, jraw := range jobs {
		job, _ := jraw.(map[string]any)
		if job["name"] != "ship" {
			continue
		}
		shipJob = job
		shipID, _ = job["id"].(string)
		dep, _ := job["deployment"].(map[string]any)
		deployment, _ = dep["deployment_id"].(string)
	}
	if shipID == "" || deployment == "" {
		t.Fatalf("ship job has no deployment record: %v", jobs)
	}
	// The hold is recorded before the job can be claimed: the run says
	// pending_approval / environment, the approval projection is pending, and
	// the job's own queue reason names the environment's reviewers. This is the
	// marker the UI shows; a lost hold would fail here, not merely later.
	runDoc, _ := view["run"].(map[string]any)
	if runDoc["state"] != "pending_approval" || runDoc["approval_kind"] != "environment" {
		t.Fatalf("held run = %v", runDoc)
	}
	approval, _ := view["approval"].(map[string]any)
	if approval["pending"] != true {
		t.Fatalf("approval projection = %v", view["approval"])
	}
	if reason, _ := shipJob["queue_reason"].(string); !strings.HasPrefix(reason, "awaiting approval of deployment to workflow-env") {
		t.Fatalf("ship queue reason = %q", reason)
	}
	record := mustAPI(t, "GET", lane+"/threads/deployments/"+deployment, "", chainAliceTok)
	recordDoc, _ := record["deployment"].(map[string]any)
	if recordDoc["environment"] != "workflow-env" || recordDoc["run_id"] != runID || recordDoc["job_id"] != shipID {
		t.Fatalf("job deployment record = %v", recordDoc)
	}
	if recordDoc["state"] != "pending" {
		t.Fatalf("job deployment state = %v (want pending: a reviewer holds it)", recordDoc["state"])
	}

	// Cancelling the single job leaves the build sibling alone and names the
	// actor; a forced cancel of a queued job is the same path with a reason.
	cancelled := mustAPI(t, "POST", lane+"/threads/"+runID+"/jobs/"+shipID+"/cancel", `{"force":true}`, chainAliceTok)
	jobDoc, _ := cancelled["job"].(map[string]any)
	if jobDoc["state"] != "cancelled" {
		t.Fatalf("cancelled job = %v", jobDoc)
	}
	if by, _ := jobDoc["cancelled_by"].(string); by == "" {
		t.Fatalf("cancelled job names no actor: %v", jobDoc)
	}
	if forced, _ := jobDoc["force_cancelled"].(bool); !forced {
		t.Fatalf("force_cancelled = %v", jobDoc["force_cancelled"])
	}
	view = mustAPI(t, "GET", lane+"/threads/"+runID, "", chainAliceTok)
	jobs, _ = view["jobs"].([]any)
	for _, jraw := range jobs {
		job, _ := jraw.(map[string]any)
		if job["name"] == "build" && job["state"] == "cancelled" {
			t.Fatal("cancelling the ship job cancelled its build sibling")
		}
	}
}
