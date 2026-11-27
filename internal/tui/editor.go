package tui

import (
	"strings"

	"github.com/tern-agent/tern/internal/provider"
)

// editor is a small multi-line text editor. It intentionally owns no terminal
// state: all key handling goes through explicit methods so it stays easy to
// test and never blocks the Bubble Tea event loop.
type editor struct {
	text   []rune
	cursor int

	history   []string
	histIndex int
	draft     string
	// historyBrowsing is true while the buffer mirrors a recalled history
	// entry. historyCursorMoved records a manual cursor move so Up/Down can
	// resume normal multi-line navigation instead of stepping through history.
	historyBrowsing    bool
	historyCursorMoved bool

	search searchState

	suggestions     []string
	suggestionIndex int
	suggestionToken string
	ghost           string

	attachments []provider.ContentPart
}

type searchState struct {
	active bool
	query  string
	origin string
	index  int
}

func newEditor() editor {
	return editor{histIndex: 0}
}

// Value returns the current buffer.
func (e *editor) Value() string { return string(e.text) }

// SetValue replaces the buffer and places the cursor at the end.
func (e *editor) SetValue(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
	e.resetHistoryNav()
	e.suggestions = nil
	e.ghost = ""
}

// Reset clears the buffer, attachments and completion state.
func (e *editor) Reset() {
	e.text = nil
	e.cursor = 0
	e.attachments = nil
	e.suggestions = nil
	e.suggestionIndex = 0
	e.suggestionToken = ""
	e.ghost = ""
	e.resetHistoryNav()
}

// SetGhost sets the inline muted completion suffix rendered after the cursor.
func (e *editor) SetGhost(s string) { e.ghost = s }

// Ghost returns the current inline completion suffix.
func (e *editor) Ghost() string { return e.ghost }

// IsEmpty reports whether the buffer only contains whitespace.
func (e *editor) IsEmpty() bool { return strings.TrimSpace(string(e.text)) == "" }

// Cursor returns the rune offset of the cursor.
func (e *editor) Cursor() int { return e.cursor }

// Len returns the rune length of the buffer.
func (e *editor) Len() int { return len(e.text) }

// LineCount returns the number of logical lines.
func (e *editor) LineCount() int {
	return strings.Count(string(e.text), "\n") + 1
}

// CursorLine returns the zero-based logical line of the cursor.
func (e *editor) CursorLine() int {
	line := 0
	for i := 0; i < e.cursor && i < len(e.text); i++ {
		if e.text[i] == '\n' {
			line++
		}
	}
	return line
}

// InsertRunes inserts text at the cursor.
func (e *editor) InsertRunes(s string) {
	if s == "" {
		return
	}
	runes := []rune(strings.ReplaceAll(s, "\r\n", "\n"))
	e.text = append(e.text[:e.cursor], append(runes, e.text[e.cursor:]...)...)
	e.cursor += len(runes)
	e.resetHistoryNav()
	e.suggestions = nil
}

// InsertNewline inserts a newline at the cursor.
func (e *editor) InsertNewline() { e.InsertRunes("\n") }

// Backspace deletes the rune before the cursor.
func (e *editor) Backspace() {
	if e.cursor <= 0 {
		return
	}
	e.text = append(e.text[:e.cursor-1], e.text[e.cursor:]...)
	e.cursor--
	e.resetHistoryNav()
	e.suggestions = nil
}

// DeleteForward deletes the rune under the cursor.
func (e *editor) DeleteForward() {
	if e.cursor >= len(e.text) {
		return
	}
	e.text = append(e.text[:e.cursor], e.text[e.cursor+1:]...)
	e.resetHistoryNav()
	e.suggestions = nil
}

// noteManualCursorMove marks that the operator moved the cursor after recalling
// a history entry, so vertical keys resume normal line navigation.
func (e *editor) noteManualCursorMove() {
	if e.historyBrowsing {
		e.historyCursorMoved = true
	}
}

