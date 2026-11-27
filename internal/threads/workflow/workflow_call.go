package workflow

// `on.workflow_call` — the reusable-workflow contract (PAR-075, PAR-076;
// docs/WORKFLOWS.md §4.5, docs/threads.md §3.4b).
//
// A workflow that declares `on.workflow_call` can be called from another
// workflow's `jobs.<id>.uses`. The declaration is the callee's interface and is
// enforced in both directions:
//
//   - typed inputs (`string`, `boolean`, `number`, with `required`/`default`)
//     are the only `inputs.*` names the callee may read, and a caller's `with:`
//     is checked against them (unknown name, missing required value and a value
//     that does not parse as the declared type are all named errors);
//   - declared secrets (`required` optional) are the only `secrets.*` names the
//     callee may read — a callee that declares workflow_call is a closed secret
//     environment, never the repository's whole secret bag (D-6);
//   - outputs carry a value template over the callee's own jobs
//     (`${{ jobs.<id>.outputs.<name> }}`), and the caller reads them as
//     `needs.<caller job>.outputs.<name>`.
//
// The `jobs` context exists ONLY in an output value: it is not a substituted
// site anywhere else, so accepting it in a step template would be the
// accepted-and-inert failure the parser exists to prevent.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// CallInputTypes is the closed `on.workflow_call.inputs.<name>.type` set.
var CallInputTypes = []string{"string", "boolean", "number"}

// WorkflowCallTrigger is one parsed `on.workflow_call` declaration.
type WorkflowCallTrigger struct {
	Inputs  map[string]CallInput  `yaml:"inputs"`
	Secrets map[string]CallSecret `yaml:"secrets"`
	Outputs map[string]CallOutput `yaml:"outputs"`
}

// CallInput is one declared typed input. Default is the YAML scalar text (an
// unquoted `default: true` reads "true", which is what a boolean input must
// parse as).
type CallInput struct {
	Description string
	Required    bool
	Type        string
	Default     string
	HasDefault  bool
}

// CallSecret is one declared secret. Required means the caller must pass it
// (`secrets: inherit` on a same-repository callee satisfies every declaration;
// a cross-repository callee must list it explicitly, D-6).
type CallSecret struct {
	Description string
	Required    bool
}

// CallOutput is one declared output: a description and a value template over
// the callee's jobs.
type CallOutput struct {
	Description string
	Value       string
}

// UnmarshalYAML validates the declaration's key set by hand (yaml.v3 does not
// propagate KnownFields into a custom unmarshaler).
func (w *WorkflowCallTrigger) UnmarshalYAML(node *yaml.Node) error {
	if isNull(node) {
		return nil
	}
	if err := checkKeys(node, "inputs", "secrets", "outputs"); err != nil {
		return fmt.Errorf("on.workflow_call: %w", err)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("on.workflow_call: expected a mapping (inputs, secrets, outputs)")
	}
	var trigger WorkflowCallTrigger
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "inputs":
			inputs, err := decodeCallInputs(val)
			if err != nil {
				return err
			}
			trigger.Inputs = inputs
		case "secrets":
			secrets, err := decodeCallSecrets(val)
			if err != nil {
				return err
			}
			trigger.Secrets = secrets
		case "outputs":
			outputs, err := decodeCallOutputs(val)
			if err != nil {
				return err
			}
			trigger.Outputs = outputs
		}
	}
	*w = trigger
	return nil
}

// decodeCallInputs decodes and validates the typed inputs map.
func decodeCallInputs(node *yaml.Node) (map[string]CallInput, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("on.workflow_call.inputs: expected a mapping of input name to definition")
	}
	out := make(map[string]CallInput, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !templateIdent(name) {
			return nil, fmt.Errorf("on.workflow_call.inputs: %q is not a valid input name (letters, digits, _ and -; not starting with a digit)", name)
		}
		var in CallInput
		if err := node.Content[i+1].Decode(&in); err != nil {
			return nil, fmt.Errorf("on.workflow_call.inputs.%s: %w", name, err)
		}
		if in.Type == "" {
			return nil, fmt.Errorf("on.workflow_call.inputs.%s: type is required (one of %s)", name, strings.Join(CallInputTypes, ", "))
		}
		if !containsFold(CallInputTypes, in.Type) {
			return nil, fmt.Errorf("on.workflow_call.inputs.%s: type %q is not supported (one of %s)", name, in.Type, strings.Join(CallInputTypes, ", "))
		}
		in.Type = strings.ToLower(in.Type)
		if in.HasDefault {
			if err := validateCallInputValue(name, in.Type, in.Default); err != nil {
				return nil, fmt.Errorf("on.workflow_call.inputs.%s.default: %w", name, err)
			}
		}
		out[name] = in
	}
	return out, nil
}

