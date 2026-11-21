package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// identityFixture wires the registry onto the REAL identity service: the push
// gate under test must be the git receive-pack rule, not a double.
func identityFixture(t *testing.T) (*Service, *identity.Service, *Handler) {
	t.Helper()
	mem := store.NewMemory()
	ident := identity.New(mem, config.Defaults())
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ident.Now = func() time.Time { return now }
	svc := New(mem, ident)
	svc.Now = func() time.Time { return now }
	svc.TokenSecret = []byte("parity-secret")
	svc.SpoolDir = t.TempDir()
	return svc, ident, &Handler{Svc: svc, Auth: testAuth}
}

// TestCanPushReusesGitOwnerRule is docs/AUDIT-V1.md finding 1: the repository
// owner pushes to their own repo with NO access.json, exactly as git allows;
// a read-only principal still cannot push; an explicit write binding and a
// host admin can.
func TestCanPushReusesGitOwnerRule(t *testing.T) {
	svc, ident, _ := identityFixture(t)
	ctx := context.Background()
	const owner, repo = "alice", "private"

	// No access.json exists for alice/private: the synthesized doc is public
	// with no bindings, so only the self-namespace owner rule can admit her.
	if _, _, err := ident.GetAccess(ctx, owner, repo); err != nil {
		t.Fatalf("get access = %v", err)
	}
	if _, _, err := store.GetBytes(ctx, svc.Store, "repos/alice/private/access.json", store.GetOptions{}); err == nil {
		t.Fatal("fixture precondition: access.json must not exist")
	}
	alice := auth.Principal{Name: "alice"}
	if oerr := svc.CanPush(ctx, owner, repo, alice); oerr != nil {
		t.Fatalf("owner push without access.json = %+v", oerr)
	}

	// A stranger is denied, and so is a read binding.
	bob := auth.Principal{Name: "bob"}
	if oerr := svc.CanPush(ctx, owner, repo, bob); oerr == nil {
		t.Fatal("non-owner push must be denied")
	}
	if _, err := ident.PutAccess(ctx, owner, repo, "", identity.VisibilityPrivate, []identity.AccessBinding{
		{Subject: "user:bob", Role: identity.RoleRead},
	}); err != nil {
		t.Fatalf("put access = %v", err)
	}
	if oerr := svc.CanPush(ctx, owner, repo, bob); oerr == nil || oerr.Status != http.StatusForbidden {
		t.Fatalf("read-role push = %+v", oerr)
	}
	// ... but bob can still pull (the read gate is untouched).
	if oerr := svc.CanPull(ctx, owner, repo, bob); oerr != nil {
		t.Fatalf("read-role pull = %+v", oerr)
	}

	// A write binding may push.
	if _, err := ident.PutAccess(ctx, owner, repo, "", identity.VisibilityPrivate, []identity.AccessBinding{
		{Subject: "user:bob", Role: identity.RoleWrite},
	}); err != nil {
		t.Fatalf("put write access = %v", err)
	}
	if oerr := svc.CanPush(ctx, owner, repo, bob); oerr != nil {
		t.Fatalf("write-role push = %+v", oerr)
	}

	// A host admin may push; anonymous is a real 401.
	if oerr := svc.CanPush(ctx, owner, repo, auth.Principal{Name: "root", Admin: true}); oerr != nil {
		t.Fatalf("admin push = %+v", oerr)
	}
	if oerr := svc.CanPush(ctx, owner, repo, auth.Anonymous()); oerr == nil || oerr.Status != http.StatusUnauthorized {
		t.Fatalf("anonymous push = %+v", oerr)
	}

	// The host-wide write flag grants nothing (the #347 rule): a principal
	// with Write set but no binding cannot push.
	if oerr := svc.CanPush(ctx, owner, repo, auth.Principal{Name: "carol", Write: true}); oerr == nil {
		t.Fatalf("host-write-only push = %+v", oerr)
	}
}

