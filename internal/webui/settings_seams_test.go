package webui

// settings_seams_test.go — the repo-settings seam (settings_seams.go) over
// REAL feature services (identity, checks, notify, mirror, pushmirror) on the
// memory store, plus the page paths those seams enable. The fakes stay at the
// api.Env boundary only (RepoView/RepoRegistry), exactly where composition
// injects the WAL-backed implementation.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/mirror"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/pushmirror"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- api.Env fakes (the composition boundary) -------------------------------

type seamsRepo struct {
	doc        api.SettingsDoc
	hist       []api.SettingsEntry
	pushes     []api.PushRecord
	published  []byte
	publishMsg string
	publishErr error
}

func (f *seamsRepo) Sync(context.Context, git.RepoId, api.SyncLevel) error { return nil }
func (f *seamsRepo) Resolve(context.Context, git.RepoId, string) (api.Resolution, error) {
	return api.Resolution{}, nil
}
func (f *seamsRepo) Head(context.Context, git.RepoId) (api.Ref, bool, error) {
	return api.Ref{}, false, nil
}
func (f *seamsRepo) RefList(context.Context, git.RepoId, string, api.RefQuery) ([]api.Ref, bool, error) {
	return nil, false, nil
}
func (f *seamsRepo) Tree(context.Context, git.RepoId, string, string) (api.TreeResult, error) {
	return api.TreeResult{}, nil
}
func (f *seamsRepo) Blob(context.Context, git.RepoId, string, string, bool) (api.BlobResult, error) {
	return api.BlobResult{}, nil
}
func (f *seamsRepo) Commits(context.Context, git.RepoId, string, string, int, int) (api.CommitPage, error) {
	return api.CommitPage{}, nil
}
func (f *seamsRepo) Commit(context.Context, git.RepoId, string) (api.CommitDetail, error) {
	return api.CommitDetail{}, nil
}
func (f *seamsRepo) Summary(context.Context, git.RepoId) (api.SummaryData, error) {
	return api.SummaryData{}, nil
}
func (f *seamsRepo) Overview(context.Context, git.RepoId) (api.OverviewData, error) {
	return api.OverviewData{}, nil
}
func (f *seamsRepo) Settings(context.Context, git.RepoId) (api.SettingsDoc, error) {
	return f.doc, nil
}
func (f *seamsRepo) PublishSettings(_ context.Context, _ git.RepoId, body []byte, msg, _ string) (uint64, error) {
	if f.publishErr != nil {
		return 0, f.publishErr
	}
	f.published = append([]byte(nil), body...)
	f.publishMsg = msg
	f.doc.TOML = string(body)
	return 7, nil
}
func (f *seamsRepo) SettingsHistory(context.Context, git.RepoId) (api.SettingsHistory, error) {
	return api.SettingsHistory{Entries: f.hist}, nil
}
func (f *seamsRepo) HeadSeq(context.Context, git.RepoId) (uint64, error) { return 42, nil }
func (f *seamsRepo) PushHistory(context.Context, git.RepoId, int) ([]api.PushRecord, error) {
	return f.pushes, nil
}

type seamsRegistry struct {
	deleted []string
}

func (f *seamsRegistry) Owners(context.Context) ([]string, error) { return []string{"acme"}, nil }
func (f *seamsRegistry) Repos(context.Context, string) ([]string, error) {
	return []string{"monorepo"}, nil
}
func (f *seamsRegistry) OwnerRepoCounts(context.Context) (map[string]int, error) {
	return map[string]int{"acme": 1}, nil
}
func (f *seamsRegistry) Exists(context.Context, git.RepoId) (bool, error) { return true, nil }
func (f *seamsRegistry) Create(context.Context, git.RepoId, git.ObjectFormat) error {
	return nil
}
func (f *seamsRegistry) Delete(_ context.Context, id git.RepoId) error {
	f.deleted = append(f.deleted, id.String())
	return nil
}

// --- harness ----------------------------------------------------------------

type seamsHarness struct {
	s     *Server
	store *store.Memory
	apiE  *api.Env
	repo  *seamsRepo
	reg   *seamsRegistry
	ident *identity.Service
	notes *notify.Service
	cks   *checks.Service
	deps  SettingsDeps
}

func newSeamsHarness(t *testing.T) *seamsHarness {
	t.Helper()
	cmd := config.Defaults()
	cmd.Server.Auth.Mode = "none"
	cmd.Server.Auth.AnonymousRead = true
	cmd.Server.Roles = []string{"serve", "maintain"}
	cmd.Import.URLAllowlist = []string{"example.com"}
	cmd.Bundles.Strategy = []config.BundleStrategy{{
		Name: "daily", Kind: "daily", Base: "heads/main",
		Schedule: "0 0 3 * * *", Keep: 3, Filter: "blob:none", Refs: []string{"refs/heads/main"},
	}}

	mem := store.NewMemory()
	ident := identity.New(mem, cmd)
	notes := notify.New(mem, ident)
	cks := checks.New(mem, ident)
	mirrorSvc := mirror.New(mirror.Deps{Store: mem, Allowlist: []string{"example.com"}, Now: func() time.Time {
		return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	}})
	pushSvc := pushmirror.New(pushmirror.Deps{Store: mem, Allowlist: []string{"example.com"}, Now: func() time.Time {
		return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	}})
	repo := &seamsRepo{}
	reg := &seamsRegistry{}
	apiEnv := &api.Env{Store: mem, Repo: repo, Repos: reg, Cfg: cmd, Hostname: "node-1",
		Now: func() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) }}

	deps := SettingsDeps{}
	deps.BindSettingsServices(SettingsServices{
		API: apiEnv, Ident: ident, Checks: cks, Notify: notes,
		Mirror: &mirror.Handler{Svc: mirrorSvc},
		Push:   &pushmirror.Handler{Svc: pushSvc},
		Now:    func() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) },
	})

	f := &settingsFakes{deps: deps}
	s := newSettingsServer(t, f)
	return &seamsHarness{s: s, store: mem, apiE: apiEnv, repo: repo, reg: reg,
		ident: ident, notes: notes, cks: cks, deps: deps}
}

