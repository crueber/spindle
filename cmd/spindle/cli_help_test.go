// cli_help_test.go — the help contract: every dispatch row and every family
// answers -h/--help with its own usage (never the global table, never an
// "unknown command"), commands that own flags still print those flags, help
// always wins over execution, and the stub commands print their usage instead
// of running their stub.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the entry point for the subprocess help checks: with
// SPINDLE_TEST_MAIN_ARGV set (args joined by unit separators), the test binary
// runs the real main() with that argv instead of the test suite. This is how
// the "serve -h never binds" assertion and the per-command FlagSet help checks
// drive the actual dispatcher (a FlagSet's -h exits the process).
func TestMain(m *testing.M) {
	if argv := os.Getenv("SPINDLE_TEST_MAIN_ARGV"); argv != "" {
		os.Args = append([]string{"spindle"}, strings.Split(argv, "\x1f")...)
		main()
		return
	}
	os.Exit(m.Run())
}

// runMain executes the test binary as the spindle binary with argv.
func runMain(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), "SPINDLE_TEST_MAIN_ARGV="+strings.Join(args, "\x1f"))
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("spindle %s timed out (a help path executed): %s", strings.Join(args, " "), errOut.String())
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run spindle %s: %v", strings.Join(args, " "), err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// runHelp runs argv in-process with a config path that cannot load, so any
// command that actually executed would fail with exit 2. Only intercepted help
// paths are safe in-process; a command that owns a FlagSet exits the process
// from its own parse, so those go through runMain instead.
func dispatchHelp(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{out: &out, errOut: &errOut}
	argv := append([]string{"--config", "/nonexistent/spindle.toml"}, args...)
	code := dispatch(context.Background(), argv, c)
	return out.String() + errOut.String(), code
}

// TestEveryCommandAnswersHelp walks the dispatch table through the real
// binary: every row answers -h and --help with its own help, exits 0, and
// never runs the command (the config path cannot load, so execution would
// fail; serve would bind; version would print; a stub would error).
func TestEveryCommandAnswersHelp(t *testing.T) {
	if len(subcommands) == 0 {
		t.Fatal("empty dispatch table")
	}
	for _, cmd := range subcommands {
		for _, flag := range []string{"-h", "--help"} {
			args := append([]string{"--config", "/nonexistent/spindle.toml"}, strings.Fields(cmd.name)...)
			args = append(args, flag)
			out, errOut, code := runMain(t, nil, args...)
			got := out + errOut
			name := strings.Join(append(strings.Fields(cmd.name), flag), " ")
			if code != exitOK {
				t.Errorf("spindle %s = %d, want 0\n%s", name, code, got)
				continue
			}
			if !strings.Contains(got, "spindle "+cmd.name) {
				t.Errorf("spindle %s does not name the command:\n%s", name, got)
			}
			if strings.Contains(got, "unknown command") {
				t.Errorf("spindle %s printed the unknown-command error:\n%s", name, got)
			}
			if strings.Contains(got, "not yet implemented") {
				t.Errorf("spindle %s ran a stub instead of printing help:\n%s", name, got)
			}
			if _, stub := commandStubs[cmd.name]; stub && !strings.Contains(got, "Not implemented in this build") {
				t.Errorf("spindle %s does not say it is a stub:\n%s", name, got)
			}
			if cmd.name == "version" && strings.Contains(got, "spindle "+version()) {
				t.Errorf("spindle version %s printed the build identity:\n%s", flag, got)
			}
		}
	}
}

