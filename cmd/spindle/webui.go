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
	"errors"
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
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/setup"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
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
	wirePackagesPage(c)
	wireSettingsPages(cfg, boot, apiEnv, c)
}

// wirePackagesPage binds the registry seam onto the Packages page
// (docs/features/14 §7): the page reads the same ListPackages the JSON API
// serves and gates deletes with the SAME packages.CanPush (identity.CheckPush,
// the git receive-pack rule).
func wirePackagesPage(c *collabWiring) {
	d := webui.PackagesDeps{}
	if c != nil && c.packagesSvc != nil {
		svc := c.packagesSvc
		d.List = func(ctx context.Context, owner, repo string) (*packages.RepoPackages, error) {
			res, oerr := svc.ListPackages(ctx, owner, repo, 0)
			if oerr != nil {
				return nil, oerr
			}
			return res, nil
		}
		// The service layer speaks *packages.Error; these seams speak error.
		// Returning those results directly would hand the page a TYPED-NIL
		// error on success (non-nil interface, nil pointer), which the handler
		// reads as a failure — the packages page 500'd after a successful
		// delete exactly this way. Convert explicitly, nil first.
		d.Get = func(ctx context.Context, owner, repo, digest string) (*packages.ManifestView, error) {
			v, perr := svc.GetPackage(ctx, owner, repo, digest)
			if perr != nil {
				return nil, perr
			}
			return v, nil
		}
		d.DeleteTag = func(ctx context.Context, owner, repo, tag string) error {
			if perr := svc.DeleteManifest(ctx, owner, repo, tag); perr != nil {
				return perr
			}
			return nil
		}
		d.DeleteManifest = func(ctx context.Context, owner, repo, digest string) error {
			if perr := svc.DeleteManifest(ctx, owner, repo, digest); perr != nil {
				return perr
			}
			return nil
		}
		d.CanPush = func(ctx context.Context, owner, repo string, p auth.Principal) bool {
			return svc.CanPush(ctx, owner, repo, p) == nil
		}
	}
	webui.BindPackages(d)
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
		d.UploadAvatar = func(ctx context.Context, principal string, data []byte) (*identity.Profile, error) {
			// The identity profile is created lazily (first login / first
			// role grant); the upload verb requires it. The page materializes
			// it first — the same EnsureProfile login runs — so the form is
			// never a button that 404s on a principal who has not logged in
			// through the avatar hook yet. Authority is unchanged: the verb
			// re-checks self-or-admin at the call site.
			if _, err := ident.EnsureProfile(ctx, principal); err != nil {
				return nil, err
			}
			return ident.PutUserAvatarBytes(ctx, principal, data)
		}
		d.UserExists = ident.UserExists
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
	if c != nil && c.socialSvc != nil {
		// The starred page reads the same service the JSON twin does (07 §7).
		d.Starred = c.socialSvc.Starred
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
			d.TagsList = c.tagsSvc.ListTagEntries
			d.TagDelete = c.tagsSvc.DeleteTag
		}
		if c.forksyncSvc != nil {
			d.SyncFork = c.forksyncSvc.Start
			d.SyncOutcome = c.forksyncSvc.LastOutcome
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
			d.Collaborators = ident.Collaborators
		}
		if iss := c.issuesSvc; iss != nil {
			d.IssuesList = iss.ListIssues
			d.IssuesGet = iss.GetThread
			d.IssueComment = iss.AddComment
			d.IssuePatch = iss.PatchIssue
			d.IssueMeta = iss.PatchThreadMetadata
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
			d.ReleaseDownloads = rel.AssetDownloads
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
	td := webui.ThreadsDeps{}
	if c != nil && c.threadsSvc != nil {
		th := c.threadsSvc
		td.List = func(ctx context.Context, owner, repo string, p auth.Principal, after string, n int, filter threads.RunFilter) (*threads.RunPage, error) {
			return th.ListRunsFiltered(ctx, owner+"/"+repo, p, after, n, filter)
		}
		td.Get = func(ctx context.Context, owner, repo, runID string, p auth.Principal) (*threads.RunView, error) {
			return th.GetRun(ctx, owner+"/"+repo, runID, p)
		}
		td.Workflows = func(ctx context.Context, owner, repo, commit string, p auth.Principal) (*threads.WorkflowList, error) {
			return th.ListWorkflows(ctx, owner+"/"+repo, commit, p)
		}
		// Operator verbs (15 §3): the page and the JSON API run the same
		// service calls (the page gate is only the read gate; the service
		// re-checks the write role).
		td.Cancel = func(ctx context.Context, owner, repo, runID string, p auth.Principal) error {
			_, err := th.CancelRun(ctx, owner+"/"+repo, runID, p)
			return err
		}
		td.Rerun = func(ctx context.Context, owner, repo, runID string, p auth.Principal) error {
			_, err := th.RerunRun(ctx, owner+"/"+repo, runID, p)
			return err
		}
		// The first-time-contributor gate (15 §5.2): the page's approve/deny
		// controls call exactly the API's service verbs (maintain-role gate).
		td.Approve = func(ctx context.Context, owner, repo, runID string, p auth.Principal) error {
			_, err := th.ApproveRun(ctx, owner+"/"+repo, runID, p)
			return err
		}
		td.Deny = func(ctx context.Context, owner, repo, runID string, p auth.Principal) error {
			_, err := th.DenyRun(ctx, owner+"/"+repo, runID, p)
			return err
		}
		// The Run workflow form posts through the same service call as
		// POST …/api/threads (15 §6, round 2): one dispatch path, one gate.
		td.Dispatch = func(ctx context.Context, owner, repo, ref, workflow string, inputs map[string]string, p auth.Principal) ([]*threads.Run, error) {
			return th.DispatchRun(ctx, owner+"/"+repo, ref, workflow, inputs, p)
		}
		if c.ident != nil {
			ident := c.ident
			td.Role = func(ctx context.Context, owner, repo string, p auth.Principal) (string, error) {
				if p.Admin {
					return "admin", nil
				}
				role, _ := ident.Resolve(ctx, owner, repo, p)
				return string(role), nil
			}
		}
	}
	webui.BindThreads(td)
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
			API:     apiEnv,
			Ident:   c.ident,
			Checks:  c.checksSvc,
			Notify:  c.notifySvc,
			Mirror:  c.mirrorHandler,
			Push:    c.pushMirrorHandler,
			Threads: c.threadsSvc,
			Now:     time.Now,
		})
		if c.ident != nil && apiEnv != nil {
			d.OrgView = orgViewFn(c.ident, c.notifySvc)
		}
		if c.ident != nil {
			// WP9 G2: every org settings form (profile/avatar, members,
			// teams, invitations, danger) writes through identity's verbs.
			// Without this binding the forms rendered but every submit
			// answered 503 "not wired".
			d.OrgWrite = orgWriteFn(c.ident)
		}
		if c.notifySvc != nil {
			// The org Webhooks tab (WP6.2) is composition-owned: the same
			// notify service the JSON twins call, one adapter.
			d.OrgWebhookWrite = orgWebhookWriteFn(c.notifySvc)
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
		schema := setup.BuildSchema(effective, config.Defaults())
		// F5: a field whose file value failed validation is marked in place, so
		// the pre-filled page points at the key to fix (the banner keeps the
		// full list too).
		keys := make([]string, 0, 160)
		for _, g := range schema {
			for _, k := range g.Keys {
				keys = append(keys, k.Key)
			}
		}
		fieldErrs := setupBootFieldErrors(boot.Errors, keys)
		for _, g := range schema {
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
				f := webui.SettingsSetupField{
					Key:     k.Key,
					Type:    ftype,
					Value:   setup.DisplayValue(k),
					Default: setup.DisplayValue(setup.Field{Type: k.Type, Value: k.Default}),
					Secret:  k.Secret,
					Enum:    k.Enum,
					Errors:  fieldErrs[k.Key],
				}
				if len(k.Fields) > 0 {
					// A struct slice renders as rows of its element's fields
					// (G3 tokens / G4 bundle strategies), values masked by
					// setup.RenderRows so no secret is ever published.
					f.Rows = setup.RenderRows(k)
					f.FromFile = setup.RowsDiffer(k)
				}
				sec.Essential = append(sec.Essential, f)
			}
			view.Sections = append(view.Sections, sec)
		}
		return view, nil
	}
}

