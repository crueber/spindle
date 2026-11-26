package runner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Command files (docs/threads.md §3.10, PAR-090/PAR-091). A step may extend the
// job's environment (`$GITHUB_ENV`, Forgejo's `$FORGEJO_ENV`) and prepend
// directories to `PATH` (`$GITHUB_PATH`, `$FORGEJO_PATH`) for the steps that run
// AFTER it. The files are per-step paths inside the job container, so:
//
//   - a value written by step N is invisible to step N (collection happens when
//     the step has finished) and visible to every later step of the same job;
//   - nothing about the runner process is mutated: the values are argv elements
//     of the NEXT step's `docker exec` (env) or a shell wrapper (PATH), so two
//     concurrent jobs on one runner cannot see each other's files;
//   - a docker:// action never receives either: its image is third-party code
//     and gets no job environment layer (the same rule as secrets).
//
// The format is GitHub's for these two files: one NAME=VALUE per line for
// $GITHUB_ENV, one directory per line for $GITHUB_PATH. GitHub documents no
// heredoc form for either (that is $GITHUB_OUTPUT's), so none is invented here:
// a line without '=' is a named error. The runner repeats none of the values in
// an error message (a value may be a secret the step just read), and a refused
// name is refused BY NAME.
const (
	// EnvFileVar / EnvFileAlias are the environment variables holding this
	// step's env-file path.
	EnvFileVar   = "GITHUB_ENV"
	EnvFileAlias = "FORGEJO_ENV"
	// PathFileVar / PathFileAlias are the environment variables holding this
	// step's PATH-file path.
	PathFileVar   = "GITHUB_PATH"
	PathFileAlias = "FORGEJO_PATH"

	// commandFilePrefix is the per-step in-container path. Like the output file
	// it lives in the container's /tmp, not the workspace, so a `git clean` in
	// the checkout cannot lose it.
	commandFilePrefix = "/tmp/spindle-command-"

	// MaxCommandEnvVars / MaxCommandEnvBytes / MaxCommandEnvValue bound what one
	// job may add to its environment (a runaway job cannot exhaust the exec
	// argv, which the kernel limits too).
	MaxCommandEnvVars      = 100
	MaxCommandEnvBytes     = 64 << 10
	MaxCommandEnvValueLen  = 8 << 10
	MaxCommandEnvNameLen   = 256
	MaxCommandPathEntries  = 64
	MaxCommandPathEntryLen = 4096
)

// commandFilePaths are the per-step command files of one flat step index (the
// same index that names the output file: unique across composite expansion).
func commandFilePaths(index int) (envFile, pathFile string) {
	base := commandFilePrefix + strconv.Itoa(index+1)
	return base + "-env", base + "-path"
}

// commandFiles is the job's accumulated command-file state: environment
// variables and PATH entries written by the steps that already ran.
type commandFiles struct {
	env     map[string]string
	path    []string
	pathSet map[string]bool
	// envBytes and envCount account the caps across the whole job.
	envBytes int
}

// newCommandFiles builds an empty job command-file state.
func newCommandFiles() *commandFiles {
	return &commandFiles{env: map[string]string{}, pathSet: map[string]bool{}}
}

// addEnv merges one step's $GITHUB_ENV values, enforcing the per-value,
// per-job count and byte caps before anything is stored. A duplicate name keeps
// the last value written (later steps win, as on GitHub).
func (c *commandFiles) addEnv(values map[string]string) error {
	added, bytes := 0, 0
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := values[name]
		if len(value) > MaxCommandEnvValueLen {
			return fmt.Errorf("$GITHUB_ENV: variable %s is %d bytes, over the cap of %d bytes per value", name, len(value), MaxCommandEnvValueLen)
		}
		if _, ok := c.env[name]; !ok {
			added++
		}
		bytes += len(name) + 1 + len(value)
	}
	if len(c.env)+added > MaxCommandEnvVars {
		return fmt.Errorf("$GITHUB_ENV: the job would carry more than %d added variables", MaxCommandEnvVars)
	}
	if c.envBytes+bytes > MaxCommandEnvBytes {
		return fmt.Errorf("$GITHUB_ENV: the job would carry more than %d bytes of added environment", MaxCommandEnvBytes)
	}
	for _, name := range names {
		c.env[name] = values[name]
	}
	c.envBytes += bytes
	return nil
}

