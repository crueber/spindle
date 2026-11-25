package webui

// admin_maintenance_test.go — the Maintenance page (docs/PLAN-ADMIN.md §3.5):
// the three states (normal, restricted, unwired seam), the facts it renders
// (maintainer snapshot and task kinds, event-sink configuration, storage/cache
// + the filesystem holding cache.dir, the machine endpoints' current answers,
// the per-repo ops links), and the template's a11y floor. The page is a read
// view: a <form> or a .btn.primary on it is a regression.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
)

// maintenanceServer builds the page with a real cache dir (a temp dir, so the
// statfs projection measures something) and the given deps.
func maintenanceServer(t *testing.T, deps testDeps, cfg *config.Config) *Server {
	t.Helper()
	s := adminTestPage(t, deps, settingsTestPrincipal())
	if cfg != nil {
		s.env.Cfg = cfg
	}
	s.env.Version = "9.9.9"
	return s
}

func maintenanceMaintainer() AdminMaintainView {
	return AdminMaintainView{
		Passes:               12,
		LastPassSeconds:      90,
		CheckpointLagEntries: 3,
		CheckpointAgeSeconds: 45,
		MissingObjects:       1,
		RepairObjects:        2,
		FollowRefs:           7,
		Units: []SettingsKV{
			{Key: "checkpoint", Value: "ok 4"},
			{Key: "repair"},
			{Key: "bundle", Value: "idle 1"},
			{Key: "compact"},
			{Key: "rev-index"},
			{Key: "fsck"},
			{Key: "follow", Value: "ok 2"},
		},
	}
}

// TestAdminMaintenanceFacts is the normal state: every card's facts come from
// the seam/config the page says they come from.
func TestAdminMaintenanceFacts(t *testing.T) {
	deps := gcStatusDeps(&gcStarts{}, packages.GCStatus{})
	deps.Health = func() AdminHealthView {
		return AdminHealthView{State: "ready", Label: "ready", Summary: "The instance answers and is accepting work."}
	}
	deps.Maintain = func() (AdminMaintainView, bool) { return maintenanceMaintainer(), true }

	cfg := config.Defaults()
	cfg.Store.Backend = "s3"
	cfg.Store.Bucket = "spindle"
	cfg.Store.Prefix = "/walgit/"
	cfg.Cache.Dir = t.TempDir()
	cfg.Cache.Mode = "budget"
	cfg.Cache.MaxBytes = 20 << 30
	cfg.Cache.DiskHighWatermark = 0.75
	cfg.Cache.EvictIdleAfter = config.Duration(6 * 60 * 60 * 1e9)
	cfg.Events.SweepInterval = config.Duration(5 * 60 * 1e9)
	cfg.Server.Roles = []string{"serve", "maintain"}
	cfg.Placement.Maintain = []string{"big-*"}

	s := maintenanceServer(t, deps, cfg)
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()

	for _, want := range []string{
		// maintainer state + configured facts
		"Maintainer", "running on this host", "serve, maintain", "big-*", "1m",
		"maintenance.follow_interval", "maintenance.fsck_interval", "1w",
		"checkpoints", "on", "20GiB", "no cap", // snapshot
		"12", "1m 30s", "checkpoint lag", "3 entries", "missing / repaired objects", "1 / 2",
		"follow refs", "7",
		// task kinds incl. the honest empty rows
		"Task kinds", ">checkpoint</th>", "ok 4", "no data yet", ">follow</th>",
		// storage & cache
		"Storage &amp; cache", "s3", "spindle/walgit", "budget — evict",
		"cache size (max_bytes)", "20GiB", "disk high watermark", "75%",
		"evict idle after", "6h", "in use", "aria-valuenow=",
		// event bridge
		"Event bridge", "events role off", "v1 webhook", "not set",
		"events/cursor.json", "events_bridge_lag_entries", "no instance-wide redeliver",
		// health endpoints with current answers
		"Health endpoints", "9.9.9", "ready — The instance answers", "not evaluated here",
		// ops entry points
		"Per-repository ops", `href="/how-it-works#wal"`, `href="/api"`, `href="/"`,
		"packages-gc",
	} {
		settingsMustContain(t, body, want, "maintenance facts")
	}
	// The page's controls are the packages-GC card (two forms: an unconfirmed
	// dry run / a confirmed apply) and the always-available restart control
	// (docs/PLAN-ADMIN.md §5 as amended), which is not a primary.
	if got := strings.Count(body, "<form"); got != 3 {
		t.Fatalf("maintenance forms = %d, want 3 (GC dry-run, GC apply, restart)", got)
	}
	for _, form := range strings.Split(body, "<form")[1:] {
		action := form[:strings.Index(form, ">")+1]
		if !strings.Contains(action, `action="/admin/maintenance/gc"`) && !strings.Contains(action, `action="/admin/restart"`) {
			t.Fatalf("an unexpected form posts from the maintenance page: %.200s", form)
		}
	}
	if got := strings.Count(body, `class="btn primary`); got != 1 {
		t.Fatalf("maintenance primaries = %d, want 1 (the GC apply)", got)
	}
	settingsMustContain(t, body, `action="/admin/restart"`, "the restart control posts to its handler")
	settingsMustContain(t, body, `data-danger-confirm="restart"`, "the restart control is typed-confirm")
	settingsMustContain(t, body, "restarted by its supervisor", "the restart card states who restarts the process")
	settingsMustContain(t, body, "restart: no", "the restart card states the no-supervisor outcome")
	// One h1, shell landmarks, one current nav entry (the shell contract).
	if n := a11yH1Count(body); n != 1 {
		t.Fatalf("h1 count = %d, want 1", n)
	}
	if got := strings.Count(body, `aria-current="page"`); got != 1 {
		t.Fatalf("aria-current count = %d, want 1", got)
	}
}

