package threads

// Repository-local reusable workflows (PAR-075, docs/threads.md §3.4b).
//
// `jobs.<id>.uses: ./.spindle/workflows/deploy.yml` calls a workflow file from
// the SAME repository at the run's commit. The engine expands the callee's jobs
// into the run before anything is written: each callee job becomes its own job
// record, check and log, named `<caller> / <callee>`. There is no second
// execution path and the runner never sees a `uses:` job — it would refuse one
// (BuildPlan), which is the seam's fail-closed backstop.
//
// The documented subset (docs/WORKFLOWS.md §4.6):
//
//   - `with:` values are the callee's `inputs.*`; the caller may use
//     `github.*`, `inputs.*` and `env.*` in a value, expanded once here;
//   - secrets are the repository's own: a same-repository callee needs no
//     `secrets:` block, and cross-repository `uses:` is REFUSED by name (D-6 —
//     `secrets: inherit` across a repository boundary is exfiltration by
//     design);
//   - `permissions:` is downgrade-only: every callee scope is capped at the
//     caller job's effective scope (downgradePermissions);
//   - a caller job is a CALL, not a runner job: it may not declare steps,
//     runs-on, container, services, env, strategy, timeout, concurrency,
//     environment or outputs. Those are named refusals, never silently ignored;
//   - `if:` on the caller is conjoined with every callee job's own `if:`, and
//     the caller's `needs:` are inherited by every callee job that declares no
//     `needs:` of its own (both are GitHub's rules).
//
// Not in this wave, because the workflow parser does not accept the keys yet
// (reported to the syntax track): `on.workflow_call` (typed inputs, secrets and
// outputs declarations) and `jobs.<id>.secrets`. A callee therefore declares no
// `on:` block at all today. A callee job's `outputs:` are recorded on its job
// record (the API shows them), but a CALLER job cannot reference them yet: a
// `needs.<job>` reference is validated at parse time against the caller file's
// own job keys, and the expanded `caller / callee` keys are not among them. The
// matrix row stays partial until that parser half lands.

