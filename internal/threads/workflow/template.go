package workflow

import (
	"fmt"
	"strings"
)

// `${{ … }}` templates and their substitution (docs/WORKFLOWS.md §6.2).
//
// Three uses share one scanner:
//
//   - a concurrency `group` is a bucket key and the lock that decides which run
//     may execute. Its vocabulary is validated at parse time (ValidateTemplate
//     through the group wrapper, ValidateGroupTemplate) and expanded strictly
//     at run creation (ExpandGroup: an absent value is an error, never an empty
//     key);
//   - every other substituted site (step name, run, shell, working-directory,
//     env/with values, a job name and job outputs) is validated at parse time
//     against the same closed vocabulary, then expanded at run time by the
//     server or runner (Substitute);
//   - a job `name` is display-only, so FillTemplate fills what it can and
//     leaves the rest visible.
//
// The rule everywhere is the parity rule: a reference the engine does not
// support is THAT FILE'S PARSE ERROR naming the supported set, never a literal
// that silently reaches the shell or a lock key.

// GitHubFields is the closed `github.*` (and `forgejo.*`/`forge.*`) field set a
// template or an `if:` expression may read. It is the Actions-compatible field
// set the parity corpus uses; the remaining documented fields (actor_id,
// triggering_actor, ref_type, head_ref, workflow_ref, …) are a later wave and
// are named parse errors today. `github.event.inputs.<name>` is accepted
// separately (see splitRef).
//
// `action_path` is in the set because a composite action's own steps read it
// (PAR-074), but it is NOT a global value: TemplateScope.ActionPath gates it, so
// a workflow step, a job `if:` or an output template that reads it is a named
// parse error rather than the empty string. The runner supplies the per-composite
// value (docs/WORKFLOWS.md §4.3, docs/threads.md §3.4a).
//
// `server_url`/`api_url` are the spindle server addresses (PAR-111,
// docs/threads.md §3.5): the runner substitutes them from its environment. They
// are accepted here so a validated workflow can read them; a job-level `if:`
// does not resolve them yet (the server's context predates the fields).
var GitHubFields = []string{
	"action_path", "actor", "api_url", "event_name", "job", "ref", "ref_name", "repository",
	"run_id", "server_url", "sha", "workflow",
}

// EventFields is the closed `github.event.*` (and forgejo/forge alias) field
// set Threads documents (PAR-033). The event JSON written to
// $GITHUB_EVENT_PATH carries the complete payload; the expression context is
// this bounded, documented projection — an arbitrary path would be a silent
// empty value, which is the acceptance failure the closed vocabulary exists to
// prevent. `github.event.inputs.<name>` is handled separately.
var EventFields = []string{
	"action",
	"after", "before", "forced", "ref",
	"repository.name", "repository.full_name", "repository.owner.login",
	"repository.default_branch", "repository.private",
	"sender.login",
	"pull_request.number", "pull_request.state", "pull_request.title",
	"pull_request.draft", "pull_request.merged",
	"pull_request.user.login",
	"pull_request.head.ref", "pull_request.head.sha", "pull_request.head.repo.full_name",
	"pull_request.base.ref", "pull_request.base.sha", "pull_request.base.repo.full_name",
	"release.tag_name", "release.name", "release.draft", "release.prerelease",
	"release.target_commitish", "release.author.login", "release.assets",
	"issue.number", "issue.title", "issue.state", "issue.user.login",
	"issue.labels",
	"comment.id", "comment.body", "comment.user.login",
	"comment.author_association",
	"workflow_run.id", "workflow_run.name", "workflow_run.event",
	"workflow_run.status", "workflow_run.conclusion", "workflow_run.run_attempt",
	"workflow_run.head_branch", "workflow_run.head_sha", "workflow_run.workflow_id",
	"deployment.id", "deployment.environment", "deployment.ref", "deployment.sha",
	"deployment.task", "deployment.creator.login",
	"deployment_status.state", "deployment_status.description",
	"deployment_status.creator.login",
}

// RunnerFields is the closed `runner.*` context (PAR-034).
var RunnerFields = []string{"os", "arch", "name", "temp", "tool_cache", "environment", "debug"}

// ValidRunnerField reports whether field is in the closed runner.* set.
func ValidRunnerField(field string) bool { return containsFold(RunnerFields, field) }

// clientPayloadPrefix is the one wildcard branch of the closed event-field
// vocabulary: `github.event.client_payload.<key>` names a repository_dispatch
// value, whose keys are the dispatcher's (bounded by validateClientPayload).
const clientPayloadPrefix = "client_payload."

