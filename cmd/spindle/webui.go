// webui.go — D-WEB-14 composition: the ONE place the server-rendered page layer
// (internal/webui) sees its feature services (docs/go/12_web_ui.md §2).
//
// Each page group declares a narrow function seam (PublicDeps, RepoDeps,
// CollabDeps, FormsDeps, SettingsDeps) and the page layer never imports a
// feature package upward (law 8). This file is the inverse of that rule: it
// binds the seams verbatim to the services buildCollab (09 §4 touch point 3)
// assembled, so a page and its JSON twin read through the same code path.
//
// Called ONCE per process from serveHTTP, before the server accepts requests.
// Both boot modes call it:
//   - normal/defaults: every service exists and the full binding applies;
//   - setup-only (§3.4, invalid config): there is no store, so only the
//     Settings setup* deps bind. Every other field stays nil — the affected
//     surface renders degraded (a "not wired" card) and never panics. That is
//     what lets /setup render when nothing else can.
//
// A field left nil is documented inline with the reason it cannot be bound
// today (a missing exported read, a seam that omits ctx/principal, or a write
// adapter that is its own composition step).
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/webui"
)

// repoProbeTimeout bounds one repo-chrome projection. RepoDeps deliberately
// carries no context (the page chrome treats them as cheap projections), but
// the api.Env hooks behind them do store I/O, so the adapter gives each probe
// its own deadline instead of an unbounded Background read
// (13_concurrency.md: no store call without a deadline).
// Report item: RepoDeps should grow ctx (foundation owns repo.go).
const repoProbeTimeout = 10 * time.Second

// Page-size limits for the org settings read (the API's own window sizes).
const (
	orgTeamLimit     = 100
	orgInviteLimit   = 100
	orgActivityLimit = 30
)

// wireWebUI binds every page group's service seam. c is nil in setup-only
// mode (no store → no collaboration layer); apiEnv is nil there too.
func wireWebUI(cfg *config.Config, boot server.BootState, apiEnv *api.Env, c *collabWiring) {
	wirePublicPages(c, apiEnv)
	wireRepoPages(c, apiEnv)
	wireCollabPages(apiEnv, c)
	wireFormPages(c)
	wireSettingsPages(cfg, boot, apiEnv, c)
}

// ---------------------------------------------------------------------------
// public: landing/how-it-works/api docs/owner pages/invitations/notifications
// ---------------------------------------------------------------------------

func wirePublicPages(c *collabWiring, apiEnv *api.Env) {
	d := webui.PublicDeps{}
	if c != nil && c.ident != nil {
		ident := c.ident
		d.Org = ident.GetOrg
		d.MemberOrgs = ident.MemberOrgs
		d.MyInvites = ident.MyInvitesFor // Parity: the owner page's avatar verbs and profile edit (D-WEB-14).
		d.RegenerateAvatar = ident.RegenerateUserAvatar
		d.RemoveAvatar = ident.DeleteUserAvatar
		if apiEnv != nil && apiEnv.Store != nil {
			d.ProfileSave = func(ctx context.Context, owner string, in webui.OwnerProfileInput) error {
				_, err := api.PutOwnerProfile(ctx, apiEnv.Store, owner,
					in.DisplayName, in.Location, in.Timezone, in.BioMarkdown,
					time.Now().UTC().Format("2006-01-02T15:04:05Z"))
				return err
			}
		}

		d.PreviewInvite = ident.PreviewInviteFor
		d.AcceptInvite = ident.AcceptInviteFor
		d.CancelInvite = func(ctx context.Context, principal, id string) (*identity.Invitation, error) {
			return ident.CancelInvite(ctx, principal, id)
		}
	}
	if c != nil && c.notifySvc != nil {
		notes := c.notifySvc
		d.Tray = notes.Tray
		d.UnreadCount = notes.UnreadCount
		d.ReadAll = notes.ReadAll
		d.SetNotificationState = notes.SetState
	}
	// StarCount stays nil: social.Counts takes a principal the seam does not
	// carry and costs one store read PER LISTING ROW (the header marks it
	// optional — rows render without a count, +0 round trips).
	//
	// ExtraEndpoints stays nil: api.RegisterExposed has no exported accessor
	// (api.discoveryEndpoints is package-private), so the /api page's live
	// list carries the core route table only. Report item: export the extra
	// templates (or an accessor) from internal/api.
	webui.BindPublic(d)
}

