// Settings group (docs/go/12_web_ui.md §2, D-WEB-14): four pages —
//
//	GET  /setup                     configuration editor (works in setup-only mode)
//	GET  /{owner}/{name}/settings    repo settings, path-segment tabs
//	GET  /{owner}/settings           organisation settings
//	GET  /keys                       SSH public keys
//
// # WIRING (composition owns cmd/walhub/settings.go)
//
// The feature services are NOT on api.Env, and internal/server's setup
// helpers are unexported, so this file takes a narrow function seam:
//
//	webui.BindSettings(webui.SettingsDeps{
//	    // --- /setup: bind the SAME functions the JSON API uses (§3.4).
//	    SetupAccess:   func(r, p) *auth.AuthError { ... }        // server.setupAccess's rule:
//	                                                            // WALHUB_SETUP_TOKEN ?token=/Bearer,
//	                                                            // else open while boot.Mode == "defaults"
//	                                                            // || setup-only || auth.mode == "none",
//	                                                            // else requireAdmin.
//	    SetupView:     func(ctx) (*webui.SettingsSetupView, error)
//	                                                            // buildSchemaGroups(effective, config.Defaults())
//	                                                            // + boot file_state/errors; flatten the API's
//	                                                            // typed values to display strings: config.Duration
//	                                                            // -> its String() (the adapter MAY pre-format with
//	                                                            // the spec spelling; the page also formats),
//	                                                            // config.ByteSize -> int64 bytes.
//	    SetupValidate: func(ctx, values map[string]string) ([]webui.SettingsSetupError, error)
//	                                                            // POST /api/v1/setup/test: parseSetupBody over
//	                                                            // setupBaseConfig(), then config.Validate — nothing
//	                                                            // is written.
//	    SetupSave:     func(ctx, values map[string]string) (webui.SettingsSetupResult, []webui.SettingsSetupError, error)
//	                                                            // parseSetupBody over setupBaseConfig(), then
//	                                                            // config.Validate, then config.SaveSetup — EXACTLY
//	                                                            // setupPut's two steps (values are key -> raw string
//	                                                            // from the form; build {"overrides": …} from them).
//	    SetupAuthTest: func(ctx, issuer, clientID, redirectURI string) (*webui.SettingsSetupAuthTest, []webui.SettingsSetupError)
//	                                                            // the discovery probe from setupAuthTest.
//
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
//	d := webui.SettingsDeps{ /* setup* + org fields */ }
//	d.BindSettingsServices(webui.SettingsServices{
//	    API: apiEnv, Ident: c.ident, Checks: c.checksSvc, Notify: c.notifySvc,
//	    Mirror: c.mirrorHandler, Push: c.pushMirrorHandler, Now: time.Now,
//	})
//	webui.BindSettings(d)
//
// It fills Repo{General,Scheduled,Mirror,PushMirror,Policy,Config,Access,
// Admin,Write}. A nil service leaves its field nil and the affected panel keeps
// its degraded card, so setup-only mode (no store) still renders /setup.
//
// # SETUP-ONLY MODE (wired: see cmd/walhub/webui.go)
//
// Status: cmd/walhub/serve.go builds webui.New in EVERY boot mode and calls
// wireWebUI; internal/server/router.go routes GET and POST /setup through the
// page provider in mountSetupOnly (POST is how an invalid config is fixed).
// Only the setup* deps are bound in that mode; every other binder field is nil
// and renders its degraded card.
//
// The handler itself is mode-agnostic: it renders whatever SetupView reports
// (file_state "invalid" raises the banner) and never touches the store.
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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/server/auth"
)

// ---------------------------------------------------------------------------
// deps
// ---------------------------------------------------------------------------

