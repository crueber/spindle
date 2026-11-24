// modview.go — the M2 moderation projection for pulls (PLAN-V2 §M.6 #2).
//
// One read of the repo moderation document feeds both the redaction of the
// timeline/description and the conversation lock; the rule itself lives in
// internal/moderation, reached through the consumer-owned ModerationGate so
// the JSON lanes and the rendered page cannot implement it differently.
package pulls

import (
	"context"
	"fmt"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// pullModView is one folded moderation read.
type pullModView struct {
	hidden map[string]HiddenInfo
	locked bool
	mode   string
	reason string
	rev    int
	reveal bool // caller may see hidden bodies (triage+)
}

// moderationView folds the repo moderation document into a PR read. A gate
// error fails the read closed.
func (s *Service) moderationView(ctx context.Context, owner, repo string, num int, p auth.Principal) (pullModView, error) {
	mv := pullModView{hidden: map[string]HiddenInfo{}, reveal: s.canSeeHidden(ctx, owner, repo, p)}
	if s.Moderation == nil {
		return mv, nil
	}
	locked, mode, reason, rev, err := s.Moderation.Snapshot(ctx, owner, repo, num, func(target, by, at, why string) {
		mv.hidden[target] = HiddenInfo{By: by, At: at, Reason: why}
	})
	if err != nil {
		return pullModView{}, fmt.Errorf("moderation unavailable: %v", err)
	}
	mv.locked, mv.mode, mv.reason, mv.rev = locked, mode, reason, rev
	return mv, nil
}

// canSeeHidden is the projection's role question: moderators (triage+) get
// the original body alongside the hidden marker.
func (s *Service) canSeeHidden(ctx context.Context, owner, repo string, p auth.Principal) bool {
	return s.requireRole(ctx, owner, repo, p, string(identity.RoleTriage)) == nil
}

// lockModeCollaborators is the contractual spelling of
// moderation.LockModeCollaborators (docs/features/16_moderation.md §10.1);
// the consumer-owned-gate rule keeps this package off the implementation.
const lockModeCollaborators = "collaborators"

// requireUnlocked is the M2 lock enforcement point for PR conversation
// writes. It composes with threadLocked (closed/merged): either refuses. Mode
// "all" refuses everyone, host admins included (unchanged); mode
// "collaborators" admits role ≥ write on the normal P6 ladder (a host admin
// resolves to admin, so an operator may still comment there).
func (s *Service) requireUnlocked(ctx context.Context, owner, repo string, num int, actor auth.Principal) error {
	if s.Moderation == nil {
		return nil
	}
	locked, mode, reason, _, err := s.Moderation.Snapshot(ctx, owner, repo, num, nil)
	if err != nil {
		return fmt.Errorf("moderation unavailable: %v", err)
	}
	if !locked {
		return nil
	}
	if mode == lockModeCollaborators && s.requireRole(ctx, owner, repo, actor, string(identity.RoleWrite)) == nil {
		return nil
	}
	if reason == "" {
		reason = "locked by a moderator"
	}
	return fmt.Errorf("%w: conversation locked: %s", ErrLocked, reason)
}

// applyModeration redacts the projected view: event bodies, and the PR
// description (which is its opened event, seq 1 by construction).
func (v *PullView) applyModeration(mv pullModView, num int) {
	for _, ev := range v.Events {
		h, ok := mv.hidden[HiddenTarget(num, ev.Seq)]
		if !ok {
			continue
		}
		hh := h
		ev.Hidden = &hh
		if !mv.reveal {
			ev.Body = nil
		}
	}
	if h, ok := mv.hidden[HiddenTarget(num, 0)]; ok {
		hh := h
		v.DescriptionHidden = &hh
		if !mv.reveal && v.PR != nil {
			v.PR.Body = ""
		}
	}
	v.Locked, v.LockMode, v.LockReason, v.ModerationRevision = mv.locked, mv.mode, mv.reason, mv.rev
}
