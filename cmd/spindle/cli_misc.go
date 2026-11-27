// cli_misc.go — `spindle packages`, `spindle threads`, `spindle api`, and
// `spindle completion` (11_config_cli.md §6.6).
//
// packages rides the served registry API (docs/features/14 §7: GET
// /{o}/{r}/api/packages, DELETE …/packages/tags/{tag}, DELETE …/packages/{digest}),
// so its 404s are real not-found answers from a live endpoint. threads (WP10)
// is served end to end (pass B1/B2): list/show/run read and dispatch runs, and
// cancel/rerun post the operator verbs, so a 404 there means the named run does
// not exist (exit 3).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// --- packages (WP1; docs/features/14_packages.md §6) ----------------------

var packagesGroup = &apiGroup{
	name: "packages",
	help: "OCI packages (the repository's container registry)",
	commands: []apiCommand{
		{name: "list", usage: "spindle packages list OWNER/REPO", help: "list package tags and digests",
			flags: []string{"json"}, run: runPackagesList},
		{name: "show", usage: "spindle packages show OWNER/REPO DIGEST", help: "one package manifest",
			flags: []string{"json"}, run: runPackagesShow},
		{name: "delete", usage: "spindle packages delete OWNER/REPO (--tag T | DIGEST)", help: "delete a tag or digest",
			flags: []string{"json", "tag", "quiet"}, run: runPackagesDelete},
	},
}

