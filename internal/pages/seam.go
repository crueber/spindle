// Package pages is the server-rendered page seam (docs/go/12_web_ui.md §2,
// D-WEB-14): the interface internal/server mounts so a page provider can
// answer browser routes, plus the authority functions it needs.
//
// The seam is deliberately tiny and lives in its own package so the server
// does not import the page implementation and the implementation does not
// import the server.
package pages

import (
	"context"
	"net/http"

	"github.com/crueber/spindle/internal/server/auth"
)

// Auth is the authority surface a page provider may use. It is exactly what
// the API's gated handlers use, so page gating cannot drift from the API's.
type Auth struct {
	// Authenticate resolves the request principal (never nil on success).
	Authenticate func(r *http.Request) (auth.Principal, *auth.AuthError)
	// Refresh applies the sliding session lifetime, best effort.
	Refresh func(w http.ResponseWriter, r *http.Request, p auth.Principal)
	// CheckRead is the identity require_read hook (docs/features/01 §4.1):
	// nil → visibility is not wired; callers fall back to the anonymous_read
	// flag exactly like the API.
	CheckRead func(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Provider answers browser page routes — the whole page tree (D-WEB-14; the
// SPA was deleted with the migration's final step).
type Provider interface {
	// Serve answers one page request. false = not a page this provider owns
	// (nothing has been written; the caller answers the plain 404 or falls
	// through to the non-page lane it was fronting).
	Serve(w http.ResponseWriter, r *http.Request) bool

	// BindAuth hands the provider the server's authority functions. Called
	// once at mount time, before any request.
	BindAuth(fns Auth)
}
