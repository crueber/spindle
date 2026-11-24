package threads

// Repository-local actions (docs/threads.md §3.4a, docs/features/15 §4.2):
// `uses: ./.spindle/actions/<name>` (any repository-relative path) is resolved
// at RUN CREATION from the run commit and embedded in the job spec. Two
// properties follow from resolving here rather than in the runner:
//
//   - Pinning. The definition a run executes is the one at the run's own
//     commit. The runner has no repository reader, so it cannot be made to
//     fetch an action file from another ref.
//   - The image gate has no hole. Every docker:// image an action names (the
//     action's own docker image, and every docker:// step inside a composite)
//     is visible to ImagePolicy before the job is enqueued, exactly like a job
//     container image. A refusal is a failed job with a Reason and no runner
//     ever starts a container for it.
//
// A resolution failure (missing file, unparseable metadata, wrong `using`, a
// path escaping the repository, a cycle, the depth limit, an undeclared `with:`
// key) fails THAT job with the reason. It is never a silent skip and never a
// run-wide failure: one bad action must not disable unrelated jobs. Remote
// marketplace actions (`owner/repo@ref`) are deliberately out of scope: fetching
// and trusting an arbitrary repository is its own security decision, so they
// keep the built-in refusal ("there is no marketplace").

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// LocalActionPrefix is the spelling that selects a repository-local action.
const LocalActionPrefix = "./"

// actionResolver resolves one run's local actions, caching each parsed
// definition for the jobs of that run (a workflow's jobs usually share the same
// action).
type actionResolver struct {
	git    GitReader
	repo   string
	commit string
	defs   map[string]*workflow.Action
}

// newActionResolver builds the resolver for one repository at the run commit.
func newActionResolver(g GitReader, repo, commit string) *actionResolver {
	return &actionResolver{git: g, repo: repo, commit: commit, defs: map[string]*workflow.Action{}}
}

// fill resolves every local action spec's steps use (recursively) and embeds
// the definitions in spec.Actions. The first refusal is returned as an error
// whose message is stored as the job's Reason; callers fail only that job.
func (r *actionResolver) fill(ctx context.Context, spec *Spec) error {
	if spec == nil || len(spec.Steps) == 0 {
		return nil
	}
	if spec.Actions == nil {
		spec.Actions = map[string]ActionSpec{}
	}
	return r.walk(ctx, spec, spec.Steps, 1, nil, "")
}

// walk visits one step list. stack is the chain of composite directories
// currently open (cycle detection); depth is the composite nesting level of the
// list (the workflow's own steps are depth 1). where names the parent step in
// every error, so a refusal inside a nested action still names the top-level
// step an author can find.
func (r *actionResolver) walk(ctx context.Context, spec *Spec, steps []StepSpec, depth int, stack []string, where string) error {
	for i, st := range steps {
		uses := strings.TrimSpace(st.Uses)
		if !IsLocalAction(uses) {
			continue
		}
		label := fmt.Sprintf("step %d", i+1)
		if where != "" {
			label = where + " step " + fmt.Sprint(i+1)
		}
		dir, err := LocalActionDir(uses)
		if err != nil {
			return fmt.Errorf("%s (uses: %s): %w", label, uses, err)
		}
		def, lerr := r.load(ctx, dir)
		if lerr != nil {
			return fmt.Errorf("%s (uses: %s): %w", label, uses, lerr)
		}
		if bad := def.UndeclaredInputs(st.With); len(bad) > 0 {
			return fmt.Errorf("%s (uses: %s): input %q is not declared by the action (declared: %s)",
				label, uses, bad[0], listOrNone(def.DeclaredInputs()))
		}
		switch def.Runs.Using {
		case workflow.ActionUsingComposite:
			if depth > workflow.MaxActionDepth {
				return fmt.Errorf("%s (uses: %s): composite actions are nested deeper than the limit of %d (a chain this deep is a mistake or a loop)",
					label, uses, workflow.MaxActionDepth)
			}
			if seen := indexOf(stack, dir); seen >= 0 {
				chain := append(append([]string{}, stack[seen:]...), dir)
				return fmt.Errorf("%s (uses: %s): composite action cycle: %s", label, uses, strings.Join(chain, " → "))
			}
			spec.Actions[dir] = actionSpecOf(dir, def)
			if werr := r.walk(ctx, spec, stepSpecs(def.Runs.Steps), depth+1, append(stack, dir), label); werr != nil {
				return werr
			}
		case workflow.ActionUsingDocker:
			spec.Actions[dir] = actionSpecOf(dir, def)
		}
	}
	return nil
}