// setupBootFieldErrors maps each boot error message onto the schema keys it
// names, so the pre-filled setup page marks the offending rows in place. The
// validator writes key names into its messages ("server.auth.session_secret
// must be at least 32 bytes"); matching is by whole key (a char before/after
// cannot continue the identifier), and one message may name several keys.
func setupBootFieldErrors(msgs, keys []string) map[string][]webui.SettingsSetupError {
	out := map[string][]webui.SettingsSetupError{}
	for _, msg := range msgs {
		for _, key := range keys {
			if !setup.MessageNamesKey(msg, key) {
				continue
			}
			out[key] = append(out[key], webui.SettingsSetupError{Key: key, Message: msg})
		}
	}
	return out
}

// setupValidateFn is the API's POST /api/v1/setup/test: parse over the
// file-visible base, run the full validator, write nothing.
func setupValidateFn(cfg *config.Config, boot server.BootState) func(context.Context, map[string]string) ([]webui.SettingsSetupError, error) {
	return func(_ context.Context, values map[string]string) ([]webui.SettingsSetupError, error) {
		c, err := setupCandidate(cfg, boot, values)
		if err != nil {
			return setupParseErrors(err), nil
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
			return webui.SettingsSetupResult{}, setupParseErrors(err), nil
		}
		if errs := setupErrors(setup.Validate(c)); len(errs) > 0 {
			return webui.SettingsSetupResult{}, errs, nil
		}
		dataDir, effective := "", config.Defaults()
		if cfg != nil {
			dataDir, effective = cfg.DataDir, cfg
		}
		restart, backup, serr := setup.Save(dataDir, c, effective)
		if serr != nil {
			return webui.SettingsSetupResult{}, nil, serr
		}
		return webui.SettingsSetupResult{Saved: true, RequiresRestart: restart, Backup: backup}, nil, nil
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

// setupParseErrors maps a coercion failure onto the page's per-key error row.
// Coerce reports a keyed setup.Error (the same structure config.Validate
// errors use), so a rejected value marks its row and opens its Advanced group;
// only a genuinely keyless failure stays in the banner.
func setupParseErrors(err error) []webui.SettingsSetupError {
	return []webui.SettingsSetupError{{Key: setup.KeyOf(err), Message: err.Error()}}
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
		// The API's org-write authority is CheckOrgOwner, which admits host
		// admins; CanEditOwnerProfile only knows roster owners. Mirror the
		// API here (a host admin must not see a read-only page the API would
		// let them write).
		if p.Admin {
			page.CanManage = true
		} else if can, cerr := ident.CanEditOwnerProfile(ctx, org, p); cerr == nil {
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
				page.Teams = append(page.Teams, webui.SettingsOrgTeam{
					Slug: t.Slug, Name: t.Name, Description: t.Description, Members: t.Members,
				})
			}
		}
		// Pending org invitations are an OWNER read (the API route is
		// CheckOrgOwner) and their issuer objects carry capability tokens:
		// the page never reads them for a non-owner, and even an owner's read
		// only renders the masked marker — AcceptURL is set solely by the
		// create response (SettingsWriteResult.Invite).
		if page.CanManage {
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
			// Org Webhooks tab (WP6.2): the same rows the repo tab renders
			// (delivery tail bounded exactly like settingsDeliveryLimit).
			if hooks, herr := notes.ListOrgHooks(ctx, org); herr == nil {
				page.WebhooksWired = true
				for _, h := range hooks {
					row := webui.SettingsWebhook{
						ID: h.ID, URL: h.URL, Events: strings.Join(h.Events, ", "),
						Active: h.Active, SecretSet: h.Secret != "",
					}
					if d := notes.ReadOrgDeliveries(ctx, org, h.ID); d != nil && len(d.Entries) > 0 {
						start := len(d.Entries) - 5
						if start < 0 {
							start = 0
						}
						for _, e := range d.Entries[start:] {
							row.Recent = append(row.Recent, webui.SettingsDelivery{
								Seq: strconv.Itoa(e.Seq), SeqNum: e.Seq, Event: e.Event, Status: e.Status,
								At: e.At, Error: e.Error, DurationMs: e.DurationMs,
								Redeliverable: len(e.Body) > 0,
							})
						}
						last := d.Entries[len(d.Entries)-1]
						row.LastPing = "last delivery: " + last.Event + " → " + strconv.Itoa(last.Status) + " at " + last.At
					}
					page.Webhooks = append(page.Webhooks, row)
				}
			}
		}
		return page, nil
	}
}

