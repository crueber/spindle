package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/actionscache"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// The cache service's policy half: the scope matrix (D-10), a save/restore
// round trip through two runs, the visibility rules, the GC family and the
// repository API.

func cacheSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// cachePush creates one push run on branch (the fixture fires on push:main and
// workflow_dispatch, so a push is the easiest run to create).
func (e *testEnv) cachePush(t *testing.T, branch string) *Run {
	t.Helper()
	e.git.refs["refs/heads/"+branch] = testSHA
	opts := CreateOpts{Branch: branch, Ref: "refs/heads/" + branch, Actor: "writer"}
	if branch != "main" {
		// The fixture's push trigger is main-only; a dispatch records the branch
		// the same way and always fires.
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{
			Branch: branch, Ref: "refs/heads/" + branch, Actor: "writer", Workflow: "ci.yml",
		})
		if err != nil || len(runs) == 0 {
			t.Fatalf("dispatch run on %s: %v (%d)", branch, err, len(runs))
		}
		return runs[0]
	}
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, opts)
	if err != nil || len(runs) == 0 {
		t.Fatalf("push run: %v (%d)", err, len(runs))
	}
	return runs[0]
}

// cachePR creates one pull_request run (trusted or fork) for PR num.
func (e *testEnv) cachePR(t *testing.T, num int, fork bool) *Run {
	t.Helper()
	opts := CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/" + itoa(num) + "/head",
		BaseBranch: "main", Actor: "writer", Fork: fork,
		EventPayload: PullEventPayload("o/r", "opened", num, "t", "open", "writer",
			"refs/heads/main", testSHA, "refs/heads/feat", testSHA, "o/r", false, false, fork, "writer"),
	}
	if fork {
		opts.Actor = "outsider"
		opts.EventPayload = PullEventPayload("o/r", "opened", num, "t", "open", "outsider",
			"refs/heads/main", testSHA, "refs/heads/feat", testSHA, "fork/r", false, false, true, "outsider")
	}
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, opts)
	if err != nil || len(runs) == 0 {
		t.Fatalf("pr run: %v (%d)", err, len(runs))
	}
	return runs[0]
}

// cacheClaims is an attempt token for the run's first job.
func (e *testEnv) cacheClaims(t *testing.T, run *Run) AttemptClaims {
	t.Helper()
	claims := e.artifactClaims(t, run)
	claims.Scopes = attemptScopes(nil)
	return claims
}

// saveCache uploads body and commits it under key with the run's scope.
func (e *testEnv) saveCache(t *testing.T, run *Run, key, version, body string) *CacheView {
	t.Helper()
	claims := e.cacheClaims(t, run)
	sha := cacheSHA(body)
	if err := e.svc.PutCacheBlobForJob(ctx(), claims, sha, int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatalf("PutCacheBlobForJob: %v", err)
	}
	view, err := e.svc.SaveCacheForJob(ctx(), claims, SaveCacheInput{Key: key, Version: version, SHA256: sha, Size: int64(len(body)), Files: 1})
	if err != nil {
		t.Fatalf("SaveCacheForJob: %v", err)
	}
	return view
}

