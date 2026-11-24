// transfer_test.go — PLAN-V2 §3 T.1 end-to-end: a repository move is a narrated
// `repo-transfer` task and every family that names a repo follows it.
//
// Real server subprocess (OIDC: alice is a host admin), real git binary, real
// fork (the shipped `pull-fork` task) and a real OCI tag. The bucket is read
// from disk afterwards — the assertion is about what the STORE holds, not what
// an API says about itself.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
)

// walkFiles returns every non-directory path under dir (an empty/absent tree
// yields nothing — the filesystem backend leaves empty parent directories
// behind on delete, so a directory check would be the wrong assertion).
func walkFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && !strings.HasSuffix(path, ".lock") && !strings.Contains(path, ".tmp-") {
			// The filesystem backend's flock sidecars and temp files are store
			// bookkeeping, not bucket objects (store/filesystem.go reserves
			// both namespaces).
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// attachTask opens the SSE attach stream for a task id and drains it to the
// terminal frame (bounded). It returns the event kinds seen and the terminal
// payload. The stream is opened on the SOURCE lane right after the 202, so it
// also proves the handle-free attach path (the move tears the source handle
// down before the copy pass).
func attachTask(t *testing.T, url, token string) ([]string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("attach request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("attach %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach %s: status %d", url, resp.StatusCode)
	}
	var kinds []string
	var terminal string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			kinds = append(kinds, event)
			continue
		}
		if strings.HasPrefix(line, "data: ") && (event == "result" || event == "error") {
			terminal = strings.TrimPrefix(line, "data: ")
			return kinds, terminal
		}
	}
	if terminal == "" {
		t.Fatalf("attach stream ended without a terminal frame (events %v)", kinds)
	}
	return kinds, terminal
}