// TestTokenEndpointGrantsOwnerPush proves the token SCOPE follows the same
// rule: the Docker client's /v2/token request for push is granted for the
// owner and denied for an outsider.
func TestTokenEndpointGrantsOwnerPush(t *testing.T) {
	_, _, h := identityFixture(t)
	get := func(user string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/v2/token?service=x&scope=repository:alice/private:pull,push", nil)
		r.SetBasicAuth(user, "tok")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s token = %d %s", user, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("token decode: %v", err)
		}
		return body
	}
	acts := func(body map[string]any) []string {
		access, _ := body["access"].([]any)
		if len(access) == 0 {
			return nil
		}
		entry, _ := access[0].(map[string]any)
		raw, _ := entry["actions"].([]any)
		out := make([]string, 0, len(raw))
		for _, a := range raw {
			out = append(out, fmt.Sprint(a))
		}
		return out
	}

	owner := acts(get("alice"))
	if !contains(owner, "push") || !contains(owner, "pull") {
		t.Fatalf("owner token actions = %v, want pull+push", owner)
	}
	outsider := acts(get("bob"))
	if contains(outsider, "push") {
		t.Fatalf("outsider token granted push: %v", outsider)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestDeleteByDigestDropsTags is finding 5: deleting a manifest by digest
// removes every tag pointer aimed at it, and a second delete is a real 404.
func TestDeleteByDigestDropsTags(t *testing.T) {
	f := newFixture(t)
	body := []byte(imageManifest)
	for _, tag := range []string{"t01", "t02", "keep"} {
		if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/"+tag, body); w.Code != 201 {
			t.Fatalf("put %s = %d", tag, w.Code)
		}
	}
	// keep points elsewhere.
	other := bytes.Replace(body, []byte("image.manifest.v1"), []byte("image.manifest.v2"), 1)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/keep", other); w.Code != 201 {
		t.Fatalf("put keep = %d", w.Code)
	}
	digest := digestOf(body)
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+digest, nil); w.Code != http.StatusAccepted {
		t.Fatalf("delete digest = %d %s", w.Code, w.Body.String())
	}
	w := f.as("reader", "GET", "/v2/acme/demo/tags/list", nil)
	var page struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("tags decode = %v", err)
	}
	if len(page.Tags) != 1 || page.Tags[0] != "keep" {
		t.Fatalf("tags after digest delete = %v, want [keep]", page.Tags)
	}
	// The version's tags are gone from the wire too.
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/t01", nil); w.Code != 404 {
		t.Fatalf("deleted tag GET = %d", w.Code)
	}
	// A repeat delete is a real 404 (never a silent 202).
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+digest, nil); w.Code != 404 {
		t.Fatalf("repeat delete digest = %d", w.Code)
	}
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/absent", nil); w.Code != 404 {
		t.Fatalf("unknown tag delete = %d", w.Code)
	}
}