// addPath appends one step's $GITHUB_PATH directories. A directory already on
// the list is a no-op (a PATH with the same entry twice is a bug report, not
// parity), and the caps keep the list bounded.
func (c *commandFiles) addPath(entries []string) error {
	for _, entry := range entries {
		if c.pathSet[entry] {
			continue
		}
		if len(c.path) >= MaxCommandPathEntries {
			return fmt.Errorf("$GITHUB_PATH: the job would prepend more than %d directories to PATH", MaxCommandPathEntries)
		}
		c.pathSet[entry] = true
		c.path = append(c.path, entry)
	}
	return nil
}

// parseEnvFile parses one $GITHUB_ENV file: one NAME=VALUE per line (the value
// may contain '='), blank lines ignored, a trailing CR stripped so a CRLF file
// works, last write wins. A line without '=' — including a `name<<DELIM` heredoc
// marker, which GitHub does not document for this file — is a named error. The
// error never repeats the line's content.
func parseEnvFile(data []byte) (map[string]string, error) {
	out := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected NAME=VALUE (a heredoc is $GITHUB_OUTPUT's form, not $GITHUB_ENV's)", i+1)
		}
		name := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		if err := validateCommandEnvName(name, i+1); err != nil {
			return nil, err
		}
		if reason := protectedEnvName(name); reason != "" {
			return nil, fmt.Errorf("line %d: refusing to set %s from $GITHUB_ENV: %s", i+1, name, reason)
		}
		if strings.ContainsAny(value, "\x00\r") {
			// A value is exactly one line: a CR or NUL here is a crafted or
			// corrupt file, and it would reach the container's environment.
			return nil, fmt.Errorf("line %d: %s carries a control character (one NAME=VALUE per line, LF only)", i+1, name)
		}
		out[name] = value
	}
	return out, nil
}

// validateCommandEnvName checks one written variable name (a POSIX identifier:
// the shell must be able to export it).
func validateCommandEnvName(name string, line int) error {
	if name == "" {
		return fmt.Errorf("line %d: the variable name before '=' is empty", line)
	}
	if len(name) > MaxCommandEnvNameLen {
		return fmt.Errorf("line %d: the variable name is longer than %d bytes", line, MaxCommandEnvNameLen)
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("line %d: variable name %q starts with a digit", line, name)
			}
		default:
			return fmt.Errorf("line %d: variable name %q is not a shell identifier (letters, digits and _ only, not starting with a digit)", line, name)
		}
	}
	return nil
}

// protectedEnvName returns why a name may not be set from $GITHUB_ENV ("" when
// it may). The runner owns its own namespaces (a workflow must not rewrite the
// values the engine and the checkout rely on), and PATH is set only through
// $GITHUB_PATH, which APPENDS instead of replacing.
func protectedEnvName(name string) string {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"GITHUB_", "SPINDLE_", "FORGEJO_", "ACTIONS_", "RUNNER_"} {
		if strings.HasPrefix(upper, prefix) {
			return "the runner owns the " + strings.TrimSuffix(prefix, "_") + "_* namespace"
		}
	}
	switch upper {
	case "PATH":
		return "use $GITHUB_PATH to prepend a directory to PATH (it never replaces the image's PATH)"
	case "CI":
		return "the runner owns CI"
	case "LD_PRELOAD", "LD_AUDIT", "BASH_ENV", "ENV", "NODE_OPTIONS", "IFS", "PS4", "SHELLOPTS":
		return "spindle refuses process-startup hooks that would rewrite how later steps execute"
	}
	return ""
}

// parsePathFile parses one $GITHUB_PATH file: one directory per line, trimmed,
// blank lines ignored. A NUL, CR or LF inside an entry is a named error, and so
// is an over-long entry; nothing is evaluated (no glob, no $VAR).
func parsePathFile(data []byte) ([]string, error) {
	var out []string
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, raw := range lines {
		entry := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, "\x00\r\n") {
			return nil, fmt.Errorf("line %d: a PATH entry must be one line with no control characters", i+1)
		}
		if len(entry) > MaxCommandPathEntryLen {
			return nil, fmt.Errorf("line %d: the PATH entry is longer than %d bytes", i+1, MaxCommandPathEntryLen)
		}
		out = append(out, entry)
	}
	return out, nil
}

