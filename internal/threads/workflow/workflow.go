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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	// override either. Permissions drive the job token's authority (PAR-112).
	Concurrency *Concurrency `yaml:"concurrency"`
	Permissions Permissions  `yaml:"permissions"`
	// EnableOpenIDConnect is the Forgejo spelling for a job that may exchange
	// an OIDC ID token (PAR-119): `enable-openid-connect: true` beside
	// `permissions: id-token: write`. It is a permission, not a feature
	// toggle: a fork run never gets a token whatever it says, and an explicit
	// `id-token: none` wins over it.
	EnableOpenIDConnect bool `yaml:"enable-openid-connect"`
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
	// Release, Issues and IssueComment are the event services' triggers
	// (PAR-017, PAR-020): each names the lifecycle actions its owning service
	// actually emits, and an unemittable type is a parse error.
	Release      *TypeTrigger `yaml:"release"`
	Issues       *TypeTrigger `yaml:"issues"`
	IssueComment *TypeTrigger `yaml:"issue_comment"`
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
	// ReleaseSet/IssuesSet/IssueCommentSet are the same presence rule for the
	// three typed triggers.
	ReleaseSet      bool `yaml:"-"`
	IssuesSet       bool `yaml:"-"`
	IssueCommentSet bool `yaml:"-"`
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
					if err = checkKeys(val, "branches", "branches-ignore", "tags", "tags-ignore", "paths", "paths-ignore"); err == nil {
						var p PushTrigger
						if err = val.Decode(&p); err == nil {
							err = p.validate()
						}
						if err == nil {
							t.Push = &p
						}
					}
				}
			case "pull_request":
				t.PullRequestSet = true
				if !isNull(val) {
					if err = checkKeys(val, "branches", "branches-ignore", "types", "paths", "paths-ignore"); err == nil {
						var p PRTrigger
						if err = val.Decode(&p); err == nil {
							err = p.validate()
						}
						if err == nil {
							t.PullRequest = &p
						}
					}
				}
			case "release":
				t.ReleaseSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("release", r.Types, ReleaseTypes)
						}
						if err == nil {
							t.Release = &r
						}
					}
				}
			case "issues":
				t.IssuesSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("issues", r.Types, IssuesTypes)
						}
						if err == nil {
							t.Issues = &r
						}
					}
				}
			case "issue_comment":
				t.IssueCommentSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("issue_comment", r.Types, IssueCommentTypes)
						}
						if err == nil {
							t.IssueComment = &r
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
	case "release":
		t.ReleaseSet = true
	case "issues":
		t.IssuesSet = true
	case "issue_comment":
		t.IssueCommentSet = true
	default:
		return fmt.Errorf("on: unknown trigger %q", name)
	}
	return nil
}

// PushTrigger filters branch/tag pushes and the changed paths that fire them.
// The `*-ignore` spellings are exclusion lists; a trigger may not name both
// forms of the same dimension (the include form with `!` is the way to mix).
type PushTrigger struct {
	Branches       []string `yaml:"branches"`
	BranchesIgnore []string `yaml:"branches-ignore"`
	Tags           []string `yaml:"tags"`
	TagsIgnore     []string `yaml:"tags-ignore"`
	Paths          []string `yaml:"paths"`
	PathsIgnore    []string `yaml:"paths-ignore"`
}

// PRTrigger may filter the target branches, the changed paths and the
// lifecycle actions.
type PRTrigger struct {
	Branches       []string `yaml:"branches"`
	BranchesIgnore []string `yaml:"branches-ignore"`
	Paths          []string `yaml:"paths"`
	PathsIgnore    []string `yaml:"paths-ignore"`
	// Types filters the pull_request lifecycle actions. The vocabulary is
	// closed: only the actions the pulls service actually emits can be named
	// (PullRequestTypes). An absent or empty filter fires on every emitted
	// action; a named filter fires only on the listed ones, so an action the
	// service never emits must be a parse error rather than a workflow that
	// can never run (15_threads.md §3.2, §6.2).
	Types []string `yaml:"types"`
}

// TypeTrigger is one typed event trigger (`release`, `issues`,
// `issue_comment`). An absent/empty `types` fires on every action the owning
// service emits.
type TypeTrigger struct {
	Types []string `yaml:"types"`
}