func TestCacheScopeMatrix(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"

	main := e.cachePush(t, "main")
	feature := e.cachePush(t, "feat")
	pr := e.cachePR(t, 7, false)
	fork := e.cachePR(t, 8, true)

	// A tag push: no branch, so no writable scope.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(workflowYAML, "branches: [main]", "tags: ['v*']", 1))
	tagRuns, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		RefKind: workflow.RefTag, RefName: "v1", Ref: "refs/tags/v1", Actor: "writer",
	})
	if err != nil || len(tagRuns) == 0 {
		t.Fatalf("tag run: %v (%d)", err, len(tagRuns))
	}

	cases := []struct {
		name  string
		run   *Run
		write string // "" = no writable scope
		read  []string
	}{
		{"default branch", main, "default", []string{"default"}},
		{"feature branch", feature, "branch:feat", []string{"branch:feat", "default"}},
		{"trusted PR", pr, "pr:7", []string{"pr:7", "default"}},
		{"fork PR", fork, "pr:8:untrusted", []string{"pr:8:untrusted", "default"}},
		{"tag push", tagRuns[0], "", []string{"default"}},
	}
	for _, tc := range cases {
		set, err := e.svc.cacheScopesForRun(ctx(), "o", "r", tc.run)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		write := ""
		if set.Write != nil {
			write = set.Write.String()
		}
		if write != tc.write {
			t.Fatalf("%s: write = %q, want %q", tc.name, write, tc.write)
		}
		got := make([]string, 0, len(set.Read))
		for _, sc := range set.Read {
			got = append(got, sc.String())
		}
		if strings.Join(got, ",") != strings.Join(tc.read, ",") {
			t.Fatalf("%s: read = %v, want %v", tc.name, got, tc.read)
		}
	}

	// A tag run cannot save.
	claims := e.cacheClaims(t, tagRuns[0])
	body := "tagbody"
	sha := cacheSHA(body)
	if err := e.svc.PutCacheBlobForJob(ctx(), claims, sha, int64(len(body)), strings.NewReader(body)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tag cache write = %v", err)
	}
}

func TestCacheRoundTripAcrossRunsAndRestoreKeys(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	first := e.cachePush(t, "main")
	e.saveCache(t, first, "npm-lock-111", "v1", "npm-cache-bytes")

	second := e.cachePush(t, "main")
	claims := e.cacheClaims(t, second)
	// Exact hit.
	hit, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "npm-lock-111", Version: "v1"})
	if err != nil || !hit.Hit || !hit.Exact || hit.SHA256 != cacheSHA("npm-cache-bytes") {
		t.Fatalf("exact lookup = %+v %v", hit, err)
	}
	// The payload streams back, byte for byte.
	body, size, err := e.svc.CacheBlobForJob(ctx(), claims, hit.SHA256)
	if err != nil || size != int64(len("npm-cache-bytes")) {
		t.Fatalf("blob = %v %v", size, err)
	}
	raw := make([]byte, size)
	_, _ = body.Read(raw)
	_ = body.Close()
	if string(raw) != "npm-cache-bytes" {
		t.Fatalf("blob body = %q", raw)
	}
	// A restore-key prefix selects it, flagged not-exact.
	hit, err = e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "npm-lock-999", Version: "v1", RestoreKeys: []string{"npm-lock-"}})
	if err != nil || !hit.Hit || hit.Exact {
		t.Fatalf("restore-key lookup = %+v %v", hit, err)
	}
	// A miss is not an error.
	miss, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "nothing", Version: "v1"})
	if err != nil || miss.Hit || miss.Reason == "" {
		t.Fatalf("miss = %+v %v", miss, err)
	}
	// A different version is a miss.
	if miss, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "npm-lock-111", Version: "v2"}); err != nil || miss.Hit {
		t.Fatalf("version miss = %+v %v", miss, err)
	}
}