func seamsGet(t *testing.T, h *seamsHarness, target string) (int, string) {
	t.Helper()
	rec := settingsDo(t, h.s, http.MethodGet, target, nil, nil)
	return rec.Code, rec.Body.String()
}

func seamsPost(t *testing.T, h *seamsHarness, target string, form url.Values) (int, string, string) {
	t.Helper()
	rec := settingsDo(t, h.s, http.MethodPost, target, form, nil)
	return rec.Code, rec.Header().Get("Location"), rec.Body.String()
}

func seamAdmin() auth.Principal {
	return auth.Principal{Name: "alice", Email: "alice@example.com", Write: true, Admin: true}
}

// --- Access ----------------------------------------------------------------

func TestSettingsSeamsAccessReadAndSave(t *testing.T) {
	h := newSeamsHarness(t)

	code, body := seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("GET access = %d (%s)", code, body)
	}
	settingsMustContain(t, body, "Role bindings", "access")
	settingsMustContain(t, body, "no bindings", "access")
	settingsMustContain(t, body, `name="version" value="0"`, "access CAS version")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/access", url.Values{
		"version":     {"0"},
		"visibility":  {"private"},
		"new_subject": {"user:bob@example.com"},
		"new_role":    {"write"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("POST access = %d (%s)", code, body)
	}
	settingsMustContain(t, loc, "/settings/access", "access redirect")

	doc, _, err := h.ident.GetAccess(context.Background(), "acme", "monorepo")
	if err != nil {
		t.Fatal(err)
	}
	if string(doc.Visibility) != "private" || len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "user:bob@example.com" {
		t.Fatalf("access doc = %+v", doc)
	}

	// A stale CAS version is the API's 409, rendered inline with the typed
	// form (never a redirect, never a silent last-writer win).
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/access", url.Values{
		"version":    {"0"},
		"visibility": {"public"},
	})
	if code != http.StatusConflict {
		t.Fatalf("stale access save = %d, want 409 (%s)", code, body)
	}
	settingsMustContain(t, body, "changed under you", "stale access save")

	// Teams come from the owner org (the picker's source).
	if org, err := h.ident.GetOrg(context.Background(), "acme"); err == nil && org != nil {
		t.Fatal("acme must not be an org yet")
	}
}

func TestSettingsSeamsAccessTeamPickerForOrg(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if _, err := h.ident.CreateOrg(ctx, "acme", "Acme", "", "alice"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := h.ident.PutAccess(ctx, "acme", "monorepo", "", identity.VisibilityAuthenticated, nil); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	code, body := seamsGet(t, h, "/acme/monorepo/settings/access")
	if code != http.StatusOK {
		t.Fatalf("GET access = %d", code)
	}
	settingsMustContain(t, body, "org members", "org visibility wording")
	// Add a binding through the team picker spelling.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/access", url.Values{
		"version":    {"1"},
		"new_team":   {"platform"},
		"new_role":   {"triage"},
		"visibility": {"authenticated"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("team binding save = %d (%s)", code, body)
	}
	doc, _, _ := h.ident.GetAccess(ctx, "acme", "monorepo")
	if len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "team:acme/platform" {
		t.Fatalf("bindings = %+v", doc.RoleBindings)
	}
}

func TestSettingsSeamsWriteAuthority(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if _, err := h.ident.PutAccess(ctx, "acme", "monorepo", "", identity.VisibilityPrivate,
		[]identity.AccessBinding{{Subject: "user:carol@example.com", Role: identity.RoleRead}}); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	carol := auth.Principal{Name: "carol@example.com", Email: "carol@example.com"}
	_, werr := h.deps.RepoWrite(ctx, "acme", "monorepo", carol, SettingsRepoWrite{
		Action: SettingsActionPolicy, Values: map[string]string{"policy": `{"version":1,"rules":[]}`},
	})
	if werr == nil || werr.Status != http.StatusForbidden {
		t.Fatalf("non-admin write = %+v, want 403", werr)
	}
	// The page-level gate refuses the tab before the seam runs.
	h.s.auth = pageAuthFor(carol)
	rec := settingsDo(t, h.s, http.MethodGet, "/acme/monorepo/settings/policy", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin policy tab = %d, want 403", rec.Code)
	}
}

// --- Policy ----------------------------------------------------------------

func TestSettingsSeamsPolicyRawSaveAndDryRun(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "monorepo"}
	const policy = `{"version":1,"groups":[],"rules":[{"name":"protect-default","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}`
	if werr := api.PutPolicy(ctx, h.apiE, id, []byte(policy)); werr != nil {
		t.Fatalf("PutPolicy: %v", werr)
	}
	code, body := seamsGet(t, h, "/acme/monorepo/settings/policy")
	if code != http.StatusOK {
		t.Fatalf("GET policy = %d", code)
	}
	settingsMustContain(t, body, "protect-default", "raw policy text")
	settingsMustContain(t, body, "dry-run", "dry-run control")

	// Invalid JSON: the API's 400 mapping re-renders with the typed body.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/policy", url.Values{
		"policy": {"{not json"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid policy save = %d, want 400 (%s)", code, body)
	}
	settingsMustContain(t, body, "{not json", "typed policy value")
	settingsMustContain(t, body, "policy", "inline error")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/policy", url.Values{
		"policy": {`{"version":1,"groups":[],"rules":[]}`},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/settings/policy") {
		t.Fatalf("policy save = %d %q (%s)", code, loc, body)
	}
	raw, _, _ := api.PolicyRaw(ctx, h.apiE, id)
	if !strings.Contains(string(raw), `"rules":[]`) {
		t.Fatalf("stored policy = %s", raw)
	}

	// Dry run against the last pushes: two allowed refs, one denied.
	h.repo.pushes = []api.PushRecord{
		{Seq: 11, Principal: "bob", At: time.Now(), Refs: []api.PushRef{{Name: "refs/heads/main", Old: "a", New: "b"}}},
		{Seq: 12, Principal: "bob", At: time.Now(), Refs: []api.PushRef{{Name: "refs/heads/main", Old: "b", New: "c", Force: true}}},
	}
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/policy", url.Values{
		"policy_action": {"dryrun"},
		"policy":        {policy},
		"n":             {"10"},
	})
	if code != http.StatusOK {
		t.Fatalf("dry run = %d (%s)", code, body)
	}
	settingsMustContain(t, body, "Dry run:", "dry-run result")
	settingsMustContain(t, body, "rejected by rule", "dry-run denial")
	settingsMustContain(t, body, "#12 refs/heads/main", "dry-run row")

	// An unparseable candidate never evaluates (fail closed).
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/policy", url.Values{
		"policy_action": {"dryrun"},
		"policy":        {"{bad"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad dry run = %d, want 400 (%s)", code, body)
	}
}

