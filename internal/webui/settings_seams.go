// settings_seams.go — the repo/org settings service adapters (D-WEB-14).
//
// The page layer never owns data: every read and write here is the SAME
// service call the JSON API handler makes, so a page and its API twin cannot
// drift. The one composition call
//
//	d := webui.SettingsDeps{ /* repo/org fields from cmd/spindle/webui.go */ }
//	d.BindSettingsServices(webui.SettingsServices{
//	    API: apiEnv, Ident: c.ident, Checks: c.checksSvc, Notify: c.notifySvc,
//	    Mirror: c.mirrorHandler, Push: c.pushMirrorHandler, Now: time.Now,
//	})
//	webui.BindSettings(d)
//
// fills the repo-side seam. A nil service leaves its field nil and that panel
// keeps rendering its honest "not wired" card.
//
// # Authority
//
// The page handler already gates the tab (role ladder) and same-origin, but
// the seam is public and every write re-checks the API's authority itself:
// identity CheckRole(admin) for repo config, the feature services' own
// requireRole for CI tokens, and CheckRole + CheckCreateOwner for the danger
// actions. Status mapping is the handlers' (401/403/404/409/413/422/503).
package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/mirror"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/pushmirror"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// ThreadsSettings is the secret surface the repo settings page needs (the
// Threads service's secret methods; composition binds *threads.Service).
// Runners are INSTANCE infrastructure and are administered in the
// Administration area (/admin/runners), never in repo settings.
type ThreadsSettings interface {
	ListSecrets(ctx context.Context, repo string, p auth.Principal) ([]threads.SecretView, error)
	SetSecret(ctx context.Context, repo, name, value string, p auth.Principal) error
	DeleteSecret(ctx context.Context, repo, name string, p auth.Principal) error
	SecretsEnabled() bool
}

// environmentAdmin is the deployment-environment surface Settings → Environments
// needs (docs/PLAN-THREADS-ENVIRONMENTS.md). It is discovered by type assertion
// on SettingsServices.Threads: composition binds *threads.Service once, and a
// service (or a test fake) without these methods simply renders the tab's "not
// wired" card.
type environmentAdmin interface {
	ListEnvironments(ctx context.Context, repo string, p auth.Principal) ([]threads.EnvironmentView, error)
	PutEnvironment(ctx context.Context, repo, name string, in threads.EnvironmentInput, p auth.Principal) (*threads.EnvironmentView, error)
	DeleteEnvironment(ctx context.Context, repo, name string, p auth.Principal) error
	ListEnvironmentSecrets(ctx context.Context, repo, env string, p auth.Principal) ([]threads.SecretView, error)
	SetEnvironmentSecret(ctx context.Context, repo, env, name, value string, p auth.Principal) error
	DeleteEnvironmentSecret(ctx context.Context, repo, env, name string, p auth.Principal) error
	SecretsEnabled() bool
}

// environments returns the bound environment surface (nil when unwired). A
// typed-nil *threads.Service (a restricted build wires the settings pages with
// no Threads service) is treated as unwired, so the tab renders its honest card
// instead of calling a method on a nil receiver.
func (s SettingsServices) environments() environmentAdmin {
	e, ok := s.Threads.(environmentAdmin)
	if !ok {
		return nil
	}
	if v := reflect.ValueOf(e); v.Kind() == reflect.Ptr && v.IsNil() {
		return nil
	}
	return e
}

// SettingsServices is the concrete service set the settings group adapts.
// Every field is optional; nil disables the panels that depend on it.
type SettingsServices struct {
	API    *api.Env
	Ident  *identity.Service
	Checks *checks.Service
	Notify *notify.Service
	Mirror *mirror.Handler
	Push   *pushmirror.Handler
	// Threads powers Settings → Secrets (15 §6).
	Threads ThreadsSettings

	// Now is the clock for derived timestamps (tests pin it).
	Now func() time.Time
	// TaskWait bounds how long a "sync now" click waits for the fire's task
	// record to appear in the task table (the service starts it on a detached
	// goroutine). Zero uses the default (2s); the card is the only consumer.
	TaskWait time.Duration
}

// settingsHistoryLimit bounds the config-history list the page renders.
const settingsHistoryLimit = 20

// settingsDeliveryLimit bounds the per-hook delivery rows the page renders.
const settingsDeliveryLimit = 5

func (s SettingsServices) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// BindSettingsServices fills the repo-side read/write seam on d (the caller
// keeps the setup* and org* fields it already built).
func (d *SettingsDeps) BindSettingsServices(s SettingsServices) {
	d.RepoGeneral = s.repoGeneral
	d.RepoScheduled = s.repoScheduled
	d.RepoMirror = s.repoMirror
	d.RepoPushMirror = s.repoPushMirror
	d.RepoPolicy = s.repoPolicy
	d.RepoProtection = s.repoProtection
	d.RepoConfig = s.repoConfig
	if s.environments() != nil {
		d.RepoEnvironments = s.repoEnvironments
	}
	if s.API != nil && s.API.Repo != nil {
		d.RepoWAL = s.repoWAL
	}
	if s.Ident != nil {
		d.RepoRole = s.repoRole
		d.RepoAccess = s.repoAccess
	}
	if s.Ident != nil && (s.Checks != nil || s.Notify != nil) {
		d.RepoAdmin = s.repoAdmin
	}
	if s.API != nil || s.Ident != nil {
		d.RepoWrite = s.repoWrite
	}
}

func (s SettingsServices) repoID(owner, repo string) git.RepoId {
	return git.RepoId{Owner: owner, Name: repo}
}

// settingsNextSync formats the next scheduled fire for the settings tab:
// the shared relative phrase plus the absolute UTC stamp ("in 10h · 2026-11-27
// 00:00 UTC"). iso is the machine value for <time datetime>; title spells out
// that a manual sync does not move this schedule. The relative half is
// nextSyncPhrase — the same implementation the repo header uses, never a second
// relative-time formatter.
func settingsNextSync(m api.MirrorView, now time.Time) (value, iso, title string) {
	phrase := nextSyncPhrase(m, now)
	iso = m.NextSyncAt
	at, err := time.Parse(time.RFC3339, iso)
	if iso == "" || err != nil {
		return phrase, "", ""
	}
	stamp := settingsUTCStamp(at)
	return phrase + " · " + stamp, iso, iso + " · scheduled; manual syncs don't move it"
}

// settingsPushNext is the push block's next-sync row: the pull formatter
// except for an off schedule, which has no next fire at all (every push fans
// out immediately) — never "due now", which would promise a scheduled fire.
func settingsPushNext(m api.MirrorView, schedule string, now time.Time) (value, iso, title string) {
	if schedule == "" {
		return "no schedule — syncs on push", "", ""
	}
	return settingsNextSync(m, now)
}

// settingsLastSynced formats the last successful sync: the absolute UTC stamp
// plus its age ("2026-11-26 13:19 UTC · 1m ago"). A blank/unparseable stamp
// renders the honest "never" rather than an empty cell.
func settingsLastSynced(stamp string, now time.Time) (value, iso, title string) {
	if stamp == "" {
		return "never", "", ""
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp, "", ""
	}
	value = settingsUTCStamp(at)
	if age := relTimeAt(stamp, now); age != "" {
		value += " · " + age
	}
	return value, stamp, settingsUTCStamp(at) + " · last successful sync"
}

