// cli_misc.go — `spindle packages`, `spindle threads`, `spindle api`, and
// `spindle completion` (11_config_cli.md §6.6).
//
// packages and threads are defined against their DOCUMENTED paths (WP1 /
// WP10 are not landed). A 404 from those surfaces is reported as "feature
// not enabled" (exit 1) — never the misleading not-found exit 3 — so the
// command surface ships now and starts working the day the server route
// lands, with no CLI change.
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
	help: "OCI packages (WP1; endpoints documented, not yet served)",
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
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
		return a.notEnabled(err, "packages")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var page struct {
		Packages []struct {
			Tag    string `json:"tag"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "TAG", "DIGEST", "SIZE")
	for _, p := range page.Packages {
		t.row(p.Tag, short12(p.Digest), strconv.FormatInt(p.Size, 10))
	}
	t.flush()
	return exitOK
}

func runPackagesShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "packages show")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
		return a.notEnabled(err, "packages")
	}
	return a.emitJSON(raw)
}

func runPackagesDelete(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "packages delete")
	f.add(fs)
	var tag string
	fs.StringVar(&tag, "tag", "", "delete by tag instead of digest")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, digest, ok := repoAndWord(c, pos, "packages delete OWNER/REPO (--tag T | DIGEST)")
	if !ok {
		return exitArg
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
		return a.notEnabled(err, "packages")
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

// --- threads (WP10; docs/features/15_threads.md §6) -----------------------

var threadsGroup = &apiGroup{
	name: "threads",
	help: "CI runs (WP10; endpoints documented, not yet served)",
	commands: []apiCommand{
		{name: "list", usage: "spindle threads list OWNER/REPO [--limit N]", help: "list CI runs",
			flags: []string{"json", "limit"}, run: runThreadsList},
		{name: "show", usage: "spindle threads show OWNER/REPO RUN", help: "one CI run and its jobs",
			flags: []string{"json"}, run: runThreadsShow},
		{name: "run", usage: "spindle threads run OWNER/REPO WORKFLOW [--ref R]", help: "dispatch a workflow run",
			flags: []string{"json", "quiet", "ref"}, run: runThreadsRun},
		{name: "cancel", usage: "spindle threads cancel OWNER/REPO RUN", help: "cancel a running workflow",
			flags: []string{"json", "quiet"}, run: runThreadsCancel},
	},
}

// threads path constants: the documented repo-scoped shape, so the commands
// bind the day WP10 registers its routes.
func threadsPath(owner, repo string, segs ...string) string {
	return apiPath(owner, repo, append([]string{"threads"}, segs...)...)
}

func runThreadsList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads list")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var page struct {
		Runs []struct {
			ID         string `json:"id"`
			Workflow   string `json:"workflow"`
			Event      string `json:"event"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Commit     string `json:"commit"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "RUN", "WORKFLOW", "EVENT", "STATUS", "COMMIT")
	for _, r := range page.Runs {
		t.row(r.ID, r.Workflow, r.Event, r.Status, short12(r.Commit))
	}
	t.flush()
	return exitOK
}

func runThreadsShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads show")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo, run), nil, nil)
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	return a.emitJSON(raw)
}

func runThreadsRun(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads run")
	f.add(fs)
	var ref string
	fs.StringVar(&ref, "ref", "", "git ref to run (default: the repo's default branch)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, workflow, ok := repoAndWord(c, pos, "threads run OWNER/REPO WORKFLOW")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "POST", threadsPath(owner, repo, workflow, "runs"), nil,
		map[string]string{"ref": ref})
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Dispatched %s\n", workflow)
	}
	return exitOK
}

func runThreadsCancel(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads cancel")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, run, ok := repoAndWord(c, pos, "threads cancel OWNER/REPO RUN")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "POST", threadsPath(owner, repo, run, "cancel"), nil, nil)
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Cancel requested for run %s\n", run)
	}
	return exitOK
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
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
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
