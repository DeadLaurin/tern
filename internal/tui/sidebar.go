package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
)

// twoPane reports whether the responsive two-pane workspace should be used.
// Anything narrower than 108 columns keeps the calm single-column layout.
func (m *Model) twoPane() bool {
	return m.width >= 108 && !m.sidebarHidden
}

// sidebarWidth is the fixed comfortable width of the inspector on the right.
func (m *Model) sidebarWidth() int {
	return clampInt(m.width/3, 32, 42)
}

// leftPaneWidth is the chat pane. Three columns are reserved for the vertical
// hairline divider and its breathing room.
func (m *Model) leftPaneWidth() int {
	return maxInt(24, m.width-m.sidebarWidth()-3)
}

// dividerView draws a subtle full-height hairline between the two panes.
func (m *Model) dividerView(height int) string {
	lines := make([]string, maxInt(1, height))
	for i := range lines {
		if m.st.plain {
			lines[i] = " | "
		} else {
			lines[i] = " " + m.st.paint(m.st.border, "│") + " "
		}
	}
	return strings.Join(lines, "\n")
}

// padPane gives the left pane a clean one-column breathing margin on both
// sides and locks every line to exactly the pane width.
func (m *Model) padPane(block string, width, inner, height int) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		line = truncateAnsi(line, inner)
		line = " " + line
		if pad := width - displayWidth(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		lines[i] = line
	}
	blank := strings.Repeat(" ", maxInt(1, width))
	for len(lines) < height {
		lines = append(lines, blank)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// truncateAnsi trims visible runes while preserving ANSI escape sequences.
func truncateAnsi(s string, width int) string {
	if width <= 0 || displayWidth(s) <= width {
		return s
	}
	var b strings.Builder
	visible := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1b {
			for i < len(runes) {
				b.WriteRune(runes[i])
				if runes[i] == 'm' {
					break
				}
				i++
			}
			continue
		}
		if visible >= width {
			continue
		}
		b.WriteRune(runes[i])
		visible++
	}
	return b.String()
}

// sidebarView renders the full-height live inspector.
func (m *Model) sidebarView(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	m.sidebarAgentRows = make(map[int]string)
	m.sidebarGitRows = make(map[int]string)
	lines := make([]string, 0, height)
	add := func(block string) {
		if block == "" {
			return
		}
		lines = append(lines, strings.Split(block, "\n")...)
	}

	// 1. Context and stats.
	add(m.sidebarHeader("CONTEXT & STATS", width))
	add(m.sidebarContext(width))
	add("")

	// 2. Git Changes, GitHub & Cloudflare.
	if block := m.sidebarChanges(width, len(lines)); block != "" {
		add(block)
		add("")
	}

	// 3. Sub-agents.
	add(m.sidebarSubagents(width, height-len(lines)-6, len(lines)))

	// 4. Tasks / todos.
	if block := m.sidebarTodos(width); block != "" {
		add("")
		add(block)
	}

	// 5. Active task.
	if block := m.sidebarActiveTask(width); block != "" {
		add("")
		add(block)
	}

	// Drop rows that the height clip removes so a click cannot hit an agent
	// that is no longer visible.
	for y := range m.sidebarAgentRows {
		if y >= height {
			delete(m.sidebarAgentRows, y)
		}
	}
	for y := range m.sidebarGitRows {
		if y >= height {
			delete(m.sidebarGitRows, y)
		}
	}
	return m.fitSidebar(lines, width, height)
}

func (m *Model) sidebarHeader(title string, width int) string {
	if width <= 0 {
		return title
	}
	label := m.st.paint(m.st.dim, title)
	rest := width - displayWidth(title) - 1
	if rest < 1 {
		return truncateAnsi(label, width)
	}
	if m.st.plain {
		return title + " " + strings.Repeat("-", rest)
	}
	return label + " " + m.st.paint(m.st.border, strings.Repeat("─", rest))
}

