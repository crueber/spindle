package workflow

// Context availability (PAR-040, docs/WORKFLOWS.md §6.2/§6.3,
// docs/threads.md §3.6/§3.7).
//
// GitHub documents, per key, which `${{ … }}` contexts a site may read. The
// table below is the same contract for spindle: a reference outside its site's
// set is the file's PARSE error naming the site and the available contexts —
// never an empty value that silently changes a condition. It is one table so a
// new site cannot be added with an implicit "everything is allowed".
//
// The table is deliberately conservative where the engine has no value yet:
// `vars` is not readable in a concurrency group or a run title, `runner` exists
// only once a job has a runner, and `secrets` is readable in a job-level `if:`
// (PAR-058) but not in a name or a lock key (neither is a place a secret value
// may become visible).
//
// `job` and `strategy` (PAR-039) follow GitHub's rows: `job.status` describes
// the job WHILE its steps run, so it exists only in a step field and a step
// `if:`; `strategy` describes the matrix cell and is readable wherever a cell
// exists (a job name, its lock key, its outputs, runs-on and timeouts) but not
// in a job-level `if:`, which decides whether a cell is created at all.

// Site names one authored position that may carry a `${{ … }}` span.
type Site int

const (
	// SiteStepField is a step's substituted field: name, run, with, env,
	// shell, working-directory.
	SiteStepField Site = iota
	// SiteStepIf is a step-level `if:`.
	SiteStepIf
	// SiteJobIf is a job-level `if:` (decided by the server before a runner).
	SiteJobIf
	// SiteJobName is a job's display name (expanded at run creation).
	SiteJobName
	// SiteJobGroup is a job-level `concurrency.group`.
	SiteJobGroup
	// SiteWorkflowGroup is a workflow-level `concurrency.group` (expanded once
	// per run, before any matrix exists).
	SiteWorkflowGroup
	// SiteJobOutputs is a job output template (evaluated by the runner after
	// the steps).
	SiteJobOutputs
	// SiteRunName is the workflow-level `run-name:` title (PAR-057).
	SiteRunName
	// SiteRunsOn is a `runs-on:` label (PAR-053).
	SiteRunsOn
	// SiteDefaultsRun is `defaults.run.*`: literal by decision, so it admits no
	// context at all.
	SiteDefaultsRun
	// SiteCallWith is a reusable caller's `with:` value (expanded at run
	// creation, before any step or secret exists).
	SiteCallWith
	// SiteCallOutput is an `on.workflow_call.outputs.<name>.value` template.
	SiteCallOutput
	// SiteJobTimeout is a job-level `timeout-minutes` expression.
	SiteJobTimeout
	// SiteStepTimeout is a step-level `timeout-minutes` expression.
	SiteStepTimeout
)

// String renders a site for an error message and the docs table.
func (s Site) String() string {
	switch s {
	case SiteStepIf:
		return "a step if:"
	case SiteJobIf:
		return "a job if:"
	case SiteJobName:
		return "a job name"
	case SiteJobGroup:
		return "a job concurrency.group"
	case SiteWorkflowGroup:
		return "a workflow concurrency.group"
	case SiteJobOutputs:
		return "a job outputs value"
	case SiteRunName:
		return "run-name"
	case SiteRunsOn:
		return "runs-on"
	case SiteDefaultsRun:
		return "defaults.run"
	case SiteCallWith:
		return "a reusable-workflow with: value"
	case SiteCallOutput:
		return "a workflow_call output value"
	case SiteJobTimeout:
		return "a job timeout-minutes"
	case SiteStepTimeout:
		return "a step timeout-minutes"
	default:
		return "a step field"
	}
}

// contextAvailability is the matrix: a context name → the sites that may read
// it. The github/forgejo/forge aliases share one entry because they are
// value-identical (D-1's compatibility aliases, PAR-004).
var contextAvailability = map[string]map[Site]bool{
	"github": setOf(
		SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobGroup,
		SiteWorkflowGroup, SiteJobOutputs, SiteRunName, SiteRunsOn, SiteCallWith,
		SiteJobTimeout, SiteStepTimeout,
	),
	"runner": setOf(SiteStepField, SiteStepIf, SiteJobOutputs),
	"vars":   setOf(SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobOutputs),
	"matrix": setOf(SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobGroup, SiteJobOutputs, SiteRunsOn, SiteJobTimeout, SiteStepTimeout),
	"inputs": setOf(
		SiteStepField, SiteStepIf, SiteJobIf, SiteJobName, SiteJobGroup,
		SiteWorkflowGroup, SiteJobOutputs, SiteRunName, SiteRunsOn, SiteCallWith,
		SiteJobTimeout, SiteStepTimeout,
	),
	"env":     setOf(SiteStepField, SiteStepIf, SiteJobIf, SiteJobOutputs),
	"secrets": setOf(SiteStepField, SiteStepIf, SiteJobIf, SiteJobOutputs),
	"steps":   setOf(SiteStepField, SiteStepIf, SiteJobOutputs),
	"needs":   setOf(SiteStepField, SiteStepIf, SiteJobIf, SiteJobOutputs),
	"jobs":    setOf(SiteCallOutput),
	// job.status (PAR-039) is the running job's own status, so only a step
	// (its fields and its condition) may read it.
	"job": setOf(SiteStepField, SiteStepIf),
	// strategy.* (PAR-039) describes the matrix cell: a plain job has an empty
	// strategy context, and a job-level `if:` may not read it (it runs before
	// the cell exists — GitHub's own row).
	"strategy": setOf(
		SiteStepField, SiteStepIf, SiteJobName, SiteJobGroup, SiteJobOutputs,
		SiteRunsOn, SiteJobTimeout, SiteStepTimeout,
	),
}

// setOf is a tiny literal constructor (no dependency, no mutable global).
func setOf(sites ...Site) map[Site]bool {
	out := make(map[Site]bool, len(sites))
	for _, s := range sites {
		out[s] = true
	}
	return out
}

// SiteContexts lists the contexts a site may read, in documentation order.
func SiteContexts(site Site) []string {
	order := []string{"github", "runner", "vars", "matrix", "strategy", "job", "inputs", "env", "secrets", "steps", "needs", "jobs"}
	out := make([]string, 0, len(order))
	for _, context := range order {
		if contextAvailability[context][site] {
			out = append(out, context)
		}
	}
	return out
}

// contextAvailable reports whether one context may be read at one site. The
// forgejo/forge aliases resolve to github's entry.
func contextAvailable(site Site, context string) bool {
	context = normalizeContextName(context)
	return contextAvailability[context][site]
}

// normalizeContextName maps the compatibility aliases onto the canonical
// context name.
func normalizeContextName(context string) string {
	switch context {
	case "forgejo", "forge":
		return "github"
	}
	return context
}

// siteAllowsHashFiles reports whether hashFiles() may appear at a site. It
// reads the job workspace inside the container, so only runner-evaluated sites
// (a step, or a job output evaluated after the steps) qualify; a server-side
// condition has no workspace and gets a named parse error.
func siteAllowsHashFiles(site Site) bool {
	switch site {
	case SiteStepField, SiteStepIf, SiteJobOutputs:
		return true
	}
	return false
}
