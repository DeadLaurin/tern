package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
)

// modalKind identifies the interactive overlay currently shown above the
// editor. Exactly one modal can be active at a time; Esc always closes it.
type modalKind int

const (
	modalNone modalKind = iota
	modalModel
	modalSessions
	modalStats
	modalSkills
	modalMCP
)

var reasoningEfforts = []string{"none", "low", "high", "max"}

type modelChoice struct {
	name    string
	pricing config.ModelPricing
}

type modalSession struct {
	ID        string
	Name      string
	Model     string
	Messages  int
	Turns     int
	Usage     provider.Usage
	CostUSD   float64
	Duration  time.Duration
	UpdatedAt time.Time
}

type modalState struct {
	kind modalKind

	index  int
	filter string
	// filterActive keeps shortcut letters (n/f/r/d) as text once the user has
	// started typing a filter. A leading "/" explicitly enters filter mode.
	filterActive bool

	rename      bool
	renameText  string
	renameFresh bool

	confirmDelete bool
	notice        string

	models []modelChoice
	effort string

	sessions []modalSession

	stats *session.AggregateStats

	skills       []terncontext.SkillMeta
	skillPreview string

	mcp []ext.MCPStatus
}

func (m *Model) setModal(kind modalKind) {
	m.activeModal = kind
	m.modal = modalState{kind: kind}
	m.dirty = true
}

func (m *Model) closeModal() {
	m.activeModal = modalNone
	m.modal = modalState{}
	m.statusMsg = "Ready"
	m.dirty = true
}