// PullRequestTypes is the closed set of pull_request lifecycle actions Threads
// emits. The pulls service emits exactly these through its PullEvents seam
// (internal/pulls/pulls.go PullEvent) plus the PR metadata edges: opened,
// synchronize (an applied new head commit), reopened, closed, edited,
// ready_for_review, converted_to_draft, labeled, unlabeled, assigned,
// unassigned, milestoned, demilestoned.
var PullRequestTypes = []string{
	"opened", "synchronize", "reopened", "closed", "edited",
	"ready_for_review", "converted_to_draft",
	"labeled", "unlabeled", "assigned", "unassigned",
	"milestoned", "demilestoned",
}

// ReleaseTypes is the closed set of release lifecycle actions the releases
// service emits: created (a draft), published, edited, deleted, prereleased,
// released (the prerelease flag cleared) and unpublished (published → draft).
var ReleaseTypes = []string{"created", "published", "edited", "deleted", "prereleased", "released", "unpublished"}

// IssuesTypes is the closed set of issue lifecycle actions the issues service
// emits.
var IssuesTypes = []string{"opened", "edited", "closed", "reopened", "labeled", "unlabeled", "assigned", "unassigned", "milestoned", "demilestoned"}

// IssueCommentTypes is the closed set of issue-comment actions the issues
// service emits. Comment editing and deletion are not implemented, so those
// unemittable actions stay parse errors.
var IssueCommentTypes = []string{"created"}

// validateFilters closes the two filter lists that cannot coexist and compiles
// every pattern (a malformed pattern is the file's parse error).
func (p *PushTrigger) validate() error {
	if len(p.Branches) > 0 && len(p.BranchesIgnore) > 0 {
		return fmt.Errorf("push: `branches` and `branches-ignore` cannot be used together (use `branches` with a `!` pattern to exclude)")
	}
	if len(p.Tags) > 0 && len(p.TagsIgnore) > 0 {
		return fmt.Errorf("push: `tags` and `tags-ignore` cannot be used together (use `tags` with a `!` pattern to exclude)")
	}
	if len(p.Paths) > 0 && len(p.PathsIgnore) > 0 {
		return fmt.Errorf("push: `paths` and `paths-ignore` cannot be used together (use `paths` with a `!` pattern to exclude)")
	}
	for _, list := range [][]string{p.Branches, p.Paths} {
		if _, err := compileFilter(list, false); err != nil {
			return fmt.Errorf("push: %v", err)
		}
	}
	for _, list := range [][]string{p.BranchesIgnore, p.TagsIgnore, p.PathsIgnore} {
		if _, err := compileFilter(list, true); err != nil {
			return fmt.Errorf("push: %v", err)
		}
	}
	if _, err := compileFilter(p.Tags, false); err != nil {
		return fmt.Errorf("push: %v", err)
	}
	return nil
}

// validate closes the PR trigger's filter lists and the types vocabulary.
func (p *PRTrigger) validate() error {
	if len(p.Branches) > 0 && len(p.BranchesIgnore) > 0 {
		return fmt.Errorf("pull_request: `branches` and `branches-ignore` cannot be used together (use `branches` with a `!` pattern to exclude)")
	}
	if len(p.Paths) > 0 && len(p.PathsIgnore) > 0 {
		return fmt.Errorf("pull_request: `paths` and `paths-ignore` cannot be used together (use `paths` with a `!` pattern to exclude)")
	}
	if _, err := compileFilter(p.Branches, false); err != nil {
		return fmt.Errorf("pull_request: %v", err)
	}
	if _, err := compileFilter(p.BranchesIgnore, true); err != nil {
		return fmt.Errorf("pull_request: %v", err)
	}
	if _, err := compileFilter(p.Paths, false); err != nil {
		return fmt.Errorf("pull_request: %v", err)
	}
	if _, err := compileFilter(p.PathsIgnore, true); err != nil {
		return fmt.Errorf("pull_request: %v", err)
	}
	return validateTypeFilter("pull_request", p.Types, PullRequestTypes)
}

// validateTypeFilter closes one typed trigger's vocabulary. An empty list (a
// present `types: []`) matches nothing, so it is rejected; an absent list fires
// on every emitted action.
func validateTypeFilter(trigger string, types, supported []string) error {
	if len(types) == 0 && types != nil {
		return fmt.Errorf("%s.types: an empty list matches no action; omit `types` to fire on every emitted action", trigger)
	}
	for _, t := range types {
		if !containsFold(supported, t) {
			return fmt.Errorf("%s.types: unsupported type %q (supported: %s)", trigger, t, strings.Join(supported, ", "))
		}
	}
	return nil
}

