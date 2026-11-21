package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_ThinPushAndRejectedPackTerminates covers the two halves of the
// rejected-pack defect (docs/AUDIT-V1.md "Open item", CLI round-2 "new
// defect") through the real client/server pair:
//
//  1. An incremental push carries a THIN pack (git deltas against the tip it
//     saw advertised, so the base object lives only in the server's existing
//     pack). It must ingest — receive-pack always runs index-pack --fix-thin.
//     Before the fix the pack was rejected, because the flag was keyed on a
//     capability the client cannot negotiate.
//  2. A pack the server genuinely cannot index (an unreadable base pack in
//     the serving copy) must terminate the exchange: `git push` exits
//     non-zero with the reason, promptly. Before the fix the server wrote
//     only the band-2 message and no report-status trailer — git requests
//     only `report-status-v2`, so the report check never fired and the client
//     blocked reading the status stream forever (3+ minutes in the audit).
//
// Both steps are deadline-bound so a regression fails the suite instead of
// hanging it.
func TestE2E_ThinPushAndRejectedPackTerminates(t *testing.T) {
	requireModernGit(t)
	g := newGitClient(t)
	s := newServer(t)
	repo := uniqueRepo("reject")
	url := s.gitURL(testOwner, repo)

	dir := filepath.Join(t.TempDir(), "seed")
	g.initRepo(dir)
	big := strings.Repeat("shared payload line\n", 8000)
	g.commitFile(dir, "big.txt", big, "seed")
	g.run(dir, "remote", "add", "origin", url)
	g.run(dir, "push", "-u", "origin", "main")

	// (1) Thin incremental push: delta base present on the server.
	g.commitFile(dir, "big.txt", strings.Replace(big, "shared payload line\n", "shared payload LINE\n", 1), "incremental")
	if code, out := g.cmdDeadline(30*time.Second, dir, "push", "origin", "main"); code != 0 {
		t.Fatalf("thin incremental push rejected (exit %d) — receive-pack must ingest with --fix-thin:\n%s", code, out)
	}

	// (2) Object loss in the serving copy: corrupt every pack the server holds
	// locally. The refs stay advertised (they live in the WAL), so the client
	// still builds a thin pack whose base the server cannot read.
	packs := filepath.Join(s.dataDir, "cache", testOwner, repo+".git", "objects", "pack")
	corruptPacks(t, packs)

	g.commitFile(dir, "big.txt", strings.Replace(big, "shared payload line\n", "shared payload line CHANGED\n", 1), "after-loss")
	started := time.Now()
	code, out := g.cmdDeadline(30*time.Second, dir, "push", "origin", "main")
	elapsed := time.Since(started)
	if code == 0 {
		t.Fatalf("push over an unindexable pack succeeded; want a refusal:\n%s", out)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("refusal took %v — the client must not wait for a report that never comes", elapsed)
	}
	t.Logf("refusal in %v:\n%s", elapsed, out)
	refused := strings.Contains(out, "remote: pack rejected") ||
		strings.Contains(out, "remote: connectivity check failed")
	if !refused {
		t.Fatalf("band-2 reason missing from the client output:\n%s", out)
	}
	if !strings.Contains(out, "remote rejected") && !strings.Contains(out, "unpacker error") {
		t.Fatalf("client did not receive a per-ref refusal (report-status trailer):\n%s", out)
	}
}

// corruptPacks flips every 32nd byte of every pack file under dir, keeping
// the names intact so the registry still considers them materialized. The
// packs stay listed but no object in them is readable — a delta base or a
// boundary commit read from one fails (ingest or connectivity then refuses,
// both on the same report path).
func corruptPacks(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read pack dir %s: %v", dir, err)
	}
	packs := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pack") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for i := 12; i < len(data); i += 32 { // past the 12-byte pack header
			data[i] ^= 0xff
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		packs++
	}
	if packs == 0 {
		t.Fatalf("no pack files under %s", dir)
	}
}
