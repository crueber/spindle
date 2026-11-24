// cli_file.go — `spindle file put|create|delete` (docs/features/17_file_edit.md):
// the browser file editor's one-file-per-commit write path, reachable from the
// CLI. Every verb rides `/{o}/{r}/api/contents` (both lanes via the client's
// lane rewrite), so the gates, guardrails and CAS are exactly the browser's:
// the CLI is a second client, never a second writer.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
)

var fileGroup = &apiGroup{
	name: "file",
	help: "edit one text file as one commit (the browser editor's write path)",
	commands: []apiCommand{
		{name: "put", usage: "spindle file put OWNER/REPO PATH --branch REF --message MSG (--content-file F | --content TEXT)", help: "create or edit a file on a branch", flags: []string{"json", "branch", "message", "description", "content", "content-file", "expect-blob", "expect-branch", "new-branch", "open-pr", "quiet"}, run: runFilePut},
		{name: "create", usage: "spindle file create OWNER/REPO PATH --branch REF --message MSG (--content-file F | --content TEXT)", help: "create a new file (409 when the path exists)", flags: []string{"json", "branch", "message", "description", "content", "content-file", "expect-branch", "new-branch", "open-pr", "quiet"}, run: runFileCreate},
		{name: "delete", usage: "spindle file delete OWNER/REPO PATH --branch REF --message MSG --expect-blob SHA", help: "delete a file (typed-confirm equivalent: expected blob required)", flags: []string{"json", "branch", "message", "description", "expect-blob", "expect-branch", "quiet"}, run: runFileDelete},
	},
}

// fileFlags is one verb's flag set.
type fileFlags struct {
	branch, message, description string
	content, contentFile         string
	expectBlob, expectBranch     string
	newBranch                    string
	openPR                       bool
}

func (ff *fileFlags) add(fs *flag.FlagSet, withContent bool) {
	fs.StringVar(&ff.branch, "branch", "", "branch the edit applies to (required)")
	fs.StringVar(&ff.message, "message", "", "commit message (required)")
	fs.StringVar(&ff.description, "description", "", "extended commit description")
	if withContent {
		fs.StringVar(&ff.content, "content", "", "the file's exact text")
		fs.StringVar(&ff.contentFile, "content-file", "", "read the file text from FILE (- = stdin)")
	}
	fs.StringVar(&ff.expectBlob, "expect-blob", "", "the blob sha the edit rendered from (required when the file exists)")
	fs.StringVar(&ff.expectBranch, "expect-branch", "", "the branch sha the edit rendered from (409 when it moved)")
	fs.StringVar(&ff.newBranch, "new-branch", "", "publish to a new branch instead of moving --branch")
	fs.BoolVar(&ff.openPR, "open-pr", false, "open a pull request for --new-branch")
}

// fileContent resolves --content / --content-file ("" stays empty: an empty
// file is a legitimate edit).
func fileContent(ff *fileFlags) ([]byte, error) {
	switch {
	case ff.content != "" && ff.contentFile != "":
		return nil, fmt.Errorf("use --content or --content-file, not both")
	case ff.contentFile == "-":
		return io.ReadAll(os.Stdin)
	case ff.contentFile != "":
		return os.ReadFile(ff.contentFile)
	case ff.content != "":
		return []byte(ff.content), nil
	}
	return []byte{}, nil
}

// runFileWrite is the shared body of put/create.
func runFileWrite(ctx context.Context, c *cli, args []string, method string, withExpectBlob bool) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "file "+map[string]string{"PUT": "put", "POST": "create"}[method])
	f.add(fs)
	ff := &fileFlags{}
	ff.add(fs, true)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, path, ok := fileTarget(c, pos, "file put OWNER/REPO PATH")
	if !ok {
		return exitArg
	}
	if ff.branch == "" || ff.message == "" {
		fmt.Fprintln(c.stderr(), "spindle: --branch and --message are required")
		return exitArg
	}
	content, err := fileContent(ff)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]any{
		"message":             ff.message,
		"description":         ff.description,
		"content":             base64.StdEncoding.EncodeToString(content),
		"expected_branch_sha": ff.expectBranch,
		"new_branch":          ff.newBranch,
		"open_pr":             ff.openPR,
	}
	if withExpectBlob {
		body["expected_blob_sha"] = ff.expectBlob
	}
	q := url.Values{"ref": {ff.branch}, "path": {path}}
	raw, err := a.call(ctx, method, apiPath(owner, repo, "contents"), q, body)
	if err != nil {
		return a.fail(err)
	}
	return fileEmit(a, f, raw, ff, path)
}

func runFilePut(ctx context.Context, c *cli, args []string) int {
	return runFileWrite(ctx, c, args, "PUT", true)
}

func runFileCreate(ctx context.Context, c *cli, args []string) int {
	return runFileWrite(ctx, c, args, "POST", false)
}

func runFileDelete(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "file delete")
	f.add(fs)
	ff := &fileFlags{}
	ff.add(fs, false)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, path, ok := fileTarget(c, pos, "file delete OWNER/REPO PATH")
	if !ok {
		return exitArg
	}
	if ff.branch == "" || ff.message == "" || ff.expectBlob == "" {
		fmt.Fprintln(c.stderr(), "spindle: --branch, --message and --expect-blob are required")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]any{
		"message":             ff.message,
		"description":         ff.description,
		"expected_blob_sha":   ff.expectBlob,
		"expected_branch_sha": ff.expectBranch,
	}
	q := url.Values{"ref": {ff.branch}, "path": {path}}
	raw, err := a.call(ctx, "DELETE", apiPath(owner, repo, "contents"), q, body)
	if err != nil {
		return a.fail(err)
	}
	return fileEmit(a, f, raw, ff, path)
}

// fileEmit prints the API's JSON (--json) or one confirmation line.
func fileEmit(a *apiClient, f *apiFlags, raw []byte, ff *fileFlags, path string) int {
	if f.json {
		return a.emitJSON(raw)
	}
	if f.quiet {
		return exitOK
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Branch string `json:"branch"`
		PR     *struct {
			Num int `json:"num"`
		} `json:"pr"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Commit.SHA == "" {
		return a.emitJSON(raw)
	}
	target := fmt.Sprintf("%s on %s", short12(out.Commit.SHA), out.Branch)
	if out.PR != nil {
		target += fmt.Sprintf(" (pull request #%d)", out.PR.Num)
	}
	fmt.Fprintf(a.out, "%s: %s\n", path, target)
	return exitOK
}

// fileTarget parses OWNER/REPO plus the file path (the repo may come from
// -R / SPINDLE_REPO, leaving only the path).
func fileTarget(c *cli, pos []string, usage string) (owner, repo, path string, ok bool) {
	pos = c.withRepoSelector(pos, 2)
	if len(pos) != 2 || pos[1] == "" {
		fmt.Fprintf(c.stderr(), "usage: spindle %s\n", usage)
		return "", "", "", false
	}
	owner, repo, ok = splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return "", "", "", false
	}
	return owner, repo, pos[1], true
}
