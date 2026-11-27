package workflow

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repository-local action metadata (docs/threads.md §3.4a). A workflow step may
// `uses: ./.spindle/actions/<name>`; the file at that directory's
// action.yml/action.yaml is read from the run commit and parsed here. The parser
// is deliberately strict, like the workflow parser: an unknown key, a missing
// `using:`, a composite run step without `shell:`, or a Dockerfile reference is
// a named error on the JOB (the action is resolved at run creation, before any
// runner or container), never a silently ignored typo.
//
// The accepted metadata is the GitHub action shape:
//
//	name/description/author/branding
//	inputs:  <name>: {description, required, default}
//	outputs: <name>: {description, value}
//	runs:
//	  using: composite
//	  steps: [...]                 # run (with shell) | uses: ./… | uses: docker://…
//	# or
//	  using: docker
//	  image: docker://image[:tag|@digest]
//	  entrypoint: string | [string, …]
//	  args: [string, …]
//	  env: {name: value}
//
// `author` and `branding` are accepted and dropped (they are display metadata
// on the GitHub marketplace, which spindle does not have).
const (
	// ActionUsingComposite is an action whose runs.steps expand into the
	// caller's step list.
	ActionUsingComposite = "composite"
	// ActionUsingDocker is an action that runs its image as a container.
	ActionUsingDocker = "docker"
	// MaxActionDepth bounds nested composite actions (the top-level `uses:` is
	// depth 1). A deeper chain is refused with a named reason: a payload that
	// recursed forever must never reach a runner.
	MaxActionDepth = 10
	// ActionFileYML and ActionFileYAML are the accepted metadata file names,
	// in lookup order.
	ActionFileYML  = "action.yml"
	ActionFileYAML = "action.yaml"
)

// Action is one parsed repository-local action.
type Action struct {
	Name        string
	Description string
	Inputs      map[string]ActionInput
	Outputs     map[string]ActionOutput
	Runs        ActionRuns
}

// ActionInput is one declared input. Default is kept as the YAML scalar text so
// an unquoted `default: true` reads "true" (GitHub's rule: defaults are strings
// in the action's environment).
type ActionInput struct {
	Description string
	Required    bool
	Default     string
	HasDefault  bool
}

// ActionOutput is one declared output: a display description and the template
// evaluated over the action's own step outputs (`${{ steps.<id>.outputs.<x> }}`).
type ActionOutput struct {
	Description string
	Value       string
}

// ActionRuns is the action's execution body: composite steps or a Docker image.
type ActionRuns struct {
	Using      string
	Steps      []Step            // composite only
	Image      string            // docker only, with the docker:// prefix stripped
	Entrypoint []string          // docker only
	Args       []string          // docker only
	Env        map[string]string // docker only
}

// IsComposite reports whether the action expands into steps.
func (a *Action) IsComposite() bool {
	return a != nil && a.Runs.Using == ActionUsingComposite
}

