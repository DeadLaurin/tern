package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/subagent"
)

// openAgentPeek opens the full transcript of one sub-agent. An empty id picks
// the currently focused agent in the right sidebar or /agents panel.
func (m *Model) openAgentPeek(id string) bool {
	if len(m.agentOrder) == 0 {
		m.addInfo("No sub-agents yet. Delegate work with the task tool.")
		return false
	}
	index := m.agentFocus
	if m.agentsPanel {
		index = m.panelIndex
	}
	id = strings.TrimSpace(id)
	if id != "" {
		found := -1
		for i, candidate := range m.agentOrder {
			if candidate == id {
				found = i
				break
			}
		}
		if found < 0 {
			m.addError("Unknown sub-agent " + id + ".")
			return false
		}
		index = found
	}
	if index < 0 || index >= len(m.agentOrder) {
		index = 0
	}
	m.agentFocus = index
	m.panelIndex = index
	m.agentsPanel = false
	m.agentsOpen = true
	m.setModal(modalAgentPeek)
	m.modal.peekAgentID = m.agentOrder[index]
	m.modal.peekScroll = 0
	m.statusMsg = "Peeking sub-agent " + m.modal.peekAgentID
	m.dirty = true
	return true
}

// openAgentPeekArg resolves `/agents <id|index>` and opens the matching
// transcript. Indices are 1-based, matching the /agents panel order.
func (m *Model) openAgentPeekArg(arg string) bool {
	arg = strings.TrimSpace(strings.Trim(arg, `"'`))
	if arg == "" {
		return m.openAgentPeek("")
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(m.agentOrder) {
		return m.openAgentPeek(m.agentOrder[n-1])
	}
	for _, id := range m.agentOrder {
		if strings.HasPrefix(id, arg) {
			return m.openAgentPeek(id)
		}
	}
	return m.openAgentPeek(arg)
}

func (m *Model) peekedAgentIndex() int {
	for i, id := range m.agentOrder {
		if id == m.modal.peekAgentID {
			return i
		}
	}
	return -1
}

func (m *Model) switchPeekedAgent(delta int) {
	if len(m.agentOrder) == 0 {
		return
	}
	index := m.peekedAgentIndex()
	if index < 0 {
		index = m.agentFocus
	}
	index = (index + delta + len(m.agentOrder)) % len(m.agentOrder)
	m.modal.peekAgentID = m.agentOrder[index]
	m.modal.peekScroll = 0
	m.agentFocus = index
	m.panelIndex = index
	m.dirty = true
}

func (m *Model) peekPageSize() int {
	return maxInt(4, m.height-18)
}

func (m *Model) peekMaxScroll() int {
	total := len(m.agentPeekTranscript(m.modal.peekAgentID))
	page := m.peekPageSize()
	if total <= page {
		return 0
	}
	return total - page
}

func (m *Model) scrollAgentPeek(delta int) {
	m.modal.peekScroll = clampInt(m.modal.peekScroll+delta, 0, m.peekMaxScroll())
	m.dirty = true
}

func (m *Model) handleAgentPeekKey(msg tea.KeyMsg) tea.Cmd {
	page := m.peekPageSize()
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.scrollAgentPeek(-1)
		return nil
	case tea.KeyDown:
		m.scrollAgentPeek(1)
		return nil
	case tea.KeyPgUp:
		m.scrollAgentPeek(-page)
		return nil
	case tea.KeyPgDown:
		m.scrollAgentPeek(page)
		return nil
	case tea.KeyHome:
		m.modal.peekScroll = 0
		m.dirty = true
		return nil
	case tea.KeyEnd:
		m.modal.peekScroll = m.peekMaxScroll()
		m.dirty = true
		return nil
	case tea.KeyLeft, tea.KeyShiftTab:
		m.switchPeekedAgent(-1)
		return nil
	case tea.KeyRight, tea.KeyTab:
		m.switchPeekedAgent(1)
		return nil
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "t":
			m.modal.peekReasoning = !m.modal.peekReasoning
			m.modal.peekScroll = clampInt(m.modal.peekScroll, 0, m.peekMaxScroll())
			m.dirty = true
		case "c":
			id := m.modal.peekAgentID
			if m.opts.Subagent != nil && m.opts.Subagent.CancelOne(id) {
				m.addInfo("Cancelled sub-agent " + id + ".")
			} else {
				m.addInfo("Sub-agent " + id + " is no longer running.")
			}
			m.dirty = true
		case "q":
			m.closeModal()
		}
		return nil
	}
	return nil
}