// TestLeafHelpKeepsItsFlags is the regression guard for the flag-less table
// line: a command that parses flags must print those flags under -h, not just
// a usage sentence.
func TestLeafHelpKeepsItsFlags(t *testing.T) {
	cases := []struct {
		argv  []string
		flags []string
	}{
		{[]string{"repo", "list", "-h"}, []string{"-json"}},
		{[]string{"bundle", "plan", "-h"}, []string{"-json"}},
		{[]string{"bundle", "run", "-h"}, []string{"-repo", "-strategy"}},
		{[]string{"config", "check", "-h"}, []string{"-env-file", "-strict"}},
		{[]string{"config", "dump", "-h"}, []string{"-env-file"}},
		{[]string{"doctor", "-h"}, []string{"-limit", "-repos", "-budget", "-json"}},
		{[]string{"wal", "ls", "-h"}, []string{"-from", "-to", "-json"}},
		{[]string{"wal", "annotate-pack", "-h"}, []string{"-rev", "-bitmap", "-commit-graph"}},
		{[]string{"import", "-h"}, []string{"-from", "-direct", "-reuse-packs"}},
		{[]string{"api", "-h"}, []string{"-X", "-d", "-H"}},
		{[]string{"search", "-h"}, []string{"-owner", "-repo", "-ref", "-regex", "-case", "-word", "-path", "-limit", "-json"}},
		{[]string{"repo", "policy", "-h"}, []string{"-file"}},
		{[]string{"repo", "settings", "-h"}, []string{"-file", "-m", "-effective"}},
		{[]string{"access", "-h"}, []string{"-file"}},
	}
	for _, tc := range cases {
		out, errOut, code := runMain(t, nil, tc.argv...)
		got := out + errOut
		if code != exitOK {
			t.Errorf("spindle %s = %d, want 0\n%s", strings.Join(tc.argv, " "), code, got)
			continue
		}
		for _, flag := range tc.flags {
			if !strings.Contains(got, flag) {
				t.Errorf("spindle %s: flag %s missing from help:\n%s", strings.Join(tc.argv, " "), flag, got)
			}
			if !strings.Contains(got, "flags:") {
				t.Errorf("spindle %s: no flags block:\n%s", strings.Join(tc.argv, " "), got)
			}
		}
	}
}

// TestInterceptedHelpHasUsage keeps the small table the dispatcher answers
// itself complete: serve, version, help and the stubs.
func TestInterceptedHelpHasUsage(t *testing.T) {
	for _, cmd := range subcommands {
		if groupByName(cmd.name) != nil || ownsFlagHelp(cmd.name) {
			continue // groups render their table; flagged commands render their FlagSet
		}
		if commandUsage[cmd.name] == "" {
			t.Errorf("%q has no commandUsage entry", cmd.name)
		}
	}
}

// TestFamilyHelpListsItsSubcommands pins the family fallback: a first token
// with two-token rows prints those rows, not the global table.
func TestFamilyHelpListsItsSubcommands(t *testing.T) {
	for _, family := range []string{"repo", "bundle", "wal", "config"} {
		got, code := dispatchHelp(t, family, "-h")
		if code != exitOK {
			t.Fatalf("spindle %s -h = %d, want 0", family, code)
		}
		if !strings.Contains(got, "Usage: spindle "+family+" <subcommand>") {
			t.Errorf("spindle %s -h has no family usage line:\n%s", family, got)
		}
		rows := familyRows(family)
		if len(rows) == 0 {
			t.Fatalf("family %q has no rows", family)
		}
		for _, row := range rows {
			sub := strings.TrimPrefix(row.name, family+" ")
			if !strings.Contains(got, "  "+sub) {
				t.Errorf("spindle %s -h does not list %q:\n%s", family, sub, got)
			}
		}
		if strings.Contains(got, "spindle — git over HTTP") {
			t.Errorf("spindle %s -h printed the global table:\n%s", family, got)
		}
	}
}

