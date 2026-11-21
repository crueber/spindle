// settings_seams.go — the repo/org settings service adapters (D-WEB-14).
//
// The page layer never owns data: every read and write here is the SAME
// service call the JSON API handler makes, so a page and its API twin cannot
// drift. The one composition call
//
//	d := webui.SettingsDeps{ /* setup* fields from cmd/spindle/webui.go */ }
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
)

// SettingsServices is the concrete service set the settings group adapts.
// Every field is optional; nil disables the panels that depend on it.
type SettingsServices struct {
	API    *api.Env
	Ident  *identity.Service
	Checks *checks.Service
	Notify *notify.Service
	Mirror *mirror.Handler
	Push   *pushmirror.Handler

	// Now is the clock for derived timestamps (tests pin it).
	Now func() time.Time
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
		MaxLength:    config.MaxRepoDescriptionRunes,
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
	for _, st := range body.Strategies {
		out.Strategies = append(out.Strategies, SettingsStrategy{
			Name: st.Name, Kind: st.Kind, Base: st.Base,
			Schedule: st.Schedule, ScheduleHuman: st.ScheduleHuman, Next: st.Next,
			Keep: strconv.Itoa(st.Keep), Filter: st.Filter,
			Refs: strings.Join(st.Refs, ", "),
		})
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
	v := mirror.ViewOf(doc, s.now())
	out.Configured = true
	out.Upstream = v.UpstreamURL
	out.Schedule = v.Schedule
	out.NextSync = v.NextSyncAt
	out.LastSynced = v.LastSyncedAt
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
	v := pushmirror.ViewOf(doc, sec, s.now())
	out.Configured = true
	out.Upstream = v.UpstreamURL
	out.AuthKind = v.AuthKind
	out.AuthLabel = settingsPushAuthLabel(v.AuthKind)
	out.Username = v.Username
	out.HasSecret = v.HasSecret
	out.SecretHint = v.SecretHint
	out.Fingerprint = v.KeyFingerprint
	out.HostKey = v.HostKeyFingerprint
	out.NextSync = v.NextSyncAt
	out.LastSynced = v.LastSyncedAt
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
	raw, _, err := api.PolicyRaw(ctx, s.API, s.repoID(owner, repo))
	out := &SettingsPolicy{Text: string(raw)}
	if err != nil {
		// Keep the bytes visible (the operator must fix them); the parse
		// error renders inline, exactly like the API's fail-closed GET.
		out.Error = err.Error()
	}
	return out, nil
}

// repoProtection reads the SAME stored policy the API enforces and projects
// it into the Branch protection tab (the managed rules seed the form; every
// rule feeds the read-only effective view).
func (s SettingsServices) repoProtection(ctx context.Context, owner, repo string) (*SettingsProtection, error) {
	if s.API == nil || s.API.Store == nil {
		return nil, nil
	}
	raw, _, err := api.PolicyRaw(ctx, s.API, s.repoID(owner, repo))
	if err != nil {
		out := &SettingsProtection{Enabled: true, Refs: "refs/heads/main", Roles: settingsProtectionRoles, Raw: string(raw)}
		out.Error = err.Error()
		return out, nil
	}
	return settingsProtectionView(raw), nil
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
						Seq: strconv.Itoa(e.Seq), Event: e.Event, Status: e.Status,
						At: e.At, Error: e.Error,
					})
				}
				last := d.Entries[len(d.Entries)-1]
				row.LastPing = "last delivery: " + last.Event + " → " + strconv.Itoa(last.Status) + " at " + last.At
			}
			out.Webhooks = append(out.Webhooks, row)
		}
	}
	sort.SliceStable(out.Webhooks, func(i, j int) bool { return out.Webhooks[i].ID < out.Webhooks[j].ID })
	return out, nil
}

// --- writes ------------------------------------------------------------------

