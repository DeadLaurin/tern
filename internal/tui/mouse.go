package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// mouseAction identifies what a recorded click target does. The TUI renders a
// shallow layout without a widget tree, so views record their visible hit
// regions while they paint and the mouse handler resolves clicks against the
// latest geometry.
type mouseAction int

const (
	actionNone mouseAction = iota
	actionEntryReasoning
	actionStatusModel
	actionStatusYolo
	actionStatusPanel
	actionStatusBottom
	actionModalRow
	actionApprovalAllow
	actionApprovalAllowAll
	actionApprovalDeny
	actionAskOption
	actionAgentsPanelRow
	actionSidebarAgent
)

// entrySpan maps a rendered transcript line range [start, end) in document
// coordinates to the entry that produced it. end is exclusive.
type entrySpan struct {
	entry *transcriptEntry
	start int
	end   int
}

// statusHit is a horizontal span on the bottom status row.
type statusHit struct {
	start  int
	end    int
	action mouseAction
}

// modalRowHit is a selectable row in a centered list modal. line is relative to
// the modal frame's content body; View translates it to absolute coordinates.
type modalRowHit struct {
	line  int
	index int
}

// overlayHit is a button or option inside a centered prompt card. line and the
// horizontal span are relative to the card itself.
type overlayHit struct {
	line   int
	start  int
	end    int
	action mouseAction
	index  int
}

// resetClickTargets clears all geometry before a repaint so stale rows from a
// previous layout can never capture a click.
func (m *Model) resetClickTargets() {
	m.entrySpans = m.entrySpans[:0]
	m.sidebarAgentRows = make(map[int]string)
	m.statusHits = nil
	m.modalRows = nil
	m.modalAbsRows = nil
	m.overlayHits = nil
	m.agentsPanelRows = nil
	m.agentsPanelAbsRows = nil
}

// copyText copies one message and reports a compact confirmation in the status
// line. clipboardFn is injectable so tests never touch the real clipboard.
func (m *Model) copyText(text string) {
	if strings.TrimSpace(text) == "" {
		m.statusMsg = "Nothing to copy"
		m.dirty = true
		return
	}
	fn := m.clipboardFn
	if fn == nil {
		fn = copyToClipboard
	}
	if err := fn(text); err != nil {
		m.statusMsg = "Copy failed: " + err.Error()
		m.dirty = true
		return
	}
	m.statusMsg = fmt.Sprintf("Copied message (%d chars)", len([]rune(text)))
	m.dirty = true
}

// entryCopyText is the payload a right-click on an entry places on the
// clipboard. Tool entries prefer their output, then their arguments.
func entryCopyText(entry *transcriptEntry) string {
	if entry == nil {
		return ""
	}
	if entry.kind == entryTool {
		if strings.TrimSpace(entry.output) != "" {
			return entry.output
		}
		if strings.TrimSpace(entry.args) != "" && entry.args != "{}" && entry.args != "null" {
			return entry.args
		}
		return ""
	}
	return entry.text
}

// entryAtLine returns the transcript entry covering document line.
func (m *Model) entryAtLine(line int) *transcriptEntry {
	for i := range m.entrySpans {
		span := m.entrySpans[i]
		if line >= span.start && line < span.end {
			return span.entry
		}
	}
	return nil
}

// handleTranscriptMouse resolves clicks inside the chat viewport.
func (m *Model) handleTranscriptMouse(msg tea.MouseMsg) bool {
	if m.width <= 0 || m.height <= 0 || m.viewport.Height <= 0 {
		return false
	}
	if msg.Y < 0 || msg.Y >= m.viewport.Height {
		return false
	}
	if msg.X < m.viewportX || msg.X >= m.viewportX+m.viewport.Width {
		return false
	}
	docLine := msg.Y + m.viewport.YOffset
	entry := m.entryAtLine(docLine)
	if entry == nil {
		return false
	}
	switch msg.Button {
	case tea.MouseButtonRight:
		text := entryCopyText(entry)
		if strings.TrimSpace(text) == "" {
			return false
		}
		m.copyText(text)
		return true
	case tea.MouseButtonLeft:
		if entry.kind == entryReasoning {
			if m.thinking == thinkingExpanded {
				m.thinking = thinkingCollapsed
				m.statusMsg = "Thinking collapsed"
			} else {
				m.thinking = thinkingExpanded
				m.showToolDetail = true
				m.statusMsg = "Thinking expanded"
			}
			m.dirty = true
			return true
		}
	}
	return false
}

// handleStatusMouse resolves clicks on the bottom status bar.
func (m *Model) handleStatusMouse(msg tea.MouseMsg) bool {
	if msg.Y != m.statusY {
		return false
	}
	x := msg.X - m.statusX
	if x < 0 {
		return false
	}
	for _, hit := range m.statusHits {
		if x < hit.start || x >= hit.end {
			continue
		}
		switch hit.action {
		case actionStatusModel:
			m.openModelModal()
		case actionStatusYolo:
			m.toggleYolo()
		case actionStatusPanel:
			m.sidebarHidden = !m.sidebarHidden
			if m.sidebarHidden {
				m.statusMsg = "Sidebar hidden · Ctrl+B to show"
			} else {
				m.statusMsg = "Sidebar shown · Ctrl+B to hide"
			}
			m.dirty = true
		case actionStatusBottom:
			m.viewport.GotoBottom()
			m.followTail = true
			m.statusMsg = "Jumped to latest"
			m.dirty = true
		default:
			return false
		}
		return true
	}
	return false
}