// --- Config history --------------------------------------------------------

func TestSettingsSeamsConfigHistoryPublishRevert(t *testing.T) {
	h := newSeamsHarness(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	h.repo.doc = api.SettingsDoc{Revision: 3, Author: "alice", TOML: "description = \"current\"\n[features]\nissues = false\n"}
	h.repo.hist = []api.SettingsEntry{
		{Seq: 1, Revision: 1, Author: "alice", Message: "first", At: now, TOML: "description = \"first\"\n"},
		{Seq: 2, Revision: 2, Author: "bob", Message: "second", At: now.Add(time.Hour), TOML: "description = \"second\"\n"},
	}
	code, body := seamsGet(t, h, "/acme/monorepo/settings/config")
	if code != http.StatusOK {
		t.Fatalf("GET config = %d", code)
	}
	settingsMustContain(t, body, "&#34;current&#34;", "effective editor")
	settingsMustContain(t, body, "#2", "history revision")
	settingsMustContain(t, body, "second", "history message")
	settingsMustContain(t, body, "- description", "history diff")
	settingsMustContain(t, body, "revert to this in the editor", "revert link")

	// Revert is a read: the revision's document seeds the editor.
	code, body = seamsGet(t, h, "/acme/monorepo/settings/config?rev=2")
	if code != http.StatusOK {
		t.Fatalf("GET config?rev=2 = %d", code)
	}
	settingsMustContain(t, body, "&#34;second&#34;", "revert seeds editor")
	settingsMustContain(t, body, "loaded #2 into the editor", "revert notice")

	// Publish validates then publishes (the same functions as the API PUT).
	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/config", url.Values{
		"config":  {"description = \"next\"\n"},
		"message": {"why"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "notice=") {
		t.Fatalf("publish = %d %q (%s)", code, loc, body)
	}
	if string(h.repo.published) != "description = \"next\"\n" || h.repo.publishMsg != "why" {
		t.Fatalf("published = %q msg %q", h.repo.published, h.repo.publishMsg)
	}

	// An invalid document is the API's 400, values restored.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/config", url.Values{
		"config": {"[server]\nlisten = \":1\"\n"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid publish = %d, want 400 (%s)", code, body)
	}
	settingsMustContain(t, body, "[server]", "typed invalid document")

	// Clear publishes the empty body (back to host config).
	code, loc, body = seamsPost(t, h, "/acme/monorepo/settings/config", url.Values{
		"config_action": {"clear"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("clear = %d (%s)", code, body)
	}
	if len(h.repo.published) != 0 {
		t.Fatalf("clear published %q, want empty", h.repo.published)
	}
}

func TestSettingsSeamsGeneralSavePreservesSections(t *testing.T) {
	h := newSeamsHarness(t)
	h.repo.doc = api.SettingsDoc{TOML: "description = \"old\"\n\n[bundles]\nmain_only = true\n"}
	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/general", url.Values{
		"description":         {"new"},
		"allow_self_approval": {"true"},
		"feature_issues":      {"false", "true"},
		"feature_pulls":       {"false"},
		"feature_releases":    {"true"},
		"feature_forks":       {"true"},
		"feature_watch":       {"true"},
		"feature_star":        {"true"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/settings/general") {
		t.Fatalf("general save = %d %q (%s)", code, loc, body)
	}
	got := string(h.repo.published)
	settingsMustContain(t, got, "description = ", "description replaced")
	if !strings.Contains(got, "new") {
		t.Fatalf("new description missing:\n%s", got)
	}
	settingsMustContain(t, got, "[bundles]\nmain_only = true", "other sections preserved")
	settingsMustContain(t, got, "[features]\nissues = true", "checkbox pair resolves to true")
	settingsMustContain(t, got, "pulls = false", "unchecked feature")
	settingsMustContain(t, got, "[review]\nallow_self_approval = true", "review section")

	// A multi-line description is refused client-shape before publishing.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/general", url.Values{
		"description": {"a\nb"},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("multi-line description = %d, want 422 (%s)", code, body)
	}
	settingsMustContain(t, body, "single line", "description error")
}

func TestSettingsSeamsScheduledDescribe(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/scheduled")
	if code != http.StatusOK {
		t.Fatalf("GET scheduled = %d", code)
	}
	settingsMustContain(t, body, "Bundle strategies", "scheduled")
	settingsMustContain(t, body, "daily", "strategy row")
	settingsMustContain(t, body, "node-1", "host name")
	settingsMustContain(t, body, "refs/heads/main", "strategy refs")
}

// --- Mirror / push mirror ---------------------------------------------------

func TestSettingsSeamsMirrorReadScheduleRemove(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	// A mirror must point at a repo that EXISTS (the PUT 404 guard needs a
	// manifest key).
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := mirror.Create(ctx, h.store, "acme", "monorepo", "https://example.com/acme/monorepo.git", "daily"); err != nil {
		t.Fatalf("mirror.Create: %v", err)
	}
	code, body := seamsGet(t, h, "/acme/monorepo/settings/mirror")
	if code != http.StatusOK {
		t.Fatalf("GET mirror = %d", code)
	}
	settingsMustContain(t, body, "https://example.com/acme/monorepo.git", "upstream")
	settingsMustContain(t, body, "last synced", "mirror facts")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"mirror_action": {"schedule"},
		"schedule":      {"weekly"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("mirror schedule = %d (%s)", code, body)
	}
	settingsMustContain(t, loc, "mirror", "mirror redirect")
	doc, _, _ := mirror.Load(ctx, h.store, "acme", "monorepo")
	if doc.Schedule != "weekly" {
		t.Fatalf("schedule = %q, want weekly", doc.Schedule)
	}

	// An unknown preset is the API's 400.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"mirror_action": {"schedule"},
		"schedule":      {"every-fortnight"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad preset = %d, want 400 (%s)", code, body)
	}
	settingsMustContain(t, body, "unknown schedule preset", "preset error")

	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"mirror_action": {"remove"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("mirror remove = %d (%s)", code, body)
	}
	if doc, _, _ := mirror.Load(ctx, h.store, "acme", "monorepo"); doc != nil {
		t.Fatalf("mirror sidecar survived: %+v", doc)
	}
}

func TestSettingsSeamsPushMirrorReadSaveKeygenRemove(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	// Create through the page: no config yet → the create form renders, and
	// the POST goes through pushmirror's own PUT implementation.
	code, body := seamsGet(t, h, "/acme/monorepo/settings/mirror")
	if code != http.StatusOK {
		t.Fatalf("GET mirror = %d", code)
	}
	settingsMustContain(t, body, "configure push mirror", "push create form")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"push_action":  {"put"},
		"upstream_url": {"https://example.com/backup/monorepo.git"},
		"auth_kind":    {"none"},
		"schedule":     {"daily"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("push create = %d (%s)", code, body)
	}
	settingsMustContain(t, loc, "mirror", "push redirect")
	doc, _, err := pushmirror.Load(ctx, h.store, "acme", "monorepo")
	if err != nil || doc == nil {
		t.Fatalf("push doc = %+v err %v", doc, err)
	}
	if doc.UpstreamURL != "https://example.com/backup/monorepo" || doc.Schedule != "daily" {
		t.Fatalf("push doc = %+v", doc)
	}

	code, body = seamsGet(t, h, "/acme/monorepo/settings/mirror")
	if code != http.StatusOK {
		t.Fatalf("GET configured push mirror = %d", code)
	}
	settingsMustContain(t, body, "https://example.com/backup/monorepo", "push upstream")
	settingsMustContain(t, body, "mirror · push", "push badge")

	// Keygen on an https upstream is the API's 400 (ssh only).
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"push_action": {"keygen"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("keygen on https = %d, want 400 (%s)", code, body)
	}
	settingsMustContain(t, body, "needs an ssh upstream", "keygen error")

	// Remove stops the mirror.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"push_action": {"remove"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("push remove = %d (%s)", code, body)
	}
	if doc, _, _ := pushmirror.Load(ctx, h.store, "acme", "monorepo"); doc != nil {
		t.Fatalf("push sidecar survived: %+v", doc)
	}
}

func TestSettingsSeamsPushMirrorKeygenAndSync(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := pushmirror.Create(ctx, h.store, "acme", "monorepo",
		"ssh://example.com/acme/mirror.git", pushmirror.AuthSSH, "git", ""); err != nil {
		t.Fatalf("pushmirror.Create: %v", err)
	}
	code, _, body := seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"push_action":     {"keygen"},
		"ssh_known_hosts": {"example.com ssh-ed25519 AAAA"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("keygen = %d (%s)", code, body)
	}
	sec, _, err := pushmirror.LoadSecret(ctx, h.store, "acme", "monorepo")
	if err != nil || sec == nil || sec.SSHPrivateKey == "" {
		t.Fatalf("secret = %+v err %v", sec, err)
	}
	code, body = seamsGet(t, h, "/acme/monorepo/settings/mirror")
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	settingsMustContain(t, body, "deploy key fingerprint", "key fingerprint row")
	settingsMustContain(t, body, "ssh-ed25519", "installed public key")
}

// --- CI tokens / webhooks ---------------------------------------------------

func TestSettingsSeamsTokensCreateRevoke(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/tokens")
	if code != http.StatusOK {
		t.Fatalf("GET tokens = %d", code)
	}
	settingsMustContain(t, body, "Create a CI token", "tokens")
	settingsMustContain(t, body, "no tokens", "empty tokens")

	// The one-shot secret answers 200 with the page (never a redirect: the
	// token must not enter a URL).
	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/tokens", url.Values{
		"name": {"ci"},
	})
	if code != http.StatusOK {
		t.Fatalf("create token = %d (%s)", code, body)
	}
	if loc != "" {
		t.Fatalf("token create redirected to %q", loc)
	}
	settingsMustContain(t, body, "Copy now — shown once.", "token secret card")
	settingsMustContain(t, body, "wct_", "token value")

	tokens, err := h.cks.ListTokens(context.Background(), "acme", "monorepo", seamAdmin())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %+v err %v", tokens, err)
	}
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/tokens/"+tokens[0].ID, url.Values{
		"token_action": {"revoke"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("revoke = %d (%s)", code, body)
	}
	tokens, _ = h.cks.ListTokens(context.Background(), "acme", "monorepo", seamAdmin())
	if tokens[0].RevokedAt == nil {
		t.Fatalf("token not revoked: %+v", tokens[0])
	}
}