func (m *Model) handleModalKey(msg tea.KeyMsg) tea.Cmd {
	switch m.activeModal {
	case modalModel:
		return m.handleModelModalKey(msg)
	case modalSessions:
		return m.handleSessionsModalKey(msg)
	case modalStats:
		return m.handleStatsModalKey(msg)
	case modalSkills:
		return m.handleSkillsModalKey(msg)
	case modalMCP:
		return m.handleMCPModalKey(msg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Model selector
// ---------------------------------------------------------------------------

func (m *Model) openModelModal() {
	m.setModal(modalModel)
	cfg := m.opts.Config
	current := ""
	effort := ""
	if cfg != nil {
		current = cfg.Model
		effort = cfg.ReasoningEffort
	}
	names := make([]string, 0, len(cfg.Pricing))
	if cfg != nil {
		for name, pricing := range cfg.Pricing {
			names = append(names, name)
			_ = pricing
		}
	}
	sort.Strings(names)
	for _, name := range names {
		m.modal.models = append(m.modal.models, modelChoice{name: name, pricing: cfg.Pricing[name]})
	}
	m.modal.index = 0
	for i, model := range m.modal.models {
		if model.name == current {
			m.modal.index = i
			break
		}
	}
	m.modal.effort = effort
	if !containsString(reasoningEfforts, effort) {
		m.modal.effort = "high"
	}
	if len(m.modal.models) == 0 {
		m.modal.notice = "No models are configured in pricing."
	}
	m.statusMsg = "Model selector"
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (m *Model) handleModelModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlP:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampModelIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampModelIndex()
		return nil
	case tea.KeyLeft:
		m.cycleEffort(-1)
		return nil
	case tea.KeyRight, tea.KeyTab:
		m.cycleEffort(1)
		return nil
	case tea.KeyShiftTab:
		m.cycleEffort(-1)
		return nil
	case tea.KeyEnter:
		m.applyModelModal()
		return nil
	}
	return nil
}

func (m *Model) clampModelIndex() {
	n := len(m.modal.models)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) cycleEffort(delta int) {
	index := 0
	for i, effort := range reasoningEfforts {
		if effort == m.modal.effort {
			index = i
			break
		}
	}
	index = (index + delta + len(reasoningEfforts)) % len(reasoningEfforts)
	m.modal.effort = reasoningEfforts[index]
	m.dirty = true
}

func (m *Model) applyModelModal() {
	if len(m.modal.models) == 0 {
		m.closeModal()
		return
	}
	selected := m.modal.models[m.modal.index]
	effort := m.modal.effort
	m.applyModel(selected.name)
	m.applyReasoningEffort(effort)
	m.closeModal()
	m.addInfo(fmt.Sprintf("Model set to %s (%s).", selected.name, effort))
}

func (m *Model) applyModel(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if m.opts.Config != nil {
		m.opts.Config.Model = name
	}
	if m.opts.Agent != nil {
		if m.opts.Agent.Config == nil {
			m.opts.Agent.Config = m.opts.Config
		}
		m.opts.Agent.Config.Model = name
		m.opts.Agent.SetModel(name)
		if m.opts.Agent.Provider != nil {
			if caps := m.opts.Agent.Provider.ModelInfo(name); caps.ContextWindow > 0 {
				m.ctxWindow = caps.ContextWindow
			}
		}
	}
	if m.opts.Config != nil {
		if pricing, ok := m.opts.Config.Pricing[name]; ok && pricing.ContextWindow > 0 {
			m.ctxWindow = pricing.ContextWindow
		}
	}
	m.dirty = true
}

func (m *Model) applyReasoningEffort(effort string) {
	effort = strings.TrimSpace(effort)
	if m.opts.Config != nil {
		m.opts.Config.ReasoningEffort = effort
	}
	if m.opts.Agent != nil {
		if m.opts.Agent.Config == nil {
			m.opts.Agent.Config = m.opts.Config
		}
		m.opts.Agent.Config.ReasoningEffort = effort
		m.opts.Agent.SetReasoningEffort(effort)
	}
	m.dirty = true
}

func (m *Model) modelModalView() string {
	var b strings.Builder
	current := ""
	if m.opts.Config != nil {
		current = m.opts.Config.Model
	}
	fmt.Fprintf(&b, "Current model: %s\n", current)
	if len(m.modal.models) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No models configured in pricing."))
		return m.modalFrame("Model", b.String(), "esc close")
	}
	limit := m.modalRowLimit()
	start, end := windowRange(len(m.modal.models), m.modal.index, limit)
	if start > 0 {
		b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
	}
	for i := start; i < end; i++ {
		model := m.modal.models[i]
		marker := "  "
		style := m.st.dim
		if i == m.modal.index {
			marker = "▸ "
			style = m.st.selected
			if m.st.plain {
				marker = "> "
			}
		}
		active := " "
		if model.name == current {
			active = "*"
		}
		tag := "text"
		if model.pricing.SupportsVision {
			tag = "vision"
		}
		ctx := compactContext(model.pricing.ContextWindow)
		line := fmt.Sprintf("%s%s %-18s %-5s %-6s  off %.2f/%.2f/%.2f  peak %.2f/%.2f/%.2f",
			marker, active, truncateCell(model.name, 18), ctx, tag,
			model.pricing.OffPeakCacheHitPerM, model.pricing.OffPeakCacheMissPerM, model.pricing.OffPeakOutputPerM,
			model.pricing.PeakCacheHitPerM, model.pricing.PeakCacheMissPerM, model.pricing.PeakOutputPerM,
		)
		b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
		b.WriteString("\n")
	}
	if end < len(m.modal.models) {
		b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.models)-end)))
	}
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.accent, "reasoning effort  "))
	for i, effort := range reasoningEfforts {
		if i > 0 {
			b.WriteString(m.st.paint(m.st.dim, " · "))
		}
		if effort == m.modal.effort {
			b.WriteString(m.st.paint(m.st.selected, "["+effort+"]"))
		} else {
			b.WriteString(m.st.paint(m.st.dim, effort))
		}
	}
	b.WriteString(m.st.paint(m.st.dim, "   ←/→ or tab cycles"))
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Model", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · ←/→ effort · enter apply · esc close")
}

