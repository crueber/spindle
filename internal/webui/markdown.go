// Package webui renders the server-side HTML for the htmx UI: the repo
// markdown subset (ported from web/src/lib/render-md.js), unified diffs
// (web/src/lib/diff.js), the commit-graph rail (web/src/lib/commit-graph.js)
// and code highlighting (web/src/lib/highlight.js).
//
// Every exported renderer returns template.HTML that was built exclusively
// from escaped text (escapeText/escapeAttr), a closed set of tags, and raw HTML
// that passed the allowlist sanitizer in mdsanitize.go. Raw HTML in markdown
// source is sanitized, never passed through, so the output is safe to embed
// with html/template without a second sanitizer pass.
package webui

import (
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// RenderMarkdown renders the repo's markdown subset to safe HTML. Relative
// URLs and #N refs pass through unlinked because there is no repo context; use
// RenderMarkdownIn or RenderMarkdownCtx from a repo page. Raw HTML passes
// through the allowlist sanitizer (mdsanitize.go).
//
// Ported from render-md.js: headings, emphasis/strong, inline code, fenced
// code (language class), ordered/unordered/task lists, blockquotes, tables,
// horizontal rules, CommonMark soft/hard breaks (a single source newline
// folds to a space; two trailing spaces or a trailing backslash is <br>) and
// @user mentions. Issue/PR refs (#N, PRN) need the repo base and stay plain
// text here; use RenderMarkdownIn from a repo page.
func RenderMarkdown(src string) template.HTML {
	return RenderMarkdownIn(src, "", "")
}

// RenderMarkdownIn is RenderMarkdown with the repo coordinates that issue/PR
// reference autolinks need: #N links to /{owner}/{repo}/issues/N and PRN to
// /{owner}/{repo}/pull/N, with the same boundary rules as render-md.js
// (boundary-delimited, original text preserved, numeric href). Empty owner
// or repo disables ref linking, exactly like the ctx-less JS call.
func RenderMarkdownIn(src, owner, repo string) template.HTML {
	base := ""
	if owner != "" && repo != "" {
		base = "/" + owner + "/" + repo
	}
	r := &mdRenderer{refBase: base}
	return template.HTML(r.blocks(splitMarkdownLines(src)))
}

// RenderMarkdownCtx renders markdown at a repo coordinate so relative images
// and links resolve against the document's directory and ref (GitHub parity;
// see MarkdownCtx.resolveURL). It also carries the #N/PRN ref base, like
// RenderMarkdownIn.
func RenderMarkdownCtx(src string, ctx MarkdownCtx) template.HTML {
	base := ""
	if ctx.Owner != "" && ctx.Repo != "" {
		base = "/" + ctx.Owner + "/" + ctx.Repo
	}
	r := &mdRenderer{refBase: base, ctx: &ctx}
	return template.HTML(r.blocks(splitMarkdownLines(src)))
}

// --- escapers ---------------------------------------------------------------

// escapeText escapes the three characters that can start markup in text
// content. Mirrors esc() in highlight.js/diff.js.
func escapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// escapeAttr additionally escapes both quote styles so a value can never
// leave a double- or single-quoted attribute.
func escapeAttr(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;",
	).Replace(s)
}

// --- block rendering --------------------------------------------------------

type mdRenderer struct {
	// refBase is "/{owner}/{repo}" or "" when refs are not linkable.
	refBase string
	// ctx resolves relative URLs; nil means pass them through (no repo
	// coordinate is known).
	ctx *MarkdownCtx
}

func splitMarkdownLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	return strings.Split(src, "\n")
}

func (r *mdRenderer) blocks(lines []string) string {
	var b strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		if isBlankLine(line) {
			i++
			continue
		}
		if f, ok := parseFence(line); ok {
			i = r.writeFence(&b, lines, i, f)
			continue
		}
		if lvl, text, ok := parseATXHeading(line); ok {
			r.writeHeading(&b, lvl, text)
			i++
			continue
		}
		if isThematicBreak(line) {
			b.WriteString("<hr>\n")
			i++
			continue
		}
		if htmlBlockStart(line) {
			i = r.writeHTMLBlock(&b, lines, i)
			continue
		}
		if _, ok := stripQuote(line); ok {
			i = r.writeBlockquote(&b, lines, i)
			continue
		}
		if _, ok := parseListMarker(line); ok {
			i = r.writeList(&b, lines, i)
			continue
		}
		if n, ok := tableStart(lines, i); ok {
			i = r.writeTable(&b, lines, i, n)
			continue
		}
		i = r.writeParagraph(&b, lines, i, false)
	}
	return b.String()
}

// blocksTight renders list-item content: paragraphs at this level drop their
// <p> wrapper (a tight list item, the marked shape) while nested blocks keep
// their own structure.
func (r *mdRenderer) blocksTight(lines []string) string {
	var b strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		if isBlankLine(line) {
			i++
			continue
		}
		if f, ok := parseFence(line); ok {
			i = r.writeFence(&b, lines, i, f)
			continue
		}
		if lvl, text, ok := parseATXHeading(line); ok {
			r.writeHeading(&b, lvl, text)
			i++
			continue
		}
		if isThematicBreak(line) {
			b.WriteString("<hr>\n")
			i++
			continue
		}
		if htmlBlockStart(line) {
			i = r.writeHTMLBlock(&b, lines, i)
			continue
		}
		if _, ok := stripQuote(line); ok {
			i = r.writeBlockquote(&b, lines, i)
			continue
		}
		if _, ok := parseListMarker(line); ok {
			i = r.writeList(&b, lines, i)
			continue
		}
		if n, ok := tableStart(lines, i); ok {
			i = r.writeTable(&b, lines, i, n)
			continue
		}
		i = r.writeParagraph(&b, lines, i, true)
	}
	return b.String()
}

