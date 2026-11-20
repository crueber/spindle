package webui

import (
	"regexp"
	"strconv"
	"strings"
)

// DiffLine is one rendered line of a hunk. Kind is "ctx", "add" or "del";
// OldNo/NewNo are the 1-based line numbers on each side (0 when the line
// does not exist on that side). "hunk" and "meta" are the vocabulary used by
// the companion DiffHunk.Header and DiffFile.Meta fields.
type DiffLine struct {
	Kind  string
	OldNo int
	NewNo int
	Text  string
}

// DiffHunk is one @@ hunk. Header carries the raw "@@ -o,n +p,q @@ ctx" line;
// the parsed start/count values are kept alongside it for display-time
// gutters. Lines are the hunk body.
type DiffHunk struct {
	Header   string
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Context  string
	Lines    []DiffLine
}

// DiffFile is one file of a unified diff. Path is the display path (the NEW
// path, matching the server's stats[] convention), OldPath the pre-image for
// renames/copies. Status is "modified", "added", "deleted", "renamed" or
// "binary" (a modified binary; added/deleted/renamed binaries keep their
// change status and set Binary). Added/Deleted are per-file badge counts.
type DiffFile struct {
	Path, OldPath, Status string
	Binary                bool
	Hunks                 []DiffHunk
	Added, Deleted        int
	// Meta holds the file-header lines that are neither the "diff --git"
	// line nor the ---/+++ pair (index, mode, similarity, binaries notice).
	// Kind is "meta"; the other DiffLine fields are zero.
	Meta []DiffLine
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: (.*))?$`)

var diffMetaPrefixes = []string{
	"old mode", "new mode", "deleted file mode", "new file mode", "copy from", "copy to",
	"rename from", "rename to", "similarity index", "dissimilarity index", "index ",
}

// ParseUnifiedDiff parses `git show`/compare output (behaviourally the
// parsePatchFiles port from web/src/lib/diff.js): file headers, renames,
// copies, binary files, hunk headers, "\ No newline at end of file" markers,
// and add/del/context lines numbered from each hunk header. Unknown lines are
// skipped, never fatal: a preview must render whatever the server sent.
func ParseUnifiedDiff(raw string) []DiffFile {
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // artifact of the trailing newline
	}
	var (
		files       []DiffFile
		f           *DiffFile
		renamed     bool
		addedFile   bool
		deletedFile bool
	)
	finish := func() {
		if f == nil {
			return
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch l.Kind {
				case "add":
					f.Added++
				case "del":
					f.Deleted++
				}
			}
		}
		switch {
		case addedFile:
			f.Status = "added"
		case deletedFile:
			f.Status = "deleted"
		case renamed || (f.OldPath != "" && f.OldPath != f.Path):
			f.Status = "renamed"
		case f.Binary:
			f.Status = "binary"
		default:
			f.Status = "modified"
		}
		files = append(files, *f)
		f = nil
		renamed, addedFile, deletedFile = false, false, false
	}

	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.HasPrefix(line, "diff --git ") {
			finish()
			f = &DiffFile{}
			parseDiffGitLine(f, line)
			i++
		header:
			for i < len(lines) {
				l := lines[i]
				switch {
				case strings.HasPrefix(l, "rename from "):
					f.OldPath = stripDiffRef(l[len("rename from "):])
					renamed = true
				case strings.HasPrefix(l, "rename to "):
					f.Path = stripDiffRef(l[len("rename to "):])
					renamed = true
				case strings.HasPrefix(l, "copy from "):
					f.OldPath = stripDiffRef(l[len("copy from "):])
					renamed = true
				case strings.HasPrefix(l, "copy to "):
					f.Path = stripDiffRef(l[len("copy to "):])
					renamed = true
				case strings.HasPrefix(l, "deleted file mode"):
					deletedFile = true
				case strings.HasPrefix(l, "new file mode"):
					addedFile = true
				case strings.HasPrefix(l, "--- "):
					old := strings.TrimSpace(strings.TrimSuffix(l[4:], "\t"))
					if old == "/dev/null" {
						f.OldPath = ""
						addedFile = true
					} else {
						f.OldPath = stripDiffRef(old)
					}
					i++
					if i < len(lines) && strings.HasPrefix(lines[i], "+++ ") {
						nw := strings.TrimSpace(strings.TrimSuffix(lines[i][4:], "\t"))
						if nw == "/dev/null" {
							deletedFile = true
						} else {
							f.Path = stripDiffRef(nw)
						}
						i++
					}
					continue
				case isDiffMetaLine(l):
					f.Meta = append(f.Meta, DiffLine{Kind: "meta", Text: l})
				default:
					break header
				}
				i++
			}
			continue
		}

		if f != nil {
			if strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
				f.Binary = true
				i++
				continue
			}
			if strings.HasPrefix(line, "GIT binary patch") {
				f.Binary = true
				i++
				continue
			}
			if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
				h := DiffHunk{Header: line, Context: m[5]}
				h.OldStart = atoiOr(m[1], 0)
				h.OldLines = 1
				if m[2] != "" {
					h.OldLines = atoiOr(m[2], 0)
				}
				h.NewStart = atoiOr(m[3], 0)
				h.NewLines = 1
				if m[4] != "" {
					h.NewLines = atoiOr(m[4], 0)
				}
				f.Hunks = append(f.Hunks, h)
				i++
				continue
			}
			if len(f.Hunks) > 0 {
				h := &f.Hunks[len(f.Hunks)-1]
				if line == "" {
					h.Lines = append(h.Lines, DiffLine{Kind: "ctx", Text: ""})
					i++
					continue
				}
				switch line[0] {
				case '+':
					h.Lines = append(h.Lines, DiffLine{Kind: "add", Text: line[1:]})
					i++
					continue
				case '-':
					h.Lines = append(h.Lines, DiffLine{Kind: "del", Text: line[1:]})
					i++
					continue
				case ' ':
					h.Lines = append(h.Lines, DiffLine{Kind: "ctx", Text: line[1:]})
					i++
					continue
				case '\\':
					i++ // "\ No newline at end of file"
					continue
				}
			}
		}
		i++
	}
	finish()
	numberDiffLines(files)
	return files
}

func parseDiffGitLine(f *DiffFile, line string) {
	rest := line[len("diff --git "):]
	if strings.HasPrefix(rest, "a/") {
		if k := strings.LastIndex(rest, " b/"); k > 2 {
			f.OldPath = rest[2:k]
			f.Path = rest[k+3:]
			return
		}
	}
	f.Path = rest
}

func stripDiffRef(p string) string {
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

func isDiffMetaLine(line string) bool {
	for _, p := range diffMetaPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// numberDiffLines assigns old/new line numbers from each hunk header: newNo
// advances on context/add, oldNo on context/del (GitHub's unified numbering).
func numberDiffLines(files []DiffFile) {
	for fi := range files {
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			oldNo := h.OldStart
			newNo := h.NewStart
			for li := range h.Lines {
				l := &h.Lines[li]
				switch l.Kind {
				case "add":
					l.NewNo = newNo
					newNo++
				case "del":
					l.OldNo = oldNo
					oldNo++
				default:
					l.OldNo = oldNo
					l.NewNo = newNo
					oldNo++
					newNo++
				}
			}
		}
	}
}
