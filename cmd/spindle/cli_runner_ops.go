// cli_runner_ops.go — runner fleet CLI: offline multi-scope serve (PAR-180),
// one-job workers, the documented stock-image catalogue, and the
// runner-authenticated fleet verbs (drain/resume/rotate/deregister, PAR-181).
//
// These verbs authenticate with the runner's OWN credential (--token-file or
// SPINDLE_RUNNER_TOKEN), never a host-admin session: a host administrator's
// fleet writes live in /admin/runners and /api/v1/admin/runners (AGENTS law 9
// keeps administration OIDC-only, so a static-token CLI verb could only 403).
// No verb ever prints a token; rotate writes the new one to the token file.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/runner"
)

// runnerShared is the process-wide half of the runner configuration (docker,
// images, container grants) that every [[runner]] entry inherits.
type runnerShared struct {
	Version            string
	DockerBin          string
	DefaultImage       string
	AllowHostDocker    bool
	ImagePrefix        string
	ImagePrefixAll     bool
	ImagePull          string
	ImageRequireDigest bool
	AllowForkRuns      bool
	Policy             runner.ContainerPolicy
	RegistryAuthFile   string
	FetchWait          time.Duration
	Logger             *slog.Logger
}

// runRunnerConfig serves every [[runner]] entry in one process (PAR-180). Each
// entry has its own credential and therefore its own admin-set scope; the
// docker/image/policy settings are shared. The first fatal error cancels the
// others.
func runRunnerConfig(ctx context.Context, c *cli, path, defaultServer string, shared runnerShared) int {
	entries, err := loadRunnerFile(path)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner serve: %v\n", err)
		return exitErr
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type pending struct {
		index  int
		config runner.Config
	}
	configs := make([]pending, 0, len(entries))
	for i, e := range entries {
		server := e.Server
		if server == "" {
			server = defaultServer
		}
		token := e.Token
		register := e.RegisterToken
		if e.RegisterFile != "" {
			register, err = readSecretFile(e.RegisterFile)
			if err != nil {
				fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d: %v\n", i+1, err)
				return exitErr
			}
		}
		if e.TokenFile != "" {
			token, err = readSecretFile(e.TokenFile)
			if err != nil {
				fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d: %v\n", i+1, err)
				return exitErr
			}
		}
		memoryBytes, merr := parseRunnerMemory(e.MaxMemory)
		if merr != nil {
			fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d: %v\n", i+1, merr)
			return exitArg
		}
		labels := e.Labels
		if len(labels) == 0 {
			labels = []string{"self-hosted", "linux", "docker"}
		}
		allowFork := e.AllowForkRuns
		if shared.AllowForkRuns {
			allowFork = true
		}
		if register != "" {
			res, rerr := runner.RegisterRunner(runCtx, server, strings.TrimSpace(register), runner.RunnerOptions{
				Name: e.Name, Labels: labels, Capacity: e.Capacity, AllowForkRuns: allowFork,
				Ephemeral: e.Ephemeral, MaxCPUs: e.MaxCPUs, MaxMemoryBytes: memoryBytes,
			})
			if rerr != nil {
				fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d register: %v\n", i+1, rerr)
				return exitErr
			}
			token = res.Token
			if e.TokenFile != "" {
				if werr := writeSecretFile(e.TokenFile, token); werr != nil {
					fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d save token: %v\n", i+1, werr)
					return exitErr
				}
				fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d registered %s; token saved to %s\n", i+1, res.RunnerID, e.TokenFile)
			} else {
				fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d registered %s; token (shown once):\n%s\n", i+1, res.RunnerID, res.Token)
			}
		}
		configs = append(configs, pending{index: i, config: runner.Config{
			ServerURL: server, Token: token, Name: e.Name, Labels: labels,
			Capacity: e.Capacity, Version: shared.Version,
			Ephemeral: e.Ephemeral, MaxCPUs: e.MaxCPUs, MaxMemoryBytes: memoryBytes,
			DockerBin: shared.DockerBin, DefaultImage: shared.DefaultImage, AllowHostDocker: shared.AllowHostDocker,
			ImagePrefix: shared.ImagePrefix, ImagePrefixAll: shared.ImagePrefixAll,
			ImagePull: shared.ImagePull, ImageRequireDigest: shared.ImageRequireDigest,
			AllowForkRuns: allowFork, Policy: shared.Policy,
			RegistryAuthFile: shared.RegistryAuthFile, ServerOrigin: server,
			FetchWait: shared.FetchWait, Logger: shared.Logger,
		}})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(configs))
	started := 0
	for _, item := range configs {
		rn, rerr := runner.New(item.config)
		if rerr != nil {
			fmt.Fprintf(c.stderr(), "spindle runner serve: runner %d: %v\n", item.index+1, rerr)
			cancel()
			wg.Wait()
			return exitErr
		}
		started++
		wg.Add(1)
		go func(idx int, r *runner.Runner) {
			defer wg.Done()
			if err := r.Run(runCtx); err != nil {
				errCh <- fmt.Errorf("runner %d: %w", idx+1, err)
				cancel()
			}
		}(item.index, rn)
	}
	info := fmt.Sprintf("spindle runner serve: %d runner(s) from %s (one process, one credential each)", started, path)
	fmt.Fprintln(c.stderr(), info)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		fmt.Fprintf(c.stderr(), "spindle runner serve: %v\n", err)
		return exitErr
	}
	return exitOK
}