func TestSettingsSeamsWebhooksCreatePingDelete(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/webhooks")
	if code != http.StatusOK {
		t.Fatalf("GET webhooks = %d", code)
	}
	settingsMustContain(t, body, "Add a webhook", "webhooks")
	settingsMustContain(t, body, "no webhooks", "empty webhooks")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/webhooks", url.Values{
		"url":    {"http://127.0.0.1:9/hook"},
		"events": {"opened, commented"},
		"secret": {"s3cr3t"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("create webhook = %d (%s)", code, body)
	}
	settingsMustContain(t, loc, "/settings/webhooks", "webhook redirect")

	hooks, err := h.notes.ListHooks(context.Background(), "acme", "monorepo")
	if err != nil || len(hooks) != 1 {
		t.Fatalf("hooks = %+v err %v", hooks, err)
	}
	code, body = seamsGet(t, h, "/acme/monorepo/settings/webhooks")
	if code != http.StatusOK {
		t.Fatalf("GET configured webhooks = %d", code)
	}
	settingsMustContain(t, body, "http://127.0.0.1:9/hook", "hook row")
	settingsMustContain(t, body, "opened, commented", "hook events")
	settingsMustContain(t, body, "set", "secret presence")

	// Ping records a delivery row (the connection fails fast on the dead
	// loopback port) — the page renders it, bounded per hook.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/webhooks/"+hooks[0].ID, url.Values{
		"webhook_action": {"ping"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("ping = %d (%s)", code, body)
	}
	code, body = seamsGet(t, h, "/acme/monorepo/settings/webhooks")
	if code != http.StatusOK {
		t.Fatalf("GET after ping = %d", code)
	}
	settingsMustContain(t, body, "recent deliveries", "deliveries")
	settingsMustContain(t, body, "ping", "delivery event")

	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/webhooks/"+hooks[0].ID, url.Values{
		"webhook_action": {"delete"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("delete webhook = %d (%s)", code, body)
	}
	if hooks, _ := h.notes.ListHooks(context.Background(), "acme", "monorepo"); len(hooks) != 0 {
		t.Fatalf("hooks survived: %+v", hooks)
	}
}

