package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

// AuthService implements §8: credential resolution order (8.2), the per-mode
// decision trees (8.3), hand-rolled JWKS ID-token verification (8.4), and
// walgit-issued wgt_ tokens / sessions (8.5).
type AuthService struct {
	cfg *config.Auth
	now func() time.Time

	static []staticToken // resolved server.auth.tokens rows (token_env resolved at startup)
	jwks   *JWKS

	// usage records last successful static-token authentication (06 §8.2.1).
	// It is debounced, asynchronous and advisory; nil = disabled (setup-only
	// builds, harnesses measuring other subsystems). It never participates in
	// auth and is only written after a credential resolves successfully.
	usage *TokenUsageRecorder

	// ExtraCredential is the Seam 2 hook for compiled-in credential
	// shapes the core chain does not know (14_extensibility.md §14.4;
	// first consumer: internal/checks' wct_ CI tokens — see
	// docs/features/05_checks_statuses.md §3). It sees the raw bearer
	// token (or Basic password) AFTER static tokens and reports
	// (principal, nil, true) when it claims the credential,
	// (zero, authErr, true) when it claims-but-rejects it (a real
	// 401/403/503 from the hook wins — the credential is the hook's to
	// judge), or (_, _, false) to fall through to the mode's normal
	// resolution. Claimed prefixes MUST NOT overlap any core prefix
	// (notably wgt_) — composition validates this at startup and panics
	// on overlap. The hook resolves the credential SHAPE only (e.g. an
	// unprivileged ci:<id> principal); scoped capability checks stay
	// handler-side per repo, where the credential's home object lives.
	// Nil = no extra shapes (legacy behavior, unchanged).
	ExtraCredential func(token string) (auth.Principal, *auth.AuthError, bool)

	// UsernameResolver maps a verified OIDC email to its immutable
	// username (Forgejo #370): the store-backed registry consulted at
	// first login (users/<username>/user.json, CAS on creation for
	// collision-uniqueness) and on every repeat login (one alias
	// GET). Wired by composition in cmd/spindle (law 8 — the server
	// never imports the identity package); nil (notably in unit
	// tests) falls back to the pure auth.DeriveUsername base, which
	// still satisfies "no @ in the principal name". An empty return
	// also falls back to the base.
	UsernameResolver func(email string) string
	// EmailLookup maps a username back to its verified email for
	// username-carrying credentials (SSH key auth via
	// PrincipalForName). Nil or miss fails closed (ErrForbidden).
	EmailLookup func(username string) (string, bool)

	// AccountLookup reads the account's directory state (docs/PLAN-ADMIN.md
	// §3.7): the deactivate flag and the credential-revocation watermark.
	// Wired by composition to the identity service's exact-key read of
	// users/<username>/user.json; nil = no directory on this build (the
	// feature is off, and no disabled state can exist to enforce).
	// Evaluated once per authenticated OIDC-derived credential — served from
	// the identity service's bounded account-state cache (10 s TTL) on the
	// warm path, so it is not a per-request bucket round trip — and an error
	// FAILS CLOSED (ErrUnavailable), because an unreadable account state must
	// not authenticate as "not disabled".
	AccountLookup func(username string) (auth.AccountState, error)
}

// staticToken is one resolved server.auth.tokens row. The secret itself is not
// kept: a constant-time compare happens over its SHA-256 digest, so a lookup
// reveals nothing about the configured value and every row is compared on every
// attempt (no early exit on the matching one).
type staticToken struct {
	principal auth.Principal
	digest    [sha256.Size]byte
	expires   time.Time // zero = never expires
	invalid   bool      // unparseable expires_at (validation rejects it): fail closed
	name      string    // configured label ("" = unnamed)
	usageKey  string    // name or "sha256:<prefix>", keys the last-used record
}

// expired reports whether the credential may no longer authenticate. The
// boundary is inclusive: at expires_at the token is already dead.
func (t staticToken) expired(now time.Time) bool {
	if t.invalid {
		return true
	}
	return !t.expires.IsZero() && !now.Before(t.expires)
}

// NewAuthService builds the service and the resolved static-token table.
func NewAuthService(a *config.Auth, now func() time.Time) *AuthService {
	if now == nil {
		now = time.Now
	}
	s := &AuthService{cfg: a, now: now}
	for _, t := range a.Tokens {
		tok := t.Token
		if t.TokenEnv != "" {
			tok = os.Getenv(t.TokenEnv) // env overrides the literal; empty-resolved dropped
		}
		if tok == "" {
			continue
		}
		entry := staticToken{
			// Static tokens are machine credentials: never anonymous, never admin
			// (administration is the OIDC admin group and nothing else).
			principal: auth.Principal{Name: t.Principal, Write: t.Write},
			digest:    sha256.Sum256([]byte(tok)),
			name:      strings.TrimSpace(t.Name),
		}
		entry.usageKey = auth.TokenUsageIdentity(entry.name, tok)
		if exp := strings.TrimSpace(t.ExpiresAt); exp != "" {
			if parsed, err := time.Parse(time.RFC3339, exp); err == nil {
				entry.expires = parsed
			} else {
				// Validate refuses this at load; a caller that bypassed it
				// must not get a credential that outlives the broken row.
				entry.invalid = true
			}
		}
		s.static = append(s.static, entry)
	}
	s.jwks = NewJWKS(a.Issuer)
	return s
}

