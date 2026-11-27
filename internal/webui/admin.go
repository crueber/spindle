// admin.go — the Administration area (docs/PLAN-ADMIN.md, docs/go/12_web_ui.md
// §2.12): the instance-scoped home that replaces the old single /setup page.
//
//	GET  /admin                   Overview (state, health, first-run checklist)
//	GET  /admin/config            Configuration — the whole schema editor
//	POST /admin/config            validate | save (the setup API's two steps)
//	POST /admin/config/auth-test  OIDC discovery probe (htmx fragment)
//	GET  /admin/access            Access — who signs in, who administers
//	GET  /admin/maintenance       Maintenance — the machinery's state
//	GET  /admin/runners           Instance runners — fleet, mint, policy edit, revoke
//	POST /admin/runners           mint / …/policy edit a runner's tenancy / …/revoke revoke
//
// # ACCESS (the one gate)
//
// AdminDeps.Access is the §3.4 rule (internal/server's setupAccess): open while
// the config file is absent/invalid or auth.mode == "none"; otherwise an
// authenticated host admin is required. Every page in the area gates through
// it, so a page can never leak what /api/v1/setup* protects (the page gate
// mirrors the API gate — AGENTS field lesson). A refusal renders the designed
// denied view WITH the API's status (401 unauthenticated / 403 non-admin), so
// credentials are never trainable into a 200 (AGENTS law 9).
//
// # WIRING (composition owns cmd/spindle/webui.go)
//
//	Access      the setup access rule (no env token: SPINDLE_SETUP_TOKEN is dead)
//	ConfigView  setup.BuildSchema over the running config + boot state
//	Validate    the API's POST /api/v1/setup/test (nothing written)
//	Save        the API's PUT /api/v1/setup (validate THEN atomic whole-file write)
//	AuthTest    setup.ProbeAuth (OIDC discovery)
//	Health      server.HealthFacts — the same facts /readyz reports
//	Maintain    Maintainer.Metrics (nil-safe: nil = the card is absent)
//	Repos       the registry's live-repo count (the first-run checklist)
//
// A nil field degrades that card ("not wired on this build") and never panics
// or 500s — the pre-existing seam discipline (settings.go WIRING header).
//
// # ONE EDITOR
//
// Configuration is the only page that writes, and it writes the WHOLE
// canonical spindle.toml (the byte-identical save contract). Access and
// Maintenance are read views: every edit affordance they show links to
// /admin/config#<section>. Maintenance additionally never invents a control
// plane: per-repo ops stay on the repository pages, and the event bridge has no
// instance-wide cursor/redeliver mutation to offer (PLAN-ADMIN §3.5).
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
)

// ---------------------------------------------------------------------------
// deps
// ---------------------------------------------------------------------------

// AdminDeps is the Administration area's service seam (see the WIRING header).
// Access is the only gate; the rest are reads plus the two existing writes
// (validate/save) and the discovery probe.
type AdminDeps struct {
	Access     func(r *http.Request, p auth.Principal) *auth.AuthError
	ConfigView func(ctx context.Context) (*AdminConfigView, error)
	Validate   func(ctx context.Context, values map[string]string) ([]AdminError, error)
	Save       func(ctx context.Context, values map[string]string) (AdminSaveResult, []AdminError, error)
	AuthTest   func(ctx context.Context, issuer, clientID, redirectURI string) (*AdminAuthTest, []AdminError)

	// Health is the readiness projection (boot state, drain/warm, warnings).
	Health func() AdminHealthView
	// Maintain is Maintainer.Metrics(); ok=false means the maintainer is not
	// running on this host (the Overview renders no maintainer card).
	Maintain func() (AdminMaintainView, bool)
	// Repos is the live-repository count for the first-run checklist (one
	// OwnerRepoCounts call, off the hot path). A nil seam leaves step 4
	// honestly unknown.
	Repos func(ctx context.Context) (int, error)

	// Reports / Audit / ResolveReport are the PLAN-V2 §M.3 host moderation
	// surfaces: the global queue, the audit viewer, and the resolve twin that
	// delegates to the repo writer. A nil field degrades its page honestly.
	Reports       func(ctx context.Context, p auth.Principal, state, after string, n int) (*AdminReportsView, error)
	Audit         func(ctx context.Context, p auth.Principal, after, n int, f AdminAuditFilter) (*AdminAuditView, error)
	ResolveReport func(ctx context.Context, owner, repo string, id int, p auth.Principal, action, note string) error

	// Runners is the INSTANCE runner fleet (docs/PLAN-INSTANCE-RUNNERS.md §5):
	// a host-admin surface. Runners lists the fleet; MintRunner mints a
	// one-time registration token (name optional); RevokeRunner revokes one
	// runner. A nil field renders the "not wired" card.
	Runners      func(ctx context.Context, p auth.Principal) ([]AdminRunnerRow, error)
	MintRunner   func(ctx context.Context, name string, p auth.Principal) (id, token, expires string, err error)
	RevokeRunner func(ctx context.Context, id string, p auth.Principal) error
	// AuditWrite records an administrative mutation in the host audit family
	// (PLAN-V2 §M.4's audit-completeness requirement): the Administration
	// area's config save today, ownership transfer's record next. Nil in the
	// restricted boot, where no collaboration wiring exists, no principal is
	// verified and no audit family is reachable — the save there IS the
	// recovery path (docs/features/16_moderation.md §14 says so).
	AuditWrite func(ctx context.Context, actor, action, target, detail string) error

	// PackagesGCStart / PackagesGCStatus / PackagesGCAttach are the
	// Maintenance page's one control (PLAN-ADMIN §8.6): start (or join) the
	// instance-scope packages-gc task, read the durable last-run summary plus
	// the live record, and attach the card to the task's narration. Nil
	// degrades the section to its honest not-wired state.
	PackagesGCStart  func(ctx context.Context, actor string, dryRun bool) (*AdminGCTask, error)
	PackagesGCStatus func(ctx context.Context) (packages.GCStatus, error)
	PackagesGCAttach func(ctx context.Context, id string) (api.TaskStream, bool, error)

	// PackagesUsage reads the stored registry usage index
	// (packages/usage.json): the instance total and per-repository bytes,
	// written by the maintainer's packages-gc pass (docs/features/14 §5). One
	// GET — never a bucket walk. Nil degrades the storage card's registry
	// section to its honest not-wired note.
	PackagesUsage func(ctx context.Context) (packages.InstanceUsage, error)

	// Restart is the restart seam behind POST /admin/restart. Composition binds
	// it to a graceful self-SIGTERM (the process's two-phase drain, then the
	// supervisor starts it again); it is bound in BOTH boot modes, because the
	// restricted boot is the recovery path a restart exists for. A nil seam
	// renders an honest 503 instead of pretending. Tests inject a fake and never
	// signal the test process.
	Restart func()

	// DirectoryUsers / DirectoryOrgs / UserOrgRoles / SetUserDisabled are the
	// instance users and organizations directory (PLAN-ADMIN §3.7): two bounded
	// reads, one organization walk shared by a page's rows, and the one
	// audited deactivate/reactivate transition. BindDirectory fills all four
	// from an identity service; a nil field renders the honest not-wired card.
	DirectoryUsers  func(ctx context.Context, q identity.DirectoryQuery) (*identity.DirectoryUserPage, error)
	DirectoryOrgs   func(ctx context.Context, q identity.DirectoryQuery) (*identity.DirectoryOrgPage, error)
	UserOrgRoles    func(ctx context.Context, names []string) (map[string][]identity.OrgMembership, bool, error)
	SetUserDisabled func(ctx context.Context, username string, disabled bool, reason string) error
}

// adminBound is the process-wide binding (one page provider per process, the
// settingsBound rule: BindAdmin runs once at composition time).
var adminBound AdminDeps

// BindAdmin installs the area's service seam.
func BindAdmin(d AdminDeps) {
	adminBound = d
	// A freshly composed area has requested no restart (the guard bounds one
	// process's lifetime; the tests rebind per server).
	resetAdminRestart()
}

// AdminBindings returns the installed seam for composition's wiring guard.
func AdminBindings() AdminDeps { return adminBound }

func adminDeps() AdminDeps { return adminBound }

// ---------------------------------------------------------------------------
// page model
// ---------------------------------------------------------------------------

// AdminNavItem is one entry of the area side-nav.
type AdminNavItem struct {
	Label  string
	Href   string
	Active bool
}

// AdminPage is the area's page model: the shared shell (nav, header, state
// chips, banners) plus the active page's data. One struct keeps the shell
// partials page-agnostic.
type AdminPage struct {
	Section string // "overview" | "config" | "access" | "maintenance"
	Title   string
	Purpose string
	Nav     []AdminNavItem

	// State chips: text, never colour alone (AGENTS a11y rule).
	HostAdmin  bool // signed in as a host admin
	FirstRun   bool // restricted boot with no config file (OIDC onboarding)
	Restricted bool // FileState == "invalid" (the restricted boot)
	FileState  string

	// Denied is the designed forbidden view (§2.4): recovery copy naming the
	// file/env path verbatim, never a bare 403.
	Denied        bool
	Detail        string
	SignedIn      bool
	PrincipalName string
	PrincipalMail string

	// Config page (today's whole editor).
	View        AdminConfigView
	RedirectURI string
	Issuer      string
	ClientID    string
	Saved       bool
	// Query / ChangedOnly are the Configuration page's view filters
	// (?q=<key>&changed=1). The server honours both without JS; the filter
	// island narrows the same rows live.
	Query       string
	ChangedOnly bool
	// ErrorLinks is the error summary: every server error with its row (or
	// section) link, announced in a role=alert list.
	ErrorLinks []AdminErrorLink
	// RequiresRestart is the saved-restart key list (the server list is
	// authoritative; live keys take effect immediately).
	RequiresRestart []string
	// RestartLinks is that list as key → #row-<key> anchors (the dedicated
	// "Restart required" card on Configuration).
	RestartLinks []AdminRestartKey
	// Restart is the restart control's own page state (POST /admin/restart):
	// nil on every page except its answer.
	Restart *AdminRestartView
	// RunnerSecurity is the /admin/runners facts-and-warning card (the image
	// allowlist / digest rule the runner fleet runs under).
	RunnerSecurity AdminRunnerSecurity
	// Backup is the spindle.toml.bak-<utc> file the first canonicalising
	// save kept.
	Backup string
	// SavePath is the file the last save wrote (from ?path=<abs>, or the
	// view's resolved target when the query does not carry one). The saved
	// banner names it: "<data-dir>/spindle.toml" was a lie under an explicit
	// --config.
	SavePath   string
	ValidateOK bool
	AuthTest   *AdminAuthTest
	AuthErrors []AdminError
	Notice     string
	Error      string

	// First-run OIDC onboarding (restricted boot with no config file).
	FirstRunIssuer         string
	FirstRunAllowedDomains string
	FirstRunClientID       string
	FirstRunClientSecret   bool
	FirstRunSessionSecret  bool
	FirstRunAdminGroup     string
	FirstRunSecretSuggest  string
	FirstRunRedirectURI    string
	FirstRunGroupsClaim    string

	// Overview.
	Instance AdminInstance
	Health   *AdminHealthView
	Maintain *AdminMaintainView

	// Moderation (PLAN-V2 §M.3/M.4): the global report queue and the audit
	// viewer. Read projections plus the resolve write; both pages gate through
	// the area's one access rule.
	Reports *AdminReportsView
	Audit   *AdminAuditView

	// Runners is the instance runner fleet (/admin/runners,
	// docs/PLAN-INSTANCE-RUNNERS.md §5): the fleet table, the mint form and the
	// one-time token state.
	Runners *AdminRunnersView

	// Directory is the instance users and organizations directory
	// (/admin/directory, PLAN-ADMIN §3.7): two bounded lists, the deactivate
	// control and its result banner.
	Directory *AdminDirectoryView

	// Access (read view; every fact links to its /admin/config#row-<key>).
	// Groups is the signed-in principal's LIVE verified group list (the denied
	// view shows a signed-in non-admin their own groups — never the configured
	// admin group or issuer).
	Groups []string
	Access AdminAccessCard

	// Maintenance (read view, §3.5): the maintainer's state and counters, the
	// event-sink configuration, storage/cache facts with the filesystem holding
	// cache.dir, the machine endpoints with their current answers, and the
	// packages-GC control (the page's one mutation, §8.6).
	Storage   AdminStorageCard
	Maint     AdminMaintainCard
	Bridge    AdminBridgeCard
	Endpoints []AdminEndpoint
	GC        *AdminGCStatus
	// GCTask is the just-started packages-GC pass (the POST's redirect target
	// carries its id); nil = no card on this render.
	GCTask *AdminGCTask
}

