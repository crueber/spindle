package server

import (
	"net/http"
	"os"
	"strings"
)

// setupAccess implements the §3.4 access rules: open while (no config file)
// OR (config invalid) OR (auth mode = none); otherwise an admin principal is
// required. Optional SPINDLE_SETUP_TOKEN: when set, the routes require it
// (query ?token= or Bearer) and skip the admin check.
func (s *Server) setupAccess(w http.ResponseWriter, r *http.Request) bool {
	if tok := os.Getenv("SPINDLE_SETUP_TOKEN"); tok != "" {
		q := r.URL.Query().Get("token")
		h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtleMatch(tok, q) || subtleMatch(tok, h) {
			return true
		}
		plainStatus(w, http.StatusUnauthorized, "setup token required")
		return false
	}
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
	if aerr := requireAdmin(p); aerr != nil {
		s.mapAuthStatus(w, aerr)
		return false
	}
	return true
}

func subtleMatch(want, got string) bool {
	if got == "" || len(want) != len(got) {
		return false
	}
	v := 0
	for i := range want {
		v |= int(want[i] ^ got[i])
	}
	return v == 0
}