// lookupStatic finds a configured credential by its secret in constant time
// over every row (the digest compare never short-circuits).
func (s *AuthService) lookupStatic(raw string) (staticToken, bool) {
	sum := sha256.Sum256([]byte(raw))
	var found staticToken
	ok := false
	for i := range s.static {
		if subtle.ConstantTimeCompare(s.static[i].digest[:], sum[:]) == 1 {
			found = s.static[i]
			ok = true
		}
	}
	return found, ok
}

// authChallenge is the 401 WWW-Authenticate value for this host. Machine
// credentials (static tokens, and the ExtraCredential shapes: CI, runner and
// job tokens) are accepted over HTTP Basic — the password is the token — and
// git's credential machinery only offers its username/password prompt when the
// 401 advertises Basic. So Basic joins the challenge whenever a machine
// credential can exist on this host; a pure OIDC build advertises Bearer only.
func (s *AuthService) authChallenge() string {
	if s != nil && (len(s.static) > 0 || s.ExtraCredential != nil) {
		return `Basic realm="walgit", Bearer realm="walgit"`
	}
	return `Bearer realm="walgit"`
}

// authChallenge is the nil-safe Server wrapper (tests build Servers without
// an auth service).
func (s *Server) authChallenge() string {
	if s.authSvc == nil {
		return `Bearer realm="walgit"`
	}
	return s.authSvc.authChallenge()
}

// credKind is where the client credential came from (§8.2).
type credKind int

const (
	credNone credKind = iota
	credWalgitHeader
	credEdgeClientAuth
	credAuthorization
)

// resolveCredential applies the §8.2 order.
func resolveCredential(r *http.Request) (cred string, from credKind) {
	if v := r.Header.Get("X-Walgit-Authorization"); v != "" {
		return v, credWalgitHeader
	}
	if strings.Contains(r.Header.Get("X-Walgit-Capabilities"), "client-authorization") {
		return "", credEdgeClientAuth // the client sent none; Authorization is the edge hop's
	}
	return r.Header.Get("Authorization"), credAuthorization
}

// parseCredential splits Bearer (case-insensitive) / Basic; password = token.
func parseCredential(cred string) (token string, basic bool, err error) {
	if hasPrefixFold(strings.TrimSpace(cred), "bearer ") {
		return strings.TrimSpace(cred[len("Bearer "):]), false, nil
	}
	if hasPrefixFold(strings.TrimSpace(cred), "basic ") {
		dec, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(cred[len("Basic "):]))
		if derr != nil {
			return "", true, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed basic credentials"}
		}
		if i := strings.IndexByte(string(dec), ':'); i >= 0 {
			return string(dec[i+1:]), true, nil // user ignored (LFS conventions aside)
		}
		return string(dec), true, nil
	}
	if strings.TrimSpace(cred) == "" {
		return "", false, nil
	}
	return "", false, fmt.Errorf("unparseable credential")
}

// Authenticate resolves the request's principal (§8.3). OIDC is the only
// authentication (docs/PLAN-OIDC-ONLY.md): machine credentials resolve first,
// then a wgt_ token, then an ID token, then the session cookie, else the
// anonymous viewer.
func (s *AuthService) Authenticate(r *http.Request, c *config.Config) (auth.Principal, *auth.AuthError) {
	return s.authOIDC(r)
}

// AuthenticateForwarded resolves the request's principal per Authenticate
// (§8.3) and then applies the §8.6 broker-forwarding rule (identityForward):
// a trusted forwarding broker's X-Walgit-Principal replaces the principal
// name. This is the ONE entry point feature (Seam 1) surfaces must resolve
// through — calling Authenticate directly drops the forwarded identity and
// misattributes brokered actions to the broker (fail-closed: an
// authentication error returns before any forwarding is considered, and
// forwarding never upgrades a denied or anonymous caller). Pure function of
// the request headers and the resolved static config; no locks, no I/O, so
// concurrent handler goroutines share it freely.
func (s *AuthService) AuthenticateForwarded(r *http.Request, c *config.Config) (auth.Principal, *auth.AuthError) {
	p, aerr := s.Authenticate(r, c)
	if aerr != nil {
		return p, aerr
	}
	return s.identityForward(r, p), nil
}