// load reads and parses one action directory at the run commit, caching the
// result. The lookup order (action.yml, then action.yaml) is GitHub's.
func (r *actionResolver) load(ctx context.Context, dir string) (*workflow.Action, error) {
	if def, ok := r.defs[dir]; ok {
		return def, nil
	}
	if r.git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	var raw []byte
	var err error
	found := ""
	for _, name := range []string{workflow.ActionFileYML, workflow.ActionFileYAML} {
		p := dir + "/" + name
		raw, err = r.git.ReadFile(ctx, r.repo, r.commit, p)
		if err == nil {
			found = p
			break
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("could not read %s at %s: %v", p, shortCommit(r.commit), err)
		}
	}
	if found == "" {
		return nil, fmt.Errorf("no action metadata at %s at the run commit %s (looked for action.yml and action.yaml)",
			dir, shortCommit(r.commit))
	}
	def, perr := workflow.ParseAction(raw)
	if perr != nil {
		return nil, fmt.Errorf("%s at %s: %v", found, shortCommit(r.commit), perr)
	}
	r.defs[dir] = def
	return def, nil
}

// actionSpecOf projects a parsed action onto the job payload shape.
func actionSpecOf(dir string, def *workflow.Action) ActionSpec {
	out := ActionSpec{
		Path: dir, Using: def.Runs.Using,
		Outputs:    map[string]string{},
		Entrypoint: append([]string{}, def.Runs.Entrypoint...),
		Args:       append([]string{}, def.Runs.Args...),
		Env:        copyStringMap(def.Runs.Env),
	}
	if len(def.Inputs) > 0 {
		out.Inputs = make(map[string]ActionInputSpec, len(def.Inputs))
		for name, in := range def.Inputs {
			out.Inputs[name] = ActionInputSpec{Default: in.Default, HasDefault: in.HasDefault, Required: in.Required}
		}
	}
	if len(def.Outputs) > 0 {
		for name, o := range def.Outputs {
			out.Outputs[name] = o.Value
		}
	}
	if def.Runs.Using == workflow.ActionUsingComposite {
		out.Steps = stepSpecs(def.Runs.Steps)
	}
	out.Image = def.Runs.Image
	return out
}

// isLocalAction reports whether a `uses:` names a repository-relative action.
// GitHub's spelling is `./<path>`; a bare `owner/repo@ref` is a remote action
// (out of scope), and a bare `owner/repo` without a ref is not a valid
// marketplace reference either, so both keep the built-in refusal.
func IsLocalAction(uses string) bool {
	return strings.HasPrefix(strings.TrimSpace(uses), LocalActionPrefix)
}

// dockerUsesImage returns the image of a `uses: docker://…` step, or "".
func DockerUsesImage(uses string) string {
	const scheme = "docker://"
	if len(uses) >= len(scheme) && strings.EqualFold(uses[:len(scheme)], scheme) {
		return strings.TrimSpace(uses[len(scheme):])
	}
	return ""
}

// localActionDir validates and cleans a repository-relative action path. The
// result is the repository-relative directory ("repo-root relative, no ./").
// Escapes ("../", absolute paths, backslashes, drive letters) are refused: the
// path is a git object path, never a host path.
func LocalActionDir(uses string) (string, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(uses), LocalActionPrefix)
	switch {
	case raw == "":
		return "", fmt.Errorf("a local action needs a directory after ./")
	case strings.HasPrefix(raw, "/"):
		return "", fmt.Errorf("action path must be repository-relative (no leading /)")
	case strings.Contains(raw, `\`):
		return "", fmt.Errorf("action path must use / separators")
	case strings.Contains(raw, ":"):
		return "", fmt.Errorf("action path must not contain ':'")
	case strings.Contains(raw, "\x00"):
		return "", fmt.Errorf("action path must not contain NUL")
	}
	dir := pathpkg.Clean(raw)
	if dir == "." || dir == "" {
		return "", fmt.Errorf("a local action needs a directory after ./")
	}
	if dir == ".." || strings.HasPrefix(dir, "../") {
		return "", fmt.Errorf("action path %q escapes the repository", uses)
	}
	return dir, nil
}

// shortCommit bounds a commit in an error message (a full sha is noise in a
// job reason; the run page already shows the commit).
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// copyStringMap clones a map (nil-safe).
func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// indexOf returns the position of s in list, or -1.
func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// listOrNone renders a sorted-or-empty list for an error message.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	out := append([]string{}, names...)
	sort.Strings(out)
	return strings.Join(out, ", ")
}
