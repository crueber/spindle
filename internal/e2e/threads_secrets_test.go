// threads_secrets_test.go — the secret-scope end to end (PAR-115/PAR-116,
// docs/threads.md §5.1b): organization, repository and selected-repository
// scopes resolve with the documented precedence, the scope is recorded on the
// run (never the value), masking covers the log, the annotation and the step
// summary, and an untrusted fork run reads NONE of them.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_ThreadsSecretScopesAndMasking is the acceptance run for PAR-115 and
// PAR-116: one organization, two repositories, every secret scope and the
// masking surfaces, driven through a real runner and real containers.
func TestE2E_ThreadsSecretScopesAndMasking(t *testing.T) {
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	org := "e2esecrets"
	// The org must exist before a repository under it can carry org secrets.
	mustAPI(t, "POST", s.base+"/api/v1/orgs",
		fmt.Sprintf(`{"org":%q,"display_name":"E2E Secrets"}`, org), chainAliceTok)

	// One workflow, two repositories. Markers report PRESENCE, never a value;
	// the values themselves are printed only so the masker has something to
	// remove.
	const workflow = `name: Secrets
on: [push, pull_request]
jobs:
  probe:
    name: probe
    runs-on: linux
    steps:
      - name: markers
        run: |
          echo "org=${ORG_TOKEN:+present}"
          echo "shared-is-${SHARED}"
          echo "onlyapi=${ONLY_API:+present}"
          echo "short=($SHORT)"
          echo "multi:$MULTI"
          echo "::error title=leak::annotation ${ORG_TOKEN:-none}"
          echo "summary ${ORG_TOKEN:-none}" >> "$GITHUB_STEP_SUMMARY"
`
	const (
		orgToken    = "org-token-value-1234"
		orgShared   = "org-shared-value-1234"
		repoShared  = "repo-shared-value-1234"
		selectedVal = "selected-only-value-1234"
		shortVal    = "ab"
		multiVal    = "line-one-1234\nline-two-5678"
	)
	seed := func(name string) (lane, gitURL, sha string) {
		t.Helper()
		work := filepath.Join(t.TempDir(), name)
		g.initRepo(work)
		sha = g.commitFile(work, ".spindle/workflows/ci.yml", workflow, "ci")
		g.run(work, "remote", "add", "origin", s.gitURL(org, name))
		g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
		return s.base + "/" + org + "/" + name + "/api", s.gitURL(org, name), sha
	}
	apiLane, apiGit, _ := seed("api")
	webLane, _, _ := seed("web")

	orgSecrets := s.base + "/api/v1/orgs/" + org + "/threads/secrets"
	for _, tc := range []struct{ name, body string }{
		{"ORG_TOKEN", fmt.Sprintf(`{"value":%q}`, orgToken)},
		{"SHARED", fmt.Sprintf(`{"value":%q}`, orgShared)},
		{"ONLY_API", fmt.Sprintf(`{"value":%q,"visibility":"selected","repositories":[%q]}`, selectedVal, org+"/api")},
		{"SHORT", fmt.Sprintf(`{"value":%q}`, shortVal)},
		{"MULTI", fmt.Sprintf(`{"value":%q}`, multiVal)},
	} {
		if st, body := apiCall(t, "PUT", orgSecrets+"/"+tc.name, tc.body, chainAliceTok); st != http.StatusNoContent {
			t.Fatalf("org secret %s = %d: %s", tc.name, st, body)
		}
	}
	// A repository secret of the same name wins over the organization's.
	if st, body := apiCall(t, "PUT", apiLane+"/threads/secrets/SHARED",
		fmt.Sprintf(`{"value":%q}`, repoShared), chainAliceTok); st != http.StatusNoContent {
		t.Fatalf("repo secret = %d: %s", st, body)
	}
	// The list surface reports the policy and never a value.
	list := mustAPI(t, "GET", orgSecrets, "", chainAliceTok)
	listBody, _ := json.Marshal(list)
	if strings.Contains(string(listBody), orgToken) || strings.Contains(string(listBody), selectedVal) {
		t.Fatalf("the org secret list returned a value: %s", listBody)
	}

	runner := startThreadsRunner(t, bin, s.base, mintRunnerToken(t, apiLane, chainAliceTok), "linux", 1)

	// Wait for one push run of a repository and return it.
	waitRun := func(lane, what string) string {
		t.Helper()
		var id string
		pollUntil(t, 90*time.Second, "the "+what+" run", func() bool {
			res := mustAPI(t, "GET", lane+"/threads?n=10", "", chainAliceTok)
			runs, _ := res["runs"].([]any)
			for _, r := range runs {
				m, _ := r.(map[string]any)
				if m["event"] != "push" {
					continue
				}
				if m["state"] == "success" || m["state"] == "failure" {
					id, _ = m["id"].(string)
					return id != ""
				}
			}
			return false
		})
		return id
	}
	logOf := func(lane, runID string) string {
		t.Helper()
		return sseLog(t, fmt.Sprintf("%s/threads/%s/jobs/j-probe/log", lane, runID), chainAliceTok)
	}

	// ---- api: repository wins over organization, selected secret reaches it --
	apiRun := waitRun(apiLane, "api")
	apiText := logOf(apiLane, apiRun)
	for _, want := range []string{"org=present", "shared-is-***", "onlyapi=present", "short=(***)"} {
		if !strings.Contains(apiText, want) {
			t.Fatalf("api log misses %q:\n%s", want, apiText)
		}
	}
	for _, leak := range []string{orgToken, orgShared, repoShared, selectedVal, "line-one-1234", "line-two-5678"} {
		if strings.Contains(apiText, leak) {
			t.Fatalf("api log leaked %q:\n%s", leak, apiText)
		}
	}
	apiScopes := jobSecretScopes(t, apiLane, apiRun, chainAliceTok, "j-probe")
	if apiScopes["SHARED"] != "repository" {
		t.Fatalf("api SHARED scope = %q, want repository (%v)", apiScopes["SHARED"], apiScopes)
	}
	if apiScopes["ORG_TOKEN"] != "organization" || apiScopes["ONLY_API"] != "organization" {
		t.Fatalf("api org scopes = %v", apiScopes)
	}
	// Annotations and the step summary are masked before storage.
	ann, summary := jobAnnotationsAndSummary(t, apiLane, apiRun, chainAliceTok, "j-probe")
	annJSON, _ := json.Marshal(ann)
	if strings.Contains(string(annJSON), orgToken) {
		t.Fatalf("the stored annotation leaked: %s", annJSON)
	}
	if !strings.Contains(string(annJSON), "***") {
		t.Fatalf("the stored annotation was not masked: %s", annJSON)
	}
	if strings.Contains(summary, orgToken) {
		t.Fatalf("the stored summary leaked: %q", summary)
	}
	if !strings.Contains(summary, "***") {
		t.Fatalf("the stored summary was not masked: %q", summary)
	}

	// ---- web: organization resolves, the unselected secret does NOT ---------
	webRun := waitRun(webLane, "web")
	webText := logOf(webLane, webRun)
	for _, want := range []string{"org=present", "shared-is-***", "onlyapi="} {
		if !strings.Contains(webText, want) {
			t.Fatalf("web log misses %q:\n%s", want, webText)
		}
	}
	if strings.Contains(webText, "onlyapi=present") {
		t.Fatalf("an unselected repository received the selected secret:\n%s", webText)
	}
	webScopes := jobSecretScopes(t, webLane, webRun, chainAliceTok, "j-probe")
	if webScopes["SHARED"] != "organization" || webScopes["ONLY_API"] != "" {
		t.Fatalf("web scopes = %v", webScopes)
	}

	// ---- untrusted: a fork run reads NONE of them ---------------------------
	forkName := "api-fork"
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+org+"/api/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`, org, forkName), chainAliceTok)
	forkURL := s.gitURL(org, forkName)
	pollUntil(t, 60*time.Second, "the fork to be servable", func() bool {
		return g.lsRemoteAuthOK(forkURL, chainAliceTok) != nil
	})
	work := filepath.Join(t.TempDir(), "fork")
	g.initRepo(work)
	g.run(work, "remote", "add", "origin", forkURL)
	g.run(work, "fetch", apiGit, "main")
	g.run(work, "checkout", "-b", "forkbranch", "FETCH_HEAD")
	g.commitFile(work, "fork.txt", "fork change\n", "fork change")
	g.runAuth(work, chainAliceTok, "push", "origin", "forkbranch")
	// The base repository must hold the head objects (the forks' bridge does
	// this in production; the e2e pushes it too).
	g.runAuth(work, chainAliceTok, "push", apiGit, "forkbranch")
	mustAPI(t, "POST", apiLane+"/pulls",
		fmt.Sprintf(`{"title":"fork secret probe","base_ref":"refs/heads/main","head_ref":"refs/heads/forkbranch","fork":{"repo":%q}}`,
			org+"/"+forkName), chainAliceTok)
	var forkRunID string
	var forkHeld bool
	pollUntil(t, 60*time.Second, "the fork run", func() bool {
		res := mustAPI(t, "GET", apiLane+"/threads?n=10", "", chainAliceTok)
		runs, _ := res["runs"].([]any)
		for _, r := range runs {
			m, _ := r.(map[string]any)
			if m["event"] != "pull_request" {
				continue
			}
			forkRunID, _ = m["id"].(string)
			forkHeld = m["state"] == "pending_approval"
			return forkRunID != ""
		}
		return false
	})
	if forkHeld {
		mustAPI(t, "POST", apiLane+"/threads/"+forkRunID+"/approve", "", chainAliceTok)
	}
	pollUntil(t, 120*time.Second, "the fork run to finish", func() bool {
		run := getRun(t, apiLane, forkRunID, chainAliceTok)
		state, _ := run.Run["state"].(string)
		return state == "success" || state == "failure"
	})
	forkView := getRun(t, apiLane, forkRunID, chainAliceTok)
	if trusted, _ := forkView.Run["trusted"].(bool); trusted {
		t.Fatal("a fork run was trusted")
	}
	forkText := logOf(apiLane, forkRunID)
	// Every marker reads empty (the reference resolves, the value is absent),
	// and the annotation/summary fallback proves the reference itself is empty.
	for _, absent := range []string{"org=present", "shared-is-org-", "shared-is-repo-", "onlyapi=present", "short=(ab)"} {
		if strings.Contains(forkText, absent) {
			t.Fatalf("a fork run read a secret (%q):\n%s", absent, forkText)
		}
	}
	for _, want := range []string{"org=", "shared-is-", "onlyapi=", "short=()", "annotation none"} {
		if !strings.Contains(forkText, want) {
			t.Fatalf("a fork run's markers are wrong (missing %q):\n%s", want, forkText)
		}
	}
	for _, leak := range []string{orgToken, orgShared, repoShared, selectedVal} {
		if strings.Contains(forkText, leak) {
			t.Fatalf("a fork run's log leaked %q:\n%s", leak, forkText)
		}
	}
	forkScopes := jobSecretScopes(t, apiLane, forkRunID, chainAliceTok, "j-probe")
	if len(forkScopes) != 0 {
		t.Fatalf("a fork run recorded secret scopes: %v", forkScopes)
	}
	waitForConsole(t, runner, "job complete")
}

// jobSecretScopes reads one job's scope provenance from the run view.
func jobSecretScopes(t *testing.T, lane, runID, token, jobID string) map[string]string {
	t.Helper()
	job := jobState(getRun(t, lane, runID, token), jobID)
	if job == nil {
		t.Fatalf("run %s has no job %s", runID, jobID)
	}
	out := map[string]string{}
	if m, ok := job["secret_scopes"].(map[string]any); ok {
		for k, v := range m {
			s, _ := v.(string)
			out[k] = s
		}
	}
	return out
}

// jobAnnotationsAndSummary reads one job's stored annotations and summary.
func jobAnnotationsAndSummary(t *testing.T, lane, runID, token, jobID string) ([]any, string) {
	t.Helper()
	job := jobState(getRun(t, lane, runID, token), jobID)
	if job == nil {
		t.Fatalf("run %s has no job %s", runID, jobID)
	}
	ann, _ := job["annotations"].([]any)
	summary, _ := job["summary"].(string)
	return ann, summary
}
