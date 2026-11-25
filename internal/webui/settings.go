// Settings group (docs/go/12_web_ui.md §2, D-WEB-14): three pages —
//
//	GET  /{owner}/{name}/settings    repo settings, path-segment tabs
//	GET  /{owner}/settings           organisation settings
//	GET  /keys                       SSH public keys
//
// The instance configuration editor moved to the Administration area
// (admin.go: /admin/config). This file keeps repo/org/keys settings only.
//
// # WIRING (composition owns cmd/spindle/webui.go)
//
// The feature services are NOT on api.Env, and internal/server's helpers are
// unexported, so this file takes a narrow function seam:
//
//	webui.BindSettings(webui.SettingsDeps{
//	    // --- repo settings: adapt the same services the SPA's SDK calls.
//	    RepoRole:       identity.Service.Resolve (Role) -> "read"|"triage"|…|"admin";
//	                    host admins (p.Admin) may short-circuit to "admin".
//	    RepoGeneral:    api.RepoView.Settings + config.ParseRepoSettings
//	                    (description, Resolve() features, AllowSelfApprovalOf).
//	    RepoScheduled:  the settings-describe recipe (strategies, this_host,
//	                    upstream follow, effective fields).
//	    RepoMirror:     mirror.Service.Get (Doc -> view) + view.NextSync/FormatLastResult.
//	    RepoPushMirror: pushmirror.Service.Get + sdk View (has_secret/secret_hint).
//	    RepoPolicy:     policy store read (the same doc policy.put writes).
//	    RepoConfig:     api.RepoView.Settings + SettingsHistory.
//	    RepoAccess:     identity.Service.GetAccess (version, visibility, bindings)
//	                    + Resolve (your role) + ListTeams for the team picker.
//	    RepoAdmin:      ciTokens.List + webhooks.List.
//	    RepoWrite:      one adapter switching on w.Action; it MUST re-check
//	                    authority (this page checked it, but the seam is public).
//	    OrgView/OrgWrite: identity.Service GetOrg/GetProfile(org).CanEdit +
//	                    members/teams/invites; activity via the org activity view.
//	}
//
// A nil field renders that card as "not wired" instead of failing the page, so
// the group lands ahead of its composition and each binding can arrive alone.
//
// RepoAccess and RepoAdmin take the caller's principal (the JSON routes gate
// per principal; a blank role or collaborator list would be a lie), and
// RepoWrite re-checks admin authority itself because the seam is public.
//
// # REPO-SIDE SEAM (wired: SettingsServices)
//
// settings_seams.go adapts the SAME services the JSON handlers call and is one
// composition call:
//
//	d := webui.SettingsDeps{ /* org fields */ }
//	d.BindSettingsServices(webui.SettingsServices{
//	    API: apiEnv, Ident: c.ident, Checks: c.checksSvc, Notify: c.notifySvc,
//	    Mirror: c.mirrorHandler, Push: c.pushMirrorHandler, Now: time.Now,
//	})
//	webui.BindSettings(d)
//
// It fills Repo{General,Scheduled,Mirror,PushMirror,Policy,Config,Access,
// Admin,Write}. A nil service leaves its field nil and the affected panel keeps
// its degraded card, so a build without the collaboration layer still renders
// the pages that need no store.
//
// # ROUTE SHAPE
//
// Repo tabs are PATH SEGMENTS (/settings/general, …/settings/danger) per
// specs/settings.md §2 — the fragment is not a routing key. "pushmirror" is
// kept as an alias for "mirror" (#627) and an unknown tab 303s to general, so
// a stale #hash link can never blank the page.
//
// Org tabs use ?tab=members instead of a path segment: net/http's ServeMux
// refuses to register BOTH "GET /{owner}/settings/{tab}" and
// "GET /{owner}/{name}/settings" — they overlap at /x/settings/settings and
// neither is more specific (verified; the panic is at registration time).
// Chosen trade: the repo tabs (the larger surface) keep path segments; org
// tabs are query-selected. A foundation router change can lift this later.
package webui

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/httpsec"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
)

// ---------------------------------------------------------------------------
// deps
// ---------------------------------------------------------------------------

// SettingsDeps is the settings group's service seam (see the WIRING header).
type SettingsDeps struct {
	RepoRole       func(ctx context.Context, owner, repo string, p auth.Principal) (string, error)
	RepoGeneral    func(ctx context.Context, owner, repo string) (*SettingsGeneral, error)
	RepoScheduled  func(ctx context.Context, owner, repo string) (*SettingsScheduled, error)
	RepoMirror     func(ctx context.Context, owner, repo string) (*SettingsMirror, error)
	RepoPushMirror func(ctx context.Context, owner, repo string) (*SettingsPushMirror, error)
	RepoPolicy     func(ctx context.Context, owner, repo string) (*SettingsPolicy, error)
	RepoProtection func(ctx context.Context, owner, repo string) (*SettingsProtection, error)
	RepoConfig     func(ctx context.Context, owner, repo string) (*SettingsConfig, error)
	RepoAccess     func(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAccess, error)
	RepoAdmin      func(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAdmin, error)
	// RepoEnvironments is the Settings → Environments read (deployment
	// environments and their scoped secret names). It is the Threads service's
	// environment surface, asserted from SettingsServices.Threads so
	// composition keeps one binding; a service without it leaves this nil and
	// the tab renders its "not wired" card.
	RepoEnvironments func(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsEnvironmentPanel, error)
	RepoWAL          func(ctx context.Context, owner, repo string) (*SettingsWAL, error)
	RepoWrite        func(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError)
	// Reports is the repo Settings → Reports queue (PLAN-V2 §M.3): the read
	// is the moderation list verb, the write its resolve verb — the page and
	// the JSON API share one implementation and one floor.
	RepoReports       func(ctx context.Context, owner, repo string, p auth.Principal, state string) (*SettingsRepoReports, error)
	RepoReportResolve func(ctx context.Context, owner, repo string, id int, p auth.Principal, action, note string) error

	OrgView  func(ctx context.Context, org string, p auth.Principal) (*SettingsOrgPage, error)
	OrgWrite func(ctx context.Context, org string, p auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError)
	// OrgWebhookWrite handles the org Webhooks tab (WP6.2). It is separate
	// from OrgWrite because the hook surface is owned by notify while the
	// rest of the org page is owned by identity; a nil OrgWebhookWrite
	// renders the Webhooks tab degraded, exactly like a nil service elsewhere.
	OrgWebhookWrite func(ctx context.Context, org string, p auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError)
}

// SettingsWriteResult is a successful mutation's one-shot output: the notice
// to show, a secret that must NEVER enter a URL (CI token), a shareable link
// (org invite), an optional redirect override, and an optional panel payload
// (a policy dry-run re-renders with its counts instead of reloading).
type SettingsWriteResult struct {
	Notice   string
	Secret   string
	Link     string
	Redirect string
	Policy   *SettingsPolicy
	// Task is a background task the write started (repository transfer,
	// PLAN-V2 §3 T.1): the page renders its live card instead of redirecting,
	// because the source page only exists until the move commits.
	Task *SettingsTask
	// Invite is the row an org invite-create just minted. It carries the
	// one-shot accept link into the pending table of the same response: the
	// org invitation read redacts every token, so only this response can show
	// the link (repo invitations are different — their admin read is allowed
	// to return tokens).
	Invite *SettingsOrgInvite
}

// settingsBound is the process-wide binding. server.go documents one page
// provider per process, so a package var is the seam; BindSettings runs once
// at composition time, before the first request.
var settingsBound SettingsDeps

// BindSettings installs the group's service seam (see the WIRING header).
func BindSettings(d SettingsDeps) { settingsBound = d }

// SettingsBindings returns the installed seam. Composition's wiring guard
// (cmd/spindle) uses it to prove no rendered settings surface is left nil;
// callers must treat the result as read-only.
func SettingsBindings() SettingsDeps { return settingsBound }

func settingsDeps() SettingsDeps { return settingsBound }

// SettingsWriteError is a rejected write. Status defaults to 422 (validation);
// 403/404/409 pass through to the page, which renders the message inline.
type SettingsWriteError struct {
	Status  int
	Field   string
	Message string
}

func (e *SettingsWriteError) status() int {
	if e == nil || e.Status == 0 {
		return http.StatusUnprocessableEntity
	}
	return e.Status
}

// ---------------------------------------------------------------------------
// repo settings data
// ---------------------------------------------------------------------------

// SettingsNavItem is one sidebar entry.
type SettingsNavItem struct {
	ID     string
	Label  string
	Href   string
	Active bool
	Danger bool
}

// SettingsRepoPage is the repo-settings page model.
type SettingsRepoPage struct {
	Owner  string
	Name   string
	Tab    string
	Role   string
	Admin  bool
	Saved  bool
	Notice string
	Error  string
	Nav    []SettingsNavItem
	Danger []SettingsNavItem
	// Chrome is the repository masthead + tab strip the layout renders above
	// the settings page (12_web_ui.md §2.2.1). Built once in settingsRepoPage
	// after the read gate; nil when the repo read fails (no fabricated chrome).
	Chrome     *RepoChrome
	General    *SettingsGeneral
	Scheduled  *SettingsScheduled
	Mirror     *SettingsMirror
	Push       *SettingsPushMirror
	Policy     *SettingsPolicy
	Protection *SettingsProtection
	Config     *SettingsConfig
	Access     *SettingsAccess
	AdminData  *SettingsAdmin
	// Environments is the Environments tab payload (deployment environments
	// and their scoped secrets, docs/PLAN-THREADS-ENVIRONMENTS.md §7).
	Environments *SettingsEnvironmentPanel
	// WebhookPanel is the shared Webhooks tab payload (the same partial the
	// org tab renders).
	WebhookPanel *SettingsWebhookPanel
	WAL          *SettingsWAL
	DangerData   *SettingsDanger
	// Reports is the Reports tab payload (PLAN-V2 §M.3).
	Reports *SettingsRepoReports
}

// SettingsFeatureRow is one feature toggle.
type SettingsFeatureRow struct {
	Key     string
	Label   string
	Hint    string
	Enabled bool
}

// SettingsGeneral is the General tab.
type SettingsGeneral struct {
	Description string
	Website     string
	MaxLength   int
	// MaxWebsite bounds the website input's maxlength (config.MaxRepoWebsiteBytes).
	MaxWebsite   int
	Features     []SettingsFeatureRow
	SelfApproval bool
	Note         string
	Error        string
}

// SettingsKV is one key/value row.
type SettingsKV struct {
	Key, Value string
}

// SettingsScheduled is the Scheduled tasks tab: the read-only facts the
// maintainer computes plus the structured editors for the four per-repo
// tuning sections (§ WP9 G14). Every edited value round-trips through
// config.ParseRepoSettings + ValidateAgainst + api.PublishSettings (the
// settings PUT's parse/publish path plus the /settings/validate rule); the
// raw TOML editor stays the power-user surface.
type SettingsScheduled struct {
	HostFacts    []SettingsKV
	UpstreamGit  string
	FollowLine   string
	LastRound    string
	Effective    []SettingsKV
	EffectiveSrc []string
	// Sections are the editable tuning sections in page order:
	// bundles, maintenance, compaction, upstream.
	Sections []SettingsSection
	Note     string
	Error    string
}

