// cli_org.go — `spindle org` (11_config_cli.md §6.6): organizations, the
// member roster, and teams. Everything here is a shipped route except
// `team add-repo`, which is defined against the documented team-repository
// shape and reports "feature not enabled" on a 404.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
		{name: "webhook list", usage: "spindle org webhook list ORG", help: "list org webhooks",
			flags: []string{"json"}, run: runOrgWebhookList},
		{name: "webhook create", usage: "spindle org webhook create ORG --url URL [--events E,…] [--secret S] [--inactive] [--insecure-tls]",
			help:  "create an org webhook",
			flags: []string{"json", "url", "events", "secret", "inactive", "insecure-tls"}, run: runOrgWebhookCreate},
		{name: "webhook delete", usage: "spindle org webhook delete ORG ID", help: "delete an org webhook",
			flags: []string{"json", "quiet"}, run: runOrgWebhookDelete},
		{name: "webhook ping", usage: "spindle org webhook ping ORG ID", help: "send a synthetic ping delivery",
			flags: []string{"json"}, run: runOrgWebhookPing},
		{name: "webhook deliveries", usage: "spindle org webhook deliveries ORG ID", help: "recent deliveries (last 25)",
			flags: []string{"json"}, run: runOrgWebhookDeliveries},
		{name: "webhook redeliver", usage: "spindle org webhook redeliver ORG ID SEQ", help: "re-send a retained delivery",
			flags: []string{"json"}, run: runOrgWebhookRedeliver},
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

// orgWebhook mirrors the wire Hook (secret never returned; secret_set instead).
type orgWebhook struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Active      bool     `json:"active"`
	InsecureTLS bool     `json:"insecure_tls"`
	SecretSet   bool     `json:"secret_set"`
	CreatedBy   string   `json:"created_by"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// orgDelivery mirrors one deliveries-ring row (body is retained for
// redeliver and stays opaque to the CLI).
type orgDelivery struct {
	Seq        int             `json:"seq"`
	Event      string          `json:"event"`
	Status     int             `json:"status"`
	At         string          `json:"at"`
	Error      string          `json:"error"`
	DurationMs int64           `json:"duration_ms"`
	Body       json.RawMessage `json:"body"`
}

type orgDeliveries struct {
	UpdatedAt string        `json:"updated_at"`
	Entries   []orgDelivery `json:"entries"`
}

func orgWebhookPath(org string, segs ...string) string {
	return topPath(append([]string{"orgs", org, "webhooks"}, segs...)...)
}

func runOrgWebhookList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook list ORG")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var out struct {
		Webhooks []orgWebhook `json:"webhooks"`
	}
	raw, err := a.callInto(ctx, "GET", orgWebhookPath(pos[0]), nil, nil, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "ID", "URL", "EVENTS", "ACTIVE", "SECRET")
	for _, h := range out.Webhooks {
		events := strings.Join(h.Events, ",")
		if events == "" {
			events = "all"
		}
		state := "off"
		if h.Active {
			state = "active"
		}
		secret := "—"
		if h.SecretSet {
			secret = "set"
		}
		t.row(h.ID, h.URL, events, state, secret)
	}
	t.flush()
	return exitOK
}

func runOrgWebhookCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook create")
	f.add(fs)
	var rawURL, events, secret string
	var inactive, insecure bool
	fs.StringVar(&rawURL, "url", "", "the delivery URL (https; loopback http allowed)")
	fs.StringVar(&events, "events", "", "comma list of actions (empty = all, * = everything)")
	fs.StringVar(&secret, "secret", "", "HMAC secret (write-only)")
	fs.BoolVar(&inactive, "inactive", false, "create disabled")
	fs.BoolVar(&insecure, "insecure-tls", false, "skip TLS verification for this hook")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook create ORG --url URL [--events E,…] [--secret S] [--inactive] [--insecure-tls]")
		return exitArg
	}
	if rawURL == "" {
		fmt.Fprintln(c.stderr(), "spindle: --url is required")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	body := map[string]any{"url": rawURL, "active": !inactive, "insecure_tls": insecure}
	if evs := orgWebhookEventList(events); len(evs) > 0 {
		body["events"] = evs
	}
	if secret != "" {
		body["secret"] = secret
	}
	var out orgWebhook
	raw, err := a.callInto(ctx, "POST", orgWebhookPath(pos[0]), nil, body, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Created webhook %s → %s\n", out.ID, out.URL)
	}
	return exitOK
}

// orgWebhookEventList splits the --events comma list.
func orgWebhookEventList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runOrgWebhookDelete(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook delete")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook delete ORG ID")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "DELETE", orgWebhookPath(pos[0], pos[1]), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Deleted webhook %s\n", pos[1])
	}
	return exitOK
}

func runOrgWebhookPing(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook ping")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook ping ORG ID")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var out struct {
		Delivery bool `json:"delivery"`
	}
	raw, err := a.callInto(ctx, "POST", orgWebhookPath(pos[0], pos[1], "ping"), nil, nil, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if out.Delivery {
		fmt.Fprintln(c.stdout(), "Ping delivered")
		return exitOK
	}
	fmt.Fprintln(c.stderr(), "spindle: ping failed — see `spindle org webhook deliveries` for the status")
	return exitErr
}

func runOrgWebhookDeliveries(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook deliveries")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook deliveries ORG ID")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var out orgDeliveries
	raw, err := a.callInto(ctx, "GET", orgWebhookPath(pos[0], pos[1], "deliveries"), nil, nil, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "SEQ", "EVENT", "STATUS", "DURATION", "AT", "ERROR")
	for _, e := range out.Entries {
		t.row(strconv.Itoa(e.Seq), e.Event, strconv.Itoa(e.Status),
			fmt.Sprintf("%dms", e.DurationMs), e.At, e.Error)
	}
	t.flush()
	return exitOK
}

func runOrgWebhookRedeliver(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org webhook redeliver")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 3 {
		fmt.Fprintln(c.stderr(), "usage: spindle org webhook redeliver ORG ID SEQ")
		return exitArg
	}
	seq, err := strconv.Atoi(pos[2])
	if err != nil || seq < 0 {
		fmt.Fprintln(c.stderr(), "spindle: SEQ must be a non-negative integer")
		return exitArg
	}
	a, cerr := newAPIClient(c, f)
	if cerr != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", cerr)
		return exitErr
	}
	var out struct {
		Delivery bool        `json:"delivery"`
		Entry    orgDelivery `json:"entry"`
	}
	raw, err := a.callInto(ctx, "POST", orgWebhookPath(pos[0], pos[1], "deliveries", strconv.Itoa(seq), "redeliver"), nil, nil, &out)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if out.Delivery {
		fmt.Fprintf(c.stdout(), "Redelivered #%d → %d in %dms\n", out.Entry.Seq, out.Entry.Status, out.Entry.DurationMs)
		return exitOK
	}
	fmt.Fprintf(c.stderr(), "spindle: redelivery failed (HTTP %d)\n", out.Entry.Status)
	return exitErr
}

func runOrgList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "org list")
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
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