// TestAdminMaintenanceBudgetWarning pins the boot guard's warning restated on
// the page: a budget larger than the filesystem is a warning, not a silent
// number.
func TestAdminMaintenanceBudgetWarning(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.Cache.DiskHighWatermark = 0.9
	cfg.Cache.MaxBytes = 1 << 50 // 1 PiB — larger than any test filesystem
	body := settingsDo(t, maintenanceServer(t, adminBaseDeps(), cfg), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "larger than the", "budget warning")
	settingsMustContain(t, body, "warn-line", "warning is styled as a warning")
}

// TestAdminMaintenanceBudgetAutoAndUnmeasured covers the unmeasurable
// filesystem and the auto/disk note, the two honest branches the normal state
// does not exercise.
func TestAdminMaintenanceBudgetAutoAndUnmeasured(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cache.Dir = "/nonexistent/spindle-maintenance-cache"
	cfg.Cache.Mode = "auto"
	cfg.Cache.MaxBytes = 1 << 20
	cfg.Cache.DiskHighWatermark = 0
	body := settingsDo(t, maintenanceServer(t, adminBaseDeps(), cfg), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "could not be read", "unmeasured filesystem")
	settingsMustContain(t, body, "could not be measured, so its share is unknown", "unknown budget share")
	settingsMustContain(t, body, "high watermark", "auto eviction note")
}

// TestAdminMaintenanceRestricted is the setup-only boot: honest cards instead of
// an error, and the machine endpoints still answer.
func TestAdminMaintenanceRestricted(t *testing.T) {
	deps := adminBaseDeps()
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "invalid"
		return v, nil
	}
	deps.Health = func() AdminHealthView {
		return AdminHealthView{State: "setup_required", Label: "setup required", Summary: "no valid configuration"}
	}
	deps.Maintain = nil // setup-only: no maintainer is constructed
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()

	body := settingsDo(t, maintenanceServer(t, deps, cfg), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	for _, want := range []string{
		"Restricted mode — the configuration file is invalid.", "restricted",
		"Restricted mode has no object store", "No effective storage configuration",
		"/readyz reports restricted", "Open configuration", "Health endpoints",
	} {
		settingsMustContain(t, body, want, "restricted maintenance state")
	}
	// No configuration facts are claimed in restricted mode.
	for _, unwanted := range []string{"store backend", "cache size (max_bytes)", "server.roles"} {
		settingsMustNotContain(t, body, unwanted, "restricted mode claims configuration facts")
	}
	settingsMustNotContain(t, body, "cfg.server.listen", "restricted mode claims configuration facts")
	if got := strings.Count(body, "<form"); got != 1 {
		t.Fatalf("restricted maintenance forms = %d, want 1 (the restart control)", got)
	}
	settingsMustContain(t, body, `action="/admin/restart"`, "restricted maintenance keeps the restart control")
}

