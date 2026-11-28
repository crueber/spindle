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
	"strconv"
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
	// RunName is the run title template (PAR-057): rendered at run creation
	// from github.* and inputs.*. It is display-only, bounded, and never used
	// as a key.
	RunName string `yaml:"run-name"`
	// Defaults is the workflow-level `defaults.run` block (PAR-054): a job's
	// own defaults override it key by key, and a step's own key wins over both.
	Defaults *Defaults      `yaml:"defaults"`
	Jobs     map[string]Job `yaml:"jobs"`
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
	Push              *PushTrigger     `yaml:"push"`
	PullRequest       *PRTrigger       `yaml:"pull_request"`
	PullRequestTarget *PRTrigger       `yaml:"pull_request_target"`
	WorkflowDispatch  *DispatchTrigger `yaml:"workflow_dispatch"`
	Schedule          []CronEntry      `yaml:"schedule"`
	// Release, Issues and IssueComment are the event services' triggers
	// (PAR-017, PAR-020): each names the lifecycle actions its owning service
	// actually emits, and an unemittable type is a parse error.
	Release      *TypeTrigger `yaml:"release"`
	Issues       *TypeTrigger `yaml:"issues"`
	IssueComment *TypeTrigger `yaml:"issue_comment"`
	// PullRequestReview and PullRequestReviewComment are the review service's
	// edges (PAR-020): a submitted/dismissed review and a created inline
	// review comment. Label and Milestone are the issues service's definition
	// lifecycle (a label or milestone created/edited/deleted by triage), which
	// is distinct from the PR/issue `labeled`/`milestoned` metadata edges.
	PullRequestReview        *TypeTrigger `yaml:"pull_request_review"`
	PullRequestReviewComment *TypeTrigger `yaml:"pull_request_review_comment"`
	Label                    *TypeTrigger `yaml:"label"`
	Milestone                *TypeTrigger `yaml:"milestone"`
	// WorkflowCall makes this file a reusable workflow (PAR-075): its typed
	// inputs, declared secrets and outputs are the contract a caller's
	// `jobs.<id>.uses` is checked against. The key never starts a run.
	WorkflowCall *WorkflowCallTrigger `yaml:"workflow_call"`
	// RepositoryDispatch is the API-dispatched custom event (PAR-018): its
	// `types` are the caller's event_type strings, bounded by a closed charset.
	RepositoryDispatch *RepositoryDispatchTrigger `yaml:"repository_dispatch"`
	// WorkflowRun follows another workflow run (PAR-019). It filters on the
	// upstream workflow's NAME, the lifecycle type and the upstream head branch.
	WorkflowRun *WorkflowRunTrigger `yaml:"workflow_run"`
	// MergeGroup is the merge-queue trigger (PAR-016): a push to a documented
	// `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref starts its checks.
	// The queue that creates those refs is a separate concern; this trigger is
	// the run contract it hands off to.
	MergeGroup    *MergeGroupTrigger `yaml:"merge_group"`
	MergeGroupSet bool               `yaml:"-"`
	// Create/Delete are the git ref lifecycle events emitted by the server's
	// push hook (PAR-021). The other repository/check/deploy names are refused
	// at parse time by name (RefusedTriggers) rather than accepted inertly.
	CreateSet           bool `yaml:"-"`
	DeleteSet           bool `yaml:"-"`
	DeploymentSet       bool `yaml:"-"`
	DeploymentStatusSet bool `yaml:"-"`
	// ForkSet/WatchSet are the fork and star events (PAR-021): both are
	// emitted by the services that own the transition (pulls, social), so a
	// bare key is a real trigger with no filters.
	ForkSet  bool `yaml:"-"`
	WatchSet bool `yaml:"-"`
	// CheckRun names the external-CI check-run edges (PAR-021):
	// created/completed. Threads' own mirrored check is the run itself, not an
	// app check-run event, so it never re-fires this trigger.
	CheckRun    *TypeTrigger `yaml:"check_run"`
	CheckRunSet bool         `yaml:"-"`
	// DispatchSet records that the key was PRESENT even as null
	// (`workflow_dispatch:` with no value decodes to a nil pointer).
	DispatchSet bool `yaml:"-"`
	// PushSet/PullRequestSet/PullRequestTargetSet are the same presence rule
	// for the branch-filtered triggers: `push:` with no value is legal and
	// means "every branch", but it decodes to a nil pointer.
	PushSet              bool `yaml:"-"`
	PullRequestSet       bool `yaml:"-"`
	PullRequestTargetSet bool `yaml:"-"`
	// ScheduleSet records `schedule:` presence (the entries may be empty;
	// cron evaluation is the maintainer role's, not the parser's).
	ScheduleSet bool `yaml:"-"`
	// ReleaseSet/IssuesSet/IssueCommentSet/RepositoryDispatchSet/WorkflowRunSet
	// are the same presence rule for the typed triggers.
	ReleaseSet                  bool `yaml:"-"`
	IssuesSet                   bool `yaml:"-"`
	IssueCommentSet             bool `yaml:"-"`
	PullRequestReviewSet        bool `yaml:"-"`
	PullRequestReviewCommentSet bool `yaml:"-"`
	LabelSet                    bool `yaml:"-"`
	MilestoneSet                bool `yaml:"-"`
	RepositoryDispatchSet       bool `yaml:"-"`
	WorkflowRunSet              bool `yaml:"-"`
	// WorkflowCallSet records `on.workflow_call` presence even as a bare null
	// (`workflow_call:` with no body is a legal callable with no inputs).
	WorkflowCallSet bool `yaml:"-"`
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
			case "pull_request_target":
				t.PullRequestTargetSet = true
				if !isNull(val) {
					if err = checkKeys(val, "branches", "branches-ignore", "types", "paths", "paths-ignore"); err == nil {
						var p PRTrigger
						if err = val.Decode(&p); err == nil {
							err = p.validate()
						}
						if err == nil {
							t.PullRequestTarget = &p
						}
					}
				}
			case "repository_dispatch":
				t.RepositoryDispatchSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r RepositoryDispatchTrigger
						if err = val.Decode(&r); err == nil {
							err = r.validate()
						}
						if err == nil {
							t.RepositoryDispatch = &r
						}
					}
				}
			case "workflow_run":
				t.WorkflowRunSet = true
				if !isNull(val) {
					if err = checkKeys(val, "workflows", "types", "branches", "branches-ignore"); err == nil {
						var w WorkflowRunTrigger
						if err = val.Decode(&w); err == nil {
							err = w.validate()
						}
						if err == nil {
							t.WorkflowRun = &w
						}
					}
				}
			case "merge_group":
				t.MergeGroupSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types", "branches", "branches-ignore"); err == nil {
						var w MergeGroupTrigger
						if err = val.Decode(&w); err == nil {
							err = w.validate()
						}
						if err == nil {
							t.MergeGroup = &w
						}
					}
				}
			case "create":
				t.CreateSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "delete":
				t.DeleteSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "deployment":
				t.DeploymentSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "deployment_status":
				t.DeploymentStatusSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "fork":
				t.ForkSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "watch":
				t.WatchSet = true
				if !isNull(val) {
					err = checkKeys(val)
				}
			case "check_run":
				t.CheckRunSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("check_run", r.Types, CheckRunTypes)
						}
						if err == nil {
							t.CheckRun = &r
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
			case "pull_request_review":
				t.PullRequestReviewSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("pull_request_review", r.Types, ReviewTypes)
						}
						if err == nil {
							t.PullRequestReview = &r
						}
					}
				}
			case "pull_request_review_comment":
				t.PullRequestReviewCommentSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("pull_request_review_comment", r.Types, ReviewCommentTypes)
						}
						if err == nil {
							t.PullRequestReviewComment = &r
						}
					}
				}
			case "label":
				t.LabelSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("label", r.Types, LabelTypes)
						}
						if err == nil {
							t.Label = &r
						}
					}
				}
			case "milestone":
				t.MilestoneSet = true
				if !isNull(val) {
					if err = checkKeys(val, "types"); err == nil {
						var r TypeTrigger
						if err = val.Decode(&r); err == nil {
							err = validateTypeFilter("milestone", r.Types, MilestoneTypes)
						}
						if err == nil {
							t.Milestone = &r
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
			case "workflow_call":
				t.WorkflowCallSet = true
				// `workflow_call:` with no body is a legal callable with no
				// declared inputs/secrets/outputs, so it still gets a non-nil
				// declaration (the call contract, just empty).
				call := &WorkflowCallTrigger{}
				if !isNull(val) {
					if err = val.Decode(call); err != nil {
						return err
					}
				}
				t.WorkflowCall = call
			default:
				if why, refused := RefusedTriggers[key]; refused {
					err = fmt.Errorf("on: %s: %s", key, why)
				} else {
					err = fmt.Errorf("on: unknown trigger %q", key)
				}
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
	case "pull_request_target":
		t.PullRequestTargetSet = true
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
	case "pull_request_review":
		t.PullRequestReviewSet = true
	case "pull_request_review_comment":
		t.PullRequestReviewCommentSet = true
	case "label":
		t.LabelSet = true
	case "milestone":
		t.MilestoneSet = true
	case "workflow_call":
		t.WorkflowCallSet = true
		t.WorkflowCall = &WorkflowCallTrigger{}
	case "repository_dispatch":
		t.RepositoryDispatchSet = true
	case "workflow_run":
		t.WorkflowRunSet = true
	case "merge_group":
		t.MergeGroupSet = true
	case "create":
		t.CreateSet = true
	case "delete":
		t.DeleteSet = true
	case "deployment":
		t.DeploymentSet = true
	case "deployment_status":
		t.DeploymentStatusSet = true
	case "fork":
		t.ForkSet = true
	case "watch":
		t.WatchSet = true
	case "check_run":
		t.CheckRunSet = true
	default:
		if why, refused := RefusedTriggers[name]; refused {
			return fmt.Errorf("on: %s: %s", name, why)
		}
		return fmt.Errorf("on: unknown trigger %q", name)
	}
	return nil
}

// RefusedTriggers names the GitHub event triggers this engine does not emit,
// with the reason. They are a NAMED parse error, never accepted-and-inert (the
// master plan's one rule both parity cards agree on): a workflow that would
// silently never run is a lie about the CI it replaced.
var RefusedTriggers = map[string]string{
	"status":                     "spindle's check surface reports check runs, not legacy commit statuses",
	"check_suite":                "spindle has no app check-suite object: its checks mirror writes individual check runs (the check_run event is emitted for an external CI report), so a check_suite run cannot be emitted",
	"registry_package":           "spindle's OCI registry does not emit registry_package events",
	"gollum":                     "spindle has no wiki",
	"page_build":                 "spindle serves static sites from committed bytes; it has no page build",
	"branch_protection_rule":     "spindle has no branch-protection-rule event emitter",
	"public":                     "spindle has no repository visibility-change event emitter",
	"discussion":                 "spindle has no discussion surface, so a discussion run cannot be emitted",
	"discussion_comment":         "spindle has no discussion surface, so a discussion_comment run cannot be emitted",
	"pull_request_review_thread": "spindle emits pull_request_review and pull_request_review_comment; resolving an inline thread is not a separate trigger",
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

// RepositoryDispatchTrigger is `on.repository_dispatch` (PAR-018). `types` are
// the event_type strings a dispatcher may send; they are the caller's names,
// so the charset is bounded (a bucket/API key must stay sane) but the
// vocabulary is not closed. An empty list matches nothing, so it is refused.
type RepositoryDispatchTrigger struct {
	Types []string `yaml:"types"`
}

// validate bounds the dispatch type strings. GitHub allows any event_type up
// to 100 characters; the same bound keeps a payload projection predictable.
func (r *RepositoryDispatchTrigger) validate() error {
	if len(r.Types) == 0 && r.Types != nil {
		return fmt.Errorf("repository_dispatch.types: an empty list matches no event_type; omit `types` to fire on every dispatch")
	}
	for _, typ := range r.Types {
		if err := validateDispatchType(typ); err != nil {
			return fmt.Errorf("repository_dispatch.types: %v", err)
		}
	}
	return nil
}

// dispatchTypeRe is the event_type charset (`[A-Za-z0-9._-]`, 1…100).
var dispatchTypeRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// validateDispatchType checks one repository_dispatch event_type.
func validateDispatchType(typ string) error {
	if !dispatchTypeRe.MatchString(typ) {
		return fmt.Errorf("%q is not a valid event_type (expected [A-Za-z0-9._-]{1,100})", typ)
	}
	return nil
}

// ValidDispatchType reports whether a dispatcher's event_type is well formed.
func ValidDispatchType(typ string) bool { return validateDispatchType(typ) == nil }

// WorkflowRunTypes is the closed set of workflow_run lifecycle types Threads
// emits: `requested` when the upstream run is created and `completed` when it
// folds terminal. `in_progress` is documented by GitHub but Threads does not
// emit it, so naming it is refused rather than accepted inertly.
var WorkflowRunTypes = []string{"requested", "completed"}

// WorkflowRunTrigger is `on.workflow_run` (PAR-019): a follow-up workflow
// filtered by the upstream workflow's name, the lifecycle type and the
// upstream head branch.
type WorkflowRunTrigger struct {
	Workflows      []string `yaml:"workflows"`
	Types          []string `yaml:"types"`
	Branches       []string `yaml:"branches"`
	BranchesIgnore []string `yaml:"branches-ignore"`
}

// validate closes the workflow_run filters. `workflows` entries are names, not
// globs (GitHub's rule), so an empty name is refused; the branches lists use
// the shared filter dialect.
func (w *WorkflowRunTrigger) validate() error {
	if len(w.Branches) > 0 && len(w.BranchesIgnore) > 0 {
		return fmt.Errorf("workflow_run: `branches` and `branches-ignore` cannot be used together (use `branches` with a `!` pattern to exclude)")
	}
	for _, name := range w.Workflows {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("workflow_run.workflows: an empty workflow name matches nothing")
		}
	}
	if len(w.Workflows) > maxWorkflowRunWorkflows {
		return fmt.Errorf("workflow_run.workflows: at most %d names", maxWorkflowRunWorkflows)
	}
	if _, err := compileFilter(w.Branches, false); err != nil {
		return fmt.Errorf("workflow_run: %v", err)
	}
	if _, err := compileFilter(w.BranchesIgnore, true); err != nil {
		return fmt.Errorf("workflow_run: %v", err)
	}
	return validateTypeFilter("workflow_run", w.Types, WorkflowRunTypes)
}

// maxWorkflowRunWorkflows bounds the name filter (a workflow file naming a
// hundred upstreams is a mistake, not a design).
const maxWorkflowRunWorkflows = 20

// MergeGroupTypes is the closed set of merge_group activity types Threads
// emits. The merge queue has one edge worth a run: `checks_requested`, the
// moment a queue entry exists and its checks must be produced. GitHub also
// documents a `destroyed` payload, but Threads does not run checks for it, so
// naming it is refused rather than accepted inertly.
var MergeGroupTypes = []string{"checks_requested"}

// MergeGroupTrigger is `on.merge_group` (PAR-016). A queued pull request is a
// push to the documented queue ref
// `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>`; the trigger's optional
// branch filters select the target branch and `types` selects the one emitted
// activity.
type MergeGroupTrigger struct {
	Types          []string `yaml:"types"`
	Branches       []string `yaml:"branches"`
	BranchesIgnore []string `yaml:"branches-ignore"`
}

// validate closes the merge_group filters: one activity type, and the shared
// branch-filter dialect applied to the TARGET branch in the ref.
func (m *MergeGroupTrigger) validate() error {
	if len(m.Branches) > 0 && len(m.BranchesIgnore) > 0 {
		return fmt.Errorf("merge_group: `branches` and `branches-ignore` cannot be used together (use `branches` with a `!` pattern to exclude)")
	}
	if _, err := compileFilter(m.Branches, false); err != nil {
		return fmt.Errorf("merge_group: %v", err)
	}
	if _, err := compileFilter(m.BranchesIgnore, true); err != nil {
		return fmt.Errorf("merge_group: %v", err)
	}
	return validateTypeFilter("merge_group", m.Types, MergeGroupTypes)
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

// ReviewTypes is the closed set of pull_request_review actions the review
// service emits: submitted (a review posted) and dismissed (a maintainer
// dismissal). Review editing is not implemented, so `edited` is a parse error.
var ReviewTypes = []string{"submitted", "dismissed"}

// ReviewCommentTypes is the closed set of pull_request_review_comment actions
// the review service emits: created for a new inline thread or reply. Comment
// editing and deletion are not implemented, so those stay parse errors.
var ReviewCommentTypes = []string{"created"}

// LabelTypes is the closed set of label-definition actions the issues service
// emits (a label created, edited or deleted — NOT the `labeled` PR/issue edge).
var LabelTypes = []string{"created", "edited", "deleted"}

// MilestoneTypes is the closed set of milestone-definition actions the issues
// service emits. `opened`/`closed` are the state transitions; `edited` is any
// other field change; `deleted` removes the milestone.
var MilestoneTypes = []string{"created", "opened", "closed", "edited", "deleted"}

// CheckRunTypes is the closed set of check_run actions the checks service
// emits for an external CI report: created when the check run first appears and
// completed when it reaches a terminal state. `rerequested` and
// `requested_action` are not implemented, so naming them is refused rather than
// accepted inertly.
var CheckRunTypes = []string{"created", "completed"}

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
	Group string `yaml:"group"`
	// CancelInProgress is a literal or one whole `${{ … }}` expression
	// (PAR-151), evaluated at run creation per matrix cell.
	CancelInProgress BoolExpr `yaml:"cancel-in-progress"`
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
			Group            string   `yaml:"group"`
			CancelInProgress BoolExpr `yaml:"cancel-in-progress"`
		}
		if err := node.Decode(&raw); err != nil {
			return fmt.Errorf("concurrency.cancel-in-progress: %w", err)
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
	case yaml.MappingNode:
		return fmt.Errorf("runs-on: the `{group:, labels:}` form is not supported (a runner group is not a spindle concept; write the label list directly, e.g. `runs-on: [self-hosted, linux]` or `runs-on: ${{ matrix.os }}`)")
	default:
		return fmt.Errorf("runs-on: expected a label or a list of labels")
	}
}

