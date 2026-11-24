package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
)

// ---- seeding helpers --------------------------------------------------------

func put(t *testing.T, st store.ObjectStore, key string, raw []byte) {
	t.Helper()
	if _, err := store.PutBytes(context.Background(), st, key, raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

func putJSON(t *testing.T, st store.ObjectStore, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	put(t, st, key, raw)
}

func putManifest(t *testing.T, st store.ObjectStore, repo string) {
	t.Helper()
	m := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          repo,
		ObjectFormat:  "sha1",
		Revision:      1,
	}
	put(t, st, "repos/"+repo+"/manifest.pb", m.Marshal())
}

func putValidThread(t *testing.T, st store.ObjectStore, repo string, num int) {
	t.Helper()
	key := "repos/" + repo + "/issues/" + padHex(num) + "/thread.json"
	putJSON(t, st, key, map[string]any{
		"num": num, "kind": "issue", "title": "seed", "state": "open", "version": 1,
	})
}

func padHex(n int) string {
	const hex = "0123456789abcdef"
	var b [6]byte
	for i := 5; i >= 0; i-- {
		b[i] = hex[n&0xf]
		n >>= 4
	}
	return string(b[:])
}

// seedCleanStore writes one valid object of every sampled family plus the
// referential targets the orphan checks follow.
func seedCleanStore(t *testing.T) *store.Memory {
	t.Helper()
	st := store.NewMemory()
	putManifest(t, st, "acme/app")

	refs := &proto.RefSnapshot{
		Seq:          1,
		ObjectFormat: "sha1",
		Refs:         []*proto.Ref{{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)}},
		HeadTarget:   "refs/heads/main",
	}
	put(t, st, "repos/acme/app/checkpoints/0000000000000001/refs.pb", refs.Marshal())

	putJSON(t, st, "repos/acme/app/access.json", map[string]any{
		"version": 1, "visibility": "public", "role_bindings": []any{}, "updated_at": "2026-11-22T00:00:00Z",
	})
	putJSON(t, st, "users/alice/user.json", map[string]any{
		"version": 1, "username": "alice", "email": "a@example.com", "created_at": "2026-11-22T00:00:00Z",
	})
	putValidThread(t, st, "acme/app", 1)
	putJSON(t, st, "repos/acme/app/issues/000001/events/000000000001.json", map[string]any{
		"seq": 1, "type": "opened", "actor": "alice", "at": "2026-11-22T00:00:00Z",
	})
	putJSON(t, st, "repos/acme/app/pulls/000001/pr.json", map[string]any{
		"num": 1, "kind": "pr", "version": 1,
		"base": map[string]any{"repo": "acme/app", "ref": "refs/heads/main", "sha": strings.Repeat("b", 40)},
		"head": map[string]any{"repo": "acme/app", "ref": "refs/heads/topic", "sha": strings.Repeat("c", 40)},
	})
	putJSON(t, st, "repos/acme/app/packages/registry/manifests/sha256/"+strings.Repeat("d", 64), map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"digest": "sha256:" + strings.Repeat("e", 64)},
		"layers":        []any{map[string]any{"digest": "sha256:" + strings.Repeat("f", 64)}},
	})
	putJSON(t, st, "repos/acme/app/threads/runs/r1.json", map[string]any{
		"id": "r1", "repo": "acme/app", "event": "push", "state": "queued", "version": 1,
	})
	putJSON(t, st, "repos/acme/app/threads/runs/r1/jobs/j1.json", map[string]any{
		"id": "j1", "run_id": "r1", "repo": "acme/app", "state": "queued", "version": 1,
	})
	putJSON(t, st, "threads/queue/index.json", map[string]any{"version": 1, "queued": []any{}})

	putJSON(t, st, "repos/acme/app/meta/moderation/reports/index.json", map[string]any{
		"version": 1, "open": []any{map[string]any{"id": 1}}, "resolved_recent": []any{},
		"next_id": 2, "updated_at": "2026-11-22T00:00:00Z",
	})
	putJSON(t, st, "repos/acme/app/meta/moderation/reports/000000000001.json", map[string]any{
		"id": 1, "repo": "acme/app", "kind": "thread", "target": "thread:000001:event:000000000001",
		"reporter": "alice", "reason": "spam", "created_at": "2026-11-22T00:00:00Z",
	})
	putJSON(t, st, "moderation/reports/index.json", map[string]any{
		"version": 1, "open": []any{map[string]any{"id": 1, "repo": "acme/app"}}, "resolved_recent": []any{},
	})
	return st
}

