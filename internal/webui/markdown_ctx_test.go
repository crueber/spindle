package webui

import (
	"os"
	"strings"
	"testing"
)

func mdCtx(src string, ctx MarkdownCtx) string { return string(RenderMarkdownCtx(src, ctx)) }

// --- relative URL resolution (WP12 deliverable A) ---------------------------

func TestRenderMarkdownCtxRelativeURLs(t *testing.T) {
	t.Parallel()
	ctx := MarkdownCtx{Owner: "acme", Repo: "loom", Ref: "main", Dir: "docs/nested"}
	cases := []struct{ src, want string }{
		// images always go to the raw-byte endpoint at the same ref
		{"![d](demo.webp)", `src="/acme/loom/api/blob/main/docs/nested/demo.webp?raw=1"`},
		{"![d](/img/a.png)", `src="/acme/loom/api/blob/main/img/a.png?raw=1"`},
		{"![d](../a.png)", `src="/acme/loom/api/blob/main/docs/a.png?raw=1"`},
		{"![d](../../../../x.png)", `src="/acme/loom/api/blob/main/x.png?raw=1"`}, // clamped at the root
		{"![d](./a.png)", `src="/acme/loom/api/blob/main/docs/nested/a.png?raw=1"`},
		// markdown files open the in-app blob page; other links the raw endpoint
		{"[g](guide.md)", `href="/acme/loom/blob/main/docs/nested/guide.md"`},
		{"[g](GUIDE.MARKDOWN)", `href="/acme/loom/blob/main/docs/nested/GUIDE.MARKDOWN"`},
		{"[s](src/main.go)", `href="/acme/loom/api/blob/main/docs/nested/src/main.go?raw=1"`},
		{"[u](../README.md)", `href="/acme/loom/blob/main/docs/README.md"`},
		// query and fragment are preserved verbatim
		{"[q](a.md?x=1#frag)", `href="/acme/loom/blob/main/docs/nested/a.md?x=1#frag"`},
		{"![q](a.png?x=1#f)", `src="/acme/loom/api/blob/main/docs/nested/a.png?raw=1&amp;x=1#f"`},
		{"[f](a.go#L10)", `href="/acme/loom/api/blob/main/docs/nested/a.go?raw=1#L10"`},
		// percent escapes are preserved, never decoded or double-encoded
		{"![e](my%20file.png)", `src="/acme/loom/api/blob/main/docs/nested/my%20file.png?raw=1"`},
		{"![e](%2e%2e/x.png)", `src="/acme/loom/api/blob/main/docs/nested/%2e%2e/x.png?raw=1"`},
		{"![e](<my file.png>)", `src="/acme/loom/api/blob/main/docs/nested/my%20file.png?raw=1"`},
		// absolute, fragment and scheme URLs are not repo paths
		{"[x](https://e.test/a?b=1#c)", `href="https://e.test/a?b=1#c"`},
		{"[x](mailto:a@e.test)", `href="mailto:a@e.test"`},
		{"[x](#frag)", `href="#frag"`},
	}
	for _, tc := range cases {
		if out := mdCtx(tc.src, ctx); !strings.Contains(out, tc.want) {
			t.Errorf("%q: missing %q in %q", tc.src, tc.want, out)
		}
	}
	// no context: untouched passthrough (the RenderMarkdown/RenderMarkdownIn contract)
	if out := md("![d](docs/demo.webp)"); !strings.Contains(out, `src="docs/demo.webp"`) {
		t.Fatalf("ctx-less image was rewritten: %q", out)
	}
	if out := md("[g](docs/guide.md)"); !strings.Contains(out, `href="docs/guide.md"`) {
		t.Fatalf("ctx-less link was rewritten: %q", out)
	}
	// empty ref disables resolution even with owner/repo
	if out := mdIn("![d](a.png)", "acme", "loom"); !strings.Contains(out, `src="a.png"`) {
		t.Fatalf("incomplete ctx was not a passthrough: %q", out)
	}
	// the resolver can only ever emit http(s)/mail/relative URLs
	assertURLsSafe(t, mdCtx("[x](docs/a.md) ![i](docs/a.png)", ctx))
}