// ValidEventField reports whether path (without the `event.` prefix) is in the
// closed github.event.* set. A `client_payload.<key>` path is valid when the key
// is an identifier; it resolves only when the current payload carries it (an
// absent client key is empty, exactly like an absent input).
func ValidEventField(path string) bool {
	path = strings.TrimSpace(path)
	if rest, ok := strings.CutPrefix(strings.ToLower(path), clientPayloadPrefix); ok {
		return templateIdent(rest) && len(rest) <= 64
	}
	return containsFold(EventFields, path)
}

// GroupGitHubFields is the subset a concurrency group may read: the group is
// expanded before a job exists, so `github.job` has no value there.
var GroupGitHubFields = []string{
	"actor", "event_name", "ref", "ref_name", "repository", "run_id", "sha", "workflow",
}

// TemplateSupported is the closed reference set, for error messages.
var TemplateSupported = []string{
	"github.ref", "github.ref_name", "github.repository", "github.workflow",
	"github.event_name", "github.sha", "github.run_id", "github.job",
	"github.actor", "github.server_url", "github.api_url",
	"github.action_path (a composite action's own steps only)",
	"github.event.inputs.<name>", "github.event.<field>",
	"forgejo.<field>", "forge.<field> (the github.* set)",
	"runner.<field>", "vars.<name>",
	"matrix.<axis>", "inputs.<name>", "env.<NAME>", "secrets.<NAME>",
	"steps.<id>.outputs.<name>", "needs.<job>.outputs.<name>",
}

// ValidGitHubField reports whether field is in the closed github.* set
// (case-insensitively; field names are lower-case in the vocabulary).
func ValidGitHubField(field string) bool {
	return containsFold(GitHubFields, field)
}

// ValidGitHubGroupField reports whether field may be read in a concurrency
// group.
func ValidGitHubGroupField(field string) bool {
	return containsFold(GroupGitHubFields, field)
}

// ValidSecretName reports whether name is a repository secret name
// (`[A-Za-z_][A-Za-z0-9_]{0,63}`, docs/threads.md §5.1).
func ValidSecretName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// TemplateRef is one `${{ context.key }}` reference.
type TemplateRef struct {
	// Raw is the reference as written (the whole `${{ … }}` span).
	Raw string
	// Context is the lowercased context name ("github", "forgejo", "matrix", …).
	Context string
	// Key is the reference key, case preserved (input, axis and secret names
	// are authored names; a lookup decides its own case rule).
	Key string
	// Step/Job and Output are set for the four-segment
	// `steps.<id>.outputs.<name>` and `needs.<job>.outputs.<name>` forms
	// (Context == "steps"/"needs"); Key is then "<id>.outputs.<name>".
	Step   string
	Job    string
	Output string
}

// templatePart is one literal run or one reference of a template string.
type templatePart struct {
	literal string
	ref     *TemplateRef
}

// splitTemplate splits s into literal runs and references. A `${{` with no
// closing `}}`, or a span that is not a `context.key` reference (an operator, a
// function call), is an error: the caller decides whether that fails the file
// (a group or a substituted site) or leaves the text visible (a job name).
func splitTemplate(s string) ([]templatePart, error) {
	var parts []templatePart
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			if s != "" {
				parts = append(parts, templatePart{literal: s})
			}
			return parts, nil
		}
		if i > 0 {
			parts = append(parts, templatePart{literal: s[:i]})
		}
		rest := s[i+3:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated ${{ expression")
		}
		expr := strings.TrimSpace(rest[:j])
		ref, ok := splitRef(expr)
		if !ok {
			return nil, fmt.Errorf("${{ %s }} is not a supported reference (supported: %s)",
				expr, strings.Join(TemplateSupported, ", "))
		}
		ref.Raw = "${{ " + expr + " }}"
		parts = append(parts, templatePart{ref: ref})
		s = rest[j+2:]
	}
}

