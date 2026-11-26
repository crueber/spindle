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
// `server_url`/`api_url` are the spindle server addresses (PAR-111,
// docs/threads.md §3.5): the runner substitutes them from its environment. They
// are accepted here so a validated workflow can read them; a job-level `if:`
// does not resolve them yet (the server's context predates the fields).
var GitHubFields = []string{
	"actor", "api_url", "event_name", "job", "ref", "ref_name", "repository",
	"run_id", "server_url", "sha", "workflow",
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
	"github.event.inputs.<name>",
	"forgejo.<field>", "forge.<field> (the github.* set)",
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
		if (context == "steps" || context == "needs") && templateIdent(owner) && templateIdent(output) {
			ref := &TemplateRef{Context: context, Key: owner + ".outputs." + output, Output: output}
			if context == "steps" {
				ref.Step = owner
			} else {
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
	// Inputs are the workflow's declared workflow_dispatch input names.
	Inputs []string
	// Steps are the lowercased ids of the steps declared before the site
	// (`if:` only; an output reference is validated separately by step order).
	Steps []string
	// Needs are the job's `needs:` keys.
	Needs []string
	// JobIf marks a job-level `if:`: secrets are not available there yet
	// (a step `if:` and step substitutions do read them).
	JobIf bool
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
	switch ref.Context {
	case "github", "forgejo", "forge":
		if name, ok := eventInputName(ref.Key); ok {
			if !containsExact(scope.Inputs, name) {
				return fmt.Errorf("${{ %s.%s }} is not a declared workflow_dispatch input (declared: %s)",
					ref.Context, ref.Key, listOrNone(scope.Inputs))
			}
			return nil
		}
		if !ValidGitHubField(ref.Key) {
			return fmt.Errorf("${{ %s.%s }} is not a supported context field (supported: %s)",
				ref.Context, ref.Key, strings.Join(GitHubFields, ", "))
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
	case "matrix":
		return vals.Matrix[ref.Key], true
	case "inputs":
		return vals.Inputs[ref.Key], true
	case "env":
		return vals.Env[ref.Key], true
	}
	return "", false
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