// AdminInstance is the Overview's instance card.
type AdminInstance struct {
	Rows []SettingsKV
}

// AdminHealthView is the readiness projection (the same facts /readyz reports).
type AdminHealthView struct {
	State          string // "ready" | "warming" | "draining" | "setup_required"
	Label          string // the human chip text (never the raw machine code)
	Summary        string
	Warnings       []string
	PrewarmPending int64
}

// AdminMaintainView is the maintainer's point-in-time counters.
type AdminMaintainView struct {
	Passes int64
	// LastPassSeconds is the DURATION of the last completed pass: that is what
	// Maintainer.Metrics() records (MetricsSnapshot.LastPassNanos is a
	// duration, not a wall clock), so the page must not present it as an age.
	LastPassSeconds      int64
	CheckpointLagEntries int64
	CheckpointAgeSeconds int64
	MissingObjects       int64
	RepairObjects        int64
	FollowRefs           int64
	Units                []SettingsKV
}

// AdminMaintainCard is the Maintenance page's maintainer section: whether a
// maintainer runs on this host (the role/placement facts), the configured
// cadence, and the live snapshot when the seam reports one. A nil seam renders
// the "not wired" state; a wired seam that reports ok=false means no
// maintainer runs here (the card then explains which fact excluded it).
type AdminMaintainCard struct {
	Known       bool // configuration is in force (cfg != nil)
	Wired       bool // AdminDeps.Maintain bound
	Running     bool // the seam reported a snapshot
	Restricted  bool // setup-only boot: there is no store to maintain
	Roles       string
	Placement   string
	Exclude     string
	Interval    string
	Follow      string
	Fsck        string
	Checkpoints bool
	MaxPack     string
	Disk        string
	Snapshot    *AdminMaintainView
	Units       []SettingsKV // kind → outcome summary ("" = no data yet)
}

// AdminBridgeCard is the event bridge's configuration facts (§3.5): whether
// sinks are configured, the sweep cadence, and whether this host runs the
// events role. Delivery cursors are per repository in the object store, so this
// card shows none — the Prometheus series on /metrics are the delivery signal.
type AdminBridgeCard struct {
	Known        bool // configuration is in force (cfg != nil and not restricted)
	Configured   bool
	RoleDisabled bool
	Restricted   bool
	Sweep        string
	Rows         []SettingsKV
}

// AdminStorageCard is the Maintenance page's storage/cache section. The kv rows
// are effective-configuration facts (the same values the Configuration page
// edits); the filesystem numbers are one statfs on the host holding cache.dir.
// Cache *contents* are deliberately not measured: that is a directory walk on a
// potentially multi-GiB cache, and no in-process gauge exists to read instead.
type AdminStorageCard struct {
	Configured bool // an effective configuration is in force
	Restricted bool
	Rows       []SettingsKV
	FS         AdminFilesystem
	BudgetNote string
	BudgetWarn bool
	// Packages is the OCI registry footprint (docs/features/14 §5): the
	// instance total and the largest repositories, read from the stored usage
	// index the maintainer pass writes. Nil only when the seam is unwired.
	Packages *AdminPackagesUsage
}

// AdminPackagesUsage is the storage card's registry section. Every number is
// "as of the last maintenance pass" and says so: the index is not rewritten on
// every push (that would put a second instance-level CAS on the hot path).
type AdminPackagesUsage struct {
	Known   bool   // the index was read
	Total   string // rendered instance total
	Repos   []SettingsKV
	Updated string // last reconciled, relative time
	Note    string
	Error   string
}

// AdminFilesystem is the filesystem holding cache.dir, measured now (statfs).
type AdminFilesystem struct {
	Measured  bool
	Type      string
	Total     int64
	Avail     int64
	UsedPct   int
	TotalText string
	FreeText  string
	Tight     bool // use at/above the configured eviction watermark
}

// AdminEndpoint is one machine endpoint with its current answer and the one
// line that says what the answer proves. It is a link plus a reading, never a
// dashboard: /metrics is not evaluated here.
type AdminEndpoint struct {
	Path   string
	Code   string // "200" | "503" | "n/a" | "text"
	Tone   string // chip tone: open | draft | closed | neutral
	Answer string
	Proves string
}

// AdminAccessCard is the Access page (§3.4, amended by PLAN-OIDC-ONLY §3.2):
// who can sign in, who administers, what THIS session carries, and the machine
// credentials. It is a read view — every fact names the config row it comes
// from and links to /admin/config#row-<key>; nothing here writes.
type AdminAccessCard struct {
	// Configured is false when no effective configuration exists (an unwired
	// build or a restricted boot); the page then renders its empty state.
	Configured bool

	// SignIn facts: server.auth.issuer / oauth_client_id / groups_claim /
	// admin_groups / anonymous_read.
	SignIn []AdminAccessFact
	// Admission facts: server.auth.allowed_domains / allowed_emails /
	// write_domains. Consequences are page copy, never validation.
	Admission []AdminAccessFact
	// SessionFacts: session_ttl / access_token_ttl / session_secret and
	// oauth_client_secret (both masked).
	SessionFacts []AdminAccessFact

	Issuer      string
	ClientID    string
	RedirectURI string
	GroupsClaim string
	AdminGroups []string

	// Live session state, from the credential boundary: the verified group
	// list the IdP delivered, set beside AdminGroups so a "valid config, wrong
	// group" lockout is diagnosable here.
	Admin           bool
	Groups          []string
	SessionIssuedAt string
	Verdict         string

	// Tokens is the static machine-token table (server.auth.tokens). The
	// secret never reaches the page: Credential is the mask or the env var
	// name that holds it.
	Tokens []AdminAccessToken
	// TokensNeverExpire counts configured tokens with no expires_at, so the
	// page can warn without re-deriving the condition in the template.
	TokensNeverExpire int
}

// AdminAccessFact is one read-only configuration fact: the human label, the
// effective value, and the config key it comes from (rendered as a link to the
// row the editor mounts at #row-<key>).
type AdminAccessFact struct {
	Label string
	Value string
	Key   string
}

// AdminAccessToken is one configured static machine credential.
type AdminAccessToken struct {
	// Identity is how the row is named: the configured Name, else the
	// "sha256:<prefix>" of its resolved secret, else a no-credential note.
	// Never the secret itself.
	Identity string
	// Name is the configured stable label ("" = unnamed).
	Name      string
	Principal string
	// Write is the row's scope: write grants push (and the API's write doors);
	// read-only is clone/fetch only. Neither can administer.
	Write bool
	// Credential is the display spelling of the secret's origin: SecretMask
	// for a stored literal, "token_env <NAME>" for an environment variable, or
	// the dropped-at-startup note when neither is set.
	Credential string
	// ExpiresAt is the configured RFC 3339 instant ("" = never), Expired says
	// the clock has passed it, and InvalidExpiry marks a value that could not
	// be parsed (the row is refused authentication, not silently immortal).
	ExpiresAt     string
	Expired       bool
	InvalidExpiry bool
	NeverExpires  bool
	// LastUsed is the recorded RFC 3339 instant of the last successful
	// authentication ("" = never observed), LastSource its remote address and
	// truncated user agent.
	LastUsed   string
	LastSource string
}

// adminSection is one row of the area's page map (label, route, h1, purpose).
type adminSection struct {
	ID      string
	Label   string
	Href    string
	Title   string
	Purpose string
}

// adminSections is the ONE page map: the side-nav, the h1 and the one-line
// purpose all read from here, so a new page cannot land with a nav entry that
// disagrees with its title.
var adminSections = []adminSection{
	{ID: "overview", Label: "Overview", Href: "/admin", Title: "Administration",
		Purpose: "The state of this instance at a glance, and what to do next."},
	{ID: "config", Label: "Configuration", Href: "/admin/config", Title: "Configuration",
		Purpose: "Edit every instance setting; validate and save the whole canonical spindle.toml."},
	{ID: "access", Label: "Access", Href: "/admin/access", Title: "Access",
		Purpose: "Who can sign in to this instance, and who can administer it."},
	{ID: "directory", Label: "Directory", Href: "/admin/directory", Title: "Users and organizations",
		Purpose: "Every account and organization on this instance, with deactivate and reactivate."},
	{ID: "maintenance", Label: "Maintenance", Href: "/admin/maintenance", Title: "Maintenance",
		Purpose: "Is the machinery healthy, and what is it doing."},
	{ID: "reports", Label: "Reports", Href: "/admin/reports", Title: "Moderation reports",
		Purpose: "The cross-repository abuse queue: resolve or dismiss what repo admins have not."},
	{ID: "audit", Label: "Audit", Href: "/admin/audit", Title: "Audit log",
		Purpose: "Every administrative action this instance recorded — configuration saves, access grants, ownership transfers, packages GC and moderation — newest page first."},
	{ID: "runners", Label: "Runners", Href: "/admin/runners", Title: "Instance runners",
		Purpose: "The CI runners this instance trusts: mint a registration token, watch the fleet, revoke one."},
}

// adminShell fills the shared shell state for a page: nav (with the current
// page), the page's h1/purpose, and the three state chips.
func (s *Server) adminShell(p auth.Principal, page *AdminPage, active string) {
	for _, sec := range adminSections {
		// Restricted mode answers only /admin and /admin/config (the read-only
		// pages are not mounted), so the nav must not offer links that 503.
		if s.env.Restricted && sec.ID != "overview" && sec.ID != "config" {
			continue
		}
		page.Nav = append(page.Nav, AdminNavItem{Label: sec.Label, Href: sec.Href, Active: sec.ID == active})
		if sec.ID == active {
			page.Section, page.Title, page.Purpose = sec.ID, sec.Title, sec.Purpose
		}
	}
	page.HostAdmin = p.Admin && !p.Anonymous
	page.SignedIn = !p.Anonymous && p.Name != ""
	page.PrincipalName = p.Name
	page.PrincipalMail = p.Email
	page.Groups = append([]string(nil), p.Groups...)
	page.FileState = page.View.FileState
	// First run is the restricted boot with no config file (the OIDC
	// onboarding card). A normally-booted instance whose config came from the
	// env overlay also reports FileState "absent" but is not first run.
	page.FirstRun = page.View.FirstRun
	page.Restricted = page.FileState == "invalid"
}

// adminRender writes the active page with the area's document title.
func (s *Server) adminRender(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, page *AdminPage) {
	title := "Administration"
	if page.Section != "overview" {
		title = page.Title + " · Administration"
	}
	s.page(w, r, p, status, "admin/"+page.Section, title, page)
}

// adminGate applies the area's one access rule. ok=false carries the status and
// the detail the denied view explains.
func (s *Server) adminGate(r *http.Request, p auth.Principal) (status int, detail string, ok bool) {
	if d := adminDeps(); d.Access != nil {
		if aerr := d.Access(r, p); aerr != nil {
			return authErrorStatus(aerr), aerr.Why, false
		}
		return 0, "", true
	}
	// Unwired seam fallback (an unwired build must still fail closed).
	if !p.Admin || p.Anonymous {
		return http.StatusForbidden, "this host requires an admin principal to view or change the instance", false
	}
	return 0, "", true
}

// requireAdminPage resolves the principal, applies the area gate, and renders
// the denied view when refused. ok=false means the response is written.
func (s *Server) requireAdminPage(w http.ResponseWriter, r *http.Request, active string) (auth.Principal, *AdminPage, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, nil, false
	}
	page := &AdminPage{}
	if status, detail, allowed := s.adminGate(r, p); !allowed {
		page.Denied = true
		page.Detail = detail
		s.adminShell(p, page, active)
		s.adminRender(w, r, p, status, page)
		return p, nil, false
	}
	return p, page, true
}

// ---------------------------------------------------------------------------
// routes
// ---------------------------------------------------------------------------

func init() { registerGroup((*Server).mountAdmin) }

