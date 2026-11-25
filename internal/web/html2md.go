package web

import (
	"html"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// HTMLToMarkdown converts an HTML document to clean Markdown and returns the
// document title (if one is present) alongside the converted body.
//
// The converter is intentionally self-contained: it uses a small streaming
// tokenizer instead of regular expressions so that nested tags, raw script
// bodies, comments, and attribute quoting are handled correctly.
func HTMLToMarkdown(document string) (title string, markdown string) {
	return HTMLToMarkdownBase(document, "")
}

// HTMLToMarkdownBase behaves like HTMLToMarkdown but resolves relative links
// and image sources against baseURL.
func HTMLToMarkdownBase(document, baseURL string) (title string, markdown string) {
	c := &mdConverter{}
	c.bufs = []*strings.Builder{{}}
	if strings.TrimSpace(baseURL) != "" {
		if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && u.Scheme != "" {
			c.base = u
		}
	}

	rest := document
	for len(rest) > 0 {
		tok, next := nextHTMLToken(rest)
		rest = next
		switch tok.kind {
		case tokHTMLText:
			c.handleText(tok.text)
		case tokHTMLStart:
			c.handleStart(tok)
			// script/style bodies are raw text: skip directly to the closing
			// tag so characters like "<" inside JavaScript do not confuse the
			// tokenizer.
			if tok.name == "script" || tok.name == "style" {
				rest = skipRawElement(rest, tok.name)
			}
		case tokHTMLEnd:
			c.handleEnd(tok)
		}
	}
	return c.title.String(), c.finish()
}

// stripHTML returns the collapsed plain text of an HTML fragment. It is used
// for search-result titles and snippets.
func stripHTML(fragment string) string {
	var b strings.Builder
	rest := fragment
	for len(rest) > 0 {
		tok, next := nextHTMLToken(rest)
		rest = next
		switch tok.kind {
		case tokHTMLText:
			b.WriteString(html.UnescapeString(tok.text))
			b.WriteByte(' ')
		case tokHTMLStart, tokHTMLEnd:
			// Structural tags collapse to a space so words do not run
			// together across tag boundaries.
			if tok.name == "br" || tok.name == "p" || tok.name == "div" {
				b.WriteByte(' ')
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type htmlTokenKind int

const (
	tokHTMLText htmlTokenKind = iota
	tokHTMLStart
	tokHTMLEnd
)

type htmlToken struct {
	kind        htmlTokenKind
	name        string
	text        string
	attrs       map[string]string
	selfClosing bool
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// nextHTMLToken returns the next token and the remaining input. Comments and
// declarations are consumed transparently. A malformed tag is treated as
// text so conversion never loops or silently drops content.
func nextHTMLToken(s string) (htmlToken, string) {
	for len(s) > 0 {
		if s[0] != '<' {
			if i := strings.IndexByte(s, '<'); i >= 0 {
				return htmlToken{kind: tokHTMLText, text: s[:i]}, s[i:]
			}
			return htmlToken{kind: tokHTMLText, text: s}, ""
		}

		// Comments.
		if strings.HasPrefix(s, "<!--") {
			if i := strings.Index(s[4:], "-->"); i >= 0 {
				s = s[4+i+3:]
				continue
			}
			return htmlToken{}, ""
		}

		// Doctype, processing instructions, CDATA declarations.
		if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") {
			if i := strings.IndexByte(s, '>'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return htmlToken{}, ""
		}

		// Closing tag.
		if strings.HasPrefix(s, "</") {
			end := strings.IndexByte(s, '>')
			if end < 0 {
				return htmlToken{kind: tokHTMLText, text: s}, ""
			}
			inner := strings.TrimSpace(s[2:end])
			name := inner
			if i := strings.IndexAny(inner, " \t\r\n"); i >= 0 {
				name = inner[:i]
			}
			return htmlToken{kind: tokHTMLEnd, name: strings.ToLower(name)}, s[end+1:]
		}

		// A bare "<" that does not begin a tag is literal text.
		if len(s) < 2 || !isASCIILetter(s[1]) {
			return htmlToken{kind: tokHTMLText, text: "<"}, s[1:]
		}

		// Opening tag.
		i := 1
		nameStart := i
		for i < len(s) && !isHTMLSpace(s[i]) && s[i] != '>' && s[i] != '/' {
			i++
		}
		tok := htmlToken{
			kind:  tokHTMLStart,
			name:  strings.ToLower(s[nameStart:i]),
			attrs: make(map[string]string),
		}

		for i < len(s) {
			for i < len(s) && isHTMLSpace(s[i]) {
				i++
			}
			if i >= len(s) {
				break
			}
			if s[i] == '>' {
				return tok, s[i+1:]
			}
			if s[i] == '/' {
				if i+1 < len(s) && s[i+1] == '>' {
					tok.selfClosing = true
					return tok, s[i+2:]
				}
				i++
				continue
			}

			keyStart := i
			for i < len(s) && !isHTMLSpace(s[i]) && s[i] != '=' && s[i] != '>' && s[i] != '/' {
				i++
			}
			key := strings.ToLower(s[keyStart:i])
			for i < len(s) && isHTMLSpace(s[i]) {
				i++
			}
			value := ""
			if i < len(s) && s[i] == '=' {
				i++
				for i < len(s) && isHTMLSpace(s[i]) {
					i++
				}
				if i < len(s) && (s[i] == '"' || s[i] == '\'') {
					quote := s[i]
					i++
					valueStart := i
					for i < len(s) && s[i] != quote {
						i++
					}
					value = s[valueStart:i]
					if i < len(s) {
						i++
					}
				} else {
					valueStart := i
					for i < len(s) && !isHTMLSpace(s[i]) && s[i] != '>' {
						i++
					}
					value = s[valueStart:i]
				}
			}
			if key != "" {
				tok.attrs[key] = html.UnescapeString(value)
			}
		}

		// Unterminated tag: emit the remainder as text.
		return htmlToken{kind: tokHTMLText, text: s}, ""
	}
	return htmlToken{}, ""
}

// skipRawElement returns the input positioned just past the closing tag of a
// raw-text element (script/style).
func skipRawElement(s, name string) string {
	marker := "</" + strings.ToLower(name)
	lower := strings.ToLower(s)
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	end := strings.IndexByte(s[idx:], '>')
	if end < 0 {
		return ""
	}
	return s[idx+end+1:]
}

// ---------------------------------------------------------------------------
// Converter
// ---------------------------------------------------------------------------

var skippedElements = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"svg":      true,
	"nav":      true,
	"header":   true,
	"footer":   true,
	"aside":    true,
}

type listState struct {
	ordered bool
	index   int
}

type linkState struct {
	href   string
	active bool
}

type mdTable struct {
	rows [][]string
}

type mdConverter struct {
	bufs  []*strings.Builder
	title strings.Builder
	base  *url.URL

	// Whitespace state: a pending space between inline tokens.
	pendingSpace bool

	// Skip state for elements whose content must not appear in the output.
	skipName  string
	skipDepth int

	// Preformatted code block state.
	preActive bool
	preLang   string
	preBuf    strings.Builder

	// Inline code capture.
	inlineCode int

	links []linkState
	lists []*listState

	// Table state.
	table    *mdTable
	inCell   int
	tableRow []string
}

func (c *mdConverter) buf() *strings.Builder {
	return c.bufs[len(c.bufs)-1]
}

func (c *mdConverter) pushBuffer() {
	c.bufs = append(c.bufs, &strings.Builder{})
}

func (c *mdConverter) popBuffer() string {
	last := len(c.bufs) - 1
	s := c.bufs[last].String()
	c.bufs = c.bufs[:last]
	return s
}

func (c *mdConverter) write(s string) {
	c.buf().WriteString(s)
}

func trimTrailingSpaces(b *strings.Builder) {
	s := b.String()
	trimmed := strings.TrimRight(s, " \t")
	if len(trimmed) != len(s) {
		b.Reset()
		b.WriteString(trimmed)
	}
}

func trailingNewlines(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		n++
	}
	return n
}

// blockStart ensures content begins in a new Markdown block. Inside list items
// a single newline is enough; elsewhere a blank line separates blocks.
func (c *mdConverter) blockStart() {
	c.pendingSpace = false
	b := c.buf()
	trimTrailingSpaces(b)
	s := b.String()
	if s == "" {
		return
	}
	want := 2
	if len(c.lists) > 0 {
		want = 1
	}
	if have := trailingNewlines(s); have < want {
		b.WriteString(strings.Repeat("\n", want-have))
	}
}

func (c *mdConverter) flushSpace() {
	if !c.pendingSpace {
		return
	}
	c.pendingSpace = false
	b := c.buf()
	if b.Len() == 0 {
		return
	}
	s := b.String()
	last := s[len(s)-1]
	if last == ' ' || last == '\t' || last == '\n' {
		return
	}
	b.WriteByte(' ')
}

func (c *mdConverter) handleText(text string) {
	if c.skipName != "" {
		if c.skipName == "title" {
			c.title.WriteString(html.UnescapeString(text))
		}
		return
	}
	if c.preActive {
		c.preBuf.WriteString(html.UnescapeString(text))
		return
	}
	if c.table != nil && c.inCell == 0 {
		return
	}

	text = html.UnescapeString(text)
	for _, r := range text {
		if unicode.IsSpace(r) {
			c.pendingSpace = true
			continue
		}
		c.flushSpace()
		c.buf().WriteRune(r)
	}
}

func (c *mdConverter) handleStart(tok htmlToken) {
	// Raw preformatted content: preserve text, ignore markup except <br>.
	if c.preActive {
		switch tok.name {
		case "br":
			c.preBuf.WriteByte('\n')
		case "code":
			if c.preLang == "" {
				c.preLang = codeLanguage(tok.attrs)
			}
		}
		return
	}

	// Inside a table cell only inline content is meaningful.
	if c.inCell > 0 {
		if tok.name == "br" {
			c.buf().WriteByte(' ')
		}
		return
	}

	// Inline code swallows markup.
	if c.inlineCode > 0 {
		return
	}

	// script/style bodies are skipped by the tokenizer itself (it jumps to
	// the raw closing tag), so they must not engage the skip stack. Doing so
	// would leave the skip active after the raw closing tag is consumed.
	if tok.name == "script" || tok.name == "style" {
		return
	}

	// Content inside a skipped element is ignored. Nested occurrences of the
	// same element are tracked so the skip ends at the right place.
	if c.skipName != "" {
		if !tok.selfClosing && tok.name == c.skipName {
			c.skipDepth++
		}
		return
	}
	if skippedElements[tok.name] {
		if !tok.selfClosing {
			c.skipName = tok.name
			c.skipDepth = 1
		}
		return
	}
	if tok.name == "title" {
		if !tok.selfClosing {
			c.skipName = "title"
			c.skipDepth = 1
		}
		return
	}

	switch tok.name {
	case "pre":
		c.pendingSpace = false
		c.preActive = true
		c.preLang = ""
		c.preBuf.Reset()
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(tok.name[1] - '0')
		c.blockStart()
		c.write(strings.Repeat("#", level) + " ")
	case "p", "div", "section", "article", "main", "figure", "figcaption", "details", "summary":
		// Separators are emitted when the block closes; this avoids leading
		// blank lines at the start of the document.
	case "ul", "ol":
		c.blockStart()
		c.lists = append(c.lists, &listState{ordered: tok.name == "ol"})
	case "li":
		c.handleListItemStart()
	case "blockquote":
		c.blockStart()
		c.pushBuffer()
	case "hr":
		c.blockStart()
		c.write("---")
		c.blockStart()
	case "br":
		c.pendingSpace = false
		trimTrailingSpaces(c.buf())
		c.buf().WriteByte('\n')
	case "strong", "b":
		c.flushSpace()
		c.write("**")
		c.pendingSpace = false
	case "em", "i":
		c.flushSpace()
		c.write("*")
		c.pendingSpace = false
	case "code":
		c.flushSpace()
		c.pushBuffer()
		c.inlineCode = 1
		c.pendingSpace = false
	case "a":
		c.flushSpace()
		href := c.resolveURL(tok.attrs["href"])
		active := href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:")
		c.links = append(c.links, linkState{href: href, active: active})
		if active {
			c.write("[")
			c.pendingSpace = false
		}
	case "img":
		src := c.resolveURL(tok.attrs["src"])
		if src == "" {
			return
		}
		c.flushSpace()
		alt := strings.Join(strings.Fields(tok.attrs["alt"]), " ")
		c.write("![" + alt + "](" + src + ")")
		c.pendingSpace = false
	case "table":
		c.blockStart()
		if c.table == nil {
			c.table = &mdTable{}
		}
	case "tr":
		if c.table != nil {
			c.tableRow = nil
		}
	case "td", "th":
		if c.table != nil {
			c.pendingSpace = false
			c.pushBuffer()
			c.inCell = 1
		}
	}
}

func (c *mdConverter) handleEnd(tok htmlToken) {
	if c.preActive {
		if tok.name == "pre" {
			c.finishPre()
		}
		return
	}

	if c.inCell > 0 {
		if tok.name == "td" || tok.name == "th" {
			content := c.popBuffer()
			c.inCell = 0
			cell := strings.Join(strings.Fields(content), " ")
			c.tableRow = append(c.tableRow, cell)
		}
		return
	}

	if c.inlineCode > 0 {
		if tok.name == "code" {
			c.finishInlineCode()
		}
		return
	}

	if c.skipName != "" {
		if tok.name == c.skipName {
			c.skipDepth--
			if c.skipDepth <= 0 {
				if c.skipName == "title" {
					title := strings.Join(strings.Fields(c.title.String()), " ")
					c.title.Reset()
					c.title.WriteString(title)
				}
				c.skipName = ""
			}
		}
		return
	}

	switch tok.name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.blockStart()
	case "p", "div", "section", "article", "main", "figure", "figcaption", "details", "summary":
		c.blockStart()
	case "ul", "ol":
		if len(c.lists) > 0 {
			c.lists = c.lists[:len(c.lists)-1]
		}
		c.blockStart()
	case "li":
		c.handleListItemEnd()
	case "blockquote":
		c.finishBlockquote()
	case "strong", "b":
		c.write("**")
		c.pendingSpace = false
	case "em", "i":
		c.write("*")
		c.pendingSpace = false
	case "a":
		if len(c.links) > 0 {
			link := c.links[len(c.links)-1]
			c.links = c.links[:len(c.links)-1]
			if link.active {
				c.write("](" + link.href + ")")
			}
			c.pendingSpace = false
		}
	case "tr":
		if c.table != nil {
			if c.inCell > 0 {
				// Defensive: a malformed row without </td>.
				c.handleEnd(htmlToken{kind: tokHTMLEnd, name: "td"})
			}
			if len(c.tableRow) > 0 {
				c.table.rows = append(c.table.rows, c.tableRow)
				c.tableRow = nil
			}
		}
	case "table":
		c.finishTable()
	}
}

func (c *mdConverter) handleListItemStart() {
	c.pendingSpace = false
	b := c.buf()
	trimTrailingSpaces(b)
	s := b.String()
	if s != "" && !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
	depth := len(c.lists) - 1
	if depth < 0 {
		depth = 0
	}
	b.WriteString(strings.Repeat("  ", depth))
	if len(c.lists) > 0 && c.lists[len(c.lists)-1].ordered {
		state := c.lists[len(c.lists)-1]
		state.index++
		b.WriteString(strconv.Itoa(state.index) + ". ")
	} else {
		b.WriteString("- ")
	}
}

func (c *mdConverter) handleListItemEnd() {
	c.pendingSpace = false
	trimTrailingSpaces(c.buf())
}

func (c *mdConverter) finishInlineCode() {
	content := strings.TrimSpace(c.popBuffer())
	c.inlineCode = 0
	if content == "" {
		return
	}
	content = strings.Join(strings.Fields(content), " ")
	fence := "`"
	if strings.Contains(content, "`") {
		fence = "``"
	}
	c.write(fence + content + fence)
	c.pendingSpace = false
}

func codeLanguage(attrs map[string]string) string {
	classes := strings.Fields(attrs["class"])
	for _, class := range classes {
		if strings.HasPrefix(class, "language-") {
			return sanitizeLanguage(strings.TrimPrefix(class, "language-"))
		}
	}
	for _, class := range classes {
		if strings.HasPrefix(class, "lang-") {
			return sanitizeLanguage(strings.TrimPrefix(class, "lang-"))
		}
	}
	if lang := attrs["data-lang"]; lang != "" {
		return sanitizeLanguage(lang)
	}
	return ""
}

func sanitizeLanguage(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '-', r == '_', r == '#', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *mdConverter) finishPre() {
	content := c.preBuf.String()
	c.preActive = false
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.Trim(content, "\n")
	if strings.TrimSpace(content) == "" {
		return
	}
	fence := "```"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	c.blockStart()
	c.write(fence + c.preLang + "\n" + content + "\n" + fence)
	c.blockStart()
}

func (c *mdConverter) finishBlockquote() {
	content := c.popBuffer()
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + line
	}
	c.blockStart()
	c.write(strings.Join(lines, "\n"))
	c.blockStart()
}

func (c *mdConverter) finishTable() {
	if c.table == nil {
		return
	}
	rows := c.table.rows
	c.table = nil
	c.tableRow = nil
	if len(rows) == 0 {
		return
	}

	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return
	}

	// Normalize row widths and sanitize cell content.
	for i, row := range rows {
		normalized := make([]string, cols)
		for j := 0; j < cols; j++ {
			if j < len(row) && row[j] != "" {
				normalized[j] = strings.ReplaceAll(row[j], "|", `\|`)
			}
		}
		rows[i] = normalized
	}

	widths := make([]int, cols)
	for _, row := range rows {
		for j, cell := range row {
			if n := len([]rune(cell)); n > widths[j] {
				widths[j] = n
			}
		}
	}
	for j := range widths {
		if widths[j] < 3 {
			widths[j] = 3
		}
	}

	formatRow := func(row []string) string {
		var b strings.Builder
		b.WriteString("|")
		for j, cell := range row {
			b.WriteString(" ")
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[j]-len([]rune(cell))))
			b.WriteString(" |")
		}
		return b.String()
	}

	var out []string
	out = append(out, formatRow(rows[0]))
	var sep strings.Builder
	sep.WriteString("|")
	for j := 0; j < cols; j++ {
		sep.WriteString(" ")
		sep.WriteString(strings.Repeat("-", widths[j]))
		sep.WriteString(" |")
	}
	out = append(out, sep.String())
	for _, row := range rows[1:] {
		out = append(out, formatRow(row))
	}

	c.blockStart()
	c.write(strings.Join(out, "\n"))
	c.blockStart()
}

func (c *mdConverter) resolveURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if c.base == nil {
		return raw
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return c.base.ResolveReference(ref).String()
}

func (c *mdConverter) finish() string {
	if c.preActive {
		c.finishPre()
	}
	for len(c.bufs) > 1 {
		// Unclosed blockquote: fold the captured content into its parent
		// rather than dropping it.
		content := c.popBuffer()
		c.blockStart()
		c.write(strings.TrimSpace(content))
	}
	return normalizeMarkdown(c.bufs[0].String())
}

// normalizeMarkdown normalizes line endings, trims trailing whitespace, and
// collapses runs of blank lines to a single blank line. The returned string
// has no leading or trailing blank lines.
func normalizeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			continue
		}
		if blank > 0 && len(out) > 0 {
			out = append(out, "")
		}
		blank = 0
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
