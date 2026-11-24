// cli_runner.go — the runner process (docs/PLAN-INSTANCE-RUNNERS.md §6,
// docs/features/15 §3/§6):
//
//	spindle runner serve [--register TOKEN] [--labels a,b] [--capacity N]
//
// Runners are INSTANCE infrastructure administered by a host administrator in
// the browser at /admin/runners. Mint/list/revoke are deliberately NOT CLI
// verbs: the CLI's API client speaks static machine credentials, which cannot
// administer (AGENTS law 9), so a token/list/revoke command could only 403.
// `runner serve --register` is the operator step after a browser mint.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

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
		capacity           int
		labels             string
		name               string
		register           string
		tokenFile          string
		dockerBin          string
		image              string
		imagePrefix        string
		imagePull          string
		imagePrefixAll     bool
		imageRequireDigest bool
		allowHostDocker    bool
		allowForkRuns      bool
		fetchTimeout       int
	)
	fs.IntVar(&capacity, "capacity", envInt("SPINDLE_RUNNER_CAPACITY", 1), "concurrent jobs this runner keeps in flight (default 1)")
	fs.StringVar(&labels, "labels", envStr("SPINDLE_RUNNER_LABELS", "self-hosted,linux,docker"), "comma-separated labels the runner declares")
	fs.StringVar(&name, "name", envStr("SPINDLE_RUNNER_NAME", ""), "runner name (recorded at registration)")
	fs.StringVar(&register, "register", "", "consume a registration token (wrg_…) and store the returned runner token, then serve")
	fs.StringVar(&tokenFile, "token-file", envStr("SPINDLE_RUNNER_TOKEN_FILE", ""), "file holding the runner token (0600); SPINDLE_RUNNER_TOKEN wins when set")
	fs.StringVar(&dockerBin, "docker", envStr("SPINDLE_RUNNER_DOCKER", "docker"), "docker CLI binary")
	fs.StringVar(&image, "image", envStr("SPINDLE_RUNNER_IMAGE", runner.DefaultImage), "default job image when the workflow names none")
	fs.StringVar(&imagePrefix, "image-prefix", envStr("SPINDLE_RUNNER_IMAGE_PREFIX", ""), "registry mirror prefix host[:port]/path every workflow image is rewritten under (off when empty)")
	fs.BoolVar(&imagePrefixAll, "image-prefix-all", envBool("SPINDLE_RUNNER_IMAGE_PREFIX_ALL"), "rewrite registry-named images too (drops their host; needs --image-prefix)")
	fs.StringVar(&imagePull, "image-pull", envStr("SPINDLE_RUNNER_IMAGE_PULL", runner.PullMissing), "docker create --pull policy: missing, always or never")
	fs.BoolVar(&imageRequireDigest, "image-require-digest", envBool("SPINDLE_RUNNER_IMAGE_REQUIRE_DIGEST"), "refuse an image without @sha256: at plan time")
	fs.BoolVar(&allowHostDocker, "allow-host-docker", envBool("SPINDLE_RUNNER_ALLOW_HOST_DOCKER"), "acknowledge that DOCKER_HOST exposes the host root filesystem (required for the default socket)")
	fs.BoolVar(&allowForkRuns, "allow-fork-runs", envBool("SPINDLE_RUNNER_ALLOW_FORK_RUNS"), "execute fork pull-request code on this runner (default off; only on a disposable host)")
	fs.IntVar(&fetchTimeout, "fetch-timeout", 20, "long-poll seconds per fetch (1–30)")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		fmt.Fprintf(c.stderr(), "spindle runner serve: unexpected argument %q\n", pos[0])
		return exitArg
	}
	images := runner.ImageConfig{
		Prefix: imagePrefix, PrefixAll: imagePrefixAll, Pull: imagePull, RequireDigest: imageRequireDigest,
	}
	// Validate before anything dials the server: a bad prefix must be a
	// startup error naming its flag, never a job that fails hours later.
	if err := runner.ValidateImageConfig(images); err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner serve: %v\n", err)
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
		res, rerr := runner.Register(ctx, serverURL, strings.TrimSpace(register), name, labelList, capacity, allowForkRuns)
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
		ImagePrefix: imagePrefix, ImagePrefixAll: imagePrefixAll,
		ImagePull: imagePull, ImageRequireDigest: imageRequireDigest,
		AllowForkRuns: allowForkRuns,
		ServerOrigin:  serverURL,
		FetchWait:     time.Duration(clamp(fetchTimeout, 1, 30)) * time.Second,
		Logger:        logger,
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
	help: "Threads instance runners: serve jobs",
	commands: []apiCommand{
		{name: "serve", usage: "spindle runner serve [--capacity N] [--labels a,b] [--register TOKEN]", help: "run jobs: declare, fetch globally, execute in docker, stream logs (SIGTERM stops cleanly)",
			flags: []string{"capacity", "labels", "name", "register", "token-file", "docker", "image", "image-prefix", "image-prefix-all", "image-pull", "image-require-digest", "allow-host-docker", "allow-fork-runs", "fetch-timeout"}, run: runRunnerServe},
	},
}
