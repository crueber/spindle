package webui

// admin_runners_test.go — /admin/runners (docs/PLAN-INSTANCE-RUNNERS.md §5):
// the host-admin gate, the fleet table, the one-time token state, the empty
// state and the revoke form. Settings → Runners is gone (the repo tab is not a
// route any more).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

var errFake = errors.New("fleet unavailable")

func adminRunnerDeps() testDeps {
	deps := adminBaseDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) {
		return []AdminRunnerRow{
			{ID: "abcd1234", Name: "build-01", Labels: "linux, docker", Capacity: "2", State: "active",
				LastSeen: "2026-11-22T10:00:00Z", CurrentJob: "acme/mono · run-1 · j-build",
				AllowAll: true, ForkRuns: false},
			{ID: "efgh5678", Name: "—", Labels: "—", Capacity: "1", State: "pending", LastSeen: "never",
				Repos: []string{"acme/*", "other/lib"}, ForkRuns: true},
		}, nil
	}
	deps.RunnerTenancy.Mint = func(_ context.Context, in threads.MintInput, _ auth.Principal) (string, string, string, error) {
		if !in.AllowAll && len(in.Repos) == 0 {
			return "", "", "", fmt.Errorf("%w: choose all repositories or at least one pattern", threads.ErrInvalid)
		}
		return "ijkl9012", "wrg_YWRtaW4.ijkl9012.secret", "2026-11-22T10:15:00Z", nil
	}
	deps.RevokeRunner = func(context.Context, string, auth.Principal) error { return nil }
	deps.RunnerTenancy.SetPolicy = func(_ context.Context, id string, in threads.RunnerPolicy, _ string) (threads.RunnerPolicy, error) {
		pol, perr := threads.ValidateRunnerPolicy(threads.RunnerPolicyInput{AllowAll: in.AllowAll, Repos: in.Repos})
		if perr != nil {
			return threads.RunnerPolicy{}, perr
		}
		return pol, nil
	}
	deps.RunnerTenancy.Drain = func(context.Context, string, auth.Principal) error { return nil }
	deps.RunnerTenancy.Resume = func(context.Context, string, auth.Principal) error { return nil }
	deps.RunnerTenancy.Rotate = func(_ context.Context, id string, _ auth.Principal) (*threads.RunnerTokenRotation, error) {
		return &threads.RunnerTokenRotation{RunnerID: id, Token: "wrt_new.secret", PreviousUntil: "2026-11-22T10:15:00Z"}, nil
	}
	deps.RunnerTenancy.Deregister = func(context.Context, string, auth.Principal) error { return nil }
	return deps
}

// TestAdminRunnersGate: the page is host-admin only, and the denied view
// carries no token or fleet card.
func TestAdminRunnersGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		want   int
		denied bool
	}{
		{"anonymous", auth.Anonymous(), http.StatusUnauthorized, true},
		{"repo admin", auth.Principal{Name: "bob", Write: true}, http.StatusForbidden, true},
		{"host admin", settingsTestPrincipal(), http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: tc.p})
			rec := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil)
			if rec.Code != tc.want {
				t.Fatalf("GET /admin/runners = %d, want %d", rec.Code, tc.want)
			}
			body := rec.Body.String()
			if tc.denied {
				settingsMustContain(t, body, "Access restricted", "denied view")
				settingsMustNotContain(t, body, "admin-runner-registration-token", "denied view leaks no token")
				settingsMustNotContain(t, body, "build-01", "denied view leaks no fleet")
			} else if n := a11yH1Count(body); n != 1 {
				t.Fatalf("h1 count = %d, want 1", n)
			}
		})
	}
}

