// moderation.go — Feature M composition (PLAN-V2 §1, M1): the block list,
// its both-lane block API (top-level twins), and the audit family skeleton
// over the P6 roles owned by identity.
//
// The gates are consumer-owned: issues/pulls/review define their own
// ModerationGate, identity defines BlockGate for invitations, notify defines
// BlockGate for the mention filter. Composition satisfies each with the one
// *moderation.Service — no consumer imports this package (law 8).
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/review"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
	"github.com/crueber/spindle/internal/webui"
)

// newModerationService builds the moderation service over st with identity
// as its namespace probe, and registers the block discovery templates
// (Forgejo #272 rule: template + handler land in the same change).
func newModerationService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry) (*moderation.Service, *moderation.Handler) {
	api.RegisterExposed(moderation.ExposedTemplates...)
	svc := moderation.New(st)
	svc.Namespaces = ident
	// M2 floors + the read-gated moderation GET resolve through the same P6
	// roles as every other surface; hide-target existence is probed in the
	// owning family (composition owns the join, law 8).
	svc.Roles = ident
	svc.Targets = moderationTargets{st: st}
	// M3: the global report rebuild rides the core (repo,kind) task table.
	if reg != nil {
		svc.Tasks = reg.Tasks()
	}
	return svc, &moderation.Handler{Svc: svc}
}

// moderationTargets probes a hide target's object in the family that owns it:
// a thread event (issues), a review, or a review-thread comment. Exact-key
// HEAD only — no LIST, and the moderation package never learns another
// feature's key layout.
type moderationTargets struct{ st store.ObjectStore }

func (p moderationTargets) Exists(ctx context.Context, owner, repo string, t moderation.Target) (bool, error) {
	var key string
	switch t.Kind {
	case moderation.TargetThread:
		key = issues.EventKey(owner, repo, t.Num, t.Seq)
	case moderation.TargetReview:
		key = review.ReviewKey(owner, repo, t.Num, t.Seq)
	case moderation.TargetReviewThread:
		key = review.ReviewThreadEventKey(owner, repo, t.Num, t.TID, t.Seq)
	default:
		return false, nil
	}
	meta, err := p.st.Head(ctx, key)
	if err != nil {
		return false, err
	}
	return meta != nil, nil
}

// wireModerationStream publishes the "moderation" collab frame once the repo
// bus exists (the service is built before notify; this runs in buildCollab
// after both). The frame is invalidation + state only — never a body.
func wireModerationStream(mod *moderation.Service, nt *notify.Service) {
	if mod == nil || nt == nil {
		return
	}
	mod.Stream = func(ctx context.Context, ev moderation.StreamEvent) {
		nt.PublishFrame(notify.RepoFrame{
			Name: "moderation", Repo: ev.Repo, Num: ev.Num, Seq: ev.Seq,
			Tid: ev.TID, Actor: ev.By, Action: ev.Action, State: ev.State(), At: ev.At,
		})
	}
}

// chainModeration fronts the core mux with the block surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including the
// §8.6 broker-forwarding rule.
func chainModeration(srv *server.Server, h *moderation.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// spamObserverOf binds the advisory spam scorer (PLAN-V2 §M.7): the server's
// collab-write middleware hands it the landed body of a comment/review and it
// pre-files at most one report per burst. Nil without the collaboration
// wiring (setup-only, serve-without-collab) — no principal, no scoring.
func spamObserverOf(c *collabWiring) func(ctx context.Context, owner, repo, principal, body string) bool {
	if c == nil || c.modSvc == nil {
		return nil
	}
	mod := c.modSvc
	return func(ctx context.Context, owner, repo, principal, body string) bool {
		return mod.NoteBody(ctx, owner, repo, principal, body)
	}
}

// wireModeration binds the one service onto every consumer-owned gate. Nil
// consumers are skipped; a nil gate means "feature off" and consumers allow
// writes.
func wireModeration(mod *moderation.Service, ident *identity.Service, iss *issues.Service, pl *pulls.Service, rv *review.Service, nt *notify.Service) {
	if mod == nil {
		return
	}
	if ident != nil {
		ident.Blocks = mod
	}
	if iss != nil {
		iss.Moderation = mod
	}
	if pl != nil {
		pl.Moderation = mod
	}
	if rv != nil {
		rv.Moderation = mod
	}
	if nt != nil {
		nt.Blocks = mod
	}
}

// blockListAdapter is the page's four-verb view of the service (webui defines
// the interface it wants; the service keeps the API's richer signatures).
type blockListAdapter struct{ svc *moderation.Service }

func blockListOf(svc *moderation.Service) webui.BlockList {
	if svc == nil {
		return nil
	}
	return blockListAdapter{svc: svc}
}

func (a blockListAdapter) Blocked(ctx context.Context, blocker, subject string) (bool, error) {
	return a.svc.BlockedBy(ctx, blocker, subject)
}

func (a blockListAdapter) List(ctx context.Context, principal string) ([]string, error) {
	doc, err := a.svc.Blocks(ctx, principal)
	return doc.Blocked, err
}

func (a blockListAdapter) Block(ctx context.Context, blocker, subject string) (bool, error) {
	_, changed, err := a.svc.Block(ctx, blocker, subject)
	return changed, err
}

func (a blockListAdapter) Unblock(ctx context.Context, blocker, subject string) (bool, error) {
	_, changed, err := a.svc.Unblock(ctx, blocker, subject)
	return changed, err
}

// wireModerationReports binds the report_filed notification (PLAN-V2 §M.3):
// the repo admins resolve through identity's collaborator expansion (direct
// bindings, team members, org owners — the P6 admin set) and are handed to
// notify's moderation class. The callback's arguments are the conversation
// pointer and the reporter, never the note or the target, so a notification
// can never carry reported content. A repo with no resolvable admin notifies
// nobody (a report still reaches the host queue).
func wireModerationReports(mod *moderation.Service, ident *identity.Service, nt *notify.Service) {
	if mod == nil || ident == nil || nt == nil {
		return
	}
	mod.ReportNotify = func(ctx context.Context, owner, repo string, num int, actor, at string) {
		var admins []string
		for _, c := range ident.Collaborators(ctx, owner, repo) {
			if c.Role == string(identity.RoleAdmin) {
				admins = append(admins, c.Principal)
			}
		}
		if len(admins) == 0 {
			return
		}
		nt.EmitModeration(ctx, owner+"/"+repo, num, actor, at, admins)
	}
}

// moderationReportsPage builds the Settings → Reports payload from the two
// service lists (ONE index read) — the page and the JSON twin share the verbs.
func moderationReportsPage(svc *moderation.Service) func(ctx context.Context, owner, repo string, p auth.Principal, state string) (*webui.SettingsRepoReports, error) {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, owner, repo string, p auth.Principal, state string) (*webui.SettingsRepoReports, error) {
		open, resolved, err := svc.ReportQueues(ctx, owner, repo, p, 200)
		if err != nil {
			return nil, err
		}
		cards := make([]webui.SettingsReportCard, 0, len(open))
		for _, c := range open {
			cards = append(cards, settingsReportCard(c))
		}
		tail := make([]webui.SettingsReportCard, 0, len(resolved))
		for _, c := range resolved {
			tail = append(tail, settingsReportCard(c))
		}
		return webui.SettingsReportsFrom(owner, repo, state, cards, tail), nil
	}
}