func compactContext(window int) string {
	if window >= 1_000_000 {
		return fmt.Sprintf("%dM", window/1_000_000)
	}
	if window >= 1_000 {
		return fmt.Sprintf("%dk", window/1_000)
	}
	return fmt.Sprintf("%d", window)
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func (m *Model) openSessionsModal(filter string) {
	if m.opts.SessionStore == nil {
		m.addError("Session storage is not configured.")
		return
	}
	m.setModal(modalSessions)
	m.modal.filter = strings.TrimSpace(filter)
	m.modal.filterActive = m.modal.filter != ""
	m.refreshSessions()
	m.statusMsg = "Sessions"
}

func (m *Model) refreshSessions() {
	m.modal.sessions = nil
	store := m.opts.SessionStore
	if store == nil {
		return
	}
	stats, err := store.Stats()
	if err == nil && stats != nil {
		for _, row := range stats.Sessions {
			m.modal.sessions = append(m.modal.sessions, modalSession{
				ID: row.ID, Name: row.Name, Model: row.Model,
				Messages: row.Messages, Turns: row.Turns,
				Usage: row.Usage, CostUSD: row.CostUSD,
				Duration: row.Duration, UpdatedAt: row.UpdatedAt,
			})
		}
	}
	if len(m.modal.sessions) == 0 {
		if list, listErr := store.List(""); listErr == nil {
			for _, item := range list {
				m.modal.sessions = append(m.modal.sessions, modalSession{
					ID: item.ID, Name: item.Name, Model: item.Model,
					Messages: item.MessageCount, Usage: item.Usage,
					CostUSD: item.CostUSD, UpdatedAt: item.UpdatedAt,
				})
			}
		} else if err == nil {
			m.modal.notice = "sessions: " + listErr.Error()
		}
	}
	sort.SliceStable(m.modal.sessions, func(i, j int) bool {
		return m.modal.sessions[i].UpdatedAt.After(m.modal.sessions[j].UpdatedAt)
	})
	m.clampSessionIndex()
}

func (m *Model) filteredSessions() []modalSession {
	query := strings.ToLower(strings.TrimSpace(m.modal.filter))
	if query == "" {
		return m.modal.sessions
	}
	out := make([]modalSession, 0, len(m.modal.sessions))
	for _, item := range m.modal.sessions {
		haystack := strings.ToLower(item.ID + " " + item.Name + " " + item.Model)
		if strings.Contains(haystack, query) {
			out = append(out, item)
		}
	}
	return out
}

func (m *Model) clampSessionIndex() {
	n := len(m.filteredSessions())
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) selectedSession() *modalSession {
	items := m.filteredSessions()
	if len(items) == 0 || m.modal.index < 0 || m.modal.index >= len(items) {
		return nil
	}
	return &items[m.modal.index]
}

func (m *Model) handleSessionsModalKey(msg tea.KeyMsg) tea.Cmd {
	if m.modal.rename {
		return m.handleSessionRenameKey(msg)
	}
	if m.modal.confirmDelete {
		switch msg.Type {
		case tea.KeyRunes:
			switch strings.ToLower(string(msg.Runes)) {
			case "y":
				m.deleteSelectedSession()
				return nil
			case "n":
				m.modal.confirmDelete = false
				m.modal.notice = ""
				return nil
			}
			return nil
		case tea.KeySpace:
			m.modal.confirmDelete = false
			m.modal.notice = ""
			return nil
		case tea.KeyEsc:
			m.modal.confirmDelete = false
			m.modal.notice = ""
			return nil
		case tea.KeyEnter:
			m.deleteSelectedSession()
			return nil
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampSessionIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampSessionIndex()
		return nil
	case tea.KeyEnter:
		m.resumeSelectedSession()
		return nil
	case tea.KeyBackspace:
		m.backspaceSessionFilter()
		return nil
	case tea.KeySpace:
		m.modal.filter += " "
		m.modal.filterActive = true
		m.modal.index = 0
		m.clampSessionIndex()
		return nil
	case tea.KeyRunes:
		text := string(msg.Runes)
		if text == "" {
			return nil
		}
		if !m.modal.filterActive && m.modal.filter == "" {
			switch text {
			case "n":
				m.createSessionFromModal()
				return nil
			case "f":
				m.forkSelectedSession()
				return nil
			case "r":
				m.startSessionRename()
				return nil
			case "d":
				m.armSessionDelete()
				return nil
			case "/":
				m.modal.filterActive = true
				m.modal.notice = "filter: type to narrow sessions"
				return nil
			}
		}
		m.modal.filter += text
		m.modal.filterActive = true
		m.modal.index = 0
		m.clampSessionIndex()
		return nil
	}
	return nil
}

func (m *Model) backspaceSessionFilter() {
	runes := []rune(m.modal.filter)
	if len(runes) == 0 {
		m.modal.filterActive = false
		return
	}
	m.modal.filter = string(runes[:len(runes)-1])
	if m.modal.filter == "" {
		m.modal.filterActive = false
	}
	m.modal.index = 0
	m.clampSessionIndex()
}

func (m *Model) handleSessionRenameKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.modal.rename = false
		m.modal.renameText = ""
		m.modal.renameFresh = false
		m.modal.notice = ""
		return nil
	case tea.KeyEnter:
		name := strings.TrimSpace(m.modal.renameText)
		selected := m.selectedSession()
		if selected == nil {
			m.modal.rename = false
			return nil
		}
		if name == "" {
			m.modal.notice = "Session name cannot be empty."
			return nil
		}
		if err := m.opts.SessionStore.Rename(selected.ID, name); err != nil {
			m.modal.notice = "rename failed: " + err.Error()
			return nil
		}
		if m.opts.Session != nil && m.opts.Session.ID() == selected.ID {
			_ = m.opts.Session.Rename(name)
		}
		m.modal.rename = false
		m.modal.renameText = ""
		m.modal.renameFresh = false
		m.modal.notice = "Renamed session to " + name + "."
		m.refreshSessions()
		return nil
	case tea.KeyBackspace:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
			return nil
		}
		runes := []rune(m.modal.renameText)
		if len(runes) > 0 {
			m.modal.renameText = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeySpace:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
		}
		m.modal.renameText += " "
		return nil
	case tea.KeyRunes:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
		}
		m.modal.renameText += string(msg.Runes)
		return nil
	}
	return nil
}

