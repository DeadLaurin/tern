package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/agent"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tickInterval  = 60 * time.Millisecond
	quitTimeout   = 3 * time.Second
	maxEditorRows = 8
	maxDiffRows   = 28
)

type entryKind int

const (
	entryInfo entryKind = iota
	entryUser
	entryAssistant
	entryReasoning
	entryTool
	entryError
)

type transcriptEntry struct {
	kind     entryKind
	text     string
	toolName string
	toolID   string
	args     string
	started  time.Time
	output   string
	isError  bool
	elapsed  time.Duration
	finished bool
}

type agentBlock struct {
	view subagent.AgentView
}

type approvalState struct {
	parent   *agent.ApprovalRequest
	sub      *subagent.ApprovalRequest
	response chan approvalResult
}

type approvalResult struct {
	allow bool
	err   error
}

type askState struct {
	question string
	options  []string
	selected int
	input    []rune
	response chan string
}

// Model is the Bubble Tea model for the TUI.
type Model struct {
	opts    Options
	st      styles
	bus     *eventBus
	program *tea.Program
	baseCtx context.Context

	width  int
	height int

	viewport viewport.Model
	ed       editor

	entries      []*transcriptEntry
	curAssistant *transcriptEntry
	curReasoning *transcriptEntry
	toolEntries  map[string]*transcriptEntry

	pendingText      string
	pendingReasoning string

	agents      map[string]*agentBlock
	agentOrder  []string
	agentLines  map[string]*strings.Builder
	agentFocus  int
	agentsOpen  bool
	agentsPanel bool
	panelIndex  int

	showReasoning  bool
	showToolDetail bool

	running   bool
	turnCtx   context.Context
	cancel    context.CancelFunc
	turnStart time.Time

	spinnerFrame int
	usingTools   bool

	// Live output throughput. Numeric deltas drive an estimate until the
	// provider reports an exact tokens_per_sec value.
	outputRunes int
	speedStart  time.Time
	liveSpeed   float64
	lastSpeed   float64

	usage        provider.Usage
	totalCost    float64
	cache        float64
	ctxTokens    int
	ctxWindow    int
	historyChars int

	quitArmed bool
	quitAt    time.Time
	statusMsg string

	pendingApproval *approvalState
	pendingAsk      *askState

	commands []terncontext.SlashCommand

	files []string

	dirty      bool
	content    string
	lastWidth  int
	followTail bool

	// Modal state. activeModal is the enum the key router and view use;
	// modal carries the selected rows, filters and transient prompts.
	activeModal modalKind
	modal       modalState
}

// Init starts the UI ticker.
func (m *Model) Init() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update handles one message. It never performs blocking agent work on the
// event loop.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dirty = true
		return m, nil

	case tickMsg:
		m.spinnerFrame++
		m.flushPending()
		m.syncAgents()
		if m.quitArmed && time.Since(m.quitAt) > quitTimeout {
			m.quitArmed = false
			if strings.Contains(m.statusMsg, "again to quit") {
				m.statusMsg = "Ready"
			}
		}
		if m.running {
			m.statusMsg = m.runningStatus()
		}
		if m.running || m.activeAgentCount() > 0 {
			m.dirty = true
		}
		return m, tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })

	case agentEventMsg:
		m.handleAgentEvent(msg.event)
		return m, nil

	case subagentEventMsg:
		m.handleSubagentEvent(msg.event)
		return m, nil

	case turnDoneMsg:
		m.handleTurnDone(msg)
		return m, nil

	case cmdResultMsg:
		if msg.err != nil {
			m.addError(msg.text + ": " + msg.err.Error())
		} else if !msg.quiet && strings.TrimSpace(msg.text) != "" {
			m.addInfo(msg.text)
		}
		return m, nil

	case approvalRequestMsg:
		m.flushPending()
		m.pendingApproval = msg.request
		m.statusMsg = "Waiting for approval"
		m.dirty = true
		m.followTail = true
		return m, nil

	case askUserMsg:
		m.flushPending()
		m.pendingAsk = msg.request
		m.statusMsg = "Waiting for input"
		m.dirty = true
		m.followTail = true
		return m, nil

	case editorExternalMsg:
		if msg.err != nil {
			m.addError("external editor: " + msg.err.Error())
			return m, nil
		}
		m.ed.SetValue(strings.TrimRight(msg.content, "\n"))
		m.statusMsg = "Loaded prompt from external editor"
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		if m.viewport.AtBottom() {
			m.followTail = true
		}
		return m, cmd

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Any other key disarms a pending quit.
	if m.quitArmed && !(msg.Type == tea.KeyCtrlC) {
		m.quitArmed = false
		if strings.Contains(m.statusMsg, "again to quit") {
			m.statusMsg = "Ready"
		}
	}

	if m.pendingApproval != nil {
		return m.handleApprovalKey(msg)
	}
	if m.pendingAsk != nil {
		return m.handleAskKey(msg)
	}
	if m.agentsPanel {
		return m.handleAgentsPanelKey(msg)
	}
	if m.activeModal != modalNone {
		return m.handleModalKey(msg)
	}

	if msg.Type == tea.KeyCtrlC {
		return m.handleCtrlC()
	}
	if msg.Type == tea.KeyCtrlT {
		m.showReasoning = !m.showReasoning
		m.showToolDetail = m.showReasoning
		if m.showReasoning {
			m.statusMsg = "Reasoning and tool details expanded"
		} else {
			m.statusMsg = "Reasoning and tool details collapsed"
		}
		m.dirty = true
		return nil
	}
	if msg.Type == tea.KeyCtrlO {
		m.toggleAgentFocus()
		return nil
	}
	if msg.Type == tea.KeyEsc {
		if m.agentsOpen {
			m.agentsOpen = false
			m.statusMsg = "Ready"
			m.dirty = true
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.followTail = m.viewport.AtBottom()
		return cmd
	case tea.KeyUp:
		if m.agentsExpanded() && m.ed.IsEmpty() {
			m.cycleAgentFocus(-1)
			return nil
		}
	case tea.KeyDown:
		if m.agentsExpanded() && m.ed.IsEmpty() {
			m.cycleAgentFocus(1)
			return nil
		}
	}

	if msg.Type == tea.KeyTab && len(m.agentOrder) > 0 && !m.completionTokenActive() {
		m.toggleAgentFocus()
		return nil
	}

	if msg.Type == tea.KeyCtrlG {
		return m.openExternalEditor()
	}
	if msg.Type == tea.KeyCtrlP {
		m.openModelModal()
		return nil
	}

	return m.handleEditorKey(msg)
}

