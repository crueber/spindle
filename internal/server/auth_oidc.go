package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/httpsec"
	"github.com/crueber/spindle/internal/server/auth"
)

// statePayload is the HMAC-signed anti-forgery state: "{now+600}\n{nonce}\n{next}".
type statePayload struct {
	ExpiresAt time.Time
	Nonce     string
	Next      string
}

// signState produces base64url(payload) + "." + base64url(mac) with the
// session secret and a 600 s window (the state HMAC is the anti-forgery;
// the carried nonce binds the browser login to its ID token — F1).
func (s *Server) signState(next string, now time.Time) string {
	state, _ := s.signStateTTL(next, now, 600*time.Second)
	return state
}

// signStateTTL signs "{now+ttl}\n{nonce}\n{next}" and also returns the carried
// nonce so the login flow can bind it into the OIDC request (F1) and the
// claimed-ticket hop can use its own 60 s window (F8).
func (s *Server) signStateTTL(next string, now time.Time, ttl time.Duration) (string, string) {
	nonce := randHex(16)
	payload := fmt.Sprintf("%d\n%s\n%s", now.Add(ttl).Unix(), nonce, next)
	mac := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte(payload))
	return b64url([]byte(payload)) + "." + b64url(mac), nonce
}

// parseState verifies the HMAC and the embedded window; returns the carried
// nonce and the raw next slot.
func (s *Server) parseState(state string) (nonce, next string, ok bool) {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return "", "", false
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return "", "", false
	}
	mac, err := b64urlDecode(parts[1])
	if err != nil {
		return "", "", false
	}
	want := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), payload)
	if !hmac.Equal(mac, want) {
		return "", "", false
	}
	lines := strings.SplitN(string(payload), "\n", 3)
	if len(lines) != 3 {
		return "", "", false
	}
	exp, err1 := parseUnix(lines[0])
	if err1 != nil || s.Now().After(exp) {
		return "", "", false
	}
	return lines[1], lines[2], true
}

// verifyState checks the HMAC and the 600 s window; returns the sanitized next.
func (s *Server) verifyState(state string) (string, bool) {
	_, next, ok := s.parseState(state)
	if !ok {
		return "", false
	}
	return sanitizeNext(next), true
}

// verifyLoginState checks the state like verifyState and additionally returns
// the carried nonce so the callback can bind the ID token to this login (F1).
func (s *Server) verifyLoginState(state string) (next, nonce string, ok bool) {
	nonce, raw, ok := s.parseState(state)
	if !ok {
		return "", "", false
	}
	return sanitizeNext(raw), nonce, true
}

// sanitizeNext runs the shared redirect-target rule (httpsec.SafeNext): a
// rooted path on this host only, so "//host", "/\host", schemes and control
// characters fall back to "/".
func sanitizeNext(next string) string {
	return httpsec.SafeNext(next, "/")
}