func settingsReportCard(c moderation.ReportCard) webui.SettingsReportCard {
	return webui.SettingsReportCard{
		ID: c.ID, Kind: c.Kind, Target: c.Target, Reporter: c.Reporter,
		Reason: c.Reason, Note: c.Note, CreatedAt: c.CreatedAt, State: c.State,
		ResolvedBy: c.ResolvedBy, ResolvedAt: c.ResolvedAt,
		Action: c.Action, ResolveNote: c.ResolveNote,
	}
}

// moderationReportResolve is the page's resolve seam: the service verb.
func moderationReportResolve(svc *moderation.Service) func(ctx context.Context, owner, repo string, id int, p auth.Principal, action, note string) error {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, owner, repo string, id int, p auth.Principal, action, note string) error {
		_, err := svc.ResolveReport(ctx, owner, repo, id, p, action, note)
		return err
	}
}

// moderationGlobalReports builds /admin/reports from the global queue verb.
func moderationGlobalReports(svc *moderation.Service) func(ctx context.Context, p auth.Principal, state, after string, n int) (*webui.AdminReportsView, error) {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, p auth.Principal, state, after string, n int) (*webui.AdminReportsView, error) {
		rows, more, err := svc.ListGlobalReports(ctx, p, state, after, n)
		if err != nil {
			return nil, err
		}
		cards := make([]webui.AdminReportCard, 0, len(rows))
		for _, c := range rows {
			cards = append(cards, webui.AdminReportCard{
				Repo: c.Repo, ID: c.ID, Kind: c.Kind, Target: c.Target,
				Reporter: c.Reporter, Reason: c.Reason, Note: c.Note,
				CreatedAt: c.CreatedAt, State: c.State, ResolvedBy: c.ResolvedBy,
				ResolvedAt: c.ResolvedAt, Action: c.Action, ResolveNote: c.ResolveNote,
				Cursor: moderation.GlobalCursor(c),
			})
		}
		return webui.AdminReportsFrom(cards, state, more), nil
	}
}

// moderationAuditView builds /admin/audit from the one audit read.
func moderationAuditView(svc *moderation.Service) func(ctx context.Context, p auth.Principal, after, n int, f webui.AdminAuditFilter) (*webui.AdminAuditView, error) {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, p auth.Principal, after, n int, f webui.AdminAuditFilter) (*webui.AdminAuditView, error) {
		if !p.Admin || p.Anonymous {
			return nil, fmt.Errorf("%w: host admin required", moderation.ErrForbidden)
		}
		filter := moderation.AuditFilter{
			Action: f.Action, Actor: f.Actor, Repo: f.Repo, Since: f.SinceTime,
		}
		page, err := svc.AuditQuery(ctx, after, n, filter)
		if err != nil {
			return nil, err
		}
		view := &webui.AdminAuditView{More: page.More, After: after, Next: page.Next, Scanned: page.Scanned}
		for _, e := range page.Entries {
			view.Rows = append(view.Rows, webui.AdminAuditRow{
				Seq: e.Seq, At: e.At, Actor: e.Actor, Action: e.Action,
				Target: e.Target, Reason: e.Reason, Context: e.Context,
			})
		}
		return view, nil
	}
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the consumers define.
var (
	_ moderation.Namespaces = (*identity.Service)(nil)
	_ issues.ModerationGate = (*moderation.Service)(nil)
	_ pulls.ModerationGate  = (*moderation.Service)(nil)
	_ review.ModerationGate = (*moderation.Service)(nil)
	_ identity.BlockGate    = (*moderation.Service)(nil)
	_ notify.BlockGate      = (*moderation.Service)(nil)
	_ server.ExtraRoutes    = (*moderation.Handler)(nil)
	_ moderation.TaskRunner = (*wal.TaskTable)(nil)
	_ webui.BlockList       = blockListAdapter{}
)
