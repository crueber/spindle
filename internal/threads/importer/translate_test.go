package importer

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// update regenerates the golden files (`go test ./internal/threads/importer -run Golden -update`).
var update = flag.Bool("update", false, "rewrite the golden conversion files")

// corpusEntry is one real-world source file the goldens pin.
type corpusEntry struct {
	name string // file name under testdata/corpus/forgejo and testdata/golden/forgejo
	// wantUntranslated is the number of refused constructs the conversion is
	// expected to report. A change here is a deliberate change to the refusal
	// list, never an accident.
	wantUntranslated int
}

var corpus = []corpusEntry{
	{name: "weave-ci.yml", wantUntranslated: 0},
	{name: "weave-publish-github.yml", wantUntranslated: 0},
	{name: "scriobh-ci.yml", wantUntranslated: 0},
	// The release build is an include-only matrix (one object per cell). The
	// target expands include/exclude with GitHub's merge rules, so it converts
	// with no refusal (docs/threads.md §3.3a).
	{name: "scriobh-release.yml", wantUntranslated: 0},
}

func TestCorpusGolden(t *testing.T) {
	for _, entry := range corpus {
		t.Run(entry.name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "corpus", "forgejo", entry.name))
			if err != nil {
				t.Fatal(err)
			}
			res, err := Translate(Forgejo, src)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if _, err := workflow.Parse(res.Output); err != nil {
				t.Fatalf("the converted file does not parse: %v\n%s", err, res.Output)
			}
			if got := len(res.Untranslated); got != entry.wantUntranslated {
				t.Errorf("untranslated constructs = %d, want %d:\n%v", got, entry.wantUntranslated, res.Untranslated)
			}
			// Every refusal must be visible in the file itself.
			for _, item := range res.Untranslated {
				if !strings.Contains(string(res.Output), "# UNTRANSLATED: "+item.Path) {
					t.Errorf("no UNTRANSLATED comment for %s in:\n%s", item.Path, res.Output)
				}
			}
			goldenPath := filepath.Join("testdata", "golden", "forgejo", entry.name)
			if *update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, res.Output, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(want) != string(res.Output) {
				t.Errorf("converted output differs from %s:\n--- got ---\n%s", goldenPath, res.Output)
			}
			// Idempotence: the same source produces the same bytes.
			again, err := Translate(Forgejo, src)
			if err != nil {
				t.Fatal(err)
			}
			if string(again.Output) != string(res.Output) {
				t.Error("a second conversion of the same source produced different bytes")
			}
		})
	}
}

// TestCorpusSourceProvenance pins the SOURCE stamp and that every copied file
// is present and non-empty: the corpus is evidence, so its provenance is part
// of the test.
func TestCorpusSourceProvenance(t *testing.T) {
	stamp, err := os.ReadFile(filepath.Join("testdata", "corpus", "forgejo", "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []string{
		".github/workflows/ci.yml",
		".github/workflows/publish-github.yml",
		".github/workflows/release.yml",
		"crueber/weave",
		"crueber/scriobh",
		"5b2526d42ab076d1b80c39b60c13372994c23bdf",
		"034c9a50d40ea5c0404cfe5a66b3f3782d3068a4",
	} {
		if !strings.Contains(string(stamp), upstream) {
			t.Errorf("SOURCE lost %q:\n%s", upstream, stamp)
		}
	}
	for _, entry := range corpus {
		info, err := os.Stat(filepath.Join("testdata", "corpus", "forgejo", entry.name))
		if err != nil || info.Size() == 0 {
			t.Errorf("corpus file %s is missing or empty: %v", entry.name, err)
		}
	}
}