func (m *Model) handleEditorKey(msg tea.KeyMsg) tea.Cmd {
	if m.ed.SearchActive() {
		switch msg.Type {
		case tea.KeyCtrlR:
			m.ed.NextSearch()
			return nil
		case tea.KeyEsc:
			m.ed.CancelSearch()
			return nil
		case tea.KeyEnter:
			m.ed.AcceptSearch()
			return nil
		case tea.KeyBackspace:
			m.ed.SearchBackspace()
			return nil
		case tea.KeyRunes:
			m.ed.UpdateSearch(string(msg.Runes))
			return nil
		case tea.KeySpace:
			m.ed.UpdateSearch(" ")
			return nil
		default:
			return nil
		}
	}

	if msg.Paste && msg.Type == tea.KeyRunes {
		m.handlePaste(string(msg.Runes))
		return nil
	}

	switch msg.Type {
	case tea.KeyEnter:
		if msg.Alt || msg.String() == "shift+enter" || msg.String() == "ctrl+j" {
			m.ed.InsertNewline()
			return nil
		}
		return m.submit()
	case tea.KeyTab:
		m.complete()
		return nil
	case tea.KeyRunes:
		m.ed.InsertRunes(string(msg.Runes))
	case tea.KeySpace:
		m.ed.InsertRunes(" ")
	case tea.KeyBackspace:
		m.ed.Backspace()
	case tea.KeyDelete:
		m.ed.DeleteForward()
	case tea.KeyLeft:
		m.ed.MoveLeft()
	case tea.KeyRight:
		m.ed.MoveRight()
	case tea.KeyUp:
		m.ed.MoveUp()
	case tea.KeyDown:
		m.ed.MoveDown()
	case tea.KeyHome:
		m.ed.MoveLineStart()
	case tea.KeyEnd:
		m.ed.MoveLineEnd()
	case tea.KeyCtrlA:
		m.ed.MoveLineStart()
	case tea.KeyCtrlE:
		m.ed.MoveLineEnd()
	case tea.KeyCtrlW:
		m.ed.DeleteWordBackward()
	case tea.KeyCtrlK:
		m.ed.DeleteToLineEnd()
	case tea.KeyCtrlU:
		m.ed.DeleteToLineStart()
	case tea.KeyCtrlR:
		m.ed.StartSearch()
	}
	return nil
}

func (m *Model) handleCtrlC() tea.Cmd {
	if m.running {
		if m.cancel != nil {
			m.cancel()
		}
		m.cancel = nil
		m.running = false
		m.flushPending()
		m.curAssistant = nil
		m.curReasoning = nil
		m.addInfo("Turn cancelled.")
		m.statusMsg = "Cancelled"
		m.quitArmed = false
		m.dirty = true
		return nil
	}
	if m.quitArmed {
		return tea.Quit
	}
	m.quitArmed = true
	m.quitAt = time.Now()
	m.statusMsg = "Press Ctrl+C again to quit"
	m.addInfo("Press Ctrl+C again to quit.")
	return nil
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.ed.Value())
	if text == "" {
		return nil
	}
	attachments := m.ed.TakeAttachments()
	m.ed.AddHistory(text)
	m.ed.Reset()
	m.flushPending()

	if strings.HasPrefix(text, "/") && !strings.Contains(text, "\n") {
		return m.handleSlash(text, attachments)
	}
	return m.startTurn(text, attachments)
}

func (m *Model) startTurn(text string, attachments []provider.ContentPart) tea.Cmd {
	if m.opts.Agent == nil {
		m.addError("No agent configured.")
		return nil
	}
	if m.running {
		m.addInfo("A turn is already running. Press Ctrl+C to cancel it.")
		return nil
	}
	base := m.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.turnCtx = ctx
	m.cancel = cancel
	m.running = true
	m.turnStart = time.Now()
	m.curAssistant = nil
	m.curReasoning = nil
	m.usingTools = false
	m.followTail = true
	m.quitArmed = false
	m.outputRunes = 0
	m.speedStart = time.Time{}
	m.liveSpeed = 0
	m.statusMsg = "Thinking…"
	m.ctxTokens += len(text) / 4

	m.entries = append(m.entries, &transcriptEntry{kind: entryUser, text: text})
	if len(attachments) > 0 {
		m.addInfo(fmt.Sprintf("Attached %d image(s).", len(attachments)))
	}
	m.dirty = true

	go func() {
		res, err := m.opts.Agent.RunTurn(ctx, text, attachments)
		m.bus.Push(turnDoneMsg{result: res, err: err})
	}()
	return nil
}