func (m *Model) startSessionRename() {
	selected := m.selectedSession()
	if selected == nil {
		return
	}
	name := selected.Name
	if strings.TrimSpace(name) == "" {
		name = selected.ID
	}
	m.modal.rename = true
	m.modal.renameText = name
	m.modal.renameFresh = true
	m.modal.notice = ""
}

func (m *Model) armSessionDelete() {
	selected := m.selectedSession()
	if selected == nil {
		return
	}
	if m.opts.Session != nil && selected.ID == m.opts.Session.ID() {
		m.modal.notice = "Cannot delete the active session. Switch to another session first."
		return
	}
	m.modal.confirmDelete = true
	m.modal.notice = "Delete session " + truncateCell(selected.displayName(), 32) + "? y/n"
}

func (m *Model) deleteSelectedSession() {
	selected := m.selectedSession()
	m.modal.confirmDelete = false
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	if m.opts.Session != nil && selected.ID == m.opts.Session.ID() {
		m.modal.notice = "Cannot delete the active session. Switch to another session first."
		return
	}
	if err := m.opts.SessionStore.Delete(selected.ID); err != nil {
		m.modal.notice = "delete failed: " + err.Error()
		return
	}
	m.modal.notice = "Deleted session " + selected.ID + "."
	m.refreshSessions()
}

func (m *Model) createSessionFromModal() {
	if m.opts.SessionStore == nil {
		return
	}
	meta := session.SessionMeta{WorkDir: m.opts.WorkDir}
	if m.opts.Config != nil {
		meta.Model = m.opts.Config.Model
	}
	sess, err := m.opts.SessionStore.Create(meta)
	if err != nil {
		m.modal.notice = "create failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Started new session "+sess.ID()+".")
}

func (m *Model) forkSelectedSession() {
	selected := m.selectedSession()
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	forked, err := m.opts.SessionStore.Fork(selected.ID, "")
	if err != nil {
		m.modal.notice = "fork failed: " + err.Error()
		return
	}
	m.activateSession(forked, "Forked session "+selected.ID+" → "+forked.ID()+".")
}

func (m *Model) resumeSelectedSession() {
	selected := m.selectedSession()
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	sess, err := m.opts.SessionStore.Open(selected.ID)
	if err != nil {
		m.modal.notice = "open failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Resumed session "+sess.ID()+".")
}

func (m *Model) activateSession(sess *session.Session, message string) {
	if sess == nil {
		return
	}
	if m.running {
		m.modal.notice = "Cannot switch sessions while a turn is running."
		return
	}
	m.opts.Session = sess
	if m.opts.Agent != nil {
		m.opts.Agent.SwitchSession(sess)
		m.usage, m.totalCost = m.opts.Agent.TotalUsage()
	} else {
		m.usage, m.totalCost = sess.TotalUsage()
	}
	m.cache = cacheRate(m.usage)
	m.rebuildTranscriptFromSession(sess)
	m.closeModal()
	m.statusMsg = message
}

func (m *Model) rebuildTranscriptFromSession(sess *session.Session) {
	msgs := sess.ReplayMessages()
	m.entries = nil
	m.curAssistant = nil
	m.curReasoning = nil
	m.pendingText = ""
	m.pendingReasoning = ""
	m.toolEntries = make(map[string]*transcriptEntry)
	for _, msg := range msgs {
		switch msg.Role {
		case provider.RoleUser:
			text := messageText(msg)
			if strings.TrimSpace(text) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryUser, text: text})
			}
		case provider.RoleAssistant:
			if strings.TrimSpace(msg.ReasoningContent) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryReasoning, text: msg.ReasoningContent})
			}
			if strings.TrimSpace(msg.Content) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryAssistant, text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				m.entries = append(m.entries, &transcriptEntry{
					kind: entryTool, toolName: call.Function.Name,
					toolID: call.ID, args: call.Function.Arguments, finished: true,
				})
			}
		case provider.RoleTool:
			if strings.TrimSpace(msg.Content) != "" {
				m.entries = append(m.entries, &transcriptEntry{
					kind: entryTool, toolName: emptyFallback(msg.Name, "tool"),
					toolID: msg.ToolCallID, output: msg.Content, finished: true,
				})
			}
		}
	}
	m.historyChars = historyChars(msgs)
	m.ctxTokens = m.historyChars / 4
	m.followTail = true
	m.dirty = true
}