// mountAdmin registers the area's routes. Tests call it directly so a sibling
// group's half-written route table cannot affect this group.
func (s *Server) mountAdmin() {
	m := &s.mux
	m.HandleFunc("GET /admin", s.adminOverviewPage)
	m.HandleFunc("GET /admin/config", s.adminConfigPage)
	m.HandleFunc("POST /admin/config", s.adminConfigSubmit)
	m.HandleFunc("POST /admin/config/auth-test", s.adminConfigAuthTest)
	m.HandleFunc("GET /admin/access", s.adminAccessPage)
	m.HandleFunc("GET /admin/maintenance", s.adminMaintenancePage)
	// The page's one control (PLAN-ADMIN §8.6): a dry run or a confirmed
	// apply, then its live card on the shared task stream.
	m.HandleFunc("POST /admin/maintenance/gc", s.adminGCStart)
	m.HandleFunc("GET /admin/maintenance/gc/stream", s.adminGCStream)
	m.HandleFunc("GET /admin/reports", s.adminReportsPage)
	// The resolve form posts here with owner/repo/id/action in the body: a
	// path carrying them would be a 5+-segment literal under /admin, which
	// overlaps the repo family's wildcard POST patterns and ServeMux refuses
	// ambiguous patterns (AGENTS field lesson).
	m.HandleFunc("POST /admin/reports/resolve", s.adminReportResolve)
	m.HandleFunc("GET /admin/audit", s.adminAuditPage)
	m.HandleFunc("GET /admin/runners", s.adminRunnersPage)
	m.HandleFunc("POST /admin/runners", s.adminRunnerMint)
	m.HandleFunc("POST /admin/runners/policy", s.adminRunnerPolicy)
	m.HandleFunc("POST /admin/runners/revoke", s.adminRunnerRevoke)
	m.HandleFunc("POST /admin/runners/drain", func(w http.ResponseWriter, r *http.Request) { s.adminRunnerFleet(w, r, "drain") })
	m.HandleFunc("POST /admin/runners/resume", func(w http.ResponseWriter, r *http.Request) { s.adminRunnerFleet(w, r, "resume") })
	m.HandleFunc("POST /admin/runners/rotate", func(w http.ResponseWriter, r *http.Request) { s.adminRunnerFleet(w, r, "rotate") })
	m.HandleFunc("POST /admin/runners/deregister", func(w http.ResponseWriter, r *http.Request) { s.adminRunnerFleet(w, r, "deregister") })
	// The users and organizations directory (PLAN-ADMIN §3.7): bounded reads
	// plus the audited deactivate/reactivate pair. The account travels in the
	// body (a literal multi-segment /admin path would overlap the repo
	// family's wildcard POST patterns).
	m.HandleFunc("GET /admin/directory", s.adminDirectoryPage)
	m.HandleFunc("POST /admin/directory/deactivate", s.adminDirectoryDeactivate)
	m.HandleFunc("POST /admin/directory/reactivate", s.adminDirectoryReactivate)
	// The restart control (docs/PLAN-ADMIN.md §5 as amended): typed confirm,
	// audit, one delayed self-signal. A literal path with no wildcard, mounted
	// in both boot modes (the restricted boot is the recovery path).
	m.HandleFunc("POST /admin/restart", s.adminRestartSubmit)
}

// ---------------------------------------------------------------------------
// Configuration — GET /admin/config
// ---------------------------------------------------------------------------

// adminConfigPage answers GET /admin/config: the whole schema editor. Access is
// the §3.4 rule, delegated to the binding; a refusal renders the shared denied
// view WITH the JSON API's status, so a denial is visible to clients instead of
// a misleading 200.
func (s *Server) adminConfigPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "config")
	if !ok {
		return
	}
	d := adminDeps()
	if d.ConfigView == nil {
		page.Error = "configuration schema is not wired on this build"
		s.adminShell(p, page, "config")
		s.adminRender(w, r, p, http.StatusOK, page)
		return
	}
	view, err := d.ConfigView(r.Context())
	if err != nil {
		log.Printf("webui admin config view %s: %v", r.URL.Path, err)
		s.errorPage(w, r, http.StatusServiceUnavailable, "configuration schema unavailable")
		return
	}
	if view == nil {
		view = &AdminConfigView{}
	}
	adminPrepareConfig(view)
	q := r.URL.Query()
	page.Query = strings.TrimSpace(q.Get("q"))
	page.ChangedOnly = q.Get("changed") == "1"
	// ?reset=<key> reseeds one row from its compiled default in the FORM —
	// nothing is written; Save still writes the whole file (§4.5).
	if seeded, ok := adminResetConfig(view, q.Get("reset")); ok {
		key, display := strings.TrimSpace(q.Get("reset")), seeded
		if display == "" {
			display = "empty"
		}
		page.Notice = fmt.Sprintf("%s reset to its compiled default (%s) in the form — nothing was saved. Press Save to write the file.", key, display)
	}
	adminCountConfig(view)
	adminFilterConfig(view, page.Query, page.ChangedOnly)
	page.View = *view
	page.ErrorLinks = adminErrorLinks(view, nil)
	page.RedirectURI = adminRedirectURI(r, view.PublicURL)
	page.Issuer = view.Issuer
	page.ClientID = view.ClientID
	if view.Access == "denied" || view.Access == "admin_required" {
		page.Denied = true
		page.Detail = view.Detail
		s.adminShell(p, page, "config")
		s.adminRender(w, r, p, http.StatusForbidden, page)
		return
	}
	page.Saved = q.Get("saved") == "1"
	page.Backup = q.Get("backup")
	page.SavePath = q.Get("path")
	if page.SavePath == "" {
		page.SavePath = view.SavePath
	}
	if rq := q.Get("restart"); rq != "" {
		page.RequiresRestart = strings.Split(rq, ",")
		page.RestartLinks = adminRestartKeyLinks(page.RequiresRestart)
	}
	page.ValidateOK = q.Get("validated") == "1"
	if n := q.Get("notice"); n != "" {
		page.Notice = n
	}
	if q.Get("error") != "" {
		page.Error = q.Get("error")
	}
	s.adminShell(p, page, "config")
	s.adminRender(w, r, p, http.StatusOK, page)
}

// adminConfigSubmit answers POST /admin/config: op=validate runs the full
// validator without saving; op=save validates THEN writes through the same
// functions the JSON API uses (the binding owns that two-step — this handler
// never coerces).
func (s *Server) adminConfigSubmit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := adminDeps()
	if status, detail, allowed := s.adminGate(r, p); !allowed {
		s.errorPage(w, r, status, detail)
		return
	}
	if d.ConfigView == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "configuration is not wired on this build")
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	values := settingsFormValues(r, "cfg.")
	op := r.PostFormValue("op")
	// Validate is the API's POST /test; save is the API's PUT — the binding
	// calls the same coercion + config.Validate for both (never duplicated
	// here). Save validates before it writes, inside the binding.
	if op == "validate" {
		if d.Validate == nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "configuration validation is not wired on this build")
			return
		}
		errs, err := d.Validate(r.Context(), values)
		if err != nil {
			log.Printf("webui admin config validate %s: %v", r.URL.Path, err)
			s.errorPage(w, r, http.StatusServiceUnavailable, "validation failed")
			return
		}
		if len(errs) > 0 {
			s.adminConfigReject(w, r, p, d, values, errs, "validation failed — nothing was saved")
			return
		}
		http.Redirect(w, r, "/admin/config?validated=1", http.StatusSeeOther)
		return
	}
	if d.Save == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "configuration save is not wired on this build")
		return
	}
	result, errs, err := d.Save(r.Context(), values)
	if err != nil {
		log.Printf("webui admin config save %s: %v", r.URL.Path, err)
		s.errorPage(w, r, http.StatusServiceUnavailable, "save failed")
		return
	}
	if len(errs) > 0 {
		s.adminConfigReject(w, r, p, d, values, errs, "validation failed — nothing was saved")
		return
	}
	// The save is committed; the audit record follows it (never before, and a
	// failed write never reverts a saved file — the audit-after-commit rule).
	if d.AuditWrite != nil {
		detail := "restart=" + strings.Join(result.RequiresRestart, ",")
		if result.Path != "" {
			detail += ";path=" + result.Path
		}
		if result.Backup != "" {
			detail += ";backup=" + result.Backup
		}
		if err := d.AuditWrite(r.Context(), p.Name, "config_save", "instance:config", detail); err != nil {
			log.Printf("webui admin config audit %s: %v", r.URL.Path, err)
		}
	}
	http.Redirect(w, r, "/admin/config?saved=1&restart="+url.QueryEscape(strings.Join(result.RequiresRestart, ","))+
		"&path="+url.QueryEscape(result.Path)+
		"&backup="+url.QueryEscape(result.Backup), http.StatusSeeOther)
}

// adminConfigReject re-renders /admin/config with the typed values and the
// server's per-key errors (422; 200 for htmx).
func (s *Server) adminConfigReject(w http.ResponseWriter, r *http.Request, p auth.Principal, d AdminDeps, values map[string]string, errs []AdminError, msg string) {
	view, verr := d.ConfigView(r.Context())
	if verr != nil || view == nil {
		view = &AdminConfigView{}
	}
	adminPrepareConfig(view)
	view.Errors = errs
	adminApplyValues(view, values, errs)
	adminCountConfig(view)
	if len(errs) > 0 {
		adminMarkFocus(view, errs[0].Key)
	}
	page := &AdminPage{
		View: *view, Error: msg, ErrorLinks: adminErrorLinks(view, errs),
		RedirectURI: adminRedirectURI(r, view.PublicURL),
		Issuer:      view.Issuer, ClientID: view.ClientID,
	}
	status := http.StatusUnprocessableEntity
	if settingsHTMX(r) {
		status = http.StatusOK // htmx swaps only 2xx bodies
	}
	s.adminShell(p, page, "config")
	s.adminRender(w, r, p, status, page)
}

// adminRedirectURI is the OIDC redirect URI to register at the issuer: the
// configured public URL when set, else the request's origin (§8.6).
func adminRedirectURI(r *http.Request, publicURL string) string {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = p
		}
		base = scheme + "://" + r.Host
	}
	return base + "/_auth/callback"
}

// adminConfigAuthTest answers POST /admin/config/auth-test: an htmx fragment
// (the discovery probe), never JSON.
func (s *Server) adminConfigAuthTest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := adminDeps()
	if status, detail, allowed := s.adminGate(r, p); !allowed {
		// An unbound access seam must not leave the outbound OIDC discovery
		// probe open to non-admins.
		s.errorPage(w, r, status, detail)
		return
	}
	if d.AuthTest == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "OIDC discovery test is not wired on this build")
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	_ = r.ParseForm()
	res, errs := d.AuthTest(r.Context(),
		strings.TrimSpace(r.PostFormValue("issuer")),
		strings.TrimSpace(r.PostFormValue("client_id")),
		strings.TrimSpace(r.PostFormValue("redirect_uri")))
	s.settingsRenderFragment(w, r, "admin/config", "admin-config-authtest", struct {
		Result *AdminAuthTest
		Errors []AdminError
	}{res, errs})
}

// ---------------------------------------------------------------------------
// administration data (the config editor + the area shell)
// ---------------------------------------------------------------------------

// AdminConfigView is what the page renders: the schema groups plus the
// setup API's boot state (file_state / errors) and access verdict.
type AdminConfigView struct {
	FileState string // "absent" | "valid" | "invalid"
	// SavePath is the absolute file a save writes: the file the process
	// loaded (an explicit --config / SPINDLE_CONFIG / WALGIT_CONFIG file, a
	// data-dir spindle.toml or its walgit.toml alias), else the first-run
	// <data-dir>/spindle.toml target. The page names it — an operator must be
	// able to see which file changed (config.SaveTargetPath).
	SavePath string
	// FirstRun is true only for the restricted boot with no config file (the
	// OIDC onboarding card). An env-configured instance also has FileState
	// "absent" but boots normally, so it is not first run.
	FirstRun  bool
	Access    string // "" (ok) | "denied" | "admin_required"
	Detail    string
	Errors    []AdminError
	Sections  []AdminConfigSection
	AuthMode  string
	PublicURL string
	Issuer    string
	ClientID  string
	// TOML is a preview of the SAVED configuration's canonical document —
	// exactly the bytes a zero-edit Save writes (config.MarshalTOML over
	// setup.BaseConfig). It deliberately excludes unsaved form edits: the
	// page is a whole-file editor, and only Save moves the file. The rendered
	// copy never calls it "read-only" (operators read that as "Save cannot
	// write"). Empty means the producer could not serialize it (the
	// disclosure is not rendered).
	TOML string

	// Filter accounting over the prepared view (before ?q/?changed narrow it).
	TotalKeys   int
	ChangedKeys int
	ShownKeys   int
}

// AdminConfigSection is one schema card.
type AdminConfigSection struct {
	Section string
	// Why is the one-line "why this matters" note (sectionWhy); it is page
	// copy, never validation.
	Why       string
	Essential []AdminConfigField
	Advanced  []AdminConfigField

	// Display accounting (computed before ?q/?changed filtering).
	Keys    int
	Changed int
	Shown   int
	// ForceAdvancedOpen opens the Advanced disclosure while a filter is
	// active: a matching row inside a collapsed group would otherwise be
	// invisible (the island does the same while the query is non-empty).
	ForceAdvancedOpen bool
}