// String renders the label set for the record's convenience field.
func (l Labels) String() string { return strings.Join(l, ", ") }

// Container is a job container: the scalar image shorthand or the mapping with
// env/ports/command/entrypoint/options/volumes. Every mapping key is applied by
// the runner (docs/threads.md §4.1); `credentials` is carried so the server can
// refuse it BY NAME with the registry-identity policy, rather than failing the
// whole file on an unknown key.
type Container struct {
	Image      string            `yaml:"image"`
	Env        map[string]string `yaml:"env"`
	Ports      []string          `yaml:"ports"`
	Command    []string          `yaml:"command"`
	Entrypoint []string          `yaml:"entrypoint"`
	Options    string            `yaml:"options"`
	Volumes    []string          `yaml:"volumes"`
	// Credentials is Forgejo's registry-credential mapping. It is parsed so the
	// refusal can name the field (the pull identity is the runner operator's,
	// docs/threads.md §8.6), never applied.
	Credentials *ContainerCredentials `yaml:"credentials"`
}

// ContainerCredentials is `container.credentials`: Forgejo's spelling for a
// workflow-supplied registry login.
type ContainerCredentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// UnmarshalYAML accepts only the documented two keys.
func (c *ContainerCredentials) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("credentials: expected a mapping with username and password")
	}
	if err := checkKeys(node, "username", "password"); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	type raw ContainerCredentials
	var r raw
	if err := node.Decode(&r); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	*c = ContainerCredentials(r)
	return nil
}

