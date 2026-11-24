package webui

// primary_matrix_test.go — style-guideline §1 F3, the one-primary rule, as a
// page matrix: at most ONE `.btn.primary` per rendered page, and exactly one
// wherever the page has a real main action. The rule is applied by the
// renderer's projection (projectPrimary, render.go), so this test reads the
// same count the renderer does — a conditional CTA that only exists for one
// viewer is exercised with that viewer, not asserted from the template source.
//
// Pages that deliberately render none carry the reason in the case: a read
// view, or a list whose actions are per row (no single page action to accent).
// A page that renders two of its own is a template bug this test names.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/releases"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/tags"
	"github.com/crueber/spindle/internal/threads"
)

// wantPrimary asserts the page's primary count and, for zero, that the case
// documented why the page has no single action.
func wantPrimary(t *testing.T, body string, want int, zeroWhy string) {
	t.Helper()
	got := countPrimaryButtons([]byte(body))
	if got > 1 {
		t.Fatalf("rendered %d .btn.primary nodes, want at most 1", got)
	}
	if got != want {
		t.Fatalf("rendered %d .btn.primary nodes, want %d", got, want)
	}
	if got == 0 && zeroWhy == "" {
		t.Fatal("zero primaries without a documented reason")
	}
}

// primaryRepoFixture is the busy repo every repository-page case starts from.
func primaryRepoFixture(t *testing.T, anonRead bool) *repoFixture {
	t.Helper()
	bindChromeProjections(t)
	f := newRepoFixture(t, anonRead)
	f.view.summary = busySummary()
	f.view.tree = api.TreeResult{Ref: "refs/heads/main"}
	return f
}