func messageText(msg provider.Message) string {
	if len(msg.Parts) > 0 {
		var b strings.Builder
		for _, part := range msg.Parts {
			if part.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(part.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return msg.Content
}

func (item modalSession) displayName() string {
	if strings.TrimSpace(item.Name) != "" {
		return item.Name
	}
	return item.ID
}

func (m *Model) sessionsModalView() string {
	var b strings.Builder
	items := m.filteredSessions()
	filter := m.modal.filter
	if m.modal.rename {
		b.WriteString(m.st.paint(m.st.amber, "rename: ") + m.modal.renameText + "▌\n\n")
	} else {
		b.WriteString(m.st.paint(m.st.dim, "filter: ") + filter)
		if filter == "" {
			b.WriteString(m.st.paint(m.st.dim, " (type to narrow, / to force)"))
		}
		b.WriteString("\n\n")
	}
	activeID := ""
	if m.opts.Session != nil {
		activeID = m.opts.Session.ID()
	}
	if len(items) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No sessions found. Press n to create one."))
	} else {
		limit := m.modalRowLimit()
		start, end := windowRange(len(items), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		for i := start; i < end; i++ {
			item := items[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			active := " "
			if item.ID == activeID {
				active = "*"
			}
			line := fmt.Sprintf("%s%s %-24s %-16s %2dt %3dm %6s $%.3f %6s %s",
				marker, active,
				truncateCell(item.displayName(), 24),
				truncateCell(item.Model, 16),
				item.Turns, item.Messages,
				compactTokens(item.Usage.TotalTokens),
				item.CostUSD,
				compactDuration(item.Duration),
				item.UpdatedAt.Local().Format("01-02 15:04"),
			)
			b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
			b.WriteString("\n")
		}
		if end < len(items) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(items)-end)))
		}
	}
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Sessions", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · enter open · n new · f fork · r rename · d delete · esc close")
}

// ---------------------------------------------------------------------------
// Usage & activity stats
// ---------------------------------------------------------------------------

func (m *Model) openStatsModal() {
	if m.opts.SessionStore == nil {
		m.addError("Session storage is not configured.")
		return
	}
	stats, err := m.opts.SessionStore.Stats()
	if err != nil {
		m.addError("stats: " + err.Error())
		return
	}
	m.setModal(modalStats)
	m.modal.stats = stats
	m.modal.index = 0
	m.statusMsg = "Usage & activity"
}

func (m *Model) handleStatsModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampStatsIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampStatsIndex()
		return nil
	case tea.KeyPgUp:
		m.modal.index -= m.modalRowLimit()
		m.clampStatsIndex()
		return nil
	case tea.KeyPgDown:
		m.modal.index += m.modalRowLimit()
		m.clampStatsIndex()
		return nil
	case tea.KeyEnter:
		m.resumeStatsSession()
		return nil
	}
	return nil
}

func (m *Model) clampStatsIndex() {
	n := 0
	if m.modal.stats != nil {
		n = len(m.modal.stats.Sessions)
	}
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) resumeStatsSession() {
	if m.modal.stats == nil || m.opts.SessionStore == nil {
		return
	}
	if len(m.modal.stats.Sessions) == 0 {
		return
	}
	row := m.modal.stats.Sessions[m.modal.index]
	sess, err := m.opts.SessionStore.Open(row.ID)
	if err != nil {
		m.modal.notice = "open failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Resumed session "+sess.ID()+".")
}