// agentPeekChild returns the persisted child transcript belonging to id.
func (m *Model) agentPeekChild(id string) *session.Session {
	if m.opts.Session == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	children, err := m.opts.Session.ListChildren()
	if err != nil {
		return nil
	}
	parentID := m.opts.Session.ID()
	var match *session.Session
	for _, child := range children {
		if child == nil {
			continue
		}
		childID := child.ID()
		if childID == parentID+".sub."+id ||
			strings.HasSuffix(childID, ".sub."+id) ||
			strings.HasSuffix(childID, "."+id) ||
			strings.HasSuffix(childID, id) {
			match = child
		}
	}
	return match
}

// agentPeekTranscript assembles persisted child messages plus the live stream
// and tool trail so the modal works while an agent is still running.
func (m *Model) agentPeekTranscript(id string) []string {
	width := maxInt(20, m.modalTextWidth()-4)
	var lines []string
	addBlank := func() {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
	}
	addBlock := func(block string) {
		if strings.TrimSpace(block) == "" {
			return
		}
		addBlank()
		lines = append(lines, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
	}

	if child := m.agentPeekChild(id); child != nil {
		for _, msg := range child.ReplayMessages() {
			addBlock(m.agentPeekMessage(msg, width))
		}
	}

	block := m.agents[id]
	liveStreamed := ""
	if b := m.agentLines[id]; b != nil {
		liveStreamed = b.String()
	}
	liveSummary := ""
	if block != nil {
		liveSummary = block.view.Summary
	}
	if strings.TrimSpace(liveStreamed) != "" || strings.TrimSpace(liveSummary) != "" {
		var b strings.Builder
		b.WriteString(m.st.paint(m.st.dim, "Live output"))
		if strings.TrimSpace(liveStreamed) != "" {
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.quote, wrapPlainCell(strings.TrimRight(liveStreamed, "\n"), width)))
		}
		if strings.TrimSpace(liveSummary) != "" {
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.quote, wrapPlainCell(truncateLines(strings.TrimSpace(liveSummary), 8), width)))
		}
		addBlock(b.String())
	}

	if block != nil && len(block.view.Steps) > 0 {
		var b strings.Builder
		b.WriteString(m.st.paint(m.st.dim, "Tool trail"))
		for _, step := range block.view.Steps {
			label := strings.TrimSpace(step.Tool)
			if detail := strings.TrimSpace(step.Detail); detail != "" {
				label += " " + detail
			}
			if label == "" {
				continue
			}
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.toolName, "⚙ ") + m.st.paint(m.st.dim, truncateCell(label, width-2)))
		}
		addBlock(b.String())
	}

	if len(lines) == 0 {
		lines = append(lines, m.st.paint(m.st.dim, "No transcript recorded yet."))
	}
	return lines
}

