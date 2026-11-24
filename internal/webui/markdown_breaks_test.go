package webui

// markdown_breaks_test.go — the soft-break contract (CommonMark/GFM): one
// source newline inside a paragraph folds to a space, so a README reflows to
// the container instead of breaking at the author's line width. A hard break
// needs two or more trailing spaces or a trailing backslash. The reference
// fixture (testdata/readme-breaks.md) carries one case per shape; each case's
// exact HTML is pinned here, and the whole README is pin-checked for the
// things that must NOT change (code blocks verbatim, explicit <br> in the
// centered header, raw HTML blocks through the sanitizer).

import (
	"strings"
	"testing"
)

// fixtureCases splits the reference README on its "<!-- case: x -->" markers.
func fixtureCases(t *testing.T, src string) map[string]string {
	t.Helper()
	cases := map[string]string{}
	name := ""
	var cur []string
	flush := func() {
		if name != "" {
			cases[name] = strings.Join(cur, "\n")
		}
	}
	for _, line := range splitMarkdownLines(src) {
		if strings.HasPrefix(line, "<!-- case: ") && strings.HasSuffix(line, " -->") {
			flush()
			name = strings.TrimSuffix(strings.TrimPrefix(line, "<!-- case: "), " -->")
			cur = nil
			continue
		}
		if name != "" {
			cur = append(cur, line)
		}
	}
	flush()
	return cases
}

func TestRenderMarkdownSoftBreakFixture(t *testing.T) {
	t.Parallel()
	cases := fixtureCases(t, readTestdata(t, "readme-breaks.md"))
	want := map[string]string{
		// (a) the user's paragraph: one flowing block, no <br>.
		"a": "<p>A self-hosted, minimal personal boards app for links and notes. Boards\n" +
			"hold columns, columns hold cards, and each card holds an ordered mix of\n" +
			"links, markdown notes, todo checklists, and images. One small Go binary,\n" +
			"no framework JavaScript, your data in a single SQLite file.</p>\n",
		// (b) two trailing spaces are a hard break.
		"b": "<p>A hard break needs two trailing spaces<br>\n" +
			"and this sentence starts on the next line.</p>\n",
		// (c) a trailing backslash is a hard break; the marker never renders.
		"c": "<p>A hard break also takes a trailing backslash<br>\n" +
			"and this sentence starts on the next line.</p>\n",
		// (d) list items: indented continuation and lazy continuation both
		// stay in the open item paragraph.
		"d": "<ul>\n" +
			"<li><strong>Boards</strong> hold columns — switch boards from the navbar; collapse\n" +
			"columns into slim rails when you need focus.</li>\n" +
			"<li>A second card type is a markdown note whose text\n" +
			"wraps onto an unindented line and stays in the item.</li>\n" +
			"</ul>\n",
		// (e) blockquote: the unindented line continues the quote paragraph.
		"e": "<blockquote>\n" +
			"<p>A blockquote that wraps at the author's line width\n" +
			"and continues lazily on an unindented line.</p>\n" +
			"</blockquote>\n",
		// (f) the centered header: the explicit <br> survives, the newline
		// after it is a soft break.
		"f": "<div align=\"center\">\n" +
			"<h2>Boards</h2>\n" +
			"<p>A self-hosted, minimal personal boards app for links and notes<br>\n" +
			"No account, no cloud, no telemetry</p>\n" +
			"</div>\n",
		// (g) fenced code is verbatim: the backslash and the indentation are
		// content, not a hard break or markup.
		"g": "<pre><code class=\"language-sh\">docker run -p 8080:8080 \\\n" +
			"  -v boards-data:/data \\\n" +
			"  boards\n" +
			"</code></pre>\n",
		// (h) tables are unchanged.
		"h": "<table>\n<thead>\n<tr>\n<th>Card type</th>\n<th>Stored as</th>\n</tr>\n</thead>\n" +
			"<tbody>\n<tr>\n<td>Link</td>\n<td>URL + title</td>\n</tr>\n" +
			"<tr>\n<td>Note</td>\n<td>Markdown</td>\n</tr>\n</tbody>\n</table>\n",
	}
	for name, wantHTML := range want {
		src, ok := cases[name]
		if !ok {
			t.Fatalf("fixture case %q missing", name)
		}
		if got := md(src); got != wantHTML {
			t.Errorf("case %q:\n got %q\nwant %q", name, got, wantHTML)
		}
	}
	if len(cases) != len(want) {
		t.Fatalf("fixture has %d cases, want %d", len(cases), len(want))
	}
}