// TestOnePrimaryPerPageMatrix is the inventory the style guideline asks for.
func TestOnePrimaryPerPageMatrix(t *testing.T) {
	t.Run("landing signed out", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
		rec := publicGet(s, "/")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("landing signed in", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
		rec := publicGet(s, "/")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("how it works", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
		rec := publicGet(s, "/how-it-works")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("owner profile edit", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		env.API.OwnerEdit = publicFakeOwnerEdit{allow: true}
		s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{
			ProfileSave: func(context.Context, string, OwnerProfileInput) error { return nil },
		})
		rec := publicGet(s, "/alice?edit=1")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("explore", func(t *testing.T) {
		reg := &coverageRegistry{
			owners: []string{"acme"},
			repos:  map[string][]string{"acme": {"widgets"}},
			counts: map[string]int{"acme": 1},
		}
		s := newCoverageExploreServer(t, reg)
		rec := httptest.NewRecorder()
		s.explore(rec, httptest.NewRequest(http.MethodGet, "/explore", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("explore = %d", rec.Code)
		}
		// Browse is a read index: no single page action.
		wantPrimary(t, rec.Body.String(), 0, "read-only index")
	})

	// The repository family: Clone is the page's primary exactly when the
	// page body renders none of its own.
	t.Run("code signed out", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		wantPrimary(t, f.mustGet("/demo/spindle", http.StatusOK), 1, "")
	})
	t.Run("code signed in", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.rebind()
		wantPrimary(t, f.mustGet("/demo/spindle", http.StatusOK), 1, "")
	})
	t.Run("commits", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		sha := strings.Repeat("a", 40)
		f.view.commits = api.CommitPage{Ref: "refs/heads/main", SHA: sha, Commits: commitsFixture(sha)}
		wantPrimary(t, f.mustGet("/demo/spindle/commits", http.StatusOK), 1, "")
	})
	t.Run("tags", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		BindRepo(RepoDeps{
			TagsList: func(context.Context, string, string, auth.Principal) ([]tags.TagEntry, error) {
				return []tags.TagEntry{{Name: "v1.0.0", Commit: strings.Repeat("a", 40), Date: "2026-11-01T00:00:00Z", Author: "ada"}}, nil
			},
		})
		wantPrimary(t, f.mustGet("/demo/spindle/tags", http.StatusOK), 1, "")
	})
	t.Run("branches signed out", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		wantPrimary(t, f.mustGet("/demo/spindle/branches", http.StatusOK), 1, "")
	})
	t.Run("branches signed in writer", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.rebind()
		wantPrimary(t, f.mustGet("/demo/spindle/branches", http.StatusOK), 1, "")
	})
	t.Run("compare", func(t *testing.T) {
		f := compareFixture(t)
		wantPrimary(t, f.mustGet("/demo/spindle/compare/main...feature", http.StatusOK), 1, "")
	})
	t.Run("blob", func(t *testing.T) {
		f := primaryRepoFixture(t, true)
		f.view.blob = api.BlobResult{Ref: "refs/heads/main", Name: "main.go", Path: "main.go", Size: 25,
			Contents: []byte("package main\n\nfunc main() {}\n")}
		wantPrimary(t, f.mustGet("/demo/spindle/blob/main/main.go", http.StatusOK), 1, "")
	})
	t.Run("editor", func(t *testing.T) {
		f := newEditFixture(t, auth.Principal{Name: "jane", Write: true})
		wantPrimary(t, f.getStatus("/acme/widgets/edit/main/docs/guide.md", http.StatusOK), 1, "")
	})
	t.Run("commit signed out", func(t *testing.T) {
		f, _ := tagFixture(t)
		sha := strings.Repeat("a", 40)
		// Read-only: no Create tag form, so Clone carries the accent.
		f.principal = auth.Principal{Name: "bob", Write: true}
		f.rebind()
		wantPrimary(t, f.mustGet("/demo/spindle/commit/"+sha, http.StatusOK), 1, "")
	})
	t.Run("commit writer with create tag", func(t *testing.T) {
		f, _ := tagFixture(t)
		sha := strings.Repeat("a", 40)
		wantPrimary(t, f.mustGet("/demo/spindle/commit/"+sha, http.StatusOK), 1, "")
	})
	t.Run("packages", func(t *testing.T) {
		f, _ := packagesFixture(t, true)
		wantPrimary(t, bodyText(f.get("/acme/widgets/packages")), 1, "")
	})

	// Collaboration family: the page's own CTA is primary; Clone demotes.
	t.Run("issues list signed out", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.view.summary = busySummary()
		f.deps(issueDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/issues")), 1, "")
	})
	t.Run("issues list signed in", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(issueDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/issues")), 1, "")
	})
	t.Run("issue detail signed out", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.view.summary = busySummary()
		f.deps(issueDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/issues/3")), 1, "")
	})
	t.Run("issue detail signed in", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(issueDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/issues/3")), 1, "")
	})
	t.Run("pulls list", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(pullDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/pulls?state=all")), 1, "")
	})
	t.Run("pull detail open", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(pullDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/pull/5")), 1, "")
	})
	t.Run("pull detail merged", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		d := pullDeps()
		base := d.PullGet
		d.PullGet = func(ctx context.Context, o, r string, n int, p auth.Principal, a, b int) (*pulls.PullView, error) {
			v, err := base(ctx, o, r, n, p, a, b)
			if err == nil {
				v.PR.Merged = true
				v.PR.MergedBy = ptr("alice")
			}
			return v, err
		}
		f.deps(d)
		// Merged locks the composer: Clone is the page's one action.
		wantPrimary(t, bodyText(f.get("/acme/widgets/pull/5")), 1, "")
	})
	t.Run("pull detail closed", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		d := pullDeps()
		base := d.PullGet
		d.PullGet = func(ctx context.Context, o, r string, n int, p auth.Principal, a, b int) (*pulls.PullView, error) {
			v, err := base(ctx, o, r, n, p, a, b)
			if err == nil {
				v.Thread.State = "closed"
			}
			return v, err
		}
		f.deps(d)
		wantPrimary(t, bodyText(f.get("/acme/widgets/pull/5")), 1, "")
	})
	t.Run("pull detail draft", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		d := pullDeps()
		base := d.PullGet
		d.PullGet = func(ctx context.Context, o, r string, n int, p auth.Principal, a, b int) (*pulls.PullView, error) {
			v, err := base(ctx, o, r, n, p, a, b)
			if err == nil {
				v.PR.Draft = true
			}
			return v, err
		}
		f.deps(d)
		wantPrimary(t, bodyText(f.get("/acme/widgets/pull/5")), 1, "")
	})
	t.Run("pulls files", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(pullDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/pull/5/files")), 1, "")
	})
	t.Run("forks list", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(pullDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/forks")), 1, "")
	})
	t.Run("releases list", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(releaseDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/releases")), 1, "")
	})
	t.Run("release detail published", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true}
		f.view.summary = busySummary()
		f.deps(releaseDeps())
		wantPrimary(t, bodyText(f.get("/acme/widgets/releases/v1.2.3")), 1, "")
	})
	t.Run("release detail draft", func(t *testing.T) {
		bindChromeProjections(t)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "alice", Write: true, Admin: true}
		f.view.summary = busySummary()
		d := releaseDeps()
		base := d.ReleaseGet
		d.ReleaseGet = func(ctx context.Context, o, r string, p auth.Principal, tag string) (*releases.Release, store.Version, error) {
			rel, v, err := base(ctx, o, r, p, tag)
			if err == nil {
				rel.Draft = true
				rel.PublishedAt = nil
			}
			return rel, v, err
		}
		f.deps(d)
		wantPrimary(t, bodyText(f.get("/acme/widgets/releases/v1.2.3")), 1, "")
	})
	t.Run("threads list", func(t *testing.T) {
		bindChromeProjections(t)
		run := threadsFixtureRun()
		bindThreadsFixture(t, run)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "ada", Write: true}
		f.view.summary = busySummary()
		wantPrimary(t, bodyText(f.get("/acme/widgets/threads")), 1, "")
	})
	t.Run("threads run", func(t *testing.T) {
		bindChromeProjections(t)
		run := threadsFixtureRun()
		bindThreadsFixture(t, run)
		f := newCollabFixture(t)
		f.principal = auth.Principal{Name: "ada", Write: true}
		f.view.summary = busySummary()
		wantPrimary(t, bodyText(f.get("/acme/widgets/threads/"+run.ID)), 1, "")
	})
	t.Run("threads run held for approval", func(t *testing.T) {
		bindChromeProjections(t)
		run := threadsFixtureRun()
		run.State = threads.RunPendingApproval
		run.Trusted = false
		run.Actor = "newcomer"
		job := threadsFixtureJob(run)
		job.State = threads.JobQueued
		BindThreads(ThreadsDeps{
			Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
				return &threads.RunView{Run: run, Jobs: []*threads.Job{job}}, nil
			},
			Role: func(context.Context, string, string, auth.Principal) (string, error) { return "maintain", nil },
		})
		t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
		f := newCollabFixture(t)
		f.view.summary = busySummary()
		body := bodyText(f.get("/acme/widgets/threads/" + run.ID))
		wantPrimary(t, body, 1, "")
		if !strings.Contains(body, "approve run") {
			t.Fatal("held run lost its approve affordance")
		}
	})

	// Settings and account surfaces.
	t.Run("repo settings tabs", func(t *testing.T) {
		bindChromeProjections(t)
		f := settingsRepoFakes("admin")
		s := newSettingsServer(t, f)
		s.env.API = &api.Env{
			Repo:  &collabFakeView{summary: busySummary(), known: map[string]bool{"acme/monorepo": true}},
			Repos: &collabFakeRegistry{exists: map[string]bool{"acme/monorepo": true}},
		}
		for _, tab := range []string{
			"general", "scheduled", "mirror", "policy", "protection",
			"config", "access", "tokens", "webhooks", "secrets",
			"danger", "wal",
		} {
			rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/"+tab, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("settings %s = %d", tab, rec.Code)
			}
			t.Run(tab, func(t *testing.T) {
				// The WAL and danger read panels have no single action; then
				// Clone still carries the page accent, so the count stays 1.
				wantPrimary(t, rec.Body.String(), 1, "")
			})
		}
	})
	t.Run("repo settings protection list and form", func(t *testing.T) {
		bindChromeProjections(t)
		f := settingsRepoFakes("admin")
		raw := []byte(rulesetDoc)
		f.deps.RepoProtection = func(context.Context, string, string) (*SettingsProtection, error) {
			return settingsProtectionView(raw, "acme", "monorepo"), nil
		}
		s := newSettingsServer(t, f)
		s.env.API = &api.Env{
			Repo:  &collabFakeView{summary: busySummary(), known: map[string]bool{"acme/monorepo": true}},
			Repos: &collabFakeRegistry{exists: map[string]bool{"acme/monorepo": true}},
		}
		for _, path := range []string{
			"/acme/monorepo/settings/protection",
			"/acme/monorepo/settings/protection/main",
			"/acme/monorepo/settings/protection/new",
		} {
			rec := settingsDo(t, s, http.MethodGet, path, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d", path, rec.Code)
			}
			wantPrimary(t, rec.Body.String(), 1, "")
		}
	})
	t.Run("org settings tabs", func(t *testing.T) {
		f := settingsOrgFakes(true)
		baseView := f.deps.OrgView
		f.deps.OrgView = func(ctx context.Context, org string, p auth.Principal) (*SettingsOrgPage, error) {
			page, err := baseView(ctx, org, p)
			if err == nil {
				page.WebhooksWired = true
			}
			return page, err
		}
		s := newSettingsServer(t, f)
		for _, tab := range []string{"", "members", "teams", "invitations", "webhooks", "activity", "danger"} {
			path := "/acme/settings"
			if tab != "" {
				path += "?tab=" + tab
			}
			rec := settingsDo(t, s, http.MethodGet, path, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("org %s = %d", tab, rec.Code)
			}
			t.Run("tab "+tab, func(t *testing.T) {
				// Profile/members/teams/invitations/webhooks each own exactly
				// one form action. Activity and danger are read views (the
				// danger delete is a typed-confirmation `.btn danger`).
				want := 1
				why := ""
				if tab == "activity" {
					want, why = 0, "read-only activity log"
				}
				if tab == "danger" {
					want, why = 0, "read-only danger zone (typed-id delete is danger, not primary)"
				}
				wantPrimary(t, rec.Body.String(), want, why)
			})
		}
	})
	t.Run("account keys", func(t *testing.T) {
		s := newSettingsServer(t, &settingsFakes{})
		s.env.API = &api.Env{SSHKeys: &settingsFakeKeys{}}
		rec := settingsDo(t, s, http.MethodGet, "/keys", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("keys = %d", rec.Code)
		}
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("notifications", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Principal{Name: "alice"}, publicNotifDeps(t))
		rec := publicGet(s, "/notifications")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 0, "read-only notification inbox")
	})
	t.Run("invitations inbox", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Principal{Name: "alice"}, publicInviteDeps([]identity.InboxEntry{
			{ID: "inv-repo", Repo: "acme/widget", Role: "write", InvitedBy: "bob", ExpiresAt: "2020-01-01T00:00:00Z"},
			{ID: "inv-org", Org: "acme", Role: "member", InvitedBy: "bob", ExpiresAt: "2030-01-01T00:00:00Z"},
		}))
		rec := publicGet(s, "/invitations")
		publicWantStatus(t, rec, http.StatusOK)
		// Per-row accept/decline pairs: no single page action to accent.
		wantPrimary(t, rec.Body.String(), 0, "per-row inbox actions")
	})
	t.Run("invitations shared link", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Principal{Name: "alice"}, publicInviteDeps(nil))
		rec := publicGet(s, "/invitations?id=inv-shared&token=tok")
		publicWantStatus(t, rec, http.StatusOK)
		// The focused shared invitation owns the accent; rows stay quiet.
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("login required", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
		rec := publicGet(s, "/login-required")
		publicWantStatus(t, rec, http.StatusOK)
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("admin overview configured", func(t *testing.T) {
		s := adminTestPage(t, adminBaseDeps(), auth.Principal{Name: "alice", Admin: true})
		rec := settingsDo(t, s, http.MethodGet, "/admin", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin = %d", rec.Code)
		}
		wantPrimary(t, rec.Body.String(), 0, "instance overview is a read view once configured")
	})
	t.Run("admin overview first run", func(t *testing.T) {
		deps := adminBaseDeps()
		deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
			v := adminFakeView()
			v.FileState = "absent"
			v.FirstRun = true
			return v, nil
		}
		s := adminTestPage(t, deps, settingsTestPrincipal())
		rec := settingsDo(t, s, http.MethodGet, "/admin", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin first run = %d", rec.Code)
		}
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("admin config", func(t *testing.T) {
		s := adminTestPage(t, adminBaseDeps(), auth.Principal{Name: "alice", Admin: true})
		rec := settingsDo(t, s, http.MethodGet, "/admin/config", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin config = %d", rec.Code)
		}
		wantPrimary(t, rec.Body.String(), 1, "")
	})
	t.Run("admin access", func(t *testing.T) {
		s := adminTestPage(t, adminBaseDeps(), auth.Principal{Name: "alice", Admin: true})
		rec := settingsDo(t, s, http.MethodGet, "/admin/access", nil, nil)
		wantPrimary(t, rec.Body.String(), 0, "read-only access view")
	})
	t.Run("admin maintenance", func(t *testing.T) {
		s := adminTestPage(t, adminBaseDeps(), auth.Principal{Name: "alice", Admin: true})
		rec := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil)
		wantPrimary(t, rec.Body.String(), 0, "read-only maintenance view")
	})

	// The signed in/out header never competes with the page accent.
	t.Run("header login is secondary", func(t *testing.T) {
		env, _ := alicePublicEnv(t)
		s := newPublicServer(t, env, auth.Anonymous(), PublicDeps{})
		body := publicGet(s, "/").Body.String()
		for _, n := range byClass(t, body, "btn") {
			if hasClass(n, "primary") && strings.TrimSpace(text(n)) == "Login" {
				t.Fatal("header Login is still a primary")
			}
		}
	})
}

