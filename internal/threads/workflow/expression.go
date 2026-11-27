package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// `if:` and substitution expression validation (docs/threads.md §3.7).
//
// This is the parse-time half of the shared evaluator (expr_eval.go): the same
// grammar, checked against the contexts and functions the engine supports and
// against the availability table for the site, so an unusable condition is THAT
// FILE'S PARSE ERROR instead of a step that fails mid-run (or, worse, silently
// skips). The grammar is:
//
//	or      := and ("||" and)*
//	and     := unary ("&&" unary)*
//	unary   := "!" unary | comparison
//	compare := primary (("=="|"!="|"<"|"<="|">"|">=") primary)?
//	primary := "(" or ")" | 'text' | "text" | true | false
//	         | call | reference ("[" 'key' "]")*
//	call    := name "(" [or ("," or)*] ")"
//
// Operators return a value, not always a boolean: `&&`/`||` yield an operand
// (`a && b` is a when a is falsy and b otherwise; `a || b` is a when a is truthy
// and b otherwise), exactly like GitHub. Anything outside the grammar is an
// error naming the supported set, never an empty value.

// ifFunctions is the closed function set with its arity (a negative value is
// "one or more").
var ifFunctions = []ifFunction{
	{"success", 0, ""},
	{"failure", 0, ""},
	{"always", 0, ""},
	{"cancelled", 0, ""},
	{"canceled", 0, ""},
	{"startswith", 2, "startsWith(text, prefix)"},
	{"endswith", 2, "endsWith(text, suffix)"},
	{"contains", 2, "contains(text, search)"},
	{"format", -1, "format('{0}-{1}', a, b)"},
	{"join", 2, "join(value, separator)"},
	{"hashfiles", -1, "hashFiles('**/go.sum')"},
}

// ifFunction is one supported function: its lowercased name, arity (-1 means
// "one or more") and usage text.
type ifFunction struct {
	name   string
	arity  int
	detail string
}

// IfFunctions lists the supported expression function names for error messages
// and docs.
var IfFunctions = []string{
	"success()", "failure()", "always()", "cancelled()", "startsWith()",
	"endsWith()", "contains()", "format()", "join()", "hashFiles()",
}

// ValidateIf checks one `if:` expression against the scope. An empty string is
// valid (the implicit success()). A `${{ … }}` span is accepted only when it is
// the WHOLE condition (the substitution path, e.g. `if: ${{ inputs.flag }}`);
// an expression with operators is written natively, as GitHub's docs advise.
func ValidateIf(expr string, scope TemplateScope) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil
	}
	if inner, ok := wholeSpan(trimmed); ok {
		trimmed = strings.TrimSpace(inner)
		if trimmed == "" {
			return fmt.Errorf("if: ${{ }} is empty")
		}
	} else if strings.Contains(trimmed, "${{") {
		return fmt.Errorf("if: a ${{ … }} span is only substituted when it is the whole condition; write the expression natively (do not wrap it in ${{ }})")
	}
	if err := validateExpr(trimmed, scope); err != nil {
		return fmt.Errorf("if: %w", err)
	}
	return nil
}

// wholeSpan returns the inside of s when s is exactly one `${{ … }}` span.
func wholeSpan(s string) (string, bool) {
	if !strings.HasPrefix(s, "${{") || !strings.HasSuffix(s, "}}") {
		return "", false
	}
	inner := s[len("${{") : len(s)-len("}}")]
	if strings.Contains(inner, "}}") {
		return "", false
	}
	return inner, true
}

// validateExpr checks one expression (the inside of an `if:` or a `${{ … }}`
// span) against a scope, including the context availability table.
func validateExpr(expr string, scope TemplateScope) error {
	v := &ifValidator{src: expr, scope: scope}
	v.skipSpace()
	if err := v.parseOr(); err != nil {
		return err
	}
	v.skipSpace()
	if v.pos != len(v.src) {
		return fmt.Errorf("unexpected %q", v.src[v.pos:])
	}
	return nil
}

type ifValidator struct {
	src   string
	pos   int
	scope TemplateScope
}

func (v *ifValidator) parseOr() error {
	if err := v.parseAnd(); err != nil {
		return err
	}
	for {
		v.skipSpace()
		if !v.consume("||") {
			return nil
		}
		if err := v.parseAnd(); err != nil {
			return err
		}
	}
}

