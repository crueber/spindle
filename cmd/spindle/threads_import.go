// threads_import.go — `spindle threads import`, the one-way workflow importer
// (PAR-002, docs/features/21_parity_master.md §2.3, security decision S14).
//
// It is operator-local even though it lives in the `threads` group: it reads a
// checkout on disk and needs no server, no token and no data dir. Read-only by
// default (converted files go to stdout); --out writes them under a directory;
// --apply commits them on a NEW branch and never pushes or forces.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/threads/importer"
)

func runThreadsImport(ctx context.Context, c *cli, args []string) int {
	fs := flag.NewFlagSet("threads import", flag.ContinueOnError)
	fs.SetOutput(c.stderr())
	var (
		from, repo, srcPath, out, branch string
		apply, allowPartial, jsonOut     bool
	)
	fs.StringVar(&from, "from", "", "source platform: forgejo or github (required)")
	fs.StringVar(&repo, "repo", "", "repository directory to read (required)")
	fs.StringVar(&srcPath, "path", "", "one source file or directory (default: the platform's workflow directory)")
	fs.StringVar(&out, "out", "", "write the converted files under DIR (repo-relative layout)")
	fs.BoolVar(&apply, "apply", false, "commit the converted files on a new branch")
	fs.StringVar(&branch, "branch", "", "branch name for --apply (default spindle/import-<platform>-<timestamp>)")
	fs.BoolVar(&allowPartial, "allow-partial", false, "allow --apply when constructs are untranslated")
	fs.BoolVar(&jsonOut, "json", false, "print a machine-readable report")
	fs.Usage = func() {
		w := c.stderr()
		fmt.Fprintln(w, "usage: spindle threads import --from forgejo|github --repo DIR [--path PATH] [--out DIR] [--apply [--branch NAME] [--allow-partial]] [--json]")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Convert .forgejo/workflows or .github/workflows into .spindle/workflows.")
		fmt.Fprintln(w, "Read-only by default; --apply commits on a new branch and never pushes.")
		fmt.Fprintln(w, "A construct that cannot be translated is marked `# UNTRANSLATED:` and exits 1.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Exit: 0 fully translated, 1 untranslated constructs or an error, 2 usage.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitArg
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(c.stderr(), "spindle threads import: unexpected argument %q\n\n", fs.Args()[0])
		fs.Usage()
		return exitArg
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(repo) == "" {
		fmt.Fprintln(c.stderr(), "spindle threads import: --from and --repo are required")
		fs.Usage()
		return exitArg
	}
	platform, err := importer.ParsePlatform(from)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle threads import: %v\n", err)
		return exitArg
	}
	if apply && out != "" {
		fmt.Fprintln(c.stderr(), "spindle threads import: --apply and --out are mutually exclusive")
		return exitArg
	}
	rep, err := importer.Run(ctx, importer.Options{
		Repo:         repo,
		Platform:     platform,
		Path:         srcPath,
		Out:          out,
		Apply:        apply,
		Branch:       branch,
		AllowPartial: allowPartial,
	})
	contentOnStdout := rep != nil && !jsonOut && !apply && out == ""
	if jsonOut && rep != nil {
		if enc, merr := json.MarshalIndent(rep, "", "  "); merr == nil {
			fmt.Fprintln(c.stdout(), string(enc))
		}
	} else if contentOnStdout {
		// A dry run prints the converted files themselves.
		for _, f := range rep.Files {
			fmt.Fprintf(c.stdout(), "# ==> %s (from %s)\n", f.Output, f.Source)
			_, _ = c.stdout().Write(rep.Outputs[f.Output])
			fmt.Fprintln(c.stdout())
		}
	}
	if rep != nil && !jsonOut {
		printImportReport(c, rep, contentOnStdout)
	}
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if !rep.OK() {
		fmt.Fprintf(c.stderr(), "spindle: %d untranslated construct(s): review the report above; the converted files still write\n", rep.Untranslated())
		return exitErr
	}
	return exitOK
}

// printImportReport writes the human summary. When the converted YAML was
// already printed to stdout, the summary goes to stderr so a pipe stays clean.
func printImportReport(c *cli, rep *importer.Report, contentOnStdout bool) {
	w := c.stdout()
	if contentOnStdout {
		w = c.stderr()
	}
	fmt.Fprintf(w, "%s: %d file(s)\n", rep.Platform, len(rep.Files))
	for _, f := range rep.Files {
		fmt.Fprintf(w, "  %s -> %s (%d translated, %d untranslated)\n",
			f.Source, f.Output, f.Translated, len(f.Untranslated))
		for _, item := range f.Untranslated {
			fmt.Fprintf(w, "    UNTRANSLATED %s: %s\n", item.Path, item.Reason)
		}
		for _, item := range f.Notes {
			fmt.Fprintf(w, "    note %s: %s\n", item.Path, item.Reason)
		}
	}
	if rep.Applied {
		fmt.Fprintf(w, "committed on branch %s (not pushed)\n", rep.Branch)
	}
}
