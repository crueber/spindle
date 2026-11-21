// cli_client_test.go — the WP11 contract tests that need no HTTP server:
// server/token resolution, the 0600 state file, the exit-code mapping,
// argv permutation (flags after positionals), token redaction, and shell
// completion generated from the same command table dispatch reads.
package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateCLI pins the environment every API command reads: a throwaway
// config dir, the test server URL, and no ambient token.
func isolateCLI(t *testing.T, server string) *cli {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPINDLE_CONFIG_DIR", dir)
	t.Setenv("SPINDLE_SERVER", server)
	t.Setenv("SPINDLE_TOKEN", "")
	t.Setenv("WALGIT_TOKEN", "")
	return &cli{}
}

func TestNormalizeServer(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080", false},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"https://git.example.com/spindle/", "https://git.example.com/spindle", false},
		{"ftp://example.com", "", true},
		{"", "", true},
		{"http://", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeServer(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeServer(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeServer(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{&apiError{Status: http.StatusUnauthorized}, exitAuth},
		{&apiError{Status: http.StatusForbidden}, exitAuth},
		{&apiError{Status: http.StatusNotFound}, exitNotFound},
		{&apiError{Status: http.StatusBadRequest}, exitErr},
		{&apiError{Status: http.StatusConflict}, exitErr},
		{&apiError{Status: http.StatusServiceUnavailable}, exitErr},
		{errors.New("dial tcp: connection refused"), exitErr},
	}
	for _, tc := range cases {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestStateFileRoundTripAndMode(t *testing.T) {
	isolateCLI(t, "http://example.invalid")
	st := loginState{Version: 1, Current: "http://example.invalid", Servers: map[string]loginEntry{
		"http://example.invalid": {Token: "wgt_secret", Principal: "alice", Write: true},
	}}
	if err := saveState(st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	info, err := os.Stat(statePath())
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state mode = %o, want 600", perm)
	}
	got, err := loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if got.Servers["http://example.invalid"].Token != "wgt_secret" || got.Current != "http://example.invalid" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestStateFileMalformedIsAnError(t *testing.T) {
	isolateCLI(t, "http://example.invalid")
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(); err == nil {
		t.Fatal("loadState accepted malformed JSON")
	}
}

func TestTokenResolutionPrecedence(t *testing.T) {
	isolateCLI(t, "http://a.invalid")
	st := loginState{Servers: map[string]loginEntry{"http://a.invalid": {Token: "state-token"}}}
	if tok, src := resolveToken("http://a.invalid", st); tok != "state-token" || src != "state" {
		t.Fatalf("state token = %q/%q", tok, src)
	}
	t.Setenv("SPINDLE_TOKEN", "env-token")
	if tok, src := resolveToken("http://a.invalid", st); tok != "env-token" || src != "env" {
		t.Fatalf("env token = %q/%q", tok, src)
	}
	t.Setenv("SPINDLE_TOKEN", "")
	t.Setenv("WALGIT_TOKEN", "legacy-token")
	if tok, src := resolveToken("http://a.invalid", st); tok != "legacy-token" || src != "env" {
		t.Fatalf("legacy token = %q/%q", tok, src)
	}
}

func TestResolveServerFlagBeatsEnvBeatsState(t *testing.T) {
	isolateCLI(t, "http://env.invalid")
	st := loginState{Current: "http://state.invalid", Servers: map[string]loginEntry{}}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	c := &cli{}
	got, _, err := resolveServer(c)
	if err != nil || got != "http://env.invalid" {
		t.Fatalf("env: got %q, %v", got, err)
	}
	t.Setenv("SPINDLE_SERVER", "")
	c.server = "http://flag.invalid"
	got, _, err = resolveServer(c)
	if err != nil || got != "http://flag.invalid" {
		t.Fatalf("flag: got %q, %v", got, err)
	}
	c.server = ""
	got, _, err = resolveServer(c)
	if err != nil || got != "http://state.invalid" {
		t.Fatalf("state: got %q, %v", got, err)
	}
}

func TestParseAPIAllowsFlagsAfterPositionals(t *testing.T) {
	c := &cli{errOut: &bytes.Buffer{}}
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "issue list")
	f.add(fs)
	var labels repeatable
	var state string
	fs.Var(&labels, "label", "")
	fs.StringVar(&state, "state", "", "")
	pos, ok := parseAPI(fs, []string{"acme/repo", "--state", "open", "--label=bug", "--label", "ui", "--json"})
	if !ok {
		t.Fatal("parseAPI failed")
	}
	if len(pos) != 1 || pos[0] != "acme/repo" {
		t.Fatalf("positionals = %v", pos)
	}
	if !f.json || state != "open" || len(labels) != 2 || labels[0] != "bug" || labels[1] != "ui" {
		t.Fatalf("flags = %+v state=%q labels=%v", f, state, labels)
	}
}

func TestTokenRedactionInErrors(t *testing.T) {
	const token = "wgt_supersecret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream echoed "+token, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := isolateCLI(t, srv.URL)
	t.Setenv("SPINDLE_TOKEN", token)
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	code := runAPI(context.Background(), c, []string{"/api/v1/me"})
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if strings.Contains(errOut.String(), token) {
		t.Fatalf("token leaked in stderr: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in stderr, got %q", errOut.String())
	}
}

func TestCompletionGeneratedFromCommandTable(t *testing.T) {
	words := completionWords()
	for _, want := range []string{"issue", "pr", "org", "packages", "threads", "api", "completion"} {
		if !containsWord(words[""], want) {
			t.Errorf("top-level completion misses %q", want)
		}
	}
	for _, want := range []string{"list", "show", "create", "comment", "close", "reopen"} {
		if !containsWord(words["issue"], want) {
			t.Errorf("issue completion misses %q", want)
		}
	}
	for _, want := range []string{"list", "add", "remove"} {
		if !containsWord(words["org member"], want) {
			t.Errorf("org member completion misses %q", want)
		}
	}
	if !containsWord(words["issue list"], "--label") || !containsWord(words["issue list"], "--json") {
		t.Errorf("issue list flags missing: %v", words["issue list"])
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script, err := completionScript(shell)
		if err != nil {
			t.Fatalf("%s: %v", shell, err)
		}
		for _, needle := range []string{"spindle", "issue", "pr", "org"} {
			if !strings.Contains(script, needle) {
				t.Errorf("%s completion misses %q", shell, needle)
			}
		}
	}
	if _, err := completionScript("powershell"); err == nil {
		t.Error("unknown shell accepted")
	}
}

func TestGroupRunnerUnknownSubcommandIsUsageError(t *testing.T) {
	var errOut bytes.Buffer
	c := &cli{errOut: &errOut}
	code := groupRunner(orgGroup)(context.Background(), c, []string{"nope"})
	if code != exitArg {
		t.Fatalf("exit = %d, want %d", code, exitArg)
	}
	if !strings.Contains(errOut.String(), "member") {
		t.Fatalf("usage does not list subcommands: %q", errOut.String())
	}
}

func TestGroupRunnerResolvesTwoTokenSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &cli{out: &out, errOut: &errOut}
	// A missing ORG is a usage error from runOrgMemberList — reaching that
	// error proves the two-token lookup ran.
	code := groupRunner(orgGroup)(context.Background(), c, []string{"member", "list"})
	if code != exitArg {
		t.Fatalf("exit = %d, want %d", code, exitArg)
	}
	if !strings.Contains(errOut.String(), "org member list") {
		t.Fatalf("wrong subcommand dispatched: %q", errOut.String())
	}
}

func TestReadBodyFlagFileAndLiteral(t *testing.T) {
	c := &cli{errOut: &bytes.Buffer{}}
	dir := t.TempDir()
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, code := readBodyFlag(c, "", path); code != exitOK || got != "from file" {
		t.Fatalf("file body = %q/%d", got, code)
	}
	if got, code := readBodyFlag(c, "literal", ""); code != exitOK || got != "literal" {
		t.Fatalf("literal body = %q/%d", got, code)
	}
}

func containsWord(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestAPICommandRedactsTokenInResponseBody(t *testing.T) {
	const token = "wgt_echo_me"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":"` + token + `"}`))
	}))
	defer srv.Close()
	c := isolateCLI(t, srv.URL)
	t.Setenv("SPINDLE_TOKEN", token)
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := runAPI(context.Background(), c, []string{"/api/v1/echo"}); code != exitOK {
		t.Fatalf("exit = %d (%s)", code, errOut.String())
	}
	if strings.Contains(out.String(), token) {
		t.Fatalf("token leaked in output: %q", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("expected [REDACTED], got %q", out.String())
	}
}
