package runner

import (
	"fmt"
	"strconv"
	"strings"
)

// Step `if` expressions (15 §1/§4). v1 evaluates the small, documented subset a
// workflow actually needs — it is not GitHub's expression language:
//
//	success()  failure()  always()  cancelled()
//	github.<field>  matrix.<axis>  inputs.<name>  env.<NAME>  steps.<id>.outcome
//	'literal'  true  false
//	!  ==  !=  &&  ||  ( )
//
// Anything else is an evaluation ERROR, and the step fails with a clear
// message (never silently skipped): a workflow author must be able to see that
// spindle did not understand their condition.

// ExprContext is what an `if` can read.
type ExprContext struct {
	Success   bool
	Failure   bool
	Cancelled bool
	GitHub    map[string]string
	Matrix    map[string]string
	Inputs    map[string]string
	Env       map[string]string
	Steps     map[string]string // step id → outcome
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
		if len(raw) == 3 && strings.EqualFold(raw[2], "outcome") {
			return value{s: p.ctx.Steps[strings.ToLower(raw[1])]}, nil
		}
		return value{}, fmt.Errorf("if: only steps.<id>.outcome is supported")
	}
	return value{}, fmt.Errorf("if: unknown context %q", raw[0])
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
