// help.go — per-command, per-family and per-group help. Dispatch consults
// this before running anything, so a `-h`/`--help` never executes a command:
// no port bind, no version print, no stub error. The tables here are the one
// source for `spindle <cmd> -h` and `spindle help <cmd>`; families are derived
// from the two-token dispatch rows, never listed apart.
package main

import (
	"context"
	"fmt"
	"strings"
)

// commandUsage is the usage line for the commands whose help the dispatcher
// answers itself: the ones that own no FlagSet (serve, version, help) and the
// stubs. Commands that parse flags print their own FlagSet usage, flags
// included, so they are not duplicated here. Keep the stub lines in step with
// helpText and docs/CLI.md §5.1.
var commandUsage = map[string]string{
	"serve":           "serve [--config PATH] [--data-dir PATH]",
	"version":         "version",
	"help":            "help [command|family]",
	"synth":           "synth --out DIR --size s|m|l [--seed N]",
	"mirror":          "mirror --from URL --to URL --dir PATH",
	"wal materialize": "wal materialize REPO --at-seq N --out DIR",
}

// commandStubs marks the surfaces this build lists but does not implement. They
// stay in the table (and answer -h); running one exits 1 with the honest notice.
var commandStubs = map[string]string{
	"synth":           "deterministic synthetic repo via git fast-import",
	"mirror":          "external bridge via a local bare buffer repo",
	"wal materialize": "standalone repo at a point in time",
}

// familyHelp is the one-line summary a family table prints. A family is any
// first token that prefixes two-token dispatch rows (repo, bundle, wal, config).
var familyHelp = map[string]string{
	"repo":   "repositories: registration, listing, manifest stats and the per-repo documents",
	"bundle": "the bundle-uri scheduler: slots, plans, composition and removal",
	"wal":    "the log and pack tools",
	"config": "the config file ⊕ env",
}

// hasHelpFlag reports whether argv asks for help (-h or --help anywhere).
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// stripHelpFlags removes help tokens from argv.
func stripHelpFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-h" || a == "--help" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// helpKey resolves argv to the longest dispatch key: the first token, or the
// first two when that exact two-token row exists.
func helpKey(args []string) string {
	if len(args) >= 2 && !strings.HasPrefix(args[1], "-") {
		if lookup(args[0]+" "+args[1]) != nil {
			return args[0] + " " + args[1]
		}
	}
	return args[0]
}

// groupByName returns the API-client group behind a dispatch row, or nil.
// runner is a group too, but it speaks the runner protocol rather than the
// JSON API, so it lives outside apiGroups (docs/CLI.md §1).
func groupByName(name string) *apiGroup {
	for _, g := range apiGroups {
		if g.name == name {
			return g
		}
	}
	if runnerGroup.name == name {
		return runnerGroup
	}
	return nil
}

// familyRows returns the dispatch rows under a family ("" when none).
func familyRows(family string) []command {
	var rows []command
	for _, cmd := range subcommands {
		if strings.HasPrefix(cmd.name, family+" ") {
			rows = append(rows, cmd)
		}
	}
	return rows
}

// helpTarget answers an explicit help request: args is argv after the globals
// (any help tokens still in it). It reports whether the request was handled and
// the exit code to use.
//
// Families, groups, and the commands that must not run (serve, version, help,
// the stubs) are answered here. Every other command owns a FlagSet, so it
// answers its own -h (usage AND flags) — the same path the group dispatcher
// uses for a group subcommand. `args` keeps the command's own tokens, plus the
// help token, so the FlagSet sees exactly what the operator typed.
func helpTarget(ctx context.Context, c *cli, args []string) (handled bool, code int) {
	topic := stripHelpFlags(args)
	if len(topic) == 0 || strings.HasPrefix(topic[0], "-") {
		return false, 0
	}
	if g := groupByName(topic[0]); g != nil {
		if len(topic) >= 2 && !strings.HasPrefix(topic[1], "-") {
			return false, 0 // group subcommand: its runner prints usage + flags
		}
		printGroupHelp(c, g)
		return true, exitOK
	}
	if cmd := lookup(helpKey(topic)); cmd != nil {
		if !ownsFlagHelp(cmd.name) {
			printCommandUsage(c, cmd)
			return true, exitOK
		}
		consumed := len(strings.Fields(cmd.name))
		return true, cmd.run(ctx, c, args[consumed:])
	}
	if rows := familyRows(topic[0]); len(rows) > 0 {
		printFamilyHelp(c, topic[0], rows)
		return true, exitOK
	}
	return false, 0
}

// ownsFlagHelp reports whether a row's own -h (its FlagSet usage + flags) is
// the right help. serve must not boot, version must not print, help has no
// flags, and a stub must not run — the dispatcher answers those itself.
func ownsFlagHelp(name string) bool {
	switch name {
	case "serve", "version", "help":
		return false
	}
	_, stub := commandStubs[name]
	return !stub
}

// printCommandUsage renders one command's help: what it does, its usage line,
// and (for a stub) that this build does not implement it.
func printCommandUsage(c *cli, cmd *command) {
	w := c.stderr()
	fmt.Fprintf(w, "spindle %s — %s\n\n", cmd.name, cmd.help)
	usage := commandUsage[cmd.name]
	if usage == "" {
		usage = cmd.name + " <subcommand> [flags]"
	}
	fmt.Fprintf(w, "Usage: spindle %s\n", usage)
	if why, ok := commandStubs[cmd.name]; ok {
		fmt.Fprintf(w, "\nNot implemented in this build: %s.\n", why)
		fmt.Fprintln(w, "This command is listed for completeness; running it exits 1.")
	}
	fmt.Fprintf(w, "\nRun \"spindle help\" for the command table.\n")
}

// printFamilyHelp renders a family's subcommand table from the dispatch rows.
func printFamilyHelp(c *cli, family string, rows []command) {
	w := c.stderr()
	summary := familyHelp[family]
	if summary == "" {
		summary = "subcommands"
	}
	fmt.Fprintf(w, "spindle %s — %s\n\n", family, summary)
	fmt.Fprintf(w, "Usage: spindle %s <subcommand> [flags]\n\n", family)
	t := newTable(w)
	for i := range rows {
		help := rows[i].help
		if _, stub := commandStubs[rows[i].name]; stub {
			help += " (stub: not implemented in this build)"
		}
		t.row("  "+strings.TrimPrefix(rows[i].name, family+" "), help)
	}
	t.flush()
	fmt.Fprintf(w, "\nRun \"spindle %s <subcommand> -h\" for flags.\n", family)
}