// TestAdminMaintenanceUnwired covers nil seams and a nil config object: every
// card degrades honestly ("not wired on this build"), never a panic or a fake
// zero.
func TestAdminMaintenanceUnwired(t *testing.T) {
	deps := testDeps{AdminDeps: AdminDeps{Access: adminDeny}}
	s := adminTestPage(t, deps, settingsTestPrincipal())
	s.env.Cfg = nil
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	for _, want := range []string{
		"not wired on this build", "Storage is not wired on this build",
		"event-bridge configuration is not wired on this build",
		"readiness is not wired on this build", "Health endpoints", "unknown",
	} {
		settingsMustContain(t, body, want, "unwired maintenance state")
	}
	settingsMustContain(t, body, `action="/admin/restart"`, "unwired maintenance keeps the restart control")
}

// TestAdminMaintenanceNoUnits covers a live maintainer with no unit outcomes
// yet: the task-kind block says so instead of rendering an empty table.
func TestAdminMaintenanceNoUnits(t *testing.T) {
	deps := adminBaseDeps()
	deps.Maintain = func() (AdminMaintainView, bool) {
		return AdminMaintainView{Passes: 0}, true
	}
	body := settingsDo(t, maintenanceServer(t, deps, config.Defaults()), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "No unit outcomes recorded yet", "empty task-kind state")
	settingsMustContain(t, body, "not yet", "a maintainer that never completed a pass says so")
}

// TestAdminMaintenanceWiredButNotRunning covers the wired seam that reports
// ok=false (a serve-only process with a shared binding).
func TestAdminMaintenanceWiredButNotRunning(t *testing.T) {
	deps := adminBaseDeps()
	deps.Maintain = func() (AdminMaintainView, bool) { return AdminMaintainView{}, false }
	body := settingsDo(t, maintenanceServer(t, deps, config.Defaults()), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "not running on this host", "serve-only state")
	settingsMustNotContain(t, body, "Last pass &amp; counters", "no snapshot block without a snapshot")
}

// TestAdminMaintenanceBridgeConfigured covers configured sinks and the
// disabled-events-role state.
func TestAdminMaintenanceBridgeConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.Events.WebhookURL = "https://hooks.example.com/spindle"
	cfg.Events.WebhookSecret = "s3cret"
	cfg.Events.Sinks = []config.EventSink{{Name: "archive", Kind: "jsonl", Path: "/var/lib/spindle/events.jsonl"}}
	cfg.Server.Roles = []string{"events"}
	body := settingsDo(t, maintenanceServer(t, adminBaseDeps(), cfg), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	for _, want := range []string{`chip-open">configured`, "webhook signing secret", "sink archive", "jsonl"} {
		settingsMustContain(t, body, want, "configured bridge")
	}
	// The secret value is never rendered, only "set".
	settingsMustNotContain(t, body, "s3cret", "webhook secret must stay masked")
}

// TestAdminMaintenanceEndpointsStates pins the readiness answer projection:
// each state's code/tone words, and the n/a seam.
func TestAdminMaintenanceEndpointsStates(t *testing.T) {
	for _, tc := range []struct {
		state, code, answer string
	}{
		{"ready", "200", "ready — live"},
		{"warming", "503", "warming — prewarm"},
		{"draining", "503", "draining — stopping"},
		{"setup_required", "200", "setup required — no config"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			deps := adminBaseDeps()
			deps.Health = func() AdminHealthView {
				return AdminHealthView{State: tc.state, Label: strings.SplitN(tc.answer, " — ", 2)[0], Summary: strings.SplitN(tc.answer, " — ", 2)[1]}
			}
			s := maintenanceServer(t, deps, config.Defaults())
			eps := s.adminEndpoints(adminDeps())
			if len(eps) != 3 {
				t.Fatalf("endpoints = %d, want 3", len(eps))
			}
			if eps[1].Code != tc.code || eps[1].Answer != tc.answer {
				t.Fatalf("ready endpoint = %+v, want code %s answer %q", eps[1], tc.code, tc.answer)
			}
			if eps[1].Proves == "" || eps[0].Proves == "" || eps[2].Proves == "" {
				t.Fatal("every endpoint needs its one-line explanation")
			}
		})
	}
}