// SettingsDeps is the settings group's service seam (see the WIRING header).
type SettingsDeps struct {
	SetupAccess   func(r *http.Request, p auth.Principal) *auth.AuthError
	SetupView     func(ctx context.Context) (*SettingsSetupView, error)
	SetupValidate func(ctx context.Context, values map[string]string) ([]SettingsSetupError, error)
	SetupSave     func(ctx context.Context, values map[string]string) (SettingsSetupResult, []SettingsSetupError, error)
	SetupAuthTest func(ctx context.Context, issuer, clientID, redirectURI string) (*SettingsSetupAuthTest, []SettingsSetupError)

	RepoRole       func(ctx context.Context, owner, repo string, p auth.Principal) (string, error)
	RepoGeneral    func(ctx context.Context, owner, repo string) (*SettingsGeneral, error)
	RepoScheduled  func(ctx context.Context, owner, repo string) (*SettingsScheduled, error)
	RepoMirror     func(ctx context.Context, owner, repo string) (*SettingsMirror, error)
	RepoPushMirror func(ctx context.Context, owner, repo string) (*SettingsPushMirror, error)
	RepoPolicy     func(ctx context.Context, owner, repo string) (*SettingsPolicy, error)
	RepoConfig     func(ctx context.Context, owner, repo string) (*SettingsConfig, error)
	RepoAccess     func(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAccess, error)
	RepoAdmin      func(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAdmin, error)
	RepoWrite      func(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError)

	OrgView  func(ctx context.Context, org string, p auth.Principal) (*SettingsOrgPage, error)
	OrgWrite func(ctx context.Context, org string, p auth.Principal, w SettingsOrgWrite) (SettingsWriteResult, *SettingsWriteError)
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
}

// settingsBound is the process-wide binding. server.go documents one page
// provider per process, so a package var is the seam; BindSettings runs once
// at composition time, before the first request.
var settingsBound SettingsDeps

// BindSettings installs the group's service seam (see the WIRING header).
func BindSettings(d SettingsDeps) { settingsBound = d }

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
// setup data
// ---------------------------------------------------------------------------

// SettingsSetupView is what the page renders: the schema groups plus the
// setup API's boot state (file_state / errors) and access verdict.
type SettingsSetupView struct {
	FileState string // "absent" | "valid" | "invalid"
	Access    string // "" (ok) | "denied" | "admin_required" | "token_required"
	Detail    string
	Errors    []SettingsSetupError
	Sections  []SettingsSetupSection
	AuthMode  string
	PublicURL string
	Issuer    string
	ClientID  string
}

// SettingsSetupSection is one schema card.
type SettingsSetupSection struct {
	Section   string
	Essential []SettingsSetupField
	Advanced  []SettingsSetupField
}

// SettingsSetupField is one editable key row.
type SettingsSetupField struct {
	Key      string
	Type     string // string|int|float|bool|duration|size|list|enum|toml|url|listen|path
	Value    string
	Default  string
	Example  string
	Note     string
	Enum     []string
	Secret   bool
	Advanced bool
	FromFile bool
	Errors   []SettingsSetupError

	// Derived for the template (computed once, never in the template).
	Control string
	InputID string
	Bool    bool
}

// SettingsSetupError is one server-side validation error.
type SettingsSetupError struct {
	Key     string
	Message string
	Value   string
}

// SettingsSetupResult is a successful save.
type SettingsSetupResult struct {
	Saved           bool
	RequiresRestart []string
}

// SettingsSetupAuthTest is a successful OIDC discovery probe.
type SettingsSetupAuthTest struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
	ClientIDPresent       bool
}

// SettingsSetupPage is the /setup page model.
type SettingsSetupPage struct {
	View            SettingsSetupView
	RedirectURI     string
	Issuer          string
	ClientID        string
	Denied          bool
	Detail          string
	Token           string
	Saved           bool
	RequiresRestart []string
	ValidateOK      bool
	AuthTest        *SettingsSetupAuthTest
	AuthErrors      []SettingsSetupError
	Notice          string
	Error           string
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
	Owner      string
	Name       string
	Tab        string
	Role       string
	Admin      bool
	Saved      bool
	Notice     string
	Error      string
	Nav        []SettingsNavItem
	Danger     []SettingsNavItem
	General    *SettingsGeneral
	Scheduled  *SettingsScheduled
	Mirror     *SettingsMirror
	Push       *SettingsPushMirror
	Policy     *SettingsPolicy
	Config     *SettingsConfig
	Access     *SettingsAccess
	AdminData  *SettingsAdmin
	DangerData *SettingsDanger
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
	Description  string
	MaxLength    int
	Features     []SettingsFeatureRow
	SelfApproval bool
	Note         string
	Error        string
}

// SettingsStrategy is one bundle strategy row.
type SettingsStrategy struct {
	Name, Kind, Base, Schedule, ScheduleHuman, Next, Keep, Filter, Refs string
}

// SettingsKV is one key/value row.
type SettingsKV struct {
	Key, Value string
}

