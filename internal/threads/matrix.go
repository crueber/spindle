package threads

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Matrix expansion (15 §1/§4, pass B2). `strategy.matrix` is the declaration
// the parser accepted (axes plus include/exclude): one job instance per
// expanded cell, in a deterministic order (axes sorted by name, values in
// declaration order; include entries in list order).
// Names follow GitHub's convention — the job key and display name gain
// "(v1, v2)" — and every instance gets its own job id, checks context and
// Spec.Matrix cell. fail-fast rides the strategy and is enforced by Complete.

// matrixCell is one expanded combination.
type matrixCell struct {
	// Values is the axis → value map (sorted by axis).
	Values map[string]string
	// Suffix is the display suffix " (v1, v2)" ("" for a plain job).
	Suffix string
}

// strategyMatrix converts a spec's matrix declaration to the parser's shared
// expansion input.
func strategyMatrix(s *StrategySpec) workflow.Matrix {
	if s == nil {
		return workflow.Matrix{}
	}
	m := workflow.Matrix{Axes: s.Matrix}
	for _, combo := range s.Include {
		m.Include = append(m.Include, workflow.MatrixCombination(combo))
	}
	for _, combo := range s.Exclude {
		m.Exclude = append(m.Exclude, workflow.MatrixCombination(combo))
	}
	return m
}

// expandMatrix expands a matrix declaration into cells. A job with no matrix
// yields exactly one empty cell, so the caller has one code path. Axes are
// sorted so two servers expand the same workflow identically (the job ids are
// bucket keys and checks contexts); `include`/`exclude` follow the parser's
// documented rules (workflow.Matrix.Expand).
func expandMatrix(s *StrategySpec) []matrixCell {
	m := strategyMatrix(s)
	if !m.Enabled() {
		return []matrixCell{{}}
	}
	cells := m.Expand()
	out := make([]matrixCell, 0, len(cells))
	for _, cell := range cells {
		parts := make([]string, 0, len(cell.Order))
		for _, key := range cell.Order {
			parts = append(parts, cell.Values[key])
		}
		suffix := ""
		if len(parts) > 0 {
			suffix = " (" + strings.Join(parts, ", ") + ")"
		}
		out = append(out, matrixCell{Values: cell.Values, Suffix: suffix})
	}
	return out
}

// stepExprs reports whether any step (or parallel member) has an
// expression-valued timeout, so a job without one pays no copy.
func stepExprs(steps []StepSpec) bool {
	for _, st := range steps {
		if st.TimeoutExpr != "" || stepExprs(st.Parallel) {
			return true
		}
	}
	return false
}

// evalStepTimeouts resolves every expression-valued step timeout for one matrix
// cell. The slice is copied before mutation: the spec's steps are shared by all
// cells of the product.
func evalStepTimeouts(steps []StepSpec, github, matrix, inputs, strategy map[string]string) ([]StepSpec, error) {
	out := make([]StepSpec, len(steps))
	copy(out, steps)
	for i := range out {
		if out[i].TimeoutExpr != "" {
			mins, err := workflow.EvalMinutesWithStrategy(out[i].TimeoutExpr, github, matrix, inputs, strategy)
			if err != nil {
				return nil, fmt.Errorf("step timeout-minutes: %w", err)
			}
			out[i].TimeoutMinutes = mins
			out[i].TimeoutExpr = ""
		}
		if len(out[i].Parallel) > 0 {
			parallel, err := evalStepTimeouts(out[i].Parallel, github, matrix, inputs, strategy)
			if err != nil {
				return nil, err
			}
			out[i].Parallel = parallel
		}
	}
	return out, nil
}

// matrixGroupKey is the base job key a matrix expansion belongs to. It is
// carried on the spec so fail-fast can cancel exactly the sibling cells (a run
// may hold several matrix jobs).
func matrixGroupKey(key string, cells int) string {
	if cells <= 1 {
		return ""
	}
	return key
}

