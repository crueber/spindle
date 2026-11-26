package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Status values a row may carry (docs/features/21_parity_master.md §9).
const (
	StatusHave    = "have"
	StatusPartial = "partial"
	StatusMissing = "missing"
	StatusNonGoal = "non-goal"
)

// areas is the render order of the matrix's per-area tables.
var areas = []string{
	"discovery", "triggers", "expressions", "jobs", "steps", "commands",
	"secrets", "environments", "concurrency", "runners", "observability",
	"conformance",
}

// Row is one PAR- entry.
type Row struct {
	ID         string   `json:"id"`
	Area       string   `json:"area"`
	Capability string   `json:"capability"`
	Provenance string   `json:"provenance"`
	Status     string   `json:"status"`
	Wave       int      `json:"wave"`
	VerifyOnly bool     `json:"verify_only,omitempty"`
	Track      []string `json:"track"`
	Security   []string `json:"security,omitempty"`
	Decision   string   `json:"decision,omitempty"`
	ApprovedBy string   `json:"approved_by,omitempty"`
	Acceptance string   `json:"acceptance,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	DocsRefs   []string `json:"docs_refs,omitempty"`
	Tests      []string `json:"tests,omitempty"`
}

// DocsRevision is the plan's docs stamp (the matrix header repeats it verbatim).
type DocsRevision struct {
	Forgejo    string `json:"forgejo"`
	GitHub     string `json:"github"`
	Checked    string `json:"checked"`
	Plan       string `json:"plan"`
	PlanCommit string `json:"plan_commit,omitempty"`
}

// Doc is parity.json: the docs stamp plus one row per PAR- id.
type Doc struct {
	DocsRevision DocsRevision `json:"docs_revision"`
	Rows         []*Row       `json:"rows"`
}

// Load reads parity.json.
func Load(path string) (*Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read parity.json: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var doc Doc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Rows) == 0 {
		return nil, fmt.Errorf("%s: no rows", path)
	}
	return &doc, nil
}

// TestIndex maps every Test function name found under the tree to the files
// that declare it. A name may repeat across packages (Go allows it), so the
// index keeps every file.
type TestIndex map[string][]string

// ScanTests walks <root>/internal for *_test.go and indexes `func TestXxx(`.
// Every implementation package lives under internal/, so a row cannot claim a
// test that lives nowhere the gate looks.
func ScanTests(root string) (TestIndex, error) {
	idx := TestIndex{}
	fn := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	err := filepathWalk(root, func(path string) error {
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := relPath(root, path)
		for _, m := range fn.FindAllStringSubmatch(string(raw), -1) {
			idx[m[1]] = append(idx[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(idx) == 0 {
		return nil, fmt.Errorf("no Test functions found under %s/internal", root)
	}
	return idx, nil
}

// ValidateOptions is what the structural rules need from the world.
type ValidateOptions struct {
	Tests   TestIndex
	PlanIDs map[string]bool
	Strict  bool
}

// Finding is one rule violation, naming the row it belongs to (so a red gate
// says WHICH item, never just "the matrix is wrong").
type Finding struct {
	ID     string
	Detail string
}

func (f Finding) String() string {
	if f.ID == "" {
		return f.Detail
	}
	return f.ID + ": " + f.Detail
}

var (
	idRe     = regexp.MustCompile(`^PAR-[0-9]{3}$`)
	verifyRe = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
)

// Validate applies every integrity rule (and the closure rule in strict mode).
func (d *Doc) Validate(opt ValidateOptions) []Finding {
	var out []Finding
	add := func(id, format string, args ...any) {
		out = append(out, Finding{ID: id, Detail: fmt.Sprintf(format, args...)})
	}
	seen := map[string]bool{}
	areaSeen := map[string]bool{}
	for _, a := range areas {
		areaSeen[a] = true
	}
	for _, r := range d.Rows {
		if r == nil {
			add("", "null row")
			continue
		}
		if !idRe.MatchString(r.ID) {
			add(r.ID, "id must be PAR-NNN")
			continue
		}
		if seen[r.ID] {
			add(r.ID, "appears twice (every PAR- id appears exactly once)")
		}
		seen[r.ID] = true
		if !areaSeen[r.Area] {
			add(r.ID, "unknown area %q", r.Area)
		}
		if strings.TrimSpace(r.Capability) == "" {
			add(r.ID, "no capability")
		}
		if len(r.Track) == 0 {
			add(r.ID, "no track")
		}
		if r.Wave < 1 || r.Wave > 4 {
			add(r.ID, "wave = %d, want 1..4", r.Wave)
		}
		switch r.Status {
		case StatusHave:
			if len(r.Tests) == 0 {
				add(r.ID, "status have with no test: a green row must name its acceptance test")
			}
			for _, name := range r.Tests {
				if err := opt.checkTest(name); err != nil {
					add(r.ID, "%v", err)
				}
			}
		case StatusPartial, StatusMissing:
			if r.Wave == 0 && r.Decision == "" {
				add(r.ID, "status %s with neither a wave nor a decision (unplanned gap)", r.Status)
			}
			if opt.Strict && r.Status == StatusMissing {
				add(r.ID, "status missing in closure mode (§9: no missing row at the plan's end)")
			}
		case StatusNonGoal:
			if strings.TrimSpace(r.ApprovedBy) == "" {
				add(r.ID, "non-goal without approved_by")
			}
		default:
			add(r.ID, "unknown status %q", r.Status)
		}
		if r.Decision != "" && !regexp.MustCompile(`^D-[0-9]+$`).MatchString(r.Decision) {
			add(r.ID, "decision %q is not a D-n id", r.Decision)
		}
	}
	for _, id := range sortedKeys(opt.PlanIDs) {
		if !seen[id] {
			add(id, "named by the master plan but absent from parity.json")
		}
	}
	for id := range seen {
		if len(opt.PlanIDs) > 0 && !opt.PlanIDs[id] {
			add(id, "in parity.json but the master plan does not list it")
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

// checkTest resolves one tests[] entry: a bare Test name (searched anywhere
// under internal/) or `path/to/file_test.go#TestName` (the file must declare it).
func (opt ValidateOptions) checkTest(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("empty test reference")
	}
	name := entry
	if i := strings.LastIndexAny(entry, "#:"); i >= 0 {
		file, rest := entry[:i], entry[i+1:]
		if !strings.HasSuffix(file, "_test.go") {
			return fmt.Errorf("test %q names %q, which is not a _test.go file", entry, file)
		}
		if filepathBase(file) == "" {
			return fmt.Errorf("test %q has no file name", entry)
		}
		name = rest
		files, ok := opt.Tests[name]
		if !ok {
			return fmt.Errorf("test %s does not exist", name)
		}
		for _, got := range files {
			if strings.HasSuffix(got, file) || filepathBase(got) == filepathBase(file) {
				return nil
			}
		}
		return fmt.Errorf("test %s lives in %v, not %s", name, files, file)
	}
	if !verifyRe.MatchString(name) {
		return fmt.Errorf("test %q is not a Test… function name", entry)
	}
	if _, ok := opt.Tests[name]; !ok {
		return fmt.Errorf("test %s does not exist under internal/**", name)
	}
	return nil
}

// Render is the deterministic matrix.md body (the gate compares it byte for
// byte, so a hand edit fails).
func (d *Doc) Render() string {
	var b strings.Builder
	rev := d.DocsRevision
	fmt.Fprintf(&b, "<!-- GENERATED by internal/devtools/paritygate from parity.json — do not edit by hand. -->\n")
	fmt.Fprintf(&b, "# Threads parity matrix\n\n")
	fmt.Fprintf(&b, "Docs revision stamp: **forgejo %s · github %s · checked %s**.\n\n",
		rev.Forgejo, rev.GitHub, rev.Checked)
	fmt.Fprintf(&b, "Source of truth: [`parity.json`](parity.json) (one row per `PAR-` id). The plan is\n")
	fmt.Fprintf(&b, "[`21_parity_master.md`](../21_parity_master.md). Regenerate with\n")
	fmt.Fprintf(&b, "`go run ./internal/devtools/paritygate -check=false`; gate with `make parity`.\n\n")
	counts := map[string]int{}
	for _, r := range d.Rows {
		counts[r.Status]++
	}
	fmt.Fprintf(&b, "**%d rows** — %d have · %d partial · %d missing · %d non-goal.\n",
		len(d.Rows), counts[StatusHave], counts[StatusPartial], counts[StatusMissing], counts[StatusNonGoal])
	fmt.Fprintf(&b, "A `have` row is green only because its acceptance test is committed and named here;\n")
	fmt.Fprintf(&b, "the gate fails a green row whose test is missing from the tree.\n")
	for _, area := range areas {
		var rows []*Row
		for _, r := range d.Rows {
			if r.Area == area {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		fmt.Fprintf(&b, "\n## %s\n\n", area)
		fmt.Fprintf(&b, "| ID | Capability | Status | Wave | Track | Security | Tests |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
		for _, r := range rows {
			status := r.Status
			if r.VerifyOnly {
				status += " (1V)"
			}
			if r.Decision != "" && r.Status != StatusHave {
				status += " · " + r.Decision
			}
			tests := "—"
			if len(r.Tests) > 0 {
				tests = strings.Join(quoteAll(r.Tests), "<br>")
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %s |\n",
				r.ID, escapeCell(r.Capability), escapeCell(status), r.Wave,
				escapeCell(strings.Join(r.Track, "+")), escapeCell(strings.Join(r.Security, " ")), escapeCell(tests))
		}
	}
	return b.String()
}

// Summary is the line `make parity` prints on success.
func (d *Doc) Summary(strict bool) string {
	counts := map[string]int{}
	for _, r := range d.Rows {
		counts[r.Status]++
	}
	mode := "integrity"
	if strict {
		mode = "closure"
	}
	return fmt.Sprintf("paritygate: %d rows (%s mode) — have %d, partial %d, missing %d, non-goal %d; matrix.md fresh\n",
		len(d.Rows), mode, counts[StatusHave], counts[StatusPartial], counts[StatusMissing], counts[StatusNonGoal])
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, "`"+s+"`")
	}
	return out
}

// escapeCell makes a value safe inside a markdown table cell.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
