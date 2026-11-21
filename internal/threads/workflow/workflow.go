// Package workflow parses the `.spindle/workflows/*.yml` files that Threads
// runs.
//
// It is the one place `gopkg.in/yaml.v3` is allowed (AGENTS.md law 1, amended
// 2026-11-20): workflow files are YAML because that is the format CI authors
// already know, and hand-rolling a YAML subset is a failure mode, not a
// saving. Everything else in the dependency budget is unchanged.
//
// The parser is deliberately strict (yaml.Decoder.KnownFields): an unknown key
// is an error, not a silently ignored typo. The keys accepted are the ones the
// server and the runner actually consume (v1 subset, extended in the Threads
// pass A of 2026-11-21): on (string | list | mapping), workflow_dispatch.inputs,
// concurrency, permissions, strategy.matrix/fail-fast, list runs-on, mapped
// container, and job-level uses. Validation happens here, before a run record
// is created; the caller parses each file independently, so one bad file fails
// only its own workflow (15_threads.md §2.1).
//
// Fields that are accepted and carried but not yet enforced (permissions,
// matrix/fail-fast, concurrency) say so on their declaration: pass A records
// the spec so the runner (pass B) never reads the workflow file.
package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dir is the repository directory workflow files live in.
const Dir = ".spindle/workflows"

// Workflow is one parsed workflow file.
type Workflow struct {
	Name string            `yaml:"name"`
	On   Triggers          `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]Job    `yaml:"jobs"`
	// Concurrency and Permissions are workflow-level defaults; a job may
	// override either. Carried into the job spec; not enforced yet.
	Concurrency *Concurrency `yaml:"concurrency"`
	Permissions Permissions  `yaml:"permissions"`
}

// Triggers selects the events that start a run. A workflow with no triggers is
// valid but never fires; validation warns rather than fails so an in-progress
// file can be pushed.
//
// `on` accepts all three CI spellings: a scalar event name (`on: push`), a
// list (`on: [push, pull_request]`), and the mapping with per-event filters.
// UnmarshalYAML decodes them into the same struct and records key presence,
// because a bare `push:` / `pull_request:` / `workflow_dispatch:` key is a
// real trigger that decodes to a nil pointer.
type Triggers struct {
	Push             *PushTrigger     `yaml:"push"`
	PullRequest      *PRTrigger       `yaml:"pull_request"`
	WorkflowDispatch *DispatchTrigger `yaml:"workflow_dispatch"`
	Schedule         []CronEntry      `yaml:"schedule"`
	// DispatchSet records that the key was PRESENT even as null
	// (`workflow_dispatch:` with no value decodes to a nil pointer).
	DispatchSet bool `yaml:"-"`
	// PushSet/PullRequestSet are the same presence rule for the two
	// branch-filtered triggers: `push:` with no value is legal and means
	// "every branch", but it decodes to a nil pointer.
	PushSet        bool `yaml:"-"`
	PullRequestSet bool `yaml:"-"`
	// ScheduleSet records `schedule:` presence (the entries may be empty;
	// cron evaluation is the maintainer role's, not the parser's).
	ScheduleSet bool `yaml:"-"`
}

// UnmarshalYAML accepts the scalar, sequence and mapping forms of `on`. The
// event vocabulary is closed: an unknown event name is a parse error (a
// workflow that can never fire is a typo, not a feature).
//
// yaml.v3 does not propagate Decoder.KnownFields into a custom unmarshaler, so
// every mapping decoded here validates its own key set (checkKeys); the rest of
// the document keeps the decoder's strictness.
func (t *Triggers) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		return t.enable(node.Value)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("on: list entries must be event names")
			}
			if err := t.enable(item.Value); err != nil {
				return err
			}
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i].Value, node.Content[i+1]
			var err error
			switch key {
			case "push":
				t.PushSet = true
				if !isNull(val) {
					if err = checkKeys(val, "branches", "tags"); err == nil {
						var p PushTrigger
						if err = val.Decode(&p); err == nil {
							t.Push = &p
						}
					}
				}
			case "pull_request":
				t.PullRequestSet = true
				if !isNull(val) {
					if err = checkKeys(val, "branches", "types"); err == nil {
						var p PRTrigger
						if err = val.Decode(&p); err == nil {
							t.PullRequest = &p
						}
					}
				}
			case "workflow_dispatch":
				t.DispatchSet = true
				if !isNull(val) {
					if err = checkKeys(val, "inputs"); err == nil {
						var d DispatchTrigger
						if err = val.Decode(&d); err == nil {
							t.WorkflowDispatch = &d
						}
					}
				}
			case "schedule":
				t.ScheduleSet = true
				if err = val.Decode(&t.Schedule); err == nil {
					// The scheduler can only evaluate the documented subset; an
					// unsupported expression is THIS file's parse error (15 §1),
					// never a silently dead schedule.
					err = validateCronEntries(t.Schedule)
				}
			default:
				err = fmt.Errorf("on: unknown trigger %q", key)
			}
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("on: expected a string, a list, or a mapping")
	}
}

// enable marks one bare event name (the `on: push` / `on: [push]` forms) as
// present, with no filters.
func (t *Triggers) enable(name string) error {
	switch name {
	case "push":
		t.PushSet = true
	case "pull_request":
		t.PullRequestSet = true
	case "workflow_dispatch":
		t.DispatchSet = true
	case "schedule":
		// `on: schedule` without `- cron:` entries has nothing to evaluate;
		// the mapping form is the only legal spelling.
		return fmt.Errorf("on: schedule requires a list of `- cron: \"…\"` entries")
	default:
		return fmt.Errorf("on: unknown trigger %q", name)
	}
	return nil
}