// matrixIsMatrix reports whether the strategy declares any combination: a
// usable axis or an include entry.
func matrixIsMatrix(s *StrategySpec) bool {
	return strategyMatrix(s).Enabled()
}

// strategyContextFor is one cell's `strategy.*` context (PAR-039): job-index and
// job-total only exist for a matrix job, fail-fast defaults to true, and
// max-parallel reads the declared bound or the product's size (spindle's
// effective bound: every cell may run at once). A plain job has no cell, so its
// context is empty and every strategy reference reads empty.
func strategyContextFor(s *StrategySpec, index, total int) map[string]string {
	return strategyContextIf(matrixIsMatrix(s), s, index, total)
}

// strategyContextIf is strategyContextFor with the matrix check hoisted, for a
// caller expanding many cells of one declaration.
func strategyContextIf(isMatrix bool, s *StrategySpec, index, total int) map[string]string {
	if !isMatrix {
		return nil
	}
	ctx := map[string]string{
		"job-index": strconv.Itoa(index),
		"job-total": strconv.Itoa(total),
		"fail-fast": "true",
	}
	if s != nil && s.FailFast != nil && !*s.FailFast {
		ctx["fail-fast"] = "false"
	}
	maxParallel := total
	if s != nil && s.MaxParallel > 0 {
		maxParallel = s.MaxParallel
	}
	ctx["max-parallel"] = strconv.Itoa(maxParallel)
	return ctx
}

// StrategyContextOf is the `strategy.*` context of one stored job record
// (PAR-039). The server writes MatrixIndex/MatrixTotal at expansion time and
// the runner reads them back, so both ends resolve the same values; a plain job
// (part of no matrix) has no strategy context at all.
func StrategyContextOf(spec *Spec) map[string]string {
	if spec == nil || spec.MatrixTotal <= 0 {
		return nil
	}
	return strategyContextFor(spec.Strategy, spec.MatrixIndex, spec.MatrixTotal)
}

// validateMatrixProduct bounds the expansion (a workflow cannot ask one event
// for an unbounded number of jobs; the run record would be unreadable). The
// parser owns the bound; the server keeps the same one for a stored spec.
const maxMatrixJobs = workflow.MaxMatrixCells

// matrixProductSize returns the expanded cell count and a validation error. It
// is the server-side safety net for a spec stored before the parser bounded the
// declaration.
func matrixProductSize(s *StrategySpec) (int, error) {
	m := strategyMatrix(s)
	if !m.Enabled() {
		return 1, nil
	}
	if err := m.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnprocessable, err)
	}
	return len(m.Expand()), nil
}

// SubstituteMatrix replaces every `${{ matrix.<axis> }}` in s with the cell
// value (GitHub's expression spelling; whitespace inside the braces is
// tolerated). An unknown axis is left untouched so the runner can report it.
func SubstituteMatrix(s string, vals map[string]string) string {
	return workflow.FillContext(s, "matrix", vals)
}

// SubstituteInputs replaces every `${{ inputs.<name> }}` in s with the resolved
// workflow_dispatch input value (15 §1; the runner substitutes step names,
// scripts, env, if, shell and working-directory, exactly like matrix axes). An
// unknown input is left untouched.
func SubstituteInputs(s string, vals map[string]string) string {
	return workflow.FillContext(s, "inputs", vals)
}

// jobInstance is one job record to create: the expanded key/name and its spec.
type jobInstance struct {
	Key  string
	Name string
	Spec *Spec
}

// expandContext is the run-level context a job's templates expand against.
type expandContext struct {
	// GitHub is the run's github.* map (ref, ref_name, repository, workflow,
	// event_name, sha, run_id).
	GitHub map[string]string
	// Inputs is the effective workflow_dispatch inputs map (run values plus
	// declared defaults), used for `inputs.*` in a group or a job name.
	Inputs map[string]string
}

