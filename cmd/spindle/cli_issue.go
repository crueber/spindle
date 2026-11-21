// cli_issue.go — `spindle issue` (11_config_cli.md §6.6): list/show/create/
// comment/close/reopen over the shipped /{owner}/{repo}/api/issues surface
// (docs/features/02_issues.md §7). Labels, assignees, and milestones ride
// the same PATCH the web UI uses — the CLI forks no write path.
package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var issueGroup = &apiGroup{
	name: "issue",
	help: "issues: list, read, and write",
	commands: []apiCommand{
		{name: "list", usage: "spindle issue list OWNER/REPO [--state open|closed] [--label L]… [--assignee A] [--milestone M] [--limit N]",
			help:  "list issues",
			flags: []string{"json", "quiet", "limit", "state", "label", "assignee", "milestone", "after"}, run: runIssueList},
		{name: "show", usage: "spindle issue show OWNER/REPO NUM [--comments N]",
			help:  "one issue with its event timeline",
			flags: []string{"json", "comments"}, run: runIssueShow},
		{name: "create", usage: "spindle issue create OWNER/REPO --title T [--body B|--body-file F] [--label L]… [--assignee A]… [--milestone M]",
			help:  "open an issue",
			flags: []string{"json", "quiet", "title", "body", "body-file", "label", "assignee", "milestone"}, run: runIssueCreate},
		{name: "comment", usage: "spindle issue comment OWNER/REPO NUM (--body B|--body-file F)",
			help:  "add a comment",
			flags: []string{"json", "quiet", "body", "body-file"}, run: runIssueComment},
		{name: "close", usage: "spindle issue close OWNER/REPO NUM [--reason completed|not_planned]",
			help:  "close an issue",
			flags: []string{"json", "quiet", "reason"}, run: runIssueClose},
		{name: "reopen", usage: "spindle issue reopen OWNER/REPO NUM",
			help:  "reopen a closed issue",
			flags: []string{"json", "quiet"}, run: runIssueReopen},
	},
}

// issueCard is the list projection (docs/features/02_issues.md §7).
type issueCard struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	Author       string   `json:"author"`
	UpdatedAt    string   `json:"updated_at"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    *string  `json:"milestone"`
	CommentCount int      `json:"comment_count"`
}

type issueListResponse struct {
	Issues []issueCard `json:"issues"`
	More   bool        `json:"more"`
}

// issueEvent is one timeline entry.
type issueEvent struct {
	Seq   int     `json:"seq"`
	Type  string  `json:"type"`
	Actor string  `json:"actor"`
	At    string  `json:"at"`
	Body  *string `json:"body,omitempty"`
}

// issueThread mirrors the thread document.
type issueThread struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	StateReason  *string  `json:"state_reason"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    *string  `json:"milestone"`
	CommentCount int      `json:"comment_count"`
}

type issueView struct {
	Thread     *issueThread `json:"thread"`
	Events     []issueEvent `json:"events"`
	EventsMore bool         `json:"events_more"`
}

func runIssueList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue list")
	f.add(fs)
	var state, assignee, milestone, after string
	var labels repeatable
	fs.StringVar(&state, "state", "", "open|closed (default: all)")
	fs.Var(&labels, "label", "filter by label (repeatable, AND)")
	fs.StringVar(&assignee, "assignee", "", "assignee login or *none")
	fs.StringVar(&milestone, "milestone", "", "milestone id or none")
	fs.StringVar(&after, "after", "", "issue-number cursor")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "issue list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if len(labels) > 0 {
		q.Set("labels", strings.Join(labels, ","))
	}
	if assignee != "" {
		q.Set("assignee", assignee)
	}
	if milestone != "" {
		q.Set("milestone", milestone)
	}
	if after != "" {
		q.Set("after", after)
	}
	if f.limit > 0 {
		q.Set("n", strconv.Itoa(f.limit))
	}
	var res issueListResponse
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "issues"), q, nil, &res)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "NUM", "STATE", "TITLE", "AUTHOR", "LABELS", "UPDATED")
	for _, is := range res.Issues {
		t.row("#"+strconv.Itoa(is.Num), is.State, is.Title, is.Author,
			strings.Join(is.Labels, ","), is.UpdatedAt)
	}
	t.flush()
	if res.More {
		fmt.Fprintln(c.stderr(), "more results available (use --limit/--after)")
	}
	return exitOK
}

func runIssueShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue show")
	f.add(fs)
	var comments int
	fs.IntVar(&comments, "comments", 0, "event window size (server default 50)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "issue show OWNER/REPO NUM")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	q := url.Values{}
	if comments > 0 {
		q.Set("n", strconv.Itoa(comments))
	}
	var view issueView
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "issues", strconv.Itoa(num)), q, nil, &view)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	printIssueView(c, view)
	return exitOK
}

func runIssueCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue create")
	f.add(fs)
	var title, body, bodyFile, milestone string
	var labels, assignees repeatable
	fs.StringVar(&title, "title", "", "issue title (required)")
	fs.StringVar(&body, "body", "", "issue body")
	fs.StringVar(&bodyFile, "body-file", "", "read the body from FILE (- = stdin)")
	fs.Var(&labels, "label", "label to apply (repeatable)")
	fs.Var(&assignees, "assignee", "assignee login (repeatable)")
	fs.StringVar(&milestone, "milestone", "", "milestone id")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "issue create OWNER/REPO --title T")
	if !ok {
		return exitArg
	}
	if strings.TrimSpace(title) == "" {
		fmt.Fprintln(c.stderr(), "spindle: --title is required")
		return exitArg
	}
	text, exit := readBodyFlag(c, body, bodyFile)
	if exit != exitOK {
		return exit
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var created struct {
		Thread *issueThread `json:"thread"`
	}
	raw, err := a.callInto(ctx, "POST", apiPath(owner, repo, "issues"), nil,
		map[string]string{"title": title, "body": text}, &created)
	if err != nil {
		return a.fail(err)
	}
	// Labels/assignees/milestone are a second PATCH — the create body accepts
	// only {title, body} (02_issues.md §7), so the CLI must not pretend.
	if len(labels) > 0 || len(assignees) > 0 || milestone != "" {
		patch := map[string]any{}
		if len(labels) > 0 {
			patch["labels"] = []string(labels)
		}
		if len(assignees) > 0 {
			patch["assignees"] = []string(assignees)
		}
		if milestone != "" {
			patch["milestone"] = milestone
		}
		if created.Thread == nil {
			fmt.Fprintln(c.stderr(), "spindle: server returned no issue number; metadata not applied")
			return exitErr
		}
		num := strconv.Itoa(created.Thread.Num)
		raw, err = a.callInto(ctx, "PATCH", apiPath(owner, repo, "issues", num), nil, patch, &created)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: issue #%s was created, but the metadata update failed\n", num)
			return a.fail(err)
		}
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if created.Thread != nil && !f.quiet {
		fmt.Fprintf(c.stdout(), "Opened #%d %s\n%s\n",
			created.Thread.Num, created.Thread.Title,
			a.base+"/"+owner+"/"+repo+"/issues/"+strconv.Itoa(created.Thread.Num))
	}
	return exitOK
}

func runIssueComment(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue comment")
	f.add(fs)
	var body, bodyFile string
	fs.StringVar(&body, "body", "", "comment body")
	fs.StringVar(&bodyFile, "body-file", "", "read the body from FILE (- = stdin)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "issue comment OWNER/REPO NUM --body B")
	if !ok {
		return exitArg
	}
	text, exit := readBodyFlag(c, body, bodyFile)
	if exit != exitOK {
		return exit
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(c.stderr(), "spindle: --body or --body-file is required")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "POST", apiPath(owner, repo, "issues", strconv.Itoa(num), "comments"), nil,
		map[string]string{"body": text})
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Commented on #%d\n", num)
	}
	return exitOK
}