// settingsUTCStamp renders one instant as "2026-11-27 00:00 UTC" — the
// reader's spelling; the RFC3339 form lives only in the datetime/title
// attributes.
func settingsUTCStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// settingsSyncTask is the card a just-spawned mirror fire renders. title names
// the direction; refresh re-fetches the settings tab once the task succeeds so
// the last-sync facts land without a manual reload (its notice is what a direct
// GET of that URL shows at the top of the tab).
func settingsSyncTask(owner, repo, kind, title string, rec api.TaskRecord, found bool) *SettingsTask {
	if !found {
		return nil
	}
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	return &SettingsTask{
		ID:      rec.ID,
		Kind:    kind,
		Stream:  base + "/api/tasks/" + url.PathEscape(rec.ID),
		Title:   title,
		Refresh: base + "/settings/mirror?notice=" + url.QueryEscape("sync complete — the last-sync facts below are current"),
	}
}

// syncTaskRecord resolves the task-table record a fire just started as. The
// service's SyncAsync mints its OWN async id ("msync-…"/"psync-…"), which the
// shared /api/tasks/{id} stream does not know, so the card must attach to the
// WAL record. SyncAsync starts the body on a detached goroutine (it loads the
// sidecar before the table installs the record), so this polls the repo's
// tasks for the kind — bounded, and the single-flight means at most one such
// record exists: joining a fire already running is exactly the card the
// operator needs. start is the click's instant: a FINISHED record older than
// that is a previous fire (an already-in-sync no-op can complete before the
// first poll), never this click's outcome.
func (s SettingsServices) syncTaskRecord(ctx context.Context, owner, repo, kind string, start time.Time) (api.TaskRecord, bool) {
	if s.API == nil || s.API.Tasks == nil {
		return api.TaskRecord{}, false
	}
	wait := s.TaskWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		running, recent, err := s.API.Tasks.List(ctx, s.repoID(owner, repo))
		if err == nil {
			for _, rec := range append(running, recent...) {
				if rec.Kind != kind {
					continue
				}
				if rec.OK != nil {
					// Finished: only this click's fire may answer.
					at, perr := time.Parse(time.RFC3339Nano, rec.Started)
					if perr != nil || at.Before(start) {
						continue
					}
				}
				return rec, true
			}
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return api.TaskRecord{}, false
		}
		select {
		case <-ctx.Done():
			return api.TaskRecord{}, false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- reads -------------------------------------------------------------------

func (s SettingsServices) repoRole(ctx context.Context, owner, repo string, p auth.Principal) (string, error) {
	if p.Admin {
		return string(identity.RoleAdmin), nil
	}
	role, _ := s.Ident.Resolve(ctx, owner, repo, p)
	return string(role), nil
}

func (s SettingsServices) repoGeneral(ctx context.Context, owner, repo string) (*SettingsGeneral, error) {
	if s.API == nil || s.API.Repo == nil {
		return nil, nil
	}
	doc, err := s.API.Repo.Settings(ctx, s.repoID(owner, repo))
	if err != nil {
		return nil, err
	}
	body := []byte(doc.TOML)
	f := config.FeaturesOf(body)
	return &SettingsGeneral{
		Description:  config.DescriptionOf(body),
		Website:      config.WebsiteOf(body),
		MaxLength:    config.MaxRepoDescriptionRunes,
		MaxWebsite:   config.MaxRepoWebsiteBytes,
		SelfApproval: config.AllowSelfApprovalOf(body),
		Features: []SettingsFeatureRow{
			{Key: "issues", Enabled: f.Issues},
			{Key: "pulls", Enabled: f.Pulls},
			{Key: "releases", Enabled: f.Releases},
			{Key: "forks", Enabled: f.Forks},
			{Key: "watch", Enabled: f.Watch},
			{Key: "star", Enabled: f.Star},
		},
	}, nil
}

func (s SettingsServices) repoScheduled(ctx context.Context, owner, repo string) (*SettingsScheduled, error) {
	if s.API == nil || s.API.Repo == nil {
		return nil, nil
	}
	body, err := api.SettingsDescribe(ctx, s.API, s.repoID(owner, repo))
	if err != nil {
		return nil, err
	}
	out := &SettingsScheduled{
		UpstreamGit: body.Upstream.Git,
		LastRound:   body.Upstream.LastRound,
	}
	// The structured editors read the SAME stored + effective projections the
	// JSON settings API serves (api.SettingsDocument): stored sections decide
	// the override state, the merged config supplies inherited values.
	if _, stored, eff, derr := api.SettingsDocument(ctx, s.API, s.repoID(owner, repo)); derr == nil {
		out.Sections = settingsScheduledSections(stored, eff, nil)
		// The describe read still supplies the next-slot projection: show it
		// on the matching row (display-only; the editor never writes it).
		for _, st := range body.Strategies {
			for i := range out.Sections {
				if out.Sections[i].ID != "bundles" {
					continue
				}
				for j := range out.Sections[i].Rows {
					row := &out.Sections[i].Rows[j]
					if row.Base >= 0 && len(row.Cells) > 0 && row.Cells[0].Value == st.Name {
						row.Next = st.Next
					}
				}
			}
		}
	} else {
		out.Error = derr.Error()
	}
	host := body.Maintenance.ThisHost
	out.HostFacts = []SettingsKV{
		{Key: "hostname", Value: host.Name},
		{Key: "serves", Value: yesNo(host.Serves)},
		{Key: "maintains", Value: yesNo(host.Maintains)},
		{Key: "disk", Value: host.Disk},
		{Key: "max_pack_bytes", Value: strconv.FormatInt(host.MaxPackBytes, 10)},
		{Key: "cache_budget_bytes", Value: strconv.FormatInt(host.CacheBudgetBytes, 10)},
		{Key: "roles", Value: strings.Join(host.Roles, ", ")},
	}
	follow := strings.Join(body.Upstream.Follow, ", ")
	if follow == "" {
		follow = "nothing followed (a mirror follows its upstream)"
	}
	out.FollowLine = fmt.Sprintf("follow interval %ds · %s", body.Upstream.FollowIntervalSecs, follow)
	for _, f := range body.Fields {
		out.Effective = append(out.Effective, SettingsKV{
			Key:   f.Key,
			Value: fmt.Sprint(f.Value) + " (host " + fmt.Sprint(f.HostValue) + ", " + f.Source + ")",
		})
	}
	return out, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// repoWAL reads the Settings → WAL panel (WP9 G16): the same overview facts
// the WAL page renders, plus the shared ops inventory. Running an op goes to
// the existing …/api/ops/{op} endpoint; this read never starts anything.
func (s SettingsServices) repoWAL(ctx context.Context, owner, repo string) (*SettingsWAL, error) {
	if s.API == nil || s.API.Repo == nil {
		return nil, nil
	}
	id := s.repoID(owner, repo)
	ov, err := s.API.Repo.Overview(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &SettingsWAL{
		Owner:    owner,
		Name:     repo,
		Health:   ov.Health.Status,
		ServedBy: ov.Hostname,
		CloneURL: ov.CloneURL,
		Packs: fmt.Sprintf("%d live packs · %s · %d pushes recorded",
			ov.Packs.Live, fmtBytes(ov.Packs.LiveBytes), ov.Packs.Pushes),
	}
	if out.Health == "" {
		out.Health = "ok"
	}
	out.HealthTone = chipTone(out.Health)
	out.Facts = []SettingsKV{
		{Key: "manifest version", Value: strconv.FormatUint(ov.Manifest.Version, 10)},
		{Key: "next_seq", Value: strconv.FormatUint(ov.Manifest.NextSeq, 10)},
		{Key: "min_seq", Value: strconv.FormatUint(ov.Manifest.MinSeq, 10)},
		{Key: "entries", Value: strconv.FormatUint(ov.Manifest.Entries, 10)},
		{Key: "tail entries", Value: strconv.FormatUint(ov.Manifest.TailEntries, 10)},
		{Key: "checkpoint", Value: anyLabel(ov.Manifest.Checkpoint, "@")},
		{Key: "packset", Value: anyLabel(ov.Manifest.Packset, "")},
		{Key: "local size", Value: fmtBytes(ov.Local.SizeBytes)},
	}
	if s.API.Tasks != nil {
		out.Panel = walOpsPanel{
			Ops:    walOpsFrom(s.API.Tasks),
			Recent: walRecentFrom(ctx, s.API.Tasks, id),
			OpsURL: "/" + owner + "/" + repo + "/api/ops/",
		}
	}
	return out, nil
}

func (s SettingsServices) repoMirror(ctx context.Context, owner, repo string) (*SettingsMirror, error) {
	if s.Mirror == nil || s.Mirror.Svc == nil {
		return nil, nil
	}
	st := s.Mirror.Svc.Store()
	doc, _, err := mirror.Load(ctx, st, owner, repo)
	if err != nil {
		return nil, err
	}
	out := &SettingsMirror{Presets: settingsMirrorPresets, Schedule: "daily"}
	if doc == nil {
		return out, nil
	}
	now := s.now()
	v := mirror.ViewOf(doc, now)
	out.Configured = true
	out.Upstream = v.UpstreamURL
	out.Schedule = v.Schedule
	out.NextSync, out.NextSyncISO, out.NextSyncTitle = settingsNextSync(api.MirrorView{
		UpstreamURL:  v.UpstreamURL,
		NextSyncAt:   v.NextSyncAt,
		LastSyncedAt: v.LastSyncedAt,
		Due:          v.Due,
	}, now)
	out.LastSynced, out.LastSyncedISO, out.LastSyncedTitle = settingsLastSynced(v.LastSyncedAt, now)
	out.LastResult = v.LastResult
	out.Failures = v.ConsecutiveFailures
	return out, nil
}

func (s SettingsServices) repoPushMirror(ctx context.Context, owner, repo string) (*SettingsPushMirror, error) {
	if s.Push == nil || s.Push.Svc == nil {
		return nil, nil
	}
	st := s.Push.Svc.Store()
	doc, _, err := pushmirror.Load(ctx, st, owner, repo)
	if err != nil {
		return nil, err
	}
	out := &SettingsPushMirror{Presets: settingsPushPresets, AuthKinds: settingsPushAuthKinds}
	if doc == nil {
		return out, nil
	}
	sec, _, serr := pushmirror.LoadSecret(ctx, st, owner, repo)
	if serr != nil {
		return nil, serr
	}
	now := s.now()
	v := pushmirror.ViewOf(doc, sec, now)
	out.Configured = true
	out.Upstream = v.UpstreamURL
	out.AuthKind = v.AuthKind
	out.AuthLabel = settingsPushAuthLabel(v.AuthKind)
	out.Username = v.Username
	out.HasSecret = v.HasSecret
	out.SecretHint = v.SecretHint
	out.Fingerprint = v.KeyFingerprint
	out.HostKey = v.HostKeyFingerprint
	out.NextSync, out.NextSyncISO, out.NextSyncTitle = settingsPushNext(api.MirrorView{
		UpstreamURL:  v.UpstreamURL,
		NextSyncAt:   v.NextSyncAt,
		LastSyncedAt: v.LastSyncedAt,
		Due:          v.Due,
	}, v.Schedule, now)
	out.LastSynced, out.LastSyncedISO, out.LastSyncedTitle = settingsLastSynced(v.LastSyncedAt, now)
	out.LastResult = v.LastResult
	out.Schedule = v.Schedule
	out.PublicKey = v.PublicKey
	if sec != nil {
		out.KnownHosts = sec.SSHKnownHosts
	}
	return out, nil
}

func settingsPushAuthLabel(kind string) string {
	for _, k := range settingsPushAuthKinds {
		if k.ID == kind {
			return k.Label
		}
	}
	if kind == "" {
		return "no auth"
	}
	return kind
}

func (s SettingsServices) repoPolicy(ctx context.Context, owner, repo string) (*SettingsPolicy, error) {
	if s.API == nil || s.API.Store == nil {
		return nil, nil
	}
	raw, ver, err := api.PolicyRaw(ctx, s.API, s.repoID(owner, repo))
	out := &SettingsPolicy{Text: string(raw), Exists: ver != ""}
	if err != nil {
		// Keep the bytes visible (the operator must fix them); the parse
		// error renders inline, exactly like the API's fail-closed GET.
		out.Error = err.Error()
	}
	return out, nil
}

// repoProtection reads the SAME stored policy the API enforces and projects
// it into the Branch protection tab: every named ruleset (the pre-ruleset
// names read as "default"), the read-only rule table, and the raw document.
func (s SettingsServices) repoProtection(ctx context.Context, owner, repo string) (*SettingsProtection, error) {
	if s.API == nil || s.API.Store == nil {
		return nil, nil
	}
	raw, _, err := api.PolicyRaw(ctx, s.API, s.repoID(owner, repo))
	if err != nil {
		out := &SettingsProtection{
			Owner: owner, Repo: repo, Roles: settingsProtectionRoles,
			Methods: settingsMergeMethodOptions, Raw: string(raw),
		}
		out.Error = err.Error()
		return out, nil
	}
	return settingsProtectionView(raw, owner, repo), nil
}

func (s SettingsServices) repoConfig(ctx context.Context, owner, repo string) (*SettingsConfig, error) {
	if s.API == nil || s.API.Repo == nil {
		return nil, nil
	}
	id := s.repoID(owner, repo)
	doc, err := s.API.Repo.Settings(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &SettingsConfig{Text: doc.TOML}
	hist, err := s.API.Repo.SettingsHistory(ctx, id)
	if err != nil {
		return nil, err
	}
	entries := hist.Entries
	if len(entries) > settingsHistoryLimit {
		entries = entries[len(entries)-settingsHistoryLimit:]
	}
	// Newest first, each diffed against the current published document (the
	// SPA's history view).
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		out.Revisions = append(out.Revisions, SettingsRevision{
			Rev:     e.Revision,
			Author:  e.Author,
			At:      rfc3339(e.At),
			Message: e.Message,
			TOML:    e.TOML,
			Diff:    settingsLineDiff(e.TOML, doc.TOML),
		})
		if len(out.Revisions) >= settingsHistoryLimit {
			break
		}
	}
	return out, nil
}

func (s SettingsServices) repoAccess(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAccess, error) {
	role, _ := s.Ident.Resolve(ctx, owner, repo, p)
	out := &SettingsAccess{Role: string(role), Roles: settingsAccessRoles}
	doc, _, err := s.Ident.GetAccess(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out.Visibility = string(doc.Visibility)
	out.Version = uint64(doc.Version)
	for _, b := range doc.RoleBindings {
		out.Bindings = append(out.Bindings, SettingsBinding{Subject: b.Subject, Role: string(b.Role)})
	}
	for _, c := range s.Ident.Collaborators(ctx, owner, repo) {
		out.Collaborators = append(out.Collaborators, SettingsCollaborator{
			Principal: c.Principal, Role: c.Role, Source: c.Source,
		})
	}
	// Pending collaborator invitations (G9) are an ADMIN read: the list
	// carries each invite's capability token, so a triage reader never
	// triggers the read and never receives a token. The page gate (Access
	// MinRead triage) is therefore still safe — the panel renders read-only
	// for non-admins.
	if role == identity.RoleAdmin || p.Admin {
		out.InvitesWired = true
		invs, ierr := s.Ident.ListRepoInvites(ctx, owner, repo, 100)
		if ierr != nil {
			return nil, ierr
		}
		now := s.now().UTC()
		for _, inv := range invs {
			row := SettingsInvite{
				ID: inv.ID, Subject: inv.Subject, Role: inv.Role,
				InvitedBy: inv.InvitedBy, ExpiresAt: inv.ExpiresAt,
				AcceptURL: InvitePageURL(inv.ID, inv.Token),
			}
			if exp, perr := time.Parse(time.RFC3339, inv.ExpiresAt); !exp.IsZero() && perr == nil && !now.Before(exp) {
				row.Expired = true
			}
			out.Invites = append(out.Invites, row)
		}
	}
	// The owner kind decides the private-visibility wording and whether the
	// team picker renders (org-owned namespaces only).
	if org, oerr := s.Ident.GetOrg(ctx, owner); oerr == nil && org != nil {
		out.IsOrg = true
		if teams, terr := s.Ident.ListTeams(ctx, owner, 100); terr == nil {
			for _, t := range teams {
				if t == nil {
					continue
				}
				out.Teams = append(out.Teams, SettingsOption{ID: t.Slug, Label: t.Name})
			}
		}
	}
	return out, nil
}

func (s SettingsServices) repoAdmin(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsAdmin, error) {
	out := &SettingsAdmin{}
	if s.Checks != nil {
		tokens, err := s.Checks.ListTokens(ctx, owner, repo, p)
		if err != nil {
			return nil, err
		}
		out.TokensWired = true
		for _, t := range tokens {
			row := SettingsToken{
				ID: t.ID, Name: t.Name, Scopes: strings.Join(t.Scopes, ", "),
				CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt,
			}
			if t.RevokedAt != nil {
				row.RevokedAt = *t.RevokedAt
			}
			out.Tokens = append(out.Tokens, row)
		}
	}
	if s.Notify != nil {
		hooks, err := s.Notify.ListHooks(ctx, owner, repo)
		if err != nil {
			return nil, err
		}
		out.WebhooksWired = true
		for _, h := range hooks {
			row := SettingsWebhook{
				ID: h.ID, URL: h.URL, Events: strings.Join(h.Events, ", "),
				Active: h.Active, SecretSet: h.Secret != "",
			}
			if d := s.Notify.ReadDeliveries(ctx, owner, repo, h.ID); d != nil && len(d.Entries) > 0 {
				start := len(d.Entries) - settingsDeliveryLimit
				if start < 0 {
					start = 0
				}
				for _, e := range d.Entries[start:] {
					row.Recent = append(row.Recent, SettingsDelivery{
						Seq: strconv.Itoa(e.Seq), SeqNum: e.Seq, Event: e.Event, Status: e.Status,
						At: e.At, Error: e.Error, DurationMs: e.DurationMs,
						Redeliverable: len(e.Body) > 0,
					})
				}
				last := d.Entries[len(d.Entries)-1]
				row.LastPing = "last delivery: " + last.Event + " → " + strconv.Itoa(last.Status) + " at " + last.At
			}
			out.Webhooks = append(out.Webhooks, row)
		}
	}
	sort.SliceStable(out.Webhooks, func(i, j int) bool { return out.Webhooks[i].ID < out.Webhooks[j].ID })
	if s.Threads != nil {
		secrets, err := s.Threads.ListSecrets(ctx, owner+"/"+repo, p)
		if err != nil {
			return nil, err
		}
		out.SecretsWired = true
		out.SecretsEnabled = s.Threads.SecretsEnabled()
		for _, sec := range secrets {
			out.Secrets = append(out.Secrets, SettingsSecret{Name: sec.Name, CreatedAt: sec.CreatedAt, UpdatedAt: sec.UpdatedAt})
		}
	}
	return out, nil
}

// --- writes ------------------------------------------------------------------

func (s SettingsServices) repoWrite(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.API == nil && s.Ident == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings writes are not configured")
	}
	// Every repo settings mutation re-checks the admin gate at the seam (the
	// page checked it, but the seam is public) — the same rule the API's
	// AuthAdmin routes apply. Deployment environments are the exception and
	// carry the same maintain floor as their service methods
	// (docs/PLAN-THREADS-ENVIRONMENTS.md §7).
	seamRole := identity.RoleAdmin
	switch w.Action {
	case SettingsActionEnvironmentPut, SettingsActionEnvironmentDelete,
		SettingsActionEnvironmentSecretPut, SettingsActionEnvironmentSecretDelete:
		seamRole = identity.RoleMaintain
	}
	if s.Ident != nil {
		if aerr := s.Ident.CheckRole(ctx, owner, repo, p, seamRole); aerr != nil {
			return SettingsWriteResult{}, settingsAuthErr(aerr)
		}
	} else if !p.Admin && seamRole == identity.RoleAdmin {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusForbidden, "admin access required")
	}
	id := s.repoID(owner, repo)
	switch w.Action {
	case SettingsActionGeneral:
		return s.writeGeneral(ctx, id, p, w)
	case SettingsActionScheduled:
		return s.writeScheduled(ctx, owner, repo, p, w)
	case SettingsActionAccess:
		return s.writeAccess(ctx, owner, repo, w)
	case SettingsActionInviteCreate:
		return s.createRepoInvite(ctx, owner, repo, p, w)
	case SettingsActionInviteCancel:
		return s.cancelRepoInvite(ctx, owner, repo, w)
	case SettingsActionPolicy:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy store not configured")
		}
		// An emptied editor expresses removal: `{}`/blank is not a valid
		// policy document (version 1 is required), so Save on an empty
		// textarea removes the document instead of answering 400.
		if strings.TrimSpace(w.Get("policy")) == "" {
			if werr := api.DeletePolicy(ctx, s.API, id); werr != nil {
				return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
			}
			return SettingsWriteResult{Notice: "policy removed — every push is allowed"}, nil
		}
		if werr := api.PutPolicy(ctx, s.API, id, []byte(w.Get("policy"))); werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "policy saved"}, nil
	case SettingsActionPolicyRemove:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy store not configured")
		}
		if werr := api.DeletePolicy(ctx, s.API, id); werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "policy removed — every push is allowed"}, nil
	case SettingsActionProtection:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy store not configured")
		}
		cur, _, _ := api.PolicyRaw(ctx, s.API, id)
		in := settingsRulesetInputFrom(w)
		in.Owner, in.Repo = owner, repo
		// A ruleset delete is destructive: the row's typed confirmation names
		// the pair, and the server re-checks it (the disabled button is only
		// an affordance, never the boundary). Unchecking "enabled" needs no
		// arm — it is the plain form and it removes exactly the same bytes.
		if in.Action == "delete" && w.Get("confirm") != "protection:"+in.Slug {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusBadRequest,
				"type protection:"+in.Slug+" exactly to confirm removal")
		}
		doc, warnings, berr := settingsProtectionApply(cur, owner, repo, in)
		if berr != nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusBadRequest, berr.Error())
		}
		if werr := api.PutPolicy(ctx, s.API, id, doc); werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		notice := settingsProtectionNotice(in)
		if len(warnings) > 0 {
			notice += " — " + strings.Join(warnings, "; ")
		}
		res := SettingsWriteResult{Notice: notice}
		// Saving a ruleset returns to its form (the list is one hop away);
		// every other verb lands on the list. The notice rides the redirect
		// the handler appends, so it is not repeated here.
		if in.Action == "save" && in.Enabled {
			slug := in.NewSlug
			if slug == "" {
				slug = in.Slug
			}
			res.Redirect = "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
				"/settings/protection/" + url.PathEscape(slug)
		}
		return res, nil
	case SettingsActionPolicyDryRun:
		if s.API == nil || s.API.Repo == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy evaluation not configured")
		}
		n := 10
		if v, err := strconv.Atoi(w.Get("n")); err == nil {
			n = v
		}
		res, werr := api.DryRunPolicy(ctx, s.API, id, []byte(w.Get("policy")), n)
		if werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		// A dry run evaluates the POSTED document and changes nothing, so the
		// stored-policy facts must survive the verdict: without Exists the
		// Remove card would claim "No policy document is stored" while the
		// document is live, and the typed-confirm removal would vanish.
		_, ver, _ := api.PolicyRaw(ctx, s.API, id)
		panel := &SettingsPolicy{
			Text: w.Get("policy"), Exists: ver != "", DryRunN: n, DryRunDone: true,
			DryRunAllow: res.Allowed, DryRunDeny: res.Denied,
			DryRunRefs: settingsDryRunRows(res),
		}
		if len(res.Warnings) > 0 {
			panel.Note = strings.Join(res.Warnings, "; ")
		}
		return SettingsWriteResult{Policy: panel}, nil
	case SettingsActionConfig:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings store not configured")
		}
		rev, werr := api.PublishSettings(ctx, s.API, id, []byte(w.Get("config")), w.Get("message"), p.Name)
		if werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "published revision #" + strconv.FormatUint(rev, 10)}, nil
	case SettingsActionConfigClear:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings store not configured")
		}
		rev, werr := api.DeleteSettings(ctx, s.API, id, p.Name)
		if werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "cleared — back to host config at #" + strconv.FormatUint(rev, 10)}, nil
	case SettingsActionMirrorPut, SettingsActionMirrorSchedule:
		return s.writeMirror(ctx, owner, repo, w)
	case SettingsActionMirrorSync:
		return s.syncMirror(ctx, owner, repo, w)
	case SettingsActionMirrorRemove:
		return s.removeMirror(ctx, owner, repo)
	case SettingsActionPushPut, SettingsActionPushSchedule:
		return s.writePush(ctx, owner, repo, w)
	case SettingsActionPushKeygen:
		return s.keygenPush(ctx, owner, repo, w)
	case SettingsActionPushSync:
		return s.syncPush(ctx, owner, repo, w)
	case SettingsActionPushRemove:
		return s.removePush(ctx, owner, repo)
	case SettingsActionTokenCreate:
		return s.createToken(ctx, owner, repo, p, w)
	case SettingsActionTokenRevoke:
		return s.revokeToken(ctx, owner, repo, p, w)
	case SettingsActionSecretPut:
		return s.putSecret(ctx, owner, repo, p, w)
	case SettingsActionSecretDelete:
		return s.deleteSecret(ctx, owner, repo, p, w)
	case SettingsActionEnvironmentPut:
		return s.putEnvironment(ctx, owner, repo, p, w)
	case SettingsActionEnvironmentDelete:
		return s.deleteEnvironment(ctx, owner, repo, p, w)
	case SettingsActionEnvironmentSecretPut:
		return s.putEnvironmentSecret(ctx, owner, repo, p, w)
	case SettingsActionEnvironmentSecretDelete:
		return s.deleteEnvironmentSecret(ctx, owner, repo, p, w)
	case SettingsActionWebhookCreate:
		return s.createWebhook(ctx, owner, repo, p, w)
	case SettingsActionWebhookUpdate:
		return s.updateWebhook(ctx, owner, repo, w)
	case SettingsActionWebhookRotate:
		return s.rotateWebhookSecret(ctx, owner, repo, w)
	case SettingsActionWebhookDelete:
		return s.deleteWebhook(ctx, owner, repo, w)
	case SettingsActionWebhookPing:
		return s.pingWebhook(ctx, owner, repo, p, w)
	case SettingsActionWebhookToggle:
		return s.toggleWebhook(ctx, owner, repo, w)
	case SettingsActionWebhookRedeliver:
		return s.redeliverWebhook(ctx, owner, repo, w)
	case SettingsActionTransfer:
		return s.transferRepo(ctx, owner, repo, p, w)
	case SettingsActionDelete:
		return s.deleteRepo(ctx, repo, id)
	}
	return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "unknown settings action "+w.Action)
}

