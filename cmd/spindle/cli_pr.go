// cli_pr.go — `spindle pr` (11_config_cli.md §6.6): list/show/create/merge/
// review/comment/checks over the shipped pulls + review + checks routes
// (docs/features/03_pull_requests.md §8, 04_code_review.md, 05_checks_statuses.md).
// Merge and update are tasks; --wait polls the documented merge-task poll
// route instead of inventing a synchronous merge.
package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var prGroup = &apiGroup{
	name: "pr",
	help: "pull requests: list, read, write, merge, review, checks",
	commands: []apiCommand{
		{name: "list", usage: "spindle pr list OWNER/REPO [--state open|closed] [--base B] [--head H] [--limit N]",
			help:  "list pull requests",
			flags: []string{"json", "limit", "state", "base", "head", "after"}, run: runPRList},
		{name: "show", usage: "spindle pr show OWNER/REPO NUM",
			help:  "one pull request with its timeline",
			flags: []string{"json", "comments"}, run: runPRShow},
		{name: "create", usage: "spindle pr create OWNER/REPO --title T --base B --head H [--body B|--body-file F] [--draft]",
			help:  "open a pull request",
			flags: []string{"json", "quiet", "title", "body", "body-file", "base", "head", "draft"}, run: runPRCreate},
		{name: "merge", usage: "spindle pr merge OWNER/REPO NUM [--method merge|squash|rebase] [--delete-head] [--title T] [--message M] [--wait]",
			help:  "merge a pull request (a task)",
			flags: []string{"json", "quiet", "method", "delete-head", "title", "message", "wait"}, run: runPRMerge},
		{name: "review", usage: "spindle pr review OWNER/REPO NUM (--approve|--request-changes|--comment) [--body B] [--commit SHA]",
			help:  "submit a review verdict",
			flags: []string{"json", "quiet", "approve", "request-changes", "comment", "body", "body-file", "commit"}, run: runPRReview},
		{name: "comment", usage: "spindle pr comment OWNER/REPO NUM (--body B|--body-file F)",
			help:  "comment on a pull request",
			flags: []string{"json", "quiet", "body", "body-file"}, run: runPRComment},
		{name: "checks", usage: "spindle pr checks OWNER/REPO NUM",
			help:  "combined check state for the PR head",
			flags: []string{"json"}, run: runPRChecks},
	},
}

// prListRow is the list projection (PROut).
type prListRow struct {
	Num       int    `json:"num"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Author    string `json:"author"`
	BaseRef   string `json:"base_ref"`
	HeadRef   string `json:"head_ref"`
	HeadSHA   string `json:"head_sha"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	UpdatedAt string `json:"updated_at"`
}

type prListResponse struct {
	Pulls []prListRow `json:"pulls"`
	More  bool        `json:"more"`
}

// prEndpoint is the base/head endpoint document.
type prEndpoint struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

// prDoc mirrors pr.json.
type prDoc struct {
	Num      int        `json:"num"`
	Kind     string     `json:"kind"`
	Base     prEndpoint `json:"base"`
	Head     prEndpoint `json:"head"`
	Body     string     `json:"body"`
	Merged   bool       `json:"merged"`
	Draft    bool       `json:"draft"`
	Version  int        `json:"version"`
	MergedAt *string    `json:"merged_at"`
	MergedBy *string    `json:"merged_by"`
	MergeSHA *string    `json:"merge_commit_sha"`
	MergeHow *string    `json:"merge_strategy"`
}

// prView is GET …/pulls/{num}.
type prView struct {
	Thread     *issueThread `json:"thread"`
	PR         *prDoc       `json:"pr"`
	HeadLive   string       `json:"head_live_sha"`
	BaseLive   string       `json:"base_live_sha"`
	HeadRefOk  bool         `json:"head_ref_ok"`
	Events     []issueEvent `json:"events"`
	EventsMore bool         `json:"events_more"`
}

// prTask is the task record returned by merge/update-branch (202).
type prTask struct {
	Task *struct {
		ID       string   `json:"id"`
		Kind     string   `json:"kind"`
		Num      int      `json:"num"`
		State    string   `json:"state"`
		Summary  string   `json:"summary"`
		Error    string   `json:"error"`
		Progress []string `json:"progress"`
	} `json:"task"`
}

