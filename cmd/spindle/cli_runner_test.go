// cli_runner_test.go — the runner group's surface: `serve`'s image-flag
// validation/help, and the deliberate absence of the admin verbs (the CLI's
// machine credentials cannot administer; administration is browser-only).
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunnerServeImageFlagValidation(t *testing.T) {
	// A bad image policy is a startup usage error that names its flag, before
	// the runner dials anything (docs/threads.md §8.1).
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"scheme", []string{"--image-prefix", "https://ak.example/dockerhub"}, "--image-prefix"},
		{"trailing slash", []string{"--image-prefix", "ak.example/dockerhub/"}, "--image-prefix"},
		{"no path", []string{"--image-prefix", "ak.example"}, "--image-prefix"},
		{"prefix-all alone", []string{"--image-prefix-all"}, "--image-prefix-all"},
		{"bad pull", []string{"--image-pull", "sometimes"}, "--image-pull"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := isolateCLI(t, "http://127.0.0.1:1")
			var out, errOut bytes.Buffer
			c.out, c.errOut = &out, &errOut
			code := runRunnerServe(t.Context(), c, tc.args)
			if code != exitArg {
				t.Fatalf("exit = %d, want %d (%s)", code, exitArg, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("stderr %q misses %q", errOut.String(), tc.want)
			}
		})
	}
}

func TestRunnerGroupHelpListsImageFlags(t *testing.T) {
	// The apiGroup's flag list is what `--help` renders: a flag missing there
	// is a flag the operator cannot discover.
	flags := ""
	for _, cmd := range runnerGroup.commands {
		if cmd.name == "serve" {
			flags = strings.Join(cmd.flags, ",")
		}
	}
	for _, want := range []string{"image-prefix", "image-prefix-all", "image-pull", "image-require-digest"} {
		if !strings.Contains(flags, want) {
			t.Fatalf("runner serve help flags %q miss %q", flags, want)
		}
	}
}

func TestRunnerAdminVerbsAreGone(t *testing.T) {
	// Administration is OIDC/browser-only (AGENTS law 9): the CLI's machine
	// credentials cannot mint, list or revoke. The runner-authenticated fleet
	// verbs (drain/resume/rotate/deregister) are different: they use the runner's
	// OWN credential, so they are present and never a 403-only admin command.
	names := map[string]bool{}
	for _, cmd := range runnerGroup.commands {
		names[cmd.name] = true
	}
	for _, want := range []string{"serve", "drain", "resume", "rotate", "deregister", "images", "pool"} {
		if !names[want] {
			t.Fatalf("runner group is missing %q (have %v)", want, names)
		}
	}
	for _, verb := range []string{"token", "list", "revoke"} {
		if names[verb] {
			t.Fatalf("dead admin verb %q is back", verb)
		}
	}
	// The group dispatcher treats an admin verb as a group-help request (exit
	// arg), never as an operation: nothing dials the server.
	c := isolateCLI(t, "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := groupRunner(runnerGroup)(t.Context(), c, []string{"token"}); code != exitArg {
		t.Fatalf("runner token exit = %d, want %d", code, exitArg)
	}
	if !strings.Contains(errOut.String(), `unknown subcommand "token"`) || !strings.Contains(errOut.String(), "serve") {
		t.Fatalf("group help = %q", errOut.String())
	}
}