func (m *Model) statsModalView() string {
	if m.modal.stats == nil {
		return m.modalFrame("Usage & activity", "No stats available.", "esc close")
	}
	s := m.modal.stats
	var b strings.Builder
	cachePct := int(s.CacheHitRate*100 + 0.5)
	fmt.Fprintf(&b, "%s  total sessions: %d · sub-agents: %d · turns: %d · active time: %s\n",
		m.st.paint(m.st.accent, "Lifetime"),
		s.SessionCount, s.SubagentCount, s.TurnCount, compactDuration(s.TotalDuration))
	fmt.Fprintf(&b, "Total Tokens %s · %s input · %s cache hit · %s cache miss · %s output · %s reasoning\n",
		compactTokens(s.TotalUsage.TotalTokens),
		compactTokens(s.TotalUsage.PromptTokens),
		compactTokens(s.TotalUsage.PromptCacheHitTokens),
		compactTokens(s.TotalUsage.PromptCacheMissTokens),
		compactTokens(s.TotalUsage.CompletionTokens),
		compactTokens(s.TotalUsage.ReasoningTokens))
	fmt.Fprintf(&b, "Cache hit %d%% · average %s · total estimated cost $%.4f\n",
		cachePct, formatTokensPerSec(s.AvgTokensPerSec), s.TotalCostUSD)

	if len(s.Models) > 0 {
		b.WriteString("\n" + m.st.paint(m.st.bold, "Models") + "\n")
		for _, model := range s.Models {
			fmt.Fprintf(&b, "  %-18s %3d sessions %4d turns %8s  %s  $%.4f\n",
				truncateCell(model.Model, 18), model.Sessions, model.Turns,
				compactTokens(model.Usage.TotalTokens),
				formatTokensPerSec(model.AvgTokensPerSec), model.CostUSD)
		}
	}

	b.WriteString("\n" + m.st.paint(m.st.bold, "Session log") + " " + m.st.paint(m.st.dim, "(newest first)") + "\n")
	if len(s.Sessions) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "  No sessions recorded yet."))
	} else {
		limit := m.modalRowLimit()
		start, end := windowRange(len(s.Sessions), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		activeID := ""
		if m.opts.Session != nil {
			activeID = m.opts.Session.ID()
		}
		for i := start; i < end; i++ {
			row := s.Sessions[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			active := " "
			if row.ID == activeID {
				active = "*"
			}
			name := row.Name
			if strings.TrimSpace(name) == "" {
				name = row.ID
			}
			line := fmt.Sprintf("%s%s %-22s %-14s %2dt %3dm %7s $%.3f %s",
				marker, active, truncateCell(name, 22), truncateCell(row.Model, 14),
				row.Turns, row.Messages, compactTokens(row.Usage.TotalTokens),
				row.CostUSD, compactDuration(row.Duration))
			b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
			b.WriteString("\n")
		}
		if end < len(s.Sessions) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(s.Sessions)-end)))
		}
	}
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Usage & activity", strings.TrimRight(b.String(), "\n"),
		"↑/↓ scroll · enter resume session · esc close")
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func (m *Model) openSkillsModal() {
	skills, err := terncontext.ListSkills(m.opts.WorkDir, m.opts.GlobalConfigDir)
	if err != nil {
		m.addError("skills: " + err.Error())
		return
	}
	m.setModal(modalSkills)
	m.modal.skills = skills
	m.modal.index = 0
	m.loadSkillPreview()
	m.statusMsg = "Skills"
}

func (m *Model) loadSkillPreview() {
	m.modal.skillPreview = ""
	if len(m.modal.skills) == 0 {
		return
	}
	if m.modal.index < 0 {
		m.modal.index = len(m.modal.skills) - 1
	}
	if m.modal.index >= len(m.modal.skills) {
		m.modal.index = 0
	}
	meta := m.modal.skills[m.modal.index]
	skill, err := terncontext.LoadSkill(m.opts.WorkDir, m.opts.GlobalConfigDir, meta.Name)
	if err != nil {
		m.modal.skillPreview = "Could not load skill: " + err.Error()
		return
	}
	m.modal.skillPreview = skill.Body
}

func (m *Model) handleSkillsModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.loadSkillPreview()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.loadSkillPreview()
		return nil
	case tea.KeyEnter:
		m.loadSelectedSkill()
		return nil
	case tea.KeySpace:
		return nil
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "c":
			m.createExampleSkill()
		}
		return nil
	}
	return nil
}

func (m *Model) loadSelectedSkill() {
	if len(m.modal.skills) == 0 {
		return
	}
	meta := m.modal.skills[m.modal.index]
	skill, err := terncontext.LoadSkill(m.opts.WorkDir, m.opts.GlobalConfigDir, meta.Name)
	if err != nil {
		m.modal.notice = "skills: " + err.Error()
		return
	}
	m.ed.SetValue(skill.Body)
	m.closeModal()
	m.statusMsg = "Loaded skill " + meta.Name + " into the editor"
	m.addInfo("Loaded skill " + meta.Name + " into the editor. Press Enter to submit it as context.")
}

