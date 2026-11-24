package pulls

import (
	"context"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/config"
	walgit "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- role helpers (P6, same ladder as internal/issues) -----------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireRead enforces the read gate (identity require_read hook when
// wired: public visibility or role ≥ read).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// features resolves the repo's feature flags (Forgejo #522), failing open
// to all-enabled when the hook is unwired or declines: display metadata
// must never break a write on a transient settings read (the CollabCounts
// precedent — only an explicit false refuses).
func (s *Service) features(ctx context.Context, owner, repo string) config.ResolvedFeatures {
	if s.Features == nil {
		return config.AllFeatures()
	}
	f, ok := s.Features(ctx, owner, repo)
	if !ok {
		return config.AllFeatures()
	}
	return f
}

// requireAuthenticated rejects anonymous callers (open/comment/merge need a
// principal to attribute).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// requireNotBlocked is pulls' one moderation enforcement point
// (PLAN-V2 §M.5): called after role resolution and before any CAS by every
// interaction verb. Host admins are exempt; a gate error fails closed; the
// refusal text never names the block (identity.InteractionRefused).
func (s *Service) requireNotBlocked(ctx context.Context, owner, repo string, actor auth.Principal) error {
	if s.Moderation == nil || actor.Anonymous || actor.Admin {
		return nil
	}
	blocked, _, err := s.Moderation.BlockedFromRepo(ctx, owner, repo, normPrincipal(actor.Name))
	if err != nil {
		return fmt.Errorf("moderation unavailable: %v", err)
	}
	if blocked {
		return fmt.Errorf("%w: %s", ErrForbidden, identity.InteractionRefused)
	}
	return nil
}

// --- protected-ref gate (§5 step 4) ------------------------------------------

// loadPolicy reads and parses repos/<o>/<r>/policy.json. A missing file is
// allow-all (anyone with the role may move any ref) — the only implicit
// default. An unparseable file fails closed (merge refused, never "skip
// policy").
func (s *Service) loadPolicy(ctx context.Context, owner, repo string) (*policy.Document, error) {
	raw, _, err := s.getJSON(ctx, PolicyKey(owner, repo))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return policy.Parse([]byte(`{"version":1,"rules":[]}`))
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return nil, fmt.Errorf("%w: policy.json unparseable: %v", ErrConflict, perr)
	}
	return doc, nil
}

// checkProtectedRef explicitly evaluates policy.json for (principal,
// ref, op) on the server-side ref-write decisions that still take the
// protect verdict — the merge task's head cleanup (delete); the tags
// service mirrors this shape for its server-side writes. It is NOT the
// merge gate: protect restricts/bypass govern direct ref updates
// (pushes), so runMerge does not call it for the base-ref update (see
// checkMergeTarget). Evaluation is pure and local (no network in eval).
// op is create|update|delete|force-push.
func (s *Service) checkProtectedRef(ctx context.Context, owner, repo string, principal, ref, op string) error {
	if walgit.IsManagedRef(ref) {
		// Only the PR task publishes managed refs, server-side; a client
		// principal never satisfies this path (the push pipeline refuses
		// managed refs before policy). The task's own base/head publishes
		// are never managed refs — reaching here is a bug signal.
		return fmt.Errorf("%w: refused to publish managed ref %q through the policy path", ErrForbidden, ref)
	}
	doc, err := s.loadPolicy(ctx, owner, repo)
	if err != nil {
		return err
	}
	// Protect-only evaluation (policy.EvaluateProtect): the publisher is
	// server-side, NOT a receive-pack push, so observation effects with
	// their own gates (required-reviews / require_checks) must not deny a
	// ref delete here — they are merge gates, consulted separately by
	// runMerge. On the delete path the protect verdict IS the gate.
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: principal, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("rejected by rule '%s'", v.Rule)
	}
	return nil
}

// checkMergeTarget is the merge-time policy precondition (03 §5 step 4):
// the target must not be a server-managed ref, and policy.json must load
// and parse (fail closed — an unreadable policy never becomes "merge
// without a policy"). Protect `restricts`/`bypass` are deliberately NOT
// evaluated: they govern direct ref updates (client pushes), not the
// server-side merge publish. A merge is constrained by the observation
// half of the policy instead — require_checks (05 §6, always evaluated
// for the merging actor, bypass or not) and required-reviews (04 §6,
// which owns its own bypass list) — through their own gates.
func (s *Service) checkMergeTarget(ctx context.Context, owner, repo, ref string) error {
	_, err := s.loadMergeGate(ctx, owner, repo, ref, "")
	return err
}

