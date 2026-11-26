// Conformance corpus and harness (PAR-200, PAR-201).
//
// Tier 1 — parse/plan (this file, runs in -short, no docker):
//
//	every corpus file parses and yields the documented jobs/steps, and the
//	engine's real run-creation path (discovery → CreateRun → job specs) accepts
//	it. A file that still needs an open row must carry its DOCUMENTED refusal (an
//	UNTRANSLATED marker, or a parse error the entry declares); when the matrix
//	turns that row green the entry flips to the strict assertion, so "it does
//	not work yet" can never hide.
//
// Tier 2 — execution (TestE2E_ParityExecution, opt-in: SPINDLE_PARITY_EXEC=1 and
// a docker daemon): a real server plus a real runner execute a corpus-shaped
// workflow to completion. The PR gate never needs docker.
//
// The corpus lives at testdata/parity/. A source file is a byte copy of a real
// repository's workflow with a SOURCE.json stamp (repository, path, commit,
// fetch date, sha256, golden sha256, importer revision, translation); the
// converted file (`<slug>.yml`, no `.source`) is the importer's golden. The
// conversion is a real translation (runs-on, needs, $GITHUB_SERVER_URL,
// dispatch-input aliases, and an UNTRANSLATED marker for matrix.include), so the
// goldens differ from their sources; the stamp records why, and
// TestParityImporterReproducesGoldens proves the importer still produces them.
//
// The copies are sanitized: the upstream host is rewritten to the neutral
// `git.example.com` in every source and golden. The repository, path and commit
// provenance is real, but the host is not: the publish transform rewrites the
// private estate's host in every text file, so a fixture that carried it would
// be rewritten on the way out and its byte stamps would stop matching. Run
// `go test ./internal/e2e -run TestParityGoldensMatchTheirStamps -update` to
// regenerate the goldens and restamp SOURCE.json from the sources through the
// real importer; TestParityFixturesCarryNoPrivateHost keeps the host out.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

const parityDir = "testdata/parity"