// splitRef splits `context.key` — the four-segment
// `steps.<id>.outputs.<name>` / `needs.<job>.outputs.<name>` forms, or
// `github.event.inputs.<name>` (with the forgejo/forge aliases) — and checks
// every segment is an identifier.
func splitRef(expr string) (*TemplateRef, bool) {
	segments := strings.Split(expr, ".")
	if len(segments) == 4 && strings.EqualFold(strings.TrimSpace(segments[2]), "outputs") {
		context := strings.ToLower(strings.TrimSpace(segments[0]))
		owner := strings.TrimSpace(segments[1])
		output := strings.TrimSpace(segments[3])
		if (context == "steps" || context == "needs" || context == "jobs") && templateIdent(owner) && templateIdent(output) {
			ref := &TemplateRef{Context: context, Key: owner + ".outputs." + output, Output: output}
			if context == "steps" {
				ref.Step = owner
			} else {
				// needs.<job> and jobs.<id> both name a job key.
				ref.Job = owner
			}
			return ref, true
		}
	}
	if len(segments) == 4 && isGitHubContext(strings.TrimSpace(segments[0])) &&
		strings.EqualFold(strings.TrimSpace(segments[1]), "event") &&
		strings.EqualFold(strings.TrimSpace(segments[2]), "inputs") {
		context := strings.ToLower(strings.TrimSpace(segments[0]))
		name := strings.TrimSpace(segments[3])
		if templateIdent(name) {
			return &TemplateRef{Context: context, Key: "event.inputs." + name}, true
		}
	}
	// `github.event.<path>` is a documented-event-field reference; the path is
	// validated against EventFields by validateRef (an arbitrary path is the
	// parse error, never an empty value).
	if len(segments) >= 3 && isGitHubContext(strings.TrimSpace(segments[0])) &&
		strings.EqualFold(strings.TrimSpace(segments[1]), "event") {
		path := make([]string, 0, len(segments)-2)
		for _, seg := range segments[2:] {
			if !templateIdent(strings.TrimSpace(seg)) {
				return nil, false
			}
			path = append(path, strings.TrimSpace(seg))
		}
		if len(path) == 1 && strings.EqualFold(path[0], "inputs") {
			return nil, false // the bare `event.inputs` is not a field
		}
		context := strings.ToLower(strings.TrimSpace(segments[0]))
		return &TemplateRef{Context: context, Key: "event." + strings.Join(path, ".")}, true
	}
	dot := strings.IndexByte(expr, '.')
	if dot <= 0 || dot == len(expr)-1 {
		return nil, false
	}
	context := strings.ToLower(strings.TrimSpace(expr[:dot]))
	key := strings.TrimSpace(expr[dot+1:])
	if !templateIdent(context) || !templateIdent(key) {
		return nil, false
	}
	return &TemplateRef{Context: context, Key: key}, true
}

// isGitHubContext reports whether context is the canonical github context or
// one of its compatibility aliases (they name the same values).
func isGitHubContext(context string) bool {
	switch strings.ToLower(strings.TrimSpace(context)) {
	case "github", "forgejo", "forge":
		return true
	}
	return false
}

// templateIdent is the identifier charset of a reference segment.
func templateIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '-':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// TemplateRefs returns every reference in s (validation and tests read this).
func TemplateRefs(s string) ([]TemplateRef, error) {
	parts, err := splitTemplate(s)
	if err != nil {
		return nil, err
	}
	refs := make([]TemplateRef, 0, len(parts))
	for _, p := range parts {
		if p.ref != nil {
			refs = append(refs, *p.ref)
		}
	}
	return refs, nil
}

// TemplateScope is what one substituted site can resolve, used by the
// parse-time validators.
type TemplateScope struct {
	// Axes are the job's matrix keys (axes plus include keys).
	Axes []string
	// Inputs are the workflow's declared workflow_dispatch and workflow_call
	// input names.
	Inputs []string
	// Steps are the lowercased ids of the steps declared before the site
	// (`if:` only; an output reference is validated separately by step order).
	Steps []string
	// Needs are the job's `needs:` keys.
	Needs []string
	// JobIf marks a job-level `if:`: secrets are not available there yet
	// (a step `if:` and step substitutions do read them).
	JobIf bool
	// ActionPath permits `${{ github.action_path }}`: only a composite
	// action's own steps have a per-action value for it (PAR-074).
	ActionPath bool
	// CallWith marks a reusable-caller `with:` value. It is expanded at run
	// creation, so only `github.*` and `inputs.*` resolve there — no secrets,
	// steps, needs, matrix, runner, vars or env.
	CallWith bool
	// CallOutput marks an `on.workflow_call.outputs.<name>.value` template:
	// only `jobs.<id>.outputs.<name>` resolves there.
	CallOutput bool
	// Jobs maps a job key to its declared output names. It is set only for an
	// output value (CallOutput) and is what closes a `jobs.` reference.
	Jobs map[string][]string
	// SecretNames closes `secrets.*` to a workflow_call callee's declarations;
	// SecretsClosed is false for an ordinary workflow (the repository's secrets
	// are addressable by name).
	SecretNames   []string
	SecretsClosed bool
}

// ValidateTemplate checks every `${{ … }}` span in a substituted site against
// the closed vocabulary. A span must be one reference — an expression with
// operators or a function call is only supported in `if:` (ValidateIf).
func ValidateTemplate(s string, scope TemplateScope) error {
	if !strings.Contains(s, "${{") {
		return nil
	}
	parts, err := splitTemplate(s)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if p.ref == nil {
			continue
		}
		if err := validateRef(*p.ref, scope); err != nil {
			return err
		}
	}
	return nil
}

