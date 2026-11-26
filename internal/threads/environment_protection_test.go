package threads

// environment_protection_test.go — deployment protection parity (PAR-131) and
// the pending-deployment surfaces (PAR-135): required reviewers (all of them),
// team reviewers, prevent-self-review, the wait timer, the deployment
// branch/tag policy, the run-list/approval projection and the reviewer notice.
//
// The enforcement points are named in each test:
//
//	creation        applyEnvironmentDecision (branch policy, approver snapshot, timer)
//	approval        ApproveRun/recordDeploymentApproval (one approval per principal,
//	                self-review refusal, admin bypass)
//	claim           queue.claimEntry (the wait timer holds the job)
//	payload         guardDeploymentSecrets (approved + timer, the final backstop)

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// fakeTeamRoster is the identity seam's test double.
type fakeTeamRoster struct {
	teams map[string][]string
	err   error
}

func (f fakeTeamRoster) TeamMembers(_ context.Context, org, team string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.teams[org+"/"+team], nil
}

// deployRunAs creates the deploy workflow's run for an arbitrary event (the
// branch-policy tests need a ref the push filter would otherwise suppress).
func (f *envFixture) deployRunAs(t *testing.T, event string, opts CreateOpts) *Run {
	t.Helper()
	if opts.Actor == "" {
		opts.Actor = "writer"
	}
	runs, err := f.svc.CreateRun(ctx(), "o/r", testSHA, event, opts)
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", event, err)
	}
	if len(runs) != 1 {
		t.Fatalf("%s runs = %d, want 1", event, len(runs))
	}
	return runs[0]
}

// putEnv stores one environment with full protection input.
func (f *envFixture) putEnv(t *testing.T, name string, in EnvironmentInput) *EnvironmentView {
	t.Helper()
	env, err := f.svc.PutEnvironment(ctx(), "o/r", name, in, f.maintain())
	if err != nil {
		t.Fatalf("PutEnvironment(%s): %v", name, err)
	}
	return env
}

// TestEnvironmentProtectionRulesRoundTrip pins the record shapes and their
// save-time limits: every rule is stored and read back, and each bound is a
// named error (six reviewers AND teams together, a 6-hour timer, a resolvable
// team, a documented glob dialect).
func TestEnvironmentProtectionRulesRoundTrip(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	f.svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{
		"acme/releasers": {"ada", "grace"}, "acme/other": {"grace"},
	}}
	env := f.putEnv(t, "production", EnvironmentInput{
		RequiredReviewers:  []string{"ada"},
		TeamReviewers:      []string{"Acme/Releasers"},
		WaitTimerSeconds:   600,
		PreventSelfReview:  true,
		DeploymentBranches: []string{"main", "release/*"},
	})
	if env.WaitTimerSeconds != 600 || !env.PreventSelfReview ||
		strings.Join(env.TeamReviewers, ",") != "acme/releasers" ||
		strings.Join(env.DeploymentBranches, ",") != "main,release/*" {
		t.Fatalf("protection round trip = %+v", env)
	}

	// Six is the ceiling across reviewers AND teams.
	many := make([]string, 0, MaxEnvironmentReviewers+1)
	for i := 0; i <= MaxEnvironmentReviewers; i++ {
		name := fmt.Sprintf("rev%d", i)
		f.roles.roles[name] = "write"
		many = append(many, name)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: many}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("seven reviewers err = %v, want invalid", err)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{RequiredReviewers: many[:5], TeamReviewers: []string{"acme/releasers", "acme/other"}},
		f.maintain()); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("5 reviewers + 2 teams err = %v, want the combined limit", err)
	}
	// Wait-timer bounds are named.
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{WaitTimerSeconds: MaxEnvironmentWaitSeconds + 1}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("7-hour timer err = %v", err)
	}
	// A team that does not resolve is refused at save (never a gate nobody can open).
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{TeamReviewers: []string{"acme/ghosts"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown team err = %v", err)
	}
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{TeamReviewers: []string{"not-a-team"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed team err = %v", err)
	}
	// With no roster wired, a team reviewer is refused by name (fail closed).
	f.svc.TeamRoster = nil
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{TeamReviewers: []string{"acme/releasers"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unwired roster err = %v", err)
	}
	// Patterns use the documented dialect only.
	if _, err := f.svc.PutEnvironment(ctx(), "o/r", "production",
		EnvironmentInput{DeploymentBranches: []string{"release/[ab]"}}, f.maintain()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bracket pattern err = %v", err)
	}
}

// TestEnvironmentEveryReviewerMustApprove pins GitHub's rule: one approval per
// required principal, a partial approval keeps the run pending (and enqueues
// nothing), and the last approval releases it.
func TestEnvironmentEveryReviewerMustApprove(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada", "grace"}})
	run := f.deployRun(t, CreateOpts{Actor: "writer"})
	if run.State != RunPendingApproval {
		t.Fatalf("run = %s, want pending_approval", run.State)
	}
	job := envJob(t, f.testEnv, run.ID)
	if got := strings.Join(job.Deployment.RequiredApprovers, ","); got != "ada,grace" {
		t.Fatalf("required approvers = %q", got)
	}

	first, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"})
	if err != nil {
		t.Fatalf("first approval: %v", err)
	}
	if first.State != RunPendingApproval {
		t.Fatalf("a partial approval released the run: %s", first.State)
	}
	if got := f.queueEntries(t); got != 0 {
		t.Fatalf("partial approval enqueued %d jobs, want 0", got)
	}
	job = envJob(t, f.testEnv, run.ID)
	if job.Deployment.State != DeploymentPending || strings.Join(job.Deployment.Approvals, ",") != "ada" {
		t.Fatalf("deployment after first approval = %+v", job.Deployment)
	}
	// The page names who still owes one.
	av := f.svc.ApprovalFor(ctx(), "o", "r", first, auth.Principal{Name: "ada"})
	if strings.Join(av.WaitingFor, ",") != "grace" || strings.Join(av.Approvals, ",") != "ada" {
		t.Fatalf("approval view = %+v", av)
	}
	// The same principal approving again is idempotent, not a second decision.
	again, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"})
	if err != nil || again.State != RunPendingApproval {
		t.Fatalf("repeat approval = %v (%v), want still pending", again, err)
	}
	done, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "grace"})
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if done.State != RunQueued || done.ApprovedBy != "grace" {
		t.Fatalf("completed approval = %+v", done)
	}
	job = envJob(t, f.testEnv, run.ID)
	if job.Deployment.State != DeploymentApproved || len(job.Deployment.Approvals) != 2 {
		t.Fatalf("deployment after completion = %+v", job.Deployment)
	}
	if got := f.queueEntries(t); got != 2 {
		t.Fatalf("completed approval enqueued %d jobs, want both", got)
	}
	// Once released, a further approval is the documented conflict.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving a released run = %v, want conflict", err)
	}
}

