package actions

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGit is a minimal GitSource over in-memory maps.
type fakeGit struct {
	refs  map[string]string
	trees map[string][]TreeEntry
	files map[string][]byte
	calls int
}

func newFakeGit() *fakeGit {
	return &fakeGit{refs: map[string]string{}, trees: map[string][]TreeEntry{}, files: map[string][]byte{}}
}

func (f *fakeGit) add(repo, ref, commit string, files ...File) {
	f.refs[repo+"@"+ref] = commit
	entries := make([]TreeEntry, 0, len(files))
	for _, file := range files {
		key := repo + "@" + commit + ":" + file.Path
		f.files[key] = file.Data
		entries = append(entries, TreeEntry{Path: file.Path, Mode: file.Mode, Size: int64(len(file.Data))})
	}
	f.trees[repo+"@"+commit] = entries
}

func (f *fakeGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	f.calls++
	if sha, ok := f.refs[repo+"@"+ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("%w: unknown ref", ErrNotFound)
}

func (f *fakeGit) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	if data, ok := f.files[repo+"@"+commit+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
}

func (f *fakeGit) ListTree(ctx context.Context, repo, commit, dir string) ([]TreeEntry, error) {
	entries, ok := f.trees[repo+"@"+commit]
	if !ok {
		return nil, fmt.Errorf("%w: no tree", ErrNotFound)
	}
	out := make([]TreeEntry, 0, len(entries))
	for _, e := range entries {
		if dir == "" || strings.HasPrefix(e.Path, dir+"/") {
			out = append(out, e)
		}
	}
	return out, nil
}

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func fakeActionFiles() []File {
	return []File{
		{Path: "action.yml", Mode: 0o644, Data: []byte("name: hello\nruns:\n  using: node20\n  main: dist/index.js\n")},
		{Path: "dist/index.js", Mode: 0o644, Data: []byte("console.log('hi')\n")},
	}
}

func TestLocalSourceResolveAndFetch(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", "v1", testCommit, fakeActionFiles()...)
	src := &LocalSource{Git: g}
	ref, _ := ParseRef("acme/tools@v1")
	commit, err := src.Resolve(context.Background(), ref)
	if err != nil || commit != testCommit {
		t.Fatalf("Resolve = %q %v", commit, err)
	}
	files, err := src.Fetch(context.Background(), ref, commit)
	if err != nil || len(files) != 2 || files[0].Path != "action.yml" {
		t.Fatalf("Fetch = %+v %v", files, err)
	}
}

func TestLocalSourceSubpath(t *testing.T) {
	g := newFakeGit()
	g.add("acme/tools", "v1", testCommit,
		File{Path: "actions/setup/action.yml", Mode: 0o644, Data: []byte("runs:\n  using: node20\n  main: m.js\n")},
		File{Path: "actions/setup/m.js", Mode: 0o644, Data: []byte("1")})
	src := &LocalSource{Git: g}
	ref, _ := ParseRef("acme/tools/actions/setup@v1")
	files, err := src.Fetch(context.Background(), ref, testCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "action.yml" {
		t.Fatalf("files = %+v (subpath must be stripped)", files)
	}
}

func TestLocalSourceRefusals(t *testing.T) {
	base := func() (*fakeGit, *LocalSource, Ref) {
		g := newFakeGit()
		g.add("acme/tools", "v1", testCommit, fakeActionFiles()...)
		ref, _ := ParseRef("acme/tools@v1")
		return g, &LocalSource{Git: g}, ref
	}
	t.Run("unknown ref", func(t *testing.T) {
		_, src, _ := base()
		ref, _ := ParseRef("acme/tools@nope")
		if _, err := src.Resolve(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "not found on this instance") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		g, src, ref := base()
		g.add("acme/tools", "v1", testCommit,
			File{Path: "action.yml", Mode: 0o644},
			File{Path: "link", Mode: 0o120000, Data: []byte("/etc/passwd")})
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("submodule", func(t *testing.T) {
		g, src, ref := base()
		g.add("acme/tools", "v1", testCommit, File{Path: "action.yml", Mode: 0o644}, File{Path: "sub", Mode: 0o160000})
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "submodule") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("per-file cap", func(t *testing.T) {
		g, src, ref := base()
		g.add("acme/tools", "v1", testCommit,
			File{Path: "action.yml", Mode: 0o644, Data: []byte("x")},
			File{Path: "big.js", Mode: 0o644, Data: make([]byte, 2048)})
		src.Limits = Limits{MaxBytes: 4096, MaxFileBytes: 1024, MaxFiles: 10}
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "per-file limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("total cap", func(t *testing.T) {
		g, src, ref := base()
		g.add("acme/tools", "v1", testCommit,
			File{Path: "action.yml", Mode: 0o644, Data: []byte("xxxx")},
			File{Path: "x.js", Mode: 0o644, Data: []byte("yyyy")})
		src.Limits = Limits{MaxBytes: 6, MaxFileBytes: 100, MaxFiles: 10}
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "total limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("file count", func(t *testing.T) {
		g, src, ref := base()
		var files []File
		for i := 0; i < 5; i++ {
			files = append(files, File{Path: fmt.Sprintf("f%d", i), Mode: 0o644})
		}
		g.add("acme/tools", "v1", testCommit, files...)
		src.Limits = Limits{MaxBytes: 1000, MaxFileBytes: 100, MaxFiles: 2}
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "over the limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty action dir", func(t *testing.T) {
		g := newFakeGit()
		g.refs["acme/tools@v1"] = testCommit
		g.trees["acme/tools@"+testCommit] = nil
		src := &LocalSource{Git: g}
		ref, _ := ParseRef("acme/tools@v1")
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "no files") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no git wired", func(t *testing.T) {
		src := &LocalSource{}
		ref, _ := ParseRef("acme/tools@v1")
		if _, err := src.Resolve(context.Background(), ref); err == nil {
			t.Fatal("must fail closed")
		}
		if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil {
			t.Fatal("must fail closed")
		}
	})
}

func TestDigestDeterministicAndSensitive(t *testing.T) {
	a := fakeActionFiles()
	d1, err := Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	rev := []File{a[1], a[0]}
	d2, _ := Digest(rev)
	if d1 != d2 {
		t.Fatalf("digest depends on input order: %q vs %q", d1, d2)
	}
	mut := append([]File{}, a...)
	mut[1].Data = []byte("console.log('changed')\n")
	d3, _ := Digest(mut)
	if d3 == d1 {
		t.Fatal("a changed byte must change the digest")
	}
	if _, err := Digest(nil); err == nil {
		t.Fatal("empty action must not hash")
	}
}

func TestDigestGuardRefusesChangedContent(t *testing.T) {
	g := NewDigestGuard(2)
	if err := g.Check("acme/tools", testCommit, "sha256:a"); err != nil {
		t.Fatal(err)
	}
	if err := g.Check("acme/tools", testCommit, "sha256:a"); err != nil {
		t.Fatal(err)
	}
	err := g.Check("acme/tools", testCommit, "sha256:b")
	if err == nil || !strings.Contains(err.Error(), "changed content under the same commit") {
		t.Fatalf("err = %v", err)
	}
	// Bounded: the evicted entry is re-learned.
	g.Check("acme/a", "1", "x")
	g.Check("acme/b", "2", "y")
	if err := g.Check("acme/tools", testCommit, "sha256:b"); err != nil {
		t.Fatalf("evicted entry must be re-learned, got %v", err)
	}
}

// --- the HTTP source -------------------------------------------------------

func TestHTTPSourceRefusals(t *testing.T) {
	for _, base := range []string{
		"", "ftp://x/y", "http://example.com", "https://user@example.com", "https://example.com/?q=1",
	} {
		if _, err := NewHTTPSource(base, DefaultLimits()); err == nil {
			t.Fatalf("%q accepted", base)
		}
	}
	if src, err := NewHTTPSource("http://127.0.0.1:9999/base", DefaultLimits()); err != nil || src.Host() != "127.0.0.1:9999" {
		t.Fatalf("loopback http source = %v %v", src, err)
	}
}

func TestHTTPSourceResolveAndFetch(t *testing.T) {
	tarBytes := makeTar(t, "repo-abc/", map[string]string{
		"action.yml": "name: x\nruns:\n  using: node20\n  main: m.js\n",
		"m.js":       "console.log(1)\n",
		"other.txt":  "inside the action directory too",
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/refs/tags/v1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": testCommit}})
		case strings.HasSuffix(r.URL.Path, "/git/refs/heads/main"):
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"object": map[string]string{"sha": testCommit}}})
		case strings.HasSuffix(r.URL.Path, "/archive/"+testCommit+".tar.gz"):
			w.Write(tarBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: srv.Client(), Limits: DefaultLimits()}
	ref, _ := ParseRef("acme/tools@v1")
	commit, err := src.Resolve(context.Background(), ref)
	if err != nil || commit != testCommit {
		t.Fatalf("Resolve = %q %v", commit, err)
	}
	branch, _ := ParseRef("acme/tools@main")
	if c, err := src.Resolve(context.Background(), branch); err != nil || c != testCommit {
		t.Fatalf("array-shaped ref = %q %v", c, err)
	}
	files, err := src.Fetch(context.Background(), ref, commit)
	if err != nil || len(files) != 3 {
		t.Fatalf("Fetch = %+v %v", files, err)
	}
	sub, _ := ParseRef("acme/tools/m@v1")
	if _, err := src.Fetch(context.Background(), sub, commit); err == nil {
		t.Fatal("a subpath with no files must be refused")
	}
	unknown, _ := ParseRef("acme/tools@v9")
	if _, err := src.Resolve(context.Background(), unknown); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown ref = %v", err)
	}
	// A full SHA needs no API call.
	pinned, _ := ParseRef("acme/tools@" + testCommit)
	if c, err := src.Resolve(context.Background(), pinned); err != nil || c != testCommit {
		t.Fatalf("sha ref = %q %v", c, err)
	}
}

func TestHTTPSourceSymlinkAndLimits(t *testing.T) {
	link := makeTarRaw(t, []tarEntry{
		{name: "repo-abc/action.yml", body: "x"},
		{name: "repo-abc/link", link: "/etc/passwd", typeflag: '2'},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(link)
	}))
	defer srv.Close()
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: srv.Client()}
	ref, _ := ParseRef("acme/tools@" + testCommit)
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink err = %v", err)
	}

	big := makeTar(t, "repo-abc/", map[string]string{"action.yml": strings.Repeat("x", 4096)})
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(big) }))
	defer srv2.Close()
	src2 := &HTTPSource{Base: mustURL(t, srv2.URL), Client: srv2.Client(), Limits: Limits{MaxBytes: 1024, MaxFileBytes: 512, MaxFiles: 10}}
	if _, err := src2.Fetch(context.Background(), ref, testCommit); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit err = %v", err)
	}
}

func TestHTTPSourceRefusesRedirect(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	// Use the default client (which refuses redirects) rather than the test
	// server's client.
	src := &HTTPSource{Base: mustURL(t, srv.URL), Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("refused") }}}
	ref, _ := ParseRef("acme/tools@" + testCommit)
	if _, err := src.Fetch(context.Background(), ref, testCommit); err == nil {
		t.Fatal("a redirect must be refused")
	}
}