func (m *Model) handleTurnDone(msg turnDoneMsg) {
	m.flushPending()
	m.running = false
	m.usingTools = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if msg.result != nil {
		m.usage = msg.result.TotalUsage
		m.totalCost = msg.result.TotalCostUSD
		m.cache = msg.result.CacheHitRate
		if msg.result.Usage.PromptTokens > 0 {
			m.ctxTokens = msg.result.Usage.PromptTokens
		}
		if msg.result.TokensPerSec > 0 {
			m.liveSpeed = msg.result.TokensPerSec
			m.lastSpeed = msg.result.TokensPerSec
		}
	}
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) && !errors.Is(msg.err, context.DeadlineExceeded) {
			m.addError(msg.err.Error())
			m.statusMsg = "Error"
		}
	} else if msg.result != nil {
		switch msg.result.StopReason {
		case agent.StopMaxSteps:
			m.addInfo("Stopped: maximum steps reached.")
		case agent.StopBudget:
			m.addInfo("Stopped: cost budget reached.")
		case agent.StopCancelled:
			m.statusMsg = "Cancelled"
		default:
			m.statusMsg = "Ready"
		}
	}
	m.curAssistant = nil
	m.curReasoning = nil
	m.dirty = true
}

func (m *Model) handleAgentEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventTurnStart:
		m.running = true
		m.followTail = true
		m.dirty = true

	case agent.EventReasoningDelta:
		m.pendingReasoning += ev.Text
		m.ctxTokens += len(ev.Text) / 4
		m.noteOutputStream(ev.Text)

	case agent.EventTextDelta:
		m.pendingText += ev.Text
		m.ctxTokens += len(ev.Text) / 4
		m.noteOutputStream(ev.Text)

	case agent.EventToolStart:
		m.flushPending()
		m.curAssistant = nil
		m.curReasoning = nil
		m.usingTools = true
		entry := &transcriptEntry{
			kind:     entryTool,
			toolName: ev.ToolName,
			toolID:   ev.ToolCallID,
			args:     string(ev.Args),
			started:  time.Now(),
		}
		m.entries = append(m.entries, entry)
		if m.toolEntries == nil {
			m.toolEntries = make(map[string]*transcriptEntry)
		}
		key := ev.ToolCallID
		if key == "" {
			key = ev.ToolName
		}
		m.toolEntries[key] = entry
		m.statusMsg = "Running " + ev.ToolName + "…"
		m.dirty = true

	case agent.EventToolEnd:
		m.flushPending()
		key := ev.ToolCallID
		if key == "" {
			key = ev.ToolName
		}
		entry := m.toolEntries[key]
		if entry == nil {
			entry = &transcriptEntry{kind: entryTool, toolName: ev.ToolName, toolID: ev.ToolCallID}
			m.entries = append(m.entries, entry)
			if m.toolEntries == nil {
				m.toolEntries = make(map[string]*transcriptEntry)
			}
			m.toolEntries[key] = entry
		}
		entry.output = ev.Output
		entry.isError = ev.IsError
		entry.elapsed = time.Duration(ev.DurationMS) * time.Millisecond
		entry.finished = true
		m.usingTools = false
		m.ctxTokens += len(ev.Output) / 4
		m.statusMsg = "Thinking…"
		m.dirty = true

	case agent.EventUsage:
		if ev.Usage != nil {
			m.usage = addUsage(m.usage, *ev.Usage)
		}
		if ev.TotalCostUSD > 0 {
			m.totalCost = ev.TotalCostUSD
		}
		if ev.CacheHitRate > 0 {
			m.cache = ev.CacheHitRate
		}
		if ev.TokensPerSec > 0 {
			m.liveSpeed = ev.TokensPerSec
			m.lastSpeed = ev.TokensPerSec
		}

	case agent.EventTurnEnd:
		m.flushPending()
		m.running = false
		m.usingTools = false
		if ev.TotalCostUSD > 0 {
			m.totalCost = ev.TotalCostUSD
		}
		if ev.CacheHitRate > 0 {
			m.cache = ev.CacheHitRate
		}
		if ev.TokensPerSec > 0 {
			m.liveSpeed = ev.TokensPerSec
			m.lastSpeed = ev.TokensPerSec
		}
		m.curAssistant = nil
		m.curReasoning = nil
		m.dirty = true

	case agent.EventCompact:
		if ev.Compact != nil {
			m.addInfo(fmt.Sprintf("Context compacted: %d → %d messages (%s).",
				ev.Compact.MessagesBefore, ev.Compact.MessagesAfter, ev.Compact.Reason))
		} else if ev.Message != "" {
			m.addInfo(ev.Message)
		}

	case agent.EventError:
		if ev.Reason == string(agent.StopCancelled) || strings.Contains(ev.Message, "context canceled") {
			return
		}
		m.flushPending()
		m.addError(ev.Message)
		m.statusMsg = "Error"

	case agent.EventCacheDebug:
		if ev.Message != "" {
			m.addInfo("cache: " + ev.Message)
		}
	}
}