// UnmarshalYAML accepts `container: golang:1.27` and the mapping form. GitHub's
// documented scalar-or-list spellings are accepted for ports/command/entrypoint
// (`command: /bin/sh -c 'sleep infinity'` is the spelling authors use), and every
// accepted key is applied by the runner.
func (c *Container) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil
		}
		c.Image = node.Value
		return nil
	case yaml.MappingNode:
		if err := checkKeys(node, "image", "env", "ports", "command", "entrypoint", "options", "volumes", "credentials"); err != nil {
			return fmt.Errorf("container: %w", err)
		}
		type raw struct {
			Image       string                `yaml:"image"`
			Env         map[string]string     `yaml:"env"`
			Ports       yaml.Node             `yaml:"ports"`
			Command     yaml.Node             `yaml:"command"`
			Entrypoint  yaml.Node             `yaml:"entrypoint"`
			Options     string                `yaml:"options"`
			Volumes     []string              `yaml:"volumes"`
			Credentials *ContainerCredentials `yaml:"credentials"`
		}
		var r raw
		if err := node.Decode(&r); err != nil {
			return fmt.Errorf("container: %w", err)
		}
		out := Container{Image: r.Image, Env: r.Env, Options: r.Options, Volumes: r.Volumes, Credentials: r.Credentials}
		for _, field := range []struct {
			name string
			node yaml.Node
			dst  *[]string
			cmd  bool
		}{
			{"ports", r.Ports, &out.Ports, false},
			{"command", r.Command, &out.Command, true},
			{"entrypoint", r.Entrypoint, &out.Entrypoint, true},
		} {
			if field.node.Kind == 0 {
				continue
			}
			var err error
			if field.cmd {
				err = DecodeCommandWords(&field.node, field.dst)
			} else {
				err = decodeStringOrList(&field.node, field.dst)
			}
			if err != nil {
				return fmt.Errorf("container.%s: %w", field.name, err)
			}
		}
		*c = out
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
	// MaxParallel bounds how many cells of this matrix run at once (PAR-052):
	// nil means no limit beyond the runner fleet, and the scheduler admits
	// cells in expansion order.
	MaxParallel *int `yaml:"max-parallel"`
}