// handleSidebarMouse opens the sub-agent passport for a click on its sidebar
// row. The whole sidebar to the right of the divider is accepted so the click
// target is forgiving.
func (m *Model) handleSidebarMouse(msg tea.MouseMsg) bool {
	if !m.twoPane() || msg.X < m.sidebarX {
		return false
	}
	id, ok := m.sidebarAgentRows[msg.Y]
	if !ok || id == "" {
		return false
	}
	for i, candidate := range m.agentOrder {
		if candidate == id {
			m.agentFocus = i
			m.panelIndex = i
			break
		}
	}
	m.openAgentPeek(id)
	return true
}

// handleOverlayMouse resolves clicks on approval buttons and ask options.
func (m *Model) handleOverlayMouse(msg tea.MouseMsg) bool {
	if len(m.overlayHits) == 0 {
		return false
	}
	localY := msg.Y - m.overlayTop
	localX := msg.X - m.overlayLeft
	for _, hit := range m.overlayHits {
		if localY != hit.line {
			continue
		}
		if localX < hit.start || localX >= hit.end {
			continue
		}
		switch hit.action {
		case actionApprovalAllow:
			m.resolveApproval(true)
		case actionApprovalAllowAll:
			m.turnAutoApprove = true
			m.resolveApproval(true)
		case actionApprovalDeny:
			m.resolveApproval(false)
		case actionAskOption:
			if m.pendingAsk != nil && hit.index >= 0 && hit.index < len(m.pendingAsk.options) {
				m.pendingAsk.selected = hit.index
				m.dirty = true
			}
		default:
			return false
		}
		return true
	}
	return false
}

// handleModalMouse selects a row in the active centered list modal.
func (m *Model) handleModalMouse(msg tea.MouseMsg) bool {
	if len(m.modalAbsRows) == 0 {
		return false
	}
	row, ok := m.modalAbsRows[msg.Y]
	if !ok {
		return false
	}
	m.modal.index = row
	m.clampActiveModalIndex()
	m.dirty = true
	return true
}

func (m *Model) clampActiveModalIndex() {
	switch m.activeModal {
	case modalModel:
		m.clampModelIndex()
	case modalSessions:
		m.clampSessionIndex()
	case modalStats:
		m.clampStatsIndex()
	case modalSkills:
		if n := len(m.modal.skills); n > 0 {
			m.modal.index = clampInt(m.modal.index, 0, n-1)
		}
	case modalMCP:
		if n := len(m.modal.mcp); n > 0 {
			m.modal.index = clampInt(m.modal.index, 0, n-1)
			m.refreshMCPStatus()
		}
	case modalHelp:
		m.clampHelpIndex()
	}
}

// handleAgentsPanelMouse selects a sub-agent row in the /agents panel.
func (m *Model) handleAgentsPanelMouse(msg tea.MouseMsg) bool {
	if len(m.agentsPanelAbsRows) == 0 {
		return false
	}
	index, ok := m.agentsPanelAbsRows[msg.Y]
	if !ok || index < 0 || index >= len(m.agentOrder) {
		return false
	}
	m.panelIndex = index
	m.dirty = true
	return true
}

// positionModal records absolute click rows for a centered modal frame.
func (m *Model) positionModal(view string) {
	m.modalAbsRows = make(map[int]int, len(m.modalRows))
	if len(m.modalRows) == 0 {
		return
	}
	top := (m.height - lineCount(view)) / 2
	if top < 0 {
		top = 0
	}
	offset := 2
	if !m.st.plain {
		offset = 3
	}
	for _, row := range m.modalRows {
		y := top + offset + row.line
		if y >= 0 && y < m.height {
			m.modalAbsRows[y] = row.index
		}
	}
}

// positionOverlay records the absolute origin of a centered prompt card.
func (m *Model) positionOverlay(view string) {
	m.overlayTop = (m.height - lineCount(view)) / 2
	if m.overlayTop < 0 {
		m.overlayTop = 0
	}
	left := (m.width - maxLineWidth(view)) / 2
	if left < 0 {
		left = 0
	}
	m.overlayLeft = left
}

// positionAgentsPanel records absolute rows for the /agents panel, which is
// rendered as a plain centered block rather than through modalFrame.
func (m *Model) positionAgentsPanel(view string) {
	m.agentsPanelAbsRows = make(map[int]int, len(m.agentsPanelRows))
	top := (m.height - lineCount(view)) / 2
	if top < 0 {
		top = 0
	}
	for line, index := range m.agentsPanelRows {
		y := top + line
		if y >= 0 && y < m.height {
			m.agentsPanelAbsRows[y] = index
		}
	}
}

func maxLineWidth(view string) int {
	width := 0
	for _, line := range strings.Split(view, "\n") {
		if w := displayWidth(line); w > width {
			width = w
		}
	}
	return width
}

// handleLeftClick routes a left press to the most specific visible target,
// from blocking prompts down to transcript thinking blocks and sidebar agents.
func (m *Model) handleLeftClick(msg tea.MouseMsg) bool {
	if m.pendingApproval != nil || m.pendingAsk != nil {
		if m.handleOverlayMouse(msg) {
			return true
		}
		return false
	}
	if m.agentsPanel {
		return m.handleAgentsPanelMouse(msg)
	}
	if m.activeModal != modalNone {
		return m.handleModalMouse(msg)
	}
	if m.handleStatusMouse(msg) {
		return true
	}
	if m.handleSidebarMouse(msg) {
		return true
	}
	return m.handleTranscriptMouse(msg)
}

// addModalRow records one selectable row at the current body line.
func (m *Model) addModalRow(line, index int) {
	m.modalRows = append(m.modalRows, modalRowHit{line: line, index: index})
}