func (m *Model) handleSubagentEvent(ev subagent.Event) {
	block := m.agents[ev.AgentID]
	if block == nil {
		block = &agentBlock{view: subagent.AgentView{ID: ev.AgentID}}
		m.agents[ev.AgentID] = block
		m.agentOrder = append(m.agentOrder, ev.AgentID)
	}
	if ev.AgentType != "" {
		block.view.AgentType = ev.AgentType
	}
	if ev.Description != "" {
		block.view.Description = ev.Description
	}
	if ev.Status != "" {
		block.view.Status = ev.Status
	}
	if ev.Step > 0 {
		block.view.Step = ev.Step
	}
	if ev.CurrentTool != "" {
		block.view.CurrentTool = ev.CurrentTool
	}
	if ev.Usage.TotalTokens > 0 {
		block.view.Usage = ev.Usage
	}
	if ev.CostUSD > 0 {
		block.view.CostUSD = ev.CostUSD
	}
	if ev.Elapsed > 0 {
		block.view.Elapsed = ev.Elapsed
	}

	switch ev.Type {
	case subagent.EventSubagentDelta:
		if m.agentLines[ev.AgentID] == nil {
			m.agentLines[ev.AgentID] = &strings.Builder{}
		}
		m.agentLines[ev.AgentID].WriteString(ev.Detail)
	case subagent.EventSubagentStep:
		if ev.CurrentTool != "" {
			addStep(&block.view, subagent.StepView{
				Tool:   ev.CurrentTool,
				Status: subagent.StatusRunning,
			})
		}
		m.dirty = true
	case subagent.EventSubagentApproval:
		if ev.CurrentTool != "" {
			addStep(&block.view, subagent.StepView{
				Tool:   ev.CurrentTool,
				Detail: ev.Detail,
				Status: subagent.StatusWaitingApproval,
			})
		}
		m.statusMsg = "Sub-agent approval required"
		m.dirty = true
	case subagent.EventSubagentStart:
		m.dirty = true
	case subagent.EventSubagentEnd:
		if ev.Summary != "" {
			block.view.Summary = ev.Summary
		}
		if ev.Detail != "" {
			block.view.Error = ev.Detail
		}
		m.flushAgentLine(ev.AgentID)
		m.dirty = true
	}
}

func addStep(view *subagent.AgentView, step subagent.StepView) {
	for _, existing := range view.Steps {
		if existing.Tool == step.Tool && existing.Detail == step.Detail {
			return
		}
	}
	view.Steps = append(view.Steps, step)
}

func (m *Model) flushAgentLine(id string) {
	b := m.agentLines[id]
	if b == nil {
		return
	}
	if s := b.String(); s != "" {
		if block := m.agents[id]; block != nil {
			block.view.Summary = s
		}
	}
	m.agentLines[id] = &strings.Builder{}
}

func (m *Model) flushPending() {
	if m.pendingReasoning == "" && m.pendingText == "" {
		return
	}
	if m.pendingReasoning != "" {
		if m.curReasoning == nil {
			m.curReasoning = &transcriptEntry{kind: entryReasoning}
			m.entries = append(m.entries, m.curReasoning)
		}
		m.curReasoning.text += m.pendingReasoning
		m.pendingReasoning = ""
	}
	if m.pendingText != "" {
		if m.curAssistant == nil {
			m.curAssistant = &transcriptEntry{kind: entryAssistant}
			m.entries = append(m.entries, m.curAssistant)
		}
		m.curAssistant.text += m.pendingText
		m.pendingText = ""
	}
	m.dirty = true
}

func (m *Model) addInfo(text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	m.flushPending()
	m.entries = append(m.entries, &transcriptEntry{kind: entryInfo, text: text})
	m.followTail = true
	m.dirty = true
}

func (m *Model) addError(text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	m.flushPending()
	m.entries = append(m.entries, &transcriptEntry{kind: entryError, text: text})
	m.followTail = true
	m.dirty = true
}

// View renders the whole interface.
func (m *Model) View() string {
	if m.width <= 0 {
		m.width = 100
	}
	if m.height <= 0 {
		m.height = 30
	}
	if m.running && m.pendingApproval == nil && m.pendingAsk == nil {
		m.statusMsg = m.runningStatus()
	}
	m.flushPending()
	m.rebuildContent()

	editorView := m.ed.View(m.width, m.st)
	todoView := m.todoView()
	agentView := m.agentsView()
	statusView := m.statusView()

	mainView := ""
	switch {
	case m.pendingApproval != nil:
		mainView = m.approvalView()
	case m.pendingAsk != nil:
		mainView = m.askView()
	case m.agentsPanel:
		mainView = m.agentsPanelView()
	case m.activeModal != modalNone:
		mainView = m.modalView()
	default:
		mainView = ""
	}

	bottom := make([]string, 0, 5)
	if strings.TrimSpace(m.statusMsg) != "" {
		bottom = append(bottom, m.st.paint(m.st.dim, m.statusMsg))
	}
	if todoView != "" {
		bottom = append(bottom, todoView)
	}
	if agentView != "" {
		bottom = append(bottom, agentView)
	}
	bottom = append(bottom, editorView)
	bottom = append(bottom, statusView)

	used := 0
	for _, part := range bottom {
		used += lineCount(part)
	}
	height := m.height - used
	if height < 1 {
		height = 1
	}
	m.viewport.Width = maxInt(20, m.width)
	m.viewport.Height = height
	if mainView != "" {
		// Modals replace the transcript area.
		return m.finishView(strings.Join(append([]string{mainView}, bottom...), "\n"))
	}
	if m.followTail {
		m.viewport.GotoBottom()
	}
	mainView = m.viewport.View()
	return m.finishView(strings.Join(append([]string{mainView}, bottom...), "\n"))
}

