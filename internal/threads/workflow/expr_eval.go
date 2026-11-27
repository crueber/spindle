package workflow

// The workflow expression language (docs/WORKFLOWS.md §5.3/§6.3,
// docs/threads.md §3.7).
//
// One evaluator serves BOTH ends of a run: the server evaluates a job-level
// `if:`, and the runner evaluates a step `if:` and every `${{ … }}` span it
// substitutes. It lives here, beside the parser that validates the same
// grammar, so a condition accepted at parse time can always be evaluated — a
// second parser in another package would drift (PAR-038).
//
// The language is deliberately small, and every unsupported form is an error
// naming the supported set, never an empty value:
//
//	or       := and ("||" and)*
//	and      := not ("&&" not)*
//	not      := "!" not | comparison
//	comparison := primary (("=="|"!="|"<"|"<="|">"|">=") primary)?
//	primary  := "(" or ")" | 'text' | "text" | true | false
//	          | name "(" [or ("," or)*] ")" | reference ("[" 'key' "]")*
//	reference:= context ("." segment)+
//
// `&&` and `||` return an OPERAND, not a boolean (GitHub's rule):
// `a && b` is a when a is falsy and b otherwise; `a || b` is a when a is truthy
// and b otherwise. `!` and every comparison produce a boolean.
//
// Comparison type rules (documented, and pinned by tests): `==`/`!=` compare
// text case-insensitively — so `1 == '1'` and `true == 'true'` hold — while
// `<`/`<=`/`>`/`>=` compare numerically when BOTH operands parse as numbers and
// lexicographically (case-insensitive) otherwise, so `'2' < '10'` is true and
// `'a' < 'b'` is true.
//
// Filters: `needs.*.result`, `needs.*.outputs.<name>`, `steps.*.outcome`,
// `steps.*.conclusion` and `steps.*.outputs.<name>` produce a list, which
// `contains()` and `join()` consume. Any other `*` position is a named error.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EvalScope is what an expression can read. It is aliased as threads.ExprContext
// so the server's job condition and the runner's step condition are the same
// type (one evaluator, one meaning).
type EvalScope struct {
	Success   bool
	Failure   bool
	Cancelled bool
	GitHub    map[string]string
	Runner    map[string]string
	Vars      map[string]string
	Matrix    map[string]string
	Inputs    map[string]string
	Env       map[string]string
	// Steps maps a step id to its raw outcome (success|failure|cancelled|
	// skipped|timed_out); StepConclusions maps the same id to the conclusion
	// after continue-on-error. Outputs maps the same id to the outputs the
	// step published; a step that failed or was skipped published none.
	Steps           map[string]string
	StepConclusions map[string]string
	Outputs         map[string]map[string]string
	// Needs maps a `needs:` job key to its result; NeedsOutputs maps the key
	// to the dependency's outputs. Both are empty on the runner.
	Needs        map[string]string
	NeedsOutputs map[string]map[string]string
	// Secrets is the run's secret map. A reference to an absent secret reads
	// empty (never the literal text); an untrusted run's map is empty.
	Secrets map[string]string
	// HashFiles is the workspace walker for `hashFiles(...)` (PAR-041). It is
	// nil outside a job (a job-level `if:` has no workspace), where the parser
	// already refuses the function; a nil here is a named evaluation error.
	HashFiles func(patterns []string) (string, error)
	// Site enforces the context availability table at evaluation time too
	// (PAR-040): a stored workflow that bypassed the parser cannot read
	// `runner.*` in a job condition as an empty string. The zero value is a
	// step field, the permissive site.
	Site Site
}

// EvalIf evaluates one condition. An empty condition means success() (the
// implicit default).
func EvalIf(expr string, c EvalScope) (bool, error) {
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
		return false, fmt.Errorf("if: %w", err)
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return false, fmt.Errorf("if: unexpected %q", p.src[p.pos:])
	}
	b, terr := v.truthy()
	if terr != nil {
		return false, fmt.Errorf("if: %w", terr)
	}
	return b, nil
}