// validateRef checks one reference against a scope.
func validateRef(ref TemplateRef, scope TemplateScope) error {
	if scope.CallOutput && ref.Context != "jobs" {
		return fmt.Errorf("%s is not available in a workflow_call output value (only jobs.<id>.outputs.<name> resolves there)",
			ref.Raw)
	}
	if scope.CallWith {
		switch ref.Context {
		case "github", "forgejo", "forge", "inputs":
		default:
			return fmt.Errorf("%s is not available in a reusable-workflow `with:` value (supported: github.*, inputs.*; the value is expanded when the run is created, before any step, matrix, secret or variable exists)", ref.Raw)
		}
	}
	switch ref.Context {
	case "github", "forgejo", "forge":
		if strings.EqualFold(ref.Key, "action_path") {
			if !scope.ActionPath {
				return fmt.Errorf("${{ %s.action_path }} is only available in a composite action's own steps (it is the action's directory; a workflow step has none)",
					ref.Context)
			}
			return nil
		}
		if name, ok := eventInputName(ref.Key); ok {
			if !containsExact(scope.Inputs, name) {
				return fmt.Errorf("${{ %s.%s }} is not a declared workflow_dispatch input (declared: %s)",
					ref.Context, ref.Key, listOrNone(scope.Inputs))
			}
			return nil
		}
		if path, ok := eventFieldName(ref.Key); ok {
			if !ValidEventField(path) {
				return fmt.Errorf("${{ %s.%s }} is not a supported event field (supported: %s)",
					ref.Context, ref.Key, strings.Join(EventFields, ", "))
			}
			return nil
		}
		if !ValidGitHubField(ref.Key) {
			return fmt.Errorf("${{ %s.%s }} is not a supported context field (supported: %s)",
				ref.Context, ref.Key, strings.Join(GitHubFields, ", "))
		}
		return nil
	case "runner":
		if scope.JobIf {
			return fmt.Errorf("${{ runner.%s }} is not available in a job-level if: (the runner is chosen after the job is eligible; use a step if:)", ref.Key)
		}
		if !ValidRunnerField(ref.Key) {
			return fmt.Errorf("${{ runner.%s }} is not a supported runner field (supported: %s)",
				ref.Key, strings.Join(RunnerFields, ", "))
		}
		return nil
	case "vars":
		if scope.JobIf {
			return fmt.Errorf("${{ vars.%s }} is not available in a job-level if: (variables are resolved when the job is claimed; use a step if:)", ref.Key)
		}
		if !templateIdent(ref.Key) {
			return fmt.Errorf("${{ vars.%s }} is not a valid variable name", ref.Key)
		}
		return nil
	case "matrix":
		if !containsExact(scope.Axes, ref.Key) {
			return fmt.Errorf("${{ matrix.%s }} is not a declared matrix key (declared: %s)",
				ref.Key, listOrNone(scope.Axes))
		}
		return nil
	case "inputs":
		if !containsExact(scope.Inputs, ref.Key) {
			return fmt.Errorf("${{ inputs.%s }} is not a declared workflow_dispatch input (declared: %s)",
				ref.Key, listOrNone(scope.Inputs))
		}
		return nil
	case "env":
		return nil // env names are authored at run time; a missing one reads empty
	case "secrets":
		if scope.JobIf {
			return fmt.Errorf("${{ secrets.%s }} is not available in a job-level if: (use a step if:, or a step env value)", ref.Key)
		}
		if !ValidSecretName(ref.Key) {
			return fmt.Errorf("${{ secrets.%s }} is not a valid secret reference (the form is secrets.<name>; names match [A-Za-z_][A-Za-z0-9_]*)", ref.Key)
		}
		if scope.SecretsClosed && !containsExact(scope.SecretNames, ref.Key) {
			return fmt.Errorf("${{ secrets.%s }} is not declared by this workflow's `on.workflow_call.secrets` (declared: %s); a called workflow receives only the secrets its interface declares",
				ref.Key, listOrNone(scope.SecretNames))
		}
		return nil
	case "steps":
		if !containsExact(scope.Steps, strings.ToLower(ref.Step)) {
			return fmt.Errorf("${{ %s }} names a step that is not declared before this one (declared: %s)",
				ref.Raw, listOrNone(scope.Steps))
		}
		return nil
	case "needs":
		if !containsExactFold([]string(scope.Needs), ref.Job) {
			return fmt.Errorf("${{ %s }} names no `needs:` dependency of this job (declared: %s)",
				ref.Raw, listOrNone(scope.Needs))
		}
		return nil
	case "jobs":
		if !scope.CallOutput {
			return fmt.Errorf("%s is only available in an `on.workflow_call.outputs.<name>.value` template (a job output is read elsewhere as needs.<job>.outputs.<name>)", ref.Raw)
		}
		outputs, ok := scope.Jobs[ref.Job]
		if !ok {
			return fmt.Errorf("${{ %s }} names no job of this file", ref.Raw)
		}
		if !containsExact(outputs, ref.Output) {
			return fmt.Errorf("${{ %s }}: job %s does not declare the output %q (declared: %s)",
				ref.Raw, ref.Job, ref.Output, listOrNone(outputs))
		}
		return nil
	}
	return fmt.Errorf("unknown context %q in ${{ %s }} (supported: %s)",
		ref.Context, ref.Raw, strings.Join(TemplateSupported, ", "))
}