func (m *Model) finishView(view string) string {
	if !m.st.plain {
		return view
	}
	return asciiOnly(view)
}

// asciiOnly replaces the small set of decorative Unicode runes used by the UI
// with ASCII equivalents. It keeps NO_COLOR sessions usable on terminals that
// cannot render UTF-8 box drawing or status glyphs.
func asciiOnly(s string) string {
	replacer := strings.NewReplacer(
		"▸", ">", "❯", ">", "│", "|", "├", "|", "└", "`",
		"·", "|", "…", "...", "▌", "|", "─", "-", "⠋", "*",
		"✓", "[ok]", "✗", "[x]", "⚠", "!", "⊘", "[-]", "◐", "[~]",
		"○", "[ ]", "→", "->", "↑", "^", "↓", "v", "←", "<",
		"┃", "|", "●", "*", "┌", "+", "┐", "+", "┘", "+",
	)
	return replacer.Replace(s)
}

func (m *Model) rebuildContent() {
	if !m.dirty && m.lastWidth == m.width {
		return
	}
	m.computeContextTokens()
	m.content = m.renderTranscript()
	m.viewport.SetContent(m.content)
	m.lastWidth = m.width
	m.dirty = false
	if m.followTail {
		m.viewport.GotoBottom()
	}
}

func (m *Model) computeContextTokens() {
	chars := m.historyChars
	for _, entry := range m.entries {
		chars += len(entry.text) + len(entry.output)
	}
	tokens := chars / 4
	if tokens > m.ctxTokens {
		m.ctxTokens = tokens
	}
}

func (m *Model) renderTranscript() string {
	if len(m.entries) == 0 {
		return m.renderWelcome()
	}
	var blocks []string
	for _, entry := range m.entries {
		rendered := m.renderEntry(entry)
		if strings.TrimSpace(rendered) != "" {
			blocks = append(blocks, rendered)
		}
	}
	return strings.Join(blocks, "\n\n")
}

