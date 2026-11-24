package e2e

// oidc_test.go — the PLAN-OIDC-ONLY first-run and admin-by-group paths against
// the real binary and the in-test IdP (oidp_test.go). Every credential here is
// verified by the production chain: discovery, JWKS signature, nonce, PKCE,
// admission and the group claim.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// oidcBrowserClient dials the canonical host `walgit.localhost:port` at
// 127.0.0.1 (so cookies and the OIDC redirect URI stay on one host without
// depending on DNS); every other host dials normally (the IdP).
func oidcBrowserClient(t *testing.T, port int) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if strings.EqualFold(host, "walgit.localhost") {
				addr = fmt.Sprintf("127.0.0.1:%d", port)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Jar:       jar,
		Transport: tr,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// browserDo issues one request on the canonical host without following.
func browserDo(t *testing.T, c *http.Client, port int, method, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	u := fmt.Sprintf("http://walgit.localhost:%d%s", port, path)
	var resp *http.Response
	var err error
	if form != nil {
		resp, err = c.PostForm(u, form)
	} else {
		req, rerr := http.NewRequest(method, u, nil)
		if rerr != nil {
			t.Fatalf("%s %s: %v", method, u, rerr)
		}
		resp, err = c.Do(req)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	return resp, string(body)
}

// browserGet is browserDo for a GET.
func browserGet(t *testing.T, c *http.Client, port int, path string) (int, string) {
	t.Helper()
	resp, body := browserDo(t, c, port, http.MethodGet, path, nil)
	return resp.StatusCode, body
}

var hiddenInputRE = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`)

// loginThroughIdP drives GET /_auth/login → the IdP's authorize form (GET,
// then POST) → the callback → next, returning next's status and body. Every
// hop is a real HTTP request with the browser's cookie jar.
func loginThroughIdP(t *testing.T, c *http.Client, port int, idp *oidp, email, groups, next string) (int, string) {
	t.Helper()
	cb, cbOut := loginCallback(t, c, port, idp, email, groups, next)
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d (%s)", cb.StatusCode, cbOut)
	}
	return browserGet(t, c, port, next)
}

// loginCallback drives the login hops and returns the callback's own response
// (not the page behind it), so a refused sign-in can be asserted instead of
// failing the hop.
func loginCallback(t *testing.T, c *http.Client, port int, idp *oidp, email, groups, next string) (*http.Response, string) {
	t.Helper()
	resp, body := browserDo(t, c, port, http.MethodGet, "/_auth/login?next="+url.QueryEscape(next), nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /_auth/login = %d, want 302 (%s)", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, idp.issuer) {
		t.Fatalf("login Location = %q, want the IdP", loc)
	}
	// The provider serves its minimal form for a GET without email.
	authResp, err := c.Get(loc)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	formBody, _ := io.ReadAll(io.LimitReader(authResp.Body, 1<<20))
	authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("authorize GET = %d (%s)", authResp.StatusCode, formBody)
	}
	fields := url.Values{"email": {email}, "groups": {groups}}
	for _, m := range hiddenInputRE.FindAllStringSubmatch(string(formBody), -1) {
		fields.Set(m[1], m[2])
	}
	cbResp, err := c.PostForm(idp.issuer+"/authorize", fields)
	if err != nil {
		t.Fatalf("authorize POST: %v", err)
	}
	cbBody, _ := io.ReadAll(io.LimitReader(cbResp.Body, 1<<20))
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize POST = %d (%s)", cbResp.StatusCode, cbBody)
	}
	// Fetch the callback with the PKCE cookie the login hop set.
	cbURL := cbResp.Header.Get("Location")
	u, err := url.Parse(cbURL)
	if err != nil {
		t.Fatalf("callback URL %q: %v", cbURL, err)
	}
	cb, err := c.Get(fmt.Sprintf("http://walgit.localhost:%d%s?%s", port, u.Path, u.RawQuery))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	cbOut, _ := io.ReadAll(io.LimitReader(cb.Body, 1<<20))
	cb.Body.Close()
	return cb, string(cbOut)
}

func TestE2E_OIDCFirstRunAndAdminByGroup(t *testing.T) {
	requireModernGit(t)
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	idp := newOIDP(t)
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)

	// ---- first run: restricted, /admin is the only surface ------------------
	if st, b := httpGet(s.base+"/admin", 5*time.Second); st != http.StatusOK || !strings.Contains(string(b), "configure OIDC") {
		t.Fatalf("first-run /admin = %d, want the OIDC onboarding card (%s)", st, b)
	}
	if st, _ := httpGet(s.base+"/api/v1/owners", 5*time.Second); st != http.StatusServiceUnavailable {
		t.Fatalf("first-run API = %d, want 503", st)
	}

	// ---- save a complete OIDC config through the open setup API -------------
	overrides := map[string]any{
		"server.listen":                   fmt.Sprintf("0.0.0.0:%d", port),
		"server.public_url":               fmt.Sprintf("http://walgit.localhost:%d", port),
		"server.auto_create_on_push":      true,
		"server.auth.issuer":              idp.issuer,
		"server.auth.oauth_client_id":     idp.client,
		"server.auth.oauth_client_secret": idp.secret,
		"server.auth.session_secret":      "e2e-session-secret-0123456789abcdef",
		"server.auth.allowed_domains":     []string{"example.com"},
		"server.auth.admin_groups":        []string{"spindle-admin"},
		"server.auth.audiences":           []string{idp.client},
		"store.backend":                   "filesystem",
		"store.root":                      filepath.Join(dataDir, "store"),
		"cache.dir":                       filepath.Join(dataDir, "cache"),
	}
	payload, _ := json.Marshal(map[string]any{"overrides": overrides})
	saveStatus, saveBody := httpRequest(t, http.MethodPut, s.base+"/api/v1/setup", payload)
	if saveStatus < 200 || saveStatus > 299 {
		t.Fatalf("save = %d (%s)", saveStatus, saveBody)
	}
	s.stop()
	s2 := startServer(t, bin, dataDir, port)
	t.Cleanup(s2.stop)
	var ready map[string]any
	getJSON(t, s2.base+"/readyz", &ready)
	if ready["status"] != "ready" {
		t.Fatalf("readyz after save+restart = %v", ready["status"])
	}

	// ---- sign in as a group member: administer ------------------------------
	admin := oidcBrowserClient(t, port)
	status, body := loginThroughIdP(t, admin, port, idp, "alice@example.com", "spindle-admin", "/admin")
	if status != http.StatusOK {
		t.Fatalf("admin /admin = %d (%s)", status, body)
	}
	for _, want := range []string{"Administration", "host admin"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin /admin lacks %q", want)
		}
	}
	// The same session reads the admin API and /_auth/me reports its groups.
	meResp, err := admin.Get(fmt.Sprintf("http://walgit.localhost:%d/_auth/me", port))
	if err != nil {
		t.Fatal(err)
	}
	meBody, _ := io.ReadAll(meResp.Body)
	meResp.Body.Close()
	var me struct {
		Principal   string   `json:"principal"`
		Groups      []string `json:"groups"`
		Admin       bool     `json:"admin"`
		GroupsClaim string   `json:"groups_claim"`
	}
	if err := json.Unmarshal(meBody, &me); err != nil {
		t.Fatalf("/_auth/me = %s (%v)", meBody, err)
	}
	if !me.Admin || len(me.Groups) != 1 || me.Groups[0] != "spindle-admin" || me.GroupsClaim != "groups" {
		t.Fatalf("/_auth/me = %+v", me)
	}
	if status, body := httpRequestAuth(t, "GET", s2.base+"/api/v1/setup", nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous setup GET = %d, want 401 (%s)", status, body)
	}

	// ---- sign in WITHOUT the group: the denied page names the diagnosis -----
	plain := oidcBrowserClient(t, port)
	status, body = loginThroughIdP(t, plain, port, idp, "bob@example.com", "", "/admin")
	if status != http.StatusForbidden {
		t.Fatalf("non-admin /admin = %d, want 403 (%s)", status, body)
	}
	for _, want := range []string{"Access restricted", "verified groups", "server.auth.admin_groups", "no unauthenticated back door"} {
		if !strings.Contains(body, want) {
			t.Fatalf("denied page lacks %q:\n%s", want, body)
		}
	}
	// And it can never reach the editor.
	if status, body := browserGet(t, plain, port, "/admin/config"); status != http.StatusForbidden || strings.Contains(body, "cfg.server.listen") {
		t.Fatalf("non-admin /admin/config = %d, leaked form: %v", status, strings.Contains(body, "cfg.server.listen"))
	}
}

// TestE2E_OIDCDiscoveryFailureMessages pins the Test button's precise failures
// through the setup probe endpoint: an unreachable issuer and a discovery
// document whose JWKS serves no usable signing key.
func TestE2E_OIDCDiscoveryFailureMessages(t *testing.T) {
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)

	post := func(issuer string) (int, map[string]any) {
		payload, _ := json.Marshal(map[string]any{"issuer": issuer, "client_id": "c", "redirect_uri": "http://localhost/_auth/callback"})
		st, body := httpRequest(t, http.MethodPost, s.base+"/api/v1/setup/auth/test", payload)
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return st, out
	}
	errText := func(out map[string]any) string {
		var b strings.Builder
		if errs, ok := out["errors"].([]any); ok {
			for _, e := range errs {
				if m, ok := e.(map[string]any); ok {
					b.WriteString(fmt.Sprint(m["message"]))
					b.WriteString(" ")
				}
			}
		}
		return b.String()
	}

	st, out := post("http://127.0.0.1:1")
	if st != http.StatusUnprocessableEntity || !strings.Contains(errText(out), "discovery fetch failed") {
		t.Fatalf("unreachable issuer = %d %v", st, out)
	}

	idp := newOIDP(t)
	idp.noKeys = true
	st, out = post(idp.issuer)
	if st != http.StatusUnprocessableEntity || !strings.Contains(errText(out), "no usable signing key") {
		t.Fatalf("empty JWKS = %d %v", st, out)
	}

	st, out = post(newOIDP(t).issuer)
	if st != http.StatusOK || out["ok"] != true {
		t.Fatalf("healthy issuer = %d %v", st, out)
	}
}

// TestE2E_OIDCDirectoryDeactivate drives the instance directory's one
// mutation end to end against the in-test IdP (PLAN-ADMIN §3.7): a host admin
// deactivates a signed-in account, the account's EXISTING session is refused at
// the next request, a fresh sign-in is refused at the callback, reactivation
// restores sign-in while the pre-deactivation session stays revoked, and both
// transitions are in the audit log.
func TestE2E_OIDCDirectoryDeactivate(t *testing.T) {
	requireModernGit(t)
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	idp := newOIDP(t)
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)

	overrides := map[string]any{
		"server.listen":                   fmt.Sprintf("0.0.0.0:%d", port),
		"server.public_url":               fmt.Sprintf("http://walgit.localhost:%d", port),
		"server.auto_create_on_push":      true,
		"server.auth.issuer":              idp.issuer,
		"server.auth.oauth_client_id":     idp.client,
		"server.auth.oauth_client_secret": idp.secret,
		"server.auth.session_secret":      "e2e-session-secret-0123456789abcdef",
		"server.auth.allowed_domains":     []string{"example.com"},
		"server.auth.admin_groups":        []string{"spindle-admin"},
		"server.auth.audiences":           []string{idp.client},
		"store.backend":                   "filesystem",
		"store.root":                      filepath.Join(dataDir, "store"),
		"cache.dir":                       filepath.Join(dataDir, "cache"),
	}
	payload, _ := json.Marshal(map[string]any{"overrides": overrides})
	if st, body := httpRequest(t, http.MethodPut, s.base+"/api/v1/setup", payload); st < 200 || st > 299 {
		t.Fatalf("save = %d (%s)", st, body)
	}
	s.stop()
	s2 := startServer(t, bin, dataDir, port)
	t.Cleanup(s2.stop)

	admin := oidcBrowserClient(t, port)
	if st, body := loginThroughIdP(t, admin, port, idp, "alice@example.com", "spindle-admin", "/admin"); st != http.StatusOK {
		t.Fatalf("admin sign-in = %d (%s)", st, body)
	}
	// bob signs in once (no admin group): that creates his account binding.
	bob := oidcBrowserClient(t, port)
	if st, _ := loginThroughIdP(t, bob, port, idp, "bob@example.com", "", "/admin"); st != http.StatusForbidden {
		t.Fatalf("non-admin sign-in = %d, want 403", st)
	}

	st, body := browserGet(t, admin, port, "/admin/directory")
	if st != http.StatusOK {
		t.Fatalf("GET /admin/directory = %d (%s)", st, body)
	}
	for _, want := range []string{"Users and organizations", ">bob<", `href="/bob"`, "deactivate account"} {
		if !strings.Contains(body, want) {
			t.Fatalf("directory lacks %q", want)
		}
	}

	// Deactivate through the page (same-origin, typed confirmation + reason).
	if st, body := directoryPost(t, admin, port, "/admin/directory/deactivate", url.Values{
		"username": {"bob"}, "confirm": {"bob"}, "reason": {"e2e: account abuse"},
	}); st != http.StatusSeeOther {
		t.Fatalf("deactivate = %d (%s)", st, body)
	}
	// The EXISTING session is refused at the next request — not at expiry.
	if st, _ := browserGet(t, bob, port, "/_auth/me"); st != http.StatusForbidden {
		t.Fatalf("disabled account's live session = %d, want 403", st)
	}
	// A fresh sign-in is refused at the callback (a real 403, not a 200 page).
	fresh := oidcBrowserClient(t, port)
	cb, cbBody := loginCallback(t, fresh, port, idp, "bob@example.com", "", "/")
	if cb.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled sign-in callback = %d (%s), want 403", cb.StatusCode, cbBody)
	}
	if strings.Contains(cbBody, `"principal"`) || strings.Contains(cbBody, "Administration") {
		t.Fatalf("disabled sign-in returned a page body: %s", cbBody)
	}

	// Reactivation is a NEW second in practice; the test pins that boundary
	// explicitly because the wire's `iat` has second precision (the gate is
	// fail-closed inside the deactivation second — see 06 §8.5).
	time.Sleep(1100 * time.Millisecond)

	// Reactivate: sign-in works again, the pre-deactivation session does not.
	if st, body := directoryPost(t, admin, port, "/admin/directory/reactivate", url.Values{"username": {"bob"}}); st != http.StatusSeeOther {
		t.Fatalf("reactivate = %d (%s)", st, body)
	}
	if st, _ := browserGet(t, bob, port, "/_auth/me"); st != http.StatusUnauthorized {
		t.Fatalf("pre-deactivation session after reactivation = %d, want 401 (revoked forever)", st)
	}
	again := oidcBrowserClient(t, port)
	if st, body := loginThroughIdP(t, again, port, idp, "bob@example.com", "", "/_auth/me"); st != http.StatusOK {
		t.Fatalf("reactivated sign-in = %d (%s)", st, body)
	}
	if st, body := browserGet(t, again, port, "/_auth/me"); st != http.StatusOK || !strings.Contains(body, `"principal":"bob"`) {
		t.Fatalf("reactivated /_auth/me = %d (%s)", st, body)
	}

	// Both transitions are in the audit log, with the operator and the reason.
	if st, body := browserGet(t, admin, port, "/admin/audit"); st != http.StatusOK ||
		!strings.Contains(body, "user_deactivate") || !strings.Contains(body, "user_reactivate") ||
		!strings.Contains(body, "e2e: account abuse") {
		t.Fatalf("audit page lacks the directory transitions (%d): %s", st, body)
	}
}

// directoryPost posts an admin form on the canonical host with the Origin the
// page would send (the area's same-origin rule).
func directoryPost(t *testing.T, c *http.Client, port int, path string, form url.Values) (int, string) {
	t.Helper()
	origin := fmt.Sprintf("http://walgit.localhost:%d", port)
	req, err := http.NewRequest(http.MethodPost, origin+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode, string(body)
}
