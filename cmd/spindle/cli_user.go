// cli_user.go — `spindle user` (11_config_cli.md §6.6): whoami, the owner
// listing, profiles, and the self-service SSH key surface
// (GET/POST/DELETE /api/v1/ssh-keys, 17_ssh.md §3).
package main

import (
	"context"
	"fmt"
	"strings"
)

var userGroup = &apiGroup{
	name: "user",
	help: "users, profiles, and SSH keys",
	commands: []apiCommand{
		{name: "whoami", usage: "spindle user whoami", help: "the authenticated principal",
			flags: []string{"json", "quiet"}, run: runUserWhoami},
		{name: "list", usage: "spindle user list", help: "list repository owners (users and orgs; no users-only route exists)",
			flags: []string{"json", "quiet"}, run: runUserList},
		{name: "show", usage: "spindle user show LOGIN", help: "one user profile",
			flags: []string{"json"}, run: runUserShow},
		{name: "keys list", usage: "spindle user keys list", help: "list your SSH public keys",
			flags: []string{"json", "quiet"}, run: runKeysList},
		{name: "keys add", usage: "spindle user keys add [KEY] [--file F] [--title T]", help: "register an SSH public key",
			flags: []string{"json", "file", "title", "quiet"}, run: runKeysAdd},
		{name: "keys remove", usage: "spindle user keys remove ID", help: "remove one of your SSH keys",
			flags: []string{"json", "quiet"}, run: runKeysRemove},
	},
}

// userProfile mirrors the profile document served by GET /api/v1/users/{p}
// (only the display fields the CLI prints; unknown fields are ignored).
type userProfile struct {
	Version     int    `json:"version"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Bio         string `json:"bio"`
	Location    string `json:"location"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// sshKeyRecord mirrors internal/api.SshKeyRecord.
type sshKeyRecord struct {
	Principal   string `json:"principal"`
	Fingerprint string `json:"fingerprint"`
	ID          string `json:"id"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	CreatedAt   string `json:"created_at"`
}

func runUserWhoami(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user whoami")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var me meResponse
	raw, err := a.callInto(ctx, "GET", "/api/v1/me", nil, nil, &me)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if me.Anonymous {
		fmt.Fprintln(c.stdout(), "anonymous (no credential accepted)")
		return exitOK
	}
	fmt.Fprintln(c.stdout(), me.Principal)
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "write: %v\nadmin: %v\n", me.Write, me.Admin)
	}
	return exitOK
}

func runUserList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var owners []string
	raw, err := a.callInto(ctx, "GET", "/api/v1/owners", nil, nil, &owners)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	for _, o := range owners {
		fmt.Fprintln(c.stdout(), o)
	}
	return exitOK
}

func runUserShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user show")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle user show LOGIN")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var p userProfile
	raw, err := a.callInto(ctx, "GET", topPath("users", pos[0]), nil, nil, &p)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	fmt.Fprintf(c.stdout(), "%s\n", p.Username)
	if p.DisplayName != "" {
		fmt.Fprintf(c.stdout(), "  name:     %s\n", p.DisplayName)
	}
	if p.Email != "" {
		fmt.Fprintf(c.stdout(), "  email:    %s\n", p.Email)
	}
	if p.Location != "" {
		fmt.Fprintf(c.stdout(), "  location: %s\n", p.Location)
	}
	if p.Bio != "" {
		fmt.Fprintf(c.stdout(), "  bio:      %s\n", p.Bio)
	}
	fmt.Fprintf(c.stdout(), "  joined:   %s\n", p.CreatedAt)
	return exitOK
}

func runKeysList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user keys list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var keys []sshKeyRecord
	raw, err := a.callInto(ctx, "GET", "/api/v1/ssh-keys", nil, nil, &keys)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "ID", "TITLE", "TYPE", "FINGERPRINT", "CREATED")
	for _, k := range keys {
		t.row(k.ID, k.Title, keyType(k.Key), k.Fingerprint, k.CreatedAt)
	}
	t.flush()
	return exitOK
}

func runKeysAdd(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user keys add")
	f.add(fs)
	var file, title string
	fs.StringVar(&file, "file", "", "read the authorized_keys line from FILE (- = stdin)")
	fs.StringVar(&title, "title", "", "a display title for the key")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	var key string
	switch {
	case file != "":
		raw, exit := readBodyFlag(c, "", file)
		if exit != exitOK {
			return exit
		}
		key = raw
	case len(pos) == 1:
		key = pos[0]
	case len(pos) > 1:
		fmt.Fprintln(c.stderr(), "usage: spindle user keys add [KEY] [--file F] [--title T]")
		return exitArg
	default:
		fmt.Fprintln(c.stderr(), "spindle: no key: pass the authorized_keys line, --file F, or --file -")
		return exitArg
	}
	key = strings.TrimRight(key, "\r\n")
	if strings.TrimSpace(key) == "" {
		fmt.Fprintln(c.stderr(), "spindle: the key line is empty")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var rec sshKeyRecord
	raw, err := a.callInto(ctx, "POST", "/api/v1/ssh-keys", nil,
		map[string]string{"key": key, "title": title}, &rec)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Added %s (%s)\n", rec.Fingerprint, rec.ID)
	}
	return exitOK
}

func runKeysRemove(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "user keys remove")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle user keys remove ID")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "DELETE", topPath("ssh-keys", pos[0]), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Removed key %s\n", pos[0])
	}
	return exitOK
}

// keyType is the second field of an authorized_keys line (ssh-ed25519 …).
func keyType(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[0]
	}
	return ""
}
