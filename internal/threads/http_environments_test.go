package threads

// http_environments_test.go — the deployment-environment JSON API on BOTH
// lanes (docs/PLAN-THREADS-ENVIRONMENTS.md §9): CRUD, the write-only secrets,
// the maintain floor and the write-only value discipline (a value is accepted
// and never echoed, and only names appear in a read).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// lanes returns the two JSON lane spellings for a repo path.
func lanes(path string) []string {
	return []string{"/o/r/api" + path, "/o/r/api-browser" + path}
}

func TestEnvironmentAPILanesCRUD(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint"}
	e.roles.roles["maint"] = "maintain"
	e.roles.roles["writer"] = "write"

	for _, base := range lanes("/threads/environments") {
		rec := do(h, http.MethodGet, base, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s list = %d: %s", base, rec.Code, rec.Body.String())
		}
		var listed struct {
			Environments []EnvironmentView `json:"environments"`
			Enabled      bool              `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("%s list json: %v", base, err)
		}
		if len(listed.Environments) != 0 || listed.Enabled {
			t.Fatalf("%s empty list = %+v", base, listed)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s must be no-store", base)
		}

		rec = do(h, http.MethodPut, base+"/production", `{"required_reviewers":["writer"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s put = %d: %s", base, rec.Code, rec.Body.String())
		}
		var put struct {
			Environment *EnvironmentView `json:"environment"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
			t.Fatalf("%s put json: %v", base, err)
		}
		if put.Environment == nil || put.Environment.Name != "production" ||
			len(put.Environment.RequiredReviewers) != 1 {
			t.Fatalf("%s put = %+v", base, put.Environment)
		}

		rec = do(h, http.MethodGet, base+"/production", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s get = %d: %s", base, rec.Code, rec.Body.String())
		}
		rec = do(h, http.MethodDelete, base+"/production", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s delete = %d: %s", base, rec.Code, rec.Body.String())
		}
		rec = do(h, http.MethodGet, base+"/production", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s get deleted = %d", base, rec.Code)
		}
	}
}

func TestEnvironmentAPIFloorAndValidation(t *testing.T) {
	e, h, cur := newHTTP(t)
	e.roles.roles["maint"] = "maintain"
	// A reader cannot read or write the surface.
	*cur = auth.Principal{Name: "reader"}
	for _, path := range []string{"/o/r/api/threads/environments", "/o/r/api/threads/environments/production"} {
		if rec := do(h, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("reader GET %s = %d, want 403", path, rec.Code)
		}
	}
	// A maintainer is refused a reviewer without write access (400 names it).
	*cur = auth.Principal{Name: "maint"}
	rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production",
		`{"required_reviewers":["reader"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("read-role reviewer = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "write access") {
		t.Fatalf("error does not name the rule: %s", rec.Body.String())
	}
	// Unknown body fields are rejected (fail closed on writes).
	rec = do(h, http.MethodPut, "/o/r/api/threads/environments/production", `{"secrets":["X"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("unknown field = %d: %s", rec.Code, rec.Body.String())
	}
	// A bad name is a 400, never a record under a wrong key.
	rec = do(h, http.MethodPut, "/o/r/api/threads/environments/prod%20east", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvironmentAPISecretsWriteOnly(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint"}
	e.roles.roles["maint"] = "maintain"
	k, err := NewKeyring(strings.Repeat("k", 32), "")
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetKeyring(k)
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	// Disabled store answers 503 on a create attempt, before the key exists.
	// (Set here means the happy path; the disabled case is the service test.)

	for _, base := range lanes("/threads/environments/production/secrets") {
		rec := do(h, http.MethodPut, base+"/DEPLOY_TOKEN", `{"value":"super-secret-value"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s put secret = %d: %s", base, rec.Code, rec.Body.String())
		}
		rec = do(h, http.MethodGet, base, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s list = %d: %s", base, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "super-secret-value") {
			t.Fatalf("%s leaked the value", base)
		}
		var listed struct {
			Secrets []SecretView `json:"secrets"`
			Enabled bool         `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("%s json: %v", base, err)
		}
		if !listed.Enabled || len(listed.Secrets) != 1 || listed.Secrets[0].Name != "DEPLOY_TOKEN" {
			t.Fatalf("%s list = %+v", base, listed)
		}
		// The record lists the name (the resolver's allowlist).
		env, gerr := e.svc.GetEnvironment(ctx(), "o/r", "production", *cur)
		if gerr != nil || len(env.Secrets) != 1 || env.Secrets[0] != "DEPLOY_TOKEN" {
			t.Fatalf("%s record allowlist = %+v, %v", base, env, gerr)
		}
		rec = do(h, http.MethodDelete, base+"/DEPLOY_TOKEN", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s delete = %d: %s", base, rec.Code, rec.Body.String())
		}
	}
	// The value never appears in an error either.
	rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production/secrets/TOKEN", `{"value":""}`)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatalf("empty value = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEnvironmentAPIRouteEdges covers the dispatcher's method/404 branches and
// every handler's 401 branch (an unauthenticated caller) — the routing surface
// the happy-path tests do not reach.
func TestEnvironmentAPIRouteEdges(t *testing.T) {
	e, h, cur := newHTTP(t)
	e.roles.roles["maint"] = "maintain"
	*cur = auth.Principal{Name: "maint"}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/o/r/api/threads/environments", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/o/r/api/threads/environments", http.StatusMethodNotAllowed},
		{http.MethodPost, "/o/r/api/threads/environments/production", http.StatusMethodNotAllowed},
		{http.MethodPost, "/o/r/api/threads/environments/production/secrets", http.StatusMethodNotAllowed},
		{http.MethodGet, "/o/r/api/threads/environments/production/secrets/TOKEN", http.StatusMethodNotAllowed},
		{http.MethodPatch, "/o/r/api/threads/environments/production/secrets/TOKEN", http.StatusMethodNotAllowed},
		{http.MethodGet, "/o/r/api/threads/environments/production/extra/deep", http.StatusNotFound},
	}
	for _, tc := range cases {
		if rec := do(h, tc.method, tc.path, ""); rec.Code != tc.want {
			t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	// Unknown body fields on the secret write are rejected.
	if rec := do(h, http.MethodPut, "/o/r/api/threads/environments/production/secrets/TOKEN", `{"value":"x","other":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown secret field = %d", rec.Code)
	}
	// Every handler answers a real 401 for an unauthenticated caller.
	denied := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sign in"}
	}}
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/o/r/api/threads/environments"},
		{http.MethodGet, "/o/r/api/threads/environments/production"},
		{http.MethodPut, "/o/r/api/threads/environments/production"},
		{http.MethodDelete, "/o/r/api/threads/environments/production"},
		{http.MethodGet, "/o/r/api/threads/environments/production/secrets"},
		{http.MethodPut, "/o/r/api/threads/environments/production/secrets/TOKEN"},
		{http.MethodDelete, "/o/r/api/threads/environments/production/secrets/TOKEN"},
	} {
		rec := do(denied, tc.method, tc.path, `{"value":"x","required_reviewers":[]}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	// Service errors surface as their status: a reader is 403 on every verb.
	*cur = auth.Principal{Name: "reader"}
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/o/r/api/threads/environments/production", ""},
		{http.MethodPut, "/o/r/api/threads/environments/production", `{}`},
		{http.MethodDelete, "/o/r/api/threads/environments/production", ""},
		{http.MethodGet, "/o/r/api/threads/environments/production/secrets", ""},
		{http.MethodPut, "/o/r/api/threads/environments/production/secrets/TOKEN", `{"value":"x"}`},
		{http.MethodDelete, "/o/r/api/threads/environments/production/secrets/TOKEN", ""},
	} {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("reader %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}
