package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func testHandler(t *testing.T, p auth.Principal) (*Handler, *Service) {
	t.Helper()
	svc, _ := testService(t)
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
	return h, svc
}

func do(h *Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	if !h.Handle(rec, req) {
		rec.Code = http.StatusNotFound
	}
	return rec
}

func TestBlocksGetMatrix(t *testing.T) {
	alice := auth.Principal{Name: "alice"}
	bob := auth.Principal{Name: "bob"}
	admin := auth.Principal{Name: "root", Admin: true}

	if rec := do(mustHandler(t, auth.Anonymous()), "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if rec := do(mustHandler(t, bob), "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusForbidden {
		t.Fatalf("other = %d", rec.Code)
	}
	// Browser lane is the same handler.
	rec := do(mustHandler(t, alice), "GET", "/api-browser/v1/users/alice/blocks")
	if rec.Code != http.StatusOK {
		t.Fatalf("self = %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	var got struct {
		Version int      `json:"version"`
		Blocked []string `json:"blocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Blocked == nil || len(got.Blocked) != 0 {
		t.Fatalf("body = %+v", got)
	}
	if rec := do(mustHandler(t, admin), "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusOK {
		t.Fatalf("admin = %d", rec.Code)
	}
	// Unknown principal is the profile-existence probe: 404.
	if rec := do(mustHandler(t, admin), "GET", "/api/v1/users/ghost/blocks"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rec.Code)
	}
}

func mustHandler(t *testing.T, p auth.Principal) *Handler {
	h, _ := testHandler(t, p)
	return h
}

func TestBlocksMutateMatrix(t *testing.T) {
	alice := auth.Principal{Name: "alice"}
	bob := auth.Principal{Name: "bob"}

	h := mustHandler(t, alice)
	rec := do(h, "PUT", "/api/v1/users/alice/blocks/bob")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":1`) {
		t.Fatalf("block = %d %s", rec.Code, rec.Body.String())
	}
	// Already blocked → the plan's 409.
	if rec := do(h, "PUT", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusConflict {
		t.Fatalf("repeat = %d", rec.Code)
	}
	// Not your list.
	if rec := do(mustHandler(t, bob), "PUT", "/api/v1/users/alice/blocks/carol"); rec.Code != http.StatusForbidden {
		t.Fatalf("other edit = %d", rec.Code)
	}
	// Anonymous → 401.
	if rec := do(mustHandler(t, auth.Anonymous()), "PUT", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon edit = %d", rec.Code)
	}
	// Self-block, unknown subject, invalid subject → 400.
	for _, target := range []string{"alice", "ghost", "bad%2Fname"} {
		if rec := do(h, "PUT", "/api/v1/users/alice/blocks/"+target); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", target, rec.Code, rec.Body.String())
		}
	}
	// The list now shows bob.
	rec = do(h, "GET", "/api/v1/users/alice/blocks")
	if !strings.Contains(rec.Body.String(), `"bob"`) {
		t.Fatalf("list = %s", rec.Body.String())
	}
	// Unblock, then a second unblock is 404 (the plan's contract).
	if rec := do(h, "DELETE", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusOK {
		t.Fatalf("unblock = %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusNotFound {
		t.Fatalf("repeat unblock = %d", rec.Code)
	}
}

func TestBlocksRoutingAndMethods(t *testing.T) {
	h := mustHandler(t, auth.Principal{Name: "alice"})
	for _, path := range []string{
		"/api/v1/users/alice",
		"/api/v1/users/alice/blocked",
		"/api/v1/users/alice/blocks/bob/extra",
		"/v1/users/alice/blocks",
		"/api/v1/orgs/acme/blocks",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		if h.Handle(rec, req) {
			t.Fatalf("%s claimed", path)
		}
	}
	if rec := do(h, "POST", "/api/v1/users/alice/blocks"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST list = %d", rec.Code)
	}
	if rec := do(h, "PATCH", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH subject = %d", rec.Code)
	}
	// ServeHTTP 404s unclaimed paths.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ServeHTTP = %d", rec.Code)
	}
}

func TestBlocksAuthErrorMaps(t *testing.T) {
	svc, _ := testService(t)
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	}}
	if rec := do(h, "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusForbidden {
		t.Fatalf("auth error = %d", rec.Code)
	}
}

func TestBlockStoreFailureIs503(t *testing.T) {
	svc, _ := testService(t)
	svc.Store = errStore{}
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}}
	rec := do(h, "PUT", "/api/v1/users/alice/blocks/bob")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "15" {
		t.Fatal("503 must carry Retry-After")
	}
}

// errStore fails every read (the fail-closed store outage).
type errStore struct{ store.ObjectStore }

func (errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("boom")
}

func (errStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return nil, errors.New("boom")
}

func (errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("boom")
}

func TestExposedCoversRoutes(t *testing.T) {
	h := mustHandler(t, auth.Principal{Name: "alice"})
	cases := []struct {
		method, path, canonical string
		want                    bool
	}{
		{"GET", "/api/v1/users/alice/blocks", "/api/v1/users/{principal}/blocks", true},
		{"GET", "/api-browser/v1/users/alice/blocks", "/api/v1/users/{principal}/blocks", true},
		{"PUT", "/api/v1/users/alice/blocks/bob", "/api/v1/users/{principal}/blocks/{subject}", true},
		{"DELETE", "/api-browser/v1/users/alice/blocks/bob", "/api/v1/users/{principal}/blocks/{subject}", true},
		{"GET", "/acme/widgets/api/moderation", "/{owner}/{repo}/api/moderation", true},
		{"GET", "/acme/widgets/api-browser/moderation", "/{owner}/{repo}/api/moderation", true},
		{"GET", "/acme/widgets/api/moderation/reports", "/{owner}/{repo}/api/moderation/reports", true},
		{"GET", "/acme/widgets/api-browser/moderation/reports", "/{owner}/{repo}/api/moderation/reports", true},
		{"POST", "/acme/widgets/api/moderation/reports", "/{owner}/{repo}/api/moderation/reports", true},
		{"POST", "/acme/widgets/api/moderation/reports/1/resolve", "/{owner}/{repo}/api/moderation/reports/{id}/resolve", true},
		{"POST", "/acme/widgets/api-browser/moderation/reports/1/dismiss", "/{owner}/{repo}/api/moderation/reports/{id}/dismiss", true},
		{"GET", "/api/v1/moderation/reports", "/api/v1/moderation/reports", true},
		{"GET", "/api-browser/v1/moderation/reports", "/api/v1/moderation/reports", true},
		{"POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", "/api/v1/moderation/reports/{owner}/{repo}/{id}/resolve", true},
		{"GET", "/api/v1/moderation/audit", "/api/v1/moderation/audit", true},
		{"GET", "/api/v1/users/alice", "", false},
	}
	covered := make([]bool, len(ExposedTemplates))
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		if got := h.Handle(rec, req); got != c.want {
			t.Fatalf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
		if !c.want {
			continue
		}
		seg := strings.Split(strings.TrimPrefix(c.path, "/"), "/")
		if strings.HasPrefix(c.path, "/api-browser/") {
			seg[0] = "api"
		}
		canon := "/" + strings.Join(seg, "/")
		if c.canonical != "" {
			canon = c.canonical
		}
		found := false
		for i, tpl := range ExposedTemplates {
			if tpl == canon {
				covered[i] = true
				found = true
			}
		}
		if !found {
			t.Fatalf("%s not in ExposedTemplates", canon)
		}
	}
	for i, ok := range covered {
		if !ok {
			t.Fatalf("template %s has no served route", ExposedTemplates[i])
		}
	}
}
