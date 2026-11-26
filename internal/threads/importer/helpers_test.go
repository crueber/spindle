package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

func mapNode(kv ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[i]},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[i+1]})
	}
	return n
}

func TestMapIndex(t *testing.T) {
	if got := mapIndex(nil, "a"); got != -1 {
		t.Errorf("nil = %d", got)
	}
	if got := mapIndex(&yaml.Node{Kind: yaml.SequenceNode}, "a"); got != -1 {
		t.Errorf("sequence = %d", got)
	}
	m := mapNode("a", "1", "b", "2")
	if got := mapIndex(m, "b"); got != 2 {
		t.Errorf("b = %d", got)
	}
	if got := mapIndex(m, "c"); got != -1 {
		t.Errorf("c = %d", got)
	}
}

func TestDropHelpers(t *testing.T) {
	tr := &translator{}
	m := mapNode("a", "1", "b", "2")
	tr.drop(m, 0, "# note")
	if len(m.Content) != 2 || m.Content[0].Value != "b" || !strings.Contains(m.Content[0].HeadComment, "# note") {
		t.Errorf("drop to next key = %+v", m.Content)
	}
	m = mapNode("a", "1")
	tr.drop(m, 0, "# last")
	if len(m.Content) != 0 || !strings.Contains(m.HeadComment, "# last") {
		t.Errorf("drop of last key = %+v %q", m.Content, m.HeadComment)
	}
	tr.drop(m, 5, "# ignored") // out of range: no panic, no change
	m = mapNode("a", "1")
	m.Content[1].HeadComment = "# inner"
	tr.drop(m, 0, "# outer")
	if !strings.Contains(m.HeadComment, "# inner") || !strings.Contains(m.HeadComment, "# outer") {
		t.Errorf("inner comment was not lifted: %q", m.HeadComment)
	}

	seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "a"}, {Kind: yaml.ScalarNode, Value: "b"},
	}}
	tr.dropItem(seq, 0, "# first")
	if len(seq.Content) != 1 || seq.Style == yaml.FlowStyle || !strings.Contains(seq.Content[0].HeadComment, "# first") {
		t.Errorf("dropItem = %+v style=%d", seq.Content, seq.Style)
	}
	tr.dropItem(seq, 0, "")
	tr.dropItem(seq, 9, "# out of range") // no panic
	tr.dropItem(seq, 0, "# gone")         // empty sequence: comment on the sequence
	if len(seq.Content) != 0 {
		t.Errorf("sequence not emptied: %+v", seq.Content)
	}

	tr.commentAt(m, 99, "# mapping")
	if !strings.Contains(m.HeadComment, "# mapping") {
		t.Errorf("commentAt out of range: %q", m.HeadComment)
	}
	tr.commentAt(m, 0, "")
}

func TestCopyAndResolveAliases(t *testing.T) {
	if copyNode(nil) != nil {
		t.Error("copyNode(nil) should be nil")
	}
	target := mapNode("branches", "main")
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "x"}, target,
		{Kind: yaml.ScalarNode, Value: "y"}, {Kind: yaml.AliasNode, Alias: target},
	}}
	budget := 0
	if err := resolveAliases(root, &budget); err != nil {
		t.Fatal(err)
	}
	if root.Content[3].Kind != yaml.MappingNode {
		t.Errorf("alias was not resolved: %+v", root.Content[3])
	}
	// An alias with no target is refused.
	bad := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "x"}, {Kind: yaml.AliasNode},
	}}
	budget = 0
	if err := resolveAliases(bad, &budget); err == nil {
		t.Error("an alias with no target should fail")
	}
	// The expansion cap holds.
	big := &yaml.Node{Kind: yaml.SequenceNode}
	src := mapNode("a", "1")
	for i := 0; i < maxAliasExpansions+1; i++ {
		big.Content = append(big.Content, &yaml.Node{Kind: yaml.AliasNode, Alias: src})
	}
	budget = 0
	if err := resolveAliases(big, &budget); err == nil {
		t.Error("the alias expansion cap did not trigger")
	}
}

func TestRewriteDirect(t *testing.T) {
	tr := &translator{}
	tr.rewrite(nil, "", false) // nil is a no-op
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapNode("a", "$GITHUB_SERVER_URL")}}
	tr.rewrite(doc, "", false)
	if got := doc.Content[0].Content[1].Value; got != "$SPINDLE_SERVER" {
		t.Errorf("document-node walk = %q", got)
	}
	null := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
	tr.rewrite(null, "x", false)
}