// checkStatus mirrors one status record.
type checkStatus struct {
	SHA         string `json:"sha"`
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Creator     string `json:"creator"`
	UpdatedAt   string `json:"updated_at"`
}

type checkCombined struct {
	SHA         string         `json:"sha"`
	State       string         `json:"state"`
	TotalCounts map[string]int `json:"total_counts"`
	Statuses    []checkStatus  `json:"statuses"`
}

func runPRList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr list")
	f.add(fs)
	var state, base, head, after string
	fs.StringVar(&state, "state", "", "open|closed")
	fs.StringVar(&base, "base", "", "filter by base branch")
	fs.StringVar(&head, "head", "", "filter by head branch")
	fs.StringVar(&after, "after", "", "pull-number cursor")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "pr list OWNER/REPO")
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
	if base != "" {
		q.Set("base", normalizeRef(base))
	}
	if head != "" {
		q.Set("head", normalizeRef(head))
	}
	if after != "" {
		q.Set("after", after)
	}
	if f.limit > 0 {
		q.Set("n", strconv.Itoa(f.limit))
	}
	var res prListResponse
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "pulls"), q, nil, &res)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "NUM", "STATE", "TITLE", "BASE←HEAD", "AUTHOR", "UPDATED")
	for _, p := range res.Pulls {
		state := p.State
		if p.Merged {
			state = "merged"
		} else if p.Draft {
			state = "draft"
		}
		t.row("#"+strconv.Itoa(p.Num), state, p.Title, shortRef(p.BaseRef)+"←"+shortRef(p.HeadRef), p.Author, p.UpdatedAt)
	}
	t.flush()
	if res.More {
		fmt.Fprintln(c.stderr(), "more results available (use --limit/--after)")
	}
	return exitOK
}

func runPRShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr show")
	f.add(fs)
	var comments int
	fs.IntVar(&comments, "comments", 0, "event window size (server default 50)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "pr show OWNER/REPO NUM")
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
	var view prView
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "pulls", strconv.Itoa(num)), q, nil, &view)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	printPRView(c, view)
	return exitOK
}

func runPRCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr create")
	f.add(fs)
	var title, body, bodyFile, base, head string
	var draft bool
	fs.StringVar(&title, "title", "", "pull-request title (required)")
	fs.StringVar(&body, "body", "", "pull-request body")
	fs.StringVar(&bodyFile, "body-file", "", "read the body from FILE (- = stdin)")
	fs.StringVar(&base, "base", "", "base branch (required)")
	fs.StringVar(&head, "head", "", "head branch (required)")
	fs.BoolVar(&draft, "draft", false, "open as a draft")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "pr create OWNER/REPO --title T --base B --head H")
	if !ok {
		return exitArg
	}
	if strings.TrimSpace(title) == "" || base == "" || head == "" {
		fmt.Fprintln(c.stderr(), "spindle: --title, --base, and --head are required")
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
	req := map[string]any{"title": title, "base_ref": normalizeRef(base), "head_ref": normalizeRef(head), "body": text}
	if draft {
		req["draft"] = true
	}
	var out struct {
		Thread *issueThread `json:"thread"`
		PR     *prDoc       `json:"pr"`
	}
	raw, err := a.callInto(ctx, "POST", apiPath(owner, repo, "pulls"), nil, req, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if out.Thread != nil && !f.quiet {
		fmt.Fprintf(c.stdout(), "Opened #%d %s\n%s\n",
			out.Thread.Num, out.Thread.Title,
			a.base+"/"+owner+"/"+repo+"/pulls/"+strconv.Itoa(out.Thread.Num))
	}
	return exitOK
}

func runPRMerge(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr merge")
	f.add(fs)
	var method, title, message string
	var deleteHead, wait bool
	fs.StringVar(&method, "method", "merge", "merge|squash|rebase")
	fs.BoolVar(&deleteHead, "delete-head", false, "delete the head branch after merging")
	fs.StringVar(&title, "title", "", "merge commit title")
	fs.StringVar(&message, "message", "", "merge commit message")
	fs.BoolVar(&wait, "wait", false, "poll the merge task until it finishes")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "pr merge OWNER/REPO NUM")
	if !ok {
		return exitArg
	}
	switch method {
	case "merge", "squash", "rebase":
	default:
		fmt.Fprintln(c.stderr(), "spindle: --method must be merge, squash, or rebase")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]any{"strategy": method, "delete_head": deleteHead}
	if title != "" {
		body["commit_title"] = title
	}
	if message != "" {
		body["commit_message"] = message
	}
	var out prTask
	raw, err := a.callInto(ctx, "POST", apiPath(owner, repo, "pulls", strconv.Itoa(num), "merge"), nil, body, &out)
	if err != nil {
		return a.fail(err)
	}
	if out.Task == nil {
		if f.json {
			return a.emitJSON(raw)
		}
		return exitOK
	}
	if f.json && !wait {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Merge task %s (%s) started\n", out.Task.ID, out.Task.State)
	}
	if wait {
		code := waitMergeTask(ctx, c, a, owner, repo, num, f)
		if code != exitOK {
			return code
		}
		if f.json {
			return a.emitJSON(raw)
		}
	}
	return exitOK
}

