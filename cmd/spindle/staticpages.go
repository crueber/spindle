// staticpages.go — Feature 19 composition (docs/features/19_static_sites.md):
// the dedicated static-site listener's handler over the same serving seams the
// archive and pulls surfaces use. The listener itself (its own address, one
// Host) is started by serve.go; this file only builds the handler, and every
// seam it supplies fails closed:
//
//   - publication state is the per-repo [static_site] document read through the
//     WAL settings projection (an unreadable document means NOT published);
//   - visibility is an anonymous CheckRead (a private or unknown repo is 404);
//   - bytes come from the synced serving copy through pullsDirs + the package's
//     own subprocess pool, never the control-plane pool (13 §7).
//
// Nil seams never panic: the constructor returns nil and serve.go skips wiring
// (a restricted boot has no store and no identity, so there is nothing to
// serve).
package main

import (
	"context"
	"log/slog"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/staticpages"
	"github.com/crueber/spindle/internal/wal"
)

// newStaticPagesHandler builds the static-site handler over composition seams.
// It returns nil when a required dependency is missing, so the caller never
// starts a listener that cannot serve anything.
func newStaticPagesHandler(cfg *config.Config, ident *identity.Service, reg *wal.Registry, apiEnv *api.Env, log *slog.Logger) *staticpages.Handler {
	if cfg == nil || ident == nil || reg == nil || apiEnv == nil || apiEnv.Repo == nil {
		return nil
	}
	return staticpages.New(staticpages.Options{
		Host:         cfg.StaticSites.Host,
		MaxFileBytes: int64(cfg.StaticSites.MaxFileBytes),
		IndexFiles:   cfg.StaticSites.IndexFiles,
		// The serving copy: the same adapter the archive service uses, so a
		// request syncs to serve level and releases the read guard before any
		// subprocess starts.
		Dirs: &pullsDirs{reg: reg},
		// One runner per process: the package's bounded pool is process-wide,
		// never shared with control-plane git traffic (19 §8).
		Git:      staticpages.NewSubprocessGit(cfg.Git.Binary),
		Settings: staticSiteSettings{view: apiEnv.Repo},
		Visible:  staticSiteVisible{ident: ident},
		// The instance's configured logger: a failed sync, git run or blob read
		// is logged once here, never on the happy path (19 §7).
		Logger: log,
	})
}

// staticSiteSettings adapts the per-repo settings document to the handler's
// Settings seam. It fails closed: a missing view, a read error or an
// unparseable/invalid document all resolve to "not published" (zero value), so
// the handler answers 404 and existence is never leaked (19 §5).
type staticSiteSettings struct{ view api.RepoView }

// Site implements staticpages.Settings.
func (s staticSiteSettings) Site(ctx context.Context, owner, repo string) (staticpages.SiteSettings, error) {
	if s.view == nil {
		return staticpages.SiteSettings{}, nil
	}
	doc, err := s.view.Settings(ctx, git.RepoId{Owner: owner, Name: repo})
	if err != nil {
		return staticpages.SiteSettings{}, nil
	}
	resolved := config.StaticSiteOf([]byte(doc.TOML))
	return staticpages.SiteSettings{
		Enabled: resolved.Enabled,
		Ref:     resolved.Ref,
		Root:    resolved.Root,
	}, nil
}

// staticSiteVisible adapts identity's require_read gate to the handler's
// Visible seam. The listener is unauthenticated by design: it asks exactly the
// anonymous question, and any error (or a nil service) reads as not visible —
// private and unknown repositories are indistinguishable 404s (19 §2).
type staticSiteVisible struct{ ident *identity.Service }

// CanRead implements staticpages.Visible.
func (v staticSiteVisible) CanRead(ctx context.Context, owner, repo string) (bool, error) {
	if v.ident == nil {
		return false, nil
	}
	if err := v.ident.CheckRead(ctx, owner, repo, auth.Anonymous()); err != nil {
		return false, nil
	}
	return true, nil
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the staticpages package defines (core never imports the command).
var (
	_ staticpages.RepoDirs  = (*pullsDirs)(nil)
	_ staticpages.GitRunner = (*staticpages.SubprocessGit)(nil)
	_ staticpages.Settings  = staticSiteSettings{}
	_ staticpages.Visible   = staticSiteVisible{}
)