// TestEnvironmentPreventSelfReviewRefusesTheActor pins the rule on the approval
// path: the run's own actor cannot approve, and an admin does not get a
// rubber-stamp through it either (the environment is the authority).
func TestEnvironmentPreventSelfReviewRefusesTheActor(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	f.putEnv(t, "production", EnvironmentInput{
		RequiredReviewers: []string{"ada", "grace"}, PreventSelfReview: true,
	})
	run := f.deployRun(t, CreateOpts{Actor: "ada"})
	_, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "self-review") {
		t.Fatalf("self approval err = %v, want the named refusal", err)
	}
	// The page hides the control from the actor too.
	av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "ada"})
	if av.CanDecide || !av.PreventSelfReview {
		t.Fatalf("actor approval view = %+v", av)
	}
	// An admin who is the actor is refused as well; another reviewer releases it.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada", Admin: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin self approval err = %v", err)
	}
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "grace"}); err != nil {
		t.Fatalf("other reviewer: %v", err)
	}
}

// TestEnvironmentAdminApprovalBypassesTheSet pins the documented bypass: a repo
// admin releases a deployment without the reviewer set, and the audit says so.
func TestEnvironmentAdminApprovalBypassesTheSet(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada", "grace"}})
	run := f.deployRun(t, CreateOpts{})
	admin := auth.Principal{Name: "root", Admin: true}
	released, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, admin)
	if err != nil {
		t.Fatalf("admin approval: %v", err)
	}
	if released.State != RunQueued || released.ApprovedBy != "root" {
		t.Fatalf("admin release = %+v", released)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.Deployment.State != DeploymentApproved || job.Deployment.ApprovedBy != "root" {
		t.Fatalf("admin deployment = %+v", job.Deployment)
	}
	if len(f.audit) == 0 || !strings.Contains(f.audit[len(f.audit)-1], "root "+AuditEnvironmentApprove) {
		t.Fatalf("audit = %v", f.audit)
	}
}

// TestEnvironmentTeamReviewerApproves pins team reviewers end to end: the
// team's members are the required approvers at creation, a member's approval
// counts, and a team deleted after the record was saved fails the deployment
// closed at creation rather than holding it for nobody.
func TestEnvironmentTeamReviewerApproves(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	roster := fakeTeamRoster{teams: map[string][]string{"acme/releasers": {"ada"}}}
	f.svc.TeamRoster = roster
	f.putEnv(t, "production", EnvironmentInput{TeamReviewers: []string{"acme/releasers"}})
	run := f.deployRun(t, CreateOpts{})
	job := envJob(t, f.testEnv, run.ID)
	if strings.Join(job.Deployment.RequiredApprovers, ",") != "ada" {
		t.Fatalf("team approvers = %v", job.Deployment.RequiredApprovers)
	}
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"}); err != nil {
		t.Fatalf("team member approval: %v", err)
	}
	// A team that vanished fails the deployment closed at creation.
	f.svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{}}
	run2 := f.deployRun(t, CreateOpts{})
	job2 := envJob(t, f.testEnv, run2.ID)
	if job2.State != JobFailure || !strings.Contains(job2.Reason, "acme/releasers") {
		t.Fatalf("vanished team job = %s / %q", job2.State, job2.Reason)
	}
	if job2.Deployment.State != DeploymentFailed {
		t.Fatalf("vanished team deployment = %+v", job2.Deployment)
	}
}