// waitMergeTask polls the documented merge-task projection
// (GET …/pulls/{num}/merge/task) until the task leaves "running".
func waitMergeTask(ctx context.Context, c *cli, a *apiClient, owner, repo string, num int, f *apiFlags) int {
	path := apiPath(owner, repo, "pulls", strconv.Itoa(num), "merge", "task")
	printed := 0
	for {
		var out prTask
		if _, err := a.callInto(ctx, "GET", path, nil, nil, &out); err != nil {
			return a.fail(err)
		}
		if out.Task == nil {
			fmt.Fprintln(c.stderr(), "spindle: merge task disappeared")
			return exitErr
		}
		// Progress is append-only; print only the lines new since the last
		// poll so a long merge does not repeat its history every tick.
		if printed > len(out.Task.Progress) {
			printed = 0
		}
		for _, line := range out.Task.Progress[printed:] {
			if !f.quiet {
				fmt.Fprintf(c.stdout(), "  %s\n", line)
			}
		}
		printed = len(out.Task.Progress)
		switch out.Task.State {
		case "ok":
			if !f.quiet {
				fmt.Fprintf(c.stdout(), "Merged #%d (%s)\n", num, out.Task.Summary)
			}
			return exitOK
		case "error":
			fmt.Fprintf(c.stderr(), "spindle: merge failed: %s\n", out.Task.Error)
			return exitErr
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(c.stderr(), "spindle: %v\n", ctx.Err())
			return exitErr
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func runPRReview(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr review")
	f.add(fs)
	var body, bodyFile, commit string
	var approve, requestChanges, comment bool
	fs.StringVar(&body, "body", "", "review body")
	fs.StringVar(&bodyFile, "body-file", "", "read the body from FILE (- = stdin)")
	fs.StringVar(&commit, "commit", "", "commit sha to review (default: the live head)")
	fs.BoolVar(&approve, "approve", false, "submit APPROVED")
	fs.BoolVar(&requestChanges, "request-changes", false, "submit CHANGES_REQUESTED")
	fs.BoolVar(&comment, "comment", false, "submit COMMENTED")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "pr review OWNER/REPO NUM (--approve|--request-changes|--comment)")
	if !ok {
		return exitArg
	}
	chosen := 0
	for _, v := range []bool{approve, requestChanges, comment} {
		if v {
			chosen++
		}
	}
	if chosen != 1 {
		fmt.Fprintln(c.stderr(), "spindle: pick exactly one of --approve, --request-changes, --comment")
		return exitArg
	}
	state := "APPROVED"
	if requestChanges {
		state = "CHANGES_REQUESTED"
	} else if comment {
		state = "COMMENTED"
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
	if commit == "" {
		// The review binds to a commit; default to the live head (the same
		// value the web UI submits).
		var view prView
		if _, err := a.callInto(ctx, "GET", apiPath(owner, repo, "pulls", strconv.Itoa(num)), nil, nil, &view); err != nil {
			return a.fail(err)
		}
		commit = view.HeadLive
		if commit == "" && view.PR != nil {
			commit = view.PR.Head.SHA
		}
		if commit == "" {
			fmt.Fprintln(c.stderr(), "spindle: cannot determine the head commit; pass --commit SHA")
			return exitErr
		}
	}
	req := map[string]any{"state": state, "body": text, "commit_sha": commit}
	raw, err := a.callInto(ctx, "POST", apiPath(owner, repo, "pulls", strconv.Itoa(num), "reviews"), nil, req, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Submitted %s review on #%d\n", state, num)
	}
	return exitOK
}

func runPRComment(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr comment")
	f.add(fs)
	var body, bodyFile string
	fs.StringVar(&body, "body", "", "comment body")
	fs.StringVar(&bodyFile, "body-file", "", "read the body from FILE (- = stdin)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "pr comment OWNER/REPO NUM --body B")
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
	raw, err := a.call(ctx, "POST", apiPath(owner, repo, "pulls", strconv.Itoa(num), "comments"), nil,
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

func runPRChecks(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "pr checks")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, num, ok := repoAndNum(c, pos, "pr checks OWNER/REPO NUM")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var view prView
	if _, err := a.callInto(ctx, "GET", apiPath(owner, repo, "pulls", strconv.Itoa(num)), nil, nil, &view); err != nil {
		return a.fail(err)
	}
	sha := view.HeadLive
	if sha == "" && view.PR != nil {
		sha = view.PR.Head.SHA
	}
	if sha == "" {
		fmt.Fprintln(c.stderr(), "spindle: the pull request has no head commit")
		return exitErr
	}
	var combined checkCombined
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "checks", sha), nil, nil, &combined)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	printCombinedChecks(c, combined)
	return exitOK
}

// normalizeRef turns a short branch name into the full ref the pulls API
// validates (refs/heads/<name>); an already-full ref passes through.
func normalizeRef(r string) string {
	if strings.HasPrefix(r, "refs/") {
		return r
	}
	return "refs/heads/" + r
}

// shortRef trims refs/heads/ and refs/tags/ for human display.
func shortRef(r string) string {
	r = strings.TrimPrefix(r, "refs/heads/")
	return strings.TrimPrefix(r, "refs/tags/")
}

// printPRView renders the human pull-request view.
func printPRView(c *cli, v prView) {
	if v.Thread == nil {
		return
	}
	th := v.Thread
	state := th.State
	if v.PR != nil && v.PR.Merged {
		state = "merged"
	} else if v.PR != nil && v.PR.Draft {
		state = "draft"
	}
	fmt.Fprintf(c.stdout(), "#%d  %s  %s\n", th.Num, state, th.Title)
	base, head := "", ""
	if v.PR != nil {
		base, head = v.PR.Base.Ref, v.PR.Head.Ref
	}
	fmt.Fprintf(c.stdout(), "by %s · %s←%s · updated %s", th.Author, shortRef(base), shortRef(head), th.UpdatedAt)
	if v.HeadLive != "" {
		fmt.Fprintf(c.stdout(), " · head %s", short12(v.HeadLive))
	}
	fmt.Fprintln(c.stdout())
	if v.PR != nil && strings.TrimSpace(v.PR.Body) != "" {
		fmt.Fprintln(c.stdout())
		fmt.Fprintln(c.stdout(), strings.TrimSpace(v.PR.Body))
	}
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

// printCombinedChecks renders the combined view (05_checks_statuses.md §5).
func printCombinedChecks(c *cli, v checkCombined) {
	fmt.Fprintf(c.stdout(), "%s  %s\n", short12(v.SHA), v.State)
	t := newTable(c.stdout(), "CONTEXT", "STATE", "DESCRIPTION", "UPDATED")
	for _, s := range v.Statuses {
		t.row(s.Context, s.State, s.Description, s.UpdatedAt)
	}
	t.flush()
}
