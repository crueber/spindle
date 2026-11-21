package webui

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

func md(src string) string { return string(RenderMarkdown(src)) }

func mdIn(src, owner, repo string) string { return string(RenderMarkdownIn(src, owner, repo)) }

var attrRe = regexp.MustCompile(`(?:href|src)="([^"]*)"`)

func urlAttrs(out string) []string {
	var vals []string
	for _, m := range attrRe.FindAllStringSubmatch(out, -1) {
		vals = append(vals, html.UnescapeString(m[1]))
	}
	return vals
}

// assertURLsSafe is the security tripwire: every href/src in rendered output
// must be http, https, mailto or relative (never protocol-relative), and no
// control characters may survive into a URL.
func assertURLsSafe(t *testing.T, out string) {
	t.Helper()
	for _, u := range urlAttrs(out) {
		low := strings.ToLower(u)
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		if strings.HasPrefix(low, "//") || strings.ContainsAny(u, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d") {
			t.Fatalf("unsafe URL %q in %q", u, out)
		}
		for _, bad := range []string{"javascript:", "data:", "vbscript:", "file:", "ftp:", "jar:"} {
			if strings.HasPrefix(low, bad) {
				t.Fatalf("unsafe scheme %q in %q", u, out)
			}
		}
		if strings.HasPrefix(u, "/") {
			continue // same-origin relative path
		}
		if schemeRe.MatchString(u) {
			if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
				t.Fatalf("unexpected scheme in %q", u)
			}
		}
	}
}

// --- the JS test vectors (blob-md.test.js) ---------------------------------

func TestRenderMarkdownBlobPipeline(t *testing.T) {
	src := "# hello\n\n[evil](javascript:alert(1)) [ok](/rel)\n\n```js\nconst a = 1;\n```\n"
	out := md(src)
	for _, want := range []string{
		"<h1>hello</h1>",
		`<a href="/rel" rel="nofollow ugc noopener">ok</a>`,
		`<pre><code class="language-js">const a = 1;`,
		"evil", // the link text survives; only the dangerous href is dropped
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, `href="javascript:`) {
		t.Fatalf("javascript href survived: %q", out)
	}
}

// --- blocks ----------------------------------------------------------------

func TestRenderMarkdownHeadings(t *testing.T) {
	for lvl := 1; lvl <= 6; lvl++ {
		src := strings.Repeat("#", lvl) + " title"
		want := "<h" + string(rune('0'+lvl)) + ">title</h" + string(rune('0'+lvl)) + ">\n"
		if out := md(src); out != want {
			t.Errorf("%d: got %q want %q", lvl, out, want)
		}
	}
	if out := md("####### seven"); strings.Contains(out, "<h7") || strings.Contains(out, "<h6") {
		t.Errorf("7 hashes must not be a heading: %q", out)
	}
	if out := md("## closed ##"); out != "<h2>closed</h2>\n" {
		t.Errorf("closing hashes: %q", out)
	}
	if out := md("#no space"); strings.Contains(out, "<h1>") {
		t.Errorf("# without a space is not a heading: %q", out)
	}
}

func TestRenderMarkdownSetext(t *testing.T) {
	if out := md("Title\n=====\n"); out != "<h1>Title</h1>\n" {
		t.Fatalf("setext h1 = %q", out)
	}
	if out := md("Sub\n---\n"); out != "<h2>Sub</h2>\n" {
		t.Fatalf("setext h2 = %q", out)
	}
}

func TestRenderMarkdownInline(t *testing.T) {
	tests := []struct{ src, want string }{
		{"**bold**", "<p><strong>bold</strong></p>\n"},
		{"__bold__", "<p><strong>bold</strong></p>\n"},
		{"*em*", "<p><em>em</em></p>\n"},
		{"_em_", "<p><em>em</em></p>\n"},
		{"***both***", "<p><em><strong>both</strong></em></p>\n"},
		{"~~gone~~", "<p><del>gone</del></p>\n"},
		{"`a < b *x*`", "<p><code>a &lt; b *x*</code></p>\n"},
		{"a & b < c", "<p>a &amp; b &lt; c</p>\n"},
		{`\*not em\*`, "<p>*not em*</p>\n"},
		{"snake_case_name", "<p>snake_case_name</p>\n"},
	}
	for _, tc := range tests {
		if out := md(tc.src); out != tc.want {
			t.Errorf("%q: got %q want %q", tc.src, out, tc.want)
		}
	}
}

