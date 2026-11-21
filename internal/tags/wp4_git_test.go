package tags

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestListTagRefsAnnotatedAndLightweight(t *testing.T) {
	dir := gitTestRepo(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("tag", "-a", "v2", "-m", "release two", "HEAD")
	commit := run("rev-parse", "HEAD^{commit}")
	g := NewSubprocessGit("git")
	refs, err := g.ListTagRefs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs %d: %+v", len(refs), refs)
	}
	byName := map[string]TagRef{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	if r := byName["v2"]; !r.Annotated || r.Commit != commit || r.Object == r.Commit || r.Author == "" || r.CreatedAt == "" {
		t.Fatalf("annotated %+v", r)
	}
	if r := byName["v1"]; r.Annotated || r.Commit == "" || r.Object != r.Commit {
		t.Fatalf("lightweight %+v", r)
	}
}

func TestListTagRefsBackendFailure(t *testing.T) {
	g := NewSubprocessGit("/no/such/git")
	if _, err := g.ListTagRefs(context.Background(), t.TempDir()); !isErr(err, errUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestListTagEntriesReleaseErrorAndDirsFailure(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("o", "r", "jane", "read")
	x.git.tagRefs = []TagRef{{Name: "v1", Commit: testSHA}}
	x.svc.SetReleaseExists(func(context.Context, string, string, string) (bool, error) {
		return false, errStore{err: errOf("store down")}.err
	})
	entries, err := x.svc.ListTagEntries(ctx(), "o", "r", writer())
	if err != nil || len(entries) != 1 || entries[0].Release {
		t.Fatalf("release probe must fail open: %+v %v", entries, err)
	}
	x.svc.Dirs = errDirs{}
	if _, err := x.svc.ListTagEntries(ctx(), "o", "r", writer()); !isErr(err, errUnavailable) {
		t.Fatalf("dirs: %v", err)
	}
}

func TestDeleteTagReleaseProbeError(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	x.svc.SetReleaseExists(func(context.Context, string, string, string) (bool, error) {
		return false, errOf("store down")
	})
	if err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1"); !isErr(err, errUnavailable) {
		t.Fatalf("err %v", err)
	}
}

// errOf is a tiny error constructor for the probe-failure table.
func errOf(s string) error { return &probeErr{s} }

type probeErr struct{ s string }

func (e *probeErr) Error() string { return e.s }