// TestPrimaryCounterReadsTokens pins the renderer's reader: class token
// membership (any order), not a substring that a template could satisfy by
// accident.
func TestPrimaryCounterReadsTokens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"none", `<button class="btn">x</button>`, 0},
		{"plain", `<button class="btn primary">x</button>`, 1},
		{"order and utility", `<a class="mt-2 primary btn px-2">x</a>`, 1},
		{"not a button", `<div class="btn primary">x</div>`, 1}, // nodes with the classes count
		{"primary without btn", `<span class="chip primary">x</span>`, 0},
		{"substring only", `<div class="btn-primary">x</div>`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countPrimaryButtons([]byte(tc.body)); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRepoChromeOfFindsBothShapes: the layout attaches chrome as PageData.Repo
// for most groups; the repo group's own templates carry it in their page data
// (12_web_ui.md §2.2.1). The projection must find both, and never panic on a
// page without chrome.
func TestRepoChromeOfFindsBothShapes(t *testing.T) {
	t.Parallel()
	layout := &RepoChrome{ClonePrimary: true}
	if got := repoChromeOf(PageData{Repo: layout}); got != layout {
		t.Fatal("layout chrome not found")
	}
	type repoPage struct{ Repo *RepoChrome }
	inline := &RepoChrome{ClonePrimary: true}
	if got := repoChromeOf(PageData{Data: &repoPage{Repo: inline}}); got != inline {
		t.Fatal("inline chrome not found")
	}
	for _, d := range []PageData{
		{Data: struct{ Name string }{"x"}},
		{Data: (*repoPage)(nil)},
		{Data: nil},
		{Data: 42},
		{Data: struct{ Repo *RepoChrome }(struct{ Repo *RepoChrome }{})},
	} {
		if got := repoChromeOf(d); got != nil {
			t.Fatalf("chrome from %#v = %+v, want nil", d.Data, got)
		}
	}
}
