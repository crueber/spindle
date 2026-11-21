package webui

import (
	"strings"

	"golang.org/x/net/html"
)

// Raw-HTML passthrough is the one place markdown can inject markup, so this
// file is the security boundary for it (WP12): a tokenizer walks the fragment,
// emits only an allowlisted tag/attribute set, and never re-serializes anything
// it did not construct itself. Text tokens are escaped; URL attributes go
// through sanitizeURL and then the same repo resolver markdown links use.

// htmlAllowedTags is the practical GitHub set: layout, disclosure, media,
// typography and basic semantics. Anything else is unwrapped (its text stays)
// unless it is in htmlDroppedTags.
var htmlAllowedTags = map[string]bool{
	"div": true, "span": true, "p": true, "br": true, "hr": true,
	"details": true, "summary": true, "blockquote": true,
	"pre": true, "code": true, "kbd": true, "samp": true, "var": true,
	"sub": true, "sup": true, "mark": true, "del": true, "ins": true, "abbr": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "td": true, "th": true,
	"ul": true, "ol": true, "li": true,
	"img": true, "picture": true, "source": true, "video": true, "audio": true,
	"figure": true, "figcaption": true,
	"b": true, "i": true, "strong": true, "em": true, "u": true, "s": true, "a": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlVoidTags never take a closing tag (the HTML void set we allow).
var htmlVoidTags = map[string]bool{
	"br": true, "hr": true, "img": true, "source": true,
}

// htmlDroppedTags are removed WITH their content. Markup that can execute,
// load or submit (script/style/iframe/object/embed/form controls) and foreign
// content (svg/math/template) is never emitted or unwrapped.
var htmlDroppedTags = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "form": true, "input": true, "button": true,
	"textarea": true, "select": true, "option": true, "link": true,
	"meta": true, "base": true, "svg": true, "math": true,
	"template": true, "noscript": true, "frame": true, "frameset": true,
	"applet": true, "param": true, "noembed": true, "noframes": true,
	"title": true, "head": true, "body": true, "html": true,
}

// htmlBlockTags are block-level containers whose start tag begins an HTML
// block at the top of a markdown line (CommonMark HTML block type 6, plus the
// media/disclosure tags GitHub renders).
var htmlBlockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"details": true, "dialog": true, "div": true, "dl": true, "dd": true,
	"dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "header": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"summary": true, "table": true, "thead": true, "tbody": true, "tr": true,
	"td": true, "th": true, "ul": true, "video": true, "audio": true,
	"picture": true, "br": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true,
}

// htmlStrayCloseTags may be emitted even when their opener is in another
// markdown block. GitHub's HTML blocks end at a blank line, so
// "<details>" … blank … markdown … blank … "</details>" splits; the closing
// tag must survive to keep the document balanced. Restricting this to block
// containers keeps an authored "</div>" from closing an inline wrapper while
// still allowing the details/div/figure patterns READMEs use.
var htmlStrayCloseTags = map[string]bool{
	"div": true, "details": true, "figure": true, "blockquote": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "td": true,
	"th": true, "ul": true, "ol": true, "li": true, "dl": true, "dd": true,
	"dt": true, "p": true, "section": true, "article": true, "aside": true,
	"header": true, "footer": true, "nav": true, "main": true,
	"video": true, "audio": true, "picture": true,
}

// htmlBlockStart reports whether line begins a raw-HTML block: a block-level
// tag, a comment, or a line that carries nothing but tags (so "<br>" or
// "<img …>" become HTML, while "<kbd>Ctrl</kbd> and text" stays a paragraph
// with inline HTML).
func htmlBlockStart(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || !strings.HasPrefix(t, "<") {
		return false
	}
	if strings.HasPrefix(t, "<!--") {
		return true
	}
	name, ok := htmlTagName(t)
	if !ok {
		return false
	}
	if htmlBlockTags[name] {
		return true
	}
	return htmlTagsOnly(t)
}