func TestSettingsSeamsDangerTransferAndDelete(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/danger", url.Values{
		"danger_action": {"transfer"},
		"owner":         {"alice"},
		"confirm":       {"acme/monorepo"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("transfer = %d (%s)", code, body)
	}
	settingsMustContain(t, loc, "/alice/monorepo/settings/general", "transfer redirect")

	// Delete goes through the registry (the API's repo delete seam).
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("re-seed manifest: %v", err)
	}
	code, loc, body = seamsPost(t, h, "/acme/monorepo/settings/danger", url.Values{
		"danger_action": {"delete"},
		"confirm":       {"acme/monorepo"},
	})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/") {
		t.Fatalf("delete = %d %q (%s)", code, loc, body)
	}
	if len(h.reg.deleted) != 1 || h.reg.deleted[0] != "acme/monorepo" {
		t.Fatalf("deleted = %v", h.reg.deleted)
	}
}

// --- cross-origin on every wired write --------------------------------------

func TestSettingsSeamsCrossOriginRefused(t *testing.T) {
	h := newSeamsHarness(t)
	for _, target := range []string{
		"/acme/monorepo/settings/general",
		"/acme/monorepo/settings/access",
		"/acme/monorepo/settings/policy",
		"/acme/monorepo/settings/config",
		"/acme/monorepo/settings/mirror",
		"/acme/monorepo/settings/tokens",
		"/acme/monorepo/settings/tokens/abc",
		"/acme/monorepo/settings/webhooks",
		"/acme/monorepo/settings/webhooks/abc",
		"/acme/monorepo/settings/danger",
	} {
		rec := settingsDo(t, h.s, http.MethodPost, target, url.Values{"confirm": {"acme/monorepo"}}, map[string]string{
			"Origin": "http://evil.example",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST %s = %d, want 403", target, rec.Code)
		}
	}
}

// --- seam unit bits ---------------------------------------------------------

func TestSettingsSeamsTomlPassengers(t *testing.T) {
	in := "description = \"old\"\n\n[features]\nissues = false\n\n[bundles]\nmain_only = true\n"
	got := settingsWithFeatures(in, map[string]bool{"issues": true, "pulls": true, "releases": true, "forks": true, "watch": true, "star": true})
	settingsMustContain(t, got, "[features]\nissues = true", "features replaced")
	settingsMustContain(t, got, "[bundles]\nmain_only = true", "bundles kept")
	if strings.Count(got, "[features]") != 1 {
		t.Fatalf("features block duplicated:\n%s", got)
	}
	out := settingsWithDescription(got, `q"uote`)
	settingsMustContain(t, out, `description = "q\"uote"`, "escaped description")
	out = settingsWithSelfApproval(out, false)
	settingsMustContain(t, out, "[review]\nallow_self_approval = false", "review appended")
	out2 := settingsWithSelfApproval(out, true)
	if strings.Count(out2, "allow_self_approval") != 1 || !strings.Contains(out2, "allow_self_approval = true") {
		t.Fatalf("review not replaced:\n%s", out2)
	}
	if got := settingsWithDescription("", "first"); got != "description = \"first\"\n" {
		t.Fatalf("empty description doc = %q", got)
	}
	if got := settingsWithFeatures("", map[string]bool{}); !strings.HasPrefix(got, "[features]") {
		t.Fatalf("empty features doc = %q", got)
	}
	if got := settingsWithSelfApproval("", true); got != "[review]\nallow_self_approval = true\n" {
		t.Fatalf("empty review doc = %q", got)
	}
}

func TestSettingsSeamsStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{api.ErrNotFound, http.StatusNotFound},
		{api.ErrExists, http.StatusConflict},
		{errors.New("boom"), http.StatusServiceUnavailable},
		{identity.ErrInvalid, http.StatusBadRequest},
		{identity.ErrConflict, http.StatusConflict},
		{identity.ErrTooLarge, http.StatusRequestEntityTooLarge},
		{identity.ErrUnsupportedMedia, http.StatusUnsupportedMediaType},
		{identity.ErrUnauthorized, http.StatusUnauthorized},
		{checks.ErrInvalid, http.StatusBadRequest},
		{checks.ErrConflict, http.StatusConflict},
		{checks.ErrUnauthorized, http.StatusUnauthorized},
		{checks.ErrForbidden, http.StatusForbidden},
		{checks.ErrNotFound, http.StatusNotFound},
		{errors.New("boom"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		if got := settingsRepoErrStatus(tc.err); got != tc.want && (errors.Is(tc.err, api.ErrNotFound) || errors.Is(tc.err, api.ErrExists)) {
			t.Fatalf("repo status(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if got := settingsIdentityStatus(identity.ErrNotFound); got != http.StatusNotFound {
		t.Fatalf("identity 404 = %d", got)
	}
	if got := settingsIdentityStatus(errors.New("x")); got != http.StatusServiceUnavailable {
		t.Fatalf("identity 503 = %d", got)
	}
	if got := settingsChecksStatus(errors.New("x")); got != http.StatusServiceUnavailable {
		t.Fatalf("checks 503 = %d", got)
	}
	if got := settingsNotifyStatus(notify.ErrConflict); got != http.StatusConflict {
		t.Fatalf("notify 409 = %d", got)
	}
	if got := settingsNotifyStatus(errors.New("x")); got != http.StatusServiceUnavailable {
		t.Fatalf("notify 503 = %d", got)
	}
	if got := settingsNotifyStatus(notify.ErrInvalid); got != http.StatusBadRequest {
		t.Fatalf("notify 400 = %d", got)
	}
	if got := settingsNotifyStatus(notify.ErrUnauthorized); got != http.StatusUnauthorized {
		t.Fatalf("notify 401 = %d", got)
	}
	if got := settingsNotifyStatus(notify.ErrForbidden); got != http.StatusForbidden {
		t.Fatalf("notify 403 = %d", got)
	}
	if got := settingsNotifyStatus(notify.ErrNotFound); got != http.StatusNotFound {
		t.Fatalf("notify 404 = %d", got)
	}
	if got := settingsAuthErr(&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}); got.Status != http.StatusForbidden {
		t.Fatalf("auth 403 = %+v", got)
	}
	if got := settingsAuthErr(&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}); got.Status != http.StatusServiceUnavailable {
		t.Fatalf("auth 503 = %+v", got)
	}
	if got := settingsAuthErr(&auth.AuthError{Kind: auth.ErrUnauthorized, Why: "who"}); got.Status != http.StatusUnauthorized {
		t.Fatalf("auth 401 = %+v", got)
	}
	if got := settingsValidateDescription(strings.Repeat("x", 513)); !strings.Contains(got, "limit is 512") {
		t.Fatalf("long description = %q", got)
	}
	if got := settingsEventList(" a , ,b "); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("event list = %v", got)
	}
	if got := settingsEventList(""); got != nil {
		t.Fatalf("empty event list = %v", got)
	}
}