// PushTrigger may filter branches/tags by simple globs (`main`, `release/*`).
type PushTrigger struct {
	Branches []string `yaml:"branches"`
	Tags     []string `yaml:"tags"`
}

// PRTrigger may filter the target branches.
type PRTrigger struct {
	Branches []string `yaml:"branches"`
	// Types is accepted and carried; v1 fires on opened/synchronize/reopened
	// only (15_threads.md §1) and does not filter by type yet.
	Types []string `yaml:"types"`
}

// DispatchTrigger is the manual trigger. Inputs are the typed
// workflow_dispatch inputs; the UI/API may pass values for them once the
// runner consumes them (pass B).
type DispatchTrigger struct {
	Inputs map[string]Input `yaml:"inputs"`
}

// Input is one workflow_dispatch input. v1 types: string (default), choice,
// boolean. `options` is required for choice; `default` is carried as the YAML
// scalar text (so a boolean default reads "true"/"false").
type Input struct {
	Description string   `yaml:"description"`
	Type        string   `yaml:"type"`
	Required    bool     `yaml:"required"`
	Default     *string  `yaml:"-"`
	Options     []string `yaml:"options"`
}

// UnmarshalYAML validates keys by hand (see Triggers.UnmarshalYAML) and keeps
// the default's scalar text without forcing a Go type.
func (in *Input) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "description", "type", "required", "default", "options"); err != nil {
		return err
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("workflow_dispatch input must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "description":
			if err := val.Decode(&in.Description); err != nil {
				return err
			}
		case "type":
			if err := val.Decode(&in.Type); err != nil {
				return err
			}
		case "required":
			if err := val.Decode(&in.Required); err != nil {
				return err
			}
		case "options":
			if err := val.Decode(&in.Options); err != nil {
				return err
			}
		case "default":
			if isNull(val) {
				continue
			}
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("workflow_dispatch input default must be a scalar")
			}
			def := val.Value
			in.Default = &def
		}
	}
	return nil
}

// Schedule is a list of cron entries (`schedule: - cron: "…"`), matching
// GitHub's spelling. The cron expression is validated by the maintainer's
// scheduler; syntax validation lives there, not here.
type Schedule struct {
	Cron []CronEntry `yaml:"cron"`
}

// CronEntry is one schedule element. The list shape mirrors GitHub's
// `schedule: - cron: "0 3 * * *"`.
type CronEntry struct {
	Cron string `yaml:"cron"`
}