// EvalString evaluates one `${{ … }}` expression and renders its value as the
// text a substitution site receives. A boolean renders "true"/"false"; a list
// renders its members comma-joined (the form `format()` and a shell see).
func EvalString(expr string, c EvalScope) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", fmt.Errorf("${{ }} is empty")
	}
	p := &exprParser{src: expr, ctx: c}
	v, err := p.parseOr()
	if err != nil {
		return "", fmt.Errorf("${{ %s }}: %w", expr, err)
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return "", fmt.Errorf("${{ %s }}: unexpected %q", expr, p.src[p.pos:])
	}
	return v.text(), nil
}

// exprValue is one scalar or one filtered list. The engine has no objects: a
// context is a flat name→string map, exactly like the runner's environment.
type exprValue struct {
	s      string
	list   []exprValue
	isList bool
}

func scalar(s string) exprValue { return exprValue{s: s} }
func listValue(items []exprValue) exprValue {
	return exprValue{list: items, isList: true}
}

// text renders the value for a substitution site or a function argument.
func (v exprValue) text() string {
	if !v.isList {
		return v.s
	}
	parts := make([]string, len(v.list))
	for i, item := range v.list {
		parts[i] = item.text()
	}
	return strings.Join(parts, ",")
}

// truthy applies the documented boolean rule. A non-boolean, non-numeric
// string is an ERROR, never a silent true/false: the stand-in for a condition
// that is not a condition must be visible (the pre-existing rule, pinned by
// TestEvalIfErrorsAreExplicit).
func (v exprValue) truthy() (bool, error) {
	if v.isList {
		return len(v.list) > 0, nil
	}
	switch strings.ToLower(strings.TrimSpace(v.s)) {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%q is not a boolean", v.s)
}

// logical is the truthiness `&&`/`||` use to choose an operand: a non-empty
// string other than "false"/"0" is true (GitHub's rule for a value-returning
// logical operator). It is deliberately more permissive than truthy(), which
// guards a FINAL condition: `cond && 'prod' || 'dev'` is a value expression,
// not a boolean.
func (v exprValue) logical() bool {
	if v.isList {
		return len(v.list) > 0
	}
	switch strings.ToLower(strings.TrimSpace(v.s)) {
	case "", "false", "0":
		return false
	}
	return true
}

// number reports whether the value is numeric for an ordering comparison.
func (v exprValue) number() (float64, bool) {
	if v.isList {
		return 0, false
	}
	s := strings.TrimSpace(v.s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// equalValue is the documented loose equality: text-wise and case-insensitive
// (true == 'true'), list-wise for filters.
func equalValue(a, b exprValue) bool {
	if a.isList || b.isList {
		if !a.isList || !b.isList || len(a.list) != len(b.list) {
			return false
		}
		for i := range a.list {
			if !equalValue(a.list[i], b.list[i]) {
				return false
			}
		}
		return true
	}
	return strings.EqualFold(a.s, b.s)
}

// orderValue compares two scalars per the documented type rule.
func orderValue(a, b exprValue) int {
	if an, ok := a.number(); ok {
		if bn, ok := b.number(); ok {
			switch {
			case an < bn:
				return -1
			case an > bn:
				return 1
			default:
				return 0
			}
		}
	}
	return strings.Compare(strings.ToLower(a.s), strings.ToLower(b.s))
}

type exprParser struct {
	src string
	pos int
	ctx EvalScope
}

func (p *exprParser) parseOr() (exprValue, error) {
	left, err := p.parseAnd()
	if err != nil {
		return exprValue{}, err
	}
	for {
		p.skipSpace()
		if !p.consume("||") {
			return left, nil
		}
		right, rerr := p.parseAnd()
		if rerr != nil {
			return exprValue{}, rerr
		}
		if !left.logical() {
			left = right
		}
	}
}

func (p *exprParser) parseAnd() (exprValue, error) {
	left, err := p.parseUnary()
	if err != nil {
		return exprValue{}, err
	}
	for {
		p.skipSpace()
		if !p.consume("&&") {
			return left, nil
		}
		right, rerr := p.parseUnary()
		if rerr != nil {
			return exprValue{}, rerr
		}
		if left.logical() {
			left = right
		}
	}
}

func (p *exprParser) parseUnary() (exprValue, error) {
	p.skipSpace()
	if p.consume("!") {
		v, err := p.parseUnary()
		if err != nil {
			return exprValue{}, err
		}
		b, berr := v.truthy()
		if berr != nil {
			return exprValue{}, berr
		}
		return scalar(strconv.FormatBool(!b)), nil
	}
	return p.parseComparison()
}

func (p *exprParser) parseComparison() (exprValue, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return exprValue{}, err
	}
	p.skipSpace()
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if !p.consume(op) {
			continue
		}
		right, rerr := p.parsePrimary()
		if rerr != nil {
			return exprValue{}, rerr
		}
		var out bool
		switch op {
		case "==":
			out = equalValue(left, right)
		case "!=":
			out = !equalValue(left, right)
		case "<":
			out = orderValue(left, right) < 0
		case "<=":
			out = orderValue(left, right) <= 0
		case ">":
			out = orderValue(left, right) > 0
		case ">=":
			out = orderValue(left, right) >= 0
		}
		return scalar(strconv.FormatBool(out)), nil
	}
	return left, nil
}

func (p *exprParser) parsePrimary() (exprValue, error) {
	p.skipSpace()
	if p.consume("(") {
		v, err := p.parseOr()
		if err != nil {
			return exprValue{}, err
		}
		p.skipSpace()
		if !p.consume(")") {
			return exprValue{}, fmt.Errorf("missing )")
		}
		return v, nil
	}
	if p.pos < len(p.src) && (p.src[p.pos] == '\'' || p.src[p.pos] == '"') {
		return p.parseString()
	}
	word := p.parseWord()
	if word == "" {
		return exprValue{}, fmt.Errorf("expected a value at %q", p.src[p.pos:])
	}
	switch strings.ToLower(word) {
	case "true", "false":
		return scalar(strings.ToLower(word)), nil
	}
	if isNumberLiteral(word) {
		return scalar(word), nil
	}
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		args, err := p.parseArgs()
		if err != nil {
			return exprValue{}, err
		}
		return p.call(strings.ToLower(word), args)
	}
	path, err := p.parseIndexes(strings.Split(word, "."))
	if err != nil {
		return exprValue{}, err
	}
	return p.reference(path)
}