// ---- tests ------------------------------------------------------------------

func TestCleanSeededStoreIsClean(t *testing.T) {
	rep, err := Run(context.Background(), seedCleanStore(t), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("findings = %+v, want none", rep.Findings)
	}
	if rep.Summary.ReposSampled != 1 || rep.Summary.ReposTotal != 1 {
		t.Fatalf("repos = %+v, want 1 of 1", rep.Summary)
	}
	// Every family was sampled at least once.
	if len(rep.Families) != len(families()) {
		t.Fatalf("families = %d, want %d", len(rep.Families), len(families()))
	}
	for _, f := range rep.Families {
		if f.Sampled == 0 {
			t.Errorf("family %s sampled 0 objects", f.Family)
		}
	}
}

func TestUndecodableObjectIsReportedWithKey(t *testing.T) {
	st := seedCleanStore(t)
	bad := "repos/acme/app/issues/000002/thread.json"
	put(t, st, bad, []byte(`{"num": 2, "kind": "issue", `)) // truncated JSON
	rep, err := Run(context.Background(), st, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, f := range rep.Findings {
		if f.Check == CheckDecode && f.Key == bad {
			found = true
			if !strings.Contains(f.Reason, "invalid JSON") {
				t.Errorf("reason = %q, want invalid JSON", f.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("no bucket.decode finding for %s in %+v", bad, rep.Findings)
	}
	if rep.Clean() {
		t.Fatal("report claims clean with a finding")
	}
}

func TestOrphanArtifactAndRunRecord(t *testing.T) {
	st := seedCleanStore(t)
	art := "repos/acme/app/threads/runs/gone/artifacts/build/manifest.json"
	putJSON(t, st, art, map[string]any{
		"name": "build", "run": "gone", "job": "j1", "files": []any{}, "total_size": 0,
	})
	rep, err := Run(context.Background(), st, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, f := range rep.Findings {
		if f.Check == CheckArtifact && f.Key == art && strings.Contains(f.Reason, "no run record") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no orphan.artifact for %s in %+v", art, rep.Findings)
	}
}

func TestOrphanQueueReportForkAndManifest(t *testing.T) {
	st := store.NewMemory()
	putManifest(t, st, "other/app")

	// A repo prefix with WAL history but no manifest: it cannot open.
	put(t, st, "repos/acme/app/log/0000000000000001.pb", []byte{0x00})

	// Global queue index row whose job record is gone.
	putJSON(t, st, "threads/queue/index.json", map[string]any{
		"version": 1,
		"queued":  []any{map[string]any{"repo": "acme/app", "run": "r9", "job": "j9"}},
	})

	// Global report index row with no immutable record.
	putJSON(t, st, "moderation/reports/index.json", map[string]any{
		"version": 1, "open": []any{map[string]any{"id": 7, "repo": "acme/app"}}, "resolved_recent": []any{},
	})

	// Child points at parent other/app; parent index omits it and lists a
	// ghost child that has no fork.json.
	putJSON(t, st, "repos/acme/app/fork.json", map[string]any{
		"parent": "other/app", "root": "other/app", "version": 1,
	})
	putJSON(t, st, "repos/other/app/meta/forks.json", map[string]any{
		"version": 1, "forks": []any{map[string]any{"repo": "acme/ghost"}},
	})

	rep, err := Run(context.Background(), st, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string]string{
		"repos/acme/app/manifest.pb":                               CheckManifest,
		"repos/acme/app/threads/runs/r9/jobs/j9.json":              CheckQueue,
		"repos/acme/app/meta/moderation/reports/000000000007.json": CheckReport,
		"repos/acme/ghost/fork.json":                               CheckFork,
	}
	for key, check := range want {
		found := false
		for _, f := range rep.Findings {
			if f.Check == check && f.Key == key {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s finding for %s in %+v", check, key, rep.Findings)
		}
	}
	// Both fork directions are reported: the child's parent index omits it.
	found := false
	for _, f := range rep.Findings {
		if f.Check == CheckFork && f.Key == "repos/acme/app/fork.json" && strings.Contains(f.Reason, "does not list this child") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no parent-index-omits-child finding in %+v", rep.Findings)
	}
}

func TestLimitReportsSampledMOfN(t *testing.T) {
	st := store.NewMemory()
	putManifest(t, st, "acme/app")
	for i := 1; i <= 5; i++ {
		putValidThread(t, st, "acme/app", i)
	}
	rep, err := Run(context.Background(), st, Options{Limit: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var collab *FamilyReport
	for i := range rep.Families {
		if rep.Families[i].Family == "collab" {
			collab = &rep.Families[i]
		}
	}
	if collab == nil {
		t.Fatalf("no collab family in %+v", rep.Families)
	}
	if collab.Sampled != 2 || collab.Total != 5 {
		t.Fatalf("collab = %+v, want sampled 2 of 5", collab)
	}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), "family collab: sampled 2 of 5") {
		t.Fatalf("text = %q, want sampled 2 of 5", buf.String())
	}
}

// recordingStore records every method name so the no-writes test can assert
// the doctor only reads.
type recordingStore struct {
	store.ObjectStore
	ops map[string]int
}

func (r *recordingStore) note(name string) { r.ops[name]++ }

func (r *recordingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	r.note("Get")
	return r.ObjectStore.Get(ctx, key, opts)
}
func (r *recordingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	r.note("Head")
	return r.ObjectStore.Head(ctx, key)
}
func (r *recordingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	r.note("Put")
	return store.ObjectMeta{}, errors.New("doctor wrote to the store")
}
func (r *recordingStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	r.note("Delete")
	return errors.New("doctor deleted from the store")
}
func (r *recordingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	r.note("List")
	return r.ObjectStore.List(ctx, prefix, startAfter, fn)
}
func (r *recordingStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	r.note("ListPrefixes")
	return r.ObjectStore.ListPrefixes(ctx, prefix, fn)
}
func (r *recordingStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	r.note("Compose")
	return store.ObjectMeta{}, errors.New("doctor composed in the store")
}

func TestDoctorNeverWrites(t *testing.T) {
	st := seedCleanStore(t)
	rec := &recordingStore{ObjectStore: st, ops: map[string]int{}}
	if _, err := Run(context.Background(), rec, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, mutating := range []string{"Put", "Delete", "Compose"} {
		if rec.ops[mutating] != 0 {
			t.Fatalf("%s called %d times; the doctor is read-only", mutating, rec.ops[mutating])
		}
	}
	if rec.ops["Get"] == 0 || rec.ops["List"] == 0 || rec.ops["ListPrefixes"] == 0 {
		t.Fatalf("ops = %v, want reads (Get/List/ListPrefixes)", rec.ops)
	}
}

func TestJSONShapeIsStable(t *testing.T) {
	rep, err := Run(context.Background(), seedCleanStore(t), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertKeys(t, "report", top, "version", "findings", "summary", "families")

	var summary map[string]json.RawMessage
	if err := json.Unmarshal(top["summary"], &summary); err != nil {
		t.Fatalf("summary: %v", err)
	}
	assertKeys(t, "summary", summary,
		"findings", "clean", "limit", "budget", "decoded", "budget_exhausted",
		"repos_sampled", "repos_total", "repos_truncated")

	var fams []map[string]json.RawMessage
	if err := json.Unmarshal(top["families"], &fams); err != nil {
		t.Fatalf("families: %v", err)
	}
	if len(fams) == 0 {
		t.Fatal("no families")
	}
	assertKeys(t, "family", fams[0], "family", "sampled", "total", "truncated", "findings")
}

func TestEmptyReportMarshalsArraysNotNull(t *testing.T) {
	raw, err := json.Marshal(NewReport(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"findings":[]`, `"families":[]`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("JSON = %s, want %s", raw, want)
		}
	}
}

func assertKeys(t *testing.T, what string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s keys = %d, want %d (%v)", what, len(got), len(want), keysOf(got))
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s missing key %q (have %v)", what, k, keysOf(got))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- config -----------------------------------------------------------------

func TestConfigFindingsClean(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Auth.Issuer = "https://id.example.com"
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.GroupsClaim = "groups"
	cfg.Server.Auth.AdminGroups = []string{"spindle-admin"}
	cfg.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	cfg.Server.Auth.OAuthClientID = "spindle"
	cfg.Server.Auth.OAuthClientSecret = "secret"
	if fs := ValidateConfig(cfg); len(fs) != 0 {
		t.Fatalf("findings = %+v, want none", fs)
	}
}

func TestConfigRemovedKeyIsReportedWithItsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	src := `
[server.auth]
mode = "none"
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
groups_claim = "groups"
admin_groups = ["spindle-admin"]
session_secret = "` + strings.Repeat("s", 32) + `"
oauth_client_id = "spindle"
oauth_client_secret = "secret"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyEnv := func(string) string { return "" }
	cfg, _, err := config.LoadExplicit(path, emptyEnv)
	if err == nil {
		t.Fatal("strict load accepted server.auth.mode")
	}
	if cfg != nil {
		t.Fatalf("strict load returned a config on the decode error: %+v", cfg)
	}
	best, errs := config.LoadSetupOnly(dir, []string{path}, emptyEnv)
	if best == nil {
		t.Fatal("LoadSetupOnly returned no best-effort config")
	}
	fs := LoadFindings(errs)
	want := false
	for _, f := range fs {
		if f.Check == CheckConfigKey && f.Key == "server.auth.mode" {
			want = true
		}
	}
	if !want {
		t.Fatalf("findings = %+v, want config.keys server.auth.mode", fs)
	}
	// The best-effort config is otherwise valid: the removed key is the one
	// thing the doctor must name (and the bucket checks could still run).
	if all := ValidateConfig(best); len(all) != 0 {
		t.Fatalf("ValidateConfig(best) = %+v, want none", all)
	}
}

func TestConfigKeyExtraction(t *testing.T) {
	cases := map[string]string{
		`server.auth.issuer is required — OIDC discovery needs it`:                  "server.auth.issuer",
		`unsupported key "server.auth.mode" (line 3): server.auth.mode was removed`: "server.auth.mode",
		`unknown key "store.bkuet" (line 9)`:                                        "store.bkuet",
		`bundles.strategy[0].name is required`:                                      "bundles.strategy[0].name",
		`negative value for server.request_timeout`:                                 "server.request_timeout",
		`OIDC requires server.auth.session_secret, server.auth.oauth_client_id`:     "server.auth.session_secret",
		`cache.dir must be an absolute path (got "relative")`:                       "cache.dir",
		`something with no key at all`:                                              "config",
	}
	for msg, want := range cases {
		if got := ConfigKey(msg); got != want {
			t.Errorf("ConfigKey(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestBudgetStopsDecoding(t *testing.T) {
	st := store.NewMemory()
	putManifest(t, st, "acme/app")
	for i := 1; i <= 10; i++ {
		putValidThread(t, st, "acme/app", i)
	}
	rep, err := Run(context.Background(), st, Options{Limit: 10, Budget: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Summary.BudgetExhausted {
		t.Fatalf("summary = %+v, want budget_exhausted", rep.Summary)
	}
	if rep.Summary.Decoded != 2 {
		t.Fatalf("decoded = %d, want 2", rep.Summary.Decoded)
	}
}

func TestValidateConfigWithLoad(t *testing.T) {
	// nil config + nil load error: nothing to say.
	if fs := ValidateConfigWithLoad(nil, nil); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
	// nil config + a load error is reported, never dropped.
	if fs := ValidateConfigWithLoad(nil, errors.New("disk on fire")); len(fs) != 1 || fs[0].Check != CheckConfigLoad {
		t.Fatalf("findings = %+v", fs)
	}
	// A load error that is exactly the joined validation messages is not
	// duplicated.
	cfg := config.Defaults()
	warnings, errs := config.Validate(cfg)
	joined := make([]string, len(errs))
	for i, e := range errs {
		joined[i] = e.Error()
	}
	fs := ValidateConfigWithLoad(cfg, errors.New(strings.Join(joined, "; ")))
	if len(fs) != len(ConfigFindings(warnings, errs)) {
		t.Fatalf("duplicated load error: %+v", fs)
	}
	// A distinct loader error (env overlay) is added even though Validate is
	// clean.
	valid := config.Defaults()
	valid.Server.Auth.Issuer = "https://id.example.com"
	valid.Server.Auth.AllowedDomains = []string{"example.com"}
	valid.Server.Auth.GroupsClaim = "groups"
	valid.Server.Auth.AdminGroups = []string{"spindle-admin"}
	valid.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	valid.Server.Auth.OAuthClientID = "spindle"
	valid.Server.Auth.OAuthClientSecret = "secret"
	fs = ValidateConfigWithLoad(valid, errors.New("SPINDLE__SERVER__AUTH__MODE: server.auth.mode was removed"))
	if len(fs) != 1 || fs[0].Key != "server.auth.mode" {
		t.Fatalf("findings = %+v, want the env overlay key", fs)
	}
}

func TestRemovedEnvKeyIsAFinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	src := `
[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
groups_claim = "groups"
admin_groups = ["spindle-admin"]
session_secret = "` + strings.Repeat("s", 32) + `"
oauth_client_id = "spindle"
oauth_client_secret = "secret"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPINDLE__SERVER__AUTH__MODE", "none")
	cfg, _, err := config.LoadExplicit(path, os.Getenv)
	if err == nil {
		t.Fatal("the env overlay was accepted")
	}
	if cfg == nil {
		t.Fatal("Load returned no config for an env-overlay error")
	}
	fs := ValidateConfigWithLoad(cfg, err)
	found := false
	for _, f := range fs {
		if f.Key == "server.auth.mode" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want server.auth.mode", fs)
	}
}

func TestOverrideFindings(t *testing.T) {
	fs := OverrideFindings([]config.Override{
		{Name: "SPINDLE__STORE__BKUET", Key: "store.bkuet", Reason: "unknown key store.bkuet"},
		{Name: "WALGIT__STORE__BUCKET", Key: "store.bucket", Reason: "overridden by SPINDLE__STORE__BUCKET"},
	})
	if len(fs) != 1 || fs[0].Key != "store.bkuet" || fs[0].Check != CheckConfigKey {
		t.Fatalf("findings = %+v", fs)
	}
	if got := OverrideFindings([]config.Override{{Name: "SPINDLE__X", Reason: "unknown key x"}}); len(got) != 1 || got[0].Key != "config" {
		t.Fatalf("keyless override = %+v", got)
	}
}

func TestIgnoredEnvTypoIsFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	src := `
[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
groups_claim = "groups"
admin_groups = ["spindle-admin"]
session_secret = "` + strings.Repeat("s", 32) + `"
oauth_client_id = "spindle"
oauth_client_secret = "secret"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPINDLE__STORE__BKUET", "typo")
	_, overrides, err := config.LoadExplicit(path, os.Getenv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fs := OverrideFindings(overrides)
	if len(fs) != 1 || fs[0].Key != "store.bkuet" {
		t.Fatalf("findings = %+v, want store.bkuet", fs)
	}
}
