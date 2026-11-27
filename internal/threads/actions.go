package threads

// Action resolution at run creation (docs/threads.md §3.4a/§3.4d).
//
// Two families are resolved here, before a job is enqueued:
//
//   - Repository-local actions (`uses: ./.spindle/actions/<name>`) are read at
//     the RUN commit and embedded in the job spec. Pinning follows from
//     resolving here: the definition a run executes is the one at the run's own
//     commit, and the runner has no repository reader, so it cannot be made to
//     fetch an action file from another ref.
//   - Remote actions (`owner/repo[/path]@ref`, PAR-070) are authorised by the
//     instance policy, resolved to a commit, hashed, and embedded with their
//     file set. The digest and commit are recorded on the run header
//     (Run.ActionLoads) and printed in the job log by the runner. This instance
//     is the only default source; an external source exists only when the
//     operator configured one (threads.actions.source).
//
// The image gate has no hole in either family: every docker:// image an action
// names (the action's own image, and every docker:// step inside a composite)
// is visible to ImagePolicy before the job is enqueued, exactly like a job
// container image. A refusal is a failed job with a Reason and no runner ever
// starts a container for it.
//
// A resolution failure (missing file, unparseable metadata, wrong `using`, a
// path escaping the repository, a cycle, the depth limit, an undeclared `with:`
// key, a mutable ref outside the allowlist, an over-size fetch) fails THAT job
// with the reason. It is never a silent skip and never a run-wide failure: one
// bad action must not disable unrelated jobs.

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"sync"

	"github.com/crueber/spindle/internal/actions"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// LocalActionPrefix is the spelling that selects a repository-local action.
const LocalActionPrefix = "./"

// RemoteActionPrefix marks an embedded remote action in a StepSpec.Uses. The
// server rewrites a remote `uses:` to this key and embeds the definition in
// Spec.Actions; the runner looks it up and materializes it.
const RemoteActionPrefix = "remote://"

// RemoteActionsRoot is the in-container directory remote action content is
// materialized into. It is deliberately OUTSIDE the workspace volume: the
// checked-out repository cannot pre-create a path there (no symlink traversal),
// and a fresh container per job means nothing survives between jobs.
const RemoteActionsRoot = "/spindle/remote-actions"

// RemoteStateRoot is the in-container directory holding JavaScript actions'
// $GITHUB_STATE files (one per action invocation), also outside the workspace
// volume so a repository cannot pre-create or redirect one.
const RemoteStateRoot = "/spindle/state"

// maxRemoteActionBytes bounds the action content one JOB may carry (distinct
// from the per-action bound). It keeps a hostile workflow from turning the job
// spec into a megabyte-scale object by naming many actions.
const maxRemoteActionBytes = 64 << 20

// maxRemoteActionFiles bounds the file records in one job spec.
const maxRemoteActionFiles = 8192

// IsRemoteAction reports whether a `uses:` names an embedded remote action.
func IsRemoteAction(uses string) bool {
	return strings.HasPrefix(strings.TrimSpace(uses), RemoteActionPrefix)
}

// RemoteActionKey returns the Spec.Actions key for a remote reference.
func RemoteActionKey(a *actions.Action) string {
	id := a.Ref.Identity()
	if a.Ref.Path != "" {
		id += "/" + a.Ref.Path
	}
	return RemoteActionPrefix + id + "@" + a.Commit
}

// actionResolver resolves one run's actions, caching each parsed definition for
// the jobs of that run (a workflow's jobs usually share the same action).
type actionResolver struct {
	git    GitReader
	repo   string
	commit string
	defs   map[string]*workflow.Action

	// remote resolves marketplace references; nil keeps the built-in refusal
	// path (tests that exercise only local actions).
	remote *actions.Resolver
	// allowedActions is the raw `threads.actions.allow` list, for the record.
	policy actions.Policy

	mu          sync.Mutex
	loads       []ActionLoad
	loadSeen    map[string]bool
	remoteKeys  map[string]bool
	remoteBytes int
	remoteFiles int
}

// newActionResolver builds the resolver for one repository at the run commit.
func newActionResolver(g GitReader, repo, commit string) *actionResolver {
	return &actionResolver{git: g, repo: repo, commit: commit,
		defs: map[string]*workflow.Action{}, loadSeen: map[string]bool{}, remoteKeys: map[string]bool{}}
}

// withRemote installs the remote-action resolver and its policy.
func (r *actionResolver) withRemote(res *actions.Resolver, policy actions.Policy) *actionResolver {
	r.remote = res
	r.policy = policy
	return r
}

