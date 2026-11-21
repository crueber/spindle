package server

// auth_challenge_test.go — the 401 WWW-Authenticate challenge. Static tokens
// authenticate over HTTP Basic (password = token); a challenge that only
// advertises Bearer leaves git's credential machinery with no username/
// password prompt, so `git push http://user:token@…` can never authenticate.

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestAuthChallengeAdvertisesBasicForStaticTokens(t *testing.T) {
	// Static tokens configured (newTestServer seeds alice/tok123).
	s, _ := newTestServer(t, nil)
	if got, want := s.authChallenge(), `Basic realm="walgit", Bearer realm="walgit"`; got != want {
		t.Fatalf("challenge with static tokens = %q, want %q", got, want)
	}

	// No static tokens → Bearer only (ID tokens / browser sessions only).
	plain, _ := newTestServer(t, func(o *Options) { o.Config.Server.Auth.Tokens = nil })
	if got, want := plain.authChallenge(), `Bearer realm="walgit"`; got != want {
		t.Fatalf("challenge without static tokens = %q, want %q", got, want)
	}

	// An unwired auth service must not panic.
	var bare Server
	if got := bare.authChallenge(); got != `Bearer realm="walgit"` {
		t.Fatalf("nil auth service challenge = %q", got)
	}

	// The challenge is actionable: Basic user:token reaches the static token
	// table (the username is ignored by walgit's LFS-era convention).
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:tok123")))
	p, aerr := s.authSvc.Authenticate(req, &config.Config{Server: config.Server{Auth: config.Auth{Mode: "token"}}})
	if aerr != nil || p.Name != "alice" || !p.Write {
		t.Fatalf("Basic static token = %+v %v", p, aerr)
	}
}
