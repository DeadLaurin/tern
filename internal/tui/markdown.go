package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// MarkdownOptions controls RenderMarkdownWithOptions.
type MarkdownOptions struct {
	// Width is the wrapping width in terminal cells. Zero means no wrapping.
	Width int
	// NoColor disables all styling and uses ASCII decorations only.
	NoColor bool
}

// RenderMarkdown renders streaming Markdown as terminal text. It is safe to
// call on a partial document: an unterminated fenced code block is rendered as
// code until the closing fence arrives.
func RenderMarkdown(input string, width int, plain bool) string {
	return RenderMarkdownWithOptions(input, MarkdownOptions{Width: width, NoColor: plain})
}

// RenderMarkdownWithOptions renders Markdown with explicit options.
func RenderMarkdownWithOptions(input string, opts MarkdownOptions) string {
	r := markdownRenderer{st: newStyles(opts.NoColor), width: opts.Width}
	return r.render(input)
}

type markdownRenderer struct {
	st    styles
	width int
}

func (r *markdownRenderer) render(input string) string {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var out []string
	inCode := false
	lang := ""
	prevBlank := true

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				inCode = true
				lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				out = append(out, r.codeFenceOpen(lang))
			} else {
				inCode = false
				lang = ""
				out = append(out, r.codeFenceClose())
			}
			prevBlank = false
			continue
		}
		if inCode {
			out = append(out, r.highlightCodeLine(lang, raw))
			prevBlank = false
			continue
		}

		if trimmed == "" {
			// Collapse consecutive blank lines while streaming.
			if !prevBlank {
				out = append(out, "")
			}
			prevBlank = true
			continue
		}
		prevBlank = false

		switch {
		case isHeading(trimmed):
			level, text := headingParts(trimmed)
			style := r.st.heading
			if level > 2 {
				style = r.st.bold
			}
			rendered := r.paint(style, r.inline(text))
			out = append(out, r.wrapRendered(rendered, text, style)...)
		case isHorizontalRule(trimmed):
			if r.st.plain {
				out = append(out, "---")
			} else {
				out = append(out, r.paint(r.st.dim, strings.Repeat("─", minInt(r.width, 40))))
			}
		case strings.HasPrefix(trimmed, ">"):
			text := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			marker := "> "
			if !r.st.plain {
				marker = "│ "
			}
			out = append(out, r.prefixed(marker, r.inline(text), r.st.quote)...)
		case isListItem(trimmed):
			marker, text := splitListItem(trimmed)
			if r.st.plain {
				marker = "- "
			}
			out = append(out, r.prefixed("  "+marker, r.inline(text), r.st.list)...)
		default:
			out = append(out, r.wrapRendered(r.inline(trimmed), trimmed, lipgloss.NewStyle())...)
		}
	}

	return strings.Join(out, "\n")
}

func (r *markdownRenderer) paint(style lipgloss.Style, text string) string {
	return r.st.paint(style, text)
}

func (r *markdownRenderer) codeFenceOpen(lang string) string {
	if r.st.plain {
		if lang != "" {
			return "```" + lang
		}
		return "```"
	}
	label := strings.TrimSpace(lang)
	if label == "" {
		label = "code"
	}
	return r.paint(r.st.dim, "  ┌─ "+label)
}

func (r *markdownRenderer) codeFenceClose() string {
	if r.st.plain {
		return "```"
	}
	return r.paint(r.st.dim, "  └─")
}

func (r *markdownRenderer) highlightCodeLine(lang, line string) string {
	if r.st.plain {
		return "  " + line
	}
	return "  " + highlightCode(lang, line, r.st)
}