// SettingsScheduled is the Scheduled tasks tab.
type SettingsScheduled struct {
	Strategies   []SettingsStrategy
	HostFacts    []SettingsKV
	UpstreamGit  string
	FollowLine   string
	LastRound    string
	Effective    []SettingsKV
	EffectiveSrc []string
	Error        string
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
	DryRunN     int
	DryRunDone  bool
	DryRunAllow int
	DryRunDeny  int
	DryRunRefs  []SettingsKV
	Note        string
	Error       string
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

// SettingsAccess is the Access tab.
type SettingsAccess struct {
	Role          string
	Collaborators []SettingsCollaborator
	Visibility    string
	IsOrg         bool
	Bindings      []SettingsBinding
	Roles         []string
	Teams         []SettingsOption
	Version       uint64
	Note          string
	Error         string
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
}

// SettingsDelivery is one recent delivery row (the bounded per-hook ring).
type SettingsDelivery struct {
	Seq    string
	Event  string
	Status int
	At     string
	Error  string
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
	// Draft values re-rendered after a rejected create (never secrets).
	NewName   string
	NewURL    string
	NewEvents string
}

// SettingsDanger is the Danger Zone tab.
type SettingsDanger struct {
	FullName string // typed confirmation target
	IsMirror bool
	Error    string
}

// SettingsRepoWrite is one settings mutation. Action selects the operation; a
// nil dep renders the panels as "not wired".
type SettingsRepoWrite struct {
	Action string
	Values map[string]string
	File   []byte
}

// Get returns a form value ("" when absent).
func (w SettingsRepoWrite) Get(key string) string { return w.Values[key] }

// Write actions (the seam's vocabulary).
const (
	SettingsActionGeneral        = "general"
	SettingsActionAccess         = "access"
	SettingsActionPolicy         = "policy"
	SettingsActionPolicyDryRun   = "policy-dryrun"
	SettingsActionConfig         = "config"
	SettingsActionConfigClear    = "config-clear"
	SettingsActionMirrorPut      = "mirror-put"
	SettingsActionMirrorSchedule = "mirror-schedule"
	SettingsActionMirrorSync     = "mirror-sync"
	SettingsActionMirrorRemove   = "mirror-remove"
	SettingsActionPushPut        = "pushmirror-put"
	SettingsActionPushSchedule   = "pushmirror-schedule"
	SettingsActionPushKeygen     = "pushmirror-keygen"
	SettingsActionPushSync       = "pushmirror-sync"
	SettingsActionPushRemove     = "pushmirror-remove"
	SettingsActionTokenCreate    = "token-create"
	SettingsActionTokenRevoke    = "token-revoke"
	SettingsActionWebhookCreate  = "webhook-create"
	SettingsActionWebhookDelete  = "webhook-delete"
	SettingsActionWebhookPing    = "webhook-ping"
	SettingsActionTransfer       = "transfer"
	SettingsActionDelete         = "delete"
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
	Slug, Name string
	Members    []string
}

// SettingsOrgInvite is one invitation row.
type SettingsOrgInvite struct {
	ID, Subject, Role, InvitedBy, ExpiresAt string
	Expired                                 bool
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
	SettingsOrgProfileSave = "profile"
	SettingsOrgAvatar      = "avatar"
	SettingsOrgAvatarRm    = "avatar-remove"
	SettingsOrgMemberSet   = "member"
	SettingsOrgMemberRm    = "member-remove"
	SettingsOrgTeamSave    = "team"
	SettingsOrgTeamRm      = "team-remove"
	SettingsOrgTeamAdd     = "team-add"
	SettingsOrgTeamDel     = "team-del"
	SettingsOrgInviteMake  = "invite"
	SettingsOrgInviteRm    = "invite-remove"
	SettingsOrgDelete      = "delete"
)

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

// settingsRepoTabs is the sidebar order (lib/settingsNav.js).
var settingsRepoTabs = []settingsTab{
	{ID: "general", Label: "General", MinRead: "read"},
	{ID: "scheduled", Label: "Scheduled tasks", MinRead: "read"},
	{ID: "mirror", Label: "Mirror", MinRead: "read"},
	{ID: "policy", Label: "Push policy", MinRead: "admin"},
	{ID: "config", Label: "Effective config & history", MinRead: "admin"},
	{ID: "access", Label: "Access", MinRead: "triage"},
	{ID: "tokens", Label: "CI tokens", MinRead: "admin"},
	{ID: "webhooks", Label: "Webhooks", MinRead: "admin"},
	{ID: "wal", Label: "WAL", MinRead: "read"},
	{ID: "danger", Label: "Danger Zone", Danger: true, MinRead: "admin"},
}

// settingsOrgTabs is the org tab order (Org.jsx: Profile/Members/Teams/
// Invitations always, Activity/Danger only for owners).
var settingsOrgTabs = []settingsTab{
	{ID: "profile", Label: "Profile"},
	{ID: "members", Label: "Members"},
	{ID: "teams", Label: "Teams"},
	{ID: "invitations", Label: "Invitations"},
	{ID: "activity", Label: "Activity", MinRead: "admin"},
	{ID: "danger", Label: "Danger", Danger: true, MinRead: "admin"},
}

// settingsRoleRank orders the repo roles (identity.Role spellings).
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
	case "none":
		return 0
	default:
		return 5 // unwired/unknown: no page-level denial (the server gate already ran)
	}
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

	m.HandleFunc("GET /setup", s.settingsSetupPage)
	m.HandleFunc("POST /setup", s.settingsSetupSubmit)
	m.HandleFunc("POST /setup/auth-test", s.settingsSetupAuthTest)

	m.HandleFunc("GET /keys", s.settingsKeysPage)
	m.HandleFunc("POST /keys", s.settingsKeyAdd)
	m.HandleFunc("POST /keys/{id}/delete", s.settingsKeyDelete)

	// Repo settings: path-segment tabs (see the ROUTE SHAPE note).
	m.HandleFunc("GET /{owner}/{name}/settings", s.settingsRepoRedirect)
	m.HandleFunc("GET /{owner}/{name}/settings/{tab}", s.settingsRepoTab)
	m.HandleFunc("POST /{owner}/{name}/settings/{tab}", s.settingsRepoWrite)
	m.HandleFunc("POST /{owner}/{name}/settings/{tab}/{id}", s.settingsRepoWrite)

	// Org settings: ?tab= (see the ROUTE SHAPE note).
	m.HandleFunc("GET /{owner}/settings", s.settingsOrgPage)
	m.HandleFunc("POST /{owner}/settings", s.settingsOrgSubmit)
	m.HandleFunc("POST /{owner}/settings/preview", s.settingsOrgPreview)
}