// writeHeading emits an ATX/setext heading, demoted one level. Demotion keeps
// user markdown in its place in the page's heading order: the page already
// owns the single h1 (the repo masthead or the page title), so a README's
// "# Seed repo" must not introduce a second h1 or skip levels under it. h6
// stays h6 (five demotion steps is the deepest outline a rendered document
// can carry without abusing h7+). Both .markdown-body styles already cover
// h1–h6, so a demoted h2 renders with the old h1 treatment.
func (r *mdRenderer) writeHeading(b *strings.Builder, level int, text string) {
	if level < 6 {
		level++
	}
	tag := "h" + strconv.Itoa(level)
	b.WriteString("<" + tag + ">")
	b.WriteString(r.inline(text))
	b.WriteString("</" + tag + ">\n")
}

// writeHTMLBlock consumes a raw-HTML block (CommonMark type 6/7: through the
// next blank line) and emits its sanitized form. GitHub ends these blocks at
// a blank line too, which is what lets markdown between "<details>" and
// "</details>" render as markdown; the stray-closing-tag rule in the
// sanitizer keeps the split document balanced.
func (r *mdRenderer) writeHTMLBlock(b *strings.Builder, lines []string, i int) int {
	start := i
	for i < len(lines) && !isBlankLine(lines[i]) {
		i++
	}
	out := sanitizeHTMLFragment(strings.Join(lines[start:i], "\n"), r.ctx)
	if out != "" {
		b.WriteString(out)
		b.WriteString("\n")
	}
	return i
}

func (r *mdRenderer) writeParagraph(b *strings.Builder, lines []string, i int, tight bool) int {
	var para []string
	for i < len(lines) && !isBlankLine(lines[i]) {
		if len(para) > 0 {
			if lvl, ok := setextLevel(lines[i]); ok {
				r.writeHeading(b, lvl, strings.Join(paragraphText(para), " "))
				return i + 1
			}
			if isBlockStart(lines[i]) || startsTable(lines, i) || htmlBlockStart(lines[i]) {
				break
			}
		}
		para = append(para, lines[i])
		i++
	}
	if len(para) == 0 {
		return i + 1
	}
	texts, hards := paragraphLines(para)
	if !tight {
		b.WriteString("<p>")
	}
	for k := range texts {
		if k > 0 {
			// CommonMark: a soft break renders as a space (the newline is
			// insignificant whitespace, so the text reflows to the container);
			// only the previous line's hard-break marker emits <br>.
			if hards[k-1] {
				b.WriteString("<br>\n")
			} else {
				b.WriteString("\n")
			}
		}
		b.WriteString(r.inline(texts[k]))
	}
	if !tight {
		b.WriteString("</p>\n")
	}
	return i
}

// paragraphLines splits a paragraph's raw lines into inline text and the
// hard-break flag that belongs to each line's trailing newline. The last line
// has no following newline, so its trailing backslash stays literal.
func paragraphLines(para []string) ([]string, []bool) {
	texts := make([]string, len(para))
	hards := make([]bool, len(para))
	for k, line := range para {
		line = strings.TrimLeft(line, " \t")
		if k == len(para)-1 {
			texts[k] = strings.TrimRight(line, " \t")
			continue
		}
		texts[k], hards[k] = splitHardBreak(line)
	}
	return texts, hards
}

// paragraphText returns a paragraph's lines as one heading line: leading
// whitespace and hard-break markers are dropped, a single space joins them.
func paragraphText(lines []string) []string {
	texts, _ := paragraphLines(lines)
	return texts
}

