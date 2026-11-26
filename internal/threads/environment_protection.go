package threads

// Environment protection parity (PAR-131, docs/threads.md §5.6).
//
// An environment record carries the five documented protection rules, and each
// one is enforced at a NAMED point — never left inert:
//
//	required reviewers   resolved at run creation into Deployment.RequiredApprovers
//	                     (direct reviewers plus each team's members) and
//	                     re-resolved from the live record at each decision;
//	                     ApproveRun records one approval per principal and
//	                     releases the run only when every required principal has
//	                     approved.
//	prevent self-review  ApproveRun refuses the run's own actor.
//	wait timer           Deployment.WaitUntil is written at creation and the CLAIM
//	                     path refuses to offer the job before it (queue.go
//	                     claimEntry), so the timer holds even with no reviewer.
//	branch/tag policy    checked at creation against the run's ref; a violation
//	                     fails the deployment job with a named reason (fail
//	                     closed, like an undefined environment).
//	team reviewers       resolved through Service.TeamRoster at save and at
//	                     creation; an unresolvable team is a named error (fail
//	                     closed), never a hold nobody can satisfy.
//
// The record stays the authority: every decision re-reads it (holdsForEnvironment
// is advisory), so a roster edited between the page render and the POST cannot
// widen an approval.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Protection bounds (GitHub's documented limits, docs/threads.md §5.6).
const (
	// MaxEnvironmentReviewers is GitHub's ceiling: at most six users or teams
	// may be required reviewers of one environment. A seventh is a named save
	// error, never a silent truncation.
	MaxEnvironmentReviewers = 6
	// MaxDeploymentBranchPatterns bounds the deployment branch/tag policy list.
	MaxDeploymentBranchPatterns = 50
	// MaxEnvironmentWaitSeconds is GitHub's 6-hour wait-timer ceiling.
	MaxEnvironmentWaitSeconds = 6 * 60 * 60
)

// TeamRoster resolves an organization team to its member principals. It is the
// consumer-owned seam (identity satisfies it through composition); nil means
// the instance has no team reader, and a record that names team reviewers then
// fails closed instead of holding a deployment nobody can release.
type TeamRoster interface {
	TeamMembers(ctx context.Context, org, team string) ([]string, error)
}

// DeploymentNotice is one pending-deployment notification (PAR-135): the
// reviewers who must approve, the environments awaiting them, and the run.
// Composition translates it onto internal/notify (threads never imports it).
type DeploymentNotice struct {
	Repo         string   `json:"repo"`
	RunID        string   `json:"run_id"`
	Workflow     string   `json:"workflow,omitempty"`
	Environments []string `json:"environments"`
	Reviewers    []string `json:"reviewers"`
	Actor        string   `json:"actor,omitempty"`
	At           string   `json:"at"`
}

// normalizeTeamReviewers validates team reviewer names ("org/team"), requires
// the team to resolve through the roster, and returns them lowercased, sorted
// and deduped. A nil roster with a non-empty list is a named error: a team
// requirement the server cannot resolve is a deployment held forever.
func (s *Service) normalizeTeamReviewers(ctx context.Context, in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		org, slug, ok := strings.Cut(name, "/")
		if !ok || org == "" || slug == "" || strings.Contains(slug, "/") {
			return nil, fmt.Errorf("%w: team reviewer %q must be spelled org/team", ErrInvalid, raw)
		}
		if s == nil || s.TeamRoster == nil {
			return nil, fmt.Errorf("%w: team reviewer %q needs the team directory, which is not wired on this instance", ErrInvalid, raw)
		}
		members, err := s.TeamRoster.TeamMembers(ctx, org, slug)
		if err != nil {
			return nil, fmt.Errorf("%w: team reviewer %q: %v", ErrInvalid, raw, err)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("%w: team reviewer %q has no members, so no one could ever approve", ErrInvalid, raw)
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Strings(out)
	return out, nil
}

// normalizeDeploymentBranches validates the deployment branch/tag policy: the
// same simple glob dialect as a trigger filter (exact names and `*`), matched
// against the short branch/tag name and the full ref. Empty means "all refs".
func normalizeDeploymentBranches(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" || seen[p] {
			continue
		}
		if len(p) > 256 {
			return nil, fmt.Errorf("%w: deployment branch pattern is longer than 256 bytes", ErrInvalid)
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("%w: deployment branch pattern %q contains a control character", ErrInvalid, raw)
			}
			if strings.ContainsRune("\\{}[]?!,", r) {
				return nil, fmt.Errorf("%w: deployment branch pattern %q uses %q; the documented dialect is exact names and `*`", ErrInvalid, raw, r)
			}
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > MaxDeploymentBranchPatterns {
		return nil, fmt.Errorf("%w: at most %d deployment branch patterns (got %d)", ErrInvalid, MaxDeploymentBranchPatterns, len(out))
	}
	sort.Strings(out)
	return out, nil
}

// deploymentRefAllowed applies the environment's branch/tag policy to one run.
// It returns (true, "") when the ref may deploy, or (false, reason) with the
// sentence a failed job carries. No policy matches every ref; a run with no ref
// (defensive) is refused by a non-empty policy.
func deploymentRefAllowed(patterns []string, run *Run, env string) (bool, string) {
	if len(patterns) == 0 {
		return true, ""
	}
	ref := strings.TrimSpace(run.Ref)
	short := refName(ref, run.Branch)
	for _, p := range patterns {
		if globMatchRef(p, short) || (ref != "" && globMatchRef(p, ref)) {
			return true, ""
		}
	}
	label := ref
	if label == "" {
		label = short
	}
	if label == "" {
		label = "(no ref)"
	}
	return false, fmt.Sprintf("environment %q does not allow deployments from ref %q (configured patterns: %s)",
		env, label, strings.Join(patterns, ", "))
}