// authOIDC is the §8.3 `oidc` tree.
func (s *AuthService) authOIDC(r *http.Request) (auth.Principal, *auth.AuthError) {
	cred, _ := resolveCredential(r)
	if cred != "" {
		tok, basic, perr := parseCredential(cred)
		if perr != nil {
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid credentials"}
		}
		// Static tokens work in oidc mode. An expired (or unparseable-expiry)
		// row is a real 401 — indistinguishable from an unknown token, so a
		// leaked credential learns nothing and git erases it.
		if st, ok := s.lookupStatic(tok); ok {
			if st.expired(s.now()) {
				return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid token"}
			}
			if s.usage != nil {
				s.usage.note(st.usageKey,
					auth.TokenUsageEntry{Name: st.name, Principal: st.principal.Name},
					r.RemoteAddr, r.UserAgent())
			}
			return st.principal, nil
		}
		if p, aerr, claimed := s.extra(tok); claimed {
			return p, aerr
		}
		if strings.HasPrefix(tok, "wgt_") {
			return s.wgtPrincipal(tok)
		}
		if basic {
			// Basic: static token or wgt_ only — no ID tokens over Basic.
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid token"}
		}
		// else verify as ID token via JWKS.
		return s.verifyIDToken(r.Context(), tok, false, "")
	}
	// No credential: session cookie → authenticated principal, else anonymous.
	if c, cerr := r.Cookie("walgit_session"); cerr == nil && c.Value != "" {
		st, terr := s.VerifyToken(c.Value)
		if terr == nil && st.Kind == "session" {
			// The session's mint time rides the gate: a session minted at
			// or before the account's revocation watermark is refused
			// (deactivation kills the browser login too, and reactivation
			// requires a fresh sign-in).
			p, aerr := s.principalFromEmailGroups(st.Email, st.Groups, st.IssuedAt)
			if aerr != nil {
				return auth.Principal{}, aerr
			}
			return p, nil
		}
	}
	return auth.Anonymous(), nil
}

// verifyIDToken verifies a JWT via the hand-rolled JWKS (§8.4). browserFlow
// pins the audience to exactly the oauth client id. A non-empty nonce binds
// the token to one browser login (F1 — must equal the ID-token nonce claim);
// direct bearer ID tokens pass "" and skip the check (nothing to bind to).
func (s *AuthService) verifyIDToken(ctx context.Context, raw string, browserFlow bool, nonce string) (auth.Principal, *auth.AuthError) {
	claims, err := s.jwks.Verify(ctx, raw, s.cfg, browserFlow, nonce)
	if err != nil {
		return auth.Principal{}, err
	}
	// An ID token is a fresh IdP credential, not a walgit-minted one: the
	// account's revocation watermark deliberately does not apply to it (a
	// reactivated user's next login works without a second wait), but the
	// deactivate flag does.
	return s.principalFromEmailGroups(claims.Email, claims.Groups, time.Time{})
}

// wgtPrincipal verifies a wgt_-prefixed token and derives the principal from
// the email claim (§8.3 oidc tree). Only kind-token wires authenticate here —
// a session-cookie wire with a forged prefix is rejected (F5).
func (s *AuthService) wgtPrincipal(wire string) (auth.Principal, *auth.AuthError) {
	st, aerr := s.VerifyToken(wire)
	if aerr != nil {
		return auth.Principal{}, aerr
	}
	if st.Kind != tokenKind {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "wrong token kind"}
	}
	// A wgt_ token is user-bound: its mint time rides the gate, so a token
	// minted at or before deactivation stays dead after reactivation.
	return s.principalFromEmailGroups(st.Email, st.Groups, st.IssuedAt)
}

// principalFromEmail applies the §8.4 email policy (7) and returns the
// username principal (Forgejo #370): the principal NAME is the
// immutable username (resolved via UsernameResolver when wired, else
// the pure auth.DeriveUsername base) — the email never becomes a
// principal name, owner segment, or path component. The verified email
// rides along on Principal.Email for alias matching and session/token
// mints (the wire keeps carrying the email).
func (s *AuthService) principalFromEmail(email string) (auth.Principal, *auth.AuthError) {
	return s.principalFromEmailGroups(email, nil, time.Time{})
}

// principalFromEmailGroups is principalFromEmail plus the WP13 group-admin
// rule: the verified OIDC group list (the configured groups_claim from the
// signature-checked ID token, carried across session/token wires) grants the
// global admin flag when it intersects server.auth.admin_groups
// (case-insensitive). An empty admin_groups list never grants admin, and
// group membership never upgrades Write — write stays the email policy's
// business (write_domains), admin is an independent flag per §8.1.
//
// issuedAt is the mint time of the credential being resolved (zero for a
// credential with no offline lifetime: an ID token, SSH key auth, a
// forwarded identity). It feeds the account gate below, which is the ONE
// deactivation enforcement point (docs/PLAN-ADMIN.md §3.7): every
// OIDC-derived credential resolves through this function, so a disabled
// account is refused here — before any page, API or git route sees a
// principal — rather than page by page.
func (s *AuthService) principalFromEmailGroups(email string, groups []string, issuedAt time.Time) (auth.Principal, *auth.AuthError) {
	email = strings.ToLower(strings.TrimSpace(email))
	domain := ""
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		domain = email[i+1:]
	}
	allowed := false
	for _, d := range s.cfg.AllowedDomains {
		if strings.EqualFold(domain, d) {
			allowed = true
		}
	}
	for _, e := range s.cfg.AllowedEmails {
		if strings.EqualFold(email, e) {
			allowed = true
		}
	}
	if !allowed {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden,
			Why: "email not allowed"}
	}
	write := true
	if len(s.cfg.WriteDomains) > 0 {
		write = false
		for _, d := range s.cfg.WriteDomains {
			if strings.EqualFold(domain, d) {
				write = true
			}
		}
	}
	// The ONE admin predicate: signed-in ∧ groups ∩ admin_groups ≠ ∅.
	// Nothing else grants administration — no email list, no domain list, no
	// static-token flag, no forwarded header.
	admin := false
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue // an empty claim value never matches an admin group
		}
		for _, want := range s.cfg.AdminGroups {
			if strings.EqualFold(g, strings.TrimSpace(want)) {
				admin = true
			}
		}
	}
	name := s.usernameFor(email)
	if aerr := s.accountGate(name, issuedAt); aerr != nil {
		return auth.Principal{}, aerr
	}
	return auth.Principal{Name: name, Email: email,
		Groups: groups, Write: write, Admin: admin}, nil
}

