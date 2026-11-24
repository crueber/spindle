package workflow

import (
	"fmt"
	"strings"
)

// `${{ … }}` templates outside `if:` (concurrency groups and job names).
//
// The vocabulary is CLOSED and small on purpose. A concurrency group is a
// bucket key and the lock that decides which run may execute; a reference that
// silently expands to nothing turns "cancel the run on this branch" into
// "cancel every run in the repository" (round-2 audit). So a group is validated
// at parse time (ValidateGroupTemplate: the file fails, naming the key and the
// supported set — never a literal fallback) and expanded strictly at run
// creation (ExpandGroup: a context value that is absent or empty is an error,
// not an empty key).
//
// A job `name` is display-only, so it is filled leniently (FillTemplate): a
// reference the parser cannot resolve stays visible in the rendered name
// instead of failing a workflow that would otherwise run.
//
// The scanner here is also the single expander the step substitutions use
// (FillContext), so `${{ matrix.go }}` means the same thing at every site.

// TemplateGitHubFields is the closed `github.*` field set a template may read.
// The values are the same keys the expression evaluator exposes
// (internal/threads/expr.go) minus `github.job`, whose value is not known
// before the matrix expansion that consumes the template.
var TemplateGitHubFields = []string{
	"ref", "ref_name", "repository", "workflow", "event_name", "sha", "run_id",
}

// TemplateSupported is the closed reference set, for error messages.
var TemplateSupported = []string{
	"github.ref", "github.ref_name", "github.repository", "github.workflow",
	"github.event_name", "github.sha", "github.run_id",
	"matrix.<axis>", "inputs.<name>",
}

// TemplateRef is one `${{ context.key }}` reference.
type TemplateRef struct {
	// Raw is the reference as written (the whole `${{ … }}` span).
	Raw string
	// Context is the lowercased context name ("github", "matrix", "inputs", …).
	Context string
	// Key is the reference key, case preserved (input and axis names are
	// authored names; a lookup is case-insensitive, see lookupRef).
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
// closing `}}`, or a span that is not a `context.key` reference (an operator,
// a function call), is an error: the caller decides whether that fails the file
// (a group) or leaves the text visible (a job name).
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

// splitRef splits `context.key` — or the four-segment
// `steps.<id>.outputs.<name>` / `needs.<job>.outputs.<name>` forms — and checks
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

// ValidateGroupTemplate checks a concurrency group's references against what
// the position can resolve: the fixed `github.*` set, the declaring job's
// matrix axes (none at workflow level — a workflow-level group expands once per
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
		case "github":
			if !containsFold(TemplateGitHubFields, ref.Key) {
				return fmt.Errorf("concurrency.group: ${{ github.%s }} is not supported (supported: %s)",
					ref.Key, strings.Join(TemplateSupported, ", "))
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
	switch ref.Context {
	case "github":
		var present bool
		val, present = github[strings.ToLower(ref.Key)]
		ok = present && strings.TrimSpace(val) != ""
	case "matrix":
		val, ok = matrix[ref.Key]
	case "inputs":
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
