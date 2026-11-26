package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// `if:` expression validation (docs/threads.md §3.7).
//
// The evaluator lives in internal/threads/expr.go; this is the parse-time half:
// the same grammar, checked against the contexts and functions the engine
// supports, so an unusable condition is THAT FILE'S PARSE ERROR instead of a
// step that fails mid-run (or, worse, silently skips). The grammar is:
//
//	or      := and ("||" and)*
//	and     := unary ("&&" unary)*
//	unary   := "!" unary | comparison
//	compare := primary (("==" | "!=") primary)?
//	primary := "(" or ")" | 'text' | "text" | true | false | call | reference
//	call    := name "(" [or ("," or)*] ")"
//
// Functions: success(), failure(), always(), cancelled()/canceled(),
// startsWith(a, b), contains(a, b), format(fmt, arg…). Anything else is an
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
	{"contains", 2, "contains(text, search)"},
	{"format", -1, "format('{0}-{1}', a, b)"},
}

// ifFunction is one supported function: its lowercased name, arity (-1 means
// one or more) and usage text.
type ifFunction struct {
	name   string
	arity  int
	detail string
}

// IfFunctions lists the supported `if:` function names for error messages and
// docs.
var IfFunctions = []string{"success()", "failure()", "always()", "cancelled()", "startsWith()", "contains()", "format()"}

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
	v := &ifValidator{src: trimmed, scope: scope}
	return v.validate()
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

type ifValidator struct {
	src   string
	pos   int
	scope TemplateScope
}

func (v *ifValidator) validate() error {
	v.skipSpace()
	if err := v.parseOr(); err != nil {
		return err
	}
	v.skipSpace()
	if v.pos != len(v.src) {
		return fmt.Errorf("if: unexpected %q", v.src[v.pos:])
	}
	return nil
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
	for _, op := range []string{"==", "!="} {
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
			return fmt.Errorf("if: missing )")
		}
		return nil
	}
	if v.pos < len(v.src) && (v.src[v.pos] == '\'' || v.src[v.pos] == '"') {
		return v.parseString()
	}
	word := v.parseWord()
	if word == "" {
		return fmt.Errorf("if: expected a value at %q", v.src[v.pos:])
	}
	switch strings.ToLower(word) {
	case "true", "false":
		return nil
	}
	if v.pos < len(v.src) && v.src[v.pos] == '(' {
		v.pos++
		return v.parseCall(strings.ToLower(word))
	}
	return v.validateReference(word)
}

// parseCall validates one function call and its arguments.
func (v *ifValidator) parseCall(name string) error {
	fn, ok := functionSpec(name)
	if !ok {
		return fmt.Errorf("if: unsupported function %q (supported: %s)", name, strings.Join(IfFunctions, ", "))
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
				return fmt.Errorf("if: expected , or ) in %s(", name)
			}
		}
	}
	if fn.arity >= 0 && args != fn.arity {
		usage := name + "()"
		if fn.detail != "" {
			usage = fn.detail
		}
		return fmt.Errorf("if: %s takes %d argument(s), got %d (usage: %s)", name, fn.arity, args, usage)
	}
	if fn.arity < 0 && args == 0 {
		return fmt.Errorf("if: %s needs at least one argument (usage: %s)", name, fn.detail)
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

// validateReference checks one dotted reference used as a value.
func (v *ifValidator) validateReference(word string) error {
	segments := strings.Split(word, ".")
	switch strings.ToLower(segments[0]) {
	case "steps":
		if len(segments) == 3 && (strings.EqualFold(segments[2], "outcome") || strings.EqualFold(segments[2], "conclusion")) {
			return v.requireStep(segments[1], word)
		}
		if len(segments) >= 4 && strings.EqualFold(segments[2], "outputs") {
			return v.requireStep(segments[1], word)
		}
		return fmt.Errorf("if: only steps.<id>.outcome, steps.<id>.conclusion and steps.<id>.outputs.<name> are supported")
	case "needs":
		if len(segments) == 3 && strings.EqualFold(segments[2], "result") {
			return v.requireNeed(segments[1], word)
		}
		if len(segments) >= 4 && strings.EqualFold(segments[2], "outputs") {
			return v.requireNeed(segments[1], word)
		}
		return fmt.Errorf("if: only needs.<job>.result and needs.<job>.outputs.<name> are supported")
	}
	ref, ok := splitRef(word)
	if !ok {
		if len(segments) < 2 {
			return fmt.Errorf("if: unknown identifier %q", word)
		}
		return fmt.Errorf("if: %q is not a supported reference (supported: %s)", word, strings.Join(TemplateSupported, ", "))
	}
	return validateRef(*ref, v.scope)
}

// requireStep checks a `steps.<id>` reference against the steps declared before
// the site.
func (v *ifValidator) requireStep(id, word string) error {
	if !containsExact(v.scope.Steps, strings.ToLower(id)) {
		return fmt.Errorf("if: %s names a step that is not declared before this one (declared: %s)", word, listOrNone(v.scope.Steps))
	}
	return nil
}

// requireNeed checks a `needs.<job>` reference against the job's dependencies.
func (v *ifValidator) requireNeed(job, word string) error {
	if !containsExactFold([]string(v.scope.Needs), job) {
		return fmt.Errorf("if: %s names no `needs:` dependency of this job (declared: %s)", word, listOrNone(v.scope.Needs))
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
	return fmt.Errorf("if: unterminated string")
}

// parseWord reads an identifier, dotted expression or function name.
func (v *ifValidator) parseWord() string {
	start := v.pos
	for v.pos < len(v.src) {
		c := v.src[v.pos]
		if c == '_' || c == '.' || c == '-' ||
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
// references only.
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
	if err := ValidateIf(step.If, scope); err != nil {
		return fmt.Errorf("workflow: job %s step %d if: %w", jobName, index+1, err)
	}
	return nil
}
