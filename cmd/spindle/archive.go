// archive.go — WP3.1 composition: the repo source-archive route
// (GET /{o}/{r}/archive/{ref}.{tar.gz,zip}) over the P6 read gate, stock git
// through the archive package's own bounded pool, and repo dirs through the
// WAL registry. Bytes are derived on read (nothing stored), so this is pure
// wiring — no task kind, no bucket key.
package main

import (
	"net/http"

	"github.com/crueber/spindle/internal/archive"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/wal"
)

// newArchiveService builds the archive Service + Handler.
func newArchiveService(ident *identity.Service, reg *wal.Registry, gitBinary string) (*archive.Service, *archive.Handler) {
	svc := archive.New(ident)
	svc.Git = archive.NewSubprocessGit(gitBinary)
	svc.Dirs = &pullsDirs{reg: reg}
	return svc, &archive.Handler{Svc: svc}
}

// chainArchive registers the route on the repo-subpath chain (outside the
// api lanes, never compressed) with the server's principal chain.
func chainArchive(srv *server.Server, h *archive.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainRepo(h)
}

var _ server.RepoRoutes = (*archive.Handler)(nil)
