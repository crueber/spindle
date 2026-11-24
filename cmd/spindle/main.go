// Package main is the spindle binary: global --config/--data-dir flags, the
// subcommand dispatcher (a hand-rolled table, 11_config_cli.md §6.2), build
// identity, and the §6.1 exit codes (0 ok, 1 command/config error, 2 argv /
// missing explicit config, 3 config check --strict with ignored overrides).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
)

// buildSHA is linker-injected at build time (-X main.buildSHA=$(SHA)).
var buildSHA = "dev"

// Exit codes (normative, §6.1).
const (
	exitOK     = 0 // success
	exitErr    = 1 // command/config error (bad flag value, validation, strategy not found…)
	exitArg    = 2 // argv errors + a missing explicitly-named config file
	exitStrict = 3 // config check --strict observed ignored overrides
)

// version resolves the build identity (§6.1): linker-injected buildSHA → VCS
// build info (first 12 hex chars) → "dev".
func version() string {
	if buildSHA != "" && buildSHA != "dev" {
		return buildSHA
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "dev"
}

// command is one dispatch-table row; two-token subcommands resolve through the
// flat table ("bundle run", "wal ls", …).
type command struct {
	name string // full dispatch key: "serve", "bundle run", …
	help string
	run  func(ctx context.Context, c *cli, args []string) int
}

// cli carries the peeled globals to every subcommand.
type cli struct {
	configPath string    // explicit --config or the env pointer ("" = default location)
	dataDir    string    // explicit --data-dir ("" = resolve from env/defaults)
	server     string    // explicit --server for the API commands ("" = env/state/default)
	repo       string    // explicit -R OWNER/REPO ("" = SPINDLE_REPO/env or the positional)
	out        io.Writer // stdout override for tests (nil = os.Stdout)
	errOut     io.Writer // stderr override for tests (nil = os.Stderr)
}

// stdout/stderr are the command output streams (test-injectable).
func (c *cli) stdout() io.Writer {
	if c != nil && c.out != nil {
		return c.out
	}
	return os.Stdout
}

func (c *cli) stderr() io.Writer {
	if c != nil && c.errOut != nil {
		return c.errOut
	}
	return os.Stderr
}

var subcommands []command

// init builds the dispatch table. It is an init (not a var initializer)
// because the table holds runCompletion, which reads the table back to
// generate shell completion — a var-initializer cycle.
func init() {
	subcommands = []command{
		{"serve", "run the spindle server (the default)", runServe},
		{"compact", "run one maintenance pass / loop the maintainer", runCompact},
		{"bundle run", "run bundle slots due now (--repo ID, --strategy N)", runBundleRun},
		{"bundle plan", "print the slot table for a repo", runBundlePlan},
		{"bundle compose", "build one bundle slot now", runBundleCompose},
		{"bundle rm", "remove bundle entries by id", runBundleRm},
		{"repo create", "register a new repository", runRepoCreate},
		{"repo list", "list repositories", runRepoList},
		{"repo info", "print manifest stats for a repo", runRepoInfo},
		{"repo policy", "read/save/clear the repo policy document", runRepoPolicy},
		{"repo settings", "per-repo settings (D24) surface", runRepoSettings},
		{"access", "read/replace the repo access.json role bindings", runAccess},
		{"wal ls", "log table for a repo", runWalLs},
		{"wal show", "one log entry, full", runWalShow},
		{"wal materialize", "build a standalone repo at a point in time", runWalMaterialize},
		{"wal add-pack", "publish an existing pack as a COMPACT entry", runWalAddPack},
		{"wal annotate-pack", "retrofit side files via manifest-only CAS", runWalAnnotatePack},
		{"wal rev-index", "write a .rev from an .idx", runWalRevIndex},
		{"synth", "deterministic synthetic repo via git fast-import", runSynth},
		{"import", "import a git directory (--from) or a git URL (--url)", runImport},
		{"mirror", "external bridge via a local bare buffer repo", runMirror},
		{"config check", "validate file ⊕ env, report ignored overrides", runConfigCheck},
		{"config dump", "print the effective config as TOML", runConfigDump},
		{"doctor", "read-only upgrade readiness check (config + bounded bucket sample)", runDoctor},
		{"version", "print the build identity", runVersion},
		// WP11: the API client surface (11_config_cli.md §6.6).
		{"auth", "log in/out, inspect the stored token", groupRunner(authGroup)},
		{"user", "users and SSH keys", groupRunner(userGroup)},
		{"org", "organizations, members, teams", groupRunner(orgGroup)},
		{"issue", "issues: list/show/create/comment/close/reopen", groupRunner(issueGroup)},
		{"pr", "pull requests: list/show/create/merge/review/comment/checks", groupRunner(prGroup)},
		{"release", "releases and assets", groupRunner(releaseGroup)},
		{"checks", "commit checks and statuses", groupRunner(checksGroup)},
		{"packages", "OCI packages (WP1)", groupRunner(packagesGroup)},
		{"threads", "CI runs (WP10)", groupRunner(threadsGroup)},
		{"file", "edit one text file as one commit (the browser editor's path)", groupRunner(fileGroup)},
		{"runner", "Threads runners: serve jobs", groupRunner(runnerGroup)},
		{"api", "one authenticated API request", runAPI},
		{"completion", "generate shell completion (bash|zsh|fish)", runCompletion},
		{"help", "print this help", runHelp},
	}
}

// main dispatches: peel the globals, resolve the subcommand (two-token
// subcommands consume a second token), run, map errors onto exit codes.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &cli{}
	rest := peelGlobals(os.Args[1:], c)
	if len(rest) == 0 {
		os.Exit(runServe(ctx, c, nil))
	}
	// Two-token commands first (bundle/wal/repo/config consume a second token).
	if len(rest) >= 2 {
		if cmd := lookup(rest[0] + " " + rest[1]); cmd != nil {
			os.Exit(cmd.run(ctx, c, rest[2:]))
		}
	}
	if cmd := lookup(rest[0]); cmd != nil {
		os.Exit(cmd.run(ctx, c, rest[1:]))
	}
	fmt.Fprintf(os.Stderr, "spindle: unknown command %q\n\n", rest[0])
	fmt.Fprint(os.Stderr, helpText)
	os.Exit(exitArg)
}

