// Package importer converts one Forgejo or GitHub Actions workflow file into a
// Spindle workflow (`PAR-002`, docs/features/21_parity_master.md §2.3).
//
// It is a one-way migration aid, not a second runtime. The engine never reads
// `.forgejo/workflows/` or `.github/workflows/` (PAR-001): those directories are
// read here, once, by an operator command, and the converted file is an
// ordinary `.spindle/workflows/<name>.yml`. The importer never executes the
// source, never edits the source directory, and is read-only unless asked to
// write.
//
// It refuses to guess (security decision S14). Every construct it cannot
// express in the target language is (a) reported, (b) marked with a
// `# UNTRANSLATED:` comment at its site, and (c) a non-zero exit — a lossy
// conversion must never look like success. A construct with no exact
// translation but a safe mechanical rewrite (`needs: test` -> `needs: [test]`,
// `runs-on: docker` -> the default runner labels, a built-in action's `@ref`
// dropped) is rewritten and recorded as a note.
//
// The target grammar has exactly one implementation: every generated file is
// passed through workflow.Parse before it is written or printed, and a file the
// parser rejects is an importer error naming the construct, never a warning.
//
// `gopkg.in/yaml.v3` is imported here for the same reason the workflow parser
// imports it (AGENTS.md law 1, amended 2026-11-26): both read a YAML workflow
// document. This package never re-implements the Spindle grammar.
package importer

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// MaxSourceBytes caps one source file. A workflow is a text file; the cap keeps
// a hostile repository from turning an import into a memory problem.
const MaxSourceBytes = 1 << 20

// ErrUnconvertible marks a source file the importer cannot turn into a valid
// Spindle workflow at all (invalid YAML, no jobs, more than one document). It is
// distinct from an untranslated construct: a refusal still writes a file.
var ErrUnconvertible = errors.New("importer: cannot convert")

// Platform is the dialect a source directory is written for.
type Platform string

const (
	Forgejo Platform = "forgejo"
	GitHub  Platform = "github"
)

// ParsePlatform accepts the two documented spellings.
func ParsePlatform(s string) (Platform, error) {
	switch p := Platform(strings.ToLower(strings.TrimSpace(s))); p {
	case Forgejo, GitHub:
		return p, nil
	default:
		return "", fmt.Errorf("unknown platform %q (supported: forgejo, github)", s)
	}
}

// profile is everything that differs between the two dialects. The translator
// is one implementation; only the source directory and the label conventions
// change (docs/WORKFLOWS.md §10.1).
type profile struct {
	// dir is the source workflow directory, repo-relative.
	dir string
	// labels expands a `runs-on` label to the labels a default Threads runner
	// declares. An unmapped label is carried through verbatim.
	labels map[string][]string
	// fallback is the `runs-on` assumed when a job declares none the importer
	// can use (a reusable-workflow job, a refused label shape).
	fallback []string
	// hosted names a hosted label and what the runner does with it; the note is
	// attached when the label is carried through.
	hosted map[string]string
}

var profiles = map[Platform]profile{
	Forgejo: {
		dir:      ".forgejo/workflows",
		labels:   map[string][]string{"docker": {"self-hosted", "linux", "docker"}},
		fallback: []string{"self-hosted", "linux", "docker"},
		hosted: map[string]string{
			"ubuntu-latest": "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
			"ubuntu-24.04":  "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
			"ubuntu-22.04":  "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
		},
	},
	GitHub: {
		dir:      ".github/workflows",
		fallback: []string{"ubuntu-latest"},
		hosted: map[string]string{
			"ubuntu-latest": "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
			"ubuntu-24.04":  "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
			"ubuntu-22.04":  "carried verbatim; a runner maps the hosted Linux label onto self-hosted + linux at enqueue",
			"windows-latest": "hosted Windows runners are a written non-goal (docs/features/21_parity_master.md §8 N6); " +
				"this job will queue with a named reason until an operator declares the label",
			"macos-latest": "hosted macOS runners are a written non-goal (docs/features/21_parity_master.md §8 N6); " +
				"this job will queue with a named reason until an operator declares the label",
		},
	},
}

