// cli_auth.go — `spindle auth` (11_config_cli.md §6.6): the login state
// file, status, logout, and the token subcommands. Token list/create/revoke
// is deliberately NOT implemented: the JSON API exposes no token surface
// (minting is session-only at /_auth/tokens), so the commands exist to say
// exactly that instead of inventing an endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

var authGroup = &apiGroup{
	name: "auth",
	help: "credential management for the JSON API",
	commands: []apiCommand{
		{name: "login", usage: "spindle auth login [--token-file F] [--stdin] [--no-verify] [--server URL]",
			help:  "store a token for the server (0600 state file)",
			flags: []string{"token-file", "stdin", "no-verify", "json", "quiet"},
			run:   runAuthLogin},
		{name: "logout", usage: "spindle auth logout [--all]",
			help:  "remove the stored token for the current server",
			flags: []string{"all", "quiet"},
			run:   runAuthLogout},
		{name: "status", usage: "spindle auth status",
			help:  "show the server and principal the CLI would use",
			flags: []string{"json", "quiet"},
			run:   runAuthStatus},
		{name: "token list", usage: "spindle auth token list",
			help:  "tokens are session-minted at /_auth/tokens (no JSON API)",
			flags: []string{},
			run:   runAuthTokenUnsupported},
		{name: "token create", usage: "spindle auth token create",
			help:  "tokens are session-minted at /_auth/tokens (no JSON API)",
			flags: []string{},
			run:   runAuthTokenUnsupported},
		{name: "token revoke", usage: "spindle auth token revoke",
			help:  "tokens are session-minted at /_auth/tokens (no JSON API)",
			flags: []string{},
			run:   runAuthTokenUnsupported},
	},
}

