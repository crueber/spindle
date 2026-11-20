package webui

import (
	"html/template"
	"regexp"
	"strings"
)

// highlightLangForExt is the filename-extension → language table from
// web/src/lib/highlight.js. Unknown extensions render plain (escaped) text.
var highlightLangForExt = map[string]string{
	"js": "js", "mjs": "js", "cjs": "js", "jsx": "js", "ts": "js", "tsx": "js",
	"go": "go", "py": "python", "rb": "ruby", "rs": "rust", "sh": "shell", "bash": "shell", "zsh": "shell",
	"json": "json", "yml": "yaml", "yaml": "yaml", "toml": "toml", "ini": "toml", "conf": "toml",
	"c": "c", "h": "c", "cpp": "c", "cc": "c", "hpp": "c", "hxx": "c", "cs": "c", "java": "c", "kt": "c", "swift": "c",
	"css": "css", "html": "html", "xml": "html", "svg": "html", "sql": "sql",
}

// LanguageFor returns the tokenizer language for a filename ("main.go" →
// "go"), or "" when the extension is unknown (render plain text).
func LanguageFor(filename string) string {
	if k := strings.LastIndexByte(filename, '.'); k >= 0 {
		ext := filename[k+1:]
		if ext != "" && isASCIIAlnum(ext) {
			return highlightLangForExt[strings.ToLower(ext)]
		}
	}
	return ""
}

// Highlight renders code with token spans (all text escaped): keywords,
// strings, comments and numbers, chosen by the file path's extension. Ports
// highlight.js exactly — the same language table, token rules and order, the
// same class names (tok-kw/tok-str/tok-com/tok-num). Unknown extensions
// render escaped plain text.
func Highlight(path, code string) template.HTML {
	return highlightCode(code, LanguageFor(path))
}

// highlightCode tokenizes code as lang. Exported behaviour goes through
// Highlight; this is the language-explicit core the tests pin.
func highlightCode(code, lang string) template.HTML {
	rule, ok := highlightRules[lang]
	if !ok {
		return template.HTML(escapeText(code))
	}
	matches := rule.rx.FindAllStringIndex(code, -1)
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(escapeText(code[last:m[0]]))
		last = m[1]
		text := code[m[0]:m[1]]
		class := ""
		switch {
		case text[0] >= '0' && text[0] <= '9':
			class = "tok-num"
		case text[0] == '"' || text[0] == '\'' || text[0] == '`':
			class = "tok-str"
		case strings.HasPrefix(text, "//") || strings.HasPrefix(text, "#") ||
			strings.HasPrefix(text, "--") || strings.HasPrefix(text, "/*"):
			class = "tok-com"
		default:
			if rule.kw[text] {
				class = "tok-kw"
			}
		}
		if class == "" {
			b.WriteString(escapeText(text))
			continue
		}
		b.WriteString(`<span class="` + class + `">` + escapeText(text) + `</span>`)
	}
	b.WriteString(escapeText(code[last:]))
	return template.HTML(b.String())
}

type highlightRule struct {
	rx *regexp.Regexp
	kw map[string]bool
}

// Rules are precompiled once: these run per page render, never per line.
var highlightRules = buildHighlightRules()

var highlightKeywords = map[string]string{
	"js":     "abstract async await break case catch class const continue debugger default delete do else enum export extends false finally for function get if implements import in instanceof interface let new null of package private protected public return set static super switch this throw true try typeof undefined var void while with yield",
	"go":     "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false make new",
	"python": "and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False",
	"rust":   "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
	"ruby":   "begin break case class def do else elsif end ensure for if module next nil redo rescue retry return self super then undef unless until when while yield true false",
	"c":      "auto break case char class const continue default delete do double else enum extern false float for goto if inline int interface long namespace new NULL override private protected public restrict return short signed sizeof static struct switch template this true typedef union unsigned using virtual void volatile while",
	"shell":  "break case continue do done elif else esac fi for function if in return select then until while local export readonly shift unset set echo cd",
	"json":   "true false null",
	"yaml":   "true false null yes no on off",
	"toml":   "true false",
	"sql":    "select from where insert into update delete values create table drop alter join left right inner outer on group by order limit as and or not null primary key index",
}

func buildHighlightRules() map[string]*highlightRule {
	out := make(map[string]*highlightRule, len(highlightKeywords))
	for lang, kws := range highlightKeywords {
		out[lang] = &highlightRule{rx: buildHighlightRegex(lang, kws), kw: splitKeywordSet(kws)}
	}
	return out
}

func buildHighlightRegex(lang, keywords string) *regexp.Regexp {
	parts := []string{`(?s)/\*.*?\*/`} // block comments (the shared default; c is identical)
	for _, lc := range highlightLineComments(lang) {
		parts = append(parts, regexp.QuoteMeta(lc)+`[^\n]*`)
	}
	for _, q := range highlightStringChars(lang) {
		safe := q
		if q == "]" || q == `\` || q == "^" || q == "-" {
			safe = `\` + q
		}
		parts = append(parts, q+`(?:\\.|[^\\`+safe+`\n])*`+q)
	}
	parts = append(parts, `\b\d[\w.]*\b`)
	if keywords != "" {
		parts = append(parts, `\b[A-Za-z_$][\w$]*\b`)
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}

func highlightLineComments(lang string) []string {
	switch lang {
	case "python", "ruby", "shell", "yaml", "toml":
		return []string{"#"}
	case "sql":
		return []string{"--"}
	}
	return []string{"//"}
}

func highlightStringChars(lang string) []string {
	switch lang {
	case "c", "rust", "python", "shell":
		return []string{"'", `"`}
	case "sql":
		return []string{"'"}
	case "json":
		return []string{`"`}
	case "yaml", "toml":
		return []string{"'", `"`}
	}
	return []string{"'", `"`, "`"}
}

func splitKeywordSet(s string) map[string]bool {
	fields := strings.Fields(s)
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return set
}

func isASCIIAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
