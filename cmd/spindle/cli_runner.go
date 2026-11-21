// cli_runner.go — the runner protocol's operator verbs (docs/features/15 §3/§6):
//
//	spindle runner token  OWNER/REPO [--labels linux,docker] [--name N]
//	spindle runner list   OWNER/REPO
//	spindle runner revoke OWNER/REPO ID
//
// `runner token` mints a one-time registration token (printed once, the
// `checks tokens` UX); the runner process itself is pass B2 and lives outside
// this binary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/runner"
)

// runRunnerServe is the runner process (15 §3/§4, pass B2): declare, long-poll
// fetch, claim one job at a time per capacity slot, execute it in docker, and
// repeat until SIGTERM (which cancels the in-flight jobs, cleans their
// containers and reports them cancelled). Configuration is flags ⊕
// environment; the token is never a flag value in process listings (a file or
// the environment only).
func runRunnerServe(ctx context.Context, c *cli, args []string) int {
	fs := newAPIFlagSet(c, "runner serve")
	var (
		capacity        int
		labels          string
		name            string
		register        string
		tokenFile       string
		dockerBin       string
		image           string
		allowHostDocker bool
		fetchTimeout    int
	)
	fs.IntVar(&capacity, "capacity", envInt("SPINDLE_RUNNER_CAPACITY", 1), "concurrent jobs this runner keeps in flight (default 1)")
	fs.StringVar(&labels, "labels", envStr("SPINDLE_RUNNER_LABELS", "self-hosted,linux,docker"), "comma-separated labels the runner declares")
	fs.StringVar(&name, "name", envStr("SPINDLE_RUNNER_NAME", ""), "runner name (recorded at registration)")
	fs.StringVar(&register, "register", "", "consume a registration token (wrg_…) and store the returned runner token, then serve")
	fs.StringVar(&tokenFile, "token-file", envStr("SPINDLE_RUNNER_TOKEN_FILE", ""), "file holding the runner token (0600); SPINDLE_RUNNER_TOKEN wins when set")
	fs.StringVar(&dockerBin, "docker", envStr("SPINDLE_RUNNER_DOCKER", "docker"), "docker CLI binary")
	fs.StringVar(&image, "image", envStr("SPINDLE_RUNNER_IMAGE", runner.DefaultImage), "default job image when the workflow names none")
	fs.BoolVar(&allowHostDocker, "allow-host-docker", envBool("SPINDLE_RUNNER_ALLOW_HOST_DOCKER"), "acknowledge that DOCKER_HOST exposes the host root filesystem (required for the default socket)")
	fs.IntVar(&fetchTimeout, "fetch-timeout", 20, "long-poll seconds per fetch (1–30)")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		fmt.Fprintf(c.stderr(), "spindle runner serve: unexpected argument %q\n", pos[0])
		return exitArg
	}
	serverURL, _, err := resolveServer(c)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	labelList := splitLabels(labels)
	token := strings.TrimSpace(os.Getenv("SPINDLE_RUNNER_TOKEN"))
	if token == "" && tokenFile != "" && register == "" {
		b, rerr := os.ReadFile(tokenFile)
		if rerr != nil {
			fmt.Fprintf(c.stderr(), "spindle runner serve: read token file: %v\n", rerr)
			return exitErr
		}
		token = strings.TrimSpace(string(b))
	}
	if register != "" {
		res, rerr := runner.Register(ctx, serverURL, strings.TrimSpace(register), name, labelList)
		if rerr != nil {
			fmt.Fprintf(c.stderr(), "spindle runner serve: register: %v\n", rerr)
			return exitErr
		}
		token = res.Token
		if tokenFile != "" {
			if werr := os.WriteFile(tokenFile, []byte(res.Token+"\n"), 0o600); werr != nil {
				fmt.Fprintf(c.stderr(), "spindle runner serve: save token: %v\n", werr)
				return exitErr
			}
			fmt.Fprintf(c.stderr(), "spindle runner serve: registered runner %s; token saved to %s\n", res.RunnerID, tokenFile)
		} else {
			fmt.Fprintf(c.stderr(), "spindle runner serve: registered runner %s; token (save it, it is shown once):\n%s\n", res.RunnerID, res.Token)
		}
	}
	logger := slog.New(slog.NewTextHandler(c.stderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	rn, err := runner.New(runner.Config{
		ServerURL: serverURL, Token: token, Name: name, Labels: labelList,
		Capacity: capacity, Version: version(),
		DockerBin: dockerBin, DefaultImage: image, AllowHostDocker: allowHostDocker,
		ServerOrigin: serverURL,
		FetchWait:    time.Duration(clamp(fetchTimeout, 1, 30)) * time.Second,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner serve: %v\n", err)
		return exitErr
	}
	if err := rn.Run(ctx); err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner serve: %v\n", err)
		return exitErr
	}
	return exitOK
}

// splitLabels splits a comma-separated label list.
func splitLabels(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envStr reads one string from the environment with a default.
func envStr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// envInt reads one integer from the environment with a default.
func envInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// envBool reads one boolean from the environment.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// clamp bounds an int.
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

var runnerGroup = &apiGroup{
	name: "runner",
	help: "Threads runners: serve jobs, mint registration tokens, list and revoke",
	commands: []apiCommand{
		{name: "serve", usage: "spindle runner serve [--capacity N] [--labels a,b] [--register TOKEN]", help: "run jobs: declare, fetch, execute in docker, stream logs (SIGTERM stops cleanly)",
			flags: []string{"capacity", "labels", "name", "register", "token-file", "docker", "image", "allow-host-docker", "fetch-timeout"}, run: runRunnerServe},
		{name: "token", usage: "spindle runner token OWNER/REPO [--labels a,b] [--name N]", help: "mint a one-time runner registration token",
			flags: []string{"json", "labels", "name"}, run: runRunnerToken},
		{name: "list", usage: "spindle runner list OWNER/REPO", help: "list registered runners",
			flags: []string{"json"}, run: runRunnerList},
		{name: "revoke", usage: "spindle runner revoke OWNER/REPO ID", help: "revoke a runner",
			flags: []string{"json", "quiet"}, run: runRunnerRevoke},
	},
}

// runnerTokenAnswer is the mint response (the one place the secret is shown).
type runnerTokenAnswer struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func runRunnerToken(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "runner token")
	f.add(fs)
	var labels, name string
	fs.StringVar(&labels, "labels", "", "comma-separated labels the runner will declare (default self-hosted)")
	fs.StringVar(&name, "name", "", "runner name recorded at registration")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "runner token OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, http.MethodPost, threadsRunnerPath(owner, repo, "tokens"), nil,
		map[string]string{"name": name, "labels": labels})
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var answer runnerTokenAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Registration token (shown once, expires %s):\n%s\n\n", answer.ExpiresAt, answer.Token)
		fmt.Fprintln(c.stdout(), "Pass it to the runner's `register` call; it cannot be read back.")
	}
	return exitOK
}

