package actions

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMoreParserEdges(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"bad output name", "outputs:\n  'a b':\n    value: x\nruns:\n  using: node20\n  main: m.js\n", "not a valid output name"},
		{"composite entrypoint", "runs:\n  using: composite\n  entrypoint: /bin/sh\n  steps:\n    - run: x\n      shell: sh\n", "entrypoint/args/env"},
		{"composite args", "runs:\n  using: composite\n  args: [x]\n  steps:\n    - run: x\n      shell: sh\n", "entrypoint/args/env"},
		{"composite env", "runs:\n  using: composite\n  env:\n    A: b\n  steps:\n    - run: x\n      shell: sh\n", "entrypoint/args/env"},
		{"composite pre-if", "runs:\n  using: composite\n  pre-if: always()\n  steps:\n    - run: x\n      shell: sh\n", "entrypoint/args/env"},
		{"composite empty docker", "runs:\n  using: composite\n  steps:\n    - uses: docker://\n", "needs an image reference"},
		{"composite bad id", "runs:\n  using: composite\n  steps:\n    - id: 'a b'\n      run: x\n      shell: sh\n", "not a valid step id"},
		{"composite bad remote ref", "runs:\n  using: composite\n  steps:\n    - uses: owner/repo\n", "needs @<ref>"},
		{"docker with steps", "runs:\n  using: docker\n  image: docker://x\n  steps:\n    - run: x\n", "docker action has using"},
		{"docker empty docker://", "runs:\n  using: docker\n  image: 'docker://'\n", "needs an image reference"},
		{"docker path escape", "runs:\n  using: docker\n  image: ../Dockerfile\n", "escapes"},
		{"docker not a dockerfile", "runs:\n  using: docker\n  image: scripts/run.sh\n", "neither a docker:// image nor a Dockerfile"},
		{"node post escape", "runs:\n  using: node20\n  main: m.js\n  post: ../post.js\n", "escapes"},
		{"node pre escape", "runs:\n  using: node20\n  main: m.js\n  pre: /pre.js\n", "must be action-directory-relative"},
	} {
		_, err := ParseAction([]byte(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// An alias node decodes as its anchor.
	var s scalar
	if err := s.UnmarshalYAML(&yaml.Node{Kind: yaml.AliasNode, Alias: &yaml.Node{Kind: yaml.ScalarNode, Value: "hello"}}); err != nil || string(s) != "hello" {
		t.Fatalf("alias = %q %v", s, err)
	}
	if stringMap(map[string]any{"k": map[string]any{"x": 1}})["k"] == "" {
		t.Fatal("stringMap must text-format an unexpected scalar")
	}
}

func TestRefNameEdges(t *testing.T) {
	for _, ref := range []string{"", "-x", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a..b", "a.", "a/", "/a", "a@{b"} {
		if err := validRefName(ref); err == nil {
			t.Fatalf("%q accepted", ref)
		}
	}
	for _, ref := range []string{"v1", "refs/heads/main", testCommit, "release/1.2"} {
		if err := validRefName(ref); err != nil {
			t.Fatalf("%q refused: %v", ref, err)
		}
	}
	// http schemes are accepted only as a loopback source; the parser keeps the
	// scheme for the resolver to decide.
	ref, err := ParseRef("http://127.0.0.1:9999/a/b@v1")
	if err != nil || ref.Scheme != "http" {
		t.Fatalf("loopback http = %+v %v", ref, err)
	}
}

func TestResolverCommitShapeAndNil(t *testing.T) {
	r := NewResolver(DefaultPolicy(), &LocalSource{Git: shortSHAGit{}}, nil, nil, DefaultLimits())
	if _, err := r.Resolve(context.Background(), "acme/tools@"+testCommit); err == nil || !strings.Contains(err.Error(), "not a full commit SHA") {
		t.Fatalf("short commit = %v", err)
	}
	// A source with no git wired is a named refusal.
	r2 := NewResolver(DefaultPolicy(), &LocalSource{}, nil, nil, DefaultLimits())
	if _, err := r2.Resolve(context.Background(), "acme/tools@"+testCommit); err == nil || !strings.Contains(err.Error(), "no git reader") {
		t.Fatalf("nil git = %v", err)
	}
	// Digest refuses a path that cannot be represented.
	if _, err := Digest([]File{{Path: "a\x00b", Data: []byte("x")}}); err == nil {
		t.Fatal("NUL in a path must be refused")
	}
}

type shortSHAGit struct{}

func (shortSHAGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	return "abc123", nil
}
func (shortSHAGit) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	return nil, nil
}
func (shortSHAGit) ListTree(ctx context.Context, repo, commit, dir string) ([]TreeEntry, error) {
	return nil, nil
}

func TestLocalSourceEntryEdges(t *testing.T) {
	g := newFakeGit()
	g.refs["acme/tools@"+testCommit] = testCommit
	g.trees["acme/tools@"+testCommit] = []TreeEntry{{Path: "", Mode: 0o100644}}
	src := &LocalSource{Git: g}
	ref, _ := ParseRef("acme/tools@" + testCommit)
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("empty entry path = %v", err)
	}
	// A listing failure is a named not-found.
	if _, err := src.Fetch(context.Background(), ref, "ffffffffffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("no tree must fail")
	}
}

func TestHTTPFetchSubpathFilterAndEmptyRoot(t *testing.T) {
	archive := makeTarRaw(t, []tarEntry{
		{name: "./repo-abc/sub/action.yml", body: "runs:\n  using: node20\n  main: m.js\n"},
		{name: "./repo-abc/sub/m.js", body: "1"},
		{name: "./repo-abc/other.txt", body: "skip"},
	})
	srv := newTLSServerBytes(t, archive)
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: srv.Client()}
	ref, _ := ParseRef("acme/tools/sub@" + testCommit)
	files, err := src.Fetch(context.Background(), ref, testCommit)
	if err != nil || len(files) != 2 {
		t.Fatalf("subpath files = %+v %v", files, err)
	}
	empty := makeTarRaw(t, []tarEntry{{name: "/", typeflag: '5'}})
	srv2 := newTLSServerBytes(t, empty)
	src2 := &HTTPSource{Base: mustURL(t, srv2.URL), Client: srv2.Client()}
	if _, err := src2.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("empty archive = %v", err)
	}
}
