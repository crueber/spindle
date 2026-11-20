package server

import (
	"net/http"

	"github.com/crueber/spindle/internal/server/auth"
)

// setupAccess implements the §3.4 access rules: open while (no config file)
// OR (config invalid) OR (auth mode = none); otherwise an authenticated admin
// principal is required. Admin means Principal.Admin — the same flag every
// other host-admin surface uses — so an OIDC user in one of the configured
// server.auth.admin_groups qualifies (WP13), as does an admin_email /
// admin_domain match. There is NO env-token escape hatch: SPINDLE_SETUP_TOKEN
// has no effect.
func (s *Server) setupAccess(w http.ResponseWriter, r *http.Request) bool {
	open := s.boot.Mode == "defaults" || s.boot.InSetupOnly() ||
		s.cfg.Server.Auth.Mode == "none"
	if open {
		return true
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return false
	}
	if p.Anonymous {
		s.mapAuthStatus(w, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		return false
	}
	if aerr := requireAdmin(p); aerr != nil {
		s.mapAuthStatus(w, aerr)
		return false
	}
	return true
}