// ---------------------------------------------------------------------------
// repo: tree/blob/commits/commit/wal + shared repo chrome
// ---------------------------------------------------------------------------

func wireRepoPages(c *collabWiring, apiEnv *api.Env) {
	d := webui.RepoDeps{}
	if apiEnv != nil {
		// The api.Env hooks carry ctx; each adapter below bounds its probe
		// (see repoProbeTimeout) and keeps the typed projection.
		if visibility := apiEnv.RepoVisibility; visibility != nil {
			d.Visibility = func(owner, repo string) (string, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return visibility(ctx, owner, repo)
			}
		}
		if collabCounts := apiEnv.CollabCounts; collabCounts != nil {
			d.CollabCounts = func(owner, repo string) (api.CollabCounts, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return collabCounts(ctx, owner, repo)
			}
		}
		if forkInfo := apiEnv.ForkInfo; forkInfo != nil {
			d.ForkInfo = func(owner, repo string) (api.ForkSummary, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return forkInfo(ctx, owner, repo)
			}
		}
		if checksSummary := apiEnv.ChecksSummary; checksSummary != nil {
			d.ChecksSummary = func(owner, repo string) (api.ChecksSummary, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return checksSummary(ctx, owner, repo)
			}
		}
		if mirrorSummary := apiEnv.MirrorSummary; mirrorSummary != nil {
			d.Mirror = func(owner, repo string) (api.MirrorView, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return mirrorSummary(ctx, owner, repo)
			}
		}
		if pushMirrorSummary := apiEnv.PushMirrorSummary; pushMirrorSummary != nil {
			d.PushMirror = func(owner, repo string) (api.PushMirrorView, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				return pushMirrorSummary(ctx, owner, repo)
			}
		}
		// Placeholder: the #210 sidecar (meta/placeholder.json) is an exported
		// key + doc shape, so this probe is real — one exact-key GET, and only
		// for empty repos (repo.go gates on c.Empty).
		if st := apiEnv.Store; st != nil {
			d.Placeholder = func(owner, repo string) (webui.RepoPlaceholder, bool) {
				ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
				defer cancel()
				id := git.RepoId{Owner: owner, Name: repo}
				body, _, err := store.GetBytes(ctx, st, id.StorePrefix()+api.PlaceholderKeySuffix, store.GetOptions{})
				if err != nil || body == nil {
					return webui.RepoPlaceholder{}, false
				}
				var doc api.PlaceholderDoc
				if json.Unmarshal(body, &doc) != nil {
					return webui.RepoPlaceholder{}, false
				}
				ph := webui.RepoPlaceholder{CreatedBy: doc.CreatedBy, CreatedAt: doc.CreatedAt}
				if doc.ExpiresAt != nil {
					ph.ExpiresAt = *doc.ExpiresAt
				}
				return ph, true
			}
		}
	}
	// Fsck stays nil: the only source is the api overview projection
	// (api.Env.Repo.Overview — a full manifest/packs/plan read, too heavy for a
	// degraded banner) and RepoDeps.Fsck carries no ctx to bound it. The
	// overview JSON twin still carries fsck. Report item: a lightweight
	// FsckInfo hook on api.Env + ctx in RepoDeps.
	// Parity seams (D-WEB-14): star/watch, create tag, per-row check pills.
	if c != nil {
		if c.socialSvc != nil {
			d.SocialCounts = c.socialSvc.Counts
			d.SocialViewer = c.socialSvc.ViewerState
			d.Star = c.socialSvc.Star
			d.Unstar = c.socialSvc.Unstar
		}
		if c.notifySvc != nil {
			d.WatchSet = c.notifySvc.SetWatch
		}
		if c.tagsSvc != nil {
			d.Tags = c.tagsSvc.CreateTag
		}
		if c.ident != nil {
			d.RoleOf = func(ctx context.Context, owner, repo string, p auth.Principal) identity.Role {
				role, _ := c.ident.Resolve(ctx, owner, repo, p)
				return role
			}
		}
		if c.checksSvc != nil {
			d.ChecksIndex = c.checksSvc.ListChecks
		}
	}
	webui.BindRepo(d)
}

