package workflow

// The job container mapping's scalar spellings and strictness (PAR-077), at the
// parser boundary: scalar-or-list entrypoint/command/ports, quote-aware scalar
// splitting, and the credential mapping that the server refuses by name.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// node parses one YAML fragment into its node.
func node(t *testing.T, src string) yaml.Node {
	t.Helper()
	var v yaml.Node
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if len(v.Content) == 0 {
		t.Fatal("empty document")
	}
	return *v.Content[0]
}

// TestDecodeCommandWordsTable pins the scalar/list rule: a scalar is one command
// line (split with quote awareness), a list element is one argument.
func TestDecodeCommandWordsTable(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{`-c "sleep infinity"`, "-c|sleep infinity"},
		{`/bin/sh`, "/bin/sh"},
		{`  spaced   out  `, "spaced|out"},
		{`"--flag=1"`, "--flag=1"},
		{`""`, ""},
		{`- /bin/sh
- -c "sleep infinity"`, `/bin/sh|-c "sleep infinity"`},
		{`["/bin/sh", "-e"]`, "/bin/sh|-e"},
	} {
		var out []string
		if err := DecodeCommandWords(ptr(node(t, tc.src)), &out); err != nil {
			t.Fatalf("%q: %v", tc.src, err)
		}
		if got := strings.Join(out, "|"); got != tc.want {
			t.Fatalf("%q = %q, want %q", tc.src, got, tc.want)
		}
	}
	// A null scalar decodes to nothing (the key was written empty).
	var out []string
	if err := DecodeCommandWords(ptr(node(t, "null")), &out); err != nil || out != nil {
		t.Fatalf("null: %v %v", out, err)
	}
	// A mapping is neither a scalar nor a list.
	if err := DecodeCommandWords(ptr(node(t, "{a: b}")), &out); err == nil {
		t.Fatal("a mapping must be refused")
	}
	// A list of non-scalars is refused.
	if err := DecodeCommandWords(ptr(node(t, "[[1]]")), &out); err == nil || !strings.Contains(err.Error(), "strings") {
		t.Fatalf("nested list: %v", err)
	}
}

// TestSplitCommandWordsTable pins the splitter's own rules (a scalar's inner
// quotes group a word).
func TestSplitCommandWordsTable(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{`'a b' c`, "a b|c"},
		{`"a b" 'c d'`, "a b|c d"},
		{`a\ b`, `a\|b`},
		{`'unterminated`, "unterminated"},
		{"a\tb\nc", "a|b|c"},
	} {
		if got := strings.Join(splitCommandWords(tc.in), "|"); got != tc.want {
			t.Fatalf("%q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func ptr(n yaml.Node) *yaml.Node { return &n }

// TestContainerMappingScalarAndList parses a whole container mapping and checks
// the three scalar spellings plus the credential mapping.
func TestContainerMappingScalarAndList(t *testing.T) {
	src := `
image: alpine:3.20
ports: 8080
command: -c "sleep infinity"
entrypoint: /bin/sh
credentials:
  username: u
  password: p
`
	var c Container
	if err := c.UnmarshalYAML(ptr(node(t, src))); err != nil {
		t.Fatalf("UnmarshalYAML: %v", err)
	}
	if strings.Join(c.Command, "|") != "-c|sleep infinity" || strings.Join(c.Entrypoint, "|") != "/bin/sh" {
		t.Fatalf("command/entrypoint = %v / %v", c.Command, c.Entrypoint)
	}
	if strings.Join(c.Ports, "|") != "8080" {
		t.Fatalf("ports = %v", c.Ports)
	}
	if c.Credentials == nil || c.Credentials.Username != "u" || c.Credentials.Password != "p" {
		t.Fatalf("credentials = %+v", c.Credentials)
	}
	// The scalar image shorthand still works and carries nothing else.
	var shorthand Container
	if err := shorthand.UnmarshalYAML(ptr(node(t, "golang:1.27"))); err != nil || shorthand.Image != "golang:1.27" || shorthand.Command != nil {
		t.Fatalf("shorthand = %+v (%v)", shorthand, err)
	}
}

// TestContainerMappingRefusals: an unknown key, a malformed credential mapping
// and a non-mapping container are named parse errors.
func TestContainerMappingRefusals(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{"image: x\nrestart: always", `unknown key "restart"`},
		{"image: x\ncredentials: {username: u, token: t}", `unknown key "token"`},
		{"image: x\ncredentials: \"u:p\"", "expected a mapping"},
		{"[1, 2]", "expected an image or a mapping"},
		{"image: x\nports: [{a: b}]", "container.ports"},
		{"image: x\ncommand: {a: b}", "container.command"},
	} {
		var c Container
		err := c.UnmarshalYAML(ptr(node(t, tc.src)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: err = %v, want %q", tc.src, err, tc.want)
		}
	}
}

// TestContainerStructuralSpansRefused: entrypoint/command are applied literals
// (D-16), so a `${{ }}` span there — on the job container or a service — is a
// parse error naming the field, never an argument holding expression text.
func TestContainerStructuralSpansRefused(t *testing.T) {
	doc := func(container, service string) []byte {
		return []byte(`name: x
on: push
jobs:
  b:
    runs-on: linux
` + container + `
    services:
      pg:
        image: postgres:17
` + service + `
    steps:
      - run: echo hi
`)
	}
	for _, tc := range []struct {
		name      string
		container string
		service   string
		want      string
	}{
		{"container command", "    container:\n      image: alpine\n      command: [\"-c\", \"${{ secrets.X }}\"]", "", "container.command[1]"},
		{"container entrypoint", "    container:\n      image: alpine\n      entrypoint: [\"${{ vars.X }}\"]", "", "container.entrypoint[0]"},
		{"service command", "", "        command: [\"${{ secrets.X }}\"]", "services.pg.command[0]"},
		{"service cmd", "", "        cmd: [\"-c\", \"${{ vars.X }}\"]", "services.pg.cmd[1]"},
		{"service entrypoint", "", "        entrypoint: [\"${{ vars.X }}\"]", "services.pg.entrypoint[0]"},
	} {
		if _, err := Parse(doc(tc.container, tc.service)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// Literal braces stay fine.
	if _, err := Parse(doc("    container:\n      image: alpine\n      command: [\"-c\", \"echo {ok}\"]", "")); err != nil {
		t.Fatalf("a literal brace must parse: %v", err)
	}
}