// expandJobSpec expands one workflow job into its matrix instances. A job with
// no product yields exactly one instance carrying the original key/name.
func expandJobSpec(key, display string, base *Spec, ec expandContext) ([]jobInstance, error) {
	if base == nil {
		base = &Spec{}
	}
	var strat *StrategySpec
	if base.Strategy != nil {
		strat = base.Strategy
	}
	size, err := matrixProductSize(strat)
	if err != nil {
		return nil, err
	}
	cells := expandMatrix(strat)
	// A name that names a matrix axis already distinguishes its cells, so the
	// "(v1, v2)" suffix is not appended (GitHub's rule): `cell-${{ matrix.v }}`
	// renders as `cell-a`, and the check context follows the rendered name.
	suffix := true
	if jobNameNamesMatrix(display) {
		suffix = false
	}
	out := make([]jobInstance, 0, len(cells))
	isMatrix := matrixIsMatrix(strat)
	for i, cell := range cells {
		spec := *base // shallow copy: env/steps/maps are read-only after creation
		spec.Matrix = nil
		if len(cell.Values) > 0 {
			spec.Matrix = cell.Values
		}
		spec.MatrixIndex = i
		spec.MatrixTotal = len(cells)
		if !isMatrix {
			// A job with no matrix has exactly one empty cell; it carries no
			// strategy context (PAR-039), so leave the index/total at zero.
			spec.MatrixIndex, spec.MatrixTotal = 0, 0
		}
		// The cell's `strategy.*` context (PAR-039): resolved here, once, so
		// runs-on, the job name, its lock key and its timeouts all read the
		// same values the runner substitutes in the steps.
		strategy := strategyContextIf(isMatrix, strat, i, len(cells))
		spec.MatrixGroup = matrixGroupKey(key, size)
		// `runs-on` labels are the ONE structural field a matrix may vary
		// (PAR-053): resolve them for this cell before the job record is
		// written, so the queue matches the real label set.
		labels, lerr := workflow.ExpandRunsOnWithStrategy(spec.RunsOn, ec.GitHub, cell.Values, ec.Inputs, strategy)
		if lerr != nil {
			return nil, lerr
		}
		spec.RunsOn = labels
		// A job timeout is a literal or one expression (PAR-055); a job that
		// names none gets GitHub's 360-minute default.
		if spec.TimeoutExpr != "" {
			mins, terr := workflow.EvalMinutesWithStrategy(spec.TimeoutExpr, ec.GitHub, cell.Values, ec.Inputs, strategy)
			if terr != nil {
				return nil, fmt.Errorf("workflow: job %s timeout-minutes: %w", key, terr)
			}
			spec.TimeoutMinutes = mins
			spec.TimeoutExpr = ""
		}
		if spec.TimeoutMinutes == 0 {
			spec.TimeoutMinutes = workflow.DefaultJobTimeoutMinutes
		}
		if stepExprs(spec.Steps) {
			steps, serr := evalStepTimeouts(spec.Steps, ec.GitHub, cell.Values, ec.Inputs, strategy)
			if serr != nil {
				return nil, fmt.Errorf("workflow: job %s: %w", key, serr)
			}
			spec.Steps = steps
		}
		// The concurrency group may name a matrix axis ("build-${{ matrix.go }}")
		// or the run's ref ("ci-${{ github.ref }}"): each cell gets its own
		// expanded group, which is what keeps a matrix parallel and a branch's
		// runs off another branch's key.
		if spec.Concurrency != nil {
			conc := *spec.Concurrency
			group, gerr := ExpandGroupWithStrategy(conc.Group, ec.GitHub, cell.Values, ec.Inputs, strategy)
			if gerr != nil {
				return nil, gerr
			}
			conc.Group = group
			spec.Concurrency = &conc
		}
		name := ExpandJobNameWithStrategy(display, ec.GitHub, cell.Values, ec.Inputs, strategy)
		suffixText := cell.Suffix
		if !suffix || suffixText == "" {
			suffixText = ""
			name = strings.TrimSpace(name)
		}
		out = append(out, jobInstance{Key: key + cell.Suffix, Name: name + suffixText, Spec: &spec})
	}
	return out, nil
}
