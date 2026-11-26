package workflow

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// `strategy.matrix` expansion (docs/WORKFLOWS.md §5.2).
//
// The matrix is a product of scalar axes plus GitHub's `include` / `exclude`
// lists. Expansion is a pure function of the declaration, so the parser can
// bound it (MaxMatrixCells) and the server expands the same cells the tests
// pin. The rules implemented here are GitHub's documented ones:
//
//  1. The base is the product of the non-empty axes, axes in name order and
//     values in declaration order.
//  2. An `exclude` object removes every base combination that matches ALL of
//     its key/value pairs.
//  3. An `include` object is merged into every surviving base combination
//     whose ORIGINAL axis values do not conflict with it (a key that is an
//     axis must already hold the same value); keys that are not axes are added
//     or overwritten. An object that fits no surviving combination becomes a
//     new combination of its own, in include-list order.
//  4. Include objects are never merged into combinations created by an earlier
//     include, and an excluded combination cannot be resurrected.
//
// Point 3 is what makes `include: [{color: green}, {color: pink, animal: cat}]`
// paint every cell green and then repaint the `cat` cells pink, while
// `include: [{fruit: banana}]` creates a banana cell rather than rewriting the
// fruit axis — the two examples GitHub documents.

// MaxMatrixCells bounds one matrix expansion. A workflow cannot ask one event
// for an unbounded number of jobs: the run record (and its check mirrors) would
// be unreadable. The parser rejects a larger declaration and the server keeps
// the same bound as a safety net.
const MaxMatrixCells = 256

// MatrixCombination is one `include:`/`exclude:` object: key → scalar value.
// Array values are a parse error (a nested matrix is not the documented shape).
type MatrixCombination map[string]string

// Matrix is `strategy.matrix`: the axes plus the include/exclude lists.
type Matrix struct {
	Axes    map[string][]string
	Include []MatrixCombination
	Exclude []MatrixCombination
}

// UnmarshalYAML decodes the mapping form. `include`/`exclude` are sequences of
// objects; every other key is an axis whose value is a scalar or a list of
// scalars.
func (m *Matrix) UnmarshalYAML(node *yaml.Node) error {
	if isNull(node) {
		*m = Matrix{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("strategy.matrix must be a mapping of axis to list")
	}
	out := Matrix{Axes: make(map[string][]string, len(node.Content)/2)}
	for i := 0; i+1 < len(node.Content); i += 2 {
		axis, val := node.Content[i].Value, node.Content[i+1]
		switch axis {
		case "include", "exclude":
			combos, err := matrixCombinations(val)
			if err != nil {
				return fmt.Errorf("strategy.matrix.%s: %w", axis, err)
			}
			if axis == "include" {
				out.Include = combos
			} else {
				out.Exclude = combos
			}
		default:
			list, err := scalarList(val)
			if err != nil {
				return fmt.Errorf("strategy.matrix.%s: %w", axis, err)
			}
			out.Axes[axis] = list
		}
	}
	*m = out
	return nil
}

// matrixCombinations decodes one include/exclude list.
func matrixCombinations(node *yaml.Node) ([]MatrixCombination, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("must be a list of objects (each object is a set of key: value pairs)")
	}
	out := make([]MatrixCombination, 0, len(node.Content))
	for index, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("entry %d must be an object of key: value pairs", index+1)
		}
		combo := make(MatrixCombination, len(item.Content)/2)
		for i := 0; i+1 < len(item.Content); i += 2 {
			key, val := item.Content[i].Value, item.Content[i+1]
			if key == "" {
				return nil, fmt.Errorf("entry %d has an empty key", index+1)
			}
			if val.Kind != yaml.ScalarNode || isNull(val) {
				return nil, fmt.Errorf("entry %d key %q must be a scalar value", index+1, key)
			}
			combo[key] = val.Value
		}
		out = append(out, combo)
	}
	return out, nil
}

// Enabled reports whether the matrix declares any combination: a usable axis
// with at least one value, or an include entry. A matrix that declares none is
// not a matrix (the job runs once).
func (m Matrix) Enabled() bool {
	if len(m.Include) > 0 {
		return true
	}
	for _, values := range m.Axes {
		if len(values) > 0 {
			return true
		}
	}
	return false
}

// Axes lists the usable axis names (declared with at least one value), sorted.
func (m Matrix) AxesNames() []string {
	out := make([]string, 0, len(m.Axes))
	for axis, values := range m.Axes {
		if len(values) == 0 {
			continue
		}
		out = append(out, axis)
	}
	sort.Strings(out)
	return out
}

