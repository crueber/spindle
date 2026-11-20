// settings_seam_test.go — pins the non-HTTP settings seam the server-rendered
// pages consume (DescribeBody/PublishSettings/DeleteSettings/PutPolicy/
// PolicyRaw/DryRunPolicy). The JSON handlers call the same functions, so a
// page and its API twin cannot drift.
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/policy"
	"github.com/crueber/walhub/internal/store"
)

// seamExpander is a policy.Expander that resolves nothing (and says so).
type seamExpander struct{}

func (seamExpander) ExpandGroups(context.Context, []string) ([]string, []string) {
	return nil, []string{"team:gone/x resolved to nobody"}
}

func seamEnv(t *testing.T) *Env {
	t.Helper()
	cfg := config.Defaults()
	cfg.Bundles.Strategy = []config.BundleStrategy{{
		Name: "daily", Kind: "daily", Schedule: "0 0 3 * * *", Keep: 2,
	}}
	cfg.Server.Roles = []string{"serve"}
	fv := newFakeView()
	env := &Env{Store: store.NewMemory(), Repo: fv, Repos: &fakeRegistry{repos: map[string][]string{"acme": {"repo"}}, created: map[string]bool{}}, Cfg: cfg, Hostname: "node-1"}
	env.Ready()
	return env
}

type seamSettingsErrView struct{ *fakeView }

func (seamSettingsErrView) Settings(context.Context, git.RepoId) (SettingsDoc, error) {
	return SettingsDoc{}, ErrNotFound
}

type seamBrokenSettings struct{ *fakeView }

func (seamBrokenSettings) Settings(context.Context, git.RepoId) (SettingsDoc, error) {
	return SettingsDoc{TOML: "[server]\nlisten = \":1\"\n"}, nil
}

type seamErrView struct{ *fakeView }

func (seamErrView) PublishSettings(context.Context, git.RepoId, []byte, string, string) (uint64, error) {
	return 0, ErrExists
}

func TestSettingsDescribeSeam(t *testing.T) {
	env := seamEnv(t)
	id := git.RepoId{Owner: "acme", Name: "repo"}
	body, err := SettingsDescribe(context.Background(), env, id)
	if err != nil {
		t.Fatalf("SettingsDescribe: %v", err)
	}
	if len(body.Strategies) != 1 || body.Strategies[0].Name != "daily" || body.Strategies[0].Next == "" {
		t.Fatalf("strategies = %+v", body.Strategies)
	}
	if body.Maintenance.ThisHost.Name != "node-1" || !body.Maintenance.ThisHost.Serves {
		t.Fatalf("host facts = %+v", body.Maintenance.ThisHost)
	}
	if body.Sections[0] != "bundles" {
		t.Fatalf("sections = %v", body.Sections)
	}
	// A RepoView error is returned verbatim (the handler maps it).
	bad := &Env{Repo: seamSettingsErrView{newFakeView()}, Cfg: env.Cfg}
	if _, err := SettingsDescribe(context.Background(), bad, id); err == nil {
		t.Fatal("unknown repo must error")
	}
	// A broken stored document surfaces the merge error.
	broken := &Env{Repo: seamBrokenSettings{newFakeView()}, Cfg: env.Cfg}
	if _, err := SettingsDescribe(context.Background(), broken, id); err == nil {
		t.Fatal("broken settings must error")
	}
}