// ---------------------------------------------------------------------------
// /setup
// ---------------------------------------------------------------------------

// settingsSetupPage answers GET /setup. Access is the §3.4 rule, delegated to
// the binding; a refusal still renders the page (the restricted card), exactly
// like the SPA — only writes get a bare 403.
func (s *Server) settingsSetupPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := settingsDeps()
	page := &SettingsSetupPage{Token: r.URL.Query().Get("token")}
	if d.SetupAccess != nil {
		if aerr := d.SetupAccess(r, p); aerr != nil {
			page.Denied = true
			page.Detail = aerr.Why
			s.page(w, r, p, http.StatusOK, "settings/setup", "Setup", page)
			return
		}
	} else if s.authMode() != "none" && !p.Admin {
		page.Denied = true
		page.Detail = "this host requires an admin principal or a setup token to view or change configuration"
	}
	if page.Denied {
		s.page(w, r, p, http.StatusOK, "settings/setup", "Setup", page)
		return
	}
	if d.SetupView == nil {
		page.Error = "configuration schema is not wired on this build"
		s.page(w, r, p, http.StatusOK, "settings/setup", "Setup", page)
		return
	}
	view, err := d.SetupView(r.Context())
	if err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "configuration schema unavailable: "+err.Error())
		return
	}
	if view == nil {
		view = &SettingsSetupView{}
	}
	settingsPrepareSetup(view)
	page.View = *view
	page.RedirectURI = settingsRedirectURI(r, view.PublicURL)
	page.Issuer = view.Issuer
	page.ClientID = view.ClientID
	if view.Access == "denied" || view.Access == "admin_required" || view.Access == "token_required" {
		page.Denied = true
		page.Detail = view.Detail
	}
	q := r.URL.Query()
	page.Saved = q.Get("saved") == "1"
	if rq := q.Get("restart"); rq != "" {
		page.RequiresRestart = strings.Split(rq, ",")
	}
	if q.Get("validated") == "1" {
		page.ValidateOK = true
	}
	s.page(w, r, p, http.StatusOK, "settings/setup", "Setup", page)
}