// SettingsSection is one structured settings-section editor (a fieldset of
// scalar rows plus, for bundles, the repeating strategy rows). Override is
// the section's stored override state: unchecked means the section is absent
// from the document (host config wins) and its posted fields are ignored, so
// an untouched page re-publishes the same document.
type SettingsSection struct {
	ID        string // "bundles" | "maintenance" | "compaction" | "upstream"
	Label     string
	Hint      string
	Override  bool
	Inherited bool // section absent from the stored document (values shown are host)
	Fields    []SettingsField
	Rows      []SettingsRow // bundles.strategy: stored rows + one blank add row
	RowPrefix string
	Error     string
	// FieldErrors marks a rejected cell by its form name.
	FieldErrors map[string]string
}

// SettingsField is one scalar row of a section editor.
type SettingsField struct {
	Name  string // form name, e.g. "bundles.min_bytes"
	Label string
	Type  string // text|int|bool|size|duration|list|select
	Value string
	Enum  []string
	Hint  string
}

// Custom reports whether the current value is not one of the enum options
// (the template renders it as an explicit "current" choice rather than
// silently selecting the first option).
func (f SettingsField) Custom() bool { return customEnumValue(f.Value, f.Enum) }

// SettingsRowCell is one cell of a bundle-strategy row.
type SettingsRowCell struct {
	Name  string // form name, e.g. "bundles.strategy#0.name"
	Label string
	Type  string // text|int|bool|list|select
	Value string
	Enum  []string
}

// Custom reports whether the cell's current value is outside its enum.
func (c SettingsRowCell) Custom() bool { return customEnumValue(c.Value, c.Enum) }

func customEnumValue(value string, enum []string) bool {
	if value == "" {
		return false
	}
	for _, v := range enum {
		if v == value {
			return false
		}
	}
	return true
}

// SettingsRow is one bundle strategy exactly as rendered. Base is the stored
// index the row was rendered from (-1 for the blank add row); the forms post
// it as __base so an edit can never silently become a different row.
type SettingsRow struct {
	Index     int
	Base      int
	Caption   string
	Next      string // next slot from the describe read (display-only)
	Cells     []SettingsRowCell
	BaseKey   string
	RemoveKey string
}

// SettingsWAL is the Settings → WAL panel (WP9 G16): the page facts the WAL
// view reports plus the same ops inventory the WAL page runs. The run forms
// POST the existing …/api/ops/{op} stream endpoint (AuthWrite) — this page
// never invents a second write path.
type SettingsWAL struct {
	Owner      string
	Name       string
	Health     string
	HealthTone string
	ServedBy   string
	CloneURL   string
	Facts      []SettingsKV
	Packs      string
	Panel      walOpsPanel
	Note       string
	Error      string
}

// SettingsOption is a select option.
type SettingsOption struct {
	ID, Label string
}

// SettingsMirror is the pull-mirror container.
type SettingsMirror struct {
	Configured bool
	Upstream   string
	Status     string
	NextSync   string
	LastSynced string
	LastResult string
	Failures   int
	Schedule   string
	Presets    []SettingsOption
	Note       string
	Error      string
}

// SettingsPushMirror is the push-mirror container.
type SettingsPushMirror struct {
	Configured  bool
	Upstream    string
	AuthKind    string
	AuthLabel   string
	Username    string
	HasSecret   bool
	SecretHint  string
	Fingerprint string
	HostKey     string
	NextSync    string
	LastSynced  string
	LastResult  string
	Schedule    string
	Presets     []SettingsOption
	AuthKinds   []SettingsOption
	PublicKey   string
	KnownHosts  string
	Note        string
	Error       string
}

// SettingsPolicy is the Push policy tab.
type SettingsPolicy struct {
	Text        string
	Exists      bool // a stored policy document exists (Remove is meaningful)
	DryRunN     int
	DryRunDone  bool
	DryRunAllow int
	DryRunDeny  int
	DryRunRefs  []SettingsKV
	Note        string
	Error       string
}

// SettingsProtection is the Branch protection tab (WP2.3, reworked as
// rulesets by PLAN-V2 §2.4): the list of named protection rulesets, the
// selected ruleset's form, and the computed effective-rules view. Every
// field maps onto the SAME policy document the API validates — a ruleset is
// the named pair `protection:<slug>-push` + `protection:<slug>-reviews`,
// and every other rule is preserved byte-for-byte.
type SettingsProtection struct {
	Owner    string
	Repo     string
	Rulesets []SettingsRuleset
	// Editing is the ruleset the form renders; nil means the list view.
	Editing *SettingsRuleset
	// New marks the empty form for a not-yet-stored ruleset.
	New       bool
	Roles     []SettingsOption
	Methods   []SettingsOption
	Rules     []SettingsPolicyRule
	Effective SettingsEffective
	RefArg    string
	Raw       string
	// HasManaged reports at least one protection ruleset exists.
	HasManaged bool
	Error      string
	Note       string

	doc *policy.Document
}

// SettingsRuleset is one named ruleset: its identity, the effects the form
// owns, and whether the form can express it at all. A ruleset the form
// cannot express is Editable=false and rendered read-only — its rules are
// never rewritten (a save of another ruleset leaves its bytes untouched).
type SettingsRuleset struct {
	Slug     string
	PushRule string
	RevRule  string
	// Enabled is present-but-inert today: a ruleset is enforced by being in
	// the document. Unchecking it on the form removes its rules (the old
	// tab's semantics), so the list only ever shows stored rulesets.
	Enabled         bool
	RefPatterns     []string
	Refs            string
	Approvals       int
	DismissStale    bool
	Checks          []string
	ChecksText      string
	PushRoles       []string
	BlockForce      bool
	BlockDelete     bool
	RequireUpToDate bool
	MergeMethods    []string
	MethodsText     string
	Summary         []string
	Editable        bool
	ReadOnly        string
	CanMoveUp       bool
	CanMoveDown     bool
	// Order is the ruleset's position (the policy document's rule order).
	Order int
}

// SettingsEffectiveOp is one op row of the effective-rules view.
type SettingsEffectiveOp struct {
	Op      string
	Allowed bool
	Rule    string
	Verdict string
}

// SettingsEffective is what the stored document actually does to one ref for
// one principal: the push verdict per op (computed through the same
// policy.EvaluateProtect the server publishes with), and the merge demands
// resolved by policy.MergeGateFor plus the reviews/checks unions.
type SettingsEffective struct {
	Ref             string
	Principal       string
	Ops             []SettingsEffectiveOp
	Rules           []SettingsPolicyRule
	Checks          []string
	Approvals       int
	DismissStale    bool
	ReviewsRule     string
	ReviewsBypassed bool
	UpToDate        bool
	UpToDateRules   []string
	Methods         []string
	MethodsNarrowed bool
	HasRules        bool
}

// SettingsPolicyRule is one row of the read-only effective policy view.
type SettingsPolicyRule struct {
	Name     string
	Refs     string
	Summary  string
	Managed  bool
	Scoped   bool
	ReadOnly bool
}

// PushRoleChecked reports whether a role is part of the bypass set (the
// template's checkbox arm; no template FuncMap entry needed).
func (p *SettingsProtection) PushRoleChecked(role string) bool {
	if p == nil || p.Editing == nil {
		return false
	}
	for _, r := range p.Editing.PushRoles {
		if r == role {
			return true
		}
	}
	return false
}

// MergeMethodChecked reports whether a ruleset permits a merge method (an
// empty set means "every method", which the form renders as all checked).
func (p *SettingsRuleset) MergeMethodChecked(method string) bool {
	if p == nil {
		return false
	}
	if len(p.MergeMethods) == 0 {
		return true
	}
	return containsStr(p.MergeMethods, method)
}

// SettingsDiffLine is one line of the config-history diff.
type SettingsDiffLine struct {
	Sign string
	Text string
}

// SettingsRevision is one published config revision. TOML is the revision's
// document (seeded into the editor by "revert to this"; never rendered raw).
type SettingsRevision struct {
	Rev     uint64
	Author  string
	At      string
	Message string
	TOML    string
	Diff    []SettingsDiffLine
}

// SettingsConfig is the Effective config & history tab.
type SettingsConfig struct {
	Text      string
	Message   string
	Revisions []SettingsRevision
	Note      string
	Error     string
}

// SettingsCollaborator is one effective collaborator.
type SettingsCollaborator struct {
	Principal, Role, Source string
}

// SettingsBinding is one editable role binding.
type SettingsBinding struct {
	Subject, Role string
}

// SettingsInvite is one pending collaborator invitation (G9). AcceptURL is
// the human accept page with the invite's capability token; it is populated
// for repo admins only — the seam never reads the invite list for a
// non-admin, so a triage reader cannot learn a capability token.
type SettingsInvite struct {
	ID, Subject, Role, InvitedBy, ExpiresAt, AcceptURL string
	Expired                                            bool
}

// AcceptLink is the copy control's view of the row (shared with the org tab's
// pending table through the settings-invite-accept partial).
func (i SettingsInvite) AcceptLink() SettingsInviteLink {
	return SettingsInviteLink{ID: i.ID, URL: i.AcceptURL}
}

// SettingsInviteLink is one pending invitation's accept-link cell: the row id
// names the copy target, URL is empty for a row whose token the read redacted
// (the org tab's pending rows), which renders a masked marker instead.
type SettingsInviteLink struct {
	ID  string
	URL string
}

// SettingsAccess is the Access tab.
type SettingsAccess struct {
	Role          string
	Collaborators []SettingsCollaborator
	Visibility    string
	IsOrg         bool
	Bindings      []SettingsBinding
	Roles         []string
	Teams         []SettingsOption
	Invites       []SettingsInvite
	// InvitesWired distinguishes "the identity service is not wired on this
	InvitesWired bool
	// InviteLink is the one-shot share card after a create (never seeded
	InviteLink string
	// NewSubject / NewRole re-seed the invite form after a rejected write
	NewSubject string
	NewRole    string
	Version    uint64
	// CanEdit is false for a triage viewer: the tab is readable at triage but
	// every access write is admin-only, so the form must not render.
	CanEdit bool
	Note    string
	Error   string
}

// SettingsToken is one CI token row.
type SettingsToken struct {
	ID, Name, Scopes, CreatedBy, CreatedAt, Status, RevokedAt string
}

// SettingsWebhook is one webhook row.
type SettingsWebhook struct {
	ID, URL, Events, LastPing string
	Active, SecretSet         bool
	Recent                    []SettingsDelivery
	// Edit* re-renders a rejected per-hook edit with the operator's values
	// (the row opens and carries the inline error; nothing was written).
	EditOpen   bool
	EditURL    string
	EditEvents string
	EditActive bool
	EditError  string
	// RotateError marks a rejected secret rotation on the row.
	RotateError string
}

