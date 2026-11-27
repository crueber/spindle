package runner

// JavaScript-action state (PAR-092, docs/threads.md §3.4e and §3.9a).
//
// `$GITHUB_STATE` is the pre→post hand-off of a JavaScript action. The pre hook
// (or the main program) appends `NAME=VALUE` lines to the per-action state file,
// and the post hook receives them as `STATE_<NAME>` environment variables —
// GitHub's documented contract, and the one real actions (`actions/cache`,
// `setup-node`) actually use.
//
// The file is `/spindle/state/<n>`, one per action invocation, OUTSIDE the
// workspace volume: a checked-out repository cannot pre-create or symlink it,
// and a fresh container per job means nothing survives between jobs. The pre,
// main and post steps of ONE invocation share the path, so the file itself is
// also readable at `$GITHUB_STATE` in every phase.
//
// Collected at the end of each phase that wrote it, merged under a bound, and
// delivered to the post hook BY NAME on the docker CLI's environment: a state
// value can be a token (an action caching a credential in its state is
// ordinary), so it never appears in a docker exec argv or the log.
//
// A `::save-state name=X::value` command is the deprecated spelling of a file
// write; both feed the same map and the FILE wins a collision, the same rule
// `$GITHUB_OUTPUT` uses against `::set-output::`.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	// StateEnvVar is the environment variable holding the action's state file.
	StateEnvVar = "GITHUB_STATE"
	// MaxStateBytes bounds ONE action invocation's state file (GitHub bounds
	// `GITHUB_STATE` the same way: state is a hand-off, not a data channel).
	MaxStateBytes = 1 << 20
	// MaxStateEntries bounds the names one action may save: every entry becomes
	// an environment variable of the post hook.
	MaxStateEntries = 128
	// MaxStateValueBytes bounds one state value (it becomes one env value).
	MaxStateValueBytes = 64 << 10
)

// actionState is one job attempt's per-action state, keyed by the state file
// path so two invocations of the same action never share state. Unused (nil) on
// a plan that was hand-built in a test.
type actionState struct {
	mu     sync.Mutex
	values map[string]map[string]string
}

// newActionState builds an empty state map.
func newActionState() *actionState {
	return &actionState{values: map[string]map[string]string{}}
}

// set records one value. The caps are enforced across the whole action: a
// refused entry never leaves a partial state behind.
func (s *actionState) set(path, name, value string) error {
	if s == nil || path == "" {
		return fmt.Errorf("internal: no action state path")
	}
	if !validStateName(name) {
		return fmt.Errorf("state name %q is refused: it becomes the STATE_%s environment variable, so it may contain letters, digits, '.', '_' and '-' (no '=' or whitespace, no leading '-')", name, name)
	}
	if len(value) > MaxStateValueBytes {
		return fmt.Errorf("state value %q is over the cap of %d bytes per value", name, MaxStateValueBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]map[string]string{}
	}
	cur := s.values[path]
	if cur == nil {
		cur = map[string]string{}
		s.values[path] = cur
	}
	if _, ok := cur[name]; !ok && len(cur) >= MaxStateEntries {
		return fmt.Errorf("the action already saved %d state values, over the cap of %d", len(cur), MaxStateEntries)
	}
	cur[name] = value
	return nil
}

// snapshot copies one action's state for its post hook.
func (s *actionState) snapshot(path string) map[string]string {
	if s == nil || path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.values[path]
	if len(cur) == 0 {
		return nil
	}
	out := make(map[string]string, len(cur))
	for k, v := range cur {
		out[k] = v
	}
	return out
}