// splitHardBreak separates a paragraph line's content from its line-break
// marker. CommonMark makes two or more trailing spaces, or an unescaped
// trailing backslash, a hard break (<br>); one trailing space, or none, is a
// soft break (a space). The marker never reaches inline rendering.
func splitHardBreak(line string) (string, bool) {
	content := strings.TrimRight(line, " \t")
	if len(line)-len(strings.TrimRight(line, " ")) >= 2 {
		return content, true
	}
	if strings.HasSuffix(content, `\`) {
		k := len(content) - 1
		for k > 0 && content[k-1] == '\\' {
			k--
		}
		// An odd run ends in a live backslash: the last one is the break
		// marker, the escaped pairs before it stay literal.
		if (len(content)-k)%2 == 1 {
			return content[:len(content)-1], true
		}
	}
	return content, false
}

func (r *mdRenderer) writeBlockquote(b *strings.Builder, lines []string, i int) int {
	var inner []string
	for i < len(lines) {
		if s, ok := stripQuote(lines[i]); ok {
			inner = append(inner, s)
			i++
			continue
		}
		if isBlankLine(lines[i]) {
			// Keep a blank only when the quote continues after it.
			j := i
			for j < len(lines) && isBlankLine(lines[j]) {
				j++
			}
			if j < len(lines) {
				if _, ok := stripQuote(lines[j]); ok {
					for ; i < j; i++ {
						inner = append(inner, "")
					}
					continue
				}
			}
			break
		}
		// Lazy continuation: an unindented line extends the quote's open
		// paragraph instead of ending the quote (CommonMark 5.2).
		if len(inner) > 0 && lazyParagraphOpen(inner) &&
			!isBlockStart(lines[i]) && !startsTable(lines, i) && !htmlBlockStart(lines[i]) {
			inner = append(inner, lines[i])
			i++
			continue
		}
		break
	}
	b.WriteString("<blockquote>\n")
	b.WriteString(r.blocks(inner))
	b.WriteString("</blockquote>\n")
	return i
}

type fenceInfo struct {
	ch   byte
	n    int
	info string
}

func parseFence(line string) (fenceInfo, bool) {
	lead := leadingSpaces(line)
	if lead > 3 || lead >= len(line) {
		return fenceInfo{}, false
	}
	c := line[lead]
	if c != '`' && c != '~' {
		return fenceInfo{}, false
	}
	n := runLength(line, lead, c)
	if n < 3 {
		return fenceInfo{}, false
	}
	info := strings.TrimSpace(line[lead+n:])
	if c == '`' && strings.ContainsRune(info, '`') {
		return fenceInfo{}, false
	}
	return fenceInfo{ch: c, n: n, info: info}, true
}

func (r *mdRenderer) writeFence(b *strings.Builder, lines []string, i int, f fenceInfo) int {
	i++
	var body []string
	for i < len(lines) {
		line := lines[i]
		if isFenceClose(line, f) {
			i++
			break
		}
		body = append(body, line)
		i++
	}
	code := strings.Join(body, "\n")
	if len(body) > 0 {
		code += "\n"
	}
	lang := fenceLanguage(f.info)
	b.WriteString("<pre><code")
	if lang != "" {
		b.WriteString(` class="language-` + lang + `"`)
	}
	b.WriteString(">")
	b.WriteString(escapeText(code))
	b.WriteString("</code></pre>\n")
	return i
}

// isFenceClose reports whether line closes the fence f: the same character run
// at least as long, indented no more than three spaces, with nothing but
// whitespace after it.
func isFenceClose(line string, f fenceInfo) bool {
	lead := leadingSpaces(line)
	if lead > 3 || lead >= len(line) || line[lead] != f.ch {
		return false
	}
	n := runLength(line, lead, f.ch)
	return n >= f.n && strings.TrimSpace(line[lead+n:]) == ""
}

// fenceLanguage keeps the first info-string word, restricted to the charset
// a class attribute needs. Anything else yields no class (plain <code>).
func fenceLanguage(info string) string {
	if info == "" {
		return ""
	}
	if k := strings.IndexAny(info, " \t{"); k >= 0 {
		info = info[:k]
	}
	var b strings.Builder
	for i := 0; i < len(info); i++ {
		c := info[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '+', c == '-', c == '_', c == '#', c == '.':
			b.WriteByte(c)
		}
	}
	return b.String()
}

type listMark struct {
	ordered      bool
	indent       int
	contentStart int
	text         string
}

func parseListMarker(line string) (listMark, bool) {
	ind := leadingSpaces(line)
	if ind > 3 || ind >= len(line) {
		return listMark{}, false
	}
	if c := line[ind]; c == '-' || c == '*' || c == '+' {
		j := ind + 1
		if j >= len(line) || (line[j] != ' ' && line[j] != '\t') {
			return listMark{}, false
		}
		for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
			j++
		}
		return listMark{indent: ind, contentStart: j, text: line[j:]}, true
	}
	j := ind
	for j < len(line) && j-ind < 9 && line[j] >= '0' && line[j] <= '9' {
		j++
	}
	if j == ind || j >= len(line) || (line[j] != '.' && line[j] != ')') {
		return listMark{}, false
	}
	j++
	if j >= len(line) || (line[j] != ' ' && line[j] != '\t') {
		return listMark{}, false
	}
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	return listMark{ordered: true, indent: ind, contentStart: j, text: line[j:]}, true
}

func (r *mdRenderer) writeList(b *strings.Builder, lines []string, i int) int {
	first, _ := parseListMarker(lines[i])
	tag := "ul"
	if first.ordered {
		tag = "ol"
	}
	b.WriteString("<" + tag + ">\n")
	for i < len(lines) {
		if isBlankLine(lines[i]) {
			j := i
			for j < len(lines) && isBlankLine(lines[j]) {
				j++
			}
			if j < len(lines) {
				if m, ok := parseListMarker(lines[j]); ok && m.ordered == first.ordered && m.indent == first.indent {
					i = j
					continue
				}
			}
			break
		}
		m, ok := parseListMarker(lines[i])
		if !ok || m.ordered != first.ordered || m.indent != first.indent {
			break
		}
		item := []string{m.text}
		contentIndent := m.contentStart
		i++
		for i < len(lines) {
			line := lines[i]
			if isBlankLine(line) {
				j := i
				for j < len(lines) && isBlankLine(lines[j]) {
					j++
				}
				if j < len(lines) {
					nm, isItem := parseListMarker(lines[j])
					if leadingSpaces(lines[j]) >= contentIndent && (!isItem || nm.indent > first.indent) {
						for ; i < j; i++ {
							item = append(item, "")
						}
						continue
					}
				}
				break
			}
			lead := leadingSpaces(line)
			if lead >= contentIndent {
				item = append(item, line[contentIndent:])
				i++
				continue
			}
			if lead >= first.indent+2 {
				item = append(item, line[first.indent+2:])
				i++
				continue
			}
			// Lazy continuation: an unindented line extends the item's open
			// paragraph rather than ending the list (CommonMark 5.2). A block
			// start (another item, heading, fence, quote, table) still ends it.
			if lazyParagraphOpen(item) && !isBlockStart(line) && !startsTable(lines, i) && !htmlBlockStart(line) {
				item = append(item, strings.TrimLeft(line, " \t"))
				i++
				continue
			}
			break
		}
		r.writeListItem(b, item)
	}
	b.WriteString("</" + tag + ">\n")
	return i
}