// accountGate enforces the instance directory's account state for a
// resolved username (docs/PLAN-ADMIN.md §3.7):
//
//   - a disabled account is refused with 403 and a neutral message (it never
//     names who deactivated it, or why);
//   - a credential minted at or before the account's revocation watermark is
//     refused with 401 (the wgt_ token and browser-session rule: deactivation
//     revokes them, reactivation does not restore them). The wire carries `iat`
//     at second precision, so the comparison is inclusive at second
//     granularity: a credential minted in the same second as the deactivation
//     is refused too (fail closed). The cost is that a sign-in in the very
//     second after a reactivation can still be refused once — retrying (or the
//     next page load a moment later) succeeds, which is the honest side of the
//     trade;
//   - an unreadable account state fails CLOSED with 503 — "not disabled"
//     must never be the answer to a store error;
//   - no lookup wired = no directory on this build (the feature is off; no
//     disabled state can exist to enforce).
//
// Cost: the lookup is memoized by the identity service (bounded LRU, 4096
// entries, 10 s TTL, negative entries included, errors never cached), so a
// warm signed-in request costs NO store op; a cold/expired entry is one
// exact-key GET. This instance invalidates on its own deactivate/reactivate,
// so a local transition is enforced on the next request; the TTL is the
// cross-instance bound (docs/PLAN-ADMIN.md §3.7). The anonymous path never
// calls it, and machine credentials never reach it.
func (s *AuthService) accountGate(username string, issuedAt time.Time) *auth.AuthError {
	if s.AccountLookup == nil || username == "" {
		return nil
	}
	st, err := s.AccountLookup(username)
	if err != nil {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "account state unavailable"}
	}
	if st.Disabled {
		return &auth.AuthError{Kind: auth.ErrForbidden,
			Why: "this account is deactivated; contact the instance administrator"}
	}
	if !st.TokensRevokedAt.IsZero() && !issuedAt.IsZero() &&
		!issuedAt.After(st.TokensRevokedAt) {
		return &auth.AuthError{Kind: auth.ErrUnauthorized,
			Why: "this credential was revoked; sign in again"}
	}
	return nil
}

// usernameFor resolves the immutable username for a verified email:
// the wired registry when present, else the pure derivation base.
// Never returns an @-carrying name (fail closed on the leak axis —
// the worst case is a non-uniquified base, never an exposed email).
func (s *AuthService) usernameFor(email string) string {
	if s.UsernameResolver != nil {
		if u := s.UsernameResolver(email); auth.ValidUsername(u) {
			return u
		}
	}
	return auth.DeriveUsername(email)
}

// emailOf reports the wire identity for session/token mints: the
// verified email when the principal carries one (OIDC), else the name
// (static-token/legacy behavior, unchanged).
func emailOf(p auth.Principal) string {
	if p.Email != "" {
		return p.Email
	}
	return p.Name
}

// extra consults the Seam 2 ExtraCredential hook (nil-safe): (zero,
// nil, false) when no hook claims the token.
func (s *AuthService) extra(tok string) (auth.Principal, *auth.AuthError, bool) {
	if s.ExtraCredential == nil || tok == "" {
		return auth.Principal{}, nil, false
	}
	return s.ExtraCredential(tok)
}

// requireRead/Write/Admin are the §8.3 checks.
func requireRead(p auth.Principal, anonymousRead bool) *auth.AuthError {
	if p.Anonymous && !anonymousRead {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return nil
}

func requireWrite(p auth.Principal) *auth.AuthError {
	if !p.Write {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "write access required"}
	}
	return nil
}

func requireAdmin(p auth.Principal) *auth.AuthError {
	if !p.Admin {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin access required"}
	}
	return nil
}

// BrowserLoginEnabled reports §8.6 gating: the browser-login trio. Config
// validation makes it true for every normally-booted instance.
func (s *AuthService) BrowserLoginEnabled() bool {
	return s.cfg.SessionSecret != "" &&
		s.cfg.OAuthClientID != "" && s.cfg.OAuthClientSecret != ""
}

// StaticPrincipal resolves ONE raw static token from the static credential
// chain (server.auth.tokens, token_env resolved at startup).
// An expired credential does not resolve (06 §8.2.1). The Seam 2 hook is
// deliberately not consulted: a CI/feature credential is
// not a browser identity and must never mint a session.
func (s *AuthService) StaticPrincipal(raw string) (auth.Principal, bool) {
	st, ok := s.lookupStatic(raw)
	if !ok || st.expired(s.now()) {
		return auth.Principal{}, false
	}
	return st.principal, true
}

// --- wgt_ tokens & sessions (§8.5) ---------------------------------------------------