// SettingsDelivery is one recent delivery row (the bounded per-hook ring).
type SettingsDelivery struct {
	Seq        string
	SeqNum     int
	Event      string
	Status     int
	At         string
	Error      string
	DurationMs int64
	// Redeliverable is true only when the ring retained the exact POST body
	// (the redeliver row is honest: no control where the payload is gone).
	Redeliverable bool
}

// SettingsWebhookPanel is the shared Webhooks tab payload: the repo tab and
// the org tab render the same canonical table/form with different action
// plumbing (repo: path-scoped URLs + webhook_action; org: one URL + hidden
// org_action + id).
type SettingsWebhookPanel struct {
	// CanEdit gates the per-hook edit disclosure: the repo surface wires an
	// update action, the org surface does not (its API has no PATCH), so the
	// shared panel renders the form only where the action exists.
	CanEdit      bool
	Wired        bool
	CreateAction string
	CreateFields []SettingsKV
	NewURL       string
	NewEvents    string
	Error        string
	// EventHints is the filter vocabulary the form offers as suggestions
	// (a <datalist> plus the hint line). Repo hooks get the collaboration
	// actions and the push/ref family; org hooks get the org vocabulary and
	// NEVER push (an org has no repository to push to), which is why the two
	// panels differ here and nowhere else.
	EventHints []string
	// OffersPush says whether EventHints contains the push family (repo only),
	// so the hint line never promises a topic the surface will reject.
	OffersPush bool
	Rows       []SettingsWebhookRow
}

// SettingsWebhookRow is one hook plus its form plumbing.
type SettingsWebhookRow struct {
	SettingsWebhook
	ActionURL      string
	ActionField    string
	Fields         []SettingsKV
	ToggleValue    string
	UpdateValue    string
	RotateValue    string
	PingValue      string
	DeleteValue    string
	RedeliverValue string
}

// SettingsAdmin is the CI tokens + Webhooks tabs.
type SettingsAdmin struct {
	Tokens    []SettingsToken
	Webhooks  []SettingsWebhook
	NewToken  string // shown once, never in a URL
	NewSecret string
	Note      string
	Error     string
	// TokensWired / WebhooksWired distinguish "the service is not wired on
	// this build" from a genuinely empty list, so the panels never claim
	// "no tokens" for an unwired surface.
	TokensWired   bool
	WebhooksWired bool
	// Secrets (15 §6): the admin-only Threads surface. (Runners are
	// INSTANCE-scoped and live in the Administration area, /admin/runners —
	// docs/PLAN-INSTANCE-RUNNERS.md.)
	Secrets        []SettingsSecret
	SecretsWired   bool
	SecretsEnabled bool
	// Draft values re-rendered after a rejected create (never secrets).
	NewName   string
	NewURL    string
	NewEvents string
}

// SettingsSecret is one write-only secret row (name + timestamps only).
type SettingsSecret struct {
	Name, CreatedAt, UpdatedAt string
}

// SettingsEnvironmentPanel is the Environments tab payload: every stored
// deployment environment, its required reviewers, its scoped secret names
// (never values) and the form drafts.
type SettingsEnvironmentPanel struct {
	// Wired is false when the composition did not bind a Threads service (the
	// panel then renders the honest "not wired" card instead of an empty list).
	Wired bool
	// Enabled mirrors the secret store's availability (server.secrets_key /
	// session secret); an environment can still be created without it.
	Enabled bool
	Rows    []SettingsEnvironment
	// Drafts survive a rejected write (never a secret value).
	NewName      string
	NewReviewers string
	NewSecret    string
	Error        string
}

// SettingsEnvironment is one environment row plus its secret names.
type SettingsEnvironment struct {
	Name          string
	Reviewers     []string
	ReviewersText string
	Secrets       []SettingsSecret
	CreatedAt     string
	UpdatedAt     string
}

// SettingsDanger is the Danger Zone tab.
type SettingsDanger struct {
	FullName string // typed confirmation target
	IsMirror bool
	Error    string
	// Task is the running transfer this submit just started (or joined): the
	// card streams its narration and opens the destination when the task
	// verifies the move. nil = no transfer in flight from this render.
	Task *SettingsTask
}

// SettingsTask is a server-started background task rendered as a live card
// (the shared ops/SSE pattern; islands/tasks.js consumes it). ID/Stream are the
// task's own id and attach URL; Next is where a successful task goes when the
// record's repo is not the destination (a repository transfer's record is keyed
// by the SOURCE).
type SettingsTask struct {
	ID     string
	Kind   string
	Stream string
	Next   string
	Title  string
}

// SettingsRepoWrite is one settings mutation. Action selects the operation; a
// nil dep renders the panels as "not wired".
type SettingsRepoWrite struct {
	Action string
	Values map[string]string
	// Multi carries the repeatable form keys (checkbox groups such as the
	// Branch protection push roles); Values keeps the last value.
	Multi map[string][]string
	File  []byte
}

// Get returns a form value ("" when absent).
func (w SettingsRepoWrite) Get(key string) string { return w.Values[key] }

// Write actions (the seam's vocabulary).
const (
	SettingsActionAccess                  = "access"
	SettingsActionConfig                  = "config"
	SettingsActionConfigClear             = "config-clear"
	SettingsActionDelete                  = "delete"
	SettingsActionGeneral                 = "general"
	SettingsActionInviteCancel            = "invite-cancel"
	SettingsActionInviteCreate            = "invite-create"
	SettingsActionMirrorPut               = "mirror-put"
	SettingsActionMirrorRemove            = "mirror-remove"
	SettingsActionMirrorSchedule          = "mirror-schedule"
	SettingsActionMirrorSync              = "mirror-sync"
	SettingsActionPolicy                  = "policy"
	SettingsActionPolicyDryRun            = "policy-dryrun"
	SettingsActionPolicyRemove            = "policy-remove"
	SettingsActionProtection              = "protection"
	SettingsActionPushKeygen              = "pushmirror-keygen"
	SettingsActionPushPut                 = "pushmirror-put"
	SettingsActionPushRemove              = "pushmirror-remove"
	SettingsActionPushSchedule            = "pushmirror-schedule"
	SettingsActionPushSync                = "pushmirror-sync"
	SettingsActionScheduled               = "scheduled"
	SettingsActionSecretDelete            = "secret-delete"
	SettingsActionSecretPut               = "secret-put"
	SettingsActionEnvironmentPut          = "environment-put"
	SettingsActionEnvironmentDelete       = "environment-delete"
	SettingsActionEnvironmentSecretPut    = "environment-secret-put"
	SettingsActionEnvironmentSecretDelete = "environment-secret-delete"
	SettingsActionTokenCreate             = "token-create"
	SettingsActionTokenRevoke             = "token-revoke"
	SettingsActionTransfer                = "transfer"
	SettingsActionWebhookCreate           = "webhook-create"
	SettingsActionWebhookDelete           = "webhook-delete"
	SettingsActionWebhookPing             = "webhook-ping"
	SettingsActionWebhookRedeliver        = "webhook-redeliver"
	SettingsActionWebhookRotate           = "webhook-rotate"
	SettingsActionWebhookToggle           = "webhook-toggle"
	SettingsActionWebhookUpdate           = "webhook-update"
)

// ---------------------------------------------------------------------------
// org + keys data
// ---------------------------------------------------------------------------

// SettingsOrgPage is the organisation settings page model.
type SettingsOrgPage struct {
	Org       string
	CanManage bool
	Tab       string
	Nav       []SettingsNavItem
	Notice    string
	Error     string

	Profile    *SettingsOrgProfile
	Members    []SettingsOrgMember
	Teams      []SettingsOrgTeam
	Invites    []SettingsOrgInvite
	Activity   []SettingsOrgEvent
	More       bool
	InviteLink string

	// Webhooks is the org Webhooks tab (WP6.2); WebhooksWired distinguishes
	// "not wired on this build" from a genuinely empty list.
	Webhooks      []SettingsWebhook
	WebhooksWired bool
	NewURL        string
	NewEvents     string
	WebhookPanel  *SettingsWebhookPanel
}

// SettingsOrgProfile is the Profile tab.
type SettingsOrgProfile struct {
	DisplayName string
	Description string
	Location    string
	Timezone    string
	BioMarkdown string
	AvatarURL   string
	AvatarType  string
	CreatedAt   string
	UpdatedAt   string
	ReadOnly    bool
}

// SettingsOrgMember is one roster row.
type SettingsOrgMember struct {
	Principal, Role string
}

// SettingsOrgTeam is one team row.
type SettingsOrgTeam struct {
	Slug, Name, Description string
	Members                 []string
}

// SettingsOrgInvite is one invitation row. AcceptURL is set ONLY on the row a
// create response just minted: org invitation reads redact the token by
// contract, so a pending row from a read carries no link (the template renders
// a masked marker where the token was).
type SettingsOrgInvite struct {
	ID, Subject, Role, InvitedBy, ExpiresAt string
	Expired                                 bool
	AcceptURL                               string
}

// AcceptLink is the copy control's view of the row (shared partial).
func (i SettingsOrgInvite) AcceptLink() SettingsInviteLink {
	return SettingsInviteLink{ID: i.ID, URL: i.AcceptURL}
}

// SettingsOrgEvent is one activity row.
type SettingsOrgEvent struct {
	Seq    uint64
	At     string
	Action string
	Actor  string
	Title  string
}

// SettingsOrgWrite is one org mutation.
type SettingsOrgWrite struct {
	Action string
	Values map[string]string
	File   []byte
}

// Get returns a form value ("" when absent).
func (w SettingsOrgWrite) Get(key string) string { return w.Values[key] }

// Org write actions.
const (
	SettingsOrgProfileSave  = "profile"
	SettingsOrgAvatar       = "avatar"
	SettingsOrgAvatarRm     = "avatar-remove"
	SettingsOrgMemberSet    = "member"
	SettingsOrgMemberRm     = "member-remove"
	SettingsOrgTeamSave     = "team"
	SettingsOrgTeamUpdate   = "team-update"
	SettingsOrgTeamRm       = "team-remove"
	SettingsOrgTeamAdd      = "team-add"
	SettingsOrgTeamDel      = "team-del"
	SettingsOrgTeamMemberRm = "team-member-remove"
	SettingsOrgInviteMake   = "invite"
	SettingsOrgInviteRm     = "invite-remove"
	SettingsOrgDelete       = "delete"

	// Org Webhooks tab actions (WP6.2) — one hidden org_action value per
	// operation, the same vocabulary the repo tab's webhook_action uses.
	SettingsOrgWebhookCreate    = "webhook-create"
	SettingsOrgWebhookToggle    = "webhook-toggle"
	SettingsOrgWebhookRotate    = "webhook-rotate"
	SettingsOrgWebhookPing      = "webhook-ping"
	SettingsOrgWebhookDelete    = "webhook-delete"
	SettingsOrgWebhookRedeliver = "webhook-redeliver"
)