func (v *ifValidator) parseAnd() error {
	if err := v.parseUnary(); err != nil {
		return err
	}
	for {
		v.skipSpace()
		if !v.consume("&&") {
			return nil
		}
		if err := v.parseUnary(); err != nil {
			return err
		}
	}
}

func (v *ifValidator) parseUnary() error {
	v.skipSpace()
	if v.consume("!") {
		return v.parseUnary()
	}
	return v.parseComparison()
}

func (v *ifValidator) parseComparison() error {
	if err := v.parsePrimary(); err != nil {
		return err
	}
	v.skipSpace()
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if v.consume(op) {
			return v.parsePrimary()
		}
	}
	return nil
}

func (v *ifValidator) parsePrimary() error {
	v.skipSpace()
	if v.consume("(") {
		if err := v.parseOr(); err != nil {
			return err
		}
		v.skipSpace()
		if !v.consume(")") {
			return fmt.Errorf("missing )")
		}
		return nil
	}
	if v.pos < len(v.src) && (v.src[v.pos] == '\'' || v.src[v.pos] == '"') {
		return v.parseString()
	}
	word := v.parseWord()
	if word == "" {
		return fmt.Errorf("expected a value at %q", v.src[v.pos:])
	}
	switch strings.ToLower(word) {
	case "true", "false":
		return nil
	}
	if isNumberLiteral(word) {
		return nil
	}
	if v.pos < len(v.src) && v.src[v.pos] == '(' {
		v.pos++
		return v.parseCall(strings.ToLower(word))
	}
	path := strings.Split(word, ".")
	for {
		v.skipSpace()
		if v.pos >= len(v.src) {
			break
		}
		switch v.src[v.pos] {
		case '[':
			v.pos++
			v.skipSpace()
			if v.pos >= len(v.src) || (v.src[v.pos] != '\'' && v.src[v.pos] != '"') {
				return fmt.Errorf("property access must be a quoted key, as in github['sha']")
			}
			start := v.pos
			if err := v.parseString(); err != nil {
				return err
			}
			key := strings.TrimSpace(v.src[start+1 : v.pos-1])
			v.skipSpace()
			if !v.consume("]") {
				return fmt.Errorf("missing ] after a property key")
			}
			path = append(path, key)
		case '.':
			v.pos++
			next := v.parseWord()
			if next == "" {
				return fmt.Errorf("expected a property name after .")
			}
			path = append(path, strings.Split(next, ".")...)
		default:
			return v.validateReference(path, strings.Join(path, "."))
		}
	}
	return v.validateReference(path, strings.Join(path, "."))
}

// parseCall validates one function call and its arguments.
func (v *ifValidator) parseCall(name string) error {
	fn, ok := functionSpec(name)
	if !ok {
		return fmt.Errorf("unsupported function %q (supported: %s)", name, strings.Join(IfFunctions, ", "))
	}
	if name == "hashfiles" && !siteAllowsHashFiles(v.scope.effectiveSite()) {
		return fmt.Errorf("hashFiles() is not available in %s (it reads the job workspace, so it runs in a step; available in: a step field, a step if:, a job outputs value)", v.scope.effectiveSite())
	}
	args := 0
	v.skipSpace()
	if !v.consume(")") {
		for {
			if err := v.parseOr(); err != nil {
				return err
			}
			args++
			v.skipSpace()
			if v.consume(")") {
				break
			}
			if !v.consume(",") {
				return fmt.Errorf("expected , or ) in %s(", name)
			}
		}
	}
	if fn.arity >= 0 && args != fn.arity {
		usage := name + "()"
		if fn.detail != "" {
			usage = fn.detail
		}
		return fmt.Errorf("%s takes %d argument(s), got %d (usage: %s)", name, fn.arity, args, usage)
	}
	if fn.arity < 0 && args == 0 {
		return fmt.Errorf("%s needs at least one argument (usage: %s)", name, fn.detail)
	}
	return nil
}

// functionSpec looks up an if function by its lowercased name.
func functionSpec(name string) (ifFunction, bool) {
	for _, fn := range ifFunctions {
		if fn.name == name {
			return fn, true
		}
	}
	return ifFunction{}, false
}