// settingsSetupSubmit answers POST /setup: op=validate runs the full validator
// without saving; op=save validates THEN writes through the same functions the
// JSON API uses (the binding owns that two-step — this handler never coerces).
func (s *Server) settingsSetupSubmit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := settingsDeps()
	if d.SetupAccess != nil {
		if aerr := d.SetupAccess(r, p); aerr != nil {
			s.errorPage(w, r, authErrorStatus(aerr), aerr.Why)
			return
		}
	} else if s.authMode() != "none" && !p.Admin {
		s.errorPage(w, r, http.StatusForbidden, "setup requires an admin principal or a setup token")
		return
	}
	if d.SetupView == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "setup is not wired on this build")
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
	if op == "validate" && d.SetupValidate == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "setup validation is not wired on this build")
		return
	}
	if op != "validate" && d.SetupSave == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "setup save is not wired on this build")
		return
	}
	token := r.PostFormValue("token")
	qs := ""
	if token != "" {
		qs = "?token=" + url.QueryEscape(token)
	}
	// Validate is the API's POST /test; save is the API's PUT — the binding
	// calls the same coercion + config.Validate for both (never duplicated
	// here). Save validates before it writes, inside the binding.
	if op == "validate" {
		if d.SetupValidate == nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "setup validation is not wired on this build")
			return
		}
		errs, err := d.SetupValidate(r.Context(), values)
		if err != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "validation failed: "+err.Error())
			return
		}
		if len(errs) > 0 {
			s.settingsSetupReject(w, r, p, d, token, values, errs, "validation failed — nothing was saved")
			return
		}
		http.Redirect(w, r, "/setup"+settingsJoinQuery(qs, "validated=1"), http.StatusSeeOther)
		return
	}
	result, errs, err := d.SetupSave(r.Context(), values)
	if err != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "save failed: "+err.Error())
		return
	}
	if len(errs) > 0 {
		s.settingsSetupReject(w, r, p, d, token, values, errs, "validation failed — nothing was saved")
		return
	}
	http.Redirect(w, r, "/setup"+settingsJoinQuery(qs, "saved=1&restart="+url.QueryEscape(strings.Join(result.RequiresRestart, ","))), http.StatusSeeOther)
}

// settingsSetupReject re-renders /setup with the typed values and the
// server's per-key errors (422; 200 for htmx).
func (s *Server) settingsSetupReject(w http.ResponseWriter, r *http.Request, p auth.Principal, d SettingsDeps, token string, values map[string]string, errs []SettingsSetupError, msg string) {
	view, verr := d.SetupView(r.Context())
	if verr != nil || view == nil {
		view = &SettingsSetupView{}
	}
	settingsPrepareSetup(view)
	view.Errors = errs
	settingsApplySetupValues(view, values, errs)
	page := &SettingsSetupPage{
		View: *view, Token: token, Error: msg,
		RedirectURI: settingsRedirectURI(r, view.PublicURL),
		Issuer:      view.Issuer, ClientID: view.ClientID,
	}
	status := http.StatusUnprocessableEntity
	if settingsHTMX(r) {
		status = http.StatusOK // htmx swaps only 2xx bodies
	}
	s.page(w, r, p, status, "settings/setup", "Setup", page)
}

// settingsRedirectURI is the OIDC redirect URI to register at the issuer: the
// configured public URL when set, else the request's origin (§8.6).
func settingsRedirectURI(r *http.Request, publicURL string) string {
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

// settingsJoinQuery appends a query fragment to an existing "?token=…" (or
// starts the query). Kept tiny so the token survives every hop (§1 "token").
func settingsJoinQuery(existing, add string) string {
	if existing == "" {
		return "?" + add
	}
	return existing + "&" + add
}

// settingsSetupAuthTest answers POST /setup/auth-test: an htmx fragment (the
// discovery probe), never JSON.
func (s *Server) settingsSetupAuthTest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	d := settingsDeps()
	if d.SetupAccess != nil {
		if aerr := d.SetupAccess(r, p); aerr != nil {
			s.errorPage(w, r, authErrorStatus(aerr), aerr.Why)
			return
		}
	}
	if d.SetupAuthTest == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "OIDC discovery test is not wired on this build")
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
		return
	}
	_ = r.ParseForm()
	res, errs := d.SetupAuthTest(r.Context(),
		strings.TrimSpace(r.PostFormValue("issuer")),
		strings.TrimSpace(r.PostFormValue("client_id")),
		strings.TrimSpace(r.PostFormValue("redirect_uri")))
	s.settingsRenderFragment(w, r, "settings/setup", "settings-setup-authtest", struct {
		Result *SettingsSetupAuthTest
		Errors []SettingsSetupError
	}{res, errs})
}