// MoveLeft moves one rune left.
func (e *editor) MoveLeft() {
	if e.cursor > 0 {
		e.cursor--
	}
	e.noteManualCursorMove()
}

// MoveRight moves one rune right.
func (e *editor) MoveRight() {
	if e.cursor < len(e.text) {
		e.cursor++
	}
	e.noteManualCursorMove()
}

// MoveLineStart moves to the start of the current line.
func (e *editor) MoveLineStart() {
	for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
		e.cursor--
	}
	e.noteManualCursorMove()
}

// MoveLineEnd moves to the end of the current line.
func (e *editor) MoveLineEnd() {
	for e.cursor < len(e.text) && e.text[e.cursor] != '\n' {
		e.cursor++
	}
	e.noteManualCursorMove()
}

// MoveUp moves the cursor up one line, preferring history navigation when the
// cursor is already on the first line. While browsing a recalled multi-line
// entry, an untouched cursor steps straight to the previous history entry just
// like zsh.
func (e *editor) MoveUp() bool {
	if e.historyBrowsing && !e.historyCursorMoved {
		return e.HistoryPrev()
	}
	isFirstLine := true
	for i := 0; i < e.cursor && i < len(e.text); i++ {
		if e.text[i] == '\n' {
			isFirstLine = false
			break
		}
	}
	if isFirstLine {
		return e.HistoryPrev()
	}
	col := e.cursor - e.lineStart()
	e.cursor = e.lineStart()
	if e.cursor > 0 {
		e.cursor-- // move onto the newline
		e.cursor = e.lineStart()
	}
	e.cursor = clampInt(e.cursor+col, e.lineStart(), e.lineEnd())
	e.noteManualCursorMove()
	return false
}

// MoveDown moves the cursor down one line, preferring history navigation when
// the cursor is on the last line. While browsing an untouched recalled entry it
// steps forward through history immediately.
func (e *editor) MoveDown() bool {
	if e.historyBrowsing && !e.historyCursorMoved {
		return e.HistoryNext()
	}
	if e.CursorLine() >= e.LineCount()-1 {
		return e.HistoryNext()
	}
	col := e.cursor - e.lineStart()
	end := e.lineEnd()
	if end >= len(e.text) {
		return false
	}
	e.cursor = end + 1
	e.cursor = clampInt(e.cursor+col, e.lineStart(), e.lineEnd())
	e.noteManualCursorMove()
	return false
}

// DeleteWordBackward deletes the previous word.
func (e *editor) DeleteWordBackward() {
	if e.cursor == 0 {
		return
	}
	i := e.cursor
	for i > 0 && isSpaceRune(e.text[i-1]) {
		i--
	}
	for i > 0 && !isSpaceRune(e.text[i-1]) {
		i--
	}
	e.text = append(e.text[:i], e.text[e.cursor:]...)
	e.cursor = i
	e.resetHistoryNav()
}

// DeleteToLineStart deletes from the cursor back to the start of the line.
func (e *editor) DeleteToLineStart() {
	start := e.lineStart()
	e.text = append(e.text[:start], e.text[e.cursor:]...)
	e.cursor = start
	e.resetHistoryNav()
}

// DeleteToLineEnd deletes from the cursor to the end of the line.
func (e *editor) DeleteToLineEnd() {
	end := e.lineEnd()
	e.text = append(e.text[:e.cursor], e.text[end:]...)
	e.resetHistoryNav()
}

