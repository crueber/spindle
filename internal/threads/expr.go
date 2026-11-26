package threads

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// `if:` expressions (15 §1/§4, extended 2026-11-21). v1 evaluates the small,
// documented subset a workflow actually needs — it is not GitHub's expression
// language:
//
//	success()  failure()  always()  cancelled()
//	startsWith(a, b)  contains(a, b)  format('{0}', a, …)
//	github.<field>  forgejo.<field>  forge.<field>  github.event.inputs.<name>
//	matrix.<key>  inputs.<name>  env.<NAME>  secrets.<NAME>
//	steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
//	needs.<job>.result  needs.<job>.outputs.<name>
//	'literal'  true  false
//	!  ==  !=  &&  ||  ( )
//
// The github field set is closed (workflow.GitHubFields): a field the engine
// does not provide is an evaluation error naming the set, never an empty value.
// secrets.<name> reads the run's secret map (empty on an untrusted run, which
// is GitHub's rule: the reference resolves, the value is absent).
//
// Anything else is an evaluation ERROR, and the caller fails the step (or the
// job) with a clear message (never silently skipped): a workflow author must be
// able to see that spindle did not understand their condition.
//
// The evaluator lives in this package, not the runner, because BOTH ends
// evaluate conditions: the runner evaluates a step `if:` with the live step
// results, and the server evaluates a job-level `if:` (the queue decides
// eligibility, so a false condition must terminalise the job without a runner).
// One subset, one parser.

// ExprContext is what an `if` can read.
type ExprContext struct {
	Success   bool
	Failure   bool
	Cancelled bool
	GitHub    map[string]string
	Matrix    map[string]string
	Inputs    map[string]string
	Env       map[string]string
	// Steps maps a step id to its raw outcome (success|failure|cancelled|
	// skipped); StepConclusions maps the same id to the conclusion after
	// continue-on-error. Outputs maps the same id to the outputs the step
	// published (docs/threads.md §3.9); a step that failed or was skipped
	// published none.
	Steps           map[string]string
	StepConclusions map[string]string
	Outputs         map[string]map[string]string
	// Needs maps a `needs:` job key to its result (success|failure|cancelled|
	// skipped) for a job-level `if:`; NeedsOutputs maps the same key to the
	// dependency's outputs (only a successful dependency contributes). Both are
	// empty on the runner (a job never sees a dependency's record).
	Needs        map[string]string
	NeedsOutputs map[string]map[string]string
	// Secrets maps a secret name to its value for a trusted run. A reference to
	// an absent secret reads empty (never the literal `secrets.<name>` text).
	Secrets map[string]string
}

// EvalIf evaluates one condition. An empty condition means "success()" (the
// implicit default), which is what makes a plain step list run in order.
func EvalIf(expr string, c ExprContext) (bool, error) {
	expr = strings.TrimSpace(expr)
	if inner, ok := wholeIfSpan(expr); ok {
		expr = strings.TrimSpace(inner)
	}
	if expr == "" {
		return c.Success, nil
	}
	p := &exprParser{src: expr, ctx: c}
	v, err := p.parseOr()
	if err != nil {
		return false, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return false, fmt.Errorf("if: unexpected %q", p.src[p.pos:])
	}
	return v.truthy()
}

type exprParser struct {
	src string
	pos int
	ctx ExprContext
}

// value is a string/bool literal (all comparisons are string-wise, as in
// GitHub's loose equality: true/false compare as "true"/"false").
type value struct{ s string }

func (v value) truthy() (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v.s)) {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("if: %q is not a boolean", v.s)
}

func (p *exprParser) parseOr() (value, error) {
	left, err := p.parseAnd()
	if err != nil {
		return value{}, err
	}
	for {
		p.skipSpace()
		if !p.consume("||") {
			return left, nil
		}
		right, rerr := p.parseAnd()
		if rerr != nil {
			return value{}, rerr
		}
		lb, _ := left.truthy()
		rb, _ := right.truthy()
		left = value{s: strconv.FormatBool(lb || rb)}
	}
}

