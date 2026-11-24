package e2e

// oidp.go — the in-test OIDC provider (docs/PLAN-OIDC-ONLY.md §6.2).
//
// This file exists ONLY in the e2e test binary (package e2e, *_test files are
// not part of the release build): it is an httptest server serving a real
// discovery document, a real JWKS, an auto-approving authorize endpoint and a
// token endpoint that mints RS256 ID tokens. The spindle subprocess under test
// points server.auth.issuer at this listener, so every credential it accepts
// runs the PRODUCTION verification path: signature over the real JWKS, issuer,
// audience, expiry, nonce, email_verified, and the configured group claim.
//
// There is no injected principal and no bypass: delete this file and the e2e
// suite stops booting, but the released binary never could use it.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// oidp is one test identity provider.
type oidp struct {
	t       *testing.T
	srv     *httptest.Server
	key     *rsa.PrivateKey
	kid     string
	client  string
	secret  string
	mu      sync.Mutex
	codes   map[string]oidpCode // authorize code → pending login
	nextID  int
	issuer  string
	jwksURI string
	// noKeys serves an empty JWK set (the "discovery advertises an endpoint
	// that cannot verify anything" failure the Test button must name).
	noKeys bool
}

// oidpCode is one in-flight authorization code: the claims the token endpoint
// mints when the client exchanges it.
type oidpCode struct {
	email  string
	groups []string
	nonce  string
	aud    string
}

// newOIDP starts a provider on a loopback listener and registers cleanup.
func newOIDP(t *testing.T) *oidp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("oidp: generate RSA key: %v", err)
	}
	p := &oidp{
		t:      t,
		key:    key,
		kid:    "e2e-key-1",
		client: "spindle-e2e",
		secret: "e2e-client-secret",
		codes:  map[string]oidpCode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	p.issuer = p.srv.URL
	p.jwksURI = p.srv.URL + "/jwks"
	return p
}

func (p *oidp) discovery(w http.ResponseWriter, r *http.Request) {
	writeOIDPJSON(w, map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.issuer + "/authorize",
		"token_endpoint":                        p.issuer + "/token",
		"jwks_uri":                              p.jwksURI,
		"scopes_supported":                      []string{"openid", "email"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	})
}

func (p *oidp) jwks(w http.ResponseWriter, r *http.Request) {
	if p.noKeys {
		writeOIDPJSON(w, map[string]any{"keys": []any{}})
		return
	}
	pub := p.key.PublicKey
	writeOIDPJSON(w, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": p.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize auto-approves every login. ?login_hint (or ?email) picks the email
// and ?groups is a comma-separated group list (the test's control surface).
// Without email it serves a tiny form (the browser-login drive can use either).
func (p *oidp) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_ = r.ParseForm()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		redirect = r.FormValue("redirect_uri")
	}
	state := q.Get("state")
	if state == "" {
		state = r.FormValue("state")
	}
	if redirect == "" {
		http.Error(w, "redirect_uri required", http.StatusBadRequest)
		return
	}
	if q.Get("email") == "" && q.Get("login_hint") == "" && r.Method == http.MethodGet {
		// A real minimal authorize page: the browser drive fills it and posts.
		fmt.Fprintf(w, `<!doctype html><html><body><h1>oidp</h1>
<form method="post" action="/authorize">
<input type="hidden" name="redirect_uri" value="%s">
<input type="hidden" name="state" value="%s">
<input type="hidden" name="nonce" value="%s">
<input type="hidden" name="client_id" value="%s">
<label>Email <input name="email" value="alice@example.com"></label>
<label>Groups <input name="groups" value="spindle-admin"></label>
<button type="submit">Approve</button>
</form></body></html>`,
			htmlEscape(redirect), htmlEscape(state), htmlEscape(q.Get("nonce")), htmlEscape(q.Get("client_id")))
		return
	}
	email := r.FormValue("email")
	if email == "" {
		email = r.FormValue("login_hint")
	}
	if email == "" {
		email = "alice@example.com"
	}
	groups := splitGroups(r.FormValue("groups"))
	nonce := r.FormValue("nonce")
	if nonce == "" {
		nonce = q.Get("nonce")
	}
	clientID := r.FormValue("client_id")
	if clientID == "" {
		clientID = q.Get("client_id")
	}
	p.mu.Lock()
	p.nextID++
	code := fmt.Sprintf("code-%d", p.nextID)
	p.codes[code] = oidpCode{email: email, groups: groups, nonce: nonce, aud: clientID}
	p.mu.Unlock()
	sep := "?"
	if strings.Contains(redirect, "?") {
		sep = "&"
	}
	http.Redirect(w, r, redirect+sep+url.Values{"code": {code}, "state": {state}}.Encode(), http.StatusFound)
}