func TestSettingsSeamsUnwiredServices(t *testing.T) {
	// An empty service set leaves every repo-side field nil: the panels keep
	// their honest "not wired" cards and no write path is claimed.
	var deps SettingsDeps
	deps.BindSettingsServices(SettingsServices{})
	if deps.RepoWrite != nil {
		t.Fatal("RepoWrite must stay nil without services")
	}
	f := &settingsFakes{deps: deps}
	s := newSettingsServer(t, f)
	for _, tab := range []string{"mirror", "policy", "access", "tokens"} {
		rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tab, rec.Code)
		}
		settingsMustContain(t, rec.Body.String(), "not wired on this build", tab+" unwired card")
	}
	rec := settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/policy",
		url.Values{"policy": {"{}"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired write = %d, want 503", rec.Code)
	}
}

func TestSettingsSeamsDeleteRepoRegistryError(t *testing.T) {
	h := newSeamsHarness(t)
	h.apiE.Repos = nil
	_, werr := h.deps.RepoWrite(context.Background(), "acme", "monorepo", seamAdmin(), SettingsRepoWrite{
		Action: SettingsActionDelete, Values: map[string]string{"confirm": "acme/monorepo"},
	})
	if werr == nil || werr.Status != http.StatusServiceUnavailable {
		t.Fatalf("delete without registry = %+v", werr)
	}
}

