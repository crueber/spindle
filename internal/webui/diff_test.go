package webui

import (
	"strings"
	"testing"
)

const diffMod = `diff --git a/x.txt b/x.txt
index 1111111..2222222 100644
--- a/x.txt
+++ b/x.txt
@@ -1,3 +1,3 @@
 a
-b
+c
 d
`

func TestParseUnifiedDiffHappyPath(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(diffMod)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	f := files[0]
	if f.Path != "x.txt" || f.OldPath != "x.txt" || f.Status != "modified" || f.Binary {
		t.Fatalf("file = %+v", f)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("hunks = %d", len(f.Hunks))
	}
	if f.Added != 1 || f.Deleted != 1 {
		t.Fatalf("badges = +%d -%d", f.Added, f.Deleted)
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.NewStart != 1 || h.OldLines != 3 || h.NewLines != 3 {
		t.Fatalf("hunk header = %+v", h)
	}
	if h.Header != "@@ -1,3 +1,3 @@" {
		t.Fatalf("raw header = %q", h.Header)
	}
	wantKinds := []string{"ctx", "del", "add", "ctx"}
	wantText := []string{"a", "b", "c", "d"}
	wantOld := []int{1, 2, 0, 3}
	wantNew := []int{1, 0, 2, 3}
	for i, l := range h.Lines {
		if l.Kind != wantKinds[i] || l.Text != wantText[i] || l.OldNo != wantOld[i] || l.NewNo != wantNew[i] {
			t.Fatalf("line %d = %+v", i, l)
		}
	}
	if len(f.Meta) != 1 || f.Meta[0].Kind != "meta" || f.Meta[0].Text != "index 1111111..2222222 100644" {
		t.Fatalf("meta = %+v", f.Meta)
	}
}

func TestParseUnifiedDiffEmpty(t *testing.T) {
	t.Parallel()
	if files := ParseUnifiedDiff(""); files != nil {
		t.Fatalf("empty = %+v", files)
	}
}

func TestParseUnifiedDiffNewFile(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..789
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
`)
	f := files[0]
	if f.Status != "added" || !(f.Path == "new.txt") || f.OldPath != "" {
		t.Fatalf("file = %+v", f)
	}
	if f.Added != 2 || f.Deleted != 0 {
		t.Fatalf("badges = +%d -%d", f.Added, f.Deleted)
	}
	h := f.Hunks[0]
	if h.OldStart != 0 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 2 {
		t.Fatalf("hunk = %+v", h)
	}
	if h.Lines[0].NewNo != 1 || h.Lines[1].NewNo != 2 || h.Lines[0].OldNo != 0 {
		t.Fatalf("line numbers = %+v", h.Lines)
	}
}

func TestParseUnifiedDiffDevNullMarksAddedWithoutModeLine(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/n.txt b/n.txt
--- /dev/null
+++ b/n.txt
@@ -0,0 +1 @@
+x
`)
	if files[0].Status != "added" || files[0].Path != "n.txt" {
		t.Fatalf("file = %+v", files[0])
	}
}

func TestParseUnifiedDiffDeletedFile(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 789..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,1 +0,0 @@
-bye
`)
	f := files[0]
	if f.Status != "deleted" || f.Path != "gone.txt" || f.OldPath != "gone.txt" {
		t.Fatalf("file = %+v", f)
	}
	if f.Deleted != 1 || f.Added != 0 {
		t.Fatalf("badges = +%d -%d", f.Added, f.Deleted)
	}
	if h := f.Hunks[0]; h.OldLines != 1 || h.NewLines != 0 || h.Lines[0].OldNo != 1 || h.Lines[0].NewNo != 0 {
		t.Fatalf("hunk = %+v", h)
	}
}

func TestParseUnifiedDiffRenameUsesNewPath(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/old.txt b/renamed.txt
similarity index 95%
rename from old.txt
rename to renamed.txt
--- a/old.txt
+++ b/renamed.txt
@@ -1 +1 @@
-same
+same
`)
	f := files[0]
	if f.Path != "renamed.txt" || f.OldPath != "old.txt" || f.Status != "renamed" {
		t.Fatalf("file = %+v", f)
	}
}

