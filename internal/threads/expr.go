package threads

import (
	"fmt"
	"strconv"
	"strings"
)

// `if:` expressions (15 §1/§4, extended 2026-11-21). v1 evaluates the small,
// documented subset a workflow actually needs — it is not GitHub's expression
// language:
//
//	success()  failure()  always()  cancelled()
//	github.<field>  matrix.<axis>  inputs.<name>  env.<NAME>
//	steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
//	needs.<job>.result  needs.<job>.outputs.<name>
//	'literal'  true  false
//	!  ==  !=  &&  ||  ( )
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
}

// EvalIf evaluates one condition. An empty condition means "success()" (the
// implicit default), which is what makes a plain step list run in order.
func EvalIf(expr string, c ExprContext) (bool, error) {
	expr = strings.TrimSpace(expr)
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
	if strings.HasSuffix(word, "(") || strings.HasSuffix(word, "()") {
		if !strings.HasSuffix(word, "()") {
			return value{}, fmt.Errorf("if: unsupported function call %q", word)
		}
		fn := strings.ToLower(strings.TrimSuffix(word, "()"))
		switch fn {
		case "success":
			return value{s: strconv.FormatBool(p.ctx.Success)}, nil
		case "failure":
			return value{s: strconv.FormatBool(p.ctx.Failure)}, nil
		case "always":
			return value{s: "true"}, nil
		case "cancelled", "canceled":
			return value{s: strconv.FormatBool(p.ctx.Cancelled)}, nil
		}
		return value{}, fmt.Errorf("if: unsupported function %q", fn)
	}
	raw := strings.Split(word, ".")
	if len(raw) < 2 {
		// A bare identifier is the `true` shorthand only.
		return value{}, fmt.Errorf("if: unknown identifier %q", word)
	}
	switch strings.ToLower(raw[0]) {
	case "github":
		return value{s: p.ctx.GitHub[strings.ToLower(raw[1])]}, nil
	case "matrix":
		return value{s: p.ctx.Matrix[strings.ToLower(raw[1])]}, nil
	case "inputs":
		// Input names are case-sensitive (workflow_dispatch declares them).
		return value{s: p.ctx.Inputs[strings.Join(raw[1:], ".")]}, nil
	case "env":
		// Env names are case-sensitive (FOO, not foo).
		return value{s: p.ctx.Env[strings.Join(raw[1:], ".")]}, nil
	case "steps":
		return p.stepValue(raw)
	case "needs":
		return p.needsValue(raw)
	}
	return value{}, fmt.Errorf("if: unknown context %q", raw[0])
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
	word := p.src[start:p.pos]
	// A function call: the parser consumed the name, now the "()" pair.
	if p.pos+1 < len(p.src) && p.src[p.pos] == '(' && p.src[p.pos+1] == ')' {
		p.pos += 2
		word += "()"
	}
	return word
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
