package threads

// Authoring affordances (PAR-196).
//
// A workflow file could only be judged by pushing it: the parse error appeared
// as a failed run, and an invalid key deep in a matrix job was a line number
// with no path. This file is the server-side validator and explainer behind the
// Threads page's editor pane and `spindle threads explain`:
//
//   - ValidateWorkflowSource parses one file with the SAME parser a push uses and
//     reports every issue with the YAML key path (`jobs.build.steps[0].uses`) and
//     the line, so an author fixes the key the message names before pushing.
//   - ExplainWorkflow summarises what the file will do: triggers, jobs, their
//     `runs-on` and dependencies. It is a read, never an execution.
//
// The validator is not a second parser: it runs workflow.Parse (KnownFields
// strict, the same unmarshalers) and only adds the path/line mapping that
// yaml.v3's TypeError omits, plus a small structural pass for keys the
// hand-written unmarshalers reject with a bare message.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// WorkflowIssue is one validation finding.
type WorkflowIssue struct {
	File string `json:"file"`
	// Key is the YAML key path (`jobs.build.runs-on`); empty when the parser's
	// message cannot be attributed to one node.
	Key     string `json:"key,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// WorkflowValidation is the validator's answer.
type WorkflowValidation struct {
	File   string          `json:"file"`
	OK     bool            `json:"ok"`
	Issues []WorkflowIssue `json:"issues"`
}

// WorkflowJobSummary is one job's author-facing summary.
type WorkflowJobSummary struct {
	Key    string   `json:"key"`
	Name   string   `json:"name,omitempty"`
	RunsOn []string `json:"runs_on"`
	Needs  []string `json:"needs,omitempty"`
	Steps  int      `json:"steps"`
	Uses   string   `json:"uses,omitempty"`
}

// WorkflowExplanation is the explainer's answer.
type WorkflowExplanation struct {
	File     string               `json:"file"`
	Name     string               `json:"name"`
	Triggers []string             `json:"triggers"`
	Jobs     []WorkflowJobSummary `json:"jobs"`
	Valid    bool                 `json:"valid"`
	Error    string               `json:"error,omitempty"`
	IssueKey string               `json:"issue_key,omitempty"`
}

// ValidateWorkflowSource validates one file. file is the display path the
// message names (it is echoed in every issue, so a multi-file editor can say
// which file a key belongs to).
func ValidateWorkflowSource(file string, src []byte) WorkflowValidation {
	out := WorkflowValidation{File: file, Issues: []WorkflowIssue{}}
	_, err := workflow.Parse(src)
	if err == nil {
		out.OK = true
		return out
	}
	lines := yamlPaths(src)
	for _, msg := range splitYAMLErrors(err.Error()) {
		issue := WorkflowIssue{File: file, Message: msg}
		if line, rest := errorLine(msg); line > 0 {
			issue.Line = line
			issue.Message = strings.TrimSpace(rest)
			if p, ok := lines[line]; ok {
				issue.Key = p
			}
		} else {
			// Many hand-written validators name the key in the message; the
			// line is the only structural fact we can add.
			issue.Message = strings.TrimSpace(msg)
			if k := keyFromMessage(issue.Message); k != "" {
				issue.Key = k
			}
		}
		out.Issues = append(out.Issues, issue)
	}
	out.OK = false
	return out
}

// ExplainWorkflow parses and summarises one file. A file that does not parse is
// still explained as far as the parser got (no jobs), with the error and key.
func ExplainWorkflow(file string, src []byte) WorkflowExplanation {
	out := WorkflowExplanation{File: file, Triggers: []string{}, Jobs: []WorkflowJobSummary{}}
	w, err := workflow.Parse(src)
	if err != nil {
		v := ValidateWorkflowSource(file, src)
		out.Error = err.Error()
		if len(v.Issues) > 0 {
			out.IssueKey = v.Issues[0].Key
			if out.IssueKey != "" {
				out.Error = strings.TrimSpace(v.Issues[0].Message)
			}
		}
		return out
	}
	out.Valid = true
	out.Name = w.Name
	if out.Name == "" {
		out.Name = file
	}
	out.Triggers = workflowTriggerNames(w)
	keys := make([]string, 0, len(w.Jobs))
	for k := range w.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		job := w.Jobs[k]
		summary := WorkflowJobSummary{
			Key: k, Name: job.Name, RunsOn: append([]string{}, job.RunsOn...),
			Needs: append([]string{}, job.Needs...), Steps: len(job.Steps), Uses: job.Uses,
		}
		out.Jobs = append(out.Jobs, summary)
	}
	return out
}

// workflowTriggerNames renders the declared triggers (sorted, with their
// filters' names only where a reader needs them to identify the trigger).
func workflowTriggerNames(w *workflow.Workflow) []string {
	if w == nil {
		return []string{}
	}
	var out []string
	add := func(name string, present bool) {
		if present {
			out = append(out, name)
		}
	}
	add("push", w.On.Push != nil || w.On.PushSet)
	add("pull_request", w.On.PullRequest != nil || w.On.PullRequestSet)
	add("pull_request_target", w.On.PullRequestTarget != nil || w.On.PullRequestTargetSet)
	add("workflow_dispatch", w.On.WorkflowDispatch != nil || w.On.DispatchSet)
	add("schedule", w.On.ScheduleSet)
	add("release", w.On.Release != nil || w.On.ReleaseSet)
	add("issues", w.On.Issues != nil || w.On.IssuesSet)
	add("issue_comment", w.On.IssueComment != nil || w.On.IssueCommentSet)
	add("pull_request_review", w.On.PullRequestReview != nil || w.On.PullRequestReviewSet)
	add("pull_request_review_comment", w.On.PullRequestReviewComment != nil || w.On.PullRequestReviewCommentSet)
	add("label", w.On.Label != nil || w.On.LabelSet)
	add("milestone", w.On.Milestone != nil || w.On.MilestoneSet)
	add("check_run", w.On.CheckRun != nil || w.On.CheckRunSet)
	add("fork", w.On.ForkSet)
	add("watch", w.On.WatchSet)
	add("repository_dispatch", w.On.RepositoryDispatch != nil || w.On.RepositoryDispatchSet)
	add("workflow_run", w.On.WorkflowRun != nil || w.On.WorkflowRunSet)
	add("workflow_call", w.On.WorkflowCall != nil || w.On.WorkflowCallSet)
	add("create", w.On.CreateSet)
	add("delete", w.On.DeleteSet)
	add("deployment", w.On.DeploymentSet)
	add("deployment_status", w.On.DeploymentStatusSet)
	sort.Strings(out)
	return out
}

// ValidateWorkflowForRepo is the authenticated validator: any repository reader
// may validate a file before pushing it (the content is the caller's own, and
// nothing is stored).
func (s *Service) ValidateWorkflowForRepo(ctx context.Context, repo string, p auth.Principal, file string, src []byte) (*WorkflowValidation, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	if len(src) > 1<<20 {
		return nil, fmt.Errorf("%w: a workflow file is at most 1 MiB", ErrUnprocessable)
	}
	if strings.TrimSpace(file) == "" {
		file = ".spindle/workflows/untitled.yml"
	}
	v := ValidateWorkflowSource(file, src)
	return &v, nil
}

// yamlPaths maps a line number to the YAML key path at that line.
func yamlPaths(src []byte) map[int]string {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil
	}
	out := map[int]string{}
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n == nil {
			return
		}
		if path != "" {
			out[n.Line] = path
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i], n.Content[i+1]
				child := key.Value
				if path != "" {
					child = path + "." + key.Value
				}
				out[key.Line] = child
				out[val.Line] = child
				walk(val, child)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				child := fmt.Sprintf("%s[%d]", path, i)
				out[c.Line] = child
				walk(c, child)
			}
		}
	}
	walk(&doc, "")
	return out
}

// splitYAMLErrors splits a yaml.TypeError's multi-line message into one issue
// per line.
func splitYAMLErrors(msg string) []string {
	msg = strings.TrimSpace(msg)
	// The parser wraps yaml's multi-line answer ("workflow: yaml: unmarshal
	// errors:\n  line 5: ..."); take everything after "unmarshal errors:".
	if _, rest, ok := strings.Cut(msg, "unmarshal errors:"); ok {
		msg = rest
	} else {
		msg = strings.TrimPrefix(msg, "workflow: ")
		msg = strings.TrimPrefix(msg, "yaml: ")
	}
	var out []string
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = []string{strings.TrimSpace(msg)}
	}
	return out
}

// errorLine extracts "line N:" from one yaml error and returns the remainder.
func errorLine(msg string) (int, string) {
	trimmed := strings.TrimSpace(msg)
	if !strings.HasPrefix(trimmed, "line ") {
		return 0, msg
	}
	rest := strings.TrimPrefix(trimmed, "line ")
	idx := strings.Index(rest, ":")
	if idx < 0 {
		return 0, msg
	}
	var n int
	if _, err := fmt.Sscanf(rest[:idx], "%d", &n); err != nil {
		return 0, msg
	}
	return n, rest[idx+1:]
}

// keyFromMessage pulls a dotted key out of the parser's hand-written messages
// ("permissions.attestations: ..."), so an issue still names its key.
func keyFromMessage(msg string) string {
	// The key is the leading token before ": " when it looks like a path.
	head, _, ok := strings.Cut(msg, ":")
	if !ok {
		return ""
	}
	head = strings.TrimSpace(head)
	head = strings.TrimPrefix(head, "workflow: ")
	if head == "" || strings.ContainsAny(head, " \t") {
		return ""
	}
	for _, r := range head {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '[', r == ']':
		default:
			return ""
		}
	}
	if !strings.ContainsAny(head, ".-[") {
		return ""
	}
	return head
}