// Item is one finding: an untranslated construct or a translation note.
type Item struct {
	// Path is the construct's position, e.g. `jobs.test.steps[2].uses`.
	Path string `json:"path"`
	// Construct is the source spelling, e.g. `on.push.paths`.
	Construct string `json:"construct"`
	// Reason says why it was refused or how it was rewritten.
	Reason string `json:"reason"`
}

// Result is one converted file.
type Result struct {
	// Output is the converted Spindle workflow. It always parses.
	Output []byte
	// Translated counts the constructs that were carried across (triggers,
	// jobs, steps, labels, rewrites).
	Translated int
	// Untranslated lists what could not be expressed. Each entry has a
	// matching `# UNTRANSLATED:` comment in Output.
	Untranslated []Item
	// Notes lists exact, mechanical rewrites worth reviewing.
	Notes []Item
}

// Translate converts one source workflow. platform selects the dialect profile;
// the workflow itself is treated as untrusted text.
func Translate(platform Platform, src []byte) (*Result, error) {
	prof, ok := profiles[platform]
	if !ok {
		return nil, fmt.Errorf("%w: unknown platform %q", ErrUnconvertible, platform)
	}
	if len(src) > MaxSourceBytes {
		return nil, fmt.Errorf("%w: the source is %d bytes, over the %d-byte limit", ErrUnconvertible, len(src), MaxSourceBytes)
	}
	doc, err := decode(src)
	if err != nil {
		return nil, err
	}
	t := &translator{platform: platform, prof: prof}
	t.header(doc)
	if err := t.workflow(doc); err != nil {
		return nil, err
	}
	// Normalize the documented expression and environment aliases once, over
	// the finished tree.
	t.rewrite(doc, "", false)
	liftEmptyComments(doc)

	out, err := encode(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding: %v", ErrUnconvertible, err)
	}
	if _, err := workflow.Parse(out); err != nil {
		return nil, fmt.Errorf("%w: the converted workflow does not parse (importer bug): %v", ErrUnconvertible, err)
	}
	return &Result{
		Output:       out,
		Translated:   t.translated,
		Untranslated: t.untranslated,
		Notes:        t.notes,
	}, nil
}

// decode reads exactly one YAML document into a node tree. The node tree (not
// a Go model) is the working representation so comments, key order and anchors
// survive the round trip, which is what lets a refusal be a comment at the
// construct's own site.
func decode(src []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, fmt.Errorf("%w: the file is empty", ErrUnconvertible)
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: YAML: %v", ErrUnconvertible, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%w: more than one YAML document", ErrUnconvertible)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0] == nil {
		return nil, fmt.Errorf("%w: the file has no document", ErrUnconvertible)
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: a workflow must be a YAML mapping at the top level", ErrUnconvertible)
	}
	budget := 0
	if err := resolveAliases(doc, &budget); err != nil {
		return nil, err
	}
	return doc, nil
}

// maxAliasExpansions caps alias materialization. A source is untrusted text; a
// small file with thousands of nested aliases would otherwise blow up the tree.
const maxAliasExpansions = 1000

// resolveAliases replaces every `*alias` with a copy of its anchor target, so
// the translator never has to know about YAML's reference syntax. Resolving is
// the documented option for anchors: a copied value translates exactly like an
// inline one, and the output carries no anchor that could dangle.
func resolveAliases(n *yaml.Node, budget *int) error {
	if n == nil {
		return nil
	}
	for i, child := range n.Content {
		if child == nil {
			continue
		}
		if child.Kind == yaml.AliasNode {
			*budget++
			if *budget > maxAliasExpansions {
				return fmt.Errorf("%w: more than %d YAML alias expansions", ErrUnconvertible, maxAliasExpansions)
			}
			if child.Alias == nil {
				return fmt.Errorf("%w: a YAML alias has no anchor target", ErrUnconvertible)
			}
			n.Content[i] = copyNode(child.Alias)
			continue
		}
		if err := resolveAliases(child, budget); err != nil {
			return err
		}
	}
	return nil
}