// TestHelpTopic prints the same help as the command's own flag, flags
// included, and `spindle help` alone stays the global table.
func TestHelpTopic(t *testing.T) {
	got, code := dispatchHelp(t, "help")
	if code != exitOK {
		t.Fatalf("spindle help = %d, want 0", code)
	}
	if !strings.Contains(got, "spindle — git over HTTP") || !strings.Contains(got, "Commands:") {
		t.Fatalf("spindle help is not the global table:\n%s", got)
	}

	repoHelp, code := dispatchHelp(t, "help", "repo")
	if code != exitOK {
		t.Fatalf("spindle help repo = %d, want 0", code)
	}
	if !strings.Contains(repoHelp, "spindle repo —") || !strings.Contains(repoHelp, "  list") {
		t.Fatalf("spindle help repo is not the repo family table:\n%s", repoHelp)
	}
	if strings.Contains(repoHelp, "spindle — git over HTTP") {
		t.Fatalf("spindle help repo printed the global table:\n%s", repoHelp)
	}

	// `spindle help <leaf>` delegates to the same FlagSet path as `<leaf> -h`.
	out, errOut, code := runMain(t, nil, "help", "repo", "list")
	leaf := out + errOut
	if code != exitOK {
		t.Fatalf("spindle help repo list = %d, want 0\n%s", code, leaf)
	}
	if !strings.Contains(leaf, "usage: spindle repo list") || !strings.Contains(leaf, "-json") {
		t.Fatalf("spindle help repo list has no usage + flags:\n%s", leaf)
	}
}

// TestHelpUnknownTopic names the topic and exits 2.
func TestHelpUnknownTopic(t *testing.T) {
	got, code := dispatchHelp(t, "help", "nope")
	if code != exitArg {
		t.Fatalf("spindle help nope = %d, want %d", code, exitArg)
	}
	if !strings.Contains(got, `unknown command "nope"`) {
		t.Fatalf("spindle help nope does not name the topic:\n%s", got)
	}
}

// TestServeHelpDoesNotBind drives the real binary: serve -h must print usage
// and exit 0 even though its listen address is already taken. If it booted the
// server it would fail to bind and exit non-zero.
func TestServeHelpDoesNotBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	out, errOut, code := runMain(t, []string{"SPINDLE__SERVER__LISTEN=" + addr},
		"serve", "-h", "--data-dir", t.TempDir())
	if code != exitOK {
		t.Fatalf("spindle serve -h = %d, want 0 (stdout=%q stderr=%q)", code, out, errOut)
	}
	got := out + errOut
	if !strings.Contains(got, "Usage: spindle serve") {
		t.Fatalf("spindle serve -h printed no usage:\n%s", got)
	}
	if strings.Contains(got, "address already in use") {
		t.Fatalf("spindle serve -h tried to bind %s:\n%s", addr, got)
	}
}

// TestStubCommandsAnswerHelp keeps the stub decision honest: each stub lists
// its usage under -h and still exits 1 with the stub notice when run.
func TestStubCommandsAnswerHelp(t *testing.T) {
	for _, stub := range []string{"synth", "mirror", "wal materialize"} {
		help, code := dispatchHelp(t, append(strings.Fields(stub), "-h")...)
		if code != exitOK {
			t.Errorf("spindle %s -h = %d, want 0", stub, code)
		}
		if !strings.Contains(help, "Usage: spindle "+stub) {
			t.Errorf("spindle %s -h has no usage line:\n%s", stub, help)
		}
		if !strings.Contains(help, "Not implemented in this build") {
			t.Errorf("spindle %s -h does not say it is a stub:\n%s", stub, help)
		}

		var out, errOut bytes.Buffer
		c := &cli{out: &out, errOut: &errOut}
		if code := dispatch(context.Background(), strings.Fields(stub), c); code != exitErr {
			t.Errorf("spindle %s = %d, want %d", stub, code, exitErr)
		}
		want := fmt.Sprintf("spindle: %s: not yet implemented in this build", stub)
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("spindle %s stderr = %q, want %q", stub, errOut.String(), want)
		}
	}
}

// TestAccessHelpIsNotAUsageError covers a single-token command with internal
// subcommands: `access -h` used to parse "-h" as the subcommand and exit 2.
func TestAccessHelpIsNotAUsageError(t *testing.T) {
	got, code := dispatchHelp(t, "access", "-h")
	if code != exitOK {
		t.Fatalf("spindle access -h = %d, want 0:\n%s", code, got)
	}
	if !strings.Contains(got, "usage: spindle access get|put") || !strings.Contains(got, "-file") {
		t.Fatalf("spindle access -h has no usage + flags:\n%s", got)
	}
}