// UnmarshalYAML validates one input definition's keys.
func (in *CallInput) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "description", "required", "type", "default"); err != nil {
		return err
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping (description/required/type/default)")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "description":
			if err := val.Decode(&in.Description); err != nil {
				return fmt.Errorf("description: %w", err)
			}
		case "required":
			if err := val.Decode(&in.Required); err != nil {
				return fmt.Errorf("required: %w", err)
			}
		case "type":
			if err := val.Decode(&in.Type); err != nil {
				return fmt.Errorf("type: %w", err)
			}
		case "default":
			if isNull(val) {
				continue
			}
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("default must be a scalar")
			}
			in.Default, in.HasDefault = val.Value, true
		}
	}
	return nil
}

// decodeCallSecrets decodes the declared secret names.
func decodeCallSecrets(node *yaml.Node) (map[string]CallSecret, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("on.workflow_call.secrets: expected a mapping of secret name to definition")
	}
	out := make(map[string]CallSecret, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !ValidSecretName(name) {
			return nil, fmt.Errorf("on.workflow_call.secrets: %q is not a valid secret name (names match [A-Za-z_][A-Za-z0-9_]*)", name)
		}
		var sec CallSecret
		if err := node.Content[i+1].Decode(&sec); err != nil {
			return nil, fmt.Errorf("on.workflow_call.secrets.%s: %w", name, err)
		}
		out[name] = sec
	}
	return out, nil
}

// UnmarshalYAML validates one secret declaration's keys.
func (s *CallSecret) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "description", "required"); err != nil {
		return err
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping (description/required)")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "description":
			if err := val.Decode(&s.Description); err != nil {
				return fmt.Errorf("description: %w", err)
			}
		case "required":
			if err := val.Decode(&s.Required); err != nil {
				return fmt.Errorf("required: %w", err)
			}
		}
	}
	return nil
}

// decodeCallOutputs decodes the declared outputs.
func decodeCallOutputs(node *yaml.Node) (map[string]CallOutput, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("on.workflow_call.outputs: expected a mapping of output name to definition")
	}
	out := make(map[string]CallOutput, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !ValidOutputName(name) {
			return nil, fmt.Errorf("on.workflow_call.outputs: invalid output name %q (letters, digits, _ and - only, at most %d bytes)", name, MaxOutputNameLen)
		}
		var co CallOutput
		if err := node.Content[i+1].Decode(&co); err != nil {
			return nil, fmt.Errorf("on.workflow_call.outputs.%s: %w", name, err)
		}
		if strings.TrimSpace(co.Value) == "" {
			return nil, fmt.Errorf("on.workflow_call.outputs.%s: value is required (a ${{ jobs.<id>.outputs.<name> }} reference)", name)
		}
		out[name] = co
	}
	return out, nil
}

// UnmarshalYAML validates one output definition's keys.
func (o *CallOutput) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "description", "value"); err != nil {
		return err
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping (description/value)")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "description":
			if err := val.Decode(&o.Description); err != nil {
				return fmt.Errorf("description: %w", err)
			}
		case "value":
			if err := val.Decode(&o.Value); err != nil {
				return fmt.Errorf("value: %w", err)
			}
		}
	}
	return nil
}

// validateCallInputValue checks one default or caller value against the
// declared type. GitHub's rule: booleans are true/false, numbers are decimal
// numbers, strings are anything.
func validateCallInputValue(name, typ, value string) error {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "boolean":
		if !isBooleanText(value) {
			return fmt.Errorf("input %q is a boolean, but %q is not true or false", name, value)
		}
	case "number":
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
			return fmt.Errorf("input %q is a number, but %q is not a number", name, value)
		}
	}
	return nil
}

// isBooleanText reports whether s is GitHub's boolean spelling.
func isBooleanText(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "false":
		return true
	}
	return false
}

// CallInputNames lists the declared workflow_call input names, sorted.
func (w *WorkflowCallTrigger) CallInputNames() []string {
	if w == nil {
		return nil
	}
	return sortedCallNames(w.Inputs)
}

// CallSecretNames lists the declared secret names, sorted (error messages).
func (w *WorkflowCallTrigger) CallSecretNames() []string {
	if w == nil {
		return nil
	}
	return sortedCallNames(w.Secrets)
}

// CallOutputNames lists the declared output names, sorted.
func (w *WorkflowCallTrigger) CallOutputNames() []string {
	if w == nil {
		return nil
	}
	return sortedCallNames(w.Outputs)
}

func sortedCallNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NormalizeCallInputs resolves one call's `with:` values against the callee's
// declarations: an unknown name, a missing required input (with no default) and
// a value that does not parse as the declared type are named errors. Every
// declared input is present in the result (its default when the caller passed
// nothing), so `inputs.*` never reads absent for a declared name.
func NormalizeCallInputs(decl map[string]CallInput, supplied map[string]string) (map[string]string, error) {
	names := sortedCallNames(supplied)
	for _, name := range names {
		if _, ok := decl[name]; !ok {
			return nil, fmt.Errorf("`%s` is not a declared `on.workflow_call` input (declared: %s)", name, listOrNone(sortedCallNames(decl)))
		}
	}
	out := make(map[string]string, len(decl))
	for name, in := range decl {
		if val, ok := supplied[name]; ok {
			typ := in.Type
			if typ == "" {
				typ = "string"
			}
			if err := validateCallInputValue(name, typ, val); err != nil {
				return nil, err
			}
			out[name] = val
			continue
		}
		if in.Required && !in.HasDefault {
			return nil, fmt.Errorf("required input %q was not supplied", name)
		}
		out[name] = in.Default
	}
	return out, nil
}

