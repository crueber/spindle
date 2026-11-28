package threads

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	wf "github.com/crueber/spindle/internal/threads/workflow"
)

// Job-level `if:` and `continue-on-error` (15 §4.1, 2026-11-21). Both keys are
// parsed by the workflow package, so before this they were accepted but inert.
//
// Evaluation point. The job `if:` is decided by the SERVER, before the job is
// claimable: the queue owns eligibility (needs.go), so a false condition must
// terminalise the job (`skipped`, with the reason on the record) without a
// runner and without a queue entry that a probe has to keep refusing.
//
// Contexts: the same subset the step `if:` evaluates (internal/threads/expr.go)
// plus the job-level ones that exist here — `github.*`, `matrix.*`, `inputs.*`,
// `env.*`, `needs.<job>.result`, `needs.<job>.outputs.<name>` (a successful
// dependency's reported outputs), and success()/failure()/cancelled() derived
// from the dependency results. `steps.*` is a runner context and is rejected at
// parse time in a job-level `if:` (it reads empty if a stored workflow reaches
// the evaluator).
//
// Two moments decide a condition:
//   - run creation / approval / rerun: a job with no `needs:` is decided
//     immediately (nothing can change its answer), and a false one is written
//     `skipped` and never enqueued;
//   - the `needs:` cascade (needs.go): a dependent is decided once every
//     dependency is terminal, so `needs.<job>.result` is a fact.
//
// PAR-058: a job `if:` may read `secrets.*` and `vars.*`. A trusted run reads
// the decrypted values; an untrusted (fork) run reads an EMPTY secrets map —
// the reference resolves, the value is absent, and the condition is false
// rather than an error. The values are never logged: only the authored
// expression appears in the job's `reason`.
//
// A job with a non-empty `if:` that evaluates TRUE runs even when a dependency
// failed — that is the point of writing `if: always()` or
// `if: needs.build.result == 'failure'`. A job without one keeps the default:
// every dependency must have succeeded.

// jobIfVerdict is one job-level `if:` decision.
type jobIfVerdict int

const (
	// jobIfRun: the condition holds (or there is none): the job may run.
	jobIfRun jobIfVerdict = iota
	// jobIfSkip: the condition is false: the job is skipped.
	jobIfSkip
	// jobIfError: the expression is outside the supported subset.
	jobIfError
)

// evaluateJobIf decides one job's job-level `if:`. facts is the folded view of
// the job's `needs:` (results and outputs); it is empty for a job with no
// dependencies. run supplies the github.* context and may be a header the
// caller has not written yet (run creation).
// jobIfValues are the extra server-side contexts a job `if:` may read
// (PAR-058). An untrusted run gets an empty Secrets map, never an error.
type jobIfValues struct {
	Secrets map[string]string
	Vars    map[string]string
}

func evaluateJobIf(job *Job, run *Run, facts needFacts) (jobIfVerdict, string, error) {
	return evaluateJobIfWith(job, run, facts, jobIfValues{})
}

// evaluateJobIfWith is evaluateJobIf with the decrypted secrets/vars the server
// resolved for this run (PAR-058).
func evaluateJobIfWith(job *Job, run *Run, facts needFacts, extra jobIfValues) (jobIfVerdict, string, error) {
	if job == nil {
		return jobIfRun, "", nil
	}
	expr := ""
	if job.Spec != nil {
		expr = strings.TrimSpace(job.Spec.If)
	}
	// The default: every dependency must have succeeded. A job with no
	// dependencies trivially qualifies.
	if expr == "" {
		for key, res := range facts.Results {
			if res != JobSuccess {
				return jobIfSkip, fmt.Sprintf("needs: %s concluded %s", key, res), nil
			}
		}
		return jobIfRun, "", nil
	}
	expr = SubstituteInputs(SubstituteMatrix(expr, specMatrix(job)), specInputs(job))
	ctx := jobIfContextWith(job, run, facts, extra)
	ok, err := EvalIf(expr, ctx)
	if err != nil {
		return jobIfError, err.Error(), err
	}
	if !ok {
		return jobIfSkip, "if: " + expr, nil
	}
	return jobIfRun, "", nil
}

// loadJobIfValues resolves the PAR-058 contexts a job `if:` may read. An
// untrusted run gets an empty secrets map (never a store read, never an error);
// the values are used for the decision only and never logged.
func (s *Service) loadJobIfValues(ctx context.Context, owner, repo string, job *Job, run *Run) (jobIfValues, error) {
	var out jobIfValues
	if job == nil || job.Spec == nil {
		return out, nil
	}
	if job.Spec.IfSecrets {
		if run != nil && run.Trusted {
			vals, err := s.secretValues(ctx, owner, repo)
			if err != nil {
				return out, err
			}
			out.Secrets = vals
		} else {
			out.Secrets = map[string]string{}
		}
	}
	if job.Spec.IfVars {
		vals, err := s.variableValues(ctx, owner, repo)
		if err != nil {
			return out, err
		}
		out.Vars = vals
	}
	return out, nil
}

// jobIfContext builds the `if` context of one job record. The github.* keys are
// the same ones the runner exposes to a step (plan.go builtinEnv), so a
// condition written once behaves the same at job and step level.
func jobIfContext(job *Job, run *Run, facts needFacts) ExprContext {
	return jobIfContextWith(job, run, facts, jobIfValues{})
}

