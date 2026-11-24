package server

import (
	"net/http"

	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
)

// SetUI installs the server-rendered page provider (internal/pages seam).
// Without one the page routes answer the plain 404 — there is no SPA shell.
func (s *Server) SetUI(ui pages.Provider) { s.ui = ui }

// uiOr returns a handler that lets the page provider answer first and falls
// through to the next lane (the JSON API/docs lane) when the path is not a
// page it owns.
func (s *Server) uiOr(fallback http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ui != nil && s.ui.Serve(w, r) {
			return
		}
		fallback(w, r)
	}
}

// pageOr404 answers a browser page route through the page provider
// (internal/pages seam) and is the plain §3 404 for anything that provider
// does not own — the page layer is the only UI (D-WEB-14).
func (s *Server) pageOr404(w http.ResponseWriter, r *http.Request) {
	if s.ui != nil && s.ui.Serve(w, r) {
		return
	}
	notFound(w, r)
}

// bindUI hands the server's authority functions to the page provider (mount).
func (s *Server) bindUI() {
	if s.ui == nil {
		return
	}
	s.ui.BindAuth(pages.Auth{
		Authenticate: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return s.authSvc.Authenticate(r, s.cfg)
		},
		Refresh:     s.maybeRefreshSession,
		CheckRead:   s.checkReadGate,
		SessionInfo: s.sessionInfo,
	})
}