// --- merge-method gate (PLAN-V2 §2.2) ---------------------------------------

// loadMergeGate is checkMergeTarget's one-read form: it refuses a
// server-managed target, loads and parses policy.json (fail closed — an
// unreadable policy never becomes "merge without a policy"), and resolves
// the merge-method half of the `protect` effect for (base ref, merging
// actor) through policy.MergeGateFor — the same resolver the PR page uses
// to decide which methods to offer, so page and gate cannot drift.
func (s *Service) loadMergeGate(ctx context.Context, owner, repo, ref, principal string) (policy.MergeGate, error) {
	if walgit.IsManagedRef(ref) {
		// The merge task never publishes a managed ref: a PR's base ref is
		// a branch, and refs/pull/** belongs to the PR open task alone.
		return policy.MergeGate{}, fmt.Errorf("%w: refused to publish managed ref %q through the merge path", ErrForbidden, ref)
	}
	doc, err := s.loadPolicy(ctx, owner, repo)
	if err != nil {
		return policy.MergeGate{}, err
	}
	return policy.MergeGateFor(doc, policy.Request{Principal: principal, Ref: ref, Op: policy.OpUpdate}), nil
}

// checkMergeMethod refuses a strategy the base ref's policy forbids
// (merge_methods, PLAN-V2 §2.2/§2.5.2). The allowed set is intersected
// across every matching rule; a rule that omits the field never narrows it.
// The refusal names the method, the rule(s) and the allowed set — the same
// text the API's 409 and the page's inline error carry.
func checkMergeMethod(g policy.MergeGate, strategy, baseRef string) error {
	if g.Allows(strategy) {
		return nil
	}
	return fmt.Errorf("%w: %w: %q is not allowed on %s by rule %s (allowed: %s)",
		ErrConflict, ErrMethodNotAllowed, strategy, baseRef, quoteRules(g.Rules),
		strings.Join(g.MethodsOrAll(), ", "))
}

// checkMergeUpToDate refuses a merge whose head does not contain the base
// tip when require_up_to_date applies to the merging actor (PLAN-V2
// §2.2/§2.5.2). The fix is the update-branch task, so the message names it.
func checkMergeUpToDate(g policy.MergeGate, contained bool, baseRef string) error {
	if !g.RequireUpToDate || contained {
		return nil
	}
	return fmt.Errorf("%w: %w (required on %s by rule %s)",
		ErrConflict, ErrHeadBehindBase, baseRef, quoteRules(g.Rules))
}

// quoteRules renders rule names for a refusal ("'a', 'b'").
func quoteRules(names []string) string {
	if len(names) == 0 {
		return "policy"
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "'"+n+"'")
	}
	return strings.Join(out, ", ")
}

// --- message templates (§5 step 3) -------------------------------------------

// mergeMessage renders the default commit message for a strategy. Titles:
// merge — `Merge pull request #<num> from <head-ref-shorthand>`;
// squash/rebase — the head's subject (+ body = squashed head messages or
// the request's commit_message). commit_title/commit_message overrides win.
// Default-branch merges of protected refs append `(<full sha>)` per GitHub
// convention — the UI renders the sha link.
func mergeMessage(strategy string, num int, headRef, headSubject, headBody, titleOverride, msgOverride string) (title, body string) {
	short := headRef
	if i := strings.LastIndex(headRef, "/"); i >= 0 {
		short = headRef[i+1:]
	}
	switch strategy {
	case StrategyMerge:
		title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
		body = strings.TrimSpace(headSubject)
		if headBody != "" {
			body += "\n\n" + strings.TrimSpace(headBody)
		}
	case StrategySquash, StrategyRebase:
		title = headSubject
		if title == "" {
			title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
		}
		body = strings.TrimSpace(headBody)
	default:
		title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
	}
	if titleOverride != "" {
		title = titleOverride
	}
	if msgOverride != "" {
		body = msgOverride
	}
	return title, body
}

// fullMessage joins title + body into the commit-tree -m message.
func fullMessage(title, body string) string {
	if strings.TrimSpace(body) == "" {
		return title
	}
	return title + "\n\n" + body
}