// ---------------------------------------------------------------------------
// collab: issues/pulls/reviews/checks/releases reads + their page writes
// ---------------------------------------------------------------------------

func wireCollabPages(apiEnv *api.Env, c *collabWiring) {
	d := webui.CollabDeps{}
	if c != nil {
		if ident := c.ident; ident != nil {
			// The page wants the role only; the AccessDoc is the service's
			// own accounting (spec §6 F1).
			d.RoleOf = func(ctx context.Context, owner, repo string, p auth.Principal) identity.Role {
				role, _ := ident.Resolve(ctx, owner, repo, p)
				return role
			}
		}
		if iss := c.issuesSvc; iss != nil {
			d.IssuesList = iss.ListIssues
			d.IssuesGet = iss.GetThread
			d.IssueComment = iss.AddComment
			d.IssuePatch = iss.PatchIssue
			d.IssueReact = iss.AddReaction
			d.IssueUnreact = iss.RemoveReaction
			// The header's GAP is closed: internal/issues now exports both
			// reads (store.go ListLabels/ListMilestones).
			d.Labels = iss.ListLabels
			d.Milestones = iss.ListMilestones
		}
		if pl := c.pullsSvc; pl != nil {
			d.PullsList = pl.ListPRs
			d.PullGet = pl.GetPR
			d.PullComment = pl.AddComment
			d.PullUpdate = pl.UpdatePR
			d.PullCommits = pl.Commits
			d.PullDiff = pl.Diff
			d.ForksList = pl.ListForks
			d.MergeStart = pl.StartMerge
			d.MergeTask = pl.MergeTask
			d.UpdateBranch = pl.UpdateBranch
		}
		if rv := c.reviewSvc; rv != nil {
			d.ReviewsList = rv.ListReviews
			d.ReviewSubmit = rv.SubmitReview
			d.ReviewDismiss = rv.DismissReview
			d.ThreadsList = rv.ListThreads
			d.ThreadGet = rv.GetThread
			d.ThreadOpen = rv.OpenThread
			d.ThreadComment = rv.AddThreadComment
			d.ThreadResolve = rv.ResolveThread
			d.ThreadUnresolve = rv.UnresolveThread
			d.RequestsGet = rv.GetRequests
			d.RequestsAdd = rv.AddRequests
			d.RequestsRemove = rv.RemoveRequests
		}
		if ck := c.checksSvc; ck != nil {
			d.ChecksList = ck.ListChecks
			d.ChecksCombined = ck.Combined
			d.ChecksStatuses = ck.GetStatuses
		}
		if rel := c.releasesSvc; rel != nil {
			// The header's GAP is closed: ListReleasePage is the exported page
			// read over the same rows ListReleases returns.
			d.ReleasesList = rel.ListReleasePage
			d.ReleaseGet = rel.GetRelease
			d.ReleaseLatest = rel.LatestRelease
			d.ReleasePut = rel.PutRelease
			d.ReleaseDelete = rel.DeleteRelease
			d.AssetUpload = rel.UploadAsset
			d.AssetDelete = rel.DeleteAsset
		}
	}
	if apiEnv != nil {
		// The merge box reads the same policy document the API enforces
		// (spec §6 F3; api.PolicyOf is the exported read — raw bytes and
		// version stay internal).
		env := apiEnv
		d.PolicyOf = func(ctx context.Context, id git.RepoId) (*policy.Document, error) {
			return api.PolicyOf(ctx, env, id)
		}
	}
	webui.BindCollab(d)
}

// ---------------------------------------------------------------------------
// forms: create/import/fork/release/issue/PR/labels/milestones
// ---------------------------------------------------------------------------