// ValidateCallOutputs closes every `on.workflow_call.outputs.<name>.value`
// template: it may reference only `jobs.<id>.outputs.<name>` where <id> is a
// job of THIS file that is not itself a reusable-workflow caller, and <name> is
// an output that job declares. A caller job's output is read through the call's
// own declared outputs, not through a `jobs.` reference — a nested call's job
// keys do not exist in this file's namespace.
func ValidateCallOutputs(w *Workflow) error {
	if w == nil || w.On.WorkflowCall == nil {
		return nil
	}
	names := w.On.WorkflowCall.CallOutputNames()
	for _, name := range names {
		value := w.On.WorkflowCall.Outputs[name].Value
		refs, err := CallOutputRefs(value)
		if err != nil {
			return fmt.Errorf("on.workflow_call.outputs.%s: %w", name, err)
		}
		if len(refs) == 0 {
			return fmt.Errorf("on.workflow_call.outputs.%s: value must read a job output (${{ jobs.<id>.outputs.<name> }})", name)
		}
		for _, ref := range refs {
			job, ok := w.Jobs[ref.Job]
			if !ok {
				return fmt.Errorf("on.workflow_call.outputs.%s: ${{ %s }} names no job of this file (jobs: %s)",
					name, ref.Raw, listOrNone(sortedMapKeys(w.Jobs)))
			}
			if strings.TrimSpace(job.Uses) != "" {
				return fmt.Errorf("on.workflow_call.outputs.%s: ${{ %s }} names a reusable-workflow caller; read the call's own outputs (needs.%s.outputs.*) instead",
					name, ref.Raw, ref.Job)
			}
			if _, ok := job.Outputs[ref.Output]; !ok {
				return fmt.Errorf("on.workflow_call.outputs.%s: job %s does not declare the output %q (declared: %s)",
					name, ref.Job, ref.Output, listOrNone(sortedMapKeys(job.Outputs)))
			}
		}
	}
	return nil
}

// callInputNames is the declared input-name set a template may read: the
// workflow_dispatch inputs and the workflow_call inputs (one file may declare
// both triggers).
func callInputNames(w *Workflow) []string {
	set := map[string]bool{}
	for _, name := range dispatchInputNames(w.On.WorkflowDispatch) {
		set[name] = true
	}
	for _, name := range w.On.WorkflowCall.CallInputNames() {
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// callSecretNames is the closed `secrets.*` set of a callee: when a file
// declares on.workflow_call it is a closed secret environment, so a secret it
// does not declare is a parse error naming the declaration (D-6: a caller can
// only hand over what the callee declares).
func callSecretNames(w *Workflow) []string {
	if w.On.WorkflowCall == nil {
		return nil
	}
	return w.On.WorkflowCall.CallSecretNames()
}

// JobSecrets is one caller job's `secrets:` block: either `inherit` or an
// explicit callee-name → `${{ secrets.SOURCE }}` map. A literal value is
// refused (it would put a secret in the workflow file, where the engine would
// treat it as public text).
type JobSecrets struct {
	// Inherit passes the repository's whole secret set (same repository only;
	// cross-repository inheritance is refused by D-6 at parse time).
	Inherit bool
	// Values maps the name the callee reads (`secrets.<callee name>`) to the
	// repository secret name it is taken from.
	Values map[string]string
}

// UnmarshalYAML accepts the two GitHub spellings.
func (s *JobSecrets) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if isNull(node) {
			return fmt.Errorf("secrets: expected `inherit` or a mapping of name: ${{ secrets.NAME }}")
		}
		if !strings.EqualFold(strings.TrimSpace(node.Value), "inherit") {
			return fmt.Errorf("secrets: %q is not supported (the scalar spelling is exactly `inherit`)", node.Value)
		}
		s.Inherit = true
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("secrets: expected `inherit` or a mapping of name: ${{ secrets.NAME }}")
	}
	values := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !ValidSecretName(name) {
			return fmt.Errorf("secrets: %q is not a valid secret name (names match [A-Za-z_][A-Za-z0-9_]*)", name)
		}
		raw := node.Content[i+1]
		if raw.Kind != yaml.ScalarNode || isNull(raw) {
			return fmt.Errorf("secrets.%s: the value must be ${{ secrets.<name> }}", name)
		}
		source, ok := wholeSecretRef(raw.Value)
		if !ok {
			return fmt.Errorf("secrets.%s: %q is not a secret reference; the value must be exactly ${{ secrets.<name> }} (a literal would put a secret in the workflow file)", name, raw.Value)
		}
		values[name] = source
	}
	s.Values = values
	return nil
}