// inline renders bold, italic and inline code spans. It deliberately works on
// a single line so streaming output stays stable.
func (r *markdownRenderer) inline(line string) string {
	if r.st.plain || line == "" {
		return stripInlineMarkers(line)
	}
	var b strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); {
		switch {
		case runes[i] == '`':
			if end := indexRune(runes, i+1, '`'); end > i {
				b.WriteString(r.paint(r.st.inline, string(runes[i+1:end])))
				i = end + 1
				continue
			}
		case runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '*':
			if end := indexString(runes, i+2, "**"); end > i {
				b.WriteString(r.paint(r.st.bold, string(runes[i+2:end])))
				i = end + 2
				continue
			}
		case runes[i] == '*':
			if end := indexRune(runes, i+1, '*'); end > i && end-i > 1 {
				b.WriteString(r.paint(r.st.italic, string(runes[i+1:end])))
				i = end + 1
				continue
			}
		case runes[i] == '_' && (i == 0 || unicode.IsSpace(runes[i-1])):
			if end := indexRune(runes, i+1, '_'); end > i && end-i > 1 {
				b.WriteString(r.paint(r.st.italic, string(runes[i+1:end])))
				i = end + 1
				continue
			}
		case runes[i] == '[':
			// [text](url) becomes "text (url)".
			if close := indexRune(runes, i+1, ']'); close > i && close+1 < len(runes) && runes[close+1] == '(' {
				if end := indexRune(runes, close+2, ')'); end > close {
					text := string(runes[i+1 : close])
					url := string(runes[close+2 : end])
					b.WriteString(text)
					b.WriteString(r.paint(r.st.dim, " ("+url+")"))
					i = end + 1
					continue
				}
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

func (r *markdownRenderer) prefixed(marker, text string, style lipgloss.Style) []string {
	rendered := r.paint(style, marker) + text
	return r.wrapRendered(rendered, marker+stripANSI(text), style)
}

// wrapRendered wraps plain source text to the renderer width. An optional
// styled variant is used for the first line when the caller already rendered
// the text (for example headings); subsequent lines are rendered from source.
func (r *markdownRenderer) wrapRendered(rendered, plain string, style lipgloss.Style) []string {
	if r.width <= 0 {
		return []string{rendered}
	}
	plainWidth := displayWidth(plain)
	if plainWidth <= r.width {
		return []string{rendered}
	}
	words := strings.Fields(plain)
	if len(words) == 0 {
		return []string{rendered}
	}
	var lines []string
	var current string
	for _, word := range words {
		w := displayWidth(word)
		if current == "" {
			current = word
			continue
		}
		if displayWidth(current)+1+w <= r.width {
			current += " " + word
			continue
		}
		if current != "" {
			lines = append(lines, current)
		}
		// Hard split words wider than the viewport.
		for displayWidth(word) > r.width && r.width > 0 {
			head, tail := splitWidth(word, r.width)
			lines = append(lines, head)
			word = tail
		}
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return []string{rendered}
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if i == 0 {
			// Re-render the first line from source so styling is not broken by
			// wrapping at a boundary that did not exist before.
			out = append(out, r.paint(style, r.inline(line)))
			continue
		}
		out = append(out, r.paint(style, r.inline(line)))
	}
	return out
}

func highlightCode(lang, line string, st styles) string {
	if line == "" {
		return ""
	}
	runes := []rune(line)
	var b strings.Builder
	i := 0
	for i < len(runes) {
		ch := runes[i]

		// Comments. Only treat # as a comment for languages where that is
		// conventional, and only when it starts a token.
		if i == 0 || unicode.IsSpace(runes[i-1]) {
			if ch == '#' && hashComment(lang) {
				b.WriteString(st.comment.Render(string(runes[i:])))
				break
			}
			if ch == '/' && i+1 < len(runes) && runes[i+1] == '/' {
				b.WriteString(st.comment.Render(string(runes[i:])))
				break
			}
			if ch == '-' && i+1 < len(runes) && runes[i+1] == '-' && dashComment(lang) {
				b.WriteString(st.comment.Render(string(runes[i:])))
				break
			}
		}

		if ch == '"' || ch == '\'' || ch == '`' {
			end := i + 1
			for end < len(runes) {
				if runes[end] == '\\' && end+1 < len(runes) {
					end += 2
					continue
				}
				if runes[end] == ch {
					end++
					break
				}
				end++
			}
			b.WriteString(st.str.Render(string(runes[i:end])))
			i = end
			continue
		}

		if unicode.IsLetter(ch) || ch == '_' {
			end := i + 1
			for end < len(runes) && (unicode.IsLetter(runes[end]) || unicode.IsDigit(runes[end]) || runes[end] == '_') {
				end++
			}
			word := string(runes[i:end])
			if codeKeywords[word] {
				b.WriteString(st.keyword.Render(word))
			} else {
				b.WriteString(word)
			}
			i = end
			continue
		}

		if unicode.IsDigit(ch) {
			end := i + 1
			for end < len(runes) && (unicode.IsDigit(runes[end]) || runes[end] == '.' || runes[end] == 'x' || (runes[end] >= 'a' && runes[end] <= 'f')) {
				end++
			}
			b.WriteString(st.number.Render(string(runes[i:end])))
			i = end
			continue
		}

		b.WriteRune(ch)
		i++
	}
	return b.String()
}

var codeKeywords = map[string]bool{
	// Go and similar languages.
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true, "interface": true,
	"map": true, "package": true, "range": true, "return": true, "select": true,
	"struct": true, "switch": true, "type": true, "var": true,
	// Shared value/type words.
	"nil": true, "true": true, "false": true, "null": true, "undefined": true,
	"string": true, "int": true, "bool": true, "error": true, "byte": true, "rune": true,
	"float64": true, "float32": true, "any": true, "void": true,
	// JavaScript / TypeScript / Python / Rust / Java.
	"class": true, "function": true, "let": true, "async": true, "await": true,
	"export": true, "from": true, "of": true, "new": true, "this": true,
	"def": true, "elif": true, "except": true, "finally": true, "lambda": true,
	"None": true, "True": true, "False": true, "self": true, "with": true, "as": true,
	"fn": true, "impl": true, "pub": true, "use": true, "match": true, "mut": true,
	"public": true, "private": true, "static": true, "final": true,
}

func hashComment(lang string) bool {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "py", "python", "sh", "bash", "zsh", "fish", "ruby", "rb", "yaml", "yml", "toml", "makefile", "dockerfile", "perl", "r", "conf":
		return true
	}
	return false
}

func dashComment(lang string) bool {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "sql", "lua", "haskell":
		return true
	}
	return false
}

func isHeading(line string) bool {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	return n > 0 && n <= 6 && n < len(line) && line[n] == ' '
}

func headingParts(line string) (int, string) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	return n, strings.TrimSpace(line[n:])
}

func isHorizontalRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	ch := line[0]
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	for i := 1; i < len(line); i++ {
		if line[i] != ch && line[i] != ' ' {
			return false
		}
	}
	return strings.Count(line, string(ch)) >= 3
}

func isListItem(line string) bool {
	if len(line) < 2 {
		return false
	}
	if line[0] == '-' || line[0] == '*' || line[0] == '+' {
		return line[1] == ' '
	}
	// Ordered lists: "1. text".
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' '
}

func splitListItem(line string) (string, string) {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') {
		return line[:1] + " ", strings.TrimSpace(line[2:])
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i+1 < len(line) && (line[i] == '.' || line[i] == ')') {
		return line[:i+1] + " ", strings.TrimSpace(line[i+2:])
	}
	return "- ", line
}

func stripInlineMarkers(line string) string {
	// In plain mode we keep the text but drop the Markdown emphasis markers so
	// output remains readable.
	line = strings.ReplaceAll(line, "**", "")
	line = strings.ReplaceAll(line, "`", "")
	return line
}

func stripANSI(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1b {
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}

func displayWidth(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

func splitWidth(s string, width int) (string, string) {
	if width <= 0 {
		return s, ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s, ""
	}
	return string(runes[:width]), string(runes[width:])
}

func indexRune(runes []rune, from int, target rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

func indexString(runes []rune, from int, target string) int {
	t := []rune(target)
	if len(t) == 0 {
		return from
	}
	for i := from; i+len(t) <= len(runes); i++ {
		match := true
		for j := range t {
			if runes[i+j] != t[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