func (s SettingsServices) repoWrite(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.API == nil && s.Ident == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings writes are not configured")
	}
	// Every repo settings mutation re-checks the admin gate at the seam (the
	// page checked it, but the seam is public) — the same rule the API's
	// AuthAdmin routes apply.
	if s.Ident != nil {
		if aerr := s.Ident.CheckRole(ctx, owner, repo, p, identity.RoleAdmin); aerr != nil {
			return SettingsWriteResult{}, settingsAuthErr(aerr)
		}
	} else if !p.Admin {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusForbidden, "admin access required")
	}
	id := s.repoID(owner, repo)
	switch w.Action {
	case SettingsActionGeneral:
		return s.writeGeneral(ctx, id, p, w)
	case SettingsActionAccess:
		return s.writeAccess(ctx, owner, repo, w)
	case SettingsActionPolicy:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy store not configured")
		}
		if werr := api.PutPolicy(ctx, s.API, id, []byte(w.Get("policy"))); werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "policy saved"}, nil
	case SettingsActionProtection:
		if s.API == nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "policy store not configured")
		}
		cur, _, _ := api.PolicyRaw(ctx, s.API, id)
		in, verr := settingsProtectionInputFrom(SettingsProtection{
			Enabled:      w.Get("enabled") != "",
			Refs:         w.Get("refs"),
			Approvals:    settingsAtoiOr(w.Get("approvals"), 0),
			DismissStale: w.Get("dismiss_stale") != "",
			Checks:       w.Get("checks"),
			PushRoles:    w.Multi["push_roles"],
			BlockForce:   w.Get("block_force") != "",
			BlockDelete:  w.Get("block_delete") != "",
		})
		if verr != nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusBadRequest, verr.Error())
		}
		in.Owner, in.Repo = owner, repo
		doc, berr := settingsProtectionDocument(cur, in)
		if berr != nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusBadRequest, berr.Error())
		}
		if werr := api.PutPolicy(ctx, s.API, id, doc); werr != nil {
			return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
		}
		return SettingsWriteResult{Notice: "branch protection saved"}, nil
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
		panel := &SettingsPolicy{
			Text: w.Get("policy"), DryRunN: n, DryRunDone: true,
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
	case SettingsActionWebhookCreate:
		return s.createWebhook(ctx, owner, repo, p, w)
	case SettingsActionWebhookDelete:
		return s.deleteWebhook(ctx, owner, repo, w)
	case SettingsActionWebhookPing:
		return s.pingWebhook(ctx, owner, repo, p, w)
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

// writeGeneral edits the three passenger keys of the settings TOML (the SPA's
// withDescription/withFeatures/withSelfApproval helpers, ported) and
// publishes the document — every other section passes through untouched.
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
	text := settingsWithDescription(doc.TOML, desc)
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
	id := s.Mirror.Svc.SyncAsync(ctx, owner, repo, w.Get("token"), w.Get("force") == "true")
	return SettingsWriteResult{Notice: "sync started (task " + id + ")"}, nil
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
	id := s.Push.Svc.SyncAsync(ctx, owner, repo, w.Get("force") == "true")
	return SettingsWriteResult{Notice: "push sync started (task " + id + ")"}, nil
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
	if err := s.Ident.TransferRepo(ctx, owner, repo, dstOwner, dstRepo); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsIdentityStatus(err), err.Error())
	}
	return SettingsWriteResult{
		Notice:   "repository transferred to " + dstOwner + "/" + dstRepo,
		Redirect: "/" + dstOwner + "/" + dstRepo + "/settings/general?saved=1",
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
	line := `description = "` + settingsEscapeTomlBasic(desc) + `"`
	if strings.TrimSpace(text) == "" {
		return line + "\n"
	}
	lines := strings.Split(text, "\n")
	end := settingsTopLevelRegion(lines)
	for i := 0; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "description") {
			continue
		}
		if strings.HasPrefix(t, "description =") || strings.HasPrefix(t, "description=") {
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
