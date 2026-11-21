// cli_org.go — `spindle org` (11_config_cli.md §6.6): organizations, the
// member roster, and teams. Everything here is a shipped route except
// `team add-repo`, which is defined against the documented team-repository
// shape and reports "feature not enabled" on a 404.
package main

import (
	"context"
	"fmt"
)

var orgGroup = &apiGroup{
	name: "org",
	help: "organizations, members, and teams",
	commands: []apiCommand{
		{name: "list", usage: "spindle org list", help: "list organizations",
			flags: []string{"json", "quiet"}, run: runOrgList},
		{name: "show", usage: "spindle org show ORG", help: "one organization profile",
			flags: []string{"json"}, run: runOrgShow},
		{name: "create", usage: "spindle org create ORG [--display-name N] [--description D]",
			help:  "create an organization",
			flags: []string{"json", "display-name", "description"}, run: runOrgCreate},
		{name: "member list", usage: "spindle org member list ORG", help: "list the member roster",
			flags: []string{"json"}, run: runOrgMemberList},
		{name: "member add", usage: "spindle org member add ORG PRINCIPAL [--role member|owner]",
			help:  "add or update a member",
			flags: []string{"json", "role"}, run: runOrgMemberAdd},
		{name: "member remove", usage: "spindle org member remove ORG PRINCIPAL",
			help:  "remove a member",
			flags: []string{"json", "quiet"}, run: runOrgMemberRemove},
		{name: "team list", usage: "spindle org team list ORG", help: "list teams",
			flags: []string{"json"}, run: runOrgTeamList},
		{name: "team create", usage: "spindle org team create ORG --slug S --name N [--description D]",
			help:  "create a team",
			flags: []string{"json", "slug", "name", "description"}, run: runOrgTeamCreate},
		{name: "team add-repo", usage: "spindle org team add-repo ORG TEAM REPO",
			help:  "grant a team access to a repository (documented; WP10/admin surface)",
			flags: []string{"json"}, run: runOrgTeamAddRepo},
	},
}

// orgDoc mirrors GET /api/v1/orgs/{org}.
type orgDoc struct {
	Version     int    `json:"version"`
	Org         string `json:"org"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Location    string `json:"location"`
	BioMarkdown string `json:"bio_markdown"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// orgMember mirrors one entry of the members document.
type orgMember struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	JoinedAt  string `json:"joined_at"`
}

// orgMembers is the members.json wire shape.
type orgMembers struct {
	Version int         `json:"version"`
	Members []orgMember `json:"members"`
}

// orgTeam mirrors GET/POST /api/v1/orgs/{org}/teams.
type orgTeam struct {
	Version     int      `json:"version"`
	Org         string   `json:"org"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"`
}

func runOrgList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org list")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok || len(pos) > 0 {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var orgs []string
	raw, err := a.callInto(ctx, "GET", "/api/v1/orgs", nil, nil, &orgs)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	for _, o := range orgs {
		fmt.Fprintln(c.stdout(), o)
	}
	return exitOK
}

func runOrgShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org show")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org show ORG")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var o orgDoc
	raw, err := a.callInto(ctx, "GET", topPath("orgs", pos[0]), nil, nil, &o)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	fmt.Fprintln(c.stdout(), o.Org)
	if o.DisplayName != "" {
		fmt.Fprintf(c.stdout(), "  name:        %s\n", o.DisplayName)
	}
	if o.Description != "" {
		fmt.Fprintf(c.stdout(), "  description: %s\n", o.Description)
	}
	if o.Location != "" {
		fmt.Fprintf(c.stdout(), "  location:    %s\n", o.Location)
	}
	fmt.Fprintf(c.stdout(), "  created:     %s\n", o.CreatedAt)
	return exitOK
}

func runOrgCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org create")
	f.add(fs)
	var display, desc string
	fs.StringVar(&display, "display-name", "", "the display name")
	fs.StringVar(&desc, "description", "", "the description")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org create ORG [--display-name N] [--description D]")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]string{"org": pos[0], "display_name": display, "description": desc}
	var out struct {
		Org string `json:"org"`
	}
	raw, err := a.callInto(ctx, "POST", "/api/v1/orgs", nil, body, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Created organization %s\n", out.Org)
	}
	return exitOK
}

func runOrgMemberList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org member list")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org member list ORG")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var m orgMembers
	raw, err := a.callInto(ctx, "GET", topPath("orgs", pos[0], "members"), nil, nil, &m)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "PRINCIPAL", "ROLE", "JOINED")
	for _, e := range m.Members {
		t.row(e.Principal, e.Role, e.JoinedAt)
	}
	t.flush()
	return exitOK
}

func runOrgMemberAdd(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org member add")
	f.add(fs)
	var role string
	fs.StringVar(&role, "role", "member", "member or owner")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle org member add ORG PRINCIPAL [--role member|owner]")
		return exitArg
	}
	if role != "member" && role != "owner" {
		fmt.Fprintln(c.stderr(), "spindle: --role must be member or owner")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.callInto(ctx, "PUT", topPath("orgs", pos[0], "members", pos[1]), nil,
		map[string]string{"role": role}, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "%s is now %s of %s\n", pos[1], role, pos[0])
	}
	return exitOK
}

func runOrgMemberRemove(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org member remove")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle org member remove ORG PRINCIPAL")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "DELETE", topPath("orgs", pos[0], "members", pos[1]), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Removed %s from %s\n", pos[1], pos[0])
	}
	return exitOK
}

func runOrgTeamList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org team list")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org team list ORG")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var teams []orgTeam
	raw, err := a.callInto(ctx, "GET", topPath("orgs", pos[0], "teams"), nil, nil, &teams)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "SLUG", "NAME", "MEMBERS", "DESCRIPTION")
	for _, tm := range teams {
		t.row(tm.Slug, tm.Name, fmt.Sprint(len(tm.Members)), tm.Description)
	}
	t.flush()
	return exitOK
}

func runOrgTeamCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org team create")
	f.add(fs)
	var slug, name, desc string
	fs.StringVar(&slug, "slug", "", "the team slug (lowercase)")
	fs.StringVar(&name, "name", "", "the display name")
	fs.StringVar(&desc, "description", "", "the description")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org team create ORG --slug S --name N [--description D]")
		return exitArg
	}
	if slug == "" || name == "" {
		fmt.Fprintln(c.stderr(), "spindle: --slug and --name are required")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]string{"slug": slug, "name": name, "description": desc}
	var out orgTeam
	raw, err := a.callInto(ctx, "POST", topPath("orgs", pos[0], "teams"), nil, body, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Created team %s/%s\n", out.Org, out.Slug)
	}
	return exitOK
}

// runOrgTeamAddRepo targets the documented team→repository binding. No
// shipped route serves it yet, so a 404 is reported as "feature not enabled"
// (exit 1), never the misleading not-found (exit 3).
func runOrgTeamAddRepo(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org team add-repo")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) != 3 {
		fmt.Fprintln(c.stderr(), "usage: spindle org team add-repo ORG TEAM OWNER/REPO")
		return exitArg
	}
	repoOwner, repoName, ok := splitRepo(pos[2])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[2])
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "PUT", topPath("orgs", pos[0], "teams", pos[1], "repos", repoOwner, repoName), nil, nil)
	if err != nil {
		return a.notEnabled(err, "org team add-repo")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Granted team %s/%s access to %s\n", pos[0], pos[1], pos[2])
	}
	return exitOK
}