// refName is the short name of a ref (branch or tag), falling back to the
// run's branch field.
func refName(ref, branch string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	if strings.TrimSpace(ref) == "" {
		return strings.TrimSpace(branch)
	}
	return ref
}

// globMatchRef is the documented deployment-policy glob: exact names and `*`
// wildcards (the same dialect `on.push.branches` uses).
func globMatchRef(pattern, s string) bool {
	pattern, s = strings.TrimSpace(pattern), strings.TrimSpace(s)
	if pattern == "" || s == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// deploymentApprovers resolves the principals a deployment waits for: the
// record's direct reviewers plus the current members of every team reviewer.
// An unresolvable team is an error (fail closed) — the deployment must not be
// released by a set nobody can enumerate.
func (s *Service) deploymentApprovers(ctx context.Context, doc *EnvironmentDoc) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(doc.RequiredReviewers)+4)
	add := func(name string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range doc.RequiredReviewers {
		add(name)
	}
	for _, team := range doc.TeamReviewers {
		org, slug, ok := strings.Cut(team, "/")
		if !ok || s.TeamRoster == nil {
			return nil, fmt.Errorf("%w: team reviewer %q cannot be resolved on this instance", ErrUnavailable, team)
		}
		members, err := s.TeamRoster.TeamMembers(ctx, org, slug)
		if err != nil {
			return nil, fmt.Errorf("%w: team reviewer %q: %v", ErrUnavailable, team, err)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("%w: team reviewer %q has no members", ErrUnavailable, team)
		}
		for _, m := range members {
			add(m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// waitUntilFor is the wait-timer deadline for a deployment created at now.
func waitUntilFor(doc *EnvironmentDoc, now time.Time) *string {
	if doc == nil || doc.WaitTimerSeconds <= 0 {
		return nil
	}
	stamp := now.Add(time.Duration(doc.WaitTimerSeconds) * time.Second).Format(dateTimeFmt)
	return &stamp
}

// deploymentWaitRemaining reports whether a claimed job's wait timer has not
// elapsed, and the deadline. It is the claim-path guard (queue.go): the timer
// holds the job without a reviewer too, and a timer the server cannot parse
// holds it (fail closed) rather than releasing it early.
func deploymentWaitRemaining(job *Job, now time.Time) (bool, string) {
	if job == nil || job.Deployment == nil || job.Deployment.WaitUntil == nil {
		return false, ""
	}
	raw := strings.TrimSpace(*job.Deployment.WaitUntil)
	if raw == "" {
		return false, ""
	}
	at, err := time.Parse(dateTimeFmt, raw)
	if err != nil {
		return true, raw
	}
	return now.Before(at), at.UTC().Format(dateTimeFmt)
}

// requiredApprovalComplete reports whether every required approver has
// approved. An empty requirement is complete (a timer-only hold).
func requiredApprovalComplete(dep *Deployment) bool {
	if dep == nil {
		return true
	}
	for _, want := range dep.RequiredApprovers {
		found := false
		for _, got := range dep.Approvals {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// awaitingApprovers is the ordered list of principals who have not approved.
func awaitingApprovers(dep *Deployment) []string {
	if dep == nil {
		return nil
	}
	var out []string
	for _, want := range dep.RequiredApprovers {
		approved := false
		for _, got := range dep.Approvals {
			if got == want {
				approved = true
				break
			}
		}
		if !approved {
			out = append(out, want)
		}
	}
	return out
}

// recordApproval appends one principal's approval (idempotent, bounded by the
// required set plus one admin bypass marker).
func recordApproval(dep *Deployment, who string) {
	who = strings.ToLower(strings.TrimSpace(who))
	if dep == nil || who == "" {
		return
	}
	for _, got := range dep.Approvals {
		if got == who {
			return
		}
	}
	dep.Approvals = append(dep.Approvals, who)
}

// notifyPendingDeployment tells the required reviewers a deployment is waiting
// (PAR-135). Best effort and never fatal: the hold exists and is visible on the
// run page even when the notification seam is unwired or its emission fails.
func (s *Service) notifyPendingDeployment(ctx context.Context, owner, repo string, run *Run, reviewers []string) {
	if s == nil || s.NotifyDeployment == nil || run == nil || len(reviewers) == 0 {
		return
	}
	ev := DeploymentNotice{
		Repo: repoID(owner, repo), RunID: run.ID, Workflow: run.Workflow,
		Environments: append([]string{}, run.Environments...),
		Reviewers:    append([]string{}, reviewers...),
		Actor:        run.Actor, At: s.nowUTC().Format(dateTimeFmt),
	}
	s.NotifyDeployment(ctx, ev)
}

// pendingReviewers is the union of the required approvers of the given
// environments, for the pending-deployment notice. A record that cannot be read
// contributes nobody (the hold is still visible on the page); the delivery is
// best effort, never a reason to fail a created run.
func (s *Service) pendingReviewers(ctx context.Context, owner, repo string, envs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, env := range envs {
		doc, err := s.loadEnvironment(ctx, owner, repo, env)
		if err != nil || doc == nil {
			continue
		}
		approvers, aerr := s.deploymentApprovers(ctx, doc)
		if aerr != nil {
			continue
		}
		for _, name := range approvers {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