// ParseAction decodes and validates one action metadata file.
func ParseAction(src []byte) (*Action, error) {
	trimmed := strings.TrimSpace(string(src))
	if trimmed == "" || trimmed == "---" {
		return nil, fmt.Errorf("action: empty file")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	var a Action
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("action: %w", err)
	}
	var extra Action
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("action: more than one document")
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// UnmarshalYAML validates the metadata's key set by hand (yaml.v3 does not
// propagate KnownFields into a custom unmarshaler) and decodes the body.
func (a *Action) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "name", "description", "author", "branding", "inputs", "outputs", "runs"); err != nil {
		return fmt.Errorf("action: %w", err)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action: expected a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "name":
			if err := val.Decode(&a.Name); err != nil {
				return fmt.Errorf("action.name: %w", err)
			}
		case "description":
			if err := val.Decode(&a.Description); err != nil {
				return fmt.Errorf("action.description: %w", err)
			}
		case "author", "branding":
			// Display metadata on the GitHub marketplace; accepted and dropped.
		case "inputs":
			if err := decodeActionInputs(val, &a.Inputs); err != nil {
				return err
			}
		case "outputs":
			if err := decodeActionOutputs(val, &a.Outputs); err != nil {
				return err
			}
		case "runs":
			if err := val.Decode(&a.Runs); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeActionInputs decodes the inputs map strictly (each entry's key set is
// checked by ActionInput.UnmarshalYAML).
func decodeActionInputs(node *yaml.Node, out *map[string]ActionInput) error {
	if isNull(node) {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action.inputs: expected a mapping of input name to definition")
	}
	inputs := make(map[string]ActionInput, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if err := validateActionInputName(name); err != nil {
			return fmt.Errorf("action.inputs: %w", err)
		}
		var in ActionInput
		if err := node.Content[i+1].Decode(&in); err != nil {
			return fmt.Errorf("action.inputs.%s: %w", name, err)
		}
		inputs[name] = in
	}
	*out = inputs
	return nil
}

// UnmarshalYAML validates one input definition's keys.
func (in *ActionInput) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "description", "required", "default"); err != nil {
		return err
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping (description/required/default)")
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

// decodeActionOutputs decodes the outputs map strictly.
func decodeActionOutputs(node *yaml.Node, out *map[string]ActionOutput) error {
	if isNull(node) {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action.outputs: expected a mapping of output name to definition")
	}
	outputs := make(map[string]ActionOutput, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !ValidOutputName(name) {
			return fmt.Errorf("action.outputs: invalid output name %q (letters, digits, _ and - only, at most %d bytes)", name, MaxOutputNameLen)
		}
		var o ActionOutput
		if err := node.Content[i+1].Decode(&o); err != nil {
			return fmt.Errorf("action.outputs.%s: %w", name, err)
		}
		outputs[name] = o
	}
	*out = outputs
	return nil
}

// UnmarshalYAML validates one output definition's keys.
func (o *ActionOutput) UnmarshalYAML(node *yaml.Node) error {
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

// UnmarshalYAML decodes one runs block. `using` decides which keys are legal,
// so a composite action's `image:` is an error naming the key rather than a
// silently ignored field, and a missing `using` is the first thing reported.
func (r *ActionRuns) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action.runs: expected a mapping")
	}
	var using string
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "using" {
			if err := node.Content[i+1].Decode(&using); err != nil {
				return fmt.Errorf("action.runs.using: %w", err)
			}
		}
	}
	using = strings.ToLower(strings.TrimSpace(using))
	switch using {
	case "":
		return fmt.Errorf("action.runs: using is required (composite or docker)")
	case ActionUsingComposite:
		if err := checkKeys(node, "using", "steps"); err != nil {
			return fmt.Errorf("action.runs: %w (a composite action has using and steps)", err)
		}
	case ActionUsingDocker:
		if err := checkKeys(node, "using", "image", "entrypoint", "args", "env"); err != nil {
			return fmt.Errorf("action.runs: %w (a docker action has using, image, entrypoint, args and env)", err)
		}
	default:
		return fmt.Errorf("action.runs.using: %q is not supported (spindle runs composite and docker actions)", using)
	}
	r.Using = using
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "using":
			// Already read.
		case "steps":
			if err := decodeActionSteps(val, &r.Steps); err != nil {
				return err
			}
		case "image":
			if err := val.Decode(&r.Image); err != nil {
				return fmt.Errorf("action.runs.image: %w", err)
			}
		case "entrypoint":
			if err := decodeStringOrList(val, &r.Entrypoint); err != nil {
				return fmt.Errorf("action.runs.entrypoint: %w", err)
			}
		case "args":
			if err := decodeStringOrList(val, &r.Args); err != nil {
				return fmt.Errorf("action.runs.args: %w", err)
			}
		case "env":
			if err := val.Decode(&r.Env); err != nil {
				return fmt.Errorf("action.runs.env: %w", err)
			}
		}
	}
	return nil
}

// decodeActionSteps decodes a composite action's steps strictly.
func decodeActionSteps(node *yaml.Node, out *[]Step) error {
	if isNull(node) {
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("action.runs.steps: expected a list")
	}
	steps := make([]Step, 0, len(node.Content))
	for i, item := range node.Content {
		if err := checkKeys(item, "id", "name", "run", "uses", "with", "env", "if", "shell",
			"working-directory", "continue-on-error", "timeout-minutes",
			"background", "wait", "wait-all", "cancel", "parallel"); err != nil {
			return fmt.Errorf("action.runs.steps[%d]: %w", i, err)
		}
		var st Step
		if err := item.Decode(&st); err != nil {
			return fmt.Errorf("action.runs.steps[%d]: %w", i, err)
		}
		steps = append(steps, st)
	}
	*out = steps
	return nil
}