// copyNode deep-copies a node tree, dropping the anchor (the copy has no alias
// pointing at it, and an anchor without an alias is noise in the output).
func copyNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Anchor = ""
	c.Alias = nil
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = copyNode(child)
		}
	}
	return &c
}

// encode marshals the tree with the two-space indentation the rest of the tree
// uses. The output is deterministic: the tree is ordered by its YAML nodes, not
// by a Go map.
func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// translator carries the per-file state: the platform profile, the report
// slices, and the workflow_dispatch input names (a concurrency group may
// reference them, so they must be translated before the group is validated).
type translator struct {
	platform Platform
	prof     profile

	translated   int
	untranslated []Item
	notes        []Item
	inputNames   []string
}

// --- reporting -------------------------------------------------------------

// untranslated records a refusal and returns the comment line that marks its
// site. Callers attach the line with drop/commentAt so the report and the
// generated file can never disagree.
func (t *translator) untranslatedAt(path, construct, reason string) string {
	reason = oneLine(reason)
	t.untranslated = append(t.untranslated, Item{Path: path, Construct: oneLine(construct), Reason: reason})
	if construct == "" || construct == path {
		return "# UNTRANSLATED: " + path + ": " + reason
	}
	return "# UNTRANSLATED: " + path + " (" + oneLine(construct) + "): " + reason
}

// note records a mechanical rewrite worth a reviewer's eye. It is not a
// failure and does not change the exit code.
func (t *translator) note(path, construct, reason string) {
	reason = oneLine(reason)
	t.notes = append(t.notes, Item{Path: path, Construct: oneLine(construct), Reason: reason})
}

// header prepends the conversion banner. It deliberately carries no timestamp:
// converting the same source twice must produce the same bytes.
func (t *translator) header(doc *yaml.Node) {
	line := "# Converted from a " + string(t.platform) + " workflow by `spindle threads import --from " +
		string(t.platform) + "`.\n# This is a Spindle workflow: review the conversion report; untranslated constructs are marked below."
	doc.HeadComment = joinComment(line, doc.HeadComment)
}

// --- node helpers ----------------------------------------------------------

// mapIndex returns the index of key in a mapping's Content, or -1.
func mapIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// drop removes the key/value pair at i (an even index) and places comment at the
// removed key's site: on the next surviving key, or on the mapping itself when
// the removed pair was the last one. A comment already sitting on the value (a
// refusal recorded inside the removed subtree) is lifted into the site comment,
// so no refusal loses its marker when its container goes.
func (t *translator) drop(m *yaml.Node, i int, comment string) {
	if i < 0 || i+1 >= len(m.Content) {
		return
	}
	if inner := m.Content[i+1].HeadComment; inner != "" {
		m.Content[i+1].HeadComment = ""
		comment = joinComment(inner, comment)
	}
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	t.commentAt(m, i, comment)
}

// dropItem removes a sequence item and moves its comment to the next item (or
// the sequence, when it was last). A flow sequence is widened to block style:
// a comment inside `[ … ]` would otherwise render between the brackets.
func (t *translator) dropItem(seq *yaml.Node, i int, comment string) {
	if i < 0 || i >= len(seq.Content) {
		return
	}
	seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
	if comment == "" {
		return
	}
	if seq.Style == yaml.FlowStyle {
		seq.Style = 0
	}
	if i < len(seq.Content) {
		seq.Content[i].HeadComment = joinComment(seq.Content[i].HeadComment, comment)
		return
	}
	seq.HeadComment = joinComment(seq.HeadComment, comment)
}

// commentAt attaches comment to the key now at index i (or to the mapping). A
// flow mapping is widened to block style for the same reason as dropItem.
func (t *translator) commentAt(m *yaml.Node, i int, comment string) {
	if comment == "" {
		return
	}
	if m.Style == yaml.FlowStyle {
		m.Style = 0
	}
	if i >= 0 && i < len(m.Content) {
		m.Content[i].HeadComment = joinComment(m.Content[i].HeadComment, comment)
		return
	}
	m.HeadComment = joinComment(m.HeadComment, comment)
}

// joinComment appends a comment line to an existing HeadComment block.
func joinComment(existing, add string) string {
	if existing == "" {
		return add
	}
	if add == "" {
		return existing
	}
	return existing + "\n" + add
}