func TestCachePRIsolation(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"

	trusted := e.cachePR(t, 11, false)
	e.saveCache(t, trusted, "key-1", "v1", "trusted-bytes")

	// Another PR cannot read it, even with the exact key and a restore prefix.
	other := e.cachePR(t, 12, false)
	claims := e.cacheClaims(t, other)
	if hit, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "key-1", Version: "v1", RestoreKeys: []string{"key-"}}); err != nil || hit.Hit {
		t.Fatalf("PR 12 read PR 11's cache: %+v %v", hit, err)
	}
	// The known digest is not readable either (scope, not key, is the gate).
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, cacheSHA("trusted-bytes")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("digest read across PRs = %v", err)
	}

	// A fork PR writes only the untrusted half; a trusted run of the same PR
	// cannot read that entry.
	fork := e.cachePR(t, 13, true)
	e.saveCache(t, fork, "fork-key", "v1", "fork-bytes")
	entry, lerr := e.svc.cacheEngine().List(ctx(), "o", "r")
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	var forkScope string
	for _, ent := range entry {
		if ent.Key == "fork-key" {
			forkScope = ent.Scope
		}
	}
	if forkScope != "pr:13:untrusted" {
		t.Fatalf("fork write scope = %q", forkScope)
	}
	// A trusted PR 13 run (same number, trusted half) misses.
	trusted13 := e.cachePR(t, 13, false)
	if hit, err := e.svc.CacheLookupForJob(ctx(), e.cacheClaims(t, trusted13), CacheLookupInput{Key: "fork-key", Version: "v1"}); err != nil || hit.Hit {
		t.Fatalf("trusted PR read a fork entry: %+v %v", hit, err)
	}
	// The fork itself restores its own entry.
	if hit, err := e.svc.CacheLookupForJob(ctx(), e.cacheClaims(t, fork), CacheLookupInput{Key: "fork-key", Version: "v1"}); err != nil || !hit.Hit {
		t.Fatalf("fork restore = %+v %v", hit, err)
	}
}

func TestCacheScopeFallbackFromABranchToDefault(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	e.saveCache(t, e.cachePush(t, "main"), "shared-1", "v1", "default-bytes")
	feat := e.cachePush(t, "feat")
	hit, err := e.svc.CacheLookupForJob(ctx(), e.cacheClaims(t, feat), CacheLookupInput{Key: "shared-1", Version: "v1"})
	if err != nil || !hit.Hit || hit.Scope != "default" {
		t.Fatalf("feature-branch fallback = %+v %v", hit, err)
	}
}

func TestCacheRefusalsAndValidation(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	claims := e.cacheClaims(t, run)

	// A key with a control character is refused by name.
	if _, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "bad\x01", Version: "v1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control key = %v", err)
	}
	// A save whose payload was never uploaded.
	if _, err := e.svc.SaveCacheForJob(ctx(), claims, SaveCacheInput{Key: "k", Version: "v1", SHA256: cacheSHA("missing"), Size: 7}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("save without payload = %v", err)
	}
	// A blob whose claimed digest is not what arrives.
	if err := e.svc.PutCacheBlobForJob(ctx(), claims, cacheSHA("expected"), 4, strings.NewReader("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("digest mismatch = %v", err)
	}
	// An attempt token without the cache scope cannot use the service.
	noScope := claims
	noScope.Scopes = []string{ScopeLog}
	if _, err := e.svc.CacheLookupForJob(ctx(), noScope, CacheLookupInput{Key: "k", Version: "v1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing cache scope = %v", err)
	}
	// A pull_request run whose payload lost its number fails closed.
	broken := *run
	broken.Event = EventPullReq
	broken.EventPayload = nil
	if _, err := e.svc.cacheScopesForRun(ctx(), "o", "r", &broken); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PR without a number = %v", err)
	}
	if _, err := e.svc.cacheScopesForRun(ctx(), "o", "r", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil run = %v", err)
	}
}

