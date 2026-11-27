package workflow

// toJSON/fromJSON coverage (PAR-037): deterministic JSON for every value the
// language produces, parsing with a named error, whole-context objects, indexing
// a parsed document, and the parse-time half accepting exactly what the
// evaluator can run.

import (
	"strings"
	"testing"
)

func jsonTestScope() EvalScope {
	return EvalScope{
		Success: true,
		GitHub: map[string]string{
			"sha": "abc123", "ref": "refs/heads/main", "event_name": "push",
			"event.inputs.mode": "fast",
		},
		Matrix:          map[string]string{"go": "1.27", "os": "linux"},
		Inputs:          map[string]string{"mode": "fast"},
		Env:             map[string]string{"FLAG": "1"},
		Secrets:         map[string]string{"TOKEN": "s3cret"},
		Steps:           map[string]string{"build": "success"},
		StepConclusions: map[string]string{"build": "success"},
		Outputs: map[string]map[string]string{
			"build": {"json": `{"a":1}`, "digest": "deadbeef"},
		},
		Needs:        map[string]string{"prep": "success"},
		NeedsOutputs: map[string]map[string]string{"prep": {"matrix": `{"go":["1.26","1.27"]}`}},
		Site:         SiteStepIf,
	}
}

func TestToJSONAndFromJSONEvaluation(t *testing.T) {
	c := jsonTestScope()

	// Substitution sites: the platform-expected value for each shape.
	for _, tc := range []struct{ expr, want string }{
		{`toJSON('a<b')`, `"a<b"`}, // JSON escaping, never HTML escaping
		{`toJSON('say "hi"')`, `"say \"hi\""`},
		{`toJSON(1.5)`, `1.5`},
		{`toJSON(true)`, `true`},
		{`toJSON(fromJSON('null'))`, `null`},
		{`toJSON(fromJSON('{"b":2,"a":[1,true,null,"x"]}'))`, `{"a":[1,true,null,"x"],"b":2}`},
		{`toJSON(fromJSON('[{"b":2,"a":1}]'))`, `[{"a":1,"b":2}]`},
		{`toJSON(fromJSON('1e3'))`, `1e3`},
		{`toJSON(matrix)`, `{"go":"1.27","os":"linux"}`},
		{`toJSON(github)`, `{"event":{"inputs":{"mode":"fast"}},"event_name":"push","ref":"refs/heads/main","sha":"abc123"}`},
		{`toJSON(steps)`, `{"build":{"conclusion":"success","outcome":"success","outputs":{"digest":"deadbeef","json":"{\"a\":1}"}}}`},
		{`toJSON(needs)`, `{"prep":{"outputs":{"matrix":"{\"go\":[\"1.26\",\"1.27\"]}"},"result":"success"}}`},
		{`toJSON(secrets)`, `{"TOKEN":"s3cret"}`},
		{`toJSON(env)`, `{"FLAG":"1"}`},
		{`fromJSON('{"a":{"b":[1,2]}}')['a'].b[1]`, `2`},
		{`fromJSON('[10,20]')[0]`, `10`},
		{`fromJSON('[10]')['0']`, `10`},
		{`fromJSON('"x"')`, `x`},
		{`fromJSON('  {"a" : 1}  ')['a']`, `1`},
		{`toJSON(fromJSON(toJSON(github)))`, `{"event":{"inputs":{"mode":"fast"}},"event_name":"push","ref":"refs/heads/main","sha":"abc123"}`},
		// Whole-context objects for every context that has one.
		{`toJSON(inputs)`, `{"mode":"fast"}`},
		{`toJSON(vars)`, `{}`},
		{`toJSON(env)`, `{"FLAG":"1"}`},
		{`toJSON(job)`, `{}`},
		{`toJSON(strategy)`, `{}`},
		{`toJSON(runner)`, `{}`},
		// Typed scalars and a whole parsed value in text position.
		{`1.5`, `1.5`},
		{`true`, `true`},
		{`github`, `{"event":{"inputs":{"mode":"fast"}},"event_name":"push","ref":"refs/heads/main","sha":"abc123"}`},
		{`fromJSON('{"a":1}')`, `{"a":1}`},
		{`fromJSON('null')`, ``},
		{`fromJSON('[1]')[5]`, ``},
		{`fromJSON('null').a`, ``},
		// `&&`/`||` yield the operand: a non-empty object is truthy, null and an
		// empty object are falsy.
		{`fromJSON('null') || 'fallback'`, `fallback`},
		{`fromJSON('{"a":1}') && 'yes'`, `yes`},
		{`fromJSON('{}') || 'fallback'`, `fallback`},
		{`'' || fromJSON('{"a":1}')`, `{"a":1}`},
	} {
		got, err := EvalString(tc.expr, c)
		if err != nil || got != tc.want {
			t.Errorf("EvalString(%q) = %q, %v; want %q", tc.expr, got, err, tc.want)
		}
	}

	// if: sites: parsed values participate in comparisons and truthiness.
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`toJSON(fromJSON('{"a":1}')) == '{"a":1}'`, true},
		{`toJSON(fromJSON('{"a":1}')) == '{"a": 1}'`, false},
		{`fromJSON('{"a":[1,2]}').a[1] == 2`, true},
		{`fromJSON('{"ok":true}').ok`, true},
		{`fromJSON('false')`, false},
		{`fromJSON('{}').missing == ''`, true}, // a missing key is null, which compares as empty
		{`contains(fromJSON('["a","b"]'), 'b')`, true},
		{`fromJSON('{"a":1}') == fromJSON('{"a":1}')`, true},
		{`fromJSON('{"a":1}') == fromJSON('{"a":2}')`, false},
		{`toJSON(github) == toJSON(forgejo)`, true},
		{`toJSON(fromJSON('[1,2]')) == '[1,2]'`, true},
		{`fromJSON('{"a":1}')`, true},
		{`fromJSON('{}')`, false},
		{`fromJSON('null')`, false},
		{`fromJSON('{"a":1}') == fromJSON('{"a":1,"b":2}')`, false},
		{`fromJSON('{"a":1}') == 'x'`, false},
	} {
		got, err := EvalIf(tc.expr, c)
		if err != nil || got != tc.want {
			t.Errorf("EvalIf(%q) = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}

	// Deterministic output: map iteration order must not reach the bytes.
	first, err := EvalString("toJSON(github)", c)
	if err != nil {
		t.Fatalf("toJSON(github): %v", err)
	}
	for i := 0; i < 100; i++ {
		again, aerr := EvalString("toJSON(github)", c)
		if aerr != nil || again != first {
			t.Fatalf("run %d = %q, %v; want the deterministic %q", i, again, aerr, first)
		}
	}

	// A whole-context span is an expression (the same JSON toJSON renders), and
	// it validates at parse time.
	if got, err := Substitute("${{ github }}", TemplateValues{GitHub: map[string]string{"sha": "abc"}}); err != nil || got != `{"sha":"abc"}` {
		t.Fatalf("whole-context span = %q, %v", got, err)
	}
	if err := ValidateTemplate("${{ github }}", TemplateScope{}); err != nil {
		t.Fatalf("whole-context span validation: %v", err)
	}
	// The context objects include an entry whose only source is one map (an
	// output-only step, an outputs-only dependency).
	extra := jsonTestScope()
	extra.Outputs["only_out"] = map[string]string{"x": "1"}
	extra.StepConclusions["only_conclusion"] = "success"
	if got, err := EvalString("toJSON(steps)", extra); err != nil ||
		!strings.Contains(got, `"only_out":{"outputs":{"x":"1"}}`) ||
		!strings.Contains(got, `"only_conclusion":{"conclusion":"success"}`) {
		t.Fatalf("toJSON(steps) with union-only ids = %q, %v", got, err)
	}
	extra.NeedsOutputs["only_needs_out"] = map[string]string{"y": "2"}
	if got, err := EvalString("toJSON(needs)", extra); err != nil ||
		!strings.Contains(got, `"only_needs_out":{"outputs":{"y":"2"}}`) {
		t.Fatalf("toJSON(needs) with an outputs-only dependency = %q, %v", got, err)
	}
}

func TestFromJSONErrors(t *testing.T) {
	c := jsonTestScope()
	for _, tc := range []struct{ expr, want string }{
		{`fromJSON('{')`, "invalid JSON"},
		{`fromJSON('')`, "invalid JSON"},
		{`fromJSON('{"a":1} trailing')`, "trailing"},
		{`fromJSON('1 2')`, "trailing"},
		{`fromJSON(matrix)`, "takes a JSON string"},
		{`fromJSON(github)`, "takes a JSON string"},
		{`fromJSON(fromJSON('[]'))`, "a list"},
		{`fromJSON(1)`, "a number"},
		{`fromJSON(true)`, "a boolean"},
		{`fromJSON(fromJSON('null'))`, "null"},
		{`fromJSON('[1]').a`, "property \"a\" of a list"},
		{`fromJSON('"x"').a`, "property \"a\" of a string"},
		{`fromJSON('[]')[`, "missing ]"},
		{`fromJSON('[]')[0`, "missing ] after an index"},
		{`fromJSON('[]')[-]`, "not an integer index"},
		{`fromJSON('[]').`, "expected a property name"},
		{`fromJSON('[]').*`, "property name"},
		{`toJSON(Inf)`, "not a JSON number"},
		{`toJSON(01)`, "not a JSON number"},
		{`fromJSON('{"a":1}').a.b`, "cannot read property"},
		{`fromJSON('"x"')[0]`, "cannot read index"},
		{`fromJSON('{"a":1}')[0]`, "cannot read index"},
		{`toJSON(fromJSON('{"a":1}'))['a']`, "cannot read property"},
	} {
		_, err := EvalString(tc.expr, c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("EvalString(%q) = %v, want %q", tc.expr, err, tc.want)
		}
	}
	// From a substitution site the error is wrapped with the span.
	if _, err := Substitute("${{ fromJSON('{') }}", TemplateValues{}); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("Substitute invalid JSON = %v, want the named parse/eval error", err)
	}
}

func TestJSONFunctionParseTime(t *testing.T) {
	scope := TemplateScope{
		Axes:   []string{"go"},
		Inputs: []string{"mode"},
		Steps:  []string{"build"},
		Needs:  []string{"prep"},
	}
	ok := []string{
		"toJSON(github) == 'x'",
		"toJSON(matrix) != ''",
		"fromJSON(inputs.mode).ok == '1'",
		`fromJSON('{"a":1}')['a'] == 1`,
		`fromJSON('[1,2]')[0] == 1`,
		"toJSON(fromJSON(toJSON(github))) == 'x'",
	}
	for _, expr := range ok {
		if err := ValidateIf(expr, scope); err != nil {
			t.Errorf("ValidateIf(%q): %v", expr, err)
		}
	}
	bad := []struct{ expr, want string }{
		{"toJSON()", "takes 1 argument"},
		{"fromJSON('a', 'b')", "takes 1 argument"},
		{"toJSON", "unknown identifier"}, // missing () is not a reference
		{"fromJSON('x')['a'", "missing ]"},
		{"fromJSON('x').", "expected a property name"},
		{"fromJSON('x')[a]", "quoted key or an integer index"},
		{"fromJSON('x').*", "property name"},
		{"toJSON(nope)", "unknown identifier"},
		{"case('a', 'b')", "unsupported function"},
	}
	for _, tc := range bad {
		err := ValidateIf(tc.expr, scope)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateIf(%q) = %v, want %q", tc.expr, err, tc.want)
		}
	}
	// A whole context follows the availability table at parse time too.
	jobScope := scope
	jobScope.JobIf = true
	if err := ValidateIf("toJSON(runner) == '{}'", jobScope); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("job toJSON(runner) = %v, want the named limitation", err)
	}
	// Step fields are substitute sites: the same calls validate there.
	stepScope := TemplateScope{Steps: []string{"build"}, Inputs: []string{"mode"}}
	if err := ValidateTemplate("echo ${{ toJSON(fromJSON(inputs.mode)) }}", stepScope); err != nil {
		t.Errorf("step field toJSON: %v", err)
	}
	if err := ValidateTemplate("echo ${{ fromJSON('{') }}", stepScope); err != nil {
		t.Errorf("fromJSON is a run-time parse: %v", err)
	}
}
