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
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// newPackagesService builds the registry service over st/ident. Limits come
// from the [packages] config section (compiled-in defaults when unset) and the
// spool lives under cache.dir (the LFS/release spool precedent).
func newPackagesService(st store.ObjectStore, ident *identity.Service, cfg *config.Config, apiEnv *api.Env, cacheDir string) (*packages.Service, *packages.Handler) {
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
		// Repo existence: the registry is per repository, so a push to an
		// unknown repo is NAME_UNKNOWN rather than an implicit create.
		if apiEnv.Repos != nil {
			svc.Exists = func(ctx context.Context, owner, repo string) (bool, error) {
				return apiEnv.Repos.Exists(ctx, git.RepoId{Owner: owner, Name: repo})
			}
		}
	}
	h := &packages.Handler{Svc: svc, AuthRequired: cfg.Server.Auth.Mode != "none"}
	return svc, h
}

// chainPackages mounts the registry namespace and binds Seam 2: credentials
// resolve through the same forwarded server chain every other feature surface
// uses (Basic is the git HTTP path exactly — the password is the token).
func chainPackages(srv *server.Server, h *packages.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainTop(h)
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the packages package defines (core never imports packages).
var (
	_ packages.RoleService = (*identity.Service)(nil)
	_ server.ExtraRoutes   = (*packages.Handler)(nil)
)