// parseIndexes consumes `['key']` / `["key"]` property accesses and any dotted
// segment that follows one, so `github['sha']` means `github.sha` and
// `steps['build'].outputs.x` is the output form.
func (p *exprParser) parseIndexes(path []string) ([]string, error) {
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return path, nil
		}
		switch p.src[p.pos] {
		case '[':
			p.pos++
			p.skipSpace()
			if p.pos >= len(p.src) || (p.src[p.pos] != '\'' && p.src[p.pos] != '"') {
				return nil, fmt.Errorf("property access must be a quoted key, as in github['sha']")
			}
			v, err := p.parseString()
			if err != nil {
				return nil, err
			}
			p.skipSpace()
			if !p.consume("]") {
				return nil, fmt.Errorf("missing ] after a property key")
			}
			path = append(path, v.s)
		case '.':
			p.pos++
			next := p.parseWord()
			if next == "" {
				return nil, fmt.Errorf("expected a property name after .")
			}
			path = append(path, strings.Split(next, ".")...)
		default:
			return path, nil
		}
	}
}

// reference resolves one dotted path, including the documented filters.
func (p *exprParser) reference(path []string) (exprValue, error) {
	if len(path) < 2 {
		return exprValue{}, fmt.Errorf("unknown identifier %q", path[0])
	}
	context := strings.ToLower(path[0])
	if !contextAvailable(p.ctx.Site, context) {
		return exprValue{}, fmt.Errorf("%s is not available in %s (available contexts: %s)", strings.Join(path, "."), p.ctx.Site, strings.Join(SiteContexts(p.ctx.Site), ", "))
	}
	switch context {
	case "github", "forgejo", "forge":
		return p.githubValue(context, path)
	case "runner":
		if pathHasFilter(path) || len(path) != 2 || !ValidRunnerField(path[1]) {
			return exprValue{}, fmt.Errorf("runner.%s is not a supported runner field (supported: %s)", strings.Join(path[1:], "."), strings.Join(RunnerFields, ", "))
		}
		return scalar(p.ctx.Runner[strings.ToLower(path[1])]), nil
	case "vars":
		if pathHasFilter(path) || len(path) != 2 {
			return exprValue{}, fmt.Errorf("vars.%s is not a valid variable reference (the form is vars.<name>)", strings.Join(path[1:], "."))
		}
		return scalar(lookupFold(p.ctx.Vars, path[1])), nil
	case "matrix":
		if pathHasFilter(path) || len(path) != 2 {
			return exprValue{}, fmt.Errorf("matrix.%s is not a valid matrix reference (the form is matrix.<axis>)", strings.Join(path[1:], "."))
		}
		return scalar(p.ctx.Matrix[path[1]]), nil
	case "inputs":
		if pathHasFilter(path) || len(path) != 2 {
			return exprValue{}, fmt.Errorf("inputs.%s is not a valid input reference (the form is inputs.<name>)", strings.Join(path[1:], "."))
		}
		return scalar(p.ctx.Inputs[path[1]]), nil
	case "env":
		if pathHasFilter(path) || len(path) != 2 {
			return exprValue{}, fmt.Errorf("env.%s is not a valid variable reference (the form is env.<NAME>)", strings.Join(path[1:], "."))
		}
		return scalar(p.ctx.Env[path[1]]), nil
	case "secrets":
		if pathHasFilter(path) || len(path) != 2 || !ValidSecretName(path[1]) {
			return exprValue{}, fmt.Errorf("secrets.%s is not a valid secret reference (the form is secrets.<name>)", strings.Join(path[1:], "."))
		}
		return scalar(lookupSecret(p.ctx.Secrets, path[1])), nil
	case "steps":
		return p.stepValue(path)
	case "needs":
		return p.needsValue(path)
	}
	return exprValue{}, fmt.Errorf("unknown context %q (supported: %s)", path[0], strings.Join(TemplateSupported, ", "))
}

