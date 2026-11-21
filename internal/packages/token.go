package packages

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// Registry bearer tokens (docs/features/14 §4, path 2). The Docker client
// challenges once, fetches a short-lived token from /v2/token, and sends it
// on every request; the registry verifies it locally (HMAC, no store read,
// no lock). Tokens carry the granted scopes so a pull-only token can never
// push even if the principal has write.
//
// Wire format: wgp1.<base64url(payload)>.<base64url(hmac-sha256)>. The
// "wgp1" prefix keeps it disjoint from every core credential shape, so the
// server auth chain falls through to this handler's verifier rather than
// trying to parse it as a static token.

const tokenPrefix = "wgp1."

// tokenClaims is the HMAC payload.
type tokenClaims struct {
	Sub    string   `json:"sub"`
	Scopes []string `json:"scopes"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
}

// MintToken issues a short-lived scoped token for principal p.
func (s *Service) MintToken(p auth.Principal, scopes []string) (string, time.Time) {
	now := s.now()
	exp := now.Add(s.tokenTTL())
	claims := tokenClaims{Sub: p.Name, Scopes: scopes, Iat: now.Unix(), Exp: exp.Unix()}
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.key())
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return tokenPrefix + body + "." + sig, exp
}

// VerifyToken verifies a registry bearer token. ok=false for a malformed,
// forged, or expired token (the caller answers 401 with the challenge).
func (s *Service) VerifyToken(tok string) (*tokenClaims, bool) {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return nil, false
	}
	rest := strings.TrimPrefix(tok, tokenPrefix)
	body, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.key())
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	var c tokenClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, false
	}
	if c.Exp <= s.now().Unix() || c.Sub == "" {
		return nil, false
	}
	return &c, true
}

// scopeGrants reports whether any claim grants action on "owner/repo".
func (c *tokenClaims) scopeGrants(repo, action string) bool {
	for _, raw := range c.Scopes {
		if sc, ok := parseScope(raw); ok && sc.grants(repo, action) {
			return true
		}
	}
	return false
}

// --- challenge ----------------------------------------------------------------

// realm returns the token endpoint clients should use.
func (h *Handler) realm(r *http.Request) string {
	if h.Svc != nil && h.Svc.PublicURL != "" {
		return strings.TrimRight(h.Svc.PublicURL, "/") + "/v2/token"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v2/token"
}

// serviceName is the token `service` claim (the registry host).
func (h *Handler) serviceName(r *http.Request) string {
	if h.Svc != nil && h.Svc.ServiceName != "" {
		return h.Svc.ServiceName
	}
	return r.Host
}

// challenge sets the Docker Bearer challenge. scopes may be empty (the ping
// route names the realm and service only).
func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, scopes ...string) {
	v := `Bearer realm="` + h.realm(r) + `",service="` + h.serviceName(r) + `"`
	if len(scopes) > 0 {
		v += `,scope="` + strings.Join(scopes, " ") + `"`
	}
	w.Header().Set("WWW-Authenticate", v)
}

// pullScope/pushScope render the request's repo scope for the challenge.
func pullScope(owner, repo string) string {
	return "repository:" + owner + "/" + repo + ":pull"
}

func pushScope(owner, repo string) string {
	return "repository:" + owner + "/" + repo + ":pull,push"
}