// AdvancedHasErrors reports whether the collapsed Advanced group holds a row
// with a validation error, so the template opens that disclosure (a rejected
// advanced key must be visible, not hidden behind a toggle).
func (s AdminConfigSection) AdvancedHasErrors() bool {
	for _, f := range s.Advanced {
		if len(f.Errors) > 0 {
			return true
		}
	}
	return false
}

// AdminConfigField is one editable key row.
type AdminConfigField struct {
	Key     string
	Type    string // string|int|float|bool|duration|size|list|enum|toml|url|listen|path|rows
	Value   string
	Default string
	Example string
	Note    string
	Enum    []string
	Secret  bool
	// Masked is true when Value is the placeholder for a stored secret (the
	// real value never leaves the server). Masked rows are overwrite-only:
	// submitting the placeholder (or an empty field) keeps the stored secret.
	Masked   bool
	Advanced bool
	FromFile bool
	// CanReset offers the per-field "reset to default" link: a scalar,
	// non-secret row whose value differs from the compiled default. Struct
	// slices (rows) and secrets are excluded: a secret is overwrite-only and a
	// row set is not representable as one default spelling.
	CanReset bool
	// Focus marks the row whose control a rejected submission focuses (the
	// error-navigation island), so a keyboard/AT user lands on the cause.
	Focus  bool
	Errors []AdminError
	// Rows is a struct slice's repeating-row editor (server.auth.tokens,
	// bundles.strategy): one row per element plus a blank "add" row, each cell
	// named by setup.RowCellKey. Empty for scalar keys.
	Rows []setup.Row

	// Derived for the template (computed once, never in the template).
	Control string
	InputID string
	Bool    bool
	// Options is a select control's choices (Control == "select"), built from
	// the schema's enum set and the current value: an empty value renders an
	// "(unset)" option and a value outside the set renders as a "current: …"
	// option, so a save never silently rewrites an operator's value.
	Options []AdminConfigOption
}

// AdminConfigOption is one <option> of an enum control.
type AdminConfigOption struct {
	Value    string
	Label    string
	Selected bool
}

// AdminError is one server-side validation error.
type AdminError struct {
	Key     string
	Message string
	Value   string
}

// AdminErrorLink is one entry of the page's error summary: the message plus
// where to fix it. Href is the row anchor (#row-<key>), the section anchor for
// a key that only names a card (#<section>), or empty for a keyless failure
// (the summary prints it without a link).
type AdminErrorLink struct {
	Key     string
	Message string
	Href    string
}

// AdminSaveResult is a successful save.
type AdminSaveResult struct {
	Saved           bool
	RequiresRestart []string
	// Backup is the spindle.toml.bak-<utc> file kept before the first
	// canonicalising rewrite of an existing config ("" when none was made:
	// no file, or a byte-identical save).
	Backup string
	// Path is the absolute file the save wrote — the file the process loaded
	// (the explicit --config/SPINDLE_CONFIG/WALGIT_CONFIG file, else the
	// data-dir one). The page names it so an operator can see which file
	// changed; "" means the binding did not report it.
	Path string
}

// AdminAuthTest is a successful OIDC discovery probe.
type AdminAuthTest struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
	ClientIDPresent       bool
}

// adminPrepareConfig fills the per-field derived fields (input id, control
// kind, boolean value) so templates never do presentation logic, then
// partitions each section's rows into the open Essentials and the collapsed
// Advanced group using the one display-metadata list (adminFieldMeta via
// adminPrepareField). A producer that already partitioned (or a view built
// by a test) is normalised the same way, so the grouping has a single source
// of truth and the old "everything is essential" page cannot come back.
func adminPrepareConfig(v *AdminConfigView) {
	for i := range v.Sections {
		sec := &v.Sections[i]
		if sec.Why == "" {
			sec.Why = sectionWhy[sec.Section]
		}
		for j := range sec.Essential {
			adminPrepareField(&sec.Essential[j])
		}
		for j := range sec.Advanced {
			adminPrepareField(&sec.Advanced[j])
		}
		essential := make([]AdminConfigField, 0, len(sec.Essential)+len(sec.Advanced))
		advanced := make([]AdminConfigField, 0, len(sec.Advanced))
		for _, f := range sec.Essential {
			if f.Advanced {
				advanced = append(advanced, f)
			} else {
				essential = append(essential, f)
			}
		}
		for _, f := range sec.Advanced {
			if f.Advanced {
				advanced = append(advanced, f)
			} else {
				essential = append(essential, f)
			}
		}
		sec.Essential, sec.Advanced = essential, advanced
	}
}

// sectionWhy is the one-line "why this matters" copy for each schema card
// (PLAN-ADMIN Appendix A.2). It is page copy: the validator never reads it,
// and a section absent here renders without a note.
var sectionWhy = map[string]string{
	"server":       "How this instance answers HTTP: the listen address, request limits, and the URL it advertises to the world.",
	"auth":         "Who can sign in, and who becomes a host admin — the front door for every other permission.",
	"store":        "Where repository data actually lives — the bucket is the repository; everything else is a cache.",
	"cache":        "Local disk warmth: what is cached, how large it may grow, and what evicts.",
	"wal":          "The write-ahead log: batching, checkpoints and freshness — the durability and speed of every push.",
	"maintenance":  "The background loop that keeps repositories small and healthy (checkpoints, fsck, follow).",
	"placement":    "Which hosts serve and maintain which repositories in a multi-host deployment.",
	"compaction":   "Repacking git objects into fewer packs as they accumulate.",
	"bundles":      "Pre-generated clone bundles that make cold clones fast: schedules, retention, serving.",
	"lfs":          "Large-file storage: enabling, size limits, and how objects are served.",
	"upstream":     "The upstream origin this instance follows for read-only mirroring.",
	"git":          "How the git subprocess layer behaves: engine, filters, object format, history packs.",
	"telemetry":    "What the instance logs and exposes to metrics scrapers.",
	"events":       "The instance's outbound event webhook bridge.",
	"releases":     "Upload size limit for release assets.",
	"import":       "Bounds and SSRF rules for the repository import surface.",
	"attachments":  "Image size limit for issue and pull-request attachments.",
	"packages":     "Per-repository OCI registry limits and the default storage quota a repository's registry may fill.",
	"threads":      "Instance CI policy: run retention, first-time-fork approval, default job permissions, and the image allowlist / digest rule that decide what a job may run.",
	"webedit":      "Browser file editing: whether it is offered and the largest file the editor will commit.",
	"static_sites": "Publishes repositories as static sites on a dedicated listener and hostname — off by default, public repositories only.",
	"rate_limit":   "Request throttling for collaboration writes, issue/PR opens, reports and failed logins — off by default, enforced in-process, restart-required.",
	"search":       "Code search over each repository's serving copy — no index and no extra storage; control whether it is offered and how many searches may run at once.",
}

// adminPrepareField derives the template-facing fields of one row.
func adminPrepareField(f *AdminConfigField) {
	f.InputID = "admin-in-" + strings.ReplaceAll(f.Key, ".", "--")
	if meta, ok := adminFieldMeta[f.Key]; ok {
		if f.Example == "" {
			f.Example = meta.Example
		}
		if f.Note == "" {
			f.Note = meta.Note
		}
		if !f.Advanced {
			f.Advanced = meta.Advanced
		}
		if (f.Type == "" || f.Type == "string") && meta.Type != "" {
			f.Type = meta.Type
		}
	}
	// Enum choices are the schema's (internal/setup), never a page-local copy:
	// the producer normally carries them on the field, and this fallback keeps
	// a hand-built view (tests, a future producer) rendering the same set.
	if len(f.Enum) == 0 {
		f.Enum = setup.EnumValues(f.Key)
		if len(f.Enum) > 0 && (f.Type == "" || f.Type == "string") {
			f.Type = "enum"
		}
	}
	// A struct slice is a row editor, never a scalar text input: its
	// file/default signal is computed from the rendered rows upstream (a
	// secret row always renders the mask, which is not comparable to a
	// default).
	if f.Rows != nil {
		f.Control = "rows"
		return
	}
	// A secret value never reaches a template: the row renders the mask and is
	// overwrite-only (the save path keeps the stored value when the mask comes
	// back — setup.ParseValues).
	if f.Secret && f.Value != "" && f.Value != setup.SecretMask {
		f.Value = setup.SecretMask
		f.Masked = true
	}
	// An unset pointer scalar renders its EFFECTIVE value: a nil *bool means
	// the compiled default (config.Search.Enabled is nil = enabled), so the
	// switch must show on with the "default" chip instead of an off switch
	// labelled "disabled" for something that is enabled. The chip stays
	// honest because Value == Default (FromFile false) once seeded.
	if f.Type == "bool" && strings.TrimSpace(f.Value) == "" && strings.TrimSpace(f.Default) != "" {
		f.Value = f.Default
	}
	f.Bool = strings.EqualFold(strings.TrimSpace(f.Value), "true") || strings.TrimSpace(f.Value) == "1"
	switch f.Type {
	case "duration":
		// The API's Duration string ("1h0m0s") renders in the spelling the
		// TOML file and the validator accept ("1h").
		f.Value = fmtSpecDuration(f.Value)
		f.Default = fmtSpecDuration(f.Default)
	case "size":
		f.Value = fmtSpecSize(f.Value)
		f.Default = fmtSpecSize(f.Default)
	}
	switch f.Type {
	case "bool":
		f.Control = "bool"
	case "enum":
		f.Control = "select"
		f.Options = adminOptions(f.Enum, f.Value)
	case "int", "float":
		f.Control = "number"
	case "toml":
		f.Control = "textarea"
	case "list", "globs":
		f.Control = "list"
	default:
		f.Control = "text"
		if f.Secret {
			f.Control = "password"
		}
	}
	if f.Masked {
		// The mask always differs from the default; a live secret means "from
		// the file".
		f.FromFile = true
	} else {
		f.FromFile = f.Value != f.Default
	}
	// Per-field reset seeds Value from Default; it is honest only for a
	// scalar, non-secret row where one default spelling exists (PLAN-ADMIN
	// §4.5). Secrets stay overwrite-only, and a row set has no single default.
	f.CanReset = f.FromFile && !f.Secret && f.Rows == nil
}

// adminMatchesQuery reports whether one row key matches the Configuration
// page's key filter. The SAME semantics as the filter island's filterMatches
// (web/src/islands/filter.js): trimmed, case-insensitive substring, so the
// no-JS server filter and the live client filter agree row-for-row
// (admin_config_lift_test.go mirrors the island's test cases).
func adminMatchesQuery(key, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(key), q)
}

// adminCountConfig computes the unfiltered display counts: per section and for
// the whole view. Changed is the FromFile count (value differs from the
// compiled default; a stored secret counts as set because it cannot be
// compared). Called after prepare and any reset, before ?q/?changed narrows
// the sections.
func adminCountConfig(v *AdminConfigView) {
	total, changed := 0, 0
	for i := range v.Sections {
		sec := &v.Sections[i]
		sec.Keys, sec.Changed = 0, 0
		for _, fs := range [][]AdminConfigField{sec.Essential, sec.Advanced} {
			for _, f := range fs {
				sec.Keys++
				if f.FromFile {
					sec.Changed++
				}
			}
		}
		total += sec.Keys
		changed += sec.Changed
	}
	v.TotalKeys, v.ChangedKeys = total, changed
	v.ShownKeys = total
}

// adminFilterConfig narrows the visible sections and rows to the query and/or
// the changed-only view (PLAN-ADMIN §4.3/§4.4). Sections with nothing left
// are dropped entirely, and every remaining Advanced group opens (a matching
// row inside a collapsed detail would otherwise be invisible). With no filter
// this is a no-op, so the full page still mounts every row and the save
// payload is complete.
func adminFilterConfig(v *AdminConfigView, query string, changedOnly bool) {
	if strings.TrimSpace(query) == "" && !changedOnly {
		return
	}
	out := make([]AdminConfigSection, 0, len(v.Sections))
	shown := 0
	for i := range v.Sections {
		sec := v.Sections[i]
		filter := func(in []AdminConfigField) []AdminConfigField {
			matched := make([]AdminConfigField, 0, len(in))
			for _, f := range in {
				if adminMatchesQuery(f.Key, query) && (!changedOnly || f.FromFile) {
					matched = append(matched, f)
				}
			}
			return matched
		}
		sec.Essential = filter(sec.Essential)
		sec.Advanced = filter(sec.Advanced)
		sec.ForceAdvancedOpen = true
		sec.Shown = len(sec.Essential) + len(sec.Advanced)
		if sec.Shown == 0 {
			continue
		}
		shown += sec.Shown
		out = append(out, sec)
	}
	v.Sections = out
	v.ShownKeys = shown
}