// lazyParagraphOpen reports whether a container's last block is an open
// paragraph — the only block CommonMark lets a lazy continuation line extend
// (spec 5.2). Fences and raw-HTML blocks are tracked so a line after a closed
// code block or an ended HTML block is not mistaken for paragraph text.
func lazyParagraphOpen(lines []string) bool {
	var fence fenceInfo
	inFence := false
	inHTML := false
	open := false
	for _, line := range lines {
		if inFence {
			if isFenceClose(line, fence) {
				inFence = false
			}
			continue
		}
		if inHTML {
			if isBlankLine(line) {
				inHTML = false
			}
			continue
		}
		if isBlankLine(line) {
			open = false
			continue
		}
		if f, ok := parseFence(line); ok {
			fence, inFence, open = f, true, false
			continue
		}
		if htmlBlockStart(line) {
			inHTML, open = true, false
			continue
		}
		if isBlockStart(line) {
			open = false
			continue
		}
		open = true
	}
	return open
}

func (r *mdRenderer) writeListItem(b *strings.Builder, item []string) {
	b.WriteString("<li>")
	if len(item) > 0 {
		if rest, checked, ok := taskBox(item[0]); ok {
			item[0] = rest
			if checked {
				b.WriteString(`<input type="checkbox" checked disabled>`)
			} else {
				b.WriteString(`<input type="checkbox" disabled>`)
			}
			b.WriteString(" ")
		}
	}
	b.WriteString(r.blocksTight(item))
	b.WriteString("</li>\n")
}

func taskBox(s string) (rest string, checked, ok bool) {
	if len(s) < 3 || s[0] != '[' || s[2] != ']' {
		return "", false, false
	}
	switch s[1] {
	case ' ':
		rest = strings.TrimPrefix(s[3:], " ")
		return rest, false, true
	case 'x', 'X':
		rest = strings.TrimPrefix(s[3:], " ")
		return rest, true, true
	}
	return "", false, false
}

func isThematicBreak(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

func setextLevel(line string) (int, bool) {
	s := strings.TrimRight(line, " \t")
	if s == "" {
		return 0, false
	}
	c := s[0]
	if c != '=' && c != '-' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return 0, false
		}
	}
	if c == '=' {
		return 1, true
	}
	return 2, true
}

func parseATXHeading(line string) (int, string, bool) {
	lead := leadingSpaces(line)
	if lead > 3 || lead >= len(line) {
		return 0, "", false
	}
	n := runLength(line, lead, '#')
	if n < 1 || n > 6 {
		return 0, "", false
	}
	if lead+n < len(line) && line[lead+n] != ' ' && line[lead+n] != '\t' {
		return 0, "", false
	}
	text := strings.TrimSpace(line[lead+n:])
	// Closing hashes ("## title ##") are markup only when space-separated.
	if k := len(text); k > 0 && text[k-1] == '#' {
		m := k
		for m > 0 && text[m-1] == '#' {
			m--
		}
		if m == 0 {
			text = ""
		} else if text[m-1] == ' ' || text[m-1] == '\t' {
			text = strings.TrimSpace(text[:m])
		}
	}
	return n, text, true
}

func stripQuote(line string) (string, bool) {
	lead := leadingSpaces(line)
	if lead > 3 || lead >= len(line) || line[lead] != '>' {
		return "", false
	}
	s := line[lead+1:]
	if strings.HasPrefix(s, " ") {
		s = s[1:]
	}
	return s, true
}

func isBlockStart(line string) bool {
	if _, ok := parseFence(line); ok {
		return true
	}
	if _, _, ok := parseATXHeading(line); ok {
		return true
	}
	if isThematicBreak(line) {
		return true
	}
	if _, ok := stripQuote(line); ok {
		return true
	}
	if _, ok := parseListMarker(line); ok {
		return true
	}
	return false
}

// --- tables -----------------------------------------------------------------

func startsTable(lines []string, i int) bool {
	_, ok := tableStart(lines, i)
	return ok
}

func tableStart(lines []string, i int) (int, bool) {
	if i+1 >= len(lines) {
		return 0, false
	}
	head := lines[i]
	if !strings.Contains(head, "|") || isBlockStart(head) {
		return 0, false
	}
	delim := splitTableRow(lines[i+1])
	hc := splitTableRow(head)
	if len(delim) == 0 || len(delim) != len(hc) {
		return 0, false
	}
	for _, cell := range delim {
		if !isDelimiterCell(cell) {
			return 0, false
		}
	}
	return len(hc), true
}