// TestAdminMaintenanceHelpers covers the display helpers' branches (spellings,
// defaults, and the empty states the page relies on).
func TestAdminMaintenanceHelpers(t *testing.T) {
	t.Parallel()
	if got := rolesLabel(nil); got != "serve, maintain, events (all)" {
		t.Fatalf("rolesLabel(nil) = %q", got)
	}
	if got := rolesLabel([]string{"serve"}); got != "serve" {
		t.Fatalf("rolesLabel = %q", got)
	}
	if !roleEnabled(nil, "events") || roleEnabled([]string{"serve"}, "events") || !roleEnabled([]string{"serve", "events"}, "events") {
		t.Fatal("roleEnabled wrong")
	}
	if got := globsLabel(nil, "all repositories"); got != "all repositories" {
		t.Fatalf("globsLabel(nil) = %q", got)
	}
	if got := globsLabel([]string{"a", "b"}, "x"); got != "a, b" {
		t.Fatalf("globsLabel = %q", got)
	}
	if got := cadenceText(config.Duration(-1), "not set"); got != "disabled" {
		t.Fatalf("negative cadence = %q", got)
	}
	if got := cadenceText(0, "not set"); got != "not set" {
		t.Fatalf("zero cadence = %q", got)
	}
	if got := cadenceText(config.Duration(90*1e9), "not set"); got != "90s" {
		t.Fatalf("90s cadence = %q", got)
	}
	if got := watermarkText(0); !strings.Contains(got, "90%") {
		t.Fatalf("watermark default = %q", got)
	}
	if got := watermarkText(0.5); got != "50%" {
		t.Fatalf("watermark = %q", got)
	}
	for mode, want := range map[string]string{
		"budget": "cache.max_bytes", "disk": "filesystem high watermark",
		"auto": "byte budget", "": "budget (default)", "custom": "custom",
	} {
		if got := cachePolicy(mode); !strings.Contains(got, want) {
			t.Errorf("cachePolicy(%q) = %q, want %q", mode, got, want)
		}
	}
	if clampPercent(-5) != 0 || clampPercent(150) != 100 || clampPercent(42) != 42 {
		t.Fatal("clampPercent wrong")
	}
	if fs := adminFilesystem("", 0.9); fs.Measured {
		t.Fatal("empty dir measured")
	}
	// A sink with no name is labelled by its kind, never a blank row.
	bridge := config.Defaults()
	bridge.Events.Sinks = []config.EventSink{{Kind: "jsonl"}}
	if card := adminBridgeCard(bridge, false); len(card.Rows) != 3 || card.Rows[2].Key != "sink jsonl" {
		t.Fatalf("unnamed sink rows = %+v", card.Rows)
	}
	// A measured filesystem in disk mode adds the watermark sentence. The
	// AdminFilesystem value is constructed, never measured: the host's temp
	// filesystem must not decide whether this test passes.
	disk := config.Defaults()
	disk.Cache.Dir = t.TempDir()
	disk.Cache.Mode = "disk"
	fs := AdminFilesystem{Measured: true, Type: "filesystem", Total: 64 << 30, Avail: 48 << 30, TotalText: "64 GiB", FreeText: "48 GiB", UsedPct: 25}
	if note, warn := adminBudgetNote(disk, fs); warn || !strings.Contains(note, "high watermark") {
		t.Fatalf("disk-mode budget note = %q warn=%v fs=%+v", note, warn, fs)
	}
	// The budget guard: larger than the filesystem warns, smaller does not.
	// Both are constructed filesystems, so the branch is not host-dependent.
	if note, warn := adminBudgetNote(disk, AdminFilesystem{Measured: true, Type: "tmpfs", Total: 8 << 30, Avail: 8 << 30, TotalText: "8.0 GiB", FreeText: "8.0 GiB"}); !warn || !strings.Contains(note, "larger than the 8.0 GiB tmpfs filesystem") {
		t.Fatalf("over-budget note = %q warn=%v", note, warn)
	}
	small := config.Defaults()
	small.Cache.MaxBytes = 1 << 20
	if note, warn := adminBudgetNote(small, fs); warn || !strings.Contains(note, "about 0% of this filesystem") {
		t.Fatalf("under-budget note = %q warn=%v", note, warn)
	}
	// An unmeasured filesystem is reported as unknown, never as a warning.
	if note, warn := adminBudgetNote(disk, AdminFilesystem{}); warn || !strings.Contains(note, "could not be measured") {
		t.Fatalf("unmeasured budget note = %q warn=%v", note, warn)
	}
	// Nil config / restricted mode report nothing rather than defaults.
	if card := adminBridgeCard(nil, false); card.Known || card.Configured {
		t.Fatalf("nil bridge card = %+v", card)
	}
	if card := adminBridgeCard(config.Defaults(), true); card.Known {
		t.Fatalf("restricted bridge card = %+v", card)
	}
	// A zero budget means "unbounded by configuration", never a fake 0.
	zero := config.Defaults()
	zero.Cache.MaxBytes = 0
	if note, warn := adminBudgetNote(zero, AdminFilesystem{}); warn || !strings.Contains(note, "not set") {
		t.Fatalf("zero budget note = %q warn=%v", note, warn)
	}
}