// oneLine keeps a comment and a report entry on one line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}

// isNull reports a YAML null scalar (`key:` with no value).
func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Tag == "" && n.Value == ""))
}

// scalarText returns the scalar's text, or ok=false for a mapping/sequence.
func scalarText(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// failScalar rewrites a node into a plain string scalar.
func setScalar(n *yaml.Node, value string) {
	n.Kind = yaml.ScalarNode
	n.Tag = "!!str"
	n.Style = 0
	n.Value = value
	n.Content = nil
}

// setSequence rewrites a node into a block sequence of string scalars.
func setSequence(n *yaml.Node, values []string) {
	n.Kind = yaml.SequenceNode
	n.Tag = "!!seq"
	n.Style = 0
	n.Value = ""
	n.Content = make([]*yaml.Node, 0, len(values))
	for _, v := range values {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
}

// failureScript is the body of a step that stands in for a construct with no
// translation. It fails loudly with the reason instead of doing nothing.
func failureScript(reason string) string {
	reason = oneLine(reason)
	reason = strings.ReplaceAll(reason, "'", `'\''`)
	return "echo 'spindle: UNTRANSLATED: " + reason + "' >&2\nexit 1"
}

// stringsExpr matches a `${{ … }}` span holding exactly one dotted reference.
var dottedExpr = regexp.MustCompile(`\$\{\{\s*([A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z_][A-Za-z0-9_-]*)+)\s*\}\}`)

// liftEmptyComments moves a head comment off an empty collection, where the
// encoder would render it inside `{}`/`[]`, onto the key that owns it.
func liftEmptyComments(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			liftEmptyComments(c)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			liftEmptyComments(val)
			if len(val.Content) > 0 || val.HeadComment == "" {
				continue
			}
			if val.Kind == yaml.MappingNode || val.Kind == yaml.SequenceNode {
				key.HeadComment = joinComment(key.HeadComment, val.HeadComment)
				val.HeadComment = ""
			}
		}
	}
}

// --- alias rewrites --------------------------------------------------------