// settingsPrepareSetup fills the per-field derived fields (input id, control
// kind, boolean value) so templates never do presentation logic.
func settingsPrepareSetup(v *SettingsSetupView) {
	for i := range v.Sections {
		for j := range v.Sections[i].Essential {
			settingsPrepareField(&v.Sections[i].Essential[j])
		}
		for j := range v.Sections[i].Advanced {
			settingsPrepareField(&v.Sections[i].Advanced[j])
		}
	}
}

// settingsPrepareField derives the template-facing fields of one row.
func settingsPrepareField(f *SettingsSetupField) {
	f.InputID = "setup-in-" + strings.ReplaceAll(f.Key, ".", "--")
	f.Bool = strings.EqualFold(strings.TrimSpace(f.Value), "true") || strings.TrimSpace(f.Value) == "1"
	if meta, ok := settingsFieldMeta[f.Key]; ok {
		if f.Example == "" {
			f.Example = meta.Example
		}
		if f.Note == "" {
			f.Note = meta.Note
		}
		if !f.Advanced {
			f.Advanced = meta.Advanced
		}
		if len(f.Enum) == 0 {
			f.Enum = meta.Enum
		}
		if (f.Type == "" || f.Type == "string") && meta.Type != "" {
			f.Type = meta.Type
		}
	}
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
	f.FromFile = f.Value != f.Default
}