func (m *Model) createExampleSkill() {
	dir := filepath.Join(m.opts.WorkDir, ".tern", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.modal.notice = "create skill: " + err.Error()
		return
	}
	path := filepath.Join(dir, "example.md")
	if _, err := os.Stat(path); err == nil {
		m.modal.notice = "Starter skill already exists at " + path
		return
	}
	content := "---\n" +
		"name: example\n" +
		"description: A starter skill for tern.\n" +
		"tags: [example]\n" +
		"---\n\n" +
		"# Example skill\n\n" +
		"Describe when to use this skill and the steps the agent should follow.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.modal.notice = "create skill: " + err.Error()
		return
	}
	skills, err := terncontext.ListSkills(m.opts.WorkDir, m.opts.GlobalConfigDir)
	if err == nil {
		m.modal.skills = skills
		for i, skill := range skills {
			if skill.Name == "example" {
				m.modal.index = i
				break
			}
		}
		m.loadSkillPreview()
	}
	m.modal.notice = "Created starter skill at " + path
}

func (m *Model) skillsModalView() string {
	var b strings.Builder
	if len(m.modal.skills) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No skills found."))
		b.WriteString("\n\nPress c to create a starter skill at ./.tern/skills/example.md.\n")
	} else {
		limit := maxInt(4, minInt(len(m.modal.skills), m.modalRowLimit()/2))
		start, end := windowRange(len(m.modal.skills), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		for i := start; i < end; i++ {
			skill := m.modal.skills[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			source := skill.Scope
			if source == "" {
				source = "project"
			}
			line := fmt.Sprintf("%s%-20s %-7s %s", marker, truncateCell(skill.Name, 20),
				truncateCell(source, 7), truncateCell(skill.Description, maxInt(20, m.modalTextWidth()-32)))
			b.WriteString(m.st.paint(style, line))
			b.WriteString("\n")
		}
		if end < len(m.modal.skills) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.skills)-end)))
		}
		b.WriteString("\n" + m.st.paint(m.st.bold, "Preview") + "\n")
		preview := m.modal.skillPreview
		if strings.TrimSpace(preview) == "" {
			preview = "(empty skill body)"
		}
		preview = truncateLines(preview, m.modalRowLimit()/2)
		b.WriteString(m.st.paint(m.st.dim, indentLines(wrapPlainCell(preview, maxInt(20, m.modalTextWidth()-4)), "  ")))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.dim, "global: ") + filepath.Join(m.opts.GlobalConfigDir, "skills") + m.st.paint(m.st.dim, "  (~/.config/tern/skills)"))
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.dim, "project: ") + filepath.Join(m.opts.WorkDir, ".tern", "skills") + m.st.paint(m.st.dim, "  (./.tern/skills)"))
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Skills", strings.TrimRight(b.String(), "\n"),
		"↑/↓ browse · enter load into editor · c create starter · esc close")
}

// ---------------------------------------------------------------------------
// MCP servers
// ---------------------------------------------------------------------------

func (m *Model) openMCPModal() {
	m.setModal(modalMCP)
	m.refreshMCPStatus()
	m.statusMsg = "MCP servers"
}

func (m *Model) refreshMCPStatus() {
	m.modal.mcp = nil
	if m.opts.MCP == nil {
		if m.opts.Extensions != nil {
			m.opts.MCP = m.opts.Extensions
		}
	}
	if m.opts.MCP != nil {
		m.modal.mcp = m.opts.MCP.Status()
	}
	n := len(m.modal.mcp)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) handleMCPModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.refreshMCPStatus()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.refreshMCPStatus()
		return nil
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "r":
			m.reconnectMCP()
		}
		return nil
	}
	return nil
}

func (m *Model) reconnectMCP() {
	if m.opts.MCP == nil {
		m.modal.notice = "No MCP manager configured."
		return
	}
	m.modal.notice = "Reconnecting MCP servers…"
	m.statusMsg = "Reconnecting MCP servers…"
	m.dirty = true
	manager := m.opts.MCP
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := manager.Connect(ctx)
		if err != nil {
			m.bus.Push(cmdResultMsg{text: "MCP reconnect", err: err})
			return
		}
		m.bus.Push(cmdResultMsg{text: "MCP servers reconnected."})
	}()
}

