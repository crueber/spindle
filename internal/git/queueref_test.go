package git

import (
	"os/exec"
	"strings"
	"testing"
)

// TestQueueRefsAreNotBranches pins the merge queue's ref-visibility contract
// (docs/threads.md §6.10): the temporary refs live under refs/heads/ (the
// documented shape) but a clone must not receive them as branches, while an
// explicit ref-prefix fetch still can.
func TestQueueRefsAreNotBranches(t *testing.T) {
	repo, l, head := realRepoFixture(t)
	queueRef := "refs/heads/gh-readonly-queue/main/pr-7-" + strings.ToLower(string(head))
	cmd := exec.Command("git", "--git-dir", repo.Path, "update-ref", queueRef, string(head))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	if !IsMergeQueueRef(queueRef) || IsMergeQueueRef("refs/heads/main") {
		t.Fatal("IsMergeQueueRef")
	}

	// v0: the clone advertisement excludes the queue ref, keeps the branch and
	// HEAD.
	out, err := l.Advertisement(repo, ServiceUploadPack, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if strings.Contains(body, "gh-readonly-queue") {
		t.Fatalf("v0 advertisement leaked a queue ref:\n%s", body)
	}
	if !strings.Contains(body, "refs/heads/main") || !strings.Contains(body, " HEAD") {
		t.Fatalf("v0 advertisement lost its branches:\n%s", body)
	}

	// v2 ls-refs with no prefixes (the clone path) excludes it.
	out, err = l.LsRefs(repo, LsRefsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "gh-readonly-queue") {
		t.Fatalf("ls-refs leaked a queue ref:\n%s", out)
	}
	if !strings.Contains(string(out), "refs/heads/main") {
		t.Fatalf("ls-refs lost the branch:\n%s", out)
	}
	// An explicit prefix (a deliberate fetch) still sees it.
	out, err = l.LsRefs(repo, LsRefsArgs{Prefixes: []string{"refs/heads/gh-readonly-queue/"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), queueRef) {
		t.Fatalf("explicit queue fetch cannot see the ref:\n%s", out)
	}
}