// TestAdminRunnersFleetAndMint: the default Fleet view renders every fact —
// including each runner's tenancy policy and trust class — the mint renders the
// token in place on the Mint token view (never a redirect/URL), and the views
// are server-rendered tabs.
func TestAdminRunnersFleetAndMint(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	for _, want := range []string{
		"build-01", "abcd1234", "2026-11-22T10:00:00Z",
		"acme/mono · run-1 · j-build", "active", "pending", "never",
		"revoke…", `data-danger-confirm="abcd1234"`,
		`href="/admin/runners"`,
		// Labels are wrapping chips, not a joined cell string.
		`class="chip mono">linux</span>`, `class="chip mono">docker</span>`,
		// The tenancy policy and the trust class are both stated in text.
		"all repositories", "acme/*, other/lib",
		"off — trusted code only", "on — untrusted fork code",
		// Every fact has its own labelled row in the card.
		"labels", "repository policy", "fork runs", "capacity", "last seen", "current job",
		// The Fleet view is the default; the mint form lives on its own view.
		`id="admin-runner-grid"`, "Fleet", "Mint token", "Security policy",
	} {
		settingsMustContain(t, body, want, "fleet page")
	}
	settingsMustNotContain(t, body, "mint token</button>", "the mint form is not on the fleet view")

	// The mint view carries the policy controls and the tenancy rule.
	mintView := settingsDo(t, s, http.MethodGet, "/admin/runners?tab=mint", nil, nil).Body.String()
	for _, want := range []string{
		"mint token</button>", `name="allow_all"`, `name="repos"`, `id="admin-runner-repos"`,
		`id="admin-runner-policy-help"`, "changed in place on the Fleet card",
		"All repositories", "…or only these repositories",
		`aria-current="page">Mint token</a>`,
	} {
		settingsMustContain(t, mintView, want, "mint view")
	}

	// Mint: the token is rendered in place on the mint view, with the TTL and
	// the shown-once warning; the response is 200 (no redirect that could log
	// the secret).
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"name": {"build-02"}, "allow_all": {"1"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{
		"wrg_YWRtaW4.ijkl9012.secret", `id="admin-runner-registration-token"`,
		"shown once", "2026-11-22T10:15:00Z", "copy",
	} {
		settingsMustContain(t, body, want, "token-once state")
	}
}

// TestAdminRunnersFleetIsAGrid pins the layout contract: one card per runner in
// a responsive grid (1 / 2 / 3 columns), never the old ten-column table, and no
// fleet-level horizontal scroll wrapper.
func TestAdminRunnersFleetIsAGrid(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	for _, want := range []string{
		`class="grid gap-3 md:grid-cols-2 xl:grid-cols-3"`,
		`id="admin-runner-card-abcd1234"`, `id="admin-runner-card-efgh5678"`,
		`id="admin-runner-name-abcd1234"`,
	} {
		settingsMustContain(t, body, want, "fleet grid")
	}
	// One card per row (two rows in the fixture).
	if n := strings.Count(body, `id="admin-runner-card-`); n != 2 {
		t.Fatalf("runner cards = %d, want 2", n)
	}
	settingsMustNotContain(t, body, "<table", "the fleet is not a table")
	settingsMustNotContain(t, body, "data-table", "the fleet is not a data table")
	settingsMustNotContain(t, body, "overflow-x-auto\">\n              <table", "no table wrapper around the fleet")
	// The policy is stated in full (wrapping), never summarised away.
	settingsMustContain(t, body, "acme/*, other/lib", "the whole policy is visible")
}

// TestAdminRunnersTabs: the three views are server-rendered ?tab= links with
// aria-current on the active one, Fleet by default, every view a deep link, and
// an unknown tab normalizes to Fleet (never a blank panel).
func TestAdminRunnersTabs(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	for _, tc := range []struct {
		query   string
		current string
		want    string
		notWant string
	}{
		{"", "Fleet", `id="admin-runner-grid"`, `id="admin-runner-mint"`},
		{"?tab=mint", "Mint token", `id="admin-runner-mint"`, `id="admin-runner-grid"`},
		{"?tab=security", "Security policy", `id="admin-runner-security"`, `id="admin-runner-grid"`},
		{"?tab=bogus", "Fleet", `id="admin-runner-grid"`, `id="admin-runner-security"`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			body := settingsDo(t, s, http.MethodGet, "/admin/runners"+tc.query, nil, nil).Body.String()
			settingsMustContain(t, body, `aria-current="page">`+tc.current+`</a>`, "the active view is marked")
			settingsMustContain(t, body, tc.want, "the active view rendered")
			settingsMustNotContain(t, body, tc.notWant, "inactive views are not rendered")
			settingsMustContain(t, body, `aria-label="Runner views"`, "the strip names itself")
			// The strip may scroll internally on a narrow viewport; it must be
			// the strip, never the page.
			settingsMustContain(t, body, `class="mb-4 min-w-0 overflow-x-auto"`, "the tab strip scrolls in place")
			if n := a11yH1Count(body); n != 1 {
				t.Fatalf("h1 count = %d, want 1", n)
			}
		})
	}
}