// loadsFor returns the run's deduplicated action resolution record, sorted for
// a stable run header.
func (r *actionResolver) loadsFor() []ActionLoad {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.loads) == 0 {
		return nil
	}
	out := append([]ActionLoad{}, r.loads...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// fill resolves every action spec's steps use (recursively) and embeds the
// definitions in spec.Actions. The first refusal is returned as an error whose
// message is stored as the job's Reason; callers fail only that job.
func (r *actionResolver) fill(ctx context.Context, spec *Spec) error {
	if spec == nil || len(spec.Steps) == 0 {
		return nil
	}
	if spec.Actions == nil {
		spec.Actions = map[string]ActionSpec{}
	}
	steps, err := r.walk(ctx, spec, spec.Steps, 1, nil, "")
	if err != nil {
		return err
	}
	spec.Steps = steps
	return nil
}

// walk visits one step list. stack is the chain of composite actions currently
// open (cycle detection); depth is the composite nesting level of the list (the
// workflow's own steps are depth 1). where names the parent step in every
// error, so a refusal inside a nested action still names the top-level step an
// author can find.
func (r *actionResolver) walk(ctx context.Context, spec *Spec, steps []StepSpec, depth int, stack []string, where string) ([]StepSpec, error) {
	out := make([]StepSpec, 0, len(steps))
	for i := range steps {
		st := steps[i]
		uses := strings.TrimSpace(st.Uses)
		label := fmt.Sprintf("step %d", i+1)
		if where != "" {
			label = where + " step " + fmt.Sprint(i+1)
		}
		switch {
		case uses == "":
			out = append(out, st)
			continue
		case IsLocalAction(uses):
			if err := r.resolveLocal(ctx, spec, &st, label, depth, stack); err != nil {
				return nil, err
			}
		case DockerUsesImage(uses) != "":
			// The runner plans the image; the image gate sees it.
		case IsBuiltinUses(uses):
			// A built-in action keeps the runner's implementation.
		default:
			if err := r.resolveRemote(ctx, spec, &st, label, depth, stack); err != nil {
				// A setup-* action that cannot run still gets the cache recipe
				// (PAR-096): the refusal names the exact actions/cache
				// replacement instead of only the resolution failure.
				if base := setupActionBase(uses); base != "" {
					return nil, fmt.Errorf("%s (uses: %s): %s", label, uses, actions.SetupRefusal(base, st.With, err.Error()))
				}
				return nil, err
			}
		}
		// A setup-* action's cache input is rewritten around a built-in cache
		// pair (PAR-096) after its own resolution, so a resolvable setup action
		// restores and saves with no separate cache step.
		wrapped, err := r.wrapSetupCache(spec, st, label)
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped...)
	}
	return out, nil
}

// setupActionBase returns the "actions/setup-*" base of a remote reference, or
// "" when it is not a setup action.
func setupActionBase(uses string) string {
	ref, err := actions.ParseRef(uses)
	if err != nil {
		return ""
	}
	base := strings.ToLower(ref.Repository())
	if strings.HasPrefix(base, "actions/setup-") {
		return base
	}
	return ""
}

// wrapSetupCache rewrites a resolved setup-* action around the built-in cache
// pair its `cache:` input promises (PAR-096). The action itself still runs; the
// cache restore happens before it and the save is the built-in action's
// deferred save at the end of a green job. An input this table does not map is
// a named refusal, never a silent no-cache.
func (r *actionResolver) wrapSetupCache(spec *Spec, st StepSpec, label string) ([]StepSpec, error) {
	if !IsRemoteAction(st.Uses) {
		return []StepSpec{st}, nil
	}
	def, ok := spec.Actions[st.Uses]
	if !ok || def.Origin == nil {
		return []StepSpec{st}, nil
	}
	base := strings.ToLower(def.Origin.Repository)
	cache, asked := st.With["cache"]
	if !strings.HasPrefix(base, "actions/setup-") || !asked || strings.TrimSpace(cache) == "" {
		return []StepSpec{st}, nil
	}
	c, supported, reason := actions.SetupCacheInput(base, cache)
	if !supported {
		return nil, fmt.Errorf("%s (uses: %s): %s", label, st.Uses, reason)
	}
	env := copyStringMap(st.Env)
	if env == nil {
		env = map[string]string{}
	}
	for k, v := range c.Env {
		env[k] = v
	}
	st.Env = env
	cacheStep := StepSpec{
		Name: "cache (" + base + ")",
		Uses: "actions/cache",
		With: map[string]string{
			"path": c.Paths, "key": c.Key, "restore-keys": c.RestoreKey,
		},
	}
	return []StepSpec{cacheStep, st}, nil
}