// validStateName matches the names real actions use. A state name becomes the
// `STATE_<name>` environment variable, and actions/cache's own state key is
// `cache-primary-key` — a hyphen is legitimate in an OS environment name (Node
// reads `process.env["STATE_cache-primary-key"]`), so `.`/`_`/`-` are allowed
// and only '=', whitespace, control characters and a leading '-' are refused.
func validStateName(name string) bool {
	if name == "" || len(name) > 256 || name[0] == '-' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// actionState returns the plan's per-action state map (nil when the plan was
// hand-built without one).
func (p *Plan) actionState() *actionState {
	if p == nil || p.exec == nil {
		return nil
	}
	return p.exec.state
}

// applyActionState injects a post hook's `STATE_<name>` environment (PAR-092).
// The names travel as bare `--env STATE_<name>` elements (docker reads the value
// from the CLI's own environment), so a state value never enters argv.
func applyActionState(p *Plan, step StepPlan, state map[string]string) StepPlan {
	if p == nil || len(state) == 0 || step.Node == nil || !step.Node.IsPost {
		return step
	}
	names := make([]string, 0, len(state))
	for name := range state {
		names = append(names, name)
	}
	sort.Strings(names)
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
	argv := make([]string, 0, len(step.Argv)+2*len(names))
	argv = append(argv, step.Argv[:idx]...)
	step.EnvState = make(map[string]string, len(names))
	for _, name := range names {
		argv = append(argv, "--env", "STATE_"+name)
		step.EnvState["STATE_"+name] = state[name]
	}
	argv = append(argv, step.Argv[idx:]...)
	step.Argv = argv
	return step
}

// collectActionState reads the finished phase's `$GITHUB_STATE` file and merges
// it into the action's state map. A malformed or oversized file fails a step
// that otherwise succeeded (the same rule as `$GITHUB_ENV`); on a failed step
// it is only logged by the caller.
func (e *Executor) collectActionState(ctx context.Context, plan *Plan, step StepPlan, stream StepStream) error {
	state := plan.actionState()
	if state == nil || step.Node == nil || step.Node.StatePath == "" {
		return nil
	}
	if ctx.Err() != nil || stream.Cancelled() {
		return nil // the container may have been killed by a timeout/cancel
	}
	data, present, err := e.readCommandFile(ctx, plan, step.Node.StatePath, MaxStateBytes)
	if err != nil {
		return fmt.Errorf("%s (%s): %v", StateEnvVar, step.Node.StatePath, err)
	}
	if !present {
		return nil
	}
	entries, perr := parseStateFile(data)
	if perr != nil {
		return fmt.Errorf("%s (%s): %v", StateEnvVar, step.Node.StatePath, perr)
	}
	for _, entry := range entries {
		if serr := state.set(step.Node.StatePath, entry.Name, entry.Value); serr != nil {
			return fmt.Errorf("%s (%s): %v", StateEnvVar, step.Node.StatePath, serr)
		}
	}
	return nil
}

// stateEntry is one parsed state line.
type stateEntry struct {
	Name  string
	Value string
}

// parseStateFile parses `$GITHUB_STATE`: one `NAME=VALUE` per line (the value
// may contain `=`), blank lines ignored, CRLF tolerated, last value wins. A
// heredoc, an empty name or a line without `=` is a NAMED failure: a state value
// is never guessed.
func parseStateFile(data []byte) ([]stateEntry, error) {
	var out []stateEntry
	seen := map[string]int{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected NAME=VALUE (a heredoc belongs to $GITHUB_OUTPUT)", i+1)
		}
		name, value := line[:eq], line[eq+1:]
		if !validStateName(name) {
			return nil, fmt.Errorf("line %d: state name %q may contain letters, digits, '.', '_' and '-' (no '=' or whitespace)", i+1, name)
		}
		if len(value) > MaxStateValueBytes {
			return nil, fmt.Errorf("line %d: the value of %q is over the cap of %d bytes", i+1, name, MaxStateValueBytes)
		}
		if first, ok := seen[name]; ok {
			// Last wins: drop the earlier entry so the merged map cannot depend
			// on map iteration order.
			out[first].Value = value
			continue
		}
		seen[name] = len(out)
		out = append(out, stateEntry{Name: name, Value: value})
		if len(out) > MaxStateEntries {
			return nil, fmt.Errorf("over the cap of %d state values", MaxStateEntries)
		}
	}
	return out, nil
}
