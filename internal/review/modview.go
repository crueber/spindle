// modview.go — the M2 moderation projection for review (PLAN-V2 §M.6 #2).
//
// Review bodies and review-thread comments are separate content families in
// the moderation document's target grammar; both reads consult the same
// consumer-owned ModerationGate as the write verbs, so the JSON lanes and
// the rendered page apply one rule. The body never reaches a client that may
// not see it: redaction is server-side and the projection result is short
// lived (a fresh unmarshal per read).
package review

import (
	"context"
	"fmt"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// reviewModView is one folded moderation read.
type reviewModView struct {
	hidden map[string]HiddenInfo
	locked bool
	mode   string
	reason string
	reveal bool // caller may see hidden bodies (triage+)
}

// moderationView folds the repo moderation document into a review read. A
// gate error fails the read closed.
func (s *Service) moderationView(ctx context.Context, owner, repo string, num int, p auth.Principal) (reviewModView, error) {
	mv := reviewModView{hidden: map[string]HiddenInfo{}, reveal: s.canSeeHidden(ctx, owner, repo, p)}
	if s.Moderation == nil {
		return mv, nil
	}
	locked, mode, reason, _, err := s.Moderation.Snapshot(ctx, owner, repo, num, func(target, by, at, why string) {
		mv.hidden[target] = HiddenInfo{By: by, At: at, Reason: why}
	})
	if err != nil {
		return reviewModView{}, fmt.Errorf("moderation unavailable: %v", err)
	}
	mv.locked, mv.mode, mv.reason = locked, mode, reason
	return mv, nil
}

// canSeeHidden is the projection's role question: moderators (triage+) get
// the original body alongside the hidden marker.
func (s *Service) canSeeHidden(ctx context.Context, owner, repo string, p auth.Principal) bool {
	return s.requireRole(ctx, owner, repo, p, string(identity.RoleTriage)) == nil
}

// lockModeCollaborators is the contractual spelling of
// moderation.LockModeCollaborators (docs/features/16_moderation.md §10.1).
const lockModeCollaborators = "collaborators"

// requireUnlocked is the M2 lock enforcement point for review writes. It
// composes with threadLocked (closed/merged PR): either refuses. Mode "all"
// refuses everyone, host admins included (unchanged); mode "collaborators"
// admits role ≥ write on the normal P6 ladder (a host admin resolves to
// admin, so an operator may still review there).
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

// reviewTarget is the plan's target key for one review body.
func reviewTarget(num, seq int) string {
	return fmt.Sprintf("review:%06x:%012x", num, seq)
}

// reviewThreadTarget is the plan's target key for one review-thread comment.
func reviewThreadTarget(num int, tid string, seq int) string {
	return fmt.Sprintf("review_thread:%06x:%s:%012x", num, tid, seq)
}

// applyReview hides or reveals one review body according to mv.
func applyReview(ev *ReviewEvent, mv reviewModView, num int) {
	if ev == nil {
		return
	}
	h, ok := mv.hidden[reviewTarget(num, ev.Seq)]
	if !ok {
		return
	}
	hh := h
	ev.Hidden = &hh
	if !mv.reveal {
		ev.Body = ""
	}
}

// applyThreadComment is applyReview's review-thread twin.
func applyThreadComment(c *ThreadComment, mv reviewModView, num int, tid string) {
	if c == nil {
		return
	}
	h, ok := mv.hidden[reviewThreadTarget(num, tid, c.Seq)]
	if !ok {
		return
	}
	hh := h
	c.Hidden = &hh
	if !mv.reveal {
		c.Body = ""
	}
}