func (m *Model) renderWelcome() string {
	version := m.opts.Version
	if version == "" {
		version = "dev"
	}
	model := "unknown"
	effort := ""
	if m.opts.Config != nil {
		model = m.opts.Config.Model
		effort = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	modelLabel := model
	if effort != "" {
		modelLabel += " (" + effort + ")"
	}
	cwd := m.opts.WorkDir
	if cwd == "" {
		cwd = "."
	}
	branch := strings.TrimSpace(m.opts.GitBranch)
	if branch == "" {
		branch = "no-branch"
	}
	sep := "  ·  "
	dim := func(text string) string { return m.st.paint(m.st.dim, text) }
	key := func(text string) string { return m.st.paint(m.st.info, text) }
	lines := []string{
		m.st.paint(m.st.heading, "tern") + dim("  "+version),
		dim("An interactive coding agent."),
		"",
		dim("model   ") + m.st.paint(m.st.accent, modelLabel),
		dim("cwd     ") + m.st.paint(m.st.teal, cwd),
		dim("branch  ") + m.st.paint(m.st.sage, branch),
		"",
		key("/model") + dim(" or ") + key("Ctrl+P") + dim(" switch model") + sep +
			key("/sessions") + dim(" resume") + sep + key("/stats") + dim(" usage"),
		key("/skills") + dim(" inspect") + sep + key("/mcp") + dim(" servers") + sep +
			key("/help") + dim(" all commands"),
	}
	if !m.st.plain {
		bar := m.st.paint(m.st.userAccent, "┃")
		for i, line := range lines {
			if i == 0 {
				continue
			}
			lines[i] = bar + " " + line
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderEntry(entry *transcriptEntry) string {
	width := maxInt(20, m.width-2)
	switch entry.kind {
	case entryUser:
		marker := "❯ "
		if m.st.plain {
			marker = "> "
		}
		body := m.st.paint(m.st.userAccent, "┃ ") + m.st.paint(m.st.user, wrapPlainCell(entry.text, width-4))
		return m.st.paint(m.st.user, marker) + body

	case entryAssistant:
		return RenderMarkdown(entry.text, width, m.st.plain)

	case entryReasoning:
		label := "✻ Reasoning"
		if m.st.plain {
			label = "* Reasoning"
		}
		if !m.showReasoning {
			n := len([]rune(entry.text))
			return m.st.paint(m.st.dim, fmt.Sprintf("%s (%d chars, Ctrl+T to expand)", label, n))
		}
		body := RenderMarkdown(entry.text, width-2, m.st.plain)
		return m.st.paint(m.st.reasoning, label) + "\n" + m.st.paint(m.st.dim, body)

	case entryTool:
		marker := "⚙ "
		if m.st.plain {
			marker = "* "
		}
		summary := toolSummary(entry.toolName, entry.args)
		head := m.st.paint(m.st.toolName, marker+entry.toolName)
		if summary != "" {
			head += m.st.paint(m.st.dim, " · "+summary)
		}
		if entry.finished && entry.elapsed > 0 {
			head += m.st.paint(m.st.dim, fmt.Sprintf(" · %s", formatDuration(entry.elapsed)))
		} else if !entry.finished && !entry.started.IsZero() {
			head += m.st.paint(m.st.dim, fmt.Sprintf(" · %s", formatDuration(time.Since(entry.started))))
		}
		if entry.isError {
			head += m.st.paint(m.st.error, " · failed")
		}
		if !m.showToolDetail {
			return head
		}
		var b strings.Builder
		b.WriteString(head)
		if strings.TrimSpace(entry.args) != "" && entry.args != "{}" && entry.args != "null" {
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.dim, indentLines(wrapPlainCell(entry.args, width-4), "    ")))
		}
		if strings.TrimSpace(entry.output) != "" {
			b.WriteString("\n")
			output := truncateLines(entry.output, 20)
			style := m.st.dim
			if entry.isError {
				style = m.st.error
			}
			b.WriteString(m.st.paint(style, indentLines(wrapPlainCell(output, width-4), "    ")))
		}
		return b.String()

	case entryError:
		marker := "✗ "
		if m.st.plain {
			marker = "! "
		}
		return m.st.paint(m.st.error, marker+wrapPlainCell(entry.text, width-2))

	default: // info
		marker := "• "
		if m.st.plain {
			marker = "- "
		}
		return m.st.paint(m.st.info, marker) + m.st.paint(m.st.dim, wrapPlainCell(entry.text, width-2))
	}
}

func (m *Model) todoView() string {
	if m.opts.Todos == nil {
		return ""
	}
	items := m.opts.Todos.Items()
	if len(items) == 0 {
		return ""
	}
	lines := []string{m.st.paint(m.st.bold, "Tasks")}
	for i, item := range items {
		if i >= 7 {
			lines = append(lines, m.st.paint(m.st.dim, fmt.Sprintf("  … %d more", len(items)-i)))
			break
		}
		marker := "[ ]"
		style := m.st.dim
		switch item.Status {
		case tools.TodoInProgress:
			marker = "[~]"
			style = m.st.warn
		case tools.TodoCompleted:
			marker = "[x]"
			style = m.st.success
		case tools.TodoCancelled:
			marker = "[-]"
			style = m.st.dim
		}
		if !m.st.plain {
			switch item.Status {
			case tools.TodoInProgress:
				marker = "◐"
			case tools.TodoCompleted:
				marker = "✓"
			case tools.TodoCancelled:
				marker = "⊘"
			default:
				marker = "○"
			}
		}
		lines = append(lines, "  "+m.st.paint(style, marker+" "+wrapPlainCell(item.Content, m.width-8)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) statusView() string {
	modelName := "unknown"
	effort := ""
	if m.opts.Config != nil {
		modelName = m.opts.Config.Model
		effort = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	modelLabel := modelName
	if effort != "" {
		modelLabel += " (" + effort + ")"
	}
	mode := "ask"
	if m.opts.Agent != nil && m.opts.Agent.Permissions != nil {
		mode = m.opts.Agent.Permissions.Mode().String()
	} else if m.opts.Config != nil && m.opts.Config.PermissionMode != "" {
		mode = m.opts.Config.PermissionMode
	}
	ctxPct := 0
	if m.ctxWindow > 0 {
		ctxPct = int(float64(m.ctxTokens) / float64(m.ctxWindow) * 100)
		if ctxPct < 0 {
			ctxPct = 0
		}
	}
	cachePct := int(m.cache*100 + 0.5)
	branch := strings.TrimSpace(m.opts.GitBranch)
	if branch == "" {
		branch = "no-branch"
	}
	text := m.st.statusSegments(
		modelLabel,
		mode,
		formatTokensPerSec(m.speed()),
		fmt.Sprintf("ctx %d%%", ctxPct),
		fmt.Sprintf("cache %d%%", cachePct),
		fmt.Sprintf("$%.3f", m.totalCost),
		branch,
	)
	if m.st.plain {
		return text
	}
	pad := m.width - displayWidth(text)
	if pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return m.st.statusBar.Render(text)
}

// speed returns the most useful throughput number for display: the live
// estimate while streaming, otherwise the last exact value.
func (m *Model) speed() float64 {
	if m.running && m.liveSpeed > 0 {
		return m.liveSpeed
	}
	if m.liveSpeed > 0 {
		return m.liveSpeed
	}
	return m.lastSpeed
}

func formatTokensPerSec(v float64) string {
	if v < 0 {
		v = 0
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f tok/s", v)
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f tok/s", v)
	}
	return fmt.Sprintf("%.1f tok/s", v)
}

// noteOutputStream estimates throughput from streamed deltas. The estimate is
// replaced by the provider's exact value when usage/turn-end events arrive.
func (m *Model) noteOutputStream(text string) {
	n := len([]rune(text))
	if n == 0 {
		return
	}
	if m.speedStart.IsZero() {
		m.speedStart = time.Now()
	}
	m.outputRunes += n
	elapsed := time.Since(m.speedStart).Seconds()
	if elapsed < 0.2 {
		elapsed = 0.2
	}
	m.liveSpeed = float64(m.outputRunes/4) / elapsed
}

func (m *Model) runningStatus() string {
	frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	if m.st.plain {
		frame = "*"
	}
	elapsed := formatDuration(time.Since(m.turnStart))
	speed := ""
	if m.liveSpeed > 0 {
		speed = " · " + formatTokensPerSec(m.liveSpeed)
	}
	if m.usingTools {
		return frame + " Running tools… " + elapsed + speed
	}
	return frame + " Thinking… " + elapsed + speed
}

func (m *Model) activeAgentCount() int {
	n := 0
	for _, id := range m.agentOrder {
		if block := m.agents[id]; block != nil {
			switch block.view.Status {
			case subagent.StatusQueued, subagent.StatusRunning, subagent.StatusWaitingApproval:
				n++
			}
		}
	}
	return n
}

func (m *Model) syncAgents() {
	if m.opts.Subagent == nil {
		return
	}
	list := m.opts.Subagent.List()
	changed := false
	for _, st := range list {
		block := m.agents[st.ID]
		if block == nil {
			block = &agentBlock{view: subagent.AgentView{ID: st.ID}}
			m.agents[st.ID] = block
			m.agentOrder = append(m.agentOrder, st.ID)
			changed = true
		}
		before := block.view
		block.view.AgentType = st.AgentType
		block.view.Description = st.Description
		block.view.Status = st.Status
		block.view.Step = st.Step
		block.view.CurrentTool = st.CurrentTool
		block.view.Usage = st.Usage
		block.view.CostUSD = st.CostUSD
		block.view.Error = st.Error
		if st.Summary != "" {
			block.view.Summary = st.Summary
		}
		if st.Status == subagent.StatusRunning || st.Status == subagent.StatusWaitingApproval || st.Status == subagent.StatusQueued {
			block.view.Elapsed = time.Since(st.StartedAt)
		}
		if before.Status != block.view.Status || before.Step != block.view.Step || before.CurrentTool != block.view.CurrentTool {
			changed = true
		}
		m.flushAgentLine(st.ID)
	}
	if changed {
		m.dirty = true
	}
}

func (m *Model) agentsExpanded() bool {
	return m.agentsOpen && len(m.agentOrder) > 0
}

// agentOpen is the expansion flag for the focused agent. It is kept separate
// from the panel visibility so Esc can collapse inspection without closing the
// panel unexpectedly.
func (m *Model) agentFocused() *agentBlock {
	if len(m.agentOrder) == 0 {
		return nil
	}
	if m.agentFocus < 0 || m.agentFocus >= len(m.agentOrder) {
		m.agentFocus = 0
	}
	return m.agents[m.agentOrder[m.agentFocus]]
}

func (m *Model) toggleAgentFocus() {
	if len(m.agentOrder) == 0 {
		m.addInfo("No sub-agents yet. Delegate work with the task tool.")
		return
	}
	if m.agentsOpen {
		m.agentsOpen = false
		m.statusMsg = "Ready"
		m.dirty = true
		return
	}
	m.agentsOpen = true
	if m.panelIndex >= len(m.agentOrder) {
		m.panelIndex = 0
	}
	m.agentFocus = m.panelIndex
	m.statusMsg = "Inspecting sub-agents (Esc to return)"
	m.dirty = true
}

func (m *Model) cycleAgentFocus(delta int) {
	if len(m.agentOrder) == 0 {
		return
	}
	m.agentFocus = (m.agentFocus + delta + len(m.agentOrder)) % len(m.agentOrder)
	m.panelIndex = m.agentFocus
	m.dirty = true
}

func (m *Model) completionCandidates(query string) []string {
	files := m.workspaceFiles()
	type scored struct {
		path  string
		score int
	}
	var matches []scored
	for _, path := range files {
		if score, ok := fuzzyScore(query, path); ok {
			matches = append(matches, scored{path: path, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return matches[i].path < matches[j].path
		}
		return matches[i].score > matches[j].score
	})
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.path)
		if len(out) >= 30 {
			break
		}
	}
	return out
}

func (m *Model) workspaceFiles() []string {
	var files []string
	root := m.opts.WorkDir
	if root == "" {
		return files
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == ".git" || name == "node_modules" || name == "vendor" || name == ".tern") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= 6000 {
			return filepath.SkipAll
		}
		return nil
	})
	m.files = files
	return files
}

func (m *Model) completionTokenActive() bool {
	start, _, token := m.ed.TokenAtCursor()
	if strings.HasPrefix(token, "@") {
		return true
	}
	return start == 0 && strings.HasPrefix(token, "/")
}

func (m *Model) complete() {
	start, end, token := m.ed.TokenAtCursor()
	if token == "" {
		return
	}
	var candidates []string
	var replacement func(string) string
	switch {
	case strings.HasPrefix(token, "@"):
		query := strings.TrimPrefix(token, "@")
		candidates = m.completionCandidates(query)
		replacement = func(path string) string { return "@" + path }
	case strings.HasPrefix(token, "/") && start == 0:
		query := strings.ToLower(strings.TrimPrefix(token, "/"))
		for _, name := range builtinCommandNames {
			if strings.HasPrefix(name, query) {
				candidates = append(candidates, "/"+name)
			}
		}
		for _, cmd := range m.commands {
			value := "/" + cmd.Name
			if strings.HasPrefix(strings.ToLower(cmd.Name), query) {
				found := false
				for _, existing := range candidates {
					if existing == value {
						found = true
						break
					}
				}
				if !found {
					candidates = append(candidates, value)
				}
			}
		}
		sort.Strings(candidates)
		replacement = func(value string) string { return value + " " }
	default:
		return
	}
	if len(candidates) == 0 {
		m.ed.ClearSuggestions()
		return
	}
	index := 0
	if m.ed.suggestionToken == token {
		index = (m.ed.suggestionIndex + 1) % len(candidates)
	}
	value := candidates[index]
	m.ed.ReplaceRange(start, end, replacement(value))
	m.ed.SetSuggestions(token, candidates, index)
}

func (m *Model) handlePaste(text string) {
	if m.attachPastedPaths(text) {
		return
	}
	m.ed.InsertRunes(text)
}

func (m *Model) attachPastedPaths(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"'`)
		if line == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	if len(cleaned) == 0 {
		return false
	}
	var replaced []string
	var attachments []provider.ContentPart
	for _, candidate := range cleaned {
		path := candidate
		if strings.HasPrefix(path, "file://") {
			path = strings.TrimPrefix(path, "file://")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.opts.WorkDir, path)
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return false
		}
		if isImagePath(path) {
			part, err := imagePart(path)
			if err != nil {
				return false
			}
			attachments = append(attachments, part)
			replaced = append(replaced, "[image: "+filepath.Base(path)+"]")
			continue
		}
		rel, err := filepath.Rel(m.opts.WorkDir, path)
		if err != nil {
			rel = path
		}
		replaced = append(replaced, "@"+filepath.ToSlash(rel))
	}
	for _, part := range attachments {
		m.ed.AddAttachment(part)
	}
	m.ed.InsertRunes(strings.Join(replaced, " "))
	if len(attachments) > 0 {
		m.addInfo(fmt.Sprintf("Attached %d image(s).", len(attachments)))
	}
	return true
}

func imagePart(path string) (provider.ContentPart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provider.ContentPart{}, err
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	}
	return provider.ContentPart{
		Type:     "image_url",
		ImageURL: &provider.ImageURL{URL: "data:" + mime + ";base64," + base64Encode(data)},
	}, nil
}

func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

func (m *Model) openExternalEditor() tea.Cmd {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		m.addError("Set $EDITOR or $VISUAL to use Ctrl+G.")
		return nil
	}
	dir := os.TempDir()
	file, err := os.CreateTemp(dir, "tern-prompt-*.md")
	if err != nil {
		m.addError("external editor: " + err.Error())
		return nil
	}
	path := file.Name()
	content := m.ed.Value()
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		os.Remove(path)
		m.addError("external editor: " + err.Error())
		return nil
	}
	file.Close()

	fields := strings.Fields(editor)
	args := append(fields[1:], path)
	cmd := execCommand(fields[0], args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editorExternalMsg{err: err}
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return editorExternalMsg{err: readErr}
		}
		return editorExternalMsg{content: string(data)}
	})
}

