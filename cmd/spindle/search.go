// search.go — composition for the code-search service (docs/features/20_search.md,
// P2). The service itself lives in internal/search and owns no I/O: this file
// binds its three seams over the WAL registry and stock git:
//
//   - the grep primitive is git.Layer (its derived ScanPool, never the
//     control-plane pool — 04_git.md §17.2);
//   - the repository source is the WAL registry (listings from the repository
//     registry, refs-only warmth for owner scope, serve-level materialization
//     for single-repository scope);
//   - the visibility gate is identity.Service.CheckRead, the one read
//     authority every AuthRead route already consults (§3, §8).
//
// P3 owns the routes; the service is stored on collabWiring so the HTTP layer
// binds it in its own change.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// searchResolveTimeout bounds the one `git rev-parse` a search ref resolution
// runs. It is not a scan: the read is a ref lookup in the serving copy.
const searchResolveTimeout = 10 * time.Second

// newSearchService builds the service over reg/ident. It is nil-safe: a boot
// without the collaboration wiring gets a service whose seams answer
// ErrUnavailable, never a panic and never an ungated search.
func newSearchService(cfg *config.Config, reg *wal.Registry, apiEnv *api.Env, ident *identity.Service, iss *issues.Service) *search.Service {
	opts := search.Options{Disabled: !cfg.Search.IsEnabled(), MaxConcurrent: cfg.Search.MaxConcurrent}
	if reg != nil {
		opts.Git = reg.GitLayer()
		var listing api.RepoRegistry
		if apiEnv != nil {
			listing = apiEnv.Repos
		}
		opts.Repos = &searchRepos{reg: reg, listing: listing, binary: cfg.Git.Binary}
	}
	if ident != nil {
		// A nil *identity.Service assigned to the interface would be a typed
		// nil: CheckRead on a nil receiver panics. Assign only the real value.
		opts.Access = ident
	}
	// P4: issue/PR search over the existing index/card/event reads. A nil
	// issues service (a boot without the collaboration layer) stays nil, so
	// type=issues answers ErrIssuesUnavailable rather than an empty section.
	if iss != nil {
		opts.Issues = search.NewIssueSearcher(iss)
	}
	return search.New(opts)
}

// searchRepos is the search service's RepoSource seam (docs/features/20_search.md
// §3/§9) over the WAL registry, the repository registry and stock git.
type searchRepos struct {
	reg     *wal.Registry
	listing api.RepoRegistry // the owner listing (api.Env.Repos); nil = unwired
	binary  string
}

var _ search.RepoSource = (*searchRepos)(nil)

// ListOwner is the only owner-scope candidate source (§3): the repository
// registry's owner listing — one LIST plus one manifest HEAD per name, the
// same cost every listing pays, never a disk walk and never an instance-wide
// LIST. The service bounds the candidates it considers (50) after this
// returns; this method adds no second list and no per-candidate probe.
func (r *searchRepos) ListOwner(ctx context.Context, owner string) ([]string, error) {
	if r.listing == nil {
		return nil, search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
	}
	names, err := r.listing.Repos(ctx, owner)
	if err != nil {
		return nil, search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
	}
	return names, nil
}

// ResolveRepo is single-repository scope (§9): resolve the requested ref ("" =
// the default branch), materialize the serving copy to serve level under the
// engine's serve bound, and hand back the bare copy. This is the one search
// path that may download packs — the caller asked for this repository.
func (r *searchRepos) ResolveRepo(ctx context.Context, repo, ref string) (search.Target, error) {
	h, err := r.open(ctx, repo)
	if err != nil {
		return search.Target{}, err
	}
	guard, err := h.Sync(ctx, wal.LevelServe)
	if err != nil {
		return search.Target{}, searchServeErr(err)
	}
	guard.Release() // objects stay materialized on disk (§9, the pullsDirs shape)

	branch, sha, err := repoHead(ctx, h)
	if err != nil {
		return search.Target{}, search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
	}
	if ref != "" {
		sha, err = r.resolveRef(ctx, h.Dir(), ref)
		if err != nil {
			return search.Target{}, err
		}
		branch = ref
	} else if sha == "" {
		// HEAD names a branch with no tip: an unborn repository has no tree to
		// search (04_git.md §17, the "empty repository" 404 of §4).
		return search.Target{}, search.Wrap(search.ErrNotFound, "empty repository: "+repo)
	}
	return search.Target{Repo: h.Repo(), Ref: branch, SHA: sha}, nil
}