// eventInputName reports whether a github-context key is the
// `event.inputs.<name>` form, returning the input name.
func eventInputName(key string) (string, bool) {
	const prefix = "event.inputs."
	if len(key) <= len(prefix) || !strings.EqualFold(key[:len(prefix)], prefix) {
		return "", false
	}
	return key[len(prefix):], true
}

// eventFieldName reports whether a github-context key is an `event.<path>`
// reference (not the inputs alias), returning the path.
func eventFieldName(key string) (string, bool) {
	const prefix = "event."
	if len(key) <= len(prefix) || !strings.EqualFold(key[:len(prefix)], prefix) {
		return "", false
	}
	rest := key[len(prefix):]
	if strings.EqualFold(rest, "inputs") || strings.HasPrefix(strings.ToLower(rest), "inputs.") {
		return "", false
	}
	return rest, true
}

// ValidateGroupTemplate checks a concurrency group's references against what
// the position can resolve: the fixed `github.*` set, the declaring job's
// matrix keys (none at workflow level — a workflow-level group expands once per
// run, before any matrix), and the workflow's declared `workflow_dispatch`
// inputs. The error names the offending key and the supported set. An empty
// group or a group with no reference is unchanged and valid.
func ValidateGroupTemplate(group string, matrixAxes, inputNames []string, workflowLevel bool) error {
	if strings.TrimSpace(group) == "" {
		return nil
	}
	parts, err := splitTemplate(group)
	if err != nil {
		return fmt.Errorf("concurrency.group: %w", err)
	}
	for _, p := range parts {
		if p.ref == nil {
			continue
		}
		ref := p.ref
		switch ref.Context {
		case "github", "forgejo", "forge":
			if _, ok := eventInputName(ref.Key); ok {
				return fmt.Errorf("concurrency.group: ${{ %s.%s }} is not available; write ${{ inputs.<name> }} (supported: %s)",
					ref.Context, ref.Key, strings.Join(TemplateSupported, ", "))
			}
			if !ValidGitHubGroupField(ref.Key) {
				return fmt.Errorf("concurrency.group: ${{ %s.%s }} is not supported (supported: %s)",
					ref.Context, ref.Key, strings.Join(TemplateSupported, ", "))
			}
		case "matrix":
			if workflowLevel {
				return fmt.Errorf("concurrency.group: ${{ matrix.%s }} is not available in a workflow-level group (it expands once per run, before any matrix; move `concurrency` into the job, or drop the reference)",
					ref.Key)
			}
			if !containsExact(matrixAxes, ref.Key) {
				return fmt.Errorf("concurrency.group: ${{ matrix.%s }} is not a declared matrix axis of this job (declared: %s)",
					ref.Key, listOrNone(matrixAxes))
			}
		case "inputs":
			if !containsExact(inputNames, ref.Key) {
				return fmt.Errorf("concurrency.group: ${{ inputs.%s }} is not a declared workflow_dispatch input (declared: %s)",
					ref.Key, listOrNone(inputNames))
			}
		default:
			return fmt.Errorf("concurrency.group: ${{ %s.%s }} is not supported (supported: %s)",
				ref.Context, ref.Key, strings.Join(TemplateSupported, ", "))
		}
	}
	return nil
}

// ExpandGroup resolves every reference in a concurrency group. A context value
// that is absent (or empty) is an error naming the key and the supported set:
// a group is the lock, so it must never degrade to a literal or to an empty
// segment.
func ExpandGroup(group string, github, matrix, inputs map[string]string) (string, error) {
	if !strings.Contains(group, "${{") {
		return group, nil
	}
	parts, err := splitTemplate(group)
	if err != nil {
		return "", fmt.Errorf("concurrency.group: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		val, ok := lookupRef(*p.ref, github, matrix, inputs)
		if !ok {
			return "", fmt.Errorf("concurrency.group: ${{ %s.%s }} resolved to nothing for this run (supported: %s)",
				p.ref.Context, p.ref.Key, strings.Join(TemplateSupported, ", "))
		}
		b.WriteString(val)
	}
	return b.String(), nil
}

// FillTemplate replaces the references it can resolve and leaves the others
// exactly as written. A malformed template is returned unchanged. It is the
// lenient form for display names.
func FillTemplate(s string, github, matrix, inputs map[string]string) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	parts, err := splitTemplate(s)
	if err != nil {
		return s
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		if val, ok := lookupRef(*p.ref, github, matrix, inputs); ok {
			b.WriteString(val)
		} else {
			b.WriteString(p.ref.Raw)
		}
	}
	return b.String()
}