func TestSettingsSeamsMirrorSyncAndErrors(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	// "sync now" without a configured mirror is the API's 404.
	code, _, body := seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"mirror_action": {"sync"}})
	if code != http.StatusNotFound {
		t.Fatalf("sync without mirror = %d (%s)", code, body)
	}
	if _, err := mirror.Create(ctx, h.store, "acme", "monorepo", "https://example.com/acme/monorepo.git", "daily"); err != nil {
		t.Fatalf("mirror.Create: %v", err)
	}
	// The task id is narrated; without a registry the fire fails fast in the
	// background (this harness has no git registry) — never a blocked request.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"mirror_action": {"sync"}, "token": {"t"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("mirror sync = %d (%s)", code, body)
	}
	// Removing a mirror twice: the second is 404.
	if code, _, _ = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"mirror_action": {"remove"}}); code != http.StatusSeeOther {
		t.Fatalf("remove = %d", code)
	}
	if code, _, _ = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"mirror_action": {"remove"}}); code != http.StatusNotFound {
		t.Fatalf("remove twice = %d, want 404", code)
	}
}

func TestSettingsSeamsPushSyncAndErrors(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	code, _, body := seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"push_action": {"sync"}})
	if code != http.StatusNotFound {
		t.Fatalf("push sync without config = %d (%s)", code, body)
	}
	if _, err := pushmirror.Create(ctx, h.store, "acme", "monorepo",
		"https://example.com/backup/monorepo.git", pushmirror.AuthNone, "", "daily"); err != nil {
		t.Fatalf("pushmirror.Create: %v", err)
	}
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"push_action": {"sync"}})
	if code != http.StatusSeeOther {
		t.Fatalf("push sync = %d (%s)", code, body)
	}
	// An unknown auth-kind on save is the feature's 400.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{
		"push_action": {"schedule"}, "schedule": {"daily"}, "auth_kind": {"kerberos"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad auth kind = %d (%s)", code, body)
	}
	if code, _, _ = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"push_action": {"remove"}}); code != http.StatusSeeOther {
		t.Fatalf("push remove = %d", code)
	}
	if code, _, _ = seamsPost(t, h, "/acme/monorepo/settings/mirror", url.Values{"push_action": {"remove"}}); code != http.StatusNotFound {
		t.Fatalf("push remove twice = %d, want 404", code)
	}
}

