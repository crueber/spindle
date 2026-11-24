package webui

// compare_test.go — the Compare page: the commit list, the summary, the
// shared unified-diff renderer, the honest identical/empty/disjoint states,
// the PR compare URL shape (query → path, cross-repo → composer), and the
// read-gate matrix.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

func (v *repoFakeView) Compare(_ context.Context, _ git.RepoId, base, head string, _ int) (api.CompareResult, error) {
	v.compareN++
	if v.compareErr != nil {
		return api.CompareResult{}, v.compareErr
	}
	res := v.compare
	res.BaseSHA, res.HeadSHA = base, head
	return res, nil
}

const comparePatch = `diff --git a/hello.txt b/hello.txt
index 1111111..2222222 100644
--- a/hello.txt
+++ b/hello.txt
@@ -1,2 +1,2 @@
 hello world
-hello again
+hello there
`

func compareFixture(t *testing.T) *repoFixture {
	t.Helper()
	f := newRepoFixture(t, true)
	f.view.compare = api.CompareResult{
		MergeBase: strings.Repeat("a", 40),
		Ahead:     2,
		Behind:    1,
		Commits: []api.Commit{
			{SHA: strings.Repeat("b", 40), Subject: "second subject", Author: "Ada", AuthorDate: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)},
			{SHA: strings.Repeat("c", 40), Subject: "first subject", Author: "Bob", AuthorDate: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)},
		},
		Stats: []api.Stat{{Path: "hello.txt", Additions: 1, Deletions: 1}},
		Patch: comparePatch,
	}
	return f
}