// settingsApplySetupValues re-renders a failed submit with the operator's typed
// values (never the saved ones) and the per-key error lines.
func settingsApplySetupValues(v *SettingsSetupView, values map[string]string, errs []SettingsSetupError) {
	apply := func(f *SettingsSetupField) {
		f.Errors = nil
		if sval, ok := values[f.Key]; ok {
			f.Value = sval
		}
		for _, e := range errs {
			if e.Key == f.Key || (e.Key == "" && len(errs) == 1) {
				f.Errors = append(f.Errors, e)
			}
		}
		settingsPrepareField(f)
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

// settingsFieldMeta is the display metadata the schema API does not carry
// (the `ex`/`note`/`advanced` columns of the former setup.js FIELDS table; internal/setup is the schema authority). It is
// data, not validation: the Go validator stays authoritative. The full table
// belongs in internal/setup when §8.2 lands; this covers the keys whose
// absence makes the form unhelpful.
type settingsFieldMetaT struct {
	Type     string
	Example  string
	Note     string
	Advanced bool
	Enum     []string
}

var settingsFieldMeta = map[string]settingsFieldMetaT{
	"server.listen":              {Example: "0.0.0.0:8080", Note: "127.0.0.1:8080 = loopback only"},
	"server.http2":               {Type: "bool", Example: "true", Advanced: true},
	"server.request_timeout":     {Type: "duration", Example: "1h", Advanced: true},
	"server.drain_timeout":       {Type: "duration", Example: "30s", Advanced: true},
	"server.max_push_bytes":      {Type: "size", Example: "2GiB"},
	"server.roles":               {Type: "list", Example: "serve, maintain"},
	"server.auto_create_on_push": {Type: "bool", Example: "true"},
	"server.public_url":          {Type: "url", Example: "https://git.example.com"},
	"server.ssh.listen":          {Example: "0.0.0.0:2222", Note: "empty = the SSH transport is disabled"},
	"server.ssh.external_port":   {Type: "int", Example: "12222", Note: "advertised in ssh:// clone URLs"},
	"server.auth.mode":           {Type: "enum", Enum: []string{"none", "token", "oidc"}, Example: "token", Note: "oidc additionally needs issuer, an allowlist, and the browser-login trio"},
	"server.auth.anonymous_read": {Type: "bool", Example: "true", Note: "oidc: true (public browsing) recommended"},
	"server.auth.issuer":         {Type: "url", Example: "https://id.example.com", Note: "discovery at <issuer>/.well-known/openid-configuration"},
	"server.auth.session_secret": {Example: "openssl rand -hex 32", Note: "any random ≥ 32 bytes; rotating revokes sessions"},
	"store.backend":              {Type: "enum", Enum: []string{"s3", "gcs", "memory", "filesystem"}, Example: "filesystem"},
	"store.bucket":               {Example: "walhub-test", Note: "s3/gcs only"},
	"store.prefix":               {Example: "walhub/"},
	"store.root":                 {Example: "/var/lib/walhub/store", Note: "filesystem backend only"},
	"store.multipart_threshold":  {Type: "size", Example: "64MiB", Advanced: true},
	"store.multipart_part_size":  {Type: "size", Example: "16MiB", Advanced: true, Note: "must be ≤ multipart_threshold"},
	"store.s3.endpoint":          {Example: "http://127.0.0.1:9000", Note: "rustfs/MinIO"},
	"store.s3.force_path_style":  {Type: "bool", Example: "true", Note: "true for rustfs/MinIO"},
	"cache.dir":                  {Example: "/var/cache/walhub"},
	"cache.mode":                 {Type: "enum", Enum: []string{"budget", "disk", "auto"}, Example: "budget"},
	"cache.max_bytes":            {Type: "size", Example: "20GiB", Note: "everything on disk must fit this in budget mode"},
	"wal.freshness_ttl":          {Type: "duration", Example: "30s"},
	"maintenance.interval":       {Type: "duration", Example: "5m", Advanced: true},
	"telemetry.log_format":       {Example: "json", Advanced: true},
	"telemetry.metrics_listen":   {Example: "127.0.0.1:9090", Advanced: true},
	"events.webhook_url":         {Type: "url", Example: "https://example.com/hook", Advanced: true},
	"lfs.enabled":                {Type: "bool", Example: "true"},
	"git.max_pack_bytes":         {Type: "size", Example: "2GiB", Advanced: true},
	"upstream.allow_import":      {Type: "bool", Example: "true"},
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

func (s *Server) settingsKeysPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	page := &SettingsKeysPage{}
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
		page.Error = "key registry unavailable: " + err.Error()
	}
	page.Keys = settingsKeyRows(keys)
	s.page(w, r, p, http.StatusOK, "settings/keys", "SSH keys", page)
}

// settingsKeyAdd answers POST /keys: the registry owns validation; the page
// renders its error verbatim (no duplicated shape check).
func (s *Server) settingsKeyAdd(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
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

// settingsKeyDelete answers POST /keys/{id}/delete.
func (s *Server) settingsKeyDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin write refused")
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
	d := settingsDeps()
	role := "admin" // unwired identity: the server's read gate already ran
	if d.RepoRole != nil {
		got, err := d.RepoRole(r.Context(), owner, name, p)
		if err != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "access check failed: "+err.Error())
			return p, "", false
		}
		role = got
	}
	if settingsRoleRank(role) == 0 {
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return p, "", false
	}
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
	s.page(w, r, p, http.StatusOK, "settings/repo", "Settings", page)
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
	switch tab {
	case "general":
		if d.RepoGeneral != nil {
			if v, err := d.RepoGeneral(ctx, owner, name); err != nil {
				page.Error = err.Error()
			} else if v != nil {
				if v.MaxLength == 0 {
					v.MaxLength = 512
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
				page.Access = v
			}
		}
	case "tokens", "webhooks":
		if d.RepoAdmin != nil {
			if v, err := d.RepoAdmin(ctx, owner, name, p); err != nil {
				page.Error = err.Error()
			} else {
				page.AdminData = v
			}
		}
	case "danger":
		page.DangerData = &SettingsDanger{FullName: owner + "/" + name}
	case "wal":
		// GAP: the WAL panel is the repo group's shared partial
		// (specs/settings.md §2.9 / §8.4); until it lands this tab renders
		// the placeholder card with the data hook the islands group expects.
	}
	return page
}

