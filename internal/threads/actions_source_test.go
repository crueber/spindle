package threads

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/actions"
)

// The git adapter and service builder for remote actions (internal/actions
// wiring): a reader without the tree capability fails closed, one with it
// resolves, and the configured source/policy reach the resolver.

// treeFakeGit is a GitReader that ALSO implements the action extensions.
type treeFakeGit struct {
	*fakeGit
	trees   map[string][]actions.TreeEntry
	limited bool
}

// newTreeFake builds the fake with the action's git extensions populated.
func newTreeFake() *treeFakeGit {
	f := newFakeGit()
	f.refs["v1"] = remoteCommit
	f.addIn("acme/tools", remoteCommit, "action.yml", "runs:\n  using: node20\n  main: m.js\n")
	return &treeFakeGit{fakeGit: f, trees: map[string][]actions.TreeEntry{
		"acme/tools@" + remoteCommit: {{Path: "action.yml", Mode: 0o644, Size: 4}},
	}}
}

func (g *treeFakeGit) ListTree(ctx context.Context, repo, commit, dir string) ([]actions.TreeEntry, error) {
	if g.limited {
		return nil, fmt.Errorf("no")
	}
	return g.trees[repo+"@"+commit], nil
}

func (g *treeFakeGit) ReadFileUpTo(ctx context.Context, repo, commit, path string, max int64) ([]byte, error) {
	return g.ReadFile(ctx, repo, commit, path)
}

// plainFakeGit is a GitReader without the extensions.
type plainFakeGit struct{ *fakeGit }

func TestActionGitSource(t *testing.T) {
	full := newTreeFake()
	src := actionGitSource{git: full, max: 1 << 20}
	if got, err := src.ResolveRef(context.Background(), "acme/tools", "v1"); err != nil || got != remoteCommit {
		t.Fatalf("ResolveRef = %q %v", got, err)
	}
	if got, err := src.ReadFile(context.Background(), "acme/tools", remoteCommit, "action.yml"); err != nil || len(got) == 0 {
		t.Fatalf("ReadFile = %q %v", got, err)
	}
	if entries, err := src.ListTree(context.Background(), "acme/tools", remoteCommit, ""); err != nil || len(entries) != 1 {
		t.Fatalf("ListTree = %+v %v", entries, err)
	}
	// A plain reader cannot list: a named refusal, not a partial fetch.
	plain := actionGitSource{git: plainFakeGit{&fakeGit{}}}
	if _, err := plain.ListTree(context.Background(), "acme/tools", remoteCommit, ""); err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("plain ListTree = %v", err)
	}
	// Without the sized reader, ReadFile falls back to the workflow cap.
	if data, err := plain.ReadFile(context.Background(), "acme/tools", remoteCommit, "action.yml"); err == nil {
		t.Fatalf("plain ReadFile of a missing file = %q", data)
	}
}

func TestNewRunActionResolverWiring(t *testing.T) {
	svc := New(nil, nil)
	// No git: a resolver that can only fail closed.
	r := svc.newRunActionResolver("acme/caller", remoteCommit)
	if r.remote != nil {
		t.Fatal("a nil git must not get a remote resolver")
	}
	// A configured external source and policy reach the resolver.
	svc.Git = plainFakeGit{&fakeGit{}}
	svc.ActionPolicy = actions.Policy{Mode: actions.ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}
	svc.ActionSource = "https://git.example.com"
	svc.ActionMaxBytes = 1024
	r = svc.newRunActionResolver("acme/caller", remoteCommit)
	if r.remote == nil {
		t.Fatal("the configured source must be wired")
	}
	if svc.ActionDigests == nil {
		t.Fatal("the digest guard must be created once")
	}
	// An invalid source is ignored (the resolver fails closed at use) but does
	// not panic.
	svc.ActionSource = "ftp://nope"
	if r := svc.newRunActionResolver("acme/caller", remoteCommit); r.remote == nil {
		t.Fatal("the local resolver must still be installed")
	}
}

func TestNormalizeActionPolicy(t *testing.T) {
	if got := normalizeActionPolicy(actions.Policy{}); got.Mode != actions.ModeAllowlist || !got.RequireSHA {
		t.Fatalf("default = %+v", got)
	}
	in := actions.Policy{Mode: "OPEN"}
	if got := normalizeActionPolicy(in); got.Mode != actions.ModeOpen {
		t.Fatalf("explicit = %+v", got)
	}
}

