// cli_runner_test.go — the runner group's HTTP shape (docs/features/15 §3/§6):
// the mint POST, the list GET and the revoke DELETE reach the documented
// endpoints, and the one-time token prints once (never via a URL).
package main

import (
	"strings"
	"testing"
)

func TestRunnerTokenCommand(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/acme/mono/api/threads/runners/tokens", 201,
		`{"id":"abcd1234","token":"wrg_x.abcd1234.secret","expires_at":"2026-11-20T12:15:00Z"}`)
	code, out, errOut := runCmd(t, srv, "tok", runRunnerToken, "acme/mono", "--labels", "linux,docker", "--name", "build-01")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "wrg_x.abcd1234.secret") || !strings.Contains(out, "shown once") {
		t.Fatalf("mint output = %q", out)
	}
	rec := f.last()
	if rec.Method != "POST" || rec.Path != "/acme/mono/api/threads/runners/tokens" {
		t.Fatalf("mint request = %s %s", rec.Method, rec.Path)
	}
	decodeBody(t, rec, map[string]any{"name": "build-01", "labels": "linux,docker"})
	// --json prints the endpoint body verbatim.
	f.on("POST", "/acme/mono/api/threads/runners/tokens", 201,
		`{"id":"abcd1234","token":"t","expires_at":"2026-11-20T12:15:00Z"}`)
	code, out, _ = runCmd(t, srv, "tok", runRunnerToken, "acme/mono", "--json")
	if code != exitOK || !strings.Contains(out, `"token": "t"`) {
		t.Fatalf("json mint = %d %q", code, out)
	}
}

func TestRunnerListAndRevokeCommands(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/acme/mono/api/threads/runners", 200,
		`{"runners":[{"id":"abcd1234","name":"build-01","labels":["linux"],"state":"active","last_seen_at":"2026-11-20T12:00:00Z"}]}`)
	code, out, errOut := runCmd(t, srv, "tok", runRunnerList, "acme/mono")
	if code != exitOK {
		t.Fatalf("list exit = %d (%s)", code, errOut)
	}
	for _, want := range []string{"abcd1234", "build-01", "active", "linux", "2026-11-20T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output %q missing %q", out, want)
		}
	}
	f.on("DELETE", "/acme/mono/api/threads/runners/abcd1234", 204, "")
	code, out, errOut = runCmd(t, srv, "tok", runRunnerRevoke, "acme/mono", "abcd1234")
	if code != exitOK || !strings.Contains(out, "Revoked runner abcd1234") {
		t.Fatalf("revoke = %d %q (%s)", code, out, errOut)
	}
	if rec := f.last(); rec.Method != "DELETE" {
		t.Fatalf("revoke method = %s", rec.Method)
	}
	// An unknown runner surfaces the server's 404 (the CLI's error mapping).
	f.on("DELETE", "/acme/mono/api/threads/runners/ghost", 404, "unknown runner")
	if code, _, _ := runCmd(t, srv, "tok", runRunnerRevoke, "acme/mono", "ghost"); code != exitErr {
		t.Fatalf("unknown revoke exit = %d", code)
	}
	// A malformed OWNER/REPO is an argv error.
	if code, _, _ := runCmd(t, srv, "tok", runRunnerList, "mono"); code != exitArg {
		t.Fatalf("bad repo exit = %d", code)
	}
}
