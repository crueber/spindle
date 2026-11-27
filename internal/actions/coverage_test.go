package actions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Coverage edges: the branches that a table of happy shapes does not reach.

func TestScalarAndListEdges(t *testing.T) {
	for _, src := range []string{
		"runs:\n  using: docker\n  image: docker://x\n  entrypoint:\n    alt: x\n",
	} {
		if _, err := ParseAction([]byte(src)); err == nil || !strings.Contains(err.Error(), "entries must be strings") && !strings.Contains(err.Error(), "expected a string") {
			t.Fatalf("src = %q err = %v", src, err)
		}
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("x"), &node); err != nil {
		t.Fatal(err)
	}
	var s scalar
	if err := s.UnmarshalYAML(&yaml.Node{Kind: yaml.SequenceNode}); err == nil {
		t.Fatal("a sequence is not a scalar")
	}
	var l scalarOrList
	if err := l.UnmarshalYAML(&yaml.Node{Kind: yaml.MappingNode}); err == nil {
		t.Fatal("a mapping is not a scalar-or-list")
	}
	if err := l.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}); err != nil || l != nil {
		t.Fatalf("null = %v %v", l, err)
	}
}

func TestCleanActionPathEdges(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		allowEmpty bool
		ok         bool
	}{
		{"", true, true},
		{"", false, false},
		{"./", true, true},
		{"./", false, false},
		{"./a/./b", false, true},
		{`a\b`, false, false},
		{"/abs", false, false},
		{"a\x00b", false, false},
		{"a/../b", false, false},
		{"..", false, false},
	} {
		if _, err := cleanActionPath(tc.raw, tc.allowEmpty); (err == nil) != tc.ok {
			t.Fatalf("%q allowEmpty=%v: err = %v", tc.raw, tc.allowEmpty, err)
		}
	}
	if got, err := cleanActionPath("./a//b/", false); err != nil || got != "a/b" {
		t.Fatalf("clean = %q %v", got, err)
	}
}

func TestNameValidators(t *testing.T) {
	if validInputName(strings.Repeat("a", 101)) || validInputName("") || validInputName("a b") {
		t.Fatal("invalid input names must be refused")
	}
	if !validInputName("a.b-c_D9") {
		t.Fatal("a legal input name was refused")
	}
	if validStepID(strings.Repeat("a", 101)) || validStepID("a b") || !validStepID("a-b_C9") {
		t.Fatal("step id validation")
	}
	if !isDockerfileName("Dockerfile") || !isDockerfileName("docker/Dockerfile.dev") || isDockerfileName("Containerfile") {
		t.Fatal("dockerfile detection")
	}
}

func TestSetupRestoreHintFallback(t *testing.T) {
	if got := restoreHint("plain-key"); got != "plain-key" {
		t.Fatalf("fallback = %q", got)
	}
	if envText(map[string]string{"B": "2", "A": "1"}) != "A: 1, B: 2" {
		t.Fatal("envText must sort")
	}
}

func TestParseFetchedPrefersYML(t *testing.T) {
	a, err := parseFetched([]File{
		{Path: "action.yaml", Data: []byte("runs:\n  using: node20\n  main: y.js\n")},
		{Path: "action.yml", Data: []byte("runs:\n  using: node20\n  main: m.js\n")},
	})
	if err != nil || a.Main != "m.js" {
		t.Fatalf("prefer yml = %+v %v", a, err)
	}
	if _, err := parseFetched([]File{{Path: "action.yaml", Data: []byte("runs:\n  using: node20\n  main: y.js\n")}}); err != nil {
		t.Fatalf("yaml fallback = %v", err)
	}
	if _, err := parseFetched([]File{{Path: "x"}}); err == nil {
		t.Fatal("no metadata must be refused")
	}
}

func TestSourceHelpers(t *testing.T) {
	var nilSrc *HTTPSource
	if nilSrc.Host() != "" {
		t.Fatal("nil source host")
	}
	if m, ok := actionMode(0o100644); !ok || m != 0o644 {
		t.Fatal("actionMode 644")
	}
	if m, ok := actionMode(0o100755); !ok || m != 0o755 {
		t.Fatal("actionMode 755")
	}
	if _, ok := actionMode(0o100600); ok {
		t.Fatal("actionMode 600 must be refused")
	}
	if shortSHA("abcdefghijklmnop") != "abcdefghijkl" {
		t.Fatal("shortSHA")
	}
	if humanBytes(1024) != "1.0 KiB" || humanBytes(10) != "10 B" {
		t.Fatal("humanBytes")
	}
	if refSHA([]byte("not json")) != "" {
		t.Fatal("refSHA must tolerate junk")
	}
	if refSHA([]byte(`{"object":{"sha":"`+testCommit+`"}}`)) != testCommit {
		t.Fatal("refSHA object shape")
	}
	if refSHA([]byte(`[{"object":{"sha":"`+testCommit+`"}}]`)) != testCommit {
		t.Fatal("refSHA array shape")
	}
}

func TestHTTPSourceHTTPErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/refs/") {
			if strings.HasSuffix(r.URL.Path, "/tags/v1") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: srv.Client()}
	ref, _ := ParseRef("acme/tools@v1")
	if _, err := src.Resolve(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("500 = %v", err)
	}
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("403 = %v", err)
	}
	// A non-gzip body is a named error.
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("plain")) }))
	defer srv2.Close()
	src2 := &HTTPSource{Base: mustURL(t, srv2.URL), Client: srv2.Client()}
	if _, err := src2.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "not a gzip archive") {
		t.Fatalf("gzip = %v", err)
	}
	// A source with no client builds the redirect-refusing default.
	if _, err := (&HTTPSource{Base: mustURL(t, srv.URL)}).Resolve(context.Background(), ref); err == nil {
		t.Fatal("the default client must still see the 500")
	}
}

func TestHTTPArchiveMultipleRoots(t *testing.T) {
	archive := makeTarRaw(t, []tarEntry{
		{name: "a/action.yml", body: "x"},
		{name: "b/y", body: "y"},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: srv.Client()}
	ref, _ := ParseRef("acme/tools@" + testCommit)
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "more than one root") {
		t.Fatalf("multi-root = %v", err)
	}
}

func TestLocalSourceRefusedModesAndSubpathFilter(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", "v1", testCommit,
		File{Path: "sub/action.yml", Mode: 0o644, Data: []byte("runs:\n  using: node20\n  main: m.js\n")},
		File{Path: "sub/m.js", Mode: 0o644, Data: []byte("1")},
		File{Path: "sub/run", Mode: 0o755, Data: []byte("#!/bin/sh\n")})
	src := &LocalSource{Git: g}
	ref, _ := ParseRef("acme/tools/sub@v1")
	files, err := src.Fetch(context.Background(), ref, testCommit)
	if err != nil || len(files) != 3 {
		t.Fatalf("subpath fetch = %+v %v", files, err)
	}
	if files[2].Mode != 0o755 {
		t.Fatalf("exec bit lost: %+v", files[2])
	}
	// A tree mode outside the two action modes is a named refusal.
	g.trees["acme/tools@"+testCommit] = append(g.trees["acme/tools@"+testCommit], TreeEntry{Path: "sub/sock", Mode: 0o100660})
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "unsupported mode") {
		t.Fatalf("mode refusal = %v", err)
	}
	// A read failure is surfaced, never an empty file.
	g.trees["acme/tools@"+testCommit] = g.trees["acme/tools@"+testCommit][:3]
	g.files = map[string][]byte{}
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil {
		t.Fatal("a missing blob must fail")
	}
}

func TestResolverDirNameAndGuardEdges(t *testing.T) {
	a := &Action{Commit: testCommit, Digest: "sha256:x"}
	if dirName(a.Commit, a.Digest) == "" || dirName(a.Commit, a.Digest) != dirName(a.Commit, a.Digest) {
		t.Fatal("dirName must be stable")
	}
	g := NewDigestGuard(1)
	if err := g.Check("s", "c", "d"); err != nil {
		t.Fatal(err)
	}
	if err := g.Check("s", "c", "d"); err != nil {
		t.Fatal(err)
	}
	if err := g.Check("s", "c", "e"); err == nil {
		t.Fatal("changed digest must be refused")
	}
}

func TestResolverDownloadFailurePropagates(t *testing.T) {
	// A resolver with a source that resolves but cannot fetch surfaces the
	// fetch error unchanged (no partial action is returned).
	r := NewResolver(DefaultPolicy(), &LocalSource{Git: brokenGit{}}, nil, nil, DefaultLimits())
	if _, err := r.Resolve(context.Background(), "acme/tools@"+testCommit); err == nil {
		t.Fatal("fetch failure must surface")
	}
}

type brokenGit struct{}

func (brokenGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	return testCommit, nil
}
func (brokenGit) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	return nil, errors.New("boom")
}
func (brokenGit) ListTree(ctx context.Context, repo, commit, dir string) ([]TreeEntry, error) {
	return []TreeEntry{{Path: "action.yml", Mode: 0o644}}, nil
}

func TestHumanBytesUnit(t *testing.T) {
	for _, n := range []int64{0, 1, 2048, 5 << 20, 3 << 30} {
		if humanBytes(n) == "" {
			t.Fatal("humanBytes")
		}
	}
	_ = fmt.Sprint()
}