// WarmRefs is owner scope (§3.1): sync refs only (cheap), never materialize,
// and report ready=false when the local pack set is not current. A 50-repo
// query must never start 50 downloads, so [search] never warms a repository.
func (r *searchRepos) WarmRefs(ctx context.Context, repo string) (search.Target, bool, error) {
	h, err := r.open(ctx, repo)
	if err != nil {
		return search.Target{}, false, err
	}
	guard, err := h.Sync(ctx, wal.LevelRefs)
	if err != nil {
		return search.Target{}, false, searchRefsErr(err)
	}
	guard.Release()
	if !h.PacksReady() {
		// Read-only snapshot: no sync, no fetch (wal.RepoHandle.PacksReady).
		return search.Target{}, false, nil
	}
	branch, sha, err := repoHead(ctx, h)
	if err != nil {
		return search.Target{}, false, search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
	}
	return search.Target{Repo: h.Repo(), Ref: branch, SHA: sha}, true, nil
}

// open resolves the repository handle, mapping the engine's not-found onto the
// service's 404 class.
func (r *searchRepos) open(ctx context.Context, repo string) (*wal.RepoHandle, error) {
	h, err := r.reg.Open(ctx, repo)
	if err != nil {
		return nil, searchOpenErr(err)
	}
	return h, nil
}

// resolveRef resolves any ref to its commit sha with the argv pulls' ResolveRef
// documents (§3 of docs/features/03; 04_git.md's read discipline):
//
//	git rev-parse --verify --quiet <ref>^{commit}
//
// A non-zero exit is a well-formed-but-unknown ref (404); a missing binary or
// timeout is unavailable (503) — never misreported as unknown (§10).
func (r *searchRepos) resolveRef(ctx context.Context, dir, ref string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, searchResolveTimeout)
	defer cancel()
	binary := r.binary
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(cctx, binary, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", search.Wrap(search.ErrNotFound, fmt.Sprintf("unknown revision %q", ref))
		}
		return "", search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" || !git.ValidOid(sha) {
		return "", search.Wrap(search.ErrNotFound, fmt.Sprintf("unknown revision %q", ref))
	}
	return sha, nil
}

// repoHead reports the repository's default branch (short name) and tip,
// mirroring the refs the WAL is the authority for: HEAD's target, with the
// tag peel rule the snapshot already applies. sha is "" for an unborn
// repository.
func repoHead(ctx context.Context, h *wal.RepoHandle) (branch, sha string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	snap, err := h.Layer().Snapshot(h.Repo())
	if err != nil {
		return "", "", err
	}
	target := snap.HeadTarget
	if target == "" {
		target = "refs/heads/main"
	}
	name := strings.TrimPrefix(target, "refs/heads/")
	for _, ref := range snap.Refs {
		if ref.Name == target {
			if ref.Peeled != "" {
				return name, ref.Peeled, nil
			}
			return name, string(ref.Oid), nil
		}
	}
	return name, "", nil // unborn HEAD
}

// searchOpenErr maps engine errors onto the service's classes: not-found is the
// 404 class; everything else (store, corrupt manifest, CAS) is unavailable.
func searchOpenErr(err error) error {
	var we *wal.WalError
	if errors.As(err, &we) {
		switch we.Kind {
		case wal.WalErrNotFound:
			return search.Wrap(search.ErrNotFound, "not found")
		case wal.WalErrTimeout:
			return search.Wrap(search.ErrWarming, search.ErrWarming.Error())
		case wal.WalErrTooLarge:
			return search.Wrap(search.ErrNotLocal, search.ErrNotLocal.Error())
		}
	}
	// An absent manifest reaches the caller as the store's not-found (the
	// registry's Open probes without a version, so absent is an unconditional
	// GET miss): the same 404 class as the engine's ErrNotFound.
	if store.IsNotFound(err) {
		return search.Wrap(search.ErrNotFound, "not found")
	}
	return search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
}

// searchServeErr maps a serve-level sync failure (§9): the serve bound firing
// is "warming" (503 + Retry-After at the transport), a pack set over the cache
// budget is "no local serving copy", and not-found stays not-found.
func searchServeErr(err error) error {
	var we *wal.WalError
	if errors.As(err, &we) {
		switch we.Kind {
		case wal.WalErrNotFound:
			return search.Wrap(search.ErrNotFound, "not found")
		case wal.WalErrTimeout:
			return search.Wrap(search.ErrWarming, search.ErrWarming.Error())
		case wal.WalErrTooLarge:
			return search.Wrap(search.ErrNotLocal, search.ErrNotLocal.Error())
		}
	}
	return search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
}

// searchRefsErr maps a refs-only sync failure. Owner scope never waits on a
// pack phase, so a timeout here is an outage, not a warming repository.
func searchRefsErr(err error) error {
	var we *wal.WalError
	if errors.As(err, &we) && we.Kind == wal.WalErrNotFound {
		return search.Wrap(search.ErrNotFound, "not found")
	}
	return search.Wrap(search.ErrUnavailable, search.ErrUnavailable.Error())
}