func isDelimiterCell(cell string) bool {
	s := strings.TrimSpace(cell)
	if strings.HasPrefix(s, ":") {
		s = s[1:]
	}
	if strings.HasSuffix(s, ":") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			return false
		}
	}
	return true
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	cells := make([]string, 0, 4)
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if c == '|' {
			cells = append(cells, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	cells = append(cells, cur.String())
	if len(cells) > 0 && strings.TrimSpace(cells[0]) == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
		cells = cells[:len(cells)-1]
	}
	for i := range cells {
		cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], `\|`, "|"))
	}
	return cells
}

func (r *mdRenderer) writeTable(b *strings.Builder, lines []string, i, n int) int {
	head := splitTableRow(lines[i])
	i += 2
	b.WriteString("<table>\n<thead>\n<tr>\n")
	for k := 0; k < n; k++ {
		cell := ""
		if k < len(head) {
			cell = head[k]
		}
		b.WriteString("<th>" + r.inline(cell) + "</th>\n")
	}
	b.WriteString("</tr>\n</thead>\n")
	var rows []string
	for i < len(lines) {
		line := lines[i]
		if isBlankLine(line) || !strings.Contains(line, "|") || isBlockStart(line) {
			break
		}
		rows = append(rows, line)
		i++
	}
	if len(rows) > 0 {
		b.WriteString("<tbody>\n")
		for _, row := range rows {
			cells := splitTableRow(row)
			b.WriteString("<tr>\n")
			for k := 0; k < n; k++ {
				cell := ""
				if k < len(cells) {
					cell = cells[k]
				}
				b.WriteString("<td>" + r.inline(cell) + "</td>\n")
			}
			b.WriteString("</tr>\n")
		}
		b.WriteString("</tbody>\n")
	}
	b.WriteString("</table>\n")
	return i
}

// --- inline rendering -------------------------------------------------------

func (r *mdRenderer) inline(s string) string {
	var b strings.Builder
	r.inlineInto(&b, s, true)
	return b.String()
}

// resolveURL applies the repo context to a validated relative URL (nil ctx
// leaves it untouched).
func (r *mdRenderer) resolveURL(u string, image bool) string {
	if r.ctx == nil {
		return u
	}
	return r.ctx.resolveURL(u, image)
}

// parseInlineHTML consumes one raw-HTML construct starting at s[i]: a comment
// (dropped), a single tag, or a balanced element. It returns the sanitized
// HTML and the index after it. Unbalanced/unterminated markup is not HTML —
// the caller escapes it, so a stray "<kbd>" can never leak an open tag.
func (r *mdRenderer) parseInlineHTML(s string, i int) (string, int, bool) {
	if strings.HasPrefix(s[i:], "<!--") {
		if end := strings.Index(s[i+4:], "-->"); end >= 0 {
			return "", i + 4 + end + 3, true
		}
		return "", 0, false
	}
	name, ok := htmlTagName(s[i:])
	if !ok {
		return "", 0, false
	}
	openEnd := findHTMLTagEnd(s, i)
	if openEnd < 0 {
		return "", 0, false
	}
	if strings.HasPrefix(s[i:], "</") {
		if !htmlAllowedTags[name] || htmlVoidTags[name] {
			return "", openEnd + 1, true
		}
		return "</" + name + ">", openEnd + 1, true
	}
	selfClosing := strings.HasSuffix(strings.TrimRight(s[i:openEnd], " \t"), "/")
	if selfClosing || htmlVoidTags[name] {
		return sanitizeHTMLFragment(s[i:openEnd+1], r.ctx), openEnd + 1, true
	}
	end, ok := matchInlineClose(s, openEnd+1, name)
	if !ok {
		return "", 0, false
	}
	return sanitizeHTMLFragment(s[i:end], r.ctx), end, true
}

// matchInlineClose finds the close tag matching an already-consumed open tag
// of name, counting nested same-name elements. The returned index is just past
// the closing ">".
func matchInlineClose(s string, from int, name string) (int, bool) {
	depth := 1
	for j := from; j < len(s); {
		k := strings.IndexByte(s[j:], '<')
		if k < 0 {
			return 0, false
		}
		k += j
		nm, ok := htmlTagName(s[k:])
		if !ok || nm != name {
			j = k + 1
			continue
		}
		end := findHTMLTagEnd(s, k)
		if end < 0 {
			return 0, false
		}
		if strings.HasPrefix(s[k:], "</") {
			depth--
			if depth == 0 {
				return end + 1, true
			}
		} else if !strings.HasSuffix(strings.TrimRight(s[k:end], " \t"), "/") && !htmlVoidTags[name] {
			depth++
		}
		j = end + 1
	}
	return 0, false
}