// toolSummary renders a one-line description of a tool call.
func toolSummary(name, rawArgs string) string {
	args := strings.TrimSpace(rawArgs)
	if args == "" || args == "{}" || args == "null" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		return truncateCell(args, 80)
	}
	for _, key := range []string{"command", "path", "file_path", "pattern", "query", "url", "description", "name"} {
		if value, ok := decoded[key]; ok {
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				return truncateCell(s, 90)
			}
		}
	}
	return truncateCell(args, 80)
}

func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		PromptTokens:          a.PromptTokens + b.PromptTokens,
		CompletionTokens:      a.CompletionTokens + b.CompletionTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
		PromptCacheHitTokens:  a.PromptCacheHitTokens + b.PromptCacheHitTokens,
		PromptCacheMissTokens: a.PromptCacheMissTokens + b.PromptCacheMissTokens,
		ReasoningTokens:       a.ReasoningTokens + b.ReasoningTokens,
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}

func truncateLines(s string, max int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > max {
		lines = append(lines[:max], fmt.Sprintf("… %d more lines", len(lines)-max))
	}
	return strings.Join(lines, "\n")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func truncateCell(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if displayWidth(s) <= max {
		return s
	}
	head, _ := splitWidth(s, max)
	return head + "…"
}

func wrapPlainCell(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		var current string
		for _, word := range strings.Fields(line) {
			if current == "" {
				current = word
				continue
			}
			if displayWidth(current)+1+displayWidth(word) <= width {
				current += " " + word
			} else {
				out = append(out, current)
				current = word
			}
		}
		if current != "" {
			out = append(out, current)
		}
	}
	return strings.Join(out, "\n")
}

// fuzzyScore matches query runes as an ordered subsequence. Higher scores mean
// prefix, substring, or boundary matches.
func fuzzyScore(query, candidate string) (int, bool) {
	q := []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 1, true
	}
	c := []rune(strings.ToLower(candidate))
	if strings.HasPrefix(string(c), string(q)) {
		return 1000 - len(c), true
	}
	if strings.Contains(string(c), string(q)) {
		return 500 - len(c), true
	}
	score := 0
	pos := 0
	last := -2
	for _, qr := range q {
		found := false
		for pos < len(c) {
			if c[pos] == qr {
				if pos == last+1 {
					score += 4
				} else {
					score++
				}
				last = pos
				pos++
				found = true
				break
			}
			pos++
		}
		if !found {
			return 0, false
		}
	}
	return score, true
}

func base64Encode(data []byte) string {
	// Kept in a helper so tests can exercise image attachment without pulling
	// in an extra import at the call site.
	return base64Encoding.EncodeToString(data)
}
