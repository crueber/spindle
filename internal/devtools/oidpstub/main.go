// Command oidpstub is a local, stdlib-only OIDC provider for development
// (docs/PLAN-OIDC-ONLY.md §6.3). It serves a real discovery document, a real
// JWKS, an auto-approving authorize endpoint (with an optional small form) and
// a token endpoint that mints RS256 ID tokens.
//
// It is DEV TOOLING, not product: `internal/devtools` is not in the release
// binary's import graph (cmd/spindle never imports it), the Dockerfile does
// not ship it, and this file's -help says so. Point a local spindle at it with
// the [server.auth] block it prints on startup.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	clientID     = "spindle-dev"
	clientSecret = "spindle-dev-secret"
	sessionKey   = "dev-session-secret-0123456789abcdef"
)

type pendingLogin struct {
	email  string
	groups []string
	nonce  string
	aud    string
}

type stub struct {
	issuer string
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]pendingLogin
	next   int
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address")
	issuer := flag.String("issuer", "", "issuer URL (default http://<addr>)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `oidpstub — a local OIDC provider for spindle development.

DEV TOOLING ONLY: not part of the spindle release binary or image.

Usage: go run ./internal/devtools/oidpstub [-addr 127.0.0.1:9999] [-issuer URL]

Point spindle at it, then sign in with any email and the groups you type.
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	iss := *issuer
	if iss == "" {
		iss = "http://" + *addr
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	s := &stub{issuer: strings.TrimRight(iss, "/"), key: key, codes: map[string]pendingLogin{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/jwks", s.jwks)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "oidpstub: see /.well-known/openid-configuration")
	})

	fmt.Printf(`oidpstub listening on %s (issuer %s)

Add this to spindle.toml (or save it on /admin/config):

[server.auth]
issuer = %q
oauth_client_id = %q
oauth_client_secret = %q
session_secret = %q
allowed_domains = ["example.com"]
admin_groups = ["spindle-admin"]
audiences = [%q]

Register the redirect URI http://localhost:8080/_auth/callback at this stub.
`, *addr, s.issuer, s.issuer, clientID, clientSecret, sessionKey, clientID)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *stub) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/authorize",
		"token_endpoint":                        s.issuer + "/token",
		"jwks_uri":                              s.issuer + "/jwks",
		"scopes_supported":                      []string{"openid", "email"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (s *stub) jwks(w http.ResponseWriter, r *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "dev-1",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (s *stub) authorize(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := r.URL.Query()
	get := func(k string) string {
		if v := r.FormValue(k); v != "" {
			return v
		}
		return q.Get(k)
	}
	redirect := get("redirect_uri")
	if redirect == "" {
		http.Error(w, "redirect_uri required", http.StatusBadRequest)
		return
	}
	email := get("email")
	if email == "" && r.Method == http.MethodGet {
		fmt.Fprintf(w, `<!doctype html><html><body><h1>oidpstub</h1>
<form method="post" action="/authorize">
<input type="hidden" name="redirect_uri" value="%s">
<input type="hidden" name="state" value="%s">
<input type="hidden" name="nonce" value="%s">
<input type="hidden" name="client_id" value="%s">
<label>Email <input name="email" value="alice@example.com"></label>
<label>Groups <input name="groups" value="spindle-admin"></label>
<button type="submit">Approve</button>
</form></body></html>`, html.EscapeString(redirect), html.EscapeString(get("state")),
			html.EscapeString(get("nonce")), html.EscapeString(get("client_id")))
		return
	}
	if email == "" {
		email = "alice@example.com"
	}
	var groups []string
	for _, g := range strings.Split(get("groups"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	s.mu.Lock()
	s.next++
	code := fmt.Sprintf("code-%d", s.next)
	s.codes[code] = pendingLogin{email: email, groups: groups, nonce: get("nonce"), aud: get("client_id")}
	s.mu.Unlock()

	sep := "?"
	if strings.Contains(redirect, "?") {
		sep = "&"
	}
	http.Redirect(w, r, redirect+sep+url.Values{"code": {code}, "state": {get("state")}}.Encode(), http.StatusFound)
}

func (s *stub) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.FormValue("client_id") != "" && r.FormValue("client_secret") != clientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	c, ok := s.codes[r.FormValue("code")]
	delete(s.codes, r.FormValue("code"))
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	aud := c.aud
	if aud == "" {
		aud = clientID
	}
	raw := s.mint(c.email, c.groups, aud, c.nonce, time.Now().Add(time.Hour))
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "dev-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": raw,
	})
}

func (s *stub) mint(email string, groups []string, aud, nonce string, exp time.Time) string {
	now := time.Now()
	claims := map[string]any{
		"iss": s.issuer, "sub": email, "aud": aud,
		"exp": exp.Unix(), "iat": now.Unix(), "nbf": now.Unix(),
		"email": email, "email_verified": true, "groups": groups,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	hdr, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "dev-1"})
	body, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		log.Printf("sign: %v", err)
		os.Exit(1)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