func runRunnerList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "runner list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "runner list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, http.MethodGet, threadsRunnerPath(owner, repo), nil, nil)
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var body struct {
		Runners []struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Labels       []string `json:"labels"`
			State        string   `json:"state"`
			LastSeenAt   *string  `json:"last_seen_at"`
			RegisteredAt *string  `json:"registered_at"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "ID", "NAME", "STATE", "LABELS", "LAST SEEN")
	for _, r := range body.Runners {
		seen := "-"
		if r.LastSeenAt != nil {
			seen = *r.LastSeenAt
		}
		t.row(r.ID, r.Name, r.State, joinComma(r.Labels), seen)
	}
	t.flush()
	return exitOK
}

func runRunnerRevoke(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "runner revoke")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, id, ok := repoAndWord(c, pos, "runner revoke OWNER/REPO ID")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, http.MethodDelete, threadsRunnerPath(owner, repo, url.PathEscape(id)), nil, nil)
	if err != nil {
		return a.notEnabled(err, "threads")
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Revoked runner %s\n", id)
	}
	return exitOK
}

// threadsRunnerPath builds /{o}/{r}/api/threads/runners[/…].
func threadsRunnerPath(owner, repo string, segs ...string) string {
	return apiPath(owner, repo, append([]string{"threads", "runners"}, segs...)...)
}

// joinComma renders a label list for the table.
func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += s
	}
	if out == "" {
		return "-"
	}
	return out
}

// compile-time assertion: the CLI's mint answer mirrors the API type.
var _ = threads.RegistrationToken{}