func TestPublishSettingsSeam(t *testing.T) {
	env := seamEnv(t)
	id := git.RepoId{Owner: "acme", Name: "repo"}
	rev, werr := PublishSettings(context.Background(), env, id, []byte("description = \"hi\"\n"), "why", "jane")
	if werr != nil || rev == 0 {
		t.Fatalf("publish = %d %+v", rev, werr)
	}
	if werr := (&WriteError{Status: 0}); werr.Status != 0 {
		t.Fatal("WriteError must be a plain struct")
	}
	if _, werr := PublishSettings(context.Background(), env, id, []byte("[server]\nlisten = \":1\"\n"), "", "jane"); werr == nil || werr.Status != http.StatusBadRequest {
		t.Fatalf("host-only section = %+v", werr)
	}
	big := []byte(strings.Repeat("x", config.MaxRepoSettingsBytes+1))
	if _, werr := PublishSettings(context.Background(), env, id, big, "", "jane"); werr == nil || werr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %+v", werr)
	}
	if _, werr := DeleteSettings(context.Background(), env, id, "jane"); werr != nil {
		t.Fatalf("delete = %+v", werr)
	}
	if _, werr := PublishSettings(context.Background(), &Env{}, id, nil, "", "jane"); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unwired publish = %+v", werr)
	}
	if _, werr := DeleteSettings(context.Background(), &Env{}, id, "jane"); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unwired delete = %+v", werr)
	}
	// viewError's 409 branch (a PublishSettings race surfaced as exists).
	errEnv := &Env{Repo: seamErrView{newFakeView()}}
	if _, werr := PublishSettings(context.Background(), errEnv, id, nil, "", "jane"); werr == nil || werr.Status != http.StatusConflict {
		t.Fatalf("exists = %+v", werr)
	}
	if _, werr := PublishSettings(context.Background(), errEnv, id, []byte("description = \"x\"\n"), "", "jane"); werr == nil {
		t.Fatal("invalid body must win over the write")
	}
}

func TestPolicySeam(t *testing.T) {
	env := seamEnv(t)
	id := git.RepoId{Owner: "acme", Name: "repo"}
	ctx := context.Background()
	raw, _, err := PolicyRaw(ctx, env, id)
	if err != nil {
		t.Fatalf("PolicyRaw: %v", err)
	}
	if !strings.Contains(string(raw), `"version":1`) {
		t.Fatalf("default policy = %s", raw)
	}
	const doc = `{"version":1,"groups":[],"rules":[{"name":"p","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}`
	if werr := PutPolicy(ctx, env, id, []byte(doc)); werr != nil {
		t.Fatalf("PutPolicy: %+v", werr)
	}
	// PolicyOf is the parsed-document read the merge box uses.
	if parsed, perr := PolicyOf(ctx, env, id); perr != nil || parsed == nil || len(parsed.Rules) != 1 {
		t.Fatalf("PolicyOf = %+v err %v", parsed, perr)
	}
	if raw, _, err := PolicyRaw(ctx, env, id); err != nil || string(raw) != doc {
		t.Fatalf("raw = %s err %v", raw, err)
	}
	if werr := PutPolicy(ctx, env, id, []byte("{bad")); werr == nil || werr.Status != http.StatusBadRequest {
		t.Fatalf("invalid = %+v", werr)
	}
	if werr := PutPolicy(ctx, env, id, make([]byte, 1<<20+1)); werr == nil || werr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %+v", werr)
	}

	env.Repo.(*fakeView).pushes[id.String()] = []PushRecord{
		{Seq: 1, Principal: "bob", Refs: []PushRef{{Name: "refs/heads/main", Old: "a", New: "b"}}},
		{Seq: 2, Principal: "bob", Refs: []PushRef{{Name: "refs/heads/main", Old: "b", New: "c", Force: true}}},
	}
	res, werr := DryRunPolicy(ctx, env, id, []byte(doc), 10)
	if werr != nil || res.Pushes != 2 || res.Allowed != 1 || res.Denied != 1 {
		t.Fatalf("dry run = %+v %+v", res, werr)
	}
	if len(res.Results) != 2 || !res.Results[1].Refs[0].OK == false {
		t.Fatalf("dry-run results = %+v", res.Results)
	}
	// Fail closed on an unparseable candidate, and on a bad history read.
	if _, werr := DryRunPolicy(ctx, env, id, []byte("{bad"), 10); werr == nil || werr.Status != http.StatusBadRequest {
		t.Fatalf("bad candidate = %+v", werr)
	}
	if _, werr := DryRunPolicy(ctx, &Env{}, id, nil, 10); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unwired dry run = %+v", werr)
	}
	if _, werr := DryRunPolicy(ctx, env, id, nil, -5); werr != nil {
		t.Fatalf("clamped last = %+v", werr)
	}
	// Seam 3 expansion warnings ride the page note.
	withGroup := `{"version":1,"groups":[],"rules":[{"name":"p","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"],"bypass":["team:gone/x"]}}}]}`
	env.GroupExpander = policy.Expander(seamExpander{})
	if res, werr := DryRunPolicy(ctx, env, id, []byte(withGroup), 10); werr != nil || len(res.Warnings) == 0 {
		t.Fatalf("expanded dry run = %+v %+v", res, werr)
	}
	env.GroupExpander = nil
	// A corrupt stored document is the handler's fail-closed 503.
	_, curVer, _ := PolicyRaw(ctx, env, id)
	if _, err := env.Store.Put(ctx, policyKey(id), store.PutBody{Bytes: []byte("{bad")},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: curVer}); err != nil {
		t.Fatalf("seed corrupt policy: %v", err)
	}
	if raw, _, err := PolicyRaw(ctx, env, id); err == nil || string(raw) != "{bad" {
		t.Fatalf("corrupt PolicyRaw = %s err %v", raw, err)
	}
	if werr := PutPolicy(ctx, env, id, []byte(doc)); werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("put over corrupt = %+v", werr)
	}
}