func TestRenderMarkdownCtxSlashedRef(t *testing.T) {
	t.Parallel()
	ctx := MarkdownCtx{Owner: "acme", Repo: "loom", Ref: "feat/readme", Dir: ""}
	out := mdCtx("[g](guide.md) ![i](a.png)", ctx)
	for _, want := range []string{
		`href="/acme/loom/blob/feat/readme/guide.md"`,
		`src="/acme/loom/api/blob/feat/readme/a.png?raw=1"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

// A full ref name (refs/heads/main) is shortened before it enters a URL, so
// a slash in "refs/heads" can never split the route.
func TestRenderMarkdownCtxShortensFullRef(t *testing.T) {
	t.Parallel()
	ctx := MarkdownCtx{Owner: "acme", Repo: "loom", Ref: "refs/heads/main"}
	if out := mdCtx("[g](a.md)", ctx); !strings.Contains(out, `href="/acme/loom/blob/main/a.md"`) {
		t.Fatalf("full ref not shortened: %q", out)
	}
}

// --- raw-HTML sanitizer corpus (WP12 deliverable B) -------------------------

func TestRenderMarkdownSanitizerNeuters(t *testing.T) {
	t.Parallel()
	payloads := []struct{ name, src string }{
		{"script", `<script>alert(1)</script>`},
		{"script attr", `<div><script src="https://evil.test/x.js"></script>hi</div>`},
		{"img onerror", `<img src="x.png" onerror="alert(1)">`},
		{"img onload", `<img src=x.png onload=alert(1)>`},
		{"javascript href", `<a href="javascript:alert(1)">click</a>`},
		{"entity javascript", `<a href="jav&#x61;script:alert(1)">click</a>`},
		{"control javascript", "<a href=\"java\x00script:alert(1)\">click</a>"},
		{"protocol relative", `<a href="//evil.test/x">click</a>`},
		{"iframe", `<iframe src="https://evil.test">fallback</iframe>`},
		{"style at import", `<style>@import url(//evil.test/x.css);</style>`},
		{"svg onload", `<svg onload="alert(1)"><circle r="4"/></svg>`},
		{"form", `<form action="https://evil.test"><input name="a"><button>go</button></form>`},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=https://evil.test">`},
		{"style attribute", `<div style="background:url(javascript:alert(1))">x</div>`},
		{"class attribute", `<div class="x" id="y">z</div>`},
		{"data text html", `<a href="data:text/html,<script>alert(1)</script>">x</a>`},
		{"data text img", `<img src="data:text/html;base64,PHNjcmlwdD4=">`},
		{"srcdoc", `<iframe srcdoc="<script>alert(1)</script>"></iframe>`},
		{"srcset", `<img src="x.png" srcset="javascript:alert(1) 1x">`},
		{"target", `<a href="/x" target="_top">x</a>`},
		{"template", `<template><img src=x onerror=alert(1)></template>`},
		{"math", `<math><mtext><script>alert(1)</script></mtext></math>`},
		{"tab obfuscation", "<a href=\"jav\tascript:alert(1)\">x</a>"},
	}
	for _, tc := range payloads {
		out := strings.ToLower(md(tc.src))
		for _, bad := range []string{"<script", "<style", "<iframe", "<svg", "<form", "<input", "<button", "<meta", "onerror", "onload", "javascript:", "vbscript:", "srcdoc", "srcset", "<template", "<math"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %q survived in %q", tc.name, bad, out)
			}
		}
		assertURLsSafe(t, md(tc.src))
	}
	// <a href="//evil"> keeps its text but loses the href
	if out := md(`<a href="//evil.test/x">click</a>`); strings.Contains(out, `href=`) || !strings.Contains(out, "click") {
		t.Fatalf("protocol-relative link = %q", out)
	}
	// data:image is the one allowed data URI, and only on img
	if out := md(`<img src="data:image/png;base64,iVBORw0KGgo=" alt="x">`); !strings.Contains(out, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("data image dropped: %q", out)
	}
}