import (
	"context"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// MaxReusableDepth bounds nested local reusable workflows (a callee calling a
// callee). It is deliberately small: a chain deeper than this is a mistake, and
// the expansion produces one job record per callee job.
const MaxReusableDepth = 4

// reusableJob is one job the run will create: a plain workflow job, or a job
// expanded out of a local reusable workflow.
type reusableJob struct {
	// key is the job key used for `needs:` resolution and the Job record.
	key string
	// name is the display name; a callee job is `<caller> / <callee>`.
	name string
	// wf/wfName are the file that DEFINES the job (the callee for an expanded
	// job) and its display name.
	wf     *workflow.Workflow
	wfName string
	def    workflow.Job
	// inputs are the values `inputs.*` resolves to in this job.
	inputs map[string]string
	// needs are the workflow job keys this job waits for (already expanded).
	needs []string
	// cap is the caller's effective permission map for a callee job (nil for a
	// plain job).
	cap map[string]string
	// refusal, when set, makes this job a failed record with the named reason:
	// a bad `uses:` fails THAT job, never the run.
	refusal string
}

// planJobDefs turns one workflow file's jobs into the run's job list, expanding
// local reusable workflows. Only infrastructure errors are returned; every
// workflow-level refusal is carried as a job refusal.
func (s *Service) planJobDefs(ctx context.Context, repo, commit string, f *WorkflowFile, inputs map[string]string) ([]reusableJob, error) {
	keys := make([]string, 0, len(f.Workflow.Jobs))
	for k := range f.Workflow.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []reusableJob
	for _, key := range keys {
		def := f.Workflow.Jobs[key]
		display := jobDisplayName(key, def)
		if strings.TrimSpace(def.Uses) == "" {
			out = append(out, reusableJob{
				key: key, name: display, wf: f.Workflow, wfName: f.Name,
				def: def, inputs: inputs, needs: nonNilStr([]string(def.Needs)),
			})
			continue
		}
		expanded, err := s.expandReusable(ctx, repo, commit, f, key, display, def, inputs, 1)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	return out, nil
}

// jobDisplayName is the display name of one job definition.
func jobDisplayName(key string, def workflow.Job) string {
	if name := strings.TrimSpace(def.Name); name != "" {
		return name
	}
	return key
}

// expandReusable resolves one `uses:` caller into its callee jobs (recursively).
// depth is the reusable nesting level (the caller itself is 1).
func (s *Service) expandReusable(ctx context.Context, repo, commit string, wf *WorkflowFile, callerKey, callerName string, caller workflow.Job, inputs map[string]string, depth int) ([]reusableJob, error) {
	refusal := func(reason string) []reusableJob {
		return []reusableJob{{
			key: callerKey, name: callerName, wf: wf.Workflow, wfName: wf.Name,
			def: caller, inputs: inputs, needs: nonNilStr([]string(caller.Needs)), refusal: reason,
		}}
	}
	// A caller job is a call, not a run: every key that would describe a runner
	// job is refused BY NAME. Silently ignoring one would be the
	// accepted-and-inert failure both source cards forbid.
	if bad := reusableCallerRefusal(caller); bad != "" {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s", callerKey, caller.Uses, bad)), nil
	}
	// `with:` needs `on.workflow_call` typed inputs to have a destination. The
	// parser does not accept that key yet (reported to the syntax track), and a
	// value handed to a callee that cannot read `inputs.*` would be inert — so
	// it is a named refusal until the parser half lands.
	if len(caller.With) > 0 {
		names := make([]string, 0, len(caller.With))
		for name := range caller.With {
			names = append(names, name)
		}
		sort.Strings(names)
		return refusal(fmt.Sprintf("job %s (uses: %s): `with:` (%s) needs `on.workflow_call` typed inputs, which this parser does not accept yet; a value handed to a callee that cannot read `inputs.*` would be silently ignored", callerKey, caller.Uses, strings.Join(names, ", "))), nil
	}
	if depth > MaxReusableDepth {
		return refusal(fmt.Sprintf("job %s (uses: %s): reusable workflows are nested deeper than the limit of %d", callerKey, caller.Uses, MaxReusableDepth)), nil
	}
	calleePath, err := localReusablePath(caller.Uses)
	if err != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %v", callerKey, caller.Uses, err)), nil
	}
	if s.Git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	raw, rerr := s.Git.ReadFile(ctx, repo, commit, calleePath)
	if rerr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): could not read %s at the run commit: %v", callerKey, caller.Uses, calleePath, rerr)), nil
	}
	callee, perr := workflow.Parse(raw)
	if perr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s: %v", callerKey, caller.Uses, calleePath, perr)), nil
	}
	if len(callee.Jobs) == 0 {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s declares no jobs", callerKey, caller.Uses, calleePath)), nil
	}
	// The caller's `with:` is refused above until workflow_call lands, so a
	// callee's `inputs.*` namespace is empty (a callee that references one is
	// already a parse error in its own file).
	cap := s.permissionCap(wf.Workflow, caller)
	calleeName := callee.Name
	if strings.TrimSpace(calleeName) == "" {
		calleeName = strings.TrimSuffix(pathpkg.Base(calleePath), pathpkg.Ext(calleePath))
	}
	keys := make([]string, 0, len(callee.Jobs))
	for k := range callee.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []reusableJob
	for _, calleeKey := range keys {
		def := callee.Jobs[calleeKey]
		display := jobDisplayName(calleeKey, def)
		expandedKey := callerKey + " / " + calleeKey
		expandedName := callerName + " / " + display
		if uses := strings.TrimSpace(def.Uses); uses != "" {
			nested, nerr := s.expandReusable(ctx, repo, commit, &WorkflowFile{Path: calleePath, Name: calleeName, Workflow: callee},
				expandedKey, expandedName, def, nil, depth+1)
			if nerr != nil {
				return nil, nerr
			}
			out = append(out, nested...)
			continue
		}
		needs := make([]string, 0, len(caller.Needs)+len(def.Needs))
		for _, key := range caller.Needs {
			needs = appendUnique(needs, key)
		}
		for _, key := range def.Needs {
			needs = appendUnique(needs, callerKey+prefixSeparator+key)
		}
		jobDef := def
		// The caller's condition gates every callee job: `if: false` on the
		// caller must skip the whole callee, and a condition that reads the
		// caller's needs keeps working because both contexts are visible.
		if cond := strings.TrimSpace(caller.If); cond != "" {
			if own := strings.TrimSpace(def.If); own != "" {
				jobDef.If = "(" + cond + ") && (" + own + ")"
			} else {
				jobDef.If = cond
			}
		}
		jobDef.Needs = needs
		out = append(out, reusableJob{
			key: expandedKey, name: expandedName, wf: callee, wfName: calleeName,
			def: jobDef, inputs: nil, needs: needs, cap: cap,
		})
	}
	return out, nil
}