func TestSettingsSeamsTokenWebhookErrorsAndTransfer(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	if code, _, body := seamsPost(t, h, "/acme/monorepo/settings/tokens/nope", url.Values{"token_action": {"revoke"}}); code != http.StatusNotFound {
		t.Fatalf("revoke unknown = %d (%s)", code, body)
	}
	if code, _, body := seamsPost(t, h, "/acme/monorepo/settings/webhooks/nope", url.Values{"webhook_action": {"delete"}}); code != http.StatusNotFound {
		t.Fatalf("delete unknown hook = %d (%s)", code, body)
	}
	if code, _, body := seamsPost(t, h, "/acme/monorepo/settings/webhooks/nope", url.Values{"webhook_action": {"ping"}}); code != http.StatusNotFound {
		t.Fatalf("ping unknown hook = %d (%s)", code, body)
	}
	if code, _, body := seamsPost(t, h, "/acme/monorepo/settings/webhooks", url.Values{"url": {"ftp://example.com/x"}}); code != http.StatusBadRequest {
		t.Fatalf("bad hook url = %d (%s)", code, body)
	}
	// Transfer onto an existing repo is the identity 409.
	if _, err := h.store.Put(ctx, store.RepoPrefix("acme", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("m")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if _, err := h.store.Put(ctx, store.RepoPrefix("alice", "monorepo")+store.Manifest,
		store.PutBody{Bytes: []byte("m")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	code, _, body := seamsPost(t, h, "/acme/monorepo/settings/danger", url.Values{
		"danger_action": {"transfer"}, "owner": {"alice"}, "confirm": {"acme/monorepo"},
	})
	if code != http.StatusConflict {
		t.Fatalf("transfer collision = %d (%s)", code, body)
	}
}

// --- Branch protection (WP2.3) ---------------------------------------------

// TestSettingsSeamsProtectionRoundTrip is the definition-of-done round trip:
// UI payload → the SAME validated policy document → the push gate enforces
// it (the bypass role admits its holder, everyone else is denied).
func TestSettingsSeamsProtectionRoundTrip(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "monorepo"}
	// A hand-written rule the structured form must preserve.
	const custom = `{"version":1,"rules":[{"name":"release-tags","match":{"refs":["refs/tags/v*"]},"effect":{"protect":{"restricts":["delete"]}}}]}`
	if werr := api.PutPolicy(ctx, h.apiE, id, []byte(custom)); werr != nil {
		t.Fatalf("seed policy: %v", werr)
	}
	if _, err := h.ident.PutAccess(ctx, "acme", "monorepo", "", identity.Visibility("private"),
		[]identity.AccessBinding{{Subject: "user:bob@example.com", Role: identity.RoleMaintain}}); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}

	code, body := seamsGet(t, h, "/acme/monorepo/settings/protection")
	if code != http.StatusOK {
		t.Fatalf("GET protection = %d (%s)", code, body)
	}
	settingsMustContain(t, body, "Branch protection", "tab heading")
	settingsMustContain(t, body, "Effective policy", "effective view")
	settingsMustContain(t, body, "release-tags", "existing rule listed")
	settingsMustContain(t, body, `name="refs"`, "structured form")
	settingsMustContain(t, body, `name="policy"`, "advanced raw editor (not the only path)")

	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/protection", url.Values{
		"enabled":       {"1"},
		"refs":          {"refs/heads/main, refs/heads/release/*"},
		"approvals":     {"2"},
		"dismiss_stale": {"1"},
		"checks":        {"ci/build, lint"},
		"push_roles":    {"maintain", "admin"},
		"block_force":   {"1"},
		"block_delete":  {"1"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/settings/protection") {
		t.Fatalf("save = %d %q (%s)", code, loc, body)
	}

	raw, _, err := api.PolicyRaw(ctx, h.apiE, id)
	if err != nil {
		t.Fatalf("stored policy unparseable: %v (%s)", err, raw)
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		t.Fatalf("parse: %v (%s)", perr, raw)
	}
	if doc.RuleByName("release-tags") == nil {
		t.Fatalf("custom rule dropped: %s", raw)
	}
	push := doc.RuleByName("protection-push")
	if push == nil || push.Protect() == nil {
		t.Fatalf("protection-push missing: %s", raw)
	}
	pe := push.Protect()
	if !containsStr(pe.RestrictOps, "force-push") || !containsStr(pe.RestrictOps, "delete") ||
		!containsStr(pe.RestrictOps, "update") {
		t.Fatalf("restricts = %v", pe.RestrictOps)
	}
	if len(pe.Bypass) != 2 || pe.Bypass[0] != "role:acme/monorepo:maintain" {
		t.Fatalf("bypass = %v", pe.Bypass)
	}
	if len(pe.RequireChecks) != 2 || pe.RequireChecks[0] != "ci/build" {
		t.Fatalf("checks = %v", pe.RequireChecks)
	}
	rev := doc.RuleByName("protection-reviews")
	if rev == nil || rev.RequiredReviews() == nil || rev.RequiredReviews().MinApprovals != 2 || !rev.RequiredReviews().DismissStale {
		t.Fatalf("reviews = %+v", rev)
	}

	// Enforcement: expand the role: bypass through identity, then run the
	// push half. A maintainer bypasses; a writer does not.
	exp, warnings := pe.ExpandedCopy(ctx, h.ident.PolicyExpander())
	if len(warnings) != 0 {
		t.Fatalf("expansion warnings: %v", warnings)
	}
	enforced := &policy.Document{Rules: []*policy.Rule{{Name: push.Name, Match: push.Match, Effect: exp}}}
	if v := policy.Evaluate(ctx, enforced, policy.Request{Principal: "bob@example.com", Ref: "refs/heads/main", Op: policy.OpUpdate}); !v.Allow {
		t.Fatalf("maintainer denied: %+v", v)
	}
	if v := policy.Evaluate(ctx, enforced, policy.Request{Principal: "carol@example.com", Ref: "refs/heads/main", Op: policy.OpUpdate}); v.Allow {
		t.Fatal("non-bypass push allowed")
	}

	// Disabling removes only the managed rules.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/protection", url.Values{"refs": {"refs/heads/main"}})
	if code != http.StatusSeeOther {
		t.Fatalf("disable = %d (%s)", code, body)
	}
	raw, _, _ = api.PolicyRaw(ctx, h.apiE, id)
	doc, perr = policy.Parse(raw)
	if perr != nil {
		t.Fatalf("parse after disable: %v (%s)", perr, raw)
	}
	if doc.RuleByName("protection-push") != nil || doc.RuleByName("protection-reviews") != nil {
		t.Fatalf("managed rules survived disable: %s", raw)
	}
	if doc.RuleByName("release-tags") == nil {
		t.Fatalf("custom rule dropped on disable: %s", raw)
	}
}

// TestSettingsSeamsProtectionValidation pins the inline refusals: an empty
// ref list, an unknown role and a bad check context never reach the store.
func TestSettingsSeamsProtectionValidation(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "monorepo"}
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no refs", url.Values{"enabled": {"1"}, "refs": {" "}}, "at least one ref pattern"},
		{"unknown role", url.Values{"enabled": {"1"}, "refs": {"refs/heads/main"}, "push_roles": {"owner"}}, "unknown push role"},
		{"bad check", url.Values{"enabled": {"1"}, "refs": {"refs/heads/main"}, "checks": {"ci;build"}}, "invalid context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := seamsPost(t, h, "/acme/monorepo/settings/protection", tc.form)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", code, body)
			}
			settingsMustContain(t, body, tc.want, "inline error")
		})
	}
	if raw, _, _ := api.PolicyRaw(ctx, h.apiE, id); strings.Contains(string(raw), "protection-push") {
		t.Fatalf("a rejected form wrote the store: %s", raw)
	}
}