func wireFormPages(c *collabWiring) {
	d := webui.FormsDeps{}
	if c != nil {
		// The interfaces are satisfied by the concrete services; a nil service
		// leaves that interface nil (the page renders 503).
		if c.ident != nil {
			d.Identity = c.ident
		}
		if c.issuesSvc != nil {
			d.Issues = c.issuesSvc
		}
		if c.pullsSvc != nil {
			d.Pulls = c.pullsSvc
		}
		if c.releasesSvc != nil {
			d.Releases = c.releasesSvc
		}
		if c.importSvc != nil {
			d.Import = c.importSvc
		}
		if c.mirrorSvc != nil {
			d.Mirror = c.mirrorSvc
		}
	}
	webui.BindForms(d)
}

// ---------------------------------------------------------------------------
// settings: /setup (both boot modes) + repo/org settings
// ---------------------------------------------------------------------------

func wireSettingsPages(cfg *config.Config, boot server.BootState, apiEnv *api.Env, c *collabWiring) {
	d := webui.SettingsDeps{
		SetupAccess:   setupAccessFn(cfg, boot),
		SetupView:     setupViewFn(cfg, boot),
		SetupValidate: setupValidateFn(cfg, boot),
		SetupSave:     setupSaveFn(cfg, boot),
		SetupAuthTest: setupAuthTestFn(),
	}
	if c != nil {
		// One call fills Repo{General,Scheduled,Mirror,PushMirror,Policy,
		// Config,Role,Access,Admin,Write} from the same services the JSON
		// handlers call; a nil service leaves its panel degraded. It must run
		// after any manual assignment (it overwrites those fields).
		d.BindSettingsServices(webui.SettingsServices{
			API:    apiEnv,
			Ident:  c.ident,
			Checks: c.checksSvc,
			Notify: c.notifySvc,
			Mirror: c.mirrorHandler,
			Push:   c.pushMirrorHandler,
			Now:    time.Now,
		})
		if c.ident != nil && apiEnv != nil {
			d.OrgView = orgViewFn(c.ident, c.notifySvc)
		}
	}
	webui.BindSettings(d)
}

// setupAccessFn mirrors internal/server's setupAccess rule (§3.4) without the
// write-a-response half: open while the config is invalid/absent or
// auth.mode == none, else an authenticated admin principal is required.
// Admin is Principal.Admin, so OIDC users in server.auth.admin_groups qualify
// (WP13) along with the admin email/domain rules. No env token exists.
func setupAccessFn(cfg *config.Config, boot server.BootState) func(*http.Request, auth.Principal) *auth.AuthError {
	return func(r *http.Request, p auth.Principal) *auth.AuthError {
		open := boot.Mode == "defaults" || boot.InSetupOnly() ||
			(cfg != nil && cfg.Server.Auth.Mode == "none")
		if open {
			return nil
		}
		if p.Anonymous {
			return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required for setup"}
		}
		if !p.Admin {
			return &auth.AuthError{Kind: auth.ErrForbidden, Why: "setup requires an admin principal (an OIDC admin group, admin email, or admin domain)"}
		}
		return nil
	}
}

// setupViewFn builds the schema view from the SAME functions GET
// /api/v1/setup serves (setup.BuildSchema over the running config + the boot
// state), flattened to the display strings the form posts back.
func setupViewFn(cfg *config.Config, boot server.BootState) func(context.Context) (*webui.SettingsSetupView, error) {
	return func(context.Context) (*webui.SettingsSetupView, error) {
		fileState := "absent"
		switch {
		case boot.Mode == "normal":
			fileState = "valid"
		case boot.InSetupOnly():
			fileState = "invalid"
		}
		view := &webui.SettingsSetupView{FileState: fileState}
		if cfg != nil {
			view.AuthMode = cfg.Server.Auth.Mode
			view.PublicURL = cfg.Server.PublicURL
			view.Issuer = cfg.Server.Auth.Issuer
			view.ClientID = cfg.Server.Auth.OAuthClientID
		}
		for _, e := range boot.Errors {
			view.Errors = append(view.Errors, webui.SettingsSetupError{Message: e})
		}
		effective := cfg
		if effective == nil {
			effective = config.Defaults()
		}
		for _, g := range setup.BuildSchema(effective, config.Defaults()) {
			sec := webui.SettingsSetupSection{Section: g.Section}
			for _, k := range g.Keys {
				ftype := k.Type
				if k.DisplayType != "" {
					ftype = k.DisplayType
				}
				// Secret rows carry the raw value here; the page layer masks it
				// before rendering (webui.settingsPrepareSetup), so the real
				// value never reaches a template. Save is overwrite-only:
				// setup.ParseValues keeps the stored value when the mask or an
				// empty field comes back.
				sec.Essential = append(sec.Essential, webui.SettingsSetupField{
					Key:     k.Key,
					Type:    ftype,
					Value:   setupValueString(k.Value),
					Default: setupValueString(k.Default),
					Secret:  k.Secret,
				})
			}
			view.Sections = append(view.Sections, sec)
		}
		return view, nil
	}
}

