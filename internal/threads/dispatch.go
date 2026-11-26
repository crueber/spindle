package threads

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// workflow_dispatch inputs (15 §1/§6, round 2). The parser already carries the
// declared schema (string/choice/boolean with required/default/options); this
// file turns it into the wire/UI shape the Threads page renders and validates a
// dispatcher's values against it. A mismatch is ErrUnprocessable (422) and the
// message names the offending input, which is what the page shows inline.

// DispatchInput is one declared workflow_dispatch input on the wire and in the
// page model. Default is the YAML scalar text ("" with HasDefault false when
// none was declared).
type DispatchInput struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // string | choice | boolean | number | environment
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required"`
	Default     string   `json:"default,omitempty"`
	HasDefault  bool     `json:"has_default,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// dispatchInputs projects a workflow file's declared inputs, sorted by name (a
// map in YAML has no order, and the page/API must be deterministic).
func dispatchInputs(f *WorkflowFile) []DispatchInput {
	if f == nil || f.Workflow == nil || f.Workflow.On.WorkflowDispatch == nil {
		return nil
	}
	declared := f.Workflow.On.WorkflowDispatch.Inputs
	if len(declared) == 0 {
		return nil
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]DispatchInput, 0, len(names))
	for _, name := range names {
		in := declared[name]
		typ := strings.TrimSpace(in.Type)
		if typ == "" {
			typ = "string"
		}
		item := DispatchInput{
			Name: name, Type: typ, Description: in.Description,
			Required: in.Required, Options: nonNilStr(in.Options),
		}
		if in.Default != nil {
			item.Default = *in.Default
			item.HasDefault = true
		}
		out = append(out, item)
	}
	return out
}

// resolveDispatchInputs validates the dispatcher's values against the declared
// schema and returns the effective map (declared defaults filled in for every
// input the dispatcher omitted). Every error wraps ErrUnprocessable and names
// the offending input, so the API answers 422 with it and the page shows it
// inline.
//
// Types: string accepts anything; boolean accepts true/false (any case,
// normalized to "true"/"false"); choice must be one of the declared options;
// number must be a decimal number (kept as authored, GitHub exposes the string
// form to `event.inputs`); environment must name an existing repository
// environment (environments is the resolved list; nil means the repository has
// none).
func resolveDispatchInputs(f *WorkflowFile, supplied map[string]string, environments []string) (map[string]string, error) {
	declared := map[string]workflow.Input{}
	if f != nil && f.Workflow != nil && f.Workflow.On.WorkflowDispatch != nil {
		declared = f.Workflow.On.WorkflowDispatch.Inputs
	}
	names := make([]string, 0, len(supplied))
	for name := range supplied {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("%w: input %q is not declared by %s", ErrUnprocessable, name, workflowLabel(f))
		}
	}
	effective := make(map[string]string, len(declared))
	declaredNames := make([]string, 0, len(declared))
	for name := range declared {
		declaredNames = append(declaredNames, name)
	}
	sort.Strings(declaredNames)
	for _, name := range declaredNames {
		in := declared[name]
		raw, given := supplied[name]
		raw = strings.TrimSpace(raw)
		if !given || raw == "" {
			if in.Default != nil {
				raw = strings.TrimSpace(*in.Default)
			}
		}
		if raw == "" {
			if in.Required {
				return nil, fmt.Errorf("%w: input %q is required by %s", ErrUnprocessable, name, workflowLabel(f))
			}
			continue // an omitted optional input has no value at all
		}
		switch inputType(in) {
		case "boolean":
			b, berr := strconv.ParseBool(strings.ToLower(raw))
			if berr != nil {
				return nil, fmt.Errorf("%w: input %q must be true or false, got %q", ErrUnprocessable, name, raw)
			}
			effective[name] = strconv.FormatBool(b)
		case "choice":
			ok := false
			for _, opt := range in.Options {
				if raw == opt {
					ok = true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf("%w: input %q must be one of %s, got %q",
					ErrUnprocessable, name, strings.Join(in.Options, ", "), raw)
			}
			effective[name] = raw
		case "number":
			if _, nerr := strconv.ParseFloat(raw, 64); nerr != nil {
				return nil, fmt.Errorf("%w: input %q must be a number, got %q", ErrUnprocessable, name, raw)
			}
			effective[name] = raw
		case "environment":
			ok := false
			for _, env := range environments {
				if raw == env {
					ok = true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf("%w: input %q must name a repository environment (declared: %s), got %q",
					ErrUnprocessable, name, strings.Join(environments, ", "), raw)
			}
			effective[name] = raw
		default:
			effective[name] = raw
		}
	}
	if len(effective) == 0 {
		return nil, nil
	}
	return effective, nil
}

// declaresEnvironmentInput reports whether a workflow declares an
// `environment`-typed input (the dispatch path then resolves the repository's
// environment names for validation and the picker).
func declaresEnvironmentInput(f *WorkflowFile) bool {
	if f == nil || f.Workflow == nil || f.Workflow.On.WorkflowDispatch == nil {
		return false
	}
	for _, in := range f.Workflow.On.WorkflowDispatch.Inputs {
		if inputType(in) == "environment" {
			return true
		}
	}
	return false
}

// inputType normalizes a declared input type (the parser's default is string).
func inputType(in workflow.Input) string {
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	if typ == "" {
		return "string"
	}
	return typ
}

// workflowLabel is the human name for an input-validation message.
func workflowLabel(f *WorkflowFile) string {
	if f == nil || strings.TrimSpace(f.Path) == "" {
		return "the workflow"
	}
	return f.Path
}
