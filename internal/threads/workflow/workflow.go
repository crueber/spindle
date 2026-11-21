// Package workflow parses the `.spindle/workflows/*.yml` files that Threads
// runs.
//
// It is the one place `gopkg.in/yaml.v3` is allowed (AGENTS.md law 1, amended
// 2026-11-20): workflow files are YAML because that is the format CI authors
// already know, and hand-rolling a YAML subset is a failure mode, not a
// saving. Everything else in the dependency budget is unchanged.
//
// The parser is deliberately strict (yaml.Decoder.KnownFields): an unknown key
// is an error, not a silently ignored typo. Validation happens here, before a
// run record is created, so a workflow that cannot execute never enqueues a
// job.
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
}

// Triggers selects the events that start a run. A workflow with no triggers is
// valid but never fires; validation warns rather than fails so an in-progress
// file can be pushed.
type Triggers struct {
	Push             *PushTrigger  `yaml:"push"`
	PullRequest      *PRTrigger    `yaml:"pull_request"`
	WorkflowDispatch *EmptyTrigger `yaml:"workflow_dispatch"` // may be null; see DispatchSet
	Schedule         []CronEntry   `yaml:"schedule"`
	// DispatchSet records that the key was PRESENT even as null
	// (`workflow_dispatch:` with no value decodes to a nil pointer).
	DispatchSet bool `yaml:"-"`
	// PushSet/PullRequestSet are the same presence rule for the two
	// branch-filtered triggers: `push:` with no value is legal and means
	// "every branch", but it decodes to a nil pointer.
	PushSet        bool `yaml:"-"`
	PullRequestSet bool `yaml:"-"`
}

// PushTrigger may filter branches/tags by simple globs (`main`, `release/*`).
type PushTrigger struct {
	Branches []string `yaml:"branches"`
	Tags     []string `yaml:"tags"`
}

// PRTrigger may filter the target branches.
type PRTrigger struct {
	Branches []string `yaml:"branches"`
}

// EmptyTrigger accepts an empty mapping (`workflow_dispatch:`).
type EmptyTrigger struct{}

// Schedule is a list of cron entries (`schedule: - cron: "…"`), matching
// GitHub's spelling. The cron expression is validated by
// the caller against the scheduler's parser (the same one the mirror presets
// use); syntax validation lives there, not here.
type Schedule struct {
	Cron []CronEntry `yaml:"cron"`
}

// CronEntry is one schedule element. The list shape mirrors GitHub's
// `schedule: - cron: "0 3 * * *"`.
type CronEntry struct {
	Cron string `yaml:"cron"`
}

// Job is one unit of work. Steps run inside `Container` (or the runner's
// default image) on one workspace.
type Job struct {
	Name            string             `yaml:"name"`
	RunsOn          string             `yaml:"runs-on"`
	Container       string             `yaml:"container"`
	Services        map[string]Service `yaml:"services"`
	Env             map[string]string  `yaml:"env"`
	Steps           []Step             `yaml:"steps"`
	Needs           []string           `yaml:"needs"`
	If              string             `yaml:"if"`
	TimeoutMinutes  int                `yaml:"timeout-minutes"`
	ContinueOnError bool               `yaml:"continue-on-error"`
}

// Service is a sidecar container reachable by its key on the job network.
type Service struct {
	Image   string            `yaml:"image"`
	Env     map[string]string `yaml:"env"`
	Ports   []string          `yaml:"ports"`
	Command []string          `yaml:"command"`
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
	for name, job := range w.Jobs {
		if strings.TrimSpace(job.RunsOn) == "" {
			return nil, fmt.Errorf("%w: %s", ErrNoRunsOn, name)
		}
		if len(job.Steps) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNoSteps, name)
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
	// Presence scan: `workflow_dispatch:` with no value is legal and decodes
	// to a nil pointer, so the key's existence decides whether the trigger
	// exists. The same rule applies to `push:` / `pull_request:` (an empty
	// trigger means "every branch").
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(src, &top); err == nil {
		if on, ok := top["on"]; ok {
			var keys map[string]yaml.Node
			if err := on.Decode(&keys); err == nil {
				_, w.On.DispatchSet = keys["workflow_dispatch"]
				_, w.On.PushSet = keys["push"]
				_, w.On.PullRequestSet = keys["pull_request"]
			}
		}
	}
	return &w, nil
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
	default:
		return false
	}
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