func TestRenderMarkdownSoftBreakFixtureWholeREADME(t *testing.T) {
	t.Parallel()
	cases := fixtureCases(t, readTestdata(t, "readme-breaks.md"))

	// The prose paragraph must contain no <br> at all — that is the defect.
	if out := md(cases["a"]); strings.Contains(out, "<br>") {
		t.Fatalf("soft-break paragraph emitted <br>: %q", out)
	}
	// The centered header keeps its authored <br>, and only that one.
	if out := md(cases["f"]); strings.Count(out, "<br>") != 1 {
		t.Fatalf("centered header breaks = %q", out)
	}
	// Code stays verbatim, including the trailing backslashes and indentation.
	code := md(cases["g"])
	for _, line := range []string{"docker run -p 8080:8080 \\\n", "  -v boards-data:/data \\\n", "  boards\n"} {
		if !strings.Contains(code, line) {
			t.Fatalf("code line %q changed: %q", line, code)
		}
	}
	if strings.Contains(code, "<br>") {
		t.Fatalf("fenced code gained <br>: %q", code)
	}

	// The whole README renders one document with the same properties, and the
	// sanitizer still owns the raw-HTML path.
	full := md(readTestdata(t, "readme-breaks.md"))
	if out := md(cases["a"]); !strings.Contains(full, strings.TrimSuffix(out, "\n")) {
		t.Fatalf("whole README lost the prose paragraph")
	}
	if strings.Contains(full, "&lt;div") {
		t.Fatalf("raw HTML was escaped in the README: %q", full)
	}
	assertURLsSafe(t, full)
}

// lazyParagraphOpen gates lazy continuation: only an open paragraph accepts
// the next unindented line, so every container state that closes it is pinned.
func TestLazyParagraphOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"empty", nil, false},
		{"paragraph", []string{"a"}, true},
		{"paragraph continues", []string{"a", "b"}, true},
		{"blank closes", []string{"a", ""}, false},
		{"blank then text", []string{"a", "", "b"}, true},
		{"heading", []string{"# h"}, false},
		{"heading then text", []string{"# h", "x"}, true},
		{"fence open", []string{"```", "x"}, false},
		{"fence closed", []string{"```", "x", "```"}, false},
		{"fence then text", []string{"```", "x", "```", "p"}, true},
		{"html block", []string{`<div>`, "x"}, false},
		{"html block closed by blank", []string{`<div>`, "x", "", "p"}, true},
		{"list marker", []string{"- x"}, false},
		{"quote marker", []string{"> q"}, false},
		{"thematic break", []string{"---"}, false},
	}
	for _, tc := range tests {
		if got := lazyParagraphOpen(tc.lines); got != tc.want {
			t.Errorf("%s: lazyParagraphOpen(%q) = %v want %v", tc.name, tc.lines, got, tc.want)
		}
	}
}

// TestSplitHardBreak pins the marker rule directly: only two or more trailing
// spaces or an unescaped trailing backslash are hard breaks.
func TestSplitHardBreak(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		hard bool
	}{
		{"plain", "plain", false},
		{"one space ", "one space", false},
		{"two spaces  ", "two spaces", true},
		{"three spaces   ", "three spaces", true},
		{"tab\t", "tab", false},
		{"backslash\\", "backslash", true},
		{"escaped\\\\", "escaped\\\\", false},
		{"mixed\\\\\\", "mixed\\\\", true},
		{"spaces and backslash \\", "spaces and backslash ", true},
	}
	for _, tc := range tests {
		got, hard := splitHardBreak(tc.in)
		if got != tc.want || hard != tc.hard {
			t.Errorf("splitHardBreak(%q) = %q,%v want %q,%v", tc.in, got, hard, tc.want, tc.hard)
		}
	}
}
