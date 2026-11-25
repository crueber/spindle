// languages_test.go — 04_git.md §16.5: the bounded `ls-tree -r -l -z` language
// tally. The integration test pins regular blobs only (symlinks and gitlinks
// are skipped), byte sizes as committed, the pinned extension table and the
// entry cap; the unit tests pin the parser's chunk boundaries and determinism.
package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixtureFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInsightsLanguagesRealGit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	writeFixtureFile(t, dir, "a.go", strings.Repeat("a", 100))
	writeFixtureFile(t, dir, "pkg/b.go", strings.Repeat("b", 20))
	writeFixtureFile(t, dir, "app.js", strings.Repeat("c", 50))
	writeFixtureFile(t, dir, "style.css", strings.Repeat("d", 25))
	writeFixtureFile(t, dir, "notes.unknownext", strings.Repeat("e", 10))
	writeFixtureFile(t, dir, "empty.go", "")
	writeFixtureFile(t, dir, "bin.dat", "\x00\x01")
	writeFixtureFile(t, dir, "Dockerfile", "FROM x\n")
	writeFixtureFile(t, dir, "Makefile", "all:\n")
	if err := os.Symlink("a.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	activityGit(t, dir, nil, "add", "-A")
	activityGit(t, dir, nil, "update-index", "--add", "--cacheinfo",
		"160000,"+strings.Repeat("f", 40)+",sub")
	activityGit(t, dir, nil, "commit", "-qm", "language fixture")
	head := activityGit(t, dir, nil, "rev-parse", "HEAD")

	l := NewLayer()
	got, capped, err := l.InsightsLanguages(ctx, &LocalRepo{Path: dir}, head, 0)
	if err != nil {
		t.Fatalf("InsightsLanguages: %v", err)
	}
	if capped {
		t.Fatal("fixture is under the cap, got capped=true")
	}
	byName := map[string]InsightsLanguage{}
	for _, g := range got {
		byName[g.Name] = g
	}
	// Go: a.go 100 + pkg/b.go 20 + empty.go 0. The symlink to a.go is
	// 120000 and must NOT add its target's bytes.
	if g := byName["Go"]; g.Bytes != 120 || g.Files != 3 {
		t.Errorf("Go = %+v, want 120 bytes / 3 files", g)
	}
	if g := byName["JavaScript"]; g.Bytes != 50 || g.Files != 1 {
		t.Errorf("JavaScript = %+v, want 50 bytes / 1 file", g)
	}
	if g := byName["CSS"]; g.Bytes != 25 || g.Files != 1 {
		t.Errorf("CSS = %+v", g)
	}
	// 10 unknown + 2 binary = 12; the gitlink contributes nothing.
	if g := byName["Other"]; g.Bytes != 12 || g.Files != 2 {
		t.Errorf("Other = %+v, want 12 bytes / 2 files", g)
	}
	if g := byName["Dockerfile"]; g.Bytes != 7 {
		t.Errorf("Dockerfile = %+v", g)
	}
	if g := byName["Makefile"]; g.Bytes != 5 {
		t.Errorf("Makefile = %+v", g)
	}
	// Deterministic order: bytes desc, name asc.
	for i := 1; i < len(got); i++ {
		if got[i-1].Bytes < got[i].Bytes ||
			(got[i-1].Bytes == got[i].Bytes && got[i-1].Name > got[i].Name) {
			t.Fatalf("order not bytes desc / name asc: %+v", got)
		}
	}

	// The cap counts regular blobs in listing order and reports truncation.
	cappedOut, cappedFlag, err := l.InsightsLanguages(ctx, &LocalRepo{Path: dir}, head, 2)
	if err != nil {
		t.Fatalf("capped scan: %v", err)
	}
	if !cappedFlag {
		t.Fatal("cap=2 over 8 regular blobs must report capped")
	}
	var files int
	for _, g := range cappedOut {
		files += g.Files
	}
	if files != 2 {
		t.Fatalf("capped scan counted %d files, want 2: %+v", files, cappedOut)
	}
}

func TestInsightsLanguagesRejectsInputs(t *testing.T) {
	ctx := context.Background()
	l := NewLayer()
	repo := &LocalRepo{Path: t.TempDir()}
	sha := strings.Repeat("a", 40)
	if _, _, err := l.InsightsLanguages(ctx, nil, sha, 10); err == nil {
		t.Fatal("nil repo must fail")
	}
	if _, _, err := l.InsightsLanguages(ctx, repo, "HEAD", 10); err == nil {
		t.Fatal("non-hex revision must fail")
	}
	if _, _, err := l.InsightsLanguages(ctx, repo, strings.Repeat("0", 40), 10); err == nil {
		t.Fatal("zero oid must fail")
	}
	if _, _, err := l.InsightsLanguages(ctx, repo, sha, 10); err == nil {
		t.Fatal("missing object must fail")
	}
}

// TestLanguageTallyChunkBoundaries feeds records split across Write calls:
// the parser must assemble them exactly, and records past the cap must be
// dropped without panic.
func TestLanguageTallyChunkBoundaries(t *testing.T) {
	tally := &languageTally{cap: 2, bytes: map[string]int64{}}
	stream := "100644 blob aaa 100\ta.go\x00100755 blob bbb 50\tb.js\x00120644 blob ddd 0\tlink.go\x00160644 commit eee -\tsub\x00100644 blob fff 25\tc.css\x00"
	for i := 0; i < len(stream); i++ {
		if _, err := tally.Write([]byte{stream[i]}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := tally.sorted()
	if len(got) != 2 || got[0].Name != "Go" || got[1].Name != "JavaScript" {
		t.Fatalf("tally = %+v", got)
	}
	if !tally.capped {
		t.Fatal("third regular blob past the cap must set capped")
	}
}

func TestLanguageFor(t *testing.T) {
	cases := map[string]string{
		"a.go":             "Go",
		"dir/x.JS":         "JavaScript",
		"x.tsx":            "TypeScript",
		"a.scss":           "CSS",
		"index.html":       "HTML",
		"run.sh":           "Shell",
		"x.py":             "Python",
		"x.rs":             "Rust",
		"X.java":           "Java",
		"x.h":              "C",
		"x.hpp":            "C++",
		"x.rb":             "Ruby",
		"x.php":            "PHP",
		"x.json":           "JSON",
		"x.yaml":           "YAML",
		"README.md":        "Markdown",
		"x.sql":            "SQL",
		"dockerfile":       "Dockerfile",
		"GNUmakefile":      "Makefile",
		"LICENSE":          "Other",
		".gitignore":       "Other",
		"noext":            "Other",
		"weird.unknownext": "Other",
	}
	for in, want := range cases {
		if got := languageFor(in); got != want {
			t.Errorf("languageFor(%q) = %q, want %q", in, got, want)
		}
	}
}
