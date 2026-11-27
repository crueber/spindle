package threads

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/actions"
)

// Remote action resolution (PAR-070/071/072/074/096) at the run-creation seam:
// policy → pinned commit → embedded file set → run record.

const remoteCommit = "abcdef0123456789abcdef0123456789abcdef01"

// fakeActionSource is an actions.GitSource over in-memory maps.
type fakeActionSource struct {
	refs  map[string]string
	trees map[string][]actions.TreeEntry
	files map[string][]byte
}

func newFakeActionSource() *fakeActionSource {
	return &fakeActionSource{refs: map[string]string{}, trees: map[string][]actions.TreeEntry{}, files: map[string][]byte{}}
}

func (f *fakeActionSource) add(repo, ref, commit string, files ...actions.File) {
	f.refs[repo+"@"+ref] = commit
	entries := make([]actions.TreeEntry, 0, len(files))
	for _, file := range files {
		f.files[repo+"@"+commit+":"+file.Path] = file.Data
		entries = append(entries, actions.TreeEntry{Path: file.Path, Mode: file.Mode, Size: int64(len(file.Data))})
	}
	f.trees[repo+"@"+commit] = entries
}

func (f *fakeActionSource) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	if sha, ok := f.refs[repo+"@"+ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("%w: unknown", ErrNotFound)
}

func (f *fakeActionSource) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	if data, ok := f.files[repo+"@"+commit+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
}

func (f *fakeActionSource) ListTree(ctx context.Context, repo, commit, dir string) ([]actions.TreeEntry, error) {
	entries, ok := f.trees[repo+"@"+commit]
	if !ok {
		return nil, fmt.Errorf("%w: no tree", ErrNotFound)
	}
	return entries, nil
}

// remoteResolver builds an actionResolver over the fake source and policy.
func remoteResolver(t *testing.T, src *fakeActionSource, policy actions.Policy) *actionResolver {
	t.Helper()
	res := actions.NewResolver(policy, &actions.LocalSource{Git: src}, nil, actions.NewDigestGuard(0), actions.DefaultLimits())
	return newActionResolver(nil, "acme/caller", "1111111111111111111111111111111111111111").withRemote(res, policy)
}

// nodeAction is one remote JavaScript action's metadata.
const nodeActionYAML = `
name: hello
inputs:
  who:
    default: world
runs:
  using: node20
  main: dist/index.js
  post: dist/post.js
  post-if: always()
`

func TestResolveRemoteActionEmbedsAndRecords(t *testing.T) {
	src := newFakeActionSource()
	src.add("acme/tools", remoteCommit, remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(nodeActionYAML)},
		actions.File{Path: "dist/index.js", Mode: 0o644, Data: []byte("console.log(1)\n")},
		actions.File{Path: "dist/post.js", Mode: 0o644, Data: []byte("console.log(2)\n")})
	spec := &Spec{Steps: []StepSpec{{ID: "hello", Uses: "acme/tools@" + remoteCommit, With: map[string]string{"who": "there"}}}}
	if err := remoteResolver(t, src, actions.DefaultPolicy()).fill(context.Background(), spec); err != nil {
		t.Fatalf("fill: %v", err)
	}
	step := spec.Steps[0]
	if !IsRemoteAction(step.Uses) || !strings.HasPrefix(step.Uses, RemoteActionPrefix+"acme/tools@") {
		t.Fatalf("uses = %q", step.Uses)
	}
	def, ok := spec.Actions[step.Uses]
	if !ok {
		t.Fatalf("action %q not embedded (have %v)", step.Uses, spec.Actions)
	}
	if def.Using != "node20" || def.Main != "dist/index.js" || def.Post != "dist/post.js" || !def.ThirdParty {
		t.Fatalf("def = %+v", def)
	}
	if len(def.Files) != 3 || def.Origin == nil || def.Origin.Commit != remoteCommit || def.Origin.Digest == "" {
		t.Fatalf("files/origin = %+v / %+v", def.Files, def.Origin)
	}
	if def.Origin.Policy != "sha-pinned" || def.Origin.Source != "instance" {
		t.Fatalf("origin policy = %+v", def.Origin)
	}
	loads := remoteResolver(t, src, actions.DefaultPolicy()).loadsFor()
	if loads != nil {
		t.Fatalf("a fresh resolver has no loads: %+v", loads)
	}
}

