// threads_wave4_test.go — Wave 4 operations end to end
// (docs/features/21_parity_master.md PAR-154/PAR-155/PAR-157/PAR-181):
// schedule run-now, queue ordering by priority, CI pause, and the
// runner-authenticated drain/rotate verbs against a live server.
//
// No docker is required: the test proves the server's scheduling decisions
// (claims, ordering, pause, credentials), which is where the parity rows live.
// The job-execution half is the runner e2e suite.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const wave4Workflow = `name: Ops
on:
  push:
  workflow_dispatch:
  schedule:
    - cron: "0 3 * * *"
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

func TestE2E_ThreadsWave4FleetPauseAndPriority(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tier")
	}
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	owner, repo := testOwner, uniqueRepo("threadsw4")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"
	origin := strings.TrimSuffix(repoLane, "/"+owner+"/"+repo+"/api")
	gitURL := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/ops.yml", wave4Workflow, "ci")
	g.run(work, "remote", "add", "origin", gitURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// Offline-style registration over the protocol.
	regTok := mintRunnerToken(t, repoLane, chainAliceTok)
	reg := mustAPI(t, "POST", origin+"/api/v1/threads/runners/register",
		fmt.Sprintf(`{"token":%q,"name":"wave4","labels":["linux"],"capacity":1}`, regTok), chainAliceTok)
	runnerToken, _ := reg["token"].(string)
	if !strings.HasPrefix(runnerToken, "wrt_") {
		t.Fatalf("register = %v", reg)
	}
	fetch := func(timeout int) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/threads/runners/fetch?timeout=%d", origin, timeout), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+runnerToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	// Priority ordering: a priority run's jobs are first in the claim order even
	// though it was dispatched second.
	low := mustAPI(t, "POST", repoLane+"/threads",
		`{"ref":"refs/heads/main","workflow":"ops.yml"}`, chainAliceTok)
	high := mustAPI(t, "POST", repoLane+"/threads",
		`{"ref":"refs/heads/main","workflow":"ops.yml","priority":5}`, chainAliceTok)
	highID := firstRunID(t, high)
	lowID := firstRunID(t, low)
	if highID == "" || lowID == "" {
		t.Fatalf("dispatch answers = %v / %v", high, low)
	}
	q := mustAPI(t, "GET", repoLane+"/threads/queue", "", chainAliceTok)
	rows, _ := q["rows"].([]any)
	if len(rows) == 0 {
		t.Fatalf("queue = %v", q)
	}
	first, _ := rows[0].(map[string]any)
	if first["priority"] != float64(5) || first["run_id"] != highID {
		t.Fatalf("claim order = %v, want the priority-5 run first", rows)
	}

	// Pause: the paused repository claims nothing.
	if st, body := apiCall(t, "POST", repoLane+"/threads/pause", `{"paused":true}`, chainAliceTok); st != http.StatusOK {
		t.Fatalf("pause = %d %s", st, body)
	}
	if resp, body := fetch(1); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("paused fetch = %d %s", resp.StatusCode, body)
	}
	q = mustAPI(t, "GET", repoLane+"/threads/queue", "", chainAliceTok)
	if q["paused"] != true {
		t.Fatalf("queue paused = %v", q["paused"])
	}
	if st, body := apiCall(t, "POST", repoLane+"/threads/pause", `{"paused":false}`, chainAliceTok); st != http.StatusOK {
		t.Fatalf("resume = %d %s", st, body)
	}
	if resp, body := fetch(10); resp.StatusCode != http.StatusOK {
		t.Fatalf("resumed fetch = %d %s", resp.StatusCode, body)
	}

	// Schedule operations: the list names next fire, run-now starts a run.
	list := mustAPI(t, "GET", repoLane+"/threads/schedules", "", chainAliceTok)
	entries, _ := list["entries"].([]any)
	if len(entries) == 0 {
		t.Fatalf("schedules = %v", list)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["next_fire"] == "" {
		t.Fatalf("schedule entry = %v", entry)
	}
	runNow := mustAPI(t, "POST", repoLane+"/threads/schedules/run-now",
		fmt.Sprintf(`{"path":%q,"cron":%q,"timezone":""}`, entry["path"], entry["cron"]), chainAliceTok)
	runs, _ := runNow["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("run-now = %v", runNow)
	}

	// Fleet verbs with the runner's own credential: drain claims nothing,
	// resume claims again, rotate keeps the old credential valid while both work.
	if st, body := runnerCall(t, origin, runnerToken, "drain"); st != http.StatusOK {
		t.Fatalf("drain = %d %s", st, body)
	}
	if resp, body := fetch(1); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("drained fetch = %d %s", resp.StatusCode, body)
	}
	if st, body := runnerCall(t, origin, runnerToken, "resume"); st != http.StatusOK {
		t.Fatalf("resume = %d %s", st, body)
	}
	rot, st, body := runnerCallJSON(t, origin, runnerToken, "rotate")
	if st != http.StatusOK {
		t.Fatalf("rotate = %d %s", st, body)
	}
	newToken, _ := rot["token"].(string)
	if newToken == "" || newToken == runnerToken {
		t.Fatalf("rotate answer = %v", rot)
	}
	// Both credentials authenticate inside the overlap window.
	for _, tok := range []string{runnerToken, newToken} {
		if st, _ := runnerCall(t, origin, tok, "resume"); st != http.StatusOK {
			t.Fatalf("credential after rotation = %d", st)
		}
	}
	// Deregister removes the record and credential (a fresh runner that never
	// claimed a job), and the credential is a real 401 afterwards.
	regTok2 := mintRunnerToken(t, repoLane, chainAliceTok)
	reg2 := mustAPI(t, "POST", origin+"/api/v1/threads/runners/register",
		fmt.Sprintf(`{"token":%q,"name":"wave4-retire","labels":["linux"],"capacity":1}`, regTok2), chainAliceTok)
	retireToken, _ := reg2["token"].(string)
	if st, _ := runnerCall(t, origin, retireToken, "deregister"); st != http.StatusOK {
		t.Fatalf("deregister = %d", st)
	}
	if st, _ := runnerCall(t, origin, retireToken, "resume"); st != http.StatusUnauthorized {
		t.Fatalf("retired credential = %d, want 401", st)
	}
}

func firstRunID(t *testing.T, answer map[string]any) string {
	t.Helper()
	runs, _ := answer["runs"].([]any)
	if len(runs) == 0 {
		return ""
	}
	first, _ := runs[0].(map[string]any)
	id, _ := first["id"].(string)
	return id
}

func runnerCall(t *testing.T, origin, token, verb string) (int, string) {
	t.Helper()
	_, st, body := runnerCallJSON(t, origin, token, verb)
	return st, body
}

func runnerCallJSON(t *testing.T, origin, token, verb string) (map[string]any, int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, origin+"/api/v1/threads/runners/"+verb, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, string(raw)
}