// prefixSeparator joins a caller key and a callee key into the expanded job key.
const prefixSeparator = " / "

// reusableCallerRefusal returns why a caller job is invalid ("" when it is a
// valid call). Every key is checked explicitly so a typo cannot become an
// inert field on a job nobody runs.
func reusableCallerRefusal(caller workflow.Job) string {
	switch {
	case len(caller.Steps) > 0:
		return "a reusable-workflow caller job may not declare `steps` (the callee's steps run instead)"
	case len(caller.RunsOn) > 0:
		return "a reusable-workflow caller job may not declare `runs-on` (the callee's jobs choose their runners)"
	case caller.Container != nil:
		return "a reusable-workflow caller job may not declare `container` (each callee job declares its own)"
	case len(caller.Services) > 0:
		return "a reusable-workflow caller job may not declare `services` (each callee job declares its own)"
	case len(caller.Env) > 0:
		return "a reusable-workflow caller job may not declare `env`: a called workflow does not inherit the caller's environment (pass a `with:` input instead)"
	case caller.Strategy != nil:
		return "a reusable-workflow caller job may not declare `strategy`: a matrix caller is not supported in this wave (put the matrix in the callee, or call once per cell)"
	case caller.TimeoutMinutes != 0:
		return "a reusable-workflow caller job may not declare `timeout-minutes`; set it on the callee's jobs"
	case caller.Concurrency != nil:
		return "a reusable-workflow caller job may not declare `concurrency`; set it on the callee's jobs"
	case caller.Environment != nil:
		return "a reusable-workflow caller job may not declare `environment`; set it on the callee's jobs"
	case len(caller.Outputs) > 0:
		return "a reusable-workflow caller job may not declare `outputs`: workflow-level outputs need `on.workflow_call.output`, which this parser does not accept yet; read the callee job's output as `needs.<caller> / <job>.outputs.<name>`"
	}
	return ""
}

// localReusablePath resolves a local `uses:` reference to a repository path.
// Only `.spindle/workflows/<file>.yml|.yaml` is accepted: that is the directory
// the engine discovers, so a callee is a file the run could have been created
// from. A remote/foreign reference is refused by name (D-6).
func localReusablePath(uses string) (string, error) {
	raw := strings.TrimSpace(uses)
	if raw == "" {
		return "", fmt.Errorf("uses is empty")
	}
	lower := strings.ToLower(raw)
	if strings.Contains(raw, "@") || strings.HasPrefix(lower, "docker://") || strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("only a repository-local reusable workflow is supported: use `./.spindle/workflows/<name>.yml` (cross-repository callers are refused by decision D-6: a secret passed across a repository boundary is exfiltration by design)")
	}
	if !strings.HasPrefix(raw, "./") {
		return "", fmt.Errorf("a local reusable workflow must start with `./` (e.g. `./.spindle/workflows/deploy.yml`); %q is not a repository path", raw)
	}
	clean := pathpkg.Clean(strings.TrimPrefix(raw, "./"))
	if clean == "." || strings.HasPrefix(clean, "../") || pathpkg.IsAbs(clean) {
		return "", fmt.Errorf("%q is not a path inside this repository", raw)
	}
	if pathpkg.Dir(clean) != workflow.Dir {
		return "", fmt.Errorf("%q is not in %s/ (the only directory this engine reads)", raw, workflow.Dir)
	}
	switch pathpkg.Ext(clean) {
	case ".yml", ".yaml":
	default:
		return "", fmt.Errorf("%q must name a .yml or .yaml file", raw)
	}
	return clean, nil
}

// permissionCap is the caller job's effective permission map, the ceiling a
// callee's declared scopes are capped at. It reuses the token resolver, so the
// cap can never disagree with what the caller's own job would be granted.
func (s *Service) permissionCap(wf *workflow.Workflow, caller workflow.Job) map[string]string {
	return s.EffectivePermissions(&Spec{Permissions: mergePermissions(wf.Permissions, caller.Permissions)}, true)
}

// appendUnique appends a value not already present.
func appendUnique(list []string, value string) []string {
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}
