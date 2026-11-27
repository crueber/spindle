package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Step and job outputs (docs/threads.md §3.9). The caps live here, in the
// leaf package both ends already import, so the parser, the server (which
// stores the reported map on the job record) and the runner (which collects it
// from the step's output file) can never disagree about a bound.
const (
	// MaxOutputsPerStep bounds the number of entries one step may publish
	// through its output file.
	MaxOutputsPerStep = 50
	// MaxOutputsPerJob bounds the collected outputs of one job (declared job
	// outputs and the reported map the server stores).
	MaxOutputsPerJob = 100
	// MaxOutputValueBytes bounds one output value.
	MaxOutputValueBytes = 64 << 10
	// MaxOutputBytes bounds the total name+value bytes one job may publish.
	MaxOutputBytes = 1 << 20
	// MaxOutputNameLen bounds one output name.
	MaxOutputNameLen = 128
)

// ValidOutputName reports whether name is a usable output name. The charset is
// the step-id charset (the shape `steps.<id>.outputs.<name>` reads), minus the
// leading-digit rule: an output is data, not an identifier prefix.
func ValidOutputName(name string) bool {
	if name == "" || len(name) > MaxOutputNameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateOutputs checks one collected output map against the caps and the name
// charset. Both ends call it: the runner before it reports, the server before
// it stores (a bugging runner must not enlarge a job record past the bound), and
// the parser for a declared `outputs:` map. The error names the offending
// output and the cap.
func ValidateOutputs(outs map[string]string) error {
	if len(outs) > MaxOutputsPerJob {
		return fmt.Errorf("outputs: %d outputs exceed the cap of %d per job", len(outs), MaxOutputsPerJob)
	}
	names := make([]string, 0, len(outs))
	for name := range outs {
		names = append(names, name)
	}
	sort.Strings(names)
	total := 0
	for _, name := range names {
		if !ValidOutputName(name) {
			return fmt.Errorf("outputs: invalid output name %q (letters, digits, _ and - only, at most %d bytes)", name, MaxOutputNameLen)
		}
		val := outs[name]
		if len(val) > MaxOutputValueBytes {
			return fmt.Errorf("outputs: output %q is %d bytes, over the cap of %d bytes per value", name, len(val), MaxOutputValueBytes)
		}
		total += len(name) + len(val)
	}
	if total > MaxOutputBytes {
		return fmt.Errorf("outputs: %d bytes total, over the cap of %d bytes per job", total, MaxOutputBytes)
	}
	return nil
}

// validateJobOutputs checks a job's declared `outputs:` map at parse time: the
// name charset, the caps, and every `${{ … }}` reference. A job output template
// may read `steps.<id>.outputs.<name>` (a step of this job), `matrix.<axis>`
// (this job's declared axes) and `inputs.<name>` (the workflow's declared
// dispatch inputs); anything else cannot be resolved at job end and is a parse
// error, never a literal that silently crosses a job boundary.
func validateJobOutputs(jobName string, job Job, inputNames, axes []string) error {
	if len(job.Outputs) == 0 {
		return nil
	}
	if err := ValidateOutputs(job.Outputs); err != nil {
		return fmt.Errorf("%w: job %s: %v", ErrBadOutput, jobName, err)
	}
	declared := make([]string, 0, len(job.Steps))
	for _, step := range job.Steps {
		if step.ID != "" {
			declared = append(declared, strings.ToLower(step.ID))
		}
	}
	names := make([]string, 0, len(job.Outputs))
	for name := range job.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ValidateTemplate(job.Outputs[name], TemplateScope{
			Axes: axes, Inputs: inputNames, Steps: declared, Needs: []string(job.Needs),
			Site: SiteJobOutputs,
		}); err != nil {
			return fmt.Errorf("%w: job %s: outputs.%s: %v", ErrBadOutput, jobName, name, err)
		}
	}
	return nil
}

// validateStepFieldRefs checks the `${{ steps.<id>.outputs.<name> }}` and
// `${{ needs.<job>.outputs.<name> }}` references in one step's fields against
// what the position can resolve: a step may only read the output of a step
// declared BEFORE it, and a needed job must be one the job declares. Catching
// the typo at parse time beats failing the step mid-run.
func validateStepFieldRefs(jobName string, index int, step Step, needs Needs, declared []string) error {
	fields := []struct {
		name  string
		value string
	}{
		{"name", step.Name}, {"run", step.Run}, {"if", step.If},
		{"shell", step.Shell}, {"working-directory", step.WorkingDirectory},
	}
	for k, v := range step.Env {
		fields = append(fields, struct {
			name  string
			value string
		}{"env." + k, v})
	}
	withKeys := make([]string, 0, len(step.With))
	for k := range step.With {
		withKeys = append(withKeys, k)
	}
	sort.Strings(withKeys)
	for _, k := range withKeys {
		fields = append(fields, struct {
			name  string
			value string
		}{"with." + k, step.With[k]})
	}
	for _, field := range fields {
		refs, err := OutputRefs(field.value)
		if err != nil {
			return fmt.Errorf("%w: job %s step %d: %s: %v", ErrBadOutput, jobName, index+1, field.name, err)
		}
		for _, ref := range refs {
			switch ref.Context {
			case "steps":
				if !containsExact(declared, strings.ToLower(ref.Step)) {
					return fmt.Errorf("%w: job %s step %d: %s: ${{ %s }} names a step that is not declared before this one (declared: %s)",
						ErrBadOutput, jobName, index+1, field.name, ref.Raw, listOrNone(declared))
				}
			case "needs":
				if !containsExactFold([]string(needs), ref.Job) {
					return fmt.Errorf("%w: job %s step %d: %s: ${{ %s }} names no `needs:` dependency of this job (declared: %s)",
						ErrBadOutput, jobName, index+1, field.name, ref.Raw, listOrNone([]string(needs)))
				}
			}
		}
	}
	return nil
}