// inlineInto renders inline markup. allowLinkify is false inside an anchor
// (link label), where the JS passes skip #N/@user linkification too.
func (r *mdRenderer) inlineInto(b *strings.Builder, s string, allowLinkify bool) {
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			if i+1 < len(s) && isASCIIPunct(s[i+1]) {
				b.WriteString(escapeText(s[i+1 : i+2]))
				i += 2
				continue
			}
			b.WriteString(`\`)
			i++
		case '`':
			n := runLength(s, i, '`')
			if end := findCloseRun(s, i+n, '`', n); end >= 0 {
				b.WriteString("<code>")
				b.WriteString(escapeText(s[i+n : end]))
				b.WriteString("</code>")
				i = end + n
				continue
			}
			b.WriteString(escapeText(s[i : i+n]))
			i += n
		case '!':
			if i+1 < len(s) && s[i+1] == '[' {
				if html, next, ok := r.parseLink(s, i+1, true, allowLinkify); ok {
					b.WriteString(html)
					i = next
					continue
				}
			}
			r.writeText(b, s[i:i+1], allowLinkify)
			i++
		case '[':
			if html, next, ok := r.parseLink(s, i, false, allowLinkify); ok {
				b.WriteString(html)
				i = next
				continue
			}
			b.WriteByte('[')
			i++
		case '*', '_':
			if html, next, ok := r.parseEmphasis(s, i, allowLinkify); ok {
				b.WriteString(html)
				i = next
				continue
			}
			b.WriteString(escapeText(s[i : i+1]))
			i++
		case '~':
			if strings.HasPrefix(s[i:], "~~") {
				if end := strings.Index(s[i+2:], "~~"); end >= 0 {
					var inner strings.Builder
					r.inlineInto(&inner, s[i+2:i+2+end], allowLinkify)
					b.WriteString("<del>" + inner.String() + "</del>")
					i += 2 + end + 2
					continue
				}
				b.WriteString("~~")
				i += 2
				continue
			}
			b.WriteByte('~')
			i++
		case '<':
			if html, next, ok := angleAutolink(s, i); ok {
				b.WriteString(html)
				i = next
				continue
			}
			if html, next, ok := r.parseInlineHTML(s, i); ok {
				b.WriteString(html)
				i = next
				continue
			}
			b.WriteString("&lt;")
			i++
		case 'h', 'H':
			if allowLinkify {
				if html, next, ok := bareURLAutolink(s, i); ok {
					b.WriteString(html)
					i = next
					continue
				}
			}
			n := textRunLength(s, i)
			r.writeText(b, s[i:i+n], allowLinkify)
			i += n
		default:
			n := textRunLength(s, i)
			r.writeText(b, s[i:i+n], allowLinkify)
			i += n
		}
	}
}

func textRunLength(s string, i int) int {
	j := i + 1
	for j < len(s) {
		switch s[j] {
		case ' ', '\t', '\n', '\\', '`', '*', '_', '~', '[', '!', '<', 'h', 'H':
			return j - i
		}
		j++
	}
	return j - i
}

func (r *mdRenderer) parseEmphasis(s string, i int, allowLinkify bool) (string, int, bool) {
	ch := s[i]
	run := runLength(s, i, ch)
	if run >= 3 {
		if end := findCloseRun(s, i+3, ch, 3); end > i+3 {
			var inner strings.Builder
			r.inlineInto(&inner, s[i+3:end], allowLinkify)
			return "<em><strong>" + inner.String() + "</strong></em>", end + 3, true
		}
	}
	n := 1
	if run >= 2 {
		n = 2
	}
	if ch == '_' {
		if i+n < len(s) && isSpaceByte(s[i+n]) {
			return "", 0, false
		}
		if i > 0 && isWordByte(s[i-1]) {
			return "", 0, false
		}
	}
	for n >= 1 {
		if end := findCloseRun(s, i+n, ch, n); end > i+n {
			if ch == '_' && (isSpaceByte(s[end-1]) || (end+n < len(s) && isWordByte(s[end+n]))) {
				n--
				if n == 0 {
					return "", 0, false
				}
				continue
			}
			var inner strings.Builder
			r.inlineInto(&inner, s[i+n:end], allowLinkify)
			if n == 2 {
				return "<strong>" + inner.String() + "</strong>", end + 2, true
			}
			return "<em>" + inner.String() + "</em>", end + 1, true
		}
		n--
	}
	return "", 0, false
}

func (r *mdRenderer) parseLink(s string, start int, image, allowLinkify bool) (string, int, bool) {
	depth := 0
	j := start
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				goto found
			}
		}
	}