func settingsDryRunRows(res *api.DryRunBody) []SettingsKV {
	if res == nil {
		return nil
	}
	out := make([]SettingsKV, 0, len(res.Results))
	for _, r := range res.Results {
		for _, ref := range r.Refs {
			detail := "allowed"
			if !ref.OK {
				detail = ref.Reason
			}
			out = append(out, SettingsKV{
				Key:   "#" + strconv.FormatUint(r.Seq, 10) + " " + ref.Name,
				Value: detail,
			})
		}
	}
	return out
}

// writeGeneral edits the passenger keys of the settings TOML (the SPA's
// withDescription/withWebsite/withFeatures/withSelfApproval helpers, ported)
// and publishes the document — every other section passes through untouched.
func (s SettingsServices) writeGeneral(ctx context.Context, id git.RepoId, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.API == nil || s.API.Repo == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings store not configured")
	}
	doc, err := s.API.Repo.Settings(ctx, id)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsRepoErrStatus(err), err.Error())
	}
	desc := w.Get("description")
	if msg := settingsValidateDescription(desc); msg != "" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, msg)
	}
	website := w.Get("website")
	if err := config.ValidateRepoWebsite(website); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, err.Error())
	}
	text := settingsWithWebsite(doc.TOML, website)
	text = settingsWithDescription(text, desc)
	if _, ok := w.Values["feature_issues"]; ok {
		flags := map[string]bool{}
		for _, f := range settingsFeatureDefs {
			flags[f.Key] = w.Get("feature_"+f.Key) == "true"
		}
		text = settingsWithFeatures(text, flags)
	}
	text = settingsWithSelfApproval(text, w.Get("allow_self_approval") == "true")
	if _, werr := api.PublishSettings(ctx, s.API, id, []byte(text), "", p.Name); werr != nil {
		return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
	}
	return SettingsWriteResult{Notice: "saved"}, nil
}