// Concurrency is a concurrency group: scalar group name or the mapping with
// `cancel-in-progress`. Carried into the spec; the scheduler enforcement is
// pass B.
type Concurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

// UnmarshalYAML accepts `concurrency: ci-main` and the mapping form.
func (c *Concurrency) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		c.Group = node.Value
		return nil
	case yaml.MappingNode:
		if err := checkKeys(node, "group", "cancel-in-progress"); err != nil {
			return err
		}
		var raw struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		c.Group, c.CancelInProgress = raw.Group, raw.CancelInProgress
		return nil
	default:
		return fmt.Errorf("concurrency: expected a group name or a mapping")
	}
}

// Permissions is the GITHUB_TOKEN permission map. The scalar shorthand
// (`read-all` / `write-all` / `{}`) is normalized to a single "all" entry.
// Accepted and carried into the job spec; not enforced yet (there is no job
// token in pass A).
type Permissions map[string]string

// UnmarshalYAML accepts the scalar shorthand and the mapping form.
func (p *Permissions) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			*p = nil
			return nil
		}
		*p = Permissions{"all": node.Value}
		return nil
	case yaml.MappingNode:
		out := make(Permissions, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			val := node.Content[i+1]
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("permissions.%s must be a scalar", node.Content[i].Value)
			}
			out[node.Content[i].Value] = val.Value
		}
		*p = out
		return nil
	default:
		return fmt.Errorf("permissions: expected a mapping or read-all/write-all")
	}
}

// Labels is `runs-on`: one label or a list of labels.
type Labels []string

// UnmarshalYAML accepts both spellings.
func (l *Labels) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			*l = nil
			return nil
		}
		*l = Labels{node.Value}
		return nil
	case yaml.SequenceNode:
		out := make(Labels, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("runs-on labels must be scalars")
			}
			out = append(out, item.Value)
		}
		*l = out
		return nil
	default:
		return fmt.Errorf("runs-on: expected a label or a list of labels")
	}
}

// String renders the label set for the record's convenience field.
func (l Labels) String() string { return strings.Join(l, ", ") }

// Container is a job container: the scalar image shorthand or the mapping with
// env/ports/command/entrypoint/options/volumes. v1 carries the mapping; the
// executor is pass B.
type Container struct {
	Image      string            `yaml:"image"`
	Env        map[string]string `yaml:"env"`
	Ports      []string          `yaml:"ports"`
	Command    []string          `yaml:"command"`
	Entrypoint []string          `yaml:"entrypoint"`
	Options    string            `yaml:"options"`
	Volumes    []string          `yaml:"volumes"`
}

// UnmarshalYAML accepts `container: golang:1.27` and the mapping form.
func (c *Container) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		c.Image = node.Value
		return nil
	case yaml.MappingNode:
		if err := checkKeys(node, "image", "env", "ports", "command", "entrypoint", "options", "volumes"); err != nil {
			return err
		}
		type raw Container
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		*c = Container(r)
		return nil
	default:
		return fmt.Errorf("container: expected an image or a mapping")
	}
}

// Matrix is `strategy.matrix`: a simple product of scalar axes. include/
// exclude and nested objects are the documented v1 limit (a parse error).
type Matrix map[string][]string

// UnmarshalYAML decodes the axis → list mapping.
func (m *Matrix) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("strategy.matrix must be a mapping of axis to list")
	}
	out := make(Matrix, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		axis, val := node.Content[i].Value, node.Content[i+1]
		switch axis {
		case "include", "exclude":
			return fmt.Errorf("strategy.matrix.%s is not supported (v1 is a simple product)", axis)
		}
		list, err := scalarList(val)
		if err != nil {
			return fmt.Errorf("strategy.matrix.%s: %w", axis, err)
		}
		out[axis] = list
	}
	*m = out
	return nil
}

// Strategy is the job's matrix. FailFast is carried; expansion and the
// fail-fast policy are the scheduler's (pass B).
type Strategy struct {
	Matrix   Matrix `yaml:"matrix"`
	FailFast *bool  `yaml:"fail-fast"`
}

