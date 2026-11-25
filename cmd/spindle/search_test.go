// search_test.go — the P2 composition (docs/features/20_search.md §3/§9):
// the search service over the REAL WAL registry, repository registry, identity
// read gate and git primitive. A real commit is seeded into the serving copy,
// so the seams (ref resolution, refs-only warmth, serving dir, gating) are
// exercised in their shipped shape rather than through doubles.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

func testSearchConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	return cfg
}

// seedSearchCommit writes one file into the bare repository's object store and
// points refs/heads/main at the commit. It is the fixture's stand-in for a
// push: the objects and the ref land in the serving copy the grep reads.
func seedSearchCommit(t *testing.T, gitDir, name, body string) string {
	t.Helper()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = gitDir
		cmd.Env = append(os.Environ(),
			"GIT_DIR="+gitDir,
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run(body, "hash-object", "-w", "--stdin")
	tree := run(fmt.Sprintf("100644 blob %s\t%s\n", blob, name), "mktree")
	commit := run("", "commit-tree", tree, "-m", "seed")
	run("", "update-ref", "refs/heads/main", commit)
	return commit
}

// searchComposition is one shipped-shape wiring: registry + repo listing +
// identity gate + the search service.
type searchComposition struct {
	svc    *search.Service
	repos  *searchRepos
	ident  *identity.Service
	issues *issues.Service
	reg    *wal.Registry
}

func newSearchComposition(t *testing.T) *searchComposition {
	t.Helper()
	ctx := context.Background()
	cfg := testSearchConfig(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	ident := identity.New(st, cfg)
	iss := issues.New(st, ident)
	apiEnv := api.NewEnv(st, &repoRegistry{reg: reg, st: st}, cfg, nil, "test", "test")
	return &searchComposition{
		svc:    newSearchService(cfg, reg, apiEnv, ident, iss),
		repos:  &searchRepos{reg: reg, listing: &repoRegistry{reg: reg, st: st}, binary: cfg.Git.Binary},
		ident:  ident,
		issues: iss,
		reg:    reg,
	}
}

func TestSearchCompositionRealStack(t *testing.T) {
	ctx := context.Background()
	c := newSearchComposition(t)
	h, err := c.reg.Create(ctx, "acme/app", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sha := seedSearchCommit(t, h.Dir(), "needle.txt", "a needle in the tree\n")

	scope := search.Scope{Kind: search.ScopeRepo, Owner: "acme", Repo: "app"}
	// Anonymous on a repository with no access.json: the legacy public
	// default allows the read (identity's documented fallback).
	ans, err := c.svc.Search(ctx, scope, search.Params{Text: "needle"}, auth.Anonymous())
	if err != nil {
		t.Fatalf("anonymous public search: %v", err)
	}
	r := ans.Code.Results[0]
	if r.SHA != sha || r.Ref != "main" || r.MatchCount != 1 || r.Files[0].Path != "needle.txt" {
		t.Fatalf("result = %+v", r)
	}

	// Making it private flips the same query to 401 and never leaks the
	// repository name in the refusal.
	if _, err := c.ident.PutAccess(ctx, "acme", "app", "", identity.VisibilityPrivate, nil); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	_, err = c.svc.Search(ctx, scope, search.Params{Text: "needle"}, auth.Anonymous())
	if !errors.Is(err, search.ErrUnauthorized) {
		t.Fatalf("anonymous private: err = %v", err)
	}
	if strings.Contains(err.Error(), "acme") {
		t.Fatalf("refusal leaks the repository: %q", err.Error())
	}
	// A signed-in principal without a role is 403 ...
	if _, err := c.svc.Search(ctx, scope, search.Params{Text: "needle"}, auth.Principal{Name: "mallory"}); !errors.Is(err, search.ErrForbidden) {
		t.Fatalf("signed-in denied: err = %v", err)
	}
	// ... and an admin is allowed.
	if _, err := c.svc.Search(ctx, scope, search.Params{Text: "needle"}, auth.Principal{Name: "root", Admin: true}); err != nil {
		t.Fatalf("admin: %v", err)
	}

	// Owner scope: the private repository is absent from results and counts
	// for the anonymous caller, and present for the admin.
	owner := search.Scope{Kind: search.ScopeOwner, Owner: "acme"}
	ans, err = c.svc.Search(ctx, owner, search.Params{Text: "needle", Type: "code"}, auth.Anonymous())
	if err != nil {
		t.Fatalf("anonymous owner: %v", err)
	}
	if ans.Code.ReposConsidered != 0 || len(ans.Code.Results) != 0 || !ans.Code.Complete {
		t.Fatalf("anonymous owner section = %+v", ans.Code)
	}
	ans, err = c.svc.Search(ctx, owner, search.Params{Text: "needle", Type: "code"}, auth.Principal{Name: "root", Admin: true})
	if err != nil {
		t.Fatalf("admin owner: %v", err)
	}
	if ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 || ans.Code.Results[0].Repo != "acme/app" {
		t.Fatalf("admin owner section = %+v", ans.Code)
	}
}

func TestSearchCompositionIssueWiring(t *testing.T) {
	ctx := context.Background()
	c := newSearchComposition(t)
	if _, err := c.reg.Create(ctx, "acme/app", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.issues.CreateIssue(ctx, "acme", "app", auth.Principal{Name: "jane", Write: true}, "Needle in the title", "a body without the word", nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	scope := search.Scope{Kind: search.ScopeRepo, Owner: "acme", Repo: "app"}
	ans, err := c.svc.Search(ctx, scope, search.Params{Text: "needle", Type: "issues"}, auth.Anonymous())
	if err != nil {
		t.Fatalf("issue search: %v", err)
	}
	if ans.Issues == nil || len(ans.Issues.Hits) != 1 || ans.Issues.Hits[0].Matched != search.MatchTitle {
		t.Fatalf("issue section = %+v", ans.Issues)
	}
	if ans.Issues.Hits[0].URL != "/acme/app/issues/1" {
		t.Fatalf("hit url = %q", ans.Issues.Hits[0].URL)
	}

	// Owner scope uses the same visible-repository candidate path.
	ans, err = c.svc.Search(ctx, search.Scope{Kind: search.ScopeOwner, Owner: "acme"}, search.Params{Text: "needle", Type: "issues"}, auth.Anonymous())
	if err != nil {
		t.Fatalf("owner issue search: %v", err)
	}
	if ans.Issues == nil || len(ans.Issues.Repos) != 1 || len(ans.Issues.Hits) != 1 {
		t.Fatalf("owner issue section = %+v", ans.Issues)
	}
}

func TestSearchReposSeam(t *testing.T) {
	ctx := context.Background()
	c := newSearchComposition(t)
	h, err := c.reg.Create(ctx, "acme/app", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sha := seedSearchCommit(t, h.Dir(), "f.txt", "needle\n")

	// ResolveRepo with no ref reports the default branch and its tip.
	tgt, err := c.repos.ResolveRepo(ctx, "acme/app", "")
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if tgt.SHA != sha || tgt.Ref != "main" || tgt.Repo == nil || tgt.Repo.Path != h.Dir() {
		t.Fatalf("target = %+v", tgt)
	}
	// An explicit sha is resolved too (rev-parse accepts the object id).
	if tgt, err = c.repos.ResolveRepo(ctx, "acme/app", sha); err != nil || tgt.SHA != sha {
		t.Fatalf("sha resolve = %+v, %v", tgt, err)
	}
	// A well-formed unknown ref is the 404 class with the existing body.
	if _, err := c.repos.ResolveRepo(ctx, "acme/app", "no-such-ref"); !errors.Is(err, search.ErrNotFound) || !strings.Contains(err.Error(), "unknown revision") {
		t.Fatalf("unknown ref: %v", err)
	}
	// An unknown repository is not-found too.
	if _, err := c.repos.ResolveRepo(ctx, "acme/missing", ""); !errors.Is(err, search.ErrNotFound) {
		t.Fatalf("unknown repo: %v", err)
	}
	// An unborn repository says so (§4).
	if _, err := c.reg.Create(ctx, "acme/empty", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repos.ResolveRepo(ctx, "acme/empty", ""); !errors.Is(err, search.ErrNotFound) || !strings.Contains(err.Error(), "empty repository") {
		t.Fatalf("unborn: %v", err)
	}
	// Owner-scope warmth never needs packs: refs-only, PacksReady, the tip.
	tgt, ready, err := c.repos.WarmRefs(ctx, "acme/app")
	if err != nil || !ready || tgt.SHA != sha {
		t.Fatalf("WarmRefs = %+v ready=%v err=%v", tgt, ready, err)
	}
	tgt, ready, err = c.repos.WarmRefs(ctx, "acme/empty")
	if err != nil || !ready || tgt.SHA != "" {
		t.Fatalf("unborn WarmRefs = %+v ready=%v err=%v", tgt, ready, err)
	}
	// The owner listing is the registry's, and an unwired listing is the 503
	// class (never an empty owner that reads as "no repositories").
	names, err := c.repos.ListOwner(ctx, "acme")
	if err != nil {
		t.Fatalf("ListOwner: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
	if _, err := (&searchRepos{reg: c.reg}).ListOwner(ctx, "acme"); !errors.Is(err, search.ErrUnavailable) {
		t.Fatalf("unwired listing: %v", err)
	}
}

func TestNewSearchServiceIsNilSafe(t *testing.T) {
	ctx := context.Background()
	scope := search.Scope{Kind: search.ScopeRepo, Owner: "acme", Repo: "app"}
	// No registry: the service answers unavailable (its seams are missing).
	svc := newSearchService(config.Defaults(), nil, nil, nil, nil)
	if _, err := svc.Search(ctx, scope, search.Params{Text: "x"}, auth.Anonymous()); !errors.Is(err, search.ErrUnavailable) {
		t.Fatalf("nil composition: %v", err)
	}
	// Registry but no identity gate: fail closed (never an ungated search).
	cfg := testSearchConfig(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	t.Cleanup(reg.Close)
	svc = newSearchService(cfg, reg, nil, nil, nil)
	if _, err := svc.Search(ctx, scope, search.Params{Text: "x"}, auth.Anonymous()); !errors.Is(err, search.ErrUnavailable) {
		t.Fatalf("ungated composition: %v", err)
	}
	// An explicit `enabled = false` answers the disabled class.
	cfg.Search.Enabled = config.Bool(false)
	svc = newSearchService(cfg, reg, nil, identity.New(store.NewMemory(), cfg), nil)
	if _, err := svc.Search(ctx, scope, search.Params{Text: "x"}, auth.Anonymous()); !errors.Is(err, search.ErrDisabled) {
		t.Fatalf("disabled composition: %v", err)
	}
}

func TestSearchErrorMappers(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		open  error
		serve error
		refs  error
	}{
		{"engine not found", wal.ErrNotFound("acme/app"), search.ErrNotFound, search.ErrNotFound, search.ErrNotFound},
		{"store not found", store.NewNotFound("repos/acme/app/manifest.pb"), search.ErrNotFound, search.ErrUnavailable, search.ErrUnavailable},
		{"serve timeout", &wal.WalError{Kind: wal.WalErrTimeout}, search.ErrWarming, search.ErrWarming, search.ErrUnavailable},
		{"too large", wal.ErrTooLarge(100, 10), search.ErrNotLocal, search.ErrNotLocal, search.ErrUnavailable},
		{"other", errors.New("store exploded"), search.ErrUnavailable, search.ErrUnavailable, search.ErrUnavailable},
	}
	for _, tc := range cases {
		if got := searchOpenErr(tc.err); !errors.Is(got, tc.open) {
			t.Errorf("%s open(%v) = %v, want %v", tc.name, tc.err, got, tc.open)
		}
		if got := searchServeErr(tc.err); !errors.Is(got, tc.serve) {
			t.Errorf("%s serve(%v) = %v, want %v", tc.name, tc.err, got, tc.serve)
		}
		if got := searchRefsErr(tc.err); !errors.Is(got, tc.refs) {
			t.Errorf("%s refs(%v) = %v, want %v", tc.name, tc.err, got, tc.refs)
		}
	}
}