// TestPackagesAPISurface exercises the four JSON endpoints, both lanes, and
// the permission matrix (read lists, write deletes, anon denied).
func TestPackagesAPISurface(t *testing.T) {
	f := newFixture(t)
	f.roles.public["acme/demo"] = true
	f.roles.roles["read|acme/demo"] = identity.RoleRead
	f.roles.roles["write|acme/demo"] = identity.RoleWrite
	body := []byte(imageManifest)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/1.0.0", body); w.Code != 201 {
		t.Fatalf("put = %d", w.Code)
	}

	for _, lane := range []string{"api", "api-browser"} {
		w := f.as("read", "GET", "/acme/demo/"+lane+"/packages", nil)
		if w.Code != 200 {
			t.Fatalf("%s list = %d %s", lane, w.Code, w.Body.String())
		}
		var got RepoPackages
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s decode: %v", lane, err)
		}
		if got.Repository != "acme/demo" || len(got.Packages) != 1 {
			t.Fatalf("%s list body = %+v", lane, got)
		}
		row := got.Packages[0]
		if row.Tag != "1.0.0" || row.Digest != digestOf(body) || row.Size == 0 || row.Pull == "" {
			t.Fatalf("%s row = %+v", lane, row)
		}
		if !strings.HasPrefix(row.Pull, "docker pull ") {
			t.Fatalf("%s pull command = %q", lane, row.Pull)
		}
		if len(got.Manifests) != 1 || len(got.Manifests[0].Tags) != 1 {
			t.Fatalf("%s manifests = %+v", lane, got.Manifests)
		}
	}

	// Get one manifest by digest, with its JSON body.
	w := f.as("reader", "GET", "/acme/demo/api/packages/"+digestOf(body), nil)
	if w.Code != 200 {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	var view ManifestView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("get decode: %v", err)
	}
	if view.Digest != digestOf(body) || len(view.Tags) != 1 || len(view.Manifest) == 0 {
		t.Fatalf("get view = %+v", view)
	}

	// Not found shapes.
	if w := f.as("reader", "GET", "/acme/demo/api/packages/sha256:"+strings.Repeat("0", 64), nil); w.Code != 404 {
		t.Fatalf("get missing = %d", w.Code)
	}
	if w := f.as("writer", "DELETE", "/acme/demo/api/packages/tags/absent", nil); w.Code != 404 {
		t.Fatalf("delete missing tag = %d", w.Code)
	}

	// Write gates: read cannot delete; anon cannot list a private repo.
	if w := f.as("read", "DELETE", "/acme/demo/api/packages/tags/1.0.0", nil); w.Code != 403 {
		t.Fatalf("read delete = %d", w.Code)
	}
	delete(f.roles.public, "acme/demo")
	if w := f.req("GET", "/acme/demo/api/packages", nil, nil); w.Code != 401 {
		t.Fatalf("anon private list = %d", w.Code)
	}
	f.roles.public["acme/demo"] = true

	// Delete by tag, then by digest: both 204, and the manifest is gone.
	if w := f.as("write", "DELETE", "/acme/demo/api/packages/tags/1.0.0", nil); w.Code != 204 {
		t.Fatalf("delete tag = %d %s", w.Code, w.Body.String())
	}
	if w := f.as("write", "DELETE", "/acme/demo/api/packages/"+digestOf(body), nil); w.Code != 204 {
		t.Fatalf("delete digest = %d %s", w.Code, w.Body.String())
	}
	if w := f.as("reader", "GET", "/acme/demo/api/packages/"+digestOf(body), nil); w.Code != 404 {
		t.Fatalf("get after delete = %d", w.Code)
	}

	// Feature off ⇒ 404, exactly like the registry routes.
	f.featOff["acme/demo"] = true
	if w := f.as("writer", "DELETE", "/acme/demo/api/packages/tags/x", nil); w.Code != 404 {
		t.Fatalf("feature-off delete = %d", w.Code)
	}
	delete(f.featOff, "acme/demo")

	// Method and shape edges.
	if w := f.as("reader", "POST", "/acme/demo/api/packages", nil); w.Code != 405 {
		t.Fatalf("post list = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/acme/demo/api/packages/a/b/c", nil); w.Code != 404 {
		t.Fatalf("deep path = %d", w.Code)
	}
	if w := f.as("writer", "DELETE", "/acme/demo/api/packages/not-a-digest", nil); w.Code != 400 {
		t.Fatalf("bad digest delete = %d", w.Code)
	}
	// Unknown repo and a non-packages lane are not ours.
	if w := f.as("read", "GET", "/acme/nope/api/packages", nil); w.Code != 404 {
		t.Fatalf("unknown repo = %d", w.Code)
	}
	if h := f.h.HandleAPI(httptest.NewRecorder(), httptest.NewRequest("GET", "/acme/demo/api/releases", nil), []string{"acme", "demo", "api", "releases"}); h {
		t.Fatal("releases path claimed by packages")
	}
}