// authLogin answers GET /_auth/login?next= (§8.6): fetch discovery → signed
// state → 302 to the issuer's authorization endpoint with response_type=code,
// scope=openid email, prompt=select_account, &hd= = first allowed domain, the
// state-carried nonce (verified against the ID token at the callback — F1),
// and PKCE S256 (the verifier rides a short-lived HttpOnly cookie — F1).
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		// #344 / F1: the 501 string must not be the end-user experience — a
		// browser gets a rendered login page that is HONEST for the mode
		// (token mode offers the token-paste form or the file/API recovery;
		// it never blames the OIDC trio). API clients keep the plain status.
		if browserLooks(r) {
			next := sanitizeNext(r.URL.Query().Get("next"))
			mode := s.cfg.Server.Auth.Mode
			status := http.StatusNotImplemented
			if mode == "token" && s.authSvc.TokenSignInEnabled() {
				status = http.StatusOK // the token-paste form IS the login page
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(loginPageHTML(mode, s.authSvc.TokenSignInEnabled(), next)))
			return
		}
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	next := sanitizeNext(r.URL.Query().Get("next"))
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	state, nonce := s.signStateTTL(next, s.Now(), 600*time.Second)
	verifier := randHex(32)
	chalSum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_pkce", Value: verifier, Path: "/_auth/callback",
		MaxAge: 600, HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	redirect := s.authRedirectURI(r)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("scope", "openid email")
	q.Set("prompt", "select_account")
	q.Set("nonce", nonce)
	q.Set("code_challenge", b64url(chalSum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("client_id", s.cfg.Server.Auth.OAuthClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	if len(s.cfg.Server.Auth.AllowedDomains) > 0 {
		q.Set("hd", s.cfg.Server.Auth.AllowedDomains[0])
	}
	target := disc.AuthEndpoint + "?" + q.Encode()
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// tokenSignInFormHTML is the token-paste form shared by the login page and the
// rejected-credential re-render. esc is an already-HTML-escaped next path.
func tokenSignInFormHTML(esc string) string {
	return `<form method="post" action="/_auth/token">
<input type="hidden" name="next" value="` + esc + `">
<p><label>Admin token <input type="password" name="token" autocomplete="off" required autofocus></label></p>
<p><button type="submit">Sign in</button></p>
</form>`
}

// authTokenSignIn answers POST /_auth/token (F1): the token-mode counterpart of
// the OIDC login flow. It validates the pasted value against the same static
// token map the credential chain uses, requires an admin principal (the setup
// page is the destination), mints a normal walgit_session with session_secret,
// and redirects to next. No new credential type and no new secret: the session
// is the existing §8.5 wire, and the cookie authenticates through authToken's
// session branch.
//
// Same-origin: a browser form POST carries Origin/Sec-Fetch-Site. Cross-site
// values are refused before the token is read (CSRF: minting a cookie for an
// attacker's chosen token is not useful, but refusing keeps the surface
// uniform with POST /_auth/tokens).
func (s *Server) authTokenSignIn(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Server.Auth.Mode != "token" {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !sameOriginHost(o, r.Host) && !s.isCanonicalOrigin(o) {
		plainStatus(w, http.StatusForbidden, "cross-origin sign-in refused")
		return
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		plainStatus(w, http.StatusForbidden, "cross-site sign-in refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		plainStatus(w, http.StatusBadRequest, "unreadable form")
		return
	}
	next := sanitizeNext(r.PostFormValue("next"))
	esc := html.EscapeString(next)
	if !s.authSvc.TokenSignInEnabled() {
		plainStatus(w, http.StatusNotImplemented,
			"browser sign-in requires server.auth.session_secret (set it and restart, or use the JSON API with an admin token)")
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("token"))
	if raw == "" {
		writeTokenSignInError(w, esc, "Enter an admin static token.", http.StatusUnauthorized)
		return
	}
	p, ok := s.authSvc.StaticPrincipal(raw)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		writeTokenSignInError(w, esc,
			"That token is not in server.auth.tokens (or its token_env is unset on this host).",
			http.StatusUnauthorized)
		return
	}
	if !p.Admin {
		plainStatus(w, http.StatusForbidden, "admin access required: this token's principal is not an admin")
		return
	}
	sess, merr := s.authSvc.MintSession(p.Name)
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "session mint failed")
		return
	}
	s.setSessionCookie(w, sess)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// writeTokenSignInError re-renders the token-paste page with the reason and the
// original next target (the submitted token is never echoed back).
func writeTokenSignInError(w http.ResponseWriter, esc, msg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html><head><title>spindle — sign in</title></head><body>
<h1>Sign in with a static token</h1>
<p role="alert">` + html.EscapeString(msg) + `</p>` +
		tokenSignInFormHTML(esc) +
		`<p>Paste a <code>[[server.auth.tokens]]</code> value whose principal has
<code>admin = true</code>; or use the JSON API with an admin token
(<code>curl -H 'Authorization: Bearer &lt;token&gt;' ` + esc + `</code>).</p>
</body></html>`))
}

// loginPageHTML renders the browser login entry for the caller's auth mode
// (F1): the OIDC flow's disabled-trio explanation, the token-mode token-paste
// form (or its file/API recovery when no session secret exists), or the
// authentication-disabled notice. The next target travels only as a sanitized
// path in the query/hidden field (sanitizeNext ran at the call site), and the
// form posts back to sanitizeNext again — no open redirect.
//
// Served in two places: GET /_auth/login when browser login is disabled, and
// the gated-group 401 path for browser-ish GETs (middleware.go authFailure) so
// an unauthenticated browser never faces a bare "authentication required" with
// no path forward.
func loginPageHTML(mode string, tokenSignIn bool, next string) string {
	if next == "" {
		next = "/"
	}
	esc := html.EscapeString(next)
	switch mode {
	case "token":
		if tokenSignIn {
			return `<!doctype html><html><head><title>spindle — sign in</title></head><body>
<h1>Sign in with a static token</h1>
<p>This host authenticates with static tokens (<code>server.auth.mode = "token"</code>).
Paste an <strong>admin</strong> static token (a <code>[[server.auth.tokens]]</code> row
with <code>admin = true</code>) to start a browser session.</p>` +
				tokenSignInFormHTML(esc) +
				`<p>The session is an HMAC-signed <code>walgit_session</code> cookie
(<code>server.auth.session_secret</code>, lifetime <code>server.auth.session_ttl</code>).
It is not a new credential: removing the token from <code>server.auth.tokens</code>
invalidates the session on the next request.</p>
</body></html>`
		}
		return `<!doctype html><html><head><title>spindle — token mode</title></head><body>
<h1>Token mode: browser sign-in is not enabled</h1>
<p>This host authenticates with static tokens (<code>server.auth.mode = "token"</code>),
but <code>server.auth.session_secret</code> is missing or shorter than 32 bytes, so no
signed session can be minted.</p>
<p>Two ways forward:</p>
<ol>
<li><strong>Edit <code>spindle.toml</code></strong> on the server (set
<code>server.auth.session_secret</code> to at least 32 random bytes, e.g.
<code>openssl rand -hex 32</code>) and restart. Then this page offers a token-paste
sign-in.</li>
<li><strong>Use the JSON API</strong> with an admin token:
<code>curl -H 'Authorization: Bearer &lt;token&gt;' ` + esc + `</code>
(or <code>PUT /api/v1/setup</code> to validate and save the fix).</li>
</ol>
<p>No identity-provider configuration is involved on this host.</p>
</body></html>`
	case "oidc":
		loginURL := "/_auth/login?next=" + url.QueryEscape(next)
		return `<!doctype html><html><head><title>spindle — log in</title></head><body>
<h1>Log in with OIDC</h1>
<p>Browser login is not enabled on this instance: the server's OIDC
configuration is incomplete (it needs <code>server.auth.session_secret</code>,
<code>server.auth.oauth_client_id</code> and
<code>server.auth.oauth_client_secret</code> alongside
<code>auth.mode = "oidc"</code>). The button below starts the sign-in flow
and works as soon as an administrator completes that configuration and
restarts the server.</p>
<p><a href="` + loginURL + `"><button>Log in with OIDC</button></a></p>
<p>If you are the administrator: run <code>spindle config check</code> (it
names the missing keys) or open <code>/setup</code> — the OIDC section
flags the incomplete trio before you save.</p>
</body></html>`
	default:
		return `<!doctype html><html><head><title>spindle — sign in</title></head><body>
<h1>Authentication is disabled</h1>
<p>This instance runs with <code>server.auth.mode = "none"</code>: there is no
account to sign in to and no login flow to start. Set a real auth mode in
<code>spindle.toml</code> (or on the setup page) and restart to require
credentials.</p>
<p><a href="/setup">Open setup</a></p>
</body></html>`
	}
}

// authRedirectURI is {public_url}/_auth/callback; loopback origins use
// http(s)://localhost[:port] (§8.6 — the claimed-ticket hop then sets the
// cookie on walgit.localhost, a different cookie host).
func (s *Server) authRedirectURI(r *http.Request) string {
	base := s.baseURL(r)
	if isLoopbackHost(hostOnly(r.Host)) {
		scheme := requestScheme(r)
		_, port, _ := netSplit(r.Host)
		if port != "" {
			base = scheme + "://localhost:" + port
		} else {
			base = scheme + "://localhost"
		}
	}
	return base + "/_auth/callback"
}

type oidcDiscovery struct {
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
}

// authCallback answers GET /_auth/callback?code&state (§8.6): verify state,
// require the PKCE verifier cookie, exchange the code (one retry on transport
// errors and 5xx only), verify the ID token (aud exactly the client id, nonce
// bound to this login, then domain policy), set the session cookie, redirect
// to next.
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	next, nonce, ok := s.verifyLoginState(state)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "invalid state")
		return
	}
	if code == "" {
		plainStatus(w, http.StatusBadRequest, "missing code")
		return
	}
	// The PKCE verifier is one-time: consume and clear it before the
	// exchange so a replayed callback cannot reuse it.
	verifier := ""
	if c, cerr := r.Cookie("walgit_pkce"); cerr != nil || c.Value == "" {
		plainStatus(w, http.StatusBadRequest, "missing PKCE verifier")
		return
	} else {
		verifier = c.Value
	}
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_pkce", Value: "", Path: "/_auth/callback",
		MaxAge: -1, HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	idToken := s.exchangeCode(r.Context(), disc.TokenEndpoint, code, s.authRedirectURI(r), verifier)
	if idToken == "" {
		plainStatus(w, http.StatusServiceUnavailable, "token exchange failed")
		return
	}
	p, aerr := s.authSvc.verifyIDToken(r.Context(), idToken, true, nonce)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	// The session wire carries the verified EMAIL (existing sessions
	// stay valid; law 5) — the username resolves from it on every
	// request via principalFromEmail.
	sess, merr := s.authSvc.MintSession(emailOf(p), p.Groups...)
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "session mint failed")
		return
	}
	// Forgejo #376: enqueue a background avatar generation for users
	// without one (never blocks the login response — the hook only
	// enqueues; generation + install happen on a per-principal
	// single-flight goroutine in the identity service).
	if s.avatarHook != nil {
		s.avatarHook(p.Name, emailOf(p))
	}
	if isLoopbackHost(hostOnly(r.Host)) {
		// Loopback: bounce through /_auth/claimed with a 60 s signed ticket so
		// the cookie lands on walgit.localhost (different cookie host).
		// The ticket names the verified email (the cookie itself only
		// carries the wire, whose payload re-resolves it).
		ticket, _ := s.signStateTTL(emailOf(p)+"|"+sess.Wire, s.Now(), 60*time.Second)
		ticket = strings.ReplaceAll(ticket, "\n", "") // wire form is url-safe already
		target := requestScheme(r) + "://walgit." + hostOnlyPortSuffix(r.Host) + "/_auth/claimed?ticket=" +
			url.QueryEscape(ticket) + "&next=" + url.QueryEscape(next)
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
		return
	}
	s.setSessionCookie(w, sess)
	w.Header().Set("Location", next)
	w.WriteHeader(http.StatusFound)
}

// authClaimed answers GET /_auth/claimed?ticket= — the loopback hop that sets
// the cookie on walgit.localhost (60 s ticket).
func (s *Server) authClaimed(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	raw, ok := s.verifyStateTicket(ticket)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "invalid ticket")
		return
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		plainStatus(w, http.StatusBadRequest, "invalid ticket")
		return
	}
	sess := SessionToken{Kind: sessionKind, Email: parts[0], Wire: parts[1]}
	s.setSessionCookie(w, sess)
	next := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Location", next)
	w.WriteHeader(http.StatusFound)
}

// verifyStateTicket reuses the state HMAC for claimed tickets (60 s window
// enforced inside verifyState via the embedded expiry).
func (s *Server) verifyStateTicket(ticket string) (string, bool) {
	next, ok := s.verifyState(ticket)
	if !ok {
		return "", false
	}
	// The payload's third line is the "next" slot carrying "name|wire".
	parts := strings.Split(ticket, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return "", false
	}
	lines := strings.SplitN(string(payload), "\n", 3)
	if len(lines) != 3 {
		return "", false
	}
	return lines[2], ok && next != ""
}

func hostOnlyPortSuffix(host string) string {
	_, port, err := netSplit(host)
	if err != nil || port == "" {
		return "localhost"
	}
	return "localhost:" + port
}

// exchangeCode POSTs the code for tokens (§8.6): one retry on transport
// errors and 5xx only (4xx and malformed bodies fail fast — F6), with a 10 s
// per-attempt timeout so a hung provider cannot stall callback workers.
func (s *Server) exchangeCode(ctx context.Context, tokenEndpoint, code, redirect, verifier string) string {
	a := s.cfg.Server.Auth
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", a.OAuthClientID)
	form.Set("client_secret", a.OAuthClientSecret)
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	try := func() (string, bool) { // token, retryable
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
			strings.NewReader(form.Encode()))
		if err != nil {
			return "", false
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return "", true
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", resp.StatusCode >= 500
		}
		var out struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
			return "", false
		}
		return out.IDToken, false
	}
	tok, retryable := try()
	if tok == "" && retryable { // one retry
		tok, _ = try()
	}
	return tok
}

// authLogout clears the cookie (§8.6).
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	target := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// authTokensPage renders GET /_auth/tokens (session required; §8.6).
func (s *Server) authTokensPage(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil || p.Anonymous {
		w.Header().Set("WWW-Authenticate", s.authChallenge())
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(tokensPageHTML(s.baseURL(r), p)))
}

// tokensPageHTML is the mint page: explains the token lifecycle (rotating the
// secret revokes everything; nothing can be listed or revoked individually —
// §8.5) and mints via the POST button.
func tokensPageHTML(base string, p auth.Principal) string {
	return `<!doctype html><html><head><title>walgit tokens</title></head><body>
<h1>API token</h1>
<p>Principal: ` + p.Name + `</p>
<p>Tokens are HMAC-signed, valid until expiry, and cannot be listed or
revoked individually — rotating the session secret revokes all of them.</p>
<button onclick="mint()">Mint token</button>
<pre id="out"></pre>
<script>
async function mint(){
  const r = await fetch("` + base + `/_auth/tokens",{method:"POST",credentials:"include"});
  const j = await r.json();
  document.getElementById("out").textContent = r.ok ? j.token : ("error: "+(j.message||r.status));
}
</script></body></html>`
}

// authTokensMint answers POST /_auth/tokens: session required, same-origin
// CSRF guard (Sec-Fetch-Site must be same-origin), returns
// {token, principal, write, expires_at} (no-store) (§8.6).
func (s *Server) authTokensMint(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		plainStatus(w, http.StatusForbidden, "cross-site token mint refused")
		return
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	if p.Anonymous {
		w.Header().Set("WWW-Authenticate", s.authChallenge())
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Mint against the verified email when the principal carries one
	// (OIDC — the token wire keeps the email, the username resolves
	// from it); else the name (static-token behavior, unchanged).
	tok, merr := s.authSvc.MintToken(emailOf(p), p.Groups...)
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "token mint failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONBody(w, http.StatusOK, map[string]any{
		"token":      tok.Wire,
		"principal":  p.Name,
		"write":      p.Write,
		"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
	})
}