// Defaults is a `defaults:` block (PAR-054). Only `run` is accepted, at
// workflow and job level; a step-level `defaults:` is an unknown key.
type Defaults struct {
	Run *RunDefaults `yaml:"run"`
}

// RunDefaults is `defaults.run`: the shell and working directory a run step
// inherits when it names neither. Both are literals by decision — a context or
// expression in either is a parse error naming the key — and a step-level key
// still wins.
type RunDefaults struct {
	Shell            string `yaml:"shell"`
	WorkingDirectory string `yaml:"working-directory"`
}

// DefaultJobTimeoutMinutes is GitHub's default job timeout (PAR-055): a job
// that names none is bounded at 360 minutes.
const DefaultJobTimeoutMinutes = 360

// MaxTimeoutMinutes is the documented upper bound for a job or step timeout.
const MaxTimeoutMinutes = 360

// Minutes is a `timeout-minutes:` value: a positive integer literal or one
// whole `${{ … }}` expression, evaluated per matrix cell before the run record
// is written (PAR-055).
type Minutes struct {
	// Value is the literal (0 when the key is absent or expression-valued).
	Value int
	// Expr is the inside of the `${{ … }}` span, "" for a literal.
	Expr string
	// Present records that the key was authored (an explicit 0 is invalid,
	// while an absent key means "use the default").
	Present bool
}

// UnmarshalYAML accepts an integer or one whole expression span.
func (m *Minutes) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || isNull(node) {
		return fmt.Errorf("timeout-minutes: expected an integer or a ${{ … }} expression")
	}
	raw := strings.TrimSpace(node.Value)
	if inner, ok := wholeSpan(raw); ok {
		if strings.TrimSpace(inner) == "" {
			return fmt.Errorf("timeout-minutes: ${{ }} is empty")
		}
		m.Expr = strings.TrimSpace(inner)
		m.Present = true
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("timeout-minutes: expected an integer or a ${{ … }} expression, got %q", node.Value)
	}
	m.Value = n
	m.Present = true
	return nil
}

// Int returns the literal value (0 when bound to an expression).
func (m Minutes) Int() int { return m.Value }

// Set reports whether the key was authored at all.
func (m Minutes) Set() bool { return m.Present }

// BoolExpr is a boolean field that accepts a literal `true`/`false` or one
// whole `${{ … }}` expression (PAR-056 `continue-on-error`, PAR-151
// `concurrency.cancel-in-progress`). An expression is evaluated per matrix cell
// before the job record is written, from the same cell values as the rest of
// the strategy, so one cell can differ from another.
type BoolExpr struct {
	// Value is the literal (false when the key is absent or expression-valued).
	Value bool
	// Expr is the inside of the `${{ … }}` span, "" for a literal.
	Expr string
	// Present records that the key was authored.
	Present bool
}

// UnmarshalYAML accepts a YAML boolean or one whole expression span.
func (b *BoolExpr) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || isNull(node) {
		return fmt.Errorf("expected true, false or a ${{ … }} expression")
	}
	raw := strings.TrimSpace(node.Value)
	if inner, ok := wholeSpan(raw); ok {
		if strings.TrimSpace(inner) == "" {
			return fmt.Errorf("${{ }} is empty")
		}
		b.Expr = strings.TrimSpace(inner)
		b.Present = true
		return nil
	}
	switch strings.ToLower(raw) {
	case "true":
		b.Value, b.Present = true, true
		return nil
	case "false":
		b.Value, b.Present = false, true
		return nil
	}
	return fmt.Errorf("expected true, false or a ${{ … }} expression, got %q", node.Value)
}

