package webui

import (
	"context"
	"html/template"
	"net/http"

	"github.com/crueber/walhub/internal/pages"
	"github.com/crueber/walhub/internal/server/auth"
)

// Server is the page provider. One instance per process; routes are registered
// by init-time group registration (see registerGroup) so migration groups stay
// in their own files.
type Server struct {
	env  Env
	auth pages.Auth
	mux  http.ServeMux

	// sentinel is the catch-all pattern's handler: it writes nothing, so
	// Serve can tell "no page owns this path" from a page that answered.
	sentinel http.HandlerFunc

	// tpl holds one parsed template set per page ("group/name").
	tpl map[string]*template.Template
}

// New builds the provider. Templates are parsed once; a template error is
// fatal at boot (a broken page must not silently 500 at request time).
func New(env Env) (*Server, error) {
	s := &Server{env: env}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.mount()
	return s, nil
}

// BindAuth implements pages.Provider.
func (s *Server) BindAuth(fns pages.Auth) { s.auth = fns }

// Serve implements pages.Provider: it answers a page it owns and reports
// false (writing nothing) for anything else, so the server can answer the
// next lane (the JSON API/docs lane, repo dispatch) or the plain 404.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.auth.Authenticate == nil {
		return false
	}
	pw := &probeWriter{ResponseWriter: w}
	s.mux.ServeHTTP(pw, r)
	return pw.wrote
}

// probeWriter records whether a handler answered. It forwards everything to
// the real writer, so streams and status codes are unaffected.
type probeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (p *probeWriter) WriteHeader(code int) {
	p.wrote = true
	p.ResponseWriter.WriteHeader(code)
}

func (p *probeWriter) Write(b []byte) (int, error) {
	p.wrote = true
	return p.ResponseWriter.Write(b)
}

func (p *probeWriter) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- authority helpers -------------------------------------------------------

// authErrorStatus maps an auth failure to the page status (401/403/503),
// mirroring the server's API mapping.
func authErrorStatus(aerr *auth.AuthError) int {
	switch aerr.Kind {
	case auth.ErrForbidden:
		return http.StatusForbidden
	case auth.ErrUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnauthorized
	}
}

// principal resolves the request identity and refreshes the session. On
// failure it renders the error page and returns ok=false.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, aerr := s.auth.Authenticate(r)
	if aerr != nil {
		s.errorPage(w, r, authErrorStatus(aerr), aerr.Why)
		return auth.Principal{}, false
	}
	if s.auth.Refresh != nil {
		s.auth.Refresh(w, r, p)
	}
	return p, true
}

// gated resolves the principal and applies the require-read rule: anonymous
// callers are refused unless anonymous_read is on. This is the same rule the
// API's AuthRead routes use.
func (s *Server) gated(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	if p.Anonymous && !s.anonymousRead() {
		s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
		return p, false
	}
	return p, true
}

func (s *Server) anonymousRead() bool {
	return s.env.Cfg == nil || s.env.Cfg.Server.Auth.AnonymousRead
}

func (s *Server) authMode() string {
	if s.env.Cfg == nil {
		return ""
	}
	return s.env.Cfg.Server.Auth.Mode
}

// repoVisible applies the repo read authority (Forgejo #345): the identity
// gate decides when wired; otherwise the anonymous_read flag does — the exact
// order the server uses for git/API reads.
func (s *Server) repoVisible(ctx context.Context, owner, repo string, p auth.Principal) bool {
	if s.auth.CheckRead != nil {
		return s.auth.CheckRead(ctx, owner, repo, p) == nil
	}
	return !p.Anonymous || s.anonymousRead()
}
