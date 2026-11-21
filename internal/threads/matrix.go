package threads

import (
	"fmt"
	"sort"
	"strings"
)

// Matrix expansion (15 §1/§4, pass B2). `strategy.matrix` is the simple
// product the parser accepts: one job instance per combination, in a
// deterministic axis order (axes sorted by name, values in declaration order).
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

// expandMatrix expands a matrix product into cells. A nil/empty matrix yields
// exactly one empty cell, so the caller has one code path. Axes are sorted so
// two servers expand the same workflow identically (the job ids are bucket
// keys and checks contexts).
func expandMatrix(m map[string][]string) []matrixCell {
	if len(m) == 0 {
		return []matrixCell{{}}
	}
	axes := make([]string, 0, len(m))
	for axis := range m {
		if len(m[axis]) == 0 {
			continue // an empty axis contributes no value: skip it
		}
		axes = append(axes, axis)
	}
	sort.Strings(axes)
	if len(axes) == 0 {
		return []matrixCell{{}}
	}
	cells := []matrixCell{{Values: map[string]string{}}}
	for _, axis := range axes {
		next := make([]matrixCell, 0, len(cells)*len(m[axis]))
		for _, cell := range cells {
			for _, value := range m[axis] {
				vals := make(map[string]string, len(cell.Values)+1)
				for k, v := range cell.Values {
					vals[k] = v
				}
				vals[axis] = value
				next = append(next, matrixCell{Values: vals})
			}
		}
		cells = next
	}
	for i := range cells {
		parts := make([]string, 0, len(axes))
		for _, axis := range axes {
			parts = append(parts, cells[i].Values[axis])
		}
		cells[i].Suffix = " (" + strings.Join(parts, ", ") + ")"
	}
	return cells
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

// matrixIsMatrix reports whether the strategy declares a usable product.
func matrixIsMatrix(s *StrategySpec) bool {
	if s == nil || len(s.Matrix) == 0 {
		return false
	}
	for _, vals := range s.Matrix {
		if len(vals) > 0 {
			return true
		}
	}
	return false
}

// validateMatrixProduct bounds the expansion (a workflow cannot ask one event
// for an unbounded number of jobs; the run record would be unreadable).
const maxMatrixJobs = 256

// matrixProductSize returns the product size and a validation error.
func matrixProductSize(s *StrategySpec) (int, error) {
	if !matrixIsMatrix(s) {
		return 1, nil
	}
	size := 1
	for axis, vals := range s.Matrix {
		if len(vals) == 0 {
			continue
		}
		size *= len(vals)
		if size > maxMatrixJobs {
			return 0, fmt.Errorf("%w: strategy.matrix expands to more than %d jobs", ErrUnprocessable, maxMatrixJobs)
		}
		_ = axis
	}
	return size, nil
}

// SubstituteMatrix replaces every `${{ matrix.<axis> }}` in s with the cell
// value (GitHub's expression spelling; whitespace inside the braces is
// tolerated). An unknown axis is left untouched so the runner can report it.
func SubstituteMatrix(s string, vals map[string]string) string {
	return substituteContext(s, "matrix", vals)
}

// SubstituteInputs replaces every `${{ inputs.<name> }}` in s with the resolved
// workflow_dispatch input value (15 §1; the runner substitutes step names,
// scripts, env, if, shell and working-directory, exactly like matrix axes). An
// unknown input is left untouched.
func SubstituteInputs(s string, vals map[string]string) string {
	return substituteContext(s, "inputs", vals)
}

// substituteContext is the shared `${{ <context>.<key> }}` substituter.
func substituteContext(s, context string, vals map[string]string) string {
	if s == "" || len(vals) == 0 || !strings.Contains(s, context+".") {
		return s
	}
	for key, val := range vals {
		for _, form := range []string{
			"${{ " + context + "." + key + " }}",
			"${{" + context + "." + key + "}}",
			"${{ " + context + "." + key + "}}",
			"${{" + context + "." + key + " }}",
		} {
			s = strings.ReplaceAll(s, form, val)
		}
	}
	return s
}

// jobInstance is one job record to create: the expanded key/name and its spec.
type jobInstance struct {
	Key  string
	Name string
	Spec *Spec
}

// expandJobSpec expands one workflow job into its matrix instances. A job with
// no product yields exactly one instance carrying the original key/name.
func expandJobSpec(key, display string, base *Spec) ([]jobInstance, error) {
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
	var product map[string][]string
	if strat != nil {
		product = strat.Matrix
	}
	cells := expandMatrix(product)
	out := make([]jobInstance, 0, len(cells))
	for _, cell := range cells {
		spec := *base // shallow copy: env/steps/maps are read-only after creation
		spec.Matrix = nil
		if len(cell.Values) > 0 {
			spec.Matrix = cell.Values
		}
		spec.MatrixGroup = matrixGroupKey(key, size)
		// The concurrency group may name a matrix axis ("build-${{ matrix.go }}");
		// each cell gets its own group, which is what keeps a matrix parallel.
		if spec.Concurrency != nil {
			conc := *spec.Concurrency
			conc.Group = SubstituteMatrix(conc.Group, cell.Values)
			spec.Concurrency = &conc
		}
		out = append(out, jobInstance{Key: key + cell.Suffix, Name: display + cell.Suffix, Spec: &spec})
	}
	return out, nil
}