func TestViewErrorSeam(t *testing.T) {
	if got := viewError(ErrNotFound); got.Status != http.StatusNotFound {
		t.Fatalf("not found = %+v", got)
	}
	if got := viewError(ErrExists); got.Status != http.StatusConflict {
		t.Fatalf("exists = %+v", got)
	}
	if got := viewError(errors.New("boom")); got.Status != http.StatusServiceUnavailable {
		t.Fatalf("boom = %+v", got)
	}
}

// TestSettingsDescribeHandlerErrorMapping pins the HTTP twin of
// SettingsDescribe: a missing repo is 404 and a broken document is 503, the
// exact statuses the pre-seam handler wrote.
func TestSettingsDescribeHandlerErrorMapping(t *testing.T) {
	f := newFixture(t)
	// The describe route is repo-scoped under the demo hello fixture.
	f.view.published["demo/hello"] = []byte("[server]\nlisten = \":1\"\n")
	if w := f.req("GET", "/demo/hello/api/settings/describe"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken settings describe = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	// Seed a valid document: the describe shape carries the strategies.
	f.view.published["demo/hello"] = []byte("description = \"hi\"\n")
	if w := f.req("GET", "/demo/hello/api/settings/describe"); w.Code != http.StatusOK {
		t.Fatalf("describe = %d (%s)", w.Code, w.Body.String())
	}
	// An unknown repo surfaces the RepoView error through the same mapping.
	f.env.Repo = seamSettingsErrView{f.view}
	if w := f.req("GET", "/demo/hello/api/settings/describe"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown repo describe = %d, want 404", w.Code)
	}
}

// TestSettingsSeamHandlerBranches covers the handler-side mappings the seam
// now shares: the 413 body budget, the unwired-view 503, and the policy 400s.
func TestSettingsSeamHandlerBranches(t *testing.T) {
	f := newFixture(t)
	big := strings.NewReader(strings.Repeat("x", config.MaxRepoSettingsBytes+1))
	if w := f.req("PUT", "/demo/hello/api/settings", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize settings PUT = %d, want 413", w.Code)
	}
	bad := strings.NewReader("[server]\nlisten = \":1\"\n")
	if w := f.req("PUT", "/demo/hello/api/settings", bad); w.Code != http.StatusBadRequest {
		t.Fatalf("host-only settings PUT = %d, want 400", w.Code)
	}
	f.env.Repo = nil
	if w := f.req("DELETE", "/demo/hello/api/settings"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired settings DELETE = %d, want 503", w.Code)
	}
	f.env.Repo = f.view
	if w := f.req("PUT", "/demo/hello/api/policy", strings.NewReader("{bad")); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy PUT = %d, want 400", w.Code)
	}
	if w := f.req("POST", "/demo/hello/api/policy/dry-run", strings.NewReader("{bad")); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid dry-run = %d, want 400", w.Code)
	}
	if w := f.req("GET", "/demo/hello/api/policy"); w.Code != http.StatusOK {
		t.Fatalf("policy GET = %d", w.Code)
	}
	// A corrupt stored document is the fail-closed 503, and nothing is
	// silently overwritten by a PUT over it.
	if _, err := f.env.Store.Put(context.Background(), "repos/demo/hello/policy.json",
		store.PutBody{Bytes: []byte("{bad")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed corrupt policy: %v", err)
	}
	if w := f.req("GET", "/demo/hello/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt policy GET = %d, want 503", w.Code)
	}
	if w := f.req("DELETE", "/demo/hello/api/policy"); w.Code != http.StatusNoContent {
		t.Fatalf("policy DELETE = %d", w.Code)
	}
	if w := f.req("GET", "/demo/hello/api/settings/history"); w.Code != http.StatusOK {
		t.Fatalf("history GET = %d", w.Code)
	}
	if w := f.req("GET", "/demo/hello/api/settings/effective"); w.Code != http.StatusOK {
		t.Fatalf("effective GET = %d", w.Code)
	}
	if w := f.req("POST", "/demo/hello/api/settings/validate", strings.NewReader("[server]\nlisten = \":1\"\n")); w.Code != http.StatusOK {
		t.Fatalf("validate = %d", w.Code)
	}
	if w := f.req("PUT", "/demo/hello/api/settings", strings.NewReader("description = \"ok\"\n")); w.Code != http.StatusOK {
		t.Fatalf("valid settings PUT = %d", w.Code)
	}
	f.view.published["demo/hello"] = []byte("[server]\nlisten = \":1\"\n")
	if w := f.req("GET", "/demo/hello/api/settings/effective"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken effective = %d, want 503", w.Code)
	}
	f.view.published["demo/hello"] = []byte("description = \"ok\"\n")
	if w := f.req("POST", "/demo/hello/api/policy/validate", strings.NewReader("{bad")); w.Code != http.StatusOK {
		t.Fatalf("policy validate = %d", w.Code)
	}

}