func (e *editor) lineStart() int {
	i := e.cursor
	for i > 0 && e.text[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd() int {
	i := e.cursor
	for i < len(e.text) && e.text[i] != '\n' {
		i++
	}
	return i
}

// AddHistory records a submitted prompt. Consecutive duplicates are ignored.
// It reports whether a new entry was actually appended.
func (e *editor) AddHistory(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	added := false
	if n := len(e.history); n == 0 || e.history[n-1] != s {
		e.history = append(e.history, s)
		if len(e.history) > 500 {
			e.history = e.history[len(e.history)-500:]
		}
		added = true
	}
	e.histIndex = len(e.history)
	e.draft = ""
	e.historyBrowsing = false
	e.historyCursorMoved = false
	return added
}

// SetHistory replaces the in-memory history (used when loading the persisted
// terminal-style history file).
func (e *editor) SetHistory(items []string) {
	e.history = e.history[:0]
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if n := len(e.history); n > 0 && e.history[n-1] == item {
			continue
		}
		e.history = append(e.history, item)
	}
	if len(e.history) > 500 {
		e.history = e.history[len(e.history)-500:]
	}
	e.resetHistoryNav()
}

func (e *editor) resetHistoryNav() {
	e.histIndex = len(e.history)
	e.draft = ""
	e.historyBrowsing = false
	e.historyCursorMoved = false
}

// HistoryPrev recalls the previous history entry. It reports whether history
// handled the key.
func (e *editor) HistoryPrev() bool {
	if len(e.history) == 0 {
		return false
	}
	if e.histIndex == len(e.history) {
		e.draft = string(e.text)
	}
	if e.histIndex > 0 {
		e.histIndex--
	}
	e.setTextFromHistory(e.history[e.histIndex])
	e.historyBrowsing = true
	e.historyCursorMoved = false
	return true
}

// HistoryNext recalls the next history entry or restores the draft.
func (e *editor) HistoryNext() bool {
	if len(e.history) == 0 || e.histIndex >= len(e.history) {
		return false
	}
	e.histIndex++
	if e.histIndex == len(e.history) {
		e.setTextFromHistory(e.draft)
		e.historyBrowsing = false
		e.historyCursorMoved = false
	} else {
		e.setTextFromHistory(e.history[e.histIndex])
		e.historyBrowsing = true
		e.historyCursorMoved = false
	}
	return true
}

func (e *editor) setTextFromHistory(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
	e.suggestions = nil
}

// History returns the recorded prompts.
func (e *editor) History() []string { return append([]string(nil), e.history...) }

// StartSearch begins incremental reverse history search (Ctrl+R).
func (e *editor) StartSearch() {
	if e.search.active {
		e.NextSearch()
		return
	}
	e.search.active = true
	e.search.query = ""
	e.search.origin = string(e.text)
	e.search.index = len(e.history)
}

// SearchActive reports whether reverse search is active.
func (e *editor) SearchActive() bool { return e.search.active }

// SearchQuery returns the current reverse-search query.
func (e *editor) SearchQuery() string { return e.search.query }

// UpdateSearch changes the query and selects the newest matching history item.
func (e *editor) UpdateSearch(change string) {
	e.search.query += change
	e.search.index = len(e.history)
	e.findSearchMatch()
}

// SearchBackspace removes the last query rune.
func (e *editor) SearchBackspace() {
	if e.search.query == "" {
		return
	}
	r := []rune(e.search.query)
	e.search.query = string(r[:len(r)-1])
	e.search.index = len(e.history)
	e.findSearchMatch()
}

// NextSearch selects the next older matching history item.
func (e *editor) NextSearch() {
	if !e.search.active {
		return
	}
	e.search.index--
	e.findSearchMatch()
}

func (e *editor) findSearchMatch() {
	if e.search.index > len(e.history) {
		e.search.index = len(e.history)
	}
	q := strings.ToLower(e.search.query)
	for i := e.search.index - 1; i >= 0; i-- {
		if q == "" || strings.Contains(strings.ToLower(e.history[i]), q) {
			e.search.index = i
			e.text = []rune(e.history[i])
			e.cursor = len(e.text)
			return
		}
	}
	// No match: keep the previous text so the user can keep typing.
}

// AcceptSearch keeps the selected history entry.
func (e *editor) AcceptSearch() {
	e.search = searchState{}
	e.resetHistoryNav()
}

// CancelSearch restores the buffer that was active before the search started.
func (e *editor) CancelSearch() {
	if e.search.active {
		e.text = []rune(e.search.origin)
		e.cursor = len(e.text)
	}
	e.search = searchState{}
	e.resetHistoryNav()
}

// TokenAtCursor returns the start and end offsets and text of the token under
// the cursor. Tokens are delimited by whitespace.
func (e *editor) TokenAtCursor() (int, int, string) {
	start := e.cursor
	for start > 0 && !isSpaceRune(e.text[start-1]) {
		start--
	}
	end := e.cursor
	for end < len(e.text) && !isSpaceRune(e.text[end]) {
		end++
	}
	return start, end, string(e.text[start:end])
}

// ReplaceRange replaces [start,end) with repl and places the cursor after it.
func (e *editor) ReplaceRange(start, end int, repl string) {
	if start < 0 {
		start = 0
	}
	if end > len(e.text) {
		end = len(e.text)
	}
	if start > end {
		start = end
	}
	runes := []rune(repl)
	next := make([]rune, 0, len(e.text)-(end-start)+len(runes))
	next = append(next, e.text[:start]...)
	next = append(next, runes...)
	next = append(next, e.text[end:]...)
	e.text = next
	e.cursor = start + len(runes)
	e.suggestions = nil
	e.resetHistoryNav()
}

// SetSuggestions stores completion candidates for display.
func (e *editor) SetSuggestions(token string, items []string, index int) {
	e.suggestionToken = token
	e.suggestions = items
	e.suggestionIndex = index
}

// ClearSuggestions removes completion candidates.
func (e *editor) ClearSuggestions() {
	e.suggestions = nil
	e.suggestionIndex = 0
	e.suggestionToken = ""
}

// Suggestions returns the current candidates.
func (e *editor) Suggestions() []string { return e.suggestions }

// AddAttachment records an image attachment.
func (e *editor) AddAttachment(part provider.ContentPart) {
	e.attachments = append(e.attachments, part)
}

// TakeAttachments returns and clears pending attachments.
func (e *editor) TakeAttachments() []provider.ContentPart {
	out := e.attachments
	e.attachments = nil
	return out
}

// View renders the editor, including reverse-search status and completion
// candidates.
func (e *editor) View(width int, st styles) string {
	if width < 8 {
		width = 8
	}
	var lines []string
	if e.search.active {
		label := "(reverse-i-search)`" + e.search.query + "`: "
		lines = append(lines, st.paint(st.info, label)+string(e.text))
	}

	promptPrefix := "❯ "
	if st.plain {
		promptPrefix = "> "
	}
	contPrefix := "  "
	available := width - displayWidth(promptPrefix)
	if available < 4 {
		available = 4
	}

	// Empty editor: render placeholder cleanly on row 0.
	if len(e.text) == 0 {
		ph := st.paint(st.placeholder, "Ask tern anything… (/help for commands)")
		if st.plain {
			ph = "Ask tern anything... (/help for commands)"
		}
		lines = append(lines, st.paint(st.prompt, promptPrefix)+ph)
	} else {
		// Soft-wrap every logical line into visual rows so long paragraphs wrap
		// naturally like a web textarea instead of scrolling off-screen horizontally.
		type visualRow struct {
			text       string
			isFirst    bool
			hasCursor  bool
			cursorCol  int
			atLineEnd  bool
		}
		var vrows []visualRow
		curVisualRow := 0

		rawLines := strings.Split(string(e.text), "\n")
		lineStarts := lineStartOffsets(e.text)
		curLogLine := e.CursorLine()
		curLogCol := e.cursor - lineStarts[curLogLine]

		for li, raw := range rawLines {
			runes := []rune(raw)
			wrapped := softWrapRunes(runes, available)
			offset := 0
			for wi, seg := range wrapped {
				segLen := len(seg)
				hasCur := false
				cCol := 0
				if li == curLogLine {
					isLastSeg := wi == len(wrapped)-1
					if curLogCol >= offset && (curLogCol < offset+segLen || (isLastSeg && curLogCol == offset+segLen)) {
						hasCur = true
						cCol = curLogCol - offset
						curVisualRow = len(vrows)
					}
				}
				vrows = append(vrows, visualRow{
					text:      string(seg),
					isFirst:   li == 0 && wi == 0,
					hasCursor: hasCur,
					cursorCol: cCol,
					atLineEnd: li == curLogLine && wi == len(wrapped)-1 && curLogCol == len(runes),
				})
				offset += segLen
			}
		}

		// Keep the editor from crowding out the transcript on very long prompts.
		startRow := 0
		if len(vrows) > maxEditorRows {
			startRow = curVisualRow - maxEditorRows/2
			if startRow < 0 {
				startRow = 0
			}
			if startRow+maxEditorRows > len(vrows) {
				startRow = len(vrows) - maxEditorRows
			}
			if startRow > 0 {
				lines = append(lines, st.paint(st.dim, "  …"))
			}
			vrows = vrows[startRow : startRow+maxEditorRows]
		}

		for _, vr := range vrows {
			prefix := contPrefix
			if vr.isFirst {
				prefix = promptPrefix
			}
			display := vr.text
			if vr.hasCursor {
				display = insertCursor(vr.text, vr.cursorCol, st)
				if e.ghost != "" && vr.atLineEnd {
					rem := available - displayWidth(display)
					if rem > 0 {
						ghost := truncateCell(e.ghost, rem)
						if ghost != "" {
							display += st.paint(st.placeholder, ghost)
						}
					}
				}
			}
			lines = append(lines, st.paint(st.prompt, prefix)+display)
		}
	}

	if len(e.suggestions) > 0 {
		max := len(e.suggestions)
		if max > 6 {
			max = 6
		}
		for i := 0; i < max; i++ {
			item := e.suggestions[i]
			marker := "  "
			style := st.dim
			if i == e.suggestionIndex {
				marker = "▸ "
				style = st.selected
				if st.plain {
					marker = "> "
				}
			}
			lines = append(lines, marker+st.paint(style, item))
		}
		if len(e.suggestions) > max {
			lines = append(lines, st.paint(st.dim, "  …"))
		}
	}
	return strings.Join(lines, "\n")
}

func softWrapRunes(runes []rune, maxWidth int) [][]rune {
	if maxWidth < 2 || len(runes) == 0 {
		return [][]rune{runes}
	}
	var out [][]rune
	start := 0
	for start < len(runes) {
		w := 0
		lastSpace := -1
		end := start
		for end < len(runes) {
			rw := displayWidth(string(runes[end]))
			if rw < 1 {
				rw = 1
			}
			// Reserve 1 cell for the cursor marker when at the end of a segment.
			if w+rw > maxWidth-1 && end > start {
				break
			}
			if runes[end] == ' ' || runes[end] == '\t' {
				lastSpace = end
			}
			w += rw
			end++
		}
		if end < len(runes) && lastSpace > start {
			end = lastSpace + 1
		}
		out = append(out, runes[start:end])
		start = end
	}
	if len(out) == 0 {
		return [][]rune{{}}
	}
	return out
}

func insertCursor(line string, col int, st styles) string {
	runes := []rune(line)
	if col < 0 {
		col = 0
	}
	if col > len(runes) {
		col = len(runes)
	}
	marker := "▌"
	if st.plain {
		marker = "|"
	}
	before := string(runes[:col])
	after := string(runes[col:])
	return before + marker + after
}

func truncateAround(line string, col, width int, active bool) string {
	if displayWidth(line) <= width {
		return line
	}
	if !active {
		head, _ := splitWidth(line, width)
		return head
	}
	// Keep the cursor visible by shifting the window.
	runes := []rune(line)
	start := col - width/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

func lineStartOffsets(runes []rune) []int {
	starts := []int{0}
	for i, r := range runes {
		if r == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
