package workflow

import (
	"strings"
	"testing"
)

// TestFilterDialect pins the documented cheat sheet (PAR-013): `*` stops at a
// slash, `**` crosses it, `?`/`+` quantify the preceding character, and an
// ordered `!` negates.
func TestFilterDialect(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*", "a/b", false},
		{"**", "a/b", true},
		{"src/**", "src/a/b.go", true},
		{"src/*", "src/a/b.go", false},
		{"release/*", "release/1.2", true},
		{"release/*", "releases/1.2", false},
		{"v?", "v", true},
		{"v?", "v1", false},
		{"v+", "vvvv", true},
		{"v+", "v", true}, // + is one or more of the preceding character
		{"a?b", "ab", true},
		{"a?b", "axb", false},
		{"**/*.md", "docs/a.md", true},
	}
	for _, tc := range cases {
		f, err := compileFilter([]string{tc.pattern}, false)
		if err != nil {
			t.Fatalf("%q: %v", tc.pattern, err)
		}
		if got := f.Match(tc.name); got != tc.want {
			t.Errorf("%q ~ %q = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestFilterOrderedNegation pins the order rule: the LAST matching pattern
// decides, so a later `!` excludes and a later positive includes again.
func TestFilterOrderedNegation(t *testing.T) {
	f, err := compileFilter([]string{"main", "release/*", "!release/experimental", "release/experimental/*"}, false)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"main":                   true,
		"release/1":              true,
		"release/experimental":   false,
		"release/experimental/x": true,
		"feature/nope":           false,
	}
	for name, want := range cases {
		if got := f.Match(name); got != want {
			t.Errorf("%q = %v, want %v", name, got, want)
		}
	}
	// An ignore list is the inverse: a match excludes, no match includes.
	ignore, err := compileFilter([]string{"docs/**", "*.md"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ignore.Match("docs/a.md") || ignore.Match("readme.md") || !ignore.Match("src/a.go") {
		t.Fatalf("ignore matching is inverted")
	}
}

// TestFilterParseErrors pins fail-closed compilation: a malformed pattern is
// that file's parse error, never a silently dead filter.
func TestFilterParseErrors(t *testing.T) {
	for _, bad := range []string{"", "!", "?x", "+x", "*?"} {
		if _, err := compileFilter([]string{bad}, false); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := compileFilter([]string{"!x"}, true); err == nil {
		t.Error("an ignore list accepted a negated pattern")
	}
}

// TestPushPathsFilter pins the parser surface for paths/paths-ignore: both
// lists parse, and naming both is refused.
func TestPushPathsFilter(t *testing.T) {
	src := `name: p
on:
  push:
    branches: [main]
    paths: ["src/**", "!src/vendor/**"]
    paths-ignore: [docs/**]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "paths") {
		t.Fatalf("paths + paths-ignore accepted: %v", err)
	}
	src = strings.Replace(src, "    paths-ignore: [docs/**]\n", "", 1)
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.On.Push.Paths) != 2 {
		t.Fatalf("paths = %v", w.On.Push.Paths)
	}
	if !w.FiresWithChanges("push", RefBranch, "main", "", []string{"src/a.go"}, true) {
		t.Fatal("a src change did not fire")
	}
	if w.FiresWithChanges("push", RefBranch, "main", "", []string{"docs/a.md"}, true) {
		t.Fatal("a docs-only change fired a paths: [src/**] trigger")
	}
	if w.Fires("push", RefBranch, "main", "") {
		t.Fatal("a path-filtered trigger fired with no diff (must fail closed)")
	}
}