func (m *Model) sidebarContext(width int) string {
	modelName := "unknown"
	effort := ""
	if m.opts.Config != nil {
		modelName = m.opts.Config.Model
		effort = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	modelLine := modelName
	if effort != "" {
		modelLine += " · " + effort
	}
	lines := []string{m.st.paint(m.st.statusValue, truncateCell(modelLine, width))}
	if ver := m.versionLabel(); ver != "" {
		if m.updateAvailable != "" {
			lines = append(lines, m.st.paint(m.st.warn, truncateCell(ver+" · ↑ "+m.updateAvailable+" (tern update)", width)))
		} else {
			lines = append(lines, m.st.paint(m.st.dim, truncateCell("tern "+ver, width)))
		}
	}
	if branch := strings.TrimSpace(m.opts.GitBranch); branch != "" {
		lines = append(lines, m.st.paint(m.st.dim, truncateCell("branch "+branch, width)))
	}
	if m.balance != nil {
		if compact := m.balance.FormatCompact(); compact != "" {
			balStyle := m.st.success
			if m.balance.HasLimit && (!m.balance.Available || m.balance.Remaining < 1.0) {
				balStyle = m.st.warn
			} else if m.balance.IsFreeTier {
				balStyle = m.st.dim
			}
			lines = append(lines, m.st.paint(balStyle, truncateCell("Bal "+compact, width)))
		}
	}

	barWidth := 12
	if width < 36 {
		barWidth = 10
	}
	ctxPct := 0
	if m.ctxWindow > 0 && m.ctxTokens > 0 {
		ctxPct = clampInt(int(float64(m.ctxTokens)/float64(m.ctxWindow)*100+0.5), 0, 100)
	}
	ctxSuffix := ""
	if m.ctxWindow > 0 && m.ctxTokens > 0 {
		ctxSuffix = fmt.Sprintf("%s/%s", compactTokens(m.ctxTokens), compactTokens(m.ctxWindow))
	}
	lines = append(lines, m.sidebarGauge("Ctx", ctxPct, barWidth, ctxSuffix, false))

	cachePct := 0
	if m.cache > 0 {
		cachePct = clampInt(int(m.cache*100+0.5), 0, 100)
	}
	lines = append(lines, m.sidebarGauge("Cache", cachePct, barWidth, "", true))

	stats := make([]string, 0, 3)
	if speed := m.speed(); speed > 0 {
		stats = append(stats, formatTokensPerSec(speed))
	}
	if m.totalCost > 0 {
		stats = append(stats, fmt.Sprintf("$%.3f", m.totalCost))
	}
	if !m.sessionStart.IsZero() {
		stats = append(stats, compactDuration(time.Since(m.sessionStart)))
	}
	if len(stats) > 0 {
		lines = append(lines, m.st.paint(m.st.dim, truncateCell(strings.Join(stats, " · "), width)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) sidebarGauge(label string, pct, barWidth int, suffix string, sage bool) string {
	pct = clampInt(pct, 0, 100)
	filled := (pct*barWidth + 50) / 100
	var bar string
	if m.st.plain {
		bar = strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
	} else {
		fillStyle := m.st.statusValue
		if sage {
			fillStyle = m.st.sage
		} else if pct >= 80 {
			fillStyle = m.st.amber
		}
		bar = m.st.paint(fillStyle, strings.Repeat("━", filled)) +
			m.st.paint(m.st.border, strings.Repeat("─", barWidth-filled))
	}
	line := fmt.Sprintf("%-5s %s %3d%%", label, bar, pct)
	if suffix != "" {
		line += " " + suffix
	}
	return truncateAnsi(line, m.sidebarWidth())
}

func (m *Model) sidebarChanges(width, startLine int) string {
	if !m.gitStatus.IsRepo && !m.cfStatus.Detected {
		return ""
	}
	title := "CHANGES"
	if n := len(m.gitStatus.Files); n > 0 {
		title = fmt.Sprintf("CHANGES (%d)", n)
	}
	lines := []string{m.sidebarHeader(title, width)}

	if m.gitStatus.IsRepo {
		meta := m.gitStatus.Branch
		if m.gitStatus.Ahead > 0 {
			meta += fmt.Sprintf(" ↑%d", m.gitStatus.Ahead)
		}
		if m.gitStatus.Behind > 0 {
			meta += fmt.Sprintf(" ↓%d", m.gitStatus.Behind)
		}
		if m.gitStatus.TotalAdded > 0 || m.gitStatus.TotalRemoved > 0 {
			meta += fmt.Sprintf(" +%d -%d", m.gitStatus.TotalAdded, m.gitStatus.TotalRemoved)
		}
		if meta != "" {
			lines = append(lines, m.st.paint(m.st.dim, truncateCell(meta, width)))
		}
		if len(m.gitStatus.Files) == 0 {
			lines = append(lines, m.st.paint(m.st.dim, "Working tree clean"))
		} else {
			maxFiles := 5
			for i, f := range m.gitStatus.Files {
				if i >= maxFiles {
					lines = append(lines, m.st.paint(m.st.dim, fmt.Sprintf("  … %d more (/git)", len(m.gitStatus.Files)-i)))
					m.sidebarGitRows[startLine+len(lines)-1] = "__open__"
					break
				}
				mark := "·"
				if f.Staged {
					mark = "✓"
				}
				num := ""
				if f.Added > 0 || f.Removed > 0 {
					num = fmt.Sprintf(" +%d -%d", f.Added, f.Removed)
				}
				avail := maxInt(8, width-displayWidth(num)-5)
				row := fmt.Sprintf("%s %s %s", mark, f.Status, truncateCell(f.Path, avail))
				if pad := width - displayWidth(row) - displayWidth(num); pad > 0 {
					row += strings.Repeat(" ", pad)
				}
				row += m.st.paint(m.st.dim, num)
				lines = append(lines, truncateAnsi(row, width))
				m.sidebarGitRows[startLine+len(lines)-1] = f.Path
			}
		}
		lines = append(lines, m.st.paint(m.st.accent, truncateCell("[Diff · Commit & Push] (/git)", width)))
		m.sidebarGitRows[startLine+len(lines)-1] = "__commit__"
	}

	if m.ghStatus.Repo != "" {
		ghLine := "GH " + m.ghStatus.Repo
		if m.ghStatus.PR != nil {
			ghLine = fmt.Sprintf("GH PR #%d · %s", m.ghStatus.PR.Number, emptyFallback(m.ghStatus.PR.ChecksState, m.ghStatus.PR.State))
		} else if m.ghStatus.LatestRun != nil {
			ghLine = fmt.Sprintf("GH CI · %s", emptyFallback(m.ghStatus.LatestRun.Conclusion, m.ghStatus.LatestRun.Status))
		}
		lines = append(lines, m.st.paint(m.st.dim, truncateCell(ghLine, width)))
		m.sidebarGitRows[startLine+len(lines)-1] = "__open__"
	}

	if m.cfStatus.Detected {
		cfLine := fmt.Sprintf("CF %s · %s", m.cfStatus.Kind, emptyFallback(m.cfStatus.ProjectName, m.cfStatus.ConfigFile))
		lines = append(lines, m.st.paint(m.st.warn, truncateCell(cfLine, width)))
		m.sidebarGitRows[startLine+len(lines)-1] = "__open__"
	}

	return strings.Join(lines, "\n")
}

func (m *Model) sidebarSubagents(width, budget, startLine int) string {
	total := len(m.agentOrder)
	running := m.activeAgentCount()
	title := "SUB-AGENTS"
	if total == 0 {
		title += " · idle"
	} else {
		title += fmt.Sprintf(" (%d running · %d total)", running, total)
	}
	lines := []string{m.sidebarHeader(title, width)}
	if total == 0 {
		lines = append(lines, m.st.paint(m.st.dim, "No sub-agents yet."))
		return strings.Join(lines, "\n")
	}

	limit := 5
	if budget > 0 {
		limit = clampInt(budget, 2, 8)
	}
	if m.agentsOpen {
		limit = minInt(limit, 5)
	}
	start, end := windowRange(total, m.agentFocus, limit)
	for i := start; i < end; i++ {
		block := m.agents[m.agentOrder[i]]
		if block == nil {
			continue
		}
		selected := m.agentsOpen && i == m.agentFocus
		lines = append(lines, m.sidebarAgentRow(block.view, selected, width))
		m.sidebarAgentRows[startLine+len(lines)-1] = block.view.ID
		if selected {
			m.flushAgentLine(block.view.ID)
			if detail := m.sidebarAgentDetail(block.view, width); detail != "" {
				lines = append(lines, strings.Split(detail, "\n")...)
			}
		}
	}
	if m.agentsOpen {
		lines = append(lines, m.st.paint(m.st.dim, truncateCell("enter peek · ↑/↓ select · c cancel · esc back", width)))
		lines = append(lines, m.st.paint(m.st.dim, truncateCell("Esc chat · C cancel all", width)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) sidebarAgentRow(view subagent.AgentView, selected bool, width int) string {
	icon := m.agentStatusIcon(view.Status)
	label := emptyFallback(view.AgentType, "agent")
	if view.CurrentTool != "" && view.Status == subagent.StatusRunning {
		label += ":" + view.CurrentTool
	}
	elapsed := ""
	if view.Elapsed > 0 {
		elapsed = " " + compactDuration(view.Elapsed)
	}
	available := maxInt(8, width-displayWidth(elapsed)-4)
	line := icon + " " + truncateCell(label+" · "+emptyFallback(view.Description, "working"), available)
	if elapsed != "" {
		if pad := width - displayWidth(line) - displayWidth(elapsed); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		line += m.st.paint(m.st.dim, elapsed)
	}
	if selected {
		return m.st.paint(m.st.selected, truncateAnsi(line, width))
	}
	return truncateAnsi(line, width)
}

func (m *Model) sidebarAgentDetail(view subagent.AgentView, width int) string {
	var lines []string
	if view.CurrentTool != "" {
		lines = append(lines, m.st.paint(m.st.dim, "  tool ")+truncateCell(view.CurrentTool, width-7))
	}
	steps := view.Steps
	if len(steps) > 3 {
		steps = steps[len(steps)-3:]
	}
	for _, step := range steps {
		status := m.agentStatusIcon(step.Status)
		detail := step.Detail
		if detail == "" {
			detail = step.Tool
		}
		lines = append(lines, "  "+status+" "+truncateCell(detail, width-4))
	}
	m.flushAgentLine(view.ID)
	if preview := livePreview(view.Summary, maxInt(10, width-4), 4); preview != "" {
		for _, line := range strings.Split(preview, "\n") {
			lines = append(lines, m.st.paint(m.st.dim, "  "+truncateAnsi(line, width-2)))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) agentStatusIcon(status string) string {
	if m.st.plain {
		switch status {
		case subagent.StatusRunning, subagent.StatusWaitingApproval:
			return "*"
		case subagent.StatusQueued:
			return "o"
		case subagent.StatusDone:
			return "*"
		case subagent.StatusFailed, subagent.StatusCancelled:
			return "x"
		case subagent.StatusLimitReached:
			return "[-]"
		}
		return "-"
	}
	switch status {
	case subagent.StatusRunning, subagent.StatusWaitingApproval:
		return spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	case subagent.StatusQueued:
		return "◌"
	case subagent.StatusDone:
		return m.st.paint(m.st.success, "●")
	case subagent.StatusFailed, subagent.StatusCancelled:
		return m.st.paint(m.st.error, "✕")
	case subagent.StatusLimitReached:
		return m.st.paint(m.st.warn, "⊘")
	}
	return m.st.paint(m.st.dim, "○")
}

func (m *Model) sidebarTodos(width int) string {
	if m.opts.Todos == nil {
		return ""
	}
	items := m.opts.Todos.Items()
	if len(items) == 0 {
		return ""
	}
	completed := 0
	for _, item := range items {
		if item.Status == tools.TodoCompleted {
			completed++
		}
	}
	lines := []string{m.sidebarHeader(fmt.Sprintf("TODOS (%d/%d)", completed, len(items)), width)}
	for i, item := range items {
		if i >= 6 {
			lines = append(lines, m.st.paint(m.st.dim, fmt.Sprintf("  … %d more", len(items)-i)))
			break
		}
		marker := "○"
		style := m.st.dim
		switch item.Status {
		case tools.TodoInProgress:
			marker = "▸"
			style = m.st.accent
		case tools.TodoCompleted:
			marker = "✓"
			style = m.st.dim
		case tools.TodoCancelled:
			marker = "⊘"
			style = m.st.dim
		}
		if m.st.plain {
			switch item.Status {
			case tools.TodoInProgress:
				marker = ">"
			case tools.TodoCompleted:
				marker = "x"
			case tools.TodoCancelled:
				marker = "-"
			default:
				marker = " "
			}
		}
		wrapped := wrapPlainCell(item.Content, maxInt(10, width-4))
		for j, part := range strings.Split(wrapped, "\n") {
			prefix := "  "
			if j == 0 {
				prefix = marker + " "
			}
			lines = append(lines, m.st.paint(style, prefix+truncateCell(part, width-2)))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) sidebarActiveTask(width int) string {
	if !m.running && !m.usingTools {
		return ""
	}
	var phase string
	if m.usingTools {
		name := "tool"
		started := m.turnStart
		for i := len(m.entries) - 1; i >= 0; i-- {
			entry := m.entries[i]
			if entry.kind == entryTool && !entry.finished {
				name = entry.toolName
				started = entry.started
				break
			}
		}
		if started.IsZero() {
			started = m.turnStart
		}
		phase = fmt.Sprintf("Running %s… %s", name, formatDuration(time.Since(started)))
	} else if m.liveSpeed > 0 {
		phase = "Streaming… " + formatTokensPerSec(m.liveSpeed)
	} else {
		phase = "Thinking… " + formatDuration(time.Since(m.turnStart))
	}
	return m.sidebarHeader("ACTIVE TASK", width) + "\n" + m.st.paint(m.st.accent, truncateCell(phase, width))
}

func (m *Model) fitSidebar(lines []string, width, height int) string {
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		line = truncateAnsi(line, width)
		if pad := width - displayWidth(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		lines[i] = line
	}
	blank := strings.Repeat(" ", width)
	for len(lines) < height {
		lines = append(lines, blank)
	}
	content := strings.Join(lines, "\n")
	if !m.st.plain {
		content = m.st.panel.Width(width).Render(content)
	}
	return content
}

func (m *Model) cancelFocusedAgent() {
	if len(m.agentOrder) == 0 {
		return
	}
	if m.agentFocus < 0 || m.agentFocus >= len(m.agentOrder) {
		m.agentFocus = 0
	}
	id := m.agentOrder[m.agentFocus]
	if m.opts.Subagent != nil && m.opts.Subagent.CancelOne(id) {
		m.addInfo("Cancelled sub-agent " + id + ".")
	} else {
		m.addInfo("Sub-agent " + id + " is no longer running.")
	}
	m.dirty = true
}