// orgOwnerGate returns the page write error for a failed org-owner check
// (nil = allowed). Host admins pass exactly like the API's CheckOrgOwner;
// the page handler checked the tab, but every seam re-checks its own
// authority because the seam is public (the SettingsServices.repoWrite rule).
func orgOwnerGate(aerr *auth.AuthError) *webui.SettingsWriteError {
	if aerr == nil {
		return nil
	}
	status := http.StatusUnauthorized
	switch aerr.Kind {
	case auth.ErrForbidden:
		status = http.StatusForbidden
	case auth.ErrUnavailable:
		status = http.StatusServiceUnavailable
	}
	return orgWebhookErr(status, aerr.Why)
}

// orgWriteFn is the identity-backed org settings write seam (WP9 G2): the
// same verbs the JSON API's org routes call, so every form the page renders
// round-trips. (S)hell-pages adapters are thin; validation and the CAS live
// in identity. Fields the Org model does not have (e.g. website) are not
// invented here — the page renders what the document stores.
func orgWriteFn(ident *identity.Service) func(context.Context, string, auth.Principal, webui.SettingsOrgWrite) (webui.SettingsWriteResult, *webui.SettingsWriteError) {
	return func(ctx context.Context, org string, p auth.Principal, w webui.SettingsOrgWrite) (webui.SettingsWriteResult, *webui.SettingsWriteError) {
		if ident == nil {
			return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusServiceUnavailable, "identity service not configured")
		}
		if werr := orgOwnerGate(ident.CheckOrgOwner(ctx, org, p)); werr != nil {
			return webui.SettingsWriteResult{}, werr
		}
		switch w.Action {
		case webui.SettingsOrgProfileSave:
			_, err := ident.PutOrg(ctx, org, identity.OrgEdit{
				DisplayName: w.Get("display_name"),
				Description: w.Get("description"),
				Location:    w.Get("location"),
				Timezone:    w.Get("timezone"),
				BioMarkdown: w.Get("bio_markdown"),
			})
			if err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "profile saved"}, nil
		case webui.SettingsOrgAvatar:
			if len(w.File) == 0 {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "choose an image to upload")
			}
			if _, err := ident.PutOrgAvatar(ctx, org, w.File); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "avatar updated"}, nil
		case webui.SettingsOrgAvatarRm:
			if _, err := ident.DeleteOrgAvatar(ctx, org); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "avatar removed"}, nil
		case webui.SettingsOrgMemberSet:
			principal := strings.ToLower(strings.TrimSpace(w.Get("principal")))
			if principal == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a principal is required")
			}
			role := identity.OrgRole(strings.ToLower(strings.TrimSpace(w.Get("role"))))
			if role != identity.OrgOwner && role != identity.OrgMember {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "role must be member or owner")
			}
			if _, err := ident.SetMember(ctx, org, principal, role); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			// The API's member PUT ensures the profile row exists too.
			if _, err := ident.EnsureProfile(ctx, principal); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: principal + " is now " + string(role)}, nil
		case webui.SettingsOrgMemberRm:
			principal := strings.ToLower(strings.TrimSpace(w.Get("principal")))
			if principal == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a principal is required")
			}
			if _, err := ident.RemoveMember(ctx, org, principal); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: principal + " removed"}, nil
		case webui.SettingsOrgTeamSave:
			slug := strings.ToLower(strings.TrimSpace(w.Get("slug")))
			if slug == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a team slug is required")
			}
			if _, err := ident.CreateTeam(ctx, org, slug, w.Get("name"), w.Get("description")); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "team " + slug + " created"}, nil
		case webui.SettingsOrgTeamUpdate:
			slug := strings.ToLower(strings.TrimSpace(w.Get("slug")))
			if _, err := ident.PutTeam(ctx, org, slug, w.Get("name"), w.Get("description")); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "team " + slug + " updated"}, nil
		case webui.SettingsOrgTeamAdd:
			slug := strings.ToLower(strings.TrimSpace(w.Get("slug")))
			principal := strings.ToLower(strings.TrimSpace(w.Get("principal")))
			if principal == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a principal is required")
			}
			if _, err := ident.SetTeamMember(ctx, org, slug, principal); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: principal + " joined team " + slug}, nil
		case webui.SettingsOrgTeamMemberRm:
			slug := strings.ToLower(strings.TrimSpace(w.Get("slug")))
			principal := strings.ToLower(strings.TrimSpace(w.Get("principal")))
			if _, err := ident.RemoveTeamMember(ctx, org, slug, principal); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: principal + " left team " + slug}, nil
		case webui.SettingsOrgTeamRm, webui.SettingsOrgTeamDel:
			slug := strings.ToLower(strings.TrimSpace(w.Get("slug")))
			team, _, err := ident.GetTeam(ctx, org, slug)
			if err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			if team == nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusNotFound, "unknown team")
			}
			if err := ident.DeleteTeam(ctx, org, slug); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "team " + slug + " deleted"}, nil
		case webui.SettingsOrgInviteMake:
			// The subject is an email OR a username (the same addressing the
			// repo Access tab accepts): a static-token principal has no
			// verified email, so an email-only form would make it uninvitable.
			subject := strings.ToLower(strings.TrimSpace(w.Get("subject")))
			if subject == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "an email or username is required")
			}
			role := strings.ToLower(strings.TrimSpace(w.Get("role")))
			if role == "" {
				role = string(identity.OrgMember)
			}
			inv, err := ident.CreateOrgInvite(ctx, org, subject, role, p.Name, 7*24*time.Hour)
			if err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			// Same one-shot treatment as the repo Access tab: the human accept
			// page, rendered once in the page body (never a redirect) and
			// carried on the created row so it sits in the pending table.
			// Later reads redact the token and render the masked marker.
			link := webui.InvitePageURL(inv.ID, inv.Token)
			return webui.SettingsWriteResult{
				Notice: "invitation created — share the link below",
				Link:   link,
				Invite: &webui.SettingsOrgInvite{
					ID: inv.ID, Subject: inv.Subject, Role: inv.Role,
					InvitedBy: inv.InvitedBy, ExpiresAt: inv.ExpiresAt, AcceptURL: link,
				},
			}, nil
		case webui.SettingsOrgInviteRm:
			id := strings.TrimSpace(w.Get("id"))
			if _, err := ident.DeleteOrgInvite(ctx, org, id, p.Name); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "invitation cancelled"}, nil
		case webui.SettingsOrgDelete:
			if err := ident.DeleteOrg(ctx, org); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgIdentityStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "organization deleted", Redirect: "/"}, nil
		}
		return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusServiceUnavailable, "unknown organization action "+w.Action)
	}
}