// token exchanges a code for a signed RS256 ID token (client_secret_post and
// client_secret_basic both accepted; PKCE is not re-verified — the signature
// and nonce are what the server under test checks).
func (p *oidp) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.FormValue("code")
	p.mu.Lock()
	c, ok := p.codes[code]
	if ok {
		delete(p.codes, code)
	}
	p.mu.Unlock()
	if !ok {
		writeOIDPJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	aud := c.aud
	if aud == "" {
		aud = p.client
	}
	raw := p.mint(c.email, c.groups, aud, c.nonce, time.Now().Add(time.Hour))
	writeOIDPJSON(w, map[string]any{
		"access_token": "oidp-access-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     raw,
	})
}

// idToken mints a DIRECT bearer ID token (the oidc tree's bearer branch) for
// email with the given verified groups.
func (p *oidp) idToken(email string, groups ...string) string {
	return p.mint(email, groups, p.client, "", time.Now().Add(time.Hour))
}

func (p *oidp) mint(email string, groups []string, aud, nonce string, exp time.Time) string {
	now := time.Now()
	payload := map[string]any{
		"iss":            p.issuer,
		"sub":            email,
		"aud":            aud,
		"exp":            exp.Unix(),
		"iat":            now.Unix(),
		"nbf":            now.Unix(),
		"email":          email,
		"email_verified": true,
		"groups":         groups,
	}
	if nonce != "" {
		payload["nonce"] = nonce
	}
	hdr := map[string]any{"alg": "RS256", "typ": "JWT", "kid": p.kid}
	hb, _ := json.Marshal(hdr)
	pb, _ := json.Marshal(payload)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, 5 /* crypto.SHA256 */, sum[:])
	if err != nil {
		p.t.Fatalf("oidp: sign ID token: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// configBlock is the [server.auth] TOML for a spindle pointed at this IdP.
// allowedDomains admits example.com; adminGroups is the given group.
func (p *oidp) configBlock(adminGroups ...string) string {
	if len(adminGroups) == 0 {
		adminGroups = []string{"spindle-admin"}
	}
	quoted := make([]string, len(adminGroups))
	for i, g := range adminGroups {
		quoted[i] = fmt.Sprintf("%q", g)
	}
	return fmt.Sprintf(`[server.auth]
issuer = %q
oauth_client_id = %q
oauth_client_secret = %q
session_secret = "e2e-session-secret-0123456789abcdef"
allowed_domains = ["example.com"]
admin_groups = [%s]
audiences = [%q]
`, p.issuer, p.client, p.secret, strings.Join(quoted, ", "), p.client)
}

func splitGroups(raw string) []string {
	var out []string
	for _, g := range strings.Split(raw, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func writeOIDPJSON(w http.ResponseWriter, v any) { writeOIDPJSONStatus(w, http.StatusOK, v) }

func writeOIDPJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// writeE2EConfig writes a complete, normally-booting spindle.toml pointed at
// the test IdP, and points the git client default bearer at alice (a member of
// the admin group, so auto-create and pushes work).
func writeE2EConfig(t *testing.T, dataDir string, p *oidp) {
	t.Helper()
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, p.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	e2eDefaultBearer = p.idToken("alice@example.com", "spindle-admin")
}