func TestRenderMarkdownFencedCode(t *testing.T) {
	out := md("```html\n<script>alert(1)</script>\n```")
	if !strings.Contains(out, `<pre><code class="language-html">&lt;script&gt;alert(1)&lt;/script&gt;`) {
		t.Fatalf("fenced code = %q", out)
	}
	out = md("```\nplain\n```")
	if !strings.Contains(out, "<pre><code>plain\n</code></pre>") {
		t.Fatalf("no-language fence = %q", out)
	}
	out = md("~~~go\nfunc f() {}\n~~~")
	if !strings.Contains(out, `<code class="language-go">`) {
		t.Fatalf("tilde fence = %q", out)
	}
	out = md("```go\nfunc f() {}\n")
	if !strings.Contains(out, "</code></pre>") {
		t.Fatalf("unclosed fence must still close = %q", out)
	}
	out = md("```go\"><script>\nx\n```")
	if strings.Contains(out, `class="language-go""><`) || strings.Contains(out, "<script>") {
		t.Fatalf("fence info injection = %q", out)
	}
}

func TestRenderMarkdownUnorderedList(t *testing.T) {
	out := md("- a\n- b\n")
	want := "<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestRenderMarkdownOrderedList(t *testing.T) {
	out := md("1. a\n2. b\n")
	if !strings.HasPrefix(out, "<ol>") || !strings.Contains(out, "<li>a</li>") || !strings.Contains(out, "<li>b</li>") {
		t.Fatalf("ordered list = %q", out)
	}
	out = md("1) a\n")
	if !strings.Contains(out, "<li>a</li>") {
		t.Fatalf("paren marker = %q", out)
	}
}

func TestRenderMarkdownNestedList(t *testing.T) {
	out := md("- a\n  - b\n- c\n")
	if !strings.Contains(out, "<li>a<ul>") {
		t.Fatalf("nested list = %q", out)
	}
	if !strings.Contains(out, "<li>b</li>") || !strings.Contains(out, "<li>c</li>") {
		t.Fatalf("nested items = %q", out)
	}
}

func TestRenderMarkdownTaskList(t *testing.T) {
	out := md("- [ ] todo\n- [x] done\n- [X] also\n")
	if !strings.Contains(out, `<li><input type="checkbox" disabled> todo</li>`) {
		t.Fatalf("unchecked item = %q", out)
	}
	if strings.Count(out, `<input type="checkbox" checked disabled>`) != 2 {
		t.Fatalf("checked items = %q", out)
	}
}

func TestRenderMarkdownBlockquote(t *testing.T) {
	out := md("> quote\n> more\n")
	want := "<blockquote>\n<p>quote<br>\nmore</p>\n</blockquote>\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestRenderMarkdownTable(t *testing.T) {
	out := md("| name | value |\n| --- | ---: |\n| a | <b> |\n| b | 2 |\n")
	for _, want := range []string{
		"<table>", "<thead>", "<th>name</th>", "<th>value</th>",
		"<tbody>", "<td>a</td>", "<td>&lt;b&gt;</td>", "<td>b</td>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if out := md("| a | b |\n|---|---|\n| 1 |\n"); strings.Count(out, "<td>") != 2 {
		t.Fatalf("short row must be padded: %q", out)
	}
}

func TestRenderMarkdownThematicBreak(t *testing.T) {
	for _, src := range []string{"---", "***", "___", "- - -"} {
		if out := md(src); !strings.Contains(out, "<hr>") {
			t.Errorf("%q: no <hr> in %q", src, out)
		}
	}
}

func TestRenderMarkdownLineBreaks(t *testing.T) {
	if out := md("a\nb\n"); out != "<p>a<br>\nb</p>\n" {
		t.Fatalf("breaks = %q", out)
	}
	if out := md("a\n\nb\n"); strings.Count(out, "<p>") != 2 {
		t.Fatalf("paragraphs = %q", out)
	}
}