// orgIdentityStatus mirrors identity's error → status mapping (401/403/404/
// 409/413/415; a store failure is 503, never a 500 with internals).
func orgIdentityStatus(err error) int {
	switch {
	case errors.Is(err, identity.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, identity.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, identity.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, identity.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, identity.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, identity.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, identity.ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	}
	return http.StatusServiceUnavailable
}

// orgWebhookWriteFn adapts the org-hook service to the settings page seam
// (WP6.2). The service methods are principal-free, so this adapter re-checks
// the narrow owner checker first (the page checked it, but the seam is
// public; the same rule SettingsServices.repoWrite follows).
func orgWebhookWriteFn(notes *notify.Service) func(context.Context, string, auth.Principal, webui.SettingsOrgWrite) (webui.SettingsWriteResult, *webui.SettingsWriteError) {
	return func(ctx context.Context, org string, p auth.Principal, w webui.SettingsOrgWrite) (webui.SettingsWriteResult, *webui.SettingsWriteError) {
		if notes == nil {
			return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusServiceUnavailable, "notifications service not configured")
		}
		if notes.OrgOwner != nil {
			if werr := orgOwnerGate(notes.OrgOwner.CheckOrgOwner(ctx, org, p)); werr != nil {
				return webui.SettingsWriteResult{}, werr
			}
		} else if !p.Admin {
			return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusForbidden, "org owner required")
		}
		switch w.Action {
		case webui.SettingsOrgWebhookCreate:
			url := strings.TrimSpace(w.Get("url"))
			if url == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "url is required")
			}
			spec := notify.HookSpec{URL: &url}
			if evs := orgWebhookEvents(w.Get("events")); len(evs) > 0 {
				spec.Events = evs
			}
			if secret := w.Get("secret"); secret != "" {
				spec.Secret = &secret
			}
			if _, err := notes.CreateOrgHook(ctx, org, p.Name, spec); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "webhook created"}, nil
		case webui.SettingsOrgWebhookToggle:
			id := w.Get("id")
			h := notes.GetOrgHook(ctx, org, id)
			if h == nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusNotFound, "unknown webhook")
			}
			active := !h.Active
			if _, err := notes.PatchOrgHook(ctx, org, id, notify.HookSpec{Active: &active}); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(err), err.Error())
			}
			if active {
				return webui.SettingsWriteResult{Notice: "webhook enabled"}, nil
			}
			return webui.SettingsWriteResult{Notice: "webhook disabled"}, nil
		case webui.SettingsOrgWebhookRotate:
			secret := w.Get("secret")
			if secret == "" {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a new secret is required")
			}
			if _, err := notes.PatchOrgHook(ctx, org, w.Get("id"), notify.HookSpec{Secret: &secret}); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "webhook secret rotated"}, nil
		case webui.SettingsOrgWebhookPing:
			delivered, err := notes.PingOrgHook(ctx, org, w.Get("id"), p.Name)
			if err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(err), err.Error())
			}
			if !delivered {
				return webui.SettingsWriteResult{Notice: "ping failed — the delivery row carries the detail"}, nil
			}
			return webui.SettingsWriteResult{Notice: "ping delivered"}, nil
		case webui.SettingsOrgWebhookDelete:
			if err := notes.DeleteOrgHook(ctx, org, w.Get("id")); err != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(err), err.Error())
			}
			return webui.SettingsWriteResult{Notice: "webhook deleted"}, nil
		case webui.SettingsOrgWebhookRedeliver:
			seq, err := strconv.Atoi(w.Get("seq"))
			if err != nil || seq < 0 {
				return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusUnprocessableEntity, "a delivery seq is required")
			}
			entry, rerr := notes.RedeliverOrgHook(ctx, org, w.Get("id"), seq)
			if rerr != nil {
				return webui.SettingsWriteResult{}, orgWebhookErr(orgWebhookStatus(rerr), rerr.Error())
			}
			return webui.SettingsWriteResult{Notice: "redelivered #" + strconv.Itoa(entry.Seq) + " → " + strconv.Itoa(entry.Status)}, nil
		}
		return webui.SettingsWriteResult{}, orgWebhookErr(http.StatusServiceUnavailable, "unknown webhook action")
	}
}

// orgWebhookEvents splits the comma filter (the settingsEventList rule).
func orgWebhookEvents(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// orgWebhookStatus maps notify sentinels to wire statuses.
func orgWebhookStatus(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, notify.ErrNotFound.Error()):
		return http.StatusNotFound
	case strings.Contains(msg, notify.ErrUnauthorized.Error()):
		return http.StatusUnauthorized
	case strings.Contains(msg, notify.ErrForbidden.Error()):
		return http.StatusForbidden
	case strings.Contains(msg, notify.ErrInvalid.Error()):
		return http.StatusBadRequest
	case strings.Contains(msg, notify.ErrConflict.Error()):
		return http.StatusConflict
	case strings.Contains(msg, notify.ErrGone.Error()):
		return http.StatusGone
	}
	return http.StatusServiceUnavailable
}

func orgWebhookErr(status int, msg string) *webui.SettingsWriteError {
	return &webui.SettingsWriteError{Status: status, Message: msg}
}