// resolveLocal expands one `uses: ./…` step, recursively embedding composites.
func (r *actionResolver) resolveLocal(ctx context.Context, spec *Spec, st *StepSpec, label string, depth int, stack []string) error {
	uses := strings.TrimSpace(st.Uses)
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
		if err := checkActionDepth(label, uses, depth); err != nil {
			return err
		}
		if seen := indexOf(stack, dir); seen >= 0 {
			chain := append(append([]string{}, stack[seen:]...), dir)
			return fmt.Errorf("%s (uses: %s): composite action cycle: %s", label, uses, strings.Join(chain, " → "))
		}
		as := actionSpecOf(dir, def)
		// walk rewrites a nested remote `uses:` inside as.Steps in place; store
		// the walked copy (spec.Actions is a map, so the value must be assigned
		// after the walk).
		walked, werr := r.walk(ctx, spec, as.Steps, depth+1, append(stack, dir), label)
		if werr != nil {
			return werr
		}
		as.Steps = walked
		spec.Actions[dir] = as
	case workflow.ActionUsingDocker:
		spec.Actions[dir] = actionSpecOf(dir, def)
	}
	return nil
}

// resolveRemote authorises, fetches and embeds one remote action (PAR-070). A
// resolved action's `uses:` is rewritten to RemoteActionPrefix+key, so the
// runner never re-derives a source, ref or network call.
func (r *actionResolver) resolveRemote(ctx context.Context, spec *Spec, st *StepSpec, label string, depth int, stack []string) error {
	uses := strings.TrimSpace(st.Uses)
	if r.remote == nil {
		return fmt.Errorf("%s (uses: %s): remote actions are not wired on this instance (no action source); use a repository-local action (uses: ./<path>) or a docker:// image", label, uses)
	}
	def, err := r.remote.Resolve(ctx, uses)
	if err != nil {
		return fmt.Errorf("%s (uses: %s): %w", label, uses, err)
	}
	key := RemoteActionKey(def)
	// The same reference used twice in one job (or by two jobs) is one embed.
	// The definition is already content-pinned, so sharing is safe.
	if spec.Actions[key].Path != "" && r.remoteKeys[key] {
		st.Uses = key
		return nil
	}
	if err := checkActionDepth(label, uses, depth); err != nil {
		return err
	}
	if seen := indexOf(stack, key); seen >= 0 {
		chain := append(append([]string{}, stack[seen:]...), key)
		return fmt.Errorf("%s (uses: %s): action cycle: %s", label, uses, strings.Join(chain, " → "))
	}
	as, err := r.actionSpecOfRemote(def)
	if err != nil {
		return fmt.Errorf("%s (uses: %s): %w", label, uses, err)
	}
	r.mu.Lock()
	if !r.remoteKeys[key] {
		r.remoteKeys[key] = true
		r.remoteFiles += len(def.Files)
		for _, f := range def.Files {
			r.remoteBytes += len(f.Data)
		}
	}
	overBytes := r.remoteBytes > maxRemoteActionBytes
	overFiles := r.remoteFiles > maxRemoteActionFiles
	actionCount := len(r.remoteKeys)
	if !r.loadSeen[key] {
		r.loadSeen[key] = true
		r.loads = append(r.loads, ActionLoad{
			Source: sourceName(def), Repository: def.Ref.Repository(), Path: def.Ref.Path,
			Ref: def.Ref.Ref, Commit: def.Commit, Digest: def.Digest, Using: def.Using,
			Policy: policyName(r.policy, def.Ref), AllowEntry: allowEntry(r.policy, def.Ref),
		})
	}
	r.mu.Unlock()
	if overBytes {
		return fmt.Errorf("%s (uses: %s): the job's remote action content is over the limit of %d bytes (threads.actions.max_bytes; %d actions embedded)", label, uses, maxRemoteActionBytes, actionCount)
	}
	if overFiles {
		return fmt.Errorf("%s (uses: %s): the job's remote action content has more than %d files (threads.actions.max_bytes)", label, uses, maxRemoteActionFiles)
	}
	st.Uses = key
	if def.Using == actions.UsingComposite {
		walked, werr := r.walk(ctx, spec, as.Steps, depth+1, append(stack, key), label)
		if werr != nil {
			return werr
		}
		as.Steps = walked
	}
	spec.Actions[key] = as
	return nil
}

