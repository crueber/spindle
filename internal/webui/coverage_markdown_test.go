package webui

// coverage_markdown_test.go — the renderer's remaining branch coverage: the
// tight (list-item) block dispatcher, malformed inline constructs, and the
// byte-level helpers. The happy-path vectors live in markdown_test.go; these
// cases pin the fallbacks that must never panic or leak raw markup.

import (
	"strings"
	"testing"
)

func TestCoverageBlocksTightDispatchesEveryBlock(t *testing.T) {
	t.Parallel()
	r := &mdRenderer{}
	out := r.blocksTight([]string{
		"",
		"```go",
		"x := 1",
		"```",
		"# Heading",
		"---",
		"> quoted",
		"",
		"a | b",
		"--- | ---",
		"1 | 2",
		"",
		"- nested",
		"",
		`<div align="center">raw</div>`,
	})
	for _, want := range []string{
		`<pre><code class="language-go">`,
		"<h2>Heading</h2>",
		"<hr>",
		"<blockquote>",
		"<table",
		"<li>",
		`<div align="center">raw</div>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("blocksTight missing %q in %q", want, out)
		}
	}
}

func TestCoverageFenceLanguageCharset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"go", "go"},
		{"c++", "c++"},
		{"js-x_y#z.w", "js-x_y#z.w"},
		{"go {.numberLines}", "go"},
		{"go`<script>`", "goscript"},
	} {
		if got := fenceLanguage(tc.in); got != tc.want {
			t.Fatalf("fenceLanguage(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoverageListBlankLineContinuation(t *testing.T) {
	t.Parallel()
	// A blank line inside an item keeps the indented continuation (the
	// CommonMark loose-item rule), so both paragraphs belong to one <li>.
	out := md("- one\n\n  two\n")
	if strings.Count(out, "<li>") != 1 {
		t.Fatalf("blank line split the item: %q", out)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("continuation lost: %q", out)
	}
}

func TestCoverageTaskBoxFallback(t *testing.T) {
	t.Parallel()
	if rest, checked, ok := taskBox("[x] done"); !ok || !checked || rest != "done" {
		t.Fatalf("checked task = %q %v %v", rest, checked, ok)
	}
	if rest, checked, ok := taskBox("[ ] todo"); !ok || checked || rest != "todo" {
		t.Fatalf("open task = %q %v %v", rest, checked, ok)
	}
	// Unknown checkbox content is not a task box: the item renders verbatim.
	if _, _, ok := taskBox("[-] nope"); ok {
		t.Fatal("unknown checkbox must not be a task box")
	}
	if _, _, ok := taskBox("[x"); ok {
		t.Fatal("short marker must not be a task box")
	}
}

func TestCoverageBlockPredicates(t *testing.T) {
	t.Parallel()
	if isThematicBreak("   ") {
		t.Fatal("blank line is not a thematic break")
	}
	if !isThematicBreak("---") || !isThematicBreak(" * * * ") {
		t.Fatal("thematic breaks must be recognised")
	}
	if _, ok := setextLevel(""); ok {
		t.Fatal("blank line is not a setext underline")
	}
	for _, line := range []string{"```go", "# heading", "---", "> quote"} {
		if !isBlockStart(line) {
			t.Fatalf("isBlockStart(%q) = false", line)
		}
	}
	if isBlockStart("plain paragraph") {
		t.Fatal("plain text must not start a block")
	}
	if isDelimiterCell(":") {
		t.Fatal("a lone colon is not a delimiter cell")
	}
	if !isDelimiterCell(":--:") || !isDelimiterCell("---") {
		t.Fatal("delimiter cells must be recognised")
	}
}

func TestCoverageEmphasisFallbacks(t *testing.T) {
	t.Parallel()
	// An underscore run whose closing candidate is preceded by a space is not
	// emphasis at all (intraword/space rules), so the text survives verbatim.
	if out := md("_a _"); !strings.Contains(out, "_a _") {
		t.Fatalf("space-flanked underscore run = %q", out)
	}
	// A triple run falls back to the double, then to the single run: the
	// rejected candidate keeps the scan alive instead of giving up.
	if out := md("___a __ b_"); !strings.Contains(out, "<em>") {
		t.Fatalf("retry emphasis = %q", out)
	}
	// An unclosed run is literal text.
	if out := md("*abc"); !strings.Contains(out, "*abc") {
		t.Fatalf("unclosed emphasis = %q", out)
	}
}

func TestCoverageLinkFallbacks(t *testing.T) {
	t.Parallel()
	// No closing bracket: literal text.
	if out := md("[x"); !strings.Contains(out, "[x") {
		t.Fatalf("unclosed label = %q", out)
	}
	// Closing bracket but no destination parens: literal text.
	if out := md("[x] y"); !strings.Contains(out, "[x] y") {
		t.Fatalf("label without destination = %q", out)
	}
	// A backslash-escaped bracket stays inside the label.
	out := md(`[a\]b](/x)`)
	if !strings.Contains(out, `href="/x"`) {
		t.Fatalf("escaped bracket link = %q", out)
	}
	// Whitespace after "(" and around a quoted title is tolerated.
	out = md(`[x]( /y "t"  )`)
	if !strings.Contains(out, `href="/y"`) {
		t.Fatalf("spaced destination = %q", out)
	}
}

func TestCoverageBareURLTerminatorsAndRefs(t *testing.T) {
	t.Parallel()
	// A double quote ends the autolink (the renderer must not swallow markup).
	out := md(`see https://x.test/a"b`)
	if !strings.Contains(out, `href="https://x.test/a"`) {
		t.Fatalf("quote-terminated autolink = %q", out)
	}
	// A zero reference normalises to #0 rather than an empty path (refs need
	// repo coordinates; the ctx-less renderer leaves them as text).
	out = mdIn("see #0000", "o", "r")
	if !strings.Contains(out, `href="/o/r/issues/0"`) {
		t.Fatalf("zero reference = %q", out)
	}
}