// githubValue resolves the github/forgejo/forge context against the closed
// field set, plus `event.inputs.<name>` and `event.<documented field>`.
func (p *exprParser) githubValue(context string, path []string) (exprValue, error) {
	if pathHasFilter(path) {
		return exprValue{}, fmt.Errorf("%s.%s: object filters are not supported on the github context", context, strings.Join(path[1:], "."))
	}
	if len(path) >= 4 && strings.EqualFold(path[1], "event") && strings.EqualFold(path[2], "inputs") {
		return scalar(p.ctx.Inputs[strings.Join(path[3:], ".")]), nil
	}
	if len(path) >= 3 && strings.EqualFold(path[1], "event") {
		field := strings.Join(path[2:], ".")
		if !ValidEventField(field) {
			return exprValue{}, fmt.Errorf("%s.event.%s is not a supported event field (supported: %s)", context, field, strings.Join(EventFields, ", "))
		}
		return scalar(p.ctx.GitHub["event."+strings.ToLower(field)]), nil
	}
	if len(path) != 2 {
		return exprValue{}, fmt.Errorf("only %s.<field> is supported (supported: %s)", context, strings.Join(GitHubFields, ", "))
	}
	field := strings.ToLower(path[1])
	if !ValidGitHubField(field) {
		return exprValue{}, fmt.Errorf("%s.%s is not a supported context field (supported: %s)", context, path[1], strings.Join(GitHubFields, ", "))
	}
	return scalar(p.ctx.GitHub[field]), nil
}

// stepValue resolves steps.<id>.outcome/.conclusion/.outputs.<name>, and the
// documented `steps.*` filter forms.
func (p *exprParser) stepValue(path []string) (exprValue, error) {
	if len(path) >= 2 && path[1] == "*" {
		return p.stepFilter(path)
	}
	if len(path) == 3 {
		id := strings.ToLower(path[1])
		switch {
		case strings.EqualFold(path[2], "outcome"):
			return scalar(p.ctx.Steps[id]), nil
		case strings.EqualFold(path[2], "conclusion"):
			return scalar(p.ctx.StepConclusions[id]), nil
		}
	}
	if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
		id := strings.ToLower(path[1])
		if !ctxHasStep(p.ctx, id) {
			return exprValue{}, fmt.Errorf("steps.%s.outputs.%s names no step id of this job", path[1], strings.Join(path[3:], "."))
		}
		return scalar(p.ctx.Outputs[id][strings.Join(path[3:], ".")]), nil
	}
	return exprValue{}, fmt.Errorf("only steps.<id>.outcome, steps.<id>.conclusion, steps.<id>.outputs.<name> and steps.*.<field> are supported")
}