func TestRemoteRunRecord(t *testing.T) {
	src := newFakeActionSource()
	src.add("acme/tools", remoteCommit, remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(nodeActionYAML)},
		actions.File{Path: "dist/index.js", Mode: 0o644, Data: []byte("1")},
		actions.File{Path: "dist/post.js", Mode: 0o644, Data: []byte("2")})
	r := remoteResolver(t, src, actions.DefaultPolicy())
	spec := &Spec{Steps: []StepSpec{
		{Uses: "acme/tools@" + remoteCommit},
		{Uses: "acme/tools@" + remoteCommit},
	}}
	if err := r.fill(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	loads := r.loadsFor()
	if len(loads) != 1 {
		t.Fatalf("loads = %+v (deduplicated by action)", loads)
	}
	if loads[0].Repository != "acme/tools" || loads[0].Commit != remoteCommit || loads[0].Digest == "" || loads[0].Using != "node20" {
		t.Fatalf("load = %+v", loads[0])
	}
}

// TestRemoteMutableRefPolicy pins D-5's shape end to end: a tag outside the
// allowlist is a named refusal; an allowlisted repository's tag runs.
func TestRemoteMutableRefPolicy(t *testing.T) {
	src := newFakeActionSource()
	src.add("acme/tools", "v1", remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(nodeActionYAML)},
		actions.File{Path: "dist/index.js", Mode: 0o644, Data: []byte("1")},
		actions.File{Path: "dist/post.js", Mode: 0o644, Data: []byte("2")})
	spec := &Spec{Steps: []StepSpec{{Uses: "acme/tools@v1"}}}
	err := remoteResolver(t, src, actions.DefaultPolicy()).fill(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "not in threads.actions.allow") || !strings.Contains(err.Error(), "40-character") {
		t.Fatalf("mutable refusal = %v", err)
	}
	spec2 := &Spec{Steps: []StepSpec{{Uses: "acme/tools@v1"}}}
	allowed := actions.Policy{Mode: actions.ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}
	if err := remoteResolver(t, src, allowed).fill(context.Background(), spec2); err != nil {
		t.Fatalf("allowlisted tag: %v", err)
	}
	def := spec2.Actions[spec2.Steps[0].Uses]
	if def.Origin.Policy != "allowlisted" || def.Origin.AllowEntry != "acme/*" {
		t.Fatalf("origin = %+v", def.Origin)
	}
}

// TestNestedRemoteComposite is PAR-074: a fetched composite may `uses:` another
// remote action, depth-capped, and each level embeds its own definition.
func TestNestedRemoteComposite(t *testing.T) {
	src := newFakeActionSource()
	src.add("sigstore/cosign-installer", "v3", remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(`
name: cosign
inputs:
  version:
    default: latest
runs:
  using: composite
  steps:
    - id: install
      shell: bash
      run: echo "install $INPUT_VERSION from $GITHUB_ACTION_PATH"
    - uses: acme/setup-go@` + remoteCommit + `
`)})
	src.add("acme/setup-go", remoteCommit, remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(`
runs:
  using: composite
  steps:
    - shell: sh
      run: echo nested
`)})
	spec := &Spec{Steps: []StepSpec{{ID: "cosign", Uses: "sigstore/cosign-installer@v3", With: map[string]string{"version": "v2"}}}}
	policy := actions.Policy{Mode: actions.ModeAllowlist, Allow: []string{"sigstore/*"}, RequireSHA: true}
	if err := remoteResolver(t, src, policy).fill(context.Background(), spec); err != nil {
		t.Fatalf("fill: %v", err)
	}
	cosign := spec.Actions[spec.Steps[0].Uses]
	if len(cosign.Steps) != 2 {
		t.Fatalf("cosign steps = %+v", cosign.Steps)
	}
	nested := cosign.Steps[1]
	if !IsRemoteAction(nested.Uses) || !strings.Contains(nested.Uses, "acme/setup-go") {
		t.Fatalf("nested uses = %q (must be resolved and rewritten)", nested.Uses)
	}
	if _, ok := spec.Actions[nested.Uses]; !ok {
		t.Fatalf("nested action not embedded: %v", spec.Actions)
	}
}

// TestRemoteCompositeCycleAndDepth pins the loop/depth refusals across remote
// composites (a hostile source cannot make the resolver recurse forever).
func TestRemoteCompositeCycleAndDepth(t *testing.T) {
	src := newFakeActionSource()
	src.add("acme/a", remoteCommit, remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte("runs:\n  using: composite\n  steps:\n    - uses: acme/a@" + remoteCommit + "\n")})
	spec := &Spec{Steps: []StepSpec{{Uses: "acme/a@" + remoteCommit}}}
	err := remoteResolver(t, src, actions.DefaultPolicy()).fill(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle = %v", err)
	}
}