// settingsIsOrgWebhookAction reports whether an org_action belongs to the
// composition-owned Webhooks tab (the OrgWebhookWrite seam).
func settingsIsOrgWebhookAction(action string) bool {
	switch action {
	case SettingsOrgWebhookCreate, SettingsOrgWebhookToggle, SettingsOrgWebhookRotate,
		SettingsOrgWebhookPing, SettingsOrgWebhookDelete, SettingsOrgWebhookRedeliver:
		return true
	}
	return false
}

// SettingsKeysPage is the /keys page model.
type SettingsKeysPage struct {
	Keys     []SettingsKeyRow
	KeyLine  string
	Title    string
	Notice   string
	Error    string
	NotWired bool
}

// SettingsKeyRow is one key as the table renders it (the middle of the blob is
// elided — shortKey is display-only, the row identity is the registry ID).
type SettingsKeyRow struct {
	ID    string
	Title string
	Short string
	Added string
}

// ---------------------------------------------------------------------------
// tabs
// ---------------------------------------------------------------------------

type settingsTab struct {
	ID      string
	Label   string
	Danger  bool
	MinRead string
}

// settingsRepoTabs is the sidebar order (lib/settingsNav.js). MinRead is the
// page gate and must never be weaker than the POST/API gate: General,
// Scheduled tasks and Mirror are editing surfaces whose writes are admin-only,
// so they render for admins only (a guest gets the same 403 as the other
// mutation tabs — never a live form, and never a configured upstream URL).
var settingsRepoTabs = []settingsTab{
	{ID: "general", Label: "General", MinRead: "admin"},
	{ID: "scheduled", Label: "Scheduled tasks", MinRead: "admin"},
	{ID: "mirror", Label: "Mirror", MinRead: "admin"},
	{ID: "policy", Label: "Push policy", MinRead: "admin"},
	{ID: "protection", Label: "Branch protection", MinRead: "admin"},
	{ID: "config", Label: "Effective config & history", MinRead: "admin"},
	{ID: "access", Label: "Access", MinRead: "triage"},
	{ID: "tokens", Label: "CI tokens", MinRead: "admin"},
	{ID: "secrets", Label: "Secrets", MinRead: "admin"},
	{ID: "environments", Label: "Environments", MinRead: "maintain"},
	{ID: "webhooks", Label: "Webhooks", MinRead: "admin"},
	{ID: "reports", Label: "Reports", MinRead: "admin"},
	{ID: "wal", Label: "WAL", MinRead: "read"},
	{ID: "danger", Label: "Danger Zone", Danger: true, MinRead: "admin"},
}

// settingsOrgTabs is the org tab order (Org.jsx: Profile/Members/Teams/
// Webhooks/Invitations/Activity/Danger). Invitations is owner-only: the API's
// collection route (GET/POST /api/v1/orgs/{org}/invitations) is CheckOrgOwner,
// and the create response carries a capability token, so the page gate must
// mirror it (a member never sees the tab or a row).
var settingsOrgTabs = []settingsTab{
	{ID: "profile", Label: "Profile"},
	{ID: "members", Label: "Members"},
	{ID: "teams", Label: "Teams"},
	{ID: "invitations", Label: "Invitations", MinRead: "admin"},
	{ID: "webhooks", Label: "Webhooks", MinRead: "admin"},
	{ID: "activity", Label: "Activity", MinRead: "admin"},
	{ID: "danger", Label: "Danger", Danger: true, MinRead: "admin"},
}

// settingsRoleRank orders the repo roles (identity.Role spellings). An empty
// or unknown role ranks 0 — below every tab floor — so the page fails closed.
// identity.Resolve answers "" for a principal with no binding (an authenticated
// non-member of a public repo, an unrecognised spelling); ranking that as admin
// was the round-2 audit defect (every tab 200 with live forms). Only the
// never-wired identity seam (settingsRepoAccess) assumes admin, and it says so
// at the assignment because there is no role concept to consult there.

// settingsWebhookPanel builds the shared Webhooks tab payload. Repo and org
// differ only in the action plumbing: repo rows carry the id in the URL and a
// webhook_action field; org rows post one URL with org_action + a hidden id.
func settingsWebhookPanel(rows []SettingsWebhook, wired bool, createAction string, createFields []SettingsKV, actionURL func(id string) string, actionField, prefix, idField string, newURL, newEvents, errText string) *SettingsWebhookPanel {
	p := &SettingsWebhookPanel{
		Wired: wired, CreateAction: createAction, CreateFields: createFields,
		NewURL: newURL, NewEvents: newEvents, Error: errText,
	}
	for _, h := range rows {
		row := SettingsWebhookRow{
			SettingsWebhook: h,
			ActionURL:       actionURL(h.ID),
			ActionField:     actionField,
			ToggleValue:     prefix + "toggle",
			RotateValue:     prefix + "rotate",
			PingValue:       prefix + "ping",
			DeleteValue:     prefix + "delete",
			UpdateValue:     prefix + "update",
			RedeliverValue:  prefix + "redeliver",
		}
		if idField != "" {
			row.Fields = []SettingsKV{{Key: idField, Value: h.ID}}
		}
		p.Rows = append(p.Rows, row)
	}
	return p
}

// settingsRepoWebhookPanel adapts the repo admin rows.
func settingsRepoWebhookPanel(owner, name string, admin *SettingsAdmin, newURL, newEvents, errText string) *SettingsWebhookPanel {
	if admin == nil {
		return &SettingsWebhookPanel{Error: errText, EventHints: notify.RepoHookTopics(), OffersPush: true}
	}
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/settings/webhooks"
	p := settingsWebhookPanel(admin.Webhooks, admin.WebhooksWired, base, nil,
		func(id string) string { return base + "/" + url.PathEscape(id) },
		"webhook_action", "", "", newURL, newEvents, errText)
	p.CanEdit = true
	p.EventHints = notify.RepoHookTopics()
	p.OffersPush = true
	return p
}

// settingsOrgWebhookPanel adapts the org rows.
func settingsOrgWebhookPanel(org string, page *SettingsOrgPage) *SettingsWebhookPanel {
	action := "/" + url.PathEscape(org) + "/settings"
	p := settingsWebhookPanel(page.Webhooks, page.WebhooksWired, action,
		[]SettingsKV{{Key: "org_action", Value: SettingsOrgWebhookCreate}},
		func(string) string { return action },
		"org_action", "webhook-", "id", page.NewURL, page.NewEvents, page.Error)
	p.EventHints = notify.OrgHookTopics()
	return p
}
func settingsRoleRank(role string) int {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "read":
		return 1
	case "triage":
		return 2
	case "write":
		return 3
	case "maintain":
		return 4
	case "admin":
		return 5
	}
	return 0
}

func settingsRoleAtLeast(role, want string) bool {
	return settingsRoleRank(role) >= settingsRoleRank(want)
}

// settingsTabByID resolves a repo tab id: "" = unknown, "pushmirror" aliases
// "mirror" (#627). A bare "" path segment is the default tab.
func settingsTabByID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return "general"
	}
	if id == "pushmirror" {
		return "mirror"
	}
	for _, t := range settingsRepoTabs {
		if t.ID == id {
			return id
		}
	}
	return ""
}

func settingsOrgTabByID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return "profile"
	}
	for _, t := range settingsOrgTabs {
		if t.ID == id {
			return id
		}
	}
	return ""
}

func settingsTabMinRead(tabs []settingsTab, id string) string {
	for _, t := range tabs {
		if t.ID == id {
			if t.MinRead == "" {
				return "read"
			}
			return t.MinRead
		}
	}
	return "read"
}

// ---------------------------------------------------------------------------
// routes
// ---------------------------------------------------------------------------

func init() { registerGroup((*Server).mountSettings) }

// mountSettings registers the group's routes. Tests call it directly so a
// sibling group's half-written route table cannot affect this group.
func (s *Server) mountSettings() {
	m := &s.mux

	m.HandleFunc("GET /keys", s.settingsKeysPage)
	m.HandleFunc("POST /keys", s.settingsKeyAdd)
	m.HandleFunc("POST /keys/{id}/delete", s.settingsKeyDelete)

	// Repo settings: path-segment tabs (see the ROUTE SHAPE note).
	m.HandleFunc("GET /{owner}/{name}/settings", s.settingsRepoRedirect)
	m.HandleFunc("GET /{owner}/{name}/settings/{tab}", s.settingsRepoTab)
	// One ruleset's form (PLAN-V2 §2.4): the same tab handler, scoped by the
	// path's slug. Only the protection tab has per-item GET pages today, so
	// any other tab rejects the deeper path instead of rendering a form for
	// an id it does not own.
	m.HandleFunc("GET /{owner}/{name}/settings/{tab}/{id}", s.settingsRepoTab)
	m.HandleFunc("POST /{owner}/{name}/settings/{tab}", s.settingsRepoWrite)
	m.HandleFunc("POST /{owner}/{name}/settings/{tab}/{id}", s.settingsRepoWrite)
	// Reports rows own one deeper write shape (a resolve action names the
	// verb in its path, the M2 moderation-action precedent).
	m.HandleFunc("POST /{owner}/{name}/settings/reports/{id}/{action}", s.settingsReportAction)

	// Org settings: ?tab= (see the ROUTE SHAPE note).
	m.HandleFunc("GET /{owner}/settings", s.settingsOrgPage)
	m.HandleFunc("POST /{owner}/settings", s.settingsOrgSubmit)
	m.HandleFunc("POST /{owner}/settings/preview", s.settingsOrgPreview)
}

// ---------------------------------------------------------------------------
// /keys
// ---------------------------------------------------------------------------

// settingsKeyRows adapts the registry records to table rows.
func settingsKeyRows(recs []api.SshKeyRecord) []SettingsKeyRow {
	out := make([]SettingsKeyRow, 0, len(recs))
	for _, k := range recs {
		added := ""
		if !k.CreatedAt.IsZero() {
			added = k.CreatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, SettingsKeyRow{ID: k.ID, Title: k.Title, Short: shortKey(k.Key), Added: relTime(added)})
	}
	return out
}