func (p *exprParser) parseAnd() (value, error) {
	left, err := p.parseUnary()
	if err != nil {
		return value{}, err
	}
	for {
		p.skipSpace()
		if !p.consume("&&") {
			return left, nil
		}
		right, rerr := p.parseUnary()
		if rerr != nil {
			return value{}, rerr
		}
		lb, _ := left.truthy()
		rb, _ := right.truthy()
		left = value{s: strconv.FormatBool(lb && rb)}
	}
}

func (p *exprParser) parseUnary() (value, error) {
	p.skipSpace()
	if p.consume("!") {
		v, err := p.parseUnary()
		if err != nil {
			return value{}, err
		}
		b, berr := v.truthy()
		if berr != nil {
			return value{}, berr
		}
		return value{s: strconv.FormatBool(!b)}, nil
	}
	return p.parseComparison()
}

func (p *exprParser) parseComparison() (value, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return value{}, err
	}
	p.skipSpace()
	for _, op := range []string{"==", "!="} {
		if p.consume(op) {
			right, rerr := p.parsePrimary()
			if rerr != nil {
				return value{}, rerr
			}
			eq := strings.EqualFold(left.s, right.s)
			if op == "!=" {
				eq = !eq
			}
			return value{s: strconv.FormatBool(eq)}, nil
		}
	}
	return left, nil
}

func (p *exprParser) parsePrimary() (value, error) {
	p.skipSpace()
	if p.consume("(") {
		v, err := p.parseOr()
		if err != nil {
			return value{}, err
		}
		p.skipSpace()
		if !p.consume(")") {
			return value{}, fmt.Errorf("if: missing )")
		}
		return v, nil
	}
	if p.pos < len(p.src) && (p.src[p.pos] == '\'' || p.src[p.pos] == '"') {
		return p.parseString()
	}
	word := p.parseWord()
	if word == "" {
		return value{}, fmt.Errorf("if: expected a value at %q", p.src[p.pos:])
	}
	switch strings.ToLower(word) {
	case "true", "false":
		return value{s: strings.ToLower(word)}, nil
	}
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		args, err := p.parseArgs()
		if err != nil {
			return value{}, err
		}
		return p.call(strings.ToLower(word), args)
	}
	return p.reference(word)
}

// reference resolves one dotted value.
func (p *exprParser) reference(word string) (value, error) {
	raw := strings.Split(word, ".")
	if len(raw) < 2 {
		// A bare identifier is the `true` shorthand only.
		return value{}, fmt.Errorf("if: unknown identifier %q", word)
	}
	switch strings.ToLower(raw[0]) {
	case "github", "forgejo", "forge":
		return p.githubValue(raw[0], raw)
	case "matrix":
		return value{s: p.ctx.Matrix[strings.Join(raw[1:], ".")]}, nil
	case "inputs":
		// Input names are case-sensitive (workflow_dispatch declares them).
		return value{s: p.ctx.Inputs[strings.Join(raw[1:], ".")]}, nil
	case "env":
		// Env names are case-sensitive (FOO, not foo).
		return value{s: p.ctx.Env[strings.Join(raw[1:], ".")]}, nil
	case "secrets":
		if len(raw) != 2 || !workflow.ValidSecretName(raw[1]) {
			return value{}, fmt.Errorf("if: secrets.%s is not a valid secret reference (the form is secrets.<name>)", strings.Join(raw[1:], "."))
		}
		return value{s: secretValue(p.ctx.Secrets, raw[1])}, nil
	case "steps":
		return p.stepValue(raw)
	case "needs":
		return p.needsValue(raw)
	}
	return value{}, fmt.Errorf("if: unknown context %q (supported: %s)", raw[0], strings.Join(workflow.TemplateSupported, ", "))
}

