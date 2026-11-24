package api

// sshkeys_test.go — the /api/v1/ssh-keys surface (17_ssh.md §3): ownership,
// gates, and the registry error mapping, over a stub store.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type stubSSHKeys struct {
	recs   map[string]SshKeyRecord
	addErr error
	getErr error
}

func (s *stubSSHKeys) List(_ context.Context, principal string) ([]SshKeyRecord, error) {
	var out []SshKeyRecord
	for _, r := range s.recs {
		if r.Principal == principal {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *stubSSHKeys) Add(_ context.Context, principal, key, title string) (SshKeyRecord, error) {
	if s.addErr != nil {
		return SshKeyRecord{}, s.addErr
	}
	rec := SshKeyRecord{Principal: principal, ID: "fp-1", Key: key, Title: title}
	s.recs[rec.ID] = rec
	return rec, nil
}

func (s *stubSSHKeys) Get(_ context.Context, principal, id string) (SshKeyRecord, error) {
	if s.getErr != nil {
		return SshKeyRecord{}, s.getErr
	}
	rec, ok := s.recs[id]
	if !ok {
		return SshKeyRecord{}, ErrKeyNotFound
	}
	if rec.Principal != principal {
		return SshKeyRecord{}, ErrKeyForbidden
	}
	return rec, nil
}

func (s *stubSSHKeys) Delete(_ context.Context, principal, id string) error {
	if _, err := s.Get(nil, principal, id); err != nil {
		return err
	}
	delete(s.recs, id)
	return nil
}

const validKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop"

func TestSSHKeysListRefusesAnonymous(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = true
	f.env.SSHKeys = &stubSSHKeys{recs: map[string]SshKeyRecord{
		"fp-1": {ID: "fp-1", Principal: "anonymous", Key: validKeyLine},
	}}

	w := f.do("GET", "/api/v1/ssh-keys", nil, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if h := w.Header().Get("WWW-Authenticate"); h != `Bearer realm="walgit"` {
		t.Fatalf("anonymous list WWW-Authenticate = %q", h)
	}
	if strings.Contains(w.Body.String(), "fp-1") || strings.Contains(w.Body.String(), "AAAAC3") {
		t.Fatalf("anonymous list leaked a key: %s", w.Body.String())
	}

	// A named (even read-only) principal keeps its own listing.
	w = f.do("GET", "/api/v1/ssh-keys", nil, nil, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("read-only principal list = %d (%s)", w.Code, w.Body.String())
	}
}

func TestSSHKeysAPINotConfigured(t *testing.T) {
	f := newFixture(t)
	// SSHKeys nil → 503 on every verb
	w := f.do("GET", "/api/v1/ssh-keys", nil, nil, readP())
	if w.Code != 503 {
		t.Fatalf("list = %d %s, want 503", w.Code, w.Body.String())
	}
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "x"}`), nil, readP())
	if w.Code != 503 {
		t.Fatalf("add = %d %s, want 503", w.Code, w.Body.String())
	}
	w = f.do("DELETE", "/api/v1/ssh-keys/x", nil, nil, readP())
	if w.Code != 503 {
		t.Fatalf("delete = %d, want 503", w.Code)
	}
}

func TestSSHKeysAPIWriteGate(t *testing.T) {
	// AuthWrite was relaxed to AuthRead for add/delete (identity self-service):
	// a read-only principal may manage their own keys (their SSH rights still
	// resolve per principal at auth time).
	f := newFixture(t)
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub
	w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`"}`), nil, readP())
	if w.Code != 201 {
		t.Fatalf("read-only principal add = %d %s", w.Code, w.Body.String())
	}
	if rec := stub.recs["fp-1"]; rec.Principal != "reader" {
		t.Fatalf("record principal = %q", rec.Principal)
	}
	_ = store.ErrKindNotFound // keep the store import if unused elsewhere
}

// TestSSHKeysAddDeleteBranches covers the handler's client-error and failure
// arms for a signed-in principal (the anonymous arms are pinned separately).
func TestSSHKeysAddDeleteBranches(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = true
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub
	alice := auth.Principal{Name: "alice@example.com", Write: true}

	// Unreadable JSON and a missing key are 400s.
	if w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader("{"), nil, &alice); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", w.Code)
	}
	if w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key":"  "}`), nil, &alice); w.Code != http.StatusBadRequest {
		t.Fatalf("empty key = %d", w.Code)
	}
	// A store failure that is not a known key error is a 500.
	stub.addErr = errors.New("store down")
	if w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key":"ssh-ed25519 AAAA x"}`), nil, &alice); w.Code != http.StatusInternalServerError {
		t.Fatalf("generic add error = %d", w.Code)
	}
	stub.addErr = nil

	// Delete: empty id 400, unknown 404, forbidden 403, generic 500.
	if w := f.do("DELETE", "/api/v1/ssh-keys/", nil, nil, &alice); w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Fatalf("empty id = %d", w.Code)
	}
	if w := f.do("DELETE", "/api/v1/ssh-keys/missing", nil, nil, &alice); w.Code != http.StatusNotFound {
		t.Fatalf("unknown delete = %d", w.Code)
	}
	stub.getErr = ErrKeyForbidden
	if w := f.do("DELETE", "/api/v1/ssh-keys/fp-1", nil, nil, &alice); w.Code != http.StatusForbidden {
		t.Fatalf("forbidden delete = %d", w.Code)
	}
	stub.getErr = errors.New("store down")
	if w := f.do("DELETE", "/api/v1/ssh-keys/fp-1", nil, nil, &alice); w.Code != http.StatusInternalServerError {
		t.Fatalf("generic delete = %d", w.Code)
	}
	stub.getErr = nil

	// List with a store failure is a 500.
	f.env.SSHKeys = &errListKeys{stub: stub}
	if w := f.do("GET", "/api/v1/ssh-keys", nil, nil, &alice); w.Code != http.StatusInternalServerError {
		t.Fatalf("list error = %d", w.Code)
	}
}

// errListKeys makes List fail.
type errListKeys struct{ stub *stubSSHKeys }

func (e *errListKeys) List(context.Context, string) ([]SshKeyRecord, error) {
	return nil, errors.New("list down")
}
func (e *errListKeys) Add(ctx context.Context, p, k, t string) (SshKeyRecord, error) {
	return e.stub.Add(ctx, p, k, t)
}
func (e *errListKeys) Get(ctx context.Context, p, id string) (SshKeyRecord, error) {
	return e.stub.Get(ctx, p, id)
}
func (e *errListKeys) Delete(ctx context.Context, p, id string) error {
	return e.stub.Delete(ctx, p, id)
}
