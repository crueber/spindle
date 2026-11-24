// backup_test.go — the backup/restore drill (docs/BACKUP.md).
//
// One instance seeds a repository (real git push), an issue, an OCI package
// tag and a Threads run (the run RECORD: it is created by the push itself and
// sits queued, because this drill deliberately uses no runner container — the
// executor is covered by threads_runner_test.go). The store is then snapshotted
// with the documented commands (scripts/backup-store.sh), restored into a
// fresh, empty instance (scripts/restore-store.sh), and every surface is read
// back and compared by semantic value, not by producer bytes.
//
// The negative half proves the documented failure mode: a copy taken in the
// push window holds the new log segment and pack but the OLD manifest, because
// the manifest CAS is the commit point. That store still boots and serves the
// old head; the new commit simply is not there.
//
// Skipped in -short mode. No docker, no network beyond loopback: filesystem
// store, real git, the in-test IdP.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store/proto"
)

// backupDrillConfig writes a normally-booting spindle.toml for one instance.
// storeRoot is separate from dataDir on the restored instance, which is how
// "restore into a fresh data dir" works with the same config shape.
func backupDrillConfig(t *testing.T, dataDir string, p *oidp, storeRoot string) {
	t.Helper()
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), storeRoot, filepath.Join(dataDir, "cache")))
}

// startBackupInstance boots a spindle subprocess on its own data dir/port.
func startBackupInstance(t *testing.T, bin, dataDir, storeRoot string, p *oidp, port int) *e2eServer {
	t.Helper()
	backupDrillConfig(t, dataDir, p, storeRoot)
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)
	return s
}

// runBackupScript runs one of the shipped, documented scripts and fails the
// test with its output. The drill must exercise the procedure a human would
// run, not a private copy path.
func runBackupScript(t *testing.T, root, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(root, "scripts", script)}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", script, args, err, out)
	}
	return string(out)
}

// copyNonManifest overlays every store object from src onto dst EXCEPT the
// manifest: the shape a live copy produces when it runs between a push's log
// segment/pack PUTs and the manifest CAS. Lock/temp sidecars are store
// bookkeeping and are skipped (restore-store.sh's archive carries whatever the
// quiesced store had; a live copy cannot rely on them).
func copyNonManifest(t *testing.T, src, dst string) int {
	t.Helper()
	copied := 0
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if base == "manifest.pb" || strings.HasSuffix(base, ".lock") || strings.Contains(base, ".tmp-") {
			return nil
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dst, rel)
		if merr := os.MkdirAll(filepath.Dir(out), 0o700); merr != nil {
			return merr
		}
		if werr := os.WriteFile(out, data, info.Mode().Perm()); werr != nil {
			return werr
		}
		_ = os.Chtimes(out, info.ModTime(), info.ModTime())
		copied++
		return nil
	})
	if err != nil {
		t.Fatalf("overlay non-manifest objects: %v", err)
	}
	return copied
}

