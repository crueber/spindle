package server

import (
	"net/http"

	"github.com/crueber/spindle/internal/server/auth"
)

// setupAccess implements the §3.4 access rules: open ONLY in the restricted
// boot (no config / invalid config); otherwise an authenticated admin
// principal is required. Admin means Principal.Admin — the ONE admin predicate
// (signed in ∧ groups ∩ admin_groups ≠ ∅, derived at the credential boundary)
// that every host-admin surface uses. There is NO env-token escape hatch:
// SPINDLE_SETUP_TOKEN has no effect.
func (s *Server) setupAccess(w http.ResponseWriter, r *http.Request) bool {
	if s.boot.InSetupOnly() {
		return true
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, r, aerr)
		return false
	}
	if p.Anonymous {
		s.mapAuthStatus(w, r, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		return false
	}
	if aerr := requireAdmin(p); aerr != nil {
		s.mapAuthStatus(w, r, aerr)
		return false
	}
	return true
}