func TestCacheRepositoryAPIAndDelete(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	view := e.saveCache(t, run, "k", "v1", "bytes")

	entries, err := e.svc.ListCaches(ctx(), "o/r", e.reader())
	if err != nil || len(entries) != 1 || entries[0].SHA256 != view.SHA256 {
		t.Fatalf("ListCaches = %+v %v", entries, err)
	}
	// An anonymous reader of a public repo can list (the read gate mirrors the
	// run page).
	if _, err := e.svc.ListCaches(ctx(), "o/r", e.public()); err != nil {
		t.Fatalf("public list: %v", err)
	}
	// Delete needs maintain: a reader is refused, a writer is refused, a
	// maintainer is allowed.
	if _, err := e.svc.DeleteCache(ctx(), "o/r", entries[0].ID, e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader delete = %v", err)
	}
	if _, err := e.svc.DeleteCache(ctx(), "o/r", entries[0].ID, e.writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer delete = %v", err)
	}
	audits := 0
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits++
		if action != "threads.cache.delete" {
			t.Errorf("audit action = %q", action)
		}
		return nil
	}
	e.roles.roles["maint"] = "maintain"
	freed, err := e.svc.DeleteCache(ctx(), "o/r", entries[0].ID, auth.Principal{Name: "maint"})
	if err != nil || freed == 0 || audits != 1 {
		t.Fatalf("maintain delete = %d %v audits=%d", freed, err, audits)
	}
	if _, err := e.svc.DeleteCache(ctx(), "o/r", entries[0].ID, auth.Principal{Name: "maint"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestCacheRetentionReclaimsAndLiveRunSurvives(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	view := e.saveCache(t, run, "k", "v1", "abcdefgh")
	res, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	// The entry is fresh, so nothing is reclaimed.
	if res.CachesDeleted != 0 || res.CacheBytes != 0 {
		t.Fatalf("fresh reclaim = %+v", res)
	}
	// Advance past the idle window: the cache family reclaims it and reports the
	// bytes separately, while the run (still younger than retention) survives.
	e.advance(8 * 24 * time.Hour)
	res, err = e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("Retain 2: %v", err)
	}
	if res.CachesDeleted != 1 || res.CacheBytes != int64(len("abcdefgh")) {
		t.Fatalf("cache reclaim = %+v", res)
	}
	if !strings.Contains(res.String(), "caches=1") || !strings.Contains(res.String(), "cache=") {
		t.Fatalf("report = %q", res.String())
	}
	if entries, _ := e.svc.ListCaches(ctx(), "o/r", e.reader()); len(entries) != 0 {
		t.Fatalf("entries after reclaim = %+v", entries)
	}
	if hit, lerr := e.svc.CacheLookupForJob(ctx(), e.cacheClaims(t, run), CacheLookupInput{Key: "k", Version: "v1"}); lerr != nil || hit.Hit {
		t.Fatalf("lookup after reclaim = %+v %v", hit, lerr)
	}
	_ = view
}

func TestDiscardPRCaches(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	pr := e.cachePR(t, 21, false)
	fork := e.cachePR(t, 21, true)
	e.saveCache(t, pr, "k", "v1", "trusted")
	e.saveCache(t, fork, "k2", "v1", "fork")
	e.saveCache(t, e.cachePush(t, "main"), "keep", "v1", "default")

	freed, err := e.svc.DiscardPRCaches(ctx(), "o/r", 21)
	if err != nil || freed != int64(len("trusted")+len("fork")) {
		t.Fatalf("DiscardPRCaches = %d %v", freed, err)
	}
	entries, _ := e.svc.ListCaches(ctx(), "o/r", e.reader())
	if len(entries) != 1 || entries[0].Key != "keep" {
		t.Fatalf("entries after discard = %+v", entries)
	}
	if _, err := e.svc.DiscardPRCaches(ctx(), "o/r", 21); err != nil {
		t.Fatalf("second discard: %v", err)
	}
	if _, err := e.svc.DiscardPRCaches(ctx(), "o/r", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero PR = %v", err)
	}
}

func TestCacheCapsAndEngineDefaults(t *testing.T) {
	e := newTestEnv(t)
	caps := e.svc.CacheCaps()
	if caps.MaxBytesPerEntry != actionscache.DefaultMaxEntryBytes || caps.MaxBytesPerRepo != actionscache.DefaultMaxBytes ||
		caps.MaxEntries != actionscache.DefaultMaxEntries || caps.RetentionDays != 7 {
		t.Fatalf("default caps = %+v", caps)
	}
	e.svc.MaxCacheBytes = 1024
	e.svc.MaxCacheEntryBytes = 4096 // clamped to the repo budget
	e.svc.CacheRetention = 48 * time.Hour
	caps = e.svc.CacheCaps()
	if caps.MaxBytesPerRepo != 1024 || caps.MaxBytesPerEntry != 1024 || caps.RetentionDays != 2 {
		t.Fatalf("clamped caps = %+v", caps)
	}
}