func TestIsNullAndScalarText(t *testing.T) {
	if !isNull(nil) || !isNull(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}) {
		t.Error("isNull missed a null")
	}
	if isNull(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "x"}) {
		t.Error("isNull matched a string")
	}
	if _, ok := scalarText(&yaml.Node{Kind: yaml.MappingNode}); ok {
		t.Error("scalarText accepted a mapping")
	}
	if got, ok := scalarText(&yaml.Node{Kind: yaml.ScalarNode, Value: "v"}); !ok || got != "v" {
		t.Error("scalarText rejected a scalar")
	}
}

func TestLiftEmptyCommentsSequence(t *testing.T) {
	doc := mapNode("a", "")
	doc.Content[1].Kind = yaml.SequenceNode
	doc.Content[1].Tag = "!!seq"
	doc.Content[1].HeadComment = "# empty list"
	liftEmptyComments(doc)
	if !strings.Contains(doc.Content[0].HeadComment, "# empty list") {
		t.Errorf("comment not lifted: %q", doc.Content[0].HeadComment)
	}
	liftEmptyComments(nil)
}

func TestFailureScriptBounds(t *testing.T) {
	got := failureScript(strings.Repeat("x", 500))
	if len(got) > 400 {
		t.Errorf("failure script is unbounded: %d bytes", len(got))
	}
}

func TestReadCappedMissing(t *testing.T) {
	if _, err := readCapped(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing file should fail")
	}
}

func TestWriteOutputMkdirFailure(t *testing.T) {
	base := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(base, ".spindle/workflows/ci.yml", []byte("x")); err == nil {
		t.Error("writing below a file should fail")
	}
}

func TestInspectRepoBadToplevel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho /nonexistent-spindle-toplevel\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = bin
	t.Cleanup(func() { gitBin = old })
	if _, err := inspectRepo(context.Background(), dir); err == nil {
		t.Error("a toplevel that does not resolve should fail")
	}
}

func TestWriteOutputThroughUnresolvableBase(t *testing.T) {
	// base does not exist: safeRel falls back to a lexical check, MkdirAll
	// creates it, and the write succeeds.
	base := filepath.Join(t.TempDir(), "out")
	if err := writeOutput(base, ".spindle/workflows/ci.yml", []byte("on: push\n")); err != nil {
		t.Fatalf("writeOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, ".spindle", "workflows", "ci.yml")); err != nil {
		t.Fatal(err)
	}
}

func TestOutputPathSeparators(t *testing.T) {
	if _, err := OutputPath(`a\b.yml`); err == nil {
		t.Error("a backslash in the name should be refused")
	}
}

func TestResolveAliasesNilChild(t *testing.T) {
	n := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "x"}, nil,
	}}
	budget := 0
	if err := resolveAliases(n, &budget); err != nil {
		t.Fatalf("a nil child should be skipped: %v", err)
	}
}

func TestDiscoverMissingExplicitPath(t *testing.T) {
	root := t.TempDir()
	if _, err := Discover(root, Forgejo, "no/such/file.yml"); err == nil {
		t.Fatal("a missing explicit path should fail")
	}
}

func TestRunOutIsAFile(t *testing.T) {
	root := importRepo(t, sampleWorkflow)
	out := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(out, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: root, Platform: Forgejo, Out: out}); err == nil {
		t.Fatal("--out naming a file must fail")
	}
}

func TestInspectRepoStatusFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *--show-toplevel*) pwd ;;\n" +
		"  *) echo boom >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = bin
	t.Cleanup(func() { gitBin = old })
	if _, err := inspectRepo(context.Background(), dir); err == nil {
		t.Fatal("a failing git status must fail the inspection")
	}
}

func TestInspectRepoHeadFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *--show-toplevel*) pwd ;;\n" +
		"  *status*) exit 0 ;;\n" +
		"  *) echo boom >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = bin
	t.Cleanup(func() { gitBin = old })
	if _, err := inspectRepo(context.Background(), dir); err == nil {
		t.Fatal("a failing rev-parse HEAD must fail the inspection")
	}
}

func TestReusableJobWithBadSteps(t *testing.T) {
	src := "on: push\njobs:\n  call:\n    uses: ./other.yml\n    steps: nope\n"
	res, err := Translate(Forgejo, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("does not parse: %v\n%s", err, res.Output)
	}
	if len(res.Untranslated) != 1 {
		t.Errorf("untranslated = %+v", res.Untranslated)
	}
}