// Bool returns the literal value (false when bound to an expression).
func (b BoolExpr) Bool() bool { return b.Value }

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
	Environment *Environment       `yaml:"environment"`
	Container   *Container         `yaml:"container"`
	Services    map[string]Service `yaml:"services"`
	Env         map[string]string  `yaml:"env"`
	Steps       []Step             `yaml:"steps"`
	Needs       Needs              `yaml:"needs"`
	If          string             `yaml:"if"`
	// TimeoutMinutes bounds the whole job (PAR-055): a literal or one whole
	// expression, defaulted to 360 at run creation.
	TimeoutMinutes Minutes `yaml:"timeout-minutes"`
	// Defaults is the job-level `defaults.run` block (PAR-054); it overrides
	// the workflow's per key, and a step's own key wins.
	Defaults *Defaults `yaml:"defaults"`
	// ContinueOnError tolerates this job's failure: a literal `true`/`false` or
	// one whole `${{ … }}` expression evaluated per matrix cell (PAR-056).
	ContinueOnError BoolExpr `yaml:"continue-on-error"`
	// Outputs are the job's declared output templates
	// (`name: ${{ steps.<id>.outputs.<x> }}`), evaluated by the runner once the
	// steps finish and reported on complete (docs/threads.md §3.9). A dependent
	// job reads them as `needs.<job>.outputs.<name>`.
	Outputs map[string]string `yaml:"outputs"`
	// Reusable workflow (accepted and carried; no runs-on/steps required).
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	// Secrets is the reusable call's secret set (PAR-075): an explicit
	// `name: ${{ secrets.SOURCE }}` map, or `inherit`. A nil pointer means the
	// caller declared no block (same repository: the implicit full set; cross
	// repository: none). On an ordinary job the key is a parse error.
	Secrets *JobSecrets `yaml:"secrets"`
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
	TimeoutMinutes   Minutes           `yaml:"timeout-minutes"`
	// Background starts this step asynchronously in its own session/process
	// group (PAR-080). It needs an `id`, and is accepted on a run step, a
	// docker:// step, or a local `uses: ./…` action (the runner refuses a local
	// composite at plan time, when the action kind is known).
	Background bool `yaml:"background"`
	// Wait names a background step started earlier, or "all"; it is a control
	// step (no run/uses, no id) that joins the process and fails when it failed.
	Wait string `yaml:"wait"`
	// WaitAll is the boolean spelling of `wait: all`.
	WaitAll bool `yaml:"wait-all"`
	// Cancel names a background step started earlier, or "all"; the control
	// step succeeds once the process group is gone.
	Cancel string `yaml:"cancel"`
	// Parallel holds ordinary run/uses steps that execute concurrently as one
	// unit. A nil slice means the key is absent; an empty non-nil slice
	// (`parallel: []`) is refused as a group with no members.
	Parallel []Step `yaml:"parallel"`
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
	// ErrStepExecution marks an invalid PAR-080 step shape (a control step with
	// a process, a parallel group with an invalid member, a background without
	// an id or an addressable target). The message names the rule.
	ErrStepExecution = errors.New("workflow: invalid step execution")
	// ErrBadTimeout marks a `timeout-minutes` outside 1…360 (PAR-055).
	ErrBadTimeout = errors.New("workflow: invalid timeout")
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
	inputNames := callInputNames(&w)
	if w.Concurrency != nil {
		if err := ValidateGroupTemplate(w.Concurrency.Group, nil, inputNames, true); err != nil {
			return nil, fmt.Errorf("workflow: %w", err)
		}
		// `cancel-in-progress` may be one expression (PAR-151); a workflow-level
		// value expands once per run, so only github.* and inputs.* resolve.
		if err := validateBoolExpr("workflow: concurrency.cancel-in-progress", w.Concurrency.CancelInProgress,
			TemplateScope{Inputs: inputNames, Site: SiteWorkflowConcurrencyCancel}); err != nil {
			return nil, err
		}
	}
	// Workflow-level env is copied verbatim by the runner (only a step's env
	// values are substituted), so a `${{ … }}` here is a parse error too
	// (docs/WORKFLOWS.md §6.2).
	if err := validateStructuralEnv("env", w.Env); err != nil {
		return nil, err
	}
	if err := validateDefaults("defaults", w.Defaults); err != nil {
		return nil, err
	}
	// `run-name` is a display-only title (PAR-057): github.* and inputs.*
	// resolve; any other context is this file's parse error.
	if err := ValidateTemplate(w.RunName, TemplateScope{Inputs: inputNames, Site: SiteRunName}); err != nil {
		return nil, fmt.Errorf("workflow: run-name: %w", err)
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
		if err := validateDefaults("jobs."+name+".defaults", job.Defaults); err != nil {
			return nil, err
		}
		if err := validateRunsOn(name, job.RunsOn, TemplateScope{
			Axes: strategyAxes(job.Strategy), Inputs: inputNames,
		}); err != nil {
			return nil, err
		}
		// `container.image` joins `runs-on` as a substituted structural field
		// (PAR-030): it is expanded per cell before the job record, so the image
		// gate sees the real tag rather than the authored span.
		if err := validateContainerImage(name, job.Container, TemplateScope{
			Axes: strategyAxes(job.Strategy), Inputs: inputNames,
		}); err != nil {
			return nil, err
		}
		if err := validateTimeout(name, job.TimeoutMinutes, TemplateScope{
			Axes: strategyAxes(job.Strategy), Inputs: inputNames, Site: SiteJobTimeout,
		}); err != nil {
			return nil, err
		}
		// `continue-on-error` may be one expression (PAR-056): it is evaluated
		// per matrix cell at run creation, from the same cell values as the rest
		// of the strategy.
		if err := validateBoolExpr(fmt.Sprintf("workflow: job %s continue-on-error", name), job.ContinueOnError,
			TemplateScope{Axes: strategyAxes(job.Strategy), Inputs: inputNames, Site: SiteJobContinueOnError}); err != nil {
			return nil, err
		}
		reusable := strings.TrimSpace(job.Uses) != ""
		// A call's keys are meaningful only on a `uses:` job: `with:` and
		// `secrets:` on a runner job would be accepted-and-inert values.
		if len(job.With) > 0 && !reusable {
			return nil, fmt.Errorf("workflow: job %s declares `with:` but no `uses:` (a `with:` value is the callee's input; a job that runs steps has none)", name)
		}
		if job.Secrets != nil {
			if !reusable {
				return nil, fmt.Errorf("workflow: job %s declares `secrets:` but no `uses:` (a `secrets:` block passes secrets to a reusable workflow)", name)
			}
			if job.Secrets.Inherit && !isLocalReusableUses(job.Uses) {
				// D-6: a cross-repository callee receives only explicitly
				// listed secrets; `inherit` across a repository boundary is
				// exfiltration by design.
				return nil, fmt.Errorf("workflow: job %s (uses: %s): `secrets: inherit` is refused for a cross-repository callee (decision D-6: a secret never crosses a repository boundary without being listed explicitly); list each secret under `secrets:`", name, job.Uses)
			}
		}
		if reusable {
			if err := ValidateCallWith(job.With, inputNames); err != nil {
				return nil, fmt.Errorf("workflow: job %s: %w", name, err)
			}
		}
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
		scope := TemplateScope{
			Axes: strategyAxes(job.Strategy), Inputs: inputNames,
			SecretNames: callSecretNames(&w), SecretsClosed: w.On.WorkflowCall != nil,
		}
		scope.Site = SiteJobName
		if err := ValidateTemplate(job.Name, scope); err != nil {
			return nil, fmt.Errorf("workflow: job %s name: %w", name, err)
		}
		if err := ValidateIf(job.If, TemplateScope{
			Axes: scope.Axes, Inputs: inputNames, Needs: job.Needs, JobIf: true,
			SecretNames: scope.SecretNames, SecretsClosed: scope.SecretsClosed,
		}); err != nil {
			return nil, fmt.Errorf("workflow: job %s if: %w", name, err)
		}
		stepIDs := map[string]bool{}
		// backgroundIDs names the background steps started so far, in
		// declaration order: a wait/cancel control step may only name one of
		// them (the runner's plan keeps exactly this ordering).
		backgroundIDs := map[string]bool{}
		// declared grows in step order: a step may only read outputs of an
		// earlier step (docs/threads.md §3.9).
		declared := make([]string, 0, len(job.Steps))
		for i, step := range job.Steps {
			if err := checkStepID(name, i, step.ID, stepIDs); err != nil {
				return nil, err
			}
			if err := validateStepExecution(name, i, step, backgroundIDs); err != nil {
				return nil, err
			}
			if err := validateStepFieldRefs(name, i, step, job.Needs, declared); err != nil {
				return nil, err
			}
			if err := validateStepTemplates(name, i, step, TemplateScope{
				Axes: scope.Axes, Inputs: inputNames, Steps: declared, Needs: job.Needs,
				SecretNames: scope.SecretNames, SecretsClosed: scope.SecretsClosed,
				Site: SiteStepField,
			}); err != nil {
				return nil, err
			}
			if step.ID != "" {
				declared = append(declared, strings.ToLower(step.ID))
			}
			// Parallel members are ordinary steps: validate their ids,
			// references and templates too. A member cannot read another
			// member's outputs (they run concurrently and their outputs land
			// after the group), so their ids enter `declared` only after the
			// whole group, for the steps that follow it.
			groupIDs := make([]string, 0, len(step.Parallel))
			for j, member := range step.Parallel {
				if err := checkStepID(name, i, member.ID, stepIDs); err != nil {
					return nil, fmt.Errorf("job %s step %d parallel member %d: %w", name, i+1, j+1, err)
				}
				if err := validateStepFieldRefs(name, i, member, job.Needs, declared); err != nil {
					return nil, fmt.Errorf("job %s step %d parallel member %d: %w", name, i+1, j+1, err)
				}
				if err := validateStepTemplates(name, i, member, TemplateScope{
					Axes: scope.Axes, Inputs: inputNames, Steps: declared, Needs: job.Needs,
					SecretNames: scope.SecretNames, SecretsClosed: scope.SecretsClosed,
					Site: SiteStepField,
				}); err != nil {
					return nil, fmt.Errorf("job %s step %d parallel member %d: %w", name, i+1, j+1, err)
				}
				if member.ID != "" {
					groupIDs = append(groupIDs, strings.ToLower(member.ID))
				}
			}
			declared = append(declared, groupIDs...)
			if step.Background {
				backgroundIDs[strings.ToLower(step.ID)] = true
			}
		}
		if err := validateJobOutputs(name, job, inputNames, scope.Axes); err != nil {
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
			// A job-level expression-valued cancel-in-progress describes the
			// matrix cell, so it reads the same contexts as a job group (PAR-151).
			if err := validateBoolExpr(fmt.Sprintf("workflow: job %s concurrency.cancel-in-progress", name), job.Concurrency.CancelInProgress,
				TemplateScope{Axes: strategyAxes(job.Strategy), Inputs: inputNames, Site: SiteJobConcurrencyCancel}); err != nil {
				return nil, err
			}
		}
	}
	// The callee's output interface is closed here, where every job and its
	// declared outputs are known (PAR-075).
	if err := ValidateCallOutputs(&w); err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}
	if err := checkAcyclic(w.Jobs); err != nil {
		return nil, err
	}
	return &w, nil
}