// paritySource is one SOURCE.json entry.
type paritySource struct {
	Slug       string `json:"slug"`
	System     string `json:"system"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Commit     string `json:"commit"`
	Fetched    string `json:"fetched"`
	SHA256     string `json:"sha256"`
	// GoldenSHA256 pins the converted file the importer must reproduce.
	GoldenSHA256 string `json:"golden_sha256"`
	// ImporterCommit is the importer revision the golden came from: the last
	// commit that changed the importer's production code (test and testdata
	// changes cannot move a converted byte, so they do not move this).
	ImporterCommit string `json:"importer_commit"`
	// Translation records what the conversion changes (empty = path move only).
	Translation string `json:"translation"`
}

type parityStamp struct {
	Entries []paritySource `json:"entries"`
	// Note records corpus-wide provenance the per-entry fields cannot: the host
	// neutralization every copied file carries so the publish transform cannot
	// rewrite its bytes.
	Note string `json:"note,omitempty"`
}

// update regenerates what the corpus pins
// (`go test ./internal/e2e -run TestParityGoldensMatchTheirStamps -update`): the
// converted goldens come from the sources through the real importer and
// SOURCE.json is restamped from the bytes it just wrote. It is the only
// supported way to move a stamp.
var update = flag.Bool("update", false, "regenerate the converted goldens and restamp SOURCE.json")

// parityJob is one expected YAML job: its key and its step count.
type parityJob struct {
	Key   string
	Steps int
}

// parityEntry is one corpus file and what the engine must make of it.
type parityEntry struct {
	Slug string
	// DependsOn are the PAR- ids the file still needs. While any is not `have`
	// in the matrix, the entry asserts the documented refusal instead of the
	// strict shape: Refusal when the file cannot parse at all, or
	// ExpectUntranslated when the importer marked a key it cannot translate (the
	// converted file parses, but a construct is gone and named).
	DependsOn []string
	Refusal   string
	// ExpectUntranslated are fragments the CONVERTED file must carry while the
	// blocking row is open. Absent when the row is green (and then a fragment
	// that is still there fails with a message naming the entry).
	ExpectUntranslated []string
	// Jobs are the file's YAML jobs in document order.
	Jobs []parityJob
	// Event/Branch/Ref drive run creation (the trigger the corpus entry
	// declares). A tags-only workflow must be driven by a TAG (wave 1a): a tag
	// carries no branch, and RefKind says which glob set matched it.
	Event   string
	Branch  string
	Ref     string
	RefName string
	Tag     bool
}

// corpus is the Wave-1 Forgejo corpus (docs/features/21_parity_master.md §1).
func corpus() []parityEntry {
	return []parityEntry{
		{
			Slug:   "weave-ci",
			Event:  threads.EventPush,
			Branch: "main",
			Ref:    "refs/heads/main",
			// The release job's `if: startsWith(github.ref, 'refs/tags/v')` is
			// outside the expression subset until PAR-037. It has a `needs:`, so
			// the needs cascade decides it once `test` concludes — with a runner,
			// not here: the plan tier pins the job and its steps, and PAR-037's
			// own table test pins the evaluation.
			Jobs: []parityJob{
				{Key: "test", Steps: 3},
				{Key: "package", Steps: 3},
				{Key: "release", Steps: 3},
			},
		},
		{
			Slug:   "weave-publish-github",
			Event:  threads.EventDispatch,
			Branch: "main",
			Ref:    "refs/heads/main",
			Jobs:   []parityJob{{Key: "publish", Steps: 2}},
		},
		{
			Slug:   "scriobh-ci",
			Event:  threads.EventPush,
			Branch: "main",
			Ref:    "refs/heads/main",
			Jobs: []parityJob{
				{Key: "test", Steps: 9},
				{Key: "editor", Steps: 2},
				{Key: "container", Steps: 3},
			},
		},
		{
			// matrix.include is translated verbatim by the importer (PAR-051),
			// so `cli` is a four-cell matrix job. The file is TAGS-ONLY
			// (`on.push.tags: ["v*"]`), so it is driven by a tag push: a branch
			// push must not fire it (wave 1a, PAR-011) and Run.Branch stays empty.
			Slug:    "scriobh-release",
			Event:   threads.EventPush,
			Ref:     "refs/tags/v1.2.3",
			RefName: "v1.2.3",
			Tag:     true,
			Jobs: []parityJob{
				{Key: "prepare", Steps: 4},
				{Key: "cli", Steps: 4},
				{Key: "docker", Steps: 3},
				{Key: "tauri-linux", Steps: 5},
				{Key: "tauri-windows", Steps: 6},
				{Key: "tauri-macos", Steps: 9},
			},
		},
	}
}

// parityRowStatus reads parity.json (the gate's source of truth) and returns
// status by row id.
func parityRowStatus(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/features/parity/parity.json")
	if err != nil {
		t.Fatalf("read the parity matrix: %v", err)
	}
	var doc struct {
		Rows []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode parity.json: %v", err)
	}
	out := make(map[string]string, len(doc.Rows))
	for _, r := range doc.Rows {
		out[r.ID] = r.Status
	}
	if len(out) == 0 {
		t.Fatal("parity.json has no rows")
	}
	return out
}

func readStamp(t *testing.T, system string) parityStamp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityDir, system, "SOURCE.json"))
	if err != nil {
		t.Fatalf("read %s/SOURCE.json: %v", system, err)
	}
	var stamp parityStamp
	if err := json.Unmarshal(raw, &stamp); err != nil {
		t.Fatalf("decode SOURCE.json: %v", err)
	}
	if len(stamp.Entries) == 0 {
		t.Fatal("SOURCE.json has no entries")
	}
	return stamp
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TestParityGoldensMatchTheirStamps pins the corpus: each source is the file its
// stamp describes (byte copy, sha256) and each converted golden is the artefact
// the importer produced from it (golden_sha256, importer_commit, and the
// translation the stamp records). The conversion is NOT assumed to be a path
// move — the importer rewrites `runs-on: docker` to the instance label set,
// `needs: <str>` to a list, `$GITHUB_SERVER_URL` to `$SPINDLE_SERVER` and marks a
// key it cannot translate — so the golden drifts only with the stamp that
// explains it, and TestParityImporterReproducesGoldens proves the importer still
// produces it.
func TestParityGoldensMatchTheirStamps(t *testing.T) {
	if *update {
		updateParityGoldens(t)
	}
	stamp := readStamp(t, "forgejo")
	bySlug := map[string]paritySource{}
	for _, e := range stamp.Entries {
		bySlug[e.Slug] = e
	}
	entries := corpus()
	if len(entries) != len(stamp.Entries) {
		t.Fatalf("corpus has %d entries, SOURCE.json stamps %d", len(entries), len(stamp.Entries))
	}
	for _, e := range entries {
		src, ok := bySlug[e.Slug]
		if !ok {
			t.Fatalf("%s: no SOURCE.json entry", e.Slug)
		}
		if src.System != "forgejo" {
			t.Fatalf("%s: system %q", e.Slug, src.System)
		}
		if src.Repository == "" || src.Path == "" || src.Commit == "" || src.Fetched == "" {
			t.Fatalf("%s: incomplete stamp %+v", e.Slug, src)
		}
		if !strings.HasPrefix(src.Path, ".forgejo/workflows/") {
			t.Fatalf("%s: stamp path %q is not a Forgejo workflow", e.Slug, src.Path)
		}
		if src.GoldenSHA256 == "" || src.ImporterCommit == "" || src.Translation == "" {
			t.Fatalf("%s: the stamp must pin the golden, its importer revision and what the conversion changes", e.Slug)
		}
		source, err := os.ReadFile(filepath.Join(parityDir, "forgejo", e.Slug+".source.yml"))
		if err != nil {
			t.Fatalf("%s: read source: %v", e.Slug, err)
		}
		if got := sha256Hex(source); got != src.SHA256 {
			t.Fatalf("%s: source sha256 %s, stamp says %s (the byte copy drifted from its stamp)", e.Slug, got, src.SHA256)
		}
		golden, err := os.ReadFile(filepath.Join(parityDir, "forgejo", e.Slug+".yml"))
		if err != nil {
			t.Fatalf("%s: read converted golden: %v", e.Slug, err)
		}
		if got := sha256Hex(golden); got != src.GoldenSHA256 {
			t.Fatalf("%s: golden sha256 %s, stamp says %s (update the stamp and say why in `translation`)", e.Slug, got, src.GoldenSHA256)
		}
		// A corpus entry for a system the harness cannot convert or parse is a
		// silent gap: every stamped entry must be in corpus().
		delete(bySlug, e.Slug)
	}
	for slug := range bySlug {
		t.Fatalf("%s: stamped but not driven by the harness", slug)
	}
}

// updateParityGoldens is the -update path: it converts every corpus source with
// the real importer, writes the converted golden, and restamps SOURCE.json with
// the source hash, the golden hash and the importer revision. The test's ordinary
// assertions then run against the bytes it just wrote, so an update that the
// harness cannot reproduce fails in the same invocation.
func updateParityGoldens(t *testing.T) {
	t.Helper()
	bin := buildSpindle(t)
	stamp := readStamp(t, "forgejo")
	declared := make(map[string]parityEntry, len(corpus()))
	for _, entry := range corpus() {
		declared[entry.Slug] = entry
	}
	for i := range stamp.Entries {
		src := &stamp.Entries[i]
		entry, ok := declared[src.Slug]
		if !ok {
			t.Fatalf("%s: stamped but not driven by the harness", src.Slug)
		}
		source, err := os.ReadFile(filepath.Join(parityDir, "forgejo", src.Slug+".source.yml"))
		if err != nil {
			t.Fatalf("%s: read source: %v", src.Slug, err)
		}
		converted, out, cerr := convertWithImporter(t, bin, src.Slug, source)
		if cerr != nil && len(entry.ExpectUntranslated) == 0 {
			t.Fatalf("%s: threads import: %v\n%s", src.Slug, cerr, out)
		}
		goldenPath := filepath.Join(parityDir, "forgejo", src.Slug+".yml")
		if err := os.WriteFile(goldenPath, converted, 0o644); err != nil {
			t.Fatalf("%s: write golden: %v", src.Slug, err)
		}
		src.SHA256 = sha256Hex(source)
		src.GoldenSHA256 = sha256Hex(converted)
		t.Logf("%s: sha256=%s golden_sha256=%s", src.Slug, src.SHA256, src.GoldenSHA256)
	}
	if rev := importerRevision(t); rev != "" {
		for i := range stamp.Entries {
			stamp.Entries[i].ImporterCommit = rev
		}
		t.Logf("importer_commit=%s", rev)
	}
	if err := writeStamp(filepath.Join(parityDir, "forgejo", "SOURCE.json"), stamp); err != nil {
		t.Fatalf("write SOURCE.json: %v", err)
	}
}

// writeStamp encodes SOURCE.json the way it is committed: two-space indent, no
// HTML escaping (the translations name `<job>` and `$GITHUB_SERVER_URL`), and a
// trailing newline.
func writeStamp(path string, stamp parityStamp) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(stamp); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// importerRevision names the revision whose importer produced the goldens: the
// last commit that changed the importer's production code. Test and testdata
// changes cannot move a converted byte, so they do not move this either. Returns
// "" when git cannot answer (an exported tarball), and then the stamp keeps the
// value it had.
func importerRevision(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Logf("importer revision: %v (stamp unchanged)", err)
		return ""
	}
	cmd := exec.Command("git", "log", "-1", "--format=%H", "HEAD", "--",
		"internal/threads/importer",
		":(exclude)internal/threads/importer/testdata",
		":(exclude)internal/threads/importer/*_test.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Logf("importer revision: git log: %v (stamp unchanged)", err)
		return ""
	}
	rev := strings.TrimSpace(string(out))
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return rev
}

// convertWithImporter runs the documented invocation
// (`spindle threads import --from forgejo --repo DIR --path FILE --out DIR`) over
// one source body and returns the converted file and the command's output. A
// construct the importer cannot translate exits 1 after writing the file; the
// caller decides whether that refusal is declared. The source must come back
// untouched.
func convertWithImporter(t *testing.T, bin, slug string, source []byte) (converted, output []byte, err error) {
	t.Helper()
	tree := t.TempDir()
	rel := filepath.Join(".forgejo", "workflows", slug+".yml")
	in := filepath.Join(tree, rel)
	if werr := os.MkdirAll(filepath.Dir(in), 0o755); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(in, source, 0o644); werr != nil {
		t.Fatal(werr)
	}
	outDir := filepath.Join(tree, "out")
	cmd := exec.Command(bin, "threads", "import", "--from", "forgejo", "--repo", tree, "--path", rel, "--out", outDir)
	cmd.Dir = tree
	cmd.Env = append(os.Environ(), "HOME="+tree)
	output, err = cmd.CombinedOutput()
	converted, rerr := os.ReadFile(filepath.Join(outDir, ".spindle", "workflows", slug+".yml"))
	if rerr != nil {
		t.Fatalf("%s: the importer wrote no converted file: %v\n%s", slug, rerr, output)
	}
	if after, _ := os.ReadFile(in); string(after) != string(source) {
		t.Fatalf("%s: the importer modified its source file", slug)
	}
	return converted, output, err
}

// TestParityFixturesCarryNoPrivateHost keeps the corpus publishable. The publish
// transform rewrites the private estate's host to the public forge in every text
// file under the tree, fixtures included, so a fixture carrying it is rewritten
// on the way out and its byte stamps stop matching — the published v0.10.0 tree
// failed TestParityGoldensMatchTheirStamps exactly that way while the same file
// passed locally. The copies carry a neutral host (git.example.com) instead;
// this guard is the tripwire for a future copy-paste.
func TestParityFixturesCarryNoPrivateHost(t *testing.T) {
	// The token is assembled at run time: spelled out, this guard would itself
	// trip the publish gate, which refuses a tree where any file still names the
	// private estate (including the file that guards against it).
	needle := "pack" + "den"
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{
		filepath.Join(root, "internal", "e2e", "testdata", "parity"),
		filepath.Join(root, "internal", "threads", "importer", "testdata"),
	} {
		err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(strings.ToLower(string(body)), needle) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s names the private host: fixtures must carry the neutral host (git.example.com), or the publish transform rewrites their bytes and the stamps stop matching", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
}

// parityGit serves one converted workflow file at one commit (the GitReader the
// engine's discovery seam consumes).
type parityGit struct {
	commit string
	path   string
	body   []byte
}

func (g *parityGit) ListWorkflowFiles(_ context.Context, _ string, commit string) ([]string, error) {
	if commit != g.commit {
		return nil, fmt.Errorf("%w: commit %s", threads.ErrNotFound, commit)
	}
	return []string{g.path}, nil
}

func (g *parityGit) ReadFile(_ context.Context, _, commit, path string) ([]byte, error) {
	if commit != g.commit || path != g.path {
		return nil, fmt.Errorf("%w: %s at %s", threads.ErrNotFound, path, commit)
	}
	return g.body, nil
}

func (g *parityGit) ResolveRef(_ context.Context, _, ref string) (string, error) {
	return g.commit, nil
}

// TestParityCorpusParsesAndPlans is the parse/plan tier: every corpus file
// either parses and plans (with the documented jobs and steps), or fails with
// the refusal its open rows declare — never silently.
func TestParityCorpusParsesAndPlans(t *testing.T) {
	statuses := parityRowStatus(t)
	commit := strings.Repeat("a", 40)
	for _, entry := range corpus() {
		entry := entry
		t.Run(entry.Slug, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(parityDir, "forgejo", entry.Slug+".yml"))
			if err != nil {
				t.Fatalf("read converted workflow: %v", err)
			}
			blocked := ""
			for _, id := range entry.DependsOn {
				if statuses[id] != "have" {
					blocked = id
					break
				}
			}
			// A converted file may carry an UNTRANSLATED marker for a key the
			// importer cannot translate. While the blocking row is open the marker
			// must be there (named, never silent); when the row turns green the
			// importer must have translated it, so a marker that is still there
			// fails right here.
			for _, fragment := range entry.ExpectUntranslated {
				present := strings.Contains(string(raw), fragment)
				switch {
				case blocked != "" && !present:
					t.Fatalf("%s is blocked by %s but the converted file lost the documented marker %q", entry.Slug, blocked, fragment)
				case blocked == "" && present:
					t.Fatalf("%s: %q is untranslated but the blocking row is green; the importer should translate it now — update this entry", entry.Slug, fragment)
				case blocked != "":
					t.Logf("%s: documented untranslated key (blocked by %s): %s", entry.Slug, blocked, fragment)
				}
			}
			wf, perr := workflow.Parse(raw)
			if perr != nil {
				// A file the importer could not make parse is the documented
				// refusal path: only allowed while a dependency row is open, and it
				// must be the refusal the entry declares.
				if blocked == "" {
					t.Fatalf("%s: parse: %v", entry.Slug, perr)
				}
				if entry.Refusal == "" || !strings.Contains(perr.Error(), entry.Refusal) {
					t.Fatalf("refusal changed: parser says %q, the entry documents %q", perr, entry.Refusal)
				}
				t.Logf("%s: documented parse refusal (blocked by %s): %v", entry.Slug, blocked, perr)
				return
			}
			// The YAML jobs the file declares, in document order.
			var keys []string
			for _, j := range entry.Jobs {
				keys = append(keys, j.Key)
			}
			got := make([]string, 0, len(wf.Jobs))
			for k := range wf.Jobs {
				got = append(got, k)
			}
			sort.Strings(got)
			sorted := append([]string(nil), keys...)
			sort.Strings(sorted)
			if strings.Join(got, ",") != strings.Join(sorted, ",") {
				t.Fatalf("%s: jobs = %v, want %v", entry.Slug, got, sorted)
			}
			for _, want := range entry.Jobs {
				def, ok := wf.Jobs[want.Key]
				if !ok {
					t.Fatalf("%s: no job %q", entry.Slug, want.Key)
				}
				if len(def.Steps) != want.Steps {
					t.Fatalf("%s: job %s has %d steps, want %d", entry.Slug, want.Key, len(def.Steps), want.Steps)
				}
			}
			// The engine's real run-creation path: discovery → trigger match →
			// job specs → queue. A memory store keeps it a pure record test.
			svc := threads.New(store.NewMemory(), nil)
			path := ".spindle/workflows/" + entry.Slug + ".yml"
			svc.Git = &parityGit{commit: commit, path: path, body: raw}
			opts := threads.CreateOpts{Branch: entry.Branch, Ref: entry.Ref, RefName: entry.RefName, Actor: "parity"}
			if entry.Tag {
				opts.RefKind = workflow.RefTag
			}
			if entry.Event == threads.EventDispatch {
				opts.Workflow = entry.Slug + ".yml"
			}
			runs, cerr := svc.CreateRun(context.Background(), "o/r", commit, entry.Event, opts)
			if cerr != nil {
				t.Fatalf("%s: CreateRun: %v", entry.Slug, cerr)
			}
			if len(runs) != 1 {
				t.Fatalf("%s: CreateRun made %d runs, want 1", entry.Slug, len(runs))
			}
			run := runs[0]
			if run.WorkflowPath != path {
				t.Fatalf("%s: run workflow path = %q", entry.Slug, run.WorkflowPath)
			}
			if run.Ref != entry.Ref {
				t.Fatalf("%s: run ref = %q, want %q", entry.Slug, run.Ref, entry.Ref)
			}
			if run.Branch != entry.Branch {
				t.Fatalf("%s: run branch = %q, want %q (a tag push carries no branch)", entry.Slug, run.Branch, entry.Branch)
			}
			view, gerr := svc.GetRun(context.Background(), "o/r", run.ID, auth.Principal{Name: "parity"})
			if gerr != nil {
				t.Fatalf("%s: GetRun: %v", entry.Slug, gerr)
			}
			// Every YAML job has at least one job record (a matrix job expands),
			// and the records keep the workflow's step counts.
			stepWant := map[string]int{}
			for _, want := range entry.Jobs {
				stepWant[want.Key] = want.Steps
			}
			seen := map[string]bool{}
			for _, job := range view.Jobs {
				if job.Spec == nil {
					t.Fatalf("%s: job %s has no spec", entry.Slug, job.ID)
				}
				// A matrix cell's key is the group key plus " (v1, v2)".
				key, ok := jobGroupKey(job.Key, stepWant)
				if !ok {
					t.Fatalf("%s: unexpected job %s (key %q)", entry.Slug, job.ID, job.Key)
				}
				seen[key] = true
				if n := stepWant[key]; len(job.Spec.Steps) != n {
					t.Fatalf("%s: job %s spec has %d steps, want %d", entry.Slug, job.Key, len(job.Spec.Steps), n)
				}
			}
			for k := range stepWant {
				if !seen[k] {
					t.Fatalf("%s: no job record for %q", entry.Slug, k)
				}
			}

		})
	}
}

// parityCreate drives one corpus entry's converted file through the engine's
// real run-creation path with an explicit trigger context, and returns the runs
// it produced.
func parityCreate(t *testing.T, path string, body []byte, commit, event, branch, ref, refName string, tag bool, action string) []*threads.Run {
	t.Helper()
	svc := threads.New(store.NewMemory(), nil)
	svc.Git = &parityGit{commit: commit, path: path, body: body}
	opts := threads.CreateOpts{Branch: branch, Ref: ref, RefName: refName, Actor: "parity", Action: action}
	if tag {
		opts.RefKind = workflow.RefTag
	}
	runs, err := svc.CreateRun(context.Background(), "o/r", commit, event, opts)
	if err != nil {
		t.Fatalf("CreateRun(%s,%s,%s): %v", event, branch, ref, err)
	}
	return runs
}

// TestParityCorpusTriggerSemantics drives the corpus's real trigger shapes
// through the engine: a tags-only workflow (scriobh-release, `on.push.tags:
// ["v*"]`) fires on a matching tag and on NOTHING else — not a branch push, not
// a non-matching tag — while a workflow that lists both (weave-ci) fires on
// each. It is PAR-010/PAR-011 proved on corpus content, not on a fixture.
func TestParityCorpusTriggerSemantics(t *testing.T) {
	commit := strings.Repeat("b", 40)
	read := func(slug string) (string, []byte) {
		path := ".spindle/workflows/" + slug + ".yml"
		body, err := os.ReadFile(filepath.Join(parityDir, "forgejo", slug+".yml"))
		if err != nil {
			t.Fatal(err)
		}
		return path, body
	}

	tagOnlyPath, tagOnly := read("scriobh-release")
	cases := []struct {
		name         string
		branch       string
		ref, refName string
		tag          bool
		wantRuns     int
	}{
		{name: "matching tag fires", ref: "refs/tags/v1.2.3", refName: "v1.2.3", tag: true, wantRuns: 1},
		{name: "non-matching tag does not", ref: "refs/tags/nightly", refName: "nightly", tag: true, wantRuns: 0},
		{name: "branch push does not", branch: "main", ref: "refs/heads/main", wantRuns: 0},
	}
	for _, c := range cases {
		c := c
		t.Run("tags-only/"+strings.ReplaceAll(c.name, " ", "-"), func(t *testing.T) {
			runs := parityCreate(t, tagOnlyPath, tagOnly, commit, threads.EventPush, c.branch, c.ref, c.refName, c.tag, "")
			if len(runs) != c.wantRuns {
				t.Fatalf("%s: %d runs, want %d", c.name, len(runs), c.wantRuns)
			}
			if c.wantRuns == 1 {
				if runs[0].Ref != c.ref || runs[0].Branch != "" {
					t.Fatalf("tag run ref=%q branch=%q, want %q and no branch", runs[0].Ref, runs[0].Branch, c.ref)
				}
			}
		})
	}

	// weave-ci lists branches AND tags: both fire.
	bothPath, both := read("weave-ci")
	if runs := parityCreate(t, bothPath, both, commit, threads.EventPush, "main", "refs/heads/main", "main", false, ""); len(runs) != 1 {
		t.Fatalf("weave-ci on a branch push: %d runs, want 1", len(runs))
	}
	if runs := parityCreate(t, bothPath, both, commit, threads.EventPush, "", "refs/tags/v1.2.3", "v1.2.3", true, ""); len(runs) != 1 {
		t.Fatalf("weave-ci on a tag push: %d runs, want 1", len(runs))
	}
}

// jobGroupKey maps one job record's key onto the YAML job it came from: the key
// itself, or — for a matrix cell — the group key before its " (…)" suffix.
func jobGroupKey(recKey string, wants map[string]int) (string, bool) {
	if _, ok := wants[recKey]; ok {
		return recKey, true
	}
	if i := strings.Index(recKey, " ("); i > 0 {
		if key := recKey[:i]; wants[key] > 0 {
			return key, true
		}
	}
	return "", false
}

// TestParityImporterReproducesGoldens converts each corpus source with the real
// importer when it exists (PAR-002) and requires the committed golden to be its
// output. It skips with a notice until the importer package lands, so the parse
// tier stays independent of that work; `make parity` (not -short) runs it.
//
// The invocation is the documented one: read-only by default, `--out DIR`
// writes the converted files under the repo-relative layout, and a construct it
// cannot translate is marked in the file AND exits 1 — which is exactly the
// entry that carries ExpectUntranslated. The source file must be untouched.
func TestParityImporterReproducesGoldens(t *testing.T) {
	if testing.Short() {
		t.Skip("importer golden check: skipped in -short (builds the CLI)")
	}
	if _, err := os.Stat("../../internal/threads/importer"); err != nil {
		t.Skip("importer not landed yet (PAR-002): the committed goldens are pinned by TestParityGoldensMatchTheirStamps")
	}
	bin := buildSpindle(t)
	for _, entry := range corpus() {
		entry := entry
		t.Run(entry.Slug, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(parityDir, "forgejo", entry.Slug+".source.yml"))
			if err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile(filepath.Join(parityDir, "forgejo", entry.Slug+".yml"))
			if err != nil {
				t.Fatal(err)
			}
			converted, out, cerr := convertWithImporter(t, bin, entry.Slug, source)
			if cerr != nil && len(entry.ExpectUntranslated) == 0 {
				t.Fatalf("threads import: %v\n%s", cerr, out)
			}
			if string(converted) != string(golden) {
				t.Fatalf("the importer's output differs from the committed golden; update the golden and its stamp in the same change:\n--- golden\n%s\n--- importer\n%s", golden, converted)
			}
		})
	}
}