// settingsKeysPage answers GET /keys: the calling principal's SSH keys.
//
// This is an identity-owned page, so it carries the identity gate rather than
// the anonymous_read gate: a signed-out caller gets the API's 401 — never an
// "Add a key" form nobody owns (Forgejo #371).
func (s *Server) settingsKeysPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	page := &SettingsKeysPage{}
	switch s.identityAccessFor(p) {
	case identitySignIn:
		s.errorPage(w, r, http.StatusUnauthorized, "sign in to manage your SSH keys")
		return
	}
	q := r.URL.Query()
	switch q.Get("notice") {
	case "added":
		page.Notice = "key added — clone with ssh:// (the key is live immediately)"
	case "removed":
		page.Notice = "key removed"
	}
	if q.Get("error") != "" {
		page.Error = q.Get("error")
	}
	if s.env.API == nil || s.env.API.SSHKeys == nil {
		page.NotWired = true
		s.page(w, r, p, http.StatusOK, "settings/keys", "SSH keys", page)
		return
	}
	keys, err := s.env.API.SSHKeys.List(r.Context(), p.Name)
	if err != nil {
		log.Printf("webui keys list %s: %v", r.URL.Path, err)
		page.Error = "key registry unavailable"
	}
	page.Keys = settingsKeyRows(keys)
	s.page(w, r, p, http.StatusOK, "settings/keys", "SSH keys", page)
}

// settingsKeyAdd answers POST /keys: the registry owns validation; the page
// renders its error verbatim (no duplicated shape check). writePrincipal
// carries the identity gate (401 signed out, 403 when auth is disabled) plus
// the same-origin rule.
func (s *Server) settingsKeyAdd(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if s.env.API == nil || s.env.API.SSHKeys == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "ssh key registry not configured")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	key := strings.TrimSpace(r.PostFormValue("key"))
	title := strings.TrimSpace(r.PostFormValue("title"))
	if key == "" {
		s.settingsKeyReject(w, r, p, "paste an authorized_keys line first", key, title)
		return
	}
	if _, err := s.env.API.SSHKeys.Add(r.Context(), p.Name, key, title); err != nil {
		s.settingsKeyReject(w, r, p, err.Error(), key, title)
		return
	}
	http.Redirect(w, r, "/keys?notice=added", http.StatusSeeOther)
}

// settingsKeyReject re-renders the page with the registry's message and the
// typed values (400 for a shape problem, 503 when the registry is down).
func (s *Server) settingsKeyReject(w http.ResponseWriter, r *http.Request, p auth.Principal, msg, key, title string) {
	page := &SettingsKeysPage{KeyLine: key, Title: title, Error: msg}
	if s.env.API != nil && s.env.API.SSHKeys != nil {
		recs, _ := s.env.API.SSHKeys.List(r.Context(), p.Name)
		page.Keys = settingsKeyRows(recs)
	}
	status := http.StatusBadRequest
	if settingsHTMX(r) {
		status = http.StatusOK
	}
	s.page(w, r, p, status, "settings/keys", "SSH keys", page)
}