// FieldNames lists every matrix key a `${{ matrix.<key> }}` reference may name:
// the usable axes plus the keys an include entry declares.
func (m Matrix) FieldNames() []string {
	seen := make(map[string]bool, len(m.Axes))
	out := m.AxesNames()
	for _, axis := range out {
		seen[axis] = true
	}
	for _, combo := range m.Include {
		for key := range combo {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// MatrixCell is one expanded combination.
type MatrixCell struct {
	// Values maps every key of the combination to its value.
	Values map[string]string
	// Order is the display order of Values' keys: the matrix axes (name order),
	// then any include-only keys (name order). It drives the job name suffix.
	Order []string
}

// Validate bounds one matrix declaration without expanding it. The parser runs
// it so an over-large matrix is that file's parse error, not a failed run.
func (m Matrix) Validate() error {
	axes := m.AxesNames()
	cells := 0
	if len(axes) > 0 {
		cells = 1
	}
	for _, axis := range axes {
		cells *= len(m.Axes[axis])
		if cells > MaxMatrixCells {
			return fmt.Errorf("strategy.matrix expands to more than %d jobs", MaxMatrixCells)
		}
	}
	if cells+len(m.Include) > MaxMatrixCells {
		return fmt.Errorf("strategy.matrix expands to more than %d jobs", MaxMatrixCells)
	}
	return nil
}

// Expand returns the matrix cells in a deterministic order: the base product
// first, then combinations created by an include entry, in include-list order.
// A matrix with no usable axis and no include entry yields one empty cell, so a
// caller always has the same code path.
func (m Matrix) Expand() []MatrixCell {
	axes := m.AxesNames()
	if len(axes) == 0 && len(m.Include) == 0 {
		return []MatrixCell{{Values: map[string]string{}}}
	}
	cells := make([]MatrixCell, 0, len(m.Include)+1)
	if len(axes) > 0 {
		cells = append(cells, MatrixCell{Values: map[string]string{}})
		for _, axis := range axes {
			values := m.Axes[axis]
			next := make([]MatrixCell, 0, len(cells)*len(values))
			for _, cell := range cells {
				for _, value := range values {
					vals := cloneValues(cell.Values, len(cell.Values)+1)
					vals[axis] = value
					next = append(next, MatrixCell{Values: vals})
				}
			}
			cells = next
		}
	}
	axisSet := make(map[string]bool, len(axes))
	for _, axis := range axes {
		axisSet[axis] = true
	}
	// exclude: a base combination goes when it matches every pair of an entry.
	if len(m.Exclude) > 0 {
		kept := cells[:0]
		for _, cell := range cells {
			if !excluded(cell.Values, m.Exclude) {
				kept = append(kept, cell)
			}
		}
		cells = kept
	}
	// include: merge into surviving BASE cells, else append a new combination.
	// An entry never merges into a combination an earlier include created (that
	// is why the two `fruit: banana` entries are two cells, not one).
	base := len(cells)
	for _, inc := range m.Include {
		merged := false
		for i := 0; i < base; i++ {
			if !includeApplies(cells[i].Values, axisSet, inc) {
				continue
			}
			for key, value := range inc {
				cells[i].Values[key] = value
			}
			merged = true
		}
		if !merged {
			cells = append(cells, MatrixCell{Values: cloneValues(inc, len(inc))})
		}
	}
	for i := range cells {
		cells[i].Order = cellOrder(axes, cells[i].Values)
	}
	if len(cells) > MaxMatrixCells {
		// Expand cannot fail (the parser rejected the declaration); the caller
		// bounds the result. Truncating here would silently change a workflow.
		return cells
	}
	return cells
}

// excluded reports whether a combination matches every pair of any exclude
// entry.
func excluded(values map[string]string, excludes []MatrixCombination) bool {
	for _, ex := range excludes {
		match := true
		for key, want := range ex {
			if values[key] != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// includeApplies reports whether an include object can merge into one base
// combination: every key that is a matrix axis must already hold the same
// value. Keys that are not axes are additive (and may overwrite an earlier
// include's value).
func includeApplies(values map[string]string, axisSet map[string]bool, inc MatrixCombination) bool {
	for key, want := range inc {
		if axisSet[key] && values[key] != want {
			return false
		}
	}
	return true
}

// cellOrder builds a cell's display order: axes in name order, then any other
// keys in name order.
func cellOrder(axes []string, values map[string]string) []string {
	order := make([]string, 0, len(values))
	for _, axis := range axes {
		if _, ok := values[axis]; ok {
			order = append(order, axis)
		}
	}
	extra := make([]string, 0, len(values))
	for key := range values {
		if containsExact(axes, key) {
			continue
		}
		extra = append(extra, key)
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// cloneValues copies a value map with room for extra keys.
func cloneValues(in map[string]string, capacity int) map[string]string {
	out := make(map[string]string, capacity)
	for k, v := range in {
		out[k] = v
	}
	return out
}