// validateReference checks one dotted reference (with any bracket segments
// already flattened into path) against the site's availability table and the
// scope's declared names.
func (v *ifValidator) validateReference(path []string, word string) error {
	if len(path) < 2 {
		return fmt.Errorf("unknown identifier %q", word)
	}
	context := strings.ToLower(path[0])
	if context == "jobs" && !v.scope.CallOutput {
		return fmt.Errorf("%s is only available in an `on.workflow_call.outputs.<name>.value` template (a job output is read elsewhere as needs.<job>.outputs.<name>)", word)
	}
	if !contextAvailable(v.scope.effectiveSite(), context) {
		return fmt.Errorf("%s is not available in %s (available contexts: %s)",
			word, v.scope.effectiveSite(), listOrNone(SiteContexts(v.scope.effectiveSite())))
	}
	switch context {
	case "steps":
		return v.validateStepPath(path, word)
	case "needs":
		return v.validateNeedPath(path, word)
	case "github", "forgejo", "forge":
		if pathHasFilter(path) {
			return fmt.Errorf("%s: object filters are not supported on the github context", word)
		}
		if len(path) == 2 && strings.EqualFold(path[1], "action_path") {
			if !v.scope.ActionPath {
				return fmt.Errorf("${{ %s.action_path }} is only available in a composite action's own steps (it is the action's directory; a workflow step has none)", context)
			}
			return nil
		}
		if name, ok := eventInputName(strings.Join(path[1:], ".")); ok {
			if !containsExact(v.scope.Inputs, name) {
				return fmt.Errorf("%s is not a declared workflow_dispatch input (declared: %s)",
					word, listOrNone(v.scope.Inputs))
			}
			return nil
		}
		if field, ok := eventFieldName(strings.Join(path[1:], ".")); ok {
			if !ValidEventField(field) {
				return fmt.Errorf("%s is not a supported event field (supported: %s)",
					word, strings.Join(EventFields, ", "))
			}
			return nil
		}
		if len(path) != 2 || !ValidGitHubField(path[1]) {
			return fmt.Errorf("%s is not a supported context field (supported: %s)",
				word, strings.Join(GitHubFields, ", "))
		}
		return nil
	case "runner":
		if len(path) != 2 || !ValidRunnerField(path[1]) {
			return fmt.Errorf("%s is not a supported runner field (supported: %s)", word, strings.Join(RunnerFields, ", "))
		}
		return nil
	case "vars":
		if len(path) != 2 || !templateIdent(path[1]) {
			return fmt.Errorf("%s is not a valid variable name", word)
		}
		return nil
	case "matrix":
		if len(path) != 2 || !containsExact(v.scope.Axes, path[1]) {
			return fmt.Errorf("%s is not a declared matrix key (declared: %s)", word, listOrNone(v.scope.Axes))
		}
		return nil
	case "inputs":
		if len(path) != 2 || !containsExact(v.scope.Inputs, path[1]) {
			return fmt.Errorf("%s is not a declared workflow_dispatch input (declared: %s)", word, listOrNone(v.scope.Inputs))
		}
		return nil
	case "env":
		if len(path) != 2 {
			return fmt.Errorf("%s is not a valid environment reference (the form is env.<NAME>)", word)
		}
		return nil // env names are authored at run time; a missing one reads empty
	case "secrets":
		if len(path) != 2 || !ValidSecretName(path[1]) {
			return fmt.Errorf("%s is not a valid secret reference (the form is secrets.<name>; names match [A-Za-z_][A-Za-z0-9_]*)", word)
		}
		if v.scope.SecretsClosed && !containsExact(v.scope.SecretNames, path[1]) {
			return fmt.Errorf("%s is not declared by this workflow's `on.workflow_call.secrets` (declared: %s); a called workflow receives only the secrets its interface declares",
				word, listOrNone(v.scope.SecretNames))
		}
		return nil
	}
	return fmt.Errorf("unknown context %q (supported: %s)", path[0], strings.Join(TemplateSupported, ", "))
}

// validateStepPath checks steps.<id>.* and the documented steps.* filters.
func (v *ifValidator) validateStepPath(path []string, word string) error {
	if len(path) >= 2 && path[1] == "*" {
		if (len(path) == 3 && (strings.EqualFold(path[2], "outcome") || strings.EqualFold(path[2], "conclusion"))) ||
			(len(path) >= 4 && strings.EqualFold(path[2], "outputs")) {
			return nil
		}
		return fmt.Errorf("%s: steps.* supports .outcome, .conclusion and .outputs.<name> only", word)
	}
	if len(path) == 3 && (strings.EqualFold(path[2], "outcome") || strings.EqualFold(path[2], "conclusion")) {
		return v.requireStep(path[1], word)
	}
	if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
		return v.requireStep(path[1], word)
	}
	return fmt.Errorf("only steps.<id>.outcome, steps.<id>.conclusion, steps.<id>.outputs.<name> and steps.*.<field> are supported")
}