// FillContext replaces only `<context>.<key>` references, leaving every other
// reference and every unknown key as written. It is the lenient substitution
// the step fields use (matrix and inputs, resolved separately and in order).
func FillContext(s, context string, vals map[string]string) string {
	if !strings.Contains(s, "${{") || len(vals) == 0 {
		return s
	}
	parts, err := splitTemplate(s)
	if err != nil {
		return s
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		if p.ref.Context == context {
			if val, ok := vals[p.ref.Key]; ok {
				b.WriteString(val)
				continue
			}
		}
		b.WriteString(p.ref.Raw)
	}
	return b.String()
}

// TemplateValues is the complete runtime value set of one substitution site.
// The server and runner supply what the site may read; an absent supported
// value expands empty (GitHub's rule for an unset secret or output), except a
// steps/needs output reference, which is strict: an id or output that was never
// declared is an error, never an empty string (docs/threads.md §3.9).
type TemplateValues struct {
	GitHub  map[string]string
	Runner  map[string]string
	Vars    map[string]string
	Matrix  map[string]string
	Inputs  map[string]string
	Env     map[string]string
	Secrets map[string]string
	Steps   map[string]map[string]string
	Needs   map[string]map[string]string
}

// Substitute replaces every supported `${{ … }}` reference in s with its value
// in ONE pass: a substituted value is data and is never rescanned for another
// reference. It is what the runner calls for `run`, `with`, `env`, `name`,
// `shell`, `working-directory` and (through FillOutputs' callers) job outputs;
// the parse-time validators guarantee every span is a supported reference, so
// this returns an error only for an unterminated span, an unsupported context,
// or a `steps`/`needs` output the job never declared.
func Substitute(s string, vals TemplateValues) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	parts, err := splitTemplate(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		ref := *p.ref
		// github.action_path is a per-composite-step value the RUNNER supplies
		// (the parser accepts it only inside an action's steps). An absent value
		// is a named error, never the empty string: silently emptying it would
		// run an action from the wrong directory.
		if isGitHubContext(ref.Context) && strings.EqualFold(ref.Key, "action_path") {
			val, ok := vals.GitHub["action_path"]
			if !ok {
				return "", fmt.Errorf("%s has no value at this site: github.action_path is supplied by the runner for each composite action's own steps", ref.Raw)
			}
			b.WriteString(val)
			continue
		}
		switch ref.Context {
		case "steps", "needs":
			owner, values, label := ref.Step, vals.Steps, "step"
			if ref.Context == "needs" {
				owner, values, label = ref.Job, vals.Needs, "needs dependency"
			}
			outs, ok := values[strings.ToLower(owner)]
			if !ok {
				return "", fmt.Errorf("%s: no %s with id %q in this job", ref.Raw, label, owner)
			}
			val, ok := outs[ref.Output]
			if !ok {
				return "", fmt.Errorf("%s: %s %q did not publish the output %q", ref.Raw, label, owner, ref.Output)
			}
			b.WriteString(val)
		case "secrets":
			b.WriteString(lookupSecret(vals.Secrets, ref.Key))
		default:
			val, ok := lookupValue(ref, vals)
			if !ok {
				return "", fmt.Errorf("unknown context %q in ${{ %s }} (supported: %s)",
					ref.Context, ref.Raw, strings.Join(TemplateSupported, ", "))
			}
			b.WriteString(val)
		}
	}
	return b.String(), nil
}

// lookupValue resolves one non-secret, non-output reference.
func lookupValue(ref TemplateRef, vals TemplateValues) (string, bool) {
	if isGitHubContext(ref.Context) {
		if name, ok := eventInputName(ref.Key); ok {
			return vals.Inputs[name], true
		}
		return vals.GitHub[strings.ToLower(ref.Key)], true
	}
	switch ref.Context {
	case "runner":
		return vals.Runner[strings.ToLower(ref.Key)], true
	case "vars":
		return lookupFold(vals.Vars, ref.Key), true
	case "matrix":
		return vals.Matrix[ref.Key], true
	case "inputs":
		return vals.Inputs[ref.Key], true
	case "env":
		return vals.Env[ref.Key], true
	}
	return "", false
}