func TestRenderMarkdownSanitizerAllows(t *testing.T) {
	t.Parallel()
	src := strings.Join([]string{
		`<div align="center">`,
		`<p align="center">centered</p>`,
		`</div>`,
		`<details open><summary>More</summary>`,
		`<p>body</p>`,
		`</details>`,
		`<table><thead><tr><th colspan="2">h</th></tr></thead><tbody><tr><td rowspan="2" width="50%">a</td><td>b</td></tr></tbody></table>`,
		`<video controls loop muted playsinline poster="p.png" width="320"><source src="v.mp4" type="video/mp4">fallback</video>`,
		`<picture><source src="a.webp" type="image/webp"><img src="a.png" alt="a" width="10" height="20"></picture>`,
		`<figure><img src="f.png"><figcaption>cap</figcaption></figure>`,
		`<kbd>Ctrl</kbd> <sub>2</sub> <sup>3</sup> <mark>m</mark> <del>d</del> <ins>i</ins> <abbr title="t">ab</abbr>`,
		`<ul><li>one</li><li>two</li></ul>`,
		`<h1>t</h1>`,
	}, "\n")
	out := md(src)
	for _, want := range []string{
		`<div align="center">`, `<p align="center">centered</p>`,
		`<details open>`, `<summary>More</summary>`,
		`<th colspan="2">`, `<td rowspan="2" width="50%">`,
		`<video controls loop muted playsinline poster="p.png" width="320">`,
		`<source src="v.mp4" type="video/mp4">`, `<picture>`,
		`<img src="f.png">`, `<figcaption>cap</figcaption>`,
		`<kbd>Ctrl</kbd>`, `<sub>2</sub>`, `<sup>3</sup>`, `<mark>m</mark>`,
		`<abbr title="t">ab</abbr>`, `<h1>t</h1>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// nested details survives (the sanitizer keeps structure, not a depth limit)
	nested := md("<details><details><summary>x</summary>y</details></details>")
	if strings.Count(nested, "<details>") != 2 || strings.Count(nested, "</details>") != 2 {
		t.Fatalf("nested details = %q", nested)
	}
	// align only on block elements / img, and only left|center|right
	bad := md(`<span align="center">x</span>`)
	if strings.Contains(bad, "align=") {
		t.Fatalf("align on span survived: %q", bad)
	}
	if out := md(`<div align="diagonal">x</div>`); strings.Contains(out, "align=") {
		t.Fatalf("bad align survived: %q", out)
	}
}

// Raw HTML resolves relative URLs with the same rules as markdown syntax.
func TestRenderMarkdownSanitizerResolvesRelativeURLs(t *testing.T) {
	t.Parallel()
	ctx := MarkdownCtx{Owner: "acme", Repo: "loom", Ref: "main", Dir: "docs"}
	out := mdCtx(`<p><img src="a.png"><a href="guide.md">g</a><a href="https://e.test/x">e</a></p>`, ctx)
	for _, want := range []string{
		`src="/acme/loom/api/blob/main/docs/a.png?raw=1"`,
		`href="/acme/loom/blob/main/docs/guide.md" rel="nofollow ugc noopener"`,
		`href="https://e.test/x" rel="nofollow ugc noopener" target="_blank"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

// Inline raw HTML inside a paragraph is sanitized, not escaped, and normal
// text stays escaped.
func TestRenderMarkdownInlineRawHTML(t *testing.T) {
	t.Parallel()
	out := md("press <kbd>Ctrl</kbd> + <kbd>C</kbd> now")
	if !strings.Contains(out, "<kbd>Ctrl</kbd>") || !strings.Contains(out, "<kbd>C</kbd>") {
		t.Fatalf("inline kbd = %q", out)
	}
	if strings.Contains(out, "&lt;kbd&gt;") {
		t.Fatalf("inline kbd was escaped: %q", out)
	}
	if out := md("a < b & c"); out != "<p>a &lt; b &amp; c</p>\n" {
		t.Fatalf("text escaping regressed: %q", out)
	}
	if out := md(`<span onclick="x">bad</span>`); strings.Contains(out, "onclick") || !strings.Contains(out, "<span>bad</span>") {
		t.Fatalf("inline span = %q", out)
	}
	if out := md("<b>unclosed"); !strings.Contains(out, "&lt;b&gt;unclosed") {
		t.Fatalf("unbalanced inline tag leaked: %q", out)
	}
	if out := md("a <!-- comment --> b"); !strings.Contains(out, "a  b") {
		t.Fatalf("comment not dropped: %q", out)
	}
}

// --- the golden fixture: a trimmed copy of the real loom README -------------

func TestRenderMarkdownLoomReadme(t *testing.T) {
	t.Parallel()
	src := readTestdata(t, "loom-readme.md")
	ctx := MarkdownCtx{Owner: "acme", Repo: "loom", Ref: "main", Dir: ""}
	out := mdCtx(src, ctx)

	for _, want := range []string{
		`<div align="center">`, // centered header block survives
		"<h2>Loom</h2>",        // markdown inside the div still renders (h1 demoted)
		`</div>`,
		"<details>",
		"<summary><strong>🐳 Docker (recommended)</strong></summary>",
		"</details>",
		`<img src="/acme/loom/api/blob/main/docs/demo.webp?raw=1"`,
		`<a href="/acme/loom/api/blob/main/docs/demo.webm?raw=1" rel="nofollow ugc noopener">`,
		`<a href="/acme/loom/blob/main/docs/guide.md" rel="nofollow ugc noopener">the guide</a>`,
		`href="/acme/loom/api/blob/main/src/main.go?raw=1#L10"`,
		"<hr>",
		"language-sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
	if strings.Contains(out, "&lt;div") || strings.Contains(out, "&lt;details") || strings.Contains(out, "&lt;summary") {
		t.Fatalf("raw HTML was escaped:\n%s", out)
	}
	assertURLsSafe(t, out)
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(b)
}