// TestEnvironmentDeploymentBranchPolicy pins the ref allowlist: a run from a
// refused ref fails its deployment job with the ref and patterns named, is not
// held for approval, and never resolves environment secrets.
func TestEnvironmentDeploymentBranchPolicy(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"maint"}, DeploymentBranches: []string{"main", "refs/tags/v*"}})
	// main is allowed.
	okRun := f.deployRun(t, CreateOpts{Branch: "main", Ref: "refs/heads/main"})
	if okRun.State != RunPendingApproval {
		t.Fatalf("main run = %s, want the approval hold", okRun.State)
	}
	// A feature branch is refused, with both the ref and the patterns named. The
	// run is a PR edge so the fixture's `on.push.branches: [main]` filter does
	// not silently suppress it (the policy is about the run's ref).
	badRun := f.deployRunAs(t, EventPullReq, CreateOpts{
		Branch: "feature/x", Ref: "refs/heads/feature/x", Action: "opened",
	})
	badJob := envJob(t, f.testEnv, badRun.ID)
	if badJob.State != JobFailure || !strings.Contains(badJob.Reason, `"refs/heads/feature/x"`) ||
		!strings.Contains(badJob.Reason, "main, refs/tags/v*") {
		t.Fatalf("policy refusal = %s / %q", badJob.State, badJob.Reason)
	}
	if badJob.Deployment == nil || badJob.Deployment.State != DeploymentFailed {
		t.Fatalf("policy deployment = %+v", badJob.Deployment)
	}
	if badRun.State == RunPendingApproval {
		t.Fatal("a policy refusal held the run for approval")
	}
	// A matching tag in the full-ref form is allowed.
	tagRun := f.deployRun(t, CreateOpts{Branch: "", Ref: "refs/tags/v1.2.3"})
	if tagRun.State != RunPendingApproval {
		t.Fatalf("tag run = %s, want the approval hold", tagRun.State)
	}
}

// TestEnvironmentWaitTimerHoldsTheClaim pins the timer at the claim path: with
// no reviewer at all, the job is queued but no runner is offered it before the
// deadline; after it, the job is claimable. The payload guard refuses too.
func TestEnvironmentWaitTimerHoldsTheClaim(t *testing.T) {
	e := newEnvFixture(t)
	e.svc.TeamRoster = nil
	e.putEnv(t, "production", EnvironmentInput{WaitTimerSeconds: 3600})
	run := e.deployRun(t, CreateOpts{})
	if run.State != RunQueued || run.ApprovalKind != "" {
		t.Fatalf("timer-only run = %+v, want queued with no approval hold", run)
	}
	job := envJob(t, e.testEnv, run.ID)
	if job.Deployment == nil || job.Deployment.WaitUntil == nil || job.Deployment.State != DeploymentNotRequired {
		t.Fatalf("timer-only deployment = %+v", job.Deployment)
	}
	runner := needsRunner()
	// The build job is claimable; the deployment job is not, before the deadline.
	claim, err := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil || claim == nil || claim.Job.Name != "build" {
		t.Fatalf("first claim = %+v (%v), want the build job", claim, err)
	}
	next, err := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if next != nil {
		t.Fatalf("the wait timer did not hold the deployment (claimed %s)", next.Job.ID)
	}
	if _, err := e.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, &Claim{
		Entry: &QueueEntry{RunID: run.ID, JobID: job.ID, Attempt: 1}, Job: job, Run: run,
	}, job, ""); err == nil || !strings.Contains(err.Error(), "wait timer") {
		t.Fatalf("payload before the deadline = %v, want the named timer refusal", err)
	}
	// Past the deadline the job is claimable and its exports resolve.
	for i := 0; i < 120; i++ {
		e.advance(time.Minute)
	}
	var ship *Claim
	for i := 0; i < 4 && ship == nil; i++ {
		got, cerr := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
		if cerr != nil {
			t.Fatalf("claim: %v", cerr)
		}
		if got == nil {
			break
		}
		if got.Job.Name == "ship" {
			ship = got
			break
		}
		e.svc.releaseClaim(ctx(), "o", "r", got)
	}
	if ship == nil {
		t.Fatal("the deployment was not offered after its timer")
	}
	if _, perr := e.svc.fetchPayload(ctx(), "o", "r", &runnerAuth{Doc: runner}, ship, ship.Job, ""); perr != nil {
		t.Fatalf("payload after the deadline: %v", perr)
	}
}