func jobIfContextWith(job *Job, run *Run, facts needFacts, extra jobIfValues) ExprContext {
	wfName := ""
	if run != nil {
		wfName = run.Workflow
	}
	if job != nil {
		wfName = job.specWorkflow(wfName)
	}
	success, failure, cancelled := needsSummary(facts.Results)
	github := githubContext(run, wfName)
	if run != nil {
		github["job"] = jobIdentifier(job)
	}
	return ExprContext{
		Success: success, Failure: failure, Cancelled: cancelled,
		GitHub: github, Matrix: specMatrix(job), Inputs: specInputs(job),
		Env: specEnv(job), Needs: facts.Results, NeedsOutputs: facts.Outputs,
		Secrets: extra.Secrets, Vars: extra.Vars, Site: wf.SiteJobIf,
	}
}

// githubContext builds the run's github.* map (the keys TemplateGitHubFields
// lists). It is shared by the `if` evaluator and the concurrency-group/job-name
// expansion, so one reference means one value.
func githubContext(run *Run, workflow string) map[string]string {
	if run == nil {
		return map[string]string{}
	}
	ref := strings.TrimSpace(run.Ref)
	if ref == "" && strings.TrimSpace(run.Branch) != "" {
		ref = "refs/heads/" + strings.TrimSpace(run.Branch)
	}
	fields := EventFieldsOf(run.EventPayload)
	if fields == nil {
		fields = map[string]string{}
	}
	fields["ref"] = ref
	fields["ref_name"] = runRefName(run)
	fields["ref_type"] = oidcRefType(ref)
	fields["sha"] = run.Commit
	fields["event_name"] = run.Event
	fields["workflow"] = workflow
	fields["repository"] = run.Repo
	fields["repository_owner"] = ownerPart(run.Repo)
	fields["run_id"] = run.ID
	fields["run_attempt"] = strconv.Itoa(run.Attempt)
	fields["actor"] = run.Actor
	// A re-run keeps its triggering actor: spindle has no separate "re-run by"
	// identity, so the two fields carry the same real value (PAR-032).
	fields["triggering_actor"] = run.Actor
	// The workflow file's own identity: the path it was read from at the commit
	// it was read at (a fork PR reads the base tip, which is what these mean).
	// Without a ref the `@<ref>` suffix would be malformed, so the field stays
	// empty (a real value, not a broken one).
	if ref != "" {
		fields["workflow_ref"] = oidcJobWorkflowRef(run.Repo, run.WorkflowPath, ref)
	}
	fields["workflow_sha"] = run.WorkflowCommit
	// A pull request's head and base branch names come from the bounded event
	// payload; every other event leaves them empty, exactly as GitHub does.
	fields["head_ref"] = fields["event.pull_request.head.ref"]
	fields["base_ref"] = fields["event.pull_request.base.ref"]
	return fields
}

// runRefName is `github.ref_name`: the tag name for a tag push ("v1.2.3" out
// of "refs/tags/v1.2.3"), otherwise the branch name. A pull request's identity
// is its BASE branch (matching `github.ref`'s GITHUB_REF rule, 15 §3.5), so its
// refs/pull/<n>/head Ref never leaks the PR number here. A run with neither
// answers "" — never the ref with its prefix stripped blindly. The caller
// (githubContext) has already handled a nil run.
func runRefName(run *Run) string {
	if tag, ok := strings.CutPrefix(strings.TrimSpace(run.Ref), "refs/tags/"); ok {
		return tag
	}
	if branch := strings.TrimSpace(run.Branch); branch != "" {
		return branch
	}
	// A merge_group (PAR-016) has no branch: its identity is the queue ref's
	// short name (`gh-readonly-queue/main/pr-7-<sha>`). A run with neither a
	// branch nor a ref answers "" — never the ref with its prefix stripped
	// blindly.
	return shortRefName(run.Ref)
}

// jobIdentifier is the GITHUB_JOB spelling of a job record (its id).
func jobIdentifier(job *Job) string {
	if job == nil {
		return ""
	}
	return job.ID
}

// needsSummary folds dependency results into the success()/failure()/
// cancelled() functions of a job-level `if:`.
func needsSummary(results map[string]string) (success, failure, cancelled bool) {
	success = true
	for _, res := range results {
		switch res {
		case JobSuccess:
			// neutral
		case JobFailure, JobTimedOut:
			success, failure = false, true
		case JobCancelled:
			success, cancelled = false, true
		default:
			success = false
		}
	}
	return success, failure, cancelled
}

// specMatrix/specInputs/specEnv read a job spec's maps nil-safely.
func specMatrix(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Matrix
}

func specInputs(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Inputs
}

func specEnv(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Env
}

// entryNeedMap resolves one job's `needs:` to key → job ids (sorted), so a
// caller can both gate on the ids and evaluate `needs.<key>.result` without a
// second index. A key with no matching jobs yields no entry.
func entryNeedMap(idx map[string][]string, job *Job) map[string][]string {
	if job == nil || len(job.Needs) == 0 {
		return nil
	}
	out := make(map[string][]string, len(job.Needs))
	for _, key := range job.Needs {
		ids := idx[key]
		if len(ids) == 0 {
			continue
		}
		out[key] = append([]string{}, ids...)
	}
	return out
}
