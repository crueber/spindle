package threads

// PAR-037 through a parsed workflow: the parser accepts toJSON/fromJSON in the
// step `if:` and in a substituted step field, the spec carries them unchanged,
// and the same evaluator the runner uses produces the value.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestWorkflowJSONFunctionsParseAndEvaluate(t *testing.T) {
	src := []byte(`name: JSON
on: push
jobs:
  json:
    runs-on: linux
    strategy:
      matrix:
        go: ['1.27']
    steps:
      - id: build
        run: echo build
      - name: round trip
        if: fromJSON(toJSON(github)).event_name == 'push'
        run: echo ${{ toJSON(fromJSON('{"a":[1,2]}')) }}
      - name: json gate
        if: toJSON(matrix) == '{"go":"1.27"}'
        run: echo ok
      - name: from an output
        if: fromJSON(steps.build.outputs.payload).ok == '1'
        run: echo payload
`)
	w, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	spec := buildSpec("JSON", w, w.Jobs["json"], nil)
	if len(spec.Steps) != 4 {
		t.Fatalf("steps = %d", len(spec.Steps))
	}
	ctx := ExprContext{
		Success: true,
		GitHub:  map[string]string{"event_name": "push"},
		Matrix:  map[string]string{"go": "1.27"},
		Steps:   map[string]string{"build": "success"},
		Outputs: map[string]map[string]string{"build": {"payload": `{"ok":1}`}},
	}
	for _, i := range []int{1, 2, 3} {
		ok, err := EvalIf(spec.Steps[i].If, ctx)
		if err != nil || !ok {
			t.Errorf("step %q if %q = %v, %v; want true", spec.Steps[i].Name, spec.Steps[i].If, ok, err)
		}
	}
	got, err := workflow.Substitute("${{ toJSON(fromJSON('{\"a\":[1,2]}')) }}", workflow.TemplateValues{})
	if err != nil || got != `{"a":[1,2]}` {
		t.Fatalf("substitution = %q, %v; want the deterministic JSON", got, err)
	}
	// A step `run:` carrying the same span round-trips the parsed text.
	if !strings.Contains(spec.Steps[1].Run, "toJSON(fromJSON(") {
		t.Fatalf("run text = %q", spec.Steps[1].Run)
	}
	if out, err := workflow.Substitute(spec.Steps[1].Run, workflow.TemplateValues{}); err != nil || out != `echo {"a":[1,2]}` {
		t.Fatalf("run substitution = %q, %v", out, err)
	}
	// `case` stays a parse error: it is not a function in this language.
	if _, err := workflow.Parse([]byte(strings.Replace(string(src), "fromJSON(toJSON(github)).event_name == 'push'", "case(github.ref, 'a', 'b')", 1))); err == nil {
		t.Fatal("case(...) accepted")
	}
}