// TestEnvironmentApprovalPlusTimer pins the interaction: every approval in is
// not enough when the timer has not elapsed; the run is queued but the job
// waits, and the page shows the deadline.
func TestEnvironmentApprovalPlusTimer(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada"}, WaitTimerSeconds: 1800})
	run := f.deployRun(t, CreateOpts{})
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	job := envJob(t, f.testEnv, run.ID)
	if job.Deployment.State != DeploymentApproved || job.Deployment.WaitUntil == nil {
		t.Fatalf("approved deployment with timer = %+v", job.Deployment)
	}
	if waiting, until := deploymentWaitRemaining(job, f.svc.nowUTC()); !waiting || until == "" {
		t.Fatalf("wait remaining = %v %q, want held", waiting, until)
	}
	runner := needsRunner()
	claim, err := f.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim != nil && claim.Job.Name == "ship" {
		t.Fatal("the approved deployment started before its wait timer")
	}
}

// TestPendingDeploymentNotifiesReviewers pins PAR-135's notice: one emission
// naming the run, the environments and the reviewers still awaited, best effort
// (an unwired seam is not an error).
func TestPendingDeploymentNotifiesReviewers(t *testing.T) {
	f := newEnvFixture(t)
	f.roles.roles["ada"] = "write"
	f.roles.roles["grace"] = "write"
	var got []DeploymentNotice
	f.svc.NotifyDeployment = func(_ context.Context, ev DeploymentNotice) { got = append(got, ev) }
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada", "grace"}})
	run := f.deployRun(t, CreateOpts{Actor: "writer"})
	if len(got) != 1 {
		t.Fatalf("notices = %d, want one per hold", len(got))
	}
	ev := got[0]
	if ev.RunID != run.ID || ev.Repo != "o/r" || strings.Join(ev.Environments, ",") != "production" ||
		strings.Join(ev.Reviewers, ",") != "ada,grace" || ev.Actor != "writer" {
		t.Fatalf("notice = %+v", ev)
	}
	// A partial approval notifies only the remainder.
	if _, err := f.svc.ApproveRun(ctx(), "o/r", run.ID, auth.Principal{Name: "ada"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(got) != 2 || strings.Join(got[1].Reviewers, ",") != "grace" {
		t.Fatalf("partial notice = %+v", got)
	}
	// A principal that is both a direct reviewer and a team member is named once.
	f.svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{"acme/releasers": {"ada"}}}
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"ada"}, TeamReviewers: []string{"acme/releasers"}})
	before := len(got)
	f.deployRun(t, CreateOpts{Actor: "writer"})
	if len(got) != before+1 || strings.Join(got[len(got)-1].Reviewers, ",") != "ada" {
		t.Fatalf("duplicate reviewer in the notice = %+v", got)
	}
	// An ungated environment notifies nobody (the record is the authority, so
	// clearing its reviewers is what makes the same job ungated).
	f.putEnv(t, "production", EnvironmentInput{})
	before = len(got)
	f.deployRun(t, CreateOpts{Actor: "writer"})
	if len(got) != before {
		t.Fatalf("an ungated deployment notified reviewers: %+v", got)
	}
}

// TestEnvironmentExpressionInNameIsANamedParseError closes PAR-130's
// fail-closed half: the documented forms are the bare name and the {name,url}
// mapping, and a `${{ … }}` in either is a named parse error for the file (the
// structural-field rule, docs/threads.md §3.6) — never a literal bucket key.
func TestEnvironmentExpressionInNameIsANamedParseError(t *testing.T) {
	for _, doc := range []string{
		"environment: ${{ matrix.env }}",
		"environment: {name: prod, url: \"https://${{ github.sha }}.example.test/\"}",
	} {
		src := "name: Deploy\non: [push]\njobs:\n  ship:\n    runs-on: linux\n    " + doc +
			"\n    steps:\n      - run: 'true'\n"
		_, err := workflow.Parse([]byte(src))
		if err == nil {
			t.Fatalf("%q: parsed, want a named error", doc)
		}
		// Either the structural-field refusal (a `${{ }}` in a field that is
		// read before a matrix cell exists) or the environment-name refusal —
		// both name the field, and neither can become a literal bucket key.
		if !strings.Contains(err.Error(), "environment") ||
			(!errors.Is(err, workflow.ErrBadEnvironment) && !strings.Contains(err.Error(), "does not take ${{ }}")) {
			t.Fatalf("%q: unexpected error: %v", doc, err)
		}
	}
}