// ValidPullRequestType reports whether action is one Threads can emit.
func ValidPullRequestType(action string) bool {
	return containsFold(PullRequestTypes, action)
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
// `schedule: - cron: "0 3 * * *"`. Timezone is the documented per-entry IANA
// zone; empty means UTC. The DST rule is in cron.go (a missed local time is
// skipped, a repeated one fires once).
type CronEntry struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
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

// Strategy is the job's matrix. FailFast is carried; expansion (matrix.go) is
// shared by the parser's bound check and the server's job expansion, and the
// fail-fast policy is the scheduler's.
type Strategy struct {
	Matrix   Matrix `yaml:"matrix"`
	FailFast *bool  `yaml:"fail-fast"`
}

// Environment is a job's deployment environment
// (`jobs.<key>.environment`, docs/PLAN-THREADS-ENVIRONMENTS.md): a bare name
// or `{name, url}`. The name is validated here (it is a bucket key segment);
// whether the environment exists and who must approve it is the repository's
// stored environment record, resolved by the server at run creation.
type Environment struct {
	Name string `yaml:"name"`
	// URL is the deployment URL a run page renders. Only absolute http(s)
	// URLs are accepted: a page renders it as a link, so a scheme the page
	// could execute is this file's parse error.
	URL string `yaml:"url"`
}

// EnvironmentNameRe pins an environment name: it is a bucket key segment and
// a URL path segment.
var EnvironmentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// UnmarshalYAML accepts the scalar name and the mapping form.
func (e *Environment) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		e.Name = node.Value
		return nil
	case yaml.MappingNode:
		if err := checkKeys(node, "name", "url"); err != nil {
			return err
		}
		var raw struct {
			Name string `yaml:"name"`
			URL  string `yaml:"url"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		e.Name, e.URL = raw.Name, raw.URL
		return nil
	default:
		return fmt.Errorf("environment: expected a name or a mapping")
	}
}

// Validate checks one environment declaration.
func (e *Environment) Validate(job string) error {
	if e == nil {
		return nil
	}
	name := strings.TrimSpace(e.Name)
	if !EnvironmentNameRe.MatchString(name) {
		return fmt.Errorf("%w: job %s environment %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}",
			ErrBadEnvironment, job, e.Name)
	}
	if u := strings.TrimSpace(e.URL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%w: job %s environment %s url %q must be an absolute http(s) URL",
				ErrBadEnvironment, job, name, e.URL)
		}
	}
	return nil
}

// Needs is `jobs.<key>.needs`: one job key (`needs: test`) or a list
// (`needs: [test, build]`). Both spellings mean the same dependency set.
type Needs []string

// UnmarshalYAML accepts the scalar and sequence spellings.
func (n *Needs) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			*n = nil
			return nil
		}
		*n = Needs{node.Value}
		return nil
	case yaml.SequenceNode:
		out := make(Needs, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("needs entries must be job names")
			}
			out = append(out, item.Value)
		}
		*n = out
		return nil
	default:
		return fmt.Errorf("needs: expected a job name or a list of job names")
	}
}

// Job is one unit of work. Steps run inside `Container` (or the runner's
// default image) on one workspace. A job-level `uses` names a reusable
// workflow instead of steps; it is accepted and carried, execution is pass B/C.
type Job struct {
	Name   string `yaml:"name"`
	RunsOn Labels `yaml:"runs-on"`
	// Environment is the deployment environment this job targets (nil when the
	// job names none: an ordinary job is unchanged).
	Environment     *Environment       `yaml:"environment"`
	Container       *Container         `yaml:"container"`
	Services        map[string]Service `yaml:"services"`
	Env             map[string]string  `yaml:"env"`
	Steps           []Step             `yaml:"steps"`
	Needs           Needs              `yaml:"needs"`
	If              string             `yaml:"if"`
	TimeoutMinutes  int                `yaml:"timeout-minutes"`
	ContinueOnError bool               `yaml:"continue-on-error"`
	// Outputs are the job's declared output templates
	// (`name: ${{ steps.<id>.outputs.<x> }}`), evaluated by the runner once the
	// steps finish and reported on complete (docs/threads.md §3.9). A dependent
	// job reads them as `needs.<job>.outputs.<name>`.
	Outputs map[string]string `yaml:"outputs"`
	// Reusable workflow (accepted and carried; no runs-on/steps required).
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	// Strategy/Concurrency/Permissions are carried into the spec.
	Strategy    *Strategy    `yaml:"strategy"`
	Concurrency *Concurrency `yaml:"concurrency"`
	Permissions Permissions  `yaml:"permissions"`
	// EnableOpenIDConnect is the job-level Forgejo spelling (PAR-119); it is
	// ORed with the workflow-level value (a job cannot turn the workflow's
	// grant off with a boolean, but `permissions: id-token: none` denies it).
	EnableOpenIDConnect bool `yaml:"enable-openid-connect"`
}

// Service is a sidecar container reachable by its key on the job network.
type Service struct {
	Image string `yaml:"image"`
	// Cmd is Forgejo's spelling of `command`; both are carried and the runner
	// applies the same argv tail.
	Cmd        []string          `yaml:"cmd"`
	Env        map[string]string `yaml:"env"`
	Ports      []string          `yaml:"ports"`
	Command    []string          `yaml:"command"`
	Entrypoint []string          `yaml:"entrypoint"`
	Options    string            `yaml:"options"`
	Volumes    []string          `yaml:"volumes"`
}

// Step is one command or built-in action.
type Step struct {
	// ID is the step's `id:` (GitHub's `steps.<id>` key). It must be a
	// GitHub-style identifier and unique within its job; the runner records
	// the step's outcome/conclusion under it so a later step's `if:` can read
	// `steps.<id>.outcome`.
	ID               string            `yaml:"id"`
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
	ErrEmpty          = errors.New("workflow: empty file")
	ErrNoJobs         = errors.New("workflow: no jobs")
	ErrNoSteps        = errors.New("workflow: job has no steps")
	ErrNoRunsOn       = errors.New("workflow: job has no runs-on")
	ErrBothStep       = errors.New("workflow: step has both run and uses")
	ErrNoStepVerb     = errors.New("workflow: step has neither run nor uses")
	ErrBadNeeds       = errors.New("workflow: needs names an unknown job")
	ErrBadStepID      = errors.New("workflow: invalid step id")
	ErrBadOutput      = errors.New("workflow: invalid job outputs")
	ErrBadEnvironment = errors.New("workflow: invalid environment")
	ErrCycle          = errors.New("workflow: job dependencies form a cycle")
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
	// Concurrency groups are expanded into bucket keys (the lock that decides
	// which run executes), so a reference that would not resolve is THIS file's
	// parse error — never a literal fallback (round-2 audit; 15 §4).
	inputNames := dispatchInputNames(w.On.WorkflowDispatch)
	if w.Concurrency != nil {
		if err := ValidateGroupTemplate(w.Concurrency.Group, nil, inputNames, true); err != nil {
			return nil, fmt.Errorf("workflow: %w", err)
		}
	}
	// Workflow-level env is copied verbatim by the runner (only a step's env
	// values are substituted), so a `${{ … }}` here is a parse error too
	// (docs/WORKFLOWS.md §6.2).
	if err := validateStructuralEnv("env", w.Env); err != nil {
		return nil, err
	}
	for name, job := range w.Jobs {
		// Structural fields (runs-on, container/service images, environment) and
		// the job/container/service env layers are read before a matrix cell or
		// dispatch input exists and are never substituted, so a `${{ … }}` there
		// is this file's parse error rather than a literal that queues the job
		// against no runner (docs/WORKFLOWS.md §6.2).
		if err := validateStructuralFields(name, &job); err != nil {
			return nil, err
		}
		reusable := strings.TrimSpace(job.Uses) != ""
		if err := job.Environment.Validate(name); err != nil {
			return nil, err
		}
		if err := validateStrategy(name, job.Strategy); err != nil {
			return nil, err
		}
		if !reusable {
			if len(job.RunsOn) == 0 {
				return nil, fmt.Errorf("%w: %s", ErrNoRunsOn, name)
			}
			if len(job.Steps) == 0 {
				return nil, fmt.Errorf("%w: %s", ErrNoSteps, name)
			}
		}
		axes := strategyAxes(job.Strategy)
		if err := ValidateTemplate(job.Name, TemplateScope{Axes: axes, Inputs: inputNames}); err != nil {
			return nil, fmt.Errorf("workflow: job %s name: %w", name, err)
		}
		if err := ValidateIf(job.If, TemplateScope{Axes: axes, Inputs: inputNames, Needs: job.Needs, JobIf: true}); err != nil {
			return nil, fmt.Errorf("workflow: job %s if: %w", name, err)
		}
		stepIDs := map[string]bool{}
		// declared grows in step order: a step may only read outputs of an
		// earlier step (docs/threads.md §3.9).
		declared := make([]string, 0, len(job.Steps))
		for i, step := range job.Steps {
			if step.Run != "" && step.Uses != "" {
				return nil, fmt.Errorf("%w: %s step %d", ErrBothStep, name, i+1)
			}
			if step.Run == "" && step.Uses == "" {
				return nil, fmt.Errorf("%w: %s step %d", ErrNoStepVerb, name, i+1)
			}
			if err := checkStepID(name, i, step.ID, stepIDs); err != nil {
				return nil, err
			}
			if err := validateStepFieldRefs(name, i, step, job.Needs, declared); err != nil {
				return nil, err
			}
			if err := validateStepTemplates(name, i, step, TemplateScope{
				Axes: axes, Inputs: inputNames, Steps: declared, Needs: job.Needs,
			}); err != nil {
				return nil, err
			}
			if step.ID != "" {
				declared = append(declared, strings.ToLower(step.ID))
			}
		}
		if err := validateJobOutputs(name, job, inputNames, axes); err != nil {
			return nil, err
		}
		for _, need := range job.Needs {
			if _, ok := w.Jobs[need]; !ok {
				return nil, fmt.Errorf("%w: %s needs %s", ErrBadNeeds, name, need)
			}
		}
		if job.Concurrency != nil {
			if err := ValidateGroupTemplate(job.Concurrency.Group, strategyAxes(job.Strategy), inputNames, false); err != nil {
				return nil, fmt.Errorf("workflow: job %s: %w", name, err)
			}
		}
	}
	if err := checkAcyclic(w.Jobs); err != nil {
		return nil, err
	}
	return &w, nil
}

// checkStepID validates one step's `id:` (GitHub-style: letters, digits, `_`
// and `-`, not starting with a digit) and rejects a duplicate within the same
// job. An empty id is the common case and always fine.
func checkStepID(job string, index int, id string, seen map[string]bool) error {
	if id == "" {
		return nil
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '-':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("%w: job %s step %d: %q starts with a digit", ErrBadStepID, job, index+1, id)
			}
		default:
			return fmt.Errorf("%w: job %s step %d: %q must contain only letters, digits, _ and -", ErrBadStepID, job, index+1, id)
		}
	}
	// GitHub treats step ids case-insensitively in `steps.<id>`; a duplicate
	// that only differs in case would make one of the two unreachable.
	key := strings.ToLower(id)
	if seen[key] {
		return fmt.Errorf("%w: job %s: duplicate step id %q", ErrBadStepID, job, id)
	}
	seen[key] = true
	return nil
}

// validateDispatch checks the typed workflow_dispatch inputs (types:
// string/choice/boolean/number/environment; choice needs options; an
// environment input's options are the repository's environment names, resolved
// at dispatch time).
func validateDispatch(d *DispatchTrigger) error {
	if d == nil {
		return nil
	}
	for name, in := range d.Inputs {
		switch in.Type {
		case "", "string", "boolean", "number", "environment":
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

// dispatchInputNames lists the declared workflow_dispatch input names, sorted
// (the names a `${{ inputs.… }}` reference may use).
func dispatchInputNames(d *DispatchTrigger) []string {
	if d == nil || len(d.Inputs) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.Inputs))
	for name := range d.Inputs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// strategyAxes lists a strategy's usable matrix fields, sorted: the declared
// axes and the keys an include entry names (a `${{ matrix.<key> }}` reference
// may read either).
func strategyAxes(s *Strategy) []string {
	if s == nil {
		return nil
	}
	return s.Matrix.FieldNames()
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

// validateStrategy bounds one job's matrix at parse time.
func validateStrategy(jobName string, s *Strategy) error {
	if s == nil {
		return nil
	}
	if err := s.Matrix.Validate(); err != nil {
		return fmt.Errorf("workflow: job %s: %w", jobName, err)
	}
	return nil
}

// RefKind is the kind of git ref an event is delivered for. A push to a branch
// and a push to a tag are different triggers (on.push.branches vs
// on.push.tags), so the caller must say which one it is delivering; passing a
// branch name for a tag push would silently match the wrong filter.
type RefKind uint8

const (
	// RefBranch is a `refs/heads/*` ref (the only kind the push hook delivers
	// today).
	RefBranch RefKind = iota
	// RefTag is a `refs/tags/*` ref.
	RefTag
)

// String renders the kind for error messages and records.
func (k RefKind) String() string {
	if k == RefTag {
		return "tag"
	}
	return "branch"
}

// fires applies one push trigger to a ref. GitHub's rule, implemented exactly:
//
//   - a branch push is filtered by `branches`; when only `tags` is declared a
//     branch push does not fire (`on.push.tags` never matches a branch);
//   - a tag push is filtered by `tags`; when only `branches` is declared a tag
//     push does not fire;
//   - a bare `on: push` (neither list) fires on both.
func (p *PushTrigger) fires(kind RefKind, name string) bool {
	if kind == RefTag {
		if len(p.Tags) > 0 || len(p.TagsIgnore) > 0 {
			return matchFilter(p.Tags, p.TagsIgnore, name)
		}
		return len(p.Branches) == 0 && len(p.BranchesIgnore) == 0
	}
	if len(p.Branches) > 0 || len(p.BranchesIgnore) > 0 {
		return matchFilter(p.Branches, p.BranchesIgnore, name)
	}
	return len(p.Tags) == 0 && len(p.TagsIgnore) == 0
}

// matchFilter applies an include list or an exclude list to one name. A
// compile error (only possible for a hand-built trigger) matches nothing.
func matchFilter(include, exclude []string, name string) bool {
	if len(include) > 0 {
		f, err := compileFilter(include, false)
		if err != nil {
			return false
		}
		return f.Match(name)
	}
	if len(exclude) > 0 {
		f, err := compileFilter(exclude, true)
		if err != nil {
			return false
		}
		return f.Match(name)
	}
	return true
}

// Fires reports whether the workflow triggers on an event. event is one of
// "push", "pull_request", "workflow_dispatch"; schedule is handled by the
// scheduler. kind names the ref kind (a branch or a tag) and matters only for
// push. ref is the short ref name: the branch name, the tag name, or the PR
// target branch; it may be empty (no filter applies). action is the
// pull_request lifecycle action the caller is delivering (opened, synchronize,
// reopened) and is ignored by the other events; a pull_request event with no
// recorded action matches only an unfiltered trigger, because a `types` filter
// can only be decided from a known action.
func (w *Workflow) Fires(event string, kind RefKind, ref, action string) bool {
	return w.FiresWithChanges(event, kind, ref, action, nil, false)
}

// FiresWithChanges is Fires plus the changed-file set a path-filtered trigger
// needs. known is false when the caller did not compute a diff; a trigger that
// declares `paths`/`paths-ignore` then does not fire (fail closed).
func (w *Workflow) FiresWithChanges(event string, kind RefKind, ref, action string, changed []string, known bool) bool {
	switch event {
	case "push":
		if w.On.Push == nil && !w.On.PushSet {
			return false
		}
		if w.On.Push == nil {
			return true // `push:` with no filters: every branch and tag
		}
		if !w.On.Push.fires(kind, ref) {
			return false
		}
		return matchPaths(w.On.Push.Paths, w.On.Push.PathsIgnore, changed, known)
	case "pull_request":
		if w.On.PullRequest == nil && !w.On.PullRequestSet {
			return false
		}
		if w.On.PullRequest == nil {
			return true // `pull_request:` with no filters: every target branch
		}
		if !matchFilter(w.On.PullRequest.Branches, w.On.PullRequest.BranchesIgnore, ref) {
			return false
		}
		if !matchPRType(w.On.PullRequest.Types, action) {
			return false
		}
		return matchPaths(w.On.PullRequest.Paths, w.On.PullRequest.PathsIgnore, changed, known)
	case "release":
		return typedFires(w.On.ReleaseSet, w.On.Release, action)
	case "issues":
		return typedFires(w.On.IssuesSet, w.On.Issues, action)
	case "issue_comment":
		return typedFires(w.On.IssueCommentSet, w.On.IssueComment, action)
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
	if w.On.Release != nil || w.On.ReleaseSet {
		out = append(out, "release")
	}
	if w.On.Issues != nil || w.On.IssuesSet {
		out = append(out, "issues")
	}
	if w.On.IssueComment != nil || w.On.IssueCommentSet {
		out = append(out, "issue_comment")
	}
	return out
}

// typedFires applies one typed trigger's `types` filter (an absent trigger
// behind the presence flag fires, an absent filter fires on every action).
func typedFires(set bool, trigger *TypeTrigger, action string) bool {
	if trigger == nil {
		return set
	}
	return matchPRType(trigger.Types, action)
}

// matchPRType applies a validated `types` filter. An absent filter matches
// every emitted action; a present one matches only the listed actions, so an
// unknown/empty action never satisfies a filter.
func matchPRType(types []string, action string) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if action != "" && strings.EqualFold(t, action) {
			return true
		}
	}
	return false
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