// TestAdminFilesystemMeasurement covers the real statfs path, but asserts
// properties that hold on any filesystem holding TMPDIR (a tiny tmpfs or a
// nearly full root) rather than host-specific sizes.
func TestAdminFilesystemMeasurement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := adminFilesystem(dir, 0.9)
	if !fs.Measured {
		t.Fatalf("an existing directory must measure: %+v", fs)
	}
	if fs.Type == "" || fs.TotalText == "" || fs.FreeText == "" {
		t.Fatalf("a measured filesystem needs a type and human sizes: %+v", fs)
	}
	if fs.Total <= 0 || fs.Avail < 0 || fs.Avail > fs.Total {
		t.Fatalf("nonsensical total/avail: %+v", fs)
	}
	if fs.UsedPct < 0 || fs.UsedPct > 100 {
		t.Fatalf("UsedPct = %d, want [0,100]", fs.UsedPct)
	}
	// Tight is exactly the watermark comparison, whatever the host's use is.
	if fs.Tight != (fs.UsedPct >= 90) {
		t.Fatalf("Tight = %v at %d%% with watermark 0.9", fs.Tight, fs.UsedPct)
	}
	// The watermark fallback is the eviction pass's own 90% (0 and >=1 both).
	for _, w := range []float64{0, 1, -0.5} {
		if got := adminFilesystem(dir, w); got.Tight != (got.UsedPct >= 90) {
			t.Fatalf("watermark %v: Tight = %v at %d%%, want the 90%% fallback", w, got.Tight, got.UsedPct)
		}
	}
	if got := adminFilesystem(dir, 0.5); got.Tight != (got.UsedPct >= 50) {
		t.Fatalf("watermark 0.5: Tight = %v at %d%%", got.Tight, got.UsedPct)
	}
	// A path that cannot be statfs'd is honestly unmeasured.
	if got := adminFilesystem("/nonexistent/spindle-cache-probe", 0.9); got.Measured {
		t.Fatalf("nonexistent dir measured: %+v", got)
	}
}