func TestParseUnifiedDiffCopy(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/src.txt b/dst.txt
similarity index 100%
copy from src.txt
copy to dst.txt
`)
	f := files[0]
	if f.Path != "dst.txt" || f.OldPath != "src.txt" || f.Status != "renamed" {
		t.Fatalf("file = %+v", f)
	}
}

func TestParseUnifiedDiffBinary(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/img.png b/img.png
index 11..22 100644
Binary files a/img.png and b/img.png differ
diff --git a/data.bin b/data.bin
index 33..44 100644
GIT binary patch
literal 10
`)
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	for i, f := range files {
		if !f.Binary || f.Status != "binary" || len(f.Hunks) != 0 {
			t.Fatalf("file %d = %+v", i, f)
		}
	}
}

func TestParseUnifiedDiffAddedBinaryKeepsAddedStatus(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/pic.png b/pic.png
new file mode 100644
index 0000000..abc
Binary files /dev/null and b/pic.png differ
`)
	if f := files[0]; !f.Binary || f.Status != "added" {
		t.Fatalf("file = %+v", f)
	}
}

func TestParseUnifiedDiffMultipleFiles(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-1
+2
diff --git a/b.txt b/b.txt
--- a/b.txt
+++ b/b.txt
@@ -1 +1 @@
-3
+4
`)
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	if files[0].Path != "a.txt" || files[1].Path != "b.txt" {
		t.Fatalf("paths = %q %q", files[0].Path, files[1].Path)
	}
	if files[0].Added != 1 || files[1].Added != 1 {
		t.Fatalf("badges = %d %d", files[0].Added, files[1].Added)
	}
}

func TestParseUnifiedDiffHunkWithoutCounts(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
+y
`)
	h := files[0].Hunks[0]
	if h.OldLines != 1 || h.NewLines != 1 {
		t.Fatalf("counts = %d %d", h.OldLines, h.NewLines)
	}
}

func TestParseUnifiedDiffNoNewlineMarker(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
\ No newline at end of file
+y
+y2
\ No newline at end of file
`)
	lines := files[0].Hunks[0].Lines
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %+v", len(lines), lines)
	}
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
	}
	if strings.Join(texts, ",") != "x,y,y2" {
		t.Fatalf("texts = %v", texts)
	}
	if files[0].Added != 2 {
		t.Fatalf("additions = %d", files[0].Added)
	}
}

func TestParseUnifiedDiffBareLFIsContext(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1,2 +1,2 @@
 a

 b
`)
	lines := files[0].Hunks[0].Lines
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, l := range lines {
		if l.Kind != "ctx" {
			t.Fatalf("line %d = %+v", i, l)
		}
	}
	if lines[1].Text != "" || lines[1].OldNo != 2 || lines[1].NewNo != 2 {
		t.Fatalf("blank context = %+v", lines[1])
	}
}

func TestParseUnifiedDiffJunkTolerated(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`some leading junk
diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
+y
trailing junk
`)
	if len(files) != 1 || files[0].Added != 1 {
		t.Fatalf("files = %+v", files)
	}
}

func TestParseUnifiedDiffMalformedHeaderFallback(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git weird name here
+not really
`)
	if len(files) != 1 || files[0].Path != "weird name here" {
		t.Fatalf("files = %+v", files)
	}
}

func TestParseUnifiedDiffHunkContext(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff(`diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -10,2 +10,2 @@ func main() {
 a
-b
+c
`)
	h := files[0].Hunks[0]
	if h.Context != "func main() {" {
		t.Fatalf("context = %q", h.Context)
	}
	if h.Lines[0].OldNo != 10 || h.Lines[2].NewNo != 11 {
		t.Fatalf("numbers = %+v", h.Lines)
	}
}

func TestParseUnifiedDiffMissingTrailingNewline(t *testing.T) {
	t.Parallel()
	files := ParseUnifiedDiff("diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-x\n+y")
	if len(files) != 1 || files[0].Added != 1 || files[0].Deleted != 1 {
		t.Fatalf("files = %+v", files)
	}
}