// setupValueString flattens one schema value to the raw string the form posts
// back (the spellings setup.Coerce accepts: "64MiB", "7d", true, lists as
// comma-separated items).
func setupValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []string:
		return strings.Join(x, ", ")
	default:
		return ""
	}
}

// setupValidateFn is the API's POST /api/v1/setup/test: parse over the
// file-visible base, run the full validator, write nothing.
func setupValidateFn(cfg *config.Config, boot server.BootState) func(context.Context, map[string]string) ([]webui.SettingsSetupError, error) {
	return func(_ context.Context, values map[string]string) ([]webui.SettingsSetupError, error) {
		c, err := setupCandidate(cfg, boot, values)
		if err != nil {
			return []webui.SettingsSetupError{{Message: err.Error()}}, nil
		}
		return setupErrors(setup.Validate(c)), nil
	}
}

// setupSaveFn is the API's PUT /api/v1/setup: validate THEN atomically write
// <data-dir>/spindle.toml and diff against the running effective config.
func setupSaveFn(cfg *config.Config, boot server.BootState) func(context.Context, map[string]string) (webui.SettingsSetupResult, []webui.SettingsSetupError, error) {
	return func(_ context.Context, values map[string]string) (webui.SettingsSetupResult, []webui.SettingsSetupError, error) {
		c, err := setupCandidate(cfg, boot, values)
		if err != nil {
			return webui.SettingsSetupResult{}, []webui.SettingsSetupError{{Message: err.Error()}}, nil
		}
		if errs := setupErrors(setup.Validate(c)); len(errs) > 0 {
			return webui.SettingsSetupResult{}, errs, nil
		}
		dataDir, effective := "", config.Defaults()
		if cfg != nil {
			dataDir, effective = cfg.DataDir, cfg
		}
		restart, serr := setup.Save(dataDir, c, effective)
		if serr != nil {
			return webui.SettingsSetupResult{}, nil, serr
		}
		return webui.SettingsSetupResult{Saved: true, RequiresRestart: restart}, nil, nil
	}
}

// setupCandidate applies the form's raw key→value pairs on top of the
// file-visible base (defaults ⊕ the first existing config file, §3.4) — the
// same base setupPut edits, so untouched keys keep their current values.
func setupCandidate(cfg *config.Config, boot server.BootState, values map[string]string) (*config.Config, error) {
	dataDir := ""
	if cfg != nil {
		dataDir = cfg.DataDir
	}
	base := setup.BaseConfig(dataDir, boot.ConfigPaths)
	v := url.Values{}
	for k, raw := range values {
		v.Set(k, raw)
	}
	return setup.ParseValues(v, base)
}

// setupErrors maps setup.Error onto the page's error row.
func setupErrors(in []setup.Error) []webui.SettingsSetupError {
	out := make([]webui.SettingsSetupError, 0, len(in))
	for _, e := range in {
		out = append(out, webui.SettingsSetupError{Key: e.Key, Message: e.Message})
	}
	return out
}