func settingsValidateDescription(d string) string {
	if strings.ContainsAny(d, "\r\n\x00") {
		return "description must be a single line"
	}
	if n := len([]rune(d)); n > config.MaxRepoDescriptionRunes {
		return fmt.Sprintf("description is %d characters; limit is %d", n, config.MaxRepoDescriptionRunes)
	}
	return ""
}

// writeAccess replaces access.json (full-document PUT with the CAS version
// the form carried) — the identity route's exact shape.
func (s SettingsServices) writeAccess(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	cur, curVer, err := s.Ident.GetAccess(ctx, owner, repo)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, err.Error())
	}
	// The API's CAS rule: the body version must equal the stored doc version
	// (an omitted version reads as 0).
	want, perr := strconv.Atoi(strings.TrimSpace(w.Get("version")))
	if strings.TrimSpace(w.Get("version")) == "" {
		want, perr = 0, nil
	}
	if perr != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "version must be a number")
	}
	if want != cur.Version {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusConflict, "access.json changed under you; reload")
	}
	var bindings []identity.AccessBinding
	n := 0
	if v, err := strconv.Atoi(w.Get("binding_count")); err == nil {
		n = v
	}
	for i := 0; i < n; i++ {
		if w.Get(fmt.Sprintf("binding_remove_%d", i)) == "true" {
			continue
		}
		subject := strings.TrimSpace(w.Get(fmt.Sprintf("binding_subject_%d", i)))
		if subject == "" {
			continue
		}
		bindings = append(bindings, identity.AccessBinding{
			Subject: subject,
			Role:    identity.Role(w.Get(fmt.Sprintf("binding_role_%d", i))),
		})
	}
	if subject := strings.TrimSpace(w.Get("new_subject")); subject != "" {
		bindings = append(bindings, identity.AccessBinding{
			Subject: subject,
			Role:    identity.Role(defaultStr(w.Get("new_role"), "read")),
		})
	} else if team := strings.TrimSpace(w.Get("new_team")); team != "" {
		bindings = append(bindings, identity.AccessBinding{
			Subject: "team:" + owner + "/" + team,
			Role:    identity.Role(defaultStr(w.Get("new_role"), "read")),
		})
	}
	vis := identity.Visibility(defaultStr(w.Get("visibility"), string(cur.Visibility)))
	out, perr := s.Ident.PutAccess(ctx, owner, repo, curVer, vis, bindings)
	if perr != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsIdentityStatus(perr), perr.Error())
	}
	return SettingsWriteResult{Notice: "saved (version " + strconv.Itoa(out.Version) + ")"}, nil
}