// Job is one unit of work. Steps run inside `Container` (or the runner's
// default image) on one workspace. A job-level `uses` names a reusable
// workflow instead of steps; it is accepted and carried, execution is pass B/C.
type Job struct {
	Name            string             `yaml:"name"`
	RunsOn          Labels             `yaml:"runs-on"`
	Container       *Container         `yaml:"container"`
	Services        map[string]Service `yaml:"services"`
	Env             map[string]string  `yaml:"env"`
	Steps           []Step             `yaml:"steps"`
	Needs           []string           `yaml:"needs"`
	If              string             `yaml:"if"`
	TimeoutMinutes  int                `yaml:"timeout-minutes"`
	ContinueOnError bool               `yaml:"continue-on-error"`
	// Reusable workflow (accepted and carried; no runs-on/steps required).
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	// Strategy/Concurrency/Permissions are carried into the spec.
	Strategy    *Strategy    `yaml:"strategy"`
	Concurrency *Concurrency `yaml:"concurrency"`
	Permissions Permissions  `yaml:"permissions"`
}

// Service is a sidecar container reachable by its key on the job network.
type Service struct {
	Image      string            `yaml:"image"`
	Env        map[string]string `yaml:"env"`
	Ports      []string          `yaml:"ports"`
	Command    []string          `yaml:"command"`
	Entrypoint []string          `yaml:"entrypoint"`
	Options    string            `yaml:"options"`
	Volumes    []string          `yaml:"volumes"`
}

// Step is one command or built-in action.
type Step struct {
	Name             string            `yaml:"name"`
	Run              string            `yaml:"run"`
	Uses             string            `yaml:"uses"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	If               string            `yaml:"if"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	ContinueOnError  bool              `yaml:"continue-on-error"`
	TimeoutMinutes   int               `yaml:"timeout-minutes"`
}

// Errors returned by Parse. They are values so tests and the UI can match.
var (
	ErrEmpty      = errors.New("workflow: empty file")
	ErrNoJobs     = errors.New("workflow: no jobs")
	ErrNoSteps    = errors.New("workflow: job has no steps")
	ErrNoRunsOn   = errors.New("workflow: job has no runs-on")
	ErrBothStep   = errors.New("workflow: step has both run and uses")
	ErrNoStepVerb = errors.New("workflow: step has neither run nor uses")
	ErrBadNeeds   = errors.New("workflow: needs names an unknown job")
	ErrCycle      = errors.New("workflow: job dependencies form a cycle")
)

// Parse decodes and validates one workflow file.
func Parse(src []byte) (*Workflow, error) {
	trimmed := strings.TrimSpace(string(src))
	if trimmed == "" || trimmed == "---" {
		return nil, ErrEmpty
	}
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	var w Workflow
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}
	// An empty second document must not hide trailing content; a second
	// non-empty document is ambiguous in one file.
	var extra Workflow
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("workflow: more than one document")
	}
	if len(w.Jobs) == 0 {
		return nil, ErrNoJobs
	}
	if err := validateDispatch(w.On.WorkflowDispatch); err != nil {
		return nil, err
	}
	for name, job := range w.Jobs {
		reusable := strings.TrimSpace(job.Uses) != ""
		if !reusable {
			if len(job.RunsOn) == 0 {
				return nil, fmt.Errorf("%w: %s", ErrNoRunsOn, name)
			}
			if len(job.Steps) == 0 {
				return nil, fmt.Errorf("%w: %s", ErrNoSteps, name)
			}
		}
		for i, step := range job.Steps {
			if step.Run != "" && step.Uses != "" {
				return nil, fmt.Errorf("%w: %s step %d", ErrBothStep, name, i+1)
			}
			if step.Run == "" && step.Uses == "" {
				return nil, fmt.Errorf("%w: %s step %d", ErrNoStepVerb, name, i+1)
			}
		}
		for _, need := range job.Needs {
			if _, ok := w.Jobs[need]; !ok {
				return nil, fmt.Errorf("%w: %s needs %s", ErrBadNeeds, name, need)
			}
		}
	}
	if err := checkAcyclic(w.Jobs); err != nil {
		return nil, err
	}
	return &w, nil
}