// SessionToken is a verified walgit-issued token.
type SessionToken struct {
	Kind      string // "session" | "token"
	Email     string
	Groups    []string // verified OIDC groups captured at mint (WP13; may be nil)
	ExpiresAt time.Time
	IssuedAt  time.Time
	Wire      string // the full wire form (session tokens without prefix)
}

const (
	tokenPrefix = "wgt_"
	tokenKind   = "token"
	sessionKind = "session"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64urlDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// MintToken issues an access token ("wgt_" prefix, kind token, TTL access_token_ttl).
// The optional groups are the principal's verified OIDC groups (WP13): they
// ride the wire so a minted access token re-derives the same admin rule.
func (s *AuthService) MintToken(email string, groups ...string) (SessionToken, error) {
	return s.mint(tokenKind, email, tokenPrefix, groups)
}

// MintSession issues a session token (kind session, no prefix).
func (s *AuthService) MintSession(email string, groups ...string) (SessionToken, error) {
	return s.mint(sessionKind, email, "", groups)
}

// mint writes the §8.5 payload `kind\nexp\ni at\nemail` plus an optional fifth
// line carrying the verified group list as a JSON array (WP13; law 5 note in
// 06_server_http.md Decisions). The groups line is part of the HMAC payload, so
// a client cannot forge membership; it is decoded on every session/token
// verification and the admin_groups POLICY is re-applied per request.
func (s *AuthService) mint(kind, email, prefix string, groups []string) (SessionToken, error) {
	now := s.now()
	ttl := time.Duration(s.cfg.AccessTokenTTL)
	if kind == sessionKind {
		ttl = time.Duration(s.cfg.SessionTTL)
	}
	payload := fmt.Sprintf("%s\n%d\n%d\n%s", kind, now.Add(ttl).Unix(), now.Unix(), email)
	gj := []byte("[]")
	if len(groups) > 0 {
		gj, _ = json.Marshal(groups) // []string always marshals
	}
	payload += "\n" + string(gj)
	mac := hmacSHA256([]byte(s.cfg.SessionSecret), []byte(payload))
	wire := prefix + b64url([]byte(payload)) + "." + b64url(mac)
	return SessionToken{Kind: kind, Email: email, Groups: groups, IssuedAt: now,
		ExpiresAt: now.Add(ttl), Wire: wire}, nil
}

// VerifyToken verifies the wire format
// `wgt_? + base64url(payload) + "." + base64url(mac)` with HMAC-SHA256 (§8.5).
func (s *AuthService) VerifyToken(wire string) (SessionToken, *auth.AuthError) {
	fail := func(k auth.AuthErrorKind, why string) (SessionToken, *auth.AuthError) {
		return SessionToken{}, &auth.AuthError{Kind: k, Why: why}
	}
	if s.cfg.SessionSecret == "" {
		return fail(auth.ErrUnavailable, "no session secret configured")
	}
	body := strings.TrimPrefix(wire, tokenPrefix)
	parts := strings.Split(body, ".")
	if len(parts) != 2 {
		return fail(auth.ErrInvalid, "malformed token")
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	mac, err := b64urlDecode(parts[1])
	if err != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	want := hmacSHA256([]byte(s.cfg.SessionSecret), payload)
	if subtle.ConstantTimeCompare(mac, want) != 1 {
		return fail(auth.ErrInvalid, "invalid token")
	}
	lines := strings.SplitN(string(payload), "\n", 5)
	if len(lines) != 4 && len(lines) != 5 {
		return fail(auth.ErrInvalid, "malformed token")
	}
	kind := lines[0]
	if kind != tokenKind && kind != sessionKind {
		return fail(auth.ErrInvalid, "malformed token")
	}
	exp, err1 := parseUnix(lines[1])
	iat, err2 := parseUnix(lines[2])
	if err1 != nil || err2 != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	if s.now().After(exp) {
		return fail(auth.ErrInvalid, "token expired")
	}
	// The fifth line (WP13) is the verified group list. Four-line tokens
	// minted before groups existed stay valid with no groups.
	var groups []string
	if len(lines) == 5 {
		if g := strings.TrimSpace(lines[4]); g != "" && g != "[]" && g != "null" {
			if terr := json.Unmarshal([]byte(g), &groups); terr != nil {
				return fail(auth.ErrInvalid, "malformed token")
			}
		}
	}
	return SessionToken{Kind: kind, Email: lines[3], Groups: groups, IssuedAt: iat,
		ExpiresAt: exp, Wire: wire}, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func parseUnix(s string) (time.Time, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

// --- hand-rolled JWKS (§8.4) -----------------------------------------------------------

// jwk is one JSON Web Key.
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`

	pub   crypto.PublicKey
	ecdsa *ecdsa.PublicKey
	rsa   *rsa.PublicKey
}

// JWKS caches the issuer discovery + keys per §8.4: discovery once per
// process, keys honoring the JWKS Cache-Control max-age (default 300 s),
// stale-while-refresh on expiry, inline refresh on unknown kid (singleflight).
type JWKS struct {
	mu      sync.RWMutex
	issuer  string
	disc    oidcDiscovery
	discHas bool
	keys    map[string]*jwk
	fetched time.Time
	ttl     time.Duration
	client  *http.Client

	sfMu sync.Mutex
	sf   *jwksFlight // in-flight refresh; followers share the leader's outcome (F7)
}

func NewJWKS(issuer string) *JWKS {
	return &JWKS{issuer: issuer, keys: map[string]*jwk{}, client: &http.Client{Timeout: 10 * time.Second}}
}

// nowKey carries a test clock into the JWKS fetch path.
type nowKey struct{}

// SetNow overrides the clock (tests).
func (j *JWKS) SetNow(t time.Time) { j.mu.Lock(); j.fetched = t; j.mu.Unlock() }

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// fetch (leader) refreshes discovery + keys with a 10 s context.
func (j *JWKS) fetch(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	j.mu.RLock()
	issuer := j.issuer
	j.mu.RUnlock()
	jwksURI, err := j.discover(cctx, issuer)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	ttl := 300 * time.Second
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		if d := maxAgeOf(cc); d > 0 {
			ttl = d
		}
	}
	j.mu.Lock()
	j.keys = map[string]*jwk{}
	for i := range doc.Keys {
		k := &doc.Keys[i]
		if err := k.parse(); err == nil {
			j.keys[k.Kid] = k
		}
	}
	j.fetched = j.nowOf(cctx)
	j.ttl = ttl
	j.mu.Unlock()
	return nil
}

func (j *JWKS) nowOf(ctx context.Context) time.Time {
	if t, ok := ctx.Value(nowKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// Get returns the key for kid: stale-while-refresh on expiry, inline refresh
// on unknown kid; refresh failures → ErrUnavailable (§8.4).
func (j *JWKS) Get(ctx context.Context, kid string) (*jwk, *auth.AuthError) {
	j.mu.RLock()
	k := j.keys[kid]
	stale := j.ttl > 0 && time.Since(j.fetched) > j.ttl
	j.mu.RUnlock()
	if k != nil && !stale {
		return k, nil
	}
	if k == nil {
		// Inline refresh on unknown kid (thundering-herd safe: singleflight;
		// leader context has its own 10 s timeout).
		if err := j.refresh(ctx); err != nil {
			return nil, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "key refresh failed: " + err.Error()}
		}
		j.mu.RLock()
		k = j.keys[kid]
		j.mu.RUnlock()
		if k != nil {
			return k, nil
		}
		return nil, &auth.AuthError{Kind: auth.ErrInvalid, Why: "unknown key id"}
	}
	// Stale-while-refresh: serve stale keys while one background refresh runs.
	go func() {
		_ = j.refresh(context.Background())
	}()
	if k != nil {
		return k, nil
	}
	return nil, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "keys unavailable"}
}

// jwksFlight is one in-flight JWKS refresh: the leader fetches, followers
// wait on done and share err (F7 — never queue behind a slow issuer twice:
// a follower whose own context ends first returns its context error).
type jwksFlight struct {
	done chan struct{}
	err  error
}

// refresh runs at most one fetch at a time; followers share the outcome.
func (j *JWKS) refresh(ctx context.Context) error {
	j.sfMu.Lock()
	if f := j.sf; f != nil {
		j.sfMu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &jwksFlight{done: make(chan struct{})}
	j.sf = f
	j.sfMu.Unlock()
	// fetch detaches from caller cancellation with its own 10 s timeout, so
	// the leader always terminates and every follower is released.
	f.err = j.fetch(ctx)
	j.sfMu.Lock()
	if j.sf == f {
		j.sf = nil
	}
	j.sfMu.Unlock()
	close(f.done)
	return f.err
}

// discoverDoc returns the parsed discovery document (cached once per
// process, §8.4).
func (j *JWKS) discoverDoc(ctx context.Context) (oidcDiscovery, error) {
	j.mu.RLock()
	cached := j.disc
	have := j.discHas
	j.mu.RUnlock()
	if have {
		return cached, nil
	}
	uri := strings.TrimSuffix(j.issuer, "/") + "/.well-known/openid-configuration"
	resp, err := j.client.Get(uri)
	if err != nil {
		return oidcDiscovery{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcDiscovery{}, fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return oidcDiscovery{}, err
	}
	j.mu.Lock()
	j.disc = doc
	j.discHas = true
	j.mu.Unlock()
	return doc, nil
}

// discover caches the discovery document once per process.
func (j *JWKS) discover(ctx context.Context, issuer string) (string, error) {
	uri := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	resp, err := j.client.Get(uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery: no jwks_uri")
	}
	return doc.JWKSURI, nil
}

func maxAgeOf(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		p := strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(p), "max-age=") {
			var secs int
			if _, err := fmt.Sscanf(p, "max-age=%d", &secs); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return 0
}

// parse converts the JWK JSON into a crypto key.
func (k *jwk) parse() error {
	switch k.Kty {
	case "RSA":
		n, err := b64urlDecode(k.N)
		if err != nil {
			return err
		}
		e, err := b64urlDecode(k.E)
		if err != nil {
			return err
		}
		ei := new(big.Int).SetBytes(e).Int64()
		k.rsa = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(ei)}
		return nil
	case "EC":
		if k.Crv != "P-256" {
			return fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64urlDecode(k.X)
		if err != nil {
			return err
		}
		y, err := b64urlDecode(k.Y)
		if err != nil {
			return err
		}
		k.ecdsa = &ecdsa.PublicKey{Curve: elliptic.P256(),
			X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		return nil
	}
	return fmt.Errorf("unsupported kty %q", k.Kty)
}

// idClaims is the verified ID-token claim set.
type idClaims struct {
	Iss           string `json:"iss"`
	Sub           string `json:"sub"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Nbf           int64  `json:"nbf"`
	Iat           int64  `json:"iat"`
	Email         string `json:"email"`
	EmailVerified *bool  `json:"email_verified"`
	// Groups is the configured claim (server.auth.groups_claim) decoded from
	// the SAME signature-verified payload — never a request header. A JSON
	// array of strings yields every string element; a single string yields
	// one entry; anything else (absent, number, object) yields none.
	Groups []string `json:"-"`
}

// Verify checks alg/key/claims per §8.4 (leeway 30 s; iss with trailing slash
// stripped or its bare host; aud ∈ audiences ∪ {client_id}, pinned to the
// client id for the browser flow; email + email_verified required; a non-empty
// nonce must equal the token's nonce claim — F1 browser-login binding).
func (j *JWKS) Verify(ctx context.Context, raw string, a *config.Auth, browserFlow bool, nonce string) (idClaims, *auth.AuthError) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	hdrJSON, err := b64urlDecode(parts[0])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	// 1. alg MUST be RS256 or ES256.
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "unsupported alg"}
	}
	key, aerr := j.Get(ctx, hdr.Kid)
	if aerr != nil {
		return idClaims{}, aerr
	}
	// 2. The key's advertised alg must match the token alg.
	if key.Alg != "" && key.Alg != hdr.Alg {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key alg mismatch"}
	}
	signing := raw[:len(raw)-len(parts[2])-1]
	sig, err := b64urlDecode(parts[2])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed signature"}
	}
	sum := sha256.Sum256([]byte(signing))
	switch hdr.Alg {
	case "RS256":
		if key.rsa == nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key type mismatch"}
		}
		if err := rsa.VerifyPKCS1v15(key.rsa, crypto.SHA256, sum[:], sig); err != nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
	case "ES256":
		if key.ecdsa == nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key type mismatch"}
		}
		if len(sig) != 64 {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
		r := new(big.Int).SetBytes(sig[:32])
		sv := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(key.ecdsa, sum[:], r, sv) {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
	}
	payload, err := b64urlDecode(parts[1])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed claims"}
	}
	var c struct {
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Exp           int64           `json:"exp"`
		Nbf           int64           `json:"nbf"`
		Iat           int64           `json:"iat"`
		Email         string          `json:"email"`
		EmailVerified *bool           `json:"email_verified"`
		Nonce         string          `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed claims"}
	}
	var auds []string
	if len(c.Aud) > 0 && c.Aud[0] == '"' {
		var one string
		if err := json.Unmarshal(c.Aud, &one); err == nil {
			auds = []string{one}
		}
	} else if len(c.Aud) > 0 {
		_ = json.Unmarshal(c.Aud, &auds)
	}
	now := j.nowOf(ctx)
	const leeway = 30 * time.Second // 3
	if c.Exp != 0 && now.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token expired"}
	}
	if c.Nbf != 0 && now.Add(leeway).Before(time.Unix(c.Nbf, 0)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token not yet valid"}
	}
	if c.Iat != 0 && now.Add(leeway).Before(time.Unix(c.Iat, 0)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token issued in the future"}
	}
	// 4. iss equals the configured issuer (trailing slash stripped) or its bare host.
	issWant := strings.TrimSuffix(a.Issuer, "/")
	issGot := strings.TrimSuffix(c.Iss, "/")
	if issGot != issWant && issGot != bareHost(issWant) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "issuer mismatch"}
	}
	// 5. aud.
	audOK := containsString(auds, a.OAuthClientID)
	if !browserFlow {
		audOK = audOK || anyOf(auds, a.Audiences)
	}
	if !audOK {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "audience mismatch"}
	}
	// 6. email + email_verified required.
	if c.Email == "" || c.EmailVerified == nil || !*c.EmailVerified {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "email claim missing or unverified"}
	}
	// 7. nonce binding (F1): a login-bound verification requires the token
	// to carry the state nonce back. Empty expected nonce skips the check
	// (direct bearer ID tokens have no login to bind to).
	if nonce != "" && c.Nonce != nonce {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "nonce mismatch"}
	}
	// WP13: extract the configured group claim from the verified payload.
	// A second unmarshal into raw fields is what lets the claim NAME stay
	// configurable (the typed struct above has fixed keys); the signature
	// already passed, so these bytes are trustworthy.
	var rawClaims map[string]json.RawMessage
	_ = json.Unmarshal(payload, &rawClaims)
	claims := idClaims{Iss: c.Iss, Email: c.Email, Exp: c.Exp, Nbf: c.Nbf, Iat: c.Iat,
		Groups: groupsFromClaim(rawClaims, a.GroupsClaim)}
	return claims, nil
}

// groupsFromClaim decodes the configured group claim: a JSON array of strings
// (non-string elements skipped) or a single string. Absent/unknown/empty →
// nil, never an error — a missing claim simply grants no group-derived admin.
func groupsFromClaim(raw map[string]json.RawMessage, claim string) []string {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		return nil
	}
	v, ok := raw[claim]
	if !ok {
		return nil
	}
	s := strings.TrimSpace(string(v))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var one string
		if json.Unmarshal(v, &one) != nil {
			return nil
		}
		one = strings.TrimSpace(one)
		if one == "" {
			return nil
		}
		return []string{one}
	}
	if s[0] != '[' {
		return nil
	}
	var items []any
	if json.Unmarshal(v, &items) != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if g, ok := it.(string); ok {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func anyOf(list, allowed []string) bool {
	for _, x := range list {
		if containsString(allowed, x) {
			return true
		}
	}
	return false
}

func bareHost(iss string) string {
	s := strings.TrimSuffix(iss, "/")
	if i := strings.Index(s, "://"); i >= 0 {
		return s[i+3:]
	}
	return s
}

// --- /_auth endpoints shared with the OIDC flow ---------------------------------------

// authMe answers GET /_auth/me → {principal, write, anonymous, email, groups,
// admin, groups_claim} (§8.6). The group fields are the live verified claims
// beside the configured claim name, so a "valid config but wrong group"
// lockout is diagnosable with one curl.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, r, aerr)
		return
	}
	groups := p.Groups
	if groups == nil {
		groups = []string{}
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"principal": p.Name, "write": p.Write, "anonymous": p.Anonymous,
		"email": p.Email, "groups": groups, "admin": p.Admin,
		"groups_claim": s.cfg.Server.Auth.GroupsClaim,
	})
}

// sessionInfo reports the walgit_session cookie's issued-at for the Access
// diagnostics (pages.Auth.SessionInfo). ok=false for bearer/static/anonymous
// requests, so the card simply omits the line.
func (s *Server) sessionInfo(r *http.Request) (time.Time, bool) {
	if s.authSvc == nil {
		return time.Time{}, false
	}
	c, err := r.Cookie("walgit_session")
	if err != nil || c.Value == "" {
		return time.Time{}, false
	}
	st, aerr := s.authSvc.VerifyToken(c.Value)
	if aerr != nil || st.Kind != sessionKind {
		return time.Time{}, false
	}
	return st.IssuedAt.UTC(), true
}

// checkHandler answers /_auth/check (the edge's auth_request): 204 +
// X-Walgit-Principal + X-Walgit-Write + Cache-Control: private, max-age=300.
func (s *Server) authCheck(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, r, aerr)
		return
	}
	if p.Anonymous && !s.cfg.Server.Auth.AnonymousRead {
		w.Header().Set("WWW-Authenticate", s.authChallenge())
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	h := w.Header()
	h.Set("X-Walgit-Principal", p.Name)
	if p.Write {
		h.Set("X-Walgit-Write", "1")
	} else {
		h.Set("X-Walgit-Write", "0")
	}
	h.Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusNoContent)
}

// identityForward resolves §8.6 identity forwarding (push broker): the hop's
// X-Walgit-Principal is honored only when the caller has write AND its name is
// in server.auth.trusted_forwarders; admin re-derived from policy.
func (s *AuthService) identityForward(r *http.Request, caller auth.Principal) auth.Principal {
	name := r.Header.Get("X-Walgit-Principal")
	if name == "" || !caller.Write || !containsString(s.cfg.TrustedForwarders, caller.Name) {
		return caller
	}
	p := caller
	p.Name = name // forwarded name replaces the principal, keeps write
	if em := principalEmail(name); em != "" {
		if fp, aerr := s.principalFromEmail(em); aerr == nil {
			p.Name = fp.Name
			p.Email = fp.Email
			p.Admin = fp.Admin
			p.Write = fp.Write || p.Write
		}
	}
	// A forwarded username (no @) carries no email to re-derive policy
	// from; it keeps the caller's flags (fail closed — forwarding never
	// upgrades, and the name is already leak-safe).
	return p
}

func principalEmail(name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

// randHex is a helper for nonces/tickets.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b64url(b)
}

// PrincipalForName resolves a named principal's rights for SSH key auth
// (17_ssh.md §3). There is one identity source: the OIDC admission path. A
// username (no @) resolves through the wired EmailLookup back to the verified
// email and inherits that email's admission + flags (fail closed — nil lookup
// or miss denies). A static token's principal name is not an OIDC identity, so
// its SSH keys resolve nowhere.
func (a *AuthService) PrincipalForName(name string) (auth.Principal, error) {
	if strings.Contains(name, "@") {
		p, aerr := a.principalFromEmail(name)
		if aerr != nil {
			return auth.Principal{}, aerr
		}
		return p, nil
	}
	if a.EmailLookup == nil {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden,
			Why: "unknown principal"}
	}
	em, ok := a.EmailLookup(strings.ToLower(strings.TrimSpace(name)))
	if !ok {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden,
			Why: "unknown principal"}
	}
	p, aerr := a.principalFromEmail(em)
	if aerr != nil {
		return auth.Principal{}, aerr
	}
	return p, nil
}