// lookupFold resolves an authored name case-insensitively (variables are
// referenced in mixed case and stored canonically).
func lookupFold(values map[string]string, name string) string {
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

// lookupSecret resolves a secret by name, case-insensitively (secret names are
// authored once and referenced from anywhere).
func lookupSecret(secrets map[string]string, name string) string {
	if val, ok := secrets[name]; ok {
		return val
	}
	for key, val := range secrets {
		if strings.EqualFold(key, name) {
			return val
		}
	}
	return ""
}

// StepOutputRefs returns the `steps.<id>.outputs.<name>` references in s and
// nothing else; every other `${{ … }}` span is ignored (a step field may mix,
// say, `${{ matrix.go }}` with a step output, and an unsupported span is left
// literal by design). An unterminated `${{` is an error.
func StepOutputRefs(s string) ([]TemplateRef, error) {
	return outputRefs(s, "steps")
}

// NeedsOutputRefs returns the `needs.<job>.outputs.<name>` references in s.
func NeedsOutputRefs(s string) ([]TemplateRef, error) {
	return outputRefs(s, "needs")
}

// OutputRefs returns the `steps.…` and `needs.…` output references in s, in
// order of appearance.
func OutputRefs(s string) ([]TemplateRef, error) {
	all, err := outputRefs(s, "")
	return all, err
}

// outputRefs scans s for four-segment output references (context "" accepts
// both steps and needs).
func outputRefs(s, context string) ([]TemplateRef, error) {
	var out []TemplateRef
	for rest := s; ; {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return out, nil
		}
		after := rest[i+3:]
		j := strings.Index(after, "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated ${{ expression")
		}
		expr := strings.TrimSpace(after[:j])
		if ref, ok := splitRef(expr); ok && (ref.Context == "steps" || ref.Context == "needs") &&
			(context == "" || ref.Context == context) {
			ref.Raw = "${{ " + expr + " }}"
			out = append(out, *ref)
		}
		rest = after[j+2:]
	}
}

// FillStepOutputs replaces every `steps.<id>.outputs.<name>` reference from
// values (a lowercased step id → its collected outputs) and leaves every other
// `${{ … }}` span exactly as written. It is strict about its own references: an
// unknown step id, or a step that did not publish the named output, is an error
// naming the reference. Step outputs cross a substitution site, so silently
// expanding one to an empty string would hide a typo or a step that never ran
// (the fail-closed rule, docs/threads.md §3.9).
func FillStepOutputs(s string, values map[string]map[string]string) (string, error) {
	return fillOutputs(s, "steps", values)
}

// FillNeedsOutputs replaces every `needs.<job>.outputs.<name>` reference from
// values (a job key → its reported outputs), with the same strictness.
func FillNeedsOutputs(s string, values map[string]map[string]string) (string, error) {
	return fillOutputs(s, "needs", values)
}

// FillOutputs replaces both steps and needs output references in ONE pass. It is
// what the runner and the job-output evaluator call: a step field may read both.
// One pass matters: a substituted VALUE is data and must never be rescanned for
// another reference.
func FillOutputs(s string, steps, needs map[string]map[string]string) (string, error) {
	return fillFrom(s, func(ref *TemplateRef) (map[string]map[string]string, string) {
		switch ref.Context {
		case "steps":
			return steps, "step"
		case "needs":
			return needs, "needs dependency"
		}
		return nil, ""
	})
}

// fillOutputs is the shared scanner for one context.
func fillOutputs(s, context string, values map[string]map[string]string) (string, error) {
	return fillFrom(s, func(ref *TemplateRef) (map[string]map[string]string, string) {
		if ref.Context != context {
			return nil, ""
		}
		if context == "steps" {
			return values, "step"
		}
		return values, "needs dependency"
	})
}

// fillFrom walks s, replacing every output reference pick claims (a nil map
// means "not mine": the span is written back exactly as authored).
func fillFrom(s string, pick func(*TemplateRef) (map[string]map[string]string, string)) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		after := s[i+3:]
		j := strings.Index(after, "}}")
		if j < 0 {
			return "", fmt.Errorf("unterminated ${{ expression")
		}
		expr := strings.TrimSpace(after[:j])
		raw := "${{ " + expr + " }}"
		ref, ok := splitRef(expr)
		if !ok {
			b.WriteString(raw)
			s = after[j+2:]
			continue
		}
		values, label := pick(ref)
		if values == nil {
			b.WriteString(raw)
			s = after[j+2:]
			continue
		}
		owner := ref.Step
		if ref.Context == "needs" {
			owner = ref.Job
		}
		outs, ok := values[strings.ToLower(owner)]
		if !ok {
			return "", fmt.Errorf("%s: no %s with id %q in this job", raw, label, owner)
		}
		val, ok := outs[ref.Output]
		if !ok {
			return "", fmt.Errorf("%s: %s %q did not publish the output %q", raw, label, owner, ref.Output)
		}
		b.WriteString(val)
		s = after[j+2:]
	}
}

// lookupRef resolves one reference against the contexts a group may name.
func lookupRef(ref TemplateRef, github, matrix, inputs map[string]string) (string, bool) {
	var val string
	var ok bool
	switch {
	case isGitHubContext(ref.Context):
		if name, isInput := eventInputName(ref.Key); isInput {
			val, ok = inputs[name]
			break
		}
		var present bool
		val, present = github[strings.ToLower(ref.Key)]
		ok = present
	case ref.Context == "matrix":
		val, ok = matrix[ref.Key]
	case ref.Context == "inputs":
		val, ok = inputs[ref.Key]
	}
	return val, ok && strings.TrimSpace(val) != ""
}