// decodeStringOrList accepts GitHub's scalar-or-list spelling (entrypoint).
func decodeStringOrList(node *yaml.Node, out *[]string) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		*out = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		list := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("entries must be strings")
			}
			list = append(list, item.Value)
		}
		*out = list
		return nil
	default:
		return fmt.Errorf("expected a string or a list of strings")
	}
}

// validate applies the rules that need the whole action (not one node).
func (a *Action) validate() error {
	switch a.Runs.Using {
	case ActionUsingComposite:
		return a.validateComposite()
	case ActionUsingDocker:
		return a.validateDocker()
	default:
		return fmt.Errorf("action.runs: using is required (composite or docker)")
	}
}

// validateComposite checks the composite body and its step shapes.
func (a *Action) validateComposite() error {
	if len(a.Runs.Steps) == 0 {
		return fmt.Errorf("action.runs.steps: a composite action needs at least one step")
	}
	declared := make([]string, 0, len(a.Runs.Steps))
	for i, st := range a.Runs.Steps {
		where := fmt.Sprintf("action.runs.steps[%d]", i+1)
		if st.Background || stepControl(st) || st.Parallel != nil {
			return fmt.Errorf("%s: background, wait/cancel and parallel steps are not accepted inside a composite action", where)
		}
		run, uses := strings.TrimSpace(st.Run), strings.TrimSpace(st.Uses)
		switch {
		case run != "" && uses != "":
			return fmt.Errorf("%s: a step has both run and uses", where)
		case run == "" && uses == "":
			return fmt.Errorf("%s: a step needs run or uses", where)
		case run != "":
			if strings.TrimSpace(st.Shell) == "" {
				return fmt.Errorf("%s: a run step in a composite action must name its shell (shell: bash|sh|python)", where)
			}
			if _, err := shellName(st.Shell); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		default:
			if err := validateCompositeUses(uses); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		if st.ID != "" {
			declared = append(declared, strings.ToLower(st.ID))
		}
	}
	// Output templates read the action's own steps. A reference to a step that
	// does not exist would resolve to nothing at run time, so it is this file's
	// error, named with the step it names (fail closed).
	names := make([]string, 0, len(a.Outputs))
	for name := range a.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := a.Outputs[name].Value
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("action.outputs.%s: value is required (a ${{ steps.<id>.outputs.<x> }} reference)", name)
		}
		refs, err := TemplateRefs(value)
		if err != nil {
			return fmt.Errorf("action.outputs.%s: %w", name, err)
		}
		for _, ref := range refs {
			if ref.Context != "steps" {
				return fmt.Errorf("action.outputs.%s: ${{ %s }} is not supported (an output reads steps.<id>.outputs.<name>)", name, ref.Raw)
			}
			if !containsFold(declared, ref.Step) {
				return fmt.Errorf("action.outputs.%s: no step with id %q in this action (declared: %s)", name, ref.Step, listOrNone(declared))
			}
		}
	}
	return a.validateCompositeTemplates()
}

// validateCompositeTemplates closes every substituted string of a composite
// action's steps with the action's own scope: its declared inputs, the step ids
// declared before the site, and `github.action_path` (PAR-074). Action metadata
// is parsed before a run exists, so an unsupported reference is THIS file's
// error rather than an empty string at execution time. A workflow step, a job
// `if:` and an output template cannot read `action_path`; only these steps can,
// and each composite level supplies its own value (the runner's contract).
func (a *Action) validateCompositeTemplates() error {
	declared := make([]string, 0, len(a.Runs.Steps))
	for i, st := range a.Runs.Steps {
		where := fmt.Sprintf("action.runs.steps[%d]", i+1)
		scope := TemplateScope{Inputs: a.DeclaredInputs(), Steps: declared, ActionPath: true}
		type field struct{ name, value string }
		fields := []field{
			{where + " name", st.Name},
			{where + " run", st.Run},
			{where + " shell", st.Shell},
			{where + " working-directory", st.WorkingDirectory},
		}
		envKeys := make([]string, 0, len(st.Env))
		for k := range st.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			fields = append(fields, field{where + " env." + k, st.Env[k]})
		}
		withKeys := make([]string, 0, len(st.With))
		for k := range st.With {
			withKeys = append(withKeys, k)
		}
		sort.Strings(withKeys)
		for _, k := range withKeys {
			fields = append(fields, field{where + " with." + k, st.With[k]})
		}
		for _, f := range fields {
			if err := ValidateTemplate(f.value, scope); err != nil {
				return fmt.Errorf("%s: %w", f.name, err)
			}
		}
		if err := ValidateIf(st.If, scope); err != nil {
			return fmt.Errorf("%s if: %w", where, err)
		}
		if st.ID != "" {
			declared = append(declared, strings.ToLower(st.ID))
		}
	}
	return nil
}