// createRepoInvite issues one collaborator invitation (G9). The admin role
// was re-checked by repoWrite (CheckRole(admin), the API's route floor); the
// service validates the subject and role floor, so the page rejects exactly
// what POST …/invitations rejects. The returned Link is the human accept page
// (/invitations?id=&token=) — the same target the invitee inbox previews.
func (s SettingsServices) createRepoInvite(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Ident == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "identity service not configured")
	}
	inv, err := s.Ident.CreateRepoInvite(ctx, owner, repo, w.Get("subject"), identity.Role(w.Get("role")), p.Name, 7*24*time.Hour)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsIdentityStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "invitation created — share the link below", Link: InvitePageURL(inv.ID, inv.Token)}, nil
}

// cancelRepoInvite withdraws one pending repo invitation. Delete-on-
// transition: the issuer object and the invitee's inbox row both go away, so
// the accept link answers 409 afterwards.
func (s SettingsServices) cancelRepoInvite(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Ident == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "identity service not configured")
	}
	id := strings.TrimSpace(w.Get("id"))
	if id == "" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusBadRequest, "invitation id is required")
	}
	if _, err := s.Ident.DeleteRepoInvite(ctx, owner, repo, id); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsIdentityStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "invitation cancelled"}, nil
}

// InvitePageURL is the human accept link: /invitations carries the token-
// authorized preview and the accept form, so a copied link works for the
// invitee after sign-in (never the raw API path). Both scopes share it (repo
// create, org create) and the page renders it in the body, never a redirect.
func InvitePageURL(id, token string) string {
	return "/invitations?id=" + url.QueryEscape(id) + "&token=" + url.QueryEscape(token)
}

