package actions

import (
	"context"
	"strings"
	"testing"
)

// TestResolverPinsAndRecords pins the supply-chain contract: the ref resolves to
// a commit, the content to a digest, both are on the returned action, and the
// file set is exactly what the metadata needs.
func TestResolverPinsAndRecords(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", "v1", testCommit, fakeActionFiles()...)
	r := NewResolver(Policy{Mode: ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}, &LocalSource{Git: g}, nil, NewDigestGuard(0), DefaultLimits())
	a, err := r.Resolve(context.Background(), "acme/tools@v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a.Commit != testCommit || !strings.HasPrefix(a.Digest, "sha256:") || a.Using != UsingNode20 {
		t.Fatalf("action = %+v", a)
	}
	if a.DirName == "" || a.Files[0].Path != "action.yml" {
		t.Fatalf("files = %+v dir = %q", a.Files, a.DirName)
	}
	// The second use of the same reference is one fetch (the run-level cache).
	calls := g.calls
	if _, err := r.Resolve(context.Background(), "acme/tools@v1"); err != nil {
		t.Fatal(err)
	}
	if g.calls != calls {
		t.Fatalf("resolve fetched twice (%d → %d)", calls, g.calls)
	}
}

func TestResolverRefusals(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", testCommit, testCommit, fakeActionFiles()...)
	g.add("acme/tools", "v1", testCommit, append(fakeActionFiles(),
		File{Path: "broken.yml", Mode: 0o644})...)
	// A repository whose metadata names a main file it does not carry.
	g.add("acme/missing", testCommit, testCommit,
		File{Path: "action.yml", Mode: 0o644, Data: []byte("runs:\n  using: node20\n  main: gone.js\n")})
	// A Dockerfile action whose Dockerfile is absent.
	g.add("acme/nodockerfile", testCommit, testCommit,
		File{Path: "action.yml", Mode: 0o644, Data: []byte("runs:\n  using: docker\n  image: Dockerfile\n")})
	// A Dockerfile action that carries it.
	g.add("acme/dockerfile", testCommit, testCommit,
		File{Path: "action.yml", Mode: 0o644, Data: []byte("runs:\n  using: docker\n  image: Dockerfile\n")},
		File{Path: "Dockerfile", Mode: 0o644, Data: []byte("FROM scratch\n")})
	// Malformed metadata.
	g.add("acme/malformed", testCommit, testCommit,
		File{Path: "action.yml", Mode: 0o644, Data: []byte("runs: [")})
	// No metadata at all.
	g.add("acme/nometa", testCommit, testCommit, File{Path: "README.md", Mode: 0o644, Data: []byte("x")})

	for _, tc := range []struct {
		name  string
		uses  string
		wd    *HTTPSource
		guard *DigestGuard
		want  string
	}{
		{"default-deny mutable", "acme/tools@v1", nil, nil, "not in threads.actions.allow"},
		{"deny mode", "acme/tools@" + testCommit, nil, nil, "disabled"},
		{"not found", "acme/nope@" + testCommit, nil, nil, "not found on this instance"},
		{"missing main", "acme/missing@" + testCommit, nil, nil, `"gone.js"`},
		{"missing Dockerfile", "acme/nodockerfile@" + testCommit, nil, nil, `"Dockerfile"`},
		{"malformed", "acme/malformed@" + testCommit, nil, nil, "action metadata"},
		{"no metadata", "acme/nometa@" + testCommit, nil, nil, "no action metadata"},
		{"url without source", "https://code.forgejo.org/actions/setup-go@v5", nil, nil, "no threads.actions.source configured"},
	} {
		policy := DefaultPolicy()
		if tc.name == "deny mode" {
			policy = Policy{Mode: ModeDeny}
		}
		r := NewResolver(policy, &LocalSource{Git: g}, tc.wd, tc.guard, DefaultLimits())
		_, err := r.Resolve(context.Background(), tc.uses)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// The Dockerfile action resolves.
	r := NewResolver(DefaultPolicy(), &LocalSource{Git: g}, nil, nil, DefaultLimits())
	a, err := r.Resolve(context.Background(), "acme/dockerfile@"+testCommit)
	if err != nil || !a.IsDockerfile() {
		t.Fatalf("dockerfile = %+v %v", a, err)
	}
}

func TestResolverAllowlistAndGuard(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", "v1", testCommit, fakeActionFiles()...)
	guard := NewDigestGuard(0)
	policy := Policy{Mode: ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}
	r := NewResolver(policy, &LocalSource{Git: g}, nil, guard, DefaultLimits())
	if _, err := r.Resolve(context.Background(), "acme/tools@v1"); err != nil {
		t.Fatalf("allowlisted tag: %v", err)
	}
	// A fresh resolver (another run) sees the same commit; unchanged bytes pass.
	r2 := NewResolver(policy, &LocalSource{Git: g}, nil, guard, DefaultLimits())
	if _, err := r2.Resolve(context.Background(), "acme/tools@v1"); err != nil {
		t.Fatalf("unchanged content: %v", err)
	}
	// The source now serves different bytes under the same commit: refused, not
	// served from cache.
	g.files["acme/tools@"+testCommit+":action.yml"] = []byte("name: tampered\nruns:\n  using: node20\n  main: dist/index.js\n")
	r3 := NewResolver(policy, &LocalSource{Git: g}, nil, guard, DefaultLimits())
	if _, err := r3.Resolve(context.Background(), "acme/tools@v1"); err == nil || !strings.Contains(err.Error(), "changed content under the same commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolverExternalSource(t *testing.T) {
	// An external source consumes bare references and must match a full URL's
	// host.
	ext := &HTTPSource{Base: mustURL(t, "https://git.example.com"), Limits: DefaultLimits()}
	r := NewResolver(DefaultPolicy(), nil, ext, nil, DefaultLimits())
	ref, _ := ParseRef("https://other.example/acme/tools@v1")
	if _, err := r.sourceForOnly(ref); err == nil || !strings.Contains(err.Error(), "not the configured action source") {
		t.Fatalf("host mismatch = %v", err)
	}
	ref2, _ := ParseRef("https://git.example.com/acme/tools@v1")
	if _, err := r.sourceForOnly(ref2); err != nil {
		t.Fatalf("matching host = %v", err)
	}
	plain, _ := ParseRef("acme/tools@v1")
	if _, err := r.sourceForOnly(plain); err != nil {
		t.Fatalf("bare ref with a source = %v", err)
	}
	// With no local source and no configured source, a bare ref is refused.
	if _, err := NewResolver(DefaultPolicy(), nil, nil, nil, DefaultLimits()).sourceForOnly(plain); err == nil {
		t.Fatal("no source must be refused")
	}
}

// sourceForOnly exposes source selection without a fetch.
func (r *Resolver) sourceForOnly(ref Ref) (source, error) { return r.sourceFor(ref) }
