// cli_search.go — `spindle search QUERY` (docs/features/20_search.md §6;
// docs/go/07_api.md §9.13; docs/CLI.md §5.2). A top-level API-client command:
// one GET against the repository lane (/{o}/{r}/api/search) or the owner lane
// (/api/v1/search). It prints the document the API returned — --json hands the
// same envelope back unchanged, never a CLI-owned schema — and maps failures
// through the shared WP11 exit-code contract (401/403 → 4, 404 → 3, else 1).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/search"
)

// searchUsage is the documented syntax (§5.2). It is not in usageByCommand
// because search is a top-level command, not an apiGroup member; the FlagSet
// prints this line plus every flag.
const searchUsage = "spindle search QUERY [--owner OWNER | -R OWNER/REPO] [--ref REF]\n" +
	"       [--regex] [--case] [--word] [--path PATHSPEC]… [--limit N] [--json]"

// runSearch implements `spindle search`. Repository scope is selected by a
// repository (-R, --repo or SPINDLE_REPO); owner scope by --owner. At least one
// is required: v1 has no instance-wide code search (§1), so the command does
// not guess a target.
func runSearch(ctx context.Context, c *cli, args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(c.stderr())
	fs.Usage = func() {
		fmt.Fprintf(c.stderr(), "usage: %s\n\nflags:\n", searchUsage)
		fs.PrintDefaults()
	}

	var (
		owner     string
		repoFlag  string
		ref       string
		regex     bool
		caseSens  bool
		word      bool
		jsonOut   bool
		limit     int
		pathspecs repeatable
	)
	fs.StringVar(&owner, "owner", "", "owner scope: search every readable repository under OWNER")
	fs.StringVar(&repoFlag, "repo", "", "repository scope OWNER/REPO (same as -R)")
	fs.StringVar(&ref, "ref", "", "ref to search in repository scope (default branch when empty)")
	fs.BoolVar(&regex, "regex", false, "treat QUERY as a POSIX extended regular expression")
	fs.BoolVar(&caseSens, "case", false, "match case exactly (default: ASCII case-insensitive)")
	fs.BoolVar(&word, "word", false, "match whole words only")
	fs.BoolVar(&jsonOut, "json", false, "print the API's JSON (stable schema, re-indented)")
	fs.IntVar(&limit, "limit", 0, "kept matches per repository (1..200, default 100)")
	fs.Var(&pathspecs, "path", "restrict the search to a git pathspec (repeatable, at most 8)")

	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintf(c.stderr(), "usage: %s\n", searchUsage)
		return exitArg
	}
	query := pos[0]

	sel := strings.TrimSpace(repoFlag)
	if sel == "" {
		sel = c.repoSelector()
	}
	if sel != "" && strings.TrimSpace(owner) != "" {
		fmt.Fprintln(c.stderr(), "spindle: choose -R/--repo or --owner, not both")
		return exitArg
	}
	if sel == "" && strings.TrimSpace(owner) == "" {
		fmt.Fprintln(c.stderr(), "spindle: search needs -R/--repo OWNER/REPO or --owner OWNER (v1 has no instance-wide code search)")
		return exitArg
	}

	q := url.Values{}
	add := func(k, v string) { q.Add(k, v) }
	add("q", query)
	// The CLI command is the code-search surface (v1 issue search has no CLI
	// flag): pin the type so an owner-scope call cannot silently take the
	// API's issues default and answer the P4 refusal instead of code.
	add("type", "code")
	if ref != "" {
		add("ref", ref)
	}
	if regex {
		add("regex", "1")
	}
	if caseSens {
		add("case", "1")
	}
	if word {
		add("word", "1")
	}
	for _, p := range pathspecs {
		add("path", p)
	}
	if limit > 0 {
		add("limit", strconv.Itoa(limit))
	}

	path := topPath("search")
	if sel == "" {
		add("owner", strings.TrimSpace(owner))
	} else {
		o, r, ok := splitRepo(sel)
		if !ok {
			fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", sel)
			return exitArg
		}
		path = apiPath(o, r, "search")
	}

	a, err := newAPIClient(c, nil)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var ans search.Answer
	raw, err := a.callInto(ctx, "GET", path, q, nil, &ans)
	if err != nil {
		return a.fail(err)
	}
	if jsonOut {
		return a.emitJSON(raw)
	}
	printSearchAnswer(c, &ans)
	return exitOK
}

// printSearchAnswer renders the envelope as the compact human table: one
// header per repository (status and completeness included, so a capped or
// cold answer is visible instead of looking like "no matches"), then one row
// per match. Nothing is synthesised — every number is a wire field.
func printSearchAnswer(c *cli, ans *search.Answer) {
	out := c.stdout()
	if ans.Code != nil {
		for _, r := range ans.Code.Results {
			state := string(r.Status)
			if r.Truncated {
				state += ", truncated"
			}
			if r.TimedOut {
				state += ", timed out"
			}
			fmt.Fprintf(out, "%s @ %s (%s) — %s: %d match(es) in %d file(s)\n",
				r.Repo, shortSHA(r.SHA), r.Ref, state, r.MatchCount, r.FileCount)
			if r.Reason != "" {
				fmt.Fprintf(out, "  %s\n", r.Reason)
			}
			for _, f := range r.Files {
				for _, m := range f.Matches {
					fmt.Fprintf(out, "  %s:%d:%d  %s\n", f.Path, m.Line, m.Column, m.Text)
				}
			}
		}
		fmt.Fprintf(out, "%d repositories searched (%d considered)\n",
			ans.Code.ReposSearched, ans.Code.ReposConsidered)
		if !ans.Code.Complete {
			fmt.Fprintln(out, "partial results: some repositories were capped, timed out or not warm")
		}
	}
	if ans.Issues != nil {
		for _, hit := range ans.Issues.Hits {
			fmt.Fprintf(out, "%s #%d (%s) %s — matched %s\n",
				hit.Repo, hit.Num, hit.Kind, hit.Title, hit.Matched)
		}
		fmt.Fprintf(out, "%d thread(s) scanned; events %d\n",
			ans.Issues.Scanned.Threads, ans.Issues.Scanned.Events)
		if !ans.Issues.Complete {
			fmt.Fprintln(out, "partial issue results: the scan was capped")
		}
	}
}

// shortSHA abbreviates a resolved object id for the human header (the JSON
// body keeps the full 40/64-hex value).
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