func runPackagesList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "packages list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "packages list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "GET", apiPath(owner, repo, "packages"), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var page struct {
		Packages []struct {
			Tag       string   `json:"tag"`
			Digest    string   `json:"digest"`
			Size      int64    `json:"size"`
			Platforms []string `json:"platforms"`
			UpdatedAt string   `json:"updated_at"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "TAG", "DIGEST", "SIZE", "PLATFORMS", "UPDATED")
	for _, p := range page.Packages {
		t.row(p.Tag, short12(p.Digest), strconv.FormatInt(p.Size, 10), strings.Join(p.Platforms, ","), p.UpdatedAt)
	}
	t.flush()
	return exitOK
}

func runPackagesShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "packages show")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, digest, ok := repoAndWord(c, pos, "packages show OWNER/REPO DIGEST")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "GET", apiPath(owner, repo, "packages", digest), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	return a.emitJSON(raw)
}

func runPackagesDelete(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "packages delete")
	f.add(fs)
	var tag string
	fs.StringVar(&tag, "tag", "", "delete by tag instead of digest")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) == 0 {
		if sel := c.repoSelector(); sel != "" {
			pos = []string{sel}
		}
	}
	if len(pos) != 1 && len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle packages delete OWNER/REPO (--tag T | DIGEST)")
		return exitArg
	}
	owner, repo, ok := splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return exitArg
	}
	// Exactly one target: `--tag T` takes just OWNER/REPO, DIGEST takes
	// OWNER/REPO DIGEST. Giving both is usage, not a preference.
	var digest string
	switch {
	case tag != "" && len(pos) == 2:
		fmt.Fprintln(c.stderr(), "spindle: pass --tag T or DIGEST, not both")
		return exitArg
	case tag == "" && len(pos) != 2:
		fmt.Fprintln(c.stderr(), "usage: spindle packages delete OWNER/REPO (--tag T | DIGEST)")
		return exitArg
	case len(pos) == 2:
		digest = pos[1]
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	path := apiPath(owner, repo, "packages", digest)
	if tag != "" {
		path = apiPath(owner, repo, "packages", "tags", tag)
	}
	raw, err := a.call(ctx, "DELETE", path, nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		target := digest
		if tag != "" {
			target = "tag " + tag
		}
		fmt.Fprintf(c.stdout(), "Deleted %s\n", target)
	}
	return exitOK
}

// --- threads (WP10; docs/features/15_threads.md §3) -----------------------
//
// The threads routes are SERVED (pass B1/B2): GET /{o}/{r}/api/threads lists
// runs, POST dispatches {ref, workflow}, GET /{o}/{r}/api/threads/{run} reads
// one, and POST …/{run}/cancel and …/{run}/rerun are the operator verbs the run
// page's buttons use (repo write role, same service calls).

var threadsGroup = &apiGroup{
	name: "threads",
	help: "CI runs (WP10): list, show, dispatch, cancel, rerun",
	commands: []apiCommand{
		{name: "list", usage: "spindle threads list OWNER/REPO [--limit N]", help: "list CI runs",
			flags: []string{"json", "limit"}, run: runThreadsList},
		{name: "show", usage: "spindle threads show OWNER/REPO RUN", help: "one CI run and its jobs",
			flags: []string{"json"}, run: runThreadsShow},
		{name: "run", usage: "spindle threads run OWNER/REPO WORKFLOW --ref REF [--input k=v]... [--priority N]", help: "dispatch a workflow run",
			flags: []string{"json", "quiet", "ref", "input", "priority"}, run: runThreadsRun},
		{name: "watch", usage: "spindle threads watch OWNER/REPO RUN [--interval 2s]", help: "follow a run to its conclusion; exit 0 success, 1 failure, 130 cancelled",
			flags: []string{"json", "interval"}, run: runThreadsWatch},
		{name: "jobs", usage: "spindle threads jobs OWNER/REPO RUN", help: "list one run's jobs and their states",
			flags: []string{"json"}, run: runThreadsJobs},
		{name: "schedule list", usage: "spindle threads schedule list OWNER/REPO", help: "list cron entries with next fire, watermark and switches (PAR-154)",
			flags: []string{"json"}, run: runThreadsScheduleList},
		{name: "schedule enable", usage: "spindle threads schedule enable OWNER/REPO --path FILE --cron CRON [--timezone TZ]", help: "enable an entry without a commit",
			flags: []string{"json", "path", "cron", "timezone"}, run: runThreadsScheduleEnable(true)},
		{name: "schedule disable", usage: "spindle threads schedule disable OWNER/REPO --path FILE --cron CRON [--timezone TZ]", help: "disable an entry without a commit",
			flags: []string{"json", "path", "cron", "timezone"}, run: runThreadsScheduleEnable(false)},
		{name: "schedule run", usage: "spindle threads schedule run OWNER/REPO --path FILE --cron CRON [--timezone TZ]", help: "run an entry now (manual trigger)",
			flags: []string{"json", "path", "cron", "timezone"}, run: runThreadsScheduleRun},
		{name: "pause", usage: "spindle threads pause OWNER/REPO", help: "pause CI: claim nothing new, let in-flight jobs finish (PAR-157)",
			flags: []string{"json", "quiet"}, run: runThreadsPause(true)},
		{name: "resume", usage: "spindle threads resume OWNER/REPO", help: "resume CI after a pause",
			flags: []string{"json", "quiet"}, run: runThreadsPause(false)},
		{name: "queue", usage: "spindle threads queue OWNER/REPO", help: "show the pending window in claim order (priority, then FIFO)",
			flags: []string{"json"}, run: runThreadsQueue},
		{name: "explain", usage: "spindle threads explain FILE", help: "summarise a local workflow: triggers, jobs, runs-on, needs (PAR-196)",
			flags: []string{"json"}, run: runThreadsExplain},
		{name: "cancel", usage: "spindle threads cancel OWNER/REPO RUN", help: "cancel a run (queued jobs now, a running job at its runner's ack)",
			flags: []string{"json", "quiet"}, run: runThreadsCancel},
		{name: "rerun", usage: "spindle threads rerun OWNER/REPO RUN", help: "rerun a run as a new attempt",
			flags: []string{"json", "quiet"}, run: runThreadsRerun},
		// import is operator-local (a checkout, no server): it converts a
		// Forgejo/GitHub workflow into .spindle/workflows (PAR-002). It lives
		// in the group because it is the migration entry point operators look
		// for there; docs/CLI.md §5.3 marks it as needing no API.
		{name: "import", usage: "spindle threads import --from forgejo|github --repo DIR [--path PATH] [--out DIR] [--apply [--branch NAME]] [--json]",
			help:  "convert a Forgejo/GitHub workflow into .spindle/workflows (local)",
			flags: []string{"from", "repo", "path", "out", "apply", "branch", "allow-partial", "json"}, run: runThreadsImport},
	},
}

// threadsPath is the served repo-scoped shape /{o}/{r}/api/threads[/…].
func threadsPath(owner, repo string, segs ...string) string {
	return apiPath(owner, repo, append([]string{"threads"}, segs...)...)
}

func runThreadsList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "threads list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	q := url.Values{}
	if f.limit > 0 {
		q.Set("n", strconv.Itoa(f.limit))
	}
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo), q, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var page struct {
		Runs []struct {
			ID         string `json:"id"`
			Workflow   string `json:"workflow"`
			Event      string `json:"event"`
			State      string `json:"state"`
			Conclusion string `json:"conclusion"`
			Commit     string `json:"commit"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "RUN", "WORKFLOW", "EVENT", "STATE", "COMMIT")
	for _, r := range page.Runs {
		state := r.State
		if r.Conclusion != "" {
			state = r.Conclusion
		}
		t.row(r.ID, r.Workflow, r.Event, state, short12(r.Commit))
	}
	t.flush()
	return exitOK
}

func runThreadsShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads show")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, run, ok := repoAndWord(c, pos, "threads show OWNER/REPO RUN")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	// The route is served: a 404 here is a genuinely unknown run (exit 3).
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo, run), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	return a.emitJSON(raw)
}

func runThreadsRun(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads run")
	f.add(fs)
	var ref string
	var priority int
	var inputs repeatable
	fs.StringVar(&ref, "ref", "", "git ref to run (required: the API resolves it to a commit)")
	fs.Var(&inputs, "input", "workflow_dispatch input k=v (repeatable)")
	fs.IntVar(&priority, "priority", 0, "claim priority 0..10 (higher claims first; PAR-155)")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, workflow, ok := repoAndWord(c, pos, "threads run OWNER/REPO WORKFLOW --ref REF")
	if !ok {
		return exitArg
	}
	if strings.TrimSpace(ref) == "" {
		fmt.Fprintln(c.stderr(), "spindle: --ref is required (the dispatch API resolves it to a commit)")
		return exitArg
	}
	body := map[string]any{"ref": ref, "workflow": workflow}
	if priority != 0 {
		body["priority"] = priority
	}
	if len(inputs) > 0 {
		parsed := map[string]string{}
		for _, raw := range inputs {
			k, v, found := strings.Cut(raw, "=")
			if !found || strings.TrimSpace(k) == "" {
				fmt.Fprintf(c.stderr(), "spindle: --input %q is not k=v\n", raw)
				return exitArg
			}
			parsed[strings.TrimSpace(k)] = v
		}
		body["inputs"] = parsed
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	// The served dispatch endpoint is POST …/api/threads with {ref, workflow}.
	var out struct {
		Runs []struct {
			ID       string `json:"id"`
			Workflow string `json:"workflow"`
			State    string `json:"state"`
		} `json:"runs"`
	}
	raw, err := a.callInto(ctx, "POST", threadsPath(owner, repo), nil, body, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		for _, r := range out.Runs {
			fmt.Fprintf(c.stdout(), "Dispatched %s run %s (%s)\n", r.Workflow, r.ID, r.State)
		}
		if len(out.Runs) == 0 {
			fmt.Fprintf(c.stdout(), "Dispatched %s\n", workflow)
		}
	}
	return exitOK
}

// runThreadsCancel / runThreadsRerun are the operator verbs: POST
// …/api/threads/{run}/cancel and …/rerun (repo write role, the same service
// calls the run page's buttons make). Both answer {"run": Run} — 200 for
// cancel, 201 for rerun. The body carries no per-job detail (jobs are the
// show endpoint's), so the human line reads the returned run state: a
// `cancelled` run took effect on every job, anything else means at least one
// running job is still waiting for its runner to acknowledge the signal.
func runThreadsCancel(ctx context.Context, c *cli, args []string) int {
	return threadsOperatorVerb(ctx, c, args, "cancel")
}

func runThreadsRerun(ctx context.Context, c *cli, args []string) int {
	return threadsOperatorVerb(ctx, c, args, "rerun")
}

func threadsOperatorVerb(ctx context.Context, c *cli, args []string, verb string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads "+verb)
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, run, ok := repoAndWord(c, pos, "threads "+verb+" OWNER/REPO RUN")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var out struct {
		Run struct {
			State   string `json:"state"`
			Attempt int    `json:"attempt"`
		} `json:"run"`
	}
	// No request body: the verb is the URL, and the service owns the work.
	raw, err := a.callInto(ctx, "POST", threadsPath(owner, repo, run, verb), nil, nil, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		if verb == "cancel" {
			switch {
			case out.Run.State == "cancelled":
				fmt.Fprintf(c.stdout(), "Cancelled run %s\n", run)
			case terminalRunState(out.Run.State):
				// Every job was already terminal: the service skips them, so
				// saying "cancel requested" would promise a change that cannot come.
				fmt.Fprintf(c.stdout(), "Run %s is already %s; nothing to cancel\n", run, out.Run.State)
			default:
				fmt.Fprintf(c.stdout(), "Cancel requested for run %s (state: %s); a running job stops when its runner acknowledges\n", run, out.Run.State)
			}
		} else {
			fmt.Fprintf(c.stdout(), "Reran run %s as attempt %d (state: %s)\n", run, out.Run.Attempt, out.Run.State)
		}
	}
	return exitOK
}

// terminalRunState reports the run states that need no further work
// (docs/features/15_threads.md §2 fold rules). The CLI keeps its own copy on
// purpose: it is an API client and must not import the feature package.
func terminalRunState(state string) bool {
	switch state {
	case "success", "failure", "cancelled", "skipped":
		return true
	}
	return false
}

// --- api: the escape hatch (§6.6) -----------------------------------------

func runAPI(ctx context.Context, c *cli, args []string) int {
	fs := newAPIFlagSet(c, "api")
	var method, data string
	var headers repeatable
	var pretty bool
	fs.StringVar(&method, "X", "", "HTTP method (default: GET, or POST with -d)")
	fs.StringVar(&data, "d", "", "request body: literal, @FILE, or - for stdin")
	fs.Var(&headers, "H", "request header 'Key: Value' (repeatable)")
	fs.BoolVar(&pretty, "json", false, "pretty-print a JSON response")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle api [PATH] [-X METHOD] [-d DATA] [-H 'Key: Value']")
		return exitArg
	}
	path := pos[0]
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		path = "/" + path
	}
	if method == "" {
		method = "GET"
		if data != "" {
			method = "POST"
		}
	}

	var body []byte
	if data != "" {
		switch {
		case strings.HasPrefix(data, "@"):
			raw, err := os.ReadFile(data[1:])
			if err != nil {
				fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
				return exitErr
			}
			body = raw
		case data == "-":
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(c.stderr(), "spindle: reading stdin: %v\n", err)
				return exitErr
			}
			body = raw
		default:
			body = []byte(data)
		}
	}

	a, err := newAPIClient(c, nil)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	hdr := http.Header{}
	for _, h := range headers {
		k, v, found := strings.Cut(h, ":")
		if !found {
			fmt.Fprintf(c.stderr(), "spindle: -H %q is not 'Key: Value'\n", h)
			return exitArg
		}
		hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	isJSON := false
	if body != nil {
		isJSON = strings.HasPrefix(hdr.Get("Content-Type"), "application/json")
		if hdr.Get("Content-Type") == "" {
			hdr.Set("Content-Type", "application/json")
			isJSON = true
		}
	}
	var raw []byte
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		raw, err = a.sendAbsolute(ctx, method, path, body, hdr, isJSON)
	} else {
		raw, err = a.send(ctx, method, path, nil, body, hdr)
	}
	if err != nil {
		return a.fail(err)
	}
	// The credential is never printed, even if a proxy or an echo endpoint
	// reflects it: the escape hatch redacts it from the response body too.
	if a.token != "" && strings.Contains(string(raw), a.token) {
		raw = []byte(strings.ReplaceAll(string(raw), a.token, "[REDACTED]"))
	}
	if pretty {
		return a.emitJSON(raw)
	}
	if len(raw) > 0 {
		_, _ = a.out.Write(raw)
		if raw[len(raw)-1] != '\n' {
			fmt.Fprintln(a.out)
		}
	}
	return exitOK
}

// sendAbsolute is send for a caller-supplied absolute URL (the `spindle api`
// escape hatch). Same headers, same exit mapping.
func (a *apiClient) sendAbsolute(ctx context.Context, method, u string, body []byte, hdr http.Header, isJSON bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spindle/"+version())
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range hdr {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s", method, u, a.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil {
		return nil, fmt.Errorf("%s %s: reading response: %s", method, u, a.redact(rerr.Error()))
	}
	if resp.StatusCode >= 400 {
		return raw, &apiError{Method: method, Path: u, Status: resp.StatusCode, Body: a.redact(string(raw))}
	}
	return raw, nil
}

// --- completion -----------------------------------------------------------

func runCompletion(ctx context.Context, c *cli, args []string) int {
	fs := newAPIFlagSet(c, "completion")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle completion bash|zsh|fish")
		return exitArg
	}
	script, err := completionScript(pos[0])
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitArg
	}
	fmt.Fprint(c.stdout(), script)
	return exitOK
}
