package threads

// Reusable workflows (PAR-075 local, PAR-076 cross-repository; docs/threads.md
// §3.4b, docs/WORKFLOWS.md §4.5).
//
// `jobs.<id>.uses: ./.spindle/workflows/deploy.yml` calls a workflow file from
// the SAME repository at the run's commit; `owner/repo/.spindle/workflows/x.yml@<sha>`
// calls one from another repository on this instance at a pinned commit. The
// engine expands the callee's jobs into the run before anything is written: each
// callee job becomes its own job record, check and log, named
// `<caller> / <callee>`. There is no second execution path and the runner never
// sees a `uses:` job — it would refuse one (BuildPlan), which is the seam's
// fail-closed backstop.
//
// The contract (all of it enforced here or at parse time):
//
//   - the callee MUST declare `on.workflow_call`; its typed inputs are the only
//     `with:` names, its declared secrets the only secrets it may read, and its
//     outputs the only names a caller can consume;
//   - `with:` values are expanded ONCE here, from `github.*` and the caller's
//     own `inputs.*` (the parser refuses every other context in a `with:`
//     value); unknown inputs, missing required inputs and type mismatches are
//     named refusals on the caller job;
//   - `secrets:` is an explicit callee-name → `${{ secrets.SOURCE }}` map, or
//     `inherit` (same repository only — cross-repository inheritance is D-6, a
//     named parse error; a remote callee gets exactly the listed secrets and
//     nothing when it lists none);
//   - the callee's declared outputs are evaluated when its jobs finish and are
//     readable by a later job as `needs.<caller>.outputs.<name>`;
//   - `permissions:` is downgrade-only: every callee scope is capped at the
//     caller job's effective scope (permissionCap/downgradePermissions);
//   - a caller job is a CALL, not a runner job: it may not declare steps,
//     runs-on, container, services, env, strategy, timeout, concurrency,
//     environment or outputs (named refusals, never silently ignored);
//   - `if:` on the caller is conjoined with every callee job's own `if:`, and
//     the caller's `needs:` are inherited by every callee job that declares no
//     `needs:` of its own (GitHub's rules);
//   - nesting is capped at MaxReusableDepth and a call chain is cycle-checked by
//     (repository, path), so a cyclic chain is a named error rather than a
//     stack;
//   - a cross-repository callee must be readable without credentials (public on
//     this instance) and is resolved at a 40-hex commit SHA; a branch/tag ref is
//     refused by name, and every cross-repository call is written to the host
//     audit family (never a secret value).

import (
	"context"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// MaxReusableDepth bounds nested reusable workflows (a callee calling a
// callee). It is deliberately small: a chain deeper than this is a mistake, and
// the expansion produces one job record per callee job.
const MaxReusableDepth = 4

// AuditReusableCrossRepo is the host-audit verb for a cross-repository reusable
// workflow call (PAR-076). The detail names the pinned commit and the caller; it
// never contains a secret value.
const AuditReusableCrossRepo = "reusable_cross_repo"

// reusableJob is one job the run will create: a plain workflow job, or a job
// expanded out of a reusable workflow.
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
	// call and secrets are the reusable-call context an expanded job carries
	// into its Spec (nil for a plain job): the caller key plus the callee's
	// declared output templates, and the closed secret set the callee receives.
	call    *callSpec
	secrets *reusableSecrets
	// refusal, when set, makes this job a failed record with the named reason:
	// a bad `uses:` fails THAT job, never the run.
	refusal string
}