var (
	// github.event.inputs.x is the GitHub spelling of the dispatch input that
	// docs/WORKFLOWS.md §10.1 translates to inputs.x.
	eventInputsRe = regexp.MustCompile(`\$\{\{\s*github\.event\.inputs\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}`)
	// A repository secret in a script is an environment variable of its own
	// name; GITHUB_TOKEN is the automatic job token.
	secretInRunRe = regexp.MustCompile(`\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	// GITHUB_SERVER_URL is the address the job reaches spindle at; the
	// canonical spelling here is SPINDLE_SERVER.
	serverURLRe = regexp.MustCompile(`\$\{GITHUB_SERVER_URL\}|\$GITHUB_SERVER_URL\b`)
	// github.actor resolves in a job-level condition; the runner does not wire
	// it into a step condition yet.
	actorRe = regexp.MustCompile(`\$\{\{\s*github\.actor\b`)
)

// rewrite walks every scalar value (never a key) and normalizes the documented
// aliases. runBody is true inside a step's `run:` script, the one place a
// secret expression is turned back into a shell variable (docs/WORKFLOWS.md
// §10.1: the secret is already `$NAME`, and a script is the only site where
// `$NAME` expands).
func (t *translator) rewrite(n *yaml.Node, path string, runBody bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			t.rewrite(c, path, false)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			p := key.Value
			if path != "" {
				p = path + "." + key.Value
			}
			t.rewrite(val, p, key.Value == "run")
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			t.rewrite(c, fmt.Sprintf("%s[%d]", path, i+1), runBody)
		}
	case yaml.ScalarNode:
		if isNull(n) {
			return
		}
		t.rewriteText(n, path, runBody)
	}
}

func (t *translator) rewriteText(n *yaml.Node, path string, runBody bool) {
	text := n.Value
	out := text

	if runBody {
		out = secretInRunRe.ReplaceAllStringFunc(out, func(m string) string {
			name := secretInRunRe.FindStringSubmatch(m)[1]
			if strings.EqualFold(name, "GITHUB_TOKEN") {
				t.note(path, m, "a run script reads repository secrets as environment variables; the job token is $SPINDLE_JOB_TOKEN")
				return "${SPINDLE_JOB_TOKEN}"
			}
			t.note(path, m, "a run script reads a repository secret as an environment variable of its own name")
			return "${" + name + "}"
		})
	}
	out = eventInputsRe.ReplaceAllStringFunc(out, func(m string) string {
		name := eventInputsRe.FindStringSubmatch(m)[1]
		t.note(path, m, "the dispatch-input alias is inputs."+name)
		return "${{ inputs." + name + " }}"
	})
	if actorRe.MatchString(out) {
		t.note(path, "github.actor", "github.actor resolves in a job-level if: but is not wired into a step if: yet; use a secret or a fixed name for a step condition")
	}
	if serverURLRe.MatchString(out) {
		t.note(path, "GITHUB_SERVER_URL", "rewrote GITHUB_SERVER_URL to the canonical SPINDLE_SERVER")
		out = strings.ReplaceAll(out, "${GITHUB_SERVER_URL}", "${SPINDLE_SERVER}")
		out = serverURLRe.ReplaceAllLiteralString(out, "$SPINDLE_SERVER")
	}
	if out != text {
		n.Value = out
		n.Tag = "!!str"
	}
}

// --- top level -------------------------------------------------------------

// workflow translates the document's top-level keys. `on` is done first (a
// concurrency group may reference a declared dispatch input), jobs last (a job
// may be the only thing that makes the file valid).
func (t *translator) workflow(doc *yaml.Node) error {
	if i := mapIndex(doc, "on"); i >= 0 {
		t.triggers(doc, i)
	}
	for i := 0; i < len(doc.Content); i += 2 {
		key := doc.Content[i]
		switch key.Value {
		case "on", "jobs":
			continue
		case "name":
			if _, ok := scalarText(doc.Content[i+1]); !ok {
				t.drop(doc, i, t.untranslatedAt("name", "name", "a workflow name must be a string"))
				i -= 2
			}
		case "env":
			t.stringMapAt(doc, i, "env")
		case "concurrency":
			t.concurrencyAt(doc, i, "concurrency", false, nil)
		case "permissions":
			t.permissionsAt(doc, i, "permissions")
		default:
			reason := "not part of the Spindle workflow schema"
			if key.Value == "run-name" {
				reason = "a runtime run name is not supported yet"
			}
			if key.Value == "defaults" {
				reason = "defaults.run is not supported yet; set shell/working-directory on each step"
			}
			t.drop(doc, i, t.untranslatedAt(key.Value, key.Value, reason))
			i -= 2
		}
	}
	i := mapIndex(doc, "jobs")
	if i < 0 {
		return fmt.Errorf("%w: the workflow has no `jobs:` block", ErrUnconvertible)
	}
	return t.jobs(doc, i)
}

// --- shared mapping shapes -------------------------------------------------

// stringMapAt keeps a mapping of scalar values (env, with, container.env).
func (t *translator) stringMapAt(m *yaml.Node, i int, path string) {
	val := m.Content[i+1]
	if isNull(val) {
		t.drop(m, i, "")
		return
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "env", "expected a mapping of name: value"))
		return
	}
	for j := 0; j < len(val.Content); j += 2 {
		p := path + "." + val.Content[j].Value
		if _, ok := scalarText(val.Content[j+1]); !ok {
			reason := "a value must be a string; a nested mapping or list has no env spelling"
			t.drop(val, j, t.untranslatedAt(p, "env value", reason))
			j -= 2
			continue
		}
		t.translated++
	}
}

// permissionsAt carries the permission map. Only contents and id-token drive a
// capability; the rest parse and are inert until the scopes land
// (docs/threads.md §5.3), so they are called out rather than silently trusted.
func (t *translator) permissionsAt(m *yaml.Node, i int, path string) {
	val := m.Content[i+1]
	inert := map[string]bool{}
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			return
		}
	case yaml.MappingNode:
		for j := 0; j+1 < len(val.Content); j += 2 {
			name := val.Content[j].Value
			if _, ok := scalarText(val.Content[j+1]); !ok {
				t.drop(val, j, t.untranslatedAt(path+"."+name, "permissions."+name, "a permission level must be a string"))
				j -= 2
				continue
			}
			if name != "contents" && name != "id-token" && name != "all" {
				inert[name] = true
			}
		}
	default:
		t.drop(m, i, t.untranslatedAt(path, "permissions", "expected a mapping or read-all/write-all"))
		return
	}
	if len(inert) > 0 {
		names := make([]string, 0, len(inert))
		for name := range inert {
			names = append(names, name)
		}
		sort.Strings(names)
		t.note(path, "permissions: "+strings.Join(names, ", "),
			"parses but drives no capability yet; only contents and id-token are enforced")
	}
	t.translated++
}

// concurrencyAt translates and validates a concurrency block against the same
// rule the parser applies. A group the parser would reject (an unsupported
// context, an undeclared matrix axis or input, an empty mapping) is refused as
// a unit, so the generated file stays valid and the loss is named.
func (t *translator) concurrencyAt(m *yaml.Node, i int, path string, jobLevel bool, axes []string) {
	val := m.Content[i+1]
	group := ""
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			t.drop(m, i, "")
			return
		}
		group = val.Value
		t.translated++
	case yaml.MappingNode:
		for j := 0; j < len(val.Content); j += 2 {
			key := val.Content[j]
			switch key.Value {
			case "group":
				text, ok := scalarText(val.Content[j+1])
				if !ok {
					t.drop(val, j, t.untranslatedAt(path+".group", "concurrency.group", "the group must be a string"))
					j -= 2
					continue
				}
				group = text
			case "cancel-in-progress":
				if val.Content[j+1].Tag != "!!bool" {
					t.drop(val, j, t.untranslatedAt(path+".cancel-in-progress", "concurrency.cancel-in-progress",
						"only a literal true/false is supported; an expression is not"))
					j -= 2
					continue
				}
			default:
				t.drop(val, j, t.untranslatedAt(path+"."+key.Value, "concurrency."+key.Value, "not part of the Spindle concurrency schema"))
				j -= 2
			}
		}
	default:
		t.drop(m, i, t.untranslatedAt(path, "concurrency", "expected a group name or a mapping"))
		return
	}
	if len(val.Content) == 0 && val.Kind == yaml.MappingNode {
		// Every key was refused: remove the block rather than leave a no-op.
		t.drop(m, i, "")
		return
	}
	if err := workflow.ValidateGroupTemplate(group, axes, t.inputNames, !jobLevel); err != nil {
		t.drop(m, i, t.untranslatedAt(path, "concurrency", "the group cannot be expanded here: "+err.Error()))
		return
	}
	if group == "" {
		// A mapping with only cancel-in-progress has no group; the parser's
		// validator accepts an empty group, but a group of "" is every-run.
		t.note(path, "concurrency", "no group name; the block sequences nothing")
	}
	t.translated++
}

// environmentAt validates a job environment with the parser's own rule.
func (t *translator) environmentAt(m *yaml.Node, i int, path, job string) {
	val := m.Content[i+1]
	env := &workflow.Environment{}
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			t.drop(m, i, "")
			return
		}
		env.Name = val.Value
	case yaml.MappingNode:
		for j := 0; j < len(val.Content); j += 2 {
			key := val.Content[j]
			text, ok := scalarText(val.Content[j+1])
			if !ok {
				t.drop(val, j, t.untranslatedAt(path+"."+key.Value, "environment."+key.Value, "the value must be a string"))
				j -= 2
				continue
			}
			switch key.Value {
			case "name":
				env.Name = text
			case "url":
				env.URL = text
			default:
				t.drop(val, j, t.untranslatedAt(path+"."+key.Value, "environment."+key.Value, "not part of the Spindle environment schema"))
				j -= 2
			}
		}
	default:
		t.drop(m, i, t.untranslatedAt(path, "environment", "expected a name or a mapping"))
		return
	}
	if err := env.Validate(job); err != nil {
		t.drop(m, i, t.untranslatedAt(path, "environment", err.Error()))
		return
	}
	t.translated++
}
