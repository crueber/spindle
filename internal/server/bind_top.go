package server

import "net/http"

// bind_top.go — the top-level, non-repo feature seam (docs/features/14 §3).
// A feature whose routes live in their own namespace (/v2/…) cannot ride the
// api seam (which owns /api* and repo lanes) or the repo-subpath seam (which
// needs an owner/repo prefix). ChainTop registers such a surface; the router
// hands it the namespace's requests before the catch-all wildcard, and the
// first match wins. Implementations authenticate themselves (the api seam's
// principal injection does not apply here — exactly like RepoRoutes).

// ChainTop fronts the top-level dispatch with feature namespaces (Seam 1
// registration). Nil-safe and additive: without extras nothing changes.
func (s *Server) ChainTop(extras ...ExtraRoutes) {
	s.topExtras = append(s.topExtras, extras...)
}

// topServe runs the top-level chain for one namespace root. It answers 404
// when no extra claims the path (a registered namespace is the feature's).
func (s *Server) topServe(w http.ResponseWriter, r *http.Request) {
	for _, x := range s.topExtras {
		if x.Handle(w, r) {
			return
		}
	}
	notFound(w, r)
}