func TestRenderMarkdownRawHTMLSanitized(t *testing.T) {
	// Raw HTML is allowed through the allowlist sanitizer (WP12): the tag
	// survives, the event handler never does.
	out := md(`<div onclick="alert(1)">hi</div>`)
	if !strings.Contains(out, "<div>hi</div>") {
		t.Fatalf("allowed div not sanitized through: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "onclick") || strings.Contains(out, "alert(1)") {
		t.Fatalf("event handler survived: %q", out)
	}
	if out := md("<script>alert(1)</script>"); strings.Contains(out, "<script>") || strings.Contains(out, "alert(1)") {
		t.Fatalf("script survived: %q", out)
	}
}

// --- links and the security boundary ---------------------------------------

func TestRenderMarkdownLinks(t *testing.T) {
	tests := []struct{ src, want string }{
		{"[a](/rel)", `<a href="/rel" rel="nofollow ugc noopener">a</a>`},
		{"[a](https://x.test/a)", `<a href="https://x.test/a" rel="nofollow ugc noopener" target="_blank">a</a>`},
		{"[a](mailto:a@x.test)", `<a href="mailto:a@x.test" rel="nofollow ugc noopener">a</a>`},
		{"[a](#frag)", `<a href="#frag" rel="nofollow ugc noopener">a</a>`},
		{"[a]()", `<a href="" rel="nofollow ugc noopener">a</a>`},
		{"[a](https://x.test/?a=1&b=2)", `href="https://x.test/?a=1&amp;b=2"`},
		{"[**b**](https://x.test)", `<strong>b</strong>`},
		{`[a](<https://x.test/sp ace>)`, `href="https://x.test/sp ace"`},
		{`[a](x.md "title")`, `href="x.md"`},
	}
	for _, tc := range tests {
		if out := md(tc.src); !strings.Contains(out, tc.want) {
			t.Errorf("%q: missing %q in %q", tc.src, tc.want, out)
		}
	}
}

func TestRenderMarkdownImage(t *testing.T) {
	if out := md("![alt](a.png)"); out != "<p><img src=\"a.png\" alt=\"alt\"></p>\n" {
		t.Fatalf("image = %q", out)
	}
	out := md("![<b>](a.png)")
	if strings.Contains(out, "<b>") || !strings.Contains(out, `alt="&lt;b&gt;"`) {
		t.Fatalf("image alt not escaped: %q", out)
	}
	if out := md("![alt](javascript:alert(1))"); strings.Contains(out, "<img") {
		t.Fatalf("unsafe image kept: %q", out)
	}
}

func TestRenderMarkdownAutolinks(t *testing.T) {
	out := md("see https://x.test/a.")
	if !strings.Contains(out, `<a href="https://x.test/a" rel="nofollow ugc noopener" target="_blank">https://x.test/a</a>.`) {
		t.Fatalf("bare URL = %q", out)
	}
	out = md("<https://x.test/a>")
	if !strings.Contains(out, `<a href="https://x.test/a"`) {
		t.Fatalf("angle autolink = %q", out)
	}
	out = md("<mailto:a@x.test>")
	if !strings.Contains(out, `href="mailto:a@x.test"`) || strings.Contains(out, `<mailto:`) {
		t.Fatalf("mailto autolink = %q", out)
	}
	if out = md("<div>"); !strings.Contains(out, "<div>") {
		t.Fatalf("allowed HTML block not rendered: %q", out)
	}
}

func TestRenderMarkdownDangerousURLs(t *testing.T) {
	payloads := []string{
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		"javascript:alert(1)" + "\x00",
		"data:text/html,hi",
		"DATA:text/html,hi",
		"vbscript:msgbox",
		"java&#115;cript:alert(1)",
		"jav&#x61;script:alert(1)",
		"java&colon;script:alert(1)",
		"//evil.test/x",
		`\\evil.test/x`,
		"ftp://x.test/y",
		"file:///etc/passwd",
		"jar:file:/x",
		"\x01javascript:alert(1)",
	}
	for _, p := range payloads {
		out := md("[x](" + p + ")")
		assertURLsSafe(t, out)
		if strings.Contains(strings.ToLower(out), "evil.test") && strings.Contains(out, "href=") {
			// protocol-relative targets must not survive as an href; the text
			// itself may still be visible.
			for _, u := range urlAttrs(out) {
				if strings.Contains(u, "evil.test") {
					t.Fatalf("%q laundered into href %q", p, u)
				}
			}
		}
		img := md("![i](" + p + ")")
		assertURLsSafe(t, img)
	}
}

func TestRenderMarkdownLinkTextSurvivesDroppedHref(t *testing.T) {
	out := md("[click me](javascript:alert(1))")
	if !strings.Contains(out, "click me") || strings.Contains(out, "href=") {
		t.Fatalf("dropped link = %q", out)
	}
}

// --- #N / PRN refs (Forgejo #340) ------------------------------------------

func TestRenderMarkdownRefsNeedRepoBase(t *testing.T) {
	if out := md("#3 and PR4"); strings.Contains(out, "<a ") {
		t.Fatalf("ctx-less renderer must not link refs: %q", out)
	}
	if out := mdIn("#3", "", "r"); strings.Contains(out, "<a ") {
		t.Fatalf("missing owner must not link refs: %q", out)
	}
}

func TestRenderMarkdownRefs(t *testing.T) {
	out := mdIn("see #3, PR4, pr5 and #003 and (#7).", "o", "r")
	for _, want := range []string{
		`<a href="/o/r/issues/3" rel="nofollow ugc noopener">#3</a>`,
		`<a href="/o/r/pull/4" rel="nofollow ugc noopener">PR4</a>`,
		`<a href="/o/r/pull/5" rel="nofollow ugc noopener">pr5</a>`,
		`<a href="/o/r/issues/3" rel="nofollow ugc noopener">#003</a>`,
		`<a href="/o/r/issues/7" rel="nofollow ugc noopener">#7</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if got := strings.Count(out, "/issues/"); got != 3 {
		t.Fatalf("issues links = %d in %q", got, out)
	}
}

func TestRenderMarkdownRefBoundaries(t *testing.T) {
	out := mdIn("x#3 #3x #3.2 xPR3 PR3abc #x3 #-3", "o", "r")
	if strings.Contains(out, "<a ") {
		t.Fatalf("boundary case linked: %q", out)
	}
	out = mdIn("### heading\n#3", "o", "r")
	if !strings.HasPrefix(out, "<h3>heading</h3>") || strings.Count(out, "/issues/") != 1 {
		t.Fatalf("heading/ref = %q", out)
	}
	out = mdIn("`#3` and `PR4`", "o", "r")
	if strings.Contains(out, "<a ") {
		t.Fatalf("code spans must be exempt: %q", out)
	}
	out = mdIn("[#3](https://x.test)", "o", "r")
	if strings.Count(out, "<a ") != 1 {
		t.Fatalf("existing link must be exempt: %q", out)
	}
	out = mdIn("&#39; #3", "o", "r")
	if strings.Count(out, "/issues/") != 1 {
		t.Fatalf("entity-adjacent ref = %q", out)
	}
}

// --- @mentions (Forgejo #440) ----------------------------------------------

func TestRenderMarkdownMentions(t *testing.T) {
	out := md("hi @bob.")
	if !strings.Contains(out, `hi @<a href="/bob" rel="nofollow ugc noopener">bob</a>.`) {
		t.Fatalf("mention = %q", out)
	}
	out = md("@Carol")
	if !strings.Contains(out, `href="/carol"`) || !strings.Contains(out, `>Carol</a>`) {
		t.Fatalf("case-preserving mention = %q", out)
	}
	out = md("@bob@example.com")
	if !strings.Contains(out, `href="/bob@example.com"`) || !strings.Contains(out, `>bob@example.com</a>`) {
		t.Fatalf("email principal = %q", out)
	}
}

func TestRenderMarkdownMentionExemptions(t *testing.T) {
	for _, src := range []string{
		"a@b.com", "@@x", "@org/team", "@bob@x", "`@bob`", "[@bob](https://x.test)", "@..", "x@y",
	} {
		out := md(src)
		if src == "[@bob](https://x.test)" {
			if strings.Count(out, "<a ") != 1 {
				t.Errorf("%q: label linkified: %q", src, out)
			}
			continue
		}
		if strings.Contains(out, "<a ") {
			t.Errorf("%q: linked unexpectedly: %q", src, out)
		}
	}
}

// --- misc ------------------------------------------------------------------

func TestRenderMarkdownEmpty(t *testing.T) {
	if out := md(""); out != "" {
		t.Fatalf("empty = %q", out)
	}
	if out := md("\n\n \t\n"); out != "" {
		t.Fatalf("blank = %q", out)
	}
}

func TestRenderMarkdownCRLF(t *testing.T) {
	if out := md("# a\r\n\r\nb\r\n"); out != "<h1>a</h1>\n<p>b</p>\n" {
		t.Fatalf("crlf = %q", out)
	}
}

func TestRenderMarkdownDeepNesting(t *testing.T) {
	out := md("- a\n  - b\n    - c\n")
	if !strings.Contains(out, "<li>a<ul>") || !strings.Contains(out, "<li>b<ul>") || !strings.Contains(out, "<li>c</li>") {
		t.Fatalf("deep nesting = %q", out)
	}
}

// --- parser edge cases ------------------------------------------------------

func TestRenderMarkdownListEdges(t *testing.T) {
	// A blank line between items keeps one list.
	if out := md("- a\n\n- b\n"); strings.Count(out, "<li>") != 2 || strings.Count(out, "<ul>") != 1 {
		t.Fatalf("loose list = %q", out)
	}
	// A continuation line indented to the content column is a hard break.
	if out := md("- a\n  b\n"); !strings.Contains(out, "<li>a<br>\nb</li>") {
		t.Fatalf("continuation = %q", out)
	}
	// The forgiving indent path: content starts at column 4, continuation 2.
	if out := md("*   a\n  b\n"); !strings.Contains(out, "<li>a<br>\nb</li>") {
		t.Fatalf("forgiving indent = %q", out)
	}
	// A different bullet still belongs to the same list.
	if out := md("- a\n* b\n"); strings.Count(out, "<ul>") != 1 || strings.Count(out, "<li>") != 2 {
		t.Fatalf("mixed bullets = %q", out)
	}
	// A block at column 0 ends the list.
	if out := md("- a\n# h\n"); !strings.Contains(out, "<li>a</li>") || !strings.Contains(out, "<h1>h</h1>") {
		t.Fatalf("list then heading = %q", out)
	}
	// A 4-space nested list (2-space dedent leaves a 2-space marker).
	if out := md("- a\n    - b\n"); !strings.Contains(out, "<li>a<ul>") || !strings.Contains(out, "<li>b</li>") {
		t.Fatalf("4-space nesting = %q", out)
	}
	if out := md("    - a\n"); strings.Contains(out, "<ul>") {
		t.Fatalf("4-space marker must be a paragraph: %q", out)
	}
	if out := md("-a\n"); strings.Contains(out, "<ul>") {
		t.Fatalf("no space after marker: %q", out)
	}
	if out := md("1234567890. a\n"); strings.Contains(out, "<ol>") {
		t.Fatalf("10-digit marker is not a list: %q", out)
	}
	if out := md("1.a\n"); strings.Contains(out, "<ol>") {
		t.Fatalf("ordered marker needs a space: %q", out)
	}
	if out := md("*\tfoo\n"); !strings.Contains(out, "<li>foo</li>") {
		t.Fatalf("tab after marker = %q", out)
	}
}

func TestRenderMarkdownBlockquoteEdges(t *testing.T) {
	if out := md("> a\nlazy\n"); !strings.Contains(out, "<p>a<br>\nlazy</p>") {
		t.Fatalf("lazy continuation = %q", out)
	}
	if out := md("> a\n\n> b\n"); strings.Count(out, "<p>") != 2 {
		t.Fatalf("blank inside quote = %q", out)
	}
	if out := md("> # h\n"); !strings.Contains(out, "<blockquote>\n<h1>h</h1>") {
		t.Fatalf("heading in quote = %q", out)
	}
	if out := md("> a\n# h\n"); !strings.Contains(out, "</blockquote>") || !strings.Contains(out, "<h1>h</h1>") {
		t.Fatalf("block start ends quote = %q", out)
	}
}

func TestRenderMarkdownFenceEdges(t *testing.T) {
	if out := md("    ```\ncode\n    ```\n"); strings.Contains(out, "<pre>") {
		t.Fatalf("4-space fence is not a fence: %q", out)
	}
	if out := md("``\ncode\n``\n"); strings.Contains(out, "<pre>") {
		t.Fatalf("two backticks is not a fence: %q", out)
	}
	if out := md("```js `x`\ncode\n"); strings.Contains(out, "<pre>") {
		t.Fatalf("backtick in info string is not a fence: %q", out)
	}
	if out := md("```js {1,2}\nx\n```\n"); !strings.Contains(out, `class="language-js"`) {
		t.Fatalf("info string brace = %q", out)
	}
	if out := md("```js\"><script>\nx\n```\n"); strings.Contains(out, "<script>") || strings.Contains(out, `js"`) {
		t.Fatalf("info injection = %q", out)
	}
	if out := md("```\n```\n"); out != "<pre><code></code></pre>\n" {
		t.Fatalf("empty fence = %q", out)
	}
	if got := fenceLanguage(""); got != "" {
		t.Fatalf("empty language = %q", got)
	}
}

func TestRenderMarkdownHeadingEdges(t *testing.T) {
	if out := md("    # h\n"); strings.Contains(out, "<h1>") {
		t.Fatalf("4-space heading = %q", out)
	}
	if out := md("#####\n"); out != "<h5></h5>\n" {
		t.Fatalf("bare heading = %q", out)
	}
	if out := md("## ##\n"); out != "<h2></h2>\n" {
		t.Fatalf("closing-only heading = %q", out)
	}
	if out := md("   # h\n"); out != "<h1>h</h1>\n" {
		t.Fatalf("3-space heading = %q", out)
	}
	if out := md("a\n=x\n"); !strings.Contains(out, "a<br>") {
		t.Fatalf("invalid setext = %q", out)
	}
	if out := md("---x\n"); strings.Contains(out, "<hr>") {
		t.Fatalf("not a thematic break: %q", out)
	}
	if out := md("***bold***\n"); !strings.Contains(out, "<em><strong>bold</strong></em>") {
		t.Fatalf("triple emphasis = %q", out)
	}
}

func TestRenderMarkdownTableEdges(t *testing.T) {
	if out := md("a | b\nnope | b\n"); strings.Contains(out, "<table>") {
		t.Fatalf("invalid delimiter row: %q", out)
	}
	if out := md("no pipe\n---\n"); strings.Contains(out, "<table>") {
		t.Fatalf("header without a pipe: %q", out)
	}
	if out := md("| a | b |\n|---|---|\n| 1 | 2 | 3 |\n"); strings.Count(out, "<td>") != 2 {
		t.Fatalf("extra cell truncated: %q", out)
	}
	if out := md("| a\\|b | c |\n|---|---|\n| 1 | 2 |\n"); !strings.Contains(out, "<th>a|b</th>") {
		t.Fatalf("escaped pipe = %q", out)
	}
	if out := md("| a |\n| - |\n"); !strings.Contains(out, "<table>") {
		t.Fatalf("single column table = %q", out)
	}
	if out := md("| a | b |\n| -- | -- |\n"); !strings.Contains(out, "<table>") {
		t.Fatalf("short delimiter = %q", out)
	}
	if out := md("| a | b |\n| :x | -- |\n"); strings.Contains(out, "<table>") {
		t.Fatalf("invalid delimiter cell: %q", out)
	}
}

func TestRenderMarkdownInlineEdges(t *testing.T) {
	tests := []struct{ src, want string }{
		{`\a`, "<p>\\a</p>\n"},
		{"a\\", "<p>a\\</p>\n"},
		{"`abc", "<p>`abc</p>\n"},
		{"hi! there", "<p>hi! there</p>\n"},
		{"[not a link]", "<p>[not a link]</p>\n"},
		{"[x](no close", "<p>[x](no close</p>\n"},
		{"[x](<y)", "<p>[x](&lt;y)</p>\n"},
		{"[x](y \"unclosed)", "<p>[x](y \"unclosed)</p>\n"},
		{"~~unclosed", "<p>~~unclosed</p>\n"},
		{"~x", "<p>~x</p>\n"},
		{"_ a", "<p>_ a</p>\n"},
		{"a_b_c", "<p>a_b_c</p>\n"},
		{"<not a url>", "<p>&lt;not a url&gt;</p>\n"},
		{"https://x.test/a(b)", `href="https://x.test/a(b)"`},
		{"(see https://x.test/a)", `href="https://x.test/a"`},
		{"HTTP://x.test", "<p>HTTP://x.test</p>\n"},
		{"http://", "<p>http://</p>\n"},
	}
	for _, tc := range tests {
		out := md(tc.src)
		if strings.HasPrefix(tc.want, "href=") {
			if !strings.Contains(out, tc.want) {
				t.Errorf("%q: missing %q in %q", tc.src, tc.want, out)
			}
			continue
		}
		if out != tc.want {
			t.Errorf("%q: got %q want %q", tc.src, out, tc.want)
		}
	}
}

func TestRenderMarkdownMentionTrailingPunctuation(t *testing.T) {
	out := md("(@bob), and @bob!")
	if strings.Count(out, `<a href="/bob"`) != 2 || !strings.Contains(out, "), and") || !strings.Contains(out, "</a>!") {
		t.Fatalf("punctuation = %q", out)
	}
}

func TestRenderMarkdownWriteTextEmpty(t *testing.T) {
	var b strings.Builder
	r := &mdRenderer{refBase: "/o/r"}
	r.writeText(&b, "", true)
	r.writeText(&b, "<x>", false)
	if b.String() != "&lt;x&gt;" {
		t.Fatalf("writeText = %q", b.String())
	}
}

func TestMatchRefEdges(t *testing.T) {
	for _, in := range []string{"#", "PR", "x#3", "#3x", "#3.2", "#-1", "pRx"} {
		if _, ok := matchRef(in, 0); ok {
			t.Errorf("matchRef(%q) matched", in)
		}
	}
	m, ok := matchRef("PR007", 0)
	if !ok || m.path != "/pull/7" || m.text != "PR007" {
		t.Errorf("matchRef(PR007) = %+v %v", m, ok)
	}
	if _, ok := matchRef("a#3", 1); ok {
		t.Error("word-adjacent ref matched")
	}
}

func TestMatchMentionEdges(t *testing.T) {
	for _, in := range []string{"@", "@.", "a@b", "@@x", "@" + strings.Repeat("a", 65)} {
		if _, ok := matchMention(in, 0); ok {
			t.Errorf("matchMention(%q) matched", in)
		}
	}
	if _, ok := matchMention("@bob@x", 0); ok {
		t.Error("broken email matched")
	}
	m, ok := matchMention("@Bob", 0)
	if !ok || m.principal != "Bob" || m.href != "bob" {
		t.Errorf("matchMention(@Bob) = %+v %v", m, ok)
	}
}

func TestDecodeEntitiesEdges(t *testing.T) {
	if got := decodeEntities("&amp;&#65;&#x41;&colon;"); got != "&AA:" {
		t.Fatalf("decode = %q", got)
	}
	for _, in := range []string{"&unknown;", "&#xZZ;", "&#99999999;", "plain"} {
		if got := decodeEntities(in); got != in {
			t.Errorf("decodeEntities(%q) = %q", in, got)
		}
	}
}

func TestSanitizeURLEdges(t *testing.T) {
	ok := []string{"", "  ", "\x01", "#f", "?q=1", "/p", "p", "HTTP://x", "mailto:a@b", "https://x/?a=1&b=2"}
	for _, in := range ok {
		if _, allowed := sanitizeURL(in, urlForLink); !allowed {
			t.Errorf("sanitizeURL(%q) rejected", in)
		}
	}
	bad := []string{"//x", `\x`, `\\x`, "javascript:x", "JAVASCRIPT:x", "data:x", "vbscript:x", "file:x", "ftp:x", "java&#115;cript:x"}
	for _, in := range bad {
		if _, allowed := sanitizeURL(in, urlForLink); allowed {
			t.Errorf("sanitizeURL(%q) allowed", in)
		}
	}
	if _, allowed := sanitizeURL("mailto:a@b", urlForImage); allowed {
		t.Error("mailto image src allowed")
	}
}

func TestFindCloseRunEdges(t *testing.T) {
	if got := findCloseRun("abc", 0, '*', 1); got != -1 {
		t.Fatalf("no close = %d", got)
	}
	if got := findCloseRun("a**b*c", 0, '*', 1); got != 4 {
		t.Fatalf("single-star close = %d", got)
	}
	if got := findCloseRun("a****b", 0, '*', 2); got != -1 {
		t.Fatalf("run mismatch = %d", got)
	}
}

func TestWriteParagraphNoContent(t *testing.T) {
	var b strings.Builder
	r := &mdRenderer{}
	if next := r.writeParagraph(&b, []string{""}, 0, false); next != 1 || b.String() != "" {
		t.Fatalf("block-start paragraph = %q next=%d", b.String(), next)
	}
}
