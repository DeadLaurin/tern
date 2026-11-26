package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/subagent"
)

// agentsView renders the stacked live sub-agent group shown above the editor.
func (m *Model) agentsView() string {
	if len(m.agentOrder) == 0 {
		return ""
	}
	views := make([]subagent.AgentView, 0, len(m.agentOrder))
	for _, id := range m.agentOrder {
		block := m.agents[id]
		if block == nil {
			continue
		}
		m.flushAgentLine(id)
		views = append(views, block.view)
	}
	if len(views) == 0 {
		return ""
	}
	var blocks []string
	for i, view := range views {
		focused := m.agentsOpen && i == m.agentFocus
		rendered := subagent.Render(view)
		if focused {
			rendered = subagent.RenderAgent(view, true)
			if preview := livePreview(view.Summary, m.width-6, 6); preview != "" {
				rendered += "\n" + m.st.paint(m.st.dim, indentLines(preview, "    "))
			}
		}
		blocks = append(blocks, rendered)
	}
	body := strings.Join(blocks, "\n")
	if m.agentsOpen {
		body += "\n" + m.st.paint(m.st.dim, "Tab/Ctrl+O next · Esc return · c cancel · C cancel all")
	}
	lines := strings.Split(body, "\n")
	limit := 8
	if m.agentsOpen {
		limit = 18
	}
	if len(lines) > limit {
		lines = append(lines[:limit-1], m.st.paint(m.st.dim, fmt.Sprintf("… %d more agent lines (Tab to expand)", len(lines)-limit+1)))
	}
	return strings.Join(lines, "\n")
}

func livePreview(s string, width, maxLines int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return wrapPlainCell(strings.Join(lines, "\n"), width)
}

// agentsPanelView renders the interactive /agents panel.
func (m *Model) agentsPanelView() string {
	m.agentsPanelRows = make(map[int]int)
	title := "Sub-agents"
	var b strings.Builder
	b.WriteString(m.st.paint(m.st.heading, title))
	b.WriteString("\n")
	if len(m.agentOrder) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No sub-agents yet. Delegate work with the task tool."))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.dim, "esc close"))
		return b.String()
	}
	for i, id := range m.agentOrder {
		block := m.agents[id]
		if block == nil {
			continue
		}
		m.agentsPanelRows[strings.Count(b.String(), "\n")] = i
		view := block.view
		marker := "  "
		style := m.st.dim
		if i == m.panelIndex {
			marker = "▸ "
			style = m.st.selected
			if m.st.plain {
				marker = "> "
			}
		}
		status := view.Status
		if view.Status == subagent.StatusRunning && view.CurrentTool != "" {
			status += " · " + view.CurrentTool
		}
		line := fmt.Sprintf("%s%s %-15s %-13s %s", marker, view.ID, truncateCell(view.AgentType, 15), truncateCell(status, 13), truncateCell(view.Description, maxInt(10, m.width-48)))
		b.WriteString(m.st.paint(style, line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.dim, "↑/↓ select · enter peek · c cancel agent · C cancel all · esc close"))
	return b.String()
}

func (m *Model) handleAgentsPanelKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlO:
		m.agentsPanel = false
		m.agentsOpen = false
		m.statusMsg = "Ready"
		m.dirty = true
		return nil
	case tea.KeyCtrlC:
		m.agentsPanel = false
		return m.handleCtrlC()
	case tea.KeyUp:
		if len(m.agentOrder) > 0 {
			m.panelIndex = (m.panelIndex - 1 + len(m.agentOrder)) % len(m.agentOrder)
		}
		return nil
	case tea.KeyDown:
		if len(m.agentOrder) > 0 {
			m.panelIndex = (m.panelIndex + 1) % len(m.agentOrder)
		}
		return nil
	case tea.KeyEnter:
		if len(m.agentOrder) == 0 {
			return nil
		}
		m.agentFocus = m.panelIndex
		m.openAgentPeek(m.agentOrder[m.panelIndex])
		return nil
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "c":
			if len(m.agentOrder) == 0 || m.opts.Subagent == nil {
				return nil
			}
			id := m.agentOrder[m.panelIndex]
			if m.opts.Subagent.CancelOne(id) {
				m.addInfo("Cancelled sub-agent " + id + ".")
			} else {
				m.addInfo("Sub-agent " + id + " is no longer running.")
			}
			m.dirty = true
		case "C":
			if m.opts.Subagent == nil {
				return nil
			}
			n := m.opts.Subagent.CancelAll()
			m.addInfo(fmt.Sprintf("Cancelled %d sub-agent(s).", n))
			m.dirty = true
		case "q":
			m.agentsPanel = false
			m.agentsOpen = false
			m.statusMsg = "Ready"
			m.dirty = true
		}
		return nil
	}
	return nil
}