// validateDispatch checks the typed workflow_dispatch inputs (v1 types:
// string/choice/boolean; choice needs options).
func validateDispatch(d *DispatchTrigger) error {
	if d == nil {
		return nil
	}
	for name, in := range d.Inputs {
		switch in.Type {
		case "", "string", "boolean":
		case "choice":
			if len(in.Options) == 0 {
				return fmt.Errorf("workflow: workflow_dispatch input %q is a choice with no options", name)
			}
		default:
			return fmt.Errorf("workflow: workflow_dispatch input %q has unsupported type %q", name, in.Type)
		}
	}
	return nil
}

// ParseFile reads and parses one workflow file.
func ParseFile(path string) (*Workflow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Files lists the workflow files under a repository checkout root, sorted by
// name. It is used by the importer and by tests; the server reads workflows
// from git, not from disk.
func Files(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, Dir, "*.yml"))
	if err != nil {
		return nil, err
	}
	more, err := filepath.Glob(filepath.Join(root, Dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	out := append(matches, more...)
	sort.Strings(out)
	return out, nil
}

// Fires reports whether the workflow triggers on an event. event is one of
// "push", "pull_request", "workflow_dispatch"; schedule is handled by the
// scheduler. branch may be empty (no filter applies).
func (w *Workflow) Fires(event, branch string) bool {
	switch event {
	case "push":
		if w.On.Push == nil && !w.On.PushSet {
			return false
		}
		if w.On.Push == nil {
			return true // `push:` with no filters: every branch
		}
		return matchAny(w.On.Push.Branches, branch)
	case "pull_request":
		if w.On.PullRequest == nil && !w.On.PullRequestSet {
			return false
		}
		if w.On.PullRequest == nil {
			return true // `pull_request:` with no filters: every target branch
		}
		return matchAny(w.On.PullRequest.Branches, branch)
	case "workflow_dispatch":
		return w.On.DispatchSet
	case "schedule":
		// The maintainer's scheduler selects files itself (it needs the cron
		// entries, not just a yes/no), but Fires must answer honestly so a
		// caller can ask "does this file schedule?".
		return w.On.ScheduleSet
	default:
		return false
	}
}

// Events lists the trigger names the workflow declares, in a stable display
// order (push, pull_request, workflow_dispatch, schedule). A bare `on: push`
// and `on: [push]` report the same set as the mapping form.
func (w *Workflow) Events() []string {
	var out []string
	if w.On.Push != nil || w.On.PushSet {
		out = append(out, "push")
	}
	if w.On.PullRequest != nil || w.On.PullRequestSet {
		out = append(out, "pull_request")
	}
	if w.On.DispatchSet {
		out = append(out, "workflow_dispatch")
	}
	if w.On.ScheduleSet {
		out = append(out, "schedule")
	}
	return out
}

// matchAny implements the simple glob set CI authors expect: exact names and
// one-or-more `*` wildcards. An empty filter matches everything.
func matchAny(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// checkAcyclic rejects a job graph whose `needs` form a cycle (a run that can
// never start).
func checkAcyclic(jobs map[string]Job) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	state := make(map[string]int, len(jobs))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case grey:
			return fmt.Errorf("%w: %s", ErrCycle, name)
		case black:
			return nil
		}
		state[name] = grey
		for _, need := range jobs[name].Needs {
			if err := visit(need); err != nil {
				return err
			}
		}
		state[name] = black
		return nil
	}
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// --- node helpers ----------------------------------------------------------

// isNull reports a YAML null scalar (`key:` with no value).
func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

// checkKeys rejects keys outside the allowed set on a mapping node. Non-mapping
// nodes pass (the following Decode reports the shape error).
func checkKeys(node *yaml.Node, allowed ...string) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	ok := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		ok[k] = true
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !ok[key] {
			return fmt.Errorf("unknown key %q", key)
		}
	}
	return nil
}

// scalarList decodes a scalar or a sequence of scalars (matrix axis values).
func scalarList(node *yaml.Node) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return []string{}, nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("values must be scalars")
			}
			out = append(out, item.Value)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a scalar or a list of scalars")
	}
}