// containsExactFold is a case-insensitive membership test for `needs:` keys
// (job keys are matched case-insensitively by the expression evaluator).
func containsExactFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

// containsExact is an authored-name membership test: matrix axes and
// workflow_dispatch input names are case-sensitive (the same rule the step
// substitutions pin), while the fixed github.* field set is not.
func containsExact(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// containsFold is a case-insensitive membership test.
func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

// listOrNone renders a declared-name list for an error message.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// wholeSecretRef reports whether value is exactly one `${{ secrets.<name> }}`
// span and returns the secret name. A literal is not a secret reference: a
// `jobs.<id>.secrets` value must name where a secret comes FROM, or the file
// would carry the value itself (docs/threads.md §3.4b).
func wholeSecretRef(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "${{") || !strings.HasSuffix(trimmed, "}}") {
		return "", false
	}
	inner := strings.TrimSpace(trimmed[len("${{") : len(trimmed)-len("}}")])
	if inner == "" || strings.Contains(inner, "${{") || strings.Contains(inner, "}}") {
		return "", false
	}
	ref, ok := splitRef(inner)
	if !ok || ref.Context != "secrets" || !ValidSecretName(ref.Key) {
		return "", false
	}
	return ref.Key, true
}

// CallOutputRefs returns the `jobs.<id>.outputs.<name>` references in an
// `on.workflow_call.outputs.<name>.value` template. It is the only context that
// may appear there; a span naming anything else is left to ValidateTemplate's
// scope check, which refuses it by name.
func CallOutputRefs(s string) ([]TemplateRef, error) {
	var out []TemplateRef
	for rest := s; ; {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return out, nil
		}
		after := rest[i+3:]
		j := strings.Index(after, "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated ${{ expression")
		}
		expr := strings.TrimSpace(after[:j])
		if ref, ok := splitRef(expr); ok && ref.Context == "jobs" {
			ref.Raw = "${{ " + expr + " }}"
			out = append(out, *ref)
		}
		rest = after[j+2:]
	}
}

// SubstituteCallOutputs evaluates one `on.workflow_call.outputs.<name>.value`
// template against the callee jobs' reported outputs (a job key → its outputs,
// keyed the way the callee's file names the job). It is strict, like a step's
// output reference: a job the template names but the call did not run, or an
// output the job did not publish, is an error naming the reference. The server
// calls it when it folds a caller job's outputs for `needs.<caller>.outputs.*`.
func SubstituteCallOutputs(s string, jobs map[string]map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	parts, err := splitTemplate(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.ref == nil {
			b.WriteString(p.literal)
			continue
		}
		ref := *p.ref
		if ref.Context != "jobs" {
			return "", fmt.Errorf("unknown context %q in ${{ %s }} (a workflow_call output reads jobs.<id>.outputs.<name>)",
				ref.Context, ref.Raw)
		}
		outs, ok := jobs[ref.Job]
		if !ok {
			return "", fmt.Errorf("%s: no job with id %q in this call", ref.Raw, ref.Job)
		}
		val, ok := outs[ref.Output]
		if !ok {
			return "", fmt.Errorf("%s: job %q did not publish the output %q", ref.Raw, ref.Job, ref.Output)
		}
		b.WriteString(val)
	}
	return b.String(), nil
}

// NeedsOutputNames returns every `needs.<job>.outputs.<name>` reference in s
// (a template or an `if:` expression). The server uses it at run creation to
// check a caller job's output names against the callee's declaration; the
// parser's own validation only knows the caller file's job keys.
func NeedsOutputNames(s string) ([]TemplateRef, error) {
	var out []TemplateRef
	for i := 0; ; {
		j := strings.Index(s[i:], "needs.")
		if j < 0 {
			return out, nil
		}
		start := i + j
		// A reference must start at a boundary, not inside an identifier
		// (`myneeds.x` is not a reference).
		if start > 0 && isIdentByte(s[start-1]) {
			i = start + len("needs.")
			continue
		}
		rest := s[start:]
		end := 0
		for end < len(rest) && isRefByte(rest[end]) {
			end++
		}
		ref, ok := splitRef(rest[:end])
		if ok && ref.Context == "needs" {
			ref.Raw = rest[:end]
			out = append(out, *ref)
		}
		i = start + end
		if end == 0 {
			i++
		}
	}
}

// isIdentByte reports whether b can continue an identifier (or a `${{` span).
func isIdentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	}
	return false
}

// isRefByte reports whether b can continue a dotted reference.
func isRefByte(b byte) bool {
	return isIdentByte(b) || b == '.'
}
