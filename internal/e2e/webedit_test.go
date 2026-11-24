// webedit_test.go — Feature F's end-to-end proof (docs/PLAN-V2.md §4.11):
// a browser edit is a PUSH. The edit lands through the API, real git confirms
// the commit/tree/ref, the configured webhook delivers the ref event, Threads
// creates its push run, policy refuses a protected direct edit, and the
// new-branch + PR path merges.
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// editServer boots the binary against the in-test OIDC IdP (alice is in the
// admin group), with a real webhook sink and a Threads-capable config. The
// sweep interval is an hour so any event delivery must come from the publish
// wake-up.
func editServer(t *testing.T, hookURL string) *e2eServer {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	oidp := newOIDP(t)
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

%s
[events]
webhook_url = %q
sweep_interval = "1h"

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, oidp.configBlock(), hookURL,
		filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	e2eDefaultBearer = oidp.idToken("alice@example.com", "spindle-admin")
	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)
	return s
}

// authorName is the display name the OIDC principal resolves to: the local
// part of the verified email when the IdP sends no name claim.
var authorName = strings.SplitN(chainAlice, "@", 2)[0]

// editBody renders the contents API body.
func editBody(message, content string, extra map[string]any) string {
	m := map[string]any{"message": message, "content": base64.StdEncoding.EncodeToString([]byte(content))}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestE2E_WebeditDirectEditIsAPush(t *testing.T) {
	if testing.Short() {
		t.Skip("webedit e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)

	// Webhook sink.
	type got struct{ body []byte }
	hooks := make(chan got, 32)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readLimited(r)
		select {
		case hooks <- got{body: body}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	g := newGitClient(t)
	s := editServer(t, sink.URL)
	repo := uniqueRepo("webedit")
	owner := testOwner
	lane := s.base + "/" + owner + "/" + repo
	url := s.gitURL(owner, repo)

	// Seed: a workflow (so a push has a CI trigger) and the edited file.
	work := filepath.Join(t.TempDir(), "seed")
	g.initRepo(work)
	seedSHA := g.commitFile(work, ".spindle/workflows/ci.yml", triggerWorkflow, "workflow")
	seedSHA = g.commitFile(work, "hello.txt", "hello\n", "seed")
	blobSHA := g.run(work, "rev-parse", "HEAD:hello.txt")
	g.run(work, "remote", "add", "origin", url)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	// ---- 1. edit main directly through the API ---------------------------------
	editSHA := ""
	rec := mustAPI(t, "PUT", lane+"/api/contents?ref=main&path=hello.txt",
		editBody("Update hello", "hello from the browser\n", map[string]any{
			"description":         "browser edit",
			"expected_blob_sha":   blobSHA,
			"expected_branch_sha": seedSHA,
		}), e2eDefaultBearer)
	commit, _ := rec["commit"].(map[string]any)
	editSHA, _ = commit["sha"].(string)
	if editSHA == "" || editSHA == seedSHA {
		t.Fatalf("edit response = %v", rec)
	}
	if branch, _ := rec["branch"].(string); branch != "main" {
		t.Fatalf("branch = %q", branch)
	}

	// ---- 2. real git sees the commit, tree and ref -----------------------------
	cloneParent := t.TempDir()
	g.runAuth(cloneParent, e2eDefaultBearer, "clone", url, filepath.Join(cloneParent, "clone"))
	clone := filepath.Join(cloneParent, "clone")
	if got := g.run(clone, "rev-parse", "HEAD"); got != editSHA {
		t.Fatalf("cloned HEAD = %q, want edit commit %q", got, editSHA)
	}
	if got := g.run(clone, "show", "HEAD:hello.txt"); got != "hello from the browser" {
		t.Fatalf("cloned content = %q", got)
	}
	// The author is the signed-in principal: the IdP's email is the identity, and
	// the display name comes from the local part (no name claim in the stub).
	if got := g.run(clone, "log", "-1", "--format=%an <%ae>"); got != authorName+" <"+chainAlice+">" {
		t.Fatalf("author = %q, want %q", got, authorName+" <"+chainAlice+">")
	}
	if got := g.run(clone, "log", "-1", "--format=%cn"); got != "spindle" {
		t.Fatalf("committer = %q", got)
	}
	if got := g.run(clone, "log", "-1", "--format=%s%n%b"); !strings.Contains(got, "Update hello") || !strings.Contains(got, "browser edit") {
		t.Fatalf("message = %q", got)
	}
	refs := g.lsRemoteAuthOK(url, e2eDefaultBearer)
	if refs["refs/heads/main"] != editSHA {
		t.Fatalf("remote main = %q, want %q", refs["refs/heads/main"], editSHA)
	}
	// The workflow file survived: the edit is one tree change, not a replace.
	if got := g.run(clone, "show", "HEAD:.spindle/workflows/ci.yml"); !strings.Contains(got, "Triggers") {
		t.Fatalf("workflow file lost: %q", got)
	}

	// ---- 3. the configured webhook delivers the ref event -----------------------
	var ev refEvent
	deadline := time.After(30 * time.Second)
	for ev.New != editSHA {
		var d got
		select {
		case d = <-hooks:
		case <-deadline:
			t.Fatalf("no webhook delivery carrying %s", editSHA)
		}
		var events []refEvent
		if err := json.Unmarshal(d.body, &events); err != nil {
			continue
		}
		for _, e := range events {
			if e.RefName == "refs/heads/main" && e.New == editSHA {
				ev = e
			}
		}
	}
	if ev.Action != "update" || ev.Old != seedSHA || ev.Pusher != authorName {
		t.Fatalf("event = %+v, want update %s→%s by %s", ev, seedSHA, editSHA, authorName)
	}

	// ---- 4. Threads creates the push run (the push wake-up, not the sweep) -----
	pollUntil(t, 60*time.Second, "Threads push run for the edit", func() bool {
		res := mustAPI(t, "GET", lane+"/api/threads?n=100", "", e2eDefaultBearer)
		runs, _ := res["runs"].([]any)
		for _, raw := range runs {
			r, _ := raw.(map[string]any)
			if r == nil {
				continue
			}
			if r["event"] == "push" && r["commit"] == editSHA {
				return true
			}
		}
		return false
	})

	// ---- 5. a stale base is a 409 naming both shas ------------------------------
	st, body := apiCall(t, "PUT", lane+"/api/contents?ref=main&path=hello.txt",
		editBody("stale edit", "should not land\n", map[string]any{
			"expected_blob_sha":   blobSHA,
			"expected_branch_sha": seedSHA,
		}), e2eDefaultBearer)
	if st != http.StatusConflict {
		t.Fatalf("stale edit status = %d body=%s", st, body)
	}
	if !strings.Contains(string(body), seedSHA) || !strings.Contains(string(body), editSHA) {
		t.Fatalf("409 does not name both shas: %s", body)
	}

	// ---- 6. required-reviews refuses a direct edit; a proposed branch passes ---
	policyDoc := `{"version":1,"rules":[{"name":"reviews","match":{"refs":["refs/heads/main"]},` +
		`"effect":{"required-reviews":{"min_approvals":1}}}]}`
	mustAPI(t, "PUT", lane+"/api/policy", policyDoc, e2eDefaultBearer)
	st, body = apiCall(t, "PUT", lane+"/api/contents?ref=main&path=hello.txt",
		editBody("blocked", "nope\n", map[string]any{
			"expected_blob_sha":   g.run(clone, "rev-parse", "HEAD:hello.txt"),
			"expected_branch_sha": editSHA,
		}), e2eDefaultBearer)
	if st != http.StatusForbidden {
		t.Fatalf("review-gated direct edit = %d %s", st, body)
	}
	if !strings.Contains(string(body), "reviews") {
		t.Fatalf("403 does not name the rule: %s", body)
	}
	prRes := mustAPI(t, "PUT", lane+"/api/contents?ref=main&path=hello.txt",
		editBody("Propose hello", "proposed\n", map[string]any{
			"expected_blob_sha": g.run(clone, "rev-parse", "HEAD:hello.txt"),
			"new_branch":        "propose/hello",
			"open_pr":           true,
		}), e2eDefaultBearer)
	if pr, _ := prRes["pr"].(map[string]any); pr == nil {
		t.Fatalf("no PR on the propose path: %v", prRes)
	}
	if refs := g.lsRemoteAuthOK(url, e2eDefaultBearer); refs["refs/heads/propose/hello"] == "" {
		t.Fatalf("proposed branch not published: %v", refs)
	}
}

func TestE2E_WebeditNewBranchPRMergesAndRaces(t *testing.T) {
	if testing.Short() {
		t.Skip("webedit e2e scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readLimited(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	s := editServer(t, sink.URL)
	repo := uniqueRepo("webeditpr")
	owner := testOwner
	lane := s.base + "/" + owner + "/" + repo
	url := s.gitURL(owner, repo)

	work := filepath.Join(t.TempDir(), "seed")
	g.initRepo(work)
	seedSHA := g.commitFile(work, "readme.md", "readme\n", "seed")
	blobSHA := g.run(work, "rev-parse", "HEAD:readme.md")
	g.run(work, "remote", "add", "origin", url)
	g.runAuth(work, e2eDefaultBearer, "push", "-u", "origin", "main")

	// New branch + PR through one API call.
	prRes := mustAPI(t, "PUT", lane+"/api/contents?ref=main&path=readme.md",
		editBody("Propose readme", "readme edited\n", map[string]any{
			"expected_blob_sha":   blobSHA,
			"expected_branch_sha": seedSHA,
			"new_branch":          "edit/readme",
			"open_pr":             true,
			"pr_title":            "Edit the readme",
		}), e2eDefaultBearer)
	pr, _ := prRes["pr"].(map[string]any)
	if pr == nil {
		t.Fatalf("pr = %v", prRes["pr"])
	}
	prNum := int(pr["num"].(float64))

	// Merge it (maintain+) and wait for the async task.
	st, body := apiCall(t, "POST", fmt.Sprintf("%s/api/pulls/%d/merge", lane, prNum), `{"strategy":"merge"}`, e2eDefaultBearer)
	if st != http.StatusAccepted {
		t.Fatalf("merge start = %d %s", st, body)
	}
	pollUntil(t, 90*time.Second, "PR merge", func() bool {
		view := mustAPI(t, "GET", fmt.Sprintf("%s/api/pulls/%d", lane, prNum), "", e2eDefaultBearer)
		doc, _ := view["pr"].(map[string]any)
		merged, _ := doc["merged"].(bool)
		state, _ := doc["state"].(string)
		return merged || state == "merged"
	})
	refs := g.lsRemoteAuthOK(url, e2eDefaultBearer)
	if refs["refs/heads/main"] == seedSHA || refs["refs/heads/main"] == "" {
		t.Fatalf("main did not move after merge: %v", refs)
	}
	cloneParent := t.TempDir()
	g.runAuth(cloneParent, e2eDefaultBearer, "clone", url, filepath.Join(cloneParent, "clone"))
	clone := filepath.Join(cloneParent, "clone")
	if got := g.run(clone, "show", "HEAD:readme.md"); got != "readme edited" {
		t.Fatalf("merged content = %q", got)
	}

	// Two racing editors from the same base: exactly one wins, the loser 409s.
	baseSHA := refs["refs/heads/main"]
	baseBlob := g.run(clone, "rev-parse", "HEAD:readme.md")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, body := apiCall(t, "PUT", lane+"/api/contents?ref=main&path=readme.md",
				editBody(fmt.Sprintf("race %d", i), fmt.Sprintf("race %d\n", i), map[string]any{
					"expected_blob_sha":   baseBlob,
					"expected_branch_sha": baseSHA,
				}), e2eDefaultBearer)
			codes[i], bodies[i] = st, string(body)
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			ok++
		case http.StatusConflict:
			conflict++
			if !strings.Contains(bodies[i], baseSHA) {
				t.Fatalf("losing 409 does not name the page sha: %s", bodies[i])
			}
		default:
			t.Fatalf("race status = %d %s", code, bodies[i])
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("race outcomes = %v (want one 201, one 409)", codes)
	}
}