// githubValue resolves the github/forgejo/forge context against the closed
// field set, plus `event.inputs.<name>`.
func (p *exprParser) githubValue(context string, raw []string) (value, error) {
	if len(raw) >= 4 && strings.EqualFold(raw[1], "event") && strings.EqualFold(raw[2], "inputs") {
		return value{s: p.ctx.Inputs[strings.Join(raw[3:], ".")]}, nil
	}
	if len(raw) != 2 {
		return value{}, fmt.Errorf("if: only %s.<field> is supported (supported: %s)", context, strings.Join(workflow.GitHubFields, ", "))
	}
	field := strings.ToLower(raw[1])
	if !workflow.ValidGitHubField(field) {
		return value{}, fmt.Errorf("if: %s.%s is not a supported context field (supported: %s)", context, raw[1], strings.Join(workflow.GitHubFields, ", "))
	}
	return value{s: p.ctx.GitHub[field]}, nil
}

// parseArgs reads a function call's arguments through the closing ")".
func (p *exprParser) parseArgs() ([]value, error) {
	var args []value
	p.skipSpace()
	if p.consume(")") {
		return args, nil
	}
	for {
		arg, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		p.skipSpace()
		if p.consume(")") {
			return args, nil
		}
		if !p.consume(",") {
			return nil, fmt.Errorf("if: expected , or ) in a function call")
		}
	}
}

// call evaluates one function.
func (p *exprParser) call(name string, args []value) (value, error) {
	arity := func(want int, usage string) error {
		if len(args) != want {
			return fmt.Errorf("if: %s takes %d argument(s), got %d (usage: %s)", name, want, len(args), usage)
		}
		return nil
	}
	switch name {
	case "success":
		if err := arity(0, "success()"); err != nil {
			return value{}, err
		}
		return value{s: strconv.FormatBool(p.ctx.Success)}, nil
	case "failure":
		if err := arity(0, "failure()"); err != nil {
			return value{}, err
		}
		return value{s: strconv.FormatBool(p.ctx.Failure)}, nil
	case "always":
		if err := arity(0, "always()"); err != nil {
			return value{}, err
		}
		return value{s: "true"}, nil
	case "cancelled", "canceled":
		if err := arity(0, "cancelled()"); err != nil {
			return value{}, err
		}
		return value{s: strconv.FormatBool(p.ctx.Cancelled)}, nil
	case "startswith":
		if err := arity(2, "startsWith(text, prefix)"); err != nil {
			return value{}, err
		}
		return value{s: strconv.FormatBool(strings.HasPrefix(args[0].s, args[1].s))}, nil
	case "contains":
		if err := arity(2, "contains(text, search)"); err != nil {
			return value{}, err
		}
		return value{s: strconv.FormatBool(strings.Contains(args[0].s, args[1].s))}, nil
	case "format":
		if len(args) == 0 {
			return value{}, fmt.Errorf("if: format needs at least one argument (usage: format('{0}-{1}', a, b))")
		}
		return value{s: formatValue(args[0].s, args[1:])}, nil
	}
	return value{}, fmt.Errorf("if: unsupported function %q (supported: %s)", name, strings.Join(workflow.IfFunctions, ", "))
}

// stepValue resolves steps.<id>.outcome / .conclusion / .outputs.<name>. An
// output of a step that ran but did not publish that name reads empty (so
// `if: steps.x.outputs.y == ”` stays usable); an id that is not a step of this
// job is an error — a typo must never evaluate as "not set".
func (p *exprParser) stepValue(raw []string) (value, error) {
	if len(raw) == 3 {
		id := strings.ToLower(raw[1])
		switch {
		case strings.EqualFold(raw[2], "outcome"):
			return value{s: p.ctx.Steps[id]}, nil
		case strings.EqualFold(raw[2], "conclusion"):
			return value{s: p.ctx.StepConclusions[id]}, nil
		}
	}
	if len(raw) >= 4 && strings.EqualFold(raw[2], "outputs") {
		id := strings.ToLower(raw[1])
		if _, ok := p.ctx.Steps[id]; !ok {
			return value{}, fmt.Errorf("if: steps.%s.outputs.%s names no step id of this job", raw[1], strings.Join(raw[3:], "."))
		}
		return value{s: p.ctx.Outputs[id][strings.Join(raw[3:], ".")]}, nil
	}
	return value{}, fmt.Errorf("if: only steps.<id>.outcome, steps.<id>.conclusion and steps.<id>.outputs.<name> are supported")
}