// htmlTagsOnly reports whether s consists only of tags and whitespace.
func htmlTagsOnly(s string) bool {
	i, seen := 0, false
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] != '<' {
			return false
		}
		name, ok := htmlTagName(s[i:])
		if !ok {
			return false
		}
		if !htmlAllowedTags[name] && !htmlBlockTags[name] && !htmlDroppedTags[name] {
			return false
		}
		end := findHTMLTagEnd(s, i)
		if end < 0 {
			return false
		}
		seen = true
		i = end + 1
	}
	return seen
}

// htmlTagName reads the tag name at the start of s ("<name", "</name") and
// validates the character that follows it, so "<https://…>" is not a tag.
func htmlTagName(s string) (string, bool) {
	if !strings.HasPrefix(s, "<") {
		return "", false
	}
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	start := i
	for i < len(s) && (isAlphaByte(s[i]) || s[i] >= '0' && s[i] <= '9' || s[i] == '-') {
		i++
	}
	if i == start || !isAlphaByte(s[start]) {
		return "", false
	}
	if i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '>', '/':
		default:
			return "", false
		}
	}
	return strings.ToLower(s[start:i]), true
}

// findHTMLTagEnd returns the index of the ">" closing the tag that starts at
// i, honoring quoted attribute values. -1 when the tag is unterminated.
func findHTMLTagEnd(s string, i int) int {
	var quote byte
	for j := i; j < len(s); j++ {
		c := s[j]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '>':
			return j
		}
	}
	return -1
}

// sanitizeHTMLFragment parses a raw-HTML fragment and returns only the allowed
// markup, with text and attribute values escaped. ctx may be nil (no repo
// resolution). The result is safe to concatenate into the renderer's
// template.HTML output.
func sanitizeHTMLFragment(src string, ctx *MarkdownCtx) string {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	type level struct {
		name    string
		allowed bool
	}
	var stack []level
	var dropping []string // open dangerous tags whose content is suppressed

	popMatching := func(tag string) bool {
		if n := len(stack); n > 0 && stack[n-1].name == tag {
			stack = stack[:n-1]
			return true
		}
		return false
	}
	startTag := func(selfClosing bool) {
		name, hasAttr := z.TagName()
		tag := strings.ToLower(string(name))
		if htmlDroppedTags[tag] {
			if !htmlVoidTags[tag] && !selfClosing {
				dropping = append(dropping, tag)
			}
			return
		}
		if len(dropping) > 0 {
			return
		}
		if !htmlAllowedTags[tag] {
			if !htmlVoidTags[tag] && !selfClosing {
				stack = append(stack, level{name: tag})
			}
			return
		}
		attrs := sanitizeHTMLAttrs(tag, z, hasAttr, ctx)
		b.WriteString("<" + tag + attrs + ">")
		if !htmlVoidTags[tag] && !selfClosing {
			stack = append(stack, level{name: tag, allowed: true})
		}
	}

	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.TextToken:
			if len(dropping) == 0 {
				b.WriteString(escapeText(string(z.Text())))
			}
		case html.StartTagToken:
			startTag(false)
		case html.SelfClosingTagToken:
			startTag(true)
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := strings.ToLower(string(name))
			if len(dropping) > 0 {
				if dropping[len(dropping)-1] == tag {
					dropping = dropping[:len(dropping)-1]
				}
				continue
			}
			if !htmlAllowedTags[tag] || htmlVoidTags[tag] {
				popMatching(tag)
				continue
			}
			if popMatching(tag) {
				b.WriteString("</" + tag + ">")
			} else if htmlStrayCloseTags[tag] {
				b.WriteString("</" + tag + ">")
			}
		}
	}
}