// TestAdminMaintenanceTemplateA11y pins the template's semantics: every
// aria-labelledby resolves, the progressbar is bounded and named, table headers
// are scoped, and nothing is focusable outside the document order.
func TestAdminMaintenanceTemplateA11y(t *testing.T) {
	deps := adminBaseDeps()
	deps.Maintain = func() (AdminMaintainView, bool) { return maintenanceMaintainer(), true }
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	body := settingsDo(t, maintenanceServer(t, deps, cfg), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()

	labelled := regexp.MustCompile(`aria-labelledby="([^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(labelled) < 5 {
		t.Fatalf("aria-labelledby targets = %d, want >= 5\n%s", len(labelled), settingsTruncate(body, 4000))
	}
	for _, m := range labelled {
		if !strings.Contains(body, `id="`+m[1]+`"`) {
			t.Errorf("aria-labelledby=%q has no matching id", m[1])
		}
	}
	for _, want := range []string{
		`role="progressbar"`,
		`aria-valuemin="0"`,
		`aria-valuemax="100"`,
		`aria-valuetext="`,
		`aria-label="filesystem use where the cache lives"`,
		`aria-label="maintainer task kinds and unit outcomes"`,
		`aria-label="health endpoints with their current answers"`,
		`scope="col"`,
		`scope="row"`,
	} {
		settingsMustContain(t, body, want, "maintenance a11y")
	}
	if strings.Contains(body, "tabindex=\"1\"") || strings.Contains(body, "tabindex=\"2\"") {
		t.Fatal("positive tabindex on the maintenance page")
	}
	if !strings.Contains(body, `<a class="skip-link" href="#app">`) || !strings.Contains(body, `<main id="app"`) {
		t.Fatal("skip link / main landmark missing")
	}
	if !strings.Contains(body, "<header") || !strings.Contains(body, "<footer") {
		t.Fatal("header/footer landmarks missing")
	}
	// The gateway gate is the area's one rule: a non-admin is denied before
	// any card renders.
	denied := adminTestPage(t, adminBaseDeps(), auth.Principal{Name: "bob", Write: true})
	if rec := settingsDo(t, denied, http.MethodGet, "/admin/maintenance", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

// TestAdminMaintenancePackagesUsage: the storage card's OCI registry section
// reads the maintainer-written index (one seam call), renders the total and
// the largest registries, and states that the numbers are as of the last pass
// — plus the three honest degradations (unwired, restricted, read error).
func TestAdminMaintenancePackagesUsage(t *testing.T) {
	deps := gcStatusDeps(&gcStarts{}, packages.GCStatus{})
	deps.PackagesUsage = func(context.Context) (packages.InstanceUsage, error) {
		return packages.InstanceUsage{
			Version:    1,
			TotalBytes: 3 << 20,
			UpdatedAt:  time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			Repos: map[string]int64{
				"acme/images": 2 << 20,
				"bob/tools":   1 << 20,
				"zoe/tiny":    512,
			},
		}, nil
	}
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	s := maintenanceServer(t, deps, cfg)
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	for _, want := range []string{
		"OCI registry", "Registry storage:", "3.0 MiB",
		"acme/images", "2.0 MiB", "bob/tools", "1.0 MiB", "zoe/tiny",
		"Largest registries (up to five)",
		"as of the last pass", "packages/usage.json", "[packages]",
	} {
		settingsMustContain(t, body, want, "registry storage card")
	}

	// Unwired: the honest note, never a zero.
	deps.PackagesUsage = nil
	s = maintenanceServer(t, deps, cfg)
	body = settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "Registry usage is not wired on this build", "unwired registry card")

	// A read failure is an alert line, not a fabricated number.
	deps.PackagesUsage = func(context.Context) (packages.InstanceUsage, error) {
		return packages.InstanceUsage{}, errors.New("index unreadable")
	}
	s = maintenanceServer(t, deps, cfg)
	body = settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "index unreadable", "registry read error")
	settingsMustNotContain(t, body, "Registry storage: <strong", "error must not also print a total")

	// Restricted: no store, so the storage card is the designed empty state.
	restricted := adminBaseDeps()
	restricted.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "invalid"
		return v, nil
	}
	restricted.PackagesUsage = func(context.Context) (packages.InstanceUsage, error) {
		return packages.InstanceUsage{}, nil
	}
	cfg = config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	rec := settingsDo(t, maintenanceServer(t, restricted, cfg), http.MethodGet, "/admin/maintenance", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restricted maintenance = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "No effective storage configuration", "restricted storage card")
	settingsMustNotContain(t, rec.Body.String(), "Registry storage:", "restricted boot claims a registry total")
}