found:
	if j >= len(s) {
		return "", 0, false
	}
	label := s[start+1 : j]
	k := j + 1
	if k >= len(s) || s[k] != '(' {
		return "", 0, false
	}
	k++
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		k++
	}
	var url string
	if k < len(s) && s[k] == '<' {
		end := strings.IndexByte(s[k+1:], '>')
		if end < 0 {
			return "", 0, false
		}
		url = s[k+1 : k+1+end]
		k += end + 2
	} else {
		u := k
		parens := 0
		for k < len(s) {
			c := s[k]
			if c == '\\' && k+1 < len(s) {
				k += 2
				continue
			}
			if c == '(' {
				parens++
				k++
				continue
			}
			if c == ')' {
				if parens == 0 {
					break
				}
				parens--
				k++
				continue
			}
			if c == ' ' || c == '\t' {
				break
			}
			k++
		}
		url = s[u:k]
	}
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		k++
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		q := s[k]
		end := strings.IndexByte(s[k+1:], q)
		if end < 0 {
			return "", 0, false
		}
		k += end + 2
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
	}
	if k >= len(s) || s[k] != ')' {
		return "", 0, false
	}
	next := k + 1
	url = strings.NewReplacer(`\(`, "(", `\)`, ")", `\\`, `\`).Replace(url)
	if image {
		alt := label
		src, ok := sanitizeURL(url, urlForImage)
		if !ok {
			return escapeText(alt), next, true
		}
		src = r.resolveURL(src, true)
		return `<img src="` + escapeAttr(src) + `" alt="` + escapeAttr(alt) + `">`, next, true
	}
	var inner strings.Builder
	r.inlineInto(&inner, label, false)
	href, ok := sanitizeURL(url, urlForLink)
	if !ok {
		return inner.String(), next, true
	}
	href = r.resolveURL(href, false)
	var b strings.Builder
	b.WriteString(`<a href="` + escapeAttr(href) + `" rel="nofollow ugc noopener"`)
	if isExternalHTTP(href) {
		b.WriteString(` target="_blank"`)
	}
	b.WriteString(">" + inner.String() + "</a>")
	return b.String(), next, true
}

func angleAutolink(s string, i int) (string, int, bool) {
	end := strings.IndexByte(s[i+1:], '>')
	if end < 0 {
		return "", 0, false
	}
	body := s[i+1 : i+1+end]
	if body == "" || strings.ContainsAny(body, " \t\n<") {
		return "", 0, false
	}
	href, ok := sanitizeURL(body, urlForLink)
	if !ok || !isExternalHTTP(href) && !strings.HasPrefix(strings.ToLower(href), "mailto:") {
		return "", 0, false
	}
	var b strings.Builder
	b.WriteString(`<a href="` + escapeAttr(href) + `" rel="nofollow ugc noopener"`)
	if isExternalHTTP(href) {
		b.WriteString(` target="_blank"`)
	}
	b.WriteString(">" + escapeText(body) + "</a>")
	return b.String(), i + end + 2, true
}

func bareURLAutolink(s string, i int) (string, int, bool) {
	if i > 0 && isWordByte(s[i-1]) {
		return "", 0, false
	}
	var prefix string
	switch {
	case strings.HasPrefix(s[i:], "https://"):
		prefix = "https://"
	case strings.HasPrefix(s[i:], "http://"):
		prefix = "http://"
	default:
		return "", 0, false
	}
	end := i + len(prefix)
	for end < len(s) {
		c := s[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '<' || c == '"' {
			break
		}
		end++
	}
	for end > i+len(prefix) {
		c := s[end-1]
		if strings.ContainsRune(".,;:!?", rune(c)) {
			end--
			continue
		}
		if c == ')' && strings.Count(s[i:end], "(") < strings.Count(s[i:end], ")") {
			end--
			continue
		}
		break
	}
	if end <= i+len(prefix) {
		return "", 0, false
	}
	url := s[i:end]
	href, ok := sanitizeURL(url, urlForLink)
	if !ok {
		return "", 0, false
	}
	return `<a href="` + escapeAttr(href) + `" rel="nofollow ugc noopener" target="_blank">` + escapeText(url) + `</a>`, end, true
}

// --- #N / PRN refs and @mentions -------------------------------------------

type refMatch struct {
	text string
	path string
	end  int
}

func (r *mdRenderer) writeText(b *strings.Builder, s string, allowLinkify bool) {
	if s == "" {
		return
	}
	if !allowLinkify {
		b.WriteString(escapeText(s))
		return
	}
	last := 0
	for i := 0; i < len(s); i++ {
		if r.refBase != "" && (s[i] == '#' || s[i] == 'p' || s[i] == 'P') {
			if m, ok := matchRef(s, i); ok {
				b.WriteString(escapeText(s[last:i]))
				b.WriteString(`<a href="` + escapeAttr(r.refBase+m.path) + `" rel="nofollow ugc noopener">`)
				b.WriteString(escapeText(m.text))
				b.WriteString("</a>")
				i = m.end - 1
				last = m.end
				continue
			}
		}
		if s[i] == '@' {
			if m, ok := matchMention(s, i); ok {
				b.WriteString(escapeText(s[last:i]))
				b.WriteString(`@<a href="` + escapeAttr("/"+m.href) + `" rel="nofollow ugc noopener">`)
				b.WriteString(escapeText(m.principal))
				b.WriteString("</a>")
				b.WriteString(escapeText(s[m.textEnd:m.end]))
				i = m.end - 1
				last = m.end
			}
		}
	}
	b.WriteString(escapeText(s[last:]))
}

func matchRef(s string, i int) (refMatch, bool) {
	if i > 0 && isRefBoundaryExcluded(s[i-1]) {
		return refMatch{}, false
	}
	pr := s[i] != '#'
	j := i + 1
	if pr {
		if i+1 >= len(s) || (s[i+1] != 'r' && s[i+1] != 'R') {
			return refMatch{}, false
		}
		j = i + 2
	}
	d := j
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == d {
		return refMatch{}, false
	}
	if j < len(s) {
		if isWordByte(s[j]) {
			return refMatch{}, false
		}
		if s[j] == '.' && j+1 < len(s) && s[j+1] >= '0' && s[j+1] <= '9' {
			return refMatch{}, false
		}
	}
	num := strings.TrimLeft(s[d:j], "0")
	if num == "" {
		num = "0"
	}
	path := "/issues/" + num
	if pr {
		path = "/pull/" + num
	}
	return refMatch{text: s[i:j], path: path, end: j}, true
}

type mentionMatch struct {
	principal string // display text, as typed
	href      string // lowercased principal (the canonical profile key)
	textEnd   int
	end       int
}

func matchMention(s string, i int) (mentionMatch, bool) {
	if i > 0 && isMentionBoundaryExcluded(s[i-1]) {
		return mentionMatch{}, false
	}
	t := i + 1
	tokenEnd := 0
	// Email-shaped principal is tried first so @bob@example.com links whole.
	{
		j := t
		for j < len(s) && isEmailLocalByte(s[j]) {
			j++
		}
		if j > t && j < len(s) && s[j] == '@' {
			k := j + 1
			for k < len(s) && isEmailDomainByte(s[k]) {
				k++
			}
			best := 0
			for p := j + 2; p < k; p++ {
				if s[p] != '.' {
					continue
				}
				a := p + 1
				for a < len(s) && isAlphaByte(s[a]) {
					a++
				}
				if a-(p+1) >= 2 && a > best {
					best = a
				}
			}
			if best > 0 {
				tokenEnd = best
			}
		}
	}
	if tokenEnd == 0 {
		j := t
		for j < len(s) && isUsernameByte(s[j]) {
			j++
		}
		if j == t {
			return mentionMatch{}, false
		}
		tokenEnd = j
	}
	if tokenEnd < len(s) && (s[tokenEnd] == '/' || s[tokenEnd] == '@') {
		return mentionMatch{}, false
	}
	token := s[t:tokenEnd]
	principal := strings.TrimRight(token, ".,;:!?\"' )]}")
	if principal == "" {
		return mentionMatch{}, false
	}
	lower := strings.ToLower(principal)
	if !validMentionUsername(lower) && !validMentionEmail(lower) {
		return mentionMatch{}, false
	}
	return mentionMatch{principal: principal, href: lower, textEnd: t + len(principal), end: tokenEnd}, true
}

func validMentionUsername(t string) bool {
	if len(t) < 1 || len(t) > 64 || t[0] == '.' || t == ".." {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

var mentionEmailRe = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`)

