// audit.go — identity's writes into the host audit family (PLAN-V2 §M.4's
// audit-completeness sweep; PLAN-ADMIN §8.6). Identity is where "who may write
// what" and "who owns this repository" change, so it is where the family's
// access-grant and transfer records come from.
//
// The family itself lives in internal/moderation (moderation/audit/<seq>.json,
// one Create-only record per action). Identity must not import moderation
// (moderation imports identity for the role gates), so it takes the writer as
// a seam — composition binds moderation.Service.RecordAdmin — exactly like
// OrgEvent and Stream.
//
// # Attribution
//
// The record's actor is the operator who made the change. Every production
// writer passes through a request context, so the handler attaches the
// verified principal with WithActor; a caller that never attached one (the
// CLI, a background task) records "system" rather than inventing a name.
//
// # Failure policy
//
// A failed audit write never fails the committed change (audit-after-commit):
// the grant/move is already in the bucket, and un-writing it would be worse
// than an entry the operator can see is missing. The failure is logged with
// the action and target.
package identity

import (
	"context"
	"log/slog"
)

// Actions identity writes into the host audit family. The spellings are the
// audit viewer's filter vocabulary (docs/features/16_moderation.md §3).
const (
	AuditAccessGrant     = "access_grant"      // access.json replaced (roles/visibility)
	AuditOrgMemberGrant  = "org_member_grant"  // org roster: added or role changed
	AuditOrgMemberRevoke = "org_member_revoke" // org roster: removed
	AuditTeamGrant       = "team_member_grant"
	AuditTeamRevoke      = "team_member_revoke"
	AuditTransfer        = "transfer" // a repository ownership move completed
)

// actorKey carries the authenticated principal's name for audit attribution.
type actorKey struct{}

// WithActor attaches the acting principal's name to ctx (the handlers' one
// call, after the role gate). It never grants anything: it only names who did
// what in the audit record.
func WithActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, actorKey{}, name)
}

// ActorOf reports the acting principal's name, "" when none was attached.
func ActorOf(ctx context.Context) string {
	if name, ok := ctx.Value(actorKey{}).(string); ok {
		return name
	}
	return ""
}

// auditAdmin writes one administrative record. Nil seam = no audit family on
// this build (the change still lands; nothing to write to).
func (s *Service) auditAdmin(ctx context.Context, action, target, detail string) {
	if s.Audit == nil {
		return
	}
	actor := ActorOf(ctx)
	if actor == "" {
		actor = "system"
	}
	if err := s.Audit(ctx, actor, action, target, detail); err != nil {
		slog.Warn("identity: audit write failed", "action", action, "target", target, "err", err)
	}
}
