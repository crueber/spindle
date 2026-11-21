package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mkGit runs one git command in dir with a pinned identity (runners and
// containers have no global config — AGENTS field lesson).
func mkGit(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	c := exec.Command("git", argv...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", argv, err, out)
	}
	return strings.TrimSpace(string(out))
}

// packRev builds a pack for the given rev-list stdin
// (`^<base>` + `<tip>` for a thin pack) with optional --thin.
func packRev(t *testing.T, dir, stdin string, thin bool) []byte {
	t.Helper()
	argv := []string{"pack-objects", "--stdout", "--revs"}
	if thin {
		argv = append(argv, "--thin")
	}
	c := exec.Command("git", argv...)
	c.Dir = dir
	c.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	c.Stdout = &buf
	if err := c.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	return buf.Bytes()
}

// TestIngestThinPackFixThin pins the receive-pack ingest contract: git
// send-pack builds a THIN pack by default (deltas against objects the client
// saw advertised) — the client cannot negotiate the fetch-only `thin-pack`
// cap, so receive-pack MUST ingest with --fix-thin unconditionally
// (04_git.md §3.2). A pack ingested without it fails "unresolved delta",
// which is what every incremental push hit before the fix.
func TestIngestThinPackFixThin(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := t.TempDir()
	mkGit(t, up, "init", "-q", "-b", "main", ".")
	big := strings.Repeat("shared line of payload\n", 8000) // ~200 KiB: delta-worthy
	if err := os.WriteFile(filepath.Join(up, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	mkGit(t, up, "add", ".")
	mkGit(t, up, "commit", "-q", "-m", "c1")
	c1 := mkGit(t, up, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(up, "big.txt"),
		[]byte(strings.ReplaceAll(big, "shared line", "changed line")), 0o644); err != nil {
		t.Fatal(err)
	}
	mkGit(t, up, "add", ".")
	mkGit(t, up, "commit", "-q", "-m", "c2")
	c2 := mkGit(t, up, "rev-parse", "HEAD")

	// The thin pack omits c1's objects: its delta base lives in the pack the
	// server already has (here: ingested first as a complete pack).
	thin := packRev(t, up, "^"+c1+"\n"+c2+"\n", true)
	if len(thin) < 100 {
		t.Fatalf("thin fixture pack too small: %d", len(thin))
	}
	base := packRev(t, up, c1+"\n", false)

	repo, err := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "thin"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	layer := NewLayer()
	if _, err := layer.Ingest(t.Context(), repo, bytes.NewReader(base), 0, false, true); err != nil {
		t.Fatalf("base ingest: %v", err)
	}

	// Without --fix-thin the unresolved base rejects the pack: the audit's
	// "pack has N unresolved deltas", and the reason receive-pack must never
	// ride on.
	if _, err := layer.Ingest(t.Context(), repo, bytes.NewReader(thin), 0, false, true); err == nil {
		t.Fatal("thin pack accepted without --fix-thin")
	} else if pe, ok := err.(*PackRejectedError); !ok {
		t.Fatalf("err type = %T (%v)", err, err)
	} else if !strings.Contains(pe.Detail, "unresolved delta") {
		t.Fatalf("detail = %q, want an unresolved-delta verdict", pe.Detail)
	}

	// With --fix-thin (what receive-pack passes) the same bytes ingest and the
	// new tip is readable.
	if _, err := layer.Ingest(t.Context(), repo, bytes.NewReader(thin), 0, true, true); err != nil {
		t.Fatalf("thin ingest with fix-thin: %v", err)
	}
	mkGit(t, repo.Path, "cat-file", "-e", c2)
}

// TestWantsReport: the report trailer is owed for either negotiated cap.
// git requests only `report-status-v2` when it is advertised, so treating
// v2 as "no report" is the rejected-pack hang.
func TestWantsReport(t *testing.T) {
	cases := []struct {
		caps string
		want bool
	}{
		{"", false},
		{"side-band-64k quiet", false},
		{"report-status", true},
		{"report-status-v2", true},
		{"report-status report-status-v2 side-band-64k", true},
		{"report-status-v2 side-band-64k quiet object-format=sha1 agent=git/2.53.0", true},
	}
	for _, tc := range cases {
		req := &PushRequest{Caps: strings.Fields(tc.caps)}
		if got := req.WantsReport(); got != tc.want {
			t.Errorf("WantsReport(%q) = %v, want %v", tc.caps, got, tc.want)
		}
	}
	var nilReq *PushRequest
	if nilReq.WantsReport() {
		t.Error("nil request must not want a report")
	}
}

// TestIngestReapsIndexPack: the rejected-pack error path must wait for and
// reap git index-pack — a leaked child is a pinning subprocess (04_git.md
// §3.2 runs via Pool.Run with a wait).
func TestIngestReapsIndexPack(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	junk := bytes.Repeat([]byte("JUNKJUNKJUNK"), 500)
	if _, err := l.Ingest(t.Context(), repo, bytes.NewReader(junk), 0, true, true); err == nil {
		t.Fatal("junk accepted")
	}
	assertNoChildGit(t, "index-pack")
	// Stream mode (SSH) must reap on the same path.
	if _, err := l.IngestStream(t.Context(), repo, bytes.NewReader(junk), 0, true, true); err == nil {
		t.Fatal("junk accepted on the stream path")
	}
	assertNoChildGit(t, "index-pack")
}

// assertNoChildGit fails when a live or zombie child of this process carries
// name in its argv/comm — the observable "the subprocess was reaped" claim.
// /proc-less platforms assert nothing (the call is a no-op there).
func assertNoChildGit(t *testing.T, name string) {
	t.Helper()
	self := strconv.Itoa(os.Getpid())
	var last []string
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return
		}
		last = last[:0]
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			s := string(stat)
			open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
			if open < 0 || closeIdx <= open {
				continue
			}
			comm := s[open+1 : closeIdx]
			fields := strings.Fields(s[closeIdx+1:])
			if len(fields) < 2 || fields[1] != self {
				continue
			}
			argv, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if strings.Contains(comm, name) || strings.Contains(string(argv), name) {
				last = append(last, e.Name()+" ("+comm+"): "+
					strings.ReplaceAll(string(argv), "\x00", " "))
			}
		}
		if len(last) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("git %s survived the error path (not reaped): %v", name, last)
		}
	}
}