// TestAdminRunnersTabURLs: Fleet is the canonical default (no ?tab=).
func TestAdminRunnersTabURLs(t *testing.T) {
	v := &AdminRunnersView{}
	if v.FleetTabURL() != "/admin/runners" || v.MintTabURL() != "/admin/runners?tab=mint" ||
		v.SecurityTabURL() != "/admin/runners?tab=security" {
		t.Fatalf("tab urls = %q %q %q", v.FleetTabURL(), v.MintTabURL(), v.SecurityTabURL())
	}
	for raw, want := range map[string]string{"": "fleet", "FLEET": "fleet", " mint ": "mint", "security": "security", "x": "fleet"} {
		if got := adminRunnersTab(raw); got != want {
			t.Fatalf("adminRunnersTab(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestAdminRunnerMintPolicy: the page's mint lane passes the policy through
// (an explicit pattern list, and "all repositories"), refuses a mint with no
// policy at all (422, the typed values echoed back, nothing minted), and refuses
// a malformed pattern with the service's own message.
func TestAdminRunnerMintPolicy(t *testing.T) {
	var got []threads.MintInput
	deps := adminRunnerDeps()
	deps.RunnerTenancy.Mint = func(_ context.Context, in threads.MintInput, _ auth.Principal) (string, string, string, error) {
		pol, perr := threads.ValidateRunnerPolicy(in.RunnerPolicyInput)
		if perr != nil {
			return "", "", "", perr
		}
		// The service records the NORMALISED policy, so the fake does too.
		in.RunnerPolicyInput = threads.RunnerPolicyInput{AllowAll: pol.AllowAll, Repos: pol.Repos}
		got = append(got, in)
		return "ijkl9012", "wrg_YWRtaW4.ijkl9012.secret", "2026-11-22T10:15:00Z", nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})

	// An explicit allowlist (one per line and comma mixed, as an admin types it).
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners",
		url.Values{"name": {"build-03"}, "repos": {"acme/mono, other/*\nacme/mono"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pattern mint = %d: %s", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0].AllowAll || strings.Join(got[0].Repos, "|") != "acme/mono|other/*" {
		t.Fatalf("mint input = %+v", got)
	}
	if got[0].Name != "build-03" {
		t.Fatalf("mint name = %q", got[0].Name)
	}

	// "All repositories" is the checkbox lane.
	rec = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"allow_all": {"1"}}, nil)
	if rec.Code != http.StatusOK || len(got) != 2 || !got[1].AllowAll {
		t.Fatalf("allow-all mint = %d, input %+v", rec.Code, got)
	}

	// Neither: refused, and the form keeps what the admin typed.
	rec = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"repos": {"  "}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty-policy mint = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "at least one", "empty-policy refusal")
	settingsMustNotContain(t, rec.Body.String(), "wrg_", "no token on a refused mint")

	// A malformed pattern is the service's message, not a page-local copy.
	rec = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"repos": {"not-a-repo"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad-pattern mint = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "owner/name or owner/*", "bad-pattern refusal")
	settingsMustContain(t, rec.Body.String(), "not-a-repo", "the typed value is echoed back")
	if len(got) != 2 {
		t.Fatalf("a refused mint reached the seam: %+v", got)
	}
}

// TestAdminRunnerPolicyRowHonestEmpty: a registry record written before tenancy
// has no policy, and the fleet says so instead of implying "all"; a long
// allowlist is printed in FULL (wrapping inside its card), never truncated with
// a remainder count.
func TestAdminRunnerPolicyRowHonestEmpty(t *testing.T) {
	deps := adminBaseDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) {
		return []AdminRunnerRow{
			{ID: "legacy0001", Name: "old", Capacity: "1", State: "active"},
			{ID: "wide000001", Name: "wide", Capacity: "1", State: "active",
				Repos: []string{"a/one", "a/two", "a/three", "a/four", "a/five"}},
		}, nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	settingsMustContain(t, body, "none — no repository allowed", "an empty policy is stated honestly")
	settingsMustNotContain(t, body, "all repositories</dd>", "an empty policy is never shown as all")
	settingsMustContain(t, body, "a/one, a/two, a/three, a/four, a/five", "the full policy is visible")
	settingsMustNotContain(t, body, "and 2 more", "the policy is never silently truncated")
	// The row helpers agree with the render.
	row := AdminRunnerRow{Labels: "linux, docker"}
	if got := strings.Join(row.LabelChips(), "|"); got != "linux|docker" {
		t.Fatalf("LabelChips = %q", got)
	}
	if got := (AdminRunnerRow{Labels: "—"}).LabelChips(); len(got) != 0 {
		t.Fatalf("placeholder labels = %v", got)
	}
	if got := (AdminRunnerRow{Repos: []string{"a/one", "a/two"}}).PolicyText(); got != "a/one, a/two" {
		t.Fatalf("PolicyText = %q", got)
	}
}

func TestAdminRunnersEmptyState(t *testing.T) {
	deps := adminRunnerDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) {
		return nil, nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	settingsMustContain(t, body, "No runners are registered", "empty state")
	settingsMustContain(t, body, `href="/admin/runners?tab=mint"`, "the empty state offers the mint action")
	settingsMustContain(t, body, "Mint a registration token", "the empty action is named")
	settingsMustNotContain(t, body, `id="admin-runner-grid"`, "no empty grid")
}

func TestAdminRunnersRevokeRedirects(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners/revoke", url.Values{"id": {"abcd1234"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/admin/runners") {
		t.Fatalf("revoke location = %q", loc)
	}
	// The id is required.
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/revoke", url.Values{}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke without id = %d", rec.Code)
	}
}

// TestSettingsRunnersTabIsGone pins the pre-1.0 deletion: the repo settings tab
// and its route no longer exist.
func TestSettingsRunnersTabIsGone(t *testing.T) {
	for _, tab := range settingsRepoTabs {
		if tab.ID == "runners" {
			t.Fatal("settingsRepoTabs still lists runners")
		}
	}
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	// An unknown tab is not a tab: the page redirects to the default tab (the
	// pre-existing unknown-tab rule), so nothing renders a runners panel.
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/runners", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/settings/general") {
		t.Fatalf("GET settings/runners = %d %q, want a redirect to the default tab", rec.Code, rec.Header().Get("Location"))
	}
}

// TestAdminRunnersErrorPaths covers the honest degradation of the page's
// seams: unwired, a failing read, a failing mint and a failing revoke.
func TestAdminRunnersErrorPaths(t *testing.T) {
	p := settingsTestPrincipal()
	// Unwired Runners: the card says so, and no dead mint form renders.
	deps := adminBaseDeps()
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	settingsMustContain(t, body, "not wired on this build", "unwired runners")
	settingsMustNotContain(t, body, `id="admin-runner-grid"`, "unwired page has no fleet grid")
	settingsMustNotContain(t, body, `id="admin-runner-card-`, "unwired page has no runner card")

	// A failing fleet read renders its error.
	deps = adminBaseDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) {
		return nil, errFake
	}
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	body = settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	settingsMustContain(t, body, errFake.Error(), "fleet error")

	// Unwired mint (the fleet reads, no tenancy-aware mint) says so instead of
	// rendering a button that would 503.
	deps = adminBaseDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) { return nil, nil }
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	body = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"name": {"x"}}, nil).Body.String()
	settingsMustContain(t, body, "not wired on this build", "unwired mint")
	settingsMustNotContain(t, body, "mint token</button>", "an unwired mint renders no live button")
	// Both seams nil is the fully unwired build: the POST still degrades.
	deps = adminBaseDeps()
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	body = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{}, nil).Body.String()
	settingsMustContain(t, body, "not wired on this build", "fully unwired mint")

	deps = adminRunnerDeps()
	deps.RunnerTenancy.Mint = func(context.Context, threads.MintInput, auth.Principal) (string, string, string, error) {
		return "", "", "", errFake
	}
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	body = settingsDo(t, s, http.MethodPost, "/admin/runners", url.Values{"name": {"x"}, "allow_all": {"1"}}, nil).Body.String()
	settingsMustContain(t, body, errFake.Error(), "mint error")

	// Unwired revoke is a 503; a failing revoke renders the page with 409.
	deps = adminBaseDeps()
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/revoke", url.Values{"id": {"abcd1234"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired revoke = %d", rec.Code)
	}
	deps = adminRunnerDeps()
	deps.RevokeRunner = func(context.Context, string, auth.Principal) error { return errFake }
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: p})
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners/revoke", url.Values{"id": {"abcd1234"}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("failing revoke = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), errFake.Error(), "revoke error")

	// The revoked row/chip is a text state, never colour-only.
	row := AdminRunnerRow{State: "revoked"}
	if row.ChipClass() == "" || row.ChipLabel() != "revoked" {
		t.Fatalf("revoked chip = %q/%q", row.ChipClass(), row.ChipLabel())
	}
}

// TestAdminRunnerSecurityUnrestricted: the shipped default (an empty
// allowlist) reads as "unrestricted", both facts link their Configuration row,
// and the warning names the daemon socket, forks and "not a sandbox".
func TestAdminRunnerSecurityUnrestricted(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners?tab=security", nil, nil).Body.String()
	for _, want := range []string{
		`id="admin-runner-security"`, "Runner security",
		`href="/admin/config#row-threads.image_allow_prefixes"`,
		`href="/admin/config#row-threads.image_require_digest"`,
		"threads.image_allow_prefixes", "unrestricted — any public image",
		"threads.image_require_digest", "not required — a mutable tag is accepted",
		"not a sandbox", "daemon socket", "root-equivalent",
		"Fork runs stay off unless the runner opted in",
		"docs/threads.md#83-capacity-and-isolation", "docs/threads.md#86-mirroring-job-images",
	} {
		settingsMustContain(t, body, want, "runner security card")
	}
	// The security facts are their own view (the "Security policy" tab), not a
	// card squeezed above the fleet.
	if strings.Contains(body, `id="admin-runner-grid"`) {
		t.Fatal("the security view must not also render the fleet grid")
	}
}

// TestAdminRunnerSecurityConfigured: a configured allowlist and digest rule
// render their current values (never an editor — the card has no form).
func TestAdminRunnerSecurityConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.Threads.ImageAllowPrefixes = []string{"docker.io/library", "ak.example/dockerhub"}
	cfg.Threads.ImageRequireDigest = true
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	s.env.Cfg = cfg
	body := settingsDo(t, s, http.MethodGet, "/admin/runners?tab=security", nil, nil).Body.String()
	for _, want := range []string{
		"docker.io/library, ak.example/dockerhub",
		"required — every job image must be pinned to a @sha256: digest",
	} {
		settingsMustContain(t, body, want, "configured runner security")
	}
	settingsMustNotContain(t, body, "unrestricted — any public image", "a configured allowlist is not unrestricted")
}

// TestAdminRunnerSecurityUnwired: no effective configuration reports the fact
// instead of claiming "unrestricted".
func TestAdminRunnerSecurityUnwired(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	s.env.Cfg = nil
	body := settingsDo(t, s, http.MethodGet, "/admin/runners?tab=security", nil, nil).Body.String()
	settingsMustContain(t, body, "current image", "unwired security card")
	settingsMustNotContain(t, body, "unrestricted — any public image", "an unknown policy is not unrestricted")
}

// --- the in-place policy edit (POST /admin/runners/policy) -------------------

// TestAdminRunnerPolicyFormRenders: every live fleet row carries a no-JS policy
// editor — a details form naming the runner, prefilled with the RECORDED policy
// — and a revoked row carries none (the service would refuse it).
func TestAdminRunnerPolicyFormRenders(t *testing.T) {
	deps := adminRunnerDeps()
	deps.Runners = func(context.Context, auth.Principal) ([]AdminRunnerRow, error) {
		return []AdminRunnerRow{
			{ID: "abcd1234", Name: "build-01", Capacity: "2", State: "active", AllowAll: true, ForkRuns: false},
			{ID: "efgh5678", Name: "build-02", Capacity: "1", State: "pending",
				Repos: []string{"acme/*", "other/lib"}},
			{ID: "ijkl9012", Name: "old", Capacity: "1", State: "revoked"},
		}, nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	for _, want := range []string{
		`action="/admin/runners/policy"`,
		`id="admin-runner-policy-all-abcd1234"`, `name="allow_all"`,
		`id="admin-runner-policy-repos-abcd1234"`, `name="repos"`,
		"Repository policy for build-01 (abcd1234)",
		"Repository policy for build-02 (efgh5678)",
		"save policy", "…or only", "change policy…",
	} {
		settingsMustContain(t, body, want, "policy editor")
	}
	// The recorded policy is what the form shows: allow-all checked for the
	// first row, its patterns one per line for the second.
	settingsMustContain(t, body, `value="1" checked>`, "allow-all row is checked")
	settingsMustContain(t, body, "acme/*\nother/lib", "the recorded patterns prefill the field")
	// The revoked row renders neither the edit nor the revoke control.
	if strings.Contains(body, `id="admin-runner-policy-all-ijkl9012"`) {
		t.Fatal("a revoked row rendered a policy editor")
	}
	if strings.Contains(body, `data-danger-confirm="ijkl9012"`) {
		t.Fatal("a revoked row rendered a revoke form")
	}
}

// TestAdminRunnerPolicySubmitRedirects: the form posts through one seam call and
// redirects to the fleet, which then reports the new policy and the notice.
func TestAdminRunnerPolicySubmitRedirects(t *testing.T) {
	var gotID string
	var gotPol threads.RunnerPolicy
	var gotActor string
	deps := adminRunnerDeps()
	deps.RunnerTenancy.SetPolicy = func(_ context.Context, id string, in threads.RunnerPolicy, actor string) (threads.RunnerPolicy, error) {
		gotID, gotPol, gotActor = id, in, actor
		return threads.RunnerPolicy{Repos: []string{"acme/mono", "other/*"}}, nil
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy",
		url.Values{"id": {"ABCD1234"}, "repos": {"acme/mono, other/*\nacme/mono"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("policy edit = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/admin/runners?policy=abcd1234") ||
		!strings.Contains(loc, "tab=fleet") {
		t.Fatalf("policy edit location = %q", loc)
	}
	if gotID != "abcd1234" || gotActor != "alice" {
		t.Fatalf("seam call = id %q actor %q", gotID, gotActor)
	}
	if gotPol.AllowAll || strings.Join(gotPol.Repos, ",") != "acme/mono,other/*,acme/mono" {
		t.Fatalf("seam policy = %+v", gotPol)
	}
	// The redirect's GET names what changed and re-renders the fleet.
	body := settingsDo(t, s, http.MethodGet, "/admin/runners?policy=abcd1234", nil, nil).Body.String()
	settingsMustContain(t, body, "repository policy updated for runner abcd1234", "policy notice")
	// The allow-all lane submits the checkbox alone.
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy",
		url.Values{"id": {"abcd1234"}, "allow_all": {"1"}}, nil); rec.Code != http.StatusSeeOther || !gotPol.AllowAll {
		t.Fatalf("allow-all edit = %d, policy %+v", rec.Code, gotPol)
	}
	// The notice's id is bounded and trimmed (a hand-written URL cannot stretch
	// the banner), and a short id survives verbatim.
	if got := truncateQuery(" abcd1234 ", 40); got != "abcd1234" {
		t.Fatalf("truncateQuery trimmed = %q", got)
	}
	if got := truncateQuery(strings.Repeat("a", 80), 40); len(got) != 40 {
		t.Fatalf("truncateQuery cap = %d chars", len(got))
	}
}

// TestAdminRunnerPolicyErrors: the service's own message comes back with a
// status an operator can act on, the offending row's form is rendered open with
// the typed values, and the missing-id / cross-origin / unwired paths refuse.
func TestAdminRunnerPolicyErrors(t *testing.T) {
	deps := adminRunnerDeps()
	deps.RunnerTenancy.SetPolicy = func(_ context.Context, id string, in threads.RunnerPolicy, _ string) (threads.RunnerPolicy, error) {
		if _, perr := threads.ValidateRunnerPolicy(threads.RunnerPolicyInput{AllowAll: in.AllowAll, Repos: in.Repos}); perr != nil {
			return threads.RunnerPolicy{}, perr
		}
		if id == "revoked01" {
			return threads.RunnerPolicy{}, fmt.Errorf("%w: runner %s is revoked; its credential is dead and its policy cannot be changed", threads.ErrConflict, id)
		}
		return threads.RunnerPolicy{}, fmt.Errorf("%w: unknown runner %q", threads.ErrNotFound, id)
	}
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})

	// Empty policy: 422, the service's text, the row open with what was typed.
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"abcd1234"}, "repos": {"  "}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty-policy edit = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, `id="admin-runner-policy-error"`, "the error is announced")
	settingsMustContain(t, body, "at least one", "the service's own message")
	settingsMustContain(t, body, `class="min-w-0" open>`, "the offending row is rendered open")
	settingsMustContain(t, body, `aria-describedby="admin-runner-policy-error"`, "the open form points at the error")

	// A bad pattern is refused the same way, echoing the typed value.
	rec = settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"abcd1234"}, "repos": {"not-a-repo"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad-pattern edit = %d, want 422", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "owner/name or owner/*", "bad-pattern message")
	settingsMustContain(t, rec.Body.String(), "not-a-repo", "the typed value is echoed back")

	// A revoked runner is a 409 with its reason; an unknown one a 404.
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"revoked01"}, "allow_all": {"1"}}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("revoked edit = %d, want 409", rec.Code)
	} else {
		settingsMustContain(t, rec.Body.String(), "revoked", "revoked reason")
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"gone00001"}, "allow_all": {"1"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown runner edit = %d, want 404", rec.Code)
	}

	// The id is required; a cross-origin form is refused; an unwired seam 503s.
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"allow_all": {"1"}}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("edit without id = %d, want 400", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"abcd1234"}, "allow_all": {"1"}},
		map[string]string{"Origin": "http://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin edit = %d, want 403", rec.Code)
	}
	unwired := adminRunnerDeps()
	unwired.RunnerTenancy.SetPolicy = nil
	s = newSettingsServer(t, &settingsFakes{deps: unwired, principal: settingsTestPrincipal()})
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"abcd1234"}, "allow_all": {"1"}}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired edit = %d, want 503", rec.Code)
	}
}

// TestAdminRunnerMintAndRevokeGuards: the two POST lanes refuse a cross-origin
// form and an unreadable body, and the revoke redirect's notice renders on the
// Fleet view.
func TestAdminRunnerMintAndRevokeGuards(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	for _, target := range []string{"/admin/runners", "/admin/runners/revoke"} {
		rec := settingsDo(t, s, http.MethodPost, target, url.Values{"id": {"abcd1234"}},
			map[string]string{"Origin": "http://evil.example"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST %s = %d, want 403", target, rec.Code)
		}
		r := httptest.NewRequest(http.MethodPost, target, failingBody{})
		r.Host = "example.com"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://example.com")
		bodyRec := httptest.NewRecorder()
		s.mux.ServeHTTP(bodyRec, r)
		if bodyRec.Code != http.StatusBadRequest {
			t.Fatalf("unreadable POST %s = %d, want 400", target, bodyRec.Code)
		}
	}
	body := settingsDo(t, s, http.MethodGet, "/admin/runners?revoked=1", nil, nil).Body.String()
	settingsMustContain(t, body, "runner revoked", "the revoke notice renders")

	// Both POST lanes gate before they parse: an anonymous request is a 401.
	anon := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: auth.Anonymous()})
	for _, target := range []string{"/admin/runners", "/admin/runners/revoke"} {
		if rec := settingsDo(t, anon, http.MethodPost, target, url.Values{"id": {"abcd1234"}}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST %s = %d, want 401", target, rec.Code)
		}
	}
}

// TestAdminRunnerPolicyGate: the edit route is host-admin only (anonymous 401,
// a repo admin 403) and renders no form when the seam is unwired.
func TestAdminRunnerPolicyGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		want   int
		denied bool
	}{
		{"anonymous", auth.Anonymous(), http.StatusUnauthorized, true},
		{"repo admin", auth.Principal{Name: "bob", Write: true}, http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: tc.p})
			rec := settingsDo(t, s, http.MethodPost, "/admin/runners/policy", url.Values{"id": {"abcd1234"}, "allow_all": {"1"}}, nil)
			if rec.Code != tc.want {
				t.Fatalf("POST /admin/runners/policy = %d, want %d", rec.Code, tc.want)
			}
			if tc.denied {
				settingsMustNotContain(t, rec.Body.String(), `action="/admin/runners/policy"`, "denied view leaks no editor")
			}
		})
	}
	// Unwired: the recorded policy is still shown (the fact), the form is not.
	deps := adminRunnerDeps()
	deps.RunnerTenancy.SetPolicy = nil
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	body := settingsDo(t, s, http.MethodGet, "/admin/runners", nil, nil).Body.String()
	settingsMustNotContain(t, body, `action="/admin/runners/policy"`, "unwired page renders no dead form")
	settingsMustContain(t, body, "all repositories", "the recorded policy is still shown")
	// A nil view is a no-op (the helper runs on paths that may carry none).
	markPolicyWired(nil)
	// An unreadable body is a 400, never a policy write.
	r := httptest.NewRequest(http.MethodPost, "/admin/runners/policy", failingBody{})
	r.Host = "example.com"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body = %d, want 400", rec.Code)
	}
}

// failingBody is a request body that cannot be read (the ParseForm arm).
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