// applyCommandFiles layers the job's accumulated command-file state onto one
// step's `docker exec` argv:
//
//   - each written variable becomes `--env NAME=VALUE`, unless THIS step's own
//     `env:` block sets the name (the most specific layer wins, as on GitHub);
//   - PATH entries wrap the step's command so every entry is prepended to the
//     step's own PATH (`PATH="<entries>:$PATH"; export PATH; exec …`): docker's
//     `--env` cannot expand `$PATH`, and the image's PATH must never be
//     replaced.
//
// A docker action, an artifact step or a composite caller is left untouched (no
// command files, no wrapped command).
func applyCommandFiles(p *Plan, step StepPlan, cmds *commandFiles) StepPlan {
	if p == nil || cmds == nil || (len(cmds.env) == 0 && len(cmds.path) == 0) {
		return step
	}
	if step.Kind != "run" && step.Kind != "checkout" {
		return step
	}
	idx := -1
	for i, arg := range step.Argv {
		if arg == p.Names.Container {
			idx = i
			break
		}
	}
	if idx < 0 {
		return step // an argv shape this helper does not own: never mangle it
	}
	envVars := make([]string, 0, len(cmds.env))
	for name := range cmds.env {
		if containsString(step.StepEnv, name) {
			continue // the step's own env: block is more specific
		}
		envVars = append(envVars, name+"="+cmds.env[name])
	}
	sort.Strings(envVars)
	argv := make([]string, 0, len(step.Argv)+2*len(envVars)+3)
	argv = append(argv, step.Argv[:idx]...)
	for _, kv := range envVars {
		argv = append(argv, "--env", kv)
	}
	argv = append(argv, step.Argv[idx])
	cmd := step.Argv[idx+1:]
	if len(cmds.path) > 0 {
		// `sh -c 'script' <prefix> <original argv…>`: $0 is the prefix, "$@" is
		// the original command, so the wrapper adds PATH without touching the
		// shell/argv the step asked for.
		argv = append(argv, "sh", "-c", `PATH="$0:$PATH"; export PATH; exec "$@"`, strings.Join(cmds.path, ":"))
	}
	argv = append(argv, cmd...)
	step.Argv = argv
	return step
}

// collectCommandFiles reads one finished step's command files out of the job
// container and merges them into the job state. A malformed file is a named
// error: when the step itself succeeded, the error fails the step (never a
// silent drop); when it already failed, the caller only logs it.
func (e *Executor) collectCommandFiles(ctx context.Context, p *Plan, step StepPlan, cmds *commandFiles, stream *Streamer) error {
	if p == nil || cmds == nil || (step.EnvFile == "" && step.PathFile == "") {
		return nil
	}
	if ctx.Err() != nil || stream.Cancelled() {
		return nil // the container may have been killed by a timeout/cancel
	}
	if step.EnvFile != "" {
		data, present, err := e.readCommandFile(ctx, p, step.EnvFile, MaxCommandEnvBytes)
		if err != nil {
			return fmt.Errorf("%s (%s): %v", EnvFileVar, step.EnvFile, err)
		}
		if present {
			values, perr := parseEnvFile(data)
			if perr != nil {
				return fmt.Errorf("%s (%s): %v", EnvFileVar, step.EnvFile, perr)
			}
			if aerr := cmds.addEnv(values); aerr != nil {
				return aerr
			}
		}
	}
	if step.PathFile != "" {
		data, present, err := e.readCommandFile(ctx, p, step.PathFile, MaxCommandPathEntries*(MaxCommandPathEntryLen+1))
		if err != nil {
			return fmt.Errorf("%s (%s): %v", PathFileVar, step.PathFile, err)
		}
		if present {
			entries, perr := parsePathFile(data)
			if perr != nil {
				return fmt.Errorf("%s (%s): %v", PathFileVar, step.PathFile, perr)
			}
			if aerr := cmds.addPath(entries); aerr != nil {
				return aerr
			}
		}
	}
	return nil
}

// readCommandFile reads one in-container command file, bounded to limit+1 bytes
// (enough to detect an oversized file without buffering it whole). present is
// false when the step never created the file, which is normal.
func (e *Executor) readCommandFile(ctx context.Context, p *Plan, path string, limit int) ([]byte, bool, error) {
	script := "if [ -f " + shQuote(path) + " ]; then head -c " + strconv.Itoa(limit+1) + " " + shQuote(path) + "; else exit 3; fi"
	out, stderr, code, err := e.Docker.Run(ctx, nil, "exec", p.Names.Container, "sh", "-c", script)
	if code == 3 {
		return nil, false, nil
	}
	if err != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return nil, false, fmt.Errorf("%s: %s", path, msg)
	}
	if len(out) > limit {
		return nil, false, fmt.Errorf("the file is over the cap of %d bytes", limit)
	}
	return []byte(out), true, nil
}