// adminResetConfig implements ?reset=<key> (PLAN-ADMIN §4.5): seed that row's
// Value from its compiled Default and re-derive the display. NOTHING is
// saved — a link that visibly reseeds the form is honest; Save still writes
// the whole file. It returns the seeded spelling for the notice.
func adminResetConfig(v *AdminConfigView, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	for i := range v.Sections {
		sec := &v.Sections[i]
		for _, fs := range [][]AdminConfigField{sec.Essential, sec.Advanced} {
			for j := range fs {
				f := &fs[j]
				if f.Key != key || f.Secret || f.Rows != nil {
					continue
				}
				f.Value = f.Default
				f.Errors = nil
				adminPrepareField(f)
				return f.Default, true
			}
		}
	}
	return "", false
}

// adminKeyForMessage finds the first mounted row whose keyed error carries the
// same message, so a boot error listed only in view.Errors still links its row.
func adminKeyForMessage(v *AdminConfigView, msg string) string {
	for i := range v.Sections {
		for _, fs := range [][]AdminConfigField{v.Sections[i].Essential, v.Sections[i].Advanced} {
			for _, f := range fs {
				for _, e := range f.Errors {
					if e.Message == msg {
						return f.Key
					}
				}
			}
		}
	}
	return ""
}

// adminAnchorFor resolves a config key (including a struct-slice cell key like
// "server.auth.tokens[0].principal") to the row anchor the page renders, or ""
// when no mounted row owns it.
func adminAnchorFor(v *AdminConfigView, key string) string {
	if key == "" {
		return ""
	}
	base := key
	if i := strings.IndexByte(base, '['); i > 0 {
		base = base[:i]
	}
	for i := range v.Sections {
		for _, fs := range [][]AdminConfigField{v.Sections[i].Essential, v.Sections[i].Advanced} {
			for _, f := range fs {
				if f.Key == key || f.Key == base {
					return "row-" + f.Key
				}
			}
		}
	}
	return ""
}

// adminMarkFocus flags the row a rejected submission should focus. A struct
// slice cell error focuses its row editor (the first focusable cell); an
// unknown key stays unfocused and the summary link still lands on the card.
func adminMarkFocus(v *AdminConfigView, key string) {
	if key == "" {
		return
	}
	base := key
	if i := strings.IndexByte(base, '['); i > 0 {
		base = base[:i]
	}
	for i := range v.Sections {
		for _, fs := range [][]AdminConfigField{v.Sections[i].Essential, v.Sections[i].Advanced} {
			for j := range fs {
				if fs[j].Key == key || fs[j].Key == base {
					fs[j].Focus = true
					return
				}
			}
		}
	}
}