// ValidateCallWith checks a reusable caller's `with:` values: only `github.*`
// and `inputs.*` resolve there, because the value is expanded when the run is
// created (before any step, matrix, secret or variable exists). Literals are
// carried as written. The importer calls it too, so a foreign `with:` value the
// target cannot expand is refused at import time instead of making the whole
// converted file unparseable.
func ValidateCallWith(with map[string]string, inputNames []string) error {
	names := make([]string, 0, len(with))
	for name := range with {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ValidateTemplate(with[name], TemplateScope{Inputs: inputNames, CallWith: true}); err != nil {
			return fmt.Errorf("with.%s: %w", name, err)
		}
	}
	return nil
}

// isLocalReusableUses reports whether a `uses:` names a file inside this
// repository (`./…`), as opposed to a cross-repository reference. The forms are
// disjoint by construction: a local path must start with `./`.
func isLocalReusableUses(uses string) bool {
	return strings.HasPrefix(strings.TrimSpace(uses), "./")
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

// stepControl reports whether a step is a PAR-080 control step (wait/cancel):
// it runs no process and owns no id.
func stepControl(st Step) bool {
	return strings.TrimSpace(st.Wait) != "" || st.WaitAll || strings.TrimSpace(st.Cancel) != ""
}

// backgroundCapable reports whether the step's authored form can be a
// background step. The runner accepts a run step and a docker step; a local
// `uses: ./…` may be a docker or composite action, and only the action file
// (resolved at run creation) can tell them apart — the runner refuses a
// composite there by name, so the parser accepts the form and leaves that one
// decision to the plan.
func backgroundCapable(st Step) bool {
	if strings.TrimSpace(st.Run) != "" {
		return true
	}
	uses := strings.ToLower(strings.TrimSpace(st.Uses))
	return strings.HasPrefix(uses, "docker://") || strings.HasPrefix(uses, "./")
}

// validateStepExecution checks the PAR-080 execution keys of one workflow step
// against the runner's model (internal/threads/runner/plan.go): a control step
// runs no process and names an earlier background; a parallel group is a
// top-level list of ordinary members; a background step is addressable and is a
// run/docker step. Every refusal is named and fail-closed.
func validateStepExecution(job string, index int, st Step, background map[string]bool) error {
	where := fmt.Sprintf("job %s step %d", job, index+1)
	control := stepControl(st)
	parallel := st.Parallel != nil
	if parallel {
		if control {
			return fmt.Errorf("%w: %s: a parallel group cannot also wait or cancel", ErrStepExecution, where)
		}
		if st.Background {
			return fmt.Errorf("%w: %s: a parallel group cannot itself be background", ErrStepExecution, where)
		}
		if st.Run != "" || st.Uses != "" {
			return fmt.Errorf("%w: %s: a parallel group takes only member steps, not run/uses", ErrStepExecution, where)
		}
		if len(st.Parallel) == 0 {
			return fmt.Errorf("%w: %s: a parallel group has no member steps", ErrStepExecution, where)
		}
		for j, member := range st.Parallel {
			if member.Background || stepControl(member) || member.Parallel != nil {
				return fmt.Errorf("%w: %s parallel member %d: a member is an ordinary run/uses step (background, wait/cancel and nested parallel are refused)", ErrStepExecution, where, j+1)
			}
			switch {
			case member.Run != "" && member.Uses != "":
				return fmt.Errorf("%w: %s parallel member %d", ErrBothStep, where, j+1)
			case member.Run == "" && member.Uses == "":
				return fmt.Errorf("%w: %s parallel member %d", ErrNoStepVerb, where, j+1)
			}
		}
		return nil
	}
	if control {
		wait, cancel := strings.TrimSpace(st.Wait), strings.TrimSpace(st.Cancel)
		if wait != "" && (st.WaitAll || cancel != "") || st.WaitAll && cancel != "" {
			return fmt.Errorf("%w: %s: wait, wait-all and cancel are mutually exclusive", ErrStepExecution, where)
		}
		if strings.TrimSpace(st.Run) != "" || strings.TrimSpace(st.Uses) != "" {
			return fmt.Errorf("%w: %s: a wait/cancel step runs no process, so run/uses is refused", ErrStepExecution, where)
		}
		if strings.TrimSpace(st.ID) != "" {
			return fmt.Errorf("%w: %s: a wait/cancel step runs no process and cannot own an id", ErrStepExecution, where)
		}
		for _, ref := range []string{wait, cancel} {
			ref = strings.TrimSpace(ref)
			if ref == "" || strings.EqualFold(ref, "all") {
				continue
			}
			if !background[strings.ToLower(ref)] {
				return fmt.Errorf("%w: %s: names background step %q, which is not a background step started earlier", ErrStepExecution, where, ref)
			}
		}
		return nil
	}
	// An ordinary (or background) step.
	switch {
	case st.Run != "" && st.Uses != "":
		return fmt.Errorf("%w: %s", ErrBothStep, where)
	case st.Run == "" && st.Uses == "":
		return fmt.Errorf("%w: %s", ErrNoStepVerb, where)
	}
	if st.Background {
		if strings.TrimSpace(st.ID) == "" {
			return fmt.Errorf("%w: %s: a background step needs an id so a later step can wait for or cancel it", ErrStepExecution, where)
		}
		if !backgroundCapable(st) {
			return fmt.Errorf("%w: %s: background is accepted only on a run or docker step", ErrStepExecution, where)
		}
	}
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
	if s.MaxParallel != nil {
		if *s.MaxParallel < 1 || *s.MaxParallel > MaxMatrixCells {
			return fmt.Errorf("workflow: job %s strategy.max-parallel must be 1…%d, got %d", jobName, MaxMatrixCells, *s.MaxParallel)
		}
		if !s.Matrix.Enabled() {
			return fmt.Errorf("workflow: job %s declares strategy.max-parallel without a matrix (it bounds cell admission, so there is nothing to bound)", jobName)
		}
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
	// for an ordinary branch).
	RefBranch RefKind = iota
	// RefTag is a `refs/tags/*` ref.
	RefTag
	// RefMergeQueue is one of the merge queue's read-only refs
	// (`refs/heads/gh-readonly-queue/…`). It is a `refs/heads/*` ref to git,
	// but it is never a push: GitHub starts `merge_group`, not `push`, for it.
	RefMergeQueue
)

// String renders the kind for error messages and records.
func (k RefKind) String() string {
	switch k {
	case RefTag:
		return "tag"
	case RefMergeQueue:
		return "merge-queue"
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
	if kind == RefMergeQueue {
		// A merge-queue ref is never a branch push (GitHub: `merge_group` is
		// separate from `push`), whatever the workflow's filters say.
		return false
	}
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
// "push", "pull_request", "pull_request_target", "workflow_dispatch",
// "repository_dispatch", "workflow_run", "merge_group", "create", "delete",
// "deployment", "deployment_status"; schedule is handled by the scheduler.
// kind names the ref kind (a branch, a tag or a merge-queue ref) and matters
// only for push. ref is the short ref name: the branch name, the tag name, the
// PR target branch, the upstream run's head branch (workflow_run) or the base
// branch a merge group targets (merge_group); it may be empty (no filter
// applies). action is the lifecycle action/type the caller is delivering.
func (w *Workflow) Fires(event string, kind RefKind, ref, action string) bool {
	return w.FiresWithChanges(event, kind, ref, action, nil, false)
}

// FiresWithChanges is Fires plus the changed-file set a path-filtered trigger
// needs. known is false when the caller did not compute a diff; a trigger that
// declares `paths`/`paths-ignore` then does not fire (fail closed).
func (w *Workflow) FiresWithChanges(event string, kind RefKind, ref, action string, changed []string, known bool) bool {
	return w.FiresMatching(Match{
		Event: event, Kind: kind, Ref: ref, Action: action, Changed: changed, ChangedKnown: known,
	})
}

// Match is one event delivery as the trigger selector sees it. It exists
// because workflow_run filters on facts that are not a ref (the upstream
// workflow's name), and widening FiresWithChanges' positional signature would
// make every caller's meaning depend on argument order.
type Match struct {
	Event        string
	Kind         RefKind
	Ref          string
	Action       string
	Changed      []string
	ChangedKnown bool
	// UpstreamWorkflow is the upstream run's workflow display name, read only
	// by on.workflow_run.workflows.
	UpstreamWorkflow string
}

// FiresMatching applies every trigger's filters to one delivery.
func (w *Workflow) FiresMatching(m Match) bool {
	switch m.Event {
	case "push":
		if m.Kind == RefMergeQueue {
			return false // a merge-queue ref is merge_group, never push
		}
		if w.On.Push == nil && !w.On.PushSet {
			return false
		}
		if w.On.Push == nil {
			return true // `push:` with no filters: every branch and tag
		}
		if !w.On.Push.fires(m.Kind, m.Ref) {
			return false
		}
		return matchPaths(w.On.Push.Paths, w.On.Push.PathsIgnore, m.Changed, m.ChangedKnown)
	case "pull_request", "pull_request_target":
		trigger, set := w.On.PullRequest, w.On.PullRequestSet
		if m.Event == "pull_request_target" {
			trigger, set = w.On.PullRequestTarget, w.On.PullRequestTargetSet
		}
		if trigger == nil && !set {
			return false
		}
		if trigger == nil {
			return true // `pull_request:` with no filters: every target branch
		}
		if !matchFilter(trigger.Branches, trigger.BranchesIgnore, m.Ref) {
			return false
		}
		if !matchPRType(trigger.Types, m.Action) {
			return false
		}
		return matchPaths(trigger.Paths, trigger.PathsIgnore, m.Changed, m.ChangedKnown)
	case "release":
		return typedFires(w.On.ReleaseSet, w.On.Release, m.Action)
	case "issues":
		return typedFires(w.On.IssuesSet, w.On.Issues, m.Action)
	case "issue_comment":
		return typedFires(w.On.IssueCommentSet, w.On.IssueComment, m.Action)
	case "pull_request_review":
		return typedFires(w.On.PullRequestReviewSet, w.On.PullRequestReview, m.Action)
	case "pull_request_review_comment":
		return typedFires(w.On.PullRequestReviewCommentSet, w.On.PullRequestReviewComment, m.Action)
	case "label":
		return typedFires(w.On.LabelSet, w.On.Label, m.Action)
	case "milestone":
		return typedFires(w.On.MilestoneSet, w.On.Milestone, m.Action)
	case "check_run":
		return typedFires(w.On.CheckRunSet, w.On.CheckRun, m.Action)
	case "repository_dispatch":
		if w.On.RepositoryDispatch == nil {
			return w.On.RepositoryDispatchSet
		}
		return matchPRType(w.On.RepositoryDispatch.Types, m.Action)
	case "workflow_run":
		if w.On.WorkflowRun == nil {
			return w.On.WorkflowRunSet && strings.EqualFold(m.Action, "completed")
		}
		return w.On.WorkflowRun.fires(m)
	case "merge_group":
		if w.On.MergeGroup == nil {
			// A bare `on: merge_group` means the one emitted activity.
			return w.On.MergeGroupSet && (m.Action == "" || strings.EqualFold(m.Action, "checks_requested"))
		}
		if !matchFilter(w.On.MergeGroup.Branches, w.On.MergeGroup.BranchesIgnore, m.Ref) {
			return false
		}
		return matchPRType(w.On.MergeGroup.Types, m.Action)
	case "create":
		return w.On.CreateSet
	case "delete":
		return w.On.DeleteSet
	case "deployment":
		return w.On.DeploymentSet
	case "deployment_status":
		return w.On.DeploymentStatusSet
	case "fork":
		return w.On.ForkSet
	case "watch":
		return w.On.WatchSet
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

// fires applies the workflow_run filters: the upstream workflow NAME (exact,
// case-insensitive — GitHub's `workflows` is a name list, not a glob), the
// lifecycle type and the upstream head branch.
func (w *WorkflowRunTrigger) fires(m Match) bool {
	// GitHub's default type is `completed` (not "every emitted type"), so an
	// omitted `types:` must NOT also fire on `requested`.
	if len(w.Types) == 0 {
		if !strings.EqualFold(m.Action, "completed") {
			return false
		}
	} else if !matchPRType(w.Types, m.Action) {
		return false
	}
	if len(w.Workflows) > 0 {
		found := false
		for _, name := range w.Workflows {
			if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(m.UpstreamWorkflow)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return matchFilter(w.Branches, w.BranchesIgnore, m.Ref)
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
	if w.On.PullRequestReview != nil || w.On.PullRequestReviewSet {
		out = append(out, "pull_request_review")
	}
	if w.On.PullRequestReviewComment != nil || w.On.PullRequestReviewCommentSet {
		out = append(out, "pull_request_review_comment")
	}
	if w.On.Label != nil || w.On.LabelSet {
		out = append(out, "label")
	}
	if w.On.Milestone != nil || w.On.MilestoneSet {
		out = append(out, "milestone")
	}
	if w.On.CheckRun != nil || w.On.CheckRunSet {
		out = append(out, "check_run")
	}
	if w.On.ForkSet {
		out = append(out, "fork")
	}
	if w.On.WatchSet {
		out = append(out, "watch")
	}
	if w.On.PullRequestTarget != nil || w.On.PullRequestTargetSet {
		out = append(out, "pull_request_target")
	}
	if w.On.RepositoryDispatch != nil || w.On.RepositoryDispatchSet {
		out = append(out, "repository_dispatch")
	}
	if w.On.WorkflowRun != nil || w.On.WorkflowRunSet {
		out = append(out, "workflow_run")
	}
	if w.On.MergeGroup != nil || w.On.MergeGroupSet {
		out = append(out, "merge_group")
	}
	if w.On.CreateSet {
		out = append(out, "create")
	}
	if w.On.DeleteSet {
		out = append(out, "delete")
	}
	if w.On.DeploymentSet {
		out = append(out, "deployment")
	}
	if w.On.DeploymentStatusSet {
		out = append(out, "deployment_status")
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