func (m *Model) mcpModalView() string {
	m.refreshMCPStatus()
	var b strings.Builder
	if len(m.modal.mcp) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No MCP servers configured."))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.bold, "Example ~/.config/tern/mcp.toml") + "\n")
		b.WriteString(m.st.paint(m.st.codeBlock, strings.Join([]string{
			"[[servers]]",
			`name = "filesystem"`,
			`transport = "stdio"`,
			`command = "npx"`,
			`args = ["-y", "@modelcontextprotocol/server-filesystem", "/path"]`,
			"",
			"[[servers]]",
			`name = "remote"`,
			`transport = "http"`,
			`url = "https://example.com/mcp"`,
		}, "\n")))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.dim, "global: ~/.config/tern/mcp.toml") + "\n")
		b.WriteString(m.st.paint(m.st.dim, "project: ./.tern/mcp.toml"))
	} else {
		limit := maxInt(4, minInt(len(m.modal.mcp), m.modalRowLimit()/2))
		start, end := windowRange(len(m.modal.mcp), m.modal.index, limit)
		for i := start; i < end; i++ {
			server := m.modal.mcp[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			dot := "●"
			dotStyle := m.st.sage
			state := "connected"
			if !server.Connected {
				dot = "✗"
				dotStyle = m.st.coral
				state = "error"
			}
			if server.Error != "" {
				state = "error"
			}
			transport := emptyFallback(server.Transport, "stdio")
			line := fmt.Sprintf("%s%s %-18s %-7s %-9s %d tool(s)",
				marker, m.st.paint(dotStyle, dot), truncateCell(server.Name, 18),
				truncateCell(transport, 7), truncateCell(state, 9), server.ToolCount)
			b.WriteString(m.st.paint(style, line))
			b.WriteString("\n")
		}
		if end < len(m.modal.mcp) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.mcp)-end)))
		}
		server := m.modal.mcp[m.modal.index]
		if server.Error != "" {
			b.WriteString("\n" + m.st.paint(m.st.coral, "error: "+server.Error))
		}
		b.WriteString("\n\n" + m.st.paint(m.st.bold, "Tools") + "\n")
		if len(server.Tools) == 0 {
			b.WriteString(m.st.paint(m.st.dim, "  (none reported)"))
		} else {
			for _, tool := range server.Tools {
				b.WriteString("  " + m.st.paint(m.st.teal, truncateCell(tool, maxInt(20, m.modalTextWidth()-4))) + "\n")
			}
		}
	}
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("MCP servers", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · r reconnect · esc close")
}

// ---------------------------------------------------------------------------
// Modal frame and helpers
// ---------------------------------------------------------------------------

func (m *Model) modalView() string {
	switch m.activeModal {
	case modalModel:
		return m.modelModalView()
	case modalSessions:
		return m.sessionsModalView()
	case modalStats:
		return m.statsModalView()
	case modalSkills:
		return m.skillsModalView()
	case modalMCP:
		return m.mcpModalView()
	}
	return ""
}

func (m *Model) modalFrame(title, body, footer string) string {
	if m.st.plain {
		var b strings.Builder
		b.WriteString(title)
		b.WriteString("\n\n")
		b.WriteString(body)
		if footer != "" {
			b.WriteString("\n\n")
			b.WriteString(footer)
		}
		return b.String()
	}
	content := m.st.paint(m.st.modalTitle, title) + "\n\n" + body
	if footer != "" {
		content += "\n\n" + m.st.paint(m.st.dim, footer)
	}
	width := m.modalTextWidth()
	box := m.st.modalBorder.Width(width).Render(content)
	return lipgloss.PlaceHorizontal(maxInt(m.width, width+6), lipgloss.Center, box)
}

func (m *Model) modalTextWidth() int {
	width := m.width - 8
	if width < 32 {
		width = 32
	}
	if width > 104 {
		width = 104
	}
	return width
}

func (m *Model) modalRowLimit() int {
	rows := (m.height - 16) / 1
	if rows < 5 {
		rows = 5
	}
	if rows > 18 {
		rows = 18
	}
	return rows
}

func windowRange(total, index, limit int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if limit <= 0 {
		limit = 1
	}
	if index < 0 {
		index = 0
	}
	if index >= total {
		index = total - 1
	}
	if total <= limit {
		return 0, total
	}
	start := index - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > total {
		start = total - limit
	}
	return start, start + limit
}

func compactTokens(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func compactDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}
