// languages.go — the bounded language-mix scan behind the repository pulse
// card (04_git.md §16.5, docs/features/18_repo_insights.md).
//
// ONE `git ls-tree -r -l -z <resolved-head>` yields the byte size of every
// regular blob in the served tree. Entries are grouped by a pinned
// extension→language table: this is an estimate from committed bytes, NOT a
// line counter (generated files, vendored trees and lockfiles count by size,
// exactly as they are stored). Everything outside the table is "Other".
//
// Task path only: the refresh runs this once against the serving copy it
// already materialized for the history scan; the page reads the cached
// projection and never spawns git (law 4/law 6).
package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// InsightsLanguageScanMax caps the regular blobs one language scan counts.
// Past the cap the tally stops and the result is reported as capped, so a
// pathological tree cannot turn the refresh into an unbounded walk; the
// projection says the mix is bounded rather than presenting a truncated
// sample as complete. The cap bounds the counting work; the listing itself is
// streamed (never fully buffered) so memory stays flat.
const InsightsLanguageScanMax = 50_000

// languageScanTimeout bounds the tree listing. It runs under the task context
// too; this is the hard ceiling for a pathological repository.
const languageScanTimeout = 2 * time.Minute

// OtherLanguage is the bucket for every extension outside the pinned table.
const OtherLanguage = "Other"

// InsightsLanguage is one language's byte tally in a tree.
type InsightsLanguage struct {
	Name  string
	Bytes int64
	Files int
}

// languageExts is the pinned extension→language table. It is deliberately
// small and deterministic: adding a language is a data change here, never a
// heuristic guess (no MIME sniffing, no line counting). Extensions are
// lowercased; dotfiles fall through to Other.
var languageExts = map[string]string{
	".go": "Go",

	".js":  "JavaScript",
	".jsx": "JavaScript",
	".mjs": "JavaScript",
	".cjs": "JavaScript",

	".ts":  "TypeScript",
	".tsx": "TypeScript",
	".mts": "TypeScript",
	".cts": "TypeScript",

	".css":  "CSS",
	".scss": "CSS",

	".html": "HTML",
	".htm":  "HTML",

	".sh":   "Shell",
	".bash": "Shell",
	".zsh":  "Shell",

	".py":   "Python",
	".rs":   "Rust",
	".java": "Java",

	".c":   "C",
	".h":   "C",
	".cc":  "C++",
	".cpp": "C++",
	".cxx": "C++",
	".hpp": "C++",
	".hh":  "C++",
	".hxx": "C++",

	".rb":  "Ruby",
	".php": "PHP",

	".json":     "JSON",
	".yml":      "YAML",
	".yaml":     "YAML",
	".md":       "Markdown",
	".markdown": "Markdown",
	".sql":      "SQL",
}

// languageNames pins the special filenames with no extension.
var languageNames = map[string]string{
	"dockerfile":    "Dockerfile",
	"containerfile": "Dockerfile",
	"makefile":      "Makefile",
	"gnumakefile":   "Makefile",
}

// languageFor maps one path to its language name. The base name is matched
// case-insensitively for the pinned special names; the extension is lowercased
// and everything unknown is Other.
func languageFor(p string) string {
	base := path.Base(p)
	if lang, ok := languageNames[strings.ToLower(base)]; ok {
		return lang
	}
	ext := strings.ToLower(path.Ext(base))
	if lang, ok := languageExts[ext]; ok {
		return lang
	}
	return OtherLanguage
}

// InsightsLanguages runs `git ls-tree -r -l -z <rev>` and returns the byte
// total per language (bytes desc, name asc), plus whether the entry cap
// truncated the tally. Only regular blobs count: trees are not emitted under
// -r, symlinks (120000) and gitlinks (160000) are skipped, and an unparseable
// record is ignored rather than failing the refresh.
func (l *Layer) InsightsLanguages(ctx context.Context, repo *LocalRepo, rev string, cap int) ([]InsightsLanguage, bool, error) {
	if repo == nil || repo.Path == "" {
		return nil, false, errInvalidInput("insights: nil repo")
	}
	if !ValidOid(rev) || isZeroOid(rev) {
		return nil, false, errInvalidInput("insights: bad revision %q", rev)
	}
	if cap <= 0 {
		cap = InsightsLanguageScanMax
	}
	if cap > InsightsLanguageScanMax {
		cap = InsightsLanguageScanMax
	}
	argv := []string{"ls-tree", "-r", "-l", "-z", rev}
	tally := &languageTally{cap: cap, bytes: map[string]int64{}}
	if _, err := l.runPooled(ctx, execSpec{argv: argv, dir: repo.Path, stdout: tally, timeout: languageScanTimeout}); err != nil {
		return nil, false, fmt.Errorf("insights languages %s: %w", rev, err)
	}
	return tally.sorted(), tally.capped, nil
}

// languageTally is a streaming ls-tree parser. It consumes NUL-terminated
// records so a path can contain anything but NUL, and stops counting at cap.
type languageTally struct {
	cap     int
	bytes   map[string]int64
	files   map[string]int
	count   int
	capped  bool
	partial []byte
}

// Write implements io.Writer for exec's stdout copy. It always accepts the
// bytes (never fails the subprocess) and discards records past the cap.
func (t *languageTally) Write(p []byte) (int, error) {
	chunk := p
	if len(t.partial) > 0 {
		chunk = append(t.partial, p...)
		t.partial = nil
	}
	for {
		i := bytes.IndexByte(chunk, 0)
		if i < 0 {
			// Keep the unterminated tail for the next chunk. Before the cap
			// only; past it the tail carries no information.
			if !t.capped {
				t.partial = append(t.partial[:0], chunk...)
			}
			break
		}
		t.record(chunk[:i])
		chunk = chunk[i+1:]
		if len(chunk) == 0 {
			break
		}
	}
	return len(p), nil
}

// record tallies one `<mode> <type> <oid> <size>\t<path>` line.
func (t *languageTally) record(rec []byte) {
	if t.capped || len(rec) == 0 {
		return
	}
	tab := bytes.IndexByte(rec, '\t')
	if tab < 0 {
		return
	}
	meta := strings.Fields(string(rec[:tab]))
	if len(meta) != 4 {
		return
	}
	mode, typ, sizeText := meta[0], meta[1], meta[3]
	if typ != "blob" || (mode != "100644" && mode != "100755") {
		return // tree entry, symlink (120000) or gitlink (160000)
	}
	size := int64(0)
	if sizeText != "-" {
		n, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil || n < 0 {
			return
		}
		size = n
	}
	if t.count >= t.cap {
		t.capped = true
		return
	}
	t.count++
	name := languageFor(string(rec[tab+1:]))
	if t.bytes == nil {
		t.bytes = map[string]int64{}
	}
	t.bytes[name] += size
	if t.files == nil {
		t.files = map[string]int{}
	}
	t.files[name]++
}

// sorted renders the tally deterministically: bytes desc, name asc.
func (t *languageTally) sorted() []InsightsLanguage {
	out := make([]InsightsLanguage, 0, len(t.bytes))
	for name, b := range t.bytes {
		out = append(out, InsightsLanguage{Name: name, Bytes: b, Files: t.files[name]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Name < out[j].Name
	})
	return out
}

var _ io.Writer = (*languageTally)(nil)
