// packages.go — Feature 14 composition (docs/features/14_packages.md): the
// OCI Distribution registry, mounted as a top-level namespace (/v2/*) over the
// object store and the P6 roles owned by identity. Permissions are the
// repository's (the package IS the repo), the per-repo [features] toggle is
// read through the same WAL settings projection the summary uses, and the repo
// existence probe rides the serving RepoRegistry — so a registry route can
// never outlive the repo it names.
package main

import (
	"context"
	"net/http"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// collabPackagesSvc is the nil-safe registry accessor the admin wiring passes
// (a restricted boot has no collaboration wiring at all).
func collabPackagesSvc(c *collabWiring) *packages.Service {
	if c == nil {
		return nil
	}
	return c.packagesSvc
}

// newPackagesService builds the registry service over st/ident. Limits come
// from the [packages] config section (compiled-in defaults when unset) and the
// spool lives under cache.dir (the LFS/release spool precedent).
func newPackagesService(st store.ObjectStore, ident *identity.Service, cfg *config.Config, apiEnv *api.Env, cacheDir string) (*packages.Service, *packages.Handler) {
	api.RegisterExposed(packages.ExposedTemplates...)
	svc := packages.New(st, ident)
	svc.MaxBlobBytes = int64(cfg.Packages.MaxBlobBytes)
	svc.MaxManifestBytes = int64(cfg.Packages.MaxManifestBytes)
	svc.UploadTTL = time.Duration(cfg.Packages.UploadTTL)
	svc.AnonymousRead = cfg.Server.Auth.AnonymousRead
	svc.PublicURL = cfg.Server.PublicURL
	if cacheDir != "" {
		svc.SpoolDir = cacheDir + "/package-spool"
	}
	if apiEnv != nil {
		// The [features] projection (Forgejo #522): one settings read,
		// fail-open to enabled when the doc is unreadable (display metadata
		// must never break the registry — the CollabCounts precedent).
		svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			if apiEnv.Repo == nil {
				return config.AllFeatures(), false
			}
			doc, err := apiEnv.Repo.Settings(ctx, git.RepoId{Owner: owner, Name: repo})
			if err != nil {
				return config.AllFeatures(), false
			}
			return config.FeaturesOf([]byte(doc.TOML)), true
		}
		// The registry policy (docs/features/14 §5): the repo settings
		// [packages] section over the instance default quota. One settings
		// read, fail-open to the instance default when the doc is unreadable
		// (display metadata must never invent a quota — the Features rule).
		defaults := cfg.Packages
		svc.Policy = func(ctx context.Context, owner, repo string) config.PackagesPolicy {
			if apiEnv.Repo == nil {
				return config.PackagesPolicy{QuotaBytes: int64(cfg.Packages.QuotaBytes)}
			}
			doc, err := apiEnv.Repo.Settings(ctx, git.RepoId{Owner: owner, Name: repo})
			if err != nil {
				return config.PackagesPolicy{QuotaBytes: int64(cfg.Packages.QuotaBytes)}
			}
			return config.PackagesPolicyOf([]byte(doc.TOML), defaults)
		}
		// Repo existence: the registry is per repository, so a push to an
		// unknown repo is NAME_UNKNOWN rather than an implicit create.
		if apiEnv.Repos != nil {
			svc.Exists = func(ctx context.Context, owner, repo string) (bool, error) {
				return apiEnv.Repos.Exists(ctx, git.RepoId{Owner: owner, Name: repo})
			}
		}
	}
	h := &packages.Handler{Svc: svc, AuthRequired: true}
	return svc, h
}

// chainPackages mounts the registry namespace and binds Seam 2: credentials
// resolve through the same forwarded server chain every other feature surface
// uses (Basic is the git HTTP path exactly — the password is the token).
//
// Two seams: ChainTop owns the /v2 wire namespace (it is not a repo lane), and
// ChainExtra owns the Packages JSON API (`/{o}/{r}/api[-browser]/packages…`,
// docs/features/14 §7) — the same pair the releases surface registers.
func chainPackages(srv *server.Server, h *packages.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainTop(h)
	srv.ChainExtra(h)
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the packages package defines (core never imports packages).
var (
	_ packages.RoleService = (*identity.Service)(nil)
	_ server.ExtraRoutes   = (*packages.Handler)(nil)
)

// packagesSweeper plugs the registry's mark/sweep pass into the maintainer's
// task path (docs/features/14 §6). The due gate keeps an idle registry off the
// bucket AND out of the task table: a repo is swept at most once per
// packages.gc interval, and a real pass runs as task kind `packages-gc`
// (single-flight with any manual start).
type packagesSweeper struct{ svc *packages.Service }

func (packagesSweeper) Kind() string { return packages.TaskKindGC }

// Due implements maintain.DueSweeper: the in-memory cadence gate.
func (w packagesSweeper) Due(repo string) bool {
	owner, name, ok := splitRepo(repo)
	return ok && w.svc != nil && w.svc.SweepDue(owner, name)
}

func (w packagesSweeper) Sweep(ctx context.Context, repo string, t maintain.TaskLogger) error {
	owner, name, ok := splitRepo(repo)
	if !ok || w.svc == nil {
		return nil
	}
	_, err := w.svc.GC(ctx, owner, name, t)
	return err
}

// maintainSweepers is the Options.Sweepers list for the serving build (empty
// when a surface is not wired, e.g. setup-only mode). Every sweep goes through
// the (repo,kind) task path, so a manual start joins the pass instead of racing
// it, and each carries its own in-memory due gate.
func maintainSweepers(c *collabWiring) []maintain.Sweeper {
	if c == nil {
		return nil
	}
	var out []maintain.Sweeper
	if c.packagesSvc != nil {
		out = append(out, packagesSweeper{svc: c.packagesSvc})
	}
	// Insights (docs/features/18): the refresh is a task on its own cadence;
	// the service's head equality gate makes an unchanged repo a no-op.
	if c.insightsSvc != nil {
		out = append(out, &insightsSweeper{svc: c.insightsSvc})
	}
	// Threads (docs/features/15 §8, round-2 pass 2; round 3): `threads-gc` repairs the
	// queue every five minutes and applies the retention window once an hour PER
	// REPOSITORY (two bodies, one kind, each with its own per-repo gate — a single
	// process-wide gate let only the first repo of each hour retain); `threads-schedule`
	// evaluates `schedule:` cron against the persisted watermark on a minute-aligned gate.
	if c.threadsSvc != nil {
		out = append(out, newThreadsSweepers(c.threadsSvc)...)
	}
	return out
}