// setupAuthTestFn is the auth card's discovery probe (setup.ProbeAuth — the
// same call POST /api/v1/setup/auth/test makes).
func setupAuthTestFn() func(context.Context, string, string, string) (*webui.SettingsSetupAuthTest, []webui.SettingsSetupError) {
	return func(_ context.Context, issuer, clientID, redirectURI string) (*webui.SettingsSetupAuthTest, []webui.SettingsSetupError) {
		probe, errs := setup.ProbeAuth(issuer, clientID, redirectURI)
		if len(errs) > 0 {
			return nil, setupErrors(errs)
		}
		return &webui.SettingsSetupAuthTest{
			Issuer:                probe.Issuer,
			AuthorizationEndpoint: probe.AuthorizationEndpoint,
			TokenEndpoint:         probe.TokenEndpoint,
			JWKSURI:               probe.JWKSURI,
			ClientIDPresent:       probe.ClientIDPresent,
		}, nil
	}
}

// orgViewFn projects one organisation's settings page from the identity
// surface (+ the notify activity log) the API's org handlers read.
func orgViewFn(ident *identity.Service, notes *notify.Service) func(context.Context, string, auth.Principal) (*webui.SettingsOrgPage, error) {
	return func(ctx context.Context, org string, p auth.Principal) (*webui.SettingsOrgPage, error) {
		doc, err := ident.GetOrg(ctx, org)
		if err != nil || doc == nil {
			return nil, nil // not an org → the page renders 404
		}
		page := &webui.SettingsOrgPage{Org: org}
		if can, cerr := ident.CanEditOwnerProfile(ctx, org, p); cerr == nil {
			page.CanManage = can
		}
		profile := &webui.SettingsOrgProfile{
			DisplayName: doc.DisplayName,
			Description: doc.Description,
			Location:    doc.Location,
			Timezone:    doc.Timezone,
			BioMarkdown: doc.BioMarkdown,
			CreatedAt:   doc.CreatedAt,
			UpdatedAt:   doc.UpdatedAt,
			ReadOnly:    !page.CanManage,
		}
		if doc.AvatarContentType != "" {
			// The public avatar route the owner page uses (the BROWSER fetches
			// it; this is not a composition-level loopback):
			// /api/v1/orgs/{org}/avatar?v=<install>.
			profile.AvatarURL = "/api/v1/orgs/" + url.PathEscape(strings.ToLower(org)) + "/avatar"
			profile.AvatarType = doc.AvatarContentType
			if doc.AvatarUpdatedAt != "" {
				profile.AvatarURL += "?v=" + url.QueryEscape(doc.AvatarUpdatedAt)
			}
		}
		page.Profile = profile
		if m, merr := ident.GetMembers(ctx, org); merr == nil && m != nil {
			for _, row := range m.Members {
				page.Members = append(page.Members, webui.SettingsOrgMember{
					Principal: row.Principal, Role: string(row.Role),
				})
			}
		}
		if teams, terr := ident.ListTeams(ctx, org, orgTeamLimit); terr == nil {
			for _, t := range teams {
				if t == nil {
					continue
				}
				page.Teams = append(page.Teams, webui.SettingsOrgTeam{Slug: t.Slug, Name: t.Name, Members: t.Members})
			}
		}
		if invites, ierr := ident.ListOrgInvites(ctx, org, orgInviteLimit); ierr == nil {
			now := time.Now().UTC()
			for _, inv := range invites {
				if inv == nil {
					continue
				}
				row := webui.SettingsOrgInvite{
					ID: inv.ID, Subject: inv.Subject, Role: inv.Role,
					InvitedBy: inv.InvitedBy, ExpiresAt: inv.ExpiresAt,
				}
				if exp, perr := time.Parse(time.RFC3339, inv.ExpiresAt); perr == nil && now.After(exp) {
					row.Expired = true
				}
				page.Invites = append(page.Invites, row)
			}
		}
		if notes != nil && page.CanManage {
			if acts, more, aerr := notes.ListOrgActivity(ctx, org, 0, orgActivityLimit); aerr == nil {
				for _, a := range acts {
					page.Activity = append(page.Activity, webui.SettingsOrgEvent{
						Seq: uint64(a.Seq), At: a.At, Action: a.Action, Actor: a.Actor, Title: a.Title,
					})
				}
				page.More = more
			}
		}
		return page, nil
	}
}