// validateNeedPath checks needs.<job>.* and the documented needs.* filters.
func (v *ifValidator) validateNeedPath(path []string, word string) error {
	if len(path) >= 2 && path[1] == "*" {
		if (len(path) == 3 && strings.EqualFold(path[2], "result")) ||
			(len(path) >= 4 && strings.EqualFold(path[2], "outputs")) {
			return nil
		}
		return fmt.Errorf("%s: needs.* supports .result and .outputs.<name> only", word)
	}
	if len(path) == 3 && strings.EqualFold(path[2], "result") {
		return v.requireNeed(path[1], word)
	}
	if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
		return v.requireNeed(path[1], word)
	}
	return fmt.Errorf("only needs.<job>.result, needs.<job>.outputs.<name> and needs.*.<field> are supported")
}

// requireStep checks a `steps.<id>` reference against the steps declared before
// the site.
func (v *ifValidator) requireStep(id, word string) error {
	if !containsExact(v.scope.Steps, strings.ToLower(id)) {
		return fmt.Errorf("%s names a step that is not declared before this one (declared: %s)", word, listOrNone(v.scope.Steps))
	}
	return nil
}

// requireNeed checks a `needs.<job>` reference against the job's dependencies.
func (v *ifValidator) requireNeed(job, word string) error {
	if !containsExactFold([]string(v.scope.Needs), job) {
		return fmt.Errorf("%s names no `needs:` dependency of this job (declared: %s)", word, listOrNone(v.scope.Needs))
	}
	return nil
}

func (v *ifValidator) parseString() error {
	quote := v.src[v.pos]
	v.pos++
	for v.pos < len(v.src) {
		if v.src[v.pos] == quote {
			v.pos++
			return nil
		}
		v.pos++
	}
	return fmt.Errorf("unterminated string")
}

// parseWord reads an identifier, dotted expression, filter path or function
// name.
func (v *ifValidator) parseWord() string {
	start := v.pos
	for v.pos < len(v.src) {
		c := v.src[v.pos]
		if c == '_' || c == '.' || c == '-' || c == '*' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			v.pos++
			continue
		}
		break
	}
	return v.src[start:v.pos]
}

func (v *ifValidator) skipSpace() {
	for v.pos < len(v.src) && (v.src[v.pos] == ' ' || v.src[v.pos] == '\t' || v.src[v.pos] == '\n' || v.src[v.pos] == '\r') {
		v.pos++
	}
}

func (v *ifValidator) consume(tok string) bool {
	if strings.HasPrefix(v.src[v.pos:], tok) {
		v.pos += len(tok)
		return true
	}
	return false
}

// validateStepTemplates validates every substituted string field of one step
// against the closed vocabulary. `if:` is an expression; the other fields take
// references or (PAR-038) whole expressions.
func validateStepTemplates(jobName string, index int, step Step, scope TemplateScope) error {
	type field struct{ name, value string }
	fields := []field{
		{"name", step.Name}, {"run", step.Run},
		{"shell", step.Shell}, {"working-directory", step.WorkingDirectory},
	}
	envKeys := make([]string, 0, len(step.Env))
	for k := range step.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		fields = append(fields, field{"env." + k, step.Env[k]})
	}
	withKeys := make([]string, 0, len(step.With))
	for k := range step.With {
		withKeys = append(withKeys, k)
	}
	sort.Strings(withKeys)
	for _, k := range withKeys {
		fields = append(fields, field{"with." + k, step.With[k]})
	}
	for _, f := range fields {
		if err := ValidateTemplate(f.value, scope); err != nil {
			return fmt.Errorf("workflow: job %s step %d %s: %w", jobName, index+1, f.name, err)
		}
	}
	if err := ValidateIf(step.If, TemplateScope{
		Axes: scope.Axes, Inputs: scope.Inputs, Steps: scope.Steps, Needs: scope.Needs,
		SecretNames: scope.SecretNames, SecretsClosed: scope.SecretsClosed,
		Site: SiteStepIf,
	}); err != nil {
		return fmt.Errorf("workflow: job %s step %d if: %w", jobName, index+1, err)
	}
	if err := validateTimeout(fmt.Sprintf("job %s step %d", jobName, index+1), step.TimeoutMinutes, TemplateScope{
		Axes: scope.Axes, Inputs: scope.Inputs, Site: SiteStepTimeout,
	}); err != nil {
		return err
	}
	return nil
}