func (s SettingsServices) writeMirror(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Mirror == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "mirror service not configured")
	}
	view, created, taskID, cerr := s.Mirror.Configure(ctx, owner, repo, mirror.ConfigInput{
		UpstreamURL: strings.TrimSpace(w.Get("upstream_url")),
		Schedule:    w.Get("schedule"),
	})
	if cerr != nil {
		return SettingsWriteResult{}, settingsWriteErr(cerr.Status, cerr.Message)
	}
	_ = view
	if created {
		return SettingsWriteResult{Notice: "mirror created — first sync started (task " + taskID + ")"}, nil
	}
	return SettingsWriteResult{Notice: "mirror schedule saved"}, nil
}

func (s SettingsServices) syncMirror(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Mirror == nil || s.Mirror.Svc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "mirror service not configured")
	}
	doc, _, err := mirror.Load(ctx, s.Mirror.Svc.Store(), owner, repo)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, err.Error())
	}
	if doc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusNotFound, "not a mirror")
	}
	start := time.Now()
	id := s.Mirror.Svc.SyncAsync(ctx, owner, repo, w.Get("token"), w.Get("force") == "true")
	// The click answers with the fire's own card (law 7: a sync is long work,
	// so it is a task with progress, not a toast); the notice stays the no-JS
	// fallback and names the async id the mirror API resolves.
	notice := "sync started (task " + id + ")"
	rec, found := s.syncTaskRecord(ctx, owner, repo, mirror.KindMirrorSync, start)
	return SettingsWriteResult{
		Notice: notice,
		Task:   settingsSyncTask(owner, repo, mirror.KindMirrorSync, "syncing from "+strings.TrimSpace(doc.UpstreamURL), rec, found),
	}, nil
}

func (s SettingsServices) removeMirror(ctx context.Context, owner, repo string) (SettingsWriteResult, *SettingsWriteError) {
	if s.Mirror == nil || s.Mirror.Svc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "mirror service not configured")
	}
	st := s.Mirror.Svc.Store()
	doc, _, err := mirror.Load(ctx, st, owner, repo)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, err.Error())
	}
	if doc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusNotFound, "not a mirror")
	}
	if derr := mirror.Delete(ctx, st, owner, repo); derr != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, derr.Error())
	}
	return SettingsWriteResult{Notice: "mirror removed"}, nil
}

func (s SettingsServices) pushConfigInput(w SettingsRepoWrite) pushmirror.ConfigInput {
	schedule := w.Get("schedule")
	known := w.Get("ssh_known_hosts")
	return pushmirror.ConfigInput{
		UpstreamURL:   strings.TrimSpace(w.Get("upstream_url")),
		AuthKind:      w.Get("auth_kind"),
		Username:      w.Get("username"),
		Password:      w.Get("password"),
		Token:         w.Get("token"),
		SSHPrivateKey: w.Get("ssh_private_key"),
		Schedule:      &schedule,
		SSHKnownHosts: &known,
	}
}

func (s SettingsServices) writePush(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Push == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "push-mirror service not configured")
	}
	if _, cerr := s.Push.Configure(ctx, owner, repo, s.pushConfigInput(w)); cerr != nil {
		return SettingsWriteResult{}, settingsWriteErr(cerr.Status, cerr.Message)
	}
	return SettingsWriteResult{Notice: "push mirror saved"}, nil
}

func (s SettingsServices) keygenPush(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Push == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "push-mirror service not configured")
	}
	key, cerr := s.Push.Keygen(ctx, owner, repo, w.Get("ssh_known_hosts"))
	if cerr != nil {
		return SettingsWriteResult{}, settingsWriteErr(cerr.Status, cerr.Message)
	}
	return SettingsWriteResult{Notice: "deploy key generated (" + key.Fingerprint + ") — install the public key upstream"}, nil
}

func (s SettingsServices) syncPush(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Push == nil || s.Push.Svc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "push-mirror service not configured")
	}
	doc, _, err := pushmirror.Load(ctx, s.Push.Svc.Store(), owner, repo)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, err.Error())
	}
	if doc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusNotFound, "no push mirror configured")
	}
	start := time.Now()
	id := s.Push.Svc.SyncAsync(ctx, owner, repo, w.Get("force") == "true")
	notice := "push sync started (task " + id + ")"
	rec, found := s.syncTaskRecord(ctx, owner, repo, pushmirror.KindPushMirrorSync, start)
	return SettingsWriteResult{
		Notice: notice,
		Task:   settingsSyncTask(owner, repo, pushmirror.KindPushMirrorSync, "pushing to "+strings.TrimSpace(doc.UpstreamURL), rec, found),
	}, nil
}

func (s SettingsServices) removePush(ctx context.Context, owner, repo string) (SettingsWriteResult, *SettingsWriteError) {
	if s.Push == nil || s.Push.Svc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "push-mirror service not configured")
	}
	st := s.Push.Svc.Store()
	doc, _, err := pushmirror.Load(ctx, st, owner, repo)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, err.Error())
	}
	if doc == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusNotFound, "no push mirror configured")
	}
	if derr := pushmirror.Delete(ctx, st, owner, repo); derr != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, derr.Error())
	}
	return SettingsWriteResult{Notice: "push mirror removed"}, nil
}

func (s SettingsServices) createToken(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Checks == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "checks service not configured")
	}
	created, err := s.Checks.CreateToken(ctx, owner, repo, p, w.Get("name"), nil)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsChecksStatus(err), err.Error())
	}
	return SettingsWriteResult{
		Secret: created.Token,
		Notice: "CI token created — copy it now, it is shown once",
	}, nil
}

func (s SettingsServices) revokeToken(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Checks == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "checks service not configured")
	}
	if err := s.Checks.RevokeToken(ctx, owner, repo, w.Get("id"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsChecksStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "token revoked"}, nil
}