// validateCompositeUses closes the `uses:` vocabulary of a composite step:
// a repository-local action or a docker:// image. Built-in and marketplace
// actions are refused here, before any run exists.
func validateCompositeUses(uses string) error {
	switch {
	case strings.HasPrefix(uses, "./"):
		return nil
	case strings.HasPrefix(strings.ToLower(uses), "docker://"):
		if strings.TrimSpace(uses[len("docker://"):]) == "" {
			return fmt.Errorf("uses: docker:// needs an image reference")
		}
		return nil
	case strings.Contains(uses, "@") || strings.Contains(uses, "/"):
		return fmt.Errorf("uses: %q is not available inside a composite action (allowed: ./.spindle/… for a local action, docker://image for an image)", uses)
	default:
		return fmt.Errorf("uses: %q is not a supported action reference (allowed: ./.spindle/… for a local action, docker://image for an image)", uses)
	}
}

// validateDocker checks a docker action's image spelling.
func (a *Action) validateDocker() error {
	if len(a.Outputs) > 0 {
		return fmt.Errorf("action.outputs: a docker action cannot declare outputs (only a composite action has steps to read them from)")
	}
	image := strings.TrimSpace(a.Runs.Image)
	if image == "" {
		return fmt.Errorf("action.runs.image: a docker action needs image: docker://<image>[:tag|@digest]")
	}
	if !strings.HasPrefix(strings.ToLower(image), "docker://") {
		return fmt.Errorf("action.runs.image: %q must be a docker:// reference; building a Dockerfile is not supported (push the image to a registry instead)", image)
	}
	image = strings.TrimSpace(image[len("docker://"):])
	if image == "" {
		return fmt.Errorf("action.runs.image: docker:// needs an image reference")
	}
	a.Runs.Image = image
	return nil
}

// validateActionInputName rejects an input name that cannot become an
// INPUT_<NAME> environment variable.
func validateActionInputName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("an input name must not be empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("input name %q is longer than 128 bytes", name)
	}
	for _, r := range name {
		if r == '=' || r == 0 || r == '\n' || r == '\r' {
			return fmt.Errorf("input name %q must not contain '=' or a newline", name)
		}
	}
	return nil
}

// shellName validates a composite step's shell and returns its interpreter
// (shared with the runner's shell resolution through SupportedShell).
func shellName(shell string) (string, error) {
	name := strings.TrimSpace(shell)
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i]
	}
	switch strings.ToLower(name) {
	case "", "sh":
		return "sh", nil
	case "bash":
		return "bash", nil
	case "python", "python3":
		return "python3", nil
	case "pwsh", "powershell":
		return "pwsh", nil
	case "cmd":
		return "cmd", nil
	default:
		return "", fmt.Errorf("unsupported shell %q (v1: sh, bash, python, pwsh, cmd)", shell)
	}
}

// InputEnvName is the environment variable GitHub gives one action input:
// uppercase, spaces replaced with underscores (`fetch-depth` stays
// `INPUT_FETCH-DEPTH`, exactly as GitHub spells it).
func InputEnvName(name string) string {
	return "INPUT_" + strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(name)), " ", "_")
}

// InputEnv is the INPUT_<NAME> environment of one call: every declared input
// (its default when the caller supplied nothing) plus every caller `with:`
// value. `with` arrives already validated against the declarations.
func (a *Action) InputEnv(with map[string]string) map[string]string {
	env := make(map[string]string, len(a.Inputs)+len(with))
	names := make([]string, 0, len(a.Inputs))
	for name := range a.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env[InputEnvName(name)] = a.Inputs[name].Default
	}
	for name, val := range with {
		env[InputEnvName(name)] = val
	}
	return env
}

// UndeclaredInputs lists a call's `with:` keys the action does not declare,
// sorted. A typo in an input name is refused (the value would silently do
// nothing), which is the same rule the built-in actions' `with:` follows.
func (a *Action) UndeclaredInputs(with map[string]string) []string {
	var out []string
	for name := range with {
		if _, ok := a.Inputs[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// DeclaredInputs lists the action's input names, sorted (error messages).
func (a *Action) DeclaredInputs() []string {
	out := make([]string, 0, len(a.Inputs))
	for name := range a.Inputs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