// TestE2E_RepoTransferTaskMovesEveryFamily is the T.1 acceptance scenario:
// seed → fork → package tag → transfer (task) → fork pointer, catalog row and
// package prefix all follow, the old prefix is gone, the new path clones, and
// the old path does not answer.
func TestE2E_RepoTransferTaskMovesEveryFamily(t *testing.T) {
	requireModernGit(t)
	if testing.Short() {
		t.Skip("e2e tier: skipped in -short mode")
	}
	g := newGitClient(t)
	s := chainServer(t)
	owner := testOwner
	repo := uniqueRepo("xfer")
	forkName := repo + "-fork"
	dstOwner := "xferorg"
	storeRoot := filepath.Join(s.dataDir, "store")

	// ---- 1. seed the source repo over real git ------------------------------
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	seedSHA := g.commitFile(work, "hello.txt", "hello transfer\n", "seed commit")
	srcURL := s.gitURL(owner, repo)
	g.run(work, "remote", "add", "origin", srcURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	// ---- 2. fork it (both pointer families are born here) --------------------
	mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`,
			owner, forkName), chainAliceTok)
	forkLane := s.base + "/" + owner + "/" + forkName + "/api"
	pollUntil(t, 60*time.Second, "fork child to become servable", func() bool {
		st, data := apiCall(t, "GET", forkLane, "", chainAliceTok)
		if st != 200 {
			return false
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return false
		}
		return doc["fork_parent"] == owner+"/"+repo
	})

	// ---- 3. an OCI tag under the source (the packages family) ---------------
	image := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` +
		strings.Repeat("a", 64) + `","size":2},"layers":[]}`
	req, err := http.NewRequest(http.MethodPut, s.base+"/v2/"+owner+"/"+repo+"/manifests/v1", strings.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Set("Authorization", "Bearer "+chainAliceTok)
	presp, perr := httpClient.Do(req)
	if perr != nil {
		t.Fatalf("push package tag: %v", perr)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusCreated {
		t.Fatalf("push package tag: status %d, want 201", presp.StatusCode)
	}
	if got := walkFiles(t, filepath.Join(storeRoot, "repos", owner, repo, "packages", "registry", "tags")); len(got) == 0 {
		t.Fatal("package tag did not land in the bucket")
	}

	// ---- 4. seed the catalog row the sweep would have folded ---------------
	// (The maintainer fold is cadence-driven; writing the row through the same
	// CAS helper the sweep uses is the deterministic stand-in for "it ran".)
	dstStore, err := store.NewFilesystemRoot(storeRoot, 4)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	if _, err := sizecatalog.WriteCatalog(t.Context(), dstStore, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		if cur == nil {
			cur = &proto.RepoCatalog{}
		}
		cur.Repos = append(cur.Repos, owner+"/"+repo)
		cur.Entries = append(cur.Entries, &proto.RepoCatalogEntry{
			Repo: owner + "/" + repo, SizeBytes: 4242, ObjectCount: 7, HeadSeq: 3,
			LastCommitSHA: seedSHA,
		})
		return cur, nil
	}); err != nil {
		t.Fatalf("seed catalog row: %v", err)
	}

	// ---- 5. the destination namespace (org gift; alice is its owner) --------
	mustAPI(t, "POST", s.base+"/api/v1/orgs", fmt.Sprintf(`{"org":%q,"display_name":"Transfer Org"}`, dstOwner), chainAliceTok)

	// ---- 6. transfer: 202 + task, attached live -----------------------------
	srcLane := s.base + "/" + owner + "/" + repo + "/api"
	st, body := apiCall(t, "POST", srcLane+"/transfer", fmt.Sprintf(`{"owner":%q}`, dstOwner), chainAliceTok)
	if st != http.StatusAccepted {
		t.Fatalf("POST transfer = %d (%s), want 202", st, body)
	}
	var started struct {
		TaskID string `json:"task_id"`
		Task   struct {
			Kind string `json:"kind"`
			Repo string `json:"repo"`
		} `json:"task"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("202 body: %v (%s)", err, body)
	}
	if started.TaskID == "" || started.Task.Kind != "repo-transfer" || started.Task.Repo != owner+"/"+repo {
		t.Fatalf("202 must carry the repo-transfer task: %s", body)
	}
	kinds, terminal := attachTask(t, srcLane+"/tasks/"+started.TaskID, chainAliceTok)
	sawProgress := false
	for _, k := range kinds {
		if k == "notice" || k == "progress" || k == "task" {
			sawProgress = true
		}
	}
	if !sawProgress || !strings.Contains(terminal, `"kind":"repo-transfer"`) {
		t.Fatalf("attach stream: kinds %v terminal %s", kinds, terminal)
	}
	var final struct {
		Task struct {
			OK       *bool    `json:"ok"`
			Summary  string   `json:"summary"`
			LogTail  []string `json:"log_tail"`
			Finished string   `json:"finished"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(terminal), &final); err != nil {
		t.Fatalf("terminal frame: %v (%s)", err, terminal)
	}
	if final.Task.OK == nil || !*final.Task.OK {
		t.Fatalf("transfer task failed: %s / %s", final.Task.Summary, strings.Join(final.Task.LogTail, " | "))
	}
	if final.Task.Finished == "" {
		t.Fatal("terminal record must be finished")
	}
	joined := strings.Join(final.Task.LogTail, "\n")
	for _, want := range []string{"move committed", "catalog row moved", "verified:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("task narration lacks %q:\n%s", want, joined)
		}
	}

	// ---- 7. the destination serves, the old path does not -------------------
	dstURL := s.gitURL(dstOwner, repo)
	if refs := g.lsRemoteAuth(dstURL, chainAliceTok); refs["refs/heads/main"] != seedSHA {
		t.Fatalf("destination refs = %v, want main=%s", refs, seedSHA)
	}
	cloneDir := filepath.Join(t.TempDir(), "clone")
	if out, cerr := runGitSoft(g, []string{"-c", "http.extraHeader=Authorization: Bearer " + chainAliceTok, "clone", dstURL, cloneDir}); cerr != nil {
		t.Fatalf("clone at the new path: %v\n%s", cerr, out)
	}
	if got, rerr := os.ReadFile(filepath.Join(cloneDir, "hello.txt")); rerr != nil || string(got) != "hello transfer\n" {
		t.Fatalf("cloned content = %q, %v", got, rerr)
	}
	if refs := g.lsRemoteAuthOK(srcURL, chainAliceTok); refs != nil {
		t.Fatalf("old path still serves refs: %v", refs)
	}
	if st, sbody := apiCall(t, "GET", srcLane, "", chainAliceTok); st != 404 {
		t.Fatalf("old path summary = %d (%s), want 404 (redirects are T2)\nserver log:\n%s", st, sbody, s.logs.String())
	}

	// ---- 8. the bucket: old prefix gone, every family followed --------------
	if files := walkFiles(t, filepath.Join(storeRoot, "repos", owner, repo)); len(files) != 0 {
		t.Fatalf("old prefix residue: %v", files)
	}
	mraw, err := os.ReadFile(filepath.Join(storeRoot, "repos", dstOwner, repo, "manifest.pb"))
	if err != nil {
		t.Fatalf("destination manifest: %v", err)
	}
	m, merr := proto.UnmarshalManifest(mraw)
	if merr != nil {
		t.Fatalf("destination manifest decode: %v", merr)
	}
	if m.Repo != dstOwner+"/"+repo {
		t.Fatalf("moved manifest names %q, want %q (the serving copy keys off it)", m.Repo, dstOwner+"/"+repo)
	}
	if files := walkFiles(t, filepath.Join(storeRoot, "repos", dstOwner, repo, "packages", "registry", "tags")); len(files) == 0 {
		t.Fatal("OCI tag did not follow the move")
	}
	// Catalog: the row moved with its fields.
	cat, cerr := sizecatalog.ReadCatalog(t.Context(), dstStore)
	if cerr != nil {
		t.Fatalf("catalog read: %v", cerr)
	}
	var row *proto.RepoCatalogEntry
	for _, e := range cat.Entries {
		if e == nil {
			continue
		}
		if e.Repo == owner+"/"+repo {
			t.Fatalf("catalog still names the old path: %+v", e)
		}
		if e.Repo == dstOwner+"/"+repo {
			row = e
		}
	}
	if row == nil || row.SizeBytes != 4242 || row.LastCommitSHA != seedSHA {
		t.Fatalf("catalog row did not follow: %+v", row)
	}
	// Fork pointers: the child's provenance names the new parent, and the
	// moved index still lists the child (it travelled with the prefix).
	forkRaw, err := os.ReadFile(filepath.Join(storeRoot, "repos", owner, forkName, "fork.json"))
	if err != nil {
		t.Fatalf("child fork.json: %v", err)
	}
	var fd struct {
		Parent string `json:"parent"`
		Root   string `json:"root"`
	}
	if err := json.Unmarshal(forkRaw, &fd); err != nil {
		t.Fatalf("child fork.json decode: %v", err)
	}
	if fd.Parent != dstOwner+"/"+repo || (fd.Root != "" && fd.Root != dstOwner+"/"+repo) {
		t.Fatalf("child fork pointers = %+v, want parent %s/%s", fd, dstOwner, repo)
	}
	idxRaw, err := os.ReadFile(filepath.Join(storeRoot, "repos", dstOwner, repo, "meta", "forks.json"))
	if err != nil {
		t.Fatalf("moved fork index: %v", err)
	}
	if !strings.Contains(string(idxRaw), owner+"/"+forkName) {
		t.Fatalf("moved index lost the child: %s", idxRaw)
	}
	// The child still clones (the forkread pack fallback resolves through the
	// rewritten pointer).
	if refs := g.lsRemoteAuth(s.gitURL(owner, forkName), chainAliceTok); refs["refs/heads/main"] != seedSHA {
		t.Fatalf("child refs after the parent moved: %v", refs)
	}

	// ---- 9. re-running the same move converges (the task is re-runnable) ----
	// The source is gone and the destination exists: the pre-flight accepts the
	// pair only because the destination reproduces the retargeted manifest, so
	// the task resumes at the cross-reference/verification phases instead of
	// copying anything — and never damages the live repository.
	st, body = apiCall(t, "POST", srcLane+"/transfer", fmt.Sprintf(`{"owner":%q}`, dstOwner), chainAliceTok)
	if st != http.StatusAccepted {
		t.Fatalf("re-run = %d (%s), want 202 (a resumable pair)", st, body)
	}
	var again struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &again); err != nil || again.TaskID == "" {
		t.Fatalf("re-run body: %v (%s)", err, body)
	}
	// The source lane is gone, so the record is polled through the destination
	// lane (the same instance record, a repo that exists).
	dstLane := s.base + "/" + dstOwner + "/" + repo + "/api"
	pollUntil(t, 60*time.Second, "re-run task to finish ok", func() bool {
		code, data := apiCall(t, "GET", dstLane+"/tasks/"+again.TaskID, "", chainAliceTok)
		if code != 200 {
			return false
		}
		var rec struct {
			Finished string `json:"finished"`
			OK       *bool  `json:"ok"`
		}
		if err := json.Unmarshal(data, &rec); err != nil {
			return false
		}
		return rec.Finished != "" && rec.OK != nil && *rec.OK
	})
	if refs := g.lsRemoteAuth(dstURL, chainAliceTok); refs["refs/heads/main"] != seedSHA {
		t.Fatalf("re-run damaged the destination: %v", refs)
	}
}