// parseRunnerMemory parses a documented size spelling (16GiB, 512m) or "".
func parseRunnerMemory(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, ok := threads.ParseMemorySize(raw)
	if !ok {
		return 0, fmt.Errorf("--max-memory %q is not a size (try 16GiB, 512MiB)", raw)
	}
	return n, nil
}

// runnerFleetClient builds an authenticated client from --token-file or the
// environment. A token is never accepted on the command line.
func runnerFleetClient(c *cli, tokenFile string) (*runner.Client, string, error) {
	token := strings.TrimSpace(os.Getenv("SPINDLE_RUNNER_TOKEN"))
	if token == "" && tokenFile != "" {
		raw, err := readSecretFile(tokenFile)
		if err != nil {
			return nil, "", err
		}
		token = raw
	}
	if token == "" {
		return nil, "", fmt.Errorf("a runner credential is required: --token-file or SPINDLE_RUNNER_TOKEN")
	}
	serverURL, _, err := resolveServer(c)
	if err != nil {
		return nil, "", err
	}
	return runner.NewClient(serverURL, token), tokenFile, nil
}

// runRunnerFleetVerb is drain/resume/rotate/deregister against the runner's own
// credential.
func runRunnerFleetVerb(ctx context.Context, c *cli, args []string, verb string) int {
	fs := newAPIFlagSet(c, "runner "+verb)
	var tokenFile string
	fs.StringVar(&tokenFile, "token-file", envStr("SPINDLE_RUNNER_TOKEN_FILE", ""), "0600 file holding the runner credential")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		fmt.Fprintf(c.stderr(), "spindle runner %s: unexpected argument %q\n", verb, pos[0])
		return exitArg
	}
	client, path, err := runnerFleetClient(c, tokenFile)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner %s: %v\n", verb, err)
		return exitErr
	}
	switch verb {
	case "drain":
		if err = client.Drain(ctx); err == nil {
			fmt.Fprintln(c.stdout(), "Runner is draining: its current job finishes, new jobs are not claimed.")
		}
	case "resume":
		if err = client.Resume(ctx); err == nil {
			fmt.Fprintln(c.stdout(), "Runner is active again.")
		}
	case "rotate":
		var rot *threads.RunnerTokenRotation
		rot, err = client.Rotate(ctx)
		if err == nil {
			if path == "" {
				fmt.Fprintf(c.stdout(), "New credential (store it now; the old one is valid until %s):\n%s\n", rot.PreviousUntil, rot.Token)
			} else if werr := writeSecretFile(path, rot.Token); werr != nil {
				fmt.Fprintf(c.stderr(), "spindle runner rotate: write %s: %v\n", path, werr)
				return exitErr
			} else {
				fmt.Fprintf(c.stdout(), "Rotated the credential in %s; the old one is valid until %s.\n", path, rot.PreviousUntil)
			}
		}
	case "deregister":
		if err = client.Deregister(ctx); err == nil {
			fmt.Fprintln(c.stdout(), "Runner deregistered: its record and credential are gone.")
		}
	}
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle runner %s: %v\n", verb, err)
		return exitErr
	}
	return exitOK
}

// runRunnerImages prints the stock toolchain catalogue and the --labels line
// that declares it (PAR-177).
func runRunnerImages(ctx context.Context, c *cli, args []string) int {
	pos, exit := parseAPI(newAPIFlagSet(c, "runner images"), args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) > 0 {
		fmt.Fprintf(c.stderr(), "spindle runner images: unexpected argument %q\n", pos[0])
		return exitArg
	}
	t := newTable(c.stdout(), "LABEL", "IMAGE", "TOOLCHAIN")
	for _, img := range threads.StockImages() {
		t.row(img.Label, img.Image, img.Toolchain)
	}
	t.flush()
	fmt.Fprintln(c.stdout())
	fmt.Fprintln(c.stdout(), "Declare them on a runner (trim to the toolchains the host needs):")
	fmt.Fprintf(c.stdout(), "  --labels self-hosted,linux,%s\n", strings.Join(threads.StockImageLabelArgs(), ","))
	return exitOK
}

// runRunnerPool is the named D-14 refusal: there is no autoscaler.
func runRunnerPool(ctx context.Context, c *cli, args []string) int {
	fmt.Fprintf(c.stderr(), "spindle runner pool: %v\n", errPoolRefused)
	return exitErr
}