// issueShape is the semantic value the drill compares across instances.
type issueShape struct {
	Title    string `json:"title"`
	State    string `json:"state"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	Comments int    `json:"comment_count"`
}

func readIssue(t *testing.T, lane string, num int, token string) issueShape {
	t.Helper()
	var view struct {
		Thread struct {
			Title        string `json:"title"`
			State        string `json:"state"`
			Author       string `json:"author"`
			CommentCount int    `json:"comment_count"`
		} `json:"thread"`
		Events []struct {
			Body *string `json:"body"`
		} `json:"events"`
	}
	getJSONAuth(t, fmt.Sprintf("%s/issues/%d", lane, num), token, &view)
	body := ""
	for _, ev := range view.Events {
		if ev.Body != nil && *ev.Body != "" {
			body = *ev.Body
			break
		}
	}
	if body == "" {
		t.Fatalf("issue #%d has no body event", num)
	}
	return issueShape{
		Title:    view.Thread.Title,
		State:    view.Thread.State,
		Author:   view.Thread.Author,
		Body:     body,
		Comments: view.Thread.CommentCount,
	}
}

// getJSONAuth is getJSON with a preemptive bearer (the e2e config's JSON lanes
// accept the in-test IdP's ID token for reads that a repo gate protects).
func getJSONAuth(t *testing.T, url, token string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode JSON: %v", url, err)
	}
}

// packageShape is the tag → manifest identity the drill compares.
type packageShape struct {
	Tag      string          `json:"tag"`
	Digest   string          `json:"digest"`
	Media    string          `json:"media_type"`
	Manifest json.RawMessage `json:"manifest"`
}

func readPackage(t *testing.T, lane, tag, token string) packageShape {
	t.Helper()
	var list struct {
		Packages []struct {
			Tag    string `json:"tag"`
			Digest string `json:"digest"`
		} `json:"packages"`
	}
	getJSONAuth(t, lane+"/packages", token, &list)
	digest := ""
	for _, p := range list.Packages {
		if p.Tag == tag {
			digest = p.Digest
		}
	}
	if digest == "" {
		t.Fatalf("package tag %q not listed (packages: %+v)", tag, list.Packages)
	}
	var view struct {
		Digest    string          `json:"digest"`
		MediaType string          `json:"media_type"`
		Manifest  json.RawMessage `json:"manifest"`
	}
	getJSONAuth(t, lane+"/packages/"+digest, token, &view)
	if view.Digest != digest || view.MediaType == "" || len(view.Manifest) == 0 {
		t.Fatalf("package %s view is incomplete: %+v", digest, view)
	}
	return packageShape{Tag: tag, Digest: digest, Media: view.MediaType, Manifest: view.Manifest}
}

// runShape is the Threads run-record identity the drill compares. Logs are not
// part of it: no runner executed this run (see the file header).
type runShape struct {
	ID           string `json:"id"`
	Commit       string `json:"commit"`
	Event        string `json:"event"`
	Branch       string `json:"branch"`
	Workflow     string `json:"workflow"`
	WorkflowPath string `json:"workflow_path"`
	State        string `json:"state"`
	Trusted      bool   `json:"trusted"`
}

func readRun(t *testing.T, lane, runID, token string) runShape {
	t.Helper()
	var view struct {
		Run *runShape `json:"run"`
	}
	getJSONAuth(t, lane+"/threads/"+runID, token, &view)
	if view.Run == nil {
		t.Fatalf("threads run %s: empty view", runID)
	}
	return *view.Run
}

// waitBackupRun polls the run list for the push of sha (run creation is the
// push→events wake-up path, no runner involved).
func waitBackupRun(t *testing.T, lane, sha, token string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var page struct {
			Runs []runShape `json:"runs"`
		}
		getJSONAuth(t, lane+"/threads?n=100", token, &page)
		for _, r := range page.Runs {
			if r.Event == "push" && r.Commit == sha {
				return r.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no push run for %s within 30s (the push never woke the events bridge)", sha)
	return ""
}

// TestE2E_BackupRestoreDrill is the docs/BACKUP.md drill: seed, snapshot with
// the shipped script, restore into an empty instance, compare every surface,
// then prove the mid-push window (old manifest, new objects) still opens.
func TestE2E_BackupRestoreDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("backup drill is the e2e tier (make backup-drill), skipped in -short mode")
	}
	requireModernGit(t)

	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	bin := buildSpindle(t)
	g := newGitClient(t)
	p := newOIDP(t)
	chainAliceTok = p.idToken(chainAlice, "spindle-admin")
	e2eDefaultBearer = chainAliceTok
	owner, repo := testOwner, uniqueRepo("backup")

	// ---- 1. instance A: seed every surface --------------------------------
	dataA := t.TempDir()
	storeA := filepath.Join(dataA, "store")
	a := startBackupInstance(t, bin, dataA, storeA, p, freePort(t))
	laneA := a.base + "/" + owner + "/" + repo + "/api"

	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/backup.yml", `name: Backup drill
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo "backup drill"
`, "workflow")
	seedSHA := g.commitFile(work, "hello.txt", "hello backup\n", "seed commit")
	g.run(work, "remote", "add", "origin", a.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	issue := mustAPI(t, "POST", laneA+"/issues",
		`{"title":"Backup drill issue","body":"the body must survive a restore"}`, chainAliceTok)
	issueNum := int(jget(t, issue, "thread", "num").(float64))
	wantIssue := readIssue(t, laneA, issueNum, chainAliceTok)

	image := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` +
		strings.Repeat("b", 64) + `","size":2},"layers":[]}`
	req, err := http.NewRequest(http.MethodPut, a.base+"/v2/"+owner+"/"+repo+"/manifests/v1", strings.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Set("Authorization", "Bearer "+chainAliceTok)
	resp, perr := httpClient.Do(req)
	if perr != nil {
		t.Fatalf("push package tag: %v", perr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push package tag: status %d, want 201", resp.StatusCode)
	}
	wantPkg := readPackage(t, laneA, "v1", chainAliceTok)

	runID := waitBackupRun(t, laneA, seedSHA, chainAliceTok)
	wantRun := readRun(t, laneA, runID, chainAliceTok)

	// ---- 2. the documented backup: quiesce, then the shipped script --------
	a.stop()
	archive := filepath.Join(t.TempDir(), "spindle-store.tar.gz")
	t.Logf("backup: %s", strings.TrimSpace(runBackupScript(t, root, "backup-store.sh",
		"--from", storeA, "--to", archive)))

	// ---- 3. restore into a second, empty instance --------------------------
	dataC := t.TempDir()
	storeC := filepath.Join(dataC, "store")
	if err := os.MkdirAll(storeC, 0o700); err != nil {
		t.Fatal(err)
	}
	runBackupScript(t, root, "restore-store.sh", "--from", archive, "--to", storeC)
	c := startBackupInstance(t, bin, dataC, storeC, p, freePort(t))
	laneC := c.base + "/" + owner + "/" + repo + "/api"

	// Repo: the clone and its content, not the pack bytes.
	refs := g.lsRemoteAuth(c.gitURL(owner, repo), chainAliceTok)
	if refs["refs/heads/main"] != seedSHA {
		t.Fatalf("restored refs = %v, want main=%s", refs, seedSHA)
	}
	cloneDir := filepath.Join(t.TempDir(), "clone")
	g.runAuth(filepath.Dir(cloneDir), chainAliceTok, "clone", c.gitURL(owner, repo), cloneDir)
	if got, rerr := os.ReadFile(filepath.Join(cloneDir, "hello.txt")); rerr != nil || string(got) != "hello backup\n" {
		t.Fatalf("restored clone content = %q, %v", got, rerr)
	}

	// Issue, package tag/manifest, run record.
	gotIssue := readIssue(t, laneC, issueNum, chainAliceTok)
	if !reflect.DeepEqual(gotIssue, wantIssue) {
		t.Fatalf("issue after restore = %+v, want %+v", gotIssue, wantIssue)
	}
	gotPkg := readPackage(t, laneC, "v1", chainAliceTok)
	if gotPkg.Digest != wantPkg.Digest || gotPkg.Media != wantPkg.Media {
		t.Fatalf("package after restore = %+v, want digest %s media %s", gotPkg, wantPkg.Digest, wantPkg.Media)
	}
	if !reflect.DeepEqual(canonicalJSON(t, gotPkg.Manifest), canonicalJSON(t, wantPkg.Manifest)) {
		t.Fatal("package manifest document differs after restore")
	}
	gotRun := readRun(t, laneC, runID, chainAliceTok)
	if !reflect.DeepEqual(gotRun, wantRun) {
		t.Fatalf("threads run after restore = %+v, want %+v", gotRun, wantRun)
	}

	// ---- 4. the negative: a copy caught in the push window ----------------
	// Bring A back, advance main, stop it. The store then holds the new log
	// segment and pack AND the new manifest; a live copy that ran before the
	// manifest CAS would have the first two only. Reproduce that captured
	// state deterministically: start from the backup, overlay every object
	// except manifest.pb.
	// Same port: the seed work tree's `origin` still points at it.
	b := startBackupInstance(t, bin, dataA, storeA, p, a.port)
	newSHA := g.commitFile(work, "hello.txt", "after the backup\n", "post-backup commit")
	g.runAuth(work, chainAliceTok, "push", "origin", "main")
	if refs := g.lsRemoteAuth(b.gitURL(owner, repo), chainAliceTok); refs["refs/heads/main"] != newSHA {
		t.Fatalf("pre-capture head = %v, want %s", refs, newSHA)
	}
	b.stop()

	oldManifest, rerr := os.ReadFile(filepath.Join(storeC, "repos", owner, repo, "manifest.pb"))
	if rerr != nil {
		t.Fatalf("restored manifest: %v", rerr)
	}
	newManifest, rerr := os.ReadFile(filepath.Join(storeA, "repos", owner, repo, "manifest.pb"))
	if rerr != nil {
		t.Fatalf("post-push manifest: %v", rerr)
	}
	oldHead, oerr := proto.UnmarshalManifest(oldManifest)
	if oerr != nil {
		t.Fatalf("restored manifest decode: %v", oerr)
	}
	newHead, nerr := proto.UnmarshalManifest(newManifest)
	if nerr != nil {
		t.Fatalf("post-push manifest decode: %v", nerr)
	}
	if newHead.HeadSeq <= oldHead.HeadSeq {
		t.Fatalf("premise broken: the post-backup push did not advance head_seq (%d -> %d)",
			oldHead.HeadSeq, newHead.HeadSeq)
	}

	dataD := t.TempDir()
	storeD := filepath.Join(dataD, "store")
	if err := os.MkdirAll(storeD, 0o700); err != nil {
		t.Fatal(err)
	}
	// The captured store is the backup plus the newer objects; the old
	// manifest stays the commit point.
	runBackupScript(t, root, "restore-store.sh", "--from", archive, "--to", storeD)
	if copied := copyNonManifest(t, storeA, storeD); copied == 0 {
		t.Fatal("overlay copied nothing: the push window is not representable")
	}
	d := startBackupInstance(t, bin, dataD, storeD, p, freePort(t))
	laneD := d.base + "/" + owner + "/" + repo + "/api"

	// Consistent, at the old head: the new commit is not visible, the old
	// surfaces still read, the instance is healthy.
	if status, _ := httpGet(d.base+"/readyz", 2*time.Second); status != http.StatusOK {
		t.Fatalf("/readyz on the mid-push restore = %d, want 200", status)
	}
	refs = g.lsRemoteAuth(d.gitURL(owner, repo), chainAliceTok)
	if refs["refs/heads/main"] != seedSHA {
		t.Fatalf("mid-push store serves %v, want the pre-push head %s", refs, seedSHA)
	}
	if got := readIssue(t, laneD, issueNum, chainAliceTok); !reflect.DeepEqual(got, wantIssue) {
		t.Fatalf("mid-push issue = %+v, want %+v", got, wantIssue)
	}
	// The post-backup run record landed in the captured objects, but no
	// manifest names the new commit: the run list may show it and the repo
	// still reads at the old head. What must hold is that reads work.
	if got := readRun(t, laneD, runID, chainAliceTok); got.Commit != seedSHA {
		t.Fatalf("original run after mid-push restore = %+v, want commit %s", got, seedSHA)
	}
}

// canonicalJSON decodes and re-encodes a JSON document so two producers'
// whitespace/key order cannot masquerade as a semantic difference.
func canonicalJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonical JSON: %v (%s)", err, raw)
	}
	return v
}