// sanitizeHTMLAttrs filters one start tag's attributes. Unknown attributes
// (class, style, id, srcset, target, every on*) are dropped; allowed URL
// attributes are scheme-checked and repo-resolved.
func sanitizeHTMLAttrs(tag string, z *html.Tokenizer, hasAttr bool, ctx *MarkdownCtx) string {
	var b strings.Builder
	href := ""
	for hasAttr {
		var k, v []byte
		k, v, hasAttr = z.TagAttr()
		key := strings.ToLower(string(k))
		val := string(v)
		switch key {
		case "href":
			if tag != "a" {
				continue
			}
			u, ok := sanitizeURL(val, urlForLink)
			if !ok {
				continue
			}
			href = ctx.resolveURL(u, false)
			b.WriteString(` href="` + escapeAttr(href) + `"`)
		case "src":
			if tag != "img" && tag != "source" && tag != "video" && tag != "audio" {
				continue
			}
			if tag == "img" && isDataImage(val) {
				b.WriteString(` src="` + escapeAttr(strings.TrimSpace(val)) + `"`)
				continue
			}
			u, ok := sanitizeURL(val, urlForImage)
			if !ok {
				continue
			}
			u = ctx.resolveURL(u, true)
			b.WriteString(` src="` + escapeAttr(u) + `"`)
		case "poster":
			if tag != "video" {
				continue
			}
			u, ok := sanitizeURL(val, urlForImage)
			if !ok {
				continue
			}
			b.WriteString(` poster="` + escapeAttr(ctx.resolveURL(u, true)) + `"`)
		case "alt", "title":
			b.WriteString(" " + key + `="` + escapeAttr(val) + `"`)
		case "width", "height":
			if !isDimension(val) {
				continue
			}
			b.WriteString(" " + key + `="` + escapeAttr(val) + `"`)
		case "align":
			if tag != "img" && !htmlBlockTags[tag] {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "left", "center", "right":
				b.WriteString(` align="` + strings.ToLower(strings.TrimSpace(val)) + `"`)
			}
		case "colspan", "rowspan", "start":
			if !isDigits(val) {
				continue
			}
			b.WriteString(" " + key + `="` + escapeAttr(val) + `"`)
		case "type":
			if tag != "source" && tag != "video" && tag != "audio" {
				continue
			}
			if !isMediaType(val) {
				continue
			}
			b.WriteString(` type="` + escapeAttr(val) + `"`)
		case "controls", "loop", "muted", "playsinline", "open":
			if key == "open" && tag != "details" {
				continue
			}
			b.WriteString(" " + key)
		}
	}
	if tag == "a" {
		b.WriteString(` rel="nofollow ugc noopener"`)
		if isExternalHTTP(href) {
			b.WriteString(` target="_blank"`)
		}
	}
	return b.String()
}

// isDataImage allows only base64 raster data URIs on <img>, with a size cap;
// data:text/html and every other data payload stay rejected.
func isDataImage(u string) bool {
	u = strings.TrimSpace(u)
	low := strings.ToLower(u)
	prefixes := []string{
		"data:image/png;base64,",
		"data:image/jpeg;base64,",
		"data:image/gif;base64,",
		"data:image/webp;base64,",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			if len(u) > 1<<20 {
				return false
			}
			rest := u[len(p):]
			for i := 0; i < len(rest); i++ {
				c := rest[i]
				if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
					c == '+' || c == '/' || c == '=' {
					continue
				}
				return false
			}
			return rest != ""
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isDimension(s string) bool {
	if strings.HasSuffix(s, "%") {
		return isDigits(strings.TrimSuffix(s, "%"))
	}
	if strings.HasSuffix(strings.ToLower(s), "px") {
		return isDigits(s[:len(s)-2])
	}
	return isDigits(s)
}

func isMediaType(s string) bool {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 || len(s) > 100 {
		return false
	}
	for k := 0; k < len(s); k++ {
		c := s[k]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '.' || c == '+' || c == '-' || c == ';' || c == '=' {
			continue
		}
		return false
	}
	return true
}