func lookup(name string) *command {
	for i := range subcommands {
		if subcommands[i].name == name {
			return &subcommands[i]
		}
	}
	return nil
}

func runVersion(ctx context.Context, c *cli, args []string) int {
	fmt.Println("spindle " + version())
	return exitOK
}

// runHelp prints the command table (the "spindle help" the usage text names).
func runHelp(ctx context.Context, c *cli, args []string) int {
	fmt.Fprint(c.stdout(), helpText)
	return exitOK
}

// usageText mirrors the help table above (kept separate so help stays stable).
var usageText = `Commands:
  serve | compact | bundle | repo | access | wal | synth | import | mirror | config | doctor | version
Run "spindle help" or "spindle <command> -h" for details.
`

// peelGlobals removes --config PATH / --data-dir PATH / --server URL / -R REPO
// from anywhere in argv (last wins) into c. A `-h`/`--help` BEFORE the first
// command token prints the global table; after one it is left for that
// command's own FlagSet, which prints that command's usage and flags.
func peelGlobals(argv []string, c *cli) []string {
	var rest []string
	seenCommand := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "--help", "-h":
			if !seenCommand {
				fmt.Print(helpText)
				os.Exit(exitOK)
			}
			rest = append(rest, arg)
		case "--config", "--data-dir", "--server", "-R":
			if i+1 >= len(argv) {
				fmt.Fprintf(os.Stderr, "spindle: %s requires a value\n", arg)
				os.Exit(exitArg)
			}
			switch arg {
			case "--config":
				c.configPath = argv[i+1]
			case "--data-dir":
				c.dataDir = argv[i+1]
			case "-R":
				c.repo = argv[i+1]
			default:
				c.server = argv[i+1]
			}
			i++
		default:
			if v, ok := cutFlag(arg, "--config="); ok {
				c.configPath = v
			} else if v, ok := cutFlag(arg, "--data-dir="); ok {
				c.dataDir = v
			} else if v, ok := cutFlag(arg, "--server="); ok {
				c.server = v
			} else if v, ok := cutFlag(arg, "-R="); ok {
				c.repo = v
			} else {
				rest = append(rest, arg)
				if !strings.HasPrefix(arg, "-") {
					seenCommand = true
				}
			}
		}
	}
	return rest
}

func cutFlag(arg, prefix string) (string, bool) {
	if len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
		return arg[len(prefix):], true
	}
	return "", false
}

const helpText = `spindle — git over HTTP on a WAL engine

Usage: spindle [--config PATH] [--data-dir PATH] [--server URL] [-R OWNER/REPO] <command>

Commands:
  serve                                  run the server (default)
  compact [REPO|--all] [--once] [--base] maintenance compaction pass
  bundle run [--repo ID] [--strategy N]  build due bundle slots
  bundle plan REPO                       print the slot table
  bundle compose REPO [--strategy N]     build one slot now
  bundle rm REPO IDS…                    remove bundle entries
  repo create REPO [--object-format F]   register a repository
  repo list                              list repositories
  repo info REPO                         manifest stats
   repo policy get|set --file F|clear REPO
   repo settings show|set|clear|history REPO
   access get REPO | access put --file F REPO
  wal ls REPO [--from N] [--to N]        log table
  wal show REPO SEQ                      one entry, full
  wal materialize REPO --at-seq N --out DIR
  wal add-pack REPO PACK [--tier N]
  wal annotate-pack REPO CHECKSUM [--rev] [--bitmap] [--commit-graph]
  wal rev-index IDX [--out P]
  synth --out DIR --size s|m|l [--seed N]
  import [--direct] --from GITDIR owner/name
  import --url URL owner/name [--ref REF]… [--default-branch-only]
      [--include-pull-heads] [--include-notes] [--token-env VAR]
      [--format sha1|sha256] [--dangerous]
  mirror --from URL --to URL --dir PATH
  config check [--env-file PATH]… [--strict]
  config dump
  doctor [--limit N] [--repos N] [--budget N] [--json]
  version

API client (WP11; --json everywhere, exit 0 ok · 1 error · 2 usage · 3 not found · 4 auth):
  auth login|logout|status|token         credential for the JSON API
  user whoami|list|show|keys list|add|remove
  org list|show|create · org member list|add|remove · org team list|create|add-repo
  issue list|show|create|comment|close|reopen
  pr list|show|diff|create|close|merge|review|comment|checks
  release list|show|create|delete|upload
  checks list|status REPO [REF]
  packages list|show|delete · threads list|show|run · runner serve
  api [-X METHOD] [-d DATA] [-H 'K: V'] PATH
  completion bash|zsh|fish
  help

Every subcommand answers '--help' with its own usage and flags.
Repo: the OWNER/REPO positional, or -R OWNER/REPO / SPINDLE_REPO.
Server/token: --server URL, SPINDLE_SERVER (default http://127.0.0.1:8080),
SPINDLE_TOKEN, else the "spindle auth login" state file (0600).

Exit codes: 0 success · 1 command/config/API error · 2 argv error or missing
explicit config file · 3 config check --strict observed ignored overrides (and,
for API commands, 3 = not found) · 4 authentication/authorization failed.
`