func TestRemoteActionHelpers(t *testing.T) {
	if !IsBuiltinUses("actions/checkout@v4") || !IsBuiltinUses("actions/cache/save") || IsBuiltinUses("acme/x@v1") {
		t.Fatal("builtin detection")
	}
	if !IsRemoteAction("remote://x") || IsRemoteAction("./x") {
		t.Fatal("remote detection")
	}
	ref, _ := actions.ParseRef("https://git.example.com/acme/tools/sub@v1")
	def := &actions.Action{Ref: ref, Commit: remoteCommit}
	key := RemoteActionKey(def)
	if key != "remote://git.example.com/acme/tools/sub@"+remoteCommit {
		t.Fatalf("key = %q", key)
	}
	if sourceName(def) != "git.example.com" {
		t.Fatalf("source = %q", sourceName(def))
	}
	// A path with no ref path has no trailing slash.
	ref2, _ := actions.ParseRef("acme/tools@" + remoteCommit)
	if RemoteActionKey(&actions.Action{Ref: ref2, Commit: remoteCommit}) != "remote://acme/tools@"+remoteCommit {
		t.Fatal("no-path key")
	}
}

// TestActionSpecOfRemoteBranches pins the projection for every runtime.
func TestActionSpecOfRemoteBranches(t *testing.T) {
	r := &actionResolver{}
	ref, _ := actions.ParseRef("acme/tools@" + remoteCommit)
	base := &actions.Action{Ref: ref, Commit: remoteCommit, Digest: "sha256:x", Files: []actions.File{{Path: "action.yml", Mode: 0o644}}}

	composite := *base
	composite.Using = actions.UsingComposite
	composite.Inputs = map[string]actions.Input{"who": {Default: "x", HasDefault: true}}
	composite.Outputs = map[string]string{"o": "${{ steps.a.outputs.o }}"}
	composite.Steps = []actions.Step{{ID: "a", Run: "echo", Shell: "sh"}}
	spec, err := r.actionSpecOfRemote(&composite)
	if err != nil || spec.Using != "composite" || len(spec.Steps) != 1 || spec.Outputs["o"] == "" || spec.Inputs["who"].Default != "x" {
		t.Fatalf("composite = %+v %v", spec, err)
	}

	docker := *base
	docker.Using = actions.UsingDocker
	docker.Image = "docker://alpine:3.20"
	docker.Entrypoint = []string{"/bin/sh"}
	docker.Args = []string{"-c", "x"}
	spec, _ = r.actionSpecOfRemote(&docker)
	if spec.Image != "alpine:3.20" || spec.Dockerfile || len(spec.Args) != 2 {
		t.Fatalf("docker = %+v", spec)
	}
	built := docker
	built.Image = "Dockerfile"
	spec, _ = r.actionSpecOfRemote(&built)
	if !spec.Dockerfile || spec.Image != "Dockerfile" {
		t.Fatalf("dockerfile = %+v", spec)
	}

	node := *base
	node.Using = actions.UsingNode20
	node.Main, node.Pre, node.Post = "m.js", "pre.js", "post.js"
	node.PreIf, node.PostIf = "always()", "always()"
	spec, _ = r.actionSpecOfRemote(&node)
	if spec.Main != "m.js" || spec.Pre != "pre.js" || spec.Post != "post.js" || spec.PreIf == "" || spec.PostIf == "" {
		t.Fatalf("node = %+v", spec)
	}
}

// TestLoadsSortAndDedupe: the run header is stable and deduplicated.
func TestLoadsSortAndDedupe(t *testing.T) {
	r := &actionResolver{loadSeen: map[string]bool{}, remoteKeys: map[string]bool{}}
	r.loads = []ActionLoad{{Repository: "b/x"}, {Repository: "a/x"}, {Repository: "a/x"}}
	// The resolver dedupes on insert; the sort is what this pins.
	out := r.loadsFor()
	if len(out) != 3 || out[0].Repository != "a/x" || out[2].Repository != "b/x" {
		t.Fatalf("loads = %+v", out)
	}
	if (&actionResolver{}).loadsFor() != nil {
		t.Fatal("empty loads must be nil")
	}
}

func TestSetupActionBaseAndWrapEdges(t *testing.T) {
	if base := setupActionBase("not-a-ref"); base != "" {
		t.Fatalf("malformed = %q", base)
	}
	if base := setupActionBase("acme/tools@v1"); base != "" {
		t.Fatalf("non-setup = %q", base)
	}
	if base := setupActionBase("actions/setup-node@v4"); base != "actions/setup-node" {
		t.Fatalf("setup = %q", base)
	}
	r := &actionResolver{}
	in := []StepSpec{{Name: "a", Uses: "./x"}, {Name: "b", Run: "echo"}}
	for i, st := range in {
		out, err := r.wrapSetupCache(&Spec{}, st, "step")
		if err != nil || len(out) != 1 || out[0].Name != st.Name {
			t.Fatalf("%d: %+v %v", i, out, err)
		}
	}
}
