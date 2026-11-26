package runner

import (
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// TestStepIfReadsTheLayeredEnv is PAR-036's acceptance table: `if: env.FLAG ==
// '1'` (and the bare truthiness form) resolves against the authored environment
// layer in the documented order — workflow `env` → job `env` → container `env` —
// with the last one winning, and an absent name reading empty instead of the
// literal text.
//
// The step layer is deliberately NOT part of its own `if:`: a step's `env:` is
// applied when the step runs, and its `if:` is evaluated before that (GitHub's
// rule). TestSubstituteEnvLayers owns the step-layer-wins case for a step
// FIELD; this table owns the condition.
func TestStepIfReadsTheLayeredEnv(t *testing.T) {
	cases := []struct {
		name      string
		specEnv   map[string]string
		container map[string]string
		stepEnv   map[string]string
		expr      string
		want      bool
	}{
		{
			name:    "job layer, equality",
			specEnv: map[string]string{"FLAG": "1"},
			expr:    "env.FLAG == '1'",
			want:    true,
		},
		{
			name:    "job layer, inequality",
			specEnv: map[string]string{"FLAG": "0"},
			expr:    "env.FLAG == '1'",
			want:    false,
		},
		{
			name:      "container layer wins over the job layer",
			specEnv:   map[string]string{"FLAG": "1"},
			container: map[string]string{"FLAG": "0"},
			expr:      "env.FLAG == '1'",
			want:      false,
		},
		{
			name:      "container layer contributes a name the job layer lacks",
			container: map[string]string{"FLAG": "1"},
			expr:      "env.FLAG == '1'",
			want:      true,
		},
		{
			name:    "a step's own env is not in its own if",
			specEnv: map[string]string{"FLAG": "1"},
			stepEnv: map[string]string{"FLAG": "0"},
			expr:    "env.FLAG == '1'",
			want:    true,
		},
		{
			name: "an absent name reads empty, never the literal",
			expr: "env.MISSING == ''",
			want: true,
		},
		{
			name:    "bare truthiness follows the value",
			specEnv: map[string]string{"FLAG": "1", "OFF": "0"},
			expr:    "env.FLAG && !env.OFF",
			want:    true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			job := substJob()
			job.Spec.Env = c.specEnv
			if c.container != nil {
				job.Spec.Container.Env = c.container
			}
			job.Spec.Steps = []threads.StepSpec{{Run: "echo layered", Env: c.stepEnv}}
			p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			ctx := (&Executor{}).exprContext(p, false, quietStream(t), nil, nil)
			got, err := EvalIf(c.expr, ctx)
			if err != nil {
				t.Fatalf("EvalIf(%q): %v", c.expr, err)
			}
			if got != c.want {
				t.Fatalf("EvalIf(%q) = %v, want %v (env layer %v)", c.expr, got, c.want, ctx.Env)
			}
		})
	}
}
