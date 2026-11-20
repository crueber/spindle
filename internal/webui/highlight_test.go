package webui

import (
	"strings"
	"testing"
)

func TestLanguageFor(t *testing.T) {
	cases := map[string]string{
		"main.go":       "go",
		"a.PY":          "python",
		"x.tsx":         "js",
		"x.mjs":         "js",
		"x.cjs":         "js",
		"x.ts":          "js",
		"x.rb":          "ruby",
		"x.rs":          "rust",
		"x.bash":        "shell",
		"x.zsh":         "shell",
		"x.yml":         "yaml",
		"x.ini":         "toml",
		"x.conf":        "toml",
		"x.hpp":         "c",
		"x.hxx":         "c",
		"x.cs":          "c",
		"x.kt":          "c",
		"x.swift":       "c",
		"x.svg":         "html",
		"x.xml":         "html",
		"x.sql":         "sql",
		"Makefile":      "",
		"x.unknownext":  "",
		"a.b-c":         "",
		".hidden":       "",
		"trailing.":     "",
		"dir.v2/x.go":   "go",
		"dir/go/x":      "",
		"file.tar.gz":   "",
		"UPPER.JSON":    "json",
		"spaces ok.txt": "",
	}
	for name, want := range cases {
		if got := LanguageFor(name); got != want {
			t.Errorf("LanguageFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestHighlightUnknownLanguageRendersPlain(t *testing.T) {
	// css/html have a language name in the table but no tokenizer (the JS
	// RULES list gates them out), and unknown extensions have none at all.
	for _, path := range []string{"a.css", "a.html", "a.txt", "Makefile"} {
		got := string(Highlight(path, `<b class="x">& 'q'</b>`))
		want := `&lt;b class="x"&gt;&amp; 'q'&lt;/b&gt;`
		if got != want {
			t.Errorf("%s: got %q want %q", path, got, want)
		}
	}
}

func TestHighlightTokens(t *testing.T) {
	tests := []struct {
		name, path, code string
		want             []string
		notWant          []string
	}{
		{
			name: "go keywords strings comments",
			path: "main.go",
			code: "func main() {\n\tx := \"hi\" // note\n\treturn 42\n}",
			want: []string{
				`<span class="tok-kw">func</span>`,
				`<span class="tok-str">"hi"</span>`,
				`<span class="tok-com">// note</span>`,
				`<span class="tok-num">42</span>`,
			},
		},
		{
			name: "js backtick template literal is a string",
			path: "a.js",
			code: "const s = `a ${b} c`;",
			want: []string{`<span class="tok-kw">const</span>`, "<span class=\"tok-str\">`a ${b} c`</span>"},
		},
		{
			name: "python hash comment, quoted string, no slash comment",
			path: "a.py",
			code: "def f():\n    # hi\n    return 'x' // y",
			want: []string{
				`<span class="tok-kw">def</span>`,
				`<span class="tok-com"># hi</span>`,
				`<span class="tok-str">'x'</span>`,
			},
			notWant: []string{`<span class="tok-com">// y</span>`},
		},
		{
			name: "sql double dash comment",
			path: "a.sql",
			code: "select 1 -- note\nfrom t",
			want: []string{`<span class="tok-kw">select</span>`, `<span class="tok-com">-- note</span>`, `<span class="tok-num">1</span>`},
		},
		{
			name: "json only double-quoted strings",
			path: "a.json",
			code: `{"a": true} 'b'`,
			want: []string{`<span class="tok-str">"a"</span>`, `<span class="tok-kw">true</span>`},
		},
		{
			name: "keyword inside a string stays a string",
			path: "a.go",
			code: `"func return"`,
			want: []string{`<span class="tok-str">"func return"</span>`},
			notWant: []string{
				`<span class="tok-kw">func</span>`,
			},
		},
		{
			name: "block comment spans newlines as one span",
			path: "a.go",
			code: "/* one\ntwo */",
			want: []string{"<span class=\"tok-com\">/* one\ntwo */</span>"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Highlight(tc.path, tc.code))
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("unexpected %q in %q", bad, got)
				}
			}
		})
	}
}

func TestHighlightEscapesSource(t *testing.T) {
	got := string(Highlight("a.go", "if a < b && c > d { x() }"))
	for _, bad := range []string{"a < b", "&& c >"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unescaped source %q in %q", bad, got)
		}
	}
	for _, want := range []string{"a &lt; b", "&amp;&amp; c &gt;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing escaped %q in %q", want, got)
		}
	}
}

func TestHighlightLanguageExplicitCore(t *testing.T) {
	if got := string(highlightCode("x = 1", "")); got != "x = 1" {
		t.Fatalf("empty language = %q", got)
	}
	if got := string(highlightCode("<", "go")); got != "&lt;" {
		t.Fatalf("escaped plain = %q", got)
	}
}

func TestHighlightNumberForms(t *testing.T) {
	got := string(Highlight("a.js", "42 0x1f 3.14 x2"))
	for _, want := range []string{
		`<span class="tok-num">42</span>`,
		`<span class="tok-num">0x1f</span>`,
		`<span class="tok-num">3.14</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// "x2" is an identifier, not a number (no digit at the start).
	if strings.Contains(got, `<span class="tok-num">x2</span>`) {
		t.Fatalf("identifier highlighted as number: %q", got)
	}
}