// meResponse mirrors GET /api/v1/me (07_api.md §3).
type meResponse struct {
	Principal string `json:"principal"`
	Write     bool   `json:"write"`
	Anonymous bool   `json:"anonymous"`
	Admin     bool   `json:"admin"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

func runAuthLogin(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "auth login")
	f.add(fs)
	var tokenFile string
	var stdin, noVerify bool
	fs.StringVar(&tokenFile, "token-file", "", "read the token from FILE (mode 0600)")
	fs.BoolVar(&stdin, "stdin", false, "read the token from stdin")
	fs.BoolVar(&noVerify, "no-verify", false, "store the token without calling GET /api/v1/me")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) > 0 {
		fmt.Fprintln(c.stderr(), "spindle: auth login takes no positional arguments")
		fs.Usage()
		return exitArg
	}

	base, st, err := resolveServer(c)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if stdin && tokenFile != "" {
		fmt.Fprintln(c.stderr(), "spindle: --stdin and --token-file are mutually exclusive")
		return exitArg
	}

	var token string
	switch {
	case tokenFile != "":
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
			return exitErr
		}
		token = trimNewline(string(raw))
	case stdin:
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: reading stdin: %v\n", err)
			return exitErr
		}
		token = trimNewline(string(raw))
	default:
		if info, serr := os.Stdin.Stat(); serr == nil && (info.Mode()&os.ModeCharDevice) == 0 {
			raw, rerr := io.ReadAll(os.Stdin)
			if rerr != nil {
				fmt.Fprintf(c.stderr(), "spindle: reading stdin: %v\n", rerr)
				return exitErr
			}
			token = trimNewline(string(raw))
		}
	}
	if token == "" {
		fmt.Fprintln(c.stderr(), "spindle: no token: pass --token-file FILE or pipe the token on stdin")
		return exitArg
	}

	entry := loginEntry{Token: token, UpdatedAt: nowStamp()}
	if !noVerify {
		a := &apiClient{base: base, token: token, source: "flag", hc: &http.Client{}, out: c.stdout(), errOut: c.stderr()}
		var me meResponse
		if _, err := a.callInto(ctx, "GET", "/api/v1/me", nil, nil, &me); err != nil {
			return a.fail(err)
		}
		entry.Principal = me.Principal
		entry.Write = me.Write
		entry.Admin = me.Admin
	}
	st.Servers[base] = entry
	st.Current = base
	if err := saveState(st); err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if f.json {
		return (&apiClient{out: c.stdout()}).emitJSON(mustJSON(map[string]any{
			"server": base, "principal": entry.Principal, "write": entry.Write,
			"admin": entry.Admin, "state": statePath(),
		}))
	}
	if !f.quiet {
		if entry.Principal != "" {
			fmt.Fprintf(c.stdout(), "Logged in to %s as %s (state: %s)\n", base, entry.Principal, statePath())
		} else {
			fmt.Fprintf(c.stdout(), "Stored token for %s (state: %s)\n", base, statePath())
		}
	}
	return exitOK
}

func runAuthLogout(ctx context.Context, c *cli, args []string) int {
	fs := newAPIFlagSet(c, "auth logout")
	var all, quiet bool
	fs.BoolVar(&all, "all", false, "forget every stored server credential")
	fs.BoolVar(&quiet, "quiet", false, "print nothing")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) > 0 {
		fmt.Fprintln(c.stderr(), "spindle: auth logout takes no positional arguments")
		return exitArg
	}
	base, st, err := resolveServer(c)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if all {
		st = loginState{Version: 1, Servers: map[string]loginEntry{}}
	} else {
		delete(st.Servers, base)
		if st.Current == base {
			st.Current = ""
		}
	}
	if err := saveState(st); err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if !quiet {
		if all {
			fmt.Fprintln(c.stdout(), "Logged out of every server")
		} else {
			fmt.Fprintf(c.stdout(), "Logged out of %s\n", base)
		}
	}
	return exitOK
}

// authStatus is the documented --json shape of `auth status`.
type authStatus struct {
	Server        string `json:"server"`
	Authenticated bool   `json:"authenticated"`
	TokenSource   string `json:"token_source"` // env | state | none
	Principal     string `json:"principal,omitempty"`
	Write         bool   `json:"write"`
	Admin         bool   `json:"admin"`
}

func runAuthStatus(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "auth status")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) > 0 {
		fmt.Fprintln(c.stderr(), "spindle: auth status takes no positional arguments")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	st := authStatus{Server: a.base, TokenSource: a.source}
	// /api/v1/me is always probed: with auth.mode=none (or anonymous_read) the
	// server admits the client without a token, and status must say so rather
	// than claim "not logged in" at a server that needs no login.
	var me meResponse
	if _, err := a.callInto(ctx, "GET", "/api/v1/me", nil, nil, &me); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			if f.json {
				a.emitJSON(mustJSON(st))
			} else {
				fmt.Fprintf(c.stdout(), "server: %s\nnot authenticated (HTTP %d)\n", a.base, ae.Status)
			}
			return exitAuth
		}
		return a.fail(err)
	}
	st.Authenticated = !me.Anonymous
	st.Principal = me.Principal
	st.Write = me.Write
	st.Admin = me.Admin
	if f.json {
		return a.emitJSON(mustJSON(st))
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "server:    %s\nprincipal: %s\n", a.base, me.Principal)
		fmt.Fprintf(c.stdout(), "write:     %v\nadmin:     %v\ntoken:     %s\n", me.Write, me.Admin, a.source)
	}
	return exitOK
}

// runAuthTokenUnsupported explains the deliberate gap: token minting is a
// browser-session surface (POST /_auth/tokens with a CSRF guard), not the
// public JSON API, so the CLI cannot list/create/revoke tokens.
func runAuthTokenUnsupported(ctx context.Context, c *cli, args []string) int {
	base, _, err := resolveServer(c)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	fmt.Fprintf(c.stderr(),
		"spindle: token list/create/revoke is not offered: the JSON API exposes no token surface.\n"+
			"spindle: mint and revoke tokens in a signed-in browser at %s/_auth/tokens\n", base)
	return exitErr
}

// mustJSON marshals a CLI-native value for --json output (the API bodies are
// streamed separately; this is only for auth status/login).
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}
