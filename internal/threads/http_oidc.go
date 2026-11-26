package threads

import (
	"net/http"
	"strings"
)

// The OIDC surface (docs/PLAN-THREADS-OIDC.md):
//
//	GET  /.well-known/openid-configuration                                    discovery (open)
//	GET  /.well-known/jwks.json                                               public keys (open)
//	POST /api/v1/threads/jobs/{ref}/attempt/{n}/id-token                      exchange (request token)
//
// The two discovery routes are public by construction: a provider must be able
// to fetch them without a spindle credential. The exchange rides the runner
// protocol's path family (/api/v1/threads/jobs/…), which the server's
// anonymous-write assert already exempts and whose handlers authorise
// themselves — here with the job's opaque request token, never a session.

// handleWellKnown answers the discovery and JWKS documents. It reports false
// for any other /.well-known path so the core mux keeps the plain 404.
func (h *Handler) handleWellKnown(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) != 2 {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return true
	}
	if h.Svc == nil || h.Svc.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "threads service not wired")
		return true
	}
	switch segs[1] {
	case "openid-configuration":
		h.serveDiscovery(w, r)
		return true
	case "jwks.json":
		h.serveJWKS(w, r)
		return true
	}
	return false
}

// issuerBase is the token issuer: the configured server.public_url when set
// (the same declaration that names the container-reachable checkout origin),
// otherwise the request's origin. The fallback exists for development; a
// deployment behind a proxy must set server.public_url or the issuer changes
// with the Host header and providers will reject the tokens.
func (h *Handler) issuerBase(r *http.Request) string {
	return strings.TrimRight(h.checkoutBase(r), "/")
}

// serveDiscovery renders the discovery document.
func (h *Handler) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	iss := h.issuerBase(r)
	if iss == "" {
		writePlain(w, http.StatusServiceUnavailable, "no public origin configured for the OIDC issuer")
		return
	}
	doc := oidcDiscovery{
		Issuer:                           iss,
		JWKSURI:                          iss + "/.well-known/jwks.json",
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
		ResponseTypesSupported:           []string{"id_token"},
		ScopesSupported:                  []string{"openid"},
		ClaimsSupported: []string{
			"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "repository",
			"repository_owner", "ref", "ref_type", "ref_protected", "sha",
			"workflow", "job_workflow_ref", "job", "run_id", "run_attempt",
			"event_name", "runner_environment", "environment",
		},
	}
	// Discovery and JWKS are cacheable: keys rotate at most every 90 days, and
	// a provider refetches on an unknown kid. A minute keeps a rotation
	// visible quickly without a fetch per verification.
	writeJSONCached(w, http.StatusOK, doc, "public, max-age=60")
}

// serveJWKS renders the public half of every key in the set. The rendered set
// rides the same cache as the private key: a warm JWKS read does no store work.
func (h *Handler) serveJWKS(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Svc.oidcJWKS(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSONCached(w, http.StatusOK, map[string]any{"keys": keys}, "public, max-age=60")
}

// exchangeIDToken is POST …/attempt/{n}/id-token: the job presents its request
// token and an audience, and receives a signed short-lived ID token.
func (h *Handler) exchangeIDToken(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	if h.Svc == nil || h.Svc.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	bearer := bearerToken(r.Header.Get("Authorization"))
	if strings.HasPrefix(bearer, attemptTokenPrefix) {
		// The job token authorises the runner channel, not the exchange. Saying
		// so out loud beats a generic "unknown request token" when a workflow
		// confuses the two.
		writePlain(w, http.StatusUnauthorized, "id-token takes the job's request token (SPINDLE_ID_TOKEN_REQUEST_TOKEN), not the job token")
		return
	}
	// The audience is accepted in the body GitHub clients send and in the
	// `?audience=` query GitHub's request URL uses (PAR-120); both spellings
	// are validated identically by ExchangeIDToken. A body is optional when the
	// query names the audience.
	var in struct {
		Audience string `json:"audience"`
	}
	aud := strings.TrimSpace(r.URL.Query().Get("audience"))
	if aud == "" {
		if !decodeStrict(w, r, 4<<10, map[string]bool{"audience": true}, &in) {
			return
		}
		aud = in.Audience
	}
	iss := h.issuerBase(r)
	if iss == "" {
		writePlain(w, http.StatusServiceUnavailable, "no public origin configured for the OIDC issuer")
		return
	}
	token, err := h.Svc.ExchangeIDToken(r.Context(), ref, attempt, bearer, aud, iss)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": token})
}