func (m *Model) agentPeekMessage(msg provider.Message, width int) string {
	switch msg.Role {
	case provider.RoleUser:
		text := peekMessageText(msg)
		if strings.TrimSpace(text) == "" {
			return ""
		}
		var b strings.Builder
		b.WriteString(m.st.paint(m.st.prompt, "› Prompt"))
		b.WriteString("\n")
		b.WriteString(m.st.paint(m.st.user, RenderMarkdown(text, width, m.st.plain)))
		return b.String()

	case provider.RoleAssistant:
		var parts []string
		if reasoning := strings.TrimSpace(msg.ReasoningContent); reasoning != "" {
			if m.modal.peekReasoning {
				body := RenderMarkdown(reasoning, maxInt(20, width-2), m.st.plain)
				rail := make([]string, 0)
				rail = append(rail, m.st.paint(m.st.reasoning, "▾ Thought"))
				for _, line := range strings.Split(body, "\n") {
					rail = append(rail, m.st.paint(m.st.reasoning, "│ "+line))
				}
				parts = append(parts, strings.Join(rail, "\n"))
			} else {
				parts = append(parts, m.st.paint(m.st.reasoning,
					fmt.Sprintf("▸ Thought · %d chars · t to expand", len([]rune(reasoning)))))
			}
		}
		if text := strings.TrimSpace(msg.Content); text != "" {
			parts = append(parts, RenderMarkdown(text, width, m.st.plain))
		}
		for _, call := range msg.ToolCalls {
			name := strings.TrimSpace(call.Function.Name)
			if name == "" {
				name = "tool"
			}
			head := m.st.paint(m.st.toolName, "⚙ "+name)
			if summary := toolSummary(name, call.Function.Arguments); summary != "" {
				head += m.st.paint(m.st.dim, " · "+summary)
			}
			parts = append(parts, head)
		}
		return strings.Join(parts, "\n")

	case provider.RoleTool:
		text := strings.TrimSpace(peekMessageText(msg))
		if text == "" {
			return ""
		}
		preview := truncateLines(text, 3)
		body := wrapPlainCell(preview, maxInt(20, width-4))
		lines := strings.Split(body, "\n")
		for i := range lines {
			if i == 0 {
				lines[i] = "  ↳ " + lines[i]
			} else {
				lines[i] = "    " + lines[i]
			}
		}
		return m.st.paint(m.st.dim, strings.Join(lines, "\n"))
	}
	return ""
}

func peekMessageText(msg provider.Message) string {
	if strings.TrimSpace(msg.Content) != "" {
		return msg.Content
	}
	var parts []string
	for _, part := range msg.Parts {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		} else if part.ImageURL != nil {
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func (m *Model) agentPeekView() string {
	if block := m.agents[m.modal.peekAgentID]; block != nil {
		view := block.view
		agentType := emptyFallback(view.AgentType, "agent")
		description := emptyFallback(view.Description, "sub-agent")
		statusStyle := m.st.dim
		switch view.Status {
		case subagent.StatusRunning, subagent.StatusWaitingApproval:
			statusStyle = m.st.amber
		case subagent.StatusDone:
			statusStyle = m.st.sage
		case subagent.StatusFailed, subagent.StatusCancelled:
			statusStyle = m.st.coral
		}
		status := emptyFallback(view.Status, "unknown")
		header := m.st.paint(m.st.heading, fmt.Sprintf("%s · %s · %q", agentType, view.ID, description)) +
			"  " + m.st.paint(statusStyle, "● "+status)

		steps := view.Step
		if steps == 0 {
			steps = len(view.Steps)
		}
		cachePct := int(cacheRate(view.Usage)*100 + 0.5)
		meta := fmt.Sprintf("%d steps · %s tokens · cache %d%% · $%.4f · %s",
			steps, compactTokens(view.Usage.TotalTokens), cachePct, view.CostUSD, compactDuration(view.Elapsed))
		header += "\n" + m.st.paint(m.st.dim, meta)

		body := m.agentPeekTranscript(view.ID)
		maxScroll := m.peekMaxScroll()
		scroll := clampInt(m.modal.peekScroll, 0, maxScroll)
		m.modal.peekScroll = scroll
		page := m.peekPageSize()
		end := clampInt(scroll+page, 0, len(body))
		if scroll > end {
			scroll = end
		}
		var b strings.Builder
		b.WriteString(header)
		b.WriteString("\n\n")
		b.WriteString(strings.Join(body[scroll:end], "\n"))
		if maxScroll > 0 || len(body) > page {
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("lines %d-%d of %d", scroll+1, end, len(body))))
		}
		footer := "↑/↓ scroll · pgup/pgdn · home/end · ←/→ or tab switch · t reasoning · c cancel · esc close"
		return m.modalFrame("Sub-agent transcript", strings.TrimRight(b.String(), "\n"), footer)
	}
	return m.modalFrame("Sub-agent transcript", m.st.paint(m.st.dim, "This sub-agent is no longer available."), "esc close")
}