// seamKeys is the SSH-key registry fake for the handler branches the seam
// work touched collaterally (fail-closed mapping stays covered).
type seamKeys struct{ err error }

func (k seamKeys) List(context.Context, string) ([]SshKeyRecord, error) {
	if k.err != nil {
		return nil, k.err
	}
	return nil, nil // nil → [] on the wire
}
func (k seamKeys) Add(context.Context, string, string, string) (SshKeyRecord, error) {
	return SshKeyRecord{}, k.err
}
func (k seamKeys) Get(context.Context, string, string) (SshKeyRecord, error) {
	return SshKeyRecord{}, k.err
}
func (k seamKeys) Delete(context.Context, string, string) error { return k.err }

func TestSSHKeySeamBranches(t *testing.T) {
	f := newFixture(t)
	f.env.SSHKeys = seamKeys{}
	if w := f.req("GET", "/api/v1/ssh-keys"); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty keys = %d %q", w.Code, w.Body.String())
	}
	f.env.SSHKeys = seamKeys{err: errors.New("registry down")}
	if w := f.req("GET", "/api/v1/ssh-keys"); w.Code != http.StatusInternalServerError {
		t.Fatalf("list error = %d", w.Code)
	}
	f.env.SSHKeys = seamKeys{err: ErrKeyForbidden}
	if w := f.req("DELETE", "/api/v1/ssh-keys/fp1"); w.Code != http.StatusForbidden {
		t.Fatalf("foreign key = %d", w.Code)
	}
	f.env.SSHKeys = seamKeys{err: ErrKeyNotFound}
	if w := f.req("DELETE", "/api/v1/ssh-keys/fp1"); w.Code != http.StatusNotFound {
		t.Fatalf("missing key = %d", w.Code)
	}
	if !isKeyFormatError(errors.New("not a valid authorized_keys line")) || isKeyFormatError(nil) {
		t.Fatal("isKeyFormatError")
	}
}