func (s SettingsServices) putSecret(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Threads == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	if err := s.Threads.SetSecret(ctx, owner+"/"+repo, w.Get("secret_name"), w.Get("secret_value"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "secret stored (write-only — the value can never be read back)"}, nil
}

func (s SettingsServices) deleteSecret(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Threads == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	if err := s.Threads.DeleteSecret(ctx, owner+"/"+repo, w.Get("id"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "secret deleted"}, nil
}

// repoEnvironments is the Settings → Environments read: one list call plus one
// secret-name list per environment (values never leave the store). Every call
// is the same service method the JSON API uses, so the page and its API twin
// share one role floor (maintain).
func (s SettingsServices) repoEnvironments(ctx context.Context, owner, repo string, p auth.Principal) (*SettingsEnvironmentPanel, error) {
	envs := s.environments()
	if envs == nil {
		return nil, nil
	}
	list, err := envs.ListEnvironments(ctx, owner+"/"+repo, p)
	if err != nil {
		return nil, err
	}
	out := &SettingsEnvironmentPanel{Wired: true, Enabled: envs.SecretsEnabled()}
	for _, env := range list {
		row := SettingsEnvironment{
			Name: env.Name, Reviewers: env.RequiredReviewers,
			ReviewersText: strings.Join(env.RequiredReviewers, ", "),
			CreatedAt:     env.CreatedAt, UpdatedAt: env.UpdatedAt,
		}
		secrets, serr := envs.ListEnvironmentSecrets(ctx, owner+"/"+repo, env.Name, p)
		if serr != nil {
			return nil, serr
		}
		for _, sec := range secrets {
			row.Secrets = append(row.Secrets, SettingsSecret{Name: sec.Name, CreatedAt: sec.CreatedAt, UpdatedAt: sec.UpdatedAt})
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// putEnvironment creates or replaces one environment record (maintain floor,
// re-checked by the service).
func (s SettingsServices) putEnvironment(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	envs := s.environments()
	if envs == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	reviewers := settingsReviewerList(w.Get("environment_reviewers"))
	if _, err := envs.PutEnvironment(ctx, owner+"/"+repo, w.Get("environment_name"),
		threads.EnvironmentInput{RequiredReviewers: reviewers}, p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "environment saved"}, nil
}

// deleteEnvironment removes one environment (and its sealed secrets).
func (s SettingsServices) deleteEnvironment(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	envs := s.environments()
	if envs == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	if err := envs.DeleteEnvironment(ctx, owner+"/"+repo, w.Get("id"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "environment deleted"}, nil
}

// putEnvironmentSecret stores one environment-scoped secret (write-only).
func (s SettingsServices) putEnvironmentSecret(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	envs := s.environments()
	if envs == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	if err := envs.SetEnvironmentSecret(ctx, owner+"/"+repo, w.Get("id"),
		w.Get("environment_secret_name"), w.Get("environment_secret_value"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "environment secret stored (write-only — the value can never be read back)"}, nil
}

// deleteEnvironmentSecret removes one environment-scoped secret.
func (s SettingsServices) deleteEnvironmentSecret(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	envs := s.environments()
	if envs == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "the Threads service is not configured")
	}
	if err := envs.DeleteEnvironmentSecret(ctx, owner+"/"+repo, w.Get("id"), w.Get("environment_secret_name"), p); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsThreadsStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "environment secret deleted"}, nil
}

// settingsReviewerList splits the reviewers textarea/CSV into names.
func settingsReviewerList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == ';' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s SettingsServices) createWebhook(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	url := strings.TrimSpace(w.Get("url"))
	if url == "" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "url is required")
	}
	spec := notify.HookSpec{URL: &url}
	if events := settingsEventList(w.Get("events")); len(events) > 0 {
		spec.Events = events
	}
	if secret := w.Get("secret"); secret != "" {
		spec.Secret = &secret
	}
	if _, err := s.Notify.CreateHook(ctx, owner, repo, p.Name, spec); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "webhook created"}, nil
}

// settingsEventList splits the comma list ("*" = everything, "" = all).
func settingsEventList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// updateWebhook edits a hook's URL, event filter and active flag through the
// SAME notify.PatchHook call the JSON PATCH route makes. The secret is never
// part of this form: rotation is its own control (rotateWebhookSecret).
func (s SettingsServices) updateWebhook(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	url := strings.TrimSpace(w.Get("url"))
	if url == "" {
		return SettingsWriteResult{}, settingsWriteErrField(http.StatusUnprocessableEntity, "url", "url is required")
	}
	events := settingsEventList(w.Get("events"))
	if events == nil {
		events = []string{} // empty means all; a non-nil empty slice clears the filter
	}
	active := w.Get("active") == "true"
	spec := notify.HookSpec{URL: &url, Events: events, Active: &active}
	if _, err := s.Notify.PatchHook(ctx, owner, repo, w.Get("id"), spec); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "webhook updated"}, nil
}

// rotateWebhookSecret replaces only the signing secret — the first-class
// rotation control (a blank secret is a validation error, not a silent keep).
func (s SettingsServices) rotateWebhookSecret(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	secret := w.Get("secret")
	if strings.TrimSpace(secret) == "" {
		return SettingsWriteResult{}, settingsWriteErrField(http.StatusUnprocessableEntity, "secret", "enter the new signing secret to rotate it")
	}
	if _, err := s.Notify.PatchHook(ctx, owner, repo, w.Get("id"), notify.HookSpec{Secret: &secret}); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "webhook signing secret rotated"}, nil
}

func (s SettingsServices) deleteWebhook(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	if err := s.Notify.DeleteHook(ctx, owner, repo, w.Get("id")); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "webhook deleted"}, nil
}

func (s SettingsServices) pingWebhook(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	delivered, err := s.Notify.PingHook(ctx, owner, repo, w.Get("id"), p.Name)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	if !delivered {
		return SettingsWriteResult{Notice: "ping failed — the delivery row carries the detail"}, nil
	}
	return SettingsWriteResult{Notice: "ping delivered"}, nil
}

// toggleWebhook enables/disables one hook without touching its secret
// (PATCH active only; the same CAS the API uses).
func (s SettingsServices) toggleWebhook(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	h := s.Notify.GetHook(ctx, owner, repo, w.Get("id"))
	if h == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusNotFound, "unknown webhook")
	}
	active := !h.Active
	if _, err := s.Notify.PatchHook(ctx, owner, repo, h.ID, notify.HookSpec{Active: &active}); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	if active {
		return SettingsWriteResult{Notice: "webhook enabled"}, nil
	}
	return SettingsWriteResult{Notice: "webhook disabled"}, nil
}

// rotateWebhook replaces the HMAC secret (write-only; never echoed). An empty
// submission is rejected instead of silently clearing the secret.
func (s SettingsServices) rotateWebhook(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	secret := w.Get("secret")
	if secret == "" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "a new secret is required")
	}
	if _, err := s.Notify.PatchHook(ctx, owner, repo, w.Get("id"), notify.HookSpec{Secret: &secret}); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "webhook secret rotated"}, nil
}

// redeliverWebhook re-sends one retained delivery (no cursor change). A row
// whose payload the ring did not retain answers 410 with the honest reason.
func (s SettingsServices) redeliverWebhook(ctx context.Context, owner, repo string, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Notify == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "notifications service not configured")
	}
	seq, err := strconv.Atoi(w.Get("seq"))
	if err != nil || seq < 0 {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "a delivery seq is required")
	}
	entry, rerr := s.Notify.RedeliverHook(ctx, owner, repo, w.Get("id"), seq)
	if rerr != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsNotifyStatus(rerr), rerr.Error())
	}
	return SettingsWriteResult{Notice: "redelivered #" + strconv.Itoa(entry.Seq) + " → " + strconv.Itoa(entry.Status)}, nil
}