func runIssueClose(ctx context.Context, c *cli, args []string) int {
	return issueSetState(ctx, c, args, "close")
}

func runIssueReopen(ctx context.Context, c *cli, args []string) int {
	return issueSetState(ctx, c, args, "reopen")
}

// issueSetState implements close/reopen as the documented PATCH: state is
// "closed"/"open", optional state_reason on close.
func issueSetState(ctx context.Context, c *cli, args []string, verb string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue "+verb)
	f.add(fs)
	var reason string
	if verb == "close" {
		fs.StringVar(&reason, "reason", "", "completed|not_planned")
	}
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "issue "+verb+" OWNER/REPO NUM")
	if !ok {
		return exitArg
	}
	patch := map[string]any{"state": "closed"}
	if verb == "reopen" {
		patch["state"] = "open"
	} else if reason != "" {
		if reason != "completed" && reason != "not_planned" {
			fmt.Fprintln(c.stderr(), "spindle: --reason must be completed or not_planned")
			return exitArg
		}
		patch["state_reason"] = reason
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var out struct {
		Thread *issueThread `json:"thread"`
	}
	raw, err := a.callInto(ctx, "PATCH", apiPath(owner, repo, "issues", strconv.Itoa(num)), nil, patch, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		done := "closed"
		if verb == "reopen" {
			done = "reopened"
		}
		fmt.Fprintf(c.stdout(), "#%d %s\n", num, done)
	}
	return exitOK
}

// printIssueView renders the human issue view.
func printIssueView(c *cli, v issueView) {
	if v.Thread == nil {
		return
	}
	th := v.Thread
	fmt.Fprintf(c.stdout(), "#%d  %s  %s\n", th.Num, th.State, th.Title)
	fmt.Fprintf(c.stdout(), "by %s · updated %s", th.Author, th.UpdatedAt)
	if len(th.Labels) > 0 {
		fmt.Fprintf(c.stdout(), " · labels: %s", strings.Join(th.Labels, ", "))
	}
	if len(th.Assignees) > 0 {
		fmt.Fprintf(c.stdout(), " · assignees: %s", strings.Join(th.Assignees, ", "))
	}
	if th.Milestone != nil && *th.Milestone != "" {
		fmt.Fprintf(c.stdout(), " · milestone: %s", *th.Milestone)
	}
	fmt.Fprintln(c.stdout())
	if len(v.Events) > 0 {
		fmt.Fprintln(c.stdout())
	}
	for _, ev := range v.Events {
		line := fmt.Sprintf("  [%d] %s %s", ev.Seq, ev.Actor, ev.Type)
		if ev.Body != nil && strings.TrimSpace(*ev.Body) != "" {
			line += ": " + strings.TrimSpace(*ev.Body)
		}
		fmt.Fprintln(c.stdout(), line)
	}
	if v.EventsMore {
		fmt.Fprintln(c.stderr(), "more events available (use --comments N)")
	}
}

// oneRepo parses exactly one OWNER/REPO positional.
func oneRepo(c *cli, pos []string, usage string) (string, string, bool) {
	if len(pos) != 1 {
		fmt.Fprintf(c.stderr(), "usage: spindle %s\n", usage)
		return "", "", false
	}
	owner, repo, ok := splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return "", "", false
	}
	return owner, repo, true
}

// repoAndNum parses OWNER/REPO plus an issue/PR number.
func repoAndNum(c *cli, pos []string, usage string) (string, string, int, bool) {
	if len(pos) != 2 {
		fmt.Fprintf(c.stderr(), "usage: spindle %s\n", usage)
		return "", "", 0, false
	}
	owner, repo, ok := splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return "", "", 0, false
	}
	num, err := strconv.Atoi(pos[1])
	if err != nil || num < 1 {
		fmt.Fprintf(c.stderr(), "spindle: %q is not a number\n", pos[1])
		return "", "", 0, false
	}
	return owner, repo, num, true
}