// needsValue resolves needs.<job>.result / .outputs.<name>. A dependency that
// succeeded but published no such output reads empty; a key that is not a
// dependency of this job is an error.
func (p *exprParser) needsValue(raw []string) (value, error) {
	if len(raw) == 3 && strings.EqualFold(raw[2], "result") {
		return value{s: p.ctx.Needs[strings.ToLower(raw[1])]}, nil
	}
	if len(raw) >= 4 && strings.EqualFold(raw[2], "outputs") {
		key := strings.ToLower(raw[1])
		if _, ok := p.ctx.Needs[key]; !ok {
			return value{}, fmt.Errorf("if: needs.%s.outputs.%s names no `needs:` dependency", raw[1], strings.Join(raw[3:], "."))
		}
		return value{s: p.ctx.NeedsOutputs[key][strings.Join(raw[3:], ".")]}, nil
	}
	return value{}, fmt.Errorf("if: only needs.<job>.result and needs.<job>.outputs.<name> are supported")
}

func (p *exprParser) parseString() (value, error) {
	quote := p.src[p.pos]
	p.pos++
	start := p.pos
	for p.pos < len(p.src) {
		if p.src[p.pos] == quote {
			s := p.src[start:p.pos]
			p.pos++
			return value{s: s}, nil
		}
		p.pos++
	}
	return value{}, fmt.Errorf("if: unterminated string")
}

// parseWord reads an identifier, dotted expression or function call.
func (p *exprParser) parseWord() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c == '.' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func (p *exprParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n' || p.src[p.pos] == '\r') {
		p.pos++
	}
}

func (p *exprParser) consume(tok string) bool {
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

// wholeIfSpan returns the inside of s when s is exactly one `${{ … }}` span.
// `if: ${{ inputs.flag }}` is the documented substitution spelling; writing an
// expression with operators inside the braces is not supported (workflow's
// ValidateIf says so at parse time).
func wholeIfSpan(s string) (string, bool) {
	if !strings.HasPrefix(s, "${{") || !strings.HasSuffix(s, "}}") {
		return "", false
	}
	inner := s[len("${{") : len(s)-len("}}")]
	if strings.Contains(inner, "}}") {
		return "", false
	}
	return inner, true
}

// secretValue resolves a secret name, case-insensitively.
func secretValue(secrets map[string]string, name string) string {
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

// formatValue implements GitHub's format(): {N} refers to the Nth argument,
// {{ and }} are literal braces.
func formatValue(format string, args []value) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case '{':
			if i+1 < len(format) && format[i+1] == '{' {
				b.WriteByte('{')
				i++
				continue
			}
			end := strings.IndexByte(format[i+1:], '}')
			if end < 0 {
				b.WriteByte('{')
				continue
			}
			index := strings.TrimSpace(format[i+1 : i+1+end])
			n, err := strconv.Atoi(index)
			if err != nil || n < 0 || n >= len(args) {
				b.WriteString(format[i : i+2+end])
			} else {
				b.WriteString(args[n].s)
			}
			i += 1 + end
		case '}':
			if i+1 < len(format) && format[i+1] == '}' {
				b.WriteByte('}')
				i++
				continue
			}
			b.WriteByte('}')
		default:
			b.WriteByte(format[i])
		}
	}
	return b.String()
}