// transferRepo starts the `repo-transfer` task (PLAN-V2 §3 T.1) and answers
// the card that follows its progress. It never blocks on the move: a large
// repository takes minutes, the settings request does not, and the source page
// only exists until the move commits — so the result is a live task card whose
// success target is the destination, not a redirect.
func (s SettingsServices) transferRepo(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.Ident == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "identity service not configured")
	}
	dest := strings.TrimSpace(w.Get("owner"))
	if dest == "" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "destination owner is required")
	}
	dstOwner, dstRepo := dest, repo
	if o, n, ok := strings.Cut(dest, "/"); ok {
		dstOwner, dstRepo = strings.TrimSpace(o), strings.TrimSpace(n)
	}
	if aerr := s.Ident.CheckCreateOwner(ctx, dstOwner, p); aerr != nil {
		return SettingsWriteResult{}, settingsAuthErr(aerr)
	}
	rec, joined, err := s.Ident.BeginTransfer(ctx, identity.TransferRequest{
		SrcOwner: owner, SrcRepo: repo, DstOwner: dstOwner, DstRepo: dstRepo,
	})
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsIdentityStatus(err), err.Error())
	}
	notice := "transfer started — the task moves and verifies the repository, then opens the new path"
	if joined {
		notice = "a transfer of this repository is already running — attached to its progress"
	}
	return SettingsWriteResult{
		Notice: notice,
		Task: &SettingsTask{
			ID:     rec.ID,
			Kind:   rec.Kind,
			Stream: "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/api/tasks/" + url.PathEscape(rec.ID),
			Next:   "/" + url.PathEscape(dstOwner) + "/" + url.PathEscape(dstRepo),
			Title:  "transferring to " + dstOwner + "/" + dstRepo,
		},
	}, nil
}

func (s SettingsServices) deleteRepo(ctx context.Context, repo string, id git.RepoId) (SettingsWriteResult, *SettingsWriteError) {
	if s.API == nil || s.API.Repos == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "repo registry not configured")
	}
	if err := s.API.Repos.Delete(ctx, id); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsRepoErrStatus(err), err.Error())
	}
	return SettingsWriteResult{Notice: "repository " + id.Owner + "/" + repo + " deleted", Redirect: "/"}, nil
}

// --- error mapping (the handlers' status rules) ------------------------------

func settingsWriteErr(status int, msg string) *SettingsWriteError {
	return &SettingsWriteError{Status: status, Message: msg}
}

// settingsAuthErr maps identity's *auth.AuthError the way every handler does.
// settingsThreadsStatus maps a Threads error onto the page's write status.
func settingsThreadsStatus(err error) int {
	switch {
	case errors.Is(err, threads.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, threads.ErrInvalid):
		return http.StatusUnprocessableEntity
	case errors.Is(err, threads.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, threads.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, threads.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}

func settingsAuthErr(aerr *auth.AuthError) *SettingsWriteError {
	switch aerr.Kind {
	case auth.ErrForbidden:
		return settingsWriteErr(http.StatusForbidden, aerr.Why)
	case auth.ErrUnavailable:
		return settingsWriteErr(http.StatusServiceUnavailable, aerr.Why)
	default:
		return settingsWriteErr(http.StatusUnauthorized, aerr.Why)
	}
}

// settingsRepoErrStatus maps a RepoView error (api.mapViewErr's rule).
func settingsRepoErrStatus(err error) int {
	switch {
	case errors.Is(err, api.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, api.ErrExists):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

// settingsIdentityStatus maps identity's sentinels (identity.statusFor).
func settingsIdentityStatus(err error) int {
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

// settingsChecksStatus maps checks' sentinels (checks.statusFor).
func settingsChecksStatus(err error) int {
	switch {
	case errors.Is(err, checks.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, checks.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, checks.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, checks.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, checks.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}

// settingsNotifyStatus maps notify's sentinels (notify.statusFor, which
// matches by containment rather than errors.Is).
func settingsNotifyStatus(err error) int {
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

// --- TOML passenger editors (ported from web/src/lib/repo*.js) ---------------

// settingsFeatureKeys is the [features] key order (config.ResolvedFeatures).
var settingsFeatureKeys = []string{"issues", "pulls", "releases", "forks", "watch", "star", "packages"}

// settingsTopLevelRegion is the line index where the first table header
// starts: a top-level key can only live before it.
func settingsTopLevelRegion(lines []string) int {
	for i, line := range lines {
		if settingsTableHeader(line) != "" {
			return i
		}
	}
	return len(lines)
}

// settingsTableHeader returns the trimmed table name of a `[table]` line.
func settingsTableHeader(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return ""
	}
	if i := strings.Index(t, "]"); i > 0 {
		return strings.TrimSpace(t[1:i])
	}
	return ""
}

// settingsWithDescription sets the top-level description (basic string),
// leaving every section body untouched.
func settingsWithDescription(text, desc string) string {
	return settingsWithTopLevelString(text, "description", desc)
}

// settingsWithWebsite sets the top-level website (basic string) — an empty
// value clears it in place, exactly like the description (the key is written
// empty rather than deleted: the document the operator sees keeps its shape).
func settingsWithWebsite(text, website string) string {
	return settingsWithTopLevelString(text, "website", website)
}

// settingsWithTopLevelString replaces (or prepends) a top-level key. A
// top-level key can only live before the first [table] header: a
// section-nested lookalike is never read or rewritten (the description
// contract, issue #235).
func settingsWithTopLevelString(text, key, val string) string {
	line := key + ` = "` + settingsEscapeTomlBasic(val) + `"`
	if strings.TrimSpace(text) == "" {
		return line + "\n"
	}
	lines := strings.Split(text, "\n")
	end := settingsTopLevelRegion(lines)
	for i := 0; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, key) {
			continue
		}
		if strings.HasPrefix(t, key+" =") || strings.HasPrefix(t, key+"=") {
			lines[i] = line
			return strings.Join(lines, "\n")
		}
	}
	return line + "\n" + text
}

// settingsWithFeatures replaces (or appends) the [features] block.
func settingsWithFeatures(text string, flags map[string]bool) string {
	block := []string{"[features]"}
	for _, k := range settingsFeatureKeys {
		block = append(block, k+" = "+strconv.FormatBool(flags[k]))
	}
	if strings.TrimSpace(text) == "" {
		return strings.Join(block, "\n") + "\n"
	}
	lines := strings.Split(text, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		name := settingsTableHeader(line)
		if name == "" {
			continue
		}
		if name == "features" && start == -1 {
			start = i
			continue
		}
		if start != -1 {
			end = i
			break
		}
	}
	if start == -1 {
		sep := ""
		if !strings.HasSuffix(text, "\n") {
			sep = "\n"
		}
		return text + sep + strings.Join(block, "\n") + "\n"
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// settingsWithSelfApproval sets [review] allow_self_approval in place,
// appends it to an existing [review] block, or appends the block.
func settingsWithSelfApproval(text string, allowed bool) string {
	line := "allow_self_approval = " + strconv.FormatBool(allowed)
	if strings.TrimSpace(text) == "" {
		return "[review]\n" + line + "\n"
	}
	lines := strings.Split(text, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		name := settingsTableHeader(l)
		if name == "" {
			continue
		}
		if name == "review" && start == -1 {
			start = i
			continue
		}
		if start != -1 {
			end = i
			break
		}
	}
	if start == -1 {
		sep := ""
		if !strings.HasSuffix(text, "\n") {
			sep = "\n"
		}
		return text + sep + "[review]\n" + line + "\n"
	}
	for i := start + 1; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "allow_self_approval") {
			out := append([]string{}, lines[:i]...)
			out = append(out, line)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	out := append([]string{}, lines[:end]...)
	out = append(out, line)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// settingsEscapeTomlBasic escapes a value for a TOML basic string.
func settingsEscapeTomlBasic(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r < 0x20:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