// settingsKeyDelete answers POST /keys/{id}/delete (the same identity gate as
// the add: writePrincipal).
func (s *Server) settingsKeyDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.writePrincipal(w, r)
	if !ok {
		return
	}
	if s.env.API == nil || s.env.API.SSHKeys == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "ssh key registry not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.env.API.SSHKeys.Delete(r.Context(), p.Name, id); err != nil {
		page := &SettingsKeysPage{Error: err.Error()}
		recs, _ := s.env.API.SSHKeys.List(r.Context(), p.Name)
		page.Keys = settingsKeyRows(recs)
		status := http.StatusBadRequest
		if settingsHTMX(r) {
			status = http.StatusOK
		}
		s.page(w, r, p, status, "settings/keys", "SSH keys", page)
		return
	}
	http.Redirect(w, r, "/keys?notice=removed", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// repo settings
// ---------------------------------------------------------------------------

// settingsRepoRedirect answers GET /{owner}/{name}/settings: a bare visit lands
// on the default tab (a fragment never reaches the server, so old #hash links
// land here and the default tab renders — never a blank page).
func (s *Server) settingsRepoRedirect(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.gated(w, r); !ok {
		return
	}
	http.Redirect(w, r,
		"/"+url.PathEscape(r.PathValue("owner"))+"/"+url.PathEscape(r.PathValue("name"))+"/settings/general",
		http.StatusSeeOther)
}

// settingsRepoAccess resolves the principal, the repo read gate, and the
// caller's role. ok=false means the response is already written.
func (s *Server) settingsRepoAccess(w http.ResponseWriter, r *http.Request) (auth.Principal, string, bool) {
	p, ok := s.gated(w, r)
	if !ok {
		return p, "", false
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	if !s.repoVisible(r.Context(), owner, name, p) {
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return p, "", false
	}
	// Existence mirrors the JSON surface's 404: a page must never render a
	// settings shell for a repo the API does not know (audit F3). Probed
	// after visibility so an unreadable name never reveals more than 404.
	exists, xerr := s.repoExists(r.Context(), owner, name)
	if xerr != nil {
		log.Printf("webui repo existence %s: %v", r.URL.Path, xerr)
		s.errorPage(w, r, http.StatusServiceUnavailable, "repository lookup failed")
		return p, "", false
	}
	if !exists {
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return p, "", false
	}
	d := settingsDeps()
	role := "admin" // identity seam unwired: no role concept exists; the read gate already ran
	if d.RepoRole != nil {
		got, err := d.RepoRole(r.Context(), owner, name, p)
		if err != nil {
			log.Printf("webui repo access %s: %v", r.URL.Path, err)
			s.errorPage(w, r, http.StatusServiceUnavailable, "access check failed")
			return p, "", false
		}
		role = got
	}
	// No rank special-case here: a resolved-but-unknown role ("") is a
	// visible-repo principal with no role, so the per-tab floor answers 403
	// (the round-2 audit defect ranked it admin and served every form). A
	// private repo never reaches this point — repoVisible answered 404 first.
	return p, role, true
}

// settingsRepoTab answers GET /{owner}/{name}/settings/{tab}.
func (s *Server) settingsRepoTab(w http.ResponseWriter, r *http.Request) {
	p, role, ok := s.settingsRepoAccess(w, r)
	if !ok {
		return
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	tab := settingsTabByID(r.PathValue("tab"))
	if tab == "" {
		// Stale/unknown tab: the default, never blank (§2 tab contract).
		http.Redirect(w, r,
			"/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/settings/general",
			http.StatusSeeOther)
		return
	}
	if id := r.PathValue("id"); id != "" && tab != "protection" {
		// Only the Branch protection tab has per-item GET pages.
		s.errorPage(w, r, http.StatusNotFound, "no such settings page")
		return
	}
	admin := settingsRoleAtLeast(role, "admin")
	if !settingsRoleAtLeast(role, settingsTabMinRead(settingsRepoTabs, tab)) {
		s.errorPage(w, r, http.StatusForbidden, "this settings tab requires a higher role")
		return
	}
	page := s.settingsRepoPage(r, p, owner, name, tab, role, admin)
	page.Saved = r.URL.Query().Get("saved") == "1"
	page.Notice = r.URL.Query().Get("notice")
	if tab == "config" {
		settingsRevert(page, r.URL.Query().Get("rev"))
	}
	s.pageRepo(w, r, p, page.Chrome, http.StatusOK, "settings/repo", "Settings", page)
}

// settingsRevert seeds the config editor from a history revision ("revert to
// this" is a link, never a write: publishing is still an explicit Save).
func settingsRevert(page *SettingsRepoPage, rev string) {
	if page.Config == nil || rev == "" {
		return
	}
	for _, r := range page.Config.Revisions {
		if strconv.FormatUint(r.Rev, 10) != rev {
			continue
		}
		page.Config.Text = r.TOML
		page.Config.Message = "revert to #" + rev
		page.Notice = "loaded #" + rev + " into the editor — publish to make it current"
		return
	}
}

// settingsRepoPage loads the active tab's data. A nil dep leaves the panel
// nil, which the template renders as a "not wired" card — never a 500.
func (s *Server) settingsRepoPage(r *http.Request, p auth.Principal, owner, name, tab, role string, admin bool) *SettingsRepoPage {
	d := settingsDeps()
	page := &SettingsRepoPage{Owner: owner, Name: name, Tab: tab, Role: role, Admin: admin}
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/settings/"
	for _, t := range settingsRepoTabs {
		item := SettingsNavItem{ID: t.ID, Label: t.Label, Href: base + t.ID, Active: t.ID == tab, Danger: t.Danger}
		if !settingsRoleAtLeast(role, t.MinRead) {
			continue // admin-only entries are hidden, not shown-and-403'd
		}
		if t.Danger {
			page.Danger = append(page.Danger, item)
		} else {
			page.Nav = append(page.Nav, item)
		}
	}
	ctx := r.Context()
	// Repository chrome above the settings columns: one summary read (the
	// repo masthead's own read), AFTER the read gate. A failure leaves it nil —
	// the settings gate already passed, so chrome must not fail the page.
	page.Chrome = s.repoChromeFor(r, p, owner, name)
	switch tab {
	case "general":
		if d.RepoGeneral != nil {
			if v, err := d.RepoGeneral(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				if v.MaxLength == 0 {
					v.MaxLength = 512
				}
				if v.MaxWebsite == 0 {
					v.MaxWebsite = config.MaxRepoWebsiteBytes
				}
				settingsNormalizeFeatures(v)
				page.General = v
			}
		}
	case "scheduled":
		if d.RepoScheduled != nil {
			if v, err := d.RepoScheduled(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else {
				page.Scheduled = v
			}
		}
	case "mirror":
		if d.RepoMirror != nil {
			if v, err := d.RepoMirror(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				if len(v.Presets) == 0 {
					v.Presets = settingsMirrorPresets
				}
				page.Mirror = v
			}
		}
		if d.RepoPushMirror != nil {
			if v, err := d.RepoPushMirror(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				if len(v.Presets) == 0 {
					v.Presets = settingsPushPresets
				}
				if len(v.AuthKinds) == 0 {
					v.AuthKinds = settingsPushAuthKinds
				}
				page.Push = v
			}
		}
	case "policy":
		if d.RepoPolicy != nil {
			if v, err := d.RepoPolicy(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else {
				if v != nil && v.DryRunN == 0 {
					v.DryRunN = 10
				}
				page.Policy = v
			}
		}
	case "protection":
		if d.RepoProtection != nil {
			if v, err := d.RepoProtection(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				// Two renderings of one tab: the ruleset list (no slug) or one
				// ruleset's form (`/{slug}`, "new"). The effective view answers
				// for ?ref=, defaulting to the first stored pattern.
				v.Select(r.PathValue("id"), r.URL.Query().Get("ref"), p.Name)
				page.Protection = v
			}
		}
	case "config":
		if d.RepoConfig != nil {
			if v, err := d.RepoConfig(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else {
				page.Config = v
			}
		}
	case "access":
		if d.RepoAccess != nil {
			if v, err := d.RepoAccess(ctx, owner, name, p); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				if len(v.Roles) == 0 {
					v.Roles = settingsAccessRoles
				}
				// CanEdit is stamped from the page's admin verdict below.
				page.Access = v
			}
		}
	case "tokens", "webhooks", "runners", "secrets":
		if d.RepoAdmin != nil {
			if v, err := d.RepoAdmin(ctx, owner, name, p); err != nil {
				page.Error = err.Error()
			} else {
				page.AdminData = v
			}
		}
	case "environments":
		if d.RepoEnvironments != nil {
			if v, err := d.RepoEnvironments(ctx, owner, name, p); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				page.Environments = v
			}
		}
	case "danger":
		page.DangerData = &SettingsDanger{FullName: owner + "/" + name}
	case "reports":
		state := r.URL.Query().Get("state")
		if state != "resolved" && state != "open" {
			state = "all"
		}
		page.Reports = settingsReportsRead(ctx, d, p, owner, name, state)
	case "wal":
		// WP9 G16: the real panel — the WAL view's facts plus the same ops
		// inventory. Posting an op uses the existing …/api/ops/{op} stream
		// (AuthWrite), so this page never carries a second write path.
		if d.RepoWAL != nil {
			if v, err := d.RepoWAL(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				v.Panel.CanRun = p.Admin || settingsRoleAtLeast(role, "write")
				page.WAL = v
			}
		}
	}
	// The Access panel's editability follows the page's admin verdict, not the
	// seam: a restore path synthesizes the panel with only the submitted
	// values, and triage must never regain the form that way.
	if page.Access != nil {
		page.Access.CanEdit = admin
	}
	if tab == "webhooks" {
		newURL, newEvents := "", ""
		if page.AdminData != nil {
			newURL, newEvents = page.AdminData.NewURL, page.AdminData.NewEvents
		}
		page.WebhookPanel = settingsRepoWebhookPanel(owner, name, page.AdminData, newURL, newEvents, "")
	}
	return page
}

// settingsNormalizeFeatures fills the seven feature rows (labels/hints ported
// from web/src/lib/repoFeatures.js) when the dep left them out.
func settingsNormalizeFeatures(g *SettingsGeneral) {
	present := map[string]bool{}
	for i := range g.Features {
		present[g.Features[i].Key] = true
		for _, f := range settingsFeatureDefs {
			if g.Features[i].Key != f.Key {
				continue
			}
			if g.Features[i].Label == "" {
				g.Features[i].Label = f.Label
			}
			if g.Features[i].Hint == "" {
				g.Features[i].Hint = f.Hint
			}
		}
	}
	for _, f := range settingsFeatureDefs {
		if present[f.Key] {
			continue
		}
		g.Features = append(g.Features, SettingsFeatureRow{Key: f.Key, Label: f.Label, Hint: f.Hint, Enabled: true})
	}
}

type settingsFeatureDef struct{ Key, Label, Hint string }

var settingsFeatureDefs = []settingsFeatureDef{
	{"issues", "Issues", "Show the Issues tab and allow new issues"},
	{"pulls", "Pull requests", "Show the Pulls tab and allow new pull requests"},
	{"releases", "Releases", "Show the Releases tab"},
	{"forks", "Forking", "Allow forking this repository"},
	{"watch", "Watching", "Allow new watchers (existing watchers keep working)"},
	{"star", "Starring", "Allow new stars (existing stars are kept and counted)"},
	{"packages", "Packages", "Serve an OCI registry (docker push/pull) for this repository"},
}

var settingsMirrorPresets = []SettingsOption{
	{"hourly", "Every hour"}, {"8h", "Every 8 hours"}, {"daily", "Every day"},
	{"weekly", "Every week"}, {"monthly", "Every month"},
}

var settingsPushPresets = []SettingsOption{
	{"", "On push only (no schedule)"}, {"hourly", "Every hour"}, {"8h", "Every 8 hours"},
	{"daily", "Every day"}, {"weekly", "Every week"}, {"monthly", "Every month"},
}

var settingsPushAuthKinds = []SettingsOption{
	{"none", "No auth (public upstream / file backup)"},
	{"password", "Username + password (HTTPS)"},
	{"token", "Access token (HTTPS)"},
	{"ssh", "SSH deploy key"},
}

var settingsAccessRoles = []string{"read", "triage", "write", "maintain", "admin"}

// settingsRepoWrite answers POST /{owner}/{name}/settings/{tab}[/{id}].
func (s *Server) settingsRepoWrite(w http.ResponseWriter, r *http.Request) {
	p, role, ok := s.settingsRepoAccess(w, r)
	if !ok {
		return
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	tab := settingsTabByID(r.PathValue("tab"))
	if tab == "" {
		tab = "general"
	}
	action := settingsRepoAction(tab)
	if tab == "mirror" {
		switch r.PostFormValue("mirror_action") {
		case "put":
			action = SettingsActionMirrorPut
		case "schedule":
			action = SettingsActionMirrorSchedule
		case "sync":
			action = SettingsActionMirrorSync
		case "remove":
			action = SettingsActionMirrorRemove
		}
		switch r.PostFormValue("push_action") {
		case "put":
			action = SettingsActionPushPut
		case "schedule":
			action = SettingsActionPushSchedule
		case "keygen":
			action = SettingsActionPushKeygen
		case "sync":
			action = SettingsActionPushSync
		case "remove":
			action = SettingsActionPushRemove
		}
	}
	if tab == "access" {
		switch r.PostFormValue("invite_action") {
		case "create":
			action = SettingsActionInviteCreate
		case "cancel":
			action = SettingsActionInviteCancel
		}
	}
	if tab == "policy" && r.PostFormValue("policy_action") == "dryrun" {
		action = SettingsActionPolicyDryRun
	}
	if tab == "policy" && r.PostFormValue("policy_action") == "remove" {
		action = SettingsActionPolicyRemove
	}
	if tab == "protection" && r.PostFormValue("policy_action") == "raw" {
		// The advanced editor on the Branch protection tab writes the same
		// raw document through the same validation (never the only path).
		action = SettingsActionPolicy
	}
	if tab == "config" && r.PostFormValue("config_action") == "clear" {
		action = SettingsActionConfigClear
	}
	if tab == "tokens" {
		action = SettingsActionTokenCreate
		if r.PathValue("id") != "" {
			action = SettingsActionTokenRevoke
		}
	}
	if tab == "secrets" {
		action = SettingsActionSecretPut
		if id := r.PathValue("id"); id != "" {
			action = SettingsActionSecretDelete
		} else if r.PostFormValue("secret_action") == "delete" {
			action = SettingsActionSecretDelete
		}
	}
	if tab == "environments" {
		action = SettingsActionEnvironmentPut
		if r.PathValue("id") != "" {
			switch r.PostFormValue("environment_action") {
			case "secret-put":
				action = SettingsActionEnvironmentSecretPut
			case "secret-delete":
				action = SettingsActionEnvironmentSecretDelete
			default:
				action = SettingsActionEnvironmentDelete
			}
		}
	}
	if tab == "webhooks" {
		action = SettingsActionWebhookCreate
		if id := r.PathValue("id"); id != "" {
			action = SettingsActionWebhookDelete
			switch r.PostFormValue("webhook_action") {
			case "ping":
				action = SettingsActionWebhookPing
			case "toggle":
				action = SettingsActionWebhookToggle
			case "update":
				action = SettingsActionWebhookUpdate
			case "rotate":
				action = SettingsActionWebhookRotate
			case "redeliver":
				action = SettingsActionWebhookRedeliver
			default:
				action = SettingsActionWebhookDelete
			}
		}
	}
	if tab == "danger" {
		if r.PostFormValue("danger_action") == "transfer" {
			action = SettingsActionTransfer
		} else {
			action = SettingsActionDelete
		}
	}
	// Every repo settings mutation is gated (§5 write matrix). Deployment
	// environments are a maintain-floor surface (their create/update/delete is
	// the maintainer's job, docs/PLAN-THREADS-ENVIRONMENTS.md §7); every other
	// tab stays admin-only.
	settingsWriteRole := "admin"
	if tab == "environments" {
		settingsWriteRole = "maintain"
	}
	if !settingsRoleAtLeast(role, settingsWriteRole) {
		s.errorPage(w, r, http.StatusForbidden, "saving repository settings requires the "+settingsWriteRole+" role")
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
	// Danger actions carry a typed confirmation; the server re-checks it (the
	// client-side arm state is an affordance, never the gate). Both actions
	// require the exact `owner/name` (case-sensitive, no trimming — #39).
	// Removing the policy document is equally destructive (fail-open) and uses
	// the same typed arm.
	if action == SettingsActionTransfer || action == SettingsActionDelete ||
		action == SettingsActionPolicyRemove {
		if r.PostFormValue("confirm") != owner+"/"+name {
			page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
			if action == SettingsActionPolicyRemove {
				if page.Policy == nil {
					page.Policy = &SettingsPolicy{}
				}
				page.Policy.Error = "type " + owner + "/" + name + " exactly to confirm removal"
			} else {
				page.DangerData.Error = "type " + owner + "/" + name + " exactly to confirm"
			}
			s.settingsRepoFinish(w, r, p, page, nil)
			return
		}
	}
	// Webhook deletion is destructive: the row's typed confirmation names the
	// hook id, and the server re-checks it (the disabled button is only an
	// affordance, never the boundary).
	if action == SettingsActionWebhookDelete {
		if r.PostFormValue("confirm") != r.PathValue("id") {
			page := s.settingsRepoPage(r, p, owner, name, "webhooks", role, true)
			page.Notice = ""
			page.Error = "type " + r.PathValue("id") + " exactly to confirm"
			s.settingsRepoFinish(w, r, p, page, nil)
			return
		}
	}
	wr := SettingsRepoWrite{Action: action, Values: settingsFormValues(r, "")}
	wr.Multi = map[string][]string{}
	for name, vals := range r.PostForm {
		wr.Multi[name] = append([]string{}, vals...)
	}
	if id := r.PathValue("id"); id != "" {
		wr.Values["id"] = id
	}
	d := settingsDeps()
	if d.RepoWrite == nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Error = "this settings write is not wired on this build"
		s.pageRepo(w, r, p, page.Chrome, http.StatusServiceUnavailable, "settings/repo", "Settings", page)
		return
	}
	werrRes, werr := d.RepoWrite(r.Context(), owner, name, p, wr)
	if werr != nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Error = werr.Message
		settingsRestoreRepoValues(page, r, action, werr)
		s.settingsRepoFinish(w, r, p, page, werr)
		return
	}
	// The one-time CI token secret must never enter a URL (logs/history), so
	// a token create answers with the page directly; every other write is a
	// 303 (htmx follows it and re-selects #settings-panel).
	if werrRes.Secret != "" {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		if page.AdminData == nil {
			page.AdminData = &SettingsAdmin{}
		}
		page.AdminData.NewToken = werrRes.Secret
		page.Notice = werrRes.Notice
		s.pageRepo(w, r, p, page.Chrome, http.StatusOK, "settings/repo", "Settings", page)
		return
	}
	// A dry run returns its verdict with the typed document: render it in
	// place (the stored policy is not what was evaluated).
	if werrRes.Policy != nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Policy = werrRes.Policy
		page.Notice = werrRes.Notice
		s.pageRepo(w, r, p, page.Chrome, http.StatusOK, "settings/repo", "Settings", page)
		return
	}
	// A started background task renders its live card in place (the source page
	// stops existing when a transfer commits, so a 303 would race the move).
	if werrRes.Task != nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		if page.DangerData == nil {
			page.DangerData = &SettingsDanger{FullName: owner + "/" + name}
		}
		page.DangerData.Task = werrRes.Task
		page.Notice = werrRes.Notice
		s.pageRepo(w, r, p, page.Chrome, http.StatusOK, "settings/repo", "Settings", page)
		return
	}
	// A created invitation's accept link carries its capability token; it is
	// rendered once in the page body (never a redirect/URL) and the pending
	// list below it also carries the link.
	if werrRes.Link != "" {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		if page.Access == nil {
			page.Access = &SettingsAccess{}
		}
		page.Access.InviteLink = werrRes.Link
		page.Notice = werrRes.Notice
		s.pageRepo(w, r, p, page.Chrome, http.StatusOK, "settings/repo", "Settings", page)
		return
	}
	target := "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/settings/" + tab
	if werrRes.Redirect != "" {
		target = werrRes.Redirect
	}
	q := "?saved=1"
	if werrRes.Notice != "" {
		q = "?notice=" + url.QueryEscape(werrRes.Notice)
	}
	http.Redirect(w, r, target+q, http.StatusSeeOther)
}

// settingsRepoFinish renders a rejected repo write: 422 (or 409) for a real
// page load, 200 for htmx so the panel swaps with the inline error.
func (s *Server) settingsRepoFinish(w http.ResponseWriter, r *http.Request, p auth.Principal, page *SettingsRepoPage, werr *SettingsWriteError) {
	// The shared webhook panel owns its inline error line (the pre-panel
	// form did); mirror the page-level error into it on every render path.
	if page.WebhookPanel != nil && page.Error != "" {
		page.WebhookPanel.Error = page.Error
	}
	status := http.StatusUnprocessableEntity
	if werr != nil {
		status = werr.status()
	}
	if settingsHTMX(r) {
		status = http.StatusOK
	}
	s.pageRepo(w, r, p, page.Chrome, status, "settings/repo", "Settings", page)
}

// settingsRepoAction maps a tab to its single-shot write action.
func settingsRepoAction(tab string) string {
	switch tab {
	case "general":
		return SettingsActionGeneral
	case "access":
		return SettingsActionAccess
	case "policy":
		return SettingsActionPolicy
	case "protection":
		return SettingsActionProtection
	case "config":
		return SettingsActionConfig
	}
	return tab
}

// settingsRestoreRepoValues puts the submitted values back on the page after a
// rejected write, so the operator never loses the edit.
func settingsRestoreRepoValues(page *SettingsRepoPage, r *http.Request, action string, werr *SettingsWriteError) {
	get := func(k string) string { return r.PostFormValue(k) }
	switch action {
	case SettingsActionScheduled:
		settingsOverlayScheduledForm(page, SettingsRepoWrite{
			Action: action,
			Values: settingsFormValues(r, ""),
			Multi:  map[string][]string{},
		}, werr)
	case SettingsActionGeneral:
		if page.General == nil {
			page.General = &SettingsGeneral{}
		}
		page.General.Description = get("description")
		page.General.Website = get("website")
		page.General.SelfApproval = get("allow_self_approval") == "true"
		for i := range page.General.Features {
			if v := get("feature_" + page.General.Features[i].Key); v != "" {
				page.General.Features[i].Enabled = v == "true"
			}
		}
	case SettingsActionAccess:
		if page.Access == nil {
			page.Access = &SettingsAccess{}
		}
		page.Access.CanEdit = page.Admin
		if v := get("visibility"); v != "" {
			page.Access.Visibility = v
		}
	case SettingsActionInviteCreate, SettingsActionInviteCancel:
		// A rejected invite write re-renders the Access tab with the typed
		// subject/role preserved (the service error is the page error).
		if page.Access == nil {
			page.Access = &SettingsAccess{}
		}
		page.Access.NewSubject = get("subject")
		page.Access.NewRole = get("role")
	case SettingsActionPolicy:
		if page.Policy == nil {
			page.Policy = &SettingsPolicy{}
		}
		page.Policy.Text = get("policy")
		if page.Protection != nil {
			// The advanced editor lives on the protection tab too.
			page.Protection.Raw = get("policy")
		}
	case SettingsActionProtection:
		// A rejected ruleset write re-renders the form with the submitted
		// values (the operator never loses the edit). The view already
		// selected the ruleset from the path; overlay the fields on it.
		if page.Protection == nil {
			page.Protection = &SettingsProtection{Roles: settingsProtectionRoles, Methods: settingsMergeMethodOptions}
		}
		in := settingsRulesetInputFrom(SettingsRepoWrite{Values: settingsFormValues(r, ""), Multi: map[string][]string(r.PostForm)})
		if page.Protection.Editing == nil {
			page.Protection.Editing = &SettingsRuleset{Enabled: true, Editable: true, BlockForce: true, BlockDelete: true}
		}
		rs := page.Protection.Editing
		if in.NewSlug != "" {
			rs.Slug = in.NewSlug
		}
		rs.Enabled = in.Enabled
		rs.RefPatterns = in.Refs
		rs.Refs = strings.Join(in.Refs, ", ")
		rs.Approvals = in.Approvals
		rs.DismissStale = in.DismissStale
		rs.Checks = in.Checks
		rs.ChecksText = strings.Join(in.Checks, ", ")
		rs.BlockForce = in.BlockForce
		rs.BlockDelete = in.BlockDelete
		rs.RequireUpToDate = in.RequireUpToDate
		rs.MergeMethods = in.MergeMethods
		rs.PushRoles = in.PushRoles
	case SettingsActionConfig:
		if page.Config == nil {
			page.Config = &SettingsConfig{}
		}
		page.Config.Text = get("config")
		page.Config.Message = get("message")
	case SettingsActionMirrorPut:
		if page.Mirror == nil {
			page.Mirror = &SettingsMirror{}
		}
		page.Mirror.Upstream = get("upstream_url")
		page.Mirror.Schedule = get("schedule")
	case SettingsActionPushPut, SettingsActionPushSchedule:
		if page.Push == nil {
			page.Push = &SettingsPushMirror{}
		}
		page.Push.Upstream = get("upstream_url")
		page.Push.Username = get("username")
		page.Push.Schedule = get("schedule")
		page.Push.AuthKind = get("auth_kind")
	case SettingsActionTokenCreate:
		if page.AdminData == nil {
			page.AdminData = &SettingsAdmin{}
		}
		page.AdminData.NewName = get("name")
	case SettingsActionEnvironmentPut, SettingsActionEnvironmentSecretPut:
		// A rejected environment write re-renders the tab with the typed values
		// (never a secret value: the value input is empty by design).
		if page.Environments == nil {
			page.Environments = &SettingsEnvironmentPanel{}
		}
		page.Environments.NewName = get("environment_name")
		page.Environments.NewReviewers = get("environment_reviewers")
		page.Environments.NewSecret = get("environment_secret_name")
	case SettingsActionWebhookCreate:
		if page.AdminData == nil {
			page.AdminData = &SettingsAdmin{}
		}
		page.AdminData.NewURL = get("url")
		page.AdminData.NewEvents = get("events")
		if page.WebhookPanel != nil {
			page.WebhookPanel.NewURL = get("url")
			page.WebhookPanel.NewEvents = get("events")
		}
	}
}

// ---------------------------------------------------------------------------
// org settings
// ---------------------------------------------------------------------------

// settingsOrgTab resolves the ?tab= selection (see ROUTE SHAPE).
func settingsOrgTab(r *http.Request) string {
	tab := settingsOrgTabByID(r.URL.Query().Get("tab"))
	if tab == "" {
		return "profile"
	}
	return tab
}

// settingsOrgPage answers GET /{owner}/settings[?tab=…].
func (s *Server) settingsOrgPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	org := r.PathValue("owner")
	tab := settingsOrgTab(r)
	d := settingsDeps()
	page := &SettingsOrgPage{Org: org, Tab: tab}
	if d.OrgView == nil {
		page.Error = "organisation settings are not wired on this build"
		page.Nav = settingsOrgNav(org, tab, false)
		s.page(w, r, p, http.StatusOK, "settings/org", "Organization settings", page)
		return
	}
	view, err := d.OrgView(r.Context(), org, p)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "organization not found")
		return
	}
	if view == nil {
		s.errorPage(w, r, http.StatusNotFound, "organization not found")
		return
	}
	view.Org = org
	view.Tab = tab
	view.Nav = settingsOrgNav(org, tab, view.CanManage)
	view.WebhookPanel = settingsOrgWebhookPanel(org, view)
	view.Notice = r.URL.Query().Get("notice")
	if view.Error == "" {
		view.Error = r.URL.Query().Get("error")
	}
	if !settingsOrgTabVisible(tab, view.CanManage) {
		s.errorPage(w, r, http.StatusForbidden, "this tab requires an organization owner")
		return
	}
	s.page(w, r, p, http.StatusOK, "settings/org", "Organization settings", view)
}

