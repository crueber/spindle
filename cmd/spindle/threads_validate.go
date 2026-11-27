// threads_validate.go — `spindle threads validate`, offline workflow
// validation (PAR-199, docs/features/21_parity_master.md §4.11,
// docs/threads.md §2.3).
//
// The verb is operator-local: it reads a file, a directory or stdin on this
// host and needs no server, token or data dir. It runs the SAME validator the
// server's authoring pane uses (threads.ValidateWorkflowSource, which runs the
// push parser), so the error text an author sees here is byte-identical to the
// server's. `threads explain` stays the neighbouring verb: it summarises a
// file's triggers/jobs, while validate answers "does this file parse, and if
// not, which key on which line".
//
// Contract (docs/CLI.md §5.3):
//
//	spindle threads validate [PATH|-] [--repo DIR] [--json]
//
//	PATH      one workflow file, a directory of *.yml/*.yaml files, or "-" for
//	          stdin
//	--repo    validate DIR/.spindle/workflows (a checkout) instead of PATH
//	--json    the machine report
//
// Exit: 0 every file valid, 1 at least one invalid (or an unreadable target),
// 2 usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads"
)

// validateWorkflowPath is one file the verb checks.
type validateWorkflowPath struct {
	// Path is what the report names (the argument as written, or the joined
	// repository-relative path).
	Path string
	Src  []byte
}

// threadsValidateReport is the `--json` answer: one entry per file, each the
// server validator's own shape (file, ok, issues with key/line/message).
type threadsValidateReport struct {
	OK    bool                         `json:"ok"`
	Files []threads.WorkflowValidation `json:"files"`
}

func runThreadsValidate(ctx context.Context, c *cli, args []string) int {
	_ = ctx // validation is a pure local read; kept for the dispatch signature
	fs := flag.NewFlagSet("threads validate", flag.ContinueOnError)
	fs.SetOutput(c.stderr())
	var (
		repoDir string
		jsonOut bool
	)
	fs.StringVar(&repoDir, "repo", "", "validate DIR/.spindle/workflows instead of PATH")
	fs.BoolVar(&jsonOut, "json", false, "print a machine-readable report")
	fs.Usage = func() {
		w := c.stderr()
		fmt.Fprintln(w, "usage: spindle threads validate [PATH|-] [--repo DIR] [--json]")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Validate a Spindle workflow offline with the server's own parser.")
		fmt.Fprintln(w, "PATH is one file, a directory of *.yml/*.yaml files, or - for stdin.")
		fmt.Fprintln(w, "--repo DIR validates DIR/.spindle/workflows (no server needed).")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Exit: 0 every file valid, 1 at least one invalid or unreadable, 2 usage.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "flags:")
		fs.PrintDefaults()
	}
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	switch {
	case len(pos) > 1:
		fmt.Fprintf(c.stderr(), "spindle threads validate: unexpected argument %q\n\n", pos[1])
		fs.Usage()
		return exitArg
	case len(pos) == 1 && repoDir != "":
		fmt.Fprintln(c.stderr(), "spindle threads validate: PATH and --repo are mutually exclusive")
		return exitArg
	case len(pos) == 0 && repoDir == "":
		fmt.Fprintln(c.stderr(), "spindle threads validate: a PATH or --repo is required")
		fs.Usage()
		return exitArg
	}

	var targets []validateWorkflowPath
	var err error
	if repoDir != "" {
		targets, err = validateRepoDir(repoDir)
	} else if pos[0] == "-" {
		var raw []byte
		raw, err = io.ReadAll(c.stdin())
		if err == nil {
			targets = []validateWorkflowPath{{Path: "-", Src: raw}}
		}
	} else {
		targets, err = validateTarget(pos[0])
	}
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle threads validate: %v\n", err)
		return exitErr
	}

	report := threadsValidateReport{OK: true, Files: make([]threads.WorkflowValidation, 0, len(targets))}
	for _, target := range targets {
		res := threads.ValidateWorkflowSource(target.Path, target.Src)
		report.Files = append(report.Files, res)
		if !res.OK {
			report.OK = false
		}
	}

	if jsonOut {
		out, merr := json.MarshalIndent(report, "", "  ")
		if merr != nil {
			fmt.Fprintf(c.stderr(), "spindle threads validate: %v\n", merr)
			return exitErr
		}
		fmt.Fprintln(c.stdout(), string(out))
	} else {
		printThreadsValidateReport(c, report)
	}
	if !report.OK {
		return exitErr
	}
	return exitOK
}

// validateTarget expands one positional PATH: a workflow file, or a directory
// of direct *.yml/*.yaml children. A directory with no workflow file is an
// error (there would be nothing to validate and exit 0 would be a lie).
func validateTarget(path string) ([]validateWorkflowPath, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, rerr
		}
		return []validateWorkflowPath{{Path: path, Src: raw}}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowFileName(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no workflow files (*.yml, *.yaml) in %s", path)
	}
	out := make([]validateWorkflowPath, 0, len(names))
	for _, name := range names {
		full := filepath.Join(path, name)
		raw, rerr := os.ReadFile(full)
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, validateWorkflowPath{Path: full, Src: raw})
	}
	return out, nil
}

// validateRepoDir validates a checkout's `.spindle/workflows` directory — the
// one location Threads discovers (PAR-001), so an operator checks exactly what
// a push would.
func validateRepoDir(dir string) ([]validateWorkflowPath, error) {
	workflows := filepath.Join(dir, ".spindle", "workflows")
	if _, err := os.Stat(workflows); err != nil {
		return nil, fmt.Errorf("%s: %v (pass the repository root, or a PATH)", workflows, err)
	}
	return validateTarget(workflows)
}

// isWorkflowFileName reports whether name has a workflow extension.
func isWorkflowFileName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yml" || ext == ".yaml"
}

// printThreadsValidateReport writes the human answer: one `ok` line per valid
// file, and one `file:line: key: message` line per issue on an invalid one, so
// an editor can jump to the line and a shell can grep it.
func printThreadsValidateReport(c *cli, report threadsValidateReport) {
	invalid := 0
	for _, file := range report.Files {
		if file.OK {
			fmt.Fprintf(c.stdout(), "%s: ok\n", file.File)
			continue
		}
		invalid++
		for _, issue := range file.Issues {
			fmt.Fprintln(c.stdout(), validateIssueLine(issue))
		}
	}
	if len(report.Files) > 1 {
		fmt.Fprintf(c.stdout(), "%d file(s): %d invalid\n", len(report.Files), invalid)
	}
}

// validateIssueLine renders one issue as `file:line: key: message`, dropping
// the parts the parser could not attribute.
func validateIssueLine(issue threads.WorkflowIssue) string {
	var b strings.Builder
	b.WriteString(issue.File)
	if issue.Line > 0 {
		fmt.Fprintf(&b, ":%d", issue.Line)
	}
	b.WriteString(": ")
	if issue.Key != "" {
		b.WriteString(issue.Key)
		b.WriteString(": ")
	}
	b.WriteString(issue.Message)
	return b.String()
}