func TestRepoCompareRendersCommitListAndDiff(t *testing.T) {
	t.Parallel()
	f := compareFixture(t)
	body := f.mustGet("/demo/spindle/compare/main...feature", http.StatusOK)
	for _, want := range []string{
		"second subject", "first subject", "2 ahead · 1 behind",
		"across 1 file", "hello.txt", "+1", "−1",
		"/demo/spindle/commit/" + strings.Repeat("b", 40),
		"diff-add", "diff-del",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The shared renderer emits the sign column (text, not colour-only).
	if !strings.Contains(body, `>&#43;</td>`) || !strings.Contains(body, `>-</td>`) {
		t.Error("diff sign column missing")
	}
	// Compare page maps to the Commits tab.
	if activeTabOf(t, body) != "commits" {
		t.Errorf("active tab = %q", activeTabOf(t, body))
	}
}

func TestRepoCompareHonestStates(t *testing.T) {
	t.Parallel()
	// Identical.
	f := compareFixture(t)
	f.view.compare = api.CompareResult{Identical: true}
	body := f.mustGet("/demo/spindle/compare/main...main", http.StatusOK)
	if !strings.Contains(body, "Identical:") || strings.Contains(body, "diff-add") {
		t.Fatal("identical state wrong")
	}

	// Ahead=0 (head is behind): nothing to merge.
	f2 := compareFixture(t)
	f2.view.compare = api.CompareResult{Behind: 3}
	body = f2.mustGet("/demo/spindle/compare/main...old", http.StatusOK)
	if !strings.Contains(body, "Nothing to merge") {
		t.Fatal("nothing-to-merge notice missing")
	}

	// Disjoint histories.
	f3 := compareFixture(t)
	f3.view.compare = api.CompareResult{NoCommonAncestor: true, Ahead: 1, Patch: comparePatch,
		Stats: []api.Stat{{Path: "hello.txt", Additions: 1, Deletions: 1}}}
	body = f3.mustGet("/demo/spindle/compare/main...orphan", http.StatusOK)
	if !strings.Contains(body, "share no common ancestry") {
		t.Fatal("disjoint notice missing")
	}

	// Patch truncation.
	f4 := compareFixture(t)
	f4.view.compare = api.CompareResult{Ahead: 1, PatchTruncated: true, Stats: []api.Stat{{Path: "big", Additions: 9}}}
	body = f4.mustGet("/demo/spindle/compare/main...big", http.StatusOK)
	if !strings.Contains(body, "larger than 2 MiB") {
		t.Fatal("patch truncation notice missing")
	}

	// Commit truncation.
	f5 := compareFixture(t)
	f5.view.compare = api.CompareResult{Ahead: 300, More: true, Commits: []api.Commit{{SHA: strings.Repeat("d", 40), Subject: "one"}}}
	body = f5.mustGet("/demo/spindle/compare/main...many", http.StatusOK)
	if !strings.Contains(body, "Showing the first") {
		t.Fatal("commit truncation notice missing")
	}
}

func TestRepoCompareCrossRepo(t *testing.T) {
	t.Parallel()
	f := compareFixture(t)
	body := f.mustGet("/demo/spindle/compare/main...bob%2Ffork:topic", http.StatusOK)
	if !strings.Contains(body, "bob/fork") || !strings.Contains(body, "Compare in a new pull request") {
		t.Fatal("cross-repo card missing")
	}
	want := "/demo/spindle/pulls/new?base=main&head=bob%2Ffork%3Atopic"
	if !strings.Contains(body, strings.ReplaceAll(want, "&", "&amp;")) {
		t.Fatalf("PR link missing (%s)", want)
	}
	if f.view.compareN != 0 {
		t.Fatal("cross-repo compare must not read this repo's objects")
	}
	if strings.Count(body, "btn primary") != 1 {
		t.Fatalf("cross-repo primaries = %d, want 1", strings.Count(body, "btn primary"))
	}
	// A same-repo prefixed head is stripped and compared normally.
	f2 := compareFixture(t)
	body = f2.mustGet("/demo/spindle/compare/main...demo%2Fspindle:feature", http.StatusOK)
	if f2.view.compareN != 1 || strings.Contains(body, "Comparing across repositories") {
		t.Fatal("same-repo prefixed head must compare")
	}
	// A colon form without owner/repo is refused.
	if code, _ := f2.get("/demo/spindle/compare/main...topic:x"); code != http.StatusNotFound {
		t.Fatalf("bad colon head = %d", code)
	}
}

func TestRepoCompareURLShapes(t *testing.T) {
	t.Parallel()
	f := compareFixture(t)
	// The PR link's query shape redirects to the canonical path.
	rec := parityDo(t, f.s, http.MethodGet, "/demo/spindle/compare?base=main&head=bob/fork:topic", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("query shape = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/demo/spindle/compare/main...bob/fork:topic" {
		t.Fatalf("Location = %q", loc)
	}
	// Missing halves are an honest 404, not a silent empty compare.
	for _, target := range []string{
		"/demo/spindle/compare",
		"/demo/spindle/compare?base=main",
		"/demo/spindle/compare/main",
		"/demo/spindle/compare/main...",
		"/demo/spindle/compare/...topic",
	} {
		if code, _ := f.get(target); code != http.StatusNotFound {
			t.Errorf("%s = %d", target, code)
		}
	}
}

func TestRepoCompareGatesAndErrors(t *testing.T) {
	t.Parallel()
	// Unknown base/head name the side.
	f := compareFixture(t)
	f.view.resolveErr = api.ErrNotFound
	if code, body := f.get("/demo/spindle/compare/main...topic"); code != http.StatusNotFound || !strings.Contains(body, "base ref main") {
		t.Fatalf("unknown base = %d", code)
	}
	// Base resolves, head does not: the error names the head side.
	h := compareFixture(t)
	h.view.resolveErr = api.ErrNotFound
	h.view.resolveFailOn = "topic"
	if code, body := h.get("/demo/spindle/compare/main...topic"); code != http.StatusNotFound || !strings.Contains(body, "head ref topic") {
		t.Fatalf("unknown head = %d", code)
	}

	// Resolve failures that are not 404 map to 503 (base and head sides).
	r := compareFixture(t)
	r.view.resolveErr = errors.New("boom")
	if code, _ := r.get("/demo/spindle/compare/main...topic"); code != http.StatusServiceUnavailable {
		t.Fatalf("base 503 = %d", code)
	}
	r.view.resolveFailOn = "topic"
	if code, _ := r.get("/demo/spindle/compare/main...topic"); code != http.StatusServiceUnavailable {
		t.Fatalf("head 503 = %d", code)
	}

	// Compare read failure maps like any object read.
	g := compareFixture(t)
	g.view.compareErr = api.ErrNotFound
	if code, _ := g.get("/demo/spindle/compare/main...topic"); code != http.StatusNotFound {
		t.Fatalf("compare 404 = %d", code)
	}
	g.view.compareErr = errors.New("boom")
	if code, _ := g.get("/demo/spindle/compare/main...topic"); code != http.StatusServiceUnavailable {
		t.Fatalf("compare 503 = %d", code)
	}

	// Unwired browsing read.
	u := compareFixture(t)
	u.s.env.API.Repo = struct{ api.RepoView }{u.view}
	if code, _ := u.get("/demo/spindle/compare/main...topic"); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d", code)
	}

	// Invisible repo / anonymous read off: the API gate exactly.
	i := compareFixture(t)
	i.reg.exists = map[string]bool{}
	if code, _ := i.get("/demo/spindle/compare/main...topic"); code != http.StatusNotFound {
		t.Fatalf("invisible = %d", code)
	}
	a := newRepoFixture(t, false)
	a.view.compare = api.CompareResult{}
	if code, _ := a.get("/demo/spindle/compare/main...topic"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous read off = %d", code)
	}

	// Empty repository: guided, no compare read.
	e := compareFixture(t)
	e.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	body := e.mustGet("/demo/spindle/compare/main...topic", http.StatusOK)
	if !strings.Contains(body, "empty") || e.view.compareN != 0 {
		t.Fatal("empty repo must guide")
	}
}

func TestSplitCompareHead(t *testing.T) {
	t.Parallel()
	if repo, ref, ok := splitCompareHead("topic", "demo/spindle"); !ok || repo != "demo/spindle" || ref != "topic" {
		t.Fatalf("plain = %q %q %v", repo, ref, ok)
	}
	if repo, ref, ok := splitCompareHead("bob/fork:topic", "demo/spindle"); !ok || repo != "bob/fork" || ref != "topic" {
		t.Fatalf("cross = %q %q %v", repo, ref, ok)
	}
	if _, _, ok := splitCompareHead("topic:", "demo/spindle"); ok {
		t.Fatal("empty branch must fail")
	}
	if _, _, ok := splitCompareHead("norepo:topic", "demo/spindle"); ok {
		t.Fatal("colon form without owner/repo must fail")
	}
}

func TestCompareAnonReadGateViaPrincipal(t *testing.T) {
	t.Parallel()
	// The gate is principal-independent on purpose: compare has no writes.
	f := compareFixture(t)
	f.principal = auth.Anonymous()
	f.rebind()
	if body := f.mustGet("/demo/spindle/compare/main...topic", http.StatusOK); !strings.Contains(body, "Comparing") {
		t.Fatal("anonymous read must render")
	}
}
