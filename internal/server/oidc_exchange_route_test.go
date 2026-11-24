package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The job OIDC exchange carries its own opaque request token (wot_). The
// session authenticator must not consume it: the path hands the request to the
// api provider, whose threads handler verifies the token and refuses it with a
// real 401 otherwise. Every other path keeps the normal auth behaviour, so the
// bypass cannot be used as an auth hole.
func TestAPIServeIDTokenExchangeBypassesSessionAuth(t *testing.T) {
	s, prim := oidcServer502(t)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/threads/jobs/jr_x/attempt/1/id-token", strings.NewReader(`{"audience":"aud"}`))
	req.Header.Set("Authorization", "Bearer wot_0123456789abcdef")
	rec := httptest.NewRecorder()
	s.apiServe(rec, req)
	if prim.served != 1 {
		t.Fatalf("exchange path did not reach the api provider (served=%d, status=%d)", prim.served, rec.Code)
	}

	// The same unknown bearer on any other path is still a real 401 and never
	// reaches the handler.
	prim.served = 0
	req = httptest.NewRequest(http.MethodPost, "/api/v1/ssh-keys", nil)
	req.Header.Set("Authorization", "Bearer wot_0123456789abcdef")
	rec = httptest.NewRecorder()
	s.apiServe(rec, req)
	if rec.Code != http.StatusUnauthorized || prim.served != 0 {
		t.Fatalf("unknown bearer elsewhere = %d (served=%d), want 401 and no handler", rec.Code, prim.served)
	}

	// Discovery stays a normal open read.
	prim.served = 0
	req = httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rec = httptest.NewRecorder()
	s.apiServe(rec, req)
	if prim.served != 1 || rec.Code != http.StatusOK {
		t.Fatalf("jwks = %d (served=%d)", rec.Code, prim.served)
	}
}

func TestIDTokenExchangePathShape(t *testing.T) {
	yes := []string{
		"/api/v1/threads/jobs/jr_abc/attempt/1/id-token",
		"/api/v1/threads/jobs/jr_abc/attempt/12/id-token",
	}
	no := []string{
		"/api/v1/threads/jobs/jr_abc/id-token",
		"/api/v1/threads/jobs/jr_abc/attempt/1/id-token/extra",
		"/api/v1/threads/jobs//attempt/1/id-token",
		"/api/v1/threads/jobs/jr_abc/attempt//id-token",
		"/api/v1/threads/jobs/jr_abc/start/1/id-token",
		"/api-browser/v1/threads/jobs/jr_abc/attempt/1/id-token",
		"/api/v1/threads/runners/id-token",
	}
	for _, p := range yes {
		if !idTokenExchangePath(p) {
			t.Errorf("%q should be the exchange path", p)
		}
	}
	for _, p := range no {
		if idTokenExchangePath(p) {
			t.Errorf("%q must not be the exchange path", p)
		}
	}
}