// stepFilter implements steps.*.outcome / .conclusion / .outputs.<name>.
func (p *exprParser) stepFilter(path []string) (exprValue, error) {
	if len(path) == 3 && (strings.EqualFold(path[2], "outcome") || strings.EqualFold(path[2], "conclusion")) {
		values := p.ctx.Steps
		if strings.EqualFold(path[2], "conclusion") {
			values = p.ctx.StepConclusions
		}
		return sortedList(values), nil
	}
	if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
		name := strings.Join(path[3:], ".")
		keys := make([]string, 0, len(p.ctx.Outputs))
		for id := range p.ctx.Outputs {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		out := make([]exprValue, 0, len(keys))
		for _, id := range keys {
			out = append(out, scalar(p.ctx.Outputs[id][name]))
		}
		return listValue(out), nil
	}
	return exprValue{}, fmt.Errorf("steps.* supports .outcome, .conclusion and .outputs.<name> only")
}

// needsValue resolves needs.<job>.result/.outputs.<name>, and needs.*.
func (p *exprParser) needsValue(path []string) (exprValue, error) {
	if len(path) >= 2 && path[1] == "*" {
		if len(path) == 3 && strings.EqualFold(path[2], "result") {
			return sortedList(p.ctx.Needs), nil
		}
		if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
			name := strings.Join(path[3:], ".")
			keys := make([]string, 0, len(p.ctx.NeedsOutputs))
			for id := range p.ctx.NeedsOutputs {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			out := make([]exprValue, 0, len(keys))
			for _, id := range keys {
				out = append(out, scalar(p.ctx.NeedsOutputs[id][name]))
			}
			return listValue(out), nil
		}
		return exprValue{}, fmt.Errorf("needs.* supports .result and .outputs.<name> only")
	}
	if len(path) == 3 && strings.EqualFold(path[2], "result") {
		return scalar(p.ctx.Needs[strings.ToLower(path[1])]), nil
	}
	if len(path) >= 4 && strings.EqualFold(path[2], "outputs") {
		key := strings.ToLower(path[1])
		if !ctxHasNeed(p.ctx, key) {
			return exprValue{}, fmt.Errorf("needs.%s.outputs.%s names no `needs:` dependency", path[1], strings.Join(path[3:], "."))
		}
		return scalar(p.ctx.NeedsOutputs[key][strings.Join(path[3:], ".")]), nil
	}
	return exprValue{}, fmt.Errorf("only needs.<job>.result, needs.<job>.outputs.<name> and needs.*.<field> are supported")
}

// ctxHasStep reports whether a step id is addressable: an outcome map names the
// steps that ran, an output map names the steps whose outputs the site knows.
func ctxHasStep(c EvalScope, id string) bool {
	if _, ok := c.Steps[id]; ok {
		return true
	}
	_, ok := c.Outputs[id]
	return ok
}

// ctxHasNeed reports whether a `needs:` key is addressable.
func ctxHasNeed(c EvalScope, key string) bool {
	if _, ok := c.Needs[key]; ok {
		return true
	}
	_, ok := c.NeedsOutputs[key]
	return ok
}

// sortedList renders a name→value map as a list in name order, so two
// evaluations of one condition see the same order (join() and a test's
// expected value depend on it).
func sortedList(values map[string]string) exprValue {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]exprValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, scalar(values[key]))
	}
	return listValue(out)
}

// isNumberLiteral reports whether a bare token is a numeric literal (GitHub's
// expression language has numbers; the engine compares them by value for an
// ordering operator).
func isNumberLiteral(token string) bool {
	if token == "" {
		return false
	}
	if _, err := strconv.ParseFloat(token, 64); err != nil {
		return false
	}
	return true
}

// pathHasFilter reports whether a path uses the `*` filter segment.
func pathHasFilter(path []string) bool {
	for _, segment := range path[1:] {
		if segment == "*" {
			return true
		}
	}
	return false
}

// parseArgs reads a function call's arguments through the closing ")".
func (p *exprParser) parseArgs() ([]exprValue, error) {
	var args []exprValue
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
			return nil, fmt.Errorf("expected , or ) in a function call")
		}
	}
}