// callSpec is persisted in a callee job's Spec: the call it belongs to and the
// callee's declared output templates. Every job of one call carries the same
// value, so the needs fold can evaluate the caller's outputs from the callee
// jobs' reported outputs (foldNeeds/callOutputs).
type callSpec struct {
	Caller  string            `json:"caller"`
	Path    string            `json:"path"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

// reusableSecrets is the closed secret set of one expanded callee job
// (PAR-075/D-6). Inherit passes the repository's whole set (same repository
// only); Names maps the callee-visible name to the repository secret it comes
// from. An ordinary job has no reusableSecrets and keeps the repository's
// secrets.
type reusableSecrets struct {
	Inherit bool              `json:"inherit,omitempty"`
	Names   map[string]string `json:"names,omitempty"`
}

// planContext is the run-creation state a reusable call needs beyond the
// repository and commit: the github.* values a `with:` value may read, and the
// actor/run id for the cross-repository audit record.
type planContext struct {
	github map[string]string
	actor  string
	runID  string
	// runRepo is the repository the RUN belongs to (and whose secrets the
	// claim decrypts). A callee in any other repository is cross-repository for
	// secret purposes even when reached through a nested LOCAL `./` call inside
	// a remote callee.
	runRepo string
}

// planJobDefs turns one workflow file's jobs into the run's job list, expanding
// reusable workflows. Only infrastructure errors are returned; every
// workflow-level refusal is carried as a job refusal.
func (s *Service) planJobDefs(ctx context.Context, repo, commit string, f *WorkflowFile, inputs map[string]string, pc planContext) ([]reusableJob, error) {
	keys := make([]string, 0, len(f.Workflow.Jobs))
	for k := range f.Workflow.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []reusableJob
	calls := map[string]map[string]string{}
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
		expanded, outputs, err := s.expandReusable(ctx, repo, commit, f, key, display, def, inputs, pc, 1, nil, key, nil, nil)
		if err != nil {
			return nil, err
		}
		// A successful call registers its interface even when it declares no
		// outputs: a later `needs.<caller>.outputs.<name>` must be refused by
		// name, not left to the runner.
		if len(expanded) > 0 && expanded[0].call != nil {
			calls[key] = outputs
		}
		out = append(out, expanded...)
	}
	// A caller's outputs are the callee's interface; a later job that reads
	// `needs.<caller>.outputs.<name>` for a name the callee does not declare is
	// a named refusal on THAT job (the parser can only check the job key — the
	// callee is another file).
	if len(calls) > 0 {
		for i := range out {
			if out[i].refusal != "" {
				continue
			}
			if reason := undeclaredNeedsOutput(out[i], calls); reason != "" {
				out[i].refusal = reason
			}
		}
	}
	return out, nil
}

// undeclaredNeedsOutput reports the first `needs.<caller>.outputs.<name>`
// reference in one job definition whose name the callee does not declare.
func undeclaredNeedsOutput(jd reusableJob, calls map[string]map[string]string) string {
	for _, field := range jobStrings(jd.def) {
		refs, err := workflow.NeedsOutputNames(field.value)
		if err != nil {
			continue
		}
		for _, ref := range refs {
			declared, ok := calls[ref.Job]
			if !ok {
				continue // an ordinary job dependency: the parser validated it
			}
			if _, ok := declared[ref.Output]; !ok {
				return fmt.Sprintf("job %s %s: %s names no output of reusable workflow %q (declared: %s)",
					jd.key, field.name, ref.Raw, ref.Job, listOrNone(sortedKeys(declared)))
			}
		}
	}
	return ""
}

// jobStrings lists every string site of one job definition that may read a
// `needs.<job>.outputs.<name>` reference. It is used only for the caller-output
// check above; the parse-time validators own the structure.
func jobStrings(def workflow.Job) []struct{ name, value string } {
	out := []struct{ name, value string }{
		{"name", def.Name},
		{"if", def.If},
	}
	for _, name := range sortedKeys(def.Outputs) {
		out = append(out, struct{ name, value string }{"outputs." + name, def.Outputs[name]})
	}
	for i, st := range def.Steps {
		prefix := fmt.Sprintf("step %d", i+1)
		out = append(out,
			struct{ name, value string }{prefix + " name", st.Name},
			struct{ name, value string }{prefix + " run", st.Run},
			struct{ name, value string }{prefix + " if", st.If},
			struct{ name, value string }{prefix + " shell", st.Shell},
			struct{ name, value string }{prefix + " working-directory", st.WorkingDirectory},
		)
		for _, k := range sortedKeys(st.Env) {
			out = append(out, struct{ name, value string }{prefix + " env." + k, st.Env[k]})
		}
		for _, k := range sortedKeys(st.With) {
			out = append(out, struct{ name, value string }{prefix + " with." + k, st.With[k]})
		}
	}
	return out
}

// sortedKeys is a stable key list for error messages.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expandReusable resolves one `uses:` caller into its callee jobs (recursively).
// depth is the reusable nesting level (the caller itself is 1); repo identifies
// the repository the callee path is relative to (a remote callee's own local
// calls resolve inside the callee's repository); chain is the open
// (repository, path) list for cycle detection. rootCaller/rootOutputs are the
// author-visible call and its declared outputs, propagated unchanged to every
// job of the call; parentSecrets is the closed set the calling workflow itself
// received, so a nested call can only pass on what it holds.
func (s *Service) expandReusable(ctx context.Context, repo, commit string, wf *WorkflowFile, callerKey, callerName string, caller workflow.Job, inputs map[string]string, pc planContext, depth int, chain []string, rootCaller string, rootOutputs map[string]string, parentSecrets *reusableSecrets) ([]reusableJob, map[string]string, error) {
	refusal := func(reason string) ([]reusableJob, map[string]string, error) {
		return []reusableJob{{
			key: callerKey, name: callerName, wf: wf.Workflow, wfName: wf.Name,
			def: caller, inputs: inputs, needs: nonNilStr([]string(caller.Needs)), refusal: reason,
		}}, nil, nil
	}
	// A caller job is a call, not a run: every key that would describe a runner
	// job is refused BY NAME. Silently ignoring one would be the
	// accepted-and-inert failure both source cards forbid.
	if bad := reusableCallerRefusal(caller); bad != "" {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s", callerKey, caller.Uses, bad))
	}
	if depth > MaxReusableDepth {
		return refusal(fmt.Sprintf("job %s (uses: %s): reusable workflows are nested deeper than the limit of %d", callerKey, caller.Uses, MaxReusableDepth))
	}
	ref, err := parseReusableUses(caller.Uses)
	if err != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %v", callerKey, caller.Uses, err))
	}
	calleeRepo, calleeCommit := repo, commit
	if ref.Remote {
		// A cross-repository callee must be readable without credentials and is
		// pinned to a commit: a mutable ref would change under the run, and
		// reading a private repository would need an authority the run does not
		// carry (D-6/D-5). Both are named refusals, never inert syntax.
		if s.roleOf(ctx, ref.Owner, ref.Repo, auth.Anonymous()) == "" {
			return refusal(fmt.Sprintf("job %s (uses: %s): repository %s/%s is not publicly readable on this instance; a cross-repository reusable workflow resolves only when the callee is readable without credentials (D-6)",
				callerKey, caller.Uses, ref.Owner, ref.Repo))
		}
		calleeRepo, calleeCommit = repoID(ref.Owner, ref.Repo), ref.Commit
	}
	key := calleeRepo + "\x00" + ref.Path
	for _, open := range chain {
		if open == key {
			return refusal(fmt.Sprintf("job %s (uses: %s): reusable workflow cycle: %s", callerKey, caller.Uses,
				strings.Join(append(append([]string{}, chain...), key), " → ")))
		}
	}
	if s.Git == nil {
		return nil, nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	raw, rerr := s.Git.ReadFile(ctx, calleeRepo, calleeCommit, ref.Path)
	if rerr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): could not read %s at %s in %s: %v",
			callerKey, caller.Uses, ref.Path, shortSHA(calleeCommit), calleeRepo, rerr))
	}
	callee, perr := workflow.Parse(raw)
	if perr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s: %v", callerKey, caller.Uses, ref.Path, perr))
	}
	if callee.On.WorkflowCall == nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s is not a reusable workflow (it declares no `on.workflow_call`)",
			callerKey, caller.Uses, ref.Path))
	}
	if len(callee.Jobs) == 0 {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s declares no jobs", callerKey, caller.Uses, ref.Path))
	}
	// The caller's `with:` is expanded once, here, from the run's github.* and
	// the caller's own inputs.*; the callee's declarations then decide validity.
	supplied, rerr := resolveCallInputs(caller.With, pc.github, inputs)
	if rerr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %v", callerKey, caller.Uses, rerr))
	}
	callInputs, rerr := workflow.NormalizeCallInputs(callee.On.WorkflowCall.Inputs, supplied)
	if rerr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s: %v", callerKey, caller.Uses, ref.Path, rerr))
	}
	secretSpec, rerr := resolveCallSecrets(caller, callee, calleeRepo != pc.runRepo, parentSecrets)
	if rerr != nil {
		return refusal(fmt.Sprintf("job %s (uses: %s): %s: %v", callerKey, caller.Uses, ref.Path, rerr))
	}
	cap := s.permissionCap(wf.Workflow, caller)
	calleeName := callee.Name
	if strings.TrimSpace(calleeName) == "" {
		calleeName = strings.TrimSuffix(pathpkg.Base(ref.Path), pathpkg.Ext(ref.Path))
	}
	// The author-visible outputs belong to the OUTERMOST call: a nested call's
	// interface is not addressable from the caller file.
	outputs := rootOutputs
	if depth == 1 {
		outputs = callOutputTemplates(callee)
	}
	call := &callSpec{Caller: rootCaller, Path: ref.Path, Outputs: outputs}
	if ref.Remote {
		s.auditAdmin(ctx, pc.actor, AuditReusableCrossRepo, calleeRepo+"@"+shortSHA(calleeCommit),
			fmt.Sprintf("caller=%s workflow=%s job=%s run=%s secrets=%s",
				pc.runRepo, ref.Path, callerKey, pc.runID, secretNamesForAudit(secretSpec)))
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
			nested, _, nerr := s.expandReusable(ctx, calleeRepo, calleeCommit,
				&WorkflowFile{Path: ref.Path, Name: calleeName, Workflow: callee},
				expandedKey, expandedName, def, nil, pc, depth+1, append(chain, key), rootCaller, outputs, secretSpec)
			if nerr != nil {
				return nil, nil, nerr
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
			def: jobDef, inputs: callInputs, needs: needs, cap: cap,
			call: call, secrets: secretSpec,
		})
	}
	return out, callOutputTemplates(callee), nil
}

// jobDisplayName is the display name of one job definition.
func jobDisplayName(key string, def workflow.Job) string {
	if name := strings.TrimSpace(def.Name); name != "" {
		return name
	}
	return key
}

// callOutputTemplates is the callee's declared output interface
// (`name → ${{ jobs.<id>.outputs.<x> }}`), or nil when it declares none.
func callOutputTemplates(callee *workflow.Workflow) map[string]string {
	if callee == nil || callee.On.WorkflowCall == nil || len(callee.On.WorkflowCall.Outputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(callee.On.WorkflowCall.Outputs))
	for name, decl := range callee.On.WorkflowCall.Outputs {
		out[name] = decl.Value
	}
	return out
}

// resolveCallInputs expands one `with:` map with the run's github.* values and
// the caller's own inputs.*. The parser guarantees each value is a literal or a
// reference from those two contexts; an absent value expands empty (GitHub's
// rule), and the callee's type check decides whether that is acceptable.
func resolveCallInputs(with map[string]string, github, inputs map[string]string) (map[string]string, error) {
	if len(with) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(with))
	for name, raw := range with {
		val, err := workflow.Substitute(raw, workflow.TemplateValues{GitHub: github, Inputs: inputs})
		if err != nil {
			return nil, fmt.Errorf("with.%s: %w", name, err)
		}
		out[name] = val
	}
	return out, nil
}

// resolveCallSecrets turns a caller's `secrets:` block into the closed set a
// callee receives, expressed in the RUN repository's secret names (what the
// claim decrypts). D-6: a cross-repository callee receives only explicitly
// listed secrets, and `inherit` never crosses a repository boundary (the parser
// refuses it too; this is the expansion's own backstop).
//
// The parent set is what the calling workflow itself received: a nested call
// inside a callee can only hand on a secret its own caller handed to it, and a
// name it remaps is resolved transitively to the run repository's name. Without
// that composition a remote callee could name any of the caller's secrets in a
// nested call and read them.
func resolveCallSecrets(caller workflow.Job, callee *workflow.Workflow, crossRepo bool, parent *reusableSecrets) (*reusableSecrets, error) {
	declared := map[string]bool{}
	required := map[string]bool{}
	if callee.On.WorkflowCall != nil {
		for name, sec := range callee.On.WorkflowCall.Secrets {
			declared[name] = true
			if sec.Required {
				required[name] = true
			}
		}
	}
	// inheritSet is "the calling workflow's whole secret set", propagated one
	// level down. parent == nil is the run itself (the repository's secrets).
	inheritSet := func() *reusableSecrets {
		if parent == nil || parent.Inherit {
			return &reusableSecrets{Inherit: true}
		}
		names := make(map[string]string, len(parent.Names))
		for name, source := range parent.Names {
			names[name] = source
		}
		return &reusableSecrets{Names: names}
	}
	if caller.Secrets == nil || caller.Secrets.Inherit {
		if crossRepo {
			// Nothing crosses a repository boundary that the caller did not
			// list. A declaration the caller cannot satisfy is named here.
			if len(required) > 0 {
				return nil, fmt.Errorf("the callee requires secret(s) %s, and `secrets: inherit` is refused for a cross-repository callee (D-6); list each secret explicitly", listOrNone(sortedKeys(required)))
			}
			return &reusableSecrets{}, nil
		}
		return inheritSet(), nil
	}
	names := make([]string, 0, len(caller.Secrets.Values))
	for name := range caller.Secrets.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		if !declared[name] {
			return nil, fmt.Errorf("`secrets.%s` is not declared by the callee's `on.workflow_call.secrets` (declared: %s)", name, listOrNone(sortedKeys(declared)))
		}
		source := caller.Secrets.Values[name]
		if parent == nil || parent.Inherit {
			out[name] = source
			continue
		}
		resolved, ok := parent.Names[source]
		if !ok {
			return nil, fmt.Errorf("`secrets.%s` reads `%s`, which the calling workflow did not receive (it received: %s); a callee can only pass on secrets its own caller handed to it", name, source, listOrNone(sortedKeys(parent.Names)))
		}
		out[name] = resolved
	}
	for name := range required {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("the callee requires the secret `%s`, which the call does not pass (passed: %s)", name, listOrNone(names))
		}
	}
	return &reusableSecrets{Names: out}, nil
}

// secretNamesForAudit lists a call's passed secret NAMES (never values) for the
// audit detail.
func secretNamesForAudit(spec *reusableSecrets) string {
	if spec == nil {
		return "none"
	}
	if spec.Inherit {
		return "inherit"
	}
	return strings.Join(sortedKeys(spec.Names), ",")
}

// reusableSecretValues applies a callee's closed secret set to the repository's
// secret values at claim time. `inherit` keeps the whole set; an explicit map
// renames the listed sources; anything else is empty (D-6). An ordinary job
// (spec.ReusableSecrets == nil) keeps the repository's secrets.
func reusableSecretValues(spec *Spec, repoValues map[string]string) map[string]string {
	if spec == nil || spec.ReusableSecrets == nil {
		return repoValues
	}
	out := make(map[string]string, len(repoValues))
	if spec.ReusableSecrets.Inherit {
		for name, value := range repoValues {
			out[name] = value
		}
		return out
	}
	for name, source := range spec.ReusableSecrets.Names {
		out[name] = lookupSecretValue(repoValues, source)
	}
	return out
}

// lookupSecretValue resolves one repository secret name (case-insensitively,
// like `${{ secrets.NAME }}` itself). A missing secret is the empty string.
func lookupSecretValue(values map[string]string, name string) string {
	if val, ok := values[name]; ok {
		return val
	}
	for key, val := range values {
		if strings.EqualFold(key, name) {
			return val
		}
	}
	return ""
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
	case caller.TimeoutMinutes.Set():
		return "a reusable-workflow caller job may not declare `timeout-minutes`; set it on the callee's jobs"
	case caller.Concurrency != nil:
		return "a reusable-workflow caller job may not declare `concurrency`; set it on the callee's jobs"
	case caller.Environment != nil:
		return "a reusable-workflow caller job may not declare `environment`; set it on the callee's jobs"
	case len(caller.Outputs) > 0:
		return "a reusable-workflow caller job may not declare `outputs`: the callee's `on.workflow_call.outputs` ARE the caller job's outputs, read as `needs.<caller job>.outputs.<name>`"
	}
	return ""
}

// reusableRef is one parsed `jobs.<id>.uses` reference.
type reusableRef struct {
	// Remote is true for `owner/repo/path@sha`; false for a `./…` path in the
	// repository the caller is being expanded inside.
	Remote bool
	Owner  string
	Repo   string
	// Path is the repository-relative workflow file.
	Path string
	// Commit is the pinned 40-hex commit (remote references only).
	Commit string
}

// parseReusableUses resolves a `uses:` reference to a local path or a
// cross-repository (owner, repo, path, commit). Anything else — a URL, a
// docker:// image, a mutable ref, a foreign workflow directory — is refused by
// name (the product rule: only .spindle/workflows is readable, and only a
// pinned commit is stable).
func parseReusableUses(uses string) (reusableRef, error) {
	raw := strings.TrimSpace(uses)
	if raw == "" {
		return reusableRef{}, fmt.Errorf("uses is empty")
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "docker://") || strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return reusableRef{}, fmt.Errorf("only a workflow file is callable: use `./.spindle/workflows/<name>.yml` (same repository) or `owner/repo/.spindle/workflows/<name>.yml@<40-hex commit>`; %q is not a workflow file", raw)
	}
	if strings.HasPrefix(raw, "./") {
		path, err := localReusablePath(raw)
		if err != nil {
			return reusableRef{}, err
		}
		return reusableRef{Path: path}, nil
	}
	at := strings.LastIndexByte(raw, '@')
	if at <= 0 || at == len(raw)-1 {
		return reusableRef{}, fmt.Errorf("`uses` is either a path inside this repository (`./.spindle/workflows/<name>.yml`) or a pinned cross-repository reference (`owner/repo/.spindle/workflows/<name>.yml@<40-hex commit>`); %q is neither", raw)
	}
	before, commit := raw[:at], raw[at+1:]
	if !isCommitSHA(commit) {
		return reusableRef{}, fmt.Errorf("a cross-repository call must be pinned to a full 40-character commit SHA, not %q (a branch or tag changes under the run)", commit)
	}
	parts := strings.SplitN(before, "/", 3)
	if len(parts) != 3 {
		return reusableRef{}, fmt.Errorf("a cross-repository call is `owner/repo/.spindle/workflows/<name>.yml@<sha>` (got %q)", before)
	}
	owner, repo, path := parts[0], parts[1], parts[2]
	if !validRepoSegment(owner) || !validRepoSegment(repo) {
		return reusableRef{}, fmt.Errorf("%q is not an owner/repository name", owner+"/"+repo)
	}
	clean := pathpkg.Clean(path)
	if clean != path || pathpkg.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return reusableRef{}, fmt.Errorf("%q is not a path inside the callee repository", path)
	}
	if pathpkg.Dir(clean) != workflow.Dir {
		return reusableRef{}, fmt.Errorf("%q is not in %s/ (the only directory this engine reads; a foreign workflow directory is never read, not even when called)", path, workflow.Dir)
	}
	switch pathpkg.Ext(clean) {
	case ".yml", ".yaml":
	default:
		return reusableRef{}, fmt.Errorf("%q must name a .yml or .yaml file", path)
	}
	return reusableRef{Remote: true, Owner: owner, Repo: repo, Path: clean, Commit: strings.ToLower(commit)}, nil
}

// localReusablePath resolves a `./` reference to a repository path. Only
// `.spindle/workflows/<file>.yml|.yaml` is accepted: that is the directory the
// engine discovers, so a callee is a file the run could have been created from.
func localReusablePath(uses string) (string, error) {
	raw := strings.TrimSpace(uses)
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

// validRepoSegment reports whether s can be one owner/repository path segment.
func validRepoSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// isCommitSHA reports whether s is a full lowercase-or-uppercase 40-hex SHA.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// shortSHA abbreviates a commit for display/audit (never shortens a ref used to
// read content).
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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

// callOutputs evaluates one caller job's declared outputs from the callee jobs'
// reported outputs. It returns ok=false when the dependency set is not a single
// reusable call (an ordinary job key), so the needs fold keeps its normal
// union. It is called only for a dependency that wholly succeeded.
func callOutputs(key string, ids []string, byID map[string]*Job) (map[string]string, bool) {
	var spec *callSpec
	for _, id := range ids {
		dep := byID[id]
		if dep == nil || dep.Spec == nil || dep.Spec.Call == nil || dep.Spec.Call.Caller != key {
			return nil, false
		}
		spec = dep.Spec.Call
	}
	if spec == nil {
		return nil, false
	}
	jobs := make(map[string]map[string]string, len(ids))
	for _, id := range ids {
		dep := byID[id]
		jobs[strings.TrimPrefix(dep.Key, key+prefixSeparator)] = dep.Outputs
	}
	out := make(map[string]string, len(spec.Outputs))
	for name, tmpl := range spec.Outputs {
		val, err := workflow.SubstituteCallOutputs(tmpl, jobs)
		if err != nil {
			// A declared output whose template cannot resolve (a job the call
			// did not run) publishes nothing; the name is still absent, and a
			// reader gets the strict "did not publish" error.
			continue
		}
		out[name] = val
	}
	return out, true
}