// adminErrorLinks builds the error summary (PLAN-ADMIN §3.3, M3 error
// navigation): every error with a link to its row, its section, or no link for
// a genuinely keyless failure. A rejected submit passes its errs; a boot-time
// view (restricted mode) passes nil and the per-field boot errors are
// collected, falling back to the keyless view.Errors.
func adminErrorLinks(v *AdminConfigView, errs []AdminError) []AdminErrorLink {
	src := errs
	if len(src) == 0 {
		if len(v.Errors) > 0 {
			// A boot-time view: view.Errors holds every message (including
			// keyless ones); the per-field copies only add the key, so map
			// each message to the first field that names it.
			for _, e := range v.Errors {
				key := e.Key
				if key == "" {
					key = adminKeyForMessage(v, e.Message)
				}
				src = append(src, AdminError{Key: key, Message: e.Message})
			}
		} else {
			// A view that carries only per-field errors (no aggregate list).
			for i := range v.Sections {
				for _, fs := range [][]AdminConfigField{v.Sections[i].Essential, v.Sections[i].Advanced} {
					for _, f := range fs {
						for _, e := range f.Errors {
							src = append(src, AdminError{Key: f.Key, Message: e.Message})
						}
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	out := make([]AdminErrorLink, 0, len(src))
	for _, e := range src {
		id := e.Key + "\x00" + e.Message
		if seen[id] {
			continue
		}
		seen[id] = true
		href := ""
		if e.Key != "" {
			if anchor := adminAnchorFor(v, e.Key); anchor != "" {
				href = "#" + anchor
			} else {
				href = "#" + setup.AnchorOf(e.Key)
			}
		}
		out = append(out, AdminErrorLink{Key: e.Key, Message: e.Message, Href: href})
	}
	return out
}

// adminOptions builds a select's choices: the schema enum set, plus an
// "(unset)" row when the stored value is empty and a "current: …" row when the
// stored value is outside the set. Those two extra rows make the control
// truthful (it always shows what is stored) and keep a save from silently
// rewriting a value the select cannot represent.
func adminOptions(enum []string, value string) []AdminConfigOption {
	out := make([]AdminConfigOption, 0, len(enum)+1)
	known := false
	for _, v := range enum {
		opt := AdminConfigOption{Value: v, Label: v, Selected: v == value}
		if opt.Selected {
			known = true
		}
		out = append(out, opt)
	}
	switch {
	case value == "":
		out = append([]AdminConfigOption{{Value: "", Label: "(unset)", Selected: true}}, out...)
	case !known:
		out = append(out, AdminConfigOption{Value: value, Label: "current: " + value, Selected: true})
	}
	return out
}

// adminApplyValues re-renders a failed submit with the operator's typed
// values (never the saved ones) and the per-key error lines.
func adminApplyValues(v *AdminConfigView, values map[string]string, errs []AdminError) {
	apply := func(f *AdminConfigField) {
		f.Errors = nil
		if f.Rows != nil {
			adminApplyRows(f, values)
		} else if sval, ok := values[f.Key]; ok && !f.Secret {
			// Never echo a secret back into the rejected form: a submitted
			// secret is dropped and the row keeps its mask (or stays empty).
			f.Value = sval
		}
		for _, e := range errs {
			// A keyless error belongs to the banner only (the page prints
			// view.Errors in full); copying it onto every row made a single
			// unknown failure look like 145 broken keys.
			//
			// A struct-slice cell error is keyed by its cell
			// ("bundles.strategy[0].schedule") and belongs to the field's row
			// editor, so the row is marked (and an Advanced group opens) even
			// though the failing cell is one line inside it.
			if e.Key != "" && (e.Key == f.Key || strings.HasPrefix(e.Key, f.Key+"[")) {
				f.Errors = append(f.Errors, e)
			}
		}
		adminPrepareField(f)
	}
	for i := range v.Sections {
		for j := range v.Sections[i].Essential {
			apply(&v.Sections[i].Essential[j])
		}
		for j := range v.Sections[i].Advanced {
			apply(&v.Sections[i].Advanced[j])
		}
	}
}

// adminApplyRows rebuilds a row editor from a rejected submit so the
// operator's edits survive (the cells are the typed values, not the saved
// ones). A typed secret is NEVER echoed: a non-empty typed secret renders as
// the mask, and a submitted mask stays the mask (so the next submit keeps the
// stored value, exactly like the scalar secret rows).
func adminApplyRows(f *AdminConfigField, values map[string]string) {
	edits := setup.ParseRowEdits(f.Key, values)
	if len(edits) == 0 || len(f.Rows) == 0 {
		return // the operator removed every row: keep the empty editor
	}
	cells := f.Rows[0].Cells
	rows := make([]setup.Row, 0, len(edits)+1)
	for _, edit := range edits {
		if edit.Remove {
			continue
		}
		row := setup.Row{
			Index: len(rows), Base: edit.Base,
			BaseKey:   setup.RowBaseKey(f.Key, len(rows)),
			RemoveKey: setup.RowRemoveKey(f.Key, len(rows)),
		}
		for _, cell := range cells {
			raw := edit.Cells[cell.Name]
			if cell.Secret && strings.TrimSpace(raw) != "" {
				raw = setup.SecretMask // never echo a typed secret back
			}
			row.Cells = append(row.Cells, setup.RowCell{
				Name: cell.Name, Key: setup.RowCellKey(f.Key, row.Index, cell.Name),
				Label: cell.Label, Type: cell.Type, Secret: cell.Secret, Value: raw,
				Bool: cell.Type == "bool" && strings.EqualFold(raw, "true"),
			})
			if row.Caption == "" && (cell.Name == "principal" || cell.Name == "name") {
				row.Caption = raw
			}
		}
		rows = append(rows, row)
	}
	// Always leave the blank add row.
	if len(rows) == 0 || !adminRowBlank(rows[len(rows)-1]) {
		blank := setup.Row{
			Index: len(rows), Base: -1, Caption: "new row",
			BaseKey:   setup.RowBaseKey(f.Key, len(rows)),
			RemoveKey: setup.RowRemoveKey(f.Key, len(rows)),
		}
		for _, cell := range cells {
			blank.Cells = append(blank.Cells, setup.RowCell{
				Name: cell.Name, Key: setup.RowCellKey(f.Key, blank.Index, cell.Name),
				Label: cell.Label, Type: cell.Type, Secret: cell.Secret,
			})
		}
		rows = append(rows, blank)
	}
	f.Rows = rows
}

// adminRowBlank reports whether a typed row carries no value (it is the
// placeholder add row, not an element).
func adminRowBlank(row setup.Row) bool {
	for _, cell := range row.Cells {
		v := strings.TrimSpace(cell.Value)
		if v == "" {
			continue
		}
		if cell.Type == "bool" && (v == "false" || v == "0") {
			continue
		}
		return false
	}
	return true
}

// adminFieldMeta is the display metadata the schema API does not carry
// (the `ex`/`note`/`advanced` columns of the former setup.js FIELDS table;
// internal/setup is the schema authority for types, enums and validation).
// It is data, not validation: the Go validator stays authoritative.
//
// Advanced is the ONLY grouping list: adminPrepareConfig partitions each
// section's rows with it into the open essentials and the collapsed
// <details> group (G18). A key absent from this table is essential. Enum
// choices do NOT live here — setup.BuildSchema carries them from the one
// schema table (setup.EnumValues), so the page cannot drift from the loader.
type adminFieldMetaT struct {
	Type     string
	Example  string
	Note     string
	Advanced bool
}

var adminFieldMeta = map[string]adminFieldMetaT{"server.listen": {Example: "0.0.0.0:8080", Note: "127.0.0.1:8080 = loopback only"},
	"server.http2":                     {Type: "bool", Example: "true", Advanced: true},
	"server.max_concurrent_requests":   {Advanced: true},
	"server.max_concurrent_per_repo":   {Advanced: true},
	"server.request_timeout":           {Type: "duration", Example: "1h", Advanced: true},
	"server.drain_timeout":             {Type: "duration", Example: "30s", Advanced: true},
	"server.serve_sync_timeout":        {Advanced: true},
	"server.serve_materialize_timeout": {Advanced: true},
	"server.max_push_bytes":            {Type: "size", Example: "2GiB", Advanced: true},
	"server.max_tree_log":              {Advanced: true},
	"server.auto_create_on_push":       {Type: "bool", Example: "true"},
	"server.accel_redirect":            {Advanced: true},
	"server.public_url":                {Type: "url", Example: "https://git.example.com"},
	"server.roles":                     {Type: "list", Example: "serve, maintain"},
	"server.cors_origins":              {Advanced: true},
	"server.secrets_key":               {Advanced: true},
	"server.ssh.listen":                {Example: "0.0.0.0:2222", Note: "empty = the SSH transport is disabled", Advanced: true},
	"server.ssh.external_port":         {Type: "int", Example: "12222", Note: "advertised in ssh:// clone URLs", Advanced: true},
	"server.ssh.host_key":              {Advanced: true},
	"server.ssh.host_key_env":          {Advanced: true},
	"server.auth.tokens":               {Example: "ci-bot", Note: "One row per static machine token: principal (the identity it authenticates); name is the optional stable label used for rotation and the last-used table (unique; unnamed rows show a hash prefix); expires_at is an optional RFC 3339 instant after which the token gets a 401 (omit = never expires, and the Access page warns); token_env names an env var holding the secret (read at startup) or type a literal token once — it renders masked and is kept unless you type a new one (rotate); write grants push access. Static tokens can never administer: host admin is the OIDC admin group only. To add a token, fill the blank row; tick remove to revoke one."},
	"server.auth.anonymous_read":       {Type: "bool", Example: "true", Note: "true = public browsing without signing in (recommended)"},
	"server.auth.issuer":               {Type: "url", Example: "https://id.example.com", Note: "discovery at <issuer>/.well-known/openid-configuration"},
	"server.auth.admin_groups":         {Type: "list", Example: "spindle-admin", Note: "OIDC groups granted host admin and the Administration area; required (an empty list is a guaranteed lockout)"},
	"server.auth.groups_claim":         {Example: "groups", Note: "ID-token claim carrying group membership (array of strings or one string; nested group trees must be flattened at the IdP)"},
	"server.auth.session_secret":       {Example: "openssl rand -hex 32", Note: "any random ≥ 32 bytes; rotating revokes every session and wgt_ token"},
	"store.backend":                    {Example: "filesystem"},
	"store.bucket":                     {Example: "spindle-test", Note: "s3 only"},
	"store.prefix":                     {Example: "spindle/"},
	"store.root":                       {Example: "/var/lib/spindle/store", Note: "filesystem backend only"},
	"store.max_retries":                {Advanced: true},
	"store.multipart_threshold":        {Type: "size", Example: "64MiB", Advanced: true},
	"store.multipart_part_size":        {Type: "size", Example: "16MiB", Advanced: true, Note: "must be ≤ multipart_threshold"},
	"store.s3.endpoint":                {Example: "http://127.0.0.1:9000", Note: "rustfs/MinIO"},
	"store.s3.region":                  {Example: "us-east-1"},
	"store.s3.access_key_env":          {Example: "AWS_ACCESS_KEY_ID"},
	"store.s3.secret_key_env":          {Example: "AWS_SECRET_ACCESS_KEY"},
	"store.s3.force_path_style":        {Type: "bool", Example: "true", Note: "true for rustfs/MinIO"},
	"cache.dir":                        {Example: "/var/cache/spindle"},
	"cache.mode":                       {Example: "budget"},
	"cache.max_bytes":                  {Type: "size", Example: "20GiB", Note: "everything on disk must fit this in budget mode"},
	"cache.disk_high_watermark":        {Advanced: true},
	"cache.evict_idle_after":           {Advanced: true},
	"cache.prewarm":                    {Advanced: true},
	"cache.prewarm_parallelism":        {Advanced: true},
	"cache.prewarm_ready_timeout":      {Advanced: true},
	"cache.ref_advert_entries":         {Advanced: true},
	"cache.object_info_entries":        {Advanced: true},
	"cache.bundle_list_entries":        {Advanced: true},
	"cache.remote_block_bytes":         {Advanced: true},
	"cache.remote_object_bytes":        {Advanced: true},
	"cache.shared_render_cache":        {Advanced: true},
	"cache.store_mount":                {Advanced: true},
	"wal.batch_window":                 {Advanced: true},
	"wal.max_batch":                    {Advanced: true},
	"wal.push_broker_url":              {Advanced: true},
	"wal.push_broker_token":            {Advanced: true},
	"wal.push_broker_buffer_bytes":     {Advanced: true},
	"wal.snapshot_every_entries":       {Advanced: true},
	"wal.checkpoint_interval":          {Type: "duration", Example: "1h"},
	"wal.checkpoint_tail_bytes":        {Advanced: true},
	"wal.cas_max_retries":              {Advanced: true},
	"wal.fsck_objects":                 {Advanced: true},
	"wal.check_connectivity":           {Advanced: true},
	"wal.freshness_ttl":                {Type: "duration", Example: "30s", Advanced: true},
	"wal.prefetch_packs":               {Advanced: true},
	"wal.prefetch_max_bytes":           {Advanced: true},
	"wal.remote_objects":               {Advanced: true},
	"maintenance.interval":             {Type: "duration", Example: "5m", Advanced: true},
	"maintenance.checkpoints":          {Advanced: true},
	"maintenance.max_pack_bytes":       {Type: "size", Example: "2GiB", Advanced: true},
	"maintenance.disk":                 {Advanced: true},
	"maintenance.host":                 {Advanced: true},
	"maintenance.fsck_interval":        {Advanced: true},
	"maintenance.follow_interval":      {Advanced: true},
	"compaction.enabled":               {Advanced: true},
	"compaction.factor":                {Advanced: true},
	"compaction.trigger_packs":         {Advanced: true},
	"compaction.trigger_bytes":         {Advanced: true},
	"compaction.lease_ttl":             {Advanced: true},
	"compaction.retention_superseded":  {Advanced: true},
	"compaction.engine":                {Advanced: true},
	"bundles.strategy":                 {Example: "daily (incremental, base weekly)", Note: "One row per bundle strategy: name (unique), kind (full|incremental), base (the incremental parent's name), schedule (6-field UTC cron or @macro), keep (full only), backfill_max, chain, filter (blob:none), refs (comma-separated), min_commits. Fill the blank row to add one; tick remove to retire it."},
	"bundles.min_commits":              {Type: "int", Example: "25"},
	"bundles.min_bytes":                {Type: "size", Advanced: true},
	"bundles.main_only":                {Advanced: true},
	"bundles.extra_refs":               {Advanced: true},
	"bundles.serve_via":                {Advanced: true},
	"bundles.signed_url_ttl":           {Advanced: true},
	"bundles.signed_url_for":           {Advanced: true},
	"bundles.advertise":                {Advanced: true},
	"bundles.advertise_filtered":       {Advanced: true},
	"bundles.require":                  {Advanced: true},
	"lfs.enabled":                      {Type: "bool", Example: "true"},
	"lfs.serve_via":                    {Advanced: true},
	"lfs.signed_url_ttl":               {Advanced: true},
	"lfs.max_object_bytes":             {Advanced: true},
	"upstream.git":                     {Example: "https://example.com/upstream.git"},
	"upstream.lfs":                     {Example: "https://example.com/upstream.git/info/lfs"},
	"upstream.token_env":               {Example: "UPSTREAM_TOKEN"},
	"upstream.follow":                  {Advanced: true},
	"git.binary":                       {Advanced: true},
	"git.upload_pack_engine":           {Advanced: true},
	"git.allow_filter":                 {Advanced: true},
	"git.allow_any_sha1_in_want":       {Advanced: true},
	"git.object_format":                {Advanced: true},
	"git.commit_graph":                 {Advanced: true},
	"git.commit_graph_changed_paths":   {Advanced: true},
	"git.history_pack":                 {Advanced: true},
	"git.max_wants":                    {Advanced: true},
	"telemetry.log_format":             {Advanced: true},
	"telemetry.log_filter":             {Example: "info,walgit=debug", Advanced: true},
	"telemetry.metrics":                {Advanced: true},
	"telemetry.lock_wait_warn":         {Advanced: true},
	"events.webhook_url":               {Type: "url", Example: "https://example.com/hook", Advanced: true},
	"events.webhook_secret":            {Advanced: true},
	"events.sweep_interval":            {Advanced: true},
	"releases.max_asset_bytes":         {Advanced: true},
	"import.clone_timeout":             {Advanced: true},
	"import.git_timeout":               {Advanced: true},
	"import.max_bytes":                 {Advanced: true},
	"import.max_refs":                  {Advanced: true},
	"import.max_concurrent":            {Advanced: true},
	"import.allow_private_networks":    {Advanced: true, Note: "the import/mirror SSRF policy is read at each import request: a save applies on the next import, no restart. Off (the default) refuses loopback and private sources."},
	"import.url_allowlist":             {Advanced: true, Note: "part of the same live policy (no restart). Empty = GitHub only, other hosts need this list or the dangerous confirm."},
	"import.allow_file_urls":           {Advanced: true, Note: "part of the same live policy (no restart). file:// sources are for tests/fixtures; leave off in production."},
	"attachments.max_image_bytes":      {Advanced: true},
	"packages.max_blob_bytes":          {Advanced: true},
	"packages.max_manifest_bytes":      {Advanced: true},
	"packages.upload_ttl":              {Advanced: true},
	"packages.quota_bytes":             {Type: "size", Example: "50GiB", Note: "instance default byte quota per repository's registry: stored blob + manifest bytes a push may add before it is refused (413, named). 0 = unlimited — the compatible default. A repository's settings [packages] quota_bytes overrides it, where 0 means unlimited for that repository."},
	"threads.image_allow_prefixes":     {Type: "list", Example: "docker.io/library, ak.example/dockerhub", Note: "an image allowlist: a job's container.image, every services.*.image, and every docker:// image (a step's or a local docker action's) must start with one of these host[:port]/path prefixes, or the job is refused at creation (a failed job naming the image and this list). Empty = no restriction. Turning it on without including an image a workflow uses breaks that workflow on its next run — add every registry prefix first."},
	"threads.image_require_digest":     {Type: "bool", Example: "true", Advanced: true, Note: "require @sha256: on workflow-named images: a moved tag can no longer change what CI runs. Independent of the allowlist (an allowed prefix can still be a mutable tag). Off by default because it refuses every tag-named image — pin the images first. The runner's own --image is the operator's declaration and is not governed here."},
	"webedit.max_bytes":                {Type: "size", Example: "1MiB", Note: "largest single file the browser editor accepts; binary upload is out of scope"},
	"rate_limit.enabled":               {Type: "bool", Example: "true", Note: "off by default: turning it on is an explicit operator decision. Enforced in-process, so a change needs a restart."},
	"rate_limit.collab_write":          {Example: "30/min", Note: "comments, reviews, reactions, review threads and review requests; N/s, N/min or N/h. Git pushes and reads are never limited."},
	"rate_limit.issue_open":            {Example: "10/h", Note: "opening an issue or a pull request"},
	"rate_limit.report":                {Example: "5/h", Note: "filing an abuse report"},
	"rate_limit.auth_fail":             {Example: "10/min", Note: "failed credential attempts per address: once spent, credential-bearing requests answer 429 with Retry-After"},
	"rate_limit.trust_proxy":           {Type: "bool", Example: "true", Advanced: true, Note: "key anonymous clients on the first X-Forwarded-For hop. Only enable when a reverse proxy in front of spindle overwrites that header; otherwise a client can mint unlimited limiter keys."},
	"search.enabled":                   {Type: "bool", Example: "true", Note: "on by default: search reads the serving copy and adds no storage. Off answers the search routes as not found."},
	"search.max_concurrent":            {Example: "2", Note: "concurrent searches per instance (0 = 2, ceiling 16). One search can fan out to four grep workers, so this bounds CPU a public repository can consume."},
}

// ---------------------------------------------------------------------------
// Overview — GET /admin
// ---------------------------------------------------------------------------

// adminOverviewPage answers GET /admin: instance state, health, and (while the
// config file is absent) the first-run checklist. Read-only — its one primary
// CTA is the checklist's "Open configuration" link.
func (s *Server) adminOverviewPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "overview")
	if !ok {
		return
	}
	d := adminDeps()
	if d.ConfigView != nil {
		if view, err := d.ConfigView(r.Context()); err == nil && view != nil {
			page.View = *view
		}
	}
	s.adminShell(p, page, "overview")
	page.Instance = s.adminInstance(page)
	if d.Health != nil {
		h := d.Health()
		if h.Label == "" {
			h.Label = h.State // a producer that binds only the code still renders words it has
		}
		page.Health = &h
	}
	if d.Maintain != nil {
		if m, ok := d.Maintain(); ok {
			page.Maintain = &m
		}
	}
	if page.FirstRun {
		s.adminFirstRunCard(r, page)
	}
	s.adminRender(w, r, p, http.StatusOK, page)
}

// adminFirstRunCard fills the restricted first-run OIDC onboarding: the five
// fields the operator must set, the exact redirect URI to register at the IdP,
// and a one-time session-secret suggestion to paste (a suggestion, never a
// write path — Save stays an ordinary validated whole-document save).
func (s *Server) adminFirstRunCard(r *http.Request, page *AdminPage) {
	cfg := s.env.Cfg
	if cfg == nil {
		cfg = config.Defaults()
	}
	a := &cfg.Server.Auth
	page.FirstRunIssuer = a.Issuer
	page.FirstRunAllowedDomains = strings.Join(a.AllowedDomains, ", ")
	page.FirstRunClientID = a.OAuthClientID
	page.FirstRunClientSecret = a.OAuthClientSecret != ""
	page.FirstRunSessionSecret = a.SessionSecret != ""
	page.FirstRunGroupsClaim = a.GroupsClaim
	page.FirstRunAdminGroup = strings.Join(a.AdminGroups, ", ")
	if page.FirstRunAdminGroup == "" {
		page.FirstRunAdminGroup = "spindle-admin"
	}
	page.FirstRunRedirectURI = adminRedirectURI(r, cfg.Server.PublicURL)
	page.FirstRunSecretSuggest = randomSessionSecret()
}

// adminInstance builds the instance card rows from the effective config — the
// values /healthz and the schema page report, never a second source.
func (s *Server) adminInstance(page *AdminPage) AdminInstance {
	out := AdminInstance{}
	add := func(k, v string) {
		if v == "" {
			v = "—"
		}
		out.Rows = append(out.Rows, SettingsKV{Key: k, Value: v})
	}
	mode := "configured"
	switch {
	case page.Restricted:
		mode = "restricted — the config file is invalid"
	case page.FirstRun:
		mode = "first run — defaults, no config file"
	}
	add("state", mode)
	add("version", s.env.Version)
	cfg := s.env.Cfg
	if cfg == nil {
		return out
	}
	add("authentication", authSummary(cfg))
	add("anonymous read", yesNo(cfg.Server.Auth.AnonymousRead))
	add("store", storeTarget(cfg))
	add("listen", cfg.Server.Listen)
	if cfg.Server.PublicURL != "" {
		add("public url", cfg.Server.PublicURL)
	}
	add("roles", rolesLabel(cfg.Server.Roles))
	return out
}

// ---------------------------------------------------------------------------
// Access — GET /admin/access (PLAN-ADMIN §3.4, amended by PLAN-OIDC-ONLY)
// ---------------------------------------------------------------------------

// adminAccessPage answers GET /admin/access: how this instance signs people
// in (OIDC), who the configured host administrators are, the signed-in
// principal's LIVE verified groups beside that configured set, the admission
// rules, the static machine tokens (never their secrets), and the session
// facts. It never writes: every fact links to its row in the one editor, and
// the discovery probe reuses POST /admin/config/auth-test (no route is
// invented here).
func (s *Server) adminAccessPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "access")
	if !ok {
		return
	}
	d := adminDeps()
	if d.ConfigView != nil {
		if view, err := d.ConfigView(r.Context()); err == nil && view != nil {
			page.View = *view
		}
	}
	s.adminShell(p, page, "access")
	page.Access = adminAccessCard(s.env.Cfg, p, r, s)
	s.adminRender(w, r, p, http.StatusOK, page)
}

// adminAccessCard builds the Access read view from the effective configuration
// and the credential boundary. It reads facts only: one GET of the token-usage
// object (absent/corrupt → "not recorded yet", never an error), no write, and no
// secret rendering — an env-held token's value is read only to derive its
// one-way hash identity.
func adminAccessCard(cfg *config.Config, p auth.Principal, r *http.Request, s *Server) AdminAccessCard {
	c := AdminAccessCard{
		Admin:  p.Admin,
		Groups: append([]string(nil), p.Groups...),
	}
	if s != nil {
		c.SessionIssuedAt = sessionIssuedAtLabel(r, s)
	}
	if cfg == nil {
		return c
	}
	c.Configured = true
	a := &cfg.Server.Auth
	c.Issuer = a.Issuer
	c.ClientID = a.OAuthClientID
	c.GroupsClaim = a.GroupsClaim
	c.AdminGroups = append([]string(nil), a.AdminGroups...)
	c.RedirectURI = adminRedirectURI(r, cfg.Server.PublicURL)
	c.Verdict = adminVerdict(p, a)

	fact := func(label, value, key string) AdminAccessFact {
		if strings.TrimSpace(value) == "" {
			value = "not set"
		}
		return AdminAccessFact{Label: label, Value: value, Key: key}
	}
	list := func(values []string, empty string) string {
		if len(values) == 0 {
			return empty
		}
		return strings.Join(values, ", ")
	}
	c.SignIn = []AdminAccessFact{
		fact("issuer", a.Issuer, "server.auth.issuer"),
		fact("client id", a.OAuthClientID, "server.auth.oauth_client_id"),
		fact("group claim", firstNonEmpty(a.GroupsClaim, "groups"), "server.auth.groups_claim"),
		fact("admin groups", list(a.AdminGroups, "none — a guaranteed lockout"), "server.auth.admin_groups"),
		{Label: "anonymous read", Value: yesNo(a.AnonymousRead), Key: "server.auth.anonymous_read"},
	}
	c.Admission = []AdminAccessFact{
		{Label: "allowed domains", Value: list(a.AllowedDomains, "none"), Key: "server.auth.allowed_domains"},
		{Label: "allowed emails", Value: list(a.AllowedEmails, "none"), Key: "server.auth.allowed_emails"},
		{Label: "write domains", Value: list(a.WriteDomains, "none — every admitted user may push"), Key: "server.auth.write_domains"},
	}
	secret := func(set bool) string {
		if set {
			return setup.SecretMask
		}
		return "not set"
	}
	c.SessionFacts = []AdminAccessFact{
		{Label: "session lifetime", Value: cadenceText(a.SessionTTL, "not set"), Key: "server.auth.session_ttl"},
		{Label: "access token lifetime", Value: cadenceText(a.AccessTokenTTL, "not set"), Key: "server.auth.access_token_ttl"},
		{Label: "session secret", Value: secret(a.SessionSecret != ""), Key: "server.auth.session_secret"},
		{Label: "client secret", Value: secret(a.OAuthClientSecret != ""), Key: "server.auth.oauth_client_secret"},
	}
	// Last-used observations are read ONCE per page render from their single
	// CAS object (06 §8.2.1). Absent/unreadable degrades to "not recorded
	// yet"; this is a read-only cache, never authentication authority.
	var usage *auth.TokenUsageDoc
	if s != nil && r != nil && s.env.API != nil {
		if doc, err := auth.LoadTokenUsage(r.Context(), s.env.API.Store); err == nil {
			usage = doc
		}
	}
	now := time.Now()
	if s != nil {
		now = s.env.now()
	}
	for _, t := range a.Tokens {
		secret := adminTokenSecret(t)
		name := strings.TrimSpace(t.Name)
		tok := AdminAccessToken{Name: name, Principal: t.Principal, Write: t.Write, Credential: setup.SecretMask}
		switch {
		case t.TokenEnv != "":
			// The env var NAME is not a secret; its value is read at startup
			// and only hashed here for the row's identity.
			tok.Credential = "token_env " + t.TokenEnv
		case secret == "":
			tok.Credential = "empty — dropped at startup"
		}
		tok.Identity = auth.TokenUsageIdentity(name, secret)
		if tok.Identity == "" {
			tok.Identity = "(no credential)"
		}
		if exp := strings.TrimSpace(t.ExpiresAt); exp == "" {
			tok.NeverExpires = true
			c.TokensNeverExpire++
		} else {
			tok.ExpiresAt = exp
			if parsed, err := time.Parse(time.RFC3339, exp); err != nil {
				tok.InvalidExpiry = true
			} else if !now.Before(parsed) {
				tok.Expired = true
			}
		}
		if usage != nil {
			if e, ok := usage.Entry(auth.TokenUsageIdentity(name, secret)); ok {
				tok.LastUsed = e.LastUsed
				tok.LastSource = e.Source
				if e.UserAgent != "" {
					tok.LastSource = strings.TrimSpace(e.Source + " · " + e.UserAgent)
				}
			}
		}
		c.Tokens = append(c.Tokens, tok)
	}
	return c
}

// adminTokenSecret resolves a row the way NewAuthService does (token_env
// overrides the literal). Authentication reads it once at startup; this page
// reads the same source only to derive a one-way hash prefix (and to report a
// row that resolves to nothing). The value is never rendered, logged or stored.
func adminTokenSecret(t config.StaticToken) string {
	if t.TokenEnv != "" {
		return os.Getenv(t.TokenEnv)
	}
	return t.Token
}

// adminVerdict states the admin predicate's outcome in plain language, naming
// the claim and the configured group so the operator can fix the IdP mapping
// (or their own group membership) without reading the source.
func adminVerdict(p auth.Principal, a *config.Auth) string {
	claim := a.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	groups := strings.Join(a.AdminGroups, ", ")
	if p.Anonymous {
		return "not signed in — sign in to see your groups"
	}
	if p.Admin {
		return "admin — your token's " + claim + " claim lists a configured admin group"
	}
	got := "(none)"
	if len(p.Groups) > 0 {
		got = strings.Join(p.Groups, ", ")
	}
	return "not admin — your token's " + claim + " claim lists " + got + ", which does not include the configured admin group (" + groups + ")"
}

// sessionIssuedAtLabel reports the session cookie's issued-at when the request
// carries a verifiable one (the Access diagnostics show staleness).
// randomSessionSecret is a 32-byte hex session-secret suggestion (the card's
// copyable one-time value). It is generated per render and never stored: the
// operator pastes it into the form, Save writes it.
func randomSessionSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func sessionIssuedAtLabel(r *http.Request, s *Server) string {
	if s.auth.SessionInfo == nil {
		return ""
	}
	if t, ok := s.auth.SessionInfo(r); ok {
		return t.Format(time.RFC3339)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Maintenance — GET /admin/maintenance (read view, PLAN-ADMIN §3.5)
// ---------------------------------------------------------------------------

// adminMaintenancePage answers GET /admin/maintenance: is the machinery healthy,
// and what is it doing. It never writes and never adds a second control plane —
// per-repo ops stay on the repository pages, and edits link to the one editor.
func (s *Server) adminMaintenancePage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "maintenance")
	if !ok {
		return
	}
	s.maintenanceLoadView(r.Context(), page)
	s.adminShell(p, page, "maintenance")
	s.maintenanceFill(r.Context(), page)
	// A just-started (or joined) packages-GC pass: the POST redirected here
	// with its id, and the card attaches to the task's stream (law 7).
	if id := strings.TrimSpace(r.URL.Query().Get("gc")); id != "" {
		dry := r.URL.Query().Get("gc_dry") == "1"
		page.GCTask = &AdminGCTask{
			ID:     id,
			Kind:   packages.TaskKindGC,
			DryRun: dry,
			Title:  gcTaskTitle(dry),
			Stream: "/admin/maintenance/gc/stream?id=" + url.QueryEscape(id),
		}
	}
	s.adminRender(w, r, p, http.StatusOK, page)
}

// maintenanceFill builds every Maintenance card from the same seams and the
// same configuration the page has always read (the POST error path re-renders
// through it too, so a rejected submit never loses the page's facts).
// maintenanceLoadView reads the config view BEFORE the shell renders: the
// shell derives Restricted/FirstRun from it (the shared state chips), so the
// cards and the chrome cannot disagree about the boot state.
func (s *Server) maintenanceLoadView(ctx context.Context, page *AdminPage) {
	if d := adminDeps(); d.ConfigView != nil {
		if view, err := d.ConfigView(ctx); err == nil && view != nil {
			page.View = *view
		}
	}
}

func (s *Server) maintenanceFill(ctx context.Context, page *AdminPage) {
	d := adminDeps()
	cfg := s.env.Cfg
	page.Storage = adminStorageCard(cfg, page.Restricted)
	page.Storage.Packages = s.adminPackagesUsage(ctx, d, page.Restricted)
	page.Maint = adminMaintainCard(cfg, d, page.Restricted)
	page.Bridge = adminBridgeCard(cfg, page.Restricted)
	page.Endpoints = s.adminEndpoints(d)
	page.GC = s.adminGCCard(ctx, d, page.Restricted)
}

// adminPackagesUsage reads the stored registry usage index (one GET; never a
// bucket walk) and renders the total plus the largest registries. The index is
// written by the maintainer pass, so the card states the reading's age rather
// than pretending to be live.
func (s *Server) adminPackagesUsage(ctx context.Context, d AdminDeps, restricted bool) *AdminPackagesUsage {
	if restricted {
		return &AdminPackagesUsage{
			Note: "Restricted mode has no object store, so no registry exists to measure.",
		}
	}
	if d.PackagesUsage == nil {
		return &AdminPackagesUsage{Note: "Registry usage is not wired on this build."}
	}
	u, err := d.PackagesUsage(ctx)
	if err != nil {
		return &AdminPackagesUsage{Error: err.Error()}
	}
	out := &AdminPackagesUsage{Known: true, Total: humanBytes(u.TotalBytes)}
	if u.UpdatedAt != "" {
		out.Updated = relTime(u.UpdatedAt)
	}
	for _, r := range u.Largest(5) {
		out.Repos = append(out.Repos, SettingsKV{Key: r.Repo, Value: humanBytes(r.Bytes)})
	}
	out.Note = "The registry index (packages/usage.json) is written by the maintainer's packages-gc pass, so these numbers are as of the last pass on this repository set — a push since then is not in them. Per-repository quotas and retention are set in each repository's settings [packages] section; the Packages page shows a repository's live counter."
	return out
}

// adminGCCard builds the packages-GC section: the maintainer's own cadence and
// grace numbers, the stored last runs, and the live task (when one runs). A nil
// seam is honest about being unwired; a restricted boot has no store to sweep.
func (s *Server) adminGCCard(ctx context.Context, d AdminDeps, restricted bool) *AdminGCStatus {
	gc := &AdminGCStatus{
		Cadence: cadenceText(config.Duration(packages.DefaultGCInterval), ""),
		Grace:   cadenceText(config.Duration(packages.DefaultBlobGrace), ""),
	}
	if restricted {
		gc.Restricted = true
		gc.Scheduled = "Restricted mode has no object store, so no registry exists to sweep until a valid configuration is saved and the server restarted."
		return gc
	}
	gc.Scheduled = "The maintainer sweeps each repository's registry on its own due gate (" +
		gc.Cadence + " minimum spacing) during every maintenance pass; this control forces a pass now instead of waiting for it."
	if d.PackagesGCStatus == nil {
		gc.NotWired = true
		return gc
	}
	st, err := d.PackagesGCStatus(ctx)
	if err != nil {
		gc.Error = err.Error()
		return gc
	}
	built := AdminGCStatusFrom(st, gc.Cadence, gc.Grace)
	built.Scheduled = gc.Scheduled
	return built
}

// adminStorageCard builds the storage/cache facts. It reads the effective
// configuration (the owner of every value) and one statfs on the filesystem
// holding cache.dir; it never walks the cache.
func adminStorageCard(cfg *config.Config, restricted bool) AdminStorageCard {
	c := AdminStorageCard{Restricted: restricted}
	if cfg == nil || restricted {
		return c
	}
	c.Configured = true
	maxBytes := int64(cfg.Cache.MaxBytes)
	c.Rows = []SettingsKV{
		{Key: "store backend", Value: cfg.Store.Backend},
		{Key: "store target", Value: storeTarget(cfg)},
		{Key: "cache dir", Value: cfg.Cache.Dir},
		{Key: "eviction policy", Value: cachePolicy(cfg.Cache.Mode)},
		{Key: "cache size (max_bytes)", Value: fmtSpecSize(strconv.FormatInt(maxBytes, 10))},
		{Key: "disk high watermark", Value: watermarkText(cfg.Cache.DiskHighWatermark)},
		{Key: "evict idle after", Value: cadenceText(cfg.Cache.EvictIdleAfter, "not set")},
	}
	c.FS = adminFilesystem(cfg.Cache.Dir, cfg.Cache.DiskHighWatermark)
	c.BudgetNote, c.BudgetWarn = adminBudgetNote(cfg, c.FS)
	return c
}

// adminFilesystem measures the filesystem holding dir (one statfs, no walk).
func adminFilesystem(dir string, watermark float64) AdminFilesystem {
	fs := AdminFilesystem{}
	info, ok := config.CacheSpace(dir)
	if !ok || info.Total <= 0 {
		return fs
	}
	fs.Measured = true
	fs.Type = info.FSType
	if fs.Type == "" {
		fs.Type = "filesystem"
	}
	fs.Total, fs.Avail = info.Total, info.Avail
	used := info.Total - info.Avail
	if used < 0 {
		used = 0
	}
	fs.UsedPct = int(math.Round(float64(used) / float64(info.Total) * 100))
	fs.UsedPct = clampPercent(fs.UsedPct)
	fs.TotalText, fs.FreeText = humanBytes(info.Total), humanBytes(info.Avail)
	if watermark <= 0 || watermark >= 1 {
		watermark = 0.9 // the eviction pass's own fallback
	}
	fs.Tight = float64(fs.UsedPct) >= watermark*100
	return fs
}

// adminBudgetNote compares the configured cache budget with the filesystem
// holding cache.dir — the boot guard's warning, restated where an operator
// looks for it. BudgetWarn marks the case where writes can hit ENOSPC before
// eviction helps.
func adminBudgetNote(cfg *config.Config, fs AdminFilesystem) (string, bool) {
	maxBytes := int64(cfg.Cache.MaxBytes)
	if maxBytes <= 0 {
		return "cache.max_bytes is not set — the eviction budget is unbounded by configuration.", false
	}
	size := fmtSpecSize(strconv.FormatInt(maxBytes, 10))
	if !fs.Measured {
		return "cache.max_bytes is " + size + "; the filesystem holding cache.dir could not be measured, so its share is unknown.", false
	}
	if maxBytes > fs.Total {
		return "cache.max_bytes is " + size + ", larger than the " + fs.TotalText + " " + fs.Type +
			" filesystem holding cache.dir: writes can hit ENOSPC before eviction helps — lower cache.max_bytes or move cache.dir.", true
	}
	pct := int(math.Round(float64(maxBytes) / float64(fs.Total) * 100))
	note := "cache.max_bytes is " + size + " — about " + strconv.Itoa(clampPercent(pct)) + "% of this " + fs.Type + " filesystem."
	if cfg.Cache.Mode == "disk" || cfg.Cache.Mode == "auto" {
		note += " The eviction pass also runs when filesystem use reaches the high watermark."
	}
	return note, false
}

// adminMaintainCard builds the maintainer section: the configured facts when an
// effective configuration is in force, plus the snapshot when the seam reports
// one.
func adminMaintainCard(cfg *config.Config, d AdminDeps, restricted bool) AdminMaintainCard {
	c := AdminMaintainCard{Restricted: restricted}
	if cfg != nil && !restricted {
		c.Known = true
		c.Roles = rolesLabel(cfg.Server.Roles)
		c.Placement = globsLabel(cfg.Placement.Maintain, "all repositories")
		c.Exclude = globsLabel(cfg.Placement.MaintainExclude, "none")
		c.Interval = cadenceText(cfg.Maintenance.Interval, "not set")
		c.Follow = cadenceText(cfg.Maintenance.FollowInterval, "not set")
		c.Fsck = cadenceText(cfg.Maintenance.FsckInterval, "not set")
		c.Checkpoints = cfg.Maintenance.Checkpoints
		if cfg.Maintenance.MaxPackBytes > 0 {
			c.MaxPack = fmtSpecSize(strconv.FormatInt(int64(cfg.Maintenance.MaxPackBytes), 10))
		} else {
			c.MaxPack = "no cap"
		}
		c.Disk = cfg.Maintenance.Disk
	}
	if d.Maintain != nil {
		c.Wired = true
		if m, ok := d.Maintain(); ok {
			c.Running = true
			c.Snapshot = &m
			c.Units = m.Units
		}
	}
	return c
}

// adminBridgeCard builds the event-bridge section from configuration. The
// bridge keeps no instance-level aggregate (each repository's cursor is a store
// object), so the card reports configuration and points at the metrics series
// rather than inventing a number.
func adminBridgeCard(cfg *config.Config, restricted bool) AdminBridgeCard {
	c := AdminBridgeCard{Restricted: restricted}
	if cfg == nil || restricted {
		return c
	}
	c.Known = true
	c.RoleDisabled = !roleEnabled(cfg.Server.Roles, "events")
	c.Sweep = cadenceText(cfg.Events.SweepInterval, "not set")
	c.Configured = cfg.Events.WebhookURL != "" || len(cfg.Events.Sinks) > 0
	setValue := func(v string) string {
		if v == "" {
			return "not set"
		}
		return "set"
	}
	c.Rows = []SettingsKV{
		{Key: "v1 webhook", Value: setValue(cfg.Events.WebhookURL)},
		{Key: "webhook signing secret", Value: setValue(cfg.Events.WebhookSecret)},
	}
	for _, sink := range cfg.Events.Sinks {
		name := sink.Name
		if name == "" {
			name = sink.Kind
		}
		c.Rows = append(c.Rows, SettingsKV{Key: "sink " + name, Value: sink.Kind})
	}
	return c
}

// adminEndpoints reports the three machine endpoints with their current answer
// and what each proves. The readiness answer is the same projection the
// Overview renders (server.HealthFacts), never a second set of rules.
func (s *Server) adminEndpoints(d AdminDeps) []AdminEndpoint {
	version := s.env.Version
	if version == "" {
		version = "unknown"
	}
	eps := []AdminEndpoint{{
		Path: "/healthz", Code: "200", Tone: "open",
		Answer: "ok · version " + version,
		Proves: "The process answers HTTP and reports its version — liveness only; it says nothing about the store.",
	}}
	ready := AdminEndpoint{
		Path: "/readyz", Code: "n/a", Tone: "neutral",
		Answer: "readiness is not wired on this build",
		Proves: "Whether this instance should receive traffic: boot mode, warm-up, drain, and the auth/config warnings.",
	}
	if d.Health != nil {
		h := d.Health()
		label := h.Label
		if label == "" {
			label = h.State
		}
		ready.Answer = label
		if h.Summary != "" {
			ready.Answer = label + " — " + h.Summary
		}
		switch h.State {
		case "ready":
			ready.Code, ready.Tone = "200", "open"
		case "setup_required":
			// The restricted boot is READY for the surface it serves: 200
			// {"status":"restricted","surface":"admin"} (06_server_http.md §10.2).
			// Reporting 503 here is what made a health-gated ingress route nothing
			// and hid the recovery UI (2026-11-22 outage).
			ready.Code, ready.Tone = "200", "open"
		default:
			ready.Code, ready.Tone = "503", "draft"
		}
	}
	eps = append(eps, ready)
	eps = append(eps, AdminEndpoint{
		Path: "/metrics", Code: "text", Tone: "neutral",
		Answer: "Prometheus text exposition — not evaluated here",
		Proves: "The counters and gauges behind this page's delivery, lag and unit facts; the endpoint proves nothing until something scrapes it.",
	})
	return eps
}

// ---------------------------------------------------------------------------
// small display helpers
// ---------------------------------------------------------------------------

// authSummary renders the authentication model as one line: OIDC plus the
// admin group that grants administration.
func authSummary(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	groups := strings.Join(cfg.Server.Auth.AdminGroups, ", ")
	if groups == "" {
		groups = "—"
	}
	return "OIDC · admin group: " + groups
}

// storeTarget renders the store's human coordinate: the bucket (s3) or the root
// (filesystem). It is display-only — never a connection target.
func storeTarget(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	switch cfg.Store.Backend {
	case "s3":
		out := cfg.Store.Bucket
		if cfg.Store.Prefix != "" {
			out += "/" + strings.Trim(cfg.Store.Prefix, "/")
		}
		return out
	case "memory":
		return "in-memory (non-durable)"
	default:
		if cfg.Store.Root == "" {
			return cfg.Store.Backend
		}
		return cfg.Store.Root
	}
}

// rolesLabel renders server.roles the way the Overview states it: an empty
// list means every role runs on this host.
func rolesLabel(roles []string) string {
	if len(roles) == 0 {
		return "serve, maintain, events (all)"
	}
	return strings.Join(roles, ", ")
}

// roleEnabled reports whether a role runs on this host (empty = all roles).
func roleEnabled(roles []string, want string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// globsLabel renders a placement glob list with an honest empty spelling.
func globsLabel(globs []string, empty string) string {
	if len(globs) == 0 {
		return empty
	}
	return strings.Join(globs, ", ")
}

// cadenceText renders a configured duration in the spec spelling ("1m", "7d");
// a negative value is the config's "disabled" spelling.
func cadenceText(d config.Duration, empty string) string {
	dur := time.Duration(d)
	if dur < 0 {
		return "disabled"
	}
	if dur == 0 {
		return empty
	}
	return fmtSpecDuration(dur.String())
}

// watermarkText renders cache.disk_high_watermark as a percentage.
func watermarkText(frac float64) string {
	if frac <= 0 {
		return "not set (the eviction pass uses 90%)"
	}
	return strconv.FormatFloat(frac*100, 'f', -1, 64) + "%"
}

// cachePolicy spells the eviction policy behind cache.mode.
func cachePolicy(mode string) string {
	switch mode {
	case "budget":
		return "budget — evict least-recently-used repositories above cache.max_bytes"
	case "disk":
		return "disk — evict above the filesystem high watermark"
	case "auto":
		return "auto — the filesystem watermark, then the byte budget"
	case "":
		return "budget (default)"
	default:
		return mode
	}
}

// clampPercent keeps a rounded percentage inside the progressbar's 0–100.
func clampPercent(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}
