package threads

// The cross-run artifact gate (PAR-098, decision D-3).
//
// A `download-artifact` step that names another run's id is the untrusted-parent
// vector: bytes one run produced become input to a second run. D-3 makes that a
// per-run maintain decision, never automatic, and never for a job that holds a
// secret. This file owns the three checks:
//
//   - creation: a fork run is refused by name, and a secrets-bearing job is
//     refused by name (the job fails with the reason, the rest of the run runs);
//   - the hold: any remaining cross-run consumer holds the whole run in
//     `pending_approval` (ApprovalCrossRun), exactly like the fork gate;
//   - approval: the maintainer's decision is re-validated against the jobs (the
//     spec is immutable, but a re-run re-uses this path) and audited.
//
// "Secrets-bearing" means the job's authored spec references a repository or
// environment secret. The built-in job token is not a repository secret: every
// job gets it, so counting it would make the feature unusable and say nothing
// about the risk D-3 names.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// secretRefRe matches `secrets.<name>` in an authored template.
var secretRefRe = regexp.MustCompile(`secrets\.([A-Za-z_][A-Za-z0-9_]*)`)

// builtinSecretNames are the token aliases every job receives; they are not
// repository secrets for the D-3 rule.
var builtinSecretNames = map[string]bool{
	"GITHUB_TOKEN":      true,
	"FORGEJO_TOKEN":     true,
	"SPINDLE_JOB_TOKEN": true,
}

// stepWantsCrossRun reports whether one step names another run's artifact.
func stepWantsCrossRun(st StepSpec) bool {
	uses := strings.ToLower(strings.TrimSpace(st.Uses))
	base := uses
	if at := strings.IndexByte(base, '@'); at >= 0 {
		base = base[:at]
	}
	if base != "actions/download-artifact" {
		return false
	}
	for key, val := range st.With {
		if strings.EqualFold(strings.TrimSpace(key), "run-id") && strings.TrimSpace(val) != "" {
			return true
		}
	}
	return false
}

// jobWantsCrossRun reports whether any step of the job (or of a repository-local
// action it expands) names another run's artifact.
func jobWantsCrossRun(job *Job) bool {
	if job == nil || job.Spec == nil {
		return false
	}
	if stepsWantCrossRun(job.Spec.Steps) {
		return true
	}
	for _, action := range job.Spec.Actions {
		if stepsWantCrossRun(action.Steps) {
			return true
		}
	}
	return false
}

// stepsWantCrossRun scans one step list.
func stepsWantCrossRun(steps []StepSpec) bool {
	for _, st := range steps {
		if stepWantsCrossRun(st) {
			return true
		}
	}
	return false
}

// anyJobWantsCrossRun reports whether any job of the run consumes another run's
// artifacts.
func anyJobWantsCrossRun(jobs []*Job) bool {
	for _, job := range jobs {
		if jobWantsCrossRun(job) {
			return true
		}
	}
	return false
}

// stringsHoldRepoSecret reports whether any authored template in spec references
// a repository/environment secret (the D-3 "secrets-bearing" rule).
func specHoldsRepoSecret(spec *Spec) bool {
	if spec == nil {
		return false
	}
	if spec.Environment != nil {
		// An environment carries its own secrets; a deployment job is
		// secrets-bearing by definition.
		return true
	}
	check := func(values ...string) bool {
		for _, v := range values {
			for _, m := range secretRefRe.FindAllStringSubmatch(v, -1) {
				if !builtinSecretNames[m[1]] {
					return true
				}
			}
		}
		return false
	}
	if check(spec.If) {
		return true
	}
	for _, v := range spec.Env {
		if check(v) {
			return true
		}
	}
	inspect := func(steps []StepSpec) bool {
		for _, st := range steps {
			if check(st.If, st.Run, st.Name, st.WorkingDirectory) {
				return true
			}
			for _, v := range st.With {
				if check(v) {
					return true
				}
			}
			for _, v := range st.Env {
				if check(v) {
					return true
				}
			}
		}
		return false
	}
	if inspect(spec.Steps) {
		return true
	}
	for _, action := range spec.Actions {
		if inspect(action.Steps) {
			return true
		}
		for _, v := range action.Env {
			if check(v) {
				return true
			}
		}
	}
	return false
}

// crossRunJobRefusal names why a job may not consume another run's artifacts
// ("" when it may, or when it does not ask).
func crossRunJobRefusal(job *Job, trusted bool) string {
	if !jobWantsCrossRun(job) {
		return ""
	}
	if !trusted {
		return "a fork run cannot download another run's artifacts (D-3: cross-run consumption requires a trusted run)"
	}
	if specHoldsRepoSecret(job.Spec) {
		return "a secrets-bearing job cannot download another run's artifacts (D-3: cross-run consumption is never granted to a job that holds secrets)"
	}
	return ""
}

// crossRunApprovalRefusal re-validates the D-3 conditions at decision time
// ("" when the approval may proceed).
func crossRunApprovalRefusal(run *Run, jobs []*Job) string {
	if run == nil || !anyJobWantsCrossRun(jobs) {
		return "this run does not consume another run's artifacts"
	}
	if !run.Trusted {
		return "a fork run cannot be approved for cross-run artifact consumption (D-3)"
	}
	for _, job := range jobs {
		if reason := crossRunJobRefusal(job, true); reason != "" {
			return reason
		}
	}
	return ""
}

// approvalWorkflowRunKind mirrors the triggers track's `ApprovalWorkflowRun`
// value (PAR-019): a workflow_run whose upstream run was untrusted is held
// until a maintainer approves it. It is spelled as an unexported constant so
// this package's cross-run contract holds whether or not that constant is in the
// tree yet — the value is the wire/record spelling either way, and it does not
// collide with the exported one when it lands.
const approvalWorkflowRunKind = "workflow_run"

// workflowRunParent names the upstream run of a workflow_run consumer: the
// documented event payload projection carries it (and the triggers track also
// records it on the run header). The held run itself is the fallback so a
// refusal always names SOMETHING an operator can look up.
func workflowRunParent(run *Run) string {
	if run != nil && run.EventPayload != nil {
		if wr, ok := run.EventPayload["workflow_run"].(map[string]any); ok {
			switch v := wr["id"].(type) {
			case string:
				if id := strings.TrimSpace(v); id != "" {
					return id
				}
			case int:
				return strconv.Itoa(v)
			case int64:
				return strconv.FormatInt(v, 10)
			case float64:
				return strconv.FormatInt(int64(v), 10)
			}
		}
	}
	if run == nil {
		return ""
	}
	return run.ID
}

// workflowRunHoldRefusal is the cross-run refusal for a run held on its
// workflow_run parent (409, D-3): the consuming run may not download another
// run's artifact — nor attach a cross-run input — until the parent's approval
// decision lands. Nil when the run carries no such hold.
func workflowRunHoldRefusal(consumer *Run) error {
	if consumer == nil || consumer.State != RunPendingApproval || consumer.ApprovalKind != approvalWorkflowRunKind {
		return nil
	}
	return fmt.Errorf("%w: workflow_run approval pending: this run consumes artifacts of parent run %s; a maintainer must approve it first (D-3)",
		ErrConflict, workflowRunParent(consumer))
}