// settingsOrgNav builds the pill nav; owner-only tabs are omitted entirely
// (Forgejo #348/#358/#364: never shown-then-403'd).
func settingsOrgNav(org, active string, canManage bool) []SettingsNavItem {
	out := make([]SettingsNavItem, 0, len(settingsOrgTabs))
	for _, t := range settingsOrgTabs {
		if t.MinRead == "admin" && !canManage {
			continue
		}
		out = append(out, SettingsNavItem{
			ID:     t.ID,
			Label:  t.Label,
			Href:   "/" + url.PathEscape(org) + "/settings?tab=" + url.QueryEscape(t.ID),
			Active: t.ID == active,
			Danger: t.Danger,
		})
	}
	return out
}

func settingsOrgTabVisible(tab string, canManage bool) bool {
	for _, t := range settingsOrgTabs {
		if t.ID == tab {
			return t.MinRead != "admin" || canManage
		}
	}
	return false
}

// settingsOrgSubmit answers POST /{owner}/settings.
func (s *Server) settingsOrgSubmit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	org := r.PathValue("owner")
	d := settingsDeps()
	if d.OrgView == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "organisation settings are not wired on this build")
		return
	}
	view, err := d.OrgView(r.Context(), org, p)
	if err != nil || view == nil {
		s.errorPage(w, r, http.StatusNotFound, "organization not found")
		return
	}
	if !view.CanManage {
		s.errorPage(w, r, http.StatusForbidden, "organization owner required")
		return
	}
	view.WebhookPanel = settingsOrgWebhookPanel(org, view)
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil && err != http.ErrNotMultipart {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	action := r.PostFormValue("org_action")
	if action == "" {
		action = SettingsOrgProfileSave
	}
	wr := SettingsOrgWrite{Action: action, Values: settingsFormValues(r, "")}
	if id := r.PostFormValue("id"); id != "" {
		wr.Values["id"] = id
	}
	if action == SettingsOrgAvatar {
		if f, hdr, ferr := r.FormFile("avatar"); ferr == nil {
			defer f.Close()
			buf := make([]byte, 2<<20+1)
			n, _ := f.Read(buf)
			wr.File = buf[:n]
			wr.Values["filename"] = hdr.Filename
		}
	}
	if action == SettingsOrgDelete {
		if r.PostFormValue("confirm") != org {
			view.Tab = "danger"
			view.Nav = settingsOrgNav(org, "danger", true)
			view.Error = "type " + org + " exactly to confirm"
			status := http.StatusUnprocessableEntity
			if settingsHTMX(r) {
				status = http.StatusOK
			}
			s.page(w, r, p, status, "settings/org", "Organization settings", view)
			return
		}
	}
	// Webhook delete is destructive: the typed confirmation names the hook id
	// and the server re-checks it (the island only arms the button).
	if action == SettingsOrgWebhookDelete {
		id := r.PostFormValue("id")
		if r.PostFormValue("confirm") != id {
			view.Tab = "webhooks"
			view.Nav = settingsOrgNav(org, "webhooks", true)
			view.Error = "type " + id + " exactly to confirm"
			if view.WebhookPanel != nil {
				view.WebhookPanel.Error = view.Error
			}
			status := http.StatusUnprocessableEntity
			if settingsHTMX(r) {
				status = http.StatusOK
			}
			s.page(w, r, p, status, "settings/org", "Organization settings", view)
			return
		}
	}
	tab := settingsOrgTabForAction(action)
	var res SettingsWriteResult
	var werr *SettingsWriteError
	switch {
	case settingsIsOrgWebhookAction(action):
		if d.OrgWebhookWrite == nil {
			werr = settingsWriteErr(http.StatusServiceUnavailable, "org webhooks are not wired on this build")
		} else {
			res, werr = d.OrgWebhookWrite(r.Context(), org, p, wr)
		}
	case d.OrgWrite == nil:
		werr = settingsWriteErr(http.StatusServiceUnavailable, "organisation settings writes are not wired on this build")
	default:
		res, werr = d.OrgWrite(r.Context(), org, p, wr)
	}
	if werr != nil {
		view.Tab = tab
		view.Nav = settingsOrgNav(org, tab, true)
		view.Error = werr.Message
		if view.WebhookPanel != nil {
			view.WebhookPanel.Error = werr.Message
		}
		status := werr.status()
		if settingsHTMX(r) {
			status = http.StatusOK
		}
		s.page(w, r, p, status, "settings/org", "Organization settings", view)
		return
	}
	if res.Redirect != "" {
		// Danger actions leave the page (the org no longer exists); the
		// notice rides a query so the destination can show it.
		dest := res.Redirect
		if res.Notice != "" {
			sep := "?"
			if strings.Contains(dest, "?") {
				sep = "&"
			}
			dest += sep + "notice=" + url.QueryEscape(res.Notice)
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	if res.Link != "" {
		// The shareable invite link is a one-shot response (it is a bearer
		// token): render it, never put it in the redirect URL. The created row
		// joins the pending table so its copy control sits where the operator
		// expects it; every later read renders the masked marker instead.
		view.Tab = tab
		view.Nav = settingsOrgNav(org, tab, true)
		view.InviteLink = res.Link
		view.Notice = res.Notice
		if res.Invite != nil {
			view.Invites = append(view.Invites, *res.Invite)
		}
		s.page(w, r, p, http.StatusOK, "settings/org", "Organization settings", view)
		return
	}
	notice := res.Notice
	if notice == "" {
		notice = "saved"
	}
	http.Redirect(w, r,
		"/"+url.PathEscape(org)+"/settings?tab="+url.QueryEscape(tab)+"&notice="+url.QueryEscape(notice),
		http.StatusSeeOther)
}

func settingsOrgTabForAction(action string) string {
	switch action {
	case SettingsOrgMemberSet, SettingsOrgMemberRm:
		return "members"
	case SettingsOrgTeamSave, SettingsOrgTeamUpdate, SettingsOrgTeamRm, SettingsOrgTeamAdd,
		SettingsOrgTeamDel, SettingsOrgTeamMemberRm:
		return "teams"
	case SettingsOrgInviteMake, SettingsOrgInviteRm:
		return "invitations"
	case SettingsOrgWebhookCreate, SettingsOrgWebhookToggle, SettingsOrgWebhookRotate,
		SettingsOrgWebhookPing, SettingsOrgWebhookDelete, SettingsOrgWebhookRedeliver:
		return "webhooks"
	case SettingsOrgDelete:
		return "danger"
	}
	return "profile"
}

// settingsOrgPreview answers POST /{owner}/settings/preview: the server-side
// markdown fragment for the bio editor (no marked/DOMPurify in the browser).
func (s *Server) settingsOrgPreview(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
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
	org := r.PathValue("owner")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = p
	_, _ = w.Write([]byte(`<div class="markdown-body" id="org-bio-preview" data-preview-for="org-bio">`))
	src := r.PostFormValue("bio_markdown")
	if src == "" {
		src = r.PostFormValue("bio")
	}
	_, _ = w.Write([]byte(mdrepoString(src, org, "")))
	_, _ = w.Write([]byte(`</div>`))
}

// mdrepoString renders markdown with org-relative link resolution.
func mdrepoString(src, owner, repo string) string {
	return string(RenderMarkdownIn(src, owner, repo))
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// settingsSameOrigin enforces the write rule (PLAN "Writes"): a mutation must
// carry an Origin or Referer whose host equals the request host. No header at
// all is refused — a browser form POST always carries one.
func settingsSameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// settingsHTMX reports whether the request came from htmx (which swaps only
// 2xx bodies; the same page renders either way).
func settingsHTMX(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

// settingsFormValues collects the form values, optionally keeping only a name
// prefix (and stripping it). A repeated name keeps its LAST value, which is
// how the hidden-false/checkbox-true boolean pair resolves.
func settingsFormValues(r *http.Request, prefix string) map[string]string {
	out := map[string]string{}
	for name, vals := range r.PostForm {
		if len(vals) == 0 {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
		}
		out[name] = vals[len(vals)-1]
	}
	return out
}

// settingsRenderFragment executes one named template from a page's set. Used
// by the Administration area's OIDC discovery fragment and this group's org
// bio preview (the latter is written directly). The set already carries base +
// every partial.
func (s *Server) settingsRenderFragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	t, ok := s.tpl[page]
	if !ok {
		s.errorPage(w, r, http.StatusInternalServerError, "page template missing: "+page)
		return
	}
	var buf strings.Builder
	// A fragment template addresses its own data directly (it has no layout),
	// so it receives the value, not PageData.
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "render: "+err.Error())
		return
	}
	httpsec.Apply(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, private")
	_, _ = w.Write([]byte(buf.String()))
}

// fmtSpecDuration renders a duration in the spec spelling ("1h", "7d") — the
// same spelling the TOML file and the setup API accept (ported from
// the former setup.js fmtSpecDuration). Display only: parsing must stay in
// config.ParseDuration.
func fmtSpecDuration(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	if settingsSpecDurationRe(s) {
		return s
	}
	total := 0.0
	if t, err := strconv.ParseFloat(s, 64); err == nil {
		total = t // bare = seconds
	} else {
		matched := false
		i := 0
		for i < len(s) {
			start := i
			for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
				i++
			}
			if start == i {
				i++
				continue
			}
			num, err := strconv.ParseFloat(s[start:i], 64)
			if err != nil {
				return v
			}
			unitStart := i
			for i < len(s) && !(s[i] >= '0' && s[i] <= '9') {
				i++
			}
			unit := s[unitStart:i]
			mul := 0.0
			switch unit {
			case "h":
				mul = 3600
			case "m":
				mul = 60
			case "s":
				mul = 1
			case "ms":
				mul = 0.001
			case "us", "µs":
				mul = 0.000001
			default:
				return v // unparseable → show as-is
			}
			total += num * mul
			matched = true
		}
		if !matched {
			return v
		}
	}
	steps := []struct {
		unit string
		mul  float64
	}{{"w", 604800}, {"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	for _, st := range steps {
		if total >= st.mul && total/st.mul == float64(int64(total/st.mul)) {
			return strconv.FormatInt(int64(total/st.mul), 10) + st.unit
		}
	}
	return strconv.FormatFloat(total, 'f', -1, 64) + "s"
}

func settingsSpecDurationRe(s string) bool {
	// ^\d+(\.\d+)?(ms|s|m|h|d|w)$
	unitAt := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c == '.' {
			continue
		}
		unitAt = i
		break
	}
	if unitAt <= 0 {
		return false
	}
	switch s[unitAt:] {
	case "ms", "s", "m", "h", "d", "w":
		return true
	}
	return false
}

// fmtSpecSize renders a byte count in the spec spelling ("64GiB") — largest
// unit that divides evenly, bare bytes otherwise (ported from fmtSpecSize).
func fmtSpecSize(v string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return v
	}
	steps := []struct {
		unit string
		mul  int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	for _, st := range steps {
		if n >= st.mul && n%st.mul == 0 {
			return strconv.FormatInt(n/st.mul, 10) + st.unit
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}

// shortKey renders "ssh-ed25519 AAAAC3…last4 comment" (ported from Keys.jsx).
func shortKey(line string) string {
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) >= 2 && len(parts[1]) > 12 {
		out := parts[0] + " " + parts[1][:8] + "…" + parts[1][len(parts[1])-4:]
		if len(parts) > 2 {
			out += " " + strings.Join(parts[2:], " ")
		}
		return out
	}
	return line
}

// settingsLineDiff is the client-side LCS line diff the history rows used
// (formerly Settings.jsx lineDiff) — a small O(n·m) diff over TOML revisions. Display
// only: an empty line list means "no textual change".
func settingsLineDiff(oldText, newText string) []SettingsDiffLine {
	if oldText == newText {
		return nil
	}
	a := splitLines(oldText)
	b := splitLines(newText)
	n, m := len(a), len(b)
	// LCS table (revisions are small; bounded by MaxRepoSettingsBytes/line).
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []SettingsDiffLine
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			out = append(out, SettingsDiffLine{" ", a[i]})
			i++
			j++
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			out = append(out, SettingsDiffLine{"-", a[i]})
			i++
		} else {
			out = append(out, SettingsDiffLine{"+", b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, SettingsDiffLine{"-", a[i]})
	}
	for ; j < m; j++ {
		out = append(out, SettingsDiffLine{"+", b[j]})
	}
	return out
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// settingsSplitList splits a comma/newline/space separated list.
func settingsSplitList(v string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// settingsAtoiOr parses an optional integer form value (blank = def).
func settingsAtoiOr(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return def
}

// settingsRoleKnown reports a role on the repo ladder (the bypass picker's
// vocabulary, 01 §4).
func settingsRoleKnown(role string) bool {
	for _, r := range settingsAccessRoles {
		if r == role {
			return true
		}
	}
	return false
}