// approver is the parent agent's interactive approval callback.
func (m *Model) approver(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
	resp := make(chan approvalResult, 1)
	state := &approvalState{parent: &req, response: resp}
	m.bus.Push(approvalRequestMsg{request: state})
	select {
	case result := <-resp:
		return result.allow, result.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// subagentApprover is installed on the sub-agent approval queue. Requests are
// serialized by the queue and labeled with the asking agent.
func (m *Model) subagentApprover(ctx context.Context, req subagent.ApprovalRequest) (bool, error) {
	resp := make(chan approvalResult, 1)
	state := &approvalState{sub: &req, response: resp}
	m.bus.Push(approvalRequestMsg{request: state})
	select {
	case result := <-resp:
		return result.allow, result.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (m *Model) askUser(ctx context.Context, question string, options []string) (string, error) {
	resp := make(chan string, 1)
	state := &askState{question: question, options: options, response: resp}
	m.bus.Push(askUserMsg{request: state})
	select {
	case answer := <-resp:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *Model) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "y":
			m.resolveApproval(true)
			return nil
		case "n":
			m.resolveApproval(false)
			return nil
		case "a":
			m.turnAutoApprove = true
			m.resolveApproval(true)
			return nil
		case "d":
			m.toggleApprovalDiff()
			return nil
		}
		return nil
	case tea.KeyEnter:
		m.resolveApproval(true)
		return nil
	case tea.KeyEsc, tea.KeyCtrlC:
		m.resolveApproval(false)
		return nil
	}
	return nil
}

// enqueueApproval keeps concurrent approval requests in a strict FIFO queue.
// A request can never overwrite the one currently on screen.
// toggleApprovalDiff flips the pending approval card between its compact and
// expanded diff previews.
func (m *Model) toggleApprovalDiff() {
	state := m.pendingApproval
	if state == nil {
		return
	}
	state.diffExpanded = !state.diffExpanded
	if state.diffExpanded {
		m.statusMsg = "Full diff"
	} else {
		m.statusMsg = "Compact diff"
	}
	m.dirty = true
}

// approvalDiff returns the diff payload for whichever agent asked, already
// trimmed. It is empty when the request carries no diff.
func approvalDiff(state *approvalState) string {
	if state == nil {
		return ""
	}
	if state.sub != nil {
		return strings.TrimSpace(state.sub.DiffPreview)
	}
	if state.parent != nil {
		return strings.TrimSpace(state.parent.Diff)
	}
	return ""
}

// diffLineCount returns the number of physical lines in a diff payload.
func diffLineCount(diff string) int {
	if strings.TrimSpace(diff) == "" {
		return 0
	}
	return len(strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n"))
}

// diffStats counts added and removed lines, ignoring the +++/--- file headers.
func diffStats(diff string) (added, removed int) {
	for _, line := range strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

func (m *Model) enqueueApproval(state *approvalState) {
	if state == nil {
		return
	}
	if m.turnAutoApprove {
		m.deliverApproval(state, true)
		m.statusMsg = "Auto-approved for this turn"
		m.dirty = true
		return
	}
	if m.pendingApproval != nil {
		m.approvalQueue = append(m.approvalQueue, state)
		m.statusMsg = fmt.Sprintf("Waiting for approval (%d queued)", len(m.approvalQueue))
		m.dirty = true
		m.followTail = true
		return
	}
	m.pendingApproval = state
	m.statusMsg = "Waiting for approval"
	m.dirty = true
	m.followTail = true
}

func (m *Model) deliverApproval(state *approvalState, allow bool) {
	if state == nil {
		return
	}
	select {
	case state.response <- approvalResult{allow: allow}:
	default:
	}
}

func (m *Model) resolveApproval(allow bool) {
	state := m.pendingApproval
	m.pendingApproval = nil
	if state != nil {
		tool := ""
		if state.parent != nil {
			tool = state.parent.ToolName
		} else if state.sub != nil {
			tool = state.sub.ToolName
		}
		m.deliverApproval(state, allow)
		if allow {
			m.addInfo("Approved " + tool + ".")
		} else {
			m.addInfo("Denied " + tool + ".")
		}
	}

	// Drain queued requests when the user opted into allow-all for this turn.
	if m.turnAutoApprove {
		if len(m.approvalQueue) > 0 {
			for _, queued := range m.approvalQueue {
				m.deliverApproval(queued, true)
			}
			m.addInfo(fmt.Sprintf("Auto-approved %d queued approval(s) for this turn.", len(m.approvalQueue)))
			m.approvalQueue = nil
		}
	} else if len(m.approvalQueue) > 0 {
		m.pendingApproval = m.approvalQueue[0]
		m.approvalQueue = m.approvalQueue[1:]
	}

	if m.pendingApproval != nil {
		m.statusMsg = "Waiting for approval"
	} else {
		m.statusMsg = "Ready"
	}
	m.dirty = true
	m.followTail = true
}

func (m *Model) handleAskKey(msg tea.KeyMsg) tea.Cmd {
	state := m.pendingAsk
	if state == nil {
		return nil
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.resolveAsk("")
		return nil
	case tea.KeyUp:
		if len(state.options) > 0 {
			state.selected = (state.selected - 1 + len(state.options)) % len(state.options)
		}
		return nil
	case tea.KeyDown:
		if len(state.options) > 0 {
			state.selected = (state.selected + 1) % len(state.options)
		}
		return nil
	case tea.KeyBackspace:
		if len(state.input) > 0 {
			state.input = state.input[:len(state.input)-1]
		}
		return nil
	case tea.KeyEnter:
		answer := strings.TrimSpace(string(state.input))
		if answer == "" && len(state.options) > 0 {
			answer = state.options[state.selected]
		}
		m.resolveAsk(answer)
		return nil
	case tea.KeyRunes:
		state.input = append(state.input, msg.Runes...)
		return nil
	case tea.KeySpace:
		state.input = append(state.input, ' ')
		return nil
	}
	return nil
}

func (m *Model) resolveAsk(answer string) {
	state := m.pendingAsk
	m.pendingAsk = nil
	if state != nil {
		select {
		case state.response <- answer:
		default:
		}
		if answer != "" {
			m.addInfo("Answer: " + answer)
		} else {
			m.addInfo("Question dismissed.")
		}
	}
	m.statusMsg = "Ready"
	m.dirty = true
	m.followTail = true
}

func (m *Model) approvalView() string {
	state := m.pendingApproval
	if state == nil {
		return ""
	}
	diff := approvalDiff(state)
	added, removed := diffStats(diff)

	title := "Permission Required"
	total := 1 + len(m.approvalQueue)
	if total > 1 {
		title = fmt.Sprintf("%s (1 of %d)", title, total)
	}
	var b strings.Builder
	b.WriteString(m.st.paint(m.st.warn, title))
	if strings.TrimSpace(diff) != "" {
		b.WriteString(m.st.paint(m.st.dim, "  "))
		b.WriteString(m.st.paint(m.st.success, fmt.Sprintf("+%d", added)))
		b.WriteString(m.st.paint(m.st.dim, " "))
		b.WriteString(m.st.paint(m.st.error, fmt.Sprintf("-%d", removed)))
	}
	b.WriteString("\n\n")

	if state.sub != nil {
		req := state.sub
		fmt.Fprintf(&b, "Agent: %s (%s)\n", emptyFallback(req.AgentType, "sub-agent"), req.AgentID)
		if strings.TrimSpace(req.TaskDescription) != "" {
			fmt.Fprintf(&b, "Task: %s\n", wrapPlainCell(req.TaskDescription, maxInt(20, m.width-8)))
		}
		fmt.Fprintf(&b, "Tool: %s\n", req.ToolName)
		if strings.TrimSpace(req.Detail) != "" {
			fmt.Fprintf(&b, "Command/Path: %s\n", req.Detail)
		}
		if strings.TrimSpace(req.Reason) != "" {
			fmt.Fprintf(&b, "Reason: %s\n", req.Reason)
		}
	} else if state.parent != nil {
		req := state.parent
		fmt.Fprintf(&b, "Agent: tern (main)\n")
		fmt.Fprintf(&b, "Tool: %s\n", req.ToolName)
		if strings.TrimSpace(req.Command) != "" {
			fmt.Fprintf(&b, "Command: %s\n", wrapPlainCell(req.Command, maxInt(20, m.width-10)))
		}
		if strings.TrimSpace(req.Path) != "" {
			fmt.Fprintf(&b, "Path: %s\n", req.Path)
		}
		if strings.TrimSpace(req.Reason) != "" {
			fmt.Fprintf(&b, "Reason: %s\n", req.Reason)
		}
	}
	if strings.TrimSpace(diff) != "" {
		b.WriteString("\n")
		b.WriteString(m.st.paint(m.st.bold, "Diff"))
		b.WriteString("\n")
		b.WriteString(m.diffView(diff, state.diffExpanded))
		b.WriteString("\n")
	}

	showDiffToggle := diffLineCount(diff) > compactDiffRows
	buttons := []struct {
		label  string
		action mouseAction
		style  lipgloss.Style
	}{
		{label: "[y] Allow", action: actionApprovalAllow, style: m.st.success},
		{label: "[a] Allow all this turn", action: actionApprovalAllowAll, style: m.st.accent},
		{label: "[n] Deny", action: actionApprovalDeny, style: m.st.error},
	}
	if showDiffToggle {
		label := "[d] Full diff"
		if state.diffExpanded {
			label = "[d] Compact diff"
		}
		buttons = append(buttons, struct {
			label  string
			action mouseAction
			style  lipgloss.Style
		}{label: label, action: actionApprovalDiffToggle, style: m.st.info})
	}

	b.WriteString("\n")
	buttonLine := strings.Count(b.String(), "\n")
	insetY, insetX := 0, 0
	if !m.st.plain {
		insetY = 1 // top border
		insetX = 2 // border + horizontal padding
	}

	// Build the button row while recording a click span for every label so
	// the hit geometry always matches what the user sees.
	var bar strings.Builder
	hits := make([]overlayHit, 0, len(buttons))
	x := 0
	for i, button := range buttons {
		if i > 0 {
			bar.WriteString(m.st.paint(m.st.dim, "  "))
			x += 2
		}
		start := x
		x += displayWidth(button.label)
		bar.WriteString(m.st.paint(button.style, button.label))
		hits = append(hits, overlayHit{
			line:   insetY + buttonLine,
			start:  insetX + start,
			end:    insetX + x,
			action: button.action,
		})
	}
	b.WriteString(bar.String())
	m.overlayHits = hits
	return m.overlayCard(b.String())
}

func (m *Model) askView() string {
	state := m.pendingAsk
	if state == nil {
		return ""
	}
	title := "Question"
	var b strings.Builder
	b.WriteString(m.st.paint(m.st.info, title))
	b.WriteString("\n\n")
	b.WriteString(wrapPlainCell(state.question, maxInt(20, m.width-2)))
	b.WriteString("\n")
	optionLines := make([]int, 0, len(state.options))
	optionWidths := make([]int, 0, len(state.options))
	if len(state.options) > 0 {
		b.WriteString("\n")
		for i, option := range state.options {
			optionLines = append(optionLines, strings.Count(b.String(), "\n"))
			optionWidths = append(optionWidths, displayWidth(option))
			marker := "  "
			style := m.st.dim
			if i == state.selected {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			b.WriteString(marker + m.st.paint(style, option))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	if len(state.input) > 0 {
		b.WriteString(m.st.paint(m.st.info, "› ") + string(state.input) + "\n")
	}
	b.WriteString(m.st.paint(m.st.dim, "[Enter] Submit  [Esc] Cancel"))
	if len(state.options) > 0 {
		b.WriteString(m.st.paint(m.st.dim, "  ·  ↑/↓ choose or type a free-form answer"))
	}
	card := m.overlayCard(b.String())
	insetY, insetX := 0, 0
	if !m.st.plain {
		insetY = 1
		insetX = 2
	}
	m.overlayHits = m.overlayHits[:0]
	for i, line := range optionLines {
		width := 2 + optionWidths[i]
		m.overlayHits = append(m.overlayHits, overlayHit{
			line:   insetY + line,
			start:  insetX,
			end:    insetX + width,
			action: actionAskOption,
			index:  i,
		})
	}
	return card
}

// overlayCard wraps modal prompt content in the same crisp framed card used by
// the other overlays. In plain mode it stays border-free and copy-friendly.
func (m *Model) overlayCard(content string) string {
	if m.st.plain {
		return content
	}
	width := m.width - 8
	if width > 92 {
		width = 92
	}
	if width < 36 {
		width = 36
	}
	return m.st.modalBorder.Width(width).Render(content)
}

// diffView renders the diff preview. Compact mode caps the preview at a dozen
// lines; expanding it shows up to roughly a screenful before eliding the rest.
func (m *Model) diffView(diff string, expanded bool) string {
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	limit := compactDiffRows
	if expanded {
		limit = expandedDiffRows
	}
	if len(lines) > limit {
		trimmed := make([]string, 0, limit+1)
		trimmed = append(trimmed, lines[:limit]...)
		trimmed = append(trimmed, fmt.Sprintf("… %d more diff lines · [d] to expand", len(lines)-limit))
		if expanded {
			trimmed[len(trimmed)-1] = fmt.Sprintf("… %d more diff lines", len(lines)-limit)
		}
		lines = trimmed
	}
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):
			lines[i] = m.st.paint(m.st.diffHunk, truncateCell(line, m.width-4))
		case strings.HasPrefix(line, "+"):
			lines[i] = m.st.paint(m.st.diffAdd, truncateCell(line, m.width-4))
		case strings.HasPrefix(line, "-"):
			lines[i] = m.st.paint(m.st.diffDel, truncateCell(line, m.width-4))
		default:
			lines[i] = truncateCell(line, m.width-4)
		}
	}
	return strings.Join(lines, "\n")
}

func emptyFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