// TestSetupCacheIsWrapped is PAR-096: a resolvable setup-node with `cache: npm`
// becomes [actions/cache, setup-node] with the workspace cache env set.
func TestSetupCacheIsWrapped(t *testing.T) {
	src := newFakeActionSource()
	src.add("actions/setup-node", "v4", remoteCommit,
		actions.File{Path: "action.yml", Mode: 0o644, Data: []byte(`
name: setup node
inputs:
  node-version:
    default: "20"
  cache:
    default: ""
runs:
  using: composite
  steps:
    - shell: sh
      run: echo "node $INPUT_NODE_VERSION cache=$INPUT_CACHE"
`)})
	spec := &Spec{Steps: []StepSpec{{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "22", "cache": "npm"}}}}
	policy := actions.Policy{Mode: actions.ModeAllowlist, Allow: []string{"actions/*"}, RequireSHA: true}
	if err := remoteResolver(t, src, policy).fill(context.Background(), spec); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if len(spec.Steps) != 2 {
		t.Fatalf("steps = %+v", spec.Steps)
	}
	cache := spec.Steps[0]
	if cache.Uses != "actions/cache" || cache.With["path"] != ".npm" || !strings.Contains(cache.With["key"], "npm-") {
		t.Fatalf("cache step = %+v", cache)
	}
	setup := spec.Steps[1]
	if !IsRemoteAction(setup.Uses) {
		t.Fatalf("setup step = %+v", setup)
	}
	if setup.Env["npm_config_cache"] != ".npm" {
		t.Fatalf("setup env = %+v", setup.Env)
	}
	// Without `cache:` there is no wrapper.
	spec2 := &Spec{Steps: []StepSpec{{Uses: "actions/setup-node@v4"}}}
	if err := remoteResolver(t, src, policy).fill(context.Background(), spec2); err != nil || len(spec2.Steps) != 1 {
		t.Fatalf("no cache = %+v %v", spec2.Steps, err)
	}
	// An unmapped manager is refused by name.
	spec3 := &Spec{Steps: []StepSpec{{Uses: "actions/setup-node@v4", With: map[string]string{"cache": "bundler"}}}}
	err := remoteResolver(t, src, policy).fill(context.Background(), spec3)
	if err == nil || !strings.Contains(err.Error(), "not a manager this step maps") {
		t.Fatalf("bundler = %v", err)
	}
}

// TestSetupRefusalNamesTheRecipe: when the setup action cannot resolve, the job
// reason still carries the actions/cache replacement (PAR-096).
func TestSetupRefusalNamesTheRecipe(t *testing.T) {
	src := newFakeActionSource()
	spec := &Spec{Steps: []StepSpec{{Uses: "actions/setup-node@v4", With: map[string]string{"cache": "npm"}}}}
	err := remoteResolver(t, src, actions.DefaultPolicy()).fill(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "actions/cache") || !strings.Contains(err.Error(), "npm_config_cache") {
		t.Fatalf("refusal = %v", err)
	}
}

// TestImageGateSeesRemoteActions: a remote docker action's registry image is
// the same image gate as a job container (PAR-072).
func TestImageGateSeesRemoteActions(t *testing.T) {
	key := RemoteActionPrefix + "acme/img@" + remoteCommit
	spec := &Spec{Steps: []StepSpec{{Uses: key}}, Actions: map[string]ActionSpec{
		key: {Path: "/spindle/remote-actions/x", Using: "docker", Image: "alpine:3.20", ThirdParty: true},
	}}
	refusal, err := (ImagePolicy{RequireDigest: true}).checkJobImages(spec)
	if err != nil || refusal == nil || !strings.Contains(refusal.Reason, "not digest-pinned") {
		t.Fatalf("refusal = %+v %v", refusal, err)
	}
	built := map[string]ActionSpec{
		key: {Path: "/spindle/remote-actions/x", Using: "docker", Image: "Dockerfile", Dockerfile: true, ThirdParty: true},
	}
	if refusal, err := (ImagePolicy{RequireDigest: true}).checkJobImages(&Spec{Steps: []StepSpec{{Uses: key}}, Actions: built}); err != nil || refusal != nil {
		t.Fatalf("a built Dockerfile action has no registry reference: %+v %v", refusal, err)
	}
	// A composite's nested docker step is walked too.
	compositeKey := RemoteActionPrefix + "acme/comp@" + remoteCommit
	spec2 := &Spec{
		Steps: []StepSpec{{Uses: compositeKey}},
		Actions: map[string]ActionSpec{
			compositeKey: {Using: "composite", Steps: []StepSpec{{Uses: key}}},
			key:          {Using: "docker", Image: "alpine:3.20"},
		},
	}
	if refusal, err := (ImagePolicy{RequireDigest: true}).checkJobImages(spec2); err != nil || refusal == nil {
		t.Fatalf("nested image gate = %+v %v", refusal, err)
	}
}