// TestE2E_ParityExecution is the execution tier (opt-in). It runs a
// corpus-shaped workflow through a real server and a real runner and requires a
// green run: the engine path the corpus rows depend on, executed rather than
// planned.
func TestE2E_ParityExecution(t *testing.T) {
	if os.Getenv("SPINDLE_PARITY_EXEC") != "1" {
		t.Skip("execution tier: set SPINDLE_PARITY_EXEC=1 (needs docker; make parity-exec)")
	}
	requireDocker(t)
	requireModernGit(t)
	g := newGitClient(t)
	s := threadsServer(t)
	bin := buildSpindle(t)
	owner, repo := testOwner, uniqueRepo("parity")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"

	body, err := os.ReadFile(filepath.Join(parityDir, "spindle", "parity-smoke.yml"))
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	g.commitFile(work, ".spindle/workflows/parity-smoke.yml", string(body), "parity smoke")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")

	reg := mintRunnerToken(t, repoLane, chainAliceTok)
	startThreadsRunner(t, bin, s.base, reg, "linux", 1)

	var runID string
	pollUntil(t, 60*time.Second, "a parity smoke run", func() bool {
		runID = latestRunID(t, repoLane, chainAliceTok)
		return runID != ""
	})
	pollUntil(t, 180*time.Second, "the parity smoke run to finish", func() bool {
		state, _ := getRun(t, repoLane, runID, chainAliceTok).Run["state"].(string)
		return state == "success" || state == "failure" || state == "cancelled"
	})
	run := getRun(t, repoLane, runID, chainAliceTok)
	if run.Run["state"] != "success" {
		jobs, _ := json.Marshal(run.Jobs)
		t.Fatalf("parity smoke run = %v, jobs %s", run.Run["state"], jobs)
	}
	job := jobState(run, "j-smoke")
	if job == nil || job["state"] != "success" {
		t.Fatalf("smoke job = %v", job)
	}
}