// actionSpecOfRemote projects a fetched remote action onto the job payload.
func (r *actionResolver) actionSpecOfRemote(def *actions.Action) (ActionSpec, error) {
	out := ActionSpec{
		Path:       RemoteActionsRoot + "/" + def.DirName,
		Using:      def.Using,
		Outputs:    map[string]string{},
		Entrypoint: append([]string{}, def.Entrypoint...),
		Args:       append([]string{}, def.Args...),
		Env:        copyStringMap(def.Env),
		Main:       def.Main, Pre: def.Pre, Post: def.Post,
		PreIf: def.PreIf, PostIf: def.PostIf,
		Origin: &ActionOrigin{
			Source: sourceName(def), Repository: def.Ref.Repository(), Path: def.Ref.Path,
			Ref: def.Ref.Ref, Commit: def.Commit, Digest: def.Digest, Using: def.Using,
			Policy: policyName(r.policy, def.Ref), AllowEntry: allowEntry(r.policy, def.Ref),
		},
		ThirdParty: true,
	}
	if len(def.Inputs) > 0 {
		out.Inputs = make(map[string]ActionInputSpec, len(def.Inputs))
		for name, in := range def.Inputs {
			out.Inputs[name] = ActionInputSpec{Default: in.Default, HasDefault: in.HasDefault, Required: in.Required}
		}
	}
	for name, value := range def.Outputs {
		out.Outputs[name] = value
	}
	out.Image = def.Image
	if def.Using == actions.UsingDocker {
		out.Dockerfile = def.IsDockerfile()
		if !out.Dockerfile {
			out.Image = strings.TrimPrefix(def.Image, "docker://")
		}
	}
	for _, f := range def.Files {
		out.Files = append(out.Files, ActionFile{Path: f.Path, Mode: f.Mode, Data: f.Data})
	}
	if def.Using == actions.UsingComposite {
		out.Steps = remoteStepSpecs(def.Steps)
	}
	return out, nil
}

// remoteStepSpecs converts a remote composite's parsed steps to the payload's
// step shape. A nested remote `uses:` stays a remote reference; walk() resolves
// it and rewrites it in place.
func remoteStepSpecs(steps []actions.Step) []StepSpec {
	out := make([]StepSpec, 0, len(steps))
	for _, st := range steps {
		out = append(out, StepSpec{
			ID: st.ID, Name: st.Name, Run: st.Run, Uses: st.Uses,
			With: copyStringMap(st.With), Env: copyStringMap(st.Env), If: st.If,
			Shell: st.Shell, WorkingDirectory: st.WorkingDirectory,
			ContinueOnError: st.ContinueOnError, TimeoutMinutes: st.TimeoutMinutes,
		})
	}
	return out
}

// checkActionDepth enforces the composite nesting cap for both families.
func checkActionDepth(label, uses string, depth int) error {
	if depth > workflow.MaxActionDepth {
		return fmt.Errorf("%s (uses: %s): composite actions are nested deeper than the limit of %d (a chain this deep is a mistake or a loop)",
			label, uses, workflow.MaxActionDepth)
	}
	return nil
}

// sourceName names the provenance of one action in the record.
func sourceName(def *actions.Action) string {
	if def.Ref.Host != "" {
		return def.Ref.Host
	}
	return "instance"
}

// policyName names the decision that allowed the reference.
func policyName(policy actions.Policy, ref actions.Ref) string {
	if ref.CommitSHA() {
		return "sha-pinned"
	}
	if policy.Normalize().Mode == actions.ModeOpen {
		return "open"
	}
	return "allowlisted"
}

// allowEntry returns the matched allowlist entry for the record.
func allowEntry(policy actions.Policy, ref actions.Ref) string {
	return policy.Decide(ref).AllowEntry
}

// load reads and parses one local action directory at the run commit, caching
// the result. The lookup order (action.yml, then action.yaml) is GitHub's.
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

// actionSpecOf projects a parsed local action onto the job payload shape.
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
// GitHub's spelling is `./<path>`.
func IsLocalAction(uses string) bool {
	return strings.HasPrefix(strings.TrimSpace(uses), LocalActionPrefix)
}

// IsBuiltinUses reports whether a `uses:` names one of the runner's built-in
// actions (the ones that are not fetched from any repository).
func IsBuiltinUses(uses string) bool {
	base := strings.ToLower(strings.TrimSpace(uses))
	if at := strings.IndexByte(base, '@'); at >= 0 {
		base = base[:at]
	}
	switch base {
	case "actions/checkout", "actions/upload-artifact", "actions/download-artifact",
		"actions/cache", "actions/cache/restore", "actions/cache/save":
		return true
	}
	return false
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