// settingsNormalizeFeatures fills the six feature rows (labels/hints ported
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
	if tab == "policy" && r.PostFormValue("policy_action") == "dryrun" {
		action = SettingsActionPolicyDryRun
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
	if tab == "webhooks" {
		action = SettingsActionWebhookCreate
		if id := r.PathValue("id"); id != "" {
			action = SettingsActionWebhookDelete
			if r.PostFormValue("webhook_action") == "ping" {
				action = SettingsActionWebhookPing
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
	// Every repo settings mutation is admin-gated (§5 write matrix).
	if !settingsRoleAtLeast(role, "admin") {
		s.errorPage(w, r, http.StatusForbidden, "saving repository settings requires the admin role")
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
	if action == SettingsActionTransfer || action == SettingsActionDelete {
		if r.PostFormValue("confirm") != owner+"/"+name {
			page := s.settingsRepoPage(r, p, owner, name, "danger", role, true)
			page.DangerData.Error = "type " + owner + "/" + name + " exactly to confirm"
			s.settingsRepoFinish(w, r, p, page, nil)
			return
		}
	}
	wr := SettingsRepoWrite{Action: action, Values: settingsFormValues(r, "")}
	if id := r.PathValue("id"); id != "" {
		wr.Values["id"] = id
	}
	d := settingsDeps()
	if d.RepoWrite == nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Error = "this settings write is not wired on this build"
		s.page(w, r, p, http.StatusServiceUnavailable, "settings/repo", "Settings", page)
		return
	}
	werrRes, werr := d.RepoWrite(r.Context(), owner, name, p, wr)
	if werr != nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Error = werr.Message
		settingsRestoreRepoValues(page, r, action)
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
		s.page(w, r, p, http.StatusOK, "settings/repo", "Settings", page)
		return
	}
	// A dry run returns its verdict with the typed document: render it in
	// place (the stored policy is not what was evaluated).
	if werrRes.Policy != nil {
		page := s.settingsRepoPage(r, p, owner, name, tab, role, true)
		page.Policy = werrRes.Policy
		page.Notice = werrRes.Notice
		s.page(w, r, p, http.StatusOK, "settings/repo", "Settings", page)
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
	status := http.StatusUnprocessableEntity
	if werr != nil {
		status = werr.status()
	}
	if settingsHTMX(r) {
		status = http.StatusOK
	}
	s.page(w, r, p, status, "settings/repo", "Settings", page)
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
	case "config":
		return SettingsActionConfig
	}
	return tab
}

// settingsRestoreRepoValues puts the submitted values back on the page after a
// rejected write, so the operator never loses the edit.
func settingsRestoreRepoValues(page *SettingsRepoPage, r *http.Request, action string) {
	get := func(k string) string { return r.PostFormValue(k) }
	switch action {
	case SettingsActionGeneral:
		if page.General == nil {
			page.General = &SettingsGeneral{}
		}
		page.General.Description = get("description")
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
		if v := get("visibility"); v != "" {
			page.Access.Visibility = v
		}
	case SettingsActionPolicy:
		if page.Policy == nil {
			page.Policy = &SettingsPolicy{}
		}
		page.Policy.Text = get("policy")
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
	case SettingsActionWebhookCreate:
		if page.AdminData == nil {
			page.AdminData = &SettingsAdmin{}
		}
		page.AdminData.NewURL = get("url")
		page.AdminData.NewEvents = get("events")
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
	if d.OrgView == nil || d.OrgWrite == nil {
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
	tab := settingsOrgTabForAction(action)
	res, werr := d.OrgWrite(r.Context(), org, p, wr)
	if werr != nil {
		view.Tab = tab
		view.Nav = settingsOrgNav(org, tab, true)
		view.Error = werr.Message
		status := werr.status()
		if settingsHTMX(r) {
			status = http.StatusOK
		}
		s.page(w, r, p, status, "settings/org", "Organization settings", view)
		return
	}
	if res.Link != "" {
		// The shareable invite link is a one-shot response (it is a bearer
		// token): render it, never put it in the redirect URL.
		view.Tab = tab
		view.Nav = settingsOrgNav(org, tab, true)
		view.InviteLink = res.Link
		view.Notice = res.Notice
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
	case SettingsOrgTeamSave, SettingsOrgTeamRm, SettingsOrgTeamAdd, SettingsOrgTeamDel:
		return "teams"
	case SettingsOrgInviteMake, SettingsOrgInviteRm:
		return "invitations"
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
// for the two htmx fragments this group owns (OIDC discovery, bio preview is
// written directly). The set already carries base + every partial.
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
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