// call evaluates one function.
func (p *exprParser) call(name string, args []exprValue) (exprValue, error) {
	arity := func(want int, usage string) error {
		if len(args) != want {
			return fmt.Errorf("%s takes %d argument(s), got %d (usage: %s)", name, want, len(args), usage)
		}
		return nil
	}
	switch name {
	case "success":
		if err := arity(0, "success()"); err != nil {
			return exprValue{}, err
		}
		return scalar(strconv.FormatBool(p.ctx.Success)), nil
	case "failure":
		if err := arity(0, "failure()"); err != nil {
			return exprValue{}, err
		}
		return scalar(strconv.FormatBool(p.ctx.Failure)), nil
	case "always":
		if err := arity(0, "always()"); err != nil {
			return exprValue{}, err
		}
		return scalar("true"), nil
	case "cancelled", "canceled":
		if err := arity(0, "cancelled()"); err != nil {
			return exprValue{}, err
		}
		return scalar(strconv.FormatBool(p.ctx.Cancelled)), nil
	case "startswith":
		if err := arity(2, "startsWith(text, prefix)"); err != nil {
			return exprValue{}, err
		}
		return scalar(strconv.FormatBool(strings.HasPrefix(args[0].text(), args[1].text()))), nil
	case "endswith":
		if err := arity(2, "endsWith(text, suffix)"); err != nil {
			return exprValue{}, err
		}
		return scalar(strconv.FormatBool(strings.HasSuffix(args[0].text(), args[1].text()))), nil
	case "contains":
		if err := arity(2, "contains(text, search)"); err != nil {
			return exprValue{}, err
		}
		if args[0].isList {
			for _, item := range args[0].list {
				if equalValue(item, args[1]) {
					return scalar("true"), nil
				}
			}
			return scalar("false"), nil
		}
		return scalar(strconv.FormatBool(strings.Contains(args[0].text(), args[1].text()))), nil
	case "join":
		if err := arity(2, "join(value, separator)"); err != nil {
			return exprValue{}, err
		}
		sep := args[1].text()
		if !args[0].isList {
			return scalar(args[0].text()), nil
		}
		parts := make([]string, len(args[0].list))
		for i, item := range args[0].list {
			parts[i] = item.text()
		}
		return scalar(strings.Join(parts, sep)), nil
	case "format":
		if len(args) == 0 {
			return exprValue{}, fmt.Errorf("format needs at least one argument (usage: format('{0}-{1}', a, b))")
		}
		return scalar(formatValue(args[0].text(), args[1:])), nil
	case "hashfiles":
		if len(args) == 0 {
			return exprValue{}, fmt.Errorf("hashFiles needs at least one pattern (usage: hashFiles('**/go.sum')")
		}
		if !siteAllowsHashFiles(p.ctx.Site) {
			return exprValue{}, fmt.Errorf("hashFiles() is not available in %s (it reads the job workspace, so it runs in a step)", p.ctx.Site)
		}
		if p.ctx.HashFiles == nil {
			return exprValue{}, fmt.Errorf("hashFiles() is not available at this site (it reads the job workspace, so it runs in a step; a job-level condition has no workspace)")
		}
		patterns := make([]string, len(args))
		for i, arg := range args {
			if arg.isList {
				return exprValue{}, fmt.Errorf("hashFiles patterns must be strings, not a list")
			}
			patterns[i] = arg.s
		}
		sum, err := p.ctx.HashFiles(patterns)
		if err != nil {
			return exprValue{}, err
		}
		return scalar(sum), nil
	}
	return exprValue{}, fmt.Errorf("unsupported function %q (supported: %s)", name, strings.Join(IfFunctions, ", "))
}

func (p *exprParser) parseString() (exprValue, error) {
	quote := p.src[p.pos]
	p.pos++
	start := p.pos
	for p.pos < len(p.src) {
		if p.src[p.pos] == quote {
			s := p.src[start:p.pos]
			p.pos++
			return scalar(s), nil
		}
		p.pos++
	}
	return exprValue{}, fmt.Errorf("unterminated string")
}

// parseWord reads an identifier, dotted path or function name. `*` is part of
// the token so a filter path is one word.
func (p *exprParser) parseWord() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c == '.' || c == '-' || c == '*' ||
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

// lookupSecret resolves a secret by name, case-insensitively.
func lookupSecret(secrets map[string]string, name string) string {
	return lookupFold(secrets, name)
}

// lookupFold resolves an authored name case-insensitively (variables and
// secret names are stored in their canonical spelling and referenced freely).
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

// formatValue implements GitHub's format(): {N} refers to the Nth argument,
// {{ and }} are literal braces.
func formatValue(format string, args []exprValue) string {
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
				b.WriteString(args[n].text())
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