func validMentionEmail(t string) bool {
	return len(t) <= 254 && !strings.Contains(t, "/") && mentionEmailRe.MatchString(t)
}

func isRefBoundaryExcluded(c byte) bool {
	return isWordByte(c) || c == '&'
}

func isMentionBoundaryExcluded(c byte) bool {
	return isWordByte(c) || c == '@' || c == '-'
}

func isUsernameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

func isEmailLocalByte(c byte) bool {
	return isUsernameByte(c) || c == '%' || c == '+'
}

func isEmailDomainByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-'
}

func isAlphaByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// --- URL sanitization (the security boundary) ------------------------------

type urlPurpose int

const (
	urlForLink urlPurpose = iota
	urlForImage
)

var (
	entityRefRe = regexp.MustCompile(`&(?:#[xX]([0-9a-fA-F]{1,6})|#([0-9]{1,7})|([a-zA-Z][a-zA-Z0-9]{1,31}));`)
	schemeRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

var namedEntities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'",
	"colon": ":", "sol": "/", "bsol": `\`, "tab": "\t", "newline": "\n",
	"nbsp": "\u00a0", "lpar": "(", "rpar": ")", "period": ".", "comma": ",",
}

// decodeEntities decodes the HTML character references a URL can carry, so a
// scheme cannot hide behind one (java&#115;cript:). Named forms are matched
// case-insensitively, numeric forms in decimal and hex.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return entityRefRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := entityRefRe.FindStringSubmatch(m)
		if sub[1] != "" {
			if v, err := strconv.ParseUint(sub[1], 16, 32); err == nil && v <= 0x10ffff {
				return string(rune(v))
			}
		}
		if sub[2] != "" {
			if v, err := strconv.ParseUint(sub[2], 10, 32); err == nil && v <= 0x10ffff {
				return string(rune(v))
			}
		}
		if v, ok := namedEntities[strings.ToLower(sub[3])]; ok {
			return v
		}
		return m
	})
}

// sanitizeURL is the link/image security boundary. It allows http, https
// and mailto (links only) plus relative URLs, and rejects every other scheme
// (javascript:, data:, vbscript:, file:, …), protocol-relative URLs,
// backslash-escaped hosts, and ASCII control characters (the java\tscript:
// family). The returned value is entity-decoded and control-free, ready for
// escapeAttr.
func sanitizeURL(raw string, purpose urlPurpose) (string, bool) {
	u := strings.TrimSpace(decodeEntities(raw))
	u = stripControlChars(u)
	u = strings.TrimSpace(u)
	if u == "" {
		return "", true
	}
	if strings.HasPrefix(u, "//") || strings.HasPrefix(u, `\`) {
		return "", false
	}
	if schemeRe.MatchString(u) {
		scheme := strings.ToLower(u[:strings.IndexByte(u, ':')])
		switch scheme {
		case "http", "https":
		case "mailto":
			if purpose == urlForImage {
				return "", false
			}
		default:
			return "", false
		}
	}
	return u, true
}

func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func isExternalHTTP(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// --- small byte helpers -----------------------------------------------------

func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

func leadingSpaces(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

func runLength(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func findCloseRun(s string, from int, c byte, n int) int {
	for i := from; i+n <= len(s); i++ {
		if s[i] != c {
			continue
		}
		r := runLength(s, i, c)
		if r == n {
			return i
		}
		i += r - 1
	}
	return -1
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n'
}
